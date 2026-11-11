package findings

import (
	"sort"
	"time"

	"faultline/internal/fault"
)

// Attempt is one request of a correlation id on a route.
type Attempt struct {
	TS         time.Time
	DurationMs float64
	Faulted    bool
	RetryAfter *int
}

func (a Attempt) end() time.Time {
	return a.TS.Add(time.Duration(a.DurationMs * float64(time.Millisecond)))
}

// Hold is one request a timeout fault held open.
type Hold struct {
	HeldMs float64
	End    time.Time
}

// Evidence is a run's own proxied requests, in the views the rules need. The
// control plane serves it from disk so a run's records need not fit in memory.
type Evidence interface {
	// FaultedRoutes lists the routes on which at least one request was faulted.
	FaultedRoutes() ([]string, error)
	// Attempts calls fn once per correlation id per route, with that
	// correlation's requests in arrival order. Requests without a
	// correlation id are left out.
	Attempts(fn func(route string, attempts []Attempt)) error
	// Holds calls fn for every request the fault held open.
	Holds(faultID string, fn func(Hold)) error
}

// FromRecords serves evidence from records held in memory.
func FromRecords(records []fault.Record) Evidence { return recordSlice(records) }

type recordSlice []fault.Record

func (rs recordSlice) FaultedRoutes() ([]string, error) {
	seen := map[string]bool{}
	var routes []string
	for _, r := range rs {
		if r.FaultID != nil && !seen[r.Route] {
			seen[r.Route] = true
			routes = append(routes, r.Route)
		}
	}
	return routes, nil
}

func (rs recordSlice) Attempts(fn func(route string, attempts []Attempt)) error {
	type key struct{ route, correlation string }
	groups := map[key][]Attempt{}
	var keys []key
	for _, r := range rs {
		if r.Correlation == "" {
			continue
		}
		k := key{r.Route, r.Correlation}
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], Attempt{TS: r.TS, DurationMs: r.DurationMs, Faulted: r.FaultID != nil, RetryAfter: r.RetryAfter})
	}
	for _, k := range keys {
		as := groups[k]
		sort.SliceStable(as, func(i, j int) bool { return as[i].TS.Before(as[j].TS) })
		fn(k.route, as)
	}
	return nil
}

func (rs recordSlice) Holds(faultID string, fn func(Hold)) error {
	for _, r := range rs {
		if r.FaultID != nil && *r.FaultID == faultID && r.ClientHeldMs != nil {
			fn(Hold{HeldMs: *r.ClientHeldMs, End: end(r)})
		}
	}
	return nil
}
