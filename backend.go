package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	errors "github.com/jbcjorge/errors-library"
)

// spawnProcess starts the subprocess and sets up stdin/stdout pipes.
// Must be called with b.mu held. Does NOT release the lock.
func (b *Backend) spawnProcess() error {
	ctx, cancel := context.WithCancel(context.Background())
	b.cancelFn = cancel

	cmd := exec.CommandContext(ctx, b.def.Command[0], b.def.Command[1:]...) // #nosec G204 -- command from admin config, subprocess management is this tool's purpose

	cmd.Env = os.Environ()
	for k, v := range b.globalEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	for k, v := range b.def.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return ErrStdinPipe.Parse(errors.WithError(err))
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return ErrStdoutPipe.Parse(errors.WithError(err))
	}

	// Capture stderr so we can watch for the credential-expiry sentinel while
	// still forwarding all stderr output to the gateway's own stderr (keeps
	// backend logs visible for debugging).
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return ErrStderrPipe.Parse(errors.WithError(err))
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return ErrCmdStart.Parse(errors.WithError(err), errors.WithSafeData(map[string]any{"command": b.def.Command[0]}))
	}

	b.cmd = cmd
	b.stdin = stdin
	b.stdout = bufio.NewReaderSize(stdout, 1024*1024)
	b.pending = make(map[string]chan json.RawMessage)
	b.running = true

	pid := cmd.Process.Pid
	slog.Info("backend started", "backend", b.name, "pid", pid)

	go b.readLoop()
	go b.scanStderr(stderr, pid)
	go b.waitForExit(cmd)
	return nil
}

// scanStderr forwards backend stderr to the gateway's stderr and watches for the
// credential-expiry sentinel, arming the TTL watcher when it appears. pid is the
// process this scanner belongs to, used to guard against a stale scanner arming
// timers for an already-respawned backend.
func (b *Backend) scanStderr(r io.Reader, pid int) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		// Always forward to our own stderr for debuggability.
		_, _ = os.Stderr.Write(append(append([]byte(nil), line...), '\n'))

		idx := bytes.Index(line, []byte(credentialExpirySentinel))
		if idx < 0 {
			continue
		}
		epoch, ok := parseCredentialExpiry(line)
		if !ok {
			slog.Warn("invalid credential_expiry sentinel", "backend", b.name, "line", string(line))
			continue
		}
		b.armCredentialTTL(time.Unix(epoch, 0), pid)
	}
}

// parseCredentialExpiry extracts the unix-epoch value from a credential-expiry
// sentinel line ("[[gateway:credential_expiry]] <epoch>"). Returns ok=false if
// the sentinel is absent or the value is missing/invalid/non-positive.
func parseCredentialExpiry(line []byte) (int64, bool) {
	idx := bytes.Index(line, []byte(credentialExpirySentinel))
	if idx < 0 {
		return 0, false
	}
	rest := strings.TrimSpace(string(line[idx+len(credentialExpirySentinel):]))
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return 0, false
	}
	epoch, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || epoch <= 0 {
		return 0, false
	}
	return epoch, true
}

