package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type fixture struct {
	server *Server
	key    *rsa.PrivateKey
	jwks   *httptest.Server
}

func setup(t *testing.T) *fixture {
	t.Helper()
	store, err := openStore(filepath.Join(t.TempDir(), "permissions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.DB.Close() })
	if err = store.Migrate("../policies/seed.json"); err != nil {
		t.Fatal(err)
	}
	if err = store.Start(); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"keys": []any{map[string]string{"kty": "RSA", "kid": "test", "use": "sig", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	}))
	t.Cleanup(jwks.Close)
	return &fixture{&Server{Store: store, Verifier: &Verifier{Issuer: "https://issuer.example/realm", Audience: "gateway", JWKS: jwks.URL}, AdminToken: "admin-test", CheckToken: "check-test"}, key, jwks}
}
func (f *fixture) token(t *testing.T, name, kind string, modify func(*Identity)) string {
	t.Helper()
	id := Identity{Name: name, Kind: kind, RegisteredClaims: jwt.RegisteredClaims{Subject: "subject-" + name, Issuer: f.server.Verifier.Issuer, Audience: jwt.ClaimStrings{"gateway"}, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute))}}
	if modify != nil {
		modify(&id)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, id)
	token.Header["kid"] = "test"
	raw, err := token.SignedString(f.key)
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + raw
}
func (f *fixture) check(t *testing.T, origin, agent, path, model string) Check {
	t.Helper()
	ref, err := f.server.Store.Create(context.Background(), origin, "wf-test", time.Now().Add(time.Minute).Unix())
	if err != nil {
		t.Fatal(err)
	}
	c := Check{Authorization: f.token(t, agent, "agent", nil), Reference: ref, Workflow: "wf-test", Path: path, Method: "POST"}
	c.RPC.Method = "tools/list"
	c.RPC.Model = model
	return c
}
func TestJWTValidation(t *testing.T) {
	f := setup(t)
	cases := map[string]func(*Identity){"issuer": func(i *Identity) { i.Issuer = "https://evil" }, "audience": func(i *Identity) { i.Audience = jwt.ClaimStrings{"other"} }, "expired": func(i *Identity) { i.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute)) }, "missing-exp": func(i *Identity) { i.ExpiresAt = nil }, "kind": func(i *Identity) { i.Kind = "admin" }, "subject": func(i *Identity) { i.Subject = "" }}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := f.server.Verifier.Verify(f.token(t, "agent-a", "agent", mutate)); err == nil {
				t.Fatal("invalid JWT accepted")
			}
		})
	}
	if _, err := f.server.Verifier.Verify(f.token(t, "agent-a", "agent", nil)); err != nil {
		t.Fatal(err)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"iss": f.server.Verifier.Issuer})
	token.Header["kid"] = "test"
	raw, _ := token.SignedString([]byte("wrong"))
	if _, err := f.server.Verifier.Verify("Bearer " + raw); err == nil {
		t.Fatal("algorithm confusion")
	}
}
func TestToolAndModelIntersection(t *testing.T) {
	f := setup(t)
	for _, test := range []struct {
		origin, actor, model string
		allow                bool
	}{{"service-portal", "agent-b", "advanced", false}, {"service-erp", "agent-b", "advanced", true}, {"service-erp", "agent-c", "advanced", false}, {"service-portal", "agent-c", "standard", true}} {
		c := f.check(t, test.origin, test.actor, "/llm/v1/chat/completions", test.model)
		_, err := f.server.Decide(context.Background(), c)
		if (err == nil) != test.allow {
			t.Fatalf("%+v: %v", test, err)
		}
	}
	c := f.check(t, "service-portal", "agent-b", "/mcp/", "")
	d, err := f.server.Decide(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.AllowedTools["finance"]) != 1 {
		t.Fatal(d)
	}
	c.RPC.Method = "tools/call"
	c.RPC.Params = json.RawMessage(`{"name":"finance_approve_invoice"}`)
	if _, err = f.server.Decide(context.Background(), c); err == nil {
		t.Fatal("hidden tool invoked")
	}
}
func TestContextBindingRevocationAndDirectAgentPermission(t *testing.T) {
	f := setup(t)
	c := f.check(t, "service-erp", "agent-b", "/mcp/", "")
	for _, change := range []func(*Check){func(c *Check) { c.Reference = "" }, func(c *Check) { c.Reference = strings.Repeat("x", 43) }, func(c *Check) { c.Workflow = "other" }} {
		other := c
		change(&other)
		if _, err := f.server.Decide(context.Background(), other); err == nil {
			t.Fatal("invalid context accepted")
		}
	}
	if _, err := f.server.Store.DB.Exec("DELETE FROM agent_grants WHERE principal_id='service-erp'"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.server.Decide(context.Background(), c); err == nil {
		t.Fatal("origin permission on calling agent ignored")
	}
	if err := f.server.Store.Revoke(c.Reference); err != nil {
		t.Fatal(err)
	}
	if f.server.Store.Live(c.Reference) {
		t.Fatal("revoked reference live")
	}
}
func TestExpiryRestartAndFailedRevocation(t *testing.T) {
	f := setup(t)
	c := f.check(t, "service-erp", "agent-b", "/mcp/", "")
	_, err := f.server.Store.DB.Exec("UPDATE delegation_contexts SET expires_at=0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.server.Decide(context.Background(), c); err == nil {
		t.Fatal("expired context accepted")
	}
	c = f.check(t, "service-erp", "agent-b", "/mcp/", "")
	f.server.Store.Owner = randomToken()
	if _, err = f.server.Decide(context.Background(), c); err == nil {
		t.Fatal("previous process context accepted")
	}
	f.server.Store.DB.Close()
	_ = f.server.Store.Revoke(c.Reference)
	if f.server.Store.Live(c.Reference) {
		t.Fatal("failed DB write kept reference live")
	}
}
func TestRootRevokesBeforeResponseAndIgnoresSpoofedHeaders(t *testing.T) {
	f := setup(t)
	var reference string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reference = r.Header.Get("X-Origin-Context")
		if !f.server.Store.Live(reference) {
			t.Error("root context inactive")
		}
		if r.Header.Get("X-Origin-Service") != "service-portal" || r.Header.Get("X-Caller") != "service-portal" {
			t.Error("spoofed labels forwarded")
		}
		w.Header().Set("X-Origin-Context", reference)
		writeJSON(w, 200, map[string]string{"result": "ok"})
	}))
	defer upstream.Close()
	f.server.Upstream, _ = url.Parse(upstream.URL)
	handler := f.server.PublicHandler()
	r := httptest.NewRequest("POST", "/a2a/agent-a/jsonrpc", strings.NewReader(`{"method":"message/send"}`))
	r.Header.Set("Authorization", f.token(t, "service-portal", "service", nil))
	r.Header.Set("X-Origin-Service", "service-erp")
	r.Header.Set("X-Caller", "agent-b")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if reference == "" || f.server.Store.Live(reference) {
		t.Fatal("root context not revoked")
	}
	if w.Header().Get("X-Origin-Context") != "" {
		t.Fatal("reference leaked")
	}
}
func TestCancellationRevokesRoot(t *testing.T) {
	f := setup(t)
	captured := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		captured <- r.Header.Get("X-Origin-Context")
		<-r.Context().Done()
	}))
	defer upstream.Close()
	f.server.Upstream, _ = url.Parse(upstream.URL)
	server := httptest.NewServer(f.server.PublicHandler())
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	r, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/a2a/agent-a/jsonrpc", strings.NewReader(`{"method":"message/send"}`))
	r.Header.Set("Authorization", f.token(t, "service-erp", "service", nil))
	done := make(chan struct{})
	go func() {
		res, _ := http.DefaultClient.Do(r)
		if res != nil {
			res.Body.Close()
		}
		close(done)
	}()
	reference := <-captured
	cancel()
	<-done
	deadline := time.Now().Add(2 * time.Second)
	for f.server.Store.Live(reference) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if f.server.Store.Live(reference) {
		t.Fatal("cancelled root still active")
	}
}
func TestManagementIsProtectedAtomicAndImmediatelyEffective(t *testing.T) {
	f := setup(t)
	c := f.check(t, "service-portal", "agent-b", "/llm/v1/chat/completions", "advanced")
	if _, err := f.server.Decide(context.Background(), c); err == nil {
		t.Fatal("unexpected grant")
	}
	update := func(body, token string) int {
		r := httptest.NewRequest("PUT", "/admin/llm/groups/test-group", strings.NewReader(body))
		r.Header.Set("Authorization", token)
		w := httptest.NewRecorder()
		f.server.Admin(w, r)
		return w.Code
	}
	if update(`{"models":["advanced"],"principals":["service-portal","agent-b"]}`, "") != 401 {
		t.Fatal("unprotected admin")
	}
	if update(`{"models":["advanced"],"principals":["service-portal","agent-b"]}`, "Bearer admin-test") != 200 {
		t.Fatal("valid update failed")
	}
	if _, err := f.server.Decide(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if update(`{"models":["unknown"],"principals":[]}`, "Bearer admin-test") != 400 {
		t.Fatal("invalid assignment accepted")
	}
	if _, err := f.server.Decide(context.Background(), c); err != nil {
		t.Fatal("failed update was not atomic")
	}
	if update(`{"models":[],"principals":[]}`, "Bearer admin-test") != 200 {
		t.Fatal("revocation failed")
	}
	if _, err := f.server.Decide(context.Background(), c); err == nil {
		t.Fatal("revoked grant cached")
	}
}
func TestMigrationsDoNotReapplyRevokedGrants(t *testing.T) {
	f := setup(t)
	if _, err := f.server.Store.DB.Exec("DELETE FROM llm_grants WHERE principal_id='service-erp'"); err != nil {
		t.Fatal(err)
	}
	if err := f.server.Store.Migrate("../policies/seed.json"); err != nil {
		t.Fatal(err)
	}
	c := f.check(t, "service-erp", "agent-b", "/llm/v1/chat/completions", "advanced")
	if _, err := f.server.Decide(context.Background(), c); err == nil {
		t.Fatal("migration restored revoked grant")
	}
}

func TestActionPermissionsAreIndependentOfResourceMembership(t *testing.T) {
	f := setup(t)
	c := Check{Authorization: f.token(t, "observer", "user", nil), Workflow: "wf-observer", Path: "/mcp/", Method: "POST"}
	c.RPC.Method = "tools/list"
	d, err := f.server.Decide(context.Background(), c)
	if err != nil || len(d.AllowedTools["documents"]) != 2 {
		t.Fatal(d, err)
	}
	c.RPC.Method = "tools/call"
	c.RPC.Params = json.RawMessage(`{"name":"documents_search_docs"}`)
	if _, err = f.server.Decide(context.Background(), c); err == nil {
		t.Fatal("discovery-only user executed a tool")
	}
	c = f.check(t, "service-erp", "agent-b", "/llm/v1/chat/completions", "advanced")
	if _, err = f.server.Store.DB.Exec("DELETE FROM principal_actions WHERE principal_id='service-erp' AND action='llm.invoke'"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.server.Decide(context.Background(), c); err == nil {
		t.Fatal("origin action permission ignored")
	}
	if err = f.server.Store.Migrate("../policies/seed.json"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.server.Decide(context.Background(), c); err == nil {
		t.Fatal("migration restored revoked action")
	}
}

func TestLegacyPermissionsDatabaseIsPreserved(t *testing.T) {
	store, err := openStore(filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	schema, err := migrations.ReadFile("migrations/001_permissions.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct{ id, kind string }{{"service-portal", "service"}, {"service-erp", "service"}, {"agent-a", "agent"}, {"agent-b", "agent"}, {"agent-c", "agent"}} {
		if _, err = store.DB.Exec("INSERT INTO principals(id,kind,role,domain) VALUES(?,?,'existing-role','existing-domain')", p.id, p.kind); err != nil {
			t.Fatal(err)
		}
		if p.kind == "agent" {
			if _, err = store.DB.Exec("INSERT INTO agents VALUES(?,'http://existing/')", p.id); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err = store.DB.Exec("INSERT INTO tool_groups VALUES('custom-existing'); INSERT INTO tool_grants VALUES('service-portal','custom-existing')"); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate("../policies/seed.json"); err != nil {
		t.Fatal(err)
	}
	var group string
	if err = store.DB.QueryRow("SELECT group_id FROM tool_grants WHERE principal_id='service-portal'").Scan(&group); err != nil || group != "custom-existing" {
		t.Fatal(group, err)
	}
	var role string
	if err = store.DB.QueryRow("SELECT role FROM principals WHERE id='service-portal'").Scan(&role); err != nil || role != "existing-role" {
		t.Fatal(role, err)
	}
	tx, err := store.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ok, err := canAgent(context.Background(), tx, "service-portal", "agent-b")
	if err != nil || !ok {
		t.Fatal("direct service permission missing", err)
	}
}

func TestReferenceLifetimeAndIndependentRoots(t *testing.T) {
	f := setup(t)
	one, err := f.server.Store.Create(context.Background(), "service-portal", "one", time.Now().Add(time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	two, err := f.server.Store.Create(context.Background(), "service-erp", "two", time.Now().Add(time.Minute).Unix())
	if err != nil {
		t.Fatal(err)
	}
	var lifetime int64
	if err = f.server.Store.DB.QueryRow("SELECT expires_at-created_at FROM delegation_contexts WHERE token_hash=?", digest(one)).Scan(&lifetime); err != nil || lifetime > 300 {
		t.Fatal(lifetime, err)
	}
	if err = f.server.Store.Revoke(one); err != nil {
		t.Fatal(err)
	}
	if !f.server.Store.Live(two) {
		t.Fatal("root revocation affected another workflow")
	}
	var stored string
	if err = f.server.Store.DB.QueryRow("SELECT token_hash FROM delegation_contexts WHERE workflow_id='two'").Scan(&stored); err != nil || stored == two {
		t.Fatal("raw reference persisted", err)
	}
}

func TestRootFailureRevokesReference(t *testing.T) {
	f := setup(t)
	var reference string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reference = r.Header.Get("X-Origin-Context")
		w.WriteHeader(500)
	}))
	defer upstream.Close()
	f.server.Upstream, _ = url.Parse(upstream.URL)
	r := httptest.NewRequest("POST", "/a2a/agent-a/jsonrpc", strings.NewReader(`{"method":"message/send"}`))
	r.Header.Set("Authorization", f.token(t, "service-erp", "service", nil))
	w := httptest.NewRecorder()
	f.server.PublicHandler().ServeHTTP(w, r)
	if w.Code != 500 || reference == "" || f.server.Store.Live(reference) {
		t.Fatal(w.Code, "reference survived failure")
	}
}
