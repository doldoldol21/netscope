// Package geoip maps an IP address to its ISO 3166-1 alpha-2 country code using
// a compact database embedded in the binary. It is fully offline — no external
// lookups — so netscope never reveals which IPs you contact to a third party.
//
// Data: DB-IP IP-to-Country Lite (https://db-ip.com), licensed CC BY 4.0.
// Refresh with scripts/gen-geoip.sh.
package geoip

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/binary"
	"io"
	"net"
	"sort"
	"sync"
)

//go:embed data/v4.bin.gz
var v4gz []byte

//go:embed data/v6.bin.gz
var v6gz []byte

//go:embed data/countries.txt
var countriesRaw string

var (
	once      sync.Once
	countries []string // index -> "US", "KR", … ("ZZ" = unknown)
	v4start   []uint32
	v4idx     []uint8
	v6start   []uint64 // first 64 bits of the range start (country granularity)
	v6idx     []uint8
)

func load() {
	for i := 0; i+2 <= len(countriesRaw); i += 2 {
		countries = append(countries, countriesRaw[i:i+2])
	}
	// v4: 5-byte entries (uint32 start, uint8 country index), sorted by start.
	b4 := gunzip(v4gz)
	n4 := len(b4) / 5
	v4start = make([]uint32, n4)
	v4idx = make([]uint8, n4)
	for i := 0; i < n4; i++ {
		o := i * 5
		v4start[i] = binary.BigEndian.Uint32(b4[o:])
		v4idx[i] = b4[o+4]
	}
	// v6: 9-byte entries (uint64 prefix, uint8 country index), sorted by prefix.
	b6 := gunzip(v6gz)
	n6 := len(b6) / 9
	v6start = make([]uint64, n6)
	v6idx = make([]uint8, n6)
	for i := 0; i < n6; i++ {
		o := i * 9
		v6start[i] = binary.BigEndian.Uint64(b6[o:])
		v6idx[i] = b6[o+8]
	}
}

func gunzip(b []byte) []byte {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil
	}
	defer r.Close()
	out, _ := io.ReadAll(r)
	return out
}

// Lookup returns the ISO alpha-2 country code for ip, or "" if unknown.
func Lookup(ip net.IP) string {
	if ip == nil {
		return ""
	}
	once.Do(load)
	var code string
	if v4 := ip.To4(); v4 != nil {
		key := binary.BigEndian.Uint32(v4)
		// rightmost entry with start <= key
		i := sort.Search(len(v4start), func(i int) bool { return v4start[i] > key }) - 1
		if i >= 0 {
			code = countries[v4idx[i]]
		}
	} else if len(ip) == net.IPv6len {
		key := binary.BigEndian.Uint64(ip[:8])
		i := sort.Search(len(v6start), func(i int) bool { return v6start[i] > key }) - 1
		if i >= 0 {
			code = countries[v6idx[i]]
		}
	}
	if code == "ZZ" { // DB-IP's placeholder for unallocated/unknown
		return ""
	}
	return code
}
