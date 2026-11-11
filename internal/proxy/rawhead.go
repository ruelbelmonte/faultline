package proxy

import (
	"bytes"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Go's HTTP stack canonicalises header names and writes them sorted. To
// forward request headers byte for byte, inConn keeps the raw head of each
// request as it is read from the client, and outConn rewrites the head Go
// writes to the upstream so names and order are the client's. See
// DECISIONS D80.

type headerLine struct{ name, value string }

func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// splitHead parses a raw HTTP/1.x message head. It refuses anything that is
// not plain "name: value" lines, so only well-formed heads are ever replayed.
func splitHead(head []byte) (first string, lines []headerLine, ok bool) {
	for i, row := range strings.Split(string(head), "\n") {
		row = strings.TrimSuffix(row, "\r")
		if i == 0 {
			first = row
			continue
		}
		if row == "" {
			return first, lines, true
		}
		name, value, found := strings.Cut(row, ":")
		if !found || !isToken(name) || strings.ContainsAny(value, "\r\x00") {
			return "", nil, false
		}
		lines = append(lines, headerLine{name, strings.Trim(value, " \t")})
	}
	return "", nil, false
}

// Headers the Go server moves out of, or rewrites in, Request.Header.
var serverOwnedHeaders = map[string]bool{"Host": true, "Transfer-Encoding": true, "Content-Length": true, "Trailer": true}

// matchesRequest reports whether a raw head is the one Go parsed into r. A
// raw head is only used when it says exactly what Go understood.
func matchesRequest(first string, lines []headerLine, r *http.Request) bool {
	if first != r.Method+" "+r.RequestURI+" "+r.Proto {
		return false
	}
	sent := map[string][]string{}
	for _, l := range lines {
		if k := http.CanonicalHeaderKey(l.name); !serverOwnedHeaders[k] {
			sent[k] = append(sent[k], l.value)
		}
	}
	matched := 0
	for k, vs := range r.Header {
		if serverOwnedHeaders[k] {
			continue
		}
		if _, ok := sent[k]; !ok && k == "Cache-Control" && len(vs) == 1 && vs[0] == "no-cache" {
			// Added by Go for a request that only has Pragma: no-cache.
			continue
		}
		if !slices.Equal(sent[k], vs) {
			return false
		}
		matched++
	}
	return matched == len(sent)
}

func hopSet(connection []string) map[string]bool {
	drop := map[string]bool{}
	for _, h := range hopByHop {
		drop[h] = true
	}
	for _, v := range connection {
		for _, name := range strings.Split(v, ",") {
			drop[http.CanonicalHeaderKey(strings.TrimSpace(name))] = true
		}
	}
	return drop
}

// forwardLines is the client's header lines without the hop-by-hop ones.
func forwardLines(lines []headerLine, dropAcceptEncoding bool) []headerLine {
	var connection []string
	for _, l := range lines {
		if http.CanonicalHeaderKey(l.name) == "Connection" {
			connection = append(connection, l.value)
		}
	}
	drop := hopSet(connection)
	if dropAcceptEncoding {
		drop["Accept-Encoding"] = true
	}
	out := make([]headerLine, 0, len(lines))
	for _, l := range lines {
		if !drop[http.CanonicalHeaderKey(l.name)] {
			out = append(out, l)
		}
	}
	return out
}

// rewriteHead takes the request head Go wrote and returns it with the header
// lines of tmpl, in tmpl's order and spelling. Host and Content-Length keep
// their position and spelling but carry Go's value, because Go decides the
// upstream host and the body framing. Lines only Go has (Transfer-Encoding
// for a re-chunked body) follow at the end.
func rewriteHead(goHead []byte, tmpl []headerLine) []byte {
	first, goLines, ok := splitHead(goHead)
	if !ok {
		return goHead
	}
	goValue := map[string]string{}
	for _, l := range goLines {
		k := http.CanonicalHeaderKey(l.name)
		if _, dup := goValue[k]; !dup {
			goValue[k] = l.value
		}
	}
	_, chunked := goValue["Transfer-Encoding"]

	var b bytes.Buffer
	b.Grow(len(goHead) + 64)
	b.WriteString(first + "\r\n")
	emitted := map[string]bool{}
	for _, t := range tmpl {
		k, v := http.CanonicalHeaderKey(t.name), t.value
		if k == "Host" || k == "Content-Length" {
			if emitted[k] {
				continue
			}
			if gv, has := goValue[k]; has {
				v = gv
			} else if k == "Host" || chunked || v != "0" {
				continue
			}
		}
		emitted[k] = true
		b.WriteString(t.name + ": " + v + "\r\n")
	}
	for _, l := range goLines {
		if !emitted[http.CanonicalHeaderKey(l.name)] {
			b.WriteString(l.name + ": " + l.value + "\r\n")
		}
	}
	b.WriteString("\r\n")
	return b.Bytes()
}

// outConn is a connection to an upstream. When armed, the next request head
// written to it is rewritten from a template.
type outConn struct {
	net.Conn
	mu    sync.Mutex
	armed bool
	tmpl  []headerLine
	buf   []byte
}

// arm sets the template for the request about to be written; nil disarms.
func (c *outConn) arm(tmpl []headerLine) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed, c.tmpl, c.buf = tmpl != nil, tmpl, nil
}

