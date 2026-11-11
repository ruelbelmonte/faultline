//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	adminToken   = "itest-admin-token-0123456789"
	controlToken = "itest-control-token-0123456789"
)

// client never reuses connections, so the HTTP client never silently retries
// a request on a connection the proxy reset.
var client = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

type stack struct {
	dir          string
	cfgPath      string
	trafficAddr  string
	adminAddr    string
	controlAddr  string
	dataDir      string
	leaseTimeout string
	extra        string
	proxyExtra   string
	controlExtra string
	routes       map[string]string
	environment  string
}

func newStack(t testing.TB, routes map[string]string) *stack {
	t.Helper()
	dir := t.TempDir()
	return &stack{
		dir: dir, cfgPath: filepath.Join(dir, "faultline.yaml"),
		trafficAddr: freeAddr(t), adminAddr: freeAddr(t), controlAddr: freeAddr(t),
		dataDir: filepath.Join(dir, "data"), leaseTimeout: "10s", routes: routes, environment: "local",
	}
}

func (s *stack) writeConfig(t testing.TB) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "version: 1\nenvironment: %s\ndata_dir: %q\nexperiments_dir: %q\n", s.environment, s.dataDir, filepath.Join(s.dir, "experiments"))
	fmt.Fprintf(&b, "proxy:\n  listen: %s\n  admin_listen: %s\n  lease_timeout: %s\n%s", s.trafficAddr, s.adminAddr, s.leaseTimeout, s.proxyExtra)
	fmt.Fprintf(&b, "control:\n  listen: %s\n  proxy_admin_url: http://%s\n%s", s.controlAddr, s.adminAddr, s.controlExtra)
	b.WriteString(s.extra)
	b.WriteString("routes:\n")
	for name, up := range s.routes {
		fmt.Fprintf(&b, "  - name: %s\n    upstream: %s\n", name, up)
	}
	if err := os.WriteFile(s.cfgPath, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

type proxyProc struct {
	*proc
	s *stack
}

func (s *stack) startProxy(t testing.TB) *proxyProc {
	t.Helper()
	s.writeConfig(t)
	p := &proxyProc{s: s}
	p.proc = start(t, "faultline", []string{"FAULTLINE_ADMIN_TOKEN=" + adminToken}, "proxy", "--config", s.cfgPath)
	waitHTTP(t, "http://"+s.adminAddr+"/admin/v1/healthz")
	return p
}

func (p *proxyProc) url(path string) string   { return "http://" + p.s.trafficAddr + path }
func (p *proxyProc) admin(path string) string { return "http://" + p.s.adminAddr + path }

// adminCall performs an authenticated admin request and decodes the JSON body.
func (p *proxyProc) adminCall(t testing.TB, method, path string, body any, out any) int {
	t.Helper()
	return apiCall(t, method, p.admin(path), adminToken, body, out)
}

func apiCall(t testing.TB, method, url, token string, body any, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: status %d, undecodable body %q: %v", method, url, resp.StatusCode, data, err)
		}
	}
	return resp.StatusCode
}

type obj = map[string]any

// flt builds a fault that is active now and for the next minute.
func flt(id, typ, route string, params obj, mods ...func(obj)) obj {
	f := obj{
		"id": id, "type": typ, "route": route,
		"strategy": obj{"kind": "always"},
		"params":   params,
		"start_at": time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano),
		"end_at":   time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
	}
	for _, m := range mods {
		m(f)
	}
	return f
}

func strategy(s obj) func(obj) { return func(f obj) { f["strategy"] = s } }
func match(method, path string) func(obj) {
	return func(f obj) {
		m := obj{}
		if method != "" {
			m["method"] = method
		}
		if path != "" {
			m["path"] = path
		}
		f["match"] = m
	}
}
func window(startIn, endIn time.Duration) func(obj) {
	return func(f obj) {
		f["start_at"] = time.Now().Add(startIn).UTC().Format(time.RFC3339Nano)
		f["end_at"] = time.Now().Add(endIn).UTC().Format(time.RFC3339Nano)
	}
}

const testRun = "run_aaaaaaaaaaaa"

func (p *proxyProc) install(t testing.TB, runID, scope string, faults ...obj) {
	t.Helper()
	p.installSet(t, obj{"run_id": runID, "scope_mode": scope, "lease_id": "lease-" + runID, "faults": faults})
}

func (p *proxyProc) installSet(t testing.TB, set obj) {
	t.Helper()
	var resp obj
	if code := p.adminCall(t, http.MethodPut, "/admin/v1/faults", set, &resp); code != 200 {
		t.Fatalf("installing faults: status %d %v", code, resp)
	}
}

