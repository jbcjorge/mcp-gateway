package gateway

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jbcjorge/mcp-gateway/internal/config"

	errors "github.com/jbcjorge/errors-library"
)

const (
	testLoopbackRedirect = "http://127.0.0.1:58831"
	testVerifier         = "the-code-verifier-0123456789-abcdefgh"
)

// oauthSecurityGateway extends the demo hook route with a route whose provider
// fails, a route that asks for the unimplemented expose=opaque mode, and a plain
// route without any auth hook.
func oauthSecurityGateway(t *testing.T) *Gateway {
	t.Helper()
	gw := oauthTestGateway(t)
	gw.config.Servers["broken"] = config.BackendDef{
		Command: []string{"true"},
		Auth:    &config.AuthHook{Provider: []string{"bash", "-c", "exit 1"}, TokenFormat: "opaque"},
	}
	gw.config.Servers["opaque"] = config.BackendDef{
		Command: []string{"true"},
		Auth: &config.AuthHook{
			Provider:    []string{"bash", "-c", "echo opaque-token"},
			TokenFormat: "opaque",
			TTLSeconds:  60,
			Expose:      "opaque",
		},
	}
	gw.config.Servers["nohook"] = config.BackendDef{Command: []string{"true"}}
	return gw
}

func testChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorizeParams returns a valid /authorize query for route; callers override
// individual keys to build failing variants.
func authorizeParams(route string) url.Values {
	return url.Values{
		"response_type":         {"code"},
		"state":                 {"st-1"},
		"code_challenge":        {testChallenge(testVerifier)},
		"code_challenge_method": {"S256"},
		"redirect_uri":          {testLoopbackRedirect},
		"resource":              {"http://localhost:19900/" + route + "/mcp"},
	}
}

func doAuthorize(t *testing.T, gw *Gateway, q url.Values) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	gw.handleRequest(rec, httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil))
	return rec
}

// authorizeCode runs a successful /authorize for route and returns the code.
func authorizeCode(t *testing.T, gw *Gateway, route string) string {
	t.Helper()
	rec := doAuthorize(t, gw, authorizeParams(route))
	if rec.Code != http.StatusFound {
		t.Fatalf("authorize %s: expected 302, got %d (%s)", route, rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("authorize %s: no code in %q", route, loc)
	}
	return code
}

func doToken(t *testing.T, gw *Gateway, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	gw.handleRequest(rec, req)
	return rec
}

func codeForm(code string) url.Values {
	return url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {testVerifier},
	}
}

func TestHandleAuthorize_RejectsInvalidRequests(t *testing.T) {
	gw := oauthSecurityGateway(t)

	tests := []struct {
		name      string
		mutate    func(q url.Values)
		wantCode  int
		wantError string // OAuth error in the redirect; empty = plain 400, no redirect
	}{
		{"missing redirect_uri", func(q url.Values) { q.Del("redirect_uri") }, http.StatusBadRequest, ""},
		{"non-loopback redirect (open redirect)", func(q url.Values) { q.Set("redirect_uri", "https://evil.example/cb") }, http.StatusBadRequest, ""},
		{"https loopback is not allowed", func(q url.Values) { q.Set("redirect_uri", "https://127.0.0.1:5000") }, http.StatusBadRequest, ""},
		{"unsupported response_type", func(q url.Values) { q.Set("response_type", "token") }, http.StatusFound, "unsupported_response_type"},
		{"missing PKCE challenge", func(q url.Values) { q.Del("code_challenge") }, http.StatusFound, "invalid_request"},
		{"plain PKCE method", func(q url.Values) { q.Set("code_challenge_method", "plain") }, http.StatusFound, "invalid_request"},
		{"route without auth hook", func(q url.Values) { q.Set("resource", "http://localhost:19900/nohook/mcp") }, http.StatusFound, "invalid_target"},
		{"unknown route", func(q url.Values) { q.Set("resource", "http://localhost:19900/ghost/mcp") }, http.StatusFound, "invalid_target"},
		{"provider failure", func(q url.Values) { q.Set("resource", "http://localhost:19900/broken/mcp") }, http.StatusFound, "access_denied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := authorizeParams("demo")
			tt.mutate(q)
			assertAuthorizeOutcome(t, doAuthorize(t, gw, q), tt.wantCode, tt.wantError)
		})
	}
}

