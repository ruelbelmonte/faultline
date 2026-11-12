package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"faultline/internal/experiment"
	"faultline/internal/fault"
	"faultline/internal/findings"
	"faultline/internal/report"
)

const (
	ReasonUnhealthy       = "unhealthy_before_start"
	ReasonNotTriggered    = "fault_not_triggered"
	ReasonKillSwitch      = "kill_switch"
	ReasonManual          = "manual"
	ReasonErrorRate       = "threshold:error_rate_max"
	ReasonProbeFailures   = "threshold:probe_failures_consecutive"
	ReasonRestarted       = "control_restarted"
	ReasonProxyError      = "proxy_error"
	ReasonLeaseLost       = "lease_lost"
	recoveryConsecutiveOK = 3
	defaultRecoveryErrMax = 0.05
	abortMinRequests      = 10
)

type abortCause struct{ reason, actor string }

func (a abortCause) Error() string { return "aborted: " + a.reason }

type sseMsg struct {
	event string
	data  []byte
}

// Run is one experiment run. All mutable state is guarded by mu.
type Run struct {
	c       *Control
	id      string
	exp     *experiment.Experiment
	yaml    []byte // redacted copy; the only form that is persisted
	leaseID string

	ctx    context.Context
	cancel context.CancelCauseFunc
	done   chan struct{}

	collectMu sync.Mutex

	mu          sync.Mutex
	doc         report.Document
	meta        Meta
	buckets     map[int64]*Bucket
	nextBucket  int64
	samples     []sample
	probeChecks []probeCheck
	consecOK    map[string]int
	consecFail  map[string]int
	recordSalt  []byte
	recordSeq   int64
	injStart    time.Time
	injEnd      time.Time
	subs        map[chan sseMsg]struct{}
	closed      bool
}

func newRun(c *Control, exp *experiment.Experiment, rawYAML []byte) *Run {
	redacted := experiment.Redact(rawYAML, c.cfg.RedactionHeaders)
	sum := sha256.Sum256(redacted)
	now := time.Now()
	r := &Run{
		c: c, id: newRunID(), exp: exp, yaml: redacted, leaseID: newUUID(),
		done:    make(chan struct{}),
		buckets: map[int64]*Bucket{}, nextBucket: now.Unix(),
		consecOK: map[string]int{}, consecFail: map[string]int{},
		subs: map[chan sseMsg]struct{}{}, recordSalt: make([]byte, 16),
	}
	rand.Read(r.recordSalt)
	r.ctx, r.cancel = context.WithCancelCause(context.Background())
	r.doc = report.Document{
		SchemaVersion: report.SchemaVersion,
		RunID:         r.id,
		Experiment:    report.ExperimentInfo{Name: exp.Name, Description: exp.Description, YAMLSHA256: hex.EncodeToString(sum[:])},
		Environment:   c.cfg.Environment,
		Status:        report.StatusPending,
		StartedAt:     report.At(now),
		Hypothesis:    []report.HypothesisEntry{},
		Faults:        []report.FaultResult{},
		Findings:      []report.Finding{},
		Metrics:       report.Metrics{Proxy: map[string]report.RouteMetrics{}},
		Redaction:     report.Redaction{Enabled: len(c.cfg.RedactionHeaders) > 0, Headers: append([]string{}, c.cfg.RedactionHeaders...)},
	}
	for _, f := range exp.Faults {
		r.doc.Faults = append(r.doc.Faults, report.FaultResult{ID: f.ID, Type: f.Type, Route: f.Route})
		r.doc.Metrics.Proxy[f.Route] = report.RouteMetrics{}
	}
	r.meta = Meta{Phases: []report.Phase{}, FaultWindows: []report.FaultWindow{}, Markers: []report.Marker{}}
	return r
}

// snapshotLocked deep-copies what is handed to other goroutines.
func (r *Run) snapshotLocked() (report.Document, Meta) {
	d := r.doc
	d.Hypothesis = append([]report.HypothesisEntry{}, r.doc.Hypothesis...)
	d.Faults = append([]report.FaultResult{}, r.doc.Faults...)
	d.Findings = append([]report.Finding{}, r.doc.Findings...)
	d.Metrics.Proxy = map[string]report.RouteMetrics{}
	for k, v := range r.doc.Metrics.Proxy {
		d.Metrics.Proxy[k] = v
	}
	m := Meta{
		Phases:       append([]report.Phase{}, r.meta.Phases...),
		FaultWindows: append([]report.FaultWindow{}, r.meta.FaultWindows...),
		Markers:      append([]report.Marker{}, r.meta.Markers...),
	}
	return d, m
}

