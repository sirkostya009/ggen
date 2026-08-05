package integrationtests

//go:generate ../ggen $GOFILE

import (
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/encode"
)

// MapStruct exercises map[string]V: primitive/struct values, inner mods.
//
//ggen:generate
type MapStruct struct {
	Counts    map[string]int     `json:"counts" pipe:"maxlen=10"`
	Labels    map[string]string  `json:"labels" pipe:"inner:(trim tolower)"`
	Addresses map[string]Address `json:"addresses"`
}

// MapDiveStruct exercises inner: validation + mods on map values.
//
//ggen:generate
type MapDiveStruct struct {
	Counts  map[string]int    `json:"counts" pipe:"inner:(gte=0 lte=100)"`
	Names   map[string]string `json:"names"  pipe:"inner:(minlen=1 maxlen=5)"`
	Clamped map[string]int    `json:"clamped" pipe:"inner:clamp=0|100"`
}

// Base is embedded into Derived to exercise field promotion.
type Base struct {
	ID   string `json:"id" pipe:"required"`
	Meta string `json:"meta,omitempty"`
}

// Derived embeds Base — ID and Meta promote to the JSON object level.
//
//ggen:generate
type Derived struct {
	Base
	Name string `json:"name" pipe:"required minlen=1"`
}

func TestMap_primitiveRoundtrip(t *testing.T) {
	t.Parallel()
	in := MapStruct{
		Counts: map[string]int{"a": 1, "b": 2, "c": 3},
	}
	out, _ := encode.MarshalString(in)
	got, _, err := MapStruct{}.DecodeFrom([]byte(out))
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
	t.Parallel()
	in := MapStruct{
		Addresses: map[string]Address{
			"home": {Street: "Main 1", City: "Lviv", ZipCode: "79000"},
		},
	}
	out, _ := encode.MarshalString(in)
	got, _, err := MapStruct{}.DecodeFrom([]byte(out))
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if got.Addresses["home"].City != "Lviv" {
		t.Errorf("Addresses[home].City = %q", got.Addresses["home"].City)
	}
}

func TestMap_diveMod(t *testing.T) {
	t.Parallel()
	in := []byte(`{"labels":{"en":"  HELLO ","es":" HOLA "}}`)
	got, _, err := MapStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Labels["en"] != "hello" || got.Labels["es"] != "hola" {
		t.Errorf("mods didn't apply: %+v", got.Labels)
	}
}

func TestMap_maxlenViolation(t *testing.T) {
	t.Parallel()
	// Counts maxlen=10; 11 entries must fail.
	const alphabet = "abcdefghijk" // 11 letters
	b := strings.Builder{}
	b.WriteString(`{"counts":{`)
	for i := range len(alphabet) {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"`)
		b.WriteByte(alphabet[i])
		b.WriteString(`":`)
		b.WriteByte('0' + byte(i%10))
	}
	b.WriteString(`}}`)
	_, _, err := MapStruct{}.DecodeFrom([]byte(b.String()))
	if err == nil {
		t.Fatal("expected maxlen violation")
	}
	if !strings.Contains(err.Error(), "exceeds maximum") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestMap_diveValueValidation_intOOB(t *testing.T) {
	t.Parallel()
	in := []byte(`{"counts":{"a":50,"b":200}}`)
	_, _, err := MapDiveStruct{}.DecodeFrom(in)
	if err == nil {
		t.Fatal("expected lte violation on map value")
	}
	if !strings.Contains(err.Error(), "exceeds maximum") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestMap_diveValueValidation_stringTooLong(t *testing.T) {
	t.Parallel()
	in := []byte(`{"names":{"a":"ok","b":"toolong"}}`)
	_, _, err := MapDiveStruct{}.DecodeFrom(in)
	if err == nil {
		t.Fatal("expected maxlen violation on map value")
	}
}

func TestMap_diveMod_numericClamp(t *testing.T) {
	t.Parallel()
	in := []byte(`{"clamped":{"a":-5,"b":50,"c":250}}`)
	got, _, err := MapDiveStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Clamped["a"] != 0 || got.Clamped["b"] != 50 || got.Clamped["c"] != 100 {
		t.Errorf("clamp mods didn't apply: %+v", got.Clamped)
	}
}

func TestMap_diveValueValidation_pass(t *testing.T) {
	t.Parallel()
	in := []byte(`{"counts":{"a":50,"b":99},"names":{"x":"hi"}}`)
	got, _, err := MapDiveStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Counts["b"] != 99 || got.Names["x"] != "hi" {
		t.Errorf("got %+v", got)
	}
}

func TestEmbedded_promotedFields(t *testing.T) {
	t.Parallel()
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
	got, _, err := Derived{}.DecodeFrom([]byte(out))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID != "abc" || got.Meta != "info" || got.Name != "alice" {
		t.Errorf("got %+v", got)
	}
}

func TestEmbedded_promotedRequired(t *testing.T) {
	t.Parallel()
	// Base.ID is required; missing must fail.
	in := []byte(`{"name":"bob"}`)
	_, _, err := Derived{}.DecodeFrom(in)
	if err == nil {
		t.Fatal("expected missing-required error")
	}
}
