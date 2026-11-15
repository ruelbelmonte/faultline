package config

import (
	"net/netip"
	"strings"
	"testing"
)

// Issue #11.
func TestCheckHostMetadataRefusedByDefault(t *testing.T) {
	c := Default()
	for _, u := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://169.254.10.1:8080/",
		"http://[fe80::1]/",
		"http://metadata.google.internal/computeMetadata/v1/",
		"http://METADATA.GOOGLE.INTERNAL./",
		"http://100.100.100.200/",
		"http://[fd00:ec2::254]/",
	} {
		err := c.CheckHost(u)
		if err == nil || !strings.Contains(err.Error(), "allowed_hosts") {
			t.Errorf("%s must be refused by default, got %v", u, err)
		}
	}
	for _, u := range []string{"http://127.0.0.1:9000/", "https://shop.qa.internal/", "http://10.0.0.5/"} {
		if err := c.CheckHost(u); err != nil {
			t.Errorf("%s must be allowed with an empty allowed_hosts: %v", u, err)
		}
	}
}

func TestCheckHostAllowList(t *testing.T) {
	c := Default()
	c.AllowedHosts = []string{"*.qa.internal", "127.0.0.1:9000", "10.0.0.*"}
	allowed := []string{"http://shop.qa.internal/x", "https://SHOP.qa.internal:8443/", "http://127.0.0.1:9000/health", "http://10.0.0.7/"}
	refused := []string{"http://127.0.0.1:9001/", "http://evil.example/", "http://qa.internal.evil.example/", "http://169.254.169.254/"}
	for _, u := range allowed {
		if err := c.CheckHost(u); err != nil {
			t.Errorf("%s should be allowed: %v", u, err)
		}
	}
	for _, u := range refused {
		if err := c.CheckHost(u); err == nil {
			t.Errorf("%s should be refused", u)
		}
	}
	// A wildcard never admits a metadata address; an exact entry does.
	c.AllowedHosts = []string{"*"}
	if c.CheckHost("http://169.254.169.254/") == nil {
		t.Error("a wildcard must not admit a metadata address")
	}
	c.AllowedHosts = []string{"169.254.169.254"}
	if err := c.CheckHost("http://169.254.169.254/"); err != nil {
		t.Errorf("an exact entry admits the metadata address: %v", err)
	}
}

func TestAllowedIP(t *testing.T) {
	c := Default()
	for ip, want := range map[string]bool{"127.0.0.1": true, "10.1.2.3": true, "169.254.169.254": false, "::ffff:169.254.169.254": false, "fe80::1": false} {
		if got := c.AllowedIP(netip.MustParseAddr(ip)); got != want {
			t.Errorf("AllowedIP(%s) = %v, want %v", ip, got, want)
		}
	}
	c.AllowedHosts = []string{"169.254.169.254:80"}
	if !c.AllowedIP(netip.MustParseAddr("169.254.169.254")) {
		t.Error("an exact allowed_hosts entry admits the address at connect time")
	}
}

func TestAllowedHostsConfig(t *testing.T) {
	c, err := Parse([]byte("version: 1\ncontrol:\n  allowed_hosts: ['*.qa.internal', '127.0.0.1:9000']\n"))
	if err != nil || len(c.AllowedHosts) != 2 {
		t.Fatalf("allowed_hosts should parse: %v %+v", err, c)
	}
	for _, bad := range []string{"'[unclosed'", "''", "'http://x/y'"} {
		_, err := Parse([]byte("version: 1\ncontrol:\n  allowed_hosts: [" + bad + "]\n"))
		if err == nil || !strings.Contains(err.Error(), "control.allowed_hosts[0]") {
			t.Errorf("%s should be rejected at control.allowed_hosts[0], got %v", bad, err)
		}
	}
}
