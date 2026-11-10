package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateTempDir points os.TempDir() at a per-test directory so tests that touch
// the pidfile never read, overwrite, or delete a real running gateway's pidfile.
func isolateTempDir(t *testing.T) {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
}

func TestResolveConfigPath_Default(t *testing.T) {
	// Save and restore os.Args
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	os.Args = []string{"mcp-gateway"}
	path := resolveConfigPath()
	// Should return either "config.json" or a path ending with config.json
	if !strings.HasSuffix(path, "config.json") {
		t.Errorf("expected path ending in config.json, got %q", path)
	}
}

func TestResolveConfigPath_WithArg(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "my-config.json")
	os.WriteFile(cfgPath, []byte(`{}`), 0644)

	os.Args = []string{"mcp-gateway", cfgPath}
	path := resolveConfigPath()
	if path != cfgPath {
		t.Errorf("expected %q, got %q", cfgPath, path)
	}
}

func TestPidFilePath(t *testing.T) {
	isolateTempDir(t)
	path := pidFilePath()
	if !strings.Contains(path, "mcp-gateway.pid") {
		t.Errorf("expected path containing mcp-gateway.pid, got %q", path)
	}
}

func TestWriteAndRemovePidFile(t *testing.T) {
	isolateTempDir(t)
	path := writePidFile()
	defer os.Remove(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	if !strings.Contains(string(data), fmt.Sprintf("%d", os.Getpid())) {
		t.Errorf("pidfile should contain current pid, got %q", string(data))
	}

	removePidFile(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("pidfile should be removed")
	}
}

func TestRemovePidFile_Nonexistent(t *testing.T) {
	// Should not panic
	removePidFile("/nonexistent/path/mcp-gateway.pid")
}

func TestCheckExistingInstance_NoPidFile(t *testing.T) {
	isolateTempDir(t)
	// Remove pidfile if it exists
	path := pidFilePath()
	os.Remove(path)

	result := checkExistingInstance("127.0.0.1:0")
	if result {
		t.Error("should return false when no pidfile exists")
	}
}

func TestCheckExistingInstance_InvalidPidfile(t *testing.T) {
	isolateTempDir(t)
	pidFile := pidFilePath()
	// Write invalid content
	os.WriteFile(pidFile, []byte("not a number"), 0600)
	defer os.Remove(pidFile)

	result := checkExistingInstance("127.0.0.1:0")
	if result {
		t.Error("should return false for invalid pidfile content")
	}
}

func TestCheckExistingInstance_DeadProcess(t *testing.T) {
	isolateTempDir(t)
	pidFile := pidFilePath()
	// Write a PID that is almost certainly dead (very high number)
	os.WriteFile(pidFile, []byte("9999999"), 0600)
	defer os.Remove(pidFile)

	result := checkExistingInstance("127.0.0.1:0")
	if result {
		t.Error("should return false for dead process")
	}
}
