package core

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// The cluster scenarios: what the client shows when several nodes hold the same share with
// files that agree, differ or are missing. Each one runs against real API handlers
// (apitest), so the rules are checked end to end through the client's merge and version
// choice, without a mount.

const scenarioShare = "media"

// Times of the copies: old, mid and new, as the scenarios name them.
func timeOld() time.Time { return fileTime().Add(-48 * time.Hour) }
func timeMid() time.Time { return fileTime() }
func timeNew() time.Time { return fileTime().Add(48 * time.Hour) }

func copyOf(seed byte, size int, mtime time.Time) fakeFile {
	return fakeFile{data: pattern(seed, size), mtime: mtime}
}

// phase is one state of the nodes and what the client must show in it.
type phase struct {
	files     map[int]map[string]fakeFile // by node, then by path in the share
	emptyDirs map[int][]string            // folders without files, by node
	down      []int                       // nodes that stop before the client starts
	up        []int                       // stopped nodes that start again
	// wantFiles is every file the share must list, with the copies it may read as; with
	// more than one, the client must read the same one every time.
	wantFiles map[string][]fakeFile
	wantDirs  []string // every folder the share must list
}

type scenario struct {
	name    string
	nodes   int
	noShare []int // nodes that refuse the share
	phases  []phase
}

var nodeNames = []string{"a", "b", "c", "d", "e"}

func runScenario(t *testing.T, sc scenario) {
	t.Helper()
	env := newHarness(t, nodeNames[:sc.nodes]...)
	for _, i := range sc.noShare {
		env.fc.nodes[i].deny()
	}
	for number, ph := range sc.phases {
		env.applyPhase(ph)
		if number > 0 {
			env.restartClient(true)
		}
		picked := map[string]fakeFile{}
		env.checkPhase(ph, picked)
		env.restartClient(false)
		env.checkPhase(ph, picked)
		env.restartClient(true)
		env.checkPhase(ph, picked)
	}
}

func (h *harness) applyPhase(ph phase) {
	h.t.Helper()
	for i, files := range ph.files {
		inShare := map[string]fakeFile{}
		for name, stored := range files {
			inShare[scenarioShare+"/"+name] = stored
		}
		h.fc.nodes[i].putAll(inShare)
	}
	for i, dirs := range ph.emptyDirs {
		for _, dir := range dirs {
			h.fc.nodes[i].putEmptyDir(scenarioShare + "/" + dir)
		}
	}
	for _, i := range ph.down {
		h.fc.nodes[i].stop()
	}
	for _, i := range ph.up {
		if err := h.fc.nodes[i].node.Restart(); err != nil {
			h.t.Fatal(err)
		}
	}
}

// restartClient closes the client, as a restart of the program does; with emptyCache the
// next one starts on an empty cache folder.
func (h *harness) restartClient(emptyCache bool) {
	h.t.Helper()
	if h.client != nil {
		if err := h.client.Close(); err != nil {
			h.t.Fatalf("Close: %v", err)
		}
		h.client = nil
	}
	if emptyCache {
		dir := h.t.TempDir()
		h.cfg.Cache.Dir = dir
		h.cfg.Mounts[0].Cache.Dir = dir
	}
}

func (h *harness) checkPhase(ph phase, picked map[string]fakeFile) {
	h.t.Helper()
	fsys := h.view(0)
	files, dirs := walkShare(h.t, fsys)
	if want := slices.Sorted(maps.Keys(ph.wantFiles)); !slices.Equal(files, want) {
		h.t.Errorf("files listed = %q, want %q", files, want)
	}
	if want := slices.Sorted(slices.Values(ph.wantDirs)); !slices.Equal(dirs, want) {
		h.t.Errorf("folders listed = %q, want %q", dirs, want)
	}
	for name, copies := range ph.wantFiles {
		got := readFile(h.t, fsys, scenarioShare+"/"+name)
		match := slices.IndexFunc(copies, func(c fakeFile) bool { return bytes.Equal(c.data, got) })
		if match < 0 {
			h.t.Errorf("%s reads as none of the copies it may be (%d bytes read)", name, len(got))
			continue
		}
		if first, seen := picked[name]; seen && !bytes.Equal(first.data, got) {
			h.t.Errorf("%s reads as another copy than before", name)
		}
		picked[name] = copies[match]
		attr, err := fsys.Stat(context.Background(), scenarioShare+"/"+name)
		if err != nil || attr.Size != int64(len(got)) || !attr.MTime.Equal(copies[match].mtime) {
			h.t.Errorf("Stat %s = %+v, %v; want the size and time of the copy read", name, attr, err)
		}
	}
}

