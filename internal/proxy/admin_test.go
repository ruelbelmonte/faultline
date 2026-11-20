package proxy

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"faultline/internal/config"
)

// Issue #25: the proxy admin API's auth middleware, directly. Only an exact
// bearer token passes; health stays open; unknown paths answer 401 before
// they answer 404.
func TestAdminAuthMiddleware(t *testing.T) {
	const token = "unit-test-admin-token-0123"
	p, err := New(config.Default(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	h := p.Admin(token, "")

	cases := []struct {
		name, method, path, auth string
		want                     int
	}{
		{"missing", "GET", "/admin/v1/stats", "", 401},
		{"empty bearer", "GET", "/admin/v1/stats", "Bearer ", 401},
		{"wrong scheme", "GET", "/admin/v1/stats", "Basic " + token, 401},
		{"wrong", "GET", "/admin/v1/stats", "Bearer unit-test-admin-token-9999", 401},
		{"prefix of the token", "GET", "/admin/v1/stats", "Bearer " + token[:len(token)-1], 401},
		{"token plus a byte", "GET", "/admin/v1/stats", "Bearer " + token + "x", 401},
		{"correct", "GET", "/admin/v1/stats", "Bearer " + token, 200},
		{"clear without a token", "DELETE", "/admin/v1/faults", "", 401},
		{"health without a token", "GET", "/admin/v1/healthz", "", 200},
		{"unknown path without a token", "GET", "/admin/v1/nope", "", 401},
		{"unknown path with the token", "GET", "/admin/v1/nope", "Bearer " + token, 404},
		{"root without a token", "GET", "/", "", 401},
		{"metrics off, without a token", "GET", "/metrics", "", 401},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: %s %s: got %d, want %d", tc.name, tc.method, tc.path, rec.Code, tc.want)
		}
	}
}
