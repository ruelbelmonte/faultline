package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"faultline/internal/config"
)

// rawServer serves real HTTP on a rawListener and reports, per request, the
// header lines requestHead recovered ("<nil>" when it fell back).
func rawServer(t *testing.T) (addr string, seen <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan string, 16)
	srv := &http.Server{
		ConnContext: func(ctx context.Context, c net.Conn) context.Context { return context.WithValue(ctx, connKey{}, c) },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			lines := r.Context().Value(connKey{}).(*inConn).requestHead(r)
			io.Copy(io.Discard, r.Body)
			if lines == nil {
				ch <- "<nil>"
			} else {
				var b strings.Builder
				for _, l := range lines {
					b.WriteString(l.name + "=" + l.value + ";")
				}
				ch <- b.String()
			}
			w.Header().Set("Content-Length", "2")
			io.WriteString(w, "ok")
		}),
	}
	go srv.Serve(rawListener{ln})
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String(), ch
}

func next(t *testing.T, seen <-chan string) string {
	t.Helper()
	select {
	case s := <-seen:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("no request reached the handler")
		return ""
	}
}

func readResponses(t *testing.T, c net.Conn, n int) {
	t.Helper()
	br := bufio.NewReader(c)
	for i := 0; i < n; i++ {
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("response %d: %v", i+1, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func TestInConnFollowsRequestFraming(t *testing.T) {
	addr, seen := rawServer(t)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	steps := []struct {
		name, raw, want string
	}{
		{"fixed body",
			"POST /a HTTP/1.1\r\nhOsT: x\r\nx-b: 1\r\nX-A: 2\r\nx-b: 3\r\ncontent-length: 5\r\n\r\nhello",
			"hOsT=x;x-b=1;X-A=2;x-b=3;content-length=5;"},
		{"chunked body with extension and trailer",
			"POST /b HTTP/1.1\r\nHost: x\r\ntransfer-ENCODING: chunked\r\nx-after: 1\r\n\r\n" +
				"5;ext=1\r\nGET /\r\n10\r\n HTTP/1.1\r\n\r\nabc\r\n0\r\nX-Trailer: t\r\n\r\n",
			"Host=x;transfer-ENCODING=chunked;x-after=1;"},
		{"no body, bare LF line endings",
			"GET /c HTTP/1.1\nHost: x\nx-lf:   padded  \n\n",
			"Host=x;x-lf=padded;"},
		{"pragma does not gain a cache-control line",
			"GET /d HTTP/1.1\r\nHost: x\r\npragma: no-cache\r\n\r\n",
			"Host=x;pragma=no-cache;"},
	}
	for _, s := range steps {
		if _, err := io.WriteString(c, s.raw); err != nil {
			t.Fatal(err)
		}
		if got := next(t, seen); got != s.want {
			t.Errorf("%s: got %q, want %q", s.name, got, s.want)
		}
		readResponses(t, c, 1)
	}
}

func TestInConnPipelinedRequests(t *testing.T) {
	addr, seen := rawServer(t)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "POST /1 HTTP/1.1\r\nHost: x\r\nx-one: 1\r\nContent-Length: 3\r\n\r\nabc"+
		"GET /2 HTTP/1.1\r\nHost: x\r\nX-TWO: 2\r\n\r\n")
	for _, want := range []string{"Host=x;x-one=1;Content-Length=3;", "Host=x;X-TWO=2;"} {
		if got := next(t, seen); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	readResponses(t, c, 2)
}

func TestInConnLargeBodyInSmallWrites(t *testing.T) {
	addr, seen := rawServer(t)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	body := strings.Repeat("GET / HTTP/1.1\r\n\r\n", 20000)
	fmt.Fprintf(c, "PUT /big HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n", len(body))
	for len(body) > 0 {
		k := min(len(body), 1777)
		io.WriteString(c, body[:k])
		body = body[k:]
	}
	next(t, seen)
	readResponses(t, c, 1)
	io.WriteString(c, "GET /after HTTP/1.1\r\nHost: x\r\nx-after: yes\r\n\r\n")
	if got, want := next(t, seen), "Host=x;x-after=yes;"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A head that is not plain name: value lines is served from what Go parsed,
// on that request and for the rest of the connection.
func TestInConnFallsBackOnFoldedHeader(t *testing.T) {
	addr, seen := rawServer(t)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "GET /1 HTTP/1.1\r\nHost: x\r\nx-folded: a\r\n b\r\n\r\n")
	if got := next(t, seen); got != "<nil>" {
		t.Errorf("folded header: got %q, want fallback", got)
	}
	readResponses(t, c, 1)
	io.WriteString(c, "GET /2 HTTP/1.1\r\nHost: x\r\n\r\n")
	if got := next(t, seen); got != "<nil>" {
		t.Errorf("after a fallback: got %q, want fallback", got)
	}
}

func TestRewriteHead(t *testing.T) {
	cases := []struct {
		name, goHead string
		tmpl         []headerLine
		want         string
	}{
		{"order, spelling and duplicates are the template's",
			"POST /p HTTP/1.1\r\nHost: up:1\r\nContent-Length: 5\r\nAccept: */*\r\nX-A: 2\r\nX-B: 1\r\nX-B: 3\r\n\r\n",
			[]headerLine{{"x-b", "1"}, {"hOsT", "client:9"}, {"X-A", "2"}, {"x-b", "3"}, {"content-LENGTH", "5"}, {"accept", "*/*"}},
			"POST /p HTTP/1.1\r\nx-b: 1\r\nhOsT: up:1\r\nX-A: 2\r\nx-b: 3\r\ncontent-LENGTH: 5\r\naccept: */*\r\n\r\n"},
		{"chunked framing is appended, a stale length is dropped",
			"POST /p HTTP/1.1\r\nHost: up:1\r\nTransfer-Encoding: chunked\r\nX-A: 1\r\n\r\n",
			[]headerLine{{"x-a", "1"}, {"Content-Length", "0"}, {"host", "c"}},
			"POST /p HTTP/1.1\r\nx-a: 1\r\nhost: up:1\r\nTransfer-Encoding: chunked\r\n\r\n"},
		{"a zero length Go omits is kept",
			"GET /p HTTP/1.1\r\nHost: up:1\r\n\r\n",
			[]headerLine{{"host", "c"}, {"content-length", "0"}},
			"GET /p HTTP/1.1\r\nhost: up:1\r\ncontent-length: 0\r\n\r\n"},
		{"a request without a host line gets Go's",
			"GET /p HTTP/1.1\r\nHost: up:1\r\n\r\n",
			[]headerLine{{"x-a", "1"}},
			"GET /p HTTP/1.1\r\nx-a: 1\r\nHost: up:1\r\n\r\n"},
	}
	for _, c := range cases {
		if got := string(rewriteHead([]byte(c.goHead), c.tmpl)); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

type sink struct {
	net.Conn
	got []byte
}

func (s *sink) Write(p []byte) (int, error) {
	s.got = append(s.got, p...)
	return len(p), nil
}

func TestOutConnRewritesOnlyTheArmedHead(t *testing.T) {
	s := &sink{}
	c := &outConn{Conn: s}
	c.arm([]headerLine{{"x-b", "1"}, {"host", "c"}, {"X-a", "2"}})
	for _, part := range []string{"GET / HTTP/1.1\r\nHost: up\r\nX-", "A: 2\r\nX-B: 1\r\n\r", "\nBODY\r\n\r\n", "GET /2 HTTP/1.1\r\nHost: up\r\nX-B: 1\r\n\r\n"} {
		if n, err := c.Write([]byte(part)); n != len(part) || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	want := "GET / HTTP/1.1\r\nx-b: 1\r\nhost: up\r\nX-a: 2\r\n\r\nBODY\r\n\r\n" + "GET /2 HTTP/1.1\r\nHost: up\r\nX-B: 1\r\n\r\n"
	if string(s.got) != want {
		t.Errorf("got %q\nwant %q", s.got, want)
	}

	s.got = nil
	c.arm([]headerLine{{"x-stale", "1"}})
	c.Write([]byte("GET /abandoned HTTP/1.1\r\n"))
	c.arm(nil)
	c.Write([]byte("GET /3 HTTP/1.1\r\nHost: up\r\n\r\n"))
	if want := "GET /3 HTTP/1.1\r\nHost: up\r\n\r\n"; string(s.got) != want {
		t.Errorf("after disarm: got %q, want %q", s.got, want)
	}
}

// An https upstream is reached over HTTP/1.1 through the proxy's own TLS
// dial, and gets the same exact head as a plain one.
func TestTLSUpstreamReceivesExactHead(t *testing.T) {
	certSrv := httptest.NewTLSServer(http.NotFoundHandler())
	defer certSrv.Close()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: certSrv.TLS.Certificates, NextProtos: []string{"h2", "http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	heads := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		var head strings.Builder
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			head.WriteString(line)
			if line == "\r\n" {
				break
			}
		}
		heads <- head.String() + " alpn=" + c.(*tls.Conn).ConnectionState().NegotiatedProtocol
		io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
	}()

	cfg := config.Default()
	cfg.Routes = []config.Route{{Name: "svc", Upstream: "https://" + ln.Addr().String()}}
	p, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(certSrv.Certificate())
	p.tlsConfig.RootCAs = pool

	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler:     p,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context { return context.WithValue(ctx, connKey{}, c) },
	}
	go srv.Serve(rawListener{front})
	defer srv.Close()

	c, err := net.Dial("tcp", front.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "GET /svc/tls HTTP/1.1\r\nx-b: 1\r\nhOsT: x\r\nX-A: 2\r\n\r\n")
	readResponses(t, c, 1)
	want := "GET /tls HTTP/1.1\r\nx-b: 1\r\nhOsT: " + ln.Addr().String() + "\r\nX-A: 2\r\n\r\n alpn=http/1.1"
	select {
	case got := <-heads:
		if got != want {
			t.Errorf("upstream received\n%q\nwant\n%q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream received no request")
	}
}
