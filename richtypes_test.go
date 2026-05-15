//go:build goexperiment.jsonv2

package main

// Coverage for the rich built-in type kinds: json.RawMessage / jsontext.Value
// (passthrough), url.URL, math/big, and google/uuid. One annotated struct
// hosts every kind so the generator's dispatch matrix is exercised end to end.

import (
	"encoding/json"
	"encoding/json/jsontext"
	"math/big"
	"net/url"
	"reflect"
	"strings"
	"testing"

	gofrs "github.com/gofrs/uuid/v5"
	"github.com/google/uuid"
	"github.com/sirkostya009/ggen/decode"
	"github.com/sirkostya009/ggen/encode"
)

// RichTypes mixes every newly-added Kind. Field naming mirrors the type so
// failures point straight at the offending dispatch case. ID and GofrsID
// cover both major UUID libraries — both are `type UUID [16]byte` and
// satisfy TextMarshaler/Unmarshaler, so the same generated dispatch
// serves both with no per-lib codegen.
//
//ggen:generate
type RichTypes struct {
	Raw1    json.RawMessage `json:"raw1"`
	Raw2    jsontext.Value  `json:"raw2"`
	Site    url.URL         `json:"site"`
	Big     big.Int         `json:"big"`
	BigF    big.Float       `json:"bigF"`
	BigR    big.Rat         `json:"bigR"`
	ID      uuid.UUID       `json:"id"`
	GofrsID gofrs.UUID      `json:"gofrsId"`
}

// TestRich_Roundtrip marshal → unmarshal preserves every field. Big values
// chosen so they exceed int64 range, exercising arbitrary-precision paths.
func TestRich_Roundtrip(t *testing.T) {
	hugeInt, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	site, _ := url.Parse("https://example.com/path?q=1")
	id := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	gofrsID, _ := gofrs.FromString("550e8400-e29b-41d4-a716-446655440000")
	rat, _ := new(big.Rat).SetString("22/7")
	bigF, _, _ := big.ParseFloat("3.14159265358979323846", 10, 100, big.ToNearestEven)

	in := RichTypes{
		Raw1:    json.RawMessage(`{"nested":42}`),
		Raw2:    jsontext.Value(`[1,2,3]`),
		Site:    *site,
		Big:     *hugeInt,
		BigF:    *bigF,
		BigR:    *rat,
		ID:      id,
		GofrsID: gofrsID,
	}
	out, _ := encode.Marshal(in)
	got, err := decode.Unmarshal[RichTypes](out)
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}

	// Raw bytes should round-trip byte-equal (modulo alias vs copy).
	if string(got.Raw1) != string(in.Raw1) {
		t.Errorf("Raw1 = %s, want %s", got.Raw1, in.Raw1)
	}
	if string(got.Raw2) != string(in.Raw2) {
		t.Errorf("Raw2 = %s, want %s", got.Raw2, in.Raw2)
	}
	if got.Site.String() != in.Site.String() {
		t.Errorf("Site = %q, want %q", got.Site.String(), in.Site.String())
	}
	if got.Big.Cmp(&in.Big) != 0 {
		t.Errorf("Big = %s, want %s", got.Big.String(), in.Big.String())
	}
	if got.BigF.Cmp(&in.BigF) != 0 {
		// big.Float compare can be sensitive to precision — fall back to
		// Text equality which is what we wrote on the wire.
		if got.BigF.Text('g', 20) != in.BigF.Text('g', 20) {
			t.Errorf("BigF = %s, want %s", got.BigF.Text('g', 20), in.BigF.Text('g', 20))
		}
	}
	if got.BigR.Cmp(&in.BigR) != 0 {
		t.Errorf("BigR = %s, want %s", got.BigR.RatString(), in.BigR.RatString())
	}
	if got.ID != in.ID {
		t.Errorf("ID = %s, want %s", got.ID, in.ID)
	}
	if got.GofrsID != in.GofrsID {
		t.Errorf("GofrsID = %s, want %s", got.GofrsID, in.GofrsID)
	}
}

