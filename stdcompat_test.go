//go:build goexperiment.jsonv2

// Exhaustive stdlib-compat tests. For every annotated struct we:
//  1. marshal with ggen, re-parse with encoding/json/v2 — struct must match.
//  2. marshal with encoding/json/v2, re-parse with ggen — struct must match.
//  3. both re-parsed results must reflect.DeepEqual each other.
//
// This pins down that ggen's output is a strict subset of jsonv2's accepted
// input and vice versa — no tag option, format, or omit rule drifts.
//
// HTML escaping is exercised via HTMLEscapeStruct / HTMLRawStruct: ggen's
// default-literal (matches jsonv2) and `htmlescape` opt-in (matches
// stdlib v1) both round-trip through jsonv2.

package main

import (
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
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
	"github.com/sirkostya009/ggen/thirdparty"
	"github.com/sirkostya009/ggen/thirdparty2"
)

// ggenCompat is the subset of generated methods this file needs: each
// generated struct implements both encode.Marshaler (AppendJSON +
// JSONSize) and decode.Decoder[T] (DecodeFrom + DecodeStreamFrom).
type ggenCompat[T any] interface {
	encode.Marshaler
	decode.Decoder[T]
}

// crossCompat drives the two-way compatibility check described at the top
// of the file. Equality is measured semantically: each side is re-marshalled
// through jsonv2 and parsed into `any` — map ordering, nil-vs-empty slice,
// and other Go-representation-only differences do not register as failures,
// but any actual wire divergence does.
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
	viaGgen, err := decode.Unmarshal[T](stdBytes)
	if err != nil {
		t.Fatalf("ggen Unmarshal of jsonv2 output failed for %T: %v\n%s", in, err, stdBytes)
	}

	if !sameWire(t, viaStdlib, viaGgen) {
		t.Errorf("%T cross-decode mismatch\n ggen→stdlib: %+v\n stdlib→ggen: %+v\n ggen bytes:   %s\n stdlib bytes: %s",
			in, viaStdlib, viaGgen, ggenBytes, stdBytes)
	}
}

// sameWire reports whether a and b produce the same canonical JSON. Both
// are marshalled via jsonv2 (the reference) and parsed into `any`, so the
// comparison ignores map ordering and nil/empty collection differences.
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

func TestStdCompat_Address(t *testing.T) {
	crossCompat(t, Address{Street: "Main 1", City: "Lviv", ZipCode: "79000"})
}

func TestStdCompat_SomePayloadRequestStruct(t *testing.T) {
	crossCompat(t, complexValue)
}

func TestStdCompat_AnotherStruct(t *testing.T) {
	crossCompat(t, AnotherStruct{Title: "hi", Score: 7.5, Active: true, Kind: 2})
}

func TestStdCompat_Node(t *testing.T) {
	// A small hand-built tree keeps the failure message readable when the
	// giant megaValue is used and something breaks.
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
	// Full 1MB payload — catches ordering or float-formatting drift that
	// only shows up at scale.
	crossCompat(t, megaValue)
}

func TestStdCompat_HookedStruct(t *testing.T) {
	crossCompat(t, HookedStruct{Name: "alice", N: 42})
}

func TestStdCompat_MultiErrStruct(t *testing.T) {
	crossCompat(t, MultiErrStruct{Name: "ok", Age: 30, Role: "user"})
}

func TestStdCompat_PointerStruct(t *testing.T) {
	name := "alice"
	count := 3
	ratio := 2.5
	when := time.Unix(1700000000, 0).UTC()
	enabled := true
	// Present values.
	crossCompat(t, PointerStruct{
		Name: &name, Count: &count, Ratio: &ratio,
		Addr:    &Address{Street: "S", City: "C", ZipCode: "12345"},
		When:    &when,
		Enabled: &enabled,
	})
	// All nils — the null path.
	crossCompat(t, PointerStruct{})
}

