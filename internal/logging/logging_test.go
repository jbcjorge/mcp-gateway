package logging

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestNewRotatingWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	rw, err := newRotatingWriter(path, 1, 2)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	if rw == nil {
		t.Fatal("expected non-nil writer")
	}

	// Write some data
	n, err := rw.Write([]byte("hello world\n"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != 12 {
		t.Errorf("expected 12 bytes written, got %d", n)
	}

	// Verify file exists
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("log file should exist: %v", err)
	}
}

func TestRotatingWriter_Rotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rotate.log")

	// Use tiny max size to force rotation
	rw, err := newRotatingWriter(path, 0, 2) // 0 defaults to 10MB, use direct
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}

	// Override maxSize directly for test
	rw.maxSize = 50 // 50 bytes

	// Write enough to trigger rotation
	for i := 0; i < 10; i++ {
		rw.Write([]byte("this is a test log line that is fairly long\n"))
	}

	// Check that rotated file exists
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("rotated file should exist: %v", err)
	}
}

func TestNewRotatingWriter_Defaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "defaults.log")

	rw, err := newRotatingWriter(path, 0, 0) // both 0 -> use defaults
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	if rw.maxSize != 10*1024*1024 {
		t.Errorf("expected default max size 10MB, got %d", rw.maxSize)
	}
	if rw.maxFiles != 3 {
		t.Errorf("expected default max files 3, got %d", rw.maxFiles)
	}
}

func TestNewRotatingWriter_CreatesDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "dir", "test.log")

	rw, err := newRotatingWriter(path, 1, 1)
	if err != nil {
		t.Fatalf("newRotatingWriter with nested dir: %v", err)
	}
	rw.Write([]byte("test\n"))

	if _, err := os.Stat(path); err != nil {
		t.Errorf("log file should exist after write: %v", err)
	}
}

func TestSetup_Default(t *testing.T) {
	if err := Setup(Options{Level: "debug", BaseDir: "/tmp"}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if got := level.Level(); got != slog.LevelDebug {
		t.Errorf("level = %v, want DEBUG", got)
	}
}

func TestSetup_WithFile(t *testing.T) {
	dir := t.TempDir()
	err := Setup(Options{
		Level:     "info",
		File:      "gateway.log",
		MaxSizeMB: 1,
		MaxFiles:  2,
		BaseDir:   dir,
	})
	if err != nil {
		t.Fatalf("Setup with file: %v", err)
	}

	// Log file should be created, relative to BaseDir
	if _, err := os.Stat(filepath.Join(dir, "gateway.log")); err != nil {
		t.Errorf("log file should exist: %v", err)
	}
}

func TestSetup_AbsoluteFileIgnoresBaseDir(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "abs.log")
	if err := Setup(Options{Level: "info", File: logPath, BaseDir: "/nonexistent"}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("absolute log path should be used as-is: %v", err)
	}
}

func TestSetup_FileOpenError(t *testing.T) {
	// A regular file used as a directory component makes MkdirAll fail.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Setup(Options{File: filepath.Join(blocker, "x.log")}); err == nil {
		t.Fatal("expected error when the log file cannot be opened")
	}
}

func TestSetup_EnvOverride(t *testing.T) {
	t.Setenv(debugEnvVar, "1")
	if err := Setup(Options{Level: "error"}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if got := level.Level(); got != slog.LevelDebug {
		t.Errorf("level = %v, want DEBUG from %s override", got, debugEnvVar)
	}
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"debug", "DEBUG"},
		{"DEBUG", "DEBUG"},
		{"warn", "WARN"},
		{"warning", "WARN"},
		{"error", "ERROR"},
		{"info", "INFO"},
		{"", "INFO"},
		{"unknown", "INFO"},
	}

	for _, tt := range tests {
		got := ParseLevel(tt.input)
		if got.String() != tt.expected {
			t.Errorf("ParseLevel(%q) = %v, want %s", tt.input, got, tt.expected)
		}
	}
}
