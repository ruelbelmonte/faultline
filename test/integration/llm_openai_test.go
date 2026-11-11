//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// The LLM faults in OpenAI chat completions format (PRD 8.5 stretch), against
// the OpenAI-compatible mock in faultline-demo, stream and non-stream.

const (
	chatPath   = "/v1/chat/completions"
	chatPlain  = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	chatStream = `{"stream":true,"model":"m","messages":[{"role":"user","content":"hi"}]}`
	chatTools  = `{"model":"m","tools":[{"type":"function","function":{"name":"search","parameters":{"type":"object"}}}],"messages":[]}`
	chatToolsS = `{"stream":true,"model":"m","tools":[{"type":"function","function":{"name":"search","parameters":{"type":"object"}}}],"messages":[]}`
)

type chatStack struct {
	p          *proxyProc
	demo       *demoProc
	text       string
	plainBody  []byte
	streamBody []byte
}

func startChat(t testing.TB) *chatStack {
	t.Helper()
	s := newStack(t, nil)
	d := startDemo(t, "naive", "http://"+s.trafficAddr)
	s.routes = map[string]string{"llm": "http://" + d.llm}
	c := &chatStack{p: s.startProxy(t), demo: d}
	res := c.post(chatPlain, nil)
	if res.err != nil || res.status != 200 {
		t.Fatalf("baseline non-stream call: %d %v", res.status, res.err)
	}
	c.plainBody = res.body
	c.text = parseChat(t, res.body).Choices[0].Message.Content
	if len(c.text) != 400 {
		t.Fatalf("baseline text has %d characters", len(c.text))
	}
	res = c.post(chatStream, nil)
	if res.err != nil || res.status != 200 {
		t.Fatalf("baseline stream call: %d %v", res.status, res.err)
	}
	c.streamBody = res.body
	return c
}

func (c *chatStack) post(body string, headers map[string]string) result {
	return call("POST", c.p.url("/llm"+chatPath), headers, body)
}

func (c *chatStack) faulted(body string) result { return c.post(body, scoped(testRun)) }

func (c *chatStack) install(t testing.TB, typ string, params obj) {
	t.Helper()
	c.p.install(t, testRun, "header", flt("llm1", typ, "llm", params, match("POST", chatPath)))
}

type chatCompletion struct {
	Object  string
	Choices []struct {
		Index   int
		Message struct {
			Role      string
			Content   string
			ToolCalls []struct {
				ID       string
				Type     string
				Function struct{ Name, Arguments string }
			} `json:"tool_calls"`
		}
		FinishReason string `json:"finish_reason"`
	}
}

func parseChat(t testing.TB, body []byte) chatCompletion {
	t.Helper()
	var c chatCompletion
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatalf("response is not valid JSON: %v\n%s", err, body)
	}
	if len(c.Choices) == 0 {
		t.Fatalf("response has no choices: %s", body)
	}
	return c
}

type chatChunk struct {
	Object  string
	Choices []struct {
		Index int
		Delta struct {
			Role      string
			Content   string
			ToolCalls []struct {
				Index    int
				Function struct{ Arguments string }
			} `json:"tool_calls"`
		}
		FinishReason *string `json:"finish_reason"`
	}
}

// parseChatStream splits a complete chat stream into chunks. It fails on
// anything malformed and reports whether the stream ended with [DONE].
func parseChatStream(t testing.TB, body []byte) (chunks []chatChunk, done bool) {
	t.Helper()
	for _, block := range strings.Split(strings.TrimRight(string(body), "\n"), "\n\n") {
		data, ok := strings.CutPrefix(block, "data: ")
		if !ok || strings.Contains(data, "\n") {
			t.Fatalf("malformed event: %q", block)
		}
		if done {
			t.Fatalf("event after [DONE]: %q", block)
		}
		if data == "[DONE]" {
			done = true
			continue
		}
		var c chatChunk
		if err := json.Unmarshal([]byte(data), &c); err != nil || c.Object != "chat.completion.chunk" || len(c.Choices) != 1 {
			t.Fatalf("malformed chunk: %q", block)
		}
		chunks = append(chunks, c)
	}
	return chunks, done
}

