//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	llmPlain  = `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	llmStream = `{"stream":true,"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	llmTools  = `{"model":"m","tools":[{"name":"search","input_schema":{"type":"object"}}],"messages":[]}`
	llmToolsS = `{"stream":true,"model":"m","tools":[{"name":"search","input_schema":{"type":"object"}}],"messages":[]}`
)

type llmStack struct {
	p    *proxyProc
	demo *demoProc
	// text is the 400 character completion of the mock, read through the proxy with no fault.
	text       string
	plainBody  []byte
	streamBody []byte
}

func startLLM(t testing.TB) *llmStack {
	t.Helper()
	s := newStack(t, nil)
	d := startDemo(t, "naive", "http://"+s.trafficAddr)
	s.routes = map[string]string{"llm": "http://" + d.llm}
	l := &llmStack{p: s.startProxy(t), demo: d}

	res := l.post(llmPlain, nil)
	if res.err != nil || res.status != 200 {
		t.Fatalf("baseline non-stream call: %d %v", res.status, res.err)
	}
	l.plainBody = res.body
	l.text = messageText(t, res.body)
	if len(l.text) != 400 {
		t.Fatalf("baseline text has %d characters", len(l.text))
	}
	res = l.post(llmStream, nil)
	if res.err != nil || res.status != 200 {
		t.Fatalf("baseline stream call: %d %v", res.status, res.err)
	}
	l.streamBody = res.body
	return l
}

func (l *llmStack) post(body string, headers map[string]string) result {
	return call("POST", l.p.url("/llm/v1/messages"), headers, body)
}

func (l *llmStack) faulted(body string) result {
	return l.post(body, scoped(testRun))
}

func (l *llmStack) install(t testing.TB, typ string, params obj) {
	t.Helper()
	l.p.install(t, testRun, "header", flt("llm1", typ, "llm", params, match("POST", "/v1/messages")))
}

type llmMessage struct {
	Type       string
	Role       string
	StopReason string `json:"stop_reason"`
	Content    []struct {
		Type  string
		Text  string
		Name  string
		Input json.RawMessage
	}
}

func parseMessage(t testing.TB, body []byte) llmMessage {
	t.Helper()
	var m llmMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("response is not valid JSON: %v\n%s", err, body)
	}
	return m
}

func messageText(t testing.TB, body []byte) string {
	t.Helper()
	var b strings.Builder
	for _, c := range parseMessage(t, body).Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

// parseSSE splits a complete stream body into events. It fails on anything malformed.
func parseSSE(t testing.TB, body []byte) []sseEvent {
	t.Helper()
	var events []sseEvent
	for _, block := range strings.Split(strings.TrimRight(string(body), "\n"), "\n\n") {
		var ev sseEvent
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev.data); err != nil {
					t.Fatalf("event with malformed data: %q", block)
				}
			default:
				t.Fatalf("unexpected line in stream: %q", line)
			}
		}
		if ev.name == "" || ev.data == nil || ev.data["type"] != ev.name {
			t.Fatalf("malformed event: %q", block)
		}
		events = append(events, ev)
	}
	return events
}

