//go:build goexperiment.simd

package ggen

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"

	"simd/archsimd"
)

// TestValidUTF8SIMD_Parity checks that validUTF8x16 accepts exactly what
// utf8.Valid accepts. validUTF8x16 is a SIMD validator: it uses CPU vector
// instructions that work on 16 bytes at a time. A UTF-8 character (a rune)
// is one lead byte, which says how many bytes follow, plus 0 to 3
// continuation bytes. The test tries every single byte and every pair of
// bytes, which covers every lead and continuation combination. It then tries
// hand-picked multi-byte edge cases at every offset near a 16-byte block
// edge, and random input.
func TestValidUTF8SIMD_Parity(t *testing.T) {
	t.Parallel()
	check := func(b []byte) {
		t.Helper()
		if got, want := validUTF8x16(b), utf8.Valid(b); got != want {
			t.Fatalf("validUTF8x16(% x) = %v, utf8.Valid = %v", b, got, want)
		}
	}

	// Every single byte and every 2-byte pair.
	for a := range 256 {
		check([]byte{byte(a)})
		for b := range 256 {
			check([]byte{byte(a), byte(b)})
		}
	}

	// Hand-picked sequences: each kind of valid and invalid input, including
	// the smallest and largest value of each length. An overlong encoding
	// uses more bytes than needed and is invalid. Surrogates (U+D800 to
	// U+DFFF) are reserved for UTF-16 and are invalid in UTF-8.
	seqs := [...][]byte{
		{},
		[]byte("hello"),
		[]byte("é"), []byte("ф"), []byte("ऄ"), []byte("😀"),
		{0xC2, 0x80},
		{0xDF, 0xBF}, // smallest and largest 2-byte
		{0xC0, 0x80},
		{0xC1, 0xBF}, // overlong 2-byte
		{0xE0, 0xA0, 0x80},
		{0xEF, 0xBF, 0xBF}, // smallest and largest 3-byte
		{0xE0, 0x80, 0x80},
		{0xE0, 0x9F, 0xBF}, // overlong 3-byte
		{0xED, 0x9F, 0xBF}, // U+D7FF (valid)
		{0xED, 0xA0, 0x80},
		{0xED, 0xBF, 0xBF}, // surrogates
		{0xEE, 0x80, 0x80}, // U+E000 (valid)
		{0xF0, 0x90, 0x80, 0x80},
		{0xF4, 0x8F, 0xBF, 0xBF}, // smallest and largest 4-byte
		{0xF0, 0x80, 0x80, 0x80},
		{0xF0, 0x8F, 0xBF, 0xBF}, // overlong 4-byte
		{0xF4, 0x90, 0x80, 0x80}, // > U+10FFFF
		{0xF5, 0x80, 0x80, 0x80},
		{0xFF},
		{0xFE}, // bytes that are never a lead
		{0x80},
		{0xBF}, // continuation bytes with no lead
		{0x80, 0x80},
		{0xC2, 0x80, 0x80}, // too many continuation bytes
		{0xC2},
		{0xE0, 0xA0},
		{0xF0, 0x90, 0x80}, // cut-off runes
		{0xE1, 0x80},
		{0xF1, 0x80, 0x80}, // cut-off, leads not at a range edge
	}
	// Slide every sequence across block edges with 0 to 33 ASCII bytes in
	// front. Try it at the end of the input and with bytes after it, since a
	// cut-off rune at the end is a different case from one in the middle.
	for _, seq := range seqs {
		for pad := range 34 {
			prefix := strings.Repeat("a", pad)
			check([]byte(prefix + string(seq)))
			check([]byte(prefix + string(seq) + "zz"))
			check([]byte(prefix + string(seq) + "éé"))
		}
	}

	// Random mixes of valid runes and raw bytes.
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

// FuzzValidUTF8SIMD checks both validators against utf8.Valid on fuzzed
// input.
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
		// Fuzz inputs tend to be short, and validUTF8x64 hands short inputs
		// to validUTF8x16. Pad the input so the 64-byte code runs on the same
		// bytes.
		if len(b) > 0 {
			padded := append(bytes.Repeat([]byte("x"), utf8x64MinLen), b...)
			if got, w := validUTF8x64(padded), utf8.Valid(padded); got != w {
				t.Fatalf("validUTF8x64(padded % x) = %v, utf8.Valid = %v", b, got, w)
			}
		}
	})
}

