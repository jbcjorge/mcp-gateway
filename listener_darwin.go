//go:build darwin

package main

import (
	"log/slog"
	"net"

	launchd "github.com/bored-engineer/go-launchd"
)

// getListener returns a net.Listener, preferring a launchd-activated socket.
// On macOS, if the binary was launched by launchd with socket activation,
// launch_activate_socket("mcp") returns the pre-bound listener.
// Otherwise falls back to binding the configured address directly.
func getListener(addr string) (net.Listener, error) {
	// Try launchd socket activation (macOS)
	ln, err := launchd.Activate("mcp")
	if err == nil {
		slog.Info("using launchd socket activation")
		return ln, nil
	}

	// Fall back to binding ourselves
	return net.Listen("tcp", addr)
}
