package integrationtests

//go:generate ../ggen $GOFILE

import (
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/encode"
)

// InlineStruct exercises the json:",inline" catch-all: unknown keys absorbed
// into Extra, spliced back out at the object level on marshal.
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
	t.Parallel()
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

func TestInline_emptyDecode(t *testing.T) {
	t.Parallel()
	// No unknown keys → Extra stays nil.
	got, _, err := InlineStruct{}.DecodeFrom([]byte(`{"name":"bob"}`))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Extra != nil {
		t.Errorf("Extra should be nil, got %+v", got.Extra)
	}
}

func TestInline_marshalSpreads(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	s := InlineStruct{Name: "alice"}
	out, _ := encode.MarshalString(s)
	if out != `{"name":"alice"}` {
		t.Errorf("empty-extra marshal = %q", out)
	}
}

func TestInline_marshalOnlyExtras(t *testing.T) {
	t.Parallel()
	// Empty fixed field + inline entries — comma logic must hold.
	s := InlineStruct{Extra: map[string]any{"k": "v"}}
	out, _ := encode.MarshalString(s)
	if !strings.Contains(out, `"k":"v"`) {
		t.Errorf("missing k:v in %q", out)
	}
	if !strings.HasPrefix(out, "{") || !strings.HasSuffix(out, "}") {
		t.Errorf("bad framing: %q", out)
	}
}

// TestInline_FixedFieldOrderStable: fixed field appears in every marshal
// regardless of map iteration order.
func TestInline_FixedFieldOrderStable(t *testing.T) {
	t.Parallel()
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

// Typed inline catch-all: map[string]string (no any boxing).
func TestInline_TypedString_Decode(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	got, _, err := InlineStringsStruct{}.DecodeFrom([]byte(`{"name":"bob"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Extra != nil {
		t.Errorf("Extra should be nil, got %+v", got.Extra)
	}
}

// Non-string value into a string-typed inline must error.
func TestInline_TypedString_RejectsNonString(t *testing.T) {
	t.Parallel()
	_, _, err := InlineStringsStruct{}.DecodeFrom([]byte(`{"name":"alice","age":30}`))
	if err == nil {
		t.Fatal("expected error decoding number into string-typed inline")
	}
}

func TestInline_TypedString_MarshalSpreads(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// Typed inline catch-all: map[string]InlineStruct, decoded via the elem
// type's DecodeFrom (not json.Unmarshal fallback).
func TestInline_TypedStruct_Decode(t *testing.T) {
	t.Parallel()
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
	// Inner InlineStruct has its own catch-all — age:30 lands there.
	if age, _ := kid.Extra["age"].(float64); age != 30 {
		t.Errorf("kid.Extra[age] = %v", kid.Extra["age"])
	}
	if got.Extra["sib"].Name != "bob" {
		t.Errorf("sib.Name = %q", got.Extra["sib"].Name)
	}
}

func TestInline_TypedStruct_Roundtrip(t *testing.T) {
	t.Parallel()
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

// Empty-Extra marshal: typed inline emits only the fixed fields.
func TestInline_TypedString_MarshalEmpty(t *testing.T) {
	t.Parallel()
	out, _ := encode.MarshalString(InlineStringsStruct{Name: "alice"})
	if out != `{"name":"alice"}` {
		t.Errorf("empty-extra marshal = %q", out)
	}
}

func TestInline_TypedStruct_MarshalEmpty(t *testing.T) {
	t.Parallel()
	out, _ := encode.MarshalString(InlineStructsStruct{Name: "root"})
	if out != `{"name":"root"}` {
		t.Errorf("empty-extra marshal = %q", out)
	}
}
