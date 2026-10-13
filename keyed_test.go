package main

import (
	"encoding/json"
	"testing"
)

func TestInstanceIDStableAndDistinct(t *testing.T) {
	a := instanceID([]string{"https://aether-sonarqube.dos.corproot.net"})
	b := instanceID([]string{"https://aether-sonarqube.dos.corproot.net"})
	if a != b {
		t.Errorf("same values must yield same id: %s != %s", a, b)
	}
	if len(a) != instanceIDLen {
		t.Errorf("id length = %d, want %d", len(a), instanceIDLen)
	}

	c := instanceID([]string{"https://billing-sonarqube.dos.corproot.net"})
	if a == c {
		t.Error("different values must yield different ids")
	}

	// Trailing slash is a different id by design (no canonicalization).
	d := instanceID([]string{"https://aether-sonarqube.dos.corproot.net/"})
	if a == d {
		t.Error("trailing slash should produce a distinct id")
	}
}

func TestInstanceIDOrderAndSeparator(t *testing.T) {
	// Order matters.
	ab := instanceID([]string{"a", "b"})
	ba := instanceID([]string{"b", "a"})
	if ab == ba {
		t.Error("value order must affect id")
	}
	// NUL separator prevents concatenation collisions: ["ab",""] vs ["a","b"].
	x := instanceID([]string{"ab", ""})
	y := instanceID([]string{"a", "b"})
	if x == y {
		t.Error("separator must prevent concatenation collision")
	}
}

func keyedTestParams() []Param {
	tru := true
	fls := false
	return []Param{
		{Name: "sonarUrl", Validate: "^https://", Required: &tru},
		{Name: "org", Required: &fls},
	}
}

func TestExtractParamValues_ValidWithOptional(t *testing.T) {
	args := map[string]json.RawMessage{
		"sonarUrl": json.RawMessage(`"https://x.example"`),
		"org":      json.RawMessage(`"myorg"`),
	}
	vals, byName, err := extractParamValues(keyedTestParams(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vals[0] != "https://x.example" || vals[1] != "myorg" {
		t.Errorf("values = %v", vals)
	}
	if byName["sonarUrl"] != "https://x.example" || byName["org"] != "myorg" {
		t.Errorf("byName = %v", byName)
	}
}

func TestExtractParamValues_OptionalAbsent(t *testing.T) {
	args := map[string]json.RawMessage{
		"sonarUrl": json.RawMessage(`"https://x.example"`),
	}
	vals, byName, err := extractParamValues(keyedTestParams(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vals[1] != "" {
		t.Errorf("absent optional should be empty, got %q", vals[1])
	}
	if _, ok := byName["org"]; ok {
		t.Error("absent optional should not appear in byName")
	}
}

func TestExtractParamValues_RequiredMissing(t *testing.T) {
	_, _, err := extractParamValues(keyedTestParams(), map[string]json.RawMessage{})
	if err == nil {
		t.Fatal("expected error for missing required param")
	}
}

func TestExtractParamValues_ValidationFailure(t *testing.T) {
	args := map[string]json.RawMessage{"sonarUrl": json.RawMessage(`"ftp://nope"`)}
	_, _, err := extractParamValues(keyedTestParams(), args)
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestExtractParamValues_NonString(t *testing.T) {
	args := map[string]json.RawMessage{"sonarUrl": json.RawMessage(`123`)}
	_, _, err := extractParamValues(keyedTestParams(), args)
	if err == nil {
		t.Fatal("expected non-string error")
	}
}

func TestInjectParamsIntoTools(t *testing.T) {
	params := []Param{
		{Name: "sonarUrl", Description: "the url"},
	}
	result := json.RawMessage(`{"tools":[
		{"name":"search_issues","description":"d","inputSchema":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}},
		{"name":"no_schema","description":"d2"}
	]}`)

	out := injectParamsIntoTools(result, params)

	var parsed struct {
		Tools []struct {
			Name        string `json:"name"`
			InputSchema struct {
				Properties map[string]any `json:"properties"`
				Required   []string       `json:"required"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed.Tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(parsed.Tools))
	}

	// Tool 1: existing schema augmented, keeps its own prop + required.
	t0 := parsed.Tools[0]
	if _, ok := t0.InputSchema.Properties["sonarUrl"]; !ok {
		t.Error("sonarUrl not injected into tool with existing schema")
	}
	if _, ok := t0.InputSchema.Properties["query"]; !ok {
		t.Error("existing property 'query' was lost")
	}
	if !containsString(t0.InputSchema.Required, "sonarUrl") || !containsString(t0.InputSchema.Required, "query") {
		t.Errorf("required mismatch: %v", t0.InputSchema.Required)
	}

	// Tool 2: schema created from scratch.
	t1 := parsed.Tools[1]
	if _, ok := t1.InputSchema.Properties["sonarUrl"]; !ok {
		t.Error("sonarUrl not injected into tool lacking a schema")
	}
	if !containsString(t1.InputSchema.Required, "sonarUrl") {
		t.Error("sonarUrl should be required in generated schema")
	}
}

func TestInjectParamsOptionalNotRequired(t *testing.T) {
	fls := false
	params := []Param{{Name: "org", Required: &fls}}
	result := json.RawMessage(`{"tools":[{"name":"t","inputSchema":{"type":"object","properties":{}}}]}`)
	out := injectParamsIntoTools(result, params)

	var parsed struct {
		Tools []struct {
			InputSchema struct {
				Properties map[string]any `json:"properties"`
				Required   []string       `json:"required"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(out, &parsed)
	if _, ok := parsed.Tools[0].InputSchema.Properties["org"]; !ok {
		t.Error("optional param should still be injected as a property")
	}
	if containsString(parsed.Tools[0].InputSchema.Required, "org") {
		t.Error("optional param must not be in required")
	}
}

func TestStripParamsFromCall(t *testing.T) {
	params := []Param{{Name: "sonarUrl"}, {Name: "org"}}
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_issues","arguments":{"sonarUrl":"https://x","org":"o","query":"foo"}}}`)

	out, err := stripParamsFromCall(body, params)
	if err != nil {
		t.Fatalf("strip: %v", err)
	}
	args, err := callArguments(out)
	if err != nil {
		t.Fatalf("callArguments: %v", err)
	}
	if _, ok := args["sonarUrl"]; ok {
		t.Error("sonarUrl should be stripped")
	}
	if _, ok := args["org"]; ok {
		t.Error("org should be stripped")
	}
	if _, ok := args["query"]; !ok {
		t.Error("query should be preserved")
	}
	// Tool name must be preserved.
	if callToolName(out) != "search_issues" {
		t.Errorf("tool name altered: %q", callToolName(out))
	}
}

func TestStripParamsNoArguments(t *testing.T) {
	params := []Param{{Name: "sonarUrl"}}
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t"}}`)
	out, err := stripParamsFromCall(body, params)
	if err != nil {
		t.Fatalf("strip: %v", err)
	}
	if callToolName(out) != "t" {
		t.Errorf("tool name altered: %q", callToolName(out))
	}
}

func TestChildInstanceNameHelpers(t *testing.T) {
	n := childInstanceName("sonarqube", "abc123")
	if n != "sonarqube/abc123" {
		t.Errorf("childInstanceName = %q", n)
	}
	if !isChildInstanceName("sonarqube", "sonarqube/abc123") {
		t.Error("should recognize child instance name")
	}
	if isChildInstanceName("sonarqube", "other/abc123") {
		t.Error("should not match a different route")
	}
}
