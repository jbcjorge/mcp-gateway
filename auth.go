package main

import (
	"crypto/subtle"
	"net/http"
	"strings"

	errors "github.com/jbcjorge/errors-library"
)

// Authorizer validates whether an incoming request is permitted to access
// a given backend and tool. Implementations can range from simple token
// matching (BearerAuthorizer) to full OIDC JWT validation with role-based
// access control (future OIDCAuthorizer).
type Authorizer interface {
	// Authorize checks whether the request is allowed.
	// Returns nil if authorized, or an error describing why access was denied.
	Authorize(r *http.Request, backend string) error
}

// BearerAuthorizer validates requests against a set of allowed bearer tokens.
// Tokens are checked using constant-time comparison to prevent timing attacks.
//
// Resolution order:
//  1. If the backend has its own auth_tokens, use those exclusively.
//  2. Otherwise, fall back to the global auth_tokens.
//  3. If no tokens are configured (neither backend nor global), all requests are allowed.
type BearerAuthorizer struct {
	globalTokens  []string
	backendTokens map[string][]string // backend name -> allowed tokens
}

// NewBearerAuthorizer creates a BearerAuthorizer from config.
func NewBearerAuthorizer(globalTokens []string, backends map[string]BackendDef) *BearerAuthorizer {
	bt := make(map[string][]string)
	for name, def := range backends {
		if len(def.AuthTokens) > 0 {
			bt[name] = def.AuthTokens
		}
	}
	return &BearerAuthorizer{
		globalTokens:  globalTokens,
		backendTokens: bt,
	}
}

// Authorize checks the Authorization header against the allowed tokens.
func (a *BearerAuthorizer) Authorize(r *http.Request, backend string) error {
	tokens := a.tokensForBackend(backend)
	if len(tokens) == 0 {
		return nil // no tokens configured, open access
	}

	token := extractBearerToken(r)
	if token == "" {
		return ErrAuthRequired.Parse(errors.WithSafeData(map[string]any{"backend": backend}))
	}

	for _, allowed := range tokens {
		if subtle.ConstantTimeCompare([]byte(token), []byte(allowed)) == 1 {
			return nil
		}
	}

	return ErrInvalidToken.Parse(errors.WithSafeData(map[string]any{"backend": backend}))
}

// tokensForBackend returns the effective token set for a backend.
func (a *BearerAuthorizer) tokensForBackend(backend string) []string {
	if tokens, ok := a.backendTokens[backend]; ok {
		return tokens
	}
	return a.globalTokens
}

// IsEnabled returns true if any tokens are configured (global or per-backend).
func (a *BearerAuthorizer) IsEnabled() bool {
	if len(a.globalTokens) > 0 {
		return true
	}
	for _, tokens := range a.backendTokens {
		if len(tokens) > 0 {
			return true
		}
	}
	return false
}

// extractBearerToken pulls the token from the Authorization header.
func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	const prefix = "Bearer "
	if len(auth) < len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return ""
	}
	return auth[len(prefix):]
}
