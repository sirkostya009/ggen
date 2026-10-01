package ggen

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// chunkedReader returns one byte per Read until it runs out. A Stream keeps a
// window, a buffered slice of the reader, and a refill reads more into it.
// With one byte per Read, every Stream function must refill at every byte,
// and every value crosses the edge of the buffered data.
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

// readStreamAny decodes in with Stream.Any, starting with no buffer.
func readStreamAny(t *testing.T, in string) any {
	t.Helper()
	var s Stream
	s.Reset(&chunkedReader{data: []byte(in)}, nil)
	v, err := s.Any(true)
	if err != nil {
		t.Fatalf("Stream.Any: %v", err)
	}
	return v
}

// TestStream_AnyChunked feeds every byte through a separate Read. The result
// must still match the standard library parsing the whole input at once.
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
			got, err := s.String(true)
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

// TestStream_HintEquivalence checks that the result is the same whatever
// buffer size the caller passes in.
func TestStream_HintEquivalence(t *testing.T) {
	t.Parallel()
	in := []byte(`{"k":[1,2,3],"s":"hello"}`)
	for _, hint := range []int{0, 1, 4, 16, 1024} {
		var s Stream
		s.Reset(bytes.NewReader(in), make([]byte, 0, hint))
		got, err := s.Any(true)
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

// TestStream_TruncatedInput checks a reader that ends in the middle of a
// value. Stream must return an error, not panic.
func TestStream_TruncatedInput(t *testing.T) {
	t.Parallel()
	cases := []string{
		`{"k":1,"s":"hel`, // inside a string
		`{"k":12`,         // inside a number
		`[1,2,`,           // inside an array
		`tru`,             // inside a literal
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
			_, err := s.Any(true)
			if err == nil {
				t.Error("expected error on truncated input")
			}
		})
	}
}

// TestStream_ReadMoreKeepZero_GrowsWithoutShift checks ReadMore(0). When the
// buffer is full, it must allocate a bigger one. It must not move the bytes,
// so earlier offsets stay valid.
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

// TestStream_ReadMoreKeepFull_ResetsBuffer checks ReadMore(len(buf)). It
// drops every buffered byte, and the next Read fills from offset 0.
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

// TestStream_ReadMoreKeepMid_Memmoves checks ReadMore(keep) with
// 0 < keep < len(buf). It moves the bytes from keep onward to offset 0 and
// reads more after them. The byte that was at offset keep must now be at
// offset 0.
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

// TestStream_EOFAfterContent checks a reader that returns data together with
// io.EOF in the same call, which io.Reader allows. Stream must keep the data
// and report the end only on the next call, as io.ErrUnexpectedEOF. ReadMore
// keeps no EOF flag. It sees the end again by calling Read on the drained
// reader, which keeps returning io.EOF. So the test covers both reader
// kinds: EOF on a separate call (bytes.Reader) and data plus EOF in one call
// (iotest.DataErrReader).
func TestStream_EOFAfterContent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		r    io.Reader
	}{
		{"eof on separate call", bytes.NewReader([]byte("ABC"))},
		{"data+eof same call", iotest.DataErrReader(bytes.NewReader([]byte("ABC")))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var s Stream
			s.Reset(tc.r, make([]byte, 0, 8))
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
		})
	}
}

// TestStream_PathologicalZeroRead checks a reader that returns (0, nil).
// Stream must not loop forever. It returns io.ErrUnexpectedEOF.
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

