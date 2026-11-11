//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// M8: MCP tool faults over Streamable HTTP, against the MCP server in
// faultline-demo. The contract is docs/mcp-faults.md.

func rpc(id any, method string, params any) string {
	msg := obj{"jsonrpc": "2.0", "method": method}
	if id != nil {
		msg["id"] = id
	}
	if params != nil {
		msg["params"] = params
	}
	b, _ := json.Marshal(msg)
	return string(b)
}

func stockCall(id any) string {
	return rpc(id, "tools/call", obj{"name": "lookup_stock", "arguments": obj{"id": "sku-1"}})
}

func reportCall(id any, steps int) string {
	return rpc(id, "tools/call", obj{"name": "slow_report", "arguments": obj{"steps": steps}})
}

type rpcMessage struct {
	JSONRPC string
	ID      json.RawMessage
	Method  string
	Params  struct{ Data string }
	Result  *struct {
		Content []struct{ Type, Text string }
		IsError bool `json:"isError"`
	}
	Error *struct {
		Code    int
		Message string
	}
}

func parseRPC(t testing.TB, body []byte) rpcMessage {
	t.Helper()
	var m rpcMessage
	if err := json.Unmarshal(body, &m); err != nil || m.JSONRPC != "2.0" {
		t.Fatalf("not a JSON-RPC message: %v\n%s", err, body)
	}
	return m
}

// mcpEvents splits an event stream into the data of its events. Every event
// must be "event: message" with one data line.
func mcpEvents(t testing.TB, body []byte) []string {
	t.Helper()
	var out []string
	if len(bytes.TrimSpace(body)) == 0 {
		return out
	}
	for _, block := range strings.Split(strings.TrimSuffix(string(body), "\n\n"), "\n\n") {
		lines := strings.Split(block, "\n")
		if len(lines) != 2 || lines[0] != "event: message" || !strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("malformed event: %q", block)
		}
		out = append(out, strings.TrimPrefix(lines[1], "data: "))
	}
	return out
}

type mcpStack struct {
	p    *proxyProc
	demo *demoProc
}

func startMCP(t testing.TB) *mcpStack {
	t.Helper()
	s := newStack(t, nil)
	d := startDemo(t, "naive", "http://"+s.trafficAddr)
	s.routes = map[string]string{"llm": "http://" + d.llm}
	return &mcpStack{p: s.startProxy(t), demo: d}
}

