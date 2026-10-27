package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Config hot-reload tuning.
const (
	// configWatchInterval is how often the backends file mtime is polled.
	configWatchInterval = 5 * time.Second
	// configWatchFloor is the minimum poll interval.
	configWatchFloor = 2 * time.Second
	// configDebounce is an extra wait after an mtime change before reparsing,
	// to avoid reading a half-written file.
	configDebounce = 1 * time.Second
)

// routeSignature returns a stable hash of a resolved route's topology. Only a
// value change (members, command/args/env/url/headers/params, composition
// shape) flips the signature; map key ordering does not, because the parts are
// sorted and JSON-encoded deterministically.
func routeSignature(rr ResolvedRoute) string {
	type memberSig struct {
		Server  string            `json:"server"`
		Command []string          `json:"command,omitempty"`
		URL     string            `json:"url,omitempty"`
		Env     map[string]string `json:"env,omitempty"`
		Headers map[string]string `json:"headers,omitempty"`
		Include []string          `json:"include,omitempty"`
		Exclude []string          `json:"exclude,omitempty"`
	}
	ms := make([]memberSig, 0, len(rr.Members))
	for _, m := range rr.Members {
		ms = append(ms, memberSig{
			Server:  m.ServerName,
			Command: m.Def.Command,
			URL:     m.Def.URL,
			Env:     m.Def.Env,
			Headers: m.Def.Headers,
			Include: m.IncludeTools,
			Exclude: m.ExcludeTools,
		})
	}
	payload := struct {
		Prefix  string      `json:"prefix"`
		Members []memberSig `json:"members"`
	}{Prefix: rr.Prefix, Members: ms}
	// json.Marshal sorts map keys, giving deterministic output.
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// reconcilePlan is the set of route-level actions to apply.
type reconcilePlan struct {
	add     []ResolvedRoute
	remove  []string
	replace []ResolvedRoute // remove-then-add
}

// empty reports whether the plan has no actions.
func (p reconcilePlan) empty() bool {
	return len(p.add) == 0 && len(p.remove) == 0 && len(p.replace) == 0
}

// planReconcile diffs the desired resolved routes against the current route
// signatures and returns the actions needed. current maps route name ->
// signature of the currently-built route.
func planReconcile(current map[string]string, desired []ResolvedRoute) reconcilePlan {
	var plan reconcilePlan
	seen := make(map[string]bool, len(desired))
	for _, rr := range desired {
		seen[rr.Name] = true
		sig, exists := current[rr.Name]
		switch {
		case !exists:
			plan.add = append(plan.add, rr)
		case sig != routeSignature(rr):
			plan.replace = append(plan.replace, rr)
		}
	}
	for name := range current {
		if !seen[name] {
			plan.remove = append(plan.remove, name)
		}
	}
	sort.Strings(plan.remove)
	return plan
}

// loadResolvedRoutes reads+parses the backends file and resolves it to routes.
// It applies the same CA-bundle env injection as startup so reloaded backends
// get TLS trust.
func (gw *Gateway) loadResolvedRoutes() ([]ResolvedRoute, Config, error) {
	cfg, err := loadConfig(gw.configPath)
	if err != nil {
		return nil, cfg, err
	}
	if caErr := applyCABundleToConfig(&cfg); caErr != nil {
		return nil, cfg, caErr
	}
	resolved, err := resolveRoutes(&CompositionConfig{
		Servers:      cfg.Servers,
		Compositions: cfg.Compositions,
		Backends:     cfg.Routes,
	})
	return resolved, cfg, err
}

// currentSignatures returns the signature of each currently-built route.
// Must be called with gw.mu held (read or write).
func (gw *Gateway) currentSignaturesLocked() map[string]string {
	out := make(map[string]string, len(gw.routeSigs))
	for name, sig := range gw.routeSigs {
		out[name] = sig
	}
	return out
}

// applyReconcile applies a plan under gw.mu, rebuilding the authorizer from the
// resulting route-token set. Returns counts for logging.
func (gw *Gateway) applyReconcile(cfg Config, plan reconcilePlan, desiredByName map[string]ResolvedRoute) {
	gw.mu.Lock()
	defer gw.mu.Unlock()

	for _, name := range plan.remove {
		gw.removeRouteLocked(name)
	}
	for _, rr := range plan.replace {
		gw.removeRouteLocked(rr.Name)
	}
	routeTokens := map[string][]string{}
	// Rebuild token map for all surviving + added routes from desired config.
	for name := range gw.routes {
		if rr, ok := desiredByName[name]; ok {
			collectRouteTokens(rr, routeTokens)
		}
	}
	for _, rr := range plan.add {
		gw.buildRoute(cfg, rr, routeTokens)
		gw.routeSigs[rr.Name] = routeSignature(rr)
	}
	for _, rr := range plan.replace {
		gw.buildRoute(cfg, rr, routeTokens)
		gw.routeSigs[rr.Name] = routeSignature(rr)
	}
	gw.authorizer = NewBearerAuthorizer(cfg.AuthTokens, routeTokens)
}

// removeRouteLocked stops all backends of a route (members and keyed children)
// and deletes it from gw.routes, gw.backends, and gw.routeSigs. Caller holds gw.mu.
func (gw *Gateway) removeRouteLocked(name string) {
	route, ok := gw.routes[name]
	if !ok {
		return
	}
	for _, b := range route.allBackends() {
		b.mu.Lock()
		b.gracefulStopLocked()
		b.mu.Unlock()
		delete(gw.backends, b.name)
	}
	delete(gw.routes, name)
	delete(gw.routeSigs, name)
	slog.Info("route removed by hot-reload", "route", name)
}

// collectRouteTokens records a route's per-member auth tokens into routeTokens.
func collectRouteTokens(rr ResolvedRoute, routeTokens map[string][]string) {
	for _, m := range rr.Members {
		if len(m.Def.AuthTokens) > 0 {
			routeTokens[rr.Name] = m.Def.AuthTokens
		}
	}
}

// configWatcher polls the backends file and reconciles routes on change. It
// runs until ctx is cancelled. Read/parse errors are logged and skipped, so a
// transient bad write never tears down the running topology.
func (gw *Gateway) configWatcher(ctx context.Context, interval time.Duration) {
	if interval < configWatchFloor {
		interval = configWatchFloor
	}
	path := gw.backendsFilePath()
	if path == "" {
		slog.Debug("config watcher disabled: no backends file")
		return
	}
	last := fileModTime(path)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	slog.Info("config hot-reload watcher enabled", "file", path, "interval", interval.String())

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		mod := fileModTime(path)
		if mod.Equal(last) || mod.IsZero() {
			continue
		}
		// Debounce: wait one short interval and re-stat to avoid a partial write.
		select {
		case <-ctx.Done():
			return
		case <-time.After(configDebounce):
		}
		settled := fileModTime(path)
		last = settled
		gw.reloadOnce()
	}
}

