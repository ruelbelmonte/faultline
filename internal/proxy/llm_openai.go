package proxy

import (
	"encoding/json"
	"strings"

	"faultline/internal/fault"
)

// The LLM faults in OpenAI chat completions format. A request is treated as
// chat completions when its path ends in /chat/completions (DECISIONS D88).

const (
	chatRateLimitBody  = `{"error":{"message":"faultline injected","type":"rate_limit_error","param":null,"code":"rate_limit_exceeded"}}`
	chatOverloadedBody = `{"error":{"message":"faultline injected","type":"server_error","param":null,"code":"service_unavailable"}}`
)

func isChatCompletions(path string) bool {
	return strings.HasSuffix(strings.TrimRight(path, "/"), "/chat/completions")
}

func formatChunk(data map[string]any) []byte {
	b, _ := json.Marshal(data)
	return []byte("data: " + string(b) + "\n\n")
}

var chatDone = []byte("data: [DONE]\n\n")

func choicesOf(data map[string]any) []map[string]any {
	list, _ := data["choices"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, c := range list {
		if choice, ok := c.(map[string]any); ok {
			out = append(out, choice)
		}
	}
	return out
}

// choiceIndex is the choice's index as a map key; a chunk without one is choice 0.
func choiceIndex(choice map[string]any) string {
	if n, ok := choice["index"].(json.Number); ok {
		return n.String()
	}
	return "0"
}

// truncateChat shortens each choice's message content to max characters and
// marks it as stopped by length.
func truncateChat(max int) func(map[string]any) bool {
	return func(msg map[string]any) bool {
		truncated := false
		for _, choice := range choicesOf(msg) {
			message, _ := choice["message"].(map[string]any)
			text, ok := message["content"].(string)
			if runes := []rune(text); ok && len(runes) > max {
				message["content"] = string(runes[:max])
				choice["finish_reason"] = "length"
				truncated = true
			}
		}
		return truncated
	}
}

// truncateChatStream forwards content deltas until a choice has sent max
// characters, then finishes every open choice with finish_reason length and
// ends the stream with [DONE].
type truncateChatStream struct {
	max  int
	sent map[string]int
	open []string // choice indexes seen and not finished, in order of appearance
}

func (t *truncateChatStream) event(raw []byte, data map[string]any) ([]byte, bool) {
	over, kept := false, false
	for _, choice := range choicesOf(data) {
		idx := choiceIndex(choice)
		if _, seen := t.sent[idx]; !seen {
			t.sent[idx] = 0
			t.open = append(t.open, idx)
		}
		delta, _ := choice["delta"].(map[string]any)
		text, _ := delta["content"].(string)
		runes := []rune(text)
		if t.sent[idx]+len(runes) <= t.max {
			t.sent[idx] += len(runes)
			kept = kept || len(runes) > 0
			if choice["finish_reason"] != nil {
				t.open = remove(t.open, idx)
			}
			continue
		}
		keep := t.max - t.sent[idx]
		delta["content"] = string(runes[:keep])
		// This choice is finished below, with length.
		choice["finish_reason"] = nil
		t.sent[idx] = t.max
		over, kept = true, kept || keep > 0
	}
	if !over {
		return raw, false
	}

	delete(data, "usage")
	var out []byte
	if kept {
		out = formatChunk(data)
	}
	for _, idx := range t.open {
		data["choices"] = []any{map[string]any{"index": json.Number(idx), "delta": map[string]any{}, "finish_reason": "length"}}
		out = append(out, formatChunk(data)...)
	}
	return append(out, chatDone...), true
}

func remove(list []string, v string) []string {
	out := list[:0]
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// malformChat replaces the arguments of every tool call with broken JSON.
func malformChat(msg map[string]any) bool {
	changed := false
	for _, choice := range choicesOf(msg) {
		message, _ := choice["message"].(map[string]any)
		calls, _ := message["tool_calls"].([]any)
		for _, c := range calls {
			call, _ := c.(map[string]any)
			if fn, ok := call["function"].(map[string]any); ok {
				fn["arguments"] = fault.MalformedInput
				changed = true
			}
		}
	}
	return changed
}

// malformChatStream makes the concatenated arguments of each tool call
// unparseable: its first chunk carries broken JSON, the rest nothing.
type malformChatStream struct {
	seen map[string]bool
}

func (m *malformChatStream) event(raw []byte, data map[string]any) ([]byte, bool) {
	changed := false
	for _, choice := range choicesOf(data) {
		delta, _ := choice["delta"].(map[string]any)
		calls, _ := delta["tool_calls"].([]any)
		for _, c := range calls {
			call, _ := c.(map[string]any)
			fn, ok := call["function"].(map[string]any)
			if !ok {
				continue
			}
			if _, has := fn["arguments"]; !has {
				continue
			}
			key := choiceIndex(choice) + "/" + choiceIndex(call)
			if m.seen[key] {
				fn["arguments"] = ""
			} else {
				fn["arguments"] = fault.MalformedInput
				m.seen[key] = true
			}
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	return formatChunk(data), false
}
