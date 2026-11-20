package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"faultline/internal/config"
	"faultline/internal/fault"
)

const testMetricsToken = "unit-test-metrics-token-0123"

func scrape(t *testing.T, h http.Handler, token string) (int, map[string]*dto.MetricFamily) {
	t.Helper()
	req := httptest.NewRequest("GET", "/metrics", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return rec.Code, nil
	}
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(rec.Body)
	if err != nil {
		t.Fatalf("the scrape does not parse: %v", err)
	}
	return rec.Code, families
}

func value(t *testing.T, families map[string]*dto.MetricFamily, name, label, labelValue string) float64 {
	t.Helper()
	f := families[name]
	if f == nil {
		t.Fatalf("no metric %s", name)
	}
	for _, m := range f.Metric {
		if label != "" && !slices.ContainsFunc(m.Label, func(l *dto.LabelPair) bool { return l.GetName() == label && l.GetValue() == labelValue }) {
			continue
		}
		switch {
		case m.Counter != nil:
			return m.Counter.GetValue()
		case m.Gauge != nil:
			return m.Gauge.GetValue()
		case m.Histogram != nil:
			return float64(m.Histogram.GetSampleCount())
		}
	}
	t.Fatalf("no %s{%s=%q}", name, label, labelValue)
	return 0
}

// Issue #31: the proxy's /metrics parses, counts what happened, and its labels
// are only route names from the config and fault types from the code.
func TestProxyMetrics(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer up.Close()
	cfg := config.Default()
	cfg.Routes = []config.Route{{Name: "ok", Upstream: up.URL}, {Name: "down", Upstream: "http://" + closedAddr(t)}}
	p, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(time.Minute)
	if errs := p.engine.Install(fault.RuleSet{RunID: "run_x", ScopeMode: "all", LeaseID: "l", Faults: []fault.Install{{
		Spec: fault.Spec{ID: "f1", Type: fault.HTTPError, Route: "ok", Match: fault.Match{Path: "/faulted"},
			Strategy: fault.Strategy{Kind: fault.Always}, Params: map[string]any{"status": 503}}, EndAt: &end}}}); len(errs) > 0 {
		t.Fatal(errs)
	}
	for _, path := range []string{"/ok/a", "/ok/faulted", "/down/a", "/unknown/a", "/ok/../etc"} {
		p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}

	admin := p.Admin("unit-test-admin-token-0123", testMetricsToken)
	if code, _ := scrape(t, admin, ""); code != http.StatusUnauthorized {
		t.Fatalf("no token: expected 401, got %d", code)
	}
	if code, _ := scrape(t, admin, "unit-test-admin-token-0123"); code != http.StatusUnauthorized {
		t.Fatalf("the admin token must not read /metrics: got %d", code)
	}
	_, families := scrape(t, admin, testMetricsToken)

	for _, c := range []struct {
		name, label, value string
		want               float64
	}{
		{"faultline_proxy_requests_total", "route", "ok", 2},
		{"faultline_proxy_requests_total", "route", "down", 1},
		{"faultline_proxy_request_duration_seconds", "route", "ok", 2},
		{"faultline_proxy_upstream_errors_total", "route", "down", 1},
		{"faultline_proxy_upstream_errors_total", "route", "ok", 0},
		{"faultline_proxy_faults_triggered_total", "type", fault.HTTPError, 1},
		{"faultline_proxy_faults_triggered_total", "type", fault.Latency, 0},
		{"faultline_proxy_lease_active", "", "", 1},
		{"faultline_proxy_lease_expired_total", "", "", 0},
		{"faultline_proxy_records_dropped_total", "", "", 0},
	} {
		if got := value(t, families, c.name, c.label, c.value); got != c.want {
			t.Errorf("%s{%s=%q} = %v, want %v", c.name, c.label, c.value, got, c.want)
		}
	}
	if age := value(t, families, "faultline_proxy_lease_seconds_since_renewal", "", ""); age < 0 || age > 60 {
		t.Errorf("lease age %v out of range", age)
	}

	allowed := map[string][]string{"route": {"ok", "down"}, "type": fault.Types}
	for name, f := range families {
		for _, m := range f.Metric {
			for _, l := range m.Label {
				if l.GetName() == "le" && f.GetType() == dto.MetricType_HISTOGRAM {
					continue
				}
				if !slices.Contains(allowed[l.GetName()], l.GetValue()) {
					t.Errorf("%s has an unbounded or unexpected label %s=%q", name, l.GetName(), l.GetValue())
				}
			}
		}
	}

	p.engine.Clear("cleared")
	_, families = scrape(t, admin, testMetricsToken)
	if v := value(t, families, "faultline_proxy_lease_active", "", ""); v != 0 {
		t.Errorf("lease_active after clear = %v", v)
	}
}

func TestProxyMetricsOffWithoutToken(t *testing.T) {
	cfg := config.Default()
	p, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	admin := p.Admin("unit-test-admin-token-0123", "")
	for token, want := range map[string]int{"": http.StatusUnauthorized, "unit-test-admin-token-0123": http.StatusNotFound} {
		if code, _ := scrape(t, admin, token); code != want {
			t.Errorf("token %q: expected %d, got %d", token, want, code)
		}
	}
}

func TestProxyMetricsCountLeaseExpiry(t *testing.T) {
	cfg := config.Default()
	cfg.Routes = []config.Route{{Name: "svc", Upstream: "http://127.0.0.1:1"}}
	cfg.LeaseTimeout = 100 * time.Millisecond
	p, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go p.engine.Watch(stop)
	install(t, p.engine, "all", spec("f1", fault.Strategy{Kind: fault.Always}))
	deadline := time.Now().Add(5 * time.Second)
	for p.engine.active.Load() != nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	_, families := scrape(t, p.Admin("unit-test-admin-token-0123", testMetricsToken), testMetricsToken)
	if v := value(t, families, "faultline_proxy_lease_expired_total", "", ""); v != 1 {
		t.Fatalf("lease_expired_total = %v, want 1", v)
	}
}

func TestProxyMetricsCountDroppedRecords(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	cfg := config.Default()
	cfg.Routes = []config.Route{{Name: "svc", Upstream: up.URL}}
	p, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	p.records = newRing(1)
	end := time.Now().Add(time.Minute)
	install(t, p.engine, "all", fault.Install{Spec: fault.Spec{ID: "f1", Type: fault.Latency, Route: "svc",
		Strategy: fault.Strategy{Kind: fault.Always}, Params: map[string]any{"ms": 1}}, EndAt: &end})
	for range 3 {
		p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/svc/a", nil))
	}
	_, families := scrape(t, p.Admin("unit-test-admin-token-0123", testMetricsToken), testMetricsToken)
	if v := value(t, families, "faultline_proxy_records_dropped_total", "", ""); v != 2 {
		t.Fatalf("records_dropped_total = %v, want 2", v)
	}
}
