//go:build goexperiment.jsonv2

package integrationtests

//go:generate ../ggen $GOFILE

// Coverage for the rich built-in kinds: json.RawMessage / jsontext.Value,
// url.URL, math/big, and google/uuid.

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
	"github.com/sirkostya009/ggen/encode"
)

// RichTypes mixes every rich kind. ID and GofrsID cover both major UUID
// libraries, served by the same TextMarshaler/Unmarshaler dispatch.
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

// TestRich_Roundtrip: marshal → unmarshal preserves every field. Big values
// exceed int64 range to exercise arbitrary-precision paths.
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
	got, _, err := RichTypes{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}

	// Raw bytes round-trip byte-equal.
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
		// big.Float Cmp is precision-sensitive; fall back to wire Text equality.
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

// TestRich_GofrsUUID_AltForms: decode delegates to the lib's UnmarshalText, so
// hyphen-less and urn-prefixed forms pass; garbage still errors.
func TestRich_GofrsUUID_AltForms(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"gofrsId":"550e8400-e29b-41d4-a716-446655440000"}`), // canonical
		[]byte(`{"gofrsId":"550e8400e29b41d4a716446655440000"}`),     // hyphen-less
		[]byte(`{"gofrsId":"urn:uuid:550e8400-e29b-41d4-a716-446655440000"}`),
	}
	for _, c := range cases {
		if _, _, err := (RichTypes{}).DecodeFrom(c); err != nil {
			t.Errorf("unmarshal %s: %v", c, err)
		}
	}
	if _, _, err := (RichTypes{}).DecodeFrom([]byte(`{"gofrsId":"not-a-uuid-at-all"}`)); err == nil {
		t.Error("expected error on garbage")
	}
}

// TestRich_RawJSON_ZeroCopy: Raw1 aliases the source buffer. Verified by
// mutating the source post-decode and watching the field change.
func TestRich_RawJSON_ZeroCopy(t *testing.T) {
	in := []byte(`{"raw1":"alpha","raw2":null,"site":"http://x","big":0,"bigF":"0","bigR":"0","id":"00000000-0000-0000-0000-000000000000"}`)
	got, _, err := RichTypes{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := `"alpha"`
	if string(got.Raw1) != want {
		t.Fatalf("Raw1 initial = %s, want %s", got.Raw1, want)
	}
	// Mutate the source — alias must reflect it.
	off := strings.Index(string(in), "alpha")
	if off < 0 {
		t.Skip("payload reshaped, can't verify alias")
	}
	in[off] = 'A'
	if string(got.Raw1) == want {
		t.Errorf("expected zero-copy alias; mutating source did not affect Raw1")
	}
}

// TestRich_URLValidation: a bad URL surfaces url.Parse's error.
func TestRich_URLValidation(t *testing.T) {
	bad := []byte(`{"raw1":null,"raw2":null,"site":"://broken","big":0,"bigF":"0","bigR":"0","id":"00000000-0000-0000-0000-000000000000"}`)
	if _, _, err := (RichTypes{}).DecodeFrom(bad); err == nil {
		t.Error("expected url.Parse error")
	}
}

// TestRich_BigIntArbitraryPrecision pushes a value way past int64 range.
func TestRich_BigIntArbitraryPrecision(t *testing.T) {
	huge := strings.Repeat("9", 100) // 100-digit number
	in := []byte(`{"raw1":null,"raw2":null,"site":"http://x","big":` + huge + `,"bigF":"0","bigR":"0","id":"00000000-0000-0000-0000-000000000000"}`)
	got, _, err := RichTypes{}.DecodeFrom(in)
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
	if _, _, err := (RichTypes{}).DecodeFrom(in); err == nil {
		t.Error("expected uuid.Parse error")
	}
}

// TestRich_RoundtripDeepEqual: encode→decode is DeepEqual for simple-value
// fields (Raw* skipped — alias vs copy differs).
func TestRich_RoundtripDeepEqual(t *testing.T) {
	id := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	site, _ := url.Parse("https://x.test")
	in := RichTypes{Site: *site, ID: id}
	out, _ := encode.Marshal(in)
	got, _, err := RichTypes{}.DecodeFrom(out)
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
