// Package version carries build metadata, set at link time with -ldflags -X.
package version

import (
	"fmt"
	"runtime"
)

// Set with -ldflags "-X github.com/cloudresty/nautiluslb/internal/version.Version=...".
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// String renders the build metadata on one line.
func String() string {
	return fmt.Sprintf("nautiluslb %s (%s, %s, %s)", Version, Commit, BuildDate, runtime.Version())
}
