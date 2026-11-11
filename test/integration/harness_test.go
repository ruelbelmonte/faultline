//go:build integration

// Package integration runs real faultline, faultline-demo processes on free
// ports and talks to them over real sockets.
package integration

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var binDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "faultline-itest-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binDir = dir
	args := []string{"build", "-o", dir + string(os.PathSeparator)}
	if os.Getenv("FAULTLINE_ITEST_RACE") == "1" {
		args = append(args, "-race")
	}
	args = append(args, "./cmd/faultline", "./cmd/faultline-demo")
	build := exec.Command("go", args...)
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building binaries failed: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func bin(name string) string {
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(binDir, name)
}

var (
	handedOutMu sync.Mutex
	handedOut   = map[string]bool{}
)

// freeAddr returns a loopback address nothing is listening on. The kernel can
// offer the same port again once it is closed, so addresses already handed
// out in this test binary are never returned twice.
func freeAddr(t testing.TB) string {
	t.Helper()
	for attempt := 0; attempt < 50; attempt++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		ln.Close()
		handedOutMu.Lock()
		taken := handedOut[addr]
		handedOut[addr] = true
		handedOutMu.Unlock()
		if !taken {
			return addr
		}
	}
	t.Fatal("could not find an unused port")
	return ""
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type proc struct {
	t     testing.TB
	name  string
	cmd   *exec.Cmd
	out   *syncBuffer
	done  chan struct{}
	stdin io.WriteCloser
}

// start launches a binary and registers cleanup that kills it and fails the
// test if the process reported a data race.
func start(t testing.TB, name string, env []string, args ...string) *proc {
	t.Helper()
	return startProc(t, name, env, false, args...)
}

// startPiped is start with a stdin pipe the test controls, the way
// `faultline up` holds its children.
func startPiped(t testing.TB, name string, env []string, args ...string) *proc {
	t.Helper()
	return startProc(t, name, env, true, args...)
}

func startProc(t testing.TB, name string, env []string, piped bool, args ...string) *proc {
	t.Helper()
	p := &proc{t: t, name: name, out: &syncBuffer{}, done: make(chan struct{})}
	p.cmd = exec.Command(bin(name), args...)
	p.cmd.Env = append(os.Environ(), env...)
	if piped {
		in, err := p.cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		p.stdin = in
	}
	p.cmd.Stdout = p.out
	p.cmd.Stderr = p.out
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", name, err)
	}
	go func() {
		p.cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		p.kill()
		if strings.Contains(p.out.String(), "DATA RACE") {
			t.Errorf("%s reported a data race:\n%s", name, p.out.String())
		}
		if t.Failed() {
			t.Logf("---- %s %s output ----\n%s", name, strings.Join(args, " "), p.out.String())
		}
	})
	return p
}

// kill terminates the process the hard way (SIGKILL / TerminateProcess).
func (p *proc) kill() {
	p.cmd.Process.Kill()
	<-p.done
}

func (p *proc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// wait returns the exit code, failing the test if the process is still running after d.
func (p *proc) wait(d time.Duration) int {
	p.t.Helper()
	select {
	case <-p.done:
		return p.cmd.ProcessState.ExitCode()
	case <-time.After(d):
		p.t.Fatalf("%s did not exit within %s\n%s", p.name, d, p.out.String())
		return -1
	}
}

func waitHTTP(t testing.TB, url string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s did not become healthy", url)
}

func eventually(t testing.TB, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for: %s", d, what)
}

type demoProc struct {
	*proc
	shop, inventory, pricing, llm string
}

func startDemo(t testing.TB, mode, proxyURL string) *demoProc {
	t.Helper()
	d := &demoProc{shop: freeAddr(t), inventory: freeAddr(t), pricing: freeAddr(t), llm: freeAddr(t)}
	d.proc = start(t, "faultline-demo", nil,
		"--mode", mode, "--proxy", proxyURL,
		"--shop", d.shop, "--inventory", d.inventory, "--pricing", d.pricing, "--llm", d.llm)
	for _, a := range []string{d.shop, d.inventory, d.pricing, d.llm} {
		waitHTTP(t, "http://"+a+"/health")
	}
	return d
}

func get(t testing.TB, url string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s: reading body: %v", url, err)
	}
	return resp, body
}
