package scan

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// BenchmarkStringUTF8Cost splits the two-pass string decode cost: full
// scan.String (locate + checkSpan + gated utf8.Valid) vs the utf8.Valid pass
// alone, on ASCII vs Cyrillic (2-byte runes) spans. The delta between the
// ascii and cyrillic String rows is what a fused (single-pass) validator
// could at best reclaim.
func BenchmarkStringUTF8Cost(b *testing.B) {
	sizes := []struct {
		name string
		n    int
	}{{"16B", 16}, {"64B", 64}, {"256B", 256}, {"1KB", 1024}}
	for _, sz := range sizes {
		ascii := strings.Repeat("abcdefgh", sz.n/8)
		cyr := strings.Repeat("аб", sz.n/4) // 2 runes = 4 bytes
		asciiQ := []byte(`"` + ascii + `"`)
		cyrQ := []byte(`"` + cyr + `"`)
		b.Run(sz.name+"/String_ascii", func(b *testing.B) {
			for b.Loop() {
				_, _, _ = String(asciiQ, 0, true)
			}
		})
		b.Run(sz.name+"/String_cyrillic", func(b *testing.B) {
			for b.Loop() {
				_, _, _ = String(cyrQ, 0, true)
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
