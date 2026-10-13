# Design: Keyed backends (per-call parameterized spawning)

Status: DRAFT (for review before implementation)
Target: mcp-gateway (Go)

## Motivation

Today a route maps to a fixed set of member backends, each spawned from a static
`command` + `env` decided at config load. Some upstreams are not a single
instance but a *family* of identical servers that differ only by a runtime
coordinate the client supplies per call: for example, one SonarQube MCP server
binary that must be bound to a specific SonarQube instance URL, where the URL is
only known at call time (an agent extracts it from a GitLab pipeline).

Requirements driving this feature:

1. **Fully dynamic.** Adding a new target instance must require NO gateway/config
   change. Operators add only what the upstream itself needs (e.g. a Keychain
   entry). The gateway must not enumerate instances.
2. **Per-instance process reuse and lifecycle.** Two clients targeting the same
   instance share one child process; a client targeting a different instance
   gets its own. An idle instance is reaped independently while an active one
   stays alive.
3. **Generic.** No upstream-specific knowledge in the gateway (no URLs, tokens,
   templating, sanitization). Those belong to the upstream's wrapper. The
   gateway only transports a routing key from the client to the child.

Non-goal: the gateway resolving auth, URLs, or environment specifics. The
upstream's launcher (e.g. a `run.sh`) owns all of that. The gateway passes the
key value(s) down as environment variables and stays agnostic.

## Concept

A **keyed backend** is a `servers` template that declares one or more `params`.
When a server template has a non-empty `params` list:

- On `tools/list`, the gateway advertises the backend's tools with each `param`
  injected into every tool's `inputSchema`.
- On `tools/call`, the gateway reads the param values from `arguments`,
  validates (optional), computes a stable **instance id** = hash of the values,
  selects or lazily spawns the child bound to that id, strips the params from
  `arguments`, and forwards the rest to that child.
- Each child is a normal `*Backend` registered in `gw.backends` under
  `<route>/<id>`, so the existing idle reaper, credential-TTL watcher, health,
  shutdown, and `/_restart` all apply per child with no new lifecycle code.

The param values are passed to the child as environment variables. Everything
upstream-specific (derive URL, read token, set N extra vars, sanitize) is done
by the child's launcher using those env vars.

## Configuration

New optional field on `BackendDef`:

```go
// Param is one routing parameter for a keyed backend. The gateway injects it
// into every tool's input schema, reads+strips it on tools/call, includes it in
// the child's identity hash, and passes its value to the child as an env var.
type Param struct {
    Name        string `json:"name"`        // injected tool-arg name, e.g. "sonarUrl" (required)
    Description string `json:"description"` // help text shown to the agent
    Validate    string `json:"validate"`    // optional regex; empty = accept any value
    EnvName     string `json:"envName"`     // optional child env var name; empty = use Name
    Required    *bool  `json:"required"`    // optional; nil = true
}

// On BackendDef:
Params []Param `json:"params"` // empty/absent = ordinary backend (unchanged)
```

Example `backends.json` (the complete, permanent config for a Sonar family):

```jsonc
{
  "servers": {
    "sonarqube": {
      "command": ["bash", "/Users/<user>/.kiro/tools/sonarqube-mcp/run.sh"],
      "idle_timeout_seconds": 300,
      "params": [
        {
          "name": "sonarUrl",
          "description": "URL of the target SonarQube instance (from the GitLab pipeline).",
          "validate": "^https://[A-Za-z0-9.-]+(/.*)?$"
        }
      ]
    }
  },
  "backends": ["sonarqube"]
}
```

Adding a new instance later = add the auth the upstream needs, e.g.:

```bash
security add-generic-password -s kiro-mcp -a sonarqube-token_billing-sonarqube -w
```

No gateway or config change.

Multi-param example (identity derives from all values together):

```jsonc
"params": [
  { "name": "sonarUrl",     "description": "SonarQube base URL" },
  { "name": "organization", "description": "Org key", "required": false, "envName": "SONARQUBE_ORG" }
]
```

