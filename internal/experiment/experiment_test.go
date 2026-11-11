package experiment

import (
	"strings"
	"testing"
	"time"

	"faultline/internal/config"
	"faultline/internal/yamlx"
)

const base = `version: 1
name: inventory-503
description: Inventory returns 503s
seed: 42
target:
  route: inventory
scope: header
duration: 30s
traffic:
  base_url: http://127.0.0.1:9000
  rps: 20
  concurrency: 4
  timeout: 30s
  requests:
    - method: GET
      path: /api/orders/1
      headers: {}
      body: ""
steady_state:
  precheck:
    consecutive_ok: 3
    timeout: 15s
  probes:
    - name: shop-health
      url: http://127.0.0.1:9000/health
      interval: 1s
      timeout: 2s
      expect_status: 200
  hypothesis:
    error_rate_max: 0.05
    p95_latency_ms_max: 1500
    probe_success_min: 0.95
abort:
  error_rate_max: 0.5
  probe_failures_consecutive: 5
recovery:
  timeout: 30s
faults:
  - id: f1
    type: http_error
    route: inventory
    match:
      method: GET
      path: /stock/*
    strategy:
      kind: probability
      p: 0.3
    start: 0s
    duration: 30s
    params:
      status: 503
      retry_after: 1
`

func testConfig() *config.Config {
	c := config.Default()
	c.Routes = []config.Route{
		{Name: "inventory", Upstream: "http://127.0.0.1:9001"},
		{Name: "llm", Upstream: "http://127.0.0.1:9003"},
	}
	return c
}

func hasPath(errs yamlx.Errors, path string) bool {
	for _, e := range errs {
		if e.Path == path {
			return true
		}
	}
	return false
}

func TestBaseExperimentIsValid(t *testing.T) {
	e, errs := Parse([]byte(base), testConfig())
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if e.Name != "inventory-503" || e.TargetRoute != "inventory" || e.Duration != 30*time.Second {
		t.Fatalf("unexpected experiment: %+v", e)
	}
	f := e.Faults[0]
	if f.Route != "inventory" || f.Typed.Status != 503 || *f.Typed.RetryAfter != 1 || f.Strategy.P != 0.3 {
		t.Fatalf("unexpected fault: %+v", f)
	}
	if *e.Seed != 42 || e.Traffic.RPS != 20 || len(e.Probes) != 1 {
		t.Fatalf("unexpected fields: %+v", e)
	}
}

func TestDefaults(t *testing.T) {
	doc := `version: 1
name: minimal
target: {route: inventory}
duration: 10s
faults:
  - id: f1
    type: reset
    start: 4s
`
	e, errs := Parse([]byte(doc), testConfig())
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	f := e.Faults[0]
	if e.Scope != "header" || f.Route != "inventory" || f.Strategy.Kind != "always" {
		t.Fatalf("defaults not applied: %+v / %+v", e, f)
	}
	if f.Duration != 6*time.Second {
		t.Fatalf("fault duration should default to the rest of the injection phase, got %s", f.Duration)
	}
	if e.RecoveryTimeout != 30*time.Second || e.PrecheckConsecutiveOK != 3 || e.PrecheckTimeout != 15*time.Second {
		t.Fatalf("defaults not applied: %+v", e)
	}
}