func streamText(events []sseEvent) string {
	var b strings.Builder
	for _, e := range events {
		if e.name != "content_block_delta" {
			continue
		}
		if s, ok := e.data["delta"].(map[string]any)["text"].(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}

func TestLLMRateLimit(t *testing.T) {
	l := startLLM(t)
	const want = `{"type":"error","error":{"type":"rate_limit_error","message":"faultline injected"}}`
	for _, tc := range []struct {
		params     obj
		retryAfter string
	}{{obj{}, "5"}, {obj{"retry_after": 12}, "12"}} {
		l.install(t, "llm_rate_limit", tc.params)
		for name, body := range map[string]string{"non-stream": llmPlain, "stream": llmStream} {
			res := l.faulted(body)
			if res.status != 429 || string(res.body) != want {
				t.Fatalf("%s: got %d %s", name, res.status, res.body)
			}
			if res.header.Get("retry-after") != tc.retryAfter || res.header.Get("X-Faultline-Fault") != "llm1" {
				t.Fatalf("%s: headers %v", name, res.header)
			}
			if ct := res.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("%s: content type %q", name, ct)
			}
		}
	}
	recs := l.p.recordsN(t, testRun, 2)
	if r := recs[len(recs)-1]; r.Status != 429 || r.RetryAfter == nil || *r.RetryAfter != 12 || *r.FaultType != "llm_rate_limit" {
		t.Fatalf("record: %+v", r)
	}
	if res := l.post(llmPlain, nil); res.status != 200 {
		t.Fatalf("unscoped request must pass: %d", res.status)
	}
}

func TestLLMOverloaded(t *testing.T) {
	l := startLLM(t)
	l.install(t, "llm_overloaded", obj{})
	for name, body := range map[string]string{"non-stream": llmPlain, "stream": llmStream} {
		res := l.faulted(body)
		if res.status != 529 || string(res.body) != `{"type":"error","error":{"type":"overloaded_error","message":"faultline injected"}}` {
			t.Fatalf("%s: got %d %s", name, res.status, res.body)
		}
		if res.header.Get("X-Faultline-Fault") != "llm1" || res.header.Get("Retry-After") != "" {
			t.Fatalf("%s: headers %v", name, res.header)
		}
	}
}

func TestLLMStreamCut(t *testing.T) {
	l := startLLM(t)

	t.Run("stream", func(t *testing.T) {
		for _, n := range []int{0, 1, 333, 1000} {
			l.install(t, "llm_stream_cut", obj{"after_bytes": n})
			res := l.faulted(llmStream)
			if res.status != 200 || res.header.Get("X-Faultline-Fault") != "llm1" {
				t.Fatalf("after_bytes %d: status %d headers %v", n, res.status, res.header)
			}
			if !bytes.Equal(res.body, l.streamBody[:n]) {
				t.Fatalf("after_bytes %d: expected exactly the first %d bytes of the stream, got %d bytes", n, n, len(res.body))
			}
			if res.err == nil {
				t.Fatalf("after_bytes %d: the connection must be cut, not ended cleanly", n)
			}
			if bytes.Contains(res.body, []byte("message_stop")) {
				t.Fatalf("after_bytes %d: message_stop reached the client", n)
			}
		}
		// The cut happens when the bytes have been sent, not when the upstream finishes.
		l.install(t, "llm_stream_cut", obj{"after_bytes": 400})
		if res := l.faulted(llmStream); res.took > 700*time.Millisecond {
			t.Fatalf("a cut early in a 1s stream took %s", res.took)
		}
	})

	t.Run("non-stream", func(t *testing.T) {
		l.install(t, "llm_stream_cut", obj{"after_bytes": 120})
		res := l.faulted(llmPlain)
		if res.status != 200 || !bytes.Equal(res.body, l.plainBody[:120]) || res.err == nil {
			t.Fatalf("status %d, %d bytes, err %v", res.status, len(res.body), res.err)
		}
		var v any
		if json.Unmarshal(res.body, &v) == nil {
			t.Fatal("a cut JSON body must not parse")
		}
	})

	t.Run("beyond the body", func(t *testing.T) {
		l.install(t, "llm_stream_cut", obj{"after_bytes": 1 << 20})
		res := l.faulted(llmStream)
		if res.err != nil || !bytes.Equal(res.body, l.streamBody) {
			t.Fatalf("a cut point past the end leaves the stream whole: err %v", res.err)
		}
	})
}

// timedPost reports when the response headers arrived and when the first and last body bytes did.
func timedPost(t testing.TB, url, body string) (headers, firstByte, done time.Duration, data []byte, resp *http.Response) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("X-Chaos-Run", testRun)
	begin := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	headers = time.Since(begin)
	buf := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("reading the first byte: %v", err)
	}
	firstByte = time.Since(begin)
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	return headers, firstByte, time.Since(begin), append(buf, rest...), resp
}

