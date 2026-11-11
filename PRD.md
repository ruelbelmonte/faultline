# Faultline PRD

> Working name: **faultline**. Rename freely before the build starts; every contract below uses it, so search and replace once.

Version: 1.0 (build spec)
Owner: Ruel
Builder: Claude Fable 5.2, autonomous, from an empty repository

---

## 1. Summary

Faultline is a fault-injection proxy and control plane for application-level chaos engineering. A developer deploys it **next to** an existing API (never inside it), points one or more upstream dependency URLs at the proxy through config, and runs experiments written as YAML. Faultline injects faults into the dependency calls, drives traffic, checks a steady-state hypothesis, aborts automatically when things go too far, and produces a verdict, evidence-backed findings, a live dashboard and downloadable reports.

Primary targets, in order:

1. Local development (docker compose or plain processes).
2. CI (headless, exit codes, JUnit).
3. Shared QA environments (remote control, auth, header-scoped faults, audit log).

Production is explicitly out of scope and is actively guarded against.

## 2. Goals and non-goals

### Goals

- Zero code changes in the system under test. Integration is config only (upstream URL points at the proxy).
- Faults affect **only** traffic belonging to the active run (header scoping), so shared environments stay usable.
- Faults can never outlive their run, even if the control plane crashes.
- Every verdict and finding is backed by recorded evidence.
- First useful result within 20 minutes on an existing project.

### Non-goals (do not build)

- Kubernetes operators, CRDs, eBPF, kernel or node-level faults.
- Cloud provider faults (AWS FIS and similar).
- Load testing beyond the simple traffic driver in this spec.
- Multi-tenant RBAC, SSO, user accounts.
- Transparent HTTPS interception (MITM certificates). Reverse-proxy mode only.
- Running against production.
- MCP faults (stretch milestone M8 only, see section 13).

## 3. Users and primary journey

User: a backend developer with an existing HTTP API that calls other services, third-party APIs or LLM APIs.

1. `faultline init` in the repo.
2. Add routes to `faultline.yaml`, change the API's upstream URLs to the proxy.
3. `faultline up`, then `faultline run experiments/x.yaml`.
4. Open the dashboard, see what broke, fix, re-run.
5. Add `faultline run experiments/ --fail-on-breach` to CI.
6. Later deploy proxy and control into QA and run scoped GameDays.

## 4. Technology constraints

- **Language:** Go (latest stable). Single module. Two binaries:
  - `faultline` (CLI, proxy, control plane, embedded UI)
  - `faultline-demo` (demo system under test; never imported by `faultline`)
- **Storage:** SQLite via a pure-Go driver (`modernc.org/sqlite`) so cross-compilation works without CGO.
- **UI:** Vue 3 + TypeScript + Vite, built to static assets and embedded with `go:embed`. Charting with a small library (uPlot or Chart.js). No UI framework that needs a Node server at runtime.
- **Platforms:** must build and run on Linux, macOS and Windows (amd64 and arm64 where Go supports it).
- **Dependencies:** prefer the standard library. Every third-party dependency must be justified in `DECISIONS.md`.
- **Container:** a multi-stage `Dockerfile` producing a small image (distroless or alpine) for `faultline`, and one for `faultline-demo`.

## 5. Architecture

Three runtime components. Proxy and control **must run as separate OS processes** (separate containers in QA) so that killing the control plane does not take the data path down.

```
 CLI / CI      Web UI       (MCP later)
     \           |            /
      v          v           v
   +----------------------------------+
   | Control plane  (faultline control) |  :7700  API + UI, auth, runs, driver,
   +----------------------------------+         probes, metrics, verdicts, audit
                  | admin API (token, leases)
                  v
   +----------------------------------+
   | Proxy data plane (faultline proxy) |  :7701  traffic   :7702  admin
   +----------------------------------+
     ^                         |
     | service calls           v
  System under test     Upstream dependencies (services, partner APIs, LLM APIs)
```

