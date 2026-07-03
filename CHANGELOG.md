# Changelog

All notable changes to this project will be documented in this file.
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- Initial Go agent (M-B skeleton): frame-v1 tunnel client (dial, register,
  heartbeat, reconnect-with-backoff, request dispatch) faithfully ported from
  conduit's TypeScript `tunnel-client.ts`/`frame-protocol.ts`, built-in `echo`
  connector, six-guard env boot discipline, structured JSON logging.
  Verified against the production relay (registration + full `/v1/mcp` echo
  round-trip) on day one.
