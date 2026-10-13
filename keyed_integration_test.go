package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// gatewayWithKeyed builds a Gateway with one keyed route "sonarqube" whose
// children are fake subprocess MCP servers. The child echoes its injected
// "sonarUrl" env in tools/call results (TEST_ECHO_ENV=sonarUrl), so tests can
// assert which instance served a call.
func gatewayWithKeyed(t *testing.T) *Gateway {
	t.Helper()
	cfg := Config{
		Listen:   ":0",
		CacheDir: t.TempDir(),
		Servers: map[string]BackendDef{
			"sonarqube": {
				Command: subprocessCommand(),
				Env: map[string]string{
					"TEST_SUBPROCESS": "1",
					"TEST_ECHO_ENV":   "sonarUrl",
				},
				Params: []Param{
					{Name: "sonarUrl", Description: "Target SonarQube URL", Validate: "^https://"},
				},
			},
		},
		Routes: []RouteEntry{{Route: "sonarqube"}},
	}
	gw, err := newGateway(cfg)
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}
	return gw
}

func TestKeyed_ColdToolsListIsEmpty(t *testing.T) {
	gw := gatewayWithKeyed(t)
	defer gw.shutdownAll()

	w := postJSON(t, gw, "/sonarqube/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("tools/list: %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.Result.Tools) != 0 {
		t.Errorf("cold keyed tools/list should be empty, got %d tools", len(resp.Result.Tools))
	}
}

func TestKeyed_InitializeLocal(t *testing.T) {
	gw := gatewayWithKeyed(t)
	defer gw.shutdownAll()
	w := postJSON(t, gw, "/sonarqube/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("initialize: %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "mcp-gateway/sonarqube") {
		t.Errorf("expected serverInfo name, got %s", w.Body.String())
	}
}

func TestKeyed_CallRoutesToInstanceAndInjectsEnv(t *testing.T) {
	gw := gatewayWithKeyed(t)
	defer gw.shutdownAll()

	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_users","arguments":{"sonarUrl":"https://aether-sonarqube.dos.corproot.net","query":"x"}}}`
	w := postJSON(t, gw, "/sonarqube/mcp", call)
	if w.Code != http.StatusOK {
		t.Fatalf("tools/call: %d body=%s", w.Code, w.Body.String())
	}
	// The child echoes its injected sonarUrl env in result.instance.
	var resp struct {
		Result struct {
			Instance string `json:"instance"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v body=%s", err, w.Body.String())
	}
	if resp.Result.Instance != "https://aether-sonarqube.dos.corproot.net" {
		t.Errorf("expected child to receive injected sonarUrl env, got %q", resp.Result.Instance)
	}
}

func TestKeyed_ToolsListWarmedAndParamInjected(t *testing.T) {
	gw := gatewayWithKeyed(t)
	defer gw.shutdownAll()

	// Warm the shared cache by making a call (spawns a child, which caches
	// tools/list).
	_ = postJSON(t, gw, "/sonarqube/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_users","arguments":{"sonarUrl":"https://aether-sonarqube.dos.corproot.net"}}}`)

	// Now tools/list should return the tool surface with the sonarUrl param
	// injected into each tool's inputSchema.
	w := postJSON(t, gw, "/sonarqube/mcp", `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("tools/list: %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				InputSchema struct {
					Properties map[string]any `json:"properties"`
					Required   []string       `json:"required"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v body=%s", err, w.Body.String())
	}
	if len(resp.Result.Tools) == 0 {
		t.Fatal("expected tools after warm-up")
	}
	for _, tool := range resp.Result.Tools {
		if _, ok := tool.InputSchema.Properties["sonarUrl"]; !ok {
			t.Errorf("tool %q missing injected sonarUrl param", tool.Name)
		}
		if !containsString(tool.InputSchema.Required, "sonarUrl") {
			t.Errorf("tool %q should require sonarUrl", tool.Name)
		}
	}
}

func TestKeyed_SameInstanceReusesChild(t *testing.T) {
	gw := gatewayWithKeyed(t)
	defer gw.shutdownAll()

	url := "https://aether-sonarqube.dos.corproot.net"
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_users","arguments":{"sonarUrl":"` + url + `"}}}`

	_ = postJSON(t, gw, "/sonarqube/mcp", call)
	_ = postJSON(t, gw, "/sonarqube/mcp", call)

	// Exactly one child should exist for this instance.
	route := gw.routes["sonarqube"]
	if n := len(route.keyedMembers()); n != 1 {
		t.Errorf("expected 1 pooled child for one instance, got %d", n)
	}
	// And it should be registered in gw.backends under the composite name.
	id := instanceID([]string{url})
	gw.mu.RLock()
	_, ok := gw.backends[childInstanceName("sonarqube", id)]
	gw.mu.RUnlock()
	if !ok {
		t.Error("child should be registered in gw.backends")
	}
}

func TestKeyed_DistinctInstancesSpawnDistinctChildren(t *testing.T) {
	gw := gatewayWithKeyed(t)
	defer gw.shutdownAll()

	_ = postJSON(t, gw, "/sonarqube/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_users","arguments":{"sonarUrl":"https://aether-sonarqube.dos.corproot.net"}}}`)
	_ = postJSON(t, gw, "/sonarqube/mcp",
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_users","arguments":{"sonarUrl":"https://billing-sonarqube.dos.corproot.net"}}}`)

	route := gw.routes["sonarqube"]
	if n := len(route.keyedMembers()); n != 2 {
		t.Errorf("expected 2 pooled children for two instances, got %d", n)
	}
}

