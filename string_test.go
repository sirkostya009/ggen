package ggen

import (
	"bytes"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"
)

// stringHappyCases: every input is a complete JSON string literal that
// stdlib accepts. String must produce the same Go string.
var stringHappyCases = []struct {
	name string
	in   string
}{
	{"empty", `""`},
	{"plain", `"hello"`},
	{"escape_quote", `"a\"b"`},
	{"escape_backslash", `"a\\b"`},
	{"escape_slash", `"a\/b"`},
	{"escape_b", `"a\bb"`},
	{"escape_f", `"a\fb"`},
	{"escape_n", `"a\nb"`},
	{"escape_r", `"a\rb"`},
	{"escape_t", `"a\tb"`},
	{"unicode_bmp", `"é"`},            // é
	{"unicode_surrogate_pair", `"😀"`}, // 😀
	{"unicode_then_text", `"xéy"`},
	{"utf8_passthrough", `"héllo"`},
	{"all_ascii_escapes", `"\b\f\n\r\t\"\\\/"`},
}

func TestString_StdlibParity(t *testing.T) {
	t.Parallel()
	for _, tc := range stringHappyCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var want string
			if err := json.Unmarshal([]byte(tc.in), &want); err != nil {
				t.Fatalf("stdlib: %v", err)
			}
			got, j, err := String([]byte(tc.in), 0, true)
			if err != nil {
				t.Fatalf("String: %v", err)
			}
			if got != want {
				t.Errorf("mismatch\n got: %q\nwant: %q", got, want)
			}
			if j != len(tc.in) {
				t.Errorf("position = %d, want %d", j, len(tc.in))
			}
		})
	}
}

