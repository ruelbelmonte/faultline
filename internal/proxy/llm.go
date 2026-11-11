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

const (
	llmRateLimitBody  = `{"type":"error","error":{"type":"rate_limit_error","message":"faultline injected"}}`
	llmOverloadedBody = `{"type":"error","error":{"type":"overloaded_error","message":"faultline injected"}}`
	// maxRewriteBody bounds a non-streaming response held in memory for rewriting.
	maxRewriteBody = 32 << 20
)

// injectLLM applies the LLM fault types, in Anthropic Messages API format or,
// for a chat completions request, in OpenAI format.
func (p *Proxy) injectLLM(w http.ResponseWriter, r *http.Request, rt *route, rest string, d decision, base forwardOpts) outcome {
	rule, prm := d.rule, d.rule.params
	chat := isChatCompletions("/" + rest)
	switch rule.spec.Type {
	case fault.LLMRateLimit:
		body := llmRateLimitBody
		if chat {
			body = chatRateLimitBody
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", strconv.Itoa(*prm.RetryAfter))
		p.respond(w, rule, http.StatusTooManyRequests, body)
		return outcome{status: http.StatusTooManyRequests, retryAfter: prm.RetryAfter}

	case fault.LLMOverloaded:
		// OpenAI has no 529; an overloaded engine answers 503.
		status, body := 529, llmOverloadedBody
		if chat {
			status, body = http.StatusServiceUnavailable, chatOverloadedBody
		}
		w.Header().Set("Content-Type", "application/json")
		p.respond(w, rule, status, body)
		return outcome{status: status}

	case fault.LLMStreamCut:
		base.hasCut, base.cutAfter = true, int64(prm.AfterBytes)

	case fault.LLMSlowFirstToken:
		base.firstByteDelay = time.Duration(prm.DelayMS) * time.Millisecond

	case fault.LLMTruncate:
		base.rewrite = func(w http.ResponseWriter, r *http.Request, resp *http.Response, o forwardOpts) outcome {
			if chat {
				return p.rewriteLLM(w, r, resp, o, truncateChat(prm.MaxChars), func() sseRewriter { return &truncateChatStream{max: prm.MaxChars, sent: map[string]int{}} })
			}
			return p.rewriteLLM(w, r, resp, o, truncateMessage(prm.MaxChars), func() sseRewriter { return &truncateStream{max: prm.MaxChars} })
		}

	case fault.LLMMalformedToolCall:
		base.rewrite = func(w http.ResponseWriter, r *http.Request, resp *http.Response, o forwardOpts) outcome {
			if chat {
				return p.rewriteLLM(w, r, resp, o, malformChat, func() sseRewriter { return &malformChatStream{seen: map[string]bool{}} })
			}
			return p.rewriteLLM(w, r, resp, o, malformMessage, func() sseRewriter { return &malformStream{seen: map[any]bool{}} })
		}
	}
	return p.forward(w, r, rt, rest, base)
}

// sseRewriter turns one upstream event into what is sent to the client.
// done ends the stream after out has been written.
type sseRewriter interface {
	event(raw []byte, data map[string]any) (out []byte, done bool)
}

// rewriteLLM rewrites a successful Messages API response. Anything it does
// not recognise (errors, compressed or non-JSON bodies) is relayed untouched.
func (p *Proxy) rewriteLLM(w http.ResponseWriter, r *http.Request, resp *http.Response, o forwardOpts, message func(map[string]any) bool, stream func() sseRewriter) outcome {
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "" {
		return p.relay(w, r, resp, o)
	}
	if strings.HasPrefix(ct, "text/event-stream") {
		return p.rewriteStream(w, r, resp, o, stream())
	}
	if !strings.Contains(ct, "json") {
		return p.relay(w, r, resp, o)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRewriteBody+1))
	if err != nil {
		closeConn(w, false)
		return outcome{}
	}
	var msg map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if len(body) <= maxRewriteBody && dec.Decode(&msg) == nil && message(msg) {
		if out, err := json.Marshal(msg); err == nil {
			body = out
		}
	}
	copyHeaders(w.Header(), resp.Header)
	p.setFaultHeader(w.Header(), o.rule)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
	return outcome{status: resp.StatusCode}
}

