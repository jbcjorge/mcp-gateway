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
- **Bearer auth** - global and per-backend token authentication
- **Tool filtering** - include/exclude glob patterns per backend
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
  "backends_file": "backends.json"
}
```

### backends.json

```json
{
  "github": {
    "command": ["npx", "-y", "@modelcontextprotocol/server-github"],
    "env": {"GITHUB_TOKEN": "ghp_..."},
    "exclude_tools": ["create_or_update_file", "delete_*"]
  },
  "remote-api": {
    "url": "https://mcp.example.com/mcp",
    "transport_type": "streamable-http",
    "headers": {"Authorization": "Bearer remote-token"}
  },
  "disabled-backend": {
    "command": ["echo"],
    "disabled": true
  }
}
```

### Backend types

| Type | Detection | Description |
|------|-----------|-------------|
| stdio | `command` field present | Spawns a subprocess, communicates via stdin/stdout |
| SSE | `url` field, no `transport_type` or `"sse"` | Connects to a remote SSE MCP server |
| Streamable HTTP | `url` field + `"transport_type": "streamable-http"` | Connects to a remote HTTP MCP server |

### Backend options

| Field | Type | Description |
|-------|------|-------------|
| `command` | `[]string` | Command and args to spawn (stdio) |
| `url` | `string` | Remote server URL (SSE or HTTP) |
| `transport_type` | `string` | `"sse"` or `"streamable-http"` (auto-detected if omitted) |
| `env` | `map` | Environment variables for subprocess |
| `headers` | `map` | HTTP headers for remote connections |
| `auth_tokens` | `[]string` | Bearer tokens (overrides global) |
| `include_tools` | `[]string` | Glob patterns for tools to expose |
| `exclude_tools` | `[]string` | Glob patterns for tools to block |
| `discovery` | `bool` | Force enable/disable tool discovery |
| `categories` | `map` | Manual tool category assignments |
| `log_enabled` | `bool` | Per-backend logging toggle |
| `disabled` | `bool` | Skip this backend entirely |

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
