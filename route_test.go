package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func stubBackend(name string) *Backend { return &Backend{name: name} }

func TestRoute_ToolsListMergedNoPrefix(t *testing.T) {
	rt := newRoute("wiki", "", []*Backend{stubBackend("official"), stubBackend("legacy")})
	lists := map[string]json.RawMessage{
		"official": toolsResult("search", "get_page"),
		"legacy":   toolsResult("remove_label"),
	}
	merged, err := rt.toolsList(func(b *Backend) (json.RawMessage, error) { return lists[b.name], nil })
	if err != nil {
		t.Fatal(err)
	}
	names := listItemNames(t, merged, "tools", "name")
	if len(names) != 3 {
		t.Fatalf("expected 3 tools, got %v", names)
	}
	m, real, ok := rt.ownerOfTool("remove_label")
	if !ok || m.name != "legacy" || real != "remove_label" {
		t.Errorf("owner lookup wrong: ok=%v m=%v real=%q", ok, m, real)
	}
}

func TestRoute_LastWinsOverride(t *testing.T) {
	rt := newRoute("wiki", "", []*Backend{stubBackend("official"), stubBackend("override")})
	lists := map[string]json.RawMessage{
		"official": toolsResult("search"),
		"override": toolsResult("search"),
	}
	if _, err := rt.toolsList(func(b *Backend) (json.RawMessage, error) { return lists[b.name], nil }); err != nil {
		t.Fatal(err)
	}
	m, _, ok := rt.ownerOfTool("search")
	if !ok || m.name != "override" {
		t.Errorf("override member should own 'search', got %v", m)
	}
}

func TestRoute_PrefixOwnerLookupAndRewrite(t *testing.T) {
	rt := newRoute("jira", "jira_", []*Backend{stubBackend("jira")})
	lists := map[string]json.RawMessage{"jira": toolsResult("get_issue")}
	merged, err := rt.toolsList(func(b *Backend) (json.RawMessage, error) { return lists[b.name], nil })
	if err != nil {
		t.Fatal(err)
	}
	names := listItemNames(t, merged, "tools", "name")
	if len(names) != 1 || names[0] != "jira_get_issue" {
		t.Fatalf("expected [jira_get_issue], got %v", names)
	}
	// Client calls the prefixed name; owner lookup maps to real name.
	m, real, ok := rt.ownerOfTool("jira_get_issue")
	if !ok || m.name != "jira" || real != "get_issue" {
		t.Errorf("owner lookup wrong: m=%v real=%q ok=%v", m, real, ok)
	}
	// Rewriting the call body strips the prefix to the member's real name.
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"jira_get_issue","arguments":{"key":"X-1"}}}`)
	rewritten, err := rewriteToolCallName(body, real)
	if err != nil {
		t.Fatal(err)
	}
	if got := callToolName(rewritten); got != "get_issue" {
		t.Errorf("rewritten call name = %q, want get_issue", got)
	}
	// arguments preserved
	if !json.Valid(rewritten) || !strings.Contains(string(rewritten), `"key":"X-1"`) {
		t.Errorf("arguments not preserved: %s", string(rewritten))
	}
}

func TestRoute_UnknownToolOwner(t *testing.T) {
	rt := newRoute("wiki", "", []*Backend{stubBackend("a")})
	if _, err := rt.toolsList(func(b *Backend) (json.RawMessage, error) { return toolsResult("x"), nil }); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := rt.ownerOfTool("nope"); ok {
		t.Error("unknown tool should not resolve an owner")
	}
}

func TestRoute_IsComposite(t *testing.T) {
	if newRoute("s", "", []*Backend{stubBackend("a")}).isComposite() {
		t.Error("single member should not be composite")
	}
	if !newRoute("c", "", []*Backend{stubBackend("a"), stubBackend("b")}).isComposite() {
		t.Error("two members should be composite")
	}
}

func TestRoute_Primary(t *testing.T) {
	rt := newRoute("c", "", []*Backend{stubBackend("first"), stubBackend("second")})
	if rt.primary().name != "first" {
		t.Errorf("primary should be member[0], got %q", rt.primary().name)
	}
}