// TestFloatNumberBufBounded checks the refill in the middle of a number. When
// a float crosses the end of the buffered data, the refill must keep only
// the bytes from the start of the number and drop the rest. If it kept the
// whole buffer instead, every refill would find the buffer full and double
// it. Then a 64-byte buffer would grow to megabytes over a long array of
// short floats.
func TestFloatNumberBufBounded(t *testing.T) {
	t.Parallel()
	build := func(elem string) string {
		var sb strings.Builder
		sb.WriteByte('[')
		for i := range 50000 {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(elem) // 16 bytes, so values cross the 64-byte edge
		}
		sb.WriteByte(']')
		return sb.String()
	}
	drive := func(t *testing.T, data string, scan func(s *Stream) error) {
		t.Helper()
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

// TestStreamCaptureValue checks CaptureValue. The captured bytes must be exact
// when the value arrives in small reads. Capture must stop at the real end of
// the value when more input follows. Errors must show up only at EOF. When a
// capture starts in the middle of the buffer, the first refill must drop the
// bytes already consumed and reuse that space. It must not carry them along
// through every buffer growth.
func TestStreamCaptureValue(t *testing.T) {
	t.Parallel()
	values := []string{
		`123`, `-45.67e8`, `"plain"`, `"esc\n\tA😀"`, `true`, `null`,
		`[1,2,3,[4,5,[6,7]],8]`, `{"a":1,"b":[2,3],"c":{"d":"e"}}`,
		`"` + strings.Repeat("a", 5000) + `"`,
	}
	for _, v := range values {
		// One byte per Read, so the value is split at every offset.
		var s Stream
		s.Reset(&chunkedReader{data: []byte(v)}, make([]byte, 0, 8))
		got, err := s.CaptureValue()
		if err != nil {
			t.Fatalf("%.20q chunked: %v", v, err)
		}
		if string(got) != v {
			t.Fatalf("%.20q chunked: got %.20q", v, got)
		}
		// More input after the value. Capture must stop at the value's end.
		var s2 Stream
		s2.Reset(strings.NewReader(v+` ,"next"`), make([]byte, 0, 16))
		got2, err := s2.CaptureValue()
		if err != nil {
			t.Fatalf("%.20q trailing: %v", v, err)
		}
		if string(got2) != v {
			t.Fatalf("%.20q trailing: got %.20q", v, got2)
		}
		if s2.Offset() != len(v) {
			t.Fatalf("%.20q trailing: Offset=%d want %d", v, s2.Offset(), len(v))
		}
	}

	t.Run("prefix compaction", func(t *testing.T) {
		t.Parallel()
		// Two values of about 700 bytes go through a 1024-byte buffer. The
		// second capture starts at Pos 702 or so, with the buffer full. The
		// first refill must drop the consumed bytes. Then the second value
		// fits in the freed space and the buffer never grows. Keeping the
		// consumed bytes would double the buffer to 2048.
		v := `"` + strings.Repeat("a", 698) + `"`
		payload := `[` + v + `,` + v + `]`
		var s Stream
		s.Reset(strings.NewReader(payload), make([]byte, 0, 1024))
		if err := s.ArrayOpen(); err != nil {
			t.Fatal(err)
		}
		for k := range 2 {
			got, err := s.CaptureValue()
			if err != nil {
				t.Fatalf("capture %d: %v", k, err)
			}
			if string(got) != v {
				t.Fatalf("capture %d: got %.20q", k, got)
			}
			if s.Pos < len(s.Bytes()) && s.Bytes()[s.Pos] == ',' {
				s.Pos++
			}
		}
		if c := cap(s.Bytes()); c != 1024 {
			t.Fatalf("buffer grew to cap=%d (want 1024) — prefix compaction regressed", c)
		}
	})

	t.Run("errors at EOF", func(t *testing.T) {
		t.Parallel()
		for _, in := range []string{`"abc`, `{"a":1`, `[1,2,@]`, ``} {
			var s Stream
			s.Reset(strings.NewReader(in), make([]byte, 0, 8))
			if _, err := s.CaptureValue(); err == nil {
				t.Fatalf("%q: expected error", in)
			}
		}
	})

	t.Run("live reader", func(t *testing.T) {
		t.Parallel()
		// A keep-alive connection sends the whole value and then nothing
		// more for a while. The capture must finish without another Read.
		// On a real connection that Read would block forever.
		// liveChunkReader returns an error instead.
		for _, chunks := range liveReaderCases {
			want := strings.Join(chunks, "")
			var s Stream
			s.Reset(&liveChunkReader{chunks: chunks}, nil)
			got, err := s.CaptureValue()
			if err != nil {
				t.Fatalf("%q: %v", want, err)
			}
			if string(got) != want {
				t.Fatalf("%q: got %q", want, got)
			}
		}
		// A bare number at the end of the buffered data is really not
		// finished. "123" may go on as "1234", so one more Read is needed. A
		// live reader must send a delimiter or EOF. This cannot be avoided.
		var s Stream
		s.Reset(&liveChunkReader{chunks: []string{`123`}}, nil)
		if _, err := s.CaptureValue(); !errors.Is(err, errBlockingRead) {
			t.Fatalf("number: got %v, want the blocking-read probe error", err)
		}
	})
}

// liveReaderCases holds values split into chunks, one chunk per Read. The
// SIMD version of the test in simd_skip_test.go uses them too.
var liveReaderCases = [][]string{
	{`{"a":1}`},
	{`{"a`, `":1}`},
	{`"abc`, `def"`},
	{`[1,`, `2,3]`},
	{`tr`, `ue`},
	{`nu`, `ll`},
}

// TestStreamInt_LeadingZeroPeekTransientError checks the read after a leading
// "0". Int64 reads one more byte to reject a leading zero like "01". If that
// read fails with a non-EOF error, the error must be returned. Ignoring it
// would let a later read go on with the digits, and "01" would decode as 1
// with a nil error.
func TestStreamInt_LeadingZeroPeekTransientError(t *testing.T) {
	t.Parallel()
	errTransient := errors.New("transient")
	// "0", then a non-EOF error, then "1". The whole input "01" is invalid.
	mk := func() *flakyReader {
		return &flakyReader{seq: []any{"0", errTransient, "1"}}
	}
	var s Stream
	s.Reset(mk(), nil)
	if _, err := s.Int64(); !errors.Is(err, errTransient) {
		t.Errorf("Int64: got %v, want the transient error propagated", err)
	}
	s.Reset(mk(), nil)
	if _, err := s.Uint64(); !errors.Is(err, errTransient) {
		t.Errorf("Uint64: got %v, want the transient error propagated", err)
	}
	// A bare "0" followed by EOF is still a valid number. Here EOF is not an
	// error.
	s.Reset(strings.NewReader("0"), nil)
	if v, err := s.Int64(); err != nil || v != 0 {
		t.Errorf("bare 0: got %d, %v", v, err)
	}
	s.Reset(strings.NewReader("0"), nil)
	if v, err := s.Uint64(); err != nil || v != 0 {
		t.Errorf("bare 0 uint: got %d, %v", v, err)
	}
}

// TestStreamTransientErrorNeverSilentNorMislabeled runs every value function
// against a reader that fails once with (0, err) at each byte position. Two
// bugs are checked. First, a number must not stop at the error and return
// the digits read so far with a nil error, so "12345" must not become 1234.
// Second, string, bool, literal and skip code must not replace the reader
// error with a JSON error like ErrUnterminated, ErrBadBool or ErrBadObject.
// The caller must get the reader's own error, as ReadMore documents.
func TestStreamTransientErrorNeverSilentNorMislabeled(t *testing.T) {
	t.Parallel()
	errTransient := errTransientHiccup
	cases := []struct {
		name string
		data string
		run  func(*Stream) (any, error)
	}{
		{"Int64", "12345", func(s *Stream) (any, error) { return s.Int64() }},
		{"Uint64", "98765", func(s *Stream) (any, error) { return s.Uint64() }},
		{"Float64", "123.5", func(s *Stream) (any, error) { return s.Float64() }},
		{"Float32", "123.5", func(s *Stream) (any, error) { return s.Float32() }},
		{"Number", "777", func(s *Stream) (any, error) { return s.Number() }},
		{"String", `"abcdef"`, func(s *Stream) (any, error) { return s.String(true) }},
		{"StringEscape", `"a\nb\u0041c"`, func(s *Stream) (any, error) { return s.String(true) }},
		{"KeyView", `"key"`, func(s *Stream) (any, error) { return s.KeyView(true) }},
		{"Bool", "true", func(s *Stream) (any, error) { return s.Bool() }},
		{"Any", "123.5", func(s *Stream) (any, error) { return s.Any(true) }},
		{"AnyObject", `{"a":[1,"x"]}`, func(s *Stream) (any, error) { return s.Any(true) }},
		{"SkipNumber", "12345", func(s *Stream) (any, error) { return nil, s.SkipValue() }},
		{"SkipObject", `{"a":1,"b":2}`, func(s *Stream) (any, error) { return nil, s.SkipValue() }},
		{"SkipString", `"abcdef"`, func(s *Stream) (any, error) { return nil, s.SkipValue() }},
		{"SkipNull", "null", func(s *Stream) (any, error) { return nil, s.SkipValue() }},
	}
	for _, tc := range cases {
		for at := 1; at <= len(tc.data)+1; at++ {
			r := &hiccupReader{data: []byte(tc.data), hiccupAt: at}
			var s Stream
			s.Reset(r, make([]byte, 0, 1))
			v, err := tc.run(&s)
			if r.calls < at {
				continue // the value ended before the failing Read
			}
			if err == nil {
				t.Errorf("%s hiccup@%d: got (%v, nil) — silent truncation", tc.name, at, v)
				continue
			}
			if !errors.Is(err, errTransient) {
				t.Errorf("%s hiccup@%d: err = %v (%T), want the transient error", tc.name, at, err, err)
			}
		}
	}
	// Control cases. At EOF, values still end normally, and real JSON errors
	// are still reported as JSON errors.
	var s Stream
	s.Reset(strings.NewReader("12345"), make([]byte, 0, 1))
	if v, err := s.Int64(); err != nil || v != 12345 {
		t.Errorf("clean Int64 = %v, %v", v, err)
	}
	s.Reset(strings.NewReader(`"abc`), make([]byte, 0, 1))
	if _, err := s.String(true); !errors.Is(err, ErrUnterminated) {
		t.Errorf("unterminated: %v, want ErrUnterminated", err)
	}
	s.Reset(strings.NewReader("01"), make([]byte, 0, 1))
	if _, err := s.Int64(); !errors.Is(err, ErrBadNumber) {
		t.Errorf("leading zero: %v, want ErrBadNumber", err)
	}
}

// The Stream number functions first collect every byte in [0-9.eE+-], across
// refills, and then check the grammar. The number ends where the grammar
// says, not where the collected bytes end. So Stream stops where the []byte
// functions stop, at the longest valid number. Rejecting the whole collected
// run would make "1.5.5", "1e5e" and "01" fail on Stream but succeed on
// []byte input.
func TestStreamNumberMaximalMunchMatchesBytes(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"01", "1.5.5", "1e5e", "0", "-0", "12345", "1.5", "1e5", "00", "1-2"} {
		wantV, wantN, wantErr := Float64([]byte(in), 0)
		var s Stream
		s.Reset(strings.NewReader(in), make([]byte, 0, 1))
		gotV, gotErr := s.Float64()
		if (wantErr == nil) != (gotErr == nil) {
			t.Errorf("Float64(%q): bytes err %v, stream err %v", in, wantErr, gotErr)
			continue
		}
		if wantErr != nil {
			continue
		}
		if gotV != wantV || s.Pos != wantN {
			t.Errorf("Float64(%q): stream (%v, pos %d), bytes (%v, pos %d)", in, gotV, s.Pos, wantV, wantN)
		}
		// Number must stop at the same place.
		wantNum, wantNumN, numErr := Number([]byte(in), 0)
		s.Reset(strings.NewReader(in), make([]byte, 0, 1))
		gotNum, gotNumErr := s.Number()
		if (numErr == nil) != (gotNumErr == nil) {
			t.Errorf("Number(%q): bytes err %v, stream err %v", in, numErr, gotNumErr)
			continue
		}
		if numErr == nil && (gotNum != wantNum || s.Pos != wantNumN) {
			t.Errorf("Number(%q): stream (%q, pos %d), bytes (%q, pos %d)", in, gotNum, s.Pos, wantNum, wantNumN)
		}
	}
}

// A live reader, one that never sends EOF, has sent a complete but broken
// value. CaptureValue must return an error, not hang. More bytes cannot fix
// an error that sits before bytes already in the buffer, so CaptureValue must
// not call Read again waiting for them.
func TestStreamCaptureValue_LiveMalformedDoesNotHang(t *testing.T) {
	t.Parallel()
	for _, payload := range []string{`{"a":}`, `[1,]x`, `{"a" 1}`, `[1 2]`, `tru3`} {
		done := make(chan error, 1)
		block := make(chan struct{})
		go func() {
			var s Stream
			s.Reset(&liveReader{data: []byte(payload), block: block}, make([]byte, 0, 64))
			_, err := s.CaptureValue()
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: got nil error, want a parse error", payload)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("%s: CaptureValue hung on a live reader", payload)
		}
		close(block)
	}
}

// A value that is only cut short on a live reader must still wait, because
// more bytes can complete it. This is the control case. It shows the check
// for broken values does not give up too early.
func TestStreamCaptureValue_LiveTruncatedStillWaits(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		var s Stream
		s.Reset(&liveReader{data: []byte(`{"a":1`), block: block}, make([]byte, 0, 64))
		_, err := s.CaptureValue()
		done <- err
	}()
	select {
	case err := <-done:
		t.Errorf("returned %v on a truncated value; must keep waiting for bytes", err)
	case <-time.After(200 * time.Millisecond):
		// Correct: still waiting.
	}
	close(block)
}