// TestString_ErrorParity asserts that for every input scan rejects,
// stdlib json.Unmarshal also rejects (and vice-versa). The exact error
// type differs — scan returns its sentinel; stdlib has its own — but
// the accept/reject decision must match.
func TestString_ErrorParity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want error
	}{
		{"unterminated", `"abc`, ErrUnterminated},
		{"control_char", "\"a\x01b\"", ErrBadString},
		{"bad_escape", `"\x"`, ErrBadString},
		{"truncated_unicode", `"\u00`, ErrBadString},
		{"bad_hex", `"\u00ZZ"`, ErrBadString},
		{"trailing_backslash", `"a\`, ErrBadString},
		{"not_a_string", `123`, ErrExpectString},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := String([]byte(tc.in), 0, true)
			if !errors.Is(err, tc.want) {
				t.Errorf("scan: got %v, want %v", err, tc.want)
			}
			var s string
			stdErr := json.Unmarshal([]byte(tc.in), &s)
			if stdErr == nil {
				t.Errorf("stdlib accepted %q (decoded to %q); scan rejected — parity violation",
					tc.in, s)
			}
		})
	}
}

// TestDetach pins Detach: it returns the SAME string value but detached
// from data — Detach(String(data,…), data) equals the decoded string and
// survives a scribble of the source, whether String aliased (clean path → Detach
// clones) or owned it (escape path → Detach is a no-op). The alloc contract
// (clone IFF the string aliases data) is what makes -copy single-copy on escapes.
func TestDetach(t *testing.T) {
	// Value + scribble-survival across clean (aliased) and escaped (owned) inputs.
	cases := []struct{ in, want string }{
		{`"hello"`, "hello"}, // clean → String aliases → Detach clones
		{`"xéy"`, "xéy"},     // clean multibyte → aliased
		{`"a\nb"`, "a\nb"},   // escape → stringSlow owns → Detach no-op
		{`"😀"`, "😀"},         // surrogate escape → owned
		{`""`, ""},           // empty
	}
	for _, tc := range cases {
		src := []byte(tc.in)
		s, _, err := String(src, 0, true)
		if err != nil {
			t.Fatalf("String(%q): %v", tc.in, err)
		}
		got := Detach(s, src)
		for i := range src {
			src[i] = 'Z' // corrupts any still-aliasing string
		}
		if got != tc.want {
			t.Errorf("Detach(%q): got %q after scribble, want %q", tc.in, got, tc.want)
		}
	}

	// Alloc contract: clone (1 alloc) iff the string aliases data; a non-aliasing
	// (already-owned) string is returned untouched (0 allocs) — the escape-path win.
	owned := string(make([]byte, 64)) // heap string, not in any decode buffer
	buf := []byte("an unrelated buffer entirely")
	if n := testing.AllocsPerRun(100, func() { _ = Detach(owned, buf) }); n != 0 {
		t.Errorf("Detach of a non-aliasing string allocated %v times, want 0", n)
	}
	clean := []byte(`"a clean aliased span with some length"`)
	aliased, _, err := String(clean, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if n := testing.AllocsPerRun(100, func() { _ = Detach(aliased, clean) }); n != 1 {
		t.Errorf("Detach of an aliasing string allocated %v times, want 1", n)
	}

	// Empty aliased string: a zero-length header still carries a pointer into
	// data, pinning the whole backing array under GC — Detach must return the
	// detached "" instead.
	backing := make([]byte, 1<<16)
	empty := unsafe.String(unsafe.SliceData(backing), 0)
	detached := Detach(empty, backing)
	if detached != "" {
		t.Fatalf("Detach(empty alias) = %q, want empty", detached)
	}
	sp := uintptr(unsafe.Pointer(unsafe.StringData(detached)))
	dp := uintptr(unsafe.Pointer(unsafe.SliceData(backing)))
	if sp >= dp && sp < dp+uintptr(len(backing)) {
		t.Errorf("Detach(empty alias) still points into data — backing array stays pinned")
	}
}

// TestString_LoneSurrogateParity: jsonv2 rejects unpaired surrogate escapes
// ("invalid surrogate pair"); scan matches with ErrInvalidUTF8. encoding/json
// v1 instead substitutes U+FFFD silently — an intentional divergence from v1.
func TestString_LoneSurrogateParity(t *testing.T) {
	t.Parallel()
	cases := []string{
		`"\uD83D"`,       // lone high surrogate
		`"\uDC00"`,       // lone low surrogate
		`"\uD83D\uD83D"`, // two highs (invalid pair)
		`"a\uD83Db"`,     // lone surrogate sandwiched
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			if _, _, err := String([]byte(in), 0, true); !errors.Is(err, ErrInvalidUTF8) {
				t.Errorf("want ErrInvalidUTF8, got %v", err)
			}
		})
	}
	// Valid pair (sanity baseline) still decodes to the astral rune.
	if got, _, err := String([]byte(`"😀"`), 0, true); err != nil || got != "😀" {
		t.Errorf("valid pair: got %q err=%v", got, err)
	}
}

// TestString_InvalidUTF8Rejected: raw malformed UTF-8 inside a string literal
// errors with ErrInvalidUTF8 on both the clean-span and escape paths (jsonv2
// parity; v1 would silently substitute U+FFFD).
func TestString_InvalidUTF8Rejected(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, in string }{
		{"lone_ff", "\"ab\xffcd\""},
		{"truncated_2byte", "\"ab\xc3\""},
		{"truncated_3byte_mid", "\"a\xe2(z\""},
		{"overlong", "\"\xc0\x80\""},
		{"utf8_surrogate", "\"\xed\xa0\x80\""},
		{"invalid_after_escape", "\"a\\n\xffz\""},
		{"invalid_before_escape", "\"\xff\\tz\""},
		{"long_span_invalid", `"` + strings.Repeat("x", 64) + "\xfe" + `"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := String([]byte(c.in), 0, true); !errors.Is(err, ErrInvalidUTF8) {
				t.Errorf("want ErrInvalidUTF8, got %v", err)
			}
		})
	}
	// Valid multi-byte UTF-8 passes both paths untouched.
	for _, in := range []string{"\"héllo wörld żółć\"", "\"a\\né😀\""} {
		if got, _, err := String([]byte(in), 0, true); err != nil || got == "" {
			t.Errorf("%q: got %q err=%v", in, got, err)
		}
	}
}

