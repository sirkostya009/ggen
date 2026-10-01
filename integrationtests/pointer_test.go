package integrationtests

//go:generate ../ggen $GOFILE

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/sirkostya009/ggen"
)

// An error inside a pointee carries the full field path through the outer
// field ("addr.street").
func TestPointer_parseErrorChainsThroughPointee(t *testing.T) {
	t.Parallel()
	_, _, err := (PointerStruct{}).DecodeFrom([]byte(
		`{"name":"x","enabled":true,"addr":{"street":123,"city":"C","zipCode":"12345"}}`))
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *ggen.ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %T %v, want *ggen.ParseError", err, err)
	}
	if strings.Join(pe.Path, ".") != "addr.street" {
		t.Fatalf("Path = %v; want [addr street]", pe.Path)
	}
	if !errors.Is(err, ggen.ErrExpectString) {
		t.Fatalf("errors.Is sentinel mismatch: %v", err)
	}
}

// Single-level *T fields, one struct per pointee kind, so a regression points
// at the kind instead of hiding in the composite struct. Tags match the
// composite, so the per-field JSONSize capacity check in jsonsize_test.go
// tests exactly the code generated for each field.

//ggen:generate
type PtrNameStruct struct {
	Name *string `json:"name"`
}

//ggen:generate
type PtrCountStruct struct {
	Count *int `json:"count,omitempty"`
}

//ggen:generate
type PtrRatioStruct struct {
	Ratio *float64 `json:"ratio,omitempty"`
}

//ggen:generate
type PtrAddrStruct struct {
	Addr *Address `json:"addr,omitempty"`
}

//ggen:generate
type PtrWhenStruct struct {
	When *time.Time `json:"when,omitempty,format:unix"`
}

//ggen:generate
type PtrEnabledStruct struct {
	Enabled *bool `json:"enabled"`
}

// PointerStruct combines the single-level *T fields. A nil pointer encodes as
// null and null decodes to nil.
//
//ggen:generate
type PointerStruct struct {
	PtrNameStruct
	PtrCountStruct
	PtrRatioStruct
	PtrAddrStruct
	PtrWhenStruct
	PtrEnabledStruct
}

