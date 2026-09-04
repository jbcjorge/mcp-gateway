# Design: Composite backends (compositions) + route prefixing

Status: DRAFT (for review before implementation)
Target: mcp-gateway (Go)

## Motivation

Today each route maps to exactly one backend, and the gateway's differentiator
is **path isolation**. That stays the default. Two related needs motivate this
feature:

1. **Compose a route from multiple servers** — fill a gap in a third-party
   server, override a broken upstream tool, augment with in-house tools, or run
   new+legacy side by side during a migration.
2. **Avoid client-side tool-name collisions** — the gateway can serve multiple
   routes to one client; if two routes expose a tool with the same name (e.g.
   both have `search`), the client/LLM sees an ambiguous merged tool list. Path
   isolation fixes routing at the gateway but NOT the client's flattened tool
   namespace. An optional per-route prefix resolves this.

Both are opt-in and explicit — "isolated by default, composable when you ask".

## Concepts

- **server**: a reusable backend *template* (command/url + env + include/exclude
  filters). Definitions only; inert until referenced.
- **composition**: an ordered list of members, each instantiating a server.
  Definitions only; inert until referenced.
- **route**: an entry in `backends`, exposed to clients at `/<name>/mcp`. **Only
  `backends` entries are routes.** A route names either a server or a
  composition, and may carry an optional `prefix`.

Servers and compositions are just definitions. A composition is NOT a route
until a `backends` entry references it. This enables **lazy resolution**: the
gateway resolves starting from `backends`, transitively resolving only the
compositions/servers actually referenced. Unreferenced definitions (e.g. a
100-server composition nobody routes to) cost nothing — never resolved, never
instantiated.

A server template may be referenced by any number of compositions and/or routes;
**each use gets its own independent `*Backend` instance** (Model Y): no reuse
restriction, independent lifecycle, shared no state. Tradeoff: reuse => multiple
processes (mitigated by lazy-spawn + idle-reap).

## Version / compatibility

Pre-1.0: **no backward compatibility with the old flat `{name: def}` config.**
The new binary requires the new shape (`servers` / `compositions` / `backends`).
Rollout migrates existing config files to the new shape.

## Configuration

```json
{
  "servers": {
    "confluence-official": { "command": ["bash", "/path/run.sh"] },
    "confluence-legacy":   { "command": ["python3", "/path/server.py"] },
    "jira":   { "command": ["bash", "/path/jira.sh"] },
    "gitlab": { "command": ["python3", "/path/gitlab.py"] },
    "my-own-server": { "command": ["python3", "/path/own.py"] }
  },

  "compositions": {
    "wiki": {
      "members": [
        { "server": "confluence-official" },
        { "server": "confluence-legacy", "include_tools": ["confluence_remove_label"] }
      ]
    }
  },

  "backends": [
    "wiki",
    "my-own-server",
    { "route": "jira",   "prefix": "jira_" },
    { "route": "gitlab", "prefix": "gitlab_" }
  ]
}
```

Rules:

- `backends[]` is a list of route entries. Each entry is either:
  - a bare **string** (route name, no prefix), or
  - an **object** `{ "route": "<name>", "prefix": "<string>" }`.
- `route` must name either a `compositions` key or a `servers` key. Dangling
  reference => fatal config error.
