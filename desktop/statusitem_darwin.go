//go:build darwin

package main

/*
#cgo LDFLAGS: -framework Cocoa
#include <stdlib.h>
extern void installStatusItem(const void *png, int len);
extern void setStatusImage(const void *png, int len);
extern void setStatusText(const char *utf8);
extern void positionPopover(int winW, int winH);
extern void enablePopoverDismiss(void);
extern void focusPopover(void);
*/
import "C"

import "unsafe"

// installStatusItem adds the menu-bar status item using the given template PNG.
func installStatusItem(png []byte) {
	if len(png) == 0 {
		return
	}
	C.installStatusItem(unsafe.Pointer(&png[0]), C.int(len(png)))
}

// setStatusImage swaps the menu-bar icon to the given template PNG (one frame).
func setStatusImage(png []byte) {
	if len(png) == 0 {
		return
	}
	C.setStatusImage(unsafe.Pointer(&png[0]), C.int(len(png)))
}

// positionPopover places the popover window flush under the status item, on the
// display the menu bar currently lives on (set directly in global coordinates,
// correct across multiple monitors).
func positionPopover(winWidth, winHeight int) {
	C.positionPopover(C.int(winWidth), C.int(winHeight))
}

// setStatusText sets the live-rate text shown next to the menu-bar icon ("" clears it).
func setStatusText(s string) {
	cs := C.CString(s)
	defer C.free(unsafe.Pointer(cs))
	C.setStatusText(cs)
}

// enablePopoverDismiss hides the popover when the user clicks away.
func enablePopoverDismiss() { C.enablePopoverDismiss() }

// focusPopover makes the popover the key window so it can dismiss on blur.
func focusPopover() { C.focusPopover() }

//export statusItemClickedGo
func statusItemClickedGo() {
	// Called on the Cocoa main thread. Wails runtime calls must NOT run on the
	// main thread (they would block the event loop), so hop to a goroutine.
	go onStatusItemClick()
}

//export popoverDidHideGo
func popoverDidHideGo() {
	winMu.Lock()
	winVisible = false
	ctx := appCtx
	winMu.Unlock()
	// Click-away dismiss: stop the live stream too. This runs on the Cocoa main
	// thread (the resign-key observer), so hop off it before calling the Wails
	// runtime, which must not run on the main thread.
	go setPanelLive(ctx, false)
}