func (m *mcpStack) post(body string, headers map[string]string) result {
	h := map[string]string{"Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
	for k, v := range headers {
		h[k] = v
	}
	return call("POST", m.p.url("/llm/mcp"), h, body)
}

func (m *mcpStack) faulted(body string) result { return m.post(body, scoped(testRun)) }

func (m *mcpStack) install(t testing.TB, typ string, params obj, mods ...func(obj)) {
	t.Helper()
	m.p.install(t, testRun, "header", flt("mcp1", typ, "llm", params, append([]func(obj){match("POST", "/mcp")}, mods...)...))
}

func isJSON(res result) bool {
	return strings.HasPrefix(res.header.Get("Content-Type"), "application/json")
}

func TestDemoMCPServer(t *testing.T) {
	d := startDemo(t, "naive", "http://127.0.0.1:1")
	url := "http://" + d.llm + "/mcp"
	post := func(body string) result {
		return call("POST", url, map[string]string{"Content-Type": "application/json"}, body)
	}

	for requested, want := range map[string]string{"2025-06-18": "2025-06-18", "2025-03-26": "2025-03-26", "1999-01-01": "2025-06-18"} {
		res := post(rpc(1, "initialize", obj{"protocolVersion": requested, "capabilities": obj{}, "clientInfo": obj{"name": "t", "version": "1"}}))
		var init struct {
			Result struct {
				ProtocolVersion string
				Capabilities    map[string]any
				ServerInfo      struct{ Name, Version string }
			}
		}
		if json.Unmarshal(res.body, &init); res.status != 200 || !isJSON(res) || init.Result.ProtocolVersion != want || init.Result.ServerInfo.Name != "faultline-demo" || init.Result.Capabilities["tools"] == nil {
			t.Fatalf("initialize %s: %d %s", requested, res.status, res.body)
		}
		if res.header.Get("Mcp-Session-Id") != "" {
			t.Fatal("the demo server is stateless and must not issue a session id")
		}
	}
	if res := post(rpc(nil, "notifications/initialized", nil)); res.status != 202 || len(res.body) != 0 {
		t.Fatalf("notification: %d %q", res.status, res.body)
	}
	if res := post(rpc(2, "ping", nil)); res.status != 200 || string(parseRPC(t, res.body).ID) != "2" || !bytes.Contains(res.body, []byte(`"result":{}`)) {
		t.Fatalf("ping: %d %s", res.status, res.body)
	}
	var list struct {
		Result struct {
			Tools []struct {
				Name        string
				InputSchema map[string]any `json:"inputSchema"`
			}
		}
	}
	if res := post(rpc(3, "tools/list", nil)); json.Unmarshal(res.body, &list) != nil || len(list.Result.Tools) != 2 ||
		list.Result.Tools[0].Name != "lookup_stock" || list.Result.Tools[1].Name != "slow_report" || list.Result.Tools[0].InputSchema["type"] != "object" {
		t.Fatalf("tools/list: %s", res.body)
	}

	res := post(stockCall("abc"))
	m := parseRPC(t, res.body)
	if res.status != 200 || !isJSON(res) || string(m.ID) != `"abc"` || m.Result == nil || m.Result.IsError || len(m.Result.Content) != 1 || m.Result.Content[0].Text != `{"id":"sku-1","stock":42}` {
		t.Fatalf("lookup_stock: %d %s", res.status, res.body)
	}
	if m := parseRPC(t, post(rpc(4, "tools/call", obj{"name": "lookup_stock", "arguments": obj{"id": 5}})).body); m.Result == nil || !m.Result.IsError {
		t.Fatalf("a bad argument is a tool error: %+v", m)
	}

	req, _ := http.NewRequest("POST", url, strings.NewReader(reportCall(5, 4)))
	begin := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if headers := time.Since(begin); headers > 150*time.Millisecond || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("slow_report: headers after %s, content type %q", headers, resp.Header.Get("Content-Type"))
	}
	body, _ := io.ReadAll(resp.Body)
	if took := time.Since(begin); took < 200*time.Millisecond {
		t.Fatalf("4 steps 50ms apart take at least 200ms, took %s", took)
	}
	events := mcpEvents(t, body)
	if len(events) != 5 {
		t.Fatalf("slow_report: %d events", len(events))
	}
	for i, e := range events[:4] {
		if n := parseRPC(t, []byte(e)); n.Method != "notifications/message" || n.Params.Data != fmt.Sprintf("step %d of 4", i+1) || len(n.ID) != 0 {
			t.Fatalf("event %d: %s", i+1, e)
		}
	}
	if last := parseRPC(t, []byte(events[4])); string(last.ID) != "5" || last.Result == nil || last.Result.IsError || last.Result.Content[0].Text != "report complete after 4 steps" {
		t.Fatalf("response event: %s", events[4])
	}
	if m := parseRPC(t, post(reportCall(6, 11)).body); m.Result == nil || !m.Result.IsError {
		t.Fatal("steps out of range is a tool error")
	}

	for name, tc := range map[string]struct {
		body   string
		status int
		code   int
	}{
		"unknown tool":   {rpc(7, "tools/call", obj{"name": "nope"}), 200, -32602},
		"unknown method": {rpc(8, "resources/list", nil), 200, -32601},
		"batch":          {"[" + rpc(9, "ping", nil) + "]", 400, -32600},
		"not JSON-RPC":   {`{"hello":"world"}`, 400, -32600},
		"not JSON":       {"{nope", 400, -32700},
	} {
		res := post(tc.body)
		if m := parseRPC(t, res.body); res.status != tc.status || m.Error == nil || m.Error.Code != tc.code {
			t.Errorf("%s: %d %s", name, res.status, res.body)
		}
	}
	for _, method := range []string{"GET", "DELETE"} {
		if res := call(method, url, nil, ""); res.status != 405 {
			t.Errorf("%s /mcp: %d", method, res.status)
		}
	}
}

