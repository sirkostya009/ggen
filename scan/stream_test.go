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
	v, _, err := s.Any(0)
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
			got, _, err := s.String(0)
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
			got, _, err := s.Float64(0)
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
		got, _, err := s.Any(0)
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
			_, _, err := s.Any(0)
			if err == nil {
				t.Error("expected error on truncated input")
			}
		})
	}
}
