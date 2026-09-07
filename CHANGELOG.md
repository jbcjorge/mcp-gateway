# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Composite backends (compositions).** A route can now combine multiple
  servers under one namespace. Tool lists (and resources/prompts) merge across
  members with **last-one-wins** on name collision (intentional override); tool
  calls route to the owning member. See `docs/design/compositions.md`.
- **Route-level tool prefixing.** A `backends` route entry may set
  `{"route": "<name>", "prefix": "<string>"}` to advertise its tools as
  `<prefix><tool>` (translated back on call), avoiding client-side collisions
  when consuming multiple routes.

### Changed

- **BREAKING: backends config schema.** The flat `{name: def}` map is replaced by
  `{ "servers": {...}, "compositions": {...}, "backends": [...] }`. `servers` and
  `compositions` are definitions; only `backends` entries are exposed as routes
  (lazy resolution — unreferenced definitions are never started). The `disabled`
  field is gone: to disable a backend, omit it from `backends`. Client→gateway
  auth is keyed by route.

## [0.2.0]

### Added

- **CA bundle injection** (`ca_bundle` config). The gateway can generate and
  inject a CA bundle into every backend via `SSL_CERT_FILE`,
  `REQUESTS_CA_BUNDLE`, and `NODE_EXTRA_CA_CERTS`, solving TLS trust for
  private-CA / intercepting-proxy environments once instead of per-backend.
  The gateway stays trust-store- and OS-agnostic: a user-supplied
  `generate_command` implements a `check` (fast, prints `CURRENT` or requests
  regeneration) and `bundle` (writes `$CA_BUNDLE_PATH`) contract, so the bundle
  is regenerated only when the trust material actually changes. `require_bundle`
  can make a missing bundle a fatal startup error. Existing per-backend `env`
  values are respected. See README and
  `resources/examples/generate-ca.example.sh`.

- **Credential TTL recycling**. A backend can declare when its injected
  credential expires by printing `[[gateway:credential_expiry]] <unix-epoch>`
  to stderr at startup. The gateway then recycles the backend before expiry: a
  soft deadline (`credential_ttl_soft_margin_seconds`, default 300) recycles
  when idle or defers until in-flight requests drain; a hard deadline
  (`credential_ttl_hard_guard_seconds`, default 120) force-recycles even if
  busy. Timers are torn down on any backend exit (no orphan watchers). Backends
  that don't emit the sentinel are unaffected.

### Changed

- Backend stderr is now captured and forwarded (previously passed straight
  through) to support the credential-expiry sentinel; all stderr output is still
  written to the gateway's stderr for debugging.
- Exit codes are now named constants (`exitOK`, `exitFailure`,
  `exitCABundleFailed`).
- Bump Go toolchain to 1.27.0 (resolves a standard-library vulnerability flagged
  by `govulncheck` on the previously pinned 1.26.5).

## [0.1.1]

- CI: group CodeQL actions; dependency bumps.

## [0.1.0]

- Initial release: path-routed MCP gateway with lazy spawn, idle reaping,
  self-termination, auto-restart, bearer auth, tool filtering/discovery,
  description truncation, remote (SSE / streamable-HTTP) backends, and health
  endpoint.

[Unreleased]: https://github.com/jbcjorge/mcp-gateway/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/jbcjorge/mcp-gateway/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/jbcjorge/mcp-gateway/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/jbcjorge/mcp-gateway/releases/tag/v0.1.0
