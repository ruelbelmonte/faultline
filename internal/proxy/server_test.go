package proxy

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// Issue #21: the admin listener drops slow senders and idle connections. The
// traffic listener drops idle connections but never times out a request
// body, which must stream for as long as it takes (PRD 8.1).
func TestServerTimeouts(t *testing.T) {
	old := serverTimeouts
	if old.read != 30*time.Second || old.idle != 120*time.Second {
		t.Fatalf("default timeouts must be 30s read and 120s idle, got %+v", old)
	}
	serverTimeouts.read, serverTimeouts.idle = 300*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { serverTimeouts = old })
	drain := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.Copy(io.Discard, r.Body) })

	serve := func(srv *http.Server) string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go srv.Serve(ln)
		t.Cleanup(func() { srv.Close() })
		return ln.Addr().String()
	}
	// slowBody sends a body one byte every 50ms for up to d and reports
	// whether the server cut the connection before that.
	slowBody := func(addr string, d time.Duration) bool {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.Write([]byte("POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\n"))
		for begin := time.Now(); time.Since(begin) < d; {
			c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			if _, err := c.Read(make([]byte, 1)); err == io.EOF {
				return true
			}
			if _, err := c.Write([]byte("a")); err != nil {
				return true
			}
		}
		return false
	}
	idleClosed := func(addr string) bool {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
		br := bufio.NewReader(c)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		begin := time.Now()
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, err = br.ReadByte()
		return err == io.EOF && time.Since(begin) < 2*time.Second
	}

	admin := serve(newAdminServer(drain))
	if !slowBody(admin, 2*time.Second) {
		t.Error("the admin listener let a slow sender hold the connection")
	}
	if !idleClosed(admin) {
		t.Error("the admin listener kept an idle connection open")
	}

	traffic := serve(newTrafficServer(drain))
	if slowBody(traffic, time.Second) {
		t.Error("the traffic listener cut off a slowly streamed request body")
	}
	if !idleClosed(traffic) {
		t.Error("the traffic listener kept an idle connection open")
	}
}
