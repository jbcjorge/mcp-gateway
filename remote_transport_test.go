package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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
		"servers": {
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
		},
		"backends": ["remote-sse", "remote-http"]
	}`
	_ = writeTestFile(dir+"/config.json", config)

	cfg, err := loadConfig(dir + "/config.json")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	sse, ok := cfg.Servers["remote-sse"]
	if !ok {
		t.Fatal("expected remote-sse server")
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

	httpB, ok := cfg.Servers["remote-http"]
	if !ok {
		t.Fatal("expected remote-http server")
	}
	if httpB.TransportType != "streamable-http" {
		t.Errorf("expected streamable-http, got %s", httpB.TransportType)
	}
	if httpB.LogEnabled == nil || *httpB.LogEnabled != true {
		t.Error("expected log_enabled=true")
	}
}

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
