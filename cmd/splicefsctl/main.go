// Command splicefsctl controls a running SpliceFS client: it unmounts, mounts one more
// directory and reports what the client is doing. The client itself is the splicefs program.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/SpliceFS/client/app"
	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/internal/cli"
	"github.com/SpliceFS/client/mount/nfs"
)

const (
	program = "splicefsctl"
	usage   = `usage: splicefsctl [--config DIR] umount [--force] [TARGET...]
       splicefsctl [--config DIR] status [--json]
       splicefsctl [--config DIR] mount CLUSTER:PATH TARGET [-o OPTIONS]
       splicefsctl --version

Every command talks to the client that runs for the configuration directory; the client is
the splicefs program.

Started as mount.splicefs, the program is the helper of mount(8) and fstab: it asks the
running client to mount CLUSTER:PATH at TARGET.
`
)

var prog = cli.Program{Name: program, Usage: usage} //nolint:gochecknoglobals // constant data, only the type has no constant form

// exit is replaced in tests, so that main can run without ending the test process.
var exit = os.Exit //nolint:gochecknoglobals // the one seam the tests replace; nothing else reads or writes it

func main() {
	exit(run(context.Background(), os.Args, os.Stdin, os.Stdout, os.Stderr))
}

// run is the whole program. argv is the process's os.Args, program name first. It returns
// the exit status.
func run(ctx context.Context, argv []string, _ io.Reader, stdout, stderr io.Writer) int {
	return dispatch(ctx, argv, stdout, stderr, systemBackend())
}

// dispatch runs the mount helper when the program was started under its name, which is a
// link to this program, and the splicefsctl commands otherwise.
func dispatch(ctx context.Context, argv []string, stdout, stderr io.Writer, b backend) int {
	name, args := cli.SplitArgv(argv)
	if name == helperProgram {
		return b.executeHelper(ctx, args, stderr)
	}
	return execute(ctx, args, stdout, stderr, b)
}

// backend is everything the commands take from the client library and the operating
// system. Tests replace it so that no real client is needed.
type backend struct {
	// defaultConfigDir finds the configuration directory used when --config is not given.
	defaultConfigDir func() (string, error)
	unmount          func(ctx context.Context, socketPath string, targets []string, force bool) error
	// clearLeftBehind removes the mounts a client that died left behind, when none is running.
	clearLeftBehind func(ctx context.Context, socketPath string, targets []string) ([]nfs.Cleared, error)
	status          func(ctx context.Context, socketPath string) (app.Report, error)
	// requestMount serves the mount command and the mount helper; getenv and abs serve the
	// helper's command line.
	requestMount func(ctx context.Context, socketPath string, req app.MountRequest) error
	getenv       func(key string) string
	abs          func(path string) (string, error)
}

func systemBackend() backend {
	return backend{
		defaultConfigDir: config.DefaultConfigDir,
		unmount:          app.Unmount,
		clearLeftBehind:  app.ClearLeftBehind,
		status:           app.Status,
		requestMount:     app.RequestMount,
		getenv:           os.Getenv,
		abs:              filepath.Abs,
	}
}

// execute parses the command line and runs the command it names.
func execute(ctx context.Context, args []string, stdout, stderr io.Writer, b backend) int {
	inv, err := parseArgs(args)
	if status, done := prog.Handled(err, inv.ShowVersion, stdout, stderr); done {
		return status
	}
	if err := inv.ResolveConfigDir(b.defaultConfigDir); err != nil {
		return fail(stderr, err)
	}
	// A stop signal abandons the wait for the client.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := b.runCommand(ctx, inv, stdout, stderr); err != nil {
		return fail(stderr, err)
	}
	return cli.ExitOK
}

func (b backend) runCommand(ctx context.Context, inv invocation, stdout, stderr io.Writer) error {
	switch inv.command {
	case commandUmount:
		return b.umount(ctx, inv, stdout, stderr)
	case commandMount:
		return b.mount(ctx, inv)
	}
	return b.printStatus(ctx, inv, stdout)
}

// umount asks the running client to unmount. When none is running, it removes the mounts
// that a client that died left behind, which only macOS keeps a record of. The error of
// the missing client stays the answer when there was nothing to remove.
func (b backend) umount(ctx context.Context, inv invocation, stdout, stderr io.Writer) error {
	socketPath := app.SocketPath(inv.ConfigDir)
	notRunning := b.unmount(ctx, socketPath, inv.targets, inv.force)
	if !errors.Is(notRunning, app.ErrNotRunning) {
		return notRunning
	}
	results, recordErr := b.clearLeftBehind(ctx, socketPath, inv.targets)
	cleared, failures := tellLeftBehind(results, stdout, stderr)
	if failed := errors.Join(append(failures, recordErr)...); failed != nil || cleared > 0 {
		return failed
	}
	return notRunning
}

// tellLeftBehind prints what became of each mount and returns how many were removed and why
// the others could not be.
func tellLeftBehind(results []nfs.Cleared, stdout, stderr io.Writer) (int, []error) {
	cleared := 0
	var failures []error
	for _, r := range results {
		switch r.Outcome {
		case nfs.OutcomeCleared:
			cleared++
			cli.Sayf(stdout, "removed the mount %s left behind by a client that is no longer running\n", r.Target)
		case nfs.OutcomeServed:
			cli.Sayf(stderr, "%s is served by a client of another configuration; left alone\n", r.Target)
		case nfs.OutcomeFailed:
			failures = append(failures, fmt.Errorf("%s: %w", r.Target, r.Err))
		case nfs.OutcomeGone:
			// The mount is not there any more, so there is nothing to tell.
			continue
		default:
			// An outcome that this program does not know says nothing a person could act on.
		}
	}
	return cleared, failures
}

// mount asks the running client to mount one more directory, the same request the mount
// helper sends.
func (b backend) mount(ctx context.Context, inv invocation) error {
	req, err := b.parseHelperArgs(inv.mountArgs)
	if err != nil {
		return err
	}
	return b.requestMount(ctx, app.SocketPath(inv.ConfigDir), req)
}

// fail prints err and returns the exit status for a runtime error.
func fail(stderr io.Writer, err error) int {
	status := prog.Fail(stderr, err)
	if errors.Is(err, app.ErrBusy) {
		cli.Sayf(stderr, "%s: a mount that is in use is left alone; use \"umount --force\" to unmount it anyway\n", program)
	}
	return status
}