func TestMCPToolError(t *testing.T) {
	m := startMCP(t)

	t.Run("tool error, defaults", func(t *testing.T) {
		m.install(t, "mcp_tool_error", obj{})
		for name, body := range map[string]string{"json tool": stockCall(11), "stream tool": reportCall(11, 5)} {
			res := m.faulted(body)
			if res.err != nil || res.status != 200 || !isJSON(res) || res.header.Get("X-Faultline-Fault") != "mcp1" {
				t.Fatalf("%s: %d %v %v", name, res.status, res.err, res.header)
			}
			if string(res.body) != `{"jsonrpc":"2.0","id":11,"result":{"content":[{"type":"text","text":"faultline injected"}],"isError":true}}` {
				t.Fatalf("%s: body %s", name, res.body)
			}
			// The upstream is not called: a five-step report would take 250ms.
			if res.took > 150*time.Millisecond {
				t.Fatalf("%s: answered after %s", name, res.took)
			}
		}
	})

	t.Run("message, string id", func(t *testing.T) {
		m.install(t, "mcp_tool_error", obj{"kind": "tool", "message": `stock "db" unavailable`})
		got := parseRPC(t, m.faulted(stockCall("req-7")).body)
		if string(got.ID) != `"req-7"` || got.Result == nil || !got.Result.IsError || got.Result.Content[0].Text != `stock "db" unavailable` {
			t.Fatalf("%+v", got)
		}
	})

	t.Run("protocol error", func(t *testing.T) {
		m.install(t, "mcp_tool_error", obj{"kind": "protocol"})
		if body := m.faulted(stockCall(3)).body; string(body) != `{"jsonrpc":"2.0","id":3,"error":{"code":-32603,"message":"faultline injected"}}` {
			t.Fatalf("default: %s", body)
		}
		m.install(t, "mcp_tool_error", obj{"kind": "protocol", "code": -32000, "message": "busy"})
		if body := m.faulted(reportCall(4, 2)).body; string(body) != `{"jsonrpc":"2.0","id":4,"error":{"code":-32000,"message":"busy"}}` {
			t.Fatalf("custom: %s", body)
		}
	})

	recs := m.p.recordsN(t, testRun, 2)
	if r := recs[len(recs)-1]; r.Status != 200 || r.FaultType == nil || *r.FaultType != "mcp_tool_error" || *r.FaultID != "mcp1" || r.Path != "/mcp" {
		t.Fatalf("record: %+v", r)
	}
}

