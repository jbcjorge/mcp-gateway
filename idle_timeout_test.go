package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestResolveIdleTimeout(t *testing.T) {
	zero := 0
	sixty := 60
	negative := -5
	tests := []struct {
		name      string
		perServer *int
		want      time.Duration
	}{
		{"nil inherits global", nil, inheritGlobalIdle},
		{"zero means never reap", &zero, neverReapIdle},
		{"positive is seconds", &sixty, 60 * time.Second},
		{"negative treated as never reap", &negative, neverReapIdle},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveIdleTimeout(tt.perServer)
			if got != tt.want {
				t.Errorf("resolveIdleTimeout(%v) = %v, want %v", tt.perServer, got, tt.want)
			}
		})
	}
}

func TestEffectiveIdleTimeout(t *testing.T) {
	const global = 300 * time.Second
	tests := []struct {
		name       string
		perBackend time.Duration
		global     time.Duration
		want       time.Duration
	}{
		{"inherit uses global", inheritGlobalIdle, global, global},
		{"inherit with zero global is never", inheritGlobalIdle, 0, 0},
		{"override shorter than global", 30 * time.Second, global, 30 * time.Second},
		{"override longer than global", 3600 * time.Second, global, 3600 * time.Second},
		{"override never means never even if global set", neverReapIdle, global, neverReapIdle},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := effectiveIdleTimeout(tt.perBackend, tt.global)
			if got != tt.want {
				t.Errorf("effectiveIdleTimeout(%v, %v) = %v, want %v", tt.perBackend, tt.global, got, tt.want)
			}
		})
	}
}

func TestShouldReapLocked(t *testing.T) {
	now := time.Now()
	const global = 5 * time.Minute
	tests := []struct {
		name        string
		running     bool
		lastUsed    time.Time
		idleTimeout time.Duration
		want        bool
	}{
		{"idle past global reaps", true, now.Add(-10 * time.Minute), inheritGlobalIdle, true},
		{"recent keeps alive", true, now.Add(-1 * time.Minute), inheritGlobalIdle, false},
		{"not running never reaps", false, now.Add(-10 * time.Minute), inheritGlobalIdle, false},
		{"zero lastUsed never reaps", true, time.Time{}, inheritGlobalIdle, false},
		{"per-backend never keeps alive despite long idle", true, now.Add(-1 * time.Hour), neverReapIdle, false},
		{"per-backend longer than idle keeps alive", true, now.Add(-10 * time.Minute), 1 * time.Hour, false},
		{"per-backend shorter than idle reaps", true, now.Add(-2 * time.Minute), 1 * time.Minute, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &Backend{
				running:     tt.running,
				lastUsed:    tt.lastUsed,
				idleTimeout: tt.idleTimeout,
			}
			got := b.shouldReapLocked(now, global)
			if got != tt.want {
				t.Errorf("shouldReapLocked() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMinBackendIdleTimeout(t *testing.T) {
	tests := []struct {
		name     string
		timeouts []time.Duration
		want     time.Duration
	}{
		{"none set", []time.Duration{inheritGlobalIdle, inheritGlobalIdle}, 0},
		{"single override", []time.Duration{inheritGlobalIdle, 60 * time.Second}, 60 * time.Second},
		{"smallest wins", []time.Duration{120 * time.Second, 30 * time.Second, 90 * time.Second}, 30 * time.Second},
		{"never ignored", []time.Duration{neverReapIdle, 45 * time.Second}, 45 * time.Second},
		{"all never or inherit", []time.Duration{neverReapIdle, inheritGlobalIdle}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw := &Gateway{backends: make(map[string]*Backend)}
			for i, to := range tt.timeouts {
				name := string(rune('a' + i))
				gw.backends[name] = &Backend{name: name, idleTimeout: to}
			}
			got := gw.minBackendIdleTimeout()
			if got != tt.want {
				t.Errorf("minBackendIdleTimeout() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestReapIdleBackends_PerBackendNeverSurvives verifies a backend with
// idle_timeout_seconds: 0 (never reap) is NOT killed even when it has been idle
// far longer than the global timeout. This is the Chrome CDP use case.
func TestReapIdleBackends_PerBackendNeverSurvives(t *testing.T) {
	never := &Backend{
		name:        "chrome",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
		idleTimeout: neverReapIdle, // never reap
	}

	never.mu.Lock()
	if err := never.spawnProcess(); err != nil {
		never.mu.Unlock()
		t.Fatalf("spawnProcess: %v", err)
	}
	never.lastUsed = time.Now().Add(-1 * time.Hour) // very idle
	never.mu.Unlock()
	defer never.kill()

	gw := &Gateway{backends: map[string]*Backend{"chrome": never}}
	gw.reapIdleBackends(5 * time.Minute)

	if !never.isRunning() {
		t.Error("backend with idle_timeout_seconds=0 should NOT be reaped")
	}
}

// TestReapIdleBackends_PerBackendOverrideReaps verifies a backend with a short
// per-backend timeout is reaped on its own schedule even when the global
// timeout would keep it alive.
func TestReapIdleBackends_PerBackendOverrideReaps(t *testing.T) {
	short := &Backend{
		name:        "short-lived",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
		idleTimeout: 1 * time.Minute, // shorter than the global below
	}

	short.mu.Lock()
	if err := short.spawnProcess(); err != nil {
		short.mu.Unlock()
		t.Fatalf("spawnProcess: %v", err)
	}
	short.lastUsed = time.Now().Add(-2 * time.Minute) // past the 1m per-backend timeout
	short.mu.Unlock()

	// Global is a long 1h; without the per-backend override this would survive.
	gw := &Gateway{backends: map[string]*Backend{"short-lived": short}}
	gw.reapIdleBackends(1 * time.Hour)

	// Wait for the graceful stop to complete.
	waitUntil(2*time.Second, func() bool { return !short.isRunning() })

	if short.isRunning() {
		t.Error("backend with short per-backend idle timeout should have been reaped")
	}
}

func TestReaperInterval(t *testing.T) {
	tests := []struct {
		name        string
		global      time.Duration
		backendIdle []time.Duration
		want        time.Duration
	}{
		{"global drives interval", 300 * time.Second, nil, 30 * time.Second},
		{"global floored at minimum", 50 * time.Second, nil, minReaperInterval},
		{"no global uses smallest backend", 0, []time.Duration{600 * time.Second}, 60 * time.Second},
		{"no global and no backend floors at minimum", 0, nil, minReaperInterval},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw := &Gateway{backends: make(map[string]*Backend)}
			for i, to := range tt.backendIdle {
				name := string(rune('a' + i))
				gw.backends[name] = &Backend{name: name, idleTimeout: to}
			}
			got := gw.reaperInterval(tt.global)
			if got != tt.want {
				t.Errorf("reaperInterval(%v) = %v, want %v", tt.global, got, tt.want)
			}
		})
	}
}
