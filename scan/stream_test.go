package scan

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

// chunkedReader returns one byte per Read until exhausted. Forces every
// Stream primitive to grow the buffer one byte at a time, exercising the
// refill and value-spans-buffer-boundary paths exhaustively.
type chunkedReader struct {
	data []byte
	pos  int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.data[r.pos]
	r.pos++
	return 1, nil
}

// readStreamAny drains a Stream via Any with hint=0 (cold buffer).
func readStreamAny(t *testing.T, in string) any {
	t.Helper()
	var s Stream
	s.Reset(&chunkedReader{data: []byte(in)}, nil)
	v, err := s.Any()
	if err != nil {
		t.Fatalf("Stream.Any: %v", err)
	}
	return v
}

// TestStream_AnyChunked: every byte arrives via a separate Read; the
// result must still match stdlib's all-at-once parse.
func TestStream_AnyChunked(t *testing.T) {
	t.Parallel()
	for _, tc := range anyCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var want any
			if err := json.Unmarshal([]byte(tc.in), &want); err != nil {
				t.Fatalf("stdlib: %v", err)
			}
			got := readStreamAny(t, tc.in)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("mismatch\n got: %#v\nwant: %#v", got, want)
			}
		})
	}
}

func TestStream_StringChunked(t *testing.T) {
	t.Parallel()
	for _, tc := range stringHappyCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var want string
			if err := json.Unmarshal([]byte(tc.in), &want); err != nil {
				t.Fatalf("stdlib: %v", err)
			}
			var s Stream
			s.Reset(&chunkedReader{data: []byte(tc.in)}, nil)
			got, err := s.String()
			if err != nil {
				t.Fatalf("Stream.String: %v", err)
			}
			if got != want {
				t.Errorf("mismatch got=%q want=%q", got, want)
			}
		})
	}
}

func TestStream_NumberChunked(t *testing.T) {
	t.Parallel()
	for _, in := range floatCases {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			var want float64
			if err := json.Unmarshal([]byte(in), &want); err != nil {
				t.Fatalf("stdlib: %v", err)
			}
			var s Stream
			s.Reset(&chunkedReader{data: []byte(in)}, nil)
			got, err := s.Float64()
			if err != nil {
				t.Fatalf("Stream.Float64: %v", err)
			}
			if got != want {
				t.Errorf("mismatch got=%g want=%g", got, want)
			}
		})
	}
}

// TestStream_HintZeroVsHinted: the result must be identical regardless of
// whether the caller pre-sized the buffer.
func TestStream_HintEquivalence(t *testing.T) {
	t.Parallel()
	in := []byte(`{"k":[1,2,3],"s":"hello"}`)
	for _, hint := range []int{0, 1, 4, 16, 1024} {
		var s Stream
		s.Reset(bytes.NewReader(in), make([]byte, 0, hint))
		got, err := s.Any()
		if err != nil {
			t.Fatalf("hint=%d: %v", hint, err)
		}
		var want any
		if err := json.Unmarshal(in, &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("hint=%d mismatch\n got: %#v\nwant: %#v", hint, got, want)
		}
	}
}

// TestStream_TruncatedInput: reader exhausts mid-token. Must surface an
// error, not panic.
func TestStream_TruncatedInput(t *testing.T) {
	t.Parallel()
	cases := []string{
		`{"k":1,"s":"hel`, // string mid-flight
		`{"k":12`,         // number mid-flight
		`[1,2,`,           // array mid-flight
		`tru`,             // literal mid-flight
		``,                // empty
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			var s Stream
			s.Reset(strings.NewReader(in), nil)
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panicked: %v", r)
				}
			}()
			_, err := s.Any()
			if err == nil {
				t.Error("expected error on truncated input")
			}
		})
	}
}

// TestStream_ReadMoreKeepZero_GrowsWithoutShift: keep==0 must allocate
// a bigger backing if the current buffer is full, but offsets stay valid
// in the original coordinate system (no shift).
func TestStream_ReadMoreKeepZero_GrowsWithoutShift(t *testing.T) {
	t.Parallel()
	var s Stream
	r := bytes.NewReader([]byte("ABCDEFGHIJKLMNOPQRST"))
	s.Reset(r, make([]byte, 0, 4))
	for len(s.Bytes()) < 8 {
		if err := s.ReadMore(0); err != nil {
			t.Fatalf("ReadMore(0): %v", err)
		}
	}
	if len(s.Bytes()) < 8 {
		t.Fatalf("expected ≥8 bytes after grow, got %d", len(s.Bytes()))
	}
	if s.Bytes()[0] != 'A' {
		t.Errorf("keep=0 shifted: buf[0]=%c", s.Bytes()[0])
	}
}

