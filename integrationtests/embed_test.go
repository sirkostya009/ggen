package integrationtests

//go:generate ../ggen $GOFILE

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen"
)

// EmbedStruct exercises the json:",embed" fallback: unknown keys absorbed
// into Extra, spliced back out at the object level on marshal.
//
//ggen:generate
type EmbedStruct struct {
	Name  string         `json:"name"`
	Extra map[string]any `json:",embed"`
}

//ggen:generate
type EmbedStringsStruct struct {
	Name  string            `json:"name"`
	Extra map[string]string `json:",embed"`
}

//ggen:generate
type EmbedStructsStruct struct {
	Name  string                 `json:"name"`
	Extra map[string]EmbedStruct `json:",embed"`
}

//ggen:generate
type EmbedRawStruct struct {
	Name  string                     `json:"name"`
	Extra map[string]json.RawMessage `json:",embed"`
}

func TestEmbed_decodeAbsorbsUnknown(t *testing.T) {
	t.Parallel()
	in := []byte(`{"name":"alice","age":30,"city":"Lviv","tags":["a","b"]}`)
	got, _, err := EmbedStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Name != "alice" {
		t.Errorf("Name = %q", got.Name)
	}
	if len(got.Extra) != 3 {
		t.Fatalf("Extra len = %d, want 3: %+v", len(got.Extra), got.Extra)
	}
	// bare numbers decode into float64 for any.
	if age, _ := got.Extra["age"].(float64); age != 30 {
		t.Errorf("Extra[age] = %v", got.Extra["age"])
	}
	if city, _ := got.Extra["city"].(string); city != "Lviv" {
		t.Errorf("Extra[city] = %v", got.Extra["city"])
	}
	tags, ok := got.Extra["tags"].([]any)
	if !ok || len(tags) != 2 {
		t.Errorf("Extra[tags] = %#v", got.Extra["tags"])
	}
}

func TestEmbed_emptyDecode(t *testing.T) {
	t.Parallel()
	// No unknown keys → Extra stays nil.
	got, _, err := EmbedStruct{}.DecodeFrom([]byte(`{"name":"bob"}`))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Extra != nil {
		t.Errorf("Extra should be nil, got %+v", got.Extra)
	}
}

