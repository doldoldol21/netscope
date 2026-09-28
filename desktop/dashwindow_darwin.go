//go:build darwin

package main

/*
#cgo LDFLAGS: -framework WebKit
#include <stdlib.h>
extern void openDashWindow(const char *url);
extern void closeDashWindow(void);
extern void dashEvalJS(const char *js);
extern void setDashAppearance(const char *mode);
*/
import "C"

import "unsafe"

// dashEvalJS evaluates JavaScript in the dashboard window's web view (no-op if it
// isn't open). Pushes live changes — e.g. a theme switch — without polling.
func dashEvalJS(js string) {
	c := C.CString(js)
	defer C.free(unsafe.Pointer(c))
	C.dashEvalJS(c)
}

// openDashWindow opens (or re-focuses) the standalone dashboard window pointed at
// the given loopback URL.
func openDashWindow(url string) {
	c := C.CString(url)
	defer C.free(unsafe.Pointer(c))
	C.openDashWindow(c)
}

// closeDashWindow hides the standalone dashboard window.
func closeDashWindow() { C.closeDashWindow() }

// setDashAppearance pins the dashboard window's appearance to the theme
// ("auto" follows the system), so its title bar and sidebar material match
// the page. Safe to call before the window exists (no-op).
func setDashAppearance(mode string) {
	cs := C.CString(mode)
	defer C.free(unsafe.Pointer(cs))
	C.setDashAppearance(cs)
}
