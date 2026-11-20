//go:build integration

package integration

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Issue #31: control started with FAULTLINE_METRICS_TOKEN serves /metrics
// to that token only, and counts a finished run's statuses.
func TestControlMetricsAfterARun(t *testing.T) {
	const metricsToken = "integration-metrics-token-0123456789"
	f := startFull(t, "naive", func(s *stack) { s.controlEnv = []string{"FAULTLINE_METRICS_TOKEN=" + metricsToken} })
	id := f.control.createRun(t, f.experiment(t, expOpts{Name: "metrics", Duration: "2s"}))
	f.control.waitDone(t, id, 30*time.Second)

	scrape := func(token string) (int, string) {
		req, _ := http.NewRequest("GET", f.control.url("/metrics"), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if code, _ := scrape(controlToken); code != http.StatusUnauthorized {
		t.Fatalf("the control token must not read /metrics: %d", code)
	}
	code, body := scrape(metricsToken)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	for _, want := range []string{
		`faultline_control_run_status_transitions_total{status="injecting"} 1`,
		`faultline_control_run_status_transitions_total{status="completed"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, id) {
		t.Fatal("metrics must not carry run ids")
	}
}
