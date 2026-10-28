package main

import (
	"encoding/json"

	errors "github.com/jbcjorge/errors-library"
)

// Sentinel errors for the mcp-gateway.
// Each represents a distinct failure category that callers can match with errors.Is.

// Auth errors
var (
	ErrAuthRequired    = errors.New("authorization required")
	ErrInvalidToken    = errors.New("invalid token")
	ErrAuthHookMissing = errors.New("auth hook has no provider command")
	ErrAuthHookFailed  = errors.New("auth hook provider command failed")
	ErrAuthHookEmpty   = errors.New("auth hook provider returned no token")
)

// Config errors
var (
	ErrConfigLoad    = errors.New("failed to load config file %s")
	ErrConfigParse   = errors.New("failed to parse config file %s")
	ErrBackendsLoad  = errors.New("failed to load backends file %s")
	ErrBackendsParse = errors.New("failed to parse backends file %s")
	ErrLogFileOpen   = errors.New("failed to open log file %s")
)

// Request errors
var (
	ErrMethodNotAllowed = errors.New("method not allowed")
	ErrMissingBackend   = errors.New("missing backend name in path")
	ErrUnknownBackend   = errors.New("unknown backend: %s")
	ErrInvalidJSON      = errors.New("invalid JSON-RPC request")
	ErrReadBody         = errors.New("failed to read request body")
)

// Backend lifecycle errors
var (
	ErrBackendNotConfigured = errors.New("no command or url defined for backend %s")
	ErrBackendStartFailed   = errors.New("backend start failed")
	ErrBackendRestartFailed = errors.New("backend restart failed")
	ErrBackendInitFailed    = errors.New("backend initialization failed")
	ErrBackendNotRunning    = errors.New("backend not running")
	ErrBackendClosed        = errors.New("backend closed")
	ErrBackendTimeout       = errors.New("timeout waiting for response")
	ErrBackendSendFailed    = errors.New("failed to send to backend")
)

// Subprocess errors
var (
	ErrStdinPipe           = errors.New("stdin pipe failed")
	ErrStdoutPipe          = errors.New("stdout pipe failed")
	ErrStderrPipe          = errors.New("stderr pipe failed")
	ErrCABundleMissing     = errors.New("required CA bundle missing or unreadable")
	ErrRouteUnknown        = errors.New("backends route references unknown server or composition: %s")
	ErrMemberServerUnknown = errors.New("composition member references unknown server: %s")
	ErrMergeParse          = errors.New("failed to parse member list for merge")
	ErrUnknownTool         = errors.New("unknown tool: %s")
	ErrCmdStart            = errors.New("failed to start command")
	ErrWritePipe           = errors.New("failed to write to backend")
	ErrParseMsgID          = errors.New("failed to parse message id")
)

// Remote backend errors
var (
	ErrUnknownTransport = errors.New("unknown transport type: %s")
	ErrRemoteRequest    = errors.New("remote request failed")
	ErrRemoteStatus     = errors.New("remote returned HTTP %d")
	ErrRemoteRead       = errors.New("failed to read remote response")
	ErrSSEStreamRead    = errors.New("SSE stream read error")
	ErrSSENoResponse    = errors.New("SSE stream ended without response")
)

// Tool discovery errors
var (
	ErrUnknownCategory = errors.New("unknown category %s")
	ErrNoToolsCache    = errors.New("no tools cache")
)

// Keyed backend errors
var (
	ErrKeyedParamMissing   = errors.New("missing required parameter: %s")
	ErrKeyedParamNotString = errors.New("parameter must be a string: %s")
	ErrKeyedParamInvalid   = errors.New("parameter failed validation: %s")
	ErrKeyedParamBadRegex  = errors.New("invalid validation regex for parameter: %s")
	ErrKeyedStripParse     = errors.New("failed to parse tools/call for keyed routing")
	ErrKeyedChildFailed    = errors.New("failed to start keyed child instance")
)

// JSON-RPC error codes (MCP standard)
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInternalError  = -32603
	CodeBackendError   = -32001
	CodeAuthError      = -32002
)

// jsonRPCError builds a JSON-RPC error response.
func jsonRPCError(id *json.RawMessage, code int, err error) []byte {
	var structuredErr *errors.Error
	var data map[string]any
	if errors.As(err, &structuredErr) {
		data = structuredErr.SafeMap()
	}

	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    code,
			"message": safeErrorMessage(err),
		},
	}
	if data != nil {
		resp["error"].(map[string]any)["data"] = data
	}
	result, _ := json.Marshal(resp)
	return result
}

// safeErrorMessage returns a PII-free error message.
func safeErrorMessage(err error) string {
	var structuredErr *errors.Error
	if errors.As(err, &structuredErr) {
		return structuredErr.SafeError()
	}
	return err.Error()
}