func (r *Run) Snapshot() (report.Document, Meta) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked()
}

// finishing reports whether the run has reached a terminal status.
func (r *Run) finishing() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return report.Terminal(r.doc.Status)
}

func (r *Run) persist() {
	doc, meta := r.Snapshot()
	if err := r.c.store.SaveRun(&doc, &meta, r.yaml); err != nil {
		r.c.log.Error("saving run failed", "run_id", r.id, "error", err.Error())
	}
}

type statusEvent struct {
	RunID   string  `json:"run_id"`
	Status  string  `json:"status"`
	Verdict *string `json:"verdict"`
	Reason  *string `json:"reason"`
}

func (r *Run) publish(event string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for ch := range r.subs {
		select {
		case ch <- sseMsg{event, data}:
		default:
			// A subscriber that cannot keep up misses events; it can refetch.
		}
	}
}

// subscribe returns a channel of events, or nil when the run has ended.
func (r *Run) subscribe() (chan sseMsg, statusEvent, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := statusEvent{r.id, r.doc.Status, r.doc.Verdict, r.doc.Reason}
	if r.closed {
		return nil, st, func() {}
	}
	ch := make(chan sseMsg, 512)
	r.subs[ch] = struct{}{}
	return ch, st, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if _, ok := r.subs[ch]; ok {
			delete(r.subs, ch)
			close(ch)
		}
	}
}

// setStatus moves to a non-terminal status and opens its phase.
func (r *Run) setStatus(status string) {
	now := report.At(time.Now())
	r.mu.Lock()
	if n := len(r.meta.Phases); n > 0 && r.meta.Phases[n-1].End == nil {
		r.meta.Phases[n-1].End = &now
	}
	r.meta.Phases = append(r.meta.Phases, report.Phase{Name: status, Start: now})
	r.doc.Status = status
	r.mu.Unlock()
	r.persist()
	r.publish("status", statusEvent{r.id, status, nil, nil})
	r.c.log.Info("run status", "run_id", r.id, "status", status)
}

func (r *Run) abort(reason, actor string) {
	r.cancel(abortCause{reason, actor})
}

func (r *Run) abortCause() *abortCause {
	var a abortCause
	if errors.As(context.Cause(r.ctx), &a) {
		return &a
	}
	return nil
}

func (r *Run) bucketLocked(sec int64) *Bucket {
	b := r.buckets[sec]
	if b == nil {
		b = &Bucket{T: report.At(time.Unix(sec, 0)), Routes: map[string]report.RouteMetrics{}}
		r.buckets[sec] = b
	}
	return b
}

func (r *Run) addSample(s sample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.samples = append(r.samples, s)
	b := r.bucketLocked(s.end.Unix())
	b.Driver.Requests++
	if s.failed() {
		b.Driver.Errors++
	}
	b.latencies = append(b.latencies, s.ms)
	if !r.injStart.IsZero() && r.injEnd.IsZero() {
		d := &r.doc.Metrics.Driver
		d.Requests++
		if s.failed() {
			d.Errors++
		}
		d.ErrorRate = round(float64(d.Errors)/float64(d.Requests), 4)
	}
}

func (r *Run) addProbeCheck(name string, ok bool) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.probeChecks = append(r.probeChecks, probeCheck{now, ok})
	b := r.bucketLocked(now.Unix())
	if ok {
		b.Probes.OK++
		r.consecOK[name]++
		r.consecFail[name] = 0
	} else {
		b.Probes.Fail++
		r.consecFail[name]++
		r.consecOK[name] = 0
	}
}

func (r *Run) probesHealthy(n int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.exp.Probes {
		if r.consecOK[p.Name] < n {
			return false
		}
	}
	return true
}

