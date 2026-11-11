# Experiments

One experiment per YAML file. Durations use Go syntax (`500ms`, `30s`, `2m`). Unknown keys are a validation error, and so are YAML aliases and merge keys.

```yaml
version: 1
name: inventory-503           # [a-z0-9-]+
description: Inventory returns 503s; orders should degrade gracefully
seed: 42                      # optional; makes probability strategies reproducible
target:
  route: inventory            # default route for faults; must exist in faultline.yaml
scope: header                 # header (default) | all (needs control.allow_unscoped)
duration: 30s                 # injection phase; at most control.max_experiment_duration

traffic:                      # optional. Exactly one of requests | command
  base_url: http://127.0.0.1:9000
  rps: 20                     # total requests per second
  concurrency: 4              # default 1
  timeout: 30s                # per request, default 30s
  requests:                   # sent round-robin
    - method: GET             # default GET
      path: /api/orders/1
      headers: {}             # optional
      body: ""                # optional
  # command: "npm run e2e"    # instead of requests: a shell command, re-run until the run ends

steady_state:
  precheck:
    consecutive_ok: 3         # default 3
    timeout: 15s              # default 15s
  probes:
    - name: shop-health
      url: http://127.0.0.1:9000/health
      interval: 1s            # default 1s
      timeout: 2s             # default 2s
      expect_status: 200      # default 200
  hypothesis:                 # evaluated over the injection phase only
    error_rate_max: 0.05
    p95_latency_ms_max: 1500
    probe_success_min: 0.95

abort:                        # optional; checked every second during injection
  error_rate_max: 0.5
  probe_failures_consecutive: 5

recovery:
  timeout: 30s                # default 30s

faults:                       # at least one; see faults.md
  - id: f1
    type: http_error
    match: {method: GET, path: "/stock/*"}
    strategy: {kind: probability, p: 0.3}
    params: {status: 503, retry_after: 1}
```

## Validation

`faultline validate <file|dir>` and `POST /api/v1/experiments/validate` apply the same rules. Errors are `{path, message}` with a dotted path such as `faults[0].type`; the CLI prints them as `path: message` and exits 2.

- `version` is 1; `name` matches `[a-z0-9-]+`.
- `target.route` and every fault's `route` exist in `faultline.yaml`.
- `duration` is positive and at most `control.max_experiment_duration`.
- At least one fault; fault ids are unique; the type is known; params are valid for the type, with no unknown params.
- `strategy.p` is in (0, 1]; `count` and `n` are at least 1.
- A fault's `start + duration` does not exceed the experiment's `duration`.
- `traffic` has exactly one of `requests` and `command`; with `requests`, `base_url` and `rps` are required.
- `scope: all` is rejected unless `control.allow_unscoped: true`.

## Traffic

With `requests`, the driver sends them round-robin to `base_url` at `rps` using `concurrency` workers. Every request carries the scope header with the run id and a fresh UUID in the correlation header. The driver records status, latency and time. A status of 500 or above and a transport error both count as an error; 4xx does not. When every worker is busy the driver drops the backlog rather than queueing it, and it does not follow redirects. It runs during injection and recovery.

With `command`, Faultline runs the shell command (`sh -c`, or `cmd.exe /S /C` on Windows) again and again until the run ends, with `FAULTLINE_RUN_ID` and `FAULTLINE_SCOPE_HEADER` in its environment. Your test must send that header itself. A command produces no driver metrics, so `error_rate_max` and `p95_latency_ms_max` have no data and count as breached; use probes for the hypothesis instead.

With no `traffic` at all, something else has to send requests carrying the run id (read it from the run document) or the faults will never trigger.

## Probes

Each probe is a GET every `interval`; it passes when the status equals `expect_status` within `timeout`. Probes carry the run id too, so a probe that goes through a faulted dependency sees the fault. That is what makes `probe_success_min` and `probe_failures_consecutive` useful; a plain health endpoint is unaffected either way.

## Hypothesis and verdict

