// Command updateapps is the headless CLI. It must build with CGO_ENABLED=0
// and never import the GUI.
package main

import (
	"os"

	"github.com/gnoling/updateapps/internal/cli"
)

func main() { os.Exit(cli.Main(os.Args[1:])) }