// walkShare lists the share completely, with an explicit stack, and returns the paths of
// its files and of its folders, sorted.
func walkShare(t *testing.T, fsys FS) (files, dirs []string) {
	t.Helper()
	stack := []string{""}
	for len(stack) > 0 {
		dir := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		attrs, err := fsys.ReadDir(context.Background(), strings.TrimSuffix(scenarioShare+"/"+dir, "/"))
		if err != nil {
			t.Fatalf("ReadDir %q: %v", dir, err)
		}
		for _, attr := range attrs {
			full := join(dir, attr.Name)
			if attr.Dir {
				dirs = append(dirs, full)
				stack = append(stack, full)
			} else {
				files = append(files, full)
			}
		}
	}
	slices.Sort(files)
	slices.Sort(dirs)
	return files, dirs
}

func one(stored fakeFile) []fakeFile { return []fakeFile{stored} }

// onEvery holds the same files on each of the given nodes.
func onEvery(files map[string]fakeFile, nodes ...int) map[int]map[string]fakeFile {
	placed := map[int]map[string]fakeFile{}
	for _, i := range nodes {
		placed[i] = files
	}
	return placed
}

// wantAll wants each of the files as its one copy.
func wantAll(files ...map[string]fakeFile) map[string][]fakeFile {
	want := map[string][]fakeFile{}
	for _, set := range files {
		for name, stored := range set {
			want[name] = one(stored)
		}
	}
	return want
}

func TestBasicScenarios(t *testing.T) {
	t.Parallel()
	source := map[string]fakeFile{
		"a.txt":     copyOf(1, 100, timeMid()),
		"sub/b.bin": copyOf(2, 3*testBlockSize+5, timeMid()),
	}
	onlyOne := map[string]fakeFile{"a.txt": copyOf(3, 40, timeMid()), "only-one/x.bin": copyOf(4, testBlockSize, timeMid())}
	onlyTwo := map[string]fakeFile{"b.txt": copyOf(5, 50, timeMid()), "only-two/y.bin": copyOf(6, testBlockSize, timeMid())}
	for _, sc := range []scenario{
		{name: "three servers, the same files", nodes: 3, phases: []phase{{
			files: onEvery(source, 0, 1, 2), wantFiles: wantAll(source), wantDirs: []string{"sub"},
		}}},
		{name: "three in the list, two running, the same files", nodes: 3, phases: []phase{{
			files: onEvery(source, 0, 1), down: []int{2}, wantFiles: wantAll(source), wantDirs: []string{"sub"},
		}}},
		{name: "three in the list, two running, different files", nodes: 3, phases: []phase{{
			files: map[int]map[string]fakeFile{0: onlyOne, 1: onlyTwo}, down: []int{2},
			wantFiles: wantAll(onlyOne, onlyTwo), wantDirs: []string{"only-one", "only-two"},
		}}},
		{name: "three in the list, two running, files on one only", nodes: 3, phases: []phase{{
			files: onEvery(source, 1), down: []int{2}, wantFiles: wantAll(source), wantDirs: []string{"sub"},
		}}},
	} {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			runScenario(t, sc)
		})
	}
}

// A large file the same on every node is read from more than one of them.
func TestLargeFileIsReadFromSeveralNodes(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a", "b", "c")
	data := pattern(7, 16*testBlockSize)
	env.fc.put("media/large.bin", data, timeMid())
	if got := readFile(t, env.view(0), "media/large.bin"); !bytes.Equal(got, data) {
		t.Fatal("large.bin reads wrong")
	}
	serving := 0
	for _, n := range env.fc.nodes {
		if n.count("range") > 0 {
			serving++
		}
	}
	if serving < 2 {
		t.Errorf("%d nodes served large.bin, want at least 2", serving)
	}
}

