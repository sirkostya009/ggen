//go:build goexperiment.simd

package scan

import (
	"fmt"
	"strings"
	"testing"
)

// BenchmarkValidUTF8Kernel measures validUTF8x16 alone — no string scan around
// it — across block counts, so the per-call setup (3 LUT loads + broadcasts,
// paid once regardless of length) is separable from the per-block classify.
func BenchmarkValidUTF8Kernel(b *testing.B) {
	for _, n := range []int{2, 8, 16, 17, 32, 64, 256, 4096} {
		// Cyrillic is 2 bytes/rune, so an odd length takes one ASCII byte to
		// land on a rune boundary (a mid-rune cut would just be invalid).
		body := []byte(strings.Repeat("аб", n/2))[:n&^1]
		if n%2 == 1 {
			body = append(body, 'a')
		}
		b.Run(fmt.Sprintf("%dB", n), func(b *testing.B) {
			for b.Loop() {
				if !validUTF8x16(body) {
					b.Fatal("invalid")
				}
			}
		})
	}
}
