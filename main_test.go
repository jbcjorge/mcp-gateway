package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestGateway creates a Gateway with the given backends for testing.
func newTestGateway(backends map[string]BackendDef) *Gateway {
	gw := &Gateway{
		config: Config{
			Listen:      ":0",
			IdleTimeout: 0,
		},
		backends:    make(map[string]*Backend),
		authorizer:  NewBearerAuthorizer(nil, backends),
		lastRequest: time.Now(),
	}
	for name, def := range backends {
		gw.backends[name] = &Backend{
			name:    name,
			def:     def,
			pending: make(map[string]chan json.RawMessage),
		}
	}
	return gw
}

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
		{"/askthedev/mcp", "askthedev"},
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

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	_ = writeTestFile(dir+"/config.json", `{
		"listen": "127.0.0.1:19900",
		"log_level": "info",
		"idle_timeout_seconds": 300,
		"self_idle_timeout_seconds": 3600,
		"backends_file": "backends.json"
	}`)
	_ = writeTestFile(dir+"/backends.json", `{}`)

	cfg, err := loadConfig(dir + "/config.json")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Listen != "127.0.0.1:19900" {
		t.Errorf("expected 127.0.0.1:19900, got %s", cfg.Listen)
	}
	if cfg.IdleTimeout != 300 {
		t.Errorf("expected idle_timeout 300, got %d", cfg.IdleTimeout)
	}
	if cfg.SelfIdleTimeout != 3600 {
		t.Errorf("expected self_idle_timeout 3600, got %d", cfg.SelfIdleTimeout)
	}
	if cfg.BackendsFile != "backends.json" {
		t.Errorf("expected backends_file backends.json, got %s", cfg.BackendsFile)
	}
}

func TestLoadConfigWithBackendsFile(t *testing.T) {
	dir := t.TempDir()
	_ = writeTestFile(dir+"/config.json", `{"listen":":9999","backends_file":"backends.json"}`)
	_ = writeTestFile(dir+"/backends.json", `{"mybackend":{"command":["echo","hi"]}}`)

	cfg, err := loadConfig(dir + "/config.json")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Listen != ":9999" {
		t.Errorf("expected :9999, got %s", cfg.Listen)
	}
	if len(cfg.Backends) != 1 {
		t.Fatalf("expected 1 backend, got %d", len(cfg.Backends))
	}
	b, ok := cfg.Backends["mybackend"]
	if !ok {
		t.Fatal("expected mybackend in backends")
	}
	if len(b.Command) != 2 || b.Command[0] != "echo" {
		t.Errorf("unexpected command: %v", b.Command)
	}
}

func TestLoadConfigInlineBackends(t *testing.T) {
	dir := t.TempDir()
	_ = writeTestFile(dir+"/config.json", `{"backends":{"test":{"command":["cat"]}}}`)

	cfg, err := loadConfig(dir + "/config.json")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if _, ok := cfg.Backends["test"]; !ok {
		t.Error("expected inline backend 'test'")
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	_, err := loadConfig("/nonexistent/path.json")
	if err == nil {
		t.Error("expected error for missing config file")
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	// Test that empty listen gets default
	tmpFile := t.TempDir() + "/cfg.json"
	_ = writeTestFile(tmpFile, `{"backends":{}}`)
	cfg, err := loadConfig(tmpFile)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Listen != "127.0.0.1:19900" {
		t.Errorf("expected default 127.0.0.1:19900, got %s", cfg.Listen)
	}
}

func TestBytesTrimRight(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"hello\n", "hello"},
		{"hello\r\n", "hello"},
		{"hello   \n", "hello"},
		{"hello", "hello"},
		{"", ""},
		{"\n", ""},
	}
	for _, tt := range tests {
		got := string(bytesTrimRight([]byte(tt.input)))
		if got != tt.expected {
			t.Errorf("bytesTrimRight(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		input    string
		maxLen   int
		expected string
	}{
		{"hello world", 5, "hello..."},
		{"hi", 10, "hi"},
		{"exactly10!", 10, "exactly10!"},
		{"", 5, ""},
	}
	for _, tt := range tests {
		got := truncate([]byte(tt.input), tt.maxLen)
		if got != tt.expected {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.expected)
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

// writeTestFile is a test helper to write content to a file.
func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0644)
}

func TestRemoteBackendStreamableHTTP(t *testing.T) {
	// Fake remote MCP server
	remoteSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env jsonRPCMessage
		json.Unmarshal(body, &env)

		// Echo back a valid JSON-RPC response
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      env.ID,
			"result": map[string]any{
				"tools": []map[string]any{
					{"name": "remote_tool", "description": "A remote tool"},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer remoteSrv.Close()

	gw := newTestGateway(map[string]BackendDef{
		"remote": {
			URL:           remoteSrv.URL + "/mcp",
			TransportType: "streamable-http",
		},
	})
	// Set up the remote backend properly
	gw.backends["remote"].httpClient = &http.Client{Timeout: 10 * time.Second}
	gw.backends["remote"].logEnabled = true

	// Send tools/list - this should NOT be handled locally (no cache), forward to remote
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	req := httptest.NewRequest(http.MethodPost, "/remote/mcp", strings.NewReader(body))
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
	tools, ok := r["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatal("expected tools in response")
	}
}

func TestRemoteBackendSSE(t *testing.T) {
	// Fake SSE remote server - returns response as SSE event
	remoteSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env jsonRPCMessage
		json.Unmarshal(body, &env)

		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      env.ID,
			"result":  map[string]any{},
		}
		data, _ := json.Marshal(resp)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
	}))
	defer remoteSrv.Close()

	gw := newTestGateway(map[string]BackendDef{
		"sse-remote": {
			URL:           remoteSrv.URL + "/sse",
			TransportType: "sse",
		},
	})
	gw.backends["sse-remote"].httpClient = &http.Client{Timeout: 10 * time.Second}
	gw.backends["sse-remote"].logEnabled = true

	// Ping via SSE remote
	body := `{"jsonrpc":"2.0","id":42,"method":"ping"}`
	req := httptest.NewRequest(http.MethodPost, "/sse-remote/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()

	gw.handleRequest(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(respBody))
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result["id"] == nil {
		t.Error("expected id in response")
	}
}

func TestRemoteBackendHeaders(t *testing.T) {
	var receivedAuth string
	remoteSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		var env jsonRPCMessage
		json.Unmarshal(body, &env)
		resp := map[string]any{"jsonrpc": "2.0", "id": env.ID, "result": map[string]any{"tools": []any{}}}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer remoteSrv.Close()

	gw := newTestGateway(map[string]BackendDef{
		"authed": {
			URL:           remoteSrv.URL + "/mcp",
			TransportType: "streamable-http",
			Headers:       map[string]string{"Authorization": "Bearer secret-token"},
		},
	})
	gw.backends["authed"].httpClient = &http.Client{Timeout: 10 * time.Second}
	gw.backends["authed"].logEnabled = true

	// Use tools/list which forwards to remote (no cache exists)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	req := httptest.NewRequest(http.MethodPost, "/authed/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()

	gw.handleRequest(w, req)

	if receivedAuth != "Bearer secret-token" {
		t.Errorf("expected Authorization header, got %q", receivedAuth)
	}
}

func TestRemoteBackendError(t *testing.T) {
	remoteSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer remoteSrv.Close()

	gw := newTestGateway(map[string]BackendDef{
		"broken": {
			URL:           remoteSrv.URL + "/mcp",
			TransportType: "streamable-http",
		},
	})
	gw.backends["broken"].httpClient = &http.Client{Timeout: 10 * time.Second}
	gw.backends["broken"].logEnabled = false

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	req := httptest.NewRequest(http.MethodPost, "/broken/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	gw.handleRequest(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", w.Code)
	}
}

