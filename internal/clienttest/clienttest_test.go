package clienttest_test

import (
	"context"
	"errors"
	"slices"
	"syscall"
	"testing"

	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/internal/clienttest"
)

func TestCoreHasNoClusterBehindIt(t *testing.T) {
	t.Parallel()
	stub := clienttest.Core{}
	if err := stub.Connect(context.Background(), "east"); err != nil {
		t.Error(err)
	}
	if fsys, err := stub.View(config.Mount{}); fsys != nil || err != nil {
		t.Errorf("View = %v, %v", fsys, err)
	}
	status := stub.Status()
	if len(status.Clusters) != 1 || len(status.Caches) != 1 || !status.Clusters[0].Nodes[0].Measured {
		t.Errorf("Status = %+v", status)
	}
	if err := stub.Apply(&config.Config{}); err != nil {
		t.Error(err)
	}
	stub.Release(nil)
	if !stub.Current(nil) {
		t.Error("a reload would leave the mounts as they are only when Current is true")
	}
	if err := stub.Close(); err != nil {
		t.Error(err)
	}
}

func TestMountIsGoneOnceUnmounted(t *testing.T) {
	t.Parallel()
	stub := clienttest.NewMount()
	if stub.Err() != nil {
		t.Error("a stub mount never fails")
	}
	stub.SetBusy(true)
	err := stub.Unmount(context.Background(), false)
	if !errors.Is(err, syscall.EBUSY) {
		t.Errorf("polite unmount of a busy mount = %v, want EBUSY", err)
	}
	select {
	case <-stub.Done():
		t.Fatal("the mount ended although it was busy")
	default:
	}
	if err := stub.Unmount(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	<-stub.Done()
	stub.SetBusy(false)
	if err := stub.Unmount(context.Background(), false); err != nil {
		t.Errorf("unmount of a mount that is gone = %v", err)
	}
	if got := stub.UnmountCalls(); !slices.Equal(got, []bool{false, true, false}) {
		t.Errorf("unmount calls (force) = %v", got)
	}
}
