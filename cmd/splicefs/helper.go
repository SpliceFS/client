package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/SpliceFS/client/app"
)

const (
	// helperProgram is the name mount(8) looks for when it is given "-t splicefs" or reads
	// an fstab line of that type. The packages link it to the splicefs program.
	helperProgram = "mount.splicefs"
	helperUsage   = "usage: mount.splicefs CLUSTER:PATH TARGET [-sfnv] [-o OPTIONS]\n"
	// configEnv names the configuration directory when the helper runs, because mount(8)
	// gives it no way to take a --config flag.
	configEnv = "SPLICEFS_CONFIG"
	// helperFlags are the flags of mount(8) that have no meaning here.
	helperFlags = "sfnv"
	// exitMountFailed is the status mount(8) documents for a mount that failed.
	exitMountFailed = 32
	// helperWordCount is the number of words a helper command line holds besides its flags:
	// the source and the target.
	helperWordCount = 2
)

// The errors of a mount helper command line. Each message is completed with the offending
// value where there is one.
var (
	errNeedSourceAndTarget = errors.New("need exactly a source and a target")
	errOptionNeedsValue    = errors.New("-o needs a value")
	errUnknownFlag         = errors.New("unknown flag")
	errSourceFormat        = errors.New("is not CLUSTER:PATH")
	errUnsupportedOption   = errors.New("unsupported mount option")
)

// ignoredOptions are options of mount(8) and fstab that a mount of this client has no use
// for. They are accepted so that a common fstab line works as written. "rw" is among them
// because mount(8) passes it to every helper unless the caller asked for "ro"; the mount is
// read-only whatever is asked.
func ignoredOptions() []string {
	return []string{
		"rw", "async",
		"ro", "defaults", "auto", "noauto", "nofail",
		"_netdev", "user", "nouser", "users", "owner",
		"exec", "noexec", "suid", "nosuid", "dev", "nodev",
		"atime", "noatime", "relatime",
	}
}

// hasIgnoredPrefix tells whether opt starts an option that other programs read, such as
// x-systemd.requires=.
func hasIgnoredPrefix(opt string) bool {
	return strings.HasPrefix(opt, "x-") || strings.HasPrefix(opt, "comment=")
}

// executeHelper is the program when it runs as the mount helper: it asks the running client
// for one mount and returns the exit status that mount(8) understands.
func (b backend) executeHelper(ctx context.Context, args []string, stderr io.Writer) int {
	req, err := b.parseHelperArgs(args)
	if err != nil {
		sayf(stderr, "%s: %v\n%s", helperProgram, err, helperUsage)
		return exitError
	}
	if err := b.requestHelperMount(ctx, req); err != nil {
		sayf(stderr, "%s: %v\n", helperProgram, err)
		return exitMountFailed
	}
	return exitOK
}

// requestHelperMount sends req to the client of the configuration directory that the
// helper is told to use.
func (b backend) requestHelperMount(ctx context.Context, req app.MountRequest) error {
	dir := b.getenv(configEnv)
	if dir == "" {
		var err error
		if dir, err = b.defaultConfigDir(); err != nil {
			return err
		}
	}
	return b.requestMount(ctx, app.SocketPath(dir), req)
}

// parseHelperArgs reads "<source> <target> [-sfnv] [-o options]". Flags may come before
// or after the two words, and -o may be repeated.
func (b backend) parseHelperArgs(args []string) (app.MountRequest, error) {
	parsed, err := splitHelperArgs(args)
	if err != nil {
		return app.MountRequest{}, err
	}
	if len(parsed.words) != helperWordCount {
		return app.MountRequest{}, errNeedSourceAndTarget
	}
	source, err := parseSource(parsed.words[0])
	if err != nil {
		return app.MountRequest{}, err
	}
	// The client resolves relative paths against its own directory, which is not the
	// caller's.
	target, err := b.abs(parsed.words[1])
	if err != nil {
		return app.MountRequest{}, fmt.Errorf("resolve target %q: %w", parsed.words[1], err)
	}
	req := app.MountRequest{Cluster: source.cluster, Path: source.path, Target: filepath.Clean(target)}
	err = applyOptions(&req, parsed.options)
	return req, err
}

// helperArgs is a helper command line sorted by kind.
type helperArgs struct {
	// words are the arguments that are not flags: the source and the target.
	words []string
	// options are the values given to -o, each a comma separated list.
	options []string
}

// splitHelperArgs separates the source and target from the values of -o. The ignored flags
// are dropped.
func splitHelperArgs(args []string) (helperArgs, error) {
	var parsed helperArgs
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-o":
			i++
			if i == len(args) {
				return helperArgs{}, errOptionNeedsValue
			}
			parsed.options = append(parsed.options, args[i])
		case len(arg) > 1 && arg[0] == '-':
			if strings.ContainsFunc(arg[1:], notHelperFlag) {
				return helperArgs{}, fmt.Errorf("%w %q", errUnknownFlag, arg)
			}
		default:
			parsed.words = append(parsed.words, arg)
		}
	}
	return parsed, nil
}

func notHelperFlag(r rune) bool { return !strings.ContainsRune(helperFlags, r) }

// mountSource is the "<cluster>:<path>" a helper is asked to mount.
type mountSource struct {
	cluster string
	path    string
}

// parseSource splits "<cluster>:<path>" at the first colon, so that the path may hold
// colons. An empty path is the whole cluster.
func parseSource(text string) (mountSource, error) {
	cluster, path, found := strings.Cut(text, ":")
	if !found || cluster == "" {
		return mountSource{}, fmt.Errorf("source %q %w", text, errSourceFormat)
	}
	if path == "" {
		path = "/"
	}
	return mountSource{cluster: cluster, path: path}, nil
}

// applyOptions sets req from the comma separated lists given to -o. The last of offline
// and nooffline wins.
func applyOptions(req *app.MountRequest, lists []string) error {
	for _, list := range lists {
		for opt := range strings.SplitSeq(list, ",") {
			switch {
			case opt == "allow_other":
				req.AllowOther = true
			case opt == "offline":
				req.Offline = new(true)
			case opt == "nooffline":
				req.Offline = new(false)
			case opt == "" || isIgnoredOption(opt):
			default:
				return fmt.Errorf("%w %q", errUnsupportedOption, opt)
			}
		}
	}
	return nil
}

func isIgnoredOption(opt string) bool {
	return slices.Contains(ignoredOptions(), opt) || hasIgnoredPrefix(opt)
}