Each configured threshold becomes an entry `{name, threshold, observed, passed}`, measured over the injection phase:

| Threshold | Observed | Passes when |
|---|---|---|
| `error_rate_max` | driver errors / driver requests | observed <= threshold |
| `p95_latency_ms_max` | driver p95 latency in ms | observed <= threshold |
| `probe_success_min` | probe checks passed / probe checks made | observed >= threshold |

With no data, `observed` is `null` and the entry does not pass.

The verdict is the first rule that applies:

1. The run was aborted: `aborted`, with the reason.
2. The precheck failed: `inconclusive`, reason `unhealthy_before_start`.
3. A fault has zero triggers: `inconclusive`, reason `fault_not_triggered`.
4. A hypothesis entry did not pass: `failed`.
5. Otherwise: `passed`.

## Recovery

After injection the faults are cleared and Faultline measures `recovery_seconds`: the time until every probe has passed 3 checks in a row and the error rate of driver requests sent since the faults were cleared, over the last 5 seconds, is within `hypothesis.error_rate_max` (5% if unset). If that does not happen within `recovery.timeout`, `recovery_seconds` is `null`.

## Findings

Findings are rule-based. Each one carries the numbers that triggered it in `evidence`.

| id | Reported when | Severity | Evidence |
|---|---|---|---|
| `steady_state_breach` | a hypothesis entry did not pass (one finding each) | high | `name`, `threshold`, `observed` |
| `retry_amplification` | on a faulted route, mean attempts per correlation id >= `findings.retry_amplification_threshold` | medium; high at twice the threshold | `route`, `mean_attempts`, `max_attempts`, `correlations` |
| `retry_without_backoff` | within a correlation, the next attempt on the same route arrives less than 50 ms after a faulted response | medium | `route`, `occurrences`, `median_gap_ms` |
| `retry_after_ignored` | within a correlation, the next attempt arrives earlier than 0.9 x the `Retry-After` the proxy sent | medium | `route`, `violations`, `retry_after_s`, `median_gap_ms` |
| `missing_timeout` | for a `timeout` fault, the median time callers stayed connected is >= 0.9 x `hold` | high | `route`, `hold_ms`, `median_client_held_ms`, `samples` |
| `slow_recovery` | `recovery_seconds` is `null` or more than half of `recovery.timeout` | medium | `recovery_seconds`, `timeout_s` |
| `fault_not_triggered` | a fault had zero triggers | info | `fault_id`, `route`, `match` |

Notes:

- The retry rules need the correlation header to reach the proxy. If a service between the driver and the proxy does not forward it, attempts cannot be grouped and these rules stay silent.
- `missing_timeout` leaves out holds that ended because the faults were cleared, since those say nothing about the caller. Choose a `hold` well below `duration` so that some holds run their full length.
- `slow_recovery` and `fault_not_triggered` are not reported for aborted runs.
- The constants (50 ms, 0.9, 0.5) are in `internal/findings/thresholds.go`.

## Sensitive headers

Values of headers named in `redaction.headers`, under `traffic.requests[].headers` and under `faults[].params.headers`, are sent as written but stored and shown as `[REDACTED]`. This is why an experiment with such a header has to be run from its file (`faultline run`): the copy the dashboard can load no longer has the value.

The request `body` is stored as part of the experiment. Keep it synthetic.

## Reports

`faultline run --report-dir DIR` writes `DIR/<run_id>/report.{json,md,html,xml}`; control also writes them to `{data_dir}/reports/<run_id>/`. All four come from the same JSON document, which is also what `GET /api/v1/runs/{id}` returns.

- **JSON** is the canonical document.
- **Markdown** is short enough for a pull request comment.
- **HTML** is one self-contained file with an inline timeline chart. It references nothing outside itself and prints to PDF.
- **JUnit** has one `<testsuite>` named after the experiment, a `<testcase>` per hypothesis entry, one per fault named `fault {id} triggered`, and a failing `run not aborted` case for aborted runs.