func chatStreamText(chunks []chatChunk) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString(c.Choices[0].Delta.Content)
	}
	return b.String()
}

func finishReason(chunks []chatChunk) string {
	if last := chunks[len(chunks)-1].Choices[0].FinishReason; last != nil {
		return *last
	}
	return "<null>"
}

func TestDemoChatCompletions(t *testing.T) {
	d := startDemo(t, "naive", "http://127.0.0.1:1")
	url := "http://" + d.llm + chatPath

	res := call("POST", url, nil, chatPlain)
	c := parseChat(t, res.body)
	if res.status != 200 || c.Object != "chat.completion" || c.Choices[0].Message.Role != "assistant" || len(c.Choices[0].Message.Content) != 400 || c.Choices[0].FinishReason != "stop" {
		t.Fatalf("non-stream: %d %+v", res.status, c)
	}

	resp, begin := postJSON(t, url, chatStream, nil)
	defer resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
	}
	first := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatal(err)
	}
	if at := time.Since(begin); at > 300*time.Millisecond {
		t.Fatalf("the first chunk must be flushed at once, arrived after %s", at)
	}
	rest, _ := io.ReadAll(resp.Body)
	if took := time.Since(begin); took < 900*time.Millisecond {
		t.Fatalf("20 deltas 50ms apart take about a second, took %s", took)
	}
	chunks, done := parseChatStream(t, append(first, rest...))
	if !done || len(chunks) != 22 || chatStreamText(chunks) != c.Choices[0].Message.Content || finishReason(chunks) != "stop" {
		t.Fatalf("stream: done %v, %d chunks, finish %s", done, len(chunks), finishReason(chunks))
	}

	tc := parseChat(t, call("POST", url, nil, chatTools).body).Choices[0]
	if tc.FinishReason != "tool_calls" || len(tc.Message.ToolCalls) != 1 || tc.Message.ToolCalls[0].Function.Name != "search" || tc.Message.ToolCalls[0].Function.Arguments != `{"query":"demo"}` {
		t.Fatalf("tool call: %+v", tc)
	}
	chunks, done = parseChatStream(t, call("POST", url, nil, chatToolsS).body)
	var args strings.Builder
	parts := 0
	for _, ch := range chunks {
		for _, call := range ch.Choices[0].Delta.ToolCalls {
			if call.Function.Arguments != "" {
				parts++
			}
			args.WriteString(call.Function.Arguments)
		}
	}
	if !done || args.String() != `{"query":"demo"}` || parts < 3 || finishReason(chunks) != "tool_calls" {
		t.Fatalf("tool stream: done %v, arguments %q in %d parts, finish %s", done, args.String(), parts, finishReason(chunks))
	}

	if res := call("POST", url, nil, "{not json"); res.status != 400 || !bytes.Contains(res.body, []byte("invalid_request_error")) {
		t.Fatalf("bad request: %d %s", res.status, res.body)
	}
}

func TestChatRateLimit(t *testing.T) {
	c := startChat(t)
	const want = `{"error":{"message":"faultline injected","type":"rate_limit_error","param":null,"code":"rate_limit_exceeded"}}`
	for _, tc := range []struct {
		params     obj
		retryAfter string
	}{{obj{}, "5"}, {obj{"retry_after": 12}, "12"}} {
		c.install(t, "llm_rate_limit", tc.params)
		for name, body := range map[string]string{"non-stream": chatPlain, "stream": chatStream} {
			res := c.faulted(body)
			if res.status != 429 || string(res.body) != want {
				t.Fatalf("%s: got %d %s", name, res.status, res.body)
			}
			if res.header.Get("retry-after") != tc.retryAfter || res.header.Get("X-Faultline-Fault") != "llm1" || !strings.HasPrefix(res.header.Get("Content-Type"), "application/json") {
				t.Fatalf("%s: headers %v", name, res.header)
			}
		}
	}
	recs := c.p.recordsN(t, testRun, 4)
	if r := recs[len(recs)-1]; r.Status != 429 || r.RetryAfter == nil || *r.RetryAfter != 12 || *r.FaultType != "llm_rate_limit" {
		t.Fatalf("record: %+v", r)
	}
	if hits := c.post(chatPlain, nil); hits.status != 200 {
		t.Fatalf("unscoped request must pass: %d", hits.status)
	}
}

