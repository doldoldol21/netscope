//go:build linux

package capture

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/doldoldol21/netscope/internal/dnscache"
	"github.com/doldoldol21/netscope/pkg/types"
	"github.com/google/gopacket"
	"github.com/google/gopacket/pcap"
)

const snapLen = 2048

// Source wraps a pcap handle and decoder, yielding attributed flows.
type Source struct {
	handle *pcap.Handle
	src    *gopacket.PacketSource
	dec    *Decoder
	name   string
}

// OpenLive starts live capture on iface (empty = auto-detect) and returns a
// Source that reads packets via libpcap.
func OpenLive(iface string, dns *dnscache.Cache) (*Source, error) {
	if iface == "" {
		var err error
		iface, err = defaultInterface()
		if err != nil {
			return nil, err
		}
	}
	handle, err := pcap.OpenLive(iface, snapLen, false, 100*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("pcap open %q: %w (need root / CAP_NET_RAW?)", iface, err)
	}
	if err := handle.SetBPFFilter("ip or ip6"); err != nil {
		handle.Close()
		return nil, fmt.Errorf("set bpf filter: %w", err)
	}
	src := gopacket.NewPacketSource(handle, handle.LinkType())
	src.Lazy = true
	src.NoCopy = true
	return &Source{
		handle: handle,
		src:    src,
		dec:    NewDecoder(LocalIPs(), dns),
		name:   iface,
	}, nil
}

// OpenOffline replays a pcap file.
func OpenOffline(path string, localIPs []string, dns *dnscache.Cache) (*Source, error) {
	handle, err := pcap.OpenOffline(path)
	if err != nil {
		return nil, fmt.Errorf("pcap open offline %q: %w", path, err)
	}
	src := gopacket.NewPacketSource(handle, handle.LinkType())
	src.Lazy = true
	src.NoCopy = true
	return &Source{
		handle: handle,
		src:    src,
		dec:    NewDecoder(localIPs, dns),
		name:   path,
	}, nil
}

func (s *Source) Name() string { return s.name }

func (s *Source) Run(ctx context.Context, out chan<- types.Flow) error {
	defer s.handle.Close()
	packets := s.src.Packets()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case pkt, ok := <-packets:
			if !ok {
				return nil
			}
			flow, ok := s.dec.Decode(pkt)
			if !ok {
				continue
			}
			if flow.Timestamp.IsZero() {
				flow.Timestamp = time.Now()
			}
			select {
			case out <- flow:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// LocalIPs returns every unicast address configured on the host's interfaces.
func LocalIPs() []string {
	var ips []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ips
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			ips = append(ips, ipnet.IP.String())
		}
	}
	return ips
}

func defaultInterface() (string, error) {
	if conn, err := net.Dial("udp", "8.8.8.8:53"); err == nil {
		local := conn.LocalAddr().(*net.UDPAddr).IP
		conn.Close()
		if name := interfaceForIP(local); name != "" {
			return name, nil
		}
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.IsGlobalUnicast() {
				return ifi.Name, nil
			}
		}
	}
	return "", fmt.Errorf("no suitable network interface found")
}

func interfaceForIP(ip net.IP) string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifi := range ifaces {
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.Equal(ip) {
				return ifi.Name
			}
		}
	}
	return ""
}

// FriendlyName returns the interface name as-is on Linux (no SystemConfiguration).
func FriendlyName(bsd string) (string, bool) {
	// On Linux, try to read /sys/class/net/<iface>/ifalias for a friendly name.
	if alias, err := os.ReadFile("/sys/class/net/" + bsd + "/ifalias"); err == nil {
		name := strings.TrimSpace(string(alias))
		if name != "" {
			return name, true
		}
	}
	return bsd, false
}

// ---- simple polling-based supervisor for Linux ----

// LiveSupervisor keeps capture running on the active network interface,
// re-opening when the default route changes (e.g. Wi-Fi <-> Ethernet).
type LiveSupervisor struct {
	dns      *dnscache.Cache
	pref     string // user-pinned interface, "" = auto-detect
	prefPath string
	activeMu sync.RWMutex
	active   string
	onIface  func(string)
	resumeCh chan struct{}
}

