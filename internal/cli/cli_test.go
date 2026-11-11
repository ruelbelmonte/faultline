package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestVersion(t *testing.T) {
	code, out, _ := run("version")
	if code != 0 || !strings.HasPrefix(out, "faultline ") {
		t.Fatalf("code %d, out %q", code, out)
	}
}

func TestUnknownCommandIsUsageError(t *testing.T) {
	if code, _, _ := run("frobnicate"); code != 2 {
		t.Fatalf("expected exit 2, got %d", code)
	}
	if code, _, _ := run(); code != 2 {
		t.Fatalf("expected exit 2 with no arguments, got %d", code)
	}
}

func TestInitOutputValidates(t *testing.T) {
	dir := t.TempDir()
	if code, _, errOut := run("init", dir); code != 0 {
		t.Fatalf("init failed with %d: %s", code, errOut)
	}
	for _, f := range []string{"faultline.yaml", "experiments/example.yaml", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("init did not create %s: %v", f, err)
		}
	}
	ignore, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if !strings.Contains(string(ignore), ".faultline/") {
		t.Fatalf(".gitignore lacks the data_dir entry: %q", ignore)
	}
	cfg := filepath.Join(dir, "faultline.yaml")
	code, out, errOut := run("validate", filepath.Join(dir, "experiments", "example.yaml"), "--config", cfg)
	if code != 0 {
		t.Fatalf("init example does not validate (%d): %s %s", code, out, errOut)
	}
	if code, out, _ = run("validate", "--config", cfg, filepath.Join(dir, "experiments")); code != 0 {
		t.Fatalf("validating the directory failed (%d): %s", code, out)
	}
}

func TestInitIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_modules/"), 0o644)
	run("init", dir)
	custom := []byte("version: 1\n# mine\n")
	os.WriteFile(filepath.Join(dir, "faultline.yaml"), custom, 0o644)
	if code, _, _ := run("init", dir); code != 0 {
		t.Fatalf("second init failed with %d", code)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "faultline.yaml"))
	if !bytes.Equal(got, custom) {
		t.Fatal("init overwrote an existing faultline.yaml")
	}
	ignore, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if string(ignore) != "node_modules/\n.faultline/\n" {
		t.Fatalf("unexpected .gitignore: %q", ignore)
	}
}

func TestValidateReportsPathAndMessage(t *testing.T) {
	dir := t.TempDir()
	run("init", dir)
	bad := filepath.Join(dir, "experiments", "bad.yaml")
	os.WriteFile(bad, []byte("version: 1\nname: bad\ntarget: {route: inventory}\nduration: 5s\nfaults:\n  - {id: f1, type: gremlin}\n"), 0o644)
	code, out, _ := run("validate", bad, "--config", filepath.Join(dir, "faultline.yaml"))
	if code != 2 {
		t.Fatalf("expected exit 2, got %d", code)
	}
	found := false
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "faults[0].type: ") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a line starting with %q, got:\n%s", "faults[0].type: ", out)
	}
	if code, _, _ := run("validate", filepath.Join(dir, "experiments"), "--config", filepath.Join(dir, "faultline.yaml")); code != 2 {
		t.Fatalf("a directory with one invalid file must exit 2, got %d", code)
	}
}

func TestValidateUsageErrors(t *testing.T) {
	dir := t.TempDir()
	run("init", dir)
	cfg := filepath.Join(dir, "faultline.yaml")
	if code, _, _ := run("validate", "--config", cfg); code != 2 {
		t.Fatalf("missing argument: expected exit 2, got %d", code)
	}
	if code, _, _ := run("validate", filepath.Join(dir, "nope.yaml"), "--config", cfg); code != 2 {
		t.Fatalf("missing file: expected exit 2, got %d", code)
	}
	if code, _, _ := run("validate", filepath.Join(dir, "experiments"), "--config", filepath.Join(dir, "nope.yaml")); code != 2 {
		t.Fatalf("missing config: expected exit 2, got %d", code)
	}
}