- `faultline proxy`: reverse proxy with path-prefix routing, fault engine, scoping, lease enforcement, request records.
- `faultline control`: HTTP API, UI, run orchestration, traffic driver, probes, metrics aggregation, verdicts, findings, reports, audit log, SQLite.
- `faultline up`: convenience command that spawns `proxy` and `control` as **child processes**, generates tokens if absent, prints URLs and tokens, forwards signals, and stops both on exit.

## 6. Configuration file (`faultline.yaml`)

Both `proxy` and `control` read the same file (`--config`, default `./faultline.yaml`). Unknown keys are a validation error.

```yaml
version: 1
environment: local            # local | ci | qa. Values prod, production, live (case-insensitive) => refuse to start, exit 2
data_dir: .faultline          # SQLite db, audit.jsonl, reports
experiments_dir: experiments

proxy:
  listen: 127.0.0.1:7701
  admin_listen: 127.0.0.1:7702
  lease_timeout: 10s          # no lease renewal for this long => proxy clears ALL faults
  expose_fault_header: true   # add X-Faultline-Fault: <fault id> to faulted responses

control:
  listen: 127.0.0.1:7700
  proxy_admin_url: http://127.0.0.1:7702
  max_experiment_duration: 15m
  allow_unscoped: false       # experiments with scope: all are rejected unless true

scope:
  header: X-Chaos-Run         # header carrying the run id
correlation:
  header: X-Chaos-Correlation # header grouping attempts of one logical request

redaction:
  headers: [authorization, proxy-authorization, x-api-key, cookie, set-cookie]
  store_bodies: false         # v1 never stores bodies; keep the key for forward compatibility

findings:
  retry_amplification_threshold: 3.0

routes:
  - name: inventory           # [a-z0-9-]+, unique
    upstream: http://127.0.0.1:9001
  - name: anthropic
    upstream: https://api.anthropic.com
```

Secrets are **never** in the file:

- `FAULTLINE_TOKEN`: bearer token for the control API. Required by `control`.
- `FAULTLINE_ADMIN_TOKEN`: bearer token for the proxy admin API. Required by `proxy` and `control`.
- `faultline up` generates random 32-byte tokens when unset, passes them to children and prints them once.
- `proxy` and `control` refuse to start (exit 2) if their required token is missing or shorter than 16 chars.

## 7. Experiment file schema

One experiment per YAML file. Durations use Go duration syntax. Unknown keys are a validation error.

```yaml
version: 1
name: inventory-503           # [a-z0-9-]+
description: Inventory returns 503s; orders should degrade gracefully
seed: 42                      # optional; seeds probability strategies for reproducibility
target:
  route: inventory            # default route for faults; must exist in faultline.yaml
scope: header                 # header (default) | all  (all requires control.allow_unscoped)
duration: 30s                 # injection phase length; <= control.max_experiment_duration

traffic:                      # optional. Exactly one of requests | command
  base_url: http://127.0.0.1:9000
  rps: 20                     # total requests per second across workers
  concurrency: 4
  timeout: 30s                # per driver request
  requests:                   # round-robin
    - method: GET
      path: /api/orders/1
      headers: {}             # optional
      body: ""                # optional
  # command: "npm run e2e"    # alternative: shell command, re-run until phase ends.
  #                           # env FAULTLINE_RUN_ID, FAULTLINE_SCOPE_HEADER provided. No driver metrics.

steady_state:
  precheck:
    consecutive_ok: 3         # probes must pass this many times in a row before injection
    timeout: 15s              # else verdict inconclusive, reason unhealthy_before_start
  probes:
    - name: shop-health
      url: http://127.0.0.1:9000/health
      interval: 1s
      timeout: 2s
      expect_status: 200
  hypothesis:                 # evaluated over the injection phase only
    error_rate_max: 0.05      # driver: (status >= 500 or transport error) / total
    p95_latency_ms_max: 1500  # driver p95
    probe_success_min: 0.95   # fraction of probe checks that passed

abort:                        # optional; checked every second during injection
  error_rate_max: 0.5         # driver error rate over a rolling 5s window (min 10 requests)
  probe_failures_consecutive: 5

recovery:
  timeout: 30s                # after injection ends, wait up to this long for recovery

faults:
  - id: f1                    # unique within the experiment
    type: http_error
    route: inventory          # optional, defaults to target.route
    match:
      method: GET             # optional
      path: /stock/*          # optional glob on the path AFTER the route prefix is stripped
    strategy:
      kind: probability       # always | probability (p) | burst (count) | every_nth (n)
      p: 0.3
    start: 0s                 # offset from injection start
    duration: 30s             # optional; default: until injection ends
    params:
      status: 503
      retry_after: 1          # optional, seconds
```