func TestLLMSlowFirstToken(t *testing.T) {
	l := startLLM(t)
	l.install(t, "llm_slow_first_token", obj{"delay_ms": 900})

	t.Run("stream", func(t *testing.T) {
		headers, first, done, data, resp := timedPost(t, l.p.url("/llm/v1/messages"), llmStream)
		if headers > 400*time.Millisecond {
			t.Fatalf("status and headers must be forwarded immediately, took %s", headers)
		}
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") || resp.Header.Get("X-Faultline-Fault") != "llm1" {
			t.Fatalf("status %d headers %v", resp.StatusCode, resp.Header)
		}
		if first < 900*time.Millisecond || first > 1700*time.Millisecond {
			t.Fatalf("first body byte expected after about 900ms, arrived after %s", first)
		}
		if !bytes.Equal(data, l.streamBody) {
			t.Fatal("the stream content must be untouched")
		}
		if done < first {
			t.Fatalf("timings out of order: %s %s", first, done)
		}
	})

	t.Run("non-stream", func(t *testing.T) {
		headers, first, _, data, _ := timedPost(t, l.p.url("/llm/v1/messages"), llmPlain)
		if headers > 400*time.Millisecond || first < 900*time.Millisecond {
			t.Fatalf("headers after %s, first byte after %s", headers, first)
		}
		if !bytes.Equal(data, l.plainBody) {
			t.Fatal("the body must be untouched")
		}
	})
}

func TestLLMTruncate(t *testing.T) {
	l := startLLM(t)

	t.Run("non-stream", func(t *testing.T) {
		for _, tc := range []struct {
			params obj
			want   int
		}{{obj{}, 50}, {obj{"max_chars": 137}, 137}, {obj{"max_chars": 0}, 0}} {
			l.install(t, "llm_truncate", tc.params)
			res := l.faulted(llmPlain)
			if res.err != nil || res.status != 200 || res.header.Get("X-Faultline-Fault") != "llm1" {
				t.Fatalf("status %d err %v headers %v", res.status, res.err, res.header)
			}
			m := parseMessage(t, res.body)
			if got := messageText(t, res.body); got != l.text[:tc.want] {
				t.Fatalf("max_chars %d: text has %d characters or is not a prefix of the original", tc.want, len(got))
			}
			if m.StopReason != "max_tokens" || m.Type != "message" || m.Role != "assistant" || len(m.Content) != 1 {
				t.Fatalf("max_chars %d: %+v", tc.want, m)
			}
		}
	})

	t.Run("non-stream within the limit is untouched", func(t *testing.T) {
		l.install(t, "llm_truncate", obj{"max_chars": 400})
		res := l.faulted(llmPlain)
		if m := parseMessage(t, res.body); m.StopReason != "end_turn" || messageText(t, res.body) != l.text {
			t.Fatalf("a completion within max_chars must not be altered: %+v", m)
		}
	})

	t.Run("stream", func(t *testing.T) {
		for _, tc := range []struct {
			params obj
			want   int
			deltas int
		}{{obj{}, 50, 3}, {obj{"max_chars": 40}, 40, 2}, {obj{"max_chars": 137}, 137, 7}, {obj{"max_chars": 0}, 0, 0}} {
			l.install(t, "llm_truncate", tc.params)
			res := l.faulted(llmStream)
			if res.err != nil || res.status != 200 {
				t.Fatalf("max_chars %d: the truncated stream must end cleanly: %d %v", tc.want, res.status, res.err)
			}
			events := parseSSE(t, res.body)
			if got := streamText(events); got != l.text[:tc.want] {
				t.Fatalf("max_chars %d: stream carried %d characters", tc.want, len(got))
			}
			names := eventNames(events)
			want := []string{"message_start", "content_block_start"}
			for i := 0; i < tc.deltas; i++ {
				want = append(want, "content_block_delta")
			}
			want = append(want, "content_block_stop", "message_delta", "message_stop")
			if strings.Join(names, ",") != strings.Join(want, ",") {
				t.Fatalf("max_chars %d: events\n got %v\nwant %v", tc.want, names, want)
			}
			md := events[len(events)-2].data
			if md["delta"].(map[string]any)["stop_reason"] != "max_tokens" {
				t.Fatalf("max_chars %d: message_delta %v", tc.want, md)
			}
			if events[len(events)-3].data["index"] != float64(0) {
				t.Fatalf("content_block_stop must close block 0: %v", events[len(events)-3].data)
			}
			// 50 characters arrive within the first three 50ms ticks; the stream must not run its full second.
			if tc.want <= 50 && res.took > 700*time.Millisecond {
				t.Fatalf("max_chars %d: stream should end early, took %s", tc.want, res.took)
			}
		}
	})

	t.Run("stream within the limit is untouched", func(t *testing.T) {
		l.install(t, "llm_truncate", obj{"max_chars": 400})
		res := l.faulted(llmStream)
		if res.err != nil || !bytes.Equal(res.body, l.streamBody) {
			t.Fatal("a stream within max_chars must pass through byte for byte")
		}
	})

	t.Run("stream stays unbuffered", func(t *testing.T) {
		l.install(t, "llm_truncate", obj{"max_chars": 390})
		req, _ := http.NewRequest("POST", l.p.url("/llm/v1/messages"), strings.NewReader(llmStream))
		req.Header.Set("X-Chaos-Run", testRun)
		begin := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		events, err := readSSE(t, resp, begin)
		if err != nil {
			t.Fatal(err)
		}
		if events[0].at > 300*time.Millisecond || events[len(events)-1].at < 900*time.Millisecond {
			t.Fatalf("rewritten events must be flushed as they arrive: first %s, last %s", events[0].at, events[len(events)-1].at)
		}
	})
}

