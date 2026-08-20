// Two-way ggen ↔ jsonv2 compat: each annotated struct must round-trip through
// both, and both re-parsed results must agree.

package integrationtests

//go:generate ../ggen $GOFILE

import (
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"
	"math"
	"math/big"
	"net"
	"net/netip"
	"reflect"
	"testing"
	"time"

	gofrs "github.com/gofrs/uuid/v5"
	"github.com/google/uuid"
	"github.com/sirkostya009/ggen/decode"
	"github.com/sirkostya009/ggen/encode"
	"github.com/sirkostya009/ggen/integrationtests/thirdparty"
	"github.com/sirkostya009/ggen/integrationtests/thirdparty2"
)

// ggenCompat is the generated method subset this file needs.
type ggenCompat[T any] interface {
	encode.Marshaler
	decode.Decoder[T]
}

// crossCompat drives the two-way compat check. Equality is semantic: each
// side re-marshals through jsonv2 and parses into `any`, so map ordering and
// nil-vs-empty differences don't register but wire divergence does.
func crossCompat[T ggenCompat[T]](t *testing.T, in T) {
	// ggen marshal → jsonv2 unmarshal.
	ggenBytes, mErr := encode.Marshal(in)
	if mErr != nil {
		t.Fatalf("ggen Marshal for %T: %v", in, mErr)
	}
	var viaStdlib T
	if err := jsonv2.Unmarshal(ggenBytes, &viaStdlib); err != nil {
		t.Fatalf("jsonv2.Unmarshal of ggen output failed for %T: %v\n%s", in, err, ggenBytes)
	}

	// jsonv2 marshal → ggen unmarshal.
	stdBytes, err := jsonv2.Marshal(in)
	if err != nil {
		t.Fatalf("jsonv2.Marshal for %T: %v", in, err)
	}
	var zero T
	viaGgen, _, err := zero.DecodeFrom(stdBytes)
	if err != nil {
		t.Fatalf("ggen Unmarshal of jsonv2 output failed for %T: %v\n%s", in, err, stdBytes)
	}

	if !sameWire(t, viaStdlib, viaGgen) {
		t.Errorf("%T cross-decode mismatch\n ggen→stdlib: %+v\n stdlib→ggen: %+v\n ggen bytes:   %s\n stdlib bytes: %s",
			in, viaStdlib, viaGgen, ggenBytes, stdBytes)
	}
}

// sameWire reports whether a and b produce the same canonical JSON, both via
// jsonv2 then parsed into `any` (ignores map ordering, nil/empty differences).
func sameWire(t testing.TB, a, b any) bool {
	t.Helper()
	ba, err := jsonv2.Marshal(a)
	if err != nil {
		t.Fatalf("jsonv2.Marshal(a): %v", err)
	}
	bb, err := jsonv2.Marshal(b)
	if err != nil {
		t.Fatalf("jsonv2.Marshal(b): %v", err)
	}
	var va, vb any
	if err := jsonv2.Unmarshal(ba, &va); err != nil {
		t.Fatalf("jsonv2.Unmarshal(a): %v", err)
	}
	if err := jsonv2.Unmarshal(bb, &vb); err != nil {
		t.Fatalf("jsonv2.Unmarshal(b): %v", err)
	}
	return reflect.DeepEqual(va, vb)
}

// exactWire asserts ggen's marshal output is BYTE-IDENTICAL to jsonv2's —
// stronger than crossCompat, which normalizes away wire divergences that
// decode to the same value (float formatting, HTML escaping). Only valid for
// values whose field/element order already matches jsonv2's: single-field
// structs, slices, scalars, single-key maps.
func exactWire[T ggenCompat[T]](t *testing.T, name string, in T) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		t.Parallel()
		ggenBytes, err := encode.Marshal(in)
		if err != nil {
			t.Fatalf("ggen Marshal for %T: %v", in, err)
		}
		stdBytes, err := jsonv2.Marshal(in)
		if err != nil {
			t.Fatalf("jsonv2.Marshal for %T: %v", in, err)
		}
		if string(ggenBytes) != string(stdBytes) {
			t.Errorf("wire mismatch for %T\n ggen:   %s\n jsonv2: %s", in, ggenBytes, stdBytes)
		}
	})
}

