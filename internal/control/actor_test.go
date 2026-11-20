package control

import (
	"crypto/sha256"
	"database/sql"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"testing"

	"faultline/internal/config"
)

func testTokens(tok string) []config.NamedToken {
	return []config.NamedToken{{Name: "default", Sum: sha256.Sum256([]byte(tok))}}
}

func TestAuthenticateNamesTheToken(t *testing.T) {
	c := &Control{tokens: []config.NamedToken{
		{Name: "alice", Sum: sha256.Sum256([]byte("synthetic-alice-token-0123456789"))},
		{Name: "bob", Sum: sha256.Sum256([]byte("synthetic-bob-token-0123456789"))},
	}}
	for header, want := range map[string]string{
		"Bearer synthetic-alice-token-0123456789": "alice",
		"Bearer synthetic-bob-token-0123456789":   "bob",
		"Bearer synthetic-bob-token-012345678":    "",
		"synthetic-bob-token-0123456789":          "",
		"Bearer ":                                 "",
		"":                                        "",
	} {
		r := httptest.NewRequest("GET", "/api/v1/runs", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		name, ok := c.authenticate(r)
		if name != want || ok != (want != "") {
			t.Errorf("%q: got %q %v, want %q", header, name, ok, want)
		}
	}
}

func TestStoreAddsSourceIPToAnOldAuditTable(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "faultline.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE audit (id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, actor TEXT NOT NULL, action TEXT NOT NULL, run_id TEXT NOT NULL, detail TEXT NOT NULL);
INSERT INTO audit (ts, actor, action, run_id, detail) VALUES ('2026-01-02T03:04:05Z', 'api-token', 'run_created', 'run_old', '{}');`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	for i := 0; i < 2; i++ {
		s, err := OpenStore(dir)
		if err != nil {
			t.Fatalf("open %d: %v", i+1, err)
		}
		if i == 0 {
			if err := s.Audit(Actor{Name: "token:alice", IP: "192.0.2.1"}, "kill_switch", "", nil); err != nil {
				t.Fatal(err)
			}
		}
		log, err := s.AuditLog(10)
		s.Close()
		if err != nil || len(log) != 2 || log[1].Actor != "api-token" || log[1].SourceIP != "" || log[0].Actor != "token:alice" || log[0].SourceIP != "192.0.2.1" {
			t.Fatalf("open %d: old entries keep an empty source_ip, new ones get theirs: %v %+v", i+1, err, log)
		}
	}
}

func TestClientIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	for _, tc := range []struct {
		name, peer, xff string
		trusted         []netip.Prefix
		want            string
	}{
		{"no proxies configured", "203.0.113.7:5000", "198.51.100.1", nil, "203.0.113.7"},
		{"untrusted peer cannot claim an address", "203.0.113.7:5000", "198.51.100.1", trusted, "203.0.113.7"},
		{"trusted peer without the header", "10.0.0.2:5000", "", trusted, "10.0.0.2"},
		{"trusted peer", "10.0.0.2:5000", "198.51.100.1", trusted, "198.51.100.1"},
		{"spoofed leftmost entry", "10.0.0.2:5000", "192.0.2.66, 198.51.100.1", trusted, "198.51.100.1"},
		{"chain of trusted proxies", "10.0.0.2:5000", "198.51.100.1, 10.0.0.9", trusted, "198.51.100.1"},
		{"garbage entry stops the walk", "10.0.0.2:5000", "198.51.100.1, not-an-ip", trusted, "10.0.0.2"},
		{"ipv6 peer", "[2001:db8::1]:5000", "", nil, "2001:db8::1"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tc.peer
		if tc.xff != "" {
			r.Header.Set("X-Forwarded-For", tc.xff)
		}
		if got := clientIP(r, tc.trusted); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
