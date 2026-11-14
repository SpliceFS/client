// Command splicefs is the SpliceFS client. It mounts the directories named in its
// configuration, unmounts them again, and reports what the running client is doing.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/SpliceFS/client/app"
	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/internal/version"
	"github.com/SpliceFS/client/mount/nfs"
)

// Exit statuses.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

const (
	program = "splicefs"
	usage   = `usage: splicefs [--config DIR] [--log-level debug|info|warn|error] mount [TARGET...]
       splicefs [--config DIR] umount [--force] [TARGET...]
       splicefs [--config DIR] status [--json]
       splicefs [--config DIR] config check
       splicefs [--config DIR] config show
       splicefs --version

mount keeps running, with or without mounts, until it is stopped. SIGHUP applies the
configuration without a restart: mount files that were added are mounted, removed ones are
detached, changed ones are mounted again.

Started as mount.splicefs, the program is the helper of mount(8) and fstab: it asks the
running client to mount CLUSTER:PATH at TARGET.
`
)

// exit is replaced in tests, so that main can run without ending the test process.
var exit = os.Exit //nolint:gochecknoglobals // the one seam the tests replace; nothing else reads or writes it

func main() {
	exit(run(context.Background(), os.Args, os.Stdin, os.Stdout, os.Stderr))
}

// run is the whole program. argv is the process's os.Args, program name first. It returns
// the exit status. A mount stays in the foreground until ctx is cancelled or the process
// receives SIGINT or SIGTERM.
func run(ctx context.Context, argv []string, _ io.Reader, stdout, stderr io.Writer) int {
	return dispatch(ctx, argv, stdout, stderr, systemBackend())
}

// dispatch runs the mount helper when the program was started under its name, which is a
// link to this program, and the splicefs commands otherwise.
func dispatch(ctx context.Context, argv []string, stdout, stderr io.Writer, b backend) int {
	var name string
	var args []string
	if len(argv) > 0 {
		name, args = filepath.Base(argv[0]), argv[1:]
	}
	if name == helperProgram {
		return b.executeHelper(ctx, args, stderr)
	}
	return execute(ctx, args, stdout, stderr, b)
}

// backend is everything the commands take from the client library and the operating
// system. Tests replace it so that no real mount happens.
type backend struct {
	loadConfig func(dir string) (*config.Config, error)
	// defaultConfigDir finds the configuration directory used when --config is not given.
	defaultConfigDir func() (string, error)
	run              func(ctx context.Context, cfg *config.Config, targets []string, opts app.Options) error
	unmount          func(ctx context.Context, socketPath string, targets []string, force bool) error
	// clearLeftBehind removes the mounts a client that died left behind, when none is running.
	clearLeftBehind func(ctx context.Context, socketPath string, targets []string) ([]nfs.Cleared, error)
	status          func(ctx context.Context, socketPath string) (app.Report, error)
	// requestMount, getenv and abs serve the mount helper.
	requestMount func(ctx context.Context, socketPath string, req app.MountRequest) error
	getenv       func(key string) string
	abs          func(path string) (string, error)
	mounter      app.Mounter
	notify       func(state string) error
}

func systemBackend() backend {
	return backend{
		loadConfig:       config.LoadDir,
		defaultConfigDir: config.DefaultConfigDir,
		run:              app.Run,
		unmount:          app.Unmount,
		clearLeftBehind:  app.ClearLeftBehind,
		status:           app.Status,
		requestMount:     app.RequestMount,
		getenv:           os.Getenv,
		abs:              filepath.Abs,
		mounter:          platformMounter(),
		notify:           app.NotifyFunc(os.Getenv("NOTIFY_SOCKET")),
	}
}

