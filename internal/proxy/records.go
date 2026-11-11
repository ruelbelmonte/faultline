package proxy

import (
	"sync"

	"faultline/internal/fault"
)

const (
	DefaultRingSize = 200_000

	// Client-controlled strings are capped so the ring's memory is bounded
	// by its record count.
	maxRecordPath        = 512
	maxRecordCorrelation = 128
	maxRecordMethod      = 32
)

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

type entry struct {
	rec   fault.Record
	runID string
}

// ring is a bounded buffer of records with a monotonic sequence number. It
// grows on demand up to size, then overwrites the oldest record.
type ring struct {
	mu    sync.Mutex
	size  int
	buf   []entry
	start int
	seq   int64
}

func newRing(size int) *ring {
	return &ring{size: size}
}

func (r *ring) add(runID string, rec fault.Record) {
	rec.Path = clip(rec.Path, maxRecordPath)
	rec.Correlation = clip(rec.Correlation, maxRecordCorrelation)
	rec.Method = clip(rec.Method, maxRecordMethod)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	rec.Seq = r.seq
	e := entry{rec: rec, runID: runID}
	if len(r.buf) < r.size {
		r.buf = append(r.buf, e)
		return
	}
	r.buf[r.start] = e
	r.start = (r.start + 1) % r.size
}

// since returns records with seq > since for a run (any run when runID is
// empty). Next is the highest seq examined, to be passed back as since.
func (r *ring) since(runID string, since int64) fault.RecordPage {
	r.mu.Lock()
	defer r.mu.Unlock()
	page := fault.RecordPage{Records: []fault.Record{}, Next: since}
	oldest := r.seq - int64(len(r.buf)) + 1
	from := max(since+1, oldest)
	for s := from; s <= r.seq && len(page.Records) < fault.MaxRecordsPage; s++ {
		e := r.buf[(r.start+int(s-oldest))%len(r.buf)]
		page.Next = s
		if runID == "" || e.runID == runID {
			page.Records = append(page.Records, e.rec)
		}
	}
	return page
}
