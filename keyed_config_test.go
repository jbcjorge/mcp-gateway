package main

import (
	"testing"
)

// TestLoadConfigKeyedParams verifies that a server template with a non-empty
// "params" list parses correctly and is detected as a keyed backend.
func TestLoadConfigKeyedParams(t *testing.T) {
	dir := t.TempDir()
	_ = writeTestFile(dir+"/config.json", `{"backends_file":"backends.json"}`)
	_ = writeTestFile(dir+"/backends.json", `{
		"servers": {
			"sonarqube": {
				"command": ["bash", "/path/run.sh"],
				"idle_timeout_seconds": 300,
				"params": [
					{
						"name": "sonarUrl",
						"description": "Target SonarQube URL",
						"validate": "^https://",
						"envName": "SONARQUBE_URL",
						"required": true
					},
					{
						"name": "organization"
					}
				]
			}
		},
		"backends": ["sonarqube"]
	}`)

	cfg, err := loadConfig(dir + "/config.json")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	def, ok := cfg.Servers["sonarqube"]
	if !ok {
		t.Fatal("expected sonarqube server")
	}
	if !def.isKeyed() {
		t.Fatal("expected sonarqube to be detected as keyed")
	}
	if len(def.Params) != 2 {
		t.Fatalf("expected 2 params, got %d", len(def.Params))
	}

	p0 := def.Params[0]
	if p0.Name != "sonarUrl" {
		t.Errorf("param[0].Name = %q, want sonarUrl", p0.Name)
	}
	if p0.Description != "Target SonarQube URL" {
		t.Errorf("param[0].Description = %q", p0.Description)
	}
	if p0.Validate != "^https://" {
		t.Errorf("param[0].Validate = %q", p0.Validate)
	}
	if p0.childEnvName() != "SONARQUBE_URL" {
		t.Errorf("param[0].childEnvName() = %q, want SONARQUBE_URL", p0.childEnvName())
	}
	if !p0.paramRequired() {
		t.Error("param[0] should be required (explicit true)")
	}

	p1 := def.Params[1]
	// envName omitted => defaults to Name
	if p1.childEnvName() != "organization" {
		t.Errorf("param[1].childEnvName() = %q, want organization", p1.childEnvName())
	}
	// required omitted => defaults to true
	if !p1.paramRequired() {
		t.Error("param[1] should default to required when Required is nil")
	}
}

// TestLoadConfigNonKeyed verifies that a server without params is not keyed.
func TestLoadConfigNonKeyed(t *testing.T) {
	dir := t.TempDir()
	_ = writeTestFile(dir+"/config.json", `{"backends_file":"backends.json"}`)
	_ = writeTestFile(dir+"/backends.json", `{
		"servers": {"plain": {"command": ["cat"]}},
		"backends": ["plain"]
	}`)

	cfg, err := loadConfig(dir + "/config.json")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	def := cfg.Servers["plain"]
	if def.isKeyed() {
		t.Error("plain server without params should not be keyed")
	}
	if len(def.Params) != 0 {
		t.Errorf("expected 0 params, got %d", len(def.Params))
	}
}

// TestParamRequiredDefault checks the required-default logic directly.
func TestParamRequiredDefault(t *testing.T) {
	tru := true
	fls := false
	cases := []struct {
		name string
		p    Param
		want bool
	}{
		{"nil defaults true", Param{Name: "a"}, true},
		{"explicit true", Param{Name: "a", Required: &tru}, true},
		{"explicit false", Param{Name: "a", Required: &fls}, false},
	}
	for _, c := range cases {
		if got := c.p.paramRequired(); got != c.want {
			t.Errorf("%s: paramRequired() = %v, want %v", c.name, got, c.want)
		}
	}
}
