// Package baggage reads and writes entries of the W3C baggage header
// (https://www.w3.org/TR/baggage/), within fixed limits.
package baggage

import (
	"net/http"
	"net/url"
	"strings"
)

const (
	// MaxBytes bounds the combined length of all baggage header values. Larger
	// baggage is ignored as a whole. 8192 is the W3C limit.
	MaxBytes = 8192
	// MaxMembers bounds how many list members are read; later ones are not.
	MaxMembers = 180
)

// Get returns the value of key in the request's baggage, percent-decoded.
// Malformed members are skipped; oversized baggage is ignored entirely; the
// first valid occurrence of key wins.
func Get(h http.Header, key string) (string, bool) {
	values := h.Values("Baggage")
	if len(values) == 0 {
		return "", false
	}
	total := 0
	for _, v := range values {
		total += len(v) + 1
		if total > MaxBytes {
			return "", false
		}
	}
	members := 0
	for _, v := range values {
		for _, member := range strings.Split(v, ",") {
			if members++; members > MaxMembers {
				return "", false
			}
			kv, _, _ := strings.Cut(member, ";") // properties are ignored
			k, val, ok := strings.Cut(kv, "=")
			if !ok || strings.TrimSpace(k) != key {
				continue
			}
			val = strings.TrimSpace(val)
			if val == "" || !validValue(val) {
				continue
			}
			decoded, err := url.PathUnescape(val)
			if err != nil {
				continue
			}
			return decoded, true
		}
	}
	return "", false
}

// Append adds key=value to the request's baggage, percent-encoding value and
// keeping any members already there.
func Append(h http.Header, key, value string) {
	member := key + "=" + url.PathEscape(value)
	if existing := strings.Join(h.Values("Baggage"), ","); existing != "" {
		member = existing + "," + member
	}
	h.Set("Baggage", member)
}

// ValidKey reports whether k is an RFC 7230 token, as baggage keys must be.
func ValidKey(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if c >= 0x80 || !tokenChar[c] {
			return false
		}
	}
	return true
}

// validValue reports whether v uses only baggage-octets (W3C: printable
// ASCII except space, double quote, comma, semicolon and backslash).
func validValue(v string) bool {
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c < 0x21 || c > 0x7e || c == '"' || c == ',' || c == ';' || c == '\\' {
			return false
		}
	}
	return true
}

var tokenChar = func() (t [128]bool) {
	for c := '0'; c <= '9'; c++ {
		t[c] = true
	}
	for c := 'a'; c <= 'z'; c++ {
		t[c] = true
	}
	for c := 'A'; c <= 'Z'; c++ {
		t[c] = true
	}
	for _, c := range "!#$%&'*+-.^_`|~" {
		t[c] = true
	}
	return t
}()
