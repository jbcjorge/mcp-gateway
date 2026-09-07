# mcp-gateway

A zero-ops MCP server manager. Spawns, routes, and recycles MCP backends on demand - no daemons to manage, no processes to babysit.

## Features

- **Path routing** - each backend at its own `/<name>/mcp` endpoint, fully isolated
- **Stdio backends** - spawns subprocesses, manages their lifecycle automatically
- **Remote backends** - connects to SSE and Streamable HTTP MCP servers
- **Lazy spawn** - backends start on first request, not at gateway startup
- **Idle reaping** - unused backends killed after configurable inactivity
- **Self-termination** - gateway exits after no requests (launchd/systemd respawns on demand)
- **Auto-restart** - crashed backends respawn on next request
- **Credential TTL recycling** - backends declaring a credential expiry are recycled before it lapses, gracefully (drains in-flight requests) with a hard-deadline fallback
- **CA bundle injection** - optionally generate and inject a CA bundle into every backend (`SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`, `NODE_EXTRA_CA_CERTS`), regenerated only when the trust store actually changes
- **Bearer auth** - global and per-backend token authentication
- **Tool filtering** - include/exclude glob patterns per backend
- **Composite backends** - combine multiple servers under one route with last-wins tool override, plus optional per-route tool prefixing to avoid client-side name collisions
- **Tool discovery** - automatic category-based tool activation for large backends
- **Description truncation** - configurable max length for verbose tool descriptions
- **Health endpoint** - per-backend status, PID, idle time

## Quick Start

### Install from source

```bash
git clone https://github.com/jbcjorge/mcp-gateway.git
cd mcp-gateway
make build
./mcp-gateway config.json
```

### Install as macOS service (launchd)

```bash
make install
launchctl load ~/Library/LaunchAgents/io.github.jbcjorge.mcp-gateway.plist
```

### Docker

```bash
docker run -d -p 19900:19900 \
  -v /path/to/config.json:/config/config.json \
  -v /path/to/backends.json:/config/backends.json \
  ghcr.io/jbcjorge/mcp-gateway:latest
```

### Go install

```bash
go install github.com/jbcjorge/mcp-gateway@latest
```

## Configuration

### config.json

