//go:build !darwin

package main

import "net"

// getListener binds the configured address. Launchd socket activation is
// only available on macOS; on other platforms we always bind directly.
func getListener(addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}
