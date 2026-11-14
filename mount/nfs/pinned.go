package nfs

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/SpliceFS/client/core"
)

// quietTime is how long an opened file is kept after the last READ that used it. go-nfs
// opens the file for every READ request, and every open chooses the version of the file
// again, so without this a program that reads a file while it is replaced on the servers
// would get blocks of two copies. The kernel sends the READs of a program that is reading
// on within milliseconds of each other, so two seconds keeps one open through the whole
// read, and it is short enough that the next program to open a replaced file gets the
// new copy.
const quietTime = 2 * time.Second

// errMountEnded is what an open gets after the mount has ended. The kernel no longer
// asks, so only a request that was already running can see it.
var errMountEnded = errors.New("the mount has ended")

// afterFunc runs f once after d and returns a function that cancels it. The program uses
// time.AfterFunc; tests pass a clock they drive themselves, so none waits for real time.
type afterFunc func(d time.Duration, f func()) (cancel func() bool)

func timeAfterFunc(d time.Duration, f func()) func() bool {
	return time.AfterFunc(d, f).Stop
}

// pin is one core.File kept open for the READs of one path.
type pin struct {
	name  string
	ready chan struct{} // closed when the open has ended: file and err are final from then on
	file  core.File
	err   error

	// The fields below belong to pinnedFiles.mu.
	users   int         // READs that hold the file right now, counting the one that opened it
	retired bool        // no longer found by name: closed when the last user is done
	tick    int         // counts the timers set, so a timer that fires late knows it is old
	cancel  func() bool // the running quiet-time timer, nil while the file is in use
}

// pinnedFiles keeps the core.File of every path that is being read, so that the READs
// that arrive together or one after another share one open and so one version of the
// file. A file is closed after quietTime without a READ, after a failed read, and when
// the mount ends.
type pinnedFiles struct {
	fsys  core.FS
	log   *slog.Logger
	quiet time.Duration
	after afterFunc

	mu     sync.Mutex
	byName map[string]*pin
	ended  bool
}

func newPinnedFiles(fsys core.FS, log *slog.Logger) *pinnedFiles {
	return &pinnedFiles{
		fsys:   fsys,
		log:    log,
		quiet:  quietTime,
		after:  timeAfterFunc,
		byName: make(map[string]*pin),
	}
}

// acquire returns the pin of name, opening the file if no READ holds it. READs that
// ask while the open is running wait for it and share its answer, whether a file or an
// error. Every acquire that succeeds must be followed by one release.
func (s *pinnedFiles) acquire(ctx context.Context, name string) (*pin, error) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return nil, errMountEnded
	}
	if shared, ok := s.byName[name]; ok {
		shared.users++
		shared.stopTimer()
		s.mu.Unlock()
		<-shared.ready
		return shared.result()
	}
	opening := &pin{name: name, ready: make(chan struct{}), users: 1}
	s.byName[name] = opening
	s.mu.Unlock()

	opening.file, opening.err = s.fsys.Open(ctx, name)
	if opening.err != nil {
		// A failed open is not kept: the next READ tries again.
		s.discard(opening)
	}
	close(opening.ready)
	return opening.result()
}

func (p *pin) result() (*pin, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p, nil
}

// release ends one use. The file stays open for quietTime in case another READ comes.
func (s *pinnedFiles) release(used *pin) {
	s.mu.Lock()
	used.users--
	if used.users > 0 {
		s.mu.Unlock()
		return
	}
	if used.retired {
		s.mu.Unlock()
		s.closeFile(used)
		return
	}
	used.tick++
	tick := used.tick
	used.cancel = s.after(s.quiet, func() { s.expire(used, tick) })
	s.mu.Unlock()
}

// expire closes the file of a timer that ran out, unless a READ has used the file since
// the timer was set.
func (s *pinnedFiles) expire(idle *pin, tick int) {
	s.mu.Lock()
	if idle.tick != tick {
		s.mu.Unlock()
		return
	}
	s.retire(idle)
	s.mu.Unlock()
	s.closeFile(idle)
}

// discard stops sharing the file, so the next READ opens it again. The READs that hold it
// finish with it, and the last one closes it. A read that failed calls this: the file
// may have been replaced on the servers, and a program that keeps reading would
// otherwise keep the dead version open for as long as it keeps reading.
func (s *pinnedFiles) discard(dropped *pin) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retire(dropped)
}

// closeAll ends the sharing for good. Idle files are closed now and files in use when the
// last READ that holds them is done.
func (s *pinnedFiles) closeAll() {
	s.mu.Lock()
	s.ended = true
	var idle []*pin
	for _, kept := range s.byName {
		if kept.users == 0 {
			idle = append(idle, kept)
		}
		s.retire(kept)
	}
	s.mu.Unlock()
	for _, kept := range idle {
		s.closeFile(kept)
	}
}

// retire removes the pin from the names and stops its timer. The caller holds mu.
func (s *pinnedFiles) retire(gone *pin) {
	gone.retired = true
	gone.stopTimer()
	if s.byName[gone.name] == gone {
		delete(s.byName, gone.name)
	}
}

// stopTimer cancels the quiet-time timer. Its tick is made old as well, because a
// timer that is already running cannot be stopped. The caller holds the lock of the
// pinnedFiles.
func (p *pin) stopTimer() {
	p.tick++
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
}

func (s *pinnedFiles) closeFile(done *pin) {
	if err := done.file.Close(); err != nil {
		s.log.Warn("close of a file kept open failed", slog.String("name", done.name), slog.Any("err", err))
	}
}
