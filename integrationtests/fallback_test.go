package integrationtests

//go:generate ../ggen $GOFILE

import (
	"bytes"
	"database/sql"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sirkostya009/ggen"
	"github.com/sirkostya009/ggen/integrationtests/thirdparty"
	"github.com/sirkostya009/ggen/integrationtests/thirdparty2"
)

// FallbackStruct holds thirdparty.External, which has neither DecodeFrom nor
// UnmarshalText. So codegen emits an encoding/json.Unmarshal fallback.
//
//ggen:generate
type FallbackStruct struct {
	ID    string              `json:"id"`
	Extra thirdparty.External `json:"extra"`
}

// FastFallbackStruct holds thirdparty2.External2, which ggen generated in its
// own package. So codegen emits direct AppendJSON and DecodeFrom calls, with
// no encoding/json path.
//
//ggen:generate
type FastFallbackStruct struct {
	ID    string                `json:"id"`
	Extra thirdparty2.External2 `json:"extra"`
}

// TextFallbackStruct holds thirdparty.Tagged, which implements TextMarshaler
// and TextUnmarshaler. Codegen routes through those, and the wire form is
// "name#tag".
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
	out, _ := ggen.MarshalString(in)
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

// Cross-package dispatch to External2's generated methods round-trips.
func TestFastFallback_roundtrip(t *testing.T) {
	t.Parallel()
	in := FastFallbackStruct{
		ID:    "abc",
		Extra: thirdparty2.External2{Key: "k", Value: 42},
	}
	out, _ := ggen.MarshalString(in)
	got, _, err := FastFallbackStruct{}.DecodeFrom([]byte(out))
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if got.Extra.Key != "k" || got.Extra.Value != 42 {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

// A validation error on the nested field proves the generated decoder ran.
// The json fallback would not validate.
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

// Tagged routes through TextMarshaler and TextUnmarshaler. The "name#tag"
// string form, not a sub-object, confirms the text path ran.
func TestTextFallback_roundtrip(t *testing.T) {
	t.Parallel()
	in := TextFallbackStruct{ID: "x", Tag: thirdparty.Tagged{Name: "alice", Tag: "admin"}}
	out, _ := ggen.MarshalString(in)
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
// A value field never spells out the foreign type, but container and pointer
// positions do, so the generated file must import the foreign package for
// them. On the marshal side, the AppendJSON signature probe must match the
// emitted `func([]byte) ([]byte, error)`. Otherwise every cross-package value
// falls through to encoding/json.

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
	t.Parallel()
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

// The foreign type's methods must be reached through a pointer too. The
// method set of `*T` contains the methods of T, so the probe must look at the
// pointee.
func TestCrossPkg_PointerUsesGeneratedMethods(t *testing.T) {
	t.Parallel()
	v, _, err := CrossPkgShapes{}.DecodeFrom([]byte(`{"ptr":{"key":"","value":1}}`))
	if err == nil {
		t.Fatalf("expected the foreign type's own `required minlen=1` rule to fire, got %+v", v)
	}
}

// TextAppenderStruct checks that the cross-package TextAppender marshal path
// escapes AppendText output. Written raw between quotes, a text carrying `"`
// or `\` would give invalid JSON with a nil error. thirdparty.QuotedText's
// text is caller-controlled bytes.
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
	out, err := ggen.Marshal(got)
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
	out, _ = ggen.Marshal(clean)
	if string(out) != `{"t":"plain"}` {
		t.Errorf("clean = %s", out)
	}
}

// R10WrapHost reaches thirdparty2.Wrap through the UnmarshalJSON rung. The
// wrapper delegates to its package's generated decoder, so a positional
// error comes back relative to the span the rung handed it and has to be
// rebased onto the payload like every other nested-decode site.
//
//ggen:generate
type R10WrapHost struct {
	Pad string           `json:"pad"`
	W   thirdparty2.Wrap `json:"w"`
}

func TestCrossPkg_unmarshalJSONRungRebasesPos(t *testing.T) {
	t.Parallel()
	payload := `{"pad":"` + strings.Repeat("p", 40) + `","w":{"key":"abc","value":"q"}}`
	want := strings.Index(payload, `"q"`)
	check := func(path string, err error) {
		t.Helper()
		pe, ok := errors.AsType[*ggen.ParseError](err)
		if !ok {
			t.Fatalf("%s: got %T %v", path, err, err)
		}
		if pe.Pos != want || strings.Join(pe.Keys, ".") != "w.value" {
			t.Errorf("%s: Pos=%d Path=%v, want Pos=%d Path=w.value (%v)", path, pe.Pos, pe.Keys, want, err)
		}
	}
	_, _, berr := R10WrapHost{}.DecodeFrom([]byte(payload))
	check("bytes", berr)
	var s ggen.Stream
	s.Reset(&chunkReader{data: []byte(payload), max: 3}, make([]byte, 0, 16))
	_, serr := R10WrapHost{}.DecodeFromStream(&s)
	check("stream", serr)
}

// OpaqueBoxHost reaches thirdparty2.External2's generated decoder only
// through encoding/json (Box) or a converter (Doc), so the error that decoder
// returns cannot be placed in this payload. It stays the cause of a
// ParseError at the host's field.
//
//ggen:generate
type OpaqueBoxHost struct {
	Pad string          `json:"pad"`
	B   thirdparty2.Box `json:"b"`
	Doc int             `json:"doc" pipe:". / @ValueFromDoc"`
}

func ValueFromDoc(s string) (int, error) {
	v, _, err := thirdparty2.External2{}.DecodeFrom([]byte(s))
	return v.Value, err
}

func TestOpaqueCallee_errorWrappedAtField(t *testing.T) {
	t.Parallel()
	pad := strings.Repeat("p", 40)
	for _, tc := range []struct{ name, val, key string }{
		{"json parse", `"b":{"Label":"l","In":{"key":"k","value":"q"}}`, "b"},
		{"json validation", `"b":{"Label":"l","In":{"key":"k","value":-1}}`, "b"},
		{"converter parse", `"doc":"{\"key\":\"k\",\"value\":\"q\"}"`, "doc"},
	} {
		payload := `{"pad":"` + pad + `",` + tc.val + `}`
		start, end := len(pad)+10, len(payload)-1
		_, _, berr := OpaqueBoxHost{}.DecodeFrom([]byte(payload))
		var s ggen.Stream
		s.Reset(&chunkReader{data: []byte(payload), max: 3}, make([]byte, 0, 16))
		_, serr := OpaqueBoxHost{}.DecodeFromStream(&s)
		for i, err := range []error{berr, serr} {
			pe, ok := err.(*ggen.ParseError)
			if !ok {
				t.Errorf("%s path %d: want *ggen.ParseError, got %T %v", tc.name, i, err, err)
				continue
			}
			if !slices.Equal(pe.Keys, []string{tc.key}) || pe.Pos < start || pe.Pos > end {
				t.Errorf("%s path %d: Keys=%q Pos=%d, want [%s] and Pos in [%d,%d] (%v)", tc.name, i, pe.Keys, pe.Pos, tc.key, start, end, err)
			}
			if pe.Err == nil {
				t.Errorf("%s path %d: callee error lost", tc.name, i)
			}
		}
		if berr.Error() != serr.Error() {
			t.Errorf("%s: bytes %q, stream %q", tc.name, berr, serr)
		}
	}
}

// R10TextCodec and R10JSONCodec are same-package unannotated structs with
// exported fields and their own text or JSON marshaler pair. They are never
// generated as a dependency. The host calls their methods, exactly as it
// would for the same type in another package. This matches jsonv2.
type R10TextCodec struct{ V string }

func (c R10TextCodec) MarshalText() ([]byte, error) { return []byte("T:" + c.V), nil }
func (c *R10TextCodec) UnmarshalText(b []byte) error {
	c.V = strings.TrimPrefix(string(b), "T:")
	return nil
}

type R10JSONCodec struct{ V int }

func (c R10JSONCodec) MarshalJSON() ([]byte, error) { return []byte(`"J"`), nil }
func (c *R10JSONCodec) UnmarshalJSON(b []byte) error {
	if string(b) == `"J"` {
		c.V = 42
	}
	return nil
}

//ggen:generate nosortkeys
type R10SamePkgCodecHost struct {
	T R10TextCodec `json:"t"`
	J R10JSONCodec `json:"j"`
}

func TestSamePkgCodec_MethodsHonoured(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"DecodeFrom", "AppendJSON", "JSONSize"} {
		if _, ok := reflect.TypeFor[R10TextCodec]().MethodByName(name); ok {
			t.Errorf("R10TextCodec got a generated %s despite owning a text codec", name)
		}
		if _, ok := reflect.TypeFor[R10JSONCodec]().MethodByName(name); ok {
			t.Errorf("R10JSONCodec got a generated %s despite owning a JSON codec", name)
		}
	}
	v := R10SamePkgCodecHost{T: R10TextCodec{"x"}, J: R10JSONCodec{1}}
	want, err := jsonv2.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.AppendJSON(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("marshal:\n ggen   %s\n jsonv2 %s", got, want)
	}
	var w2 R10SamePkgCodecHost
	if err := jsonv2.Unmarshal(want, &w2); err != nil {
		t.Fatal(err)
	}
	g2, _, err := R10SamePkgCodecHost{}.DecodeFrom(want)
	if err != nil {
		t.Fatalf("decode of the jsonv2 wire: %v", err)
	}
	if g2 != w2 {
		t.Errorf("decode: ggen %+v jsonv2 %+v", g2, w2)
	}
	s2, err := R10SamePkgCodecHost{}.DecodeFromStream(ggen.NewStream(strings.NewReader(string(want)), nil))
	if err != nil {
		t.Fatalf("stream decode: %v", err)
	}
	if s2 != w2 {
		t.Errorf("stream decode: ggen %+v jsonv2 %+v", s2, w2)
	}
}

// CrossPkgDeep holds a cross-package generated struct two container levels
// down, where it must still use its own methods: a Lease writes its TTL as a
// duration string only through them.
//
//ggen:generate
type CrossPkgDeep struct {
	Rows  [][]thirdparty2.Lease          `json:"rows"`
	Arr   [2][]thirdparty2.Lease         `json:"arr"`
	Dict  map[string][]thirdparty2.Lease `json:"dict"`
	Ptrs  [][]*thirdparty2.Lease         `json:"ptrs"`
	Maps  []map[string]thirdparty2.Lease `json:"maps"`
	Extra map[string][]thirdparty2.Lease `json:",embed"`
}

func TestCrossPkg_belowFirstContainerLevel(t *testing.T) {
	t.Parallel()
	l := thirdparty2.Lease{TTL: 90 * time.Second}
	v := CrossPkgDeep{
		Rows:  [][]thirdparty2.Lease{{l}},
		Arr:   [2][]thirdparty2.Lease{{l}, {}},
		Dict:  map[string][]thirdparty2.Lease{"a": {l}},
		Ptrs:  [][]*thirdparty2.Lease{{&l, nil}},
		Maps:  []map[string]thirdparty2.Lease{{"k": l}},
		Extra: map[string][]thirdparty2.Lease{"x": {l}},
	}
	const want = `{"arr":[[{"ttl":"1m30s"}],[]],"dict":{"a":[{"ttl":"1m30s"}]},"maps":[{"k":{"ttl":"1m30s"}}],` +
		`"ptrs":[[{"ttl":"1m30s"},null]],"rows":[[{"ttl":"1m30s"}]],"x":[{"ttl":"1m30s"}]}`
	out, err := ggen.Marshal(v)
	if err != nil || string(out) != want {
		t.Fatalf("marshal: %s %v\nwant %s", out, err, want)
	}
	b, s, err := remarshalBoth[CrossPkgDeep](out)
	if err != nil || string(b) != want || string(s) != want {
		t.Errorf("round trip: bytes %s, stream %s, %v", b, s, err)
	}
}

// CrossPkgPtrDeep holds the same struct below a pointer to a container, in
// each place such a pointer can sit.
//
//ggen:generate
type CrossPkgPtrDeep struct {
	MapSlice map[string]*[]thirdparty2.Lease          `json:"ms"`
	MapMap   map[string]*map[string]thirdparty2.Lease `json:"mm"`
	MapArr   map[string]*[2]thirdparty2.Lease         `json:"ma"`
	MapPP    map[string]**[]thirdparty2.Lease         `json:"mpp"`
	MapPtrs  map[string]*[]*thirdparty2.Lease         `json:"mps"`
	Chain    []**[]thirdparty2.Lease                  `json:"chain"`
	Extra    map[string]*[]thirdparty2.Lease          `json:",embed"`
}

func TestCrossPkg_belowPointerToContainer(t *testing.T) {
	t.Parallel()
	l := thirdparty2.Lease{TTL: 90 * time.Second}
	row := []thirdparty2.Lease{l}
	rowp := &row
	v := CrossPkgPtrDeep{
		MapSlice: map[string]*[]thirdparty2.Lease{"a": &row},
		MapMap:   map[string]*map[string]thirdparty2.Lease{"a": {"k": l}},
		MapArr:   map[string]*[2]thirdparty2.Lease{"a": {l, l}},
		MapPP:    map[string]**[]thirdparty2.Lease{"a": &rowp},
		MapPtrs:  map[string]*[]*thirdparty2.Lease{"a": {&l}},
		Chain:    []**[]thirdparty2.Lease{&rowp},
		Extra:    map[string]*[]thirdparty2.Lease{"x": &row},
	}
	const d = `{"ttl":"1m30s"}`
	const want = `{"chain":[[` + d + `]],"ma":{"a":[` + d + `,` + d + `]},"mm":{"a":{"k":` + d + `}},` +
		`"mpp":{"a":[` + d + `]},"mps":{"a":[` + d + `]},"ms":{"a":[` + d + `]},"x":[` + d + `]}`
	out, err := ggen.Marshal(v)
	if err != nil || string(out) != want {
		t.Fatalf("marshal: %s %v\nwant %s", out, err, want)
	}
	b, s, err := remarshalBoth[CrossPkgPtrDeep](out)
	if err != nil || string(b) != want || string(s) != want {
		t.Errorf("round trip: bytes %s, stream %s, %v", b, s, err)
	}
}

// LeaseGrid is a container alias two levels over a cross-package generated
// struct, and LeaseNulls holds one inside a sql.Null[T]. Both keep the
// struct's own methods, so the TTL is a duration string.
//
//ggen:generate
type LeaseGrid [][]thirdparty2.Grant

//ggen:generate
type LeaseNulls struct {
	N sql.Null[[][]thirdparty2.Quota] `json:"n"`
}

func TestCrossPkg_belowContainerAliasAndSQLNull(t *testing.T) {
	t.Parallel()
	const grid = `[[{"ttl":"1m30s"}],[]]`
	out, err := ggen.Marshal(LeaseGrid{{{TTL: 90 * time.Second}}, {}})
	if err != nil || string(out) != grid {
		t.Errorf("alias marshal: %s %v, want %s", out, err, grid)
	}
	if b, s, err := remarshalBoth[LeaseGrid]([]byte(grid)); err != nil || string(b) != grid || string(s) != grid {
		t.Errorf("alias round trip: bytes %s, stream %s, %v", b, s, err)
	}
	const nulls = `{"n":[[{"ttl":"1m30s"}]]}`
	out, err = ggen.Marshal(LeaseNulls{N: sql.Null[[][]thirdparty2.Quota]{V: [][]thirdparty2.Quota{{{TTL: 90 * time.Second}}}, Valid: true}})
	if err != nil || string(out) != nulls {
		t.Errorf("sql.Null marshal: %s %v, want %s", out, err, nulls)
	}
	if b, s, err := remarshalBoth[LeaseNulls]([]byte(nulls)); err != nil || string(b) != nulls || string(s) != nulls {
		t.Errorf("sql.Null round trip: bytes %s, stream %s, %v", b, s, err)
	}
}

// PromotedHolder holds a foreign struct whose only ggen methods are promoted
// from an embedded generated struct. Those methods would drop Outer2.Extra,
// so the field takes the encoding/json fallback.
//
//ggen:generate
type PromotedHolder struct {
	O  thirdparty2.Outer2            `json:"o"`
	OS []thirdparty2.Outer2          `json:"os"`
	OM map[string]thirdparty2.Outer2 `json:"om"`
}

func TestFallback_promotedMethodsIgnored(t *testing.T) {
	t.Parallel()
	o := func(n int) thirdparty2.Outer2 {
		return thirdparty2.Outer2{ID: n, Extra: n + 1}
	}
	in := PromotedHolder{O: o(1), OS: []thirdparty2.Outer2{o(3)}, OM: map[string]thirdparty2.Outer2{"k": o(5)}}
	got, err := ggen.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	want, err := jsonv2.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var gotV, wantV any
	if err := jsonv2.Unmarshal(got, &gotV); err != nil {
		t.Fatalf("%v: %s", err, got)
	}
	if err := jsonv2.Unmarshal(want, &wantV); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotV, wantV) {
		t.Fatalf("marshal:\n got %s\nwant %s", got, want)
	}
	back, _, err := PromotedHolder{}.DecodeFrom(got)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, in) {
		t.Fatalf("round trip: got %+v, want %+v", back, in)
	}
	if n := in.JSONSize(); n < len(got) {
		t.Fatalf("JSONSize %d < %d written", n, len(got))
	}
}

