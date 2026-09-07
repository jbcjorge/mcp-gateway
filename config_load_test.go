package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	_ = writeTestFile(dir+"/config.json", `{
		"listen": "127.0.0.1:19900",
		"log_level": "info",
		"idle_timeout_seconds": 300,
		"self_idle_timeout_seconds": 3600,
		"backends_file": "backends.json"
	}`)
	_ = writeTestFile(dir+"/backends.json", `{}`)

	cfg, err := loadConfig(dir + "/config.json")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Listen != "127.0.0.1:19900" {
		t.Errorf("expected 127.0.0.1:19900, got %s", cfg.Listen)
	}
	if cfg.IdleTimeout != 300 {
		t.Errorf("expected idle_timeout 300, got %d", cfg.IdleTimeout)
	}
	if cfg.SelfIdleTimeout != 3600 {
		t.Errorf("expected self_idle_timeout 3600, got %d", cfg.SelfIdleTimeout)
	}
	if cfg.BackendsFile != "backends.json" {
		t.Errorf("expected backends_file backends.json, got %s", cfg.BackendsFile)
	}
}

func TestLoadConfigWithBackendsFile(t *testing.T) {
	dir := t.TempDir()
	_ = writeTestFile(dir+"/config.json", `{"listen":":9999","backends_file":"backends.json"}`)
	_ = writeTestFile(dir+"/backends.json", `{"servers":{"mybackend":{"command":["echo","hi"]}},"backends":["mybackend"]}`)

	cfg, err := loadConfig(dir + "/config.json")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Listen != ":9999" {
		t.Errorf("expected :9999, got %s", cfg.Listen)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(cfg.Servers))
	}
	b, ok := cfg.Servers["mybackend"]
	if !ok {
		t.Fatal("expected mybackend in servers")
	}
	if len(b.Command) != 2 || b.Command[0] != "echo" {
		t.Errorf("unexpected command: %v", b.Command)
	}
	if len(cfg.Routes) != 1 || cfg.Routes[0].Route != "mybackend" {
		t.Errorf("expected route mybackend, got %+v", cfg.Routes)
	}
}

func TestLoadConfigInlineBackends(t *testing.T) {
	dir := t.TempDir()
	_ = writeTestFile(dir+"/config.json", `{"servers":{"test":{"command":["cat"]}},"backends":["test"]}`)

	cfg, err := loadConfig(dir + "/config.json")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if _, ok := cfg.Servers["test"]; !ok {
		t.Error("expected inline server 'test'")
	}
	if len(cfg.Routes) != 1 || cfg.Routes[0].Route != "test" {
		t.Errorf("expected route 'test', got %+v", cfg.Routes)
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	_, err := loadConfig("/nonexistent/path.json")
	if err == nil {
		t.Error("expected error for missing config file")
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	// Test that empty listen gets default
	tmpFile := t.TempDir() + "/cfg.json"
	_ = writeTestFile(tmpFile, `{"backends":[]}`)
	cfg, err := loadConfig(tmpFile)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Listen != "127.0.0.1:19900" {
		t.Errorf("expected default 127.0.0.1:19900, got %s", cfg.Listen)
	}
}

func TestParseLogLevel(t *testing.T) {
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
		got := parseLogLevel(tt.input)
		if got.String() != tt.expected {
			t.Errorf("parseLogLevel(%q) = %v, want %s", tt.input, got, tt.expected)
		}
	}
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

func TestLoadConfig_CacheDir(t *testing.T) {
	dir := t.TempDir()
	cfgContent := `{"backends":[],"cache_dir":"my-cache"}`
	cfgPath := filepath.Join(dir, "config.json")
	os.WriteFile(cfgPath, []byte(cfgContent), 0644)

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	expected := filepath.Join(dir, "my-cache")
	if cfg.CacheDir != expected {
		t.Errorf("expected cache_dir %q, got %q", expected, cfg.CacheDir)
	}
}

func TestLoadConfig_AbsCacheDir(t *testing.T) {
	dir := t.TempDir()
	absCache := filepath.Join(dir, "abs-cache")
	cfgContent := fmt.Sprintf(`{"backends":[],"cache_dir":"%s"}`, absCache)
	cfgPath := filepath.Join(dir, "config.json")
	os.WriteFile(cfgPath, []byte(cfgContent), 0644)

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	if cfg.CacheDir != absCache {
		t.Errorf("expected cache_dir %q, got %q", absCache, cfg.CacheDir)
	}
}
