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

// TestStream_EOFAfterContent: reader returns content then io.EOF on the
// same call (io.Reader interface allows this). Stream must consume the
// content and surface EOF on the NEXT call only. ReadMore is stateless —
// the deferred EOF comes from re-Reading the drained reader (io.Reader EOF
// is stable), not a Stream flag — so both reader shapes are pinned:
// separate-call EOF (bytes.Reader) and data+EOF-same-call (DataErrReader).
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

// TestStreamCaptureValue pins CaptureValue: byte-exact spans across chunked
// refills, correct stop at the value's real end when input continues, errors
// surfacing only at EOF, and the first-refill prefix compaction (a capture
// starting mid-window must reuse the consumed prefix as tail capacity instead
// of dragging it through every grow).
func TestStreamCaptureValue(t *testing.T) {
	t.Parallel()
	values := []string{
		`123`, `-45.67e8`, `"plain"`, `"esc\n\tA😀"`, `true`, `null`,
		`[1,2,3,[4,5,[6,7]],8]`, `{"a":1,"b":[2,3],"c":{"d":"e"}}`,
		`"` + strings.Repeat("a", 5000) + `"`,
	}
	for _, v := range values {
		// 1-byte reads: value split at every offset.
		var s Stream
		s.Reset(&chunkedReader{data: []byte(v)}, make([]byte, 0, 8))
		got, err := s.CaptureValue()
		if err != nil {
			t.Fatalf("%.20q chunked: %v", v, err)
		}
		if string(got) != v {
			t.Fatalf("%.20q chunked: got %.20q", v, got)
		}
		// Trailing input: must stop at the value's real end.
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
		// Two ~700 B values through a 1024 B window: capturing the second
		// starts at Pos≈702 with the window full. The first refill must
		// compact the consumed prefix — the second value then fits in the
		// freed space and the buffer NEVER grows. Grow-only (the old shape)
		// would double to 2048.
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
		// A keep-alive connection delivers the whole value and then has
		// nothing in flight — the capture must complete without another
		// Read (which would block forever; liveChunkReader errors instead).
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
		// A bare number at the window edge is genuinely not final — "123"
		// may continue "1234", so the lookahead Read is required and a live
		// reader must deliver a delimiter or EOF. Pinned as inherent.
		var s Stream
		s.Reset(&liveChunkReader{chunks: []string{`123`}}, nil)
		if _, err := s.CaptureValue(); !errors.Is(err, errBlockingRead) {
			t.Fatalf("number: got %v, want the blocking-read probe error", err)
		}
	})
}

// liveReaderCases: each entry is one value delivered across that many Reads.
// Shared with the SIMD tier twin (simd_skip_stream_test.go).
var liveReaderCases = [][]string{
	{`{"a":1}`},
	{`{"a`, `":1}`},
	{`"abc`, `def"`},
	{`[1,`, `2,3]`},
	{`tr`, `ue`},
	{`nu`, `ll`},
}

// TestStreamInt_LeadingZeroPeekTransientError pins the leading-zero peek's
// error contract: a transient (non-EOF) reader error during the "01" peek
// used to be swallowed with an un-rebased cursor, so a later refill resumed
// the digit loop and "01" decoded as 1 with a nil error.
func TestStreamInt_LeadingZeroPeekTransientError(t *testing.T) {
	t.Parallel()
	errTransient := errors.New("transient")
	// "0" delivered, one transient error, then "1" — malformed "01" overall.
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
	// Bare "0" at EOF stays a valid number (the swallowed-EOF arm).
	s.Reset(strings.NewReader("0"), nil)
	if v, err := s.Int64(); err != nil || v != 0 {
		t.Errorf("bare 0: got %d, %v", v, err)
	}
	s.Reset(strings.NewReader("0"), nil)
	if v, err := s.Uint64(); err != nil || v != 0 {
		t.Errorf("bare 0 uint: got %d, %v", v, err)
	}
}

