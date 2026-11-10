package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// resolveConfigPath determines the config file path from CLI args.
// Falls back to "config.json" relative to the executable if not found in cwd.
func resolveConfigPath() string {
	configPath := "config.json"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}
	if !filepath.IsAbs(configPath) {
		if _, err := os.Stat(configPath); os.IsNotExist(err) { // #nosec G703 -- path from CLI arg, not user request input
			exeDir, _ := os.Executable()
			configPath = filepath.Join(filepath.Dir(exeDir), configPath)
		}
	}
	return configPath
}

// pidFilePath returns the path to the pidfile.
func pidFilePath() string {
	return filepath.Join(os.TempDir(), "mcp-gateway.pid")
}

// removePidFile removes the pidfile, logging any failure.
func removePidFile(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		slog.Debug("failed to remove pidfile", "path", path, "error", err)
	}
}

// writePidFile creates the pidfile with the current process ID and returns its path.
func writePidFile() string {
	pidFile := pidFilePath()
	if err := os.MkdirAll(filepath.Dir(pidFile), 0700); err != nil {
		slog.Warn("failed to create pidfile directory", "error", err)
	}
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", os.Getpid())), 0600); err != nil {
		slog.Warn("failed to write pidfile", "error", err)
	}
	return pidFile
}

// checkExistingInstance checks if an instance is already running.
// Returns true if a healthy instance is reachable on the given address.
func checkExistingInstance(addr string) bool {
	// Check pidfile first
	pidFile := pidFilePath()
	pidData, err := os.ReadFile(pidFile) // #nosec G304 -- path is from pidFilePath() constant, not user input
	if err != nil {
		return false // no pidfile, not running
	}

	// Verify the process is still alive
	var pid int
	if _, scanErr := fmt.Sscanf(string(pidData), "%d", &pid); scanErr != nil {
		removePidFile(pidFile)
		return false
	}

	proc, findErr := os.FindProcess(pid)
	if findErr != nil {
		removePidFile(pidFile)
		return false
	}

	// On Unix, FindProcess always succeeds. Send signal 0 to check if alive.
	if sigErr := proc.Signal(syscall.Signal(0)); sigErr != nil {
		removePidFile(pidFile)
		return false
	}

	// Process exists. Verify it's actually responding on the port.
	host := addr
	if strings.HasPrefix(host, ":") {
		host = "localhost" + host
	}
	healthURL := fmt.Sprintf("http://%s/health", host)

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(healthURL)
	if err != nil {
		// Process alive but not responding - stale. Kill it.
		if killErr := proc.Signal(syscall.SIGTERM); killErr != nil {
			slog.Warn("failed to kill stale process", "pid", pid, "error", killErr)
		}
		time.Sleep(500 * time.Millisecond)
		removePidFile(pidFile)
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == 200
}
