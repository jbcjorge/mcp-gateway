//go:build !darwin

package listener

import "net"

// Listen binds the configured address. Launchd socket activation is
// only available on macOS; on other platforms we always bind directly.
func Listen(addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}
