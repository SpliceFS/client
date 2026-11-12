package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
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
)

// ignoredOptions are options of mount(8) and fstab that a mount of this client has no use
// for. They are accepted so that a common fstab line works as written. "rw" is among them
// because mount(8) passes it to every helper unless the caller asked for "ro"; the mount is
// read-only whatever is asked.
var ignoredOptions = map[string]bool{
	"rw": true, "async": true,
	"ro": true, "defaults": true, "auto": true, "noauto": true, "nofail": true,
	"_netdev": true, "user": true, "nouser": true, "users": true, "owner": true,
	"exec": true, "noexec": true, "suid": true, "nosuid": true, "dev": true, "nodev": true,
	"atime": true, "noatime": true, "relatime": true,
}

// ignoredPrefixes start options that other programs read, such as x-systemd.requires=.
var ignoredPrefixes = []string{"x-", "comment="}

// executeHelper is the program when it runs as the mount helper: it asks the running client
// for one mount and returns the exit status that mount(8) understands.
func (b backend) executeHelper(ctx context.Context, args []string, stderr io.Writer) int {
	req, err := b.parseHelperArgs(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n%s", helperProgram, err, helperUsage)
		return exitError
	}
	if err := b.requestHelperMount(ctx, req); err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", helperProgram, err)
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
	words, options, err := splitHelperArgs(args)
	if err != nil {
		return app.MountRequest{}, err
	}
	if len(words) != 2 {
		return app.MountRequest{}, errors.New("need exactly a source and a target")
	}
	cluster, path, err := parseSource(words[0])
	if err != nil {
		return app.MountRequest{}, err
	}
	// The client resolves relative paths against its own directory, which is not the
	// caller's.
	target, err := b.abs(words[1])
	if err != nil {
		return app.MountRequest{}, fmt.Errorf("resolve target %q: %w", words[1], err)
	}
	req := app.MountRequest{Cluster: cluster, Path: path, Target: filepath.Clean(target)}
	return req, applyOptions(&req, options)
}

// splitHelperArgs separates the source and target from the values of -o. The ignored flags
// are dropped.
func splitHelperArgs(args []string) (words, options []string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-o":
			if i++; i == len(args) {
				return nil, nil, errors.New("-o needs a value")
			}
			options = append(options, args[i])
		case len(arg) > 1 && arg[0] == '-':
			if strings.ContainsFunc(arg[1:], notHelperFlag) {
				return nil, nil, fmt.Errorf("unknown flag %q", arg)
			}
		default:
			words = append(words, arg)
		}
	}
	return words, options, nil
}

func notHelperFlag(r rune) bool { return !strings.ContainsRune(helperFlags, r) }

// parseSource splits "<cluster>:<path>" at the first colon, so that the path may hold
// colons. An empty path is the whole cluster.
func parseSource(source string) (cluster, path string, err error) {
	cluster, path, found := strings.Cut(source, ":")
	if !found || cluster == "" {
		return "", "", fmt.Errorf("source %q is not CLUSTER:PATH", source)
	}
	if path == "" {
		path = "/"
	}
	return cluster, path, nil
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
				return fmt.Errorf("unsupported mount option %q", opt)
			}
		}
	}
	return nil
}

func isIgnoredOption(opt string) bool {
	if ignoredOptions[opt] {
		return true
	}
	for _, prefix := range ignoredPrefixes {
		if strings.HasPrefix(opt, prefix) {
			return true
		}
	}
	return false
}
