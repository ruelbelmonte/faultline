//go:build integration

package integration

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubUpstream stands where the proxy would be and records what the demo sends.
type stubUpstream struct {
	*httptest.Server
	mu      sync.Mutex
	hits    []time.Time
	headers []http.Header
	paths   []string
}

func newStub(t *testing.T, handler func(hit int, w http.ResponseWriter, r *http.Request)) *stubUpstream {
	s := &stubUpstream{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits = append(s.hits, time.Now())
		s.headers = append(s.headers, r.Header.Clone())
		s.paths = append(s.paths, r.URL.Path)
		n := len(s.hits)
		s.mu.Unlock()
		handler(n, w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *stubUpstream) snapshot() ([]time.Time, []http.Header, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.hits...), append([]http.Header(nil), s.headers...), append([]string(nil), s.paths...)
}

func TestDemoHealthEndpoints(t *testing.T) {
	d := startDemo(t, "naive", "http://127.0.0.1:1")
	for _, a := range []string{d.shop, d.inventory, d.pricing, d.llm} {
		resp, body := get(t, "http://"+a+"/health", nil)
		if resp.StatusCode != 200 || strings.TrimSpace(string(body)) != `{"ok":true}` {
			t.Fatalf("%s/health: %d %s", a, resp.StatusCode, body)
		}
	}
}

func TestDemoRejectsUnknownMode(t *testing.T) {
	p := start(t, "faultline-demo", nil, "--mode", "careless")
	if code := p.wait(10 * time.Second); code != 2 {
		t.Fatalf("expected exit 2, got %d", code)
	}
}

// The chain shop -> inventory -> pricing goes through a plain pass-through
// router here because this test covers the demo alone. The same chain through
// the real proxy is covered in proxy_test.go.
func TestDemoChainAndHeaderForwarding(t *testing.T) {
	var d *demoProc
	router := newStub(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		route, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		target := map[string]string{"inventory": d.inventory, "pricing": d.pricing}[route]
		if target == "" {
			http.NotFound(w, r)
			return
		}
		rp := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: target})
		r.URL.Path = "/" + rest
		rp.ServeHTTP(w, r)
	})
	d = startDemo(t, "naive", router.URL)

	resp, body := get(t, "http://"+d.shop+"/api/orders/7", map[string]string{
		"X-Chaos-Run":         "run_abcdefghijkl",
		"X-Chaos-Correlation": "corr-123",
		"traceparent":         "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		"X-Unrelated":         "not-forwarded",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var got struct {
		ID    string
		Stock int
		Price float64
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "7" || got.Stock != 42 || got.Price != 9.99 {
		t.Fatalf("unexpected order: %s", body)
	}

	_, headers, paths := router.snapshot()
	if len(paths) != 2 || paths[0] != "/inventory/stock/7" || paths[1] != "/pricing/price/7" {
		t.Fatalf("unexpected downstream calls: %v", paths)
	}
	for i, h := range headers {
		if h.Get("X-Chaos-Run") != "run_abcdefghijkl" || h.Get("X-Chaos-Correlation") != "corr-123" ||
			h.Get("traceparent") != "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01" {
			t.Errorf("hop %d (%s): scope, correlation or traceparent not forwarded: %v", i, paths[i], h)
		}
		if h.Get("X-Unrelated") != "" {
			t.Errorf("hop %d: unrelated header was forwarded", i)
		}
	}
}

func TestDemoEscapesIDInDownstreamPath(t *testing.T) {
	stub := newStub(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"x","stock":42,"price":9.99}`))
	})
	d := startDemo(t, "naive", stub.URL)
	get(t, "http://"+d.shop+"/api/orders/..%2F..%2Fadmin%3Fx=1", nil)
	_, _, paths := stub.snapshot()
	if len(paths) != 1 || paths[0] != "/inventory/stock/../../admin?x=1" {
		t.Fatalf("the id must stay one escaped path segment, upstream saw %v", paths)
	}
}

func TestDemoCustomHeaderNames(t *testing.T) {
	stub := newStub(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"1","stock":42,"price":9.99}`))
	})
	d := &demoProc{shop: freeAddr(t), inventory: freeAddr(t), pricing: freeAddr(t), llm: freeAddr(t)}
	d.proc = start(t, "faultline-demo", nil, "--proxy", stub.URL,
		"--shop", d.shop, "--inventory", d.inventory, "--pricing", d.pricing, "--llm", d.llm,
		"--scope-header", "X-Test-Scope", "--correlation-header", "X-Test-Corr")
	waitHTTP(t, "http://"+d.shop+"/health")
	get(t, "http://"+d.shop+"/api/orders/1", map[string]string{"X-Test-Scope": "s", "X-Test-Corr": "c", "X-Chaos-Run": "old"})
	_, headers, _ := stub.snapshot()
	if len(headers) != 1 || headers[0].Get("X-Test-Scope") != "s" || headers[0].Get("X-Test-Corr") != "c" || headers[0].Get("X-Chaos-Run") != "" {
		t.Fatalf("custom header names not honoured: %v", headers)
	}
}

