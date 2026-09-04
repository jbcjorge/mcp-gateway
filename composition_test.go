package main

import (
	"encoding/json"
	"testing"
)

func TestRouteEntry_UnmarshalStringOrObject(t *testing.T) {
	// Bare string entry -> route with no prefix.
	var s RouteEntry
	if err := json.Unmarshal([]byte(`"wiki"`), &s); err != nil {
		t.Fatalf("string entry: %v", err)
	}
	if s.Route != "wiki" || s.Prefix != "" {
		t.Errorf("string entry parsed wrong: %+v", s)
	}
	// Object entry -> route + prefix.
	var o RouteEntry
	if err := json.Unmarshal([]byte(`{"route":"jira","prefix":"jira_"}`), &o); err != nil {
		t.Fatalf("object entry: %v", err)
	}
	if o.Route != "jira" || o.Prefix != "jira_" {
		t.Errorf("object entry parsed wrong: %+v", o)
	}
}

func newCompositionConfig() *CompositionConfig {
	return &CompositionConfig{
		Servers: map[string]BackendDef{
			"conf-official": {Command: []string{"bash", "official.sh"}},
			"conf-legacy":   {Command: []string{"python3", "legacy.py"}},
			"my-own":        {Command: []string{"python3", "own.py"}},
			"unused":        {Command: []string{"echo", "never"}},
		},
		Compositions: map[string]CompositionDef{
			"wiki": {Members: []MemberDef{
				{Server: "conf-official"},
				{Server: "conf-legacy", IncludeTools: []string{"confluence_remove_label"}},
			}},
			"unused-comp": {Members: []MemberDef{{Server: "unused"}}},
		},
		Backends: []RouteEntry{
			{Route: "wiki"},
			{Route: "my-own"},
			{Route: "conf-official", Prefix: "co_"},
		},
	}
}

func TestResolveRoutes_Success(t *testing.T) {
	cfg := newCompositionConfig()
	routes, err := resolveRoutes(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(routes) != 3 {
		t.Fatalf("expected 3 routes, got %d", len(routes))
	}

	byName := map[string]ResolvedRoute{}
	for _, r := range routes {
		byName[r.Name] = r
	}

	// Composition route: two members in order, second carries include filter.
	wiki, ok := byName["wiki"]
	if !ok {
		t.Fatal("wiki route missing")
	}
	if len(wiki.Members) != 2 {
		t.Fatalf("wiki should have 2 members, got %d", len(wiki.Members))
	}
	if wiki.Members[0].ServerName != "conf-official" || wiki.Members[1].ServerName != "conf-legacy" {
		t.Errorf("wiki member order wrong: %+v", wiki.Members)
	}
	if len(wiki.Members[1].IncludeTools) != 1 || wiki.Members[1].IncludeTools[0] != "confluence_remove_label" {
		t.Errorf("member include filter not applied: %+v", wiki.Members[1])
	}
	if wiki.Prefix != "" {
		t.Errorf("wiki should have no prefix, got %q", wiki.Prefix)
	}

	// Plain server route: single member, no prefix.
	own := byName["my-own"]
	if len(own.Members) != 1 || own.Members[0].ServerName != "my-own" {
		t.Errorf("my-own should be single-member server route: %+v", own)
	}

	// Plain server route WITH prefix.
	co := byName["conf-official"]
	if co.Prefix != "co_" {
		t.Errorf("conf-official route prefix = %q, want co_", co.Prefix)
	}
	if len(co.Members) != 1 || co.Members[0].ServerName != "conf-official" {
		t.Errorf("conf-official route wrong: %+v", co)
	}
}

func TestResolveRoutes_IgnoresUnreferenced(t *testing.T) {
	cfg := newCompositionConfig()
	routes, err := resolveRoutes(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range routes {
		if r.Name == "unused" || r.Name == "unused-comp" {
			t.Errorf("unreferenced definition %q should not be resolved", r.Name)
		}
		for _, m := range r.Members {
			if m.ServerName == "unused" {
				t.Errorf("unreferenced server 'unused' should not be instantiated")
			}
		}
	}
}

func TestResolveRoutes_DanglingRouteRef(t *testing.T) {
	cfg := newCompositionConfig()
	cfg.Backends = append(cfg.Backends, RouteEntry{Route: "does-not-exist"})
	if _, err := resolveRoutes(cfg); err == nil {
		t.Fatal("expected error for dangling route reference")
	}
}

func TestResolveRoutes_DanglingMemberServerRef(t *testing.T) {
	cfg := newCompositionConfig()
	cfg.Compositions["broken"] = CompositionDef{Members: []MemberDef{{Server: "ghost"}}}
	cfg.Backends = append(cfg.Backends, RouteEntry{Route: "broken"})
	if _, err := resolveRoutes(cfg); err == nil {
		t.Fatal("expected error for dangling member server reference")
	}
}

func TestResolveRoutes_ServerReusedInMultipleRoutes(t *testing.T) {
	// Same server template referenced by a composition AND a standalone route.
	// Each use is its own instance (no error, both resolve).
	cfg := newCompositionConfig()
	routes, err := resolveRoutes(cfg)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, r := range routes {
		for _, m := range r.Members {
			if m.ServerName == "conf-official" {
				count++
			}
		}
	}
	// conf-official appears in the wiki composition AND the conf-official route.
	if count != 2 {
		t.Errorf("conf-official should be instantiated twice (2 uses), got %d", count)
	}
}
