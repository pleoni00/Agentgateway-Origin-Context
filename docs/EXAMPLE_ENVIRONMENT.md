# Example environment

This document describes the local demonstration deployment. The reusable extension consists of the Go control plane, SQLite schema and agentgateway configuration template. The Python services, agents, MCP servers, local model provider and Keycloak realm supply a runnable example.

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

## Example components and source files

| Component | Source | Behavior |
|---|---|---|
| Portal and ERP service clients | `services/portal_client.py`, `services/erp_client.py` | Obtain their own client-credentials token and invoke agents through the public Go entry point |
| Agent A | `agents/agent_a.py` | Entry/orchestration agent that can delegate to B and C |
| Agent B | `agents/agent_b.py` | Finance agent with document and invoice capabilities |
| Agent C | `agents/agent_c.py` | Document/reporting agent |
| Shared agent runtime | `agents/runtime.py`, `agents/common.py`, `agents/auth.py` | A2A/MCP/LLM clients, agent JWT acquisition and reference/workflow propagation |
| Finance MCP server | `mcp_servers/finance.py` | Exposes `get_invoice` and `approve_invoice` |
| Documents MCP server | `mcp_servers/documents.py` | Exposes `search_docs` and `read_doc` |
| MCP diagnostics | `mcp_servers/common.py` | Health and test observation/reset endpoints |
| LLM provider fixture | `llm_server/app.py` | Deterministic OpenAI-compatible responses for the example model aliases |
| Keycloak realm | `config/realm.json`, `scripts/generate_realm.py` | Example users, OAuth clients and trusted principal claim mappings |
| Initial application grants | `policies/seed.json` | One-time example policy data; not reconciled on startup |
| Environment wiring | `compose.yaml`, root `Dockerfile`, `requirements.lock` | Container build, trusted Docker network and reproducible Python fixtures |

The example diagnostic interfaces are for testing, not additional authorization entry points. In particular, held-request probes exercise actual outbound requests with the agent's own JWT and captured context; they are confined to the trusted local lab.

## Example flows

- `make demo`: ERP invokes A, which delegates through B/C and uses document tools for a report.
- Invoice approval: A delegates to B; ERP is authorized to approve while Portal is denied.
- Tool inspection: a held agent request discovers the intersection of origin/agent grants.
- Model calls: B can use `advanced` for ERP, while Portal and agent C remain limited to `standard`.
- Registry reload test: administration adds a second documents destination, discovers its tools, grants selected IDs and invokes it through agentgateway without restarting the gateway.

Run commands from the repository root:

```bash
make setup
make up
make test
make curl
make demo
make down
```

`make up` applies migrations and preserves existing data. `make down` stops containers and retains SQLite/Keycloak state. `make reset` deletes that state. See [CURLS.md](../CURLS.md) for the individual HTTP operations and [Architecture](ARCHITECTURE.md) for the distinction between extension and example components.