// armCredentialTTL sets the credential expiry and arms the soft and hard timers.
// Margins default to 5min (soft) and 2min (hard) if unset. pid guards against a
// timer killing a newer, respawned backend.
func (b *Backend) armCredentialTTL(expiry time.Time, pid int) {
	soft := b.ttlSoftMargin
	if soft <= 0 {
		soft = 5 * time.Minute
	}
	hard := b.ttlHardGuard
	if hard <= 0 {
		hard = 2 * time.Minute
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// Ignore if this scanner belongs to a superseded process.
	if b.cmd == nil || b.cmd.Process == nil || b.cmd.Process.Pid != pid {
		return
	}

	b.credentialExpiry = expiry
	if b.softTimer != nil {
		b.softTimer.Stop()
	}
	if b.hardTimer != nil {
		b.hardTimer.Stop()
	}

	now := time.Now()
	softDelay := expiry.Add(-soft).Sub(now)
	hardDelay := expiry.Add(-hard).Sub(now)
	if softDelay < 0 {
		softDelay = 0
	}
	if hardDelay < 0 {
		hardDelay = 0
	}

	b.softTimer = time.AfterFunc(softDelay, func() { b.onSoftDeadline(pid) })
	b.hardTimer = time.AfterFunc(hardDelay, func() { b.onHardDeadline(pid) })
	slog.Info("credential TTL armed", "backend", b.name,
		"expiry", expiry.Format(time.RFC3339),
		"soft_in", softDelay.Round(time.Second), "hard_in", hardDelay.Round(time.Second))
}

// onSoftDeadline recycles the backend if idle, or defers the recycle until the
// in-flight requests drain (see dispatchResponse).
func (b *Backend) onSoftDeadline(pid int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.ttlOwnsProcess(pid) {
		return
	}
	if len(b.pending) == 0 {
		slog.Info("credential TTL soft deadline: recycling idle backend", "backend", b.name)
		b.recycleLocked()
		return
	}
	slog.Info("credential TTL soft deadline: backend busy, deferring recycle", "backend", b.name, "in_flight", len(b.pending))
	b.pendingTTLKill = true
}

// onHardDeadline force-recycles the backend even if requests are in flight.
func (b *Backend) onHardDeadline(pid int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.ttlOwnsProcess(pid) {
		return
	}
	slog.Info("credential TTL hard deadline: force-recycling backend", "backend", b.name, "in_flight", len(b.pending))
	b.recycleLocked()
}

// ttlOwnsProcess reports whether the current process matches the pid a timer was
// armed for. Must be called with b.mu held.
func (b *Backend) ttlOwnsProcess(pid int) bool {
	return b.running && b.cmd != nil && b.cmd.Process != nil && b.cmd.Process.Pid == pid
}

// recycleLocked cancels the subprocess context so it terminates. The next request
// re-spawns it with a fresh credential. Must be called with b.mu held.
func (b *Backend) recycleLocked() {
	if b.cancelFn != nil {
		b.cancelFn()
	}
	b.running = false
}

// waitForExit waits for the subprocess to exit and cleans up pending requests.
func (b *Backend) waitForExit(cmd *exec.Cmd) {
	waitErr := cmd.Wait()
	b.mu.Lock()
	b.running = false
	for id, ch := range b.pending {
		close(ch)
		delete(b.pending, id)
	}
	// Tear down the credential-TTL watcher. This is the single teardown path for
	// every exit reason (idle reap, crash, manual kill, TTL recycle), so no
	// orphan timer can outlive the process.
	if b.softTimer != nil {
		b.softTimer.Stop()
		b.softTimer = nil
	}
	if b.hardTimer != nil {
		b.hardTimer.Stop()
		b.hardTimer = nil
	}
	b.pendingTTLKill = false
	b.mu.Unlock()
	if waitErr != nil {
		slog.Warn("backend process exited with error", "backend", b.name, "error", waitErr)
	} else {
		slog.Info("backend process exited", "backend", b.name)
	}
}

// initializeBackend sends the MCP initialize handshake to the subprocess.
// Returns an error if the backend fails to initialize or responds with a JSON-RPC error.
func (b *Backend) initializeBackend() error {
	initMsg := []byte(`{"jsonrpc":"2.0","id":"_gw_init","method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"mcp-gateway","version":"1.0.0"}}}`)
	resp, err := b.send(context.Background(), initMsg)
	if err != nil {
		slog.Error("backend initialize failed", "backend", b.name, "error", err)
		return ErrBackendInitFailed.Parse(errors.WithError(err))
	}

	// Check if the response contains a JSON-RPC error
	var rpcResp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(resp, &rpcResp) == nil && rpcResp.Error != nil {
		initErr := fmt.Errorf("%s", rpcResp.Error.Message)
		slog.Error("backend initialize returned error", "backend", b.name, "code", rpcResp.Error.Code, "message", rpcResp.Error.Message)
		return ErrBackendInitFailed.Parse(errors.WithError(initErr))
	}

	slog.Info("backend initialized", "backend", b.name, "response", truncate(resp, 100))

	// Send notifications/initialized (fire-and-forget)
	b.mu.Lock()
	if b.running {
		if _, err := b.stdin.Write([]byte("{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n")); err != nil {
			slog.Warn("failed to send notifications/initialized", "backend", b.name, "error", err)
		}
	}
	b.mu.Unlock()

	// Refresh tools cache if this is a new backend process
	b.refreshToolsCache()

	return nil
}

// send writes a JSON-RPC message to stdin and waits for the correlated response.
// For remote backends, it forwards via HTTP.
func (b *Backend) send(ctx context.Context, msg []byte) ([]byte, error) {
	if b.isRemote() {
		return b.sendRemote(ctx, msg)
	}

	// Touch last used timestamp
	b.mu.Lock()
	b.lastUsed = time.Now()
	b.mu.Unlock()

	// Extract the ID for correlation
	var envelope jsonRPCMessage
	if err := json.Unmarshal(msg, &envelope); err != nil {
		return nil, ErrParseMsgID.Parse(errors.WithError(err))
	}

	// For notifications (no id), just write and return empty
	if envelope.ID == nil {
		b.mu.Lock()
		if !b.running {
			b.mu.Unlock()
			return nil, ErrBackendNotRunning.Parse()
		}
		_, err := b.stdin.Write(append(msg, '\n'))
		b.mu.Unlock()
		return []byte("{}"), err
	}

	// Create response channel
	idKey := string(*envelope.ID)
	ch := make(chan json.RawMessage, 1)

	b.mu.Lock()
	if !b.running {
		b.mu.Unlock()
		return nil, ErrBackendNotRunning.Parse()
	}
	b.pending[idKey] = ch
	_, err := b.stdin.Write(append(msg, '\n'))
	b.mu.Unlock()

	if err != nil {
		b.mu.Lock()
		delete(b.pending, idKey)
		b.mu.Unlock()
		return nil, ErrWritePipe.Parse(errors.WithError(err))
	}

	// Wait for response with timeout or context cancellation
	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, ErrBackendClosed.Parse()
		}
		return resp, nil
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.pending, idKey)
		b.mu.Unlock()
		return nil, ErrBackendTimeout.Parse(errors.WithError(ctx.Err()))
	case <-time.After(120 * time.Second):
		b.mu.Lock()
		delete(b.pending, idKey)
		b.mu.Unlock()
		return nil, ErrBackendTimeout.Parse()
	}
}