- `members[]` reference a `servers` key via `server` and may add their own
  `include_tools` / `exclude_tools` (applied on top of the server's own filters).
- **Resolution is lazy and starts from `backends`.** Only referenced
  compositions/servers are resolved and instantiated; unreferenced definitions
  are ignored (no cost).

## Behavior

A route presents as a single `/<name>/mcp` endpoint. A composition fans out to
its member instances; each member is a normal `*Backend` with independent
lifecycle. The composition is orchestration only.

### List/get method families (tools, resources, prompts, ...)

MCP has parallel families with identical shape: a **list** method (enumerate)
and a **get/call** method (act on one item by name/uri):

- tools: `tools/list` + `tools/call`
- resources: `resources/list` + `resources/read`
- prompts: `prompts/list` + `prompts/get`

The gateway applies ONE generic strategy to all of them:

1. **list**: always merge every member's list (in member order), building a
   `name/uri -> (member, realName)` ownership map. On collision, **last member
   wins** (enables intentional override). Log shadowed entries (warn).
2. **route prefix** (if set): rename entries to `<prefix><name>` for
   advertisement; record display↔real mapping.
3. **get/call**: look up the (possibly prefixed) name/uri in the ownership map,
   strip the prefix, forward to the owning member. Unknown => JSON-RPC error.

Order: override (merge/last-wins) happens BEFORE prefixing.

### Caching

Like single backends cache `tools/list` today (`toolsCache`, persisted to
`cacheDir/<name>.json`), a **composition caches its MERGED (and prefixed)
lists** keyed by the route name. The cached-`initialize` fast path serves this
merged cache without spawning members. The merge is (re)built when members'
individual lists refresh (on spawn/reconnect).

### Other methods

Any non-list/get, non-lifecycle method on a composition routes to **member[0]**
(the primary). Plain routes forward as today.

### Deferred to a later version

Proper handling of member restart / tool-set change: re-initializing a member in
the background and emitting `notifications/tools/list_changed` with the updated
merged list. Not in this version.

### Lifecycle / health / restart / discovery

- Each member instance: independent lazy-spawn, idle-reap, credential TTL.
- `/health`: report the route and, for compositions, each member's state.
- `/_restart/<route>`: restart all member instances.
- Discovery / description truncation: applied per member, then merged, then
  prefixed.

## Internal shape

Introduce a `Route` abstraction so `handleRequest` stays uniform:

```go
type Route interface {
    name() string
    prefix() string
    toolsList(ctx) (json.RawMessage, error)      // merged + prefixed
    call(ctx, envelope, body) ([]byte, error)    // strip prefix + ownership dispatch
    members() []*Backend                          // health/restart/reap
}
```

- A plain server route = `Route` with one member, optional prefix.
- A composition = `Route` with N members, last-wins map, optional prefix.

`handleRequest` (main.go ~662) resolves a `Route` by path instead of a
`*Backend`; `handleLocally`/`forwardToBackend` operate via the `Route`. Members
remain plain `*Backend`s so all existing lifecycle/TTL/reaper code is unchanged.

## Collision scopes (for clarity)

1. **Within a composition**: two members, same tool → last-wins (intentional).
2. **Across routes in one client**: two routes expose same tool name → resolved
   by giving one (or both) a route `prefix`. Optional; user's choice. If the
   user declines and a collision results, that is the user's configuration
   choice, not a gateway fault (behavior stays deterministic per route).
3. Across gateways/clients: out of scope.

## Non-goals

- No implicit/automatic merging or prefixing (both explicit).
- No forced prefixing (would break existing tool names and the override case,
  and double-prefix already-namespaced tools like `confluence_*`).
- No cross-composition shared instances (each use is its own instance).

## Auth

Client→gateway auth (`auth_tokens`, the bearer token a client presents) is keyed
by the **route** (the `backends` entry / composition name) — the route is the
only thing exposed to a client. Members have no independent client-facing auth.
Backend→upstream auth (sso-helper cookie, PAT, OAuth, etc.) is each server's own
concern, unchanged by compositions.

## Resolved decisions

1. No backward compatibility (pre-1.0); new config shape is mandatory.
2. All list/get families (tools, resources, prompts, ...) always merge, last-one
   -wins on collision; get/call routes by the merged ownership map.
3. Composition is not a route; only `backends` entries are. Lazy resolution from
   `backends` — unreferenced definitions are never resolved/instantiated.
4. Compositions cache their merged (+prefixed) lists, like single-backend
   `toolsCache`.
5. Route prefix optional (`prefix: "<string>"`), default off.
6. Non-list/get methods on a composition → member[0].
7. Client→gateway auth keyed by route; backend→upstream auth per server.

## Deferred to a later version

- Member restart / tool-set change: background re-initialize + emit
  `notifications/tools/list_changed` with the updated merged list.

## Testing plan (TDD, no company/OS deps)

- Config parse: new shape; object vs string `backends` entries; legacy flat map
  still works; validation (dangling `route`/`server` refs).
- tools/list: composition union + last-wins + shadow warning; per-member
  include/exclude; route prefix applied to final set.
- tools/call: prefix stripped, routed to owning member; unknown tool errors;
  override member wins.
- Non-tool method → member[0].
- Lifecycle: members idle-reap/TTL independently; health lists members; restart
  hits all members; same server template reused in two routes → two instances.
- Backward compat: existing flat config behaves exactly as before.
