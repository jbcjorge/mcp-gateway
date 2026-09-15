//go:build windows

package main

import (
	"log/slog"
	"os/exec"
	"time"
)

// defaultStopGrace is the fallback delay between the graceful stop attempt and
// the forced kill when neither the per-server nor the global config sets one.
const defaultStopGrace = 20 * time.Second

// configureProcAttr is a no-op on Windows. Windows has no POSIX process groups;
// teardown relies on Go's default context-cancel process kill.
func configureProcAttr(_ *exec.Cmd) {}

// stopProcessGroup on Windows waits for the process to exit within the grace
// window, then force-kills the single process. Windows lacks POSIX process
// groups, so descendant reaping is best-effort via Process.Kill.
func stopProcessGroup(cmd *exec.Cmd, grace time.Duration, exited <-chan struct{}, name string) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if grace <= 0 {
		grace = defaultStopGrace
	}
	select {
	case <-exited:
		return
	case <-time.After(grace):
		slog.Warn("backend did not exit within grace, killing process", "backend", name, "grace", grace)
		if err := cmd.Process.Kill(); err != nil {
			slog.Warn("process kill failed", "backend", name, "err", err)
		}
	}
}
