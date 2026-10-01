// Two-way compatibility between ggen and jsonv2. Each annotated struct must
// round-trip through both libraries, and the two decoded results must agree.

package integrationtests

//go:generate ../ggen $GOFILE

import (
	"bytes"
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

	"github.com/sirkostya009/ggen"
	"github.com/sirkostya009/ggen/integrationtests/thirdparty"
	"github.com/sirkostya009/ggen/integrationtests/thirdparty2"
)

// ggenCompat is the subset of generated methods this file needs.
type ggenCompat[T any] interface {
	ggen.Marshaler
	ggen.Decoder[T]
}

// crossCompat runs the two-way compatibility check. It compares values, not
// bytes. Each side is marshaled again through jsonv2 and parsed into `any`.
// So map order and nil versus empty differences are ignored, but a real
// difference in the encoded data is caught.
func crossCompat[T ggenCompat[T]](t *testing.T, in T) {
	t.Helper()
	// ggen marshal, then jsonv2 unmarshal.
	ggenBytes, mErr := ggen.Marshal(in)
	if mErr != nil {
		t.Fatalf("ggen Marshal for %T: %v", in, mErr)
	}
	var viaStdlib T
	if err := jsonv2.Unmarshal(ggenBytes, &viaStdlib); err != nil {
		t.Fatalf("jsonv2.Unmarshal of ggen output failed for %T: %v\n%s", in, err, ggenBytes)
	}

	// jsonv2 marshal, then ggen unmarshal.
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

// sameWire reports whether a and b give the same JSON. Both are marshaled with
// jsonv2 and parsed into `any`, so map order and nil versus empty differences
// are ignored.
func sameWire(tb testing.TB, a, b any) bool {
	tb.Helper()
	ba, err := jsonv2.Marshal(a)
	if err != nil {
		tb.Fatalf("jsonv2.Marshal(a): %v", err)
	}
	bb, err := jsonv2.Marshal(b)
	if err != nil {
		tb.Fatalf("jsonv2.Marshal(b): %v", err)
	}
	var va, vb any
	if err := jsonv2.Unmarshal(ba, &va); err != nil {
		tb.Fatalf("jsonv2.Unmarshal(a): %v", err)
	}
	if err := jsonv2.Unmarshal(bb, &vb); err != nil {
		tb.Fatalf("jsonv2.Unmarshal(b): %v", err)
	}
	return reflect.DeepEqual(va, vb)
}

// exactWire asserts that ggen's marshal output is byte-identical to jsonv2's.
// This is stronger than crossCompat, which ignores encoding differences that
// decode to the same value, such as float formatting or HTML escaping. Use it
// only for values whose field and element order already matches jsonv2's:
// single-field structs, slices, scalars and single-key maps.
func exactWire[T ggenCompat[T]](t *testing.T, name string, in T) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		t.Parallel()
		ggenBytes, err := ggen.Marshal(in)
		if err != nil {
			t.Fatalf("ggen Marshal for %T: %v", in, err)
		}
		stdBytes, err := jsonv2.Marshal(in)
		if err != nil {
			t.Fatalf("jsonv2.Marshal for %T: %v", in, err)
		}
		if !bytes.Equal(ggenBytes, stdBytes) {
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
	// The full 1 MB payload catches order and float differences that only
	// show up at scale.
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
		Name:    &name,
		Count:   &count,
		Ratio:   &ratio,
		Addr:    &Address{Street: "S", City: "C", ZipCode: "12345"},
		When:    &when,
		Enabled: &enabled,
	})
	// All fields nil, to test the null path.
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

// --- Per-format time structs, the subset jsonv2 accepts. Each has a single
// field, so TimeFormatsStdCompat and TimeFormatsStruct can embed them and get
// the field promoted. The variants jsonv2 rejects are in wire_test.go.

// TimeCustomComma has a comma inside a quoted custom layout. jsonv2 accepts
// it, and year, month and day round-trip. ggen's tag parser must respect the
// quotes when it splits options on commas. A plain comma split would cut the
// layout to "Jan 2".
//
//ggen:generate
type TimeCustomComma struct {
	CustomComma time.Time `json:"customComma,format:'Jan 2, 2006'"` //nolint:govet // layout carries spaces
}

// TimeCustomLong has a long custom layout with literal text. jsonv2 accepts
// it, and the value round-trips exactly because year, nanoseconds and zone
// are all present. It also exercises timeFormatSize's `len(format)+6`
// fallback for a long layout.
//
//ggen:generate
type TimeCustomLong struct {
	CustomLong time.Time `json:"customLong,format:'2006-Jan-02T15:04:05.000000000_Mon_-0700'"`
}

//ggen:generate
type TimeDefault struct {
	Default time.Time `json:"default"` // no format, so RFC3339Nano
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
	UnixDate time.Time `json:"unixDate,format:UnixDate"` // MST can become an offset of up to 5 chars
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
		Default:     when,
		Unix:        when,
		UnixMilli:   when,
		UnixMicro:   when,
		UnixNano:    when,
		ANSIC:       when,
		UnixDate:    when,
		RubyDate:    when,
		RFC822:      when,
		RFC822Z:     when,
		RFC850:      when,
		RFC1123:     when,
		RFC1123Z:    when,
		RFC3339:     when,
		RFC3339Nano: when,
		Kitchen:     when,
		DateTime:    when,
		DateOnly:    when,
		TimeOnly:    when,
		CustomComma: when,
		CustomLong:  when,
	}
}

