package dnscache

import (
	"fmt"
	"testing"
	"time"
)

func BenchmarkPut(b *testing.B) {
	c := New(time.Hour, 100000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ip := fmt.Sprintf("10.0.%d.%d", (i>>8)&0xff, i&0xff)
		c.Put(ip, fmt.Sprintf("host-%d.example.com", i))
	}
}

func BenchmarkLookup(b *testing.B) {
	c := New(time.Hour, 100000)
	// Pre-populate so every lookup is a hit.
	for i := 0; i < 10000; i++ {
		ip := fmt.Sprintf("10.0.%d.%d", (i>>8)&0xff, i&0xff)
		c.Put(ip, fmt.Sprintf("host-%d.example.com", i))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx := i % 10000
		ip := fmt.Sprintf("10.0.%d.%d", (idx>>8)&0xff, idx&0xff)
		_ = c.Lookup(ip)
	}
}

func BenchmarkLookupMiss(b *testing.B) {
	c := New(time.Hour, 100000)
	for i := 0; i < 1000; i++ {
		ip := fmt.Sprintf("10.0.%d.%d", (i>>8)&0xff, i&0xff)
		c.Put(ip, fmt.Sprintf("host-%d.example.com", i))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Lookup("192.168.1.1") // never inserted
	}
}

func BenchmarkSaveLoad(b *testing.B) {
	c := New(time.Hour, 100000)
	for i := 0; i < 10000; i++ {
		ip := fmt.Sprintf("10.0.%d.%d", (i>>8)&0xff, i&0xff)
		c.Put(ip, fmt.Sprintf("host-%d.example.com", i))
	}
	path := b.TempDir() + "/dnscache.json"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.SaveTo(path)
		_ = c.LoadFrom(path)
	}
}
