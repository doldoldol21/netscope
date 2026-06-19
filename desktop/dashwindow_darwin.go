//go:build darwin

package main

/*
#cgo LDFLAGS: -framework WebKit
#include <stdlib.h>
extern void openDashWindow(const char *url);
extern void closeDashWindow(void);
*/
import "C"

import "unsafe"

// openDashWindow opens (or re-focuses) the standalone dashboard window pointed at
// the given loopback URL.
func openDashWindow(url string) {
	c := C.CString(url)
	defer C.free(unsafe.Pointer(c))
	C.openDashWindow(c)
}

// closeDashWindow hides the standalone dashboard window.
func closeDashWindow() { C.closeDashWindow() }
