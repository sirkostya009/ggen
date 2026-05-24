package scan

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
)

// chunkedReader returns one byte per Read until exhausted. Forces every
// Stream primitive to grow the buffer one byte at a time, exercising the
// Ensure/grow path and value-spans-buffer-boundary paths exhaustively.
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
	for _, tc := range anyCases {
		t.Run(tc.name, func(t *testing.T) {
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
	for _, tc := range stringHappyCases {
		t.Run(tc.name, func(t *testing.T) {
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
	for _, in := range floatCases {
		t.Run(in, func(t *testing.T) {
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
	cases := []string{
		`{"k":1,"s":"hel`, // string mid-flight
		`{"k":12`,         // number mid-flight
		`[1,2,`,           // array mid-flight
		`tru`,             // literal mid-flight
		``,                // empty
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
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
	var s Stream
	s.Reset(zeroReader{}, make([]byte, 0, 8))
	err := s.ReadMore(0)
	if err != io.ErrUnexpectedEOF {
		t.Errorf("zero-Read err = %v, want io.ErrUnexpectedEOF", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { return 0, nil }
