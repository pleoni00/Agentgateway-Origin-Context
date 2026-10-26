package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

var workflowPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
var referencePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
var agentPath = regexp.MustCompile(`^/a2a/([a-zA-Z0-9_-]+)/jsonrpc$`)
var errDenied = errors.New("authorization denied")

type Check struct {
	Authorization string `json:"authorization"`
	Reference     string `json:"reference"`
	Workflow      string `json:"workflow"`
	Path          string `json:"path"`
	Method        string `json:"method"`
	RPC           struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Model  string          `json:"model"`
		Stream bool            `json:"stream"`
	} `json:"rpc"`
}
type Decision struct {
	AllowedTools  map[string][]string `json:"allowed_tools"`
	AllowedModels []string            `json:"allowed_models"`
	Origin        string              `json:"origin"`
	Caller        string              `json:"caller"`
}
type Server struct {
	Store                  *Store
	Verifier               *Verifier
	Upstream               *url.URL
	AdminToken, CheckToken string
	Proxy                  *httputil.ReverseProxy
}

func (s *Server) Decide(ctx context.Context, c Check) (d Decision, err error) {
	defer func() {
		log.Printf("workflow=%q caller=%q origin=%q path=%q operation=%q model=%q allowed=%t", c.Workflow, d.Caller, d.Origin, c.Path, c.RPC.Method, c.RPC.Model, err == nil)
	}()
	d = Decision{AllowedTools: map[string][]string{}, AllowedModels: []string{}}
	actor, err := s.Verifier.Verify(c.Authorization)
	if err != nil {
		return d, errDenied
	}
	if !workflowPattern.MatchString(c.Workflow) {
		return d, errDenied
	}
	tx, err := s.Store.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return d, err
	}
	defer tx.Rollback()
	var kind string
	err = tx.QueryRowContext(ctx, "SELECT kind FROM principals WHERE id=? AND enabled=1", actor.Name).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return d, errDenied
	}
	if err != nil {
		return d, err
	}
	if kind != actor.Kind {
		return d, errDenied
	}
	d.Caller = actor.Name
	d.Origin = actor.Name
	if c.Reference != "" {
		if !referencePattern.MatchString(c.Reference) || !s.Store.Live(c.Reference) {
			return d, errDenied
		}
		err = tx.QueryRowContext(ctx, `SELECT c.origin_id FROM delegation_contexts c JOIN principals p ON p.id=c.origin_id WHERE c.token_hash=? AND c.workflow_id=? AND c.owner=? AND c.revoked_at IS NULL AND c.expires_at>? AND p.enabled=1 AND p.kind IN ('user','service')`, digest(c.Reference), c.Workflow, s.Store.Owner, time.Now().Unix()).Scan(&d.Origin)
		if errors.Is(err, sql.ErrNoRows) {
			return d, errDenied
		}
		if err != nil {
			return d, err
		}
		if kind != "agent" && (d.Origin != actor.Name || !agentPath.MatchString(c.Path)) {
			return d, errDenied
		}
	} else if kind == "agent" {
		return d, errDenied
	}
	if kind == "agent" {
		ok, err := canAgent(ctx, tx, d.Origin, actor.Name)
		if err != nil {
			return d, err
		}
		if !ok {
			return d, errDenied
		}
	}
	action := ""
	switch strings.TrimRight(c.Path, "/") {
	case "/mcp":
		action = "tools.read"
		if c.RPC.Method == "tools/call" {
			action = "tools.execute"
		}
	case "/llm/v1/models":
		action = "llm.read"
	case "/llm/v1/chat/completions":
		action = "llm.invoke"
	default:
		if agentPath.MatchString(c.Path) {
			action = "a2a.invoke"
		}
	}
	if action == "" {
		return d, errDenied
	}
	for _, principal := range []string{d.Caller, d.Origin} {
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM principal_actions WHERE principal_id=? AND action=?", principal, action).Scan(&count); err != nil {
			return d, err
		}
		if count == 0 {
			return d, errDenied
		}
	}
	if match := agentPath.FindStringSubmatch(c.Path); match != nil {
		if c.Method != "POST" || c.RPC.Method != "message/send" {
			return d, errDenied
		}
		for _, principal := range []string{d.Caller, d.Origin} {
			ok, err := canAgent(ctx, tx, principal, match[1])
			if err != nil {
				return d, err
			}
			if !ok {
				return d, errDenied
			}
		}
		return d, nil
	}
	switch strings.TrimRight(c.Path, "/") {
	case "/mcp":
		d.AllowedTools, err = toolsFor(ctx, tx, d.Origin, d.Caller)
		if err != nil {
			return d, err
		}
		if c.Method == "POST" {
			switch c.RPC.Method {
			case "initialize", "notifications/initialized", "ping", "tools/list":
			case "tools/call":
				var params struct {
					Name string `json:"name"`
				}
				if json.Unmarshal(c.RPC.Params, &params) != nil {
					return d, errDenied
				}
				found := false
				for target, names := range d.AllowedTools {
					for _, name := range names {
						if target+"_"+name == params.Name {
							found = true
						}
					}
				}
				if !found {
					return d, errDenied
				}
			default:
				return d, errDenied
			}
		} else if c.Method != "GET" && c.Method != "DELETE" {
			return d, errDenied
		}
	case "/llm/v1/models":
		if c.Method != "GET" {
			return d, errDenied
		}
		d.AllowedModels, err = modelsFor(ctx, tx, d.Origin, d.Caller)
	case "/llm/v1/chat/completions":
		if c.Method != "POST" || c.RPC.Stream {
			return d, errDenied
		} // Synchronous PoC lifecycle only.
		d.AllowedModels, err = modelsFor(ctx, tx, d.Origin, d.Caller)
		if err != nil {
			return d, err
		}
		if !slices.Contains(d.AllowedModels, c.RPC.Model) {
			return d, errDenied
		}
	default:
		return d, errDenied
	}
	return d, err
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	if errors.Is(err, errDenied) {
		status = http.StatusForbidden
	}
	writeJSON(w, status, map[string]string{"error": http.StatusText(status)})
}
func sharedSecret(header, secret string) bool {
	return secret != "" && subtle.ConstantTimeCompare([]byte(header), []byte("Bearer "+secret)) == 1
}
func readJSON(w http.ResponseWriter, r *http.Request, value any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("extra JSON data")
	}
	return nil
}
func (s *Server) Check(w http.ResponseWriter, r *http.Request) {
	if !sharedSecret(r.Header.Get("Authorization"), s.CheckToken) {
		writeJSON(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	var c Check
	if readJSON(w, r, &c) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	d, err := s.Decide(r.Context(), c)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, d)
}

func (s *Server) PublicHandler() http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(s.Upstream)
	originalDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		originalDirector(r)
		if strings.HasPrefix(r.URL.Path, "/llm/") {
			r.URL.Host = strings.Split(s.Upstream.Host, ":")[0] + ":8081"
			r.URL.Path = strings.TrimPrefix(r.URL.Path, "/llm")
		}
	}
	proxy.Transport = &http.Transport{ResponseHeaderTimeout: 60 * time.Second, MaxIdleConns: 100, IdleConnTimeout: 30 * time.Second}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) { fail(w, fmt.Errorf("upstream unavailable")) }
	proxy.ModifyResponse = func(response *http.Response) error {
		// Only the outer request owns the lease. Close it before response headers
		// are released, even if the client disconnects while reading the body.
		if token, _ := response.Request.Context().Value(leaseKey{}).(string); token != "" {
			if err := s.Store.Revoke(token); err != nil {
				return err
			}
		}
		response.Header.Del("X-Origin-Context")
		response.Header.Del("X-Origin-Authorization")
		return nil
	}
	s.Proxy = proxy
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			if s.Store.DB.PingContext(r.Context()) != nil {
				fail(w, fmt.Errorf("database unavailable"))
				return
			}
			writeJSON(w, 200, map[string]string{"status": "ok"})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/admin/") {
			s.Admin(w, r)
			return
		}
		actor, err := s.Verifier.Verify(r.Header.Get("Authorization"))
		if err != nil {
			writeJSON(w, 401, map[string]string{"error": "invalid access token"})
			return
		}
		// Do not accept ambiguous escaped paths or caller-selected identity labels.
		if r.URL.RawPath != "" || strings.Contains(r.URL.Path, "//") || r.Header.Get("X-Origin-Authorization") != "" {
			fail(w, errDenied)
			return
		}
		reference := r.Header.Get("X-Origin-Context")
		workflow := r.Header.Get("X-Workflow-Id")
		if workflow == "" && actor.Kind != "agent" {
			workflow = "wf-" + randomToken()[:24]
		}
		if !workflowPattern.MatchString(workflow) {
			fail(w, errDenied)
			return
		}
		if actor.Kind != "agent" && reference != "" {
			fail(w, errDenied)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			writeJSON(w, 413, map[string]string{"error": "request too large"})
			return
		}
		c := Check{Authorization: r.Header.Get("Authorization"), Reference: reference, Workflow: workflow, Path: r.URL.Path, Method: r.Method}
		if len(raw) > 0 && json.Unmarshal(raw, &c.RPC) != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
			return
		}
		d, err := s.Decide(r.Context(), c)
		if err != nil {
			fail(w, err)
			return
		}
		if actor.Kind != "agent" && agentPath.MatchString(r.URL.Path) {
			reference, err = s.Store.Create(r.Context(), actor.Name, workflow, actor.ExpiresAt.Unix())
			if err != nil {
				fail(w, err)
				return
			}
			defer func() {
				if err := s.Store.Revoke(reference); err != nil {
					log.Printf("context revocation failed: %v", err)
				}
			}()
			r = r.WithContext(context.WithValue(r.Context(), leaseKey{}, reference))
		}
		r.Header.Del("X-Origin-Service")
		r.Header.Del("X-Caller")
		r.Header.Del("X-Check-Token")
		r.Header.Del("X-Allowed-Models")
		r.Header.Set("X-Origin-Service", d.Origin)
		r.Header.Set("X-Caller", d.Caller)
		r.Header.Set("X-Workflow-Id", workflow)
		if reference != "" {
			r.Header.Set("X-Origin-Context", reference)
		}
		if strings.TrimRight(r.URL.Path, "/") == "/llm/v1/models" {
			// Discovery and invocation share the same DB decision. No provider
			// discovery is needed and provider-private model IDs are never exposed.
			models := []map[string]string{}
			for _, model := range d.AllowedModels {
				models = append(models, map[string]string{"id": model, "object": "model", "owned_by": "lab"})
			}
			writeJSON(w, 200, map[string]any{"object": "list", "data": models})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		r.ContentLength = int64(len(raw))
		s.Proxy.ServeHTTP(w, r)
	})
}

type leaseKey struct{}
