package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"faultline/internal/fault"
)

// MCP tool faults over Streamable HTTP. The contract is docs/mcp-faults.md.

// maxToolCallBody is how much of a request body is read to see whether it is
// a tool call. A larger body is not one.
const maxToolCallBody = 1 << 20

// mcpCall is a tools/call request.
type mcpCall struct {
	id   string // the JSON-RPC id as compact JSON
	tool string
}

// compactID returns a JSON-RPC id (a string or a number) as compact JSON.
func compactID(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || !(raw[0] == '"' || raw[0] == '-' || (raw[0] >= '0' && raw[0] <= '9')) {
		return "", false
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return "", false
	}
	return b.String(), true
}

// readToolCall reports the tool call a request carries, or nil. It reads at
// most maxToolCallBody+1 bytes of the body and puts them back, so the request
// is forwarded unchanged whatever it turns out to be.
func readToolCall(r *http.Request) *mcpCall {
	if r.Method != http.MethodPost || r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	head, err := io.ReadAll(io.LimitReader(r.Body, maxToolCallBody+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), r.Body), r.Body}
	if err != nil || len(head) > maxToolCallBody {
		return nil
	}
	var msg struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  struct {
			Name *string `json:"name"`
		} `json:"params"`
	}
	if json.Unmarshal(head, &msg) != nil || msg.JSONRPC != "2.0" || msg.Method != "tools/call" || msg.Params.Name == nil {
		return nil
	}
	id, ok := compactID(msg.ID)
	if !ok {
		return nil
	}
	return &mcpCall{id: id, tool: *msg.Params.Name}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (c *mcpCall) toolError(message string) string {
	return `{"jsonrpc":"2.0","id":` + c.id + `,"result":{"content":[{"type":"text","text":` + jsonString(message) + `}],"isError":true}}`
}

func (c *mcpCall) protocolError(code int, message string) string {
	return `{"jsonrpc":"2.0","id":` + c.id + `,"error":{"code":` + strconv.Itoa(code) + `,"message":` + jsonString(message) + `}}`
}

// malformed is what replaces the response to the call.
func (c *mcpCall) malformed(mode string) string {
	if mode == "schema" {
		return `{"jsonrpc":"2.0","id":` + c.id + `,"result":{"content":"faultline injected"}}`
	}
	return `{"jsonrpc":"2.0","id":` + c.id + `,"result":{"content":[{"type":"text","text":"de`
}

// answers reports whether a JSON-RPC message is the response to the call.
func (c *mcpCall) answers(msg map[string]any) bool {
	if msg == nil {
		return false
	}
	_, hasResult := msg["result"]
	_, hasError := msg["error"]
	if !hasResult && !hasError {
		return false
	}
	id, err := json.Marshal(msg["id"])
	return err == nil && string(id) == c.id
}

func decodeObject(body []byte) map[string]any {
	var v map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return nil
	}
	return v
}

func (p *Proxy) injectMCP(w http.ResponseWriter, r *http.Request, rt *route, rest string, d decision, base forwardOpts) outcome {
	rule, prm, call := d.rule, d.rule.params, d.mcp
	switch rule.spec.Type {
	case fault.MCPToolError:
		body := call.toolError(prm.Message)
		if prm.Kind == "protocol" {
			body = call.protocolError(prm.Code, prm.Message)
		}
		w.Header().Set("Content-Type", "application/json")
		p.respond(w, rule, http.StatusOK, body)
		return outcome{status: http.StatusOK}

	case fault.MCPSlowTool:
		if delay := time.Duration(prm.DelayMS) * time.Millisecond; delay > 0 && !sleep(r.Context(), d.rs, delay) && r.Context().Err() != nil {
			return outcome{}
		}

	case fault.MCPMalformedResult:
		base.rewrite = func(w http.ResponseWriter, r *http.Request, resp *http.Response, o forwardOpts) outcome {
			return p.malformMCP(w, r, resp, o, call, prm.Mode)
		}

	case fault.MCPStreamDrop:
		base.rewrite = func(w http.ResponseWriter, r *http.Request, resp *http.Response, o forwardOpts) outcome {
			return p.dropMCP(w, r, resp, o, call, prm.AfterEvents)
		}
	}
	return p.forward(w, r, rt, rest, base)
}

// mcpBody classifies a response an MCP fault may act on: "sse", "json", or ""
// for anything it must leave alone.
func mcpBody(resp *http.Response) string {
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "" {
		return ""
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	switch {
	case strings.HasPrefix(ct, "text/event-stream"):
		return "sse"
	case strings.Contains(ct, "json"):
		return "json"
	}
	return ""
}

func (p *Proxy) malformMCP(w http.ResponseWriter, r *http.Request, resp *http.Response, o forwardOpts, call *mcpCall, mode string) outcome {
	switch mcpBody(resp) {
	case "sse":
		return p.rewriteStream(w, r, resp, o, &mcpMalformStream{call: call, mode: mode})
	case "json":
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxRewriteBody+1))
		if err != nil {
			closeConn(w, false)
			return outcome{}
		}
		if len(body) <= maxRewriteBody && call.answers(decodeObject(body)) {
			body = []byte(call.malformed(mode))
		}
		copyHeaders(w.Header(), resp.Header)
		p.setFaultHeader(w.Header(), o.rule)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
		return outcome{status: resp.StatusCode}
	}
	return p.relay(w, r, resp, o)
}

// mcpMalformStream replaces the data of the one event that answers the call.
type mcpMalformStream struct {
	call *mcpCall
	mode string
	done bool
}

func (m *mcpMalformStream) event(raw []byte, data map[string]any) ([]byte, bool) {
	if m.done || !m.call.answers(data) {
		return raw, false
	}
	m.done = true
	var out []byte
	for _, line := range bytes.SplitAfter(raw, []byte("\n")) {
		if text := bytes.TrimRight(line, "\r\n"); len(text) > 0 && !bytes.HasPrefix(text, []byte("data:")) {
			out = append(out, line...)
		}
	}
	return append(out, "data: "+m.call.malformed(m.mode)+"\n\n"...), false
}

// dropMCP relays an event stream until afterEvents events have been sent or
// the response to the call arrives, then closes abruptly. A JSON response is
// dropped whole: status and headers, then the close.
func (p *Proxy) dropMCP(w http.ResponseWriter, r *http.Request, resp *http.Response, o forwardOpts, call *mcpCall, afterEvents int) outcome {
	kind := mcpBody(resp)
	if kind == "" {
		return p.relay(w, r, resp, o)
	}
	copyHeaders(w.Header(), resp.Header)
	p.setFaultHeader(w.Header(), o.rule)
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	rc.Flush()
	if kind == "json" {
		closeConn(w, false)
		return outcome{}
	}

	br := bufio.NewReaderSize(resp.Body, 64*1024)
	for sent := 0; ; sent++ {
		if sent >= afterEvents {
			closeConn(w, false)
			return outcome{}
		}
		raw, readErr := readEvent(br)
		if call.answers(eventData(raw)) || (readErr != nil && readErr != io.EOF) {
			closeConn(w, false)
			return outcome{}
		}
		if len(raw) > 0 {
			if _, err := w.Write(raw); err != nil {
				return outcome{status: resp.StatusCode}
			}
			rc.Flush()
		}
		if readErr == io.EOF {
			// The upstream ended the stream without answering the call.
			return outcome{status: resp.StatusCode}
		}
	}
}
