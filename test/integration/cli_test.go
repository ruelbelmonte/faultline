//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// cli runs the faultline binary against this stack's control plane and
// returns its exit code and combined output.
func (f *fullStack) cli(t testing.TB, args ...string) (int, string) {
	t.Helper()
	return f.cliEnv(t, []string{"FAULTLINE_TOKEN=" + controlToken, "FAULTLINE_URL=" + f.control.url("")}, args...)
}

func (f *fullStack) cliEnv(t testing.TB, env []string, args ...string) (int, string) {
	t.Helper()
	p := start(t, "faultline", env, args...)
	return p.wait(3 * time.Minute), p.out.String()
}

func writeFile(t testing.TB, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

var createdLine = regexp.MustCompile(`(?m)^run run_[a-z2-7]{12} created$`)

type cliExperiments struct {
	passed, failed, inconclusive, aborted string
}

func (f *fullStack) cliExperiments(t testing.TB) cliExperiments {
	return cliExperiments{
		passed: f.experiment(t, expOpts{Name: "cli-passed", Duration: "2s", Faults: "  - id: f1\n    type: latency\n    params: {ms: 1}"}),
		failed: f.experiment(t, expOpts{Name: "cli-failed", Duration: "2s"}),
		inconclusive: f.experiment(t, expOpts{Name: "cli-inconclusive", Duration: "2s",
			Faults: "  - id: miss\n    type: reset\n    match: {path: \"/never/*\"}"}),
		aborted: f.experiment(t, expOpts{Name: "cli-aborted", Duration: "60s", Abort: "  error_rate_max: 0.5"}),
	}
}

func TestCLIRunExitCodes(t *testing.T) {
	f := startFull(t, "naive")
	x := f.cliExperiments(t)
	dir := t.TempDir()
	passed := writeFile(t, filepath.Join(dir, "passed.yaml"), x.passed)
	failed := writeFile(t, filepath.Join(dir, "failed.yaml"), x.failed)
	inconclusive := writeFile(t, filepath.Join(dir, "inconclusive.yaml"), x.inconclusive)
	aborted := writeFile(t, filepath.Join(dir, "aborted.yaml"), x.aborted)

	cases := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"passed", []string{"run", passed, "--fail-on-breach"}, 0, "verdict: passed"},
		{"failed with --fail-on-breach", []string{"run", failed, "--fail-on-breach"}, 10, "verdict: failed"},
		{"failed without --fail-on-breach", []string{"run", failed}, 0, "verdict: failed"},
		{"flag before the file", []string{"run", "--fail-on-breach", failed}, 10, "verdict: failed"},
		{"inconclusive", []string{"run", inconclusive, "--fail-on-breach"}, 11, "verdict: inconclusive (fault_not_triggered)"},
		{"inconclusive without --fail-on-breach", []string{"run", inconclusive}, 0, "verdict: inconclusive"},
		{"aborted", []string{"run", aborted, "--fail-on-breach"}, 12, "verdict: aborted (threshold:error_rate_max)"},
		{"aborted without --fail-on-breach", []string{"run", aborted}, 0, "verdict: aborted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := f.cli(t, tc.args...)
			if code != tc.code || !strings.Contains(out, tc.want) {
				t.Fatalf("expected exit %d and %q, got exit %d:\n%s", tc.code, tc.want, code, out)
			}
			if !createdLine.MatchString(out) {
				t.Fatalf("the run id should be printed:\n%s", out)
			}
		})
	}

	t.Run("progress is streamed", func(t *testing.T) {
		_, out := f.cli(t, "run", failed)
		for _, want := range []string{"prechecking", "injecting", "recovering", "completed", "requests", "hypothesis error_rate_max", "fault f1", "finding [high] steady_state_breach"} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
	})
}