Validation rules (all enforced by `faultline validate` and `POST /api/v1/experiments/validate`):

- `name`, `target.route`, at least one fault, valid fault types and params, `start + duration <= duration`.
- `route` references must exist in config.
- `strategy.p` in (0, 1]; `count` and `n` >= 1.
- `scope: all` rejected unless `control.allow_unscoped: true` (validation error at run creation, HTTP 400).
- Errors are returned as a list of `{path, message}` where `path` is a dotted YAML path such as `faults[0].type`.

## 8. Proxy data plane

### 8.1 Routing

- Request `/{route}/{rest}?{query}` is forwarded to `{upstream}/{rest}?{query}`. Method, headers (except hop-by-hop), body and query are preserved byte for byte.
- Unknown route: HTTP 404, body `{"error":"unknown_route"}`, header `X-Faultline: unknown-route`.
- **Streaming must not be buffered.** Response bytes are flushed to the client as they arrive from upstream (SSE and chunked responses). Request bodies are streamed too.
- The `Host` header is rewritten to the upstream host.

### 8.2 Scoping

- With `scope: header`, a fault applies only when the request's scope header equals the active run id. All other requests pass through untouched, including requests carrying a different run id.
- With `scope: all`, faults apply to every request on the route.
- Scoping across hops depends on the system under test forwarding the scope header (or OTel baggage). The README must explain this.

### 8.3 Fault matching and strategies

- A fault applies if: run active, scope matches, route matches, method matches (if set), path glob matches (if set), current time in `[start_at, end_at)`.
- First matching fault in experiment order wins; one fault per request.
- Each fault keeps a counter of **matching** requests (k = 1, 2, ...):
  - `always`: every match is faulted.
  - `probability`: faulted with probability p (seeded RNG when `seed` is set).
  - `burst`: matches k <= count are faulted.
  - `every_nth`: matches where k % n == 0 are faulted.
- `triggers` = number of requests actually faulted.
- When `expose_fault_header` is true, any response the proxy produces or modifies due to a fault carries `X-Faultline-Fault: <fault id>` (unless the connection is reset or cut).

### 8.4 Generic fault types

| type | params | behaviour |
|---|---|---|
| `latency` | `ms`, `jitter_ms` (default 0) | delay of ms +/- uniform jitter before forwarding upstream |
| `http_error` | `status`, `body` (default `{"error":"faultline injected"}`), `headers` (map), `retry_after` (s) | respond immediately without calling upstream; sets `Retry-After` if given |
| `error_in_body` | `body` (required string), `content_type` (default `application/json`) | respond 200 with that body without calling upstream |
| `timeout` | `hold` (default 30s) | hold the request open without sending anything; at `hold`, close the connection with no response. Record how long the caller held the connection (`client_held_ms`) |
| `reset` | none | close the TCP connection immediately without a response (linger 0) |
| `slow_body` | `bytes_per_sec` | forward upstream response, throttled |
| `corrupt_body` | `mode`: `truncate` (with `at_bytes`) or `garble` | truncate: forward exactly `at_bytes` body bytes then close abruptly. garble: forward with deterministic byte flips (seeded), same length |

### 8.5 LLM fault types (Anthropic Messages API format)

Apply to routes whose upstream speaks the Anthropic Messages API (`POST /v1/messages`, stream and non-stream). OpenAI format is a stretch goal.

