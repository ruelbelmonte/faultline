# Progress

Gate results per milestone. Counts are `--- PASS` / `--- FAIL` / `--- SKIP` lines from `go test -v` (subtests included).

Host: Windows 11, Go 1.27.4 (portable, `.tools/`). Race gates: `golang:1.27` Linux container (see DECISIONS D3).

## M0: skeleton, config and experiment validation, `version`, `init`, `validate`

Built: `internal/yamlx` (strict YAML walker), `internal/config`, `internal/fault`, `internal/experiment` (parse, validate, redact), `internal/cli` (`version`, `init`, `validate`), `cmd/faultline`.

| Command | Where | Result |
|---|---|---|
| `go vet ./...` | Windows | clean |
| `go test ./...` | Windows | 95 pass, 0 fail, 0 skip |
| `go vet ./... && go test ./... -race` | Linux container | clean, all packages ok |

Gate: unit test for every validation rule (config: `TestValidationRules`, `TestProductionGuard`, `TestToken`; experiment: `TestValidationRules`, `TestFaultParams`, `TestNoFaults`, `TestDuplicateFaultID`), and `init` output validates (`TestInitOutputValidates`). **Green.**

Known gaps: none for M0. `up`, `proxy`, `control`, `run`, `status`, `stop`, `report` exit 1 with "not built yet" until their milestones.

## M1: `faultline-demo`

Built: `internal/demo` (shop, inventory, pricing, Anthropic-compatible LLM mock, naive and good resilience modes), `cmd/faultline-demo`, integration harness (`test/integration/harness_test.go`: builds both binaries, starts real processes on free ports, fails a test if a process prints a data race).

Also fixed after M0: YAML aliases and merge keys are now rejected and redaction fails closed (DECISIONS D12a), found by the automated security review of the M0 commit.

| Command | Where | Result |
|---|---|---|
| `go vet ./... && go vet -tags integration ./test/...` | Windows, Linux | clean |
| `go test ./...` | Windows | 98 pass, 0 fail, 0 skip |
| `go test -tags integration ./test/integration/ -count=1` | Windows | 16 pass, 0 fail, 0 skip |
| `go test ./... -race` | Linux container | ok |
| `FAULTLINE_ITEST_RACE=1 go test -race -tags integration ./test/integration/ -count=1` | Linux container | ok (binaries built with `-race`) |

Gate: shop -> inventory -> pricing returns 200 (`TestDemoChainAndHeaderForwarding`), header forwarding verified on both hops, naive vs good retry counts, backoff, Retry-After and timeouts verified against a stub upstream (`TestDemoRetryCounts`), LLM mock shapes and stream timing (`TestDemoLLM*`). **Green.**

Known gaps: the M1 chain test routes through a pass-through stub because the proxy does not exist yet. The same chain through the real proxy is an M2 test.

## M2: proxy data plane

Built: `internal/proxy` (routing, unbuffered request and response streaming, fault engine with strategies and scoping, lease watchdog, request record ring, admin API, graceful shutdown), `faultline proxy`, shared wire types in `internal/fault`.

Also fixed after M1 (security review of the M1 commit): the demo escapes the order id before building downstream URLs, and YAML keys with explicit tags are rejected (DECISIONS D12a).

| Command | Where | Result |
|---|---|---|
| `go vet ./... && go vet -tags integration ./test/...` | Windows, Linux | clean |
| `go test ./...` | Windows | 104 pass, 0 fail, 0 skip |
| `go test -tags integration ./test/integration/ -count=1` | Windows | 31 top-level tests pass, 0 fail, 0 skip |
| `go test ./... -race` | Linux container | ok |
| `FAULTLINE_ITEST_RACE=1 go test -race -tags integration ./test/integration/ -count=1` | Linux container | ok, 32 top-level tests (adds the Unix signal test) |

Gate, all with real processes and sockets:

- every generic fault type: `TestFaultLatency`, `TestFaultHTTPError`, `TestFaultErrorInBody`, `TestFaultTimeout`, `TestFaultReset`, `TestFaultSlowBody`, `TestFaultCorruptBody`
- every strategy: `TestStrategies` (always, burst, every_nth, probability with seed reproducibility)
- scoping including other run ids: `TestScoping`
- `end_at` enforced by the proxy alone: `TestFaultWindowIsEnforcedByProxy`
- lease expiry after the admin client stops renewing, including release of an in-flight hold: `TestLeaseExpiryClearsAllFaults`
- kill clears within 1s, including in-flight hold and delay: `TestKillClearsWithinOneSecond`
- streaming time to first byte against the demo LLM mock, and request body streaming: `TestProxyStreamingIsNotBuffered`
- routing, admin auth, admin validation, records, paging, stats, startup guards, shutdown.

**Green.**

