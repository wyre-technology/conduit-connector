# conduit-connector

The Conduit on-prem connector — a single-binary Go agent that dials **out** over
WSS and bridges on-prem systems (Sage 100/MSSQL, Veeam, …) into
[Conduit](https://conduit.wyre.ai). It binds **no inbound port**: as long as
outbound 443 works, the site is connected. No firewall holes, ever.

**Build contract:** `docs/onprem-connector-v1.md` in the
[conduit](https://github.com/wyre-technology/conduit) repo. This repo is the Go
agent named there (milestone M-B onward).

## Install (Linux)

`install.sh` downloads the binary from the latest [GitHub release](https://github.com/wyre-technology/conduit-connector/releases/latest),
installs it as a hardened systemd service, and starts it. It reads its two
settings from the environment, so an RMM can set site variables and run it
unattended:

```
curl -fsSL https://raw.githubusercontent.com/wyre-technology/conduit-connector/main/install.sh | \
  RELAY_URL=wss://conduit-wss.wyre.ai \
  ENROLLMENT_TOKEN=<mint in Conduit: site → Deploy connector> \
  bash
```

Optional: `CONNECTOR_URL` (a direct/signed binary link, e.g. from the Conduit
wizard) or `CONNECTOR_VERSION` (a GitHub Release tag; default `latest`). The
Windows amd64 binary in each release is **Authenticode-signed** (Azure Artifact
Signing); a Windows service wrapper is the M-E follow-up.

## Run directly (protocol v2)

```
RELAY_URL=wss://conduit-wss.wyre.ai \
ENROLLMENT_TOKEN=<identity-only token minted in Conduit> \
./conduit-connector
```

Enrollment is **identity-only** — the token binds the org, not capabilities.
The connector comes online empty; Conduit pushes which connectors to run and
their config over the tunnel (the wizard, or the admin API). There is **no
`CAPABILITIES` env var** — the connector boot-fails if it is set (that was the
legacy v1 container). Boot otherwise refuses loudly on missing/invalid config.

## Built-in connectors

Compiled in — no plugins, no sidecars. Enabled per-site via cloud-pushed config.

| Slug | What it does |
|---|---|
| `echo` | Connectivity proof (round-trips its input). |
| `mssql` | Read-only SQL Server (Sage 100 Premium) — `query` / `list_tables` / `describe_table`. |
| `postgres` | Read-only PostgreSQL — same three tools. |
| `mysql` | Read-only MySQL/MariaDB — same three tools. |
| `mcp-proxy` | Fronts any local stdio MCP server (e.g. the Veeam MCP server): spawns `{command, args, env, cwd}`, does the MCP handshake, forwards its tools. |

The SQL connectors share `internal/connectors/sqlcommon` (one read-only MCP +
query implementation; each driver package is just its DSN + placeholder style).

### Named instances (multiple connectors of one type)

A connector's config key is its **slug** (the `slug__tool` prefix clients see).
By default the slug also names the built-in. To run **several instances of one
built-in**, add a `type` field — the slug becomes a free-form name and `type`
selects the built-in:

```json
{
  "veeam-vbr": { "type": "mcp-proxy", "command": "node", "args": ["/opt/vbr-mcp/build/index.js"], "env": { "PRODUCT_NAME": "vbr", "...": "..." } },
  "veeam-one": { "type": "mcp-proxy", "command": "node", "args": ["/opt/vone-mcp/build/index.js"], "env": { "PRODUCT_NAME": "vone", "...": "..." } }
}
```

Their tools surface as `veeam-vbr__…` and `veeam-one__…`. Omit `type` and the
slug is the type (`{"postgres": {...}}` → the `postgres` built-in), so every
existing config is unchanged.

## Layout

- `cmd/conduit-connector` — entry point, env guards, service lifecycle
- `internal/tunnel` — frame protocol (v1 + v2) + WSS client: dial, register,
  heartbeat (30s), reconnect (1s→30s backoff), request dispatch, config apply.
- `internal/connectors` — the built-in connectors + the config-driven registry.

## Development

```
go build -o conduit-connector ./cmd/conduit-connector   # ~9 MB static binary
go test ./...
```

First light: 2026-07-02 — this agent registered against the production relay
and carried a full `/v1/mcp` echo round-trip
(`gateway → relay → WSS → connector → echo → back`) on the day the tunnels
went live in prod.