| type | params | behaviour |
|---|---|---|
| `llm_rate_limit` | `retry_after` (default 5) | 429, header `retry-after`, body `{"type":"error","error":{"type":"rate_limit_error","message":"faultline injected"}}` |
| `llm_overloaded` | none | 529, body `{"type":"error","error":{"type":"overloaded_error","message":"faultline injected"}}` |
| `llm_stream_cut` | `after_bytes` | forward exactly `after_bytes` bytes of the upstream response body, then close the connection abruptly. No `message_stop` reaches the client |
| `llm_slow_first_token` | `delay_ms` | forward status and headers immediately, delay before the first body byte |
| `llm_truncate` | `max_chars` (default 50) | non-stream: text content truncated to max_chars, `stop_reason` set to `max_tokens`. Stream: forward text deltas until max_chars total, then emit well-formed `content_block_stop`, `message_delta` (`stop_reason: max_tokens`), `message_stop` and end the stream |
| `llm_malformed_tool_call` | none | non-stream: each `tool_use` block's `input` is replaced by the string `{"query": "de` (invalid JSON). Stream: the concatenated `input_json_delta.partial_json` values must not parse as JSON |

### 8.6 Leases and fault lifetime (safety critical)

- Control installs faults with `PUT /admin/v1/faults`, including a `lease_id`, absolute `start_at` and `end_at` per fault.
- The proxy enforces `end_at` itself. Faults stop at `end_at` regardless of control plane state.
- Control renews the lease every `lease_timeout / 3`. If the proxy receives no renewal for `lease_timeout`, it clears **all** faults, records a `lease_expired` event, and passes all traffic through.
- `DELETE /admin/v1/faults` clears everything immediately (kill switch path). Must take effect for new requests within 1 second.
- In-flight `timeout` holds are released (connection closed) when faults are cleared.

### 8.7 Request records

For every request on a route during an active run (scoped or not), the proxy keeps a record in a bounded in-memory ring (default 200k) exposed to control:

```json
{"seq": 1, "ts": "RFC3339Nano", "route": "inventory", "method": "GET", "path": "/stock/1",
 "status": 503, "duration_ms": 2.1, "scoped": true, "correlation": "uuid-or-empty",
 "fault_id": "f1", "fault_type": "http_error", "retry_after": 1, "client_held_ms": null}
```

- `status` is 0 for reset, cut or timeout with no response.
- Header values are never recorded. Bodies are never recorded.

### 8.8 Admin API (port 7702, bearer `FAULTLINE_ADMIN_TOKEN`)

| method | path | purpose |
|---|---|---|
| GET | `/admin/v1/healthz` | no auth, `{"ok":true}` |
| PUT | `/admin/v1/faults` | install rule set `{run_id, scope_mode, lease_id, faults:[...]}` |
| DELETE | `/admin/v1/faults` | clear all faults |
| POST | `/admin/v1/lease/renew` | `{lease_id}` |
| GET | `/admin/v1/faults/active` | `{run_id, lease_id, faults:[{id,type,route,start_at,end_at,triggers}]}` or `{run_id:null, faults:[]}` |
| GET | `/admin/v1/records?run_id=&since=` | records with seq > since, max 5000 per call, plus `next` |
| GET | `/admin/v1/stats` | per-route counters, per-fault triggers |

All admin endpoints except healthz return 401 without a valid token.

## 9. Control plane

### 9.1 Run lifecycle

Statuses: `pending -> prechecking -> injecting -> recovering -> completed`, or `aborted` from any non-terminal status.

1. `pending`: validated, persisted. Only **one non-terminal run** at a time; creating another returns 409.
2. `prechecking`: start probes. Traffic driver does not run yet. If `consecutive_ok` passes are not reached within `precheck.timeout`: status `completed`, verdict `inconclusive`, reason `unhealthy_before_start`, **no faults installed**. Skipped when there are no probes.
3. `injecting`: install faults on the proxy, start lease renewal, start traffic driver, evaluate abort conditions every second. Lasts `duration`.
4. `recovering`: clear faults, keep traffic and probes running, measure `recovery_seconds` = time from injection end until probes are healthy for 3 consecutive checks and the driver's rolling 5s error rate is <= `hypothesis.error_rate_max` (or 0.05 if unset). Stop at `recovery.timeout` (`recovery_seconds` null if not recovered).
5. `completed`: compute verdict, findings, persist report.

