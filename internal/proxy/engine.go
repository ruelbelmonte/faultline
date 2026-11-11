// Package proxy is the Faultline data plane: a reverse proxy with a fault
// engine, lease enforcement and request records.
package proxy

import (
	"errors"
	"hash/fnv"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"faultline/internal/fault"
	"faultline/internal/yamlx"
)

var (
	ErrNoLease       = errors.New("no_active_lease")
	ErrLeaseMismatch = errors.New("lease_mismatch")
)

type rule struct {
	spec    fault.Spec
	params  fault.Params
	startAt time.Time
	endAt   time.Time
	seed    uint64

	mu       sync.Mutex
	matches  int64
	rng      *rand.Rand
	triggers atomic.Int64
}

// decide counts a matching request and applies the strategy.
func (r *rule) decide() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.matches++
	hit := false
	switch r.spec.Strategy.Kind {
	case fault.Always:
		hit = true
	case fault.Probability:
		hit = r.rng.Float64() < r.spec.Strategy.P
	case fault.Burst:
		hit = r.matches <= int64(r.spec.Strategy.Count)
	case fault.EveryNth:
		hit = r.matches%int64(r.spec.Strategy.N) == 0
	}
	if hit {
		r.triggers.Add(1)
	}
	return hit
}

type ruleSet struct {
	runID    string
	scopeAll bool
	leaseID  string
	rules    []*rule
	// cleared is closed when the rule set stops applying, which releases
	// in-flight holds, delays and throttles.
	cleared chan struct{}
}

func (rs *ruleSet) isCleared() bool {
	select {
	case <-rs.cleared:
		return true
	default:
		return false
	}
}

type routeCounter struct {
	requests atomic.Int64
	faulted  atomic.Int64
}

// Engine owns the active rule set. Readers use the atomic pointer; install,
// renew and clear serialise on mu.
type Engine struct {
	leaseTimeout time.Duration
	routes       map[string]*routeCounter

	active atomic.Pointer[ruleSet]

	mu        sync.Mutex
	lastRenew time.Time
	last      *ruleSet
	events    []fault.Event
	onEvent   func(fault.Event)
}

func NewEngine(routeNames []string, leaseTimeout time.Duration, onEvent func(fault.Event)) *Engine {
	e := &Engine{leaseTimeout: leaseTimeout, routes: map[string]*routeCounter{}, onEvent: onEvent}
	for _, n := range routeNames {
		e.routes[n] = &routeCounter{}
	}
	return e
}

// Validate checks an install request and builds the rule set.
func (e *Engine) build(req fault.RuleSet, now time.Time) (*ruleSet, yamlx.Errors) {
	var errs yamlx.Errors
	if req.RunID == "" {
		errs.Add("run_id", "is required")
	}
	if req.LeaseID == "" {
		errs.Add("lease_id", "is required")
	}
	rs := &ruleSet{runID: req.RunID, leaseID: req.LeaseID, cleared: make(chan struct{})}
	switch req.ScopeMode {
	case "header":
	case "all":
		rs.scopeAll = true
	default:
		errs.Add("scope_mode", "must be header or all")
	}
	ids := map[string]bool{}
	for i, f := range req.Faults {
		path := "faults[" + itoa(i) + "]"
		if f.ID == "" {
			errs.Add(path+".id", "is required")
		} else if ids[f.ID] {
			errs.Add(path+".id", "duplicate fault id")
		}
		ids[f.ID] = true
		if _, ok := e.routes[f.Route]; !ok {
			errs.Add(path+".route", "unknown route")
		}
		r := &rule{spec: f.Spec, startAt: now}
		if !fault.KnownType(f.Type) {
			errs.Add(path+".type", "unknown fault type")
		} else {
			if f.Params == nil {
				f.Params = map[string]any{}
			}
			r.params = fault.ParseParams(f.Type, f.Params, path+".params", &errs)
		}
		fault.ValidateStrategy(&r.spec.Strategy, path+".strategy", &errs)
		if f.StartAt != nil {
			r.startAt = *f.StartAt
		}
		if f.EndAt == nil {
			errs.Add(path+".end_at", "is required")
		} else {
			r.endAt = *f.EndAt
			if !r.endAt.After(r.startAt) {
				errs.Add(path+".end_at", "must be after start_at")
			}
		}
		h := fnv.New64a()
		h.Write([]byte(f.ID))
		if req.Seed != nil {
			r.seed = uint64(*req.Seed)
		} else {
			r.seed = rand.Uint64()
		}
		r.rng = rand.New(rand.NewPCG(r.seed, h.Sum64()))
		rs.rules = append(rs.rules, r)
	}
	return rs, errs
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return itoa(i/10) + string(rune('0'+i%10))
}

