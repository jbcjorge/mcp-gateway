package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// OAuth broker. The gateway acts as an OAuth 2.0 authorization server (code +
// PKCE) for routes that declare an `auth` hook. The client authenticates here;
// the gateway runs the route's provider command to obtain the upstream token.
// See docs/design/auth-broker.md. This is NOT multi-user access control.

const (
	authCodeTTL       = 2 * time.Minute   // lifetime of an issued authorization code
	defaultTokenTTL   = 2 * time.Hour     // fallback access-token validity when unknown
	oauthProviderWait = 180 * time.Second // max time to run a provider hook
)

// authCode is a one-time authorization code bound to a PKCE challenge, the
// resolved route, and the client's redirect/state. It also carries the token
// already obtained by running the hook once at /authorize, so /token does not
// run the (possibly interactive) provider a second time.
type authCode struct {
	route         string
	codeChallenge string
	redirectURI   string
	token         string
	tokenExpiry   time.Time
	expiresAt     time.Time
}

// oauthStore holds pending authorization codes only. The gateway is token-
// stateless: with expose=real the access_token IS the upstream token, so there
// is nothing to store between requests (the backend/upstream validates it).
// Only the brief authorization-code -> token exchange needs transient state.
type oauthStore struct {
	mu    sync.Mutex
	codes map[string]authCode
}

func newOAuthStore() *oauthStore {
	return &oauthStore{codes: map[string]authCode{}}
}

// randToken returns a URL-safe random string for codes/tokens.
func randToken(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// verifyPKCE checks that S256(verifier) == challenge.
func verifyPKCE(verifier, challenge string) bool {
	sum := sha256.Sum256([]byte(verifier))
	calc := base64.RawURLEncoding.EncodeToString(sum[:])
	return calc == challenge
}

// routeFromResource extracts the route name from an OAuth `resource` param of
// the form http://host:port/<route>/mcp (or /<route>). Returns "" if unparseable.
func routeFromResource(resource string) string {
	u, err := url.Parse(resource)
	if err != nil {
		return ""
	}
	p := strings.TrimPrefix(u.Path, "/")
	p = strings.TrimSuffix(p, "/") // drop any trailing slash first
	p = strings.TrimSuffix(p, "/mcp")
	p = strings.TrimSuffix(p, "/")
	return p
}

// authHookFor returns the AuthHook declared for a route, or nil if the route is
// unknown or declares no hook.
func (gw *Gateway) authHookFor(routeName string) *AuthHook {
	def, ok := gw.config.Servers[routeName]
	if !ok {
		return nil
	}
	return def.Auth
}

// handleOAuth dispatches the gateway's OAuth-broker paths. Returns true if the
// request was an OAuth path (handled here), false to fall through to MCP routing.
func (gw *Gateway) handleOAuth(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case r.URL.Path == "/.well-known/oauth-authorization-server":
		gw.handleAuthServerMetadata(w, r)
	case strings.HasPrefix(r.URL.Path, "/.well-known/oauth-protected-resource"):
		gw.handleProtectedResourceMetadata(w, r)
	case r.URL.Path == "/authorize":
		gw.handleAuthorize(w, r)
	case r.URL.Path == "/token":
		gw.handleToken(w, r)
	default:
		return false
	}
	return true
}

func writeJSONResponse(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// handleAuthServerMetadata advertises the gateway's OAuth endpoints.
func (gw *Gateway) handleAuthServerMetadata(w http.ResponseWriter, r *http.Request) {
	base := gatewayBaseURL(r)
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/authorize",
		"token_endpoint":                        base + "/token",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
	})
}

// handleProtectedResourceMetadata returns the protected-resource metadata for
// the MCP resource the client is asking about. Per the MCP auth spec the client
// probes /.well-known/oauth-protected-resource/<resource-path> (e.g.
// /.well-known/oauth-protected-resource/anyorg/mcp); we echo that exact resource
// URL back so the client uses it as the `resource` in /authorize, which
// routeFromResource then resolves to a route. With no resource path we fall
// back to the gateway base (generic metadata).
func (gw *Gateway) handleProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	base := gatewayBaseURL(r)
	resource := base
	const prefix = "/.well-known/oauth-protected-resource"
	if suffix := strings.TrimPrefix(r.URL.Path, prefix); suffix != "" && suffix != "/" {
		// suffix is like "/anyorg/mcp" -> resource is base + that path.
		resource = base + suffix
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"resource":              resource,
		"authorization_servers": []string{base},
	})
	slog.Debug("served protected-resource metadata", "path", r.URL.Path, "resource", resource)
}