// liveReader returns its data, then blocks until block is closed, like a
// socket with nothing more to send. Then it returns EOF.
type liveReader struct {
	data  []byte
	pos   int
	block chan struct{}
}

func (r *liveReader) Read(p []byte) (int, error) {
	if r.pos < len(r.data) {
		n := copy(p, r.data[r.pos:])
		r.pos += n
		return n, nil
	}
	<-r.block
	return 0, io.EOF
}

// hiccupReader returns one byte per Read, except call number hiccupAt,
// which returns (0, errTransientHiccup). This is a reader error the caller
// can recover from, like a net.Conn read deadline. It is not the end of the
// input.
type hiccupReader struct {
	data     []byte
	pos      int
	calls    int
	hiccupAt int
}

func (r *hiccupReader) Read(p []byte) (int, error) {
	r.calls++
	if r.calls == r.hiccupAt {
		return 0, errTransientHiccup
	}
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:r.pos+1])
	r.pos += n
	return n, nil
}

var errTransientHiccup = errors.New("transient")

// flakyReader returns a sequence of string chunks and errors, one per Read.
type flakyReader struct {
	seq []any // string or error
}

func (r *flakyReader) Read(p []byte) (int, error) {
	if len(r.seq) == 0 {
		return 0, io.EOF
	}
	head := r.seq[0]
	r.seq = r.seq[1:]
	switch h := head.(type) {
	case error:
		return 0, h
	case *bothRead:
		return copy(p, h.data), h.err
	}
	return copy(p, head.(string)), nil
}

// liveChunkReader returns one chunk per Read, then an error on any later
// Read. It stands in for a live socket, where a Read after the value has
// arrived would block forever.
type liveChunkReader struct {
	chunks []string
}

var errBlockingRead = errors.New("Read after value delivered — a live reader would block here")

func (r *liveChunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, errBlockingRead
	}
	n := copy(p, r.chunks[0])
	if n < len(r.chunks[0]) {
		r.chunks[0] = r.chunks[0][n:]
	} else {
		r.chunks = r.chunks[1:]
	}
	return n, nil
}

// TestStreamSkipValue_MatchesBytes checks Stream.SkipValue against SkipValue
// on []byte input. Inputs are compact values, indented values and
// truncations. Indented objects matter: after each comma, the skip code must
// also skip the whitespace before the next key.
func TestStreamSkipValue_MatchesBytes(t *testing.T) {
	t.Parallel()
	seeds := []string{
		`{"a":1,"b":2}`,
		`{"a":{"x":true,"y":[1,2]},"b":"s","c":null}`,
		`[{"a":1,"b":2},{"c":3}]`,
	}
	// Object keys that are not strings. Stream must report ErrBadObject,
	// like []byte input does, whether or not the key is already in the
	// buffer.
	cases := [][]byte{
		[]byte(`{5:1}`),
		[]byte(`{"a":1,5:2}`),
		[]byte(`{true:1}`),
		[]byte(`[{"a":1},{5:2}]`),
		[]byte(`{ 5:1}`),
	}
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
			if gotErr != wantErr {
				t.Fatalf("stream(%q, chunk=%d) err=%v, bytes err=%v", in, chunk, gotErr, wantErr)
			}
			// Check the offset on error too. Generated decoders with
			// ignoreunknown or allowdups put it in ParseError.Pos.
			if s.Offset() != wantPos {
				t.Fatalf("stream(%q, chunk=%d) offset=%d, bytes pos=%d (%v)", in, chunk, s.Offset(), wantPos, wantErr)
			}
		}
	}
}

// TestStreamStringSurrogateAcrossRefill checks a 4-byte character that
// crosses the end of the buffered data, with small buffers. The character is
// placed at every offset, so for some buffer size it lands across a refill.
// Stream must decode it the same as String on the whole input.
func TestStreamStringSurrogateAcrossRefill(t *testing.T) {
	t.Parallel()
	for pad := range 48 {
		body := strings.Repeat("a", pad) + `😀` + strings.Repeat("b", 6)
		payload := `"` + body + `"`
		want, _, err := String([]byte(payload), 0, true)
		if err != nil {
			t.Fatalf("pad=%d bytes String: %v", pad, err)
		}
		for _, cap := range []int{8, 10, 13, 16, 32, 512} {
			var s Stream
			s.Reset(strings.NewReader(payload), make([]byte, 0, cap))
			got, err := s.String(true)
			if err != nil {
				t.Fatalf("pad=%d cap=%d stream String: %v", pad, cap, err)
			}
			if got != want {
				t.Fatalf("pad=%d cap=%d: stream=%q != bytes=%q", pad, cap, got, want)
			}
		}
	}
}

// TestStreamStringSlowBufBounded checks that a long escaped string passes
// through a buffer that stays small. stringSlow writes the decoded string
// into its own buffer and does not point into the Stream's buffer. So each
// refill can drop the bytes before the current position and adjust the
// position, as Float64 and Number do. Without that, each refill would find
// the buffer full, because readers fill all free space, and would double it.
// A 64-byte buffer would then grow past the size of the string. Simple
// escapes, \uXXXX escapes and surrogate pairs each have their own refill
// loop in stringSlow, so all three are checked. The value is compared with
// String on the whole input.
func TestStreamStringSlowBufBounded(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"leading-escape-then-plain": "\\n" + strings.Repeat("a", 200000),
		"all-simple-escapes":        strings.Repeat("\\n", 100000),
		"all-unicode-escapes":       strings.Repeat("\\u0041", 40000),
		"all-surrogate-pairs":       strings.Repeat("\\uD83D\\uDE00", 20000),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			payload := `"` + body + `"`
			want, _, err := String([]byte(payload), 0, true)
			if err != nil {
				t.Fatalf("bytes String: %v", err)
			}
			var s Stream
			s.Reset(strings.NewReader(payload), make([]byte, 0, 64))
			got, err := s.String(true)
			if err != nil {
				t.Fatalf("stream String: %v", err)
			}
			if got != want {
				t.Fatalf("stream != bytes")
			}
			if c := cap(s.Bytes()); c > 4096 {
				t.Fatalf("buffer ballooned to cap=%d (started 64) — compacting escape refill regressed", c)
			}
		})
	}
}