func TestDemoRetryCounts(t *testing.T) {
	always503 := func(_ int, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	t.Run("naive: 4 attempts, no backoff", func(t *testing.T) {
		stub := newStub(t, always503)
		d := startDemo(t, "naive", stub.URL)
		resp, body := get(t, "http://"+d.shop+"/api/orders/1", nil)
		if resp.StatusCode != 502 || strings.TrimSpace(string(body)) != `{"error":"upstream"}` {
			t.Fatalf("expected 502 upstream, got %d %s", resp.StatusCode, body)
		}
		hits, _, _ := stub.snapshot()
		if len(hits) != 4 {
			t.Fatalf("naive mode must make 4 attempts, made %d", len(hits))
		}
		if span := hits[3].Sub(hits[0]); span > 150*time.Millisecond {
			t.Fatalf("naive mode must retry immediately, attempts spanned %s", span)
		}
	})

	t.Run("good: 2 attempts, 200ms backoff", func(t *testing.T) {
		stub := newStub(t, always503)
		d := startDemo(t, "good", stub.URL)
		resp, _ := get(t, "http://"+d.shop+"/api/orders/1", nil)
		if resp.StatusCode != 502 {
			t.Fatalf("expected 502, got %d", resp.StatusCode)
		}
		hits, _, _ := stub.snapshot()
		if len(hits) != 2 {
			t.Fatalf("good mode must make 2 attempts, made %d", len(hits))
		}
		if gap := hits[1].Sub(hits[0]); gap < 190*time.Millisecond || gap > 600*time.Millisecond {
			t.Fatalf("good mode must back off about 200ms, gap was %s", gap)
		}
	})

	t.Run("naive ignores Retry-After", func(t *testing.T) {
		stub := newStub(t, func(_ int, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		d := startDemo(t, "naive", stub.URL)
		get(t, "http://"+d.shop+"/api/orders/1", nil)
		hits, _, _ := stub.snapshot()
		if len(hits) != 4 || hits[3].Sub(hits[0]) > 150*time.Millisecond {
			t.Fatalf("naive mode must ignore Retry-After: %d attempts over %s", len(hits), hits[len(hits)-1].Sub(hits[0]))
		}
	})

	t.Run("good honours Retry-After, capped at 2s", func(t *testing.T) {
		for _, tc := range []struct {
			retryAfter string
			min, max   time.Duration
		}{
			{"1", 990 * time.Millisecond, 1500 * time.Millisecond},
			{"30", 1990 * time.Millisecond, 2500 * time.Millisecond},
		} {
			stub := newStub(t, func(_ int, w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", tc.retryAfter)
				w.WriteHeader(http.StatusServiceUnavailable)
			})
			d := startDemo(t, "good", stub.URL)
			get(t, "http://"+d.shop+"/api/orders/1", nil)
			hits, _, _ := stub.snapshot()
			if len(hits) != 2 {
				t.Fatalf("Retry-After %s: expected 2 attempts, got %d", tc.retryAfter, len(hits))
			}
			if gap := hits[1].Sub(hits[0]); gap < tc.min || gap > tc.max {
				t.Fatalf("Retry-After %s: gap %s outside [%s, %s]", tc.retryAfter, gap, tc.min, tc.max)
			}
		}
	})

	t.Run("good: 1s timeout per attempt", func(t *testing.T) {
		stub := newStub(t, func(_ int, w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		})
		d := startDemo(t, "good", stub.URL)
		begin := time.Now()
		resp, _ := get(t, "http://"+d.shop+"/api/orders/1", nil)
		elapsed := time.Since(begin)
		hits, _, _ := stub.snapshot()
		if resp.StatusCode != 502 || len(hits) != 2 {
			t.Fatalf("expected 502 after 2 attempts, got %d after %d", resp.StatusCode, len(hits))
		}
		if elapsed < 2100*time.Millisecond || elapsed > 3200*time.Millisecond {
			t.Fatalf("expected about 2.2s (1s + 200ms + 1s), took %s", elapsed)
		}
	})

	t.Run("naive: no client timeout", func(t *testing.T) {
		stub := newStub(t, func(_ int, w http.ResponseWriter, r *http.Request) {
			time.Sleep(2500 * time.Millisecond)
			w.Write([]byte(`{"id":"1","stock":42,"price":9.99}`))
		})
		d := startDemo(t, "naive", stub.URL)
		resp, _ := get(t, "http://"+d.shop+"/api/orders/1", nil)
		hits, _, _ := stub.snapshot()
		if resp.StatusCode != 200 || len(hits) != 1 {
			t.Fatalf("naive mode must wait for a slow upstream: status %d after %d attempts", resp.StatusCode, len(hits))
		}
	})

	t.Run("retries on transport error", func(t *testing.T) {
		stub := newStub(t, func(_ int, w http.ResponseWriter, r *http.Request) {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		})
		d := startDemo(t, "naive", stub.URL)
		resp, _ := get(t, "http://"+d.shop+"/api/orders/1", nil)
		hits, _, _ := stub.snapshot()
		if resp.StatusCode != 502 || len(hits) != 4 {
			t.Fatalf("expected 502 after 4 attempts, got %d after %d", resp.StatusCode, len(hits))
		}
	})

	t.Run("recovers when a retry succeeds", func(t *testing.T) {
		stub := newStub(t, func(hit int, w http.ResponseWriter, r *http.Request) {
			if hit == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Write([]byte(`{"id":"1","stock":42,"price":9.99}`))
		})
		d := startDemo(t, "good", stub.URL)
		resp, _ := get(t, "http://"+d.shop+"/api/orders/1", nil)
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200 after one retry, got %d", resp.StatusCode)
		}
	})
}

type sseEvent struct {
	name string
	data map[string]any
	at   time.Duration
}

// readSSE reads a whole event stream, noting when each event arrived.
func readSSE(t testing.TB, resp *http.Response, begin time.Time) ([]sseEvent, error) {
	t.Helper()
	var events []sseEvent
	var cur sseEvent
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.data); err != nil {
				t.Fatalf("bad SSE data %q: %v", line, err)
			}
		case line == "":
			cur.at = time.Since(begin)
			events = append(events, cur)
			cur = sseEvent{}
		}
	}
	return events, sc.Err()
}

