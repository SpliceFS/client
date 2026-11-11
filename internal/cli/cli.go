// Package cli holds the command-line behavior shared by the SpliceFS programs.
package cli

import (
	"flag"
	"fmt"
	"io"

	"github.com/BlueRival/splicefs/internal/version"
)

// Exit statuses returned by Run.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// Run handles the arguments both programs understand and returns the exit status.
// name is the program name used in messages, args are the arguments after it.
func Run(name string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	showVersion := flags.Bool("version", false, "print the version and exit")
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}
	if *showVersion {
		if _, err := fmt.Fprintf(stdout, "%s %s\n", name, version.String()); err != nil {
			return ExitError
		}
		return ExitOK
	}
	// Nothing useful can be done if writing to stderr fails.
	_, _ = fmt.Fprintf(stderr, "%s: not implemented yet, see docs/design.md\n", name)
	return ExitError
}