func TestChatOverloaded(t *testing.T) {
	c := startChat(t)
	c.install(t, "llm_overloaded", obj{})
	for name, body := range map[string]string{"non-stream": chatPlain, "stream": chatStream} {
		res := c.faulted(body)
		if res.status != 503 || string(res.body) != `{"error":{"message":"faultline injected","type":"server_error","param":null,"code":"service_unavailable"}}` {
			t.Fatalf("%s: got %d %s", name, res.status, res.body)
		}
		if res.header.Get("X-Faultline-Fault") != "llm1" || res.header.Get("Retry-After") != "" {
			t.Fatalf("%s: headers %v", name, res.header)
		}
	}
}

// The same fault on the same route answers each API in its own format.
func TestLLMFaultFormatFollowsThePath(t *testing.T) {
	c := startChat(t)
	c.p.install(t, testRun, "header", flt("llm1", "llm_overloaded", "llm", obj{}))
	if res := c.faulted(chatPlain); res.status != 503 || !bytes.Contains(res.body, []byte(`"server_error"`)) {
		t.Fatalf("chat completions: %d %s", res.status, res.body)
	}
	if res := call("POST", c.p.url("/llm/v1/messages"), scoped(testRun), llmPlain); res.status != 529 || !bytes.Contains(res.body, []byte(`"overloaded_error"`)) {
		t.Fatalf("messages: %d %s", res.status, res.body)
	}
}

func TestChatStreamCut(t *testing.T) {
	c := startChat(t)
	for _, n := range []int{0, 1, 333, 1000} {
		c.install(t, "llm_stream_cut", obj{"after_bytes": n})
		res := c.faulted(chatStream)
		if res.status != 200 || res.header.Get("X-Faultline-Fault") != "llm1" {
			t.Fatalf("after_bytes %d: status %d headers %v", n, res.status, res.header)
		}
		if !bytes.Equal(res.body, c.streamBody[:n]) || res.err == nil {
			t.Fatalf("after_bytes %d: expected exactly the first %d bytes and a cut connection, got %d bytes, err %v", n, n, len(res.body), res.err)
		}
		if bytes.Contains(res.body, []byte("[DONE]")) {
			t.Fatalf("after_bytes %d: [DONE] reached the client", n)
		}
	}
	c.install(t, "llm_stream_cut", obj{"after_bytes": 120})
	res := c.faulted(chatPlain)
	var v any
	if res.status != 200 || !bytes.Equal(res.body, c.plainBody[:120]) || res.err == nil || json.Unmarshal(res.body, &v) == nil {
		t.Fatalf("non-stream: status %d, %d bytes, err %v", res.status, len(res.body), res.err)
	}
}

func TestChatSlowFirstToken(t *testing.T) {
	c := startChat(t)
	c.install(t, "llm_slow_first_token", obj{"delay_ms": 900})
	for name, tc := range map[string]struct {
		body string
		want []byte
	}{"stream": {chatStream, c.streamBody}, "non-stream": {chatPlain, c.plainBody}} {
		headers, first, _, data, resp := timedPost(t, c.p.url("/llm"+chatPath), tc.body)
		if headers > 400*time.Millisecond || resp.StatusCode != 200 || resp.Header.Get("X-Faultline-Fault") != "llm1" {
			t.Fatalf("%s: status and headers must be forwarded at once: %s, %d", name, headers, resp.StatusCode)
		}
		if first < 900*time.Millisecond || first > 1700*time.Millisecond {
			t.Fatalf("%s: first body byte expected after about 900ms, arrived after %s", name, first)
		}
		if !bytes.Equal(data, tc.want) {
			t.Fatalf("%s: the content must be untouched", name)
		}
	}
}

