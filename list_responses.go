package main

import (
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
