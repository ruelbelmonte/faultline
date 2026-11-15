package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"sync"
	"time"

	"faultline/internal/config"
	"faultline/internal/experiment"
	"faultline/internal/report"
)

var ErrRunActive = errors.New("a run is already active")

// BaselineError rejects a run whose comparison baseline cannot be used.
type BaselineError struct{ Message string }

func (e *BaselineError) Error() string { return e.Message }

var runIDRE = regexp.MustCompile(`^run_[a-z2-7]{12}$`)

// checkBaseline normalises the baseline a run asked for. An explicit run id
// must name a finished run of the same experiment.
func (c *Control) checkBaseline(baseline, experimentName string) (string, error) {
	switch baseline {
	case "", report.BaselinePrevious:
		return report.BaselinePrevious, nil
	case report.BaselineLastPassed:
		return baseline, nil
	}
	if !runIDRE.MatchString(baseline) {
		return "", &BaselineError{"baseline must be previous, last-passed or a run id"}
	}
	doc, _, err := c.store.Run(baseline)
	if errors.Is(err, ErrNotFound) {
		return "", &BaselineError{"baseline run " + baseline + " does not exist"}
	}
	if err != nil {
		return "", err
	}
	if !report.Terminal(doc.Status) {
		return "", &BaselineError{"baseline run " + baseline + " has not finished"}
	}
	if doc.Experiment.Name != experimentName {
		return "", &BaselineError{"baseline run " + baseline + " is of experiment " + doc.Experiment.Name + ", not " + experimentName}
	}
	return baseline, nil
}

// Control owns the store, the proxy client and at most one active run.
type Control struct {
	cfg    *config.Config
	store  *Store
	proxy  *ProxyClient
	log    *slog.Logger
	tokens []config.NamedToken

	mu     sync.Mutex
	active *Run
}

func New(cfg *config.Config, tokens []config.NamedToken, adminToken string, log *slog.Logger) (*Control, error) {
	store, err := OpenStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	return &Control{
		cfg: cfg, store: store, log: log, tokens: tokens,
		proxy: NewProxyClient(cfg.ProxyAdminURL, adminToken),
	}, nil
}

func (c *Control) audit(actor Actor, action, runID string, detail map[string]any) {
	if err := c.store.Audit(actor, action, runID, detail); err != nil {
		c.log.Error("writing audit log failed", "action", action, "error", err.Error())
	}
}

// CreateRun validates an experiment and starts it. Validation problems come
// back as yamlx.Errors. baseline selects what the report is compared with when
// the run finishes; "" means report.BaselinePrevious.
func (c *Control) CreateRun(rawYAML []byte, actor Actor, baseline string) (string, error) {
	exp, errs := experiment.Parse(rawYAML, c.cfg)
	if len(errs) > 0 {
		return "", errs
	}
	baseline, err := c.checkBaseline(baseline, exp.Name)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	for c.active != nil {
		prev := c.active
		c.mu.Unlock()
		if !prev.finishing() {
			return "", ErrRunActive
		}
		// The previous run already shows a terminal status and is writing
		// its reports; a client that saw that status may start the next run.
		select {
		case <-prev.done:
		case <-time.After(15 * time.Second):
			return "", ErrRunActive
		}
		c.mu.Lock()
	}
	r := newRun(c, exp, rawYAML)
	r.baseline = baseline
	c.active = r
	c.mu.Unlock()

	r.persist()
	c.audit(actor, "run_created", r.id, map[string]any{"experiment": exp.Name})
	c.log.Info("run created", "run_id", r.id, "experiment", exp.Name)
	go r.run()
	return r.id, nil
}

func (c *Control) finished(r *Run) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == r {
		c.active = nil
	}
}

func (c *Control) activeRun() *Run {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}

// abortActive aborts the active run, if any, and waits for it to finish.
func (c *Control) abortActive(reason string, actor Actor) *Run {
	r := c.activeRun()
	if r == nil {
		return nil
	}
	r.abort(reason, actor)
	select {
	case <-r.done:
	case <-time.After(20 * time.Second):
		c.log.Error("run did not stop after abort", "run_id", r.id)
	}
	return r
}

