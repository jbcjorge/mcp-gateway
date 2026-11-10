package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	errors "github.com/jbcjorge/errors-library"
)

// slowBackend returns a stdio test backend whose every non-list response is
// delayed by delay, simulating a long-blocking call like wait_for_pipeline.
func slowBackend(name string, delay time.Duration) *Backend {
	return &Backend{
		name: name,
		def: BackendDef{Command: subprocessCommand(), Env: map[string]string{
			"TEST_SUBPROCESS":    "1",
			"TEST_RESP_DELAY_MS": strconv.FormatInt(delay.Milliseconds(), 10),
		}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}
}

func backendPid(t *testing.T, b *Backend) int {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.running || b.cmd == nil || b.cmd.Process == nil {
		t.Fatal("backend not running")
	}
	return b.cmd.Process.Pid
}

func TestShouldStreamCall(t *testing.T) {
	timed := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wait_for_pipeline","arguments":{"timeout_seconds":600}}}`)
	untimed := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_pipeline","arguments":{}}}`)
	tests := []struct {
		name   string
		accept string
		method string
		body   []byte
		want   bool
	}{
		{"timed call, SSE accepted", "application/json, text/event-stream", "tools/call", timed, true},
		{"timed call, JSON only", "application/json", "tools/call", timed, false},
		{"untimed call, SSE accepted", "application/json, text/event-stream", "tools/call", untimed, false},
		{"not a tools/call", "text/event-stream", "tools/list", timed, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/x/mcp", nil)
			r.Header.Set("Accept", tt.accept)
			if got := shouldStreamCall(r, tt.method, tt.body); got != tt.want {
				t.Fatalf("shouldStreamCall = %v, want %v", got, tt.want)
			}
		})
	}
}

// A timed tools/call must get headers immediately, keepalive comments while the
// backend works, and finally the JSON-RPC result as an SSE message event.
func TestStreamCall_HeadersFirstKeepalivesThenResult(t *testing.T) {
	orig := streamKeepAliveInterval
	streamKeepAliveInterval = 50 * time.Millisecond
	defer func() { streamKeepAliveInterval = orig }()

	const delay = 600 * time.Millisecond
	gw := newTestGateway(nil)
	b := slowBackend("slow", delay)
	gw.backends["slow"] = b
	gw.routes["slow"] = newRoute("slow", "", []*Backend{b})
	defer b.kill()

	srv := httptest.NewServer(http.HandlerFunc(gw.handleRequest))
	defer srv.Close()

	body := `{"jsonrpc":"2.0","id":"w1","method":"tools/call","params":{"name":"get_users","arguments":{"timeout_seconds":5}}}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/slow/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	headersAfter := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if headersAfter >= delay {
		t.Fatalf("headers arrived after %v; must precede the %v backend delay", headersAfter, delay)
	}

	var keepalives int
	var data string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == ": keepalive":
			keepalives++
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
	if keepalives < 2 {
		t.Fatalf("got %d keepalive comments, want >= 2", keepalives)
	}
	var msg struct {
		ID     string          `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &msg); err != nil {
		t.Fatalf("result event not JSON: %q (%v)", data, err)
	}
	if msg.ID != "w1" || msg.Result == nil || msg.Error != nil {
		t.Fatalf("unexpected result event: %s", data)
	}
}

// A client disconnect must return ErrClientDisconnected without killing or
// restarting the backend.
func TestForwardToBackend_ClientDisconnectKeepsBackend(t *testing.T) {
	b := slowBackend("disc", 3*time.Second)
	gw := &Gateway{backends: map[string]*Backend{"disc": b}}
	if err := b.ensureRunning(); err != nil {
		t.Fatalf("ensureRunning: %v", err)
	}
	defer b.kill()
	pidBefore := backendPid(t, b)

	id := json.RawMessage(`"d1"`)
	envelope := jsonRPCMessage{JSONRPC: "2.0", Method: "tools/call", ID: &id}
	body := []byte(`{"jsonrpc":"2.0","id":"d1","method":"tools/call","params":{"name":"get_users","arguments":{"timeout_seconds":60}}}`)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)

	start := time.Now()
	_, err := gw.forwardToBackend(ctx, b, envelope, body)
	if !errors.Is(err, ErrClientDisconnected) {
		t.Fatalf("err = %v, want ErrClientDisconnected", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("returned after %v; should return promptly on cancel", elapsed)
	}
	if pidAfter := backendPid(t, b); pidAfter != pidBefore {
		t.Fatalf("backend restarted: pid %d -> %d", pidBefore, pidAfter)
	}
}

// The gateway's own deadline (DeadlineExceeded) is still reported as a
// backend timeout, not as a client disconnect.
func TestBackendSend_DeadlineIsTimeoutNotDisconnect(t *testing.T) {
	b := slowBackend("dl", 3*time.Second)
	if err := b.ensureRunning(); err != nil {
		t.Fatalf("ensureRunning: %v", err)
	}
	defer b.kill()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := b.send(ctx, []byte(`{"jsonrpc":"2.0","id":"t1","method":"ping","params":{}}`))
	if !errors.Is(err, ErrBackendTimeout) {
		t.Fatalf("err = %v, want ErrBackendTimeout", err)
	}
	if errors.Is(err, ErrClientDisconnected) {
		t.Fatal("deadline misreported as client disconnect")
	}
}
