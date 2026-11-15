//go:build integration

package integration

import (
	"testing"
	"time"
)

const pricing503 = `  - id: f1
    type: http_error
    route: pricing
    match: {method: GET, path: "/price/*"}
    params: {status: 503}`

// Issue #29: shop -> inventory -> pricing, where every hop forwards W3C
// baggage and none forwards X-Chaos-Run or X-Chaos-Correlation. The fault on
// the third hop still triggers, and the retry findings (which need the
// correlation id) still appear.
func TestBaggageScopingAcrossThreeHops(t *testing.T) {
	f := startFull(t, "naive", func(s *stack) { s.demoArgs = []string{"--propagate", "baggage"} })
	doc := f.control.waitDone(t, f.control.createRun(t, f.experiment(t, expOpts{Name: "baggage-3-hops", Duration: "3s", Faults: pricing503})), 60*time.Second)
	if doc.Faults[0].Triggers == 0 {
		t.Fatalf("the pricing fault must trigger through baggage alone: %s (%s) %+v", doc.verdict(), doc.reason(), doc.Faults)
	}
	if doc.verdict() != "failed" {
		t.Fatalf("pricing down behind two hops fails the shop: got %s (%s)", doc.verdict(), doc.reason())
	}
	found := map[string]bool{}
	for _, fd := range doc.Findings {
		found[fd.ID] = true
	}
	if !found["retry_amplification"] && !found["retry_without_backoff"] {
		t.Fatalf("retry findings need the correlation id from baggage: %+v", doc.Findings)
	}
}

// The control: the same three hops with baggage turned off in faultline.yaml.
// Nothing carries the run id past the shop, so the fault never triggers.
func TestBaggageTurnedOffDoesNotScope(t *testing.T) {
	f := startFull(t, "naive", func(s *stack) {
		s.demoArgs = []string{"--propagate", "baggage"}
		s.extra = "scope:\n  baggage_key: ''\ncorrelation:\n  baggage_key: ''\n"
	})
	doc := f.control.waitDone(t, f.control.createRun(t, f.experiment(t, expOpts{Name: "baggage-off", Duration: "2s", Faults: pricing503})), 60*time.Second)
	if doc.verdict() != "inconclusive" || doc.reason() != "fault_not_triggered" {
		t.Fatalf("without baggage the third hop is unscoped: got %s (%s) %+v", doc.verdict(), doc.reason(), doc.Faults)
	}
}
