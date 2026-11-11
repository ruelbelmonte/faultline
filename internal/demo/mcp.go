package demo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// A small MCP server over Streamable HTTP, at /mcp on the llm server. Its
// contract is docs/mcp-faults.md. Stateless, synthetic data.

const (
	mcpVersion       = "2025-06-18"
	mcpVersionLegacy = "2025-03-26"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

var mcpTools = []map[string]any{
	{
		"name": "lookup_stock", "description": "Returns the stock level of a product.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]string{"type": "string"}}, "required": []string{"id"}},
	},
	{
		"name": "slow_report", "description": "Builds a report step by step and streams its progress.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"steps": map[string]any{"type": "integer", "minimum": 1, "maximum": 10}}},
	},
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{"content": []any{map[string]string{"type": "text", "text": text}}, "isError": isError}
}

func mcp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	null := json.RawMessage("null")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil || !json.Valid(body) {
		writeJSON(w, http.StatusBadRequest, rpcResponse{JSONRPC: "2.0", ID: null, Error: &rpcError{-32700, "parse error"}})
		return
	}
	var req rpcRequest
	if json.Unmarshal(body, &req) != nil || req.JSONRPC != "2.0" || req.Method == "" {
		writeJSON(w, http.StatusBadRequest, rpcResponse{JSONRPC: "2.0", ID: null, Error: &rpcError{-32600, "invalid request"}})
		return
	}
	if len(req.ID) == 0 || bytes.Equal(req.ID, null) {
		// A notification: accepted, never answered.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	reply := func(result any) { writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}) }
	fail := func(code int, message string) {
		writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{code, message}})
	}

	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		version := mcpVersion
		if p.ProtocolVersion == mcpVersionLegacy {
			version = mcpVersionLegacy
		}
		reply(map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "faultline-demo", "version": "0.1.0"},
		})
	case "ping":
		reply(map[string]any{})
	case "tools/list":
		reply(map[string]any{"tools": mcpTools})
	case "tools/call":
		var p struct {
			Name      string `json:"name"`
			Arguments struct {
				ID    any  `json:"id"`
				Steps *int `json:"steps"`
			} `json:"arguments"`
		}
		json.Unmarshal(req.Params, &p)
		switch p.Name {
		case "lookup_stock":
			id, ok := p.Arguments.ID.(string)
			if !ok {
				reply(toolResult("argument id must be a string", true))
				return
			}
			text, _ := json.Marshal(map[string]any{"id": id, "stock": 42})
			reply(toolResult(string(text), false))
		case "slow_report":
			steps := 3
			if p.Arguments.Steps != nil {
				steps = *p.Arguments.Steps
			}
			if steps < 1 || steps > 10 {
				reply(toolResult("argument steps must be between 1 and 10", true))
				return
			}
			streamReport(w, r, req.ID, steps)
		default:
			fail(-32602, "unknown tool: "+p.Name)
		}
	default:
		fail(-32601, "method not found: "+req.Method)
	}
}

// streamReport answers a tool call with an event stream: one log
// notification per step, then the response.
func streamReport(w http.ResponseWriter, r *http.Request, id json.RawMessage, steps int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	rc.Flush()
	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
		rc.Flush()
	}
	for step := 1; step <= steps; step++ {
		if !pause(r) {
			return
		}
		send(map[string]any{
			"jsonrpc": "2.0", "method": "notifications/message",
			"params": map[string]any{"level": "info", "logger": "slow_report", "data": fmt.Sprintf("step %d of %d", step, steps)},
		})
	}
	send(rpcResponse{JSONRPC: "2.0", ID: id, Result: toolResult(fmt.Sprintf("report complete after %d steps", steps), false)})
}
