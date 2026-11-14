//go:build darwin

package nfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	// recordFileMode lets only the owner read and write the record.
	recordFileMode = 0o600

	// probeTimeout bounds the check whether a client still serves a recorded port. A port on
	// this machine either answers or refuses at once, so a longer wait only hides a problem.
	probeTimeout = time.Second

	// umountTimeout bounds the removal of one stale mount.
	umountTimeout = 30 * time.Second
)

// ErrLeftBehind completes a sentence that starts with a target: a client that is gone left a
// mount there, and mounting over it would only stack a second mount on a dead one.
var ErrLeftBehind = errors.New("is still mounted by a client that is no longer running")

// recordMu orders the changes of record files made by this process. Mounts of one client
// start and end on different goroutines and share one file, so a change that read the
// file before another one wrote it would lose that write.
var recordMu sync.Mutex //nolint:gochecknoglobals // one lock for all files is enough; a change takes milliseconds

// recordEntry is one NFS mount this program made.
type recordEntry struct {
	// Target is the directory mounted on, as given to mount_nfs.
	Target string `json:"target"`
	// From is the source the mount table shows for the mount.
	From string `json:"from"`
	// Port is the loopback port of the NFS server that stands behind the mount.
	Port int `json:"port"`
	// PID is the process that made the mount.
	PID int `json:"pid"`
	// Created is when the mount was made.
	Created time.Time `json:"created"`
}

// loadRecord reads the record. A file that is missing is an empty list.
func loadRecord(path string) ([]recordEntry, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the record of mounts: %w", err)
	}
	var entries []recordEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrRecordDamaged, path, err)
	}
	return entries, nil
}

// saveRecord replaces the file through a temporary file in the same directory, so that a
// reader never sees half a list.
func saveRecord(path string, entries []recordEntry) error {
	// A list of strings, numbers and times always encodes, so there is no error to handle.
	data, _ := json.Marshal(entries) //nolint:errcheck,errchkjson // See above.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".nfs-*.tmp")
	if err != nil {
		return fmt.Errorf("create the temporary file: %w", err)
	}
	_, err = tmp.Write(data)
	err = errors.Join(err, tmp.Chmod(recordFileMode), tmp.Close())
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		// The write already failed and its error is the one returned. A temporary file that stays
		// behind is a few bytes, and the next write makes its own.
		_ = os.Remove(tmp.Name()) //nolint:errcheck // See above, the write error is the one that matters.
	}
	//nolint:wrapcheck // The error of the rename names both files, and the callers only report it.
	return err
}

func sameTarget(a, b string) bool { return filepath.Clean(a) == filepath.Clean(b) }

// withoutTarget returns the entries minus the one for target.
func withoutTarget(entries []recordEntry, target string) []recordEntry {
	return slices.DeleteFunc(slices.Clone(entries), func(e recordEntry) bool {
		return sameTarget(e.Target, target)
	})
}

// recorder keeps the record of the mounts of one client. An empty path keeps none. A
// failure is logged and never fails the mount: the mount works without the record, and
// only the clean-up of a crash would not find it.
type recorder struct {
	path string
	log  *slog.Logger
}

// leftBehind reports whether the record holds a mount for target that the mount table still
// lists. A target is compared as the mount layer records it, so the caller resolves the
// folder that holds it first. A record or a mount table that cannot be read is logged and
// answers no, because a mount that works without the record is worth more than the check.
func (r recorder) leftBehind(env environment, target string) bool {
	if r.path == "" {
		return false
	}
	entries, err := loadRecord(r.path)
	if err != nil {
		r.log.Warn("record of mounts ignored", slog.Any("err", err))
		return false
	}
	i := slices.IndexFunc(entries, func(e recordEntry) bool { return sameTarget(e.Target, target) })
	if i < 0 {
		return false
	}
	listed, err := env.isMounted(entries[i].Target, entries[i].From)
	if err != nil {
		r.log.Warn("mount table not read", slog.Any("err", err))
		return false
	}
	return listed
}

// add records e. A mount for a target that is already recorded replaces that entry,
// because the earlier mount is gone or the kernel would not have accepted this one.
func (r recorder) add(e recordEntry) {
	r.change(func(entries []recordEntry) ([]recordEntry, bool) {
		return append(withoutTarget(entries, e.Target), e), true
	})
}

