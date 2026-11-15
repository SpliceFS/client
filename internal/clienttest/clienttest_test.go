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
	c := clienttest.Core{}
	if err := c.Connect(context.Background(), "east"); err != nil {
		t.Error(err)
	}
	if fsys, err := c.View(config.Mount{}); fsys != nil || err != nil {
		t.Errorf("View = %v, %v", fsys, err)
	}
	status := c.Status()
	if len(status.Clusters) != 1 || len(status.Caches) != 1 || !status.Clusters[0].Nodes[0].Measured {
		t.Errorf("Status = %+v", status)
	}
	if err := c.Apply(&config.Config{}); err != nil {
		t.Error(err)
	}
	c.Release(nil)
	if !c.Current(nil) {
		t.Error("a reload would leave the mounts as they are only when Current is true")
	}
	if err := c.Close(); err != nil {
		t.Error(err)
	}
}

func TestMountIsGoneOnceUnmounted(t *testing.T) {
	t.Parallel()
	m := clienttest.NewMount()
	if m.Err() != nil {
		t.Error("a stub mount never fails")
	}
	m.SetBusy(true)
	err := m.Unmount(context.Background(), false)
	if !errors.Is(err, syscall.EBUSY) {
		t.Errorf("polite unmount of a busy mount = %v, want EBUSY", err)
	}
	select {
	case <-m.Done():
		t.Fatal("the mount ended although it was busy")
	default:
	}
	if err := m.Unmount(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	<-m.Done()
	m.SetBusy(false)
	if err := m.Unmount(context.Background(), false); err != nil {
		t.Errorf("unmount of a mount that is gone = %v", err)
	}
	if got := m.UnmountCalls(); !slices.Equal(got, []bool{false, true, false}) {
		t.Errorf("unmount calls (force) = %v", got)
	}
}