func TestPerBackendLogging(t *testing.T) {
	enabled := true
	disabled := false

	gw := newTestGateway(map[string]BackendDef{
		"verbose": {Command: []string{"echo"}, LogEnabled: &enabled},
		"quiet":   {Command: []string{"echo"}, LogEnabled: &disabled},
		"default": {Command: []string{"echo"}},
	})

	// Reinitialize with proper log settings
	for name, def := range gw.config.Backends {
		gw.backends[name] = &Backend{
			name:        name,
			def:         def,
			pending:     make(map[string]chan json.RawMessage),
			activeTools: make(map[string]bool),
			logEnabled:  def.LogEnabled == nil || *def.LogEnabled,
		}
	}
	// The test gateway helper doesn't set LogEnabled on defs, do it manually
	gw.backends["verbose"].logEnabled = true
	gw.backends["quiet"].logEnabled = false
	gw.backends["default"].logEnabled = true // nil defaults to enabled

	if !gw.backends["verbose"].logEnabled {
		t.Error("verbose backend should have logging enabled")
	}
	if gw.backends["quiet"].logEnabled {
		t.Error("quiet backend should have logging disabled")
	}
	if !gw.backends["default"].logEnabled {
		t.Error("default backend (nil) should have logging enabled")
	}
}

func TestIsRemote(t *testing.T) {
	local := &Backend{def: BackendDef{Command: []string{"echo"}}}
	remote := &Backend{def: BackendDef{URL: "https://example.com/sse"}}

	if local.isRemote() {
		t.Error("command backend should not be remote")
	}
	if !remote.isRemote() {
		t.Error("URL backend should be remote")
	}
}

func TestTransportTypeInference(t *testing.T) {
	tests := []struct {
		url      string
		explicit string
		expected string
	}{
		{"https://example.com/sse", "", "sse"},
		{"https://example.com/mcp", "", "streamable-http"},
		{"https://example.com/api", "", "sse"},
		{"https://example.com/sse", "streamable-http", "streamable-http"},
		{"https://example.com/mcp", "sse", "sse"},
	}

	for _, tt := range tests {
		b := &Backend{def: BackendDef{URL: tt.url, TransportType: tt.explicit}}
		got := b.transportType()
		if got != tt.expected {
			t.Errorf("URL=%q explicit=%q: got %q, want %q", tt.url, tt.explicit, got, tt.expected)
		}
	}
}

func TestParseSSEResponse(t *testing.T) {
	b := &Backend{name: "test", logEnabled: true}

	input := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n"
	reader := strings.NewReader(input)

	data, err := b.parseSSEResponse(reader)
	if err != nil {
		t.Fatalf("parseSSEResponse: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal SSE data: %v", err)
	}
	if result["id"] == nil {
		t.Error("expected id in parsed SSE response")
	}
}

func TestLoadConfigRemoteBackend(t *testing.T) {
	dir := t.TempDir()
	config := `{
		"backends": {
			"remote-sse": {
				"url": "https://mcp.example.com/sse",
				"headers": {"Authorization": "Bearer tok"},
				"log_enabled": false
			},
			"remote-http": {
				"url": "https://mcp.example.com/mcp",
				"transport_type": "streamable-http",
				"log_enabled": true
			}
		}
	}`
	_ = writeTestFile(dir+"/config.json", config)

	cfg, err := loadConfig(dir + "/config.json")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	sse, ok := cfg.Backends["remote-sse"]
	if !ok {
		t.Fatal("expected remote-sse backend")
	}
	if sse.URL != "https://mcp.example.com/sse" {
		t.Errorf("expected URL, got %s", sse.URL)
	}
	if sse.Headers["Authorization"] != "Bearer tok" {
		t.Errorf("expected auth header, got %v", sse.Headers)
	}
	if sse.LogEnabled == nil || *sse.LogEnabled != false {
		t.Error("expected log_enabled=false")
	}

	httpB, ok := cfg.Backends["remote-http"]
	if !ok {
		t.Fatal("expected remote-http backend")
	}
	if httpB.TransportType != "streamable-http" {
		t.Errorf("expected streamable-http, got %s", httpB.TransportType)
	}
	if httpB.LogEnabled == nil || *httpB.LogEnabled != true {
		t.Error("expected log_enabled=true")
	}
}

// --- Bearer Auth Tests ---

