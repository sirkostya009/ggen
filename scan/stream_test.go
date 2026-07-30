package scan

import (
	"bytes"
	"encoding/json"
	"errors"
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
