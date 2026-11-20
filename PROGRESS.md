# Progress

The sections M0 to M7 are the record of the first build and are left as written; where a "known gap" there has since been closed, the continuation sections ("Work item 1" to "Work item 7") and the final report say so, and they are the current state.

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

Not attempted in the first build, because the PRD gives it no contract. Built in the continuation: see "Work item 7" below.

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

## Work item 5: CI and macOS

**Closed on 2026-11-12, after the final report was first written.** When the work items were done the repository had no remote, so this item was left open. The owner then asked for it to be pushed.

What was done:

- Repository `ruelbelmonte/faultline` created, remote `origin` added. It was private for the first two CI runs and was made public afterwards, at the owner's request, so that Actions minutes are free.
- A machine-wide pre-push hook refuses direct pushes to `master`. It was not bypassed. `master` was created on GitHub at the first build's last commit (`2b7d542`), and the continuation was pushed as `feature/close-final-report-items` and opened as pull request #1, which is what triggers the workflow.
- First run of `.github/workflows/ci.yaml` on the pull request (run 43119795842, on commit `a5b61d9`): all seven jobs passed.
- **Correction.** Creating `master` at `2b7d542` also started a workflow run for that commit (run 43119782204), which I did not look at when I wrote "no change needed". In it `integration (windows-latest)` failed: `TestRunPassed`, "abort a finished run: expected 409, got 200". That is a real race in the abort endpoint, present since M3 and still present in the continuation's code; the pull request's three runs simply did not hit it. It is fixed in the section "After the merge" below.

| Job | Result |
|---|---|
| `lint-and-unit` (vet, gofmt, staticcheck, unit tests, cross-compile) | pass |
| `race` (unit and integration with `-race`) | pass |
| `integration (ubuntu-latest)` | pass |
| `integration (macos-latest)` | pass |
| `integration (windows-latest)` | pass |
| `ui` (dashboard build equals the committed one; Playwright smoke and axe audit in Chromium, Firefox, WebKit) | pass |
| `docker` (both images, compose example) | pass |

Known gaps:

- macOS is covered by the integration job only (real processes on `macos-latest`). The unit, race, UI and docker jobs run on Linux, and nobody has used the dashboard or the README quickstart on a Mac by hand.
- One run. A timing-sensitive test that passes once on a hosted runner can still flake later.
- Pull request #1 was merged on 2026-11-12.

## Work item 6: OpenAI format for LLM faults (PRD 8.5 stretch)

Built: `internal/proxy/llm_openai.go` (all six LLM fault types in OpenAI chat completions format, stream and non-stream, selected by a request path ending in `/chat/completions`), `internal/demo/openai.go` (OpenAI-compatible mock at `POST /v1/chat/completions` on the demo's `llm` server: text, stream, tool calls). Decisions D88 to D91; user documentation in `docs/faults.md`.

| Command | Where | Result |
|---|---|---|
| `go vet ./... && go vet -tags "integration load" ./test/...`, `gofmt -l cmd internal test` | Windows | clean |
| `go test ./... -count=1` | Windows | 148 pass, 0 fail, 0 skip |
| `go test -tags integration ./test/integration/ -count=1 -timeout 30m` | Windows | 87 top-level tests pass, 0 fail, 0 skip (229s) |
| `npx playwright test` in `ui/` | Windows, 3 browsers | 42 pass |
| `sh .tools/race.sh "make lint && make test-race"` | Linux container | lint clean; unit and integration ok with `-race` (260s) |
| compose example | Docker Desktop | passed, failed, failed, passed; exit 10 |
| acceptance suite | Windows | 56 passed, 0 failed, 0 skipped |

Gate, against the demo's OpenAI mock through a real proxy, stream and non-stream for each fault type: `TestChatRateLimit`, `TestChatOverloaded`, `TestChatStreamCut`, `TestChatSlowFirstToken`, `TestChatTruncate`, `TestChatMalformedToolCall`; `TestLLMFaultFormatFollowsThePath` (one fault, both APIs on one route, each answered in its own format); `TestChatFaultsThroughControl` (installed by a real run); `TestDemoChatCompletions` (the mock's shapes and stream timing). Unit tests for path detection, per-choice truncation, stream boundaries with several choices, and tool-call corruption. **Green.**

Known gaps:

- Only chat completions. The OpenAI Responses API and legacy completions are not covered and get the Messages API behaviour, which for the two rewriting faults means "left untouched".
- Tested against the demo mock only, as the brief requires: no real OpenAI endpoint was called. Shapes follow OpenAI's published format, including error objects; an OpenAI-compatible server that deviates from it may not be recognised by `llm_truncate` or `llm_malformed_tool_call`, which then relay the response unchanged.
- Streaming `llm_truncate` with several choices ends the stream at the first choice to reach the limit (D90), and no `usage` chunk is synthesised.

## Work item 7: M8, MCP tool faults

Precondition: work items 1 to 4 green (they are; item 5 was still open at that point for lack of a remote, item 6 green).

