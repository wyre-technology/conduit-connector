#!/usr/bin/env bash
#
# try-onprem-staging.sh — stand up an on-prem connector on THIS machine,
# point it at Conduit STAGING, have Conduit push it a connector config, and
# prove a tool call round-trips through the tunnel. Then tear down.
#
# This is the operator-driven flow (no wizard UI yet). You are the operator.
# Everything it needs it pulls from Azure with your existing `az login`.
#
# Usage:  ./try-onprem-staging.sh                       # echo round-trip (no external deps)
#         ./try-onprem-staging.sh --keep                # leave the connector running at the end
#         ./try-onprem-staging.sh --http-bridge          # ALSO run the http-bridge / egress
#                                                         # round-trip (Phase 2b). Needs a
#                                                         # SESSION_COOKIE — see PART 2 below.
#         ./try-onprem-staging.sh --http-bridge --keep   # both
#
set -euo pipefail

# --- staging fixtures (a standing test org + its primary user) ---
ORG="L-Fk69VfrntBjgVxOvWJX"                    # "WYRE Technology" staging org
USER_SUB="auth0|6a086bde989966bddbc05911"      # its primary (oldest) member
GATEWAY="https://staging.conduit.wyre.ai"
RELAY="wss://staging-wss.conduit.wyre.ai"
BIN="$(cd "$(dirname "$0")" && pwd)/conduit-tunnel"   # the darwin binary next to this script

KEEP=0; HTTP_BRIDGE=0
for arg in "$@"; do
  case "$arg" in
    --keep) KEEP=1 ;;
    --http-bridge) HTTP_BRIDGE=1 ;;
    *) echo "unknown flag: $arg (expected --keep and/or --http-bridge)"; exit 1 ;;
  esac
done

say() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }

# PART 2 (--http-bridge) needs a real Auth0-authenticated browser session,
# because `POST /api/orgs/:orgId/credentials/:vendor` (saving the tunnel-mode
# connection) is gated by `requireAuth0` — cookie-only, no Bearer/API-key
# alternative exists for this route (unlike the admin API used in PART 1).
# There is no non-interactive way to mint one against a real deployment, so:
#   1. Log into $GATEWAY in a browser as an admin of the org above (or any
#      org-admin member — it need not be $USER_SUB specifically, that
#      identity only matters for the /v1/mcp routing in PART 1/step 5).
#   2. DevTools → Application/Storage → Cookies → copy the VALUE of the
#      `gateway_session` cookie (it's an HMAC-signed string, not a JWT —
#      don't try to construct it by hand).
#   3. export SESSION_COOKIE='<that value>' and re-run with --http-bridge.
if [ "$HTTP_BRIDGE" = "1" ] && [ -z "${SESSION_COOKIE:-}" ]; then
  echo "--http-bridge requires SESSION_COOKIE (a live gateway_session cookie value)."
  echo "Log into $GATEWAY as an org-admin of $ORG, copy the 'gateway_session' cookie"
  echo "from DevTools, then: export SESSION_COOKIE='<value>' and re-run."
  exit 1
fi

say "1/5  pulling staging secrets from Azure (uses your az login)"
ADMIN_KEY=$(az keyvault secret show --vault-name mcpgw-staging-kv -n admin-api-key --query value -o tsv 2>/dev/null)
JWT_SECRET=$(az containerapp secret show -n conduit-prod-staging-gateway -g rg-conduit-prod --secret-name jwt-secret --query value -o tsv 2>/dev/null | grep -v WARNING)
[ -n "$ADMIN_KEY" ] && [ -n "$JWT_SECRET" ] || { echo "could not read staging secrets — are you az-logged-in to the right subscription?"; exit 1; }

say "2/5  minting an IDENTITY-ONLY enrollment token (the v2 model: no capabilities in the token)"
TOKEN=$(curl -s -X POST "$GATEWAY/admin/tunnel/enrollment-token" \
  -H "Authorization: Bearer $ADMIN_KEY" -H "Content-Type: application/json" \
  -d "{\"subtenantId\":\"$ORG\",\"ttlSeconds\":900}" \
  | python3 -c "import json,sys; d=json.load(sys.stdin); print('capabilities in token:', d['capabilities'], file=sys.stderr); print(d['token'])")
