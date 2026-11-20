package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
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

func TestAllowCommands(t *testing.T) {
	c, err := Parse([]byte("version: 1\n"))
	if err != nil || c.AllowCommands {
		t.Fatalf("control.allow_commands must default to false: %v %+v", err, c)
	}
	c, err = Parse([]byte("version: 1\ncontrol:\n  allow_commands: true\n"))
	if err != nil || !c.AllowCommands {
		t.Fatalf("control.allow_commands: true must be accepted: %v", err)
	}
	if _, err := Parse([]byte("version: 1\ncontrol:\n  allow_commands: yes-please\n")); err == nil {
		t.Fatal("a non-boolean allow_commands must be rejected")
	}
}

// Issue #16: the admin token must not cross the network in clear text by default.
func TestProxyAdminURLNeedsTLSOffLoopback(t *testing.T) {
	cfg := func(url, extra string) string {
		return "version: 1\ncontrol:\n  proxy_admin_url: " + url + "\n" + extra
	}
	for _, url := range []string{"http://127.0.0.1:7702", "http://127.8.9.10:7702", "http://localhost:7702", "http://LOCALHOST:7702", "http://[::1]:7702", "https://proxy.qa.internal:7702", "https://10.0.0.5"} {
		if _, err := Parse([]byte(cfg(url, ""))); err != nil {
			t.Errorf("%s must be accepted: %v", url, err)
		}
	}
	for _, url := range []string{"http://proxy.qa.internal:7702", "http://10.0.0.5:7702", "http://[fd00::5]:7702", "http://localhost.evil.example:7702", "http://0.0.0.0:7702"} {
		_, err := Parse([]byte(cfg(url, "")))
		if err == nil || !strings.Contains(err.Error(), "control.proxy_admin_url") || !strings.Contains(err.Error(), "allow_insecure_admin") {
			t.Errorf("%s must be rejected, naming the key and the override: %v", url, err)
		}
		c, err := Parse([]byte(cfg(url, "  allow_insecure_admin: true\n")))
		if err != nil || !c.AllowInsecureAdmin {
			t.Errorf("%s must be accepted with allow_insecure_admin: true: %v", url, err)
		}
	}
	if c, _ := Parse([]byte("version: 1\n")); c.AllowInsecureAdmin {
		t.Fatal("control.allow_insecure_admin must default to false")
	}
}

func TestExampleConfigParses(t *testing.T) {
	data, err := os.ReadFile("../../examples/faultline.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(data); err != nil {
		t.Fatalf("examples/faultline.yaml, used by docker-compose, must stay valid: %v", err)
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

func TestEnvWithoutTokens(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"FAULTLINE_TOKEN=control-secret-0123456789",
		"FAULTLINE_ADMIN_TOKEN=admin-secret-0123456789",
		"faultline_token=lower-case-on-windows",
		"FAULTLINE_URL=http://127.0.0.1:7700",
		"FAULTLINE_RUN_ID=run_abcdefghijkl",
		"HOME=/home/synthetic",
	}
	got := strings.Join(EnvWithoutTokens(in), "\n")
	for _, secret := range []string{"control-secret", "admin-secret", "lower-case-on-windows"} {
		if strings.Contains(got, secret) {
			t.Fatalf("token %q survived: %s", secret, got)
		}
	}
	for _, keep := range []string{"PATH=/usr/bin", "FAULTLINE_URL=", "FAULTLINE_RUN_ID=", "HOME="} {
		if !strings.Contains(got, keep) {
			t.Fatalf("%q must be kept: %s", keep, got)
		}
	}
}

// Issue #17: several named control tokens, stored as SHA-256 hashes.
func TestLoadTokens(t *testing.T) {
	const alice, bob = "synthetic-alice-token-0123456789", "synthetic-bob-token-0123456789"
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	file := func(content string) string {
		p := filepath.Join(t.TempDir(), "tokens")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Setenv(EnvTokensFile, "")
	t.Setenv(EnvToken, alice)
	got, err := LoadTokens()
	if err != nil || len(got) != 1 || got[0].Name != "default" || got[0].Sum != sha256.Sum256([]byte(alice)) {
		t.Fatalf("FAULTLINE_TOKEN alone must be the token named default: %v %+v", err, got)
	}

	t.Setenv(EnvToken, "")
	t.Setenv(EnvTokensFile, file("# team tokens\nalice:"+sum(alice)+"\n\n  bob : "+strings.ToUpper(sum(bob))+"  \n"))
	got, err = LoadTokens()
	if err != nil || len(got) != 2 || got[0].Name != "alice" || got[1].Name != "bob" || got[1].Sum != sha256.Sum256([]byte(bob)) {
		t.Fatalf("tokens file: %v %+v", err, got)
	}

	t.Setenv(EnvToken, alice)
	if _, err := LoadTokens(); err == nil || !strings.Contains(err.Error(), EnvToken) || !strings.Contains(err.Error(), EnvTokensFile) {
		t.Fatalf("both set must be refused, naming both: %v", err)
	}

	t.Setenv(EnvToken, "")
	t.Setenv(EnvTokensFile, "")
	if _, err := LoadTokens(); err == nil {
		t.Fatal("neither set must be refused")
	}

	for name, content := range map[string]string{
		"empty":          "# nothing\n",
		"no colon":       "alice " + sum(alice) + "\n",
		"short hash":     "alice:abc123\n",
		"not hex":        "alice:" + strings.Repeat("zz", 32) + "\n",
		"bad name":       "Alice Smith:" + sum(alice) + "\n",
		"duplicate name": "alice:" + sum(alice) + "\nalice:" + sum(bob) + "\n",
		"duplicate hash": "alice:" + sum(alice) + "\nbob:" + sum(alice) + "\n",
		"empty token":    "alice:" + sum("") + "\n",
	} {
		t.Setenv(EnvTokensFile, file(content))
		_, err := LoadTokens()
		if err == nil {
			t.Errorf("%s: must be rejected", name)
			continue
		}
		if strings.Contains(err.Error(), sum(alice)) || strings.Contains(err.Error(), sum(bob)) {
			t.Errorf("%s: the error repeats a token hash: %v", name, err)
		}
	}
	t.Setenv(EnvTokensFile, filepath.Join(t.TempDir(), "missing"))
	if _, err := LoadTokens(); err == nil {
		t.Fatal("a missing tokens file must be refused")
	}
}

func TestTrustedProxies(t *testing.T) {
	c, err := Parse([]byte("version: 1\ncontrol:\n  trusted_proxies: ['10.0.0.0/8', '192.168.1.5', 'fd00::/8']\n"))
	if err != nil || len(c.TrustedProxies) != 3 || c.TrustedProxies[1].String() != "192.168.1.5/32" {
		t.Fatalf("trusted_proxies: %v %+v", err, c.TrustedProxies)
	}
	if _, err := Parse([]byte("version: 1\ncontrol:\n  trusted_proxies: ['proxy.internal']\n")); err == nil || !strings.Contains(err.Error(), "trusted_proxies[0]") {
		t.Fatalf("a host name must be rejected: %v", err)
	}
	if c, _ := Parse([]byte("version: 1\n")); len(c.TrustedProxies) != 0 {
		t.Fatal("trusted_proxies must default to empty")
	}
}
