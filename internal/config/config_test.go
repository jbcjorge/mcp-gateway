package config

import (
	"fmt"
	"os"
	"path/filepath"
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

	cfg, err := Load(dir + "/config.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
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

	cfg, err := Load(dir + "/config.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
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

	cfg, err := Load(dir + "/config.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := cfg.Servers["test"]; !ok {
		t.Error("expected inline server 'test'")
	}
	if len(cfg.Routes) != 1 || cfg.Routes[0].Route != "test" {
		t.Errorf("expected route 'test', got %+v", cfg.Routes)
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	_, err := Load("/nonexistent/path.json")
	if err == nil {
		t.Error("expected error for missing config file")
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	// Test that empty listen gets default
	tmpFile := t.TempDir() + "/cfg.json"
	_ = writeTestFile(tmpFile, `{"backends":[]}`)
	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:19900" {
		t.Errorf("expected default 127.0.0.1:19900, got %s", cfg.Listen)
	}
}

func TestLoadConfig_CacheDir(t *testing.T) {
	dir := t.TempDir()
	cfgContent := `{"backends":[],"cache_dir":"my-cache"}`
	cfgPath := filepath.Join(dir, "config.json")
	os.WriteFile(cfgPath, []byte(cfgContent), 0644)

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
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

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.CacheDir != absCache {
		t.Errorf("expected cache_dir %q, got %q", absCache, cfg.CacheDir)
	}
}
