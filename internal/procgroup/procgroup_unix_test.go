//go:build !windows

package procgroup

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestConfigure_SetsProcessGroup(t *testing.T) {
	cmd := exec.Command("sleep", "1") // #nosec G204 -- fixed test command
	Configure(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("expected Setpgid to be true after Configure")
	}
}

func TestConfigure_PreservesExistingSysProcAttr(t *testing.T) {
	cmd := exec.Command("sleep", "1") // #nosec G204 -- fixed test command
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: false}
	Configure(cmd)
	if !cmd.SysProcAttr.Setpgid || cmd.SysProcAttr.Pgid != 0 {
		t.Fatalf("Configure must set Setpgid with Pgid 0, got %+v", cmd.SysProcAttr)
	}
}

func TestSignalGroup_NoSuchProcessIsOK(t *testing.T) {
	// A pid that does not exist should not produce an error (ESRCH is success).
	if err := signalGroup(1<<30, syscall.SIGTERM); err != nil {
		t.Errorf("signalGroup on nonexistent group returned error: %v", err)
	}
}

func TestStop_NilCommandIsNoop(t *testing.T) {
	Stop(nil, time.Second, nil, "nil-cmd")
	Stop(&exec.Cmd{}, time.Second, nil, "unstarted-cmd")
}

// TestStop_GracefulExit verifies that a process honoring SIGTERM is stopped
// before the grace window elapses (no SIGKILL escalation needed).
func TestStop_GracefulExit(t *testing.T) {
	cmd := exec.Command("sleep", "30") // #nosec G204 -- fixed test command
	Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	start := time.Now()
	Stop(cmd, 10*time.Second, exited, "sleeper")
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Errorf("graceful stop took %v; expected well under the 10s grace", elapsed)
	}
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("process did not exit after Stop")
	}
}