func TestStdCompat_Address(t *testing.T) {
	t.Parallel()
	crossCompat(t, Address{Street: "Main 1", City: "Lviv", ZipCode: "79000"})
}

func TestStdCompat_Node(t *testing.T) {
	t.Parallel()
	in := Node{
		ID: 1, Name: "root", Score: 1.5, Active: true,
		Tags:  []string{"a", "b"},
		Props: map[string]string{"k": "v"},
		Children: []Node{
			{ID: 2, Name: "child", Tags: []string{"x"}, Props: map[string]string{"p": "q"}},
		},
	}
	crossCompat(t, in)
}

func TestStdCompat_Node_mega(t *testing.T) {
	t.Parallel()
	// Full 1MB payload — catches ordering/float drift that only shows at scale.
	crossCompat(t, megaValue)
}

func TestStdCompat_HookedStruct(t *testing.T) {
	t.Parallel()
	crossCompat(t, HookedStruct{Name: "alice", N: 42})
}

func TestStdCompat_MultiErrStruct(t *testing.T) {
	t.Parallel()
	crossCompat(t, MultiErrStruct{Name: "ok", Age: 30, Role: "user"})
}

func TestStdCompat_PointerStruct(t *testing.T) {
	t.Parallel()
	name := "alice"
	count := 3
	ratio := 2.5
	when := time.Unix(1700000000, 0).UTC()
	enabled := true
	crossCompat(t, PointerStruct{
		PtrNameStruct:    PtrNameStruct{Name: &name},
		PtrCountStruct:   PtrCountStruct{Count: &count},
		PtrRatioStruct:   PtrRatioStruct{Ratio: &ratio},
		PtrAddrStruct:    PtrAddrStruct{Addr: &Address{Street: "S", City: "C", ZipCode: "12345"}},
		PtrWhenStruct:    PtrWhenStruct{When: &when},
		PtrEnabledStruct: PtrEnabledStruct{Enabled: &enabled},
	})
	// All nils — the null path.
	crossCompat(t, PointerStruct{})
}

func TestStdCompat_NativeTypes(t *testing.T) {
	t.Parallel()
	addr, _ := netip.ParseAddr("192.0.2.7")
	prefix, _ := netip.ParsePrefix("10.0.0.0/24")
	crossCompat(t, NativeTypes{
		CreatedAt: time.Unix(1700000000, 123456789).UTC(),
		UnixAt:    time.Unix(1700000000, 0).UTC(),
		IssuedAt:  time.Unix(1700000000, 0).UTC(),
		SecDur:    90 * time.Second,
		UnitDur:   time.Hour + 30*time.Minute,
		Blob:      []byte("hello"),
		HexBlob:   []byte{0xde, 0xad, 0xbe, 0xef},
		ByteArray: []byte{1, 2, 3, 4},
		LegacyIP:  net.ParseIP("192.0.2.1"),
		Addr:      addr,
		Cidr:      prefix,
	})
}

// --- Per-format time structs: jsonv2-compatible subset. Each is a single
// field so TimeFormatsStdCompat / TimeFormatsStruct can embed them via
// anonymous-field promotion. jsonv2-rejected variants live in wire_test.go.

// A comma inside a quoted custom layout: jsonv2 accepts it (year+month+day
// round-trips), and ggen's tag parser must split options quote-aware — a
// naive comma split truncated this to "Jan 2".
//
//ggen:generate
type TimeCustomComma struct {
	CustomComma time.Time `json:"customComma,format:'Jan 2, 2006'"`
}

// Verbose custom layout with literals: jsonv2-accepted and value-exact on
// round-trip (year+nanos+zone all present). Also exercises timeFormatSize's
// `len(format)+6` fallback at the long end.
//
//ggen:generate
type TimeCustomLong struct {
	CustomLong time.Time `json:"customLong,format:'2006-Jan-02T15:04:05.000000000_Mon_-0700'"`
}

//ggen:generate
type TimeDefault struct {
	Default time.Time `json:"default"` // empty format → RFC3339Nano
}

//ggen:generate
type TimeUnix struct {
	Unix time.Time `json:"unix,format:unix"`
}

//ggen:generate
type TimeUnixMilli struct {
	UnixMilli time.Time `json:"unixMilli,format:unixmilli"`
}

//ggen:generate
type TimeUnixMicro struct {
	UnixMicro time.Time `json:"unixMicro,format:unixmicro"`
}

