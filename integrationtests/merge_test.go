package integrationtests

import (
	"reflect"
	"testing"
	"unsafe"
)

// Decode-into-receiver tests. The receiver passed to DecodeFrom IS the merge
// source — scalars from receiver persist when JSON omits them, containers
// reset (slice[:0] / clear(map)) before refill, and nested struct fields
// recurse through the same value-receiver pattern.
//
// All tests call (T).DecodeFrom directly rather than decode.Unmarshal[T]
// (which always builds a fresh zero value).

func TestMerge_scalarFieldsPersistAcrossOmitted(t *testing.T) {
	// receiver carries Name and ZipCode. Payload only sets Street. Merge
	// keeps the un-omitted fields from the receiver.
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
	// receiver carries a Tags slice with cap=8. After decoding a 3-element
	// JSON array, the resulting Tags must share the same backing array
	// (proof of [:0] reuse, not a fresh make()).
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

func TestMerge_sliceDoesNotAppendOverExisting(t *testing.T) {
	// "new decoder must not append into an existing slice" — receiver had 5
	// entries, JSON has 2; result has exactly 2 (not 7).
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
	// Children[0] carries Name="cached"; JSON omits child name, so merge
	// preserves it. Children[0]'s Tags is filled in JSON — that field
	// resets.
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
	// Children slice was reset+refilled by the parent slice's emit, which
	// pre-grows with `Node{}` zero-values. So the inner Node receiver IS
	// zero, and Name="cached" does NOT survive. This is by design: the
	// outer slice owns the reset, the inner Node merge happens only when
	// the slice slot was already populated by the receiver AND the JSON
	// has data for it — but we [:0]'d the slice up front, so the slot is
	// always a fresh zero-value pre-grow before recurse.
	if got.Children[0].Name != "" {
		t.Errorf("Name=%q — slice elem merged against receiver, expected fresh zero", got.Children[0].Name)
	}
	if !reflect.DeepEqual(got.Children[0].Tags, []string{"fresh"}) {
		t.Errorf("Tags=%v want [fresh]", got.Children[0].Tags)
	}
}

// Pointer-field merge against the existing PointerStruct (`*T`) and NPtrStruct
// (`**T`/…), both declared in pointer_test.go: a non-nil pointee is reused in
// place rather than reallocated, an omitted pointer field keeps its receiver
// value, JSON null nils the field, multi-level chains reuse their non-nil
// prefix (allocating only from the first nil level down), and a parse failure
// leaves the receiver untouched.

func TestMerge_pointerFieldPersistsWhenOmitted(t *testing.T) {
	keep := new("keep")
	receiver := PointerStruct{PtrNameStruct: PtrNameStruct{Name: keep}, PtrCountStruct: PtrCountStruct{Count: new(7)}}
	// Payload omits "name" → its receiver pointer is left untouched.
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
	// Non-nil *Address pointee is decoded in place — same pointer, no realloc.
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
	// nil receiver fields → `new` fires, fresh pointees decoded.
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
	// A wrong-typed leaf must NOT leave a freshly-allocated chain behind:
	// the leaf is parsed into a temp first, so the nil receiver field stays
	// nil on error.
	got, _, err := NPtrStruct{}.DecodeFrom([]byte(`{"pp":"not a number"}`))
	if err == nil {
		t.Fatal("expected parse error")
	}
	if got.PP != nil {
		t.Errorf("PP allocated for a value that never parsed: %v", got.PP)
	}
}
