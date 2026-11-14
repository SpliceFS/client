//go:build !darwin

package nfs

import "context"

// ClearStale does nothing here. A FUSE mount whose client died is cleared by the next
// client start, so no list of mounts is kept and there is nothing to find.
func ClearStale(context.Context, string, []string) ([]Cleared, error) { return nil, nil }
