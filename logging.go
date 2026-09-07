package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	errors "github.com/jbcjorge/errors-library"
)

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
