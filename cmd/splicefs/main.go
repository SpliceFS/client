// Command splicefs is the SpliceFS client.
package main

import (
	"os"

	"github.com/BlueRival/splicefs/internal/cli"
)

// exit is replaced in tests.
var exit = os.Exit

func main() {
	exit(cli.Run("splicefs", os.Args[1:], os.Stdout, os.Stderr))
}
