package main

import (
	"encoding/json"

	errors "github.com/jbcjorge/errors-library"
)

// memberList pairs a member name with its raw list result payload (the "result"
// object of a tools/list, resources/list, or prompts/list response).
type memberList struct {
	member string
	result json.RawMessage
}

// itemOwner records which member owns a merged item and its real (unprefixed)
// name/uri, so get/call can be routed and de-prefixed.
type itemOwner struct {
	member   string
	realName string
}

// mergeLists merges the array under result[listKey] across members (in order),
// keyed by itemKey ("name" for tools/prompts, "uri" for resources). On key
// collision the LAST member wins (override). If prefix is non-empty, every
// surviving item's key is rewritten to prefix+key for advertisement, and the
// ownership map is keyed by that display name mapping back to (member, realName).
//
// Returns the merged result object ({listKey: [...]}) and the ownership map.
func mergeLists(members []memberList, listKey, itemKey, prefix string) (json.RawMessage, map[string]itemOwner, error) {
	order := []string{}
	items := map[string]map[string]any{} // realName -> item object
	owners := map[string]itemOwner{}

	for _, ml := range members {
		if err := ingestMember(ml, listKey, itemKey, &order, items, owners); err != nil {
			return nil, nil, err
		}
	}

	out, finalOwners := finalizeMerged(order, items, owners, itemKey, prefix)
	merged, err := json.Marshal(map[string]any{listKey: out})
	if err != nil {
		return nil, nil, ErrMergeParse.Parse(errors.WithError(err))
	}
	return merged, finalOwners, nil
}

// ingestMember parses one member's list result and folds its items into the
// accumulating order/items/owners (last-wins on collision).
func ingestMember(ml memberList, listKey, itemKey string, order *[]string, items map[string]map[string]any, owners map[string]itemOwner) error {
	if len(ml.result) == 0 {
		return nil
	}
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(ml.result, &wrapper); err != nil {
		return ErrMergeParse.Parse(errors.WithError(err))
	}
	rawList, ok := wrapper[listKey]
	if !ok {
		return nil // member exposes nothing for this family
	}
	var arr []map[string]any
	if err := json.Unmarshal(rawList, &arr); err != nil {
		return ErrMergeParse.Parse(errors.WithError(err))
	}
	for _, it := range arr {
		key, _ := it[itemKey].(string)
		if key == "" {
			continue
		}
		if _, seen := items[key]; !seen {
			*order = append(*order, key)
		}
		items[key] = it // last-wins
		owners[key] = itemOwner{member: ml.member, realName: key}
	}
	return nil
}

// finalizeMerged builds the ordered output array, applying the optional prefix
// and producing the display-name-keyed ownership map.
func finalizeMerged(order []string, items map[string]map[string]any, owners map[string]itemOwner, itemKey, prefix string) ([]map[string]any, map[string]itemOwner) {
	finalOwners := map[string]itemOwner{}
	out := make([]map[string]any, 0, len(order))
	for _, key := range order {
		it := items[key]
		display := key
		if prefix != "" {
			display = prefix + key
			clone := make(map[string]any, len(it))
			for k, v := range it {
				clone[k] = v
			}
			clone[itemKey] = display
			it = clone
		}
		out = append(out, it)
		finalOwners[display] = owners[key]
	}
	return out, finalOwners
}
