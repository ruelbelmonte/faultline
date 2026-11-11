package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"faultline/internal/config"
	"faultline/internal/experiment"
	"faultline/internal/report"
)

var ErrRunActive = errors.New("a run is already active")

// Control owns the store, the proxy client and at most one active run.
type Control struct {
	cfg   *config.Config
	store *Store
	proxy *ProxyClient
	log   *slog.Logger
	token string

	mu     sync.Mutex
	active *Run
}

func New(cfg *config.Config, token, adminToken string, log *slog.Logger) (*Control, error) {
	store, err := OpenStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	return &Control{
		cfg: cfg, store: store, log: log, token: token,
		proxy: NewProxyClient(cfg.ProxyAdminURL, adminToken),
	}, nil
}

func (c *Control) audit(actor, action, runID string, detail map[string]any) {
	if err := c.store.Audit(actor, action, runID, detail); err != nil {
		c.log.Error("writing audit log failed", "action", action, "error", err.Error())
	}
}

// CreateRun validates an experiment and starts it. Validation problems come
// back as yamlx.Errors.
func (c *Control) CreateRun(rawYAML []byte) (string, error) {
	exp, errs := experiment.Parse(rawYAML, c.cfg)
	if len(errs) > 0 {
		return "", errs
	}
	c.mu.Lock()
	if c.active != nil {
		c.mu.Unlock()
		return "", ErrRunActive
	}
	r := newRun(c, exp, rawYAML)
	c.active = r
	c.mu.Unlock()

	r.persist()
	c.audit(ActorToken, "run_created", r.id, map[string]any{"experiment": exp.Name})
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
func (c *Control) abortActive(reason, actor string) *Run {
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
		c.audit(ActorSystem, "run_aborted", id, map[string]any{"reason": reason})
		c.log.Warn("run interrupted by a control restart marked aborted", "run_id", id)
	}
	return nil
}

// Serve runs the control plane until ctx is cancelled.
func Serve(ctx context.Context, cfg *config.Config, token, adminToken string, log *slog.Logger) error {
	c, err := New(cfg, token, adminToken, log)
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
	srv := &http.Server{Handler: c.Handler(), ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 64 << 10}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("control listening", "addr", ln.Addr().String(), "environment", cfg.Environment, "proxy_admin_url", cfg.ProxyAdminURL)

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
