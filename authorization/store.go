package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Seed struct {
	Actions     map[string][]string `json:"actions"`
	Principals  []struct{ ID, Kind, Role, Domain string }
	Tools       map[string][]string
	ToolGroups  map[string][]string `json:"tool_groups"`
	ToolGrants  map[string][]string `json:"tool_grants"`
	AgentGroups map[string][]string `json:"agent_groups"`
	AgentGrants map[string][]string `json:"agent_grants"`
	LLMGroups   map[string][]string `json:"llm_groups"`
	LLMGrants   map[string][]string `json:"llm_grants"`
}

type Store struct {
	DB    *sql.DB
	Owner string
	mu    sync.RWMutex
	live  map[string]bool
}

func openStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA foreign_keys=ON; PRAGMA busy_timeout=1000"); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{DB: db, Owner: randomToken(), live: map[string]bool{}}, nil
}

// Schema changes happen only through the explicit migrate command.
func (s *Store) Migrate(seedPath string) error {
	if _, err := s.DB.Exec(`CREATE TABLE IF NOT EXISTS authz_schema_migrations(version TEXT PRIMARY KEY)`); err != nil {
		return err
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		var count int
		if err = s.DB.QueryRow("SELECT count(*) FROM authz_schema_migrations WHERE version=?", entry.Name()).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		script, _ := migrations.ReadFile("migrations/" + entry.Name())
		tx, err := s.DB.Begin()
		if err != nil {
			return err
		}
		if _, err = tx.Exec(string(script)); err == nil {
			_, err = tx.Exec("INSERT INTO authz_schema_migrations VALUES(?)", entry.Name())
		}
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", entry.Name(), err)
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	var applied int
	if err = s.DB.QueryRow("SELECT count(*) FROM authz_schema_migrations WHERE version='seed-v1'").Scan(&applied); err != nil {
		return err
	}
	if applied > 0 {
		return s.SeedActions(seedPath)
	}
	data, err := os.ReadFile(seedPath)
	if err != nil {
		return err
	}
	var seed Seed
	if err = json.Unmarshal(data, &seed); err != nil {
		return err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing int
	if err = tx.QueryRow("SELECT count(*) FROM principals").Scan(&existing); err != nil {
		return err
	}
	run := func(query string, args ...any) {
		if err == nil {
			_, err = tx.Exec(query, args...)
		}
	}
	if existing == 0 {
		for _, p := range seed.Principals {
			run("INSERT INTO principals(id,kind,role,domain) VALUES(?,?,?,?)", p.ID, p.Kind, p.Role, p.Domain)
		}
		for _, p := range seed.Principals {
			if p.Kind == "agent" {
				run("INSERT INTO agents VALUES(?,?)", p.ID, "http://"+p.ID+":8000/")
			}
		}
		for server, names := range seed.Tools {
			run("INSERT INTO mcp_servers VALUES(?,?)", server, "http://mcp-"+server+":8000/mcp")
			for _, name := range names {
				run("INSERT INTO tools(server_id,name) VALUES(?,?)", server, name)
			}
		}
		for group, names := range seed.ToolGroups {
			run("INSERT INTO tool_groups VALUES(?)", group)
			for _, name := range names {
				parts := strings.SplitN(name, "/", 2)
				run("INSERT INTO tool_group_members SELECT ?,id FROM tools WHERE server_id=? AND name=?", group, parts[0], parts[1])
			}
		}
		for p, groups := range seed.ToolGrants {
			for _, g := range groups {
				run("INSERT INTO tool_grants VALUES(?,?)", p, g)
			}
		}
		for g, agents := range seed.AgentGroups {
			run("INSERT INTO agent_groups VALUES(?)", g)
			for _, a := range agents {
				run("INSERT INTO agent_group_members VALUES(?,?)", g, a)
			}
		}
		for p, groups := range seed.AgentGrants {
			for _, g := range groups {
				run("INSERT INTO agent_grants VALUES(?,?)", p, g)
			}
		}
	} else {
		// Preserve existing tool/agent grants. Apply the required direct service
		// access to B/C once; later revocations are never reconciled by startup.
		for _, p := range seed.Principals {
			if p.Kind == "user" {
				run("INSERT OR IGNORE INTO principals(id,kind,role,domain) VALUES(?,?,?,?)", p.ID, p.Kind, p.Role, p.Domain)
			}
		}
		run("INSERT OR IGNORE INTO agent_groups VALUES('origin-direct-agents')")
		for _, a := range []string{"agent-a", "agent-b", "agent-c"} {
			run("INSERT OR IGNORE INTO agent_group_members SELECT 'origin-direct-agents',id FROM agents WHERE id=?", a)
		}
		for _, p := range []string{"service-portal", "service-erp"} {
			run("INSERT OR IGNORE INTO agent_grants SELECT id,'origin-direct-agents' FROM principals WHERE id=?", p)
		}
		// User examples get their initial grants without changing existing principals.
		run("INSERT OR IGNORE INTO agent_groups VALUES('user-entry')")
		run("INSERT OR IGNORE INTO agent_group_members SELECT 'user-entry',id FROM agents WHERE id='agent-a'")
		run("INSERT OR IGNORE INTO agent_grants VALUES('alice','user-entry')")
		for _, p := range []string{"alice", "observer"} {
			run("INSERT OR IGNORE INTO tool_grants SELECT ?,'documents' FROM tool_groups WHERE id='documents'", p)
		}
	}
	for g, models := range seed.LLMGroups {
		run("INSERT INTO llm_groups VALUES(?)", g)
		for _, m := range models {
			run("INSERT OR IGNORE INTO llm_models VALUES(?,1)", m)
			run("INSERT INTO llm_group_members VALUES(?,?)", g, m)
		}
	}
	for p, groups := range seed.LLMGrants {
		for _, g := range groups {
			run("INSERT INTO llm_grants SELECT id,? FROM principals WHERE id=?", g, p)
		}
	}
	run("INSERT INTO authz_schema_migrations VALUES('seed-v1')")
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return s.SeedActions(seedPath)
}

// Versioned example initialization also supports upgrading an existing DB without
// ever restoring action grants an operator subsequently revokes.
func (s *Store) SeedActions(seedPath string) error {
	var count int
	if err := s.DB.QueryRow("SELECT count(*) FROM authz_schema_migrations WHERE version='seed-actions-v1'").Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	data, err := os.ReadFile(seedPath)
	if err != nil {
		return err
	}
	var seed Seed
	if err = json.Unmarshal(data, &seed); err != nil {
		return err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, action := range []string{"a2a.invoke", "tools.read", "tools.execute", "llm.read", "llm.invoke"} {
		if _, err = tx.Exec("INSERT INTO principal_actions SELECT id,? FROM principals", action); err != nil {
			return err
		}
	}
	for principal, actions := range seed.Actions {
		if _, err = tx.Exec("DELETE FROM principal_actions WHERE principal_id=?", principal); err != nil {
			return err
		}
		for _, action := range actions {
			if _, err = tx.Exec("INSERT INTO principal_actions SELECT id,? FROM principals WHERE id=?", action, principal); err != nil {
				return err
			}
		}
	}
	if _, err = tx.Exec("INSERT INTO authz_schema_migrations VALUES('seed-actions-v1')"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Start() error {
	var count int
	if err := s.DB.QueryRow("SELECT count(*) FROM authz_schema_migrations WHERE version IN ('001_permissions.sql','002_contexts_llm.sql','003_actions.sql','seed-v1','seed-actions-v1')").Scan(&count); err != nil {
		return err
	}
	if count != 5 {
		return fmt.Errorf("schema not current: run migrate")
	}
	_, err := s.DB.Exec("UPDATE delegation_contexts SET revoked_at=? WHERE revoked_at IS NULL", time.Now().Unix())
	return err
}

func (s *Store) Create(ctx context.Context, origin, workflow string, expiration int64) (string, error) {
	now := time.Now().Unix()
	if expiration > now+300 {
		expiration = now + 300
	}
	if expiration <= now {
		return "", fmt.Errorf("expired origin")
	}
	token := randomToken()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM delegation_contexts WHERE expires_at<=?", now); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO delegation_contexts VALUES(?,?,?,?,?,?,NULL)", digest(token), origin, workflow, s.Owner, now, expiration); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	s.mu.Lock()
	s.live[digest(token)] = true
	s.mu.Unlock()
	return token, nil
}
func (s *Store) Live(token string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.live[digest(token)]
}
func (s *Store) Revoke(token string) error {
	s.mu.Lock()
	delete(s.live, digest(token))
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := s.DB.ExecContext(ctx, "UPDATE delegation_contexts SET revoked_at=? WHERE token_hash=? AND owner=?", time.Now().Unix(), digest(token), s.Owner)
	return err
}

func canAgent(ctx context.Context, tx *sql.Tx, principal, agent string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agent_grants g JOIN agent_group_members m ON m.group_id=g.group_id JOIN principals p ON p.id=m.agent_id WHERE g.principal_id=? AND m.agent_id=? AND p.enabled=1`, principal, agent).Scan(&n)
	return n > 0, err
}
func toolsFor(ctx context.Context, tx *sql.Tx, origin, actor string) (map[string][]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT t.server_id,t.name FROM tools t JOIN tool_group_members m ON m.tool_id=t.id JOIN tool_grants g ON g.group_id=m.group_id WHERE g.principal_id=? AND t.enabled=1 AND t.id IN (SELECT m.tool_id FROM tool_group_members m JOIN tool_grants g ON g.group_id=m.group_id WHERE g.principal_id=?) ORDER BY t.server_id,t.name`, origin, actor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var server, name string
		if err = rows.Scan(&server, &name); err != nil {
			return nil, err
		}
		out[server] = append(out[server], name)
	}
	return out, rows.Err()
}
func modelsFor(ctx context.Context, tx *sql.Tx, origin, actor string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT m.id FROM llm_models m JOIN llm_group_members gm ON gm.model_id=m.id JOIN llm_grants g ON g.group_id=gm.group_id WHERE m.enabled=1 AND g.principal_id=? AND m.id IN (SELECT gm.model_id FROM llm_group_members gm JOIN llm_grants g ON g.group_id=gm.group_id WHERE g.principal_id=?)`, origin, actor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var m string
		if err = rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	sort.Strings(out)
	return out, rows.Err()
}