// TestStreamStringInvalidUTF8 checks that String, StringView and KeyView on a
// Stream reject invalid UTF-8 with ErrInvalidUTF8, like jsonv2, also across
// refills. A rune is one UTF-8 encoded character of 1 to 4 bytes. A valid
// multi-byte rune split across a refill must still pass, because utf8.Valid
// runs over the whole string, never one chunk at a time.
func TestStreamStringInvalidUTF8(t *testing.T) {
	t.Parallel()
	invalid := []struct{ name, payload string }{
		{"raw_ff", "\"ab\xffcd\""},
		{"truncated_rune", "\"ab\xe2(z\""},
		{"lone_surrogate", `"\uD83D"`},
		{"surrogate_pair_split_escape", `"a\uD83Dz"`},
		{"invalid_after_escape", "\"a\\n\xffz\""},
		{"long_span_invalid", `"` + strings.Repeat("x", 40) + "\xfe" + `"`},
	}
	methods := []struct {
		name string
		fn   func(*Stream, bool) (string, error)
	}{
		{"String", (*Stream).String},
		{"StringView", (*Stream).StringView},
		{"KeyView", (*Stream).KeyView},
	}
	for _, c := range invalid {
		for _, m := range methods {
			for _, bufCap := range []int{8, 64, 512} {
				var s Stream
				s.Reset(&chunkedReader{data: []byte(c.payload)}, make([]byte, 0, bufCap))
				if _, err := m.fn(&s, true); err != ErrInvalidUTF8 {
					t.Errorf("%s/%s cap=%d: want ErrInvalidUTF8, got %v", c.name, m.name, bufCap, err)
				}
			}
		}
	}
	// Valid multi-byte runes split by one-byte reads decode intact.
	valid := []string{"\"héllo wörld żółć\"", "\"a😀b\"", "\"a\\né😀\"", `"żółć"`}
	for _, payload := range valid {
		want, _, err := String([]byte(payload), 0, true)
		if err != nil {
			t.Fatalf("bytes %q: %v", payload, err)
		}
		for _, m := range methods {
			var s Stream
			s.Reset(&chunkedReader{data: []byte(payload)}, make([]byte, 0, 8))
			got, err := m.fn(&s, true)
			if err != nil || got != want {
				t.Errorf("%s(%q): got %q err=%v want %q", m.name, payload, got, err, want)
			}
		}
	}
}

// A Read may return bytes and a non-EOF error together. ReadMore must keep the
// bytes and not report the error, so Int64 reads the new digits. Otherwise
// the number would come back cut short, with the new digits left unread.
func TestStreamReadMore_BytesBeforeError(t *testing.T) {
	t.Parallel()
	var s Stream
	s.Reset(&flakyReader{seq: []any{"1", &bothRead{"23", errFlaky}}}, nil)
	v, err := s.Int64()
	if err != nil || v != 123 {
		t.Errorf("Int64 = %d, %v — want 123 (bytes delivered with the error must be consumed)", v, err)
	}
}

// When SkipSpace fails, Pos must account for the whitespace bytes dropped
// from the buffer. Otherwise Offset() counts the whitespace twice and Pos
// points past len(buf).
func TestStreamSkipSpace_ErrorPos(t *testing.T) {
	t.Parallel()
	var s Stream
	s.Reset(&flakyReader{seq: []any{"  ", errFlaky}}, nil)
	if err := s.SkipSpace(); !errors.Is(err, errFlaky) {
		t.Fatalf("got %v, want the reader error", err)
	}
	if s.Pos > len(s.Bytes()) {
		t.Errorf("Pos %d past len(buf) %d", s.Pos, len(s.Bytes()))
	}
	if got := s.Offset(); got != 2 {
		t.Errorf("Offset = %d, want 2 (whitespace run double-counted)", got)
	}
}

// A reader error while waiting for the low half of a surrogate pair must be
// returned as is. It must not turn into ErrInvalidUTF8 for a lone
// surrogate.
func TestStreamStringSurrogate_TransientError(t *testing.T) {
	t.Parallel()
	var s Stream
	s.Reset(&flakyReader{seq: []any{`"\ud83d`, errFlaky, `\ude00"`}}, nil)
	if _, err := s.String(true); !errors.Is(err, errFlaky) {
		t.Errorf("got %v, want the reader error (not ErrInvalidUTF8)", err)
	}
}

var errFlaky = errors.New("flaky reader")

// bothRead makes flakyReader return bytes and an error from one Read call.
// io.Reader allows this.
type bothRead struct {
	data string
	err  error
}

// intT also implements StreamDecoder. Its []byte half is in decode_test.go.
func (intT) DecodeFromStream(s *Stream) (intT, error) {
	v, err := s.Int64()
	return intT(v), err
}

// sliceT is a StreamDecoder that holds a slice, so buffer reuse can be seen.
// It passes its own slice to the nested Slice call. A generated decoder with
// a slice field reuses it the same way.
type sliceT struct{ Vals []intT }

func (recv sliceT) DecodeFromStream(s *Stream) (sliceT, error) {
	result := recv
	v, err := s.Slice(result.Vals)
	result.Vals = v
	return result, err
}

// TestStreamMethods checks the generic Stream methods. NewStream can be
// chained into them. Value and Slice leave the position right after the
// value, so the caller can keep reading. The []byte functions cannot do
// that.
func TestStreamMethods(t *testing.T) {
	t.Parallel()
	v, err := NewStream(strings.NewReader("42"), nil).Value[intT]()
	if err != nil || v != 42 {
		t.Fatalf("Value = %v, %v; want 42", v, err)
	}

	// Slice does not reject data after the array, so the next value can
	// still be read from the same Stream.
	s := NewStream(strings.NewReader("[1,2] 7"), nil)
	got, err := s.Slice[intT]()
	if err != nil || len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("Slice = %v, %v; want [1 2]", got, err)
	}
	if err := s.SkipSpace(); err != nil {
		t.Fatalf("SkipSpace: %v", err)
	}
	if tail, err := s.Value[intT](); err != nil || tail != 7 {
		t.Errorf("Value after Slice = %v, %v; want 7", tail, err)
	}

	// An empty array decodes to a non-nil empty slice, like UnmarshalSlice
	// and jsonv2.
	if e, err := NewStream(strings.NewReader(" [ ] "), nil).Slice[intT](); err != nil || e == nil || len(e) != 0 {
		t.Errorf("Slice([]) = %#v, %v; want non-nil empty", e, err)
	}

	// Several values in a row from one Stream.
	s = NewStream(strings.NewReader("1 2"), nil)
	first, err := s.Value[intT]()
	if err != nil || first != 1 {
		t.Fatalf("first Value = %v, %v", first, err)
	}
	if err := s.SkipSpace(); err != nil {
		t.Fatalf("SkipSpace: %v", err)
	}
	if second, err := s.Value[intT](); err != nil || second != 2 {
		t.Errorf("second Value = %v, %v; want 2", second, err)
	}

	// A malformed array still errors.
	if _, err := NewStream(strings.NewReader("[1 2]"), nil).Slice[intT](); err == nil {
		t.Error("Slice([1 2]) must fail on the missing comma")
	}
}

