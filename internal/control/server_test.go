package control

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// Issue #21: a client that sends its body slowly or idles on a keep-alive
// connection is disconnected instead of holding the connection forever.
func TestServerDropsSlowAndIdleClients(t *testing.T) {
	old := serverTimeouts
	if old.read != 30*time.Second || old.idle != 120*time.Second {
		t.Fatalf("default timeouts must be 30s read and 120s idle, got %+v", old)
	}
	serverTimeouts.read, serverTimeouts.idle = 300*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { serverTimeouts = old })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
	}))
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	t.Run("slow body", func(t *testing.T) {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.Write([]byte("POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\n"))
		begin := time.Now()
		for time.Since(begin) < 3*time.Second {
			if _, err := c.Write([]byte("a")); err != nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		io.Copy(io.Discard, c)
		if took := time.Since(begin); took > 2*time.Second {
			t.Fatalf("a slow sender held the connection for %s", took)
		}
	})

	t.Run("idle keep-alive", func(t *testing.T) {
		c, err := net.Dial("tcp", ln.Addr().String())
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
		if _, err := br.ReadByte(); err == nil {
			t.Fatal("unexpected data on an idle connection")
		}
		if took := time.Since(begin); took > 2*time.Second {
			t.Fatalf("an idle connection stayed open for %s", took)
		}
	})
}
