//go:build darwin

package main

/*
#cgo LDFLAGS: -framework Cocoa
extern void installStatusItem(const void *png, int len);
extern void statusItemAnchor(int winWidth, int *outX, int *outY);
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

// statusItemAnchor returns the top-left position (Wails screen coords) for a
// popover of width winWidth, anchored under the status item.
func statusItemAnchor(winWidth int) (x, y int) {
	var cx, cy C.int
	C.statusItemAnchor(C.int(winWidth), &cx, &cy)
	return int(cx), int(cy)
}

// enablePopoverDismiss hides the popover when the user clicks away.
func enablePopoverDismiss() { C.enablePopoverDismiss() }

// focusPopover makes the popover the key window so it can dismiss on blur.
func focusPopover() { C.focusPopover() }

//export statusItemClickedGo
func statusItemClickedGo() {
	onStatusItemClick()
}

//export popoverDidHideGo
func popoverDidHideGo() {
	winVisible = false
}
