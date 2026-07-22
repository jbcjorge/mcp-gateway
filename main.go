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
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	errors "github.com/jbcjorge/errors-library"
)

// Build-time variables injected via -ldflags.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// logLevel is the runtime-adjustable log level for the gateway.
var logLevel = new(slog.LevelVar)

// Size limits for request and response bodies.
const (
	maxRequestBodySize  = 5 * 1024 * 1024  // 5MB - JSON-RPC requests are small
	maxResponseBodySize = 50 * 1024 * 1024 // 50MB - tool responses can be large
	maxErrorBodySize    = 4096             // 4KB - error bodies for logging
)

// Config is the top-level configuration.
type Config struct {
	Listen          string                `json:"listen"`
	LogLevel        string                `json:"log_level"`                 // debug, info, warn, error (default: info)
	LogFile         string                `json:"log_file"`                  // path to log file (default: stderr)
	LogMaxSizeMB    int                   `json:"log_max_size_mb"`           // max log file size in MB before rotation (default: 10)
	LogMaxFiles     int                   `json:"log_max_files"`             // number of rotated files to keep (default: 3)
	CacheDir        string                `json:"cache_dir"`                 // directory for persistent tools cache (default: alongside config)
	MaxDescLen      int                   `json:"max_description_length"`    // truncate tool descriptions to this length (0 = no truncation)
	Discovery       *bool                 `json:"discovery"`                 // global discovery toggle: nil=auto, true=force, false=disable for all backends
	Env             map[string]string     `json:"env"`                       // global environment variables for all backends
	AuthTokens      []string              `json:"auth_tokens"`               // global bearer tokens (used when backend has no own tokens)
	IdleTimeout     int                   `json:"idle_timeout_seconds"`      // kill backends idle for this long (0 = never)
	SelfIdleTimeout int                   `json:"self_idle_timeout_seconds"` // kill gateway itself after this long with no requests (0 = never)
	BackendsFile    string                `json:"backends_file"`             // path to backends.json (relative to config dir)
	Backends        map[string]BackendDef `json:"backends"`
}

// BackendDef defines a backend MCP subprocess or remote server.
type BackendDef struct {
	Command      []string            `json:"command"`
	Env          map[string]string   `json:"env"`
	Discovery    *bool               `json:"discovery"`     // nil=auto (>5 tools), true=force, false=disable
	Categories   map[string][]string `json:"categories"`    // manual category overrides: {"read": ["tool_a", "tool_b"]}
	IncludeTools []string            `json:"include_tools"` // glob patterns for tools to expose (empty = all)
	ExcludeTools []string            `json:"exclude_tools"` // glob patterns for tools to block (applied after include)
	Disabled     bool                `json:"disabled"`      // skip this backend entirely

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

	mu         sync.Mutex
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdout     *bufio.Reader
	pending    map[string]chan json.RawMessage // id string -> response channel
	running    bool
	cancelFn   context.CancelFunc
	lastUsed   time.Time       // last time a request was forwarded
	toolsCache json.RawMessage // cached tools/list response (nil = no cache)

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
	backends    map[string]*Backend
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
		os.Exit(0)
	}

	configPath := resolveConfigPath()

	cfg, err := loadConfig(configPath)
	if err != nil {
		slog.Error("failed to load config", "path", configPath, "error", err)
		os.Exit(1)
	}

	// Configure logging
	if logErr := initLogging(cfg, configPath); logErr != nil {
		fmt.Fprintf(os.Stderr, "logging setup failed: %v\n", logErr)
		os.Exit(1)
	}

	// Single-instance check: if already running, verify and exit.
	if existingOK := checkExistingInstance(cfg.Listen); existingOK {
		fmt.Printf("mcp-gateway already running on %s\n", cfg.Listen)
		os.Exit(0)
	}

	// Write pidfile
	pidFile := writePidFile()
	defer os.Remove(pidFile)

	gw := newGateway(cfg)

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
		os.Exit(1)
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
		os.Exit(1)
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

// initLogging configures the structured logger from the config.
// MCP_GATEWAY_DEBUG environment variable overrides the configured level.
func initLogging(cfg Config, configPath string) error {
	logLevel.Set(parseLogLevel(cfg.LogLevel))
	if os.Getenv("MCP_GATEWAY_DEBUG") != "" {
		logLevel.Set(slog.LevelDebug)
	}
	var logOutput io.Writer = os.Stderr
	if cfg.LogFile != "" {
		logPath := cfg.LogFile
		if !filepath.IsAbs(logPath) {
			logPath = filepath.Join(filepath.Dir(configPath), logPath)
		}
		rw, err := newRotatingWriter(logPath, cfg.LogMaxSizeMB, cfg.LogMaxFiles)
		if err != nil {
			return ErrLogFileOpen.Parse(errors.WithParsedMessage(logPath), errors.WithError(err))
		}
		logOutput = rw
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(logOutput, &slog.HandlerOptions{Level: logLevel})))
	return nil
}

