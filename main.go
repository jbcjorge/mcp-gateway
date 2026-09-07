// Package main implements mcp-gateway, a local HTTP multiplexer for MCP stdio
// servers. It accepts MCP Streamable HTTP requests, routes them by path to
// subprocess backends, and handles lifecycle (lazy spawn, idle reaping, and
// self-termination for launchd socket activation).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Build-time variables injected via -ldflags.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// logLevel is the runtime-adjustable log level for the gateway.
var logLevel = new(slog.LevelVar)

// Exit codes. Use these constants for every os.Exit call.
const (
	exitOK             = 0 // clean exit
	exitFailure        = 1 // generic startup/runtime failure
	exitCABundleFailed = 3 // required CA bundle missing/unreadable (require_bundle=true)
)

// Size limits for request and response bodies.
const (
	maxRequestBodySize  = 5 * 1024 * 1024  // 5MB - JSON-RPC requests are small
	maxResponseBodySize = 50 * 1024 * 1024 // 50MB - tool responses can be large
	maxErrorBodySize    = 4096             // 4KB - error bodies for logging
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
	CABundle                *CABundleConfig   `json:"ca_bundle"`                          // optional CA bundle management (see CABundleConfig)
	AuthTokens              []string          `json:"auth_tokens"`                        // global bearer tokens (used when backend has no own tokens)
	IdleTimeout             int               `json:"idle_timeout_seconds"`               // kill backends idle for this long (0 = never)
	SelfIdleTimeout         int               `json:"self_idle_timeout_seconds"`          // kill gateway itself after this long with no requests (0 = never)
	CredentialTTLSoftMargin int               `json:"credential_ttl_soft_margin_seconds"` // recycle (defer-if-busy) this long before credential expiry (0 = default 300)
	CredentialTTLHardGuard  int               `json:"credential_ttl_hard_guard_seconds"`  // force-recycle this long before credential expiry (0 = default 120)
	BackendsFile            string            `json:"backends_file"`                      // path to backends.json (relative to config dir)
	// Backend topology (new schema, pre-1.0): servers/compositions/backends.
	// Loaded inline here or from BackendsFile.
	Servers      map[string]BackendDef     `json:"servers"`
	Compositions map[string]CompositionDef `json:"compositions"`
	Routes       []RouteEntry              `json:"backends"`
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
}

// Backend manages a single stdio MCP subprocess or remote connection.
type Backend struct {
	name      string
	def       BackendDef
	globalEnv map[string]string
	cacheDir  string // directory for persistent cache files

	mu            sync.Mutex
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	stdout        *bufio.Reader
	pending       map[string]chan json.RawMessage // id string -> response channel
	running       bool
	cancelFn      context.CancelFunc
	lastUsed      time.Time       // last time a request was forwarded
	toolsCache    json.RawMessage // cached tools/list response (nil = no cache)
	toolsCachePid int             // pid of the backend that produced the cache

	// Credential TTL watcher state. A backend may emit a stderr sentinel
	// "[[gateway:credential_expiry]] <unix-epoch>" after spawn, declaring when
	// its injected credential (e.g. an SSO session cookie) expires. The gateway
	// then recycles the backend before that time so the next spawn fetches a
	// fresh credential. Guarded by b.mu.
	credentialExpiry time.Time     // zero = no credential expiry declared
	ttlSoftMargin    time.Duration // recycle (defer-if-busy) at expiry-softMargin
	ttlHardGuard     time.Duration // force-recycle (even if busy) at expiry-hardGuard
	softTimer        *time.Timer   // fires at the soft deadline
	hardTimer        *time.Timer   // fires at the hard deadline
	pendingTTLKill   bool          // soft deadline reached while busy; recycle when idle

	// Remote backend state
	httpClient *http.Client                    // shared HTTP client for remote backends
	ssePending map[string]chan json.RawMessage // SSE response correlation

	// Tool discovery state
	categories   map[string][]string // category name -> tool names
	activeTools  map[string]bool     // set of activated tool names
	hasDiscovery bool                // true if backend natively has discover_tools
	maxDescLen   int                 // max description length (0 = no truncation)
	logEnabled   bool                // whether to log request/response activity for this backend
}

// jsonRPCMessage is a minimal JSON-RPC envelope for id extraction.
type jsonRPCMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
}

// Gateway holds all backends and routes requests.
type Gateway struct {
	config      Config
	backends    map[string]*Backend // all member instances (keyed by instance name) for lifecycle
	routes      map[string]*Route   // client-facing routes (keyed by route name)
	authorizer  Authorizer
	mu          sync.RWMutex
	lastRequest time.Time // last time any request was received
	reqMu       sync.Mutex

	// SSE listeners for subscriptions/listen
	listenersMu sync.Mutex
	listeners   []chan string // channels to push notifications to
}

