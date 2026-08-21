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

// stringSlowRef is the pre-gate stringSlow: a byte-at-a-time copy with no bulk
// arm, kept as the differential oracle for the escRunWindow hybrid. Value,
// position, and error identity must match the shipped version exactly.
func stringSlowRef(data []byte, start, j, capHint int, validate bool) (string, int, error) {
	bad, rawHigh := ctrlOrHigh(data[start:j])
	if bad {
		return "", start, ErrBadString
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
					return "", len(data), ErrBadString
				}
				r, ok := parseHex4(data[j+2 : j+6])
				if !ok {
					return "", j, ErrBadString
				}
				j += 6
				if utf16.IsSurrogate(r) {
					if j+6 <= len(data) && data[j] == '\\' && data[j+1] == 'u' {
						if r2, ok := parseHex4(data[j+2 : j+6]); ok {
							if dec := utf16.DecodeRune(r, r2); dec != utf8.RuneError {
								r = dec
								j += 6
							}
						}
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

// escapeAlphabet: fragments that stress every boundary the escRunWindow gate
// introduces — runs shorter and longer than the window, escapes straddling it,
// ctrl/high bytes on both sides of the switchover, truncation-prone \u forms.
var escapeAlphabet = []string{
	`a`, `bc`, `def`, `ghijklmn`, `opqrstuvwxyz0123`,
	`0123456789abcdef0123456789abcdef`, // 2× window
	`\"`, `\\`, `\/`, `\b`, `\f`, `\n`, `\r`, `\t`,
	`A`, `é`, `😀`, `\ud83d`, `\ude00`, `\u00`, `\uZZZZ`,
	"é", "日本語", "😀", "\xc3", "\xff\xfe", "\x80",
	"\x01", "\x1f", "\x7f", " ", "\t\\t",
}

// TestStringSlow_RefDifferential pins the escRunWindow bulk arm against the
// byte-at-a-time reference: same value, same end position, same error, for
// randomized escape/ctrl/high-byte bodies in both validate modes.
func TestStringSlow_RefDifferential(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(20260809))
	for range 200000 {
		body := make([]byte, 0, 64)
		for range rng.Intn(9) {
			body = append(body, escapeAlphabet[rng.Intn(len(escapeAlphabet))]...)
		}
		// Ensure at least one escape so the input actually reaches stringSlow.
		body = append(body, `\n`...)
		for range rng.Intn(6) {
			body = append(body, escapeAlphabet[rng.Intn(len(escapeAlphabet))]...)
		}
		if rng.Intn(4) != 0 { // 3/4 terminated, 1/4 truncated
			body = append(body, '"')
		}
		// Randomly truncate to exercise every partial-escape/partial-run edge.
		if rng.Intn(5) == 0 && len(body) > 0 {
			body = body[:rng.Intn(len(body))]
		}
		data := append([]byte(`"`), body...)

		for _, validate := range [2]bool{true, false} {
			// start=1 (past the opening quote); j = first backslash, as the
			// callers compute it. No backslash → nothing to compare.
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

// TestHasCtrlByte_DifferentialExhaustive places a control byte at every
// position of buffers spanning all 8 word-phase alignments, alongside
// high-bit (UTF-8) bytes that must NOT false-positive, and checks the SWAR
// path agrees with the naive byte loop bit-for-bit.
func TestHasCtrlByte_DifferentialExhaustive(t *testing.T) {
	t.Parallel()
	// Base spans cover lengths around the 8-byte word boundary (0..40) so
	// the SWAR loop, the scalar tail, and their seam are all exercised.
	for n := 0; n <= 40; n++ {
		// All-clean baseline: mix ASCII printable and high (UTF-8) bytes,
		// none of which are control chars.
		base := make([]byte, n)
		for i := range base {
			if i%3 == 0 {
				base[i] = 0x80 + byte(i%0x80) // high bit set, must not trip
			} else {
				base[i] = 0x20 + byte(i%0x5f) // printable ASCII
			}
		}
		if got, want := hasCtrlByte(base), naiveHasCtrl(base); got != want {
			t.Fatalf("clean n=%d: hasCtrlByte=%v naive=%v (%v)", n, got, want, base)
		}
		// Inject a control byte at every position, with every control value.
		for pos := 0; pos < n; pos++ {
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

// TestHasCtrlByte_Boundary pins 0x1f (control) vs 0x20 (space, the lowest
// legal byte) at the exact lane boundary.
func TestHasCtrlByte_Boundary(t *testing.T) {
	t.Parallel()
	for _, n := range []int{7, 8, 9, 15, 16, 17} {
		clean := make([]byte, n)
		for i := range clean {
			clean[i] = 0x20 // all spaces — lowest legal
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

// BenchmarkStringUTF8Cost splits the two-pass string decode cost: full
// String (locate + checkSpan + gated utf8.Valid) vs the utf8.Valid pass
// alone, on ASCII vs Cyrillic (2-byte runes) spans. The delta between the
// ascii and cyrillic String rows is what a fused (single-pass) validator
// could at best reclaim.
func BenchmarkStringUTF8Cost(b *testing.B) {
	sizes := []struct {
		name string
		n    int
	}{{"16B", 16}, {"64B", 64}, {"256B", 256}, {"1KB", 1024}}
	for _, sz := range sizes {
		ascii := strings.Repeat("abcdefgh", sz.n/8)
		cyr := strings.Repeat("аб", sz.n/4) // 2 runes = 4 bytes
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
