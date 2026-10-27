# Config hot-reload (backends.json)

## Problem

Adding, removing, or changing a backend requires editing `backends.json` and
then restarting the gateway, because the topology is read only once in
`newGateway`. MCP clients (e.g. kiro-cli) hot-reload their own `mcp.json` and
try to connect to a newly added route immediately; if the gateway has not been
restarted yet, that route returns `404 unknown backend`, which the client may
misinterpret (e.g. fall back to an OAuth flow). The restart is an operational
papercut and the cause of connect-before-route-exists races.

## Goal

Watch the backends topology file and reconcile routes live, without a restart
and without disturbing unaffected backends, mirroring how MCP clients reconcile
their own config.

- New route in config → built and registered (lazy-spawn as usual).
- Route removed from config → its backends stopped and deregistered.
- Route changed (command/args/env/composition) → stopped and rebuilt.
- Unchanged route → left running untouched.

## Non-goals

- Reloading gateway-level settings (listen address, log file, CA bundle,
  timeouts). Those still require a restart. Only the backends topology
  (`servers`, `compositions`, `backends`) is hot-reloaded.
- A filesystem-notification dependency. We poll the file mtime on an interval,
  consistent with the project's no-blind-dependency stance and the existing
  ticker-based reaper.

## Design

### Watching

A background goroutine (`gw.configWatcher(ctx, path, interval)`), started like
the idle reaper, stats the backends file every `interval` (default 5s, floored).
On an mtime change it debounces briefly (one extra interval) to avoid reading a
half-written file, re-parses, and reconciles. Parse/read errors are logged and
skipped (keep running on the last-good topology); a later successful read
recovers.

### Reconciliation

`reconcileRoutes(newResolved []ResolvedRoute)` diffs the new resolved route set
against the current `gw.routes` by a stable signature per route:

- signature = hash of the resolved route (members' server names + each member's
  BackendDef command/args/env/params + composition shape). Only a value change
  flips the signature; key-order changes do not.

Plan (computed under `gw.mu`):

- present in new, absent in old        → ADD
- absent in new, present in old        → REMOVE
- present in both, signature differs   → REPLACE (REMOVE + ADD)
- present in both, signature equal     → KEEP

Apply:

- ADD: `buildRoute` (reuses existing path; lazy-spawn, no eager start).
- REMOVE: stop each member backend (`gracefulStopLocked`), drop keyed children,
  delete from `gw.routes` and `gw.backends`, rebuild the authorizer token map.
- REPLACE: REMOVE then ADD.

The authorizer is rebuilt from the new route-token map after each apply.

### Concurrency

- The whole plan-and-apply runs under `gw.mu` (write), so in-flight dispatch
  either sees the old or new route map, never a partial one. A request that
  resolved its route before a REMOVE proceeds against the (now stopping)
  backend and fails normally; this matches existing reap behaviour.
- Reconcile runs only when the file changes, so steady-state cost is one stat
  per interval.

### Safety

- A malformed/removed file is never applied (keep last-good).
- Removing the last member of a route stops its process group (same path as the
  reaper), so no orphaned subprocess (and, for sso-helper backends, no orphaned
  browser).

## Testing

- Pure: route signature stability (key-order independent), plan computation
  (add/remove/replace/keep) as table-driven tests.
- Integration: write a temp backends file, start a gateway pointed at it, mutate
  the file, assert routes appear/disappear and unchanged ones keep the same
  backend instance (pid) — i.e. not restarted.