// TestValidUTF8SIMD_EOFTruncation checks the end-of-input check. A rune cut
// off at the end of the input has no next byte to fail against. If the last
// block is partial, the zero padding after the input catches it. If the
// input ends exactly on a 16-byte block edge, a separate test against
// utf8MaxIncomplete catches it. simdutf calls that test check_eof. Both cases
// matter, so every length near every 16-byte edge is checked against
// utf8.Valid.
func TestValidUTF8SIMD_EOFTruncation(t *testing.T) {
	t.Parallel()
	// Multi-byte runes of each length, cut at every shorter length.
	runes := []string{"é", "€", "😀", "߿", "￿", "\U0010ffff"}
	var truncated []string
	for _, r := range runes {
		for cut := 1; cut < len(r); cut++ {
			truncated = append(truncated, r[:cut])
		}
	}
	// Also lone lead bytes with nothing after them.
	truncated = append(truncated, "\xc2", "\xe0", "\xf0", "\xf4", "\xdf", "\xef")

	for _, tail := range truncated {
		// Slide the cut-off rune across every offset near the block edges.
		// A lane is one byte slot inside a vector. The lone lead byte lands in
		// each of the last three lanes of a full block and at every position of
		// a padded one.
		for fill := range 41 {
			in := strings.Repeat("a", fill) + tail
			want := utf8.ValidString(in)
			got := validUTF8x16([]byte(in))
			if got != want {
				t.Fatalf("fill=%d tail=%q (len %d): got %v want %v", fill, tail, len(in), got, want)
			}
		}
	}

	// Valid inputs that end exactly on a block edge must not be rejected.
	// The check must not fire on a complete rune in the last lanes.
	for _, r := range runes {
		for fill := range 41 {
			in := strings.Repeat("a", fill) + r
			if want, got := utf8.ValidString(in), validUTF8x16([]byte(in)); got != want {
				t.Fatalf("valid case fill=%d rune=%q (len %d): got %v want %v", fill, r, len(in), got, want)
			}
		}
	}

	if !validUTF8x16(nil) || !validUTF8x16([]byte{}) {
		t.Fatal("empty input must be valid")
	}
}

// TestConcatShiftSemantics checks how ConcatShiftBytesRight orders its two
// operands and their bytes. The UTF-8 validator depends on this. The wanted
// result is prev1[i] = (prev ++ cur)[i+15]. So prev1[0] = prev[15], and
// prev1[i] = cur[i-1] for i > 0.
func TestConcatShiftSemantics(t *testing.T) {
	t.Parallel()
	var prevA, curA [16]uint8
	for i := range 16 {
		prevA[i] = uint8(i)      // 0..15
		curA[i] = uint8(100 + i) // 100..115
	}
	prev := archsimd.LoadUint8x16(prevA[:])
	cur := archsimd.LoadUint8x16(curA[:])

	// The rule validUTF8x16 relies on to build prev1, prev2 and prev3:
	// recv.ConcatShiftBytesRight(arg, N)[i] == (arg ++ recv)[i+N].
	check := func(name string, got [16]uint8, arg, recv [16]uint8, n int) {
		t.Helper()
		concat := append(append([]uint8{}, arg[:]...), recv[:]...)
		for i := range 16 {
			if got[i] != concat[i+n] {
				t.Fatalf("%s: out[%d] = %d, want (arg++recv)[%d] = %d\nout = %v",
					name, i, got[i], i+n, concat[i+n], got)
			}
		}
	}

	var out [16]uint8
	cur.ConcatShiftBytesRight(prev, 15).Store(out[:])
	check("cur.ConcatShiftBytesRight(prev, 15)", out, prevA, curA, 15)
	prev.ConcatShiftBytesRight(cur, 15).Store(out[:])
	check("prev.ConcatShiftBytesRight(cur, 15)", out, curA, prevA, 15)
	cur.ConcatShiftBytesRight(prev, 14).Store(out[:])
	check("cur.ConcatShiftBytesRight(prev, 14)", out, prevA, curA, 14)
	cur.ConcatShiftBytesRight(prev, 13).Store(out[:])
	check("cur.ConcatShiftBytesRight(prev, 13)", out, prevA, curA, 13)
}