Two test bugs were found and fixed while getting to green, neither by loosening an assertion: the lease and kill tests listed a catch-all fault before the hold so the hold was never exercised (reordered, and they now assert the held connection got no bytes); record assertions read the record list before the proxy had stored the record (they now wait for the expected count). One product bug was found by the tests: the garble mask barely depended on the seed.

Known gaps: LLM fault types are accepted by the admin API but pass traffic through unchanged until M4. Protocol upgrades are not proxied (D28).

## M3: control plane

Built: `internal/control` (SQLite store with audit log and `audit.jsonl`, proxy admin client, run lifecycle, traffic driver with requests or command, probes, 1-second metrics, hypothesis and verdict rules, abort paths, kill switch, lease renewal, restart recovery, API with bearer auth and SSE), `internal/report` (canonical run document types), `internal/ui` (embedded static handler; the UI itself is M6), `faultline control`, `faultline up`.

Also fixed after M2 (security review of the M2 commit): record `path`, `correlation` and `method` are length-capped and the record ring grows on demand instead of preallocating.

| Command | Where | Result |
|---|---|---|
| `go vet ./... && go vet -tags integration ./test/...` | Windows, Linux | clean |
| `go test ./...` | Windows | 110 pass, 0 fail, 0 skip |
| `go test -tags integration ./test/integration/ -count=1` | Windows | 58 top-level tests pass, 0 fail, 0 skip (104s) |
| `go test ./... -race` | Linux container | ok |
| `FAULTLINE_ITEST_RACE=1 go test -race -tags integration ./test/integration/ -count=1` | Linux container | ok (93s), includes 3 Unix-only signal tests |

Gate, all with demo + proxy + control as real processes:

- pass: `TestRunPassed` (also run document shape, timeseries, runs list, audit)
- fail: `TestRunFailed` (also header scoping end to end and lease renewal with `lease_timeout` shorter than the run)
- inconclusive, both reasons: `TestRunInconclusiveUnhealthyBeforeStart` (no faults installed), `TestRunInconclusiveFaultNotTriggered`
- abort paths: `TestAbortManual`, `TestAbortDuringPrecheck`, `TestAbortKillSwitch`, `TestAbortThresholdErrorRate`, `TestAbortThresholdProbeFailures`
- control killed mid-run (SIGKILL on Linux, TerminateProcess on Windows), faults cleared by lease, run marked `control_restarted` on restart: `TestControlKilledFaultsClearByLease`, `TestControlRestartClearsFaultsImmediately`
- auth on every route, environment guard, token rules, redaction scan of `data_dir`, API responses and logs (`TestRedactionOfSecrets`), experiments API, audit, SSE, `faultline up`.

**Green.**

Test fixes made while getting to green, none by loosening an assertion: `TestAbortManual` aborted before the driver had finished its first request (it now waits for a measured request); `TestStructuredLogs` read the log before the startup line was written; a harness field for extra config was declared but never written into the config file.

Known gaps:

- Findings are always `[]`, reports are not generated and `GET /api/v1/runs/{id}/report` answers 404 until M5. `TestRedactionOfSecrets` already requests the four report formats so it will cover them once they exist.
- `run`, `status`, `stop`, `report` CLI commands are M5.
- `traffic.command` is implemented but has no integration test yet.

## M4: LLM fault types

Built: `internal/proxy/llm.go` (`llm_rate_limit`, `llm_overloaded`, `llm_stream_cut`, `llm_slow_first_token`, SSE-aware `llm_truncate` and `llm_malformed_tool_call` for the Anthropic Messages API, stream and non-stream), per-platform shell helper for `traffic.command`.

| Command | Where | Result |
|---|---|---|
| `go vet ./... && go vet -tags integration ./test/...` | Windows, Linux | clean |
| `go build ./...` with `GOOS=linux` and `GOOS=darwin` | Windows | ok |
| `go test ./...` | Windows | 114 pass, 0 fail, 0 skip |
| `go test -tags integration ./test/integration/ -count=1` | Windows | 66 top-level tests pass, 0 fail, 0 skip |
| `go test ./... -race` | Linux container | ok |
| `FAULTLINE_ITEST_RACE=1 go test -race -tags integration ./test/integration/ -count=1` | Linux container | ok (124s) |

Gate, against the demo LLM mock through a real proxy, stream and non-stream for each: `TestLLMRateLimit`, `TestLLMOverloaded`, `TestLLMStreamCut`, `TestLLMSlowFirstToken`, `TestLLMTruncate`, `TestLLMMalformedToolCall`, plus `TestLLMFaultsThroughControl` (LLM faults installed by a real run). **Green.**

Closed from M3's known gaps: `traffic.command` now has an integration test (`TestTrafficCommand`), which found that quoted commands could not work on Windows (fixed, D50).

Known gaps: OpenAI format is a PRD stretch goal and is not built.

## M5: findings, reports, `run` CLI

