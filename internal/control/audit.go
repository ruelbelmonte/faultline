package control

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Each line of audit.jsonl carries prev_hash: the SHA-256 of every byte of the
// file before that line. An edit, deletion or reordering anywhere earlier,
// including lines written before the chain existed, breaks the next hash (D113).

type auditLine struct {
	n       int
	entry   AuditEntry
	ok      bool
	chained bool
	prevOK  bool
}

// readAuditFile splits audit.jsonl into lines and checks each prev_hash
// against the bytes before it.
func readAuditFile(data []byte) []auditLine {
	h := sha256.New()
	var out []auditLine
	for n := 1; len(data) > 0; n++ {
		line := data
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line = data[:i+1]
		}
		data = data[len(line):]
		l := auditLine{n: n}
		var e AuditEntry
		if bytes.HasSuffix(line, []byte("\n")) && json.Unmarshal(line, &e) == nil && e.Action != "" {
			l.entry, l.ok, l.chained = e, true, e.PrevHash != ""
			l.prevOK = e.PrevHash == hex.EncodeToString(h.Sum(nil))
		}
		h.Write(line)
		out = append(out, l)
	}
	return out
}

func auditEntries(lines []auditLine) []AuditEntry {
	var out []AuditEntry
	for _, l := range lines {
		if l.ok {
			out = append(out, l.entry)
		}
	}
	return out
}

func insertAudit(db *sql.DB, e AuditEntry) error {
	detail, err := json.Marshal(e.Detail)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO audit (ts, actor, source_ip, action, run_id, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		e.TS.Format(time.RFC3339), e.Actor, e.SourceIP, e.Action, e.RunID, string(detail))
	return err
}

// recoverAudit copies entries that reached audit.jsonl but not SQLite, which
// happens when control dies between the two writes.
func recoverAudit(db *sql.DB, lines []auditLine) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit`).Scan(&n); err != nil {
		return err
	}
	entries := auditEntries(lines)
	for _, e := range entries[min(n, len(entries)):] {
		if err := insertAudit(db, e); err != nil {
			return err
		}
	}
	return nil
}

// VerifyAudit checks the hash chain of audit.jsonl and that faultline.db
// holds the same entries in the same order. It only reads, so it can run
// beside a live control plane. It returns one line per problem.
func VerifyAudit(dataDir string) (entries int, problems []string, err error) {
	data, err := os.ReadFile(filepath.Join(dataDir, "audit.jsonl"))
	if err != nil {
		return 0, nil, err
	}

	lines := readAuditFile(data)
	chained := false
	for _, l := range lines {
		switch {
		case !l.ok:
			problems = append(problems, fmt.Sprintf("line %d is not a complete audit entry", l.n))
		case l.chained && !l.prevOK:
			problems = append(problems, fmt.Sprintf("line %d: prev_hash does not match the lines before it, so an earlier line was edited, removed or reordered", l.n))
		case !l.chained && chained:
			problems = append(problems, fmt.Sprintf("line %d has no prev_hash although earlier lines do, so it was inserted", l.n))
		}
		chained = chained || l.chained
	}

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dataDir, "faultline.db"))+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return 0, nil, err
	}
	defer db.Close()
	stored, err := readAuditDB(db)
	if err != nil {
		return 0, nil, err
	}
	file := auditEntries(lines)
	for i := 0; i < max(len(file), len(stored)); i++ {
		switch {
		case i >= len(stored):
			problems = append(problems, fmt.Sprintf("entry %d (%s) is in audit.jsonl but not in SQLite; control adds it at its next start", i+1, file[i].Action))
		case i >= len(file):
			problems = append(problems, fmt.Sprintf("SQLite entry %d (%s) is missing from audit.jsonl", i+1, stored[i].Action))
		case !sameAuditEntry(file[i], stored[i]):
			problems = append(problems, fmt.Sprintf("entry %d differs between audit.jsonl (%s) and SQLite (%s)", i+1, file[i].Action, stored[i].Action))
		}
	}
	return len(file), problems, nil
}

func readAuditDB(db *sql.DB) ([]AuditEntry, error) {
	var hasSourceIP bool
	if err := db.QueryRow(`SELECT COUNT(*) > 0 FROM pragma_table_info('audit') WHERE name = 'source_ip'`).Scan(&hasSourceIP); err != nil {
		return nil, err
	}
	sourceIP := "''"
	if hasSourceIP {
		sourceIP = "source_ip"
	}
	rows, err := db.Query(`SELECT ts, actor, ` + sourceIP + `, action, run_id, detail FROM audit ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ts, detail string
		if err := rows.Scan(&ts, &e.Actor, &e.SourceIP, &e.Action, &e.RunID, &detail); err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			return nil, err
		}
		e.TS.Time = t
		if err := json.Unmarshal([]byte(detail), &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func sameAuditEntry(a, b AuditEntry) bool {
	da, _ := json.Marshal(a.Detail)
	db, _ := json.Marshal(b.Detail)
	return a.TS.Equal(b.TS.Time) && a.Actor == b.Actor && a.SourceIP == b.SourceIP &&
		a.Action == b.Action && a.RunID == b.RunID && bytes.Equal(da, db)
}
