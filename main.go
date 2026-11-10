// Command mcp-gateway is a local HTTP multiplexer for MCP stdio servers. It
// accepts MCP Streamable HTTP requests, routes them by path to subprocess
// backends, and handles lifecycle (lazy spawn, idle reaping, and
// self-termination for launchd socket activation).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jbcjorge/mcp-gateway/internal/cabundle"
	"github.com/jbcjorge/mcp-gateway/internal/config"
	"github.com/jbcjorge/mcp-gateway/internal/gateway"
	"github.com/jbcjorge/mcp-gateway/internal/listener"
	"github.com/jbcjorge/mcp-gateway/internal/logging"
	"github.com/jbcjorge/mcp-gateway/internal/version"
)

// Exit codes. Use these constants for every os.Exit call.
const (
	exitOK             = 0 // clean exit
	exitFailure        = 1 // generic startup/runtime failure
	exitCABundleFailed = 3 // required CA bundle missing/unreadable (require_bundle=true)
)

// HTTP server timeouts. The write timeout is a default ceiling; long-running
// tools/call requests extend their own write deadline (see the gateway package).
const (
	serverReadTimeout  = 30 * time.Second
	serverWriteTimeout = 120 * time.Second
)

func main() {
	// Handle --version / -v flag
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("mcp-gateway %s (%s) built %s\n", version.Version, version.Commit, version.BuildDate)
		os.Exit(exitOK)
	}

	configPath := resolveConfigPath()

	cfg, err := config.Load(configPath)
	if err != nil {
		slog.Error("failed to load config", "path", configPath, "error", err)
		os.Exit(exitFailure)
	}

	// Configure logging
	if logErr := logging.Setup(logging.Options{
		Level:     cfg.LogLevel,
		File:      cfg.LogFile,
		MaxSizeMB: cfg.LogMaxSizeMB,
		MaxFiles:  cfg.LogMaxFiles,
		BaseDir:   filepath.Dir(configPath),
	}); logErr != nil {
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
	cfg.Env, err = cabundle.Apply(cfg.CABundle, cfg.Env)
	if err != nil {
		slog.Error("CA bundle required but unavailable", "error", err)
		os.Exit(exitCABundleFailed)
	}

	gw, err := gateway.New(cfg)
	if err != nil {
		slog.Error("failed to build gateway from config", "error", err)
		os.Exit(exitFailure)
	}
	gw.SetConfigPath(configPath)

	mux := http.NewServeMux()
	mux.Handle("/", gw)

	server := &http.Server{
		Handler:      mux,
		ReadTimeout:  serverReadTimeout,
		WriteTimeout: serverWriteTimeout,
	}

	// Get listener BEFORE setting up signal handlers.
	// The launchd XPC syscall (launch_activate_socket) must happen before
	// Go's signal handling is initialized to avoid signal_recv crashes.
	ln, err := listener.Listen(cfg.Listen)
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
		gw.ShutdownAll()
	}()

	gw.StartBackgroundWorkers(ctx, stop)

	slog.Info("mcp-gateway started", "version", version.Version, "commit", version.Commit, "addr", ln.Addr().String(), "pid", os.Getpid())
	if err := server.Serve(ln); err != http.ErrServerClosed {
		slog.Error("server error", "error", err)
		os.Exit(exitFailure)
	}
}