// newGateway creates a Gateway with backends initialized from the config.
func newGateway(cfg Config) *Gateway {
	gw := &Gateway{
		config:     cfg,
		backends:   make(map[string]*Backend),
		authorizer: NewBearerAuthorizer(cfg.AuthTokens, cfg.Backends),
	}

	for name, def := range cfg.Backends {
		if def.Disabled {
			slog.Info("backend disabled, skipping", "backend", name)
			continue
		}
		if def.Discovery == nil && cfg.Discovery != nil {
			def.Discovery = cfg.Discovery
		}
		b := &Backend{
			name:        name,
			def:         def,
			globalEnv:   cfg.Env,
			cacheDir:    cfg.CacheDir,
			maxDescLen:  cfg.MaxDescLen,
			pending:     make(map[string]chan json.RawMessage),
			activeTools: make(map[string]bool),
			logEnabled:  def.LogEnabled == nil || *def.LogEnabled,
		}
		if def.URL != "" {
			b.httpClient = &http.Client{Timeout: 120 * time.Second}
			b.ssePending = make(map[string]chan json.RawMessage)
		}
		b.loadToolsCache()
		b.buildCategories()
		gw.backends[name] = b
	}

	gw.lastRequest = time.Now()
	return gw
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

// newRotatingWriter opens or creates a log file with rotation support.
func newRotatingWriter(path string, maxSizeMB, maxFiles int) (*rotatingWriter, error) {
	if maxSizeMB <= 0 {
		maxSizeMB = 10
	}
	if maxFiles <= 0 {
		maxFiles = 3
	}
	w := &rotatingWriter{
		path:     path,
		maxSize:  int64(maxSizeMB) * 1024 * 1024,
		maxFiles: maxFiles,
	}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingWriter) open() error {
	if err := os.MkdirAll(filepath.Dir(w.path), 0750); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0640) // #nosec G302 -- log files need group-read for ops tools
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		if closeErr := f.Close(); closeErr != nil {
			slog.Debug("close after stat failure", "error", closeErr)
		}
		return err
	}
	w.file = f
	w.size = info.Size()
	return nil
}

func (w *rotatingWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.size+int64(len(p)) > w.maxSize {
		w.rotate()
	}
	n, err = w.file.Write(p)
	w.size += int64(n)
	return
}

func (w *rotatingWriter) rotate() {
	if err := w.file.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "mcp-gateway: log rotate close failed: %v\n", err)
	}
	// Shift old files: .3 -> delete, .2 -> .3, .1 -> .2, current -> .1
	for i := w.maxFiles; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", w.path, i)
		dst := fmt.Sprintf("%s.%d", w.path, i+1)
		if i == w.maxFiles {
			if err := os.Remove(src); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "mcp-gateway: log rotate remove %s failed: %v\n", src, err)
			}
		} else {
			if err := os.Rename(src, dst); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "mcp-gateway: log rotate rename %s -> %s failed: %v\n", src, dst, err)
			}
		}
	}
	if err := os.Rename(w.path, w.path+".1"); err != nil {
		fmt.Fprintf(os.Stderr, "mcp-gateway: log rotate rename %s -> %s.1 failed: %v\n", w.path, w.path, err)
	}
	if err := w.open(); err != nil {
		fmt.Fprintf(os.Stderr, "mcp-gateway: log rotate reopen failed: %v\n", err)
	}
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

	// Load backends from separate file if specified
	if cfg.BackendsFile != "" {
		backendsPath := cfg.BackendsFile
		if !filepath.IsAbs(backendsPath) {
			backendsPath = filepath.Join(filepath.Dir(path), backendsPath)
		}
		backendsData, err := os.ReadFile(backendsPath) // #nosec G703 G304 -- path from config file, admin-controlled
		if err != nil {
			return cfg, ErrBackendsLoad.Parse(errors.WithParsedMessage(backendsPath), errors.WithError(err))
		}
		if err := json.Unmarshal(backendsData, &cfg.Backends); err != nil {
			return cfg, ErrBackendsParse.Parse(errors.WithParsedMessage(backendsPath), errors.WithError(err))
		}
	}

	if cfg.Backends == nil {
		cfg.Backends = make(map[string]BackendDef)
	}
	return cfg, nil
}


// selfIdleTimer exits the gateway if no requests have been received for the given duration.
func (gw *Gateway) selfIdleTimer(ctx context.Context, stop context.CancelFunc, timeout time.Duration) {
	interval := timeout / 6
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			gw.reqMu.Lock()
			idle := time.Since(gw.lastRequest)
			gw.reqMu.Unlock()

			if idle > timeout {
				slog.Info("no requests, self-terminating", "idle", idle.Round(time.Second))
				stop() // triggers graceful shutdown
				return
			}
		}
	}
}

// touchLastRequest updates the gateway-level last request timestamp.
func (gw *Gateway) touchLastRequest() {
	gw.reqMu.Lock()
	gw.lastRequest = time.Now()
	gw.reqMu.Unlock()
}

// notifyToolsChanged sends notifications/tools/list_changed to all SSE listeners.
func (gw *Gateway) notifyToolsChanged() {
	notification := `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`
	gw.listenersMu.Lock()
	defer gw.listenersMu.Unlock()
	for _, ch := range gw.listeners {
		select {
		case ch <- notification:
		default:
			// Listener buffer full, skip (non-blocking)
		}
	}
	if len(gw.listeners) > 0 {
		slog.Debug("tools/list_changed pushed to listeners", "count", len(gw.listeners))
	}
}

// addListener registers an SSE listener channel and returns a remove function.
func (gw *Gateway) addListener(ch chan string) func() {
	gw.listenersMu.Lock()
	gw.listeners = append(gw.listeners, ch)
	gw.listenersMu.Unlock()
	return func() {
		gw.listenersMu.Lock()
		for i, l := range gw.listeners {
			if l == ch {
				gw.listeners = append(gw.listeners[:i], gw.listeners[i+1:]...)
				break
			}
		}
		gw.listenersMu.Unlock()
	}
}

// handleSubscriptionsListen keeps the response open as an SSE stream and pushes
// notifications (like tools/list_changed) when they occur.
func (gw *Gateway) handleSubscriptionsListen(w http.ResponseWriter, r *http.Request, env jsonRPCMessage) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Register listener
	ch := make(chan string, 16)
	remove := gw.addListener(ch)
	defer remove()

	slog.Info("subscriptions/listen stream opened")

	// Stream until client disconnects
	ctx := r.Context()
	keepAlive := time.NewTicker(30 * time.Second)
	defer keepAlive.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("subscriptions/listen stream closed")
			return
		case msg := <-ch:
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
			flusher.Flush()
		case <-keepAlive.C:
			// SSE keep-alive comment
			fmt.Fprintf(w, ":\n\n")
			flusher.Flush()
		}
	}
}

