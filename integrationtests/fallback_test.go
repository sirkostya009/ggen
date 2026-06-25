package integrationtests

//go:generate ../ggen $GOFILE

import (
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/encode"
	"github.com/sirkostya009/ggen/integrationtests/thirdparty"
	"github.com/sirkostya009/ggen/integrationtests/thirdparty2"
)

// FallbackStruct: thirdparty.External has no DecodeFrom/TextUnmarshaler, so
// codegen emits an encoding/json.Unmarshal fallback.
//
//ggen:generate
type FallbackStruct struct {
	ID    string              `json:"id"`
	Extra thirdparty.External `json:"extra"`
}

// FastFallbackStruct: thirdparty2.External2 is ggen-generated in its own
// package, so codegen emits direct AppendJSON/DecodeFrom calls (no json path).
//
//ggen:generate
type FastFallbackStruct struct {
	ID    string                `json:"id"`
	Extra thirdparty2.External2 `json:"extra"`
}

// TextFallbackStruct: thirdparty.Tagged implements TextMarshaler/Unmarshaler;
// codegen routes through those. Wire form is "name#tag".
//
//ggen:generate
type TextFallbackStruct struct {
	ID  string            `json:"id"`
	Tag thirdparty.Tagged `json:"tag"`
}

// TestFallback_roundtrip: nested type with no generated methods uses the
// json fallback both ways.
func TestFallback_roundtrip(t *testing.T) {
	in := FallbackStruct{
		ID:    "abc",
		Extra: thirdparty.External{Key: "k", Value: 42},
	}
	out, _ := encode.MarshalString(in)
	if !strings.Contains(out, `"extra":{"key":"k","value":42}`) {
		t.Errorf("marshal output missing expected shape: %s", out)
	}

	got, _, err := FallbackStruct{}.DecodeFrom([]byte(out))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Extra.Key != "k" || got.Extra.Value != 42 {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

// TestFastFallback_roundtrip: cross-pkg dispatch to External2's generated
// methods round-trips.
func TestFastFallback_roundtrip(t *testing.T) {
	in := FastFallbackStruct{
		ID:    "abc",
		Extra: thirdparty2.External2{Key: "k", Value: 42},
	}
	out, _ := encode.MarshalString(in)
	got, _, err := FastFallbackStruct{}.DecodeFrom([]byte(out))
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if got.Extra.Key != "k" || got.Extra.Value != 42 {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

// TestFastFallback_validationFromGeneratedDecoder: a validation error on the
// nested field proves the generated decoder ran (json fallback wouldn't validate).
func TestFastFallback_validationFromGeneratedDecoder(t *testing.T) {
	// Empty Key violates required.
	bad := []byte(`{"id":"x","extra":{"key":"","value":1}}`)
	_, _, err := FastFallbackStruct{}.DecodeFrom(bad)
	if err == nil {
		t.Fatal("expected validation error from generated External2 decoder")
	}
	if !strings.Contains(err.Error(), "key") {
		t.Errorf("error doesn't reference key: %v", err)
	}
}

// TestTextFallback_roundtrip: Tagged routes through TextMarshaler/Unmarshaler;
// the "name#tag" string form (not a sub-object) confirms the text path ran.
func TestTextFallback_roundtrip(t *testing.T) {
	in := TextFallbackStruct{ID: "x", Tag: thirdparty.Tagged{Name: "alice", Tag: "admin"}}
	out, _ := encode.MarshalString(in)
	if !strings.Contains(out, `"tag":"alice#admin"`) {
		t.Fatalf("expected text-encoded form, got: %s", out)
	}
	got, _, err := TextFallbackStruct{}.DecodeFrom([]byte(out))
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if got.Tag != in.Tag {
		t.Errorf("roundtrip mismatch: got %+v, want %+v", got.Tag, in.Tag)
	}
}

// TestTextFallback_unmarshalErrorPropagates: UnmarshalText's error surfaces.
func TestTextFallback_unmarshalErrorPropagates(t *testing.T) {
	bad := []byte(`{"id":"x","tag":"no-separator-here"}`)
	_, _, err := TextFallbackStruct{}.DecodeFrom(bad)
	if err == nil {
		t.Fatal("expected UnmarshalText error to propagate")
	}
	if !strings.Contains(err.Error(), "missing '#'") {
		t.Errorf("error didn't include UnmarshalText message: %v", err)
	}
}
