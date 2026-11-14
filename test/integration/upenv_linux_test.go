//go:build integration && linux

package integration

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// childrenOf returns the pids whose parent is pid, from /proc/*/stat.
func childrenOf(pid int) []int {
	var out []int
	stats, _ := filepath.Glob("/proc/[0-9]*/stat")
	for _, path := range stats {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		// pid (comm) state ppid ...; comm may contain spaces, so cut at the last ')'.
		i := bytes.LastIndexByte(data, ')')
		if i < 0 {
			continue
		}
		fields := strings.Fields(string(data[i+1:]))
		if len(fields) < 2 {
			continue
		}
		if ppid, _ := strconv.Atoi(fields[1]); ppid == pid {
			child, _ := strconv.Atoi(filepath.Base(filepath.Dir(path)))
			out = append(out, child)
		}
	}
	return out
}

// Issue #10: `up` gives the proxy child the admin token only and the control
// child both, read from the children's real environments.
func TestUpChildrenGetOnlyTheTokensTheyNeed(t *testing.T) {
	up := newUpstream(t)
	s := newStack(t, map[string]string{"svc": up.URL})
	s.writeConfig(t)
	p := start(t, "faultline", []string{"FAULTLINE_TOKEN=" + controlToken, "FAULTLINE_ADMIN_TOKEN=" + adminToken}, "up", "--config", s.cfgPath)
	eventually(t, 20*time.Second, "`up` to report ready", func() bool { return strings.Contains(p.out.String(), "Faultline is up") })

	envs := map[string]string{}
	for _, child := range childrenOf(p.cmd.Process.Pid) {
		cmdline, err1 := os.ReadFile("/proc/" + strconv.Itoa(child) + "/cmdline")
		environ, err2 := os.ReadFile("/proc/" + strconv.Itoa(child) + "/environ")
		if err1 != nil || err2 != nil {
			continue
		}
		args := strings.Split(string(cmdline), "\x00")
		if len(args) > 1 {
			envs[args[1]] = "\n" + strings.ReplaceAll(string(environ), "\x00", "\n")
		}
	}
	proxy, control := envs["proxy"], envs["control"]
	if proxy == "" || control == "" {
		t.Fatalf("could not find both children of `up` in /proc: %v", envs)
	}
	if strings.Contains(proxy, "\nFAULTLINE_TOKEN=") || strings.Contains(proxy, controlToken) {
		t.Fatal("the proxy child must not get FAULTLINE_TOKEN")
	}
	if !strings.Contains(proxy, "\nFAULTLINE_ADMIN_TOKEN="+adminToken+"\n") {
		t.Fatal("the proxy child needs FAULTLINE_ADMIN_TOKEN")
	}
	for _, want := range []string{"\nFAULTLINE_TOKEN=" + controlToken + "\n", "\nFAULTLINE_ADMIN_TOKEN=" + adminToken + "\n"} {
		if !strings.Contains(control, want) {
			t.Fatalf("the control child is missing %q", strings.TrimSpace(strings.SplitN(want, "=", 2)[0]))
		}
	}
}