func TestLLMMalformedToolCall(t *testing.T) {
	l := startLLM(t)
	l.install(t, "llm_malformed_tool_call", obj{})

	t.Run("non-stream", func(t *testing.T) {
		res := l.faulted(llmTools)
		if res.err != nil || res.status != 200 || res.header.Get("X-Faultline-Fault") != "llm1" {
			t.Fatalf("status %d err %v", res.status, res.err)
		}
		m := parseMessage(t, res.body)
		if len(m.Content) != 1 || m.Content[0].Type != "tool_use" || m.Content[0].Name != "search" {
			t.Fatalf("unexpected content: %+v", m.Content)
		}
		var input string
		if err := json.Unmarshal(m.Content[0].Input, &input); err != nil {
			t.Fatalf("input must be a JSON string, got %s", m.Content[0].Input)
		}
		if input != `{"query": "de` {
			t.Fatalf("input = %q", input)
		}
		var v any
		if json.Unmarshal([]byte(input), &v) == nil {
			t.Fatal("the replaced input must be invalid JSON")
		}
	})

	t.Run("stream", func(t *testing.T) {
		res := l.faulted(llmToolsS)
		if res.err != nil || res.status != 200 {
			t.Fatalf("status %d err %v", res.status, res.err)
		}
		events := parseSSE(t, res.body)
		var partial strings.Builder
		for _, e := range events {
			if e.name == "content_block_delta" {
				delta := e.data["delta"].(map[string]any)
				if delta["type"] != "input_json_delta" {
					t.Fatalf("unexpected delta: %v", delta)
				}
				partial.WriteString(delta["partial_json"].(string))
			}
		}
		var v any
		if partial.Len() == 0 || json.Unmarshal([]byte(partial.String()), &v) == nil {
			t.Fatalf("concatenated partial_json must not parse as JSON: %q", partial.String())
		}
		names := eventNames(events)
		if names[0] != "message_start" || names[len(names)-1] != "message_stop" {
			t.Fatalf("the stream must otherwise stay well formed: %v", names)
		}
	})

	t.Run("responses without tool use are untouched", func(t *testing.T) {
		if res := l.faulted(llmPlain); !bytes.Equal(res.body, l.plainBody) {
			t.Fatal("a text response must not be altered")
		}
		if res := l.faulted(llmStream); !bytes.Equal(res.body, l.streamBody) {
			t.Fatal("a text stream must pass through byte for byte")
		}
	})

	t.Run("unscoped", func(t *testing.T) {
		res := l.post(llmTools, nil)
		m := parseMessage(t, res.body)
		var input map[string]string
		if json.Unmarshal(m.Content[0].Input, &input) != nil || input["query"] != "demo" {
			t.Fatalf("unscoped tool call was altered: %s", m.Content[0].Input)
		}
	})
}

