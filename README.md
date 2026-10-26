# Agentgateway Origin Context

A Go control plane adds verified caller propagation, request-scoped origin references, and database-backed permissions to [agentgateway](https://github.com/agentgateway/agentgateway). Users, services and agents authenticate with Keycloak. Agentgateway routes A2A, federates MCP tools and proxies LLM requests; the control plane owns the application authorization model.

The repository is self-contained and includes a local demonstration environment.

## Request flow

```mermaid
sequenceDiagram
    participant S as User / service
    participant G as Go control plane
    participant P as Agentgateway
    participant A as Agent
    S->>G: Own access token + A2A message
    G->>G: Verify identity and grants; create origin reference
    G->>P: Request + verified labels + reference
    P->>G: Internal extAuthz check
    P->>A: Authorized A2A message
    A->>G: Own agent token + reference + workflow
    G->>P: Authorized agent / tool / LLM request
    P->>G: Internal extAuthz check
    P-->>A: Result
    A-->>P: Final response
    P-->>G: Final response
    G->>G: Revoke origin reference before returning headers
    G-->>S: Response without reference
```

The Go component exposes a public entry point on port 8080 and a private `extAuthz` endpoint on port 8000. The underlying agentgateway listener is not published to the host. Both functions run in the same Go process. The public proxy is required for reference lifecycle management; an authorization check alone cannot observe root completion.

## Authorization rules

- JWTs must have a valid RS256 signature, issuer, audience, subject, expiration, and issuer-controlled `principal_name` / `principal_kind` claims.
- The enabled database principal must match the verified name and kind. Caller-supplied `X-Origin-Service` and `X-Caller` values are overwritten.
- External users/services cannot supply an origin reference. The gateway creates one only for synchronous `POST /a2a/{agent}/jsonrpc` requests using `message/send`.
- Agents must send their own access token, `X-Origin-Context` and `X-Workflow-Id` on every subsequent request.
- The origin must have direct access to the calling agent. Both the origin and the calling principal must be allowed to invoke any downstream agent.
- Effective tools and models are the intersection of the origin's grants and the current agent's grants. Permissions are read on each request, without a permissions cache.
- MCP discovery uses the same effective tool set as invocation. Agentgateway's native `mcpAuthorization` applies the external decision to each tool.
- LLM discovery returns the effective model set; unauthorized, disabled and unknown models cannot be invoked. Provider credentials remain in agentgateway.
- Direct user/service MCP and LLM requests use that principal's own grants and do not create a delegation reference.

## Persistence and lifecycle

Action permissions (`a2a.invoke`, `tools.read`, `tools.execute`, `llm.read`, `llm.invoke`) are checked for both the origin and the actor. Resource membership does not grant an action by itself; Observer can discover document tools but cannot execute them.

SQLite is the authoritative, persistent application database at `authorization/data/permissions.db`. It contains principals, action permissions, agent/tool groups and grants, LLM models/groups/grants, and hashed origin references. Only SHA-256 hashes of 256-bit random references are persisted. References expire within five minutes and no later than the initial JWT expiration.

The explicit `migrate` command applies embedded, transactional, versioned SQL migrations before serving traffic. `policies/seed.json` initializes the example once. Permission revocations are never restored by startup or repeated migrations. Back up the DB before upgrades.

The root response closes the reference before headers are sent. Failures and client cancellation also close it. A local active-reference fence closes references even if the revocation DB write fails. References left open by a previous process are revoked on startup. An already authorized operation is not cancelled by reference revocation.

This implementation requires **one Go control-plane process/replica**, synchronous A2A messages and non-streaming LLM calls. Streaming, asynchronous tasks, durable execution, and multiple replicas are outside this implementation. Agentgateway and provider-side automatic fallbacks must not introduce model destinations that bypass the authorization policy.

## Run

Requires Docker Compose, Python 3.12, and Go 1.25 for local Go checks.

```bash
git clone https://github.com/pleoni00/Agentgateway-Origin-Context.git
cd Agentgateway-Origin-Context
make setup
make up
make test
make curl
make demo
make down
```

`make up` generates independent admin/check secrets in ignored `.env`, runs DB migrations, starts the example, and waits for Keycloak. It preserves existing secrets and data. `make down` preserves the DB and Keycloak volume; `make reset` explicitly deletes them.

The example uses agentgateway 1.6.0 pinned by digest, Keycloak 26.3.2, three deterministic A2A agents, two MCP servers, and a deterministic OpenAI-compatible LLM provider fixture. It makes no paid provider calls. Agentgateway's native LLM proxy maps `standard` and `advanced` to the fixture's `demo-standard` and `demo-advanced` models.

| Identity | Agent access | Tools | LLM models |
|---|---|---|---|
| Portal | A/B/C | Documents + invoice read | standard |
| ERP | A/B/C | Documents + invoice read/approval | standard, advanced |
| A | B/C | Documents | standard, advanced |
| B | C | Documents + invoice read/approval | standard, advanced |
| C | None | Documents | standard |
| Alice | A | Documents | standard |
| Observer | None | Documents discovery only | None |
| Outsider | None | None | None |

Services and agents use OAuth client credentials. Each example client secret is `agentgateway-lab-<principal>-secret`. Browser users use `lab-browser` with Authorization Code + PKCE. The separate `lab-test-password` client enables password grants solely for local integration tests. Example users share `Agentgateway-Lab-User-2026!`.

Local endpoints:

| Endpoint | Address |
|---|---|
| Public Go entry point | http://localhost:8080 |
| Keycloak | http://localhost:8180 |
| MCP backend diagnostics | localhost:9101, localhost:9102 |
| Agent diagnostics | localhost:9201, localhost:9202, localhost:9203 |
| LLM fixture diagnostics | localhost:9400 |

Keycloak admin is `admin` / `Agentgateway-Lab-Admin-2026!`. All example host ports bind to loopback. Backend diagnostic endpoints are trusted test interfaces, including captured-context probes; they must not be exposed to untrusted clients. Production deployments need HTTPS, secret management, appropriately isolated networks and a Keycloak realm without the password-grant fixture. The `lab` Docker network is a trusted deployment boundary.

## Managing LLM groups

Management requests use `AUTHZ_ADMIN_TOKEN` from `.env`, independently of caller JWTs. Agents never receive this secret.

- `GET /admin/llm`: list configured models, groups, memberships and grants.
- `PUT /admin/llm/groups/{group}`: atomically replace a group's `models` and `principals` arrays. Models and principals must already exist.
- `DELETE /admin/llm/groups/{group}`: remove the group and its assignments.
- `PATCH /admin/llm/models/{model}` with `{"enabled": false}`: disable a model immediately.

These are management APIs, not an administrative UI. To add a provider/model, configure it in agentgateway and register the corresponding model ID in `llm_models`; routing configuration and authorization grants are deliberately distinct. Existing agent/tool groups can be managed in the DB; only LLM group management has an HTTP API in this version. See [CURLS.md](CURLS.md) for runnable examples.

## Source map and verification

- `authorization/`: Go identity verification, authorization, public proxy, management APIs, versioned migrations and unit tests.
- `gateway/`: agentgateway image and enforcement/routing configuration.
- `policies/seed.json`: one-time example data.
- `config/realm.json`: independent Keycloak realm fixture.
- `agents/`, `services/`, `mcp_servers/`, `llm_server/`: example application components.
- `tests/`: live tests for discovery/invocation, origin/agent intersections, users, revocation, tampering, model groups and failure handling.
- `scripts/`: startup readiness, local secret generation and redacted curl flows.

`make test-go` exercises the control plane without Docker, including JWT validation, cancellation, pre-response revocation, failed DB writes and atomic management. `make test-integration` requires the running example. `make check` validates Go, Python, shell and Compose configuration.

## License and contributions

Original project code is licensed under [Apache-2.0](LICENSE). See [CONTRIBUTING.md](CONTRIBUTING.md) for development and validation instructions. Agentgateway and other dependencies retain their respective licenses.
