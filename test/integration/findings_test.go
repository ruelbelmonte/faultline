//go:build integration

package integration

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func findingIDs(d runDoc) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, f := range d.Findings {
		out[f.ID] = f.Evidence
	}
	return out
}

var retryRules = []string{"retry_amplification", "retry_without_backoff", "retry_after_ignored", "missing_timeout"}

// The same two experiments run against the naive and the good demo. The
// naive one must earn all four retry and timeout findings, the good one none.
func TestFindingsNaiveVersusGood(t *testing.T) {
	experiments := func(f *fullStack) (string, string) {
		rejecting := f.experiment(t, expOpts{
			Name: "retry-behaviour", Duration: "4s", RPS: 5, Hypothesis: "    error_rate_max: 1",
			Faults: `  - id: f1
    type: http_error
    match: {method: GET, path: "/stock/*"}
    params: {status: 503, retry_after: 1}`,
		})
		hanging := f.experiment(t, expOpts{
			Name: "timeout-behaviour", Duration: "5s", RPS: 4, Hypothesis: "    error_rate_max: 1",
			Faults: `  - id: t1
    type: timeout
    match: {method: GET, path: "/stock/*"}
    params: {hold: 2s}`,
		})
		return rejecting, hanging
	}

	t.Run("naive", func(t *testing.T) {
		f := startFull(t, "naive")
		rejecting, hanging := experiments(f)

		doc := f.control.waitDone(t, f.control.createRun(t, rejecting), 60*time.Second)
		got := findingIDs(doc)
		for _, id := range []string{"retry_amplification", "retry_without_backoff", "retry_after_ignored"} {
			if got[id] == nil {
				t.Fatalf("naive mode must trigger %s; findings: %+v", id, doc.Findings)
			}
			if got[id]["route"] != "inventory" {
				t.Errorf("%s evidence must name the route: %v", id, got[id])
			}
		}
		amp := got["retry_amplification"]
		if amp["mean_attempts"] != 4.0 || amp["max_attempts"] != 4.0 || amp["correlations"].(float64) < 10 {
			t.Errorf("naive mode makes 4 attempts per request: %v", amp)
		}
		if nb := got["retry_without_backoff"]; nb["occurrences"].(float64) < 10 || nb["median_gap_ms"].(float64) >= 50 {
			t.Errorf("retry_without_backoff evidence: %v", nb)
		}
		if ra := got["retry_after_ignored"]; ra["violations"].(float64) < 10 || ra["retry_after_s"] != 1.0 || ra["median_gap_ms"].(float64) >= 900 {
			t.Errorf("retry_after_ignored evidence: %v", ra)
		}

		doc = f.control.waitDone(t, f.control.createRun(t, hanging), 60*time.Second)
		mt := findingIDs(doc)["missing_timeout"]
		if mt == nil {
			t.Fatalf("naive mode must trigger missing_timeout; findings: %+v", doc.Findings)
		}
		if mt["route"] != "inventory" || mt["hold_ms"] != 2000.0 || mt["median_client_held_ms"].(float64) < 1800 || mt["samples"].(float64) < 1 {
			t.Errorf("missing_timeout evidence: %v", mt)
		}
	})

	t.Run("good", func(t *testing.T) {
		f := startFull(t, "good")
		rejecting, hanging := experiments(f)
		for name, yaml := range map[string]string{"rejecting": rejecting, "hanging": hanging} {
			doc := f.control.waitDone(t, f.control.createRun(t, yaml), 60*time.Second)
			if doc.Status != "completed" || doc.Faults[0].Triggers == 0 {
				t.Fatalf("%s: the experiment must have run and triggered: %s, %+v", name, doc.Status, doc.Faults)
			}
			got := findingIDs(doc)
			for _, id := range retryRules {
				if got[id] != nil {
					t.Errorf("%s: good mode must not trigger %s: %v", name, id, got[id])
				}
			}
		}
	})
}

