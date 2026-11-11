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

## M7: packaging and docs

Built: `Makefile` (build, ui, test, test-integration, test-race, test-ui, lint, demo, docker, clean), `Dockerfile`, `Dockerfile.demo`, `.dockerignore`, `examples/` (compose file, config, four experiments, Terraform snippet), `docs/` (architecture, experiments, faults, qa-deployment), `README.md`, `LICENSE`, `.github/workflows/ci.yaml`.

| Check | Where | Result |
|---|---|---|
| `make build test test-integration lint` from a copy without build outputs | Linux container (`golang:1.27`) | all four targets succeed; integration 203s; staticcheck clean |
| `docker build` for both images | Docker Desktop | ok; `faultline` 28.2 MB, `faultline-demo` 16.6 MB |
| `docker compose -f examples/docker-compose.yaml up --build -d` | Docker Desktop | demo, proxy, control running; only 7700, 7701, 9000 published, on 127.0.0.1 |
| `faultline run examples/experiments/ --fail-on-breach` against compose | Windows CLI | passed, failed, failed, passed; exit code 10, as documented in the README |
| README quickstart, run exactly as written | Git Bash on Windows | verdict passed, exit 0, dashboard answers, `kill %1 %2` stops everything |
| Cross-compile linux, darwin, windows for amd64 and arm64 | Windows | ok for all six |
| Dashboard build reproducible | Windows vs `node:22-alpine` | identical file hashes |
| `go test ./...` | Windows | 133 pass, 0 fail, 0 skip |

Gate: Dockerfiles build; compose brings up demo + proxy + control and an example experiment passes via `faultline run` (`01-inventory-flaky` and `04-llm-slow-first-token` pass; the other two fail by design); README quickstart works verbatim; the GitHub Actions workflow runs lint, unit, integration and race tests. **Green, with the caveats below.**

Added while documenting: `TestProxyRestartAbortsRunWithLeaseLost`, because the QA guide states that behaviour.

Known gaps:

- **The GitHub Actions workflow has never run.** There is no remote. Every command in it was run by hand locally except the macOS integration job and `npx playwright install --with-deps`.
- **macOS is untested.** The code cross-compiles for darwin and has no platform-specific code beyond the Unix shell helper, but no test has run there.
- `make` targets were run in a Linux container. `make` is not installed on the Windows build machine, where the equivalent `go` commands were used.
- The README quickstart was run in Git Bash on Windows. It was not run on Linux or macOS, though each command in it has been run on Linux inside containers.

## M8: MCP tool faults (stretch)

**Not attempted.** M0 to M7 are green, so the precondition holds, but the PRD gives M8 no contract: section 2 points to section 13 for it, and section 13 describes only the demo. Building it would have meant inventing fault types, parameters and a demo MCP server with nothing to check them against. What it would take is listed under "What I would do next".

## Continuation baseline (2026-11-12)

Full existing gate re-run at commit `2b7d542` before any new work. **Green.**

| Command | Where | Result |
|---|---|---|
| `go vet ./... && go vet -tags integration ./test/...`, `gofmt -l cmd internal test` | Windows | clean |
| `go test ./... -count=1` | Windows | 133 pass, 0 fail, 0 skip |
| `go test -tags integration ./test/integration/ -count=1 -timeout 30m` | Windows | 76 top-level tests pass, 0 fail, 0 skip (215s) |
| `npx playwright test` in `ui/` | Windows, Chromium | 7 pass, 0 fail, 0 skip |
| `sh .tools/race.sh "make lint && make test-race"` | Linux container | vet, gofmt, staticcheck clean; unit ok; integration ok with `-race` (239s) |
| `docker compose -f examples/docker-compose.yaml up --build -d`, then `faultline run examples/experiments/ --fail-on-breach` | Docker Desktop, Windows CLI | passed, failed, failed, passed; exit code 10, as documented |

The first two compose attempts failed with exit code 1 and were not product failures: a second agent session had been given the same task in this working tree and was starting and stopping the same compose project at the same moment (visible in `docker events` as the containers being recreated two seconds after they started). The result above is the run made after that session was stopped.

## Work item 1: acceptance suite