// TestStreamSeq checks the Seq iterator. It yields top-level values one after
// another and ends cleanly when the reader runs out. Breaking out early
// leaves the Stream usable. It reuses one value for every iteration, so a
// long run does not allocate.
func TestStreamSeq(t *testing.T) { //nolint:paralleltest // measures allocations
	var got []intT
	for v, err := range NewStream(strings.NewReader("1 2\n3\n"), nil).Seq[intT]() {
		if err != nil {
			t.Fatalf("Seq yielded error: %v", err)
		}
		got = append(got, v)
	}
	if len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Errorf("Seq = %v, want [1 2 3]", got)
	}

	// Empty or whitespace-only input ends at once with no error.
	for _, err := range NewStream(strings.NewReader("  \n "), nil).Seq[intT]() {
		t.Fatalf("Seq over blank input yielded %v", err)
	}

	// A malformed value yields exactly one error, then stops.
	var errs, vals int
	for _, err := range NewStream(strings.NewReader("1 x"), nil).Seq[intT]() {
		if err != nil {
			errs++
		} else {
			vals++
		}
	}
	if vals != 1 || errs != 1 {
		t.Errorf("malformed stream: vals=%d errs=%d, want 1/1", vals, errs)
	}

	// Seq reuses one value across iterations, so a long run reaches zero
	// allocations. Passing a value to Seq gives it slices to reuse from the
	// start.
	var r strings.Reader
	var st Stream
	sbuf := make([]byte, 0, 256)
	drain := func(seed ...sliceT) (n int, last *intT) {
		r.Reset("[1,2,3] [4,5,6] [7,8,9]")
		for v := range st.Reset(&r, sbuf).Seq(seed...) {
			n++
			last = &v.Vals[0]
		}
		return n, last
	}
	if n, _ := drain(); n != 3 {
		t.Fatalf("Seq yielded %d values, want 3", n)
	}
	warm := sliceT{Vals: make([]intT, 0, 8)}
	warmPtr := &warm.Vals[:1][0]
	if n, got := drain(warm); n != 3 || got != warmPtr {
		t.Errorf("rcv.Seq() reuse: n=%d ptr-reused=%v", n, got == warmPtr)
	}
	if allocs := testing.AllocsPerRun(50, func() { drain(warm) }); allocs > 0 {
		t.Errorf("rcv.Seq() allocates %.0f per drain; want 0", allocs)
	}

	// The buffer stays small over a long stream. Once the buffer is full,
	// Seq drops the values already consumed, so refills reuse that space
	// instead of doubling it. Generated decoders refill without dropping
	// bytes, which is where this really matters. That case is
	// TestSeqBufferStaysBounded in integrationtests.
	{
		const n = 2000
		var sb strings.Builder
		for i := range n {
			sb.WriteString(strconv.Itoa(i % 97))
			sb.WriteByte('\n')
		}
		var lr strings.Reader
		lr.Reset(sb.String())
		var ls Stream
		ls.Reset(&lr, make([]byte, 0, 64))
		count := 0
		for _, err := range ls.Seq[intT]() {
			if err != nil {
				t.Fatalf("long stream yielded error at %d: %v", count, err)
			}
			count++
		}
		if count != n {
			t.Errorf("long stream yielded %d values, want %d", count, n)
		}
		if got := cap(ls.Bytes()); got > 256 {
			t.Errorf("buffer grew to cap %d over %d values (started at 64) — compaction not holding", got, n)
		}
	}

	// Breaking out leaves the Stream positioned for further reads.
	s := NewStream(strings.NewReader("1 2 3"), nil)
	for v := range s.Seq[intT]() {
		if v == 1 {
			break
		}
	}
	if err := s.SkipSpace(); err != nil {
		t.Fatalf("SkipSpace after break: %v", err)
	}
	if next, err := s.Value[intT](); err != nil || next != 2 {
		t.Errorf("after break Value = %v, %v; want 2", next, err)
	}
}

// TestStreamBufferReuse checks that Slice reuses the slices it is given: the
// outer slice and each element's own slice. Decoding again into the same
// value then stops allocating.
func TestStreamBufferReuse(t *testing.T) { //nolint:paralleltest // measures allocations
	const payload = `[[1,2,3],[4,5,6]]`
	// Reuse the reader, the Stream and its buffer so the only allocations left
	// to measure are the decode's own.
	var r strings.Reader
	var s Stream
	sbuf := make([]byte, 0, 256)
	decodeInto := func(buf []sliceT) []sliceT {
		r.Reset(payload)
		got, err := s.Reset(&r, sbuf).Slice(buf)
		if err != nil {
			t.Fatalf("Slice: %v", err)
		}
		return got
	}

	first := decodeInto(nil)
	if len(first) != 2 || len(first[0].Vals) != 3 || first[1].Vals[2] != 6 {
		t.Fatalf("first decode wrong: %+v", first)
	}
	outerPtr, elemPtr := &first[0], &first[0].Vals[0]

	second := decodeInto(first)
	if len(second) != 2 || second[1].Vals[2] != 6 {
		t.Fatalf("second decode wrong: %+v", second)
	}
	if &second[0] != outerPtr {
		t.Error("outer slice backing array not reused")
	}
	if &second[0].Vals[0] != elemPtr {
		t.Error("element container not reused — elements were appended blindly")
	}

	// Steady state must not allocate at all.
	buf := second
	if n := testing.AllocsPerRun(50, func() { buf = decodeInto(buf) }); n > 0 {
		t.Errorf("reuse still allocates %.0f per decode; want 0", n)
	}

	// Without a slice to reuse, each decode makes new slices.
	fresh := decodeInto(nil)
	if &fresh[0] == outerPtr {
		t.Error("no-rcv decode must not reuse the previous buffer")
	}
}

// TestStreamArray checks the Array iterator, which yields array elements one
// at a time. It follows the same grammar as Slice. It leaves the position
// after the closing bracket. On error it yields one error and stops. It
// does not collect elements.
func TestStreamArray(t *testing.T) {
	t.Parallel()
	// Agrees with Slice on element values.
	var got []intT
	for v, err := range NewStream(strings.NewReader("[1, 2,3]"), nil).Array[intT]() {
		if err != nil {
			t.Fatalf("Array yielded error: %v", err)
		}
		got = append(got, v)
	}
	if len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Errorf("Array = %v, want [1 2 3]", got)
	}

	// Empty array yields nothing, no error.
	for _, err := range NewStream(strings.NewReader(" [ ] "), nil).Array[intT]() {
		t.Fatalf("Array over [] yielded %v", err)
	}

	// The closing bracket is consumed, so the Stream reads on.
	s := NewStream(strings.NewReader("[1,2] 9"), nil)
	n := 0
	for range s.Array[intT]() {
		n++
	}
	if n != 2 {
		t.Fatalf("Array yielded %d, want 2", n)
	}
	if err := s.SkipSpace(); err != nil {
		t.Fatalf("SkipSpace: %v", err)
	}
	if tail, err := s.Value[intT](); err != nil || tail != 9 {
		t.Errorf("Value after Array = %v, %v; want 9", tail, err)
	}

	// A malformed array yields exactly one error after the good elements.
	var vals, errs int
	for _, err := range NewStream(strings.NewReader("[1 2]"), nil).Array[intT]() {
		if err != nil {
			errs++
		} else {
			vals++
		}
	}
	if vals != 1 || errs != 1 {
		t.Errorf("malformed array: vals=%d errs=%d, want 1/1", vals, errs)
	}

	// Not an array at all.
	var sawErr bool
	for _, err := range NewStream(strings.NewReader("42"), nil).Array[intT]() {
		sawErr = err != nil
	}
	if !sawErr {
		t.Error("Array over a non-array must yield an error")
	}

	// A long array through a small buffer. Nothing builds up and the buffer
	// stays small.
	const count = 5000
	var sb strings.Builder
	sb.WriteByte('[')
	for i := range count {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.Itoa(i % 97))
	}
	sb.WriteByte(']')
	var lr strings.Reader
	lr.Reset(sb.String())
	var ls Stream
	ls.Reset(&lr, make([]byte, 0, 64))
	seen := 0
	for _, err := range ls.Array[intT]() {
		if err != nil {
			t.Fatalf("long array error at %d: %v", seen, err)
		}
		seen++
	}
	if seen != count {
		t.Errorf("long array yielded %d, want %d", seen, count)
	}
	if c := cap(ls.Bytes()); c > 256 {
		t.Errorf("buffer grew to cap %d over %d elements (started at 64)", c, count)
	}
}

// Array must not allocate per element once the reused value is warm.
func TestStreamArrayNoAlloc(t *testing.T) { //nolint:paralleltest // measures allocations
	payload := "[[1,2,3],[4,5,6],[7,8,9]]"
	var r strings.Reader
	var s Stream
	buf := make([]byte, 0, 256)
	warm := sliceT{Vals: make([]intT, 0, 8)}
	drain := func() {
		r.Reset(payload)
		for _, err := range s.Reset(&r, buf).Array(warm) {
			if err != nil {
				t.Fatalf("Array: %v", err)
			}
		}
	}
	drain()
	if n := testing.AllocsPerRun(50, drain); n > 0 {
		t.Errorf("rcv.Array() allocates %.0f per drain; want 0", n)
	}
}

// sizedChunkReader returns at most n bytes per Read, then io.EOF on every
// later call.
type sizedChunkReader struct {
	data []byte
	n    int
}