func TestCLIRunDirectoryUsesWorstExitCode(t *testing.T) {
	f := startFull(t, "naive")
	x := f.cliExperiments(t)

	// 10 (failed) outranks 11 (inconclusive).
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a-passed.yaml"), x.passed)
	writeFile(t, filepath.Join(dir, "b-inconclusive.yml"), x.inconclusive)
	writeFile(t, filepath.Join(dir, "c-failed.yaml"), x.failed)
	writeFile(t, filepath.Join(dir, "notes.txt"), "not an experiment")
	code, out := f.cli(t, "run", dir, "--fail-on-breach")
	if code != 10 {
		t.Fatalf("passed + inconclusive + failed: expected exit 10, got %d:\n%s", code, out)
	}
	var runs []map[string]any
	f.control.api(t, "GET", "/api/v1/runs", nil, &runs)
	if len(runs) != 3 || runs[2]["experiment"] != "cli-passed" || runs[1]["experiment"] != "cli-inconclusive" || runs[0]["experiment"] != "cli-failed" {
		t.Fatalf("files must run one after another in name order: %v", runs)
	}
	if !strings.Contains(out, "Summary:") || strings.Count(out, "verdict: ") != 3 {
		t.Fatalf("expected a summary of 3 runs:\n%s", out)
	}
	if code, _ := f.cli(t, "run", dir); code != 0 {
		t.Fatalf("without --fail-on-breach the exit code is 0, got %d", code)
	}

	// 11 alone, and 12 (aborted) outranks 10.
	only := t.TempDir()
	writeFile(t, filepath.Join(only, "a.yaml"), x.passed)
	writeFile(t, filepath.Join(only, "b.yaml"), x.inconclusive)
	if code, out := f.cli(t, "run", only, "--fail-on-breach"); code != 11 {
		t.Fatalf("passed + inconclusive: expected exit 11, got %d:\n%s", code, out)
	}
	writeFile(t, filepath.Join(only, "c.yaml"), x.aborted)
	writeFile(t, filepath.Join(only, "d.yaml"), x.failed)
	if code, out := f.cli(t, "run", only, "--fail-on-breach"); code != 12 {
		t.Fatalf("with an aborted run: expected exit 12, got %d:\n%s", code, out)
	}
}

func TestCLIRunErrors(t *testing.T) {
	f := startFull(t, "naive")
	x := f.cliExperiments(t)
	dir := t.TempDir()
	good := writeFile(t, filepath.Join(dir, "good.yaml"), x.passed)
	bad := writeFile(t, filepath.Join(dir, "bad.yaml"), strings.Replace(x.passed, "type: latency", "type: gremlin", 1))

	t.Run("invalid experiment", func(t *testing.T) {
		code, out := f.cli(t, "run", bad, "--fail-on-breach")
		if code != 2 {
			t.Fatalf("expected exit 2, got %d:\n%s", code, out)
		}
		found := false
		for _, line := range strings.Split(out, "\n") {
			found = found || strings.HasPrefix(line, "faults[0].type: ")
		}
		if !found {
			t.Fatalf("validation errors are printed as path: message:\n%s", out)
		}
		var runs []any
		f.control.api(t, "GET", "/api/v1/runs", nil, &runs)
		if len(runs) != 0 {
			t.Fatal("an invalid experiment must not create a run")
		}
	})

	usage := [][]string{
		{"run"},
		{"run", filepath.Join(dir, "missing.yaml")},
		{"run", good, "--format", "pdf", "--report-dir", dir},
		{"run", good, "--format", "json"},
		{"run", good, "--no-such-flag"},
		{"run", good, good},
		{"report", "run_aaaaaaaaaaaa"},
		{"report", "run_aaaaaaaaaaaa", "--format", "pdf"},
		{"report"},
	}
	for _, args := range usage {
		if code, out := f.cli(t, args...); code != 2 {
			t.Errorf("%v: expected exit 2, got %d:\n%s", args, code, out)
		}
	}

	t.Run("missing token", func(t *testing.T) {
		for _, args := range [][]string{{"run", good}, {"status"}, {"stop"}, {"report", "run_aaaaaaaaaaaa", "--format", "json"}} {
			if code, out := f.cliEnv(t, []string{"FAULTLINE_TOKEN=", "FAULTLINE_URL=" + f.control.url("")}, args...); code != 2 || !strings.Contains(out, "FAULTLINE_TOKEN") {
				t.Errorf("%v: expected exit 2 naming FAULTLINE_TOKEN, got %d:\n%s", args, code, out)
			}
		}
	})

	t.Run("wrong token", func(t *testing.T) {
		code, out := f.cliEnv(t, []string{"FAULTLINE_TOKEN=wrong-token-0123456789", "FAULTLINE_URL=" + f.control.url("")}, "run", good)
		if code != 1 || !strings.Contains(out, "401") {
			t.Fatalf("expected exit 1 mentioning 401, got %d:\n%s", code, out)
		}
		if strings.Contains(out, "wrong-token-0123456789") {
			t.Fatal("the token must not be printed")
		}
	})

	t.Run("control plane unreachable", func(t *testing.T) {
		dead := "http://" + freeAddr(t)
		for _, args := range [][]string{{"run", good, "--url", dead}, {"status", "--url", dead}, {"stop", "--url", dead}, {"report", "run_aaaaaaaaaaaa", "--format", "json", "--url", dead}} {
			if code, out := f.cli(t, args...); code != 1 {
				t.Errorf("%v: expected exit 1, got %d:\n%s", args, code, out)
			}
		}
	})

	t.Run("--url overrides FAULTLINE_URL", func(t *testing.T) {
		env := []string{"FAULTLINE_TOKEN=" + controlToken, "FAULTLINE_URL=http://" + freeAddr(t)}
		if code, out := f.cliEnv(t, env, "status", "--url", f.control.url("")); code != 0 {
			t.Fatalf("expected exit 0, got %d:\n%s", code, out)
		}
	})

	t.Run("a run is already active", func(t *testing.T) {
		id := f.control.createRun(t, longRun(f, t, expOpts{Name: "busy"}))
		code, out := f.cli(t, "run", good)
		if code != 1 || !strings.Contains(out, "409") {
			t.Fatalf("expected exit 1 mentioning the conflict, got %d:\n%s", code, out)
		}
		f.control.api(t, "POST", "/api/v1/runs/"+id+"/abort", nil, nil)
	})
}

