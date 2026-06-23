//go:build darwin

package main

/*
#cgo LDFLAGS: -framework Cocoa
#include <stdlib.h>
extern const void *appIconPNG(const char *path, const char *name, int px, int *outLen);
*/
import "C"

import "unsafe"

// appIcon returns a PNG of the macOS icon for the app at execPath (or, if that's
// empty, the application named name), rendered at px square. Returns nil when no
// icon can be resolved.
func appIcon(execPath, name string, px int) []byte {
	cp := C.CString(execPath)
	cn := C.CString(name)
	defer C.free(unsafe.Pointer(cp))
	defer C.free(unsafe.Pointer(cn))
	var n C.int
	ptr := C.appIconPNG(cp, cn, C.int(px), &n)
	if ptr == nil || n <= 0 {
		return nil
	}
	defer C.free(unsafe.Pointer(ptr))
	return C.GoBytes(ptr, n)
}
