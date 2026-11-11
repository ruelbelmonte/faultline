//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// raw sends a request over a bare TCP connection and reads until the peer
// closes or resets, so the test sees exactly what happened on the wire.
func raw(t testing.TB, addr, request string) (data []byte, err error, took time.Duration) {
	t.Helper()
	conn, dialErr := net.Dial("tcp", addr)
	if dialErr != nil {
		t.Fatal(dialErr)
	}
	defer conn.Close()
	begin := time.Now()
	if _, err := conn.Write([]byte(request)); err != nil {
		return nil, err, time.Since(begin)
	}
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	data, err = io.ReadAll(conn)
	return data, err, time.Since(begin)
}

func rawGet(path, runID string) string {
	return fmt.Sprintf("GET %s HTTP/1.1\r\nHost: test\r\nX-Chaos-Run: %s\r\n\r\n", path, runID)
}

func TestProxyRouting(t *testing.T) {
	p, up := startEchoProxy(t)

	t.Run("preserves method, path, query, headers and body", func(t *testing.T) {
		res := call("POST", p.url("/svc/a/b%2Fc/d?x=1&y=two%20words&x=3"), map[string]string{
			"X-Custom":   "kept",
			"Connection": "close, X-Hop",
			"X-Hop":      "dropped",
			"Keep-Alive": "timeout=5",
		}, `{"hello":"world"}`)
		if res.err != nil || res.status != 200 {
			t.Fatalf("status %d err %v", res.status, res.err)
		}
		var e echo
		if err := json.Unmarshal(res.body, &e); err != nil {
			t.Fatal(err)
		}
		if e.Method != "POST" || e.Path != "/a/b%2Fc/d" || e.Query != "x=1&y=two%20words&x=3" || e.Body != `{"hello":"world"}` {
			t.Fatalf("request not preserved: %+v", e)
		}
		if e.Headers.Get("X-Custom") != "kept" {
			t.Fatal("end-to-end header was dropped")
		}
		if e.Headers.Get("X-Hop") != "" || e.Headers.Get("Keep-Alive") != "" {
			t.Fatalf("hop-by-hop headers were forwarded: %v", e.Headers)
		}
		if e.Headers.Get("X-Forwarded-For") != "" {
			t.Fatal("the proxy must not add forwarding headers")
		}
		if want := strings.TrimPrefix(up.URL, "http://"); e.Host != want {
			t.Fatalf("Host must be rewritten to the upstream host %q, got %q", want, e.Host)
		}
		if res.header.Get("X-Upstream") != "echo" {
			t.Fatal("upstream response header was dropped")
		}
		if res.header.Get("X-Faultline-Fault") != "" {
			t.Fatal("unfaulted response carries a fault header")
		}
	})

	t.Run("does not add a User-Agent", func(t *testing.T) {
		data, err, _ := raw(t, p.s.trafficAddr, "GET /svc/ua HTTP/1.1\r\nHost: test\r\nConnection: close\r\n\r\n")
		if err != nil {
			t.Fatal(err)
		}
		_, body, _ := bytes.Cut(data, []byte("\r\n\r\n"))
		if bytes.Contains(bytes.ToLower(body), []byte("user-agent")) {
			t.Fatalf("a User-Agent header was invented: %s", body)
		}
	})

	t.Run("route root", func(t *testing.T) {
		res := call("GET", p.url("/svc"), nil, "")
		var e echo
		json.Unmarshal(res.body, &e)
		if res.status != 200 || e.Path != "/" {
			t.Fatalf("status %d path %q", res.status, e.Path)
		}
	})

	t.Run("upstream status codes pass through", func(t *testing.T) {
		for _, code := range []int{201, 404, 418, 500, 503} {
			if res := call("GET", p.url(fmt.Sprintf("/svc/status/%d", code)), nil, ""); res.status != code {
				t.Fatalf("expected %d, got %d", code, res.status)
			}
		}
	})

	t.Run("unknown route", func(t *testing.T) {
		for _, path := range []string{"/nope/x", "/", "/SVC/x"} {
			res := call("GET", p.url(path), nil, "")
			if res.status != 404 || string(res.body) != `{"error":"unknown_route"}` || res.header.Get("X-Faultline") != "unknown-route" {
				t.Fatalf("%s: got %d %q %q", path, res.status, res.body, res.header.Get("X-Faultline"))
			}
		}
	})

	t.Run("upstream down", func(t *testing.T) {
		s := newStack(t, map[string]string{"dead": "http://" + freeAddr(t)})
		dead := s.startProxy(t)
		res := call("GET", dead.url("/dead/x"), nil, "")
		if res.status != 502 || res.header.Get("X-Faultline") != "upstream-error" {
			t.Fatalf("expected 502 upstream-error, got %d %q", res.status, res.header.Get("X-Faultline"))
		}
	})
}

