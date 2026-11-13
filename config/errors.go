package config

import "errors"

// The errors below are the reasons a configuration is refused. Each carries only the reason;
// the code that finds the problem puts the key and the value in front of it, and where the
// reason ends in a limit the limit follows it, so that the message an administrator reads
// names what to fix. Callers test for a reason with errors.Is.
var (
	// ErrEmpty is the reason a required value is refused.
	ErrEmpty = errors.New("must not be empty")
	// ErrMultipleDocuments is the reason a file with a second YAML document is refused.
	ErrMultipleDocuments = errors.New("more than one YAML document")
	// ErrNamePattern is the reason a cluster or mount name of the wrong form is refused. The
	// pattern follows it.
	ErrNamePattern = errors.New("name must match")
	// ErrURLNotAbsolute is the reason a cluster URL without scheme or host is refused.
	ErrURLNotAbsolute = errors.New("is not an absolute http or https URL with a host")
	// ErrURLQuery is the reason a cluster URL with a query or fragment is refused.
	ErrURLQuery = errors.New("must not have a query or fragment")
	// ErrBlockSizeRange is the reason a block size outside the limits is refused. The
	// smallest and the largest size follow it.
	ErrBlockSizeRange = errors.New("is not allowed, use a size from")
	// ErrStreamsRange is the reason a number of streams outside the limits is refused. The
	// smallest and the largest number follow it.
	ErrStreamsRange = errors.New("is not between")
	// ErrReadAheadTooSmall is the reason a read-ahead that is unlimited or below one block is
	// refused. The block size follows it.
	ErrReadAheadTooSmall = errors.New("is not allowed, use a finite size of at least one block")
	// ErrUnknownCluster is the reason a mount of a cluster that no file defines is refused.
	ErrUnknownCluster = errors.New("is not a cluster defined in clusters")
	// ErrPathNoSlash is the reason a mount path that does not start with a slash is refused.
	ErrPathNoSlash = errors.New(`must start with "/"`)
	// ErrPathDotDot is the reason a mount path with a ".." segment is refused.
	ErrPathDotDot = errors.New(`must not contain ".."`)
	// ErrNotAbsolute is the reason a directory that is not absolute is refused.
	ErrNotAbsolute = errors.New("is not an absolute path")
	// ErrTargetShared is the reason a second mount at the same target is refused.
	ErrTargetShared = errors.New("is already the target of another mount")
	// ErrTargetInside is the reason a target below the target of another mount is refused.
	// The other target follows it.
	ErrTargetInside = errors.New("is inside the target")
	// ErrTargetContains is the reason a target above the target of another mount is refused.
	// The other target follows it.
	ErrTargetContains = errors.New("contains the target")
	// ErrCacheDirOverlap is the reason a cache directory that is, lies inside or contains a
	// mount target is refused. The target follows it.
	ErrCacheDirOverlap = errors.New("must not be, lie inside or contain the target")
	// ErrMaxSizeTooSmall is the reason a cache size below one block of the largest cluster is
	// refused. The block size follows it.
	ErrMaxSizeTooSmall = errors.New("is smaller than one block of the largest cluster")
	// ErrMaxAge is the reason a maximum age of zero or less is refused.
	ErrMaxAge = errors.New(`is not allowed, use a duration above 0 or "unlimited"`)
	// ErrUnlimitedSize is the reason an unlimited size is refused where only a finite one has
	// a meaning.
	ErrUnlimitedSize = errors.New("unlimited is not allowed, use a finite size")
	// ErrRevalidate is the reason a revalidation time that is unlimited or zero or less is
	// refused.
	ErrRevalidate = errors.New("is not allowed, use a finite duration above 0")
	// ErrListingTTL is the reason a listing time that is unlimited or below zero is refused.
	ErrListingTTL = errors.New("is not allowed, use a finite duration of at least 0")
)
