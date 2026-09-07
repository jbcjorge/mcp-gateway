package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	errors "github.com/jbcjorge/errors-library"
)

// handleRequest routes /<backend>/mcp to the appropriate backend.
func (gw *Gateway) handleRequest(w http.ResponseWriter, r *http.Request) {
	// Restart endpoint: POST /_restart/<backend>
	if strings.HasPrefix(r.URL.Path, "/_restart/") {
		gw.handleRestart(w, r)
		return
	}

	// Health endpoint (GET)
	if r.URL.Path == "/health" {
		gw.handleHealth(w)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Track gateway-level activity
	gw.touchLastRequest()

	// Parse path: /<backend>/mcp or /<backend>
	path := strings.TrimPrefix(r.URL.Path, "/")
	path = strings.TrimSuffix(path, "/mcp")
	path = strings.TrimSuffix(path, "/")

	if path == "" {
		http.Error(w, "missing backend name in path", http.StatusBadRequest)
		return
	}

	gw.mu.RLock()
	route, ok := gw.routes[path]
	gw.mu.RUnlock()
	if !ok {
		http.Error(w, fmt.Sprintf("unknown backend: %s", path), http.StatusNotFound)
		return
	}

	// Auth check (keyed by route name)
	if authErr := gw.authorizer.Authorize(r, path); authErr != nil {
		w.WriteHeader(http.StatusUnauthorized)
		gw.writeResponse(w, r, jsonRPCError(nil, CodeAuthError, authErr))
		return
	}

	// Read request body (capped to prevent OOM)
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodySize))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Parse the JSON-RPC envelope to check the method
	var envelope jsonRPCMessage
	if unmarshalErr := json.Unmarshal(body, &envelope); unmarshalErr != nil {
		parseErr := ErrInvalidJSON.Parse(errors.WithError(unmarshalErr))
		w.WriteHeader(http.StatusBadRequest)
		gw.writeResponse(w, r, jsonRPCError(nil, CodeParseError, parseErr))
		return
	}

	// Handle subscriptions/listen - long-lived SSE stream
	if envelope.Method == "subscriptions/listen" {
		gw.handleSubscriptionsListen(w, r, envelope)
		return
	}

	resp, err := gw.dispatchRoute(r.Context(), route, envelope, body)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		gw.writeResponse(w, r, jsonRPCError(envelope.ID, CodeBackendError, err))
		return
	}
	gw.writeResponse(w, r, resp)
}

// dispatchRoute handles a request for a route. Single-member routes with no
// prefix take the exact legacy path (delegate to the one backend). Composite or
// prefixed routes use route-level merged tools/list and ownership dispatch.
func (gw *Gateway) dispatchRoute(ctx context.Context, route *Route, envelope jsonRPCMessage, body []byte) ([]byte, error) {
	if !route.isComposite() && route.prefix == "" {
		b := route.members[0]
		if resp, handled := gw.handleLocally(envelope, body, b); handled {
			return resp, nil
		}
		return gw.forwardToBackend(ctx, b, envelope, body)
	}
	return gw.dispatchComposite(ctx, route, envelope, body)
}

// dispatchComposite handles a request for a composite or prefixed route.
func (gw *Gateway) dispatchComposite(ctx context.Context, route *Route, envelope jsonRPCMessage, body []byte) ([]byte, error) {
	switch envelope.Method {
	case "initialize":
		return localInitializeResponse(envelope.ID, route.name), nil
	case "notifications/initialized":
		return []byte{}, nil
	case "ping":
		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": envelope.ID, "result": map[string]any{}})
		return data, nil
	case "tools/list":
		return gw.compositeToolsList(ctx, route, envelope.ID)
	case "tools/call":
		return gw.compositeToolsCall(ctx, route, envelope, body)
	default:
		// Non-tool methods route to the primary member.
		return gw.forwardToBackend(ctx, route.primary(), envelope, body)
	}
}