// handleRequest routes /<backend>/mcp to the appropriate backend.
func (gw *Gateway) handleRequest(w http.ResponseWriter, r *http.Request) {
	// Restart endpoint: POST /_restart/<backend>
	if strings.HasPrefix(r.URL.Path, "/_restart/") {
		gw.handleRestart(w, r)
		return
	}

	// Health endpoint (GET)
	if r.URL.Path == "/health" {
		gw.handleHealth(w)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Track gateway-level activity
	gw.touchLastRequest()

	// Parse path: /<backend>/mcp or /<backend>
	path := strings.TrimPrefix(r.URL.Path, "/")
	path = strings.TrimSuffix(path, "/mcp")
	path = strings.TrimSuffix(path, "/")

	if path == "" {
		http.Error(w, "missing backend name in path", http.StatusBadRequest)
		return
	}

	gw.mu.RLock()
	backend, ok := gw.backends[path]
	gw.mu.RUnlock()
	if !ok {
		http.Error(w, fmt.Sprintf("unknown backend: %s", path), http.StatusNotFound)
		return
	}

	// Auth check
	if authErr := gw.authorizer.Authorize(r, path); authErr != nil {
		w.WriteHeader(http.StatusUnauthorized)
		gw.writeResponse(w, r, jsonRPCError(nil, CodeAuthError, authErr))
		return
	}

	// Read request body (capped to prevent OOM)
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodySize))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Parse the JSON-RPC envelope to check the method
	var envelope jsonRPCMessage
	if unmarshalErr := json.Unmarshal(body, &envelope); unmarshalErr != nil {
		parseErr := ErrInvalidJSON.Parse(errors.WithError(unmarshalErr))
		w.WriteHeader(http.StatusBadRequest)
		gw.writeResponse(w, r, jsonRPCError(nil, CodeParseError, parseErr))
		return
	}

	// Handle subscriptions/listen - long-lived SSE stream
	if envelope.Method == "subscriptions/listen" {
		gw.handleSubscriptionsListen(w, r, envelope)
		return
	}

	// Handle MCP lifecycle methods locally (no backend spawn needed)
	if resp, handled := gw.handleLocally(envelope, body, backend); handled {
		gw.writeResponse(w, r, resp)
		return
	}

	// Forward to backend
	resp, err := gw.forwardToBackend(r.Context(), backend, envelope, body)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		gw.writeResponse(w, r, jsonRPCError(envelope.ID, CodeBackendError, err))
		return
	}

	gw.writeResponse(w, r, resp)
}

// handleRestart kills a backend so the next request re-spawns it fresh.
func (gw *Gateway) handleRestart(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/_restart/")
	name = strings.TrimSuffix(name, "/")
	gw.mu.RLock()
	b, ok := gw.backends[name]
	gw.mu.RUnlock()
	if !ok {
		http.Error(w, fmt.Sprintf("unknown backend: %s", name), http.StatusNotFound)
		return
	}
	wasRunning := b.isRunning()
	if wasRunning {
		b.kill()
		slog.Info("backend restarted via /_restart", "backend", name)
	}
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{"backend": name, "was_running": wasRunning, "status": "killed"}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Warn("restart response write failed", "error", err)
	}
}

// handleHealth responds with gateway and backend status.
func (gw *Gateway) handleHealth(w http.ResponseWriter) {
	gw.touchLastRequest()
	w.Header().Set("Content-Type", "application/json")
	type backendStatus struct {
		Running bool `json:"running"`
		IdleSec int  `json:"idle_seconds,omitempty"`
		Pid     int  `json:"pid,omitempty"`
	}
	statuses := make(map[string]backendStatus)
	gw.mu.RLock()
	for name, b := range gw.backends {
		b.mu.Lock()
		s := backendStatus{Running: b.running}
		if b.running {
			if !b.lastUsed.IsZero() {
				s.IdleSec = int(time.Since(b.lastUsed).Seconds())
			}
			if b.cmd != nil && b.cmd.Process != nil {
				s.Pid = b.cmd.Process.Pid
			}
		}
		b.mu.Unlock()
		statuses[name] = s
	}
	gw.mu.RUnlock()
	if err := json.NewEncoder(w).Encode(map[string]any{
		"status":   "ok",
		"version":  Version,
		"pid":      os.Getpid(),
		"backends": statuses,
	}); err != nil {
		slog.Error("health response write failed", "error", err)
	}
}

// forwardToBackend sends a request to the backend, retrying once on failure.
// Also handles tools/list caching and smart category activation.
func (gw *Gateway) forwardToBackend(ctx context.Context, backend *Backend, envelope jsonRPCMessage, body []byte) ([]byte, error) {
	if err := backend.ensureRunning(); err != nil {
		return nil, ErrBackendStartFailed.Parse(errors.WithError(err))
	}

	resp, err := backend.send(ctx, body)
	if err != nil {
		slog.Warn("send failed, restarting backend", "backend", backend.name, "error", err)
		backend.kill()
		if restartErr := backend.ensureRunning(); restartErr != nil {
			return nil, ErrBackendRestartFailed.Parse(errors.WithError(restartErr))
		}
		resp, err = backend.send(ctx, body)
		if err != nil {
			return nil, ErrBackendSendFailed.Parse(errors.WithError(err))
		}
	}

	// Cache tools/list responses
	if envelope.Method == "tools/list" {
		backend.mu.Lock()
		backend.toolsCache = json.RawMessage(resp)
		backend.mu.Unlock()
		backend.saveToolsCache()
		backend.buildCategories()
		gw.notifyToolsChanged()
		slog.Debug("tools/list cached and categories rebuilt", "backend", backend.name)
	}

	// Smart category activation on tools/call
	if envelope.Method == "tools/call" {
		var callBody struct {
			Params *struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if json.Unmarshal(body, &callBody) == nil && callBody.Params != nil && callBody.Params.Name != "" {
			backend.smartActivate(callBody.Params.Name)
		}
	}

	return resp, nil
}

// handleLocally responds to MCP lifecycle methods without spawning the backend.
// Returns the response bytes and true if handled, or nil/false to forward to backend.
func (gw *Gateway) handleLocally(env jsonRPCMessage, body []byte, b *Backend) ([]byte, bool) {
	switch env.Method {
	case "initialize":
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      env.ID,
			"result": map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities": map[string]any{
					"tools":     map[string]any{"listChanged": true},
					"resources": map[string]any{"listChanged": true},
				},
				"serverInfo": map[string]any{
					"name":    "mcp-gateway/" + b.name,
					"version": "1.0.0",
				},
			},
		}
		data, _ := json.Marshal(resp)
		slog.Debug("initialize handled locally", "backend", b.name)
		return data, true

	case "notifications/initialized":
		return []byte{}, true

	case "ping":
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      env.ID,
			"result":  map[string]any{},
		}
		data, _ := json.Marshal(resp)
		return data, true

	case "tools/list":
		return b.handleToolsList(env.ID)

	case "tools/call":
		return gw.handleDiscoverTools(env, body, b)

	default:
		return nil, false
	}
}

