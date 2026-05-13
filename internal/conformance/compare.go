// Package conformance provides semantic comparison utilities for fixture- and
// diff-based validation of extension outputs across runtimes.
//
// Rules applied by Compare:
//   - Registration lists are compared ignoring ordering, keyed by a
//     caller-specified identity field (e.g., command "name", shortcut "key_id").
//   - All other arrays (e.g., hostcall logs) are compared with order preserved.
//   - Objects are compared ignoring key ordering.
//   - Missing vs null is treated as equivalent.
//   - Missing vs empty array ([]) is treated as equivalent.
//   - Floats are compared with epsilon 1e-10 by default.
package conformance

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// DefaultEpsilon is the maximum absolute difference allowed between two floats
// when FloatEpsilon is not set in CompareOptions.
const DefaultEpsilon = 1e-10

// CompareOptions controls the semantic comparison rules.
type CompareOptions struct {
	// RegistrationLists maps dot-separated JSON paths (rooted at "$") to the
	// identity field used to key each entry.  Arrays at those paths are
	// compared as unordered sets; all other arrays are compared in order.
	//
	// Example:
	//   "$.commands"  → "name"
	//   "$.shortcuts" → "key_id"
	RegistrationLists map[string]string

	// FloatEpsilon is the maximum absolute difference allowed between two
	// floats.  Defaults to DefaultEpsilon (1e-10) when zero.
	FloatEpsilon float64
}

// Compare semantically compares got and want (JSON bytes) using the rules
// described in the package doc.  Returns nil on semantic equality; otherwise
// returns a human-readable diff report with the number of differences found.
func Compare(got, want []byte, opts CompareOptions) error {
	if opts.FloatEpsilon == 0 {
		opts.FloatEpsilon = DefaultEpsilon
	}
	var gotVal, wantVal any
	if err := json.Unmarshal(got, &gotVal); err != nil {
		return fmt.Errorf("conformance: parse got: %w", err)
	}
	if err := json.Unmarshal(want, &wantVal); err != nil {
		return fmt.Errorf("conformance: parse want: %w", err)
	}
	var diffs []string
	cmpVal("$", gotVal, wantVal, opts, &diffs)
	if len(diffs) == 0 {
		return nil
	}
	return fmt.Errorf("conformance mismatch (%d difference(s)):\n%s",
		len(diffs), strings.Join(diffs, "\n"))
}

// cmpVal recursively compares got and want at path, appending human-readable
// difference strings to diffs.
func cmpVal(path string, got, want any, opts CompareOptions, diffs *[]string) {
	// null / missing / empty-array equivalence.
	gotEmpty := isEmpty(got)
	wantEmpty := isEmpty(want)
	if gotEmpty && wantEmpty {
		return
	}
	if wantEmpty {
		// want is absent/null/[] — flag only if got carries a real value.
		if !gotEmpty {
			*diffs = append(*diffs, fmt.Sprintf("  %s: unexpected value in got: %v", path, got))
		}
		return
	}
	if gotEmpty {
		*diffs = append(*diffs, fmt.Sprintf("  %s: missing in got (want %v)", path, want))
		return
	}

	// Both sides are non-empty.
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			*diffs = append(*diffs, fmt.Sprintf("  %s: type mismatch: got %T, want object", path, got))
			return
		}
		cmpObject(path, g, w, opts, diffs)

	case []any:
		g, ok := got.([]any)
		if !ok {
			*diffs = append(*diffs, fmt.Sprintf("  %s: type mismatch: got %T, want array", path, got))
			return
		}
		if idField, isReg := opts.RegistrationLists[path]; isReg {
			cmpRegistrationList(path, g, w, idField, opts, diffs)
		} else {
			cmpOrderedArray(path, g, w, opts, diffs)
		}

	case float64:
		gf, ok := asFloat(got)
		if !ok {
			*diffs = append(*diffs, fmt.Sprintf("  %s: type mismatch: got %T, want number", path, got))
			return
		}
		diff := math.Abs(gf - w)
		if diff > opts.FloatEpsilon {
			*diffs = append(*diffs, fmt.Sprintf(
				"  %s: float mismatch: got %g, want %g (Δ=%g, ε=%g)",
				path, gf, w, diff, opts.FloatEpsilon))
		}

	case bool:
		gb, ok := got.(bool)
		if !ok {
			*diffs = append(*diffs, fmt.Sprintf("  %s: type mismatch: got %T, want bool", path, got))
			return
		}
		if gb != w {
			*diffs = append(*diffs, fmt.Sprintf("  %s: bool mismatch: got %v, want %v", path, gb, w))
		}

	case string:
		gs, ok := got.(string)
		if !ok {
			*diffs = append(*diffs, fmt.Sprintf("  %s: type mismatch: got %T, want string", path, got))
			return
		}
		if gs != w {
			*diffs = append(*diffs, fmt.Sprintf(
				"  %s: string mismatch:\n    got:  %q\n    want: %q", path, gs, w))
		}

	default:
		if got != want {
			*diffs = append(*diffs, fmt.Sprintf("  %s: mismatch: got %v, want %v", path, got, want))
		}
	}
}

