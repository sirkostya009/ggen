package ggen

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/rand"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

// stringHappyCases are complete JSON strings that the standard library
// accepts. String must decode each to the same Go string.
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
	{"unicode_bmp", `"é"`},
	{"unicode_surrogate_pair", `"😀"`},
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

// TestString_ErrorParity checks that json.Unmarshal also rejects every input
// that String rejects. String must return the listed error. The standard
// library has its own error types, so only the reject decision is compared.
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

// TestDetach checks Detach. A string aliases data when it points into data's
// bytes without a copy. Detach returns the same string value, but no longer
// tied to data. The result must survive overwriting data. When String
// aliased data (no escapes), Detach clones the string. When String already
// made its own copy (escapes), Detach does nothing. Detach clones only when
// the string aliases data. That is why -copy mode copies an escaped string
// only once.
func TestDetach(t *testing.T) { //nolint:paralleltest // measures allocations
	// The value must survive overwriting the source, for both aliased and
	// copied strings.
	cases := []struct{ in, want string }{
		{`"hello"`, "hello"}, // no escape: String aliases, Detach clones
		{`"xéy"`, "xéy"},     // no escape, multi-byte: aliased
		{`"a\nb"`, "a\nb"},   // escape: stringSlow copies, Detach does nothing
		{`"😀"`, "😀"},         // no escape, 4-byte rune: aliased
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
			src[i] = 'Z' // breaks any string that still aliases src
		}
		if got != tc.want {
			t.Errorf("Detach(%q): got %q after scribble, want %q", tc.in, got, tc.want)
		}
	}

	// Allocations: one when the string aliases data, because Detach clones
	// it. Zero when the string does not alias data, because Detach returns it
	// as is. The zero case is what saves a copy for escaped strings.
	owned := string(make([]byte, 64)) // its own heap string, not in buf
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

	// An empty string can still point into data. That pointer keeps the whole
	// backing array alive for the GC. Detach must return an empty string that
	// does not point into data.
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

// TestString_LoneSurrogateParity checks unpaired surrogate escapes. A
// character above U+FFFF is written in JSON as two \uXXXX escapes, a high
// surrogate then a low surrogate. jsonv2 rejects a surrogate escape without
// its partner, and String does too, with ErrInvalidUTF8. encoding/json v1
// silently replaces it with U+FFFD. ggen follows jsonv2 here on purpose.
func TestString_LoneSurrogateParity(t *testing.T) {
	t.Parallel()
	cases := []string{
		`"\uD83D"`,       // lone high surrogate
		`"\uDC00"`,       // lone low surrogate
		`"\uD83D\uD83D"`, // two highs (invalid pair)
		`"a\uD83Db"`,     // lone surrogate between letters
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			if _, _, err := String([]byte(in), 0, true); !errors.Is(err, ErrInvalidUTF8) {
				t.Errorf("want ErrInvalidUTF8, got %v", err)
			}
		})
	}
	// A valid 4-byte character still decodes.
	if got, _, err := String([]byte(`"😀"`), 0, true); err != nil || got != "😀" {
		t.Errorf("valid pair: got %q err=%v", got, err)
	}
}

// TestString_InvalidUTF8Rejected checks that invalid UTF-8 bytes inside a
// string give ErrInvalidUTF8. This must hold for strings with and without
// escapes, which take different code paths. jsonv2 does the same. encoding/json
// v1 would silently replace the bytes with U+FFFD.
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
	// Valid multi-byte UTF-8 passes on both paths.
	for _, in := range []string{"\"héllo wörld żółć\"", "\"a\\né😀\""} {
		if got, _, err := String([]byte(in), 0, true); err != nil || got == "" {
			t.Errorf("%q: got %q err=%v", in, got, err)
		}
	}
}

// TestString_ZeroCopyAlias checks that a string without escapes aliases the
// input. The returned string shares memory with data.
func TestString_ZeroCopyAlias(t *testing.T) {
	t.Parallel()
	data := []byte(`"hello world"`)
	s, _, err := String(data, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	// The string points into data[1:12]. Changing data must change the
	// string. Callers rely on this unsafe behaviour.
	data[1] = 'H'
	if !strings.HasPrefix(s, "H") {
		t.Errorf("expected zero-copy alias, got %q", s)
	}
}

// TestStringEscapeAllocBounded checks how stringSlow sizes its buffer. It
// must size it from the string itself, up to the closing quote, not from the
// whole rest of the input. It must also not copy the result a second time on
// return. A short escaped string at the front of 1 MiB of input must not
// allocate megabytes per decode.
func TestStringEscapeAllocBounded(t *testing.T) { //nolint:paralleltest // measures allocations
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
		_, _, _ = String(payload, 0, true)
	}
	runtime.ReadMemStats(&m1)
	if total := m1.TotalAlloc - m0.TotalAlloc; total > 16*1024 {
		t.Errorf("16 escaped-string decodes allocated %d bytes, want <16KiB (buffer sized off whole payload?)", total)
	}
}

