package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"
)

// The registry owns desired MCP routing. agentgateway owns MCP federation and
// invocation. The template continues to own the fixed enforcement/A2A/LLM routes.
type MCPRegistry struct {
	Store            *Store
	Template, Output string
	mu               sync.Mutex
}

var serverIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,63}$`)

func validEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && !strings.Contains(endpoint, "$")
}

func (m *MCPRegistry) Publish(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.publish(ctx)
}

// Publish atomically through a directory mount: mounting the file itself would
// leave Docker watching the old inode after rename. No secret values are rendered.
func (m *MCPRegistry) publish(ctx context.Context) error {
	raw, err := os.ReadFile(m.Template)
	if err != nil {
		return err
	}
	var config map[string]any
	if err = yaml.Unmarshal(raw, &config); err != nil {
		return err
	}
	targets := []any{}
	rows, err := m.Store.DB.QueryContext(ctx, "SELECT id,endpoint FROM mcp_servers WHERE enabled=1 ORDER BY id")
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, endpoint string
		if err = rows.Scan(&id, &endpoint); err != nil {
			rows.Close()
			return err
		}
		if !serverIDPattern.MatchString(id) || !validEndpoint(endpoint) {
			rows.Close()
			return fmt.Errorf("invalid MCP registry entry %q", id)
		}
		targets = append(targets, map[string]any{"name": id, "mcp": map[string]any{"host": endpoint}})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	found := 0
	binds, _ := config["binds"].([]any)
	for _, b := range binds {
		bind, _ := b.(map[string]any)
		listeners, _ := bind["listeners"].([]any)
		for _, l := range listeners {
			listener, _ := l.(map[string]any)
			routes, _ := listener["routes"].([]any)
			for _, r := range routes {
				route, _ := r.(map[string]any)
				if route["name"] != "mcp" {
					continue
				}
				backends, _ := route["backends"].([]any)
				if len(backends) != 1 {
					return fmt.Errorf("MCP template needs exactly one backend")
				}
				backend, _ := backends[0].(map[string]any)
				mc, _ := backend["mcp"].(map[string]any)
				if mc == nil {
					return fmt.Errorf("MCP template backend missing")
				}
				mc["targets"] = targets
				found++
			}
		}
	}
	if found != 1 {
		return fmt.Errorf("MCP template needs exactly one named mcp route")
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	current, _ := os.ReadFile(m.Output)
	if bytes.Equal(current, data) {
		return nil
	}
	if err = os.MkdirAll(filepath.Dir(m.Output), 0755); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(m.Output), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0644); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(file.Name(), m.Output)
}

// Discovery uses the official MCP client, including initialization, sessions,
// SSE responses and pagination. All pages must succeed before touching the DB.
func discoverTools(ctx context.Context, endpoint string) ([]*mcp.Tool, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "origin-context-registry", Version: "1.0.0"}, nil)
	transport := &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, DisableStandaloneSSE: true, MaxRetries: -1}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	tools := []*mcp.Tool{}
	cursor := ""
	seen := map[string]bool{}
	names := map[string]bool{}
	for page := 0; page < 100; page++ {
		result, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, tool := range result.Tools {
			if tool == nil || tool.Name == "" || names[tool.Name] {
				return nil, fmt.Errorf("invalid or duplicate tool")
			}
			names[tool.Name] = true
			tools = append(tools, tool)
			if len(tools) > 10000 {
				return nil, fmt.Errorf("catalog too large")
			}
		}
		if result.NextCursor == "" {
			return tools, nil
		}
		cursor = result.NextCursor
		if seen[cursor] {
			return nil, fmt.Errorf("repeated catalog cursor")
		}
		seen[cursor] = true
	}
	return nil, fmt.Errorf("too many catalog pages")
}

func (m *MCPRegistry) syncTools(ctx context.Context, id string) error {
	var endpoint string
	if err := m.Store.DB.QueryRowContext(ctx, "SELECT endpoint FROM mcp_servers WHERE id=?", id).Scan(&endpoint); err != nil {
		return err
	}
	tools, err := discoverTools(ctx, endpoint)
	if err != nil {
		return err
	}
	tx, err := m.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE tools SET available=0 WHERE server_id=?", id); err != nil {
		return err
	}
	for _, tool := range tools {
		raw, err := json.Marshal(tool)
		if err != nil {
			return err
		}
		// Existing tool IDs, grants and explicit disables survive a refresh. New
		// tools have no group membership, so discovery never grants permissions.
		if _, err = tx.ExecContext(ctx, `INSERT INTO tools(server_id,name,available,definition) VALUES(?,?,1,?) ON CONFLICT(server_id,name) DO UPDATE SET available=1,definition=excluded.definition`, id, tool.Name, string(raw)); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE mcp_servers SET synced_at=? WHERE id=?", time.Now().Unix(), id); err != nil {
		return err
	}
	return tx.Commit()
}

// Admin authenticates before entering this handler. Endpoint registration is a
// trusted administration operation and can intentionally reach private MCPs.
func (s *Server) AdminMCP(w http.ResponseWriter, r *http.Request) {
	m := s.Registry
	if m == nil {
		writeJSON(w, 503, map[string]string{"error": "registry configuration unavailable"})
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.URL.Path == "/admin/mcp" && r.Method == "GET" {
		m.catalog(w, r)
		return
	}
	if r.URL.Path == "/admin/mcp/apply" && r.Method == "POST" {
		if err := m.publish(r.Context()); err != nil {
			writeJSON(w, 503, map[string]string{"error": "configuration publication failed; retry apply"})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "published"})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/admin/mcp/groups/") {
		s.mcpGroup(w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/admin/mcp/tools/") && r.Method == "PATCH" {
		id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/admin/mcp/tools/"), 10, 64)
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if err != nil || id <= 0 || readJSON(w, r, &body) != nil || body.Enabled == nil {
			writeJSON(w, 400, map[string]string{"error": "tool ID and enabled flag are required"})
			return
		}
		result, err := m.Store.DB.ExecContext(r.Context(), "UPDATE tools SET enabled=? WHERE id=?", *body.Enabled, id)
		if err != nil {
			fail(w, err)
			return
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			writeJSON(w, 404, map[string]string{"error": "unknown tool"})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "updated"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/admin/mcp/servers/")
	parts := strings.Split(path, "/")
	id := parts[0]
	if !strings.HasPrefix(r.URL.Path, "/admin/mcp/servers/") || !serverIDPattern.MatchString(id) {
		writeJSON(w, 404, map[string]string{"error": "unknown management route"})
		return
	}
	if len(parts) == 2 && parts[1] == "sync" && r.Method == "POST" {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if err := m.syncTools(ctx, id); err != nil {
			if err == sql.ErrNoRows {
				writeJSON(w, 404, map[string]string{"error": "unknown MCP server"})
			} else {
				writeJSON(w, 502, map[string]string{"error": "MCP discovery failed; catalog unchanged"})
			}
			return
		}
		writeJSON(w, 200, map[string]string{"status": "synchronized"})
		return
	}

	if len(parts) == 1 && r.Method == "DELETE" {
		tx, err := m.Store.DB.BeginTx(r.Context(), nil)
		if err != nil {
			fail(w, err)
			return
		}
		defer tx.Rollback()
		_, err = tx.ExecContext(r.Context(), "DELETE FROM tool_group_members WHERE tool_id IN (SELECT id FROM tools WHERE server_id=?)", id)
		if err == nil {
			_, err = tx.ExecContext(r.Context(), "DELETE FROM tools WHERE server_id=?", id)
		}
		if err == nil {
			_, err = tx.ExecContext(r.Context(), "DELETE FROM mcp_servers WHERE id=?", id)
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			fail(w, err)
			return
		}
		if err = m.publish(r.Context()); err != nil {
			writeJSON(w, 503, map[string]string{"error": "server removed; configuration publication failed; retry apply"})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "published"})
		return
	}
	if len(parts) != 1 || r.Method != "PUT" {
		w.WriteHeader(405)
		return
	}
	var body struct {
		Endpoint string `json:"endpoint"`
		Enabled  *bool  `json:"enabled"`
	}
	if readJSON(w, r, &body) != nil || !validEndpoint(body.Endpoint) {
		writeJSON(w, 400, map[string]string{"error": "endpoint must be an HTTP(S) URL without credentials, query, fragment or environment interpolation"})
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	tx, err := m.Store.DB.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, err)
		return
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(r.Context(), `UPDATE tools SET available=0 WHERE server_id=? AND EXISTS(SELECT 1 FROM mcp_servers WHERE id=? AND endpoint<>?)`, id, id, body.Endpoint)
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `INSERT INTO mcp_servers(id,endpoint,enabled) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET endpoint=excluded.endpoint,enabled=excluded.enabled,synced_at=CASE WHEN endpoint<>excluded.endpoint THEN NULL ELSE synced_at END`, id, body.Endpoint, enabled)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		fail(w, err)
		return
	}
	if err = m.publish(r.Context()); err != nil {
		writeJSON(w, 503, map[string]string{"error": "registration saved; configuration publication failed; retry apply"})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "published"})
}

func (m *MCPRegistry) catalog(w http.ResponseWriter, r *http.Request) {
	tx, err := m.Store.DB.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		fail(w, err)
		return
	}
	defer tx.Rollback()
	out := map[string]any{}
	queries := map[string]string{
		"servers": "SELECT id,endpoint,enabled,synced_at FROM mcp_servers ORDER BY id",
		"tools":   "SELECT id,server_id,name,enabled,available,definition FROM tools ORDER BY server_id,name",
		"groups":  "SELECT id FROM tool_groups ORDER BY id",
		"members": "SELECT group_id,tool_id FROM tool_group_members ORDER BY group_id,tool_id",
		"grants":  "SELECT principal_id,group_id FROM tool_grants ORDER BY principal_id,group_id",
	}
	for key, query := range queries {
		rows, err := tx.QueryContext(r.Context(), query)
		if err != nil {
			fail(w, err)
			return
		}
		columns, _ := rows.Columns()
		items := []map[string]any{}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err = rows.Scan(pointers...); err != nil {
				rows.Close()
				fail(w, err)
				return
			}
			item := map[string]any{}
			for i, c := range columns {
				item[c] = values[i]
			}
			items = append(items, item)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			fail(w, err)
			return
		}
		out[key] = items
	}
	writeJSON(w, 200, out)
}

func (s *Server) mcpGroup(w http.ResponseWriter, r *http.Request) {
	group := strings.TrimPrefix(r.URL.Path, "/admin/mcp/groups/")
	if !namePattern.MatchString(group) {
		writeJSON(w, 400, map[string]string{"error": "invalid group"})
		return
	}

	if r.Method == "DELETE" {
		tx, err := s.Store.DB.BeginTx(r.Context(), nil)
		if err != nil {
			fail(w, err)
			return
		}
		defer tx.Rollback()
		_, err = tx.ExecContext(r.Context(), "DELETE FROM tool_group_members WHERE group_id=?", group)
		if err == nil {
			_, err = tx.ExecContext(r.Context(), "DELETE FROM tool_grants WHERE group_id=?", group)
		}
		if err == nil {
			_, err = tx.ExecContext(r.Context(), "DELETE FROM tool_groups WHERE id=?", group)
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "deleted"})
		return
	}
	if r.Method != "PUT" {
		w.WriteHeader(405)
		return
	}
	var body struct {
		Tools      []int64  `json:"tools"`
		Principals []string `json:"principals"`
	}
	if readJSON(w, r, &body) != nil || body.Tools == nil || body.Principals == nil {
		writeJSON(w, 400, map[string]string{"error": "tools and principals arrays are required"})
		return
	}
	tx, err := s.Store.DB.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, err)
		return
	}
	defer tx.Rollback()
	_, err = tx.Exec("INSERT OR IGNORE INTO tool_groups VALUES(?)", group)
	if err == nil {
		_, err = tx.Exec("DELETE FROM tool_group_members WHERE group_id=?", group)
	}
	if err == nil {
		_, err = tx.Exec("DELETE FROM tool_grants WHERE group_id=?", group)
	}
	for _, id := range body.Tools {
		if err == nil {
			_, err = tx.Exec("INSERT INTO tool_group_members VALUES(?,?)", group, id)
		}
	}
	for _, id := range body.Principals {
		if err == nil {
			_, err = tx.Exec("INSERT INTO tool_grants VALUES(?,?)", id, group)
		}
	}
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid tool, principal or duplicate assignment"})
		return
	}
	if err = tx.Commit(); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "updated"})
}