// TestStreamTransientErrorNeverSilentNorMislabeled sweeps every value
// primitive against a reader that hiccups (0, err) at each byte position.
// Two failure modes are pinned: the number scanners' refill loops used to
// `break` on ANY ReadMore error and return the digits scanned so far with a
// NIL error (silent truncation — `12345` → 1234 at top level), and the
// string/bool/literal/skip refill arms relabeled the hiccup as a grammar
// sentinel (ErrUnterminated / ErrBadBool / ErrBadObject), destroying error
// identity. Both contradict ReadMore's documented contract.
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
		{"Number", "777", func(s *Stream) (any, error) { return s.Number() }},
		{"String", `"abcdef"`, func(s *Stream) (any, error) { return s.String(true) }},
		{"StringEscape", `"a\nb\u0041c"`, func(s *Stream) (any, error) { return s.String(true) }},
		{"KeyView", `"key"`, func(s *Stream) (any, error) { return s.KeyView(true) }},
		{"Bool", "true", func(s *Stream) (any, error) { return s.Bool() }},
		{"Any", "123.5", func(s *Stream) (any, error) { return s.Any() }},
		{"AnyObject", `{"a":[1,"x"]}`, func(s *Stream) (any, error) { return s.Any() }},
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
				continue // value completed before the hiccup could fire
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
	// Control: the drained-window path still ends values normally, and real
	// grammar errors still report as grammar errors.
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

// The stream number scanners collect a loose [0-9.eE+-] span across refills
// and then grammar-check it; the GRAMMAR end is authoritative, not the loose
// scan's, so they stop where the bytes path stops (maximal munch). Erroring
// on the whole span instead made `1.5.5` / `1e5e` / `01` reject on the
// stream where the bytes path succeeds — a bytes/stream parity break.
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
		// Number mirrors the same extent.
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

// A live (never-EOF) reader that has delivered a complete but MALFORMED
// value must get an error, not a hang: the capture loop treated every skip
// failure as "read more" and blocked in Read forever waiting for bytes that
// could not repair a malformation sitting before bytes already buffered.
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

// A TRUNCATED value on a live reader must still wait (more bytes can
// complete it) — the control proving the finality check isn't over-eager.
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

// liveReader delivers its payload, then blocks until block is closed (a
// socket with nothing in flight) and finally reports EOF.
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

// hiccupReader delivers one byte per Read except call #hiccupAt, which
// returns (0, errTransient) — the recoverable-reader class (a net.Conn read
// deadline), distinct from a drained window.
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

// flakyReader plays a sequence of string chunks and injected errors.
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

// liveChunkReader delivers each chunk in one Read, then errors on any further
// Read — a stand-in for a live socket whose next Read would block forever
// once the value has been delivered.
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
	// Non-string object keys: the stream loop used to relabel the key
	// skipString's ErrExpectString only when the window was drained, so a
	// buffered non-quote key reported ErrExpectString where bytes reports
	// ErrBadObject.
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

// TestStreamStringSlowBufBounded pins the compacting escape refill: a long
// escaped string must stream through a bounded window, not grow-only. stringSlow
// decodes into its own scratch and aliases THAT (not s.buf), so every ReadMore
// keeps from the cursor + rebases (like Float64/Number) — else each mid-string
// refill lands with len == cap (readers fill the whole window) and DOUBLES the
// buffer (a 64 B buffer ballooned to 256 KB on a 200 KB escaped string before the
// fix). Same class the float/number/skip-tree compaction already fixed. Every
// escape kind — simple, \uXXXX, surrogate pair — routes through stringSlow's
// distinct ensure-loops, so all three must stay bounded. Value checked against
// the (fully buffered, correct) bytes path.
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

