package control

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"faultline/internal/config"
	"faultline/internal/experiment"
	"faultline/internal/fault"
)

// The proxy keeps recording until control clears the faults, which is a
// moment after the injection phase ends. Requests in that moment are past
// every fault window, so they are unfaulted; they must not count towards
// metrics.proxy, which describes the injection phase, but they stay
// available to the finding rules.
func TestCollectCountsOnlyTheInjectionPhase(t *testing.T) {
	injStart := time.Now().Add(-10 * time.Second).UTC()
	injEnd := injStart.Add(4 * time.Second)
	id, typ := "f1", fault.HTTPError
	rec := func(seq int64, at time.Time, faulted bool) fault.Record {
		r := fault.Record{Seq: seq, TS: at, Route: "inventory", Method: "GET", Path: "/stock/1", Status: 200, DurationMs: 1, Scoped: true, Correlation: "c"}
		if faulted {
			r.FaultID, r.FaultType, r.Status = &id, &typ, 503
		}
		return r
	}
	records := []fault.Record{
		rec(1, injStart.Add(time.Second), true),
		rec(2, injEnd.Add(-time.Millisecond), true),
		rec(3, injEnd, false),
		rec(4, injEnd.Add(30*time.Millisecond), false),
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/v1/records":
			page := fault.RecordPage{Records: []fault.Record{}, Next: 4}
			if r.URL.Query().Get("since") == "0" {
				page.Records = records
			}
			json.NewEncoder(w).Encode(page)
		default:
			json.NewEncoder(w).Encode(fault.Stats{})
		}
	}))
	defer proxy.Close()

	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ProxyAdminURL = proxy.URL
	cfg.Routes = []config.Route{{Name: "inventory", Upstream: "http://127.0.0.1:1"}}
	c, err := New(cfg, "unit-test-control-token-0123", "unit-test-admin-token-0123", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.store.Close()
	yaml := []byte("version: 1\nname: collect\ntarget: {route: inventory}\nduration: 4s\nfaults:\n  - id: f1\n    type: http_error\n    params: {status: 503}\n")
	exp, errs := experiment.Parse(yaml, cfg)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	run := newRun(c, exp, yaml)
	run.injStart = injStart

	run.collect()

	if got := run.doc.Metrics.Proxy["inventory"]; got.Requests != 2 || got.Faulted != 2 {
		t.Errorf("metrics.proxy must cover the injection phase only: %+v, want 2 requests, 2 faulted", got)
	}
	total := 0
	for _, b := range run.buckets {
		total += b.Routes["inventory"].Requests
	}
	if total != 2 {
		t.Errorf("the per-second buckets hold %d proxy requests, want 2", total)
	}
	var stored int
	if err := c.store.db.QueryRow(`SELECT COUNT(*) FROM records WHERE run_id = ?`, run.id).Scan(&stored); err != nil || stored != 4 {
		t.Errorf("all four records must be kept as evidence for findings: %d, %v", stored, err)
	}
}
