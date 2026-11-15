//go:build unix

package app

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestClaimRefusesADirectoryOfAnotherUser(t *testing.T) {
	t.Parallel()
	sys := systemHost()
	euid := sys.geteuid
	sys.geteuid = func() int { return euid() + 1 }
	_, err := sys.claim(t.Context(), filepath.Join(shortDir(t), "c.sock"))
	if err == nil || !strings.Contains(err.Error(), "belongs to another user") {
		t.Fatalf("claim = %v", err)
	}
}
