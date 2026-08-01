package integrationtests

//go:generate ../ggen $GOFILE

import (
	"encoding/json"
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

// A nested type with no generated methods uses the json fallback both ways.
func TestFallback_roundtrip(t *testing.T) {
	t.Parallel()
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

// Cross-pkg dispatch to External2's generated methods round-trips.
func TestFastFallback_roundtrip(t *testing.T) {
	t.Parallel()
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

// A validation error on the nested field proves the generated decoder ran
// (the json fallback wouldn't validate).
func TestFastFallback_validationFromGeneratedDecoder(t *testing.T) {
	t.Parallel()
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

// Tagged routes through TextMarshaler/Unmarshaler; the "name#tag" string
// form (not a sub-object) confirms the text path ran.
func TestTextFallback_roundtrip(t *testing.T) {
	t.Parallel()
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

// UnmarshalText's error surfaces.
func TestTextFallback_unmarshalErrorPropagates(t *testing.T) {
	t.Parallel()
	bad := []byte(`{"id":"x","tag":"no-separator-here"}`)
	_, _, err := TextFallbackStruct{}.DecodeFrom(bad)
	if err == nil {
		t.Fatal("expected UnmarshalText error to propagate")
	}
	if !strings.Contains(err.Error(), "missing '#'") {
		t.Errorf("error didn't include UnmarshalText message: %v", err)
	}
}

// ---- cross-package ggen types in every field position ----------------
//
// A VALUE field never spells the foreign type out, which is why the container
// and pointer positions were the ones emitting a file that named a package it
// never imported. The marshal side had its own trap: the AppendJSON signature
// probe looked for `func([]byte) []byte` while the emitter writes
// `([]byte, error)`, so every cross-package value fell through to
// encoding/json.

//ggen:generate
type CrossPkgShapes struct {
	One  thirdparty2.External2            `json:"one"`
	Many []thirdparty2.External2          `json:"many"`
	Dict map[string]thirdparty2.External2 `json:"dict"`
	Ptr  *thirdparty2.External2           `json:"ptr"`
	PPtr **thirdparty2.External2          `json:"pptr"`
	Slab []*thirdparty2.External2         `json:"slab"`
	Arr  [2]thirdparty2.External2         `json:"arr"`
}

func TestCrossPkg_AllPositions(t *testing.T) {
	const payload = `{
		"one":{"key":"a","value":1},
		"many":[{"key":"b","value":2},{"key":"c","value":3}],
		"dict":{"d":{"key":"d","value":4}},
		"ptr":{"key":"e","value":5},
		"pptr":{"key":"f","value":6},
		"slab":[{"key":"g","value":7}],
		"arr":[{"key":"h","value":8},{"key":"i","value":9}]
	}`

	got, _, err := CrossPkgShapes{}.DecodeFrom([]byte(payload))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.One.Key != "a" || got.One.Value != 1 {
		t.Errorf("value field: %+v", got.One)
	}
	if len(got.Many) != 2 || got.Many[1].Key != "c" {
		t.Errorf("slice field: %+v", got.Many)
	}
	if got.Dict["d"].Value != 4 {
		t.Errorf("map field: %+v", got.Dict)
	}
	if got.Ptr == nil || got.Ptr.Key != "e" {
		t.Errorf("pointer field: %+v", got.Ptr)
	}
	if got.PPtr == nil || *got.PPtr == nil || (*got.PPtr).Key != "f" {
		t.Errorf("double-pointer field: %+v", got.PPtr)
	}
	if len(got.Slab) != 1 || got.Slab[0].Value != 7 {
		t.Errorf("pointer-slab field: %+v", got.Slab)
	}
	if got.Arr[1].Key != "i" {
		t.Errorf("array field: %+v", got.Arr)
	}

	// Marshal has to reach the foreign type's own AppendJSON, and round-trip.
	out, err := got.AppendJSON(nil)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	back, _, err := CrossPkgShapes{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("re-decode %s: %v", out, err)
	}
	if back.One != got.One || back.Dict["d"] != got.Dict["d"] || back.Arr != got.Arr {
		t.Errorf("round-trip mismatch:\n got %+v\nback %+v", got, back)
	}
	if size, n := got.JSONSize(), len(out); size < n {
		t.Errorf("JSONSize %d under-estimates output %d", size, n)
	}
}

// The foreign type's methods must be reached through a pointer too: `*T`'s
// method set contains T's, so the probe has to look at the pointee.
func TestCrossPkg_PointerUsesGeneratedMethods(t *testing.T) {
	v, _, err := CrossPkgShapes{}.DecodeFrom([]byte(`{"ptr":{"key":"","value":1}}`))
	if err == nil {
		t.Fatalf("expected the foreign type's own `required minlen=1` rule to fire, got %+v", v)
	}
}

// The cross-package TextAppender marshal arm used to drop AppendText output
// raw between quotes — a text carrying `"`/`\` emitted invalid JSON with a
// nil error. thirdparty.QuotedText's text is caller-controlled bytes.
//
//ggen:generate
type TextAppenderStruct struct {
	T thirdparty.QuotedText `json:"t"`
}

func TestTextAppender_OutputEscaped(t *testing.T) {
	t.Parallel()
	got, _, err := TextAppenderStruct{}.DecodeFrom([]byte(`{"t":"a\"b\\c"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.T.V != `a"b\c` {
		t.Fatalf("V = %q", got.T.V)
	}
	out, err := encode.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(out) {
		t.Errorf("invalid JSON: %s", out)
	}
	if want := `{"t":"a\"b\\c"}`; string(out) != want {
		t.Errorf("marshal = %s, want %s", out, want)
	}
	// Clean text stays byte-identical to the raw fast path.
	clean := TextAppenderStruct{T: thirdparty.QuotedText{V: "plain"}}
	out, _ = encode.Marshal(clean)
	if string(out) != `{"t":"plain"}` {
		t.Errorf("clean = %s", out)
	}
}
