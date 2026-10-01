//go:build goexperiment.simd

package ggen

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// TestAppendStringSIMD_Parity checks that every SIMD escaper writes the same
// bytes as the plain scalar one. SIMD means testing 16, 32 or 64 bytes with
// one CPU instruction. A lane is one byte slot inside such a vector. The
// fixed cases put one escape at many positions: inside a lane, at the edge
// between two lanes, and in the leftover tail. Random strings then mix plain
// ASCII, escapes, control bytes, HTML characters and multi-byte UTF-8.
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
	// One special byte at many positions, in strings sized near lane edges.
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
			if !bytes.Equal(want, got) {
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

// BenchmarkEscapeTailRem varies rem, the number of bytes left after the last
// full 64-byte vector step. The tail code handles those bytes, so its cost
// depends on rem. BenchmarkEscapeScan cannot show this, because its sizes 64
// and 256 leave rem=0 at every vector width.
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

// TestAppendStringSIMD_OverlapTailParity checks the tail step in full. For
// the leftover bytes, the escaper reloads the last full vector of the string.
// That vector overlaps bytes the main loop already wrote. The result is a
// mask, one bit per byte that needs escaping. The escaper shifts it by
// lane-rem to drop the bits for bytes already written. A bit kept by mistake
// would escape a byte twice. A bit lost by mistake would skip an escape. So
// the test tries every length and every escape position near the overlap.
// This covers every rem from 0 to lane-1, with escapes on both sides of the
// overlap edge.
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
			// Every length from one lane up to three lanes. rem goes through
			// 0..lane-1 twice, with and without a second full lane.
			for n := tr.lane; n <= tr.lane*3; n++ {
				clean := []byte(strings.Repeat("abcdefgh", n/8+1))[:n]
				for _, e := range escapes {
					// One escape at every position, including the whole
					// overlap region [n-lane, n).
					for pos := range n {
						b := append([]byte(nil), clean...)
						b[pos] = e
						in := string(b)
						want := tr.scalar([]byte{}, in)
						got := tr.tiered([]byte{}, in)
						if !bytes.Equal(want, got) {
							t.Fatalf("n=%d pos=%d esc=%q:\n got %q\nwant %q", n, pos, e, got, want)
						}
					}
					// Two escapes on both sides of the overlap edge. One is in
					// the last lane the main loop handled, one is in the tail.
					if n > tr.lane {
						b := append([]byte(nil), clean...)
						b[n-tr.lane] = e
						b[n-1] = e
						in := string(b)
						want := tr.scalar([]byte{}, in)
						got := tr.tiered([]byte{}, in)
						if !bytes.Equal(want, got) {
							t.Fatalf("n=%d straddle esc=%q:\n got %q\nwant %q", n, e, got, want)
						}
					}
				}
			}
		})
	}
}