// errorRate is the driver error rate over requests that finished after
// `since`, and how many there were.
func (r *Run) errorRateLocked(since time.Time) (float64, int) {
	n, errs := 0, 0
	for i := len(r.samples) - 1; i >= 0; i-- {
		s := r.samples[i]
		if s.end.Before(since) {
			// Samples are appended in completion order.
			break
		}
		n++
		if s.failed() {
			errs++
		}
	}
	if n == 0 {
		return 0, 0
	}
	return float64(errs) / float64(n), n
}

// emitBuckets finalises, stores and publishes every bucket before `until`.
func (r *Run) emitBuckets(until int64) {
	r.mu.Lock()
	var ready []*Bucket
	for ; r.nextBucket < until; r.nextBucket++ {
		b := r.bucketLocked(r.nextBucket)
		sorted := sortedCopy(b.latencies)
		b.Driver.P50Ms, b.Driver.P95Ms = round(percentile(sorted, 0.50), 2), round(percentile(sorted, 0.95), 2)
		cp := *b
		cp.Routes = map[string]report.RouteMetrics{}
		for k, v := range b.Routes {
			cp.Routes[k] = v
		}
		ready = append(ready, &cp)
	}
	r.mu.Unlock()
	for _, b := range ready {
		if err := r.c.store.SaveBucket(r.id, b); err != nil {
			r.c.log.Error("saving metrics failed", "run_id", r.id, "error", err.Error())
		}
		r.publish("metrics", b.Point())
	}
}

// collect pulls new request records and trigger counts from the proxy.
func (r *Run) collect() {
	r.collectMu.Lock()
	defer r.collectMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	for {
		r.mu.Lock()
		since := r.recordSeq
		r.mu.Unlock()
		page, err := r.c.proxy.Records(ctx, r.id, since)
		if err != nil {
			r.c.log.Warn("reading proxy records failed", "run_id", r.id, "error", err.Error())
			return
		}
		var own []fault.Record
		r.mu.Lock()
		r.recordSeq = page.Next
		injectionEnd := r.injStart.Add(r.exp.Duration)
		for _, rec := range page.Records {
			if !rec.Scoped && r.exp.Scope != experiment.ScopeAll {
				// Someone else's traffic on a shared route is not this run's.
				continue
			}
			own = append(own, rec)
			if !rec.TS.Before(injectionEnd) {
				// Arrived after the injection phase, in the moment before the
				// faults were cleared: evidence for retries, not a metric.
				continue
			}
			done := rec.TS.Add(time.Duration(rec.DurationMs * float64(time.Millisecond)))
			b := r.bucketLocked(max(done.Unix(), r.nextBucket))
			rm, total := b.Routes[rec.Route], r.doc.Metrics.Proxy[rec.Route]
			rm.Requests++
			total.Requests++
			if rec.FaultID != nil {
				rm.Faulted++
				total.Faulted++
			}
			b.Routes[rec.Route], r.doc.Metrics.Proxy[rec.Route] = rm, total
		}
		r.mu.Unlock()
		if err := r.c.store.SaveRecords(r.id, r.recordSalt, own); err != nil {
			r.c.log.Error("storing request records failed; findings will lack them", "run_id", r.id, "error", err.Error())
		}
		if len(page.Records) < fault.MaxRecordsPage {
			break
		}
	}
	stats, err := r.c.proxy.Stats(ctx)
	if err != nil || stats.RunID == nil || *stats.RunID != r.id {
		return
	}
	r.mu.Lock()
	for i := range r.doc.Faults {
		r.doc.Faults[i].Triggers = stats.Faults[r.doc.Faults[i].ID]
	}
	r.mu.Unlock()
}

// clearProxy removes our faults. It retries briefly; if the proxy stays
// unreachable the lease and each fault's end_at still bound their lifetime.
func (r *Run) clearProxy() {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err = r.c.proxy.Clear(ctx)
		cancel()
		if err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	r.c.log.Error("clearing proxy faults failed; relying on lease expiry", "run_id", r.id, "error", err.Error())
}

// run drives the whole lifecycle. It is the only goroutine that changes status.
func (r *Run) run() {
	defer close(r.done)
	defer r.c.finished(r)

	bg, stopBG := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-bg.Done():
				return
			case now := <-tick.C:
				r.mu.Lock()
				injecting := r.doc.Status == report.StatusInjecting
				r.mu.Unlock()
				if injecting {
					r.collect()
				}
				r.emitBuckets(now.Unix())
			}
		}
	}()

	precheckFailed := r.execute(bg, &workers)
	stopBG()
	workers.Wait()
	r.finalize(precheckFailed)
}

