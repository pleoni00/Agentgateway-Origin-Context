# Policy management and evaluation

The current authorization policy is implemented by Go code operating on SQLite action permissions and resource-group grants. Administrators change assignments and enablement through management APIs or explicit database administration. Agentgateway enforces the resulting decisions on routed traffic and MCP tool visibility.

## What is a policy in this implementation?

| Layer | Definition | Where it is changed |
|---|---|---|
| Identity requirements | Signature, issuer, audience, expiration, subject and trusted principal claims | Go verifier and trusted OIDC environment configuration |
| Application decision rules | Origin-context validation, required actions, direct agent access, resource intersections and supported operations | `authorization/server.go` and query helpers in `store.go` |
| Resource assignments | Which groups a principal receives, group contents and enabled flags | SQLite, with MCP and LLM management APIs |
| Gateway enforcement | extAuthz call, fail-closed behavior, native MCP filtering, header removal and routing | `gateway/config.yaml`, published by Go |
| Initial example policy data | Seed principals, resource groups, grants and action overrides | `policies/seed.json`, applied once |

The `policies/` directory contains bootstrap data, not executable rule documents. There is no stored `policy_ref`, rule-language interpreter or per-request selection of a named policy file. `principals.role` and `principals.domain` do not grant access; changing them alone has no authorization effect.

## Identities and trust

Keycloak signs identity JWTs. Go requires RS256, the configured issuer/audience, a subject, expiration and trusted `principal_name` / `principal_kind` claims. The principal must exist, be enabled and match the claimed kind. JWKS keys are cached for up to a minute; database grants are read on each decision. JWT validation does not perform token introspection.

Three credentials have separate purposes:

| Credential | Used by | Purpose |
|---|---|---|
| Keycloak access token | User, service or agent | Identify the immediate caller |
| `AUTHZ_ADMIN_TOKEN` | Administrator | Manage MCP and LLM resources/grants |
| `AUTHZ_CHECK_TOKEN` | Agentgateway | Authenticate its private `/check` request to Go |

An origin reference supplements the agent JWT; it is not a replacement identity credential. Its hash resolves the external origin and active workflow. Caller-supplied origin/caller labels cannot grant access: Go overwrites them after its decision. Agents forward only the reference/workflow with their own JWT, not the original caller's access token.

## Evaluation rules

Let `O` be the original user/service and `A` the immediate caller. For a direct user/service request, `A = O`. For a delegated request, `A` is an agent and `O` is resolved from the active reference.

For every operation:

1. Validate the caller JWT and enabled registry identity.
2. For agent requests, require a live reference matching origin, workflow, expiration, process owner and revocation state. The origin must remain enabled.
3. Require the origin to have direct permission to the acting agent.
4. Require the operation's action permission for both origin and caller.
5. Evaluate the requested agent, tool or model resource against the corresponding group grants.
6. Deny unknown operations/resources and database failures.

Within each resource family, a principal's effective group membership is the **union** of all its granted groups. In a delegated request, tool/model permissions are then the **intersection** of origin and current agent resources. Group identifiers do not imply hierarchy or inherit permissions from similarly named groups. There are no explicit deny-rule priorities; missing grants deny, and enabled/available checks impose additional restrictions.

### Required actions

| Request | Required action for origin and caller | Additional condition |
|---|---|---|
| `POST /a2a/{agent}/jsonrpc`, `message/send` | `a2a.invoke` | Both principals can invoke the target agent |
| MCP `initialize`, `notifications/initialized`, `ping`, `tools/list`; allowed GET/DELETE transport requests | `tools.read` | Tool visibility remains filtered by resource grants |
| MCP `tools/call` | `tools.execute` | Exact prefixed tool is in the allowed set |
| `GET /llm/v1/models` | `llm.read` | Return enabled aliases in the allowed set |
| `POST /llm/v1/chat/completions` | `llm.invoke` | Requested alias is allowed and `stream` is false |

MCP initialization also requires `tools.read`, even for a caller that intends only to execute a tool. Action permissions alone do not grant resources; resource groups alone do not grant the action. Observer demonstrates this distinction: it can discover document tools but cannot invoke them.

### Agent calls

`agent_grants` and `agent_group_members` determine which targets a principal can invoke. When A calls B for ERP, both ERP and A must be allowed to invoke B. When B subsequently calls a tool, ERP must also be allowed to access B directly. Delegation does not automatically grant the origin access to every downstream agent.

The acting agent's own permissions bound its outgoing requests. A can delegate to B even when A cannot invoke a finance tool itself; B's tool request is checked using `ERP ∩ B`, not a cumulative intersection of every earlier agent in the chain. The context records the origin and workflow, not a complete delegation path.

### Tool calls and discovery

```text
allowed_tools(O, A) = tools_granted_to(O)
                      ∩ tools_granted_to(A)
                      ∩ enabled_tools
                      ∩ available_tools
                      ∩ tools_on_enabled_servers
```