func TestPointer_roundtripAllSet(t *testing.T) {
	t.Parallel()
	in := PointerStruct{
		Name:    new("alice"),
		Count:   new(7),
		Ratio:   new(0.5),
		Addr:    &Address{Street: "Main 1", City: "Lviv", ZipCode: "79000"},
		When:    new(time.Unix(1_700_000_000, 0).UTC()),
		Enabled: new(true),
	}
	out, _ := ggen.MarshalString(in)
	got, _, err := PointerStruct{}.DecodeFrom([]byte(out))
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if *got.Name != "alice" || *got.Count != 7 || *got.Ratio != 0.5 ||
		got.Addr.City != "Lviv" || !got.When.Equal(*in.When) || *got.Enabled != true {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

func TestPointer_nilOmitted(t *testing.T) {
	t.Parallel()
	in := PointerStruct{Name: new("bob"), Enabled: new(false)}
	out, _ := ggen.MarshalString(in)
	// Nil omitempty fields are absent.
	for _, absent := range []string{"count", "ratio", "addr", "when"} {
		if strings.Contains(out, `"`+absent+`"`) {
			t.Errorf("expected %q omitted, got %q", absent, out)
		}
	}
	// Fields without omitempty stay present.
	if !strings.Contains(out, `"name":"bob"`) {
		t.Errorf("name missing: %q", out)
	}
	if !strings.Contains(out, `"enabled":false`) {
		t.Errorf("enabled missing: %q", out)
	}
}

func TestPointer_nullRoundtrip(t *testing.T) {
	t.Parallel()
	in := []byte(`{"name":null,"enabled":null}`)
	got, _, err := PointerStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Name != nil {
		t.Errorf("expected Name nil, got %v", *got.Name)
	}
	if got.Enabled != nil {
		t.Errorf("expected Enabled nil, got %v", *got.Enabled)
	}
}

// Multi-level pointer fields (**T, ***T and deeper), one struct per depth and
// leaf type. This way the per-field JSONSize capacity check in
// jsonsize_test.go tests each depth of nil checks on its own. Reuse of a
// pointer chain is tested by the TestMerge_multiLevel* tests in merge_test.go,
// and parse failure by TestMerge_pointerParseFailureLeavesReceiverUntouched.
// Container variants are in NPtrContainersStruct below.

//ggen:generate
type PtrPPStruct struct {
	PP **int `json:"pp"`
}

//ggen:generate
type PtrPPPStruct struct {
	PPP ***int `json:"ppp"`
}

//ggen:generate
type PtrPPPPStruct struct {
	PPPP ****string `json:"pppp"`
}

//ggen:generate
type PtrAddr2Struct struct {
	Addr **Address `json:"addr"`
}

//ggen:generate
type NPtrStruct struct {
	PtrPPStruct
	PtrPPPStruct
	PtrPPPPStruct
	PtrAddr2Struct
}

func TestNPtr_scalarRoundtrip(t *testing.T) {
	t.Parallel()
	v1 := 42
	pv1 := &v1
	ppv1 := &pv1
	v2 := 7
	pv2 := &v2
	ppv2 := &pv2
	pppv2 := &ppv2
	s := "hello"
	ps := &s
	pps := &ps
	ppps := &pps
	in := NPtrStruct{
		PP:   ppv1,
		PPP:  pppv2,
		PPPP: &ppps,
		Addr: new(&Address{Street: "Main 1", City: "Lviv", ZipCode: "79000"}),
	}
	out, _ := ggen.MarshalString(in)
	got, _, err := NPtrStruct{}.DecodeFrom([]byte(out))
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if got.PP == nil || *got.PP == nil || **got.PP != 42 {
		t.Errorf("PP roundtrip: got %v", got.PP)
	}
	if got.PPP == nil || *got.PPP == nil || **got.PPP == nil || ***got.PPP != 7 {
		t.Errorf("PPP roundtrip: got %v", got.PPP)
	}
	if got.PPPP == nil || *got.PPPP == nil || **got.PPPP == nil || ***got.PPPP == nil || ****got.PPPP != "hello" {
		t.Errorf("PPPP roundtrip: got %v", got.PPPP)
	}
	if got.Addr == nil || *got.Addr == nil || (*got.Addr).City != "Lviv" {
		t.Errorf("Addr roundtrip: got %+v", got.Addr)
	}
}

func TestNPtr_allNull(t *testing.T) {
	t.Parallel()
	in := []byte(`{"pp":null,"ppp":null,"pppp":null,"addr":null}`)
	got, _, err := NPtrStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.PP != nil || got.PPP != nil || got.PPPP != nil || got.Addr != nil {
		t.Errorf("expected all nil, got %+v", got)
	}
}

// A non-nil outer pointer with a nil inner pointer marshals the field as null.
// Encoding stops at the first nil level.
func TestNPtr_intermediateNilMarshalsNull(t *testing.T) {
	t.Parallel()
	in := NPtrStruct{PP: new((*int)(nil))} // non-nil outer, nil inner
	out, err := ggen.MarshalString(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(out, `"pp":null`) {
		t.Errorf("expected \"pp\":null for intermediate-nil pointer, got %q", out)
	}
}

// []*T and [N]*T fields, one struct per container shape so a regression points
// at the shape. These decode their pointees into one shared backing slice
// (a slab) instead of one allocation per element. null elements decode to nil
// pointers.

//ggen:generate
type PtrSliceItemsStruct struct {
	Items []*Address `json:"items"`
}

//ggen:generate
type PtrSliceTupleStruct struct {
	Tuple [3]*Address `json:"tuple"`
}

//ggen:generate
type PtrSliceNodesStruct struct {
	Nodes []*Node `json:"nodes"`
}

// PtrSliceStruct combines the []*T and [N]*T shapes above.
//
//ggen:generate
type PtrSliceStruct struct {
	PtrSliceItemsStruct
	PtrSliceTupleStruct
	PtrSliceNodesStruct
}

// NPtrContainersStruct pins multi-level pointers inside containers. Slice
// elements, array elements and map values each decode their own pointer chain.
// They use no slab and no encoding/json fallback.
//
//ggen:generate
type NPtrContainersStruct struct {
	SPP  []**int             `json:"spp"`
	APP  [3]**int            `json:"app"`
	NSPP [][]**int           `json:"nspp"`
	MP   map[string]*int     `json:"mp"`
	MPP  map[string]**int    `json:"mpp"`
	MPA  map[string]*Address `json:"mpa"`
}

func TestNPtr_containersRoundtrip(t *testing.T) {
	t.Parallel()
	in := []byte(
		`{"spp":[1,null,3],"app":[null,5,null],"nspp":[[7,null]],"mp":{"a":1,"b":null},"mpp":{"x":7,"y":null},"mpa":{"k":{"street":"Main 1","city":"Lviv","zipCode":"79000"},"n":null}}`,
	)
	got, n, err := NPtrContainersStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if n != len(in) {
		t.Fatalf("consumed %d of %d", n, len(in))
	}
	if len(got.SPP) != 3 || **got.SPP[0] != 1 || got.SPP[1] != nil || **got.SPP[2] != 3 {
		t.Errorf("SPP = %v", got.SPP)
	}
	if got.APP[0] != nil || **got.APP[1] != 5 || got.APP[2] != nil {
		t.Errorf("APP = %v", got.APP)
	}
	if len(got.NSPP) != 1 || **got.NSPP[0][0] != 7 || got.NSPP[0][1] != nil {
		t.Errorf("NSPP = %v", got.NSPP)
	}
	if *got.MP["a"] != 1 || got.MP["b"] != nil {
		t.Errorf("MP = %v", got.MP)
	}
	if **got.MPP["x"] != 7 || got.MPP["y"] != nil {
		t.Errorf("MPP = %v", got.MPP)
	}
	if got.MPA["k"] == nil || got.MPA["k"].City != "Lviv" || got.MPA["n"] != nil {
		t.Errorf("MPA = %v", got.MPA)
	}

	// Marshal then decode must give back the same value, and the output must
	// fit in the JSONSize budget.
	size := got.JSONSize()
	out, err := got.AppendJSON(make([]byte, 0, size))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(out) > size {
		t.Errorf("JSONSize under-budget: emitted %d, reserved %d", len(out), size)
	}
	again, _, err := NPtrContainersStruct{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("re-decode: %v\n%s", err, out)
	}
	// Compare values, not bytes, because map fields marshal in random order.
	if !reflect.DeepEqual(got, again) {
		t.Errorf("roundtrip not a fixed point:\n%#v\n%#v", got, again)
	}
}

func TestNPtr_containersStream(t *testing.T) {
	t.Parallel()
	in := []byte(`{"spp":[1,null],"app":[9,null,2],"mp":{"a":4},"mpp":{"z":null}}`)
	var s ggen.Stream
	s.Reset(bytes.NewReader(in), make([]byte, 16))
	got, err := NPtrContainersStruct{}.DecodeFromStream(&s)
	if err != nil {
		t.Fatalf("stream decode: %v", err)
	}
	if len(got.SPP) != 2 || **got.SPP[0] != 1 || got.SPP[1] != nil {
		t.Errorf("SPP = %v", got.SPP)
	}
	if **got.APP[0] != 9 || got.APP[1] != nil || **got.APP[2] != 2 {
		t.Errorf("APP = %v", got.APP)
	}
	if *got.MP["a"] != 4 {
		t.Errorf("MP = %v", got.MP)
	}
	if v, ok := got.MPP["z"]; !ok || v != nil {
		t.Errorf("MPP = %v", got.MPP)
	}
}

// ---- pointers to containers, any depth -------------------------------
//
// The receiver reset has to reach through every pointer level. Decode only
// allocates a pointee when the pointer itself is nil. Without the reset, a
// reused receiver would append into the carried container, while a plain
// `[]T` field replaces its content.

//ggen:generate
type PtrContainers struct {
	Plain []int                         `json:"plain"`
	One   *[]int                        `json:"one"`
	Deep  ***[]int                      `json:"deep"`
	M1    *map[string]int               `json:"m1"`
	M3    ***map[string]int             `json:"m3"`
	Elems *[]PtrContainerElem           `json:"elems"`
	MElem **map[string]PtrContainerElem `json:"melem"`
}

//ggen:generate
type PtrContainerElem struct {
	K string `json:"k"`
}

func TestPtrContainer_ResetOnReuse(t *testing.T) {
	t.Parallel()
	first := []byte(`{"plain":[1,2,3],"one":[1,2,3],"deep":[1,2,3],"m1":{"a":1,"b":2},"m3":{"a":1,"b":2},` +
		`"elems":[{"k":"a"},{"k":"b"}],"melem":{"a":{"k":"a"},"b":{"k":"b"}}}`)
	second := []byte(`{"plain":[9],"one":[9],"deep":[9],"m1":{"z":9},"m3":{"z":9},` +
		`"elems":[{"k":"z"}],"melem":{"z":{"k":"z"}}}`)

	v, _, err := PtrContainers{}.DecodeFrom(first)
	if err != nil {
		t.Fatal(err)
	}
	v, _, err = v.DecodeFrom(second)
	if err != nil {
		t.Fatal(err)
	}

	if got := v.Plain; !reflect.DeepEqual(got, []int{9}) {
		t.Errorf("[]T: %v", got)
	}
	if got := *v.One; !reflect.DeepEqual(got, []int{9}) {
		t.Errorf("*[]T appended instead of replacing: %v", got)
	}
	if got := ***v.Deep; !reflect.DeepEqual(got, []int{9}) {
		t.Errorf("***[]T appended instead of replacing: %v", got)
	}
	if got := *v.M1; !reflect.DeepEqual(got, map[string]int{"z": 9}) {
		t.Errorf("*map merged instead of replacing: %v", got)
	}
	if got := ***v.M3; !reflect.DeepEqual(got, map[string]int{"z": 9}) {
		t.Errorf("***map merged instead of replacing: %v", got)
	}
	if got := *v.Elems; len(got) != 1 || got[0].K != "z" {
		t.Errorf("*[]struct appended instead of replacing: %v", got)
	}
	if got := **v.MElem; len(got) != 1 || got["z"].K != "z" {
		t.Errorf("**map[string]struct merged instead of replacing: %v", got)
	}
}

// An omitted key leaves the field as a fresh decode would: a pointer to a
// container comes back nil, not a cleared pointee.
func TestPtrContainer_OmittedKeyClears(t *testing.T) {
	t.Parallel()
	v, _, err := PtrContainers{}.DecodeFrom([]byte(`{"one":[1,2,3],"m1":{"a":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	v, _, err = v.DecodeFrom([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if v.One != nil {
		t.Errorf("omitted *[]T not cleared: %v", *v.One)
	}
	if v.M1 != nil {
		t.Errorf("omitted *map not cleared: %v", *v.M1)
	}
}

// Multi-level pointers to containers must decode too. The parser has to strip
// every pointer level to find the element kind. If it strips only one, the
// element kind at depth 2 and deeper is left as its zero value (KindString),
// and a `**[]T` would scan a string into a T slot.
func TestPtrContainer_DeepDecodes(t *testing.T) {
	t.Parallel()
	v, _, err := PtrContainers{}.DecodeFrom([]byte(`{"deep":[4,5],"m3":{"k":7},"melem":{"q":{"k":"q"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := ***v.Deep; !reflect.DeepEqual(got, []int{4, 5}) {
		t.Errorf("***[]int: %v", got)
	}
	if got := ***v.M3; !reflect.DeepEqual(got, map[string]int{"k": 7}) {
		t.Errorf("***map: %v", got)
	}
	if got := **v.MElem; got["q"].K != "q" {
		t.Errorf("**map[string]struct: %v", got)
	}
	out, err := v.AppendJSON(nil)
	if err != nil {
		t.Fatal(err)
	}
	back, _, err := PtrContainers{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("round-trip %s: %v", out, err)
	}
	if !reflect.DeepEqual(***back.Deep, ***v.Deep) {
		t.Errorf("round-trip mismatch: %v vs %v", ***back.Deep, ***v.Deep)
	}
}

// PtrMapHeavy covers a pointer to a map whose values own allocations. The
// bytes decode replaces such a map with a new one instead of clearing it, so
// the reset has to come from that replacement. PtrContainers only has scalar
// element types, which take the clear() path, so it cannot cover this.
//
//ggen:generate
type PtrMapHeavy struct {
	M *map[string]Node `json:"m"`
}

func TestPtrMap_valueReuseDropsOmittedKeys(t *testing.T) {
	t.Parallel()
	first, _, err := PtrMapHeavy{}.DecodeFrom([]byte(`{"m":{"a":{"id":1,"tags":["x","y"]},"b":{"id":2}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(*first.M) != 2 {
		t.Fatalf("first decode: %v", *first.M)
	}
	tags := unsafe.SliceData((*first.M)["a"].Tags)

	got, _, err := first.DecodeFrom([]byte(`{"m":{"a":{"id":9}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(*got.M) != 1 {
		t.Errorf("dropped key survived: %v", *got.M)
	}
	a := (*got.M)["a"]
	if a.ID != 9 || len(a.Tags) != 0 {
		t.Errorf("stale value data: %+v", a)
	}
	if cap(a.Tags) == 0 || unsafe.SliceData(a.Tags[:cap(a.Tags)]) != tags {
		t.Error("value backing reallocated — carried value was not reused")
	}
}
