// Command splicefs is the SpliceFS client. It mounts the directories named in its
// configuration and keeps them mounted. The splicefsctl program unmounts them again and
// reports what the running client is doing.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/SpliceFS/client/app"
	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/internal/cli"
)

const (
	program = "splicefs"
	usage   = `usage: splicefs [--config DIR] [--log-level debug|info|warn|error] [--target TARGET]...
       splicefs [--config DIR] config check
       splicefs [--config DIR] config show
       splicefs --version

Without a command, splicefs is the client: it mounts what the configuration names and keeps
running, with or without mounts, until it is stopped. --target limits it to the mounts with
that target and can be given more than once. SIGHUP applies the configuration without a
restart: mount files that were added are mounted, removed ones are detached, changed ones
are mounted again.

Unmounting, the status and mounting one more directory are commands of splicefsctl.
`
)

var prog = cli.Program{Name: program, Usage: usage} //nolint:gochecknoglobals // constant data, only the type has no constant form

// exit is replaced in tests, so that main can run without ending the test process.
var exit = os.Exit //nolint:gochecknoglobals // the one seam the tests replace; nothing else reads or writes it

func main() {
	exit(run(context.Background(), os.Args, os.Stdin, os.Stdout, os.Stderr))
}

// run is the whole program. argv is the process's os.Args, program name first. It returns
// the exit status. The service stays in the foreground until ctx is cancelled or the process
// receives SIGINT or SIGTERM.
func run(ctx context.Context, argv []string, _ io.Reader, stdout, stderr io.Writer) int {
	_, args := cli.SplitArgv(argv)
	return execute(ctx, args, stdout, stderr, systemBackend())
}

// backend is everything the commands take from the client library and the operating
// system. Tests replace it so that no real mount happens.
type backend struct {
	loadConfig func(dir string) (*config.Config, error)
	// defaultConfigDir finds the configuration directory used when --config is not given.
	defaultConfigDir func() (string, error)
	run              func(ctx context.Context, cfg *config.Config, targets []string, opts app.Options) error
	mounter          app.Mounter
	notify           func(state string) error
}

func systemBackend() backend {
	return backend{
		loadConfig:       config.LoadDir,
		defaultConfigDir: config.DefaultConfigDir,
		run:              app.Run,
		mounter:          platformMounter(),
		notify:           app.NotifyFunc(os.Getenv("NOTIFY_SOCKET")),
	}
}

// execute parses the command line and runs the command it names.
func execute(ctx context.Context, args []string, stdout, stderr io.Writer, b backend) int {
	inv, err := parseArgs(args)
	if status, done := prog.Handled(err, inv.ShowVersion, stdout, stderr); done {
		return status
	}
	if err := inv.ResolveConfigDir(b.defaultConfigDir); err != nil {
		return prog.Fail(stderr, err)
	}
	// A stop signal ends the service, which unmounts what it mounted.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := b.runCommand(ctx, inv, stdout, stderr); err != nil {
		return prog.Fail(stderr, err)
	}
	return cli.ExitOK
}

func (b backend) runCommand(ctx context.Context, inv invocation, stdout, stderr io.Writer) error {
	if inv.command == commandConfig {
		return b.runConfig(inv, stdout)
	}
	return b.serve(ctx, inv, stderr)
}

// runConfig implements config check and config show. A configuration with problems is an
// error that lists all of them.
func (b backend) runConfig(inv invocation, stdout io.Writer) error {
	cfg, err := b.loadConfig(inv.ConfigDir)
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

// serve loads the configuration and keeps the mounts up until ctx ends. SIGHUP makes the
// run read the same configuration directory again.
func (b backend) serve(ctx context.Context, inv invocation, stderr io.Writer) error {
	cfg, err := b.loadConfig(inv.ConfigDir)
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
		ConfigDir: inv.ConfigDir,
		Reload:    reload,
		Load:      func() (*config.Config, error) { return b.loadConfig(inv.ConfigDir) },
	})
}
