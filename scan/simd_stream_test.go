//go:build goexperiment.simd

package scan

import (
	"bytes"
	"io"
	"math/rand"
	"strings"
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
		fn   func(*Stream, bool) (string, error)
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
	// Invalid UTF-8 at window seams: permissive mode must pass bytes
	// through verbatim, strict must reject identically to scalar.
	for _, n := range []int{1, 15, 16, 31, 32, 63, 64} {
		pad := strings.Repeat("x", n)
		bodies = append(bodies, pad+"\xff", "\xff"+pad, pad+"\xe2(")
	}
	rng := rand.New(rand.NewSource(11))
	alphabet := []byte("abcdefgh \x01\x1fé日\xff")
	for range 500 {
		n := rng.Intn(150)
		b := make([]byte, 0, n)
		for len(b) < n {
			b = append(b, alphabet[rng.Intn(len(alphabet))])
		}
		bodies = append(bodies, string(b))
	}
	decode := func(fn func(*Stream, bool) (string, error), payload []byte, chunk int, validate bool) (string, error) {
		var s Stream
		s.Reset(&chunkReader{bytes.NewReader(payload), chunk}, make([]byte, 0, 8))
		return fn(&s, validate)
	}
	for _, body := range bodies {
		payload := []byte(`"` + body + `"`)
		for _, chunk := range []int{1, 3, 7, 16, 64} {
			for _, validate := range []bool{true, false} {
				want, wantErr := decode((*Stream).String, payload, chunk, validate)
				for _, tier := range tiers {
					got, gotErr := decode(tier.fn, payload, chunk, validate)
					if (wantErr == nil) != (gotErr == nil) || wantErr != gotErr {
						t.Fatalf("%s(%q, chunk=%d, validate=%v): err %v, scalar err %v", tier.name, body, chunk, validate, gotErr, wantErr)
					}
					if got != want {
						t.Fatalf("%s(%q, chunk=%d, validate=%v) = %q, scalar %q", tier.name, body, chunk, validate, got, want)
					}
				}
			}
		}
	}
}
