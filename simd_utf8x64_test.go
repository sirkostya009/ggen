//go:build goexperiment.simd

package ggen

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

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
