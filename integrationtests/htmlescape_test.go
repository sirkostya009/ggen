// HTML-safe string escaping. By default <, > and & are emitted literally. The
// htmlescape annotation opts in to \uXXXX escapes for them.

package integrationtests

//go:generate ../ggen $GOFILE

import (
	"bytes"
	"database/sql"
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

// TestJSONSize_HTMLEscapeStruct and TestJSONSize_HTMLEscapeNested live in
// jsonsize_test.go.
