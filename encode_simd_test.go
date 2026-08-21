//go:build goexperiment.simd

package ggen

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// TestAppendStringSIMD_Parity pins byte-parity of every tier against the
// scalar escapers: fixed cases at every vector-phase alignment (escape at
// each lane position, at lane seams, in the tail) plus randomized bodies
// mixing clean ASCII, escapes, ctrl bytes, HTML chars, and UTF-8.
func TestAppendStringSIMD_Parity(t *testing.T) {
	t.Parallel()
	type pair struct {
		name           string
		scalar, tiered func([]byte, string) []byte
	}
	pairs := []pair{
		{"NoHTMLAVX", AppendStringNoHTML, AppendStringNoHTMLAVX},
		{"NoHTMLAVX2", AppendStringNoHTML, AppendStringNoHTMLAVX2},
		{"NoHTMLAVX512", AppendStringNoHTML, AppendStringNoHTMLAVX512},
		{"HTMLAVX", AppendString, AppendStringAVX},
		{"HTMLAVX2", AppendString, AppendStringAVX2},
		{"HTMLAVX512", AppendString, AppendStringAVX512},
	}
	var cases []string
	cases = append(cases, "", `plain`, "\x00", "\x1f", `"`, `\`, "<>&", "héllo wörld")
	// One special byte at every position of bodies sized around lane seams.
	for _, n := range []int{1, 15, 16, 17, 31, 32, 33, 63, 64, 65, 100, 130} {
		for _, c := range []byte{'"', '\\', '\n', 0x01, '<', '&'} {
			for pos := 0; pos < n; pos += max(1, n/7) {
				b := make([]byte, n)
				for i := range b {
					b[i] = 'a' + byte(i%26)
				}
				b[pos] = c
				cases = append(cases, string(b))
			}
		}
	}
	rng := rand.New(rand.NewSource(7))
	alphabet := []byte("abcdefgh\"\\\n\r\t\b\f\x00\x01\x1f<>&é日")
	for range 3000 {
		n := rng.Intn(200)
		b := make([]byte, 0, n)
		for len(b) < n {
			b = append(b, alphabet[rng.Intn(len(alphabet))])
		}
		cases = append(cases, string(b))
	}
	for _, p := range pairs {
		for _, in := range cases {
			want := p.scalar([]byte{}, in)
			got := p.tiered([]byte{}, in)
			if string(want) != string(got) {
				t.Fatalf("%s(%q):\n got %q\nwant %q", p.name, in, got, want)
			}
		}
	}
}

func BenchmarkEscapeScan(b *testing.B) {
	for _, n := range []int{64, 256, 2800} {
		s := strings.Repeat("a", n)
		buf := make([]byte, 0, n+8)
		b.Run("scalar_"+strings.Repeat("x", 0)+string(rune('0'+n/100)), func(b *testing.B) {
			for b.Loop() {
				buf = AppendStringNoHTML(buf[:0], s)
			}
		})
		b.Run("avx512_"+string(rune('0'+n/100)), func(b *testing.B) {
			for b.Loop() {
				buf = AppendStringNoHTMLAVX512(buf[:0], s)
			}
		})
	}
}

// BenchmarkEscapeTailRem sweeps the sub-lane remainder — the bytes left after
// the last full vector iteration. That leftover is what the tail handles, so
// its cost scales with rem and is invisible at the lane-multiple sizes
// BenchmarkEscapeScan uses (64/256 are rem=0 at every tier).
func BenchmarkEscapeTailRem(b *testing.B) {
	const lane = 64
	for _, rem := range []int{0, 1, 16, 32, 48, 63} {
		n := lane*2 + rem
		s := strings.Repeat("a", n)
		buf := make([]byte, 0, n+8)
		b.Run(fmt.Sprintf("rem%d", rem), func(b *testing.B) {
			b.SetBytes(int64(n))
			for b.Loop() {
				buf = AppendStringNoHTMLAVX512(buf[:0], s)
			}
		})
	}
}

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