func (e *Engine) Install(req fault.RuleSet) yamlx.Errors {
	now := time.Now()
	rs, errs := e.build(req, now)
	if len(errs) > 0 {
		return errs
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if old := e.active.Swap(rs); old != nil {
		close(old.cleared)
	}
	e.last = rs
	e.lastRenew = now
	e.event("installed", rs.runID)
	return nil
}

func (e *Engine) Renew(leaseID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	rs := e.active.Load()
	if rs == nil {
		return ErrNoLease
	}
	if rs.leaseID != leaseID {
		return ErrLeaseMismatch
	}
	e.lastRenew = time.Now()
	return nil
}

// Clear removes every fault. It reports whether a rule set was active.
func (e *Engine) Clear(reason string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.clearLocked(reason)
}

func (e *Engine) clearLocked(reason string) bool {
	old := e.active.Swap(nil)
	if old == nil {
		return false
	}
	close(old.cleared)
	e.event(reason, old.runID)
	return true
}

func (e *Engine) event(typ, runID string) {
	ev := fault.Event{TS: time.Now().UTC(), Type: typ, RunID: runID}
	e.events = append(e.events, ev)
	if len(e.events) > 100 {
		e.events = e.events[len(e.events)-100:]
	}
	if e.onEvent != nil {
		e.onEvent(ev)
	}
}

// Watch clears all faults when the lease is not renewed in time. It returns
// when stop is closed.
func (e *Engine) Watch(stop <-chan struct{}) {
	t := time.NewTicker(min(250*time.Millisecond, e.leaseTimeout/4))
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			e.mu.Lock()
			if e.active.Load() != nil && time.Since(e.lastRenew) >= e.leaseTimeout {
				e.clearLocked("lease_expired")
			}
			e.mu.Unlock()
		}
	}
}

type decision struct {
	rs     *ruleSet
	rule   *rule
	scoped bool
}

// Decide picks the fault for a request: the first rule in experiment order
// that matches, if its strategy fires. A match whose strategy does not fire
// passes through; later rules are not consulted.
func (e *Engine) Decide(route, method, path, scopeValue string, now time.Time) decision {
	if c := e.routes[route]; c != nil {
		c.requests.Add(1)
	}
	rs := e.active.Load()
	if rs == nil {
		return decision{}
	}
	d := decision{rs: rs, scoped: scopeValue != "" && scopeValue == rs.runID}
	if !rs.scopeAll && !d.scoped {
		return d
	}
	for _, r := range rs.rules {
		if r.spec.Route != route || !r.spec.Match.Matches(method, path) {
			continue
		}
		if now.Before(r.startAt) || !now.Before(r.endAt) {
			continue
		}
		if r.decide() {
			d.rule = r
			e.routes[route].faulted.Add(1)
		}
		break
	}
	return d
}

func (e *Engine) Active() fault.Active {
	rs := e.active.Load()
	if rs == nil {
		return fault.Active{Faults: []fault.ActiveFault{}}
	}
	out := fault.Active{RunID: &rs.runID, LeaseID: rs.leaseID, Faults: []fault.ActiveFault{}}
	for _, r := range rs.rules {
		out.Faults = append(out.Faults, fault.ActiveFault{
			ID: r.spec.ID, Type: r.spec.Type, Route: r.spec.Route,
			StartAt: r.startAt.UTC(), EndAt: r.endAt.UTC(), Triggers: r.triggers.Load(),
		})
	}
	return out
}

func (e *Engine) Stats() fault.Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := fault.Stats{
		Active: e.active.Load() != nil,
		Routes: map[string]fault.RouteCounters{},
		Faults: map[string]int64{},
		Events: append([]fault.Event{}, e.events...),
	}
	for name, c := range e.routes {
		s.Routes[name] = fault.RouteCounters{Requests: c.requests.Load(), Faulted: c.faulted.Load()}
	}
	if e.last != nil {
		s.RunID = &e.last.runID
		for _, r := range e.last.rules {
			s.Faults[r.spec.ID] = r.triggers.Load()
		}
	}
	return s
}
