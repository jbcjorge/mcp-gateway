package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCategorizeToolName(t *testing.T) {
	tests := []struct {
		name     string
		expected string
	}{
		{"get_users", "read"},
		{"read_post", "read"},
		{"download_file", "read"},
		{"view_item", "read"},
		{"create_issue", "create"},
		{"add_comment", "create"},
		{"batch_create_issues", "create"},
		{"delete_repo", "delete"},
		{"remove_label", "delete"},
		{"cancel_pipeline", "delete"},
		{"unapprove_merge_request", "delete"},
		{"search_code", "search"},
		{"list_items", "search"},
		{"filter_topics", "search"},
		{"find_users", "search"},
		{"update_issue", "update"},
		{"edit_comment", "update"},
		{"transition_issue", "update"},
		{"link_to_epic", "update"},
		{"approve_merge_request", "update"},
		{"merge_merge_request", "update"},
		{"custom_action", "general"},
		{"something_else", "general"},
		// Namespace prefix tests
		{"jira_get_issue", "read"},
		{"confluence_search_pages", "search"},
		{"discourse_create_topic", "create"},
		{"devops_delete_item", "delete"},
		{"mdp_update_doc", "update"},
		{"iam_custom_tool", "general"},
	}

	for _, tt := range tests {
		got := categorizeToolName(tt.name)
		if got != tt.expected {
			t.Errorf("categorizeToolName(%q) = %q, want %q", tt.name, got, tt.expected)
		}
	}
}

func TestBuildCategories(t *testing.T) {
	toolsJSON := `{"jsonrpc":"2.0","id":"1","result":{"tools":[
		{"name":"get_users","description":"Get users"},
		{"name":"create_issue","description":"Create issue"},
		{"name":"delete_repo","description":"Delete repo"},
		{"name":"search_code","description":"Search code"},
		{"name":"list_items","description":"List items"},
		{"name":"update_thing","description":"Update thing"},
		{"name":"custom_action","description":"Custom action"}
	]}}`

	b := &Backend{
		name:        "test-cats",
		def:         BackendDef{},
		toolsCache:  json.RawMessage(toolsJSON),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}

	b.buildCategories()

	if b.categories == nil {
		t.Fatal("categories should not be nil for 7 tools")
	}

	// Check expected categories exist
	if _, ok := b.categories["read"]; !ok {
		t.Error("expected 'read' category")
	}
	if _, ok := b.categories["create"]; !ok {
		t.Error("expected 'create' category")
	}
	if _, ok := b.categories["delete"]; !ok {
		t.Error("expected 'delete' category")
	}
	if _, ok := b.categories["search"]; !ok {
		t.Error("expected 'search' category")
	}
	if _, ok := b.categories["update"]; !ok {
		t.Error("expected 'update' category")
	}
	if _, ok := b.categories["general"]; !ok {
		t.Error("expected 'general' category")
	}
}

func TestBuildCategories_FewTools(t *testing.T) {
	toolsJSON := `{"jsonrpc":"2.0","id":"1","result":{"tools":[
		{"name":"get_users","description":"Get users"},
		{"name":"create_issue","description":"Create"}
	]}}`

	b := &Backend{
		name:        "test-few",
		def:         BackendDef{},
		toolsCache:  json.RawMessage(toolsJSON),
		activeTools: make(map[string]bool),
	}

	b.buildCategories()

	if b.categories != nil {
		t.Error("categories should be nil for <=5 tools (no discovery)")
	}
}

func TestBuildCategories_ForceDiscovery(t *testing.T) {
	force := true
	toolsJSON := `{"jsonrpc":"2.0","id":"1","result":{"tools":[
		{"name":"get_users","description":"Get users"},
		{"name":"create_issue","description":"Create"}
	]}}`

	b := &Backend{
		name:        "test-force",
		def:         BackendDef{Discovery: &force},
		toolsCache:  json.RawMessage(toolsJSON),
		activeTools: make(map[string]bool),
	}

	b.buildCategories()

	if b.categories == nil {
		t.Error("categories should be set when discovery is forced")
	}
}