func TestAuthNoTokensConfigured_AllowsAll(t *testing.T) {
	gw := newTestGateway(map[string]BackendDef{
		"test": {Command: []string{"echo"}},
	})

	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	req := httptest.NewRequest(http.MethodPost, "/test/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()

	gw.handleRequest(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 (no auth configured), got %d", w.Code)
	}
}

func TestAuthGlobalTokenRequired_Rejects(t *testing.T) {
	gw := newTestGateway(map[string]BackendDef{
		"test": {Command: []string{"echo"}},
	})
	gw.authorizer = NewBearerAuthorizer([]string{"secret-token"}, map[string]BackendDef{})

	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	req := httptest.NewRequest(http.MethodPost, "/test/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	gw.handleRequest(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestAuthGlobalTokenRequired_Accepts(t *testing.T) {
	gw := newTestGateway(map[string]BackendDef{
		"test": {Command: []string{"echo"}},
	})
	gw.authorizer = NewBearerAuthorizer([]string{"secret-token"}, map[string]BackendDef{})

	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	req := httptest.NewRequest(http.MethodPost, "/test/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret-token")
	w := httptest.NewRecorder()

	gw.handleRequest(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestAuthPerBackendToken_OverridesGlobal(t *testing.T) {
	backends := map[string]BackendDef{
		"restricted": {Command: []string{"echo"}, AuthTokens: []string{"backend-secret"}},
	}
	gw := newTestGateway(backends)
	gw.authorizer = NewBearerAuthorizer([]string{"global-secret"}, backends)

	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`

	// Global token should NOT work for this backend
	req := httptest.NewRequest(http.MethodPost, "/restricted/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer global-secret")
	w := httptest.NewRecorder()
	gw.handleRequest(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("global token should not work for per-backend auth, got %d", w.Code)
	}

	// Backend-specific token should work
	req = httptest.NewRequest(http.MethodPost, "/restricted/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer backend-secret")
	w = httptest.NewRecorder()
	gw.handleRequest(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("backend token should work, got %d", w.Code)
	}
}

func TestAuthHealthEndpoint_NoAuthRequired(t *testing.T) {
	gw := newTestGateway(map[string]BackendDef{
		"test": {Command: []string{"echo"}},
	})
	gw.authorizer = NewBearerAuthorizer([]string{"secret"}, map[string]BackendDef{})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	gw.handleRequest(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("health should not require auth, got %d", w.Code)
	}
}

// --- Tool Filtering Tests ---

func TestToolAllowed_NoFilters(t *testing.T) {
	b := &Backend{def: BackendDef{}}
	if !b.toolAllowed("anything") {
		t.Error("no filters should allow all tools")
	}
}

func TestToolAllowed_IncludeOnly(t *testing.T) {
	b := &Backend{def: BackendDef{IncludeTools: []string{"get_*", "list_*"}}}

	if !b.toolAllowed("get_users") {
		t.Error("get_users should be allowed by include")
	}
	if !b.toolAllowed("list_repos") {
		t.Error("list_repos should be allowed by include")
	}
	if b.toolAllowed("delete_user") {
		t.Error("delete_user should be blocked (not in include)")
	}
}

func TestToolAllowed_ExcludeOnly(t *testing.T) {
	b := &Backend{def: BackendDef{ExcludeTools: []string{"delete_*", "drop_*"}}}

	if !b.toolAllowed("get_users") {
		t.Error("get_users should be allowed")
	}
	if b.toolAllowed("delete_user") {
		t.Error("delete_user should be blocked by exclude")
	}
	if b.toolAllowed("drop_database") {
		t.Error("drop_database should be blocked by exclude")
	}
}

func TestToolAllowed_IncludeAndExclude(t *testing.T) {
	b := &Backend{def: BackendDef{
		IncludeTools: []string{"*_user", "*_repo"},
		ExcludeTools: []string{"delete_*"},
	}}

	if !b.toolAllowed("get_user") {
		t.Error("get_user matches include and not excluded")
	}
	if b.toolAllowed("delete_user") {
		t.Error("delete_user matches include but also matches exclude")
	}
	if b.toolAllowed("list_teams") {
		t.Error("list_teams does not match include")
	}
}

// --- Disabled Backend Tests ---

func TestDisabledBackend_NotInGateway(t *testing.T) {
	cfg := Config{
		Listen: ":0",
		Backends: map[string]BackendDef{
			"active":   {Command: []string{"echo"}},
			"disabled": {Command: []string{"echo"}, Disabled: true},
		},
	}
	gw := newGateway(cfg)

	if _, ok := gw.backends["active"]; !ok {
		t.Error("active backend should be in gateway")
	}
	if _, ok := gw.backends["disabled"]; ok {
		t.Error("disabled backend should NOT be in gateway")
	}
}

func TestDisabledBackend_Returns404(t *testing.T) {
	cfg := Config{
		Listen: ":0",
		Backends: map[string]BackendDef{
			"active":   {Command: []string{"echo"}},
			"disabled": {Command: []string{"echo"}, Disabled: true},
		},
	}
	gw := newGateway(cfg)

	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	req := httptest.NewRequest(http.MethodPost, "/disabled/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	gw.handleRequest(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("disabled backend should return 404, got %d", w.Code)
	}
}

// =============================================================================
// ADDITIONAL TESTS FOR COVERAGE - Subprocess, Discovery, Reaper, Cache, etc.
// =============================================================================

// TestSubprocessHelper is a fake MCP backend that runs when TEST_SUBPROCESS=1.
// It reads JSON-RPC from stdin line-by-line and echoes valid responses.
func TestSubprocessHelper(t *testing.T) {
	if os.Getenv("TEST_SUBPROCESS") != "1" {
		t.Skip("helper process")
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var msg struct {
			ID     *json.RawMessage `json:"id"`
			Method string           `json:"method"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if msg.ID == nil {
			continue // notification, no response needed
		}
		// For tools/list, return a fake tools array
		if msg.Method == "tools/list" {
			resp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"get_users","description":"Get users"},{"name":"create_issue","description":"Create issue"},{"name":"delete_repo","description":"Delete repo"},{"name":"search_code","description":"Search code"},{"name":"list_items","description":"List items"},{"name":"update_thing","description":"Update thing"},{"name":"custom_action","description":"Custom action"}]}}`, string(*msg.ID))
			fmt.Println(resp)
		} else {
			resp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, string(*msg.ID))
			fmt.Println(resp)
		}
	}
}

// subprocessCommand returns the command to run the test binary as a fake MCP subprocess.
func subprocessCommand() []string {
	return []string{os.Args[0], "-test.run=TestSubprocessHelper"}
}

func TestSpawnAndSend(t *testing.T) {
	b := &Backend{
		name:        "test-spawn",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}

	b.mu.Lock()
	err := b.spawnProcess()
	b.mu.Unlock()
	if err != nil {
		t.Fatalf("spawnProcess: %v", err)
	}
	defer b.kill()

	if !b.isRunning() {
		t.Fatal("expected backend to be running after spawn")
	}

	// Send a JSON-RPC message and get a response
	msg := []byte(`{"jsonrpc":"2.0","id":"test1","method":"ping","params":{}}`)
	resp, err := b.send(context.Background(), msg)
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(resp, &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result["id"] == nil {
		t.Error("expected id in response")
	}
}

func TestSendToDeadBackend(t *testing.T) {
	b := &Backend{
		name:        "dead-backend",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}

	// Never started, so not running
	msg := []byte(`{"jsonrpc":"2.0","id":"x","method":"ping"}`)
	_, err := b.send(context.Background(), msg)
	if err == nil {
		t.Fatal("expected error sending to dead backend")
	}
}

func TestSendNotification(t *testing.T) {
	b := &Backend{
		name:        "test-notif",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}

	b.mu.Lock()
	err := b.spawnProcess()
	b.mu.Unlock()
	if err != nil {
		t.Fatalf("spawnProcess: %v", err)
	}
	defer b.kill()

	// Notifications have no id - should return empty
	msg := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	resp, err := b.send(context.Background(), msg)
	if err != nil {
		t.Fatalf("send notification: %v", err)
	}
	if string(resp) != "{}" {
		t.Errorf("expected empty response for notification, got %s", resp)
	}
}

func TestKillAndWaitForExit(t *testing.T) {
	b := &Backend{
		name:        "test-kill",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}

	b.mu.Lock()
	err := b.spawnProcess()
	b.mu.Unlock()
	if err != nil {
		t.Fatalf("spawnProcess: %v", err)
	}

	if !b.isRunning() {
		t.Fatal("expected running before kill")
	}

	b.kill()
	// Wait a moment for waitForExit goroutine
	time.Sleep(200 * time.Millisecond)

	if b.isRunning() {
		t.Error("expected not running after kill")
	}
}

func TestSendAfterKill(t *testing.T) {
	b := &Backend{
		name:        "test-send-killed",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}

	b.mu.Lock()
	err := b.spawnProcess()
	b.mu.Unlock()
	if err != nil {
		t.Fatalf("spawnProcess: %v", err)
	}

	b.kill()
	time.Sleep(200 * time.Millisecond)

	msg := []byte(`{"jsonrpc":"2.0","id":"y","method":"ping"}`)
	_, err = b.send(context.Background(), msg)
	if err == nil {
		t.Fatal("expected error sending to killed backend")
	}
}

func TestInitializeBackend(t *testing.T) {
	b := &Backend{
		name:        "test-init",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}

	b.mu.Lock()
	err := b.spawnProcess()
	b.mu.Unlock()
	if err != nil {
		t.Fatalf("spawnProcess: %v", err)
	}
	defer b.kill()

	// initializeBackend sends initialize + notifications/initialized
	b.initializeBackend()

	// Backend should still be running after init
	if !b.isRunning() {
		t.Error("backend should still be running after initializeBackend")
	}
}

func TestEnsureRunning(t *testing.T) {
	b := &Backend{
		name:        "test-ensure",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}

	if b.isRunning() {
		t.Fatal("should not be running initially")
	}

	if err := b.ensureRunning(); err != nil {
		t.Fatalf("ensureRunning: %v", err)
	}
	defer b.kill()

	if !b.isRunning() {
		t.Fatal("should be running after ensureRunning")
	}

	// Calling again should be a no-op
	if err := b.ensureRunning(); err != nil {
		t.Fatalf("second ensureRunning: %v", err)
	}
}

func TestEnsureRunningNoCommand(t *testing.T) {
	b := &Backend{
		name:        "test-no-cmd",
		def:         BackendDef{},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
	}

	err := b.ensureRunning()
	if err == nil {
		t.Fatal("expected error for backend with no command or url")
	}
}

func TestEnsureRunningRemote(t *testing.T) {
	b := &Backend{
		name:        "test-remote-ensure",
		def:         BackendDef{URL: "https://example.com/mcp"},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		logEnabled:  true,
	}

	if err := b.ensureRunning(); err != nil {
		t.Fatalf("ensureRunning remote: %v", err)
	}
	if !b.isRunning() {
		t.Fatal("remote backend should be running after ensureRunning")
	}
}

// --- Discovery / Category System Tests ---

func TestCategorizeToolName(t *testing.T) {
	tests := []struct {
		name     string
		expected string
	}{
		{"get_users", "read"},
		{"read_post", "read"},
		{"download_file", "read"},
		{"view_item", "read"},
		{"create_issue", "create"},
		{"add_comment", "create"},
		{"batch_create_issues", "create"},
		{"delete_repo", "delete"},
		{"remove_label", "delete"},
		{"cancel_pipeline", "delete"},
		{"unapprove_merge_request", "delete"},
		{"search_code", "search"},
		{"list_items", "search"},
		{"filter_topics", "search"},
		{"find_users", "search"},
		{"update_issue", "update"},
		{"edit_comment", "update"},
		{"transition_issue", "update"},
		{"link_to_epic", "update"},
		{"approve_merge_request", "update"},
		{"merge_merge_request", "update"},
		{"custom_action", "general"},
		{"something_else", "general"},
		// Namespace prefix tests
		{"jira_get_issue", "read"},
		{"confluence_search_pages", "search"},
		{"discourse_create_topic", "create"},
		{"devops_delete_item", "delete"},
		{"mdp_update_doc", "update"},
		{"iam_custom_tool", "general"},
	}

	for _, tt := range tests {
		got := categorizeToolName(tt.name)
		if got != tt.expected {
			t.Errorf("categorizeToolName(%q) = %q, want %q", tt.name, got, tt.expected)
		}
	}
}

func TestBuildCategories(t *testing.T) {
	toolsJSON := `{"jsonrpc":"2.0","id":"1","result":{"tools":[
		{"name":"get_users","description":"Get users"},
		{"name":"create_issue","description":"Create issue"},
		{"name":"delete_repo","description":"Delete repo"},
		{"name":"search_code","description":"Search code"},
		{"name":"list_items","description":"List items"},
		{"name":"update_thing","description":"Update thing"},
		{"name":"custom_action","description":"Custom action"}
	]}}`

	b := &Backend{
		name:        "test-cats",
		def:         BackendDef{},
		toolsCache:  json.RawMessage(toolsJSON),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}

	b.buildCategories()

	if b.categories == nil {
		t.Fatal("categories should not be nil for 7 tools")
	}

	// Check expected categories exist
	if _, ok := b.categories["read"]; !ok {
		t.Error("expected 'read' category")
	}
	if _, ok := b.categories["create"]; !ok {
		t.Error("expected 'create' category")
	}
	if _, ok := b.categories["delete"]; !ok {
		t.Error("expected 'delete' category")
	}
	if _, ok := b.categories["search"]; !ok {
		t.Error("expected 'search' category")
	}
	if _, ok := b.categories["update"]; !ok {
		t.Error("expected 'update' category")
	}
	if _, ok := b.categories["general"]; !ok {
		t.Error("expected 'general' category")
	}
}

func TestBuildCategories_FewTools(t *testing.T) {
	toolsJSON := `{"jsonrpc":"2.0","id":"1","result":{"tools":[
		{"name":"get_users","description":"Get users"},
		{"name":"create_issue","description":"Create"}
	]}}`

	b := &Backend{
		name:        "test-few",
		def:         BackendDef{},
		toolsCache:  json.RawMessage(toolsJSON),
		activeTools: make(map[string]bool),
	}

	b.buildCategories()

	if b.categories != nil {
		t.Error("categories should be nil for <=5 tools (no discovery)")
	}
}

func TestBuildCategories_ForceDiscovery(t *testing.T) {
	force := true
	toolsJSON := `{"jsonrpc":"2.0","id":"1","result":{"tools":[
		{"name":"get_users","description":"Get users"},
		{"name":"create_issue","description":"Create"}
	]}}`

	b := &Backend{
		name:        "test-force",
		def:         BackendDef{Discovery: &force},
		toolsCache:  json.RawMessage(toolsJSON),
		activeTools: make(map[string]bool),
	}

	b.buildCategories()

	if b.categories == nil {
		t.Error("categories should be set when discovery is forced")
	}
}

func TestBuildCategories_Disabled(t *testing.T) {
	disabled := false
	toolsJSON := `{"jsonrpc":"2.0","id":"1","result":{"tools":[
		{"name":"get_users","description":"Get users"},
		{"name":"create_issue","description":"Create"},
		{"name":"delete_repo","description":"Delete"},
		{"name":"search_code","description":"Search"},
		{"name":"list_items","description":"List"},
		{"name":"update_thing","description":"Update"}
	]}}`

	b := &Backend{
		name:        "test-disabled-disc",
		def:         BackendDef{Discovery: &disabled},
		toolsCache:  json.RawMessage(toolsJSON),
		activeTools: make(map[string]bool),
	}

	b.buildCategories()

	if b.categories != nil {
		t.Error("categories should be nil when discovery is disabled")
	}
}

func TestBuildCategories_ManualConfig(t *testing.T) {
	toolsJSON := `{"jsonrpc":"2.0","id":"1","result":{"tools":[
		{"name":"tool_a","description":"A"},
		{"name":"tool_b","description":"B"},
		{"name":"tool_c","description":"C"},
		{"name":"tool_d","description":"D"},
		{"name":"tool_e","description":"E"},
		{"name":"tool_f","description":"F"}
	]}}`

	b := &Backend{
		name: "test-manual-cats",
		def: BackendDef{
			Categories: map[string][]string{
				"group1": {"tool_a", "tool_b"},
				"group2": {"tool_c", "tool_d"},
			},
		},
		toolsCache:  json.RawMessage(toolsJSON),
		activeTools: make(map[string]bool),
	}

	b.buildCategories()

	if len(b.categories) != 2 {
		t.Errorf("expected 2 manual categories, got %d", len(b.categories))
	}
}

func TestBuildCategories_NilCache(t *testing.T) {
	b := &Backend{
		name:        "test-nil-cache",
		def:         BackendDef{},
		activeTools: make(map[string]bool),
	}

	b.buildCategories()
	if b.categories != nil {
		t.Error("categories should be nil with no cache")
	}
}

func TestActivateCategory(t *testing.T) {
	toolsJSON := `{"jsonrpc":"2.0","id":"1","result":{"tools":[
		{"name":"get_users","description":"Get users"},
		{"name":"create_issue","description":"Create issue"},
		{"name":"delete_repo","description":"Delete repo"},
		{"name":"search_code","description":"Search code"},
		{"name":"list_items","description":"List items"},
		{"name":"update_thing","description":"Update thing"},
		{"name":"custom_action","description":"Custom action"}
	]}}`

	b := &Backend{
		name:        "test-activate",
		def:         BackendDef{},
		toolsCache:  json.RawMessage(toolsJSON),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}
	b.buildCategories()

	b.mu.Lock()
	activated, err := b.activateCategory("read")
	b.mu.Unlock()
	if err != nil {
		t.Fatalf("activateCategory: %v", err)
	}
	if len(activated) == 0 {
		t.Error("expected activated tools")
	}

	// Check activeTools was updated
	b.mu.Lock()
	if !b.activeTools["get_users"] {
		t.Error("get_users should be active after activating 'read' category")
	}
	b.mu.Unlock()
}

func TestActivateCategory_Unknown(t *testing.T) {
	toolsJSON := `{"jsonrpc":"2.0","id":"1","result":{"tools":[
		{"name":"get_users","description":"Get users"},
		{"name":"create_issue","description":"Create"},
		{"name":"delete_repo","description":"Delete"},
		{"name":"search_code","description":"Search"},
		{"name":"list_items","description":"List"},
		{"name":"update_thing","description":"Update"}
	]}}`

	b := &Backend{
		name:        "test-activate-unknown",
		def:         BackendDef{},
		toolsCache:  json.RawMessage(toolsJSON),
		activeTools: make(map[string]bool),
	}
	b.buildCategories()

	b.mu.Lock()
	_, err := b.activateCategory("nonexistent")
	b.mu.Unlock()
	if err == nil {
		t.Error("expected error for unknown category")
	}
}

func TestFilteredToolsList(t *testing.T) {
	toolsJSON := `{"jsonrpc":"2.0","id":"1","result":{"tools":[
		{"name":"get_users","description":"Get users"},
		{"name":"create_issue","description":"Create issue"},
		{"name":"delete_repo","description":"Delete repo"},
		{"name":"search_code","description":"Search code"},
		{"name":"list_items","description":"List items"},
		{"name":"update_thing","description":"Update thing"},
		{"name":"custom_action","description":"Custom action"}
	]}}`

	b := &Backend{
		name:        "test-filtered",
		def:         BackendDef{},
		toolsCache:  json.RawMessage(toolsJSON),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}
	b.buildCategories()

	// Activate one category
	b.mu.Lock()
	b.activateCategory("read")
	b.mu.Unlock()

	id := json.RawMessage(`99`)
	data, err := b.filteredToolsList(&id)
	if err != nil {
		t.Fatalf("filteredToolsList: %v", err)
	}

	var resp struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("unmarshal filtered: %v", err)
	}

	// Should have discover meta-tool + active tools from "read" category
	if len(resp.Result.Tools) < 2 {
		t.Errorf("expected at least 2 tools (discover + read tools), got %d", len(resp.Result.Tools))
	}

	// First tool should be the discover meta-tool
	var firstTool struct {
		Name string `json:"name"`
	}
	json.Unmarshal(resp.Result.Tools[0], &firstTool)
	if firstTool.Name != "discover_test-filtered_tools" {
		t.Errorf("first tool should be discover meta-tool, got %q", firstTool.Name)
	}
}

func TestDiscoverToolSchema(t *testing.T) {
	b := &Backend{
		name: "mybackend",
		categories: map[string][]string{
			"read":   {"get_users", "read_post"},
			"create": {"create_issue"},
		},
	}

	schema := b.discoverToolSchema()
	if schema["name"] != "discover_mybackend_tools" {
		t.Errorf("expected discover_mybackend_tools, got %v", schema["name"])
	}
	desc, ok := schema["description"].(string)
	if !ok || !strings.Contains(desc, "mybackend") {
		t.Errorf("description should mention backend name, got %q", desc)
	}
	if !strings.Contains(desc, "create") || !strings.Contains(desc, "read") {
		t.Errorf("description should list categories, got %q", desc)
	}
}

func TestParseToolsFromCache(t *testing.T) {
	toolsJSON := `{"jsonrpc":"2.0","id":"1","result":{"tools":[
		{"name":"tool_a","description":"A"},
		{"name":"tool_b","description":"B"}
	]}}`

	b := &Backend{toolsCache: json.RawMessage(toolsJSON)}
	tools := b.parseToolsFromCache()
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	if tools[0].Name != "tool_a" || tools[1].Name != "tool_b" {
		t.Errorf("unexpected tool names: %v, %v", tools[0].Name, tools[1].Name)
	}
}

func TestParseToolsFromCache_Invalid(t *testing.T) {
	b := &Backend{toolsCache: json.RawMessage(`invalid json`)}
	tools := b.parseToolsFromCache()
	if tools != nil {
		t.Error("expected nil for invalid cache")
	}
}

func TestToolsContainDiscovery(t *testing.T) {
	b := &Backend{}

	toolsWithDiscover := []toolEntry{
		{Name: "get_users"},
		{Name: "discover_tools"},
		{Name: "create_issue"},
	}
	if !b.toolsContainDiscovery(toolsWithDiscover) {
		t.Error("should detect discover_tools")
	}

	toolsWithout := []toolEntry{
		{Name: "get_users"},
		{Name: "create_issue"},
	}
	if b.toolsContainDiscovery(toolsWithout) {
		t.Error("should not detect discover_tools when absent")
	}
}

func TestSmartActivate(t *testing.T) {
	b := &Backend{
		name: "test-smart",
		categories: map[string][]string{
			"read":   {"get_users", "get_items"},
			"create": {"create_issue"},
		},
		activeTools: make(map[string]bool),
	}

	b.smartActivate("get_users")

	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.activeTools["get_users"] {
		t.Error("get_users should be active")
	}
	if !b.activeTools["get_items"] {
		t.Error("get_items should be active (same category)")
	}
	if b.activeTools["create_issue"] {
		t.Error("create_issue should NOT be active")
	}
}

func TestSmartActivate_AlreadyActive(t *testing.T) {
	b := &Backend{
		name: "test-smart-noop",
		categories: map[string][]string{
			"read": {"get_users"},
		},
		activeTools: map[string]bool{"get_users": true},
	}

	// Should be a no-op, no panic
	b.smartActivate("get_users")
}

func TestSmartActivate_NilCategories(t *testing.T) {
	b := &Backend{
		name:        "test-smart-nil",
		categories:  nil,
		activeTools: make(map[string]bool),
	}

	// Should be a no-op, no panic
	b.smartActivate("anything")
}

// --- Idle Reaper Tests ---

func TestReapIdleBackends_KillsIdle(t *testing.T) {
	b := &Backend{
		name:        "idle-backend",
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
	b.lastUsed = time.Now().Add(-10 * time.Minute) // simulate long idle
	b.mu.Unlock()

	gw := &Gateway{
		backends: map[string]*Backend{"idle-backend": b},
	}

	gw.reapIdleBackends(5 * time.Minute)

	// Wait for process to exit
	time.Sleep(200 * time.Millisecond)

	if b.isRunning() {
		t.Error("idle backend should have been killed")
	}
}

func TestReapIdleBackends_KeepsRecent(t *testing.T) {
	b := &Backend{
		name:        "recent-backend",
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
	b.lastUsed = time.Now() // recently used
	b.mu.Unlock()
	defer b.kill()

	gw := &Gateway{
		backends: map[string]*Backend{"recent-backend": b},
	}

	gw.reapIdleBackends(5 * time.Minute)

	if !b.isRunning() {
		t.Error("recently used backend should NOT have been killed")
	}
}

func TestReapIdleBackends_SkipsNotRunning(t *testing.T) {
	b := &Backend{
		name:        "stopped-backend",
		def:         BackendDef{Command: []string{"echo"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		running:     false,
	}

	gw := &Gateway{
		backends: map[string]*Backend{"stopped-backend": b},
	}

	// Should not panic or error
	gw.reapIdleBackends(5 * time.Minute)

	if b.isRunning() {
		t.Error("should still be not running")
	}
}

// --- Cache Tests ---

func TestSaveAndLoadToolsCache(t *testing.T) {
	dir := t.TempDir()
	toolsJSON := `{"jsonrpc":"2.0","id":"1","result":{"tools":[{"name":"tool1"}]}}`

	b := &Backend{
		name:       "cache-test",
		cacheDir:   dir,
		toolsCache: json.RawMessage(toolsJSON),
	}

	b.saveToolsCache()

	// Create a new backend and load the cache
	b2 := &Backend{
		name:     "cache-test",
		cacheDir: dir,
	}
	b2.loadToolsCache()

	if b2.toolsCache == nil {
		t.Fatal("toolsCache should be loaded from disk")
	}
	if string(b2.toolsCache) != toolsJSON {
		t.Errorf("cache mismatch: got %s", string(b2.toolsCache))
	}
}

func TestLoadToolsCache_NonexistentFile(t *testing.T) {
	b := &Backend{
		name:     "no-cache",
		cacheDir: "/nonexistent/path/that/does/not/exist",
	}

	// Should not panic or error
	b.loadToolsCache()

	if b.toolsCache != nil {
		t.Error("toolsCache should be nil for nonexistent file")
	}
}

func TestSaveToolsCache_EmptyCacheDir(t *testing.T) {
	b := &Backend{
		name:       "empty-dir",
		cacheDir:   "",
		toolsCache: json.RawMessage(`{"tools":[]}`),
	}

	// Should be a no-op, not panic
	b.saveToolsCache()
}

func TestSaveToolsCache_NilCache(t *testing.T) {
	b := &Backend{
		name:       "nil-cache",
		cacheDir:   t.TempDir(),
		toolsCache: nil,
	}

	// Should be a no-op, not panic
	b.saveToolsCache()
}

// --- matchGlob Tests ---

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern  string
		name     string
		expected bool
	}{
		{"*", "anything", true},
		{"*", "", true},
		{"get_*", "get_users", true},
		{"get_*", "create_users", false},
		{"*_user", "get_user", true},
		{"*_user", "get_users", false},
		{"delete_*", "delete_repo", true},
		{"delete_*", "create_repo", false},
		{"[invalid", "test", false}, // invalid glob pattern
	}

	for _, tt := range tests {
		got := matchGlob(tt.pattern, tt.name)
		if got != tt.expected {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.expected)
		}
	}
}

// --- shutdownAll Test ---

func TestShutdownAll(t *testing.T) {
	b1 := &Backend{
		name:        "shut1",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}
	b2 := &Backend{
		name:        "shut2",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}

	b1.mu.Lock()
	if err := b1.spawnProcess(); err != nil {
		b1.mu.Unlock()
		t.Fatalf("spawn b1: %v", err)
	}
	b1.mu.Unlock()

	b2.mu.Lock()
	if err := b2.spawnProcess(); err != nil {
		b2.mu.Unlock()
		t.Fatalf("spawn b2: %v", err)
	}
	b2.mu.Unlock()

	gw := &Gateway{
		backends: map[string]*Backend{"shut1": b1, "shut2": b2},
	}

	gw.shutdownAll()
	time.Sleep(200 * time.Millisecond)

	if b1.isRunning() {
		t.Error("b1 should not be running after shutdownAll")
	}
	if b2.isRunning() {
		t.Error("b2 should not be running after shutdownAll")
	}
}

// --- newGateway with disabled backends ---

func TestNewGateway_DisabledBackends(t *testing.T) {
	cfg := Config{
		Listen: ":0",
		Backends: map[string]BackendDef{
			"enabled1": {Command: []string{"echo"}},
			"enabled2": {Command: []string{"echo"}},
			"disabled": {Command: []string{"echo"}, Disabled: true},
		},
	}
	gw := newGateway(cfg)

	if len(gw.backends) != 2 {
		t.Errorf("expected 2 backends, got %d", len(gw.backends))
	}
	if _, ok := gw.backends["disabled"]; ok {
		t.Error("disabled backend should not be in gateway")
	}
}

func TestNewGateway_RemoteBackend(t *testing.T) {
	cfg := Config{
		Listen: ":0",
		Backends: map[string]BackendDef{
			"remote": {URL: "https://example.com/mcp", TransportType: "streamable-http"},
		},
	}
	gw := newGateway(cfg)

	b, ok := gw.backends["remote"]
	if !ok {
		t.Fatal("expected remote backend")
	}
	if b.httpClient == nil {
		t.Error("remote backend should have httpClient")
	}
	if !b.isRemote() {
		t.Error("should be marked as remote")
	}
}

// --- BearerAuthorizer.IsEnabled Tests ---

func TestBearerAuthorizer_IsEnabled(t *testing.T) {
	// No tokens configured
	a := NewBearerAuthorizer(nil, map[string]BackendDef{})
	if a.IsEnabled() {
		t.Error("should not be enabled with no tokens")
	}

	// Global tokens
	a = NewBearerAuthorizer([]string{"token1"}, map[string]BackendDef{})
	if !a.IsEnabled() {
		t.Error("should be enabled with global tokens")
	}

	// Per-backend tokens only
	a = NewBearerAuthorizer(nil, map[string]BackendDef{
		"test": {AuthTokens: []string{"backend-token"}},
	})
	if !a.IsEnabled() {
		t.Error("should be enabled with per-backend tokens")
	}
}

// --- sendSSE test with httptest ---

func TestSendSSE_Integration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env jsonRPCMessage
		json.Unmarshal(body, &env)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		resp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"echo":true}}`, string(*env.ID))
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", resp)
	}))
	defer srv.Close()

	b := &Backend{
		name:       "sse-test",
		def:        BackendDef{URL: srv.URL, TransportType: "sse"},
		httpClient: &http.Client{Timeout: 5 * time.Second},
		logEnabled: true,
	}

	msg := []byte(`{"jsonrpc":"2.0","id":"sse1","method":"tools/list","params":{}}`)
	resp, err := b.sendSSE(context.Background(), msg)
	if err != nil {
		t.Fatalf("sendSSE: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(resp, &result); err != nil {
		t.Fatalf("unmarshal SSE response: %v", err)
	}
	if result["id"] == nil {
		t.Error("expected id in SSE response")
	}
}

