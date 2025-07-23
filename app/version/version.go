package version

import (
	"fmt"
	"runtime"
)

// Build information. Populated at build-time via ldflags.
var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildTime = "unknown"
	GitTag    = "dev"
)

// BuildInfo returns detailed build information
type BuildInfo struct {
	Version   string
	GitCommit string
	BuildTime string
	GitTag    string
	GoVersion string
	Platform  string
}

// Get returns build information
func Get() BuildInfo {
	return BuildInfo{
		Version:   Version,
		GitCommit: GitCommit,
		BuildTime: BuildTime,
		GitTag:    GitTag,
		GoVersion: runtime.Version(),
		Platform:  fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
	}
}

// String returns a formatted version string
func (b BuildInfo) String() string {
	if b.GitTag != "dev" && b.GitTag != "" {
		return b.GitTag
	}
	return b.Version
}

// DetailedString returns detailed build information
func (b BuildInfo) DetailedString() string {
	return fmt.Sprintf(`Version: %s
Git Commit: %s
Build Time: %s
Go Version: %s
Platform: %s`,
		b.String(),
		b.GitCommit,
		b.BuildTime,
		b.GoVersion,
		b.Platform,
	)
}
