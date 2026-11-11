//go:build integration

package integration

import (
	"bytes"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"text/template"
	"time"
)

type controlProc struct {
	*proc
	s *stack
}

func (s *stack) startControl(t testing.TB, extraEnv ...string) *controlProc {
	t.Helper()
	s.writeConfig(t)
	c := &controlProc{s: s}
	env := append([]string{"FAULTLINE_TOKEN=" + controlToken, "FAULTLINE_ADMIN_TOKEN=" + adminToken}, extraEnv...)
	c.proc = start(t, "faultline", env, "control", "--config", s.cfgPath)
	waitHTTP(t, "http://"+s.controlAddr+"/healthz")
	return c
}

func (c *controlProc) url(path string) string { return "http://" + c.s.controlAddr + path }

func (c *controlProc) api(t testing.TB, method, path string, body, out any) int {
	t.Helper()
	return apiCall(t, method, c.url(path), controlToken, body, out)
}

// runDoc mirrors the canonical run document.
type runDoc struct {
	SchemaVersion string `json:"schema_version"`
	RunID         string `json:"run_id"`
	Experiment    struct {
		Name, Description string
		YAMLSHA256        string `json:"yaml_sha256"`
	}
	Environment string
	Status      string
	Verdict     *string
	Reason      *string
	StartedAt   string  `json:"started_at"`
	EndedAt     *string `json:"ended_at"`
	Hypothesis  []struct {
		Name      string
		Threshold float64
		Observed  *float64
		Passed    bool
	}
	Faults []struct {
		ID, Type, Route string
		Triggers        int64
		StartAt         *string `json:"start_at"`
		EndAt           *string `json:"end_at"`
	}
	Findings []struct {
		ID, Severity, Title, Detail string
		Evidence                    map[string]any
	}
	Metrics struct {
		Driver struct {
			Requests, Errors int
			ErrorRate        float64 `json:"error_rate"`
			P50Ms            float64 `json:"p50_ms"`
			P95Ms            float64 `json:"p95_ms"`
		}
		Proxy map[string]struct{ Requests, Faulted int }
	}
	RecoverySeconds *float64 `json:"recovery_seconds"`
	Redaction       struct {
		Enabled bool
		Headers []string
	}
}

func (d runDoc) verdict() string {
	if d.Verdict == nil {
		return "<null>"
	}
	return *d.Verdict
}

func (d runDoc) reason() string {
	if d.Reason == nil {
		return "<null>"
	}
	return *d.Reason
}

func (d runDoc) hypothesis(name string) (observed *float64, passed, found bool) {
	for _, h := range d.Hypothesis {
		if h.Name == name {
			return h.Observed, h.Passed, true
		}
	}
	return nil, false, false
}

var runIDRE = regexp.MustCompile(`^run_[a-z2-7]{12}$`)

func (c *controlProc) createRun(t testing.TB, yaml string) string {
	t.Helper()
	var resp struct {
		RunID  string `json:"run_id"`
		Error  string
		Errors []struct{ Path, Message string }
	}
	if code := c.api(t, "POST", "/api/v1/runs", obj{"experiment_yaml": yaml}, &resp); code != 201 {
		t.Fatalf("creating run: status %d %+v\n%s", code, resp, yaml)
	}
	if !runIDRE.MatchString(resp.RunID) {
		t.Fatalf("run id %q does not match run_ + 12 lowercase base32 characters", resp.RunID)
	}
	return resp.RunID
}

func (c *controlProc) run(t testing.TB, id string) runDoc {
	t.Helper()
	var d runDoc
	if code := c.api(t, "GET", "/api/v1/runs/"+id, nil, &d); code != 200 {
		t.Fatalf("GET run %s: status %d", id, code)
	}
	return d
}

func terminal(status string) bool { return status == "completed" || status == "aborted" }

func (c *controlProc) waitStatus(t testing.TB, id string, d time.Duration, statuses ...string) runDoc {
	t.Helper()
	var doc runDoc
	eventually(t, d, "run "+id+" to reach "+strings.Join(statuses, "|"), func() bool {
		doc = c.run(t, id)
		for _, s := range statuses {
			if doc.Status == s {
				return true
			}
		}
		if terminal(doc.Status) {
			t.Fatalf("run ended as %s/%s (%s) while waiting for %v", doc.Status, doc.verdict(), doc.reason(), statuses)
		}
		return false
	})
	return doc
}

