package core

import (
	"strings"
	"testing"
	"time"

	"github.com/SpliceFS/common/units"
)

// agingHarness is a harness whose cache evicts files not read for testMaxAge.
func agingHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, "a")
	h.cfg.Mounts[0].Cache.MaxAge = units.Duration(testMaxAge)
	h.fc.put("media/old", pattern(1, testSize), fileTime())
	h.fc.put("media/new", pattern(2, testSize), fileTime())
	return h
}

func (h *harness) cached(path string) bool {
	_, ok := h.cache(0).Peek(cacheKey(path))
	return ok
}

func TestNewEvictsWhatAnEarlierRunLeft(t *testing.T) {
	t.Parallel()
	env := agingHarness(t)
	readFile(t, env.view(0), "media/old")
	if err := env.client.Close(); err != nil {
		t.Fatal(err)
	}
	env.clock.Advance(2 * testMaxAge)
	env.start()
	if env.cached("media/old") {
		t.Error("an entry older than max_age survived the start")
	}
}

func TestEvictionRunsFromOpenAtMostEveryInterval(t *testing.T) {
	t.Parallel()
	env := agingHarness(t)
	fsys := env.view(0)
	readFile(t, fsys, "media/old")

	env.clock.Advance(2 * testMaxAge)
	readFile(t, fsys, "media/new")
	if !env.cached("media/old") {
		t.Fatal("an eviction pass ran within the interval")
	}
	env.clock.Advance(evictInterval)
	opened, err := fsys.Open(t.Context(), "media/new")
	if err != nil {
		t.Fatal(err)
	}
	if env.cached("media/old") {
		t.Error("the open after the interval did not evict the old entry")
	}
	if !env.cached("media/new") {
		t.Error("the entry being opened was evicted")
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEvictionRunsFromClose(t *testing.T) {
	t.Parallel()
	env := agingHarness(t)
	fsys := env.view(0)
	f, err := fsys.Open(t.Context(), "media/old")
	if err != nil {
		t.Fatal(err)
	}
	env.clock.Advance(evictInterval)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if env.cached("media/old") {
		t.Error("the close after the interval did not evict the released entry")
	}
}

func TestEvictionFailureIsLogged(t *testing.T) {
	t.Parallel()
	env := agingHarness(t)
	fsys := env.view(0)
	if err := env.cache(0).Close(); err != nil {
		t.Fatal(err)
	}
	env.clock.Advance(evictInterval + time.Second)
	readFile(t, fsys, "media/new")
	if !strings.Contains(env.logs.String(), "cache eviction failed") {
		t.Errorf("the failed pass was not logged: %s", env.logs.String())
	}
}
