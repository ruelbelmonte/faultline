package control

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"time"

	"faultline/internal/fault"
	"faultline/internal/findings"
)

// Request records are kept on disk while a run is active, so the evidence for
// findings is not bounded by memory. Only what the finding rules read is
// stored, and rows are deleted once the run's findings exist.

// correlationKey stands in for a correlation id on disk. The id is a header
// value chosen by the caller, so it is never written: rows carry a hash keyed
// with a salt that exists only in the memory of the run.
func correlationKey(salt []byte, correlation string) any {
	if correlation == "" {
		return nil
	}
	sum := sha256.Sum256(append(append([]byte{}, salt...), correlation...))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

func (s *Store) SaveRecords(runID string, salt []byte, recs []fault.Record) error {
	if len(recs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO records
		(run_id, seq, ts, route, corr, duration_ms, fault_id, retry_after, client_held_ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range recs {
		if _, err := stmt.Exec(runID, r.Seq, r.TS.UnixNano(), r.Route, correlationKey(salt, r.Correlation),
			r.DurationMs, r.FaultID, r.RetryAfter, r.ClientHeldMs); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteRecords(runID string) error {
	_, err := s.db.Exec(`DELETE FROM records WHERE run_id = ?`, runID)
	return err
}

// Evidence serves a run's stored records to the finding rules. Its callbacks
// run while a query is open and must not use the store.
func (s *Store) Evidence(runID string) findings.Evidence {
	return storedEvidence{s.db, runID}
}

type storedEvidence struct {
	db    *sql.DB
	runID string
}

func (e storedEvidence) FaultedRoutes() ([]string, error) {
	rows, err := e.db.Query(`SELECT DISTINCT route FROM records WHERE run_id = ? AND fault_id IS NOT NULL`, e.runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var routes []string
	for rows.Next() {
		var route string
		if err := rows.Scan(&route); err != nil {
			return nil, err
		}
		routes = append(routes, route)
	}
	return routes, rows.Err()
}

func (e storedEvidence) Attempts(fn func(route string, attempts []findings.Attempt)) error {
	rows, err := e.db.Query(`SELECT route, corr, ts, duration_ms, fault_id IS NOT NULL, retry_after
		FROM records WHERE run_id = ? AND corr IS NOT NULL ORDER BY route, corr, ts, seq`, e.runID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var (
		route   string
		corr    int64
		group   []findings.Attempt
		started bool
	)
	for rows.Next() {
		var (
			rt         string
			c, ts      int64
			a          findings.Attempt
			retryAfter sql.NullInt64
		)
		if err := rows.Scan(&rt, &c, &ts, &a.DurationMs, &a.Faulted, &retryAfter); err != nil {
			return err
		}
		a.TS = time.Unix(0, ts)
		if retryAfter.Valid {
			v := int(retryAfter.Int64)
			a.RetryAfter = &v
		}
		if started && (rt != route || c != corr) {
			fn(route, group)
			group = nil
		}
		route, corr, started = rt, c, true
		group = append(group, a)
	}
	if started {
		fn(route, group)
	}
	return rows.Err()
}

func (e storedEvidence) Holds(faultID string, fn func(findings.Hold)) error {
	rows, err := e.db.Query(`SELECT client_held_ms, ts, duration_ms FROM records
		WHERE run_id = ? AND fault_id = ? AND client_held_ms IS NOT NULL ORDER BY seq`, e.runID, faultID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			h        findings.Hold
			ts       int64
			duration float64
		)
		if err := rows.Scan(&h.HeldMs, &ts, &duration); err != nil {
			return err
		}
		h.End = time.Unix(0, ts).Add(time.Duration(duration * float64(time.Millisecond)))
		fn(h)
	}
	return rows.Err()
}
