package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFakeGenerator creates an executable shell script acting as a dummy
// generate-ca command. `checkStatus` is what `check` prints; if `writeBundle`
// is true, the `bundle` subcommand writes some bytes to $CA_BUNDLE_PATH.
func writeFakeGenerator(t *testing.T, dir, checkStatus string, writeBundle bool) []string {
	t.Helper()
	script := filepath.Join(dir, "gen.sh")
	body := "#!/usr/bin/env bash\nset -e\ncase \"$1\" in\n" +
		"  check) echo \"" + checkStatus + "\" ;;\n" +
		"  bundle)"
	if writeBundle {
		body += " printf 'FAKE-CA-BUNDLE\\n' > \"$CA_BUNDLE_PATH\" ;;\n"
	} else {
		body += " : ;;\n" // no-op: does not create the bundle
	}
	body += "  *) echo UNKNOWN ;;\nesac\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return []string{"bash", script}
}

func TestResolveCABundle_DisabledWhenNoConfig(t *testing.T) {
	env, err := resolveCABundleEnv(nil)
	if err != nil || env != nil {
		t.Errorf("nil config should be no-op, got env=%v err=%v", env, err)
	}
	// Config present but no command -> also no-op.
	env, err = resolveCABundleEnv(&CABundleConfig{Path: "/tmp/x.pem"})
	if err != nil || env != nil {
		t.Errorf("no command should be no-op, got env=%v err=%v", env, err)
	}
}

func TestResolveCABundle_CurrentSkipsGeneration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	// Pre-existing readable bundle; generator's bundle would NOT write (proving skip).
	if err := os.WriteFile(path, []byte("EXISTING\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &CABundleConfig{Path: path, GenerateCommand: writeFakeGenerator(t, dir, "CURRENT", true)}
	env, err := resolveCABundleEnv(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env["SSL_CERT_FILE"] != path {
		t.Errorf("env not injected: %v", env)
	}
	// If it skipped bundle, the file content is unchanged.
	data, _ := os.ReadFile(path)
	if string(data) != "EXISTING\n" {
		t.Errorf("bundle was regenerated despite CURRENT; content=%q", string(data))
	}
}

func TestResolveCABundle_StaleTriggersGeneration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	cfg := &CABundleConfig{Path: path, GenerateCommand: writeFakeGenerator(t, dir, "STALE", true)}
	env, err := resolveCABundleEnv(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env["REQUESTS_CA_BUNDLE"] != path {
		t.Errorf("env not injected: %v", env)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "FAKE-CA-BUNDLE\n" {
		t.Errorf("bundle not generated; content=%q", string(data))
	}
}

func TestResolveCABundle_UnknownTokenTreatedAsStale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	cfg := &CABundleConfig{Path: path, GenerateCommand: writeFakeGenerator(t, dir, "I_DONT_CARE", true)}
	_, err := resolveCABundleEnv(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bundleReadable(path) {
		t.Error("unknown token should have triggered bundle generation")
	}
}

func TestResolveCABundle_CurrentButMissing_NonFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	// check=CURRENT but no bundle exists and bundle subcommand does NOT create it.
	cfg := &CABundleConfig{Path: path, GenerateCommand: writeFakeGenerator(t, dir, "CURRENT", false), RequireBundle: false}
	env, err := resolveCABundleEnv(cfg)
	if err != nil {
		t.Fatalf("non-fatal path should not error, got %v", err)
	}
	if env != nil {
		t.Errorf("no env should be injected when bundle unreadable, got %v", env)
	}
}

func TestResolveCABundle_CurrentButMissing_Fatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	cfg := &CABundleConfig{Path: path, GenerateCommand: writeFakeGenerator(t, dir, "CURRENT", false), RequireBundle: true}
	_, err := resolveCABundleEnv(cfg)
	if err == nil {
		t.Fatal("require_bundle=true with missing bundle should return a fatal error")
	}
}

func TestInjectCAEnv_RespectsOverrides(t *testing.T) {
	env := map[string]string{"SSL_CERT_FILE": "/keep.pem"}
	injectCAEnv(env, "/new.pem")
	if env["SSL_CERT_FILE"] != "/keep.pem" {
		t.Errorf("existing override lost: %q", env["SSL_CERT_FILE"])
	}
	if env["NODE_EXTRA_CA_CERTS"] != "/new.pem" {
		t.Errorf("absent var not set: %q", env["NODE_EXTRA_CA_CERTS"])
	}
}

func TestMergeCAEnv_ExistingWins(t *testing.T) {
	cfgEnv := map[string]string{"SSL_CERT_FILE": "/custom.pem", "OTHER": "x"}
	merged := mergeCAEnv(cfgEnv, map[string]string{"SSL_CERT_FILE": "/ca.pem", "REQUESTS_CA_BUNDLE": "/ca.pem"})
	if merged["SSL_CERT_FILE"] != "/custom.pem" {
		t.Errorf("config env should win: %q", merged["SSL_CERT_FILE"])
	}
	if merged["REQUESTS_CA_BUNDLE"] != "/ca.pem" {
		t.Errorf("new var should be added: %q", merged["REQUESTS_CA_BUNDLE"])
	}
	if merged["OTHER"] != "x" {
		t.Errorf("unrelated env lost: %q", merged["OTHER"])
	}
}

func TestMergeCAEnv_EmptyCAEnvReturnsUnchanged(t *testing.T) {
	cfgEnv := map[string]string{"A": "1"}
	if got := mergeCAEnv(cfgEnv, nil); got["A"] != "1" || len(got) != 1 {
		t.Errorf("empty caEnv should return cfgEnv unchanged, got %v", got)
	}
}

func TestMergeCAEnv_NilCfgEnvCreatesMap(t *testing.T) {
	got := mergeCAEnv(nil, map[string]string{"SSL_CERT_FILE": "/ca.pem"})
	if got == nil || got["SSL_CERT_FILE"] != "/ca.pem" {
		t.Errorf("nil cfgEnv should be created and populated, got %v", got)
	}
}

// writeExitGenerator creates a generator that exits non-zero for the given
// subcommand (to exercise error branches). If bundleWrites is true, the bundle
// subcommand still writes a file before/independent of exit.
func writeExitGenerator(t *testing.T, dir, failSubcommand string) []string {
	t.Helper()
	script := filepath.Join(dir, "genfail.sh")
	body := "#!/usr/bin/env bash\ncase \"$1\" in\n" +
		"  " + failSubcommand + ") exit 7 ;;\n" +
		"  check) echo STALE ;;\n" +
		"  bundle) printf 'FAKE\\n' > \"$CA_BUNDLE_PATH\" ;;\n" +
		"  *) exit 0 ;;\nesac\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return []string{"bash", script}
}

func TestResolveCABundle_CheckCommandErrorTreatedAsStale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	// `check` exits non-zero -> treated as stale -> bundle runs and writes file.
	cfg := &CABundleConfig{Path: path, GenerateCommand: writeExitGenerator(t, dir, "check")}
	env, err := resolveCABundleEnv(cfg)
	if err != nil {
		t.Fatalf("check error should be non-fatal (treated as stale): %v", err)
	}
	if env["SSL_CERT_FILE"] != path {
		t.Errorf("bundle should have been generated after check error; env=%v", env)
	}
}