// TestValidUTF8x64_Parity checks the 64-byte validator against both
// utf8.Valid and validUTF8x16. The risky spots come from its structure. The
// first 16 bytes have their own check. The 64-byte loop starts at offset 16
// and loads the previous bytes it needs from memory. The zero-padded tail
// and the end-of-input check close the end. So runes are placed at each of
// those edges, not only at random offsets.
func TestValidUTF8x64_Parity(t *testing.T) {
	t.Parallel()
	check := func(t *testing.T, b []byte) {
		t.Helper()
		want := utf8.Valid(b)
		if got := validUTF8x64(b); got != want {
			t.Fatalf("validUTF8x64(%q...len %d) = %v, utf8.Valid = %v", trunc(b), len(b), got, want)
		}
		if got := validUTF8x16(b); got != want {
			t.Fatalf("validUTF8x16(%q...len %d) = %v, utf8.Valid = %v", trunc(b), len(b), got, want)
		}
	}

	runes := []string{"é", "€", "😀", "ب", "日", "\U0010ffff"}
	bad := []string{"\x80", "\xff", "\xc0\x80", "\xe0\x80\x80", "\xed\xa0\x80", "\xf5\x80\x80\x80", "\xc2"}

	// A multi-byte rune at every offset near the end of the first 16 bytes,
	// the end of the first 64-byte block (80), and the minimum length for
	// the 64-byte code (128).
	for _, r := range runes {
		for _, base := range []int{0, 8, 12, 14, 16, 18, 60, 76, 78, 80, 82, 120, 126, 128, 130} {
			for _, total := range []int{128, 140, 191, 192, 193, 256} {
				if base+len(r) > total {
					continue
				}
				b := bytes.Repeat([]byte("a"), total)
				copy(b[base:], r)
				check(t, b)
			}
		}
	}
	// Invalid sequences at the same edges must be rejected by both.
	for _, s := range bad {
		for _, base := range []int{0, 15, 16, 17, 79, 80, 81, 127, 128, 190} {
			for _, total := range []int{128, 192, 200} {
				if base+len(s) > total {
					continue
				}
				b := bytes.Repeat([]byte("a"), total)
				copy(b[base:], s)
				check(t, b)
			}
		}
	}
	// Cut-off runes at the end of the input, for every tail length.
	for _, r := range runes {
		for cut := 1; cut < len(r); cut++ {
			for fill := 120; fill <= 200; fill++ {
				b := append(bytes.Repeat([]byte("a"), fill), r[:cut]...)
				check(t, b)
			}
		}
	}
	// Dense non-ASCII text at every length up to and past the minimum
	// length for the 64-byte code.
	for n := range 301 {
		body := []byte(strings.Repeat("аб😀", n))
		if len(body) > 600 {
			body = body[:600]
		}
		check(t, body)
		if n < len(body) {
			check(t, body[:n]) // any cut, often inside a rune
		}
	}
	// Random, mostly valid text with some broken bytes.
	rng := rand.New(rand.NewSource(99))
	alphabet := []string{"a", "é", "€", "😀", "\x80", "\xff", "\xc2", "\xed\xa0\x80"}
	for range 20000 {
		var sb []byte
		for len(sb) < 128+rng.Intn(200) {
			sb = append(sb, alphabet[rng.Intn(len(alphabet))]...)
		}
		check(t, sb)
	}
}

func trunc(b []byte) []byte {
	if len(b) > 24 {
		return b[:24]
	}
	return b
}

// BenchmarkValidUTF8Kernel measures validUTF8x16 and validUTF8x64 alone,
// with no string scan around them, at many lengths. Each call has a fixed
// setup cost: it loads three lookup tables (16 entries, indexed by 4 bits of
// a byte) and fills a few vectors with constants. Varying the length
// separates that cost from the cost per block.
func BenchmarkValidUTF8Kernel(b *testing.B) {
	for _, n := range []int{16, 64, 128, 129, 192, 256, 1024, 4096, 16384} {
		// Each Cyrillic letter is 2 bytes, so an odd length ends with one
		// ASCII byte. Cutting a letter in half would make the input invalid.
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

// BenchmarkStringUTF8CostAVX512 is BenchmarkStringUTF8Cost for StringAVX512.
// AVX-512 is the x86 instruction set that works on 64 bytes at a time.
// StringAVX512 finds the closing quote and validates UTF-8 with vector code.
// The utf8Valid row runs the scalar utf8.Valid on the same Cyrillic text for
// comparison.
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
