// Package control is the Faultline control plane: API, run orchestration,
// traffic driver, probes, metrics, verdicts and audit log.
package control

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"faultline/internal/report"
)

var ErrNotFound = errors.New("not found")

const schema = `
CREATE TABLE IF NOT EXISTS runs (
	run_id          TEXT PRIMARY KEY,
	experiment      TEXT NOT NULL,
	status          TEXT NOT NULL,
	verdict         TEXT,
	started_at      TEXT NOT NULL,
	doc             TEXT NOT NULL,
	meta            TEXT NOT NULL,
	experiment_yaml TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS buckets (
	run_id TEXT NOT NULL,
	t      INTEGER NOT NULL,
	data   TEXT NOT NULL,
	PRIMARY KEY (run_id, t)
);
CREATE TABLE IF NOT EXISTS audit (
	id     INTEGER PRIMARY KEY AUTOINCREMENT,
	ts     TEXT NOT NULL,
	actor  TEXT NOT NULL,
	action TEXT NOT NULL,
	run_id TEXT NOT NULL,
	detail TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS records (
	run_id         TEXT NOT NULL,
	seq            INTEGER NOT NULL,
	ts             INTEGER NOT NULL,
	route          TEXT NOT NULL,
	corr           INTEGER,
	duration_ms    REAL NOT NULL,
	fault_id       TEXT,
	retry_after    INTEGER,
	client_held_ms REAL,
	PRIMARY KEY (run_id, seq)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS records_attempts ON records (run_id, route, corr, ts, seq);`

// Store persists runs, buckets and the audit log in SQLite, and mirrors the
// audit log to audit.jsonl. It never receives unredacted experiments.
type Store struct {
	db      *sql.DB
	dataDir string

	auditMu    sync.Mutex
	auditFile  *os.File
	auditChain hash.Hash
}