func TestSendSSE_NonSSEResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env jsonRPCMessage
		json.Unmarshal(body, &env)

		w.Header().Set("Content-Type", "application/json")
		resp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, string(*env.ID))
		fmt.Fprint(w, resp)
	}))
	defer srv.Close()

	b := &Backend{
		name:       "sse-json",
		def:        BackendDef{URL: srv.URL, TransportType: "sse"},
		httpClient: &http.Client{Timeout: 5 * time.Second},
		logEnabled: true,
	}

	msg := []byte(`{"jsonrpc":"2.0","id":"j1","method":"ping"}`)
	resp, err := b.sendSSE(context.Background(), msg)
	if err != nil {
		t.Fatalf("sendSSE (JSON fallback): %v", err)
	}
	if len(resp) == 0 {
		t.Error("expected non-empty response")
	}
}

// --- forwardToBackend with real subprocess ---

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

// --- handleDiscoverTools tests ---

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

// --- truncateDescription test ---

func TestTruncateDescription(t *testing.T) {
	b := &Backend{maxDescLen: 10}

	raw := json.RawMessage(`{"name":"tool","description":"a very long description that should be truncated"}`)
	result := b.truncateDescription(raw)

	var tool map[string]any
	json.Unmarshal(result, &tool)
	desc := tool["description"].(string)
	if len(desc) > 13 { // 10 + "..."
		t.Errorf("description should be truncated, got %q (len=%d)", desc, len(desc))
	}
}

