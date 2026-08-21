package ggen

import (
	"strings"
	"testing"
)

// BenchmarkFloat64Forms splits the number shapes by which path they take:
// plain decimals ride exactShort, exponent forms used to fall through to
// strconv.ParseFloat, and the out-of-range rows still do (they pin that the
// added arm costs nothing when it declines).
func BenchmarkFloat64Forms(b *testing.B) {
	forms := []struct{ name, span string }{
		{"plain_int", "12345"},
		{"plain_frac", "1234.5678"},
		{"plain_wide", "1234567890.12345"},
		{"exp_small", "1.5e3"},
		{"exp_neg", "-2.25e-4"},
		{"exp_wide", "1.234567890123e5"},
		{"exp_reject", "1e23"},                  // |power| > 22 → ParseFloat
		{"wide_reject", "1.234567890123456789"}, // > 16 B → ParseFloat
	}
	for _, f := range forms {
		data := []byte(f.span + ",")
		b.Run(f.name, func(b *testing.B) {
			for b.Loop() {
				if _, _, err := Float64(data, 0); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	// A payload-shaped run: many numbers back to back, exponent-heavy.
	var sb strings.Builder
	for i := range 200 {
		if i > 0 {
			sb.WriteByte(',')
		}
		if i%2 == 0 {
			sb.WriteString("1.5e3")
		} else {
			sb.WriteString("1234.5678")
		}
	}
	mixed := []byte(sb.String())
	b.Run("mixed_200", func(b *testing.B) {
		b.SetBytes(int64(len(mixed)))
		for b.Loop() {
			i := 0
			for i < len(mixed) {
				_, n, err := Float64(mixed, i)
				if err != nil {
					b.Fatal(err)
				}
				i = n + 1
			}
		}
	})
}