func cmpObject(path string, got, want map[string]any, opts CompareOptions, diffs *[]string) {
	for k, wv := range want {
		cmpVal(path+"."+k, got[k], wv, opts, diffs)
	}
	// Extra keys in got that want doesn't have (and that carry a real value).
	for k, gv := range got {
		if _, ok := want[k]; !ok && !isEmpty(gv) {
			*diffs = append(*diffs, fmt.Sprintf("  %s.%s: unexpected key in got", path, k))
		}
	}
}

// cmpOrderedArray compares two arrays element-by-element (order matters).
// Used for hostcall logs and other sequenced output.
func cmpOrderedArray(path string, got, want []any, opts CompareOptions, diffs *[]string) {
	if len(got) != len(want) {
		*diffs = append(*diffs, fmt.Sprintf(
			"  %s: array length mismatch: got %d, want %d", path, len(got), len(want)))
	}
	n := min(len(got), len(want))
	for i := range n {
		cmpVal(fmt.Sprintf("%s[%d]", path, i), got[i], want[i], opts, diffs)
	}
}

// cmpRegistrationList compares two arrays as unordered sets, keyed by idField.
// Entries missing from either side are reported individually.
func cmpRegistrationList(path string, got, want []any, idField string, opts CompareOptions, diffs *[]string) {
	wantByID := indexByField(want, idField, path, diffs)
	gotByID := indexByField(got, idField, path, diffs)

	// Deterministic diff order.
	allIDs := make(map[string]bool, len(wantByID)+len(gotByID))
	for id := range wantByID {
		allIDs[id] = true
	}
	for id := range gotByID {
		allIDs[id] = true
	}
	ids := make([]string, 0, len(allIDs))
	for id := range allIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		wv, wok := wantByID[id]
		gv, gok := gotByID[id]
		entryPath := fmt.Sprintf("%s[%s=%q]", path, idField, id)
		switch {
		case wok && gok:
			cmpVal(entryPath, gv, wv, opts, diffs)
		case !gok:
			*diffs = append(*diffs, fmt.Sprintf("  %s: missing in got", entryPath))
		default:
			*diffs = append(*diffs, fmt.Sprintf("  %s: unexpected entry in got", entryPath))
		}
	}
}

func indexByField(arr []any, field, path string, diffs *[]string) map[string]any {
	m := make(map[string]any, len(arr))
	for i, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			*diffs = append(*diffs, fmt.Sprintf("  %s[%d]: not an object, cannot index by %q", path, i, field))
			continue
		}
		id, ok := obj[field].(string)
		if !ok || id == "" {
			*diffs = append(*diffs, fmt.Sprintf("  %s[%d]: missing or non-string field %q — item is unindexable and will not be compared", path, i, field))
			continue
		}
		m[id] = item
	}
	return m
}

// isEmpty reports whether v should be treated as "not present": JSON null
// (Go nil) or an empty array.  Empty objects are NOT considered absent.
func isEmpty(v any) bool {
	if v == nil {
		return true
	}
	if arr, ok := v.([]any); ok && len(arr) == 0 {
		return true
	}
	return false
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}