// TestStreamStringInvalidUTF8: the stream string paths (String/StringView/
// KeyView + stringSlow) reject invalid UTF-8 with ErrInvalidUTF8 (jsonv2
// parity), across refill boundaries; valid multi-byte runes straddling a
// refill still pass (the utf8.Valid runs over the full contiguous span, never
// per chunk).
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
	// Valid multi-byte runes straddling one-byte refills decode intact.
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

// ReadMore used to surface a simultaneous non-EOF error even when the Read
// delivered bytes — the number scanners' refill swallow then returned a
// TRUNCATED value with the fresh digits sitting unread in the buffer.
func TestStreamReadMore_BytesBeforeError(t *testing.T) {
	t.Parallel()
	var s Stream
	s.Reset(&flakyReader{seq: []any{"1", &bothRead{"23", errFlaky}}}, nil)
	v, err := s.Int64()
	if err != nil || v != 123 {
		t.Errorf("Int64 = %d, %v — want 123 (bytes delivered with the error must be consumed)", v, err)
	}
}

// skipSpaceSlow's error paths wrote the PRE-compaction cursor: Offset()
// double-counted the discarded whitespace run and Pos landed past len(buf).
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

// The surrogate-pair ensure loop swallowed transient reader errors and
// mislabeled them as lone surrogates (spurious ErrInvalidUTF8).
func TestStreamStringSurrogate_TransientError(t *testing.T) {
	t.Parallel()
	var s Stream
	s.Reset(&flakyReader{seq: []any{`"\ud83d`, errFlaky, `\ude00"`}}, nil)
	if _, err := s.String(true); !errors.Is(err, errFlaky) {
		t.Errorf("got %v, want the reader error (not ErrInvalidUTF8)", err)
	}
}

var errFlaky = errors.New("flaky reader")

// bothRead delivers bytes AND an error from one Read call (io.Reader permits
// and documents this pairing).
type bothRead struct {
	data string
	err  error
}

// intT also satisfies StreamDecoder (its bytes-path half is in decode_test.go).
func (intT) DecodeFromStream(s *Stream) (intT, error) {
	v, err := s.Int64()
	return intT(v), err
}

// sliceT is a StreamDecoder whose value carries a container, so buffer reuse
// is observable. It threads its own buffer into the nested Slice call, which
// is how a generated decoder with a slice field recycles it.
type sliceT struct{ Vals []intT }

func (recv sliceT) DecodeFromStream(s *Stream) (sliceT, error) {
	result := recv
	v, err := s.Slice(result.Vals)
	result.Vals = v
	return result, err
}

