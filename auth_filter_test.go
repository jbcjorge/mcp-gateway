package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
	gw.authorizer = NewBearerAuthorizer([]string{"secret-token"}, nil)

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
	gw.authorizer = NewBearerAuthorizer([]string{"secret-token"}, nil)

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
	gw.authorizer = NewBearerAuthorizer([]string{"global-secret"}, map[string][]string{"restricted": {"backend-secret"}})

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
	gw.authorizer = NewBearerAuthorizer([]string{"secret"}, nil)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	gw.handleRequest(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("health should not require auth, got %d", w.Code)
	}
}

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

func TestBearerAuthorizer_IsEnabled(t *testing.T) {
	// No tokens configured
	a := NewBearerAuthorizer(nil, nil)
	if a.IsEnabled() {
		t.Error("should not be enabled with no tokens")
	}

	// Global tokens
	a = NewBearerAuthorizer([]string{"token1"}, nil)
	if !a.IsEnabled() {
		t.Error("should be enabled with global tokens")
	}

	// Per-route tokens only
	a = NewBearerAuthorizer(nil, map[string][]string{
		"test": {"backend-token"},
	})
	if !a.IsEnabled() {
		t.Error("should be enabled with per-route tokens")
	}
}

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
