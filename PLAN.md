# Faultline build plan

Source of truth: `PRD.md`. Ambiguities are resolved in `DECISIONS.md`. Gate results are in `PROGRESS.md`.

## Package layout

```
cmd/faultline/          CLI entry point (thin; calls internal/cli)
cmd/faultline-demo/     demo system under test (imports internal/demo only)
internal/yamlx/         strict YAML walker: typed getters, unknown-key detection, dotted error paths
internal/config/        faultline.yaml: parse, defaults, validation, environment guard, tokens
internal/fault/         fault spec shared by control and proxy: types, params, strategies, glob
internal/experiment/    experiment schema, validation, redaction
internal/proxy/         data plane: routing, forwarding, fault engine, LLM faults, records, lease, admin API
internal/control/       control plane: API, store (SQLite), run orchestration, driver, probes, metrics, verdict, audit, SSE
internal/findings/      rule-based findings; thresholds in one file
internal/report/        canonical run document; Markdown, HTML, JUnit renderers
internal/cli/           commands: version, init, validate, up, proxy, control, run, status, stop, report
internal/demo/          shop, inventory, pricing, llm mock servers
internal/ui/            go:embed of the built UI (dist/)
ui/                     Vue 3 + TypeScript + Vite source, Playwright smoke test
test/integration/       real-process integration tests (build tag `integration`)
```

## Key interfaces

- `yamlx.Map`: `Str/Int/Float/Bool/Dur/Map/Seq`, then `Done()` reports unknown keys. Every error is `{path, message}`.
- `fault.Spec`: `{id, type, route, match, strategy, params}`. The same JSON shape is used in the experiment and in `PUT /admin/v1/faults` (plus absolute `start_at`, `end_at`). `fault.ParseParams(type, map)` is the single validator used by both sides.
- `proxy.Engine`: holds one immutable `*ruleSet` behind an atomic pointer. `Decide(route, method, path, scopeHeader, now)` returns the fault to apply or nil. `Clear()` swaps the pointer to nil and closes the rule set's `cleared` channel, which releases in-flight holds.
- `proxy.Records`: bounded ring with monotonic `seq`.
- `control.ProxyClient`: admin API client (install, clear, renew, active, records, stats).
- `control.Runner`: one goroutine per run driving `pending -> prechecking -> injecting -> recovering -> completed|aborted`.
- `control.Store`: SQLite (runs, buckets, audit) plus `audit.jsonl`.
- `report.Document`: canonical JSON; `report.Markdown/HTML/JUnit(doc, timeseries)`.

## Build order

- **M0** yamlx, config, fault, experiment, CLI `version`/`init`/`validate`. Unit test per validation rule.
- **M1** demo servers and resilience modes; integration harness (builds binaries, free ports, process lifecycle).
- **M2** proxy forwarding (streaming first), admin API, engine, strategies, scoping, lease, generic faults, records.
- **M3** store, auth, proxy client, runner, driver, probes, metrics, verdict, abort paths, kill switch, audit, guard, redaction, `up`.
- **M4** LLM faults (static errors, stream cut, slow first token, SSE-aware truncate and malformed tool call).
- **M5** findings, reports, `run`/`status`/`stop`/`report` CLI and exit codes.
- **M6** Vue UI, embed, Playwright smoke test.
- **M7** Dockerfiles, compose, docs, README, CI workflow, Makefile.

## Main risks and how they are handled

- **Streaming without buffering.** The proxy does not use a buffering copy: it calls `Transport.RoundTrip` directly, writes headers, then loops read, write, flush. Transport compression is disabled so bytes pass through unchanged. Proven by a time-to-first-byte test against the demo LLM stream.
- **Lease safety.** Fault lifetime is enforced in the proxy in three independent ways: per-request `end_at` check, a lease watchdog that clears everything when no renewal arrives within `lease_timeout`, and `DELETE`. None depends on control being alive. Proven by killing control mid-run.
- **Reset, hold and cut semantics.** These need the raw connection, so the proxy hijacks it. Reset closes with linger 0 (RST). Cut and hold close normally (FIN) after flushing, so the client receives exactly the forwarded bytes and then an unexpected end. Holds watch the client connection and the rule set's `cleared` channel.
- **Windows.** No Unix-only syscalls. Child processes cannot receive SIGTERM on Windows, so `faultline up` gives each child a stdin pipe and children treat its closure as a shutdown request. Hard kill uses `Process.Kill` on every platform. The race detector needs cgo, which this Windows machine lacks, so `-race` gates run in a Linux container.
- **Redaction.** Secret header values exist only in memory in the driver. The persisted experiment copy is redacted before anything is written.
- **Hidden acceptance suite.** Contracts are copied literally from the PRD. Anything the PRD leaves open is recorded in `DECISIONS.md` with the simplest reading.
