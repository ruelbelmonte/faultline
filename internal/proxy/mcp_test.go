package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"faultline/internal/fault"
)

func toolCallRequest(method, body string) *http.Request {
	r, _ := http.NewRequest(method, "http://proxy/llm/mcp", strings.NewReader(body))
	return r
}

func TestReadToolCall(t *testing.T) {
	cases := []struct {
		name, method, body string
		want               *mcpCall
	}{
		{"number id", "POST", `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"lookup_stock","arguments":{"id":"1"}}}`, &mcpCall{"7", "lookup_stock"}},
		{"string id, spaced", "POST", `{ "jsonrpc": "2.0", "id": "a b", "method": "tools/call", "params": { "name": "t" } }`, &mcpCall{`"a b"`, "t"}},
		{"other method", "POST", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil},
		{"notification", "POST", `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"t"}}`, nil},
		{"null id", "POST", `{"jsonrpc":"2.0","id":null,"method":"tools/call","params":{"name":"t"}}`, nil},
		{"object id", "POST", `{"jsonrpc":"2.0","id":{},"method":"tools/call","params":{"name":"t"}}`, nil},
		{"no tool name", "POST", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`, nil},
		{"tool name not a string", "POST", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":5}}`, nil},
		{"wrong version", "POST", `{"jsonrpc":"1.0","id":1,"method":"tools/call","params":{"name":"t"}}`, nil},
		{"batch", "POST", `[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t"}}]`, nil},
		{"not JSON", "POST", `tools/call`, nil},
		{"two values", "POST", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t"}} {}`, nil},
		{"GET", "GET", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t"}}`, nil},
		{"too large", "POST", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t","arguments":{"pad":"` + strings.Repeat("x", maxToolCallBody) + `"}}}`, nil},
	}
	for _, tc := range cases {
		r := toolCallRequest(tc.method, tc.body)
		got := readToolCall(r)
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
		// Whatever it was, the body is forwarded whole.
		if rest, _ := io.ReadAll(r.Body); string(rest) != tc.body {
			t.Errorf("%s: the body was not put back (%d of %d bytes)", tc.name, len(rest), len(tc.body))
		}
	}
	if readToolCall(&http.Request{Method: "POST", Body: http.NoBody}) != nil {
		t.Error("a request without a body is not a tool call")
	}
}

func TestMCPCallAnswers(t *testing.T) {
	call := &mcpCall{id: "7"}
	for body, want := range map[string]bool{
		`{"jsonrpc":"2.0","id":7,"result":{}}`:                           true,
		`{"jsonrpc":"2.0","id":7,"error":{"code":1,"message":"x"}}`:      true,
		`{"jsonrpc":"2.0","id":8,"result":{}}`:                           false,
		`{"jsonrpc":"2.0","id":"7","result":{}}`:                         false,
		`{"jsonrpc":"2.0","id":7,"method":"sampling/createMessage"}`:     false,
		`{"jsonrpc":"2.0","method":"notifications/message","params":{}}`: false,
	} {
		if got := call.answers(decodeObject([]byte(body))); got != want {
			t.Errorf("answers(%s) = %v, want %v", body, got, want)
		}
	}
	if call.answers(nil) {
		t.Error("no message answers nothing")
	}
	if (&mcpCall{id: `"7"`}).answers(decodeObject([]byte(`{"jsonrpc":"2.0","id":7,"result":{}}`))) {
		t.Error("a string id is not answered by a number id")
	}
}

func TestMCPReplacementMessages(t *testing.T) {
	call := &mcpCall{id: `"req-1"`}
	if decodeObject([]byte(call.malformed("invalid_json"))) != nil {
		t.Error("invalid_json must not parse")
	}
	schema := decodeObject([]byte(call.malformed("schema")))
	if schema == nil || !call.answers(schema) {
		t.Fatalf("schema mode must stay a valid response to the call: %s", call.malformed("schema"))
	}
	if _, isString := schema["result"].(map[string]any)["content"].(string); !isString {
		t.Error("schema mode must make content a string")
	}
	toolErr := decodeObject([]byte(call.toolError(`quote " and newline` + "\n")))
	if toolErr == nil || toolErr["result"].(map[string]any)["isError"] != true {
		t.Fatalf("tool error: %v", toolErr)
	}
	proto := decodeObject([]byte(call.protocolError(-32000, "busy")))
	if e := proto["error"].(map[string]any); e["code"].(interface{ String() string }).String() != "-32000" || e["message"] != "busy" {
		t.Fatalf("protocol error: %v", proto)
	}
}

func TestMCPMalformStreamRewritesOnlyTheAnswer(t *testing.T) {
	m := &mcpMalformStream{call: &mcpCall{id: "3"}, mode: "invalid_json"}
	note := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{}}\n\n"
	if out, done := m.event([]byte(note), eventData([]byte(note))); string(out) != note || done {
		t.Fatal("a notification passes through byte for byte")
	}
	other := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":4,\"result\":{}}\n\n"
	if out, _ := m.event([]byte(other), eventData([]byte(other))); string(out) != other {
		t.Fatal("the response to another call passes through")
	}
	answer := "event: message\r\nid: 42\r\ndata: {\"jsonrpc\":\"2.0\",\"id\":3,\"result\":{\"content\":[]}}\r\n\r\n"
	out, done := m.event([]byte(answer), eventData([]byte(answer)))
	want := "event: message\r\nid: 42\r\ndata: " + m.call.malformed("invalid_json") + "\n\n"
	if string(out) != want || done {
		t.Fatalf("got %q\nwant %q", out, want)
	}
	if out, _ := m.event([]byte(answer), eventData([]byte(answer))); string(out) != answer {
		t.Fatal("only the first answer is rewritten")
	}
}

// Being a tool call is part of an MCP rule's match: other requests are not
// counted and fall through to the next rule.
func TestDecideCallMatchesToolCallsOnly(t *testing.T) {
	end := time.Now().Add(time.Minute)
	e := NewEngine([]string{"llm"}, time.Minute, nil)
	mcpRule := fault.Install{Spec: fault.Spec{ID: "mcp", Type: fault.MCPToolError, Route: "llm",
		Strategy: fault.Strategy{Kind: fault.Burst, Count: 2}, Params: map[string]any{"tool": "lookup_stock"}}, EndAt: &end}
	fallback := fault.Install{Spec: fault.Spec{ID: "http", Type: fault.HTTPError, Route: "llm",
		Strategy: fault.Strategy{Kind: fault.Always}, Params: map[string]any{"status": 503}}, EndAt: &end}
	if errs := e.Install(fault.RuleSet{RunID: "run", ScopeMode: "header", LeaseID: "l", Faults: []fault.Install{mcpRule, fallback}}); len(errs) > 0 {
		t.Fatal(errs)
	}
	// The faults start when they are installed.
	now := time.Now()

	asked := 0
	decide := func(call *mcpCall) string {
		d := e.DecideCall("llm", "POST", "/mcp", "run", now, func() *mcpCall { asked++; return call })
		if d.rule == nil {
			return ""
		}
		if (d.mcp != nil) != fault.IsMCP(d.rule.spec.Type) {
			t.Fatalf("decision carries a tool call for %s: %v", d.rule.spec.Type, d.mcp)
		}
		return d.rule.spec.ID
	}
	stock, report := &mcpCall{id: "1", tool: "lookup_stock"}, &mcpCall{id: "2", tool: "slow_report"}
	for i, tc := range []struct {
		call *mcpCall
		want string
	}{
		{nil, "http"},    // not a tool call: falls through, not counted
		{report, "http"}, // another tool: falls through, not counted
		{stock, "mcp"},   // match 1
		{nil, "http"},
		{stock, "mcp"}, // match 2
		{stock, ""},    // match 3: the burst is over, and the first match wins without firing
	} {
		if got := decide(tc.call); got != tc.want {
			t.Fatalf("request %d: fault %q, want %q", i+1, got, tc.want)
		}
	}
	if asked != 6 {
		t.Fatalf("the body was inspected %d times for 6 requests", asked)
	}

	// Out of scope, the body is never inspected.
	if d := e.DecideCall("llm", "POST", "/mcp", "other-run", now, func() *mcpCall { asked++; return stock }); d.rule != nil || asked != 6 {
		t.Fatalf("an unscoped request must not be inspected or faulted: %+v, asked %d", d.rule, asked)
	}
	// Decide, which cannot see a body, never matches an MCP rule.
	if d := e.Decide("llm", "POST", "/mcp", "run", now); d.rule == nil || d.rule.spec.ID != "http" {
		t.Fatalf("Decide must skip MCP rules: %+v", d.rule)
	}
}

func TestMCPBodyKinds(t *testing.T) {
	resp := func(status int, contentType, encoding string) *http.Response {
		h := http.Header{}
		h.Set("Content-Type", contentType)
		if encoding != "" {
			h.Set("Content-Encoding", encoding)
		}
		return &http.Response{StatusCode: status, Header: h}
	}
	for _, tc := range []struct {
		resp *http.Response
		want string
	}{
		{resp(200, "application/json", ""), "json"},
		{resp(200, "application/json; charset=utf-8", ""), "json"},
		{resp(200, "text/event-stream", ""), "sse"},
		{resp(200, "Text/Event-Stream; charset=utf-8", ""), "sse"},
		{resp(202, "application/json", ""), ""},
		{resp(500, "application/json", ""), ""},
		{resp(200, "application/json", "gzip"), ""},
		{resp(200, "text/plain", ""), ""},
	} {
		if got := mcpBody(tc.resp); got != tc.want {
			t.Errorf("mcpBody(%d, %q, %q) = %q, want %q", tc.resp.StatusCode, tc.resp.Header.Get("Content-Type"), tc.resp.Header.Get("Content-Encoding"), got, tc.want)
		}
	}
}