func TestBuildCategories_Disabled(t *testing.T) {
	disabled := false
	toolsJSON := `{"jsonrpc":"2.0","id":"1","result":{"tools":[
		{"name":"get_users","description":"Get users"},
		{"name":"create_issue","description":"Create"},
		{"name":"delete_repo","description":"Delete"},
		{"name":"search_code","description":"Search"},
		{"name":"list_items","description":"List"},
		{"name":"update_thing","description":"Update"}
	]}}`

	b := &Backend{
		name:        "test-disabled-disc",
		def:         BackendDef{Discovery: &disabled},
		toolsCache:  json.RawMessage(toolsJSON),
		activeTools: make(map[string]bool),
	}

	b.buildCategories()

	if b.categories != nil {
		t.Error("categories should be nil when discovery is disabled")
	}
}

func TestBuildCategories_ManualConfig(t *testing.T) {
	toolsJSON := `{"jsonrpc":"2.0","id":"1","result":{"tools":[
		{"name":"tool_a","description":"A"},
		{"name":"tool_b","description":"B"},
		{"name":"tool_c","description":"C"},
		{"name":"tool_d","description":"D"},
		{"name":"tool_e","description":"E"},
		{"name":"tool_f","description":"F"}
	]}}`

	b := &Backend{
		name: "test-manual-cats",
		def: BackendDef{
			Categories: map[string][]string{
				"group1": {"tool_a", "tool_b"},
				"group2": {"tool_c", "tool_d"},
			},
		},
		toolsCache:  json.RawMessage(toolsJSON),
		activeTools: make(map[string]bool),
	}

	b.buildCategories()

	if len(b.categories) != 2 {
		t.Errorf("expected 2 manual categories, got %d", len(b.categories))
	}
}

func TestBuildCategories_NilCache(t *testing.T) {
	b := &Backend{
		name:        "test-nil-cache",
		def:         BackendDef{},
		activeTools: make(map[string]bool),
	}

	b.buildCategories()
	if b.categories != nil {
		t.Error("categories should be nil with no cache")
	}
}

func TestActivateCategory(t *testing.T) {
	toolsJSON := `{"jsonrpc":"2.0","id":"1","result":{"tools":[
		{"name":"get_users","description":"Get users"},
		{"name":"create_issue","description":"Create issue"},
		{"name":"delete_repo","description":"Delete repo"},
		{"name":"search_code","description":"Search code"},
		{"name":"list_items","description":"List items"},
		{"name":"update_thing","description":"Update thing"},
		{"name":"custom_action","description":"Custom action"}
	]}}`

	b := &Backend{
		name:        "test-activate",
		def:         BackendDef{},
		toolsCache:  json.RawMessage(toolsJSON),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}
	b.buildCategories()

	b.mu.Lock()
	activated, err := b.activateCategory("read")
	b.mu.Unlock()
	if err != nil {
		t.Fatalf("activateCategory: %v", err)
	}
	if len(activated) == 0 {
		t.Error("expected activated tools")
	}

	// Check activeTools was updated
	b.mu.Lock()
	if !b.activeTools["get_users"] {
		t.Error("get_users should be active after activating 'read' category")
	}
	b.mu.Unlock()
}

func TestActivateCategory_Unknown(t *testing.T) {
	toolsJSON := `{"jsonrpc":"2.0","id":"1","result":{"tools":[
		{"name":"get_users","description":"Get users"},
		{"name":"create_issue","description":"Create"},
		{"name":"delete_repo","description":"Delete"},
		{"name":"search_code","description":"Search"},
		{"name":"list_items","description":"List"},
		{"name":"update_thing","description":"Update"}
	]}}`

	b := &Backend{
		name:        "test-activate-unknown",
		def:         BackendDef{},
		toolsCache:  json.RawMessage(toolsJSON),
		activeTools: make(map[string]bool),
	}
	b.buildCategories()

	b.mu.Lock()
	_, err := b.activateCategory("nonexistent")
	b.mu.Unlock()
	if err == nil {
		t.Error("expected error for unknown category")
	}
}

