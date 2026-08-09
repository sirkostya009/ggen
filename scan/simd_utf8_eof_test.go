//go:build goexperiment.simd

package scan

import (
	"strings"
	"testing"
	"unicode/utf8"
)

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
