# conduit-connector

The Conduit on-prem connector — a single-binary Go agent that dials **out** over
WSS and bridges on-prem systems (Sage 100/MSSQL, Veeam, …) into
[Conduit](https://conduit.wyre.ai). It binds **no inbound port**: as long as
outbound 443 works, the site is connected. No firewall holes, ever.

**Build contract:** `docs/onprem-connector-v1.md` in the
[conduit](https://github.com/wyre-technology/conduit) repo. This repo is the Go
agent named there (milestone M-B onward).

## Run (v1-compatible env config)

```
RELAY_URL=wss://conduit-wss.wyre.ai \
ENROLLMENT_TOKEN=<minted in Conduit> \
CAPABILITIES=echo \
./conduit-connector
```

Boot refuses loudly on any missing/invalid config (six-guard discipline ported
from conduit `src/onprem/index.ts`), including a capability with no built-in
connector.

> Frame v2 (identity-only enrollment + cloud-pushed config via the Conduit
> wizard) replaces `CAPABILITIES` — the env form remains for v1 relay compat
> and headless testing.

## Layout

- `cmd/conduit-connector` — entry point, env guards, service lifecycle
- `internal/tunnel` — frame protocol (v1) + WSS client: dial, register,
  heartbeat (30s), reconnect (1s→30s backoff), request dispatch. Faithful port
  of conduit `src/onprem/tunnel-client.ts` + `src/relay/frame-protocol.ts` —
  keep the two in lockstep until frame v2 lands.
- `internal/connectors` — built-in connectors, compiled in (no plugins, no
  sidecars). v1: `echo`; next: `mssql`/`sage100` (read-only), `veeam`.

## Development

```
go build -o conduit-connector ./cmd/conduit-connector   # ~9 MB static binary
go test ./...
```

First light: 2026-07-02 — this agent registered against the production relay
and carried a full `/v1/mcp` echo round-trip
(`gateway → relay → WSS → connector → echo → back`) on the day the tunnels
went live in prod.
