// Command updateapps-gui is the desktop front end: the same engine, config,
// definitions and state as updateapps, with a window, tray icon and
// notifications. Needs cgo and OpenGL; the CLI doesn't.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/gnoling/updateapps/internal/config"
	"github.com/gnoling/updateapps/internal/gui"
)

func main() {
	var opts gui.Options
	flag.StringVar(&opts.Config, "config", "", "config file (default "+config.DefaultPath()+")")
	flag.StringVar(&opts.Defs, "defs", "", "use just this flat folder of definitions, trusted (for developing a repository)")
	flag.BoolVar(&opts.Hidden, "hidden", false, "start in the tray without showing the window (for autostart)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: updateapps-gui [--config FILE] [--defs DIR] [--hidden]")
		flag.PrintDefaults()
	}
	flag.Parse()
	os.Exit(gui.Main(opts))
}
