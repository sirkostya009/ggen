package integrationtests

//go:generate ../ggen $GOFILE

import (
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/encode"
)

// InlineStruct exercises the json:",inline" catch-all: every JSON key that
// doesn't match a named field is absorbed into Extra. On marshal the map
// entries splice back out at the object level.
//
//ggen:generate
type InlineStruct struct {
	Name  string         `json:"name"`
	Extra map[string]any `json:",inline"`
}

//ggen:generate
type InlineStringsStruct struct {
	Name  string            `json:"name"`
	Extra map[string]string `json:",inline"`
}

//ggen:generate
type InlineStructsStruct struct {
	Name  string                  `json:"name"`
	Extra map[string]InlineStruct `json:",inline"`
}

func TestInline_decodeAbsorbsUnknown(t *testing.T) {
	in := []byte(`{"name":"alice","age":30,"city":"Lviv","tags":["a","b"]}`)
	got, _, err := InlineStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Name != "alice" {
		t.Errorf("Name = %q", got.Name)
	}
	if len(got.Extra) != 3 {
		t.Fatalf("Extra len = %d, want 3: %+v", len(got.Extra), got.Extra)
	}
	// jsonv2 decodes bare numbers into float64 for `any`.
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

func TestInline_emptyDecode(t *testing.T) {
	// No unknown keys → Extra stays nil, not an empty map.
	got, _, err := InlineStruct{}.DecodeFrom([]byte(`{"name":"bob"}`))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Extra != nil {
		t.Errorf("Extra should be nil, got %+v", got.Extra)
	}
}

func TestInline_marshalSpreads(t *testing.T) {
	s := InlineStruct{
		Name:  "alice",
		Extra: map[string]any{"age": 30, "city": "Lviv"},
	}
	out, _ := encode.MarshalString(s)
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

func TestInline_roundtrip(t *testing.T) {
	orig := InlineStruct{
		Name:  "alice",
		Extra: map[string]any{"age": float64(30), "city": "Lviv", "active": true},
	}
	out, _ := encode.Marshal(orig)
	got, _, err := InlineStruct{}.DecodeFrom(out)
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

func TestInline_marshalEmpty(t *testing.T) {
	s := InlineStruct{Name: "alice"}
	out, _ := encode.MarshalString(s)
	if out != `{"name":"alice"}` {
		t.Errorf("empty-extra marshal = %q", out)
	}
}

func TestInline_marshalOnlyExtras(t *testing.T) {
	// Empty Name + extras — ensure comma logic works when the "fixed" field
	// emits an empty value and the inline map has entries.
	s := InlineStruct{Extra: map[string]any{"k": "v"}}
	out, _ := encode.MarshalString(s)
	if !strings.Contains(out, `"k":"v"`) {
		t.Errorf("missing k:v in %q", out)
	}
	if !strings.HasPrefix(out, "{") || !strings.HasSuffix(out, "}") {
		t.Errorf("bad framing: %q", out)
	}
}

// TestInline_FixedFieldOrderStable: when an inline map is present alongside
// fixed fields, the fixed field MUST appear in every marshal regardless of
// map iteration order (which is random in Go).
func TestInline_FixedFieldOrderStable(t *testing.T) {
	s := InlineStruct{
		Name:  "alice",
		Extra: map[string]any{"age": 30, "city": "Lviv", "active": true, "score": 9.5},
	}
	for i := range 20 {
		out, _ := encode.Marshal(s)
		if !strings.Contains(string(out), `"name":"alice"`) {
			t.Errorf("iter %d: missing name field: %s", i, out)
		}
	}
}

// Typed inline catch-all: map[string]string. Unknown keys absorbed into a
// string-typed map (no `any` boxing). Mirrors jsonv2's typed-inline behavior.
func TestInline_TypedString_Decode(t *testing.T) {
	in := []byte(`{"name":"alice","city":"Lviv","role":"admin"}`)
	got, _, err := InlineStringsStruct{}.DecodeFrom(in)
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

func TestInline_TypedString_EmptyExtra(t *testing.T) {
	got, _, err := InlineStringsStruct{}.DecodeFrom([]byte(`{"name":"bob"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Extra != nil {
		t.Errorf("Extra should be nil, got %+v", got.Extra)
	}
}

// Non-string value in payload must error — typed inline rejects shape
// mismatch (parity with jsonv2's "cannot unmarshal JSON number into Go string").
func TestInline_TypedString_RejectsNonString(t *testing.T) {
	_, _, err := InlineStringsStruct{}.DecodeFrom([]byte(`{"name":"alice","age":30}`))
	if err == nil {
		t.Fatal("expected error decoding number into string-typed inline")
	}
}

func TestInline_TypedString_MarshalSpreads(t *testing.T) {
	s := InlineStringsStruct{
		Name:  "alice",
		Extra: map[string]string{"city": "Lviv", "role": "admin"},
	}
	out, _ := encode.MarshalString(s)
	for _, want := range []string{`"name":"alice"`, `"city":"Lviv"`, `"role":"admin"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	if !strings.HasPrefix(out, "{") || !strings.HasSuffix(out, "}") {
		t.Errorf("bad framing: %q", out)
	}
}

func TestInline_TypedString_Roundtrip(t *testing.T) {
	orig := InlineStringsStruct{
		Name:  "alice",
		Extra: map[string]string{"city": "Lviv", "role": "admin", "lang": "uk"},
	}
	out, _ := encode.Marshal(orig)
	got, _, err := InlineStringsStruct{}.DecodeFrom(out)
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

// Typed inline catch-all: map[string]InlineStruct. Unknown keys decoded
// via the elem type's DecodeFrom (zero-alloc structured value, not via
// json.Unmarshal fallback).
func TestInline_TypedStruct_Decode(t *testing.T) {
	in := []byte(`{"name":"root","kid":{"name":"alice","age":30},"sib":{"name":"bob"}}`)
	got, _, err := InlineStructsStruct{}.DecodeFrom(in)
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
	// Inner InlineStruct itself has its own catch-all — `age:30` lands there.
	if age, _ := kid.Extra["age"].(float64); age != 30 {
		t.Errorf("kid.Extra[age] = %v", kid.Extra["age"])
	}
	if got.Extra["sib"].Name != "bob" {
		t.Errorf("sib.Name = %q", got.Extra["sib"].Name)
	}
}

func TestInline_TypedStruct_Roundtrip(t *testing.T) {
	orig := InlineStructsStruct{
		Name: "root",
		Extra: map[string]InlineStruct{
			"kid": {Name: "alice", Extra: map[string]any{"age": float64(30)}},
			"sib": {Name: "bob"},
		},
	}
	out, _ := encode.Marshal(orig)
	got, _, err := InlineStructsStruct{}.DecodeFrom(out)
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

// Empty-Extra marshal: typed inline still emits exactly the fixed fields,
// no trailing comma, no nested object.
func TestInline_TypedString_MarshalEmpty(t *testing.T) {
	out, _ := encode.MarshalString(InlineStringsStruct{Name: "alice"})
	if out != `{"name":"alice"}` {
		t.Errorf("empty-extra marshal = %q", out)
	}
}

func TestInline_TypedStruct_MarshalEmpty(t *testing.T) {
	out, _ := encode.MarshalString(InlineStructsStruct{Name: "root"})
	if out != `{"name":"root"}` {
		t.Errorf("empty-extra marshal = %q", out)
	}
}
