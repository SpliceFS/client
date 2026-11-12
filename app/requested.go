package app

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/SpliceFS/client/config"
)

// requestedExt replaces the extension of the control socket to name the requested list.
const requestedExt = ".mounts.json"

// requestedPath returns the file of the requested mounts: next to the control socket, in
// the runtime directory, so that it is gone after a reboot, when fstab asks again.
func requestedPath(socketPath string) string {
	return socketPath[:len(socketPath)-len(filepath.Ext(socketPath))] + requestedExt
}

// requested returns the mount a request stands for.
func (m MountRequest) requested() config.Requested {
	return config.Requested{
		Cluster: m.Cluster, Path: m.Path, Target: m.Target,
		AllowOther: m.AllowOther, Offline: m.Offline,
	}
}

func mountRequestOf(r config.Requested) MountRequest {
	return MountRequest{
		Cluster: r.Cluster, Path: r.Path, Target: r.Target,
		AllowOther: r.AllowOther, Offline: r.Offline,
	}
}

// requestedStore is the list of mounts asked for while the client runs. The mount watchers
// change it as well as the run loop, so the mutex guards it. Every change is written to
// the file at once. A nil store is empty and ignores changes, which is what a run built
// without one needs.
type requestedStore struct {
	mu   sync.Mutex
	path string
	log  *slog.Logger
	// items keeps the order of asking, so that the last asked is the first dropped.
	items []config.Requested
}

func newRequestedStore(path string, log *slog.Logger) *requestedStore {
	return &requestedStore{path: path, log: log}
}

// load reads the file. A file that is missing is an empty list, and one that cannot be
// read or understood is logged and treated as empty, because a stale list must never keep
// the client from starting.
func (s *requestedStore) load() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = nil
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	var stored []MountRequest
	if err == nil {
		err = json.Unmarshal(data, &stored)
	}
	if err != nil {
		s.log.Warn("requested mounts ignored", "file", s.path, "err", err)
		return
	}
	for _, m := range stored {
		s.items = append(s.items, m.requested())
	}
}

// add puts r in the list. A mount for a target that is already listed replaces it where
// it stands.
func (s *requestedStore) add(r config.Requested) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.index(r.Target); i >= 0 {
		s.items[i] = r
	} else {
		s.items = append(s.items, r)
	}
	s.save()
}

// remove takes the mount for target out of the list and reports whether it was there.
func (s *requestedStore) remove(target string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(target)
	if i < 0 {
		return false
	}
	s.items = slices.Delete(s.items, i, i+1)
	s.save()
	return true
}

// list returns a copy of the list, oldest first.
func (s *requestedStore) list() []config.Requested {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.items)
}

func (s *requestedStore) index(target string) int {
	want := filepath.Clean(target)
	return slices.IndexFunc(s.items, func(r config.Requested) bool {
		return filepath.Clean(r.Target) == want
	})
}

// save writes the list and logs a failure: the mount itself works without the file, only
// a restart of the client would not bring it back.
func (s *requestedStore) save() {
	if err := s.write(); err != nil {
		s.log.Warn("requested mounts not saved", "file", s.path, "err", err)
	}
}

// write replaces the file through a temporary file in the same directory, so that a
// reader never sees half a list.
func (s *requestedStore) write() error {
	stored := make([]MountRequest, 0, len(s.items))
	for _, r := range s.items {
		stored = append(stored, mountRequestOf(r))
	}
	// A list of strings and booleans always encodes, so there is no error to handle.
	data, _ := json.Marshal(stored)
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".mounts-*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	err = errors.Join(err, tmp.Chmod(0o600), tmp.Close())
	if err == nil {
		err = os.Rename(tmp.Name(), s.path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}
