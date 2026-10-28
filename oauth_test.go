package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestVerifyPKCE(t *testing.T) {
	verifier := "abc123-verifier-abc123-verifier-abc123"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	tests := []struct {
		name     string
		verifier string
		want     bool
	}{
		{"match", verifier, true},
		{"mismatch", "wrong-verifier", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := verifyPKCE(tt.verifier, challenge); got != tt.want {
				t.Errorf("verifyPKCE = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRouteFromResource(t *testing.T) {
	tests := []struct {
		resource string
		want     string
	}{
		{"http://localhost:19900/anyorg/mcp", "anyorg"},
		{"http://localhost:19900/anyorg", "anyorg"},
		{"http://localhost:19900/anyorg/mcp/", "anyorg"},
		{"https://host/wiki/mcp", "wiki"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.resource, func(t *testing.T) {
			if got := routeFromResource(tt.resource); got != tt.want {
				t.Errorf("routeFromResource(%q) = %q, want %q", tt.resource, got, tt.want)
			}
		})
	}
}

// oauthTestGateway builds a gateway with one route "demo" that declares an auth
// hook whose provider prints a fixed token.
func oauthTestGateway(t *testing.T) *Gateway {
	t.Helper()
	gw := &Gateway{
		config: Config{
			Servers: map[string]BackendDef{
				"demo": {
					Command: []string{"true"},
					Auth: &AuthHook{
						Provider:    []string{"bash", "-c", "echo demo-token-123"},
						TokenFormat: "opaque",
						TTLSeconds:  3600,
						Expose:      "real",
					},
				},
			},
		},
		oauth: newOAuthStore(),
	}
	return gw
}

func TestOAuthFlow_AuthorizeThenToken(t *testing.T) {
	gw := oauthTestGateway(t)

	verifier := "the-code-verifier-0123456789-abcdefgh"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	redirect := "http://127.0.0.1:58831"

	// --- /authorize ---
	authURL := "/authorize?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {"Q DEV CLI"},
		"state":                 {"xyz"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"redirect_uri":          {redirect},
		"resource":              {"http://localhost:19900/demo/mcp"},
	}.Encode()

	rec := httptest.NewRecorder()
	gw.handleRequest(rec, httptest.NewRequest(http.MethodGet, authURL, nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("authorize: expected 302, got %d (%s)", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, redirect) {
		t.Fatalf("authorize: redirect to %q, want prefix %q", loc, redirect)
	}
	lu, _ := url.Parse(loc)
	code := lu.Query().Get("code")
	if code == "" || lu.Query().Get("state") != "xyz" {
		t.Fatalf("authorize: missing code/state in %q", loc)
	}

	// --- /token ---
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirect},
	}
	req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	gw.handleRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("token: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "demo-token-123") {
		t.Errorf("token: expected real token in access_token, got %s", body)
	}
	if !strings.Contains(body, "refresh_token") {
		t.Errorf("token: expected a refresh_token, got %s", body)
	}
}

func TestOAuthFlow_WrongVerifierRejected(t *testing.T) {
	gw := oauthTestGateway(t)
	sum := sha256.Sum256([]byte("real-verifier"))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	redirect := "http://127.0.0.1:58831"

	authURL := "/authorize?" + url.Values{
		"response_type":         {"code"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"redirect_uri":          {redirect},
		"resource":              {"http://localhost:19900/demo/mcp"},
	}.Encode()
	rec := httptest.NewRecorder()
	gw.handleRequest(rec, httptest.NewRequest(http.MethodGet, authURL, nil))
	code, _ := url.Parse(rec.Header().Get("Location"))
	authCodeVal := code.Query().Get("code")

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCodeVal},
		"code_verifier": {"WRONG-verifier"},
	}
	req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	gw.handleRequest(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("token with wrong verifier: expected 400, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_grant") {
		t.Errorf("expected invalid_grant, got %s", rec.Body.String())
	}
}

func TestOAuth_DiscoveryMetadata(t *testing.T) {
	gw := oauthTestGateway(t)
	rec := httptest.NewRecorder()
	gw.handleRequest(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metadata: expected 200, got %d", rec.Code)
	}
	for _, want := range []string{"authorization_endpoint", "token_endpoint", "S256"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("metadata missing %q: %s", want, rec.Body.String())
		}
	}
}

func TestOAuthFlow_RefreshReRunsHook(t *testing.T) {
	gw := oauthTestGateway(t)
	// A valid refresh token encodes its route: gwr_<route>_<random>.
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"gwr_demo_abc123"}}
	req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	gw.handleRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "demo-token-123") {
		t.Errorf("refresh: expected fresh real token, got %s", rec.Body.String())
	}

	// A malformed refresh token (no route) is rejected.
	bad := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"garbage"}}
	req2 := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(bad.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec2 := httptest.NewRecorder()
	gw.handleRequest(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Errorf("malformed refresh: expected 400, got %d", rec2.Code)
	}
}

func TestRouteFromRefresh(t *testing.T) {
	tests := []struct {
		refresh string
		want    string
	}{
		{"gwr_demo_abc123", "demo"},
		{"gwr_anyorg_xyz", "anyorg"},
		{"gwr_a_b_c", "a_b"}, // route may contain underscores; split on last _
		{"garbage", ""},
		{"gwr_", ""},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.refresh, func(t *testing.T) {
			if got := routeFromRefresh(tt.refresh); got != tt.want {
				t.Errorf("routeFromRefresh(%q) = %q, want %q", tt.refresh, got, tt.want)
			}
		})
	}
}

func TestInjectAuthMeta(t *testing.T) {
	gw := oauthTestGateway(t) // route "demo" has an auth hook
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"anyorg_search","arguments":{"keyword":"x"}}}`

	t.Run("hook route injects bearer into _meta", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/demo/mcp", nil)
		r.Header.Set("Authorization", "Bearer realtok123")
		out := gw.injectAuthMeta("demo", "tools/call", r, []byte(call))
		var m struct {
			Params struct {
				Name string `json:"name"`
				Args struct {
					Keyword string `json:"keyword"`
				} `json:"arguments"`
				Meta struct {
					Authorization string `json:"authorization"`
				} `json:"_meta"`
			} `json:"params"`
		}
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if m.Params.Meta.Authorization != "Bearer realtok123" {
			t.Errorf("_meta.authorization = %q, want Bearer realtok123", m.Params.Meta.Authorization)
		}
		if m.Params.Name != "anyorg_search" || m.Params.Args.Keyword != "x" {
			t.Errorf("existing params not preserved: %+v", m.Params)
		}
	})

	t.Run("non-hook route unchanged", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/other/mcp", nil)
		r.Header.Set("Authorization", "Bearer realtok123")
		out := gw.injectAuthMeta("other", "tools/call", r, []byte(call))
		if string(out) != call {
			t.Errorf("non-hook route body changed:\n got %s", out)
		}
	})

	t.Run("missing header unchanged", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/demo/mcp", nil)
		out := gw.injectAuthMeta("demo", "tools/call", r, []byte(call))
		if string(out) != call {
			t.Errorf("no-header body changed:\n got %s", out)
		}
	})

	t.Run("non tools/call unchanged", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/demo/mcp", nil)
		r.Header.Set("Authorization", "Bearer realtok123")
		out := gw.injectAuthMeta("demo", "tools/list", r, []byte(call))
		if string(out) != call {
			t.Errorf("tools/list body changed:\n got %s", out)
		}
	})
}