Built: `internal/findings` (seven rules, thresholds in `thresholds.go`), `internal/report` renderers (JSON, Markdown, self-contained HTML with inline SVG timeline, JUnit), report files under `{data_dir}/reports/{run_id}/`, `GET /api/v1/runs/{id}/report`, `finding` events on the run stream, CLI `run`, `status`, `stop`, `report`.

| Command | Where | Result |
|---|---|---|
| `go vet ./... && go vet -tags integration ./test/...` | Windows, Linux | clean |
| `go test ./...` | Windows | 131 pass, 0 fail, 0 skip |
| `go test -tags integration ./test/integration/ -count=1 -timeout 30m` | Windows | 74 top-level tests pass, 0 fail, 0 skip |
| `go test ./... -race` | Linux container | ok |
| `FAULTLINE_ITEST_RACE=1 go test -race -tags integration ./test/integration/ -count=1 -timeout 30m` | Linux container | ok (265s) |

Gate:

- naive mode triggers `retry_amplification`, `retry_without_backoff`, `retry_after_ignored`, `missing_timeout`; good mode triggers none of them for the same two experiments: `TestFindingsNaiveVersusGood`
- the other three rules end to end: `TestFindingsFromVerdictRules`
- HTML has no external URLs, JUnit parses, four formats with content types, files on disk: `TestReports`, `TestCLIRunWritesReports`, unit `TestHTMLIsSelfContained`, `TestJUnit`
- `run` exit codes 0, 10, 11, 12, 2, 1 and worst-of across a directory: `TestCLIRunExitCodes`, `TestCLIRunDirectoryUsesWorstExitCode`, `TestCLIRunErrors`
- `status`, `stop`, `report`, and the `init` example passing against the demo: `TestCLIStatusStopReport`.

**Green.**

Design change made while writing the gate tests, recorded as D53: the first experiment design for `missing_timeout` would not have produced the finding in naive mode, because most sampled holds were cut short by the end of injection. The rule now leaves those samples out, with a unit test for each direction (`TestMissingTimeoutIgnoresHoldsCutByClear`).

Closed from M3's known gaps: reports and findings exist, so `TestRedactionOfSecrets` now scans all four report formats and the report files on disk.

Known gaps: none for M5.

## M6: web UI

Built: `ui/` (Vue 3, TypeScript, Vite, uPlot): token gate, header with kill switch, active run indicator and environment badge, runs list, run detail (verdict banner, KPI tiles, live timeline over the event stream with fault windows, phase boundaries and abort marker, hypothesis, findings with evidence, fault triggers, four export buttons, abort button), experiments editor with debounced validation, audit log. Built assets are embedded from `internal/ui/dist`. `GET /api/v1/meta` added for the environment badge (D65).

Also fixed after M5 (security review of the M5 commit): the CLI validates run ids before using them in URLs or report paths, rejects non-HTTP control URLs and warns when the token would travel over plain HTTP to a non-loopback host.

| Command | Where | Result |
|---|---|---|
| `npm run build` in `ui/` (`vue-tsc --noEmit && vite build`) | Windows | ok, no type errors |
| `npx playwright test` in `ui/` | Windows, Chromium | 7 pass, 0 fail, 0 skip |
| `go vet ./... && go vet -tags integration ./test/...` | Windows, Linux | clean |
| `go test ./...` | Windows | 133 pass, 0 fail, 0 skip |
| `go test -tags integration ./test/integration/ -count=1 -timeout 30m` | Windows | 75 top-level tests pass, 0 fail, 0 skip |
| `go test ./... -race` | Linux container | ok |
| `FAULTLINE_ITEST_RACE=1 go test -race -tags integration ./test/integration/ -count=1 -timeout 30m` | Linux container | ok (238s) |

Gate, `ui/e2e/smoke.spec.ts` against real demo, proxy and control processes: token gate (wrong token rejected, right one accepted, stale one cleared), runs list, run detail verdict, kill switch (confirmation, faults cleared, run aborted), the four export buttons (each download's name and content checked), plus the experiment editor, audit log and keyboard-only use. `TestEmbeddedUI` checks that the binary serves the built UI with all nine PRD `data-testid` values. **Green.**

The run detail and runs list were also checked by eye in light and dark mode from screenshots.

Two problems surfaced by the first Linux race run of this milestone, both fixed: the test harness could hand the same free port to two servers (it now remembers ports it has given out), and the `run_aborted` audit entry was written after the aborted status was visible (D72).

Known gaps:

- The Playwright test has only been run on Windows with Chromium. Firefox and WebKit are not covered.
- Accessibility was built in (labels, focus order, dialog semantics, non-colour signals) and one keyboard path is tested, but no screen reader or automated accessibility audit was run.
- An experiment with a sensitive header cannot be run from the UI editor with its real value (D38).
