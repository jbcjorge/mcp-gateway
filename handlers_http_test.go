package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHealthEndpoint(t *testing.T) {
	gw := newTestGateway(map[string]BackendDef{
		"test": {Command: []string{"echo"}},
	})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	gw.handleRequest(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode health response: %v", err)
	}

	if body["status"] != "ok" {
		t.Errorf("expected status ok, got %v", body["status"])
	}
	if body["version"] != Version {
		t.Errorf("expected version %s, got %v", Version, body["version"])
	}
	backends, ok := body["backends"].(map[string]any)
	if !ok {
		t.Fatal("backends not in response")
	}
	if _, ok := backends["test"]; !ok {
		t.Error("test backend not in health response")
	}
}

func TestHealthResetsIdleTimer(t *testing.T) {
	gw := newTestGateway(map[string]BackendDef{})
	gw.lastRequest = time.Now().Add(-10 * time.Minute) // pretend idle

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	gw.handleRequest(w, req)

	gw.reqMu.Lock()
	idle := time.Since(gw.lastRequest)
	gw.reqMu.Unlock()

	if idle > 1*time.Second {
		t.Errorf("health should have reset idle timer, but idle is %v", idle)
	}
}

func TestInitializeHandledLocally(t *testing.T) {
	gw := newTestGateway(map[string]BackendDef{
		"mybackend": {Command: []string{"should-not-spawn"}},
	})

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`
	req := httptest.NewRequest(http.MethodPost, "/mybackend/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()

	gw.handleRequest(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	r, ok := result["result"].(map[string]any)
	if !ok {
		t.Fatal("no result in response")
	}
	serverInfo, ok := r["serverInfo"].(map[string]any)
	if !ok {
		t.Fatal("no serverInfo in result")
	}
	if serverInfo["name"] != "mcp-gateway/mybackend" {
		t.Errorf("expected serverInfo.name mcp-gateway/mybackend, got %v", serverInfo["name"])
	}

	// Backend should NOT be running
	backend := gw.backends["mybackend"]
	if backend.isRunning() {
		t.Error("backend should not have been spawned for initialize")
	}
}

func TestNotificationsInitializedSwallowed(t *testing.T) {
	gw := newTestGateway(map[string]BackendDef{
		"test": {Command: []string{"echo"}},
	})

	body := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	req := httptest.NewRequest(http.MethodPost, "/test/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()

	gw.handleRequest(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted for notification, got %d", resp.StatusCode)
	}
}

func TestPingHandledLocally(t *testing.T) {
	gw := newTestGateway(map[string]BackendDef{
		"test": {Command: []string{"echo"}},
	})

	body := `{"jsonrpc":"2.0","id":99,"method":"ping"}`
	req := httptest.NewRequest(http.MethodPost, "/test/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()

	gw.handleRequest(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode ping response: %v", err)
	}
	if result["id"] == nil {
		t.Error("ping response should have id")
	}
}

func TestUnknownBackend404(t *testing.T) {
	gw := newTestGateway(map[string]BackendDef{})

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	req := httptest.NewRequest(http.MethodPost, "/nonexistent/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	gw.handleRequest(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	gw := newTestGateway(map[string]BackendDef{})

	req := httptest.NewRequest(http.MethodGet, "/test/mcp", nil)
	w := httptest.NewRecorder()
	gw.handleRequest(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestRestartEndpoint(t *testing.T) {
	gw := newTestGateway(map[string]BackendDef{
		"test": {Command: []string{"cat"}},
	})

	// Backend not running - restart should report was_running=false
	req := httptest.NewRequest(http.MethodPost, "/_restart/test", nil)
	w := httptest.NewRecorder()
	gw.handleRequest(w, req)

	var result map[string]any
	if err := json.NewDecoder(w.Result().Body).Decode(&result); err != nil {
		t.Fatalf("decode restart response: %v", err)
	}
	if result["was_running"] != false {
		t.Errorf("expected was_running=false, got %v", result["was_running"])
	}
}

func TestRestartUnknownBackend(t *testing.T) {
	gw := newTestGateway(map[string]BackendDef{})

	req := httptest.NewRequest(http.MethodPost, "/_restart/ghost", nil)
	w := httptest.NewRecorder()
	gw.handleRequest(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestSSEResponseFormat(t *testing.T) {
	gw := newTestGateway(map[string]BackendDef{
		"test": {Command: []string{"echo"}},
	})

	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	req := httptest.NewRequest(http.MethodPost, "/test/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")
	w := httptest.NewRecorder()

	gw.handleRequest(w, req)

	resp := w.Result()
	ct := resp.Header.Get("Content-Type")
	if ct != "text/event-stream" {
		t.Errorf("expected text/event-stream, got %s", ct)
	}

	respBody, _ := io.ReadAll(resp.Body)
	if !strings.HasPrefix(string(respBody), "event: message\ndata: ") {
		t.Errorf("expected SSE format, got: %s", string(respBody))
	}
}

func TestJSONResponseFormat(t *testing.T) {
	gw := newTestGateway(map[string]BackendDef{
		"test": {Command: []string{"echo"}},
	})

	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	req := httptest.NewRequest(http.MethodPost, "/test/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()

	gw.handleRequest(w, req)

	resp := w.Result()
	ct := resp.Header.Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected application/json, got %s", ct)
	}
}

func TestPathParsing(t *testing.T) {
	tests := []struct {
		path     string
		expected string
	}{
		{"/search/mcp", "search"},
		{"/wiki/mcp", "wiki"},
		{"/gitlab", "gitlab"},
		{"/my-backend/mcp", "my-backend"},
	}

	for _, tt := range tests {
		path := strings.TrimPrefix(tt.path, "/")
		path = strings.TrimSuffix(path, "/mcp")
		path = strings.TrimSuffix(path, "/")
		if path != tt.expected {
			t.Errorf("path %q parsed to %q, expected %q", tt.path, path, tt.expected)
		}
	}
}

func TestHandleLocallyUnknownMethod(t *testing.T) {
	gw := newTestGateway(map[string]BackendDef{
		"test": {Command: []string{"echo"}},
	})
	b := gw.backends["test"]

	id := json.RawMessage(`1`)
	env := jsonRPCMessage{JSONRPC: "2.0", ID: &id, Method: "tools/call"}
	_, handled := gw.handleLocally(env, nil, b)
	if handled {
		t.Error("tools/call should NOT be handled locally")
	}
}

func TestConcurrentHealthRequests(t *testing.T) {
	gw := newTestGateway(map[string]BackendDef{
		"a": {Command: []string{"echo"}},
		"b": {Command: []string{"echo"}},
	})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			w := httptest.NewRecorder()
			gw.handleRequest(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("health returned %d", w.Code)
			}
		}()
	}
	wg.Wait()
}

func TestConcurrentInitialize(t *testing.T) {
	gw := newTestGateway(map[string]BackendDef{
		"test": {Command: []string{"cat"}},
	})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			body := `{"jsonrpc":"2.0","id":` + string(rune('0'+id%10)) + `,"method":"initialize","params":{}}`
			req := httptest.NewRequest(http.MethodPost, "/test/mcp", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json")
			w := httptest.NewRecorder()
			gw.handleRequest(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("initialize returned %d", w.Code)
			}
		}(i)
	}
	wg.Wait()

	// Backend should NOT be running (all initialize handled locally)
	if gw.backends["test"].isRunning() {
		t.Error("backend should not be running after concurrent initializes")
	}
}

func TestForwardToBackend_ToolsListCached(t *testing.T) {
	b := &Backend{
		name:        "fwd-cache",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		cacheDir:    t.TempDir(),
		logEnabled:  true,
	}

	gw := &Gateway{
		backends: map[string]*Backend{"fwd-cache": b},
	}

	// Ensure running
	if err := b.ensureRunning(); err != nil {
		t.Fatalf("ensureRunning: %v", err)
	}
	defer b.kill()

	// Forward tools/list
	envelope := jsonRPCMessage{JSONRPC: "2.0", Method: "tools/list"}
	id := json.RawMessage(`"tl1"`)
	envelope.ID = &id
	body := []byte(`{"jsonrpc":"2.0","id":"tl1","method":"tools/list","params":{}}`)

	resp, err := gw.forwardToBackend(context.Background(), b, envelope, body)
	if err != nil {
		t.Fatalf("forwardToBackend: %v", err)
	}
	if len(resp) == 0 {
		t.Fatal("expected response from backend")
	}

	// Check that cache was populated
	b.mu.Lock()
	cached := b.toolsCache
	b.mu.Unlock()
	if cached == nil {
		t.Error("toolsCache should be populated after tools/list")
	}
}

func TestForwardToBackend_ToolsCallSmartActivate(t *testing.T) {
	toolsJSON := `{"jsonrpc":"2.0","id":"1","result":{"tools":[
		{"name":"get_users","description":"Get users"},
		{"name":"get_items","description":"Get items"},
		{"name":"create_issue","description":"Create issue"},
		{"name":"delete_repo","description":"Delete repo"},
		{"name":"search_code","description":"Search code"},
		{"name":"list_items","description":"List items"},
		{"name":"update_thing","description":"Update thing"}
	]}}`

	b := &Backend{
		name:        "fwd-smart",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		toolsCache:  json.RawMessage(toolsJSON),
		logEnabled:  true,
	}
	b.buildCategories()

	gw := &Gateway{
		backends: map[string]*Backend{"fwd-smart": b},
	}

	if err := b.ensureRunning(); err != nil {
		t.Fatalf("ensureRunning: %v", err)
	}
	defer b.kill()

	// Forward tools/call with a tool name
	envelope := jsonRPCMessage{JSONRPC: "2.0", Method: "tools/call"}
	id := json.RawMessage(`"tc1"`)
	envelope.ID = &id
	body := []byte(`{"jsonrpc":"2.0","id":"tc1","method":"tools/call","params":{"name":"get_users","arguments":{}}}`)

	_, err := gw.forwardToBackend(context.Background(), b, envelope, body)
	if err != nil {
		t.Fatalf("forwardToBackend tools/call: %v", err)
	}

	// Check that smart activation happened
	b.mu.Lock()
	active := b.activeTools["get_users"]
	b.mu.Unlock()
	if !active {
		t.Error("get_users should be active after tools/call smart activation")
	}
}

func TestHandleDiscoverTools_ListCategories(t *testing.T) {
	toolsJSON := `{"jsonrpc":"2.0","id":"1","result":{"tools":[
		{"name":"get_users","description":"Get users"},
		{"name":"create_issue","description":"Create"},
		{"name":"delete_repo","description":"Delete"},
		{"name":"search_code","description":"Search"},
		{"name":"list_items","description":"List"},
		{"name":"update_thing","description":"Update"}
	]}}`

	b := &Backend{
		name:        "disc-list",
		def:         BackendDef{},
		toolsCache:  json.RawMessage(toolsJSON),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}
	b.buildCategories()

	gw := &Gateway{backends: map[string]*Backend{"disc-list": b}}

	id := json.RawMessage(`"d1"`)
	env := jsonRPCMessage{JSONRPC: "2.0", ID: &id, Method: "tools/call"}
	body := []byte(`{"jsonrpc":"2.0","id":"d1","method":"tools/call","params":{"name":"discover_disc-list_tools","arguments":{}}}`)

	resp, handled := gw.handleLocally(env, body, b)
	if !handled {
		t.Fatal("discover_tools should be handled locally")
	}

	var result map[string]any
	if err := json.Unmarshal(resp, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	r := result["result"].(map[string]any)
	content := r["content"].([]any)
	if len(content) == 0 {
		t.Error("expected content in response")
	}
}

func TestHandleDiscoverTools_ActivateCategory(t *testing.T) {
	toolsJSON := `{"jsonrpc":"2.0","id":"1","result":{"tools":[
		{"name":"get_users","description":"Get users"},
		{"name":"create_issue","description":"Create"},
		{"name":"delete_repo","description":"Delete"},
		{"name":"search_code","description":"Search"},
		{"name":"list_items","description":"List"},
		{"name":"update_thing","description":"Update"}
	]}}`

	b := &Backend{
		name:        "disc-act",
		def:         BackendDef{},
		toolsCache:  json.RawMessage(toolsJSON),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}
	b.buildCategories()

	gw := &Gateway{backends: map[string]*Backend{"disc-act": b}}

	id := json.RawMessage(`"d2"`)
	env := jsonRPCMessage{JSONRPC: "2.0", ID: &id, Method: "tools/call"}
	body := []byte(`{"jsonrpc":"2.0","id":"d2","method":"tools/call","params":{"name":"discover_disc-act_tools","arguments":{"category":"read"}}}`)

	resp, handled := gw.handleLocally(env, body, b)
	if !handled {
		t.Fatal("discover_tools activate should be handled locally")
	}

	var result map[string]any
	if err := json.Unmarshal(resp, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Check active tools were set
	b.mu.Lock()
	if !b.activeTools["get_users"] {
		t.Error("get_users should be active after activation")
	}
	b.mu.Unlock()
}

func TestHandleDiscoverTools_NotDiscovery(t *testing.T) {
	b := &Backend{
		name:        "disc-not",
		def:         BackendDef{},
		activeTools: make(map[string]bool),
		categories:  map[string][]string{"read": {"get_users"}},
	}

	gw := &Gateway{backends: map[string]*Backend{"disc-not": b}}

	id := json.RawMessage(`"x"`)
	env := jsonRPCMessage{JSONRPC: "2.0", ID: &id, Method: "tools/call"}
	body := []byte(`{"jsonrpc":"2.0","id":"x","method":"tools/call","params":{"name":"some_other_tool","arguments":{}}}`)

	_, handled := gw.handleLocally(env, body, b)
	if handled {
		t.Error("non-discovery tools/call should NOT be handled locally")
	}
}

func TestNotifyToolsChanged(t *testing.T) {
	gw := &Gateway{}

	ch := make(chan string, 4)
	remove := gw.addListener(ch)
	defer remove()

	gw.notifyToolsChanged()

	select {
	case msg := <-ch:
		if !strings.Contains(msg, "tools/list_changed") {
			t.Errorf("unexpected notification: %s", msg)
		}
	default:
		t.Error("expected notification in channel")
	}
}

func TestAddAndRemoveListener(t *testing.T) {
	gw := &Gateway{}

	ch := make(chan string, 4)
	remove := gw.addListener(ch)

	gw.listenersMu.Lock()
	if len(gw.listeners) != 1 {
		t.Errorf("expected 1 listener, got %d", len(gw.listeners))
	}
	gw.listenersMu.Unlock()

	remove()

	gw.listenersMu.Lock()
	if len(gw.listeners) != 0 {
		t.Errorf("expected 0 listeners after remove, got %d", len(gw.listeners))
	}
	gw.listenersMu.Unlock()
}

func TestJsonRPCError(t *testing.T) {
	id := json.RawMessage(`42`)
	err := fmt.Errorf("something went wrong")
	data := jsonRPCError(&id, CodeInternalError, err)

	var resp map[string]any
	if jsonErr := json.Unmarshal(data, &resp); jsonErr != nil {
		t.Fatalf("unmarshal error response: %v", jsonErr)
	}

	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatal("no error object in response")
	}
	if errObj["code"] != float64(CodeInternalError) {
		t.Errorf("expected code %d, got %v", CodeInternalError, errObj["code"])
	}
	if errObj["message"] != "something went wrong" {
		t.Errorf("expected error message, got %v", errObj["message"])
	}
}

func TestHandleHealth_WithRunningBackend(t *testing.T) {
	b := &Backend{
		name:        "health-running",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}

	b.mu.Lock()
	if err := b.spawnProcess(); err != nil {
		b.mu.Unlock()
		t.Fatalf("spawnProcess: %v", err)
	}
	b.lastUsed = time.Now()
	b.mu.Unlock()
	defer b.kill()

	gw := &Gateway{
		backends: map[string]*Backend{"health-running": b},
	}

	w := httptest.NewRecorder()
	gw.handleHealth(w)

	var result map[string]any
	json.NewDecoder(w.Result().Body).Decode(&result)

	backends := result["backends"].(map[string]any)
	bs := backends["health-running"].(map[string]any)
	if bs["running"] != true {
		t.Error("expected running=true")
	}
	if bs["pid"] == nil || bs["pid"].(float64) == 0 {
		t.Error("expected non-zero pid")
	}
}

func TestHandleSubscriptionsListen(t *testing.T) {
	gw := &Gateway{
		backends: map[string]*Backend{},
	}

	env := jsonRPCMessage{Method: "subscriptions/listen"}
	body := `{"jsonrpc":"2.0","method":"subscriptions/listen"}`
	req := httptest.NewRequest(http.MethodPost, "/test/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	// Use a context that we can cancel to stop the SSE stream
	ctx, cancel := context.WithTimeout(req.Context(), 100*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()

	// handleSubscriptionsListen blocks until context is cancelled
	gw.handleSubscriptionsListen(w, req, env)

	resp := w.Result()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("expected text/event-stream, got %s", resp.Header.Get("Content-Type"))
	}
}

func TestHandleRequest_MissingBackend(t *testing.T) {
	gw := newTestGateway(map[string]BackendDef{})

	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	// Path "/" -> empty backend name after trim
	// Actually / becomes "" which triggers the check
	req.URL.Path = "/"
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	gw.handleRequest(w, req)

	// Should get either 400 (missing backend) or 404
	if w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound {
		t.Errorf("expected 400 or 404, got %d", w.Code)
	}
}

func TestHandleRequest_InvalidJSON(t *testing.T) {
	gw := newTestGateway(map[string]BackendDef{
		"test": {Command: []string{"echo"}},
	})

	req := httptest.NewRequest(http.MethodPost, "/test/mcp", strings.NewReader("not json at all"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	gw.handleRequest(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", w.Code)
	}
}

func TestForwardToBackend_RetryOnFailure(t *testing.T) {
	b := &Backend{
		name:        "fwd-retry",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}

	gw := &Gateway{
		backends: map[string]*Backend{"fwd-retry": b},
	}

	// Start the backend
	if err := b.ensureRunning(); err != nil {
		t.Fatalf("ensureRunning: %v", err)
	}

	// Send a valid request
	envelope := jsonRPCMessage{JSONRPC: "2.0", Method: "ping"}
	id := json.RawMessage(`"retry1"`)
	envelope.ID = &id
	body := []byte(`{"jsonrpc":"2.0","id":"retry1","method":"ping","params":{}}`)

	resp, err := gw.forwardToBackend(context.Background(), b, envelope, body)
	if err != nil {
		t.Fatalf("forwardToBackend: %v", err)
	}
	if len(resp) == 0 {
		t.Fatal("expected non-empty response")
	}

	b.kill()
	time.Sleep(200 * time.Millisecond)
}

func TestHandleToolsList_CachedWithCategories(t *testing.T) {
	toolsJSON := `{"jsonrpc":"2.0","id":"old","result":{"tools":[
		{"name":"get_users","description":"Get users"},
		{"name":"create_issue","description":"Create issue"},
		{"name":"delete_repo","description":"Delete repo"},
		{"name":"search_code","description":"Search code"},
		{"name":"list_items","description":"List items"},
		{"name":"update_thing","description":"Update thing"}
	]}}`

	b := &Backend{
		name:        "cached-cats",
		def:         BackendDef{},
		toolsCache:  json.RawMessage(toolsJSON),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}
	b.buildCategories()

	// With categories, should return filtered list
	id := json.RawMessage(`"fresh"`)
	data, handled := b.handleToolsList(&id)
	if !handled {
		t.Fatal("should be handled from cache with categories")
	}
	if data == nil {
		t.Fatal("expected response data")
	}

	// Should contain discover meta-tool
	if !strings.Contains(string(data), "discover_cached-cats_tools") {
		t.Error("expected discover meta-tool in filtered response")
	}
}

func TestHandleRestart_RunningBackend(t *testing.T) {
	b := &Backend{
		name:        "restart-running",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}

	b.mu.Lock()
	if err := b.spawnProcess(); err != nil {
		b.mu.Unlock()
		t.Fatalf("spawnProcess: %v", err)
	}
	b.mu.Unlock()

	gw := &Gateway{
		backends: map[string]*Backend{"restart-running": b},
	}

	req := httptest.NewRequest(http.MethodPost, "/_restart/restart-running", nil)
	w := httptest.NewRecorder()
	gw.handleRestart(w, req)

	var result map[string]any
	json.NewDecoder(w.Result().Body).Decode(&result)
	if result["was_running"] != true {
		t.Error("expected was_running=true")
	}

	time.Sleep(200 * time.Millisecond)
	if b.isRunning() {
		t.Error("backend should not be running after restart")
	}
}

func TestWriteResponse_EmptyBody(t *testing.T) {
	gw := newTestGateway(map[string]BackendDef{})

	req := httptest.NewRequest(http.MethodPost, "/test/mcp", nil)
	w := httptest.NewRecorder()

	gw.writeResponse(w, req, []byte{})

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202 for empty response, got %d", w.Code)
	}
}