// handleToolsList serves tools/list from cache when possible.
// Returns nil, false if the request should be forwarded to the backend.
func (b *Backend) handleToolsList(id *json.RawMessage) ([]byte, bool) {
	b.mu.Lock()
	hasCats := len(b.categories) > 0
	cached := b.toolsCache
	b.mu.Unlock()

	if cached != nil && hasCats {
		data, err := b.filteredToolsList(id)
		if err == nil {
			slog.Debug("tools/list served filtered (discover mode)", "backend", b.name, "activeTools", len(b.activeTools))
			return data, true
		}
	}
	if cached != nil {
		var cachedResp map[string]any
		if err := json.Unmarshal(cached, &cachedResp); err == nil {
			cachedResp["id"] = id
			data, _ := json.Marshal(cachedResp)
			slog.Debug("tools/list served from cache (no discovery)", "backend", b.name)
			return data, true
		}
	}
	return nil, false
}

// handleDiscoverTools intercepts discover_<backend>_tools calls and responds from cache.
// Returns nil, false if this is not a discovery call and should be forwarded.
func (gw *Gateway) handleDiscoverTools(env jsonRPCMessage, body []byte, b *Backend) ([]byte, bool) {
	expectedName := fmt.Sprintf("discover_%s_tools", b.name)
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(body, &struct {
		Params *struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}{Params: &params}); err != nil || (params.Name != expectedName && params.Name != "discover_tools") {
		return nil, false
	}

	var args struct {
		Category string `json:"category"`
	}
	if params.Arguments != nil {
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			slog.Warn("discover_tools: failed to parse arguments", "backend", b.name, "error", err)
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if args.Category == "" {
		return b.listCategories(env.ID), true
	}

	return gw.activateAndRespond(env.ID, b, args.Category)
}

// listCategories returns a JSON-RPC response listing all tool categories.
// Must be called with b.mu held.
func (b *Backend) listCategories(id *json.RawMessage) []byte {
	catList := make([]map[string]any, 0, len(b.categories))
	for cat, tools := range b.categories {
		active := false
		for _, t := range tools {
			if b.activeTools[t] {
				active = true
				break
			}
		}
		catList = append(catList, map[string]any{
			"id":        cat,
			"toolCount": len(tools),
			"active":    active,
		})
	}
	sort.Slice(catList, func(i, j int) bool {
		return catList[i]["id"].(string) < catList[j]["id"].(string)
	})
	content, _ := json.MarshalIndent(map[string]any{"categories": catList}, "", "  ")
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": string(content)},
			},
		},
	}
	data, _ := json.Marshal(resp)
	slog.Debug("discover_tools: listed categories", "backend", b.name)
	return data
}

// activateAndRespond activates a category and returns the JSON-RPC response.
// Must be called with b.mu held.
func (gw *Gateway) activateAndRespond(id *json.RawMessage, b *Backend, category string) ([]byte, bool) {
	activated, err := b.activateCategory(category)
	if err != nil {
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"result": map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": err.Error()},
				},
				"isError": true,
			},
		}
		data, _ := json.Marshal(resp)
		return data, true
	}

	toolNames := make([]string, 0, len(activated))
	for _, t := range activated {
		if name, ok := t["name"].(string); ok {
			toolNames = append(toolNames, name)
		}
	}
	totalActive := len(b.activeTools)
	content, _ := json.MarshalIndent(map[string]any{
		"activated":  category,
		"addedTools": toolNames,
		"totalTools": totalActive,
	}, "", "  ")
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": string(content)},
			},
		},
	}
	data, _ := json.Marshal(resp)

	gw.notifyToolsChanged()
	slog.Info("discover_tools: category activated", "backend", b.name, "category", category, "tools", len(toolNames))
	return data, true
}

// writeResponse sends the response in the format the client expects (SSE or JSON).
func (gw *Gateway) writeResponse(w http.ResponseWriter, r *http.Request, resp []byte) {
	if len(resp) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "text/event-stream") {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", resp)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	} else {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(resp); err != nil {
			slog.Error("response write failed", "error", err)
		}
	}
}

// ensureRunning starts the subprocess if not already running, or connects to remote backend.
func (b *Backend) ensureRunning() error {
	b.mu.Lock()

	if b.running {
		b.mu.Unlock()
		return nil
	}

	// Remote backends - mark as running (stateless HTTP, always "ready")
	if b.isRemote() {
		b.running = true
		b.lastUsed = time.Now()
		b.mu.Unlock()
		b.logInfo("remote backend connected", "url", b.def.URL, "transport", b.transportType())
		return nil
	}

	if len(b.def.Command) == 0 {
		b.mu.Unlock()
		return ErrBackendNotConfigured.Parse(errors.WithParsedMessage(b.name))
	}

	if err := b.spawnProcess(); err != nil {
		b.mu.Unlock()
		return err
	}

	// Release lock before sending initialize (send() needs to acquire it)
	b.mu.Unlock()

	const maxInitRetries = 3
	var initErr error
	for attempt := 1; attempt <= maxInitRetries; attempt++ {
		initErr = b.initializeBackend()
		if initErr == nil {
			return nil
		}

		slog.Warn("backend init failed, will retry", "backend", b.name, "attempt", attempt, "max", maxInitRetries, "error", initErr)

		// Kill the broken process
		b.kill()

		if attempt == maxInitRetries {
			break
		}

		// Backoff: 1s, 2s
		time.Sleep(time.Duration(attempt) * time.Second)

		// Re-spawn for next attempt
		b.mu.Lock()
		if err := b.spawnProcess(); err != nil {
			b.mu.Unlock()
			return err
		}
		b.mu.Unlock()
	}

	return initErr
}

