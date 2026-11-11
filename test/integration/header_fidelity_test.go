//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// rawUpstream is a TCP server that keeps the exact head bytes of every
// request it receives. It speaks just enough HTTP/1.1 to answer.
type rawUpstream struct {
	ln    net.Listener
	heads chan string
}

func newRawUpstream(t testing.TB) *rawUpstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := &rawUpstream{ln: ln, heads: make(chan string, 4096)}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go u.serve(c)
		}
	}()
	return u
}

func (u *rawUpstream) addr() string { return u.ln.Addr().String() }

func (u *rawUpstream) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	for {
		var head bytes.Buffer
		length, chunked := 0, false
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			head.WriteString(line)
			if line == "\r\n" {
				break
			}
			name, value, _ := strings.Cut(strings.TrimRight(line, "\r\n"), ":")
			switch strings.ToLower(name) {
			case "content-length":
				length, _ = strconv.Atoi(strings.TrimSpace(value))
			case "transfer-encoding":
				chunked = true
			}
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(br, body); err != nil {
			return
		}
		for chunked {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			n, _ := strconv.ParseInt(strings.TrimSpace(line), 16, 64)
			chunk := make([]byte, n+2)
			if _, err := io.ReadFull(br, chunk); err != nil {
				return
			}
			body = append(body, chunk[:n]...)
			chunked = n > 0
		}
		u.heads <- head.String() + string(body)
		fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	}
}

func (u *rawUpstream) next(t testing.TB) string {
	t.Helper()
	select {
	case h := <-u.heads:
		return h
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream received no request")
		return ""
	}
}

// exchange writes raw request bytes on an open client connection and reads one response.
func exchange(t testing.TB, c net.Conn, br *bufio.Reader, raw string) string {
	t.Helper()
	if _, err := io.WriteString(c, raw); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, body %q", resp.StatusCode, body)
	}
	return string(body)
}

