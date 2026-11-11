# Decisions

Each entry: the decision, the alternatives, the reason. Entries are grouped by area, not by date.

## Build and tooling

**D1. Module path is `faultline`.**
Alternatives: a `github.com/<owner>/faultline` path. Reason: the PRD names no hosting location and inventing an owner would be wrong. Rename with one search and replace when the repo gets a home.

**D2. Go toolchain lives in `.tools/` on the build machine.**
The build machine had no Go. A checksum-verified Go 1.27.4 archive was unpacked into `.tools/` (git-ignored) instead of installing system-wide. Nothing in the repo depends on it.

**D3. `-race` gates run in a Linux container.**
The race detector needs cgo and a C compiler, which the Windows build machine lacks. `go test -race` runs in `golang:1.27` with the repo mounted. Non-race unit and integration tests also run natively on Windows, so both platforms are exercised.

**D4. Integration tests sit behind the `integration` build tag in `test/integration/`.**
Alternatives: `testing.Short()`. Reason: `make test` stays fast and `make test-integration` is explicit. They are never skipped at run time.

## Dependencies

| Module | Why |
|---|---|
| `go.yaml.in/yaml/v3` | The standard library has no YAML parser. The node API is needed for unknown-key detection, dotted error paths and in-place redaction. This is the maintained continuation of `gopkg.in/yaml.v3`. |
| `modernc.org/sqlite` | Mandated by the PRD (pure Go, no cgo). |

UI build-time dependencies (not shipped in the Go binary other than as built assets): `vue`, `vite`, `typescript`, `@vitejs/plugin-vue`, `vue-tsc`, `uplot`, `@playwright/test`. All named or implied by PRD sections 4 and 14.

## Configuration

**D5. Relative `data_dir` and `experiments_dir` resolve against the working directory of the process.**
Alternative: relative to the config file. Reason: simplest reading, and the same as every path flag.

**D6. An `environment` other than `local`, `ci`, `qa` is a validation error (exit 2).** The three production names trip the guard with their own message before any other validation.

**D7. `redaction.store_bodies: true` is a validation error.** The key is kept for forward compatibility, but accepting `true` and silently not storing bodies would mislead.

## Experiments

**D8. Path globs: `*` matches any run of characters including `/`; `?` matches one character.**
Alternative: `path.Match`, where `*` stops at `/`. Reason: `/stock/*` reads as "everything under /stock" to most people.

**D9. A fault with no `strategy` uses `always`. A fault with no `duration` runs until the injection phase ends.**

**D10. `traffic.rps` and `traffic.base_url` are required with `requests`; `concurrency` defaults to 1, `timeout` to 30s.** `precheck` defaults to 3 and 15s, `recovery.timeout` to 30s, probes to 1s interval, 2s timeout, status 200.

**D11. No validation rules beyond the PRD.** For example a hypothesis threshold with no data source is accepted and evaluates to `observed: null`, which fails, as section 9.4 says.

## Redaction

**D12. The persisted experiment copy is redacted in place.** Header values under `traffic.requests[].headers` and `faults[].params.headers` whose name is in `redaction.headers` become `[REDACTED]`. If nothing needed redacting the stored bytes are identical to what was submitted.

**D12a. YAML aliases and merge keys are a validation error in config and experiment files.**
Alternative: resolve them before redacting. Reason: they let a value appear under a different key after decoding than in the raw document, so redaction of the stored copy could miss a sensitive header (found by security review of M0). Rejecting them keeps "what is validated" and "what is redacted" the same tree. `Redact` also fails closed: a document that does not parse or contains either construct is stored as a placeholder comment.

**D13. `yaml_sha256` is the hash of the persisted (redacted) copy.**
Alternative: hash of the submitted bytes. Reason: a hash of a document containing a low-entropy secret can be brute-forced; hashing the redacted copy cannot leak it. For experiments without sensitive headers the hash equals `sha256sum` of the file.

## CLI

