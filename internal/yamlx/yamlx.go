// Package yamlx walks YAML documents strictly: typed getters, unknown-key
// detection and dotted error paths such as faults[0].type.
package yamlx

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type Error struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (e Error) String() string {
	if e.Path == "" {
		return e.Message
	}
	return e.Path + ": " + e.Message
}

type Errors []Error

func (e *Errors) Add(path, format string, args ...any) {
	*e = append(*e, Error{Path: path, Message: fmt.Sprintf(format, args...)})
}

// Map is a YAML mapping being consumed key by key.
type Map struct {
	path string
	errs *Errors
	keys []string
	vals map[string]*yaml.Node
	used map[string]bool
}

// Parse returns the document root, or nil when the document is not a mapping.
func Parse(data []byte, errs *Errors) *Map {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		errs.Add("", "invalid YAML: %v", err)
		return nil
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		errs.Add("", "document is empty")
		return nil
	}
	before := len(*errs)
	rejectIndirection(doc.Content[0], "", errs)
	if len(*errs) > before {
		return nil
	}
	return newMap(doc.Content[0], "", errs)
}

// HasIndirection reports whether a tree uses aliases or merge keys.
func HasIndirection(n *yaml.Node) bool {
	var errs Errors
	rejectIndirection(n, "", &errs)
	return len(errs) > 0
}

// Aliases and merge keys are refused outright: they let one value appear under
// another key after decoding, which redaction of the stored copy could miss.
func rejectIndirection(n *yaml.Node, path string, errs *Errors) {
	switch n.Kind {
	case yaml.AliasNode:
		errs.Add(path, "YAML aliases are not supported")
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Kind != yaml.ScalarNode {
				errs.Add(path, "mapping keys must be plain scalars")
				continue
			}
			if k.ShortTag() == "!!merge" || k.Value == "<<" {
				errs.Add(join(path, k.Value), "YAML merge keys are not supported")
				continue
			}
			rejectIndirection(v, join(path, k.Value), errs)
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			rejectIndirection(c, fmt.Sprintf("%s[%d]", path, i), errs)
		}
	}
}

func newMap(n *yaml.Node, path string, errs *Errors) *Map {
	if n.Kind != yaml.MappingNode {
		errs.Add(path, "must be a mapping")
		return nil
	}
	m := &Map{path: path, errs: errs, vals: map[string]*yaml.Node{}, used: map[string]bool{}}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i].Value
		if _, dup := m.vals[k]; dup {
			errs.Add(join(path, k), "duplicate key")
			continue
		}
		m.keys = append(m.keys, k)
		m.vals[k] = n.Content[i+1]
	}
	return m
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func (m *Map) Path(key string) string { return join(m.path, key) }

func (m *Map) Has(key string) bool {
	n, ok := m.vals[key]
	return ok && n.ShortTag() != "!!null"
}

func (m *Map) get(key string) *yaml.Node {
	m.used[key] = true
	n, ok := m.vals[key]
	if !ok || n.ShortTag() == "!!null" {
		return nil
	}
	return n
}

func (m *Map) Require(key string) bool {
	if !m.Has(key) {
		m.used[key] = true
		m.errs.Add(m.Path(key), "is required")
		return false
	}
	return true
}

func (m *Map) Str(key string) (string, bool) {
	n := m.get(key)
	if n == nil {
		return "", false
	}
	if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!str" {
		m.errs.Add(m.Path(key), "must be a string")
		return "", false
	}
	return n.Value, true
}

func (m *Map) Int(key string) (int64, bool) {
	n := m.get(key)
	if n == nil {
		return 0, false
	}
	if n.Kind == yaml.ScalarNode && n.ShortTag() == "!!int" {
		if v, err := strconv.ParseInt(n.Value, 0, 64); err == nil {
			return v, true
		}
	}
	m.errs.Add(m.Path(key), "must be an integer")
	return 0, false
}

func (m *Map) Float(key string) (float64, bool) {
	n := m.get(key)
	if n == nil {
		return 0, false
	}
	if n.Kind == yaml.ScalarNode && (n.ShortTag() == "!!int" || n.ShortTag() == "!!float") {
		var v float64
		if err := n.Decode(&v); err == nil {
			return v, true
		}
	}
	m.errs.Add(m.Path(key), "must be a number")
	return 0, false
}

func (m *Map) Bool(key string) (bool, bool) {
	n := m.get(key)
	if n == nil {
		return false, false
	}
	if n.Kind == yaml.ScalarNode && n.ShortTag() == "!!bool" {
		var v bool
		if err := n.Decode(&v); err == nil {
			return v, true
		}
	}
	m.errs.Add(m.Path(key), "must be true or false")
	return false, false
}

func (m *Map) Dur(key string) (time.Duration, bool) {
	n := m.get(key)
	if n == nil {
		return 0, false
	}
	if n.Kind == yaml.ScalarNode {
		if d, err := time.ParseDuration(n.Value); err == nil && d >= 0 {
			return d, true
		}
	}
	m.errs.Add(m.Path(key), "must be a duration such as 30s")
	return 0, false
}

func (m *Map) Map(key string) *Map {
	n := m.get(key)
	if n == nil {
		return nil
	}
	return newMap(n, m.Path(key), m.errs)
}

// Item is one element of a sequence.
type Item struct {
	Path string
	node *yaml.Node
	errs *Errors
}

func (it Item) Map() *Map { return newMap(it.node, it.Path, it.errs) }

func (it Item) Str() (string, bool) {
	if it.node.Kind != yaml.ScalarNode || it.node.ShortTag() != "!!str" {
		it.errs.Add(it.Path, "must be a string")
		return "", false
	}
	return it.node.Value, true
}

func (m *Map) Seq(key string) []Item {
	n := m.get(key)
	if n == nil {
		return nil
	}
	if n.Kind != yaml.SequenceNode {
		m.errs.Add(m.Path(key), "must be a list")
		return nil
	}
	items := make([]Item, len(n.Content))
	for i, c := range n.Content {
		items[i] = Item{Path: fmt.Sprintf("%s[%d]", m.Path(key), i), node: c, errs: m.errs}
	}
	return items
}

// StrMap reads a mapping of scalar values, such as HTTP headers.
func (m *Map) StrMap(key string) map[string]string {
	n := m.get(key)
	if n == nil {
		return nil
	}
	if n.Kind != yaml.MappingNode {
		m.errs.Add(m.Path(key), "must be a mapping")
		return nil
	}
	out := map[string]string{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if v.Kind != yaml.ScalarNode {
			m.errs.Add(m.Path(key)+"."+k.Value, "must be a scalar")
			continue
		}
		out[k.Value] = v.Value
	}
	return out
}

// Any decodes a subtree into plain Go values.
func (m *Map) Any(key string) (map[string]any, bool) {
	n := m.get(key)
	if n == nil {
		return nil, false
	}
	if n.Kind != yaml.MappingNode {
		m.errs.Add(m.Path(key), "must be a mapping")
		return nil, false
	}
	out := map[string]any{}
	if err := n.Decode(&out); err != nil {
		m.errs.Add(m.Path(key), "invalid value: %v", err)
		return nil, false
	}
	return out, true
}

// Done reports every key that no getter asked for.
func (m *Map) Done() {
	var unknown []string
	for _, k := range m.keys {
		if !m.used[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	for _, k := range unknown {
		m.errs.Add(m.Path(k), "unknown key")
	}
}

func (e Errors) Error() string {
	parts := make([]string, len(e))
	for i, x := range e {
		parts[i] = x.String()
	}
	return strings.Join(parts, "; ")
}
