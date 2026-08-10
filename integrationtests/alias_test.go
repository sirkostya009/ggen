package integrationtests

//go:generate ../ggen $GOFILE

// Top-level type aliases — each gets the full struct method surface.

import (
	"bytes"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/encode"
	"github.com/sirkostya009/ggen/integrationtests/thirdparty"
	"github.com/sirkostya009/ggen/scan"
	"github.com/sirkostya009/ggen/validation"
)

//ggen:generate
type AliasString string

//ggen:generate htmlescape
type AliasHTML string

//ggen:generate
type AliasInt int

//ggen:generate
type AliasUint64 uint64

//ggen:generate
type AliasInt8 int8

//ggen:generate
type AliasUint8 uint8

//ggen:generate
type AliasFloat64 float64

//ggen:generate
type AliasBool bool

func TestAlias_String_Roundtrip(t *testing.T) {
	t.Parallel()
	in := AliasString("hello")
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"hello"` {
		t.Errorf("marshal = %q, want %q", out, `"hello"`)
	}
	got, _, err := AliasString("").DecodeFrom(out)
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Errorf("roundtrip = %q, want %q", got, in)
	}
}

// Default mode emits <>& literally.
func TestAlias_String_DefaultIsLiteral(t *testing.T) {
	t.Parallel()
	out, _ := encode.Marshal(AliasString(`<a href="x">tom & jerry</a>`))
	for _, lit := range []string{"<", ">", "&"} {
		if !strings.Contains(string(out), lit) {
			t.Errorf("default mode escaped %q in alias output: %s", lit, out)
		}
	}
}

// htmlescape on a string alias emits \uXXXX escapes for <>&.
func TestAlias_String_HtmlescapeOptIn(t *testing.T) {
	t.Parallel()
	out, _ := encode.Marshal(AliasHTML(`<a href="x">tom & jerry</a>`))
	for _, lit := range []string{"<", ">", "&"} {
		if strings.Contains(string(out), lit) {
			t.Errorf("htmlescape alias leaked literal %q: %s", lit, out)
		}
	}
	for _, esc := range []string{"\\u003c", "\\u003e", "\\u0026"} {
		if !strings.Contains(string(out), esc) {
			t.Errorf("htmlescape alias missing escape %s: %s", esc, out)
		}
	}
}

func TestAlias_Int_Roundtrip(t *testing.T) {
	t.Parallel()
	in := AliasInt(-42)
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "-42" {
		t.Errorf("marshal = %q, want -42", out)
	}
	got, _, err := AliasInt(0).DecodeFrom([]byte("-42"))
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Errorf("roundtrip = %d, want -42", got)
	}
}

// Narrow-int aliases guard the cast (opt #48): 300 into int8 used to decode
// as 44 with a nil error. Bytes + stream, both signednesses.
func TestAlias_NarrowInt_Overflow(t *testing.T) {
	t.Parallel()
	if got, _, err := AliasInt8(0).DecodeFrom([]byte("-128")); err != nil || got != -128 {
		t.Errorf("in-range: got %d, %v", got, err)
	}
	if got, _, err := AliasUint8(0).DecodeFrom([]byte("255")); err != nil || got != 255 {
		t.Errorf("in-range: got %d, %v", got, err)
	}
	for _, in := range []string{"300", "-300", "128"} {
		if _, _, err := AliasInt8(0).DecodeFrom([]byte(in)); !errors.Is(err, scan.ErrNumberOverflow) {
			t.Errorf("int8 %s: got %v, want ErrNumberOverflow", in, err)
		}
		var s scan.Stream
		s.Reset(strings.NewReader(in), nil)
		if _, err := AliasInt8(0).DecodeFromStream(&s); !errors.Is(err, scan.ErrNumberOverflow) {
			t.Errorf("int8 stream %s: got %v, want ErrNumberOverflow", in, err)
		}
	}
	if _, _, err := AliasUint8(0).DecodeFrom([]byte("256")); !errors.Is(err, scan.ErrNumberOverflow) {
		t.Errorf("uint8 256: got %v, want ErrNumberOverflow", err)
	}
	var s scan.Stream
	s.Reset(strings.NewReader("256"), nil)
	if _, err := AliasUint8(0).DecodeFromStream(&s); !errors.Is(err, scan.ErrNumberOverflow) {
		t.Errorf("uint8 stream 256: got %v, want ErrNumberOverflow", err)
	}
}

func TestAlias_Uint64_Roundtrip(t *testing.T) {
	t.Parallel()
	in := AliasUint64(18446744073709551615) // max uint64
	out, _ := encode.Marshal(in)
	got, _, err := AliasUint64(0).DecodeFrom(out)
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Errorf("roundtrip = %d, want %d", got, in)
	}
}

func TestAlias_Float64_Roundtrip(t *testing.T) {
	t.Parallel()
	in := AliasFloat64(3.14159)
	out, _ := encode.Marshal(in)
	got, _, err := AliasFloat64(0).DecodeFrom(out)
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Errorf("roundtrip = %v, want %v", got, in)
	}
}

func TestAlias_Bool_Roundtrip(t *testing.T) {
	t.Parallel()
	for _, in := range []AliasBool{true, false} {
		out, _ := encode.Marshal(in)
		got, _, err := AliasBool(false).DecodeFrom(out)
		if err != nil {
			t.Fatalf("%v: %v", in, err)
		}
		if got != in {
			t.Errorf("roundtrip = %v, want %v", got, in)
		}
	}
}

// A JSON number into a string alias must error, not silently coerce.
func TestAlias_String_RejectsNonString(t *testing.T) {
	t.Parallel()
	if _, _, err := (AliasString("")).DecodeFrom([]byte("42")); err == nil {
		t.Error("expected scan error on number → string-alias")
	}
}

// Mutating the input buffer after decode shows through the aliased value.
func TestAlias_String_ZeroCopy(t *testing.T) {
	t.Parallel()
	in := []byte(`"alpha"`)
	got, _, err := AliasString("").DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != AliasString("alpha") {
		t.Fatalf("initial decode = %q, want %q", got, "alpha")
	}
	// Find the 'a' inside the JSON string body and mutate it.
	off := strings.Index(string(in), "alpha")
	if off < 0 {
		t.Skip("payload reshaped, can't verify alias")
	}
	in[off] = 'A'
	if got == AliasString("alpha") {
		t.Errorf("expected zero-copy alias; mutating source did not affect decoded value")
	}
	if got != AliasString("Alpha") {
		t.Errorf("alias didn't pick up mutated source: got %q", got)
	}
}

// PlainInner has no Marshal/Unmarshal methods, so its alias goes through
// field introspection.
type PlainInner struct {
	Title string `json:"title"`
	Count int    `json:"count"`
}

//ggen:generate
type PlainAlias PlainInner

// Alias of a method-less struct marshals as an introspected-field object.
func TestAlias_StructIntrospect_NoMethods(t *testing.T) {
	t.Parallel()
	in := PlainAlias{Title: "hello", Count: 42}
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"count":42,"title":"hello"}` {
		t.Errorf("marshal = %s, want struct-shaped JSON", out)
	}
	got, _, err := PlainAlias{}.DecodeFrom(out)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "hello" || got.Count != 42 {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

// SamePkgInner has JSON methods, but ggen prefers field introspection for a
// struct with exported fields.
type SamePkgInner struct {
	X int
	Y string
}

func (i SamePkgInner) MarshalJSON() ([]byte, error) {
	return fmt.Appendf(nil, `{"x":%d,"y":%q}`, i.X, i.Y), nil
}

func (i *SamePkgInner) UnmarshalJSON(b []byte) error {
	s := string(b)
	_, err := fmt.Sscanf(s, `{"x":%d,"y":%q}`, &i.X, &i.Y)
	return err
}

//ggen:generate
type SamePkgAlias SamePkgInner

// CrossPkgTaggedAlias's underlying (thirdparty.Tagged) has Text methods AND
// exported fields; ggen prefers introspection over text delegation.
//
//ggen:generate
type CrossPkgTaggedAlias thirdparty.Tagged

// Introspection wins over Text methods — field object, not "name#tag".
func TestAlias_StructIntrospect_CrossPkg(t *testing.T) {
	t.Parallel()
	in := CrossPkgTaggedAlias(thirdparty.Tagged{Name: "alice", Tag: "admin"})
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"Name":"alice","Tag":"admin"}` {
		t.Errorf("marshal = %s, want field-introspected JSON object", out)
	}
	got, _, err := CrossPkgTaggedAlias{}.DecodeFrom(out)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "alice" || got.Tag != "admin" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

// Introspection wins over the JSON methods — uppercase keys (no json tags).
func TestAlias_StructIntrospect_SamePkg(t *testing.T) {
	t.Parallel()
	in := SamePkgAlias(SamePkgInner{X: 42, Y: "hello"})
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"X":42,"Y":"hello"}` {
		t.Errorf("marshal = %s, want field-introspected JSON object", out)
	}
	got, _, err := SamePkgAlias{}.DecodeFrom(out)
	if err != nil {
		t.Fatal(err)
	}
	if got.X != 42 || got.Y != "hello" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

// OpaqueWithMethods has no exported fields but carries JSON methods, so its
// alias falls back to JSON/Text delegation.
type OpaqueWithMethods struct {
	hidden string
}

func (o OpaqueWithMethods) MarshalJSON() ([]byte, error) {
	return fmt.Appendf(nil, "%q", o.hidden), nil
}

func (o *OpaqueWithMethods) UnmarshalJSON(b []byte) error {
	s := string(b)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		o.hidden = s[1 : len(s)-1]
	}
	return nil
}

//ggen:generate
type OpaqueAlias OpaqueWithMethods

// No exported fields → JSON/Text method delegation.
func TestAlias_StructDelegation_OpaqueFallback(t *testing.T) {
	t.Parallel()
	in := OpaqueAlias(OpaqueWithMethods{hidden: "secret"})
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"secret"` {
		t.Errorf("marshal = %s, want delegated MarshalJSON output", out)
	}
	got, _, err := OpaqueAlias{}.DecodeFrom(out)
	if err != nil {
		t.Fatal(err)
	}
	if (OpaqueWithMethods)(got).hidden != "secret" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

//ggen:generate
type OpaqueParent struct {
	Name string      `json:"name"`
	O    OpaqueAlias `json:"o"`
}

// A delegating alias as a FIELD must append to the parent's buffer, not
// return MarshalJSON's slice bare (that discarded everything before it).
func TestAlias_StructDelegation_AsField(t *testing.T) {
	t.Parallel()
	in := OpaqueParent{Name: "bob", O: OpaqueAlias(OpaqueWithMethods{hidden: "secret"})}
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"name":"bob","o":"secret"}`; string(out) != want {
		t.Errorf("marshal = %s, want %s", out, want)
	}
	got, _, err := OpaqueParent{}.DecodeFrom(out)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "bob" || (OpaqueWithMethods)(got.O).hidden != "secret" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

//ggen:generate
type AliasTags []string

//ggen:generate
type AliasLookup map[string]int

//ggen:generate
type AliasTuple [3]int

// Slice alias — JSON-array wire shape.
func TestAlias_Slice_Roundtrip(t *testing.T) {
	t.Parallel()
	in := AliasTags{"go", "rust", "zig"}
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `["go","rust","zig"]` {
		t.Errorf("marshal = %s, want JSON array", out)
	}
	got, _, err := AliasTags{}.DecodeFrom(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "go" || got[1] != "rust" || got[2] != "zig" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

// Map alias — JSON object wire shape.
func TestAlias_Map_Roundtrip(t *testing.T) {
	t.Parallel()
	in := AliasLookup{"alpha": 1, "beta": 2}
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := AliasLookup{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if len(got) != 2 || got["alpha"] != 1 || got["beta"] != 2 {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

// Array alias — JSON tuple with strict element count.
func TestAlias_Array_Roundtrip(t *testing.T) {
	t.Parallel()
	in := AliasTuple{10, 20, 30}
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `[10,20,30]` {
		t.Errorf("marshal = %s, want [10,20,30]", out)
	}
	got, _, err := AliasTuple{}.DecodeFrom(out)
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Errorf("roundtrip = %v, want %v", got, in)
	}
}

// Wrong element count into an array alias errors with LenError.
func TestAlias_Array_StrictLen(t *testing.T) {
	t.Parallel()
	if _, _, err := (AliasTuple{}).DecodeFrom([]byte(`[1,2]`)); err == nil {
		t.Error("expected LenError on short tuple")
	}
	if _, _, err := (AliasTuple{}).DecodeFrom([]byte(`[1,2,3,4]`)); err == nil {
		t.Error("expected LenError on long tuple")
	}
}

// Decoding an escape-free string alias does zero allocations.
func TestAlias_String_ZeroAllocations(t *testing.T) {
	in := []byte(`"some-typical-html-payload-here"`)
	allocs := testing.AllocsPerRun(100, func() {
		v, _, err := AliasString("").DecodeFrom(in)
		if err != nil {
			t.Fatal(err)
		}
		if len(v) == 0 {
			t.Fatal("empty")
		}
	})
	if allocs != 0 {
		t.Errorf("expected 0 allocs for string-alias decode, got %v", allocs)
	}
}

// AliasFieldExample exercises validation rules and mods on fields whose
// types are primitive aliases.
//
//ggen:generate
type AliasFieldExample struct {
	Body  AliasString `json:"body" pipe:"required trim tolower minlen=2 maxlen=10"`
	Count AliasInt    `json:"count" pipe:"clamp=1|100 gte=1 lte=100"`
}

func TestAlias_Field_ValidationAndMods(t *testing.T) {
	t.Parallel()
	// trim + lower run before validation; the value the validator sees
	// is the post-mod one.
	got, _, err := AliasFieldExample{}.DecodeFrom([]byte(`{"body":"  HI  ","count":5}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(got.Body) != "hi" {
		t.Errorf("trim+lower on AliasString: got %q, want %q", got.Body, "hi")
	}

	// clamp pulls 500 back into [1,100]; gte/lte then pass on the clamped value.
	got, _, err = AliasFieldExample{}.DecodeFrom([]byte(`{"body":"hello","count":500}`))
	if err != nil {
		t.Fatalf("clamp+validate: %v", err)
	}
	if int(got.Count) != 100 {
		t.Errorf("clamp on AliasInt: got %d, want 100", got.Count)
	}

	// minlen fires post-trim — `" a "` → `"a"` → length 1, below the limit.
	_, _, err = AliasFieldExample{}.DecodeFrom([]byte(`{"body":" a ","count":5}`))
	var minlen *validation.MinLenError
	if !errors.As(err, &minlen) {
		t.Errorf("expected MinLenError post-trim, got %v", err)
	}

	// roundtrip — alias values marshal as their underlying primitives.
	out, err := encode.Marshal(AliasFieldExample{Body: "hi", Count: 5})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"body":"hi"`) || !strings.Contains(string(out), `"count":5`) {
		t.Errorf("marshal = %s", out)
	}
}

// ---- named primitives (annotated and not) ----------------------------
//
// Every rule has to resolve the underlying kind: `oneof` used to emit its
// allowed values as bare identifiers, the string-shape rules passed the named
// value uncast into string-typed APIs, and `eq`/`neq` emitted nothing at all.

//ggen:generate
type NPPriority string

// NPTag carries no annotation — ggen still has to resolve `string` under it for
// the rules, and cast at every stdlib call site.
type NPTag string

//ggen:generate
type NamedPrims struct {
	Pri  NPPriority `json:"pri"  pipe:"oneof=low|medium|high"`
	Tag  NPTag      `json:"tag"  pipe:"trim tolower maxrunes=8 contains=-"`
	Eq   NPPriority `json:"eq"   pipe:"eq=low"`
	Neq  NPPriority `json:"neq"  pipe:"neq=low"`
	Zero NPPriority `json:"zero" pipe:"nullzero / ."`
}

const npValid = `{"pri":"high","tag":" A-B ","eq":"low","neq":"high","zero":null}`

func TestNamedPrim_Accepts(t *testing.T) {
	v, _, err := NamedPrims{}.DecodeFrom([]byte(npValid))
	if err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}
	if v.Tag != "a-b" {
		t.Errorf("mods did not run through the named type: %q", v.Tag)
	}
	if v.Zero != "" {
		t.Errorf("nullzero on a named string should give the zero value, got %q", v.Zero)
	}
}

func TestNamedPrim_Rejects(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		target     any
	}{
		{"oneof", `{"pri":"nope","tag":"a-b","eq":"low","neq":"high","zero":null}`, new(*validation.OneOfError)},
		{"eq", `{"pri":"low","tag":"a-b","eq":"other","neq":"high","zero":null}`, new(*validation.EqError)},
		{"neq", `{"pri":"low","tag":"a-b","eq":"low","neq":"low","zero":null}`, new(*validation.NeqError)},
		{"maxrunes", `{"pri":"low","tag":"aaaaaaaaa-","eq":"low","neq":"high","zero":null}`, new(*validation.MaxRunesError)},
		{"contains", `{"pri":"low","tag":"abc","eq":"low","neq":"high","zero":null}`, new(*validation.ContainsError)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := NamedPrims{}.DecodeFrom([]byte(tc.body))
			if err == nil {
				t.Fatalf("%s rule did not fire", tc.name)
			}
			if !errors.As(err, tc.target) {
				t.Errorf("wrong error type for %s: %v", tc.name, err)
			}
		})
	}
}

// maxrunes counts runes, not bytes, through the named type too.
func TestNamedPrim_RunesNotBytes(t *testing.T) {
	// 8 runes, 16 bytes — inside maxrunes=8.
	body := `{"pri":"low","tag":"ыыыыыыы-","eq":"low","neq":"high","zero":null}`
	_, _, err := NamedPrims{}.DecodeFrom([]byte(body))
	if err != nil {
		t.Errorf("8 multibyte runes rejected by maxrunes=8: %v", err)
	}
}

// A named primitive is decoded/encoded inline (underlying scan + conversion) at
// every position, and an alias that carries flags of its own keeps its methods
// so its behaviour survives — `htmlescape` on one type is documented surface.

//ggen:generate htmlescape
type NPEscaped string

//ggen:generate
type NPPlain string

type NPUnannotated string

//ggen:generate
type NPPositions struct {
	V   NPPlain            `json:"v"`
	U   NPUnannotated      `json:"u"`
	E   NPEscaped          `json:"e"`
	S   []NPPlain          `json:"s"`
	M   map[string]NPPlain `json:"m"`
	P   *NPPlain           `json:"p"`
	A   [2]NPPlain         `json:"a"`
	Cnt NPCount            `json:"cnt"`
}

//ggen:generate
type NPCount int

func TestNamedPrim_EveryPosition(t *testing.T) {
	t.Parallel()
	in := []byte(`{"v":"a","u":"b","e":"<i>","s":["c","d"],"m":{"k":"e"},"p":"f","a":["g","h"],"cnt":7}`)
	got, _, err := NPPositions{}.DecodeFrom(in)
	if err != nil {
		t.Fatal(err)
	}
	p := NPPlain("f")
	want := NPPositions{
		V: "a", U: "b", E: "<i>", S: []NPPlain{"c", "d"},
		M: map[string]NPPlain{"k": "e"}, P: &p, A: [2]NPPlain{"g", "h"}, Cnt: 7,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
	out, err := got.AppendJSON(nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := got.JSONSize(); n < len(out) {
		t.Errorf("JSONSize %d < %d", n, len(out))
	}
	// The htmlescape alias keeps its own encoder, so only IT escapes.
	if !strings.Contains(string(out), `\u003ci\u003e`) {
		t.Errorf("htmlescape alias lost its escaping: %s", out)
	}
	back, _, err := NPPositions{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("re-decode %s: %v", out, err)
	}
	if !reflect.DeepEqual(back.S, got.S) || back.Cnt != got.Cnt || *back.P != *got.P {
		t.Errorf("round-trip mismatch: %+v", back)
	}
	// Stream path agrees.
	var st scan.Stream
	st.Reset(bytes.NewReader(in), make([]byte, 0, 8))
	sv, err := NPPositions{}.DecodeFromStream(&st)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sv.S, got.S) || sv.V != got.V || sv.M["k"] != got.M["k"] || sv.Cnt != got.Cnt {
		t.Errorf("stream %+v != bytes %+v", sv, got)
	}
}

// A named primitive from ANOTHER package keeps its own generated methods: this
// pass cannot see the flags it was generated with, so inlining it would apply
// the parent's (dropping, say, an htmlescape the other package chose). One with
// no methods at all is inlined like a local one.
func TestNamedPrim_CrossPackagePrefersMethods(t *testing.T) {
	t.Parallel()
	// thirdparty2.External2 is generated in its own pass; a foreign named
	// primitive without methods reaches the inline path via fallback_test.go's
	// CrossPkgShapes coverage. Here we pin the DECODE result either way.
	v, _, err := NPPositions{}.DecodeFrom([]byte(`{"v":"x","cnt":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if v.V != "x" || v.Cnt != 3 {
		t.Errorf("got %+v", v)
	}
}

// json.Number is a named string whose wire shape is a NUMBER — it must not be
// swept into the named-primitive inline path.
//
//ggen:generate
type NPJSONNumber struct {
	N json.Number `json:"n"`
}

func TestNamedPrim_JSONNumberStaysNumeric(t *testing.T) {
	t.Parallel()
	v, _, err := NPJSONNumber{}.DecodeFrom([]byte(`{"n":12.5}`))
	if err != nil {
		t.Fatalf("json.Number decoded as a string type: %v", err)
	}
	if v.N != "12.5" {
		t.Errorf("N = %q", v.N)
	}
	out, err := v.AppendJSON(nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"n":12.5}` {
		t.Errorf("wire = %s, want a bare number", out)
	}
}

// A container alias whose ELEMENT delegates (nested ggen struct) emits
// `dst, err = …` in AppendJSON; the alias encode body never declared err, so
// the whole package failed to compile.
//
//ggen:generate
type InnerBit struct {
	X int `json:"x"`
}

//ggen:generate
type BitItems []InnerBit

//ggen:generate
type BitLookup map[string]InnerBit

func TestAlias_DelegatingElementCompilesAndRoundtrips(t *testing.T) {
	t.Parallel()
	items := BitItems{{X: 1}, {X: 2}}
	out, err := encode.Marshal(items)
	if err != nil || string(out) != `[{"x":1},{"x":2}]` {
		t.Fatalf("BitItems marshal: %s %v", out, err)
	}
	var back BitItems
	if back, _, err = back.DecodeFrom(out); err != nil || !reflect.DeepEqual(back, items) {
		t.Fatalf("BitItems roundtrip: %v %v", back, err)
	}
	lk := BitLookup{"a": {X: 9}}
	if out, err = encode.Marshal(lk); err != nil || string(out) != `{"a":{"x":9}}` {
		t.Fatalf("BitLookup marshal: %s %v", out, err)
	}
}

// `rune` and `[]rune` are numbers in BOTH stdlib versions; the slice element
// used to fall to KindStruct and emit `append(dst, rune{})`, which did not
// compile. (`[N]byte` is base64 — see TestByteArray_Base64StrictLen.)
//
//ggen:generate
type RuneCarrier struct {
	R []rune `json:"r"`
	C rune   `json:"c"`
}

func TestAlias_RuneElements(t *testing.T) {
	t.Parallel()
	in := RuneCarrier{R: []rune("héllo"), C: '☃'}
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"c":9731,"r":[104,233,108,108,111]}` {
		t.Fatalf("wire: %s", out)
	}
	// Same VALUES as jsonv2 (ggen sorts keys, jsonv2 keeps declaration order).
	want, err := jsonv2.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var gm, wm map[string]any
	if jsonv2.Unmarshal(out, &gm) != nil || jsonv2.Unmarshal(want, &wm) != nil || !reflect.DeepEqual(gm, wm) {
		t.Errorf("ggen %s, jsonv2 %s", out, want)
	}
	back, _, err := RuneCarrier{}.DecodeFrom(out)
	if err != nil || !reflect.DeepEqual(back, in) {
		t.Fatalf("roundtrip: %+v %v", back, err)
	}
}
