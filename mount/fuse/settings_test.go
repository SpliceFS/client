package fuse

import (
	"testing"
	"time"
)

func TestDefaultSettings(t *testing.T) {
	t.Parallel()
	want := settings{
		entryTimeout:    30 * time.Second,
		attrTimeout:     30 * time.Second,
		negativeTimeout: 5 * time.Second,
	}
	if got := defaultSettings(); got != want {
		t.Fatalf("defaultSettings() = %+v, want %+v", got, want)
	}
}
