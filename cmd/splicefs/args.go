package main

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/SpliceFS/client/internal/cli"
)

// The commands.
const (
	commandMount  = "mount"
	commandConfig = "config"
)

// The commands after "config".
const (
	configCheck = "check"
	configShow  = "show"
)

// The errors of a command line. Each message is completed with the offending value.
var (
	errInvalidLogLevel = errors.New("invalid --log-level")
	errMissingCommand  = errors.New("missing command")
	errUnknownCommand  = errors.New("unknown command")
	errConfigWord      = errors.New("config needs one of: check, show")
)

// invocation is a parsed command line.
type invocation struct {
	cli.Flags
	logLevel slog.Level
	command  string
	// targets are the mount targets given to mount.
	targets []string
	// configCommand is check or show for the config command.
	configCommand string
}

// parseArgs reads the command line. It returns flag.ErrHelp when help was asked for.
func parseArgs(args []string) (invocation, error) {
	inv := invocation{logLevel: slog.LevelInfo}
	var level string
	fs := cli.NewFlagSet(program)
	inv.Register(fs)
	fs.StringVar(&level, "log-level", "info", "")
	if err := cli.ParseFlags(fs, args); err != nil {
		return invocation{}, err
	}
	var ok bool
	if inv.logLevel, ok = parseLevel(level); !ok {
		return invocation{}, fmt.Errorf("%w %q: use debug, info, warn or error", errInvalidLogLevel, level)
	}
	if inv.ShowVersion {
		return inv, nil
	}
	if fs.NArg() == 0 {
		return invocation{}, errMissingCommand
	}
	inv.command = fs.Arg(0)
	err := inv.parseCommand(fs.Args()[1:])
	return inv, err
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

// parseCommand reads the arguments that follow the command name.
func (inv *invocation) parseCommand(args []string) error {
	fs := cli.NewFlagSet(inv.command)
	switch inv.command {
	case commandMount, commandConfig:
		// These take no flags.
	default:
		return fmt.Errorf("%w %q", errUnknownCommand, inv.command)
	}
	if err := cli.ParseFlags(fs, args); err != nil {
		return err
	}
	inv.targets = fs.Args()
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