func TestChatTruncate(t *testing.T) {
	c := startChat(t)

	t.Run("non-stream", func(t *testing.T) {
		for _, tc := range []struct {
			params obj
			want   int
		}{{obj{}, 50}, {obj{"max_chars": 137}, 137}, {obj{"max_chars": 0}, 0}} {
			c.install(t, "llm_truncate", tc.params)
			res := c.faulted(chatPlain)
			if res.err != nil || res.status != 200 || res.header.Get("X-Faultline-Fault") != "llm1" {
				t.Fatalf("status %d err %v headers %v", res.status, res.err, res.header)
			}
			m := parseChat(t, res.body)
			if m.Choices[0].Message.Content != c.text[:tc.want] || m.Choices[0].FinishReason != "length" || m.Object != "chat.completion" || m.Choices[0].Message.Role != "assistant" {
				t.Fatalf("max_chars %d: %+v", tc.want, m)
			}
		}
	})

	t.Run("within the limit is untouched", func(t *testing.T) {
		c.install(t, "llm_truncate", obj{"max_chars": 400})
		if m := parseChat(t, c.faulted(chatPlain).body); m.Choices[0].FinishReason != "stop" || m.Choices[0].Message.Content != c.text {
			t.Fatalf("a completion within max_chars must not be altered: %+v", m)
		}
		if res := c.faulted(chatStream); res.err != nil || !bytes.Equal(res.body, c.streamBody) {
			t.Fatal("a stream within max_chars must pass through byte for byte")
		}
	})

	t.Run("stream", func(t *testing.T) {
		for _, tc := range []struct {
			params  obj
			want    int
			content int // chunks that carry text
		}{{obj{}, 50, 3}, {obj{"max_chars": 40}, 40, 2}, {obj{"max_chars": 137}, 137, 7}, {obj{"max_chars": 0}, 0, 0}} {
			c.install(t, "llm_truncate", tc.params)
			res := c.faulted(chatStream)
			if res.err != nil || res.status != 200 {
				t.Fatalf("max_chars %d: the truncated stream must end cleanly: %d %v", tc.want, res.status, res.err)
			}
			chunks, done := parseChatStream(t, res.body)
			if !done {
				t.Fatalf("max_chars %d: the stream must end with [DONE]", tc.want)
			}
			if got := chatStreamText(chunks); got != c.text[:tc.want] {
				t.Fatalf("max_chars %d: stream carried %d characters", tc.want, len(got))
			}
			withText := 0
			for _, ch := range chunks {
				if ch.Choices[0].Delta.Content != "" {
					withText++
				}
			}
			// The role chunk, the text chunks, one finishing chunk.
			if withText != tc.content || len(chunks) != tc.content+2 || finishReason(chunks) != "length" {
				t.Fatalf("max_chars %d: %d chunks, %d with text, finish %s", tc.want, len(chunks), withText, finishReason(chunks))
			}
			for _, ch := range chunks[:len(chunks)-1] {
				if ch.Choices[0].FinishReason != nil {
					t.Fatalf("max_chars %d: only the last chunk may carry a finish_reason", tc.want)
				}
			}
			if tc.want <= 50 && res.took > 700*time.Millisecond {
				t.Fatalf("max_chars %d: stream should end early, took %s", tc.want, res.took)
			}
		}
	})

	t.Run("stream stays unbuffered", func(t *testing.T) {
		c.install(t, "llm_truncate", obj{"max_chars": 390})
		_, first, done, _, _ := timedPost(t, c.p.url("/llm"+chatPath), chatStream)
		if first > 300*time.Millisecond || done < 900*time.Millisecond {
			t.Fatalf("rewritten chunks must be flushed as they arrive: first byte %s, last %s", first, done)
		}
	})
}