func main() {
	// Handle --version / -v flag
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("mcp-gateway %s (%s) built %s\n", Version, Commit, BuildDate)
		os.Exit(exitOK)
	}

	configPath := resolveConfigPath()

	cfg, err := loadConfig(configPath)
	if err != nil {
		slog.Error("failed to load config", "path", configPath, "error", err)
		os.Exit(exitFailure)
	}

	// Configure logging
	if logErr := initLogging(cfg, configPath); logErr != nil {
		fmt.Fprintf(os.Stderr, "logging setup failed: %v\n", logErr)
		os.Exit(exitFailure)
	}

	// Single-instance check: if already running, verify and exit.
	if existingOK := checkExistingInstance(cfg.Listen); existingOK {
		fmt.Printf("mcp-gateway already running on %s\n", cfg.Listen)
		os.Exit(exitOK)
	}

	// Write pidfile
	pidFile := writePidFile()
	defer os.Remove(pidFile)

	// Resolve the optional CA bundle and inject its path into all backends'
	// environment. Company/OS specifics live entirely in the configured
	// generate command; the gateway only dispatches and injects.
	if caErr := applyCABundleToConfig(&cfg); caErr != nil {
		slog.Error("CA bundle required but unavailable", "error", caErr)
		os.Exit(exitCABundleFailed)
	}

	gw, gwErr := newGateway(cfg)
	if gwErr != nil {
		slog.Error("failed to build gateway from config", "error", gwErr)
		os.Exit(exitFailure)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", gw.handleRequest)

	server := &http.Server{
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
	}

	// Get listener BEFORE setting up signal handlers.
	// The launchd XPC syscall (launch_activate_socket) must happen before
	// Go's signal handling is initialized to avoid signal_recv crashes.
	ln, err := getListener(cfg.Listen)
	if err != nil {
		slog.Error("listener failed", "error", err)
		os.Exit(exitFailure)
	}

	// Graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		slog.Info("shutting down...")
		if err := server.Close(); err != nil {
			slog.Warn("server close error", "error", err)
		}
		gw.shutdownAll()
	}()

	// Start backend idle reaper
	if cfg.IdleTimeout > 0 {
		go gw.idleReaper(ctx, time.Duration(cfg.IdleTimeout)*time.Second)
		slog.Info("backend idle reaper enabled", "timeout_seconds", cfg.IdleTimeout)
	}

	// Start self-idle timer (gateway exits after no requests)
	if cfg.SelfIdleTimeout > 0 {
		go gw.selfIdleTimer(ctx, stop, time.Duration(cfg.SelfIdleTimeout)*time.Second)
		slog.Info("self-idle timer enabled", "timeout_seconds", cfg.SelfIdleTimeout)
	}

	slog.Info("mcp-gateway started", "version", Version, "commit", Commit, "addr", ln.Addr().String(), "pid", os.Getpid())
	if err := server.Serve(ln); err != http.ErrServerClosed {
		slog.Error("server error", "error", err)
		os.Exit(exitFailure)
	}
}

// pidFilePath returns the path to the pidfile.
func pidFilePath() string {
	return filepath.Join(os.TempDir(), "mcp-gateway.pid")
}

// removePidFile removes the pidfile, logging any failure.
func removePidFile(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		slog.Debug("failed to remove pidfile", "path", path, "error", err)
	}
}

// writePidFile creates the pidfile with the current process ID and returns its path.
func writePidFile() string {
	pidFile := pidFilePath()
	if err := os.MkdirAll(filepath.Dir(pidFile), 0700); err != nil {
		slog.Warn("failed to create pidfile directory", "error", err)
	}
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", os.Getpid())), 0600); err != nil {
		slog.Warn("failed to write pidfile", "error", err)
	}
	return pidFile
}

// checkExistingInstance checks if an instance is already running.
// Returns true if a healthy instance is reachable on the given address.
func checkExistingInstance(addr string) bool {
	// Check pidfile first
	pidFile := pidFilePath()
	pidData, err := os.ReadFile(pidFile) // #nosec G304 -- path is from pidFilePath() constant, not user input
	if err != nil {
		return false // no pidfile, not running
	}

	// Verify the process is still alive
	var pid int
	if _, scanErr := fmt.Sscanf(string(pidData), "%d", &pid); scanErr != nil {
		removePidFile(pidFile)
		return false
	}

	proc, findErr := os.FindProcess(pid)
	if findErr != nil {
		removePidFile(pidFile)
		return false
	}

	// On Unix, FindProcess always succeeds. Send signal 0 to check if alive.
	if sigErr := proc.Signal(syscall.Signal(0)); sigErr != nil {
		removePidFile(pidFile)
		return false
	}

	// Process exists. Verify it's actually responding on the port.
	host := addr
	if strings.HasPrefix(host, ":") {
		host = "localhost" + host
	}
	healthURL := fmt.Sprintf("http://%s/health", host)

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(healthURL)
	if err != nil {
		// Process alive but not responding - stale. Kill it.
		if killErr := proc.Signal(syscall.SIGTERM); killErr != nil {
			slog.Warn("failed to kill stale process", "pid", pid, "error", killErr)
		}
		time.Sleep(500 * time.Millisecond)
		removePidFile(pidFile)
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == 200
}

// rotatingWriter is an io.Writer that rotates the underlying file when it exceeds maxSize.
type rotatingWriter struct {
	path     string
	maxSize  int64
	maxFiles int
	mu       sync.Mutex
	file     *os.File
	size     int64
}

// credentialExpirySentinel is the stderr marker a backend emits to declare when
// its injected credential expires: "[[gateway:credential_expiry]] <unix-epoch>".
const credentialExpirySentinel = "[[gateway:credential_expiry]]"

// bytesTrimRight trims trailing whitespace characters from a byte slice.
func bytesTrimRight(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}

// truncate returns a string truncated to maxLen characters.
func truncate(b []byte, maxLen int) string {
	if len(b) <= maxLen {
		return string(b)
	}
	return string(b[:maxLen]) + "..."
}

// toolEntry is a minimal tool descriptor for category building.
type toolEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}
