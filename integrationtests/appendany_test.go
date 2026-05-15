//go:build goexperiment.jsonv2

// AppendAny tests. The function type-switches on the runtime value of
// `any` to skip the encoding/json reflection cliff for common cases —
// these tests pin every fast-path branch and verify the fallback path
// still produces output that round-trips through jsonv2.

package integrationtests

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"reflect"
	"testing"

	"github.com/sirkostya009/ggen/encode"
)

// reparse marshals via jsonv2 (the reference) and parses to any so map
// ordering and other Go-representation noise doesn't fail the compare.
func reparse(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := jsonv2.Unmarshal(b, &v); err != nil {
		t.Fatalf("jsonv2.Unmarshal(%s): %v", b, err)
	}
	return v
}

func checkAny(t *testing.T, in any) {
	t.Helper()
	got, err := encode.AppendAny(nil, in)
	if err != nil {
		t.Fatalf("AppendAny(%#v): %v", in, err)
	}
	want, err := jsonv2.Marshal(in)
	if err != nil {
		t.Fatalf("jsonv2.Marshal(%#v): %v", in, err)
	}
	if !reflect.DeepEqual(reparse(t, got), reparse(t, want)) {
		t.Errorf("AppendAny mismatch\n in:    %#v\n got:   %s\n want:  %s", in, got, want)
	}
}

