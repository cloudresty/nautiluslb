//go:build linux

package pipe

import "testing"

func BenchmarkRunSplice(b *testing.B) { benchRun(b, ModeSplice) }

func BenchmarkCopyThroughputSplice(b *testing.B) { benchCopyThroughput(b, ModeSplice) }
