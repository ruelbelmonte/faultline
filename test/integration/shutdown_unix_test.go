//go:build integration && !windows

package integration

import (
	"os"
	"syscall"
	"testing"
	"time"
)

func TestProxyGracefulShutdownOnSignal(t *testing.T) {
	for _, sig := range []os.Signal{syscall.SIGTERM, os.Interrupt} {
		p, _ := startEchoProxy(t)
		p.cmd.Process.Signal(sig)
		if code := p.wait(8 * time.Second); code != 0 {
			t.Fatalf("%v: expected a clean exit, got %d", sig, code)
		}
	}
}