Contract first: `docs/mcp-faults.md`, written before any code. It specifies the four fault types, their parameters and validation rules, what makes a request an MCP tool call, and the MCP server in `faultline-demo`. Choices behind it: DECISIONS D92 to D98. No existing contract changed.

Built to that contract:

- `internal/fault`: `mcp_tool_error`, `mcp_slow_tool`, `mcp_malformed_result`, `mcp_stream_drop`, with parameter validation shared by the CLI, the control API and the proxy admin API.
- `internal/proxy/mcp.go` and `Engine.DecideCall`: being a `tools/call` (optionally for a named tool) is part of an MCP fault's match; the request body is read (at most 1 MiB, forwarded unchanged) only when an MCP fault matches on everything else.
- `internal/demo/mcp.go`: a stateless MCP server over Streamable HTTP at `/mcp` on the demo's `llm` server, with a tool that answers in JSON (`lookup_stock`) and one that answers with an event stream (`slow_report`).

| Command | Where | Result |
|---|---|---|
| `go vet ./... && go vet -tags "integration load" ./test/...`, `gofmt -l cmd internal test` | Windows | clean |
| `go test ./... -count=1` | Windows | 155 pass, 0 fail, 0 skip |
| `go test -tags integration ./test/integration/ -count=1 -timeout 30m` | Windows | 95 top-level tests pass, 0 fail, 0 skip (244s) |
| `npx playwright test` in `ui/` | Windows, 3 browsers | 42 pass |
| `sh .tools/race.sh "make lint && make test-race"` | Linux container | first run: 1 unit test failed (below); after the fix: lint clean, unit and integration ok with `-race` (271s) |
| compose example | Docker Desktop | passed, failed, failed, passed; exit 10 |
| acceptance suite | Windows | 56 passed, 0 failed, 0 skipped |

The Linux failure was in a new unit test, `TestDecideCallMatchesToolCallsOnly`, not in the product: the test took its "now" before installing the faults, and a fault starts at the moment it is installed, so on Linux's finer clock the request was a few microseconds too early to match. On Windows the two readings of the clock were equal. The test now takes its timestamp after the install; no assertion changed.

Gate, same standard as M4 (demo, proxy and control as real processes):

- each fault type against the demo MCP server, for the JSON tool and the stream tool: `TestMCPToolError`, `TestMCPSlowTool`, `TestMCPMalformedResult`, `TestMCPStreamDrop`;
- every parameter and default, and every validation rule: the tests above, `TestMCPFaultParams` (unit, 11 valid and 22 invalid cases), `TestMCPAdminValidation`;
- matching: `TestMCPMatching` (only tool calls are touched: `initialize`, `tools/list`, notifications, batches, non-JSON and oversized bodies pass; the tool filter; the strategy counts tool calls only; fall-through to a later fault; scoping; `expose_fault_header` off; the request body reaches the server unchanged) and unit `TestDecideCallMatchesToolCallsOnly`, `TestReadToolCall`;
- streams stay unbuffered through `mcp_malformed_result`; a pending `mcp_slow_tool` delay is released when the faults are cleared;
- installed by a real run: `TestMCPFaultsThroughControl` (passed, failed and inconclusive verdicts, and validation through the control API);
- the demo server against its own table in the contract: `TestDemoMCPServer`.

**Green.**

Known gaps:

- Tested against the demo MCP server only. No third-party MCP client or server (an SDK, a real agent) was put through the proxy, so interoperability beyond the shapes in the specification is unverified.
- Out of scope by contract (D98): JSON-RPC batches, the client's `GET` stream, stdio and legacy HTTP+SSE transports, MCP methods other than `tools/call`, session faults.
- The contract is mine. The PRD names the milestone and nothing else, so there is nothing external it was checked against; the acceptance suite does not cover M8.
- No example experiment for MCP was added under `examples/experiments/`, because that directory's documented results (four verdicts, exit code 10) are part of the README and the compose check. The example is in `docs/mcp-faults.md`.
- The dashboard needed no change (fault types are shown as reported), and no UI test exercises an MCP fault.

## After the merge: two timing defects found by CI

Pull request #1 was merged on 2026-11-12. Looking at the workflow history afterwards showed one failed run, for the first build's commit on `master` (see the correction under work item 5).

Defect: a run remains the active run while it writes its reports, after its terminal status is visible. An abort arriving in that window was accepted: it cancelled the finished run's context and answered 200 with the run's real status instead of 409 `run_not_active`. The run itself was not harmed (its verdict and status were already final), but the API answered wrongly, and a test that aborts a run right after seeing it finish could fail on a slow machine.

Fix: `handleAbort` treats a run with a terminal status as not active, and answers 200 only when the run really ended as aborted (D99).

| Command | Where | Result |
|---|---|---|
| `go test ./internal/control/ -run TestAbortOfARunThatIsFinishing` without the fix | Windows | fails (500 after the 20 s abort wait; in the real server the run would have been released and the answer would have been 200) |
| the same with the fix | Windows | passes |
| full gate | Windows, Linux container, compose, acceptance suite | green: 156 unit and 95 integration tests pass on Windows, 0 fail, 0 skip; 42 Playwright tests pass; lint clean and unit and integration ok with `-race` on Linux (272s); compose exit 10 as documented; acceptance 56 passed |

