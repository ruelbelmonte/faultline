package control

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"faultline/internal/config"
	"faultline/internal/experiment"
	"faultline/internal/report"
)

// Issue #31: control's /metrics needs the metrics token, not a control token,
// parses, and counts runs entering each status.
func TestControlMetrics(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Routes = []config.Route{{Name: "inventory", Upstream: "http://127.0.0.1:1"}}
	const token, metricsToken = "unit-test-control-token-0123", "unit-test-metrics-token-0123"
	c, err := New(cfg, testTokens(token), "unit-test-admin-token-0123", metricsToken, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.store.Close()
	yaml := []byte("version: 1\nname: metrics\ntarget: {route: inventory}\nduration: 1s\nfaults:\n  - id: f1\n    type: reset\n")
	exp, errs := experiment.Parse(yaml, cfg)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	run := newRun(c, exp, yaml)
	run.setStatus(report.StatusPrechecking)
	run.setStatus(report.StatusInjecting)

	get := func(bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/metrics", nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		c.Handler().ServeHTTP(rec, req)
		return rec
	}
	if rec := get(token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a control token must not read /metrics: %d", rec.Code)
	}
	rec := get(metricsToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(rec.Body)
	if err != nil {
		t.Fatalf("the scrape does not parse: %v", err)
	}
	got := map[string]float64{}
	for _, m := range families["faultline_control_run_status_transitions_total"].GetMetric() {
		if len(m.Label) != 1 || m.Label[0].GetName() != "status" {
			t.Fatalf("unexpected labels: %v", m.Label)
		}
		got[m.Label[0].GetValue()] = m.GetCounter().GetValue()
	}
	want := map[string]float64{"prechecking": 1, "injecting": 1, "recovering": 0, "completed": 0, "aborted": 0}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
