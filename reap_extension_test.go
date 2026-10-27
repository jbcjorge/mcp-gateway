package main

import (
	"encoding/json"
	"testing"
	"time"
)

func rawArgs(t *testing.T, m map[string]any) map[string]json.RawMessage {
	t.Helper()
	out := map[string]json.RawMessage{}
	for k, v := range m {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %q: %v", k, err)
		}
		out[k] = b
	}
	return out
}

func TestResponseWait(t *testing.T) {
	tests := []struct {
		name     string
		declared time.Duration
		want     time.Duration
	}{
		{"no declared timeout uses default", 0, defaultResponseTimeout},
		{"negative treated as none", -5 * time.Second, defaultResponseTimeout},
		{"declared plus headroom", 300 * time.Second, 300*time.Second + responseHeadroom},
		{"wait_for_job 300s", 300 * time.Second, 330 * time.Second},
		{"absurd clamped to cap", 10 * time.Hour, maxResponseTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := responseWait(tt.declared); got != tt.want {
				t.Errorf("responseWait(%v) = %v, want %v", tt.declared, got, tt.want)
			}
		})
	}
}

func TestParseCallTimeout(t *testing.T) {
	tests := []struct {
		name    string
		args    map[string]any
		wantDur time.Duration
		wantKey string
		wantOK  bool
	}{
		{"no timeout key", map[string]any{"project_id": 84430}, 0, "", false},
		{"timeout_seconds", map[string]any{"timeout_seconds": 420}, 420 * time.Second, "timeout_seconds", true},
		{"plain timeout", map[string]any{"timeout": 90}, 90 * time.Second, "timeout", true},
		{"milliseconds normalized", map[string]any{"timeout_ms": 5000}, 5 * time.Second, "timeout_ms", true},
		{"largest wins across keys", map[string]any{"timeout": 30, "max_wait_seconds": 600}, 600 * time.Second, "max_wait_seconds", true},
		{"string value ignored", map[string]any{"timeout": "soon"}, 0, "", false},
		{"fractional seconds", map[string]any{"timeout_seconds": 1.5}, 1500 * time.Millisecond, "timeout_seconds", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, key, ok := parseCallTimeout(rawArgs(t, tt.args))
			if ok != tt.wantOK || d != tt.wantDur || key != tt.wantKey {
				t.Errorf("parseCallTimeout(%v) = (%v,%q,%v), want (%v,%q,%v)",
					tt.args, d, key, ok, tt.wantDur, tt.wantKey, tt.wantOK)
			}
		})
	}
}

func TestExtendedReapDeadline(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	idle := 300 * time.Second
	tests := []struct {
		name     string
		declared time.Duration
		wantFrom time.Duration // expected offset from now
	}{
		{"wait_for_pipeline 420s", 420 * time.Second, 420*time.Second + idle},
		{"tiny clamped to floor", 5 * time.Second, reapExtensionFloor + idle},
		{"absurd clamped to cap", 10 * time.Hour, reapExtensionCap + idle},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extendedReapDeadline(now, tt.declared, idle)
			want := now.Add(tt.wantFrom)
			if !got.Equal(want) {
				t.Errorf("extendedReapDeadline(%v) = %v, want %v", tt.declared, got, want)
			}
		})
	}
}

func TestShouldReapLocked_Guards(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	global := 300 * time.Second
	staleLastUsed := now.Add(-400 * time.Second) // well past the idle timeout

	newBackend := func() *Backend {
		return &Backend{
			name:     "gitlab",
			running:  true,
			lastUsed: staleLastUsed,
			pending:  map[string]chan json.RawMessage{},
		}
	}

	t.Run("stale and quiet is reaped", func(t *testing.T) {
		b := newBackend()
		if !b.shouldReapLocked(now, global) {
			t.Fatal("expected reap for a stale, quiet backend")
		}
	})

	t.Run("in-flight request blocks reap", func(t *testing.T) {
		b := newBackend()
		b.pending["id-1"] = make(chan json.RawMessage, 1)
		if b.shouldReapLocked(now, global) {
			t.Fatal("expected NO reap while a request is in flight")
		}
	})

	t.Run("reapNotBefore in future blocks reap", func(t *testing.T) {
		b := newBackend()
		b.reapNotBefore = now.Add(time.Minute)
		if b.shouldReapLocked(now, global) {
			t.Fatal("expected NO reap before reapNotBefore")
		}
	})

	t.Run("reapNotBefore in past allows reap", func(t *testing.T) {
		b := newBackend()
		b.reapNotBefore = now.Add(-time.Second)
		if !b.shouldReapLocked(now, global) {
			t.Fatal("expected reap once reapNotBefore has passed")
		}
	})
}