// Round-trips the time formats jsonv2 accepts. The time must be in UTC so
// that RFC1123, RFC850 and UnixDate emit the literal `UTC` that jsonv2
// expects. Non-zero nanoseconds exercise the fractional part of
// `format:unix`.
func TestStdCompat_TimeFormatsStdCompat(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 5, 14, 12, 34, 56, 789000000, time.UTC)
	crossCompat(t, timeFormatsStdCompat(when))
}

func TestStdCompat_OmitStruct(t *testing.T) {
	t.Parallel()
	// Zero values on omitempty and omitzero fields.
	crossCompat(t, OmitStruct{Name: "alice", StrCount: 1})
	// All fields set.
	crossCompat(t, OmitStruct{Name: "alice", Bio: "dev", Score: 9.5, StrCount: 42, Tags: []string{"go"}})
}

func TestStdCompat_FallbackStruct(t *testing.T) {
	t.Parallel()
	crossCompat(t, FallbackStruct{ID: "abc", Extra: thirdparty.External{Key: "k", Value: 42}})
}

func TestStdCompat_ModStruct(t *testing.T) {
	t.Parallel()
	// Use a value the mods do not change. Otherwise the ggen-decoded value
	// differs on purpose.
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
	crossCompat(t, Derived{ID: "abc", Meta: "m", Name: "alice"})
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

func TestStdCompat_EmbedStruct(t *testing.T) {
	t.Parallel()
	crossCompat(t, EmbedStruct{
		Name:  "alice",
		Extra: map[string]any{"age": float64(30), "city": "Lviv", "active": true},
	})
}

type ignoredEmbedBase struct {
	Secret string `json:"secret"`
}

type keptEmbedBase struct {
	Kept string `json:"kept"`
}

//ggen:generate
type IgnoredEmbedStruct struct {
	ignoredEmbedBase `json:"-"`
	keptEmbedBase    `db:"kept"`

	Name string `json:"name"`
}

func TestStdCompat_IgnoredEmbedStruct(t *testing.T) {
	t.Parallel()
	in := IgnoredEmbedStruct{
		Secret: "s",
		Kept:   "k",
		Name:   "alice",
	}
	crossCompat(t, in)
	exactWire(t, "wire", in)
}

// richSubset is RichTypes without url.URL. ggen encodes url.URL as a JSON
// string, but jsonv2 encodes its 11-field struct. url.URL is covered
// in TestRich_Roundtrip.
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
	// A mix of set and nil elements exercises the null branch of the slab
	// path (pointees decoded into one shared backing slice).
	crossCompat(t, PtrSliceStruct{
		Items: []*Address{&a, &b},
		Tuple: [3]*Address{&a, nil, &b},
		Nodes: []*Node{{ID: 1, Name: "x"}, nil, {ID: 2, Name: "y"}},
	})
}

// SQLNullStruct is left out of the compatibility checks on purpose. ggen
// encodes sql.Null* as the inner value or null, but the standard library
// encodes the plain struct `{"Field":val,"Valid":true}`. Its round-trip is
// tested in TestSQLNull_PerType and TestSQLNull_Composite.

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

// Single-field structs for exactWire. With one field, ggen's field order and
// jsonv2's declaration order cannot disagree, so the byte compare only checks
// how the value itself is encoded.

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

// ggen's float output is byte-identical to jsonv2's across magnitude
// boundaries. crossCompat would hide formatting differences, because `1e+06`
// and `1000000` decode to the same value.
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

// ggen.AppendAny encodes the same bytes as jsonv2. HTML-special characters in
// strings, slices and maps held in an `any` are written as is, and floats in
// an `any` use the same AppendFloat. Maps have a single key so their order is
// fixed for the byte compare.
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
	// With htmlescape, <, > and & are written as \uXXXX. jsonv2 still decodes
	// them to an equal value.
	crossCompat(t, HTMLEscapeStruct{Note: `<a href="x">tom & jerry</a>`})
}

