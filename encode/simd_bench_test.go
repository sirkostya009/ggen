//go:build goexperiment.simd

package encode

import (
	"strings"
	"testing"
)

func BenchmarkEscapeScan(b *testing.B) {
	for _, n := range []int{64, 256, 2800} {
		s := strings.Repeat("a", n)
		buf := make([]byte, 0, n+8)
		b.Run("scalar_"+strings.Repeat("x", 0)+string(rune('0'+n/100)), func(b *testing.B) {
			for b.Loop() {
				buf = AppendStringNoHTML(buf[:0], s)
			}
		})
		b.Run("avx512_"+string(rune('0'+n/100)), func(b *testing.B) {
			for b.Loop() {
				buf = AppendStringNoHTMLAVX512(buf[:0], s)
			}
		})
	}
}
