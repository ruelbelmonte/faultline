package cli

import (
	"strings"
	"testing"

	"faultline/internal/config"
)

// Issue #10: each child of `up` gets only the tokens it needs, and none it
// inherited from the parent's environment.
func TestUpChildEnvTokens(t *testing.T) {
	parent := []string{"PATH=/usr/bin", config.EnvToken + "=inherited-control-0123456789", config.EnvAdminToken + "=inherited-admin-0123456789"}
	tokens := map[string]string{config.EnvToken: "control-token-0123456789", config.EnvAdminToken: "admin-token-0123456789"}

	proxy := strings.Join(childEnv("proxy", parent, tokens), "\n")
	if strings.Contains(proxy, config.EnvToken+"=") {
		t.Fatalf("the proxy child must not get %s:\n%s", config.EnvToken, proxy)
	}
	if !strings.Contains(proxy, config.EnvAdminToken+"=admin-token-0123456789") {
		t.Fatalf("the proxy child needs the admin token:\n%s", proxy)
	}

	control := strings.Join(childEnv("control", parent, tokens), "\n")
	for _, want := range []string{config.EnvToken + "=control-token-0123456789", config.EnvAdminToken + "=admin-token-0123456789"} {
		if !strings.Contains(control, want) {
			t.Fatalf("the control child needs %q:\n%s", want, control)
		}
	}
	for _, env := range []string{proxy, control} {
		if strings.Contains(env, "inherited-") {
			t.Fatalf("a token inherited from the parent must not be passed on:\n%s", env)
		}
		if !strings.Contains(env, "PATH=/usr/bin") || !strings.Contains(env, envParentWatch+"=1") {
			t.Fatalf("the rest of the environment must be kept:\n%s", env)
		}
	}
}
