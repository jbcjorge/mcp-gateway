package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	errors "github.com/jbcjorge/errors-library"
)

// CABundleConfig configures optional CA-bundle management. It is fully
// company- and OS-agnostic: the gateway only runs the configured command and
// injects the resulting bundle path into every backend's environment. All
// knowledge of how to produce or validate the bundle lives in the external
// command.
//
// The command is invoked with the subcommand as its final argument and the
// target path provided via the CA_BUNDLE_PATH environment variable:
//
//	<generate_command...> check    -> prints "CURRENT" (bundle is up to date)
//	                                  or anything else (regenerate).
//	<generate_command...> bundle   -> (re)creates the bundle at CA_BUNDLE_PATH.
type CABundleConfig struct {
	Path            string   `json:"path"`             // where the bundle lives / is written
	GenerateCommand []string `json:"generate_command"` // command to check/generate the bundle
	RequireBundle   bool     `json:"require_bundle"`   // if true, a missing/unreadable bundle is fatal
}

// caEnvVars are the environment variables that point TLS clients at a CA bundle.
// SSL_CERT_FILE covers OpenSSL (Python urllib, curl), REQUESTS_CA_BUNDLE covers
// Python requests/certifi, NODE_EXTRA_CA_CERTS covers Node. All three are
// industry-standard and company-agnostic.
var caEnvVars = []string{"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS"}

// caCheckStatusCurrent is the only status that skips regeneration. Any other
// output (including errors, timeouts, or unknown tokens) is treated as stale.
const caCheckStatusCurrent = "CURRENT"

// caCheckTimeout bounds the fast check phase so a misbehaving script cannot
// stall startup.
const caCheckTimeout = 10 * time.Second

// caBundleTimeout bounds the (rarer) generation phase.
const caBundleTimeout = 60 * time.Second

// injectCAEnv sets the CA-bundle environment variables on env unless the caller
// has already provided them (explicit per-config/per-backend overrides win).
// No-op if bundlePath is empty.
func injectCAEnv(env map[string]string, bundlePath string) {
	if bundlePath == "" {
		return
	}
	for _, k := range caEnvVars {
		if _, ok := env[k]; !ok {
			env[k] = bundlePath
		}
	}
}

// bundleReadable reports whether the file at path exists, is regular, and is
// non-empty.
func bundleReadable(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Size() > 0
}

// runCACommand runs the configured command with the given subcommand and
// CA_BUNDLE_PATH set, returning trimmed stdout.
func runCACommand(cfg *CABundleConfig, subcommand string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	args := append([]string{}, cfg.GenerateCommand[1:]...)
	args = append(args, subcommand)
	cmd := exec.CommandContext(ctx, cfg.GenerateCommand[0], args...) // #nosec G204 -- command from admin config, this tool's purpose
	cmd.Env = append(os.Environ(), "CA_BUNDLE_PATH="+cfg.Path)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// resolveCABundleEnv runs the check/bundle flow and returns the CA environment
// variables to inject into backends. It is the testable core (no os.Exit).
//
//   - No config or no command -> returns nil, nil (feature disabled).
//   - check == CURRENT and bundle readable -> inject.
//   - check != CURRENT (or check fails) -> run bundle, then verify readable.
//   - bundle missing/unreadable at the end -> if RequireBundle, returns a fatal
//     error; otherwise logs and returns nil, nil (continue without injection).
func resolveCABundleEnv(cfg *CABundleConfig) (map[string]string, error) {
	if cfg == nil || cfg.Path == "" || len(cfg.GenerateCommand) == 0 {
		return nil, nil
	}

	status, err := runCACommand(cfg, "check", caCheckTimeout)
	if err != nil {
		slog.Warn("CA bundle check failed, treating as stale", "error", err)
		status = ""
	}

	if status == caCheckStatusCurrent {
		slog.Debug("CA bundle check: current", "path", cfg.Path)
	} else {
		slog.Info("CA bundle check: stale, regenerating", "status", status, "path", cfg.Path)
		if _, gerr := runCACommand(cfg, "bundle", caBundleTimeout); gerr != nil {
			slog.Warn("CA bundle generation command failed", "error", gerr)
		}
	}

	if !bundleReadable(cfg.Path) {
		if cfg.RequireBundle {
			return nil, ErrCABundleMissing.Parse(errors.WithSafeData(map[string]any{"path": cfg.Path}))
		}
		slog.Error("CA bundle missing or unreadable; continuing without CA injection", "path", cfg.Path)
		return nil, nil
	}

	env := map[string]string{}
	injectCAEnv(env, cfg.Path)
	slog.Info("CA bundle ready, injecting into backends", "path", cfg.Path)
	return env, nil
}

// mergeCAEnv merges the CA env vars into the config's global Env (existing keys
// win, matching injectCAEnv's override semantics at the config level).
func mergeCAEnv(cfgEnv map[string]string, caEnv map[string]string) map[string]string {
	if len(caEnv) == 0 {
		return cfgEnv
	}
	if cfgEnv == nil {
		cfgEnv = map[string]string{}
	}
	for k, v := range caEnv {
		if _, ok := cfgEnv[k]; !ok {
			cfgEnv[k] = v
		}
	}
	return cfgEnv
}