### A second one, on macOS

The workflow run on `master` after the merge (run 43129665351) failed in `integration (macos-latest)`: `TestRunFailed`, "expected every inventory call faulted with retries: {Requests:310 Faulted:307}". Three requests arrived after the fault window had ended and before control cleared the faults; the proxy correctly did not fault them, but control counted them in `metrics.proxy`. On a fast machine that gap is too short to catch a request, which is why it never showed locally or on the pull request's own macOS runs.

Fix: `metrics.proxy` and the proxy series now count requests that arrived during the injection phase, like the driver metrics; later records are kept for the finding rules only (D100). The test's assertion is unchanged.

| Command | Where | Result |
|---|---|---|
| `go test ./internal/control/ -run TestCollectCountsOnlyTheInjectionPhase` without the fix | Windows | fails: 4 requests counted, 2 faulted |
| the same with the fix | Windows | passes |
| full gate | Windows, Linux container, compose, acceptance suite | green: 157 unit and 95 integration tests pass on Windows, 0 fail, 0 skip; 42 Playwright tests pass; lint clean and unit and integration ok with `-race` on Linux (272s); compose exit 10 as documented; acceptance 56 passed |

Both fixes went in as pull request #2, merged on 2026-11-12. Its own CI run passed all seven jobs, and so did the run on `master` afterwards (run 43140395443, commit `543ac01`): the first fully green run on `master`, including `integration (windows-latest)` and `integration (macos-latest)`.

Known gaps: the integration test that caught the first one (`TestRunPassed`) is unchanged and still depends on timing to hit the window; the new unit test is what pins the behaviour. This also weakens one claim made earlier: "CI passed without a single fix" was true of the pull request's runs and not of the repository's whole CI history. Two of the first five workflow runs failed on a timing-dependent defect, one on Windows and one on macOS, and both were real. More of the same kind may exist: the hosted runners are slower and less regular than the machine everything was developed on, and each run is one sample.

---

# Final report

Rewritten on 2026-11-12 after the continuation work (work items 1 to 7). It replaces the report written at the end of the first build.

**In one paragraph.** All seven work items are closed with green gates: the acceptance suite passes 56 of 56, request headers are forwarded byte for byte, run records no longer have to fit in memory and the proxy has load numbers, the UI gate runs in three browsers with an accessibility audit, the LLM faults speak OpenAI chat completions, M8 (MCP tool faults) is built to a written contract, and the CI workflow is green on GitHub on its first run, including the macOS integration job. What is left is listed under "Not met, or only partly" and "What is left".

## Final verification

From a fresh `git clone` of commit `d773cdd` (`M8: MCP tool faults`), on 2026-11-12. The commits after it change `PROGRESS.md` alone. The same commit range then passed the CI workflow on GitHub (work item 5):

| Check | Linux (`golang:1.27` container) | Windows 11 |
|---|---|---|
| `go vet`, `gofmt`, `staticcheck` | clean (`make lint`) | vet and gofmt clean |
| `make build` | ok | not run (`make` is not installed); `go build` ok |
| Unit tests | ok (`make test`), ok with `-race` | 155 pass, 0 fail, 0 skip |
| Integration tests (real processes) | ok (`make test-integration`), ok with `-race` and race-built binaries (235s without, 275s with `-race`) | 95 top-level tests pass, 0 fail, 0 skip (244s) |
| Load test | ok (work item 3) | ok |
| Cross-compile linux, darwin, windows on amd64 and arm64 | not run | ok for all six |
| UI build equals committed `internal/ui/dist` | identical (work item 4, Playwright image) | no diff |
| Playwright: smoke and axe audit, Chromium, Firefox, WebKit | 42 pass (work item 4, not repeated on the final commit) | 42 pass, 0 fail, 0 skip |
| README quickstart, as written | not run | verdict passed, exit code 0, dashboard answers 200 (Git Bash) |
| Compose example, `faultline run examples/experiments/ --fail-on-breach` | containers on Docker Desktop | passed, failed, failed, passed; exit code 10, as documented |
| Acceptance suite (`faultline-acceptance/`, black box) | not run | 56 passed, 0 failed, 0 skipped (290s) |

## Definition of done

| Item | Status |
|---|---|
| Acceptance suite green, or every remaining failure explained as a PRD conflict | **Met.** 56 passed, 0 failed, 0 skipped, on the first run and after every later work item. No failure had to be explained. Run on Windows only. |
| All gates green from a fresh clone on Linux (with `-race`) and Windows | **Met**, table above. |
| macOS green in CI if a remote exists | **Met.** `integration (macos-latest)` passed in the first CI run, with the six other jobs (work item 5). |
| `make build test test-integration lint` succeed | **Met**, in a Linux container. `make` does not exist on the Windows machine. |
| Compose example and README quickstart still work verbatim | **Met.** Compose gives passed, failed, failed, passed and exit code 10, as the README documents (D73). The quickstart was run in Git Bash on Windows, not on Linux or macOS. |
| README and docs cover everything added | **Met.** Header fidelity and record storage in `docs/architecture.md` and `docs/qa-deployment.md`; OpenAI format and MCP in `docs/faults.md`; the MCP contract in `docs/mcp-faults.md`; new make targets and limits in `README.md`. |
| This report is complete and honest | Below. |

