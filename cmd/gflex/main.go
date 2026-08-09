// Command gflex programs a Werewolf VFLEX USB-C Power Delivery voltage adapter
// over its USB-MIDI interface.
//
// Everything lives in internal/cli; this is only the entry point, so that the
// whole command tree stays drivable in-process from tests.
package main

import (
	"os"

	"github.com/jzbz/gflex/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