//ggen:generate
type TimeUnixNano struct {
	UnixNano time.Time `json:"unixNano,format:unixnano"`
}

//ggen:generate
type TimeANSIC struct {
	ANSIC time.Time `json:"ansic,format:ANSIC"`
}

//ggen:generate
type TimeUnixDate struct {
	UnixDate time.Time `json:"unixDate,format:UnixDate"` // MST → up to 5-char offset
}

//ggen:generate
type TimeRubyDate struct {
	RubyDate time.Time `json:"rubyDate,format:RubyDate"`
}

//ggen:generate
type TimeRFC822 struct {
	RFC822 time.Time `json:"rfc822,format:RFC822"`
}

//ggen:generate
type TimeRFC822Z struct {
	RFC822Z time.Time `json:"rfc822Z,format:RFC822Z"`
}

//ggen:generate
type TimeRFC850 struct {
	RFC850 time.Time `json:"rfc850,format:RFC850"`
}

//ggen:generate
type TimeRFC1123 struct {
	RFC1123 time.Time `json:"rfc1123,format:RFC1123"`
}

//ggen:generate
type TimeRFC1123Z struct {
	RFC1123Z time.Time `json:"rfc1123Z,format:RFC1123Z"`
}

//ggen:generate
type TimeRFC3339 struct {
	RFC3339 time.Time `json:"rfc3339,format:RFC3339"`
}

//ggen:generate
type TimeRFC3339Nano struct {
	RFC3339Nano time.Time `json:"rfc3339Nano,format:RFC3339Nano"`
}

//ggen:generate
type TimeKitchen struct {
	Kitchen time.Time `json:"kitchen,format:Kitchen"` // smallest stdlib preset
}

//ggen:generate
type TimeDateTime struct {
	DateTime time.Time `json:"dateTime,format:DateTime"`
}

//ggen:generate
type TimeDateOnly struct {
	DateOnly time.Time `json:"dateOnly,format:DateOnly"`
}

//ggen:generate
type TimeTimeOnly struct {
	TimeOnly time.Time `json:"timeOnly,format:TimeOnly"`
}

// TimeFormatsStdCompat embeds every jsonv2-compatible per-format type.
//
//ggen:generate
type TimeFormatsStdCompat struct {
	TimeDefault
	TimeUnix
	TimeUnixMilli
	TimeUnixMicro
	TimeUnixNano
	TimeANSIC
	TimeUnixDate
	TimeRubyDate
	TimeRFC822
	TimeRFC822Z
	TimeRFC850
	TimeRFC1123
	TimeRFC1123Z
	TimeRFC3339
	TimeRFC3339Nano
	TimeKitchen
	TimeDateTime
	TimeDateOnly
	TimeTimeOnly
	TimeCustomComma
	TimeCustomLong
}

// timeFormatsStdCompat builds the jsonv2-compatible subset.
func timeFormatsStdCompat(when time.Time) TimeFormatsStdCompat {
	return TimeFormatsStdCompat{
		TimeDefault:     TimeDefault{Default: when},
		TimeUnix:        TimeUnix{Unix: when},
		TimeUnixMilli:   TimeUnixMilli{UnixMilli: when},
		TimeUnixMicro:   TimeUnixMicro{UnixMicro: when},
		TimeUnixNano:    TimeUnixNano{UnixNano: when},
		TimeANSIC:       TimeANSIC{ANSIC: when},
		TimeUnixDate:    TimeUnixDate{UnixDate: when},
		TimeRubyDate:    TimeRubyDate{RubyDate: when},
		TimeRFC822:      TimeRFC822{RFC822: when},
		TimeRFC822Z:     TimeRFC822Z{RFC822Z: when},
		TimeRFC850:      TimeRFC850{RFC850: when},
		TimeRFC1123:     TimeRFC1123{RFC1123: when},
		TimeRFC1123Z:    TimeRFC1123Z{RFC1123Z: when},
		TimeRFC3339:     TimeRFC3339{RFC3339: when},
		TimeRFC3339Nano: TimeRFC3339Nano{RFC3339Nano: when},
		TimeKitchen:     TimeKitchen{Kitchen: when},
		TimeDateTime:    TimeDateTime{DateTime: when},
		TimeDateOnly:    TimeDateOnly{DateOnly: when},
		TimeTimeOnly:    TimeTimeOnly{TimeOnly: when},
		TimeCustomComma: TimeCustomComma{CustomComma: when},
		TimeCustomLong:  TimeCustomLong{CustomLong: when},
	}
}