// TestStreamMethods pins the generic Stream methods: Reset chains into them,
// and both leave the cursor positioned so the caller can keep reading — the
// capability the bytes walkers cannot offer.
func TestStreamMethods(t *testing.T) {
	v, err := NewStream(strings.NewReader("42"), nil).Value[intT]()
	if err != nil || v != 42 {
		t.Fatalf("Value = %v, %v; want 42", v, err)
	}

	// Slice does NOT reject trailing data, so a value after the array is
	// still readable from the same Stream.
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

	// An empty array decodes to a NON-nil empty slice, matching the bytes
	// walker and jsonv2.
	if e, err := NewStream(strings.NewReader(" [ ] "), nil).Slice[intT](); err != nil || e == nil || len(e) != 0 {
		t.Errorf("Slice([]) = %#v, %v; want non-nil empty", e, err)
	}

	// Consecutive values off one Stream.
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

// TestStreamSeq pins the iterator: consecutive top-level values, a clean end
// when the reader drains, early break leaving the Stream usable, and the
// one-value reuse that makes a long run allocation-free.
func TestStreamSeq(t *testing.T) {
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

	// Empty / whitespace-only input ends immediately with no error.
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

	// Seq reuses one value across iterations, so a long run settles at zero
	// allocations; an rcv seeds that value with containers already to hand.
	var r strings.Reader
	var st Stream
	sbuf := make([]byte, 0, 256)
	drain := func(seed ...sliceT) (n int, last *intT) {
		r.Reset("[1,2,3] [4,5,6] [7,8,9]")
		for v := range st.Reset(&r, sbuf).Seq[sliceT](seed...) {
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

	// The window stays bounded over a long stream. Seq drops consumed values
	// once the buffer is full, so refills reuse that space instead of
	// doubling. (The generated-decoder case, where the emitted refills are
	// grow-only and this actually bites, is TestSeqBufferStaysBounded in
	// integrationtests.)
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

// TestStreamBufferReuse pins that rcv recycles containers — the outer slice
// AND each element's own slice — so a steady-state re-decode stops allocating.
func TestStreamBufferReuse(t *testing.T) {
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

	// Without rcv the decode is independent (fresh containers every time).
	fresh := decodeInto(nil)
	if &fresh[0] == outerPtr {
		t.Error("no-rcv decode must not reuse the previous buffer")
	}
}

// TestStreamArray pins the lazy array iterator: same grammar as Slice, cursor
// left past the closing bracket, one error then stop, and no accumulation.
func TestStreamArray(t *testing.T) {
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

	// A long array through a small buffer: nothing accumulates and the window
	// stays bounded.
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
func TestStreamArrayNoAlloc(t *testing.T) {
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

// sizedChunkReader delivers at most n bytes per Read, then a stable io.EOF.
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

// TestBytesStreamTruncationErrorParity pins the drained-truncation error
// identity of the stream walkers to the bytes path: for every truncated
// input, SkipValue / Any / AnyNumber must surface the SAME sentinel on both
// paths, at every chunk size (each refill site is a potential divergence).
func TestBytesStreamTruncationErrorParity(t *testing.T) {
	t.Parallel()
	inputs := []string{
		"", "nul", "n", "nu", "tru", "t", "fals", "f",
		"[", "[1,", "[1", "[[", "[ ",
		"{", `{"a"`, `{"a":`, `{"a":1`, `{"a":1,`,
		`"ab`, `"a\`, "-", "1.", "12.", "1e",
	}
	chunks := []int{1, 3, 64}
	for _, in := range inputs {
		_, wantSkip := SkipValue([]byte(in), 0)
		_, _, wantAny := Any([]byte(in), 0)
		_, _, wantAnyNum := AnyNumber([]byte(in), 0)
		for _, cs := range chunks {
			var s Stream
			s.Reset(&sizedChunkReader{data: []byte(in), n: cs}, nil)
			if got := s.SkipValue(); got != wantSkip {
				t.Errorf("SkipValue(%q) chunk=%d: stream %v, bytes %v", in, cs, got, wantSkip)
			}
			s.Reset(&sizedChunkReader{data: []byte(in), n: cs}, nil)
			if _, got := s.Any(); got != wantAny {
				t.Errorf("Any(%q) chunk=%d: stream %v, bytes %v", in, cs, got, wantAny)
			}
			s.Reset(&sizedChunkReader{data: []byte(in), n: cs}, nil)
			if _, got := s.AnyNumber(); got != wantAnyNum {
				t.Errorf("AnyNumber(%q) chunk=%d: stream %v, bytes %v", in, cs, got, wantAnyNum)
			}
		}
	}
	// Value primitives called with the window already drained: every head
	// used to return the raw ReadMore error where the bytes twin reports a
	// grammar sentinel ("-" additionally covers Int64's sign arm).
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
	// ConsumeColon has no bytes twin — generated bytes code reports
	// ErrBadObject for a missing ':', so a drained head must too.
	var s Stream
	s.Reset(&sizedChunkReader{data: nil, n: 1}, nil)
	if got := s.ConsumeColon(); got != ErrBadObject {
		t.Errorf("ConsumeColon(drained): got %v, want %v", got, ErrBadObject)
	}
}

// TestStreamNumberLosslessRetry pins the number scanners' post-compaction
// cursor. Every mid-number refill keeps from the VALUE HEAD, so a transient
// reader error can return with Pos back on the intact span and the documented
// lossless retry re-scans it. Refilling from the cursor instead threw the span
// away: Int64 lost its '-' (silently flipping the sign) and the digit loops
// lost the digits already folded into the accumulator, while Float64/Number
// left a pre-compaction Pos that inflated Offset() by the discarded prefix.
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
		{"Int64 sign", []any{"[1,-", errFlaky, "123]"}, 4, 3,
			func(s *Stream) (string, error) {
				v, err := s.Int64()
				return strconv.FormatInt(v, 10), err
			}, "-123", 3},
		{"Uint64 digits", []any{"[9,12", errFlaky, "345]"}, 5, 3,
			func(s *Stream) (string, error) {
				v, err := s.Uint64()
				return strconv.FormatUint(v, 10), err
			}, "12345", 3},
		{"Float64 digits", []any{"[0,22", errFlaky, "23]"}, 5, 3,
			func(s *Stream) (string, error) {
				v, err := s.Float64()
				return strconv.FormatFloat(v, 'f', -1, 64), err
			}, "2223", 3},
		{"Number digits", []any{"[0,22", errFlaky, "23]"}, 5, 3,
			func(s *Stream) (string, error) {
				v, err := s.Number()
				return string(v), err
			}, "2223", 3},
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

// TestStreamString_ErrorPos pins the string scanners' error POSITION across a
// compacting refill: Pos is buffer-relative, so an error return that skips the
// rebase leaves the pre-compaction cursor — generated stream decoders stamp it
// straight into ParseError.Pos, where it reads as inflated by the discarded
// prefix and can even exceed the document length.
func TestStreamString_ErrorPos(t *testing.T) {
	t.Parallel()
	// A leading string is consumed first so the scanner under test starts at a
	// non-zero Pos — the offset the stale cursor inflates.
	const prefix = `"pre"`
	in := prefix + `"` + strings.Repeat("a", 100) + "\x01\""
	for _, tc := range []struct {
		name string
		run  func(*Stream) error
		// spanStart: the scanner reports the span start like bytes String
		// (skipString discards the span head, so it reports its cursor).
		spanStart bool
	}{
		{"String", func(s *Stream) error { _, err := s.String(true); return err }, true},
		{"StringView", func(s *Stream) error { _, err := s.StringView(true); return err }, true},
		{"KeyView", func(s *Stream) error { _, err := s.KeyView(true); return err }, true},
		{"skipString", (*Stream).skipString, false},
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
				if got := s.Offset(); got > len(in) {
					t.Errorf("chunk=%d: Offset %d past document length %d", chunk, got, len(in))
				}
				if got := s.Offset(); tc.spanStart && got != len(prefix)+1 {
					t.Errorf("chunk=%d: Offset %d, want %d (the span start)", chunk, got, len(prefix)+1)
				}
			}
		})
	}
}

// TestStreamStringSlow_ErrorPos is the escape-path sibling: stringSlow copies
// into an owned scratch and compacts s.buf from its cursor on every refill.
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

// TestStreamSkipNumber_StopsAfterReaderError: refillSkip records a real reader
// error and returns false, but the fraction/exponent/sign gates read false as
// merely "no more bytes" — each called refillSkip AGAIN, issuing fresh blocking
// Reads before the error was ever consulted. On a reader that stops producing
// after the hiccup, SkipValue wedged in Read and no deadline could cancel it.
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

// errThenBlockReader delivers its payload, then fails once, then blocks — a
// reader whose error must end the scan instead of triggering another Read.
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

// TestStreamCaptureValue_MaxDepthDoesNotHang: the finality test was purely
// positional (end < len(buf)), but skipArray/skipObject return ErrMaxDepth with
// the cursor on the last consumed bracket — a bracket run ending at the window
// edge looked like truncation, so the depth cap's own DoS hardening wedged the
// goroutine in Read. No arriving byte can un-exceed the cap.
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
