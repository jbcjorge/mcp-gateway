package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	errors "github.com/jbcjorge/errors-library"
)

// buildCategories parses the cached tools and groups them into categories.
// For backends with a native discover_tools, it extracts the categories from its description.
// For others, it auto-categorizes by action verb prefix (get_, create_, list_, search_, etc.).
// Discovery is only enabled for backends with more than 5 tools (unless overridden by config).
func (b *Backend) buildCategories() {
	if b.toolsCache == nil {
		return
	}

	if b.def.Discovery != nil && !*b.def.Discovery {
		b.categories = nil
		return
	}

	tools := b.parseToolsFromCache()
	if tools == nil {
		return
	}

	forceDiscovery := b.def.Discovery != nil && *b.def.Discovery
	if len(tools) <= 5 && !forceDiscovery {
		b.categories = nil
		return
	}

	b.categories = make(map[string][]string)
	b.hasDiscovery = false

	if len(b.def.Categories) > 0 {
		b.categories = b.def.Categories
		b.hasDiscovery = b.toolsContainDiscovery(tools)
		slog.Debug("categories from config", "backend", b.name, "categories", len(b.categories))
		return
	}

	for _, tool := range tools {
		if tool.Name == "discover_tools" {
			b.hasDiscovery = true
			continue
		}
		if !b.toolAllowed(tool.Name) {
			continue
		}
		category := categorizeToolName(tool.Name)
		b.categories[category] = append(b.categories[category], tool.Name)
	}

	slog.Debug("categories built", "backend", b.name, "categories", len(b.categories), "hasNativeDiscovery", b.hasDiscovery)
}

