package core

import (
	"context"
	"errors"
	"hash/fnv"
	"net/http"
	"slices"
	"testing"
)

func attrNames(attrs []Attr) []string {
	names := make([]string, len(attrs))
	for i, a := range attrs {
		names[i] = a.Name
	}
	return names
}

func TestReadDirMergesListings(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a", "b", "c")
	env.fc.nodes[0].put("media/x", pattern(1, 10), fileTime())
	env.fc.nodes[1].put("media/y", pattern(2, 20), fileTime())
	env.fc.nodes[2].put("media/x", pattern(1, 10), fileTime())
	env.fc.nodes[2].put("media/sub/z", pattern(3, 30), fileTime())
	env.addMount("/media", "/mnt/media")
	ctx := context.Background()

	for _, testCase := range []struct {
		mount int
		dir   string
	}{{0, "media"}, {1, ""}} {
		attrs, err := env.view(testCase.mount).ReadDir(ctx, testCase.dir)
		if err != nil {
			t.Fatalf("ReadDir(%q) in mount %d: %v", testCase.dir, testCase.mount, err)
		}
		if got, want := attrNames(attrs), []string{"sub", "x", "y"}; !slices.Equal(got, want) {
			t.Errorf("ReadDir(%q) in mount %d = %v, want %v", testCase.dir, testCase.mount, got, want)
		}
		if !attrs[0].Dir || attrs[1].Dir || attrs[2].Size != 20 || !attrs[2].MTime.Equal(fileTime()) {
			t.Errorf("ReadDir(%q) in mount %d: wrong attributes %+v", testCase.dir, testCase.mount, attrs)
		}
	}
	shares, err := env.view(0).ReadDir(ctx, "")
	if err != nil || !slices.Equal(attrNames(shares), []string{"media"}) {
		t.Errorf("ReadDir of the root = %v, %v; want [media]", attrNames(shares), err)
	}
}

func TestStatDescribesEntries(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a")
	env.fc.put("media/dir/file", pattern(1, 123), fileTime())
	env.addMount("/media/dir", "/mnt/dir")
	ctx := context.Background()

	root, err := env.view(1).Stat(ctx, "")
	if err != nil || root.Name != "" || !root.Dir {
		t.Errorf("Stat of the view root = %+v, %v; want an unnamed directory", root, err)
	}
	f, err := env.view(1).Stat(ctx, "file")
	if err != nil || f.Name != "file" || f.Dir || f.Size != 123 || !f.MTime.Equal(fileTime()) {
		t.Errorf("Stat(file) = %+v, %v", f, err)
	}
}

func TestStatThroughTheClusterRoot(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a")
	env.fc.put("media/dir/file", pattern(1, 123), fileTime())

	nested, err := env.view(0).Stat(context.Background(), "media/dir/file")
	if err != nil || nested.Name != "file" {
		t.Errorf("Stat(media/dir/file) = %+v, %v", nested, err)
	}
}

func TestRefusedNamesAskNoNode(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a")
	for _, p := range []string{"media/ok", "media/.DS_Store", "media/._ok", "media/.Trashes/f", "media/.config"} {
		env.fc.put(p, pattern(1, 10), fileTime())
	}
	fsys := env.view(0)
	ctx := context.Background()

	refusedPaths := []string{
		".DS_Store", ".hidden", ".localized", ".metadata_never_index", ".Spotlight-V100",
		".fseventsd", ".TemporaryItems", ".apdisk", ".VolumeIcon.icns", "._ok", ".Trash",
		".Trashes", ".Trash-1000", "media/.DS_Store", "media/._ok", "media/.Trashes/f",
	}
	for _, name := range refusedPaths {
		if _, err := fsys.Stat(ctx, name); !errors.Is(err, ErrNotExist) {
			t.Errorf("Stat(%q) = %v, want ErrNotExist", name, err)
		}
		if _, err := fsys.Open(ctx, name); !errors.Is(err, ErrNotExist) {
			t.Errorf("Open(%q) = %v, want ErrNotExist", name, err)
		}
		if _, err := fsys.ReadDir(ctx, name); !errors.Is(err, ErrNotExist) {
			t.Errorf("ReadDir(%q) = %v, want ErrNotExist", name, err)
		}
	}
	if n := env.fc.count("login") + env.fc.count("list"); n != 0 {
		t.Errorf("refused names sent %d requests, want none", n)
	}

	attrs, err := fsys.ReadDir(ctx, "media")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := attrNames(attrs), []string{".config", "ok"}; !slices.Equal(got, want) {
		t.Errorf("ReadDir(media) = %v, want %v", got, want)
	}
}