func (p *Proxy) rewriteStream(w http.ResponseWriter, r *http.Request, resp *http.Response, o forwardOpts, rw sseRewriter) outcome {
	copyHeaders(w.Header(), resp.Header)
	p.setFaultHeader(w.Header(), o.rule)
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	rc.Flush()

	br := bufio.NewReaderSize(resp.Body, 64*1024)
	for {
		raw, readErr := readEvent(br)
		if len(raw) > 0 {
			out, done := rw.event(raw, eventData(raw))
			if len(out) > 0 {
				if _, err := w.Write(out); err != nil {
					return outcome{status: resp.StatusCode}
				}
				rc.Flush()
			}
			if done {
				return outcome{status: resp.StatusCode}
			}
		}
		if readErr == io.EOF {
			return outcome{status: resp.StatusCode}
		}
		if readErr != nil {
			closeConn(w, false)
			return outcome{status: resp.StatusCode}
		}
	}
}

// readEvent returns the raw bytes of one SSE event including its blank-line
// terminator. At end of stream it returns whatever is left with io.EOF.
func readEvent(br *bufio.Reader) ([]byte, error) {
	var raw []byte
	for {
		line, err := br.ReadBytes('\n')
		raw = append(raw, line...)
		if err != nil {
			return raw, err
		}
		if len(bytes.TrimRight(line, "\r\n")) == 0 {
			return raw, nil
		}
	}
}

// eventData decodes the JSON carried by an event's data lines, or nil.
func eventData(raw []byte) map[string]any {
	var data []byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if rest, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data = append(data, bytes.TrimPrefix(rest, []byte(" "))...)
		}
	}
	if len(data) == 0 {
		return nil
	}
	var v map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return nil
	}
	return v
}

func formatEvent(name string, data map[string]any) []byte {
	b, _ := json.Marshal(data)
	return []byte("event: " + name + "\ndata: " + string(b) + "\n\n")
}

func deltaOf(data map[string]any, deltaType string) (map[string]any, bool) {
	if data == nil || data["type"] != "content_block_delta" {
		return nil, false
	}
	delta, ok := data["delta"].(map[string]any)
	return delta, ok && delta["type"] == deltaType
}

// truncateMessage shortens the text content of a non-streaming message to
// max characters in total and marks it as stopped by max_tokens.
func truncateMessage(max int) func(map[string]any) bool {
	return func(msg map[string]any) bool {
		blocks, _ := msg["content"].([]any)
		budget, truncated := max, false
		for _, b := range blocks {
			block, ok := b.(map[string]any)
			if !ok || block["type"] != "text" {
				continue
			}
			text, _ := block["text"].(string)
			runes := []rune(text)
			if len(runes) > budget {
				block["text"] = string(runes[:budget])
				truncated = true
				budget = 0
				continue
			}
			budget -= len(runes)
		}
		if truncated {
			msg["stop_reason"] = "max_tokens"
		}
		return truncated
	}
}

// truncateStream forwards text deltas until max characters have been sent,
// then closes the message the way the API does when it hits max_tokens.
type truncateStream struct {
	max  int
	sent int
}

func (t *truncateStream) event(raw []byte, data map[string]any) ([]byte, bool) {
	delta, ok := deltaOf(data, "text_delta")
	if !ok {
		return raw, false
	}
	text, _ := delta["text"].(string)
	runes := []rune(text)
	if t.sent+len(runes) <= t.max {
		t.sent += len(runes)
		return raw, false
	}
	var out []byte
	if keep := t.max - t.sent; keep > 0 {
		delta["text"] = string(runes[:keep])
		out = formatEvent("content_block_delta", data)
		t.sent = t.max
	}
	out = append(out, formatEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": data["index"]})...)
	out = append(out, formatEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "max_tokens", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": (t.sent + 3) / 4},
	})...)
	out = append(out, formatEvent("message_stop", map[string]any{"type": "message_stop"})...)
	return out, true
}

// malformMessage replaces every tool_use input with a string of broken JSON.
func malformMessage(msg map[string]any) bool {
	blocks, _ := msg["content"].([]any)
	changed := false
	for _, b := range blocks {
		if block, ok := b.(map[string]any); ok && block["type"] == "tool_use" {
			block["input"] = fault.MalformedInput
			changed = true
		}
	}
	return changed
}

// malformStream makes the concatenated input_json_delta values of each
// tool_use block unparseable: the first carries broken JSON, the rest nothing.
type malformStream struct {
	seen map[any]bool
}

func (m *malformStream) event(raw []byte, data map[string]any) ([]byte, bool) {
	delta, ok := deltaOf(data, "input_json_delta")
	if !ok {
		return raw, false
	}
	index := data["index"]
	if n, ok := index.(json.Number); ok {
		index = n.String()
	}
	if m.seen[index] {
		delta["partial_json"] = ""
	} else {
		delta["partial_json"] = fault.MalformedInput
		m.seen[index] = true
	}
	return formatEvent("content_block_delta", data), false
}