func (c *controlProc) waitDone(t testing.TB, id string, d time.Duration) runDoc {
	t.Helper()
	return c.waitStatus(t, id, d, "completed", "aborted")
}

type fullStack struct {
	s       *stack
	demo    *demoProc
	proxy   *proxyProc
	control *controlProc
}

// startFull starts demo, proxy and control as three real processes.
func startFull(t testing.TB, mode string, mods ...func(*stack)) *fullStack {
	t.Helper()
	s := newStack(t, nil)
	for _, m := range mods {
		m(s)
	}
	f := &fullStack{s: s}
	f.demo = startDemo(t, mode, "http://"+s.trafficAddr)
	s.routes = map[string]string{
		"inventory": "http://" + f.demo.inventory,
		"pricing":   "http://" + f.demo.pricing,
		"llm":       "http://" + f.demo.llm,
	}
	f.proxy = s.startProxy(t)
	f.control = s.startControl(t)
	return f
}

type expOpts struct {
	Name       string
	Duration   string
	Scope      string
	BaseURL    string
	RPS        int
	Requests   string
	NoTraffic  bool
	Precheck   string
	Probes     string
	Hypothesis string
	Abort      string
	Recovery   string
	Faults     string
}

var expTemplate = template.Must(template.New("exp").Parse(`version: 1
name: {{.Name}}
description: integration test experiment
seed: 42
target:
  route: inventory
scope: {{.Scope}}
duration: {{.Duration}}
{{if not .NoTraffic}}traffic:
  base_url: {{.BaseURL}}
  rps: {{.RPS}}
  concurrency: 8
  timeout: 10s
  requests:
{{.Requests}}
{{end}}steady_state:
  precheck:
{{.Precheck}}
  probes:
{{.Probes}}
  hypothesis:
{{.Hypothesis}}
{{if .Abort}}abort:
{{.Abort}}
{{end}}recovery:
  timeout: {{.Recovery}}
faults:
{{.Faults}}
`))

const always503 = `  - id: f1
    type: http_error
    match: {method: GET, path: "/stock/*"}
    params: {status: 503}`

// experiment renders an experiment against this stack's demo, with defaults
// that pass against a healthy system.
func (f *fullStack) experiment(t testing.TB, o expOpts) string {
	t.Helper()
	def := func(v *string, d string) {
		if *v == "" {
			*v = d
		}
	}
	shop := "http://" + f.demo.shop
	def(&o.Name, "itest")
	def(&o.Duration, "3s")
	def(&o.Scope, "header")
	def(&o.BaseURL, shop)
	def(&o.Requests, "    - {method: GET, path: /api/orders/1}")
	def(&o.Precheck, "    consecutive_ok: 2\n    timeout: 5s")
	def(&o.Probes, "    - {name: shop-health, url: "+shop+"/health, interval: 200ms, timeout: 1s, expect_status: 200}")
	def(&o.Hypothesis, "    error_rate_max: 0.05\n    p95_latency_ms_max: 1500\n    probe_success_min: 0.95")
	def(&o.Recovery, "10s")
	def(&o.Faults, always503)
	if o.RPS == 0 {
		o.RPS = 20
	}
	var b bytes.Buffer
	if err := expTemplate.Execute(&b, o); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// faultedThroughProxy reports whether a scoped request to the inventory route is currently faulted.
func (f *fullStack) faultedThroughProxy(runID string) bool {
	res := call("GET", f.proxy.url("/inventory/stock/1"), scoped(runID), "")
	return res.status == http.StatusServiceUnavailable
}

func (f *fullStack) proxyEvents(t testing.TB) []string {
	t.Helper()
	var stats struct {
		Events []struct{ Type string }
	}
	f.proxy.adminCall(t, "GET", "/admin/v1/stats", nil, &stats)
	var out []string
	for _, e := range stats.Events {
		out = append(out, e.Type)
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