// TestSkipString_ParityWithString checks that skipString returns the same end
// position and error as String for every valid input. It must also reject
// every input that String rejects.
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

// TestSkipString_EscapeDenseLinear checks that skipString stays linear on a
// string with many escapes. skipString remembers where it last found a
// quote. Without that, it would search to the same far closing quote again
// after every escape, which is quadratic. The test checks results against
// String, including escaped quotes, which force a new search. It then checks
// how the run time grows.
func TestSkipString_EscapeDenseLinear(t *testing.T) {
	t.Parallel()
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
	// 16 times the escapes should take about 16 times as long if linear,
	// and about 256 times if quadratic. The 80 times limit sits between
	// them, with plenty of room for timing noise.
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

// TestSkipValue_EscapedStringNoAlloc checks that skipping an escaped string
// never allocates. Decoding it with String would allocate a buffer.
func TestSkipValue_EscapedStringNoAlloc(t *testing.T) { //nolint:paralleltest // measures allocations
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

// TestString_CtrlBeforeEscapeRejected checks that a raw control byte before
// the first backslash is rejected, like the standard library does.
// stringSlow copies the bytes before the first escape in one go, and it must
// still check them.
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
	// Stream must reject it too, in (*Stream).stringSlow.
	var s Stream
	s.Reset(bytes.NewReader(in), nil)
	if _, err := s.String(true); err != ErrBadString {
		t.Errorf("Stream.String = %v, want ErrBadString", err)
	}
}

// stringSlowRef is a simple version of stringSlow that copies one byte at a
// time. stringSlow copies runs of plain bytes in windows of escRunWindow
// bytes. stringSlowRef is the reference it is tested against. The value,
// position and exact error must match.
func stringSlowRef(data []byte, start, j, capHint int, validate bool) (string, int, error) {
	bad, rawHigh := ctrlOrHigh(data[start:j])
	if bad {
		for k := start; k < j; k++ {
			if data[k] < 0x20 {
				return "", k, ErrBadString
			}
		}
	}
	buf := make([]byte, 0, capHint)
	buf = append(buf, data[start:j]...)
	var rawHi byte
	for j < len(data) {
		c := data[j]
		if c == '"' {
			if validate && (rawHigh || rawHi&0x80 != 0) && !utf8.Valid(buf) {
				return "", j, ErrInvalidUTF8
			}
			return string(buf), j + 1, nil
		}
		if c == '\\' {
			if j+1 >= len(data) {
				return "", len(data), ErrBadString
			}
			esc := data[j+1]
			switch esc {
			case '"', '\\', '/':
				buf = append(buf, esc)
				j += 2
			case 'b':
				buf = append(buf, '\b')
				j += 2
			case 'f':
				buf = append(buf, '\f')
				j += 2
			case 'n':
				buf = append(buf, '\n')
				j += 2
			case 'r':
				buf = append(buf, '\r')
				j += 2
			case 't':
				buf = append(buf, '\t')
				j += 2
			case 'u':
				if j+6 > len(data) {
					if uEscapePrefixRef(data[j:]) {
						return "", len(data), ErrBadString
					}
					return "", j, ErrBadString
				}
				r, ok := parseHex4(data[j+2 : j+6])
				if !ok {
					return "", j, ErrBadString
				}
				j += 6
				if utf16.IsSurrogate(r) {
					if j+6 <= len(data) {
						if data[j] == '\\' && data[j+1] == 'u' {
							if r2, ok := parseHex4(data[j+2 : j+6]); ok {
								if dec := utf16.DecodeRune(r, r2); dec != utf8.RuneError {
									r = dec
									j += 6
								}
							}
						}
					} else if validate && uEscapePrefixRef(data[j:]) {
						return "", len(data), ErrInvalidUTF8
					}
					if validate && utf16.IsSurrogate(r) {
						return "", j, ErrInvalidUTF8
					}
				}
				buf = utf8.AppendRune(buf, r)
			default:
				return "", j, ErrBadString
			}
			continue
		}
		if c < 0x20 {
			return "", j, ErrBadString
		}
		rawHi |= c
		buf = append(buf, c)
		j++
	}
	return "", len(data), ErrUnterminated
}

// uEscapePrefixRef reports whether tail could still grow into a full \uXXXX
// escape. It is written out by hand so the reference shares no code with
// stringSlow.
func uEscapePrefixRef(tail []byte) bool {
	for k, c := range tail {
		switch {
		case k == 0 && c == '\\', k == 1 && c == 'u':
		case k >= 2 && (c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'):
		default:
			return false
		}
	}
	return true
}

// escapeAlphabet holds pieces for random strings that test the edges of
// the escRunWindow copy. It has plain runs shorter and longer than the
// window, escapes that cross a window edge, control and non-ASCII bytes, and
// \u forms that are easy to cut short.
var escapeAlphabet = []string{
	`a`, `bc`, `def`, `ghijklmn`, `opqrstuvwxyz0123`,
	`0123456789abcdef0123456789abcdef`, // twice the window
	`\"`, `\\`, `\/`, `\b`, `\f`, `\n`, `\r`, `\t`,
	`A`, `é`, `😀`, `\ud83d`, `\ude00`, `\u00`, `\uZZZZ`,
	"é", "日本語", "😀", "\xc3", "\xff\xfe", "\x80",
	"\x01", "\x1f", "\x7f", " ", "\t\\t",
}

// TestStringSlow_RefDifferential checks stringSlow against the byte-at-a-time
// reference on random strings with escapes, control bytes and non-ASCII
// bytes. The value, end position and error must match, with and without
// UTF-8 validation.
func TestStringSlow_RefDifferential(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(20260809))
	for range 200000 {
		body := make([]byte, 0, 64)
		for range rng.Intn(9) {
			body = append(body, escapeAlphabet[rng.Intn(len(escapeAlphabet))]...)
		}
		// Add at least one escape so the input reaches stringSlow.
		body = append(body, `\n`...)
		for range rng.Intn(6) {
			body = append(body, escapeAlphabet[rng.Intn(len(escapeAlphabet))]...)
		}
		if rng.Intn(4) != 0 { // 3 in 4 get a closing quote
			body = append(body, '"')
		}
		// Sometimes cut the input short, to end inside an escape or a run.
		if rng.Intn(5) == 0 && len(body) > 0 {
			body = body[:rng.Intn(len(body))]
		}
		data := append([]byte(`"`), body...)

		for _, validate := range [2]bool{true, false} {
			// start=1 is just past the opening quote. j is the first
			// backslash, as the callers compute it. With no backslash there
			// is nothing to compare.
			j := -1
			for k := 1; k < len(data); k++ {
				if data[k] == '\\' {
					j = k
					break
				}
				if data[k] == '"' {
					break
				}
			}
			if j < 0 {
				continue
			}
			capHint := stringSpanEnd(data, 1) - 1

			gotV, gotP, gotE := stringSlow(data, 1, j, capHint, validate)
			wantV, wantP, wantE := stringSlowRef(data, 1, j, capHint, validate)
			if gotE != wantE || gotP != wantP || gotV != wantV {
				t.Fatalf("mismatch validate=%v input=%q\n got: %q %d %v\nwant: %q %d %v",
					validate, data, gotV, gotP, gotE, wantV, wantP, wantE)
			}
		}
	}
}

func naiveHasCtrl(b []byte) bool {
	for _, c := range b {
		if c < 0x20 {
			return true
		}
	}
	return false
}

// TestHasCtrlByte_DifferentialExhaustive checks hasCtrlByte against a simple
// byte loop. hasCtrlByte uses SWAR: it loads 8 bytes as one uint64 and tests
// all 8 with a few arithmetic ops. The test puts a control byte at every
// position, so it lands at each of the 8 offsets within a word. Bytes with
// the high bit set (UTF-8) are mixed in and must not count as control
// bytes.
func TestHasCtrlByte_DifferentialExhaustive(t *testing.T) {
	t.Parallel()
	// Lengths 0 to 40 cover the 8-byte word loop, the byte-by-byte tail,
	// and the point where one hands off to the other.
	for n := range 41 {
		// Clean input: printable ASCII mixed with high-bit bytes, none of
		// them control bytes.
		base := make([]byte, n)
		for i := range base {
			if i%3 == 0 {
				base[i] = 0x80 + byte(i%0x80) // high bit set, not a control byte
			} else {
				base[i] = 0x20 + byte(i%0x5f) // printable ASCII
			}
		}
		if got, want := hasCtrlByte(base), naiveHasCtrl(base); got != want {
			t.Fatalf("clean n=%d: hasCtrlByte=%v naive=%v (%v)", n, got, want, base)
		}
		// Put a control byte at every position, using several control values.
		for pos := range n {
			for _, ctrl := range []byte{0x00, 0x01, 0x09, 0x0a, 0x0d, 0x1f} {
				b := append([]byte(nil), base...)
				b[pos] = ctrl
				if got, want := hasCtrlByte(b), naiveHasCtrl(b); got != want {
					t.Fatalf("n=%d pos=%d ctrl=%#x: hasCtrlByte=%v naive=%v",
						n, pos, ctrl, got, want)
				}
			}
		}
	}
}

// TestHasCtrlByte_Boundary checks 0x1f, the highest control byte, against
// 0x20, the space and lowest allowed byte. The byte sits at the end of
// inputs sized around 8 and 16 bytes.
func TestHasCtrlByte_Boundary(t *testing.T) {
	t.Parallel()
	for _, n := range []int{7, 8, 9, 15, 16, 17} {
		clean := make([]byte, n)
		for i := range clean {
			clean[i] = 0x20 // space, the lowest allowed byte
		}
		if hasCtrlByte(clean) {
			t.Fatalf("n=%d all-0x20 reported ctrl", n)
		}
		dirty := append([]byte(nil), clean...)
		dirty[n-1] = 0x1f
		if !hasCtrlByte(dirty) {
			t.Fatalf("n=%d trailing 0x1f not detected", n)
		}
	}
}

// BenchmarkStringUTF8Cost splits the cost of decoding a string. String first
// finds the closing quote and checks for control bytes, then runs
// utf8.Valid only if it saw non-ASCII bytes. The rows compare full String on
// ASCII text, full String on Cyrillic text (2 bytes per letter), and
// utf8.Valid alone. The gap between the two String rows is the most a
// single-pass validator could save.
func BenchmarkStringUTF8Cost(b *testing.B) {
	sizes := []struct {
		name string
		n    int
	}{{"16B", 16}, {"64B", 64}, {"256B", 256}, {"1KB", 1024}}
	for _, sz := range sizes {
		ascii := strings.Repeat("abcdefgh", sz.n/8)
		cyr := strings.Repeat("аб", sz.n/4) // 2 letters, 4 bytes
		asciiQ := []byte(`"` + ascii + `"`)
		cyrQ := []byte(`"` + cyr + `"`)
		b.Run(sz.name+"/String_ascii", func(b *testing.B) {
			for b.Loop() {
				_, _, _ = String(asciiQ, 0, true)
			}
		})
		b.Run(sz.name+"/String_cyrillic", func(b *testing.B) {
			for b.Loop() {
				_, _, _ = String(cyrQ, 0, true)
			}
		})
		b.Run(sz.name+"/utf8Valid_cyrillic", func(b *testing.B) {
			body := []byte(cyr)
			for b.Loop() {
				_ = utf8.Valid(body)
			}
		})
	}
}

// TestString_MalformedTailIsFinal checks the error position at the end of the
// data. If the bytes already present are broken, the error position must be
// inside data. Only a tail that more bytes could still complete reports
// len(data), which means "cut short". String and skipString must agree.
// This matters for Stream. CaptureValue treats a len(data) position as "read
// more and retry". A broken value reported as cut short would make a live
// reader block forever, and Stream would report a different error than
// []byte input.
func TestString_MalformedTailIsFinal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in  string
		pos int
		err error
	}{
		// A non-hex byte among the \u digits: broken, at the backslash.
		{`"\u12"`, 1, ErrBadString},
		{`"\u1"`, 1, ErrBadString},
		{`"\u"`, 1, ErrBadString},
		{`"ab\uZ"`, 3, ErrBadString},
		{`"abc\u00"`, 4, ErrBadString},
		{`"\u00ZZ"`, 1, ErrBadString},
		// Every byte present is still a valid start: cut short.
		{`"\u00`, 5, ErrBadString},
		{`"\u`, 3, ErrBadString},
		{`"\`, 2, ErrBadString},
		// A control byte in an unclosed string: broken, at the byte itself,
		// never ErrUnterminated.
		{"\"ab\x01}", 3, ErrBadString},
		{"\"ab\x01", 3, ErrBadString},
		{"\"ab\x01\\n", 3, ErrBadString},
		{`"ab`, 3, ErrUnterminated},
	}
	for _, tc := range cases {
		data := []byte(tc.in)
		if _, p, err := String(data, 0, true); err != tc.err || p != tc.pos {
			t.Errorf("String(%q) = (%d, %v), want (%d, %v)", tc.in, p, err, tc.pos, tc.err)
		}
		if p, err := skipString(data, 0); err != tc.err || p != tc.pos {
			t.Errorf("skipString(%q) = (%d, %v), want (%d, %v)", tc.in, p, err, tc.pos, tc.err)
		}
	}
	// After a high surrogate escape, String looks for the low half. A tail
	// that can still become \uXXXX is cut short. Anything else is a lone
	// surrogate, reported at the current position.
	for _, tc := range []struct {
		in  string
		pos int
	}{
		{`"\ud83d\uDE`, 11},
		{`"\ud83d\u`, 9},
		{`"\ud83d`, 7},
		{`"\ud83d"`, 7},
		{`"\ud83d\n"`, 7},
		{`"\ud83dx"`, 7},
	} {
		if _, p, err := String([]byte(tc.in), 0, true); err != ErrInvalidUTF8 || p != tc.pos {
			t.Errorf("String(%q) = (%d, %v), want (%d, %v)", tc.in, p, err, tc.pos, ErrInvalidUTF8)
		}
	}
}