func TestProxyChainThroughRealProxy(t *testing.T) {
	s := newStack(t, nil)
	d := startDemo(t, "naive", "http://"+s.trafficAddr)
	s.routes = map[string]string{"inventory": "http://" + d.inventory, "pricing": "http://" + d.pricing}
	p := s.startProxy(t)

	// An idle rule set makes the proxy record requests, which shows what was forwarded.
	p.install(t, testRun, "header", flt("idle", "reset", "inventory", obj{}, match("DELETE", "")))
	resp, body := get(t, "http://"+d.shop+"/api/orders/7", map[string]string{"X-Chaos-Run": testRun, "X-Chaos-Correlation": "c-1"})
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"price":9.99`) {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	recs := p.recordsN(t, testRun, 2)
	if len(recs) != 2 {
		t.Fatalf("expected 2 proxied calls, got %+v", recs)
	}
	seen := map[string]string{}
	for _, r := range recs {
		seen[r.Route] = r.Path
		if !r.Scoped || r.Correlation != "c-1" || r.Status != 200 {
			t.Fatalf("scope or correlation header lost between hops: %+v", r)
		}
	}
	if seen["inventory"] != "/stock/7" || seen["pricing"] != "/price/7" {
		t.Fatalf("unexpected hops: %v", seen)
	}
}

func TestProxyStreamingIsNotBuffered(t *testing.T) {
	s := newStack(t, nil)
	d := startDemo(t, "naive", "http://"+s.trafficAddr)
	s.routes = map[string]string{"llm": "http://" + d.llm}
	p := s.startProxy(t)

	t.Run("response: time to first byte", func(t *testing.T) {
		resp, begin := postJSON(t, p.url("/llm/v1/messages"), `{"model":"m","stream":true,"messages":[]}`, nil)
		events, err := readSSE(t, resp, begin)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 25 {
			t.Fatalf("expected 25 events, got %d", len(events))
		}
		first, last := events[0].at, events[len(events)-1].at
		if first > 300*time.Millisecond {
			t.Fatalf("first event arrived after %s: the stream is being buffered", first)
		}
		if last < 950*time.Millisecond {
			t.Fatalf("stream finished after %s, expected about 1s", last)
		}
		// Each delta must arrive about when the upstream sent it, not in one burst at the end.
		for i := 2; i < 22; i++ {
			expected := time.Duration(i-1) * 50 * time.Millisecond
			if events[i].at > expected+300*time.Millisecond {
				t.Fatalf("delta %d arrived at %s, upstream sent it around %s", i-1, events[i].at, expected)
			}
		}
	})

	t.Run("request body is streamed", func(t *testing.T) {
		firstChunk := make(chan time.Time, 1)
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			buf := make([]byte, 5)
			io.ReadFull(r.Body, buf)
			firstChunk <- time.Now()
			io.Copy(io.Discard, r.Body)
		}))
		defer up.Close()
		s2 := newStack(t, map[string]string{"up": up.URL})
		p2 := s2.startProxy(t)

		pr, pw := io.Pipe()
		req, _ := http.NewRequest("POST", p2.url("/up/upload"), pr)
		done := make(chan error, 1)
		go func() {
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
			}
			done <- err
		}()
		pw.Write([]byte("first"))
		select {
		case <-firstChunk:
		case <-time.After(3 * time.Second):
			t.Fatal("upstream did not receive the first chunk while the request body was still open")
		}
		pw.Write([]byte("second"))
		pw.Close()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestAdminAuth(t *testing.T) {
	p, _ := startEchoProxy(t)
	var health obj
	if code := apiCall(t, "GET", p.admin("/admin/v1/healthz"), "", nil, &health); code != 200 || health["ok"] != true {
		t.Fatalf("healthz must be open: %d %v", code, health)
	}
	endpoints := []struct{ method, path string }{
		{"PUT", "/admin/v1/faults"},
		{"DELETE", "/admin/v1/faults"},
		{"POST", "/admin/v1/lease/renew"},
		{"GET", "/admin/v1/faults/active"},
		{"GET", "/admin/v1/records"},
		{"GET", "/admin/v1/stats"},
		{"GET", "/admin/v1/unknown"},
	}
	for _, e := range endpoints {
		for _, tok := range []string{"", "wrong-token-0123456789", adminToken + "x", adminToken[:len(adminToken)-1]} {
			if code := apiCall(t, e.method, p.admin(e.path), tok, obj{}, nil); code != 401 {
				t.Errorf("%s %s with token %q: expected 401, got %d", e.method, e.path, tok, code)
			}
		}
	}
	req, _ := http.NewRequest("GET", p.admin("/admin/v1/stats"), nil)
	req.Header.Set("Authorization", adminToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("a token without the Bearer scheme must be rejected, got %d", resp.StatusCode)
	}
	// The admin API is not reachable through the traffic port.
	if res := call("GET", p.url("/admin/v1/healthz"), nil, ""); res.status != 404 {
		t.Fatalf("admin path on the traffic port: expected 404, got %d", res.status)
	}
}

func TestAdminValidation(t *testing.T) {
	p, _ := startEchoProxy(t)
	good := flt("f1", "reset", "svc", obj{})
	cases := map[string]obj{
		"missing run_id":     {"scope_mode": "header", "lease_id": "l", "faults": []obj{good}},
		"missing lease_id":   {"run_id": testRun, "scope_mode": "header", "faults": []obj{good}},
		"bad scope_mode":     {"run_id": testRun, "scope_mode": "some", "lease_id": "l", "faults": []obj{good}},
		"unknown route":      {"run_id": testRun, "scope_mode": "header", "lease_id": "l", "faults": []obj{flt("f1", "reset", "nope", obj{})}},
		"unknown type":       {"run_id": testRun, "scope_mode": "header", "lease_id": "l", "faults": []obj{flt("f1", "gremlin", "svc", obj{})}},
		"bad params":         {"run_id": testRun, "scope_mode": "header", "lease_id": "l", "faults": []obj{flt("f1", "http_error", "svc", obj{})}},
		"bad strategy":       {"run_id": testRun, "scope_mode": "header", "lease_id": "l", "faults": []obj{flt("f1", "reset", "svc", obj{}, strategy(obj{"kind": "probability", "p": 2}))}},
		"missing end_at":     {"run_id": testRun, "scope_mode": "header", "lease_id": "l", "faults": []obj{flt("f1", "reset", "svc", obj{}, func(f obj) { delete(f, "end_at") })}},
		"end before start":   {"run_id": testRun, "scope_mode": "header", "lease_id": "l", "faults": []obj{flt("f1", "reset", "svc", obj{}, window(time.Minute, time.Second))}},
		"duplicate fault id": {"run_id": testRun, "scope_mode": "header", "lease_id": "l", "faults": []obj{good, good}},
	}
	for name, body := range cases {
		var resp struct {
			Error  string
			Errors []struct{ Path, Message string }
		}
		if code := p.adminCall(t, "PUT", "/admin/v1/faults", body, &resp); code != 400 || len(resp.Errors) == 0 {
			t.Errorf("%s: expected 400 with errors, got %d %+v", name, code, resp)
		}
	}
	if a := p.active(t); a.RunID != nil {
		t.Fatal("a rejected rule set must not be installed")
	}
	req, _ := http.NewRequest("PUT", p.admin("/admin/v1/faults"), strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, _ := client.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("malformed JSON: expected 400, got %d", resp.StatusCode)
	}
}

func TestFaultLatency(t *testing.T) {
	p, _ := startEchoProxy(t)
	if res := call("GET", p.url("/svc/x"), scoped(testRun), ""); res.took > 250*time.Millisecond {
		t.Fatalf("baseline too slow to measure latency: %s", res.took)
	}
	p.install(t, testRun, "header", flt("lat", "latency", "svc", obj{"ms": 400}))
	res := call("GET", p.url("/svc/x"), scoped(testRun), "")
	if res.status != 200 || res.took < 400*time.Millisecond || res.took > 900*time.Millisecond {
		t.Fatalf("expected a 200 after about 400ms, got %d after %s", res.status, res.took)
	}
	if res.header.Get("X-Faultline-Fault") != "lat" {
		t.Fatal("missing X-Faultline-Fault header")
	}
	var e echo
	if json.Unmarshal(res.body, &e) != nil || e.Path != "/x" {
		t.Fatal("the delayed request must still reach the upstream")
	}

	p.install(t, testRun, "header", flt("jit", "latency", "svc", obj{"ms": 300, "jitter_ms": 200}))
	var lo, hi time.Duration = time.Hour, 0
	for i := 0; i < 12; i++ {
		res := call("GET", p.url("/svc/x"), scoped(testRun), "")
		if res.took < 100*time.Millisecond || res.took > 800*time.Millisecond {
			t.Fatalf("jittered delay %s outside 300ms +/- 200ms", res.took)
		}
		lo, hi = min(lo, res.took), max(hi, res.took)
	}
	if hi-lo < 40*time.Millisecond {
		t.Fatalf("jitter had no visible effect: delays ranged %s to %s", lo, hi)
	}
}

func TestFaultHTTPError(t *testing.T) {
	p, up := startEchoProxy(t)
	p.install(t, testRun, "header", flt("e1", "http_error", "svc", obj{"status": 503}))
	before := up.hits.Load()
	res := call("GET", p.url("/svc/x"), scoped(testRun), "")
	if res.status != 503 || string(res.body) != `{"error":"faultline injected"}` {
		t.Fatalf("got %d %q", res.status, res.body)
	}
	if res.header.Get("X-Faultline-Fault") != "e1" || res.header.Get("Retry-After") != "" {
		t.Fatalf("unexpected headers: %v", res.header)
	}
	if up.hits.Load() != before {
		t.Fatal("http_error must not call the upstream")
	}

	p.install(t, testRun, "header", flt("e2", "http_error", "svc", obj{
		"status": 429, "body": "slow down", "retry_after": 7,
		"headers": obj{"X-Reason": "test", "Content-Type": "text/plain"},
	}))
	res = call("POST", p.url("/svc/x"), scoped(testRun), "payload")
	if res.status != 429 || string(res.body) != "slow down" || res.header.Get("Retry-After") != "7" ||
		res.header.Get("X-Reason") != "test" || res.header.Get("Content-Type") != "text/plain" {
		t.Fatalf("got %d %q %v", res.status, res.body, res.header)
	}
	recs := p.recordsN(t, testRun, 2)
	last := recs[len(recs)-1]
	if last.Status != 429 || last.RetryAfter == nil || *last.RetryAfter != 7 || *last.FaultID != "e2" || *last.FaultType != "http_error" {
		t.Fatalf("unexpected record: %+v", last)
	}
}

func TestFaultErrorInBody(t *testing.T) {
	p, up := startEchoProxy(t)
	p.install(t, testRun, "header",
		flt("b1", "error_in_body", "svc", obj{"body": `{"error":"quota"}`}, match("GET", "/json")),
		flt("b2", "error_in_body", "svc", obj{"body": "<oops/>", "content_type": "application/xml"}, match("GET", "/xml")))
	before := up.hits.Load()
	res := call("GET", p.url("/svc/json"), scoped(testRun), "")
	if res.status != 200 || string(res.body) != `{"error":"quota"}` || res.header.Get("Content-Type") != "application/json" || res.header.Get("X-Faultline-Fault") != "b1" {
		t.Fatalf("got %d %q %v", res.status, res.body, res.header)
	}
	res = call("GET", p.url("/svc/xml"), scoped(testRun), "")
	if res.status != 200 || string(res.body) != "<oops/>" || res.header.Get("Content-Type") != "application/xml" {
		t.Fatalf("got %d %q %v", res.status, res.body, res.header)
	}
	if up.hits.Load() != before {
		t.Fatal("error_in_body must not call the upstream")
	}
}

func TestFaultTimeout(t *testing.T) {
	p, up := startEchoProxy(t)
	p.install(t, testRun, "header", flt("t1", "timeout", "svc", obj{"hold": "1200ms"}))
	before := up.hits.Load()

	t.Run("holds, then closes with no response", func(t *testing.T) {
		data, err, took := raw(t, p.s.trafficAddr, rawGet("/svc/x", testRun))
		if len(data) != 0 {
			t.Fatalf("nothing may be sent during a hold, got %q", data)
		}
		if err != nil {
			t.Fatalf("expected a clean close after the hold, got %v", err)
		}
		if took < 1200*time.Millisecond || took > 2*time.Second {
			t.Fatalf("expected the connection to be held about 1.2s, was %s", took)
		}
		recs := p.recordsN(t, testRun, 1)
		r := recs[len(recs)-1]
		if r.Status != 0 || r.ClientHeldMs == nil || *r.ClientHeldMs < 1190 || *r.ClientHeldMs > 2000 {
			t.Fatalf("unexpected record: %+v", r)
		}
	})

	t.Run("records how long an impatient client held", func(t *testing.T) {
		c := &http.Client{Timeout: 300 * time.Millisecond, Transport: &http.Transport{DisableKeepAlives: true}}
		req, _ := http.NewRequest("POST", p.url("/svc/x"), strings.NewReader("with a body"))
		req.Header.Set("X-Chaos-Run", testRun)
		if _, err := c.Do(req); err == nil {
			t.Fatal("expected the client to time out")
		}
		var r record
		eventually(t, 3*time.Second, "record of the abandoned request", func() bool {
			recs, _ := p.records(t, testRun, 0)
			if len(recs) == 0 {
				return false
			}
			r = recs[len(recs)-1]
			return r.Method == "POST"
		})
		if r.ClientHeldMs == nil || *r.ClientHeldMs < 250 || *r.ClientHeldMs > 900 {
			t.Fatalf("client_held_ms should be about 300, got %+v", r)
		}
	})

	t.Run("default hold is 30s", func(t *testing.T) {
		p.install(t, testRun, "header", flt("t2", "timeout", "svc", obj{}))
		done := make(chan time.Duration, 1)
		go func() {
			_, _, took := raw(t, p.s.trafficAddr, rawGet("/svc/x", testRun))
			done <- took
		}()
		select {
		case took := <-done:
			t.Fatalf("a default hold must outlast 2s, ended after %s", took)
		case <-time.After(2 * time.Second):
		}
		p.clear(t)
		<-done
	})

	if up.hits.Load() != before {
		t.Fatal("timeout must not call the upstream")
	}
}

func TestFaultReset(t *testing.T) {
	p, up := startEchoProxy(t)
	p.install(t, testRun, "header", flt("r1", "reset", "svc", obj{}))
	before := up.hits.Load()
	data, err, took := raw(t, p.s.trafficAddr, rawGet("/svc/x", testRun))
	if len(data) != 0 {
		t.Fatalf("a reset must not send a response, got %q", data)
	}
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("expected a TCP reset, got a clean close (err=%v)", err)
	}
	if took > time.Second {
		t.Fatalf("a reset must be immediate, took %s", took)
	}
	if res := call("GET", p.url("/svc/x"), scoped(testRun), ""); res.err == nil {
		t.Fatalf("an HTTP client must see a transport error, got status %d", res.status)
	}
	if up.hits.Load() != before {
		t.Fatal("reset must not call the upstream")
	}
	recs := p.recordsN(t, testRun, 2)
	if r := recs[0]; r.Status != 0 || *r.FaultType != "reset" || r.ClientHeldMs != nil {
		t.Fatalf("unexpected record: %+v", r)
	}
}

func TestFaultSlowBody(t *testing.T) {
	p, _ := startEchoProxy(t)
	p.install(t, testRun, "header", flt("s1", "slow_body", "svc", obj{"bytes_per_sec": 1000}))
	res := call("GET", p.url("/svc/bytes/2000"), scoped(testRun), "")
	if res.err != nil || res.status != 200 || !bytes.Equal(res.body, pattern(2000)) {
		t.Fatalf("body must arrive intact: status %d err %v len %d", res.status, res.err, len(res.body))
	}
	if res.took < 1700*time.Millisecond || res.took > 4*time.Second {
		t.Fatalf("2000 bytes at 1000 B/s should take about 2s, took %s", res.took)
	}
	if res.header.Get("X-Faultline-Fault") != "s1" {
		t.Fatal("missing X-Faultline-Fault header")
	}
	if fast := call("GET", p.url("/svc/bytes/2000"), nil, ""); fast.took > 500*time.Millisecond {
		t.Fatalf("unscoped request was throttled: %s", fast.took)
	}
}

func TestFaultCorruptBody(t *testing.T) {
	p, _ := startEchoProxy(t)

	t.Run("truncate forwards exactly at_bytes then cuts", func(t *testing.T) {
		for i, path := range []string{"/svc/bytes/1000", "/svc/chunked/1000"} {
			p.install(t, testRun, "header", flt("c1", "corrupt_body", "svc", obj{"mode": "truncate", "at_bytes": 137}))
			res := call("GET", p.url(path), scoped(testRun), "")
			if res.status != 200 || res.header.Get("X-Faultline-Fault") != "c1" {
				t.Fatalf("%s: status %d headers %v", path, res.status, res.header)
			}
			if !bytes.Equal(res.body, pattern(1000)[:137]) {
				t.Fatalf("%s: expected exactly the first 137 bytes, got %d", path, len(res.body))
			}
			if res.err == nil {
				t.Fatalf("%s: the client must see an abrupt end, not a clean one", path)
			}
			recs := p.recordsN(t, testRun, i+1)
			if r := recs[len(recs)-1]; r.Status != 0 {
				t.Fatalf("%s: a cut response is recorded with status 0, got %+v", path, r)
			}
		}
	})

	t.Run("truncate beyond the body leaves it whole", func(t *testing.T) {
		p.install(t, testRun, "header", flt("c2", "corrupt_body", "svc", obj{"mode": "truncate", "at_bytes": 5000}))
		res := call("GET", p.url("/svc/bytes/1000"), scoped(testRun), "")
		if res.err != nil || !bytes.Equal(res.body, pattern(1000)) {
			t.Fatalf("err %v len %d", res.err, len(res.body))
		}
	})

	t.Run("truncate at zero", func(t *testing.T) {
		p.install(t, testRun, "header", flt("c3", "corrupt_body", "svc", obj{"mode": "truncate", "at_bytes": 0}))
		res := call("GET", p.url("/svc/bytes/1000"), scoped(testRun), "")
		if res.status != 200 || len(res.body) != 0 || res.err == nil {
			t.Fatalf("status %d len %d err %v", res.status, len(res.body), res.err)
		}
	})

	t.Run("garble keeps the length and is reproducible per seed", func(t *testing.T) {
		garbled := func(seed int) []byte {
			p.installSet(t, obj{"run_id": testRun, "scope_mode": "header", "lease_id": "l", "seed": seed,
				"faults": []obj{flt("g1", "corrupt_body", "svc", obj{"mode": "garble"})}})
			res := call("GET", p.url("/svc/bytes/1000"), scoped(testRun), "")
			if res.err != nil || res.status != 200 {
				t.Fatalf("status %d err %v", res.status, res.err)
			}
			return res.body
		}
		a, b, c := garbled(42), garbled(42), garbled(43)
		if len(a) != 1000 {
			t.Fatalf("garble must keep the length, got %d", len(a))
		}
		if bytes.Equal(a, pattern(1000)) {
			t.Fatal("garble did not change the body")
		}
		if !bytes.Equal(a, b) {
			t.Fatal("the same seed must produce the same corruption")
		}
		if bytes.Equal(a, c) {
			t.Fatal("a different seed should produce different corruption")
		}
		diff := 0
		for i := range a {
			if a[i] != pattern(1000)[i] {
				diff++
			}
		}
		if diff < 100 || diff > 500 {
			t.Fatalf("expected a moderate share of flipped bytes, got %d of 1000", diff)
		}
	})
}

// faultedPattern sends n scoped requests and reports which ones were faulted.
func faultedPattern(t testing.TB, p *proxyProc, n int) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < n; i++ {
		switch res := call("GET", p.url("/svc/x"), scoped(testRun), ""); res.status {
		case 503:
			b.WriteByte('F')
		case 200:
			b.WriteByte('.')
		default:
			t.Fatalf("unexpected status %d (err %v)", res.status, res.err)
		}
	}
	return b.String()
}

func TestStrategies(t *testing.T) {
	p, _ := startEchoProxy(t)
	install := func(seed any, s obj) {
		set := obj{"run_id": testRun, "scope_mode": "header", "lease_id": "l",
			"faults": []obj{flt("f1", "http_error", "svc", obj{"status": 503}, strategy(s))}}
		if seed != nil {
			set["seed"] = seed
		}
		p.installSet(t, set)
	}

	t.Run("always", func(t *testing.T) {
		install(nil, obj{"kind": "always"})
		if got := faultedPattern(t, p, 6); got != "FFFFFF" {
			t.Fatalf("got %s", got)
		}
		if n := p.triggers(t, "f1"); n != 6 {
			t.Fatalf("triggers = %d, want 6", n)
		}
	})

	t.Run("burst", func(t *testing.T) {
		install(nil, obj{"kind": "burst", "count": 3})
		if got := faultedPattern(t, p, 8); got != "FFF....." {
			t.Fatalf("got %s", got)
		}
		if n := p.triggers(t, "f1"); n != 3 {
			t.Fatalf("triggers = %d, want 3", n)
		}
	})

	t.Run("every_nth", func(t *testing.T) {
		install(nil, obj{"kind": "every_nth", "n": 3})
		if got := faultedPattern(t, p, 10); got != "..F..F..F." {
			t.Fatalf("got %s", got)
		}
		if n := p.triggers(t, "f1"); n != 3 {
			t.Fatalf("triggers = %d, want 3", n)
		}
	})

	t.Run("probability", func(t *testing.T) {
		install(7, obj{"kind": "probability", "p": 0.3})
		a := faultedPattern(t, p, 200)
		install(7, obj{"kind": "probability", "p": 0.3})
		b := faultedPattern(t, p, 200)
		if a != b {
			t.Fatalf("the same seed must fault the same requests:\n%s\n%s", a, b)
		}
		install(8, obj{"kind": "probability", "p": 0.3})
		if c := faultedPattern(t, p, 200); c == a {
			t.Fatal("a different seed should fault different requests")
		}
		if n := strings.Count(a, "F"); n < 35 || n > 90 {
			t.Fatalf("p=0.3 over 200 requests faulted %d", n)
		}
		install(nil, obj{"kind": "probability", "p": 1})
		if got := faultedPattern(t, p, 5); got != "FFFFF" {
			t.Fatalf("p=1 must always fault, got %s", got)
		}
	})

	t.Run("counter only counts matching requests", func(t *testing.T) {
		p.install(t, testRun, "header", flt("f1", "http_error", "svc", obj{"status": 503},
			strategy(obj{"kind": "every_nth", "n": 2}), match("GET", "/x")))
		for i := 0; i < 3; i++ {
			call("GET", p.url("/svc/other"), scoped(testRun), "")
			call("POST", p.url("/svc/x"), scoped(testRun), "")
			call("GET", p.url("/svc/x"), nil, "")
		}
		if got := faultedPattern(t, p, 4); got != ".F.F" {
			t.Fatalf("non-matching requests advanced the counter: %s", got)
		}
	})
}

func TestScoping(t *testing.T) {
	p, _ := startEchoProxy(t)

	t.Run("header scope", func(t *testing.T) {
		p.install(t, testRun, "header", flt("f1", "http_error", "svc", obj{"status": 503}))
		cases := []struct {
			name    string
			headers map[string]string
			want    int
			scoped  bool
		}{
			{"own run id", scoped(testRun), 503, true},
			{"no header", nil, 200, false},
			{"another run id", scoped("run_bbbbbbbbbbbb"), 200, false},
			{"prefix of the run id", scoped(testRun[:10]), 200, false},
			{"empty header", scoped(""), 200, false},
		}
		for _, tc := range cases {
			res := call("GET", p.url("/svc/x"), tc.headers, "")
			if res.status != tc.want {
				t.Errorf("%s: expected %d, got %d", tc.name, tc.want, res.status)
			}
			if tc.want == 200 && res.header.Get("X-Faultline-Fault") != "" {
				t.Errorf("%s: pass-through response carries a fault header", tc.name)
			}
		}
		recs := p.recordsN(t, testRun, len(cases))
		if len(recs) != len(cases) {
			t.Fatalf("every request during a run is recorded, scoped or not: got %d records", len(recs))
		}
		for i, tc := range cases {
			if recs[i].Scoped != tc.scoped || (recs[i].FaultID != nil) != (tc.want == 503) {
				t.Errorf("%s: unexpected record %+v", tc.name, recs[i])
			}
		}
		if n := p.triggers(t, "f1"); n != 1 {
			t.Fatalf("triggers = %d, want 1", n)
		}
	})

	t.Run("scope all", func(t *testing.T) {
		p.install(t, testRun, "all", flt("f1", "http_error", "svc", obj{"status": 503}))
		for _, h := range []map[string]string{nil, scoped(testRun), scoped("run_bbbbbbbbbbbb")} {
			if res := call("GET", p.url("/svc/x"), h, ""); res.status != 503 {
				t.Fatalf("scope all must fault every request, got %d for %v", res.status, h)
			}
		}
	})

	t.Run("match and route", func(t *testing.T) {
		up2 := newUpstream(t)
		s := newStack(t, map[string]string{"a": up2.URL, "b": up2.URL})
		p2 := s.startProxy(t)
		p2.install(t, testRun, "header", flt("f1", "http_error", "a", obj{"status": 503}, match("POST", "/stock/*")))
		for _, tc := range []struct {
			method, path string
			want         int
		}{
			{"POST", "/a/stock/1", 503},
			{"POST", "/a/stock/1/detail?x=1", 503},
			{"GET", "/a/stock/1", 200},
			{"POST", "/a/price/1", 200},
			{"POST", "/a/stock", 200},
			{"POST", "/b/stock/1", 200},
		} {
			if res := call(tc.method, p2.url(tc.path), scoped(testRun), ""); res.status != tc.want {
				t.Errorf("%s %s: expected %d, got %d", tc.method, tc.path, tc.want, res.status)
			}
		}
	})

	t.Run("first matching fault wins, one fault per request", func(t *testing.T) {
		p.install(t, testRun, "header",
			flt("first", "http_error", "svc", obj{"status": 503}, strategy(obj{"kind": "burst", "count": 2})),
			flt("second", "http_error", "svc", obj{"status": 500}))
		if got := faultedPattern(t, p, 4); got != "FF.." {
			t.Fatalf("got %s", got)
		}
		if p.triggers(t, "first") != 2 || p.triggers(t, "second") != 0 {
			t.Fatalf("triggers: first=%d second=%d", p.triggers(t, "first"), p.triggers(t, "second"))
		}
	})

	t.Run("expose_fault_header false", func(t *testing.T) {
		p3, _ := startEchoProxy(t, func(s *stack) { s.extra = ""; s.leaseTimeout = "10s\n  expose_fault_header: false" })
		p3.install(t, testRun, "header", flt("f1", "http_error", "svc", obj{"status": 503}))
		res := call("GET", p3.url("/svc/x"), scoped(testRun), "")
		if res.status != 503 || res.header.Get("X-Faultline-Fault") != "" {
			t.Fatalf("status %d, fault header %q", res.status, res.header.Get("X-Faultline-Fault"))
		}
	})
}

func TestFaultWindowIsEnforcedByProxy(t *testing.T) {
	p, _ := startEchoProxy(t)
	p.install(t, testRun, "header", flt("w1", "http_error", "svc", obj{"status": 503}, window(700*time.Millisecond, 1500*time.Millisecond)))
	statusAt := func() int { return call("GET", p.url("/svc/x"), scoped(testRun), "").status }

	if got := statusAt(); got != 200 {
		t.Fatalf("before start_at: expected 200, got %d", got)
	}
	time.Sleep(900 * time.Millisecond)
	if got := statusAt(); got != 503 {
		t.Fatalf("inside the window: expected 503, got %d", got)
	}
	// No admin call is made from here on: the proxy alone must stop the fault.
	time.Sleep(800 * time.Millisecond)
	if got := statusAt(); got != 200 {
		t.Fatalf("after end_at: expected 200, got %d", got)
	}
	a := p.active(t)
	if a.RunID == nil || a.Faults[0].Triggers != 1 {
		t.Fatalf("unexpected active state: %+v", a)
	}
}

func TestLeaseExpiryClearsAllFaults(t *testing.T) {
	p, _ := startEchoProxy(t, func(s *stack) { s.leaseTimeout = "1s" })
	leaseID := "lease-" + testRun
	renew := func(id string) int {
		return p.adminCall(t, "POST", "/admin/v1/lease/renew", obj{"lease_id": id}, nil)
	}
	if code := renew(leaseID); code != 404 {
		t.Fatalf("renewing with no rule set: expected 404, got %d", code)
	}

	p.install(t, testRun, "header",
		flt("hold", "timeout", "svc", obj{"hold": "30s"}, match("GET", "/hold")),
		flt("f1", "http_error", "svc", obj{"status": 503}))
	if code := renew("someone-else"); code != 409 {
		t.Fatalf("renewing with the wrong lease id: expected 409, got %d", code)
	}

	// Renewing keeps the faults alive well past lease_timeout.
	for i := 0; i < 8; i++ {
		time.Sleep(300 * time.Millisecond)
		if code := renew(leaseID); code != 200 {
			t.Fatalf("renewal %d failed with %d", i, code)
		}
	}
	if res := call("GET", p.url("/svc/x"), scoped(testRun), ""); res.status != 503 {
		t.Fatalf("faults must survive while the lease is renewed, got %d", res.status)
	}

	// The admin client stops renewing. An in-flight hold must be released too.
	held := make(chan time.Duration, 1)
	go func() {
		data, _, took := raw(t, p.s.trafficAddr, rawGet("/svc/hold", testRun))
		if len(data) != 0 {
			t.Errorf("the held request got a response: %q", data)
		}
		held <- took
	}()
	stopped := time.Now()
	eventually(t, 3*time.Second, "faults cleared by lease expiry", func() bool {
		return call("GET", p.url("/svc/x"), scoped(testRun), "").status == 200
	})
	if elapsed := time.Since(stopped); elapsed < 800*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("faults should clear about 1s after the last renewal, took %s", elapsed)
	}
	select {
	case took := <-held:
		if took > 2500*time.Millisecond {
			t.Fatalf("hold released late: %s", took)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an in-flight hold was not released when the lease expired")
	}
	if a := p.active(t); a.RunID != nil || len(a.Faults) != 0 {
		t.Fatalf("expected no active faults, got %+v", a)
	}
	var rawStats obj
	p.adminCall(t, "GET", "/admin/v1/stats", nil, &rawStats)
	found := false
	for _, ev := range rawStats["events"].([]any) {
		e := ev.(obj)
		if e["type"] == "lease_expired" && e["run_id"] == testRun {
			found = true
		}
	}
	if !found {
		t.Fatalf("no lease_expired event recorded: %v", rawStats["events"])
	}
	if code := renew(leaseID); code != 404 {
		t.Fatalf("an expired lease cannot be renewed: expected 404, got %d", code)
	}
}

func TestKillClearsWithinOneSecond(t *testing.T) {
	p, _ := startEchoProxy(t)
	p.install(t, testRun, "header",
		flt("hold", "timeout", "svc", obj{"hold": "30s"}, match("GET", "/hold")),
		flt("lat", "latency", "svc", obj{"ms": 30000}, match("GET", "/slow")),
		flt("f1", "http_error", "svc", obj{"status": 503}))

	var wg sync.WaitGroup
	inflight := make([][]byte, 2)
	for i, path := range []string{"/svc/hold", "/svc/slow"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			inflight[i], _, _ = raw(t, p.s.trafficAddr, "GET "+path+" HTTP/1.1\r\nHost: t\r\nConnection: close\r\nX-Chaos-Run: "+testRun+"\r\n\r\n")
		}()
	}
	time.Sleep(300 * time.Millisecond)
	if res := call("GET", p.url("/svc/x"), scoped(testRun), ""); res.status != 503 {
		t.Fatalf("fault not active before the kill: %d", res.status)
	}

	killed := time.Now()
	p.clear(t)
	res := call("GET", p.url("/svc/x"), scoped(testRun), "")
	if res.status != 200 {
		t.Fatalf("a request right after the kill was still faulted: %d", res.status)
	}
	if since := time.Since(killed); since > time.Second {
		t.Fatalf("kill took %s to take effect", since)
	}
	wg.Wait()
	if since := time.Since(killed); since > time.Second {
		t.Fatalf("in-flight hold and delay were released %s after the kill", since)
	}
	if len(inflight[0]) != 0 {
		t.Fatalf("a released hold must close the connection with no response, got %q", inflight[0])
	}
	if !bytes.HasPrefix(inflight[1], []byte("HTTP/1.1 200")) {
		t.Fatalf("a released delay must forward the request, got %q", inflight[1])
	}
	if a := p.active(t); a.RunID != nil || len(a.Faults) != 0 {
		t.Fatalf("expected {run_id:null, faults:[]}, got %+v", a)
	}
	var cleared obj
	p.adminCall(t, "DELETE", "/admin/v1/faults", nil, &cleared)
	if cleared["cleared"] != false {
		t.Fatalf("clearing twice must be harmless: %v", cleared)
	}
}

func TestRecordsAndStats(t *testing.T) {
	p, _ := startEchoProxy(t)

	// Nothing is recorded outside a run.
	call("GET", p.url("/svc/before"), scoped(testRun), "")
	if recs, next := p.records(t, "", 0); len(recs) != 0 || next != 0 {
		t.Fatalf("requests outside a run must not be recorded: %+v", recs)
	}
	var idle obj
	p.adminCall(t, "GET", "/admin/v1/faults/active", nil, &idle)
	if v, ok := idle["run_id"]; !ok || v != nil || len(idle["faults"].([]any)) != 0 {
		t.Fatalf("expected {run_id:null, faults:[]}, got %v", idle)
	}

	const secret = "Bearer itest-secret-value-9f3a"
	p.install(t, testRun, "header", flt("f1", "http_error", "svc", obj{"status": 503, "retry_after": 1}, match("GET", "/fail/*")))
	call("GET", p.url("/svc/fail/1?token=query-secret"), map[string]string{"X-Chaos-Run": testRun, "X-Chaos-Correlation": "corr-a", "Authorization": secret}, "")
	call("POST", p.url("/svc/ok"), map[string]string{"X-Chaos-Run": testRun, "X-Chaos-Correlation": "corr-a", "Cookie": "session=" + secret}, "body-secret")
	call("GET", p.url("/svc/ok"), nil, "")

	p.recordsN(t, testRun, 3)
	var page struct {
		Records []obj
		Next    float64
	}
	p.adminCall(t, "GET", "/admin/v1/records?run_id="+testRun+"&since=0", nil, &page)
	if len(page.Records) != 3 || page.Next != 3 {
		t.Fatalf("expected 3 records and next=3, got %d and %v", len(page.Records), page.Next)
	}
	wantKeys := []string{"seq", "ts", "route", "method", "path", "status", "duration_ms", "scoped", "correlation", "fault_id", "fault_type", "retry_after", "client_held_ms"}
	for _, rec := range page.Records {
		if len(rec) != len(wantKeys) {
			t.Fatalf("record has unexpected fields: %v", rec)
		}
		for _, k := range wantKeys {
			if _, ok := rec[k]; !ok {
				t.Fatalf("record lacks %q: %v", k, rec)
			}
		}
	}
	first, second, third := page.Records[0], page.Records[1], page.Records[2]
	if first["seq"] != float64(1) || first["route"] != "svc" || first["method"] != "GET" || first["path"] != "/fail/1" ||
		first["status"] != float64(503) || first["scoped"] != true || first["correlation"] != "corr-a" ||
		first["fault_id"] != "f1" || first["fault_type"] != "http_error" || first["retry_after"] != float64(1) || first["client_held_ms"] != nil {
		t.Fatalf("unexpected faulted record: %v", first)
	}
	if _, err := time.Parse(time.RFC3339Nano, first["ts"].(string)); err != nil {
		t.Fatalf("ts is not RFC3339Nano: %v", first["ts"])
	}
	if second["status"] != float64(200) || second["fault_id"] != nil || second["fault_type"] != nil || second["retry_after"] != nil || second["method"] != "POST" {
		t.Fatalf("unexpected pass-through record: %v", second)
	}
	if third["scoped"] != false || third["correlation"] != "" {
		t.Fatalf("unexpected unscoped record: %v", third)
	}

	// Header values, query strings and bodies never reach the records.
	all, _ := json.Marshal(page)
	for _, s := range []string{"itest-secret-value", "query-secret", "body-secret"} {
		if bytes.Contains(all, []byte(s)) {
			t.Fatalf("records leak %q: %s", s, all)
		}
	}

	if recs, next := p.records(t, testRun, 2); len(recs) != 1 || recs[0].Seq != 3 || next != 3 {
		t.Fatalf("since=2 should return only seq 3: %+v next=%d", recs, next)
	}
	if recs, next := p.records(t, testRun, 3); len(recs) != 0 || next != 3 {
		t.Fatalf("since=3 should return nothing: %+v next=%d", recs, next)
	}
	if recs, _ := p.records(t, "run_bbbbbbbbbbbb", 0); len(recs) != 0 {
		t.Fatalf("records of another run id leaked: %+v", recs)
	}
	if code := p.adminCall(t, "GET", "/admin/v1/records?since=abc", nil, nil); code != 400 {
		t.Fatalf("invalid since: expected 400, got %d", code)
	}

	var stats struct {
		RunID  *string `json:"run_id"`
		Active bool
		Routes map[string]struct{ Requests, Faulted int64 }
		Faults map[string]int64
	}
	p.adminCall(t, "GET", "/admin/v1/stats", nil, &stats)
	if stats.Routes["svc"].Requests != 4 || stats.Routes["svc"].Faulted != 1 || stats.Faults["f1"] != 1 || !stats.Active || *stats.RunID != testRun {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	// Triggers of the last run stay readable after the faults are cleared.
	p.clear(t)
	p.adminCall(t, "GET", "/admin/v1/stats", nil, &stats)
	if stats.Active || stats.Faults["f1"] != 1 || *stats.RunID != testRun {
		t.Fatalf("stats after clear: %+v", stats)
	}
	if recs, _ := p.records(t, testRun, 0); len(recs) != 3 {
		t.Fatalf("records must stay readable after clear, got %d", len(recs))
	}
}

func TestRecordsPaging(t *testing.T) {
	p, _ := startEchoProxy(t)
	p.install(t, testRun, "all", flt("f1", "error_in_body", "svc", obj{"body": "x"}))
	const total = 5200
	var wg sync.WaitGroup
	c := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 16}}
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < total/16; i++ {
				resp, err := c.Get(p.url("/svc/x"))
				if err != nil {
					t.Error(err)
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	eventually(t, 3*time.Second, "all records stored", func() bool {
		_, n := p.records(t, testRun, 5000)
		return n == total
	})
	first, next := p.records(t, testRun, 0)
	if len(first) != 5000 || next != 5000 {
		t.Fatalf("a page holds at most 5000 records: got %d, next %d", len(first), next)
	}
	rest, next2 := p.records(t, testRun, next)
	if len(rest) != total-5000 || next2 != total {
		t.Fatalf("second page: got %d records, next %d", len(rest), next2)
	}
	for i, r := range append(first, rest...) {
		if r.Seq != int64(i+1) {
			t.Fatalf("seq must increase by one: record %d has seq %d", i, r.Seq)
		}
	}
}

func TestProxyStartupGuards(t *testing.T) {
	up := newUpstream(t)
	run := func(t *testing.T, env []string, mod func(*stack)) (int, string) {
		s := newStack(t, map[string]string{"svc": up.URL})
		if mod != nil {
			mod(s)
		}
		s.writeConfig(t)
		p := start(t, "faultline", env, "proxy", "--config", s.cfgPath)
		code := p.wait(10 * time.Second)
		return code, p.out.String()
	}
	token := []string{"FAULTLINE_ADMIN_TOKEN=" + adminToken}

	for _, env := range []string{"prod", "production", "live", "PROD", "Production", "LIVE"} {
		code, out := run(t, token, func(s *stack) { s.environment = env })
		if code != 2 || !strings.Contains(strings.ToLower(out), "production") {
			t.Errorf("environment %s: expected exit 2 with a clear message, got %d: %s", env, code, out)
		}
	}
	if code, out := run(t, []string{"FAULTLINE_ADMIN_TOKEN="}, nil); code != 2 || !strings.Contains(out, "FAULTLINE_ADMIN_TOKEN") {
		t.Errorf("missing token: expected exit 2 naming the variable, got %d: %s", code, out)
	}
	if code, out := run(t, []string{"FAULTLINE_ADMIN_TOKEN=short-token-123"}, nil); code != 2 {
		t.Errorf("15 character token: expected exit 2, got %d: %s", code, out)
	} else if strings.Contains(out, "short-token-123") {
		t.Errorf("the token value was printed: %s", out)
	}
	if code, _ := run(t, token, func(s *stack) { s.extra = "surprise: true\n" }); code != 2 {
		t.Errorf("unknown config key: expected exit 2, got %d", code)
	}
	p := start(t, "faultline", token, "proxy", "--config", "does-not-exist.yaml")
	if code := p.wait(10 * time.Second); code != 2 {
		t.Errorf("missing config: expected exit 2, got %d", code)
	}
}
