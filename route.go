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
type Route struct {
	name    string
	prefix  string
	members []*Backend // ordered; later members win on tool-name collision

	mu          sync.Mutex
	toolOwners  map[string]itemOwner // display tool name -> (member name, real name)
	mergedTools json.RawMessage      // cached merged+prefixed tools/list result object
}

// newRoute builds a Route from ordered member backends.
func newRoute(name, prefix string, members []*Backend) *Route {
	return &Route{name: name, prefix: prefix, members: members}
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
