package engine

import (
	"testing"
	"time"

	"github.com/doldoldol21/netscope/internal/dnscache"
	"github.com/doldoldol21/netscope/pkg/types"
)

func BenchmarkIngest(b *testing.B) {
	dns := dnscache.New(time.Hour, 10000)
	e := New(Config{}, nil, dns, nil)

	flow := types.Flow{
		Timestamp:  time.Now(),
		Proto:      types.ProtoTCP,
		Direction:  types.DirOut,
		LocalPort:  54321,
		RemoteIP:   "93.184.216.34",
		RemotePort: 443,
		Bytes:      1500,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.ingest(flow)
	}
}

func BenchmarkIngestWithDNS(b *testing.B) {
	dns := dnscache.New(time.Hour, 10000)
	for i := 0; i < 1000; i++ {
		dns.Put("10.0.0.1", "example.com")
	}
	e := New(Config{}, nil, dns, nil)

	flow := types.Flow{
		Timestamp:  time.Now(),
		Proto:      types.ProtoTCP,
		Direction:  types.DirOut,
		LocalPort:  54321,
		RemoteIP:   "10.0.0.1",
		RemotePort: 443,
		Bytes:      1500,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.ingest(flow)
	}
}

func BenchmarkSnapshot(b *testing.B) {
	dns := dnscache.New(time.Hour, 10000)
	e := New(Config{}, nil, dns, nil)

	// Pre-populate: 200 apps, each with traffic.
	for a := 0; a < 200; a++ {
		flow := types.Flow{
			Timestamp:  time.Now(),
			Proto:      types.ProtoTCP,
			Direction:  types.DirOut,
			LocalPort:  uint16(50000 + a),
			RemoteIP:   "93.184.216.34",
			RemotePort: 443,
			Bytes:      1500,
		}
		for i := 0; i < 50; i++ {
			e.ingest(flow)
		}
	}

	_ = e.Snapshot() // warm up
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.updateSnapshot()
		_ = e.Snapshot()
	}
}

func BenchmarkConnections(b *testing.B) {
	dns := dnscache.New(time.Hour, 10000)
	for i := 0; i < 200; i++ {
		dns.Put("10.0.0.1", "example.com")
	}
	e := New(Config{ActiveWindow: 30 * time.Second}, nil, dns, nil)

	// Pre-populate 200 distinct connections.
	for a := 0; a < 200; a++ {
		flow := types.Flow{
			Timestamp:  time.Now(),
			Proto:      types.ProtoTCP,
			Direction:  types.DirOut,
			LocalPort:  uint16(50000 + a),
			RemoteIP:   "93.184.216.34",
			RemotePort: uint16(80 + (a % 10)),
			Bytes:      1500,
		}
		e.ingest(flow)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = e.Connections(15 * time.Second)
	}
}
