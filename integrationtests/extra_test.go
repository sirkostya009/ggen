package integrationtests

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/sirkostya009/ggen/decode/validation"
	"github.com/sirkostya009/ggen/encode"
	"github.com/sirkostya009/ggen/scan"
)

// ExtraStruct exercises the keys:/hintlen/clamp/nested-dive features in a
// single annotated struct.
//
//ggen:generate
type ExtraStruct struct {
	// `hintlen=N` sizes the preallocation independently of any validation
	// bound. Useful when the payload is expected to land near N but maxlen
	// is much larger (or absent).
	HintedTags []string `json:"hintedTags" ggen:"hintlen=4,maxlen=1000"`
	// `clamp=lo|hi` mod rounds the value into [lo, hi] before validation.
	ClampedScore int `json:"clampedScore" mod:"clamp=0|100"`
	// `keys:` applies rules to map keys (always strings). Key mods run
	// before the key is stored, so the map key here ends up trimmed and
	// lower-cased.
	KeyedMap map[string]int `json:"keyedMap" ggen:"keys:minrunes=2,maxrunes=16" mod:"keys:trim,lower"`
	// Nested slice — arbitrary-depth dive:. Each `dive:` peels one level.
	NestedInts [][]int `json:"nestedInts" ggen:"dive:minlen=1,dive:gte=0,lte=100"`
	// Three-level nesting exercised by the recursive emitter.
	Triple [][][]string `json:"triple" ggen:"dive:minlen=1,dive:minlen=1,dive:minlen=1"`
}

// TupleStruct exercises fixed-length arrays `[N]T` treated as JSON tuples.
// Strict count: decode errors when the JSON array's element count ≠ N.
//
//ggen:generate
type TupleStruct struct {
	// Classic XY pair.
	Point [2]float64 `json:"point"`
	// RGB triple with per-component clamp mod + outer dive validation.
	RGB [3]int `json:"rgb" mod:"dive:clamp=0|255" ggen:"dive:gte=0,lte=255"`
	// Slice-of-tuple — each inner [2]int is its own tuple; outer is a
	// variable-length slice. Exercises the peel helper switching between
	// slice and array kinds at different depths.
	Segments [][2]int `json:"segments"`
	// Array-of-slice — inverse mix. Exactly 2 sub-slices, each variable.
	Pair [2][]string `json:"pair"`
}

// TestHintlen_Prealloc sizes a slice via hintlen and decodes normally. We
// can't directly observe cap() through the generated API, but we can confirm
// the field decodes correctly at capacity much larger than the default (8).
func TestHintlen_Prealloc(t *testing.T) {
	// 12 elements — default cap 8 would grow once; hintlen=4 would grow more;
	// hintlen=4 is the written hint and the test just proves decoding works.
	in := []byte(`{"hintedTags":["a","b","c","d","e","f","g","h","i","j","k","l"]}`)
	got, _, err := ExtraStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.HintedTags) != 12 {
		t.Fatalf("HintedTags len = %d, want 12", len(got.HintedTags))
	}
}

// TestClamp_Numeric_ModLowBound exercises clamp=0|100 where the incoming
// value is well below the lower bound.
func TestClamp_Numeric_ModLowBound(t *testing.T) {
	got, _, err := ExtraStruct{}.DecodeFrom([]byte(`{"clampedScore":-50}`))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ClampedScore != 0 {
		t.Errorf("ClampedScore = %d, want 0 (clamped low)", got.ClampedScore)
	}
}

func TestClamp_Numeric_ModHighBound(t *testing.T) {
	got, _, err := ExtraStruct{}.DecodeFrom([]byte(`{"clampedScore":9999}`))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ClampedScore != 100 {
		t.Errorf("ClampedScore = %d, want 100 (clamped high)", got.ClampedScore)
	}
}

func TestClamp_Numeric_ModInRange(t *testing.T) {
	got, _, err := ExtraStruct{}.DecodeFrom([]byte(`{"clampedScore":42}`))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ClampedScore != 42 {
		t.Errorf("ClampedScore = %d, want 42 (unchanged)", got.ClampedScore)
	}
}

