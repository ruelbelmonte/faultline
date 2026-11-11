package proxy

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"faultline/internal/config"
	"faultline/internal/fault"
)

const (
	headerFault    = "X-Faultline-Fault"
	headerFaultlne = "X-Faultline"
)

type route struct {
	name     string
	upstream *url.URL
}

// Proxy is the traffic-facing handler.
type Proxy struct {
	cfg       *config.Config
	routes    map[string]*route
	engine    *Engine
	records   *ring
	transport http.RoundTripper
	tlsConfig *tls.Config
	log       *slog.Logger

	lossWarned atomic.Pointer[string]
}

func New(cfg *config.Config, log *slog.Logger) (*Proxy, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	// HTTP/1.1 only: HTTP/2 lower-cases header names on the wire.
	tlsDialer := &tls.Dialer{NetDialer: dialer, Config: &tls.Config{NextProtos: []string{"http/1.1"}}}
	wrap := func(c net.Conn, err error) (net.Conn, error) {
		if err != nil {
			return nil, err
		}
		return &outConn{Conn: c}, nil
	}
	p := &Proxy{
		cfg:       cfg,
		routes:    map[string]*route{},
		records:   newRing(DefaultRingSize),
		tlsConfig: tlsDialer.Config,
		log:       log,
		transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			// Bytes must reach the client exactly as the upstream sent them.
			DisableCompression:  true,
			MaxIdleConns:        512,
			MaxIdleConnsPerHost: 128,
			IdleConnTimeout:     90 * time.Second,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return wrap(dialer.DialContext(ctx, network, addr))
			},
			DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return wrap(tlsDialer.DialContext(ctx, network, addr))
			},
		},
	}
	var names []string
	for _, r := range cfg.Routes {
		u, err := url.Parse(r.Upstream)
		if err != nil {
			return nil, err
		}
		p.routes[r.Name] = &route{name: r.Name, upstream: u}
		names = append(names, r.Name)
	}
	p.engine = NewEngine(names, cfg.LeaseTimeout, func(ev fault.Event) {
		if ev.Type == "lease_expired" {
			log.Warn("lease expired: all faults cleared", "run_id", ev.RunID)
			return
		}
		log.Info("faults "+ev.Type, "run_id", ev.RunID)
	})
	return p, nil
}

// outcome is what the client got, for the request record.
type outcome struct {
	status       int
	retryAfter   *int
	clientHeldMs *float64
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	begin := time.Now()
	if c, ok := r.Context().Value(connKey{}).(*inConn); ok {
		// Every request on the connection must pass here, routed or not.
		if lines := c.requestHead(r); lines != nil {
			r = r.WithContext(context.WithValue(r.Context(), headKey{}, lines))
		}
	}
	name, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	rt := p.routes[name]
	if rt == nil {
		w.Header().Set(headerFaultlne, "unknown-route")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":"unknown_route"}`)
		return
	}
	path := "/" + rest
	if decoded, err := url.PathUnescape(path); err == nil {
		path = decoded
	}

	d := p.engine.Decide(name, r.Method, path, r.Header.Get(p.cfg.ScopeHeader), begin)
	var out outcome
	aborted := false
	func() {
		// An aborted response must still be recorded before the abort propagates.
		defer func() {
			if v := recover(); v != nil {
				if v != http.ErrAbortHandler {
					panic(v)
				}
				aborted = true
			}
		}()
		if d.rule == nil {
			out = p.forward(w, r, rt, rest, forwardOpts{})
		} else {
			out = p.inject(w, r, rt, rest, d)
		}
	}()
	defer func() {
		if aborted {
			panic(http.ErrAbortHandler)
		}
	}()
	if d.rs == nil {
		return
	}
	rec := fault.Record{
		TS:           begin.UTC(),
		Route:        name,
		Method:       r.Method,
		Path:         path,
		Status:       out.status,
		DurationMs:   ms(time.Since(begin)),
		Scoped:       d.scoped,
		Correlation:  r.Header.Get(p.cfg.CorrelationHeader),
		RetryAfter:   out.retryAfter,
		ClientHeldMs: out.clientHeldMs,
	}
	if d.rule != nil {
		rec.FaultID, rec.FaultType = &d.rule.spec.ID, &d.rule.spec.Type
	}
	if p.records.add(d.rs.runID, rec) {
		p.warnRecordsLost(d.rs.runID)
	}
}

// warnRecordsLost logs, once per run, that the record ring wrapped before the
// control plane read it.
func (p *Proxy) warnRecordsLost(runID string) {
	if prev := p.lossWarned.Swap(&runID); prev == nil || *prev != runID {
		p.log.Warn("request records were overwritten before they were read; findings for this run will be based on incomplete records",
			"run_id", runID, "ring_size", p.records.size)
	}
}

