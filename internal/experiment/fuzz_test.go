package experiment

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// Seeds: a valid experiment, the redaction bypasses found during the first
// build (alias value, merge key, alias key, binary-tagged key), and the
// shapes that carry secrets.
var fuzzSeeds = []string{
	base,
	strings.Replace(base, "headers: {}", "headers: {Authorization: Bearer s3cr3t-value-0001, X-Trace: keep-me}", 1),
	strings.Replace(base, "status: 503", "status: 503\n      headers: {Set-Cookie: session=s3cr3t-cookie-0010}", 1),
	"version: 1\nname: alias\ndescription: &tok Bearer s3cr3t-value-0002\ntarget: {route: inventory}\nduration: 5s\ntraffic:\n  base_url: http://127.0.0.1:9000\n  rps: 1\n  requests:\n    - path: /a\n      headers: {Authorization: *tok}\nfaults:\n  - {id: f1, type: reset}\n",
	"version: 1\nname: merge\ntarget: {route: inventory}\nduration: 5s\nfaults:\n  - id: f1\n    type: http_error\n    params:\n      status: 503\n      headers:\n        <<: {Set-Cookie: session=s3cr3t-value-0003}\n",
	"version: 1\nname: aliaskey\ndescription: &k Set-Cookie\ntarget: {route: inventory}\nduration: 5s\nfaults:\n  - id: f1\n    type: http_error\n    params:\n      status: 503\n      headers: {*k : session=s3cr3t-value-0004}\n",
	"version: 1\nname: binarykey\ntarget: {route: inventory}\nduration: 5s\nfaults:\n  - id: f1\n    type: http_error\n    params:\n      status: 503\n      headers: {!!binary U2V0LUNvb2tpZQ==: session=s3cr3t-value-0005}\n",
	"version: 1\nname: body\ntarget: {route: inventory}\nduration: 5s\ntraffic:\n  base_url: http://user:s3cr3t-pass@127.0.0.1:9000/?token=s3cr3t-query\n  rps: 1\n  requests:\n    - {method: POST, path: /a, body: '{\"card\":\"s3cr3t-body-0006\"}'}\nfaults:\n  - {id: f1, type: reset}\n",
	"version: 1\nname: command\ntarget: {route: inventory}\nduration: 5s\ntraffic:\n  command: \"curl -H 'Authorization: Bearer s3cr3t-cmd-0007' http://127.0.0.1:9000\"\nfaults:\n  - {id: f1, type: reset}\n",
	"traffic: {requests: [{headers: {AUTHORIZATION: !!str s3cr3t-value-0008}}]}",
	"traffic: {requests: [{headers: {\"Authori\\x7Aation\": s3cr3t-value-0009}}]}",
	"name: [unclosed s3cr3t",
}

// FuzzParse: validation never panics, every error has a message, and a valid
// experiment still validates after redaction.
func FuzzParse(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add([]byte(s))
	}
	cfg := testConfig()
	f.Fuzz(func(t *testing.T, data []byte) {
		_, errs := Parse(data, cfg)
		for _, e := range errs {
			if e.Message == "" {
				t.Fatalf("error without a message at %q", e.Path)
			}
		}
		if len(errs) > 0 {
			return
		}
		if _, errs := Parse(Redact(data, cfg), cfg); len(errs) > 0 {
			t.Fatalf("a valid experiment no longer validates after redaction: %v\n%s", errs, Redact(data, cfg))
		}
	})
}

var placeholders = regexp.MustCompile(`\[REDACTED( sha256:[0-9a-f]{64} len:\d+)?\]|` + regexp.QuoteMeta(strings.TrimSpace(unredactable)))

// FuzzRedact: no value that redaction must hide survives in the stored copy,
// whatever the YAML does to place it (aliases, merge keys, tags, quoting).
func FuzzRedact(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add([]byte(s))
	}
	cfg := testConfig()
	cfg.AllowCommands = true
	sensitive := map[string]bool{}
	for _, h := range cfg.RedactionHeaders {
		sensitive[strings.ToLower(h)] = true
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		out := Redact(data, cfg)
		if _, errs := Parse(data, cfg); len(errs) > 0 {
			if string(out) != unredactable {
				t.Fatalf("a document that does not validate must be withheld, got:\n%s", out)
			}
			return
		}
		var doc any
		if err := yaml.Unmarshal(data, &doc); err != nil {
			t.Fatalf("a valid experiment does not decode: %v", err)
		}
		hidden, visible := secretsOf(doc, sensitive)
		stored := placeholders.ReplaceAllString(string(out), "")
		for _, s := range hidden {
			if len(s) < 8 || strings.Contains(visible, s) {
				continue
			}
			if strings.Contains(stored, s) {
				t.Fatalf("%q survived redaction:\n%s", s, out)
			}
		}
	})
}

// secretsOf walks a decoded document, aliases and merges already resolved, and
// returns the values redaction must hide, and every other scalar and key
// joined into one string, since a value that also appears there may stay.
func secretsOf(doc any, sensitive map[string]bool) (hidden []string, visible string) {
	var other strings.Builder
	var walk func(v any, kind string)
	walk = func(v any, kind string) {
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				walkEntry(k, x, kind, &hidden, &other, walk, sensitive)
			}
		case map[any]any:
			for k, x := range v {
				walkEntry(fmt.Sprint(k), x, kind, &hidden, &other, walk, sensitive)
			}
		case []any:
			for _, x := range v {
				walk(x, kind)
			}
		case nil:
		default:
			if kind == "secret" {
				hidden = append(hidden, fmt.Sprint(v))
			} else {
				other.WriteString(fmt.Sprint(v) + "\x00")
			}
		}
	}
	walk(doc, "root")
	return hidden, other.String()
}

// walkEntry classifies one mapping entry by where it sits: the kind passed
// down names the parent, so headers under traffic.requests[] or
// faults[].params count, and body and command under traffic.
func walkEntry(k string, x any, parent string, hidden *[]string, other *strings.Builder, walk func(any, string), sensitive map[string]bool) {
	other.WriteString(k + "\x00")
	next := "other"
	switch {
	case parent == "secret":
		next = "secret"
	case parent == "headers" && sensitive[strings.ToLower(k)]:
		next = "secret"
	case parent == "root" && k == "traffic", parent == "root" && k == "faults":
		next = k
	case parent == "traffic" && k == "requests":
		next = "request"
	case parent == "traffic" && k == "command", parent == "request" && k == "body":
		next = "secret"
	case parent == "request" && k == "headers", parent == "params" && k == "headers":
		next = "headers"
	case parent == "faults" && k == "params":
		next = "params"
	}
	walk(x, next)
}
