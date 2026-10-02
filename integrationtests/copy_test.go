package integrationtests

//go:generate ../ggen $GOFILE

// In -copy mode the bytes path copies retained values out of the input
// instead of aliasing it. An aliased string points into the caller's input
// bytes, with no copy. The copied values are strings, map keys and values,
// slice elements, json.RawMessage and strings inside an any. So the decoded
// value survives a later change to the source buffer, the same lifetime the
// stream path gives. CopyDoc carries `//ggen:generate copy`. AliasDoc is the
// default aliasing control.

import (
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"slices"
	"sort"
	"strings"
	"testing"
)

//ggen:generate copy
type CopyDoc struct {
	RawMap   map[string]json.RawMessage `json:"rawMap"`
	Name     string                     `json:"name"`
	Tags     []string                   `json:"tags"`
	Props    map[string]string          `json:"props"`
	Extra    any                        `json:"extra"`
	Raw      json.RawMessage            `json:"raw"`
	Refs     []*CopyRef                 `json:"refs"`
	Children []CopyDoc                  `json:"children"`
}

//ggen:generate copy
type CopyRef struct {
	Label string `json:"label"`
}

// AliasDoc mirrors CopyDoc's leading field without `copy`. Its Name aliases
// the input, so it is the negative control that proves overwriting the source
// (the scribble) has a visible effect.
//
//ggen:generate
type AliasDoc struct {
	Name string `json:"name"`
}

// TestCopy_AliasVsCopy proves that -copy decouples the decoded value from the
// input buffer. After the source bytes are overwritten, the aliasing AliasDoc
// is corrupted, which shows the overwrite works. Every retained CopyDoc value
// stays intact: string, slice element, map key and value, any, and
// json.RawMessage.
func TestCopy_AliasVsCopy(t *testing.T) {
	t.Parallel()
	const js = `{"name":"hello world","tags":["alpha","beta"],"props":{"key":"value"},"extra":"xtra","raw":{"a":"b"},"refs":[{"label":"ref-one"}]}`

	// Alias control on a Name-only payload. Mutating the source must corrupt
	// the decoded string. Otherwise the scribble has no effect and the copy
	// assertions prove nothing.
	a := []byte(`{"name":"hello world"}`)
	ad, _, err := AliasDoc{}.DecodeFrom(a)
	if err != nil {
		t.Fatalf("AliasDoc decode: %v", err)
	}
	if ad.Name != "hello world" {
		t.Fatalf("AliasDoc.Name = %q, want %q", ad.Name, "hello world")
	}
	for i := range a {
		a[i] = 'Z'
	}
	if ad.Name == "hello world" {
		t.Fatal("aliasing decode survived input mutation — scribble ineffective, copy assertions cannot detect aliasing")
	}

	// Copy mode: the same mutation leaves every retained value intact.
	c := []byte(js)
	cd, _, err := CopyDoc{}.DecodeFrom(c)
	if err != nil {
		t.Fatalf("CopyDoc decode: %v", err)
	}
	for i := range c {
		c[i] = 'Z'
	}
	if cd.Name != "hello world" {
		t.Errorf("Name aliased input: got %q", cd.Name)
	}
	if !slices.Equal(cd.Tags, []string{"alpha", "beta"}) {
		t.Errorf("Tags aliased input: got %q", cd.Tags)
	}
	if cd.Props["key"] != "value" {
		t.Errorf("Props aliased input: got %q", cd.Props)
	}
	if s, ok := cd.Extra.(string); !ok || s != "xtra" {
		t.Errorf("Extra (any) aliased input: got %#v", cd.Extra)
	}
	if string(cd.Raw) != `{"a":"b"}` {
		t.Errorf("Raw aliased input: got %s", cd.Raw)
	}
	if len(cd.Refs) != 1 || cd.Refs[0] == nil || cd.Refs[0].Label != "ref-one" {
		t.Errorf("Refs aliased input: got %#v", cd.Refs)
	}
}