func TestStdCompat_HTMLRawStruct(t *testing.T) {
	t.Parallel()
	// By default <, > and & are written as is, like jsonv2 does.
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

// crossCompatMerge is the decode-into-receiver version of crossCompat. Both
// sides decode the payload into a receiver that already holds data, and the
// results must give the same JSON. `mk` builds a fresh receiver for each side
// so neither sees the other's changes. Use it only where ggen's merge matches
// jsonv2. The differences are in TestStdCompatMerge_IntentionalDivergences.
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
// match: slice replace, null to nil, nested struct merge, `*T` and `**T`
// reuse for a present key, array overwrite with an exact-length payload, and
// empty `[]` on a non-nil receiver. The differences are in
// TestStdCompatMerge_IntentionalDivergences.
func TestStdCompatMerge_Parity(t *testing.T) {
	t.Parallel()

	// A non-nil slice is replaced (reset and refilled), not appended to.
	crossCompatMerge(t, "slice_replace",
		func() Node { return Node{Tags: []string{"a", "b", "c"}} },
		`{"tags":["x"]}`)

	// An empty array into a non-nil slice gives an empty slice on both sides.
	crossCompatMerge(t, "slice_empty_on_nonnil",
		func() Node { return Node{Tags: []string{"a", "b"}} },
		`{"tags":[]}`)

	// JSON null sets a carried slice or map to nil.
	crossCompatMerge(t, "slice_null_to_nil",
		func() Node { return Node{Tags: []string{"a"}} },
		`{"tags":null}`)
	crossCompatMerge(t, "map_null_to_nil",
		func() Node { return Node{Props: map[string]string{"old": "1"}} },
		`{"props":null}`)

	// A nested struct merges field by field.
	crossCompatMerge(t, "nested_struct_merge",
		func() Node { return Node{Children: []Node{{ID: 7, Name: "cached"}}} },
		`{"children":[{"score":1.5}]}`)

	// Pointer `*T`: a present key decodes into the carried pointee, and null
	// drops it.
	crossCompatMerge(t, "ptr_scalar_present",
		func() PointerStruct {
			return PointerStruct{Name: new("keep"), Count: new(1)}
		},
		`{"count":9}`)
	crossCompatMerge(t, "ptr_null_drops",
		func() PointerStruct {
			return PointerStruct{Name: new("x"), Enabled: new(true)}
		},
		`{"name":null,"enabled":null}`)

	// Multi-level pointer `**int`: a present key decodes through the chain.
	crossCompatMerge(t, "multilevel_ptr",
		func() NPtrStruct { return NPtrStruct{PP: new(new(3))} },
		`{"pp":9}`)

	// An exact-length array payload overwrites every slot. A shorter payload
	// behaves differently in the two libraries.
	crossCompatMerge(t, "array_exact_len_overwrite",
		func() TupleStruct { return TupleStruct{RGB: [3]int{9, 9, 9}} },
		`{"rgb":[1,2,3]}`)
}

// The places where ggen's merge differs from jsonv2 on purpose. They follow
// from two ggen rules: containers are reset when decoding starts, and
// scalars are parsed strictly. This is the opposite of
// TestStdCompatMerge_Parity.
func TestStdCompatMerge_IntentionalDivergences(t *testing.T) {
	t.Parallel()

	// 1. Present map key: jsonv2 merges entries. ggen clears the map, then
	//    fills it.
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

	// 2. Omitted container key: ggen resets every container when decoding
	//    starts, so a slice or map whose key is absent is emptied. jsonv2
	//    leaves it untouched.
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

	// 3. Omitted key on a value or pointer field: jsonv2 leaves the
	//    receiver's value in place. ggen zeroes it, so decoding into a
	//    receiver gives what a fresh decode would give. Only allocations are
	//    reused.
	t.Run("omitted_scalar_zero_vs_retain", func(t *testing.T) {
		t.Parallel()
		const payload = `{"id":99}`
		std := Node{ID: 1, Name: "keep", Score: 2.5, Active: true}
		if err := jsonv2.Unmarshal([]byte(payload), &std); err != nil {
			t.Fatalf("jsonv2: %v", err)
		}
		if std.Name != "keep" || std.Score != 2.5 || !std.Active {
			t.Errorf("stdlib expected to retain omitted scalars, got %+v", std)
		}
		g, _, err := (Node{ID: 1, Name: "keep", Score: 2.5, Active: true}).DecodeFrom([]byte(payload))
		if err != nil {
			t.Fatalf("ggen: %v", err)
		}
		if g.ID != 99 || g.Name != "" || g.Score != 0 || g.Active {
			t.Errorf("ggen expected to zero omitted scalars, got %+v", g)
		}

		const ptrPayload = `{"count":9}`
		pstd := PointerStruct{Name: new("keep"), Count: new(1)}
		if err := jsonv2.Unmarshal([]byte(ptrPayload), &pstd); err != nil {
			// jsonv2 in Go 1.27 rejects PointerStruct's `format` tag. The
			// pointer part of this rule is tested in merge_test.go.
			t.Skipf("jsonv2 cannot merge PointerStruct: %v", err)
		}
		if pstd.Name == nil {
			t.Errorf("stdlib expected to retain the omitted pointer")
		}
		pg, _, err := (PointerStruct{Name: new("keep"), Count: new(1)}).DecodeFrom([]byte(ptrPayload))
		if err != nil {
			t.Fatalf("ggen: %v", err)
		}
		if pg.Name != nil {
			t.Errorf("ggen expected to nil the omitted pointer, got %q", *pg.Name)
		}
	})

	// 4. Explicit null on a non-pointer scalar: jsonv2 zeroes it. ggen
	//    returns an error, because only pointers, slices, maps and []byte
	//    accept null. Use a pointer to accept null.
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
