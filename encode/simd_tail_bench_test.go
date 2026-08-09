//go:build goexperiment.simd

package encode

import (
	"fmt"
	"strings"
	"testing"
)

// BenchmarkEscapeTailRem sweeps the sub-lane remainder — the bytes left after
// the last full vector iteration. That leftover is what the tail handles, so
// its cost scales with rem and is invisible at the lane-multiple sizes
// BenchmarkEscapeScan uses (64/256 are rem=0 at every tier).
func BenchmarkEscapeTailRem(b *testing.B) {
	const lane = 64
	for _, rem := range []int{0, 1, 16, 32, 48, 63} {
		n := lane*2 + rem
		s := strings.Repeat("a", n)
		buf := make([]byte, 0, n+8)
		b.Run(fmt.Sprintf("rem%d", rem), func(b *testing.B) {
			b.SetBytes(int64(n))
			for b.Loop() {
				buf = AppendStringNoHTMLAVX512(buf[:0], s)
			}
		})
	}
}
