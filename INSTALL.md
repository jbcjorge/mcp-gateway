# Installation

## Prerequisites

- Go 1.23+ (`brew install go`)
- macOS (for launchd socket activation)

## Quick Install

```bash
git clone <repo-url> ~/code/mcp-gateway
cd ~/code/mcp-gateway
make install
launchctl load ~/Library/LaunchAgents/io.github.jbcjorge.mcp-gateway.plist
```

This builds the binary, installs it to `~/.local/bin/`, creates the config directory, generates the launchd plist, and tells you how to load it.

## What Gets Installed

| Path | Purpose |
|------|---------|
| `~/.local/bin/mcp-gateway` | Binary |
| `~/.config/mcp-gateway/config.json` | Gateway configuration (created on first install, never overwritten) |
| `~/.config/mcp-gateway/backends.json` | Your machine-specific backend definitions (create manually) |
| `~/.local/var/log/mcp-gateway.log` | Application log (rotated by the binary) |
| `~/.local/var/log/mcp-gateway.stderr.log` | Crash/startup log (fallback, managed by launchd) |
| `~/Library/LaunchAgents/io.github.jbcjorge.mcp-gateway.plist` | launchd service (socket activation) |

## Step by Step

### 1. Build and install

```bash
make install
```

This:
- Builds `mcp-gateway` with version from git tags
- Copies it to `~/.local/bin/`
- Creates `~/.config/mcp-gateway/` if it doesn't exist
- Copies the default `config.json` (only if one doesn't already exist)
- Generates and installs the launchd plist (first time only; prompts on changes)

### 2. Create your backends file

Create `~/.config/mcp-gateway/backends.json` with your machine-specific MCP backends:

```json
{
  "wiki": {
    "command": ["python3", "/path/to/your/confluence/server.py"]
  },
  "jira": {
    "command": ["bash", "/path/to/your/mcp-atlassian/run.sh"]
  }
}
```

Each backend needs a `command` (array of strings) and optionally `env` (map of extra environment variables).

### 3. Edit the config

Edit `~/.config/mcp-gateway/config.json` if you need to change defaults:

```json
{
  "listen": ":19900",
  "log_level": "info",
  "log_file": "/path/to/log/mcp-gateway.log",
  "log_max_size_mb": 10,
  "log_max_files": 3,
  "idle_timeout_seconds": 300,
  "self_idle_timeout_seconds": 3600,
  "backends_file": "backends.json"
}
```

| Field | Default | Description |
|-------|---------|-------------|
| `listen` | `:19900` | Address to listen on |
| `log_level` | `info` | Log level: debug, info, warn, error |
| `log_file` | (stderr) | Path to log file. If set, logs to file with rotation |
| `log_max_size_mb` | `10` | Rotate log when it exceeds this size |
| `log_max_files` | `3` | Number of rotated log files to keep |
| `idle_timeout_seconds` | `300` | Kill backend subprocess after this many seconds idle |
| `self_idle_timeout_seconds` | `3600` | Gateway exits after this many seconds with no requests |
| `backends_file` | - | Path to backends.json (relative to config dir or absolute) |

### 4. Load the launchd service

```bash
launchctl load ~/Library/LaunchAgents/io.github.jbcjorge.mcp-gateway.plist
```

With launchd socket activation:
- The gateway is completely dormant until something connects to port 19900
- macOS spawns it on the first connection
- After 1 hour of inactivity (configurable), the gateway exits
- launchd respawns it on the next connection

You only need to run `launchctl load` once. Subsequent `make install` calls update the binary in-place; launchd picks up the new version on next spawn.

### 5. Configure Kiro to use the gateway

In `~/.kiro/settings/mcp.json`, use URL-based servers pointing to the gateway:

```json
{
  "mcpServers": {
    "wiki": { "url": "http://localhost:19900/wiki/mcp" },
    "jira": { "url": "http://localhost:19900/jira/mcp" }
  }
}
```

## Running Manually (without launchd)

If you prefer not to use launchd:

```bash
mcp-gateway ~/.config/mcp-gateway/config.json
```

The binary is idempotent - running it multiple times is safe. The second invocation detects the running instance and exits cleanly.

## Updating

```bash
cd ~/code/mcp-gateway
git pull
make install
# If the service is running, it picks up the new binary on next restart.
# Force restart:
launchctl unload ~/Library/LaunchAgents/io.github.jbcjorge.mcp-gateway.plist
launchctl load ~/Library/LaunchAgents/io.github.jbcjorge.mcp-gateway.plist
```

## Uninstalling

```bash
cd ~/code/mcp-gateway
make uninstall
```

This stops the service, removes the binary, and removes the plist. Your config files in `~/.config/mcp-gateway/` are preserved.

## Troubleshooting

### Gateway not responding

```bash
# Check if the service is loaded
launchctl list | grep mcp-gateway

# Check the log
cat ~/.local/var/log/mcp-gateway.log

# Check the crash log (pre-config-load failures)
cat ~/.local/var/log/mcp-gateway.stderr.log

# Try running manually to see errors
mcp-gateway ~/.config/mcp-gateway/config.json
```

### Backend not spawning

```bash
# Check health for backend status
curl http://localhost:19900/health | python3 -m json.tool

# Restart a specific backend
curl -X POST http://localhost:19900/_restart/wiki
```

### Port already in use

```bash
# Check what's on the port
lsof -i :19900

# The binary is idempotent - if it detects a healthy instance, it exits 0
mcp-gateway ~/.config/mcp-gateway/config.json
```

### Enable debug logging

Either set in config:
```json
{"log_level": "debug"}
```

Or via environment variable (overrides config):
```bash
MCP_GATEWAY_DEBUG=1 mcp-gateway ~/.config/mcp-gateway/config.json
```
