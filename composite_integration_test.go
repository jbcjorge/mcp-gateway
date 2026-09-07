package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// gatewayWithComposite builds a Gateway with one composite route "combo" over
// two real subprocess members, plus a prefixed single-server route "pref".
func gatewayWithComposite(t *testing.T) *Gateway {
	t.Helper()
	cfg := Config{
		Listen: ":0",
		Servers: map[string]BackendDef{
			"m1": {Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
			"m2": {Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		},
		Compositions: map[string]CompositionDef{
			"combo": {Members: []MemberDef{{Server: "m1"}, {Server: "m2"}}},
		},
		Routes: []RouteEntry{
			{Route: "combo"},
			{Route: "m1", Prefix: "p_"},
		},
	}
	gw, err := newGateway(cfg)
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}
	return gw
}

func postJSON(t *testing.T, gw *Gateway, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	gw.handleRequest(w, req)
	return w
}

func TestComposite_InitializeAndToolsListMerged(t *testing.T) {
	gw := gatewayWithComposite(t)
	defer gw.shutdownAll()

	// initialize is handled locally for the composite route.
	w := postJSON(t, gw, "/combo/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("initialize: got %d", w.Code)
	}

	// tools/list merges both members. The subprocess helper returns 7 tools;
	// both members return the SAME set, so last-wins dedupes to 7 (not 14).
	w = postJSON(t, gw, "/combo/mcp", `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("tools/list: got %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v body=%s", err, w.Body.String())
	}
	if len(resp.Result.Tools) != 7 {
		t.Errorf("expected 7 merged tools (deduped), got %d", len(resp.Result.Tools))
	}
}

func TestComposite_ToolsCallDispatched(t *testing.T) {
	gw := gatewayWithComposite(t)
	defer gw.shutdownAll()

	// Prime the merged list so ownership is known.
	_ = postJSON(t, gw, "/combo/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)

	// Call a known tool; should dispatch to the owning member and return a result.
	w := postJSON(t, gw, "/combo/mcp", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_users","arguments":{}}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("tools/call: got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"id":3`) {
		t.Errorf("expected reply with id 3, got %s", w.Body.String())
	}
}

func TestPrefixedRoute_ToolsListAndCall(t *testing.T) {
	gw := gatewayWithComposite(t)
	defer gw.shutdownAll()

	// Prefixed single-server route: tools advertised with p_ prefix.
	w := postJSON(t, gw, "/m1/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("tools/list: %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"p_get_users"`) {
		t.Errorf("expected prefixed tool p_get_users, got %s", w.Body.String())
	}

	// Calling the prefixed name should dispatch (prefix stripped) and succeed.
	w = postJSON(t, gw, "/m1/mcp", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"p_get_users","arguments":{}}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("prefixed tools/call: %d body=%s", w.Code, w.Body.String())
	}
}

func TestComposite_UnknownToolErrors(t *testing.T) {
	gw := gatewayWithComposite(t)
	defer gw.shutdownAll()
	_ = postJSON(t, gw, "/combo/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	w := postJSON(t, gw, "/combo/mcp", `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"does_not_exist","arguments":{}}}`)
	// The gateway returns a JSON-RPC error (HTTP 502 with error body).
	if w.Code == http.StatusOK && !strings.Contains(w.Body.String(), "error") {
		t.Errorf("expected error for unknown tool, got %s", w.Body.String())
	}
}
