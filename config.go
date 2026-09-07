package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	errors "github.com/jbcjorge/errors-library"
)

func loadConfig(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path) // #nosec G703 G304 -- path from CLI arg or resolved config, not user request input
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:19900"
	}

	// Default cache dir to "cache/" alongside the config file
	if cfg.CacheDir == "" {
		cfg.CacheDir = filepath.Join(filepath.Dir(path), "cache")
	} else if !filepath.IsAbs(cfg.CacheDir) {
		cfg.CacheDir = filepath.Join(filepath.Dir(path), cfg.CacheDir)
	}

	// Load backend topology from a separate file if specified. The file holds
	// the new schema: {servers, compositions, backends}.
	if cfg.BackendsFile != "" {
		backendsPath := cfg.BackendsFile
		if !filepath.IsAbs(backendsPath) {
			backendsPath = filepath.Join(filepath.Dir(path), backendsPath)
		}
		backendsData, err := os.ReadFile(backendsPath) // #nosec G703 G304 -- path from config file, admin-controlled
		if err != nil {
			return cfg, ErrBackendsLoad.Parse(errors.WithParsedMessage(backendsPath), errors.WithError(err))
		}
		var topo CompositionConfig
		if err := json.Unmarshal(backendsData, &topo); err != nil {
			return cfg, ErrBackendsParse.Parse(errors.WithParsedMessage(backendsPath), errors.WithError(err))
		}
		cfg.Servers = topo.Servers
		cfg.Compositions = topo.Compositions
		cfg.Routes = topo.Backends
	}

	if cfg.Servers == nil {
		cfg.Servers = make(map[string]BackendDef)
	}
	return cfg, nil
}

// loadConfig reads and parses the JSON configuration file at the given path.
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

// parseLogLevel converts a string log level name to a slog.Level.
// Defaults to slog.LevelInfo for empty or unrecognized values.
func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
