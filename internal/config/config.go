// Package config defines the mcp-gateway configuration model (the top-level
// Config, the backend/server definitions, and compositions) and loads it from
// disk. It has no dependency on the gateway runtime, so it can be shared by the
// entry point, the gateway, and the hot-reload watcher.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"

	errors "github.com/jbcjorge/errors-library"

	"github.com/jbcjorge/mcp-gateway/internal/cabundle"
)

// Config is the top-level configuration.
type Config struct {
	Listen                  string            `json:"listen"`
	LogLevel                string            `json:"log_level"`                          // debug, info, warn, error (default: info)
	LogFile                 string            `json:"log_file"`                           // path to log file (default: stderr)
	LogMaxSizeMB            int               `json:"log_max_size_mb"`                    // max log file size in MB before rotation (default: 10)
	LogMaxFiles             int               `json:"log_max_files"`                      // number of rotated files to keep (default: 3)
	CacheDir                string            `json:"cache_dir"`                          // directory for persistent tools cache (default: alongside config)
	MaxDescLen              int               `json:"max_description_length"`             // truncate tool descriptions to this length (0 = no truncation)
	Discovery               *bool             `json:"discovery"`                          // global discovery toggle: nil=auto, true=force, false=disable for all backends
	Env                     map[string]string `json:"env"`                                // global environment variables for all backends
	CABundle                *cabundle.Config  `json:"ca_bundle"`                          // optional CA bundle management (see cabundle.Config)
	AuthTokens              []string          `json:"auth_tokens"`                        // global bearer tokens (used when backend has no own tokens)
	IdleTimeout             int               `json:"idle_timeout_seconds"`               // kill backends idle for this long (0 = never)
	SelfIdleTimeout         int               `json:"self_idle_timeout_seconds"`          // kill gateway itself after this long with no requests (0 = never)
	StopGraceSeconds        int               `json:"stop_grace_seconds"`                 // global default grace between SIGTERM and SIGKILL on backend stop (0 = default 20s)
	CredentialTTLSoftMargin int               `json:"credential_ttl_soft_margin_seconds"` // recycle (defer-if-busy) this long before credential expiry (0 = default 300)
	CredentialTTLHardGuard  int               `json:"credential_ttl_hard_guard_seconds"`  // force-recycle this long before credential expiry (0 = default 120)
	BackendsFile            string            `json:"backends_file"`                      // path to backends.json (relative to config dir)
	// Backend topology (new schema, pre-1.0): servers/compositions/backends.
	// Loaded inline here or from BackendsFile.
	Servers      map[string]BackendDef     `json:"servers"`
	Compositions map[string]CompositionDef `json:"compositions"`
	Routes       []RouteEntry              `json:"backends"`
}

// Load reads and parses the JSON configuration file at path, applies defaults,
// and merges in the backend topology from backends_file when one is configured.
func Load(path string) (Config, error) {
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
