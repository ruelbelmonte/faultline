package control

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"faultline/internal/fault"
	"faultline/internal/findings"
)

// randomRecords builds a run's worth of records: retried and single calls on
// two routes, some faulted, some with Retry-After, some held, some anonymous.
func randomRecords(rng *rand.Rand, n int) []fault.Record {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	faultIDs, typ := []string{"f1", "f2", "hold"}, "http_error"
	var recs []fault.Record
	at := 0.0
	for len(recs) < n {
		corr := fmt.Sprintf("corr-secret-%d", len(recs))
		if rng.IntN(10) == 0 {
			corr = ""
		}
		route := []string{"inventory", "pricing"}[rng.IntN(2)]
		for attempt, attempts := 0, 1+rng.IntN(4); attempt < attempts; attempt++ {
			at += rng.Float64() * 120
			r := fault.Record{
				Seq: int64(len(recs) + 1), TS: base.Add(time.Duration(at * float64(time.Millisecond))),
				Route: route, Method: "GET", Path: "/x", Status: 200, DurationMs: rng.Float64() * 30, Scoped: true, Correlation: corr,
			}
			if rng.IntN(2) == 0 {
				id := faultIDs[rng.IntN(len(faultIDs))]
				r.FaultID, r.FaultType, r.Status = &id, &typ, 503
				if rng.IntN(2) == 0 {
					ra := 1 + rng.IntN(2)
					r.RetryAfter = &ra
				}
				if id == "hold" {
					held := rng.Float64() * 1000
					r.ClientHeldMs, r.DurationMs, r.Status = &held, held, 0
				}
			}
			recs = append(recs, r)
		}
	}
	return recs
}

func TestStoredEvidenceGivesTheSameFindingsAsMemory(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for seed := uint64(1); seed <= 5; seed++ {
		runID := fmt.Sprintf("run_%012d", seed)
		recs := randomRecords(rand.New(rand.NewPCG(seed, 7)), 3000)
		salt := []byte("0123456789abcdef")
		// Stored the way a run stores them: page by page, with one page repeated.
		for i := 0; i < len(recs); i += 700 {
			page := recs[i:min(i+700, len(recs))]
			if err := store.SaveRecords(runID, salt, page); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.SaveRecords(runID, salt, recs[:700]); err != nil {
			t.Fatal(err)
		}

		in := findings.Input{
			RetryAmplificationThreshold: 1.5,
			TimeoutFaults:               []findings.TimeoutFault{{ID: "hold", Route: "inventory", HoldMs: 500}},
			ClearedAt:                   recs[len(recs)-1].TS,
		}
		fromDisk, err := findings.EvaluateFrom(in, store.Evidence(runID))
		if err != nil {
			t.Fatal(err)
		}
		in.Records = recs
		fromMemory := findings.Evaluate(in)
		if len(fromMemory) < 4 {
			t.Fatalf("seed %d: the generated records trigger only %d findings; the comparison would prove little", seed, len(fromMemory))
		}
		if !reflect.DeepEqual(fromDisk, fromMemory) {
			t.Errorf("seed %d: findings differ\n disk:   %+v\n memory: %+v", seed, fromDisk, fromMemory)
		}
	}

	// Another run's records never leak into a run's evidence.
	if routes, err := store.Evidence("run_unknown").FaultedRoutes(); err != nil || len(routes) != 0 {
		t.Errorf("evidence of an unknown run: %v, %v", routes, err)
	}
}

func TestStoredRecordsHoldNoCorrelationIDsAndAreDeleted(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	recs := randomRecords(rand.New(rand.NewPCG(1, 1)), 500)
	if err := store.SaveRecords("run_a", []byte("salt-salt-salt-1"), recs); err != nil {
		t.Fatal(err)
	}
	count := func() (n int) {
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM records`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count() != len(recs) {
		t.Fatalf("stored %d of %d records", count(), len(recs))
	}
	store.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("corr-secret")) {
			t.Errorf("%s contains a correlation id; only its salted hash may reach the disk", filepath.Base(f))
		}
	}

	if err := store.SaveRecords("run_b", []byte("salt-salt-salt-2"), recs[:10]); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteRecords("run_a"); err != nil {
		t.Fatal(err)
	}
	if count() != 10 {
		t.Fatalf("deleting one run's records left %d rows, want the other run's 10", count())
	}
	store.Close()

	// A restart starts without records: they belong to runs that are over.
	store, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if count() != 0 {
		t.Fatalf("%d records survived a restart", count())
	}
}
