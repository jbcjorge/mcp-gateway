//go:build windows

package gateway

// runStubbornHelper is a no-op on Windows; the group-signal escalation tests are
// unix-only (see procgroup_unix_test.go).
func runStubbornHelper() {}
