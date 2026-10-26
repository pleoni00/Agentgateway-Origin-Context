package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func registryFixture(t *testing.T) *fixture {
	t.Helper()
	f := setup(t)
	f.server.Registry = &MCPRegistry{Store: f.server.Store, Template: "../gateway/config.yaml", Output: filepath.Join(t.TempDir(), "gateway", "config.json")}
	return f
}
func adminMCP(t *testing.T, f *fixture, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer admin-test")
	w := httptest.NewRecorder()
	f.server.Admin(w, r)
	return w
}
func TestMCPRegistryPublicationAndAuthorization(t *testing.T) {
	f := registryFixture(t)
	r := httptest.NewRequest("GET", "/admin/mcp", nil)
	w := httptest.NewRecorder()
	f.server.Admin(w, r)
	if w.Code != 401 {
		t.Fatal("unprotected registry")
	}
	for _, endpoint := range []string{"file:///tmp/x", "http://user:secret@host/mcp", "http://host/mcp?token=x", "http://$SECRET/mcp"} {
		raw, _ := json.Marshal(map[string]any{"endpoint": endpoint})
		if w := adminMCP(t, f, "PUT", "/admin/mcp/servers/new", string(raw)); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := adminMCP(t, f, "PUT", "/admin/mcp/servers/finance", `{"endpoint":"http://mcp-finance:8000/mcp","enabled":false}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	raw, err := os.ReadFile(f.server.Registry.Output)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err = json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	if len(config["binds"].([]any)) != 1 || config["llm"] == nil || !strings.Contains(string(raw), "mcpAuthorization") || strings.Contains(string(raw), "http://mcp-finance:8000/mcp") {
		t.Fatal("enforcement or destinations corrupted")
	}
	c := f.check(t, "service-erp", "agent-b", "/mcp/", "")
	d, err := f.server.Decide(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.AllowedTools["finance"]) != 0 || len(d.AllowedTools["documents"]) != 2 {
		t.Fatal(d)
	}
	// Changing an endpoint must immediately invalidate the old catalog, even
	// before agentgateway reloads, without deleting group memberships.
	if w := adminMCP(t, f, "PUT", "/admin/mcp/servers/documents", `{"endpoint":"http://other:8000/mcp"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	d, err = f.server.Decide(context.Background(), c)
	if err != nil || len(d.AllowedTools) != 0 {
		t.Fatal(d, err)
	}
	if w := adminMCP(t, f, "GET", "/admin/mcp", ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}
func TestMCPDiscoveryPaginationAndAtomicRefresh(t *testing.T) {
	f := registryFixture(t)
	phase := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Cursor string `json:"cursor"`
			} `json:"params"`
		}
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(400)
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(202)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "catalog", "version": "1"}}
		case "tools/list":
			if phase == 2 && req.Params.Cursor != "" {
				w.WriteHeader(500)
				return
			}
			name := "search_docs"
			if req.Params.Cursor != "" {
				name = "new_tool"
			}
			if phase == 1 {
				name = "new_tool"
			}
			page := map[string]any{"tools": []any{map[string]any{"name": name, "description": "discovered tool", "inputSchema": map[string]any{"type": "object"}}}}
			if req.Params.Cursor == "" && phase != 1 {
				page["nextCursor"] = "page-two"
			}
			result = page
		default:
			w.WriteHeader(400)
			return
		}
		writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer backend.Close()
	raw, _ := json.Marshal(map[string]any{"endpoint": backend.URL})
	if w := adminMCP(t, f, "PUT", "/admin/mcp/servers/documents", string(raw)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := adminMCP(t, f, "POST", "/admin/mcp/servers/documents/sync", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var available, enabled int
	var definition string
	if err := f.server.Store.DB.QueryRow("SELECT available,enabled,definition FROM tools WHERE server_id='documents' AND name='new_tool'").Scan(&available, &enabled, &definition); err != nil || available != 1 || !strings.Contains(definition, "discovered tool") {
		t.Fatal(available, definition, err)
	}
	c := f.check(t, "service-erp", "agent-b", "/mcp/", "")
	d, err := f.server.Decide(context.Background(), c)
	if err != nil || len(d.AllowedTools["documents"]) != 1 || d.AllowedTools["documents"][0] != "search_docs" {
		t.Fatal("discovery granted access", d, err)
	}

	var toolID int64
	if err = f.server.Store.DB.QueryRow("SELECT id FROM tools WHERE server_id='documents' AND name='search_docs'").Scan(&toolID); err != nil {
		t.Fatal(err)
	}
	if w := adminMCP(t, f, "PATCH", fmt.Sprintf("/admin/mcp/tools/%d", toolID), `{"enabled":false}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}

	phase = 1
	if w := adminMCP(t, f, "POST", "/admin/mcp/servers/documents/sync", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err = f.server.Store.DB.QueryRow("SELECT available,enabled FROM tools WHERE server_id='documents' AND name='search_docs'").Scan(&available, &enabled); err != nil || available != 0 || enabled != 0 {
		t.Fatal(available, enabled, err)
	}
	phase = 2
	if w := adminMCP(t, f, "POST", "/admin/mcp/servers/documents/sync", ""); w.Code != 502 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err = f.server.Store.DB.QueryRow("SELECT available FROM tools WHERE server_id='documents' AND name='search_docs'").Scan(&available); err != nil || available != 0 {
		t.Fatal("partial refresh committed", available, err)
	}
}
func TestMCPGroupsAtomicAndPublicationRetry(t *testing.T) {
	f := registryFixture(t)
	var id int64
	if err := f.server.Store.DB.QueryRow("SELECT id FROM tools WHERE server_id='finance' AND name='get_invoice'").Scan(&id); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"tools": []int64{id}, "principals": []string{"alice", "agent-b"}})
	if w := adminMCP(t, f, "PUT", "/admin/mcp/groups/custom", string(body)); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := adminMCP(t, f, "PUT", "/admin/mcp/groups/custom", `{"tools":[99999],"principals":["alice"]}`); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	var count int
	if err := f.server.Store.DB.QueryRow("SELECT count(*) FROM tool_group_members WHERE group_id='custom'").Scan(&count); err != nil || count != 1 {
		t.Fatal("failed update removed grants", count, err)
	}

	if w := adminMCP(t, f, "DELETE", "/admin/mcp/groups/custom", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := f.server.Store.DB.QueryRow("SELECT count(*) FROM tool_groups WHERE id='custom'").Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	template := f.server.Registry.Template
	f.server.Registry.Template = "missing-template"
	if w := adminMCP(t, f, "PUT", "/admin/mcp/servers/new", `{"endpoint":"http://new:8000/mcp"}`); w.Code != 503 || !strings.Contains(w.Body.String(), "registration saved") {
		t.Fatal(w.Code, w.Body.String())
	}
	f.server.Registry.Template = template
	if w := adminMCP(t, f, "POST", "/admin/mcp/apply", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	raw, err := os.ReadFile(f.server.Registry.Output)
	if err != nil || !strings.Contains(string(raw), "http://new:8000/mcp") {
		t.Fatal(err, string(raw))
	}
}