func TestInodes(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a")
	env.fc.put("media/a", pattern(1, 10), fileTime())
	env.fc.put("media/b", pattern(1, 10), fileTime())
	env.addMount("/media", "/mnt/media")
	ctx := context.Background()

	stat := func(fsys FS, name string) uint64 {
		t.Helper()
		a, err := fsys.Stat(ctx, name)
		if err != nil {
			t.Fatalf("Stat(%q): %v", name, err)
		}
		return a.Inode
	}
	fromRoot, fromMedia := stat(env.view(0), "media/a"), stat(env.view(1), "a")
	if fromRoot != fromMedia {
		t.Errorf("media/a has inode %d in the root view and %d in the media view", fromRoot, fromMedia)
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(testCluster + "\x00media/a\x00f"))
	if want := hash.Sum64(); fromRoot != want {
		t.Errorf("inode of media/a = %d, want FNV-1a %d", fromRoot, want)
	}
	if stat(env.view(0), "media/b") == fromRoot {
		t.Error("two paths share an inode")
	}
	if stat(env.view(1), "") != stat(env.view(0), "media") {
		t.Error("the media view's root and media in the root view differ")
	}

	again := newHarness(t, "a")
	again.fc.put("media/a", pattern(1, 10), fileTime())
	if got := stat(again.view(0), "media/a"); got != fromRoot {
		t.Errorf("after a restart media/a has inode %d, want %d", got, fromRoot)
	}
}

func TestInodeDependsOnTheKind(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a")
	node := env.fc.nodes[0]
	node.put("media/x", pattern(1, 10), fileTime())
	fsys := env.view(0)
	ctx := context.Background()
	asFile, err := fsys.Stat(ctx, "media/x")
	if err != nil || asFile.Dir {
		t.Fatalf("Stat(media/x) = %+v, %v; want a file", asFile, err)
	}

	node.remove("media/x")
	node.put("media/x/y", pattern(1, 10), fileTime())
	env.clock.Advance(testCacheTTL)
	asDir, err := fsys.Stat(ctx, "media/x")
	if err != nil || !asDir.Dir {
		t.Fatalf("Stat(media/x) = %+v, %v; want a directory", asDir, err)
	}
	if asDir.Inode == asFile.Inode {
		t.Error("a file and the directory that replaced it share an inode")
	}
	if root, err := fsys.Stat(ctx, ""); err != nil || root.Inode != inode(testCluster, "", kindDir) {
		t.Errorf("root = %+v, %v; want the cluster root as a directory inode", root, err)
	}
}

func TestRemapReserved(t *testing.T) {
	t.Parallel()
	for in, want := range map[uint64]uint64{0: 2, 1: 3, 2: 2, 1 << 63: 1 << 63} {
		if got := remapReserved(in); got != want {
			t.Errorf("remapReserved(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestLookupErrors(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a")
	h.fc.put("media/file", pattern(1, 10), fileTime())
	fsys := h.view(0)
	ctx := context.Background()

	if _, err := fsys.Stat(ctx, "media/none"); !errors.Is(err, ErrNotExist) {
		t.Errorf("Stat of a missing file = %v, want ErrNotExist", err)
	}
	if _, err := fsys.ReadDir(ctx, "media/file"); !errors.Is(err, ErrNotDir) {
		t.Errorf("ReadDir of a file = %v, want ErrNotDir", err)
	}
	if _, err := fsys.Open(ctx, "media"); !errors.Is(err, ErrIsDir) {
		t.Errorf("Open of a directory = %v, want ErrIsDir", err)
	}
	if _, err := fsys.Open(ctx, "media/none"); !errors.Is(err, ErrNotExist) {
		t.Errorf("Open of a missing file = %v, want ErrNotExist", err)
	}
}

func TestUnreachableIsNotNotFound(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a", "b")
	h.fc.put("media/file", pattern(1, 10), fileTime())
	h.fc.setAllDown()
	fsys := h.view(0)
	ctx := context.Background()

	if _, err := fsys.Stat(ctx, "media/file"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Stat with every node down = %v, want ErrUnavailable", err)
	}
	if _, err := fsys.ReadDir(ctx, "media"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("ReadDir with every node down = %v, want ErrUnavailable", err)
	}
}

func TestCancelledLookupReturnsContextError(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a")
	env.fc.put("media/file", pattern(1, 10), fileTime())
	fsys := env.view(0)
	if _, err := fsys.Stat(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	node := env.fc.nodes[0]
	release := node.hold()
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := fsys.ReadDir(ctx, "media")
		done <- err
	}()
	<-node.waits
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnavailable) {
		t.Errorf("cancelled ReadDir = %v, want only context.Canceled", err)
	}
}

func TestTokenRejectedOnceIsRetried(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a")
	env.fc.put("media/file", pattern(1, 10), fileTime())
	fsys := env.view(0)
	ctx := context.Background()
	if _, err := fsys.Stat(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.ReadDir(ctx, ""); err != nil {
		t.Fatal(err)
	}
	node := env.fc.nodes[0]
	node.rejectNext(1)
	before := node.loginCount()

	a, err := fsys.Stat(ctx, "media/file")
	if err != nil || a.Size != 10 {
		t.Fatalf("Stat after a rejected token = %+v, %v", a, err)
	}
	if got := node.loginCount(); got != before+1 {
		t.Errorf("logins = %d, want %d: the rejected token must be replaced once", got, before+1)
	}
	if got := node.count("list"); got != 3 {
		t.Errorf("list requests = %d, want 3 (root, the rejected one, its retry)", got)
	}
}

func TestForbiddenNodeAnswersNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a", "b")
	h.fc.put("media/file", pattern(1, 10), fileTime())
	h.fc.nodes[1].setStatus(http.StatusForbidden)
	a, err := h.view(0).Stat(context.Background(), "media/file")
	if err != nil || a.Size != 10 {
		t.Errorf("Stat with one forbidden node = %+v, %v", a, err)
	}
}
