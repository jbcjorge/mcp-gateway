package main

import (
	"encoding/json"
	"testing"
)

func filterNames(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var w map[string]json.RawMessage
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	var tools []map[string]any
	if err := json.Unmarshal(w["tools"], &tools); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools {
		names = append(names, tool["name"].(string))
	}
	return names
}

func TestFilterResultTools_IncludeOnly(t *testing.T) {
	b := &Backend{def: BackendDef{IncludeTools: []string{"confluence_remove_label"}}}
	res := toolsResult("confluence_get_page", "confluence_remove_label", "confluence_search_pages")
	out := filterResultTools(res, b)
	names := filterNames(t, out)
	if len(names) != 1 || names[0] != "confluence_remove_label" {
		t.Errorf("include filter should keep only remove_label, got %v", names)
	}
}

func TestFilterResultTools_Exclude(t *testing.T) {
	b := &Backend{def: BackendDef{ExcludeTools: []string{"*_delete", "danger_*"}}}
	res := toolsResult("get", "do_delete", "danger_zone", "keep")
	names := filterNames(t, filterResultTools(res, b))
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	if !got["get"] || !got["keep"] || got["do_delete"] || got["danger_zone"] {
		t.Errorf("exclude filter wrong, got %v", names)
	}
}

func TestFilterResultTools_NoFilterUnchanged(t *testing.T) {
	b := &Backend{def: BackendDef{}}
	res := toolsResult("a", "b")
	out := filterResultTools(res, b)
	if string(out) != string(res) {
		t.Errorf("no-filter should return input unchanged")
	}
}

func TestFilterResultTools_IncludeAndExclude(t *testing.T) {
	// include broad, then exclude a subset.
	b := &Backend{def: BackendDef{IncludeTools: []string{"conf_*"}, ExcludeTools: []string{"conf_delete_*"}}}
	res := toolsResult("conf_read", "conf_delete_page", "other_tool")
	names := filterNames(t, filterResultTools(res, b))
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	if !got["conf_read"] || got["conf_delete_page"] || got["other_tool"] {
		t.Errorf("include+exclude wrong, got %v", names)
	}
}