Ran `faultline-acceptance/` as a black box against `bin/faultline.exe` and `bin/faultline-demo.exe` built from commit `2b7d542`. Only its README, `requirements.txt` and test output were read; the test source was not opened.

| Command | Where | Result |
|---|---|---|
| `python -m pytest --tb=line -rfEs -q -p no:cacheprovider --durations=10` in `faultline-acceptance/`, with `FAULTLINE_BIN` and `FAULTLINE_DEMO_BIN` set, Python 3.10 venv with `requirements.txt` plus `playwright` and its Chromium | Windows | **56 passed, 0 failed, 0 skipped** (298s) |

All seven groups ran, including the UI test (G), which the suite skips unless Playwright is installed. Group counts match the suite's README: A 7, B 14, C 9, D 10, E 9, F 6, G 1.

There was nothing to reconcile: no product change and no change to a decision. The suite did not contradict any reading in the "Risks I cannot close from here" table. That is weaker than confirming them: a reading the suite does not probe (for example D5, D31 or D39) is still only my reading of the PRD.

Gate: unchanged from the continuation baseline above, since no code changed.

Known gaps: the suite was run on Windows only, once at this point and again after the later work items (see the final report).

## Work item 2: request header fidelity (PRD 8.1)

Built: `internal/proxy/rawhead.go`. Request header lines now reach the upstream with the client's spelling, in the client's order, with duplicates as separate lines. The proxy keeps the raw head of each request as it is read (`inConn`), uses it only when it says exactly what Go's server parsed, and rewrites the head Go writes upstream (`outConn`). Upstream connections are now HTTP/1.1 only, also over TLS. Design, limits and alternatives: DECISIONS D80; user-facing description: `docs/architecture.md`.

| Command | Where | Result |
|---|---|---|
| `go vet ./... && go vet -tags integration ./test/...`, `gofmt -l cmd internal test` | Windows | clean |
| `go test ./... -count=1` | Windows | 140 pass, 0 fail, 0 skip |
| `go test -tags integration ./test/integration/ -count=1 -timeout 30m` | Windows | 78 top-level tests pass, 0 fail, 0 skip (209s) |
| `npx playwright test` in `ui/` | Windows, Chromium | 7 pass |
| `sh .tools/race.sh "make lint && make test-race"` | Linux container | lint clean; unit and integration ok with `-race` (242s) |
| compose example, `faultline run examples/experiments/ --fail-on-breach` | Docker Desktop | passed, failed, failed, passed; exit 10 |
| acceptance suite | Windows | 56 passed, 0 failed, 0 skipped |

Gate: `TestRequestHeadersArePreservedByteForByte` (real proxy process; a raw TCP client sends mixed-case, unordered, duplicated headers; a raw TCP upstream compares the bytes it receives: spelling, order, duplicates, hop-by-hop removal, `Host` rewritten in place, keep-alive reuse after a chunked body, a faulted request, pipelined requests) and `TestRequestHeaderFidelityUnderConcurrency` (8 connections, 320 requests, each checked against its own headers). Unit tests: framing across fixed, chunked and pipelined requests, fallback, head rewriting, an `https` upstream that offers HTTP/2 and is still spoken to in HTTP/1.1 with the exact head. **Green.**

Two mistakes in the new tests' own data were fixed while getting to green (a chunk size that did not match its data, and a channel too small for the concurrency test); no assertion was loosened.

Known gaps:

- **Response** header names are still canonicalised and sorted by Go on the way back to the client. PRD 8.1 states fidelity for the forwarded request only, so this is recorded rather than built.
- Fallback to canonical, sorted names: a request with an obsolete folded header (and the rest of that client connection), and an `https` upstream reached through `HTTPS_PROXY`.
- Normalised, not preserved: whitespace around header values, line endings, and the chunk boundaries of a chunked request body.
- `https` upstreams no longer use HTTP/2.

## Work item 3: limits and load

Built:

- The record ring size stays fixed: the config contract does not allow a new key (D81).
- Control no longer holds a run's request records in memory. Each page read from the proxy goes to a `records` table in SQLite, the finding rules read it back grouped by route and correlation (`findings.Evidence`, `internal/control/records.go`), and the rows are deleted once the findings exist (D82). Only what the rules read is written; the correlation id is replaced by a salted hash.
- The proxy logs a warning when its ring overwrites a record that was never read (D83).
- `TestLoadProxyOverhead` behind the build tag `load`, and `make test-load` (D84).

