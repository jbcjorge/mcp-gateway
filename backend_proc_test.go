package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSpawnAndSend(t *testing.T) {
	b := &Backend{
		name:        "test-spawn",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}

	b.mu.Lock()
	err := b.spawnProcess()
	b.mu.Unlock()
	if err != nil {
		t.Fatalf("spawnProcess: %v", err)
	}
	defer b.kill()

	if !b.isRunning() {
		t.Fatal("expected backend to be running after spawn")
	}

	// Send a JSON-RPC message and get a response
	msg := []byte(`{"jsonrpc":"2.0","id":"test1","method":"ping","params":{}}`)
	resp, err := b.send(context.Background(), msg)
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(resp, &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result["id"] == nil {
		t.Error("expected id in response")
	}
}

func TestSendToDeadBackend(t *testing.T) {
	b := &Backend{
		name:        "dead-backend",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}

	// Never started, so not running
	msg := []byte(`{"jsonrpc":"2.0","id":"x","method":"ping"}`)
	_, err := b.send(context.Background(), msg)
	if err == nil {
		t.Fatal("expected error sending to dead backend")
	}
}

func TestSendNotification(t *testing.T) {
	b := &Backend{
		name:        "test-notif",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}

	b.mu.Lock()
	err := b.spawnProcess()
	b.mu.Unlock()
	if err != nil {
		t.Fatalf("spawnProcess: %v", err)
	}
	defer b.kill()

	// Notifications have no id - should return empty
	msg := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	resp, err := b.send(context.Background(), msg)
	if err != nil {
		t.Fatalf("send notification: %v", err)
	}
	if string(resp) != "{}" {
		t.Errorf("expected empty response for notification, got %s", resp)
	}
}

func TestKillAndWaitForExit(t *testing.T) {
	b := &Backend{
		name:        "test-kill",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}

	b.mu.Lock()
	err := b.spawnProcess()
	b.mu.Unlock()
	if err != nil {
		t.Fatalf("spawnProcess: %v", err)
	}

	if !b.isRunning() {
		t.Fatal("expected running before kill")
	}

	b.kill()
	// Wait a moment for waitForExit goroutine
	time.Sleep(200 * time.Millisecond)

	if b.isRunning() {
		t.Error("expected not running after kill")
	}
}

func TestSendAfterKill(t *testing.T) {
	b := &Backend{
		name:        "test-send-killed",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}

	b.mu.Lock()
	err := b.spawnProcess()
	b.mu.Unlock()
	if err != nil {
		t.Fatalf("spawnProcess: %v", err)
	}

	b.kill()
	time.Sleep(200 * time.Millisecond)

	msg := []byte(`{"jsonrpc":"2.0","id":"y","method":"ping"}`)
	_, err = b.send(context.Background(), msg)
	if err == nil {
		t.Fatal("expected error sending to killed backend")
	}
}

func TestInitializeBackend(t *testing.T) {
	b := &Backend{
		name:        "test-init",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}

	b.mu.Lock()
	err := b.spawnProcess()
	b.mu.Unlock()
	if err != nil {
		t.Fatalf("spawnProcess: %v", err)
	}
	defer b.kill()

	// initializeBackend sends initialize + notifications/initialized
	b.initializeBackend()

	// Backend should still be running after init
	if !b.isRunning() {
		t.Error("backend should still be running after initializeBackend")
	}
}

func TestEnsureRunning(t *testing.T) {
	b := &Backend{
		name:        "test-ensure",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
	}

	if b.isRunning() {
		t.Fatal("should not be running initially")
	}

	if err := b.ensureRunning(); err != nil {
		t.Fatalf("ensureRunning: %v", err)
	}
	defer b.kill()

	if !b.isRunning() {
		t.Fatal("should be running after ensureRunning")
	}

	// Calling again should be a no-op
	if err := b.ensureRunning(); err != nil {
		t.Fatalf("second ensureRunning: %v", err)
	}
}

func TestEnsureRunningNoCommand(t *testing.T) {
	b := &Backend{
		name:        "test-no-cmd",
		def:         BackendDef{},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
	}

	err := b.ensureRunning()
	if err == nil {
		t.Fatal("expected error for backend with no command or url")
	}
}

func TestEnsureRunningRemote(t *testing.T) {
	b := &Backend{
		name:        "test-remote-ensure",
		def:         BackendDef{URL: "https://example.com/mcp"},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		logEnabled:  true,
	}

	if err := b.ensureRunning(); err != nil {
		t.Fatalf("ensureRunning remote: %v", err)
	}
	if !b.isRunning() {
		t.Fatal("remote backend should be running after ensureRunning")
	}
}

func TestDispatchResponse_MatchesPending(t *testing.T) {
	b := &Backend{
		name:    "dispatch-test",
		pending: make(map[string]chan json.RawMessage),
	}

	ch := make(chan json.RawMessage, 1)
	b.pending[`"req1"`] = ch

	line := []byte(`{"jsonrpc":"2.0","id":"req1","result":{}}`)
	b.dispatchResponse(line)

	select {
	case resp := <-ch:
		if resp == nil {
			t.Error("expected response data")
		}
	default:
		t.Error("expected response in channel")
	}
}

func TestDispatchResponse_NoID(t *testing.T) {
	b := &Backend{
		name:    "dispatch-noid",
		pending: make(map[string]chan json.RawMessage),
	}

	// Should not panic
	line := []byte(`{"jsonrpc":"2.0","method":"notification"}`)
	b.dispatchResponse(line)
}

func TestDispatchResponse_InvalidJSON(t *testing.T) {
	b := &Backend{
		name:    "dispatch-invalid",
		pending: make(map[string]chan json.RawMessage),
	}

	// Should not panic
	b.dispatchResponse([]byte(`not json at all`))
}

func TestDispatchResponse_UnmatchedID(t *testing.T) {
	b := &Backend{
		name:    "dispatch-unmatched",
		pending: make(map[string]chan json.RawMessage),
	}

	// No pending request for this id - should not panic
	line := []byte(`{"jsonrpc":"2.0","id":"orphan","result":{}}`)
	b.dispatchResponse(line)
}

// newTTLBackend spawns a helper backend that emits a credential_expiry sentinel
// (expiry set relative to now) with the given soft/hard margins.
func newTTLBackend(t *testing.T, expiry time.Time, soft, hard time.Duration, respDelayMS int) *Backend {
	t.Helper()
	env := map[string]string{
		"TEST_SUBPROCESS":  "1",
		"TEST_CRED_EXPIRY": fmt.Sprintf("%d", expiry.Unix()),
	}
	if respDelayMS > 0 {
		env["TEST_RESP_DELAY_MS"] = fmt.Sprintf("%d", respDelayMS)
	}
	b := &Backend{
		name:          "ttl-backend",
		def:           BackendDef{Command: subprocessCommand(), Env: env},
		pending:       make(map[string]chan json.RawMessage),
		activeTools:   make(map[string]bool),
		logEnabled:    true,
		ttlSoftMargin: soft,
		ttlHardGuard:  hard,
	}
	b.mu.Lock()
	if err := b.spawnProcess(); err != nil {
		b.mu.Unlock()
		t.Fatalf("spawnProcess: %v", err)
	}
	b.lastUsed = time.Now()
	b.mu.Unlock()
	return b
}

func TestCredentialTTL_SentinelParsed(t *testing.T) {
	// Expiry far in the future so no timer fires; just verify parsing/arming.
	expiry := time.Now().Add(1 * time.Hour)
	b := newTTLBackend(t, expiry, 5*time.Minute, 2*time.Minute, 0)
	defer b.kill()

	// Give the stderr scanner a moment to read the sentinel line.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		set := !b.credentialExpiry.IsZero()
		b.mu.Unlock()
		if set {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	b.mu.Lock()
	got := b.credentialExpiry
	b.mu.Unlock()
	if got.IsZero() {
		t.Fatal("credentialExpiry should have been set from the sentinel")
	}
	if d := got.Unix() - expiry.Unix(); d != 0 {
		t.Errorf("credentialExpiry mismatch: got %d want %d", got.Unix(), expiry.Unix())
	}
	if b.isRunning() != true {
		t.Error("backend should still be running (expiry far in future)")
	}
}

func TestCredentialTTL_SoftDeadlineRecyclesWhenIdle(t *testing.T) {
	// soft deadline = expiry - soft. Put it ~150ms in the future.
	soft := 500 * time.Millisecond
	hard := 100 * time.Millisecond
	expiry := time.Now().Add(soft + 150*time.Millisecond)
	b := newTTLBackend(t, expiry, soft, hard, 0)
	defer b.kill()

	// No pending requests -> should recycle at the soft deadline.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !b.isRunning() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if b.isRunning() {
		t.Error("idle backend should have been recycled at soft deadline")
	}
}

func TestCredentialTTL_DefersWhileBusyThenRecycles(t *testing.T) {
	// Response delayed 400ms. Soft deadline fires early (~immediately) while the
	// request is in flight, so recycle must be deferred until the response
	// drains. The hard guard is set to land well after the response completes so
	// it does NOT force-kill first; the drain path must do the recycle.
	respDelayMS := 400
	soft := 300 * time.Millisecond
	hard := 100 * time.Millisecond
	// expiry - soft ~= now (soft fires ~immediately).
	// expiry - hard = soft - hard + 1200ms ~= 1400ms out, safely after the 400ms
	// response completes.
	expiry := time.Now().Add(soft + 1200*time.Millisecond)
	b := newTTLBackend(t, expiry, soft, hard, respDelayMS)
	defer b.kill()

	// Fire a request that the backend will take 400ms to answer.
	respDone := make(chan struct{})
	go func() {
		_, _ = b.send(context.Background(), []byte(`{"jsonrpc":"2.0","id":"busy1","method":"tools/call","params":{}}`))
		close(respDone)
	}()

	// Give the soft deadline time to fire while the request is in flight; the
	// backend must still be running (kill deferred, not forced).
	time.Sleep(200 * time.Millisecond)
	if !b.isRunning() {
		t.Fatal("backend should NOT be recycled while a request is in flight")
	}

	// Wait for the response to complete, which triggers the deferred recycle.
	select {
	case <-respDone:
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight request did not complete")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !b.isRunning() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if b.isRunning() {
		t.Error("backend should have been recycled after the in-flight request drained")
	}
}

func TestCredentialTTL_HardDeadlineForceKillsWhileBusy(t *testing.T) {
	// Response delayed very long; hard deadline must force-kill even though busy.
	soft := 200 * time.Millisecond
	hard := 100 * time.Millisecond
	// hard deadline = expiry - hard. Put it ~300ms out. Response takes 5s.
	expiry := time.Now().Add(hard + 300*time.Millisecond)
	b := newTTLBackend(t, expiry, soft, hard, 5000)
	defer b.kill()

	go func() {
		_, _ = b.send(context.Background(), []byte(`{"jsonrpc":"2.0","id":"busy2","method":"tools/call","params":{}}`))
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !b.isRunning() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if b.isRunning() {
		t.Error("busy backend should have been force-killed at hard deadline")
	}
}

func TestCredentialTTL_TimersStoppedOnExit(t *testing.T) {
	// Arm timers with a future soft deadline, then kill the backend explicitly.
	// After exit, the timers must be nil (no orphan watcher).
	soft := 5 * time.Second
	hard := 2 * time.Second
	expiry := time.Now().Add(10 * time.Second)
	b := newTTLBackend(t, expiry, soft, hard, 0)

	// Wait until timers are armed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		armed := b.softTimer != nil || b.hardTimer != nil
		b.mu.Unlock()
		if armed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	b.kill()
	time.Sleep(300 * time.Millisecond) // let waitForExit run

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.softTimer != nil || b.hardTimer != nil {
		t.Error("TTL timers should be stopped and cleared after backend exit")
	}
	if b.pendingTTLKill {
		t.Error("pendingTTLKill should be cleared after backend exit")
	}
}

func TestParseCredentialExpiry(t *testing.T) {
	cases := []struct {
		name  string
		line  string
		want  int64
		wantK bool
	}{
		{"valid", "[[gateway:credential_expiry]] 1788454542", 1788454542, true},
		{"valid with prefix noise", "INFO foo [[gateway:credential_expiry]] 42 trailing", 42, true},
		{"no sentinel", "some unrelated log line", 0, false},
		{"empty value", "[[gateway:credential_expiry]]   ", 0, false},
		{"non-numeric", "[[gateway:credential_expiry]] abc", 0, false},
		{"zero", "[[gateway:credential_expiry]] 0", 0, false},
		{"negative", "[[gateway:credential_expiry]] -5", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseCredentialExpiry([]byte(c.line))
			if got != c.want || ok != c.wantK {
				t.Errorf("parseCredentialExpiry(%q) = (%d,%v), want (%d,%v)", c.line, got, ok, c.want, c.wantK)
			}
		})
	}
}

func TestScanStderr_ArmsTTLFromSentinel(t *testing.T) {
	b := &Backend{
		name:        "scan-backend",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
	}
	b.mu.Lock()
	if err := b.spawnProcess(); err != nil {
		b.mu.Unlock()
		t.Fatalf("spawnProcess: %v", err)
	}
	pid := b.cmd.Process.Pid
	b.mu.Unlock()
	defer b.kill()

	// Feed a sentinel line through scanStderr directly (far-future expiry so no
	// timer fires); it should set credentialExpiry and arm timers.
	future := time.Now().Add(2 * time.Hour).Unix()
	r := strings.NewReader("noise line\n[[gateway:credential_expiry]] " + fmt.Sprintf("%d", future) + "\n")
	done := make(chan struct{})
	go func() { b.scanStderr(r, pid); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scanStderr did not return")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.credentialExpiry.Unix() != future {
		t.Errorf("credentialExpiry = %d, want %d", b.credentialExpiry.Unix(), future)
	}
	if b.softTimer == nil || b.hardTimer == nil {
		t.Error("timers should be armed after sentinel")
	}
}

func TestArmCredentialTTL_DefaultMargins(t *testing.T) {
	b := &Backend{
		name:        "defaults-backend",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		// ttlSoftMargin / ttlHardGuard left zero -> defaults (5m / 2m) apply.
	}
	b.mu.Lock()
	if err := b.spawnProcess(); err != nil {
		b.mu.Unlock()
		t.Fatalf("spawnProcess: %v", err)
	}
	pid := b.cmd.Process.Pid
	b.mu.Unlock()
	defer b.kill()

	// Far-future expiry so nothing fires; just exercise the default-margin path.
	b.armCredentialTTL(time.Now().Add(3*time.Hour), pid)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.softTimer == nil || b.hardTimer == nil {
		t.Error("timers should be armed with default margins")
	}
}

func TestSendStreamableHTTP_SSEResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env jsonRPCMessage
		json.Unmarshal(body, &env)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		resp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"streamed":true}}`, string(*env.ID))
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", resp)
	}))
	defer srv.Close()

	b := &Backend{
		name:       "http-sse",
		def:        BackendDef{URL: srv.URL, TransportType: "streamable-http"},
		httpClient: &http.Client{Timeout: 5 * time.Second},
		logEnabled: true,
	}

	msg := []byte(`{"jsonrpc":"2.0","id":"sh1","method":"tools/list","params":{}}`)
	resp, err := b.sendStreamableHTTP(context.Background(), msg)
	if err != nil {
		t.Fatalf("sendStreamableHTTP (SSE): %v", err)
	}

	var result map[string]any
	json.Unmarshal(resp, &result)
	r := result["result"].(map[string]any)
	if r["streamed"] != true {
		t.Error("expected streamed=true in response")
	}
}

func TestSendRemote_UnknownTransport(t *testing.T) {
	b := &Backend{
		name:       "bad-transport",
		def:        BackendDef{URL: "https://example.com", TransportType: "grpc"},
		httpClient: &http.Client{Timeout: 5 * time.Second},
		logEnabled: true,
	}
	b.mu.Lock()
	b.lastUsed = time.Now()
	b.mu.Unlock()

	msg := []byte(`{"jsonrpc":"2.0","id":"x","method":"ping"}`)
	_, err := b.sendRemote(context.Background(), msg)
	if err == nil {
		t.Fatal("expected error for unknown transport")
	}
}

func TestParseSSEResponse_NoResponse(t *testing.T) {
	b := &Backend{name: "sse-empty", logEnabled: true}

	// Empty SSE stream with no data lines
	reader := strings.NewReader("")
	_, err := b.parseSSEResponse(reader)
	if err == nil {
		t.Error("expected error for empty SSE stream")
	}
}

func TestParseSSEResponse_NotificationOnly(t *testing.T) {
	b := &Backend{name: "sse-notif", logEnabled: true}

	// SSE with only a notification (no id field), then stream ends with unterminated data
	input := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notification\"}\n\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"final\",\"result\":{}}"
	reader := strings.NewReader(input)

	data, err := b.parseSSEResponse(reader)
	if err != nil {
		t.Fatalf("parseSSEResponse: %v", err)
	}
	// Should get the unterminated data line (fallback behavior)
	if len(data) == 0 {
		t.Error("expected data from unterminated SSE")
	}
}

func TestSendStreamableHTTP_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	b := &Backend{
		name:       "http-err",
		def:        BackendDef{URL: srv.URL, TransportType: "streamable-http"},
		httpClient: &http.Client{Timeout: 5 * time.Second},
		logEnabled: true,
	}

	msg := []byte(`{"jsonrpc":"2.0","id":"e1","method":"ping"}`)
	_, err := b.sendStreamableHTTP(context.Background(), msg)
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
}

func TestSendSSE_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	b := &Backend{
		name:       "sse-err",
		def:        BackendDef{URL: srv.URL, TransportType: "sse"},
		httpClient: &http.Client{Timeout: 5 * time.Second},
		logEnabled: true,
	}

	msg := []byte(`{"jsonrpc":"2.0","id":"e2","method":"ping"}`)
	_, err := b.sendSSE(context.Background(), msg)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}