Abort paths (status `aborted`, verdict `aborted`): kill switch (`kill_switch`), manual abort (`manual`), abort threshold (`threshold:error_rate_max` or `threshold:probe_failures_consecutive`), control restart with a run in progress (`control_restarted`, detected at startup). On every abort, faults are cleared on the proxy before the status is persisted.

### 9.2 Traffic driver

- Sends `traffic.requests` round-robin to `base_url` at `rps` with `concurrency` workers.
- Every request carries the scope header (run id) and a fresh UUID in the correlation header.
- Records status (0 for transport error), latency, timestamp. Runs during `injecting` and `recovering`.

### 9.3 Metrics

1-second buckets, persisted per run:

- driver: requests, errors, p50, p95
- proxy (per route): requests, faulted
- probes: ok, fail

### 9.4 Verdict rules

Evaluated in order:

1. Aborted -> `aborted`.
2. Precheck failed -> `inconclusive`, reason `unhealthy_before_start`.
3. Any fault with `triggers == 0` -> `inconclusive`, reason `fault_not_triggered`.
4. Any hypothesis threshold breached -> `failed`.
5. Otherwise -> `passed`.

Each configured threshold yields a hypothesis entry `{name, threshold, observed, passed}` (`observed` null if there is no data, which counts as passed=false).

### 9.5 Findings (rule-based, evidence required)

Each finding: `{id, severity (info|low|medium|high), title, detail, evidence: {...}}`. Evidence must contain the numbers that triggered it.

| id | rule | severity |
|---|---|---|
| `steady_state_breach` | one per breached hypothesis threshold; evidence: name, threshold, observed | high |
| `retry_amplification` | on a faulted route during injection, mean attempts per correlation id >= `findings.retry_amplification_threshold`; evidence: route, mean_attempts, max_attempts, correlations | medium (high if mean >= 2x threshold) |
| `retry_without_backoff` | within a correlation, the next attempt on the same route arrives < 50 ms after a faulted response; evidence: route, occurrences, median_gap_ms | medium |
| `retry_after_ignored` | within a correlation, the next attempt arrives earlier than 0.9 x the Retry-After the proxy sent; evidence: route, violations, retry_after_s, median_gap_ms | medium |
| `missing_timeout` | for a `timeout` fault, median `client_held_ms` >= 0.9 x hold; evidence: route, hold_ms, median_client_held_ms, samples | high |
| `slow_recovery` | `recovery_seconds` null or > 0.5 x `recovery.timeout`; evidence: recovery_seconds, timeout_s | medium |
| `fault_not_triggered` | a fault had zero triggers; evidence: fault_id, route, match | info |

Rule thresholds are constants in one file so they are easy to tune.

### 9.6 Auth, audit, guards

- All `/api/v1/*` routes require `Authorization: Bearer $FAULTLINE_TOKEN`; 401 otherwise. `/healthz` and static UI assets are public; the UI asks for the token and keeps it in `sessionStorage`.
- Constant-time token comparison.
- Audit log: append-only JSON lines in `{data_dir}/audit.jsonl` and in SQLite. Entry: `{ts, actor:"api-token", action, run_id, detail}`. Actions at least: `run_created`, `run_aborted`, `kill_switch`, `experiment_saved`, `report_downloaded`.
- Environment guard: `prod`, `production`, `live` (any case) in `environment` -> exit code 2 with a clear message, for both `proxy` and `control`.
- Redaction: header values listed in `redaction.headers` are replaced with `[REDACTED]` anywhere they could be stored or displayed. This includes `traffic.requests[].headers` inside the experiment YAML: the run keeps the real value in memory to send it, but the persisted copy of the experiment, the run document and every report hold the redacted version. The test is simple: a secret sent in an `Authorization` header during a run must not appear anywhere under `data_dir`, in any API response, or in any report.

### 9.7 Control API (port 7700, JSON, bearer `FAULTLINE_TOKEN`)

