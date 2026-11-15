//go:build integration

package integration

import (
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var createdID = regexp.MustCompile(`(?m)^run (run_[a-z2-7]{12}) created$`)

func lastRunID(t testing.TB, out string) string {
	t.Helper()
	m := createdID.FindAllStringSubmatch(out, -1)
	if len(m) == 0 {
		t.Fatalf("no run id in output:\n%s", out)
	}
	return m[len(m)-1][1]
}

type comparisonDoc struct {
	RunID      string `json:"run_id"`
	Comparison *struct {
		BaselineRunID      string   `json:"baseline_run_id"`
		BaselineSelection  string   `json:"baseline_selection"`
		SameExperimentYAML bool     `json:"same_experiment_yaml"`
		Regression         bool     `json:"regression"`
		RegressionReasons  []string `json:"regression_reasons"`
		Hypothesis         []struct {
			Name  string   `json:"name"`
			Delta *float64 `json:"delta"`
		} `json:"hypothesis"`
		Faults []struct {
			ID    string `json:"id"`
			Delta *int64 `json:"delta"`
		} `json:"faults"`
	} `json:"comparison"`
}

// Issue #28: runs of the same experiment are compared with a baseline, the
// comparison is in the run document and the Markdown report, and
// --fail-on-regression exits 13.
func TestRunOverRunComparison(t *testing.T) {
	f := startFull(t, "naive")
	dir := t.TempDir()
	passing := writeFile(t, filepath.Join(dir, "passing.yaml"), f.experiment(t, expOpts{Name: "cmp", Duration: "2s", Faults: "  - id: f1\n    type: latency\n    params: {ms: 1}"}))
	failing := writeFile(t, filepath.Join(dir, "failing.yaml"), f.experiment(t, expOpts{Name: "cmp", Duration: "2s"}))
	other := writeFile(t, filepath.Join(dir, "other.yaml"), f.experiment(t, expOpts{Name: "cmp-other", Duration: "2s", Faults: "  - id: f1\n    type: latency\n    params: {ms: 1}"}))

	doc := func(id string) comparisonDoc {
		var d comparisonDoc
		if code := f.control.api(t, "GET", "/api/v1/runs/"+id, nil, &d); code != 200 {
			t.Fatalf("GET run %s: %d", id, code)
		}
		return d
	}

	// First run: nothing to compare with.
	code, out := f.cli(t, "run", passing, "--fail-on-regression")
	if code != 0 || !strings.Contains(out, "compared with: no earlier run of this experiment") {
		t.Fatalf("first run: exit %d:\n%s", code, out)
	}
	first := lastRunID(t, out)
	if d := doc(first); d.Comparison != nil {
		t.Fatalf("the first run has no comparison: %+v", d.Comparison)
	}

	// Same file again: compared with the previous run, no regression.
	code, out = f.cli(t, "run", passing, "--fail-on-regression")
	second := lastRunID(t, out)
	if code != 0 || !strings.Contains(out, "compared with "+first+" (previous): no regression") {
		t.Fatalf("second run: exit %d:\n%s", code, out)
	}
	d := doc(second)
	if d.Comparison == nil || d.Comparison.BaselineRunID != first || !d.Comparison.SameExperimentYAML || d.Comparison.Regression {
		t.Fatalf("second run comparison: %+v", d.Comparison)
	}
	if len(d.Comparison.Faults) != 1 || d.Comparison.Faults[0].Delta == nil {
		t.Fatalf("trigger change per fault: %+v", d.Comparison.Faults)
	}

	// A failing version against an explicit baseline: regression, exit 13.
	code, out = f.cli(t, "run", failing, "--baseline", second, "--fail-on-regression")
	third := lastRunID(t, out)
	if code != 13 || !strings.Contains(out, "REGRESSION") {
		t.Fatalf("regression: expected exit 13, got %d:\n%s", code, out)
	}
	d = doc(third)
	if d.Comparison == nil || d.Comparison.BaselineRunID != second || d.Comparison.BaselineSelection != "explicit" || d.Comparison.SameExperimentYAML || !d.Comparison.Regression {
		t.Fatalf("third run comparison: %+v", d.Comparison)
	}
	if !strings.Contains(strings.Join(d.Comparison.RegressionReasons, "\n"), "verdict passed -> failed") {
		t.Fatalf("reasons: %v", d.Comparison.RegressionReasons)
	}
	md, mdCode := f.control.rawGet(t, "/api/v1/runs/"+third+"/report?format=md")
	if mdCode != 200 || !strings.Contains(md, "## Compared with `"+second+"`") || !strings.Contains(md, "**Regression:**") {
		t.Fatalf("markdown report: %d\n%s", mdCode, md)
	}

	// Without --fail-on-regression the regression does not change the exit code.
	if code, out = f.cli(t, "run", failing, "--baseline", second); code != 0 {
		t.Fatalf("without the flag: exit %d:\n%s", code, out)
	}
	// A failed run outranks a regression when both flags are given.
	if code, out = f.cli(t, "run", failing, "--baseline", second, "--fail-on-regression", "--fail-on-breach"); code != 10 {
		t.Fatalf("failed and regressed: expected exit 10, got %d:\n%s", code, out)
	}

	// last-passed skips the failing runs and finds the second run's twin.
	code, out = f.cli(t, "run", passing, "--baseline", "last-passed")
	if latest := lastRunID(t, out); code != 0 || doc(latest).Comparison == nil || doc(latest).Comparison.BaselineRunID != second {
		t.Fatalf("last-passed: exit %d, comparison %+v:\n%s", code, doc(latest).Comparison, out)
	}

	// Baselines that cannot be used are rejected before anything runs.
	if code, out = f.cli(t, "run", passing, "--baseline", "run_zzzzzzzzzzzz"); code != 2 || !strings.Contains(out, "does not exist") {
		t.Fatalf("unknown baseline: expected exit 2, got %d:\n%s", code, out)
	}
	if code, out = f.cli(t, "run", passing, "--baseline", "yesterday"); code != 2 {
		t.Fatalf("malformed baseline: expected exit 2, got %d:\n%s", code, out)
	}
	code, out = f.cli(t, "run", other)
	if code != 0 {
		t.Fatalf("other experiment: exit %d:\n%s", code, out)
	}
	otherID := lastRunID(t, out)
	if code, out = f.cli(t, "run", passing, "--baseline", otherID); code != 2 || !strings.Contains(out, "cmp-other") {
		t.Fatalf("baseline of another experiment: expected exit 2, got %d:\n%s", code, out)
	}

	// Per-experiment history: GET /api/v1/runs?experiment=.
	var runs []struct {
		RunID      string `json:"run_id"`
		Experiment string `json:"experiment"`
	}
	f.control.api(t, "GET", "/api/v1/runs?experiment=cmp", nil, &runs)
	if len(runs) != 6 {
		t.Fatalf("expected the 6 runs of cmp, got %d: %+v", len(runs), runs)
	}
	for _, r := range runs {
		if r.Experiment != "cmp" {
			t.Fatalf("the filter leaked another experiment: %+v", r)
		}
	}
}

// rawGet fetches an authenticated control API path and returns its body.
func (c *controlProc) rawGet(t testing.TB, path string) (string, int) {
	t.Helper()
	req, _ := http.NewRequest("GET", c.url(path), nil)
	req.Header.Set("Authorization", "Bearer "+controlToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body), resp.StatusCode
}
