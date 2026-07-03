// Package version reports the build version of the binary.
package version

import "runtime/debug"

// version is injected at build time via
//
//	-ldflags "-X github.com/muhammetsafak/egresszero/internal/version.version=v1.2.3"
//
// and takes precedence over module build info.
var version = ""

// Version returns the injected build version, falling back to the Go
// module version (populated by `go install ...@vX.Y.Z` and by builds
// from a clean tagged checkout), then to "devel".
func Version() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "devel"
}
