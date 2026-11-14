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