// isLoopbackRedirect reports whether a redirect_uri is an http loopback URL
// (127.0.0.1 or localhost), the only form OAuth MCP clients use. Restricting to
// loopback closes the open-redirect vector.
func isLoopbackRedirect(redirectURI string) bool {
	u, err := url.Parse(redirectURI)
	if err != nil || u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// gatewayBaseURL reconstructs the gateway's own base URL from the request.
func gatewayBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// handleAuthorize validates the authorization request, resolves the resource to
// a route with an auth hook, runs the hook to prime the upstream token, issues a
// one-time code, and 302-redirects to the client's redirect_uri.
func (gw *Gateway) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	slog.Debug("oauth authorize request", "resource", q.Get("resource"), "client_id", q.Get("client_id"))
	redirectURI := q.Get("redirect_uri")
	state := q.Get("state")
	if redirectURI == "" {
		http.Error(w, "missing redirect_uri", http.StatusBadRequest)
		return
	}
	// Security: only allow loopback http redirect URIs (what MCP clients use).
	// This closes the open-redirect vector: we never redirect to an arbitrary
	// caller-supplied host. Reject before any redirect occurs.
	if !isLoopbackRedirect(redirectURI) {
		http.Error(w, "invalid redirect_uri (loopback http only)", http.StatusBadRequest)
		return
	}
	if q.Get("response_type") != "code" {
		redirectErr(w, r, redirectURI, state, "unsupported_response_type")
		return
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		redirectErr(w, r, redirectURI, state, "invalid_request")
		return
	}
	routeName := routeFromResource(q.Get("resource"))
	hook := gw.authHookFor(routeName)
	if routeName == "" || hook == nil {
		// Not an OAuth-broker backend: nothing to authorize against.
		redirectErr(w, r, redirectURI, state, "invalid_target")
		return
	}
	if strings.EqualFold(hook.Expose, "opaque") {
		// Only expose=real is implemented today; opaque (gateway-held token with
		// an opaque handle to the client) is a declared-but-future mode. Serve
		// real and say so, rather than silently diverging from the config.
		slog.Warn("auth hook requested expose=opaque which is not implemented; serving real token", "route", routeName)
	}

	// Run the hook now so failures surface as an OAuth error rather than later,
	// and reuse its result at /token (do not run the provider twice).
	upstream, err := gw.runAuthHook(routeName, hook)
	if err != nil {
		slog.Warn("auth hook failed during authorize", "route", routeName, "error", err)
		redirectErr(w, r, redirectURI, state, "access_denied")
		return
	}

	code := randToken(32)
	gw.oauth.mu.Lock()
	gw.oauth.codes[code] = authCode{
		route:         routeName,
		codeChallenge: q.Get("code_challenge"),
		redirectURI:   redirectURI,
		token:         upstream,
		tokenExpiry:   hookTokenExpiry(upstream, hook),
		expiresAt:     time.Now().Add(authCodeTTL),
	}
	gw.oauth.mu.Unlock()

	sep := "?"
	if strings.Contains(redirectURI, "?") {
		sep = "&"
	}
	loc := redirectURI + sep + "code=" + url.QueryEscape(code)
	if state != "" {
		loc += "&state=" + url.QueryEscape(state)
	}
	slog.Info("oauth authorize issued code", "route", routeName)
	// #nosec G710 -- redirectURI validated as http loopback by isLoopbackRedirect above
	http.Redirect(w, r, loc, http.StatusFound)
}

// redirectErr 302-redirects an OAuth error back to the client (or 400 if the URI
// is not a safe loopback target).
func redirectErr(w http.ResponseWriter, r *http.Request, redirectURI, state, errCode string) {
	if !isLoopbackRedirect(redirectURI) {
		http.Error(w, errCode, http.StatusBadRequest)
		return
	}
	sep := "?"
	if strings.Contains(redirectURI, "?") {
		sep = "&"
	}
	loc := redirectURI + sep + "error=" + url.QueryEscape(errCode)
	if state != "" {
		loc += "&state=" + url.QueryEscape(state)
	}
	// #nosec G710 -- redirectURI validated as http loopback by isLoopbackRedirect
	http.Redirect(w, r, loc, http.StatusFound)
}

// handleToken exchanges an authorization code (or refresh token) for an access
// token, verifying PKCE for the code grant.
func (gw *Gateway) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONResponse(w, http.StatusMethodNotAllowed, map[string]string{"error": "invalid_request"})
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		gw.tokenFromCode(w, r)
	case "refresh_token":
		gw.tokenFromRefresh(w, r)
	default:
		writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
	}
}

func (gw *Gateway) tokenFromCode(w http.ResponseWriter, r *http.Request) {
	code := r.PostForm.Get("code")
	verifier := r.PostForm.Get("code_verifier")
	redirectURI := r.PostForm.Get("redirect_uri")

	gw.oauth.mu.Lock()
	ac, ok := gw.oauth.codes[code]
	if ok {
		delete(gw.oauth.codes, code) // one-time use
	}
	gw.oauth.mu.Unlock()

	if !ok || time.Now().After(ac.expiresAt) {
		writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}
	if !verifyPKCE(verifier, ac.codeChallenge) {
		writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}
	// #4: redirect_uri, when sent to /token, must match the one bound at /authorize.
	if redirectURI != "" && redirectURI != ac.redirectURI {
		writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}
	hook := gw.authHookFor(ac.route)
	if hook == nil {
		writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid_target"})
		return
	}
	// #1: reuse the token obtained at /authorize; do not run the hook again.
	gw.mintToken(w, ac.route, hook, ac.token, ac.tokenExpiry)
}

