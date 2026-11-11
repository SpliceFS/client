package nfs

import (
	"math"
	"os"
	"testing"
)

func TestProcessOwner(t *testing.T) {
	t.Parallel()
	got := processOwner()
	if int(got.uid) != os.Getuid() || int(got.gid) != os.Getgid() {
		t.Errorf("processOwner() = %+v, want uid %d gid %d", got, os.Getuid(), os.Getgid())
	}
}

func TestIDToUint32(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		id   int
		want uint32
	}{
		{"zero", 0, 0},
		{"ordinary", 501, 501},
		{"largest", math.MaxUint32, math.MaxUint32},
		{"negative", -1, nobody},
		{"too large", math.MaxUint32 + 1, nobody},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := idToUint32(tt.id); got != tt.want {
				t.Errorf("idToUint32(%d) = %d, want %d", tt.id, got, tt.want)
			}
		})
	}
}
