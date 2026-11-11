# Faults

A fault is one entry in an experiment's `faults` list:

```yaml
faults:
  - id: f1                    # unique within the experiment
    type: http_error
    route: inventory          # optional, defaults to target.route
    match:
      method: GET             # optional
      path: /stock/*          # optional glob on the path after the route prefix
    strategy:
      kind: probability       # always | probability | burst | every_nth
      p: 0.3
    start: 0s                 # offset from the start of injection
    duration: 30s             # optional, default: until injection ends
    params:
      status: 503
```

## When a fault applies

A request is a candidate for a fault when all of these hold:

- a run is injecting;
- the scope matches: with `scope: header` the request's scope header equals the run id, with `scope: all` every request on the route qualifies;
- the route matches;
- `match.method` equals the request method, if set;
- `match.path` matches the path after the route prefix, if set. `*` matches any run of characters including `/`, `?` matches one character. The query string is not part of the path;
- the current time is inside the fault's window.

**The first fault in file order that matches is the fault for that request.** Its strategy then decides whether the request is actually faulted. If it is not, the request passes through; later faults are not tried. Give two faults on the same route different matches if both should see traffic.

## Strategies

Each fault counts its matching requests: k = 1, 2, 3, ...

| kind | Parameter | Faulted when |
|---|---|---|
| `always` | none | every match |
| `probability` | `p` in (0, 1] | with probability p. Reproducible when the experiment has a `seed` |
| `burst` | `count` >= 1 | k <= count |
| `every_nth` | `n` >= 1 | k is a multiple of n |

`triggers` is the number of requests actually faulted. A fault with zero triggers makes the run `inconclusive`.

## Generic faults

| type | params | Behaviour |
|---|---|---|
| `latency` | `ms`, `jitter_ms` (default 0) | Waits `ms` plus or minus a uniform jitter, then forwards the request. |
| `http_error` | `status` (200 to 599), `body` (default `{"error":"faultline injected"}`), `headers` (map), `retry_after` (seconds) | Answers at once without calling the upstream. Sets `Retry-After` when given. `Content-Type` is `application/json` unless `headers` says otherwise. |
| `error_in_body` | `body` (required), `content_type` (default `application/json`) | Answers 200 with that body without calling the upstream. |
| `timeout` | `hold` (default `30s`) | Accepts the request and sends nothing. After `hold` it closes the connection with no response. Records how long the caller stayed connected. |
| `reset` | none | Closes the TCP connection at once with a reset. No response. |
| `slow_body` | `bytes_per_sec` | Forwards the upstream response, throttled to that rate. |
| `corrupt_body` | `mode: truncate` with `at_bytes`, or `mode: garble` | truncate: forwards exactly `at_bytes` body bytes, then closes abruptly. A body shorter than `at_bytes` is left whole. garble: forwards the full length with every seventh byte flipped, reproducibly for a given seed. |

## LLM faults

These are meant for a route whose upstream speaks the Anthropic Messages API (`POST /v1/messages`) or OpenAI chat completions, streaming or not. The table describes the Messages API format; the chat completions format follows it.

| type | params | Behaviour |
|---|---|---|
| `llm_rate_limit` | `retry_after` (default 5) | 429 with a `retry-after` header and body `{"type":"error","error":{"type":"rate_limit_error","message":"faultline injected"}}`. The upstream is not called. |
| `llm_overloaded` | none | 529 with body `{"type":"error","error":{"type":"overloaded_error","message":"faultline injected"}}`. The upstream is not called. |
| `llm_stream_cut` | `after_bytes` | Forwards exactly `after_bytes` bytes of the response body, then closes abruptly. No `message_stop` reaches the client. |
| `llm_slow_first_token` | `delay_ms` | Forwards status and headers at once, then waits before the first body byte. |
| `llm_truncate` | `max_chars` (default 50) | Non-streaming: text content is cut to `max_chars` characters and `stop_reason` becomes `max_tokens`. Streaming: text deltas are forwarded until `max_chars` in total, then well-formed `content_block_stop`, `message_delta` (`stop_reason: max_tokens`) and `message_stop` events end the stream. A response already within the limit is not altered. |
| `llm_malformed_tool_call` | none | Non-streaming: each `tool_use` block's `input` becomes the string `{"query": "de`. Streaming: the `input_json_delta` values of each tool call no longer concatenate to valid JSON. Responses without a tool call are not altered. |