**D14. `faultline validate` prints results to stdout**, one `file: valid|invalid` line per file followed by one `path: message` line per error. Problems with the command itself (missing file, unreadable config) go to stderr. Both exit 2.

**D15. `faultline init` never overwrites.** Existing files are reported and left unchanged; exit 0.

**D16. Flags may appear before or after positional arguments.** The standard `flag` package stops at the first positional argument, and the PRD shows `faultline run <file> --fail-on-breach`.

## Proxy

**D17. First matching fault wins, and there is no fall-through.** The first fault in experiment order whose scope, route, method, path and time window match is the fault for that request. If its strategy does not fire, the request passes through unfaulted; later faults are not consulted. Alternative: fall through to the next fault. Reason: literal reading of "first matching fault in experiment order wins; one fault per request" in PRD 8.3. Consequence: two faults with the same match do not combine; give them distinct matches.

**D18. Reset closes with linger 0 (TCP RST). Cut and hold close normally (FIN) with the response left unterminated.** Alternative: RST for all three. Reason: an RST lets the client's kernel discard bytes it has not read yet, which would break "forward exactly N bytes". With FIN the client receives exactly the forwarded bytes and then an unexpected end of stream.

**D19. Record `status` is 0 for reset, for timeout, and for any response the proxy cut mid-body** (`corrupt_body` truncate, `llm_stream_cut`), even though the client saw a status line. Reason: PRD 8.7 lists "reset, cut or timeout". A truncate whose `at_bytes` exceeds the body is not a cut and keeps the upstream status.

**D20. `fault_id`, `fault_type`, `retry_after` and `client_held_ms` are `null` when they do not apply. `correlation` is `""` when the header is absent** (the PRD example says "uuid-or-empty").

**D21. `GET /admin/v1/records` returns `{records, next}`.** `next` is the highest sequence number examined; pass it back as `since`. With an empty `run_id` records of every run are returned.

**D22. `GET /admin/v1/stats` returns `{run_id, active, routes, faults, events}`.** `routes` counts every request since the proxy started. `faults` holds triggers for the current or most recent run, so control can read final trigger counts after it has cleared the faults. `events` holds the last 100 of `installed`, `cleared`, `lease_expired`, `shutdown`.

**D23. `PUT /admin/v1/faults` requires `run_id`, `lease_id`, `scope_mode` and an `end_at` on every fault; `start_at` defaults to now.** Faults use the same shape as in the experiment file (`id, type, route, match, strategy, params`) plus the absolute window. An optional top-level `seed` seeds probability and garble. Lease renewal answers 404 when nothing is installed and 409 for a different `lease_id`.

**D24. The proxy does not enforce `control.allow_unscoped`.** That key governs which experiments control accepts. The admin API is the trusted, token-protected interface.

**D25. Clearing faults also releases in-flight work:** holds are closed, latency and slow-first-token delays end and the request is forwarded, throttled bodies continue at full speed.

**D26. `latency` and every other fault that still produces a response carry `X-Faultline-Fault`.** The header is absent only when no response is sent (reset, timeout) or when `expose_fault_header` is false.

**D27. Upstream failure is 502 `{"error":"upstream_unreachable"}` with `X-Faultline: upstream-error`.** The PRD does not specify it.

**D28. Not supported: protocol upgrades (WebSocket) and `CONNECT`.** `Upgrade` is a hop-by-hop header and is removed. The PRD scopes the proxy to reverse-proxied HTTP.

**D29. `garble` flips every seventh byte** (offsets 0, 7, 14, ...) with a mask derived from the seed and the offset. Same seed, same corruption, independent of how the body is chunked.

**D30. Children of `faultline up` shut down when their stdin closes** (opt-in through the internal `FAULTLINE_PARENT_WATCH=1` environment variable that `up` sets). Reason: Windows cannot deliver SIGTERM to a child process, and it also stops the children if `up` itself is killed. SIGINT and SIGTERM work as usual on every platform that has them.

## Control plane