// PRD 8.1: request headers reach the upstream byte for byte. Names keep the
// client's spelling, lines keep their order, duplicates stay separate lines.
func TestRequestHeadersArePreservedByteForByte(t *testing.T) {
	up := newRawUpstream(t)
	s := newStack(t, map[string]string{"svc": "http://" + up.addr()})
	p := s.startProxy(t)

	c, err := net.Dial("tcp", s.trafficAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReader(c)

	t.Run("spelling, order, duplicates, hop-by-hop removed, host rewritten in place", func(t *testing.T) {
		exchange(t, c, br, "POST /svc/a/b?z=1&a=%20x HTTP/1.1\r\n"+
			"x-zeta: 1\r\n"+
			"hOsT: "+s.trafficAddr+"\r\n"+
			"X-ALPHA: two\r\n"+
			"Connection: keep-alive, x-hop\r\n"+
			"x-hop: gone\r\n"+
			"x-zeta: 3\r\n"+
			"content-LENGTH: 5\r\n"+
			"aCCept: */*\r\n"+
			"Keep-Alive: timeout=5\r\n"+
			"user-AGENT: raw/1.0\r\n"+
			"\r\nhello")
		want := "POST /a/b?z=1&a=%20x HTTP/1.1\r\n" +
			"x-zeta: 1\r\n" +
			"hOsT: " + up.addr() + "\r\n" +
			"X-ALPHA: two\r\n" +
			"x-zeta: 3\r\n" +
			"content-LENGTH: 5\r\n" +
			"aCCept: */*\r\n" +
			"user-AGENT: raw/1.0\r\n" +
			"\r\nhello"
		if got := up.next(t); got != want {
			t.Errorf("upstream received\n%q\nwant\n%q", got, want)
		}
	})

	t.Run("nothing is added to a bare request on the same connection", func(t *testing.T) {
		exchange(t, c, br, "GET /svc/bare HTTP/1.1\r\nhost: x\r\npragma: no-cache\r\n\r\n")
		want := "GET /bare HTTP/1.1\r\nhost: " + up.addr() + "\r\npragma: no-cache\r\n\r\n"
		if got := up.next(t); got != want {
			t.Errorf("upstream received\n%q\nwant\n%q", got, want)
		}
	})

	t.Run("chunked body, then the next request is still exact", func(t *testing.T) {
		body := exchange(t, c, br, "PUT /svc/chunked HTTP/1.1\r\n"+
			"x-b: 1\r\nHost: x\r\nTransfer-Encoding: chunked\r\nx-a: 2\r\n\r\n"+
			"4\r\nGET \r\n10\r\n/x HTTP/1.1\r\n\r\na\r\n0\r\n\r\n")
		if body != "GET /x HTTP/1.1\r\n\r\na" {
			t.Errorf("body came back as %q", body)
		}
		// Transfer-Encoding is hop-by-hop: the proxy frames the body itself.
		wantHead := "PUT /chunked HTTP/1.1\r\nx-b: 1\r\nHost: " + up.addr() + "\r\nx-a: 2\r\nTransfer-Encoding: chunked\r\n\r\n"
		if got := up.next(t); got != wantHead+body {
			t.Errorf("upstream received\n%q\nwant\n%q", got, wantHead+body)
		}

		exchange(t, c, br, "DELETE /svc/after HTTP/1.1\r\nX-Last: 1\r\nx-first: 2\r\nHOST: x\r\n\r\n")
		want := "DELETE /after HTTP/1.1\r\nX-Last: 1\r\nx-first: 2\r\nHOST: " + up.addr() + "\r\n\r\n"
		if got := up.next(t); got != want {
			t.Errorf("upstream received\n%q\nwant\n%q", got, want)
		}
	})

	t.Run("a faulted request is forwarded just as exactly", func(t *testing.T) {
		p.install(t, testRun, "header", flt("slow", "latency", "svc", obj{"ms": 50}))
		defer p.clear(t)
		begin := time.Now()
		exchange(t, c, br, "GET /svc/faulted HTTP/1.1\r\nx-chaos-RUN: "+testRun+"\r\nHost: x\r\nx-B: 1\r\nX-a: 2\r\n\r\n")
		if took := time.Since(begin); took < 50*time.Millisecond {
			t.Errorf("latency fault did not apply: %s", took)
		}
		want := "GET /faulted HTTP/1.1\r\nx-chaos-RUN: " + testRun + "\r\nHost: " + up.addr() + "\r\nx-B: 1\r\nX-a: 2\r\n\r\n"
		if got := up.next(t); got != want {
			t.Errorf("upstream received\n%q\nwant\n%q", got, want)
		}
	})

	t.Run("pipelined requests", func(t *testing.T) {
		io.WriteString(c, "POST /svc/p1 HTTP/1.1\r\nx-one: 1\r\nHost: x\r\nContent-Length: 3\r\n\r\nabc"+
			"GET /svc/p2 HTTP/1.1\r\nX-TWO: 2\r\nhost: x\r\n\r\n")
		for _, want := range []string{
			"POST /p1 HTTP/1.1\r\nx-one: 1\r\nHost: " + up.addr() + "\r\nContent-Length: 3\r\n\r\nabc",
			"GET /p2 HTTP/1.1\r\nX-TWO: 2\r\nhost: " + up.addr() + "\r\n\r\n",
		} {
			if got := up.next(t); got != want {
				t.Errorf("upstream received\n%q\nwant\n%q", got, want)
			}
		}
		for i := 0; i < 2; i++ {
			c.SetReadDeadline(time.Now().Add(10 * time.Second))
			resp, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatalf("response %d: %v", i+1, err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

// Under concurrency on many connections every request must still carry its own headers.
func TestRequestHeaderFidelityUnderConcurrency(t *testing.T) {
	up := newRawUpstream(t)
	s := newStack(t, map[string]string{"svc": "http://" + up.addr()})
	s.startProxy(t)

	const conns, perConn = 8, 40
	errs := make(chan error, conns)
	for w := 0; w < conns; w++ {
		go func() {
			c, err := net.Dial("tcp", s.trafficAddr)
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			br := bufio.NewReader(c)
			for i := 0; i < perConn; i++ {
				id := fmt.Sprintf("%d-%d", w, i)
				payload := "x-ID: " + id + "\r\nX-" + id + ": v\r\n"
				io.WriteString(c, "POST /svc/c HTTP/1.1\r\nhost: x\r\n"+payload+fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(payload), payload))
				c.SetReadDeadline(time.Now().Add(10 * time.Second))
				resp, err := http.ReadResponse(br, nil)
				if err != nil {
					errs <- err
					return
				}
				echoed, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if string(echoed) != payload {
					errs <- fmt.Errorf("request %s: body %q", id, echoed)
					return
				}
			}
			errs <- nil
		}()
	}
	for w := 0; w < conns; w++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	// Each request's body repeats the header lines it was sent with.
	for i := 0; i < conns*perConn; i++ {
		got := up.next(t)
		head, body, _ := strings.Cut(got, "\r\n\r\n")
		want := "POST /c HTTP/1.1\r\nhost: " + up.addr() + "\r\n" + body + fmt.Sprintf("Content-Length: %d", len(body))
		if head != want {
			t.Fatalf("upstream received head\n%q\nwant\n%q", head, want)
		}
	}
}
