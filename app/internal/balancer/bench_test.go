package balancer

import (
	"fmt"
	"testing"

	"github.com/cloudresty/nautiluslb/internal/backend"
)

var algorithms = []string{"round_robin", "least_conn", "source_ip_hash", "random_two_choices"}

func BenchmarkPick(b *testing.B) {
	for _, alg := range algorithms {
		for _, nb := range []int{3, 10, 100} {
			for _, n := range []int{1, 3} {
				b.Run(fmt.Sprintf("%s/backends=%d/n=%d", alg, nb, n), func(b *testing.B) {
					weights := make([]int, nb)
					for i := range weights {
						weights[i] = i%10 + 1
					}
					snap := backend.NewSnapshot(pool(weights...))
					p := mustNew(b, alg, Options{})
					p.Rebuild(snap)
					b.ReportAllocs()
					b.ResetTimer()
					b.RunParallel(func(pb *testing.PB) {
						i := 0
						for pb.Next() {
							i++
							if len(p.Pick(snap, ipKey(i), n)) == 0 {
								b.Fatal("empty pick")
							}
						}
					})
				})
			}
		}
	}
}

func BenchmarkRebuild(b *testing.B) {
	weights := make([]int, 100)
	for i := range weights {
		weights[i] = i%10 + 1
	}
	bs := pool(weights...)
	for _, alg := range algorithms {
		b.Run(alg, func(b *testing.B) {
			p := mustNew(b, alg, Options{})
			b.ReportAllocs()
			for b.Loop() {
				// a fresh snapshot each time defeats any identity-based skip
				p.Rebuild(backend.NewSnapshot(bs))
			}
		})
	}
}
