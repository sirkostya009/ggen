//go:build goexperiment.simd

package scan

import (
	"bytes"
	"io"
	"math/rand"
	"testing"
)

// chunkReader yields at most n bytes per Read to force mid-string refills.
type chunkReader struct {
	r io.Reader
	n int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(p) > c.n {
		p = p[:c.n]
	}
	return c.r.Read(p)
}

// TestStreamStringSIMD_Parity pins the stream tier scanners against scalar
// Stream.String across every refill shape: bodies sized around lane seams,
// escapes/ctrl at each phase, chunked reads of 1..64 bytes, and randomized
// mixed bodies. Value, view, and key contracts all route through the same
// per-tier core, so String* parity covers all three shells.
func TestStreamStringSIMD_Parity(t *testing.T) {
	t.Parallel()
	tiers := []struct {
		name string
		fn   func(*Stream) (string, error)
	}{
		{"AVX", (*Stream).StringAVX},
		{"AVX2", (*Stream).StringAVX2},
		{"AVX512", (*Stream).StringAVX512},
	}
	var bodies []string
	bodies = append(bodies, "", "a", "hello", `esc\"aped`, `tail\\`, `uniécode`)
	for _, n := range []int{1, 15, 16, 17, 31, 32, 33, 63, 64, 65, 130, 600} {
		b := bytes.Repeat([]byte{'x'}, n)
		bodies = append(bodies, string(b))
		for _, c := range []byte{'\\', 0x01} {
			for pos := 0; pos < n; pos += max(1, n/5) {
				bb := bytes.Repeat([]byte{'y'}, n)
				bb[pos] = c
				if c == '\\' && pos == n-1 {
					continue // trailing backslash escapes the closing quote
				}
				if c == '\\' {
					bb[pos+1] = 'n'
				}
				bodies = append(bodies, string(bb))
			}
		}
	}
	rng := rand.New(rand.NewSource(11))
	alphabet := []byte("abcdefgh \x01\x1fé日")
	for range 500 {
		n := rng.Intn(150)
		b := make([]byte, 0, n)
		for len(b) < n {
			b = append(b, alphabet[rng.Intn(len(alphabet))])
		}
		bodies = append(bodies, string(b))
	}
	decode := func(fn func(*Stream) (string, error), payload []byte, chunk int) (string, error) {
		var s Stream
		s.Reset(&chunkReader{bytes.NewReader(payload), chunk}, make([]byte, 0, 8))
		return fn(&s)
	}
	for _, body := range bodies {
		payload := []byte(`"` + body + `"`)
		for _, chunk := range []int{1, 3, 7, 16, 64} {
			want, wantErr := decode((*Stream).String, payload, chunk)
			for _, tier := range tiers {
				got, gotErr := decode(tier.fn, payload, chunk)
				if (wantErr == nil) != (gotErr == nil) || wantErr != gotErr {
					t.Fatalf("%s(%q, chunk=%d): err %v, scalar err %v", tier.name, body, chunk, gotErr, wantErr)
				}
				if got != want {
					t.Fatalf("%s(%q, chunk=%d) = %q, scalar %q", tier.name, body, chunk, got, want)
				}
			}
		}
	}
}