func (p *proxyProc) clear(t testing.TB) {
	t.Helper()
	if code := p.adminCall(t, http.MethodDelete, "/admin/v1/faults", nil, nil); code != 200 {
		t.Fatalf("clearing faults: status %d", code)
	}
}

type activeFaults struct {
	RunID   *string `json:"run_id"`
	LeaseID string  `json:"lease_id"`
	Faults  []struct {
		ID, Type, Route string
		StartAt         time.Time `json:"start_at"`
		EndAt           time.Time `json:"end_at"`
		Triggers        int64
	}
}

func (p *proxyProc) active(t testing.TB) activeFaults {
	t.Helper()
	var a activeFaults
	if code := p.adminCall(t, http.MethodGet, "/admin/v1/faults/active", nil, &a); code != 200 {
		t.Fatalf("faults/active: status %d", code)
	}
	return a
}

func (p *proxyProc) triggers(t testing.TB, id string) int64 {
	t.Helper()
	for _, f := range p.active(t).Faults {
		if f.ID == id {
			return f.Triggers
		}
	}
	t.Fatalf("fault %s is not active", id)
	return 0
}

type record struct {
	Seq          int64
	TS           time.Time
	Route        string
	Method       string
	Path         string
	Status       int
	DurationMs   float64 `json:"duration_ms"`
	Scoped       bool
	Correlation  string
	FaultID      *string  `json:"fault_id"`
	FaultType    *string  `json:"fault_type"`
	RetryAfter   *int     `json:"retry_after"`
	ClientHeldMs *float64 `json:"client_held_ms"`
}

func (p *proxyProc) records(t testing.TB, runID string, since int64) ([]record, int64) {
	t.Helper()
	var page struct {
		Records []record
		Next    int64
	}
	path := fmt.Sprintf("/admin/v1/records?run_id=%s&since=%d", runID, since)
	if code := p.adminCall(t, http.MethodGet, path, nil, &page); code != 200 {
		t.Fatalf("records: status %d", code)
	}
	return page.Records, page.Next
}

// recordsN waits until the run has at least n records. The proxy stores a
// record just after it finishes the response, so a client can get there first.
func (p *proxyProc) recordsN(t testing.TB, runID string, n int) []record {
	t.Helper()
	var recs []record
	eventually(t, 3*time.Second, fmt.Sprintf("%d records", n), func() bool {
		recs, _ = p.records(t, runID, 0)
		return len(recs) >= n
	})
	return recs
}

type result struct {
	status int
	header http.Header
	body   []byte
	err    error
	took   time.Duration
}

// call sends one request through the client and reports whatever happened,
// including transport errors and partial bodies.
func call(method, url string, headers map[string]string, body string) result {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return result{err: err}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	begin := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return result{err: err, took: time.Since(begin)}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return result{status: resp.StatusCode, header: resp.Header, body: data, err: err, took: time.Since(begin)}
}

func scoped(runID string) map[string]string { return map[string]string{"X-Chaos-Run": runID} }

type echo struct {
	Method  string
	Path    string
	Query   string
	Host    string
	Headers http.Header
	Body    string
}

// upstream is a real HTTP server standing in for a dependency.
type upstream struct {
	*httptest.Server
	hits atomic.Int64
}

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}

func newUpstream(t testing.TB) *upstream {
	u := &upstream{}
	mux := http.NewServeMux()
	mux.HandleFunc("/bytes/{n}", func(w http.ResponseWriter, r *http.Request) {
		var n int
		fmt.Sscanf(r.PathValue("n"), "%d", &n)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(n))
		w.Write(pattern(n))
	})
	mux.HandleFunc("/chunked/{n}", func(w http.ResponseWriter, r *http.Request) {
		var n int
		fmt.Sscanf(r.PathValue("n"), "%d", &n)
		data := pattern(n)
		for len(data) > 0 {
			k := min(100, len(data))
			w.Write(data[:k])
			w.(http.Flusher).Flush()
			data = data[k:]
		}
	})
	mux.HandleFunc("/status/{code}", func(w http.ResponseWriter, r *http.Request) {
		var code int
		fmt.Sscanf(r.PathValue("code"), "%d", &code)
		w.WriteHeader(code)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", "echo")
		json.NewEncoder(w).Encode(echo{
			Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery, Host: r.Host,
			Headers: r.Header, Body: string(body),
		})
	})
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(u.Close)
	return u
}

// startEchoProxy starts an upstream and a proxy with one route, "svc".
func startEchoProxy(t testing.TB, mods ...func(*stack)) (*proxyProc, *upstream) {
	t.Helper()
	up := newUpstream(t)
	s := newStack(t, map[string]string{"svc": up.URL})
	for _, m := range mods {
		m(s)
	}
	return s.startProxy(t), up
}
