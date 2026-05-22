// Package config implements dotted-path get/set over state/config.yaml.
//
// We keep the YAML as a generic map[string]any tree so the CLI can
// manipulate keys the design doc has not yet enumerated (e.g.
// schedule.active_window.timezone) without us having to bump a typed
// struct every time the doc grows. Validation happens in callers if
// they care.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Tree is a recursive map representing parsed config YAML.
type Tree map[string]any

// Load reads cfgPath and returns the parsed tree. A missing file is an
// error — callers should ensure `harness init` ran first.
func Load(cfgPath string) (Tree, error) {
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	t := Tree{}
	if err := yaml.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return t, nil
}

// Save writes the tree back to cfgPath with stable formatting.
func Save(cfgPath string, t Tree) error {
	out, err := yaml.Marshal(t)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(cfgPath, out, 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// Get returns the leaf value at the dotted key, or os.ErrNotExist if any
// path segment is missing.
//
// Booleans/ints come back as their YAML-native Go types (bool, int,
// float64, string). Sub-trees come back as map[string]any.
func Get(t Tree, dotted string) (any, error) {
	parts := splitKey(dotted)
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty key")
	}
	var cur any = t
	for i, p := range parts {
		m, ok := asMap(cur)
		if !ok {
			return nil, fmt.Errorf("key %s: parent %q is not a map", dotted, strings.Join(parts[:i], "."))
		}
		v, ok := m[p]
		if !ok {
			return nil, os.ErrNotExist
		}
		cur = v
	}
	return cur, nil
}

// asMap normalizes the various map shapes that yaml.v3 emits
// (map[string]any, named Tree, map[interface{}]interface{}) into a
// single map[string]any view, returning ok=false if the value is not a
// map.
func asMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case Tree:
		return map[string]any(m), true
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, vv := range m {
			ks, ok := k.(string)
			if !ok {
				return nil, false
			}
			out[ks] = vv
		}
		return out, true
	}
	return nil, false
}

// Set writes value into the tree at the dotted key, creating
// intermediate maps as needed.
//
// value is coerced from its string form into bool / int / float / string
// using YAML's own scalar parsing — so "true", "42", "3.14", and plain
// strings all DWIM the way `harness config set` users would expect.
func Set(t Tree, dotted, raw string) error {
	parts := splitKey(dotted)
	if len(parts) == 0 {
		return fmt.Errorf("empty key")
	}
	val, err := parseScalar(raw)
	if err != nil {
		return err
	}
	m := map[string]any(t)
	for i, p := range parts {
		if i == len(parts)-1 {
			m[p] = val
			return nil
		}
		next, exists := m[p]
		if !exists {
			nm := map[string]any{}
			m[p] = nm
			m = nm
			continue
		}
		nm, ok := asMap(next)
		if !ok {
			return fmt.Errorf("cannot descend into non-map at %q", strings.Join(parts[:i+1], "."))
		}
		// Re-bind the parent slot to the normalized map[string]any view
		// so subsequent reads (and YAML marshal) see a uniform shape.
		m[p] = nm
		m = nm
	}
	return nil
}

func splitKey(dotted string) []string {
	if dotted == "" {
		return nil
	}
	return strings.Split(dotted, ".")
}

// parseScalar runs the input through yaml.Unmarshal so we get the same
// scalar typing rules the rest of the config file uses.
func parseScalar(raw string) (any, error) {
	var v any
	if err := yaml.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("parse value: %w", err)
	}
	return v, nil
}

// Format renders v for human-friendly CLI output. Maps round-trip
// through YAML; scalars come back as their bare form.
func Format(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case bool:
		if x {
			return "true", nil
		}
		return "false", nil
	case int:
		return fmt.Sprintf("%d", x), nil
	case int64:
		return fmt.Sprintf("%d", x), nil
	case float64:
		return fmt.Sprintf("%g", x), nil
	case nil:
		return "null", nil
	default:
		b, err := yaml.Marshal(v)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\n"), nil
	}
}
