package integrationtests

//go:generate ../ggen $GOFILE

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/sirkostya009/ggen/validation"
	"github.com/sirkostya009/ggen/encode"
	"github.com/sirkostya009/ggen/scan"
)

// ExtraStruct exercises keys:/hint:/clamp/nested-dive.
//
//ggen:generate
type ExtraStruct struct {
	HintedTags   []string       `json:"hintedTags" pipe:"maxlen=1000" hint:"4"`
	ClampedScore int            `json:"clampedScore" pipe:"clamp=0|100"`
	KeyedMap     map[string]int `json:"keyedMap" pipe:"keys:(trim lower minrunes=2 maxrunes=16)"`
	NestedInts   [][]int        `json:"nestedInts" pipe:"inner:(minlen=1 inner:(gte=0 lte=100))"`
	Triple       [][][]string   `json:"triple" pipe:"inner:(minlen=1 inner:(minlen=1 inner:minlen=1))"`
}

// TupleStruct exercises fixed-length arrays [N]T as strict-count JSON tuples.
//
//ggen:generate
type TupleStruct struct {
	Point    [2]float64  `json:"point"`
	RGB      [3]int      `json:"rgb" pipe:"inner:(clamp=0|255 gte=0 lte=255)"`
	Segments [][2]int    `json:"segments"`
	Pair     [2][]string `json:"pair"`
}

// A hint-sized slice decodes correctly past the default cap.
func TestHintlen_Prealloc(t *testing.T) {
	t.Parallel()
	in := []byte(`{"hintedTags":["a","b","c","d","e","f","g","h","i","j","k","l"]}`)
	got, _, err := ExtraStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.HintedTags) != 12 {
		t.Fatalf("HintedTags len = %d, want 12", len(got.HintedTags))
	}
}

// A value below the lower bound is clamped up.
func TestClamp_Numeric_ModLowBound(t *testing.T) {
	t.Parallel()
	got, _, err := ExtraStruct{}.DecodeFrom([]byte(`{"clampedScore":-50}`))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ClampedScore != 0 {
		t.Errorf("ClampedScore = %d, want 0 (clamped low)", got.ClampedScore)
	}
}

func TestClamp_Numeric_ModHighBound(t *testing.T) {
	t.Parallel()
	got, _, err := ExtraStruct{}.DecodeFrom([]byte(`{"clampedScore":9999}`))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ClampedScore != 100 {
		t.Errorf("ClampedScore = %d, want 100 (clamped high)", got.ClampedScore)
	}
}

func TestClamp_Numeric_ModInRange(t *testing.T) {
	t.Parallel()
	got, _, err := ExtraStruct{}.DecodeFrom([]byte(`{"clampedScore":42}`))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ClampedScore != 42 {
		t.Errorf("ClampedScore = %d, want 42 (unchanged)", got.ClampedScore)
	}
}

// A short map key fails minrunes=2.
func TestKeys_ValidationOnMapKey(t *testing.T) {
	t.Parallel()
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

// trim+lower mods run on the key before insertion.
func TestKeys_ModOnMapKey(t *testing.T) {
	t.Parallel()
	in := []byte(`{"keyedMap":{"  FOO  ":7}}`)
	got, _, err := ExtraStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, ok := got.KeyedMap["foo"]; !ok || v != 7 {
		t.Errorf("KeyedMap[%q] = (%d, %v), want (7, true); full map: %+v", "foo", v, ok, got.KeyedMap)
	}
}

// [][]int decodes; per-level validation passes.
func TestNestedDive_TwoLevels_OK(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	// Empty inner slice violates inner:minlen=1.
	in := []byte(`{"nestedInts":[[]]}`)
	_, _, err := ExtraStruct{}.DecodeFrom(in)
	if err == nil {
		t.Fatal("expected minlen=1 violation on inner slice")
	}
}

func TestNestedDive_InnerViolation(t *testing.T) {
	t.Parallel()
	// Element > 100 violates lte=100 at the deepest level.
	in := []byte(`{"nestedInts":[[1,2,999]]}`)
	_, _, err := ExtraStruct{}.DecodeFrom(in)
	if err == nil {
		t.Fatal("expected lte=100 violation on inner element")
	}
}

// Three levels of slice nesting decode.
func TestTripleNested(t *testing.T) {
	t.Parallel()
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

// Stream path handles nested slices.
func TestNestedDive_Stream(t *testing.T) {
	t.Parallel()
	in := []byte(`{"nestedInts":[[1,2],[3,4,5]],"triple":[[["a"]]]}`)
	var s scan.Stream
	s.Reset(bytes.NewReader(in), make([]byte, 0, len(in)))
	got, err := ExtraStruct{}.DecodeFromStream(&s)
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

// [N]T fields decode each slot into position.
func TestTuple_Basic(t *testing.T) {
	t.Parallel()
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

// Fewer than N elements errors.
func TestTuple_StrictTooFew(t *testing.T) {
	t.Parallel()
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

// More than N elements errors.
func TestTuple_StrictTooMany(t *testing.T) {
	t.Parallel()
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

// Per-element clamp mod applies inside a tuple via inner:.
func TestTuple_Clamp(t *testing.T) {
	t.Parallel()
	in := []byte(`{"rgb":[-5,300,128]}`)
	got, _, err := TupleStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.RGB != [3]int{0, 255, 128} {
		t.Errorf("RGB = %v, want [0 255 128] after clamp", got.RGB)
	}
}

func TestTuple_MarshalRoundtrip(t *testing.T) {
	t.Parallel()
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

// Stream path handles [N]T strictness.
func TestTuple_Stream(t *testing.T) {
	t.Parallel()
	in := []byte(`{"point":[1.25,2.5],"rgb":[0,0,0],"segments":[[1,2]],"pair":[["a"],["b","c"]]}`)
	var s scan.Stream
	s.Reset(bytes.NewReader(in), make([]byte, 0, len(in)))
	got, err := TupleStruct{}.DecodeFromStream(&s)
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

// Nested slices survive Marshal + Unmarshal.
func TestNestedMarshalRoundtrip(t *testing.T) {
	t.Parallel()
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