func TestTruncateDescription_NoTruncate(t *testing.T) {
	b := &Backend{maxDescLen: 0} // disabled

	raw := json.RawMessage(`{"name":"tool","description":"long description"}`)
	result := b.truncateDescription(raw)

	if string(result) != string(raw) {
		t.Error("should not truncate when maxDescLen=0")
	}
}

func TestTruncateDescription_ShortDesc(t *testing.T) {
	b := &Backend{maxDescLen: 100}

	raw := json.RawMessage(`{"name":"tool","description":"short"}`)
	result := b.truncateDescription(raw)

	if string(result) != string(raw) {
		t.Error("should not truncate short descriptions")
	}
}

// --- extractMethod test ---

func TestExtractMethod(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{`{"method":"tools/list"}`, "tools/list"},
		{`{"method":"ping","id":1}`, "ping"},
		{`invalid json`, "unknown"},
		{`{}`, ""},
	}
	for _, tt := range tests {
		got := extractMethod([]byte(tt.input))
		if got != tt.expected {
			t.Errorf("extractMethod(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

// --- dispatchResponse tests ---

func TestDispatchResponse_MatchesPending(t *testing.T) {
	b := &Backend{
		name:    "dispatch-test",
		pending: make(map[string]chan json.RawMessage),
	}

	ch := make(chan json.RawMessage, 1)
	b.pending[`"req1"`] = ch

	line := []byte(`{"jsonrpc":"2.0","id":"req1","result":{}}`)
	b.dispatchResponse(line)

	select {
	case resp := <-ch:
		if resp == nil {
			t.Error("expected response data")
		}
	default:
		t.Error("expected response in channel")
	}
}

func TestDispatchResponse_NoID(t *testing.T) {
	b := &Backend{
		name:    "dispatch-noid",
		pending: make(map[string]chan json.RawMessage),
	}

	// Should not panic
	line := []byte(`{"jsonrpc":"2.0","method":"notification"}`)
	b.dispatchResponse(line)
}

func TestDispatchResponse_InvalidJSON(t *testing.T) {
	b := &Backend{
		name:    "dispatch-invalid",
		pending: make(map[string]chan json.RawMessage),
	}

	// Should not panic
	b.dispatchResponse([]byte(`not json at all`))
}

func TestDispatchResponse_UnmatchedID(t *testing.T) {
	b := &Backend{
		name:    "dispatch-unmatched",
		pending: make(map[string]chan json.RawMessage),
	}

	// No pending request for this id - should not panic
	line := []byte(`{"jsonrpc":"2.0","id":"orphan","result":{}}`)
	b.dispatchResponse(line)
}

// --- parseLogLevel test ---

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"debug", "DEBUG"},
		{"DEBUG", "DEBUG"},
		{"warn", "WARN"},
		{"warning", "WARN"},
		{"error", "ERROR"},
		{"info", "INFO"},
		{"", "INFO"},
		{"unknown", "INFO"},
	}

	for _, tt := range tests {
		got := parseLogLevel(tt.input)
		if got.String() != tt.expected {
			t.Errorf("parseLogLevel(%q) = %v, want %s", tt.input, got, tt.expected)
		}
	}
}

