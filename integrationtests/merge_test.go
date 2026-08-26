package integrationtests

//go:generate ../ggen $GOFILE

import (
	"bytes"
	jsonv2 "encoding/json/v2"
	"reflect"
	"testing"
	"unsafe"

	"github.com/sirkostya009/ggen"
)

// Decode-into-receiver tests. The result is what a fresh decode would give —
// every field the payload omits comes back zeroed — while container capacity
// and element allocations are recycled out of the receiver. Tests call
// (T).DecodeFrom directly (ggen.Unmarshal starts fresh).

func TestMerge_omittedScalarsZeroed(t *testing.T) {
	t.Parallel()
	receiver := Node{ID: 7, Name: "old", Score: 1.5, Active: true}
	got, _, err := receiver.DecodeFrom([]byte(`{"name":"new"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "new" || got.ID != 0 || got.Score != 0 || got.Active {
		t.Errorf("omitted scalars not zeroed: %+v", got)
	}
}

func TestMerge_sliceBackingReused(t *testing.T) {
	t.Parallel()
	// A 3-elem decode into a cap-8 receiver must reuse the backing, not make
	// a fresh one.
	pre := make([]string, 0, 8)
	pre = append(pre, "old1", "old2", "old3")
	receiver := Node{Tags: pre}
	preBackingPtr := unsafe.SliceData(receiver.Tags)

	got, _, err := receiver.DecodeFrom([]byte(`{"tags":["a","b","c"]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(got.Tags, []string{"a", "b", "c"}) {
		t.Errorf("tags=%v want [a b c]", got.Tags)
	}
	gotBackingPtr := unsafe.SliceData(got.Tags)
	if gotBackingPtr != preBackingPtr {
		t.Errorf("backing array was reallocated — decode-into-receiver not reusing the slice")
	}
	if cap(got.Tags) < 8 {
		t.Errorf("cap shrunk: got %d, original 8", cap(got.Tags))
	}
}

func TestMerge_nestedSliceBackingReused(t *testing.T) {
	t.Parallel()
	// [][]int decode-into-receiver reuses BOTH outer and inner row backings
	// when the new shape fits the carried caps.
	first, _, err := ExtraStruct{}.DecodeFrom([]byte(`{"nestedInts":[[1,2,3],[4,5,6]]}`))
	if err != nil {
		t.Fatalf("first decode: %v", err)
	}
	outerPtr := unsafe.SliceData(first.NestedInts)
	row0Ptr := unsafe.SliceData(first.NestedInts[0])
	row1Ptr := unsafe.SliceData(first.NestedInts[1])

	// New rows fit the carried caps (3, 3).
	got, _, err := first.DecodeFrom([]byte(`{"nestedInts":[[7,8],[9,10,11]]}`))
	if err != nil {
		t.Fatalf("second decode: %v", err)
	}
	if !reflect.DeepEqual(got.NestedInts, [][]int{{7, 8}, {9, 10, 11}}) {
		t.Fatalf("nestedInts=%v want [[7 8] [9 10 11]]", got.NestedInts)
	}
	if unsafe.SliceData(got.NestedInts) != outerPtr {
		t.Errorf("outer backing reallocated — reslice-grow not reusing the outer slice")
	}
	if unsafe.SliceData(got.NestedInts[0]) != row0Ptr {
		t.Errorf("row 0 backing reallocated — nested row not reused")
	}
	if unsafe.SliceData(got.NestedInts[1]) != row1Ptr {
		t.Errorf("row 1 backing reallocated — nested row not reused")
	}
}

func TestMerge_sliceDoesNotAppendOverExisting(t *testing.T) {
	t.Parallel()
	// Receiver had 5, JSON has 2 → result is exactly 2 (no append-over).
	receiver := Node{Tags: []string{"old1", "old2", "old3", "old4", "old5"}}
	got, _, err := receiver.DecodeFrom([]byte(`{"tags":["a","b"]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(got.Tags, []string{"a", "b"}) {
		t.Errorf("got %v want [a b] — appended over existing instead of resetting", got.Tags)
	}
}

func TestMerge_sliceNullSetsNil(t *testing.T) {
	t.Parallel()
	receiver := Node{Tags: []string{"old"}}
	got, _, err := receiver.DecodeFrom([]byte(`{"tags":null}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Tags != nil {
		t.Errorf("expected nil tags after JSON null, got %v", got.Tags)
	}
}

func TestMerge_sliceEmptyPreservesNonNilBacking(t *testing.T) {
	t.Parallel()
	pre := make([]string, 0, 16)
	receiver := Node{Tags: pre}
	got, _, err := receiver.DecodeFrom([]byte(`{"tags":[]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Tags == nil {
		t.Errorf("empty [] should keep non-nil backing for a non-nil receiver")
	}
	if len(got.Tags) != 0 {
		t.Errorf("len=%d want 0", len(got.Tags))
	}
	if cap(got.Tags) < 16 {
		t.Errorf("cap=%d, original 16 — backing not reused", cap(got.Tags))
	}
}

func TestMerge_sliceEmptyOnNilReceiverProducesNonNilEmpty(t *testing.T) {
	t.Parallel()
	// stdlib parity: nil receiver + JSON [] → non-nil empty.
	receiver := Node{}
	got, _, err := receiver.DecodeFrom([]byte(`{"tags":[]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Tags == nil {
		t.Errorf("expected non-nil empty []string for JSON [], got nil")
	}
	if len(got.Tags) != 0 {
		t.Errorf("len=%d want 0", len(got.Tags))
	}
}

func TestMerge_mapClearedAndRefilled(t *testing.T) {
	t.Parallel()
	pre := map[string]string{"old": "value", "ghost": "data"}
	receiver := Node{Props: pre}
	got, _, err := receiver.DecodeFrom([]byte(`{"props":{"a":"1","b":"2"}}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := got.Props["old"]; ok {
		t.Errorf("receiver key 'old' survived — map was not cleared")
	}
	if got.Props["a"] != "1" || got.Props["b"] != "2" {
		t.Errorf("got %v want [a:1 b:2]", got.Props)
	}
}

func TestMerge_mapNullSetsNil(t *testing.T) {
	t.Parallel()
	receiver := Node{Props: map[string]string{"old": "value"}}
	got, _, err := receiver.DecodeFrom([]byte(`{"props":null}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Props != nil {
		t.Errorf("expected nil props after JSON null, got %v", got.Props)
	}
}

func TestMerge_nestedStructRecursesIntoExisting(t *testing.T) {
	t.Parallel()
	receiver := Node{
		Children: []Node{{Name: "cached", Tags: []string{"stale"}}},
	}
	got, _, err := receiver.DecodeFrom([]byte(`{"children":[{"tags":["fresh"]}]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Children) != 1 {
		t.Fatalf("len(children)=%d want 1", len(got.Children))
	}
	// The element decodes as a fresh Node — "name" is absent from the payload,
	// so the carried Name="cached" does NOT survive.
	if got.Children[0].Name != "" {
		t.Errorf("Name=%q — slice elem merged against receiver, expected fresh zero", got.Children[0].Name)
	}
	if !reflect.DeepEqual(got.Children[0].Tags, []string{"fresh"}) {
		t.Errorf("Tags=%v want [fresh]", got.Children[0].Tags)
	}
}

// Pointer-field merge (PointerStruct *T, NPtrStruct **T/…, both in
// pointer_test.go): a PRESENT key decodes into the carried pointee in place
// (chains reuse their non-nil prefix), an omitted key nils the field, null
// nils it too, and a parse failure leaves the receiver untouched.

func TestMerge_pointerFieldClearedWhenOmitted(t *testing.T) {
	t.Parallel()
	receiver := PointerStruct{Name: new("keep"), Count: new(7)}
	// "name" omitted → nil, as a fresh decode would give.
	got, _, err := receiver.DecodeFrom([]byte(`{"count":9,"enabled":true}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != nil {
		t.Errorf("omitted Name pointer not cleared: %v", *got.Name)
	}
	if got.Count == nil || *got.Count != 9 {
		t.Errorf("Count=%v want 9", got.Count)
	}
}

func TestMerge_pointerScalarReusesPointee(t *testing.T) {
	t.Parallel()
	orig := new(3)
	receiver := PointerStruct{Count: orig}
	got, _, err := receiver.DecodeFrom([]byte(`{"count":9}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Count != orig {
		t.Errorf("pointee reallocated: got %p want %p", got.Count, orig)
	}
	if *got.Count != 9 {
		t.Errorf("*Count=%d want 9", *got.Count)
	}
}

func TestMerge_pointerStructPointeeReused(t *testing.T) {
	t.Parallel()
	// Non-nil *Address pointee decoded in place — same pointer.
	orig := &Address{Street: "Main 1", City: "Lviv", ZipCode: "79000"}
	receiver := PointerStruct{Addr: orig}
	got, _, err := receiver.DecodeFrom([]byte(`{"addr":{"street":"Main 2","city":"Kyiv","zipCode":"01001"}}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Addr != orig {
		t.Errorf("struct pointee reallocated: got %p want %p", got.Addr, orig)
	}
	if got.Addr.City != "Kyiv" {
		t.Errorf("City=%q want Kyiv", got.Addr.City)
	}
}

func TestMerge_pointerNilReceiverAllocates(t *testing.T) {
	t.Parallel()
	// nil receiver fields → fresh pointees allocated.
	got, _, err := PointerStruct{}.DecodeFrom([]byte(`{"count":9,"name":"x"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Count == nil || *got.Count != 9 {
		t.Errorf("Count=%v want 9", got.Count)
	}
	if got.Name == nil || *got.Name != "x" {
		t.Errorf("Name=%v want x", got.Name)
	}
}

func TestMerge_pointerNullDropsExistingPointee(t *testing.T) {
	t.Parallel()
	// JSON null nils the field even when the receiver carried a pointee.
	receiver := PointerStruct{Name: new("x"), Enabled: new(true)}
	got, _, err := receiver.DecodeFrom([]byte(`{"name":null,"enabled":null}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != nil || got.Enabled != nil {
		t.Errorf("null should nil pointee: name=%v enabled=%v", got.Name, got.Enabled)
	}
}

func TestMerge_multiLevelReusesWholeChain(t *testing.T) {
	t.Parallel()
	inner := new(3) // *int
	outer := &inner // **int, both levels allocated
	receiver := NPtrStruct{PP: outer}
	got, _, err := receiver.DecodeFrom([]byte(`{"pp":9}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.PP != outer {
		t.Errorf("outer pointer reallocated: %p != %p", got.PP, outer)
	}
	if *got.PP != inner {
		t.Errorf("inner pointer reallocated: %p != %p", *got.PP, inner)
	}
	if **got.PP != 9 {
		t.Errorf("**PP=%d want 9", **got.PP)
	}
}

func TestMerge_multiLevelAllocatesFromFirstNil(t *testing.T) {
	t.Parallel()
	var inner *int  // nil *int
	outer := &inner // **int, outer non-nil, inner nil
	receiver := NPtrStruct{PP: outer}
	got, _, err := receiver.DecodeFrom([]byte(`{"pp":9}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.PP != outer {
		t.Errorf("outer reallocated despite being non-nil")
	}
	if *got.PP == nil || **got.PP != 9 {
		t.Errorf("inner not allocated/decoded: %v", got.PP)
	}
}

func TestMerge_multiLevelStructLeafReused(t *testing.T) {
	t.Parallel()
	leaf := &Address{Street: "Main 1", City: "Lviv", ZipCode: "79000"}
	mid := &leaf // **Address, fully allocated
	receiver := NPtrStruct{Addr: mid}
	got, _, err := receiver.DecodeFrom([]byte(`{"addr":{"street":"Main 2","city":"Kyiv","zipCode":"01001"}}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Addr != mid || *got.Addr != leaf {
		t.Errorf("chain not reused")
	}
	if (**got.Addr).City != "Kyiv" {
		t.Errorf("City=%q want Kyiv", (**got.Addr).City)
	}
}

func TestMerge_pointerParseFailureLeavesReceiverUntouched(t *testing.T) {
	t.Parallel()
	// Leaf parses into a temp first, so a failure leaves the field nil
	// (no half-allocated chain).
	got, _, err := NPtrStruct{}.DecodeFrom([]byte(`{"pp":"not a number"}`))
	if err == nil {
		t.Fatal("expected parse error")
	}
	if got.PP != nil {
		t.Errorf("PP allocated for a value that never parsed: %v", got.PP)
	}
}

// Array slots decode fresh, never merged — the element decoder zeroes what
// the payload omits, so `[2]Inner`, `[]Inner`, `[2]*Inner` and `[2]**Inner`
// all agree with jsonv2. Pinned on both paths.
//
//ggen:generate
type ArrMerge struct {
	AI [2]MergeInner   `json:"ai"`
	SI []MergeInner    `json:"si"`
	AP [2]*MergeInner  `json:"ap"`
	AM [2]**MergeInner `json:"am"`
}

//ggen:generate
type MergeInner struct {
	X int    `json:"x"`
	Y string `json:"y"`
}

func carriedArrMerge() ArrMerge {
	i0, i1 := &MergeInner{1, "keep0"}, &MergeInner{2, "keep1"}
	p0, p1 := &i0, &i1
	return ArrMerge{
		AI: [2]MergeInner{{1, "keep0"}, {2, "keep1"}},
		SI: []MergeInner{{1, "keep0"}},
		AP: [2]*MergeInner{{1, "keep0"}, {2, "keep1"}},
		AM: [2]**MergeInner{p0, p1},
	}
}

func TestMerge_ArraySlotsOverwrite(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"ai":[{"x":5},{"x":6}],"si":[{"x":5}],"ap":[{"x":5},{"x":6}],"am":[{"x":5},{"x":6}]}`)

	got, _, err := carriedArrMerge().DecodeFrom(payload)
	if err != nil {
		t.Fatal(err)
	}
	want := carriedArrMerge()
	if err := jsonv2.Unmarshal(payload, &want); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if got.AI[i] != want.AI[i] {
			t.Errorf("AI[%d]: ggen %+v, jsonv2 %+v", i, got.AI[i], want.AI[i])
		}
		if *got.AP[i] != *want.AP[i] {
			t.Errorf("AP[%d]: ggen %+v, jsonv2 %+v", i, *got.AP[i], *want.AP[i])
		}
		if **got.AM[i] != **want.AM[i] {
			t.Errorf("AM[%d]: ggen %+v, jsonv2 %+v", i, **got.AM[i], **want.AM[i])
		}
	}
	if got.SI[0] != want.SI[0] {
		t.Errorf("SI[0]: ggen %+v, jsonv2 %+v", got.SI[0], want.SI[0])
	}

	var st ggen.Stream
	st.Reset(bytes.NewReader(payload), make([]byte, 0, 8))
	sg, err := carriedArrMerge().DecodeFromStream(&st)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if sg.AI[i] != got.AI[i] || **sg.AM[i] != **got.AM[i] {
			t.Errorf("stream[%d] diverges: AI %+v vs %+v", i, sg.AI[i], got.AI[i])
		}
	}
}

// A generated struct element is handed the carried slot instead of a blanked
// one, so its inner allocations (here the element's own []string) are reused
// across decodes while the decoded value still comes out fresh.
func TestMerge_sliceElementAllocationsReused(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"children":[{"name":"a","tags":["t1","t2"]},{"name":"b","tags":["t3"]}]}`)
	first, _, err := Node{}.DecodeFrom(payload)
	if err != nil {
		t.Fatal(err)
	}
	outer := unsafe.SliceData(first.Children)
	row0 := unsafe.SliceData(first.Children[0].Tags)
	row1 := unsafe.SliceData(first.Children[1].Tags)

	got, _, err := first.DecodeFrom(payload)
	if err != nil {
		t.Fatal(err)
	}
	if unsafe.SliceData(got.Children) != outer {
		t.Error("children backing reallocated")
	}
	if unsafe.SliceData(got.Children[0].Tags) != row0 {
		t.Error("element 0 tags backing reallocated — carried element was blanked")
	}
	if unsafe.SliceData(got.Children[1].Tags) != row1 {
		t.Error("element 1 tags backing reallocated — carried element was blanked")
	}
	if got.Children[0].Name != "a" || !reflect.DeepEqual(got.Children[1].Tags, []string{"t3"}) {
		t.Errorf("values wrong: %+v", got.Children)
	}
}

// Every field kind the payload omits comes back zeroed; only the container
// keeps its (emptied) backing.
//
//ggen:generate
type OmitZeroed struct {
	S    string      `json:"s"`
	N    int         `json:"n"`
	In   MergeInner  `json:"in"`
	P    *MergeInner `json:"p"`
	A    [2]int      `json:"a"`
	Tags []string    `json:"tags"`
}

func TestMerge_omittedKeysZeroEveryKind(t *testing.T) {
	t.Parallel()
	full := []byte(`{"s":"x","n":7,"in":{"x":1,"y":"y"},"p":{"x":2},"a":[3,4],"tags":["a","b"]}`)
	check := func(t *testing.T, got OmitZeroed) {
		t.Helper()
		if got.S != "keep" {
			t.Errorf("S=%q want keep", got.S)
		}
		if got.N != 0 {
			t.Errorf("N=%d want 0", got.N)
		}
		if got.In != (MergeInner{}) {
			t.Errorf("In=%+v want zero", got.In)
		}
		if got.P != nil {
			t.Errorf("P=%+v want nil", *got.P)
		}
		if got.A != [2]int{} {
			t.Errorf("A=%v want zero", got.A)
		}
		if got.Tags == nil || len(got.Tags) != 0 {
			t.Errorf("Tags=%v want empty non-nil (capacity kept)", got.Tags)
		}
	}

	first, _, err := OmitZeroed{}.DecodeFrom(full)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := first.DecodeFrom([]byte(`{"s":"keep"}`))
	if err != nil {
		t.Fatal(err)
	}
	check(t, got)

	var st ggen.Stream
	st.Reset(bytes.NewReader([]byte(`{"s":"keep"}`)), make([]byte, 0, 8))
	sg, err := first.DecodeFromStream(&st)
	if err != nil {
		t.Fatal(err)
	}
	check(t, sg)
}

// The hard case for element reuse: a LEANER second payload. Recycled element
// allocations must carry no data forward — the element's own reset is what
// makes reusing its slot safe — while the backings themselves are still reused.
func TestMerge_leanRedecodeLeavesNoStaleData(t *testing.T) {
	t.Parallel()
	fat := []byte(`{"id":42,"name":"hello","active":true,"props":{"k":"v"},` +
		`"children":[{"id":7,"name":"kid","tags":["a","b"],"children":[{"id":9,"name":"deep"}]}]}`)
	lean := []byte(`{"children":[{"id":1}]}`)

	first, _, err := Node{}.DecodeFrom(fat)
	if err != nil {
		t.Fatal(err)
	}
	outer := unsafe.SliceData(first.Children)
	tags := unsafe.SliceData(first.Children[0].Tags)

	got, _, err := first.DecodeFrom(lean)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 0 || got.Name != "" || got.Active || len(got.Props) != 0 {
		t.Errorf("top-level stale: ID=%d Name=%q Active=%v Props=%v",
			got.ID, got.Name, got.Active, got.Props)
	}
	if len(got.Children) != 1 {
		t.Fatalf("children len %d, want 1", len(got.Children))
	}
	if c := got.Children[0]; c.ID != 1 || c.Name != "" || len(c.Tags) != 0 || len(c.Children) != 0 {
		t.Errorf("element stale: %+v", c)
	}
	if unsafe.SliceData(got.Children) != outer {
		t.Error("children backing reallocated")
	}
	if c := got.Children[0]; cap(c.Tags) == 0 || unsafe.SliceData(c.Tags[:cap(c.Tags)]) != tags {
		t.Error("element tags backing reallocated — carried element was blanked")
	}

	// Same through the stream path, at a chunk size that forces refills.
	var s ggen.Stream
	s.Reset(bytes.NewReader(fat), make([]byte, 0, 8))
	sv, err := Node{}.DecodeFromStream(&s)
	if err != nil {
		t.Fatal(err)
	}
	s.Reset(bytes.NewReader(lean), make([]byte, 0, 8))
	sv, err = sv.DecodeFromStream(&s)
	if err != nil {
		t.Fatal(err)
	}
	if sv.ID != 0 || sv.Name != "" || sv.Active {
		t.Errorf("stream top-level stale: %+v", sv)
	}
	if len(sv.Children) != 1 || sv.Children[0].ID != 1 || len(sv.Children[0].Tags) != 0 {
		t.Errorf("stream element stale: %+v", sv.Children)
	}
}