// PromotedTextCodec and PromotedJSONCodec have no methods of their own, but
// embed a struct with a codec, so they get its methods by promotion and,
// like in encoding/json, take the codec ladder instead of being generated.
type PromotedTextCodec struct {
	R10TextCodec

	B int
}

type PromotedJSONCodec struct {
	R10JSONCodec

	B int
}

//ggen:generate nosortkeys
type PromotedCodecHost struct {
	T  PromotedTextCodec   `json:"t"`
	J  PromotedJSONCodec   `json:"j"`
	TS []PromotedTextCodec `json:"ts"`
}

func TestSamePkgCodec_PromotedMethodsHonoured(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"DecodeFrom", "AppendJSON", "JSONSize"} {
		if _, ok := reflect.TypeFor[PromotedTextCodec]().MethodByName(name); ok {
			t.Errorf("PromotedTextCodec got a generated %s despite a promoted text codec", name)
		}
	}
	v := PromotedCodecHost{
		T:  PromotedTextCodec{R10TextCodec{"x"}, 1},
		J:  PromotedJSONCodec{R10JSONCodec{1}, 2},
		TS: []PromotedTextCodec{{R10TextCodec{"y"}, 3}},
	}
	want, err := jsonv2.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.AppendJSON(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("marshal:\n ggen   %s\n jsonv2 %s", got, want)
	}
	in := []byte(`{"t":"T:a","j":"J","ts":["T:b"]}`)
	var w PromotedCodecHost
	if err := jsonv2.Unmarshal(in, &w); err != nil {
		t.Fatal(err)
	}
	g, _, err := PromotedCodecHost{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("decode: ggen %+v jsonv2 %+v", g, w)
	}
	s, err := PromotedCodecHost{}.DecodeFromStream(ggen.NewStream(strings.NewReader(string(in)), nil))
	if err != nil {
		t.Fatalf("stream decode: %v", err)
	}
	if !reflect.DeepEqual(s, w) {
		t.Errorf("stream decode: ggen %+v jsonv2 %+v", s, w)
	}
}

