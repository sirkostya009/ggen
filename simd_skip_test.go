//go:build goexperiment.simd

package ggen

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// TestSkipValueSIMD_Parity checks the SIMD versions of SkipValue against the
// plain scalar one. SIMD means handling 16, 32 or 64 bytes with one CPU
// instruction, and a lane is one byte slot in such a vector. The end position
// and the exact error must match. Inputs are valid values, both compact and
// indented, with whitespace runs that cross lane edges. They also include
// every truncation of each value and random broken mutations.
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
		// Indented copies with whitespace runs of 1 to 80 bytes around every
		// bracket, comma and colon.
		var v any
		if json.Unmarshal([]byte(s), &v) == nil {
			for _, indent := range []string{" ", "  ", "\t", strings.Repeat(" ", 17), strings.Repeat(" ", 80)} {
				pretty, err := json.MarshalIndent(v, "", indent)
				if err == nil {
					cases = append(cases, pretty)
				}
			}
		}
		// Every truncation.
		for cut := range len(s) {
			cases = append(cases, []byte(s[:cut]))
		}
	}
	// Control bytes inside strings, also with the string cut off. These are
	// cases where different code could report different errors.
	cases = append(cases,
		[]byte("\"abc\x01def\""), []byte("\"abc\x01def"), []byte("\"abc\x01de\\n\""),
		[]byte("\"abc\x01de\\n"), []byte("\"\x1f\""), []byte("\"\x00"),
		// Broken escapes and literals at the very end of the input, next to
		// inputs that are only cut short.
		[]byte(`"\u12"`), []byte(`"ab\uZ"`), []byte(`"\u00`), []byte(`"\ud83d\uDE`),
		[]byte("\"ab\x01}"), []byte("trux"), []byte("fals"),
	)
	rng := rand.New(rand.NewSource(21))
	for _, seed := range seeds {
		for range 200 {
			b := []byte(seed)
			b[rng.Intn(len(b))] = byte(rng.Intn(128))
			cases = append(cases, b)
		}
	}
	// Leading whitespace runs with lengths near lane edges.
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

// TestSkipSpaceSIMD_Parity checks the SIMD versions of SkipSpace against the
// scalar one, at every run length up to 130 and with random whitespace
// mixes.
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
	for n := range 131 {
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

// TestStreamSkipValueSIMD_Parity checks the SIMD versions of
// Stream.SkipValue against the scalar one. Offset must match: the end of the
// value, or the byte where it gave up on error. The error must match too.
// Inputs are compact and indented values, truncations and broken mutations.
// The readers return small chunks, so the Stream must read more input in the
// middle of a string or a whitespace run.
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
		for cut := range len(s) {
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
				if wantErr != gotErr || gotOff != wantOff {
					t.Fatalf("%s(%q, chunk=%d) = (%d, %v), scalar (%d, %v)",
						tier.name, in, chunk, gotOff, gotErr, wantOff, wantErr)
				}
			}
		}
	}
}

// SIMD version of the "live reader" subtest of TestStreamCaptureValue. A live
// reader, like a socket, may have no more data yet. CaptureValue must return
// a complete value without waiting for more input. Each SIMD version has its
// own read loop, so each one is checked.
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

// SIMD version of TestStreamCaptureValue_MaxDepthDoesNotHang. A run of '['
// deeper than maxDepth must fail with ErrMaxDepth right away. It must not
// block in Read waiting for more input when the run ends at the edge of the
// buffered data. Each SIMD version has its own read loop, so each one is
// checked.
func TestStreamCaptureValueSIMD_MaxDepthDoesNotHang(t *testing.T) {
	t.Parallel()
	for _, tier := range []struct {
		name string
		fn   func(*Stream) ([]byte, error)
	}{
		{"AVX", (*Stream).CaptureValueAVX},
		{"AVX2", (*Stream).CaptureValueAVX2},
		{"AVX512", (*Stream).CaptureValueAVX512},
	} {
		t.Run(tier.name, func(t *testing.T) {
			t.Parallel()
			block := make(chan struct{})
			defer close(block)
			done := make(chan error, 1)
			go func() {
				var s Stream
				s.Reset(&liveReader{data: bytes.Repeat([]byte("["), maxDepth+1), block: block}, make([]byte, 0, 64))
				_, err := tier.fn(&s)
				done <- err
			}()
			select {
			case err := <-done:
				if err != ErrMaxDepth {
					t.Errorf("got %v, want ErrMaxDepth", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("hung on a depth-capped value")
			}
		})
	}
}

// SIMD version of the skipString row in TestStreamString_ErrorPos. When the
// Stream reads more input, it may move unread bytes to the front of its
// buffer. Every such read must then adjust Pos. If one does not, Pos ends up
// past the end of the buffer.
func TestStreamSkipStringSIMD_ErrorPos(t *testing.T) {
	t.Parallel()
	const prefix = `"pre"`
	in := prefix + `"` + strings.Repeat("a", 100) + "\x01\""
	for _, tier := range []struct {
		name string
		fn   func(*Stream) error
	}{
		{"AVX", (*Stream).skipStringAVX},
		{"AVX2", (*Stream).skipStringAVX2},
		{"AVX512", (*Stream).skipStringAVX512},
	} {
		t.Run(tier.name, func(t *testing.T) {
			t.Parallel()
			for _, chunk := range []int{1, 7, 64} {
				var s Stream
				s.Reset(strings.NewReader(in), make([]byte, 0, chunk))
				if err := tier.fn(&s); err != nil {
					t.Fatalf("chunk=%d: prefix: %v", chunk, err)
				}
				if err := tier.fn(&s); err != ErrBadString {
					t.Fatalf("chunk=%d: err %v, want ErrBadString", chunk, err)
				}
				if s.Pos > len(s.Bytes()) {
					t.Errorf("chunk=%d: Pos %d past len(buf) %d", chunk, s.Pos, len(s.Bytes()))
				}
				// The error offset is the control byte's own index, like the scalar
				// skipString reports.
				if got := s.Offset(); got != len(in)-2 {
					t.Errorf("chunk=%d: Offset %d, want %d (the control byte)", chunk, got, len(in)-2)
				}
			}
		})
	}
}
