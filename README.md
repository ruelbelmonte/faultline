# Faultline

Faultline is a fault-injection proxy and control plane for application-level chaos engineering. You run it next to an existing API, point that API's upstream URLs at the proxy, and run experiments written as YAML. Faultline injects faults into the dependency calls, drives traffic, checks a steady-state hypothesis, aborts when things go too far, and gives you a verdict, findings backed by recorded evidence, a dashboard and reports.

> **Test environments and synthetic data only.**
> Do not point Faultline at an environment that holds real personal, health or financial data. It refuses to start when `environment` is `prod`, `production` or `live`, but that guard is a name check, not a guarantee. Faultline makes no compliance claim of any kind; whether a given environment may be used is your call.

## How it works

```
 CLI / CI      Web UI
     \           |
      v          v
   +------------------------------------+
   | Control plane  (faultline control) |  :7700  API + UI, runs, driver, probes,
   +------------------------------------+         metrics, verdicts, reports, audit
                  | admin API (token, lease)
                  v
   +------------------------------------+
   | Proxy data plane (faultline proxy) |  :7701 traffic   :7702 admin
   +------------------------------------+
     ^                         |
     | service calls           v
  System under test     Upstream dependencies
```

- **No code changes.** Integration is configuration: an upstream URL points at `http://<proxy>:7701/<route>`.
- **Faults touch only the run's own traffic.** A fault applies when a request carries the run id in the scope header (`X-Chaos-Run` by default). Everything else passes through untouched.
- **Faults cannot outlive their run.** The proxy enforces each fault's end time itself, and clears every fault if the control plane stops renewing its lease. Proxy and control are separate processes, so the data path stays up when control dies.

## Quickstart

You need Go 1.27 or newer (the version in `go.mod`). The commands are for a POSIX shell (Linux, macOS, WSL, Git Bash on Windows). Run them from the repository root.

```sh
go build -o bin/ ./cmd/faultline ./cmd/faultline-demo     # or: make build
export PATH="$PWD/bin:$PATH"
export FAULTLINE_TOKEN=$(openssl rand -hex 32)
export FAULTLINE_ADMIN_TOKEN=$(openssl rand -hex 32)

mkdir quickstart && cd quickstart
faultline init
faultline-demo &
faultline up &
sleep 3
faultline run experiments/example.yaml --fail-on-breach
echo "exit code: $?"
```

What this does:

1. `faultline init` writes `faultline.yaml`, `experiments/example.yaml` and a `.gitignore` entry for the data directory.
2. `faultline-demo` starts a small demo system: shop (9000) calls inventory (9001) calls pricing (9002), each through the proxy, plus an LLM mock (9003) that speaks the Anthropic Messages API and OpenAI chat completions and hosts a small MCP server at `/mcp`.
3. `faultline up` starts the proxy and the control plane as two child processes.
4. `faultline run` runs the example: 30% of inventory calls return 503. The demo shop retries, so orders keep working. The verdict is `passed` and the exit code is 0.

Open the dashboard at <http://127.0.0.1:7700> and sign in with the value of `$FAULTLINE_TOKEN` (`echo $FAULTLINE_TOKEN`).

To stop everything: `kill %1 %2`.

If you start `faultline up` without setting the two token variables, it generates them and prints them once.

### With Docker Compose

```sh
export FAULTLINE_TOKEN=$(openssl rand -hex 32)
export FAULTLINE_ADMIN_TOKEN=$(openssl rand -hex 32)
docker compose -f examples/docker-compose.yaml up --build -d
faultline run examples/experiments/ --fail-on-breach
echo "exit code: $?"
```

Demo, proxy and control run in three containers. The four example experiments give these results against the demo in its default `naive` mode, and the command exits with **10** because two of them fail on purpose:

| Experiment | Verdict | Findings |
|---|---|---|
| `01-inventory-flaky` | passed | `retry_without_backoff` |
| `02-inventory-outage` | failed | `steady_state_breach`, `retry_amplification`, `retry_without_backoff`, `retry_after_ignored` |
| `03-inventory-hang` | failed | `steady_state_breach`, `missing_timeout`, `retry_amplification`, `retry_without_backoff` |
| `04-llm-slow-first-token` | passed | none |