The launcher (`run.sh`) owns all specifics; the gateway passes `sonarUrl` and
`organization` as env vars and nothing else:

```bash
#!/usr/bin/env bash
set -euo pipefail
URL="${sonarUrl:?missing sonarUrl}"
id="$(printf '%s' "$URL" | sed -E 's#https://([^./]+)\..*#\1#')"  # launcher sanitizes
export SONARQUBE_URL="$URL"
export SONARQUBE_TRANSPORT="stdio"
export SONARQUBE_TOKEN="$(security find-generic-password -s kiro-mcp -a "sonarqube-token_${id}" -w)"
exec java -jar "$HOME/.kiro/tools/sonarqube-mcp/sonarqube-mcp-server-<VERSION>.jar"
```

## Instance identity

The child id is `sha256(v1 \0 v2 \0 ... )` over the param values in declared
order, truncated to 12 hex chars. Rationale:

- Stable, short, filesystem/log-safe (used as `gw.backends` key `<route>/<id>`
  and cache file name).
- SHA-256 (not MD5) to avoid tripping security scanners (gosec / the repo's own
  Sonar pipeline).
- Identity is over the EXACT values. The gateway does not canonicalize; e.g.
  `https://x.example` and `https://x.example/` hash differently and spawn two
  children. Per the "upstream sanitizes" principle, canonicalization (if wanted)
  is the launcher's or caller's job. Documented, not enforced.

The human-readable param values are logged alongside the id for debuggability
(subject to not logging secrets; param values here are URLs/keys, not tokens).

## Behavior

### Routing

A keyed route cannot use the single-member fast path in `dispatchRoute`
(there is no `members[0]` to bind). Add a branch: if the resolved route is
keyed, dispatch through new keyed handlers. Normal and composite routes are
unchanged (`Params` empty => existing behavior exactly).

### tools/list (reuses the existing cache/discover flow)

Transport-agnostic: identical whether the child is a JAR, a binary, or a remote
server. It reuses the existing cache mechanism (`toolsCache` on disk at
`cacheDir/<route>.json`), with one added step: inject `params` into each tool's
`inputSchema`.

1. If a cached tool list exists for the route: return it with each `param`
   injected into every tool's `inputSchema.properties` (and added to
   `required` when the param is required). No child spawned.
2. If no cache yet (cold): return an empty tools list immediately. Discovery
   happens on the first keyed `tools/call` (the first moment a child can exist),
   which populates the shared cache. The next `tools/list` is complete.

This matches the gateway's existing "empty until discovered, then cached, served
to everyone" model. The only keyed-specific behavior is param injection on read.
The cache is shared across all instance ids of the route (the tool surface is
identical per instance; it is the same upstream program).

Rationale for empty-until-first-call: advertising a non-empty list before any
key exists would require spawning a child speculatively, which needs some key,
which reintroduces a canonical/bootstrap value and breaks requirement 1 (fully
dynamic, no instance knowledge in the gateway). Empty-until-discovered keeps the
gateway instance-agnostic at the cost of a one-call warm-up.

### tools/call

1. Parse `params.arguments`. For each declared `Param`:
   - If required and absent => JSON-RPC error (clear message naming the param).
   - If `Validate` set and value fails the regex => JSON-RPC error.
2. Compute instance id from the values.
3. `ensureChild(route, id, paramValues)`: under `gw.mu`, look up
   `gw.backends["<route>/<id>"]`; if absent, build a child `*Backend` from the
   server template `BackendDef` plus the param values injected as env
   (`EnvName` or `Name`), register it, and return it. A per-id spawn guard
   prevents a double-spawn race when two calls arrive concurrently for a new id.
4. Strip the declared params from `arguments` (so the child does not receive
   unknown args).
5. Forward to the child via the existing `forwardToBackend` path (lazy spawn,
   send, restart-on-failure, tools/list caching side-effect, smart activation).

### Lifecycle (no new reaper logic)

