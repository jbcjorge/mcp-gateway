//go:build !windows

package main

import (
	"log/slog"
	"os/exec"
	"syscall"
	"time"
)

// defaultStopGrace is the fallback delay between SIGTERM and SIGKILL when a
// backend is stopped and neither the per-server nor the global config sets one.
const defaultStopGrace = 20 * time.Second

// configureProcAttr places the child in its own process group so that a single
// signal to the negated group id reaches the child AND every process it spawns
// (e.g. a browser and its renderer children). This is what makes teardown
// atomic: the gateway never has to walk a process tree.
func configureProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pgid = 0 // 0 = new group led by the child itself
}

// signalGroup delivers sig to the entire process group led by pid. Because the
// child is a group leader (see configureProcAttr), signalling -pid reaches the
// child and all its descendants in one call. syscall.ESRCH ("no such process")
// means the group already exited, which we treat as success.
func signalGroup(pid int, sig syscall.Signal) error {
	if err := syscall.Kill(-pid, sig); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

// stopProcessGroup gracefully stops the process group led by cmd: it sends
// SIGTERM to the group, waits up to grace for the process to exit, then sends
// SIGKILL to the group as a guaranteed fallback. This ensures no descendant
// (e.g. a browser owned by a backend) is ever orphaned, even if the backend
// itself is wedged and cannot run its own teardown.
//
// The exited channel must be closed by the caller (typically waitForExit) when
// cmd.Wait() returns, so this function can detect a clean, graceful exit and
// skip the SIGKILL escalation.
func stopProcessGroup(cmd *exec.Cmd, grace time.Duration, exited <-chan struct{}, name string) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if grace <= 0 {
		grace = defaultStopGrace
	}

	// Phase 1: polite SIGTERM to the whole group.
	if err := signalGroup(pid, syscall.SIGTERM); err != nil {
		slog.Warn("group SIGTERM failed", "backend", name, "pid", pid, "err", err)
	}

	// Phase 2: wait for a graceful exit, or escalate after the grace window.
	select {
	case <-exited:
		slog.Info("backend exited gracefully after SIGTERM", "backend", name, "pid", pid)
		return
	case <-time.After(grace):
		slog.Warn("backend did not exit within grace, sending group SIGKILL",
			"backend", name, "pid", pid, "grace", grace)
		if err := signalGroup(pid, syscall.SIGKILL); err != nil {
			slog.Warn("group SIGKILL failed", "backend", name, "pid", pid, "err", err)
		}
	}
}
