package proxy

import (
	"bytes"
	"testing"
	"time"

	"faultline/internal/fault"
)

func install(t *testing.T, e *Engine, scope string, faults ...fault.Install) {
	t.Helper()
	if errs := e.Install(fault.RuleSet{RunID: "run_x", ScopeMode: scope, LeaseID: "l", Faults: faults}); len(errs) > 0 {
		t.Fatal(errs)
	}
}

func spec(id string, s fault.Strategy) fault.Install {
	end := time.Now().Add(time.Minute)
	return fault.Install{Spec: fault.Spec{ID: id, Type: fault.Reset, Route: "svc", Strategy: s}, EndAt: &end}
}

func TestDecideHonoursWindowAndScope(t *testing.T) {
	e := NewEngine([]string{"svc", "other"}, time.Minute, nil)
	now := time.Now()
	if d := e.Decide("svc", "GET", "/", "run_x", now); d.rs != nil || d.rule != nil {
		t.Fatal("no rule set: nothing may apply")
	}
	f := spec("f1", fault.Strategy{Kind: fault.Always})
	start, end := now.Add(time.Second), now.Add(2*time.Second)
	f.StartAt, f.EndAt = &start, &end
	install(t, e, "header", f)
	for _, tc := range []struct {
		at   time.Time
		want bool
	}{
		{start.Add(-time.Nanosecond), false},
		{start, true},
		{end.Add(-time.Nanosecond), true},
		{end, false},
	} {
		if got := e.Decide("svc", "GET", "/", "run_x", tc.at).rule != nil; got != tc.want {
			t.Errorf("at %s: faulted=%v, want %v", tc.at.Sub(now), got, tc.want)
		}
	}
	if e.Decide("other", "GET", "/", "run_x", start).rule != nil {
		t.Error("fault applied to another route")
	}
	if d := e.Decide("svc", "GET", "/", "run_y", start); d.rule != nil || d.scoped {
		t.Error("fault applied to another run id")
	}
}

func TestClearClosesChannelOnce(t *testing.T) {
	e := NewEngine([]string{"svc"}, time.Minute, nil)
	install(t, e, "all", spec("f1", fault.Strategy{Kind: fault.Always}))
	rs := e.active.Load()
	if !e.Clear("cleared") || !rs.isCleared() {
		t.Fatal("clear must close the rule set")
	}
	if e.Clear("cleared") {
		t.Fatal("second clear must report nothing to clear")
	}
	install(t, e, "all", spec("f1", fault.Strategy{Kind: fault.Always}))
	first := e.active.Load()
	install(t, e, "all", spec("f2", fault.Strategy{Kind: fault.Always}))
	if !first.isCleared() {
		t.Fatal("replacing a rule set must release the old one")
	}
}

func TestWatchExpiresLease(t *testing.T) {
	e := NewEngine([]string{"svc"}, 200*time.Millisecond, nil)
	stop := make(chan struct{})
	defer close(stop)
	go e.Watch(stop)
	install(t, e, "all", spec("f1", fault.Strategy{Kind: fault.Always}))
	for i := 0; i < 5; i++ {
		time.Sleep(100 * time.Millisecond)
		if err := e.Renew("l"); err != nil {
			t.Fatalf("renew %d: %v", i, err)
		}
	}
	time.Sleep(450 * time.Millisecond)
	if e.active.Load() != nil {
		t.Fatal("lease did not expire")
	}
	if err := e.Renew("l"); err != ErrNoLease {
		t.Fatalf("expected ErrNoLease, got %v", err)
	}
	events := e.Stats().Events
	if events[len(events)-1].Type != "lease_expired" {
		t.Fatalf("unexpected events: %+v", events)
	}
}

func TestRingWrapsAndPages(t *testing.T) {
	r := newRing(10)
	for i := 0; i < 25; i++ {
		run := "a"
		if i%2 == 1 {
			run = "b"
		}
		r.add(run, fault.Record{Status: i})
	}
	page := r.since("", 0)
	if len(page.Records) != 10 || page.Records[0].Seq != 16 || page.Next != 25 {
		t.Fatalf("expected the newest 10 records (seq 16..25), got %d starting at %d, next %d", len(page.Records), page.Records[0].Seq, page.Next)
	}
	for i, rec := range page.Records {
		if rec.Seq != int64(16+i) || rec.Status != 15+i {
			t.Fatalf("record %d out of order: %+v", i, rec)
		}
	}
	if page = r.since("b", 20); len(page.Records) != 2 || page.Records[0].Seq != 22 || page.Next != 25 {
		t.Fatalf("filter by run: %+v", page)
	}
	if page = r.since("", 25); len(page.Records) != 0 || page.Next != 25 {
		t.Fatalf("nothing newer: %+v", page)
	}
}

func TestGarbleSplitAcrossChunksMatchesWhole(t *testing.T) {
	whole := bytes.Repeat([]byte("abcdefghij"), 20)
	parts := append([]byte(nil), whole...)
	garble(whole, 0, 42)
	garble(parts[:33], 0, 42)
	garble(parts[33:], 33, 42)
	if !bytes.Equal(whole, parts) {
		t.Fatal("corruption must not depend on how the body is chunked")
	}
}

func TestRingReportsOverwritingUnreadRecords(t *testing.T) {
	r := newRing(4)
	for i := 0; i < 4; i++ {
		if r.add("a", fault.Record{}) {
			t.Fatalf("record %d: nothing can be lost before the ring is full", i+1)
		}
	}
	r.since("a", 0)
	for i := 0; i < 4; i++ {
		if r.add("a", fault.Record{}) {
			t.Fatalf("overwriting record %d, which was read, must not count as a loss", i+1)
		}
	}
	if !r.add("a", fault.Record{}) {
		t.Fatal("overwriting record 5, which nobody read, must be reported")
	}
	r.since("a", 1<<40)
	if r.add("a", fault.Record{}) {
		t.Fatal("after a read up to the newest record nothing unread is left to lose")
	}
	// Records 10 to 13 now fill the ring; none of them has been read.
	for i := 0; i < 3; i++ {
		r.add("a", fault.Record{})
	}
	if !r.add("a", fault.Record{}) {
		t.Fatal("a cursor beyond the newest record must not mark later records as read")
	}
}
