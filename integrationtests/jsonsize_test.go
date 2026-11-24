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

	"github.com/sirkostya009/ggen"
	"github.com/sirkostya009/ggen/integrationtests/thirdparty2"
)

// URLStruct isolates a single url.URL field for the JSONSize URL-summing path.
//
//ggen:generate
type URLStruct struct {
	Site url.URL `json:"site"`
}

// IPNetStruct, TCPAddrStruct and UDPAddrStruct hold one net value each, so
// JSONSize is that value's budget plus the fixed object bytes.
//
//ggen:generate
type IPNetStruct struct {
	V net.IPNet `json:"v"`
}

//ggen:generate
type TCPAddrStruct struct {
	V net.TCPAddr `json:"v"`
}

//ggen:generate
type UDPAddrStruct struct {
	V net.UDPAddr `json:"v"`
}

// JSONSize is an upper bound. ggen.Marshal preallocates exactly that many
// bytes, and AppendJSON must never grow the buffer. The bound covers
// realistic worst-case input: long ASCII, two-char escapes, HTML characters,
// max-width numbers and deep nesting. Control characters below 0x20 take a
// 6-byte \uXXXX escape and are left out on purpose. They are rare, and one
// realloc for them keeps the buffer tight in every other case.
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
			// omitempty and omitzero: zero-valued omitted fields must not be
			// reserved in the cap.
			name: "OmitStruct_all_omits_at_zero",
			v: OmitStruct{
				Name:     "alice",
				StrCount: 42,
			},
		},
		{
			// Every JSON-empty guard is populated: text kinds, nested
			// struct, any, pointers and [N]byte omitzero. Every guarded
			// budget must be reserved.
			name: "R10OmitEmpty_all_populated",
			v: R10OmitEmpty{
				IP:   net.ParseIP("2001:db8:85a3::8a2e:370:7334"),
				Addr: new(netip.MustParseAddr("2001:db8:85a3::8a2e:370:7334")),
				Pfx:  new(netip.MustParsePrefix("2001:db8::/32")),
				Site: &url.URL{Scheme: "https", Host: "x.io", Path: "/" + worstShort, RawQuery: worstShort},
				In:   &R10OmitInner{A: worstShort, N: -1 << 62},
				Any:  worstShort,
				PS:   new(worstShort),
				PL:   &[]int{-1 << 62, 1<<63 - 1},
				PPS:  new(new(worstShort)),
				Raw:  [16]byte{0xff, 1, 2, 3},
			},
		},
		{
			// Every format size path (time, duration, bytes) plus IPv6, the
			// wider side of the IPv4 and IPv6 split, under one cap.
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
			// The IPv4 side. Its budget must not underestimate.
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
			// All pointers populated. This exercises the non-nil branch
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
			// `=` alias fields are sized as the types they name, so a long
			// URL or a zoned IPv6 address behind an alias still fits.
			name: "SameViaAlias_target_sizes",
			v: SameViaAlias{
				TCP:   net.TCPAddr{IP: net.ParseIP("2001:db8:85a3::8a2e:370:7334"), Port: 65535, Zone: "eth0"},
				PTCP:  &net.TCPAddr{IP: net.ParseIP("fe80::1"), Port: 65535, Zone: worstShort},
				Nets:  []net.IPNet{mustCIDR("2001:db8::/32"), mustCIDR("10.0.0.0/8")},
				ByKey: map[string]net.IPNet{worstShort: mustCIDR("2001:db8::/32")},
				UDP:   net.UDPAddr{IP: net.ParseIP("2001:db8:85a3::8a2e:370:7334"), Port: 65535, Zone: "eth0"},
				Dur:   time.Duration(math.MinInt64),
				Durs:  []time.Duration{time.Duration(math.MinInt64), time.Duration(math.MaxInt64)},
				URL:   url.URL{Scheme: "https", Host: "x.io", Path: "/" + worstShort, RawQuery: worstShort},
				Addr:  netip.MustParseAddr("2001:db8:85a3::8a2e:370:7334"),
				NS:    sql.NullString{String: worstShort, Valid: true},
				Cnt:   -1 << 62,
				Words: []string{worstShort, worstHTML},
				PInt:  new(-1 << 62),
				Opt:   sql.Null[int64]{V: math.MinInt64, Valid: true},
			},
		},
		{
			// The cross-package External2's generated JSONSize must be
			// called, not a flat fallback.
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

// JSONSize must shrink on the cheaper side of a runtime split. IPv4 is
// shorter than IPv6, and a short URL is shorter than a long one. This pins
// tightness. Over-allocation still passes the worst-case cap test, so check
// it explicitly.
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

// The URL component-summing path across empty, full, credential and
// percent-encoded shapes.
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

// URLHTMLStruct is URLStruct under htmlescape, where &, < and > grow 6×.
//
//ggen:generate htmlescape
type URLHTMLStruct struct {
	Site url.URL `json:"site"`
}

// A dense run of bytes that survive URL escaping and grow under JSON or HTML
// escaping, in every component, and a Go-built scheme holding quotes.
func TestJSONSize_URLEscapedParts_NoRealloc(t *testing.T) {
	t.Parallel()
	dense := strings.Repeat("&<>", 13)
	parsed := []string{
		"mailto:a&b&c&d&e&f&g",
		"http://x/?q=<script>&r=<b>",
		"http://x/?a&b&c&d&e&f&g&h",
		"http://x/a&b&c&d&e&f&g&h&i&j&k&l&m",
		"http://x/#a&b&c&d&e&f&g&h&i&j&k&l&m",
		"http://a&b&c&d&e&f&g&h&i&j@x/",
	}
	var urls []url.URL
	for _, raw := range parsed {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		urls = append(urls, *u)
	}
	urls = append(urls,
		url.URL{Scheme: strings.Repeat(`"`, 40), Host: "h"},
		url.URL{Scheme: strings.Repeat(`\`, 40), Host: "h"},
		url.URL{Scheme: "h", Host: dense, Path: "/" + dense, RawQuery: dense, Fragment: dense},
		url.URL{Scheme: dense, Opaque: dense},
	)
	check := func(t *testing.T, size int, appendJSON func([]byte) ([]byte, error)) {
		t.Helper()
		got, err := appendJSON(make([]byte, 0, size))
		if err != nil {
			t.Fatalf("AppendJSON: %v", err)
		}
		if len(got) > size || cap(got) != size {
			t.Errorf("realloc: JSONSize=%d cap=%d len=%d out=%s", size, cap(got), len(got), got)
		}
	}
	for _, u := range urls {
		t.Run(u.String(), func(t *testing.T) {
			t.Parallel()
			plain := URLStruct{Site: u}
			check(t, plain.JSONSize(), plain.AppendJSON)
			html := URLHTMLStruct{Site: u}
			check(t, html.JSONSize(), html.AppendJSON)
		})
	}
}

// The budget of every time format in isolation, with input that gives the
// longest output: numeric zone fallback and max nanoseconds.
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

// Cap-guard for [N]T fields. A cap-guard checks that AppendJSON stays within
// the capacity JSONSize reserved.
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

// `[0]T` in a container budgets the two bytes it encodes as, per element and
// per map entry.
func TestJSONSize_ZeroTupleContainers_NoRealloc(t *testing.T) {
	t.Parallel()
	in := R10BZeroTupleContainers{
		M: map[string][0]int{"k": {}, "kk": {}},
		S: [][0]int{{}, {}},
		N: map[string][0][3]int{"k": {}},
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

// A pointer to a fixed byte array budgets the encoded pointee, not a tuple.
func TestJSONSize_PtrByteArray_NoRealloc(t *testing.T) {
	t.Parallel()
	p := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	q4 := [4]byte{9, 9, 9, 9}
	q := &q4
	h := [4]byte{0xde, 0xad, 0xbe, 0xef}
	a := [3]byte{1, 2, 3}
	o := [2]byte{7, 7}
	in := R10BPtrByteArray{P: &p, Q: &q, H: &h, A: &a, O: &o}
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

// The htmlescape opt-in expands <, > and & to 6 bytes each (\uXXXX). The
// bound must cover a string made only of them.
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

// The same 6-byte bound holds for strings two container levels down, a
// netip zone among them, and for a sql.NullString value.
func TestJSONSize_HTMLEscapeNested_NoRealloc(t *testing.T) {
	t.Parallel()
	in := htmlEscapeNested(strings.Repeat("<>&", 50))
	size := in.JSONSize()
	got, err := in.AppendJSON(make([]byte, 0, size))
	if err != nil {
		t.Fatalf("AppendJSON: %v", err)
	}
	if len(got) > size || cap(got) != size {
		t.Errorf("htmlescape realloc: JSONSize=%d cap=%d len=%d\nout=%s", size, cap(got), len(got), got)
	}
}

// A raw value grows 6× under htmlescape when it is all <, > and &.
func TestJSONSize_HTMLEscapeRaw_NoRealloc(t *testing.T) {
	t.Parallel()
	s := json.RawMessage(`"` + strings.Repeat("<>&", 50) + `"`)
	in := HTMLEscapeRaw{
		Meta:  s,
		Val:   s,
		Metas: []json.RawMessage{s},
		ByKey: map[string]json.RawMessage{"k": s},
		Any:   []any{s, htmlLabel{strings.Repeat("<>&", 50)}},
		Rest:  map[string]json.RawMessage{"extra": s},
	}
	size := in.JSONSize()
	got, err := in.AppendJSON(make([]byte, 0, size))
	if err != nil {
		t.Fatalf("AppendJSON: %v", err)
	}
	if len(got) > size || cap(got) != size {
		t.Errorf("htmlescape realloc: JSONSize=%d cap=%d len=%d\nout=%s", size, cap(got), len(got), got)
	}
}

// An `any` is sized by walking the value it holds. So the bound holds for a
// payload of any size at every position an `any` can take. That includes the
// htmlescape escape factor and a generated struct reporting its own JSONSize.
func TestJSONSize_AnyPositions_NoRealloc(t *testing.T) {
	t.Parallel()
	s := strings.Repeat("q\"b\\\n<>&é", 30)
	worst := func() any {
		return []any{
			s, -math.MaxFloat64, int64(math.MinInt64), json.Number("-1.5e300"), nil, true,
			map[string]any{s: []any{s, map[string]any{s: -math.SmallestNonzeroFloat64}}},
			time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.FixedZone("", -7*3600)),
			new(big.Int).Exp(big.NewInt(10), big.NewInt(200), nil),
			Address{Street: s, City: s, ZipCode: "12345"},
			[]string{s, s},
			map[string]int{s: math.MinInt},
			json.RawMessage(`{"k":[1,2,3]}`),
		}
	}
	cases := []struct {
		name string
		v    ggen.Marshaler
	}{
		{"field", AnyStruct{Name: s, Body: worst()}},
		{"omitempty field", R10OmitEmpty{Any: worst()}},
		{"slice element", ElemKinds{Anys: []any{worst(), worst()}}},
		{"map value", MapVals{Anys: map[string]any{"a": worst(), s: worst()}}},
		{"embed catch-all", EmbedStruct{Name: s, Extra: map[string]any{"x": worst(), "y<>": worst()}}},
		{"htmlescape field", HTMLEscapeStruct{Note: s, Payload: worst()}},
	}
	for _, tc := range cases {
		size := tc.v.JSONSize()
		got, err := tc.v.AppendJSON(make([]byte, 0, size))
		if err != nil {
			t.Fatalf("%s: AppendJSON: %v", tc.name, err)
		}
		if len(got) > size || cap(got) != size {
			t.Errorf("%s: JSONSize=%d but AppendJSON wrote %d (cap %d)", tc.name, size, len(got), cap(got))
		}
	}
}

// The bound must cover the fixed field and every spliced catch-all map entry.
func TestJSONSize_InlineStruct_NoRealloc(t *testing.T) {
	t.Parallel()
	in := EmbedStruct{
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

// A typed catch-all value is budgeted as a map value of its type, and the
// bound must cover the longest text each value writes.
func TestJSONSize_EmbedTypedValues_NoRealloc(t *testing.T) {
	t.Parallel()
	ip := net.ParseIP("1111:2222:3333:4444:5555:6666:7777:8888")
	tcp := net.TCPAddr{IP: ip, Port: 65535, Zone: strings.Repeat(`"\`, 16)}
	udp := net.UDPAddr(tcp)
	cidr := net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}
	long := time.Duration(math.MinInt64)
	lease := EmbedLease{Addr: tcp, TTL: long}
	key := strings.Repeat(`"`, 20)
	for name, v := range map[string]ggen.Marshaler{
		"IPNet":       EmbedIPNets{Extra: map[string]net.IPNet{key: cidr}},
		"*IPNet":      EmbedIPNetPtrs{Extra: map[string]*net.IPNet{key: &cidr, "nil": nil}},
		"TCPAddr":     EmbedTCPAddrs{Extra: map[string]net.TCPAddr{key: tcp}},
		"*TCPAddr":    EmbedTCPAddrPtrs{Extra: map[string]*net.TCPAddr{key: &tcp}},
		"UDPAddr":     EmbedUDPAddrs{Extra: map[string]net.UDPAddr{key: udp}},
		"*UDPAddr":    EmbedUDPAddrPtrs{Extra: map[string]*net.UDPAddr{key: &udp}},
		"Duration":    EmbedDurations{Extra: map[string]time.Duration{key: long}},
		"*Duration":   EmbedDurationPtrs{Extra: map[string]*time.Duration{key: &long}},
		"[16]byte":    EmbedByteArrays{Extra: map[string][16]byte{key: {255}}},
		"*generated":  EmbedLeasePtrs{Extra: map[string]*EmbedLease{key: &lease}},
		"[]generated": EmbedLeaseSlices{Extra: map[string][]EmbedLease{key: {lease, lease}}},
		"[]*UDPAddr":  EmbedUDPAddrPtrSlices{Extra: map[string][]*net.UDPAddr{key: {&udp, nil}}},
		"htmlescape":  EmbedFlaggedUDPAddrPtrs{Extra: map[string]*net.UDPAddr{key: {IP: ip, Port: 65535, Zone: strings.Repeat("<>&", 10)}}},
	} {
		size := v.JSONSize()
		got, err := v.AppendJSON(make([]byte, 0, size))
		if err != nil {
			t.Fatalf("%s: AppendJSON: %v", name, err)
		}
		if cap(got) != size {
			t.Errorf("%s: JSONSize=%d but AppendJSON wrote %d (cap %d)", name, size, len(got), cap(got))
		}
	}
}

// The ,string wrap adds two quotes over the bare-number budget at every width.
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

// Cap-guard for every sql.NullX flavor, with Valid both true and false. The
// size is max(inner, len("null")).
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
		nullCase  ggen.Marshaler
		validCase ggen.Marshaler
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
		// Generic sql.Null[T] has the same size contract as the named flavors.
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
		// Custom inner types: a named primitive (json fallback) and a
		// TextMarshaler.
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
	check := func(t *testing.T, v ggen.Marshaler) {
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

// Cap-guard for []*T slabs, with nil and non-nil elements. A slab is a []*T
// whose pointees share one backing array.
func TestJSONSize_PtrSliceStruct_NoRealloc(t *testing.T) {
	t.Parallel()
	a := Address{Street: "Main 1", City: "Lviv", ZipCode: "79000"}
	b := Address{Street: strings.Repeat("x", 200), City: strings.Repeat("y", 200), ZipCode: "00000"}
	in := PtrSliceStruct{
		Items: []*Address{&a, nil, &b},
		Tuple: [3]*Address{&a, nil, &b},
		Nodes: []*Node{{ID: 1, Name: strings.Repeat("z", 100)}, nil},
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

// Per-shape cap-guard for the three slab flavors: Items, Tuple and Nodes.
func TestJSONSize_PtrSlicePerShape_NoRealloc(t *testing.T) {
	t.Parallel()
	a := Address{Street: "Main 1", City: "Lviv", ZipCode: "79000"}
	b := Address{Street: strings.Repeat("x", 200), City: strings.Repeat("y", 200), ZipCode: "00000"}
	cases := []struct {
		name string
		v    ggen.Marshaler
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

// Cap-guard for pointers to containers as container elements. Each pointee is
// budgeted inside the loop over its container, with outer lengths below the
// inner ones, nil slots, and long strings off the diagonal, so a pointee loop
// indexing with the outer variable either panics or undercounts.
func TestJSONSize_PtrContainerElems_NoRealloc(t *testing.T) {
	t.Parallel()
	worst := strings.Repeat(`"\`+"\n\t", 16)
	zoned := netip.MustParseAddr("fe80::1%" + strings.Repeat("z", 40))
	addr := Address{Street: worst, City: worst, ZipCode: "00000"}
	cases := []struct {
		name string
		v    ggen.Marshaler
	}{
		{"slice_of_ptr_array/short_outer", PtrContainerElems{SA: []*[2]string{{worst, worst}}}},
		{"slice_of_ptr_array/nil_first", PtrContainerElems{SA: []*[2]string{nil, {worst, worst}}}},
		{"slice_of_ptr_array/off_diagonal", PtrContainerElems{SA: []*[2]string{{"", worst}, {worst, ""}}}},
		{"slice_of_ptr_slice", PtrContainerElems{SS: []*[]string{{worst, worst, worst}, nil}}},
		{"array_of_ptr_array", PtrContainerElems{AA: [2]*[3]netip.Addr{nil, {zoned, zoned, zoned}}}},
		{"slice_of_ptr_struct_slice", PtrContainerElems{SI: []*[]Address{{addr, addr}}}},
		{"map_of_ptr_array", PtrContainerElems{MA: map[string]*[2]string{"k": {worst, worst}, "n": nil}}},
		{"map_of_slice_of_ptr_array", PtrContainerElems{MS: map[string][]*[2]string{"k": {{worst, worst}}, "j": {nil, {worst, worst}}}}},
		{"nested_slice_of_ptr_array", PtrContainerElems{NA: [][]*[2]string{{{worst, worst}}, {nil, {worst, worst}}}}},
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

// Per-pointee-kind cap-guard for single-level *T fields, nil and populated.
func TestJSONSize_PtrFieldPerKind_NoRealloc(t *testing.T) {
	t.Parallel()
	worst := strings.Repeat(`"\`+"\n\t", 16)
	cases := []struct {
		name string
		v    ggen.Marshaler
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

// Per-depth cap-guard for multi-level pointers, from **int to **Address. It
// covers a nil top level and a fully allocated chain.
func TestJSONSize_NPtrPerDepth_NoRealloc(t *testing.T) {
	t.Parallel()
	worst := strings.Repeat(`"\`+"\n\t", 16)
	cases := []struct {
		name string
		v    ggen.Marshaler
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

// OmitPPStruct holds multi-level pointers under an omit guard, whose sizer
// sees the pointee with one level removed.
//
//ggen:generate
type OmitPPStruct struct {
	PPSl **[]string          `json:"ppsl,omitempty"`
	PPM  **map[string]string `json:"ppm,omitzero"`
	PPS  **string            `json:"pps,omitempty"`
	PPZ  **string            `json:"ppz,omitzero"`
	PPA  **Address           `json:"ppa,omitzero"`
}

func TestJSONSize_OmitMultiLevelPointer_NoRealloc(t *testing.T) {
	t.Parallel()
	worst := strings.Repeat(`"\`+"\n\t", 16)
	cases := []struct {
		name string
		v    OmitPPStruct
	}{
		{"nil", OmitPPStruct{}},
		{"inner_nil", OmitPPStruct{
			PPSl: new((*[]string)(nil)), PPM: new((*map[string]string)(nil)),
			PPS: new((*string)(nil)), PPZ: new((*string)(nil)), PPA: new((*Address)(nil)),
		}},
		{"full", OmitPPStruct{
			PPSl: new(&[]string{worst, worst}), PPM: new(&map[string]string{worst: worst}),
			PPS: new(new(worst)), PPZ: new(new(worst)),
			PPA: new(&Address{Street: worst, City: worst, ZipCode: worst}),
		}},
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
		})
	}
}

// Cap-guard for an any field over a mix of nested maps, arrays and strings.
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

// Container aliases are sized by the same per-kind rules as a struct field of
// that shape. A flat estimate would let a large container grow the buffer
// and break the single-allocation Marshal contract.
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
	// About 7 KB on the wire, well past any small flat estimate.
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

// The net.IPNet, net.TCPAddr and net.UDPAddr budgets at their widest: the
// longest IPv4 and IPv6 texts, port 65535, and zones whose every byte escapes
// at the full width of the escape mode, at every position NetAddrs has.
func TestJSONSize_NetKinds_NoRealloc(t *testing.T) {
	t.Parallel()
	v6 := net.ParseIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")
	v4 := net.IPv4(255, 255, 255, 255)
	for _, zone := range []string{"", `""""`, `\\`} {
		wide := net.TCPAddr{IP: v6, Port: 65535, Zone: zone}
		narrow := net.TCPAddr{IP: v4, Port: 65535}
		n6 := net.IPNet{IP: v6, Mask: net.CIDRMask(128, 128)}
		n4 := net.IPNet{IP: v4.To4(), Mask: net.CIDRMask(32, 32)}
		in := NetAddrs{
			Net: n6, TCP: wide, UDP: net.UDPAddr(narrow), PNet: &n4, PTCP: &wide,
			Nets: []net.IPNet{n6, n4}, PAddrs: []*net.UDPAddr{(*net.UDPAddr)(&wide), nil},
			ByName: map[string]net.TCPAddr{"a": wide, "b": narrow}, PByKey: map[string]*net.IPNet{"n": &n6},
			Tuple: [2]net.UDPAddr{net.UDPAddr(wide), net.UDPAddr(narrow)}, OTCP: wide, OPUDP: (*net.UDPAddr)(&wide),
			ONet: &n4, ZNet: n6, EUDP: (*net.UDPAddr)(&wide),
			Deep: map[string][]net.IPNet{"d": {n6}}, Wide: map[string]*net.UDPAddr{"w": (*net.UDPAddr)(&wide)},
		}
		size := in.JSONSize()
		got, err := in.AppendJSON(make([]byte, 0, size))
		if err != nil || cap(got) != size {
			t.Errorf("zone %q: realloc or error: JSONSize=%d cap=%d len=%d (%v)", zone, size, cap(got), len(got), err)
		}
	}
	h := NetAddrsHTML{TCP: net.TCPAddr{IP: v6, Port: 65535, Zone: "<>&"}}
	size := h.JSONSize()
	got, err := h.AppendJSON(make([]byte, 0, size))
	if err != nil || cap(got) != size {
		t.Errorf("htmlescape: realloc or error: JSONSize=%d cap=%d len=%d (%v)", size, cap(got), len(got), err)
	}
}

// Each net budget is exactly the length of the widest text of its kind and
// address family, so a value that writes that text has no slack to hide a
// budget one byte short.
func TestJSONSize_NetKinds_Exact(t *testing.T) {
	t.Parallel()
	v6 := net.ParseIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")
	v4 := net.IPv4(255, 255, 255, 255)
	wide := net.TCPAddr{IP: v6, Port: 65535}
	narrow := net.TCPAddr{IP: v4, Port: 65535}
	// Every zone byte escapes to two bytes, or to six under htmlescape.
	zoned := net.TCPAddr{IP: v6, Port: 65535, Zone: `"\`}
	for name, v := range map[string]ggen.Marshaler{
		"IPNet v4":        IPNetStruct{V: net.IPNet{IP: v4.To4(), Mask: net.CIDRMask(32, 32)}},
		"IPNet v6":        IPNetStruct{V: net.IPNet{IP: v6, Mask: net.CIDRMask(128, 128)}},
		"TCPAddr v4":      TCPAddrStruct{V: narrow},
		"TCPAddr v6":      TCPAddrStruct{V: wide},
		"TCPAddr zone":    TCPAddrStruct{V: zoned},
		"UDPAddr v4":      UDPAddrStruct{V: net.UDPAddr(narrow)},
		"UDPAddr v6":      UDPAddrStruct{V: net.UDPAddr(wide)},
		"UDPAddr zone":    UDPAddrStruct{V: net.UDPAddr(zoned)},
		"htmlescape zone": NetAddrsHTML{TCP: net.TCPAddr{IP: v6, Port: 65535, Zone: "<>&"}},
	} {
		out, err := v.AppendJSON(nil)
		if err != nil {
			t.Fatalf("%s: AppendJSON: %v", name, err)
		}
		if size := v.JSONSize(); size != len(out) {
			t.Errorf("%s: JSONSize=%d but AppendJSON wrote %d: %s", name, size, len(out), out)
		}
	}
}

// A zoned netip.Addr must budget its '%' and zone bytes. Without them the
// text exceeds the 39 bytes of a plain IPv6 address and breaks the
// single-allocation Marshal contract, even before escaping.
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

// TimeWideLayouts has layouts whose elements print wider than they are
// spelled: day and month names, a year past 9999, a zone offset of many hours
// and a zone name. Slice and map elements share no slack, so each element's
// budget covers its own widest output.
//
//ggen:generate
type TimeWideLayouts struct {
	RFC850   []time.Time `json:"rfc850,format:RFC850"`
	Names    []time.Time `json:"names,format:'Monday January 2 MST'"` //nolint:govet // layout carries spaces
	DateOnly []time.Time `json:"dateOnly,format:DateOnly"`
	RFC1123Z []time.Time `json:"rfc1123z,format:RFC1123Z"`
	Frac     []time.Time `json:"frac,format:'15:04:05.000000000000 Z07:00:00'"` //nolint:govet // layout carries spaces
}

// TimeHTMLLayout writes a custom layout with <, > and & under htmlescape,
// which escapes each of them to six bytes.
//
//ggen:generate htmlescape
type TimeHTMLLayout struct {
	At  time.Time   `json:"at,format:'<<<<<<<<<<<<<<<< &&&&&&&&&&&&&&&& 2006 >>>>>>>>>>>>>>>>'"` //nolint:govet // layout carries spaces
	All []time.Time `json:"all,format:'<&> 2006'"`                                               //nolint:govet // layout carries spaces
}

func TestJSONSize_TimeWideLayouts_NoRealloc(t *testing.T) {
	t.Parallel()
	whens := []time.Time{
		time.Date(2021, time.September, 22, 12, 45, 59, 0, time.FixedZone("", 5*3600+1800)),
		time.Date(-292277022000, time.January, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("", -2000000000)),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("", math.MinInt)),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone(strings.Repeat("Zone", 50), 3600)),
	}
	for _, when := range whens {
		ts := []time.Time{when, when, when}
		cases := []interface {
			AppendJSON(dst []byte) ([]byte, error)
			JSONSize() int
		}{
			TimeWideLayouts{
				RFC850: ts, Names: ts, DateOnly: ts, RFC1123Z: ts, Frac: ts,
			},
			TimeHTMLLayout{At: when, All: ts},
		}
		for _, v := range cases {
			size := v.JSONSize()
			got, err := v.AppendJSON(make([]byte, 0, size))
			if err != nil {
				t.Fatalf("AppendJSON: %v", err)
			}
			if cap(got) != size {
				t.Errorf("%T at %v: realloc, JSONSize=%d len=%d", v, when, size, len(got))
			}
		}
	}
}

// A time layout's budget is its widest output and no more: AppendFormat
// grows the buffer only by what it writes.
func TestJSONSize_TimeExact(t *testing.T) {
	t.Parallel()
	when := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.FixedZone("", -7*3600))
	v := TimeDefault{Default: when}
	out, err := v.AppendJSON(nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := v.JSONSize(); n != len(out) {
		t.Errorf("JSONSize %d, output %d: %s", n, len(out), out)
	}
}
