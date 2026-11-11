package demo

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	streamDeltas     = 20
	streamDeltaChars = 20
	streamInterval   = 50 * time.Millisecond
)

// FixedText is the 400 character completion every text response carries.
var FixedText = strings.Repeat("Synthetic demo completion text for Faultline. ", 9)[:streamDeltas*streamDeltaChars]

type llmRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
	Tools  []struct {
		Name string `json:"name"`
	} `json:"tools"`
}

func (a *app) llm() http.Handler {
	mux := http.NewServeMux()
	health(mux)
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		var req llmRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"type":  "error",
				"error": map[string]string{"type": "invalid_request_error", "message": "request body is not valid JSON"},
			})
			return
		}
		if req.Model == "" {
			req.Model = "demo-model"
		}
		switch {
		case req.Stream && len(req.Tools) > 0:
			streamToolUse(w, r, req)
		case req.Stream:
			streamText(w, r, req)
		default:
			writeJSON(w, http.StatusOK, message(req))
		}
	})
	return mux
}

func message(req llmRequest) map[string]any {
	msg := map[string]any{
		"id":            "msg_demo_0001",
		"type":          "message",
		"role":          "assistant",
		"model":         req.Model,
		"stop_sequence": nil,
		"usage":         map[string]int{"input_tokens": 12, "output_tokens": 100},
	}
	if len(req.Tools) > 0 {
		msg["content"] = []any{map[string]any{
			"type": "tool_use", "id": "toolu_demo_0001", "name": req.Tools[0].Name,
			"input": map[string]string{"query": "demo"},
		}}
		msg["stop_reason"] = "tool_use"
	} else {
		msg["content"] = []any{map[string]string{"type": "text", "text": FixedText}}
		msg["stop_reason"] = "end_turn"
	}
	return msg
}

type sse struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func newSSE(w http.ResponseWriter) *sse {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	return &sse{w: w, rc: http.NewResponseController(w)}
}

func (s *sse) send(event string, data any) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, b)
	s.rc.Flush()
}

func (s *sse) start(req llmRequest) {
	s.send("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_demo_0001", "type": "message", "role": "assistant", "model": req.Model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]int{"input_tokens": 12, "output_tokens": 1},
		},
	})
}

func (s *sse) finish(stopReason string) {
	s.send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	s.send("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]int{"output_tokens": 100},
	})
	s.send("message_stop", map[string]string{"type": "message_stop"})
}

// pause waits between deltas and reports false when the client has gone.
func pause(r *http.Request) bool {
	select {
	case <-time.After(streamInterval):
		return true
	case <-r.Context().Done():
		return false
	}
}

func streamText(w http.ResponseWriter, r *http.Request, req llmRequest) {
	s := newSSE(w)
	s.start(req)
	s.send("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]string{"type": "text", "text": ""},
	})
	for i := 0; i < streamDeltas; i++ {
		if !pause(r) {
			return
		}
		s.send("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]string{"type": "text_delta", "text": FixedText[i*streamDeltaChars : (i+1)*streamDeltaChars]},
		})
	}
	s.finish("end_turn")
}

func streamToolUse(w http.ResponseWriter, r *http.Request, req llmRequest) {
	s := newSSE(w)
	s.start(req)
	s.send("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{
			"type": "tool_use", "id": "toolu_demo_0001", "name": req.Tools[0].Name, "input": map[string]any{},
		},
	})
	for _, chunk := range []string{`{"que`, `ry":"`, `demo`, `"}`} {
		if !pause(r) {
			return
		}
		s.send("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]string{"type": "input_json_delta", "partial_json": chunk},
		})
	}
	s.finish("tool_use")
}
