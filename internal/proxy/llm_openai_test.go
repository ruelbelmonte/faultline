package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIsChatCompletions(t *testing.T) {
	for path, want := range map[string]bool{
		"/v1/chat/completions": true, "/openai/deployments/x/chat/completions": true, "/v1/chat/completions/": true,
		"/v1/messages": false, "/v1/completions": false, "/chat/completions/extra": false, "/": false,
	} {
		if got := isChatCompletions(path); got != want {
			t.Errorf("isChatCompletions(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestTruncateChatPerChoiceAndCountsRunes(t *testing.T) {
	msg := decode(t, `{"object":"chat.completion","choices":[
		{"index":0,"message":{"role":"assistant","content":"héllo wörld"},"finish_reason":"stop"},
		{"index":1,"message":{"role":"assistant","content":"hey"},"finish_reason":"stop"},
		{"index":2,"message":{"role":"assistant","content":null,"tool_calls":[]},"finish_reason":"tool_calls"}]}`)
	if !truncateChat(5)(msg) {
		t.Fatal("a choice beyond the limit must be reported as truncated")
	}
	choices := choicesOf(msg)
	if got := choices[0]["message"].(map[string]any)["content"]; got != "héllo" || choices[0]["finish_reason"] != "length" {
		t.Fatalf("choice 0: %v %v", got, choices[0]["finish_reason"])
	}
	if choices[1]["message"].(map[string]any)["content"] != "hey" || choices[1]["finish_reason"] != "stop" {
		t.Fatalf("a choice within the limit must be left alone: %v", choices[1])
	}
	if choices[2]["finish_reason"] != "tool_calls" {
		t.Fatalf("a choice without text must be left alone: %v", choices[2])
	}
	if truncateChat(100)(msg) {
		t.Fatal("nothing beyond the limit: not truncated")
	}
}

func chunk(t *testing.T, index int, content string, finish string) map[string]any {
	t.Helper()
	b, _ := json.Marshal(map[string]any{
		"id": "c1", "object": "chat.completion.chunk", "model": "m",
		"choices": []any{map[string]any{"index": index, "delta": map[string]any{"content": content}, "finish_reason": nil}},
	})
	m := decode(t, string(b))
	if finish != "" {
		choicesOf(m)[0]["finish_reason"] = finish
	}
	return m
}

func TestTruncateChatStreamBoundary(t *testing.T) {
	ts := &truncateChatStream{max: 10, sent: map[string]int{}}
	for _, text := range []string{"12345", "67890"} {
		if out, done := ts.event([]byte("raw"), chunk(t, 0, text, "")); string(out) != "raw" || done {
			t.Fatalf("a delta within the budget passes through untouched (%q)", text)
		}
	}
	if out, done := ts.event([]byte("data: [DONE]\n\n"), nil); string(out) != "data: [DONE]\n\n" || done {
		t.Fatal("events without JSON pass through")
	}
	out, done := ts.event([]byte("raw"), chunk(t, 0, "more", ""))
	if !done {
		t.Fatal("text beyond the limit ends the stream")
	}
	events := strings.Split(strings.TrimSuffix(string(out), "\n\n"), "\n\n")
	if len(events) != 2 || events[1] != "data: [DONE]" || strings.Contains(events[0], "more") {
		t.Fatalf("expected one finishing chunk and [DONE], got %q", out)
	}
	last := eventData([]byte(events[0] + "\n\n"))
	if c := choicesOf(last)[0]; c["finish_reason"] != "length" || len(c["delta"].(map[string]any)) != 0 || last["id"] != "c1" {
		t.Fatalf("finishing chunk: %v", last)
	}

	// A delta that crosses the limit is cut to fit, and every open choice is finished.
	ts = &truncateChatStream{max: 7, sent: map[string]int{}}
	ts.event(nil, chunk(t, 1, "ab", ""))
	ts.event(nil, chunk(t, 0, "12345", ""))
	out, done = ts.event(nil, chunk(t, 0, "67890", ""))
	events = strings.Split(strings.TrimSuffix(string(out), "\n\n"), "\n\n")
	if !done || len(events) != 4 || events[3] != "data: [DONE]" {
		t.Fatalf("expected cut delta, two finishing chunks and [DONE], got %q", out)
	}
	if !strings.Contains(events[0], `"content":"67"`) || strings.Contains(events[0], `"length"`) {
		t.Fatalf("cut delta: %s", events[0])
	}
	finished := map[string]bool{}
	for _, e := range events[1:3] {
		c := choicesOf(eventData([]byte(e + "\n\n")))[0]
		if c["finish_reason"] != "length" {
			t.Fatalf("finishing chunk: %s", e)
		}
		finished[choiceIndex(c)] = true
	}
	if !finished["0"] || !finished["1"] {
		t.Fatalf("both open choices must be finished: %v", finished)
	}

	// A choice the upstream already finished is not finished again.
	ts = &truncateChatStream{max: 3, sent: map[string]int{}}
	ts.event(nil, chunk(t, 1, "ab", "stop"))
	out, _ = ts.event(nil, chunk(t, 0, "12345", ""))
	if n := strings.Count(string(out), `"length"`); n != 1 {
		t.Fatalf("only the open choice is finished, got %d finishing chunks: %s", n, out)
	}
}

func TestMalformChat(t *testing.T) {
	msg := decode(t, `{"choices":[{"index":0,"message":{"content":null,"tool_calls":[
		{"id":"a","type":"function","function":{"name":"f","arguments":"{\"q\":1}"}},
		{"id":"b","type":"function","function":{"name":"g","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	if !malformChat(msg) {
		t.Fatal("tool calls must be reported as changed")
	}
	for _, c := range choicesOf(msg)[0]["message"].(map[string]any)["tool_calls"].([]any) {
		args := c.(map[string]any)["function"].(map[string]any)["arguments"].(string)
		var v any
		if json.Unmarshal([]byte(args), &v) == nil {
			t.Fatalf("arguments %q must not parse", args)
		}
	}
	if malformChat(decode(t, `{"choices":[{"index":0,"message":{"content":"hi"},"finish_reason":"stop"}]}`)) {
		t.Fatal("a response without tool calls is not changed")
	}
}

func TestMalformChatStreamPerToolCall(t *testing.T) {
	m := &malformChatStream{seen: map[string]bool{}}
	ev := func(call int, args string) string {
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
			"tool_calls": []any{map[string]any{"index": call, "function": map[string]any{"arguments": args}}},
		}}}})
		out, done := m.event(nil, decode(t, string(b)))
		if done {
			t.Fatal("the stream must not be ended")
		}
		calls := choicesOf(eventData(out))[0]["delta"].(map[string]any)["tool_calls"].([]any)
		return calls[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
	}
	got := map[int]string{}
	got[0] += ev(0, "")
	got[1] += ev(1, "{")
	got[0] += ev(0, "{}")
	got[1] += ev(1, "}")
	for call, joined := range got {
		var v any
		if joined == "" || json.Unmarshal([]byte(joined), &v) == nil {
			t.Fatalf("tool call %d: concatenated arguments %q must not parse", call, joined)
		}
	}
	if out, done := m.event([]byte("raw"), chunk(t, 0, "text", "")); string(out) != "raw" || done {
		t.Fatal("content deltas pass through untouched")
	}
}
