package main

import (
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

func TestInitLogging_Default(t *testing.T) {
	cfg := Config{LogLevel: "debug"}
	err := initLogging(cfg, "/tmp/test-config.json")
	if err != nil {
		t.Fatalf("initLogging: %v", err)
	}
}

func TestInitLogging_WithFile(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		LogLevel:     "info",
		LogFile:      "gateway.log",
		LogMaxSizeMB: 1,
		LogMaxFiles:  2,
	}
	configPath := filepath.Join(dir, "config.json")
	err := initLogging(cfg, configPath)
	if err != nil {
		t.Fatalf("initLogging with file: %v", err)
	}

	// Log file should be created
	logPath := filepath.Join(dir, "gateway.log")
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("log file should exist: %v", err)
	}
}

func TestInitLogging_EnvOverride(t *testing.T) {
	os.Setenv("MCP_GATEWAY_DEBUG", "1")
	defer os.Unsetenv("MCP_GATEWAY_DEBUG")

	cfg := Config{LogLevel: "error"}
	err := initLogging(cfg, "/tmp/test.json")
	if err != nil {
		t.Fatalf("initLogging: %v", err)
	}
	// logLevel should be debug due to env override
	// (we can't easily check the global, but at least it doesn't error)
}

func TestBytesTrimRight(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"hello\n", "hello"},
		{"hello\r\n", "hello"},
		{"hello   \n", "hello"},
		{"hello", "hello"},
		{"", ""},
		{"\n", ""},
	}
	for _, tt := range tests {
		got := string(bytesTrimRight([]byte(tt.input)))
		if got != tt.expected {
			t.Errorf("bytesTrimRight(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		input    string
		maxLen   int
		expected string
	}{
		{"hello world", 5, "hello..."},
		{"hi", 10, "hi"},
		{"exactly10!", 10, "exactly10!"},
		{"", 5, ""},
	}
	for _, tt := range tests {
		got := truncate([]byte(tt.input), tt.maxLen)
		if got != tt.expected {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.expected)
		}
	}
}
