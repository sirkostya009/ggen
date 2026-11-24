// HTML-safe string escaping. By default <, > and & are emitted literally. The
// htmlescape annotation opts in to \uXXXX escapes for them.

package integrationtests

//go:generate ../ggen $GOFILE

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen"
)

//ggen:generate
type HTMLRawStruct struct {
	Note string `json:"note"`
}

//ggen:generate htmlescape
type HTMLEscapeStruct struct {
	Note    string `json:"note"`
	Payload any    `json:"payload,omitempty"`
}

func TestHTMLEscape_DefaultLiteral(t *testing.T) {
	t.Parallel()
	const in = `<a href="x">tom & jerry</a>`
	out, err := ggen.Marshal(HTMLRawStruct{Note: in})
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, lit := range []string{"<", ">", "&"} {
		if !strings.Contains(got, lit) {
			t.Errorf("default mode unexpectedly escaped %q in output: %s", lit, got)
		}
	}
	for _, esc := range []string{"\\u003c", "\\u003e", "\\u0026"} {
		if strings.Contains(got, esc) {
			t.Errorf("default mode emitted escape %s in output: %s", esc, got)
		}
	}
}

func TestHTMLEscape_OptIn(t *testing.T) {
	t.Parallel()
	const in = `<a href="x">tom & jerry</a>`
	out, err := ggen.Marshal(HTMLEscapeStruct{Note: in})
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, lit := range []string{"<", ">", "&"} {
		if strings.Contains(got, lit) {
			t.Errorf("htmlescape mode leaked literal %q in output: %s", lit, got)
		}
	}
	for _, esc := range []string{"\\u003c", "\\u003e", "\\u0026"} {
		if !strings.Contains(got, esc) {
			t.Errorf("htmlescape mode missing escape %s in output: %s", esc, got)
		}
	}
}

// HTMLEscapeNested holds strings two container levels down and in a
// sql.NullString. htmlescape escapes them like a top-level string.
//
//ggen:generate htmlescape
type HTMLEscapeNested struct {
	Rows  [][]string            `json:"rows"`
	Fixed [2][]string           `json:"fixed"`
	Pairs [][2]string           `json:"pairs"`
	Deep  map[string][][]string `json:"deep"`
	Anys  [][]any               `json:"anys"`
	Addrs [][]netip.Addr        `json:"addrs"`
	Null  sql.NullString        `json:"null"`
}

func htmlEscapeNested(s string) HTMLEscapeNested {
	return HTMLEscapeNested{
		Rows:  [][]string{{s}},
		Fixed: [2][]string{{s}, {s}},
		Pairs: [][2]string{{s, s}},
		Deep:  map[string][][]string{"k": {{s}}},
		Anys:  [][]any{{s}},
		Addrs: [][]netip.Addr{{netip.MustParseAddr("fe80::1%" + s)}},
		Null:  sql.NullString{String: s, Valid: true},
	}
}

func TestHTMLEscape_NestedPositions(t *testing.T) {
	t.Parallel()
	in := htmlEscapeNested("</script>&")
	out, err := ggen.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if i := bytes.IndexAny(out, "<>&"); i >= 0 {
		t.Errorf("htmlescape leaked %q at offset %d: %s", out[i], i, out)
	}
	got, _, err := HTMLEscapeNested{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, in)
	}
}

// HTMLEscapeRaw passes raw JSON through at every position a raw value takes.
//
//ggen:generate htmlescape
type HTMLEscapeRaw struct {
	Meta  json.RawMessage            `json:"meta"`
	Val   jsontext.Value             `json:"val"`
	Metas []json.RawMessage          `json:"metas"`
	ByKey map[string]json.RawMessage `json:"byKey"`
	Any   any                        `json:"any"`
	Rest  map[string]json.RawMessage `json:",embed"`
}

// htmlLabel writes itself as a JSON string without HTML escaping.
type htmlLabel struct{ S string }

func (l htmlLabel) MarshalJSON() ([]byte, error) { return []byte(`"` + l.S + `"`), nil }

const htmlRawInput = `{"meta":"</script>&","val":{"<k>":"a&b"},"metas":["<x>"],"byKey":{"k":"<&>"},"any":null,"extra":"</script>"}`

// Raw values are HTML-escaped like strings, from typed fields and from an
// any, as encoding/json and jsonv2 with EscapeForHTML do.
func TestHTMLEscape_RawValues(t *testing.T) {
	t.Parallel()
	v, _, err := HTMLEscapeRaw{}.DecodeFrom([]byte(htmlRawInput))
	if err != nil {
		t.Fatal(err)
	}
	v.Any = []any{v.Meta, htmlLabel{"</script>"}, []json.RawMessage{v.Meta}}
	out, err := ggen.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if i := bytes.IndexAny(out, "<>&"); i >= 0 {
		t.Errorf("htmlescape leaked %q at offset %d: %s", out[i], i, out)
	}
	var got, want any
	if err := jsonv2.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	ref, err := json.Marshal(map[string]any{
		"meta": v.Meta, "val": v.Val, "metas": v.Metas, "byKey": v.ByKey,
		"any": v.Any, "extra": v.Rest["extra"],
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jsonv2.Unmarshal(ref, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("value changed:\n got %s\nwant %s", out, ref)
	}
}

func TestAppendAnyHTML_RawValues(t *testing.T) {
	t.Parallel()
	for _, v := range []any{
		json.RawMessage(`{"<k>":"a&b"}`),
		[]json.RawMessage{json.RawMessage(`"<b>"`), nil},
		htmlLabel{"<b>&"},
	} {
		got, err := ggen.AppendAnyHTML(nil, v)
		if err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%T: got %s, want %s", v, got, want)
		}
		if n := ggen.AnySizeHTML(v); len(got) > n {
			t.Errorf("%T: AnySizeHTML %d under the %d bytes written", v, n, len(got))
		}
		plain, err := ggen.AppendAny(nil, v)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.ContainsAny(plain, "<>&") {
			t.Errorf("%T: AppendAny escaped a raw value: %s", v, plain)
		}
	}
}

// TestJSONSize_HTMLEscapeStruct, TestJSONSize_HTMLEscapeNested and
// TestJSONSize_HTMLEscapeRaw live in jsonsize_test.go.