// remove takes the entry for target out.
func (r recorder) remove(target string) {
	r.change(func(entries []recordEntry) ([]recordEntry, bool) {
		kept := withoutTarget(entries, target)
		return kept, len(kept) != len(entries)
	})
}

// change reads the record, applies edit and writes the result when edit says it changed.
func (r recorder) change(edit func([]recordEntry) ([]recordEntry, bool)) {
	if r.path == "" {
		return
	}
	recordMu.Lock()
	defer recordMu.Unlock()
	entries, err := loadRecord(r.path)
	damaged := errors.Is(err, ErrRecordDamaged)
	if damaged {
		r.log.Warn("record of mounts replaced", slog.Any("err", err))
	}
	if err != nil && !damaged {
		r.log.Warn("record of mounts not read", slog.Any("err", err))
		return
	}
	entries, write := edit(entries)
	if !write {
		return
	}
	if err := saveRecord(r.path, entries); err != nil {
		r.log.Warn("record of mounts not saved", slog.String("file", r.path), slog.Any("err", err))
	}
}

// ClearStale removes the NFS mounts in the record that a client that died left behind.
// It looks at the entries for targets, or at all of them when targets is empty, and
// returns one result per entry looked at, in the order of the record. Entries that are
// cleared or already gone are dropped from the record; the others stay.
//
// A mount is stale when the mount table lists it and nothing accepts a connection on its
// recorded port. A damaged record gives an error that matches ErrRecordDamaged and
// changes nothing. An error from writing the changed record comes with the results,
// which are still true.
//
// Nothing here reads from a target: a stale mount blocks every call that touches it. The
// mount table is read with a call that never contacts the server, and targets are
// compared as written, so a target must be given in the form mount_nfs got it, with
// symbolic links resolved.
func ClearStale(ctx context.Context, recordPath string, targets []string) ([]Cleared, error) {
	return clearWith(ctx, systemEnvironment(), recordPath, targets)
}

func clearWith(ctx context.Context, env environment, recordPath string, targets []string) ([]Cleared, error) {
	if recordPath == "" {
		return nil, nil
	}
	recordMu.Lock()
	defer recordMu.Unlock()
	entries, err := loadRecord(recordPath)
	if err != nil {
		return nil, err
	}
	var results []Cleared
	kept := make([]recordEntry, 0, len(entries))
	for _, entry := range entries {
		asked := len(targets) == 0 || slices.ContainsFunc(targets, func(t string) bool { return sameTarget(t, entry.Target) })
		if !asked {
			kept = append(kept, entry)
			continue
		}
		result := clearOne(ctx, env, entry)
		results = append(results, result)
		if result.Outcome == OutcomeServed || result.Outcome == OutcomeFailed {
			kept = append(kept, entry)
		}
	}
	if len(kept) != len(entries) {
		if err := saveRecord(recordPath, kept); err != nil {
			return results, fmt.Errorf("save the record of mounts: %w", err)
		}
	}
	return results, nil
}

func clearOne(ctx context.Context, env environment, entry recordEntry) Cleared {
	failed := func(err error) Cleared { return Cleared{Target: entry.Target, Outcome: OutcomeFailed, Err: err} }
	listed, err := env.isMounted(entry.Target, entry.From)
	if err != nil {
		return failed(fmt.Errorf("read the mount table: %w", err))
	}
	if !listed {
		return Cleared{Target: entry.Target, Outcome: OutcomeGone}
	}
	live, err := env.serves(ctx, entry.Port)
	if err != nil {
		return failed(err)
	}
	if live {
		return Cleared{Target: entry.Target, Outcome: OutcomeServed}
	}
	ctx, cancel := context.WithTimeout(ctx, umountTimeout)
	defer cancel()
	if out, err := env.run(ctx, umountPath, "-f", entry.Target); err != nil {
		return failed(unmountError(err, out))
	}
	return Cleared{Target: entry.Target, Outcome: OutcomeCleared}
}

// servesOnLoopback reports whether something accepts a TCP connection on a loopback port.
// Only a refusal means nobody is there; any other failure is returned, because removing the
// mount of a client that is alive would cut it off.
func servesOnLoopback(ctx context.Context, port int) (bool, error) {
	dialer := net.Dialer{Timeout: probeTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(loopbackHost, strconv.Itoa(port)))
	if errors.Is(err, syscall.ECONNREFUSED) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check port %d: %w", port, err)
	}
	_ = conn.Close() //nolint:errcheck // The connection only proved that something answers.
	return true, nil
}