func TestKeyed_MissingRequiredParamErrors(t *testing.T) {
	gw := gatewayWithKeyed(t)
	defer gw.shutdownAll()

	w := postJSON(t, gw, "/sonarqube/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_users","arguments":{"query":"x"}}}`)
	// Missing required sonarUrl => JSON-RPC error (HTTP 502 with error body).
	if w.Code == http.StatusOK && !strings.Contains(w.Body.String(), "error") {
		t.Errorf("expected error for missing required param, got %s", w.Body.String())
	}
}

func TestKeyed_InvalidParamErrors(t *testing.T) {
	gw := gatewayWithKeyed(t)
	defer gw.shutdownAll()

	w := postJSON(t, gw, "/sonarqube/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_users","arguments":{"sonarUrl":"ftp://nope"}}}`)
	if w.Code == http.StatusOK && !strings.Contains(w.Body.String(), "error") {
		t.Errorf("expected validation error, got %s", w.Body.String())
	}
}

// childPID returns the OS pid of the keyed child serving the given instance id,
// or 0 if none is running.
func childPID(gw *Gateway, route, id string) int {
	gw.mu.RLock()
	b, ok := gw.backends[childInstanceName(route, id)]
	gw.mu.RUnlock()
	if !ok {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cmd != nil && b.cmd.Process != nil {
		return b.cmd.Process.Pid
	}
	return 0
}

// TestKeyed_ParamsInjectedIntoEveryToolAndSetPreserved asserts that after
// warm-up the full tool set from the upstream survives param injection (count
// and names), and that EVERY tool carries the injected param in both its
// properties and its required list.
func TestKeyed_ParamsInjectedIntoEveryToolAndSetPreserved(t *testing.T) {
	gw := gatewayWithKeyed(t)
	defer gw.shutdownAll()

	// Warm the shared cache.
	_ = postJSON(t, gw, "/sonarqube/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_users","arguments":{"sonarUrl":"https://aether-sonarqube.dos.corproot.net"}}}`)

	w := postJSON(t, gw, "/sonarqube/mcp", `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("tools/list: %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				InputSchema struct {
					Properties map[string]any `json:"properties"`
					Required   []string       `json:"required"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v body=%s", err, w.Body.String())
	}

	// The fake upstream advertises exactly these 7 tools; all must survive.
	wantNames := []string{
		"get_users", "create_issue", "delete_repo", "search_code",
		"list_items", "update_thing", "custom_action",
	}
	if len(resp.Result.Tools) != len(wantNames) {
		t.Fatalf("expected %d tools to survive injection, got %d", len(wantNames), len(resp.Result.Tools))
	}
	got := make(map[string]bool, len(resp.Result.Tools))
	for _, tool := range resp.Result.Tools {
		got[tool.Name] = true
		if _, ok := tool.InputSchema.Properties["sonarUrl"]; !ok {
			t.Errorf("tool %q missing injected sonarUrl property", tool.Name)
		}
		if !containsString(tool.InputSchema.Required, "sonarUrl") {
			t.Errorf("tool %q should require sonarUrl", tool.Name)
		}
	}
	for _, n := range wantNames {
		if !got[n] {
			t.Errorf("tool %q was dropped during injection", n)
		}
	}
}

// TestKeyed_SameInstanceReusesSameProcess proves reuse by process identity: two
// calls for the same instance are served by the SAME child PID (not merely one
// pool entry).
func TestKeyed_SameInstanceReusesSameProcess(t *testing.T) {
	gw := gatewayWithKeyed(t)
	defer gw.shutdownAll()

	url := "https://aether-sonarqube.dos.corproot.net"
	id := instanceID([]string{url})
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_users","arguments":{"sonarUrl":"` + url + `"}}}`

	if w := postJSON(t, gw, "/sonarqube/mcp", call); w.Code != http.StatusOK {
		t.Fatalf("first call: %d body=%s", w.Code, w.Body.String())
	}
	pid1 := childPID(gw, "sonarqube", id)
	if pid1 == 0 {
		t.Fatal("expected a running child after first call")
	}

	if w := postJSON(t, gw, "/sonarqube/mcp", call); w.Code != http.StatusOK {
		t.Fatalf("second call: %d body=%s", w.Code, w.Body.String())
	}
	pid2 := childPID(gw, "sonarqube", id)

	if pid1 != pid2 {
		t.Errorf("same instance should reuse the same process: pid1=%d pid2=%d", pid1, pid2)
	}
	if n := len(gw.routes["sonarqube"].keyedMembers()); n != 1 {
		t.Errorf("expected exactly 1 pooled child, got %d", n)
	}
}

// TestKeyed_DistinctInstancesUseDistinctProcesses proves isolation by process
// identity: two different instances are served by DIFFERENT child PIDs.
func TestKeyed_DistinctInstancesUseDistinctProcesses(t *testing.T) {
	gw := gatewayWithKeyed(t)
	defer gw.shutdownAll()

	urlA := "https://aether-sonarqube.dos.corproot.net"
	urlB := "https://billing-sonarqube.dos.corproot.net"

	if w := postJSON(t, gw, "/sonarqube/mcp",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_users","arguments":{"sonarUrl":"`+urlA+`"}}}`); w.Code != http.StatusOK {
		t.Fatalf("call A: %d", w.Code)
	}
	if w := postJSON(t, gw, "/sonarqube/mcp",
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_users","arguments":{"sonarUrl":"`+urlB+`"}}}`); w.Code != http.StatusOK {
		t.Fatalf("call B: %d", w.Code)
	}

	pidA := childPID(gw, "sonarqube", instanceID([]string{urlA}))
	pidB := childPID(gw, "sonarqube", instanceID([]string{urlB}))

	if pidA == 0 || pidB == 0 {
		t.Fatalf("expected both children running: pidA=%d pidB=%d", pidA, pidB)
	}
	if pidA == pidB {
		t.Errorf("distinct instances must use distinct processes, both were pid=%d", pidA)
	}
}
