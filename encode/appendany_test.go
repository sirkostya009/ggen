//go:build goexperiment.jsonv2

package encode

// BenchmarkAppendAny_* — encode.AppendAny across the shapes an `any`
// field holds at runtime (scalars, homogeneous primitive slices/maps),
// each container 32 entries. Three rows per shape: stdlib v1, stdlib v2,
// ggen — surfacing where ggen's concrete fast path beats reflection.

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
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
	checkAny(t, "<a href=\"x\">b & c</a>") // HTML-escape exercised
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
	checkAny(t, byte(7)) // alias for uint8 — same case, exercised explicitly
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
	// Zero value → 0 (v1 parity); the raw append used to emit ZERO bytes,
	// producing {"n":} in a struct field. Non-empty content passes verbatim
	// unvalidated — same trust as RawMessage.
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
	// Stdlib marshals []byte as a base64 string — match.
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

// Untagged exported struct — uses Go field names verbatim.
func TestAppendAny_Struct_Bare(t *testing.T) {
	t.Parallel()
	type point struct {
		X, Y int
	}
	checkAny(t, point{X: 1, Y: 2})
}

// json-tagged fields: rename + skip via "-" + omitempty + omitzero.
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

// Pointer fields and pointer-typed top-level value.
func TestAppendAny_Struct_Pointers(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// Fields promoted through a nil embedded pointer are omitted, not a panic.
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

// Field shadowing an embedded field: stdlib dominant-field rules — the
// shallowest wins, equal-depth tagged beats untagged, ambiguous names drop.
// Flattening used to emit BOTH keys.
func TestAppendAny_Struct_EmbeddedShadowing(t *testing.T) {
	t.Parallel()
	type Base struct {
		ID   int    `json:"id"`
		Note string `json:"note"`
	}
	type Outer struct {
		Base
		ID int `json:"id"` // shadows Base.ID
	}
	out, err := AppendAny(nil, Outer{Base: Base{ID: 1, Note: "n"}, ID: 2})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"note":"n","id":2}`; string(out) != want {
		t.Errorf("shadowed embed:\n got %s\nwant %s", out, want)
	}
	checkAny(t, Outer{Base: Base{ID: 1, Note: "n"}, ID: 2})

	// Two embeds carrying the same name at equal depth — ambiguous, dropped.
	type Other struct {
		ID int `json:"id"`
	}
	type Clash struct {
		Base
		Other
		Name string `json:"name"`
	}
	checkAny(t, Clash{Base: Base{ID: 1, Note: "n"}, Other: Other{ID: 2}, Name: "x"})
}

// Nested struct fields hit the recursion path.
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

// ,string quotes numeric kinds only (through one pointer level), matching
// generated code and jsonv2: bool stays bare, string keeps its single
// encoding (a bare wrap emitted invalid JSON), nil *int stays bare null.
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

// [N]byte through the any-walker used to panic: the base64 arm called
// Slice() on the unaddressable reflect.ValueOf array (the comment dodged
// Bytes()'s panic and walked into Slice()'s identical one).
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
	t.Parallel()
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
	t.Parallel()
	type myString string
	type myInt int
	type myBool bool
	checkAny(t, myString("hi"))
	checkAny(t, myInt(42))
	checkAny(t, myBool(true))
}

// Unsupported kinds (channels, funcs) error instead of silently skipping.
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

// TextAppender must outrank json.Marshaler so types implementing both
// route via AppendText.
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

// TextMarshaler must rank above json.Marshaler: types with both route
// via MarshalText.
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

// AppendJSON-style: caller-owned dst gets appended to (not replaced).
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

type namedStr string

type namedIntMap map[string]int
type namedStrMap map[string]string

type ptStruct struct {
	X int    `json:"x"`
	Y int    `json:"y"`
	S string `json:"s"`
	B bool   `json:"b"`
}

// TestAppendAny_ReflectHeavy covers the reflect-path shapes (named maps,
// composite-element slices, struct fields), each checked against jsonv2
// (reparsed, order-independent).
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

// TestAppendAny_NoHTMLEscapeDefault: AppendAny strings must use the package
// default escaping (jsonv2 shape — <, >, & literal), matching the sibling
// generated string fields. Raw-byte comparison: checkAny reparses decoded
// values and would mask escaping differences.
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
		if string(got) != string(want) {
			t.Errorf("AppendAny escaping mismatch\n in:   %#v\n got:  %s\n want: %s", v, got, want)
		}
	}
}

// quotedAppender's text carries `"`, `\`, and a ctrl byte — the TextAppender
// arm used to drop it raw between quotes (the TextMarshaler arm escaped).
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
	// Clean text stays on the raw fast path.
	out, _ = AppendAny(nil, quotedAppender{s: "plain"})
	if string(out) != `"plain"` {
		t.Errorf("clean: got %s", out)
	}
	// HTML variant escapes <>& too.
	out, _ = AppendAnyHTML(nil, quotedAppender{s: "a<b"})
	if string(out) != `"a\u003cb"` {
		t.Errorf("html: got %s", out)
	}
}

// Named element types box through the full switch: json.Number stays
// unquoted, and a named primitive's marshaler is honored — the reflect
// container walk used to route by KIND, silently bypassing both.
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

// A (nil, nil) MarshalJSON used to emit ZERO bytes — `{"k":` with nil error.
type emptyMarshaler struct{}

func (emptyMarshaler) MarshalJSON() ([]byte, error) { return nil, nil }

func TestAppendAny_EmptyMarshalJSON(t *testing.T) {
	t.Parallel()
	if _, err := AppendAny(nil, emptyMarshaler{}); !errors.Is(err, ErrEmptyMarshalJSON) {
		t.Errorf("got %v, want ErrEmptyMarshalJSON (stdlib v1/v2 both error)", err)
	}
}
