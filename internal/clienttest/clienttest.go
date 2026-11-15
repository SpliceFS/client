// Package clienttest holds the doubles for the parts of the client that need a cluster or a
// kernel: tests of the programs use them to run the client without either.
package clienttest

import (
	"context"
	"fmt"
	"sync"
	"syscall"

	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/core"
)

// Core is the core client with no cluster behind it. FS is what every mount shows.
type Core struct{ FS core.FS }

// Connect succeeds without a cluster.
func (Core) Connect(context.Context, string) error { return nil }

// View returns the filesystem of the double.
//
//nolint:ireturn // implements the client's View, which returns the interface
func (c Core) View(config.Mount) (core.FS, error) { return c.FS, nil }

// Status reports one cluster with one measured node and one cache.
func (Core) Status() core.Status {
	east := core.NodeStatus{Name: "east", BytesPerSecond: 12_000_000, Measured: true}
	return core.Status{
		Clusters: []core.ClusterStatus{{Name: "example", Nodes: []core.NodeStatus{east}}},
		Caches:   []core.CacheStatus{{Dir: "/var/cache/splicefs", Entries: 3, Bytes: 4096}},
	}
}

// Apply accepts every configuration.
func (Core) Apply(*config.Config) error { return nil }

// Release does nothing.
func (Core) Release(core.FS) {}

// Current is true so that a reload would leave the mounts of a test as they are.
func (Core) Current(core.FS) bool { return true }

// Close does nothing.
func (Core) Close() error { return nil }

// Mount is a mount that is gone as soon as it is unmounted.
type Mount struct {
	done chan struct{}
	once sync.Once

	mu    sync.Mutex
	calls []bool
	busy  bool
}

// NewMount returns a mount that has not ended.
func NewMount() *Mount { return &Mount{done: make(chan struct{})} }

// Done is closed once the mount was unmounted.
func (m *Mount) Done() <-chan struct{} { return m.done }

// Err is nil: the mount only ends by being unmounted.
func (*Mount) Err() error { return nil }

// Unmount records the call. A busy mount refuses a polite unmount, as a mount with open
// files does.
//
//nolint:revive // flag-parameter: implements the mount layer's Unmount, which takes force
func (m *Mount) Unmount(_ context.Context, force bool) error {
	m.mu.Lock()
	m.calls = append(m.calls, force)
	busy := m.busy
	m.mu.Unlock()
	if busy && !force {
		return fmt.Errorf("unmount: %w", syscall.EBUSY)
	}
	m.once.Do(func() { close(m.done) })
	return nil
}

// SetBusy makes the mount refuse a polite unmount.
func (m *Mount) SetBusy(busy bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.busy = busy
}

// UnmountCalls returns the force argument of every Unmount call so far.
func (m *Mount) UnmountCalls() []bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]bool(nil), m.calls...)
}