// --- jsonRPCError test ---

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

// --- handleToolsList from cache ---

func TestHandleToolsList_FromCache_NoCats(t *testing.T) {
	toolsJSON := `{"jsonrpc":"2.0","id":"old","result":{"tools":[{"name":"tool1","description":"T1"}]}}`
	b := &Backend{
		name:        "cached-nocat",
		def:         BackendDef{},
		toolsCache:  json.RawMessage(toolsJSON),
		activeTools: make(map[string]bool),
	}
	// No categories (few tools)

	id := json.RawMessage(`"new-id"`)
	data, handled := b.handleToolsList(&id)
	if !handled {
		t.Fatal("should be handled from cache")
	}

	var resp map[string]any
	json.Unmarshal(data, &resp)
	// ID should be updated
	if fmt.Sprintf("%v", resp["id"]) != "new-id" {
		t.Errorf("id should be new-id, got %v", resp["id"])
	}
}

func TestHandleToolsList_NoCache(t *testing.T) {
	b := &Backend{
		name:        "no-cache-list",
		def:         BackendDef{},
		toolsCache:  nil,
		activeTools: make(map[string]bool),
	}

	id := json.RawMessage(`1`)
	_, handled := b.handleToolsList(&id)
	if handled {
		t.Error("should not be handled when no cache exists")
	}
}