// TestStream_ReadMoreKeepFull_ResetsBuffer: keep == len(buf) clears
// the buffer entirely; next Read refills from offset 0.
func TestStream_ReadMoreKeepFull_ResetsBuffer(t *testing.T) {
	t.Parallel()
	var s Stream
	r := bytes.NewReader([]byte("ABCDEFGHIJKLMNOPQRST"))
	s.Reset(r, make([]byte, 0, 8))
	if err := s.ReadMore(0); err != nil {
		t.Fatalf("first ReadMore: %v", err)
	}
	prefix := string(s.Bytes())
	if prefix == "" {
		t.Fatal("buffer empty after first ReadMore")
	}
	if err := s.ReadMore(len(s.Bytes())); err != nil {
		t.Fatalf("ReadMore(len): %v", err)
	}
	if string(s.Bytes()) == prefix {
		t.Errorf("buffer didn't advance: still %q", s.Bytes())
	}
}

// TestStream_ReadMoreKeepMid_Memmoves: 0 < keep < len(buf) memmoves
// the tail to offset 0 and refills behind it. Bytes at the original
// keep offset must now be at offset 0.
func TestStream_ReadMoreKeepMid_Memmoves(t *testing.T) {
	t.Parallel()
	var s Stream
	r := bytes.NewReader([]byte("ABCDEFGHIJKLMNOPQRSTUVWXYZ"))
	s.Reset(r, make([]byte, 0, 8))
	if err := s.ReadMore(0); err != nil {
		t.Fatalf("first ReadMore: %v", err)
	}
	if len(s.Bytes()) < 4 {
		t.Skipf("need ≥4 bytes loaded; got %d", len(s.Bytes()))
	}
	keepStart := s.Bytes()[4]
	if err := s.ReadMore(4); err != nil {
		t.Fatalf("ReadMore(4): %v", err)
	}
	if s.Bytes()[0] != keepStart {
		t.Errorf("memmove broken: buf[0]=%c want %c", s.Bytes()[0], keepStart)
	}
}

// TestStream_ShiftDisabled_GrowOnly: with Shift=false, ReadMore must
// behave as if keep=0 regardless of the caller's keep argument. Used
// by RawJSON capture and json.Unmarshal fallback paths.
func TestStream_ShiftDisabled_GrowOnly(t *testing.T) {
	t.Parallel()
	var s Stream
	r := bytes.NewReader([]byte("ABCDEFGHIJKLMNOPQRSTUVWXYZ"))
	s.Reset(r, make([]byte, 0, 8))
	s.Shift = false
	if err := s.ReadMore(0); err != nil {
		t.Fatalf("ReadMore: %v", err)
	}
	originalLen := len(s.Bytes())
	if originalLen == 0 {
		t.Fatal("empty buffer")
	}
	if err := s.ReadMore(4); err != nil {
		t.Fatalf("ReadMore with shift=false: %v", err)
	}
	if len(s.Bytes()) < originalLen {
		t.Errorf("Shift=false caused shift: lenBefore=%d lenAfter=%d", originalLen, len(s.Bytes()))
	}
	if s.Bytes()[0] != 'A' {
		t.Errorf("Shift=false shifted anyway: buf[0]=%c want A", s.Bytes()[0])
	}
}

// TestStream_EOFAfterContent: reader returns content then io.EOF on the
// same call (io.Reader interface allows this). Stream must consume the
// content and surface EOF on the NEXT call only.
func TestStream_EOFAfterContent(t *testing.T) {
	t.Parallel()
	var s Stream
	r := bytes.NewReader([]byte("ABC"))
	s.Reset(r, make([]byte, 0, 8))
	if err := s.ReadMore(0); err != nil {
		t.Fatalf("first ReadMore: %v", err)
	}
	if string(s.Bytes()) != "ABC" {
		t.Errorf("first ReadMore content = %q", s.Bytes())
	}
	err := s.ReadMore(0)
	if err != io.ErrUnexpectedEOF {
		t.Errorf("second ReadMore err = %v, want io.ErrUnexpectedEOF", err)
	}
}

