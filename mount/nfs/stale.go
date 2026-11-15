package nfs

import (
	"errors"
	"path/filepath"
)

// This file holds what callers see of the clean-up of stale mounts, so that code that
// runs on every system can use it. Only macOS has anything to clean up: see ClearStale.

// ErrRecordDamaged is returned, wrapped, when the record of mounts exists but cannot be
// understood. Mounting treats such a record as empty and replaces it at the next write.
var ErrRecordDamaged = errors.New("the record of mounts cannot be understood")

// Outcome says what ClearStale did with one recorded mount.
type Outcome int

const (
	// OutcomeCleared means the mount was stale and umount removed it.
	OutcomeCleared Outcome = iota + 1
	// OutcomeGone means the mount table no longer lists the mount; only the entry was dropped.
	OutcomeGone
	// OutcomeServed means something still accepts connections on the recorded port, so the mount
	// and the entry were left alone.
	OutcomeServed
	// OutcomeFailed means the mount could not be judged or removed; Err says why, and the
	// entry stays.
	OutcomeFailed
)

// Cleared is what happened to one recorded mount.
type Cleared struct {
	// Target is the directory of the mount, as recorded.
	Target  string
	Outcome Outcome
	// Err is set when Outcome is OutcomeFailed.
	Err error
}

// ResolveTarget returns target the way the mount layer records it, with the symbolic links
// in the folder that holds it resolved (macOS turns /tmp/x into /private/tmp/x). The
// target itself is never looked at, because a stale mount blocks every call that touches
// it. A folder that cannot be resolved leaves the target as it is, cleaned.
func ResolveTarget(target string) string {
	// Cleaned first, so that a trailing slash does not make the target its own folder.
	target = filepath.Clean(target)
	parent, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return target
	}
	return filepath.Join(parent, filepath.Base(target))
}
