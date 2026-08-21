//go:build goexperiment.simd

package ggen

import (
	"math/rand"
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
