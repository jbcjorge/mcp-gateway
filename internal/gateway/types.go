package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/jbcjorge/mcp-gateway/internal/config"
)

// Size limits for request and response bodies.
const (
	maxRequestBodySize  = 5 * 1024 * 1024  // 5MB - JSON-RPC requests are small
	maxResponseBodySize = 50 * 1024 * 1024 // 50MB - tool responses can be large
	maxErrorBodySize    = 4096             // 4KB - error bodies for logging
)

// Backend manages a single stdio MCP subprocess or remote connection.
type Backend struct {
	name      string
	def       config.BackendDef
	globalEnv map[string]string
	cacheDir  string // directory for persistent cache files
	cacheName string // tool-cache file basename override (empty = use name); keyed children share the route's cache

	mu             sync.Mutex
	cmd            *exec.Cmd
	stdin          io.WriteCloser
	stdout         *bufio.Reader
	pending        map[string]chan json.RawMessage // id string -> response channel
	running        bool
	cancelFn       context.CancelFunc
	stopGrace      time.Duration   // grace between SIGTERM and SIGKILL when stopping this backend's process group
	idleTimeout    time.Duration   // per-backend idle timeout (resolved): 0 = inherit global, neverReapIdle = never reap, >0 = reap after this duration
	exited         chan struct{}   // closed by waitForExit when the process exits; enables graceful-stop detection
	lastUsed       time.Time       // last time a request was forwarded
	reapNotBefore  time.Time       // idle reaper must not reap before this time (set from a tools/call's declared timeout); zero = no extension
	toolsCache     json.RawMessage // cached tools/list response (nil = no cache)
	resourcesCache json.RawMessage // cached resources/list result object (nil = serve empty without spawning)
	promptsCache   json.RawMessage // cached prompts/list result object (nil = serve empty without spawning)
	toolsCachePid  int             // pid of the backend that produced the cache
	listCachePid   int             // pid that produced the resources/prompts caches

	// Credential TTL watcher state. A backend may emit a stderr sentinel
	// "[[gateway:credential_expiry]] <unix-epoch>" after spawn, declaring when
	// its injected credential (e.g. an SSO session cookie) expires. The gateway
	// then recycles the backend before that time so the next spawn fetches a
	// fresh credential. Guarded by b.mu.
	credentialExpiry time.Time     // zero = no credential expiry declared
	ttlSoftMargin    time.Duration // recycle (defer-if-busy) at expiry-softMargin
	ttlHardGuard     time.Duration // force-recycle (even if busy) at expiry-hardGuard
	softTimer        *time.Timer   // fires at the soft deadline
	hardTimer        *time.Timer   // fires at the hard deadline
	pendingTTLKill   bool          // soft deadline reached while busy; recycle when idle

	// Remote backend state
	httpClient *http.Client                    // shared HTTP client for remote backends
	ssePending map[string]chan json.RawMessage // SSE response correlation

	// Tool discovery state
	categories   map[string][]string // category name -> tool names
	activeTools  map[string]bool     // set of activated tool names
	hasDiscovery bool                // true if backend natively has discover_tools
	maxDescLen   int                 // max description length (0 = no truncation)
	logEnabled   bool                // whether to log request/response activity for this backend
}

// jsonRPCMessage is a minimal JSON-RPC envelope for id extraction.
type jsonRPCMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
}

// Gateway holds all backends and routes requests.
type Gateway struct {
	config      config.Config
	configPath  string              // path to the main config file (for hot-reload of backends)
	backends    map[string]*Backend // all member instances (keyed by instance name) for lifecycle
	routes      map[string]*Route   // client-facing routes (keyed by route name)
	routeSigs   map[string]string   // route name -> topology signature (for hot-reload diff)
	oauth       *oauthStore         // OAuth-broker code/token store
	authorizer  Authorizer
	mu          sync.RWMutex
	lastRequest time.Time // last time any request was received
	reqMu       sync.Mutex

	// SSE listeners for subscriptions/listen
	listenersMu sync.Mutex
	listeners   []chan string // channels to push notifications to
}

// credentialExpirySentinel is the stderr marker a backend emits to declare when
// its injected credential expires: "[[gateway:credential_expiry]] <unix-epoch>".
const credentialExpirySentinel = "[[gateway:credential_expiry]]"

// bytesTrimRight trims trailing whitespace characters from a byte slice.
func bytesTrimRight(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}

// truncate returns a string truncated to maxLen characters.
func truncate(b []byte, maxLen int) string {
	if len(b) <= maxLen {
		return string(b)
	}
	return string(b[:maxLen]) + "..."
}

// toolEntry is a minimal tool descriptor for category building.
type toolEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}