// execute runs precheck, injection and recovery. It returns early when the
// run is aborted; finalize then reads the cause from the context.
func (r *Run) execute(bg context.Context, workers *sync.WaitGroup) (precheckFailed bool) {
	if len(r.exp.Probes) > 0 {
		for _, p := range r.exp.Probes {
			workers.Add(1)
			go func() {
				defer workers.Done()
				r.probe(bg, p)
			}()
		}
		r.setStatus(report.StatusPrechecking)
		healthy := r.precheck()
		if r.ctx.Err() != nil {
			return false
		}
		if !healthy {
			return true
		}
	}
	if r.ctx.Err() != nil {
		return false
	}

	t0 := time.Now()
	rs := fault.RuleSet{RunID: r.id, ScopeMode: r.exp.Scope, LeaseID: r.leaseID, Seed: r.exp.Seed}
	r.mu.Lock()
	for i, f := range r.exp.Faults {
		start := t0.Add(f.Start)
		end := start.Add(f.Duration)
		rs.Faults = append(rs.Faults, fault.Install{Spec: f.Spec, StartAt: &start, EndAt: &end})
		r.doc.Faults[i].StartAt, r.doc.Faults[i].EndAt = report.AtPtr(start), report.AtPtr(end)
		r.meta.FaultWindows = append(r.meta.FaultWindows, report.FaultWindow{FaultID: f.ID, Start: report.At(start), End: report.At(end)})
	}
	r.mu.Unlock()

	installCtx, cancelInstall := context.WithTimeout(r.ctx, 5*time.Second)
	err := r.c.proxy.Install(installCtx, rs)
	cancelInstall()
	if err != nil {
		r.c.log.Error("installing faults failed", "run_id", r.id, "error", err.Error())
		r.abort(ReasonProxyError, ActorSystem)
		return false
	}

	leaseCtx, stopLease := context.WithCancel(context.Background())
	leaseDone := make(chan struct{})
	go r.renewLease(leaseCtx, leaseDone)
	endLease := sync.OnceFunc(func() {
		stopLease()
		<-leaseDone
	})
	defer endLease()

	r.mu.Lock()
	r.injStart = t0
	r.mu.Unlock()
	r.setStatus(report.StatusInjecting)

	workers.Add(1)
	go func() {
		defer workers.Done()
		r.drive(bg)
	}()

	end := time.NewTimer(time.Until(t0.Add(r.exp.Duration)))
	defer end.Stop()
	check := time.NewTicker(time.Second)
	defer check.Stop()
injection:
	for {
		select {
		case <-r.ctx.Done():
			return false
		case <-end.C:
			break injection
		case <-check.C:
			if reason := r.thresholdBreached(); reason != "" {
				r.abort(reason, ActorSystem)
				return false
			}
		}
	}

	endLease()
	now := time.Now()
	r.mu.Lock()
	r.injEnd = now
	for name := range r.consecOK {
		// Recovery counts consecutive successes from the end of injection.
		r.consecOK[name] = 0
	}
	r.mu.Unlock()
	r.clearProxy()
	r.collect()
	r.setStatus(report.StatusRecovering)
	r.awaitRecovery(now)
	return false
}

func (r *Run) precheck() bool {
	deadline := time.NewTimer(r.exp.PrecheckTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-tick.C:
			if r.probesHealthy(r.exp.PrecheckConsecutiveOK) {
				return true
			}
		}
	}
}

func (r *Run) renewLease(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	tick := time.NewTicker(r.c.cfg.LeaseTimeout / 3)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			callCtx, cancel := context.WithTimeout(ctx, r.c.cfg.LeaseTimeout/3)
			err := r.c.proxy.Renew(callCtx, r.leaseID)
			cancel()
			switch {
			case err == nil || ctx.Err() != nil:
			case errors.Is(err, ErrLeaseLost):
				// The proxy dropped our faults; the run no longer measures anything.
				r.abort(ReasonLeaseLost, ActorSystem)
				return
			default:
				r.c.log.Warn("lease renewal failed", "run_id", r.id, "error", err.Error())
			}
		}
	}
}

