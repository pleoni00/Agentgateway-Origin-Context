package main

import (
	"database/sql"
	"net/http"
	"regexp"
	"strings"
)

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:-]{0,127}$`)

// Management credentials are independent of caller identities and are never sent
// to agents. Parameters and fixed SQL statements prevent caller-controlled SQL.
func (s *Server) Admin(w http.ResponseWriter, r *http.Request) {
	if !sharedSecret(r.Header.Get("Authorization"), s.AdminToken) {
		writeJSON(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/admin/mcp") {
		s.AdminMCP(w, r)
		return
	}
	if r.URL.Path == "/admin/llm" && r.Method == "GET" {
		tx, err := s.Store.DB.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
		if err != nil {
			fail(w, err)
			return
		}
		defer tx.Rollback()
		out := map[string]any{}
		queries := map[string]string{"models": "SELECT id,enabled FROM llm_models ORDER BY id", "groups": "SELECT id FROM llm_groups ORDER BY id", "members": "SELECT group_id,model_id FROM llm_group_members ORDER BY group_id,model_id", "grants": "SELECT principal_id,group_id FROM llm_grants ORDER BY principal_id,group_id"}
		for name, query := range queries {
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
			out[name] = items
		}
		writeJSON(w, 200, out)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/admin/llm/groups/") {
		group := strings.TrimPrefix(r.URL.Path, "/admin/llm/groups/")
		if !namePattern.MatchString(group) {
			writeJSON(w, 400, map[string]string{"error": "invalid group"})
			return
		}
		tx, err := s.Store.DB.BeginTx(r.Context(), nil)
		if err != nil {
			fail(w, err)
			return
		}
		defer tx.Rollback()
		switch r.Method {
		case "PUT":
			var body struct {
				Models     []string `json:"models"`
				Principals []string `json:"principals"`
			}
			if readJSON(w, r, &body) != nil || body.Models == nil || body.Principals == nil {
				writeJSON(w, 400, map[string]string{"error": "invalid group definition"})
				return
			}
			if _, err = tx.Exec("INSERT OR IGNORE INTO llm_groups VALUES(?)", group); err == nil {
				_, err = tx.Exec("DELETE FROM llm_group_members WHERE group_id=?", group)
			}
			if err == nil {
				_, err = tx.Exec("DELETE FROM llm_grants WHERE group_id=?", group)
			}
			for _, model := range body.Models {
				if err == nil {
					_, err = tx.Exec("INSERT INTO llm_group_members VALUES(?,?)", group, model)
				}
			}
			for _, principal := range body.Principals {
				if err == nil {
					_, err = tx.Exec("INSERT INTO llm_grants VALUES(?,?)", principal, group)
				}
			}
		case "DELETE":
			_, err = tx.Exec("DELETE FROM llm_groups WHERE id=?", group)
		default:
			w.WriteHeader(405)
			return
		}
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid model, principal or duplicate assignment"})
			return
		}
		if err = tx.Commit(); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "updated"})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/admin/llm/models/") && r.Method == "PATCH" {
		model := strings.TrimPrefix(r.URL.Path, "/admin/llm/models/")
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if !namePattern.MatchString(model) || readJSON(w, r, &body) != nil || body.Enabled == nil {
			writeJSON(w, 400, map[string]string{"error": "enabled is required"})
			return
		}
		result, err := s.Store.DB.ExecContext(r.Context(), "UPDATE llm_models SET enabled=? WHERE id=?", *body.Enabled, model)
		if err != nil {
			fail(w, err)
			return
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			writeJSON(w, 404, map[string]string{"error": "unknown model"})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "updated"})
		return
	}
	writeJSON(w, 404, map[string]string{"error": "unknown management route"})
}
