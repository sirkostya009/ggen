package ggen

import (
	"bytes"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// anyCases covers the shapes Any decodes. Each input goes to json.Unmarshal,
// with and without UseNumber, and to Any and AnyNumber. The results must be
// equal under reflect.DeepEqual.
var anyCases = []struct {
	name string
	in   string
}{
	{"null", `null`},
	{"true", `true`},
	{"false", `false`},
	{"empty_string", `""`},
	{"plain_string", `"hello"`},
	{"escaped_string", `"a\\b\tc\n\"\u00e9"`},
	{"int", `42`},
	{"neg_int", `-7`},
	{"float", `3.14`},
	{"sci", `1.5e3`},
	{"large_int", `9007199254740993`},
	{"empty_array", `[]`},
	{"empty_object", `{}`},
	{"flat_array", `[1, 2, 3]`},
	{"mixed_array", `[1, "two", true, null, 4.5]`},
	{"nested_array", `[[1,2],[3,4]]`},
	{"flat_object", `{"a":1,"b":"x","c":true}`},
	{"nested_object", `{"x":{"y":{"z":[1,2,3]}}}`},
	{"whitespace", "  {  \"a\" : [ 1 , 2 ] , \"b\" : null }  "},
	{"complex", `{
		"name": "alice",
		"age": 30,
		"active": true,
		"score": 87.5,
		"big": 9007199254740993,
		"tags": ["go", "rust", "json"],
		"address": {"city": "Lviv", "zip": "79000"},
		"nested": [[1,2,3],[4,5,6]],
		"missing": null
	}`},
}

func TestAny_StdlibParity(t *testing.T) {
	t.Parallel()
	for _, tc := range anyCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var want any
			if err := json.Unmarshal([]byte(tc.in), &want); err != nil {
				t.Fatalf("stdlib: %v", err)
			}
			got, _, err := Any([]byte(tc.in), 0, true)
			if err != nil {
				t.Fatalf("Any: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("mismatch\n got: %#v (%T)\nwant: %#v (%T)", got, got, want, want)
			}
		})
	}
}

func TestAnyNumber_StdlibParity(t *testing.T) {
	t.Parallel()
	for _, tc := range anyCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dec := json.NewDecoder(strings.NewReader(tc.in))
			dec.UseNumber()
			var want any
			if err := dec.Decode(&want); err != nil {
				t.Fatalf("stdlib UseNumber: %v", err)
			}
			got, _, err := AnyNumber([]byte(tc.in), 0, true)
			if err != nil {
				t.Fatalf("AnyNumber: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("mismatch\n got: %#v (%T)\nwant: %#v (%T)", got, got, want, want)
			}
		})
	}
}

// TestAnyCopy_ParityAndDecoupled checks that AnyCopy returns the same value as
// the standard library, and that every string is a copy. A string aliases the
// input when it points into the input bytes without a copy. After the input
// is overwritten, the decoded value must not change. An aliased string would
// change with it.
func TestAnyCopy_ParityAndDecoupled(t *testing.T) {
	t.Parallel()
	for _, tc := range anyCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var want any
			if err := json.Unmarshal([]byte(tc.in), &want); err != nil {
				t.Fatalf("stdlib: %v", err)
			}
			buf := []byte(tc.in)
			got, _, err := AnyCopy(buf, 0, true)
			if err != nil {
				t.Fatalf("AnyCopy: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("parity mismatch\n got: %#v\nwant: %#v", got, want)
			}
			for i := range buf {
				buf[i] = ' '
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("AnyCopy aliased the input: value changed after source mutation\n got: %#v\nwant: %#v", got, want)
			}
		})
	}
}

// TestAnyNumberCopy_ParityAndDecoupled is the same test for AnyNumberCopy. The
// json.Number text is copied too, so it must survive overwriting the input.
func TestAnyNumberCopy_ParityAndDecoupled(t *testing.T) {
	t.Parallel()
	for _, tc := range anyCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dec := json.NewDecoder(strings.NewReader(tc.in))
			dec.UseNumber()
			var want any
			if err := dec.Decode(&want); err != nil {
				t.Fatalf("stdlib UseNumber: %v", err)
			}
			buf := []byte(tc.in)
			got, _, err := AnyNumberCopy(buf, 0, true)
			if err != nil {
				t.Fatalf("AnyNumberCopy: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("parity mismatch\n got: %#v\nwant: %#v", got, want)
			}
			for i := range buf {
				buf[i] = ' '
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("AnyNumberCopy aliased the input: value changed after source mutation\n got: %#v\nwant: %#v", got, want)
			}
		})
	}
}

// anyPayload mixes the shapes Any decodes: a nested object, an array of
// scalars with a nested array, ints, floats and a large int, strings without
// escapes (so they alias the input), booleans and null.
var anyPayload = []byte(`{
  "name": "alice",
  "age": 30,
  "active": true,
  "score": 87.5,
  "big": 9007199254740993,
  "tags": ["go", "rust", "json"],
  "address": {"city": "Lviv", "zip": "79000"},
  "nested": [[1,2,3],[4,5,6]],
  "missing": null
}`)