// execute parses the command line and runs the command it names.
func execute(ctx context.Context, args []string, stdout, stderr io.Writer, b backend) int {
	inv, err := parseArgs(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return announcef(stdout, "%s", usage)
	case err != nil:
		sayf(stderr, "%s: %v\n%s", program, err, usage)
		return exitUsage
	case inv.showVersion:
		return announcef(stdout, "%s %s\n", program, version.String())
	}
	if inv.configDir == "" {
		if inv.configDir, err = b.defaultConfigDir(); err != nil {
			return fail(stderr, err)
		}
	}
	// A stop signal ends the command that is running: it unmounts for mount and
	// abandons the wait for the other two.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := b.runCommand(ctx, inv, stdout, stderr); err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

func (b backend) runCommand(ctx context.Context, inv invocation, stdout, stderr io.Writer) error {
	switch inv.command {
	case commandMount:
		return b.mount(ctx, inv, stderr)
	case commandUmount:
		return b.umount(ctx, inv, stdout, stderr)
	case commandConfig:
		return b.runConfig(inv, stdout)
	}
	return b.printStatus(ctx, inv, stdout)
}

// umount asks the running client to unmount. When none is running, it removes the mounts
// that a client that died left behind, which only macOS keeps a record of. The error of
// the missing client stays the answer when there was nothing to remove.
func (b backend) umount(ctx context.Context, inv invocation, stdout, stderr io.Writer) error {
	socketPath := app.SocketPath(inv.configDir)
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
			sayf(stdout, "removed the mount %s left behind by a client that is no longer running\n", r.Target)
		case nfs.OutcomeServed:
			sayf(stderr, "%s is served by a client of another configuration; left alone\n", r.Target)
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

// runConfig implements config check and config show. A configuration with problems is an
// error that lists all of them.
func (b backend) runConfig(inv invocation, stdout io.Writer) error {
	cfg, err := b.loadConfig(inv.configDir)
	if err != nil {
		return err
	}
	if inv.configCommand == configCheck {
		_, err = fmt.Fprintf(stdout, "ok: %d clusters, %d mounts\n", len(cfg.Clusters), len(cfg.Mounts))
		//nolint:wrapcheck // The error of the write already names the file, and the command prints it as it is.
		return err
	}
	return showConfig(stdout, cfg)
}

// mount loads the configuration and keeps the mounts up until ctx ends. SIGHUP makes the
// run read the same configuration directory again.
func (b backend) mount(ctx context.Context, inv invocation, stderr io.Writer) error {
	cfg, err := b.loadConfig(inv.configDir)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: inv.logLevel}))
	// The signal package never blocks on a full channel, so the channel needs room for one
	// signal. A second SIGHUP that arrives while one is waiting is dropped, which loses
	// nothing: the reload that is waiting reads the files after both changes.
	reload := make(chan os.Signal, 1)
	signal.Notify(reload, syscall.SIGHUP)
	defer signal.Stop(reload)
	return b.run(ctx, cfg, inv.targets, app.Options{
		Logger:    logger,
		Mounter:   b.mounter,
		Notify:    b.notify,
		ConfigDir: inv.configDir,
		Reload:    reload,
		Load:      func() (*config.Config, error) { return b.loadConfig(inv.configDir) },
	})
}

// fail prints err and returns the exit status for a runtime error.
func fail(stderr io.Writer, err error) int {
	sayf(stderr, "%s: %v\n", program, err)
	if errors.Is(err, app.ErrBusy) {
		sayf(stderr, "%s: a mount that is in use is left alone; use \"umount --force\" to unmount it anyway\n", program)
	}
	return exitError
}

// announcef writes the answer to a help or version request to stdout and returns the exit
// status, which is success whether or not the answer could be written.
func announcef(stdout io.Writer, format string, args ...any) int {
	sayf(stdout, format, args...)
	return exitOK
}

// sayf writes a message to w and drops a failed write. For stderr there is nowhere left to
// report it, because stderr is where failures are reported. The answer to a help or version
// request has always ended in success, so it is dropped there as well.
func sayf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...) //nolint:errcheck // See above, a failed write is dropped on purpose.
}
