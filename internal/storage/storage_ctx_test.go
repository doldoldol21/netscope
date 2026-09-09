package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/doldoldol21/netscope/pkg/types"
)

// A cancelled request must stop the read, not finish it for nobody.
func TestReadsStopWhenTheContextIsCancelled(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	if err := s.FlushApps(now.Unix(), []types.AppTraffic{{Name: "Safari", RxBytes: 10}}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	since, until := now.Add(-time.Hour), now.Add(time.Hour)
	if _, err := s.Apps(ctx, since, until); !errors.Is(err, context.Canceled) {
		t.Errorf("Apps with a cancelled context = %v, want context.Canceled", err)
	}
	if _, err := s.Domains(ctx, since, until); !errors.Is(err, context.Canceled) {
		t.Errorf("Domains with a cancelled context = %v, want context.Canceled", err)
	}
	if _, err := s.TimeSeries(ctx, since, until, time.Minute); !errors.Is(err, context.Canceled) {
		t.Errorf("TimeSeries with a cancelled context = %v, want context.Canceled", err)
	}
	if _, err := s.IfaceUsageAllSince(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("IfaceUsageAllSince with a cancelled context = %v, want context.Canceled", err)
	}
	// And a live context still answers.
	apps, err := s.Apps(context.Background(), since, until)
	if err != nil || len(apps) != 1 {
		t.Fatalf("Apps with a live context = %v, %v", apps, err)
	}
}

func bucketIndexes(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'index' AND name IN ('idx_app_bucket', 'idx_domain_bucket')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	return names
}

// The bucket indexes duplicated each table's primary key. A fresh database
// must not create them, and a database from a version that did must lose them
// on open.
func TestOpenDropsTheRedundantBucketIndexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := bucketIndexes(t, s.db); len(got) != 0 {
		t.Fatalf("a fresh database has the redundant indexes: %v", got)
	}
	// Recreate what an older version left behind.
	for _, stmt := range []string{
		`CREATE INDEX IF NOT EXISTS idx_app_bucket ON app_samples(bucket)`,
		`CREATE INDEX IF NOT EXISTS idx_domain_bucket ON domain_samples(bucket)`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if got := bucketIndexes(t, s.db); len(got) != 2 {
		t.Fatalf("could not stage the legacy indexes: %v", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := bucketIndexes(t, s.db); len(got) != 0 {
		t.Fatalf("legacy indexes survived reopening: %v", got)
	}
}
