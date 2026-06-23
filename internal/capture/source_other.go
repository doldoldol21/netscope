//go:build !darwin

package capture

import (
	"context"
	"errors"

	"github.com/doldoldol21/netscope/internal/dnscache"
	"github.com/doldoldol21/netscope/pkg/types"
)

// errUnsupported is returned by the capture entry points on non-macOS builds.
// netscope's Phase 1 target is macOS; these stubs keep `go build ./...`
// working on other platforms (CI, editors).
var errUnsupported = errors.New("capture: live/offline pcap is only supported on darwin in phase 1")

// Source is a non-functional placeholder on unsupported platforms.
type Source struct{ name string }

func OpenLive(iface string, dns *dnscache.Cache) (*Source, error) { return nil, errUnsupported }

// NewLiveSupervisor returns a non-functional placeholder on unsupported
// platforms; its Run reports the unsupported error like the other stubs.
func NewLiveSupervisor(iface string, dns *dnscache.Cache, prefPath string) *Source {
	return &Source{name: iface}
}

func OpenOffline(path string, localIPs []string, dns *dnscache.Cache) (*Source, error) {
	return nil, errUnsupported
}

func (s *Source) Name() string { return s.name }

func (s *Source) Run(ctx context.Context, out chan<- types.Flow) error { return errUnsupported }

// Capturer stubs so the live source satisfies api.Capturer on all platforms.
func (s *Source) ListInterfaces() []types.NetIface        { return nil }
func (s *Source) PreferredInterface() string              { return "" }
func (s *Source) SetPreferredInterface(name string) error { return errUnsupported }

// LocalIPs returns no addresses on unsupported platforms.
func LocalIPs() []string { return nil }