// parseToolsFromCache extracts the tools list from the cached tools/list response.
func (b *Backend) parseToolsFromCache() []toolEntry {
	var resp struct {
		Result struct {
			Tools []toolEntry `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b.toolsCache, &resp); err != nil {
		return nil
	}
	return resp.Result.Tools
}

// toolsContainDiscovery checks if the tools list contains a native discover_tools tool.
func (b *Backend) toolsContainDiscovery(tools []toolEntry) bool {
	for _, tool := range tools {
		if tool.Name == "discover_tools" {
			return true
		}
	}
	return false
}

// categorizeToolName extracts a category from a tool name based on its action verb prefix.
func categorizeToolName(name string) string {
	parts := strings.Split(name, "_")

	// Skip known namespace prefixes (jira_, confluence_, discourse_, devops_, mdp_, iam_)
	namespaces := map[string]bool{
		"jira": true, "confluence": true, "discourse": true,
		"devops": true, "mdp": true, "iam": true,
	}
	start := 0
	if len(parts) > 1 && namespaces[parts[0]] {
		start = 1
	}

	if start >= len(parts) {
		return "general"
	}

	// Categorize by action verb
	verb := parts[start]
	switch verb {
	case "get", "read", "download", "view":
		return "read"
	case "list", "search", "filter", "find":
		return "search"
	case "create", "add", "batch":
		return "create"
	case "update", "edit", "transition", "link", "approve", "merge":
		return "update"
	case "delete", "remove", "unapprove", "cancel":
		return "delete"
	default:
		return "general"
	}
}

// discoverToolSchema returns the JSON schema for the discover_tools meta-tool for this backend.
func (b *Backend) discoverToolSchema() map[string]any {
	categories := make([]string, 0, len(b.categories))
	for cat := range b.categories {
		categories = append(categories, cat)
	}
	sort.Strings(categories)

	desc := fmt.Sprintf(
		"Discover and activate tool categories for %s. Available categories: %s. Call with a category to activate those tools.",
		b.name, strings.Join(categories, ", "),
	)

	// Use backend-specific name to avoid collisions when multiple backends have discovery
	toolName := fmt.Sprintf("discover_%s_tools", b.name)

	return map[string]any{
		"name":        toolName,
		"description": desc,
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"category": map[string]any{
					"type":        "string",
					"description": fmt.Sprintf("Category to activate. Options: %s. Omit to list all categories.", strings.Join(categories, ", ")),
				},
			},
		},
	}
}

// activateCategory adds all tools from a category to the active set.
// Returns the list of tool schemas that were activated.
func (b *Backend) activateCategory(category string) ([]map[string]any, error) {
	toolNames, ok := b.categories[category]
	if !ok {
		available := make([]string, 0, len(b.categories))
		for cat := range b.categories {
			available = append(available, cat)
		}
		sort.Strings(available)
		return nil, ErrUnknownCategory.Parse(errors.WithParsedMessage(category), errors.WithSafeData(map[string]any{"available": strings.Join(available, ", ")}))
	}

	// Get full tool schemas from cache
	var resp struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b.toolsCache, &resp); err != nil {
		return nil, err
	}

	// Build name->schema index
	toolIndex := make(map[string]json.RawMessage)
	for _, raw := range resp.Result.Tools {
		var t struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &t) == nil {
			toolIndex[t.Name] = raw
		}
	}

	// Activate and collect schemas
	activated := make([]map[string]any, 0, len(toolNames))
	for _, name := range toolNames {
		b.activeTools[name] = true
		if raw, ok := toolIndex[name]; ok {
			var schema map[string]any
			if json.Unmarshal(raw, &schema) == nil {
				activated = append(activated, schema)
			}
		}
	}

	slog.Info("category activated", "backend", b.name, "category", category, "tools", len(toolNames))
	return activated, nil
}

// smartActivate finds the category containing the given tool name and activates
// the entire category. This ensures related tools are available when one tool
// from a category is called directly (e.g. by the agent retrying a cached name).
func (b *Backend) smartActivate(toolName string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.categories == nil || b.activeTools[toolName] {
		return
	}

	for category, tools := range b.categories {
		for _, name := range tools {
			if name == toolName {
				// Activate all tools in this category
				for _, t := range tools {
					b.activeTools[t] = true
				}
				slog.Info("smart-activated category", "backend", b.name, "category", category, "trigger", toolName)
				return
			}
		}
	}
}

// filteredToolsList returns the tools/list response with only discover_tools + active tools.
func (b *Backend) filteredToolsList(id *json.RawMessage) ([]byte, error) {
	if b.toolsCache == nil {
		return nil, ErrNoToolsCache.Parse()
	}

	// Parse full cache
	var resp struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b.toolsCache, &resp); err != nil {
		return nil, err
	}

	// Build filtered list: discover_tools meta-tool + active tools
	filtered := make([]json.RawMessage, 0)

	// Add discover_tools meta-tool
	discoverSchema := b.discoverToolSchema()
	discoverJSON, _ := json.Marshal(discoverSchema)
	filtered = append(filtered, discoverJSON)

	// Add active tools (with optional description truncation)
	for _, raw := range resp.Result.Tools {
		var t struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &t) == nil && b.activeTools[t.Name] && b.toolAllowed(t.Name) {
			filtered = append(filtered, b.truncateDescription(raw))
		}
	}

	result := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]any{
			"tools": filtered,
		},
	}
	return json.Marshal(result)
}

// truncateDescription shortens the description field of a tool schema if maxDescLen is set.
func (b *Backend) truncateDescription(raw json.RawMessage) json.RawMessage {
	if b.maxDescLen <= 0 {
		return raw
	}
	var tool map[string]any
	if err := json.Unmarshal(raw, &tool); err != nil {
		return raw
	}
	if desc, ok := tool["description"].(string); ok && len(desc) > b.maxDescLen {
		tool["description"] = desc[:b.maxDescLen] + "..."
		if modified, err := json.Marshal(tool); err == nil {
			return modified
		}
	}
	return raw
}

// matchGlob performs a simple glob match supporting * as wildcard.
func matchGlob(pattern, name string) bool {
	if pattern == "*" {
		return true
	}
	// Use filepath.Match for standard glob semantics
	matched, err := filepath.Match(pattern, name)
	if err != nil {
		return false
	}
	return matched
}

// toolAllowed returns true if the tool name passes the include/exclude filters.
// Logic: if include_tools is set, tool must match at least one include pattern.
// Then, if exclude_tools is set, tool must NOT match any exclude pattern.
func (b *Backend) toolAllowed(name string) bool {
	if len(b.def.IncludeTools) > 0 {
		matched := false
		for _, pattern := range b.def.IncludeTools {
			if matchGlob(pattern, name) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for _, pattern := range b.def.ExcludeTools {
		if matchGlob(pattern, name) {
			return false
		}
	}
	return true
}

// loadToolsCache reads the cached tools/list response from disk.
func (b *Backend) loadToolsCache() {
	if b.cacheDir == "" {
		return
	}
	path := filepath.Join(b.cacheDir, b.name+".json")
	data, err := os.ReadFile(path) // #nosec G304 -- path from cacheDir config + backend name, not user request input
	if err != nil {
		return // no cache file, that's fine
	}
	b.toolsCache = json.RawMessage(data)
	slog.Debug("tools cache loaded from disk", "backend", b.name, "path", path)
}

// saveToolsCache writes the tools/list response to disk for persistence across restarts.
func (b *Backend) saveToolsCache() {
	if b.cacheDir == "" || b.toolsCache == nil {
		return
	}
	if err := os.MkdirAll(b.cacheDir, 0750); err != nil {
		slog.Warn("failed to create cache directory", "backend", b.name, "error", err)
		return
	}
	path := filepath.Join(b.cacheDir, b.name+".json")
	if err := os.WriteFile(path, b.toolsCache, 0600); err != nil {
		slog.Warn("failed to write tools cache", "backend", b.name, "error", err)
	} else {
		slog.Debug("tools cache saved to disk", "backend", b.name, "path", path)
	}
}

// refreshToolsCache fetches tools/list from the live backend and updates the cache
// if this backend process hasn't already refreshed it (tracked by pid).
func (b *Backend) refreshToolsCache() {
	b.mu.Lock()
	pid := 0
	if b.cmd != nil && b.cmd.Process != nil {
		pid = b.cmd.Process.Pid
	}
	if pid == 0 || pid == b.toolsCachePid {
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()

	toolsMsg := []byte(`{"jsonrpc":"2.0","id":"_gw_tools","method":"tools/list","params":{}}`)
	resp, err := b.send(context.Background(), toolsMsg)
	if err != nil {
		slog.Warn("failed to refresh tools cache", "backend", b.name, "error", err)
		return
	}

	// Verify it's a valid tools/list response
	var check struct {
		Result *struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if json.Unmarshal(resp, &check) != nil || check.Result == nil {
		slog.Warn("tools cache refresh got invalid response", "backend", b.name)
		return
	}

	b.mu.Lock()
	b.toolsCache = json.RawMessage(resp)
	b.toolsCachePid = pid
	b.mu.Unlock()

	b.saveToolsCache()
	b.buildCategories()
	slog.Info("tools cache refreshed from live backend", "backend", b.name, "pid", pid, "tools", len(check.Result.Tools))
}

// handleDiscoverTools intercepts discover_<backend>_tools calls and responds from cache.
// Returns nil, false if this is not a discovery call and should be forwarded.
func (gw *Gateway) handleDiscoverTools(env jsonRPCMessage, body []byte, b *Backend) ([]byte, bool) {
	expectedName := fmt.Sprintf("discover_%s_tools", b.name)
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(body, &struct {
		Params *struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}{Params: &params}); err != nil || (params.Name != expectedName && params.Name != "discover_tools") {
		return nil, false
	}

	var args struct {
		Category string `json:"category"`
	}
	if params.Arguments != nil {
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			slog.Warn("discover_tools: failed to parse arguments", "backend", b.name, "error", err)
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if args.Category == "" {
		return b.listCategories(env.ID), true
	}

	return gw.activateAndRespond(env.ID, b, args.Category)
}

// handleToolsList serves tools/list from cache when possible.
// Returns nil, false if the request should be forwarded to the backend.
func (b *Backend) handleToolsList(id *json.RawMessage) ([]byte, bool) {
	b.mu.Lock()
	hasCats := len(b.categories) > 0
	cached := b.toolsCache
	b.mu.Unlock()

	if cached != nil && hasCats {
		data, err := b.filteredToolsList(id)
		if err == nil {
			slog.Debug("tools/list served filtered (discover mode)", "backend", b.name, "activeTools", len(b.activeTools))
			return data, true
		}
	}
	if cached != nil {
		var cachedResp map[string]any
		if err := json.Unmarshal(cached, &cachedResp); err == nil {
			cachedResp["id"] = id
			data, _ := json.Marshal(cachedResp)
			slog.Debug("tools/list served from cache (no discovery)", "backend", b.name)
			return data, true
		}
	}
	return nil, false
}

// listCategories returns a JSON-RPC response listing all tool categories.
// Must be called with b.mu held.
func (b *Backend) listCategories(id *json.RawMessage) []byte {
	catList := make([]map[string]any, 0, len(b.categories))
	for cat, tools := range b.categories {
		active := false
		for _, t := range tools {
			if b.activeTools[t] {
				active = true
				break
			}
		}
		catList = append(catList, map[string]any{
			"id":        cat,
			"toolCount": len(tools),
			"active":    active,
		})
	}
	sort.Slice(catList, func(i, j int) bool {
		return catList[i]["id"].(string) < catList[j]["id"].(string)
	})
	content, _ := json.MarshalIndent(map[string]any{"categories": catList}, "", "  ")
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": string(content)},
			},
		},
	}
	data, _ := json.Marshal(resp)
	slog.Debug("discover_tools: listed categories", "backend", b.name)
	return data
}

// activateAndRespond activates a category and returns the JSON-RPC response.
// Must be called with b.mu held.
func (gw *Gateway) activateAndRespond(id *json.RawMessage, b *Backend, category string) ([]byte, bool) {
	activated, err := b.activateCategory(category)
	if err != nil {
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"result": map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": err.Error()},
				},
				"isError": true,
			},
		}
		data, _ := json.Marshal(resp)
		return data, true
	}

	toolNames := make([]string, 0, len(activated))
	for _, t := range activated {
		if name, ok := t["name"].(string); ok {
			toolNames = append(toolNames, name)
		}
	}
	totalActive := len(b.activeTools)
	content, _ := json.MarshalIndent(map[string]any{
		"activated":  category,
		"addedTools": toolNames,
		"totalTools": totalActive,
	}, "", "  ")
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": string(content)},
			},
		},
	}
	data, _ := json.Marshal(resp)

	gw.notifyToolsChanged()
	slog.Info("discover_tools: category activated", "backend", b.name, "category", category, "tools", len(toolNames))
	return data, true
}
