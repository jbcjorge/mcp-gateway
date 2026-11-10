// Package procgroup places backend subprocesses in their own process group and
// tears the whole group down (SIGTERM, then SIGKILL after a grace window) so no
// descendant, such as a browser owned by a backend, is ever orphaned. On Windows,
// which has no POSIX process groups, teardown is best-effort on the single
// process.
package procgroup
