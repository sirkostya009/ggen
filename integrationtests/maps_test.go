package integrationtests

import (
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/decode"
	"github.com/sirkostya009/ggen/encode"
)

// MapStruct exercises map[string]V fields: primitive values, struct values,
// and dive-mods applied to each value.
//
//ggen:generate
type MapStruct struct {
	// Simple int values; outer maxlen caps map size.
	Counts map[string]int `json:"counts" ggen:"maxlen=10"`
	// String values, trimmed via dive mod.
	Labels map[string]string `json:"labels" mod:"dive:trim,lower"`
	// Nested annotated structs.
	Addresses map[string]Address `json:"addresses"`
}

// MapDiveStruct exercises dive: validation rules + dive: mods applied to
// map values across both string and non-string element kinds.
//
//ggen:generate
type MapDiveStruct struct {
	Counts map[string]int    `json:"counts" ggen:"dive:gte=0,lte=100"`
	Names  map[string]string `json:"names"  ggen:"dive:minlen=1,maxlen=5"`
	// Clamped: numeric dive mod on map values — values outside [0,100] are
	// pulled back to the bound before validation runs.
	Clamped map[string]int `json:"clamped" mod:"dive:clamp=0|100"`
}

// Base is embedded into Derived to exercise field promotion. Same-package,
// untagged embedding — fields lift into the parent's JSON object.
type Base struct {
	ID   string `json:"id" ggen:"required"`
	Meta string `json:"meta,omitempty"`
}

// Derived embeds Base — ID and Meta are promoted to the JSON object level.
//
//ggen:generate
type Derived struct {
	Base
	Name string `json:"name" ggen:"required,minlen=1"`
}

func TestMap_primitiveRoundtrip(t *testing.T) {
	in := MapStruct{
		Counts: map[string]int{"a": 1, "b": 2, "c": 3},
	}
	out, _ := encode.MarshalString(in)
	got, err := decode.Unmarshal[MapStruct]([]byte(out))
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	for k, v := range in.Counts {
		if got.Counts[k] != v {
			t.Errorf("Counts[%q] = %d, want %d", k, got.Counts[k], v)
		}
	}
}

func TestMap_structValueRoundtrip(t *testing.T) {
	in := MapStruct{
		Addresses: map[string]Address{
			"home": {Street: "Main 1", City: "Lviv", ZipCode: "79000"},
		},
	}
	out, _ := encode.MarshalString(in)
	got, err := decode.Unmarshal[MapStruct]([]byte(out))
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if got.Addresses["home"].City != "Lviv" {
		t.Errorf("Addresses[home].City = %q", got.Addresses["home"].City)
	}
}

func TestMap_diveMod(t *testing.T) {
	// Labels has `mod:"dive:trim,lower"` — each value normalized on decode.
	in := []byte(`{"labels":{"en":"  HELLO ","es":" HOLA "}}`)
	got, err := decode.Unmarshal[MapStruct](in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Labels["en"] != "hello" || got.Labels["es"] != "hola" {
		t.Errorf("mods didn't apply: %+v", got.Labels)
	}
}

func TestMap_maxlenViolation(t *testing.T) {
	// Counts has maxlen=10; 11 unique entries should fail.
	const alphabet = "abcdefghijk" // 11 letters
	b := strings.Builder{}
	b.WriteString(`{"counts":{`)
	for i := 0; i < len(alphabet); i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"`)
		b.WriteByte(alphabet[i])
		b.WriteString(`":`)
		b.WriteByte('0' + byte(i%10))
	}
	b.WriteString(`}}`)
	_, err := decode.Unmarshal[MapStruct]([]byte(b.String()))
	if err == nil {
		t.Fatal("expected maxlen violation")
	}
	if !strings.Contains(err.Error(), "exceeds maximum") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestMap_diveValueValidation_intOOB(t *testing.T) {
	in := []byte(`{"counts":{"a":50,"b":200}}`)
	_, err := decode.Unmarshal[MapDiveStruct](in)
	if err == nil {
		t.Fatal("expected lte violation on map value")
	}
	if !strings.Contains(err.Error(), "exceeds maximum") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestMap_diveValueValidation_stringTooLong(t *testing.T) {
	in := []byte(`{"names":{"a":"ok","b":"toolong"}}`)
	_, err := decode.Unmarshal[MapDiveStruct](in)
	if err == nil {
		t.Fatal("expected maxlen violation on map value")
	}
}

func TestMap_diveMod_numericClamp(t *testing.T) {
	in := []byte(`{"clamped":{"a":-5,"b":50,"c":250}}`)
	got, err := decode.Unmarshal[MapDiveStruct](in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Clamped["a"] != 0 || got.Clamped["b"] != 50 || got.Clamped["c"] != 100 {
		t.Errorf("clamp mods didn't apply: %+v", got.Clamped)
	}
}

func TestMap_diveValueValidation_pass(t *testing.T) {
	in := []byte(`{"counts":{"a":50,"b":99},"names":{"x":"hi"}}`)
	got, err := decode.Unmarshal[MapDiveStruct](in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Counts["b"] != 99 || got.Names["x"] != "hi" {
		t.Errorf("got %+v", got)
	}
}

func TestEmbedded_promotedFields(t *testing.T) {
	in := Derived{
		Base: Base{ID: "abc", Meta: "info"},
		Name: "alice",
	}
	out, _ := encode.MarshalString(in)
	for _, want := range []string{`"id":"abc"`, `"name":"alice"`, `"meta":"info"`} {
		if !strings.Contains(out, want) {
			t.Errorf("marshal missing %q in %q", want, out)
		}
	}
	got, err := decode.Unmarshal[Derived]([]byte(out))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID != "abc" || got.Meta != "info" || got.Name != "alice" {
		t.Errorf("got %+v", got)
	}
}

func TestEmbedded_promotedRequired(t *testing.T) {
	// Base.ID is required; missing should fail.
	in := []byte(`{"name":"bob"}`)
	_, err := decode.Unmarshal[Derived](in)
	if err == nil {
		t.Fatal("expected missing-required error")
	}
}