**D31. Control API timestamps are RFC 3339 in UTC with second precision** (`2026-01-02T15:04:05Z`): the run document, timeseries (`t`, phase and fault-window bounds, markers) and audit entries. Proxy records keep RFC3339Nano as PRD 8.7 says. Reason: the PRD writes "RFC3339" for the report and "RFC3339Nano" for records, and second precision parses everywhere.

**D32. Probes carry the scope header (the run id) and a fresh correlation id, like driver requests.** Alternative: unscoped probes. Reason: with header scoping an unscoped probe can never observe a fault, so `probe_success_min` and `probe_failures_consecutive` would be meaningless for any probe that exercises a dependency. A plain health endpoint behaves the same either way.

**D33. Recovery uses a rolling 5s window that never reaches back into the injection phase.** `recovery_seconds` is the time from injection end until every probe has passed 3 consecutive checks made after injection ended and the error rate of driver requests started after injection ended (at least one) is within the limit. Alternative: a plain rolling 5s window, which would count injection-phase errors and put a floor of about 5s under every failing experiment. With no probes and no request traffic, recovery is 0. The phase ends as soon as recovery is observed.

**D34. `metrics.driver` and the hypothesis cover driver requests that started during injection.** A request still in flight when the run ends is dropped rather than counted as an error. `metrics.proxy` counts the run's own proxied requests (scoped records, or all records under `scope: all`).

**D35. Two abort reasons beyond the PRD's list: `proxy_error`** (the proxy rejected or could not receive the rule set) **and `lease_lost`** (the proxy reported the lease gone mid-injection, so the faults are no longer installed). Reason: continuing either run would produce a verdict about faults that were not there.

**D36. A graceful control shutdown (SIGINT, SIGTERM, parent pipe) aborts the active run with reason `control_restarted`** after clearing its faults. Alternative: leave the run for the next start. Reason: faults are cleared immediately instead of after `lease_timeout`, and the reason stays within the PRD's vocabulary.

**D37. Audit `actor` is `api-token` for API calls and `system` for automatic aborts** (thresholds, restart, proxy error, lease lost).

**D38. The experiments API never returns or writes sensitive header values.** `GET /api/v1/experiments/{name}` returns the file with those values redacted; `PUT` redacts before writing. Consequence: an experiment that needs a secret header must be run from a local file with `faultline run` (the CLI posts the file as is and control keeps the secret in memory only) or posted directly to `POST /api/v1/runs`. Running it from the UI editor would send the literal `[REDACTED]`.

**D39. `faultline up` prints tokens only when it generated them**, as `FAULTLINE_TOKEN=<hex>` and `FAULTLINE_ADMIN_TOKEN=<hex>` lines on stdout. Tokens supplied through the environment are not echoed. Child process logs go to stderr.

**D40. Status codes the PRD leaves open:** `POST /api/v1/runs/{id}/abort` on a finished run is 409 `run_not_active`, unknown run 404 `run_not_found`. `POST /api/v1/kill` answers 502 `{"cleared":false,"error":"proxy_unreachable"}` if the proxy cannot be reached (the run is still aborted). `GET /api/v1/faults/active` answers 502 in the same case. Malformed JSON bodies are 400 `invalid_json`. `POST /api/v1/experiments/validate` is 200 for any YAML, valid or not.

**D41. `GET /api/v1/runs` items carry exactly the five PRD fields.** The UI reads `ended_at` from the run document to show duration, rather than adding a field to a contract.

**D42. Hypothesis entries are also computed for aborted runs**, over the part of the injection that ran. The verdict is still `aborted`.

**D43. Run events are consumed with `fetch` and an `Authorization` header.** `EventSource` cannot send headers, and accepting the token in the query string would put it in access logs.

**D44. The traffic driver is open loop.** It dispatches `rps` requests per second to `concurrency` workers; when every worker is busy the backlog is dropped, not queued. Redirects are not followed.

**D45. `faultline up` waits for both children to answer their health checks before printing**, and exits non-zero if either child exits.

## LLM faults

