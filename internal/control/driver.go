package control

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"faultline/internal/experiment"
)

// drive runs the experiment's traffic until ctx is cancelled.
func (r *Run) drive(ctx context.Context) {
	t := r.exp.Traffic
	switch {
	case t == nil:
	case t.Command != "":
		r.driveCommand(ctx, t.Command)
	default:
		r.driveRequests(ctx, t)
	}
}

func (r *Run) driveRequests(ctx context.Context, t *experiment.Traffic) {
	client := &http.Client{
		Timeout:   t.Timeout,
		Transport: &http.Transport{MaxIdleConnsPerHost: t.Concurrency, DisableCompression: true},
		// The status of the system under test is what counts, not where it points.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()

	jobs := make(chan int, t.Concurrency)
	var wg sync.WaitGroup
	for i := 0; i < t.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range jobs {
				r.sendOne(ctx, client, t.BaseURL, t.Requests[n%len(t.Requests)])
			}
		}()
	}

	// Pace by how many requests are due, so timer granularity does not skew the rate.
	begin := time.Now()
	tick := time.NewTicker(min(5*time.Millisecond, time.Duration(float64(time.Second)/t.RPS)))
	defer tick.Stop()
	sent := 0
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-tick.C:
			due := int(time.Since(begin).Seconds() * t.RPS)
			for sent < due {
				select {
				case jobs <- sent:
					sent++
				default:
					// Every worker is busy: the backlog is dropped, not queued.
					sent = due
				}
			}
		}
	}
	close(jobs)
	wg.Wait()
}

func (r *Run) sendOne(ctx context.Context, client *http.Client, base string, spec experiment.Request) {
	var body io.Reader
	if spec.Body != "" {
		body = strings.NewReader(spec.Body)
	}
	req, err := http.NewRequestWithContext(ctx, spec.Method, base+spec.Path, body)
	if err != nil {
		return
	}
	for k, v := range spec.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set(r.c.cfg.ScopeHeader, r.id)
	req.Header.Set(r.c.cfg.CorrelationHeader, newUUID())

	s := sample{start: time.Now()}
	resp, err := client.Do(req)
	if err == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err == nil {
			s.status = resp.StatusCode
		}
	}
	if ctx.Err() != nil {
		// Cut off by the end of the run, not by the system under test.
		return
	}
	s.end = time.Now()
	s.ms = float64(s.end.Sub(s.start).Microseconds()) / 1000
	r.addSample(s)
}

// driveCommand re-runs a shell command until ctx is cancelled. Its output is
// discarded: it may contain anything, and nothing of it is stored.
func (r *Run) driveCommand(ctx context.Context, command string) {
	for ctx.Err() == nil {
		cmd := shellCommand(ctx, command)
		cmd.WaitDelay = 2 * time.Second
		cmd.Env = append(os.Environ(),
			"FAULTLINE_RUN_ID="+r.id,
			"FAULTLINE_SCOPE_HEADER="+r.c.cfg.ScopeHeader,
		)
		if err := cmd.Run(); err != nil && ctx.Err() == nil {
			r.c.log.Warn("traffic command failed", "run_id", r.id, "error", err.Error())
		}
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// probe checks one probe on its interval until ctx is cancelled.
func (r *Run) probe(ctx context.Context, p experiment.Probe) {
	client := &http.Client{
		Timeout:       p.Timeout,
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	tick := time.NewTicker(p.Interval)
	defer tick.Stop()
	for {
		ok := false
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
		if err == nil {
			req.Header.Set(r.c.cfg.ScopeHeader, r.id)
			req.Header.Set(r.c.cfg.CorrelationHeader, newUUID())
			if resp, err := client.Do(req); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				ok = resp.StatusCode == p.ExpectStatus
			}
		}
		if ctx.Err() != nil {
			return
		}
		r.addProbeCheck(p.Name, ok)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
