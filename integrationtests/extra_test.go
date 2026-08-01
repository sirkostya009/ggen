package integrationtests

//go:generate ../ggen $GOFILE

import (
	"bytes"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/sirkostya009/ggen/encode"
	"github.com/sirkostya009/ggen/scan"
	"github.com/sirkostya009/ggen/validation"
	"unsafe"

	"github.com/sirkostya009/ggen/decode"
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

// tupleLen pins the named-const array-length path: the AST reads `[tupleLen]`
// as 0, so the length must resolve through go/types.
const tupleLen = 2

// TupleStruct exercises fixed-length arrays [N]T as strict-count JSON tuples.
//
//ggen:generate
type TupleStruct struct {
	Point    [2]float64      `json:"point"`
	RGB      [3]int          `json:"rgb" pipe:"inner:(clamp=0|255 gte=0 lte=255)"`
	Segments [][2]int        `json:"segments"`
	Pair     [2][]string     `json:"pair"`
	Named    [tupleLen]int   `json:"named"`
	Nested   [][tupleLen]int `json:"nested"`
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

// A named-const length ([tupleLen]int) used to parse as ArrayLen 0, emitting a
// strict length-0 tuple that rejected every non-empty payload and silently
// accepted [] — marshal output couldn't round-trip its own decoder.
func TestTuple_NamedConstLen(t *testing.T) {
	t.Parallel()
	in := []byte(`{"named":[7,8],"nested":[[1,2],[3,4]]}`)
	got, _, err := TupleStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Named != [tupleLen]int{7, 8} {
		t.Errorf("Named = %v", got.Named)
	}
	if !reflect.DeepEqual(got.Nested, [][tupleLen]int{{1, 2}, {3, 4}}) {
		t.Errorf("Nested = %v", got.Nested)
	}
	// Strict count enforced with the RESOLVED length.
	var ve *validation.LenError
	_, _, errNamed := TupleStruct{}.DecodeFrom([]byte(`{"named":[7]}`))
	if !errors.As(errNamed, &ve) {
		t.Errorf("short named tuple: got %v, want *validation.LenError", errNamed)
	}
	_, _, errNested := TupleStruct{}.DecodeFrom([]byte(`{"nested":[[1]]}`))
	if !errors.As(errNested, &ve) {
		t.Errorf("short nested tuple: got %v, want *validation.LenError", errNested)
	}
	out, err := encode.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	back, _, err := TupleStruct{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("re-decode own marshal: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(back, got) {
		t.Errorf("roundtrip mismatch: %+v", back)
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

// PreallocWidths pins the WIDTH-DRIVEN default capacity: with no hint the
// generated make() uses a constant derived from the element size, and that
// constant must agree with decode.PreallocCap (the runtime spec of the ladder,
// which the emitted expression only mirrors — it cannot call it, because gc
// declines to inline into a generated DecodeFrom).
//
//ggen:generate
type PreallocWidths struct {
	Strs   []string       `json:"strs"`
	Ints   []int          `json:"ints"`
	Rows   []PreallocRow  `json:"rows"`
	Wide   []PreallocWide `json:"wide"`
	Ptrs   []*PreallocRow `json:"ptrs"`
	Nested [][]int        `json:"nested"`
	Hinted []PreallocWide `json:"hinted" hint:"3"`
	Lened  []PreallocWide `json:"lened"  pipe:"len=6"`
	Minned []PreallocWide `json:"minned" pipe:"minlen=5"`
	// maxlen is the exact upper bound: preallocated when that many elements
	// still fit a 512-byte span, ignored when they do not.
	MaxFits   []PreallocRow  `json:"maxFits"   pipe:"maxlen=8"`
	MaxTooBig []PreallocWide `json:"maxTooBig" pipe:"maxlen=8"`
}

//ggen:generate
type PreallocRow struct {
	A, B string `json:"-"`
	C    string `json:"c"`
}

//ggen:generate
type PreallocWide struct {
	Pad [40]int64 `json:"-"`
	C   string    `json:"c"`
}

func TestPrealloc_WidthDrivenCaps(t *testing.T) {
	t.Parallel()
	in := []byte(`{"strs":["a"],"ints":[1],"rows":[{"c":"x"}],"wide":[{"c":"x"}],` +
		`"ptrs":[{"c":"x"}],"nested":[[1]],"hinted":[{"c":"x"}],` +
		`"lened":[{"c":"x"},{"c":"x"},{"c":"x"},{"c":"x"},{"c":"x"},{"c":"x"}],` +
		`"minned":[{"c":"x"},{"c":"x"},{"c":"x"},{"c":"x"},{"c":"x"}],` +
		`"maxFits":[{"c":"x"}],"maxTooBig":[{"c":"x"}]}`)
	got, _, err := PreallocWidths{}.DecodeFrom(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		got  int
		size uintptr
	}{
		{"strs", cap(got.Strs), unsafe.Sizeof(*new(string))},
		{"rows", cap(got.Rows), unsafe.Sizeof(*new(PreallocRow))},
		{"wide", cap(got.Wide), unsafe.Sizeof(*new(PreallocWide))},
		{"ptrs", cap(got.Ptrs), unsafe.Sizeof(*new(*PreallocRow))},
		{"nested", cap(got.Nested), unsafe.Sizeof(*new([]int))},
	} {
		if want := decode.PreallocCap(c.size); c.got != want {
			t.Errorf("%s: cap = %d, want %d (element %d bytes)", c.name, c.got, want, c.size)
		}
		if c.got*int(c.size) > 512 && c.got > 1 {
			t.Errorf("%s: cap %d × %d bytes overshoots the 512-byte span budget", c.name, c.got, c.size)
		}
		if c.got < 1 {
			t.Errorf("%s: cap %d, want at least 1", c.name, c.got)
		}
	}
	// PreallocRow is 48 B: 8 × 48 = 384 <= 512, so the bound wins over the
	// width default (10). PreallocWide is 328 B: 8 × 328 blows the span, so the
	// width default (1) stands.
	if got, want := cap(got.MaxFits), 8; got != want {
		t.Errorf("maxlen=8 that fits a span: cap = %d, want %d", got, want)
	}
	if got, want := cap(got.MaxTooBig), decode.PreallocCap(unsafe.Sizeof(*new(PreallocWide))); got != want {
		t.Errorf("maxlen=8 that overshoots a span: cap = %d, want the width default %d", got, want)
	}
	// A numeric slice beats the width guess outright: scalar elements carry no
	// `,`, so the comma pre-count (opt #42) sizes it exactly.
	if cap(got.Ints) != len(got.Ints) {
		t.Errorf("[]int should be exact-counted: cap %d, len %d", cap(got.Ints), len(got.Ints))
	}
	// Every explicit sizing rule outranks the width guess: hint > len > minlen.
	// PreallocWide is 328 bytes, so the width ladder would say 1 for all three.
	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{`hint:"3"`, cap(got.Hinted), 3},
		{"len=6", cap(got.Lened), 6},
		{"minlen=5", cap(got.Minned), 5},
	} {
		if c.got != c.want {
			t.Errorf("%s overridden by the width default: cap = %d, want %d", c.name, c.got, c.want)
		}
	}
	// An element too wide for two in a span falls back to one, not zero.
	if n := decode.PreallocCap(unsafe.Sizeof(*new(PreallocWide))); n != 1 {
		t.Errorf("a %d-byte element should prealloc 1, got %d", unsafe.Sizeof(*new(PreallocWide)), n)
	}
}

// ElemKinds pins dedicated-kind slice ELEMENTS (time/duration/raw/[]byte/
// map/any): these were accepted at parse time but the element emitters fell
// through their kind switches — []any/[]time.Duration didn't compile, and
// the rest pre-grew a zero slot without scanning (ErrBadArray on any
// non-empty array) with a marshal loop that emitted nothing.
//
//ggen:generate
type ElemKinds struct {
	Times []time.Time       `json:"times"`
	Durs  []time.Duration   `json:"durs"`
	Raws  []json.RawMessage `json:"raws"`
	Blobs [][]byte          `json:"blobs"`
	Maps  []map[string]int  `json:"maps"`
	Anys  []any             `json:"anys"`
}

func TestElemKinds_DedicatedKindElements(t *testing.T) {
	t.Parallel()
	in := []byte(`{"anys":[1.5,"two",true,null,{"k":"v"}],"blobs":["aGVsbG8=","d29ybGQ="],"durs":["1m30s","1h0m0s"],"maps":[{"x":1,"y":2},{"z":3}],"raws":[{"a":1},[true,null],"s"],"times":["2020-01-01T00:00:00Z","2021-06-15T12:30:00Z"]}`)
	got, _, err := ElemKinds{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("bytes decode: %v", err)
	}
	// Durations use ggen's units default (stdlib int64-nanos diverges) —
	// check them directly, diff the rest against jsonv2.
	if got.Durs[0] != 90*time.Second || got.Durs[1] != time.Hour {
		t.Errorf("Durs = %v", got.Durs)
	}
	stdin := []byte(`{"anys":[1.5,"two",true,null,{"k":"v"}],"blobs":["aGVsbG8=","d29ybGQ="],"maps":[{"x":1,"y":2},{"z":3}],"raws":[{"a":1},[true,null],"s"],"times":["2020-01-01T00:00:00Z","2021-06-15T12:30:00Z"]}`)
	var want ElemKinds
	if err := json.Unmarshal(stdin, &want); err != nil {
		t.Fatalf("stdlib: %v", err)
	}
	for name, pair := range map[string][2]any{
		"times": {got.Times, want.Times},
		"raws":  {got.Raws, want.Raws},
		"blobs": {got.Blobs, want.Blobs},
		"maps":  {got.Maps, want.Maps},
		"anys":  {got.Anys, want.Anys},
	} {
		if !reflect.DeepEqual(pair[0], pair[1]) {
			t.Errorf("%s: got %v, want %v", name, pair[0], pair[1])
		}
	}
	// Stream path decodes identically through 1-byte chunks.
	var s scan.Stream
	s.Reset(&chunkReader{data: in, max: 1}, nil)
	sgot, err := ElemKinds{}.DecodeFromStream(&s)
	if err != nil {
		t.Fatalf("stream decode: %v", err)
	}
	if !reflect.DeepEqual(sgot, got) {
		t.Errorf("stream mismatch:\n bytes:  %+v\n stream: %+v", got, sgot)
	}
	// Marshal: valid JSON, within JSONSize, roundtrips through own decoder.
	size := got.JSONSize()
	out, err := got.AppendJSON(make([]byte, 0, size))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !json.Valid(out) {
		t.Fatalf("invalid JSON: %s", out)
	}
	if len(out) > size || cap(out) != size {
		t.Errorf("JSONSize=%d len=%d cap=%d", size, len(out), cap(out))
	}
	back, _, err := ElemKinds{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("re-decode: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(back, got) {
		t.Errorf("roundtrip mismatch")
	}
}

// MapVals pins dedicated-kind map VALUES: the decode value switch's default
// arm skipped the span leaving `mk` unused, and the marshal loop emitted
// `"k":` with no value — neither compiled (map[string]any included).
//
//ggen:generate
type MapVals struct {
	Times map[string]time.Time       `json:"times"`
	Durs  map[string]time.Duration   `json:"durs"`
	Raws  map[string]json.RawMessage `json:"raws"`
	Blobs map[string][]byte          `json:"blobs"`
	Ints  map[string][]int           `json:"ints"`
	Anys  map[string]any             `json:"anys"`
}

func TestMapVals_DedicatedKindValues(t *testing.T) {
	t.Parallel()
	in := []byte(`{"anys":{"a":1.5,"b":"two","c":null},"blobs":{"x":"aGVsbG8="},"durs":{"d":"1m30s"},"ints":{"i":[1,2,3]},"raws":{"r":{"nested":true}},"times":{"t":"2020-01-01T00:00:00Z"}}`)
	got, _, err := MapVals{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("bytes decode: %v", err)
	}
	if got.Durs["d"] != 90*time.Second {
		t.Errorf("Durs = %v", got.Durs)
	}
	stdin := []byte(`{"anys":{"a":1.5,"b":"two","c":null},"blobs":{"x":"aGVsbG8="},"ints":{"i":[1,2,3]},"raws":{"r":{"nested":true}},"times":{"t":"2020-01-01T00:00:00Z"}}`)
	var want MapVals
	if err := json.Unmarshal(stdin, &want); err != nil {
		t.Fatalf("stdlib: %v", err)
	}
	for name, pair := range map[string][2]any{
		"times": {got.Times, want.Times},
		"raws":  {got.Raws, want.Raws},
		"blobs": {got.Blobs, want.Blobs},
		"ints":  {got.Ints, want.Ints},
		"anys":  {got.Anys, want.Anys},
	} {
		if !reflect.DeepEqual(pair[0], pair[1]) {
			t.Errorf("%s: got %v, want %v", name, pair[0], pair[1])
		}
	}
	var s scan.Stream
	s.Reset(&chunkReader{data: in, max: 1}, nil)
	sgot, err := MapVals{}.DecodeFromStream(&s)
	if err != nil {
		t.Fatalf("stream decode: %v", err)
	}
	if !reflect.DeepEqual(sgot, got) {
		t.Errorf("stream mismatch:\n bytes:  %+v\n stream: %+v", got, sgot)
	}
	size := got.JSONSize()
	out, err := got.AppendJSON(make([]byte, 0, size))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !json.Valid(out) {
		t.Fatalf("invalid JSON: %s", out)
	}
	if len(out) > size || cap(out) != size {
		t.Errorf("JSONSize=%d len=%d cap=%d", size, len(out), cap(out))
	}
	back, _, err := MapVals{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("re-decode: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(back, got) {
		t.Errorf("roundtrip mismatch")
	}
}

// A fail-fast validation error from a NESTED decode used to pass through
// NewParseErr with its outer segments missing — "zipCode" instead of
// "addr.zipCode".
func TestNestedValidationPath_Complete(t *testing.T) {
	t.Parallel()
	_, _, err := PtrAddrStruct{}.DecodeFrom([]byte(`{"addr":{"street":"s","city":"c","zipCode":"123"}}`))
	var le *validation.LenError
	if !errors.As(err, &le) {
		t.Fatalf("got %v, want *validation.LenError", err)
	}
	if len(le.Path) != 2 || le.Path[0] != "addr" || le.Path[1] != "zipCode" {
		t.Errorf("Path = %v, want [addr zipCode]", le.Path)
	}
}

// jsonv2 quoted names: '-' is a literal "-" key (only a bare - ignores the
// field) and quotes protect commas in names.
//
//ggen:generate
type QuotedNames struct {
	Dash  string `json:"'-'"`
	Comma string `json:"'a,b'"`
}

func TestQuotedNames_roundtrip(t *testing.T) {
	t.Parallel()
	in := QuotedNames{Dash: "d", Comma: "c"}
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	want, err := jsonv2.Marshal(in)
	if err != nil {
		t.Fatalf("jsonv2 rejects what ggen accepted: %v", err)
	}
	var v1, v2 map[string]any
	if jsonv2.Unmarshal(out, &v1) != nil || jsonv2.Unmarshal(want, &v2) != nil || !reflect.DeepEqual(v1, v2) {
		t.Fatalf("wire mismatch: ggen %s, jsonv2 %s", out, want)
	}
	back, _, err := QuotedNames{}.DecodeFrom(out)
	if err != nil || back != in {
		t.Fatalf("decode: %+v %v", back, err)
	}
}
