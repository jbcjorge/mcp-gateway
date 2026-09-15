//go:build windows

package main

// runStubbornHelper is a no-op on Windows; the group-signal escalation tests are
// unix-only (see procgroup_unix_test.go).
func runStubbornHelper() {}
