package fuse

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// firstOptionalField is the index in a mountinfo line of the first field after the mount
// options. Fields from there up to a lone "-" are optional; the file system type follows
// the "-".
const firstOptionalField = 6

// fuseConnectionID finds the kernel connection number of the FUSE mount at target in the
// content of /proc/self/mountinfo. The kernel numbers a connection after the minor number
// of the anonymous device it gives the mount. The table is read instead of calling stat
// on the mount point: a stat fills the attribute cache of the mount, and a mount whose
// server died would then keep answering from that cache instead of with ENOTCONN. The
// last matching line wins because a later mount on the same point hides an earlier one.
func fuseConnectionID(mountinfo, target string) (uint32, bool) {
	target = filepath.Clean(target)
	var id uint32
	var found bool
	for line := range strings.SplitSeq(mountinfo, "\n") {
		if mnt, ok := parseFuseMount(line); ok && mnt.point == target {
			id, found = mnt.minor, true
		}
	}
	return id, found
}

// fuseMount is the part of a mountinfo line that names a FUSE mount.
type fuseMount struct {
	point string
	minor uint32
}

// parseFuseMount reads one mountinfo line. It answers with ok set only for a FUSE mount,
// and a line it cannot make sense of is not one.
func parseFuseMount(line string) (fuseMount, bool) {
	fields := strings.Fields(line)
	if len(fields) <= firstOptionalField {
		return fuseMount{}, false
	}
	tail := fields[firstOptionalField:]
	sep := slices.Index(tail, "-")
	if sep < 0 || sep+1 >= len(tail) || !strings.HasPrefix(tail[sep+1], "fuse") {
		return fuseMount{}, false
	}
	_, minorText, found := strings.Cut(fields[2], ":")
	if !found {
		return fuseMount{}, false
	}
	n, err := strconv.ParseUint(minorText, 10, 32)
	if err != nil {
		return fuseMount{}, false
	}
	return fuseMount{point: unescapeMountinfo(fields[4]), minor: uint32(n)}, true
}

// unescapeMountinfo undoes the escaping of a mount point: the kernel writes a space, a tab,
// a newline and a backslash as a backslash and three octal digits. Anything that does not
// look like that stays as it is.
func unescapeMountinfo(escaped string) string {
	out := make([]byte, 0, len(escaped))
	for i := 0; i < len(escaped); i++ {
		if escaped[i] == '\\' && i+3 < len(escaped) {
			if b, err := strconv.ParseUint(escaped[i+1:i+4], 8, 8); err == nil {
				out = append(out, byte(b))
				i += 3
				continue
			}
		}
		out = append(out, escaped[i])
	}
	return string(out)
}