func TestChatMalformedToolCall(t *testing.T) {
	c := startChat(t)
	c.install(t, "llm_malformed_tool_call", obj{})

	t.Run("non-stream", func(t *testing.T) {
		res := c.faulted(chatTools)
		if res.err != nil || res.status != 200 || res.header.Get("X-Faultline-Fault") != "llm1" {
			t.Fatalf("status %d err %v", res.status, res.err)
		}
		m := parseChat(t, res.body).Choices[0]
		if len(m.Message.ToolCalls) != 1 || m.Message.ToolCalls[0].Function.Name != "search" || m.FinishReason != "tool_calls" {
			t.Fatalf("unexpected message: %+v", m)
		}
		args := m.Message.ToolCalls[0].Function.Arguments
		var v any
		if args != `{"query": "de` || json.Unmarshal([]byte(args), &v) == nil {
			t.Fatalf("arguments = %q", args)
		}
	})

	t.Run("stream", func(t *testing.T) {
		res := c.faulted(chatToolsS)
		if res.err != nil || res.status != 200 {
			t.Fatalf("status %d err %v", res.status, res.err)
		}
		chunks, done := parseChatStream(t, res.body)
		var args strings.Builder
		for _, ch := range chunks {
			for _, call := range ch.Choices[0].Delta.ToolCalls {
				args.WriteString(call.Function.Arguments)
			}
		}
		var v any
		if args.Len() == 0 || json.Unmarshal([]byte(args.String()), &v) == nil {
			t.Fatalf("concatenated arguments must not parse as JSON: %q", args.String())
		}
		if !done || finishReason(chunks) != "tool_calls" {
			t.Fatalf("the stream must otherwise stay well formed: done %v, finish %s", done, finishReason(chunks))
		}
	})

	t.Run("responses without tool calls are untouched", func(t *testing.T) {
		if res := c.faulted(chatPlain); !bytes.Equal(res.body, c.plainBody) {
			t.Fatal("a text response must not be altered")
		}
		if res := c.faulted(chatStream); !bytes.Equal(res.body, c.streamBody) {
			t.Fatal("a text stream must pass through byte for byte")
		}
	})

	t.Run("unscoped", func(t *testing.T) {
		m := parseChat(t, c.post(chatTools, nil).body).Choices[0]
		if m.Message.ToolCalls[0].Function.Arguments != `{"query":"demo"}` {
			t.Fatalf("unscoped tool call was altered: %+v", m.Message.ToolCalls)
		}
	})
}

// A chat completions fault installed by a real run.
func TestChatFaultsThroughControl(t *testing.T) {
	f := startFull(t, "naive")
	yaml := f.experiment(t, expOpts{
		Name: "chat-overloaded", Duration: "2s", BaseURL: "http://" + f.s.trafficAddr,
		Requests:   "    - {method: POST, path: /llm" + chatPath + ", headers: {Content-Type: application/json}, body: '" + chatPlain + "'}",
		Hypothesis: "    error_rate_max: 0.05",
		Faults:     "  - id: f1\n    type: llm_overloaded\n    route: llm\n    match: {method: POST, path: " + chatPath + "}",
	})
	doc := f.control.waitDone(t, f.control.createRun(t, yaml), 30*time.Second)
	if doc.verdict() != "failed" || doc.Faults[0].Triggers == 0 || doc.Metrics.Proxy["llm"].Faulted == 0 {
		t.Fatalf("503s are errors: got %s (%s), %+v", doc.verdict(), doc.reason(), doc.Faults)
	}
}
