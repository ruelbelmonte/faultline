# Architecture

Faultline has three runtime pieces. The proxy and the control plane are separate OS processes on purpose: the proxy is on the data path of the system under test, and it has to keep working, and to clean up after itself, when the control plane is gone.

```
 CLI / CI      Web UI
     \           |
      v          v
   +------------------------------------+
   | Control plane  (faultline control) |  :7700
   +------------------------------------+
                  | admin API (bearer token, lease)
                  v
   +------------------------------------+
   | Proxy data plane (faultline proxy) |  :7701 traffic   :7702 admin
   +------------------------------------+
     ^                         |
     | service calls           v
  System under test     Upstream dependencies
```

| Piece | Command | Does |
|---|---|---|
| Proxy | `faultline proxy` | Reverse proxy with path-prefix routing, the fault engine, scoping, lease enforcement and request records. Holds no persistent state. |
| Control | `faultline control` | HTTP API, dashboard, run orchestration, traffic driver, probes, metrics, verdicts, findings, reports, audit log, SQLite. |
| Launcher | `faultline up` | Starts the two above as child processes, generates tokens when they are not set, stops both on exit. |

`faultline-demo` is a separate binary that plays the system under test in the docs and the tests. The `faultline` binary never imports it.

## Proxy

**Routing.** `/{route}/{rest}?{query}` goes to `{upstream}/{rest}?{query}`. Method, headers (minus hop-by-hop headers), query and body are passed through; `Host` becomes the upstream host. An unknown route is `404 {"error":"unknown_route"}` with `X-Faultline: unknown-route`. An unreachable upstream is `502 {"error":"upstream_unreachable"}` with `X-Faultline: upstream-error`.

**Streaming.** The proxy calls the HTTP transport directly and copies in a read, write, flush loop, so server-sent events and chunked responses reach the client as they arrive. Request bodies are streamed too. Transport-level compression is disabled so bytes are not altered.

**Fault engine.** The active rule set sits behind an atomic pointer. For each request the engine checks, in experiment order: run active, scope matches, route, method, path glob, time window. The first fault that matches is the fault for that request; its strategy then decides whether it fires. A match that does not fire passes through; later faults are not consulted.

**Connection-level faults.** `reset`, `timeout` and the two cut faults take the connection away from the HTTP server. `reset` closes with linger 0, which sends a TCP RST. Cuts and holds flush what was sent and close normally, so the client sees exactly the forwarded bytes followed by an unexpected end.

**Request records.** While a rule set is installed, every request on a route is recorded in a bounded in-memory ring (200,000 entries): route, method, path without query string, status, duration, whether it carried the run id, correlation id, fault id and type, the `Retry-After` the proxy sent, and for holds how long the client stayed connected. No header values, no bodies. Path, correlation id and method are length-capped.

### Fault lifetime

Three independent mechanisms bound how long a fault can live. None depends on the control plane being alive.

1. **`end_at`.** Every installed fault has an absolute end time that the proxy checks on each request.
2. **Lease.** `PUT /admin/v1/faults` carries a `lease_id`. Control renews it every `lease_timeout / 3`. If no renewal arrives for `lease_timeout`, the proxy clears every fault, records a `lease_expired` event and passes all traffic through.
3. **Clear.** `DELETE /admin/v1/faults` removes everything at once. Requests held by a `timeout` fault are released, pending delays end, throttled bodies continue at full speed.

Because 1 compares absolute times set by control against the proxy's clock, the two hosts need synchronised clocks. Ordinary NTP is enough.

### Admin API (port 7702, `Authorization: Bearer $FAULTLINE_ADMIN_TOKEN`)

| Method | Path | Purpose |
|---|---|---|
| GET | `/admin/v1/healthz` | No auth. `{"ok":true}` |
| PUT | `/admin/v1/faults` | Install `{run_id, scope_mode, lease_id, seed?, faults:[{id, type, route, match, strategy, params, start_at, end_at}]}` |
| DELETE | `/admin/v1/faults` | Clear all faults |
| POST | `/admin/v1/lease/renew` | `{lease_id}`. 404 when nothing is installed, 409 for another lease |
| GET | `/admin/v1/faults/active` | `{run_id, lease_id, faults:[{id, type, route, start_at, end_at, triggers}]}` or `{run_id:null, faults:[]}` |
| GET | `/admin/v1/records?run_id=&since=` | `{records, next}`; records with `seq > since`, at most 5000 per call |
| GET | `/admin/v1/stats` | `{run_id, active, routes, faults, events}` |

## Control plane

### Run lifecycle

```
pending -> prechecking -> injecting -> recovering -> completed
              \             \             \
               +-------------+-------------+--> aborted
```

