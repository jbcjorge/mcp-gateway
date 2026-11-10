// Package listener creates the gateway's network listener. On macOS it prefers a
// launchd-activated socket (socket activation); everywhere else, and as a
// fallback, it binds the configured TCP address directly.
package listener
