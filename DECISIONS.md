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

**D12. The persisted experiment copy is redacted in place.** Header values under `traffic.requests[].headers` and `faults[].params.headers` whose name is in `redaction.headers` become `[REDACTED]`. If nothing needed redacting the stored bytes are identical to what was submitted. If the header value is a YAML alias, the anchored value is redacted too.

**D13. `yaml_sha256` is the hash of the persisted (redacted) copy.**
Alternative: hash of the submitted bytes. Reason: a hash of a document containing a low-entropy secret can be brute-forced; hashing the redacted copy cannot leak it. For experiments without sensitive headers the hash equals `sha256sum` of the file.

## CLI

**D14. `faultline validate` prints results to stdout**, one `file: valid|invalid` line per file followed by one `path: message` line per error. Problems with the command itself (missing file, unreadable config) go to stderr. Both exit 2.

**D15. `faultline init` never overwrites.** Existing files are reported and left unchanged; exit 0.

**D16. Flags may appear before or after positional arguments.** The standard `flag` package stops at the first positional argument, and the PRD shows `faultline run <file> --fail-on-breach`.
