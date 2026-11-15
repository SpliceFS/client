package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
)

// The commands.
const (
	commandMount  = "mount"
	commandUmount = "umount"
	commandStatus = "status"
	commandConfig = "config"
)

// The commands after "config".
const (
	configCheck = "check"
	configShow  = "show"
)

// The errors of a command line. Each message is completed with the offending value.
var (
	errInvalidLogLevel    = errors.New("invalid --log-level")
	errMissingCommand     = errors.New("missing command")
	errUnknownCommand     = errors.New("unknown command")
	errUnexpectedArgument = errors.New("unexpected argument")
	errConfigWord         = errors.New("config needs one of: check, show")
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
	// configCommand is check or show for the config command.
	configCommand string
}

// parseArgs reads the command line. It returns flag.ErrHelp when help was asked for.
func parseArgs(args []string) (invocation, error) {
	// An empty configDir means the default, which execute looks up only when a command needs
	// it, because finding it can fail.
	inv := invocation{logLevel: slog.LevelInfo}
	var level string
	fs := newFlagSet(program)
	fs.StringVar(&inv.configDir, "config", "", "")
	fs.StringVar(&level, "log-level", "info", "")
	fs.BoolVar(&inv.showVersion, "version", false, "")
	if err := parseFlags(fs, args); err != nil {
		return invocation{}, err
	}
	var ok bool
	if inv.logLevel, ok = parseLevel(level); !ok {
		return invocation{}, fmt.Errorf("%w %q: use debug, info, warn or error", errInvalidLogLevel, level)
	}
	if inv.showVersion {
		return inv, nil
	}
	if fs.NArg() == 0 {
		return invocation{}, errMissingCommand
	}
	inv.command = fs.Arg(0)
	err := inv.parseCommand(fs.Args()[1:])
	return inv, err
}

// parseFlags reads the flags of fs from args. The message is the flag package's own, which
// already says what is wrong and is shown to the user as it is, so the wrapping adds no
// words; it only keeps flag.ErrHelp recognizable for errors.Is.
func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w", err)
	}
	return nil
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
	case commandMount, commandConfig:
		// These take no flags.
	case commandUmount:
		fs.BoolVar(&inv.force, "force", false, "")
	case commandStatus:
		fs.BoolVar(&inv.asJSON, "json", false, "")
	default:
		return fmt.Errorf("%w %q", errUnknownCommand, inv.command)
	}
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	inv.targets = fs.Args()
	if inv.command == commandStatus && len(inv.targets) > 0 {
		return fmt.Errorf("%w %q", errUnexpectedArgument, inv.targets[0])
	}
	if inv.command == commandConfig {
		return inv.parseConfigCommand()
	}
	return nil
}

// parseConfigCommand reads the word after "config", which is held in targets.
func (inv *invocation) parseConfigCommand() error {
	if len(inv.targets) != 1 || (inv.targets[0] != configCheck && inv.targets[0] != configShow) {
		return errConfigWord
	}
	inv.configCommand = inv.targets[0]
	inv.targets = nil
	return nil
}
