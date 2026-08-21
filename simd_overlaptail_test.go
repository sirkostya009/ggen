//go:build goexperiment.simd

package ggen

import (
	"strings"
	"testing"
)

// TestAppendStringSIMD_OverlapTailParity exhaustively pins the overlapping
// tail reload. The tail reclassifies the LAST full lane, which re-covers bytes
// the main loop already emitted; only a correct shift (lane-rem) drops those
// bits. A stale bit would double-escape a byte, a lost one would drop an
// escape — so every (length, escape position) pair around the seam is checked,
// which walks every possible rem 0..lane-1 with an escape on both sides of the
// overlap boundary.
func TestAppendStringSIMD_OverlapTailParity(t *testing.T) {
	t.Parallel()
	tiers := []struct {
		name           string
		lane           int
		scalar, tiered func([]byte, string) []byte
	}{
		{"NoHTMLAVX", 16, AppendStringNoHTML, AppendStringNoHTMLAVX},
		{"NoHTMLAVX2", 32, AppendStringNoHTML, AppendStringNoHTMLAVX2},
		{"NoHTMLAVX512", 64, AppendStringNoHTML, AppendStringNoHTMLAVX512},
		{"HTMLAVX", 16, AppendString, AppendStringAVX},
		{"HTMLAVX2", 32, AppendString, AppendStringAVX2},
		{"HTMLAVX512", 64, AppendString, AppendStringAVX512},
	}
	escapes := []byte{'"', '\\', '\n', 0x00, 0x1f, '<', '>', '&'}
	for _, tr := range tiers {
		t.Run(tr.name, func(t *testing.T) {
			t.Parallel()
			// Every length from one lane to two lanes past it: rem cycles
			// through 0..lane-1 twice, with and without a second full lane.
			for n := tr.lane; n <= tr.lane*3; n++ {
				clean := []byte(strings.Repeat("abcdefgh", n/8+1))[:n]
				for _, e := range escapes {
					// Single escape at EVERY position, incl. the whole
					// overlap region [n-lane, n).
					for pos := range n {
						b := append([]byte(nil), clean...)
						b[pos] = e
						in := string(b)
						want := tr.scalar([]byte{}, in)
						got := tr.tiered([]byte{}, in)
						if string(want) != string(got) {
							t.Fatalf("n=%d pos=%d esc=%q:\n got %q\nwant %q", n, pos, e, got, want)
						}
					}
					// Two escapes straddling the overlap boundary: one in the
					// last full lane the main loop handled, one in the tail.
					if n > tr.lane {
						b := append([]byte(nil), clean...)
						b[n-tr.lane] = e
						b[n-1] = e
						in := string(b)
						want := tr.scalar([]byte{}, in)
						got := tr.tiered([]byte{}, in)
						if string(want) != string(got) {
							t.Fatalf("n=%d straddle esc=%q:\n got %q\nwant %q", n, e, got, want)
						}
					}
				}
			}
		})
	}
}