func TestVersionScenarios(t *testing.T) {
	t.Parallel()
	newA, oldA := copyOf(1, 60, timeNew()), copyOf(2, 11, timeOld())
	oldB, newB := copyOf(3, 11, timeOld()), copyOf(4, 60, timeNew())
	sameOne, sameTwo := copyOf(5, 4*testBlockSize, timeMid()), copyOf(6, 4*testBlockSize, timeMid())
	voteTwo, voteOne := copyOf(7, 30, timeOld()), copyOf(8, 60, timeNew())
	goneOld, goneNew := copyOf(9, 26, timeOld()), copyOf(10, 48, timeNew())
	for _, sc := range []scenario{
		{name: "same name, different contents, the newest wins", nodes: 3, phases: []phase{{
			files: map[int]map[string]fakeFile{
				0: {"doc-a.txt": newA, "doc-b.txt": oldB},
				1: {"doc-a.txt": oldA, "doc-b.txt": newB},
			},
			down:      []int{2},
			wantFiles: map[string][]fakeFile{"doc-a.txt": one(newA), "doc-b.txt": one(newB)},
		}}},
		{name: "same name, same size and time, different contents", nodes: 3, phases: []phase{{
			files:     map[int]map[string]fakeFile{0: {"same.bin": sameOne}, 1: {"same.bin": sameTwo}},
			down:      []int{2},
			wantFiles: map[string][]fakeFile{"same.bin": {sameOne, sameTwo}},
		}}},
		{name: "same name, different contents, most servers win", nodes: 3, phases: []phase{{
			files: map[int]map[string]fakeFile{
				0: {"vote.txt": voteTwo}, 1: {"vote.txt": voteTwo}, 2: {"vote.txt": voteOne},
			},
			wantFiles: map[string][]fakeFile{"vote.txt": one(voteTwo)},
		}}},
		{name: "the server with the chosen copy stops", nodes: 3, phases: []phase{
			{
				files:     map[int]map[string]fakeFile{0: {"gone.txt": goneOld}, 1: {"gone.txt": goneNew}},
				down:      []int{2},
				wantFiles: map[string][]fakeFile{"gone.txt": one(goneNew)},
			},
			{down: []int{1}, wantFiles: map[string][]fakeFile{"gone.txt": one(goneOld)}},
		}},
	} {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			runScenario(t, sc)
		})
	}
}

// Copies that agree in size, time and first block and differ inside later blocks and in
// the last bytes: the tail digest tells them apart, so a read is one copy, never a mix.
func TestTailScenario(t *testing.T) {
	t.Parallel()
	first := copyOf(1, 10*testBlockSize, timeMid())
	other := fakeFile{data: bytes.Clone(first.data), mtime: timeMid()}
	for block := 1; block < 10; block++ {
		other.data[block*testBlockSize+100] ^= 0xff
	}
	other.data[len(other.data)-1] ^= 0xff
	runScenario(t, scenario{name: "copies that differ after the start and at the end", nodes: 3, phases: []phase{{
		files:     map[int]map[string]fakeFile{0: {"end.bin": first}, 1: {"end.bin": other}},
		down:      []int{2},
		wantFiles: map[string][]fakeFile{"end.bin": {first, other}},
	}}})
}

