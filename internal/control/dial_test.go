package control

import (
	"context"
	"net"
	"strings"
	"testing"

	"faultline/internal/config"
)

// Issue #11: the connect-time check refuses a metadata address even when it
// was reached through a name that validation could not judge.
func TestDialRefusesMetadataAddresses(t *testing.T) {
	c := &Control{cfg: config.Default()}
	_, err := c.dialContext(context.Background(), "tcp", "169.254.169.254:80")
	if err == nil || !strings.Contains(err.Error(), "allowed_hosts") {
		t.Fatalf("dialling a metadata address must be refused before connecting, got %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	conn, err := c.dialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("loopback must be allowed: %v", err)
	}
	conn.Close()
}

func TestHostWarning(t *testing.T) {
	cfg := config.Default()
	if HostWarning(cfg) != "" {
		t.Fatal("no warning for a local environment")
	}
	cfg.Environment = "qa"
	if !strings.Contains(HostWarning(cfg), "allowed_hosts") {
		t.Fatal("a qa environment with no allowed_hosts must warn")
	}
	cfg.AllowedHosts = []string{"*.qa.internal"}
	if HostWarning(cfg) != "" {
		t.Fatal("no warning once allowed_hosts is set")
	}
}