```json
{
  "listen": "127.0.0.1:19900",
  "idle_timeout_seconds": 300,
  "self_idle_timeout_seconds": 3600,
  "auth_tokens": ["your-secret-token"],
  "backends_file": "backends.json",

  "credential_ttl_soft_margin_seconds": 300,
  "credential_ttl_hard_guard_seconds": 120,

  "ca_bundle": {
    "path": "/absolute/path/to/ca-bundle.pem",
    "generate_command": ["/absolute/path/to/generate-ca.sh"],
    "require_bundle": false
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `listen` | `string` | Address to listen on |
| `idle_timeout_seconds` | `int` | Kill backends idle for this long (0 = never) |
| `self_idle_timeout_seconds` | `int` | Exit the gateway after no requests for this long (0 = never) |
| `auth_tokens` | `[]string` | Global bearer tokens |
| `backends_file` | `string` | Path to backends.json (relative to config dir) |
| `env` | `map` | Global environment variables applied to every backend |
| `credential_ttl_soft_margin_seconds` | `int` | See [Credential TTL recycling](#credential-ttl-recycling) (default 300) |
| `credential_ttl_hard_guard_seconds` | `int` | See [Credential TTL recycling](#credential-ttl-recycling) (default 120) |
| `ca_bundle` | `object` | See [CA bundle injection](#ca-bundle-injection) |

> Paths in `config.json` are used verbatim (no `~` expansion). Use absolute paths.

### backends.json

The backend topology has three sections:

- **`servers`** — reusable backend *definitions* (templates). Inert until routed.
- **`compositions`** — named, ordered lists of members that combine servers.
- **`backends`** — the *routes* actually exposed at `/<name>/mcp`. Each entry
  names a server or a composition, and may carry an optional `prefix`.

Only entries listed in `backends` become routes. Servers/compositions not
referenced by any route are never started (lazy resolution). A server may be
reused by multiple routes/compositions; each use is an independent instance.

```json
{
  "servers": {
    "github": {
      "command": ["npx", "-y", "@modelcontextprotocol/server-github"],
      "env": {"GITHUB_TOKEN": "ghp_..."},
      "exclude_tools": ["create_or_update_file", "delete_*"]
    },
    "docs-official": { "command": ["my-docs-server"] },
    "docs-extra":    { "command": ["my-inhouse-docs-server"] },
    "remote-api": {
      "url": "https://mcp.example.com/mcp",
      "transport_type": "streamable-http",
      "headers": {"Authorization": "Bearer remote-token"}
    }
  },

  "compositions": {
    "docs": {
      "members": [
        {"server": "docs-official"},
        {"server": "docs-extra", "include_tools": ["extra_tool"]}
      ]
    }
  },

  "backends": [
    "docs",
    {"route": "github", "prefix": "gh_"},
    "remote-api"
  ]
}
```

### Routes (`backends`)

Each entry is either a bare string (route name, no prefix) or an object
`{ "route": "<name>", "prefix": "<string>" }`. The route name must reference a
server or a composition.

- **`prefix`** (optional): advertise this route's tool names as `<prefix><name>`
  and translate back on call. Use it to avoid tool-name collisions when a client
  consumes multiple routes at once. Default: no prefix (names unchanged).

### Compositions

A composition merges the tool lists of its ordered members under one route.

- Members are merged in order; on a **tool-name collision the last member wins**
  (intentional override / supplement). tools/call is routed to the owning member.
- Each member may add its own `include_tools` / `exclude_tools` on top of the
  server's own filters.
- Resources and prompts merge with the same last-wins rule.

### Backend types (servers)

| Type | Detection | Description |
|------|-----------|-------------|
| stdio | `command` field present | Spawns a subprocess, communicates via stdin/stdout |
| SSE | `url` field, no `transport_type` or `"sse"` | Connects to a remote SSE MCP server |
| Streamable HTTP | `url` field + `"transport_type": "streamable-http"` | Connects to a remote HTTP MCP server |

### Server options

| Field | Type | Description |
|-------|------|-------------|
| `command` | `[]string` | Command and args to spawn (stdio) |
| `url` | `string` | Remote server URL (SSE or HTTP) |
| `transport_type` | `string` | `"sse"` or `"streamable-http"` (auto-detected if omitted) |
| `env` | `map` | Environment variables for subprocess |
| `headers` | `map` | HTTP headers for remote connections |
| `auth_tokens` | `[]string` | Client→gateway bearer tokens for the route (overrides global) |
| `include_tools` | `[]string` | Glob patterns for tools to expose |
| `exclude_tools` | `[]string` | Glob patterns for tools to block |
| `discovery` | `bool` | Force enable/disable tool discovery |
| `categories` | `map` | Manual tool category assignments |
| `log_enabled` | `bool` | Per-backend logging toggle |

## CA bundle injection

Some environments sit behind a TLS-intercepting proxy or use a private
Certificate Authority whose root is **not** in the default trust store shipped
with language runtimes (Python `certifi`, Node, etc.). Backends that call such
hosts then fail TLS verification.

`ca_bundle` lets the gateway provide a CA bundle to **every** backend, so this
is solved once instead of per-backend. The gateway itself is trust-store- and
OS-agnostic: it never inspects certificates or knows where they come from. It
only runs a command you supply and injects the resulting bundle path into each
backend's environment as:

- `SSL_CERT_FILE` (OpenSSL: Python `urllib`, `curl`, Go with cgo)
- `REQUESTS_CA_BUNDLE` (Python `requests` / `certifi`)
- `NODE_EXTRA_CA_CERTS` (Node.js)

Existing values on a backend (via its `env`) are respected and not overwritten.

### Configuration

```json
"ca_bundle": {
  "path": "/absolute/path/to/ca-bundle.pem",
  "generate_command": ["/absolute/path/to/generate-ca.sh"],
  "require_bundle": false
}
```

| Field | Type | Description |
|-------|------|-------------|
| `path` | `string` | Where the bundle lives / is written (absolute) |
| `generate_command` | `[]string` | Command to check/generate the bundle |
| `require_bundle` | `bool` | If `true`, a missing/unreadable bundle is fatal (gateway exits with a non-zero code); if `false` (default), it logs an error and continues without injection |

If `ca_bundle` is omitted, the feature is disabled and nothing changes.

### Generator contract

On startup the gateway runs your command with a subcommand as the final
argument and `CA_BUNDLE_PATH` set in the environment:

```
<generate_command...> check    # print "CURRENT" (up to date) or anything else (regenerate)
<generate_command...> bundle   # (re)write the PEM bundle to $CA_BUNDLE_PATH
```

- `check` runs on **every** gateway start, so it must be fast (target &lt; 100ms).
  Use a cheap change signal (file mtimes, a store version, an ETag), not a full
  certificate re-read.
- `check` printing exactly `CURRENT` skips regeneration. **Any** other output, a
  non-zero exit, or a timeout is treated as stale and triggers `bundle`
  (fail-safe).
- `bundle` runs only when needed. It owns the whole lifecycle: gather certs,
  write `$CA_BUNDLE_PATH`, and record whatever state its own `check` needs.

This design regenerates **only when the trust material actually changes** — no
time-based expiry, no blunt periodic refresh.

### Example generator

A ready-to-adapt example is provided at
[`resources/examples/generate-ca.example.sh`](resources/examples/generate-ca.example.sh).
It gathers roots from the OS trust store and uses source-file mtimes as the
change signal. Adapt the `SOURCES` (and optional filtered `EXTRA_SOURCE`) to
your environment — a corporate PKI endpoint, a mounted secret, a vault, etc.

### Possible uses

- Trust a **private/internal CA** whose root isn't in `certifi`/system stores.
- Work behind a **TLS-intercepting corporate proxy**.
- Pin backends to a **curated CA set** for a regulated environment.

## Credential TTL recycling

A backend can tell the gateway when its injected credential (an OAuth token, a
session cookie, a signed URL, etc.) expires, so the gateway recycles the backend
**before** the credential lapses. The next request re-spawns it, picking up a
fresh credential — transparently, thanks to lazy spawn.

A backend opts in by printing a sentinel line to **stderr** once at startup:

```
[[gateway:credential_expiry]] <unix-epoch-seconds>
```

The gateway forwards all backend stderr as usual (so this doesn't interfere with
logging) and arms two deadlines from the declared expiry:

- **Soft deadline** (`expiry - credential_ttl_soft_margin_seconds`, default
  300s): recycle now if idle; if requests are in flight, defer and recycle as
  soon as they drain (graceful).
- **Hard deadline** (`expiry - credential_ttl_hard_guard_seconds`, default
  120s): force-recycle even if requests are in flight (the credential is about
  to be useless anyway).

Timers are torn down whenever the backend exits for any reason (idle reap,
crash, manual restart), so there are no orphan watchers. Backends that never
emit the sentinel are unaffected.

## Client Configuration

Any MCP client that supports HTTP transport can use mcp-gateway. Point it at `http://localhost:19900/<backend>/mcp`.

### Example (generic)

```json
{
  "mcpServers": {
    "github": {"url": "http://localhost:19900/github/mcp"},
    "jira": {"url": "http://localhost:19900/jira/mcp"}
  }
}
```

## Endpoints

| Path | Method | Description |
|------|--------|-------------|
| `/<backend>/mcp` | POST | Forward MCP JSON-RPC to backend |
| `/health` | GET | Gateway status, version, per-backend state |
| `/_restart/<backend>` | POST | Kill backend (re-spawns on next request) |

## Authentication

When `auth_tokens` is configured, requests must include an `Authorization: Bearer <token>` header.

- Global tokens apply to all backends by default
- Per-backend `auth_tokens` override the global set for that backend
- `/health` does not require authentication

## Development

```bash
make tools        # install development tools
make check        # run all quality gates (fmt, vet, shadow, lint, vuln, gosec, gitleaks, complexity, test)
make build        # compile binary
make test         # run tests
make test-report  # tests with coverage + JUnit XML
make release      # cross-compile for all platforms
make install      # build + install + launchd service (macOS)
make clean        # remove artifacts
```

See [INSTALL.md](INSTALL.md) for detailed setup instructions and troubleshooting.

## License

Apache 2.0. See [LICENSE](LICENSE).