### OpenAI chat completions format

The same six fault types answer in OpenAI format when the request path ends in `/chat/completions` (after the route prefix, so `/v1/chat/completions` and Azure-style deployment paths both qualify). Every other path gets the Messages API behaviour above. There is nothing to configure: one fault on one route serves both APIs, each in its own format.

| type | Behaviour on a chat completions request |
|---|---|
| `llm_rate_limit` | 429 with a `retry-after` header and body `{"error":{"message":"faultline injected","type":"rate_limit_error","param":null,"code":"rate_limit_exceeded"}}`. |
| `llm_overloaded` | 503 (OpenAI has no 529) with body `{"error":{"message":"faultline injected","type":"server_error","param":null,"code":"service_unavailable"}}`. |
| `llm_stream_cut` | As above: exactly `after_bytes` bytes, then an abrupt close. No `data: [DONE]` reaches the client. |
| `llm_slow_first_token` | As above. |
| `llm_truncate` | Non-streaming: each choice's `message.content` longer than `max_chars` is cut to it and that choice's `finish_reason` becomes `length`. Streaming: content deltas are forwarded until a choice has carried `max_chars` characters; then every choice still open gets a chunk with an empty delta and `finish_reason: "length"`, `data: [DONE]` follows and the stream ends. A response within the limit is not altered. |
| `llm_malformed_tool_call` | Non-streaming: the `function.arguments` of every tool call becomes `{"query": "de`. Streaming: per tool call, the first chunk that carries `arguments` gets `{"query": "de` and the later ones an empty string, so the concatenation is not valid JSON. Responses without tool calls are not altered. |

Records and `X-Faultline-Fault` behave the same in both formats; the record `status` for `llm_overloaded` is 503 here and 529 for the Messages API.

`llm_truncate` and `llm_malformed_tool_call` rewrite only a 200 response that is `text/event-stream` or JSON. Upstream errors and compressed bodies are passed through as they are; the proxy asks the upstream for an uncompressed body for these two faults.

## MCP tool faults

`mcp_tool_error`, `mcp_slow_tool`, `mcp_malformed_result` and `mcp_stream_drop` act on MCP `tools/call` requests over Streamable HTTP and on nothing else. Their parameters, validation rules and exact behaviour, and the MCP server in `faultline-demo` that they are tested against, are specified in [mcp-faults.md](mcp-faults.md).

| type | params | In one line |
|---|---|---|
| `mcp_tool_error` | `kind` (`tool`, `protocol`), `message`, `code`, `tool` | Answers the call with a tool execution error or a JSON-RPC error, without calling the server. |
| `mcp_slow_tool` | `delay_ms`, `tool` | Delays the call. |
| `mcp_malformed_result` | `mode` (`invalid_json`, `schema`), `tool` | Replaces the response to the call with broken JSON or with a result that violates the tool result schema. |
| `mcp_stream_drop` | `after_events`, `tool` | Cuts the response stream before the result arrives. |

For these four types, being a tool call is part of the match: any other request on the route passes them by, is not counted by their strategy, and can still match a later fault.

## What the client sees

| Fault | Status | `X-Faultline-Fault` | Record `status` |
|---|---|---|---|
| `latency`, `slow_body`, `corrupt_body` garble, `llm_slow_first_token`, `llm_truncate`, `llm_malformed_tool_call` | upstream's | yes | upstream's |
| `http_error` | `status` | yes | `status` |
| `error_in_body` | 200 | yes | 200 |
| `llm_rate_limit`, `llm_overloaded` | 429, 529 | yes | 429, 529 |
| `corrupt_body` truncate, `llm_stream_cut` | upstream's, then the connection is cut | yes | 0 when a cut happened |
| `timeout`, `reset` | none | no | 0 |

`X-Faultline-Fault: <fault id>` is added when `proxy.expose_fault_header` is true (the default). Turn it off if the system under test must not be able to tell a fault from the real thing.

## When faults end

A fault stops applying at the end of its window, when the run ends, when the kill switch is used, or when the proxy's lease runs out. Whatever was in flight is released at that moment: held connections are closed, pending delays end and the request is forwarded, throttled bodies continue at full speed.
