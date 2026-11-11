package config

import (
	"errors"
	"strings"
	"testing"
	"time"

	"faultline/internal/yamlx"
)

const full = `version: 1
environment: local
data_dir: .faultline
experiments_dir: experiments
proxy:
  listen: 127.0.0.1:7701
  admin_listen: 127.0.0.1:7702
  lease_timeout: 10s
  expose_fault_header: true
control:
  listen: 127.0.0.1:7700
  proxy_admin_url: http://127.0.0.1:7702
  max_experiment_duration: 15m
  allow_unscoped: false
scope:
  header: X-Chaos-Run
correlation:
  header: X-Chaos-Correlation
redaction:
  headers: [authorization, proxy-authorization, x-api-key, cookie, set-cookie]
  store_bodies: false
findings:
  retry_amplification_threshold: 3.0
routes:
  - name: inventory
    upstream: http://127.0.0.1:9001
  - name: anthropic
    upstream: https://api.anthropic.com
`

func TestParseFull(t *testing.T) {
	c, err := Parse([]byte(full))
	if err != nil {
		t.Fatal(err)
	}
	if c.LeaseTimeout != 10*time.Second || c.MaxExperimentDuration != 15*time.Minute || len(c.Routes) != 2 {
		t.Fatalf("unexpected config: %+v", c)
	}
	if r, ok := c.Route("anthropic"); !ok || r.Upstream != "https://api.anthropic.com" {
		t.Fatalf("route lookup failed: %+v", r)
	}
}

func TestDefaultsApply(t *testing.T) {
	c, err := Parse([]byte("version: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	d := Default()
	if c.ProxyListen != d.ProxyListen || c.ScopeHeader != "X-Chaos-Run" || c.Environment != "local" || len(c.RedactionHeaders) != 5 {
		t.Fatalf("defaults not applied: %+v", c)
	}
}

func TestProductionGuard(t *testing.T) {
	for _, env := range []string{"prod", "production", "live", "PROD", "Production", "LiVe"} {
		_, err := Parse([]byte("version: 1\nenvironment: " + env + "\n"))
		var pe *ProductionError
		if !errors.As(err, &pe) {
			t.Errorf("environment %q: expected ProductionError, got %v", env, err)
		}
	}
}

func TestValidationRules(t *testing.T) {
	cases := []struct{ name, old, new, path string }{
		{"missing version", "version: 1\n", "", "version"},
		{"wrong version", "version: 1\n", "version: 3\n", "version"},
		{"unknown environment", "environment: local", "environment: staging", "environment"},
		{"unknown top-level key", "data_dir: .faultline", "data_dir: .faultline\nextra: 1", "extra"},
		{"unknown proxy key", "  lease_timeout: 10s", "  lease_timeout: 10s\n  tls: true", "proxy.tls"},
		{"unknown control key", "  allow_unscoped: false", "  allow_unscoped: false\n  workers: 2", "control.workers"},
		{"bad listen", "listen: 127.0.0.1:7701", "listen: nowhere", "proxy.listen"},
		{"bad lease timeout", "lease_timeout: 10s", "lease_timeout: ten", "proxy.lease_timeout"},
		{"lease timeout too short", "lease_timeout: 10s", "lease_timeout: 10ms", "proxy.lease_timeout"},
		{"bad bool", "expose_fault_header: true", "expose_fault_header: maybe", "proxy.expose_fault_header"},
		{"bad proxy admin url", "proxy_admin_url: http://127.0.0.1:7702", "proxy_admin_url: 127.0.0.1:7702", "control.proxy_admin_url"},
		{"store_bodies true", "store_bodies: false", "store_bodies: true", "redaction.store_bodies"},
		{"bad threshold", "retry_amplification_threshold: 3.0", "retry_amplification_threshold: 0.5", "findings.retry_amplification_threshold"},
		{"bad route name", "name: inventory", "name: Inventory!", "routes[0].name"},
		{"duplicate route name", "name: anthropic", "name: inventory", "routes[1].name"},
		{"missing upstream", "    upstream: http://127.0.0.1:9001\n", "", "routes[0].upstream"},
		{"bad upstream", "upstream: https://api.anthropic.com", "upstream: api.anthropic.com", "routes[1].upstream"},
		{"bad scope header", "header: X-Chaos-Run", "header: 'X Chaos Run'", "scope.header"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(full, tc.old) {
				t.Fatalf("test bug: %q not in document", tc.old)
			}
			_, err := Parse([]byte(strings.Replace(full, tc.old, tc.new, 1)))
			var errs yamlx.Errors
			if !errors.As(err, &errs) {
				t.Fatalf("expected validation errors, got %v", err)
			}
			for _, e := range errs {
				if e.Path == tc.path {
					return
				}
			}
			t.Fatalf("expected an error at %q, got %v", tc.path, errs)
		})
	}
}

func TestToken(t *testing.T) {
	t.Setenv("FAULTLINE_TEST_TOKEN", "")
	if _, err := Token("FAULTLINE_TEST_TOKEN"); err == nil {
		t.Fatal("missing token must be rejected")
	}
	t.Setenv("FAULTLINE_TEST_TOKEN", "short")
	if _, err := Token("FAULTLINE_TEST_TOKEN"); err == nil {
		t.Fatal("short token must be rejected")
	}
	t.Setenv("FAULTLINE_TEST_TOKEN", "0123456789abcdef")
	if v, err := Token("FAULTLINE_TEST_TOKEN"); err != nil || v != "0123456789abcdef" {
		t.Fatalf("16 character token must be accepted: %v", err)
	}
}