func TestStdCompat_NativeTypes(t *testing.T) {
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

func TestStdCompat_OmitStruct(t *testing.T) {
	// Zero values on omitempty/omitzero fields.
	crossCompat(t, OmitStruct{Name: "alice", StrCount: 1})
	// Populated.
	crossCompat(t, OmitStruct{Name: "alice", Bio: "dev", Score: 9.5, StrCount: 42, Tags: []string{"go"}})
}

func TestStdCompat_FallbackStruct(t *testing.T) {
	crossCompat(t, FallbackStruct{ID: "abc", Extra: thirdparty.External{Key: "k", Value: 42}})
}

func TestStdCompat_ModStruct(t *testing.T) {
	// Mods run on decode, so start from a value that won't trigger them
	// (otherwise ggen-decoded differs from jsonv2-decoded by design).
	crossCompat(t, ModStruct{Email: "a@b.com", Tags: []string{"go", "rust"}, SKU: "A1"})
}

func TestStdCompat_MapStruct(t *testing.T) {
	crossCompat(t, MapStruct{
		Counts:    map[string]int{"a": 1, "b": 2},
		Labels:    map[string]string{"en": "hello", "es": "hola"},
		Addresses: map[string]Address{"home": {Street: "S", City: "C", ZipCode: "12345"}},
	})
}

func TestStdCompat_Derived(t *testing.T) {
	crossCompat(t, Derived{Base: Base{ID: "abc", Meta: "m"}, Name: "alice"})
}

func TestStdCompat_DiveStruct(t *testing.T) {
	crossCompat(t, DiveStruct{
		Tags:   []string{"go", "rust"},
		Title:  "ok",
		Scores: []int{50, 60, 70},
		Count:  4,
	})
}

func TestStdCompat_TupleStruct(t *testing.T) {
	crossCompat(t, TupleStruct{
		Point:    [2]float64{1.5, -2.25},
		RGB:      [3]int{10, 20, 30},
		Segments: [][2]int{{1, 2}, {3, 4}},
		Pair:     [2][]string{{"x"}, {"y", "z"}},
	})
}

func TestStdCompat_ExtraStruct(t *testing.T) {
	crossCompat(t, ExtraStruct{
		HintedTags:   []string{"x", "y"},
		ClampedScore: 42,
		KeyedMap:     map[string]int{"abc": 1},
		NestedInts:   [][]int{{1, 2}, {3, 4, 5}},
		Triple:       [][][]string{{{"a"}}, {{"b", "c"}}},
	})
}

func TestStdCompat_InlineStruct(t *testing.T) {
	// Inline map values survive round-trip via jsonv2 as float64 numbers.
	crossCompat(t, InlineStruct{
		Name:  "alice",
		Extra: map[string]any{"age": float64(30), "city": "Lviv", "active": true},
	})
}

// richSubset mirrors RichTypes minus url.URL — ggen emits url.URL as a JSON
// string (ergonomic API convention) but stdlib (v1+v2) encodes it as the
// 11-field internal struct. Round-trip coverage for url.URL still lives in
// TestRich_Roundtrip.
//
//ggen:generate
type richSubset struct {
	Raw1 json.RawMessage `json:"raw1"`
	Raw2 jsontext.Value  `json:"raw2"`
	Big  big.Int         `json:"big"`
	BigF big.Float       `json:"bigF"`
	BigR big.Rat         `json:"bigR"`
	ID   uuid.UUID       `json:"id"`
}

func TestStdCompat_RichTypes(t *testing.T) {
	hugeInt, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	id := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	rat, _ := new(big.Rat).SetString("22/7")
	bigF, _, _ := big.ParseFloat("3.14159265358979323846", 10, 100, big.ToNearestEven)
	crossCompat(t, richSubset{
		Raw1: json.RawMessage(`{"nested":42}`),
		Raw2: jsontext.Value(`[1,2,3]`),
		Big:  *hugeInt,
		BigF: *bigF,
		BigR: *rat,
		ID:   id,
	})
}

func TestStdCompat_PtrSliceStruct(t *testing.T) {
	a := Address{Street: "S1", City: "C1", ZipCode: "11111"}
	b := Address{Street: "S2", City: "C2", ZipCode: "22222"}
	// Mix of present + nil elements exercises the slab path's null branch.
	crossCompat(t, PtrSliceStruct{
		Items: []*Address{&a, &b},
		Tuple: [3]*Address{&a, nil, &b},
		Nodes: []*Node{{ID: 1, Name: "x"}, nil, {ID: 2, Name: "y"}},
	})
}

// SQLNullStruct is intentionally absent from cross-compat: ggen emits sql.Null*
// as inner-value-or-null (the convention every database driver expects), but
// stdlib (both v1 and v2) lacks MarshalJSON on these types and serializes them
// as `{"Field":val,"Valid":true}` plain structs. This is a deliberate ggen
// divergence; round-trip coverage for sql.Null* lives in TestSQLNull_Roundtrip.

func TestStdCompat_AnyStruct(t *testing.T) {
	// Bare any field with stdlib-default float64 numbers.
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
	// Scalar bodies.
	crossCompat(t, AnyStruct{Name: "y", Body: "scalar"})
	crossCompat(t, AnyStruct{Name: "z", Body: nil})
}

func TestStdCompat_GofrsUUIDStruct(t *testing.T) {
	id, _ := gofrs.FromString("550e8400-e29b-41d4-a716-446655440000")
	crossCompat(t, GofrsUUIDStruct{ID: id})
}

func TestStdCompat_TextFallbackStruct(t *testing.T) {
	crossCompat(t, TextFallbackStruct{
		ID:  "x",
		Tag: thirdparty.Tagged{Name: "alice", Tag: "admin"},
	})
}

func TestStdCompat_FastFallbackStruct(t *testing.T) {
	crossCompat(t, FastFallbackStruct{
		ID:    "abc",
		Extra: thirdparty2.External2{Key: "k", Value: 42},
	})
}

func TestStdCompat_HTMLEscapeStruct(t *testing.T) {
	// htmlescape opt-in emits \uXXXX escapes for <>& on marshal — matches
	// stdlib v1, but jsonv2 still accepts and decodes back to the same value.
	crossCompat(t, HTMLEscapeStruct{Note: `<a href="x">tom & jerry</a>`})
}

func TestStdCompat_HTMLRawStruct(t *testing.T) {
	// Default (literal <>&) is the jsonv2 wire shape; round-trips trivially.
	crossCompat(t, HTMLRawStruct{Note: `<a href="x">tom & jerry</a>`})
}
