package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A control plane that answers with a hostile run id must not be able to
// steer where the CLI writes report files.
func TestRunRejectsMalformedRunIDFromServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"run_id":"../../escape"}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	exp := filepath.Join(dir, "exp.yaml")
	os.WriteFile(exp, []byte("version: 1\n"), 0o644)
	t.Setenv("FAULTLINE_TOKEN", "0123456789abcdef")
	reports := filepath.Join(dir, "reports")
	code, _, errOut := run("run", exp, "--url", srv.URL, "--report-dir", reports)
	if code != 1 || !strings.Contains(errOut, "malformed run id") {
		t.Fatalf("expected exit 1 and a clear message, got %d: %s", code, errOut)
	}
	if _, err := os.Stat(reports); err == nil {
		t.Fatal("nothing may be written for a run with a malformed id")
	}
}

func TestRemoteURLAndRunIDChecks(t *testing.T) {
	t.Setenv("FAULTLINE_TOKEN", "0123456789abcdef")
	if code, _, errOut := run("status", "--url", "ftp://files.invalid"); code != 2 || !strings.Contains(errOut, "not an http or https URL") {
		t.Fatalf("expected exit 2, got %d: %s", code, errOut)
	}
	if code, _, _ := run("report", "../../etc/passwd", "--format", "json"); code != 2 {
		t.Fatalf("a malformed run id is a usage error, got %d", code)
	}
	for host, want := range map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true, "10.0.0.5": false, "qa.internal": false} {
		if got := loopback(host); got != want {
			t.Errorf("loopback(%q) = %v, want %v", host, got, want)
		}
	}
}
