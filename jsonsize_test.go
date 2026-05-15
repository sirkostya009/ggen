package main

import (
	"math"
	"math/big"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sirkostya009/ggen/thirdparty2"
)

// URLStruct isolates a single url.URL field so the URL component
// summing in JSONSize can be exercised without other field
// contributions muddying the math.
//
//ggen:generate
type URLStruct struct {
	Site url.URL `json:"site"`
}

// JSONSize is documented as an upper bound: encode.Marshal preallocates
// exactly that many bytes and expects AppendJSON to never grow the
// buffer. The bound covers realistic worst-case input (long ASCII, JSON
// short-escapes \n \" \\ \t etc., html chars without htmlescape mode,
// max-width numbers, deep nesting). Control chars below 0x20 that
// expand to \uXXXX (6×) are intentionally NOT covered — they're rare
// in real payloads and the one-time realloc on pathological input is
// an acceptable trade for keeping the preallocated buffer tight.
//
// Each case picks input that maximizes its size class while staying in
// the realistic regime, then verifies AppendJSON fits inside the
// JSONSize cap.
func TestJSONSize_NoReallocOnWorstCase(t *testing.T) {
	t.Parallel()

	// Plain ASCII — pass-through, exercises the no-escape happy path.
	worstASCII := strings.Repeat("a", 128)
	// HTML chars without htmlescape: literal pass-through in
	// AppendStringNoHTML (Node's encoder), so 1× like plain ASCII.
	worstHTML := strings.Repeat("<>&", 32)
	// JSON short-escape chars — every byte becomes 2 (matches the 2×
	// budget exactly). The tightest legal input under the bound.
	worstShort := strings.Repeat(`"\`+"\n\t", 16)

	cases := []struct {
		name string
		v    interface {
			AppendJSON(dst []byte) ([]byte, error)
			JSONSize() int
		}
	}{
		{
			name: "Node_deep_max_numbers_and_strings",
			v: Node{
				ID:     -1 << 62,
				Name:   worstShort,
				Score:  -1.7976931348623157e+308,
				Active: true,
				Tags:   []string{worstASCII, worstHTML, worstShort},
				Props: map[string]string{
					worstASCII: worstShort,
					worstHTML:  worstASCII,
					worstShort: worstHTML,
				},
				Children: []Node{
					{
						ID:    -1 << 62,
						Name:  worstShort,
						Score: -1.7976931348623157e+308,
						Tags:  []string{worstASCII, worstShort},
						Children: []Node{
							{Name: worstShort, Tags: []string{worstShort}},
						},
					},
				},
			},
		},
		{
			name: "WideStruct_all_fields_short_escapes",
			v:    wideStructAllShort(worstShort),
		},
		{
			name: "Address_all_short_escapes",
			v: Address{
				Street:  worstShort,
				City:    worstShort,
				ZipCode: "00000",
			},
		},
		{
			// OmitStruct exercises omitempty/omitzero. With every
			// omit-able field at its zero value, JSONSize must not
			// reserve room for them — otherwise the preallocated cap
			// stays full-width even though the actual output is just
			// `{"name":...,"count":"..."}`. Sanity-checked separately
			// against the all-populated variant below.
			name: "OmitStruct_all_omits_at_zero",
			v: OmitStruct{
				Name:     "alice",
				StrCount: 42,
			},
		},
		{
			// NativeTypes exercises every format-specific size path:
			// time (RFC3339Nano default + unix int + RFC3339), duration
			// (sec float + units string), bytes (base64/hex/array), and
			// IPv6 for the IP family — the wider branch of the v4/v6
			// runtime split. JSONSize must cover all of them under one
			// preallocated cap with no realloc.
			name: "NativeTypes_v6_max_formats",
			v: NativeTypes{
				CreatedAt: time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC),
				UnixAt:    time.Unix(math.MaxInt32, 0).UTC(),
				IssuedAt:  time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC),
				SecDur:    time.Duration(math.MaxInt64),
				UnitDur:   time.Duration(math.MinInt64),
				Blob:      make([]byte, 96),
				HexBlob:   make([]byte, 96),
				ByteArray: make([]byte, 96),
				LegacyIP:  net.ParseIP("2001:db8:85a3::8a2e:370:7334"),
				Addr:      netip.MustParseAddr("2001:db8:85a3::8a2e:370:7334"),
				Cidr:      netip.MustParsePrefix("2001:db8::/32"),
			},
		},
		{
			// IPv4 branch — same struct, smaller per-field runtime
			// contribution. Confirms the v4 path doesn't underestimate
			// despite the 15-byte vs 39-byte split.
			name: "NativeTypes_v4_formats",
			v: NativeTypes{
				CreatedAt: time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
				UnixAt:    time.Unix(0, 0).UTC(),
				IssuedAt:  time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
				SecDur:    time.Second,
				UnitDur:   time.Hour,
				Blob:      make([]byte, 32),
				HexBlob:   make([]byte, 32),
				ByteArray: make([]byte, 32),
				LegacyIP:  net.ParseIP("192.168.1.1"),
				Addr:      netip.MustParseAddr("10.0.0.1"),
				Cidr:      netip.MustParsePrefix("10.0.0.0/8"),
			},
		},
		{
			// RichTypes exercises URL component summing (Scheme/Host/
			// Path/RawQuery/Fragment), big.Int (BitLen-based), big.Float
			// (66-byte cap), big.Rat (Num+Denom BitLens), uuid via
			// TextAppender, plus json.RawMessage / jsontext.Value
			// passthrough. Tightens the URL bound to actual length.
			name: "RichTypes_full_url_and_bignums",
			v:    richTypesWorst(),
		},
		{
			// PointerStruct: nil-pointer omitempty fields are skipped
			// entirely (must shrink JSONSize), while non-nil pointers
			// dereference cleanly without re-checking nil inside the
			// outer guard. All populated here to exercise the
			// non-nil branch through every pointee kind.
			name: "PointerStruct_all_populated",
			v: PointerStruct{
				Name:    new(worstShort),
				Count:   new(-1 << 62),
				Ratio:   new(-1.7976931348623157e+308),
				Addr:    &Address{Street: worstShort, City: worstShort, ZipCode: "00000"},
				When:    new(time.Unix(math.MaxInt32, 0).UTC()),
				Enabled: new(false),
			},
		},
		{
			// Cross-package struct field: External2 lives in
			// thirdparty2 and ggen-generates JSONSize there. The
			// generator's go/types introspection must detect that
			// method and call it (not fall back to a flat 128).
			name: "FastFallbackStruct_foreign_jsonsize",
			v: FastFallbackStruct{
				ID:    worstShort,
				Extra: thirdparty2.External2{Key: worstShort, Value: math.MaxInt32},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			size := c.v.JSONSize()
			buf := make([]byte, 0, size)
			got, err := c.v.AppendJSON(buf)
			if err != nil {
				t.Fatalf("AppendJSON: %v", err)
			}
			if cap(got) != size {
				t.Errorf("realloc happened: JSONSize=%d but cap(out)=%d (out len=%d)\n%s",
					size, cap(got), len(got), summarizeOverflow(size, len(got)))
			}
			if len(got) > size {
				t.Errorf("output exceeded JSONSize budget: len=%d > size=%d", len(got), size)
			}
		})
	}
}

// JSONSize must shrink when the actual value lives in the cheaper
// branch of a runtime split (IPv4 < IPv6, short URL < long URL, …).
// These regressions look like "still passes worst-case cap test"
// because the bound is still an upper bound — they only show up as
// over-allocation. Pin the tightness explicitly.
func TestJSONSize_RuntimeBranches(t *testing.T) {
	t.Parallel()

	v4 := NativeTypes{
		LegacyIP: net.ParseIP("192.168.1.1"),
		Addr:     netip.MustParseAddr("10.0.0.1"),
		Cidr:     netip.MustParsePrefix("10.0.0.0/8"),
	}
	v6 := NativeTypes{
		LegacyIP: net.ParseIP("2001:db8::1"),
		Addr:     netip.MustParseAddr("2001:db8::1"),
		Cidr:     netip.MustParsePrefix("2001:db8::/32"),
	}
	if v4.JSONSize() >= v6.JSONSize() {
		t.Errorf("IPv4 JSONSize (%d) should be < IPv6 (%d) — runtime split regressed",
			v4.JSONSize(), v6.JSONSize())
	}

	shortURL, _ := url.Parse("https://x/")
	longURL, _ := url.Parse("https://very-long-host.example.invalid:8080/some/long/path?q=value&other=val#frag")
	short := RichTypes{Site: *shortURL}
	long := RichTypes{Site: *longURL}
	if short.JSONSize() >= long.JSONSize() {
		t.Errorf("short-URL JSONSize (%d) should be < long-URL (%d) — URL component summing regressed",
			short.JSONSize(), long.JSONSize())
	}
}

// TestJSONSize_URLStruct exercises the URL component-summing path on
// a single-field struct so it can be evaluated without other field
// contributions getting in the way. Covers the dimensions of the
// emitted size code:
//   - empty URL (zero url.URL)
//   - all components populated, no user
//   - username only
//   - username + password
//   - credentials with percent-encodable chars (worst case 3× expansion)
func TestJSONSize_URLStruct(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"scheme_host_only", "https://example.com"},
		{"all_components_no_user", "https://example.com:8080/some/path?key=value&k2=v2#frag"},
		{"username_only", "https://alice@example.com/path"},
		{"user_pass", "https://alice:supersecret@example.com/path"},
		{"creds_with_percent_chars", "https://us%20er:p%40ss%21@host.example/api?token=a+b#x"},
		{"cyrillic_path_query_frag", "https://приклад.укр/шлях/розділ?запит=значення#якір"},
		{"opaque_form", "mailto:user@example.com?subject=hi"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var s URLStruct
			if c.raw != "" {
				u, err := url.Parse(c.raw)
				if err != nil {
					t.Fatalf("parse %q: %v", c.raw, err)
				}
				s.Site = *u
			}
			size := s.JSONSize()
			got, err := s.AppendJSON(make([]byte, 0, size))
			if err != nil {
				t.Fatalf("AppendJSON: %v", err)
			}
			if cap(got) != size {
				t.Errorf("realloc happened: JSONSize=%d cap=%d len=%d out=%q",
					size, cap(got), len(got), got)
			}
			if len(got) > size {
				t.Errorf("output exceeded budget: len=%d > size=%d (out=%q)",
					len(got), size, got)
			}
		})
	}
}

// JSONSize must shrink when omit-eligible fields are at their zero
// value. The all-populated baseline reserves bytes for every field's
// worst case; the zero variant must reserve strictly less, otherwise
// omit-awareness in renderSize regressed.
func TestJSONSize_OmitFieldsShrinkPrecalc(t *testing.T) {
	t.Parallel()
	zeroed := OmitStruct{Name: "alice", StrCount: 42}
	populated := OmitStruct{
		Name:     "alice",
		Bio:      "some bio text",
		Score:    3.14,
		StrCount: 42,
		Tags:     []string{"go", "json"},
		Labels:   map[string]string{"k": "v"},
		Meta:     map[string]string{"m": "n"},
		Extra:    []string{"x"},
	}
	if zeroed.JSONSize() >= populated.JSONSize() {
		t.Errorf("expected zeroed JSONSize (%d) < populated JSONSize (%d) — omit-aware sizing regressed",
			zeroed.JSONSize(), populated.JSONSize())
	}
}

func wideStructAllShort(s string) WideStruct {
	return WideStruct{
		F1: s, F2: s, F3: s, F4: s, F5: s, F6: s, F7: s, F8: s, F9: s, F10: s,
		F11: s, F12: s, F13: s, F14: s, F15: s, F16: s, F17: s, F18: s, F19: s, F20: s,
		F21: s, F22: s, F23: s, F24: s, F25: s, F26: s, F27: s, F28: s, F29: s, F30: s,
		F31: s, F32: s, F33: s, F34: s, F35: s, F36: s, F37: s, F38: s, F39: s, F40: s,
	}
}

func summarizeOverflow(budget, actual int) string {
	if actual <= budget {
		return ""
	}
	return "JSONSize underestimates worst-case input — increase the per-field budget"
}

// richTypesWorst returns a RichTypes value loaded with the heaviest
// per-kind content: a long URL with every component populated
// (including user:password credentials), big numbers wide enough to
// stress the BitLen-derived bounds, raw JSON blobs at realistic sizes,
// and a UUID literal.
func richTypesWorst() RichTypes {
	site, _ := url.Parse("https://user:supersecret@very-long-host.example.invalid:8080/a/long/path/segment?key1=val1&key2=val2&key3=val3#fragment-anchor-here")
	hugeInt, _ := new(big.Int).SetString("123456789012345678901234567890123456789012345678901234567890", 10)
	hugeRat, _ := new(big.Rat).SetString("987654321098765432109876543210/123456789012345678901234567890")
	hugeFloat, _, _ := big.ParseFloat("3.14159265358979323846264338327950288419716939937510", 10, 200, big.ToNearestEven)
	return RichTypes{
		Raw1: []byte(`{"nested":{"deep":[1,2,3,"abc","def"]}}`),
		Raw2: []byte(`[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15]`),
		Site: *site,
		Big:  *hugeInt,
		BigF: *hugeFloat,
		BigR: *hugeRat,
		ID:   uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
	}
}