// TestKeys_ValidationOnMapKey: key too short should fail the minrunes=2 rule.
func TestKeys_ValidationOnMapKey(t *testing.T) {
	in := []byte(`{"keyedMap":{"a":1}}`)
	_, _, err := ExtraStruct{}.DecodeFrom(in)
	if err == nil {
		t.Fatal("expected minrunes violation on map key")
	}
	var ve *validation.MinRunesError
	if !errors.As(err, &ve) {
		t.Errorf("got %v, want *validation.MinRunesError", err)
	}
}

// TestKeys_ModOnMapKey confirms trim+lower mods run on the map key before
// insertion. The incoming key `"  FOO  "` should become `foo` in the map.
func TestKeys_ModOnMapKey(t *testing.T) {
	in := []byte(`{"keyedMap":{"  FOO  ":7}}`)
	got, _, err := ExtraStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, ok := got.KeyedMap["foo"]; !ok || v != 7 {
		t.Errorf("KeyedMap[%q] = (%d, %v), want (7, true); full map: %+v", "foo", v, ok, got.KeyedMap)
	}
}

// TestNestedDive_TwoLevels checks that a [][]int decodes and per-level
// validation (outer minlen=1 via dive:, inner gte=0,lte=100) both fire.
func TestNestedDive_TwoLevels_OK(t *testing.T) {
	in := []byte(`{"nestedInts":[[1,2,3],[10,20]]}`)
	got, _, err := ExtraStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := [][]int{{1, 2, 3}, {10, 20}}
	if !reflect.DeepEqual(got.NestedInts, want) {
		t.Errorf("NestedInts = %v, want %v", got.NestedInts, want)
	}
}

func TestNestedDive_OuterViolation(t *testing.T) {
	// Inner slice empty violates `dive:minlen=1`.
	in := []byte(`{"nestedInts":[[]]}`)
	_, _, err := ExtraStruct{}.DecodeFrom(in)
	if err == nil {
		t.Fatal("expected minlen=1 violation on inner slice")
	}
}

func TestNestedDive_InnerViolation(t *testing.T) {
	// Inner element > 100 violates `dive:...,lte=100` at deepest level.
	in := []byte(`{"nestedInts":[[1,2,999]]}`)
	_, _, err := ExtraStruct{}.DecodeFrom(in)
	if err == nil {
		t.Fatal("expected lte=100 violation on inner element")
	}
}

// TestTripleNested decodes a [][][]string to prove the recursive emitter
// handles three levels of slice nesting.
func TestTripleNested(t *testing.T) {
	in := []byte(`{"triple":[[["a","b"],["c"]],[["d"]]]}`)
	got, _, err := ExtraStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := [][][]string{{{"a", "b"}, {"c"}}, {{"d"}}}
	if !reflect.DeepEqual(got.Triple, want) {
		t.Errorf("Triple = %v, want %v", got.Triple, want)
	}
}

// TestNestedDive_Stream confirms the streaming decoder handles nested slices.
// UnmarshalStream shares the same recursive emitter as the bytes path but
// threads positions through a *scan.Stream.
func TestNestedDive_Stream(t *testing.T) {
	in := []byte(`{"nestedInts":[[1,2],[3,4,5]],"triple":[[["a"]]]}`)
	var s scan.Stream
	s.Reset(bytes.NewReader(in), make([]byte, 0, len(in)))
	got, err := ExtraStruct{}.DecodeStreamFrom(&s)
	if err != nil {
		t.Fatalf("UnmarshalStream: %v", err)
	}
	if !reflect.DeepEqual(got.NestedInts, [][]int{{1, 2}, {3, 4, 5}}) {
		t.Errorf("NestedInts = %v", got.NestedInts)
	}
	if !reflect.DeepEqual(got.Triple, [][][]string{{{"a"}}}) {
		t.Errorf("Triple = %v", got.Triple)
	}
}

// TestTuple_Basic decodes a struct with fixed-length arrays ([N]T) and
// checks that each slot lands in the expected position. [2]float64, [3]int,
// [2][]string all exercised.
func TestTuple_Basic(t *testing.T) {
	in := []byte(`{"point":[1.5,2.5],"rgb":[10,20,30],"segments":[[1,2],[3,4]],"pair":[["a","b"],["c"]]}`)
	got, _, err := TupleStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Point != [2]float64{1.5, 2.5} {
		t.Errorf("Point = %v", got.Point)
	}
	if got.RGB != [3]int{10, 20, 30} {
		t.Errorf("RGB = %v", got.RGB)
	}
	if !reflect.DeepEqual(got.Segments, [][2]int{{1, 2}, {3, 4}}) {
		t.Errorf("Segments = %v", got.Segments)
	}
	if !reflect.DeepEqual(got.Pair, [2][]string{{"a", "b"}, {"c"}}) {
		t.Errorf("Pair = %v", got.Pair)
	}
}