// assertAuthorizeOutcome checks an /authorize response. wantError "" means the
// request must be refused with no redirect at all (bad redirect_uri); otherwise
// it must redirect with that OAuth error, echo state, and carry no code.
func assertAuthorizeOutcome(t *testing.T, rec *httptest.ResponseRecorder, wantCode int, wantError string) {
	t.Helper()
	if rec.Code != wantCode {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, wantCode, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if wantError == "" {
		if loc != "" {
			t.Fatalf("must not redirect on rejected redirect_uri, got Location %q", loc)
		}
		return
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse Location %q: %v", loc, err)
	}
	q := u.Query()
	if got := q.Get("error"); got != wantError {
		t.Errorf("error = %q, want %q (Location %q)", got, wantError, loc)
	}
	if q.Get("state") != "st-1" {
		t.Errorf("state not echoed in error redirect: %q", loc)
	}
	if q.Get("code") != "" {
		t.Errorf("error redirect must not carry a code: %q", loc)
	}
}

func TestHandleAuthorize_OpaqueExposeFallsBackToRealToken(t *testing.T) {
	gw := oauthSecurityGateway(t)
	code := authorizeCode(t, gw, "opaque")

	rec := doToken(t, gw, codeForm(code))
	if rec.Code != http.StatusOK {
		t.Fatalf("token: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "opaque-token") {
		t.Errorf("expose=opaque should serve the real token today, got %s", rec.Body.String())
	}
}

func TestTokenFromCode_Rejections(t *testing.T) {
	t.Run("unknown code", func(t *testing.T) {
		gw := oauthSecurityGateway(t)
		assertTokenError(t, doToken(t, gw, codeForm("never-issued")), "invalid_grant")
	})

	t.Run("code is single use", func(t *testing.T) {
		gw := oauthSecurityGateway(t)
		code := authorizeCode(t, gw, "demo")
		if rec := doToken(t, gw, codeForm(code)); rec.Code != http.StatusOK {
			t.Fatalf("first exchange: expected 200, got %d (%s)", rec.Code, rec.Body.String())
		}
		assertTokenError(t, doToken(t, gw, codeForm(code)), "invalid_grant")
	})

	t.Run("redirect_uri mismatch", func(t *testing.T) {
		gw := oauthSecurityGateway(t)
		form := codeForm(authorizeCode(t, gw, "demo"))
		form.Set("redirect_uri", "http://127.0.0.1:9999")
		assertTokenError(t, doToken(t, gw, form), "invalid_grant")
	})

	t.Run("expired code", func(t *testing.T) {
		gw := oauthSecurityGateway(t)
		gw.oauth.codes["stale"] = authCode{
			route:         "demo",
			codeChallenge: testChallenge(testVerifier),
			redirectURI:   testLoopbackRedirect,
			token:         "t",
			expiresAt:     time.Now().Add(-time.Second),
		}
		assertTokenError(t, doToken(t, gw, codeForm("stale")), "invalid_grant")
	})

	t.Run("route lost its hook", func(t *testing.T) {
		gw := oauthSecurityGateway(t)
		gw.oauth.codes["orphan"] = authCode{
			route:         "nohook",
			codeChallenge: testChallenge(testVerifier),
			redirectURI:   testLoopbackRedirect,
			token:         "t",
			expiresAt:     time.Now().Add(time.Minute),
		}
		assertTokenError(t, doToken(t, gw, codeForm("orphan")), "invalid_target")
	})
}

func assertTokenError(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if body["error"] != want {
		t.Errorf("error = %q, want %q", body["error"], want)
	}
	if _, leaked := body["access_token"]; leaked {
		t.Errorf("rejected exchange must not return a token: %s", rec.Body.String())
	}
}

func TestRedirectErr(t *testing.T) {
	tests := []struct {
		name        string
		redirectURI string
		state       string
		wantStatus  int
		wantLoc     string
	}{
		{"loopback with state", "http://127.0.0.1:5000/cb", "s1", http.StatusFound, "http://127.0.0.1:5000/cb?error=access_denied&state=s1"},
		{"keeps existing query", "http://localhost:5000/cb?x=1", "", http.StatusFound, "http://localhost:5000/cb?x=1&error=access_denied"},
		{"non-loopback is not redirected", "https://evil.example/cb", "s1", http.StatusBadRequest, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			redirectErr(rec, httptest.NewRequest(http.MethodGet, "/authorize", nil), tt.redirectURI, tt.state, "access_denied")
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("Location"); got != tt.wantLoc {
				t.Errorf("Location = %q, want %q", got, tt.wantLoc)
			}
		})
	}
}

func TestJwtExp(t *testing.T) {
	enc := func(s string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(s))
	}
	tests := []struct {
		name  string
		token string
		want  int64
	}{
		{"valid exp", "h." + enc(`{"exp":1893456000}`) + ".sig", 1893456000},
		{"no exp claim", "h." + enc(`{"sub":"x"}`) + ".sig", 0},
		{"not three parts", "a.b", 0},
		{"payload not base64url", "h.!!!.sig", 0},
		{"payload not JSON", "h." + enc("not-json") + ".sig", 0},
		{"empty", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := jwtExp(tt.token); got != tt.want {
				t.Errorf("jwtExp = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestHandleProtectedResourceMetadata(t *testing.T) {
	gw := oauthTestGateway(t)
	tests := []struct {
		path         string
		wantResource string
	}{
		{"/.well-known/oauth-protected-resource", "http://example.com"},
		{"/.well-known/oauth-protected-resource/", "http://example.com"},
		{"/.well-known/oauth-protected-resource/demo/mcp", "http://example.com/demo/mcp"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			gw.handleRequest(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
			}
			var body struct {
				Resource string   `json:"resource"`
				Servers  []string `json:"authorization_servers"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Resource != tt.wantResource {
				t.Errorf("resource = %q, want %q", body.Resource, tt.wantResource)
			}
			if len(body.Servers) != 1 || body.Servers[0] != "http://example.com" {
				t.Errorf("authorization_servers = %v", body.Servers)
			}
		})
	}
}

func TestRunAuthHook(t *testing.T) {
	tests := []struct {
		name     string
		provider []string
		want     string
		wantErr  error
	}{
		{"plain token, whitespace trimmed", []string{"bash", "-c", "printf '  tok-1 \\n'"}, "tok-1", nil},
		{"JSON token object", []string{"bash", "-c", `echo '{"token":"tok-json"}'`}, "tok-json", nil},
		{"no provider configured", nil, "", ErrAuthHookMissing},
		{"provider exits non-zero", []string{"bash", "-c", "exit 3"}, "", ErrAuthHookFailed},
		{"provider prints nothing", []string{"bash", "-c", "true"}, "", ErrAuthHookEmpty},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw := &Gateway{oauth: newOAuthStore()}
			got, err := gw.runAuthHook("r", &config.AuthHook{Provider: tt.provider})
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("token = %q, want %q", got, tt.want)
			}
		})
	}
}
