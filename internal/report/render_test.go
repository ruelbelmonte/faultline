package report

import (
	"encoding/json"
	"encoding/xml"
	"regexp"
	"strings"
	"testing"
	"time"
)

func f64(v float64) *float64 { return &v }
func str(v string) *string   { return &v }

var started = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func sampleDoc() *Document {
	end := At(started.Add(40 * time.Second))
	return &Document{
		SchemaVersion: "1", RunID: "run_abcdefghijkl",
		Experiment:  ExperimentInfo{Name: "inventory-503", Description: `Inventory returns 503s <b>&"quoted"</b> | piped`, YAMLSHA256: strings.Repeat("ab", 32)},
		Environment: "local", Status: StatusCompleted, Verdict: str(VerdictFailed),
		StartedAt: At(started), EndedAt: &end,
		Hypothesis: []HypothesisEntry{
			{Name: "error_rate_max", Threshold: 0.05, Observed: f64(0.12), Passed: false},
			{Name: "p95_latency_ms_max", Threshold: 1500, Observed: f64(900), Passed: true},
			{Name: "probe_success_min", Threshold: 0.95, Observed: nil, Passed: false},
		},
		Faults: []FaultResult{
			{ID: "f1", Type: "http_error", Route: "inventory", Triggers: 312, StartAt: AtPtr(started.Add(5 * time.Second)), EndAt: AtPtr(started.Add(35 * time.Second))},
			{ID: "f<2>", Type: "reset", Route: "pricing", Triggers: 0},
		},
		Findings: []Finding{{
			ID: "retry_amplification", Severity: "medium", Title: "Retries amplify load on route inventory",
			Detail:   "Each logical request made 4.00 calls.",
			Evidence: map[string]any{"route": "inventory", "mean_attempts": 4.0, "max_attempts": 4, "correlations": 310, "match": map[string]any{"path": "/stock/*"}},
		}},
		Metrics: Metrics{
			Driver: DriverMetrics{Requests: 1240, Errors: 150, ErrorRate: 0.121, P50Ms: 40, P95Ms: 900},
			Proxy:  map[string]RouteMetrics{"inventory": {Requests: 1800, Faulted: 312}},
		},
		RecoverySeconds: f64(4),
		Redaction:       Redaction{Enabled: true, Headers: []string{"authorization", "cookie"}},
	}
}

func sampleSeries() *Timeseries {
	ts := &Timeseries{BucketSeconds: 1}
	for i := 0; i < 40; i++ {
		var p Point
		p.T = At(started.Add(time.Duration(i) * time.Second))
		p.Driver.Requests, p.Driver.P95Ms = 20, 40
		if i >= 5 && i < 35 {
			p.Driver.Errors, p.Driver.P95Ms, p.Proxy.Faulted = 3, 900, 10
		}
		p.Proxy.Requests, p.Probes.OK = 30, 1
		ts.Series = append(ts.Series, p)
	}
	inj, rec := At(started.Add(5*time.Second)), At(started.Add(35*time.Second))
	end := At(started.Add(40 * time.Second))
	ts.Phases = []Phase{{Name: "prechecking", Start: At(started), End: &inj}, {Name: "injecting", Start: inj, End: &rec}, {Name: "recovering", Start: rec, End: &end}}
	ts.FaultWindows = []FaultWindow{{FaultID: "f1", Start: inj, End: rec}}
	return ts
}

