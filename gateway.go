package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// newGateway creates a Gateway with routes and member backends initialized from
// the config. Returns an error if the backend topology fails to resolve
// (dangling route/member references).
func newGateway(cfg Config) (*Gateway, error) {
	resolved, err := resolveRoutes(&CompositionConfig{
		Servers:      cfg.Servers,
		Compositions: cfg.Compositions,
		Backends:     cfg.Routes,
	})
	if err != nil {
		return nil, err
	}

	routeTokens := map[string][]string{}
	gw := &Gateway{
		config:   cfg,
		backends: make(map[string]*Backend),
		routes:   make(map[string]*Route),
	}

	for _, rr := range resolved {
		members := make([]*Backend, 0, len(rr.Members))
		for _, m := range rr.Members {
			instanceName := rr.Name
			if len(rr.Members) > 1 {
				instanceName = rr.Name + "/" + m.ServerName
			}
			b := gw.buildMemberBackend(cfg, instanceName, m)
			gw.backends[instanceName] = b
			members = append(members, b)
			if len(m.Def.AuthTokens) > 0 {
				routeTokens[rr.Name] = m.Def.AuthTokens
			}
		}
		gw.routes[rr.Name] = newRoute(rr.Name, rr.Prefix, members)
	}

	gw.authorizer = NewBearerAuthorizer(cfg.AuthTokens, routeTokens)
	gw.lastRequest = time.Now()
	return gw, nil
}

// buildMemberBackend constructs a *Backend instance for a resolved member.
func (gw *Gateway) buildMemberBackend(cfg Config, instanceName string, m ResolvedMember) *Backend {
	def := m.Def
	def.IncludeTools = m.IncludeTools
	def.ExcludeTools = m.ExcludeTools
	if def.Discovery == nil && cfg.Discovery != nil {
		def.Discovery = cfg.Discovery
	}
	b := &Backend{
		name:          instanceName,
		def:           def,
		globalEnv:     cfg.Env,
		cacheDir:      cfg.CacheDir,
		maxDescLen:    cfg.MaxDescLen,
		pending:       make(map[string]chan json.RawMessage),
		activeTools:   make(map[string]bool),
		logEnabled:    def.LogEnabled == nil || *def.LogEnabled,
		ttlSoftMargin: time.Duration(cfg.CredentialTTLSoftMargin) * time.Second,
		ttlHardGuard:  time.Duration(cfg.CredentialTTLHardGuard) * time.Second,
	}
	if def.URL != "" {
		b.httpClient = &http.Client{Timeout: 120 * time.Second}
		b.ssePending = make(map[string]chan json.RawMessage)
	}
	b.loadToolsCache()
	b.buildCategories()
	return b
}

// selfIdleTimer exits the gateway if no requests have been received for the given duration.
func (gw *Gateway) selfIdleTimer(ctx context.Context, stop context.CancelFunc, timeout time.Duration) {
	interval := timeout / 6
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			gw.reqMu.Lock()
			idle := time.Since(gw.lastRequest)
			gw.reqMu.Unlock()

			if idle > timeout {
				slog.Info("no requests, self-terminating", "idle", idle.Round(time.Second))
				stop() // triggers graceful shutdown
				return
			}
		}
	}
}

// touchLastRequest updates the gateway-level last request timestamp.
func (gw *Gateway) touchLastRequest() {
	gw.reqMu.Lock()
	gw.lastRequest = time.Now()
	gw.reqMu.Unlock()
}

// shutdownAll kills all backend subprocesses.
func (gw *Gateway) shutdownAll() {
	gw.mu.RLock()
	defer gw.mu.RUnlock()

	for _, backend := range gw.backends {
		backend.kill()
	}
}

// idleReaper periodically checks backends and kills those that have been idle too long.
func (gw *Gateway) idleReaper(ctx context.Context, timeout time.Duration) {
	interval := timeout / 10
	if interval < 10*time.Second {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			gw.reapIdleBackends(timeout)
		}
	}
}

// reapIdleBackends kills any backends that have been idle longer than timeout.
func (gw *Gateway) reapIdleBackends(timeout time.Duration) {
	now := time.Now()
	gw.mu.RLock()
	defer gw.mu.RUnlock()

	for _, backend := range gw.backends {
		backend.mu.Lock()
		if backend.running && !backend.lastUsed.IsZero() && now.Sub(backend.lastUsed) > timeout {
			slog.Info("backend idle, killing", "backend", backend.name, "idle", now.Sub(backend.lastUsed).Round(time.Second))
			if backend.cancelFn != nil {
				backend.cancelFn()
			}
			backend.running = false
		}
		backend.mu.Unlock()
	}
}