| method | path | response |
|---|---|---|
| GET | `/healthz` | `{"ok":true}` (no auth) |
| GET | `/api/v1/routes` | `[{name, upstream}]` |
| GET | `/api/v1/experiments` | files in `experiments_dir`: `[{name, file, valid}]` |
| GET | `/api/v1/experiments/{name}` | `{name, file, yaml}` |
| PUT | `/api/v1/experiments/{name}` | body `{yaml}`; validates then writes `{experiments_dir}/{name}.yaml`; 400 on invalid |
| POST | `/api/v1/experiments/validate` | body `{yaml}` -> `{valid, errors:[{path,message}]}` (always 200) |
| POST | `/api/v1/runs` | body `{experiment_yaml}` -> 201 `{run_id}`; 400 invalid; 409 if a run is active |
| GET | `/api/v1/runs` | newest first, `[{run_id, experiment, status, verdict, started_at}]` |
| GET | `/api/v1/runs/{id}` | full run document (same schema as the JSON report, section 10) |
| GET | `/api/v1/runs/{id}/timeseries` | `{bucket_seconds:1, series:[{t, driver:{requests,errors,p95_ms}, proxy:{requests,faulted}, probes:{ok,fail}}], phases:[{name,start,end}], fault_windows:[{fault_id,start,end}], markers:[{type,t,detail}]}` |
| GET | `/api/v1/runs/{id}/events` | Server-Sent Events: status changes, metrics buckets, findings |
| POST | `/api/v1/runs/{id}/abort` | 200 `{status:"aborted"}` |
| POST | `/api/v1/kill` | clears proxy faults, aborts any active run, 200 `{cleared:true}` |
| GET | `/api/v1/faults/active` | pass-through of proxy `faults/active` |
| GET | `/api/v1/runs/{id}/report?format=json\|md\|html\|junit` | report file with correct content type |
| GET | `/api/v1/audit?limit=` | newest first |

Run ids: `run_` followed by 12 lowercase base32 characters.

## 10. Reports

Generated from one canonical JSON document. Also written to `{data_dir}/reports/{run_id}/` on completion.

### 10.1 JSON (canonical)

```json
{
  "schema_version": "1",
  "run_id": "run_abcdefghijkl",
  "experiment": {"name": "inventory-503", "description": "...", "yaml_sha256": "hex"},
  "environment": "local",
  "status": "completed",
  "verdict": "failed",
  "reason": null,
  "started_at": "RFC3339", "ended_at": "RFC3339",
  "hypothesis": [{"name": "error_rate_max", "threshold": 0.05, "observed": 0.12, "passed": false}],
  "faults": [{"id": "f1", "type": "http_error", "route": "inventory", "triggers": 312, "start_at": "...", "end_at": "..."}],
  "findings": [{"id": "retry_amplification", "severity": "medium", "title": "...", "detail": "...", "evidence": {}}],
  "metrics": {
    "driver": {"requests": 1240, "errors": 150, "error_rate": 0.12, "p50_ms": 40, "p95_ms": 900},
    "proxy": {"inventory": {"requests": 1800, "faulted": 312}}
  },
  "recovery_seconds": 4.0,
  "redaction": {"enabled": true, "headers": ["authorization"]}
}
```

`verdict` and `ended_at` are null while a run is in progress.

### 10.2 Other formats

- **Markdown:** title, verdict line, hypothesis table, findings with evidence, fault triggers, metrics summary. Suitable as a PR comment.
- **HTML:** a single self-contained file: inline CSS, inline JS, inline SVG timeline chart. No external URLs of any kind (no CDN scripts, fonts or images). Prints cleanly to PDF.
- **JUnit XML:** one `<testsuite name="{experiment}">`; one `<testcase>` per hypothesis entry (`<failure>` when not passed) and one per fault named `fault {id} triggered` (`<failure>` when triggers == 0). Aborted runs add a failing `<testcase name="run not aborted">`.

## 11. CLI

