package main

import (
	"context"
	"encoding/json"
	"log/slog"

	errors "github.com/jbcjorge/errors-library"
)

// dispatchKeyed handles a request for a keyed route. Lifecycle methods are
// answered locally; tools/list reuses the shared route cache (with params
// injected); tools/call routes to a per-instance child; any other method is
// routed to the targeted child (requiring the routing params) so that
// instance-scoped non-tool methods still work.
func (gw *Gateway) dispatchKeyed(ctx context.Context, route *Route, envelope jsonRPCMessage, body []byte) ([]byte, error) {
	switch envelope.Method {
	case "initialize":
		return localInitializeResponse(envelope.ID, route.name), nil
	case "notifications/initialized":
		return []byte{}, nil
	case "ping":
		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": envelope.ID, "result": map[string]any{}})
		return data, nil
	case "tools/list":
		return gw.keyedToolsList(route, envelope.ID)
	case "tools/call":
		return gw.keyedToolsCall(ctx, route, envelope, body)
	default:
		// Non-tool methods are instance-scoped: resolve the child from the
		// routing params and forward.
		return gw.keyedForwardOther(ctx, route, envelope, body)
	}
}

// keyedToolsList serves the route's shared tools cache with the keyed params
// injected into each tool's input schema. On a cold cache (no instance ever
// spawned yet) it returns an empty tool list; the first tools/call warms the
// cache. This mirrors the gateway's normal cold-cache behavior.
func (gw *Gateway) keyedToolsList(route *Route, id *json.RawMessage) ([]byte, error) {
	cached := gw.sharedKeyedCache(route)
	if cached == nil {
		// Cold: no instance discovered yet. Return an empty tool list.
		reply, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"result":  map[string]any{"tools": []any{}},
		})
		return reply, nil
	}

	// cached is a full JSON-RPC response; extract its result, inject params,
	// re-wrap with the caller's id.
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(cached, &env); err != nil || env.Result == nil {
		reply, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"result":  map[string]any{"tools": []any{}},
		})
		return reply, nil
	}
	injected := injectParamsIntoTools(env.Result, route.keyed)
	reply, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  json.RawMessage(injected),
	})
	return reply, nil
}

// sharedKeyedCache returns the tools/list cache shared by the route's children.
// It prefers an in-memory cache from any live child, falling back to the
// on-disk route cache loaded by a freshly built (not yet started) probe child.
func (gw *Gateway) sharedKeyedCache(route *Route) json.RawMessage {
	for _, child := range route.keyedMembers() {
		child.mu.Lock()
		c := child.toolsCache
		child.mu.Unlock()
		if c != nil {
			return c
		}
	}
	// No live child with a cache; try the on-disk route cache via a probe.
	probe := route.buildChild("_probe", map[string]string{})
	probe.loadToolsCache()
	probe.mu.Lock()
	c := probe.toolsCache
	probe.mu.Unlock()
	return c
}

// keyedToolsCall extracts and validates the routing params, resolves (spawning
// if needed) the per-instance child, strips the params from the arguments, and
// forwards the call to that child via the normal forwarding path.
func (gw *Gateway) keyedToolsCall(ctx context.Context, route *Route, envelope jsonRPCMessage, body []byte) ([]byte, error) {
	args, err := callArguments(body)
	if err != nil {
		return nil, err
	}
	values, byName, err := extractParamValues(route.keyed, args)
	if err != nil {
		return nil, err
	}
	id := instanceID(values)

	// Build the child env from each param's child env name.
	env := make(map[string]string, len(route.keyed))
	for _, p := range route.keyed {
		if v, ok := byName[p.Name]; ok {
			env[p.childEnvName()] = v
		}
	}

	child := route.resolveChild(id, env)

	stripped, err := stripParamsFromCall(body, route.keyed)
	if err != nil {
		return nil, err
	}

	slog.Debug("keyed tools/call routed", "route", route.name, "instance", id, "params", values)
	return gw.forwardToBackend(ctx, child, envelope, stripped)
}

// keyedForwardOther resolves the child from the routing params for a non-tool,
// non-lifecycle method and forwards the (param-stripped) request to it. Such
// methods must carry the routing params in a top-level "params" object field
// matching the declared param names.
func (gw *Gateway) keyedForwardOther(ctx context.Context, route *Route, envelope jsonRPCMessage, body []byte) ([]byte, error) {
	var env struct {
		Params map[string]json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, ErrKeyedStripParse.Parse(errors.WithError(err))
	}
	values, byName, err := extractParamValues(route.keyed, env.Params)
	if err != nil {
		return nil, err
	}
	id := instanceID(values)
	childEnv := make(map[string]string, len(route.keyed))
	for _, p := range route.keyed {
		if v, ok := byName[p.Name]; ok {
			childEnv[p.childEnvName()] = v
		}
	}
	child := route.resolveChild(id, childEnv)
	return gw.forwardToBackend(ctx, child, envelope, body)
}
