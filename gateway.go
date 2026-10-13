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
		gw.buildRoute(cfg, rr, routeTokens)
	}

	gw.authorizer = NewBearerAuthorizer(cfg.AuthTokens, routeTokens)
	gw.lastRequest = time.Now()
	return gw, nil
}

// buildRoute resolves a single route into either a keyed route or a normal/
// composite route of member backends, registering members in gw.backends and
// the route in gw.routes, and recording any route-level auth tokens.
func (gw *Gateway) buildRoute(cfg Config, rr ResolvedRoute, routeTokens map[string][]string) {
	// Keyed route: single server template with declared params. No eager
	// member; children are spawned per instance id on demand.
	if len(rr.Members) == 1 && rr.Members[0].Def.isKeyed() {
		gw.routes[rr.Name] = gw.buildKeyedRoute(cfg, rr)
		if len(rr.Members[0].Def.AuthTokens) > 0 {
			routeTokens[rr.Name] = rr.Members[0].Def.AuthTokens
		}
		return
	}
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

// resolveStopGrace determines the SIGTERM-to-SIGKILL grace for a backend:
// the per-server override wins, then the global config value, then the built-in
// default. A configured value of 0 at either level means "use the next level
// down" rather than "no grace", to avoid an accidental immediate SIGKILL.
func resolveStopGrace(perServer *int, global int) time.Duration {
	if perServer != nil && *perServer > 0 {
		return time.Duration(*perServer) * time.Second
	}
	if global > 0 {
		return time.Duration(global) * time.Second
	}
	return defaultStopGrace
}

// Idle-timeout sentinels for the resolved per-backend idleTimeout field.
// The zero value MUST mean "inherit global" so that any Backend not built
// through buildMemberBackend (e.g. in tests) defaults to the global behavior
// rather than silently never being reaped.
const (
	inheritGlobalIdle time.Duration = 0  // no per-backend override; use the global timeout
	neverReapIdle     time.Duration = -1 // explicit per-backend "never reap this backend"
)

// resolveIdleTimeout determines a backend's idle timeout from its optional
// per-server override. Unlike resolveStopGrace, a per-server value of 0 is
// meaningful here: it means "never reap this backend". The distinction is:
//   - nil      -> inherit the global timeout (returns inheritGlobalIdle, the zero value)
//   - 0        -> never reap this backend (returns neverReapIdle)
//   - positive -> reap after that many seconds
func resolveIdleTimeout(perServer *int) time.Duration {
	if perServer == nil {
		return inheritGlobalIdle
	}
	if *perServer <= 0 {
		return neverReapIdle
	}
	return time.Duration(*perServer) * time.Second
}

// effectiveIdleTimeout combines a backend's resolved per-backend idle timeout
// with the global timeout to produce the duration the reaper should enforce.
// A return of 0 means "never reap". global is the global idle timeout
// (0 = never reap globally).
func effectiveIdleTimeout(perBackend time.Duration, global time.Duration) time.Duration {
	if perBackend == inheritGlobalIdle {
		return global
	}
	return perBackend
}

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
		stopGrace:     resolveStopGrace(def.StopGraceSeconds, cfg.StopGraceSeconds),
		idleTimeout:   resolveIdleTimeout(def.IdleTimeoutSeconds),
	}
	if def.URL != "" {
		b.httpClient = &http.Client{Timeout: 120 * time.Second}
		b.ssePending = make(map[string]chan json.RawMessage)
	}
	b.loadToolsCache()
	b.buildCategories()
	return b
}

// buildKeyedRoute constructs a keyed Route from a resolved single-member route
// whose server template declares params. The route builds one child *Backend
// per instance id on demand (via buildKeyedChild), registering each in
// gw.backends so the reaper/TTL/health/shutdown apply per child.
func (gw *Gateway) buildKeyedRoute(cfg Config, rr ResolvedRoute) *Route {
	m := rr.Members[0]
	buildChild := func(id string, env map[string]string) *Backend {
		return gw.buildKeyedChild(cfg, rr.Name, id, m, env)
	}
	register := func(name string, b *Backend) {
		gw.mu.Lock()
		gw.backends[name] = b
		gw.mu.Unlock()
	}
	unregister := func(name string) {
		gw.mu.Lock()
		delete(gw.backends, name)
		gw.mu.Unlock()
	}
	return newKeyedRoute(rr.Name, m.Def, buildChild, register, unregister)
}

