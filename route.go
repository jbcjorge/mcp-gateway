package main

import (
	"encoding/json"
	"sync"

	errors "github.com/jbcjorge/errors-library"
)

// Route is a client-facing endpoint (/<name>/mcp) backed by one or more member
// backends. A single-server route has one member; a composition has several.
// The gateway resolves requests to a Route; members remain plain *Backend with
// independent lifecycle.
//
// A KEYED route is a special single-server case: it has no eager member.
// Instead it spawns one child *Backend per instance id (derived from per-call
// params) on demand, pooling and reusing them. Children are registered in
// gw.backends under "<route>/<id>" so the existing reaper/TTL/health/shutdown
// treat each child independently. See docs/design/keyed-backends.md.
type Route struct {
	name    string
	prefix  string
	members []*Backend // ordered; later members win on tool-name collision

	mu          sync.Mutex
	toolOwners  map[string]itemOwner // display tool name -> (member name, real name)
	mergedTools json.RawMessage      // cached merged+prefixed tools/list result object

	// Keyed-route state (nil/empty for normal and composite routes).
	keyed      []Param             // declared routing params; non-empty => keyed route
	keyedDef   BackendDef          // server template used to build each child
	children   map[string]*Backend // instance id -> child backend
	spawning   map[string]chan struct{}
	buildChild func(id string, env map[string]string) *Backend // build (not start) a child
	register   func(name string, b *Backend)                   // register child in gw.backends
	unregister func(name string)                               // remove child from gw.backends
}

// newRoute builds a Route from ordered member backends.
func newRoute(name, prefix string, members []*Backend) *Route {
	return &Route{name: name, prefix: prefix, members: members}
}

// newKeyedRoute builds a keyed Route with no eager members. The gateway injects
// buildChild (constructs but does not start a child bound to the instance env),
// register/unregister (add/remove the child in gw.backends so lifecycle applies).
func newKeyedRoute(
	name string,
	def BackendDef,
	buildChild func(id string, env map[string]string) *Backend,
	register func(name string, b *Backend),
	unregister func(name string),
) *Route {
	return &Route{
		name:       name,
		keyed:      def.Params,
		keyedDef:   def,
		children:   make(map[string]*Backend),
		spawning:   make(map[string]chan struct{}),
		buildChild: buildChild,
		register:   register,
		unregister: unregister,
	}
}

// isKeyed reports whether this route spawns children per instance id.
func (rt *Route) isKeyed() bool { return len(rt.keyed) > 0 }

// resolveChild returns the child backend for the given instance id, building and
// registering it on first use. env holds the param values to inject into the
// child's environment (keyed by each param's child env name). A per-id spawn
// guard ensures concurrent calls for a new id build exactly one child.
func (rt *Route) resolveChild(id string, env map[string]string) *Backend {
	for {
		rt.mu.Lock()
		if b, ok := rt.children[id]; ok {
			rt.mu.Unlock()
			return b
		}
		if ch, inFlight := rt.spawning[id]; inFlight {
			// Another goroutine is building this id; wait and retry.
			rt.mu.Unlock()
			<-ch
			continue
		}
		// Claim the build.
		done := make(chan struct{})
		rt.spawning[id] = done
		rt.mu.Unlock()

		b := rt.buildChild(id, env)
		name := childInstanceName(rt.name, id)
		if rt.register != nil {
			rt.register(name, b)
		}

		rt.mu.Lock()
		rt.children[id] = b
		delete(rt.spawning, id)
		close(done)
		rt.mu.Unlock()
		return b
	}
}

// dropChild removes a child from the pool and from gw.backends. Called when a
// keyed child process exits (reap, crash, TTL recycle) so a later call spawns a
// fresh one. id is derived from the composite name "<route>/<id>".
func (rt *Route) dropChild(id string) {
	rt.mu.Lock()
	_, ok := rt.children[id]
	delete(rt.children, id)
	rt.mu.Unlock()
	if ok && rt.unregister != nil {
		rt.unregister(childInstanceName(rt.name, id))
	}
}

// keyedMembers returns a snapshot of the current child backends (for health and
// shutdown iteration if needed).
func (rt *Route) keyedMembers() []*Backend {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]*Backend, 0, len(rt.children))
	for _, b := range rt.children {
		out = append(out, b)
	}
	return out
}

// backendByName returns the member backend with the given name.
func (rt *Route) backendByName(name string) *Backend {
	for _, m := range rt.members {
		if m.name == name {
			return m
		}
	}
	return nil
}

// isComposite reports whether this route has more than one member.
func (rt *Route) isComposite() bool { return len(rt.members) > 1 }

// toolsList returns the merged (and prefixed) tools/list result object for the
// route, fetching each member's list via getList and caching the result and the
// ownership map. getList lets tests inject member results; in production it is
// backed by the member's live/cached tools/list.
func (rt *Route) toolsList(getList func(*Backend) (json.RawMessage, error)) (json.RawMessage, error) {
	lists := make([]memberList, 0, len(rt.members))
	for _, m := range rt.members {
		res, err := getList(m)
		if err != nil {
			return nil, err
		}
		lists = append(lists, memberList{member: m.name, result: res})
	}
	merged, owners, err := mergeLists(lists, "tools", "name", rt.prefix)
	if err != nil {
		return nil, err
	}
	rt.mu.Lock()
	rt.toolOwners = owners
	rt.mergedTools = merged
	rt.mu.Unlock()
	return merged, nil
}

// ownerOfTool returns the member backend and real tool name for a (possibly
// prefixed) display tool name, using the cached ownership map.
func (rt *Route) ownerOfTool(displayName string) (*Backend, string, bool) {
	rt.mu.Lock()
	owners := rt.toolOwners
	rt.mu.Unlock()
	if owners == nil {
		return nil, "", false
	}
	o, ok := owners[displayName]
	if !ok {
		return nil, "", false
	}
	m := rt.backendByName(o.member)
	if m == nil {
		return nil, "", false
	}
	return m, o.realName, true
}

// rewriteToolCallName returns a copy of a tools/call body with params.name
// rewritten to realName (stripping the route prefix). Used before forwarding to
// the owning member, which only knows its real tool names.
func rewriteToolCallName(body []byte, realName string) ([]byte, error) {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, ErrMergeParse.Parse(errors.WithError(err))
	}
	var params map[string]json.RawMessage
	if len(msg["params"]) > 0 {
		if err := json.Unmarshal(msg["params"], &params); err != nil {
			return nil, ErrMergeParse.Parse(errors.WithError(err))
		}
	} else {
		params = map[string]json.RawMessage{}
	}
	nameJSON, _ := json.Marshal(realName)
	params["name"] = nameJSON
	newParams, _ := json.Marshal(params)
	msg["params"] = newParams
	return json.Marshal(msg)
}

// primary returns the first member (used for non-list/get methods).
func (rt *Route) primary() *Backend { return rt.members[0] }

// allBackends returns every live backend owned by the route: its ordered
// members plus any spawned keyed children. Used by hot-reload to stop a route's
// whole process set on removal.
func (rt *Route) allBackends() []*Backend {
	out := make([]*Backend, 0, len(rt.members)+len(rt.children))
	out = append(out, rt.members...)
	rt.mu.Lock()
	for _, b := range rt.children {
		out = append(out, b)
	}
	rt.mu.Unlock()
	return out
}

// callToolName extracts params.name from a tools/call body.
func callToolName(body []byte) string {
	var b struct {
		Params *struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &b) == nil && b.Params != nil {
		return b.Params.Name
	}
	return ""
}