// document returns a run from memory when it is active, else from the store.
func (c *Control) document(id string) (*report.Document, *Meta, error) {
	if r := c.activeRun(); r != nil && r.id == id {
		doc, meta := r.Snapshot()
		return &doc, &meta, nil
	}
	return c.store.Run(id)
}

func (c *Control) timeseries(id string) (*report.Timeseries, error) {
	_, meta, err := c.document(id)
	if err != nil {
		return nil, err
	}
	buckets, err := c.store.Buckets(id)
	if err != nil {
		return nil, err
	}
	ts := &report.Timeseries{BucketSeconds: 1, Series: []report.Point{}, Phases: meta.Phases, FaultWindows: meta.FaultWindows, Markers: meta.Markers}
	for _, b := range buckets {
		ts.Series = append(ts.Series, b.Point())
	}
	return ts, nil
}

// recoverUnfinished handles runs a previous control process left behind:
// faults are cleared on the proxy, then the run is marked aborted.
func (c *Control) recoverUnfinished() error {
	ids, err := c.store.Unfinished()
	if err != nil || len(ids) == 0 {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.proxy.Clear(ctx); err != nil {
		c.log.Warn("could not clear proxy faults after restart; the lease bounds their lifetime", "error", err.Error())
	}
	for _, id := range ids {
		doc, meta, err := c.store.Run(id)
		if err != nil {
			return err
		}
		now := report.At(time.Now())
		verdict, reason := report.VerdictAborted, ReasonRestarted
		doc.Status, doc.Verdict, doc.Reason, doc.EndedAt = report.StatusAborted, &verdict, &reason, &now
		if n := len(meta.Phases); n > 0 && meta.Phases[n-1].End == nil {
			meta.Phases[n-1].End = &now
		}
		meta.Markers = append(meta.Markers, report.Marker{Type: "abort", T: now, Detail: reason})
		if err := c.store.SaveRun(doc, meta, nil); err != nil {
			return err
		}
		c.writeReports(id)
		c.audit(ActorSystem, "run_aborted", id, map[string]any{"reason": reason})
		c.log.Warn("run interrupted by a control restart marked aborted", "run_id", id)
	}
	return nil
}

// Serve runs the control plane until ctx is cancelled.
func Serve(ctx context.Context, cfg *config.Config, tokens []config.NamedToken, adminToken string, log *slog.Logger) error {
	c, err := New(cfg, tokens, adminToken, log)
	if err != nil {
		return err
	}
	defer c.store.Close()
	if err := c.recoverUnfinished(); err != nil {
		return fmt.Errorf("recovering unfinished runs: %w", err)
	}
	ln, err := net.Listen("tcp", cfg.ControlListen)
	if err != nil {
		return fmt.Errorf("control listen: %w", err)
	}
	srv := newServer(c.Handler())
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("control listening", "addr", ln.Addr().String(), "environment", cfg.Environment, "proxy_admin_url", cfg.ProxyAdminURL)
	if w := HostWarning(cfg); w != "" {
		log.Warn(w)
	}

	select {
	case <-ctx.Done():
		err = nil
	case err = <-errc:
	}
	// A run cannot outlive its control plane: clear its faults and close it.
	c.abortActive(ReasonRestarted, ActorSystem)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if srv.Shutdown(shutdownCtx) != nil {
		srv.Close()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Request bodies here are small and capped, so a slow sender is cut off. SSE
// responses are unaffected: ReadTimeout ends when the request has been read.
var serverTimeouts = struct{ read, idle time.Duration }{30 * time.Second, 120 * time.Second}

func newServer(h http.Handler) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: serverTimeouts.read, IdleTimeout: serverTimeouts.idle, MaxHeaderBytes: 64 << 10}
}

// HostWarning is the startup warning for a shared environment whose probe
// and traffic hosts are not restricted, or "" when there is nothing to say.
func HostWarning(cfg *config.Config) string {
	if cfg.Environment == "qa" && len(cfg.AllowedHosts) == 0 {
		return "control.allowed_hosts is empty: experiments may send probe and traffic requests to any host the control plane can reach. Set it to the hosts of the systems under test"
	}
	return ""
}