// buildKeyedChild constructs (but does not start) a child *Backend for a keyed
// route instance. It clones the server template's env and overlays the per-call
// param values (keyed by each param's child env name), so the child launcher
// receives them as environment variables. The child's name is "<route>/<id>".
func (gw *Gateway) buildKeyedChild(cfg Config, route, id string, m ResolvedMember, env map[string]string) *Backend {
	def := m.Def
	def.IncludeTools = m.IncludeTools
	def.ExcludeTools = m.ExcludeTools
	if def.Discovery == nil && cfg.Discovery != nil {
		def.Discovery = cfg.Discovery
	}
	// Clone env so sibling children never share a map, then overlay param values.
	childEnv := make(map[string]string, len(def.Env)+len(env))
	for k, v := range def.Env {
		childEnv[k] = v
	}
	for k, v := range env {
		childEnv[k] = v
	}
	def.Env = childEnv

	b := &Backend{
		name:          childInstanceName(route, id),
		def:           def,
		globalEnv:     cfg.Env,
		cacheDir:      cfg.CacheDir,
		maxDescLen:    cfg.MaxDescLen,
		pending:       make(map[string]chan json.RawMessage),
		activeTools:   make(map[string]bool),
		logEnabled:    def.LogEnabled == nil || *def.LogEnabled,
		ttlSoftMargin: time.Duration(cfg.CredentialTTLSoftMargin) * time.Second,
		ttlHardGuard:  time.Duration(cfg.CredentialTTLHardGuard) * time.Second,
		stopGrace:     resolveStopGrace(def.StopGraceSeconds, cfg.StopGraceSeconds),
		idleTimeout:   resolveIdleTimeout(def.IdleTimeoutSeconds),
	}
	// Keyed children share the route's tool-cache file (identical tool surface
	// per instance); load it so tools/list can be served without spawning.
	b.cacheName = route
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
// The global timeout may be 0 (no global reaping) while per-backend timeouts still
// exist. In that case interval is derived from the smallest per-backend timeout.
func (gw *Gateway) idleReaper(ctx context.Context, globalTimeout time.Duration) {
	interval := gw.reaperInterval(globalTimeout)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			gw.reapIdleBackends(globalTimeout)
		}
	}
}

// minReaperInterval is the floor for the reaper check interval.
const minReaperInterval = 10 * time.Second

// reaperInterval computes the reaper tick period. When a global timeout is set,
// the interval is timeout/10 (floored at 10s). When no global is set (0) but
// per-backend timeouts exist, it falls back to the smallest per-backend timeout
// divided by 10 (floored at 10s).
func (gw *Gateway) reaperInterval(globalTimeout time.Duration) time.Duration {
	base := globalTimeout
	if base <= 0 {
		base = gw.minBackendIdleTimeout()
	}
	interval := base / 10
	if interval < minReaperInterval {
		interval = minReaperInterval
	}
	return interval
}

// minBackendIdleTimeout returns the smallest positive idleTimeout among all
// backends, or 0 if none have a per-backend override.
func (gw *Gateway) minBackendIdleTimeout() time.Duration {
	gw.mu.RLock()
	defer gw.mu.RUnlock()

	var min time.Duration
	for _, b := range gw.backends {
		t := b.idleTimeout
		if t > 0 && (min == 0 || t < min) {
			min = t
		}
	}
	return min
}

// reapIdleBackends kills any backends that have been idle longer than their
// effective idle timeout. The per-backend override (if any) takes precedence
// over the global timeout; an effective timeout of 0 means "never reap".
func (gw *Gateway) reapIdleBackends(globalTimeout time.Duration) {
	now := time.Now()
	gw.mu.RLock()
	var reapedKeyed []string
	for _, backend := range gw.backends {
		backend.mu.Lock()
		if backend.shouldReapLocked(now, globalTimeout) {
			idle := now.Sub(backend.lastUsed).Round(time.Second)
			slog.Info("backend idle, killing", "backend", backend.name, "idle", idle)
			backend.gracefulStopLocked()
			reapedKeyed = append(reapedKeyed, backend.name)
		}
		backend.mu.Unlock()
	}
	gw.mu.RUnlock()

	// Drop reaped keyed children from their route pools (and gw.backends) so a
	// later call respawns a fresh instance. Non-keyed names are ignored.
	for _, name := range reapedKeyed {
		gw.dropKeyedChildByName(name)
	}
}

// dropKeyedChildByName locates the keyed route owning the given child instance
// name ("<route>/<id>") and removes the child from its pool and gw.backends.
// No-op for names that are not keyed children.
func (gw *Gateway) dropKeyedChildByName(name string) {
	gw.mu.RLock()
	route, id, ok := gw.keyedOwnerOf(name)
	gw.mu.RUnlock()
	if ok {
		route.dropChild(id)
	}
}

// keyedOwnerOf returns the keyed route and instance id for a child instance name
// "<route>/<id>". Must be called with gw.mu held (read).
func (gw *Gateway) keyedOwnerOf(name string) (*Route, string, bool) {
	for routeName, route := range gw.routes {
		if !route.isKeyed() {
			continue
		}
		if isChildInstanceName(routeName, name) {
			return route, name[len(routeName)+1:], true
		}
	}
	return nil, "", false
}

// shouldReapLocked reports whether the backend has been idle long enough to be
// reaped, honoring its per-backend idle timeout override. Must be called with
// b.mu held.
func (b *Backend) shouldReapLocked(now time.Time, globalTimeout time.Duration) bool {
	if !b.running || b.lastUsed.IsZero() {
		return false
	}
	eff := effectiveIdleTimeout(b.idleTimeout, globalTimeout)
	if eff <= 0 {
		return false // never reap
	}
	return now.Sub(b.lastUsed) > eff
}