// TestCopy_NestedDecouples runs the decouple check over a deep tree. It
// covers every retained-string site at depth: field strings, string elements
// of slices and maps, map keys, json.RawMessage spans, strings and object
// keys inside an any (ggen.AnyCopy), and structs behind pointer-slice
// elements. A content fingerprint taken before and after scribbling the
// source must be identical.
func TestCopy_NestedDecouples(t *testing.T) {
	t.Parallel()
	const js = `{
		"name": "root",
		"tags": ["t0", "t1", "t2"],
		"props": {"alpha": "A", "beta": "B"},
		"extra": {"nested": ["x", "y"], "k": "v"},
		"raw": {"deep": {"arr": [1, "two", true]}},
		"refs": [{"label": "r0"}, {"label": "r1"}],
		"children": [
			{
				"name": "child0",
				"tags": ["c0t"],
				"props": {"ck": "cv"},
				"extra": "child-extra",
				"raw": ["a", "b"],
				"refs": [{"label": "cr0"}],
				"children": [
					{"name": "grandchild", "tags": ["g"], "extra": ["deep1", "deep2"], "raw": "gr"}
				]
			}
		]
	}`
	src := []byte(js)
	got, _, err := CopyDoc{}.DecodeFrom(src)
	if err != nil {
		t.Fatalf("CopyDoc decode: %v", err)
	}

	before := copyFingerprint(&got)
	if len(before) == 0 {
		t.Fatal("fingerprint empty — payload exercised no string content")
	}
	for i := range src {
		src[i] = 'Z'
	}
	after := copyFingerprint(&got)

	if !slices.Equal(before, after) {
		t.Fatalf("decoded CopyDoc aliases the input: fingerprint diverged after source mutation\nbefore: %v\nafter:  %v", before, after)
	}
}

// TestCopy_EscapedDecouples proves -copy decouples and correctly unescapes at
// every retained-string site when the value takes the escape path. That path
// builds the unescaped string in a fresh buffer of its own, so it never
// aliases the input. The NestedDecouples payloads have no escapes and never
// take it, so this test pins the escape path for a field string, a slice
// element, a map key and value, an any string and a pointer leaf. Each one
// carries a two-char escape, a \uXXXX escape or a surrogate pair, the two
// \uXXXX escapes that encode one character above U+FFFF. Every decoded value
// must equal the unescaped Go string both before and after the source is
// scribbled.
func TestCopy_EscapedDecouples(t *testing.T) {
	t.Parallel()
	const js = `{"name":"a\nb\"c\\d\u00e9\ud83d\ude00","tags":["t\u00e9","plain"],"props":{"k\ny":"v\"w"},"extra":"x\ud83d\ude00","refs":[{"label":"r\t0"}]}`

	wantName := "a\nb\"c\\dé\U0001F600"
	wantTag0 := "té"
	wantPropKey, wantPropVal := "k\ny", "v\"w"
	wantExtra := "x\U0001F600"
	wantLabel := "r\t0"

	src := []byte(js)
	cd, _, err := CopyDoc{}.DecodeFrom(src)
	if err != nil {
		t.Fatalf("CopyDoc escaped decode: %v", err)
	}

	check := func(when string) {
		if cd.Name != wantName {
			t.Errorf("%s: Name = %q, want %q", when, cd.Name, wantName)
		}
		if len(cd.Tags) == 0 || cd.Tags[0] != wantTag0 {
			t.Errorf("%s: Tags[0] = %q, want %q", when, cd.Tags, wantTag0)
		}
		if cd.Props[wantPropKey] != wantPropVal {
			t.Errorf("%s: Props[%q] = %q, want %q", when, wantPropKey, cd.Props[wantPropKey], wantPropVal)
		}
		if s, ok := cd.Extra.(string); !ok || s != wantExtra {
			t.Errorf("%s: Extra = %#v, want %q", when, cd.Extra, wantExtra)
		}
		if len(cd.Refs) == 0 || cd.Refs[0] == nil || cd.Refs[0].Label != wantLabel {
			t.Errorf("%s: Refs[0].Label = %#v, want %q", when, cd.Refs, wantLabel)
		}
	}
	check("before scribble")
	for i := range src {
		src[i] = 'Z'
	}
	check("after scribble") // any aliased escape-path string shows scribbled bytes here
}

