package experiment

import (
	"bytes"
	"strings"

	"go.yaml.in/yaml/v3"

	"faultline/internal/yamlx"
)

const Redacted = "[REDACTED]"

const unredactable = "# experiment withheld: it could not be redacted safely\n"

// Redact replaces the values of sensitive headers inside an experiment
// document. The input is returned untouched when nothing needed redacting,
// so the stored copy stays byte-identical to what was submitted.
//
// It fails closed: a document that does not parse, or that uses aliases or
// merge keys (which validation rejects), is replaced by a placeholder.
func Redact(data []byte, headers []string) []byte {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil || len(doc.Content) == 0 || yamlx.HasIndirection(doc.Content[0]) {
		return []byte(unredactable)
	}
	sensitive := map[string]bool{}
	for _, h := range headers {
		sensitive[strings.ToLower(h)] = true
	}
	root := doc.Content[0]
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
		return []byte(unredactable)
	}
	enc.Close()
	return buf.Bytes()
}

func child(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func seq(n *yaml.Node) []*yaml.Node {
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	return n.Content
}

func redactHeaders(n *yaml.Node, sensitive map[string]bool) bool {
	if n == nil || n.Kind != yaml.MappingNode {
		return false
	}
	changed := false
	for i := 0; i+1 < len(n.Content); i += 2 {
		if !sensitive[strings.ToLower(n.Content[i].Value)] {
			continue
		}
		v := n.Content[i+1]
		if v.Kind == yaml.ScalarNode && v.Value == Redacted {
			continue
		}
		n.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: Redacted, Style: yaml.DoubleQuotedStyle}
		changed = true
	}
	return changed
}