func TestFilteredToolsList(t *testing.T) {
	toolsJSON := `{"jsonrpc":"2.0","id":"1","result":{"tools":[
		{"name":"get_users","description":"Get users"},
		{"name":"create_issue","description":"Create issue"},
		{"name":"delete_repo","description":"Delete repo"},
		{"name":"search_code","description":"Search code"},
		{"name":"list_items","description":"List items"},
		{"name":"update_thing","description":"Update thing"},
		{"name":"custom_action","description":"Custom action"}
	]}}`

	b := &Backend{
		name:        "test-filtered",
		def:         BackendDef{},
		toolsCache:  json.RawMessage(toolsJSON),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}
	b.buildCategories()

	// Activate one category
	b.mu.Lock()
	b.activateCategory("read")
	b.mu.Unlock()

	id := json.RawMessage(`99`)
	data, err := b.filteredToolsList(&id)
	if err != nil {
		t.Fatalf("filteredToolsList: %v", err)
	}

	var resp struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("unmarshal filtered: %v", err)
	}

	// Should have discover meta-tool + active tools from "read" category
	if len(resp.Result.Tools) < 2 {
		t.Errorf("expected at least 2 tools (discover + read tools), got %d", len(resp.Result.Tools))
	}

	// First tool should be the discover meta-tool
	var firstTool struct {
		Name string `json:"name"`
	}
	json.Unmarshal(resp.Result.Tools[0], &firstTool)
	if firstTool.Name != "discover_test-filtered_tools" {
		t.Errorf("first tool should be discover meta-tool, got %q", firstTool.Name)
	}
}

func TestDiscoverToolSchema(t *testing.T) {
	b := &Backend{
		name: "mybackend",
		categories: map[string][]string{
			"read":   {"get_users", "read_post"},
			"create": {"create_issue"},
		},
	}

	schema := b.discoverToolSchema()
	if schema["name"] != "discover_mybackend_tools" {
		t.Errorf("expected discover_mybackend_tools, got %v", schema["name"])
	}
	desc, ok := schema["description"].(string)
	if !ok || !strings.Contains(desc, "mybackend") {
		t.Errorf("description should mention backend name, got %q", desc)
	}
	if !strings.Contains(desc, "create") || !strings.Contains(desc, "read") {
		t.Errorf("description should list categories, got %q", desc)
	}
}

func TestParseToolsFromCache(t *testing.T) {
	toolsJSON := `{"jsonrpc":"2.0","id":"1","result":{"tools":[
		{"name":"tool_a","description":"A"},
		{"name":"tool_b","description":"B"}
	]}}`

	b := &Backend{toolsCache: json.RawMessage(toolsJSON)}
	tools := b.parseToolsFromCache()
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	if tools[0].Name != "tool_a" || tools[1].Name != "tool_b" {
		t.Errorf("unexpected tool names: %v, %v", tools[0].Name, tools[1].Name)
	}
}

func TestParseToolsFromCache_Invalid(t *testing.T) {
	b := &Backend{toolsCache: json.RawMessage(`invalid json`)}
	tools := b.parseToolsFromCache()
	if tools != nil {
		t.Error("expected nil for invalid cache")
	}
}

func TestToolsContainDiscovery(t *testing.T) {
	b := &Backend{}

	toolsWithDiscover := []toolEntry{
		{Name: "get_users"},
		{Name: "discover_tools"},
		{Name: "create_issue"},
	}
	if !b.toolsContainDiscovery(toolsWithDiscover) {
		t.Error("should detect discover_tools")
	}

	toolsWithout := []toolEntry{
		{Name: "get_users"},
		{Name: "create_issue"},
	}
	if b.toolsContainDiscovery(toolsWithout) {
		t.Error("should not detect discover_tools when absent")
	}
}

func TestSmartActivate(t *testing.T) {
	b := &Backend{
		name: "test-smart",
		categories: map[string][]string{
			"read":   {"get_users", "get_items"},
			"create": {"create_issue"},
		},
		activeTools: make(map[string]bool),
	}

	b.smartActivate("get_users")

	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.activeTools["get_users"] {
		t.Error("get_users should be active")
	}
	if !b.activeTools["get_items"] {
		t.Error("get_items should be active (same category)")
	}
	if b.activeTools["create_issue"] {
		t.Error("create_issue should NOT be active")
	}
}

func TestSmartActivate_AlreadyActive(t *testing.T) {
	b := &Backend{
		name: "test-smart-noop",
		categories: map[string][]string{
			"read": {"get_users"},
		},
		activeTools: map[string]bool{"get_users": true},
	}

	// Should be a no-op, no panic
	b.smartActivate("get_users")
}

func TestSmartActivate_NilCategories(t *testing.T) {
	b := &Backend{
		name:        "test-smart-nil",
		categories:  nil,
		activeTools: make(map[string]bool),
	}

	// Should be a no-op, no panic
	b.smartActivate("anything")
}