Run the stack with `DEMO_MODE=good` to see the same experiments against a client that has timeouts, backoff and Retry-After handling: the retry and timeout findings disappear. Stop with `docker compose -f examples/docker-compose.yaml down -v`.

## Using it with your own service

1. Run `faultline init` in your repository.
2. Add one route per dependency to `faultline.yaml`:

   ```yaml
   routes:
     - name: inventory
       upstream: http://inventory.internal:8080
     - name: anthropic
       upstream: https://api.anthropic.com
   ```

3. Change your service's upstream URLs to go through the proxy: `http://127.0.0.1:7701/inventory`, `http://127.0.0.1:7701/anthropic`. A request to `/inventory/stock/1?x=1` is forwarded to `http://inventory.internal:8080/stock/1?x=1` with method, headers, query and body unchanged and `Host` rewritten. Responses are streamed, never buffered.
4. `faultline up`, then `faultline run experiments/your-experiment.yaml`.
5. Add `faultline run experiments/ --fail-on-breach --report-dir reports` to CI.

### Scoping across hops

A fault applies only to requests that carry the run id in the scope header. The traffic driver and the probes send it. **Faultline cannot add it for you between your services.** If service A calls service B, and B calls a dependency through the proxy, then A and B must forward the incoming `X-Chaos-Run` header (and `X-Chaos-Correlation`, which groups the attempts of one logical request) on their outgoing calls, the way you already forward `traceparent`.

If your services already propagate OpenTelemetry baggage, the simplest way is to carry the run id as a baggage entry and copy it into the `X-Chaos-Run` header in your HTTP client middleware. The proxy itself matches on the header only; it does not read baggage.

If a hop drops the header, faults behind that hop never trigger. The run then ends `inconclusive` with reason `fault_not_triggered`, which is the signal to look for a missing header.

## Experiments, faults, findings

- [docs/experiments.md](docs/experiments.md): the experiment file, validation rules, the run lifecycle, verdict rules and findings.
- [docs/faults.md](docs/faults.md): every fault type, matching, strategies and scoping.
- [docs/architecture.md](docs/architecture.md): components, the lease, storage, APIs.
- [docs/qa-deployment.md](docs/qa-deployment.md): running proxy and control in a shared QA environment.
- [examples/terraform-snippet.md](examples/terraform-snippet.md): pointing an upstream URL at the proxy from infrastructure code.

Fault types: `latency`, `http_error`, `error_in_body`, `timeout`, `reset`, `slow_body`, `corrupt_body`, and for LLM APIs (Anthropic Messages and OpenAI chat completions) `llm_rate_limit`, `llm_overloaded`, `llm_stream_cut`, `llm_slow_first_token`, `llm_truncate`, `llm_malformed_tool_call`. For MCP tool calls over Streamable HTTP there are `mcp_tool_error`, `mcp_slow_tool`, `mcp_malformed_result` and `mcp_stream_drop`; their contract is [docs/mcp-faults.md](docs/mcp-faults.md).

## CLI

```
faultline version
faultline init [dir]
faultline validate <file|dir> [--config FILE]
faultline up [--config FILE]
faultline proxy [--config FILE]
faultline control [--config FILE]
faultline run <file|dir> [--fail-on-breach] [--report-dir DIR] [--format json,md,html,junit] [--url URL]
faultline status [--url URL]
faultline stop [--url URL]
faultline report <run_id> --format F [--out FILE] [--url URL]
```

`proxy`, `control` and `up` also take `--log-format text|json`.

| Variable | Used by | Meaning |
|---|---|---|
| `FAULTLINE_TOKEN` | `control`, remote commands | Bearer token for the control API. At least 16 characters. |
| `FAULTLINE_ADMIN_TOKEN` | `proxy`, `control` | Bearer token for the proxy admin API. At least 16 characters. |
| `FAULTLINE_URL` | remote commands | Control plane URL. Default `http://127.0.0.1:7700`. `--url` overrides it. |