func (r *sizedChunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(r.n, len(r.data), len(p))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

// TestBytesStreamTruncationErrorParity checks errors for input that ends too
// early. For every such input, SkipValue, Any and AnyNumber must return the
// same error on Stream as on []byte input. This must hold at every chunk
// size, because each refill is a place where the two could differ.
func TestBytesStreamTruncationErrorParity(t *testing.T) {
	t.Parallel()
	inputs := []string{
		"", "nul", "n", "nu", "tru", "t", "fals", "f",
		"[", "[1,", "[1", "[[", "[ ",
		"{", `{"a"`, `{"a":`, `{"a":1`, `{"a":1,`,
		`"ab`, `"a\`, "-", "1.", "12.", "1e",
		// Broken values at the very end of the input, next to some that are
		// only cut short.
		"\"ab\x01", "\"abc\x01", "{\"a\x01:1}", `"\u12"`, `"ab\uZ"`, `"\u00`, `"\ud83d"`, `"\ud83d\uDE`,
		"trux", "falsy",
	}
	chunks := []int{1, 3, 64}
	for _, in := range inputs {
		_, wantSkip := SkipValue([]byte(in), 0)
		_, _, wantAny := Any([]byte(in), 0, true)
		_, _, wantAnyNum := AnyNumber([]byte(in), 0, true)
		for _, cs := range chunks {
			var s Stream
			s.Reset(&sizedChunkReader{data: []byte(in), n: cs}, nil)
			if got := s.SkipValue(); got != wantSkip {
				t.Errorf("SkipValue(%q) chunk=%d: stream %v, bytes %v", in, cs, got, wantSkip)
			}
			s.Reset(&sizedChunkReader{data: []byte(in), n: cs}, nil)
			if _, got := s.Any(true); got != wantAny {
				t.Errorf("Any(%q) chunk=%d: stream %v, bytes %v", in, cs, got, wantAny)
			}
			s.Reset(&sizedChunkReader{data: []byte(in), n: cs}, nil)
			if _, got := s.AnyNumber(true); got != wantAnyNum {
				t.Errorf("AnyNumber(%q) chunk=%d: stream %v, bytes %v", in, cs, got, wantAnyNum)
			}
		}
	}
	// Value functions called when the input has already ended. Each must
	// return the same JSON error as its []byte version, not the raw ReadMore
	// error. "-" also covers the sign handling in Int64.
	heads := []struct {
		name   string
		in     string
		bytes  func([]byte) error
		stream func(*Stream) error
	}{
		{"Int64", "", func(d []byte) error { _, _, e := Int64(d, 0); return e }, func(s *Stream) error { _, e := s.Int64(); return e }},
		{"Int64-sign", "-", func(d []byte) error { _, _, e := Int64(d, 0); return e }, func(s *Stream) error { _, e := s.Int64(); return e }},
		{"Uint64", "", func(d []byte) error { _, _, e := Uint64(d, 0); return e }, func(s *Stream) error { _, e := s.Uint64(); return e }},
		{"Float64", "", func(d []byte) error { _, _, e := Float64(d, 0); return e }, func(s *Stream) error { _, e := s.Float64(); return e }},
		{"Number", "", func(d []byte) error { _, _, e := Number(d, 0); return e }, func(s *Stream) error { _, e := s.Number(); return e }},
		{"Bool", "", func(d []byte) error { _, _, e := Bool(d, 0); return e }, func(s *Stream) error { _, e := s.Bool(); return e }},
		{"String", "", func(d []byte) error { _, _, e := String(d, 0, true); return e }, func(s *Stream) error { _, e := s.String(true); return e }},
	}
	for _, h := range heads {
		want := h.bytes([]byte(h.in))
		for _, cs := range chunks {
			var s Stream
			s.Reset(&sizedChunkReader{data: []byte(h.in), n: cs}, nil)
			if got := h.stream(&s); got != want {
				t.Errorf("%s(%q) chunk=%d: stream %v, bytes %v", h.name, h.in, cs, got, want)
			}
		}
	}
	// ConsumeColon has no []byte version. Generated []byte code reports
	// ErrBadObject for a missing ':', so ConsumeColon at the end of input
	// must too.
	var s Stream
	s.Reset(&sizedChunkReader{data: nil, n: 1}, nil)
	if got := s.ConsumeColon(); got != ErrBadObject {
		t.Errorf("ConsumeColon(drained): got %v, want %v", got, ErrBadObject)
	}
}

// TestStreamNumberLosslessRetry checks that a number can be read again after
// a reader error. A refill in the middle of a number keeps the bytes from the
// start of the number. So after a non-EOF reader error, Pos is back at the
// start of the number with all its bytes still buffered, and a retry reads
// it in full, as documented. If the refill dropped the bytes already read,
// Int64 would lose its '-' and flip the sign, and the digit loops would lose
// digits. Float64 and Number would also leave Pos unadjusted, so Offset()
// would be too large.
func TestStreamNumberLosslessRetry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		seq   []any
		cap   int
		pos   int
		retry func(*Stream) (string, error)
		want  string
		off   int
	}{
		{
			"Int64 sign",
			[]any{"[1,-", errFlaky, "123]"},
			4, 3,
			func(s *Stream) (string, error) {
				v, err := s.Int64()
				return strconv.FormatInt(v, 10), err
			}, "-123", 3,
		},
		{
			"Uint64 digits",
			[]any{"[9,12", errFlaky, "345]"},
			5, 3,
			func(s *Stream) (string, error) {
				v, err := s.Uint64()
				return strconv.FormatUint(v, 10), err
			}, "12345", 3,
		},
		{
			"Float64 digits",
			[]any{"[0,22", errFlaky, "23]"},
			5, 3,
			func(s *Stream) (string, error) {
				v, err := s.Float64()
				return strconv.FormatFloat(v, 'f', -1, 64), err
			}, "2223", 3,
		},
		{
			"Number digits",
			[]any{"[0,22", errFlaky, "23]"},
			5, 3,
			func(s *Stream) (string, error) {
				v, err := s.Number()
				return string(v), err
			}, "2223", 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var s Stream
			s.Reset(&flakyReader{seq: tc.seq}, make([]byte, 0, tc.cap))
			if err := s.ReadMore(0); err != nil {
				t.Fatal(err)
			}
			s.Pos = tc.pos
			if _, err := tc.retry(&s); !errors.Is(err, errFlaky) {
				t.Fatalf("got %v, want the transient reader error", err)
			}
			if s.Pos > len(s.Bytes()) {
				t.Errorf("Pos %d past len(buf) %d", s.Pos, len(s.Bytes()))
			}
			if got := s.Offset(); got != tc.off {
				t.Errorf("Offset = %d, want %d (the value head)", got, tc.off)
			}
			got, err := tc.retry(&s)
			if err != nil {
				t.Fatalf("retry: %v", err)
			}
			if got != tc.want {
				t.Errorf("retry = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestStreamString_ErrorPos checks the error position of the string functions
// after a refill that drops consumed bytes. Pos is relative to the buffer, so
// every error return must adjust it for the dropped bytes. Generated Stream
// decoders put the position straight into ParseError.Pos. A missed
// adjustment makes it too large, even past the end of the input. Every
// function reports the control byte itself, like the []byte versions.
// skipString drops buffered data as it goes, so the bad byte is the only
// position all of them can report.
func TestStreamString_ErrorPos(t *testing.T) {
	t.Parallel()
	// The prefix string is read first, so the function under test starts at
	// a non-zero Pos. A missed adjustment would add that offset again.
	const prefix = `"pre"`
	in := prefix + `"` + strings.Repeat("a", 100) + "\x01\""
	for _, tc := range []struct {
		name string
		run  func(*Stream) error
	}{
		{"String", func(s *Stream) error { _, err := s.String(true); return err }},
		{"StringView", func(s *Stream) error { _, err := s.StringView(true); return err }},
		{"KeyView", func(s *Stream) error { _, err := s.KeyView(true); return err }},
		{"skipString", (*Stream).skipString},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, chunk := range []int{1, 7, 64} {
				var s Stream
				s.Reset(strings.NewReader(in), make([]byte, 0, chunk))
				if err := s.skipString(); err != nil {
					t.Fatalf("chunk=%d: prefix: %v", chunk, err)
				}
				if err := tc.run(&s); err != ErrBadString {
					t.Fatalf("chunk=%d: err %v, want ErrBadString", chunk, err)
				}
				if s.Pos > len(s.Bytes()) {
					t.Errorf("chunk=%d: Pos %d past len(buf) %d", chunk, s.Pos, len(s.Bytes()))
				}
				if got := s.Offset(); got != len(in)-2 {
					t.Errorf("chunk=%d: Offset %d, want %d (the control byte)", chunk, got, len(in)-2)
				}
			}
		})
	}
}