func TestSaveAndLoadToolsCache(t *testing.T) {
	dir := t.TempDir()
	toolsJSON := `{"jsonrpc":"2.0","id":"1","result":{"tools":[{"name":"tool1"}]}}`

	b := &Backend{
		name:       "cache-test",
		cacheDir:   dir,
		toolsCache: json.RawMessage(toolsJSON),
	}

	b.saveToolsCache()

	// Create a new backend and load the cache
	b2 := &Backend{
		name:     "cache-test",
		cacheDir: dir,
	}
	b2.loadToolsCache()

	if b2.toolsCache == nil {
		t.Fatal("toolsCache should be loaded from disk")
	}
	if string(b2.toolsCache) != toolsJSON {
		t.Errorf("cache mismatch: got %s", string(b2.toolsCache))
	}
}

func TestLoadToolsCache_NonexistentFile(t *testing.T) {
	b := &Backend{
		name:     "no-cache",
		cacheDir: "/nonexistent/path/that/does/not/exist",
	}

	// Should not panic or error
	b.loadToolsCache()

	if b.toolsCache != nil {
		t.Error("toolsCache should be nil for nonexistent file")
	}
}

func TestSaveToolsCache_EmptyCacheDir(t *testing.T) {
	b := &Backend{
		name:       "empty-dir",
		cacheDir:   "",
		toolsCache: json.RawMessage(`{"tools":[]}`),
	}

	// Should be a no-op, not panic
	b.saveToolsCache()
}

func TestSaveToolsCache_NilCache(t *testing.T) {
	b := &Backend{
		name:       "nil-cache",
		cacheDir:   t.TempDir(),
		toolsCache: nil,
	}

	// Should be a no-op, not panic
	b.saveToolsCache()
}

func TestTruncateDescription(t *testing.T) {
	b := &Backend{maxDescLen: 10}

	raw := json.RawMessage(`{"name":"tool","description":"a very long description that should be truncated"}`)
	result := b.truncateDescription(raw)

	var tool map[string]any
	json.Unmarshal(result, &tool)
	desc := tool["description"].(string)
	if len(desc) > 13 { // 10 + "..."
		t.Errorf("description should be truncated, got %q (len=%d)", desc, len(desc))
	}
}

func TestTruncateDescription_NoTruncate(t *testing.T) {
	b := &Backend{maxDescLen: 0} // disabled

	raw := json.RawMessage(`{"name":"tool","description":"long description"}`)
	result := b.truncateDescription(raw)

	if string(result) != string(raw) {
		t.Error("should not truncate when maxDescLen=0")
	}
}

func TestTruncateDescription_ShortDesc(t *testing.T) {
	b := &Backend{maxDescLen: 100}

	raw := json.RawMessage(`{"name":"tool","description":"short"}`)
	result := b.truncateDescription(raw)

	if string(result) != string(raw) {
		t.Error("should not truncate short descriptions")
	}
}

func TestHandleToolsList_FromCache_NoCats(t *testing.T) {
	toolsJSON := `{"jsonrpc":"2.0","id":"old","result":{"tools":[{"name":"tool1","description":"T1"}]}}`
	b := &Backend{
		name:        "cached-nocat",
		def:         BackendDef{},
		toolsCache:  json.RawMessage(toolsJSON),
		activeTools: make(map[string]bool),
	}
	// No categories (few tools)

	id := json.RawMessage(`"new-id"`)
	data, handled := b.handleToolsList(&id)
	if !handled {
		t.Fatal("should be handled from cache")
	}

	var resp map[string]any
	json.Unmarshal(data, &resp)
	// ID should be updated
	if fmt.Sprintf("%v", resp["id"]) != "new-id" {
		t.Errorf("id should be new-id, got %v", resp["id"])
	}
}

func TestHandleToolsList_NoCache(t *testing.T) {
	b := &Backend{
		name:        "no-cache-list",
		def:         BackendDef{},
		toolsCache:  nil,
		activeTools: make(map[string]bool),
	}

	id := json.RawMessage(`1`)
	_, handled := b.handleToolsList(&id)
	if handled {
		t.Error("should not be handled when no cache exists")
	}
}

func TestSaveToolsCache_Success(t *testing.T) {
	dir := t.TempDir()
	cacheSubDir := filepath.Join(dir, "newcache")

	b := &Backend{
		name:       "save-test",
		cacheDir:   cacheSubDir,
		toolsCache: json.RawMessage(`{"result":{"tools":[]}}`),
	}

	b.saveToolsCache()

	// Verify file was created
	path := filepath.Join(cacheSubDir, "save-test.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cache file should exist: %v", err)
	}
	if string(data) != `{"result":{"tools":[]}}` {
		t.Errorf("unexpected cache content: %s", string(data))
	}
}
