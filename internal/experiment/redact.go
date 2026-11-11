package experiment

import (
	"bytes"
	"strings"

	"go.yaml.in/yaml/v3"
)

const Redacted = "[REDACTED]"

// Redact replaces the values of sensitive headers inside an experiment
// document. The input is returned untouched when nothing needed redacting,
// so the stored copy stays byte-identical to what was submitted.
func Redact(data []byte, headers []string) []byte {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil || len(doc.Content) == 0 {
		return data
	}
	sensitive := map[string]bool{}
	for _, h := range headers {
		sensitive[strings.ToLower(h)] = true
	}
	root := deref(doc.Content[0])
	changed := false
	for _, req := range seq(child(child(root, "traffic"), "requests")) {
		changed = redactHeaders(child(req, "headers"), sensitive) || changed
	}
	for _, f := range seq(child(root, "faults")) {
		changed = redactHeaders(child(child(f, "params"), "headers"), sensitive) || changed
	}
	if !changed {
		return data
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return []byte("# experiment could not be re-encoded after redaction\n")
	}
	enc.Close()
	return buf.Bytes()
}

func deref(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

func child(n *yaml.Node, key string) *yaml.Node {
	n = deref(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return deref(n.Content[i+1])
		}
	}
	return nil
}

func seq(n *yaml.Node) []*yaml.Node {
	n = deref(n)
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	return n.Content
}

func redactHeaders(n *yaml.Node, sensitive map[string]bool) bool {
	n = deref(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return false
	}
	changed := false
	for i := 0; i+1 < len(n.Content); i += 2 {
		if !sensitive[strings.ToLower(n.Content[i].Value)] {
			continue
		}
		v := deref(n.Content[i+1])
		if v.Kind == yaml.ScalarNode && v.Value == Redacted {
			continue
		}
		// The anchored node itself is mutated, so the secret is gone wherever it is aliased.
		v.Kind, v.Tag, v.Value, v.Style, v.Content, v.Alias = yaml.ScalarNode, "!!str", Redacted, yaml.DoubleQuotedStyle, nil, nil
		changed = true
	}
	return changed
}