## Work items

| # | Item | Status | Evidence |
|---|---|---|---|
| 1 | Acceptance suite | Closed | 56/56. Nothing to reconcile. |
| 2 | Header fidelity (PRD 8.1) | Closed for requests, which is what 8.1 states; see "partly met" for the edges | Raw-socket integration tests, D80 |
| 3 | Limits and load | Closed | Records on disk (D82), ring warning (D83), load test and numbers (D84, work item 3 section) |
| 4 | UI verification | Closed | 42 Playwright tests in three browsers, axe on every page, zero violations (D85 to D87) |
| 5 | CI and macOS | Closed | Pull request #1, CI run 43119795842: seven jobs pass, macOS included |
| 6 | OpenAI format for LLM faults | Closed | D88 to D91, `TestChat*` |
| 7 | M8, MCP tool faults | Closed against its own contract | `docs/mcp-faults.md`, D92 to D98, `TestMCP*` |

## PRD requirements met

Each has at least one test that would fail if it broke, named in the milestone and work item sections above.

- **4 Technology.** Go, single module, two binaries, `faultline-demo` not imported by `faultline`. SQLite through `modernc.org/sqlite`, no cgo. Vue 3 + TypeScript + Vite, uPlot, embedded with `go:embed`. Two third-party Go modules, both justified. Multi-stage Dockerfiles on distroless. Cross-compiles for linux, darwin and windows on amd64 and arm64.
- **5 Architecture.** Proxy and control are separate processes; `up` only spawns them.
- **6 Configuration.** Every key, defaults, unknown keys rejected, production guard, token rules. No key was added in the continuation (D81).
- **7 Experiment schema.** Every key and validation rule. The continuation added four values for `faults[].type` (M8) and nothing else.
- **8.1 Routing.** Path, query, body and request headers preserved, the headers now byte for byte in name and order; `Host` rewritten; unknown route contract; unbuffered streaming in both directions.
- **8.2, 8.3 Scoping, matching, strategies.**
- **8.4, 8.5 All 13 fault types**, the six LLM ones in Anthropic Messages format and, as the stretch goal, in OpenAI chat completions format, stream and non-stream.
- **8.6 Leases.** `end_at` enforced by the proxy, lease expiry clears everything, kill within 1 s, held connections released.
- **8.7, 8.8 Records and admin API.**
- **9.1 to 9.7 Control plane.** Lifecycle, driver, metrics, verdict rules, the seven finding rules (now computed from records stored on disk), auth, audit, guards, redaction, every API endpoint.
- **10 Reports.** JSON, Markdown, self-contained HTML, JUnit.
- **11 CLI.** Every command, flag and exit code.
- **12 Web UI.** Every listed element and `data-testid`; tested in Chromium, Firefox and WebKit; axe finds no WCAG 2.1 A or AA violation on any page in light or dark mode.
- **13 Demo.** Four servers, both resilience modes, LLM mock. Added without changing its command line: an OpenAI-compatible endpoint and an MCP server on the `llm` server.
- **14 Milestones.** M0 to M7, and M8 as specified in `docs/mcp-faults.md`.
- **15 Quality bar.** `go vet` and `staticcheck` clean; race tests green; integration tests use real processes and sockets; no test skipped, deleted or loosened; structured logs; graceful shutdown.
- **16 Security.** Bodies and redacted headers never persisted; tokens never logged; loopback by default. New in the continuation and consistent with it: rows stored during a run carry no method, path, status or correlation id (D82), and the one case where the proxy reads a request body, to recognise an MCP tool call, keeps it in memory only (D93).
- **17 Deliverables.**

## Not met, or only partly

- **macOS beyond the integration job (4, 15).** The integration tests pass on `macos-latest` in CI. Nothing else has run on macOS: not the unit or race jobs, not the UI tests, not the quickstart by hand.
- **Header fidelity edges (8.1).** Names and order are exact. Normalised: whitespace around values, line endings, and the chunk boundaries of a chunked request body. Fallback to Go's canonical, sorted names: a request with an obsolete folded header (and the rest of that connection), and an `https` upstream reached through `HTTPS_PROXY`. **Response** header names are still canonicalised and sorted on the way back; 8.1 does not ask for them, and it is not done. `https` upstreams are now always HTTP/1.1.
- **Long runs (work item 3).** Request records are on disk, which removed the largest term. Memory still grows by about 50 bytes per driver request and 8 bytes per retry gap. The proxy's ring is still 200,000 records and not configurable; if it ever overflows, the only signal is a warning in the proxy log, not something in the report.
- **Load numbers.** One machine, one run per row, client and server sharing cores. An order of magnitude (about 0.25 to 0.4 ms added per request, 16,000 to 24,000 requests per second through one proxy), not a capacity figure. No test with large bodies, streams or TLS upstreams under load.
- **Accessibility (12).** Verified by axe and by keyboard tests, in three browsers. Not verified: use with a screen reader, and the canvas timeline, which axe cannot judge (its numbers are repeated in a table). Safari on macOS itself was not tested; Playwright's WebKit stands in.
- **OpenAI format (8.5).** Chat completions only, and only against the demo mock. The Responses API is not covered.
- **MCP (M8).** Built and tested against a contract I wrote and a demo server I wrote. No third-party MCP client or server has been through the proxy. Batches, the `GET` stream, other transports and other MCP methods are out of scope by that contract.
- **"First useful result within 20 minutes on an existing project" (2).** Still not measured.
- **UI editor and secret headers (D38).** An experiment that needs a sensitive header value still has to be run from a file.