// spawnProcess starts the subprocess and sets up stdin/stdout pipes.
// Must be called with b.mu held. Does NOT release the lock.
func (b *Backend) spawnProcess() error {
	ctx, cancel := context.WithCancel(context.Background())
	b.cancelFn = cancel

	cmd := exec.CommandContext(ctx, b.def.Command[0], b.def.Command[1:]...) // #nosec G204 -- command from admin config, subprocess management is this tool's purpose

	cmd.Env = os.Environ()
	for k, v := range b.globalEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	for k, v := range b.def.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return ErrStdinPipe.Parse(errors.WithError(err))
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return ErrStdoutPipe.Parse(errors.WithError(err))
	}

	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		cancel()
		return ErrCmdStart.Parse(errors.WithError(err), errors.WithSafeData(map[string]any{"command": b.def.Command[0]}))
	}

	b.cmd = cmd
	b.stdin = stdin
	b.stdout = bufio.NewReaderSize(stdout, 1024*1024)
	b.pending = make(map[string]chan json.RawMessage)
	b.running = true

	slog.Info("backend started", "backend", b.name, "pid", cmd.Process.Pid)

	go b.readLoop()
	go b.waitForExit(cmd)
	return nil
}

// waitForExit waits for the subprocess to exit and cleans up pending requests.
func (b *Backend) waitForExit(cmd *exec.Cmd) {
	waitErr := cmd.Wait()
	b.mu.Lock()
	b.running = false
	for id, ch := range b.pending {
		close(ch)
		delete(b.pending, id)
	}
	b.mu.Unlock()
	if waitErr != nil {
		slog.Warn("backend process exited with error", "backend", b.name, "error", waitErr)
	} else {
		slog.Info("backend process exited", "backend", b.name)
	}
}

// initializeBackend sends the MCP initialize handshake to the subprocess.
// Returns an error if the backend fails to initialize or responds with a JSON-RPC error.
func (b *Backend) initializeBackend() error {
	initMsg := []byte(`{"jsonrpc":"2.0","id":"_gw_init","method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"mcp-gateway","version":"1.0.0"}}}`)
	resp, err := b.send(context.Background(), initMsg)
	if err != nil {
		slog.Error("backend initialize failed", "backend", b.name, "error", err)
		return ErrBackendInitFailed.Parse(errors.WithError(err))
	}

	// Check if the response contains a JSON-RPC error
	var rpcResp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(resp, &rpcResp) == nil && rpcResp.Error != nil {
		initErr := fmt.Errorf("%s", rpcResp.Error.Message)
		slog.Error("backend initialize returned error", "backend", b.name, "code", rpcResp.Error.Code, "message", rpcResp.Error.Message)
		return ErrBackendInitFailed.Parse(errors.WithError(initErr))
	}

	slog.Info("backend initialized", "backend", b.name, "response", truncate(resp, 100))

	// Send notifications/initialized (fire-and-forget)
	b.mu.Lock()
	if b.running {
		if _, err := b.stdin.Write([]byte("{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n")); err != nil {
			slog.Warn("failed to send notifications/initialized", "backend", b.name, "error", err)
		}
	}
	b.mu.Unlock()
	return nil
}

// send writes a JSON-RPC message to stdin and waits for the correlated response.
// For remote backends, it forwards via HTTP.
func (b *Backend) send(ctx context.Context, msg []byte) ([]byte, error) {
	if b.isRemote() {
		return b.sendRemote(ctx, msg)
	}

	// Touch last used timestamp
	b.mu.Lock()
	b.lastUsed = time.Now()
	b.mu.Unlock()

	// Extract the ID for correlation
	var envelope jsonRPCMessage
	if err := json.Unmarshal(msg, &envelope); err != nil {
		return nil, ErrParseMsgID.Parse(errors.WithError(err))
	}

	// For notifications (no id), just write and return empty
	if envelope.ID == nil {
		b.mu.Lock()
		if !b.running {
			b.mu.Unlock()
			return nil, ErrBackendNotRunning.Parse()
		}
		_, err := b.stdin.Write(append(msg, '\n'))
		b.mu.Unlock()
		return []byte("{}"), err
	}

	// Create response channel
	idKey := string(*envelope.ID)
	ch := make(chan json.RawMessage, 1)

	b.mu.Lock()
	if !b.running {
		b.mu.Unlock()
		return nil, ErrBackendNotRunning.Parse()
	}
	b.pending[idKey] = ch
	_, err := b.stdin.Write(append(msg, '\n'))
	b.mu.Unlock()

	if err != nil {
		b.mu.Lock()
		delete(b.pending, idKey)
		b.mu.Unlock()
		return nil, ErrWritePipe.Parse(errors.WithError(err))
	}

	// Wait for response with timeout or context cancellation
	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, ErrBackendClosed.Parse()
		}
		return resp, nil
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.pending, idKey)
		b.mu.Unlock()
		return nil, ErrBackendTimeout.Parse(errors.WithError(ctx.Err()))
	case <-time.After(120 * time.Second):
		b.mu.Lock()
		delete(b.pending, idKey)
		b.mu.Unlock()
		return nil, ErrBackendTimeout.Parse()
	}
}

