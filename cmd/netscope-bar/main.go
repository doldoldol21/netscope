// Command netscope-bar is netscope's macOS menu-bar app. It shows the live
// up/down rate in the menu bar and a dropdown of the top apps, all read from
// the netscoped daemon over its unix socket. Use the "Open Dashboard…" item to
// launch the full window.
package main

import (
	"flag"

	"github.com/doldoldol21/netscope/internal/ipc"
	"github.com/doldoldol21/netscope/internal/menubar"
)

func main() {
	sock := flag.String("sock", ipc.DefaultSocketPath(), "netscoped unix socket path")
	flag.Parse()
	menubar.Run(*sock)
}
