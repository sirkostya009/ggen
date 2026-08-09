//go:build goexperiment.simd

package scan

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestValidUTF8SIMD_Parity pins validUTF8x16's accept set to unicode/utf8.Valid
// exactly: every single byte, every 2-byte pair (exhaustive — covers all
// lead/continuation classifications), directed multi-byte edge cases at every
// block-boundary offset, and randomized fuzz.
func TestValidUTF8SIMD_Parity(t *testing.T) {
	check := func(b []byte) {
		t.Helper()
		if got, want := validUTF8x16(b), utf8.Valid(b); got != want {
			t.Fatalf("validUTF8x16(% x) = %v, utf8.Valid = %v", b, got, want)
		}
	}

	// All single bytes, all 2-byte pairs.
	for a := range 256 {
		check([]byte{byte(a)})
		for b := range 256 {
			check([]byte{byte(a), byte(b)})
		}
	}

	// Directed sequences — every legality class, incl. every boundary form.
	seqs := [...][]byte{
		{},
		[]byte("hello"),
		[]byte("é"), []byte("ф"), []byte("ऄ"), []byte("😀"),
		{0xC2, 0x80}, {0xDF, 0xBF},                         // 2-byte bounds
		{0xC0, 0x80}, {0xC1, 0xBF},                         // overlong 2
		{0xE0, 0xA0, 0x80}, {0xEF, 0xBF, 0xBF},             // 3-byte bounds
		{0xE0, 0x80, 0x80}, {0xE0, 0x9F, 0xBF},             // overlong 3
		{0xED, 0x9F, 0xBF},                                 // U+D7FF (valid)
		{0xED, 0xA0, 0x80}, {0xED, 0xBF, 0xBF},             // surrogates
		{0xEE, 0x80, 0x80},                                 // U+E000 (valid)
		{0xF0, 0x90, 0x80, 0x80}, {0xF4, 0x8F, 0xBF, 0xBF}, // 4-byte bounds
		{0xF0, 0x80, 0x80, 0x80}, {0xF0, 0x8F, 0xBF, 0xBF}, // overlong 4
		{0xF4, 0x90, 0x80, 0x80},                           // > U+10FFFF
		{0xF5, 0x80, 0x80, 0x80}, {0xFF}, {0xFE},           // illegal leads
		{0x80}, {0xBF},                                     // stray continuations
		{0x80, 0x80}, {0xC2, 0x80, 0x80},                   // continuation runs
		{0xC2}, {0xE0, 0xA0}, {0xF0, 0x90, 0x80},           // truncated runes
		{0xE1, 0x80}, {0xF1, 0x80, 0x80},                   // truncated (non-edge leads)
	}
	// Slide every sequence across block boundaries (prefix 0..33 ASCII bytes),
	// standalone and with an ASCII suffix (so truncation-vs-mid-span differs).
	for _, seq := range seqs {
		for pad := 0; pad <= 33; pad++ {
			prefix := strings.Repeat("a", pad)
			check([]byte(prefix + string(seq)))
			check([]byte(prefix + string(seq) + "zz"))
			check([]byte(prefix + string(seq) + "éé"))
		}
	}

	// Randomized: mixed valid runes and raw bytes.
	rng := rand.New(rand.NewSource(42))
	runes := []string{"a", "Z", "é", "ф", "ऄ", "😀", "߿", "�"}
	raw := []byte{0x80, 0xBF, 0xC0, 0xC2, 0xE0, 0xED, 0xF0, 0xF4, 0xF5, 0xFF, 0x20, 0x7F}
	for range 200000 {
		var sb []byte
		n := rng.Intn(80)
		for len(sb) < n {
			if rng.Intn(4) == 0 {
				sb = append(sb, raw[rng.Intn(len(raw))])
			} else {
				sb = append(sb, runes[rng.Intn(len(runes))]...)
			}
		}
		check(sb)
	}
}

// FuzzValidUTF8SIMD fuzzes raw byte spans against utf8.Valid.
func FuzzValidUTF8SIMD(f *testing.F) {
	f.Add([]byte("hello żółć 😀"))
	f.Add([]byte{0xED, 0xA0, 0x80})
	f.Add([]byte{0xF4, 0x90, 0x80, 0x80})
	f.Fuzz(func(t *testing.T, b []byte) {
		want := utf8.Valid(b)
		if got := validUTF8x16(b); got != want {
			t.Fatalf("validUTF8x16(% x) = %v, utf8.Valid = %v", b, got, want)
		}
		if got := validUTF8x64(b); got != want {
			t.Fatalf("validUTF8x64(% x) = %v, utf8.Valid = %v", b, got, want)
		}
		// Fuzz corpora skew short, and validUTF8x64 delegates below its gate —
		// pad so the 64-lane path itself is exercised on the same bytes.
		if len(b) > 0 {
			padded := append(bytes.Repeat([]byte("x"), utf8x64MinLen), b...)
			if got, w := validUTF8x64(padded), utf8.Valid(padded); got != w {
				t.Fatalf("validUTF8x64(padded % x) = %v, utf8.Valid = %v", b, got, w)
			}
		}
	})
}
