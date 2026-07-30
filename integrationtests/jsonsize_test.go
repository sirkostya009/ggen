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

// URLStruct isolates a single url.URL field for the JSONSize URL-summing path.
//
//ggen:generate
type URLStruct struct {
	Site url.URL `json:"site"`
}

// JSONSize is an upper bound: encode.Marshal preallocates exactly that many
// bytes and AppendJSON must never grow the buffer. The bound covers realistic
// worst-case input (long ASCII, short-escapes, html chars, max-width numbers,
// deep nesting); sub-0x20 control chars (\uXXXX, 6×) are deliberately not
// covered — rare, and the one-time realloc keeps the buffer tight otherwise.
func TestJSONSize_NoReallocOnWorstCase(t *testing.T) {
	t.Parallel()

	worstASCII := strings.Repeat("a", 128)        // no-escape pass-through (1×)
	worstHTML := strings.Repeat("<>&", 32)        // literal, 1× without htmlescape
	worstShort := strings.Repeat(`"\`+"\n\t", 16) // short-escapes, 2× (tightest)

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
			// omitempty/omitzero: zero-valued omit fields must not be
			// reserved in the cap.
			name: "OmitStruct_all_omits_at_zero",
			v: OmitStruct{
				Name:     "alice",
				StrCount: 42,
			},
		},
		{
			// Every format size path (time/duration/bytes) + IPv6 (the
			// wider arm of the v4/v6 split), under one cap.
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
			// IPv4 arm — the v4 path must not underestimate.
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
			// URL summing, big.Int/Float/Rat, uuid, and raw passthrough.
			name: "RichTypes_full_url_and_bignums",
			v:    richTypesWorst(),
		},
		{
			// All pointers populated — exercises the non-nil branch
			// through every pointee kind.
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
			// Cross-pkg External2's generated JSONSize must be called
			// (not a flat fallback).
			name: "FastFallbackStruct_foreign_jsonsize",
			v: FastFallbackStruct{
				ID:    worstShort,
				Extra: thirdparty2.External2{Key: worstShort, Value: math.MaxInt32},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
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

// JSONSize must shrink on the cheaper arm of a runtime split (IPv4 < IPv6,
// short URL < long URL). Pins tightness — over-allocation regressions still
// pass the worst-case cap test, so check them explicitly.
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

// URL component-summing path across empty/full/credential/percent-encoded
// shapes.
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
			t.Parallel()
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

// Per-format budget for every time format in isolation, with worst-output
// input (numeric zone fallback, max nanos).
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
			t.Parallel()
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

// JSONSize must reserve strictly less when omit-eligible fields are zeroed.
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

// richTypesWorst returns a RichTypes with the heaviest per-kind content.
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

// Cap-guard for [N]T fields.
func TestJSONSize_TupleStruct_NoRealloc(t *testing.T) {
	t.Parallel()
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

// htmlescape opt-in expands < > & to 6× (\uXXXX); the bound must cover an
// all-< > & string.
func TestJSONSize_HTMLEscapeStruct_NoRealloc(t *testing.T) {
	t.Parallel()
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

// The bound must cover the fixed field AND every spliced inline map entry.
func TestJSONSize_InlineStruct_NoRealloc(t *testing.T) {
	t.Parallel()
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

// ,string wrap adds two quotes over the bare-number budget at every width.
func TestJSONSize_StringTagStruct_NoRealloc(t *testing.T) {
	t.Parallel()
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

// populatedSQLNull builds an SQLNullStruct with every flavor Valid=true.
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

// Cap-guard for every sql.NullX flavor, both Valid=true and Valid=false
// (size = max(inner, len("null"))).
func TestJSONSize_SQLNullStruct_NoRealloc(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		v    SQLNullStruct
	}{
		{"all_null", SQLNullStruct{}},
		{"all_present", populatedSQLNull()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
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

// Per-flavor cap-guard so a single Null* regression surfaces at its flavor.
// Each row covers both arms.
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
		// Generic sql.Null[T] — same size contract as the named flavors.
		{
			"GenString",
			SQLNullGenStringStruct{},
			SQLNullGenStringStruct{S: sql.Null[string]{V: strings.Repeat("a", 100), Valid: true}},
		},
		{
			"GenInt",
			SQLNullGenIntStruct{},
			SQLNullGenIntStruct{I: sql.Null[int]{V: math.MinInt64, Valid: true}},
		},
		{
			"GenUint64",
			SQLNullGenUint64Struct{},
			SQLNullGenUint64Struct{U: sql.Null[uint64]{V: math.MaxUint64, Valid: true}},
		},
		{
			"GenFloat32",
			SQLNullGenFloat32Struct{},
			SQLNullGenFloat32Struct{F: sql.Null[float32]{V: math.MaxFloat32, Valid: true}},
		},
		{
			"GenTime",
			SQLNullGenTimeStruct{},
			SQLNullGenTimeStruct{T: sql.Null[time.Time]{V: when, Valid: true}},
		},
		// Custom inner types — named primitive (json fallback) + TextMarshaler.
		{
			"GenAccountID",
			SQLNullGenAccountStruct{},
			SQLNullGenAccountStruct{A: sql.Null[SQLAccountID]{V: math.MinInt64, Valid: true}},
		},
		{
			"GenLabel",
			SQLNullGenLabelStruct{},
			SQLNullGenLabelStruct{L: sql.Null[SQLLabel]{V: SQLLabel(strings.Repeat("a", 100)), Valid: true}},
		},
		{
			"GenUUID",
			SQLNullGenUUIDStruct{},
			SQLNullGenUUIDStruct{ID: sql.Null[uuid.UUID]{V: uuid.New(), Valid: true}},
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
		t.Run(c.name+"/null", func(t *testing.T) {
			t.Parallel()
			check(t, c.nullCase)
		})
		t.Run(c.name+"/valid", func(t *testing.T) {
			t.Parallel()
			check(t, c.validCase)
		})
	}
}

// Cap-guard for []*T slabs, with nil + non-nil elements.
func TestJSONSize_PtrSliceStruct_NoRealloc(t *testing.T) {
	t.Parallel()
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

// Per-shape cap-guard for the three slab flavors (Items / Tuple / Nodes).
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
			t.Parallel()
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

// Per-pointee-kind cap-guard for single-level *T fields, nil + populated arms.
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
			t.Parallel()
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

// Per-depth cap-guard for multi-level pointers (**int … **Address), top-nil
// + fully-allocated arms.
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
			t.Parallel()
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

// Cap-guard for an any field over a nested map+array+string mix.
func TestJSONSize_AnyStruct_NoRealloc(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// Container aliases used to return a flat 1024 — a real container past it ran
// the growth chain, breaking the single-alloc Marshal contract; they now run
// the same per-kind machinery as a struct field of that shape.
func TestJSONSize_ContainerAliases_NoRealloc(t *testing.T) {
	t.Parallel()
	check := func(name string, size int, got []byte, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if cap(got) != size {
			t.Errorf("%s realloc: JSONSize=%d cap=%d len=%d", name, size, cap(got), len(got))
		}
		if len(got) > size {
			t.Errorf("%s undersized: len=%d > size=%d", name, len(got), size)
		}
	}
	// ~6.6 KB wire — far past the old flat 1024.
	tags := make(AliasTags, 300)
	for i := range tags {
		tags[i] = "tag-value-0123456789"
	}
	size := tags.JSONSize()
	got, err := tags.AppendJSON(make([]byte, 0, size))
	check("AliasTags", size, got, err)

	lookup := AliasLookup{}
	for _, k := range []string{"alpha", "beta", "gamma", "delta"} {
		lookup[k] = 1 << 40
	}
	size = lookup.JSONSize()
	got, err = lookup.AppendJSON(make([]byte, 0, size))
	check("AliasLookup", size, got, err)

	tuple := AliasTuple{math.MaxInt, math.MinInt, 0}
	size = tuple.JSONSize()
	got, err = tuple.AppendJSON(make([]byte, 0, size))
	check("AliasTuple", size, got, err)
}

// A zoned netip.Addr was unbudgeted — the '%'+zone bytes exceeded the flat
// 39 and broke the single-alloc Marshal contract even before escaping.
func TestJSONSize_NetipZone_NoRealloc(t *testing.T) {
	t.Parallel()
	in := NativeTypes{Addr: netip.MustParseAddr("1111:2222:3333:4444:5555:6666:7777:8888%eth0")}
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