func ms(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

var hopByHop = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

type (
	connKey struct{}
	headKey struct{}
)

func copyHeaders(dst, src http.Header) {
	drop := hopSet(src.Values("Connection"))
	for k, vs := range src {
		if drop[k] {
			continue
		}
		dst[k] = append([]string(nil), vs...)
	}
}

type forwardOpts struct {
	rule *rule
	rs   *ruleSet
	// cutAfter >= 0 forwards exactly that many body bytes, then closes abruptly.
	cutAfter       int64
	hasCut         bool
	bytesPerSec    int
	garble         bool
	firstByteDelay time.Duration
	// rewrite replaces the plain relay of the upstream response.
	rewrite func(w http.ResponseWriter, r *http.Request, resp *http.Response, o forwardOpts) outcome
}

func (p *Proxy) setFaultHeader(h http.Header, r *rule) {
	if r != nil && p.cfg.ExposeFaultHeader {
		h.Set(headerFault, r.spec.ID)
	}
}

// upstreamRequest builds the request for the upstream. readable asks the
// upstream for an uncompressed body, for faults that rewrite it.
func (p *Proxy) upstreamRequest(r *http.Request, rt *route, rest string, readable bool) (*http.Request, error) {
	target := *rt.upstream
	target.Path, target.RawPath = "", ""
	base := strings.TrimRight(rt.upstream.EscapedPath(), "/")
	u, err := url.Parse(target.String() + base + "/" + rest)
	if err != nil {
		return nil, err
	}
	u.RawQuery = r.URL.RawQuery

	// The connection is told which head to write once the transport has
	// picked it. A nil template disarms a connection armed by a request that
	// was abandoned before it was written.
	var tmpl []headerLine
	sentCacheControl := true
	if lines, ok := r.Context().Value(headKey{}).([]headerLine); ok {
		tmpl = forwardLines(lines, readable)
		sentCacheControl = slices.ContainsFunc(lines, func(l headerLine) bool { return http.CanonicalHeaderKey(l.name) == "Cache-Control" })
	}
	ctx := httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		if c, ok := info.Conn.(*outConn); ok {
			c.arm(tmpl)
		}
	}})

	req, err := http.NewRequestWithContext(ctx, r.Method, u.String(), nil)
	if err != nil {
		return nil, err
	}
	copyHeaders(req.Header, r.Header)
	if readable {
		req.Header.Del("Accept-Encoding")
	}
	if !sentCacheControl {
		// Go's server adds it next to Pragma: no-cache; the client did not send it.
		req.Header.Del("Cache-Control")
	}
	if _, ok := req.Header["User-Agent"]; !ok {
		// Stops the transport from adding its own.
		req.Header.Set("User-Agent", "")
	}
	if r.ContentLength != 0 && r.Body != nil && r.Body != http.NoBody {
		req.Body = r.Body
		req.ContentLength = r.ContentLength
	}
	return req, nil
}

// forward streams the request upstream and the response back. Nothing is
// buffered: every read from upstream is written and flushed at once.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, rt *route, rest string, o forwardOpts) outcome {
	req, err := p.upstreamRequest(r, rt, rest, o.rewrite != nil)
	if err != nil {
		return p.upstreamError(w, o.rule, err)
	}
	resp, err := p.transport.RoundTrip(req)
	if err != nil {
		if r.Context().Err() != nil {
			return outcome{}
		}
		return p.upstreamError(w, o.rule, err)
	}
	defer resp.Body.Close()
	if o.rewrite != nil {
		return o.rewrite(w, r, resp, o)
	}
	return p.relay(w, r, resp, o)
}

// relay copies an upstream response to the client, applying byte-level faults.
func (p *Proxy) relay(w http.ResponseWriter, r *http.Request, resp *http.Response, o forwardOpts) outcome {
	copyHeaders(w.Header(), resp.Header)
	p.setFaultHeader(w.Header(), o.rule)
	for k := range resp.Trailer {
		w.Header().Add("Trailer", k)
	}
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	rc.Flush()

	if o.firstByteDelay > 0 && !sleep(r.Context(), o.rs, o.firstByteDelay) && r.Context().Err() != nil {
		return outcome{status: resp.StatusCode}
	}

	buf := make([]byte, 32*1024)
	var sent int64
	for {
		if o.hasCut && sent >= o.cutAfter {
			closeConn(w, false)
			return outcome{}
		}
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if o.hasCut && sent+int64(n) > o.cutAfter {
				chunk = chunk[:o.cutAfter-sent]
			}
			if o.garble {
				garble(chunk, sent, o.rule.seed)
			}
			if err := p.write(r.Context(), w, rc, chunk, o); err != nil {
				return outcome{status: resp.StatusCode}
			}
			sent += int64(len(chunk))
			continue
		}
		if readErr == io.EOF {
			for k, vs := range resp.Trailer {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			return outcome{status: resp.StatusCode}
		}
		if readErr != nil {
			// The upstream died mid-body; the client must not see a clean end.
			closeConn(w, false)
			return outcome{status: resp.StatusCode}
		}
	}
}