func TestTreeScenarios(t *testing.T) {
	t.Parallel()
	alpha := map[string]fakeFile{
		"alpha/one.txt": copyOf(1, 20, timeMid()), "alpha/sub/two.txt": copyOf(2, 25, timeMid()),
		"alpha/sub/three.bin": copyOf(3, 20000, timeMid()),
	}
	beta := map[string]fakeFile{
		"beta/a.txt": copyOf(4, 18, timeMid()), "beta/b/c.txt": copyOf(5, 19, timeMid()),
		"beta/b/d.bin": copyOf(6, 30000, timeMid()),
	}
	shared := copyOf(7, 29, timeMid())
	partOne := map[string]fakeFile{
		"common/shared.txt": shared, "common/one.txt": copyOf(8, 16, timeMid()),
		"common/deep/a/b/one.bin": copyOf(9, 5000, timeMid()), "top1.txt": copyOf(10, 27, timeMid()),
	}
	partTwo := map[string]fakeFile{
		"common/shared.txt": shared, "common/two.txt": copyOf(11, 16, timeMid()),
		"common/deep/a/c/two.bin": copyOf(12, 7000, timeMid()), "top2.txt": copyOf(13, 27, timeMid()),
	}
	thingFile, inside := copyOf(14, 27, timeMid()), copyOf(15, 36, timeMid())
	other, inner := copyOf(16, 34, timeMid()), copyOf(17, 38, timeMid())
	full := copyOf(18, 35, timeMid())
	for _, sc := range []scenario{
		{name: "trees that do not overlap", nodes: 3, phases: []phase{{
			files: map[int]map[string]fakeFile{0: alpha, 1: beta}, down: []int{2},
			wantFiles: wantAll(alpha, beta), wantDirs: []string{"alpha", "alpha/sub", "beta", "beta/b"},
		}}},
		{name: "trees that partly overlap", nodes: 3, phases: []phase{{
			files: map[int]map[string]fakeFile{0: partOne, 1: partTwo}, down: []int{2},
			wantFiles: wantAll(partOne, partTwo),
			wantDirs:  []string{"common", "common/deep", "common/deep/a", "common/deep/a/b", "common/deep/a/c"},
		}}},
		{name: "a file on one server, a folder on the other", nodes: 3, phases: []phase{
			{
				files: map[int]map[string]fakeFile{
					0: {"thing": thingFile, "other": other}, 1: {"thing/inside.txt": inside, "other": other},
					2: {"other/inner.txt": inner},
				},
				down:      []int{2},
				wantFiles: map[string][]fakeFile{"thing/inside.txt": one(inside), "other": one(other)},
				wantDirs:  []string{"thing"},
			},
			{
				up:        []int{2},
				wantFiles: map[string][]fakeFile{"thing/inside.txt": one(inside), "other": one(other)},
				wantDirs:  []string{"thing"},
			},
		}},
		{name: "an empty folder on one server", nodes: 3, phases: []phase{{
			files: map[int]map[string]fakeFile{0: {"full/x.txt": full}}, emptyDirs: map[int][]string{1: {"empty-dir"}},
			down: []int{2}, wantFiles: map[string][]fakeFile{"full/x.txt": one(full)}, wantDirs: []string{"empty-dir", "full"},
		}}},
		{name: "unusual names", nodes: 3, phases: []phase{unusualNames()}},
		{name: "a folder with many files", nodes: 3, phases: []phase{manyFiles()}},
	} {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			runScenario(t, sc)
		})
	}
}

// unusualNames are names that need care at every layer: a space, bytes outside ASCII, a
// leading dash, 200 characters and a single quote.
func unusualNames() phase {
	names := map[string]fakeFile{}
	for i, name := range []string{
		"with space.txt", "café.txt", "-dash.txt", "long-" + strings.Repeat("0", 191) + ".txt", "it's.txt",
	} {
		names[name] = copyOf(byte(20+i), 30+i, timeMid())
	}
	return phase{files: onEvery(names, 0, 1), down: []int{2}, wantFiles: wantAll(names)}
}

// manyFiles is a folder of 300 names; 100 are on both nodes as the same copy.
func manyFiles() phase {
	first, second, all := map[string]fakeFile{}, map[string]fakeFile{}, map[string]fakeFile{}
	for number := 1; number <= 300; number++ {
		name := fmt.Sprintf("f%03d", number)
		stored := copyOf(byte(number), 1024, timeMid())
		all[name] = stored
		if number <= 200 {
			first[name] = stored
		}
		if number >= 101 {
			second[name] = stored
		}
	}
	return phase{files: map[int]map[string]fakeFile{0: first, 1: second}, down: []int{2}, wantFiles: wantAll(all)}
}