// readLoop reads line-delimited JSON from the subprocess stdout and dispatches responses.
func (b *Backend) readLoop() {
	for {
		line, err := b.stdout.ReadBytes('\n')
		if err != nil {
			if err != io.EOF {
				slog.Error("backend read error", "backend", b.name, "error", err)
			}
			return
		}

		line = bytesTrimRight(line)
		if len(line) == 0 {
			continue
		}

		b.dispatchResponse(line)
	}
}

// dispatchResponse parses a JSON-RPC response line and delivers it to the pending caller.
func (b *Backend) dispatchResponse(line []byte) {
	var envelope jsonRPCMessage
	if err := json.Unmarshal(line, &envelope); err != nil {
		slog.Warn("invalid JSON from backend", "backend", b.name, "error", err)
		return
	}

	if envelope.ID == nil {
		return // notification from backend, drop
	}

	idKey := string(*envelope.ID)

	b.mu.Lock()
	ch, ok := b.pending[idKey]
	if ok {
		delete(b.pending, idKey)
	}
	b.mu.Unlock()

	if ok {
		ch <- json.RawMessage(line)
	} else {
		slog.Warn("unmatched response from backend", "backend", b.name, "id", idKey)
	}
}

// kill terminates the backend subprocess or disconnects from remote.
func (b *Backend) kill() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.isRemote() {
		b.running = false
		return
	}

	if b.cancelFn != nil {
		b.cancelFn()
	}
	b.running = false
}

// isRunning returns whether the backend subprocess is alive.
func (b *Backend) isRunning() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running
}

// shutdownAll kills all backend subprocesses.
func (gw *Gateway) shutdownAll() {
	gw.mu.RLock()
	defer gw.mu.RUnlock()

	for _, backend := range gw.backends {
		backend.kill()
	}
}

// idleReaper periodically checks backends and kills those that have been idle too long.
func (gw *Gateway) idleReaper(ctx context.Context, timeout time.Duration) {
	interval := timeout / 10
	if interval < 10*time.Second {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			gw.reapIdleBackends(timeout)
		}
	}
}

// reapIdleBackends kills any backends that have been idle longer than timeout.
func (gw *Gateway) reapIdleBackends(timeout time.Duration) {
	now := time.Now()
	gw.mu.RLock()
	defer gw.mu.RUnlock()

	for _, backend := range gw.backends {
		backend.mu.Lock()
		if backend.running && !backend.lastUsed.IsZero() && now.Sub(backend.lastUsed) > timeout {
			slog.Info("backend idle, killing", "backend", backend.name, "idle", now.Sub(backend.lastUsed).Round(time.Second))
			if backend.cancelFn != nil {
				backend.cancelFn()
			}
			backend.running = false
		}
		backend.mu.Unlock()
	}
}

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

// loadToolsCache reads the cached tools/list response from disk.
func (b *Backend) loadToolsCache() {
	if b.cacheDir == "" {
		return
	}
	path := filepath.Join(b.cacheDir, b.name+".json")
	data, err := os.ReadFile(path) // #nosec G304 -- path from cacheDir config + backend name, not user request input
	if err != nil {
		return // no cache file, that's fine
	}
	b.toolsCache = json.RawMessage(data)
	slog.Debug("tools cache loaded from disk", "backend", b.name, "path", path)
}

// saveToolsCache writes the tools/list response to disk for persistence across restarts.
func (b *Backend) saveToolsCache() {
	if b.cacheDir == "" || b.toolsCache == nil {
		return
	}
	if err := os.MkdirAll(b.cacheDir, 0750); err != nil {
		slog.Warn("failed to create cache directory", "backend", b.name, "error", err)
		return
	}
	path := filepath.Join(b.cacheDir, b.name+".json")
	if err := os.WriteFile(path, b.toolsCache, 0600); err != nil {
		slog.Warn("failed to write tools cache", "backend", b.name, "error", err)
	} else {
		slog.Debug("tools cache saved to disk", "backend", b.name, "path", path)
	}
}

// logInfo logs at info level if logging is enabled for this backend.
func (b *Backend) logInfo(msg string, args ...any) {
	if b.logEnabled {
		slog.Info(msg, append([]any{"backend", b.name}, args...)...)
	}
}

// logDebug logs at debug level if logging is enabled for this backend.
func (b *Backend) logDebug(msg string, args ...any) {
	if b.logEnabled {
		slog.Debug(msg, append([]any{"backend", b.name}, args...)...)
	}
}

// isRemote returns true if this backend connects to a remote URL rather than spawning a subprocess.
func (b *Backend) isRemote() bool {
	return b.def.URL != ""
}

