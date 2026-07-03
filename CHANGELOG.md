# Changelog

All notable changes to this project will be documented in this file.
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- **`postgres` connector** (read-only): PostgreSQL/`pgx`-backed query /
  list_tables / describe_table, same read-only guard + row caps as `mssql`.
  Driver-agnostic query + MCP logic extracted to `internal/connectors/sqlcommon`
  so mssql and postgres share one implementation (only DSN + placeholder style
  differ).


- **`mssql` connector** (M-D, the Sage 100 Premium path): read-only SQL Server
  tools — `query` (single SELECT/WITH statement, comment/multi-statement
  smuggling refused, row caps), `list_tables`, `describe_table`. Config pushed
  via `config_update` (host/port/database/user/password/encrypt); lazy
  connection pool so a down database never blocks config application. The
  real enforcement is the site's read-only SQL principal; the code guard is
  belt-and-suspenders.

### Changed

- **Protocol v2 (breaking, pre-release)**: the agent now registers identity-only
  (`capabilities: []`) and receives connector config from Conduit via
  `config_update`/`config_ack` (pairs with conduit PR #633). `CAPABILITIES` is
  no longer an agent setting — boot fails loud if it is set, pointing at the
  legacy v1 container for the old behavior. Connector enablement is
  config-driven (`connectors.Registry`), in-memory: on restart the cloud
  re-pushes. Outbound wire shapes are pinned by tests (`capabilities: []` /
  `applied: []` must serialize as arrays — Go omitempty would drop them and
  the relay would treat the frame as a protocol violation).

### Added

- `transient_unavailable` register_nack reason: treated as retryable (reconnect-with-backoff) rather than permanent stop — pairs with conduit's relay hardening (conduit PR #632).

- Initial Go agent (M-B skeleton): frame-v1 tunnel client (dial, register,
  heartbeat, reconnect-with-backoff, request dispatch) faithfully ported from
  conduit's TypeScript `tunnel-client.ts`/`frame-protocol.ts`, built-in `echo`
  connector, six-guard env boot discipline, structured JSON logging.
  Verified against the production relay (registration + full `/v1/mcp` echo
  round-trip) on day one.