func TestMoreVersionScenarios(t *testing.T) {
	t.Parallel()
	oldF, newF := copyOf(1, 34, timeOld()), copyOf(2, 56, timeNew())
	oldest, newest, middle := copyOf(3, 40, timeOld()), copyOf(4, 66, timeNew()), copyOf(5, 28, timeMid())
	sameOld := copyOf(6, 100<<10, timeOld())
	sameNew := fakeFile{data: sameOld.data, mtime: timeNew()}
	emptyNew, fullOld := copyOf(7, 0, timeNew()), copyOf(8, 200<<10, timeOld())
	emptyOld, fullNew := copyOf(9, 0, timeOld()), copyOf(10, 200<<10, timeNew())
	movie := copyOf(11, 3*testBlockSize, timeOld())
	short := fakeFile{data: movie.data[:testBlockSize], mtime: timeNew()}
	only := copyOf(12, 29, timeMid())
	for _, sc := range []scenario{
		{name: "the newer copy is on most servers", nodes: 3, phases: []phase{{
			files:     map[int]map[string]fakeFile{0: {"f.txt": oldF}, 1: {"f.txt": newF}, 2: {"f.txt": newF}},
			wantFiles: map[string][]fakeFile{"f.txt": one(newF)},
		}}},
		{name: "three servers, three copies", nodes: 3, phases: []phase{{
			files:     map[int]map[string]fakeFile{0: {"three.txt": oldest}, 1: {"three.txt": newest}, 2: {"three.txt": middle}},
			wantFiles: map[string][]fakeFile{"three.txt": one(newest)},
		}}},
		{name: "the same content with different times", nodes: 3, phases: []phase{{
			files:     map[int]map[string]fakeFile{0: {"same.bin": sameOld}, 1: {"same.bin": sameNew}},
			down:      []int{2},
			wantFiles: map[string][]fakeFile{"same.bin": one(sameNew)},
		}}},
		{name: "an empty copy and a full copy", nodes: 3, phases: []phase{{
			files: map[int]map[string]fakeFile{
				0: {"a.bin": emptyNew, "b.bin": emptyOld}, 1: {"a.bin": fullOld, "b.bin": fullNew},
			},
			down:      []int{2},
			wantFiles: map[string][]fakeFile{"a.bin": one(emptyNew), "b.bin": one(fullNew)},
		}}},
		// The hazard of trusting size and time: a short copy with a newer time wins a tie,
		// and loses once a third node holds the complete copy.
		{name: "a copy cut short by a failed transfer", nodes: 3, phases: []phase{
			{
				files:     map[int]map[string]fakeFile{0: {"movie.bin": movie}, 1: {"movie.bin": short}},
				down:      []int{2},
				wantFiles: map[string][]fakeFile{"movie.bin": one(short)},
			},
			{
				files: map[int]map[string]fakeFile{2: {"movie.bin": movie}}, up: []int{2},
				wantFiles: map[string][]fakeFile{"movie.bin": one(movie)},
			},
		}},
		{name: "one server has the file, the other an empty folder", nodes: 3, phases: []phase{{
			files: map[int]map[string]fakeFile{1: {"docs/only.txt": only}}, emptyDirs: map[int][]string{0: {"docs"}},
			down: []int{2}, wantFiles: map[string][]fakeFile{"docs/only.txt": one(only)}, wantDirs: []string{"docs"},
		}}},
	} {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			runScenario(t, sc)
		})
	}
}

func TestFailureScenarios(t *testing.T) {
	t.Parallel()
	readme := map[string]fakeFile{
		"readme.txt": copyOf(1, 28, timeMid()), "docs/a.bin": copyOf(2, 100000, timeMid()),
		"docs/deep/b.bin": copyOf(3, 65536, timeMid()), "other/c.bin": copyOf(4, 4096, timeMid()),
	}
	left := map[string]fakeFile{
		"left/a.txt": copyOf(5, 26, timeMid()), "left/deep/b.bin": copyOf(6, 50000, timeMid()),
		"joint/one.txt": copyOf(7, 27, timeMid()),
	}
	right := map[string]fakeFile{
		"right/c.txt": copyOf(8, 26, timeMid()), "right/deep/d.bin": copyOf(9, 70000, timeMid()),
		"joint/two.txt": copyOf(10, 27, timeMid()),
	}
	for _, sc := range []scenario{
		{name: "one server does not have the share", nodes: 2, noShare: []int{1}, phases: []phase{{
			files: onEvery(readme, 0), wantFiles: wantAll(readme), wantDirs: []string{"docs", "docs/deep", "other"},
		}}},
		{name: "two servers in a list of five", nodes: 5, phases: []phase{{
			files: map[int]map[string]fakeFile{0: left, 1: right}, down: []int{2, 3, 4},
			wantFiles: wantAll(left, right),
			wantDirs:  []string{"joint", "left", "left/deep", "right", "right/deep"},
		}}},
	} {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			runScenario(t, sc)
		})
	}
}

// TestGeneratedScenarios checks a generated mix of paths, held by different sets of nodes in
// up to three versions, against the merge rules computed here on their own: the version most
// running nodes hold wins, on a tie the newest time. Then the same plan with one node stopped.
func TestGeneratedScenarios(t *testing.T) {
	t.Parallel()
	plan := generatePlan(20261114, 60)
	runScenario(t, scenario{name: "a generated mix of files on three servers", nodes: 3, phases: []phase{
		{files: plan.placed(), wantFiles: plan.want(0, 1, 2), wantDirs: plan.dirs(0, 1, 2)},
		{down: []int{2}, wantFiles: plan.want(0, 1), wantDirs: plan.dirs(0, 1)},
	}})
}