func BenchmarkAny_stdlib(b *testing.B) {
	b.SetBytes(int64(len(anyPayload)))
	b.ReportAllocs()
	for b.Loop() {
		var v any
		if err := json.Unmarshal(anyPayload, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAny_scan(b *testing.B) {
	b.SetBytes(int64(len(anyPayload)))
	b.ReportAllocs()
	for b.Loop() {
		_, _, err := Any(anyPayload, 0, true)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAny_scanNumber(b *testing.B) {
	b.SetBytes(int64(len(anyPayload)))
	b.ReportAllocs()
	for b.Loop() {
		_, _, err := AnyNumber(anyPayload, 0, true)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAny_Shapes decodes each input in anyShapeInputs, the same shapes
// the AppendAny benchmark uses. Each shape has three rows: encoding/json,
// Any and AnyNumber.
func BenchmarkAny_Shapes(b *testing.B) {
	codecs := []struct {
		name string
		fn   func([]byte) error
	}{
		{"stdjson", func(p []byte) error { var v any; return json.Unmarshal(p, &v) }},
		{"ggen", func(p []byte) error { _, _, err := Any(p, 0, true); return err }},
		{"ggen_number", func(p []byte) error { _, _, err := AnyNumber(p, 0, true); return err }},
	}
	for _, sh := range anyShapeInputs {
		b.Run(sh.name, func(b *testing.B) {
			for _, c := range codecs {
				b.Run(c.name, func(b *testing.B) {
					b.SetBytes(int64(len(sh.payload)))
					b.ReportAllocs()
					for b.Loop() {
						if err := c.fn(sh.payload); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}

// anyShapeInputs is the input table for BenchmarkAny_Shapes. The JSON is
// written out here instead of being made from anyShapes, so the decode
// benchmarks do not depend on the encode fixtures.
var anyShapeInputs = func() []struct {
	name    string
	payload []byte
} {
	out := []struct {
		name    string
		payload []byte
	}{
		{"null", []byte(`null`)},
		{"bool", []byte(`true`)},
		{"string", []byte(`"hello, ggen benchmark world!"`)},
		{"int", []byte(`42`)},
		{"int64", []byte(`1125899906842624`)},
		{"float64", []byte(`3.14159`)},
		// More digits than float64 can hold (15 to 17). Any parses it to
		// float64 and silently loses precision. AnyNumber keeps the exact
		// digits by aliasing the input. So the three rows differ in a
		// useful way here.
		{"large_number", []byte(`123456789012345.678901234567890123`)},
	}
	// Arrays of 32 elements of one type. Plain ints, floats and bools
	// keep the parser on its fast path, with no escapes or whitespace.
	var sb strings.Builder
	sb.WriteByte('[')
	for i := range 32 {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString("1234567890") // 10-digit positive int
	}
	sb.WriteByte(']')
	out = append(out, struct {
		name    string
		payload []byte
	}{"[]int_32", []byte(sb.String())})

	sb.Reset()
	sb.WriteByte('[')
	for i := range 32 {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString("3.141592653589793")
	}
	sb.WriteByte(']')
	out = append(out, struct {
		name    string
		payload []byte
	}{"[]float_32", []byte(sb.String())})

	sb.Reset()
	sb.WriteByte('[')
	for i := range 32 {
		if i > 0 {
			sb.WriteByte(',')
		}
		if i%2 == 0 {
			sb.WriteString("true")
		} else {
			sb.WriteString("false")
		}
	}
	sb.WriteByte(']')
	out = append(out, struct {
		name    string
		payload []byte
	}{"[]bool_32", []byte(sb.String())})

	sb.Reset()
	sb.WriteByte('[')
	for i := range 32 {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`"sampleString0123"`)
	}
	sb.WriteByte(']')
	out = append(out, struct {
		name    string
		payload []byte
	}{"[]string_32", []byte(sb.String())})

	// An object with 32 entries. It covers the per-key code, with the
	// same map building cost as the AppendAny side.
	sb.Reset()
	sb.WriteByte('{')
	for i := range 32 {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `"key%d":%d`, i, 1234567890+i)
	}
	sb.WriteByte('}')
	out = append(out, struct {
		name    string
		payload []byte
	}{"map[string]int_32", []byte(sb.String())})

	sb.Reset()
	sb.WriteByte('{')
	for i := range 32 {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `"key%d":%f`, i, 3.14159+float64(i))
	}
	sb.WriteByte('}')
	out = append(out, struct {
		name    string
		payload []byte
	}{"map[string]float_32", []byte(sb.String())})

	sb.Reset()
	sb.WriteByte('{')
	for i := range 32 {
		if i > 0 {
			sb.WriteByte(',')
		}
		if i%2 == 0 {
			fmt.Fprintf(&sb, `"key%d":true`, i)
		} else {
			fmt.Fprintf(&sb, `"key%d":false`, i)
		}
	}
	sb.WriteByte('}')
	out = append(out, struct {
		name    string
		payload []byte
	}{"map[string]bool_32", []byte(sb.String())})

	sb.Reset()
	sb.WriteByte('{')
	for i := range 32 {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `"key%d":"sampleString0123"`, i)
	}
	sb.WriteByte('}')
	out = append(out, struct {
		name    string
		payload []byte
	}{"map[string]string_32", []byte(sb.String())})

	return out
}()

// The BenchmarkAppendAny* benchmarks run AppendAny on the values an `any`
// field holds at runtime: scalars, and slices and maps of one primitive type
// with 32 entries each. Each shape has three rows: encoding/json v1, jsonv2
// and ggen. They show where ggen's fast paths for concrete types beat
// reflection.

// reparse decodes JSON into an any with jsonv2. Comparing reparsed values
// ignores map key order and other differences that do not change the
// meaning.
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
	got, err := AppendAny(nil, in)
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
	t.Parallel()
	out, err := AppendAny(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "null" {
		t.Errorf("nil → %q, want null", out)
	}
}

func TestAppendAny_Bool(t *testing.T) {
	t.Parallel()
	checkAny(t, true)
	checkAny(t, false)
}

func TestAppendAny_String(t *testing.T) {
	t.Parallel()
	checkAny(t, "")
	checkAny(t, "hello")
	checkAny(t, "tab\there")
	checkAny(t, "quote\"inside")
	checkAny(t, "<a href=\"x\">b & c</a>") // HTML characters
}

func TestAppendAny_Numbers(t *testing.T) {
	t.Parallel()
	checkAny(t, int(42))
	checkAny(t, int8(-7))
	checkAny(t, int16(-1234))
	checkAny(t, int32(-7))
	checkAny(t, int64(1<<60))
	checkAny(t, uint(0))
	checkAny(t, uint8(255))
	checkAny(t, byte(7)) // byte is uint8, tested on its own anyway
	checkAny(t, uint16(65535))
	checkAny(t, uint32(1))
	checkAny(t, uint64(1<<63))
	checkAny(t, float32(1.5))
	checkAny(t, float64(3.14159))
}

func TestAppendAny_JSONNumber(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"12345", "-0.5", "1e9", "0", "1E+2"} {
		out, err := AppendAny(nil, json.Number(ok))
		if err != nil || string(out) != ok {
			t.Errorf("json.Number(%q) → %q, %v", ok, out, err)
		}
	}
	// The zero value writes 0, like encoding/json v1. Writing nothing
	// would give {"n":} in a struct field, which is invalid JSON.
	// Non-empty text is written as is, without validation, the same
	// trust as RawMessage.
	out, err := AppendAny(nil, json.Number(""))
	if err != nil || string(out) != "0" {
		t.Errorf("zero json.Number → %q, %v, want 0", out, err)
	}
}

func TestAppendAny_Slice(t *testing.T) {
	t.Parallel()
	checkAny(t, []any{})
	checkAny(t, []any{1, "two", true, nil})
	checkAny(t, []any{[]any{1, 2}, []any{3, 4}})
}

func TestAppendAny_Map(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	// The standard library writes []byte as a base64 string. So must
	// AppendAny.
	checkAny(t, []byte("hello world"))
	checkAny(t, []byte{0xde, 0xad, 0xbe, 0xef})
}

func TestAppendAny_StringMap(t *testing.T) {
	t.Parallel()
	checkAny(t, map[string]string{"k": "v"})
	checkAny(t, map[string]string{"a": "1", "b": "2", "c": "3"})
}

func TestAppendAny_PrimitiveMaps(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	checkAny(t, map[string]any{
		"items": []any{
			map[string]any{"k": 1},
			map[string]any{"k": 2, "n": []any{nil, true, "x"}},
		},
	})
}

// A struct without tags uses the Go field names as keys.
func TestAppendAny_Struct_Bare(t *testing.T) {
	t.Parallel()
	type point struct {
		X, Y int
	}
	checkAny(t, point{X: 1, Y: 2})
}

// json tags: rename, skip with "-", omitempty and omitzero.
func TestAppendAny_Struct_Tags(t *testing.T) {
	t.Parallel()
	type tagged struct {
		Name    string `json:"name"`
		Skip    string `json:"-"`
		Empty   string `json:"empty,omitempty"`
		Zero    int    `json:"zero,omitzero"`
		Present string `json:"present,omitempty"`
	}
	checkAny(t, tagged{Name: "alice", Skip: "ignored", Present: "hi"})
}

// Pointer fields, and a pointer as the top-level value.
func TestAppendAny_Struct_Pointers(t *testing.T) {
	t.Parallel()
	type withPtr struct {
		Name *string `json:"name"`
		N    *int    `json:"n"`
	}
	name := "alice"
	n := 42
	checkAny(t, withPtr{Name: &name, N: &n})
	checkAny(t, withPtr{}) // both nil, so both null
	// A top-level pointer writes the value it points to.
	checkAny(t, &n)
	var nilP *int
	checkAny(t, nilP)
}

// Fields of an embedded struct are written as fields of the parent.
func TestAppendAny_Struct_Embedded(t *testing.T) {
	t.Parallel()
	type Base struct {
		ID   string `json:"id"`
		Meta string `json:"meta"`
	}
	type Derived struct {
		Base

		Name string `json:"name"`
	}
	checkAny(t, Derived{ID: "abc", Meta: "m", Name: "alice"})
}

// Fields reached through a nil embedded pointer are left out. AppendAny must
// not panic.
func TestAppendAny_Struct_NilEmbeddedPointer(t *testing.T) {
	t.Parallel()
	type Base struct {
		ID string `json:"id"`
	}
	type Derived struct {
		*Base

		Name string `json:"name"`
	}
	checkAny(t, Derived{Name: "alice"})
	checkAny(t, Derived{Base: &Base{ID: "abc"}, Name: "alice"})
}

// A field with the same name as an embedded field follows the standard
// library rules. The least nested field wins. At equal depth a tagged field
// beats an untagged one. If that still leaves a tie, the name is dropped.
// Only one key may be written, never both.
func TestAppendAny_Struct_EmbeddedShadowing(t *testing.T) {
	t.Parallel()
	type Base struct {
		ID   int    `json:"id"`
		Note string `json:"note"`
	}
	type Outer struct {
		Base

		ID int `json:"id"` // hides Base.ID
	}
	out, err := AppendAny(nil, Outer{Base: Base{ID: 1, Note: "n"}, ID: 2})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"note":"n","id":2}`; string(out) != want {
		t.Errorf("shadowed embed:\n got %s\nwant %s", out, want)
	}
	checkAny(t, Outer{Base: Base{ID: 1, Note: "n"}, ID: 2})

	// Two embedded structs with the same name at equal depth. That is a
	// tie, so the name is dropped.
	type Other struct {
		ID int `json:"id"`
	}
	type Clash struct {
		Base
		Other //nolint:govet

		Name string `json:"name"`
	}
	checkAny(t, Clash{Base: Base{ID: 1, Note: "n"}, Other: Other{ID: 2}, Name: "x"})
}

// A struct field inside a struct takes the recursive path.
func TestAppendAny_Struct_Nested(t *testing.T) {
	t.Parallel()
	type addr struct {
		City string `json:"city"`
	}
	type user struct {
		Name string `json:"name"`
		Addr addr   `json:"addr"`
	}
	checkAny(t, user{Name: "alice", Addr: addr{City: "Lviv"}})
}

// ,string quotes only numbers, also through one pointer, like generated code
// does. A bool stays unquoted. A string is encoded once, not quoted a second
// time. A nil *int stays an unquoted null. Stable jsonv2 in Go 1.27 rejects
// ,string on bool and string instead. That open divergence is tracked in
// .claude/backlog.md.
func TestAppendAny_Struct_StringOpt(t *testing.T) {
	t.Parallel()
	type quoted struct {
		N  int     `json:"n,string"`
		F  float64 `json:"f,string"`
		B  bool    `json:"b,string"`
		S  string  `json:"s,string"`
		P  *int    `json:"p,string"`
		NP *int    `json:"np,string"`
	}
	n := 7
	in := quoted{N: 42, F: 1.5, B: true, S: "x", P: &n}
	out, err := AppendAny(nil, in)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"n":"42","f":"1.5","b":true,"s":"x","p":"7","np":null}`
	if string(out) != want {
		t.Errorf(",string opt:\n got %s\nwant %s", out, want)
	}
	checkAny(t, in)
}

// A [N]byte value must be written as base64 without a panic. The array from
// reflect.ValueOf is not addressable, so the code cannot call Bytes() or
// Slice() on it.
func TestAppendAny_ByteArray(t *testing.T) {
	t.Parallel()
	out, err := AppendAny(nil, [4]byte{1, 2, 3, 4})
	if err != nil || string(out) != `"AQIDBA=="` {
		t.Errorf("[4]byte → %q, %v, want \"AQIDBA==\"", out, err)
	}
	checkAny(t, [4]byte{1, 2, 3, 4})
	type withSum struct {
		Sum [4]byte `json:"sum"`
	}
	checkAny(t, withSum{Sum: [4]byte{9, 8, 7, 6}})
	type namedArr [3]byte
	checkAny(t, namedArr{5, 5, 5})
}

func TestAppendAny_RawMessage(t *testing.T) {
	t.Parallel()
	// Non-empty bytes are written as is, with no quotes or escapes.
	checkAny(t, json.RawMessage(`{"a":1,"b":[true,null]}`))
	checkAny(t, json.RawMessage(`42`))
	checkAny(t, json.RawMessage(`"escaped\nstring"`))
	// nil and empty both write null, like encoding/json v1.
	out, err := AppendAny(nil, json.RawMessage(nil))
	if err != nil || string(out) != "null" {
		t.Errorf("nil RawMessage: got %q err %v, want null", out, err)
	}
	out, err = AppendAny(nil, json.RawMessage{})
	if err != nil || string(out) != "null" {
		t.Errorf("empty RawMessage: got %q err %v, want null", out, err)
	}
}

func TestAppendAny_Time(t *testing.T) {
	t.Parallel()
	// A fixed time, so the output never changes.
	ts := time.Date(2026, 5, 25, 12, 34, 56, 789_000_000, time.UTC)
	out, err := AppendAny(nil, ts)
	if err != nil {
		t.Fatal(err)
	}
	want, err := jsonv2.Marshal(ts)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, want) {
		t.Errorf("time.Time fast path: got %s, want %s", out, want)
	}
	// A non-nil *time.Time writes the same as the value.
	out, err = AppendAny(nil, &ts)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, want) {
		t.Errorf("*time.Time non-nil: got %s, want %s", out, want)
	}
	// A nil *time.Time writes null.
	out, err = AppendAny(nil, (*time.Time)(nil))
	if err != nil || string(out) != "null" {
		t.Errorf("*time.Time nil: got %q err %v, want null", out, err)
	}
}

func TestAppendAny_PrimitivePointers(t *testing.T) {
	t.Parallel()
	s := "hi"
	b := true
	i := 42
	i8 := int8(-1)
	i16 := int16(-1000)
	i32 := int32(7)
	i64 := int64(1 << 50)
	u := uint(1)
	u8 := uint8(255)
	u16 := uint16(65535)
	u32 := uint32(1 << 31)
	u64 := uint64(1 << 63)
	f32 := float32(1.5)
	f64 := 3.14
	checkAny(t, &s)
	checkAny(t, &b)
	checkAny(t, &i)
	checkAny(t, &i8)
	checkAny(t, &i16)
	checkAny(t, &i32)
	checkAny(t, &i64)
	checkAny(t, &u)
	checkAny(t, &u8)
	checkAny(t, &u16)
	checkAny(t, &u32)
	checkAny(t, &u64)
	checkAny(t, &f32)
	checkAny(t, &f64)
	// A nil pointer to any primitive kind writes null.
	checkAny(t, (*string)(nil))
	checkAny(t, (*bool)(nil))
	checkAny(t, (*int)(nil))
	checkAny(t, (*int64)(nil))
	checkAny(t, (*uint64)(nil))
	checkAny(t, (*float64)(nil))
}

// Named primitive types go through the primitive cases of the reflect
// path.
func TestAppendAny_NamedPrimitives(t *testing.T) {
	t.Parallel()
	type myString string
	type myInt int
	type myBool bool
	checkAny(t, myString("hi"))
	checkAny(t, myInt(42))
	checkAny(t, myBool(true))
}

// Unsupported kinds, like channels and funcs, return an error. They must not
// be skipped silently.
func TestAppendAny_Unsupported(t *testing.T) {
	t.Parallel()
	_, err := AppendAny(nil, make(chan int))
	if err == nil {
		t.Fatal("expected error for chan, got nil")
	}
	_, err = AppendAny(nil, func() {})
	if err == nil {
		t.Fatal("expected error for func, got nil")
	}
}

// Tests for values that implement a marshal interface.

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
	t.Parallel()
	out, err := AppendAny(nil, fakeAppendJSON{payload: `{"raw":1}`})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"raw":1}` {
		t.Errorf("AppendJSON path → %q", out)
	}
}

func TestAppendAny_JSONMarshaler(t *testing.T) {
	t.Parallel()
	out, err := AppendAny(nil, fakeJSONMarshaler{s: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"s":"hi"}` {
		t.Errorf("MarshalJSON path → %q", out)
	}
}

func TestAppendAny_TextAppender(t *testing.T) {
	t.Parallel()
	out, err := AppendAny(nil, fakeTextAppender{word: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"hello"` {
		t.Errorf("AppendText path → %q", out)
	}
}

func TestAppendAny_TextMarshaler(t *testing.T) {
	t.Parallel()
	out, err := AppendAny(nil, fakeTextMarshaler{word: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"hello"` {
		t.Errorf("MarshalText path → %q", out)
	}
}

// fakeTextAndJSON implements both encoding.TextAppender and json.Marshaler,
// with different outputs, so the test can see which one AppendAny calls.
type fakeTextAndJSON struct{}

func (fakeTextAndJSON) AppendText(dst []byte) ([]byte, error) {
	return append(dst, "from-text"...), nil
}

func (fakeTextAndJSON) MarshalJSON() ([]byte, error) {
	return []byte(`"from-json"`), nil
}

// A type that implements both must be written with AppendText, not
// MarshalJSON.
func TestAppendAny_TextAppenderOvertakesMarshalJSON(t *testing.T) {
	t.Parallel()
	out, err := AppendAny(nil, fakeTextAndJSON{})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"from-text"` {
		t.Errorf("expected TextAppender to win: got %q, want %q", out, `"from-text"`)
	}
}

// fakeTextMarshalerAndJSON implements both encoding.TextMarshaler and
// json.Marshaler. AppendAny must call MarshalText.
type fakeTextMarshalerAndJSON struct{}

func (fakeTextMarshalerAndJSON) MarshalText() ([]byte, error) {
	return []byte("from-text"), nil
}

func (fakeTextMarshalerAndJSON) MarshalJSON() ([]byte, error) {
	return []byte(`"from-json"`), nil
}

func TestAppendAny_TextMarshalerOvertakesMarshalJSON(t *testing.T) {
	t.Parallel()
	out, err := AppendAny(nil, fakeTextMarshalerAndJSON{})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"from-text"` {
		t.Errorf("expected TextMarshaler to win: got %q, want %q", out, `"from-text"`)
	}
}

func TestAppendAny_NonStringMapKey(t *testing.T) {
	t.Parallel()
	_, err := AppendAny(nil, map[int]string{1: "a"})
	if err == nil {
		t.Fatal("expected error for non-string map key, got nil")
	}
}

// AppendAny appends to the caller's dst and keeps what is already there.
func TestAppendAny_AppendsToDst(t *testing.T) {
	t.Parallel()
	dst := []byte("prefix:")
	out, err := AppendAny(dst, 42)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "prefix:42" {
		t.Errorf("AppendAny didn't append to dst — got %q", out)
	}
}

// anyShape pairs a benchmark shape's name with its value. enc is the jsonv2
// encoding, made once per shape. Its length is the byte count passed to
// SetBytes.
type anyShape struct {
	name string
	val  any
	enc  []byte
}

var anyShapes []anyShape

func init() {
	r := rand.New(rand.NewSource(42))
	const n = 32

	randString := func(n int) string {
		const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
		b := make([]byte, n)
		for i := range b {
			b[i] = letters[r.Intn(len(letters))]
		}
		return string(b)
	}

	mkSlice := func(fn func(int) any) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = fn(i)
		}
		return out
	}

	shapes := []anyShape{
		{name: "nil", val: nil},
		{name: "bool", val: true},
		{name: "string", val: "hello, ggen benchmark world!"},
		{name: "int", val: 42},
		{name: "int64", val: int64(1 << 50)},
		{name: "float64", val: 3.14159},
		{name: "json.Number", val: json.Number("12345.6789")},
		{name: "json.RawMessage", val: json.RawMessage(`{"raw":true,"n":[1,2,3]}`)},
		{name: "time.Time", val: time.Date(2026, 5, 25, 12, 34, 56, 789_000_000, time.UTC)},

		// Pointers to primitives, common for nullable fields in APIs.
		{name: "*string", val: new("pointed-at string value")},
		{name: "*int", val: new(42)},
		{name: "*int64", val: new(int64(1 << 50))},
		{name: "*float64", val: new(3.14159)},
		{name: "*bool", val: new(true)},

		{name: "[]int", val: func() []int {
			out := make([]int, n)
			for i := range out {
				out[i] = r.Intn(1<<30) - (1 << 29)
			}
			return out
		}()},
		{name: "[]int64", val: func() []int64 {
			out := make([]int64, n)
			for i := range out {
				out[i] = r.Int63n(1<<60) - (1 << 59)
			}
			return out
		}()},
		{name: "[]uint32", val: func() []uint32 {
			out := make([]uint32, n)
			for i := range out {
				out[i] = r.Uint32()
			}
			return out
		}()},
		{name: "[]float64", val: func() []float64 {
			out := make([]float64, n)
			for i := range out {
				out[i] = r.Float64() * 1000
			}
			return out
		}()},
		{name: "[]float32", val: func() []float32 {
			out := make([]float32, n)
			for i := range out {
				out[i] = float32(r.Float64() * 1000)
			}
			return out
		}()},
		{name: "[]bool", val: func() []bool {
			out := make([]bool, n)
			for i := range out {
				out[i] = r.Intn(2) == 0
			}
			return out
		}()},
		{name: "[]string", val: func() []string {
			out := make([]string, n)
			for i := range out {
				out[i] = randString(8 + r.Intn(16))
			}
			return out
		}()},
		{name: "[]any", val: mkSlice(func(i int) any {
			switch i % 4 {
			case 0:
				return r.Intn(1000)
			case 1:
				return r.Float64() * 100
			case 2:
				return randString(8 + r.Intn(16))
			default:
				return r.Intn(2) == 0
			}
		})},

		{name: "map[string]int", val: func() map[string]int {
			m := make(map[string]int, n)
			for i := range n {
				m["key"+strconv.Itoa(i)] = r.Intn(1<<30) - (1 << 29)
			}
			return m
		}()},
		{name: "map[string]int64", val: func() map[string]int64 {
			m := make(map[string]int64, n)
			for i := range n {
				m["key"+strconv.Itoa(i)] = r.Int63n(1<<60) - (1 << 59)
			}
			return m
		}()},
		{name: "map[string]uint32", val: func() map[string]uint32 {
			m := make(map[string]uint32, n)
			for i := range n {
				m["key"+strconv.Itoa(i)] = r.Uint32()
			}
			return m
		}()},
		{name: "map[string]float64", val: func() map[string]float64 {
			m := make(map[string]float64, n)
			for i := range n {
				m["key"+strconv.Itoa(i)] = r.Float64() * 1000
			}
			return m
		}()},
		{name: "map[string]float32", val: func() map[string]float32 {
			m := make(map[string]float32, n)
			for i := range n {
				m["key"+strconv.Itoa(i)] = float32(r.Float64() * 1000)
			}
			return m
		}()},
		{name: "map[string]bool", val: func() map[string]bool {
			m := make(map[string]bool, n)
			for i := range n {
				m["key"+strconv.Itoa(i)] = r.Intn(2) == 0
			}
			return m
		}()},
		{name: "map[string]string", val: func() map[string]string {
			m := make(map[string]string, n)
			for i := range n {
				m["key"+strconv.Itoa(i)] = randString(12 + r.Intn(20))
			}
			return m
		}()},
		{name: "map[string]any", val: func() map[string]any {
			m := make(map[string]any, n)
			for i := range n {
				switch i % 4 {
				case 0:
					m["key"+strconv.Itoa(i)] = r.Intn(1000)
				case 1:
					m["key"+strconv.Itoa(i)] = r.Float64() * 100
				case 2:
					m["key"+strconv.Itoa(i)] = randString(8 + r.Intn(16))
				default:
					m["key"+strconv.Itoa(i)] = r.Intn(2) == 0
				}
			}
			return m
		}()},

		{name: "named_map_int", val: func() namedIntMap {
			m := make(namedIntMap, n)
			for i := range n {
				m["key"+strconv.Itoa(i)] = r.Intn(1 << 30)
			}
			return m
		}()},
		{name: "struct_slice", val: func() []ptStruct {
			out := make([]ptStruct, n)
			for i := range out {
				out[i] = ptStruct{X: i, Y: -i, S: randString(8), B: i%2 == 0}
			}
			return out
		}()},
		{name: "time_slice", val: func() []time.Time {
			out := make([]time.Time, n)
			base := time.Date(2026, 6, 27, 0, 0, 0, 0, time.UTC)
			for i := range out {
				out[i] = base.Add(time.Duration(i) * time.Hour)
			}
			return out
		}()},
	}

	for i := range shapes {
		enc, err := jsonv2.Marshal(shapes[i].val)
		if err != nil {
			panic(err)
		}
		shapes[i].enc = enc
	}
	anyShapes = shapes
}

// BenchmarkAppendAny marshals an `any` value with three encoders. ggen uses
// AppendAny. The others use their Marshal function. MB/s uses the length of
// the jsonv2 output for every row, so rows of the same shape compare
// fairly.
func BenchmarkAppendAny(b *testing.B) {
	codecs := []struct {
		name string
		fn   func(v any) ([]byte, error)
	}{
		{"stdjson", json.Marshal},
		{"jsonv2", func(v any) ([]byte, error) { return jsonv2.Marshal(v) }},
		{"ggen", func(v any) ([]byte, error) { return AppendAny(nil, v) }},
	}
	for _, sh := range anyShapes {
		b.Run(sh.name, func(b *testing.B) {
			for _, c := range codecs {
				b.Run(c.name, func(b *testing.B) {
					b.SetBytes(int64(len(sh.enc)))
					b.ReportAllocs()
					for b.Loop() {
						if _, err := c.fn(sh.val); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}

// BenchmarkAppendAny_Presized runs only ggen and reuses one buffer across
// calls. This removes the cost of allocating the output, and leaves the cost
// of picking the right code path and writing the value.
func BenchmarkAppendAny_Presized(b *testing.B) {
	for _, sh := range anyShapes {
		b.Run(sh.name, func(b *testing.B) {
			cap0 := 64
			if c := len(sh.enc) * 2; c > cap0 {
				cap0 = c
			}
			buf := make([]byte, 0, cap0)
			b.SetBytes(int64(len(sh.enc)))
			b.ReportAllocs()
			for b.Loop() {
				out, err := AppendAny(buf[:0], sh.val)
				if err != nil {
					b.Fatal(err)
				}
				buf = out[:0]
			}
		})
	}
}

type namedStr string

type (
	namedIntMap map[string]int
	namedStrMap map[string]string
)

type ptStruct struct {
	X int    `json:"x"`
	Y int    `json:"y"`
	S string `json:"s"`
	B bool   `json:"b"`
}

// TestAppendAny_ReflectHeavy covers shapes that go through reflect: named
// maps, slices of composite elements and struct fields. Each is checked
// against jsonv2, ignoring map key order.
func TestAppendAny_ReflectHeavy(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 27, 1, 2, 3, 0, time.UTC)
	checkAny(t, namedIntMap{"a": 1, "b": -2, "c": 1 << 40})
	checkAny(t, namedStrMap{"x": "one", "y": "two"})
	checkAny(t, namedIntMap(nil))
	checkAny(t, map[string]ptStruct{"p": {X: 1, Y: -1, S: "hi", B: true}})
	checkAny(t, []time.Time{now, now.Add(time.Hour)})
	checkAny(t, []time.Time(nil))
	checkAny(t, []json.RawMessage{json.RawMessage(`{"a":1}`), json.RawMessage(`[2,3]`), nil})
	checkAny(t, ptStruct{X: 7, Y: -7, S: "field", B: true})
	checkAny(t, []ptStruct{{X: 1, S: "a"}, {Y: 2, B: true}})
	checkAny(t, map[string][]int{"ns": {1, 2, 3}})
}

// TestAppendAny_NoHTMLEscapeDefault checks that AppendAny does not escape <, >
// and & by default, like jsonv2 and generated string fields. The test
// compares raw bytes. checkAny compares decoded values, which would hide a
// difference in escaping.
func TestAppendAny_NoHTMLEscapeDefault(t *testing.T) {
	t.Parallel()
	cases := []any{
		`<a href="x">b & c</a>`,
		[]string{"<&>"},
		map[string]string{"<k>": "<v>"},
		map[string]any{"<k>": "<v>"},
		namedStr("<n>"),
		[]any{"<e>"},
		struct {
			A string `json:"a"`
		}{A: "<x>"},
	}
	for _, v := range cases {
		got, err := AppendAny(nil, v)
		if err != nil {
			t.Errorf("AppendAny(%#v): %v", v, err)
			continue
		}
		want, err := jsonv2.Marshal(v)
		if err != nil {
			t.Fatalf("jsonv2.Marshal(%#v): %v", v, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("AppendAny escaping mismatch\n in:   %#v\n got:  %s\n want: %s", v, got, want)
		}
	}
}

// quotedAppender returns text with `"`, `\` and a control byte. AppendAny
// must escape the AppendText output, the same as it does for MarshalText.
type quotedAppender struct{ s string }

func (q quotedAppender) AppendText(b []byte) ([]byte, error) { return append(b, q.s...), nil }

func TestAppendAny_TextAppenderEscapes(t *testing.T) {
	t.Parallel()
	out, err := AppendAny(nil, quotedAppender{s: "a\"b\\c\nd"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `"a\"b\\c\nd"`; string(out) != want {
		t.Errorf("got %s want %s", out, want)
	}
	if !json.Valid(out) {
		t.Errorf("invalid JSON: %s", out)
	}
	// Text with nothing to escape is copied as is.
	out, _ = AppendAny(nil, quotedAppender{s: "plain"})
	if string(out) != `"plain"` {
		t.Errorf("clean: got %s", out)
	}
	// The HTML version also escapes <, > and &.
	out, _ = AppendAnyHTML(nil, quotedAppender{s: "a<b"})
	if string(out) != `"a\u003cb"` {
		t.Errorf("html: got %s", out)
	}
}

// Elements of a named type inside a container must get the same handling as
// a top-level value. json.Number stays unquoted, and a named primitive's
// marshal method is called. Choosing the code path by the element's kind
// alone would skip both.
type levelInt int

func (l levelInt) MarshalJSON() ([]byte, error) {
	return []byte(`"L` + strconv.Itoa(int(l)) + `"`), nil
}

func TestAppendAny_NamedElemsInContainers(t *testing.T) {
	t.Parallel()
	out, err := AppendAny(nil, []json.Number{"1", "2.5"})
	if err != nil || string(out) != `[1,2.5]` {
		t.Errorf("[]json.Number → %s, %v", out, err)
	}
	out, _ = AppendAny(nil, map[string]json.Number{"k": "7"})
	if string(out) != `{"k":7}` {
		t.Errorf("map json.Number → %s", out)
	}
	out, err = AppendAny(nil, []levelInt{1, 2})
	if err != nil || string(out) != `["L1","L2"]` {
		t.Errorf("[]levelInt → %s, %v (marshaler bypassed)", out, err)
	}
	checkAny(t, []json.Number{"1", "2.5"})
	checkAny(t, []levelInt{1, 2})
}

// A MarshalJSON that returns (nil, nil) must give an error. Writing nothing
// would leave invalid JSON like `{"k":` with no error.
type emptyMarshaler struct{}

func (emptyMarshaler) MarshalJSON() ([]byte, error) { return nil, nil }

func TestAppendAny_EmptyMarshalJSON(t *testing.T) {
	t.Parallel()
	if _, err := AppendAny(nil, emptyMarshaler{}); !errors.Is(err, ErrEmptyMarshalJSON) {
		t.Errorf("got %v, want ErrEmptyMarshalJSON (stdlib v1/v2 both error)", err)
	}
}

// big.Int has an AppendText method, so the TextAppender path would quote its
// digits. But encoding/json v1, jsonv2 and ggen's generated big.Int fields
// all write an unquoted number. So AppendAny must check for concrete types
// before it checks for interfaces.
func TestAppendAny_BigIntStaysNumeric(t *testing.T) {
	t.Parallel()
	for _, v := range []any{
		big.NewInt(123),
		*big.NewInt(123),
		map[string]*big.Int{"a": big.NewInt(7)},
		[]*big.Int{big.NewInt(7)},
		struct {
			I *big.Int `json:"i"`
		}{big.NewInt(9)},
	} {
		got, err := AppendAny(nil, v)
		if err != nil {
			t.Fatalf("%T: %v", v, err)
		}
		want, err := jsonv2.Marshal(v)
		if err != nil {
			t.Fatalf("%T: jsonv2: %v", v, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%T: ggen %s, jsonv2 %s", v, got, want)
		}
	}
	var np *big.Int
	if out, _ := AppendAny(nil, np); string(out) != "null" {
		t.Errorf("nil *big.Int → %s, want null", out)
	}
}

// json.Number is a named string type whose JSON form is a number. Under
// `,string` it is quoted, like float64. A plain string is encoded once. Stable
// jsonv2 in Go 1.27 rejects `,string` on a plain string. That open
// divergence is tracked in .claude/backlog.md.
func TestAppendAny_NumberStringTag(t *testing.T) {
	t.Parallel()
	v := struct {
		N json.Number `json:"n,string"`
		F float64     `json:"f,string"`
		S string      `json:"s,string"`
	}{N: "12", F: 1.5, S: "x"}
	got, err := AppendAny(nil, v)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := jsonv2.Marshal(v)
	if !bytes.Equal(got, want) {
		t.Errorf("ggen %s, jsonv2 %s", got, want)
	}
}

// A struct that embeds a pointer to itself must not make collectFields
// recurse forever. That would overflow the stack, a fatal crash. The
// standard library stops at the cycle and writes the fields it can reach.
func TestAppendAny_RecursiveEmbedNoOverflow(t *testing.T) {
	t.Parallel()
	type node struct {
		*node //nolint:unused

		X int
	}
	got, err := AppendAny(nil, node{X: 1})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"X":1}` {
		t.Errorf("got %s, want {\"X\":1}", got)
	}
}

// A value that contains itself must stop with ErrMaxDepth. Without the
// depth limit it would recurse until a fatal stack overflow.
func TestAppendAny_CyclicValue(t *testing.T) {
	t.Parallel()
	s := make([]any, 1)
	s[0] = s
	if _, err := AppendAny(nil, s); !errors.Is(err, ErrMaxDepth) {
		t.Errorf("slice cycle: want ErrMaxDepth, got %v", err)
	}

	m := map[string]any{}
	m["m"] = m
	if _, err := AppendAny(nil, m); !errors.Is(err, ErrMaxDepth) {
		t.Errorf("map cycle: want ErrMaxDepth, got %v", err)
	}

	type node struct {
		Next *node
	}
	n := &node{}
	n.Next = n
	if _, err := AppendAny(nil, n); !errors.Is(err, ErrMaxDepth) {
		t.Errorf("pointer cycle: want ErrMaxDepth, got %v", err)
	}
}

func TestAppendAny_DepthCap(t *testing.T) {
	t.Parallel()
	var under any = 1
	for range maxDepth - 1 {
		under = []any{under}
	}
	if _, err := AppendAny(nil, under); err != nil {
		t.Errorf("under cap: %v", err)
	}
	over := []any{under}
	for range 2 {
		over = []any{over}
	}
	if _, err := AppendAny(nil, over); !errors.Is(err, ErrMaxDepth) {
		t.Errorf("over cap: want ErrMaxDepth, got %v", err)
	}
}

// The encode depth limit counts recursion levels: containers, pointers and
// interfaces. The decode limit counts only containers. The two could
// disagree right at the limit, leaving a value ggen decodes but cannot
// encode. Anything that decodes must encode again.
func TestAppendAny_DepthCapMatchesDecode(t *testing.T) {
	t.Parallel()
	shapes := map[string]func(int) []byte{
		"array":  func(n int) []byte { return []byte(strings.Repeat("[", n) + strings.Repeat("]", n)) },
		"object": func(n int) []byte { return []byte(strings.Repeat(`{"k":`, n) + "1" + strings.Repeat("}", n)) },
	}
	for name, mk := range shapes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, n := range []int{maxDepth - 1, maxDepth} {
				v, _, err := Any(mk(n), 0, true)
				if err != nil {
					t.Fatalf("depth %d: decode rejected below the cap: %v", n, err)
				}
				if _, err := AppendAny(nil, v); err != nil {
					t.Errorf("depth %d: decodes but does not re-encode: %v", n, err)
				}
			}
			if _, _, err := Any(mk(maxDepth+1), 0, true); !errors.Is(err, ErrMaxDepth) {
				t.Errorf("depth %d: want ErrMaxDepth, got %v", maxDepth+1, err)
			}
		})
	}
}

// A marshal method declared on *T is called for a T value wherever it is.
// That includes addressable places, like a struct field, a slice or array
// element and a map value. It also includes places that are not
// addressable, like a top-level value or an element of []any or
// map[string]any. This is what jsonv2 does. encoding/json v1 skips such
// methods on values that are not addressable, so a big.Rat value inside an
// any comes out as {}.
type r10PtrJSON struct{ V int }

func (p *r10PtrJSON) MarshalJSON() ([]byte, error) { return []byte(`"ptr-json"`), nil }

type r10PtrText struct{ V int }

func (p *r10PtrText) MarshalText() ([]byte, error) { return []byte("ptr-text"), nil }

type r10PtrLevel int

func (l *r10PtrLevel) MarshalText() ([]byte, error) { return []byte("lvl"), nil }

func TestAppendAny_PointerReceiverMarshalers(t *testing.T) {
	t.Parallel()
	type holder struct {
		M r10PtrJSON  `json:"m"`
		T r10PtrText  `json:"t"`
		L r10PtrLevel `json:"l"`
	}
	for _, v := range []any{
		r10PtrJSON{1},
		r10PtrText{1},
		r10PtrLevel(1),
		holder{M: r10PtrJSON{1}, T: r10PtrText{2}, L: 3},
		&holder{M: r10PtrJSON{1}, T: r10PtrText{2}, L: 3},
		[]r10PtrJSON{{1}, {2}},
		[2]r10PtrText{{1}, {2}},
		[]r10PtrLevel{1, 2},
		map[string]r10PtrJSON{"k": {1}},
		[]any{r10PtrJSON{1}, r10PtrText{2}, r10PtrLevel(3)},
		map[string]any{"k": r10PtrJSON{1}},
		*big.NewRat(1, 2),
		*big.NewFloat(1.5),
		[]any{*big.NewRat(1, 2)},
		map[string]any{"k": *big.NewFloat(1.5)},
	} {
		got, err := AppendAny(nil, v)
		if err != nil {
			t.Fatalf("%T: %v", v, err)
		}
		want, err := jsonv2.Marshal(v)
		if err != nil {
			t.Fatalf("%T: jsonv2: %v", v, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%T:\n ggen   %s\n jsonv2 %s", v, got, want)
		}
	}
}

// omitempty drops a field whose encoded value is null, "", {} or []. It never
// drops a zero number or false. It always drops an empty map, slice or
// string, and a raw or marshaled value that came out empty. jsonv2 agrees on
// every shape here. Struct fields are the one difference, tested in the next
// test.
func TestAppendAny_OmitEmptyJSONv2Semantics(t *testing.T) {
	t.Parallel()
	type inner struct {
		S string `json:"s,omitempty"`
	}
	type s struct {
		N  int             `json:"n,omitempty"`
		B  bool            `json:"b,omitempty"`
		F  float64         `json:"f,omitempty"`
		St struct{}        `json:"st"`
		In inner           `json:"in"`
		P  *int            `json:"p,omitempty"`
		A  []int           `json:"a,omitempty"`
		M  map[string]int  `json:"m,omitempty"`
		S  string          `json:"s,omitempty"`
		I  any             `json:"i,omitempty"`
		Ar [0]int          `json:"ar,omitempty"`
		R  json.RawMessage `json:"r,omitempty"`
		Q  string          `json:"q,omitempty"`
	}
	n := 0
	for _, v := range []any{
		s{},
		s{
			N: 1, B: true, F: 0.5, In: inner{S: "x"}, P: &n, A: []int{}, M: map[string]int{"k": 0},
			S: "s", I: map[string]any{}, R: json.RawMessage(`{}`), Q: `"`,
		},
		s{A: []int{0}, I: []any{}, R: json.RawMessage(`null`)},
	} {
		got, err := AppendAny(nil, v)
		if err != nil {
			t.Fatal(err)
		}
		want, err := jsonv2.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%+v:\n ggen   %s\n jsonv2 %s", v, got, want)
		}
	}
}

// omitempty never drops a struct. It has no meaning there, which is why the
// generator rejects the option on a struct field. A nil pointer or a nil
// interface is still dropped, because the pointer is missing, not the struct
// empty. This differs from jsonv2 on purpose. jsonv2 drops a struct that
// encodes as {}.
func TestAppendAny_OmitEmptyKeepsStruct(t *testing.T) {
	t.Parallel()
	type inner struct {
		S string `json:"s,omitempty"`
	}
	type s struct {
		St struct{}       `json:"st"`
		In inner          `json:"in"`
		P  *inner         `json:"p,omitempty"`
		I  any            `json:"i,omitempty"`
		M  map[string]int `json:"m,omitempty"`
	}
	cases := []struct {
		name string
		v    s
		want string
	}{
		{"all_empty", s{}, `{"st":{},"in":{}}`},
		{"nil_pointer_omits", s{P: nil}, `{"st":{},"in":{}}`},
		{"empty_pointee_kept", s{P: &inner{}}, `{"st":{},"in":{},"p":{}}`},
		{"any_struct_kept", s{I: inner{}}, `{"st":{},"in":{},"i":{}}`},
		{"any_nil_pointer_omits", s{I: (*inner)(nil)}, `{"st":{},"in":{}}`},
		{"empty_map_omits", s{M: map[string]int{}}, `{"st":{},"in":{}}`},
		{"populated", s{In: inner{S: "x"}, M: map[string]int{"k": 1}}, `{"st":{},"in":{"s":"x"},"m":{"k":1}}`},
	}
	for _, c := range cases {
		got, err := AppendAny(nil, c.v)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if string(got) != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.name, got, c.want)
		}
	}
}

// omitzero calls an IsZero method when there is one. It looks on the field
// type, on the pointer type, and on the value an interface field holds. It
// checks pointers and interfaces for nil first. Without an IsZero method it
// compares the value to its zero value. This is what jsonv2 does. A zero
// time.Time with a location is the standard library example: it is not the
// zero value, but IsZero() returns true.
type r10Zeroer struct{ A, B int }

func (z r10Zeroer) IsZero() bool { return z.A == z.B }

type r10PtrZeroer struct{ A int }

func (z *r10PtrZeroer) IsZero() bool { return z.A < 0 }

func TestAppendAny_OmitZeroIsZeroMethod(t *testing.T) {
	t.Parallel()
	type s struct {
		Z  r10Zeroer    `json:"z,omitzero"`
		PZ *r10Zeroer   `json:"pz,omitzero"`
		AZ r10PtrZeroer `json:"az,omitzero"`
		IZ isZeroer     `json:"iz,omitzero"`
		T  time.Time    `json:"t,omitzero"`
		N  int          `json:"n,omitzero"`
	}
	for _, v := range []any{
		s{},
		s{
			Z: r10Zeroer{3, 3}, PZ: &r10Zeroer{1, 1}, AZ: r10PtrZeroer{-1}, IZ: r10Zeroer{2, 2},
			T: time.Time{}.In(time.FixedZone("X", 0)),
		},
		s{
			Z: r10Zeroer{1, 2}, PZ: &r10Zeroer{1, 2}, AZ: r10PtrZeroer{5}, IZ: (*r10Zeroer)(nil),
			T: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), N: 1,
		},
		&s{Z: r10Zeroer{1, 2}, IZ: &r10Zeroer{1, 2}},
	} {
		got, err := AppendAny(nil, v)
		if err != nil {
			t.Fatal(err)
		}
		want, err := jsonv2.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%+v:\n ggen   %s\n jsonv2 %s", v, got, want)
		}
	}
}

// A catch-all map is marked `json:",embed"`, as in jsonv2. Its entries are
// written into the parent object, after every named field, wherever the map
// is declared (see embedFirst). Generated code uses the same order.
// `,inline` is not a known option, so that map stays under its own key, as
// in jsonv2.
func TestAppendAny_EmbedSplices(t *testing.T) {
	t.Parallel()
	type embed struct {
		A     int            `json:"a"`
		Extra map[string]any `json:",embed"`
	}
	type embedFirst struct {
		Extra map[string]any `json:",embed"`
		A     int            `json:"a"`
		B     int            `json:"b"`
	}
	type inline struct {
		A     int            `json:"a"`
		Extra map[string]any `json:",inline"`
	}
	for _, v := range []any{
		embed{A: 1, Extra: map[string]any{"x": 2.0}},
		embedFirst{A: 1, B: 3, Extra: map[string]any{"x": 2.0}},
		inline{A: 1, Extra: map[string]any{"x": 2.0}},
	} {
		got, err := AppendAny(nil, v)
		if err != nil {
			t.Fatal(err)
		}
		want, err := jsonv2.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%T:\n ggen   %s\n jsonv2 %s", v, got, want)
		}
	}
}

// A map key goes through a text marshal method on the key type, with a value
// or pointer receiver. The result is escaped like any other key. A plain
// string key uses its bytes. A key type with such a method does not need to
// be a string. Only a non-string key without one is rejected (see
// TestAppendAny_NonStringMapKey).
type r10TextKey string

func (k r10TextKey) MarshalText() ([]byte, error) { return []byte("T:" + string(k)), nil }

type r10AppendKey string

func (k *r10AppendKey) AppendText(b []byte) ([]byte, error) {
	return append(append(b, "A:"...), *k...), nil
}

type r10IntKey int

func (k r10IntKey) MarshalText() ([]byte, error) { return []byte("i" + strconv.Itoa(int(k))), nil }

type r10PlainKey string

func TestAppendAny_MapKeyTextMarshaler(t *testing.T) {
	t.Parallel()
	for _, v := range []any{
		map[r10TextKey]int{"a": 1},
		map[r10AppendKey]int{"a": 1},
		map[r10IntKey]int{7: 1},
		map[r10PlainKey]int{"a": 1},
		map[r10TextKey]string{`q"\`: "v"},
		map[r10AppendKey]bool{"<x>": true},
	} {
		got, err := AppendAny(nil, v)
		if err != nil {
			t.Fatalf("%T: %v", v, err)
		}
		want, err := jsonv2.Marshal(v)
		if err != nil {
			t.Fatalf("%T: jsonv2: %v", v, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%T:\n ggen   %s\n jsonv2 %s", v, got, want)
		}
	}
	// HTML mode escapes the key text like every other key.
	got, err := AppendAnyHTML(nil, map[r10AppendKey]bool{"<x>": true})
	if err != nil || string(got) != `{"A:\u003cx\u003e":true}` {
		t.Errorf("html key: got %s err %v", got, err)
	}
}

// A time.Duration inside an any is written as a string with units, like
// "1m30s". A generated Duration field writes the same by default, so one
// document never has two forms of one Go type. jsonv2 has no format for
// Duration and returns an error, so there is no reference to compare with.
// `,string` does not quote it a second time, since it is already a string.
func TestAppendAny_DurationUnits(t *testing.T) {
	t.Parallel()
	d := 90 * time.Second
	for _, tc := range []struct {
		in   any
		want string
	}{
		{d, `"1m30s"`},
		{&d, `"1m30s"`},
		{(*time.Duration)(nil), `null`},
		{[]time.Duration{time.Second, 0}, `["1s","0s"]`},
		{map[string]time.Duration{"d": time.Minute}, `{"d":"1m0s"}`},
		{[]any{d}, `["1m30s"]`},
		{struct {
			D time.Duration `json:"d,string"`
		}{time.Hour}, `{"d":"1h0m0s"}`},
		{struct {
			D time.Duration `json:"d,omitzero"`
		}{}, `{}`},
	} {
		got, err := AppendAny(nil, tc.in)
		if err != nil {
			t.Fatalf("%T: %v", tc.in, err)
		}
		if string(got) != tc.want {
			t.Errorf("%T: got %s, want %s", tc.in, got, tc.want)
		}
	}
}

type sizeText struct{ s string }

func (t sizeText) MarshalText() ([]byte, error) { return []byte(t.s), nil }

type sizeJSON struct{ n int }

func (j sizeJSON) MarshalJSON() ([]byte, error) { return []byte(strings.Repeat("[1]", j.n)), nil }

type sizeKey struct{ id int }

func (k sizeKey) MarshalText() ([]byte, error) { return []byte("key-" + strconv.Itoa(k.id)), nil }

type sizeInner struct {
	A string  `json:"a"`
	B []int   `json:"b,omitempty"`
	C *string `json:"c,omitzero"`
}

type sizeOuter struct {
	sizeInner

	Name   string          `json:"na<me>"`
	N      int64           `json:"n,string"`
	F      float64         `json:"f,omitempty"`
	P      *sizeInner      `json:"p"`
	Any    any             `json:"any"`
	Rows   []sizeInner     `json:"rows"`
	Keys   map[sizeKey]int `json:"keys"`
	Extra  map[string]any  `json:",embed"`
	hidden int             //nolint:unused
}

// AnySize must never be smaller than what AppendAny writes, for every kind of
// value and in both escape modes. Control bytes and invalid UTF-8 are left
// out. JSONSize sizes strings for valid JSON text only, the same as for typed
// string fields.
func TestAnySize_BoundsAppendAny(t *testing.T) {
	t.Parallel()
	str := "q\"b\\n\n<>&h\u00e9😀"
	long := strings.Repeat(str, 40)
	ptrStr, ptrInt, ptrF := long, math.MinInt64, -math.MaxFloat64
	bigHuge := new(big.Int).Exp(big.NewInt(10), big.NewInt(300), nil)
	bigNeg := new(big.Int).Neg(bigHuge)
	bigF := *big.NewFloat(-1.2345678901234567e300)
	when := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.FixedZone("", -7*3600))
	type myInt int
	type myStr string
	type myBytes []byte
	cases := []any{
		nil, true, false, "", str, long, json.Number(""), json.Number("-123.456e-789"),
		math.MaxFloat64, -math.SmallestNonzeroFloat64, 1e21, 999999999999999900000.0, -0.0000012345678901234567,
		float32(math.MaxFloat32), int64(math.MinInt64), uint64(math.MaxUint64), int8(-128),
		[]any{nil, str, 1.5, []any{map[string]any{long: []any{true}}}},
		map[string]any{str: map[string]any{long: []string{str, ""}}},
		[]string{str, "", long},
		[]int{math.MinInt, 0},
		[]uint64{math.MaxUint64},
		[]float64{-math.MaxFloat64},
		[]bool{false, true},
		[]time.Time{when, {}},
		[]json.RawMessage{nil, json.RawMessage(`{"a":[1,2]}`)},
		map[string]string{str: long},
		map[string]int{long: math.MinInt},
		map[string]float64{str: -math.MaxFloat64},
		map[string]bool{str: false},
		map[string]uint8{"x": 255},
		json.RawMessage(nil), json.RawMessage(`[1,2,3]`),
		*bigHuge, bigNeg, (*big.Int)(nil), bigF,
		when, &when, (*time.Time)(nil), time.Duration(math.MinInt64),
		&ptrStr, (*string)(nil), &ptrInt, (*int)(nil), &ptrF,
		netip.MustParseAddr("fe80::1:2:3:4%eth0"), netip.MustParsePrefix("2001:db8::/32"),
		sizeText{long},
		sizeJSON{50},
		myInt(math.MinInt), myStr(long), myBytes(long),
		[16]byte{1, 2, 3},
		[]byte(nil), []byte(long),
		map[sizeKey]int{{7}: 1, {123456}: math.MinInt},
		sizeOuter{
			A: long, B: []int{math.MinInt}, C: &ptrStr,
			Name: str, N: math.MinInt64, F: -math.MaxFloat64,
			P: &sizeInner{A: str}, Any: []any{sizeText{str}, &when},
			Rows:  []sizeInner{{A: long}, {}},
			Keys:  map[sizeKey]int{{1}: 2},
			Extra: map[string]any{long: map[string]any{str: bigNeg}},
		},
		&sizeOuter{},
		[]sizeOuter{{}, {Name: long}},
	}
	for i, v := range cases {
		b, err := AppendAny(nil, v)
		if err != nil {
			t.Fatalf("case %d %T: AppendAny: %v", i, v, err)
		}
		if got := AnySize(v); len(b) > got {
			t.Errorf("case %d %T: AppendAny wrote %d bytes, AnySize reserved %d", i, v, len(b), got)
		}
		h, err := AppendAnyHTML(nil, v)
		if err != nil {
			t.Fatalf("case %d %T: AppendAnyHTML: %v", i, v, err)
		}
		if got := AnySizeHTML(v); len(h) > got {
			t.Errorf("case %d %T: AppendAnyHTML wrote %d bytes, AnySizeHTML reserved %d", i, v, len(h), got)
		}
	}
}
