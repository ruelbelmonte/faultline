package proxy

import (
	"sync"

	"faultline/internal/fault"
)

const (
	DefaultRingSize = 200_000
	MaxRecordsPage  = 5000
)

type entry struct {
	rec   fault.Record
	runID string
}

// ring is a bounded buffer of records with a monotonic sequence number.
type ring struct {
	mu    sync.Mutex
	buf   []entry
	start int
	count int
	seq   int64
}

func newRing(size int) *ring {
	return &ring{buf: make([]entry, size)}
}

func (r *ring) add(runID string, rec fault.Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	rec.Seq = r.seq
	i := (r.start + r.count) % len(r.buf)
	r.buf[i] = entry{rec: rec, runID: runID}
	if r.count < len(r.buf) {
		r.count++
	} else {
		r.start = (r.start + 1) % len(r.buf)
	}
}

// since returns records with seq > since for a run (any run when runID is
// empty). Next is the highest seq examined, to be passed back as since.
func (r *ring) since(runID string, since int64) fault.RecordPage {
	r.mu.Lock()
	defer r.mu.Unlock()
	page := fault.RecordPage{Records: []fault.Record{}, Next: since}
	oldest := r.seq - int64(r.count) + 1
	from := max(since+1, oldest)
	for s := from; s <= r.seq && len(page.Records) < MaxRecordsPage; s++ {
		e := r.buf[(r.start+int(s-oldest))%len(r.buf)]
		page.Next = s
		if runID == "" || e.runID == runID {
			page.Records = append(page.Records, e.rec)
		}
	}
	return page
}