func TestAppendAny_Nil(t *testing.T) {
	out, err := encode.AppendAny(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "null" {
		t.Errorf("nil → %q, want null", out)
	}
}

func TestAppendAny_Bool(t *testing.T) {
	checkAny(t, true)
	checkAny(t, false)
}

func TestAppendAny_String(t *testing.T) {
	checkAny(t, "")
	checkAny(t, "hello")
	checkAny(t, "tab\there")
	checkAny(t, "quote\"inside")
	checkAny(t, "<a href=\"x\">b & c</a>") // HTML-escape exercised
}

func TestAppendAny_Numbers(t *testing.T) {
	checkAny(t, int(42))
	checkAny(t, int8(-7))
	checkAny(t, int16(-1234))
	checkAny(t, int32(-7))
	checkAny(t, int64(1<<60))
	checkAny(t, uint(0))
	checkAny(t, uint8(255))
	checkAny(t, byte(7)) // alias for uint8 — same case, exercised explicitly
	checkAny(t, uint16(65535))
	checkAny(t, uint32(1))
	checkAny(t, uint64(1<<63))
	checkAny(t, float32(1.5))
	checkAny(t, float64(3.14159))
}

func TestAppendAny_JSONNumber(t *testing.T) {
	out, err := encode.AppendAny(nil, json.Number("12345"))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "12345" {
		t.Errorf("json.Number → %q, want 12345", out)
	}
}

func TestAppendAny_Slice(t *testing.T) {
	checkAny(t, []any{})
	checkAny(t, []any{1, "two", true, nil})
	checkAny(t, []any{[]any{1, 2}, []any{3, 4}})
}

func TestAppendAny_Map(t *testing.T) {
	checkAny(t, map[string]any{})
	checkAny(t, map[string]any{"k": "v"})
	checkAny(t, map[string]any{
		"id":     int64(1),
		"name":   "alice",
		"active": true,
		"tags":   []any{"a", "b"},
		"meta":   map[string]any{"city": "Lviv"},
	})
}

func TestAppendAny_PrimitiveSlices(t *testing.T) {
	checkAny(t, []string{"a", "b", "c"})
	checkAny(t, []string{})
	checkAny(t, []int{1, 2, 3})
	checkAny(t, []int8{-1, 2, -3})
	checkAny(t, []int16{-1000, 1000})
	checkAny(t, []int32{-7, 7})
	checkAny(t, []int64{1 << 60, -1 << 60})
	checkAny(t, []uint{0, 1, 2})
	checkAny(t, []uint16{0, 65535})
	checkAny(t, []uint32{0, 1 << 31})
	checkAny(t, []uint64{0, 1 << 63})
	checkAny(t, []float32{1.5, -2.5})
	checkAny(t, []float64{1.5, 2.5})
	checkAny(t, []bool{true, false, true})
}

func TestAppendAny_Bytes(t *testing.T) {
	// Stdlib marshals []byte as a base64 string — match.
	checkAny(t, []byte("hello world"))
	checkAny(t, []byte{0xde, 0xad, 0xbe, 0xef})
}

func TestAppendAny_StringMap(t *testing.T) {
	checkAny(t, map[string]string{"k": "v"})
	checkAny(t, map[string]string{"a": "1", "b": "2", "c": "3"})
}

func TestAppendAny_PrimitiveMaps(t *testing.T) {
	checkAny(t, map[string]int{"a": 1, "b": 2})
	checkAny(t, map[string]int8{"a": -1})
	checkAny(t, map[string]int16{"a": -1000})
	checkAny(t, map[string]int32{"a": -7})
	checkAny(t, map[string]int64{"a": 1 << 60})
	checkAny(t, map[string]uint{"a": 1})
	checkAny(t, map[string]uint8{"a": 255})
	checkAny(t, map[string]uint16{"a": 65535})
	checkAny(t, map[string]uint32{"a": 1 << 31})
	checkAny(t, map[string]uint64{"a": 1 << 63})
	checkAny(t, map[string]float32{"a": 1.5})
	checkAny(t, map[string]float64{"a": 3.14})
	checkAny(t, map[string]bool{"a": true, "b": false})
}

func TestAppendAny_Nested(t *testing.T) {
	checkAny(t, map[string]any{
		"items": []any{
			map[string]any{"k": 1},
			map[string]any{"k": 2, "n": []any{nil, true, "x"}},
		},
	})
}

// Untagged exported struct — uses Go field names verbatim.
func TestAppendAny_Struct_Bare(t *testing.T) {
	type point struct {
		X, Y int
	}
	checkAny(t, point{X: 1, Y: 2})
}

// json-tagged fields: rename + skip via "-" + omitempty + omitzero.
func TestAppendAny_Struct_Tags(t *testing.T) {
	type tagged struct {
		Name    string `json:"name"`
		Skip    string `json:"-"`
		Empty   string `json:"empty,omitempty"`
		Zero    int    `json:"zero,omitzero"`
		Present string `json:"present,omitempty"`
	}
	checkAny(t, tagged{Name: "alice", Skip: "ignored", Present: "hi"})
}

// Pointer fields and pointer-typed top-level value.
func TestAppendAny_Struct_Pointers(t *testing.T) {
	type withPtr struct {
		Name *string `json:"name"`
		N    *int    `json:"n"`
	}
	name := "alice"
	n := 42
	checkAny(t, withPtr{Name: &name, N: &n})
	checkAny(t, withPtr{}) // both nil → null
	// Top-level pointer derefs.
	checkAny(t, &n)
	var nilP *int
	checkAny(t, nilP)
}

// Anonymous embedded struct fields are promoted at parent level.
func TestAppendAny_Struct_Embedded(t *testing.T) {
	type Base struct {
		ID   string `json:"id"`
		Meta string `json:"meta"`
	}
	type Derived struct {
		Base
		Name string `json:"name"`
	}
	checkAny(t, Derived{Base: Base{ID: "abc", Meta: "m"}, Name: "alice"})
}

// Nested struct fields hit the recursion path.
func TestAppendAny_Struct_Nested(t *testing.T) {
	type addr struct {
		City string `json:"city"`
	}
	type user struct {
		Name string `json:"name"`
		Addr addr   `json:"addr"`
	}
	checkAny(t, user{Name: "alice", Addr: addr{City: "Lviv"}})
}

// ,string option wraps numeric/bool primitives in JSON quotes.
func TestAppendAny_Struct_StringOpt(t *testing.T) {
	type quoted struct {
		N int  `json:"n,string"`
		B bool `json:"b,string"`
	}
	out, err := encode.AppendAny(nil, quoted{N: 42, B: true})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"n":"42","b":"true"}` {
		t.Errorf(",string opt → %q", out)
	}
}

// Named primitive types route through reflection's primitive cases.
func TestAppendAny_NamedPrimitives(t *testing.T) {
	type myString string
	type myInt int
	type myBool bool
	checkAny(t, myString("hi"))
	checkAny(t, myInt(42))
	checkAny(t, myBool(true))
}

// Unsupported kinds (channels, funcs) error instead of silently skipping.
func TestAppendAny_Unsupported(t *testing.T) {
	_, err := encode.AppendAny(nil, make(chan int))
	if err == nil {
		t.Fatal("expected error for chan, got nil")
	}
	_, err = encode.AppendAny(nil, func() {})
	if err == nil {
		t.Fatal("expected error for func, got nil")
	}
}

// --- interface-dispatch cold-path coverage ---------------------------------

type fakeAppendJSON struct{ payload string }

func (f fakeAppendJSON) JSONSize() int { return len(f.payload) }
func (f fakeAppendJSON) AppendJSON(dst []byte) ([]byte, error) {
	return append(dst, f.payload...), nil
}

type fakeJSONMarshaler struct{ s string }

func (f fakeJSONMarshaler) MarshalJSON() ([]byte, error) {
	return []byte(`{"s":"` + f.s + `"}`), nil
}

type fakeTextAppender struct{ word string }

func (f fakeTextAppender) AppendText(dst []byte) ([]byte, error) {
	return append(dst, f.word...), nil
}

type fakeTextMarshaler struct{ word string }

func (f fakeTextMarshaler) MarshalText() ([]byte, error) {
	return []byte(f.word), nil
}

func TestAppendAny_Marshaler(t *testing.T) {
	out, err := encode.AppendAny(nil, fakeAppendJSON{payload: `{"raw":1}`})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"raw":1}` {
		t.Errorf("AppendJSON path → %q", out)
	}
}

func TestAppendAny_JSONMarshaler(t *testing.T) {
	out, err := encode.AppendAny(nil, fakeJSONMarshaler{s: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"s":"hi"}` {
		t.Errorf("MarshalJSON path → %q", out)
	}
}

func TestAppendAny_TextAppender(t *testing.T) {
	out, err := encode.AppendAny(nil, fakeTextAppender{word: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"hello"` {
		t.Errorf("AppendText path → %q", out)
	}
}

func TestAppendAny_TextMarshaler(t *testing.T) {
	out, err := encode.AppendAny(nil, fakeTextMarshaler{word: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"hello"` {
		t.Errorf("MarshalText path → %q", out)
	}
}

func TestAppendAny_NonStringMapKey(t *testing.T) {
	_, err := encode.AppendAny(nil, map[int]string{1: "a"})
	if err == nil {
		t.Fatal("expected error for non-string map key, got nil")
	}
}

// AppendJSON-style: caller-owned dst gets appended to (not replaced).
func TestAppendAny_AppendsToDst(t *testing.T) {
	dst := []byte("prefix:")
	out, err := encode.AppendAny(dst, 42)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "prefix:42" {
		t.Errorf("AppendAny didn't append to dst — got %q", out)
	}
}