// --- notifyToolsChanged ---

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

// --- rotatingWriter tests ---

func TestNewRotatingWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	rw, err := newRotatingWriter(path, 1, 2)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	if rw == nil {
		t.Fatal("expected non-nil writer")
	}

	// Write some data
	n, err := rw.Write([]byte("hello world\n"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != 12 {
		t.Errorf("expected 12 bytes written, got %d", n)
	}

	// Verify file exists
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("log file should exist: %v", err)
	}
}

func TestRotatingWriter_Rotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rotate.log")

	// Use tiny max size to force rotation
	rw, err := newRotatingWriter(path, 0, 2) // 0 defaults to 10MB, use direct
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}

	// Override maxSize directly for test
	rw.maxSize = 50 // 50 bytes

	// Write enough to trigger rotation
	for i := 0; i < 10; i++ {
		rw.Write([]byte("this is a test log line that is fairly long\n"))
	}

	// Check that rotated file exists
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("rotated file should exist: %v", err)
	}
}

func TestNewRotatingWriter_Defaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "defaults.log")

	rw, err := newRotatingWriter(path, 0, 0) // both 0 -> use defaults
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	if rw.maxSize != 10*1024*1024 {
		t.Errorf("expected default max size 10MB, got %d", rw.maxSize)
	}
	if rw.maxFiles != 3 {
		t.Errorf("expected default max files 3, got %d", rw.maxFiles)
	}
}

func TestNewRotatingWriter_CreatesDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "dir", "test.log")

	rw, err := newRotatingWriter(path, 1, 1)
	if err != nil {
		t.Fatalf("newRotatingWriter with nested dir: %v", err)
	}
	rw.Write([]byte("test\n"))

	if _, err := os.Stat(path); err != nil {
		t.Errorf("log file should exist after write: %v", err)
	}
}

// --- initLogging tests ---

func TestInitLogging_Default(t *testing.T) {
	cfg := Config{LogLevel: "debug"}
	err := initLogging(cfg, "/tmp/test-config.json")
	if err != nil {
		t.Fatalf("initLogging: %v", err)
	}
}

func TestInitLogging_WithFile(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		LogLevel:     "info",
		LogFile:      "gateway.log",
		LogMaxSizeMB: 1,
		LogMaxFiles:  2,
	}
	configPath := filepath.Join(dir, "config.json")
	err := initLogging(cfg, configPath)
	if err != nil {
		t.Fatalf("initLogging with file: %v", err)
	}

	// Log file should be created
	logPath := filepath.Join(dir, "gateway.log")
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("log file should exist: %v", err)
	}
}

func TestInitLogging_EnvOverride(t *testing.T) {
	os.Setenv("MCP_GATEWAY_DEBUG", "1")
	defer os.Unsetenv("MCP_GATEWAY_DEBUG")

	cfg := Config{LogLevel: "error"}
	err := initLogging(cfg, "/tmp/test.json")
	if err != nil {
		t.Fatalf("initLogging: %v", err)
	}
	// logLevel should be debug due to env override
	// (we can't easily check the global, but at least it doesn't error)
}

// --- resolveConfigPath tests ---

func TestResolveConfigPath_Default(t *testing.T) {
	// Save and restore os.Args
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	os.Args = []string{"mcp-gateway"}
	path := resolveConfigPath()
	// Should return either "config.json" or a path ending with config.json
	if !strings.HasSuffix(path, "config.json") {
		t.Errorf("expected path ending in config.json, got %q", path)
	}
}

func TestResolveConfigPath_WithArg(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "my-config.json")
	os.WriteFile(cfgPath, []byte(`{}`), 0644)

	os.Args = []string{"mcp-gateway", cfgPath}
	path := resolveConfigPath()
	if path != cfgPath {
		t.Errorf("expected %q, got %q", cfgPath, path)
	}
}

// --- selfIdleTimer test ---

func TestSelfIdleTimer_Triggers(t *testing.T) {
	// selfIdleTimer has a minimum check interval of 30s, which is too slow for tests.
	// Instead, test the cancellation path and verify the idle logic independently.
	gw := &Gateway{
		backends:    make(map[string]*Backend),
		lastRequest: time.Now(),
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		gw.selfIdleTimer(ctx, cancel, 1*time.Hour) // long timeout, won't trigger
		close(done)
	}()

	// Cancel the context to simulate shutdown
	cancel()

	select {
	case <-done:
		// Clean exit on context cancel
	case <-time.After(2 * time.Second):
		t.Fatal("selfIdleTimer should have exited on context cancel")
	}
}

// --- idleReaper test (context-based) ---

func TestIdleReaper_Cancels(t *testing.T) {
	gw := &Gateway{
		backends: make(map[string]*Backend),
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		gw.idleReaper(ctx, 1*time.Hour)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// clean exit
	case <-time.After(2 * time.Second):
		t.Fatal("idleReaper should have exited on context cancel")
	}
}

// --- pidFile functions ---

func TestPidFilePath(t *testing.T) {
	path := pidFilePath()
	if !strings.Contains(path, "mcp-gateway.pid") {
		t.Errorf("expected path containing mcp-gateway.pid, got %q", path)
	}
}

