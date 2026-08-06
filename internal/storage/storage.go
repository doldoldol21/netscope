// Package storage persists aggregated traffic into SQLite and answers the
// ranking / time-series queries the API exposes. It uses the pure-Go
// modernc.org/sqlite driver so the binary needs no system SQLite.
package storage

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/doldoldol21/netscope/pkg/types"
	_ "modernc.org/sqlite"
)

// Store is a handle to the netscope database.
type Store struct {
	db   *sql.DB // single-connection writer (flushes, maintenance)
	rdb  *sql.DB // read-only pool for history queries; == db for :memory:
	path string  // db file path, for on-disk size accounting
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

-- Per-interface daily byte totals, for metered/tethering data tracking. Kept at
-- day granularity (one row per interface per local day) so a billing cycle's
-- usage is a cheap SUM and the table stays tiny (~31 rows/interface/month).
CREATE TABLE IF NOT EXISTS iface_usage (
	iface TEXT    NOT NULL,
	day   INTEGER NOT NULL,           -- unix seconds at local midnight
	rx    INTEGER NOT NULL DEFAULT 0,
	tx    INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (iface, day)
);

-- Daily rollups of the two sample tables. The ranked today/week/month lists
-- used to aggregate raw 10s buckets — a week meant grouping ~800k rows by a
-- text column, which took ~600ms and grew with retention. The same query over
-- these (~30k rows for a month of history) runs in ~15ms, and they cost only a
-- few MB because one row covers a whole day.
--
-- day is unix seconds at LOCAL midnight, matching iface_usage and the ranges
-- the API computes (today = since local midnight).
CREATE TABLE IF NOT EXISTS app_daily (
	day   INTEGER NOT NULL,
	app   TEXT    NOT NULL,
	path  TEXT    NOT NULL DEFAULT '',
	rx    INTEGER NOT NULL DEFAULT 0,
	tx    INTEGER NOT NULL DEFAULT 0,
	conns INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (day, app)
);

CREATE TABLE IF NOT EXISTS domain_daily (
	day      INTEGER NOT NULL,
	domain   TEXT    NOT NULL,
	app      TEXT    NOT NULL DEFAULT '',
	rx       INTEGER NOT NULL DEFAULT 0,
	tx       INTEGER NOT NULL DEFAULT 0,
	category TEXT    NOT NULL DEFAULT '',
	country  TEXT    NOT NULL DEFAULT '',
	PRIMARY KEY (day, domain, app)
);
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
	// Backfill the rollups from existing samples the first time they appear, so
	// history recorded before this version still answers today/week/month.
	if err := backfillDaily(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("backfill rollups: %w", err)
	}
	// Cap the WAL's auto-checkpoint so it can't balloon between maintenance runs.
	_, _ = db.Exec(`PRAGMA wal_autocheckpoint=1000`) // ~4MB of pages

	st := &Store{db: db, rdb: db, path: path}
	// Separate read-only pool for history queries. With one shared connection,
	// dashboard reads queued behind the engine's 10s flushes and any VACUUM —
	// under WAL, readers don't block the writer (and vice versa), so give reads
	// their own connections. :memory: can't be opened twice (a second connection
	// would see a different empty DB), so tests fall back to the write handle.
	if path != ":memory:" && !strings.Contains(path, "mode=memory") {
		rdsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=query_only(1)", path)
		if rdb, err := sql.Open("sqlite", rdsn); err == nil {
			rdb.SetMaxOpenConns(3)
			st.rdb = rdb
		}
	}
	return st, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	if s.rdb != nil && s.rdb != s.db {
		_ = s.rdb.Close()
	}
	return s.db.Close()
}

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
			-- A flush whose resolver came up empty must not erase a known path.
			path  = CASE WHEN excluded.path != '' THEN excluded.path ELSE path END`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	// Same rows folded into the day rollup, in the same transaction so the two
	// can't drift. day is local midnight of the bucket's day.
	roll, err := tx.Prepare(`
		INSERT INTO app_daily (day, app, path, rx, tx, conns)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(day, app) DO UPDATE SET
			rx    = rx + excluded.rx,
			tx    = tx + excluded.tx,
			conns = MAX(conns, excluded.conns),
			-- A flush whose resolver came up empty must not erase a known path.
			path  = CASE WHEN excluded.path != '' THEN excluded.path ELSE path END`)
	if err != nil {
		return err
	}
	defer roll.Close()
	day := localMidnight(bucket)
	for _, a := range apps {
		if _, err := stmt.Exec(bucket, a.Name, a.Path, a.RxBytes, a.TxBytes, a.Connections); err != nil {
			return err
		}
		if _, err := roll.Exec(day, a.Name, a.Path, a.RxBytes, a.TxBytes, a.Connections); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Day boundaries come from Go's calendar and nowhere else. The rollups were
// first written with a parallel SQL expression for "local midnight", and the
// two disagreed twice: once by the whole UTC offset (a formatting bug that
// shipped in v0.19.0), and again in zones whose DST jump happens AT midnight,
// where 00:00 doesn't exist on the transition day. Arithmetic on 86400 has the
// same problem — DST days are 23 or 25 hours long. So every day key and every
// day boundary below is computed with dayStart/nextDay.

// repairRollupKeys wipes the rollups when any stored key isn't a real local
// midnight, so the backfill rebuilds them. That covers databases written by
// v0.19.0 and machines whose timezone has changed since the rows were written
// (their old keys are no longer midnights, and would otherwise sit alongside
// correctly-keyed rows and be counted twice).
func repairRollupKeys(db *sql.DB) error {
	for _, t := range []string{"app_daily", "domain_daily"} {
		rows, err := db.Query(`SELECT DISTINCT day FROM ` + t)
		if err != nil {
			return err
		}
		bad := 0
		for rows.Next() {
			var day int64
			if err := rows.Scan(&day); err != nil {
				rows.Close()
				return err
			}
			if day != dayStart(day) {
				bad++
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if bad == 0 {
			continue
		}
		log.Printf("storage: rebuilding daily rollups (%d day keys in %s are not local midnights)", bad, t)
		for _, w := range []string{"app_daily", "domain_daily"} {
			if _, err := db.Exec(`DELETE FROM ` + w); err != nil {
				return err
			}
		}
		return nil
	}
	return nil
}

// backfillDaily fills the rollup tables from the raw samples when they're empty
// (a database written before rollups existed, or one just repaired above). It
// runs once: afterwards the flush path keeps them current.
func backfillDaily(db *sql.DB) error {
	if err := repairRollupKeys(db); err != nil {
		return err
	}
	for _, m := range []struct{ daily, samples, cols, group string }{
		{"app_daily", "app_samples",
			"day, app, path, rx, tx, conns",
			"day, app"},
		{"domain_daily", "domain_samples",
			"day, domain, app, rx, tx, category, country",
			"day, domain, app"},
	} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + m.daily).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue // already rolled up
		}
		// Rebuild a day at a time with boundaries computed in Go, so the keys are
		// identical to the ones the flush path writes even on DST days.
		var lo, hi sql.NullInt64
		if err := db.QueryRow(`SELECT MIN(bucket), MAX(bucket) FROM `+m.samples).Scan(&lo, &hi); err != nil {
			return err
		}
		if !lo.Valid {
			continue // no samples to roll up
		}
		for day := dayStart(lo.Int64); day <= hi.Int64; day = nextDay(day) {
			if err := rebuildDay(db, m.daily, m.cols, day); err != nil {
				return err
			}
		}
	}
	return nil
}

// rebuildDay recomputes one rollup day from the samples that fall inside it.
// Both bounds are Go-computed, so a 23- or 25-hour DST day is covered exactly.
func rebuildDay(db *sql.DB, daily, cols string, day int64) error {
	end := nextDay(day)
	// One transaction: the engine flushes every few seconds on the same handle,
	// and a flush landing between the delete and the insert would either be
	// erased or collide with the rebuilt row's primary key. The upsert covers
	// the same race from the other side.
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM `+daily+` WHERE day = ?`, day); err != nil {
		return err
	}
	var q string
	if daily == "app_daily" {
		q = `INSERT INTO app_daily (day, app, path, rx, tx, conns)
			SELECT ?, app, MAX(path), SUM(rx), SUM(tx), MAX(conns) FROM app_samples
			WHERE bucket >= ? AND bucket < ? GROUP BY app
			ON CONFLICT(day, app) DO UPDATE SET
				rx = excluded.rx, tx = excluded.tx, conns = excluded.conns, path = excluded.path`
	} else {
		q = `INSERT INTO domain_daily (day, domain, app, rx, tx, category, country)
			SELECT ?, domain, app, SUM(rx), SUM(tx), MAX(category), MAX(country) FROM domain_samples
			WHERE bucket >= ? AND bucket < ? GROUP BY domain, app
			ON CONFLICT(day, domain, app) DO UPDATE SET
				rx = excluded.rx, tx = excluded.tx,
				category = excluded.category, country = excluded.country`
	}
	if _, err := tx.Exec(q, day, day, end); err != nil {
		return err
	}
	return tx.Commit()
}

