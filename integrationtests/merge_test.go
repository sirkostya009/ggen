package integrationtests

import (
	"reflect"
	"testing"
	"unsafe"
)

// Decode-into-receiver tests. The receiver IS the merge source: scalars
// persist across omitted keys, containers reset before refill, nested structs
// recurse. Tests call (T).DecodeFrom directly (decode.Unmarshal starts fresh).

func TestMerge_scalarFieldsPersistAcrossOmitted(t *testing.T) {
	t.Parallel()
	// Payload sets only Street; receiver's other fields persist.
	receiver := Address{Street: "old street", City: "OldCity", ZipCode: "12345"}
	got, _, err := receiver.DecodeFrom([]byte(`{"street":"new street","city":"NewCity","zipCode":"54321"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := Address{Street: "new street", City: "NewCity", ZipCode: "54321"}
	if got != want {
		t.Errorf("got %+v want %+v", got, want)
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
	// The parent slice resets and pre-grows each slot with a zero Node, so
	// the carried Name="cached" does NOT survive.
	if got.Children[0].Name != "" {
		t.Errorf("Name=%q — slice elem merged against receiver, expected fresh zero", got.Children[0].Name)
	}
	if !reflect.DeepEqual(got.Children[0].Tags, []string{"fresh"}) {
		t.Errorf("Tags=%v want [fresh]", got.Children[0].Tags)
	}
}

// Pointer-field merge (PointerStruct *T, NPtrStruct **T/…, both in
// pointer_test.go): non-nil pointees reused in place, omitted fields kept,
// null nils, multi-level chains reuse their non-nil prefix, and a parse
// failure leaves the receiver untouched.

func TestMerge_pointerFieldPersistsWhenOmitted(t *testing.T) {
	t.Parallel()
	keep := new("keep")
	receiver := PointerStruct{PtrNameStruct: PtrNameStruct{Name: keep}, PtrCountStruct: PtrCountStruct{Count: new(7)}}
	// "name" omitted → receiver pointer untouched.
	got, _, err := receiver.DecodeFrom([]byte(`{"count":9,"enabled":true}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != keep || *got.Name != "keep" {
		t.Errorf("omitted Name pointer not preserved: %v", got.Name)
	}
	if got.Count == nil || *got.Count != 9 {
		t.Errorf("Count=%v want 9", got.Count)
	}
}

func TestMerge_pointerScalarReusesPointee(t *testing.T) {
	t.Parallel()
	orig := new(3)
	receiver := PointerStruct{PtrCountStruct: PtrCountStruct{Count: orig}}
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
	receiver := PointerStruct{PtrAddrStruct: PtrAddrStruct{Addr: orig}}
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
	receiver := PointerStruct{PtrNameStruct: PtrNameStruct{Name: new("x")}, PtrEnabledStruct: PtrEnabledStruct{Enabled: new(true)}}
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
	receiver := NPtrStruct{PtrPPStruct: PtrPPStruct{PP: outer}}
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
	receiver := NPtrStruct{PtrPPStruct: PtrPPStruct{PP: outer}}
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
	receiver := NPtrStruct{PtrAddr2Struct: PtrAddr2Struct{Addr: mid}}
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
