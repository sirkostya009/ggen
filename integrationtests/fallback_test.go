package integrationtests

import (
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/decode"
	"github.com/sirkostya009/ggen/encode"
	"github.com/sirkostya009/ggen/integrationtests/thirdparty"
	"github.com/sirkostya009/ggen/integrationtests/thirdparty2"
)

// FallbackStruct references thirdparty.External — a plain Go struct with
// no DecodeFrom and no TextUnmarshaler. The static analyzer sees this and
// emits a plain `encoding/json.Unmarshal` call directly.
//
//ggen:generate
type FallbackStruct struct {
	ID    string              `json:"id"`
	Extra thirdparty.External `json:"extra"`
}

// FastFallbackStruct references thirdparty2.External2 — outside the main
// package's generation pass, but ggen-generated in its own package. The
// static analyzer detects the AppendJSON / DecodeFrom methods at codegen
// time and emits direct method calls, bypassing the json reflective path.
//
//ggen:generate
type FastFallbackStruct struct {
	ID    string                `json:"id"`
	Extra thirdparty2.External2 `json:"extra"`
}

// TextFallbackStruct references thirdparty.Tagged — implements
// TextMarshaler/Unmarshaler. The static analyzer routes encode/decode
// through those methods. Wire format is `"name#tag"`.
//
//ggen:generate
type TextFallbackStruct struct {
	ID  string            `json:"id"`
	Tag thirdparty.Tagged `json:"tag"`
}

// TestFallback_roundtrip exercises nested-struct codegen where the nested
// type has no generated AppendJSON/DecodeFrom. The generator emits
// jsonv2.Marshal / jsonv2.UnmarshalDecode fallbacks.
func TestFallback_roundtrip(t *testing.T) {
	in := FallbackStruct{
		ID:    "abc",
		Extra: thirdparty.External{Key: "k", Value: 42},
	}
	out, _ := encode.MarshalString(in)
	if !strings.Contains(out, `"extra":{"key":"k","value":42}`) {
		t.Errorf("marshal output missing expected shape: %s", out)
	}

	got, err := decode.Unmarshal[FallbackStruct]([]byte(out))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Extra.Key != "k" || got.Extra.Value != 42 {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

// TestFastFallback_roundtrip exercises static cross-pkg dispatch:
// FastFallbackStruct.Extra is thirdparty2.External2, ggen-generated in
// another package. The static analyzer detects DecodeFrom / AppendJSON
// at codegen time, so json.Unmarshal is bypassed.
func TestFastFallback_roundtrip(t *testing.T) {
	in := FastFallbackStruct{
		ID:    "abc",
		Extra: thirdparty2.External2{Key: "k", Value: 42},
	}
	out, _ := encode.MarshalString(in)
	got, err := decode.Unmarshal[FastFallbackStruct]([]byte(out))
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if got.Extra.Key != "k" || got.Extra.Value != 42 {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

// TestFastFallback_validationFromGeneratedDecoder proves the static
// dispatch actually ran: External2's generated DecodeFrom enforces
// ggen:"required" on Key and ggen:"gte=0" on Value. If json.Unmarshal had
// been used, neither rule would fire — stdlib doesn't read ggen tags. So
// a validation.Error from a violating payload confirms static dispatch.
func TestFastFallback_validationFromGeneratedDecoder(t *testing.T) {
	// Empty Key violates required.
	bad := []byte(`{"id":"x","extra":{"key":"","value":1}}`)
	_, err := decode.Unmarshal[FastFallbackStruct](bad)
	if err == nil {
		t.Fatal("expected validation error from generated External2 decoder")
	}
	if !strings.Contains(err.Error(), "key") {
		t.Errorf("error doesn't reference key: %v", err)
	}
}

// TestTextFallback_roundtrip checks that ggen routes Tagged through
// TextMarshaler/Unmarshaler. The wire format is `"name#tag"` — a JSON
// string, not the struct's natural `{"Name":"...","Tag":"..."}` JSON shape
// that json.Marshal would produce. Seeing the string form confirms the
// text path ran.
func TestTextFallback_roundtrip(t *testing.T) {
	in := TextFallbackStruct{ID: "x", Tag: thirdparty.Tagged{Name: "alice", Tag: "admin"}}
	out, _ := encode.MarshalString(in)
	// Must be a string-encoded value, not a sub-object.
	if !strings.Contains(out, `"tag":"alice#admin"`) {
		t.Fatalf("expected text-encoded form, got: %s", out)
	}
	got, err := decode.Unmarshal[TextFallbackStruct]([]byte(out))
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if got.Tag != in.Tag {
		t.Errorf("roundtrip mismatch: got %+v, want %+v", got.Tag, in.Tag)
	}
}

// TestTextFallback_unmarshalErrorPropagates: bad input ("missing #") must
// surface UnmarshalText's error from inside the cross-pkg fallback.
func TestTextFallback_unmarshalErrorPropagates(t *testing.T) {
	bad := []byte(`{"id":"x","tag":"no-separator-here"}`)
	_, err := decode.Unmarshal[TextFallbackStruct](bad)
	if err == nil {
		t.Fatal("expected UnmarshalText error to propagate")
	}
	if !strings.Contains(err.Error(), "missing '#'") {
		t.Errorf("error didn't include UnmarshalText message: %v", err)
	}
}
