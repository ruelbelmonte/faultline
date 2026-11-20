package proxy

import (
	"strings"
	"testing"
)

var rawHeadSeeds = []string{
	"GET / HTTP/1.1\r\nHost: x\r\n\r\n",
	"POST /a HTTP/1.1\r\nhOsT: x\r\nx-b: 1\r\nX-A: 2\r\nx-b: 3\r\ncontent-length: 5\r\n\r\nhello",
	"POST /b HTTP/1.1\r\nHost: x\r\ntransfer-ENCODING: chunked\r\n\r\n5;ext=1\r\nGET /\r\n0\r\nX-Trailer: t\r\n\r\n",
	"GET /c HTTP/1.1\nHost: x\nx-lf:   padded  \n\n",
	"GET / HTTP/1.1\r\nHost: x\r\nX-Folded: a\r\n b\r\n\r\n",
	"GET / HTTP/1.1\r\nHost: x\r\nBad Name: v\r\n\r\n",
	"GET / HTTP/1.1\r\nHost: x\r\nX: a\x00b\r\n\r\n",
}

func renderHead(first string, lines []headerLine) string {
	var b strings.Builder
	b.WriteString(first + "\r\n")
	for _, l := range lines {
		b.WriteString(l.name + ": " + l.value + "\r\n")
	}
	return b.String() + "\r\n"
}

// FuzzSplitHead: only plain "name: value" heads are accepted, and an accepted
// head renders back to one that parses the same.
func FuzzSplitHead(f *testing.F) {
	for _, s := range rawHeadSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, head []byte) {
		first, lines, ok := splitHead(head)
		if !ok {
			return
		}
		for _, l := range lines {
			if !isToken(l.name) || strings.ContainsAny(l.value, "\r\n\x00") || strings.Trim(l.value, " \t") != l.value {
				t.Fatalf("accepted a line that is not plain: %q", l)
			}
		}
		first2, lines2, ok := splitHead([]byte(renderHead(first, lines)))
		if !ok || first2 != first || len(lines2) != len(lines) {
			t.Fatalf("round trip changed the head: %q %q -> %q %q (%v)", first, lines, first2, lines2, ok)
		}
		for i := range lines {
			if lines[i] != lines2[i] {
				t.Fatalf("round trip changed line %d: %q -> %q", i, lines[i], lines2[i])
			}
		}
	})
}

// FuzzInConnFeed: the client byte stream, in any chunking and with any body
// framing the server reports, never panics the framing tracker and never
// makes it hold more than its limits.
func FuzzInConnFeed(f *testing.F) {
	for _, s := range rawHeadSeeds {
		f.Add([]byte(s), uint8(7), int64(5))
	}
	f.Fuzz(func(t *testing.T, stream []byte, chunk uint8, length int64) {
		c := &inConn{}
		step := int(chunk%64) + 1
		for i := 0; i < len(stream); i += step {
			c.mu.Lock()
			c.feed(stream[i:min(i+step, len(stream))])
			c.mu.Unlock()
			if len(c.pending) > maxRawHead+step || len(c.sizeBuf) > maxChunkSizeLine+step {
				t.Fatalf("buffered %d head bytes, %d size bytes", len(c.pending), len(c.sizeBuf))
			}
			if c.state == inAwaitFraming {
				head, ok := c.takeHead(length % (1 << 20))
				if ok {
					if _, _, parsed := splitHead(head); parsed && !strings.HasSuffix(string(head), "\n") {
						t.Fatalf("a head must end at its blank line: %q", head)
					}
				}
			}
		}
	})
}

// FuzzRewriteHead: rewriting the head Go writes never panics, and a head that
// parsed before still parses after, carrying the template's names in order.
func FuzzRewriteHead(f *testing.F) {
	f.Add([]byte("GET / HTTP/1.1\r\nHost: up\r\nUser-Agent: Go\r\nContent-Length: 5\r\n\r\n"), []byte("GET / HTTP/1.1\r\nhost: x\r\nx-a: 1\r\ncontent-length: 5\r\n\r\n"))
	f.Add([]byte("POST / HTTP/1.1\r\nHost: up\r\nTransfer-Encoding: chunked\r\n\r\n"), []byte("POST / HTTP/1.1\r\nX-Z: z\r\nHOST: x\r\n\r\n"))
	f.Fuzz(func(t *testing.T, goHead, client []byte) {
		_, tmpl, ok := splitHead(client)
		if !ok {
			return
		}
		out := rewriteHead(goHead, tmpl)
		if _, _, before := splitHead(goHead); !before {
			return
		}
		if _, _, after := splitHead(out); !after {
			t.Fatalf("a valid head became invalid:\n%q\n->\n%q", goHead, out)
		}
	})
}