```
faultline version
faultline init [dir]                       # creates faultline.yaml, experiments/example.yaml, .gitignore entry for data_dir
faultline validate <file|dir> [--config]   # exit 0 valid, 2 invalid (errors printed as path: message)
faultline up [--config]                    # spawns proxy + control child processes
faultline proxy [--config]
faultline control [--config]
faultline run <file|dir> [--fail-on-breach] [--report-dir DIR] [--format json,md,html,junit] [--url URL]
faultline status [--url URL]
faultline stop [--url URL]                 # kill switch
faultline report <run_id> --format F [--out FILE] [--url URL]
```

- Remote commands use `--url` or `FAULTLINE_URL` (default `http://127.0.0.1:7700`) and `FAULTLINE_TOKEN`.
- `run` submits each experiment sequentially, streams progress, prints a summary, writes requested reports to `{report-dir}/{run_id}/report.{json|md|html|xml}` (JUnit uses `.xml`). Default `--format` when `--report-dir` is given: all four.
- Exit codes for `run`: 0 when all passed or when `--fail-on-breach` is absent; with `--fail-on-breach`: 10 failed, 11 inconclusive, 12 aborted (worst across files: 12 > 10 > 11 > 0). 2 for validation or usage errors, 1 for internal or connection errors.
- `init` example experiment must validate against the generated config.

## 12. Web UI

Served by control at `/`. Must work with the API alone (no server-side rendering).

1. **Token gate:** prompt for token, keep in `sessionStorage`, clear on 401.
2. **Header:** always-visible **Kill switch** button (confirm dialog, then `POST /api/v1/kill`), active run indicator, environment badge.
3. **Runs list:** run id, experiment, status, verdict, started, duration.
4. **Run detail:** verdict banner, KPI tiles (requests, faults injected, peak error rate vs threshold, recovery time), live timeline (SSE) with error rate and p95, shaded fault windows, phase boundaries, abort marker, hypothesis table, findings with evidence, fault triggers, export buttons (HTML, Markdown, JSON, JUnit), abort button while active.
5. **Experiments:** list, YAML editor with validate-on-type (debounced) and inline errors, save, run.
6. **Audit log:** table.

Use `data-testid` attributes on: `token-input`, `token-submit`, `kill-switch`, `runs-table`, `run-verdict`, `export-html`, `export-md`, `export-json`, `export-junit`.

Accessible: keyboard navigable, labels on inputs, colour never the only signal, works in light and dark mode.

## 13. Demo system under test (`faultline-demo`)

A deliberately simple system used for docs, demos and testing. One process, four HTTP servers.

```
faultline-demo --mode naive|good --proxy http://127.0.0.1:7701 \
  --shop 127.0.0.1:9000 --inventory 127.0.0.1:9001 --pricing 127.0.0.1:9002 --llm 127.0.0.1:9003 \
  [--scope-header X-Chaos-Run] [--correlation-header X-Chaos-Correlation]
```

Every server has `GET /health -> 200 {"ok":true}`.

- **shop** `GET /api/orders/{id}`: calls `{proxy}/inventory/stock/{id}`. Success: 200 `{"id","stock","price"}`. Downstream failure after retries: 502 `{"error":"upstream"}`.
- **inventory** `GET /stock/{id}`: calls `{proxy}/pricing/price/{id}`; returns `{"id","stock":42,"price":<from pricing>}`, 502 on failure after retries.
- **pricing** `GET /price/{id}`: `{"id","price":9.99}`.
- **llm** `POST /v1/messages`: Anthropic-compatible mock.
  - Non-stream: one text block containing a fixed 400-character string, `stop_reason: "end_turn"`.
  - Stream (`"stream": true`): headers and `message_start` flushed immediately, then `content_block_start`, 20 `content_block_delta` events of 20 characters each sent 50 ms apart, `content_block_stop`, `message_delta` (`stop_reason: end_turn`), `message_stop`.
  - If the request has a non-empty `tools` array: respond with one `tool_use` block, `name` = first tool's name, `input` = `{"query":"demo"}` (stream: `input_json_delta` chunks of that JSON, split into at least 3 chunks).
