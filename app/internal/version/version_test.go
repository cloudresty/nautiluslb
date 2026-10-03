package version

import (
	"runtime"
	"testing"
)

func TestString(t *testing.T) {
	oldV, oldC, oldD := Version, Commit, BuildDate
	defer func() { Version, Commit, BuildDate = oldV, oldC, oldD }()

	Version, Commit, BuildDate = "v1.0.0", "abc1234", "2026-01-02"
	want := "nautiluslb v1.0.0 (abc1234, 2026-01-02, " + runtime.Version() + ")"
	if got := String(); got != want {
		t.Errorf("String() = %q; want %q", got, want)
	}
}
