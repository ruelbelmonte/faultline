//go:build integration && load

package integration

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The load test measures; it does not judge speed. It fails only when
// requests fail or when the run's request count shows records were lost.
//
//	go test -tags "integration load" ./test/integration/ -run TestLoad -count=1 -v
//
// FAULTLINE_LOAD_SECONDS sets the length of each measurement (default 5).

type loadResult struct {
	name        string
	concurrency int
	requests    int
	errors      int
	seconds     float64
	mean, p50   time.Duration
	p95, p99    time.Duration
	max         time.Duration
}

func (r loadResult) rps() float64 { return float64(r.requests) / r.seconds }

// drive sends GET requests to url from `concurrency` keep-alive connections
// for d, as fast as responses come back.
func drive(name, url string, headers map[string]string, concurrency int, d time.Duration) loadResult {
	c := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: concurrency, MaxConnsPerHost: concurrency, DisableCompression: true}}
	defer c.CloseIdleConnections()
	lat := make([][]time.Duration, concurrency)
	errs := make([]int, concurrency)
	var wg sync.WaitGroup
	begin := time.Now()
	deadline := begin.Add(d)
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				req, _ := http.NewRequest(http.MethodGet, url, nil)
				for k, v := range headers {
					req.Header.Set(k, v)
				}
				req.Header.Set("X-Chaos-Correlation", strconv.Itoa(w)+"-"+strconv.Itoa(len(lat[w])))
				t0 := time.Now()
				resp, err := c.Do(req)
				if err == nil {
					_, err = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						err = fmt.Errorf("status %d", resp.StatusCode)
					}
				}
				if err != nil {
					errs[w]++
					continue
				}
				lat[w] = append(lat[w], time.Since(t0))
			}
		}()
	}
	wg.Wait()
	res := loadResult{name: name, concurrency: concurrency, seconds: time.Since(begin).Seconds()}
	var all []time.Duration
	for w := range lat {
		all = append(all, lat[w]...)
		res.errors += errs[w]
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	res.requests = len(all)
	if n := len(all); n > 0 {
		at := func(p float64) time.Duration { return all[min(int(p*float64(n)), n-1)] }
		var sum time.Duration
		for _, v := range all {
			sum += v
		}
		res.mean, res.p50, res.p95, res.p99, res.max = sum/time.Duration(n), at(0.50), at(0.95), at(0.99), all[n-1]
	}
	return res
}

func TestLoadProxyOverhead(t *testing.T) {
	seconds := 5
	if v, err := strconv.Atoi(os.Getenv("FAULTLINE_LOAD_SECONDS")); err == nil && v > 0 {
		seconds = v
	}
	d := time.Duration(seconds) * time.Second

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"1","stock":42,"price":9.99}`)
	}))
	defer up.Close()

	s := newStack(t, map[string]string{"svc": up.URL})
	p := s.startProxy(t)
	c := s.startControl(t)

	var results []loadResult
	for _, concurrency := range []int{1, 64} {
		results = append(results,
			drive("direct to upstream", up.URL+"/stock/1", nil, concurrency, d),
			drive("through proxy, no run", p.url("/svc/stock/1"), nil, concurrency, d))

		// A real run whose fault never matches: every request is still
		// checked against the rules, recorded, and collected by control.
		runID := c.createRun(t, `version: 1
name: load
target:
  route: svc
duration: 10m
faults:
  - id: never
    type: http_error
    match: {path: /never}
    params: {status: 503}
`)
		c.waitStatus(t, runID, 10*time.Second, "injecting")
		active := drive("through proxy, active run", p.url("/svc/stock/1"), scoped(runID), concurrency, d)
		results = append(results, active)
		if code := c.api(t, "POST", "/api/v1/runs/"+runID+"/abort", nil, nil); code != 200 {
			t.Fatalf("abort: status %d", code)
		}
		doc := c.waitDone(t, runID, 30*time.Second)
		if got := doc.Metrics.Proxy["svc"].Requests; got != active.requests+active.errors {
			t.Errorf("concurrency %d: the run counted %d proxied requests, %d were sent: records were lost", concurrency, got, active.requests+active.errors)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\n%-28s %5s %9s %9s %9s %9s %9s %9s %9s %6s\n", "scenario", "conns", "req/s", "mean", "p50", "p95", "p99", "max", "requests", "errors")
	for _, r := range results {
		fmt.Fprintf(&b, "%-28s %5d %9.0f %9s %9s %9s %9s %9s %9d %6d\n", r.name, r.concurrency, r.rps(),
			r.mean.Round(time.Microsecond), r.p50.Round(time.Microsecond), r.p95.Round(time.Microsecond), r.p99.Round(time.Microsecond), r.max.Round(time.Microsecond), r.requests, r.errors)
		if r.errors > 0 {
			t.Errorf("%s at %d connections: %d requests failed", r.name, r.concurrency, r.errors)
		}
	}
	t.Log(b.String())
	if strings.Contains(p.out.String(), "overwritten before they were read") {
		t.Error("the proxy reported that its record ring overflowed")
	}
}