Each child is a `*Backend` in `gw.backends`, so:

- **Idle reaping**: `reapIdleBackends` already times each backend by its own
  `lastUsed` and honors the per-backend `idleTimeout` (inherited from the server
  template's `idle_timeout_seconds`). An idle instance is reaped while an active
  instance stays up. This is exactly requirement 2, with zero new reaping code.
- **Credential TTL**: the existing stderr sentinel
  `[[gateway:credential_expiry]] <epoch>` works per child; a child can self
  recycle before its token expires and re-spawn with a fresh Keychain read.
- **Health / `/_restart` / shutdown**: iterate `gw.backends`, so children appear
  and are controllable individually. `/_restart/<route>/<id>` works by name.

### Child de-registration

On child exit (`waitForExit`), a reaped/crashed keyed child must be removed from
`gw.backends` and from the route's child pool so a later call re-spawns a fresh
one. Normal (static) backends are never removed today; keyed children must be,
to avoid leaking map entries over time. This is the second new lifecycle
interaction (the first being lazy registration).

## Internal shape

- `Param` struct + `Params []Param` on `BackendDef` (config.go / main.go types).
- `Route` gains optional keyed state:
  ```go
  keyed      []Param              // nil/empty for normal routes
  children   map[string]*Backend  // instance id -> child, guarded by rt.mu
  spawning   map[string]chan struct{} // id -> in-flight spawn guard
  ```
- `newGateway`: when a resolved route's server def has non-empty `Params`, mark
  the route keyed; do NOT eagerly build a member.
- `dispatchRoute`: branch to keyed handlers when `route.isKeyed()`.
- New: `keyedToolsList` (cache + param injection), `keyedToolsCall`
  (parse/validate/hash/ensureChild/strip/forward), `ensureChild` (lazy build +
  register under `gw.mu` + spawn guard), param-injection helper, id-hash helper.
- `waitForExit` (or the reaper path): unregister keyed children on exit.

Normal and composite routes keep their exact current code paths; keyed is an
additive branch gated on `Params` being non-empty.

## Security

- `Validate` is optional but recommended; when present the gateway rejects
  non-matching key values before spawning. When absent, the value passes through
  and the launcher is responsible for sanitizing (explicit per the "upstream
  sanitizes" principle).
- The gateway never interpolates key values into a shell; values are passed only
  as environment variables to an argv built from the static `command`. No shell
  expansion of key values by the gateway.
- Param values may be logged (URLs/keys). Launchers must not echo secrets; the
  gateway logs only the injected params, never child env it did not set.

## Open questions (confirm before implementation)

1. Cold `tools/list`: empty-until-first-call (proposed) vs a cached on-disk
   schema snapshot shipped out-of-band. Proposed keeps the gateway fully
   instance-agnostic.
2. Hash: SHA-256 truncated to 12 hex chars (proposed). Confirm length.
3. Child de-registration timing: on `waitForExit` vs on reaper kill. Proposed:
   on `waitForExit` so every exit reason converges to one cleanup path (matches
   the existing single-teardown-path design for TTL timers).

## Testing plan (TDD, no company/OS deps)

- Config: parse `params`; a server with non-empty `params` becomes a keyed
  route; empty/absent => normal route (unchanged).
- Identity: same values => same id; different values => different id; order
  sensitivity; multi-param concatenation.
- tools/list: cold => empty; after a call populates cache => tools returned with
  each param injected into inputSchema and required-list; cache shared across
  ids.
- tools/call: missing required param => error; validate regex failure => error;
  valid => child spawned, params stripped from forwarded arguments, response
  returned; second call same id => same child reused (assert one process);
  different id => second child.
- Lifecycle: two children idle-reap independently (id A reaped while id B stays
  on fresh lastUsed); child removed from gw.backends on exit; /health lists
  children; /_restart/<route>/<id> restarts one child.
- Concurrency: two simultaneous calls for a new id spawn exactly one child
  (spawn-guard).
- Regression: all existing normal/composite route tests remain green.
