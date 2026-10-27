package main

import (
	"encoding/json"
	"time"
)

// Reap-extension tuning. A long-blocking tools/call (e.g. GitLab
// wait_for_pipeline) keeps a backend occupied without sending new requests, so
// the idle reaper would otherwise kill the backend mid-call once lastUsed goes
// stale. We derive a per-call reap extension from the call's own declared
// timeout so the backend survives the wait, while a genuinely hung backend is
// still reaped once the derived deadline passes.
const (
	// reapExtensionCap bounds the derived extension so an absurd caller-supplied
	// timeout cannot pin a backend alive indefinitely.
	reapExtensionCap = 30 * time.Minute
	// reapExtensionFloor is the minimum extension applied when a call declares a
	// timeout, covering tiny or zero declared values.
	reapExtensionFloor = 1 * time.Minute

	// defaultResponseTimeout bounds how long the gateway waits for a backend's
	// JSON-RPC response when the call declares no timeout. Long-blocking calls
	// (wait_for_job/wait_for_pipeline) declare one and get responseWait() below.
	defaultResponseTimeout = 120 * time.Second
	// responseHeadroom is added above a call's declared timeout so the backend's
	// own timeout fires first and returns a proper response, rather than the
	// gateway cutting the request off at exactly the declared value.
	responseHeadroom = 30 * time.Second
	// maxResponseTimeout caps the derived response wait so an absurd declared
	// timeout cannot block a gateway worker indefinitely.
	maxResponseTimeout = reapExtensionCap + responseHeadroom
	// writeDeadlineBuffer is added to the response wait when extending the HTTP
	// write deadline, so the server WriteTimeout outlives backend.send's own
	// deadline (send fails first with a proper JSON-RPC error, not a dropped conn).
	writeDeadlineBuffer = 15 * time.Second
)

// responseWait returns how long the gateway should wait for a backend response
// to a tools/call, given the call's declared timeout (0 when none was found).
// With no declared timeout it is defaultResponseTimeout; otherwise it is the
// declared timeout plus headroom, capped at maxResponseTimeout.
func responseWait(declared time.Duration) time.Duration {
	if declared <= 0 {
		return defaultResponseTimeout
	}
	w := declared + responseHeadroom
	if w > maxResponseTimeout {
		return maxResponseTimeout
	}
	return w
}

// timeoutKeysSeconds are tools/call argument keys whose numeric value is a
// timeout expressed in SECONDS. Matched in order; the largest value wins.
var timeoutKeysSeconds = []string{
	"timeout_seconds",
	"timeout",
	"wait_seconds",
	"max_wait_seconds",
	"deadline_seconds",
}

// timeoutKeysMillis are tools/call argument keys whose numeric value is a
// timeout expressed in MILLISECONDS.
var timeoutKeysMillis = []string{
	"timeout_ms",
	"timeoutMs",
}

// parseCallTimeout best-effort extracts a declared timeout from a tools/call's
// decoded arguments. It only considers an explicit allowlist of numeric keys
// (never string values, so a timeout-named string being written is ignored),
// normalizes milliseconds to a duration, and returns the largest match. The
// matched key is returned for logging. ok is false when no timeout key is found.
func parseCallTimeout(args map[string]json.RawMessage) (d time.Duration, key string, ok bool) {
	for _, k := range timeoutKeysSeconds {
		if v, found := numericArg(args, k); found {
			if cand := time.Duration(v * float64(time.Second)); cand > d {
				d, key, ok = cand, k, true
			}
		}
	}
	for _, k := range timeoutKeysMillis {
		if v, found := numericArg(args, k); found {
			if cand := time.Duration(v * float64(time.Millisecond)); cand > d {
				d, key, ok = cand, k, true
			}
		}
	}
	return d, key, ok
}

// numericArg returns the float value of a JSON number argument, or ok=false
// when the key is absent or not a JSON number.
func numericArg(args map[string]json.RawMessage, key string) (float64, bool) {
	raw, ok := args[key]
	if !ok {
		return 0, false
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	return n, true
}

// extendedReapDeadline computes how far into the future the idle reaper must be
// held off for a call that declared the given timeout. It is the declared
// timeout plus the backend's effective idle timeout (so the normal idle grace
// still applies after the call could have returned), clamped to
// [reapExtensionFloor+idle, reapExtensionCap+idle].
func extendedReapDeadline(now time.Time, declared, effectiveIdle time.Duration) time.Time {
	ext := declared
	if ext < reapExtensionFloor {
		ext = reapExtensionFloor
	}
	if ext > reapExtensionCap {
		ext = reapExtensionCap
	}
	return now.Add(ext + effectiveIdle)
}
