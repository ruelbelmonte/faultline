package experiment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"

	"go.yaml.in/yaml/v3"

	"faultline/internal/config"
	"faultline/internal/yamlx"
)

const Redacted = "[REDACTED]"

const unredactable = "# experiment withheld: it could not be redacted safely\n"

// Redact returns the copy of an experiment document that may be stored or
// shown. It replaces:
//
//   - values of headers in cfg.RedactionHeaders (traffic requests and fault
//     params) with [REDACTED];
//   - traffic request bodies and traffic.command with a fingerprint, the
//     SHA-256 and length of the value;
//   - in traffic.base_url and probe URLs, the userinfo (removed) and the values
//     of query parameters whose names contain an entry of
//     cfg.RedactionQueryParams.
//
// The input is returned untouched when nothing needed redacting, so the
// stored copy stays byte-identical to what was submitted.
//
// It fails closed: a document that does not parse, or that uses aliases or
// merge keys (which validation rejects), is replaced by a placeholder.
func Redact(data []byte, cfg *config.Config) []byte {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil || len(doc.Content) == 0 || yamlx.HasIndirection(doc.Content[0]) {
		return []byte(unredactable)
	}
	sensitive := map[string]bool{}
	for _, h := range cfg.RedactionHeaders {
		sensitive[strings.ToLower(h)] = true
	}
	root := doc.Content[0]
	traffic := child(root, "traffic")
	changed := false
	for _, req := range seq(child(traffic, "requests")) {
		changed = redactHeaders(child(req, "headers"), sensitive) || changed
		changed = fingerprintValue(req, "body") || changed
	}
	changed = fingerprintValue(traffic, "command") || changed
	changed = redactURLValue(traffic, "base_url", cfg.RedactionQueryParams) || changed
	for _, p := range seq(child(child(root, "steady_state"), "probes")) {
		changed = redactURLValue(p, "url", cfg.RedactionQueryParams) || changed
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

// Fingerprint is the stored stand-in for a value that is never stored: its
// SHA-256 and its length in bytes. Equal values give equal fingerprints, so
// two runs can be compared without keeping either value.
func Fingerprint(v string) string {
	sum := sha256.Sum256([]byte(v))
	return fmt.Sprintf("%s sha256:%s len:%d]", strings.TrimSuffix(Redacted, "]"), hex.EncodeToString(sum[:]), len(v))
}

func isRedacted(v string) bool { return strings.HasPrefix(v, strings.TrimSuffix(Redacted, "]")) }

func scalarValue(m *yaml.Node, key string) (*yaml.Node, int) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, -1
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1], i + 1
		}
	}
	return nil, -1
}

func setString(m *yaml.Node, i int, v string) {
	m.Content[i] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v, Style: yaml.DoubleQuotedStyle}
}

// fingerprintValue replaces a non-empty scalar m[key] with its fingerprint.
// A value that is not a plain scalar is replaced by [REDACTED].
func fingerprintValue(m *yaml.Node, key string) bool {
	v, i := scalarValue(m, key)
	switch {
	case v == nil:
		return false
	case v.Kind != yaml.ScalarNode:
		setString(m, i, Redacted)
		return true
	case v.Value == "" || isRedacted(v.Value):
		return false
	}
	setString(m, i, Fingerprint(v.Value))
	return true
}

// redactURLValue removes userinfo from the URL in m[key] and redacts the
// values of sensitive query parameters, leaving the rest of it as written.
// A value that does not parse as a URL is replaced by [REDACTED].
func redactURLValue(m *yaml.Node, key string, params []string) bool {
	v, i := scalarValue(m, key)
	if v == nil {
		return false
	}
	if v.Kind != yaml.ScalarNode {
		setString(m, i, Redacted)
		return true
	}
	out, ok := RedactURL(v.Value, params)
	if !ok {
		setString(m, i, Redacted)
		return true
	}
	if out == v.Value {
		return false
	}
	setString(m, i, out)
	return true
}

// RedactURL removes the userinfo of raw and replaces the value of every query
// parameter whose name contains one of params (case-insensitively) with
// REDACTED. Other parameters, their order and their encoding are kept as
// written. ok is false when raw does not parse as a URL.
func RedactURL(raw string, params []string) (out string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	changed := false
	if u.User != nil {
		u.User = nil
		changed = true
	}
	if u.RawQuery != "" {
		parts := strings.Split(u.RawQuery, "&")
		for j, part := range parts {
			name, _, hasValue := strings.Cut(part, "=")
			decoded, err := url.QueryUnescape(name)
			if err != nil {
				decoded = name
			}
			if !hasValue || !sensitiveParam(decoded, params) {
				continue
			}
			if redactedPart := name + "=REDACTED"; part != redactedPart {
				parts[j] = redactedPart
				changed = true
			}
		}
		u.RawQuery = strings.Join(parts, "&")
	}
	if !changed {
		return raw, true
	}
	return u.String(), true
}

func sensitiveParam(name string, params []string) bool {
	n := strings.ToLower(name)
	for _, p := range params {
		if p != "" && strings.Contains(n, p) {
			return true
		}
	}
	return false
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

// Warnings are problems that do not stop an experiment from running. Today
// that is a URL carrying credentials (user:password@): they are sent, but
// removed from every stored copy, so a reader of the stored experiment
// cannot see what was used.
func Warnings(e *Experiment) yamlx.Errors {
	var w yamlx.Errors
	hasUserinfo := func(raw string) bool {
		u, err := url.Parse(raw)
		return err == nil && u.User != nil
	}
	const msg = "contains credentials (user:password@); they are sent but never stored. Prefer a header listed in redaction.headers"
	if e.Traffic != nil && hasUserinfo(e.Traffic.BaseURL) {
		w.Add("traffic.base_url", msg)
	}
	for i, p := range e.Probes {
		if hasUserinfo(p.URL) {
			w.Add(fmt.Sprintf("steady_state.probes[%d].url", i), msg)
		}
	}
	return w
}
