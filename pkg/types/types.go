// Package types holds the data structures shared between the capture daemon,
// the aggregation/storage layer and the HTTP API. Keeping them in one place
// (instead of inside internal/) lets external tooling and tests import them.
package types

import "time"

// Protocol identifies the L4 protocol a flow uses.
type Protocol string

const (
	ProtoTCP Protocol = "tcp"
	ProtoUDP Protocol = "udp"
)

// Direction describes whether bytes were sent or received by the local host.
type Direction string

const (
	// DirOut is traffic originating from this machine (upload / tx).
	DirOut Direction = "out"
	// DirIn is traffic destined for this machine (download / rx).
	DirIn Direction = "in"
)

// Flow is a single observed packet reduced to the fields netscope cares about.
// It is the unit emitted by the capture layer and consumed by the engine.
type Flow struct {
	Timestamp time.Time
	Proto     Protocol
	Direction Direction
	// LocalPort/RemotePort/RemoteIP identify the connection the packet belongs
	// to, normalised so that "local" is always this host regardless of the
	// packet direction. They form the key used to resolve the owning process.
	LocalPort  uint16
	RemoteIP   string
	RemotePort uint16
	// Bytes is the IP payload length (L3) of the packet.
	Bytes uint64
}

// ConnKey is the normalised identity of a connection, used to look up the
// owning process and to attribute DNS-resolved domains.
type ConnKey struct {
	Proto      Protocol
	LocalPort  uint16
	RemoteIP   string
	RemotePort uint16
}

// Process identifies the application a flow is attributed to.
type Process struct {
	PID  int    `json:"pid"`
	Name string `json:"name"` // executable / bundle display name
	Path string `json:"path"` // absolute executable path
}

// AppTraffic is the per-application aggregate returned by the API.
type AppTraffic struct {
	PID         int    `json:"pid"`
	Name        string `json:"name"`
	Path        string `json:"path"`
	RxBytes     uint64 `json:"rxBytes"`
	TxBytes     uint64 `json:"txBytes"`
	Connections int    `json:"connections"`
}

// Total returns rx+tx, convenient for ranking.
func (a AppTraffic) Total() uint64 { return a.RxBytes + a.TxBytes }

// DomainStat is the per-domain aggregate returned by the API. Category is a
// neutral grouping (cloud/cdn/media/social/ai/…), used for display only.
type DomainStat struct {
	Domain   string `json:"domain"`
	AppName  string `json:"appName"`
	RxBytes  uint64 `json:"rxBytes"`
	TxBytes  uint64 `json:"txBytes"`
	Category string `json:"category,omitempty"`
	Country  string `json:"country,omitempty"` // ISO alpha-2 (GeoIP of the remote IP)
}

// Total returns rx+tx.
func (d DomainStat) Total() uint64 { return d.RxBytes + d.TxBytes }

// NetIface describes a capturable network interface for the settings UI.
type NetIface struct {
	Name    string `json:"name"`    // e.g. "en0"
	Display string `json:"display"` // e.g. "en0 (192.168.0.5)"
	Up      bool   `json:"up"`
	Active  bool   `json:"active"` // currently being captured
}

// RatePoint is one per-second throughput sample (for seeding the live chart).
type RatePoint struct {
	Time     time.Time `json:"time"`
	RxPerSec uint64    `json:"rxPerSec"`
	TxPerSec uint64    `json:"txPerSec"`
}

// TimePoint is one bucket of a time-series response.
type TimePoint struct {
	Time    time.Time `json:"time"`
	RxBytes uint64    `json:"rxBytes"`
	TxBytes uint64    `json:"txBytes"`
}

// Snapshot is a point-in-time view pushed to live dashboard clients.
//
// Apps/Domains are SESSION-CUMULATIVE: they accumulate since the daemon started
// and never reset on a storage flush, so the dashboard tables stay stable
// instead of emptying every flush interval. RxPerSec/TxPerSec are the
// instantaneous rates (expected to fluctuate).
type Snapshot struct {
	Time         time.Time    `json:"time"`
	SessionStart time.Time    `json:"sessionStart"`
	Apps         []AppTraffic `json:"apps"`
	Domains      []DomainStat `json:"domains"`
	TotalRx      uint64       `json:"totalRx"`
	TotalTx      uint64       `json:"totalTx"`
	RxPerSec     uint64       `json:"rxPerSec"`
	TxPerSec     uint64       `json:"txPerSec"`
	// ActiveApps is how many apps sent/received within the recent activity
	// window (not the cumulative count).
	ActiveApps int    `json:"activeApps"`
	Interface  string `json:"interface"`
	// Paused is true while live capture is suspended by the user.
	Paused bool `json:"paused"`
}

// Connection is one live network connection (an app talking to a remote
// endpoint), surfaced by the "Live connections" view. Distinct local ports to
// the same remote endpoint collapse into one row.
type Connection struct {
	Proto      Protocol  `json:"proto"`
	App        string    `json:"app"`
	Path       string    `json:"path"`
	Host       string    `json:"host"` // hostname if known, else the remote IP
	RemoteIP   string    `json:"remoteIP"`
	RemotePort uint16    `json:"remotePort"`
	Country    string    `json:"country"`
	Category   string    `json:"category"`
	RxBytes    uint64    `json:"rxBytes"`
	TxBytes    uint64    `json:"txBytes"`
	FirstSeen  time.Time `json:"firstSeen"`
	LastSeen   time.Time `json:"lastSeen"`
}

// Total is the connection's combined throughput so far.
func (c Connection) Total() uint64 { return c.RxBytes + c.TxBytes }
