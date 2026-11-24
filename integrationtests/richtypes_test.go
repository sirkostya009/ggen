package integrationtests

//go:generate ../ggen $GOFILE

// Coverage for the rich built-in kinds: json.RawMessage, jsontext.Value,
// url.URL, math/big, and the google and gofrs uuid packages.

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"math/big"
	"net/url"
	"reflect"
	"strings"
	"testing"

	gofrs "github.com/gofrs/uuid/v5"
	"github.com/google/uuid"

	"github.com/sirkostya009/ggen"
)

// RichTypes mixes every rich kind. ID and GofrsID cover both major UUID
// libraries. Both go through the same TextMarshaler and TextUnmarshaler code.
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

// Marshal then unmarshal preserves every field. The big values exceed the
// int64 range to exercise the arbitrary-precision code.
func TestRich_Roundtrip(t *testing.T) {
	t.Parallel()
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
	out, _ := ggen.Marshal(in)
	got, _, err := RichTypes{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}

	// Raw values come back byte for byte.
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
		// big.Float Cmp depends on precision, so compare the text form
		// instead.
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

// Decode calls the library's UnmarshalText, so the forms without hyphens and
// with a urn prefix are accepted. Garbage still fails.
func TestRich_GofrsUUID_AltForms(t *testing.T) {
	t.Parallel()
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

// Raw1 aliases the source buffer (points into it, no copy). Changing the
// source after decode changes the field.
func TestRich_RawJSON_ZeroCopy(t *testing.T) {
	t.Parallel()
	in := []byte(`{"raw1":"alpha","raw2":null,"site":"http://x","big":0,"bigF":"0","bigR":"0","id":"00000000-0000-0000-0000-000000000000"}`)
	got, _, err := RichTypes{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := `"alpha"`
	if string(got.Raw1) != want {
		t.Fatalf("Raw1 initial = %s, want %s", got.Raw1, want)
	}
	off := bytes.Index(in, []byte("alpha"))
	if off < 0 {
		t.Skip("payload reshaped, can't verify alias")
	}
	in[off] = 'A'
	if string(got.Raw1) == want {
		t.Errorf("expected zero-copy alias; mutating source did not affect Raw1")
	}
}

// A bad URL surfaces url.Parse's error.
func TestRich_URLValidation(t *testing.T) {
	t.Parallel()
	bad := []byte(`{"raw1":null,"raw2":null,"site":"://broken","big":0,"bigF":"0","bigR":"0","id":"00000000-0000-0000-0000-000000000000"}`)
	if _, _, err := (RichTypes{}).DecodeFrom(bad); err == nil {
		t.Error("expected url.Parse error")
	}
}

// A big.Int value far past int64 range round-trips.
func TestRich_BigIntArbitraryPrecision(t *testing.T) {
	t.Parallel()
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

// An invalid UUID surfaces uuid.Parse's error.
func TestRich_UUIDInvalid(t *testing.T) {
	t.Parallel()
	in := []byte(`{"raw1":null,"raw2":null,"site":"http://x","big":0,"bigF":"0","bigR":"0","id":"not-a-uuid"}`)
	if _, _, err := (RichTypes{}).DecodeFrom(in); err == nil {
		t.Error("expected uuid.Parse error")
	}
}

// Encode then decode gives DeepEqual simple-value fields. The Raw* fields are
// skipped because an aliased value and a copied value compare differently.
func TestRich_RoundtripDeepEqual(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	site, _ := url.Parse("https://x.test")
	in := RichTypes{Site: *site, ID: id}
	out, _ := ggen.Marshal(in)
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

// RawOnly holds only a raw JSON field, so every allocation of the stream
// decode comes from that field.
//
//ggen:generate
type RawOnly struct {
	Raw json.RawMessage `json:"raw"`
}

// RawOnlyCopy is RawOnly under copy, where the receiver owns its raw backing
// on both paths.
//
//ggen:generate copy
type RawOnlyCopy struct {
	Raw json.RawMessage `json:"raw"`
}

// A copy-mode receiver's raw backing is refilled, not reallocated. So a
// steady stream of values with the same shape reaches zero allocations per
// decode. Without copy the carried span may alias an earlier DecodeFrom input,
// so each decode allocates a fresh one.
func TestRawJSON_StreamReusesReceiverBacking(t *testing.T) { //nolint:paralleltest // measures allocations
	payload := []byte(`{"raw":{"nested":[1,2,3],"s":"abcdefghij"}}` + "\n")
	var s ggen.Stream
	buf := make([]byte, 0, 256)
	rd := bytes.NewReader(payload)
	prev := RawOnlyCopy{}
	n := testing.AllocsPerRun(200, func() {
		rd.Reset(payload)
		s.Reset(rd, buf)
		v, err := prev.DecodeFromStream(&s)
		if err != nil {
			t.Fatal(err)
		}
		prev = v
	})
	if n != 0 {
		t.Errorf("copy: steady-state stream decode: %v allocs/op, want 0", n)
	}
	plain := RawOnly{}
	n = testing.AllocsPerRun(200, func() {
		rd.Reset(payload)
		s.Reset(rd, buf)
		v, err := plain.DecodeFromStream(&s)
		if err != nil {
			t.Fatal(err)
		}
		plain = v
	})
	if n != 1 {
		t.Errorf("plain: steady-state stream decode: %v allocs/op, want 1", n)
	}
}

// BigFloatStruct isolates a big.Float field.
//
//ggen:generate
type BigFloatStruct struct {
	F big.Float `json:"f"`
}

// big.Float decodes what its UnmarshalText (used by encoding/json) accepts:
// base prefixes, a hex mantissa with a p exponent and _ separators.
func TestRich_BigFloatTextForms(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"1.5", "-2e3", "Inf", "0x1p-3", "0X.8P1", "1_000", "0b101", "0o17", "0x_1f", "1_0.2_5e1_0",
		"1__0", "_1", "1_", "0x", "0b2", "0x1.p", "1._5", "08", "0o8",
	} {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			var want big.Float
			wantErr := want.UnmarshalText([]byte(in))
			payload := `{"f":"` + in + `"}`
			got, _, err := BigFloatStruct{}.DecodeFrom([]byte(payload))
			gotS, errS := ggen.NewStream(strings.NewReader(payload), nil).Value[BigFloatStruct]()
			if (wantErr == nil) != (err == nil) || (wantErr == nil) != (errS == nil) {
				t.Fatalf("UnmarshalText err %v, bytes err %v, stream err %v", wantErr, err, errS)
			}
			if wantErr != nil {
				return
			}
			if got.F.Cmp(&want) != 0 || gotS.F.Cmp(&want) != 0 {
				t.Errorf("bytes %s, stream %s, want %s", got.F.String(), gotS.F.String(), want.String())
			}
		})
	}
}