func NewLiveSupervisor(iface string, dns *dnscache.Cache, prefPath string) *LiveSupervisor {
	ls := &LiveSupervisor{dns: dns, prefPath: prefPath, pref: iface, resumeCh: make(chan struct{}, 1)}
	ls.loadPref()
	return ls
}

func (ls *LiveSupervisor) SetOnInterface(fn func(string)) { ls.onIface = fn }

func (ls *LiveSupervisor) Name() string {
	ls.activeMu.RLock()
	defer ls.activeMu.RUnlock()
	return ls.active
}

func (ls *LiveSupervisor) setActive(name string) {
	ls.activeMu.Lock()
	old := ls.active
	ls.active = name
	ls.activeMu.Unlock()
	if name != old && ls.onIface != nil {
		ls.onIface(name)
	}
}

func (ls *LiveSupervisor) resolve() (string, error) {
	if ls.pref != "" {
		return ls.pref, nil
	}
	return defaultInterface()
}

func (ls *LiveSupervisor) Run(ctx context.Context, out chan<- types.Flow) error {
	// Resolve initial interface.
	start, err := ls.resolve()
	if err != nil {
		return err
	}
	ls.setActive(start)

	// Re-detect the active interface periodically, re-opening capture on change.
	for {
		iface := ls.current()
		src, err := OpenLive(iface, ls.dns)
		if err != nil {
			return fmt.Errorf("open live %q: %w", iface, err)
		}

		runCtx, cancel := context.WithCancel(ctx)
		runErr := make(chan error, 1)
		go func() { runErr <- src.Run(runCtx, out) }()

		// Poll for interface changes every 5 seconds.
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

	loop:
		for {
			select {
			case <-ctx.Done():
				cancel()
				<-runErr
				return ctx.Err()
			case err := <-runErr:
				cancel()
				return err
			case <-ls.resumeCh:
				// User changed interface preference.
			case <-ticker.C:
				next, err := ls.resolve()
				if err != nil || next == iface {
					continue
				}
				ls.setActive(next)
			}
			cancel()
			<-runErr
			break loop
		}
	}
}

func (ls *LiveSupervisor) current() string {
	ls.activeMu.RLock()
	defer ls.activeMu.RUnlock()
	if ls.pref != "" {
		return ls.pref
	}
	return ls.active
}

// -- api.Capturer implementation --

func (ls *LiveSupervisor) PreferredInterface() string { return ls.pref }
func (ls *LiveSupervisor) SetPreferredInterface(name string) error {
	ls.pref = name
	ls.savePref(name)
	ls.resume()
	return nil
}
func (ls *LiveSupervisor) Paused() bool     { return false }
func (ls *LiveSupervisor) SetPaused(p bool) {}

func (ls *LiveSupervisor) resume() {
	select {
	case ls.resumeCh <- struct{}{}:
	default:
	}
}

func (ls *LiveSupervisor) ListInterfaces() []types.NetIface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []types.NetIface
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		var display string
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.IsGlobalUnicast() {
				display = ipnet.IP.String()
				break
			}
		}
		friendly, _ := FriendlyName(ifi.Name)
		out = append(out, types.NetIface{
			Name:     ifi.Name,
			Display:  ifi.Name + " (" + display + ")",
			Friendly: friendly,
			Kind:     "ethernet",
			Up:       ifi.Flags&net.FlagUp != 0,
			Active:   ifi.Name == ls.current(),
		})
	}
	return out
}

func (ls *LiveSupervisor) loadPref() {
	if ls.prefPath == "" {
		return
	}
	b, err := os.ReadFile(ls.prefPath)
	if err != nil {
		return
	}
	ls.pref = strings.TrimSpace(string(b))
}

func (ls *LiveSupervisor) savePref(name string) {
	if ls.prefPath == "" {
		return
	}
	_ = os.WriteFile(ls.prefPath, []byte(name), 0644)
}