| Command | Where | Result |
|---|---|---|
| `go vet ./... && go vet -tags "integration load" ./test/...`, `gofmt -l cmd internal test` | Windows | clean |
| `go test ./... -count=1` | Windows | 143 pass, 0 fail, 0 skip |
| `go test -tags integration ./test/integration/ -count=1 -timeout 30m` | Windows | 78 top-level tests pass, 0 fail, 0 skip (210s) |
| `npx playwright test` in `ui/` | Windows, Chromium | 7 pass |
| `sh .tools/race.sh "make lint && make test-race"` | Linux container | lint clean (now also vets and staticchecks the `load` tag); unit and integration ok with `-race` (241s) |
| compose example, `faultline run examples/experiments/ --fail-on-breach` | Docker Desktop | passed, failed, failed, passed; exit 10 |
| acceptance suite | Windows | 56 passed, 0 failed, 0 skipped |
| `go test -tags "integration load" ./test/integration/ -run TestLoad -count=1 -v` | Windows | pass, numbers below |
| `sh .tools/race.sh "make test-load"` | Linux container | pass, numbers below |

Gate: `TestStoredEvidenceGivesTheSameFindingsAsMemory` (five generated runs of 3,000 records give identical findings from disk and from memory), `TestStoredRecordsHoldNoCorrelationIDsAndAreDeleted` (no correlation id in any file under the data directory; rows deleted per run and at restart), `TestRingReportsOverwritingUnreadRecords`, and the existing end-to-end finding tests (`TestFindingsNaiveVersusGood`, `TestFindingsFromVerdictRules`), which now go through the stored records. **Green.**

### Load test numbers

Closed loop, keep-alive connections, 5 s per row, in-process upstream returning 34 bytes of JSON, all on one machine over loopback. "Active run" is a real control run whose fault never matches: every request is matched, recorded, collected and written to SQLite. No request failed, and in every active-run row the run's request count equalled the number of requests sent (no record lost).

Windows 11 host (the clock there ticks at about 0.5 ms, so read the mean, not the percentiles):

| Scenario | Conns | req/s | mean | p50 | p95 | p99 |
|---|---|---|---|---|---|---|
| direct to upstream | 1 | 7,798 | 125 µs | 0 | 1.01 ms | 1.51 ms |
| through proxy, no run | 1 | 2,653 | 373 µs | 523 µs | 561 µs | 1.01 ms |
| through proxy, active run | 1 | 2,600 | 379 µs | 523 µs | 561 µs | 1.02 ms |
| direct to upstream | 64 | 80,101 | 776 µs | 542 µs | 2.55 ms | 3.65 ms |
| through proxy, no run | 64 | 21,892 | 2.89 ms | 2.50 ms | 6.44 ms | 8.66 ms |
| through proxy, active run | 64 | 23,828 | 2.65 ms | 2.27 ms | 5.75 ms | 7.96 ms |

Linux container (`golang:1.27` under Docker Desktop on the same machine):

| Scenario | Conns | req/s | mean | p50 | p95 | p99 |
|---|---|---|---|---|---|---|
| direct to upstream | 1 | 8,537 | 115 µs | 111 µs | 184 µs | 234 µs |
| through proxy, no run | 1 | 2,591 | 383 µs | 366 µs | 516 µs | 662 µs |
| through proxy, active run | 1 | 2,535 | 391 µs | 372 µs | 530 µs | 692 µs |
| direct to upstream | 64 | 77,624 | 820 µs | 643 µs | 2.17 ms | 3.25 ms |
| through proxy, no run | 64 | 17,568 | 3.64 ms | 3.33 ms | 7.47 ms | 9.74 ms |
| through proxy, active run | 64 | 16,511 | 3.87 ms | 3.36 ms | 8.63 ms | 12.35 ms |

Reading them: the proxy adds about 0.25 to 0.27 ms to a request on an idle connection, and an active run adds under 0.01 ms on top of that. One proxy process carried 16,000 to 24,000 requests per second here. The difference between "no run" and "active run" at 64 connections is inside the noise between rows (it has opposite signs on the two platforms).

