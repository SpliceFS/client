package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"

	"github.com/SpliceFS/client/config"
)

// The commands.
const (
	commandMount  = "mount"
	commandUmount = "umount"
	commandStatus = "status"
)

// invocation is a parsed command line.
type invocation struct {
	configDir   string
	logLevel    slog.Level
	showVersion bool
	command     string
	// targets are the mount targets given to mount and umount.
	targets []string
	// force is the --force flag of umount.
	force bool
	// asJSON is the --json flag of status.
	asJSON bool
}

// parseArgs reads the command line. It returns flag.ErrHelp when help was asked for.
func parseArgs(args []string) (invocation, error) {
	inv := invocation{configDir: config.DefaultDir, logLevel: slog.LevelInfo}
	var level string
	fs := newFlagSet(program)
	fs.StringVar(&inv.configDir, "config", config.DefaultDir, "")
	fs.StringVar(&level, "log-level", "info", "")
	fs.BoolVar(&inv.showVersion, "version", false, "")
	if err := fs.Parse(args); err != nil {
		return invocation{}, err
	}
	var ok bool
	if inv.logLevel, ok = parseLevel(level); !ok {
		return invocation{}, fmt.Errorf("invalid --log-level %q: use debug, info, warn or error", level)
	}
	if inv.showVersion {
		return inv, nil
	}
	if fs.NArg() == 0 {
		return invocation{}, errors.New("missing command")
	}
	inv.command = fs.Arg(0)
	return inv, inv.parseCommand(fs.Args()[1:])
}

// parseLevel accepts exactly the four names the usage text lists. slog would also take
// forms such as "info+2".
func parseLevel(name string) (slog.Level, bool) {
	switch name {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return slog.LevelInfo, false
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseCommand reads the arguments that follow the command name.
func (inv *invocation) parseCommand(args []string) error {
	fs := newFlagSet(inv.command)
	switch inv.command {
	case commandMount:
	case commandUmount:
		fs.BoolVar(&inv.force, "force", false, "")
	case commandStatus:
		fs.BoolVar(&inv.asJSON, "json", false, "")
	default:
		return fmt.Errorf("unknown command %q", inv.command)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	inv.targets = fs.Args()
	if inv.command == commandStatus && len(inv.targets) > 0 {
		return fmt.Errorf("unexpected argument %q", inv.targets[0])
	}
	return nil
}
