//go:build linux || freebsd

package fuse

import (
	"sync"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// openFiles tracks the files the kernel has open, so that they can be closed when the
// mount ends. The kernel announces the close of a file with a request that is dropped
// if the mount goes away first, as it does after a forced unmount.
type openFiles struct {
	mu    sync.Mutex
	files map[*handle]struct{}
}

func newOpenFiles() *openFiles {
	return &openFiles{files: make(map[*handle]struct{})}
}

func (o *openFiles) add(h *handle) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.files[h] = struct{}{}
}

func (o *openFiles) remove(h *handle) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.files, h)
}

// closeAll closes every file still open. It is only called once no request can arrive.
func (o *openFiles) closeAll() {
	o.mu.Lock()
	left := o.files
	o.files = make(map[*handle]struct{})
	o.mu.Unlock()
	for h := range left {
		if err := h.file.Close(); err != nil {
			_ = h.mapper.errno(err)
		}
	}
}

// server is a go-fuse server that releases the files of its mount when it stops.
type server struct {
	*fuse.Server
	files *openFiles
}

var _ serveLoop = server{}

// Serve implements serveLoop.
func (s server) Serve() {
	s.Server.Serve()
	s.files.closeAll()
}
