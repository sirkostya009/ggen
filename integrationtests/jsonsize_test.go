package integrationtests

//go:generate ../ggen $GOFILE

import (
	"database/sql"
	"encoding/json"
	"math"
	"math/big"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	gofrs "github.com/gofrs/uuid/v5"
	"github.com/google/uuid"
	"github.com/sirkostya009/ggen/encode"
	"github.com/sirkostya009/ggen/integrationtests/thirdparty2"
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
				PtrNameStruct:    PtrNameStruct{Name: new(worstShort)},
				PtrCountStruct:   PtrCountStruct{Count: new(-1 << 62)},
				PtrRatioStruct:   PtrRatioStruct{Ratio: new(-1.7976931348623157e+308)},
				PtrAddrStruct:    PtrAddrStruct{Addr: &Address{Street: worstShort, City: worstShort, ZipCode: "00000"}},
				PtrWhenStruct:    PtrWhenStruct{When: new(time.Unix(math.MaxInt32, 0).UTC())},
				PtrEnabledStruct: PtrEnabledStruct{Enabled: new(false)},
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

// TestJSONSize_TimeFormats exercises the per-format JSONSize budget
// for every supported time format in isolation. Each case targets one
// of the single-field structs (TimeDefault / TimeUnix / TimeRFC3339 /
// TimeCustomTiny / …) so a regression in timeFormatSize's per-format
// byte count surfaces at the exact format, not buried inside a
// composite struct. Uses a fixed-offset unnamed zone + max nanos so
// the worst-output cases (numeric MST fallback, full-width fractional
// seconds) are pinned.
func TestJSONSize_TimeFormats(t *testing.T) {
	t.Parallel()
	noName := time.FixedZone("", -7*3600)
	when := time.Date(9999, 12, 31, 23, 59, 59, 999999999, noName)
	cases := []struct {
		name string
		v    interface {
			AppendJSON(dst []byte) ([]byte, error)
			JSONSize() int
		}
	}{
		{"default", TimeDefault{Default: when}},
		{"unix", TimeUnix{Unix: when}},
		{"unixMilli", TimeUnixMilli{UnixMilli: when}},
		{"unixMicro", TimeUnixMicro{UnixMicro: when}},
		{"unixNano", TimeUnixNano{UnixNano: when}},
		{"ANSIC", TimeANSIC{ANSIC: when}},
		{"UnixDate", TimeUnixDate{UnixDate: when}},
		{"RubyDate", TimeRubyDate{RubyDate: when}},
		{"RFC822", TimeRFC822{RFC822: when}},
		{"RFC822Z", TimeRFC822Z{RFC822Z: when}},
		{"RFC850", TimeRFC850{RFC850: when}},
		{"RFC1123", TimeRFC1123{RFC1123: when}},
		{"RFC1123Z", TimeRFC1123Z{RFC1123Z: when}},
		{"RFC3339", TimeRFC3339{RFC3339: when}},
		{"RFC3339Nano", TimeRFC3339Nano{RFC3339Nano: when}},
		{"Kitchen", TimeKitchen{Kitchen: when}},
		{"DateTime", TimeDateTime{DateTime: when}},
		{"DateOnly", TimeDateOnly{DateOnly: when}},
		{"TimeOnly", TimeTimeOnly{TimeOnly: when}},
		{"Layout", TimeLayout{Layout: when}},
		{"Stamp", TimeStamp{Stamp: when}},
		{"StampMilli", TimeStampMilli{StampMilli: when}},
		{"StampMicro", TimeStampMicro{StampMicro: when}},
		{"StampNano", TimeStampNano{StampNano: when}},
		{"CustomTiny", TimeCustomTiny{CustomTiny: when}},
		{"CustomLong", TimeCustomLong{CustomLong: when}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			size := c.v.JSONSize()
			got, err := c.v.AppendJSON(make([]byte, 0, size))
			if err != nil {
				t.Fatalf("AppendJSON: %v", err)
			}
			if cap(got) != size {
				t.Errorf("realloc happened: JSONSize=%d cap=%d len=%d out=%s",
					size, cap(got), len(got), got)
			}
			if len(got) > size {
				t.Errorf("output exceeded budget: len=%d > size=%d out=%s",
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
	gofrsID, _ := gofrs.FromString("550e8400-e29b-41d4-a716-446655440000")
	return RichTypes{
		Raw1:    []byte(`{"nested":{"deep":[1,2,3,"abc","def"]}}`),
		Raw2:    []byte(`[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15]`),
		Site:    *site,
		Big:     *hugeInt,
		BigF:    *hugeFloat,
		BigR:    *hugeRat,
		ID:      uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
		GofrsID: gofrsID,
	}
}

// TestJSONSize_TupleStruct: cap-guard for fixed-array [N]T fields. Worst
// case is the per-element max times N — verifies the array emitter folds
// the constant correctly.
func TestJSONSize_TupleStruct_NoRealloc(t *testing.T) {
	in := TupleStruct{
		Point:    [2]float64{-math.MaxFloat64, math.MaxFloat64},
		RGB:      [3]int{255, 0, 128},
		Segments: [][2]int{{math.MaxInt, math.MinInt}, {0, 0}, {7, -7}},
		Pair:     [2][]string{{"aaaa", "bbbb"}, {"cccc"}},
	}
	size := in.JSONSize()
	got, err := in.AppendJSON(make([]byte, 0, size))
	if err != nil {
		t.Fatalf("AppendJSON: %v", err)
	}
	if cap(got) != size {
		t.Errorf("realloc: JSONSize=%d cap=%d len=%d\nout=%s", size, cap(got), len(got), got)
	}
	if len(got) > size {
		t.Errorf("undersized: len=%d > size=%d", len(got), size)
	}
}

// TestJSONSize_HTMLEscapeStruct: htmlescape opt-in expands < > & to 6×
// (\uXXXX) on marshal. Worst case is a string consisting entirely of
// these chars — every byte expands 6× while the no-htmlescape budget
// only reserves 2× (short-escape). The bound must cover the htmlescape
// case when the opt-in is set.
func TestJSONSize_HTMLEscapeStruct_NoRealloc(t *testing.T) {
	in := HTMLEscapeStruct{Note: strings.Repeat("<>&", 50)}
	size := in.JSONSize()
	got, err := in.AppendJSON(make([]byte, 0, size))
	if err != nil {
		t.Fatalf("AppendJSON: %v", err)
	}
	if cap(got) != size {
		t.Errorf("htmlescape realloc: JSONSize=%d cap=%d len=%d\nout=%s", size, cap(got), len(got), got)
	}
	if len(got) > size {
		t.Errorf("htmlescape undersized: len=%d > size=%d", len(got), size)
	}
}

// TestJSONSize_InlineStruct: catch-all map (json:",inline") splices entries
// at the top level. JSONSize must cover both the fixed field AND every
// inline map entry's worst case.
func TestJSONSize_InlineStruct_NoRealloc(t *testing.T) {
	in := InlineStruct{
		Name: strings.Repeat("n", 30),
		Extra: map[string]any{
			"long":   strings.Repeat("v", 80),
			"num":    float64(2147483647),
			"escape": "\"\\\n\t",
			"nested": map[string]any{"a": float64(1), "b": "c"},
			"array":  []any{float64(1), float64(2), "abc"},
		},
	}
	size := in.JSONSize()
	got, err := in.AppendJSON(make([]byte, 0, size))
	if err != nil {
		t.Fatalf("AppendJSON: %v", err)
	}
	if cap(got) != size {
		t.Errorf("inline realloc: JSONSize=%d cap=%d len=%d\nout=%s", size, cap(got), len(got), got)
	}
}

// TestJSONSize_StringTagStruct: numeric `,string` wrap adds two `"` chars
// over the bare-number budget. Each width variant must include those.
func TestJSONSize_StringTagStruct_NoRealloc(t *testing.T) {
	in := StringTagStruct{
		I8: math.MinInt8, I16: math.MinInt16, I32: math.MinInt32, I64: math.MinInt64,
		U8: math.MaxUint8, U16: math.MaxUint16, U32: math.MaxUint32, U64: math.MaxUint64,
		F32: -math.MaxFloat32, F64: math.MaxFloat64, B: false,
	}
	size := in.JSONSize()
	got, err := in.AppendJSON(make([]byte, 0, size))
	if err != nil {
		t.Fatalf("AppendJSON: %v", err)
	}
	if cap(got) != size {
		t.Errorf(",string realloc: JSONSize=%d cap=%d len=%d\nout=%s", size, cap(got), len(got), got)
	}
	if len(got) > size {
		t.Errorf(",string undersized: len=%d > size=%d", len(got), size)
	}
}

// populatedSQLNull builds an SQLNullStruct with every flavor set to a
// non-trivial Valid=true value — used by the JSONSize cap-guard test
// for the present branch.
func populatedSQLNull() SQLNullStruct {
	return populatedSQLNullAt(time.Unix(1700000000, 0).UTC())
}

func populatedSQLNullAt(when time.Time) SQLNullStruct {
	out := SQLNullStruct{}
	out.S.String, out.S.Valid = strings.Repeat("a", 100), true
	out.I.Int64, out.I.Valid = math.MinInt64, true
	out.I32.Int32, out.I32.Valid = math.MinInt32, true
	out.I16.Int16, out.I16.Valid = math.MinInt16, true
	out.B.Byte, out.B.Valid = math.MaxUint8, true
	out.BL.Bool, out.BL.Valid = true, true
	out.F.Float64, out.F.Valid = math.MaxFloat64, true
	out.T.Time, out.T.Valid = when, true
	return out
}

// TestJSONSize_SQLNullStruct: cap-guard for every database/sql.NullX flavor.
// Both Valid=true and Valid=false branches because the size code chooses
// max(innerSize, len("null")) — both arms must absorb without realloc.
func TestJSONSize_SQLNullStruct_NoRealloc(t *testing.T) {
	cases := []struct {
		name string
		v    SQLNullStruct
	}{
		{"all_null", SQLNullStruct{}},
		{"all_present", populatedSQLNull()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			size := c.v.JSONSize()
			got, err := c.v.AppendJSON(make([]byte, 0, size))
			if err != nil {
				t.Fatalf("AppendJSON: %v", err)
			}
			if cap(got) != size {
				t.Errorf("realloc: JSONSize=%d cap=%d len=%d\nout=%s", size, cap(got), len(got), got)
			}
			if len(got) > size {
				t.Errorf("undersized: len=%d > size=%d", len(got), size)
			}
		})
	}
}

// TestJSONSize_SQLNullPerType: per-flavor cap-guard so a regression in a
// single Null* size class surfaces at that flavor instead of being buried
// inside the composite test above. Mirrors the TimeFormats per-format
// table. Each row covers both Valid=false (the "null" arm) and Valid=true
// (the inner-value arm) — the size code picks max(innerSize, len("null"))
// so both must absorb without realloc.
func TestJSONSize_SQLNullPerType_NoRealloc(t *testing.T) {
	t.Parallel()
	when := time.Unix(1700000000, 0).UTC()
	cases := []struct {
		name      string
		nullCase  encode.Marshaler
		validCase encode.Marshaler
	}{
		{
			"NullString",
			SQLNullStringStruct{},
			SQLNullStringStruct{S: sql.NullString{String: strings.Repeat("a", 100), Valid: true}},
		},
		{
			"NullInt64",
			SQLNullInt64Struct{},
			SQLNullInt64Struct{I: sql.NullInt64{Int64: math.MinInt64, Valid: true}},
		},
		{
			"NullInt32",
			SQLNullInt32Struct{},
			SQLNullInt32Struct{I32: sql.NullInt32{Int32: math.MinInt32, Valid: true}},
		},
		{
			"NullInt16",
			SQLNullInt16Struct{},
			SQLNullInt16Struct{I16: sql.NullInt16{Int16: math.MinInt16, Valid: true}},
		},
		{
			"NullByte",
			SQLNullByteStruct{},
			SQLNullByteStruct{B: sql.NullByte{Byte: math.MaxUint8, Valid: true}},
		},
		{
			"NullBool",
			SQLNullBoolStruct{},
			SQLNullBoolStruct{BL: sql.NullBool{Bool: true, Valid: true}},
		},
		{
			"NullFloat64",
			SQLNullFloat64Struct{},
			SQLNullFloat64Struct{F: sql.NullFloat64{Float64: math.MaxFloat64, Valid: true}},
		},
		{
			"NullTime",
			SQLNullTimeStruct{},
			SQLNullTimeStruct{T: sql.NullTime{Time: when, Valid: true}},
		},
	}
	check := func(t *testing.T, v encode.Marshaler) {
		t.Helper()
		size := v.JSONSize()
		got, err := v.AppendJSON(make([]byte, 0, size))
		if err != nil {
			t.Fatalf("AppendJSON: %v", err)
		}
		if cap(got) != size {
			t.Errorf("realloc: JSONSize=%d cap=%d len=%d\nout=%s", size, cap(got), len(got), got)
		}
		if len(got) > size {
			t.Errorf("undersized: len=%d > size=%d", len(got), size)
		}
	}
	for _, c := range cases {
		t.Run(c.name+"/null", func(t *testing.T) { check(t, c.nullCase) })
		t.Run(c.name+"/valid", func(t *testing.T) { check(t, c.validCase) })
	}
}

// TestJSONSize_PtrSliceStruct: cap-guard for []*T slab-allocated pointer
// slices. Mix of nil + non-nil elements exercises both branches.
func TestJSONSize_PtrSliceStruct_NoRealloc(t *testing.T) {
	a := Address{Street: "Main 1", City: "Lviv", ZipCode: "79000"}
	b := Address{Street: strings.Repeat("x", 200), City: strings.Repeat("y", 200), ZipCode: "00000"}
	in := PtrSliceStruct{
		PtrSliceItemsStruct: PtrSliceItemsStruct{Items: []*Address{&a, nil, &b}},
		PtrSliceTupleStruct: PtrSliceTupleStruct{Tuple: [3]*Address{&a, nil, &b}},
		PtrSliceNodesStruct: PtrSliceNodesStruct{Nodes: []*Node{{ID: 1, Name: strings.Repeat("z", 100)}, nil}},
	}
	size := in.JSONSize()
	got, err := in.AppendJSON(make([]byte, 0, size))
	if err != nil {
		t.Fatalf("AppendJSON: %v", err)
	}
	if cap(got) != size {
		t.Errorf("realloc: JSONSize=%d cap=%d len=%d", size, cap(got), len(got))
	}
	if len(got) > size {
		t.Errorf("undersized: len=%d > size=%d", len(got), size)
	}
}

// TestJSONSize_PtrSlicePerShape: per-shape cap-guard for the three slab
// flavors. Slice-of-pointer-struct (Items), array-of-pointer-struct
// (Tuple), and slice-of-pointer-recursive-struct (Nodes) each exercise
// a distinct emit path; a regression in one no longer hides behind the
// other two in the composite test above.
func TestJSONSize_PtrSlicePerShape_NoRealloc(t *testing.T) {
	t.Parallel()
	a := Address{Street: "Main 1", City: "Lviv", ZipCode: "79000"}
	b := Address{Street: strings.Repeat("x", 200), City: strings.Repeat("y", 200), ZipCode: "00000"}
	cases := []struct {
		name string
		v    encode.Marshaler
	}{
		{"Items_mixed", PtrSliceItemsStruct{Items: []*Address{&a, nil, &b}}},
		{"Items_nil", PtrSliceItemsStruct{}},
		{"Items_empty", PtrSliceItemsStruct{Items: []*Address{}}},
		{"Tuple_mixed", PtrSliceTupleStruct{Tuple: [3]*Address{&a, nil, &b}}},
		{"Tuple_all_nil", PtrSliceTupleStruct{}},
		{"Nodes_mixed", PtrSliceNodesStruct{Nodes: []*Node{{ID: 1, Name: strings.Repeat("z", 100)}, nil}}},
		{"Nodes_nil", PtrSliceNodesStruct{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			size := c.v.JSONSize()
			got, err := c.v.AppendJSON(make([]byte, 0, size))
			if err != nil {
				t.Fatalf("AppendJSON: %v", err)
			}
			if cap(got) != size {
				t.Errorf("realloc: JSONSize=%d cap=%d len=%d\nout=%s",
					size, cap(got), len(got), got)
			}
			if len(got) > size {
				t.Errorf("undersized: len=%d > size=%d", len(got), size)
			}
		})
	}
}

// TestJSONSize_PtrFieldPerKind: per-pointee-kind cap-guard for single-level
// `*T` fields, split out of PointerStruct so a regression in (say) the
// `*time.Time format:unix` size path surfaces at that kind rather than hiding
// inside the composite PointerStruct_all_populated case. Both the nil arm
// (4-byte `null`, or 0 for an omitempty field skipped entirely) and the
// populated worst-case arm must absorb without realloc.
func TestJSONSize_PtrFieldPerKind_NoRealloc(t *testing.T) {
	t.Parallel()
	worst := strings.Repeat(`"\`+"\n\t", 16)
	cases := []struct {
		name string
		v    encode.Marshaler
	}{
		{"string/nil", PtrNameStruct{}},
		{"string/set", PtrNameStruct{Name: new(worst)}},
		{"int/nil", PtrCountStruct{}},
		{"int/set", PtrCountStruct{Count: new(-1 << 62)}},
		{"float/nil", PtrRatioStruct{}},
		{"float/set", PtrRatioStruct{Ratio: new(-1.7976931348623157e+308)}},
		{"struct/nil", PtrAddrStruct{}},
		{"struct/set", PtrAddrStruct{Addr: &Address{Street: worst, City: worst, ZipCode: "00000"}}},
		{"time/nil", PtrWhenStruct{}},
		{"time/set", PtrWhenStruct{When: new(time.Unix(math.MaxInt32, 0).UTC())}},
		{"bool/nil", PtrEnabledStruct{}},
		{"bool/set", PtrEnabledStruct{Enabled: new(false)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			size := c.v.JSONSize()
			got, err := c.v.AppendJSON(make([]byte, 0, size))
			if err != nil {
				t.Fatalf("AppendJSON: %v", err)
			}
			if cap(got) != size {
				t.Errorf("realloc: JSONSize=%d cap=%d len=%d\nout=%s", size, cap(got), len(got), got)
			}
			if len(got) > size {
				t.Errorf("undersized: len=%d > size=%d", len(got), size)
			}
		})
	}
}

// TestJSONSize_NPtrPerDepth: per-depth cap-guard for multi-level pointers,
// split out of NPtrStruct. The flat `else if` nil-ladder budgets `null` (4) at
// any nil level and the leaf worst-case only when the whole chain is allocated;
// each depth/leaf (`**int`, `***int`, `****string`, `**Address`) must absorb
// both the top-nil arm and the fully-allocated arm without realloc.
func TestJSONSize_NPtrPerDepth_NoRealloc(t *testing.T) {
	t.Parallel()
	worst := strings.Repeat(`"\`+"\n\t", 16)
	cases := []struct {
		name string
		v    encode.Marshaler
	}{
		{"pp/nil", PtrPPStruct{}},
		{"pp/full", PtrPPStruct{PP: new(new(-1 << 62))}},
		{"ppp/nil", PtrPPPStruct{}},
		{"ppp/full", PtrPPPStruct{PPP: new(new(new(-1 << 62)))}},
		{"pppp/nil", PtrPPPPStruct{}},
		{"pppp/full", PtrPPPPStruct{PPPP: new(new(new(new(worst))))}},
		{"addr/nil", PtrAddr2Struct{}},
		{"addr/full", PtrAddr2Struct{Addr: new(&Address{Street: worst, City: worst, ZipCode: "00000"})}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			size := c.v.JSONSize()
			got, err := c.v.AppendJSON(make([]byte, 0, size))
			if err != nil {
				t.Fatalf("AppendJSON: %v", err)
			}
			if cap(got) != size {
				t.Errorf("realloc: JSONSize=%d cap=%d len=%d\nout=%s", size, cap(got), len(got), got)
			}
			if len(got) > size {
				t.Errorf("undersized: len=%d > size=%d", len(got), size)
			}
		})
	}
}

// TestJSONSize_AnyStruct: cap-guard for the `any` field — both the
// default (float64 numbers) and `usenumber` (json.Number) variants.
// Worst case is a deeply nested map+array+string mix.
func TestJSONSize_AnyStruct_NoRealloc(t *testing.T) {
	body := map[string]any{
		"k":   float64(42),
		"l":   []any{float64(1), float64(2), "abc", true, nil},
		"s":   strings.Repeat("x", 100),
		"sub": map[string]any{"a": float64(1), "b": "y"},
	}
	in := AnyStruct{Name: strings.Repeat("n", 50), Body: body}
	size := in.JSONSize()
	got, err := in.AppendJSON(make([]byte, 0, size))
	if err != nil {
		t.Fatalf("AppendJSON: %v", err)
	}
	if cap(got) != size {
		t.Errorf("realloc: JSONSize=%d cap=%d len=%d\nout=%s", size, cap(got), len(got), got)
	}
	if len(got) > size {
		t.Errorf("undersized: len=%d > size=%d", len(got), size)
	}
}

func TestJSONSize_AnyNumberStruct_NoRealloc(t *testing.T) {
	in := AnyNumberStruct{
		Name: "n",
		Body: map[string]any{"big": json.Number("9007199254740993"), "a": json.Number("1.5")},
	}
	size := in.JSONSize()
	got, err := in.AppendJSON(make([]byte, 0, size))
	if err != nil {
		t.Fatalf("AppendJSON: %v", err)
	}
	if cap(got) != size {
		t.Errorf("realloc: JSONSize=%d cap=%d len=%d\nout=%s", size, cap(got), len(got), got)
	}
}
