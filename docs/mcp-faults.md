# MCP tool faults (M8)

This is the contract for milestone M8. The PRD names the milestone ("MCP (Streamable HTTP) tool faults") and gives it no contract, so this document is the specification the implementation and its tests are held to. It adds four fault types and one endpoint on the demo. It changes no existing contract: every key, type, endpoint and behaviour in `PRD.md` stays as it is.

## Scope

The faults act on **MCP tool calls carried over the Streamable HTTP transport**, between an MCP client (usually an agent) and an MCP server reached through a Faultline route.

In scope: a `tools/call` request sent as an HTTP `POST` whose body is one JSON-RPC request object, answered by the server either with `Content-Type: application/json` (one JSON-RPC response) or with `Content-Type: text/event-stream` (an SSE stream that carries the response, possibly after notifications).

Out of scope, passed through untouched: every other MCP message (`initialize`, `tools/list`, `ping`, notifications, client responses), the `GET` stream a client may open for server-initiated messages, session management (`Mcp-Session-Id` is forwarded like any header), JSON-RPC batches (removed from MCP in revision 2025-06-18), the stdio transport, and the deprecated HTTP+SSE transport.

## What makes a request an MCP tool call

A request is a tool call when all of these hold:

1. the method is `POST`;
2. the body is at most 1 MiB and is a single JSON object;
3. that object has `"jsonrpc": "2.0"`, `"method": "tools/call"`, an `id` that is a string or a number, and `params.name` that is a string.

For an MCP fault, being a tool call is **part of the match**, next to route, `match.method`, `match.path`, the time window and scope (PRD 8.3). A request that is not a tool call does not match an MCP fault: it is not counted by the fault's strategy, and matching continues with the next fault in the experiment, exactly as if the path had not matched. The optional `tool` parameter narrows the match further to calls whose `params.name` equals it.

To decide this the proxy reads up to 1 MiB of the request body before forwarding it. It does so only for a request that already matches an MCP fault on everything else (active run, scope, route, method, path, window), and it then forwards the bytes unchanged. Nothing else about request or response streaming changes, and no body is stored or logged. This is the one place where Faultline looks inside a request body.

## Fault types

All four accept the optional parameter `tool` (non-empty string). Unknown parameters are a validation error, as for every fault type.

| type | params | behaviour |
|---|---|---|
| `mcp_tool_error` | `kind`: `tool` (default) or `protocol`; `message` (default `faultline injected`); `code` (integer, default `-32603`, only with `kind: protocol`) | Answers at once without calling the upstream, HTTP 200, `Content-Type: application/json`. `kind: tool`: a tool execution error, `{"jsonrpc":"2.0","id":<id>,"result":{"content":[{"type":"text","text":"<message>"}],"isError":true}}`. `kind: protocol`: a JSON-RPC error, `{"jsonrpc":"2.0","id":<id>,"error":{"code":<code>,"message":"<message>"}}`. `<id>` is the request's id, with its JSON type preserved. |
| `mcp_slow_tool` | `delay_ms` (required, integer >= 0) | Waits `delay_ms`, then forwards the call. The response is untouched. |
| `mcp_malformed_result` | `mode`: `invalid_json` (default) or `schema` | Forwards the call and replaces the JSON-RPC response to it. `invalid_json`: the response message becomes the text `{"jsonrpc":"2.0","id":<id>,"result":{"content":[{"type":"text","text":"de`, which is not valid JSON. `schema`: the message stays valid JSON-RPC but its `result` becomes `{"content":"faultline injected"}`, which violates the tool result schema (`content` must be an array). |
| `mcp_stream_drop` | `after_events` (integer >= 0, default 0) | Forwards the call. If the response is an SSE stream: forwards complete events until `after_events` have been sent or the event carrying the response to this call arrives, whichever comes first, then closes the connection abruptly. The response to the call never reaches the client. If the response is `application/json`: forwards status and headers and closes abruptly without any body byte. |

Details that hold for all of them:

