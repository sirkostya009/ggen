//go:build goexperiment.simd

package ggen

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// BenchmarkStringUTF8CostAVX512 is BenchmarkStringUTF8Cost on the avx512 tier:
// vector locate + scalar utf8.Valid second pass. Shows how dominant the scalar
// DFA is once the locate runs at lane speed — the gap a fused vector (Lemire)
// validator would close.
func BenchmarkStringUTF8CostAVX512(b *testing.B) {
	sizes := []struct {
		name string
		n    int
	}{{"16B", 16}, {"64B", 64}, {"256B", 256}, {"1KB", 1024}, {"4KB", 4096}}
	for _, sz := range sizes {
		ascii := strings.Repeat("abcdefgh", sz.n/8)
		cyr := strings.Repeat("аб", sz.n/4)
		asciiQ := []byte(`"` + ascii + `"`)
		cyrQ := []byte(`"` + cyr + `"`)
		b.Run(sz.name+"/StringAVX512_ascii", func(b *testing.B) {
			for b.Loop() {
				_, _, _ = StringAVX512(asciiQ, 0, true)
			}
		})
		b.Run(sz.name+"/StringAVX512_cyrillic", func(b *testing.B) {
			for b.Loop() {
				_, _, _ = StringAVX512(cyrQ, 0, true)
			}
		})
		b.Run(sz.name+"/utf8Valid_cyrillic", func(b *testing.B) {
			body := []byte(cyr)
			for b.Loop() {
				_ = utf8.Valid(body)
			}
		})
	}
}