func TestEmbed_marshalSpreads(t *testing.T) {
	t.Parallel()
	s := EmbedStruct{
		Name:  "alice",
		Extra: map[string]any{"age": 30, "city": "Lviv"},
	}
	out, _ := ggen.MarshalString(s)
	for _, want := range []string{`"name":"alice"`, `"age":30`, `"city":"Lviv"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	// No nested object — extras should be at the top level.
	if strings.Contains(out, `"Extra"`) || strings.Contains(out, `{"age"`) && !strings.HasPrefix(out, `{"`) {
		t.Errorf("inline shouldn't be nested: %q", out)
	}
	// Must be a single top-level object.
	if !strings.HasPrefix(out, "{") || !strings.HasSuffix(out, "}") {
		t.Errorf("bad framing: %q", out)
	}
}

func TestEmbed_roundtrip(t *testing.T) {
	t.Parallel()
	orig := EmbedStruct{
		Name:  "alice",
		Extra: map[string]any{"age": float64(30), "city": "Lviv", "active": true},
	}
	out, _ := ggen.Marshal(orig)
	got, _, err := EmbedStruct{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if got.Name != orig.Name {
		t.Errorf("Name mismatch: %q vs %q", got.Name, orig.Name)
	}
	if len(got.Extra) != len(orig.Extra) {
		t.Fatalf("Extra len: got %d want %d\n%+v", len(got.Extra), len(orig.Extra), got.Extra)
	}
	for k, v := range orig.Extra {
		if got.Extra[k] != v {
			t.Errorf("Extra[%q] = %v want %v", k, got.Extra[k], v)
		}
	}
}

func TestEmbed_marshalEmpty(t *testing.T) {
	t.Parallel()
	s := EmbedStruct{Name: "alice"}
	out, _ := ggen.MarshalString(s)
	if out != `{"name":"alice"}` {
		t.Errorf("empty-extra marshal = %q", out)
	}
}

func TestEmbed_marshalOnlyExtras(t *testing.T) {
	t.Parallel()
	// Empty fixed field + inline entries — comma logic must hold.
	s := EmbedStruct{Extra: map[string]any{"k": "v"}}
	out, _ := ggen.MarshalString(s)
	if !strings.Contains(out, `"k":"v"`) {
		t.Errorf("missing k:v in %q", out)
	}
	if !strings.HasPrefix(out, "{") || !strings.HasSuffix(out, "}") {
		t.Errorf("bad framing: %q", out)
	}
}

// The fixed field appears in every marshal regardless of map iteration order.
func TestEmbed_FixedFieldOrderStable(t *testing.T) {
	t.Parallel()
	s := EmbedStruct{
		Name:  "alice",
		Extra: map[string]any{"age": 30, "city": "Lviv", "active": true, "score": 9.5},
	}
	for i := range 20 {
		out, _ := ggen.Marshal(s)
		if !strings.Contains(string(out), `"name":"alice"`) {
			t.Errorf("iter %d: missing name field: %s", i, out)
		}
	}
}

// Typed inline catch-all: map[string]string (no any boxing).
func TestEmbed_TypedString_Decode(t *testing.T) {
	t.Parallel()
	in := []byte(`{"name":"alice","city":"Lviv","role":"admin"}`)
	got, _, err := EmbedStringsStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "alice" {
		t.Errorf("Name = %q", got.Name)
	}
	if len(got.Extra) != 2 {
		t.Fatalf("Extra len = %d, want 2: %+v", len(got.Extra), got.Extra)
	}
	if got.Extra["city"] != "Lviv" || got.Extra["role"] != "admin" {
		t.Errorf("Extra = %+v", got.Extra)
	}
}

func TestEmbed_TypedString_EmptyExtra(t *testing.T) {
	t.Parallel()
	got, _, err := EmbedStringsStruct{}.DecodeFrom([]byte(`{"name":"bob"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Extra != nil {
		t.Errorf("Extra should be nil, got %+v", got.Extra)
	}
}

// Non-string value into a string-typed inline must error.
func TestEmbed_TypedString_RejectsNonString(t *testing.T) {
	t.Parallel()
	_, _, err := EmbedStringsStruct{}.DecodeFrom([]byte(`{"name":"alice","age":30}`))
	if err == nil {
		t.Fatal("expected error decoding number into string-typed inline")
	}
}

func TestEmbed_TypedString_MarshalSpreads(t *testing.T) {
	t.Parallel()
	s := EmbedStringsStruct{
		Name:  "alice",
		Extra: map[string]string{"city": "Lviv", "role": "admin"},
	}
	out, _ := ggen.MarshalString(s)
	for _, want := range []string{`"name":"alice"`, `"city":"Lviv"`, `"role":"admin"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	if !strings.HasPrefix(out, "{") || !strings.HasSuffix(out, "}") {
		t.Errorf("bad framing: %q", out)
	}
}

func TestEmbed_TypedString_Roundtrip(t *testing.T) {
	t.Parallel()
	orig := EmbedStringsStruct{
		Name:  "alice",
		Extra: map[string]string{"city": "Lviv", "role": "admin", "lang": "uk"},
	}
	out, _ := ggen.Marshal(orig)
	got, _, err := EmbedStringsStruct{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if got.Name != orig.Name {
		t.Errorf("Name mismatch: %q vs %q", got.Name, orig.Name)
	}
	if len(got.Extra) != len(orig.Extra) {
		t.Fatalf("Extra len: got %d want %d\n%+v", len(got.Extra), len(orig.Extra), got.Extra)
	}
	for k, v := range orig.Extra {
		if got.Extra[k] != v {
			t.Errorf("Extra[%q] = %q want %q", k, got.Extra[k], v)
		}
	}
}

// Typed inline catch-all: map[string]EmbedStruct, decoded via the elem
// type's DecodeFrom (not json.Unmarshal fallback).
func TestEmbed_TypedStruct_Decode(t *testing.T) {
	t.Parallel()
	in := []byte(`{"name":"root","kid":{"name":"alice","age":30},"sib":{"name":"bob"}}`)
	got, _, err := EmbedStructsStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "root" {
		t.Errorf("Name = %q", got.Name)
	}
	if len(got.Extra) != 2 {
		t.Fatalf("Extra len = %d, want 2: %+v", len(got.Extra), got.Extra)
	}
	kid := got.Extra["kid"]
	if kid.Name != "alice" {
		t.Errorf("kid.Name = %q", kid.Name)
	}
	// Inner EmbedStruct has its own catch-all — age:30 lands there.
	if age, _ := kid.Extra["age"].(float64); age != 30 {
		t.Errorf("kid.Extra[age] = %v", kid.Extra["age"])
	}
	if got.Extra["sib"].Name != "bob" {
		t.Errorf("sib.Name = %q", got.Extra["sib"].Name)
	}
}

func TestEmbed_TypedStruct_Roundtrip(t *testing.T) {
	t.Parallel()
	orig := EmbedStructsStruct{
		Name: "root",
		Extra: map[string]EmbedStruct{
			"kid": {Name: "alice", Extra: map[string]any{"age": float64(30)}},
			"sib": {Name: "bob"},
		},
	}
	out, _ := ggen.Marshal(orig)
	got, _, err := EmbedStructsStruct{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if got.Name != orig.Name {
		t.Errorf("Name mismatch: %q vs %q", got.Name, orig.Name)
	}
	if len(got.Extra) != len(orig.Extra) {
		t.Fatalf("Extra len: got %d want %d\n%+v", len(got.Extra), len(orig.Extra), got.Extra)
	}
	for k, want := range orig.Extra {
		gotEntry := got.Extra[k]
		if gotEntry.Name != want.Name {
			t.Errorf("Extra[%q].Name = %q want %q", k, gotEntry.Name, want.Name)
		}
		if want.Extra != nil {
			if age := gotEntry.Extra["age"]; age != want.Extra["age"] {
				t.Errorf("Extra[%q].Extra[age] = %v want %v", k, age, want.Extra["age"])
			}
		}
	}
}

// Empty-Extra marshal: typed inline emits only the fixed fields.
func TestEmbed_TypedString_MarshalEmpty(t *testing.T) {
	t.Parallel()
	out, _ := ggen.MarshalString(EmbedStringsStruct{Name: "alice"})
	if out != `{"name":"alice"}` {
		t.Errorf("empty-extra marshal = %q", out)
	}
}

func TestEmbed_TypedStruct_MarshalEmpty(t *testing.T) {
	t.Parallel()
	out, _ := ggen.MarshalString(EmbedStructsStruct{Name: "root"})
	if out != `{"name":"root"}` {
		t.Errorf("empty-extra marshal = %q", out)
	}
}

// An EMPTY RawMessage entry in the inline map must marshal as null — the
// raw passthrough used to append zero bytes, emitting `"k":` with no value
// (corrupt JSON). Field-level raw emit and AppendAny already null empties.
func TestEmbed_EmptyRawMessageMarshalsNull(t *testing.T) {
	t.Parallel()
	out, err := ggen.Marshal(EmbedRawStruct{
		Name:  "x",
		Extra: map[string]json.RawMessage{"a": nil, "b": {}, "c": json.RawMessage(`{"n":1}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output not valid JSON: %v\nwire: %s", err, out)
	}
	if string(m["a"]) != "null" || string(m["b"]) != "null" {
		t.Errorf("empty raw entries → a=%s b=%s, want null/null", m["a"], m["b"])
	}
	if string(m["c"]) != `{"n":1}` {
		t.Errorf("non-empty raw entry mangled: %s", m["c"])
	}
	// Round-trip: decode absorbs unknown keys back into the raw map.
	back, _, err := EmbedRawStruct{}.DecodeFrom(out)
	if err != nil || string(back.Extra["c"]) != `{"n":1}` {
		t.Fatalf("decode: %+v %v", back, err)
	}
}

// The catch-all map is emptied at decode entry like any other container: keys
// from a previous decode must not survive into the next one.
func TestEmbed_carriedMapDropsStaleKeys(t *testing.T) {
	t.Parallel()
	first, _, err := EmbedStructsStruct{}.DecodeFrom([]byte(`{"name":"n","a":{"name":"A"},"b":{"name":"B"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Extra) != 2 {
		t.Fatalf("first decode: %v", first.Extra)
	}
	got, _, err := first.DecodeFrom([]byte(`{"name":"n","a":{"name":"A2"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Extra) != 1 || got.Extra["a"].Name != "A2" {
		t.Errorf("stale catch-all keys survived: %v", got.Extra)
	}
}
