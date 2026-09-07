package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
)

// newTestGateway creates a Gateway with the given backends for testing. Each
// backend becomes a single-member plain route of the same name.
func newTestGateway(backends map[string]BackendDef) *Gateway {
	perRoute := map[string][]string{}
	gw := &Gateway{
		config: Config{
			Listen:      ":0",
			IdleTimeout: 0,
		},
		backends:    make(map[string]*Backend),
		routes:      make(map[string]*Route),
		lastRequest: time.Now(),
	}
	for name, def := range backends {
		b := &Backend{
			name:    name,
			def:     def,
			pending: make(map[string]chan json.RawMessage),
		}
		gw.backends[name] = b
		gw.routes[name] = newRoute(name, "", []*Backend{b})
		if len(def.AuthTokens) > 0 {
			perRoute[name] = def.AuthTokens
		}
	}
	gw.authorizer = NewBearerAuthorizer(nil, perRoute)
	return gw
}

// writeTestFile is a test helper to write content to a file.
func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0644)
}

// --- Bearer Auth Tests ---

// --- Tool Filtering Tests ---

// --- Disabled Backend Tests ---

// =============================================================================
// ADDITIONAL TESTS FOR COVERAGE - Subprocess, Discovery, Reaper, Cache, etc.
// =============================================================================

// TestSubprocessHelper is a fake MCP backend that runs when TEST_SUBPROCESS=1.
// It reads JSON-RPC from stdin line-by-line and echoes valid responses.
func TestSubprocessHelper(t *testing.T) {
	if os.Getenv("TEST_SUBPROCESS") != "1" {
		t.Skip("helper process")
	}
	// Optionally emit the credential-expiry sentinel to stderr at startup so
	// the gateway's TTL watcher can be exercised. Value is a unix epoch.
	if exp := os.Getenv("TEST_CRED_EXPIRY"); exp != "" {
		fmt.Fprintf(os.Stderr, "[[gateway:credential_expiry]] %s\n", exp)
	}
	respDelay := subprocessRespDelay()
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		if resp, ok := subprocessHandleLine(scanner.Text(), respDelay); ok {
			fmt.Println(resp)
		}
	}
}

// subprocessRespDelay reads the optional per-response delay used to simulate an
// in-flight request.
func subprocessRespDelay() time.Duration {
	d := os.Getenv("TEST_RESP_DELAY_MS")
	if d == "" {
		return 0
	}
	ms, err := strconv.Atoi(d)
	if err != nil {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

// subprocessHandleLine parses one JSON-RPC line and returns the response to
// print (ok=false means no response: blank line, parse error, or notification).
func subprocessHandleLine(line string, respDelay time.Duration) (string, bool) {
	if line == "" {
		return "", false
	}
	var msg struct {
		ID     *json.RawMessage `json:"id"`
		Method string           `json:"method"`
	}
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return "", false
	}
	if msg.ID == nil {
		return "", false // notification, no response needed
	}
	if msg.Method == "tools/list" {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"get_users","description":"Get users"},{"name":"create_issue","description":"Create issue"},{"name":"delete_repo","description":"Delete repo"},{"name":"search_code","description":"Search code"},{"name":"list_items","description":"List items"},{"name":"update_thing","description":"Update thing"},{"name":"custom_action","description":"Custom action"}]}}`, string(*msg.ID)), true
	}
	if respDelay > 0 {
		time.Sleep(respDelay)
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, string(*msg.ID)), true
}

// subprocessCommand returns the command to run the test binary as a fake MCP subprocess.
func subprocessCommand() []string {
	return []string{os.Args[0], "-test.run=TestSubprocessHelper"}
}

// --- Discovery / Category System Tests ---

// --- Idle Reaper Tests ---

// --- Cache Tests ---

// --- matchGlob Tests ---

// --- shutdownAll Test ---

// --- newGateway with disabled backends ---

// --- BearerAuthorizer.IsEnabled Tests ---

// --- sendSSE test with httptest ---

// --- forwardToBackend with real subprocess ---

// --- handleDiscoverTools tests ---

// --- truncateDescription test ---

// --- extractMethod test ---

// --- dispatchResponse tests ---

// --- parseLogLevel test ---

// --- jsonRPCError test ---

// --- handleToolsList from cache ---

// --- notifyToolsChanged ---

// --- rotatingWriter tests ---

// --- initLogging tests ---

// --- resolveConfigPath tests ---

// --- selfIdleTimer test ---

// --- idleReaper test (context-based) ---

// --- pidFile functions ---

// --- checkExistingInstance test ---

// --- handleHealth coverage for running backends ---

// --- handleSubscriptionsListen ---

// --- handleRequest missing backend name ---

// --- handleRequest invalid JSON ---

// --- forwardToBackend retry on send failure ---

// --- sendStreamableHTTP SSE response path ---

// --- sendRemote unknown transport ---

// --- Additional coverage tests ---

// --- Test loadConfig with cache_dir ---

// --- Global discovery config propagation ---

// --- writeResponse empty body ---

// --- Credential TTL watcher tests ---