func TestCLIRunWritesReports(t *testing.T) {
	f := startFull(t, "naive")
	x := f.cliExperiments(t)
	dir := t.TempDir()
	exp := writeFile(t, filepath.Join(dir, "exp.yaml"), x.failed)

	runDir := func(t *testing.T, reports string) string {
		entries, err := os.ReadDir(reports)
		if err != nil || len(entries) != 1 || !runIDRE.MatchString(entries[0].Name()) {
			t.Fatalf("expected one directory named after the run id in %s: %v %v", reports, entries, err)
		}
		return filepath.Join(reports, entries[0].Name())
	}
	names := func(t *testing.T, dir string) string {
		entries, _ := os.ReadDir(dir)
		var out []string
		for _, e := range entries {
			out = append(out, e.Name())
		}
		return strings.Join(out, ",")
	}

	t.Run("all four formats by default", func(t *testing.T) {
		reports := filepath.Join(dir, "all")
		code, out := f.cli(t, "run", exp, "--report-dir", reports, "--fail-on-breach")
		if code != 10 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		rd := runDir(t, reports)
		if got := names(t, rd); got != "report.html,report.json,report.md,report.xml" {
			t.Fatalf("files: %s", got)
		}
		data, _ := os.ReadFile(filepath.Join(rd, "report.json"))
		var doc runDoc
		if err := json.Unmarshal(data, &doc); err != nil || doc.RunID != filepath.Base(rd) || doc.verdict() != "failed" {
			t.Fatalf("report.json: %v %+v", err, doc)
		}
		xmlData, _ := os.ReadFile(filepath.Join(rd, "report.xml"))
		if !strings.Contains(string(xmlData), "<testsuite") {
			t.Fatal("report.xml is not a JUnit report")
		}
		html, _ := os.ReadFile(filepath.Join(rd, "report.html"))
		if m := externalRef.FindString(string(html)); m != "" || !strings.Contains(string(html), "<svg") {
			t.Fatalf("report.html must be self-contained with an inline chart (found %q)", m)
		}
		md, _ := os.ReadFile(filepath.Join(rd, "report.md"))
		if !strings.Contains(string(md), "**Verdict: FAILED**") {
			t.Fatal("report.md lacks the verdict line")
		}
	})

	t.Run("--format selects formats", func(t *testing.T) {
		reports := filepath.Join(dir, "some")
		if code, out := f.cli(t, "run", exp, "--report-dir", reports, "--format", "json,junit"); code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if got := names(t, runDir(t, reports)); got != "report.json,report.xml" {
			t.Fatalf("files: %s", got)
		}
	})
}