func (gw *Gateway) tokenFromRefresh(w http.ResponseWriter, r *http.Request) {
	refresh := r.PostForm.Get("refresh_token")
	// Refresh tokens encode their route as "gwr_<route>_<random>" so the gateway
	// stays token-stateless: it re-runs that route's hook for a fresh token.
	route := routeFromRefresh(refresh)
	if route == "" {
		writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}
	hook := gw.authHookFor(route)
	if hook == nil {
		writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid_target"})
		return
	}
	upstream, err := gw.runAuthHook(route, hook)
	if err != nil {
		writeJSONResponse(w, http.StatusBadGateway, map[string]string{"error": "access_denied"})
		return
	}
	gw.mintToken(w, route, hook, upstream, hookTokenExpiry(upstream, hook))
}

// routeFromRefresh extracts the route from a gateway refresh token of the form
// "gwr_<route>_<random>". Returns "" if malformed.
func routeFromRefresh(refresh string) string {
	const prefix = "gwr_"
	if !strings.HasPrefix(refresh, prefix) {
		return ""
	}
	rest := refresh[len(prefix):]
	i := strings.LastIndex(rest, "_")
	if i <= 0 {
		return ""
	}
	return rest[:i]
}

// mintToken returns the real upstream token to the client as access_token (the
// gateway is token-stateless: expose=real only). The refresh token encodes the
// route so refresh can re-run the hook without stored state. Prunes expired
// codes opportunistically.
func (gw *Gateway) mintToken(w http.ResponseWriter, routeName string, hook *AuthHook, upstream string, expiry time.Time) {
	gw.oauth.mu.Lock()
	gw.pruneExpiredLocked()
	gw.oauth.mu.Unlock()

	refresh := "gwr_" + routeName + "_" + randToken(24)
	ttl := int(time.Until(expiry).Seconds())
	if ttl < 1 {
		ttl = int(defaultTokenTTL.Seconds())
	}
	slog.Info("oauth token issued", "route", routeName, "expires_in", ttl)
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"access_token":  upstream,
		"token_type":    "Bearer",
		"expires_in":    ttl,
		"refresh_token": refresh,
	})
}

// pruneExpiredLocked removes expired authorization codes. Caller holds oauth.mu.
func (gw *Gateway) pruneExpiredLocked() {
	now := time.Now()
	for c, ac := range gw.oauth.codes {
		if now.After(ac.expiresAt) {
			delete(gw.oauth.codes, c)
		}
	}
}

// runAuthHook executes a route's provider command and returns the token it
// prints. Accepts a bare token or JSON {"token":...}. Non-zero exit is an error.
func (gw *Gateway) runAuthHook(routeName string, hook *AuthHook) (string, error) {
	if len(hook.Provider) == 0 {
		return "", ErrAuthHookMissing.Parse()
	}
	ctx, cancel := context.WithTimeout(context.Background(), oauthProviderWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, hook.Provider[0], hook.Provider[1:]...) // #nosec G204 -- provider from admin config
	// Inherit the gateway env (incl. injected CA bundle vars) plus global env.
	cmd.Env = os.Environ()
	for k, v := range gw.config.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.Output()
	if err != nil {
		return "", ErrAuthHookFailed.Parse()
	}
	token := strings.TrimSpace(string(out))
	// Allow JSON {"token":"..."} output.
	if strings.HasPrefix(token, "{") {
		var obj struct {
			Token string `json:"token"`
		}
		if json.Unmarshal([]byte(token), &obj) == nil && obj.Token != "" {
			token = obj.Token
		}
	}
	if token == "" {
		return "", ErrAuthHookEmpty.Parse()
	}
	return token, nil
}

// hookTokenExpiry computes the token expiry from a JWT exp claim (token_format
// "jwt", default) or from the hook's TTL / the global default.
func hookTokenExpiry(token string, hook *AuthHook) time.Time {
	if !strings.EqualFold(hook.TokenFormat, "opaque") {
		if exp := jwtExp(token); exp > 0 {
			return time.Unix(exp, 0)
		}
	}
	ttl := defaultTokenTTL
	if hook.TTLSeconds > 0 {
		ttl = time.Duration(hook.TTLSeconds) * time.Second
	}
	return time.Now().Add(ttl)
}

// jwtExp reads the exp claim (unix seconds) from a JWT, or 0 if unavailable.
func jwtExp(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return 0
	}
	return claims.Exp
}