// Round-trips the jsonv2-accepted time formats. UTC is required so
// RFC1123/RFC850/UnixDate emit the literal `UTC` token jsonv2 expects;
// non-zero nanos exercises the `format:unix` fractional decimal path.
func TestStdCompat_TimeFormatsStdCompat(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 5, 14, 12, 34, 56, 789000000, time.UTC)
	crossCompat(t, timeFormatsStdCompat(when))
}

func TestStdCompat_OmitStruct(t *testing.T) {
	t.Parallel()
	// Zero values on omitempty/omitzero fields.
	crossCompat(t, OmitStruct{Name: "alice", StrCount: 1})
	// Populated.
	crossCompat(t, OmitStruct{Name: "alice", Bio: "dev", Score: 9.5, StrCount: 42, Tags: []string{"go"}})
}

func TestStdCompat_FallbackStruct(t *testing.T) {
	t.Parallel()
	crossCompat(t, FallbackStruct{ID: "abc", Extra: thirdparty.External{Key: "k", Value: 42}})
}

func TestStdCompat_ModStruct(t *testing.T) {
	t.Parallel()
	// Start from a value mods won't change, else ggen-decoded diverges by design.
	crossCompat(t, ModStruct{Email: "a@b.com", Tags: []string{"go", "rust"}, SKU: "A1"})
}

func TestStdCompat_MapStruct(t *testing.T) {
	t.Parallel()
	crossCompat(t, MapStruct{
		Counts:    map[string]int{"a": 1, "b": 2},
		Labels:    map[string]string{"en": "hello", "es": "hola"},
		Addresses: map[string]Address{"home": {Street: "S", City: "C", ZipCode: "12345"}},
	})
}

func TestStdCompat_Derived(t *testing.T) {
	t.Parallel()
	crossCompat(t, Derived{Base: Base{ID: "abc", Meta: "m"}, Name: "alice"})
}

func TestStdCompat_DiveStruct(t *testing.T) {
	t.Parallel()
	crossCompat(t, DiveStruct{
		Tags:   []string{"go", "rust"},
		Title:  "ok",
		Scores: []int{50, 60, 70},
		Count:  4,
	})
}

func TestStdCompat_TupleStruct(t *testing.T) {
	t.Parallel()
	crossCompat(t, TupleStruct{
		Point:    [2]float64{1.5, -2.25},
		RGB:      [3]int{10, 20, 30},
		Segments: [][2]int{{1, 2}, {3, 4}},
		Pair:     [2][]string{{"x"}, {"y", "z"}},
	})
}

func TestStdCompat_ExtraStruct(t *testing.T) {
	t.Parallel()
	crossCompat(t, ExtraStruct{
		HintedTags:   []string{"x", "y"},
		ClampedScore: 42,
		KeyedMap:     map[string]int{"abc": 1},
		NestedInts:   [][]int{{1, 2}, {3, 4, 5}},
		Triple:       [][][]string{{{"a"}}, {{"b", "c"}}},
	})
}

func TestStdCompat_InlineStruct(t *testing.T) {
	t.Parallel()
	crossCompat(t, InlineStruct{
		Name:  "alice",
		Extra: map[string]any{"age": float64(30), "city": "Lviv", "active": true},
	})
}

// richSubset mirrors RichTypes minus url.URL — ggen emits url.URL as a JSON
// string but stdlib emits the 11-field struct (covered in TestRich_Roundtrip).
//
//ggen:generate
type richSubset struct {
	Raw1    json.RawMessage `json:"raw1"`
	Raw2    jsontext.Value  `json:"raw2"`
	Big     big.Int         `json:"big"`
	BigF    big.Float       `json:"bigF"`
	BigR    big.Rat         `json:"bigR"`
	ID      uuid.UUID       `json:"id"`
	GofrsID gofrs.UUID      `json:"gofrsId"`
}

