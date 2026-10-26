# Agentgateway Origin Context

A Go control plane adds verified caller propagation, request-scoped origin references, and database-backed permissions to [agentgateway](https://github.com/agentgateway/agentgateway). Users, services and agents authenticate with Keycloak. Agentgateway routes A2A, federates MCP tools and proxies LLM requests; the control plane owns the application authorization model.

The repository is self-contained and includes a local demonstration environment.

## Documentation

| Document | Contents |
|---|---|
| [Architecture](docs/ARCHITECTURE.md) | Functional components, implementation map, request flows, generated gateway configuration and resilience status |
| [Database](docs/DATABASE.md) | All 17 tables, columns, relationships, migrations and persistence semantics |
| [Policy management](docs/POLICIES.md) | Action/resource rules, origin/agent intersections, administration and gateway enforcement |
| [Example environment](docs/EXAMPLE_ENVIRONMENT.md) | Demonstration services, agents, MCP servers, identities and deployment endpoints |
| [Curl examples](CURLS.md) | Runnable authentication, invocation and administration requests |

## Component responsibilities

| Component | Responsibility |
|---|---|
| Go control plane | Registry, tool discovery for administration, group management, authorization, origin-reference lifecycle and gateway configuration publication |
| Agentgateway | A2A/LLM routing, MCP federation and native enforcement of the allowed tool set |
| SQLite | Authoritative catalogs, action permissions, resource groups/grants and hashed origin references |
| Keycloak | Authentication and identity JWT issuance |

MCP server endpoints and tool permissions are administered through Go. Agentgateway reads the generated configuration and calls Go for authorization; it does not read SQLite directly. The current template has no backend eviction policy enabled. Circuit-breaking execution belongs to agentgateway; see [resilience and circuit breaking](docs/ARCHITECTURE.md#resilience-and-circuit-breaking).

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

SQLite is the authoritative, persistent application database at `authorization/data/permissions.db`. It contains principals, action permissions, agent/tool groups and grants, LLM models/groups/grants, and hashed origin references. The MCP registry also stores server endpoints, discovery timestamps, tool definitions and availability. Only SHA-256 hashes of 256-bit random references are persisted. References expire within five minutes and no later than the initial JWT expiration.

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

The [example environment](docs/EXAMPLE_ENVIRONMENT.md) documents the agents, services, MCP servers, Keycloak identities, initial grants, model fixture and local endpoints.

## Managing LLM groups

Management requests use `AUTHZ_ADMIN_TOKEN` from `.env`, independently of caller JWTs. Agents never receive this secret.

- `GET /admin/llm`: list configured models, groups, memberships and grants.
- `PUT /admin/llm/groups/{group}`: atomically replace a group's `models` and `principals` arrays. Models and principals must already exist.
- `DELETE /admin/llm/groups/{group}`: remove the group and its assignments.
- `PATCH /admin/llm/models/{model}` with `{"enabled": false}`: disable a model immediately.

These are management APIs, not an administrative UI. To add a provider/model, configure it in agentgateway and register the corresponding model ID in `llm_models`; routing configuration and authorization grants are deliberately distinct. MCP servers, tool discovery and tool groups are managed through the Go APIs below. Agent groups remain managed in the DB. See [CURLS.md](CURLS.md) for runnable examples.

## Managing MCP servers and tools

The Go control plane is the single administration entry point for MCP destinations and tool permissions. It publishes the database endpoints into agentgateway's configuration; agentgateway continues to handle MCP federation, filtered discovery and invocation.

Management requests use the same `AUTHZ_ADMIN_TOKEN` as LLM administration:

| Endpoint | Purpose |
|---|---|
| `GET /admin/mcp` | List servers, tools, definitions, groups, memberships and grants |
| `PUT /admin/mcp/servers/{id}` | Register or replace an endpoint and optional `enabled` flag (defaults to true) |
| `POST /admin/mcp/servers/{id}/sync` | Initialize an MCP session and atomically refresh all tool pages |
| `DELETE /admin/mcp/servers/{id}` | Remove the server, its tools and their group memberships |
| `PATCH /admin/mcp/tools/{id}` | Enable or disable a tool with `{"enabled": false}` |
| `PUT /admin/mcp/groups/{group}` | Replace `tools` (numeric catalog IDs) and `principals` arrays atomically |
| `DELETE /admin/mcp/groups/{group}` | Remove a tool group and its assignments |
| `POST /admin/mcp/apply` | Republish the desired registry configuration |

Registration publishes routing but does not discover or authorize tools automatically. Run `sync`, inspect the catalog and assign selected tool IDs to groups. For delegated calls, both the origin and the acting agent still need the relevant group grants and action permissions. Passing empty arrays clears a group's assignments.

Discovery uses the official Go MCP SDK with Streamable HTTP, including initialization, sessions, JSON/SSE responses and paginated `tools/list`. Definitions are stored as JSON text in the `definition` column. Refresh preserves existing IDs, memberships and explicit disables; new tools have no group membership. Missing tools become `available=0` and are immediately denied. A failed discovery leaves the previous catalog unchanged. Changing a server endpoint invalidates its existing catalog until a successful sync. Disabling a server immediately denies all of its tools, including calls under existing contexts.

`gateway/config.yaml` is a **template** for the fixed A2A, LLM and enforcement configuration. Its MCP target list is empty. At startup and after server changes, Go renders the complete configuration to the ignored `authorization/data/gateway/config.json` using an atomic replacement. Compose mounts the generated directory read-only into agentgateway, which watches the file and reloads routing without a restart. See the upstream [configuration architecture](https://github.com/agentgateway/agentgateway/blob/v1.6.0/architecture/configuration.md). `make env` creates the directory as the local user before Docker mounts it. Do not edit the generated file or manage its targets through a second writer/UI. After changing the template, call `apply` or restart the control plane.

A successful `published` response means the file was written; reload is asynchronous and is not an acknowledgement from the running gateway. The DB and filesystem are not one transaction: if publication fails after registration/removal, the API returns 503 explicitly stating that the DB change was saved. Retry `apply` after correcting the file/template problem. Startup also rebuilds the file from SQLite. Disables and removals are enforced by authorization even while the gateway has older routing; endpoint changes deny old catalog entries until resynchronized. This file publisher assumes a single control-plane process and a trusted template.

The registry currently manages HTTP(S) Streamable HTTP endpoints without upstream authentication. URLs containing embedded credentials, query strings, fragments or environment interpolation are rejected. Server IDs use letters, digits and hyphens, up to 64 characters. Endpoint registration is a trusted administrator operation and can reach private network addresses. Upstream OAuth, secret distribution, stdio/legacy SSE transports, periodic discovery, and management of LLM provider routing are outside this registry API. The example endpoints and initial permissions are bootstrapped once by the seed; subsequent administration does not require editing the seed or gateway targets.

## Source map and verification

- `authorization/`: Go identity verification, authorization, public proxy, management APIs, versioned migrations and unit tests.
- `gateway/`: agentgateway image and fixed routing/enforcement template; MCP destinations come from SQLite.
- `policies/seed.json`: one-time example data.
- `config/realm.json`: independent Keycloak realm fixture.
- `agents/`, `services/`, `mcp_servers/`, `llm_server/`: example application components.
- `tests/`: live tests for discovery/invocation, origin/agent intersections, users, revocation, tampering, model groups and failure handling.
- `scripts/`: startup readiness, local secret generation and redacted curl flows.

`make test-go` exercises the control plane without Docker, including JWT validation, cancellation, pre-response revocation, failed DB writes and atomic management. `make test-integration` requires the running example. `make check` validates Go, Python, shell and Compose configuration.

## License and contributions

Original project code is licensed under [Apache-2.0](LICENSE). See [CONTRIBUTING.md](CONTRIBUTING.md) for development and validation instructions. Agentgateway and other dependencies retain their respective licenses.