**D46. `llm_truncate` and `llm_malformed_tool_call` only rewrite a 200 response they can read:** `text/event-stream`, or a JSON body up to 32 MB. Upstream errors, compressed bodies and other content types are relayed untouched. For these two fault types the proxy removes `Accept-Encoding` from the upstream request so the body is readable. The fault still counts as triggered.

**D47. `llm_truncate` leaves a response alone when its text is within `max_chars`** (`stop_reason` stays as it was; a stream passes through byte for byte). `max_chars` counts Unicode characters across all text blocks. In a stream the rewritten events are emitted as they arrive, so the stream stays unbuffered and ends as soon as the limit is crossed.

**D48. `llm_malformed_tool_call` on a stream** sets the first `input_json_delta` of each tool_use block to `{"query": "de` and the following ones to an empty string, so the event sequence keeps its shape and the concatenation is invalid JSON. Non-streaming JSON is re-encoded, which may reorder object keys; numbers are preserved exactly.

**D49. `llm_slow_first_token` and `llm_stream_cut` work on any response, streaming or not.** The cut happens as soon as `after_bytes` bytes have been forwarded, without waiting for the upstream to send more.

**D50. `traffic.command` runs through `sh -c` (own process group, killed as a group) or `cmd.exe /S /C` (killed with `taskkill /T`).** Its output is discarded.

## Findings and reports

**D51. Retry rules judge each faulted route separately and group by correlation id.** A route is "faulted" when at least one of the run's requests on it was faulted. `retry_amplification` averages over every correlation seen on that route. A request without a correlation id cannot be attributed and is ignored by the retry rules.

**D52. Retry gaps are measured from the end of the faulted response to the arrival of the next request of the same correlation on the same route.** Two requests of one correlation that overlap by more than 5 ms are parallel calls, not an attempt and its retry, and are skipped. `retry_without_backoff` and `retry_after_ignored` are reported when there is at least one occurrence.

**D53. `missing_timeout` ignores holds that ended because the faults were cleared.** Such a hold shows neither a caller giving up nor a caller sitting the hold out. Without this, a short injection phase filled the sample with cut-short holds and hid callers that have no timeout. The proxy record still carries the true `client_held_ms`; control recognises these records by their end time falling at the moment it cleared the faults (300 ms tolerance).

**D54. `slow_recovery` and `fault_not_triggered` are only reported for runs that completed injection.** An aborted run measured neither. `steady_state_breach` is reported for any hypothesis entry that did not pass, including on aborted runs.

**D55. Findings are ordered high, medium, low, info.** Several findings can share an id (one `steady_state_breach` per breached threshold, one retry finding per route).

**D56. `GET /api/v1/runs/{id}/report` answers 409 `run_in_progress` until the run is terminal, and 400 `invalid_format` for a missing or unknown format.** Reports are rendered from the stored document on every request and are sent as attachments (`Content-Disposition`), with `Content-Security-Policy: sandbox`, so an HTML report is never rendered as a page of the control plane's origin. Every successful download is audited.

**D57. The HTML report contains no URL-like reference at all**, not even an SVG namespace or a CSS `url()`: fault windows are a translucent dashed box rather than a pattern fill. Lines differ by dash pattern as well as colour, the verdict carries a symbol and a word, and the timeline numbers are repeated in a table.

**D58. JUnit: the root element is `<testsuite>`** with `tests` and `failures` counts. Testcase names are the hypothesis names, `fault {id} triggered`, and `run not aborted`.

## CLI (remote commands)

**D59. `faultline run` stops at the first file that fails validation (exit 2) or that the control plane refuses (exit 1).** Files in a directory run in name order. Without `--fail-on-breach` the exit code is 0 whatever the verdicts, as PRD 11 says.

**D60. `--format` without `--report-dir` is a usage error (exit 2).** Reports have nowhere to go otherwise.

**D61. A missing `FAULTLINE_TOKEN` is a usage error (exit 2); a rejected token or an unreachable control plane is exit 1.**

