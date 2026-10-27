package main

import (
	"encoding/json"
	"testing"
)

func TestIsLocalListMethod(t *testing.T) {
	tests := []struct {
		method string
		want   bool
	}{
		{"resources/list", true},
		{"resources/templates/list", true},
		{"prompts/list", true},
		{"tools/list", false},
		{"tools/call", false},
		{"initialize", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			if got := isLocalListMethod(tt.method); got != tt.want {
				t.Errorf("isLocalListMethod(%q) = %v, want %v", tt.method, got, tt.want)
			}
		})
	}
}

func assertEmptyListResponse(t *testing.T, data []byte, wantKey string) {
	t.Helper()
	if data == nil {
		t.Fatal("nil response")
	}
	var resp struct {
		JSONRPC string                     `json:"jsonrpc"`
		Result  map[string]json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want 2.0", resp.JSONRPC)
	}
	arr, ok := resp.Result[wantKey]
	if !ok {
		t.Fatalf("result missing key %q; got %v", wantKey, resp.Result)
	}
	if string(arr) != "[]" {
		t.Errorf("%s = %s, want []", wantKey, arr)
	}
}

func TestLocalListResponse_Empty(t *testing.T) {
	id := json.RawMessage(`7`)
	tests := []struct {
		method  string
		wantKey string
	}{
		{"resources/list", "resources"},
		{"resources/templates/list", "resourceTemplates"},
		{"prompts/list", "prompts"},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			b := &Backend{name: "probe"}
			assertEmptyListResponse(t, b.localListResponse(tt.method, &id), tt.wantKey)
		})
	}
}

func TestLocalListResponse_NotLocalMethod(t *testing.T) {
	id := json.RawMessage(`1`)
	b := &Backend{name: "probe"}
	if got := b.localListResponse("tools/list", &id); got != nil {
		t.Errorf("expected nil for non-local method, got %s", got)
	}
}

func TestLocalListResponse_ServesCache(t *testing.T) {
	id := json.RawMessage(`9`)
	b := &Backend{
		name:           "probe",
		resourcesCache: json.RawMessage(`{"resources":[{"uri":"x://1","name":"one"}]}`),
	}
	data := b.localListResponse("resources/list", &id)
	var resp struct {
		Result struct {
			Resources []map[string]any `json:"resources"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Result.Resources) != 1 || resp.Result.Resources[0]["name"] != "one" {
		t.Errorf("expected cached resource served, got %v", resp.Result.Resources)
	}
}
