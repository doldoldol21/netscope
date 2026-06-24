// Package storage persists aggregated traffic into SQLite and answers the
// ranking / time-series queries the API exposes. It uses the pure-Go
// modernc.org/sqlite driver so the binary needs no system SQLite.
package storage

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/doldoldol21/netscope/pkg/types"
	_ "modernc.org/sqlite"
)

// Store is a handle to the netscope database.
type Store struct {
	db   *sql.DB
	path string // db file path, for on-disk size accounting
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
	country  TEXT    NOT NULL DEFAULT '',
	PRIMARY KEY (bucket, domain, app)
);
CREATE INDEX IF NOT EXISTS idx_domain_bucket ON domain_samples(bucket);
`

// Open opens (creating if needed) the SQLite database at path and applies the
// schema and connection pragmas. If the existing database is corrupt (e.g. after
// power loss), it is quarantined aside and a fresh one is created — otherwise a
// fatal Open error would brick the daemon in a launchd KeepAlive crash loop
// until someone manually deletes the file.
func Open(path string) (*Store, error) {
	st, err := openOnce(path)
	if err == nil {
		return st, nil
	}
	// Couldn't open/validate the existing file — quarantine it and start fresh.
	// Losing history beats a daemon that never starts again.
	corrupt := path + ".corrupt"
	_ = os.Remove(corrupt)
	if rerr := os.Rename(path, corrupt); rerr != nil {
		return nil, fmt.Errorf("open db (%v) and could not quarantine corrupt file: %w", err, rerr)
	}
	// Also clear any leftover WAL/SHM that belonged to the corrupt DB.
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
	log.Printf("storage: %s was unusable (%v); quarantined to %s and recreated", path, err, corrupt)
	return openOnce(path)
}

// openOnce opens path, applies the schema, and runs a quick integrity check.
// Any failure (including corruption) is returned so the caller can quarantine.
func openOnce(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// modernc/sqlite is safe for a single writer; serialise to avoid
	// SQLITE_BUSY under concurrent API reads + engine flushes.
	db.SetMaxOpenConns(1)
	// Detect corruption up front: quick_check is cheap and reports the kind of
	// page/index damage an unclean shutdown can leave behind. Only trust an
	// explicit "ok"; any other result (or error) means the file is unusable.
	var res string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&res); err != nil {
		db.Close()
		return nil, fmt.Errorf("integrity check: %w", err)
	}
	if res != "ok" {
		db.Close()
		return nil, fmt.Errorf("integrity check failed: %s", res)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	// Migrate: add the country column to databases created before GeoIP support.
	// (CREATE TABLE above already includes it for fresh DBs; this is a no-op error
	// on those, which we ignore.)
	_, _ = db.Exec(`ALTER TABLE domain_samples ADD COLUMN country TEXT NOT NULL DEFAULT ''`)
	// Cap the WAL's auto-checkpoint so it can't balloon between maintenance runs.
	_, _ = db.Exec(`PRAGMA wal_autocheckpoint=1000`) // ~4MB of pages
	return &Store{db: db, path: path}, nil
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
		INSERT INTO domain_samples (bucket, domain, app, rx, tx, category, country)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(bucket, domain, app) DO UPDATE SET
			rx = rx + excluded.rx,
			tx = tx + excluded.tx`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, d := range domains {
		if _, err := stmt.Exec(bucket, d.Domain, d.AppName, d.RxBytes, d.TxBytes, d.Category, d.Country); err != nil {
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
		SELECT domain, MAX(app), SUM(rx), SUM(tx), MAX(category), MAX(country)
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
		if err := rows.Scan(&d.Domain, &d.AppName, &d.RxBytes, &d.TxBytes, &d.Category, &d.Country); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DomainsForApp returns per-domain totals for a single app over [since, until),
// ranked by total bytes — backs the dashboard's per-app drill-down.
func (s *Store) DomainsForApp(app string, since, until time.Time) ([]types.DomainStat, error) {
	rows, err := s.db.Query(`
		SELECT domain, MAX(app), SUM(rx), SUM(tx), MAX(category), MAX(country)
		FROM domain_samples
		WHERE bucket >= ? AND bucket < ? AND app = ?
		GROUP BY domain
		ORDER BY SUM(rx) + SUM(tx) DESC`,
		since.Unix(), until.Unix(), app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []types.DomainStat
	for rows.Next() {
		var d types.DomainStat
		if err := rows.Scan(&d.Domain, &d.AppName, &d.RxBytes, &d.TxBytes, &d.Category, &d.Country); err != nil {
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

// AppTimeSeries returns one app's rx/tx bucketed into intervals of step over
// [since, until). Empty intervals are omitted.
func (s *Store) AppTimeSeries(app string, since, until time.Time, step time.Duration) ([]types.TimePoint, error) {
	stepSec := int64(step.Seconds())
	if stepSec <= 0 {
		stepSec = 60
	}
	rows, err := s.db.Query(`
		SELECT (bucket / ?) * ? AS slot, SUM(rx), SUM(tx)
		FROM app_samples
		WHERE bucket >= ? AND bucket < ? AND app = ?
		GROUP BY slot
		ORDER BY slot`,
		stepSec, stepSec, since.Unix(), until.Unix(), app)
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

// Purge deletes samples older than before, for retention management. It reports
// whether any rows were actually removed (so the caller can decide to VACUUM).
func (s *Store) Purge(before time.Time) (bool, error) {
	cut := before.Unix()
	r1, err := s.db.Exec(`DELETE FROM app_samples WHERE bucket < ?`, cut)
	if err != nil {
		return false, err
	}
	r2, err := s.db.Exec(`DELETE FROM domain_samples WHERE bucket < ?`, cut)
	if err != nil {
		return false, err
	}
	n1, _ := r1.RowsAffected()
	n2, _ := r2.RowsAffected()
	return n1+n2 > 0, nil
}

// SizeOnDisk returns the total bytes the database occupies, including the WAL
// and shared-memory side files (which can dominate between checkpoints).
func (s *Store) SizeOnDisk() int64 {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if fi, err := os.Stat(s.path + suffix); err == nil {
			total += fi.Size()
		}
	}
	return total
}

// Checkpoint flushes the WAL back into the main database and truncates it, so
// the WAL file can't grow without bound. Best-effort.
func (s *Store) Checkpoint() {
	_, _ = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
}

// Vacuum rebuilds the database, reclaiming pages freed by deletes so the file
// actually shrinks on disk. It rewrites the whole file, so call it sparingly.
func (s *Store) Vacuum() error {
	_, err := s.db.Exec(`VACUUM`)
	return err
}

// EnforceSizeCap is the disk safety net: independent of time-based retention, it
// drops the oldest data (a day at a time) until the database fits under
// maxBytes. It checkpoints + vacuums between steps so on-disk size reflects each
// deletion. Returns whether anything was removed. A non-positive cap disables it.
func (s *Store) EnforceSizeCap(maxBytes int64) (bool, error) {
	if maxBytes <= 0 {
		return false, nil
	}
	deleted := false
	for i := 0; i < 400; i++ { // bounded: ~a year of daily steps
		if s.SizeOnDisk() <= maxBytes {
			break
		}
		var oldest sql.NullInt64
		if err := s.db.QueryRow(`SELECT MIN(bucket) FROM app_samples`).Scan(&oldest); err != nil {
			return deleted, err
		}
		if !oldest.Valid {
			break // table empty; can't shrink further by deleting rows
		}
		cut := oldest.Int64 + 86400 // drop the oldest day
		if _, err := s.db.Exec(`DELETE FROM app_samples WHERE bucket < ?`, cut); err != nil {
			return deleted, err
		}
		if _, err := s.db.Exec(`DELETE FROM domain_samples WHERE bucket < ?`, cut); err != nil {
			return deleted, err
		}
		deleted = true
		s.Checkpoint()
		_ = s.Vacuum() // reclaim so the next SizeOnDisk reflects the deletion
	}
	return deleted, nil
}
