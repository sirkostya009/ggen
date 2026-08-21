//go:build goexperiment.simd

package ggen

import (
	"bytes"
	"fmt"
	"math/rand"
	"simd/archsimd"
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
		{0xC2, 0x80}, {0xDF, 0xBF}, // 2-byte bounds
		{0xC0, 0x80}, {0xC1, 0xBF}, // overlong 2
		{0xE0, 0xA0, 0x80}, {0xEF, 0xBF, 0xBF}, // 3-byte bounds
		{0xE0, 0x80, 0x80}, {0xE0, 0x9F, 0xBF}, // overlong 3
		{0xED, 0x9F, 0xBF},                     // U+D7FF (valid)
		{0xED, 0xA0, 0x80}, {0xED, 0xBF, 0xBF}, // surrogates
		{0xEE, 0x80, 0x80},                                 // U+E000 (valid)
		{0xF0, 0x90, 0x80, 0x80}, {0xF4, 0x8F, 0xBF, 0xBF}, // 4-byte bounds
		{0xF0, 0x80, 0x80, 0x80}, {0xF0, 0x8F, 0xBF, 0xBF}, // overlong 4
		{0xF4, 0x90, 0x80, 0x80},                 // > U+10FFFF
		{0xF5, 0x80, 0x80, 0x80}, {0xFF}, {0xFE}, // illegal leads
		{0x80}, {0xBF}, // stray continuations
		{0x80, 0x80}, {0xC2, 0x80, 0x80}, // continuation runs
		{0xC2}, {0xE0, 0xA0}, {0xF0, 0x90, 0x80}, // truncated runes
		{0xE1, 0x80}, {0xF1, 0x80, 0x80}, // truncated (non-edge leads)
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

// TestValidUTF8SIMD_EOFTruncation pins the check_eof arm that replaced the
// all-zero epilogue block. A rune truncated at end-of-input has no successor
// byte to fail against, so it is caught either by the zero padding of a partial
// final block or — when the input ends exactly on a block boundary — by the
// saturating sub against utf8MaxIncomplete. Both regimes matter, so every
// prefix length around every 16-byte seam is checked against utf8.Valid.
func TestValidUTF8SIMD_EOFTruncation(t *testing.T) {
	t.Parallel()
	// Every multi-byte rune encoding, cut at every proper prefix.
	runes := []string{"é", "€", "😀", "߿", "￿", "\U0010ffff"}
	var truncated []string
	for _, r := range runes {
		for cut := 1; cut < len(r); cut++ {
			truncated = append(truncated, r[:cut])
		}
	}
	// Also bare leads that never start a valid rune here.
	truncated = append(truncated, "\xc2", "\xe0", "\xf0", "\xf4", "\xdf", "\xef")

	for _, tail := range truncated {
		// Slide the truncation across every offset near the block seams, so
		// the dangling lead lands at each of the last three lanes of a full
		// block and at every position of a padded one.
		for fill := 0; fill <= 40; fill++ {
			in := strings.Repeat("a", fill) + tail
			want := utf8.Valid([]byte(in))
			got := validUTF8x16([]byte(in))
			if got != want {
				t.Fatalf("fill=%d tail=%q (len %d): got %v want %v", fill, tail, len(in), got, want)
			}
		}
	}

	// Valid inputs ending exactly on a block boundary must NOT be rejected —
	// the check must not fire on a complete rune sitting in the last lanes.
	for _, r := range runes {
		for fill := 0; fill <= 40; fill++ {
			in := strings.Repeat("a", fill) + r
			if want, got := utf8.Valid([]byte(in)), validUTF8x16([]byte(in)); got != want {
				t.Fatalf("valid case fill=%d rune=%q (len %d): got %v want %v", fill, r, len(in), got, want)
			}
		}
	}

	if !validUTF8x16(nil) || !validUTF8x16([]byte{}) {
		t.Fatal("empty input must be valid")
	}
}

// Semantic probe for ConcatShiftBytesRight — pins which operand is "low" and
// the byte order, before the utf8 validator builds on it. Want: prev1[i] =
// bytes of (prev ++ cur) at offset i+15, i.e. prev1[0] = prev[15], prev1[i>0]
// = cur[i-1].
func TestConcatShiftSemantics(t *testing.T) {
	var prevA, curA [16]uint8
	for i := range 16 {
		prevA[i] = uint8(i)      // 0..15
		curA[i] = uint8(100 + i) // 100..115
	}
	prev := archsimd.LoadUint8x16(prevA[:])
	cur := archsimd.LoadUint8x16(curA[:])

	// The law validUTF8x16's prev1/prev2/prev3 lanes build on:
	// recv.ConcatShiftBytesRight(N, arg)[i] == (arg ++ recv)[i+N].
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

// TestValidUTF8x64_Parity pins the 64-lane validator against utf8.Valid AND
// validUTF8x16. Its risky seams are structural: the 16-byte head runs a
// separate classify, the wide loop starts at 16 with load-based prev1/2/3, and
// the zero-padded tail plus check_eof close the end — so runes are planted at
// every one of those boundaries rather than only at random offsets.
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

	// A multibyte rune straddling every offset around the head/wide seam (16),
	// the first wide block end (80), and the gate (128).
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
	// Invalid sequences at the same seams must be rejected identically.
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
	// Truncated runes at end-of-input, across every tail remainder.
	for _, r := range runes {
		for cut := 1; cut < len(r); cut++ {
			for fill := 120; fill <= 200; fill++ {
				b := append(bytes.Repeat([]byte("a"), fill), r[:cut]...)
				check(t, b)
			}
		}
	}
	// Dense non-ASCII of every length through the gate and past it.
	for n := 0; n <= 300; n++ {
		body := []byte(strings.Repeat("аб😀", n))
		if len(body) > 600 {
			body = body[:600]
		}
		check(t, body)
		if n < len(body) {
			check(t, body[:n]) // arbitrary cut — often mid-rune
		}
	}
	// Randomized: mostly-valid text with occasional corruption.
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
