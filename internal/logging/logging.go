// Package logging configures the gateway's structured logger (log/slog) and
// provides a size-rotating file writer.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	errors "github.com/jbcjorge/errors-library"
)

// debugEnvVar, when set to any non-empty value, forces DEBUG level regardless of
// the configured level.
const debugEnvVar = "MCP_GATEWAY_DEBUG"

// level is the runtime-adjustable log level for the gateway.
var level = new(slog.LevelVar)

// Options configures Setup.
type Options struct {
	Level     string // debug, info, warn, error (default: info)
	File      string // path to the log file; empty logs to stderr
	MaxSizeMB int    // max log file size in MB before rotation (default: 10)
	MaxFiles  int    // number of rotated files to keep (default: 3)
	BaseDir   string // relative File paths are resolved against this directory
}

// Setup configures the default slog logger from opts. The MCP_GATEWAY_DEBUG
// environment variable overrides the configured level.
func Setup(opts Options) error {
	level.Set(ParseLevel(opts.Level))
	if os.Getenv(debugEnvVar) != "" {
		level.Set(slog.LevelDebug)
	}
	var out io.Writer = os.Stderr
	if opts.File != "" {
		logPath := opts.File
		if !filepath.IsAbs(logPath) {
			logPath = filepath.Join(opts.BaseDir, logPath)
		}
		rw, err := newRotatingWriter(logPath, opts.MaxSizeMB, opts.MaxFiles)
		if err != nil {
			return ErrLogFileOpen.Parse(errors.WithParsedMessage(logPath), errors.WithError(err))
		}
		out = rw
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level})))
	return nil
}

// ParseLevel converts a level name to a slog.Level. Defaults to info for empty
// or unrecognized values.
func ParseLevel(s string) slog.Level {
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