**D62. Interrupting `faultline run` (Ctrl+C) aborts the run it started and exits 1.** Leaving faults injecting after the operator walked away would be the unsafe default.

**D63. A new run may be created the moment the previous one shows a terminal status.** Control finishes writing the previous run's reports first (it waits up to 15 s) instead of answering 409, so `faultline run <dir>` and API clients that poll for completion do not race it.

## Web UI

**D64. The built UI (`internal/ui/dist`) is committed.** Alternative: build it during `go build`. Reason: `go build ./...` and `go install` work from a clean checkout without Node. `make ui` rebuilds it; the Dockerfile and CI rebuild it from source.

**D65. One endpoint beyond the PRD table: `GET /api/v1/meta`** returns `{environment, allow_unscoped, scope_header}` (bearer token required). Reason: the header's environment badge has no other source, the UI must work from the API alone, and no existing contract was changed to provide it. The token gate also uses it to check a token.

**D66. Routing is hash based and hand written (`#/runs`, `#/runs/{id}`, `#/experiments`, `#/audit`); no router or UI framework dependency.** Charting is uPlot, as the PRD suggests.

**D67. The kill switch confirmation is a native `<dialog>`** opened with `showModal()`, which gives focus trapping and Escape to cancel without extra code. Extra test ids beyond the PRD's nine: `kill-confirm`, `kill-result`, `active-run`, `abort-run`, `kpi-requests`, `kpi-faults`, `experiment-name`, `experiment-yaml`, `experiment-status`, `experiment-errors`, `experiment-save`, `experiment-run`, `audit-table`.

**D68. Export buttons fetch the report with the `Authorization` header and hand the browser a blob.** A plain link could not carry the token. They are disabled until the run is terminal, matching D56.

**D69. The token is kept in `sessionStorage` only, is never put in a URL, and is removed on the first 401.** The UI is served with `Content-Security-Policy: default-src 'self'`, `X-Frame-Options: DENY`, `nosniff` and `no-referrer`.

**D70. Colour is never the only signal:** verdicts and results carry a symbol and a word, chart lines differ by dash pattern, fault windows and phase boundaries are labelled, and the timeline numbers are available as a table. Light and dark follow the system setting.

**D71. The UI smoke test is Playwright (Chromium) against real `faultline-demo`, `faultline proxy` and `faultline control` processes** started by `ui/e2e/stack.mjs` on ports 17700-17702 and 19000-19003. It is a separate gate (`make test-ui`), not part of `go test`.

## Control plane (addendum)

**D72. The `run_aborted` audit entry is written before the aborted status becomes visible through the API.** Found by the Linux race run: a client that saw `aborted` could read the audit log before the entry existed.

## Packaging and docs

**D73. `examples/experiments/` holds four experiments with documented results against the naive demo: passed, failed, failed, passed.** `faultline run examples/experiments/ --fail-on-breach` therefore exits 10, and the README says so. Alternative: only passing examples. Reason: the failing ones are what show the findings; an example set that cannot fail teaches nothing.

**D74. Images are `gcr.io/distroless/static-debian12:nonroot`.** The data directory `/data` is created in the build stage and copied with nonroot ownership, because the runtime image has no shell and a named volume inherits the ownership of the image's directory.

**D75. The compose example publishes ports on `127.0.0.1` only, and never the proxy admin port.** The example config binds the listeners to `0.0.0.0` inside the containers, which the default `127.0.0.1` would not allow other containers to reach.

**D76. The example experiments directory is mounted read-only in compose**, so saving from the dashboard fails there. The container runs as a non-root user that could not write to a host directory anyway.

**D77. CI rebuilds the dashboard and fails if `internal/ui/dist` differs from what is committed.** The build was checked to be byte-identical between Windows and Linux (`node:22-alpine`) with the committed lockfile.

**D78. LICENSE copyright holder is "Faultline contributors".** The PRD names MIT but no legal entity.

**D79. `make lint` is `go vet`, a `gofmt` check and `staticcheck` (pinned, run with `go run`).** No linter is added as a module dependency.
