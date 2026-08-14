//go:build goexperiment.jsonv2

package integrationtests

//go:generate ../ggen $GOFILE

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"testing"
)

// Wire keys are escaped STATICALLY at generate time — a quote-bearing name
// used to emit invalid JSON with a nil error, and a backslash-bearing name a
// silently different key.

// KeyEscQuote's field is named q"x via the jsonv2 quoted-section grammar.
//
//ggen:generate
type KeyEscQuote struct {
	A int `json:"'q\"x'"`
}

// KeyEscBackslash's field is named a\b (backslash + b) under ggen's tag
// unquoting. jsonv2 double-unescapes the section to a+backspace — a pinned
// name-SPELLING divergence (see backlog); the output must still be valid
// JSON and self-round-trip.
//
//ggen:generate
type KeyEscBackslash struct {
	A int `json:"'a\\b'"`
}

// KeyEscHTML pins htmlescape parity for NAME constants: encoding/json v1
// HTML-escapes keys, so `a&b` marshals as a&b.
//
//ggen:generate htmlescape
type KeyEscHTML struct {
	A int `json:"a&b"`
}

func TestKeyEscape_QuoteParityWithJSONv2(t *testing.T) {
	t.Parallel()
	v := KeyEscQuote{A: 7}
	got, err := v.AppendJSON(nil)
	if err != nil {
		t.Fatalf("AppendJSON: %v", err)
	}
	want, err := jsonv2.Marshal(v)
	if err != nil {
		t.Fatalf("jsonv2.Marshal: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("ggen %q != jsonv2 %q", got, want)
	}
	back, _, err := KeyEscQuote{}.DecodeFrom(got)
	if err != nil {
		t.Fatalf("round-trip decode: %v", err)
	}
	if back != v {
		t.Errorf("round-trip = %+v, want %+v", back, v)
	}
}

func TestKeyEscape_BackslashValidAndRoundTrips(t *testing.T) {
	t.Parallel()
	v := KeyEscBackslash{A: 9}
	got, err := v.AppendJSON(nil)
	if err != nil {
		t.Fatalf("AppendJSON: %v", err)
	}
	if !json.Valid(got) {
		t.Fatalf("output is not valid JSON: %q", got)
	}
	back, _, err := KeyEscBackslash{}.DecodeFrom(got)
	if err != nil {
		t.Fatalf("round-trip decode: %v", err)
	}
	if back != v {
		t.Errorf("round-trip = %+v, want %+v", back, v)
	}
}

func TestKeyEscape_HTMLEscapedName(t *testing.T) {
	t.Parallel()
	v := KeyEscHTML{A: 3}
	got, err := v.AppendJSON(nil)
	if err != nil {
		t.Fatalf("AppendJSON: %v", err)
	}
	want, err := json.Marshal(v) // v1 HTML-escapes keys
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("ggen %q != encoding/json %q", got, want)
	}
	back, _, err := KeyEscHTML{}.DecodeFrom(got)
	if err != nil || back != v {
		t.Errorf("round-trip = %+v, %v; want %+v", back, err, v)
	}
}
