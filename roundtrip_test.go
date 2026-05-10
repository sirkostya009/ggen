package main

import (
	"bytes"
	"testing"

	"github.com/sirkostya009/ggen/decode"
	"github.com/sirkostya009/ggen/encode"
)

// TestMarshalRoundtrip ensures the generated marshaller produces JSON that the
// generated parser accepts, matching the original value.
func TestMarshalRoundtrip(t *testing.T) {
	out, _ := encode.Marshal(complexValue)
	got, err := decode.Unmarshal[SomePayloadRequestStruct](out)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, out)
	}
	if got.Field1 != complexValue.Field1 {
		t.Errorf("Field1 mismatch: %q vs %q", got.Field1, complexValue.Field1)
	}
	if len(got.Slice) != len(complexValue.Slice) {
		t.Errorf("Slice len: %d vs %d", len(got.Slice), len(complexValue.Slice))
	}
	if got.Address.City != complexValue.Address.City {
		t.Errorf("Address.City: %q vs %q", got.Address.City, complexValue.Address.City)
	}
	if len(got.Contacts) != len(complexValue.Contacts) {
		t.Errorf("Contacts len: %d vs %d", len(got.Contacts), len(complexValue.Contacts))
	}
}

func TestSliceRoundtrip(t *testing.T) {
	addrs := []Address{
		{Street: "Main 1", City: "Lviv", ZipCode: "79000"},
		{Street: "Side 2", City: "Odesa", ZipCode: "65000"},
		{Street: "Back 3", City: "Kyiv", ZipCode: "01000"},
	}
	out, _ := encode.MarshalSlice(addrs)
	got, err := decode.UnmarshalSlice[Address](out)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, out)
	}
	if len(got) != len(addrs) {
		t.Fatalf("len mismatch: got %d want %d", len(got), len(addrs))
	}
	for i := range addrs {
		if got[i] != addrs[i] {
			t.Errorf("[%d] mismatch: %+v vs %+v", i, got[i], addrs[i])
		}
	}

	// String + Reader variants
	s, _ := encode.MarshalSliceString(addrs)
	if s[0] != '[' || s[len(s)-1] != ']' {
		t.Errorf("bad slice string: %q", s)
	}

	var buf bytes.Buffer
	if err := encode.WriteSlice(&buf, addrs); err != nil {
		t.Fatal(err)
	}
	got2, err := decode.UnmarshalSlice[Address](buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != len(addrs) {
		t.Errorf("reader roundtrip len mismatch: got %d want %d", len(got2), len(addrs))
	}
}

func TestWrite(t *testing.T) {
	var buf bytes.Buffer
	if err := encode.Write(&buf, complexValue); err != nil {
		t.Fatal(err)
	}
	got, err := decode.Unmarshal[SomePayloadRequestStruct](buf.Bytes())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Field1 != complexValue.Field1 {
		t.Errorf("Field1 mismatch")
	}
}
