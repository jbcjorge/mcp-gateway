package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"

	errors "github.com/jbcjorge/errors-library"
)

// instanceIDLen is the number of hex characters of the SHA-256 digest used as a
// keyed child's instance id. 12 hex chars (48 bits) is short, log- and
// filesystem-safe, with negligible collision risk at the scale of a local
// gateway (a handful of concurrent instances per keyed route).
const instanceIDLen = 12

// instanceID computes the stable identity of a keyed child from the ordered
// param values. The values are joined with a NUL separator (which cannot appear
// in a JSON string value) so that distinct value tuples cannot collide via
// concatenation, then hashed with SHA-256 and truncated. The gateway does NOT
// canonicalize values; "https://x/" and "https://x" are different ids by design
// (the upstream launcher owns canonicalization if it wants it).
func instanceID(values []string) string {
	h := sha256.New()
	for i, v := range values {
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write([]byte(v))
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum)[:instanceIDLen]
}

// extractParamValues reads the declared params from a tools/call arguments
// object. It returns the ordered values (one per declared param, in declared
// order), validating required-ness and the optional regex. Missing optional
// params contribute an empty string to the value tuple so identity stays stable
// and order-aligned. The returned map is name->value for env injection.
func extractParamValues(params []Param, arguments map[string]json.RawMessage) ([]string, map[string]string, error) {
	values := make([]string, len(params))
	byName := make(map[string]string, len(params))

	for i, p := range params {
		v, present, err := extractOneParam(p, arguments)
		if err != nil {
			return nil, nil, err
		}
		values[i] = v
		if present {
			byName[p.Name] = v
		}
	}
	return values, byName, nil
}

// extractOneParam resolves a single param's value from the arguments, applying
// required-ness and the optional regex. present is false for an absent optional
// param (value is "").
func extractOneParam(p Param, arguments map[string]json.RawMessage) (value string, present bool, err error) {
	raw, ok := arguments[p.Name]
	if !ok {
		if p.paramRequired() {
			return "", false, ErrKeyedParamMissing.Parse(errors.WithParsedMessage(p.Name))
		}
		return "", false, nil
	}
	var s string
	if uerr := json.Unmarshal(raw, &s); uerr != nil {
		return "", false, ErrKeyedParamNotString.Parse(errors.WithParsedMessage(p.Name))
	}
	if p.Validate != "" {
		re, cerr := regexp.Compile(p.Validate)
		if cerr != nil {
			return "", false, ErrKeyedParamBadRegex.Parse(errors.WithParsedMessage(p.Name), errors.WithError(cerr))
		}
		if !re.MatchString(s) {
			return "", false, ErrKeyedParamInvalid.Parse(errors.WithParsedMessage(p.Name))
		}
	}
	return s, true, nil
}

// injectParamsIntoTools augments a tools/list "result" object so every tool's
// inputSchema advertises the keyed params. Each param is added to
// inputSchema.properties (type string, with its description) and, when required,
// appended to inputSchema.required. Tools with no object schema get one. The
// input is the raw "result" object (containing a "tools" array); the output is a
// new result object. On any structural surprise the original is returned
// unchanged (fail-open: better to expose un-augmented tools than none).
func injectParamsIntoTools(result json.RawMessage, params []Param) json.RawMessage {
	if len(params) == 0 {
		return result
	}
	var wrapper map[string]json.RawMessage
	if json.Unmarshal(result, &wrapper) != nil {
		return result
	}
	rawTools, ok := wrapper["tools"]
	if !ok {
		return result
	}
	var tools []map[string]any
	if json.Unmarshal(rawTools, &tools) != nil {
		return result
	}
	for _, tool := range tools {
		injectParamsIntoSchema(tool, params)
	}
	toolsJSON, err := json.Marshal(tools)
	if err != nil {
		return result
	}
	wrapper["tools"] = toolsJSON
	out, err := json.Marshal(wrapper)
	if err != nil {
		return result
	}
	return out
}

// injectParamsIntoSchema mutates a single tool map, adding the keyed params to
// its inputSchema.properties and required list.
func injectParamsIntoSchema(tool map[string]any, params []Param) {
	schema, _ := tool["inputSchema"].(map[string]any)
	if schema == nil {
		schema = map[string]any{"type": "object"}
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	// Existing required list (may be absent or []any from JSON).
	required := toStringSlice(schema["required"])

	for _, p := range params {
		props[p.Name] = map[string]any{
			"type":        "string",
			"description": p.Description,
		}
		if p.paramRequired() && !containsString(required, p.Name) {
			required = append(required, p.Name)
		}
	}

	schema["properties"] = props
	if len(required) > 0 {
		schema["required"] = required
	}
	tool["inputSchema"] = schema
}

// stripParamsFromCall returns a copy of a tools/call body with the keyed params
// removed from params.arguments, so the child (which does not know these args)
// receives a clean call. Returns the original body if there are no arguments to
// strip.
func stripParamsFromCall(body []byte, params []Param) ([]byte, error) {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, ErrKeyedStripParse.Parse(errors.WithError(err))
	}
	rawParams, ok := msg["params"]
	if !ok {
		return body, nil
	}
	var callParams map[string]json.RawMessage
	if err := json.Unmarshal(rawParams, &callParams); err != nil {
		return nil, ErrKeyedStripParse.Parse(errors.WithError(err))
	}
	rawArgs, ok := callParams["arguments"]
	if !ok {
		return body, nil
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, ErrKeyedStripParse.Parse(errors.WithError(err))
	}
	for _, p := range params {
		delete(args, p.Name)
	}
	argsJSON, _ := json.Marshal(args)
	callParams["arguments"] = argsJSON
	paramsJSON, _ := json.Marshal(callParams)
	msg["params"] = paramsJSON
	return json.Marshal(msg)
}

// callArguments extracts params.arguments from a tools/call body as a raw map.
func callArguments(body []byte) (map[string]json.RawMessage, error) {
	var env struct {
		Params *struct {
			Arguments map[string]json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, ErrKeyedStripParse.Parse(errors.WithError(err))
	}
	if env.Params == nil || env.Params.Arguments == nil {
		return map[string]json.RawMessage{}, nil
	}
	return env.Params.Arguments, nil
}

// toStringSlice coerces a JSON-decoded value (nil, []any, or []string) into a
// []string, dropping non-string elements.
func toStringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// childInstanceName builds the gw.backends registry key for a keyed child.
func childInstanceName(route, id string) string {
	return route + "/" + id
}

// isChildInstanceName reports whether a backend name is a keyed child of the
// given route (route + "/" + id).
func isChildInstanceName(route, name string) bool {
	return strings.HasPrefix(name, route+"/")
}
