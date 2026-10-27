package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func rr(name string, members ...ResolvedMember) ResolvedRoute {
	return ResolvedRoute{Name: name, Members: members}
}

func mem(server string, cmd ...string) ResolvedMember {
	return ResolvedMember{ServerName: server, Def: BackendDef{Command: cmd}}
}

func TestRouteSignature_StableAcrossEnvKeyOrder(t *testing.T) {
	a := ResolvedRoute{Name: "x", Members: []ResolvedMember{
		{ServerName: "s", Def: BackendDef{Command: []string{"p"}, Env: map[string]string{"A": "1", "B": "2"}}},
	}}
	b := ResolvedRoute{Name: "x", Members: []ResolvedMember{
		{ServerName: "s", Def: BackendDef{Command: []string{"p"}, Env: map[string]string{"B": "2", "A": "1"}}},
	}}
	if routeSignature(a) != routeSignature(b) {
		t.Error("signature should be independent of env map key order")
	}
}

func TestRouteSignature_ChangesOnValueChange(t *testing.T) {
	base := rr("x", mem("s", "python3", "proxy.py"))
	tests := []struct {
		name   string
		mutate func(ResolvedRoute) ResolvedRoute
		differ bool
	}{
		{"same", func(r ResolvedRoute) ResolvedRoute { return r }, false},
		{"command changed", func(r ResolvedRoute) ResolvedRoute {
			return rr("x", mem("s", "python3", "other.py"))
		}, true},
		{"env added", func(r ResolvedRoute) ResolvedRoute {
			m := mem("s", "python3", "proxy.py")
			m.Def.Env = map[string]string{"K": "v"}
			return rr("x", m)
		}, true},
		{"member added", func(r ResolvedRoute) ResolvedRoute {
			return rr("x", mem("s", "python3", "proxy.py"), mem("s2", "python3", "b.py"))
		}, true},
	}
	baseSig := routeSignature(base)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := routeSignature(tt.mutate(base))
			if (got != baseSig) != tt.differ {
				t.Errorf("differ=%v but signatures equal=%v", tt.differ, got == baseSig)
			}
		})
	}
}

func TestPlanReconcile(t *testing.T) {
	// current signatures
	keep := rr("keep", mem("k", "python3", "k.py"))
	chg := rr("chg", mem("c", "python3", "c.py"))
	current := map[string]string{
		"keep": routeSignature(keep),
		"chg":  routeSignature(chg),
		"gone": "deadbeef",
	}
	// desired
	chgNew := rr("chg", mem("c", "python3", "c2.py")) // changed command
	add := rr("new", mem("n", "python3", "n.py"))
	desired := []ResolvedRoute{keep, chgNew, add}

	plan := planReconcile(current, desired)

	if len(plan.add) != 1 || plan.add[0].Name != "new" {
		t.Errorf("expected add=[new], got %v", names(plan.add))
	}
	if len(plan.replace) != 1 || plan.replace[0].Name != "chg" {
		t.Errorf("expected replace=[chg], got %v", names(plan.replace))
	}
	if len(plan.remove) != 1 || plan.remove[0] != "gone" {
		t.Errorf("expected remove=[gone], got %v", plan.remove)
	}
	// keep must not appear anywhere
	for _, r := range append(plan.add, plan.replace...) {
		if r.Name == "keep" {
			t.Error("keep should not be in add/replace")
		}
	}
}

func TestPlanReconcile_Empty(t *testing.T) {
	r := rr("a", mem("a", "python3", "a.py"))
	current := map[string]string{"a": routeSignature(r)}
	plan := planReconcile(current, []ResolvedRoute{r})
	if !plan.empty() {
		t.Errorf("expected empty plan, got add=%d remove=%d replace=%d",
			len(plan.add), len(plan.remove), len(plan.replace))
	}
}

func names(rs []ResolvedRoute) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name
	}
	return out
}

// TestReloadOnce_AddsAndRemovesRoutes exercises the full load+plan+apply path
// against a real backends file that we mutate, asserting routes appear and
// disappear without touching unchanged ones. Backends are never spawned (we
// only touch route registration), so no real subprocess runs.
func TestReloadOnce_AddsAndRemovesRoutes(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	backendsPath := filepath.Join(dir, "backends.json")

	writeJSON(t, cfgPath, map[string]any{
		"listen":        "127.0.0.1:0",
		"backends_file": "backends.json",
		"cache_dir":     dir,
	})
	writeBackends := func(servers map[string]any, routes []string) {
		writeJSON(t, backendsPath, map[string]any{"servers": servers, "backends": routes})
	}
	// initial: one route
	writeBackends(map[string]any{
		"alpha": map[string]any{"command": []string{"true"}},
	}, []string{"alpha"})

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	gw, err := newGateway(cfg)
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}
	gw.configPath = cfgPath

	if _, ok := gw.routes["alpha"]; !ok {
		t.Fatal("expected route alpha at startup")
	}

	// add beta, keep alpha
	writeBackends(map[string]any{
		"alpha": map[string]any{"command": []string{"true"}},
		"beta":  map[string]any{"command": []string{"true"}},
	}, []string{"alpha", "beta"})
	gw.reloadOnce()
	gw.mu.RLock()
	_, hasAlpha := gw.routes["alpha"]
	_, hasBeta := gw.routes["beta"]
	gw.mu.RUnlock()
	if !hasAlpha || !hasBeta {
		t.Fatalf("after add: alpha=%v beta=%v (want both true)", hasAlpha, hasBeta)
	}

	// remove alpha
	writeBackends(map[string]any{
		"beta": map[string]any{"command": []string{"true"}},
	}, []string{"beta"})
	gw.reloadOnce()
	gw.mu.RLock()
	_, hasAlpha = gw.routes["alpha"]
	_, hasBeta = gw.routes["beta"]
	gw.mu.RUnlock()
	if hasAlpha {
		t.Error("after remove: alpha should be gone")
	}
	if !hasBeta {
		t.Error("after remove: beta should remain")
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
