package control

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"faultline/internal/config"
	"faultline/internal/experiment"
	"faultline/internal/report"
)

// A run stays the active run while it writes its reports, after its terminal
// status is already visible. Aborting it in that window must answer 409, as
// for any finished run, not 200.
func TestAbortOfARunThatIsFinishing(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Routes = []config.Route{{Name: "inventory", Upstream: "http://127.0.0.1:1"}}
	const token = "unit-test-control-token-0123"
	c, err := New(cfg, token, "unit-test-admin-token-0123", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.store.Close()

	yaml := []byte("version: 1\nname: finishing\ntarget: {route: inventory}\nduration: 1s\nfaults:\n  - id: f1\n    type: reset\n")
	exp, errs := experiment.Parse(yaml, cfg)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	run := newRun(c, exp, yaml)
	verdict := report.VerdictPassed
	run.doc.Status, run.doc.Verdict = report.StatusCompleted, &verdict
	run.persist()
	c.active = run

	abort := func() (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+run.id+"/abort", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		c.Handler().ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, body := abort(); code != http.StatusConflict || !strings.Contains(body, `"run_not_active"`) || !strings.Contains(body, `"completed"`) {
		t.Fatalf("while the run is still held as active: %d %s", code, body)
	}
	if cause := run.abortCause(); cause != nil {
		t.Fatalf("a finished run must not be marked aborted: %v", cause)
	}
	c.finished(run)
	if code, body := abort(); code != http.StatusConflict || !strings.Contains(body, `"completed"`) {
		t.Fatalf("after the run has been released: %d %s", code, body)
	}
}