Cost of work item 2, measured by running the same test against commit `2b7d542` on Windows: single-connection mean 355 µs before, 373 µs after; 64-connection throughput without a run 22,998 req/s before, 21,892 after. About 5%.

Known gaps:

- The numbers are from one developer machine with client, proxy and upstream competing for the same cores. They show the order of magnitude, not a capacity figure.
- One run each; no repeated runs, no confidence intervals. Only small responses over keep-alive connections; no large bodies, no streaming, no TLS upstream.
- Memory per run still grows with driver traffic (about 50 bytes per driver request) and with the number of retry gaps found (D82).
- A ring overflow is visible in the proxy log only (D83). It never occurred in any test.

## Work item 4: UI verification

Built: Firefox and WebKit projects in the Playwright run; `ui/e2e/a11y.spec.ts`, an axe audit (WCAG 2.0 and 2.1 A and AA plus best-practice rules) of every page in light and dark colour scheme, in all three browsers; shared test helpers in `ui/e2e/helpers.ts`; `make test-ui` and the CI workflow install and run all three browsers (D85 to D87).

| Command | Where | Result |
|---|---|---|
| `npx playwright test` in `ui/` (6 projects: smoke and a11y for chromium, firefox, webkit) | Windows | 42 pass, 0 fail, 0 skip |
| same, in `mcr.microsoft.com/playwright:v1.65.0-noble` against Linux binaries | Linux container | 42 pass, 0 fail, 0 skip |
| `npm run build` in `ui/` | Windows | ok; `internal/ui/dist` unchanged |
| `go vet`, `gofmt`, `go test ./... -count=1` | Windows | clean; 143 pass, 0 fail, 0 skip |
| `go test -tags integration ./test/integration/ -count=1 -timeout 30m` | Windows | 78 top-level tests pass, 0 fail, 0 skip (210s) |
| `sh .tools/race.sh "make lint && make test-race"` | Linux container | lint clean; unit and integration ok with `-race` (241s) |
| compose example | Docker Desktop | passed, failed, failed, passed; exit 10 |
| acceptance suite | Windows | 56 passed, 0 failed, 0 skipped |

What the new checks found:

- **Axe: no violation** on any of the nine page states, in either colour scheme, in any browser. Nothing in the dashboard needed fixing. The audit was shown to be able to fail by planting three problems (image without alt text, unlabelled input, low-contrast text), which axe reported in all three browsers.
- **Firefox: a page crash in the test tooling**, not in the dashboard. Once axe has run in a Firefox process, a Playwright locator polling the text of the editor's error list crashes the page. The same page state reached without that poll does not crash. Handled by running the audit as its own project per browser and waiting on the status line (D86). The root cause inside Firefox was not found.
- **WebKit: Tab does not stop at links**, which is Safari's default. The keyboard test asserts the skip link in Chromium and Firefox only; the rest of it (reach the kill switch, open and dismiss the dialog from the keyboard) runs in all three (D87).

The smoke test's assertions are unchanged apart from that one WebKit condition.

Gate: **Green.**

Known gaps:

- No screen reader was used. Axe checks what can be checked mechanically; it cannot judge the canvas timeline (its numbers are repeated in a table) or whether the reading order makes sense when spoken.
- Safari on macOS itself was not tested: Playwright's WebKit build on Windows and Linux stands in for it.
- D38 still holds: an experiment with a sensitive header cannot be run from the editor with its real value.

---

# Final report

State at commit time: gates M0 to M7 green from a fresh `git clone`, on Linux (`golang:1.27` container, including `-race`) and on Windows 11.

| Final check, from a fresh clone | Linux | Windows |
|---|---|---|
| `go vet`, `gofmt`, `staticcheck` | clean (`make lint`) | vet and gofmt clean |
| Unit tests | ok, also with `-race` | 133 pass, 0 fail, 0 skip |
| Integration tests (real processes) | ok with `-race`, binaries built with `-race`, 237s | 76 top-level tests pass, 0 fail, 0 skip |
| `make build` | ok | not run (`make` not installed); `go build` ok |
| UI build equals committed `internal/ui/dist` | identical hashes | no diff |
| Playwright smoke test | not run | 7 pass |
| Docker images, compose example | built and run through Docker Desktop (Linux containers) | CLI run from Windows against it |

