package proxy

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestReadEventHandlesLFAndCRLF(t *testing.T) {
	for _, nl := range []string{"\n", "\r\n"} {
		stream := "event: a" + nl + "data: {\"type\":\"a\"}" + nl + nl + "data: {\"type\":\"b\"}" + nl + nl + "data: {\"type\":\"partial\"}"
		br := bufio.NewReader(strings.NewReader(stream))
		var types []string
		var rebuilt strings.Builder
		for {
			raw, err := readEvent(br)
			rebuilt.Write(raw)
			if d := eventData(raw); d != nil {
				types = append(types, d["type"].(string))
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if strings.Join(types, ",") != "a,b,partial" {
			t.Fatalf("newline %q: events %v", nl, types)
		}
		if rebuilt.String() != stream {
			t.Fatalf("newline %q: raw events must reassemble to the original bytes", nl)
		}
	}
}

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTruncateMessageSpansBlocksAndCountsRunes(t *testing.T) {
	msg := decode(t, `{"content":[{"type":"text","text":"héllo"},{"type":"tool_use","input":{}},{"type":"text","text":"wörld!"}],"stop_reason":"end_turn","usage":{"output_tokens":12345678901234567890}}`)
	if !truncateMessage(8)(msg) {
		t.Fatal("expected truncation")
	}
	blocks := msg["content"].([]any)
	if blocks[0].(map[string]any)["text"] != "héllo" || blocks[2].(map[string]any)["text"] != "wör" {
		t.Fatalf("unexpected text: %v", blocks)
	}
	if msg["stop_reason"] != "max_tokens" {
		t.Fatalf("stop_reason: %v", msg["stop_reason"])
	}
	out, _ := json.Marshal(msg)
	if !strings.Contains(string(out), "12345678901234567890") {
		t.Fatalf("numbers outside the rewritten fields must survive untouched: %s", out)
	}

	short := decode(t, `{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`)
	if truncateMessage(50)(short) || short["stop_reason"] != "end_turn" {
		t.Fatal("a message within the limit must be left alone")
	}
}

func TestTruncateStreamBoundary(t *testing.T) {
	delta := func(text string) map[string]any {
		return decode(t, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+text+`"}}`)
	}
	ts := &truncateStream{max: 10}
	if out, done := ts.event([]byte("raw1"), delta("12345")); string(out) != "raw1" || done {
		t.Fatal("a delta within the budget passes through untouched")
	}
	if out, done := ts.event([]byte("raw2"), delta("67890")); string(out) != "raw2" || done {
		t.Fatal("reaching the limit exactly does not end the stream yet")
	}
	if out, done := ts.event([]byte("ping"), decode(t, `{"type":"ping"}`)); string(out) != "ping" || done {
		t.Fatal("other events pass through")
	}
	out, done := ts.event([]byte("raw3"), delta("more"))
	if !done {
		t.Fatal("text beyond the limit ends the stream")
	}
	s := string(out)
	if strings.Contains(s, "more") || strings.Contains(s, "text_delta") {
		t.Fatalf("no text beyond the limit may be sent: %s", s)
	}
	order := []string{"event: content_block_stop", "event: message_delta", `"stop_reason":"max_tokens"`, "event: message_stop"}
	pos := -1
	for _, want := range order {
		i := strings.Index(s, want)
		if i <= pos {
			t.Fatalf("closing events missing or out of order (%q): %s", want, s)
		}
		pos = i
	}

	ts = &truncateStream{max: 7}
	ts.event(nil, delta("12345"))
	out, done = ts.event(nil, delta("67890"))
	if !done || !strings.Contains(string(out), `"text":"67"`) {
		t.Fatalf("a delta crossing the limit is cut to fit: %s", out)
	}
}

func TestMalformStreamPerBlock(t *testing.T) {
	m := &malformStream{seen: map[any]bool{}}
	ev := func(index, chunk string) string {
		out, _ := m.event(nil, decode(t, `{"type":"content_block_delta","index":`+index+`,"delta":{"type":"input_json_delta","partial_json":"`+chunk+`"}}`))
		return eventData(out)["delta"].(map[string]any)["partial_json"].(string)
	}
	got := map[string]string{}
	got["0"] += ev("0", "{")
	got["1"] += ev("1", "{")
	got["0"] += ev("0", "}")
	got["1"] += ev("1", "}")
	for idx, joined := range got {
		var v any
		if json.Unmarshal([]byte(joined), &v) == nil {
			t.Fatalf("block %s: concatenated input %q must not parse", idx, joined)
		}
	}
	if out, done := m.event([]byte("raw"), decode(t, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`)); string(out) != "raw" || done {
		t.Fatal("text deltas pass through")
	}
}
