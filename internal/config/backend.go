package config

// AuthHook declares how the gateway obtains a backend's upstream credential for
// the OAuth broker. Provider is a command run by the gateway that prints the
// bearer token to stdout (optionally JSON {"token":...,"expires_at":<unix>});
// a non-zero exit means auth failed. TokenFormat "jwt" makes the gateway read
// the token's exp claim; otherwise TTLSeconds is used. Expose selects the token
// model: "real" (default, IMPLEMENTED) returns the upstream token to the client
// so the gateway stays token-stateless; "opaque" (declared but NOT yet
// implemented) would keep the real token server-side behind a handle.
// See docs/design/auth-broker.md.
type AuthHook struct {
	Provider    []string `json:"provider"`     // command -> prints token to stdout
	TokenFormat string   `json:"token_format"` // "jwt" (read exp) | "opaque" (use ttl); default jwt
	TTLSeconds  int      `json:"ttl_seconds"`  // fallback validity when exp is unknown
	Expose      string   `json:"expose"`       // "real" (default, implemented) | "opaque" (future, not implemented)
}

// BackendDef defines a backend MCP subprocess or remote server.
type BackendDef struct {
	Command      []string            `json:"command"`
	Env          map[string]string   `json:"env"`
	Discovery    *bool               `json:"discovery"`     // nil=auto (>5 tools), true=force, false=disable
	Categories   map[string][]string `json:"categories"`    // manual category overrides: {"read": ["tool_a", "tool_b"]}
	IncludeTools []string            `json:"include_tools"` // glob patterns for tools to expose (empty = all)
	ExcludeTools []string            `json:"exclude_tools"` // glob patterns for tools to block (applied after include)

	// Remote backend fields (mutually exclusive with Command)
	URL           string            `json:"url"`            // remote MCP server URL (SSE or streamable HTTP)
	TransportType string            `json:"transport_type"` // "sse" or "streamable-http" (default: inferred from URL)
	Headers       map[string]string `json:"headers"`        // extra HTTP headers for remote connections

	// Per-backend options
	AuthTokens []string `json:"auth_tokens"` // bearer tokens for this backend (overrides global)
	LogEnabled *bool    `json:"log_enabled"` // nil=inherit global, true=verbose, false=quiet

	// Auth, when set, makes this route participate in the gateway OAuth broker:
	// MCP clients authenticate via the gateway's /authorize + /token, and the
	// gateway runs Auth.Provider to obtain the upstream token. Nil = no OAuth
	// (client connects unauthenticated, as today). See docs/design/auth-broker.md.
	Auth *AuthHook `json:"auth"`

	// IdleTimeoutSeconds overrides the global idle_timeout_seconds for this
	// backend. nil = inherit global, 0 = never reap (stays alive until the
	// gateway exits), >0 = kill after this many seconds of inactivity. Useful
	// for long-lived backends like a browser (Chrome CDP) that should not be
	// reaped on the global cadence.
	IdleTimeoutSeconds *int `json:"idle_timeout_seconds"`

	// StopGraceSeconds overrides the global stop_grace_seconds for this backend:
	// the delay between the SIGTERM sent to the backend's process group on stop
	// and the escalation to SIGKILL. Servers that own heavy child processes
	// (e.g. a browser) may need longer to tear down cleanly. nil = inherit global.
	StopGraceSeconds *int `json:"stop_grace_seconds"`

	// Params, when non-empty, marks this server as a KEYED backend: a family of
	// identical child processes that differ only by routing coordinate(s) the
	// client supplies per tool call. The gateway injects each param into every
	// tool's input schema (tools/list), reads+strips them on tools/call, hashes
	// their values to a stable instance id, lazily spawns/reuses one child per
	// id (registered in gw.backends as "<route>/<id>" so the existing reaper,
	// credential-TTL, health, and shutdown apply per child), and passes each
	// value to the child as an environment variable. All upstream specifics
	// (URL, token, extra vars, sanitization) are the child launcher's concern;
	// the gateway only transports the key. Empty/absent = ordinary backend.
	// See docs/design/keyed-backends.md.
	Params []Param `json:"params"`
}

// Param is one routing parameter of a keyed backend. The gateway injects it into
// every tool's input schema, reads and strips it on tools/call, includes its
// value in the child's identity hash, and passes the value to the child as an
// environment variable.
type Param struct {
	Name        string `json:"name"`        // injected tool-arg name, e.g. "sonarUrl" (required)
	Description string `json:"description"` // help text shown to the agent
	Validate    string `json:"validate"`    // optional regex; empty = accept any value
	EnvName     string `json:"envName"`     // optional child env var name; empty = use Name
	Required    *bool  `json:"required"`    // optional; nil = treated as required
}

// IsKeyed reports whether this backend definition declares routing params.
func (d BackendDef) IsKeyed() bool { return len(d.Params) > 0 }

// IsRequired reports whether a param is required (nil Required defaults true).
func (p Param) IsRequired() bool { return p.Required == nil || *p.Required }

// EnvVar returns the environment variable name used to pass this param's
// value to a child process: EnvName when set, otherwise Name.
func (p Param) EnvVar() string {
	if p.EnvName != "" {
		return p.EnvName
	}
	return p.Name
}
