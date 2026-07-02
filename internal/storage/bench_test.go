package storage

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/doldoldol21/netscope/pkg/types"
)

func BenchmarkApps(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	// Insert sample data: 500 apps across 100 buckets.
	now := time.Now()
	for bucket := 0; bucket < 100; bucket++ {
		apps := make([]types.AppTraffic, 500)
		for j := range apps {
			apps[j] = types.AppTraffic{
				Name:        fmt.Sprintf("app-%d", j),
				Path:        fmt.Sprintf("/usr/bin/app-%d", j),
				RxBytes:     uint64(j * 1024),
				TxBytes:     uint64(j * 512),
				Connections: 1,
			}
		}
		ts := now.Add(-time.Duration(100-bucket) * time.Minute)
		if err := s.FlushApps(ts.Unix(), apps); err != nil {
			b.Fatal(err)
		}
	}

	since := now.Add(-2 * time.Hour)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.Apps(since, now)
	}
}

func BenchmarkDomains(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	now := time.Now()
	for bucket := 0; bucket < 50; bucket++ {
		domains := make([]types.DomainStat, 200)
		for j := range domains {
			domains[j] = types.DomainStat{
				Domain:   fmt.Sprintf("host-%d.example.com", j),
				AppName:  fmt.Sprintf("app-%d", j%20),
				RxBytes:  uint64(j * 2048),
				TxBytes:  uint64(j * 1024),
				Category: "cloud",
				Country:  "US",
			}
		}
		ts := now.Add(-time.Duration(50-bucket) * time.Minute)
		if err := s.FlushDomains(ts.Unix(), domains); err != nil {
			b.Fatal(err)
		}
	}

	since := now.Add(-2 * time.Hour)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.Domains(since, now)
	}
}

func BenchmarkFlushApps(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	apps := make([]types.AppTraffic, 200)
	for j := range apps {
		apps[j] = types.AppTraffic{
			Name:        fmt.Sprintf("app-%d", j),
			RxBytes:     uint64(j * 1024),
			TxBytes:     uint64(j * 512),
			Connections: 1,
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.FlushApps(time.Now().Unix(), apps)
	}
}

func BenchmarkTimeSeries(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	now := time.Now()
	for bucket := 0; bucket < 120; bucket++ {
		apps := make([]types.AppTraffic, 50)
		for j := range apps {
			apps[j] = types.AppTraffic{
				Name:    fmt.Sprintf("app-%d", j),
				RxBytes: uint64(j * 1024),
				TxBytes: uint64(j * 512),
			}
		}
		ts := now.Add(-time.Duration(120-bucket) * 10 * time.Second)
		if err := s.FlushApps(ts.Unix(), apps); err != nil {
			b.Fatal(err)
		}
	}

	since := now.Add(-20 * time.Minute)
	step := 10 * time.Second
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.TimeSeries(since, now, step)
	}
}