echo "   token minted (capabilities: none — that is the point)"

say "3/5  starting the connector on THIS machine, dialing staging (outbound only)"
LOG=$(mktemp)
RELAY_URL="$RELAY" ENROLLMENT_TOKEN="$TOKEN" LOG_LEVEL=info "$BIN" >"$LOG" 2>&1 &
CONN_PID=$!
STUB_PID=""
cleanup() {
  if [ "$KEEP" != "1" ]; then
    kill "$CONN_PID" 2>/dev/null || true
    [ -n "$STUB_PID" ] && kill "$STUB_PID" 2>/dev/null || true
  fi
  # Best-effort: don't leave a dummy itglue tunnel connection sitting in the
  # staging org's credential store. Harmless to skip if this fails (e.g. the
  # http-bridge scenario never got far enough to create one).
  if [ "$HTTP_BRIDGE" = "1" ] && [ -n "${SESSION_COOKIE:-}" ]; then
    curl -s -o /dev/null -X DELETE "$GATEWAY/api/orgs/$ORG/credentials/itglue" \
      -H "Cookie: gateway_session=$SESSION_COOKIE" || true
  fi
}
trap cleanup EXIT
for _ in $(seq 1 30); do grep -q "tunnel registered" "$LOG" && break; sleep 1; done
grep -q "tunnel registered" "$LOG" || { echo "connector did not register:"; cat "$LOG"; exit 1; }
grep "tunnel registered" "$LOG" | tail -1
echo "   ^ online, ZERO capabilities — waiting for Conduit to push config"

say "4/5  pushing connector config from Conduit (this is what the wizard will do)"
curl -s -X PUT "$GATEWAY/admin/tunnel/config" \
  -H "Authorization: Bearer $ADMIN_KEY" -H "Content-Type: application/json" \
  -d "{\"subtenantId\":\"$ORG\",\"connectors\":{\"echo\":{}}}" | python3 -m json.tool
for _ in $(seq 1 15); do grep -q "config applied" "$LOG" && break; sleep 1; done
grep "config applied" "$LOG" | tail -1
echo "   ^ the connector applied the pushed config"

# Mints a gateway access-token JWT the way `issueAccessToken` does, for the
# staging user above. MUST carry `aud` (RFC 8725 audience binding) — every
# verification call site (`resolveBearerUserId` in bearer-identity.ts,
# `resolveUserId`/`injectCredentials` in credential-injector.ts) requires it
# since the "Access tokens are now bound to this gateway via an aud claim"
# change (see CHANGELOG.md's [Unreleased]/Security in the conduit repo) —
# a token minted without `aud` now fails verification with no error detail
# (resolveBearerUserId swallows the jose.jwtVerify exception and returns
# null, which surfaces upstream as an auth failure, not a helpful message).
mint_bearer() {  # $1=scope  $2=vendor
  python3 - "$JWT_SECRET" "$USER_SUB" "$GATEWAY" "$1" "$2" <<'PY'
import hmac, hashlib, base64, json, time, sys
b = lambda x: base64.urlsafe_b64encode(x).rstrip(b'=')
secret, sub, iss, scope, vendor = sys.argv[1].encode(), *sys.argv[2:6]
now = int(time.time())
h = b(json.dumps({"alg": "HS256", "typ": "JWT"}, separators=(',', ':')).encode())
p = b(json.dumps(
    {"sub": sub, "scope": scope, "vendor": vendor, "iss": iss, "aud": iss, "iat": now, "exp": now + 900},
    separators=(',', ':'),
).encode())
m = h + b'.' + p
s = b(hmac.new(secret, m, hashlib.sha256).digest())
print((m + b'.' + s).decode())
PY
}

