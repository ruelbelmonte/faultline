package control

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"faultline/internal/report"
)

func TestPercentileNearestRank(t *testing.T) {
	v := []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	for _, tc := range []struct{ p, want float64 }{{0.50, 50}, {0.95, 100}, {0.90, 90}, {0.01, 10}, {1, 100}} {
		if got := percentile(v, tc.p); got != tc.want {
			t.Errorf("percentile(%v) = %v, want %v", tc.p, got, tc.want)
		}
	}
	if percentile(nil, 0.95) != 0 {
		t.Error("percentile of nothing must be 0")
	}
	if percentile([]float64{7}, 0.5) != 7 {
		t.Error("percentile of one value must be that value")
	}
}

func TestRunIDFormat(t *testing.T) {
	re := regexp.MustCompile(`^run_[a-z2-7]{12}$`)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := newRunID()
		if !re.MatchString(id) {
			t.Fatalf("bad run id %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate run id %q", id)
		}
		seen[id] = true
	}
}

func TestUUIDFormat(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	for i := 0; i < 50; i++ {
		if id := newUUID(); !re.MatchString(id) {
			t.Fatalf("bad uuid %q", id)
		}
	}
}

func TestVerdictRuleOrder(t *testing.T) {
	passed, failed := report.HypothesisEntry{Passed: true}, report.HypothesisEntry{Passed: false}
	hit, miss := report.FaultResult{Triggers: 3}, report.FaultResult{Triggers: 0}
	cases := []struct {
		name           string
		cause          *abortCause
		precheckFailed bool
		faults         []report.FaultResult
		hypothesis     []report.HypothesisEntry
		verdict        string
		reason         string
	}{
		{"aborted beats everything", &abortCause{reason: ReasonManual}, true, []report.FaultResult{miss}, []report.HypothesisEntry{failed}, "aborted", "manual"},
		{"precheck beats untriggered", nil, true, []report.FaultResult{miss}, []report.HypothesisEntry{failed}, "inconclusive", "unhealthy_before_start"},
		{"untriggered beats breach", nil, false, []report.FaultResult{hit, miss}, []report.HypothesisEntry{failed}, "inconclusive", "fault_not_triggered"},
		{"breach", nil, false, []report.FaultResult{hit}, []report.HypothesisEntry{passed, failed}, "failed", ""},
		{"passed", nil, false, []report.FaultResult{hit}, []report.HypothesisEntry{passed, passed}, "passed", ""},
		{"passed with no hypothesis", nil, false, []report.FaultResult{hit}, nil, "passed", ""},
	}
	for _, tc := range cases {
		r := &Run{}
		r.doc.Faults, r.doc.Hypothesis = tc.faults, tc.hypothesis
		verdict, reason := r.verdictLocked(tc.cause, tc.precheckFailed)
		if verdict != tc.verdict || reason != tc.reason {
			t.Errorf("%s: got %s (%s), want %s (%s)", tc.name, verdict, reason, tc.verdict, tc.reason)
		}
	}
}

func TestStore(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	passed := report.VerdictPassed
	docs := []report.Document{
		{RunID: "run_a", Status: report.StatusCompleted, Verdict: &passed, StartedAt: report.At(time.Now()), Experiment: report.ExperimentInfo{Name: "one"}},
		{RunID: "run_b", Status: report.StatusInjecting, StartedAt: report.At(time.Now()), Experiment: report.ExperimentInfo{Name: "two"}},
	}
	for i := range docs {
		if err := s.SaveRun(&docs[i], &Meta{}, []byte("yaml")); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.Runs()
	if err != nil || len(runs) != 2 || runs[0].RunID != "run_b" || runs[1].RunID != "run_a" {
		t.Fatalf("runs must be listed newest first: %+v (%v)", runs, err)
	}
	if runs[0].Verdict != nil || *runs[1].Verdict != "passed" {
		t.Fatalf("verdicts: %+v", runs)
	}
	unfinished, _ := s.Unfinished()
	if len(unfinished) != 1 || unfinished[0] != "run_b" {
		t.Fatalf("unfinished: %v", unfinished)
	}
	docs[1].Status = report.StatusAborted
	s.SaveRun(&docs[1], &Meta{}, nil)
	if unfinished, _ = s.Unfinished(); len(unfinished) != 0 {
		t.Fatalf("still unfinished after update: %v", unfinished)
	}
	if runs, _ = s.Runs(); len(runs) != 2 || runs[0].RunID != "run_b" {
		t.Fatalf("an update must not reorder or duplicate runs: %+v", runs)
	}
	if _, _, err := s.Run("run_nope"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	for _, action := range []string{"run_created", "kill_switch", "run_aborted"} {
		if err := s.Audit(ActorToken, action, "run_a", map[string]any{"n": 1}); err != nil {
			t.Fatal(err)
		}
	}
	log, _ := s.AuditLog(2)
	if len(log) != 2 || log[0].Action != "run_aborted" || log[1].Action != "kill_switch" {
		t.Fatalf("audit log must be newest first and honour the limit: %+v", log)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], `"action":"run_created"`) || !strings.Contains(lines[0], `"actor":"api-token"`) {
		t.Fatalf("audit.jsonl must be append-only in order: %q", data)
	}
}

func TestBucketRoundTripsAndAggregatesRoutes(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b := &Bucket{T: report.At(time.Unix(1700000000, 0)), Routes: map[string]report.RouteMetrics{
		"a": {Requests: 3, Faulted: 1}, "b": {Requests: 2, Faulted: 2},
	}}
	b.Driver.Requests, b.Driver.Errors, b.Driver.P95Ms = 10, 2, 42.5
	b.Probes.OK, b.Probes.Fail = 4, 1
	if err := s.SaveBucket("run_a", b); err != nil {
		t.Fatal(err)
	}
	got, err := s.Buckets("run_a")
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	p := got[0].Point()
	if p.Driver.Requests != 10 || p.Driver.Errors != 2 || p.Driver.P95Ms != 42.5 || p.Proxy.Requests != 5 || p.Proxy.Faulted != 3 || p.Probes.OK != 4 || p.Probes.Fail != 1 {
		t.Fatalf("unexpected point: %+v", p)
	}
}