// VerifyRollups re-checks the last `days` local days: for each one it compares
// the rollup's totals against the raw samples and rebuilds any day that
// disagrees. The rollup is what the dashboard's ranked lists are read from, so
// a divergence would show wrong numbers with nothing to notice it — this makes
// the database self-correcting instead of trusting that every write path stays
// in step forever. Bounded to a few days so it stays cheap enough to run from
// the hourly maintenance pass. Reports whether anything was repaired.
func (s *Store) VerifyRollups(days int) (bool, error) {
	if days <= 0 {
		days = 3
	}
	repaired := false
	for _, m := range []struct{ daily, samples, cols string }{
		{"app_daily", "app_samples", "day, app, path, rx, tx, conns"},
		{"domain_daily", "domain_samples", "day, domain, app, rx, tx, category, country"},
	} {
		// Orphans first: rollup days with no samples left behind them (retention
		// or the size cap dropped the samples). They are invisible to a
		// samples-driven comparison, and would keep inflating the ranked lists
		// while the chart — which reads the samples — showed the truth.
		var oldest sql.NullInt64
		if err := s.db.QueryRow(`SELECT MIN(bucket) FROM ` + m.samples).Scan(&oldest); err != nil {
			return repaired, err
		}
		cut := int64(0)
		if oldest.Valid {
			cut = dayStart(oldest.Int64)
		}
		res, err := s.db.Exec(`DELETE FROM `+m.daily+` WHERE day < ?`, cut)
		if err != nil {
			return repaired, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			log.Printf("storage: dropped %d orphaned %s rows (their samples are gone)", n, m.daily)
			repaired = true
		}

		// Then the recent days, one at a time, against the samples they came from.
		day := dayStart(nowFn().Unix())
		for i := 0; i < days; i++ {
			end := nextDay(day)
			var srx, stx, rrx, rtx uint64
			if err := s.db.QueryRow(`SELECT IFNULL(SUM(rx),0), IFNULL(SUM(tx),0) FROM `+m.samples+
				` WHERE bucket >= ? AND bucket < ?`, day, end).Scan(&srx, &stx); err != nil {
				return repaired, err
			}
			if err := s.db.QueryRow(`SELECT IFNULL(SUM(rx),0), IFNULL(SUM(tx),0) FROM `+m.daily+
				` WHERE day = ?`, day).Scan(&rrx, &rtx); err != nil {
				return repaired, err
			}
			if srx != rrx || stx != rtx {
				log.Printf("storage: %s day %s out of step (rollup %d/%d, samples %d/%d) — rebuilding",
					m.daily, time.Unix(day, 0).Format("2006-01-02"), rrx, rtx, srx, stx)
				if err := rebuildDay(s.db, m.daily, m.cols, day); err != nil {
					return repaired, err
				}
				repaired = true
			}
			day = prevDay(day)
		}
	}
	return repaired, nil
}

