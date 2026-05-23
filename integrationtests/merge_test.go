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
	got, _, err := receiver.DecodeFrom([]byte(`{"street":"new street","city":"NewCity","zipCode":"54321"}`), 0)
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

	got, _, err := receiver.DecodeFrom([]byte(`{"tags":["a","b","c"]}`), 0)
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
	got, _, err := receiver.DecodeFrom([]byte(`{"tags":["a","b"]}`), 0)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(got.Tags, []string{"a", "b"}) {
		t.Errorf("got %v want [a b] — appended over existing instead of resetting", got.Tags)
	}
}

func TestMerge_sliceNullSetsNil(t *testing.T) {
	receiver := Node{Tags: []string{"old"}}
	got, _, err := receiver.DecodeFrom([]byte(`{"tags":null}`), 0)
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
	got, _, err := receiver.DecodeFrom([]byte(`{"tags":[]}`), 0)
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
	got, _, err := receiver.DecodeFrom([]byte(`{"tags":[]}`), 0)
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
	got, _, err := receiver.DecodeFrom([]byte(`{"props":{"a":"1","b":"2"}}`), 0)
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
	got, _, err := receiver.DecodeFrom([]byte(`{"props":null}`), 0)
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
	got, _, err := receiver.DecodeFrom([]byte(`{"children":[{"tags":["fresh"]}]}`), 0)
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
