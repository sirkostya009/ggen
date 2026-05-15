package integrationtests

import (
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/decode"
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

func TestInline_decodeAbsorbsUnknown(t *testing.T) {
	in := []byte(`{"name":"alice","age":30,"city":"Lviv","tags":["a","b"]}`)
	got, err := decode.Unmarshal[InlineStruct](in)
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
	got, err := decode.Unmarshal[InlineStruct]([]byte(`{"name":"bob"}`))
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
	got, err := decode.Unmarshal[InlineStruct](out)
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