## Risks that remain

The acceptance suite no longer counts as an unknown: it passes. What it leaves open is narrower than before but not empty. It did not contradict any of the readings below; it also may simply not probe them, and I did not read its source to find out.

| Decision | What I chose | What a reader of the PRD might expect instead |
|---|---|---|
| D5 | Relative `data_dir` resolves against the working directory | Against the config file |
| D8 | `*` in a path glob crosses `/` | Stops at `/` |
| D17 | A matching fault whose strategy does not fire ends matching for that request | Fall through to the next fault |
| D31 | Control timestamps have second precision | Sub-second |
| D32 | Probes carry the run id | Unscoped probes |
| D39 | `up` prints tokens only when it generated them | Always |

Other things worth knowing:

- **Two agents worked in this directory for a few minutes.** At the start of the continuation a second session had been given the same prompt in this working tree. It ran the baseline gate in parallel and collided with mine on the compose project (see "Continuation baseline"). It was interrupted before it changed any tracked file; it left a Python virtualenv in the git-ignored `.tools/accept-venv`, which is unused and can be deleted. Every commit after `2b7d542` is from this session.
- The Firefox page crash described in D86 is worked around, not explained. The evidence says it is the test tooling in that browser build and not the dashboard, but the cause inside Firefox was not found.
- Timing assertions have margins that held on this machine and in the container on every run. On a much slower or heavily loaded machine they could fail without a product defect. One did surface in the other direction during M8: a unit test that passed on Windows' coarse clock and failed on Linux, which was a mistake in the test.
- Fault windows are absolute times. Proxy and control on hosts with skewed clocks would start and stop faults early or late.
- On Windows a graceful stop of `proxy` or `control` is only possible through `faultline up` or Ctrl+C in a console; a kill from another program is a hard kill and the lease then does the cleanup.
- The module path is still `faultline` (D1). The repository now has a home (`github.com/ruelbelmonte/faultline`); renaming the module to match is one search and replace and was not done, because nobody asked for it and it touches every file.

## Defects found along the way

From the first build: redaction could be bypassed with YAML aliases, merge keys and tagged keys; the demo built URLs from an unescaped path value; the garble mask barely depended on the seed; record strings were unbounded; quoted `traffic.command` lines could not work on Windows; `missing_timeout` was hidden by holds cut short at the end of injection; the CLI trusted the run id returned by the server for file paths; the `run_aborted` audit entry appeared after the aborted status.

From the continuation: aborting a run in the moment between its terminal status and its release answered 200 instead of 409 (found by CI on Windows, fixed, D99); requests arriving between the end of the fault window and the clearing of the faults were counted in `metrics.proxy` as unfaulted (found by CI on macOS, fixed, D100); the proxy forwarded a `Cache-Control: no-cache` header that Go's server adds to requests carrying only `Pragma: no-cache` (found while making headers exact, fixed); the acceptance suite, the three-browser run and axe found no product defect.

## What is left

1. Nothing is waiting on a merge: pull requests #1 and #2 are in `master`, and `master` is green in CI. Keep an eye on later CI runs: a failure there is most likely a third timing defect of the kind described under "After the merge".
2. Response header fidelity, if exact response names matter to a system under test: the request mechanism mirrored (D80).
3. A real MCP client and a real OpenAI-compatible server through the proxy, to check the two stretch features against something I did not write.
4. A load test on separate machines, with streams and large bodies, repeated enough to give intervals.
5. A screen reader pass over the dashboard.

# Goal 1: security hardening and CI scanning

Started 2026-11-14 from the repository audit; issues #9, #10, #12, #13, #11 and #23 (the P1 items of epics #5 and #6). One commit per issue on `claude/practical-maxwell-z68amm`, in pull request #37, because this session could push to one branch only.

## Status

