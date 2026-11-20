package control

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"faultline/internal/config"
)

// Issue #25: the control API's auth middleware, directly. Only an exact
// bearer token passes; health stays open; unknown API paths answer 401
// before they answer 404.
func TestControlAuthMiddleware(t *testing.T) {
	const token = "unit-test-control-token-0123"
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	c, err := New(cfg, testTokens(token), "unit-test-admin-token-0123", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.store.Close()
	h := c.Handler()

	cases := []struct {
		name, path, auth string
		want             int
	}{
		{"missing", "/api/v1/runs", "", 401},
		{"empty bearer", "/api/v1/runs", "Bearer ", 401},
		{"wrong scheme", "/api/v1/runs", "Basic " + token, 401},
		{"lower-case scheme", "/api/v1/runs", "bearer " + token, 401},
		{"wrong", "/api/v1/runs", "Bearer unit-test-control-token-9999", 401},
		{"prefix of the token", "/api/v1/runs", "Bearer " + token[:len(token)-1], 401},
		{"token plus a byte", "/api/v1/runs", "Bearer " + token + "x", 401},
		{"trailing space", "/api/v1/runs", "Bearer " + token + " ", 401},
		{"the admin token", "/api/v1/runs", "Bearer unit-test-admin-token-0123", 401},
		{"correct", "/api/v1/runs", "Bearer " + token, 200},
		{"health without a token", "/healthz", "", 200},
		{"unknown API path without a token", "/api/v1/nope", "", 401},
		{"unknown API path with the token", "/api/v1/nope", "Bearer " + token, 404},
		{"outside v1 without a token", "/api/v2/runs", "", 401},
	}
	for _, tc := range cases {
		req := httptest.NewRequest("GET", tc.path, nil)
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: GET %s: got %d, want %d", tc.name, tc.path, rec.Code, tc.want)
		}
	}
}