func OpenStore(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "faultline.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// Request records only live while their run is being evaluated; whatever
	// a previous process left behind belongs to no run any more.
	if _, err := db.Exec(schema + "\nDELETE FROM records;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialising database: %w", err)
	}
	if err := addAuditSourceIP(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("adding audit.source_ip: %w", err)
	}
	s, err := openAuditFile(db, dataDir)
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// openAuditFile primes the hash chain with the file's bytes, starts a fresh
// line after a fragment left by a crash, and recovers entries SQLite lacks.
func openAuditFile(db *sql.DB, dataDir string) (*Store, error) {
	path := filepath.Join(dataDir, "audit.jsonl")
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err := recoverAudit(db, readAuditFile(data)); err != nil {
		return nil, fmt.Errorf("recovering audit entries: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	chain := sha256.New()
	chain.Write(data)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		if _, err := f.Write([]byte("\n")); err != nil {
			f.Close()
			return nil, err
		}
		chain.Write([]byte("\n"))
	}
	return &Store{db: db, dataDir: dataDir, auditFile: f, auditChain: chain}, nil
}

func (s *Store) Close() error {
	s.auditFile.Close()
	return s.db.Close()
}

// Meta is the timeline context stored next to a run.
type Meta struct {
	Phases       []report.Phase       `json:"phases"`
	FaultWindows []report.FaultWindow `json:"fault_windows"`
	Markers      []report.Marker      `json:"markers"`
}

func (s *Store) SaveRun(doc *report.Document, meta *Meta, redactedYAML []byte) error {
	docJSON, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO runs (run_id, experiment, status, verdict, started_at, doc, meta, experiment_yaml)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET status = excluded.status, verdict = excluded.verdict, doc = excluded.doc, meta = excluded.meta`,
		doc.RunID, doc.Experiment.Name, doc.Status, doc.Verdict, doc.StartedAt.Format(time.RFC3339), string(docJSON), string(metaJSON), string(redactedYAML))
	return err
}

func (s *Store) Run(id string) (*report.Document, *Meta, error) {
	var docJSON, metaJSON string
	err := s.db.QueryRow(`SELECT doc, meta FROM runs WHERE run_id = ?`, id).Scan(&docJSON, &metaJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	var doc report.Document
	var meta Meta
	if err := json.Unmarshal([]byte(docJSON), &doc); err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		return nil, nil, err
	}
	return &doc, &meta, nil
}

type RunSummary struct {
	RunID      string  `json:"run_id"`
	Experiment string  `json:"experiment"`
	Status     string  `json:"status"`
	Verdict    *string `json:"verdict"`
	StartedAt  string  `json:"started_at"`
}

func (s *Store) Runs() ([]RunSummary, error) {
	rows, err := s.db.Query(`SELECT run_id, experiment, status, verdict, started_at FROM runs ORDER BY rowid DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RunSummary{}
	for rows.Next() {
		var r RunSummary
		if err := rows.Scan(&r.RunID, &r.Experiment, &r.Status, &r.Verdict, &r.StartedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Unfinished lists runs left in a non-terminal status by a previous process.
func (s *Store) Unfinished() ([]string, error) {
	rows, err := s.db.Query(`SELECT run_id FROM runs WHERE status NOT IN (?, ?) ORDER BY rowid`, report.StatusCompleted, report.StatusAborted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) SaveBucket(runID string, b *Bucket) error {
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO buckets (run_id, t, data) VALUES (?, ?, ?)
		ON CONFLICT(run_id, t) DO UPDATE SET data = excluded.data`, runID, b.T.Unix(), string(data))
	return err
}

func (s *Store) Buckets(runID string) ([]*Bucket, error) {
	rows, err := s.db.Query(`SELECT data FROM buckets WHERE run_id = ? ORDER BY t`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Bucket
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var b Bucket
		if err := json.Unmarshal([]byte(data), &b); err != nil {
			return nil, err
		}
		out = append(out, &b)
	}
	return out, rows.Err()
}

// addAuditSourceIP migrates a database created before audit.source_ip existed.
func addAuditSourceIP(db *sql.DB) error {
	var has bool
	if err := db.QueryRow(`SELECT COUNT(*) > 0 FROM pragma_table_info('audit') WHERE name = 'source_ip'`).Scan(&has); err != nil || has {
		return err
	}
	_, err := db.Exec(`ALTER TABLE audit ADD COLUMN source_ip TEXT NOT NULL DEFAULT ''`)
	return err
}

type AuditEntry struct {
	TS       report.Time    `json:"ts"`
	Actor    string         `json:"actor"`
	SourceIP string         `json:"source_ip"`
	Action   string         `json:"action"`
	RunID    string         `json:"run_id"`
	Detail   map[string]any `json:"detail"`
	// PrevHash is set in audit.jsonl only (D113).
	PrevHash string `json:"prev_hash,omitempty"`
}

// Actor is who an audit entry is attributed to: "token:<name>" with the
// request's source address, or "system" for automatic actions (D112).
type Actor struct{ Name, IP string }

var ActorSystem = Actor{Name: "system"}

// Audit appends an entry to SQLite and audit.jsonl. Entries are never updated or deleted.
func (s *Store) Audit(actor Actor, action, runID string, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	e := AuditEntry{TS: report.At(time.Now()), Actor: actor.Name, SourceIP: actor.IP, Action: action, RunID: runID, Detail: detail}
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	e.PrevHash = hex.EncodeToString(s.auditChain.Sum(nil))
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	// The chained file is the record of truth: written and synced first, so a
	// crash before the SQLite insert is repaired from it at the next start.
	if _, err := s.auditFile.Write(line); err != nil {
		return err
	}
	s.auditChain.Write(line)
	if err := s.auditFile.Sync(); err != nil {
		return err
	}
	return insertAudit(s.db, e)
}

func (s *Store) AuditLog(limit int) ([]AuditEntry, error) {
	rows, err := s.db.Query(`SELECT ts, actor, source_ip, action, run_id, detail FROM audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
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
		e.TS = report.At(t)
		if err := json.Unmarshal([]byte(detail), &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
