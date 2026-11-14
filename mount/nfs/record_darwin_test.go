//go:build darwin

package nfs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/SpliceFS/client/mount"
)

// The targets of the clean-up tests exist nowhere on disk. The fake system has no hook for
// a stat of a target, but ClearStale must never touch one, so any such call would show up
// as an error for these paths.
const staleDir = "/nonexistent-splicefs-test"

func recordIn(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "client.nfs.json")
}

// startRecorded is startMount for a mount that writes a record. A mount left running at
// the end of a test writes again when its server stops, which must finish before the folder
// of the record is removed, so the cleanup stops the server and waits for the mount to end.
func startRecorded(t *testing.T, sys *fakeSystem, opts mount.Options) *mounted {
	t.Helper()
	live := startMount(t, sys, opts)
	t.Cleanup(func() {
		live.srv.Close()
		<-live.Done()
	})
	return live
}

func readEntries(t *testing.T, path string) []recordEntry {
	t.Helper()
	entries, err := loadRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func targetsOf(entries []recordEntry) []string {
	targets := make([]string, 0, len(entries))
	for _, e := range entries {
		targets = append(targets, e.Target)
	}
	return targets
}

func wantTargets(t *testing.T, path string, want ...string) {
	t.Helper()
	got := targetsOf(readEntries(t, path))
	if !slices.Equal(got, want) {
		t.Errorf("recorded targets = %q, want %q", got, want)
	}
}

func logged(buf *bytes.Buffer, mu *sync.Mutex) string {
	mu.Lock()
	defer mu.Unlock()
	return buf.String()
}

func TestMountRecordsTheMount(t *testing.T) {
	t.Parallel()
	path := recordIn(t)
	before := time.Now().Add(-time.Minute)
	live := startRecorded(t, newFakeSystem(), mount.Options{Source: "splicefs:example/media", RecordPath: path})

	entries := readEntries(t, path)
	if len(entries) != 1 {
		t.Fatalf("%d entries recorded, want 1", len(entries))
	}
	got := entries[0]
	created := got.Created
	got.Created = time.Time{}
	want := recordEntry{
		Target: live.target, From: "127.0.0.1:/splicefs:example/media", Port: live.srv.Port(), PID: os.Getpid(),
	}
	if got != want {
		t.Errorf("entry = %+v, want %+v", got, want)
	}
	if created.Before(before) || created.After(time.Now()) {
		t.Errorf("created = %v", created)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != recordFileMode {
		t.Errorf("record file = %v, %v, want mode %o", info, err, recordFileMode)
	}
}

func TestRecordFileKeysAreTheDocumentedOnes(t *testing.T) {
	t.Parallel()
	path := recordIn(t)
	startRecorded(t, newFakeSystem(), mount.Options{RecordPath: path})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw []map[string]any
	if err := json.Unmarshal(data, &raw); err != nil || len(raw) != 1 {
		t.Fatalf("record = %s, %v", data, err)
	}
	for _, key := range []string{"target", "from", "port", "pid", "created"} {
		if _, ok := raw[0][key]; !ok {
			t.Errorf("no %q in %s", key, data)
		}
	}
}

func TestUnmountRemovesTheEntry(t *testing.T) {
	t.Parallel()
	path := recordIn(t)
	live := startRecorded(t, newFakeSystem(), mount.Options{RecordPath: path})

	if err := live.Unmount(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	wantTargets(t, path)
	if data, err := os.ReadFile(path); err != nil || string(data) != "[]" {
		t.Errorf("record = %q, %v, want an empty list", data, err)
	}
}

func TestOneUnmountKeepsTheEntryOfTheOtherMount(t *testing.T) {
	t.Parallel()
	path := recordIn(t)
	sys := newFakeSystem()
	first := startRecorded(t, sys, mount.Options{RecordPath: path})
	second := startRecorded(t, sys, mount.Options{RecordPath: path})
	wantTargets(t, path, first.target, second.target)

	if err := first.Unmount(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	wantTargets(t, path, second.target)
}

func TestMountWithoutARecordPathKeepsNoRecord(t *testing.T) {
	t.Parallel()
	live := startRecorded(t, newFakeSystem(), mount.Options{})
	if err := live.Unmount(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	// The target is the only folder the test knows; a record would have no other place to go.
	if entries, err := os.ReadDir(live.target); err != nil || len(entries) != 0 {
		t.Errorf("files = %v, %v", entries, err)
	}
}

func TestMountOfARecordedTargetReplacesItsEntry(t *testing.T) {
	t.Parallel()
	path := recordIn(t)
	target := t.TempDir()
	ports := make([]int, 0, 2)
	for range 2 {
		// The mount table lists nothing, so the first mount is gone and the target is free again.
		sys := newFakeSystem()
		sys.table = map[string]string{}
		mnt, err := mountWith(newFakeFS(), mount.Options{Target: target, RecordPath: path}, sys.environment())
		if err != nil {
			t.Fatal(err)
		}
		live := asMounted(t, mnt)
		t.Cleanup(func() {
			live.srv.Close()
			<-live.Done()
		})
		ports = append(ports, live.srv.Port())
	}
	entries := readEntries(t, path)
	if len(entries) != 1 || entries[0].Port != ports[1] {
		t.Errorf("entries = %+v, want one for the second port %d", entries, ports[1])
	}
}

func TestMountReplacesADamagedRecord(t *testing.T) {
	t.Parallel()
	path := recordIn(t)
	if err := os.WriteFile(path, []byte("{not json"), recordFileMode); err != nil {
		t.Fatal(err)
	}
	log, buf, mu := capturedLog()
	live := startRecorded(t, newFakeSystem(), mount.Options{RecordPath: path, Logger: log})

	wantTargets(t, path, live.target)
	if got := logged(buf, mu); !strings.Contains(got, "record of mounts replaced") {
		t.Errorf("no warning about the damaged record: %s", got)
	}
}

func TestMountWorksWhenTheRecordCannotBeRead(t *testing.T) {
	t.Parallel()
	path := recordIn(t)
	if err := os.Mkdir(path, 0o700); err != nil { // a folder where the file belongs
		t.Fatal(err)
	}
	log, buf, mu := capturedLog()
	live := startRecorded(t, newFakeSystem(), mount.Options{RecordPath: path, Logger: log})
	if err := live.Unmount(t.Context(), false); err != nil {
		t.Fatal(err)
	}

	if got := logged(buf, mu); !strings.Contains(got, "record of mounts not read") {
		t.Errorf("no warning about the unreadable record: %s", got)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Errorf("the record path was changed: %v, %v", info, err)
	}
}

func TestMountWorksWhenTheRecordCannotBeSaved(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "missing", "client.nfs.json")
	log, buf, mu := capturedLog()
	startRecorded(t, newFakeSystem(), mount.Options{RecordPath: path, Logger: log})

	if got := logged(buf, mu); !strings.Contains(got, "record of mounts not saved") {
		t.Errorf("no warning about the failed save: %s", got)
	}
}

func TestUnmountOfAnUnrecordedMountWritesNothing(t *testing.T) {
	t.Parallel()
	path := recordIn(t)
	live := startRecorded(t, newFakeSystem(), mount.Options{RecordPath: path})
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	if err := live.Unmount(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a record was written for nothing: %v", err)
	}
}

func TestEntryGoesWhenTheMountIsUnmountedFromOutside(t *testing.T) {
	t.Parallel()
	path := recordIn(t)
	sys := newFakeSystem()
	sys.presence = []presence{{mounted: true}, {mounted: false}}
	live := startRecorded(t, sys, mount.Options{RecordPath: path})

	sys.ticks <- time.Time{}
	<-live.Done()
	wantTargets(t, path)
}

func TestEntryGoesWhenTheServerFailsAndUmountWorks(t *testing.T) {
	t.Parallel()
	path := recordIn(t)
	live := startRecorded(t, systemWhereUmountAnswers(nil), mount.Options{RecordPath: path})

	breakServer(t, live)
	<-live.Done()
	wantTargets(t, path)
}

func TestEntryStaysWhenTheServerFailsAndUmountFails(t *testing.T) {
	t.Parallel()
	path := recordIn(t)
	live := startRecorded(t, systemWhereUmountAnswers(errors.New("exit status 1")), mount.Options{RecordPath: path})

	breakServer(t, live)
	<-live.Done()
	// The dead mount is still in the system, and the entry is how umount finds it.
	wantTargets(t, path, live.target)
}

func TestLoadRecordOfAMissingFileIsEmpty(t *testing.T) {
	t.Parallel()
	if entries, err := loadRecord(recordIn(t)); entries != nil || err != nil {
		t.Errorf("loadRecord = %v, %v", entries, err)
	}
}

func TestSaveRecordFailures(t *testing.T) {
	t.Parallel()
	dirInTheWay := recordIn(t)
	if err := os.Mkdir(dirInTheWay, 0o700); err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		"folder is missing":        filepath.Join(t.TempDir(), "missing", "client.nfs.json"),
		"a folder is in the place": dirInTheWay,
	}
	for name, path := range tests {
		if err := saveRecord(path, nil); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	left, err := filepath.Glob(filepath.Join(filepath.Dir(dirInTheWay), ".nfs-*"))
	if err != nil || len(left) != 0 {
		t.Errorf("temporary files left behind: %v, %v", left, err)
	}
}

// staleRecord is a record of four mounts, one for each thing ClearStale can find.
func staleRecord(t *testing.T) (string, []recordEntry) {
	t.Helper()
	path := recordIn(t)
	entries := []recordEntry{
		{Target: staleDir + "/stale", From: "127.0.0.1:/splicefs:example/a", Port: 1001, PID: 1},
		{Target: staleDir + "/gone", From: "127.0.0.1:/splicefs:example/b", Port: 1002, PID: 2},
		{Target: staleDir + "/served", From: "127.0.0.1:/splicefs:example/c", Port: 1003, PID: 3},
		{Target: staleDir + "/stuck", From: "127.0.0.1:/splicefs:example/d", Port: 1004, PID: 4},
	}
	if err := saveRecord(path, entries); err != nil {
		t.Fatal(err)
	}
	return path, entries
}

// staleSystem lists every recorded mount but the gone one, has a client on port 1003 and
// lets umount fail for the stuck mount.
func staleSystem(entries []recordEntry) *fakeSystem {
	sys := newFakeSystem()
	sys.table = map[string]string{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Target, "/gone") {
			sys.table[e.Target] = e.From
		}
	}
	sys.serving = func(port int) (bool, error) { return port == 1003, nil }
	sys.handle = func(cmd command) ([]byte, error) {
		if strings.HasSuffix(cmd.args[len(cmd.args)-1], "/stuck") {
			return []byte("umount: Resource busy\n"), errors.New("exit status 1")
		}
		return nil, nil
	}
	return sys
}

func wantResults(t *testing.T, got, want []Cleared) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("results = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Target != want[i].Target || got[i].Outcome != want[i].Outcome ||
			(got[i].Err != nil) != (want[i].Err != nil) {
			t.Errorf("result %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestClearStaleHandlesEachOutcome(t *testing.T) {
	t.Parallel()
	path, entries := staleRecord(t)
	sys := staleSystem(entries)

	results, err := clearWith(t.Context(), sys.environment(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantResults(t, results, []Cleared{
		{Target: entries[0].Target, Outcome: OutcomeCleared},
		{Target: entries[1].Target, Outcome: OutcomeGone},
		{Target: entries[2].Target, Outcome: OutcomeServed},
		{Target: entries[3].Target, Outcome: OutcomeFailed, Err: errors.New("umount")},
	})
	if !errors.Is(results[3].Err, syscall.EBUSY) || !strings.Contains(results[3].Err.Error(), "Resource busy") {
		t.Errorf("the failure of the stuck mount = %v, want what umount said", results[3].Err)
	}
	// Only the stale and the stuck mount were forced off. The one a client serves was left alone.
	forced := make([]string, 0, 2)
	for _, cmd := range sys.commandsNamed(umountPath) {
		if cmd.args[0] != "-f" {
			t.Errorf("umount without -f: %q", cmd.args)
		}
		forced = append(forced, cmd.args[1])
	}
	if want := []string{entries[0].Target, entries[3].Target}; !slices.Equal(forced, want) {
		t.Errorf("forced unmounts = %q, want %q", forced, want)
	}
	if n := len(sys.commandsNamed(mountNFSPath)); n != 0 {
		t.Errorf("mount_nfs ran %d times", n)
	}
	// The cleared and the gone entries are dropped, the others stay.
	wantTargets(t, path, entries[2].Target, entries[3].Target)
}

func TestClearStaleLooksOnlyAtTheTargetsGiven(t *testing.T) {
	t.Parallel()
	path, entries := staleRecord(t)
	sys := staleSystem(entries)

	// A target is matched as a cleaned path, so a trailing slash still finds it.
	results, err := clearWith(t.Context(), sys.environment(), path,
		[]string{entries[2].Target + "/", entries[0].Target, staleDir + "/not-recorded"})
	if err != nil {
		t.Fatal(err)
	}
	wantResults(t, results, []Cleared{
		{Target: entries[0].Target, Outcome: OutcomeCleared},
		{Target: entries[2].Target, Outcome: OutcomeServed},
	})
	if n := len(sys.commandsNamed(umountPath)); n != 1 {
		t.Errorf("umount ran %d times, want only for the stale mount asked for", n)
	}
	wantTargets(t, path, entries[1].Target, entries[2].Target, entries[3].Target)
}

func TestClearStaleChecksTheSourceAsWellAsTheTarget(t *testing.T) {
	t.Parallel()
	path, entries := staleRecord(t)
	sys := newFakeSystem()
	// Something else is mounted on the recorded target now.
	sys.table = map[string]string{entries[0].Target: "127.0.0.1:/other"}

	results, err := clearWith(t.Context(), sys.environment(), path, []string{entries[0].Target})
	if err != nil {
		t.Fatal(err)
	}
	wantResults(t, results, []Cleared{{Target: entries[0].Target, Outcome: OutcomeGone}})
	if len(sys.commands) != 0 {
		t.Errorf("commands ran: %v", sys.commands)
	}
}

func TestClearStaleReportsAProblemWithOneEntryAndGoesOn(t *testing.T) {
	t.Parallel()
	path, entries := staleRecord(t)
	sys := staleSystem(entries)
	env := sys.environment()
	tableFailed := errors.New("table failed")
	probeFailed := errors.New("probe failed")
	isMounted := env.isMounted
	env.isMounted = func(target, from string) (bool, error) {
		if target == entries[0].Target {
			return false, tableFailed
		}
		return isMounted(target, from)
	}
	env.serves = func(_ context.Context, port int) (bool, error) {
		if port == 1003 {
			return false, probeFailed
		}
		return false, nil
	}

	results, err := clearWith(t.Context(), env, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantResults(t, results, []Cleared{
		{Target: entries[0].Target, Outcome: OutcomeFailed, Err: tableFailed},
		{Target: entries[1].Target, Outcome: OutcomeGone},
		{Target: entries[2].Target, Outcome: OutcomeFailed, Err: probeFailed},
		{Target: entries[3].Target, Outcome: OutcomeFailed, Err: errors.New("umount")},
	})
	if !errors.Is(results[0].Err, tableFailed) || !errors.Is(results[2].Err, probeFailed) {
		t.Errorf("errors = %v, %v, want the ones the system gave", results[0].Err, results[2].Err)
	}
	// Nothing was removed, so only the gone mount leaves the record.
	wantTargets(t, path, entries[0].Target, entries[2].Target, entries[3].Target)
}

func TestClearStaleWithoutARecord(t *testing.T) {
	t.Parallel()
	sys := newFakeSystem()
	tests := map[string]string{"no path": "", "no file": recordIn(t)}
	for name, path := range tests {
		results, err := clearWith(t.Context(), sys.environment(), path, nil)
		if results != nil || err != nil {
			t.Errorf("%s: clearStale = %v, %v", name, results, err)
		}
	}
	if len(sys.commands) != 0 {
		t.Errorf("commands ran: %v", sys.commands)
	}
}

func TestClearStaleLeavesADamagedRecordAlone(t *testing.T) {
	t.Parallel()
	path := recordIn(t)
	if err := os.WriteFile(path, []byte("[{"), recordFileMode); err != nil {
		t.Fatal(err)
	}
	results, err := clearWith(t.Context(), newFakeSystem().environment(), path, nil)
	if results != nil || !errors.Is(err, ErrRecordDamaged) {
		t.Errorf("clearStale = %v, %v, want ErrRecordDamaged", results, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "[{" {
		t.Errorf("the damaged record was changed: %q, %v", data, err)
	}
}

func TestClearStaleReportsARecordThatCannotBeRead(t *testing.T) {
	t.Parallel()
	path := recordIn(t)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	results, err := clearWith(t.Context(), newFakeSystem().environment(), path, nil)
	if results != nil || err == nil || errors.Is(err, ErrRecordDamaged) {
		t.Errorf("clearStale = %v, %v, want a read error", results, err)
	}
}

func TestClearStaleReturnsTheResultsWhenTheRecordCannotBeSaved(t *testing.T) {
	t.Parallel()
	path, entries := staleRecord(t)
	sys := staleSystem(entries)
	umount := sys.handle
	sys.handle = func(cmd command) ([]byte, error) {
		// A folder takes the place of the record while umount runs, so the rename fails.
		if err := os.Remove(path); err != nil {
			return nil, err
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			return nil, err
		}
		return umount(cmd)
	}

	results, err := clearWith(t.Context(), sys.environment(), path, []string{entries[0].Target})
	if err == nil || !strings.Contains(err.Error(), "save the record of mounts") {
		t.Errorf("error = %v, want the failed save", err)
	}
	wantResults(t, results, []Cleared{{Target: entries[0].Target, Outcome: OutcomeCleared}})
}

func TestClearStaleWithTheMountTableOfThisMachine(t *testing.T) {
	t.Parallel()
	if results, err := ClearStale(t.Context(), "", nil); results != nil || err != nil {
		t.Errorf("without a record path: %v, %v", results, err)
	}
	path := recordIn(t)
	entry := recordEntry{Target: staleDir + "/media", From: "127.0.0.1:/splicefs:example/media", Port: 1}
	if err := saveRecord(path, []recordEntry{entry}); err != nil {
		t.Fatal(err)
	}
	// The table of this machine does not list the mount, so nothing is probed or unmounted.
	results, err := ClearStale(t.Context(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantResults(t, results, []Cleared{{Target: entry.Target, Outcome: OutcomeGone}})
	wantTargets(t, path)
}

// It does not run in parallel: a port that was just closed can be taken by a listener that
// another test opens at that moment, and the test would see that one answer.
//
//nolint:paralleltest // See above.
func TestServesOnLoopback(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is a %T", listener.Addr())
	}
	port := addr.Port
	if served, err := servesOnLoopback(t.Context(), port); err != nil || !served {
		t.Errorf("with a listener: %v, %v", served, err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if served, err := servesOnLoopback(t.Context(), port); err != nil || served {
		t.Errorf("after the listener closed: %v, %v", served, err)
	}
}

func TestServesOnLoopbackReportsAFailureThatIsNotARefusal(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if served, err := servesOnLoopback(ctx, 1); err == nil || served {
		t.Errorf("with a cancelled context: %v, %v", served, err)
	}
}

// leftBehindSetup records a mount at a target that is a link to nothing, which is what a dead
// mount looks like to a program that touches it, and lists it in the mount table of the fake
// system. Anything that resolved or read the target would fail on it.
func leftBehindSetup(t *testing.T) leftBehind {
	t.Helper()
	target := filepath.Join(t.TempDir(), "dead")
	if err := os.Symlink(filepath.Join(t.TempDir(), "nothing"), target); err != nil {
		t.Fatal(err)
	}
	const from = "127.0.0.1:/splicefs:example/media"
	path := recordIn(t)
	entry := recordEntry{Target: ResolveTarget(target), From: from, Port: 1001, PID: 1}
	if err := saveRecord(path, []recordEntry{entry}); err != nil {
		t.Fatal(err)
	}
	sys := newFakeSystem()
	sys.table = map[string]string{ResolveTarget(target): from}
	return leftBehind{target: target, path: path, sys: sys}
}

// leftBehind is the setup of a mount that a dead client left: its target, the record and the
// fake system.
type leftBehind struct {
	target, path string
	sys          *fakeSystem
}

func TestMountRefusesATargetThatAMountLeftBehindStillHolds(t *testing.T) {
	t.Parallel()
	lb := leftBehindSetup(t)
	target, path, sys := lb.target, lb.path, lb.sys
	before := readEntries(t, path)

	_, err := mountWith(newFakeFS(), mount.Options{Target: target, Source: "splicefs:example/media", RecordPath: path},
		sys.environment())
	if !errors.Is(err, ErrLeftBehind) {
		t.Fatalf("mountWith = %v, want ErrLeftBehind", err)
	}
	want := target + ` is still mounted by a client that is no longer running; run "splicefs umount ` +
		target + `" to remove it`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
	if len(sys.commands) != 0 {
		t.Errorf("commands run: %v, want none", sys.commands)
	}
	if got := readEntries(t, path); !slices.Equal(got, before) {
		t.Errorf("the record changed: %v, want %v", got, before)
	}
}

func TestMountIgnoresARecordedMountOfAnotherTarget(t *testing.T) {
	t.Parallel()
	lb := leftBehindSetup(t)
	live := startRecorded(t, lb.sys, mount.Options{RecordPath: lb.path})
	wantTargets(t, lb.path, ResolveTarget(lb.target), live.target)
}

func TestMountGoesOnWhenTheCheckCannotBeMade(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		damage  func(t *testing.T, path string)
		table   func(sys *fakeSystem)
		warning string
	}{
		"damaged record": {
			damage:  func(t *testing.T, path string) { t.Helper(); writeDamaged(t, path) },
			warning: "record of mounts ignored",
		},
		"unreadable mount table": {
			table:   func(sys *fakeSystem) { sys.table = nil; sys.presence = []presence{{err: errors.New("no table")}} },
			warning: "mount table not read",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			lb := leftBehindSetup(t)
			target, path, sys := lb.target, lb.path, lb.sys
			if tt.damage != nil {
				tt.damage(t, path)
			}
			if tt.table != nil {
				tt.table(sys)
			}
			log, buf, mu := capturedLog()
			// The check names the dead link, so the mount that follows fails on resolving it, and
			// that is after the check let it through.
			_, err := mountWith(newFakeFS(), mount.Options{Target: target, RecordPath: path, Logger: log}, sys.environment())
			if err == nil || errors.Is(err, ErrLeftBehind) || !strings.Contains(err.Error(), "resolve mount target") {
				t.Errorf("mountWith = %v, want the error of the resolved target", err)
			}
			if got := logged(buf, mu); !strings.Contains(got, tt.warning) {
				t.Errorf("no %q warning: %s", tt.warning, got)
			}
		})
	}
}

func writeDamaged(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("{not json"), recordFileMode); err != nil {
		t.Fatal(err)
	}
}

// A client can be killed right after the system accepted its mount. The entry must be in
// the record by then, or nothing could find that mount later.
func TestTheEntryIsRecordedBeforeMountNFSRuns(t *testing.T) {
	t.Parallel()
	path := recordIn(t)
	sys := newFakeSystem()
	var during []string
	sys.handle = func(cmd command) ([]byte, error) {
		// Only the mount is looked at: the unmount at the end of the test comes here too.
		if cmd.name == mountNFSPath {
			during = targetsOf(readEntries(t, path))
		}
		return nil, nil
	}
	live := startRecorded(t, sys, mount.Options{Source: "splicefs:example", RecordPath: path})
	if !slices.Equal(during, []string{live.target}) {
		t.Errorf("recorded targets while mount_nfs ran = %q, want %q", during, live.target)
	}
}

func TestAMountThatMountNFSRefusedLeavesNoEntry(t *testing.T) {
	t.Parallel()
	path := recordIn(t)
	sys := newFakeSystem()
	sys.handle = func(command) ([]byte, error) {
		return []byte("mount_nfs: can't mount\n"), errors.New("exit status 1")
	}
	log, _, _ := capturedLog()
	_, err := mountWith(newFakeFS(), mount.Options{Target: t.TempDir(), Logger: log, RecordPath: path}, sys.environment())
	if err == nil {
		t.Fatal("the mount worked, want the failure of mount_nfs")
	}
	wantTargets(t, path)
}
