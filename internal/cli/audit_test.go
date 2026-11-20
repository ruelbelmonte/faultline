package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"faultline/internal/control"
)

func TestAuditVerify(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	cfg := filepath.Join(dir, "faultline.yaml")
	os.WriteFile(cfg, []byte("version: 1\ndata_dir: "+filepath.ToSlash(data)+"\n"), 0o600)
	s, err := control.OpenStore(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"run_created", "kill_switch"} {
		s.Audit(control.Actor{Name: "token:alice", IP: "192.0.2.1"}, a, "", nil)
	}
	s.Close()

	if code, out, errOut := run("audit", "verify", "--config", cfg); code != 0 || !strings.Contains(out, "2 entries") {
		t.Fatalf("intact log: code %d\n%s%s", code, out, errOut)
	}

	jsonl := filepath.Join(data, "audit.jsonl")
	b, _ := os.ReadFile(jsonl)
	os.WriteFile(jsonl, []byte(strings.Replace(string(b), "run_created", "run_deleted", 1)), 0o600)
	if code, out, _ := run("audit", "verify", "--config", cfg); code != 10 || !strings.Contains(out, "line 2") {
		t.Fatalf("edited log: expected exit 10 naming line 2, got %d\n%s", code, out)
	}

	if code, _, _ := run("audit"); code != 2 {
		t.Fatalf("audit without a subcommand is a usage error, got %d", code)
	}
}