// TestString_ZeroCopyAlias confirms the happy path aliases the input
// (no escapes → returned string shares memory with data).
func TestString_ZeroCopyAlias(t *testing.T) {
	t.Parallel()
	data := []byte(`"hello world"`)
	s, _, err := String(data, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	// The returned string should point inside data[1:12]. Mutate the source
	// and observe the alias change. (This is exactly the unsafe contract
	// callers rely on; the test just confirms it holds.)
	data[1] = 'H'
	if !strings.HasPrefix(s, "H") {
		t.Errorf("expected zero-copy alias, got %q", s)
	}
}

// TestStringEscapeAllocBounded: stringSlow must size its scratch buffer
// off the string span (closing-quote index), not the whole remaining
// payload, and must not pay a second exact-size copy on return. A 1 MiB
// payload with a short escaped string at the front must not allocate
// megabytes per decode.
func TestStringEscapeAllocBounded(t *testing.T) {
	payload := append([]byte(`"ab\nc"`), bytes.Repeat([]byte{' '}, 1<<20)...)
	var got string
	allocs := testing.AllocsPerRun(100, func() {
		s, _, err := String(payload, 0, true)
		if err != nil {
			t.Fatal(err)
		}
		got = s
	})
	if got != "ab\nc" {
		t.Fatalf("decoded %q, want %q", got, "ab\nc")
	}
	if allocs > 1 {
		t.Errorf("escaped-string decode did %v allocs, want 1 (extra string(buf) copy?)", allocs)
	}
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	for range 16 {
		String(payload, 0, true)
	}
	runtime.ReadMemStats(&m1)
	if total := m1.TotalAlloc - m0.TotalAlloc; total > 16*1024 {
		t.Errorf("16 escaped-string decodes allocated %d bytes, want <16KiB (buffer sized off whole payload?)", total)
	}
}

// TestSkipString_ParityWithString: skipString must agree with String on end
// position for every accepted input, and reject everything String rejects
// (error values may differ only where noted — none today).
func TestSkipString_ParityWithString(t *testing.T) {
	t.Parallel()
	for _, tc := range stringHappyCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, wantPos, wantErr := String([]byte(tc.in), 0, true)
			gotPos, gotErr := skipString([]byte(tc.in), 0)
			if gotErr != wantErr || gotPos != wantPos {
				t.Errorf("skipString(%q) = (%d, %v), String = (%d, %v)",
					tc.in, gotPos, gotErr, wantPos, wantErr)
			}
		})
	}
	errCases := []struct {
		name string
		in   string
	}{
		{"unterminated", `"abc`},
		{"control_char", "\"a\x01b\""},
		{"control_after_escape", "\"a\\n\x01b\""},
		{"bad_escape", `"\x"`},
		{"truncated_unicode", `"\u00`},
		{"bad_hex", `"\u00ZZ"`},
		{"trailing_backslash", `"a\`},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := skipString([]byte(tc.in), 0); err == nil {
				t.Errorf("skipString(%q) accepted malformed input", tc.in)
			}
		})
	}
}

// TestSkipString_EscapeDenseLinear pins the memoized quote candidate: an
// escape-dense skipped string used to re-run the closing-quote IndexByte to
// the same far quote per escape — O(n·escapes), ~7 ms for a 48 KB string.
// Parity (incl. escaped quotes, which force a re-locate) plus a growth guard.
func TestSkipString_EscapeDenseLinear(t *testing.T) {
	build := func(n int) []byte {
		var b strings.Builder
		b.WriteByte('"')
		for range n {
			b.WriteString(`\nx`)
		}
		b.WriteByte('"')
		return []byte(b.String())
	}
	cases := [][]byte{
		build(64),
		[]byte(`"` + strings.Repeat(`\"y`, 200) + `"`),
		[]byte(`"` + strings.Repeat(`A\"`, 100) + `zz"`),
	}
	for _, in := range cases {
		_, wantPos, wantErr := String(in, 0, true)
		gotPos, gotErr := skipString(in, 0)
		if gotErr != wantErr || gotPos != wantPos {
			t.Errorf("skipString(%.30q…) = (%d, %v), String = (%d, %v)",
				in, gotPos, gotErr, wantPos, wantErr)
		}
	}
	// Growth guard: 16× the escapes must cost far less than the quadratic
	// ~256×; linear is ~16×. 80× splits them with generous noise headroom.
	fastest := func(data []byte) time.Duration {
		best := time.Duration(1 << 62)
		for range 5 {
			st := time.Now()
			if _, err := skipString(data, 0); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(st); d < best {
				best = d
			}
		}
		return best
	}
	ts, tb := fastest(build(1000)), fastest(build(16000))
	if tb > ts*80 {
		t.Errorf("skipString growth 1k→16k escapes: %v → %v (%.0f×) — quadratic regression",
			ts, tb, float64(tb)/float64(ts))
	}
}

// TestSkipValue_EscapedStringNoAlloc pins that skipping an escaped string
// never allocates (String's stringSlow scratch would).
func TestSkipValue_EscapedStringNoAlloc(t *testing.T) {
	data := []byte(`"a\nbéc\t` + strings.Repeat("x", 100) + `"`)
	allocs := testing.AllocsPerRun(100, func() {
		if _, err := SkipValue(data, 0); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Errorf("SkipValue over escaped string allocated %v/op, want 0", allocs)
	}
}

// TestString_CtrlBeforeEscapeRejected pins the stringSlow prefix fix: an
// unescaped control byte BEFORE the first backslash must reject (stdlib
// parity) — the prefix used to be copied into scratch unchecked.
func TestString_CtrlBeforeEscapeRejected(t *testing.T) {
	t.Parallel()
	in := []byte("\"a\x01b\\nc\"")
	if _, _, err := String(in, 0, true); err != ErrBadString {
		t.Errorf("String = %v, want ErrBadString", err)
	}
	var v string
	if err := json.Unmarshal(in, &v); err == nil {
		t.Errorf("stdlib accepted %q — parity assumption broken", in)
	}
	// Stream path shares the fix via (*Stream).stringSlow.
	var s Stream
	s.Reset(bytes.NewReader(in), nil)
	if _, err := s.String(true); err != ErrBadString {
		t.Errorf("Stream.String = %v, want ErrBadString", err)
	}
}