// compositeToolsList fetches each member's tools/list (spawning as needed),
// merges + prefixes, and returns the merged result wrapped in a JSON-RPC reply.
func (gw *Gateway) compositeToolsList(ctx context.Context, route *Route, id *json.RawMessage) ([]byte, error) {
	merged, err := route.toolsList(func(b *Backend) (json.RawMessage, error) {
		if err := b.ensureRunning(); err != nil {
			return nil, err
		}
		listReq := []byte(`{"jsonrpc":"2.0","id":"_gw_tools","method":"tools/list","params":{}}`)
		resp, err := b.send(ctx, listReq)
		if err != nil {
			return nil, err
		}
		// Extract the "result" object from the member's JSON-RPC response.
		var env struct {
			Result json.RawMessage `json:"result"`
		}
		if uerr := json.Unmarshal(resp, &env); uerr != nil {
			return nil, uerr
		}
		return env.Result, nil
	})
	if err != nil {
		return nil, err
	}
	reply, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": json.RawMessage(merged)})
	return reply, nil
}

// compositeToolsCall routes a tools/call to the owning member, stripping the
// route prefix from the tool name before forwarding.
func (gw *Gateway) compositeToolsCall(ctx context.Context, route *Route, envelope jsonRPCMessage, body []byte) ([]byte, error) {
	name := callToolName(body)
	member, realName, ok := route.ownerOfTool(name)
	if !ok {
		// Refresh the merged list once (cache may be cold), then retry.
		if _, err := gw.compositeToolsList(ctx, route, nil); err == nil {
			member, realName, ok = route.ownerOfTool(name)
		}
	}
	if !ok {
		return nil, ErrUnknownTool.Parse(errors.WithParsedMessage(name))
	}
	fwdBody := body
	if realName != name {
		rewritten, err := rewriteToolCallName(body, realName)
		if err != nil {
			return nil, err
		}
		fwdBody = rewritten
	}
	return gw.forwardToBackend(ctx, member, envelope, fwdBody)
}

// localInitializeResponse builds the standard initialize reply for a route.
func localInitializeResponse(id *json.RawMessage, routeName string) []byte {
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{
				"tools":     map[string]any{"listChanged": true},
				"resources": map[string]any{"listChanged": true},
			},
			"serverInfo": map[string]any{"name": "mcp-gateway/" + routeName, "version": "1.0.0"},
		},
	}
	data, _ := json.Marshal(resp)
	return data
}

// forwardToBackend sends a request to the backend, retrying once on failure.
// Also handles tools/list caching and smart category activation.
func (gw *Gateway) forwardToBackend(ctx context.Context, backend *Backend, envelope jsonRPCMessage, body []byte) ([]byte, error) {
	if err := backend.ensureRunning(); err != nil {
		return nil, ErrBackendStartFailed.Parse(errors.WithError(err))
	}

	resp, err := backend.send(ctx, body)
	if err != nil {
		slog.Warn("send failed, restarting backend", "backend", backend.name, "error", err)
		backend.kill()
		if restartErr := backend.ensureRunning(); restartErr != nil {
			return nil, ErrBackendRestartFailed.Parse(errors.WithError(restartErr))
		}
		resp, err = backend.send(ctx, body)
		if err != nil {
			return nil, ErrBackendSendFailed.Parse(errors.WithError(err))
		}
	}

	// Cache tools/list responses
	if envelope.Method == "tools/list" {
		backend.mu.Lock()
		backend.toolsCache = json.RawMessage(resp)
		backend.mu.Unlock()
		backend.saveToolsCache()
		backend.buildCategories()
		gw.notifyToolsChanged()
		slog.Debug("tools/list cached and categories rebuilt", "backend", backend.name)
	}

	// Smart category activation on tools/call
	if envelope.Method == "tools/call" {
		var callBody struct {
			Params *struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if json.Unmarshal(body, &callBody) == nil && callBody.Params != nil && callBody.Params.Name != "" {
			backend.smartActivate(callBody.Params.Name)
		}
	}

	return resp, nil
}

// handleLocally responds to MCP lifecycle methods without spawning the backend.
// Returns the response bytes and true if handled, or nil/false to forward to backend.
func (gw *Gateway) handleLocally(env jsonRPCMessage, body []byte, b *Backend) ([]byte, bool) {
	switch env.Method {
	case "initialize":
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      env.ID,
			"result": map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities": map[string]any{
					"tools":     map[string]any{"listChanged": true},
					"resources": map[string]any{"listChanged": true},
				},
				"serverInfo": map[string]any{
					"name":    "mcp-gateway/" + b.name,
					"version": "1.0.0",
				},
			},
		}
		data, _ := json.Marshal(resp)
		slog.Debug("initialize handled locally", "backend", b.name)
		return data, true

	case "notifications/initialized":
		return []byte{}, true

	case "ping":
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      env.ID,
			"result":  map[string]any{},
		}
		data, _ := json.Marshal(resp)
		return data, true

	case "tools/list":
		return b.handleToolsList(env.ID)

	case "tools/call":
		return gw.handleDiscoverTools(env, body, b)

	default:
		return nil, false
	}
}

