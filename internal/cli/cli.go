// Package cli holds what the programs of the client have in common on the command line:
// the exit statuses, how a message is written, the flags every program takes and the
// answers to a help or version request.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/SpliceFS/client/internal/version"
)

// Exit statuses.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// Program is a program of the client, as its messages name it.
type Program struct {
	Name  string
	Usage string
}

// Flags are the flags that every program takes before its command.
type Flags struct {
	// ConfigDir is the --config flag. It is empty when the flag was not given, until
	// ResolveConfigDir fills in the default.
	ConfigDir   string
	ShowVersion bool
}

// Register adds the flags to fs.
func (f *Flags) Register(fs *flag.FlagSet) {
	fs.StringVar(&f.ConfigDir, "config", "", "")
	fs.BoolVar(&f.ShowVersion, "version", false, "")
}

// ResolveConfigDir sets ConfigDir to the default when no flag gave it. The default is
// looked up only when it is needed, because finding it can fail.
func (f *Flags) ResolveConfigDir(defaultDir func() (string, error)) error {
	if f.ConfigDir != "" {
		return nil
	}
	dir, err := defaultDir()
	if err != nil {
		return err
	}
	f.ConfigDir = dir
	return nil
}

// ProgramName is the name a program was started under, without its folder. Linux allows a
// process with no argv at all, so the name can be empty.
func ProgramName(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	return filepath.Base(argv[0])
}

// Arguments are the words after the name a program was started under.
func Arguments(argv []string) []string {
	if len(argv) == 0 {
		return nil
	}
	return argv[1:]
}

// Handled answers what needs no command: a help request, a command line that did not parse
// (parseErr) and a version request. When the second result is true, the program ends with the
// exit status in the first.
func (p Program) Handled(parseErr error, showVersion bool, stdout, stderr io.Writer) (int, bool) {
	switch {
	case errors.Is(parseErr, flag.ErrHelp):
		return Announcef(stdout, "%s", p.Usage), true
	case parseErr != nil:
		Sayf(stderr, "%s: %v\n%s", p.Name, parseErr, p.Usage)
		return ExitUsage, true
	case showVersion:
		return Announcef(stdout, "%s %s\n", p.Name, version.String()), true
	}
	return ExitOK, false
}

// Fail prints err and returns the exit status for a runtime error.
func (p Program) Fail(stderr io.Writer, err error) int {
	Sayf(stderr, "%s: %v\n", p.Name, err)
	return ExitError
}

// NewFlagSet returns a flag set that prints nothing itself: the program reports the error
// together with its usage text.
func NewFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// ParseFlags reads the flags of fs from args. The message is the flag package's own, which
// already says what is wrong and is shown to the user as it is, so the wrapping adds no
// words; it only keeps flag.ErrHelp recognizable for errors.Is.
func ParseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w", err)
	}
	return nil
}

// Announcef writes the answer to a help or version request to stdout and returns the exit
// status, which is success whether or not the answer could be written.
func Announcef(stdout io.Writer, format string, args ...any) int {
	Sayf(stdout, format, args...)
	return ExitOK
}

// Sayf writes a message to w and drops a failed write. For stderr there is nowhere left to
// report it, because stderr is where failures are reported. The answer to a help or version
// request has always ended in success, so it is dropped there as well.
func Sayf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...) //nolint:errcheck // See above, a failed write is dropped on purpose.
}