// TestRich_GofrsUUID_AltForms confirms the generated decoder delegates to
// the lib's UnmarshalText, which accepts more forms than the canonical
// 8-4-4-4-12 dashed string: hyphen-less and urn-prefixed pass too. Bytes-
// malformed input still errors.
func TestRich_GofrsUUID_AltForms(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"gofrsId":"550e8400-e29b-41d4-a716-446655440000"}`), // canonical
		[]byte(`{"gofrsId":"550e8400e29b41d4a716446655440000"}`),     // hyphen-less
		[]byte(`{"gofrsId":"urn:uuid:550e8400-e29b-41d4-a716-446655440000"}`),
	}
	for _, c := range cases {
		if _, err := decode.Unmarshal[RichTypes](c); err != nil {
			t.Errorf("unmarshal %s: %v", c, err)
		}
	}
	if _, err := decode.Unmarshal[RichTypes]([]byte(`{"gofrsId":"not-a-uuid-at-all"}`)); err == nil {
		t.Error("expected error on garbage")
	}
}

// TestRich_RawJSON_ZeroCopy: Raw1 should alias the source buffer rather than
// allocate a fresh copy. Detect by comparing the slice header's data pointer
// to the input's range — same backing array via reflect.SliceHeader-style
// check is brittle; instead, we mutate the source after decode and watch
// the field change. (DON'T do this in production code — this is the hazard
// users have to understand.)
func TestRich_RawJSON_ZeroCopy(t *testing.T) {
	in := []byte(`{"raw1":"alpha","raw2":null,"site":"http://x","big":0,"bigF":"0","bigR":"0","id":"00000000-0000-0000-0000-000000000000"}`)
	got, err := decode.Unmarshal[RichTypes](in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := `"alpha"`
	if string(got.Raw1) != want {
		t.Fatalf("Raw1 initial = %s, want %s", got.Raw1, want)
	}
	// Mutate the source buffer — alias should reflect it.
	off := strings.Index(string(in), "alpha")
	if off < 0 {
		t.Skip("payload reshaped, can't verify alias")
	}
	in[off] = 'A'
	if string(got.Raw1) == want {
		t.Errorf("expected zero-copy alias; mutating source did not affect Raw1")
	}
}

// TestRich_URLValidation: bad URL should error from url.Parse, surfacing
// through DecodeFrom's error return.
func TestRich_URLValidation(t *testing.T) {
	bad := []byte(`{"raw1":null,"raw2":null,"site":"://broken","big":0,"bigF":"0","bigR":"0","id":"00000000-0000-0000-0000-000000000000"}`)
	if _, err := decode.Unmarshal[RichTypes](bad); err == nil {
		t.Error("expected url.Parse error")
	}
}

// TestRich_BigIntArbitraryPrecision pushes a value way past int64 range.
func TestRich_BigIntArbitraryPrecision(t *testing.T) {
	huge := strings.Repeat("9", 100) // 100-digit number
	in := []byte(`{"raw1":null,"raw2":null,"site":"http://x","big":` + huge + `,"bigF":"0","bigR":"0","id":"00000000-0000-0000-0000-000000000000"}`)
	got, err := decode.Unmarshal[RichTypes](in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Big.String() != huge {
		t.Errorf("Big = %s, want %s", got.Big.String(), huge)
	}
}

// TestRich_UUIDInvalid surfaces uuid.Parse's error.
func TestRich_UUIDInvalid(t *testing.T) {
	in := []byte(`{"raw1":null,"raw2":null,"site":"http://x","big":0,"bigF":"0","bigR":"0","id":"not-a-uuid"}`)
	if _, err := decode.Unmarshal[RichTypes](in); err == nil {
		t.Error("expected uuid.Parse error")
	}
}

// TestRich_RoundtripDeepEqual checks the higher-level invariant: encoding
// then decoding produces something that compares equal under DeepEqual for
// the simple-value fields (Raw* skipped because alias vs copy differs).
func TestRich_RoundtripDeepEqual(t *testing.T) {
	id := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	site, _ := url.Parse("https://x.test")
	in := RichTypes{Site: *site, ID: id}
	out, _ := encode.Marshal(in)
	got, err := decode.Unmarshal[RichTypes](out)
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if got.ID != in.ID {
		t.Errorf("ID mismatch")
	}
	if !reflect.DeepEqual(got.Site, in.Site) {
		t.Errorf("Site mismatch:\n got:  %#v\n want: %#v", got.Site, in.Site)
	}
}