- A response the proxy produces or alters carries `X-Faultline-Fault: <fault id>` when `expose_fault_header` is true, except where no response reaches the client at all.
- `mcp_malformed_result` and `mcp_stream_drop` act only on a 200 response that is `application/json` or `text/event-stream`. Anything else (an upstream error, a `202 Accepted`, a compressed body) is relayed untouched; the fault still counts as triggered. For these two types the proxy asks the upstream for an uncompressed body.
- In an SSE stream, events other than the response to the faulted call (notifications, requests from the server, responses to other calls) pass through byte for byte and are flushed as they arrive. `mcp_malformed_result` rewrites exactly one event: the first whose data is a JSON-RPC message with a `result` or `error` and the call's `id`. The rewritten event keeps the upstream's `event:` and `id:` lines; only its `data:` changes.
- With `mode: invalid_json` on an `application/json` response the `Content-Length` is that of the replacement text.
- Clearing the faults (kill switch, lease expiry, end of run) ends a pending `mcp_slow_tool` delay at once and the call is forwarded, like `latency` (DECISIONS D25).
- Request records: `fault_type` is the MCP type. `status` is the HTTP status the client got (200 for `mcp_tool_error`), and 0 for `mcp_stream_drop` when the connection was cut, as for every cut response.

## Validation rules

Enforced by `faultline validate`, `POST /api/v1/experiments/validate`, run creation and the proxy admin API, with errors as `{path, message}`:

| path | rule |
|---|---|
| `faults[i].params.tool` | if present, a non-empty string |
| `faults[i].params.kind` | `tool` or `protocol` |
| `faults[i].params.message` | a string |
| `faults[i].params.code` | an integer; only allowed with `kind: protocol` |
| `faults[i].params.delay_ms` | required; an integer >= 0 |
| `faults[i].params.mode` | `invalid_json` or `schema` |
| `faults[i].params.after_events` | an integer >= 0 |
| `faults[i].params.<other>` | unknown parameter for this fault type |

## Example

```yaml
version: 1
name: stock-tool-fails
description: The stock lookup tool fails; the agent should report it, not invent an answer
target:
  route: llm
duration: 20s
faults:
  - id: tool-error
    type: mcp_tool_error
    match: {method: POST, path: /mcp}
    strategy: {kind: probability, p: 0.5}
    params:
      tool: lookup_stock
      message: stock database unavailable
```

## Demo MCP server

`faultline-demo` serves a small MCP server at `/mcp` on its existing `llm` server (default `127.0.0.1:9003`), so a route whose upstream is the `llm` server reaches it at `/{route}/mcp`. No flag and no server is added to the demo.

It speaks Streamable HTTP, protocol revision `2025-06-18` (it also accepts `2025-03-26` in `initialize`), without sessions: it issues no `Mcp-Session-Id` and ignores one if sent. Data is synthetic.

| request | answer |
|---|---|
| `POST /mcp`, `initialize` | 200 JSON: `protocolVersion` (the requested one if supported, else `2025-06-18`), `capabilities: {"tools":{}}`, `serverInfo: {"name":"faultline-demo","version":"0.1.0"}` |
| `POST /mcp`, any notification (no `id`) | 202, no body |
| `POST /mcp`, `ping` | 200 JSON, `result: {}` |
| `POST /mcp`, `tools/list` | 200 JSON, the two tools below with their input schemas |
| `POST /mcp`, `tools/call` `lookup_stock`, arguments `{"id": "<string>"}` | 200 `application/json`: `result.content` is one text block holding `{"id":"<id>","stock":42}`, `isError: false`. A missing or non-string `id` gives a result with `isError: true`. |
| `POST /mcp`, `tools/call` `slow_report`, arguments `{"steps": <1..10, default 3>}` | 200 `text/event-stream`, headers flushed at once: one `notifications/message` event per step, 50 ms apart, with `params.data` `step <n> of <steps>`; then the response event, `result.content` one text block `report complete after <steps> steps`, `isError: false`. Every event is `event: message` with one `data:` line. |
| `POST /mcp`, `tools/call` with an unknown tool | 200 JSON, JSON-RPC error `-32602` |
| `POST /mcp`, unknown method | 200 JSON, JSON-RPC error `-32601` |
| `POST /mcp`, a JSON array (batch) or an object that is not a JSON-RPC 2.0 message | 400 JSON, JSON-RPC error `-32600`, `id: null` |
| `POST /mcp`, body that is not JSON | 400 JSON, JSON-RPC error `-32700`, `id: null` |
| `GET /mcp`, `DELETE /mcp` | 405 |

## Gate

Same standard as M4: real `faultline proxy`, `faultline control` and `faultline-demo` processes on free ports, nothing mocked.

- each of the four fault types against the demo MCP server, for a tool that answers with JSON and a tool that answers with a stream;
- every parameter and default, and every validation rule above;
- matching: only `tools/call` is touched (`initialize`, `tools/list`, notifications and non-JSON bodies pass), the `tool` filter, the strategy counting only tool calls, fall-through to a later fault for a request that is not a tool call, scoping;
- streams stay unbuffered through `mcp_malformed_result` (time to first event);
- an MCP fault installed by a real run through control.