func (r *Run) thresholdBreached() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit := r.exp.Abort.ErrorRateMax; limit != nil {
		if rate, n := r.errorRateLocked(time.Now().Add(-rollingWindow)); n >= abortMinRequests && rate > *limit {
			return ReasonErrorRate
		}
	}
	if n := r.exp.Abort.ProbeFailuresConsecutive; n > 0 {
		for _, c := range r.consecFail {
			if c >= n {
				return ReasonProbeFailures
			}
		}
	}
	return ""
}

func (r *Run) awaitRecovery(injEnd time.Time) {
	deadline := time.NewTimer(r.exp.RecoveryTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if r.recovered(injEnd) {
			secs := round(time.Since(injEnd).Seconds(), 1)
			r.mu.Lock()
			r.doc.RecoverySeconds = &secs
			r.mu.Unlock()
			return
		}
		select {
		case <-r.ctx.Done():
			return
		case <-deadline.C:
			return
		case <-tick.C:
		}
	}
}

func (r *Run) recovered(injEnd time.Time) bool {
	if !r.probesHealthy(recoveryConsecutiveOK) {
		return false
	}
	if r.exp.Traffic == nil || r.exp.Traffic.Command != "" {
		return true
	}
	limit := defaultRecoveryErrMax
	if r.exp.Hypothesis.ErrorRateMax != nil {
		limit = *r.exp.Hypothesis.ErrorRateMax
	}
	since := time.Now().Add(-rollingWindow)
	if since.Before(injEnd) {
		// Errors from the injection phase say nothing about recovery.
		since = injEnd
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n, errs := 0, 0
	for i := len(r.samples) - 1; i >= 0 && !r.samples[i].end.Before(since); i-- {
		if r.samples[i].start.Before(injEnd) {
			continue
		}
		n++
		if r.samples[i].failed() {
			errs++
		}
	}
	return n > 0 && float64(errs)/float64(n) <= limit
}

func (r *Run) finalize(precheckFailed bool) {
	cause := r.abortCause()
	if cause != nil {
		// Faults are cleared before the aborted status is persisted, and the
		// audit entry exists before anyone can observe that status.
		r.clearProxy()
		r.c.audit(cause.actor, "run_aborted", r.id, map[string]any{"reason": cause.reason})
	}
	r.mu.Lock()
	injected := !r.injStart.IsZero()
	if injected && r.injEnd.IsZero() {
		r.injEnd = time.Now()
	}
	r.mu.Unlock()
	if injected {
		r.collect()
	}
	now := time.Now()
	r.emitBuckets(now.Unix() + 1)

	r.mu.Lock()
	if injected {
		r.summariseLocked()
	}
	verdict, reason := r.verdictLocked(cause, precheckFailed)
	in := r.findingsInputLocked(injected && cause == nil)
	r.mu.Unlock()

	// Reads the stored records; not under mu, so the API stays responsive.
	found, err := findings.EvaluateFrom(in, r.c.store.Evidence(r.id))
	if err != nil {
		r.c.log.Error("reading request records failed; findings are incomplete", "run_id", r.id, "error", err.Error())
	}
	if err := r.c.store.DeleteRecords(r.id); err != nil {
		r.c.log.Error("deleting request records failed", "run_id", r.id, "error", err.Error())
	}

	r.mu.Lock()
	r.doc.Findings = append([]report.Finding{}, found...)
	r.doc.Verdict = &verdict
	if reason != "" {
		r.doc.Reason = &reason
	}
	end := report.At(now)
	r.doc.EndedAt = &end
	if n := len(r.meta.Phases); n > 0 && r.meta.Phases[n-1].End == nil {
		r.meta.Phases[n-1].End = &end
	}
	r.doc.Status = report.StatusCompleted
	if cause != nil {
		r.doc.Status = report.StatusAborted
		r.meta.Markers = append(r.meta.Markers, report.Marker{Type: "abort", T: end, Detail: cause.reason})
	}
	final := statusEvent{r.id, r.doc.Status, r.doc.Verdict, r.doc.Reason}
	r.mu.Unlock()

	r.persist()
	r.c.writeReports(r.id)
	for _, f := range found {
		r.publish("finding", f)
	}
	r.c.log.Info("run finished", "run_id", r.id, "status", final.Status, "verdict", verdict, "reason", reason)
	r.publish("status", final)

	r.mu.Lock()
	r.closed = true
	for ch := range r.subs {
		delete(r.subs, ch)
		close(ch)
	}
	r.mu.Unlock()
}

// findingsInputLocked gathers the evidence the finding rules work from.
func (r *Run) findingsInputLocked(completed bool) findings.Input {
	in := findings.Input{
		Hypothesis:                  r.doc.Hypothesis,
		RetryAmplificationThreshold: r.c.cfg.RetryAmplificationThreshold,
		InjectionCompleted:          completed,
		RecoveryMeasured:            completed,
		RecoverySeconds:             r.doc.RecoverySeconds,
		RecoveryTimeoutS:            r.exp.RecoveryTimeout.Seconds(),
		ClearedAt:                   r.injEnd,
	}
	for i, f := range r.exp.Faults {
		in.Faults = append(in.Faults, findings.Fault{ID: f.ID, Route: f.Route, Match: f.Match, Triggers: r.doc.Faults[i].Triggers})
		if f.Type == fault.Timeout {
			in.TimeoutFaults = append(in.TimeoutFaults, findings.TimeoutFault{ID: f.ID, Route: f.Route, HoldMs: float64(f.Typed.Hold.Milliseconds())})
		}
	}
	return in
}

// summariseLocked computes injection-phase metrics and hypothesis entries.
func (r *Run) summariseLocked() {
	var lat []float64
	d := report.DriverMetrics{}
	for _, s := range r.samples {
		if s.start.Before(r.injStart) || !s.start.Before(r.injEnd) {
			continue
		}
		d.Requests++
		if s.failed() {
			d.Errors++
		}
		lat = append(lat, s.ms)
	}
	sorted := sortedCopy(lat)
	if d.Requests > 0 {
		d.ErrorRate = round(float64(d.Errors)/float64(d.Requests), 4)
		d.P50Ms, d.P95Ms = round(percentile(sorted, 0.50), 2), round(percentile(sorted, 0.95), 2)
	}
	r.doc.Metrics.Driver = d

	probeOK, probeTotal := 0, 0
	for _, c := range r.probeChecks {
		if c.at.Before(r.injStart) || !c.at.Before(r.injEnd) {
			continue
		}
		probeTotal++
		if c.ok {
			probeOK++
		}
	}

	h := r.exp.Hypothesis
	r.doc.Hypothesis = []report.HypothesisEntry{}
	add := func(name string, threshold float64, observed *float64, pass func(float64) bool) {
		e := report.HypothesisEntry{Name: name, Threshold: threshold, Observed: observed}
		e.Passed = observed != nil && pass(*observed)
		r.doc.Hypothesis = append(r.doc.Hypothesis, e)
	}
	if h.ErrorRateMax != nil {
		var obs *float64
		if d.Requests > 0 {
			obs = &d.ErrorRate
		}
		add("error_rate_max", *h.ErrorRateMax, obs, func(v float64) bool { return v <= *h.ErrorRateMax })
	}
	if h.P95LatencyMsMax != nil {
		var obs *float64
		if d.Requests > 0 {
			obs = &d.P95Ms
		}
		add("p95_latency_ms_max", *h.P95LatencyMsMax, obs, func(v float64) bool { return v <= *h.P95LatencyMsMax })
	}
	if h.ProbeSuccessMin != nil {
		var obs *float64
		if probeTotal > 0 {
			v := round(float64(probeOK)/float64(probeTotal), 4)
			obs = &v
		}
		add("probe_success_min", *h.ProbeSuccessMin, obs, func(v float64) bool { return v >= *h.ProbeSuccessMin })
	}
}

// verdictLocked applies the rules of PRD 9.4 in order.
func (r *Run) verdictLocked(cause *abortCause, precheckFailed bool) (verdict, reason string) {
	if cause != nil {
		return report.VerdictAborted, cause.reason
	}
	if precheckFailed {
		return report.VerdictInconclusive, ReasonUnhealthy
	}
	for _, f := range r.doc.Faults {
		if f.Triggers == 0 {
			return report.VerdictInconclusive, ReasonNotTriggered
		}
	}
	for _, h := range r.doc.Hypothesis {
		if !h.Passed {
			return report.VerdictFailed, ""
		}
	}
	return report.VerdictPassed, ""
}