func TestJSONMatchesCanonicalSchema(t *testing.T) {
	data, err := JSON(sampleDoc())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	want := []string{"schema_version", "run_id", "experiment", "environment", "status", "verdict", "reason", "started_at", "ended_at",
		"hypothesis", "faults", "findings", "metrics", "recovery_seconds", "redaction"}
	if len(m) != len(want) {
		t.Fatalf("top-level keys: got %d, want %d", len(m), len(want))
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}
	if m["started_at"] != "2026-01-02T03:04:05Z" || m["ended_at"] != "2026-01-02T03:04:45Z" || m["reason"] != nil {
		t.Fatalf("timestamps or reason: %v %v %v", m["started_at"], m["ended_at"], m["reason"])
	}
	h := m["hypothesis"].([]any)[2].(map[string]any)
	if v, ok := h["observed"]; !ok || v != nil || h["passed"] != false {
		t.Fatalf("observed must be an explicit null: %v", h)
	}
	f := m["faults"].([]any)[1].(map[string]any)
	if v, ok := f["start_at"]; !ok || v != nil {
		t.Fatalf("start_at must be an explicit null for a fault that was never installed: %v", f)
	}
	d := m["metrics"].(map[string]any)["driver"].(map[string]any)
	for _, k := range []string{"requests", "errors", "error_rate", "p50_ms", "p95_ms"} {
		if _, ok := d[k]; !ok {
			t.Errorf("metrics.driver lacks %q", k)
		}
	}

	var back Document
	if err := json.Unmarshal(data, &back); err != nil || back.RunID != "run_abcdefghijkl" || !back.StartedAt.Equal(started) {
		t.Fatalf("round trip failed: %v", err)
	}

	// In progress: verdict and ended_at are null.
	live := sampleDoc()
	live.Verdict, live.EndedAt, live.Status = nil, nil, StatusInjecting
	data, _ = JSON(live)
	json.Unmarshal(data, &m)
	if m["verdict"] != nil || m["ended_at"] != nil {
		t.Fatalf("verdict and ended_at must be null in progress: %v %v", m["verdict"], m["ended_at"])
	}
}

func TestMarkdown(t *testing.T) {
	md := Markdown(sampleDoc())
	for _, want := range []string{
		"# Faultline report: inventory-503",
		"**Verdict: FAILED**",
		"| error_rate_max | 0.05 | 0.12 | FAIL |",
		"| p95_latency_ms_max | 1500 | 900 | pass |",
		"| probe_success_min | 0.95 | no data | FAIL |",
		"### [medium] Retries amplify load on route inventory",
		"- mean_attempts: 4",
		"- correlations: 310",
		`- match: {"path":"/stock/*"}`,
		"| f1 | http_error | inventory | 312 |",
		"not installed",
		"| 1240 | 150 | 0.121 | 40 ms | 900 ms |",
		"| inventory | 1800 | 312 |",
		`piped`,
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q", want)
		}
	}
	if strings.Contains(md, " | piped") || !strings.Contains(md, `| piped`) {
		t.Error("a pipe in user text must be escaped so it cannot break a table")
	}
	aborted := sampleDoc()
	aborted.Verdict, aborted.Reason, aborted.Status = str(VerdictAborted), str("kill_switch"), StatusAborted
	if md := Markdown(aborted); !strings.Contains(md, "**Verdict: ABORTED** (kill_switch)") {
		t.Errorf("verdict line with reason missing:\n%s", md[:200])
	}
}

type suite struct {
	XMLName  xml.Name `xml:"testsuite"`
	Name     string   `xml:"name,attr"`
	Tests    int      `xml:"tests,attr"`
	Failures int      `xml:"failures,attr"`
	Cases    []struct {
		Name    string `xml:"name,attr"`
		Failure *struct {
			Message string `xml:"message,attr"`
		} `xml:"failure"`
	} `xml:"testcase"`
}

func parseJUnit(t *testing.T, doc *Document) suite {
	t.Helper()
	data, err := JUnit(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), `<?xml version="1.0" encoding="UTF-8"?>`) {
		t.Fatalf("missing XML declaration: %.60s", data)
	}
	var s suite
	if err := xml.Unmarshal(data, &s); err != nil {
		t.Fatalf("JUnit does not parse: %v\n%s", err, data)
	}
	return s
}

