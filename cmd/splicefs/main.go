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
	"syscall"

	"github.com/SpliceFS/client/app"
	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/internal/version"
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
`
)

// exit is replaced in tests.
var exit = os.Exit

func main() {
	exit(run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run is the whole program. It returns the exit status. A mount stays in the foreground
// until ctx is cancelled or the process receives SIGINT or SIGTERM.
func run(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
	return execute(ctx, args, stdout, stderr, systemBackend())
}

// backend is everything the commands take from the client library and the operating
// system. Tests replace it so that no real mount happens.
type backend struct {
	loadConfig func(dir string) (*config.Config, error)
	// defaultConfigDir finds the configuration directory used when --config is not given.
	defaultConfigDir func() (string, error)
	run              func(ctx context.Context, cfg *config.Config, targets []string, opts app.Options) error
	unmount          func(ctx context.Context, socketPath string, targets []string, force bool) error
	status           func(ctx context.Context, socketPath string) (app.Report, error)
	mounter          app.Mounter
	notify           func(state string) error
}

func systemBackend() backend {
	return backend{
		loadConfig:       config.LoadDir,
		defaultConfigDir: config.DefaultConfigDir,
		run:              app.Run,
		unmount:          app.Unmount,
		status:           app.Status,
		mounter:          platformMounter(),
		notify:           app.NotifyFunc(os.Getenv("NOTIFY_SOCKET")),
	}
}

// execute parses the command line and runs the command it names.
func execute(ctx context.Context, args []string, stdout, stderr io.Writer, b backend) int {
	inv, err := parseArgs(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		_, _ = io.WriteString(stdout, usage)
		return exitOK
	case err != nil:
		_, _ = fmt.Fprintf(stderr, "%s: %v\n%s", program, err, usage)
		return exitUsage
	case inv.showVersion:
		_, _ = fmt.Fprintf(stdout, "%s %s\n", program, version.String())
		return exitOK
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
		return b.unmount(ctx, app.SocketPath(inv.configDir), inv.targets, inv.force)
	case commandConfig:
		return b.runConfig(inv, stdout)
	}
	return b.printStatus(ctx, inv, stdout)
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
		return err
	}
	return showConfig(stdout, cfg)
}

// mount loads the configuration and keeps the mounts up until ctx ends.
func (b backend) mount(ctx context.Context, inv invocation, stderr io.Writer) error {
	cfg, err := b.loadConfig(inv.configDir)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: inv.logLevel}))
	return b.run(ctx, cfg, inv.targets, app.Options{
		Logger:    logger,
		Mounter:   b.mounter,
		Notify:    b.notify,
		ConfigDir: inv.configDir,
	})
}

// fail prints err and returns the exit status for a runtime error.
func fail(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "%s: %v\n", program, err)
	if errors.Is(err, app.ErrBusy) {
		_, _ = fmt.Fprintf(stderr, "%s: a mount that is in use is left alone; use \"umount --force\" to unmount it anyway\n", program)
	}
	return exitError
}