// When String finds no closing quote, it must pick the error without
// copying the input.
func TestStringUnterminatedEscapedNoAlloc(t *testing.T) { //nolint:paralleltest // measures allocations
	data := append([]byte(`"a\n`), bytes.Repeat([]byte("x"), 1<<20)...)
	var pos int
	var err error
	if allocs := testing.AllocsPerRun(5, func() {
		_, pos, err = String(data, 0, true)
	}); allocs != 0 {
		t.Errorf("String(unterminated escaped) allocates %v per call, want 0", allocs)
	}
	if pos != len(data) || err != ErrUnterminated {
		t.Errorf("String = (%d, %v), want (%d, %v)", pos, err, len(data), ErrUnterminated)
	}
}

// stringUnterminated handles a string with no closing quote. It must return
// the same position and error as stringSlow for every kind of escape,
// surrogate and control byte, with and without validation.
func TestStringUnterminatedParity(t *testing.T) {
	t.Parallel()
	bodies := []string{
		`a\n`, `a\`, `a\u`, `a\u12`, `a\u12zz`, `a\q`, `a\nx`, `\ud800`, `\ud800abc`,
		"\xf0\x90\x80\x80\\n", `\ud800\udc0`, `\ud800A`, "x\xc3\xa9y\\t", `\/\b\f\r\t`,
		"a\\n\x01b", "\x01a\\n", "ab\x01\\n", `\ud800\ud800`, `\ud83d\ude00tail`, `\uD83D\uDE00`,
		`\u0041`, `\u00`, "\\n\xff\xfe", `\ud800\u0041`,
	}
	alpha := []byte{'\\', 'u', 'n', 'q', '/', '0', '1', 'a', 'f', 'd', '8', 'D', 'A', 'x', 0x01, 0x1f, 0x80, 0xc3, 0xa9, 0xf0}
	rng := uint64(12345)
	next := func() uint64 { rng ^= rng << 13; rng ^= rng >> 7; rng ^= rng << 17; return rng }
	for range 20000 {
		body := []byte{'\\'}
		for i, l := 0, int(next()%14)+1; i < l; i++ {
			body = append(body, alpha[next()%uint64(len(alpha))])
		}
		if bytes.IndexByte(body, '"') < 0 {
			bodies = append(bodies, string(body))
		}
	}
	for _, body := range bodies {
		data := []byte(`"` + body)
		bs := bytes.IndexByte(data[1:], '\\')
		if bs < 0 || bytes.IndexByte(data[1:], '"') >= 0 {
			t.Fatalf("bad case %q", body)
		}
		for _, validate := range []bool{true, false} {
			_, wp, we := stringSlow(data, 1, 1+bs, bs+16, validate)
			_, gp, ge := stringUnterminated(data, 1, 1+bs, validate)
			if wp != gp || we != ge {
				t.Errorf("%q validate=%v: stringSlow (%d,%v), stringUnterminated (%d,%v)", body, validate, wp, we, gp, ge)
			}
		}
	}
}
