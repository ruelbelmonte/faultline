package demo

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// An OpenAI-compatible chat completions mock, next to the Messages API mock
// on the llm server. Same text, same pacing, synthetic ids.

const (
	chatID      = "chatcmpl-demo-0001"
	chatCallID  = "call_demo_0001"
	chatCreated = 1767225600
)

type chatRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
	Tools  []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

func chatCompletions(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "request body is not valid JSON", "type": "invalid_request_error", "param": nil, "code": nil},
		})
		return
	}
	if req.Model == "" {
		req.Model = "demo-model"
	}
	switch {
	case req.Stream && len(req.Tools) > 0:
		streamChatToolCall(w, r, req)
	case req.Stream:
		streamChatText(w, r, req)
	default:
		writeJSON(w, http.StatusOK, chatCompletion(req))
	}
}

func chatCompletion(req chatRequest) map[string]any {
	message := map[string]any{"role": "assistant", "content": FixedText, "refusal": nil}
	finish := "stop"
	if len(req.Tools) > 0 {
		message["content"] = nil
		message["tool_calls"] = []any{map[string]any{
			"id": chatCallID, "type": "function",
			"function": map[string]string{"name": req.Tools[0].Function.Name, "arguments": `{"query":"demo"}`},
		}}
		finish = "tool_calls"
	}
	return map[string]any{
		"id": chatID, "object": "chat.completion", "created": chatCreated, "model": req.Model,
		"choices": []any{map[string]any{"index": 0, "message": message, "logprobs": nil, "finish_reason": finish}},
		"usage":   map[string]int{"prompt_tokens": 12, "completion_tokens": 100, "total_tokens": 112},
	}
}

type chatStream struct {
	w     http.ResponseWriter
	rc    *http.ResponseController
	model string
}

func newChatStream(w http.ResponseWriter, req chatRequest) *chatStream {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	return &chatStream{w: w, rc: http.NewResponseController(w), model: req.Model}
}

// chunk sends one chat.completion.chunk; finish is nil until the last one.
func (s *chatStream) chunk(delta map[string]any, finish any) {
	b, _ := json.Marshal(map[string]any{
		"id": chatID, "object": "chat.completion.chunk", "created": chatCreated, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "logprobs": nil, "finish_reason": finish}},
	})
	fmt.Fprintf(s.w, "data: %s\n\n", b)
	s.rc.Flush()
}

func (s *chatStream) done(finish string) {
	s.chunk(map[string]any{}, finish)
	fmt.Fprint(s.w, "data: [DONE]\n\n")
	s.rc.Flush()
}

func streamChatText(w http.ResponseWriter, r *http.Request, req chatRequest) {
	s := newChatStream(w, req)
	s.chunk(map[string]any{"role": "assistant", "content": ""}, nil)
	for i := 0; i < streamDeltas; i++ {
		if !pause(r) {
			return
		}
		s.chunk(map[string]any{"content": FixedText[i*streamDeltaChars : (i+1)*streamDeltaChars]}, nil)
	}
	s.done("stop")
}

func streamChatToolCall(w http.ResponseWriter, r *http.Request, req chatRequest) {
	s := newChatStream(w, req)
	s.chunk(map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
		"index": 0, "id": chatCallID, "type": "function",
		"function": map[string]string{"name": req.Tools[0].Function.Name, "arguments": ""},
	}}}, nil)
	for _, part := range []string{`{"que`, `ry":"`, `demo`, `"}`} {
		if !pause(r) {
			return
		}
		s.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]string{"arguments": part}}}}, nil)
	}
	s.done("tool_calls")
}