| Issue | Commit | Status | Decision | Tests that would fail if it broke |
|---|---|---|---|---|
| #9 traffic.command opt-in | eaa9938 | Done | D101 | `TestCommandTrafficRejectedByDefault`, `TestAllowCommands`, `TestTrafficCommandRejectedByDefault` (integration) |
| #10 tokens out of child environments | 79376e4 | Done | D102 | `TestEnvWithoutTokens`, `TestUpChildEnvTokens`, `TestTrafficCommandEnvHasNoTokens`, `TestUpChildrenGetOnlyTheTokensTheyNeed` (Linux, `/proc`) |
| #12 upstream errors logged without URL | bdc00f1 | Done | D103 | `TestUpstreamErrorLogHasNoURL` (text and JSON) |
| #13 redaction of bodies, commands, URLs | 72ef38a | Done | D104 | `TestRedactBodiesCommandsAndURLs`, `TestRedactURL`, `TestWarningsForCredentialsInURLs`, `TestRedactionOfSecrets` (integration, extended) |
| #11 allowed_hosts and metadata block | 0d90f8e | Done | D105 | `TestCheckHostMetadataRefusedByDefault`, `TestCheckHostAllowList`, `TestAllowedIP`, `TestAllowedHostsConfig`, `TestHostAllowListInValidation`, `TestDialRefusesMetadataAddresses`, `TestHostWarning` |
| #23 CI scanning | b50bbb7 | Done, except two repository settings (below) | D106 | The CI jobs themselves |