func (c *outConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	if !c.armed {
		c.mu.Unlock()
		return c.Conn.Write(p)
	}
	c.buf = append(c.buf, p...)
	end := bytes.Index(c.buf, []byte("\r\n\r\n"))
	if end < 0 {
		c.mu.Unlock()
		return len(p), nil
	}
	out := append(rewriteHead(c.buf[:end+4], c.tmpl), c.buf[end+4:]...)
	c.armed, c.tmpl, c.buf = false, nil, nil
	c.mu.Unlock()
	if _, err := c.Conn.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

const (
	// A head longer than the server accepts is never replayed.
	maxRawHead = 1<<20 + 8192
	// Bytes that may follow a head before the handler has said how its body is framed.
	maxRawAhead      = 1 << 20
	maxChunkSizeLine = 4096
)

const (
	inHead = iota
	inAwaitFraming
	inFixedBody
	inChunkSize
	inChunkData
	inChunkEnd
	inTrailer
)

// inConn is a client connection that remembers the raw head of the request
// being served. It follows the HTTP/1.1 framing of the byte stream to know
// where each head starts; the body framing itself comes from the Go server
// (takeHead), so this code never interprets framing headers on its own.
type inConn struct {
	net.Conn

	mu      sync.Mutex
	broken  bool
	state   int
	pending []byte // the head being read, then the bytes after it while awaiting framing
	headEnd int
	scanned int
	line    int   // start of the current line in pending
	remain  int64 // body or chunk bytes still to pass
	sizeBuf []byte
	trailer int // length of the current trailer line
}

func (c *inConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.mu.Lock()
		c.feed(p[:n])
		c.mu.Unlock()
	}
	return n, err
}

// SetLinger and CloseWrite keep the TCP behaviour callers get from an unwrapped connection.
func (c *inConn) SetLinger(sec int) error {
	if tcp, ok := c.Conn.(*net.TCPConn); ok {
		return tcp.SetLinger(sec)
	}
	return nil
}

func (c *inConn) CloseWrite() error {
	if tcp, ok := c.Conn.(*net.TCPConn); ok {
		return tcp.CloseWrite()
	}
	return nil
}

func (c *inConn) giveUp() {
	c.broken, c.pending, c.sizeBuf = true, nil, nil
}

