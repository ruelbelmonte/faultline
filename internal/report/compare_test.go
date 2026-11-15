package report

import (
	"strings"
	"testing"
)

func baseDoc(id, verdict string) *Document {
	return &Document{
		SchemaVersion: SchemaVersion, RunID: id, Status: StatusCompleted, Verdict: str(verdict),
		Experiment: ExperimentInfo{Name: "inventory-503", YAMLSHA256: "abc"},
		Hypothesis: []HypothesisEntry{
			{Name: "error_rate_max", Threshold: 0.05, Observed: f64(0.01), Passed: true},
			{Name: "p95_latency_ms_max", Threshold: 1500, Observed: f64(400), Passed: true},
		},
		Faults:   []FaultResult{{ID: "f1", Type: "http_error", Route: "inventory", Triggers: 100}},
		Findings: []Finding{{ID: "retry_without_backoff", Severity: "medium", Evidence: map[string]any{"route": "inventory"}}},
	}
}

// Issue #28: the comparison names the baseline, gives the change per
// hypothesis value and per fault, and lists findings that appeared or went.
func TestCompare(t *testing.T) {
	base := baseDoc("run_aaaaaaaaaaaa", VerdictPassed)
	cur := baseDoc("run_bbbbbbbbbbbb", VerdictFailed)
	cur.Hypothesis[0].Observed, cur.Hypothesis[0].Passed = f64(0.12), false
	cur.Faults[0].Triggers = 130
	cur.Faults = append(cur.Faults, FaultResult{ID: "f2", Triggers: 5})
	cur.Findings = []Finding{{ID: "retry_amplification", Severity: "high", Evidence: map[string]any{"route": "inventory"}}}

	c := Compare(cur, base, BaselinePrevious)
	if c.BaselineRunID != base.RunID || c.BaselineSelection != BaselinePrevious || !c.SameExperimentYAML {
		t.Fatalf("baseline identity: %+v", c)
	}
	if got := c.Hypothesis[0]; got.Delta == nil || *got.Delta < 0.109 || *got.Delta > 0.111 || got.BaselinePassed == nil || !*got.BaselinePassed || got.Passed {
		t.Fatalf("error rate delta: %+v", got)
	}
	if got := c.Faults[0]; got.BaselineTriggers == nil || *got.BaselineTriggers != 100 || *got.Delta != 30 {
		t.Fatalf("trigger delta: %+v", got)
	}
	if got := c.Faults[1]; got.BaselineTriggers != nil || got.Delta != nil {
		t.Fatalf("a fault missing from the baseline has no delta: %+v", got)
	}
	if len(c.FindingsAppeared) != 1 || c.FindingsAppeared[0].ID != "retry_amplification" || c.FindingsAppeared[0].Route != "inventory" {
		t.Fatalf("appeared: %+v", c.FindingsAppeared)
	}
	if len(c.FindingsResolved) != 1 || c.FindingsResolved[0].ID != "retry_without_backoff" {
		t.Fatalf("resolved: %+v", c.FindingsResolved)
	}
	if !c.Regression {
		t.Fatal("a newly failing threshold, a worse verdict and a new finding are regressions")
	}
	joined := strings.Join(c.RegressionReasons, "\n")
	for _, want := range []string{"verdict passed -> failed", "error_rate_max", "retry_amplification"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("reasons should mention %q: %v", want, c.RegressionReasons)
		}
	}
}

func TestCompareNoRegression(t *testing.T) {
	base := baseDoc("run_aaaaaaaaaaaa", VerdictPassed)
	cur := baseDoc("run_bbbbbbbbbbbb", VerdictPassed)
	cur.Findings = nil // a finding went away: an improvement
	cur.Experiment.YAMLSHA256 = "def"
	c := Compare(cur, base, "run_aaaaaaaaaaaa")
	if c.Regression || len(c.RegressionReasons) != 0 {
		t.Fatalf("no regression expected: %+v", c)
	}
	if c.SameExperimentYAML {
		t.Fatal("different yaml_sha256 must be reported")
	}
	if c.BaselineSelection != BaselineExplicit {
		t.Fatalf("an explicit run id is reported as explicit: %q", c.BaselineSelection)
	}
}

// The comparison carries only ids, numbers and verdicts, never evidence or
// experiment content, so it cannot carry anything redaction removed.
func TestCompareCarriesNoEvidence(t *testing.T) {
	base := baseDoc("run_aaaaaaaaaaaa", VerdictPassed)
	cur := baseDoc("run_bbbbbbbbbbbb", VerdictPassed)
	cur.Findings = append(cur.Findings, Finding{ID: "missing_timeout", Detail: "s3cr3t-detail", Evidence: map[string]any{"route": "inventory", "note": "s3cr3t-evidence"}})
	cur.Experiment.Description = "s3cr3t-description"
	out, err := JSON(&Document{Comparison: Compare(cur, base, BaselinePrevious)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "s3cr3t") {
		t.Fatalf("the comparison must not copy finding detail, evidence or experiment text:\n%s", out)
	}
}

func TestMarkdownComparison(t *testing.T) {
	base := baseDoc("run_aaaaaaaaaaaa", VerdictPassed)
	cur := baseDoc("run_bbbbbbbbbbbb", VerdictFailed)
	cur.Hypothesis[0].Observed, cur.Hypothesis[0].Passed = f64(0.12), false
	cur.Comparison = Compare(cur, base, BaselinePrevious)
	md := Markdown(cur)
	for _, want := range []string{"## Compared with `run_aaaaaaaaaaaa`", "Regression", "error_rate_max", "0.01", "0.12"} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown should contain %q:\n%s", want, md)
		}
	}
	none := baseDoc("run_cccccccccccc", VerdictPassed)
	if md := Markdown(none); !strings.Contains(md, "No earlier run of this experiment to compare with") {
		t.Fatalf("markdown without a baseline should say so:\n%s", md)
	}
}