// readLoop reads line-delimited JSON from the subprocess stdout and dispatches responses.
func (b *Backend) readLoop() {
	for {
		line, err := b.stdout.ReadBytes('\n')
		if err != nil {
			if err != io.EOF {
				slog.Error("backend read error", "backend", b.name, "error", err)
			}
			return
		}

		line = bytesTrimRight(line)
		if len(line) == 0 {
			continue
		}

		b.dispatchResponse(line)
	}
}

// dispatchResponse parses a JSON-RPC response line and delivers it to the pending caller.
func (b *Backend) dispatchResponse(line []byte) {
	var envelope jsonRPCMessage
	if err := json.Unmarshal(line, &envelope); err != nil {
		slog.Warn("invalid JSON from backend", "backend", b.name, "error", err)
		return
	}

	if envelope.ID == nil {
		return // notification from backend, drop
	}

	idKey := string(*envelope.ID)

	b.mu.Lock()
	ch, ok := b.pending[idKey]
	if ok {
		delete(b.pending, idKey)
	}
	// If a credential-TTL recycle was deferred because the backend was busy,
	// and this was the last in-flight request, recycle now.
	if b.pendingTTLKill && len(b.pending) == 0 {
		slog.Info("credential TTL: in-flight requests drained, recycling backend", "backend", b.name)
		b.pendingTTLKill = false
		b.recycleLocked()
	}
	b.mu.Unlock()

	if ok {
		ch <- json.RawMessage(line)
	} else {
		slog.Warn("unmatched response from backend", "backend", b.name, "id", idKey)
	}
}

// kill terminates the backend subprocess or disconnects from remote.
func (b *Backend) kill() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.isRemote() {
		b.running = false
		return
	}

	if b.cancelFn != nil {
		b.cancelFn()
	}
	b.running = false
}

// isRunning returns whether the backend subprocess is alive.
func (b *Backend) isRunning() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running
}

// ensureRunning starts the subprocess if not already running, or connects to remote backend.
func (b *Backend) ensureRunning() error {
	b.mu.Lock()

	if b.running {
		b.mu.Unlock()
		return nil
	}

	// Remote backends - mark as running (stateless HTTP, always "ready")
	if b.isRemote() {
		b.running = true
		b.lastUsed = time.Now()
		b.mu.Unlock()
		b.logInfo("remote backend connected", "url", b.def.URL, "transport", b.transportType())
		return nil
	}

	if len(b.def.Command) == 0 {
		b.mu.Unlock()
		return ErrBackendNotConfigured.Parse(errors.WithParsedMessage(b.name))
	}

	if err := b.spawnProcess(); err != nil {
		b.mu.Unlock()
		return err
	}

	// Release lock before sending initialize (send() needs to acquire it)
	b.mu.Unlock()

	const maxInitRetries = 3
	var initErr error
	for attempt := 1; attempt <= maxInitRetries; attempt++ {
		initErr = b.initializeBackend()
		if initErr == nil {
			return nil
		}

		slog.Warn("backend init failed, will retry", "backend", b.name, "attempt", attempt, "max", maxInitRetries, "error", initErr)

		// Kill the broken process
		b.kill()

		if attempt == maxInitRetries {
			break
		}

		// Backoff: 1s, 2s
		time.Sleep(time.Duration(attempt) * time.Second)

		// Re-spawn for next attempt
		b.mu.Lock()
		if err := b.spawnProcess(); err != nil {
			b.mu.Unlock()
			return err
		}
		b.mu.Unlock()
	}

	return initErr
}

// isRemote returns true if this backend connects to a remote URL rather than spawning a subprocess.
func (b *Backend) isRemote() bool {
	return b.def.URL != ""
}

// logInfo logs at info level if logging is enabled for this backend.
func (b *Backend) logInfo(msg string, args ...any) {
	if b.logEnabled {
		slog.Info(msg, append([]any{"backend", b.name}, args...)...)
	}
}

// logDebug logs at debug level if logging is enabled for this backend.
func (b *Backend) logDebug(msg string, args ...any) {
	if b.logEnabled {
		slog.Debug(msg, append([]any{"backend", b.name}, args...)...)
	}
}
