package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Issue #22: audit.jsonl is hash-chained, written before SQLite, and the two
// are reconciled at startup.

func writeAudit(t *testing.T, dir string, actions ...string) {
	t.Helper()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, a := range actions {
		if err := s.Audit(Actor{Name: "token:alice", IP: "192.0.2.1"}, a, "run_synthetic", map[string]any{"n": 1}); err != nil {
			t.Fatal(err)
		}
	}
}

func auditLines(t *testing.T, dir string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(data), "\n")
	return lines[:len(lines)-1]
}

func setAuditLines(t *testing.T, dir string, lines []string) {
	t.Helper()
	out := strings.Join(lines, "")
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "audit.jsonl"), []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustVerify(t *testing.T, dir string) []string {
	t.Helper()
	_, problems, err := VerifyAudit(dir)
	if err != nil {
		t.Fatal(err)
	}
	return problems
}

func TestAuditChainVerifies(t *testing.T) {
	dir := t.TempDir()
	writeAudit(t, dir, "run_created", "kill_switch")
	writeAudit(t, dir, "run_aborted")
	if p := mustVerify(t, dir); len(p) != 0 {
		t.Fatalf("an untouched log must verify: %v", p)
	}
	for i, line := range auditLines(t, dir) {
		var e struct {
			PrevHash string `json:"prev_hash"`
		}
		if json.Unmarshal([]byte(line), &e); len(e.PrevHash) != 64 {
			t.Fatalf("line %d has no prev_hash: %s", i+1, line)
		}
	}
}

func TestAuditVerifyDetectsTampering(t *testing.T) {
	for name, tamper := range map[string]func([]string) []string{
		"edited line": func(l []string) []string {
			l[1] = strings.Replace(l[1], "kill_switch", "run_created", 1)
			return l
		},
		"deleted line": func(l []string) []string { return append(l[:1], l[2:]...) },
		"reordered":    func(l []string) []string { l[1], l[2] = l[2], l[1]; return l },
		"deleted tail": func(l []string) []string { return l[:len(l)-1] },
		"inserted unchained line": func(l []string) []string {
			return append(l[:2], append([]string{`{"ts":"2026-01-01T00:00:00Z","actor":"token:mallory","source_ip":"","action":"run_created","run_id":"","detail":{}}` + "\n"}, l[2:]...)...)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeAudit(t, dir, "run_created", "kill_switch", "run_aborted", "report_downloaded")
			setAuditLines(t, dir, tamper(auditLines(t, dir)))
			if p := mustVerify(t, dir); len(p) == 0 {
				t.Fatal("tampering went unnoticed")
			}
		})
	}
}

func TestAuditChainAnchorsOnLegacyLines(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"ts":"2026-01-02T03:04:05Z","actor":"api-token","action":"run_created","run_id":"run_old","detail":{}}` + "\n"
	setAuditLines(t, dir, []string{legacy})
	writeAudit(t, dir, "kill_switch")
	if p := mustVerify(t, dir); len(p) != 0 {
		t.Fatalf("legacy lines followed by a chain must verify: %v", p)
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	log, _ := s.AuditLog(10)
	s.Close()
	if len(log) != 2 || log[1].Actor != "api-token" {
		t.Fatalf("a legacy line missing from SQLite is recovered from the file: %+v", log)
	}

	lines := auditLines(t, dir)
	lines[0] = strings.Replace(lines[0], "run_old", "run_new", 1)
	setAuditLines(t, dir, lines)
	if p := mustVerify(t, dir); len(p) == 0 {
		t.Fatal("an edit to a line before the chain started went unnoticed")
	}
}

// Control is killed after writing audit.jsonl and before writing SQLite.
func TestAuditRecoversEntryWrittenOnlyToTheFile(t *testing.T) {
	dir := t.TempDir()
	writeAudit(t, dir, "run_created")
	data, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	sum := sha256.Sum256(data)
	orphan, _ := json.Marshal(map[string]any{
		"ts": "2026-11-20T00:00:00Z", "actor": "token:bob", "source_ip": "192.0.2.2",
		"action": "kill_switch", "run_id": "", "detail": map[string]any{}, "prev_hash": hex.EncodeToString(sum[:]),
	})
	setAuditLines(t, dir, append(auditLines(t, dir), string(orphan)+"\n"))
	if p := mustVerify(t, dir); len(p) == 0 || !strings.Contains(strings.Join(p, " "), "SQLite") {
		t.Fatalf("before recovery, verify reports the entry missing from SQLite: %v", p)
	}

	writeAudit(t, dir, "report_downloaded")
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	log, _ := s.AuditLog(10)
	s.Close()
	if len(log) != 3 || log[1].Action != "kill_switch" || log[1].Actor != "token:bob" || log[0].Action != "report_downloaded" {
		t.Fatalf("the orphaned entry must be recovered into SQLite in order: %+v", log)
	}
	if p := mustVerify(t, dir); len(p) != 0 {
		t.Fatalf("after recovery the log verifies: %v", p)
	}
}

// A crash in the middle of a write leaves a fragment without a newline.
func TestAuditStartsAFreshLineAfterAFragment(t *testing.T) {
	dir := t.TempDir()
	writeAudit(t, dir, "run_created")
	f, _ := os.OpenFile(filepath.Join(dir, "audit.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"ts":"2026-11-20T00:00:00Z","act`)
	f.Close()
	writeAudit(t, dir, "kill_switch")
	lines := auditLines(t, dir)
	if len(lines) != 3 || !strings.Contains(lines[2], `"action":"kill_switch"`) {
		t.Fatalf("the next entry must start on its own line: %q", lines)
	}
	p := mustVerify(t, dir)
	if len(p) != 1 || !strings.Contains(p[0], "line 2") {
		t.Fatalf("verify must report the fragment, and only it: %v", p)
	}
}