func TestResolveCABundle_BundleCommandErrorNonFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	// `check` says STALE, `bundle` exits non-zero and does not write -> unreadable
	// -> require_bundle=false -> no error, no env.
	cfg := &CABundleConfig{Path: path, GenerateCommand: writeExitGenerator(t, dir, "bundle"), RequireBundle: false}
	env, err := resolveCABundleEnv(cfg)
	if err != nil {
		t.Fatalf("bundle error with require_bundle=false should not error: %v", err)
	}
	if env != nil {
		t.Errorf("no env expected when bundle failed to produce a file, got %v", env)
	}
}

func TestInjectCAEnv_SetsAllWhenAbsent(t *testing.T) {
	env := map[string]string{}
	injectCAEnv(env, "/ca.pem")
	for _, k := range caEnvVars {
		if env[k] != "/ca.pem" {
			t.Errorf("%s not set to /ca.pem: %q", k, env[k])
		}
	}
}

func TestApplyCABundleToConfig_MergesEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	cfg := &Config{
		Env:      map[string]string{"EXISTING": "1"},
		CABundle: &CABundleConfig{Path: path, GenerateCommand: writeFakeGenerator(t, dir, "STALE", true)},
	}
	if err := applyCABundleToConfig(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Env["SSL_CERT_FILE"] != path {
		t.Errorf("CA env not merged into cfg.Env: %v", cfg.Env)
	}
	if cfg.Env["EXISTING"] != "1" {
		t.Errorf("existing env clobbered: %v", cfg.Env)
	}
}

func TestApplyCABundleToConfig_FatalWhenRequiredMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	cfg := &Config{
		CABundle: &CABundleConfig{Path: path, GenerateCommand: writeFakeGenerator(t, dir, "CURRENT", false), RequireBundle: true},
	}
	if err := applyCABundleToConfig(cfg); err == nil {
		t.Fatal("expected fatal error when required bundle is missing")
	}
}

func TestApplyCABundleToConfig_NoConfigNoOp(t *testing.T) {
	cfg := &Config{Env: map[string]string{"A": "1"}}
	if err := applyCABundleToConfig(cfg); err != nil {
		t.Fatalf("no ca_bundle should be a no-op, got %v", err)
	}
	if cfg.Env["A"] != "1" || len(cfg.Env) != 1 {
		t.Errorf("env changed unexpectedly: %v", cfg.Env)
	}
}