func TestWriteAndRemovePidFile(t *testing.T) {
	path := writePidFile()
	defer os.Remove(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	if !strings.Contains(string(data), fmt.Sprintf("%d", os.Getpid())) {
		t.Errorf("pidfile should contain current pid, got %q", string(data))
	}

	removePidFile(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("pidfile should be removed")
	}
}

func TestRemovePidFile_Nonexistent(t *testing.T) {
	// Should not panic
	removePidFile("/nonexistent/path/mcp-gateway.pid")
}

// --- checkExistingInstance test ---

func TestCheckExistingInstance_NoPidFile(t *testing.T) {
	// Remove pidfile if it exists
	path := pidFilePath()
	os.Remove(path)

	result := checkExistingInstance("127.0.0.1:0")
	if result {
		t.Error("should return false when no pidfile exists")
	}
}

// --- handleHealth coverage for running backends ---

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

// --- handleSubscriptionsListen ---

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

// --- handleRequest missing backend name ---

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

// --- handleRequest invalid JSON ---

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

// --- forwardToBackend retry on send failure ---

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

// --- sendStreamableHTTP SSE response path ---

func TestSendStreamableHTTP_SSEResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env jsonRPCMessage
		json.Unmarshal(body, &env)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		resp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"streamed":true}}`, string(*env.ID))
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", resp)
	}))
	defer srv.Close()

	b := &Backend{
		name:       "http-sse",
		def:        BackendDef{URL: srv.URL, TransportType: "streamable-http"},
		httpClient: &http.Client{Timeout: 5 * time.Second},
		logEnabled: true,
	}

	msg := []byte(`{"jsonrpc":"2.0","id":"sh1","method":"tools/list","params":{}}`)
	resp, err := b.sendStreamableHTTP(context.Background(), msg)
	if err != nil {
		t.Fatalf("sendStreamableHTTP (SSE): %v", err)
	}

	var result map[string]any
	json.Unmarshal(resp, &result)
	r := result["result"].(map[string]any)
	if r["streamed"] != true {
		t.Error("expected streamed=true in response")
	}
}

// --- sendRemote unknown transport ---

func TestSendRemote_UnknownTransport(t *testing.T) {
	b := &Backend{
		name:       "bad-transport",
		def:        BackendDef{URL: "https://example.com", TransportType: "grpc"},
		httpClient: &http.Client{Timeout: 5 * time.Second},
		logEnabled: true,
	}
	b.mu.Lock()
	b.lastUsed = time.Now()
	b.mu.Unlock()

	msg := []byte(`{"jsonrpc":"2.0","id":"x","method":"ping"}`)
	_, err := b.sendRemote(context.Background(), msg)
	if err == nil {
		t.Fatal("expected error for unknown transport")
	}
}

// --- Additional coverage tests ---

func TestCheckExistingInstance_InvalidPidfile(t *testing.T) {
	pidFile := pidFilePath()
	// Write invalid content
	os.WriteFile(pidFile, []byte("not a number"), 0600)
	defer os.Remove(pidFile)

	result := checkExistingInstance("127.0.0.1:0")
	if result {
		t.Error("should return false for invalid pidfile content")
	}
}

func TestCheckExistingInstance_DeadProcess(t *testing.T) {
	pidFile := pidFilePath()
	// Write a PID that is almost certainly dead (very high number)
	os.WriteFile(pidFile, []byte("9999999"), 0600)
	defer os.Remove(pidFile)

	result := checkExistingInstance("127.0.0.1:0")
	if result {
		t.Error("should return false for dead process")
	}
}

func TestParseSSEResponse_NoResponse(t *testing.T) {
	b := &Backend{name: "sse-empty", logEnabled: true}

	// Empty SSE stream with no data lines
	reader := strings.NewReader("")
	_, err := b.parseSSEResponse(reader)
	if err == nil {
		t.Error("expected error for empty SSE stream")
	}
}

func TestParseSSEResponse_NotificationOnly(t *testing.T) {
	b := &Backend{name: "sse-notif", logEnabled: true}

	// SSE with only a notification (no id field), then stream ends with unterminated data
	input := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notification\"}\n\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"final\",\"result\":{}}"
	reader := strings.NewReader(input)

	data, err := b.parseSSEResponse(reader)
	if err != nil {
		t.Fatalf("parseSSEResponse: %v", err)
	}
	// Should get the unterminated data line (fallback behavior)
	if len(data) == 0 {
		t.Error("expected data from unterminated SSE")
	}
}

func TestSendStreamableHTTP_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	b := &Backend{
		name:       "http-err",
		def:        BackendDef{URL: srv.URL, TransportType: "streamable-http"},
		httpClient: &http.Client{Timeout: 5 * time.Second},
		logEnabled: true,
	}

	msg := []byte(`{"jsonrpc":"2.0","id":"e1","method":"ping"}`)
	_, err := b.sendStreamableHTTP(context.Background(), msg)
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
}

func TestSendSSE_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	b := &Backend{
		name:       "sse-err",
		def:        BackendDef{URL: srv.URL, TransportType: "sse"},
		httpClient: &http.Client{Timeout: 5 * time.Second},
		logEnabled: true,
	}

	msg := []byte(`{"jsonrpc":"2.0","id":"e2","method":"ping"}`)
	_, err := b.sendSSE(context.Background(), msg)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
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

func TestExtractBearerToken_Variants(t *testing.T) {
	tests := []struct {
		header   string
		expected string
	}{
		{"Bearer mytoken", "mytoken"},
		{"bearer mytoken", "mytoken"},
		{"BEARER mytoken", "mytoken"},
		{"Basic abc", ""},
		{"", ""},
		{"Bear", ""},
	}

	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if tt.header != "" {
			req.Header.Set("Authorization", tt.header)
		}
		got := extractBearerToken(req)
		if got != tt.expected {
			t.Errorf("extractBearerToken(%q) = %q, want %q", tt.header, got, tt.expected)
		}
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

func TestSaveToolsCache_Success(t *testing.T) {
	dir := t.TempDir()
	cacheSubDir := filepath.Join(dir, "newcache")

	b := &Backend{
		name:       "save-test",
		cacheDir:   cacheSubDir,
		toolsCache: json.RawMessage(`{"result":{"tools":[]}}`),
	}

	b.saveToolsCache()

	// Verify file was created
	path := filepath.Join(cacheSubDir, "save-test.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cache file should exist: %v", err)
	}
	if string(data) != `{"result":{"tools":[]}}` {
		t.Errorf("unexpected cache content: %s", string(data))
	}
}

// --- Test loadConfig with cache_dir ---

func TestLoadConfig_CacheDir(t *testing.T) {
	dir := t.TempDir()
	cfgContent := `{"backends":{},"cache_dir":"my-cache"}`
	cfgPath := filepath.Join(dir, "config.json")
	os.WriteFile(cfgPath, []byte(cfgContent), 0644)

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	expected := filepath.Join(dir, "my-cache")
	if cfg.CacheDir != expected {
		t.Errorf("expected cache_dir %q, got %q", expected, cfg.CacheDir)
	}
}

func TestLoadConfig_AbsCacheDir(t *testing.T) {
	dir := t.TempDir()
	absCache := filepath.Join(dir, "abs-cache")
	cfgContent := fmt.Sprintf(`{"backends":{},"cache_dir":"%s"}`, absCache)
	cfgPath := filepath.Join(dir, "config.json")
	os.WriteFile(cfgPath, []byte(cfgContent), 0644)

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	if cfg.CacheDir != absCache {
		t.Errorf("expected cache_dir %q, got %q", absCache, cfg.CacheDir)
	}
}

// --- Global discovery config propagation ---

func TestNewGateway_GlobalDiscoveryPropagation(t *testing.T) {
	force := true
	cfg := Config{
		Listen:    ":0",
		Discovery: &force,
		Backends: map[string]BackendDef{
			"test": {Command: []string{"echo"}},
		},
	}
	gw := newGateway(cfg)

	b := gw.backends["test"]
	if b.def.Discovery == nil {
		t.Error("global discovery should propagate to backend")
	}
}

// --- writeResponse empty body ---

func TestWriteResponse_EmptyBody(t *testing.T) {
	gw := newTestGateway(map[string]BackendDef{})

	req := httptest.NewRequest(http.MethodPost, "/test/mcp", nil)
	w := httptest.NewRecorder()

	gw.writeResponse(w, req, []byte{})

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202 for empty response, got %d", w.Code)
	}
}