After that clone-based run, one Unix-only test was added (`TestDemoGracefulShutdownOnSignal`) and run on Linux together with the other signal tests: 4 pass.

## Definition of done

| Item | Status |
|---|---|
| Gates M0 to M7 green from a clean checkout | Met on Linux and Windows. Not run on macOS. |
| `make build test test-integration lint` succeed | Met, in a Linux container. |
| `examples/docker-compose.yaml` brings up demo + proxy + control, and `faultline run examples/experiments/ --fail-on-breach` completes with the documented results | Met. The documented result is exit code 10: two experiments pass and two fail by design (D73). |
| README quickstart works verbatim for a new user | Met in Git Bash on Windows. Not run on Linux or macOS. |
| `PROGRESS.md` final report is complete and honest | This section. |

## PRD requirements believed met

Each has at least one test that would fail if it broke, named in the milestone sections above.

- **4 Technology.** Go, single module, two binaries, `faultline-demo` not imported by `faultline`. SQLite through `modernc.org/sqlite`, no cgo. Vue 3 + TypeScript + Vite, uPlot, embedded with `go:embed`, no Node at run time. Two third-party Go modules, both justified. Multi-stage Dockerfiles on distroless. Cross-compiles for linux, darwin and windows on amd64 and arm64.
- **5 Architecture.** Proxy and control are separate processes; `up` only spawns them, generates tokens when absent, prints URLs and tokens, forwards signals and stops both.
- **6 Configuration.** Every key, defaults, unknown keys rejected, production guard (exit 2), token rules (exit 2), tokens never in the file.
- **7 Experiment schema.** Every key and every validation rule, errors as `{path, message}`.
- **8.1 Routing.** Path, query and body preserved, `Host` rewritten, unknown route contract, unbuffered response and request streaming with a time-to-first-byte test.
- **8.2, 8.3 Scoping, matching, strategies.** Including other run ids, seeded probability, per-fault counters of matching requests, `X-Faultline-Fault`.
- **8.4, 8.5 All 13 fault types**, stream and non-stream for the LLM ones.
- **8.6 Leases.** `end_at` enforced by the proxy, lease expiry clears everything, kill within 1s, held connections released.
- **8.7, 8.8 Records and admin API.** Record fields, bounded ring, paging at 5000, no header values or bodies, 401 without a token.
- **9.1 Run lifecycle.** All statuses; one run at a time (409); precheck failure installs no faults; every listed abort path, including control killed with SIGKILL and faults cleared by lease.
- **9.2, 9.3 Driver and metrics.** Round-robin, rps, concurrency, scope and correlation headers, one-second buckets persisted per run.
- **9.4 Verdict rules**, in order.
- **9.5 Findings.** All seven rules with evidence; thresholds in one file.
- **9.6 Auth, audit, guards, redaction.** Constant-time compare; the five audit actions; a secret sent in `Authorization` is absent from `data_dir`, every API response, every report and the logs.
- **9.7 Control API.** Every endpoint, status code and field in the table.
- **10 Reports.** Canonical JSON with exactly the schema's fields; Markdown; self-contained HTML with inline SVG and no external reference; JUnit that parses; files under `{data_dir}/reports/{run_id}/`.
- **11 CLI.** Every command and flag; exit codes 0, 10, 11, 12, 2, 1; worst-of across files.
- **12 Web UI.** Token gate with `sessionStorage` and clear on 401; kill switch with confirmation; active run indicator; environment badge; runs list; run detail with live timeline; experiments editor with debounced validation; audit log; all nine `data-testid` values.
- **13 Demo.** Four servers, health endpoints, both resilience modes, LLM mock (text, stream, tools), header forwarding.
- **15 Quality bar.** `go vet` and `staticcheck` clean; race tests green; integration tests use real processes and sockets with nothing mocked; no test skipped, deleted or loosened; no TODO or stub left; graceful shutdown on SIGINT and SIGTERM for all four process types; structured logs.
- **16 Security.** README warning; bodies and redacted headers never persisted; tokens never logged; `127.0.0.1` by default; QA exposure documented.
- **17 Deliverables.** Every listed file and Makefile target.

## Not met, or only partly