// dayStart floors a unix timestamp to local midnight — the day key shared by
// iface_usage and the rollup tables (and by the API's "today" range).
func dayStart(unix int64) int64 {
	t := time.Unix(unix, 0)
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location()).Unix()
}

// nextDay / prevDay step by calendar days, not by 86400 seconds: DST days are
// 23 or 25 hours long, and in a few zones the transition happens at midnight,
// so the local day can start at 01:00 or 23:00 the evening before.
func nextDay(unix int64) int64 { return addDays(unix, 1) }
func prevDay(unix int64) int64 { return addDays(unix, -1) }

func addDays(unix int64, n int) int64 {
	t := time.Unix(unix, 0)
	y, m, d := t.Date()
	return time.Date(y, m, d+n, 0, 0, 0, 0, t.Location()).Unix()
}

// localMidnight is retained for callers outside the rollup path.
func localMidnight(unix int64) int64 { return dayStart(unix) }

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
			tx = tx + excluded.tx,
			-- Category/country are often learned a flush or two after the first
			-- packet. Keep them once known and never overwrite with a blank —
			-- the rollup already did this, so without it the same domain showed
			-- a country on week ranges and none on hour ranges.
			category = CASE WHEN excluded.category != '' THEN excluded.category ELSE category END,
			country  = CASE WHEN excluded.country  != '' THEN excluded.country  ELSE country  END`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	roll, err := tx.Prepare(`
		INSERT INTO domain_daily (day, domain, app, rx, tx, category, country)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(day, domain, app) DO UPDATE SET
			rx       = rx + excluded.rx,
			tx       = tx + excluded.tx,
			category = CASE WHEN excluded.category != '' THEN excluded.category ELSE category END,
			country  = CASE WHEN excluded.country  != '' THEN excluded.country  ELSE country  END`)
	if err != nil {
		return err
	}
	defer roll.Close()
	day := localMidnight(bucket)
	for _, d := range domains {
		if _, err := stmt.Exec(bucket, d.Domain, d.AppName, d.RxBytes, d.TxBytes, d.Category, d.Country); err != nil {
			return err
		}
		if _, err := roll.Exec(day, d.Domain, d.AppName, d.RxBytes, d.TxBytes, d.Category, d.Country); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AddIfaceUsage folds rx/tx bytes into the given interface's daily total. day is
// unix seconds at local midnight. A no-op when iface is empty or both are zero.
func (s *Store) AddIfaceUsage(iface string, day int64, rx, tx uint64) error {
	if iface == "" || (rx == 0 && tx == 0) {
		return nil
	}
	_, err := s.db.Exec(`
		INSERT INTO iface_usage (iface, day, rx, tx) VALUES (?, ?, ?, ?)
		ON CONFLICT(iface, day) DO UPDATE SET rx = rx + excluded.rx, tx = tx + excluded.tx`,
		iface, day, rx, tx)
	return err
}

// IfaceUsage is one interface's total rx/tx over a queried range.
type IfaceUsage struct {
	Iface string
	Rx    uint64
	Tx    uint64
}

// IfaceUsageAllSince returns per-interface totals for every interface with usage
// on or after sinceDay (unix seconds at local midnight), most-used first.
func (s *Store) IfaceUsageAllSince(sinceDay int64) ([]IfaceUsage, error) {
	rows, err := s.rdb.Query(`
		SELECT iface, SUM(rx), SUM(tx) FROM iface_usage
		WHERE day >= ? GROUP BY iface ORDER BY SUM(rx)+SUM(tx) DESC`, sinceDay)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IfaceUsage
	for rows.Next() {
		var u IfaceUsage
		if err := rows.Scan(&u.Iface, &u.Rx, &u.Tx); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// splitRange divides [since, until) into the whole local days the rollup can
// answer, [dayFrom, dayTo), and the partial edges the raw samples must cover.
// Ranges are rarely day-aligned in practice — "week" runs from this moment
// seven days ago to now — so serving only aligned ranges from the rollup would
// almost never fire. When there isn't a whole day inside, everything collapses
// onto the sample path (dayFrom == dayTo == since leaves both edge windows
// covering the full range).
func (s *Store) splitRange(since, until time.Time) (dayFrom, dayTo int64) {
	from, to := since.Unix(), until.Unix()
	dayFrom = dayStart(from)
	if dayFrom < from { // partial first day: the rollup starts at the next midnight
		dayFrom = nextDay(from)
	}
	dayTo = dayStart(to)
	// The last, partial day can come from the rollup too — but only when the
	// range truly reaches the end of the data. The rollup row holds the whole
	// day, so using it for a range that stops earlier would count bytes
	// recorded after `until`. Ask the data, not the clock: if no sample exists
	// at or after `until`, the row and the window contain exactly the same
	// bytes. (Guessing with a grace period got this wrong; see the audit test.)
	if s.noSamplesAtOrAfter(to) {
		dayTo = nextDay(to)
	}
	if dayTo <= dayFrom {
		return from, from
	}
	return dayFrom, dayTo
}

// noSamplesAtOrAfter reports whether the sample tables are empty from ts
// onwards — an indexed existence probe, not a scan.
func (s *Store) noSamplesAtOrAfter(ts int64) bool {
	var one int
	err := s.rdb.QueryRow(`
		SELECT 1 WHERE EXISTS (SELECT 1 FROM app_samples    WHERE bucket >= ?1)
		            OR EXISTS (SELECT 1 FROM domain_samples WHERE bucket >= ?1)`, ts).Scan(&one)
	return err == sql.ErrNoRows
}

// nowFn is time.Now, overridable in tests.
var nowFn = time.Now

// Apps returns per-app totals over [since, until), ranked by total bytes.
// Whole days come from the daily rollup and only the partial edges touch the
// raw samples: aggregating a week of 10s buckets took ~600ms and grew with
// retention, while the rollup answers the same question in ~15ms.
func (s *Store) Apps(since, until time.Time) ([]types.AppTraffic, error) {
	dayFrom, dayTo := s.splitRange(since, until)
	rows, err := s.rdb.Query(`
		SELECT app, MAX(path), SUM(rx), SUM(tx), MAX(conns) FROM (
			SELECT app, path, rx, tx, conns FROM app_daily
				WHERE day >= ?1 AND day < ?2
			UNION ALL
			SELECT app, path, rx, tx, conns FROM app_samples
				WHERE bucket >= ?3 AND bucket < ?1
			UNION ALL
			SELECT app, path, rx, tx, conns FROM app_samples
				WHERE bucket >= ?2 AND bucket < ?4
		)
		GROUP BY app
		ORDER BY SUM(rx) + SUM(tx) DESC`,
		dayFrom, dayTo, since.Unix(), until.Unix())
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
// Whole days come from the daily rollup (see Apps).
func (s *Store) Domains(since, until time.Time) ([]types.DomainStat, error) {
	dayFrom, dayTo := s.splitRange(since, until)
	rows, err := s.rdb.Query(`
		SELECT domain, MAX(app), SUM(rx), SUM(tx), MAX(category), MAX(country) FROM (
			SELECT domain, app, rx, tx, category, country FROM domain_daily
				WHERE day >= ?1 AND day < ?2
			UNION ALL
			SELECT domain, app, rx, tx, category, country FROM domain_samples
				WHERE bucket >= ?3 AND bucket < ?1
			UNION ALL
			SELECT domain, app, rx, tx, category, country FROM domain_samples
				WHERE bucket >= ?2 AND bucket < ?4
		)
		GROUP BY domain
		ORDER BY SUM(rx) + SUM(tx) DESC`,
		dayFrom, dayTo, since.Unix(), until.Unix())
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
	dayFrom, dayTo := s.splitRange(since, until)
	rows, err := s.rdb.Query(`
		SELECT domain, MAX(app), SUM(rx), SUM(tx), MAX(category), MAX(country) FROM (
			SELECT domain, app, rx, tx, category, country FROM domain_daily
				WHERE day >= ?1 AND day < ?2 AND app = ?5
			UNION ALL
			SELECT domain, app, rx, tx, category, country FROM domain_samples
				WHERE bucket >= ?3 AND bucket < ?1 AND app = ?5
			UNION ALL
			SELECT domain, app, rx, tx, category, country FROM domain_samples
				WHERE bucket >= ?2 AND bucket < ?4 AND app = ?5
		)
		GROUP BY domain
		ORDER BY SUM(rx) + SUM(tx) DESC`,
		dayFrom, dayTo, since.Unix(), until.Unix(), app)
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
	rows, err := s.rdb.Query(`
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
	rows, err := s.rdb.Query(`
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
	// Rollups follow the samples exactly. Days entirely before the cutoff go;
	// the day the cutoff lands inside kept some samples and lost others, so it
	// is recomputed rather than left holding the deleted bytes.
	day := dayStart(cut)
	for _, t := range []struct{ daily, cols string }{
		{"app_daily", "day, app, path, rx, tx, conns"},
		{"domain_daily", "day, domain, app, rx, tx, category, country"},
	} {
		if _, err := s.db.Exec(`DELETE FROM `+t.daily+` WHERE day < ?`, day); err != nil {
			return false, err
		}
		if cut > day { // partial day at the boundary
			if err := rebuildDay(s.db, t.daily, t.cols, day); err != nil {
				return false, err
			}
		}
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

// VacuumIfWorthwhile vacuums only when enough of the file is dead space to
// justify rewriting it. The hourly retention purge always frees *something*,
// and unconditionally vacuuming after it rewrote the whole database every
// hour — steady IO/battery burn for a few reclaimed KB. Threshold: free pages
// exceed 20% of the file and at least ~8MB.
func (s *Store) VacuumIfWorthwhile() error {
	var freelist, pages, pageSize int64
	if err := s.db.QueryRow(`PRAGMA freelist_count`).Scan(&freelist); err != nil {
		return err
	}
	if err := s.db.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		return err
	}
	if err := s.db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		return err
	}
	const minFreeBytes = 8 << 20
	if pages == 0 || freelist*pageSize < minFreeBytes || freelist*5 < pages {
		return nil
	}
	return s.Vacuum()
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
		// Drop the oldest *whole local day*, so samples and rollups always cut at
		// the same boundary. (Adding 86400 seconds cut mid-day on DST days and
		// stranded that day's rollup row, which then inflated the ranked lists
		// while the chart, read from the samples, showed the truth.)
		cut := nextDay(oldest.Int64)
		if _, err := s.db.Exec(`DELETE FROM app_samples WHERE bucket < ?`, cut); err != nil {
			return deleted, err
		}
		if _, err := s.db.Exec(`DELETE FROM domain_samples WHERE bucket < ?`, cut); err != nil {
			return deleted, err
		}
		if _, err := s.db.Exec(`DELETE FROM app_daily WHERE day < ?`, cut); err != nil {
			return deleted, err
		}
		if _, err := s.db.Exec(`DELETE FROM domain_daily WHERE day < ?`, cut); err != nil {
			return deleted, err
		}
		deleted = true
		s.Checkpoint()
		_ = s.Vacuum() // reclaim so the next SizeOnDisk reflects the deletion
	}
	return deleted, nil
}