func TestFindingsFromVerdictRules(t *testing.T) {
	f := startFull(t, "naive")

	t.Run("steady_state_breach", func(t *testing.T) {
		doc := f.control.waitDone(t, f.control.createRun(t, f.experiment(t, expOpts{Name: "breach", Duration: "2s"})), 30*time.Second)
		var breaches []map[string]any
		for _, fd := range doc.Findings {
			if fd.ID == "steady_state_breach" {
				if fd.Severity != "high" || fd.Title == "" || fd.Detail == "" {
					t.Fatalf("unexpected finding: %+v", fd)
				}
				breaches = append(breaches, fd.Evidence)
			}
		}
		if len(breaches) != 1 || breaches[0]["name"] != "error_rate_max" || breaches[0]["threshold"] != 0.05 {
			t.Fatalf("expected one breach for error_rate_max: %+v", breaches)
		}
		obs, _, _ := doc.hypothesis("error_rate_max")
		if breaches[0]["observed"] != *obs {
			t.Fatalf("evidence (%v) must carry the observed value (%v)", breaches[0]["observed"], *obs)
		}
	})

	t.Run("fault_not_triggered", func(t *testing.T) {
		yaml := f.experiment(t, expOpts{Name: "untriggered", Duration: "2s", Faults: `  - id: miss
    type: reset
    match: {method: POST, path: "/never/*"}`})
		doc := f.control.waitDone(t, f.control.createRun(t, yaml), 30*time.Second)
		ev := findingIDs(doc)["fault_not_triggered"]
		if ev == nil || ev["fault_id"] != "miss" || ev["route"] != "inventory" {
			t.Fatalf("findings: %+v", doc.Findings)
		}
		if m := ev["match"].(map[string]any); m["method"] != "POST" || m["path"] != "/never/*" {
			t.Fatalf("evidence must carry the match: %v", ev)
		}
		if doc.Findings[0].Severity != "info" {
			t.Fatalf("severity: %+v", doc.Findings[0])
		}
	})

	t.Run("slow_recovery", func(t *testing.T) {
		// Three consecutive probe passes one second apart cannot fit into a one second recovery timeout.
		yaml := f.experiment(t, expOpts{
			Name: "slow", Duration: "2s", Recovery: "1s", Precheck: "    consecutive_ok: 1\n    timeout: 5s",
			Probes: "    - {name: shop, url: http://" + f.demo.shop + "/health, interval: 1s}",
			Faults: "  - id: f1\n    type: latency\n    params: {ms: 1}",
		})
		doc := f.control.waitDone(t, f.control.createRun(t, yaml), 30*time.Second)
		ev := findingIDs(doc)["slow_recovery"]
		if doc.RecoverySeconds != nil || ev == nil || ev["recovery_seconds"] != nil || ev["timeout_s"] != 1.0 {
			t.Fatalf("recovery_seconds %v, findings %+v", doc.RecoverySeconds, doc.Findings)
		}
	})

	t.Run("a healthy run has no findings", func(t *testing.T) {
		yaml := f.experiment(t, expOpts{Name: "clean", Duration: "2s", Faults: "  - id: f1\n    type: latency\n    params: {ms: 1}"})
		doc := f.control.waitDone(t, f.control.createRun(t, yaml), 30*time.Second)
		if doc.verdict() != "passed" || doc.Findings == nil || len(doc.Findings) != 0 {
			t.Fatalf("verdict %s, findings %+v", doc.verdict(), doc.Findings)
		}
	})

	t.Run("an aborted run reports no recovery or trigger findings", func(t *testing.T) {
		id := f.control.createRun(t, longRun(f, t, expOpts{Name: "aborted", Faults: always503 + "\n  - id: miss\n    type: reset\n    match: {path: \"/never/*\"}"}))
		f.control.waitStatus(t, id, 15*time.Second, "injecting")
		f.control.api(t, "POST", "/api/v1/runs/"+id+"/abort", nil, nil)
		got := findingIDs(f.control.run(t, id))
		if got["slow_recovery"] != nil || got["fault_not_triggered"] != nil {
			t.Fatalf("an aborted run measured neither: %v", got)
		}
	})
}

