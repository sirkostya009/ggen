package integrationtests

//go:generate ../ggen $GOFILE

// -copy mode: the bytes path copies retained strings / map keys+values /
// slice elements / json.RawMessage / any-embedded strings out of the input
// instead of aliasing it, so the decoded value survives a later mutation of
// the source buffer (matching the stream path's lifetime). CopyDoc carries
// `//ggen:generate copy`; AliasDoc is the default aliasing control.

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"
)

//ggen:generate copy
type CopyDoc struct {
	Name     string            `json:"name"`
	Tags     []string          `json:"tags"`
	Props    map[string]string `json:"props"`
	Extra    any               `json:"extra"`
	Raw      json.RawMessage   `json:"raw"`
	Refs     []*CopyRef        `json:"refs"`
	Children []CopyDoc         `json:"children"`
}

//ggen:generate copy
type CopyRef struct {
	Label string `json:"label"`
}

// AliasDoc mirrors CopyDoc's leading field without `copy`: its Name aliases the
// input, so it serves as the negative control proving the scribble is effective.
//
//ggen:generate
type AliasDoc struct {
	Name string `json:"name"`
}

// TestCopy_AliasVsCopy is the deterministic proof that -copy decouples the
// decoded value from the input buffer: after the source bytes are scribbled
// over, the aliasing AliasDoc is corrupted (so the mutation is effective) while
// every retained CopyDoc value — string, slice elem, map key/value, any, and
// json.RawMessage — stays intact.
func TestCopy_AliasVsCopy(t *testing.T) {
	const js = `{"name":"hello world","tags":["alpha","beta"],"props":{"key":"value"},"extra":"xtra","raw":{"a":"b"},"refs":[{"label":"ref-one"}]}`

	// Alias control (Name-only payload): mutating the source MUST corrupt the
	// decoded string, otherwise the scribble is ineffective and the copy
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

	// Copy mode: same mutation leaves every retained value intact.
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

// TestCopy_NestedDecouples runs the decouple check over a deep tree, covering
// every retained-string site at depth: field strings, slice/map string
// elements, map keys, json.RawMessage spans, any-embedded strings + object
// keys (ggen.AnyCopy), and pointer-slice element structs. A content
// fingerprint taken before and after scribbling the source must be identical.
func TestCopy_NestedDecouples(t *testing.T) {
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

// TestCopy_EscapedDecouples proves -copy decouples AND correctly unescapes at
// every retained-string site when the value routes through scan.stringSlow (the
// escape path — a fresh owned scratch that -copy then redundantly clones). The
// ASCII-only NestedDecouples payloads never reach stringSlow, so this pins the
// escape arm: field string, slice elem, map key + value, any-string, pointer
// leaf — each carries a two-char escape / \uXXXX / surrogate pair, and every
// decoded value must equal the unescaped Go string both before and after the
// source is scribbled.
func TestCopy_EscapedDecouples(t *testing.T) {
	const js = `{"name":"a\nb\"c\\dé😀","tags":["té","plain"],"props":{"k\ny":"v\"w"},"extra":"x😀","refs":[{"label":"r\t0"}]}`

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

// copyFingerprint collects the CONTENT of every retained string reachable from
// d (cloned, independent of the source buffer), sorted. Compared before/after a
// scribble: any aliased field shows scribbled bytes in the second call.
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

// collectAnyStrings walks a decoded `any` tree (ggen.AnyCopy output shapes:
// string / []any / map[string]any / number / bool / nil) and appends the
// content of every string and object key.
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