Tokens never go in `faultline.yaml`.

Exit codes of `faultline run`:

| Code | Meaning |
|---|---|
| 0 | Every run passed, or `--fail-on-breach` was not given |
| 10 | With `--fail-on-breach`: a run failed |
| 11 | With `--fail-on-breach`: a run was inconclusive |
| 12 | With `--fail-on-breach`: a run was aborted |
| 2 | Invalid experiment or usage error |
| 1 | Internal or connection error |

Across several files the worst result wins: 12, then 10, then 11, then 0. `--report-dir` writes `DIR/<run_id>/report.json`, `report.md`, `report.html` and `report.xml` (JUnit). `faultline stop` is the kill switch: it clears every fault and aborts the active run.

## Safety model

| Mechanism | What it does |
|---|---|
| Header scoping | Faults apply only to requests carrying the run id. `scope: all` is rejected unless `control.allow_unscoped: true`. |
| Fault end time | Each fault has an absolute `end_at` that the proxy enforces itself. |
| Lease | Control renews a lease every `lease_timeout / 3`. With no renewal for `lease_timeout`, the proxy clears all faults. |
| Kill switch | `faultline stop`, the dashboard button, or `POST /api/v1/kill`. New requests are unaffected within a second; held connections are released. |
| Abort thresholds | A run aborts itself when the error rate or consecutive probe failures cross the limits in `abort`. |
| Environment guard | `proxy`, `control` and `up` exit with code 2 when `environment` is `prod`, `production` or `live` (any case). |
| One run at a time | Creating a run while another is active answers 409. |

## Security and data handling

- Bodies are never stored. The proxy looks inside a request body in one case only: to tell whether a request is an MCP tool call, when an MCP fault of the active run otherwise matches it (at most 1 MiB, in memory, forwarded unchanged). The proxy records method, path (without query string), status, timing, the correlation id and whether a fault applied. It does not record header values.
- Values of headers listed in `redaction.headers` are replaced with `[REDACTED]` wherever they could be stored or shown, including inside a saved experiment. The driver keeps the real value in memory only.
- Tokens are compared in constant time and are never logged.
- All listeners bind to `127.0.0.1` by default. To expose them in QA, see [docs/qa-deployment.md](docs/qa-deployment.md): restrict the network, terminate TLS in front of the control plane, and keep the proxy admin port private.
- The audit log (`{data_dir}/audit.jsonl` and SQLite) is append-only from the application's side. It is not tamper-proof against someone with access to the files.
- The experiment file's `traffic.requests[].body` is stored as part of the experiment. Keep it synthetic.

## Development

```sh
make build              # both binaries into bin/
make test               # unit tests
make test-integration   # real processes on free ports
make test-race          # unit and integration tests with the race detector (needs cgo)
make test-load          # proxy latency overhead and throughput, with and without an active run
make test-ui            # Playwright smoke test and axe accessibility audit in Chromium, Firefox and WebKit (needs Node)
make lint               # go vet, gofmt, staticcheck
make ui                 # rebuild the dashboard into internal/ui/dist
make docker             # build both images
```

The dashboard's built assets are committed under `internal/ui/dist`, so building the binaries needs Go only. `PROGRESS.md` has the gate results per milestone and `DECISIONS.md` records every choice the specification left open.

## Limitations

- Reverse-proxy mode only. No transparent HTTPS interception, no protocol upgrades (WebSocket), no `CONNECT`.
- LLM faults understand the Anthropic Messages API and OpenAI chat completions (`/chat/completions`). Other OpenAI endpoints, such as the Responses API, are not covered.
- One run at a time per control plane. No user accounts, RBAC or SSO: one token for the control API, one for the proxy admin API.
- Proxy and control compare absolute times, so their clocks need to be in sync (NTP is enough).
- An experiment that needs a sensitive header value cannot be run from the dashboard editor; run it from its file.

## License

MIT. See [LICENSE](LICENSE).