// TestTuple_StrictTooFew asserts that a [N]T field with fewer than N JSON
// elements errors — strict tuple semantics.
func TestTuple_StrictTooFew(t *testing.T) {
	in := []byte(`{"point":[1.5]}`)
	_, _, err := TupleStruct{}.DecodeFrom(in)
	if err == nil {
		t.Fatal("expected error on short tuple")
	}
	var ve *validation.LenError
	if !errors.As(err, &ve) {
		t.Errorf("got %v, want *validation.LenError", err)
	}
}

// TestTuple_StrictTooMany: extra JSON elements also fail.
func TestTuple_StrictTooMany(t *testing.T) {
	in := []byte(`{"point":[1.5,2.5,3.5]}`)
	_, _, err := TupleStruct{}.DecodeFrom(in)
	if err == nil {
		t.Fatal("expected error on over-long tuple")
	}
	var ve *validation.LenError
	if !errors.As(err, &ve) {
		t.Errorf("got %v, want *validation.LenError", err)
	}
}

// TestTuple_Clamp confirms per-element mods (clamp) and validation apply
// inside a tuple the same way they apply inside a slice via `dive:`.
func TestTuple_Clamp(t *testing.T) {
	in := []byte(`{"rgb":[-5,300,128]}`)
	got, _, err := TupleStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.RGB != [3]int{0, 255, 128} {
		t.Errorf("RGB = %v, want [0 255 128] after clamp", got.RGB)
	}
}

// TestTuple_MarshalRoundtrip: Marshal a struct with tuples and re-unmarshal.
func TestTuple_MarshalRoundtrip(t *testing.T) {
	in := TupleStruct{
		Point:    [2]float64{3.14, 2.71},
		RGB:      [3]int{1, 2, 3},
		Segments: [][2]int{{10, 20}, {30, 40}},
		Pair:     [2][]string{{"x", "y", "z"}, {"q"}},
	}
	bs, _ := encode.Marshal(in)
	back, _, err := TupleStruct{}.DecodeFrom(bs)
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, bs)
	}
	if !reflect.DeepEqual(in, back) {
		t.Errorf("roundtrip mismatch\n in:  %+v\n out: %+v\n wire: %s", in, back, bs)
	}
}

// TestTuple_Stream verifies the streaming path handles [N]T strictness too.
func TestTuple_Stream(t *testing.T) {
	in := []byte(`{"point":[1.25,2.5],"rgb":[0,0,0],"segments":[[1,2]],"pair":[["a"],["b","c"]]}`)
	var s scan.Stream
	s.Reset(bytes.NewReader(in), make([]byte, 0, len(in)))
	got, err := TupleStruct{}.DecodeStreamFrom(&s)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got.Point != [2]float64{1.25, 2.5} {
		t.Errorf("Point = %v", got.Point)
	}
	if !reflect.DeepEqual(got.Pair, [2][]string{{"a"}, {"b", "c"}}) {
		t.Errorf("Pair = %v", got.Pair)
	}
}

// TestNestedMarshalRoundtrip confirms nested slices also marshal. Go through
// Marshal + Unmarshal and require DeepEqual back.
func TestNestedMarshalRoundtrip(t *testing.T) {
	in := ExtraStruct{
		HintedTags:   []string{"x"},
		ClampedScore: 50,
		KeyedMap:     map[string]int{"abc": 3},
		NestedInts:   [][]int{{1, 2}, {3}},
		Triple:       [][][]string{{{"a"}}, {{"b"}, {"c"}}},
	}
	bs, _ := encode.Marshal(in)
	back, _, err := ExtraStruct{}.DecodeFrom(bs)
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, bs)
	}
	if !reflect.DeepEqual(in.NestedInts, back.NestedInts) {
		t.Errorf("NestedInts roundtrip: got %v, want %v", back.NestedInts, in.NestedInts)
	}
	if !reflect.DeepEqual(in.Triple, back.Triple) {
		t.Errorf("Triple roundtrip: got %v, want %v", back.Triple, in.Triple)
	}
}

// TestJSONSize_TupleStruct lives in jsonsize_test.go.
