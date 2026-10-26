#!/usr/bin/env bash
# Real authenticated requests; tokens are kept in temporary mode-600 files.
set -euo pipefail
cd "$(dirname "$0")/.."
python_bin=${PYTHON_BIN:-python3}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
for service in service-portal service-erp; do
  printf '\n=== %s: obtain its own Keycloak access token ===\n' "$service"
  curl --silent --show-error --fail http://localhost:8180/realms/lab/protocol/openid-connect/token \
    --data-urlencode grant_type=client_credentials --data-urlencode "client_id=$service" \
    --data-urlencode "client_secret=agentgateway-lab-$service-secret" > "$work/token.json"
  "$python_bin" - "$work" <<'PY'
import json, pathlib, sys
path = pathlib.Path(sys.argv[1])
token = json.loads((path / 'token.json').read_text())['access_token']
(path / 'headers').write_text('Authorization: Bearer ' + token + '\nContent-Type: application/json\n')
PY
  printf 'POST /a2a/agent-b/jsonrpc — inspect effective tools and models\n'
  curl --silent --show-error --fail http://localhost:8080/a2a/agent-b/jsonrpc \
    --header "@$work/headers" \
    --data '{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"kind":"message","messageId":"curl-inspect","role":"user","parts":[{"kind":"text","text":"{\"operation\":\"inspect\"}"}]}}}' | "$python_bin" -m json.tool
  printf '\nPOST /a2a/agent-b/jsonrpc — request the advanced model\n'
  curl --silent --show-error --fail http://localhost:8080/a2a/agent-b/jsonrpc \
    --header "@$work/headers" \
    --data '{"jsonrpc":"2.0","id":2,"method":"message/send","params":{"message":{"kind":"message","messageId":"curl-chat","role":"user","parts":[{"kind":"text","text":"{\"operation\":\"llm_chat\",\"model\":\"advanced\"}"}]}}}' | "$python_bin" -m json.tool
  printf '\nAdvanced is denied for Portal and allowed for ERP. The reference is now revoked.\n'
done