func TestStdCompat_RichTypes(t *testing.T) {
	t.Parallel()
	hugeInt, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	id := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	gofrsID, _ := gofrs.FromString("550e8400-e29b-41d4-a716-446655440000")
	rat, _ := new(big.Rat).SetString("22/7")
	bigF, _, _ := big.ParseFloat("3.14159265358979323846", 10, 100, big.ToNearestEven)
	crossCompat(t, richSubset{
		Raw1:    json.RawMessage(`{"nested":42}`),
		Raw2:    jsontext.Value(`[1,2,3]`),
		Big:     *hugeInt,
		BigF:    *bigF,
		BigR:    *rat,
		ID:      id,
		GofrsID: gofrsID,
	})
}

func TestStdCompat_PtrSliceStruct(t *testing.T) {
	t.Parallel()
	a := Address{Street: "S1", City: "C1", ZipCode: "11111"}
	b := Address{Street: "S2", City: "C2", ZipCode: "22222"}
	// Mix of present + nil elements exercises the slab path's null branch.
	crossCompat(t, PtrSliceStruct{
		PtrSliceItemsStruct: PtrSliceItemsStruct{Items: []*Address{&a, &b}},
		PtrSliceTupleStruct: PtrSliceTupleStruct{Tuple: [3]*Address{&a, nil, &b}},
		PtrSliceNodesStruct: PtrSliceNodesStruct{Nodes: []*Node{{ID: 1, Name: "x"}, nil, {ID: 2, Name: "y"}}},
	})
}

// SQLNullStruct is intentionally absent from cross-compat: ggen emits sql.Null*
// as inner-value-or-null, but stdlib emits `{"Field":val,"Valid":true}` plain
// structs. Deliberate divergence; round-trip in TestSQLNull_Roundtrip.

func TestStdCompat_AnyStruct(t *testing.T) {
	t.Parallel()
	crossCompat(t, AnyStruct{
		Name: "x",
		Body: map[string]any{
			"k":   float64(1),
			"l":   []any{float64(1), float64(2), float64(3)},
			"s":   "hello",
			"b":   true,
			"nil": nil,
		},
	})
	crossCompat(t, AnyStruct{Name: "y", Body: "scalar"})
	crossCompat(t, AnyStruct{Name: "z", Body: nil})
}

// Single-field carriers for exactWire: with one field, ggen's field sort and
// jsonv2's declaration order can't disagree, so the byte compare only sees the
// value's own wire form.

//ggen:generate
type F64Wire struct {
	V float64 `json:"v"`
}

//ggen:generate
type F32Wire struct {
	V float32 `json:"v"`
}

//ggen:generate
type AnyWire struct {
	V any `json:"v"`
}

// ggen float output is byte-for-byte identical to jsonv2 across magnitude
// boundaries — crossCompat masks formatting differences (`1e+06` vs `1000000`
// decode equal).
func TestStdCompat_FloatWire(t *testing.T) {
	t.Parallel()
	for _, v := range []float64{
		0, 0.1, -2.5, 1e6, 123456789, 1e20, 1e21, 1e-6, 1e-7, 1e-9, -1e-7,
		1e100, 5e-324, math.MaxFloat64,
	} {
		exactWire(t, fmt.Sprintf("f64/%g", v), F64Wire{V: v})
	}
	for _, v := range []float32{
		0, 0.1, -2.5, 1e6, 1e7, 1e-6, 1e-7, 3.4e38,
	} {
		exactWire(t, fmt.Sprintf("f32/%g", v), F32Wire{V: v})
	}
}

// The encode.AppendAny wire shape matches jsonv2: HTML-special bytes in
// any-held strings/slices/maps emit literally, and floats through the any path
// use the same AppendFloat. Single-key maps keep iteration order deterministic
// for the byte compare.
func TestStdCompat_AnyWire(t *testing.T) {
	t.Parallel()
	cases := []any{
		`<a href="x">tom & jerry</a>`,
		[]any{"<e>", "f>g&h", "plain"},
		map[string]any{"<k>": "<v>"},
		float64(1e6),
		float64(1e-7),
		[]any{float64(1e20), float64(1e21)},
	}
	for i, body := range cases {
		exactWire(t, fmt.Sprintf("case%d", i), AnyWire{V: body})
	}
}

func TestStdCompat_TextFallbackStruct(t *testing.T) {
	t.Parallel()
	crossCompat(t, TextFallbackStruct{
		ID:  "x",
		Tag: thirdparty.Tagged{Name: "alice", Tag: "admin"},
	})
}