// copyFingerprint collects the content of every retained string reachable
// from d, sorted. Each string is cloned, so the result does not depend on the
// source buffer. Compare it before and after a scribble. Any aliased field
// shows scribbled bytes in the second call.
func copyFingerprint(d *CopyDoc) []string {
	out := collectDocStrings(d, nil)
	sort.Strings(out)
	return out
}

func collectDocStrings(d *CopyDoc, out []string) []string {
	out = append(out, "name:"+strings.Clone(d.Name))
	for _, tag := range d.Tags {
		out = append(out, "tag:"+strings.Clone(tag))
	}
	for k, v := range d.Props {
		out = append(out, "prop:"+strings.Clone(k)+"="+strings.Clone(v))
	}
	out = append(out, "raw:"+string(append([]byte(nil), d.Raw...)))
	out = collectAnyStrings(d.Extra, out)
	for _, r := range d.Refs {
		if r != nil {
			out = append(out, "ref:"+strings.Clone(r.Label))
		}
	}
	for i := range d.Children {
		out = collectDocStrings(&d.Children[i], out)
	}
	return out
}

// collectAnyStrings walks a decoded `any` tree and appends the content of
// every string and object key. The tree holds the shapes ggen.AnyCopy
// returns: string, []any, map[string]any, number, bool and nil.
func collectAnyStrings(v any, out []string) []string {
	switch t := v.(type) {
	case string:
		out = append(out, "any:"+strings.Clone(t))
	case []any:
		for _, e := range t {
			out = collectAnyStrings(e, out)
		}
	case map[string]any:
		for k, e := range t {
			out = append(out, "anykey:"+strings.Clone(k))
			out = collectAnyStrings(e, out)
		}
	}
	return out
}

//ggen:generate copy
type CopyURLDoc struct {
	U url.URL `json:"u"`
}

// url.Parse slices Scheme/Host/Path/RawQuery/Fragment out of its argument, so
// an aliasing scan under -copy would leave every one of them pointing at the
// caller's buffer.
func TestCopy_URLDecouples(t *testing.T) {
	t.Parallel()
	src := []byte(`{"u":"https://example.com/pathpart?q=1234#frag"}`)
	d, _, err := CopyURLDoc{}.DecodeFrom(src)
	if err != nil {
		t.Fatalf("CopyURLDoc decode: %v", err)
	}
	want := d.U.String()
	for i := range src {
		src[i] = 'Z'
	}
	if got := d.U.String(); got != want {
		t.Errorf("after scribble: %q, want %q", got, want)
	}
	if d.U.Host != "example.com" || d.U.Path != "/pathpart" || d.U.RawQuery != "q=1234" || d.U.Fragment != "frag" {
		t.Errorf("after scribble: %+v", d.U)
	}
}

//ggen:generate copy
type CopyIPDoc struct {
	IP net.IP `json:"ip"`
}

// net.ParseIP copies into a fresh IP, so the value itself never aliases the
// input. But the parse error keeps the scanned text, and under -copy that
// text must not alias the input either. The stream path clones it as well.
func TestCopy_NetIPErrorTextDecouples(t *testing.T) {
	t.Parallel()
	src := []byte(`{"ip":"not-an-ip-at-all"}`)
	_, _, err := CopyIPDoc{}.DecodeFrom(src)
	var pe *net.ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("got %T %v, want *net.ParseError", err, err)
	}
	// Compare against a literal, never against a string read out of pe before
	// the scribble. That string shares the same bytes, so both sides would
	// change.
	const want = "not-an-ip-at-all"
	if pe.Text != want {
		t.Fatalf("Text = %q, want %q", pe.Text, want)
	}
	for i := range src {
		src[i] = 'Z'
	}
	if pe.Text != want {
		t.Errorf("after scribble: Text = %q, want %q", pe.Text, want)
	}
}