func TestJUnit(t *testing.T) {
	s := parseJUnit(t, sampleDoc())
	if s.Name != "inventory-503" || s.Tests != 5 || s.Failures != 3 || len(s.Cases) != 5 {
		t.Fatalf("suite: %+v", s)
	}
	want := []struct {
		name   string
		failed bool
	}{
		{"error_rate_max", true}, {"p95_latency_ms_max", false}, {"probe_success_min", true},
		{"fault f1 triggered", false}, {"fault f<2> triggered", true},
	}
	for i, w := range want {
		if s.Cases[i].Name != w.name || (s.Cases[i].Failure != nil) != w.failed {
			t.Errorf("case %d: got %q failed=%v, want %q failed=%v", i, s.Cases[i].Name, s.Cases[i].Failure != nil, w.name, w.failed)
		}
	}
	if !strings.Contains(s.Cases[0].Failure.Message, "0.12") || !strings.Contains(s.Cases[0].Failure.Message, "0.05") {
		t.Errorf("failure message must carry the numbers: %q", s.Cases[0].Failure.Message)
	}

	aborted := sampleDoc()
	aborted.Status, aborted.Verdict, aborted.Reason = StatusAborted, str(VerdictAborted), str("manual")
	s = parseJUnit(t, aborted)
	last := s.Cases[len(s.Cases)-1]
	if s.Tests != 6 || last.Name != "run not aborted" || last.Failure == nil || !strings.Contains(last.Failure.Message, "manual") {
		t.Fatalf("aborted runs add a failing 'run not aborted' case: %+v", last)
	}

	passed := sampleDoc()
	passed.Hypothesis = passed.Hypothesis[1:2]
	passed.Faults = passed.Faults[:1]
	if s = parseJUnit(t, passed); s.Tests != 2 || s.Failures != 0 {
		t.Fatalf("a passing run has no failures: %+v", s)
	}
}

var externalRef = regexp.MustCompile(`(?i)https?:|//[a-z0-9]|url\(|@import|<link|<iframe|<img|src\s*=|xmlns|xlink`)

func TestHTMLIsSelfContained(t *testing.T) {
	doc := sampleDoc()
	doc.Status, doc.Verdict, doc.Reason = StatusAborted, str(VerdictAborted), str("threshold:error_rate_max")
	ts := sampleSeries()
	ts.Markers = []Marker{{Type: "abort", T: At(started.Add(20 * time.Second)), Detail: "threshold:error_rate_max"}}
	data, err := HTML(doc, ts)
	if err != nil {
		t.Fatal(err)
	}
	page := string(data)
	if m := externalRef.FindString(page); m != "" {
		i := strings.Index(page, m)
		t.Fatalf("the report refers to something outside the file: %q in %q", m, page[max(0, i-60):min(len(page), i+60)])
	}
	for _, want := range []string{
		"<!doctype html>", "<style>", "<script>", "<svg", "</html>",
		"Verdict: aborted", "threshold:error_rate_max",
		"run_abcdefghijkl", "inventory-503",
		`class="fault-window"`, `class="phase"`, `class="marker"`, `class="line-rate"`, `class="line-p95"`,
		"error_rate_max", "no data", "Retries amplify load on route inventory", "mean_attempts",
		"prefers-color-scheme:dark", "@media print",
		"authorization, cookie",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("HTML report lacks %q", want)
		}
	}
	if strings.Contains(page, "<b>&") || !strings.Contains(page, "&lt;b&gt;") {
		t.Error("user text must be escaped")
	}
	if strings.Contains(page, "fault f<2>") {
		t.Error("fault ids must be escaped")
	}
}

func TestHTMLWithoutTimeline(t *testing.T) {
	data, err := HTML(sampleDoc(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "No timeline data") || externalRef.MatchString(string(data)) {
		t.Fatal("a run without metrics must still render a self-contained report")
	}
}

func TestRenderFormats(t *testing.T) {
	for _, f := range Formats {
		data, err := Render(f, sampleDoc(), sampleSeries())
		if err != nil || len(data) == 0 || ContentType(f) == "" {
			t.Errorf("format %s: %v", f, err)
		}
	}
	if Ext(FormatJUnit) != "xml" || Ext(FormatMD) != "md" || Ext(FormatHTML) != "html" || Ext(FormatJSON) != "json" {
		t.Error("file extensions")
	}
	if _, err := Render("pdf", sampleDoc(), nil); err == nil || ContentType("pdf") != "" {
		t.Error("unknown format must be rejected")
	}
}

func TestNiceCeil(t *testing.T) {
	for in, want := range map[float64]float64{0: 1, 0.4: 0.5, 1: 1, 3: 5, 7: 10, 42: 50, 900: 1000, 1500: 2000} {
		if got := niceCeil(in); got != want {
			t.Errorf("niceCeil(%v) = %v, want %v", in, got, want)
		}
	}
}
