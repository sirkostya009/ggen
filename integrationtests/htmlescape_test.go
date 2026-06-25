// HTML-safe string escaping. Default emits <, >, & literally (jsonv2-shaped);
// the htmlescape annotation opts in to v1-style \uXXXX escapes.

package integrationtests

//go:generate ../ggen $GOFILE

import (
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/encode"
)

//ggen:generate
type HTMLRawStruct struct {
	Note string `json:"note"`
}

//ggen:generate htmlescape
type HTMLEscapeStruct struct {
	Note string `json:"note"`
}

func TestHTMLEscape_DefaultLiteral(t *testing.T) {
	const in = `<a href="x">tom & jerry</a>`
	out, err := encode.Marshal(HTMLRawStruct{Note: in})
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
	const in = `<a href="x">tom & jerry</a>`
	out, err := encode.Marshal(HTMLEscapeStruct{Note: in})
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

// TestJSONSize_HTMLEscapeStruct lives in jsonsize_test.go.