func TestCLIStatusStopReport(t *testing.T) {
	f := startFull(t, "naive")
	x := f.cliExperiments(t)

	code, out := f.cli(t, "status")
	if code != 0 || !strings.Contains(out, "active run: none") || !strings.Contains(out, "active faults: none") {
		t.Fatalf("idle status: exit %d:\n%s", code, out)
	}

	id := f.control.createRun(t, longRun(f, t, expOpts{Name: "status"}))
	f.control.waitStatus(t, id, 15*time.Second, "injecting")
	eventually(t, 5*time.Second, "fault active", func() bool { return f.faultedThroughProxy(id) })
	code, out = f.cli(t, "status")
	if code != 0 || !strings.Contains(out, "active run: "+id) || !strings.Contains(out, "injecting") || !strings.Contains(out, "active fault: f1 http_error on route inventory") {
		t.Fatalf("status during a run: exit %d:\n%s", code, out)
	}

	begin := time.Now()
	code, out = f.cli(t, "stop")
	if code != 0 {
		t.Fatalf("stop: exit %d:\n%s", code, out)
	}
	if f.faultedThroughProxy(id) {
		t.Fatalf("faults still active %s after `faultline stop`", time.Since(begin))
	}
	if doc := f.control.run(t, id); doc.Status != "aborted" || doc.reason() != "kill_switch" {
		t.Fatalf("stop is the kill switch: %s (%s)", doc.Status, doc.reason())
	}
	code, out = f.cli(t, "status")
	if code != 0 || !strings.Contains(out, "active run: none") || !strings.Contains(out, "last run: "+id) {
		t.Fatalf("status after stop: exit %d:\n%s", code, out)
	}

	t.Run("report", func(t *testing.T) {
		code, out := f.cli(t, "report", id, "--format", "md")
		if code != 0 || !strings.Contains(out, "**Verdict: ABORTED** (kill_switch)") {
			t.Fatalf("report to stdout: exit %d:\n%s", code, out)
		}
		code, out = f.cli(t, "report", id, "--format", "json")
		var doc runDoc
		if code != 0 || json.Unmarshal([]byte(out), &doc) != nil || doc.RunID != id {
			t.Fatalf("the JSON report on stdout must be exactly the document: exit %d:\n%s", code, out)
		}
		target := filepath.Join(t.TempDir(), "out.xml")
		if code, out := f.cli(t, "report", "--format", "junit", "--out", target, id); code != 0 {
			t.Fatalf("report --out: exit %d:\n%s", code, out)
		}
		data, err := os.ReadFile(target)
		if err != nil || !strings.Contains(string(data), `<testcase name="run not aborted"`) {
			t.Fatalf("--out file: %v\n%s", err, data)
		}
		if code, _ := f.cli(t, "report", "run_zzzzzzzzzzzz", "--format", "json"); code != 1 {
			t.Fatalf("unknown run: expected exit 1, got %d", code)
		}
	})

	t.Run("the init example passes against the demo", func(t *testing.T) {
		_ = x
		dir := t.TempDir()
		if code, out := f.cli(t, "init", dir); code != 0 {
			t.Fatalf("init: exit %d:\n%s", code, out)
		}
		// The generated example targets the default demo ports; point it at this test's demo.
		data, _ := os.ReadFile(filepath.Join(dir, "experiments", "example.yaml"))
		example := strings.ReplaceAll(string(data), "127.0.0.1:9000", f.demo.shop)
		example = strings.Replace(example, "duration: 15s", "duration: 4s", 1)
		path := writeFile(t, filepath.Join(dir, "experiments", "example.yaml"), example)
		code, out := f.cli(t, "run", path, "--fail-on-breach")
		if code != 0 || !strings.Contains(out, "verdict: passed") {
			t.Fatalf("the example experiment should pass against the naive demo: exit %d:\n%s", code, out)
		}
	})
}
