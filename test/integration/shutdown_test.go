//go:build integration

package integration

import (
	"testing"
	"time"
)

// Children of `faultline up` are told to stop by closing their stdin. This is
// the only graceful stop that exists on Windows, so it is tested everywhere.
func TestProxyStopsWhenParentPipeCloses(t *testing.T) {
	up := newUpstream(t)
	s := newStack(t, map[string]string{"svc": up.URL})
	s.writeConfig(t)
	p := &proxyProc{s: s}
	p.proc = startPiped(t, "faultline", []string{"FAULTLINE_ADMIN_TOKEN=" + adminToken, "FAULTLINE_PARENT_WATCH=1"}, "proxy", "--config", s.cfgPath)
	waitHTTP(t, p.admin("/admin/v1/healthz"))

	p.install(t, testRun, "header", flt("hold", "timeout", "svc", obj{"hold": "30s"}))
	held := make(chan time.Duration, 1)
	go func() {
		_, _, took := raw(t, s.trafficAddr, rawGet("/svc/x", testRun))
		held <- took
	}()
	time.Sleep(300 * time.Millisecond)

	p.stdin.Close()
	if code := p.wait(8 * time.Second); code != 0 {
		t.Fatalf("expected a clean exit, got %d", code)
	}
	select {
	case <-held:
	case <-time.After(2 * time.Second):
		t.Fatal("an in-flight hold survived shutdown")
	}
}

func TestProxyIgnoresStdinWithoutParentWatch(t *testing.T) {
	up := newUpstream(t)
	s := newStack(t, map[string]string{"svc": up.URL})
	s.writeConfig(t)
	p := startPiped(t, "faultline", []string{"FAULTLINE_ADMIN_TOKEN=" + adminToken}, "proxy", "--config", s.cfgPath)
	waitHTTP(t, "http://"+s.adminAddr+"/admin/v1/healthz")
	p.stdin.Close()
	time.Sleep(time.Second)
	if p.exited() {
		t.Fatal("a standalone proxy must not exit when stdin closes")
	}
}
