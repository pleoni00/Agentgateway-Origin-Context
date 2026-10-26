# Authenticated example requests

Start the example with `make setup && make up`. `make curl` performs the service-token, discovery and model-invocation flows without printing tokens.

## Service -> agent -> model

Obtain the service's own access token:

```bash
TOKEN=$(curl -fsS http://localhost:8180/realms/lab/protocol/openid-connect/token \
  --data-urlencode grant_type=client_credentials \
  --data-urlencode client_id=service-portal \
  --data-urlencode client_secret=agentgateway-lab-service-portal-secret \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')
```

Invoke B. The Go entry point creates the reference, B sends it alongside its own token, and the LLM proxy refuses `advanced` for Portal:

```bash
curl -sS http://localhost:8080/a2a/agent-b/jsonrpc \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -H 'X-Workflow-Id: wf-example-portal' \
  --data '{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"kind":"message","messageId":"example","role":"user","parts":[{"kind":"text","text":"{\"operation\":\"llm_chat\",\"model\":\"advanced\"}"}]}}}'
```

The outer A2A message can succeed while its structured result reports the downstream `403`. Replace the client ID/secret with `service-erp` to obtain an allowed response. Use `{"operation":"inspect"}` as the message text to discover the tools and models visible during the request.

## Direct model discovery

Services/users can also call LLM endpoints with their own token, without a delegation reference:

```bash
curl -fsS http://localhost:8080/llm/v1/models -H "Authorization: Bearer $TOKEN"
curl -sS http://localhost:8080/llm/v1/chat/completions \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  --data '{"model":"standard","messages":[{"role":"user","content":"Hello"}]}'
```

An agent token requires an active reference. Arbitrarily setting `X-Origin-Service: service-erp` cannot grant additional rights.

## LLM group administration

Load the generated local secret into your shell. Do not pass it to agents:

```bash
set -a
source .env
set +a

curl -fsS http://localhost:8080/admin/llm \
  -H "Authorization: Bearer $AUTHZ_ADMIN_TOKEN"

curl -fsS -X PUT http://localhost:8080/admin/llm/groups/project-x \
  -H "Authorization: Bearer $AUTHZ_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  --data '{"models":["standard","advanced"],"principals":["service-portal","agent-b"]}'
```

The PUT replaces all assignments of that group atomically. Portal and B now share `advanced`; other agents still need their own grant. Changes apply to existing active contexts on their next call.

Remove the additional grant:

```bash
curl -fsS -X DELETE http://localhost:8080/admin/llm/groups/project-x \
  -H "Authorization: Bearer $AUTHZ_ADMIN_TOKEN"
```

Disable a model globally:

```bash
curl -fsS -X PATCH http://localhost:8080/admin/llm/models/advanced \
  -H "Authorization: Bearer $AUTHZ_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' --data '{"enabled":false}'
```

Use `{"enabled":true}` to restore it. Unauthorized requests must produce no execution event at `http://localhost:9400/_lab/events`.
