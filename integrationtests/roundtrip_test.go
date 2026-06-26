package integrationtests

import (
	"bytes"
	"testing"

	"github.com/sirkostya009/ggen/decode"
	"github.com/sirkostya009/ggen/encode"
)

// TestMarshalRoundtrip: generated marshal output decodes back to the original.
func TestMarshalRoundtrip(t *testing.T) {
	t.Parallel()
	out, _ := encode.Marshal(complexValue)
	got, _, err := Node{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, out)
	}
	if got.Name != complexValue.Name {
		t.Errorf("Name mismatch: %q vs %q", got.Name, complexValue.Name)
	}
	if len(got.Tags) != len(complexValue.Tags) {
		t.Errorf("Tags len: %d vs %d", len(got.Tags), len(complexValue.Tags))
	}
	if len(got.Children) != len(complexValue.Children) {
		t.Errorf("Children len: %d vs %d", len(got.Children), len(complexValue.Children))
	}
	if got.Children[1].Name != complexValue.Children[1].Name {
		t.Errorf("nested mismatch: %q vs %q", got.Children[1].Name, complexValue.Children[1].Name)
	}
}

func TestSliceRoundtrip(t *testing.T) {
	t.Parallel()
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

	// String + Reader variants.
	s, _ := encode.MarshalSliceString(addrs)
	if s[0] != '[' || s[len(s)-1] != ']' {
		t.Errorf("bad slice string: %q", s)
	}

	var buf bytes.Buffer
	if err := encode.WriteSliceTo(&buf, addrs); err != nil {
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
	t.Parallel()
	var buf bytes.Buffer
	if err := encode.WriteTo(&buf, complexValue); err != nil {
		t.Fatal(err)
	}
	got, _, err := Node{}.DecodeFrom(buf.Bytes())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Name != complexValue.Name {
		t.Errorf("Name mismatch")
	}
}