// TestStreamStringSlow_ErrorPos is the same check for strings with escapes.
// stringSlow copies into its own buffer and drops consumed bytes from the
// Stream's buffer on every refill.
func TestStreamStringSlow_ErrorPos(t *testing.T) {
	t.Parallel()
	in := `"\n` + strings.Repeat("a", 100) + "\x01\""
	for _, chunk := range []int{1, 7, 64} {
		var s Stream
		s.Reset(strings.NewReader(in), make([]byte, 0, chunk))
		if _, err := s.String(true); err != ErrBadString {
			t.Fatalf("chunk=%d: err %v, want ErrBadString", chunk, err)
		}
		if s.Pos > len(s.Bytes()) {
			t.Errorf("chunk=%d: Pos %d past len(buf) %d", chunk, s.Pos, len(s.Bytes()))
		}
		if got := s.Offset(); got != len(in)-2 {
			t.Errorf("chunk=%d: Offset %d, want %d (the control byte)", chunk, got, len(in)-2)
		}
	}
}

// TestStreamSkipNumber_StopsAfterReaderError checks that skipping a number
// stops at a reader error. refillSkip records the error and returns false.
// The checks for a fraction, exponent or sign must not read false as only
// "no more bytes" and call refillSkip again. That would start new Reads
// before anyone looks at the error. On a reader that sends nothing after
// the error, SkipValue would block in Read, and no deadline could stop it.
func TestStreamSkipNumber_StopsAfterReaderError(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	defer close(block)
	done := make(chan error, 1)
	go func() {
		var s Stream
		s.Reset(&errThenBlockReader{data: "123", err: errFlaky, block: block}, make([]byte, 0, 3))
		done <- s.SkipValue()
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errFlaky) {
			t.Errorf("got %v, want the reader error", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("SkipValue kept reading after a recorded reader error")
	}
}

// errThenBlockReader returns its data, then fails once, then blocks. Its
// error must end the scan instead of causing another Read.
type errThenBlockReader struct {
	data  string
	err   error
	stage int
	block chan struct{}
}

func (r *errThenBlockReader) Read(p []byte) (int, error) {
	switch r.stage {
	case 0:
		r.stage++
		return copy(p, r.data), nil
	case 1:
		r.stage++
		return 0, r.err
	}
	<-r.block
	return 0, io.EOF
}

// TestStreamCaptureValue_MaxDepthDoesNotHang checks a run of '[' deeper than
// maxDepth on a live reader. skipArray and skipObject return ErrMaxDepth with
// the position on the last bracket read. If that bracket is the last
// buffered byte, CaptureValue must not judge by position alone and treat it
// as cut short. That would block in Read, so the depth limit meant to stop
// abuse would hang the goroutine. No later byte can undo too much depth.
func TestStreamCaptureValue_MaxDepthDoesNotHang(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	defer close(block)
	done := make(chan error, 1)
	go func() {
		var s Stream
		s.Reset(&liveReader{data: bytes.Repeat([]byte("["), maxDepth+1), block: block}, make([]byte, 0, 64))
		_, err := s.CaptureValue()
		done <- err
	}()
	select {
	case err := <-done:
		if err != ErrMaxDepth {
			t.Errorf("got %v, want ErrMaxDepth", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("CaptureValue hung on a depth-capped value")
	}
}

// A live reader, one that never sends EOF, has sent a complete string with a
// broken escape or a control byte. String, SkipValue and CaptureValue must
// return an error, not block in Read. A `\u` escape must be judged on the
// digits already buffered, without waiting for all 6 bytes. A high surrogate
// followed by a buffered `"` cannot get its low half, so there is nothing to
// wait for. A control byte in an unclosed string is already an error. Each
// call must return what the []byte version returns. An escape that is only
// cut short is the control case and must still wait.
func TestStreamString_LiveMalformedTailDoesNotHang(t *testing.T) {
	t.Parallel()
	ops := []struct {
		name   string
		bytes  func([]byte) error
		stream func(*Stream) error
	}{
		{
			"String",
			func(d []byte) error { _, _, err := String(d, 0, true); return err },
			func(s *Stream) error { _, err := s.String(true); return err },
		},
		{
			"SkipValue",
			func(d []byte) error { _, err := SkipValue(d, 0); return err },
			func(s *Stream) error { return s.SkipValue() },
		},
		{
			"CaptureValue",
			func(d []byte) error { _, err := SkipValue(d, 0); return err },
			func(s *Stream) error { _, err := s.CaptureValue(); return err },
		},
	}
	payloads := []string{`"\u12"`, `"ab\uZ"`, `"\u"`, `"\ud83d"`, `"\ud83d\n"`, "\"ab\x01}"}
	for _, payload := range payloads {
		for _, op := range ops {
			want := op.bytes([]byte(payload))
			block := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				var s Stream
				s.Reset(&liveReader{data: []byte(payload), block: block}, make([]byte, 0, 64))
				done <- op.stream(&s)
			}()
			select {
			case err := <-done:
				if err != want {
					t.Errorf("%s(%q) = %v, bytes path says %v", op.name, payload, err, want)
				}
			case <-time.After(2 * time.Second):
				t.Errorf("%s(%q) hung on a live reader", op.name, payload)
			}
			close(block)
		}
	}
	for _, op := range ops {
		block := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			var s Stream
			s.Reset(&liveReader{data: []byte(`"\u00`), block: block}, make([]byte, 0, 64))
			done <- op.stream(&s)
		}()
		select {
		case err := <-done:
			t.Errorf("%s(truncated escape) returned %v; must keep waiting for bytes", op.name, err)
		case <-time.After(100 * time.Millisecond):
		}
		close(block)
	}
}

// When Stream.Bool fails, it leaves Pos where the scan gave up. That is the
// first byte that does not match, or the end of the data when the reader
// ended inside the literal. So a generated ParseError.Pos matches BoolEnd on
// []byte input at every chunk size. After a non-EOF reader error, Pos stays
// at the start of the literal, so a retry reads the whole literal again.
func TestStreamBool_ErrorPosMatchesBytes(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"tru", "trux", "fals", "falsy", "t", "f", "x", "", "true", "false"} {
		data := []byte(in)
		_, wantPos, wantErr := Bool(data, 0)
		if wantErr != nil {
			wantPos = BoolEnd(data, 0)
		}
		for _, cs := range []int{1, 3, 64} {
			var s Stream
			s.Reset(&sizedChunkReader{data: data, n: cs}, nil)
			_, err := s.Bool()
			if err != wantErr || s.Offset() != wantPos {
				t.Errorf("Bool(%q) chunk=%d: stream (%v, off %d), bytes (%v, pos %d)",
					in, cs, err, s.Offset(), wantErr, wantPos)
			}
		}
	}
	var s Stream
	s.Reset(&hiccupReader{data: []byte("true"), hiccupAt: 3}, make([]byte, 0, 1))
	if _, err := s.Bool(); !errors.Is(err, errTransientHiccup) {
		t.Fatalf("got %v, want the transient reader error", err)
	}
	if s.Offset() != 0 {
		t.Errorf("Offset after a transient error = %d, want 0 (the literal head)", s.Offset())
	}
	if v, err := s.Bool(); err != nil || !v {
		t.Errorf("retry = (%v, %v), want (true, nil)", v, err)
	}
}

