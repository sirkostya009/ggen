package integrationtests

//go:generate ../ggen $GOFILE

import (
	"bytes"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"testing"
)

// Wire keys are JSON-escaped once, at generate time. Without that, a name
// with a quote would give invalid JSON with a nil error, and a name with a
// backslash would give a different key.

// KeyEscQuote's field is named q"x via the jsonv2 quoted-section grammar.
//
//ggen:generate
type KeyEscQuote struct {
	A int `json:"'q\"x'"`
}

// KeyEscBackslash's field is named a\b, a backslash then b, under ggen's tag
// unquoting. jsonv2 unescapes the section twice and gets a plus a backspace.
// That name-spelling divergence is pinned in .claude/backlog.md. The output
// must still be valid JSON and round-trip through its own decoder.
//
//ggen:generate
type KeyEscBackslash struct {
	A int `json:"'a\\b'"`
}

// KeyEscHTML pins htmlescape parity for name constants. encoding/json v1
// HTML-escapes keys, so `a&b` marshals as `a\u0026b`.
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
	if !bytes.Equal(got, want) {
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
	if !bytes.Equal(got, want) {
		t.Errorf("ggen %q != encoding/json %q", got, want)
	}
	back, _, err := KeyEscHTML{}.DecodeFrom(got)
	if err != nil || back != v {
		t.Errorf("round-trip = %+v, %v; want %+v", back, err, v)
	}
}