1. **pending.** The experiment is validated and stored. Only one run that is not finished may exist; a second one gets 409.
2. **prechecking.** Probes start. They must each pass `precheck.consecutive_ok` times in a row within `precheck.timeout`. If not, the run completes as `inconclusive` (`unhealthy_before_start`) and no fault is ever installed. Skipped when there are no probes.
3. **injecting.** Control installs the faults, starts renewing the lease, starts the traffic driver and checks the abort thresholds every second. Lasts `duration`.
4. **recovering.** Faults are cleared. Traffic and probes keep running until the system has recovered or `recovery.timeout` passes.
5. **completed.** Hypothesis, verdict and findings are computed, the run is stored and the reports are written.

A run is aborted by the kill switch (`kill_switch`), a manual abort (`manual`), an abort threshold (`threshold:error_rate_max`, `threshold:probe_failures_consecutive`), a control restart or shutdown (`control_restarted`), the proxy refusing or not receiving the rule set (`proxy_error`), or the proxy reporting the lease gone (`lease_lost`). On every abort the faults are cleared on the proxy before the aborted status is stored.

If control is killed outright, nothing clears the faults for it. The lease does: after `lease_timeout` the proxy drops them. The next control process finds the unfinished run, clears the proxy once more and marks the run `aborted` with reason `control_restarted`.

### Storage

Everything lives under `data_dir`:

| Path | Content |
|---|---|
| `faultline.db` | SQLite: runs (canonical document, timeline context, redacted experiment), one metrics bucket per second per run, audit entries |
| `audit.jsonl` | The audit log again, one JSON object per line, append-only |
| `reports/{run_id}/` | `report.json`, `report.md`, `report.html`, `report.xml` |

Request records are held in memory for the duration of a run and are not stored. The experiment is redacted before it is stored; the unredacted text exists only in the memory of the running control process.

### Control API (port 7700, `Authorization: Bearer $FAULTLINE_TOKEN`)

`/healthz` and the dashboard's static files need no token. Everything under `/api/v1/` does.

| Method | Path | Response |
|---|---|---|
| GET | `/healthz` | `{"ok":true}` |
| GET | `/api/v1/routes` | `[{name, upstream}]` |
| GET | `/api/v1/experiments` | `[{name, file, valid}]` |
| GET | `/api/v1/experiments/{name}` | `{name, file, yaml}` (sensitive header values redacted) |
| PUT | `/api/v1/experiments/{name}` | Body `{yaml}`. Validates, then writes `{experiments_dir}/{name}.yaml`. 400 when invalid |
| POST | `/api/v1/experiments/validate` | Body `{yaml}`. `{valid, errors:[{path, message}]}`, always 200 |
| POST | `/api/v1/runs` | Body `{experiment_yaml}`. 201 `{run_id}`, 400 invalid, 409 a run is active |
| GET | `/api/v1/runs` | Newest first: `[{run_id, experiment, status, verdict, started_at}]` |
| GET | `/api/v1/runs/{id}` | The run document, same schema as the JSON report |
| GET | `/api/v1/runs/{id}/timeseries` | `{bucket_seconds, series, phases, fault_windows, markers}` |
| GET | `/api/v1/runs/{id}/events` | Server-sent events: `status`, `metrics`, `finding`. Ends when the run does |
| POST | `/api/v1/runs/{id}/abort` | 200 `{status:"aborted"}`; 409 when the run has finished |
| POST | `/api/v1/kill` | Clears proxy faults, aborts any active run. 200 `{cleared:true}` |
| GET | `/api/v1/faults/active` | The proxy's `faults/active`, passed through |
| GET | `/api/v1/runs/{id}/report?format=json\|md\|html\|junit` | The report as a download; 409 until the run has finished |
| GET | `/api/v1/audit?limit=` | Newest first: `[{ts, actor, action, run_id, detail}]` |
| GET | `/api/v1/meta` | `{environment, allow_unscoped, scope_header}`, used by the dashboard |

Timestamps in the control API are RFC 3339, UTC, second precision. Run ids are `run_` followed by 12 lowercase base32 characters.

## Source layout

```
cmd/faultline/        CLI entry point
cmd/faultline-demo/   demo system under test
internal/yamlx/       strict YAML walker with dotted error paths
internal/config/      faultline.yaml
internal/fault/       fault spec and wire types shared by proxy and control
internal/experiment/  experiment schema, validation, redaction
internal/proxy/       data plane
internal/control/     control plane
internal/findings/    finding rules; thresholds in thresholds.go
internal/report/      run document and the four report formats
internal/cli/         commands
internal/demo/        demo servers
internal/ui/          embedded dashboard build
ui/                   dashboard source (Vue 3, TypeScript, Vite) and Playwright test
test/integration/     tests against real processes (build tag integration)
```
