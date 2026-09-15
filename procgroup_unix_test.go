//go:build !windows

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// runStubbornHelper ignores SIGTERM and spawns a grandchild ("sleep") that
// inherits the process group. It records the grandchild pid to a file so a test
// can later assert the grandchild was killed when the group was SIGKILLed. The
// helper then blocks reading stdin (a real fd, so the Go runtime does not flag a
// deadlock) until the process is SIGKILLed.
func runStubbornHelper() {
	signal.Ignore(syscall.SIGTERM)

	child := exec.Command("sleep", "300") // #nosec G204 -- fixed test command
	if err := child.Start(); err == nil {
		if pidFile := os.Getenv("TEST_GRANDCHILD_PIDFILE"); pidFile != "" {
			_ = os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0o600)
		}
	}
	// Block on stdin. The gateway holds the write end open, so this read blocks
	// indefinitely (no EOF, no deadlock-detector trip) until we are SIGKILLed.
	buf := make([]byte, 1)
	for {
		if _, err := os.Stdin.Read(buf); err != nil {
			return
		}
	}
}

// pidAlive reports whether a process with the given pid exists. On unix,
// signal 0 checks existence without delivering a signal.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// waitUntil polls cond every 20ms until it returns true or the timeout elapses.
func waitUntil(timeout time.Duration, cond func() bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForPidFile polls pidFile until it contains a positive integer pid or the
// timeout elapses. Returns 0 if no valid pid was recorded in time.
func waitForPidFile(pidFile string, timeout time.Duration) int {
	var pid int
	waitUntil(timeout, func() bool {
		data, err := os.ReadFile(pidFile) // #nosec G304 -- test temp file
		if err != nil {
			return false
		}
		p, err := strconv.Atoi(string(data))
		if err != nil || p <= 0 {
			return false
		}
		pid = p
		return true
	})
	return pid
}

func TestResolveStopGrace(t *testing.T) {
	five := 5
	zero := 0
	tests := []struct {
		name      string
		perServer *int
		global    int
		want      time.Duration
	}{
		{"per-server wins", &five, 30, 5 * time.Second},
		{"global when no per-server", nil, 30, 30 * time.Second},
		{"default when neither set", nil, 0, defaultStopGrace},
		{"per-server zero falls through to global", &zero, 30, 30 * time.Second},
		{"per-server zero and no global falls to default", &zero, 0, defaultStopGrace},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveStopGrace(tt.perServer, tt.global)
			if got != tt.want {
				t.Errorf("resolveStopGrace(%v, %d) = %v, want %v", tt.perServer, tt.global, got, tt.want)
			}
		})
	}
}

func TestConfigureProcAttr_SetsProcessGroup(t *testing.T) {
	cmd := exec.Command("sleep", "1") // #nosec G204 -- fixed test command
	configureProcAttr(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("expected Setpgid to be true after configureProcAttr")
	}
}

func TestSignalGroup_NoSuchProcessIsOK(t *testing.T) {
	// A pid that does not exist should not produce an error (ESRCH is success).
	if err := signalGroup(1<<30, syscall.SIGTERM); err != nil {
		t.Errorf("signalGroup on nonexistent group returned error: %v", err)
	}
}

// TestGracefulStop_ExitsOnSigterm verifies the graceful path: a normal backend
// exits on SIGTERM well before the grace window, and no SIGKILL escalation is
// needed. Uses the standard (non-stubborn) helper, which exits when its stdin
// closes / it is signalled.
func TestGracefulStop_ExitsOnSigterm(t *testing.T) {
	b := &Backend{
		name:        "graceful",
		def:         BackendDef{Command: subprocessCommand(), Env: map[string]string{"TEST_SUBPROCESS": "1"}},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
		stopGrace:   10 * time.Second, // long grace; graceful exit should be far faster
	}

	b.mu.Lock()
	if err := b.spawnProcess(); err != nil {
		b.mu.Unlock()
		t.Fatalf("spawnProcess: %v", err)
	}
	pid := b.cmd.Process.Pid
	b.mu.Unlock()

	start := time.Now()
	b.kill() // routes through gracefulStopLocked -> stopProcessGroup

	// Wait for the process to actually be gone.
	waitUntil(5*time.Second, func() bool { return !b.isRunning() && !pidAlive(pid) })

	if b.isRunning() || pidAlive(pid) {
		t.Fatalf("process %d still alive after graceful stop", pid)
	}
	if elapsed := time.Since(start); elapsed >= 10*time.Second {
		t.Errorf("graceful stop took %v, should be well under the 10s grace (no SIGKILL wait)", elapsed)
	}
}

// TestGracefulStop_EscalatesToSigkill verifies the escalation path AND the
// no-orphan guarantee: a wedged backend that ignores SIGTERM is force-killed
// via group SIGKILL after the grace window, and its grandchild (which would
// otherwise be orphaned) is reaped along with the group.
func TestGracefulStop_EscalatesToSigkill(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")

	b := &Backend{
		name: "stubborn",
		def: BackendDef{
			Command: subprocessCommand(),
			Env: map[string]string{
				"TEST_SUBPROCESS":         "1",
				"TEST_STUBBORN":           "1",
				"TEST_GRANDCHILD_PIDFILE": pidFile,
			},
		},
		pending:     make(map[string]chan json.RawMessage),
		activeTools: make(map[string]bool),
		logEnabled:  true,
		stopGrace:   1 * time.Second, // short grace so the test is quick
	}

	b.mu.Lock()
	if err := b.spawnProcess(); err != nil {
		b.mu.Unlock()
		t.Fatalf("spawnProcess: %v", err)
	}
	pid := b.cmd.Process.Pid
	b.mu.Unlock()

	// Wait for the grandchild pid to be recorded.
	grandchildPid := waitForPidFile(pidFile, 3*time.Second)
	if grandchildPid == 0 {
		t.Fatal("grandchild pid was never recorded; helper did not start correctly")
	}
	if !pidAlive(grandchildPid) {
		t.Fatalf("grandchild %d should be alive before stop", grandchildPid)
	}

	start := time.Now()
	b.kill() // SIGTERM ignored by helper -> must escalate to group SIGKILL after grace

	// Both the backend and its grandchild must be gone after grace + margin.
	waitUntil(5*time.Second, func() bool { return !pidAlive(pid) && !pidAlive(grandchildPid) })

	if pidAlive(pid) {
		t.Errorf("stubborn backend %d still alive after escalation", pid)
	}
	if pidAlive(grandchildPid) {
		t.Errorf("grandchild %d orphaned: not reaped with the process group", grandchildPid)
	}
	if elapsed := time.Since(start); elapsed < 1*time.Second {
		t.Errorf("escalation happened in %v, expected to wait the ~1s grace before SIGKILL", elapsed)
	}
}