func TestStdCompat_FastFallbackStruct(t *testing.T) {
	t.Parallel()
	crossCompat(t, FastFallbackStruct{
		ID:    "abc",
		Extra: thirdparty2.External2{Key: "k", Value: 42},
	})
}

func TestStdCompat_HTMLEscapeStruct(t *testing.T) {
	t.Parallel()
	// htmlescape opt-in emits \uXXXX for <>&; jsonv2 still decodes back equal.
	crossCompat(t, HTMLEscapeStruct{Note: `<a href="x">tom & jerry</a>`})
}

func TestStdCompat_HTMLRawStruct(t *testing.T) {
	t.Parallel()
	// Default literal <>& is the jsonv2 wire shape.
	crossCompat(t, HTMLRawStruct{Note: `<a href="x">tom & jerry</a>`})
}

func TestStdCompat_StringTagStruct(t *testing.T) {
	t.Parallel()
	crossCompat(t, StringTagStruct{
		I8: -8, I16: 16, I32: -32, I64: 64,
		U8: 8, U16: 16, U32: 32, U64: 64,
		F32: 1.25, F64: 2.5, B: true,
	})
}

// crossCompatMerge is the decode-into-receiver counterpart of crossCompat:
// the payload is merged into a pre-populated receiver on both sides and the
// results must agree on the wire. `mk` builds a fresh receiver per side so
// neither observes the other's mutations. Use only where ggen's merge matches
// stdlib; divergences are in TestStdCompatMerge_IntentionalDivergences.
func crossCompatMerge[T ggenCompat[T]](t *testing.T, name string, mk func() T, payload string) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		t.Parallel()
		std := mk()
		if err := jsonv2.Unmarshal([]byte(payload), &std); err != nil {
			t.Fatalf("jsonv2 merge of %q into %T: %v", payload, std, err)
		}
		viaGgen, _, err := mk().DecodeFrom([]byte(payload))
		if err != nil {
			t.Fatalf("ggen merge of %q into %T: %v", payload, viaGgen, err)
		}
		if !sameWire(t, std, viaGgen) {
			sb, _ := jsonv2.Marshal(std)
			gb, _ := jsonv2.Marshal(viaGgen)
			t.Errorf("merge mismatch for %T\n payload: %s\n jsonv2 : %s\n ggen   : %s", std, payload, sb, gb)
		}
	})
}

// ggen's decode-into-receiver merge agrees with jsonv2 everywhere they should
// match: scalar persistence, slice replace, null → nil, nested-struct merge,
// `*T`/`**T` reuse, exact-length array overwrite, empty `[]` on a non-nil
// receiver. Divergences live in TestStdCompatMerge_IntentionalDivergences.
func TestStdCompatMerge_Parity(t *testing.T) {
	t.Parallel()

	// Omitted scalar keeps receiver value; present overwrites.
	crossCompatMerge(t, "scalar_persist",
		func() Node { return Node{ID: 1, Name: "keep", Score: 2.5, Active: true} },
		`{"id":99}`)

	// Non-nil slice is replaced (reset+refill), not appended.
	crossCompatMerge(t, "slice_replace",
		func() Node { return Node{Tags: []string{"a", "b", "c"}} },
		`{"tags":["x"]}`)

	// Empty array into a non-nil slice → empty slice on both sides.
	crossCompatMerge(t, "slice_empty_on_nonnil",
		func() Node { return Node{Tags: []string{"a", "b"}} },
		`{"tags":[]}`)

	// JSON null nils a carried-in slice / map.
	crossCompatMerge(t, "slice_null_to_nil",
		func() Node { return Node{Tags: []string{"a"}} },
		`{"tags":null}`)
	crossCompatMerge(t, "map_null_to_nil",
		func() Node { return Node{Props: map[string]string{"old": "1"}} },
		`{"props":null}`)

	// Nested struct merges field-by-field.
	crossCompatMerge(t, "nested_struct_merge",
		func() Node { return Node{Children: []Node{{ID: 7, Name: "cached"}}} },
		`{"children":[{"score":1.5}]}`)

	// Pointer `*T`: omitted keeps pointee, present replaces, null drops.
	crossCompatMerge(t, "ptr_scalar_persist",
		func() PointerStruct {
			return PointerStruct{PtrNameStruct: PtrNameStruct{Name: new("keep")}, PtrCountStruct: PtrCountStruct{Count: new(1)}}
		},
		`{"count":9}`)
	crossCompatMerge(t, "ptr_null_drops",
		func() PointerStruct {
			return PointerStruct{PtrNameStruct: PtrNameStruct{Name: new("x")}, PtrEnabledStruct: PtrEnabledStruct{Enabled: new(true)}}
		},
		`{"name":null,"enabled":null}`)

	// Multi-level pointer `**int`: present key resolves the chain.
	crossCompatMerge(t, "multilevel_ptr",
		func() NPtrStruct { return NPtrStruct{PtrPPStruct: PtrPPStruct{PP: new(new(3))}} },
		`{"pp":9}`)

	// Exact-length array payload: every slot overwritten (short-payload diverges).
	crossCompatMerge(t, "array_exact_len_overwrite",
		func() TupleStruct { return TupleStruct{RGB: [3]int{9, 9, 9}} },
		`{"rgb":[1,2,3]}`)
}

