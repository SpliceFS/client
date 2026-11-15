//go:build !darwin

package nfs

import "testing"

func TestClearStaleDoesNothingOnThisSystem(t *testing.T) {
	t.Parallel()
	results, err := ClearStale(t.Context(), "/not/a/record.json", []string{"/not/a/mount"})
	if results != nil || err != nil {
		t.Errorf("ClearStale = %v, %v, want nothing", results, err)
	}
}
