//go:build goexperiment.simd

package scan

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
	run := func(fn func(*Stream) error, payload []byte, chunk int, shift bool) (int, error) {
		var s Stream
		s.Reset(&chunkReader{bytes.NewReader(payload), chunk}, make([]byte, 0, 8))
		s.Shift = shift
		err := fn(&s)
		return s.Offset(), err
	}
	for _, in := range cases {
		for _, chunk := range []int{1, 3, 7, 64} {
			for _, shift := range []bool{true, false} {
				wantOff, wantErr := run((*Stream).SkipValue, in, chunk, shift)
				for _, tier := range tiers {
					gotOff, gotErr := run(tier.fn, in, chunk, shift)
					if wantErr != gotErr || (wantErr == nil && gotOff != wantOff) {
						t.Fatalf("%s(%q, chunk=%d, shift=%v) = (%d, %v), scalar (%d, %v)",
							tier.name, in, chunk, shift, gotOff, gotErr, wantOff, wantErr)
					}
				}
			}
		}
	}
}
