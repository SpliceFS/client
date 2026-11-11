package fuse

import "time"

// settings are the tunables of one mount. Production code always uses defaultSettings;
// the type exists so that tests can lower the timeouts.
type settings struct {
	// entryTimeout is how long the kernel trusts a name to inode mapping.
	entryTimeout time.Duration
	// attrTimeout is how long the kernel trusts the attributes of an inode.
	attrTimeout time.Duration
	// negativeTimeout is how long the kernel remembers that a name does not exist.
	negativeTimeout time.Duration
}

// defaultSettings returns the timeouts a real mount runs with. File contents never
// change under a given tag, so entries and attributes can live long; a missing name is
// remembered only briefly because a file may appear on a node at any moment.
func defaultSettings() settings {
	return settings{
		entryTimeout:    30 * time.Second,
		attrTimeout:     30 * time.Second,
		negativeTimeout: 5 * time.Second,
	}
}