- **macOS (4, 15).** Builds, never run. No macOS machine was available.
- **"Headers preserved byte for byte" (8.1).** Header values are preserved. Header names are not preserved byte for byte: Go's HTTP stack canonicalises their case (`x-custom` arrives as `X-Custom`) and does not keep their order. HTTP treats names as case-insensitive, so servers should not care, but it is not literally what the PRD says.
- **OpenAI format for LLM faults (8.5).** Stretch goal, not built.
- **MCP faults (M8).** Not attempted, see above.
- **Accessibility (12).** Built to the stated rules and one keyboard-only path is tested. No screen reader test and no automated audit was run, so "accessible" is asserted rather than verified.
- **The GitHub Actions workflow (M7).** Written, never executed: the repository has no remote.
- **"First useful result within 20 minutes on an existing project" (2).** Not measured. The quickstart takes under a minute against the demo; that says little about a real project.
- **UI smoke test browsers.** Chromium only.

## Risks I cannot close from here

The acceptance suite is hidden and I did not open `faultline-acceptance/`. Where the PRD left a contract open I took the simplest reading and recorded it. The readings most likely to disagree with a suite written from the same PRD:

| Decision | What I chose | What a suite might expect instead |
|---|---|---|
| D14 | `validate` prints `path: message` lines to stdout | stderr |
| D17 | A matching fault whose strategy does not fire ends matching for that request | Fall through to the next fault |
| D19 | Record `status` 0 for a response cut mid-body | The upstream status |
| D20 | `fault_id`, `fault_type` are `null` when no fault applied | Empty strings |
| D21, D22 | Shapes of `records` (`{records, next}`) and `stats` | Other field names; the PRD does not give them |
| D23 | Admin `PUT` requires `end_at` and `lease_id` | Optional |
| D31 | Control timestamps have second precision | Sub-second |
| D32 | Probes carry the run id | Unscoped probes |
| D39 | `up` prints tokens only when it generated them | Always |
| D40, D56 | 409 for aborting a finished run and for a report of a running run; 502 from the kill switch when the proxy is unreachable | Other codes |
| D5 | Relative `data_dir` resolves against the working directory | Against the config file |
| D8 | `*` in a path glob crosses `/` | Stops at `/` |

Other things worth knowing:

- Timing assertions (latency windows, lease expiry, time to first byte) have margins that held on this machine and in the container across every run. On a much slower or heavily loaded machine they could fail without a product defect.
- Throughput was never measured. The proxy has had no load test; the largest volume in any test is 5,200 requests.
- The record ring size (200,000) and the control's in-memory record list are not configurable. A long run at high request rates could exceed the ring, and findings would then be computed from the most recent records only.
- Fault windows are absolute times. Proxy and control on hosts with skewed clocks would start and stop faults early or late.
- On Windows a graceful stop of `proxy` or `control` is only possible through `faultline up` (stdin pipe) or Ctrl+C in a console. `Process.Kill` from another program is a hard kill; the lease then does the cleanup.

## Defects found by tests and reviews along the way

Listed because they show where the risk was: redaction could be bypassed with YAML aliases, merge keys and tagged keys (M0 review); the demo built URLs from an unescaped path value (M1 review); the garble mask barely depended on the seed (M2 tests); record strings were unbounded (M2 review); quoted `traffic.command` lines could not work on Windows (M4 test); `missing_timeout` was hidden by holds cut short at the end of injection (M5 design); the CLI trusted the run id returned by the server for file paths (M5 review); the `run_aborted` audit entry appeared after the aborted status (M6 race run).

## What I would do next

1. Run the hidden acceptance suite and reconcile the decisions in the table above with whatever it expects.
2. Push to a remote and get the CI workflow green, including the macOS job.
3. Load-test the proxy: latency overhead and throughput with and without an active run.
4. Make the record ring size configurable and have control persist the records it needs for findings, so long runs are not bounded by memory.
5. Write a contract for M8 before building it: fault types for MCP tool calls over Streamable HTTP (`tools/call` error results, slow tools, malformed results, dropped SSE streams), their parameters, and an MCP server in `faultline-demo` to test against.
6. OpenAI chat-completions format for the LLM faults.
7. An automated accessibility check (axe) in the Playwright run, and Firefox and WebKit projects.
