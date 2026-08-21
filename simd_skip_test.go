//go:build goexperiment.simd

package ggen

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
)

// TestSkipValueSIMD_Parity pins the tier skip trees against scalar SkipValue:
// identical end position and error identity over well-formed values (compact
// + indented, WS runs at lane seams), truncations at every byte, and
// randomized malformed mutations.
func TestSkipValueSIMD_Parity(t *testing.T) {
	t.Parallel()
	tiers := []struct {
		name string
		fn   func([]byte, int) (int, error)
	}{
		{"AVX", SkipValueAVX},
		{"AVX2", SkipValueAVX2},
		{"AVX512", SkipValueAVX512},
	}
	var cases [][]byte
	seeds := []string{
		`null`, `true`, `false`, `0`, `-12.5e3`, `""`, `"hi"`,
		`"esc\"aped\nvalue"`, `"unié😀"`,
		`[]`, `{}`, `[1,2,3]`, `{"a":1,"b":[true,null,"x"]}`,
		`{"deep":{"er":{"est":[{"k":"v"},[[["s"]]]]}}}`,
		`"` + strings.Repeat("x", 300) + `"`,
	}
	for _, s := range seeds {
		cases = append(cases, []byte(s))
		// Indented variants with varied WS-run widths (1..80) around every
		// structural boundary.
		var v any
		if json.Unmarshal([]byte(s), &v) == nil {
			for _, indent := range []string{" ", "  ", "\t", strings.Repeat(" ", 17), strings.Repeat(" ", 80)} {
				pretty, err := json.MarshalIndent(v, "", indent)
				if err == nil {
					cases = append(cases, pretty)
				}
			}
		}
		// Truncations.
		for cut := 0; cut < len(s); cut++ {
			cases = append(cases, []byte(s[:cut]))
		}
	}
	// Ctrl bytes inside strings, incl. truncated tails (error-split cases).
	cases = append(cases,
		[]byte("\"abc\x01def\""), []byte("\"abc\x01def"), []byte("\"abc\x01de\\n\""),
		[]byte("\"abc\x01de\\n"), []byte("\"\x1f\""), []byte("\"\x00"),
	)
	rng := rand.New(rand.NewSource(21))
	for _, seed := range seeds {
		for range 200 {
			b := []byte(seed)
			b[rng.Intn(len(b))] = byte(rng.Intn(128))
			cases = append(cases, b)
		}
	}
	// Leading whitespace runs at lane seams.
	for _, n := range []int{1, 15, 16, 17, 31, 32, 33, 63, 64, 65, 200} {
		cases = append(cases, []byte(strings.Repeat(" \n\t", n)+"42"))
	}
	for _, in := range cases {
		wantPos, wantErr := SkipValue(in, 0)
		for _, tier := range tiers {
			gotPos, gotErr := tier.fn(in, 0)
			if wantErr != gotErr || gotPos != wantPos {
				t.Fatalf("%s(%q) = (%d, %v), scalar (%d, %v)", tier.name, in, gotPos, gotErr, wantPos, wantErr)
			}
		}
	}
}

// TestSkipSpaceSIMD_Parity pins the tier whitespace skips against scalar
// SkipSpace at every run length around lane seams and over random mixes.
func TestSkipSpaceSIMD_Parity(t *testing.T) {
	t.Parallel()
	tiers := []struct {
		name string
		fn   func([]byte, int) int
	}{
		{"AVX", SkipSpaceAVX},
		{"AVX2", SkipSpaceAVX2},
		{"AVX512", SkipSpaceAVX512},
	}
	ws := []byte(" \t\n\r")
	rng := rand.New(rand.NewSource(31))
	var cases [][]byte
	for n := 0; n <= 130; n++ {
		b := make([]byte, n, n+1)
		for i := range b {
			b[i] = ws[rng.Intn(4)]
		}
		cases = append(cases, b, append(bytes.Clone(b), 'x'), append(bytes.Clone(b), 0x01))
	}
	cases = append(cases, []byte("x"), []byte{0x1f}, nil)
	for _, in := range cases {
		want := SkipSpace(in, 0)
		for _, tier := range tiers {
			if got := tier.fn(in, 0); got != want {
				t.Fatalf("%s(%q) = %d, scalar %d", tier.name, in, got, want)
			}
		}
	}
}

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
