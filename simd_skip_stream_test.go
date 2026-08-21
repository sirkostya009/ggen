//go:build goexperiment.simd

package ggen

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
)

// TestStreamSkipValueSIMD_Parity pins the stream tier skip trees against
// scalar Stream.SkipValue: identical end Offset and error identity across
// compact + indented values, truncations, malformed mutations, and chunked
// readers forcing refills mid-string / mid-whitespace-run.
func TestStreamSkipValueSIMD_Parity(t *testing.T) {
	t.Parallel()
	tiers := []struct {
		name string
		fn   func(*Stream) error
	}{
		{"AVX", (*Stream).SkipValueAVX},
		{"AVX2", (*Stream).SkipValueAVX2},
		{"AVX512", (*Stream).SkipValueAVX512},
	}
	var cases [][]byte
	seeds := []string{
		`null`, `true`, `false`, `-12.5e3`, `""`, `"hi"`,
		`"esc\"aped\nvalue"`, `[1,2,3]`, `{"a":1,"b":[true,null,"x"]}`,
		`{"deep":{"er":{"est":[{"k":"v"},[[["s"]]]]}}}`,
		`"` + strings.Repeat("x", 300) + `"`,
	}
	for _, s := range seeds {
		cases = append(cases, []byte(s))
		var v any
		if json.Unmarshal([]byte(s), &v) == nil {
			for _, indent := range []string{"  ", strings.Repeat(" ", 40)} {
				if pretty, err := json.MarshalIndent(v, "", indent); err == nil {
					cases = append(cases, pretty)
				}
			}
		}
		for cut := 0; cut < len(s); cut++ {
			cases = append(cases, []byte(s[:cut]))
		}
	}
	cases = append(cases,
		[]byte("\"abc\x01def\""), []byte("\"abc\x01def"), []byte("\"abc\x01de\\n\""),
		[]byte("\"\x1f\""), []byte(strings.Repeat(" \n\t", 50)+"42"),
	)
	rng := rand.New(rand.NewSource(41))
	for _, seed := range seeds {
		for range 150 {
			b := []byte(seed)
			b[rng.Intn(len(b))] = byte(rng.Intn(128))
			cases = append(cases, b)
		}
	}
	run := func(fn func(*Stream) error, payload []byte, chunk int) (int, error) {
		var s Stream
		s.Reset(&chunkReader{bytes.NewReader(payload), chunk}, make([]byte, 0, 8))
		err := fn(&s)
		return s.Offset(), err
	}
	for _, in := range cases {
		for _, chunk := range []int{1, 3, 7, 64} {
			wantOff, wantErr := run((*Stream).SkipValue, in, chunk)
			for _, tier := range tiers {
				gotOff, gotErr := run(tier.fn, in, chunk)
				if wantErr != gotErr || (wantErr == nil && gotOff != wantOff) {
					t.Fatalf("%s(%q, chunk=%d) = (%d, %v), scalar (%d, %v)",
						tier.name, in, chunk, gotOff, gotErr, wantOff, wantErr)
				}
			}
		}
	}
}

// Tier twin of TestStreamCaptureValue's "live reader" subtest — the fill loop
// that blocked on a drained live reader was copied into every tier wrapper.
func TestStreamCaptureValueSIMD_LiveReader(t *testing.T) {
	t.Parallel()
	tiers := []struct {
		name string
		fn   func(*Stream) ([]byte, error)
	}{
		{"AVX", (*Stream).CaptureValueAVX},
		{"AVX2", (*Stream).CaptureValueAVX2},
		{"AVX512", (*Stream).CaptureValueAVX512},
	}
	for _, tier := range tiers {
		t.Run(tier.name, func(t *testing.T) {
			t.Parallel()
			for _, chunks := range liveReaderCases {
				want := strings.Join(chunks, "")
				var s Stream
				s.Reset(&liveChunkReader{chunks: append([]string(nil), chunks...)}, nil)
				got, err := tier.fn(&s)
				if err != nil {
					t.Fatalf("%q: %v", want, err)
				}
				if string(got) != want {
					t.Fatalf("%q: got %q", want, got)
				}
			}
		})
	}
}
