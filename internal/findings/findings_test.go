package findings

import (
	"testing"
	"time"

	"faultline/internal/fault"
	"faultline/internal/report"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

type recOpt func(*fault.Record)

func faulted(id string) recOpt {
	return func(r *fault.Record) {
		typ := "http_error"
		r.FaultID, r.FaultType, r.Status = &id, &typ, 503
	}
}

func retryAfter(s int) recOpt { return func(r *fault.Record) { r.RetryAfter = &s } }
func held(ms float64) recOpt {
	return func(r *fault.Record) { r.ClientHeldMs = &ms; r.DurationMs = ms; r.Status = 0 }
}
func took(ms float64) recOpt      { return func(r *fault.Record) { r.DurationMs = ms } }
func onRoute(route string) recOpt { return func(r *fault.Record) { r.Route = route } }

// rec builds a record that arrives atMs after t0 and takes 1 ms unless told otherwise.
func rec(corr string, atMs float64, opts ...recOpt) fault.Record {
	r := fault.Record{
		TS: t0.Add(time.Duration(atMs * float64(time.Millisecond))), Route: "inventory", Method: "GET", Path: "/stock/1",
		Status: 200, DurationMs: 1, Scoped: true, Correlation: corr,
	}
	for _, o := range opts {
		o(&r)
	}
	return r
}

func find(fs []report.Finding, id string) []report.Finding {
	var out []report.Finding
	for _, f := range fs {
		if f.ID == id {
			out = append(out, f)
		}
	}
	return out
}

func one(t *testing.T, fs []report.Finding, id string) report.Finding {
	t.Helper()
	got := find(fs, id)
	if len(got) != 1 {
		t.Fatalf("expected exactly one %s finding, got %d in %+v", id, len(got), fs)
	}
	if got[0].Title == "" || got[0].Detail == "" {
		t.Fatalf("%s lacks a title or detail: %+v", id, got[0])
	}
	return got[0]
}

func none(t *testing.T, fs []report.Finding, id string) {
	t.Helper()
	if got := find(fs, id); len(got) != 0 {
		t.Fatalf("expected no %s finding, got %+v", id, got)
	}
}

func f64(v float64) *float64 { return &v }

func TestSteadyStateBreach(t *testing.T) {
	fs := Evaluate(Input{Hypothesis: []report.HypothesisEntry{
		{Name: "error_rate_max", Threshold: 0.05, Observed: f64(0.12), Passed: false},
		{Name: "p95_latency_ms_max", Threshold: 1500, Observed: f64(900), Passed: true},
		{Name: "probe_success_min", Threshold: 0.95, Observed: nil, Passed: false},
	}})
	got := find(fs, SteadyStateBreach)
	if len(got) != 2 {
		t.Fatalf("one finding per breached threshold: %+v", got)
	}
	e := got[0].Evidence
	if got[0].Severity != "high" || e["name"] != "error_rate_max" || e["threshold"] != 0.05 || e["observed"] != 0.12 {
		t.Fatalf("unexpected finding: %+v", got[0])
	}
	if got[1].Evidence["name"] != "probe_success_min" || got[1].Evidence["observed"] != nil {
		t.Fatalf("a threshold with no data is breached with observed null: %+v", got[1])
	}
}

func TestRetryAmplification(t *testing.T) {
	var recs []fault.Record
	// Two logical requests, 4 attempts each, 100 ms apart.
	for _, c := range []string{"a", "b"} {
		for i := 0; i < 4; i++ {
			recs = append(recs, rec(c, float64(i)*100, faulted("f1")))
		}
	}
	in := Input{Records: recs, RetryAmplificationThreshold: 3}
	f := one(t, Evaluate(in), RetryAmplification)
	e := f.Evidence
	if f.Severity != "medium" || e["route"] != "inventory" || e["mean_attempts"] != 4.0 || e["max_attempts"] != 4 || e["correlations"] != 2 {
		t.Fatalf("unexpected finding: %+v", f)
	}

	in.RetryAmplificationThreshold = 2
	if f := one(t, Evaluate(in), RetryAmplification); f.Severity != "high" {
		t.Fatalf("mean >= 2x threshold must be high: %+v", f)
	}
	in.RetryAmplificationThreshold = 4.01
	none(t, Evaluate(in), RetryAmplification)

	// Exactly at the threshold counts.
	in.RetryAmplificationThreshold = 4
	one(t, Evaluate(in), RetryAmplification)

	// A route with no faulted request is not judged, however often it is called.
	var clean []fault.Record
	for i := 0; i < 5; i++ {
		clean = append(clean, rec("a", float64(i)*100))
	}
	none(t, Evaluate(Input{Records: clean, RetryAmplificationThreshold: 3}), RetryAmplification)

	// Requests without a correlation id cannot be grouped.
	var anonymous []fault.Record
	for i := 0; i < 5; i++ {
		anonymous = append(anonymous, rec("", float64(i)*100, faulted("f1")))
	}
	none(t, Evaluate(Input{Records: anonymous, RetryAmplificationThreshold: 3}), RetryAmplification)
}

func TestRetryAmplificationIsPerRoute(t *testing.T) {
	var recs []fault.Record
	for i := 0; i < 4; i++ {
		recs = append(recs, rec("a", float64(i)*100, faulted("f1"), onRoute("pricing")))
	}
	recs = append(recs, rec("a", 0))
	fs := find(Evaluate(Input{Records: recs, RetryAmplificationThreshold: 3}), RetryAmplification)
	if len(fs) != 1 || fs[0].Evidence["route"] != "pricing" {
		t.Fatalf("only the faulted route is reported: %+v", fs)
	}
}

func TestRetryWithoutBackoff(t *testing.T) {
	// Response at 0+1ms, retries 9 ms and then 19 ms after the previous response.
	quick := []fault.Record{rec("a", 0, faulted("f1")), rec("a", 10, faulted("f1")), rec("a", 30)}
	f := one(t, Evaluate(Input{Records: quick, RetryAmplificationThreshold: 99}), RetryWithoutBackoff)
	if f.Severity != "medium" || f.Evidence["route"] != "inventory" || f.Evidence["occurrences"] != 2 || f.Evidence["median_gap_ms"] != 14.0 {
		t.Fatalf("unexpected finding: %+v", f)
	}

	// 200 ms backoff after each faulted response.
	patient := []fault.Record{rec("a", 0, faulted("f1")), rec("a", 201, faulted("f1")), rec("a", 402)}
	none(t, Evaluate(Input{Records: patient, RetryAmplificationThreshold: 99}), RetryWithoutBackoff)

	// The gap is measured from the end of the faulted response, not its start.
	slow := []fault.Record{rec("a", 0, faulted("f1"), took(500)), rec("a", 520)}
	one(t, Evaluate(Input{Records: slow, RetryAmplificationThreshold: 99}), RetryWithoutBackoff)

	// A quick request after a response that was not faulted is not a retry of a fault.
	unfaulted := []fault.Record{rec("a", 0, faulted("f1")), rec("a", 300), rec("a", 305)}
	none(t, Evaluate(Input{Records: unfaulted, RetryAmplificationThreshold: 99}), RetryWithoutBackoff)

	// Different correlations are different logical requests.
	others := []fault.Record{rec("a", 0, faulted("f1")), rec("b", 5, faulted("f1"))}
	none(t, Evaluate(Input{Records: others, RetryAmplificationThreshold: 99}), RetryWithoutBackoff)

	// Overlapping requests of one correlation are parallel calls, not retries.
	parallel := []fault.Record{rec("a", 0, faulted("f1"), took(400)), rec("a", 100, faulted("f1"), took(400))}
	none(t, Evaluate(Input{Records: parallel, RetryAmplificationThreshold: 99}), RetryWithoutBackoff)
}

func TestRetryAfterIgnored(t *testing.T) {
	ignored := []fault.Record{rec("a", 0, faulted("f1"), retryAfter(1)), rec("a", 301, faulted("f1"), retryAfter(1)), rec("a", 602)}
	f := one(t, Evaluate(Input{Records: ignored, RetryAmplificationThreshold: 99}), RetryAfterIgnored)
	e := f.Evidence
	if f.Severity != "medium" || e["route"] != "inventory" || e["violations"] != 2 || e["retry_after_s"] != 1 || e["median_gap_ms"] != 300.0 {
		t.Fatalf("unexpected finding: %+v", f)
	}

	// Waiting 0.9 x Retry-After is enough; a little less is not.
	honoured := []fault.Record{rec("a", 0, faulted("f1"), retryAfter(1)), rec("a", 901)}
	none(t, Evaluate(Input{Records: honoured, RetryAmplificationThreshold: 99}), RetryAfterIgnored)
	almost := []fault.Record{rec("a", 0, faulted("f1"), retryAfter(1)), rec("a", 890)}
	one(t, Evaluate(Input{Records: almost, RetryAmplificationThreshold: 99}), RetryAfterIgnored)

	// No Retry-After was sent, so none can be ignored.
	plain := []fault.Record{rec("a", 0, faulted("f1")), rec("a", 10)}
	none(t, Evaluate(Input{Records: plain, RetryAmplificationThreshold: 99}), RetryAfterIgnored)

	// The caller gave up instead of retrying.
	gaveUp := []fault.Record{rec("a", 0, faulted("f1"), retryAfter(1))}
	none(t, Evaluate(Input{Records: gaveUp, RetryAmplificationThreshold: 99}), RetryAfterIgnored)
}

func TestMissingTimeout(t *testing.T) {
	tf := []TimeoutFault{{ID: "t1", Route: "inventory", HoldMs: 3000}}
	stuck := []fault.Record{rec("a", 0, faulted("t1"), held(3000)), rec("b", 0, faulted("t1"), held(2990)), rec("c", 0, faulted("t1"), held(1000))}
	f := one(t, Evaluate(Input{Records: stuck, TimeoutFaults: tf, RetryAmplificationThreshold: 99}), MissingTimeout)
	e := f.Evidence
	if f.Severity != "high" || e["route"] != "inventory" || e["hold_ms"] != 3000.0 || e["median_client_held_ms"] != 2990.0 || e["samples"] != 3 {
		t.Fatalf("unexpected finding: %+v", f)
	}

	impatient := []fault.Record{rec("a", 0, faulted("t1"), held(1000)), rec("b", 0, faulted("t1"), held(1010))}
	none(t, Evaluate(Input{Records: impatient, TimeoutFaults: tf, RetryAmplificationThreshold: 99}), MissingTimeout)

	// Exactly 0.9 x hold counts.
	edge := []fault.Record{rec("a", 0, faulted("t1"), held(2700))}
	one(t, Evaluate(Input{Records: edge, TimeoutFaults: tf, RetryAmplificationThreshold: 99}), MissingTimeout)

	none(t, Evaluate(Input{TimeoutFaults: tf, RetryAmplificationThreshold: 99}), MissingTimeout)

	// Only records of that fault count.
	other := []fault.Record{rec("a", 0, faulted("other"), held(3000))}
	none(t, Evaluate(Input{Records: other, TimeoutFaults: tf, RetryAmplificationThreshold: 99}), MissingTimeout)
}

// Holds ended by the faults being cleared are neither a caller giving up nor
// a caller sitting the hold out, so they must not sway the median either way.
func TestMissingTimeoutIgnoresHoldsCutByClear(t *testing.T) {
	tf := []TimeoutFault{{ID: "t1", Route: "inventory", HoldMs: 3000}}
	cleared := t0.Add(5 * time.Second)
	recs := []fault.Record{
		rec("a", 0, faulted("t1"), held(3000)),
		rec("b", 4000, faulted("t1"), held(1000)),
		rec("c", 4500, faulted("t1"), held(500)),
		rec("d", 4800, faulted("t1"), held(200)),
	}
	in := Input{Records: recs, TimeoutFaults: tf, RetryAmplificationThreshold: 99, ClearedAt: cleared}
	f := one(t, Evaluate(in), MissingTimeout)
	if f.Evidence["samples"] != 1 || f.Evidence["median_client_held_ms"] != 3000.0 {
		t.Fatalf("only the hold that ran its course is evidence: %+v", f)
	}
	// Without the clear time the short holds would wrongly look like timeouts.
	in.ClearedAt = time.Time{}
	none(t, Evaluate(in), MissingTimeout)

	// A caller that gave up well before the clear still counts against the finding.
	in.ClearedAt = cleared
	in.Records = []fault.Record{rec("a", 0, faulted("t1"), held(1000)), rec("b", 100, faulted("t1"), held(1000)), rec("c", 200, faulted("t1"), held(3000))}
	none(t, Evaluate(in), MissingTimeout)
}

func TestSlowRecovery(t *testing.T) {
	base := Input{RecoveryMeasured: true, RecoveryTimeoutS: 30, RetryAmplificationThreshold: 3}

	in := base
	in.RecoverySeconds = f64(15.1)
	f := one(t, Evaluate(in), SlowRecovery)
	if f.Severity != "medium" || f.Evidence["recovery_seconds"] != 15.1 || f.Evidence["timeout_s"] != 30.0 {
		t.Fatalf("unexpected finding: %+v", f)
	}

	in.RecoverySeconds = f64(15)
	none(t, Evaluate(in), SlowRecovery)

	in.RecoverySeconds = nil
	if f := one(t, Evaluate(in), SlowRecovery); f.Evidence["recovery_seconds"] != nil {
		t.Fatalf("no recovery within the timeout is reported with recovery_seconds null: %+v", f)
	}

	// An aborted run never measured recovery.
	in.RecoveryMeasured = false
	none(t, Evaluate(in), SlowRecovery)
}

func TestFaultNotTriggered(t *testing.T) {
	faults := []Fault{
		{ID: "hit", Route: "inventory", Triggers: 3},
		{ID: "miss", Route: "pricing", Match: fault.Match{Method: "GET", Path: "/never/*"}},
	}
	f := one(t, Evaluate(Input{Faults: faults, InjectionCompleted: true, RetryAmplificationThreshold: 3}), FaultNotTriggered)
	match := f.Evidence["match"].(map[string]any)
	if f.Severity != "info" || f.Evidence["fault_id"] != "miss" || f.Evidence["route"] != "pricing" || match["path"] != "/never/*" || match["method"] != "GET" {
		t.Fatalf("unexpected finding: %+v", f)
	}
	none(t, Evaluate(Input{Faults: faults, InjectionCompleted: false, RetryAmplificationThreshold: 3}), FaultNotTriggered)
}

func TestFindingsOrderedBySeverityAndNeverNil(t *testing.T) {
	if fs := Evaluate(Input{RetryAmplificationThreshold: 3}); fs == nil || len(fs) != 0 {
		t.Fatalf("no findings must be an empty list, got %#v", fs)
	}
	recs := []fault.Record{rec("a", 0, faulted("f1")), rec("a", 5, faulted("f1"))}
	fs := Evaluate(Input{
		Records: recs, RetryAmplificationThreshold: 99,
		Faults: []Fault{{ID: "miss", Route: "pricing"}}, InjectionCompleted: true,
		Hypothesis: []report.HypothesisEntry{{Name: "error_rate_max", Threshold: 0.05, Observed: f64(1)}},
	})
	var order []string
	for _, f := range fs {
		order = append(order, f.Severity)
	}
	if len(order) != 3 || order[0] != "high" || order[1] != "medium" || order[2] != "info" {
		t.Fatalf("findings must be ordered high, medium, low, info: %v", order)
	}
}