func TestValidationRules(t *testing.T) {
	cases := []struct {
		name, old, new, path string
	}{
		{"missing version", "version: 1\n", "", "version"},
		{"wrong version", "version: 1\n", "version: 2\n", "version"},
		{"missing name", "name: inventory-503\n", "", "name"},
		{"bad name", "name: inventory-503", "name: Inventory_503", "name"},
		{"missing target", "target:\n  route: inventory\n", "", "target"},
		{"missing target route", "target:\n  route: inventory\n", "target: {}\n", "target.route"},
		{"unknown target route", "target:\n  route: inventory", "target:\n  route: nope", "target.route"},
		{"bad scope", "scope: header", "scope: everyone", "scope"},
		{"scope all without allow_unscoped", "scope: header", "scope: all", "scope"},
		{"missing duration", "duration: 30s\ntraffic", "traffic", "duration"},
		{"duration not a duration", "duration: 30s\ntraffic", "duration: 30\ntraffic", "duration"},
		{"duration over maximum", "duration: 30s\ntraffic", "duration: 16m\ntraffic", "duration"},
		{"unknown top-level key", "seed: 42", "seed: 42\nsurprise: 1", "surprise"},
		{"unknown nested key", "  rps: 20", "  rps: 20\n  burst: 3", "traffic.burst"},
		{"traffic without base_url", "  base_url: http://127.0.0.1:9000\n", "", "traffic.base_url"},
		{"traffic bad base_url", "base_url: http://127.0.0.1:9000", "base_url: not-a-url", "traffic.base_url"},
		{"traffic without rps", "  rps: 20\n", "", "traffic.rps"},
		{"traffic zero rps", "rps: 20", "rps: 0", "traffic.rps"},
		{"traffic zero concurrency", "concurrency: 4", "concurrency: 0", "traffic.concurrency"},
		{"traffic requests and command", "  rps: 20", "  rps: 20\n  command: npm run e2e", "traffic.requests"},
		{"traffic neither requests nor command", "  requests:\n    - method: GET\n      path: /api/orders/1\n      headers: {}\n      body: \"\"\n", "", "traffic.requests"},
		{"request path missing", "      path: /api/orders/1\n", "", "traffic.requests[0].path"},
		{"request path relative", "path: /api/orders/1", "path: api/orders/1", "traffic.requests[0].path"},
		{"precheck consecutive_ok zero", "consecutive_ok: 3", "consecutive_ok: 0", "steady_state.precheck.consecutive_ok"},
		{"probe without name", "    - name: shop-health\n      url:", "    - url:", "steady_state.probes[0].name"},
		{"probe bad url", "url: http://127.0.0.1:9000/health", "url: ftp://x", "steady_state.probes[0].url"},
		{"probe bad status", "expect_status: 200", "expect_status: 99", "steady_state.probes[0].expect_status"},
		{"hypothesis error rate out of range", "error_rate_max: 0.05", "error_rate_max: 1.5", "steady_state.hypothesis.error_rate_max"},
		{"hypothesis p95 not a number", "p95_latency_ms_max: 1500", "p95_latency_ms_max: fast", "steady_state.hypothesis.p95_latency_ms_max"},
		{"hypothesis probe success out of range", "probe_success_min: 0.95", "probe_success_min: -1", "steady_state.hypothesis.probe_success_min"},
		{"abort error rate zero", "abort:\n  error_rate_max: 0.5", "abort:\n  error_rate_max: 0", "abort.error_rate_max"},
		{"abort probe failures zero", "probe_failures_consecutive: 5", "probe_failures_consecutive: 0", "abort.probe_failures_consecutive"},
		{"recovery timeout zero", "recovery:\n  timeout: 30s", "recovery:\n  timeout: 0s", "recovery.timeout"},
		{"fault without id", "  - id: f1\n    type:", "  - type:", "faults[0].id"},
		{"fault without type", "    type: http_error\n", "", "faults[0].type"},
		{"fault unknown type", "type: http_error", "type: gremlin", "faults[0].type"},
		{"fault unknown route", "    route: inventory", "    route: nope", "faults[0].route"},
		{"fault unknown strategy", "kind: probability", "kind: sometimes", "faults[0].strategy.kind"},
		{"fault strategy without kind", "      kind: probability\n", "", "faults[0].strategy.kind"},
		{"probability zero", "p: 0.3", "p: 0", "faults[0].strategy.p"},
		{"probability above one", "p: 0.3", "p: 1.01", "faults[0].strategy.p"},
		{"burst count zero", "kind: probability\n      p: 0.3", "kind: burst\n      count: 0", "faults[0].strategy.count"},
		{"every_nth n zero", "kind: probability\n      p: 0.3", "kind: every_nth\n      n: 0", "faults[0].strategy.n"},
		{"fault start + duration too long", "start: 0s", "start: 1s", "faults[0].duration"},
		{"fault start after end", "start: 0s\n    duration: 30s", "start: 30s", "faults[0].start"},
		{"fault param missing", "      status: 503\n", "", "faults[0].params.status"},
		{"fault param wrong type", "status: 503", "status: teapot", "faults[0].params.status"},
		{"fault param out of range", "status: 503", "status: 700", "faults[0].params.status"},
		{"fault param unknown", "status: 503", "status: 503\n      ms: 5", "faults[0].params.ms"},
		{"fault match unknown key", "      method: GET\n", "      verb: GET\n", "faults[0].match.verb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(base, tc.old) {
				t.Fatalf("test bug: %q not in base document", tc.old)
			}
			doc := strings.Replace(base, tc.old, tc.new, 1)
			_, errs := Parse([]byte(doc), testConfig())
			if !hasPath(errs, tc.path) {
				t.Fatalf("expected an error at %q, got %v", tc.path, errs)
			}
		})
	}
}

