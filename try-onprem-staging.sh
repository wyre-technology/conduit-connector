#!/usr/bin/env bash
#
# try-onprem-staging.sh — stand up an on-prem connector on THIS machine,
# point it at Conduit STAGING, have Conduit push it a connector config, and
# prove a tool call round-trips through the tunnel. Then tear down.
#
# This is the operator-driven flow (no wizard UI yet). You are the operator.
# Everything it needs it pulls from Azure with your existing `az login`.
#
# Usage:  ./try-onprem-staging.sh          # echo round-trip (no external deps)
#         ./try-onprem-staging.sh --keep   # leave the connector running at the end
#
set -euo pipefail

# --- staging fixtures (a standing test org + its primary user) ---
ORG="L-Fk69VfrntBjgVxOvWJX"                    # "WYRE Technology" staging org
USER_SUB="auth0|6a086bde989966bddbc05911"      # its primary (oldest) member
GATEWAY="https://staging.conduit.wyre.ai"
RELAY="wss://staging-wss.conduit.wyre.ai"
BIN="$(cd "$(dirname "$0")" && pwd)/conduit-connector"   # the darwin binary next to this script

KEEP=0; [ "${1:-}" = "--keep" ] && KEEP=1

say() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }

say "1/5  pulling staging secrets from Azure (uses your az login)"
ADMIN_KEY=$(az keyvault secret show --vault-name mcpgw-staging-kv -n admin-api-key --query value -o tsv 2>/dev/null)
JWT_SECRET=$(az containerapp secret show -n conduit-prod-staging-gateway -g rg-conduit-prod --secret-name jwt-secret --query value -o tsv 2>/dev/null | grep -v WARNING)
[ -n "$ADMIN_KEY" ] && [ -n "$JWT_SECRET" ] || { echo "could not read staging secrets — are you az-logged-in to the right subscription?"; exit 1; }

say "2/5  minting an IDENTITY-ONLY enrollment token (the v2 model: no capabilities in the token)"
TOKEN=$(curl -s -X POST "$GATEWAY/admin/onprem/enrollment-token" \
  -H "Authorization: Bearer $ADMIN_KEY" -H "Content-Type: application/json" \
  -d "{\"subtenantId\":\"$ORG\",\"ttlSeconds\":900}" \
  | python3 -c "import json,sys; d=json.load(sys.stdin); print('capabilities in token:', d['capabilities'], file=sys.stderr); print(d['token'])")
echo "   token minted (capabilities: none — that is the point)"

say "3/5  starting the connector on THIS machine, dialing staging (outbound only)"
LOG=$(mktemp)
RELAY_URL="$RELAY" ENROLLMENT_TOKEN="$TOKEN" LOG_LEVEL=info "$BIN" >"$LOG" 2>&1 &
CONN_PID=$!
cleanup() { [ "$KEEP" = "1" ] || kill "$CONN_PID" 2>/dev/null || true; }
trap cleanup EXIT
for _ in $(seq 1 30); do grep -q "tunnel registered" "$LOG" && break; sleep 1; done
grep -q "tunnel registered" "$LOG" || { echo "connector did not register:"; cat "$LOG"; exit 1; }
grep "tunnel registered" "$LOG" | tail -1
echo "   ^ online, ZERO capabilities — waiting for Conduit to push config"

say "4/5  pushing connector config from Conduit (this is what the wizard will do)"
curl -s -X PUT "$GATEWAY/admin/onprem/config" \
  -H "Authorization: Bearer $ADMIN_KEY" -H "Content-Type: application/json" \
  -d "{\"subtenantId\":\"$ORG\",\"connectors\":{\"echo\":{}}}" | python3 -m json.tool
for _ in $(seq 1 15); do grep -q "config applied" "$LOG" && break; sleep 1; done
grep "config applied" "$LOG" | tail -1
echo "   ^ the connector applied the pushed config"

say "5/5  calling a tool through the tunnel via $GATEWAY/v1/mcp"
BEARER=$(python3 - "$JWT_SECRET" "$USER_SUB" <<'PY'
import hmac,hashlib,base64,json,time,sys
b=lambda x: base64.urlsafe_b64encode(x).rstrip(b'=')
secret=sys.argv[1].encode(); sub=sys.argv[2]; now=int(time.time())
h=b(json.dumps({"alg":"HS256","typ":"JWT"},separators=(',',':')).encode())
p=b(json.dumps({"sub":sub,"scope":"echo","vendor":"echo","iss":"https://staging.conduit.wyre.ai","iat":now,"exp":now+900},separators=(',',':')).encode())
m=h+b'.'+p; s=b(hmac.new(secret,m,hashlib.sha256).digest()); print((m+b'.'+s).decode())
PY
)
RESP=$(curl -s -X POST "$GATEWAY/v1/mcp" -H "Authorization: Bearer $BEARER" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo__echo","arguments":{"message":"hello from my on-prem tunnel"}}}')
echo "$RESP" | python3 -c "import json,sys; d=json.load(sys.stdin); print('   ROUND-TRIP OK:', d['result']['content'][0]['text']) if 'result' in d else print('   ERROR:', json.dumps(d))"

if [ "$KEEP" = "1" ]; then
  say "connector left running (pid $CONN_PID). Tail it:  tail -f $LOG   |   stop it:  kill $CONN_PID"
else
  say "done — connector stopped, tunnel torn down. Re-run with --keep to leave it up."
fi