// FallbackUTF8Host reaches the encoding/json rung at every position. The
// catch-all's sql.NullString values take it too.
//
//ggen:generate
type FallbackUTF8Host struct {
	E thirdparty.External            `json:"e"`
	S []thirdparty.External          `json:"s"`
	M map[string]thirdparty.External `json:"m"`
	P *thirdparty.External           `json:"p"`
}

//ggen:generate
type FallbackUTF8Rest struct {
	ID   int                       `json:"id"`
	Rest map[string]sql.NullString `json:",embed"`
}

//ggen:generate allowinvalidutf8
type PermissiveFallbackHost struct {
	E    thirdparty.External       `json:"e"`
	Rest map[string]sql.NullString `json:",embed"`
}

// TestFallbackRejectsInvalidUTF8 checks that the encoding/json rung rejects
// invalid UTF-8 bytes with ggen.ErrInvalidUTF8 on both paths, where
// encoding/json alone would rewrite them to U+FFFD, and that allowinvalidutf8
// keeps accepting them.
func TestFallbackRejectsInvalidUTF8(t *testing.T) {
	t.Parallel()
	decodeBoth := func(t *testing.T, payload string, bytesDec func([]byte) error, streamDec func(*ggen.Stream) error) (error, error) {
		t.Helper()
		bErr := bytesDec([]byte(payload))
		var s ggen.Stream
		s.Reset(&chunkReader{data: []byte(payload), max: 3}, make([]byte, 0, 8))
		return bErr, streamDec(&s)
	}
	host := func(d []byte) error { _, _, err := FallbackUTF8Host{}.DecodeFrom(d); return err }
	hostS := func(s *ggen.Stream) error { _, err := FallbackUTF8Host{}.DecodeFromStream(s); return err }
	rest := func(d []byte) error { _, _, err := FallbackUTF8Rest{}.DecodeFrom(d); return err }
	restS := func(s *ggen.Stream) error { _, err := FallbackUTF8Rest{}.DecodeFromStream(s); return err }
	for _, c := range []struct {
		name    string
		payload string
		bytes   func([]byte) error
		stream  func(*ggen.Stream) error
	}{
		{"field", "{\"e\":{\"key\":\"\xff\",\"value\":1}}", host, hostS},
		{"slice_elem", "{\"s\":[{\"key\":\"a\xc0\",\"value\":1}]}", host, hostS},
		{"map_value", "{\"m\":{\"k\":{\"key\":\"\xff\",\"value\":1}}}", host, hostS},
		{"pointer", "{\"p\":{\"key\":\"\xff\",\"value\":1}}", host, hostS},
		{"catch_all", "{\"x\":{\"String\":\"a\xffb\",\"Valid\":true}}", rest, restS},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			bErr, sErr := decodeBoth(t, c.payload, c.bytes, c.stream)
			if !errors.Is(bErr, ggen.ErrInvalidUTF8) {
				t.Errorf("bytes: err=%v, want ErrInvalidUTF8", bErr)
			}
			if !errors.Is(sErr, ggen.ErrInvalidUTF8) {
				t.Errorf("stream: err=%v, want ErrInvalidUTF8", sErr)
			}
			var bp, sp *ggen.ParseError
			if errors.As(bErr, &bp) && errors.As(sErr, &sp) && bp.Pos != sp.Pos {
				t.Errorf("position: bytes=%d stream=%d", bp.Pos, sp.Pos)
			}
		})
	}
	t.Run("clean", func(t *testing.T) {
		t.Parallel()
		bErr, sErr := decodeBoth(t, `{"e":{"key":"żółć","value":1},"s":[{"key":"a"}],"p":{"key":"b"}}`, host, hostS)
		if bErr != nil || sErr != nil {
			t.Fatalf("bytes=%v stream=%v", bErr, sErr)
		}
	})
	t.Run("allowinvalidutf8", func(t *testing.T) {
		t.Parallel()
		payload := "{\"e\":{\"key\":\"\xff\",\"value\":1},\"x\":{\"String\":\"a\xffb\",\"Valid\":true}}"
		bv, _, bErr := PermissiveFallbackHost{}.DecodeFrom([]byte(payload))
		var s ggen.Stream
		s.Reset(&chunkReader{data: []byte(payload), max: 3}, make([]byte, 0, 8))
		sv, sErr := PermissiveFallbackHost{}.DecodeFromStream(&s)
		if bErr != nil || sErr != nil {
			t.Fatalf("bytes=%v stream=%v", bErr, sErr)
		}
		if bv.E.Value != 1 || sv.E.Value != 1 || !bv.Rest["x"].Valid || !sv.Rest["x"].Valid {
			t.Errorf("bytes=%+v stream=%+v", bv, sv)
		}
	})
}