func TestNoFaults(t *testing.T) {
	doc := base[:strings.Index(base, "faults:")]
	_, errs := Parse([]byte(doc), testConfig())
	if !hasPath(errs, "faults") {
		t.Fatalf("expected an error at faults, got %v", errs)
	}
	_, errs = Parse([]byte(doc+"faults: []\n"), testConfig())
	if !hasPath(errs, "faults") {
		t.Fatalf("expected an error at faults for an empty list, got %v", errs)
	}
}

func TestDuplicateFaultID(t *testing.T) {
	doc := base + "  - id: f1\n    type: reset\n"
	_, errs := Parse([]byte(doc), testConfig())
	if !hasPath(errs, "faults[1].id") {
		t.Fatalf("expected an error at faults[1].id, got %v", errs)
	}
}

func TestScopeAllAllowedWhenConfigured(t *testing.T) {
	cfg := testConfig()
	cfg.AllowUnscoped = true
	e, errs := Parse([]byte(strings.Replace(base, "scope: header", "scope: all", 1)), cfg)
	if len(errs) > 0 || e.Scope != "all" {
		t.Fatalf("scope all should be accepted: %v", errs)
	}
}

func TestCommandTraffic(t *testing.T) {
	doc := `version: 1
name: cmd
target: {route: inventory}
duration: 5s
traffic:
  command: "npm run e2e"
faults:
  - {id: f1, type: reset}
`
	e, errs := Parse([]byte(doc), testConfig())
	if len(errs) > 0 || e.Traffic.Command != "npm run e2e" {
		t.Fatalf("command traffic should be accepted: %v", errs)
	}
}

func TestInvalidYAML(t *testing.T) {
	_, errs := Parse([]byte("name: [unclosed"), testConfig())
	if len(errs) == 0 {
		t.Fatal("expected an error for malformed YAML")
	}
	_, errs = Parse([]byte("- a\n- b\n"), testConfig())
	if len(errs) == 0 {
		t.Fatal("expected an error for a non-mapping document")
	}
}

