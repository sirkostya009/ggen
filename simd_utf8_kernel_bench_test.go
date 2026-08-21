//go:build goexperiment.simd

package ggen

import (
	"fmt"
	"strings"
	"testing"
)

// BenchmarkValidUTF8Kernel measures validUTF8x16 alone — no string scan around
// it — across block counts, so the per-call setup (3 LUT loads + broadcasts,
// paid once regardless of length) is separable from the per-block classify.
func BenchmarkValidUTF8Kernel(b *testing.B) {
	for _, n := range []int{16, 64, 128, 129, 192, 256, 1024, 4096, 16384} {
		// Cyrillic is 2 bytes/rune, so an odd length takes one ASCII byte to
		// land on a rune boundary (a mid-rune cut would just be invalid).
		body := []byte(strings.Repeat("аб", n/2))[:n&^1]
		if n%2 == 1 {
			body = append(body, 'a')
		}
		b.Run(fmt.Sprintf("%dB/x16", n), func(b *testing.B) {
			b.SetBytes(int64(n))
			for b.Loop() {
				if !validUTF8x16(body) {
					b.Fatal("invalid")
				}
			}
		})
		b.Run(fmt.Sprintf("%dB/x64", n), func(b *testing.B) {
			b.SetBytes(int64(n))
			for b.Loop() {
				if !validUTF8x64(body) {
					b.Fatal("invalid")
				}
			}
		})
	}
}
