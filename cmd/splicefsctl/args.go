package main

import (
	"errors"
	"fmt"

	"github.com/SpliceFS/client/internal/cli"
)

// The commands.
const (
	commandUmount = "umount"
	commandStatus = "status"
	commandMount  = "mount"
)

// The errors of a command line. Each message is completed with the offending value.
var (
	errMissingCommand     = errors.New("missing command")
	errUnknownCommand     = errors.New("unknown command")
	errUnexpectedArgument = errors.New("unexpected argument")
)

// invocation is a parsed command line.
type invocation struct {
	cli.Flags

	command string
	// targets are the mount targets given to umount.
	targets []string
	// force is the --force flag of umount.
	force bool
	// asJSON is the --json flag of status.
	asJSON bool
	// mountArgs are the words after mount, which are read like the command line of the mount
	// helper.
	mountArgs []string
}

// parseArgs reads the command line. It returns flag.ErrHelp when help was asked for.
func parseArgs(args []string) (invocation, error) {
	var inv invocation
	fs := cli.NewFlagSet(program)
	inv.Register(fs)
	if err := cli.ParseFlags(fs, args); err != nil {
		return invocation{}, err
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

// parseCommand reads the arguments that follow the command name.
func (inv *invocation) parseCommand(args []string) error {
	fs := cli.NewFlagSet(inv.command)
	switch inv.command {
	case commandMount:
		// The words and flags of a mount are the helper's, which the flag package cannot read.
		inv.mountArgs = args
		return nil
	case commandUmount:
		fs.BoolVar(&inv.force, "force", false, "")
	case commandStatus:
		fs.BoolVar(&inv.asJSON, "json", false, "")
	default:
		return fmt.Errorf("%w %q", errUnknownCommand, inv.command)
	}
	if err := cli.ParseFlags(fs, args); err != nil {
		return err
	}
	inv.targets = fs.Args()
	if inv.command == commandStatus && len(inv.targets) > 0 {
		return fmt.Errorf("%w %q", errUnexpectedArgument, inv.targets[0])
	}
	return nil
}
