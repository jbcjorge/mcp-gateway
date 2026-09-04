package main

import (
	"bytes"
	"encoding/json"

	errors "github.com/jbcjorge/errors-library"
)

// CompositionConfig is the backends configuration (new schema, pre-1.0, no
// backward compatibility with the old flat map).
//
//   - servers: reusable backend templates (definitions only; inert until used).
//   - compositions: named, ordered lists of members (definitions only).
//   - backends: the routes actually exposed at /<name>/mcp. Each entry names a
//     server or a composition and may carry an optional prefix.
type CompositionConfig struct {
	Servers      map[string]BackendDef     `json:"servers"`
	Compositions map[string]CompositionDef `json:"compositions"`
	Backends     []RouteEntry              `json:"backends"`
}

// CompositionDef is an ordered list of members forming a route's contents.
type CompositionDef struct {
	Members []MemberDef `json:"members"`
}

// MemberDef references a server template and may add its own tool filters on top
// of the server's own include/exclude.
type MemberDef struct {
	Server       string   `json:"server"`
	IncludeTools []string `json:"include_tools"`
	ExcludeTools []string `json:"exclude_tools"`
}

// RouteEntry is a backends[] entry: either a bare string (route name, no prefix)
// or an object {"route": "...", "prefix": "..."}.
type RouteEntry struct {
	Route  string `json:"route"`
	Prefix string `json:"prefix"`
}

// UnmarshalJSON accepts either a JSON string or an object for a route entry.
func (re *RouteEntry) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		re.Route = s
		re.Prefix = ""
		return nil
	}
	// Object form: use an alias to avoid recursing into this method.
	type routeEntryAlias RouteEntry
	var a routeEntryAlias
	if err := json.Unmarshal(trimmed, &a); err != nil {
		return err
	}
	*re = RouteEntry(a)
	return nil
}

// ResolvedMember is a member of a resolved route: the server template to
// instantiate plus the effective (merged) tool filters.
type ResolvedMember struct {
	ServerName   string
	Def          BackendDef
	IncludeTools []string
	ExcludeTools []string
}

// ResolvedRoute is a route (backends[] entry) resolved to its ordered members.
// A plain-server route has exactly one member; a composition has N.
type ResolvedRoute struct {
	Name    string
	Prefix  string
	Members []ResolvedMember
}

// resolveRoutes resolves the routes declared in cfg.Backends, transitively
// resolving only the compositions/servers they reference. Unreferenced
// definitions are ignored. Dangling references are a fatal error.
func resolveRoutes(cfg *CompositionConfig) ([]ResolvedRoute, error) {
	var routes []ResolvedRoute

	for _, entry := range cfg.Backends {
		name := entry.Route
		if comp, ok := cfg.Compositions[name]; ok {
			members, err := resolveMembers(cfg, name, comp.Members)
			if err != nil {
				return nil, err
			}
			routes = append(routes, ResolvedRoute{Name: name, Prefix: entry.Prefix, Members: members})
			continue
		}
		if def, ok := cfg.Servers[name]; ok {
			routes = append(routes, ResolvedRoute{
				Name:   name,
				Prefix: entry.Prefix,
				Members: []ResolvedMember{{
					ServerName:   name,
					Def:          def,
					IncludeTools: def.IncludeTools,
					ExcludeTools: def.ExcludeTools,
				}},
			})
			continue
		}
		return nil, ErrRouteUnknown.Parse(errors.WithParsedMessage(name))
	}
	return routes, nil
}

// resolveMembers resolves a composition's members to server templates, merging
// the member's tool filters over the server's own.
func resolveMembers(cfg *CompositionConfig, compName string, members []MemberDef) ([]ResolvedMember, error) {
	out := make([]ResolvedMember, 0, len(members))
	for _, m := range members {
		def, ok := cfg.Servers[m.Server]
		if !ok {
			return nil, ErrMemberServerUnknown.Parse(errors.WithParsedMessage(compName + " -> " + m.Server))
		}
		out = append(out, ResolvedMember{
			ServerName:   m.Server,
			Def:          def,
			IncludeTools: mergeFilters(def.IncludeTools, m.IncludeTools),
			ExcludeTools: mergeFilters(def.ExcludeTools, m.ExcludeTools),
		})
	}
	return out, nil
}

// mergeFilters combines a server's own filters with a member's overriding
// filters. A non-empty member filter takes precedence; otherwise the server's
// filter is used.
func mergeFilters(serverFilter, memberFilter []string) []string {
	if len(memberFilter) > 0 {
		return memberFilter
	}
	return serverFilter
}