func postJSON(t testing.TB, url, body string, headers map[string]string) (*http.Response, time.Time) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	begin := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp, begin
}

func eventNames(events []sseEvent) []string {
	names := make([]string, len(events))
	for i, e := range events {
		names[i] = e.name
	}
	return names
}

func TestDemoLLMNonStream(t *testing.T) {
	d := startDemo(t, "naive", "http://127.0.0.1:1")
	resp, _ := postJSON(t, "http://"+d.llm+"/v1/messages", `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, nil)
	var msg struct {
		Type       string
		Role       string
		StopReason string `json:"stop_reason"`
		Content    []struct{ Type, Text string }
	}
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || msg.Type != "message" || msg.Role != "assistant" || msg.StopReason != "end_turn" {
		t.Fatalf("unexpected message: %d %+v", resp.StatusCode, msg)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "text" || len(msg.Content[0].Text) != 400 {
		t.Fatalf("expected one text block of 400 characters, got %+v", msg.Content)
	}
}

func TestDemoLLMStream(t *testing.T) {
	d := startDemo(t, "naive", "http://127.0.0.1:1")
	resp, begin := postJSON(t, "http://"+d.llm+"/v1/messages", `{"model":"m","stream":true,"messages":[]}`, nil)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q", ct)
	}
	events, err := readSSE(t, resp, begin)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"message_start", "content_block_start"}
	for i := 0; i < 20; i++ {
		want = append(want, "content_block_delta")
	}
	want = append(want, "content_block_stop", "message_delta", "message_stop")
	if got := eventNames(events); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence:\n got %v\nwant %v", got, want)
	}
	var text strings.Builder
	for _, e := range events[2:22] {
		chunk := e.data["delta"].(map[string]any)["text"].(string)
		if len(chunk) != 20 {
			t.Fatalf("delta of %d characters, want 20", len(chunk))
		}
		text.WriteString(chunk)
	}
	if text.Len() != 400 {
		t.Fatalf("stream carried %d characters, want 400", text.Len())
	}
	if sr := events[23].data["delta"].(map[string]any)["stop_reason"]; sr != "end_turn" {
		t.Fatalf("stop_reason %v", sr)
	}
	if events[0].at > 300*time.Millisecond {
		t.Fatalf("message_start must be flushed immediately, arrived after %s", events[0].at)
	}
	if total := events[len(events)-1].at; total < 950*time.Millisecond {
		t.Fatalf("20 deltas 50ms apart must take about 1s, took %s", total)
	}
}

func TestDemoLLMToolUse(t *testing.T) {
	d := startDemo(t, "naive", "http://127.0.0.1:1")
	body := `{"model":"m","tools":[{"name":"search","input_schema":{}},{"name":"other"}],"messages":[]}`

	resp, _ := postJSON(t, "http://"+d.llm+"/v1/messages", body, nil)
	var msg struct {
		Content []struct {
			Type  string
			Name  string
			Input map[string]string
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatal(err)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "tool_use" || msg.Content[0].Name != "search" || msg.Content[0].Input["query"] != "demo" {
		t.Fatalf("unexpected tool_use block: %+v", msg.Content)
	}

	resp, begin := postJSON(t, "http://"+d.llm+"/v1/messages", strings.Replace(body, `{"model"`, `{"stream":true,"model"`, 1), nil)
	events, err := readSSE(t, resp, begin)
	if err != nil {
		t.Fatal(err)
	}
	var partial strings.Builder
	chunks := 0
	for _, e := range events {
		if e.name != "content_block_delta" {
			continue
		}
		delta := e.data["delta"].(map[string]any)
		if delta["type"] != "input_json_delta" {
			t.Fatalf("unexpected delta type %v", delta["type"])
		}
		partial.WriteString(delta["partial_json"].(string))
		chunks++
	}
	if chunks < 3 || partial.String() != `{"query":"demo"}` {
		t.Fatalf("expected >= 3 chunks forming the tool input, got %d: %s", chunks, partial.String())
	}
	block := events[1].data["content_block"].(map[string]any)
	if block["type"] != "tool_use" || block["name"] != "search" {
		t.Fatalf("unexpected content_block_start: %v", block)
	}
}