// TestStreamStringSurrogate_LiveReader checks a lone high surrogate escape on
// a live reader that has sent the whole string. When looking for the low
// half, String must first check whether a backslash follows at all. It must
// not wait for six more bytes, which for a lone escape lie past the closing
// quote. Otherwise it blocks in Read forever. The []byte version answers at
// once: ErrInvalidUTF8, or U+FFFD under allowinvalidutf8. The last row is the
// control case: a complete 4-byte character must still decode.
func TestStreamStringSurrogate_LiveReader(t *testing.T) {
	t.Parallel()
	type res struct {
		v   string
		err error
	}
	for _, tc := range []struct {
		doc      string
		validate bool
	}{
		{`"\ud83d"}`, true},
		{`"\ud83d"}`, false},
		{`"\ud83dab"`, false},
		{`"😀"`, true},
	} {
		block := make(chan struct{})
		done := make(chan res, 1)
		go func() {
			var s Stream
			s.Reset(&liveReader{data: []byte(tc.doc), block: block}, make([]byte, 0, 64))
			v, err := s.String(tc.validate)
			done <- res{v, err}
		}()
		wantV, _, wantErr := String([]byte(tc.doc), 0, tc.validate)
		select {
		case r := <-done:
			if r.err != wantErr || r.v != wantV {
				t.Errorf("%q validate=%v: stream (%q, %v), bytes (%q, %v)", tc.doc, tc.validate, r.v, r.err, wantV, wantErr)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("%q validate=%v: String hung on a live reader after the whole value arrived", tc.doc, tc.validate)
		}
		close(block)
	}
}

// TestStreamErrorPos_MatchesBytes checks that Stream reports the same error
// positions as []byte input. Whatever a function returns, Offset() must equal
// the position its []byte version returns for the same input, at every
// chunk size. It must never point past the buffer or the input. A prefix is
// read first, so the buffer has dropped bytes by the time the function under
// test runs. An error return that does not adjust Pos for the dropped bytes
// makes Offset() too large. chunk == len(prefix) is the hardest case. The
// function starts at Pos == len(buf) > 0, so its first refill drops the
// whole buffer before the Read fails. The test covers the number functions,
// Bool, String, skipping (numbers, literals, brackets, cut-off escapes),
// CaptureValue, Any and AnyNumber.
func TestStreamErrorPos_MatchesBytes(t *testing.T) {
	t.Parallel()
	const prefix = `"pre"`
	numbers := []string{
		"", "-", "x", "-x", "01", "-01", "1.", "-1.", "1e", "1e+", "1.e5", "1ee",
		"12", "12.5", "1.5.5", "99999999999999999999", "-99999999999999999999",
		"18446744073709551616", "1.5e", "0", "-0", "0x",
	}
	// A control byte inside a skipped string reports the byte's own index
	// on both paths. On Stream the start of the string is gone by then,
	// because skipString drops buffered data as it goes. So the bad byte is
	// the position both can report. The long rows put it past a chunk
	// edge.
	skips := []string{
		"", " ", "-", "1.", "1e", "1e+", "1.e5", "1ee", "01", "   1.", "x", "12",
		"n", "nul", "nulx", "null", "t", "tru", "trux", "true", "f", "fals", "false",
		"[", "[1", "[1,", "[1,]", "[1 2]", "[]", "[[[", "[1,[2,]]",
		"{", `{"a"`, `{"a":`, `{"a":1`, `{"a":1,`, `{"a":1 "b":2}`, `{1:2}`, `{"a"}`, `{}`,
		`"ab`, `"a\`, `"a\u12`, `"a\uzz"`, `"a\x"`, `"ab"`,
		"\"abc\x01", "\"abc\x01\"", "\"ab\x01c\"", "\"a\\n\x01\"",
		"\"abcdefghijklmnop\x01\"", "\"abcdefghij\\n" + strings.Repeat("k", 20) + "\x01\"",
	}
	// CaptureValue runs the []byte SkipValue over the buffered data, so its
	// positions are the []byte positions, even inside a string.
	captures := skips
	anys := append(append([]string{}, skips...), "\"\xff\"", `["a\`, `{"k":"v\u1"}`)
	prims := []struct {
		name   string
		inputs []string
		bytes  func([]byte, int) (int, error)
		stream func(*Stream) error
	}{
		{"Int64", numbers, func(d []byte, i int) (int, error) { _, p, e := Int64(d, i); return p, e }, func(s *Stream) error { _, e := s.Int64(); return e }},
		{
			"Uint64",
			numbers,
			func(d []byte, i int) (int, error) { _, p, e := Uint64(d, i); return p, e },
			func(s *Stream) error { _, e := s.Uint64(); return e },
		},
		{
			"Float64",
			numbers,
			func(d []byte, i int) (int, error) { _, p, e := Float64(d, i); return p, e },
			func(s *Stream) error { _, e := s.Float64(); return e },
		},
		{
			"Number",
			numbers,
			func(d []byte, i int) (int, error) { _, p, e := Number(d, i); return p, e },
			func(s *Stream) error { _, e := s.Number(); return e },
		},
		// Bool on []byte reports the literal start. Generated code reports
		// BoolEnd's position instead, so the test does the same.
		{
			"Bool",
			[]string{"", "t", "tru", "trux", "true", "f", "fals", "false", "x", "null", "1"},
			func(d []byte, i int) (int, error) {
				_, p, e := Bool(d, i)
				if e != nil {
					p = BoolEnd(d, i)
				}
				return p, e
			}, func(s *Stream) error { _, e := s.Bool(); return e },
		},
		{
			"String",
			[]string{
				"",
				"x",
				`"ab"`,
				`"ab`,
				`"a\`,
				`"a\u12`,
				`"a\uzzzz"`,
				`"a\x"`,
				"\"ab\x01c\"",
				"\"abc\x01",
				`"\ud83d"`,
				`"a\ud83d`,
				"\"a\\n\x01\"",
				"\"\xff\"",
				`"a\n"`,
				"\"a\\n\xff\"",
				// Input that ends inside a `\uXXXX` is cut short, not a lone
				// surrogate. Both paths report the end of the input.
				`"\ud83d\ude0`,
				`"\ud83d\`,
				`"\ud83d\u`,
				`"\ud83dx"`,
			},
			func(d []byte, i int) (int, error) { _, p, e := String(d, i, true); return p, e },
			func(s *Stream) error { _, e := s.String(true); return e },
		},
		{"SkipValue", skips, SkipValue, (*Stream).SkipValue},
		{"CaptureValue", captures, SkipValue, func(s *Stream) error { _, e := s.CaptureValue(); return e }},
		{"Any", anys, func(d []byte, i int) (int, error) { _, p, e := Any(d, i, true); return p, e }, func(s *Stream) error { _, e := s.Any(true); return e }},
		{
			"AnyNumber",
			anys,
			func(d []byte, i int) (int, error) { _, p, e := AnyNumber(d, i, true); return p, e },
			func(s *Stream) error { _, e := s.AnyNumber(true); return e },
		},
	}
	for _, p := range prims {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			for _, in := range p.inputs {
				doc := prefix + in
				wantPos, wantErr := p.bytes([]byte(doc), len(prefix))
				for _, chunk := range []int{1, len(prefix), 7, 64} {
					var s Stream
					s.Reset(&sizedChunkReader{data: []byte(doc), n: chunk}, make([]byte, 0, chunk))
					if err := s.skipString(); err != nil {
						t.Fatalf("%q chunk=%d: prefix: %v", in, chunk, err)
					}
					gotErr := p.stream(&s)
					if gotErr != wantErr {
						t.Errorf("%q chunk=%d: err %v, bytes %v", in, chunk, gotErr, wantErr)
						continue
					}
					if s.Pos > len(s.Bytes()) {
						t.Errorf("%q chunk=%d: Pos %d past len(buf) %d", in, chunk, s.Pos, len(s.Bytes()))
					}
					if got := s.Offset(); got != wantPos || got > len(doc) {
						t.Errorf("%q chunk=%d (%v): Offset %d, bytes pos %d (doc len %d)", in, chunk, gotErr, got, wantPos, len(doc))
					}
				}
			}
		})
	}
}
