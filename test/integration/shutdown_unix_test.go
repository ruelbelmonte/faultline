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

func TestControlGracefulShutdownOnSignalClearsFaults(t *testing.T) {
	for _, sig := range []os.Signal{syscall.SIGTERM, os.Interrupt} {
		f := startFull(t, "naive")
		id := f.control.createRun(t, longRun(f, t, expOpts{Name: "signal"}))
		f.control.waitStatus(t, id, 15*time.Second, "injecting")
		eventually(t, 5*time.Second, "fault active", func() bool { return f.faultedThroughProxy(id) })
		f.control.cmd.Process.Signal(sig)
		if code := f.control.wait(25 * time.Second); code != 0 {
			t.Fatalf("%v: expected a clean exit, got %d", sig, code)
		}
		if f.faultedThroughProxy(id) {
			t.Fatalf("%v: control exited and left its faults installed", sig)
		}
	}
}

func TestUpForwardsSignalsAndStopsChildren(t *testing.T) {
	up := newUpstream(t)
	s := newStack(t, map[string]string{"svc": up.URL})
	s.writeConfig(t)
	p := start(t, "faultline", nil, "up", "--config", s.cfgPath)
	waitHTTP(t, "http://"+s.controlAddr+"/healthz")
	waitHTTP(t, "http://"+s.adminAddr+"/admin/v1/healthz")
	p.cmd.Process.Signal(syscall.SIGTERM)
	if code := p.wait(20 * time.Second); code != 0 {
		t.Fatalf("expected a clean exit, got %d", code)
	}
	for _, addr := range []string{s.controlAddr, s.adminAddr} {
		if res := call("GET", "http://"+addr+"/healthz", nil, ""); res.err == nil {
			t.Fatalf("%s is still listening after `up` exited", addr)
		}
	}
}
