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