func TestMCPSlowTool(t *testing.T) {
	m := startMCP(t)
	plain, stream := m.post(stockCall(1), nil), m.post(reportCall(1, 2), nil)
	m.install(t, "mcp_slow_tool", obj{"delay_ms": 600})

	res := m.faulted(stockCall(1))
	if res.took < 600*time.Millisecond || res.took > 1400*time.Millisecond {
		t.Fatalf("json tool: expected about 600ms, took %s", res.took)
	}
	if !bytes.Equal(res.body, plain.body) || res.header.Get("X-Faultline-Fault") != "mcp1" {
		t.Fatalf("json tool: the response must be the upstream's, with the fault header: %s %v", res.body, res.header)
	}
	res = m.faulted(reportCall(1, 2))
	if res.took < 700*time.Millisecond || !bytes.Equal(res.body, stream.body) {
		t.Fatalf("stream tool: took %s, body untouched: %v", res.took, bytes.Equal(res.body, stream.body))
	}

	// Clearing the faults ends a pending delay and the call goes through.
	m.install(t, "mcp_slow_tool", obj{"delay_ms": 20000})
	done := make(chan result, 1)
	go func() { done <- m.faulted(stockCall(2)) }()
	time.Sleep(300 * time.Millisecond)
	m.p.clear(t)
	select {
	case res := <-done:
		if res.status != 200 || parseRPC(t, res.body).Result == nil || res.took > 2*time.Second {
			t.Fatalf("after clear: %d %s after %s", res.status, res.body, res.took)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the delayed call was not released when the faults were cleared")
	}
}

func TestMCPMalformedResult(t *testing.T) {
	m := startMCP(t)
	const cut = `{"jsonrpc":"2.0","id":21,"result":{"content":[{"type":"text","text":"de`
	const schema = `{"jsonrpc":"2.0","id":21,"result":{"content":"faultline injected"}}`

	t.Run("json tool", func(t *testing.T) {
		for mode, want := range map[string]string{"": cut, "invalid_json": cut, "schema": schema} {
			params := obj{}
			if mode != "" {
				params["mode"] = mode
			}
			m.install(t, "mcp_malformed_result", params)
			res := m.faulted(stockCall(21))
			if res.err != nil || res.status != 200 || !isJSON(res) || res.header.Get("X-Faultline-Fault") != "mcp1" {
				t.Fatalf("mode %q: %d %v", mode, res.status, res.err)
			}
			if string(res.body) != want || res.header.Get("Content-Length") != fmt.Sprint(len(want)) {
				t.Fatalf("mode %q: body %s, content-length %s", mode, res.body, res.header.Get("Content-Length"))
			}
			var v any
			if valid := json.Unmarshal(res.body, &v) == nil; valid != (mode == "schema") {
				t.Fatalf("mode %q: parses as JSON = %v", mode, valid)
			}
		}
	})

	t.Run("stream tool", func(t *testing.T) {
		clean := mcpEvents(t, m.post(reportCall(21, 3), nil).body)
		for mode, want := range map[string]string{"invalid_json": cut, "schema": schema} {
			m.install(t, "mcp_malformed_result", obj{"mode": mode})
			res := m.faulted(reportCall(21, 3))
			if res.err != nil || res.status != 200 || !strings.HasPrefix(res.header.Get("Content-Type"), "text/event-stream") {
				t.Fatalf("mode %s: %d %v", mode, res.status, res.err)
			}
			events := mcpEvents(t, res.body)
			if len(events) != 4 || events[3] != want {
				t.Fatalf("mode %s: events %q", mode, events)
			}
			for i := 0; i < 3; i++ {
				if events[i] != clean[i] {
					t.Fatalf("mode %s: notification %d was altered: %s", mode, i+1, events[i])
				}
			}
		}
	})

	t.Run("stream stays unbuffered", func(t *testing.T) {
		m.install(t, "mcp_malformed_result", obj{})
		req, _ := http.NewRequest("POST", m.p.url("/llm/mcp"), strings.NewReader(reportCall(21, 10)))
		req.Header.Set("X-Chaos-Run", testRun)
		begin := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		first := make([]byte, 1)
		io.ReadFull(resp.Body, first)
		firstAt := time.Since(begin)
		io.Copy(io.Discard, resp.Body)
		if doneAt := time.Since(begin); firstAt > 300*time.Millisecond || doneAt < 500*time.Millisecond {
			t.Fatalf("events must be flushed as they arrive: first byte %s, last %s", firstAt, doneAt)
		}
	})

	t.Run("a JSON-RPC error answering the call is replaced too", func(t *testing.T) {
		m.install(t, "mcp_malformed_result", obj{})
		if body := m.faulted(rpc(21, "tools/call", obj{"name": "nope"})).body; string(body) != cut {
			t.Fatalf("an error response is the response to the call and is replaced: %s", body)
		}
	})
}

func TestMCPStreamDrop(t *testing.T) {
	m := startMCP(t)
	clean := mcpEvents(t, m.post(reportCall(31, 5), nil).body)

	for _, tc := range []struct {
		after, want int
	}{{0, 0}, {2, 2}, {5, 5}, {99, 5}} {
		params := obj{"after_events": tc.after}
		if tc.after == 0 {
			params = obj{}
		}
		m.install(t, "mcp_stream_drop", params)
		res := m.faulted(reportCall(31, 5))
		if res.status != 200 || res.header.Get("X-Faultline-Fault") != "mcp1" || !strings.HasPrefix(res.header.Get("Content-Type"), "text/event-stream") {
			t.Fatalf("after_events %d: %d %v", tc.after, res.status, res.header)
		}
		if res.err == nil {
			t.Fatalf("after_events %d: the connection must be cut, not ended cleanly", tc.after)
		}
		events := mcpEvents(t, res.body)
		if len(events) != tc.want {
			t.Fatalf("after_events %d: %d events reached the client, want %d", tc.after, len(events), tc.want)
		}
		for i, e := range events {
			if e != clean[i] {
				t.Fatalf("after_events %d: event %d was altered", tc.after, i+1)
			}
		}
		if bytes.Contains(res.body, []byte(`"result"`)) {
			t.Fatalf("after_events %d: the response to the call reached the client", tc.after)
		}
	}
	// after_events 0 does not wait for the upstream's first event.
	m.install(t, "mcp_stream_drop", obj{})
	if res := m.faulted(reportCall(31, 10)); res.took > 250*time.Millisecond {
		t.Fatalf("a drop before the first event took %s", res.took)
	}

	t.Run("json tool", func(t *testing.T) {
		m.install(t, "mcp_stream_drop", obj{"after_events": 3})
		res := m.faulted(stockCall(32))
		if res.status != 200 || len(res.body) != 0 || res.err == nil {
			t.Fatalf("expected status and headers, then a cut with no body: %d %q %v", res.status, res.body, res.err)
		}
	})

	recs := m.p.recordsN(t, testRun, 6)
	if r := recs[len(recs)-1]; r.Status != 0 || *r.FaultType != "mcp_stream_drop" {
		t.Fatalf("record of a dropped call: %+v", r)
	}
}

func TestMCPMatching(t *testing.T) {
	m := startMCP(t)
	untouched := func(t *testing.T, what string, res result, status int) {
		t.Helper()
		if res.err != nil || res.status != status || res.header.Get("X-Faultline-Fault") != "" {
			t.Fatalf("%s must pass untouched: %d %v %s", what, res.status, res.err, res.body)
		}
	}

	t.Run("only tool calls are touched", func(t *testing.T) {
		m.install(t, "mcp_tool_error", obj{})
		untouched(t, "initialize", m.faulted(rpc(1, "initialize", obj{"protocolVersion": "2025-06-18"})), 200)
		untouched(t, "tools/list", m.faulted(rpc(2, "tools/list", nil)), 200)
		untouched(t, "a notification", m.faulted(rpc(nil, "notifications/initialized", nil)), 202)
		untouched(t, "a tools/call notification", m.faulted(rpc(nil, "tools/call", obj{"name": "lookup_stock"})), 202)
		untouched(t, "a batch", m.faulted("["+stockCall(3)+"]"), 400)
		untouched(t, "a body that is not JSON", m.faulted("tools/call"), 400)
		untouched(t, "GET", call("GET", m.p.url("/llm/mcp"), scoped(testRun), ""), 405)
		big := rpc(4, "tools/call", obj{"name": "lookup_stock", "arguments": obj{"id": strings.Repeat("x", 1<<20)}})
		if res := m.faulted(big); res.header.Get("X-Faultline-Fault") != "" {
			t.Fatalf("a body over 1 MiB is not inspected: %d", res.status)
		}
		if m.p.triggers(t, "mcp1") != 0 {
			t.Fatalf("none of these may count as a trigger, got %d", m.p.triggers(t, "mcp1"))
		}
		if res := m.faulted(stockCall(5)); res.header.Get("X-Faultline-Fault") != "mcp1" || m.p.triggers(t, "mcp1") != 1 {
			t.Fatal("a tool call must be faulted")
		}
	})

	t.Run("the request body is forwarded unchanged", func(t *testing.T) {
		m.install(t, "mcp_slow_tool", obj{"delay_ms": 1})
		body := rpc("keep", "tools/call", obj{"name": "lookup_stock", "arguments": obj{"id": "sku-€-42"}})
		got := parseRPC(t, m.faulted(body).body)
		if got.Result == nil || got.Result.Content[0].Text != `{"id":"sku-€-42","stock":42}` || string(got.ID) != `"keep"` {
			t.Fatalf("%+v", got)
		}
	})

	t.Run("tool filter", func(t *testing.T) {
		m.install(t, "mcp_tool_error", obj{"tool": "slow_report"})
		untouched(t, "another tool", m.faulted(stockCall(1)), 200)
		if res := m.faulted(reportCall(2, 1)); res.header.Get("X-Faultline-Fault") != "mcp1" || !parseRPC(t, res.body).Result.IsError {
			t.Fatalf("the named tool must be faulted: %s", res.body)
		}
	})

	t.Run("the strategy counts tool calls only", func(t *testing.T) {
		m.install(t, "mcp_tool_error", obj{}, strategy(obj{"kind": "every_nth", "n": 2}))
		var faulted []bool
		for i := 0; i < 4; i++ {
			m.faulted(rpc(100+i, "tools/list", nil))
			faulted = append(faulted, m.faulted(stockCall(i)).header.Get("X-Faultline-Fault") != "")
		}
		if fmt.Sprint(faulted) != "[false true false true]" {
			t.Fatalf("every second tool call must be faulted whatever else is sent: %v", faulted)
		}
	})

	t.Run("a request that is not a tool call falls through to the next fault", func(t *testing.T) {
		m.p.install(t, testRun, "header",
			flt("mcp1", "mcp_tool_error", "llm", obj{}, match("POST", "/mcp")),
			flt("http1", "http_error", "llm", obj{"status": 503}, match("POST", "/mcp")))
		if res := m.faulted(rpc(1, "tools/list", nil)); res.status != 503 || res.header.Get("X-Faultline-Fault") != "http1" {
			t.Fatalf("tools/list: %d %v", res.status, res.header)
		}
		if res := m.faulted(stockCall(2)); res.status != 200 || res.header.Get("X-Faultline-Fault") != "mcp1" {
			t.Fatalf("tools/call: %d %v", res.status, res.header)
		}
	})

	t.Run("scoping", func(t *testing.T) {
		m.install(t, "mcp_tool_error", obj{})
		untouched(t, "an unscoped call", m.post(stockCall(1), nil), 200)
		untouched(t, "another run's call", m.post(stockCall(2), scoped("run_bbbbbbbbbbbb")), 200)
		m.p.install(t, testRun, "all", flt("mcp1", "mcp_tool_error", "llm", obj{}, match("POST", "/mcp")))
		if res := m.post(stockCall(3), nil); res.header.Get("X-Faultline-Fault") != "mcp1" {
			t.Fatal("scope all faults every tool call")
		}
	})

	t.Run("expose_fault_header off", func(t *testing.T) {
		s := newStack(t, map[string]string{"llm": "http://" + m.demo.llm})
		s.proxyExtra = "  expose_fault_header: false\n"
		p := s.startProxy(t)
		p.install(t, testRun, "header", flt("mcp1", "mcp_tool_error", "llm", obj{}))
		res := call("POST", p.url("/llm/mcp"), scoped(testRun), stockCall(1))
		if !parseRPC(t, res.body).Result.IsError || res.header.Get("X-Faultline-Fault") != "" {
			t.Fatalf("faulted without the header: %s %v", res.body, res.header)
		}
	})
}

func TestMCPAdminValidation(t *testing.T) {
	m := startMCP(t)
	for name, f := range map[string]obj{
		"bad kind":          flt("f", "mcp_tool_error", "llm", obj{"kind": "fatal"}),
		"code without kind": flt("f", "mcp_tool_error", "llm", obj{"code": -32000}),
		"empty tool":        flt("f", "mcp_tool_error", "llm", obj{"tool": ""}),
		"missing delay":     flt("f", "mcp_slow_tool", "llm", obj{}),
		"bad mode":          flt("f", "mcp_malformed_result", "llm", obj{"mode": "garble"}),
		"negative events":   flt("f", "mcp_stream_drop", "llm", obj{"after_events": -1}),
		"unknown param":     flt("f", "mcp_stream_drop", "llm", obj{"after_bytes": 1}),
	} {
		var resp struct {
			Errors []struct{ Path, Message string }
		}
		body := obj{"run_id": testRun, "scope_mode": "header", "lease_id": "l", "faults": []obj{f}}
		if code := m.p.adminCall(t, "PUT", "/admin/v1/faults", body, &resp); code != 400 || len(resp.Errors) != 1 || !strings.HasPrefix(resp.Errors[0].Path, "faults[0].params.") {
			t.Errorf("%s: expected 400 with one params error, got %d %+v", name, code, resp)
		}
	}
}

// MCP faults installed by a real run, with the driver making the tool calls.
func TestMCPFaultsThroughControl(t *testing.T) {
	f := startFull(t, "naive")
	exp := func(name, fault, hypothesis string) string {
		return f.experiment(t, expOpts{
			Name: name, Duration: "2s", BaseURL: "http://" + f.s.trafficAddr,
			Requests: "    - {method: POST, path: /llm/mcp, headers: {Content-Type: application/json}, body: '" + stockCall(1) + "'}\n" +
				"    - {method: POST, path: /llm/mcp, headers: {Content-Type: application/json}, body: '" + rpc(2, "tools/list", nil) + "'}",
			Hypothesis: hypothesis,
			Faults:     fault,
		})
	}

	// A tool error is a 200: the transport is healthy, the hypothesis holds, the fault is counted.
	doc := f.control.waitDone(t, f.control.createRun(t, exp("mcp-tool-error",
		"  - id: f1\n    type: mcp_tool_error\n    route: llm\n    match: {method: POST, path: /mcp}\n    params: {tool: lookup_stock}",
		"    error_rate_max: 0.05")), 30*time.Second)
	if doc.verdict() != "passed" || doc.Faults[0].Type != "mcp_tool_error" || doc.Faults[0].Triggers == 0 || doc.Metrics.Driver.Errors != 0 {
		t.Fatalf("got %s (%s), %+v", doc.verdict(), doc.reason(), doc.Faults)
	}
	// Half the driver's requests are tools/list, which the fault must not touch.
	if p := doc.Metrics.Proxy["llm"]; p.Faulted == 0 || p.Faulted*2 > p.Requests+2 {
		t.Fatalf("only the tool calls may be faulted: %+v", p)
	}

	// A dropped stream is a transport error for the driver.
	doc = f.control.waitDone(t, f.control.createRun(t, exp("mcp-stream-drop",
		"  - id: f1\n    type: mcp_stream_drop\n    route: llm\n    match: {method: POST, path: /mcp}",
		"    error_rate_max: 0.05")), 30*time.Second)
	if doc.verdict() != "failed" || doc.Faults[0].Triggers == 0 || doc.Metrics.Driver.Errors == 0 {
		t.Fatalf("got %s (%s), %+v", doc.verdict(), doc.reason(), doc.Metrics.Driver)
	}

	// A fault for a tool nobody calls never triggers.
	doc = f.control.waitDone(t, f.control.createRun(t, exp("mcp-unused-tool",
		"  - id: f1\n    type: mcp_slow_tool\n    route: llm\n    params: {delay_ms: 10, tool: slow_report}",
		"    error_rate_max: 0.05")), 30*time.Second)
	if doc.verdict() != "inconclusive" || doc.reason() != "fault_not_triggered" {
		t.Fatalf("got %s (%s)", doc.verdict(), doc.reason())
	}

	// The CLI validates the new types with the same rules.
	bad := f.experiment(t, expOpts{Name: "mcp-bad", Faults: "  - id: f1\n    type: mcp_slow_tool\n    route: llm\n    params: {tool: lookup_stock}"})
	var v struct {
		Valid  bool
		Errors []struct{ Path, Message string }
	}
	if code := f.control.api(t, "POST", "/api/v1/experiments/validate", obj{"yaml": bad}, &v); code != 200 || v.Valid || len(v.Errors) != 1 || v.Errors[0].Path != "faults[0].params.delay_ms" {
		t.Fatalf("validate: %d %+v", code, v)
	}
}
