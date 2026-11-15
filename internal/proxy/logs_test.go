package proxy

import (
	"bytes"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"faultline/internal/config"
)

// closedAddr returns a loopback address nothing listens on.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// Issue #12: a failed upstream call is logged with the route, the method and
// a class of error, in both log formats, and never with the URL or its query.
func TestUpstreamErrorLogHasNoURL(t *testing.T) {
	addr := closedAddr(t)
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			var h slog.Handler = slog.NewTextHandler(&buf, nil)
			if format == "json" {
				h = slog.NewJSONHandler(&buf, nil)
			}
			cfg := config.Default()
			cfg.Routes = []config.Route{{Name: "svc", Upstream: "http://" + addr + "/base"}}
			p, err := New(cfg, slog.New(h))
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, httptest.NewRequest("GET", "/svc/patients/lookup?patient_ref=synthetic-4242&secret=abc123", nil))
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("expected 502, got %d", rec.Code)
			}
			logs := buf.String()
			for _, leak := range []string{"synthetic-4242", "abc123", "patient_ref", "/patients/lookup", addr, "/base"} {
				if strings.Contains(logs, leak) {
					t.Fatalf("the log contains %q:\n%s", leak, logs)
				}
			}
			for _, want := range []string{"upstream request failed", "svc", "GET", "connection_refused"} {
				if !strings.Contains(logs, want) {
					t.Fatalf("the log should contain %q:\n%s", want, logs)
				}
			}
		})
	}
}

// Issue #29: the run id and correlation id come from the headers, or else
// from W3C baggage; the header wins; baggage can be turned off.
func TestScopeFromHeaderOrBaggage(t *testing.T) {
	cfg := config.Default()
	p := &Proxy{cfg: cfg}
	req := func(headers map[string]string) *http.Request {
		r := httptest.NewRequest("GET", "/svc/x", nil)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		return r
	}
	if got := p.scopeOf(req(map[string]string{"Baggage": "tenant=t1, chaos-run=run_abcdefghijkl"})); got != "run_abcdefghijkl" {
		t.Fatalf("scope from baggage: %q", got)
	}
	if got := p.correlationOf(req(map[string]string{"Baggage": "chaos-corr=c-1"})); got != "c-1" {
		t.Fatalf("correlation from baggage: %q", got)
	}
	if got := p.scopeOf(req(map[string]string{"X-Chaos-Run": "run_header00000", "Baggage": "chaos-run=run_baggage0000"})); got != "run_header00000" {
		t.Fatalf("the header must win: %q", got)
	}
	if got := p.scopeOf(req(map[string]string{"Baggage": "chaos-run=run_abcdefghijkl," + strings.Repeat("k=v,", 3000)})); got != "" {
		t.Fatalf("oversized baggage must be ignored: %q", got)
	}
	cfg.ScopeBaggageKey = ""
	if got := p.scopeOf(req(map[string]string{"Baggage": "chaos-run=run_abcdefghijkl"})); got != "" {
		t.Fatalf("baggage turned off: %q", got)
	}
}
