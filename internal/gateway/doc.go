// Package gateway implements the mcp-gateway runtime: it routes MCP Streamable
// HTTP requests by path to backends (lazily spawned stdio subprocesses or remote
// servers), merges compositions, serves keyed backends, brokers OAuth, and manages
// backend lifecycle (idle reaping, credential-TTL recycling, hot reload).
//
// The configuration model lives in package config; process-group teardown in
// procgroup. Construct a Gateway with New and serve it as an http.Handler.
package gateway
