// Package storage persists aggregated traffic into SQLite and answers the
// ranking / time-series queries the API exposes. It uses the pure-Go
// modernc.org/sqlite driver so the binary needs no system SQLite.
package storage

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/doldoldol21/netscope/pkg/types"
	_ "modernc.org/sqlite"
)

// Store is a handle to the netscope database.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS app_samples (
	bucket  INTEGER NOT NULL,           -- unix seconds, floored to bucket size
	app     TEXT    NOT NULL,
	path    TEXT    NOT NULL DEFAULT '',
	rx      INTEGER NOT NULL DEFAULT 0,
	tx      INTEGER NOT NULL DEFAULT 0,
	conns   INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (bucket, app)
);
CREATE INDEX IF NOT EXISTS idx_app_bucket ON app_samples(bucket);

CREATE TABLE IF NOT EXISTS domain_samples (
	bucket   INTEGER NOT NULL,
	domain   TEXT    NOT NULL,
	app      TEXT    NOT NULL DEFAULT '',
	rx       INTEGER NOT NULL DEFAULT 0,
	tx       INTEGER NOT NULL DEFAULT 0,
	category TEXT    NOT NULL DEFAULT '',
	PRIMARY KEY (bucket, domain, app)
);
CREATE INDEX IF NOT EXISTS idx_domain_bucket ON domain_samples(bucket);
`

// Open opens (creating if needed) the SQLite database at path and applies the
// schema and connection pragmas.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// modernc/sqlite is safe for a single writer; serialise to avoid
	// SQLITE_BUSY under concurrent API reads + engine flushes.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// FlushApps adds the per-app counters for the given bucket. Repeated flushes to
// the same bucket accumulate rather than overwrite.
func (s *Store) FlushApps(bucket int64, apps []types.AppTraffic) error {
	if len(apps) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`
		INSERT INTO app_samples (bucket, app, path, rx, tx, conns)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(bucket, app) DO UPDATE SET
			rx    = rx + excluded.rx,
			tx    = tx + excluded.tx,
			conns = MAX(conns, excluded.conns),
			path  = excluded.path`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, a := range apps {
		if _, err := stmt.Exec(bucket, a.Name, a.Path, a.RxBytes, a.TxBytes, a.Connections); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// FlushDomains adds the per-domain counters for the given bucket.
func (s *Store) FlushDomains(bucket int64, domains []types.DomainStat) error {
	if len(domains) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`
		INSERT INTO domain_samples (bucket, domain, app, rx, tx, category)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(bucket, domain, app) DO UPDATE SET
			rx = rx + excluded.rx,
			tx = tx + excluded.tx`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, d := range domains {
		if _, err := stmt.Exec(bucket, d.Domain, d.AppName, d.RxBytes, d.TxBytes, d.Category); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Apps returns per-app totals over [since, until), ranked by total bytes.
func (s *Store) Apps(since, until time.Time) ([]types.AppTraffic, error) {
	rows, err := s.db.Query(`
		SELECT app, MAX(path), SUM(rx), SUM(tx), MAX(conns)
		FROM app_samples
		WHERE bucket >= ? AND bucket < ?
		GROUP BY app
		ORDER BY SUM(rx) + SUM(tx) DESC`,
		since.Unix(), until.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []types.AppTraffic
	for rows.Next() {
		var a types.AppTraffic
		if err := rows.Scan(&a.Name, &a.Path, &a.RxBytes, &a.TxBytes, &a.Connections); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Domains returns per-domain totals over [since, until), ranked by total bytes.
func (s *Store) Domains(since, until time.Time) ([]types.DomainStat, error) {
	rows, err := s.db.Query(`
		SELECT domain, MAX(app), SUM(rx), SUM(tx), MAX(category)
		FROM domain_samples
		WHERE bucket >= ? AND bucket < ?
		GROUP BY domain
		ORDER BY SUM(rx) + SUM(tx) DESC`,
		since.Unix(), until.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []types.DomainStat
	for rows.Next() {
		var d types.DomainStat
		if err := rows.Scan(&d.Domain, &d.AppName, &d.RxBytes, &d.TxBytes, &d.Category); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// TimeSeries returns rx/tx totals bucketed into intervals of step over
// [since, until). Empty intervals are omitted.
func (s *Store) TimeSeries(since, until time.Time, step time.Duration) ([]types.TimePoint, error) {
	stepSec := int64(step.Seconds())
	if stepSec <= 0 {
		stepSec = 60
	}
	rows, err := s.db.Query(`
		SELECT (bucket / ?) * ? AS slot, SUM(rx), SUM(tx)
		FROM app_samples
		WHERE bucket >= ? AND bucket < ?
		GROUP BY slot
		ORDER BY slot`,
		stepSec, stepSec, since.Unix(), until.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []types.TimePoint
	for rows.Next() {
		var slot int64
		var p types.TimePoint
		if err := rows.Scan(&slot, &p.RxBytes, &p.TxBytes); err != nil {
			return nil, err
		}
		p.Time = time.Unix(slot, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Purge deletes samples older than before, for retention management.
func (s *Store) Purge(before time.Time) error {
	cut := before.Unix()
	if _, err := s.db.Exec(`DELETE FROM app_samples WHERE bucket < ?`, cut); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM domain_samples WHERE bucket < ?`, cut); err != nil {
		return err
	}
	return nil
}
