package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

const initConfig = `version: 1
environment: local            # local | ci | qa. Faultline refuses to start in prod, production or live.
data_dir: .faultline          # SQLite db, audit.jsonl, reports
experiments_dir: experiments

proxy:
  listen: 127.0.0.1:7701
  admin_listen: 127.0.0.1:7702
  lease_timeout: 10s          # no lease renewal for this long => the proxy clears ALL faults
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
  store_bodies: false

findings:
  retry_amplification_threshold: 3.0

# Point your service's upstream URLs at http://127.0.0.1:7701/<route name>.
# These routes match the bundled demo (faultline-demo).
routes:
  - name: inventory
    upstream: http://127.0.0.1:9001
  - name: pricing
    upstream: http://127.0.0.1:9002
  - name: llm
    upstream: http://127.0.0.1:9003
`

const initExperiment = `version: 1
name: example
description: Inventory returns 503 for 30% of calls; orders should still succeed
seed: 42
target:
  route: inventory
scope: header
duration: 15s

traffic:
  base_url: http://127.0.0.1:9000
  rps: 20
  concurrency: 4
  timeout: 30s
  requests:
    - method: GET
      path: /api/orders/1

steady_state:
  precheck:
    consecutive_ok: 3
    timeout: 15s
  probes:
    - name: shop-health
      url: http://127.0.0.1:9000/health
      interval: 1s
      timeout: 2s
      expect_status: 200
  hypothesis:
    error_rate_max: 0.05
    p95_latency_ms_max: 1500
    probe_success_min: 0.95

abort:
  error_rate_max: 0.5
  probe_failures_consecutive: 5

recovery:
  timeout: 30s

faults:
  - id: f1
    type: http_error
    match:
      method: GET
      path: /stock/*
    strategy:
      kind: probability
      p: 0.3
    params:
      status: 503
      retry_after: 1
`

func (e *env) cmdInit(args []string) int {
	fs := e.flags("init")
	pos, err := parse(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) > 1 {
		return e.usageErr("init takes at most one directory")
	}
	dir := "."
	if len(pos) == 1 {
		dir = pos[0]
	}
	if err := os.MkdirAll(filepath.Join(dir, "experiments"), 0o755); err != nil {
		fmt.Fprintf(e.stderr, "faultline: %v\n", err)
		return exitInternal
	}
	files := []struct{ path, content string }{
		{filepath.Join(dir, "faultline.yaml"), initConfig},
		{filepath.Join(dir, "experiments", "example.yaml"), initExperiment},
	}
	for _, f := range files {
		if _, err := os.Stat(f.path); err == nil {
			fmt.Fprintf(e.stdout, "exists, left unchanged: %s\n", f.path)
			continue
		}
		if err := os.WriteFile(f.path, []byte(f.content), 0o644); err != nil {
			fmt.Fprintf(e.stderr, "faultline: %v\n", err)
			return exitInternal
		}
		fmt.Fprintf(e.stdout, "created %s\n", f.path)
	}
	if err := ensureIgnored(filepath.Join(dir, ".gitignore"), ".faultline/"); err != nil {
		fmt.Fprintf(e.stderr, "faultline: %v\n", err)
		return exitInternal
	}
	fmt.Fprintf(e.stdout, "updated %s\n\nNext: faultline up, then faultline run experiments/example.yaml\n", filepath.Join(dir, ".gitignore"))
	return exitOK
}

func ensureIgnored(path, entry string) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		if string(bytes.TrimSpace(line)) == entry {
			return nil
		}
	}
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		data = append(data, '\n')
	}
	return os.WriteFile(path, append(data, []byte(entry+"\n")...), 0o644)
}
