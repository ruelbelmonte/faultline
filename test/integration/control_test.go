//go:build integration

package integration

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var rfc3339Seconds = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`)

func TestRunPassed(t *testing.T) {
	f := startFull(t, "naive")
	// 30% of inventory calls fail; the naive shop retries 3 times, so orders still succeed.
	yaml := f.experiment(t, expOpts{Name: "pass", Faults: `  - id: f1
    type: http_error
    match: {method: GET, path: "/stock/*"}
    strategy: {kind: probability, p: 0.3}
    params: {status: 503}`})
	id := f.control.createRun(t, yaml)

	live := f.control.run(t, id)
	if live.Verdict != nil || live.EndedAt != nil || terminal(live.Status) {
		t.Fatalf("verdict and ended_at must be null while the run is in progress: %+v", live)
	}
	doc := f.control.waitDone(t, id, 30*time.Second)

	if doc.Status != "completed" || doc.verdict() != "passed" || doc.Reason != nil {
		t.Fatalf("expected completed/passed, got %s/%s (%s)", doc.Status, doc.verdict(), doc.reason())
	}
	if doc.SchemaVersion != "1" || doc.RunID != id || doc.Environment != "local" {
		t.Fatalf("unexpected header fields: %+v", doc)
	}
	if doc.Experiment.Name != "pass" || doc.Experiment.Description == "" || len(doc.Experiment.YAMLSHA256) != 64 {
		t.Fatalf("unexpected experiment block: %+v", doc.Experiment)
	}
	if !rfc3339Seconds.MatchString(doc.StartedAt) || doc.EndedAt == nil || !rfc3339Seconds.MatchString(*doc.EndedAt) {
		t.Fatalf("timestamps must be RFC 3339: %s / %v", doc.StartedAt, doc.EndedAt)
	}
	if len(doc.Hypothesis) != 3 {
		t.Fatalf("expected 3 hypothesis entries, got %+v", doc.Hypothesis)
	}
	for _, h := range doc.Hypothesis {
		if !h.Passed || h.Observed == nil {
			t.Fatalf("hypothesis %s did not pass: %+v", h.Name, h)
		}
	}
	if len(doc.Faults) != 1 || doc.Faults[0].ID != "f1" || doc.Faults[0].Type != "http_error" || doc.Faults[0].Route != "inventory" {
		t.Fatalf("unexpected faults: %+v", doc.Faults)
	}
	if doc.Faults[0].Triggers < 5 || doc.Faults[0].StartAt == nil || doc.Faults[0].EndAt == nil {
		t.Fatalf("fault should have triggered: %+v", doc.Faults[0])
	}
	m := doc.Metrics
	if m.Driver.Requests < 40 || m.Driver.Requests > 70 {
		t.Fatalf("20 rps for 3s should send about 60 requests, sent %d", m.Driver.Requests)
	}
	if m.Driver.ErrorRate > 0.05 || m.Driver.P50Ms <= 0 || m.Driver.P95Ms < m.Driver.P50Ms {
		t.Fatalf("unexpected driver metrics: %+v", m.Driver)
	}
	inv := m.Proxy["inventory"]
	if inv.Requests < m.Driver.Requests || int64(inv.Faulted) != doc.Faults[0].Triggers {
		t.Fatalf("proxy metrics inconsistent: %+v vs driver %d, triggers %d", inv, m.Driver.Requests, doc.Faults[0].Triggers)
	}
	if doc.RecoverySeconds == nil || *doc.RecoverySeconds > 5 {
		t.Fatalf("a healthy system should recover quickly: %v", doc.RecoverySeconds)
	}
	if !doc.Redaction.Enabled || !contains(doc.Redaction.Headers, "authorization") {
		t.Fatalf("unexpected redaction block: %+v", doc.Redaction)
	}
	if doc.Findings == nil {
		t.Fatal("findings must be a list, not null")
	}

	// Faults are gone and the proxy never lost the lease.
	if a := f.proxy.active(t); a.RunID != nil {
		t.Fatalf("faults still installed after the run: %+v", a)
	}
	if ev := f.proxyEvents(t); contains(ev, "lease_expired") {
		t.Fatalf("the lease expired during a healthy run: %v", ev)
	}

	t.Run("runs list", func(t *testing.T) {
		var runs []map[string]any
		f.control.api(t, "GET", "/api/v1/runs", nil, &runs)
		if len(runs) != 1 || runs[0]["run_id"] != id || runs[0]["experiment"] != "pass" || runs[0]["status"] != "completed" || runs[0]["verdict"] != "passed" {
			t.Fatalf("unexpected runs list: %v", runs)
		}
		if !rfc3339Seconds.MatchString(runs[0]["started_at"].(string)) {
			t.Fatalf("started_at: %v", runs[0]["started_at"])
		}
	})

	t.Run("timeseries", func(t *testing.T) {
		var ts struct {
			BucketSeconds int `json:"bucket_seconds"`
			Series        []struct {
				T      string
				Driver struct {
					Requests, Errors int
					P95Ms            float64 `json:"p95_ms"`
				}
				Proxy  struct{ Requests, Faulted int }
				Probes struct{ OK, Fail int }
			}
			Phases       []struct{ Name, Start, End string }
			FaultWindows []struct {
				FaultID    string `json:"fault_id"`
				Start, End string
			} `json:"fault_windows"`
			Markers []struct{ Type, T, Detail string }
		}
		if code := f.control.api(t, "GET", "/api/v1/runs/"+id+"/timeseries", nil, &ts); code != 200 {
			t.Fatalf("status %d", code)
		}
		if ts.BucketSeconds != 1 || len(ts.Series) < 4 {
			t.Fatalf("unexpected series: bucket_seconds=%d, %d points", ts.BucketSeconds, len(ts.Series))
		}
		var req, faulted, probes int
		for i, p := range ts.Series {
			if !rfc3339Seconds.MatchString(p.T) {
				t.Fatalf("series t: %q", p.T)
			}
			if i > 0 {
				prev, _ := time.Parse(time.RFC3339, ts.Series[i-1].T)
				cur, _ := time.Parse(time.RFC3339, p.T)
				if cur.Sub(prev) != time.Second {
					t.Fatalf("buckets must be one second apart: %s then %s", ts.Series[i-1].T, p.T)
				}
			}
			req += p.Driver.Requests
			faulted += p.Proxy.Faulted
			probes += p.Probes.OK
		}
		if req < m.Driver.Requests || int64(faulted) != doc.Faults[0].Triggers || probes == 0 {
			t.Fatalf("series totals: requests %d, faulted %d, probes ok %d", req, faulted, probes)
		}
		var names []string
		for _, p := range ts.Phases {
			names = append(names, p.Name)
			if p.Start == "" || p.End == "" {
				t.Fatalf("phase without bounds: %+v", p)
			}
		}
		if strings.Join(names, ",") != "prechecking,injecting,recovering" {
			t.Fatalf("phases: %v", names)
		}
		if len(ts.FaultWindows) != 1 || ts.FaultWindows[0].FaultID != "f1" || len(ts.Markers) != 0 {
			t.Fatalf("fault windows %+v, markers %+v", ts.FaultWindows, ts.Markers)
		}
	})

	t.Run("audit", func(t *testing.T) {
		var audit []struct {
			TS, Actor, Action string
			RunID             string `json:"run_id"`
			Detail            map[string]any
		}
		f.control.api(t, "GET", "/api/v1/audit", nil, &audit)
		if len(audit) != 1 || audit[0].Action != "run_created" || audit[0].Actor != "api-token" || audit[0].RunID != id {
			t.Fatalf("unexpected audit log: %+v", audit)
		}
		data, err := os.ReadFile(filepath.Join(f.s.dataDir, "audit.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &line); err != nil || line["action"] != "run_created" {
			t.Fatalf("audit.jsonl: %q (%v)", data, err)
		}
	})

	t.Run("unknown run", func(t *testing.T) {
		for _, p := range []string{"", "/timeseries", "/events"} {
			if code := f.control.api(t, "GET", "/api/v1/runs/run_zzzzzzzzzzzz"+p, nil, nil); code != 404 {
				t.Errorf("GET unknown run%s: expected 404, got %d", p, code)
			}
		}
		if code := f.control.api(t, "POST", "/api/v1/runs/run_zzzzzzzzzzzz/abort", nil, nil); code != 404 {
			t.Errorf("abort unknown run: expected 404, got %d", code)
		}
		if code := f.control.api(t, "POST", "/api/v1/runs/"+id+"/abort", nil, nil); code != 409 {
			t.Errorf("abort a finished run: expected 409, got %d", code)
		}
	})
}

func TestRunFailed(t *testing.T) {
	// lease_timeout is shorter than the injection, so the run only works if control renews the lease.
	f := startFull(t, "naive", func(s *stack) { s.leaseTimeout = "1s" })
	id := f.control.createRun(t, f.experiment(t, expOpts{Name: "fail", Duration: "4s"}))

	// While injecting, traffic that does not carry the run id is untouched.
	f.control.waitStatus(t, id, 15*time.Second, "injecting")
	time.Sleep(500 * time.Millisecond)
	for _, h := range []map[string]string{nil, {"X-Chaos-Run": "run_bbbbbbbbbbbb"}} {
		if resp, _ := get(t, "http://"+f.demo.shop+"/api/orders/1", h); resp.StatusCode != 200 {
			t.Fatalf("traffic outside the run was affected: status %d with headers %v", resp.StatusCode, h)
		}
	}
	if resp, _ := get(t, "http://"+f.demo.shop+"/api/orders/1", scoped(id)); resp.StatusCode != 502 {
		t.Fatalf("traffic of the run should be faulted, got %d", resp.StatusCode)
	}

	doc := f.control.waitDone(t, id, 40*time.Second)
	if doc.Status != "completed" || doc.verdict() != "failed" || doc.Reason != nil {
		t.Fatalf("expected completed/failed, got %s/%s (%s)", doc.Status, doc.verdict(), doc.reason())
	}
	obs, passed, found := doc.hypothesis("error_rate_max")
	if !found || passed || obs == nil || *obs < 0.95 {
		t.Fatalf("error_rate_max should be breached at about 1.0: %v %v", obs, passed)
	}
	if doc.Metrics.Driver.ErrorRate != *obs {
		t.Fatalf("metrics.driver.error_rate (%v) and the hypothesis observation (%v) must agree", doc.Metrics.Driver.ErrorRate, *obs)
	}
	if _, passed, _ := doc.hypothesis("probe_success_min"); !passed {
		t.Fatal("the health probe is unaffected by the fault and should pass")
	}
	// Each order makes 4 attempts in naive mode.
	inv := doc.Metrics.Proxy["inventory"]
	if inv.Faulted != inv.Requests || inv.Requests < 3*doc.Metrics.Driver.Requests {
		t.Fatalf("expected every inventory call faulted with retries: %+v for %d orders", inv, doc.Metrics.Driver.Requests)
	}
	if ev := f.proxyEvents(t); contains(ev, "lease_expired") {
		t.Fatalf("control did not keep the lease alive: %v", ev)
	}
	if doc.RecoverySeconds == nil {
		t.Fatal("the system recovers as soon as faults clear; recovery_seconds must be set")
	}
}

func TestRunInconclusiveUnhealthyBeforeStart(t *testing.T) {
	f := startFull(t, "naive")
	yaml := f.experiment(t, expOpts{
		Name:     "unhealthy",
		Precheck: "    consecutive_ok: 3\n    timeout: 1500ms",
		Probes:   "    - {name: dead, url: http://" + freeAddr(t) + "/health, interval: 200ms, timeout: 300ms}",
	})
	begin := time.Now()
	id := f.control.createRun(t, yaml)
	doc := f.control.waitDone(t, id, 20*time.Second)
	if doc.Status != "completed" || doc.verdict() != "inconclusive" || doc.reason() != "unhealthy_before_start" {
		t.Fatalf("got %s/%s (%s)", doc.Status, doc.verdict(), doc.reason())
	}
	if took := time.Since(begin); took < 1400*time.Millisecond || took > 6*time.Second {
		t.Fatalf("precheck should give up after its 1.5s timeout, took %s", took)
	}
	// No faults were ever installed and no traffic was driven.
	if ev := f.proxyEvents(t); len(ev) != 0 {
		t.Fatalf("the proxy must not have been touched: %v", ev)
	}
	if doc.Faults[0].StartAt != nil || doc.Faults[0].Triggers != 0 || doc.Metrics.Driver.Requests != 0 {
		t.Fatalf("unexpected activity: %+v %+v", doc.Faults, doc.Metrics.Driver)
	}
	if doc.RecoverySeconds != nil {
		t.Fatalf("recovery_seconds must be null: %v", *doc.RecoverySeconds)
	}
}

func TestRunInconclusiveFaultNotTriggered(t *testing.T) {
	f := startFull(t, "naive")
	yaml := f.experiment(t, expOpts{Name: "untriggered", Duration: "2s", Faults: `  - id: hit
    type: latency
    match: {path: "/stock/*"}
    params: {ms: 5}
  - id: miss
    type: http_error
    match: {path: "/never/*"}
    params: {status: 503}`})
	doc := f.control.waitDone(t, f.control.createRun(t, yaml), 30*time.Second)
	if doc.Status != "completed" || doc.verdict() != "inconclusive" || doc.reason() != "fault_not_triggered" {
		t.Fatalf("got %s/%s (%s)", doc.Status, doc.verdict(), doc.reason())
	}
	if doc.Faults[0].Triggers == 0 || doc.Faults[1].Triggers != 0 {
		t.Fatalf("unexpected triggers: %+v", doc.Faults)
	}
}

// Rule order of PRD 9.4: an untriggered fault outranks a breached hypothesis.
func TestVerdictOrderUntriggeredBeforeBreach(t *testing.T) {
	f := startFull(t, "naive")
	yaml := f.experiment(t, expOpts{Name: "order", Duration: "2s", Faults: always503 + `
  - id: miss
    type: reset
    match: {path: "/never/*"}`})
	doc := f.control.waitDone(t, f.control.createRun(t, yaml), 30*time.Second)
	if doc.verdict() != "inconclusive" || doc.reason() != "fault_not_triggered" {
		t.Fatalf("got %s (%s)", doc.verdict(), doc.reason())
	}
	if _, passed, _ := doc.hypothesis("error_rate_max"); passed {
		t.Fatal("test setup: the hypothesis should be breached")
	}
}

func TestHypothesisWithoutDataFails(t *testing.T) {
	f := startFull(t, "naive")
	yaml := f.experiment(t, expOpts{Name: "nodata", Duration: "2s", NoTraffic: true, Scope: "header",
		Faults: `  - id: f1
    type: latency
    route: llm
    params: {ms: 1}`})
	id := f.control.createRun(t, yaml)
	f.control.waitStatus(t, id, 15*time.Second, "injecting")
	// The fault needs one trigger, or the verdict would be inconclusive.
	eventually(t, 5*time.Second, "fault triggered", func() bool {
		call("GET", f.proxy.url("/llm/health"), scoped(id), "")
		return f.control.run(t, id).Faults[0].Triggers > 0
	})
	doc := f.control.waitDone(t, id, 30*time.Second)
	if doc.verdict() != "failed" {
		t.Fatalf("got %s (%s)", doc.verdict(), doc.reason())
	}
	for _, name := range []string{"error_rate_max", "p95_latency_ms_max"} {
		if obs, passed, found := doc.hypothesis(name); !found || obs != nil || passed {
			t.Fatalf("%s: with no driver data observed must be null and passed false, got %v %v", name, obs, passed)
		}
	}
	if obs, passed, _ := doc.hypothesis("probe_success_min"); obs == nil || !passed {
		t.Fatal("probes ran, so probe_success_min has data")
	}
}

func longRun(f *fullStack, t testing.TB, o expOpts) string {
	if o.Duration == "" {
		o.Duration = "60s"
	}
	if o.Hypothesis == "" {
		o.Hypothesis = "    error_rate_max: 1"
	}
	return f.experiment(t, o)
}

func assertAborted(t *testing.T, f *fullStack, id, reason string) runDoc {
	t.Helper()
	doc := f.control.run(t, id)
	if doc.Status != "aborted" || doc.verdict() != "aborted" || doc.reason() != reason {
		t.Fatalf("expected aborted/aborted (%s), got %s/%s (%s)", reason, doc.Status, doc.verdict(), doc.reason())
	}
	if doc.EndedAt == nil {
		t.Fatal("ended_at must be set")
	}
	if a := f.proxy.active(t); a.RunID != nil {
		t.Fatalf("faults are still installed after an abort: %+v", a)
	}
	if f.faultedThroughProxy(id) {
		t.Fatal("requests are still being faulted after an abort")
	}
	var ts struct {
		Markers []struct{ Type, T, Detail string }
	}
	f.control.api(t, "GET", "/api/v1/runs/"+id+"/timeseries", nil, &ts)
	if len(ts.Markers) != 1 || ts.Markers[0].Type != "abort" || ts.Markers[0].Detail != reason {
		t.Fatalf("expected one abort marker, got %+v", ts.Markers)
	}
	var audit []struct {
		Action string
		RunID  string `json:"run_id"`
		Detail map[string]any
	}
	f.control.api(t, "GET", "/api/v1/audit", nil, &audit)
	found := false
	for _, a := range audit {
		if a.Action == "run_aborted" && a.RunID == id && a.Detail["reason"] == reason {
			found = true
		}
	}
	if !found {
		t.Fatalf("no run_aborted audit entry: %+v", audit)
	}
	return doc
}

func TestAbortManual(t *testing.T) {
	f := startFull(t, "naive")
	id := f.control.createRun(t, longRun(f, t, expOpts{Name: "manual"}))
	f.control.waitStatus(t, id, 15*time.Second, "injecting")
	eventually(t, 5*time.Second, "fault active", func() bool { return f.faultedThroughProxy(id) })
	eventually(t, 5*time.Second, "driver traffic measured", func() bool { return f.control.run(t, id).Metrics.Driver.Requests > 0 })

	var resp map[string]string
	if code := f.control.api(t, "POST", "/api/v1/runs/"+id+"/abort", nil, &resp); code != 200 || resp["status"] != "aborted" {
		t.Fatalf("abort: %d %v", code, resp)
	}
	doc := assertAborted(t, f, id, "manual")
	if doc.Faults[0].Triggers == 0 || doc.Metrics.Driver.Requests == 0 {
		t.Fatalf("an aborted run keeps what it measured: %+v", doc)
	}

	// The slot is free again.
	id2 := f.control.createRun(t, f.experiment(t, expOpts{Name: "after-abort", Duration: "1s"}))
	f.control.waitDone(t, id2, 30*time.Second)
}

func TestAbortDuringPrecheck(t *testing.T) {
	f := startFull(t, "naive")
	yaml := f.experiment(t, expOpts{
		Name:     "abort-precheck",
		Precheck: "    consecutive_ok: 3\n    timeout: 60s",
		Probes:   "    - {name: dead, url: http://" + freeAddr(t) + "/health, interval: 200ms, timeout: 300ms}",
	})
	id := f.control.createRun(t, yaml)
	f.control.waitStatus(t, id, 10*time.Second, "prechecking")
	if code := f.control.api(t, "POST", "/api/v1/runs/"+id+"/abort", nil, nil); code != 200 {
		t.Fatalf("abort: %d", code)
	}
	assertAborted(t, f, id, "manual")
}

func TestAbortKillSwitch(t *testing.T) {
	f := startFull(t, "naive")
	id := f.control.createRun(t, longRun(f, t, expOpts{Name: "kill"}))
	f.control.waitStatus(t, id, 15*time.Second, "injecting")
	eventually(t, 5*time.Second, "fault active", func() bool { return f.faultedThroughProxy(id) })

	begin := time.Now()
	var resp map[string]any
	if code := f.control.api(t, "POST", "/api/v1/kill", nil, &resp); code != 200 || resp["cleared"] != true {
		t.Fatalf("kill: %d %v", code, resp)
	}
	if f.faultedThroughProxy(id) {
		t.Fatal("a request right after the kill switch was still faulted")
	}
	if took := time.Since(begin); took > time.Second {
		t.Fatalf("kill switch took %s", took)
	}
	assertAborted(t, f, id, "kill_switch")

	var audit []struct{ Action string }
	f.control.api(t, "GET", "/api/v1/audit?limit=50", nil, &audit)
	found := false
	for _, a := range audit {
		found = found || a.Action == "kill_switch"
	}
	if !found {
		t.Fatalf("no kill_switch audit entry: %+v", audit)
	}

	// With nothing running the kill switch still answers and still clears the proxy.
	f.proxy.install(t, "run_cccccccccccc", "all", flt("stray", "http_error", "inventory", obj{"status": 503}))
	if code := f.control.api(t, "POST", "/api/v1/kill", nil, &resp); code != 200 || resp["cleared"] != true {
		t.Fatalf("kill with no run: %d %v", code, resp)
	}
	if a := f.proxy.active(t); a.RunID != nil {
		t.Fatal("the kill switch must clear faults it did not install")
	}
}

func TestAbortThresholdErrorRate(t *testing.T) {
	f := startFull(t, "naive")
	id := f.control.createRun(t, longRun(f, t, expOpts{Name: "threshold-errors", Abort: "  error_rate_max: 0.5"}))
	begin := time.Now()
	f.control.waitStatus(t, id, 30*time.Second, "aborted")
	if took := time.Since(begin); took > 12*time.Second {
		t.Fatalf("the abort threshold should trip within seconds, took %s", took)
	}
	assertAborted(t, f, id, "threshold:error_rate_max")
}

func TestAbortThresholdNotTrippedBelowLimit(t *testing.T) {
	f := startFull(t, "naive")
	// About 0.8% of orders fail; a 50% abort threshold must not trip.
	yaml := f.experiment(t, expOpts{Name: "threshold-ok", Abort: "  error_rate_max: 0.5\n  probe_failures_consecutive: 3", Faults: `  - id: f1
    type: http_error
    match: {path: "/stock/*"}
    strategy: {kind: probability, p: 0.3}
    params: {status: 503}`})
	doc := f.control.waitDone(t, f.control.createRun(t, yaml), 30*time.Second)
	if doc.Status != "completed" || doc.verdict() != "passed" {
		t.Fatalf("got %s/%s (%s)", doc.Status, doc.verdict(), doc.reason())
	}
}

func TestAbortThresholdProbeFailures(t *testing.T) {
	f := startFull(t, "naive")
	// The probe goes through the faulted dependency. Probes carry the run id, so they see the fault.
	yaml := longRun(f, t, expOpts{
		Name:   "threshold-probes",
		Probes: "    - {name: order, url: http://" + f.demo.shop + "/api/orders/1, interval: 200ms, timeout: 2s}",
		Abort:  "  probe_failures_consecutive: 4",
	})
	id := f.control.createRun(t, yaml)
	f.control.waitStatus(t, id, 30*time.Second, "aborted")
	assertAborted(t, f, id, "threshold:probe_failures_consecutive")
}

// The safety property of the whole design: if control dies, faults clear on
// their own. Control is killed the hard way, with no chance to clean up.
func TestControlKilledFaultsClearByLease(t *testing.T) {
	f := startFull(t, "naive", func(s *stack) { s.leaseTimeout = "2s" })
	id := f.control.createRun(t, longRun(f, t, expOpts{Name: "killed"}))
	f.control.waitStatus(t, id, 15*time.Second, "injecting")
	eventually(t, 5*time.Second, "fault active", func() bool { return f.faultedThroughProxy(id) })

	f.control.kill()
	killed := time.Now()
	if !f.faultedThroughProxy(id) {
		t.Fatal("control had no chance to clear anything; the fault should still be active right after the kill")
	}
	if a := f.proxy.active(t); a.RunID == nil || *a.RunID != id {
		t.Fatalf("expected the run's faults to still be installed: %+v", a)
	}

	eventually(t, 6*time.Second, "faults cleared by lease expiry", func() bool { return !f.faultedThroughProxy(id) })
	if took := time.Since(killed); took > 3500*time.Millisecond {
		t.Fatalf("faults outlived the 2s lease by too much: cleared after %s", took)
	}
	if ev := f.proxyEvents(t); !contains(ev, "lease_expired") {
		t.Fatalf("the proxy did not record a lease_expired event: %v", ev)
	}
	if a := f.proxy.active(t); a.RunID != nil || len(a.Faults) != 0 {
		t.Fatalf("expected no active faults: %+v", a)
	}
	// The data path never went down.
	if resp, _ := get(t, "http://"+f.demo.shop+"/api/orders/1", scoped(id)); resp.StatusCode != 200 {
		t.Fatalf("the proxy must keep serving after control dies: %d", resp.StatusCode)
	}

	// A new control process finds the orphaned run and closes it.
	f.control = f.s.startControl(t)
	doc := f.control.run(t, id)
	if doc.Status != "aborted" || doc.verdict() != "aborted" || doc.reason() != "control_restarted" || doc.EndedAt == nil {
		t.Fatalf("expected aborted (control_restarted), got %s/%s (%s)", doc.Status, doc.verdict(), doc.reason())
	}
	var runs []map[string]any
	f.control.api(t, "GET", "/api/v1/runs", nil, &runs)
	if len(runs) != 1 || runs[0]["status"] != "aborted" || runs[0]["verdict"] != "aborted" {
		t.Fatalf("runs list after restart: %v", runs)
	}
	f.control.waitDone(t, f.control.createRun(t, f.experiment(t, expOpts{Name: "after-restart", Duration: "1s"})), 30*time.Second)
}

// A restart while faults are still installed must clear them at once rather
// than wait for the lease.
func TestControlRestartClearsFaultsImmediately(t *testing.T) {
	f := startFull(t, "naive", func(s *stack) { s.leaseTimeout = "30s" })
	id := f.control.createRun(t, longRun(f, t, expOpts{Name: "restart"}))
	f.control.waitStatus(t, id, 15*time.Second, "injecting")
	eventually(t, 5*time.Second, "fault active", func() bool { return f.faultedThroughProxy(id) })
	f.control.kill()
	f.control = f.s.startControl(t)
	if f.faultedThroughProxy(id) {
		t.Fatal("control came back up and left the orphaned faults in place")
	}
	if doc := f.control.run(t, id); doc.reason() != "control_restarted" {
		t.Fatalf("got %s (%s)", doc.Status, doc.reason())
	}
}

func TestRunCreationErrors(t *testing.T) {
	f := startFull(t, "naive")

	t.Run("invalid experiment", func(t *testing.T) {
		var resp struct {
			Error  string
			Errors []struct{ Path, Message string }
		}
		bad := strings.Replace(f.experiment(t, expOpts{}), "type: http_error", "type: gremlin", 1)
		if code := f.control.api(t, "POST", "/api/v1/runs", obj{"experiment_yaml": bad}, &resp); code != 400 {
			t.Fatalf("expected 400, got %d", code)
		}
		if len(resp.Errors) != 1 || resp.Errors[0].Path != "faults[0].type" || resp.Errors[0].Message == "" {
			t.Fatalf("expected one error at faults[0].type, got %+v", resp.Errors)
		}
	})

	t.Run("scope all is rejected by default", func(t *testing.T) {
		var resp struct {
			Errors []struct{ Path, Message string }
		}
		code := f.control.api(t, "POST", "/api/v1/runs", obj{"experiment_yaml": f.experiment(t, expOpts{Scope: "all"})}, &resp)
		if code != 400 || len(resp.Errors) != 1 || resp.Errors[0].Path != "scope" {
			t.Fatalf("expected 400 at scope, got %d %+v", code, resp.Errors)
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		req, _ := http.NewRequest("POST", f.control.url("/api/v1/runs"), strings.NewReader("{nope"))
		req.Header.Set("Authorization", "Bearer "+controlToken)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("expected 400, got %d", resp.StatusCode)
		}
	})

	t.Run("only one run at a time", func(t *testing.T) {
		id := f.control.createRun(t, longRun(f, t, expOpts{Name: "first"}))
		var resp map[string]string
		code := f.control.api(t, "POST", "/api/v1/runs", obj{"experiment_yaml": f.experiment(t, expOpts{Name: "second"})}, &resp)
		if code != 409 {
			t.Fatalf("expected 409 while a run is active, got %d %v", code, resp)
		}
		f.control.api(t, "POST", "/api/v1/runs/"+id+"/abort", nil, nil)
		var runs []map[string]any
		f.control.api(t, "GET", "/api/v1/runs", nil, &runs)
		if len(runs) != 1 {
			t.Fatalf("the rejected run must not be stored: %v", runs)
		}
	})
}

func TestScopeAllWhenAllowed(t *testing.T) {
	f := startFull(t, "naive", func(s *stack) { s.controlExtra = "  allow_unscoped: true\n" })
	id := f.control.createRun(t, longRun(f, t, expOpts{Name: "unscoped", Scope: "all"}))
	f.control.waitStatus(t, id, 15*time.Second, "injecting")
	eventually(t, 5*time.Second, "unscoped traffic faulted", func() bool {
		resp, _ := get(t, "http://"+f.demo.shop+"/api/orders/1", nil)
		return resp.StatusCode == 502
	})
	f.control.api(t, "POST", "/api/v1/kill", nil, nil)
	if resp, _ := get(t, "http://"+f.demo.shop+"/api/orders/1", nil); resp.StatusCode != 200 {
		t.Fatalf("after the kill switch unscoped traffic must pass: %d", resp.StatusCode)
	}
}

func TestControlAuth(t *testing.T) {
	f := startFull(t, "naive")
	var health map[string]bool
	if code := apiCall(t, "GET", f.control.url("/healthz"), "", nil, &health); code != 200 || !health["ok"] {
		t.Fatalf("healthz: %d %v", code, health)
	}
	if res := call("GET", f.control.url("/"), nil, ""); res.status != 200 || !strings.Contains(res.header.Get("Content-Type"), "text/html") {
		t.Fatalf("the UI shell must be public: %d %s", res.status, res.header.Get("Content-Type"))
	}
	endpoints := []struct{ method, path string }{
		{"GET", "/api/v1/routes"},
		{"GET", "/api/v1/experiments"},
		{"GET", "/api/v1/experiments/example"},
		{"PUT", "/api/v1/experiments/example"},
		{"POST", "/api/v1/experiments/validate"},
		{"POST", "/api/v1/runs"},
		{"GET", "/api/v1/runs"},
		{"GET", "/api/v1/runs/run_aaaaaaaaaaaa"},
		{"GET", "/api/v1/runs/run_aaaaaaaaaaaa/timeseries"},
		{"GET", "/api/v1/runs/run_aaaaaaaaaaaa/events"},
		{"POST", "/api/v1/runs/run_aaaaaaaaaaaa/abort"},
		{"POST", "/api/v1/kill"},
		{"GET", "/api/v1/faults/active"},
		{"GET", "/api/v1/runs/run_aaaaaaaaaaaa/report?format=json"},
		{"GET", "/api/v1/audit"},
		{"GET", "/api/v1/nope"},
	}
	for _, e := range endpoints {
		for _, tok := range []string{"", "wrong-token-0123456789", controlToken + "x", adminToken} {
			var body map[string]string
			if code := apiCall(t, e.method, f.control.url(e.path), tok, obj{}, &body); code != 401 || body["error"] == "" {
				t.Errorf("%s %s with token %q: expected 401, got %d", e.method, e.path, tok, code)
			}
		}
	}
	// Nothing was created, saved or cleared by the rejected calls.
	var audit []any
	f.control.api(t, "GET", "/api/v1/audit", nil, &audit)
	if len(audit) != 0 {
		t.Fatalf("unauthorised calls left audit entries: %v", audit)
	}

	var routes []map[string]string
	f.control.api(t, "GET", "/api/v1/routes", nil, &routes)
	names := map[string]string{}
	for _, r := range routes {
		names[r["name"]] = r["upstream"]
	}
	if len(routes) != 3 || names["inventory"] != "http://"+f.demo.inventory {
		t.Fatalf("routes: %v", routes)
	}

	var active map[string]any
	if code := f.control.api(t, "GET", "/api/v1/faults/active", nil, &active); code != 200 || active["run_id"] != nil || len(active["faults"].([]any)) != 0 {
		t.Fatalf("faults/active pass-through: %d %v", code, active)
	}
}

func TestExperimentsAPI(t *testing.T) {
	f := startFull(t, "naive")
	good := f.experiment(t, expOpts{Name: "saved"})

	var list []map[string]any
	if code := f.control.api(t, "GET", "/api/v1/experiments", nil, &list); code != 200 || len(list) != 0 {
		t.Fatalf("empty experiments dir: %d %v", code, list)
	}

	var val struct {
		Valid  bool
		Errors []struct{ Path, Message string }
	}
	if code := f.control.api(t, "POST", "/api/v1/experiments/validate", obj{"yaml": good}, &val); code != 200 || !val.Valid || val.Errors == nil || len(val.Errors) != 0 {
		t.Fatalf("validate good: %d %+v", code, val)
	}
	bad := strings.Replace(good, "duration: 3s", "duration: 3 parsecs", 1) + "surprise: 1\n"
	if code := f.control.api(t, "POST", "/api/v1/experiments/validate", obj{"yaml": bad}, &val); code != 200 || val.Valid || len(val.Errors) != 2 {
		t.Fatalf("validate bad must still answer 200: %d %+v", code, val)
	}
	if code := f.control.api(t, "POST", "/api/v1/experiments/validate", obj{"yaml": ":::"}, &val); code != 200 || val.Valid {
		t.Fatalf("validate garbage: %d %+v", code, val)
	}

	if code := f.control.api(t, "PUT", "/api/v1/experiments/saved", obj{"yaml": good}, nil); code != 200 {
		t.Fatalf("PUT: %d", code)
	}
	onDisk, err := os.ReadFile(filepath.Join(f.s.dir, "experiments", "saved.yaml"))
	if err != nil || string(onDisk) != good {
		t.Fatalf("file not written as submitted: %v", err)
	}
	var got map[string]string
	if code := f.control.api(t, "GET", "/api/v1/experiments/saved", nil, &got); code != 200 || got["name"] != "saved" || got["file"] != "saved.yaml" || got["yaml"] != good {
		t.Fatalf("GET: %d %v", code, got)
	}

	var errResp struct {
		Errors []struct{ Path, Message string }
	}
	if code := f.control.api(t, "PUT", "/api/v1/experiments/broken", obj{"yaml": bad}, &errResp); code != 400 || len(errResp.Errors) == 0 {
		t.Fatalf("PUT invalid: %d %+v", code, errResp)
	}
	if _, err := os.Stat(filepath.Join(f.s.dir, "experiments", "broken.yaml")); err == nil {
		t.Fatal("an invalid experiment was written to disk")
	}
	for _, name := range []string{"Bad_Name", "..%2F..%2Fescape", "a.b"} {
		if code := f.control.api(t, "PUT", "/api/v1/experiments/"+name, obj{"yaml": good}, nil); code != 400 {
			t.Errorf("PUT %s: expected 400, got %d", name, code)
		}
	}
	if code := f.control.api(t, "GET", "/api/v1/experiments/missing", nil, nil); code != 404 {
		t.Fatalf("GET missing: %d", code)
	}

	os.WriteFile(filepath.Join(f.s.dir, "experiments", "invalid.yml"), []byte("version: 1\n"), 0o644)
	os.WriteFile(filepath.Join(f.s.dir, "experiments", "notes.txt"), []byte("not an experiment"), 0o644)
	f.control.api(t, "GET", "/api/v1/experiments", nil, &list)
	if len(list) != 2 || list[0]["name"] != "invalid" || list[0]["file"] != "invalid.yml" || list[0]["valid"] != false ||
		list[1]["name"] != "saved" || list[1]["valid"] != true {
		t.Fatalf("experiments list: %v", list)
	}

	var audit []struct {
		Action string
		Detail map[string]any
	}
	f.control.api(t, "GET", "/api/v1/audit", nil, &audit)
	if len(audit) != 1 || audit[0].Action != "experiment_saved" || audit[0].Detail["name"] != "saved" {
		t.Fatalf("audit: %+v", audit)
	}
}

func TestAuditOrderAndLimit(t *testing.T) {
	f := startFull(t, "naive")
	good := f.experiment(t, expOpts{})
	for _, name := range []string{"a", "b", "c"} {
		f.control.api(t, "PUT", "/api/v1/experiments/"+name, obj{"yaml": good}, nil)
	}
	var audit []struct{ Detail map[string]any }
	f.control.api(t, "GET", "/api/v1/audit?limit=2", nil, &audit)
	if len(audit) != 2 || audit[0].Detail["name"] != "c" || audit[1].Detail["name"] != "b" {
		t.Fatalf("expected the two newest entries, newest first: %+v", audit)
	}
	if code := f.control.api(t, "GET", "/api/v1/audit?limit=zero", nil, nil); code != 400 {
		t.Fatalf("invalid limit: expected 400, got %d", code)
	}
	lines, _ := os.ReadFile(filepath.Join(f.s.dataDir, "audit.jsonl"))
	if n := strings.Count(string(lines), "\n"); n != 3 {
		t.Fatalf("audit.jsonl should have 3 lines, has %d", n)
	}
}

func TestRunEventsStream(t *testing.T) {
	f := startFull(t, "naive")
	id := f.control.createRun(t, f.experiment(t, expOpts{Name: "events", Duration: "2s"}))

	req, _ := http.NewRequest("GET", f.control.url("/api/v1/runs/"+id+"/events"), nil)
	req.Header.Set("Authorization", "Bearer "+controlToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q", ct)
	}
	events, err := readSSE(t, resp, time.Now())
	if err != nil {
		t.Fatalf("the stream should end cleanly when the run does: %v", err)
	}
	var statuses []string
	metrics := 0
	for _, e := range events {
		switch e.name {
		case "status":
			statuses = append(statuses, e.data["status"].(string))
			if e.data["run_id"] != id {
				t.Fatalf("status event for another run: %v", e.data)
			}
		case "metrics":
			metrics++
			for _, k := range []string{"t", "driver", "proxy", "probes"} {
				if _, ok := e.data[k]; !ok {
					t.Fatalf("metrics event lacks %q: %v", k, e.data)
				}
			}
		}
	}
	got := strings.Join(statuses, ",")
	if !strings.HasSuffix(got, "injecting,recovering,completed") {
		t.Fatalf("status events: %s", got)
	}
	if metrics < 2 {
		t.Fatalf("expected a metrics event per second, got %d", metrics)
	}
	last := events[len(events)-1]
	if last.name != "status" || last.data["verdict"] == nil {
		t.Fatalf("the last event must be the final status with a verdict: %+v", last)
	}

	// A finished run replays its outcome and ends.
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	replay, err := readSSE(t, resp2, time.Now())
	if err != nil || len(replay) == 0 || replay[len(replay)-1].data["status"] != "completed" {
		t.Fatalf("replay for a finished run: %+v (%v)", replay, err)
	}
}

// A secret sent in an Authorization header during a run must not be stored,
// logged or returned anywhere.
func TestRedactionOfSecrets(t *testing.T) {
	const secret = "s3cr3t-itest-9f3a7c21-do-not-store"
	var mu sync.Mutex
	seen := map[string]int{}
	sut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Header.Get("Authorization")+"|"+r.Header.Get("Cookie")+"|"+r.Header.Get("X-Api-Key")]++
		mu.Unlock()
		io.Copy(io.Discard, r.Body)
		w.Write([]byte("ok"))
	}))
	defer sut.Close()

	f := startFull(t, "naive")
	yaml := f.experiment(t, expOpts{
		Name:    "redaction",
		BaseURL: sut.URL,
		Requests: `    - method: POST
      path: /thing
      headers:
        Authorization: "Bearer ` + secret + `"
        cookie: "session=` + secret + `"
        X-API-KEY: ` + secret + `
        X-Harmless: visible-value
      body: '{"n":1}'`,
		Probes: "    - {name: sut, url: " + sut.URL + "/health, interval: 200ms}",
		Faults: `  - id: f1
    type: http_error
    route: llm
    match: {path: "/v1/*"}
    params:
      status: 503
      headers: {Set-Cookie: "session=` + secret + `"}`,
	})
	id := f.control.createRun(t, yaml)
	f.control.waitStatus(t, id, 15*time.Second, "injecting")
	// The injected response really carries the header; only the stored copy is redacted.
	res := call("POST", f.proxy.url("/llm/v1/messages"), map[string]string{"X-Chaos-Run": id, "Authorization": "Bearer " + secret}, `{"secret":"`+secret+`"}`)
	if res.status != 503 || !strings.Contains(res.header.Get("Set-Cookie"), secret) {
		t.Fatalf("expected the injected response with its Set-Cookie header: %d %v", res.status, res.header)
	}
	doc := f.control.waitDone(t, id, 30*time.Second)
	if doc.Status != "completed" {
		t.Fatalf("run ended %s (%s)", doc.Status, doc.reason())
	}

	// The driver sent the real values.
	mu.Lock()
	sent := seen["Bearer "+secret+"|session="+secret+"|"+secret]
	mu.Unlock()
	if sent < 20 {
		t.Fatalf("the driver must send the real header values; saw %v", seen)
	}

	// Saving through the API must not write the secret either.
	if code := f.control.api(t, "PUT", "/api/v1/experiments/redaction", obj{"yaml": yaml}, nil); code != 200 {
		t.Fatalf("PUT: %d", code)
	}

	var leaks []string
	check := func(where string, data []byte) {
		if strings.Contains(string(data), secret) {
			leaks = append(leaks, where)
		}
	}

	for _, path := range []string{
		"/api/v1/runs", "/api/v1/runs/" + id, "/api/v1/runs/" + id + "/timeseries", "/api/v1/runs/" + id + "/events",
		"/api/v1/audit", "/api/v1/experiments", "/api/v1/experiments/redaction", "/api/v1/faults/active", "/api/v1/routes",
		"/api/v1/runs/" + id + "/report?format=json", "/api/v1/runs/" + id + "/report?format=md",
		"/api/v1/runs/" + id + "/report?format=html", "/api/v1/runs/" + id + "/report?format=junit",
	} {
		req, _ := http.NewRequest("GET", f.control.url(path), nil)
		req.Header.Set("Authorization", "Bearer "+controlToken)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		check("API "+path, body)
	}
	for _, path := range []string{"/admin/v1/records", "/admin/v1/stats", "/admin/v1/faults/active"} {
		req, _ := http.NewRequest("GET", f.proxy.admin(path), nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		check("admin API "+path, body)
	}

	// Everything on disk: data_dir (database, WAL, audit log, reports) and the experiments dir.
	files := 0
	for _, root := range []string{f.s.dataDir, filepath.Join(f.s.dir, "experiments")} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files++
			check("file "+path, data)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files < 3 {
		t.Fatalf("expected to scan the database, the audit log and the saved experiment; scanned %d files", files)
	}
	check("control log", []byte(f.control.out.String()))
	check("proxy log", []byte(f.proxy.out.String()))
	for _, tok := range []string{controlToken, adminToken} {
		if strings.Contains(f.control.out.String()+f.proxy.out.String(), tok) {
			t.Errorf("a bearer token was logged")
		}
	}
	if len(leaks) > 0 {
		t.Fatalf("the secret leaked into: %v", leaks)
	}

	// The redacted copy is what the API shows, with harmless values intact.
	var got map[string]string
	f.control.api(t, "GET", "/api/v1/experiments/redaction", nil, &got)
	if strings.Count(got["yaml"], "[REDACTED]") != 4 || !strings.Contains(got["yaml"], "visible-value") {
		t.Fatalf("expected 4 redacted values and the harmless header kept:\n%s", got["yaml"])
	}
}

func TestControlStartupGuards(t *testing.T) {
	run := func(t *testing.T, env []string, mod func(*stack)) (int, string) {
		s := newStack(t, map[string]string{"svc": "http://127.0.0.1:9"})
		if mod != nil {
			mod(s)
		}
		s.writeConfig(t)
		p := start(t, "faultline", env, "control", "--config", s.cfgPath)
		return p.wait(10 * time.Second), p.out.String()
	}
	both := []string{"FAULTLINE_TOKEN=" + controlToken, "FAULTLINE_ADMIN_TOKEN=" + adminToken}

	for _, env := range []string{"prod", "production", "live", "Prod", "PRODUCTION", "Live"} {
		code, out := run(t, both, func(s *stack) { s.environment = env })
		if code != 2 || !strings.Contains(strings.ToLower(out), "production") {
			t.Errorf("environment %s: expected exit 2 with a clear message, got %d: %s", env, code, out)
		}
	}
	if code, out := run(t, []string{"FAULTLINE_TOKEN=", "FAULTLINE_ADMIN_TOKEN=" + adminToken}, nil); code != 2 || !strings.Contains(out, "FAULTLINE_TOKEN") {
		t.Errorf("missing FAULTLINE_TOKEN: expected exit 2, got %d: %s", code, out)
	}
	if code, out := run(t, []string{"FAULTLINE_TOKEN=" + controlToken, "FAULTLINE_ADMIN_TOKEN="}, nil); code != 2 || !strings.Contains(out, "FAULTLINE_ADMIN_TOKEN") {
		t.Errorf("missing FAULTLINE_ADMIN_TOKEN: expected exit 2, got %d: %s", code, out)
	}
	if code, _ := run(t, []string{"FAULTLINE_TOKEN=fifteen-chars-x", "FAULTLINE_ADMIN_TOKEN=" + adminToken}, nil); code != 2 {
		t.Errorf("15 character token: expected exit 2, got %d", code)
	}
	if code, _ := run(t, both, func(s *stack) { s.extra = "unknown_key: 1\n" }); code != 2 {
		t.Errorf("unknown config key: expected exit 2, got %d", code)
	}
}

func TestControlStopsWhenParentPipeCloses(t *testing.T) {
	f := startFull(t, "naive")
	f.control.kill()
	f.s.writeConfig(t)
	c := &controlProc{s: f.s}
	c.proc = startPiped(t, "faultline", []string{"FAULTLINE_TOKEN=" + controlToken, "FAULTLINE_ADMIN_TOKEN=" + adminToken, "FAULTLINE_PARENT_WATCH=1"}, "control", "--config", f.s.cfgPath)
	waitHTTP(t, c.url("/healthz"))
	f.control = c

	id := c.createRun(t, longRun(f, t, expOpts{Name: "shutdown"}))
	c.waitStatus(t, id, 15*time.Second, "injecting")
	eventually(t, 5*time.Second, "fault active", func() bool { return f.faultedThroughProxy(id) })

	c.stdin.Close()
	if code := c.wait(25 * time.Second); code != 0 {
		t.Fatalf("expected a clean exit, got %d", code)
	}
	// A graceful shutdown clears faults itself instead of leaving them to the lease.
	if f.faultedThroughProxy(id) {
		t.Fatal("control exited and left its faults installed")
	}
	f.control = f.s.startControl(t)
	if doc := f.control.run(t, id); doc.Status != "aborted" || doc.reason() != "control_restarted" {
		t.Fatalf("got %s (%s)", doc.Status, doc.reason())
	}
}

var tokenLine = regexp.MustCompile(`(?m)^(FAULTLINE_TOKEN|FAULTLINE_ADMIN_TOKEN)=([0-9a-f]{64})$`)

func TestUp(t *testing.T) {
	up := newUpstream(t)
	s := newStack(t, map[string]string{"svc": up.URL})
	s.writeConfig(t)
	// Tokens are deliberately unset so that `up` has to generate them.
	p := start(t, "faultline", []string{"FAULTLINE_TOKEN=", "FAULTLINE_ADMIN_TOKEN="}, "up", "--config", s.cfgPath)

	var tokens map[string]string
	eventually(t, 20*time.Second, "`up` to print its URLs and tokens", func() bool {
		tokens = map[string]string{}
		for _, m := range tokenLine.FindAllStringSubmatch(p.out.String(), -1) {
			tokens[m[1]] = m[2]
		}
		return len(tokens) == 2 && strings.Contains(p.out.String(), "http://"+s.controlAddr)
	})
	if tokens["FAULTLINE_TOKEN"] == tokens["FAULTLINE_ADMIN_TOKEN"] {
		t.Fatal("the two tokens must differ")
	}
	if n := strings.Count(p.out.String(), tokens["FAULTLINE_TOKEN"]); n != 1 {
		t.Fatalf("the control token must be printed exactly once, was printed %d times", n)
	}
	for _, u := range []string{"http://" + s.trafficAddr, "http://" + s.adminAddr} {
		if !strings.Contains(p.out.String(), u) {
			t.Fatalf("`up` did not print %s:\n%s", u, p.out.String())
		}
	}

	// Both children are up and use the generated tokens.
	if code := apiCall(t, "GET", "http://"+s.controlAddr+"/api/v1/routes", tokens["FAULTLINE_TOKEN"], nil, nil); code != 200 {
		t.Fatalf("control with the generated token: %d", code)
	}
	if code := apiCall(t, "GET", "http://"+s.adminAddr+"/admin/v1/stats", tokens["FAULTLINE_ADMIN_TOKEN"], nil, nil); code != 200 {
		t.Fatalf("proxy admin with the generated token: %d", code)
	}
	if code := apiCall(t, "GET", "http://"+s.controlAddr+"/api/v1/faults/active", tokens["FAULTLINE_TOKEN"], nil, nil); code != 200 {
		t.Fatalf("control cannot reach the proxy with the generated admin token: %d", code)
	}
	if res := call("GET", "http://"+s.trafficAddr+"/svc/x", nil, ""); res.status != 200 {
		t.Fatalf("proxy traffic port: %d", res.status)
	}

	// Killing `up` outright must not leave orphans: the children notice their stdin closing.
	p.kill()
	for _, addr := range []string{s.controlAddr, s.adminAddr, s.trafficAddr} {
		eventually(t, 15*time.Second, addr+" to stop listening", func() bool {
			return call("GET", "http://"+addr+"/healthz", nil, "").err != nil
		})
	}
}

func TestUpWithTokensFromEnvironment(t *testing.T) {
	up := newUpstream(t)
	s := newStack(t, map[string]string{"svc": up.URL})
	s.writeConfig(t)
	p := start(t, "faultline", []string{"FAULTLINE_TOKEN=" + controlToken, "FAULTLINE_ADMIN_TOKEN=" + adminToken}, "up", "--config", s.cfgPath)
	eventually(t, 20*time.Second, "`up` to report ready", func() bool { return strings.Contains(p.out.String(), "Faultline is up") })
	if strings.Contains(p.out.String(), controlToken) || strings.Contains(p.out.String(), adminToken) {
		t.Fatal("tokens taken from the environment must not be echoed")
	}
	if code := apiCall(t, "GET", "http://"+s.controlAddr+"/api/v1/routes", controlToken, nil, nil); code != 200 {
		t.Fatalf("control with the environment token: %d", code)
	}
}

func TestUpExitsWhenAChildFails(t *testing.T) {
	up := newUpstream(t)
	s := newStack(t, map[string]string{"svc": up.URL})
	// The proxy's traffic port is taken, so the proxy child fails to start.
	blocker := newUpstream(t)
	s.trafficAddr = strings.TrimPrefix(blocker.URL, "http://")
	s.writeConfig(t)
	p := start(t, "faultline", nil, "up", "--config", s.cfgPath)
	if code := p.wait(25 * time.Second); code == 0 {
		t.Fatal("`up` must fail when a child cannot start")
	}
	eventually(t, 10*time.Second, "control child to be stopped", func() bool {
		return call("GET", "http://"+s.controlAddr+"/healthz", nil, "").err != nil
	})

	s2 := newStack(t, map[string]string{"svc": up.URL})
	s2.environment = "production"
	s2.writeConfig(t)
	guard := start(t, "faultline", nil, "up", "--config", s2.cfgPath)
	if code := guard.wait(10 * time.Second); code != 2 {
		t.Fatalf("`up` in a production environment: expected exit 2, got %d", code)
	}
}

func TestStructuredLogs(t *testing.T) {
	up := newUpstream(t)
	s := newStack(t, map[string]string{"svc": up.URL})
	s.writeConfig(t)
	env := []string{"FAULTLINE_TOKEN=" + controlToken, "FAULTLINE_ADMIN_TOKEN=" + adminToken}
	p := start(t, "faultline", env, "proxy", "--config", s.cfgPath, "--log-format", "json")
	c := start(t, "faultline", env, "control", "--config", s.cfgPath, "--log-format", "json")
	waitHTTP(t, "http://"+s.adminAddr+"/admin/v1/healthz")
	waitHTTP(t, "http://"+s.controlAddr+"/healthz")
	for name, proc := range map[string]*proc{"proxy": p, "control": c} {
		// The listener answers a moment before the startup line is written.
		eventually(t, 3*time.Second, name+" startup log line", func() bool { return strings.Contains(proc.out.String(), "\n") })
		sc := bufio.NewScanner(strings.NewReader(proc.out.String()))
		lines := 0
		for sc.Scan() {
			var entry map[string]any
			if err := json.Unmarshal(sc.Bytes(), &entry); err != nil {
				t.Fatalf("%s: log line is not JSON: %q", name, sc.Text())
			}
			if entry["msg"] == nil || entry["level"] == nil || entry["time"] == nil {
				t.Fatalf("%s: log line lacks time, level or msg: %v", name, entry)
			}
			lines++
		}
		if lines == 0 {
			t.Fatalf("%s logged nothing", name)
		}
	}
	bad := start(t, "faultline", env, "proxy", "--config", s.cfgPath, "--log-format", "xml")
	if code := bad.wait(10 * time.Second); code != 2 {
		t.Fatalf("unknown log format: expected exit 2, got %d", code)
	}
}