// TestStream_PathologicalZeroRead: a reader returning (0, nil) must not
// spin; Stream surfaces ErrUnexpectedEOF.
func TestStream_PathologicalZeroRead(t *testing.T) {
	t.Parallel()
	var s Stream
	s.Reset(zeroReader{}, make([]byte, 0, 8))
	err := s.ReadMore(0)
	if err != io.ErrUnexpectedEOF {
		t.Errorf("zero-Read err = %v, want io.ErrUnexpectedEOF", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { return 0, nil }

// TestIntegerScanNoShiftRefill: Int64/Uint64 must stay position-correct
// when a mid-number refill happens in no-shift mode. ReadMore coerces
// keep to 0 when !s.Shift and moves no bytes, so the local cursor must
// NOT reset to 0 — resetting re-reads already-consumed digits.
func TestIntegerScanNoShiftRefill(t *testing.T) {
	t.Parallel()
	t.Run("int64", func(t *testing.T) {
		t.Parallel()
		var s Stream
		s.Reset(strings.NewReader("12345 "), make([]byte, 0, 3))
		s.Shift = false
		n, err := s.Int64()
		if err != nil {
			t.Fatal(err)
		}
		if n != 12345 {
			t.Errorf("Int64 = %d, want 12345", n)
		}
		if s.Pos != 5 {
			t.Errorf("Pos = %d, want 5", s.Pos)
		}
	})
	t.Run("int64 negative", func(t *testing.T) {
		t.Parallel()
		var s Stream
		s.Reset(strings.NewReader("-1234 "), make([]byte, 0, 3))
		s.Shift = false
		n, err := s.Int64()
		if err != nil {
			t.Fatal(err)
		}
		if n != -1234 {
			t.Errorf("Int64 = %d, want -1234", n)
		}
	})
	t.Run("uint64", func(t *testing.T) {
		t.Parallel()
		var s Stream
		s.Reset(strings.NewReader("67890 "), make([]byte, 0, 3))
		s.Shift = false
		n, err := s.Uint64()
		if err != nil {
			t.Fatal(err)
		}
		if n != 67890 {
			t.Errorf("Uint64 = %d, want 67890", n)
		}
		if s.Pos != 5 {
			t.Errorf("Pos = %d, want 5", s.Pos)
		}
	})
}

// TestFloatNumberBufBounded pins the compacting mid-number refill: a
// float/number straddling window edges must refill from the value start, not
// grow-only from 0 — else every mid-number refill lands with len == cap and
// DOUBLES the buffer (a 64 B buffer ballooned to 1 MB on a 50k short-float
// stream before the fix). Same class the skip-tree compaction already fixed.
func TestFloatNumberBufBounded(t *testing.T) {
	t.Parallel()
	build := func(elem string) string {
		var sb strings.Builder
		sb.WriteByte('[')
		for i := range 50000 {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(elem) // 16 chars — straddles the 64 B window
		}
		sb.WriteByte(']')
		return sb.String()
	}
	drive := func(t *testing.T, data string, scan func(s *Stream) error) {
		var s Stream
		s.Reset(strings.NewReader(data), make([]byte, 0, 64))
		if err := s.SkipSpace(); err != nil {
			t.Fatal(err)
		}
		s.Pos++ // consume '['
		for {
			if err := s.SkipSpace(); err != nil {
				t.Fatal(err)
			}
			if err := scan(&s); err != nil {
				t.Fatal(err)
			}
			if err := s.SkipSpace(); err != nil {
				t.Fatal(err)
			}
			if s.Pos < len(s.Bytes()) && s.Bytes()[s.Pos] == ',' {
				s.Pos++
				continue
			}
			break
		}
		if c := cap(s.Bytes()); c > 4096 {
			t.Fatalf("buffer ballooned to cap=%d (started 64) — compacting refill regressed", c)
		}
	}
	t.Run("float64", func(t *testing.T) {
		t.Parallel()
		drive(t, build("123456789.123456"), func(s *Stream) error { _, err := s.Float64(); return err })
	})
	t.Run("number", func(t *testing.T) {
		t.Parallel()
		drive(t, build("123456789.123456"), func(s *Stream) error { _, err := s.Number(); return err })
	})
}

// TestFloatScanNoShiftRefill mirrors TestIntegerScanNoShiftRefill for the
// number path: under Shift=false the compacting refill must move no bytes and
// keep the cursor, so a value split across refills still scans correctly.
func TestFloatScanNoShiftRefill(t *testing.T) {
	t.Parallel()
	t.Run("float64", func(t *testing.T) {
		t.Parallel()
		var s Stream
		s.Reset(strings.NewReader("3.14159 "), make([]byte, 0, 3))
		s.Shift = false
		v, err := s.Float64()
		if err != nil {
			t.Fatal(err)
		}
		if v != 3.14159 {
			t.Errorf("Float64 = %v, want 3.14159", v)
		}
		if s.Pos != 7 {
			t.Errorf("Pos = %d, want 7", s.Pos)
		}
	})
	t.Run("number", func(t *testing.T) {
		t.Parallel()
		var s Stream
		s.Reset(strings.NewReader("-12.5e3 "), make([]byte, 0, 3))
		s.Shift = false
		n, err := s.Number()
		if err != nil {
			t.Fatal(err)
		}
		if n != "-12.5e3" {
			t.Errorf("Number = %q, want -12.5e3", n)
		}
	})
}

// TestStreamSkipValue_MatchesBytes pins stream SkipValue against the bytes
// tree over compact + pretty-printed values and truncations — the stream
// skipObject comma branch used to miss the separator whitespace skip, so any
// indented object with 2+ keys failed ErrExpectString (never caught: stream
// skip was only ever exercised on compact payloads).
func TestStreamSkipValue_MatchesBytes(t *testing.T) {
	t.Parallel()
	seeds := []string{
		`{"a":1,"b":2}`,
		`{"a":{"x":true,"y":[1,2]},"b":"s","c":null}`,
		`[{"a":1,"b":2},{"c":3}]`,
	}
	var cases [][]byte
	for _, s := range seeds {
		cases = append(cases, []byte(s))
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			t.Fatal(err)
		}
		for _, indent := range []string{" ", "  ", "\t"} {
			pretty, err := json.MarshalIndent(v, "", indent)
			if err != nil {
				t.Fatal(err)
			}
			cases = append(cases, pretty)
		}
		for cut := range len(s) {
			cases = append(cases, []byte(s[:cut]))
		}
	}
	for _, in := range cases {
		wantPos, wantErr := SkipValue(in, 0)
		for _, chunk := range []int{1, 7, 64} {
			var s Stream
			s.Reset(iotest.OneByteReader(bytes.NewReader(in)), make([]byte, 0, chunk))
			gotErr := s.SkipValue()
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("stream(%q, chunk=%d) err=%v, bytes err=%v", in, chunk, gotErr, wantErr)
			}
			if wantErr == nil && s.Offset() != wantPos {
				t.Fatalf("stream(%q, chunk=%d) offset=%d, bytes pos=%d", in, chunk, s.Offset(), wantPos)
			}
		}
	}
}

// TestStreamStringSurrogateAcrossRefill pins the stream stringSlow surrogate
// fix: a \uXXXX\uXXXX pair straddling a tiny-buffer refill boundary must
// assemble into the astral rune, not split into two lone surrogates (😀 → ��).
// Positioned at every offset so the pair lands across a ReadMore at some cap;
// checked against the (fully-buffered, correct) bytes path. Regression guard for
// a bug the escape-free fuzz/bench payloads never exercised.
func TestStreamStringSurrogateAcrossRefill(t *testing.T) {
	t.Parallel()
	for pad := 0; pad < 48; pad++ {
		body := strings.Repeat("a", pad) + `😀` + strings.Repeat("b", 6)
		payload := `"` + body + `"`
		want, _, err := String([]byte(payload), 0)
		if err != nil {
			t.Fatalf("pad=%d bytes String: %v", pad, err)
		}
		for _, cap := range []int{8, 10, 13, 16, 32, 512} {
			var s Stream
			s.Reset(strings.NewReader(payload), make([]byte, 0, cap))
			got, err := s.String()
			if err != nil {
				t.Fatalf("pad=%d cap=%d stream String: %v", pad, cap, err)
			}
			if got != want {
				t.Fatalf("pad=%d cap=%d: stream=%q != bytes=%q", pad, cap, got, want)
			}
		}
	}
}