// Where ggen deliberately differs from jsonv2 merge — consequences of ggen's
// container reset-at-entry and strict scalar parsing. Inverse of
// TestStdCompatMerge_Parity.
func TestStdCompatMerge_IntentionalDivergences(t *testing.T) {
	t.Parallel()

	// 1. Present map key: stdlib merges entries; ggen clear()s then refills.
	t.Run("map_present_key_replace_vs_merge", func(t *testing.T) {
		t.Parallel()
		const payload = `{"props":{"new":"3"}}`
		std := Node{Props: map[string]string{"old": "1", "keep": "2"}}
		if err := jsonv2.Unmarshal([]byte(payload), &std); err != nil {
			t.Fatalf("jsonv2: %v", err)
		}
		if len(std.Props) != 3 || std.Props["old"] != "1" {
			t.Errorf("stdlib expected to retain receiver keys (merge), got %v", std.Props)
		}
		g, _, err := (Node{Props: map[string]string{"old": "1", "keep": "2"}}).DecodeFrom([]byte(payload))
		if err != nil {
			t.Fatalf("ggen: %v", err)
		}
		if len(g.Props) != 1 || g.Props["new"] != "3" {
			t.Errorf("ggen expected to replace the map (clear+refill), got %v", g.Props)
		}
		if _, ok := g.Props["old"]; ok {
			t.Errorf("ggen retained receiver-only key 'old' — container-replace contract regressed")
		}
	})

	// 2. Omitted container key: ggen resets every container at entry, so an
	//    absent slice/map key is emptied — stdlib leaves it untouched.
	t.Run("omitted_container_reset_vs_retain", func(t *testing.T) {
		t.Parallel()
		const payload = `{"id":5}`
		std := Node{Tags: []string{"a", "b"}, Props: map[string]string{"old": "1"}}
		if err := jsonv2.Unmarshal([]byte(payload), &std); err != nil {
			t.Fatalf("jsonv2: %v", err)
		}
		if len(std.Tags) != 2 || len(std.Props) != 1 {
			t.Errorf("stdlib expected to retain omitted containers, got tags=%v props=%v", std.Tags, std.Props)
		}
		g, _, err := (Node{Tags: []string{"a", "b"}, Props: map[string]string{"old": "1"}}).DecodeFrom([]byte(payload))
		if err != nil {
			t.Fatalf("ggen: %v", err)
		}
		if len(g.Tags) != 0 || len(g.Props) != 0 {
			t.Errorf("ggen expected to reset omitted containers (reset-at-entry contract), got tags=%v props=%v", g.Tags, g.Props)
		}
	})

	// 3. Explicit null on a non-pointer scalar: stdlib zeroes it; ggen hard-
	//    errors (only pointer/slice/map/[]byte accept null — use a pointer).
	t.Run("scalar_null_error_vs_zero", func(t *testing.T) {
		t.Parallel()
		const payload = `{"id":null}`
		std := Node{ID: 7}
		if err := jsonv2.Unmarshal([]byte(payload), &std); err != nil {
			t.Fatalf("stdlib expected to accept null on a scalar, got %v", err)
		}
		if std.ID != 0 {
			t.Errorf("stdlib expected to zero the field on null, got ID=%d", std.ID)
		}
		if _, _, err := (Node{ID: 7}).DecodeFrom([]byte(payload)); err == nil {
			t.Errorf("ggen expected to reject null on a non-pointer scalar field")
		}
	})
}
