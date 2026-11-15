package main

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/SpliceFS/client/internal/cli"
)

// commandConfig is the one command. The client runs the service when no command is given.
const commandConfig = "config"

// The commands after "config".
const (
	configCheck = "check"
	configShow  = "show"
)

// The errors of a command line. Each message is completed with the offending value.
var (
	errInvalidLogLevel = errors.New("invalid --log-level")
	errUnknownCommand  = errors.New("unknown command")
	errConfigWord      = errors.New("config needs one of: check, show")
)

// invocation is a parsed command line.
type invocation struct {
	cli.Flags
	logLevel slog.Level
	// command is empty when the service is to run.
	command string
	// targets are the mount targets the service is limited to; none means every mount.
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
	fs.Var((*targetList)(&inv.targets), "target", "")
	if err := cli.ParseFlags(fs, args); err != nil {
		return invocation{}, err
	}
	var ok bool
	if inv.logLevel, ok = parseLevel(level); !ok {
		return invocation{}, fmt.Errorf("%w %q: use debug, info, warn or error", errInvalidLogLevel, level)
	}
	if inv.ShowVersion || fs.NArg() == 0 {
		return inv, nil
	}
	inv.command = fs.Arg(0)
	err := inv.parseCommand(fs.Args()[1:])
	return inv, err
}

// targetList is the value of a flag that may be given several times.
type targetList []string

func (l *targetList) String() string { return strings.Join(*l, ",") }

func (l *targetList) Set(value string) error {
	*l = append(*l, value)
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

// parseCommand reads the arguments that follow the command name.
func (inv *invocation) parseCommand(args []string) error {
	if inv.command != commandConfig {
		return fmt.Errorf("%w %q", errUnknownCommand, inv.command)
	}
	// The config command takes no flags.
	fs := cli.NewFlagSet(inv.command)
	if err := cli.ParseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 || (fs.Arg(0) != configCheck && fs.Arg(0) != configShow) {
		return errConfigWord
	}
	inv.configCommand = fs.Arg(0)
	return nil
}
