// Package version holds build-time metadata. The variables are stamped by the
// Makefile and Dockerfile via -ldflags -X, e.g.
//
//	-X github.com/jbcjorge/mcp-gateway/internal/version.Version=v1.2.3
package version

// Build-time variables injected via -ldflags.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)
