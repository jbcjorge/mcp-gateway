# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.7.0] - 2026-11-10

OAuth broker with per-backend auth hooks, hot-reload of backends.json, and a faster client handshake that no longer spawns every backend on connect

- ci: bump docker/login-action from 4.6.0 to 4.8.0 ([7a37715b](https://github.com/jbcjorge/mcp-gateway/commit/7a37715ba2f01a15b145a753def616b94b1684b0))
- ci: bump SonarSource/sonarqube-scan-action from 8.2.2 to 8.4.1 ([c970eff6](https://github.com/jbcjorge/mcp-gateway/commit/c970eff6957526aea2b635087f5317b036d1ad17))
- ci: bump the codeql group across 1 directory with 2 updates ([c235c7a7](https://github.com/jbcjorge/mcp-gateway/commit/c235c7a7d3034b13d19550a7fdcf66ad47188876))
- chore(test): race detector, 80% coverage gate, oauth security tests ([92532ff9](https://github.com/jbcjorge/mcp-gateway/commit/92532ff9c3d44e32e95455f3008ea25d832660b5))
- fix(handshake): answer server/discover locally, cache-first composite tools/list ([1cc85e95](https://github.com/jbcjorge/mcp-gateway/commit/1cc85e958b045bcfb59b319f3ddab44a7e2cecc2))
- feat(auth): OAuth broker with per-backend auth hooks ([b0c4d01d](https://github.com/jbcjorge/mcp-gateway/commit/b0c4d01d4b860cd05fecf9b38973ede4f20ee682))
- fix(timeout): extend HTTP write deadline for long tools/call ([544429e4](https://github.com/jbcjorge/mcp-gateway/commit/544429e43c271981e28e22c17b6c7c0360dde3e8))
- feat(reload): hot-reload backends.json without a gateway restart ([cdb34a00](https://github.com/jbcjorge/mcp-gateway/commit/cdb34a008c1d71f439eeb37fcea80f950c05ddd3))
- fix(release): correct web URL derivation (no https:// double-prefix) ([ee32d519](https://github.com/jbcjorge/mcp-gateway/commit/ee32d5192433dbd0d7c1d57ea025f869529a305c))
- ci: bump actions/upload-artifact from 7.0.1 to 7.0.2 ([adb51c7e](https://github.com/jbcjorge/mcp-gateway/commit/adb51c7ed26c3b68df683cc03ee948ed9d65a4fe))
- ci: bump docker/build-push-action from 7.3.0 to 7.4.0 ([7705391d](https://github.com/jbcjorge/mcp-gateway/commit/7705391da155164f17285fdb8d51c6d65ba6538b))

## [0.6.0] - 2026-10-27

Gateway resilience: survive long tool calls and fast connect handshakes

- feat(handshake): populate resources/prompts caches from live backends ([5d77fa32](https://github.com/jbcjorge/mcp-gateway/commit/5d77fa32b979428be10d22466991cc80e03ec4ee))
- fix(handshake): answer resources/prompts list locally without spawning ([0d00bb70](https://github.com/jbcjorge/mcp-gateway/commit/0d00bb7025649c3f9fff4a5a78fe25cf4e33dc97))
- fix(send): honor declared tools/call timeout for backend response wait ([7108c7a0](https://github.com/jbcjorge/mcp-gateway/commit/7108c7a077c3c49e08cf48f2af91c1c910f1f1fc))
- fix(reaper): keep backends alive during in-flight and timed tool calls ([a418a0e1](https://github.com/jbcjorge/mcp-gateway/commit/a418a0e18c2b7f1fbbb95e928b184e60a84a8bb3))
- chore(deps): bump go toolchain to 1.27.3 (GO-2026-6809) ([425ecaaa](https://github.com/jbcjorge/mcp-gateway/commit/425ecaaad5e49074cb1b50e7ee2b04328ef91390))

## [0.5.0] - 2026-10-13

### Added

- **Keyed backends.** A server with a non-empty `params` list becomes one route
  that spawns and pools a distinct child process per runtime key supplied as a
  per-call tool argument. The gateway injects each param into every tool's input
  schema on `tools/list`, and on `tools/call` validates the params, derives a
  stable instance id (`sha256` of the values, 12 hex chars), selects or lazily
  spawns the child for that id, strips the params, and forwards the rest. Each
  child registers as `<route>/<id>` and is lifecycle-managed independently (idle
  reap, credential TTL, restart, health), de-registering on exit. The tool
  surface is shared across instances; cold `tools/list` returns empty until the
  first call warms the cache. All instance specifics (URL, token, env,
  sanitization) are the child's concern; the gateway only transports the key, so
  adding a new instance needs no config change. Params:
  `{ name, description?, validate?, envName?, required? }`. See README
  "Keyed backends" and `docs/design/keyed-backends.md`.

### Changed

- **`make update-deps` now pins the Go version everywhere.** After bumping Go to
  latest it mirrors the exact version from the `go.mod` `go` directive into
  `.tool-versions` and asserts they match, so local tooling (mise/asdf) and CI
  (which reads `go-version-file: go.mod`) stay on the same version and never
  float ahead, which previously let a local `make check` pass while CI failed.
- Bumped the Go directive to 1.27.2 to pick up net/http standard-library
  security fixes (GO-2026-6617, GO-2026-6613) flagged by `govulncheck` in CI.
- CI action bumps: `docker/setup-qemu-action` 4.3.0 -> 4.4.0,
  `docker/setup-buildx-action` 4.3.0 -> 4.4.1,
  `SonarSource/sonarqube-scan-action` 8.2.1 -> 8.2.2,
  `codecov/codecov-action` 7.0.0 -> 7.1.1, and the CodeQL action group.

## [0.4.0] - 2026-09-15

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

## [0.3.0] - 2026-09-07

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

## [0.2.0] - 2026-09-04

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

## [0.1.1] - 2026-07-22

- CI: group CodeQL actions; dependency bumps.

## [0.1.0] - 2026-07-17

- Initial release: path-routed MCP gateway with lazy spawn, idle reaping,
  self-termination, auto-restart, bearer auth, tool filtering/discovery,
  description truncation, remote (SSE / streamable-HTTP) backends, and health
  endpoint.

[Unreleased]: https://github.com/jbcjorge/mcp-gateway/compare/v0.7.0...HEAD
[0.7.0]: https://github.com/jbcjorge/mcp-gateway/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/jbcjorge/mcp-gateway/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/jbcjorge/mcp-gateway/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/jbcjorge/mcp-gateway/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/jbcjorge/mcp-gateway/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/jbcjorge/mcp-gateway/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/jbcjorge/mcp-gateway/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/jbcjorge/mcp-gateway/releases/tag/v0.1.0