- All outbound calls forward the scope header, the correlation header and `traceparent` if present on the inbound request.

Resilience modes:

| mode | client timeout | retries | backoff | Retry-After |
|---|---|---|---|---|
| `naive` | none | 3 (4 attempts) on transport error or 5xx | none (immediate) | ignored |
| `good` | 1s per attempt | 1 (2 attempts) | 200 ms | honoured, capped at 2s |

## 14. Milestones and gates

Build in this order. A milestone is done only when its gate passes **from a clean checkout**. Record gate results in `PROGRESS.md`.

| # | Milestone | Gate (all must pass) |
|---|---|---|
| M0 | Repo skeleton, config + experiment parsing and validation, `version`, `init`, `validate` | unit tests for every validation rule; `init` output validates |
| M1 | `faultline-demo` | integration test: shop -> inventory -> pricing returns 200; header forwarding verified; naive vs good retry counts verified against a stub upstream |
| M2 | Proxy: routing, unbuffered streaming, admin API, generic faults, strategies, scoping, leases, records | integration tests with **real processes and real sockets** for every fault type and strategy, scoping (including other run ids), lease expiry after the admin client stops renewing, kill clears within 1s, streaming time-to-first-byte |
| M3 | Control: auth, runs lifecycle, driver, probes, metrics, verdicts, abort paths, kill switch, audit, env guard, redaction | end-to-end tests using demo + proxy + control processes: pass, fail, inconclusive (both reasons), each abort path including `kill -9` of control with faults cleared by lease |
| M4 | LLM fault types | tests against the demo llm mock for each LLM fault, stream and non-stream |
| M5 | Findings + reports (JSON, MD, HTML, JUnit) + `run` CLI exit codes | naive mode triggers retry_amplification, retry_without_backoff, retry_after_ignored, missing_timeout; good mode triggers none of them for the same experiments; HTML has no external URLs; JUnit parses |
| M6 | Web UI | Playwright (or equivalent) smoke test: token gate, runs list, run detail verdict, kill switch, export buttons |
| M7 | Packaging and docs | Dockerfiles build; `examples/docker-compose.yaml` brings up demo + proxy + control and an example experiment passes via `faultline run`; README quickstart works verbatim; GitHub Actions workflow runs lint, unit, integration and race tests |
| M8 | Stretch: MCP (Streamable HTTP) tool faults | only if M0 to M7 are green |

## 15. Quality bar

- `go vet` clean, `go test ./... -race` green, `staticcheck` clean if available.
- Integration tests use real processes and sockets. Do not mock the proxy, the network or the clock in integration tests.
- No test may be skipped, weakened or deleted to make a gate pass. If a requirement is impossible, write why in `DECISIONS.md` and leave the gate red.
- No `TODO`, stub or placeholder in a code path that a completed milestone claims to cover.
- Graceful shutdown on SIGINT/SIGTERM for all processes. Windows must work (no Unix-only syscalls without a Windows path).
- Logs are structured (`log/slog`, JSON with `--log-format json`), never contain redacted header values.

## 16. Security and data handling

- Faultline is for test environments with **synthetic data only**. README states this prominently, including: do not point it at environments holding real personal, health or financial data.
- Bodies are never persisted. Headers in the redaction list are never persisted or displayed.
- Tokens: never logged, never in the config file, constant-time compare.
- The control and admin APIs bind to 127.0.0.1 by default. The README explains how to expose them safely in QA (network policy, TLS terminating proxy).
- Audit log is append-only from the application's point of view.

## 17. Repository deliverables

```
cmd/faultline/        cmd/faultline-demo/
internal/...          ui/ (Vue source)       examples/ (compose, experiments, terraform-snippet.md)
docs/ (architecture.md, experiments.md, faults.md, qa-deployment.md)
README.md  LICENSE (MIT)  PROGRESS.md  DECISIONS.md  Makefile  Dockerfile  Dockerfile.demo  .github/workflows/ci.yaml
```

`Makefile` targets: `build`, `ui`, `test`, `test-integration`, `lint`, `demo`, `docker`.
