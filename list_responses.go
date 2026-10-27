package main

import (
	"context"
	"encoding/json"
	"log/slog"
)

// jsonRPCVersion is the JSON-RPC protocol version string used in responses.
const jsonRPCVersion = "2.0"

// Local MCP list methods that the gateway answers without spawning the backend.
// Clients (e.g. kiro-cli) call these during the connect handshake; forwarding
// them would cold-spawn a slow OAuth backend and blow the client's connect
// timeout. Backends probed in practice return empty lists for all three, so a
// cache-or-empty local answer is lossless. See list_responses.go.
const (
	methodResourcesList     = "resources/list"
	methodResourceTemplates = "resources/templates/list"
	methodPromptsList       = "prompts/list"
)

// emptyListResult maps each local list method to the MCP-correct empty result
// object (the array key differs per method).
var emptyListResult = map[string]map[string]any{
	methodResourcesList:     {"resources": []any{}},
	methodResourceTemplates: {"resourceTemplates": []any{}},
	methodPromptsList:       {"prompts": []any{}},
}

// isLocalListMethod reports whether method is one the gateway answers locally
// (cache-or-empty) instead of forwarding to the backend.
func isLocalListMethod(method string) bool {
	_, ok := emptyListResult[method]
	return ok
}

// localListResponse builds a JSON-RPC response for a local list method, using
// the backend's cached result object when present and otherwise the empty
// result. It never spawns the backend. Returns nil if method is not a local
// list method (caller should fall through to normal dispatch).
func (b *Backend) localListResponse(method string, id *json.RawMessage) []byte {
	empty, ok := emptyListResult[method]
	if !ok {
		return nil
	}

	cached := b.cachedListResult(method)
	result := any(empty)
	source := "empty"
	if cached != nil {
		result = json.RawMessage(cached)
		source = "cache"
	}

	resp := map[string]any{"jsonrpc": jsonRPCVersion, "id": id, "result": result}
	data, _ := json.Marshal(resp)
	slog.Debug("list method served locally (no spawn)",
		"backend", b.name, "method", method, "source", source)
	return data
}

// cachedListResult returns the cached result object for a list method, or nil
// when nothing is cached. Lock-guarded read of the per-backend caches.
func (b *Backend) cachedListResult(method string) json.RawMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch method {
	case methodResourcesList:
		return b.resourcesCache
	case methodPromptsList:
		return b.promptsCache
	default:
		// resources/templates/list has no dedicated cache; always empty.
		return nil
	}
}

// refreshListCaches pulls the backend's real resources/list and prompts/list
// once per backend process (gated by listCachePid) and caches the result
// objects, so later connect-handshake calls are served the backend's actual
// data without spawning. This runs only when the backend is ALREADY spawned
// (right after init, alongside refreshToolsCache); it never spawns a backend
// just to probe. A backend that returns empty (or errors / lacks the method)
// is cached as the verified empty result, so "empty" is confirmed per backend
// rather than assumed, and we do not re-probe on every call.
func (b *Backend) refreshListCaches() {
	b.mu.Lock()
	pid := 0
	if b.cmd != nil && b.cmd.Process != nil {
		pid = b.cmd.Process.Pid
	}
	if pid == 0 || pid == b.listCachePid {
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()

	resources := b.captureListResult(methodResourcesList)
	prompts := b.captureListResult(methodPromptsList)

	b.mu.Lock()
	b.resourcesCache = resources
	b.promptsCache = prompts
	b.listCachePid = pid
	b.mu.Unlock()

	slog.Info("resources/prompts caches refreshed from live backend",
		"backend", b.name, "pid", pid,
		"resources_empty", isEmptyListResult(resources, "resources"),
		"prompts_empty", isEmptyListResult(prompts, "prompts"))
}

// captureListResult sends a list method to the live backend and returns its
// result object. On any failure (send error, JSON-RPC error, missing result)
// it returns the MCP-correct empty result for the method, so the cache always
// holds a servable value and the method is not re-probed.
func (b *Backend) captureListResult(method string) json.RawMessage {
	empty, _ := json.Marshal(emptyListResult[method])
	msg := []byte(`{"jsonrpc":"` + jsonRPCVersion + `","id":"_gw_` + method + `","method":"` + method + `","params":{}}`)
	resp, err := b.send(context.Background(), msg)
	if err != nil {
		slog.Debug("list cache probe failed; caching empty", "backend", b.name, "method", method, "error", err)
		return empty
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(resp, &env) != nil || env.Error != nil || env.Result == nil {
		slog.Debug("list cache probe not usable; caching empty", "backend", b.name, "method", method)
		return empty
	}
	return env.Result
}

// isEmptyListResult reports whether a cached result object's array under key is
// empty or absent. Best-effort, for logging only.
func isEmptyListResult(result json.RawMessage, key string) bool {
	if result == nil {
		return true
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(result, &m) != nil {
		return false
	}
	arr, ok := m[key]
	if !ok {
		return true
	}
	var items []json.RawMessage
	if json.Unmarshal(arr, &items) != nil {
		return false
	}
	return len(items) == 0
}
