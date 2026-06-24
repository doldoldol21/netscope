//go:build darwin

package capture

/*
#cgo LDFLAGS: -framework CoreFoundation -framework SystemConfiguration
#include <SystemConfiguration/SystemConfiguration.h>
#include <stdlib.h>

// copyInterfaceMeta returns the macOS-friendly metadata for every network
// interface as newline-separated "bsdName\tdisplayName\ttype" rows. The same
// names appear in System Settings ▸ Network ("Wi-Fi", "iPhone USB",
// "Thunderbolt Ethernet"), which is what lets a user tell a tethered phone from
// regular Wi-Fi. Caller frees the returned buffer.
static char *copyInterfaceMeta(void) {
  CFArrayRef arr = SCNetworkInterfaceCopyAll();
  CFMutableStringRef out = CFStringCreateMutable(NULL, 0);
  if (arr) {
    CFIndex n = CFArrayGetCount(arr);
    for (CFIndex i = 0; i < n; i++) {
      SCNetworkInterfaceRef ifc = (SCNetworkInterfaceRef)CFArrayGetValueAtIndex(arr, i);
      CFStringRef bsd = SCNetworkInterfaceGetBSDName(ifc);
      if (!bsd) continue;
      CFStringRef disp = SCNetworkInterfaceGetLocalizedDisplayName(ifc);
      CFStringRef type = SCNetworkInterfaceGetInterfaceType(ifc);
      CFStringAppend(out, bsd);
      CFStringAppendCString(out, "\t", kCFStringEncodingUTF8);
      if (disp) CFStringAppend(out, disp);
      CFStringAppendCString(out, "\t", kCFStringEncodingUTF8);
      if (type) CFStringAppend(out, type);
      CFStringAppendCString(out, "\n", kCFStringEncodingUTF8);
    }
    CFRelease(arr);
  }
  CFIndex len = CFStringGetLength(out);
  CFIndex max = CFStringGetMaximumSizeForEncoding(len, kCFStringEncodingUTF8) + 1;
  char *buf = (char *)malloc(max);
  if (buf) {
    if (!CFStringGetCString(out, buf, max, kCFStringEncodingUTF8)) buf[0] = '\0';
  }
  CFRelease(out);
  return buf;
}
*/
import "C"

import (
	"strings"
	"unsafe"
)

// ifaceMeta is the macOS-friendly metadata for one interface.
type ifaceMeta struct {
	friendly string // localized display name, e.g. "Wi-Fi", "iPhone USB"
	kind     string // "wifi" | "ethernet" | "tether" | "other"
	tether   bool
}

// interfaceMeta returns a map keyed by BSD name (en0, en5, …). On any failure it
// returns an empty map, so callers degrade to raw names.
func interfaceMeta() map[string]ifaceMeta {
	out := map[string]ifaceMeta{}
	cstr := C.copyInterfaceMeta()
	if cstr == nil {
		return out
	}
	defer C.free(unsafe.Pointer(cstr))
	for _, line := range strings.Split(C.GoString(cstr), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 || f[0] == "" {
			continue
		}
		bsd, disp, typ := f[0], f[1], f[2]
		out[bsd] = ifaceMeta{friendly: disp, kind: classifyIface(disp, typ), tether: looksTethered(disp, typ)}
	}
	return out
}

// classifyIface maps the macOS interface type/name to a coarse kind.
func classifyIface(disp, typ string) string {
	if looksTethered(disp, typ) {
		return "tether"
	}
	switch typ {
	case "IEEE80211":
		return "wifi"
	case "Ethernet":
		return "ethernet"
	default:
		return "other"
	}
}

// looksTethered guesses whether an interface is a tethered phone (USB or hotspot).
// macOS reports these as Ethernet-type but names them distinctively.
func looksTethered(disp, typ string) bool {
	d := strings.ToLower(disp)
	for _, kw := range []string{"iphone", "ipad", "android", "usb", "rndis", "tether", "personal hotspot"} {
		if strings.Contains(d, kw) {
			return true
		}
	}
	return typ == "Bluetooth PAN"
}
