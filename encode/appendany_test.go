//go:build goexperiment.jsonv2

package encode

// BenchmarkAppendAny_* — encode.AppendAny across the shapes an `any`
// field actually holds at runtime: scalars (nil, bool, string, ints,
// floats, json.Number), homogeneous primitive slices ([]int*, []uint*,
// []float*, []bool, []string, []any), and homogeneous string-keyed
// maps (map[string]int*, …, map[string]float*, map[string]bool,
// map[string]string, map[string]any). Each container shape carries 32
// entries — small enough that per-call dispatch shows up, big enough
// that per-element costs aren't drowned by the framing.
//
// Each shape runs three rows: stdlib v1 (encoding/json), stdlib v2
// (encoding/json/v2), and ggen (encode.AppendAny). Cross-codec deltas
// surface where ggen has a concrete-type fast path vs where it falls
// through to the reflect.Map / reflect.Slice paths.

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"math/rand"
	"reflect"
	"strconv"
	"testing"
	"time"
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
	out, err := AppendAny(nil, nil)
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
	out, err := AppendAny(nil, json.Number("12345"))
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
	out, err := AppendAny(nil, quoted{N: 42, B: true})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"n":"42","b":"true"}` {
		t.Errorf(",string opt → %q", out)
	}
}

func TestAppendAny_RawMessage(t *testing.T) {
	// Non-empty bytes pass through verbatim — no quoting, no escape.
	checkAny(t, json.RawMessage(`{"a":1,"b":[true,null]}`))
	checkAny(t, json.RawMessage(`42`))
	checkAny(t, json.RawMessage(`"escaped\nstring"`))
	// nil and empty both become JSON null (stdlib v1 parity).
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
	// Fixed instant so output is byte-stable.
	ts := time.Date(2026, 5, 25, 12, 34, 56, 789_000_000, time.UTC)
	out, err := AppendAny(nil, ts)
	if err != nil {
		t.Fatal(err)
	}
	want, err := jsonv2.Marshal(ts)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(want) {
		t.Errorf("time.Time fast path: got %s, want %s", out, want)
	}
	// *time.Time non-nil: same wire shape as value.
	out, err = AppendAny(nil, &ts)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(want) {
		t.Errorf("*time.Time non-nil: got %s, want %s", out, want)
	}
	// *time.Time nil: null.
	out, err = AppendAny(nil, (*time.Time)(nil))
	if err != nil || string(out) != "null" {
		t.Errorf("*time.Time nil: got %q err %v, want null", out, err)
	}
}

func TestAppendAny_PrimitivePointers(t *testing.T) {
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
	// nil pointers of every primitive kind emit null.
	checkAny(t, (*string)(nil))
	checkAny(t, (*bool)(nil))
	checkAny(t, (*int)(nil))
	checkAny(t, (*int64)(nil))
	checkAny(t, (*uint64)(nil))
	checkAny(t, (*float64)(nil))
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
	_, err := AppendAny(nil, make(chan int))
	if err == nil {
		t.Fatal("expected error for chan, got nil")
	}
	_, err = AppendAny(nil, func() {})
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
	out, err := AppendAny(nil, fakeAppendJSON{payload: `{"raw":1}`})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"raw":1}` {
		t.Errorf("AppendJSON path → %q", out)
	}
}

func TestAppendAny_JSONMarshaler(t *testing.T) {
	out, err := AppendAny(nil, fakeJSONMarshaler{s: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"s":"hi"}` {
		t.Errorf("MarshalJSON path → %q", out)
	}
}

func TestAppendAny_TextAppender(t *testing.T) {
	out, err := AppendAny(nil, fakeTextAppender{word: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"hello"` {
		t.Errorf("AppendText path → %q", out)
	}
}

func TestAppendAny_TextMarshaler(t *testing.T) {
	out, err := AppendAny(nil, fakeTextMarshaler{word: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"hello"` {
		t.Errorf("MarshalText path → %q", out)
	}
}

// fakeTextAndJSON implements BOTH encoding.TextAppender and
// json.Marshaler with intentionally distinct outputs, so the test
// can observe which interface dispatch wins.
type fakeTextAndJSON struct{}

func (fakeTextAndJSON) AppendText(dst []byte) ([]byte, error) {
	return append(dst, "from-text"...), nil
}

func (fakeTextAndJSON) MarshalJSON() ([]byte, error) {
	return []byte(`"from-json"`), nil
}

// Confirms the case order in AppendAny: TextAppender (zero-alloc)
// must outrank json.Marshaler so types that implement both route
// via AppendText. If this flips, types like uuid.UUID (which often
// carry both hooks) would unexpectedly pay the MarshalJSON return
// alloc.
func TestAppendAny_TextAppenderOvertakesMarshalJSON(t *testing.T) {
	out, err := AppendAny(nil, fakeTextAndJSON{})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"from-text"` {
		t.Errorf("expected TextAppender to win: got %q, want %q", out, `"from-text"`)
	}
}

// Same check one step down: TextMarshaler (one alloc) still ranks
// above json.Marshaler. Types with only MarshalText + MarshalJSON
// should route via MarshalText.
type fakeTextMarshalerAndJSON struct{}

func (fakeTextMarshalerAndJSON) MarshalText() ([]byte, error) {
	return []byte("from-text"), nil
}

func (fakeTextMarshalerAndJSON) MarshalJSON() ([]byte, error) {
	return []byte(`"from-json"`), nil
}

func TestAppendAny_TextMarshalerOvertakesMarshalJSON(t *testing.T) {
	out, err := AppendAny(nil, fakeTextMarshalerAndJSON{})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"from-text"` {
		t.Errorf("expected TextMarshaler to win: got %q, want %q", out, `"from-text"`)
	}
}

func TestAppendAny_NonStringMapKey(t *testing.T) {
	_, err := AppendAny(nil, map[int]string{1: "a"})
	if err == nil {
		t.Fatal("expected error for non-string map key, got nil")
	}
}

// AppendJSON-style: caller-owned dst gets appended to (not replaced).
func TestAppendAny_AppendsToDst(t *testing.T) {
	dst := []byte("prefix:")
	out, err := AppendAny(dst, 42)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "prefix:42" {
		t.Errorf("AppendAny didn't append to dst — got %q", out)
	}
}

// anyShape pairs a benched shape's name with its value. Encoded bytes
// are produced once per shape via jsonv2 and reused as the SetBytes
// reference size.
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

		// Pointer-to-primitive — common in nullable scalar APIs.
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
				out[i] = uint32(r.Uint32())
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

// BenchmarkAppendAny — marshal an `any` value via three codecs. ggen
// goes through AppendAny; the rest go through their generic Marshal.
// MB/s is computed against the jsonv2-encoded byte length so the
// reported throughput is comparable across rows for the same shape.
func BenchmarkAppendAny(b *testing.B) {
	var codecs = []struct {
		name string
		fn   func(v any) ([]byte, error)
	}{
		{"stdjson", func(v any) ([]byte, error) { return json.Marshal(v) }},
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

// BenchmarkAppendAny_Presized — ggen-only variant that reuses a
// caller-owned buffer. Isolates per-call output-buffer allocation
// from the dispatch path itself, surfacing the steady-state cost of
// the type-switch + per-shape emit.
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