func (p *Proxy) write(ctx context.Context, w http.ResponseWriter, rc *http.ResponseController, chunk []byte, o forwardOpts) error {
	if o.bytesPerSec <= 0 {
		if _, err := w.Write(chunk); err != nil {
			return err
		}
		return rc.Flush()
	}
	piece := max(1, o.bytesPerSec/20)
	for len(chunk) > 0 {
		n := min(piece, len(chunk))
		if o.rs.isCleared() {
			n = len(chunk)
		}
		if _, err := w.Write(chunk[:n]); err != nil {
			return err
		}
		if err := rc.Flush(); err != nil {
			return err
		}
		chunk = chunk[n:]
		if len(chunk) > 0 {
			wait := time.Duration(float64(n) / float64(o.bytesPerSec) * float64(time.Second))
			if !sleep(ctx, o.rs, wait) && ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	return nil
}

func (p *Proxy) upstreamError(w http.ResponseWriter, r *rule, err error) outcome {
	p.log.Warn("upstream request failed", "error", err.Error())
	w.Header().Set(headerFaultlne, "upstream-error")
	w.Header().Set("Content-Type", "application/json")
	p.setFaultHeader(w.Header(), r)
	w.WriteHeader(http.StatusBadGateway)
	io.WriteString(w, `{"error":"upstream_unreachable"}`)
	return outcome{status: http.StatusBadGateway}
}

// sleep waits for d. It returns false when cut short by the client going
// away or by the rule set being cleared.
func sleep(ctx context.Context, rs *ruleSet, d time.Duration) bool {
	var cleared <-chan struct{}
	if rs != nil {
		cleared = rs.cleared
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	case <-cleared:
		return false
	}
}

// closeConn takes the connection away from the HTTP server and closes it.
// With reset it closes with linger 0, which sends a TCP RST.
func closeConn(w http.ResponseWriter, reset bool) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	if tcp, ok := conn.(interface{ SetLinger(int) error }); ok && reset {
		tcp.SetLinger(0)
	}
	conn.Close()
}

// garble flips every seventh byte with a mask derived from the seed and the
// position, so the output has the same length and is reproducible per seed.
func garble(chunk []byte, offset int64, seed uint64) {
	for i := range chunk {
		pos := offset + int64(i)
		if pos%7 != 0 {
			continue
		}
		// splitmix64 finaliser
		x := seed + uint64(pos/7+1)*0x9E3779B97F4A7C15
		x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
		x = (x ^ (x >> 27)) * 0x94D049BB133111EB
		mask := byte(x ^ (x >> 31))
		if mask == 0 {
			mask = 0xFF
		}
		chunk[i] ^= mask
	}
}

func (p *Proxy) inject(w http.ResponseWriter, r *http.Request, rt *route, rest string, d decision) outcome {
	rule, prm := d.rule, d.rule.params
	base := forwardOpts{rule: rule, rs: d.rs}
	switch rule.spec.Type {
	case fault.Latency:
		delay := time.Duration(prm.MS) * time.Millisecond
		if prm.JitterMS > 0 {
			rule.mu.Lock()
			delay += time.Duration((rule.rng.Float64()*2 - 1) * float64(prm.JitterMS) * float64(time.Millisecond))
			rule.mu.Unlock()
		}
		if delay > 0 && !sleep(r.Context(), d.rs, delay) && r.Context().Err() != nil {
			return outcome{}
		}
		return p.forward(w, r, rt, rest, base)

	case fault.HTTPError:
		h := w.Header()
		h.Set("Content-Type", "application/json")
		for k, v := range prm.Headers {
			h.Set(k, v)
		}
		out := outcome{status: prm.Status}
		if prm.RetryAfter != nil {
			h.Set("Retry-After", strconv.Itoa(*prm.RetryAfter))
			out.retryAfter = prm.RetryAfter
		}
		p.respond(w, rule, prm.Status, prm.Body)
		return out

	case fault.ErrorInBody:
		w.Header().Set("Content-Type", prm.ContentType)
		p.respond(w, rule, http.StatusOK, prm.Body)
		return outcome{status: http.StatusOK}

	case fault.Timeout:
		return p.hold(w, r, d.rs, prm.Hold)

	case fault.Reset:
		closeConn(w, true)
		return outcome{}

	case fault.SlowBody:
		base.bytesPerSec = prm.BytesPerSec
		return p.forward(w, r, rt, rest, base)

	case fault.CorruptBody:
		if prm.Mode == "truncate" {
			base.hasCut, base.cutAfter = true, int64(prm.AtBytes)
		} else {
			base.garble = true
		}
		return p.forward(w, r, rt, rest, base)
	}
	return p.injectLLM(w, r, rt, rest, d, base)
}

func (p *Proxy) respond(w http.ResponseWriter, r *rule, status int, body string) {
	p.setFaultHeader(w.Header(), r)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	io.WriteString(w, body)
}

// hold keeps the request open without sending anything. It ends when the hold
// elapses, the client gives up, or the faults are cleared; then the
// connection is closed with no response.
func (p *Proxy) hold(w http.ResponseWriter, r *http.Request, rs *ruleSet, d time.Duration) outcome {
	begin := time.Now()
	// The server only notices a closed client connection once the request
	// body has been consumed.
	io.Copy(io.Discard, r.Body)
	if rest := d - time.Since(begin); rest > 0 {
		sleep(r.Context(), rs, rest)
	}
	held := ms(time.Since(begin))
	closeConn(w, false)
	return outcome{clientHeldMs: &held}
}
