package scan

import (
	"bytes"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
)

// stringHappyCases: every input is a complete JSON string literal that
// stdlib accepts. scan.String must produce the same Go string.
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
	for _, tc := range stringHappyCases {
		t.Run(tc.name, func(t *testing.T) {
			var want string
			if err := json.Unmarshal([]byte(tc.in), &want); err != nil {
				t.Fatalf("stdlib: %v", err)
			}
			got, j, err := String([]byte(tc.in), 0)
			if err != nil {
				t.Fatalf("scan.String: %v", err)
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
			_, _, err := String([]byte(tc.in), 0)
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

// TestString_LoneSurrogateParity: stdlib substitutes U+FFFD for an
// unpaired surrogate. scan's stringSlow path emits the rune via
// utf8.AppendRune which also yields U+FFFD for invalid runes — outputs
// must match exactly.
func TestString_LoneSurrogateParity(t *testing.T) {
	cases := []string{
		`"\uD83D"`,       // lone high surrogate
		`"\uDC00"`,       // lone low surrogate
		`"\uD83D\uD83D"`, // two highs (invalid pair)
		`"a\uD83Db"`,     // lone surrogate sandwiched
		`"😀"`,            // valid pair (sanity baseline)
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			var want string
			if err := json.Unmarshal([]byte(in), &want); err != nil {
				t.Fatalf("stdlib: %v", err)
			}
			got, _, err := String([]byte(in), 0)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if got != want {
				t.Errorf("mismatch\n got: %q (% x)\nwant: %q (% x)", got, got, want, want)
			}
		})
	}
}

// TestString_ZeroCopyAlias confirms the happy path aliases the input
// (no escapes → returned string shares memory with data).
func TestString_ZeroCopyAlias(t *testing.T) {
	data := []byte(`"hello world"`)
	s, _, err := String(data, 0)
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
		s, _, err := String(payload, 0)
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
		String(payload, 0)
	}
	runtime.ReadMemStats(&m1)
	if total := m1.TotalAlloc - m0.TotalAlloc; total > 16*1024 {
		t.Errorf("16 escaped-string decodes allocated %d bytes, want <16KiB (buffer sized off whole payload?)", total)
	}
}
