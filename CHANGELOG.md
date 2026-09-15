# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.4.0]

### Added

- **Graceful process-group teardown on backend stop.** Each stdio backend is
  placed in its own process group, so stopping it signals the whole tree at
  once: SIGTERM to the group, wait a grace window, then escalate to group
  SIGKILL. This guarantees processes a backend spawns (e.g. a browser and its
  renderer children) are reaped with the backend instead of being orphaned when
  the backend is wedged. Recycling, idle reaping, and manual restart all route
  through the graceful path. Windows uses a best-effort single-process fallback
  since it lacks POSIX process groups.
- **`stop_grace_seconds` config.** Sets the SIGTERM-to-SIGKILL grace, at the
  global level and per server (`servers.<name>.stop_grace_seconds`), resolving
  per-server first, then global, then a 20s default. A configured `0` falls
  through to the next level rather than meaning an immediate SIGKILL. Useful for
  servers that own heavy child processes needing longer to tear down. See README.

### Changed

- Internal: added an `update-deps` Make target (update direct dependencies to
  latest, tidy, then verify with build and tests) and bumped the `go` directive
  to 1.27.1.

## [0.3.0]

### Added

- **Composite backends (compositions).** A route can now combine multiple
  servers under one namespace. Tool lists (and resources/prompts) merge across
  members with **last-one-wins** on name collision (intentional override); tool
  calls route to the owning member. Each member's `include_tools`/`exclude_tools`
  filters are applied before merging. See `docs/design/compositions.md`.
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
- Internal: `main.go`/`main_test.go` split into cohesive per-concern files
  (backend, handlers, discovery, remote, gateway, config, logging).

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

[Unreleased]: https://github.com/jbcjorge/mcp-gateway/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/jbcjorge/mcp-gateway/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/jbcjorge/mcp-gateway/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/jbcjorge/mcp-gateway/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/jbcjorge/mcp-gateway/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/jbcjorge/mcp-gateway/releases/tag/v0.1.0