// toolAllowed returns true if the tool name passes the include/exclude filters.
// Logic: if include_tools is set, tool must match at least one include pattern.
// Then, if exclude_tools is set, tool must NOT match any exclude pattern.
func (b *Backend) toolAllowed(name string) bool {
	if len(b.def.IncludeTools) > 0 {
		matched := false
		for _, pattern := range b.def.IncludeTools {
			if matchGlob(pattern, name) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for _, pattern := range b.def.ExcludeTools {
		if matchGlob(pattern, name) {
			return false
		}
	}
	return true
}

// matchGlob performs a simple glob match supporting * as wildcard.
func matchGlob(pattern, name string) bool {
	if pattern == "*" {
		return true
	}
	// Use filepath.Match for standard glob semantics
	matched, err := filepath.Match(pattern, name)
	if err != nil {
		return false
	}
	return matched
}

// transportType returns the effective transport type for a remote backend.
// Defaults to "sse" if not explicitly set.
func (b *Backend) transportType() string {
	if b.def.TransportType != "" {
		return b.def.TransportType
	}
	// Default to streamable-http if URL ends with /mcp, otherwise sse
	if strings.HasSuffix(b.def.URL, "/mcp") {
		return "streamable-http"
	}
	return "sse"
}

// sendRemote forwards a JSON-RPC message to a remote backend via HTTP.
func (b *Backend) sendRemote(ctx context.Context, msg []byte) ([]byte, error) {
	b.mu.Lock()
	b.lastUsed = time.Now()
	b.mu.Unlock()

	switch b.transportType() {
	case "streamable-http":
		return b.sendStreamableHTTP(ctx, msg)
	case "sse":
		return b.sendSSE(ctx, msg)
	default:
		return nil, ErrUnknownTransport.Parse(errors.WithParsedMessage(b.transportType()))
	}
}

// sendStreamableHTTP sends a JSON-RPC message via HTTP POST and reads the response.
// Supports both plain JSON responses and SSE-wrapped responses.
func (b *Backend) sendStreamableHTTP(ctx context.Context, msg []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.def.URL, strings.NewReader(string(msg)))
	if err != nil {
		return nil, ErrRemoteRequest.Parse(errors.WithError(err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range b.def.Headers {
		req.Header.Set(k, v)
	}

	b.logDebug("sending to remote", "method", extractMethod(msg), "url", b.def.URL)

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, ErrRemoteRequest.Parse(errors.WithError(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
		return nil, ErrRemoteStatus.Parse(errors.WithParsedMessage(resp.StatusCode), errors.WithSafeData(map[string]any{"status": resp.StatusCode}), errors.WithAdditionalData(map[string]any{"body": string(body)}))
	}

	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		// Parse SSE response - extract the last "data:" line
		return b.parseSSEResponse(resp.Body)
	}

	// Plain JSON response
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
	if err != nil {
		return nil, ErrRemoteRead.Parse(errors.WithError(err))
	}

	b.logDebug("remote response received", "size", len(data))
	return data, nil
}

// sendSSE sends a JSON-RPC message to an SSE endpoint. For SSE transport,
// messages are POSTed to the base URL and the response comes back either
// inline or via the SSE stream.
func (b *Backend) sendSSE(ctx context.Context, msg []byte) ([]byte, error) {
	// SSE endpoints typically accept POST at the same URL for sending messages
	postURL := b.def.URL
	// If URL ends with /sse, some servers expect POST to a different endpoint
	// Convention: POST to the same URL, response comes back as SSE or JSON
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, postURL, strings.NewReader(string(msg)))
	if err != nil {
		return nil, ErrRemoteRequest.Parse(errors.WithError(err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range b.def.Headers {
		req.Header.Set(k, v)
	}

	b.logDebug("sending to SSE remote", "method", extractMethod(msg), "url", postURL)

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, ErrRemoteRequest.Parse(errors.WithError(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
		return nil, ErrRemoteStatus.Parse(errors.WithParsedMessage(resp.StatusCode), errors.WithSafeData(map[string]any{"status": resp.StatusCode}), errors.WithAdditionalData(map[string]any{"body": string(body)}))
	}

	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		return b.parseSSEResponse(resp.Body)
	}

	// Some SSE servers return JSON directly for request-response patterns
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
	if err != nil {
		return nil, ErrRemoteRead.Parse(errors.WithError(err))
	}

	b.logDebug("SSE response received", "size", len(data))
	return data, nil
}

// parseSSEResponse reads an SSE stream and returns the first complete JSON-RPC response.
func (b *Backend) parseSSEResponse(body io.Reader) ([]byte, error) {
	scanner := bufio.NewScanner(body)
	var dataLines []string

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
		} else if line == "" && len(dataLines) > 0 {
			// Empty line = end of event
			data := strings.Join(dataLines, "\n")
			dataLines = nil

			// Check if this is a JSON-RPC response (has "id" field)
			var envelope jsonRPCMessage
			if json.Unmarshal([]byte(data), &envelope) == nil && envelope.ID != nil {
				b.logDebug("SSE response parsed", "size", len(data))
				return []byte(data), nil
			}
			// Otherwise it might be a notification, keep reading
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, ErrSSEStreamRead.Parse(errors.WithError(err))
	}

	// If we got data lines but no empty line terminator (stream ended)
	if len(dataLines) > 0 {
		data := strings.Join(dataLines, "\n")
		return []byte(data), nil
	}

	return nil, ErrSSENoResponse.Parse()
}

// extractMethod pulls the method field from a JSON-RPC message for logging.
func extractMethod(msg []byte) string {
	var env struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(msg, &env) == nil {
		return env.Method
	}
	return "unknown"
}

// buildCategories parses the cached tools and groups them into categories.
// For backends with a native discover_tools, it extracts the categories from its description.
// For others, it auto-categorizes by action verb prefix (get_, create_, list_, search_, etc.).
// Discovery is only enabled for backends with more than 5 tools (unless overridden by config).
func (b *Backend) buildCategories() {
	if b.toolsCache == nil {
		return
	}

	if b.def.Discovery != nil && !*b.def.Discovery {
		b.categories = nil
		return
	}

	tools := b.parseToolsFromCache()
	if tools == nil {
		return
	}

	forceDiscovery := b.def.Discovery != nil && *b.def.Discovery
	if len(tools) <= 5 && !forceDiscovery {
		b.categories = nil
		return
	}

	b.categories = make(map[string][]string)
	b.hasDiscovery = false

	if len(b.def.Categories) > 0 {
		b.categories = b.def.Categories
		b.hasDiscovery = b.toolsContainDiscovery(tools)
		slog.Debug("categories from config", "backend", b.name, "categories", len(b.categories))
		return
	}

	for _, tool := range tools {
		if tool.Name == "discover_tools" {
			b.hasDiscovery = true
			continue
		}
		if !b.toolAllowed(tool.Name) {
			continue
		}
		category := categorizeToolName(tool.Name)
		b.categories[category] = append(b.categories[category], tool.Name)
	}

	slog.Debug("categories built", "backend", b.name, "categories", len(b.categories), "hasNativeDiscovery", b.hasDiscovery)
}

// toolEntry is a minimal tool descriptor for category building.
type toolEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// parseToolsFromCache extracts the tools list from the cached tools/list response.
func (b *Backend) parseToolsFromCache() []toolEntry {
	var resp struct {
		Result struct {
			Tools []toolEntry `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b.toolsCache, &resp); err != nil {
		return nil
	}
	return resp.Result.Tools
}

// toolsContainDiscovery checks if the tools list contains a native discover_tools tool.
func (b *Backend) toolsContainDiscovery(tools []toolEntry) bool {
	for _, tool := range tools {
		if tool.Name == "discover_tools" {
			return true
		}
	}
	return false
}

// categorizeToolName extracts a category from a tool name based on its action verb prefix.
func categorizeToolName(name string) string {
	parts := strings.Split(name, "_")

	// Skip known namespace prefixes (jira_, confluence_, discourse_, devops_, mdp_, iam_)
	namespaces := map[string]bool{
		"jira": true, "confluence": true, "discourse": true,
		"devops": true, "mdp": true, "iam": true,
	}
	start := 0
	if len(parts) > 1 && namespaces[parts[0]] {
		start = 1
	}

	if start >= len(parts) {
		return "general"
	}

	// Categorize by action verb
	verb := parts[start]
	switch verb {
	case "get", "read", "download", "view":
		return "read"
	case "list", "search", "filter", "find":
		return "search"
	case "create", "add", "batch":
		return "create"
	case "update", "edit", "transition", "link", "approve", "merge":
		return "update"
	case "delete", "remove", "unapprove", "cancel":
		return "delete"
	default:
		return "general"
	}
}

// discoverToolSchema returns the JSON schema for the discover_tools meta-tool for this backend.
func (b *Backend) discoverToolSchema() map[string]any {
	categories := make([]string, 0, len(b.categories))
	for cat := range b.categories {
		categories = append(categories, cat)
	}
	sort.Strings(categories)

	desc := fmt.Sprintf(
		"Discover and activate tool categories for %s. Available categories: %s. Call with a category to activate those tools.",
		b.name, strings.Join(categories, ", "),
	)

	// Use backend-specific name to avoid collisions when multiple backends have discovery
	toolName := fmt.Sprintf("discover_%s_tools", b.name)

	return map[string]any{
		"name":        toolName,
		"description": desc,
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"category": map[string]any{
					"type":        "string",
					"description": fmt.Sprintf("Category to activate. Options: %s. Omit to list all categories.", strings.Join(categories, ", ")),
				},
			},
		},
	}
}

// activateCategory adds all tools from a category to the active set.
// Returns the list of tool schemas that were activated.
func (b *Backend) activateCategory(category string) ([]map[string]any, error) {
	toolNames, ok := b.categories[category]
	if !ok {
		available := make([]string, 0, len(b.categories))
		for cat := range b.categories {
			available = append(available, cat)
		}
		sort.Strings(available)
		return nil, ErrUnknownCategory.Parse(errors.WithParsedMessage(category), errors.WithSafeData(map[string]any{"available": strings.Join(available, ", ")}))
	}

	// Get full tool schemas from cache
	var resp struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b.toolsCache, &resp); err != nil {
		return nil, err
	}

	// Build name->schema index
	toolIndex := make(map[string]json.RawMessage)
	for _, raw := range resp.Result.Tools {
		var t struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &t) == nil {
			toolIndex[t.Name] = raw
		}
	}

	// Activate and collect schemas
	activated := make([]map[string]any, 0, len(toolNames))
	for _, name := range toolNames {
		b.activeTools[name] = true
		if raw, ok := toolIndex[name]; ok {
			var schema map[string]any
			if json.Unmarshal(raw, &schema) == nil {
				activated = append(activated, schema)
			}
		}
	}

	slog.Info("category activated", "backend", b.name, "category", category, "tools", len(toolNames))
	return activated, nil
}

// smartActivate finds the category containing the given tool name and activates
// the entire category. This ensures related tools are available when one tool
// from a category is called directly (e.g. by the agent retrying a cached name).
func (b *Backend) smartActivate(toolName string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.categories == nil || b.activeTools[toolName] {
		return
	}

	for category, tools := range b.categories {
		for _, name := range tools {
			if name == toolName {
				// Activate all tools in this category
				for _, t := range tools {
					b.activeTools[t] = true
				}
				slog.Info("smart-activated category", "backend", b.name, "category", category, "trigger", toolName)
				return
			}
		}
	}
}

// filteredToolsList returns the tools/list response with only discover_tools + active tools.
func (b *Backend) filteredToolsList(id *json.RawMessage) ([]byte, error) {
	if b.toolsCache == nil {
		return nil, ErrNoToolsCache.Parse()
	}

	// Parse full cache
	var resp struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b.toolsCache, &resp); err != nil {
		return nil, err
	}

	// Build filtered list: discover_tools meta-tool + active tools
	filtered := make([]json.RawMessage, 0)

	// Add discover_tools meta-tool
	discoverSchema := b.discoverToolSchema()
	discoverJSON, _ := json.Marshal(discoverSchema)
	filtered = append(filtered, discoverJSON)

	// Add active tools (with optional description truncation)
	for _, raw := range resp.Result.Tools {
		var t struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &t) == nil && b.activeTools[t.Name] && b.toolAllowed(t.Name) {
			filtered = append(filtered, b.truncateDescription(raw))
		}
	}

	result := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]any{
			"tools": filtered,
		},
	}
	return json.Marshal(result)
}

// truncateDescription shortens the description field of a tool schema if maxDescLen is set.
func (b *Backend) truncateDescription(raw json.RawMessage) json.RawMessage {
	if b.maxDescLen <= 0 {
		return raw
	}
	var tool map[string]any
	if err := json.Unmarshal(raw, &tool); err != nil {
		return raw
	}
	if desc, ok := tool["description"].(string); ok && len(desc) > b.maxDescLen {
		tool["description"] = desc[:b.maxDescLen] + "..."
		if modified, err := json.Marshal(tool); err == nil {
			return modified
		}
	}
	return raw
}