Go groups this set by MCP server ID. For `tools/call`, it checks the exact name `{server_id}_{tool_name}`. Agentgateway receives the same allowed set through extAuthz metadata and uses native `mcpAuthorization` to filter discovery and invocation. Being able to initialize an MCP session does not bypass tool-level enforcement.

Example: Portal has document tools and `finance/get_invoice`; B also has `finance/approve_invoice`. Portal calling through B can read an invoice but cannot approve it, because Portal lacks that tool grant.

The current policy checks tool identity, not business conditions on individual argument values. A tool group cannot express rules such as a maximum invoice amount or a restriction to a specific customer record.

### Model calls and discovery

```text
allowed_models(O, A) = models_granted_to(O)
                       ∩ models_granted_to(A)
                       ∩ enabled_models
```

Example: ERP and B share `standard` and `advanced`; ERP and C share only `standard`. `GET /llm/v1/models` returns the applicable public aliases, and invocation checks the same set. Public model aliases are the policy resources; provider-private model IDs are configured in the gateway template. Any added provider failover must preserve the authorization meaning of the alias.

## Changing assignments

All `/admin/*` requests use the administrator secret, independently of principal action permissions. See [runnable curl examples](../CURLS.md).

| Change | Management operation | Authorization effect |
|---|---|---|
| Register/update MCP destination | `PUT /admin/mcp/servers/{id}` | Publishes routing; does not create grants; endpoint changes invalidate the old catalog |
| Refresh tool catalog | `POST /admin/mcp/servers/{id}/sync` | New tools have no grants; missing tools become unavailable; existing explicit disables survive |
| Disable a server | Same server PUT with `enabled: false` | All server tools denied on subsequent decisions |
| Remove a server | `DELETE /admin/mcp/servers/{id}` | Removes its tool rows and memberships; republishes destinations |
| Disable a tool | `PATCH /admin/mcp/tools/{id}` with `enabled: false` | Denies that tool independently of group membership |
| Replace tool group | `PUT /admin/mcp/groups/{group}` with `tools` and `principals` | Atomically replaces the group's entire membership and grantee list |
| Remove tool group | `DELETE /admin/mcp/groups/{group}` | Removes the group, memberships and grants |
| Replace model group | `PUT /admin/llm/groups/{group}` with `models` and `principals` | Atomically replaces the group's entire membership and grantee list |
| Remove model group | `DELETE /admin/llm/groups/{group}` | Removes the group and cascading assignments |
| Disable model | `PATCH /admin/llm/models/{model}` with `enabled: false` | Denies the model even if a granted group still contains it |
| Republish configuration | `POST /admin/mcp/apply` | Rebuilds routing from the registry/template; changes no grants |

PUT group payloads are replacements, not incremental additions. Include all members and grantees that should remain. Empty arrays clear assignments. A principal may receive the same resource through another group: removing one grant does not revoke a resource still granted elsewhere. Creating a dedicated tool group with a single user's ID supports individual assignments using the same evaluation rules.

Principal registration/enablement, `principal_actions`, agent groups/grants and initial model catalog IDs currently require explicit database administration; there are no HTTP CRUD APIs for those entities. Keycloak identity registration and application grants are separate configuration tasks. Model provider destinations still require a template update and publication.

Database assignment changes affect subsequent decisions, including requests under an existing active context. Already authorized in-flight operations are not cancelled. Configuration publication/reload is asynchronous and distinct from the immediate DB authorization check.

## ExtAuthz contract

The private endpoint is `POST /check` on the Go service's port 8000. The request Authorization header carries `AUTHZ_CHECK_TOKEN`. Its JSON body separately carries the original request's caller Authorization value and context:

```json
{
  "authorization": "Bearer <caller-access-token>",
  "reference": "<opaque-origin-reference-for-agent-requests>",
  "workflow": "wf-example",
  "path": "/mcp/",
  "method": "POST",
  "rpc": {
    "method": "tools/call",
    "params": {"name": "finance_get_invoice", "arguments": {"invoice_id": "INV-001"}}
  }
}
```

A successful response includes `origin`, `caller`, `allowed_tools` (server ID to tool-name arrays) and `allowed_models` (public alias array). The gateway configuration extracts the allowed sets into trusted extAuthz metadata. It does not trust client-supplied allowed-resource headers. MCP outbound calls remove the caller Authorization header and origin reference before reaching the MCP server.

Invalid public identity tokens return 401. Application denials return 403; decision/storage failures return 503. The gateway's external authorization is configured to deny with 503 on check failure and uses a 2-second request timeout. These failure decisions are separate from backend health/eviction.

## Current policy scope

The current database models principal/action/resource-group permissions. It has no use-case entity, user-group membership, arbitrary entitlement labels, generic policy documents or user/use-case/agent/tool rule bindings. The supported behavior is defined by the code and tables described above. Backend health and circuit-breaking configuration belong to agentgateway; they do not grant or revoke application access. See [resilience status](ARCHITECTURE.md#resilience-and-circuit-breaking).
