package app

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func listenDatagrams(t *testing.T, name string) *net.UnixConn {
	t.Helper()
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeQuietly(conn) })
	return conn
}

func receive(t *testing.T, conn *net.UnixConn) string {
	t.Helper()
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return string(buf[:n])
}

func TestNotifyWithoutServiceManager(t *testing.T) {
	t.Parallel()
	if err := NotifyFunc("")("READY=1"); err != nil {
		t.Errorf("notify without a socket = %v", err)
	}
}

func TestNotifySendsDatagrams(t *testing.T) {
	t.Parallel()
	conn := listenDatagrams(t, filepath.Join(shortDir(t), "notify.sock"))
	notify := NotifyFunc(conn.LocalAddr().String())
	for _, state := range []string{"READY=1", "STOPPING=1"} {
		if err := notify(state); err != nil {
			t.Fatal(err)
		}
		if got := receive(t, conn); got != state {
			t.Errorf("received %q, want %q", got, state)
		}
	}
}

func TestNotifyAbstractSocket(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("the abstract socket namespace exists on Linux only")
	}
	name := fmt.Sprintf("@splicefs-test-%d", os.Getpid())
	conn := listenDatagrams(t, name)
	if err := NotifyFunc(name)("READY=1"); err != nil {
		t.Fatal(err)
	}
	if got := receive(t, conn); got != "READY=1" {
		t.Errorf("received %q", got)
	}
}

func TestNotifyFailures(t *testing.T) {
	t.Parallel()
	t.Run("nothing listens", func(t *testing.T) {
		t.Parallel()
		err := NotifyFunc(filepath.Join(shortDir(t), "missing.sock"))("READY=1")
		if err == nil || !strings.Contains(err.Error(), "connect to the service manager") {
			t.Errorf("notify = %v", err)
		}
	})
	t.Run("message cannot be sent", func(t *testing.T) {
		t.Parallel()
		conn := listenDatagrams(t, filepath.Join(shortDir(t), "notify.sock"))
		err := NotifyFunc(conn.LocalAddr().String())(strings.Repeat("x", 8<<20))
		if err == nil || !strings.Contains(err.Error(), "notify the service manager") {
			t.Errorf("notify = %v", err)
		}
	})
}
