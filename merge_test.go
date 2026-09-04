package main

import (
	"encoding/json"
	"testing"
)

// helper to build a tools/list-style result payload for a member.
func toolsResult(names ...string) json.RawMessage {
	items := make([]map[string]any, 0, len(names))
	for _, n := range names {
		items = append(items, map[string]any{"name": n, "description": "d-" + n})
	}
	b, _ := json.Marshal(map[string]any{"tools": items})
	return b
}

func TestMergeLists_UnionNoCollision(t *testing.T) {
	members := []memberList{
		{member: "a", result: toolsResult("alpha", "beta")},
		{member: "b", result: toolsResult("gamma")},
	}
	merged, owners, err := mergeLists(members, "tools", "name", "")
	if err != nil {
		t.Fatal(err)
	}
	names := listItemNames(t, merged, "tools", "name")
	if len(names) != 3 {
		t.Fatalf("expected 3 items, got %d (%v)", len(names), names)
	}
	if owners["alpha"].member != "a" || owners["gamma"].member != "b" {
		t.Errorf("ownership wrong: %+v", owners)
	}
	if owners["alpha"].realName != "alpha" {
		t.Errorf("realName should be unprefixed: %+v", owners["alpha"])
	}
}

func TestMergeLists_LastWinsOnCollision(t *testing.T) {
	members := []memberList{
		{member: "official", result: toolsResult("search", "get")},
		{member: "override", result: toolsResult("search")}, // shadows official's search
	}
	merged, owners, err := mergeLists(members, "tools", "name", "")
	if err != nil {
		t.Fatal(err)
	}
	names := listItemNames(t, merged, "tools", "name")
	// search + get, deduped: 2 items.
	if len(names) != 2 {
		t.Fatalf("expected 2 items after dedupe, got %d (%v)", len(names), names)
	}
	if owners["search"].member != "override" {
		t.Errorf("last member should own 'search', got %q", owners["search"].member)
	}
}

func TestMergeLists_AppliesPrefix(t *testing.T) {
	members := []memberList{
		{member: "a", result: toolsResult("search")},
	}
	merged, owners, err := mergeLists(members, "tools", "name", "a_")
	if err != nil {
		t.Fatal(err)
	}
	names := listItemNames(t, merged, "tools", "name")
	if len(names) != 1 || names[0] != "a_search" {
		t.Fatalf("expected prefixed name a_search, got %v", names)
	}
	// ownership keyed by DISPLAY (prefixed) name, mapping to real name.
	o, ok := owners["a_search"]
	if !ok {
		t.Fatal("owner map should key by prefixed display name")
	}
	if o.member != "a" || o.realName != "search" {
		t.Errorf("owner should map a_search -> (a, search), got %+v", o)
	}
}

func TestMergeLists_PrefixWithCollisionOverride(t *testing.T) {
	// Override happens BEFORE prefix: same real name across members dedupes, then
	// the surviving one is prefixed.
	members := []memberList{
		{member: "official", result: toolsResult("search")},
		{member: "override", result: toolsResult("search")},
	}
	merged, owners, err := mergeLists(members, "tools", "name", "w_")
	if err != nil {
		t.Fatal(err)
	}
	names := listItemNames(t, merged, "tools", "name")
	if len(names) != 1 || names[0] != "w_search" {
		t.Fatalf("expected [w_search], got %v", names)
	}
	if owners["w_search"].member != "override" || owners["w_search"].realName != "search" {
		t.Errorf("owner wrong: %+v", owners["w_search"])
	}
}

// listItemNames extracts the item key values from a merged result payload.
func listItemNames(t *testing.T, raw json.RawMessage, listKey, itemKey string) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	var items []map[string]any
	if err := json.Unmarshal(m[listKey], &items); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, it := range items {
		names = append(names, it[itemKey].(string))
	}
	return names
}
