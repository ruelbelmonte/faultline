package experiment

import (
	"strings"
	"testing"
)

// The validation rules of docs/mcp-faults.md.
func TestMCPFaultParams(t *testing.T) {
	template := `version: 1
name: params
target: {route: llm}
duration: 10s
faults:
  - id: f1
    type: TYPE
    params: PARAMS
`
	parse := func(typ, params string) (*Experiment, error) {
		exp, errs := Parse([]byte(strings.NewReplacer("TYPE", typ, "PARAMS", params).Replace(template)), testConfig())
		if len(errs) > 0 {
			return nil, errs
		}
		return exp, nil
	}

	valid := []struct{ typ, params string }{
		{"mcp_tool_error", "{}"},
		{"mcp_tool_error", "{kind: tool, message: stock database unavailable, tool: lookup_stock}"},
		{"mcp_tool_error", "{kind: protocol}"},
		{"mcp_tool_error", "{kind: protocol, code: -32000, message: busy}"},
		{"mcp_slow_tool", "{delay_ms: 0}"},
		{"mcp_slow_tool", "{delay_ms: 1500, tool: slow_report}"},
		{"mcp_malformed_result", "{}"},
		{"mcp_malformed_result", "{mode: invalid_json}"},
		{"mcp_malformed_result", "{mode: schema, tool: lookup_stock}"},
		{"mcp_stream_drop", "{}"},
		{"mcp_stream_drop", "{after_events: 3, tool: slow_report}"},
	}
	for _, tc := range valid {
		if _, err := parse(tc.typ, tc.params); err != nil {
			t.Errorf("%s %s: expected valid, got %v", tc.typ, tc.params, err)
		}
	}

	invalid := []struct{ typ, params, path string }{
		{"mcp_tool_error", "{kind: fatal}", "faults[0].params.kind"},
		{"mcp_tool_error", "{message: 5}", "faults[0].params.message"},
		{"mcp_tool_error", "{code: -32000}", "faults[0].params.code"},
		{"mcp_tool_error", "{kind: tool, code: -32000}", "faults[0].params.code"},
		{"mcp_tool_error", "{kind: protocol, code: 1.5}", "faults[0].params.code"},
		{"mcp_tool_error", "{kind: protocol, code: abc}", "faults[0].params.code"},
		{"mcp_tool_error", `{tool: ""}`, "faults[0].params.tool"},
		{"mcp_tool_error", "{tool: 7}", "faults[0].params.tool"},
		{"mcp_tool_error", "{delay_ms: 5}", "faults[0].params.delay_ms"},
		{"mcp_slow_tool", "{}", "faults[0].params.delay_ms"},
		{"mcp_slow_tool", "{delay_ms: -1}", "faults[0].params.delay_ms"},
		{"mcp_slow_tool", "{delay_ms: 1.5}", "faults[0].params.delay_ms"},
		{"mcp_slow_tool", `{delay_ms: 5, tool: ""}`, "faults[0].params.tool"},
		{"mcp_slow_tool", "{delay_ms: 5, mode: schema}", "faults[0].params.mode"},
		{"mcp_malformed_result", "{mode: garble}", "faults[0].params.mode"},
		{"mcp_malformed_result", "{kind: tool}", "faults[0].params.kind"},
		{"mcp_malformed_result", `{tool: ""}`, "faults[0].params.tool"},
		{"mcp_stream_drop", "{after_events: -1}", "faults[0].params.after_events"},
		{"mcp_stream_drop", "{after_events: soon}", "faults[0].params.after_events"},
		{"mcp_stream_drop", "{after_bytes: 10}", "faults[0].params.after_bytes"},
		{"mcp_stream_drop", `{tool: ""}`, "faults[0].params.tool"},
		// The tool parameter belongs to MCP faults only.
		{"http_error", "{status: 500, tool: lookup_stock}", "faults[0].params.tool"},
	}
	for _, tc := range invalid {
		doc := strings.NewReplacer("TYPE", tc.typ, "PARAMS", tc.params).Replace(template)
		_, errs := Parse([]byte(doc), testConfig())
		if !hasPath(errs, tc.path) {
			t.Errorf("%s %s: expected an error at %q, got %v", tc.typ, tc.params, tc.path, errs)
		}
	}

	exp, err := parse("mcp_tool_error", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if p := exp.Faults[0].Typed; p.Kind != "tool" || p.Message != "faultline injected" || p.Code != -32603 || p.Tool != "" {
		t.Errorf("mcp_tool_error defaults: %+v", p)
	}
	exp, _ = parse("mcp_malformed_result", "{}")
	if exp.Faults[0].Typed.Mode != "invalid_json" {
		t.Errorf("mcp_malformed_result default mode: %q", exp.Faults[0].Typed.Mode)
	}
	exp, _ = parse("mcp_stream_drop", "{}")
	if exp.Faults[0].Typed.AfterEvents != 0 {
		t.Errorf("mcp_stream_drop default after_events: %d", exp.Faults[0].Typed.AfterEvents)
	}
}
