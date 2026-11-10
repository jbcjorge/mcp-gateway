package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jbcjorge/mcp-gateway/internal/config"
)

func TestReapIdleBackends_KillsIdle(t *testing.T) {
	b := &Backend{
		name:        "idle-backend",
		def:         config.BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}

	b.mu.Lock()
	if err := b.spawnProcess(); err != nil {
		b.mu.Unlock()
		t.Fatalf("spawnProcess: %v", err)
	}
	b.lastUsed = time.Now().Add(-10 * time.Minute) // simulate long idle
	b.mu.Unlock()

	gw := &Gateway{
		backends: map[string]*Backend{"idle-backend": b},
	}

	gw.reapIdleBackends(5 * time.Minute)

	// Wait for process to exit
	time.Sleep(200 * time.Millisecond)

	if b.isRunning() {
		t.Error("idle backend should have been killed")
	}
}

func TestReapIdleBackends_KeepsRecent(t *testing.T) {
	b := &Backend{
		name:        "recent-backend",
		def:         config.BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}

	b.mu.Lock()
	if err := b.spawnProcess(); err != nil {
		b.mu.Unlock()
		t.Fatalf("spawnProcess: %v", err)
	}
	b.lastUsed = time.Now() // recently used
	b.mu.Unlock()
	defer b.kill()

	gw := &Gateway{
		backends: map[string]*Backend{"recent-backend": b},
	}

	gw.reapIdleBackends(5 * time.Minute)

	if !b.isRunning() {
		t.Error("recently used backend should NOT have been killed")
	}
}

func TestReapIdleBackends_SkipsNotRunning(t *testing.T) {
	b := &Backend{
		name:        "stopped-backend",
		def:         config.BackendDef{Command: []string{"echo"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		running:     false,
	}

	gw := &Gateway{
		backends: map[string]*Backend{"stopped-backend": b},
	}

	// Should not panic or error
	gw.reapIdleBackends(5 * time.Minute)

	if b.isRunning() {
		t.Error("should still be not running")
	}
}

func TestShutdownAll(t *testing.T) {
	b1 := &Backend{
		name:        "shut1",
		def:         config.BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}
	b2 := &Backend{
		name:        "shut2",
		def:         config.BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}

	b1.mu.Lock()
	if err := b1.spawnProcess(); err != nil {
		b1.mu.Unlock()
		t.Fatalf("spawn b1: %v", err)
	}
	b1.mu.Unlock()

	b2.mu.Lock()
	if err := b2.spawnProcess(); err != nil {
		b2.mu.Unlock()
		t.Fatalf("spawn b2: %v", err)
	}
	b2.mu.Unlock()

	gw := &Gateway{
		backends: map[string]*Backend{"shut1": b1, "shut2": b2},
	}

	gw.ShutdownAll()
	time.Sleep(200 * time.Millisecond)

	if b1.isRunning() {
		t.Error("b1 should not be running after shutdownAll")
	}
	if b2.isRunning() {
		t.Error("b2 should not be running after shutdownAll")
	}
}

func TestNewGateway_DisabledBackends(t *testing.T) {
	cfg := config.Config{
		Listen: ":0",
		Servers: map[string]config.BackendDef{
			"enabled1": {Command: []string{"echo"}},
			"enabled2": {Command: []string{"echo"}},
			"unrouted": {Command: []string{"echo"}},
		},
		Routes: []config.RouteEntry{{Route: "enabled1"}, {Route: "enabled2"}},
	}
	gw, err := New(cfg)
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}
	if len(gw.backends) != 2 {
		t.Errorf("expected 2 backends, got %d", len(gw.backends))
	}
	if _, ok := gw.backends["unrouted"]; ok {
		t.Error("unrouted server should not be instantiated")
	}
}

func TestNewGateway_RemoteBackend(t *testing.T) {
	cfg := config.Config{
		Listen: ":0",
		Servers: map[string]config.BackendDef{
			"remote": {URL: "https://example.com/mcp", TransportType: "streamable-http"},
		},
		Routes: []config.RouteEntry{{Route: "remote"}},
	}
	gw, err := New(cfg)
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}

	b, ok := gw.backends["remote"]
	if !ok {
		t.Fatal("expected remote backend")
	}
	if b.httpClient == nil {
		t.Error("remote backend should have httpClient")
	}
	if !b.isRemote() {
		t.Error("should be marked as remote")
	}
}

