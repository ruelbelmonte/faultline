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
