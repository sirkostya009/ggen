package main

import (
	"strings"
	"testing"
	"time"

	"github.com/sirkostya009/ggen/decode"
	"github.com/sirkostya009/ggen/encode"
)

// PointerStruct exercises optional fields via Go pointers. Each nil pointer
// encodes as JSON null and decodes back to nil.
//
//ggen:generate
type PointerStruct struct {
	Name    *string    `json:"name"`
	Count   *int       `json:"count,omitempty"`
	Ratio   *float64   `json:"ratio,omitempty"`
	Addr    *Address   `json:"addr,omitempty"`
	When    *time.Time `json:"when,omitempty,format:unix"`
	Enabled *bool      `json:"enabled"`
}

func TestPointer_roundtripAllSet(t *testing.T) {
	in := PointerStruct{
		Name:    new("alice"),
		Count:   new(7),
		Ratio:   new(0.5),
		Addr:    &Address{Street: "Main 1", City: "Lviv", ZipCode: "79000"},
		When:    new(time.Unix(1_700_000_000, 0).UTC()),
		Enabled: new(true),
	}
	out, _ := encode.MarshalString(in)
	got, err := decode.Unmarshal[PointerStruct]([]byte(out))
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if *got.Name != "alice" || *got.Count != 7 || *got.Ratio != 0.5 ||
		got.Addr.City != "Lviv" || !got.When.Equal(*in.When) || *got.Enabled != true {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

func TestPointer_nilOmitted(t *testing.T) {
	in := PointerStruct{Name: new("bob"), Enabled: new(false)}
	out, _ := encode.MarshalString(in)
	// omitempty fields (Count/Ratio/Addr/When) are nil → absent.
	for _, absent := range []string{"count", "ratio", "addr", "when"} {
		if strings.Contains(out, `"`+absent+`"`) {
			t.Errorf("expected %q omitted, got %q", absent, out)
		}
	}
	// Non-omit fields (Name, Enabled) present even when non-nil.
	if !strings.Contains(out, `"name":"bob"`) {
		t.Errorf("name missing: %q", out)
	}
	if !strings.Contains(out, `"enabled":false`) {
		t.Errorf("enabled missing: %q", out)
	}
}

func TestPointer_nullRoundtrip(t *testing.T) {
	// Non-omit pointer explicitly null should decode to nil.
	in := []byte(`{"name":null,"enabled":null}`)
	got, err := decode.Unmarshal[PointerStruct](in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Name != nil {
		t.Errorf("expected Name nil, got %v", *got.Name)
	}
	if got.Enabled != nil {
		t.Errorf("expected Enabled nil, got %v", *got.Enabled)
	}
}