func fetchReport(t testing.TB, f *fullStack, id, format string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", f.control.url("/api/v1/runs/"+id+"/report?format="+format), nil)
	req.Header.Set("Authorization", "Bearer "+controlToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

var externalRef = regexp.MustCompile(`(?i)https?:|//[a-z0-9]|url\(|@import|<link|<iframe|<img|src\s*=|xmlns|xlink`)

type junitSuite struct {
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

func TestReports(t *testing.T) {
	f := startFull(t, "naive")
	id := f.control.createRun(t, f.experiment(t, expOpts{Name: "reported", Duration: "3s", Faults: always503 + `
  - id: miss
    type: reset
    match: {path: "/never/*"}`}))

	t.Run("not available while the run is in progress", func(t *testing.T) {
		if resp, _ := fetchReport(t, f, id, "json"); resp.StatusCode != 409 {
			t.Fatalf("expected 409, got %d", resp.StatusCode)
		}
	})
	doc := f.control.waitDone(t, id, 40*time.Second)

	t.Run("json", func(t *testing.T) {
		resp, body := fetchReport(t, f, id, "json")
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("status %d content type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		var fromReport, fromAPI map[string]any
		if err := json.Unmarshal(body, &fromReport); err != nil {
			t.Fatal(err)
		}
		f.control.api(t, "GET", "/api/v1/runs/"+id, nil, &fromAPI)
		a, _ := json.Marshal(fromReport)
		b, _ := json.Marshal(fromAPI)
		if string(a) != string(b) {
			t.Fatalf("the JSON report and the run document must be the same document:\n%s\n%s", a, b)
		}
		if len(fromReport) != 15 {
			t.Fatalf("the document has %d top-level keys, the schema has 15", len(fromReport))
		}
	})

	t.Run("markdown", func(t *testing.T) {
		resp, body := fetchReport(t, f, id, "md")
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/markdown") {
			t.Fatalf("status %d content type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		md := string(body)
		for _, want := range []string{"# Faultline report: reported", "**Verdict: INCONCLUSIVE** (fault_not_triggered)", id, "## Hypothesis", "error_rate_max", "## Findings", "steady_state_breach", "## Faults", "| f1 | http_error | inventory |", "## Metrics"} {
			if !strings.Contains(md, want) {
				t.Errorf("markdown lacks %q", want)
			}
		}
	})

	t.Run("html", func(t *testing.T) {
		resp, body := fetchReport(t, f, id, "html")
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
			t.Fatalf("status %d content type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		page := string(body)
		if m := externalRef.FindString(page); m != "" {
			t.Fatalf("the HTML report refers to something outside the file: %q", m)
		}
		for _, want := range []string{"<style>", "<script>", "<svg", id, "Verdict: inconclusive", "fault-window", "line-rate", "steady_state_breach", "retry_amplification"} {
			if !strings.Contains(page, want) {
				t.Errorf("HTML lacks %q", want)
			}
		}
	})

	t.Run("junit", func(t *testing.T) {
		resp, body := fetchReport(t, f, id, "junit")
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/xml" {
			t.Fatalf("status %d content type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		var s junitSuite
		if err := xml.Unmarshal(body, &s); err != nil {
			t.Fatalf("JUnit does not parse: %v\n%s", err, body)
		}
		if s.Name != "reported" || s.Tests != len(doc.Hypothesis)+2 || len(s.Cases) != s.Tests {
			t.Fatalf("suite: %+v", s)
		}
		status := map[string]bool{}
		for _, c := range s.Cases {
			status[c.Name] = c.Failure == nil
		}
		if status["error_rate_max"] || !status["probe_success_min"] || !status["fault f1 triggered"] || status["fault miss triggered"] {
			t.Fatalf("testcases: %v", status)
		}
		if s.Failures != 2 {
			t.Fatalf("expected 2 failures, got %d", s.Failures)
		}
	})

	t.Run("format errors", func(t *testing.T) {
		for _, q := range []string{"pdf", "", "JSON"} {
			if resp, _ := fetchReport(t, f, id, q); resp.StatusCode != 400 {
				t.Errorf("format %q: expected 400, got %d", q, resp.StatusCode)
			}
		}
		if resp, _ := fetchReport(t, f, "run_zzzzzzzzzzzz", "json"); resp.StatusCode != 404 {
			t.Errorf("unknown run: expected 404, got %d", resp.StatusCode)
		}
	})

	t.Run("audit", func(t *testing.T) {
		var audit []struct {
			Action string
			RunID  string `json:"run_id"`
			Detail map[string]any
		}
		f.control.api(t, "GET", "/api/v1/audit", nil, &audit)
		formats := map[string]bool{}
		for _, a := range audit {
			if a.Action == "report_downloaded" && a.RunID == id {
				formats[a.Detail["format"].(string)] = true
			}
		}
		if len(formats) != 4 {
			t.Fatalf("each successful download is audited: %v", formats)
		}
	})

	t.Run("written to data_dir on completion", func(t *testing.T) {
		dir := filepath.Join(f.s.dataDir, "reports", id)
		for _, name := range []string{"report.json", "report.md", "report.html", "report.xml"} {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil || len(data) == 0 {
				t.Fatalf("%s: %v", name, err)
			}
		}
		data, _ := os.ReadFile(filepath.Join(dir, "report.json"))
		var onDisk runDoc
		if err := json.Unmarshal(data, &onDisk); err != nil || onDisk.RunID != id || onDisk.verdict() != doc.verdict() || len(onDisk.Findings) != len(doc.Findings) {
			t.Fatalf("report.json on disk does not match the run: %v", err)
		}
	})

	t.Run("aborted run", func(t *testing.T) {
		id := f.control.createRun(t, longRun(f, t, expOpts{Name: "aborted-report"}))
		f.control.waitStatus(t, id, 15*time.Second, "injecting")
		f.control.api(t, "POST", "/api/v1/kill", nil, nil)
		_, body := fetchReport(t, f, id, "junit")
		var s junitSuite
		if err := xml.Unmarshal(body, &s); err != nil {
			t.Fatal(err)
		}
		last := s.Cases[len(s.Cases)-1]
		if last.Name != "run not aborted" || last.Failure == nil || !strings.Contains(last.Failure.Message, "kill_switch") {
			t.Fatalf("aborted runs add a failing 'run not aborted' testcase: %+v", s.Cases)
		}
		_, page := fetchReport(t, f, id, "html")
		if !strings.Contains(string(page), `class="marker"`) || !strings.Contains(string(page), "kill_switch") {
			t.Fatal("the HTML timeline must show the abort marker")
		}
		if _, err := os.Stat(filepath.Join(f.s.dataDir, "reports", id, "report.xml")); err != nil {
			t.Fatalf("reports are written for aborted runs too: %v", err)
		}
	})
}