// handleRestart kills a backend so the next request re-spawns it fresh.
func (gw *Gateway) handleRestart(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/_restart/")
	name = strings.TrimSuffix(name, "/")
	gw.mu.RLock()
	b, ok := gw.backends[name]
	gw.mu.RUnlock()
	if !ok {
		http.Error(w, fmt.Sprintf("unknown backend: %s", name), http.StatusNotFound)
		return
	}
	wasRunning := b.isRunning()
	if wasRunning {
		b.kill()
		slog.Info("backend restarted via /_restart", "backend", name)
	}
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{"backend": name, "was_running": wasRunning, "status": "killed"}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Warn("restart response write failed", "error", err)
	}
}

// handleHealth responds with gateway and backend status.
func (gw *Gateway) handleHealth(w http.ResponseWriter) {
	gw.touchLastRequest()
	w.Header().Set("Content-Type", "application/json")
	type backendStatus struct {
		Running bool `json:"running"`
		IdleSec int  `json:"idle_seconds,omitempty"`
		Pid     int  `json:"pid,omitempty"`
	}
	statuses := make(map[string]backendStatus)
	gw.mu.RLock()
	for name, b := range gw.backends {
		b.mu.Lock()
		s := backendStatus{Running: b.running}
		if b.running {
			if !b.lastUsed.IsZero() {
				s.IdleSec = int(time.Since(b.lastUsed).Seconds())
			}
			if b.cmd != nil && b.cmd.Process != nil {
				s.Pid = b.cmd.Process.Pid
			}
		}
		b.mu.Unlock()
		statuses[name] = s
	}
	gw.mu.RUnlock()
	if err := json.NewEncoder(w).Encode(map[string]any{
		"status":   "ok",
		"version":  Version,
		"pid":      os.Getpid(),
		"backends": statuses,
	}); err != nil {
		slog.Error("health response write failed", "error", err)
	}
}

// handleSubscriptionsListen keeps the response open as an SSE stream and pushes
// notifications (like tools/list_changed) when they occur.
func (gw *Gateway) handleSubscriptionsListen(w http.ResponseWriter, r *http.Request, env jsonRPCMessage) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Register listener
	ch := make(chan string, 16)
	remove := gw.addListener(ch)
	defer remove()

	slog.Info("subscriptions/listen stream opened")

	// Stream until client disconnects
	ctx := r.Context()
	keepAlive := time.NewTicker(30 * time.Second)
	defer keepAlive.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("subscriptions/listen stream closed")
			return
		case msg := <-ch:
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
			flusher.Flush()
		case <-keepAlive.C:
			// SSE keep-alive comment
			fmt.Fprintf(w, ":\n\n")
			flusher.Flush()
		}
	}
}

// addListener registers an SSE listener channel and returns a remove function.
func (gw *Gateway) addListener(ch chan string) func() {
	gw.listenersMu.Lock()
	gw.listeners = append(gw.listeners, ch)
	gw.listenersMu.Unlock()
	return func() {
		gw.listenersMu.Lock()
		for i, l := range gw.listeners {
			if l == ch {
				gw.listeners = append(gw.listeners[:i], gw.listeners[i+1:]...)
				break
			}
		}
		gw.listenersMu.Unlock()
	}
}

// notifyToolsChanged sends notifications/tools/list_changed to all SSE listeners.
func (gw *Gateway) notifyToolsChanged() {
	notification := `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`
	gw.listenersMu.Lock()
	defer gw.listenersMu.Unlock()
	for _, ch := range gw.listeners {
		select {
		case ch <- notification:
		default:
			// Listener buffer full, skip (non-blocking)
		}
	}
	if len(gw.listeners) > 0 {
		slog.Debug("tools/list_changed pushed to listeners", "count", len(gw.listeners))
	}
}

// writeResponse sends the response in the format the client expects (SSE or JSON).
func (gw *Gateway) writeResponse(w http.ResponseWriter, r *http.Request, resp []byte) {
	if len(resp) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "text/event-stream") {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", resp)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	} else {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(resp); err != nil {
			slog.Error("response write failed", "error", err)
		}
	}
}