say "5/5  calling a tool through the tunnel via $GATEWAY/v1/mcp"
BEARER=$(mint_bearer echo echo)
RESP=$(curl -s -X POST "$GATEWAY/v1/mcp" -H "Authorization: Bearer $BEARER" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo__echo","arguments":{"message":"hello from my on-prem tunnel"}}}')
echo "$RESP" | python3 -c "import json,sys; d=json.load(sys.stdin); print('   ROUND-TRIP OK:', d['result']['content'][0]['text']) if 'result' in d else print('   ERROR:', json.dumps(d))"

if [ "$HTTP_BRIDGE" != "1" ]; then
  if [ "$KEEP" = "1" ]; then
    say "connector left running (pid $CONN_PID). Tail it:  tail -f $LOG   |   stop it:  kill $CONN_PID"
  else
    say "done — connector stopped, tunnel torn down. Re-run with --keep to leave it up, or --http-bridge for the egress scenario."
  fi
  exit 0
fi

# =============================================================================
# PART 2 — http-bridge / egress round-trip (Phase 2b, ONPREM_EGRESS_ENABLED)
#
# Proves the WHOLE chain: gateway rewrites a tunnel-mode vendor connection's
# base-URL header to a signed /egress/v1/<token> URL → the egress route
# unwraps it into an http/forward payload → relay → this connector's
# http-bridge built-in → a LAN host on the site's pushed allowlist. Here the
# "LAN host" is a stub HTTP server on 127.0.0.1, since the connector runs on
# THIS machine — from the connector's point of view that's exactly what a
# real customer's LAN server looks like.
#
# The stub returns a fake, non-IT-Glue-shaped 200 response, so the itglue-mcp
# container's OWN parsing of that body will likely fail and the tools/call
# below may come back as a JSON-RPC error. That is EXPECTED and does not mean
# the test failed — the proof this scenario is checking is "did the request
# reach the stub at all", not "did IT Glue's API contract get satisfied end
# to end". The assertion is on $STUB_LOG below, not on $RESP2.
# =============================================================================

say "6/7  starting a local HTTP stub (stands in for the customer's LAN vendor server)"
STUB_PORT=18899
STUB_LOG=$(mktemp)
STUB_PY=$(mktemp)
cat > "$STUB_PY" <<'PY'
import http.server, sys, json, datetime

port, logpath = int(sys.argv[1]), sys.argv[2]

class Handler(http.server.BaseHTTPRequestHandler):
    def _handle(self):
        length = int(self.headers.get('Content-Length', 0) or 0)
        body = self.rfile.read(length) if length else b''
        with open(logpath, 'a') as f:
            f.write(json.dumps({
                "time": datetime.datetime.utcnow().isoformat() + "Z",
                "method": self.command,
                "path": self.path,
                "headers": dict(self.headers.items()),
                "bodyLen": len(body),
            }) + "\n")
        self.send_response(200)
        self.send_header('Content-Type', 'application/vnd.api+json')
        self.end_headers()
        self.wfile.write(b'{"data": []}')
    do_GET = do_POST = do_PUT = do_DELETE = _handle
    def log_message(self, *a):
        pass  # keep the terminal quiet; STUB_LOG is the real record
http.server.HTTPServer(('127.0.0.1', port), Handler).serve_forever()
PY
python3 "$STUB_PY" "$STUB_PORT" "$STUB_LOG" &
STUB_PID=$!
sleep 1
echo "   stub listening on http://127.0.0.1:$STUB_PORT — requests logged to $STUB_LOG"

say "7/7a pushing http-bridge config: the stub is now this site's ONLY allowlisted host"
LOG_MARK=$(wc -l < "$LOG")
curl -s -X PUT "$GATEWAY/admin/tunnel/config" \
  -H "Authorization: Bearer $ADMIN_KEY" -H "Content-Type: application/json" \
  -d "{\"subtenantId\":\"$ORG\",\"connectors\":{\"echo\":{},\"http-bridge\":{\"hosts\":[{\"baseUrl\":\"http://127.0.0.1:$STUB_PORT\",\"insecureSkipVerify\":true}]}}}" \
  | python3 -m json.tool
for _ in $(seq 1 15); do
  tail -n "+$((LOG_MARK + 1))" "$LOG" | grep -q "config applied" && break
  sleep 1
done
tail -n "+$((LOG_MARK + 1))" "$LOG" | grep "config applied" | tail -1
echo "   ^ expect: msg=\"config applied\" ... applied=\"[echo http-bridge]\" (order may vary) error=<nil>"
echo "     If 'applied' is missing http-bridge, the connector binary predates"
echo "     the http-bridge built-in (need conduit-tunnel >= v0.4.0) — rebuild"
echo "     \$BIN from this branch (feat/http-bridge) or a >=0.4.0 release."

say "7/7b saving a TUNNEL-MODE IT Glue connection (egressMode=tunnel, lanBaseUrl=the stub)"
echo "   NOTE: tunnel-mode connections skip the vendor's live auth probe (it would"
echo "   dial the LAN from the cloud, which cannot reach it) — a fake apiKey is fine."
RESP1=$(curl -s -X POST "$GATEWAY/api/orgs/$ORG/credentials/itglue" \
  -H "Cookie: gateway_session=$SESSION_COOKIE" -H "Content-Type: application/json" \
  -d "{\"apiKey\":\"e2e-fake-key-do-not-use\",\"region\":\"us\",\"baseUrl\":\"http://127.0.0.1:$STUB_PORT\",\"egressMode\":\"tunnel\",\"lanBaseUrl\":\"http://127.0.0.1:$STUB_PORT\",\"insecureSkipVerify\":\"true\"}")
echo "$RESP1" | python3 -m json.tool
echo "$RESP1" | python3 -c "
import json, sys
d = json.load(sys.stdin)
if not d.get('stored'):
    print('   ERROR: connection was not stored — check ONPREM_EGRESS_ENABLED on', '$GATEWAY', 'and that the tunnel is online (409 = no online connector site).')
    sys.exit(1)
probe = d.get('probe', {})
print('   ^ stored — reachability probe:', probe)
if not probe.get('reachable'):
    print('   WARNING: probe says not reachable yet (tunnel may still be applying the')
    print('   config pushed in 7/7a) — the egress call below may 502/503 the first time.')
"

say "7/7c calling the gateway's /v1/mcp for an itglue tool — proving it crosses the tunnel"
BEARER2=$(mint_bearer itglue itglue)
TOOLS=$(curl -s -X POST "$GATEWAY/v1/mcp" -H "Authorization: Bearer $BEARER2" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}')
ITGLUE_TOOL=$(echo "$TOOLS" | python3 -c "
import json, sys
d = json.load(sys.stdin)
names = [t['name'] for t in d.get('result', {}).get('tools', []) if t['name'].startswith('itglue__')]
print(names[0] if names else '')
")
if [ -z "$ITGLUE_TOOL" ]; then
  echo "   no itglue__ tool found in tools/list — dumping the raw response for debugging:"
  echo "$TOOLS" | python3 -m json.tool
  exit 1
fi
echo "   using tool: $ITGLUE_TOOL"
RESP2=$(curl -s -X POST "$GATEWAY/v1/mcp" -H "Authorization: Bearer $BEARER2" -H "Content-Type: application/json" \
  -d "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"$ITGLUE_TOOL\",\"arguments\":{}}}")
echo "   tools/call response (an error here is fine — see PART 2's header comment):"
echo "$RESP2" | python3 -m json.tool

say "asserting the stub actually received the forwarded request"
sleep 1   # give the async tunnel hop a beat to land in the log
if [ -s "$STUB_LOG" ]; then
  echo "   ROUND-TRIP OK — the stub received $(wc -l < "$STUB_LOG" | tr -d ' ') request(s) through the tunnel:"
  cat "$STUB_LOG"
else
  echo "   FAIL — stub received nothing. Check (in order):"
  echo "     1. /egress/v1/<token> for a 401 (bad/expired EGRESS_TOKEN_SECRET mismatch),"
  echo "        503 'connector site offline', or 502 'http-bridge not enabled for this"
  echo "        site' — see docs/operations/onprem-connector-runbook.md §9 in conduit."
  echo "     2. The connector log ($LOG) for an http-bridge forward error (allowlist"
  echo "        rejection, refused connection to 127.0.0.1:$STUB_PORT, etc)."
  exit 1
fi

if [ "$KEEP" = "1" ]; then
  say "connector + stub left running (connector pid $CONN_PID, stub pid $STUB_PID)."
  echo "Tail connector:  tail -f $LOG   |   stub log:  tail -f $STUB_LOG"
  echo "Stop both:  kill $CONN_PID $STUB_PID"
else
  say "done — connector + stub stopped, tunnel torn down, itglue connection deleted."
fi
