package baggage

import (
	"net/http"
	"strings"
	"testing"
)

func header(values ...string) http.Header {
	h := http.Header{}
	for _, v := range values {
		h.Add("Baggage", v)
	}
	return h
}

func TestGet(t *testing.T) {
	cases := []struct {
		name   string
		h      http.Header
		key    string
		want   string
		wantOK bool
	}{
		{"simple", header("chaos-run=run_abcdefghijkl"), "chaos-run", "run_abcdefghijkl", true},
		{"among others with spaces and properties", header("userId=alice-synthetic, chaos-run = run_abcdefghijkl ;ttl=10, x=y"), "chaos-run", "run_abcdefghijkl", true},
		{"second header", header("a=b", "chaos-run=run_abcdefghijkl"), "chaos-run", "run_abcdefghijkl", true},
		{"percent-encoded value", header("chaos-corr=a%2Fb"), "chaos-corr", "a/b", true},
		{"missing", header("a=b"), "chaos-run", "", false},
		{"no header", http.Header{}, "chaos-run", "", false},
		{"key is case-sensitive", header("Chaos-Run=x"), "chaos-run", "", false},
		{"malformed member skipped, valid one kept", header("garbage, chaos-run=run_abcdefghijkl"), "chaos-run", "run_abcdefghijkl", true},
		{"bad value characters ignored", header(`chaos-run="quoted"`), "chaos-run", "", false},
		{"bad percent escape ignored", header("chaos-run=%zz"), "chaos-run", "", false},
		{"empty value ignored", header("chaos-run="), "chaos-run", "", false},
		{"first occurrence wins", header("chaos-run=one,chaos-run=two"), "chaos-run", "one", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Get(tc.h, tc.key)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("Get = %q, %v; want %q, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// Oversized baggage is ignored as a whole, so a large header cannot cost
// the proxy more than a bounded amount of work per request.
func TestGetBounded(t *testing.T) {
	big := "chaos-run=run_abcdefghijkl," + strings.Repeat("k=v,", MaxBytes/4)
	if _, ok := Get(header(big), "chaos-run"); ok {
		t.Fatal("baggage over MaxBytes must be ignored")
	}
	var members []string
	for i := 0; i <= MaxMembers; i++ {
		members = append(members, "k=v")
	}
	members = append(members, "chaos-run=run_abcdefghijkl")
	if _, ok := Get(header(strings.Join(members, ",")), "chaos-run"); ok {
		t.Fatal("members past MaxMembers must not be read")
	}
}

func TestAppend(t *testing.T) {
	h := http.Header{}
	Append(h, "chaos-run", "run_abcdefghijkl")
	Append(h, "chaos-corr", "c0ffee00-1111-2222-3333-444455556666")
	if got := h.Get("Baggage"); got != "chaos-run=run_abcdefghijkl,chaos-corr=c0ffee00-1111-2222-3333-444455556666" {
		t.Fatalf("Append: %q", got)
	}
	h = header("tenant=synthetic-tenant")
	Append(h, "chaos-run", "a/b c")
	if got := h.Get("Baggage"); got != "tenant=synthetic-tenant,chaos-run=a%2Fb%20c" {
		t.Fatalf("Append to existing baggage: %q", got)
	}
	if v, _ := Get(h, "chaos-run"); v != "a/b c" {
		t.Fatalf("round trip: %q", v)
	}
}

func TestValidKey(t *testing.T) {
	for _, k := range []string{"chaos-run", "a", "x_y.z"} {
		if !ValidKey(k) {
			t.Errorf("%q should be valid", k)
		}
	}
	for _, k := range []string{"", "a b", "a=b", "a,b", "ä"} {
		if ValidKey(k) {
			t.Errorf("%q should be invalid", k)
		}
	}
}
