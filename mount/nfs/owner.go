package nfs

import (
	"math"
	"os"
)

// nobody is the id used when the process id does not fit the 32 bits NFS has for it.
const nobody uint32 = 65534

// owner is the user and group every file and directory reports.
type owner struct {
	uid, gid uint32
}

// processOwner is the user and group running the client. Files are read-only for
// everyone, so the owner matters only for how tools display them.
func processOwner() owner {
	return owner{uid: idToUint32(os.Getuid()), gid: idToUint32(os.Getgid())}
}

func idToUint32(id int) uint32 {
	if id < 0 || int64(id) > math.MaxUint32 {
		return nobody
	}
	return uint32(id)
}
