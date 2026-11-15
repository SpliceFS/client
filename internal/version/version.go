// Package version reports the release the SpliceFS programs were built from.
package version

// version is set at build time:
//
//	go build -ldflags "-X github.com/SpliceFS/client/internal/version.version=v1.2.3"
var version = "dev"

// String returns the release version, or "dev" for a build without one.
func String() string {
	return version
}
