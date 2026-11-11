//go:build linux

package fuse

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// writeScript writes an executable shell script and returns its path. The tests that
// use it do not run in parallel: executing a file another goroutine may still hold open
// for writing can fail with ETXTBSY.
func writeScript(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func helperHost(unmountErr error, helper func() (string, error)) kernelHost {
	return kernelHost{
		unmountCall: func(string, int) error { return unmountErr },
		findHelper:  helper,
	}
}

func TestKernelHostStat(t *testing.T) {
	t.Parallel()
	info, err := newKernelHost().Stat(t.TempDir())
	if err != nil || !info.IsDir() {
		t.Fatalf("Stat = %v, %v", info, err)
	}
	if _, err := newKernelHost().Stat(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat of a missing path = %v, want ErrNotExist", err)
	}
}

func TestKernelHostUnmountPassesFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		detach    bool
		wantFlags int
	}{
		{"plain", false, 0},
		{"detach", true, syscall.MNT_DETACH},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var gotTarget string
			var gotFlags int
			h := kernelHost{unmountCall: func(target string, flags int) error {
				gotTarget, gotFlags = target, flags
				return nil
			}}
			if err := h.Unmount(context.Background(), "/mnt/x", tt.detach); err != nil {
				t.Fatal(err)
			}
			if gotTarget != "/mnt/x" || gotFlags != tt.wantFlags {
				t.Fatalf("umount2(%q, %d), want (%q, %d)", gotTarget, gotFlags, "/mnt/x", tt.wantFlags)
			}
		})
	}
}

func TestKernelHostUnmountErrorKeepsTheErrno(t *testing.T) {
	t.Parallel()
	h := helperHost(syscall.EBUSY, func() (string, error) {
		t.Error("helper used although the system call did not fail with EPERM")
		return "", nil
	})
	if err := h.Unmount(context.Background(), "/mnt/x", false); !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("error = %v, want one wrapping EBUSY", err)
	}
}

func TestKernelHostUnmountWithoutPrivilegeUsesTheHelper(t *testing.T) {
	tests := []struct {
		name     string
		detach   bool
		wantArgs string
	}{
		{"plain", false, "-u -- /mnt/x"},
		{"detach", true, "-u -z -- /mnt/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			helper := writeScript(t, "fusermount3", `echo "$@" > "$0.args"`)
			h := helperHost(syscall.EPERM, func() (string, error) { return helper, nil })
			if err := h.Unmount(context.Background(), "/mnt/x", tt.detach); err != nil {
				t.Fatal(err)
			}
			args, err := os.ReadFile(helper + ".args")
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(args)); got != tt.wantArgs {
				t.Fatalf("helper arguments = %q, want %q", got, tt.wantArgs)
			}
		})
	}
}

func TestKernelHostHelperFailures(t *testing.T) {
	tests := []struct {
		name     string
		script   string
		wantBusy bool
		wantText string
	}{
		{
			name:     "busy mount",
			script:   `echo "fusermount3: failed to unmount /mnt/x: Device or resource busy" >&2; exit 1`,
			wantBusy: true,
			wantText: "Device or resource busy",
		},
		{
			name:     "other failure",
			script:   `echo "fusermount3: entry for /mnt/x not found in /etc/mtab" >&2; exit 1`,
			wantText: "not found in /etc/mtab",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			helper := writeScript(t, "fusermount3", tt.script)
			h := helperHost(syscall.EPERM, func() (string, error) { return helper, nil })
			err := h.Unmount(context.Background(), "/mnt/x", false)
			if err == nil {
				t.Fatal("no error")
			}
			if got := errors.Is(err, syscall.EBUSY); got != tt.wantBusy {
				t.Fatalf("errors.Is(EBUSY) = %v, want %v (%v)", got, tt.wantBusy, err)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("error %q does not carry the helper message %q", err, tt.wantText)
			}
		})
	}
}

func TestKernelHostWithoutHelper(t *testing.T) {
	t.Parallel()
	missing := errors.New("no helper")
	h := helperHost(syscall.EPERM, func() (string, error) { return "", missing })
	if err := h.Unmount(context.Background(), "/mnt/x", false); !errors.Is(err, missing) {
		t.Fatalf("error = %v, want the lookup error", err)
	}
}

func TestFindFusermount(t *testing.T) {
	tests := []struct {
		name    string
		scripts []string
		want    string // base name of the program found; empty means an error
	}{
		{"fusermount3 is preferred", []string{"fusermount", "fusermount3"}, "fusermount3"},
		{"fusermount is the fallback", []string{"fusermount"}, "fusermount"},
		{"none", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range tt.scripts {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", dir)
			got, err := findFusermount()
			if tt.want == "" {
				if err == nil {
					t.Fatalf("found %q, want an error", got)
				}
				return
			}
			if err != nil || filepath.Base(got) != tt.want {
				t.Fatalf("findFusermount = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}