func (c *inConn) feed(b []byte) {
	for len(b) > 0 && !c.broken {
		switch c.state {
		case inHead:
			c.pending = append(c.pending, b...)
			b = nil
			c.scanHead()
		case inAwaitFraming:
			c.pending = append(c.pending, b...)
			b = nil
			if len(c.pending)-c.headEnd > maxRawAhead {
				c.giveUp()
			}
		case inFixedBody, inChunkData:
			k := min(c.remain, int64(len(b)))
			c.remain -= k
			b = b[k:]
			if c.remain == 0 {
				if c.state == inFixedBody {
					c.state = inHead
				} else {
					c.state, c.remain = inChunkEnd, 2
				}
			}
		case inChunkEnd:
			k := min(c.remain, int64(len(b)))
			c.remain -= k
			b = b[k:]
			if c.remain == 0 {
				c.state = inChunkSize
			}
		case inChunkSize:
			i := bytes.IndexByte(b, '\n')
			if i < 0 {
				c.sizeBuf = append(c.sizeBuf, b...)
				b = nil
			} else {
				c.sizeBuf = append(c.sizeBuf, b[:i]...)
				b = b[i+1:]
				c.chunkSize()
			}
			if len(c.sizeBuf) > maxChunkSizeLine {
				c.giveUp()
			}
		case inTrailer:
			ch := b[0]
			b = b[1:]
			switch {
			case ch == '\n' && c.trailer == 0:
				c.state = inHead
			case ch == '\n':
				c.trailer = 0
			case ch != '\r':
				c.trailer++
			}
		}
	}
}

// scanHead looks for the empty line that ends the head in pending.
func (c *inConn) scanHead() {
	for {
		i := bytes.IndexByte(c.pending[c.scanned:], '\n')
		if i < 0 {
			c.scanned = len(c.pending)
			if len(c.pending) > maxRawHead {
				c.giveUp()
			}
			return
		}
		end := c.scanned + i
		row := c.pending[c.line:end]
		c.scanned, c.line = end+1, end+1
		if len(row) == 0 || (len(row) == 1 && row[0] == '\r') {
			c.state, c.headEnd = inAwaitFraming, end+1
			return
		}
	}
}

func (c *inConn) chunkSize() {
	field, _, _ := strings.Cut(string(c.sizeBuf), ";")
	c.sizeBuf = c.sizeBuf[:0]
	n, err := strconv.ParseUint(strings.TrimSpace(field), 16, 62)
	switch {
	case err != nil:
		c.giveUp()
	case n == 0:
		c.state, c.trailer = inTrailer, 0
	default:
		c.state, c.remain = inChunkData, int64(n)
	}
}

// takeHead returns the raw head of the request the server just parsed and
// starts following its body: length > 0 is a fixed length, 0 no body, -1 chunked.
func (c *inConn) takeHead(length int64) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broken || c.state != inAwaitFraming {
		c.giveUp()
		return nil, false
	}
	head, ahead := c.pending[:c.headEnd:c.headEnd], c.pending[c.headEnd:]
	c.pending, c.headEnd, c.scanned, c.line = nil, 0, 0, 0
	switch {
	case length > 0:
		c.state, c.remain = inFixedBody, length
	case length < 0:
		c.state = inChunkSize
	default:
		c.state = inHead
	}
	c.feed(ahead)
	return head, true
}

// requestHead returns the header lines of r exactly as the client sent them,
// or nil when they cannot be established; the caller then forwards what Go
// parsed. It must be called once for every request served on the connection.
func (c *inConn) requestHead(r *http.Request) []headerLine {
	length := max(r.ContentLength, 0)
	if len(r.TransferEncoding) > 0 {
		length = -1
	}
	head, ok := c.takeHead(length)
	if !ok {
		return nil
	}
	first, lines, ok := splitHead(head)
	if !ok || !matchesRequest(first, lines, r) {
		c.mu.Lock()
		c.giveUp()
		c.mu.Unlock()
		return nil
	}
	return lines
}

// rawListener hands out connections that remember raw request heads.
type rawListener struct{ net.Listener }

func (l rawListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &inConn{Conn: c}, nil
}