Every new test was written before the fix and seen failing against the old code: build failures for new functions, and for behaviour, an explicit run with the fix reverted (#10's two integration tests, #12's log test, #13's integration test with the new redaction steps disabled, which reported the secret in the SQLite WAL, the saved experiment file and the experiments API).

## Acceptance criteria, as written in each issue

**#9**
- [x] With `allow_commands` unset, an experiment with `traffic.command` posted to `/api/v1/runs` gets 400, and to `/api/v1/experiments/validate` gets `valid:false`, both with path `traffic.command`: `TestTrafficCommandRejectedByDefault` (integration), `TestCommandTrafficRejectedByDefault`.
- [x] With `allow_commands: true`, the existing command-mode tests still pass: `TestTrafficCommand`, `TestCommandTraffic`.
- [x] `faultline init` output stays valid: `TestInitOutputValidates`.

**#10**
- [x] A command that prints its environment shows neither token: `TestTrafficCommandEnvHasNoTokens` (integration, all 3 OSes), `TestEnvWithoutTokens`.
- [x] The proxy child of `up` has no `FAULTLINE_TOKEN`: `TestUpChildrenGetOnlyTheTokensTheyNeed` (integration, Linux, from `/proc/<pid>/environ`), `TestUpChildEnvTokens`.

**#12**
- [x] An upstream failure with `?secret=abc` in the query: `abc` is absent from the proxy log in text and JSON formats: `TestUpstreamErrorLogHasNoURL`.

**#13**
- [x] A secret in a body, a command and a URL is absent from the SQLite database (and its WAL), all 4 report formats of both runs and the audit log (file and API): `TestRedactionOfSecrets` (integration), `TestRedactBodiesCommandsAndURLs`.

**#11**
- [x] Validation returns path-specific errors for disallowed hosts: `TestHostAllowListInValidation`.
- [x] 169.254.169.254 is rejected by default: `TestCheckHostMetadataRefusedByDefault`, and at connect time `TestDialRefusesMetadataAddresses`.
- [x] Docs updated: `docs/qa-deployment.md`, `docs/experiments.md`, README.

**#23**
- [x] All new jobs green on the pull request: run 43657493407.
- [x] #37 merged into `master` (2519ab7, 2026-11-15), closing #9, #10, #11, #12, #13 and #23.
- [x] The first Dependabot pull requests open: #38 to #42, opened at 06:33 UTC on 2026-11-15 (actions/setup-go, node base image, actions/setup-node, actions/checkout, typescript). They are major-version bumps and need reviewing like any other change; none was merged as part of this goal.
- [ ] Secret scanning and push protection turned on. **Dated exception, owner action by 2026-11-22:** a repository setting (Settings, Code security) that the tools available to this work cannot change. The owner has taken it on.

## Gates

- Local, per commit, in a clean worktree with `GOTOOLCHAIN=go1.27.4`: `make lint`, `make test`, `go test -race` on the packages touched, `make test-integration`. Green for every code commit on its own: eaa9938 (#9), 79376e4 (#10), bdc00f1 (#12), 72ef38a (#13) and 0d90f8e (#11). b50bbb7 (#23) changes CI files, the Makefile and docs only; its gate is the CI run.
- CI on pull request #37: all 8 jobs green on ec079e9, run 43657004856 (https://github.com/ruelbelmonte/faultline/actions/runs/43657004856): `lint-and-unit` (vet, gofmt, staticcheck, unit tests, cross-compile for 6 targets), `govulncheck`, `race` (unit and integration), `integration` on ubuntu, macos and windows, `ui` (npm audit, committed build matches source, Playwright and axe in 3 browsers) and `docker` (both images built, Trivy scans of both, compose example). The first run, 43656566339 on b50bbb7, failed on macOS integration only, through a test defect described below.
- `npm audit --audit-level=high`: 0 vulnerabilities, locally and in CI. govulncheck and both Trivy scans: no findings at or above their thresholds in the first CI run, so `.trivyignore.yaml` starts empty.

One test defect found by CI, fixed: in the first CI run, `TestTrafficCommandEnvHasNoTokens` failed on macOS only (run 43656566339, job `integration (macos-latest)`) with an empty output file. The test's command was `env > file`. Control re-runs it in a loop and kills its process group when the run ends, and on macOS the kill landed after the shell had truncated the file but before `env` wrote it. The command now appends (`env >> file`), as `TestTrafficCommand` already did, so every earlier complete write survives and the check still scans all of them for tokens. Not a product defect: the product behaved as designed.

One local environment problem, not a product defect: `go run honnef.co/go/tools/cmd/staticcheck@v0.8.2` under `GOTOOLCHAIN=auto` switched to go1.26 (staticcheck's own minimum) and then failed to analyse go1.27 sources. CI installs go1.27 directly and does not hit it. Locally, set `GOTOOLCHAIN=go1.27.4` (or newer) before `make lint`. `govulncheck` cannot run in this container at all, because the network policy refuses vuln.go.dev.

## Not done, and why

- **Secret scanning and push protection (#23).** They are repository settings, not files, and the goal allowed changes on a pull request branch only. Turn them on in Settings, Code security. On a public repository secret scanning may already be on; push protection should be checked.
- **Base images pinned by digest.** Left to #24, which owns the release pipeline and asks for it.
- **Acceptance suite.** The black-box suite in `faultline-acceptance/` is not in this repository and was not run. It may contain experiments with `traffic.command`, which now need `control.allow_commands: true` in its config.

## New risks and consequences found

- **The dashboard editor loses more than before (#13, D104).** Request bodies and commands now come back from `GET /api/v1/experiments/{name}` as fingerprints, so any experiment with a body has to be run from its file, and saving it from the editor writes the fingerprints over the file. Issue #15 (PUT overwrites the original) was P2; it should now be treated as P1.
- **Fingerprints of guessable values (#13).** A plain SHA-256, as the issue asks, so a short body such as a lone weak password can be recovered by trying candidates. D104 records a keyed HMAC as the next step.
- **Breaking change for command users (#9).** Existing experiments with `traffic.command` stop validating until `control.allow_commands: true` is set. Release notes for the next version need to say so.
- **allowed_hosts covers experiments, not routes (#11).** Route upstreams come from `faultline.yaml` and are not checked against it; the metadata block does not apply to the proxy's own upstream connections. A config author can still point a route at a metadata address. That is a config-file trust question rather than a token-holder one, so it was left out; worth a follow-up issue if config files are not fully trusted.
- **Error classes lose detail (#12).** Operators debugging a failing upstream now get `connection_refused` or `tls` instead of the full message, and have to reproduce against the upstream directly.

# Goal 2: product loop and release pipeline

Started 2026-11-15, after Goal 1 was merged to `master` (cdda636): issues #28, #29 and #24. Built as one commit per issue on `claude/practical-maxwell-z68amm` (#44). On 2026-11-20 rebased onto `master`, which renumbered D107 to D110 as D115 to D118, and split into one pull request per issue: #59, #60 and #61.

## Status

| Issue | Pull request | Status | Decisions |
|---|---|---|---|
| #28 run-over-run comparison | #59 | Done | D115, D116 |
| #29 W3C baggage scoping | #60 | Done | D117 |
| #24 release pipeline | #61 | Done; v0.1.0 published 2026-11-20 | D118 |

## Acceptance criteria, as written in each issue

**#28**
- [x] Running the same experiment twice gives a report that names the earlier run, with correct differences: `TestRunOverRunComparison` (integration), `TestCompare`, Playwright `run comparison and per-experiment history`.
- [x] A new finding appearing makes `--fail-on-regression` exit non-zero, with a new documented exit code (13, D116): `TestRunOverRunComparison` (exit 13 on a regression, 0 without the flag, 10 outranking 13), `TestCompare` (an appeared finding is a regression).
- [x] Report schema change is additive (D115; `schema_version` stays `"1"`, `comparison` is the 16th key): `TestJSONMatchesCanonicalSchema`, `TestReports`.
- [x] Also from the goal: grouping by experiment name and `yaml_sha256` (`TestStoreBaselinesAndHistory`), `--baseline previous|last-passed|<run id>`, per-experiment history in the UI, comparison carries nothing redaction removed (`TestCompareCarriesNoEvidence`).

**#29**
- [x] A three-hop demo forwarding only `baggage` still triggers faults and the run fails as expected: `TestBaggageScopingAcrossThreeHops` (seen failing with the proxy's baggage lookup disabled), with `TestBaggageTurnedOffDoesNotScope` as the control.
- [x] Malformed baggage is ignored safely and the parser is bounded: `TestGet`, `TestGetBounded`, `TestScopeFromHeaderOrBaggage`.
- [x] `docs/faults.md` and the README "Scoping across hops" section updated.

**#24**
- [x] Pushing `v0.1.0` produces signed binaries, images, an SBOM and provenance, and `cosign verify` succeeds: see "v0.1.0 published" below.
- [x] The README quickstart works without a Go toolchain: a release-download path sits beside build-from-source, to be tried once the first release exists.
- [x] Also from the goal: base images pinned by digest, version from the tag, six platforms, checksums, GHCR images, SBOM, keyless cosign, provenance, dry run that skips publishing.

## Gates

- Local, per commit, in a clean worktree: `make lint`, `make test`, `go test -race` on the packages touched, `make test-integration`. Green for #28 (the Go content of 7cbfebb) and #29 (675e795): both `GATE OK` (#28 on 9c4f680, #29 on 6db0a08, whose Go content is identical to 7cbfebb and 675e795; the later rewrite only changed the Playwright spec). #24 changes workflows, Dockerfiles and docs only; its gate is CI (the `docker` job builds both Dockerfiles with the digest pins and cross-compiling stages, and the `release` dry run builds everything a release would).
- Playwright, locally in Chromium: the new comparison test and the full axe audit pass. In CI the first run failed in Firefox and WebKit only: the three browser projects share one control plane, so the "first" run in the second and third browsers had the first browser's runs as a baseline. The test now uses an experiment per browser. A test defect, not a product one.
- CI on pull request #44, head b3b0b2c: all 8 `ci` jobs green, run 43699930008 (https://github.com/ruelbelmonte/faultline/actions/runs/43699930008), including `ui` (Playwright and axe in Chromium, Firefox and WebKit) and `docker` (both digest-pinned Dockerfiles). The `release` dry run, run 43699930018 (https://github.com/ruelbelmonte/faultline/actions/runs/43699930018): `plan`, `binaries` (twelve binaries, version check, SBOM, checksums) and both multi-platform `images` green, `release` skipped as designed.
- `actionlint` (v1.7.7) reports nothing for `ci.yaml` and `release.yaml`.
- After the rebase onto `master` (2026-11-20), on the full stack: native Windows `go vet`, `go test ./...` and `make test-integration`, and in Docker `make lint` and `make test-race`. All green.

## Not done, and why

- **Publishing v0.1.0.** Reserved for the owner by the goal; the owner had Claude push the tag on 2026-11-20 (see "v0.1.0 published" below).
- **Manual `workflow_dispatch` dry run.** Not possible before the workflow is on `master`; the pull request trigger runs the same dry run instead.

## Steps to cut v0.1.0

1. Merge #59, #60 and #61, in that order. Check that `master` is green in CI.
2. Optional: Actions, release, Run workflow on `master` with version `0.1.0-rc.1-dryrun`, and download the `dist` artifact to try a binary.
3. Tag and push from an up-to-date `master`:
   ```sh
   git checkout master && git pull
   git tag -a v0.1.0 -m "Faultline 0.1.0"
   git push origin v0.1.0
   ```
4. Watch the release workflow (plan, binaries, images, release).
5. On GHCR, the `faultline` and `faultline-demo` packages start private: make them public if anyone should pull them without logging in, and link them to the repository.
6. Edit the generated release notes to call out the breaking change: experiments with `traffic.command` need `control.allow_commands: true` (D101). Mention the new `comparison` key and exit code 13 (D115, D116) and baggage scoping (D117).
7. Verify:
   ```sh
   cosign verify ghcr.io/ruelbelmonte/faultline:0.1.0 \
     --certificate-identity-regexp '^https://github.com/ruelbelmonte/faultline/\.github/workflows/release\.yaml@refs/tags/v' \
     --certificate-oidc-issuer https://token.actions.githubusercontent.com
   gh attestation verify oci://ghcr.io/ruelbelmonte/faultline:0.1.0 --repo ruelbelmonte/faultline
   ```
   and follow the README quickstart's release path on a machine without Go.

## New risks and consequences found

- **Report schema now has 16 keys (D115).** Any consumer, including the external acceptance suite, that checks for exactly 15 needs updating.
- **The demo forwards `baggage` by default now (D117).** Harmless for the demo, but anyone copying its client code should know.
- **Release depends on GHCR package visibility and naming.** Expected private packages on first publish; v0.1.0's came out public.
- **Dependabot's major-version PRs (#38 to #42) touch the same files** (`actions/*` pins, the node base image). Merge #59 to #61 first, then rebase or let Dependabot recreate them, so the digest pins and the new workflow are updated together.

## v0.1.0 published (2026-11-20)

- Before tagging: the `workflow_dispatch` dry run on `master` (run 44688233841) and `master` CI on b2f466b were green.
- `v0.1.0` tagged on b2f466b. Release run 44692138005: `plan`, `binaries`, both `images` and `release` green; 14 assets.
- Checked from a clean directory: `sha256sum -c checksums.txt` OK for the Linux and Windows amd64 binaries, `faultline version` prints `faultline 0.1.0`, `gh attestation verify` passes for the Windows binary and `oci://ghcr.io/ruelbelmonte/faultline:0.1.0`, and `cosign verify` (v3.0.6) passes for both images. cosign v2.4.1 reports "no signatures found", because v3 stores the signature in the new bundle format; `docs/releasing.md` now says to use cosign 3.x.
- Release notes gained upgrade notes (D101, D110, D115 to D117, D120) and the verification commands.
- Both GHCR packages are public.
- README quickstart, release path, run as written in an empty directory with no Go on `PATH` (Windows amd64, Git Bash): checksums OK, `faultline run` verdict `passed`, exit 0, findings `retry_without_backoff` and `retry_after_ignored`. This meets the #24 criterion "the README quickstart works without a Go toolchain".