// LLM faults driven by a real run: control installs them, the driver calls the LLM route.
func TestLLMFaultsThroughControl(t *testing.T) {
	f := startFull(t, "naive")
	exp := func(name, fault, hypothesis string) string {
		return f.experiment(t, expOpts{
			Name: name, Duration: "2s", BaseURL: "http://" + f.s.trafficAddr,
			Requests:   "    - {method: POST, path: /llm/v1/messages, headers: {Content-Type: application/json}, body: '" + llmPlain + "'}",
			Hypothesis: hypothesis,
			Faults:     "  - id: f1\n    type: " + fault + "\n    route: llm\n    match: {method: POST, path: /v1/messages}",
		})
	}
	doc := f.control.waitDone(t, f.control.createRun(t, exp("overloaded", "llm_overloaded", "    error_rate_max: 0.05")), 30*time.Second)
	if doc.verdict() != "failed" || doc.Faults[0].Triggers == 0 || doc.Metrics.Proxy["llm"].Faulted == 0 {
		t.Fatalf("529s are errors: got %s (%s), %+v", doc.verdict(), doc.reason(), doc.Faults)
	}
	// 429 is not a 5xx, so the driver's error rate stays at zero.
	doc = f.control.waitDone(t, f.control.createRun(t, exp("rate-limit", "llm_rate_limit", "    error_rate_max: 0.05")), 30*time.Second)
	if doc.verdict() != "passed" || doc.Faults[0].Triggers == 0 || doc.Metrics.Driver.Errors != 0 {
		t.Fatalf("got %s (%s), %+v", doc.verdict(), doc.reason(), doc.Metrics.Driver)
	}
}

// traffic.command: the command is re-run until the run ends and gets the run
// id and scope header name in its environment.
func TestTrafficCommand(t *testing.T) {
	f := startFull(t, "naive")
	out := f.s.dir + "/command-output.txt"
	command := `echo "$FAULTLINE_RUN_ID $FAULTLINE_SCOPE_HEADER" >> "` + out + `"`
	if runtime.GOOS == "windows" {
		command = `echo %FAULTLINE_RUN_ID% %FAULTLINE_SCOPE_HEADER%>> "` + strings.ReplaceAll(out, "/", `\`) + `"`
	}
	yaml := `version: 1
name: command
target: {route: inventory}
duration: 2s
traffic:
  command: '` + command + `'
recovery: {timeout: 2s}
faults:
  - id: f1
    type: latency
    params: {ms: 1}
`
	id := f.control.createRun(t, yaml)
	f.control.waitStatus(t, id, 10*time.Second, "injecting")
	call("GET", f.proxy.url("/inventory/stock/1"), scoped(id), "")
	doc := f.control.waitDone(t, id, 30*time.Second)
	if doc.verdict() != "passed" || doc.Metrics.Driver.Requests != 0 {
		t.Fatalf("got %s (%s); a command produces no driver metrics: %+v", doc.verdict(), doc.reason(), doc.Metrics.Driver)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the command did not run: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 3 {
		t.Fatalf("the command should be re-run until the phase ends, ran %d times", len(lines))
	}
	for _, line := range lines {
		if strings.TrimSpace(line) != id+" X-Chaos-Run" {
			t.Fatalf("unexpected command environment: %q", line)
		}
	}
	// It stops with the run.
	before := len(lines)
	time.Sleep(700 * time.Millisecond)
	data, _ = os.ReadFile(out)
	if after := len(strings.Split(strings.TrimSpace(string(data)), "\n")); after != before {
		t.Fatalf("the command kept running after the run ended: %d then %d lines", before, after)
	}
}
