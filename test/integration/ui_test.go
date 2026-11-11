//go:build integration

package integration

import (
	"regexp"
	"strings"
	"testing"
)

var assetRef = regexp.MustCompile(`/assets/[A-Za-z0-9._-]+\.(js|css)`)

// The UI is embedded in the binary and served without a token; it holds no
// data until the browser supplies one.
func TestEmbeddedUI(t *testing.T) {
	f := startFull(t, "naive")

	index := call("GET", f.control.url("/"), nil, "")
	if index.status != 200 || !strings.Contains(string(index.body), `<div id="app">`) {
		t.Fatalf("index: %d %s", index.status, index.body)
	}
	if index.header.Get("Cache-Control") != "no-store" || index.header.Get("Content-Security-Policy") == "" || index.header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("index headers: %v", index.header)
	}
	refs := assetRef.FindAllString(string(index.body), -1)
	if len(refs) < 2 {
		t.Fatalf("index.html should reference the built script and stylesheet: %s", index.body)
	}
	var bundle string
	for _, ref := range refs {
		res := call("GET", f.control.url(ref), nil, "")
		if res.status != 200 || len(res.body) == 0 {
			t.Fatalf("%s: %d", ref, res.status)
		}
		if strings.HasSuffix(ref, ".js") {
			if !strings.Contains(res.header.Get("Content-Type"), "javascript") {
				t.Fatalf("%s content type %q", ref, res.header.Get("Content-Type"))
			}
			bundle = string(res.body)
		}
	}
	for _, id := range []string{"token-input", "token-submit", "kill-switch", "runs-table", "run-verdict", "export-html", "export-md", "export-json", "export-junit"} {
		if !strings.Contains(bundle, id) {
			t.Errorf("the built UI lacks data-testid %q", id)
		}
	}
	if strings.Contains(bundle, controlToken) || strings.Contains(string(index.body), controlToken) {
		t.Fatal("the served UI must not contain the token")
	}

	// Client-side routes fall back to the app shell; missing assets do not.
	if res := call("GET", f.control.url("/some/client/route"), nil, ""); res.status != 200 || !strings.Contains(string(res.body), `<div id="app">`) {
		t.Fatalf("fallback: %d", res.status)
	}
	if res := call("GET", f.control.url("/assets/missing.js"), nil, ""); res.status != 404 {
		t.Fatalf("missing asset: expected 404, got %d", res.status)
	}
	if res := call("POST", f.control.url("/"), nil, "x"); res.status != 405 {
		t.Fatalf("POST /: expected 405, got %d", res.status)
	}

	var meta map[string]any
	if code := f.control.api(t, "GET", "/api/v1/meta", nil, &meta); code != 200 || meta["environment"] != "local" {
		t.Fatalf("meta: %d %v", code, meta)
	}
}