// reloadOnce performs one load+plan+apply cycle. Safe to call standalone (tests).
func (gw *Gateway) reloadOnce() {
	resolved, cfg, err := gw.loadResolvedRoutes()
	if err != nil {
		slog.Warn("config hot-reload skipped: load failed (keeping current topology)", "error", err)
		return
	}
	desiredByName := make(map[string]ResolvedRoute, len(resolved))
	for _, rr := range resolved {
		desiredByName[rr.Name] = rr
	}
	gw.mu.RLock()
	plan := planReconcile(gw.currentSignaturesLocked(), resolved)
	gw.mu.RUnlock()
	if plan.empty() {
		return
	}
	gw.applyReconcile(cfg, plan, desiredByName)
	slog.Info("config hot-reload applied",
		"added", len(plan.add), "removed", len(plan.remove), "replaced", len(plan.replace))
}

// fileModTime returns the file's mtime, or the zero time if it cannot be statted.
func fileModTime(path string) time.Time {
	fi, err := os.Stat(path) // #nosec G304 -- admin-controlled config path
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// backendsFilePath resolves the backends topology file path relative to the
// main config path, mirroring loadConfig. Returns "" when the topology is
// inlined in the config (nothing separate to watch).
func (gw *Gateway) backendsFilePath() string {
	if gw.config.BackendsFile == "" || gw.configPath == "" {
		return ""
	}
	p := gw.config.BackendsFile
	if !filepath.IsAbs(p) {
		p = filepath.Join(filepath.Dir(gw.configPath), p)
	}
	return p
}