func TestFaultParams(t *testing.T) {
	template := `version: 1
name: params
target: {route: llm}
duration: 10s
faults:
  - id: f1
    type: TYPE
    params: PARAMS
`
	valid := []struct{ typ, params string }{
		{"latency", "{ms: 200, jitter_ms: 50}"},
		{"http_error", "{status: 500, body: oops, headers: {X-A: b}, retry_after: 2}"},
		{"error_in_body", `{body: '{"error":"x"}', content_type: text/plain}`},
		{"timeout", "{hold: 3s}"},
		{"timeout", "{}"},
		{"reset", "{}"},
		{"slow_body", "{bytes_per_sec: 100}"},
		{"corrupt_body", "{mode: truncate, at_bytes: 10}"},
		{"corrupt_body", "{mode: garble}"},
		{"llm_rate_limit", "{retry_after: 3}"},
		{"llm_rate_limit", "{}"},
		{"llm_overloaded", "{}"},
		{"llm_stream_cut", "{after_bytes: 120}"},
		{"llm_slow_first_token", "{delay_ms: 500}"},
		{"llm_truncate", "{max_chars: 20}"},
		{"llm_truncate", "{}"},
		{"llm_malformed_tool_call", "{}"},
	}
	for _, tc := range valid {
		doc := strings.NewReplacer("TYPE", tc.typ, "PARAMS", tc.params).Replace(template)
		if _, errs := Parse([]byte(doc), testConfig()); len(errs) > 0 {
			t.Errorf("%s %s: unexpected errors %v", tc.typ, tc.params, errs)
		}
	}
	invalid := []struct{ typ, params, path string }{
		{"latency", "{}", "faults[0].params.ms"},
		{"latency", "{ms: -1}", "faults[0].params.ms"},
		{"latency", "{ms: 1.5}", "faults[0].params.ms"},
		{"http_error", "{}", "faults[0].params.status"},
		{"http_error", "{status: 500, headers: [a]}", "faults[0].params.headers"},
		{"error_in_body", "{}", "faults[0].params.body"},
		{"timeout", "{hold: soon}", "faults[0].params.hold"},
		{"reset", "{ms: 1}", "faults[0].params.ms"},
		{"slow_body", "{}", "faults[0].params.bytes_per_sec"},
		{"slow_body", "{bytes_per_sec: 0}", "faults[0].params.bytes_per_sec"},
		{"corrupt_body", "{}", "faults[0].params.mode"},
		{"corrupt_body", "{mode: shuffle}", "faults[0].params.mode"},
		{"corrupt_body", "{mode: truncate}", "faults[0].params.at_bytes"},
		{"llm_stream_cut", "{}", "faults[0].params.after_bytes"},
		{"llm_slow_first_token", "{}", "faults[0].params.delay_ms"},
		{"llm_truncate", "{max_chars: -5}", "faults[0].params.max_chars"},
		{"llm_overloaded", "{retry_after: 1}", "faults[0].params.retry_after"},
	}
	for _, tc := range invalid {
		doc := strings.NewReplacer("TYPE", tc.typ, "PARAMS", tc.params).Replace(template)
		_, errs := Parse([]byte(doc), testConfig())
		if !hasPath(errs, tc.path) {
			t.Errorf("%s %s: expected an error at %q, got %v", tc.typ, tc.params, tc.path, errs)
		}
	}
}

func TestRedact(t *testing.T) {
	doc := strings.Replace(base, "headers: {}", "headers: {Authorization: Bearer s3cr3t-value, X-Trace: keep-me}", 1)
	doc = strings.Replace(doc, "status: 503", "status: 503\n      headers: {Set-Cookie: session=s3cr3t-cookie}", 1)
	out := string(Redact([]byte(doc), testConfig().RedactionHeaders))
	if strings.Contains(out, "s3cr3t") {
		t.Fatalf("secret survived redaction:\n%s", out)
	}
	if !strings.Contains(out, "keep-me") || strings.Count(out, Redacted) != 2 {
		t.Fatalf("unexpected redaction result:\n%s", out)
	}
	if _, errs := Parse([]byte(out), testConfig()); len(errs) > 0 {
		t.Fatalf("redacted document no longer validates: %v", errs)
	}
}

func TestRedactLeavesCleanDocumentUntouched(t *testing.T) {
	if out := Redact([]byte(base), testConfig().RedactionHeaders); string(out) != base {
		t.Fatal("a document with nothing to redact must be returned byte for byte")
	}
}

func TestRedactFollowsAliases(t *testing.T) {
	doc := `version: 1
name: alias
description: &tok Bearer s3cr3t-value
target: {route: inventory}
duration: 5s
traffic:
  base_url: http://127.0.0.1:9000
  rps: 1
  requests:
    - path: /a
      headers: {Authorization: *tok}
faults:
  - {id: f1, type: reset}
`
	out := string(Redact([]byte(doc), []string{"authorization"}))
	if strings.Contains(out, "s3cr3t") {
		t.Fatalf("secret reachable through an alias survived redaction:\n%s", out)
	}
}