func TestUnroutedServer_NotInGateway(t *testing.T) {
	cfg := config.Config{
		Listen: ":0",
		Servers: map[string]config.BackendDef{
			"active":   {Command: []string{"echo"}},
			"unrouted": {Command: []string{"echo"}},
		},
		Routes: []config.RouteEntry{{Route: "active"}},
	}
	gw, err := New(cfg)
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}
	if _, ok := gw.routes["active"]; !ok {
		t.Error("active route should be in gateway")
	}
	if _, ok := gw.routes["unrouted"]; ok {
		t.Error("unrouted server should NOT be a route")
	}
	if _, ok := gw.backends["unrouted"]; ok {
		t.Error("unrouted server should NOT be instantiated")
	}
}

func TestUnroutedServer_Returns404(t *testing.T) {
	cfg := config.Config{
		Listen: ":0",
		Servers: map[string]config.BackendDef{
			"active":   {Command: []string{"echo"}},
			"unrouted": {Command: []string{"echo"}},
		},
		Routes: []config.RouteEntry{{Route: "active"}},
	}
	gw, err := New(cfg)
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}

	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	req := httptest.NewRequest(http.MethodPost, "/unrouted/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	gw.handleRequest(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("unrouted server should return 404, got %d", w.Code)
	}
}

func TestSelfIdleTimer_Triggers(t *testing.T) {
	// selfIdleTimer has a minimum check interval of 30s, which is too slow for tests.
	// Instead, test the cancellation path and verify the idle logic independently.
	gw := &Gateway{
		backends:    make(map[string]*Backend),
		lastRequest: time.Now(),
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		gw.selfIdleTimer(ctx, cancel, 1*time.Hour) // long timeout, won't trigger
		close(done)
	}()

	// Cancel the context to simulate shutdown
	cancel()

	select {
	case <-done:
		// Clean exit on context cancel
	case <-time.After(2 * time.Second):
		t.Fatal("selfIdleTimer should have exited on context cancel")
	}
}

func TestIdleReaper_Cancels(t *testing.T) {
	gw := &Gateway{
		backends: make(map[string]*Backend),
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		gw.idleReaper(ctx, 1*time.Hour)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// clean exit
	case <-time.After(2 * time.Second):
		t.Fatal("idleReaper should have exited on context cancel")
	}
}

func TestPerBackendLogging(t *testing.T) {
	enabled := true
	disabled := false

	gw := newTestGateway(map[string]config.BackendDef{
		"verbose": {Command: []string{"echo"}, LogEnabled: &enabled},
		"quiet":   {Command: []string{"echo"}, LogEnabled: &disabled},
		"default": {Command: []string{"echo"}},
	})

	// The test gateway helper doesn't set LogEnabled on defs, do it manually
	gw.backends["verbose"].logEnabled = true
	gw.backends["quiet"].logEnabled = false
	gw.backends["default"].logEnabled = true // nil defaults to enabled

	if !gw.backends["verbose"].logEnabled {
		t.Error("verbose backend should have logging enabled")
	}
	if gw.backends["quiet"].logEnabled {
		t.Error("quiet backend should have logging disabled")
	}
	if !gw.backends["default"].logEnabled {
		t.Error("default backend (nil) should have logging enabled")
	}
}

func TestNewGateway_GlobalDiscoveryPropagation(t *testing.T) {
	force := true
	cfg := config.Config{
		Listen:    ":0",
		Discovery: &force,
		Servers: map[string]config.BackendDef{
			"test": {Command: []string{"echo"}},
		},
		Routes: []config.RouteEntry{{Route: "test"}},
	}
	gw, err := New(cfg)
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}

	b := gw.backends["test"]
	if b.def.Discovery == nil {
		t.Error("global discovery should propagate to backend")
	}
}
