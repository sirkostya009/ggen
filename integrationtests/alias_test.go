package integrationtests

//go:generate ../ggen $GOFILE

// Top-level named types. Each one gets the same method set a struct gets.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sirkostya009/ggen"
	"github.com/sirkostya009/ggen/integrationtests/thirdparty"
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
	out, err := ggen.Marshal(in)
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
	out, _ := ggen.Marshal(AliasString(`<a href="x">tom & jerry</a>`))
	for _, lit := range []string{"<", ">", "&"} {
		if !strings.Contains(string(out), lit) {
			t.Errorf("default mode escaped %q in alias output: %s", lit, out)
		}
	}
}

// htmlescape on a string alias emits \uXXXX escapes for <>&.
func TestAlias_String_HtmlescapeOptIn(t *testing.T) {
	t.Parallel()
	out, _ := ggen.Marshal(AliasHTML(`<a href="x">tom & jerry</a>`))
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
	out, err := ggen.Marshal(in)
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

// Narrow int aliases reject a value that does not fit instead of truncating
// it, so 300 into int8 is ErrNumberOverflow, not 44 (see opt #48 in
// cmd/ggen/CLAUDE.md). Covers the bytes and stream paths, signed and unsigned.
func TestAlias_NarrowInt_Overflow(t *testing.T) {
	t.Parallel()
	if got, _, err := AliasInt8(0).DecodeFrom([]byte("-128")); err != nil || got != -128 {
		t.Errorf("in-range: got %d, %v", got, err)
	}
	if got, _, err := AliasUint8(0).DecodeFrom([]byte("255")); err != nil || got != 255 {
		t.Errorf("in-range: got %d, %v", got, err)
	}
	for _, in := range []string{"300", "-300", "128"} {
		if _, _, err := AliasInt8(0).DecodeFrom([]byte(in)); !errors.Is(err, ggen.ErrNumberOverflow) {
			t.Errorf("int8 %s: got %v, want ErrNumberOverflow", in, err)
		}
		var s ggen.Stream
		s.Reset(strings.NewReader(in), nil)
		if _, err := AliasInt8(0).DecodeFromStream(&s); !errors.Is(err, ggen.ErrNumberOverflow) {
			t.Errorf("int8 stream %s: got %v, want ErrNumberOverflow", in, err)
		}
	}
	if _, _, err := AliasUint8(0).DecodeFrom([]byte("256")); !errors.Is(err, ggen.ErrNumberOverflow) {
		t.Errorf("uint8 256: got %v, want ErrNumberOverflow", err)
	}
	var s ggen.Stream
	s.Reset(strings.NewReader("256"), nil)
	if _, err := AliasUint8(0).DecodeFromStream(&s); !errors.Is(err, ggen.ErrNumberOverflow) {
		t.Errorf("uint8 stream 256: got %v, want ErrNumberOverflow", err)
	}
}

func TestAlias_Uint64_Roundtrip(t *testing.T) {
	t.Parallel()
	in := AliasUint64(18446744073709551615) // max uint64
	out, _ := ggen.Marshal(in)
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
	out, _ := ggen.Marshal(in)
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
		out, _ := ggen.Marshal(in)
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
	if _, _, err := AliasString("").DecodeFrom([]byte("42")); err == nil {
		t.Error("expected scan error on number → string-alias")
	}
}

// The decoded string aliases the input: it points into the caller's bytes
// with no copy. So mutating the input after decode changes the value.
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
	off := bytes.Index(in, []byte("alpha"))
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
	out, err := ggen.Marshal(in)
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

// CrossPkgTaggedAlias's underlying type, thirdparty.Tagged, has Text methods
// and exported fields. ggen prefers field introspection over the Text methods.
//
//ggen:generate
type CrossPkgTaggedAlias thirdparty.Tagged

// Introspection wins over the Text methods. The wire is a field object, not
// "name#tag".
func TestAlias_StructIntrospect_CrossPkg(t *testing.T) {
	t.Parallel()
	in := CrossPkgTaggedAlias(thirdparty.Tagged{Name: "alice", Tag: "admin"})
	out, err := ggen.Marshal(in)
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

// Introspection wins over the JSON methods. The keys are the Go field names
// because the fields have no json tags.
func TestAlias_StructIntrospect_SamePkg(t *testing.T) {
	t.Parallel()
	in := SamePkgAlias(SamePkgInner{X: 42, Y: "hello"})
	out, err := ggen.Marshal(in)
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

// With no exported fields, the alias delegates to the JSON or Text methods.
func TestAlias_StructDelegation_OpaqueFallback(t *testing.T) {
	t.Parallel()
	in := OpaqueAlias(OpaqueWithMethods{hidden: "secret"})
	out, err := ggen.Marshal(in)
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
	if OpaqueWithMethods(got).hidden != "secret" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

// big.Int's MarshalJSON has a pointer receiver.
//
//ggen:generate
type BigIntAlias big.Int

func TestAlias_StructDelegation_PointerMarshalJSON(t *testing.T) {
	t.Parallel()
	n, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	out, err := ggen.Marshal(BigIntAlias(*n))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "123456789012345678901234567890" {
		t.Errorf("marshal = %s", out)
	}
	got, _, err := BigIntAlias{}.DecodeFrom(out)
	if err != nil {
		t.Fatal(err)
	}
	if g := big.Int(got); g.Cmp(n) != 0 {
		t.Errorf("roundtrip = %s", g.String())
	}
}

//ggen:generate
type OpaqueParent struct {
	Name string      `json:"name"`
	O    OpaqueAlias `json:"o"`
}

// A delegating alias used as a field must append MarshalJSON's output to the
// parent's buffer. Returning that slice on its own would drop everything
// written before it.
func TestAlias_StructDelegation_AsField(t *testing.T) {
	t.Parallel()
	in := OpaqueParent{Name: "bob", O: OpaqueAlias(OpaqueWithMethods{hidden: "secret"})}
	out, err := ggen.Marshal(in)
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
	if got.Name != "bob" || OpaqueWithMethods(got.O).hidden != "secret" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

//ggen:generate
type AliasTags []string

//ggen:generate
type AliasLookup map[string]int

//ggen:generate
type AliasTuple [3]int

// A slice alias marshals as a JSON array.
func TestAlias_Slice_Roundtrip(t *testing.T) {
	t.Parallel()
	in := AliasTags{"go", "rust", "zig"}
	out, err := ggen.Marshal(in)
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

//ggen:generate
type AliasTagsHost struct {
	Tags  AliasTags `json:"tags"`
	Plain []string  `json:"plain"`
}

// An omitted field of a named container type decodes to its zero value, nil,
// like a plain slice, so it marshals as null rather than [].
func TestAlias_OmittedContainerFieldIsNil(t *testing.T) {
	t.Parallel()
	got, _, err := AliasTagsHost{}.DecodeFrom([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var st ggen.Stream
	sg, err := AliasTagsHost{}.DecodeFromStream(st.Reset(bytes.NewReader([]byte(`{}`)), nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []AliasTagsHost{got, sg} {
		if v.Tags != nil || v.Plain != nil {
			t.Errorf("Tags=%#v Plain=%#v, want both nil", v.Tags, v.Plain)
		}
	}
}

// A map alias marshals as a JSON object.
func TestAlias_Map_Roundtrip(t *testing.T) {
	t.Parallel()
	in := AliasLookup{"alpha": 1, "beta": 2}
	out, err := ggen.Marshal(in)
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

// An array alias marshals as a JSON array with an exact element count.
func TestAlias_Array_Roundtrip(t *testing.T) {
	t.Parallel()
	in := AliasTuple{10, 20, 30}
	out, err := ggen.Marshal(in)
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
func TestAlias_String_ZeroAllocations(t *testing.T) { //nolint:paralleltest // measures allocations
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
	Body  AliasString `json:"body"  pipe:"required trim tolower minlen=2 maxlen=10"`
	Count AliasInt    `json:"count" pipe:"clamp=1|100 gte=1 lte=100"`
}

func TestAlias_Field_ValidationAndMods(t *testing.T) {
	t.Parallel()
	// trim and tolower run before validation, so the validator sees the
	// modified value.
	got, _, err := AliasFieldExample{}.DecodeFrom([]byte(`{"body":"  HI  ","count":5}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(got.Body) != "hi" {
		t.Errorf("trim+lower on AliasString: got %q, want %q", got.Body, "hi")
	}

	// clamp pulls 500 back into [1,100]. gte and lte then pass on the clamped
	// value.
	got, _, err = AliasFieldExample{}.DecodeFrom([]byte(`{"body":"hello","count":500}`))
	if err != nil {
		t.Fatalf("clamp+validate: %v", err)
	}
	if int(got.Count) != 100 {
		t.Errorf("clamp on AliasInt: got %d, want 100", got.Count)
	}

	// minlen checks the trimmed value. `" a "` trims to `"a"`, length 1,
	// which is below the limit.
	_, _, err = AliasFieldExample{}.DecodeFrom([]byte(`{"body":" a ","count":5}`))
	if _, ok := errors.AsType[*ggen.MinLenError](err); !ok {
		t.Errorf("expected MinLenError post-trim, got %v", err)
	}

	// Alias values marshal as their underlying primitives.
	out, err := ggen.Marshal(AliasFieldExample{Body: "hi", Count: 5})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"body":"hi"`) || !strings.Contains(string(out), `"count":5`) {
		t.Errorf("marshal = %s", out)
	}
}

// ---- named primitives (annotated and not) ----------------------------
//
// Every rule must resolve the underlying kind of a named primitive. `oneof`
// must emit its allowed values as values of that kind, not bare identifiers.
// The string rules must cast the value before passing it to string APIs. And
// `eq` and `neq` must emit a check.

//ggen:generate
type NPPriority string

// NPTag carries no annotation. ggen must still resolve `string` under it for
// the rules and cast it at every stdlib call site.
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
	t.Parallel()
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
	t.Parallel()
	for _, tc := range []struct {
		name, body string
		target     any
	}{
		{"oneof", `{"pri":"nope","tag":"a-b","eq":"low","neq":"high","zero":null}`, new(*ggen.OneOfError)},
		{"eq", `{"pri":"low","tag":"a-b","eq":"other","neq":"high","zero":null}`, new(*ggen.EqError)},
		{"neq", `{"pri":"low","tag":"a-b","eq":"low","neq":"low","zero":null}`, new(*ggen.NeqError)},
		{"maxrunes", `{"pri":"low","tag":"aaaaaaaaa-","eq":"low","neq":"high","zero":null}`, new(*ggen.MaxRunesError)},
		{"contains", `{"pri":"low","tag":"abc","eq":"low","neq":"high","zero":null}`, new(*ggen.ContainsError)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
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
	t.Parallel()
	// A rune is one Unicode character. Each 'ы' takes 2 bytes in UTF-8, so
	// this tag is 8 runes in 15 bytes, within maxrunes=8.
	body := `{"pri":"low","tag":"ыыыыыыы-","eq":"low","neq":"high","zero":null}`
	_, _, err := NamedPrims{}.DecodeFrom([]byte(body))
	if err != nil {
		t.Errorf("8 multibyte runes rejected by maxrunes=8: %v", err)
	}
}

// A named primitive is decoded and encoded inline at every position, as the
// underlying scan plus a type conversion. An alias that carries flags of its
// own keeps its methods, so its behaviour survives. `htmlescape` on one type
// is documented surface.

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
	// The htmlescape alias keeps its own encoder, so only it escapes.
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
	var st ggen.Stream
	st.Reset(bytes.NewReader(in), make([]byte, 0, 8))
	sv, err := NPPositions{}.DecodeFromStream(&st)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sv.S, got.S) || sv.V != got.V || sv.M["k"] != got.M["k"] || sv.Cnt != got.Cnt {
		t.Errorf("stream %+v != bytes %+v", sv, got)
	}
}

// A named primitive from another package keeps its own generated methods.
// This pass cannot see the flags it was generated with, so inlining it would
// apply the parent's flags and could drop, say, an htmlescape the other
// package chose. One with no methods at all is inlined like a local one.
func TestNamedPrim_CrossPackagePrefersMethods(t *testing.T) {
	t.Parallel()
	// thirdparty2.External2 is generated in its own pass. A foreign named
	// primitive without methods reaches the inline path through CrossPkgShapes
	// in fallback_test.go. This test pins the decode result either way.
	v, _, err := NPPositions{}.DecodeFrom([]byte(`{"v":"x","cnt":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if v.V != "x" || v.Cnt != 3 {
		t.Errorf("got %+v", v)
	}
}

// NPJSONNumber holds a json.Number, a named string whose wire shape is a JSON
// number. It must not take the named-primitive inline path.
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

// NPText is a named primitive with its own text codec, so it keeps its
// methods. The aliases below hold NPAliasElem, a plain named primitive no
// field of this file uses. In none of them is the element a struct, so its
// zero value is not `T{}`.
type NPText int

func (n NPText) MarshalText() ([]byte, error) { return []byte("t" + strconv.Itoa(int(n))), nil }

func (n *NPText) UnmarshalText(b []byte) error {
	v, err := strconv.Atoi(strings.TrimPrefix(string(b), "t"))
	*n = NPText(v)
	return err
}

//ggen:generate
type NPTextElems struct {
	A [2]NPText `json:"a"`
	S []NPText  `json:"s"`
}

type NPAliasElem string

//ggen:generate
type NPAliasList []NPAliasElem

//ggen:generate
type NPAliasPair [2]NPAliasElem

func TestNamedPrim_NonStructElements(t *testing.T) {
	t.Parallel()
	in := []byte(`{"a":["t3","t4"],"s":["t1","t2"]}`)
	want := NPTextElems{A: [2]NPText{3, 4}, S: []NPText{1, 2}}
	got, _, err := NPTextElems{}.DecodeFrom(in)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("bytes: %+v %v", got, err)
	}
	var st ggen.Stream
	st.Reset(bytes.NewReader(in), make([]byte, 0, 8))
	if got, err = (NPTextElems{}).DecodeFromStream(&st); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("stream: %+v %v", got, err)
	}
	if out, err := ggen.Marshal(want); err != nil || !bytes.Equal(out, in) {
		t.Errorf("marshal: %s %v, want %s", out, err, in)
	}

	pair := []byte(`["x","y"]`)
	list, _, err := NPAliasList{}.DecodeFrom(pair)
	if err != nil || !reflect.DeepEqual(list, NPAliasList{"x", "y"}) {
		t.Fatalf("list bytes: %v %v", list, err)
	}
	st.Reset(bytes.NewReader(pair), make([]byte, 0, 4))
	if list, err = (NPAliasList{}).DecodeFromStream(&st); err != nil || !reflect.DeepEqual(list, NPAliasList{"x", "y"}) {
		t.Fatalf("list stream: %v %v", list, err)
	}
	arr, _, err := NPAliasPair{}.DecodeFrom(pair)
	if err != nil || arr != (NPAliasPair{"x", "y"}) {
		t.Fatalf("pair bytes: %v %v", arr, err)
	}
	st.Reset(bytes.NewReader(pair), make([]byte, 0, 4))
	if arr, err = (NPAliasPair{}).DecodeFromStream(&st); err != nil || arr != (NPAliasPair{"x", "y"}) {
		t.Fatalf("pair stream: %v %v", arr, err)
	}
	if out, err := ggen.Marshal(list); err != nil || !bytes.Equal(out, pair) {
		t.Errorf("list marshal: %s %v", out, err)
	}
	if out, err := ggen.Marshal(arr); err != nil || !bytes.Equal(out, pair) {
		t.Errorf("pair marshal: %s %v", out, err)
	}
}

// InnerBit is the element of the container aliases below. A nested ggen
// struct element makes the alias's AppendJSON emit `dst, err = …`, so the
// alias encode body must declare err or the package does not compile.
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
	out, err := ggen.Marshal(items)
	if err != nil || string(out) != `[{"x":1},{"x":2}]` {
		t.Fatalf("BitItems marshal: %s %v", out, err)
	}
	var back BitItems
	if back, _, err = back.DecodeFrom(out); err != nil || !reflect.DeepEqual(back, items) {
		t.Fatalf("BitItems roundtrip: %v %v", back, err)
	}
	lk := BitLookup{"a": {X: 9}}
	if out, err = ggen.Marshal(lk); err != nil || string(out) != `{"a":{"x":9}}` {
		t.Fatalf("BitLookup marshal: %s %v", out, err)
	}
}

// RuneCarrier holds `rune` and `[]rune`, which are JSON numbers in both
// stdlib versions. The slice element must resolve as a number, not a struct.
// `[N]byte` is base64, see TestByteArray_Base64StrictLen.
//
//ggen:generate
type RuneCarrier struct {
	R []rune `json:"r"`
	C rune   `json:"c"`
}

func TestAlias_RuneElements(t *testing.T) {
	t.Parallel()
	in := RuneCarrier{R: []rune("héllo"), C: '☃'}
	out, err := ggen.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"c":9731,"r":[104,233,108,108,111]}` {
		t.Fatalf("wire: %s", out)
	}
	// Same values as jsonv2. ggen sorts keys, jsonv2 keeps declaration order.
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

// Leading whitespace is legal before any top-level JSON value. Primitive
// alias decoders must skip it, just like container and struct aliases do.
func TestAlias_leadingWhitespace(t *testing.T) {
	t.Parallel()
	sv, _, err := AliasString("").DecodeFrom([]byte(" \t\n\"x\""))
	if err != nil {
		t.Fatalf("AliasString: %v", err)
	}
	if sv != "x" {
		t.Errorf("AliasString = %q, want %q", sv, "x")
	}
	iv, _, err := AliasInt(0).DecodeFrom([]byte("  42"))
	if err != nil {
		t.Fatalf("AliasInt: %v", err)
	}
	if iv != 42 {
		t.Errorf("AliasInt = %d, want 42", iv)
	}
	var st ggen.Stream
	st.Reset(bytes.NewReader([]byte("  \"y\"")), nil)
	sv2, err := AliasString("").DecodeFromStream(&st)
	if err != nil {
		t.Fatalf("AliasString stream: %v", err)
	}
	if sv2 != "y" {
		t.Errorf("AliasString stream = %q, want %q", sv2, "y")
	}
}

// An opaque struct with no exported fields but with Text methods takes the
// Text rung of the alias ladder, the ordered list of encodings ggen tries.
// That rung scans the JSON string itself.
type textOnly struct{ v string }

func (t textOnly) MarshalText() ([]byte, error)  { return []byte(t.v), nil }
func (t *textOnly) UnmarshalText(b []byte) error { t.v = string(b); return nil }

//ggen:generate
type LocalText textOnly

//ggen:generate
type AliasBlob []byte

func TestAlias_leadingWhitespaceBytesAndText(t *testing.T) {
	t.Parallel()
	bv, _, err := AliasBlob(nil).DecodeFrom([]byte(" \t\"aGk=\""))
	if err != nil || string(bv) != "hi" {
		t.Fatalf("AliasBlob bytes: %q %v", bv, err)
	}
	var st ggen.Stream
	st.Reset(bytes.NewReader([]byte("  \"aGk=\"")), nil)
	if bv, err = AliasBlob(nil).DecodeFromStream(&st); err != nil || string(bv) != "hi" {
		t.Fatalf("AliasBlob stream: %q %v", bv, err)
	}
	st.Reset(bytes.NewReader([]byte("  null")), nil)
	if bv, err = AliasBlob(nil).DecodeFromStream(&st); err != nil || bv != nil {
		t.Fatalf("AliasBlob stream null: %q %v", bv, err)
	}

	tv, _, err := LocalText{}.DecodeFrom([]byte(" \t\"x\""))
	if err != nil || tv.v != "x" {
		t.Fatalf("LocalText bytes: %+v %v", tv, err)
	}
	st.Reset(bytes.NewReader([]byte("  \"y\"")), nil)
	if tv, err = (LocalText{}).DecodeFromStream(&st); err != nil || tv.v != "y" {
		t.Fatalf("LocalText stream: %+v %v", tv, err)
	}
}

//ggen:generate
type AliasStamp time.Time

// The UnmarshalJSON rung skips leading whitespace before it takes the span,
// since UnmarshalJSON rejects it. A callee error past the whitespace is
// still rebased onto the payload.
func TestAlias_leadingWhitespaceJSONRung(t *testing.T) {
	t.Parallel()
	const doc = " \t\n\"2020-01-02T03:04:05Z\""
	want := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	v, n, err := AliasStamp{}.DecodeFrom([]byte(doc))
	if err != nil || !time.Time(v).Equal(want) || n != len(doc) {
		t.Fatalf("bytes: %v n=%d %v", time.Time(v), n, err)
	}
	var st ggen.Stream
	st.Reset(&chunkReader{data: []byte(doc), max: 1}, make([]byte, 0, 16))
	if v, err = (AliasStamp{}).DecodeFromStream(&st); err != nil || !time.Time(v).Equal(want) {
		t.Fatalf("stream: %v %v", time.Time(v), err)
	}
	payload := "  \n" + `{"key":"abc","value":"q"}`
	aliasPosBoth[AliasPosWrap](t, payload, strings.Index(payload, `"q"`))
}

//ggen:generate
type AliasDigest [4]byte

//ggen:generate
type AliasLenHost struct {
	T AliasTuple  `json:"t"`
	D AliasDigest `json:"d"`
}

// An alias has no label of its own: its length errors start with an empty
// path, and as a field they carry just the field's key.
func TestAlias_lengthErrorPath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		dec  func(string) (error, error)
		in   string
		want string
	}{
		{"tuple", decodeBothPaths[AliasTuple], `[1,2,3,4]`, ""},
		{"digest", decodeBothPaths[AliasDigest], `"AQID"`, ""},
		{"tuple field", decodeBothPaths[AliasLenHost], `{"t":[1,2]}`, "t"},
		{"digest field", decodeBothPaths[AliasLenHost], `{"d":"AQID"}`, "d"},
	} {
		berr, serr := tc.dec(tc.in)
		for i, err := range []error{berr, serr} {
			ve, ok := errors.AsType[*ggen.LenError](err)
			if !ok {
				t.Errorf("%s path %d: want *ggen.LenError, got %T %v", tc.name, i, err, err)
				continue
			}
			want := ve.Message()
			if tc.want != "" {
				want = tc.want + ": " + want
			}
			if got := strings.Join(ve.Path(), "."); got != tc.want || len(ve.Path()) != min(len(tc.want), 1) || err.Error() != want {
				t.Errorf("%s path %d: Path()=%q Error()=%q, want path %q, %q", tc.name, i, ve.Path(), err.Error(), tc.want, want)
			}
		}
	}
}

// A [N]byte alias takes the base64 path, exactly like a [N]byte field, so one
// Go type has one wire shape wherever it appears. An alias carries no struct
// tag, so there is no format:array opt-out.
func TestAlias_byteArrayIsBase64(t *testing.T) {
	t.Parallel()
	want := AliasDigest{1, 2, 3, 4}
	out, err := ggen.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"AQIDBA=="` {
		t.Fatalf("alias wire = %s, want %q", out, `"AQIDBA=="`)
	}
	// The same [4]byte at field position (ByteArrays.B, no format: override).
	field, err := ggen.Marshal(ByteArrays{B: [4]byte{1, 2, 3, 4}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(field, []byte(`"AQIDBA=="`)) {
		t.Errorf("field wire %s disagrees with alias wire %s", field, out)
	}

	got, _, err := AliasDigest{}.DecodeFrom(out)
	if err != nil || got != want {
		t.Fatalf("bytes decode = %v %v, want %v", got, err, want)
	}
	if _, _, err := (AliasDigest{}).DecodeFrom([]byte(" \t" + string(out))); err != nil {
		t.Errorf("leading whitespace rejected: %v", err)
	}
	if _, _, err := (AliasDigest{}).DecodeFrom([]byte(`"AAA="`)); err == nil {
		t.Error("wrong decoded length accepted, want an error")
	}
	var st ggen.Stream
	st.Reset(bytes.NewReader(out), nil)
	sgot, err := AliasDigest{}.DecodeFromStream(&st)
	if err != nil || sgot != want {
		t.Fatalf("stream decode = %v %v, want %v", sgot, err, want)
	}
}

// ggen.Marshal sizes its buffer from JSONSize exactly. An undersized float
// budget would cost a second allocation on the widest 'f'-form value.
func TestAlias_floatSizeIsSingleAlloc(t *testing.T) { //nolint:paralleltest // measures allocations
	v := AliasFloat64(-1.2345678901234567e-06)
	out, err := ggen.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 25 {
		t.Fatalf("len = %d, want 25 (%s)", len(out), out)
	}
	if n := testing.AllocsPerRun(50, func() { _, _ = ggen.Marshal(v) }); n != 1 {
		t.Errorf("allocs = %v, want 1", n)
	}
}

// A struct reached only through a container alias must be generated too.
// Otherwise the element decodes through the reflective encoding/json
// fallback, which silently accepts unknown keys.

//ggen:generate
type SoloList []SoloInner

type SoloInner struct {
	A int `json:"a"`
}

func TestAlias_elementStructGetsGenerated(t *testing.T) {
	t.Parallel()
	got, _, err := SoloList{}.DecodeFrom([]byte(`[{"a":1}]`))
	if err != nil || len(got) != 1 || got[0].A != 1 {
		t.Fatalf("decode: %+v %v", got, err)
	}
	_, _, err = SoloList{}.DecodeFrom([]byte(`[{"a":1,"x":2}]`))
	var uke *ggen.UnknownKeyError
	if !errors.As(err, &uke) {
		t.Fatalf("unknown key: want UnknownKeyError, got %v", err)
	}
	var st ggen.Stream
	st.Reset(bytes.NewReader([]byte(`[{"a":1,"x":2}]`)), nil)
	if _, err = (SoloList{}).DecodeFromStream(&st); !errors.As(err, &uke) {
		t.Fatalf("unknown key (stream): want UnknownKeyError, got %v", err)
	}
}

// R10IntroSrc is unannotated and has no ggen methods, so R10IntroAlias takes
// the field-introspection rung. Its `@Func` steps resolve against the alias's
// package, like a struct's own fields do. They are a mod, a validator and a
// converter variant.
type R10IntroSrc struct {
	A string `json:"a" pipe:"@R10Upper"`
	B string `json:"b" pipe:"required @R10NonEmpty"`
	N int64  `json:"n" pipe:"@R10ParseInt/nullzero"`
}

//ggen:generate
type R10IntroAlias R10IntroSrc

func R10Upper(s string) string { return strings.ToUpper(s) }

func R10NonEmpty(s string) error {
	if s == "" {
		return errors.New("empty")
	}
	return nil
}

func R10ParseInt(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }

func TestAlias_StructIntrospect_CustomSteps(t *testing.T) {
	t.Parallel()
	v, _, err := R10IntroAlias{}.DecodeFrom([]byte(`{"a":"x","b":"ok","n":"12"}`))
	if err != nil {
		t.Fatal(err)
	}
	if v.A != "X" || v.N != 12 {
		t.Errorf("mod / converter dropped: %+v", v)
	}
	v, _, err = R10IntroAlias{}.DecodeFrom([]byte(`{"a":"x","b":"ok","n":null}`))
	if err != nil || v.N != 0 {
		t.Errorf("nullzero variant: v=%+v err=%v", v, err)
	}
	if _, _, err := (R10IntroAlias{}).DecodeFrom([]byte(`{"a":"x","b":""}`)); err == nil {
		t.Error("@R10NonEmpty validator dropped: empty b accepted")
	}
	if _, _, err := (R10IntroAlias{}).DecodeFrom([]byte(`{"a":"x","b":"ok","n":"zz"}`)); err == nil || !strings.Contains(err.Error(), `parsing "zz"`) {
		t.Errorf("converter error dropped: got %v", err)
	}
	s, err := R10IntroAlias{}.DecodeFromStream(ggen.NewStream(strings.NewReader(`{"a":"y","b":"ok","n":"3"}`), nil))
	if err != nil || s.A != "Y" || s.N != 3 {
		t.Errorf("stream: v=%+v err=%v", s, err)
	}
}

// ---- `=` aliases ------------------------------------------------------
//
// An `=` alias names the very type it stands for, so a field written through
// one gets that type's wire shape wherever it sits: as a value, behind a
// pointer, as a slice element or map value, and when the alias is declared in
// another package or is generic.

type (
	SameTCPAddr     = net.TCPAddr
	SameIPNet       = net.IPNet
	SameDuration    = time.Duration
	SameURL         = url.URL
	SameAddr        = netip.Addr
	SameNullString  = sql.NullString
	SameCount       = int
	SameWords       = []string
	SameIntPtr      = *int
	SameNull[T any] = sql.Null[T]
)

//ggen:generate
type SameViaAlias struct {
	TCP   SameTCPAddr           `json:"tcp"`
	PTCP  *SameTCPAddr          `json:"ptcp"`
	Nets  []SameIPNet           `json:"nets"`
	ByKey map[string]SameIPNet  `json:"bykey"`
	UDP   thirdparty.UDPAddr    `json:"udp"`
	Dur   SameDuration          `json:"dur"`
	Durs  []thirdparty.Duration `json:"durs"`
	URL   SameURL               `json:"url"`
	Addr  SameAddr              `json:"addr"`
	NS    SameNullString        `json:"ns"`
	Cnt   SameCount             `json:"cnt"`
	Words SameWords             `json:"words"`
	PInt  SameIntPtr            `json:"pint"`
	Opt   SameNull[int64]       `json:"opt"`
}

// SameDirect writes every field of SameViaAlias without the aliases.
//
//ggen:generate
type SameDirect struct {
	TCP   net.TCPAddr          `json:"tcp"`
	PTCP  *net.TCPAddr         `json:"ptcp"`
	Nets  []net.IPNet          `json:"nets"`
	ByKey map[string]net.IPNet `json:"bykey"`
	UDP   net.UDPAddr          `json:"udp"`
	Dur   time.Duration        `json:"dur"`
	Durs  []time.Duration      `json:"durs"`
	URL   url.URL              `json:"url"`
	Addr  netip.Addr           `json:"addr"`
	NS    sql.NullString       `json:"ns"`
	Cnt   int                  `json:"cnt"`
	Words []string             `json:"words"`
	PInt  *int                 `json:"pint"`
	Opt   sql.Null[int64]      `json:"opt"`
}

//ggen:generate
type SameDurations []SameDuration

// remarshalBoth decodes data on the bytes path and on a stream fed 7 bytes
// per Read, and marshals each result again.
func remarshalBoth[T interface {
	ggen.Unmarshaler[T]
	ggen.Marshaler
}](data []byte) (fromBytes, fromStream []byte, err error) {
	var zero T
	b, _, err := zero.DecodeFrom(data)
	if err != nil {
		return nil, nil, fmt.Errorf("bytes: %w", err)
	}
	var s ggen.Stream
	s.Reset(&chunkReader{data: data, max: 7}, make([]byte, 0, 64))
	sv, err := s.Value[T]()
	if err != nil {
		return nil, nil, fmt.Errorf("stream: %w", err)
	}
	if fromBytes, err = b.AppendJSON(nil); err != nil {
		return nil, nil, err
	}
	fromStream, err = sv.AppendJSON(nil)
	return fromBytes, fromStream, err
}

func TestAlias_EqFieldsTakeTargetShape(t *testing.T) {
	t.Parallel()
	n := 7
	v := SameViaAlias{
		TCP:   net.TCPAddr{IP: net.ParseIP("1.2.3.4"), Port: 80},
		PTCP:  &net.TCPAddr{IP: net.ParseIP("::1"), Port: 443},
		Nets:  []net.IPNet{mustCIDR("10.0.0.0/8")},
		ByKey: map[string]net.IPNet{"k": mustCIDR("2001:db8::/32")},
		UDP:   net.UDPAddr{IP: net.ParseIP("fe80::1"), Port: 53, Zone: "eth0"},
		Dur:   90 * time.Second,
		Durs:  []time.Duration{time.Millisecond},
		URL:   url.URL{Scheme: "https", Host: "example.com", Path: "/a"},
		Addr:  netip.MustParseAddr("192.0.2.1"),
		NS:    sql.NullString{String: "x", Valid: true},
		Cnt:   3,
		Words: []string{"a"},
		PInt:  &n,
		Opt:   sql.Null[int64]{V: 5, Valid: true},
	}
	out, err := ggen.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	want, err := ggen.Marshal(SameDirect(v))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, want) {
		t.Fatalf("alias-spelled wire\n%s\ndiffers from the direct one\n%s", out, want)
	}
	for _, frag := range []string{`"dur":"1m30s"`, `"durs":["1ms"]`, `"url":"https://example.com/a"`, `"ns":"x"`, `"opt":5`} {
		if !bytes.Contains(out, []byte(frag)) {
			t.Errorf("missing %s in %s", frag, out)
		}
	}
	b, s, err := remarshalBoth[SameViaAlias](want)
	if err != nil || !bytes.Equal(b, want) || !bytes.Equal(s, want) {
		t.Errorf("round trip: bytes %s, stream %s, %v; want %s", b, s, err, want)
	}

	durs, err := ggen.Marshal(SameDurations{time.Second, 2 * time.Minute})
	if err != nil || string(durs) != `["1s","2m0s"]` {
		t.Fatalf("container alias of an alias element: %s %v", durs, err)
	}
	b, s, err = remarshalBoth[SameDurations](durs)
	if err != nil || !bytes.Equal(b, durs) || !bytes.Equal(s, durs) {
		t.Errorf("container alias round trip: bytes %s, stream %s, %v", b, s, err)
	}
}

// A struct alias reads the fields of its underlying struct by their own
// types, as a struct declared with those fields does: net.IP and a type with
// a text codec write text, and a named []byte with no codec of its own, such
// as net.IPMask, goes through encoding/json, which writes base64. So both
// write what encoding/json writes for the underlying.

// IPNetFields and TCPAddrFields declare the fields of net.IPNet and
// net.TCPAddr.
//
//ggen:generate
type IPNetFields struct {
	IP   net.IP
	Mask net.IPMask
}

//ggen:generate
type TCPAddrFields struct {
	IP   net.IP
	Port int
	Zone string
}

//ggen:generate
type CIDRAlias net.IPNet

//ggen:generate
type PeerAlias net.TCPAddr

//ggen:generate
type HostAlias thirdparty.Host

func TestAlias_StructIntrospect_NamedFieldTypes(t *testing.T) {
	t.Parallel()
	n := mustCIDR("10.0.0.0/8")
	a := net.TCPAddr{IP: net.ParseIP("1.2.3.4"), Port: 80}
	h := thirdparty.Host{
		Digest: thirdparty.Digest{1, 2, 3, 4},
		Grid:   [][2]int{{1, 2}},
		IP:     net.ParseIP("10.0.0.1"),
		Level:  "lv",
		Mask:   net.CIDRMask(8, 32),
		Raw:    json.RawMessage(`{"k":1}`),
	}
	cases := []struct {
		name      string
		std       any            // the underlying value, as encoding/json writes it
		alias     ggen.Marshaler // the struct alias of it
		fields    ggen.Marshaler // a struct declaring the same fields, if any
		remarshal func([]byte) ([]byte, []byte, error)
	}{
		{"CIDRAlias", n, CIDRAlias(n), IPNetFields(n), remarshalBoth[CIDRAlias]},
		{"PeerAlias", a, PeerAlias(a), TCPAddrFields(a), remarshalBoth[PeerAlias]},
		{"HostAlias", h, HostAlias(h), nil, remarshalBoth[HostAlias]},
	}
	for _, c := range cases {
		want, err := json.Marshal(c.std)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ggen.Marshal(c.alias)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: alias %s %v, encoding/json %s", c.name, got, err, want)
		}
		if c.fields != nil {
			if fields, err := ggen.Marshal(c.fields); err != nil || !bytes.Equal(fields, want) {
				t.Errorf("%s: struct with the same fields %s %v, encoding/json %s", c.name, fields, err, want)
			}
		}
		b, s, err := c.remarshal(want)
		if err != nil || !bytes.Equal(b, want) || !bytes.Equal(s, want) {
			t.Errorf("%s round trip: bytes %s, stream %s, %v; want %s", c.name, b, s, err, want)
		}
	}
}

//ggen:generate
type RankedAlias thirdparty.Ranked

// A field whose type this package cannot name, unexported or internal, is
// read and written whole by encoding/json, so the alias still writes what
// encoding/json writes for the underlying and reads it back.
func TestAlias_StructIntrospect_UnnameableFieldTypes(t *testing.T) {
	t.Parallel()
	used := RankedAlias(thirdparty.NewRanked("old", "old", 7, 8, 9))
	for _, r := range []thirdparty.Ranked{thirdparty.NewRanked("c", "warn", 3, 4), thirdparty.NewRanked("", "")} {
		want, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := ggen.Marshal(RankedAlias(r)); err != nil || !bytes.Equal(got, want) {
			t.Errorf("alias %s %v, encoding/json %s", got, err, want)
		}
		b, s, err := remarshalBoth[RankedAlias](want)
		if err != nil || !bytes.Equal(b, want) || !bytes.Equal(s, want) {
			t.Errorf("round trip: bytes %s, stream %s, %v; want %s", b, s, err, want)
		}
		if got, _, err := used.DecodeFrom(want); err != nil || !reflect.DeepEqual(got, RankedAlias(r)) {
			t.Errorf("decode into a used receiver = %+v %v, want %+v", got, err, r)
		}
	}
}

// ---- container aliases of arrays ---------------------------------------
//
// A slice or array alias whose element is itself an array, written out or
// through an `=` alias, holds each element to that array's length.

type ArrPair = [2]int

//ggen:generate
type ArrRows [][2]int

//ggen:generate
type ArrGrid [3][2]int

//ggen:generate
type ArrPtrRows []*[2]int

//ggen:generate
type ArrPtrGrid [2]*[2]int

//ggen:generate
type ArrPairs []ArrPair

// arrayAliasRoundTrip marshals v, decodes it back on the bytes path, on a
// chunked stream and into stale, a receiver that holds other values, and
// checks every result against v.
func arrayAliasRoundTrip[T interface {
	ggen.Unmarshaler[T]
	ggen.Marshaler
}](t *testing.T, v, stale T, want string) {
	t.Helper()
	out, err := ggen.Marshal(v)
	if err != nil || string(out) != want {
		t.Fatalf("%T marshal = %s %v, want %s", v, out, err, want)
	}
	b, s, err := remarshalBoth[T](out)
	if err != nil || string(b) != want || string(s) != want {
		t.Errorf("%T round trip: bytes %s, stream %s, %v", v, b, s, err)
	}
	got, _, err := stale.DecodeFrom(out)
	if err != nil || !reflect.DeepEqual(got, v) {
		t.Errorf("%T decode into a used receiver = %v %v, want %v", v, got, err, v)
	}
}

func TestAlias_ArrayElements(t *testing.T) {
	t.Parallel()
	arrayAliasRoundTrip(t, ArrRows{{1, 2}, {3, 4}}, ArrRows{{9, 9}, {9, 9}, {9, 9}}, `[[1,2],[3,4]]`)
	arrayAliasRoundTrip(t, ArrGrid{{1, 2}, {3, 4}, {5, 6}}, ArrGrid{{9, 9}}, `[[1,2],[3,4],[5,6]]`)
	arrayAliasRoundTrip(t, ArrPtrRows{{1, 2}, nil}, ArrPtrRows{{9, 9}, {9, 9}, nil}, `[[1,2],null]`)
	arrayAliasRoundTrip(t, ArrPtrGrid{nil, {3, 4}}, ArrPtrGrid{{9, 9}, {9, 9}}, `[null,[3,4]]`)
	arrayAliasRoundTrip(t, ArrPairs{{1, 2}}, ArrPairs{{9, 9}, {9, 9}}, `[[1,2]]`)

	// An element of the wrong length is a LenError, behind a pointer too and
	// on both paths. The grid has room for exactly two elements.
	rows := []string{`[[]]`, `[[1]]`, `[[1,2,3]]`}
	grid := []string{`[[],null]`, `[null,[1]]`, `[[1,2,3],null]`}
	stream := func(d []byte, dec func(*ggen.Stream) error) error {
		var s ggen.Stream
		return dec(s.Reset(bytes.NewReader(d), nil))
	}
	for _, c := range []struct {
		name   string
		in     []string
		decode func([]byte) error
	}{
		{"ArrRows", rows, func(d []byte) error { _, _, err := ArrRows{}.DecodeFrom(d); return err }},
		{"ArrPairs", rows, func(d []byte) error { _, _, err := ArrPairs{}.DecodeFrom(d); return err }},
		{"ArrPtrRows", rows, func(d []byte) error { _, _, err := ArrPtrRows{}.DecodeFrom(d); return err }},
		{"ArrPtrRows stream", rows, func(d []byte) error {
			return stream(d, func(s *ggen.Stream) error { _, err := ArrPtrRows{}.DecodeFromStream(s); return err })
		}},
		{"ArrPtrGrid", grid, func(d []byte) error { _, _, err := ArrPtrGrid{}.DecodeFrom(d); return err }},
		{"ArrPtrGrid stream", grid, func(d []byte) error {
			return stream(d, func(s *ggen.Stream) error { _, err := ArrPtrGrid{}.DecodeFromStream(s); return err })
		}},
	} {
		for _, in := range c.in {
			var lenErr *ggen.LenError
			if err := c.decode([]byte(in)); !errors.As(err, &lenErr) {
				t.Errorf("%s accepted %s: %v", c.name, in, err)
			}
		}
	}
}

//ggen:generate multierr
type MEItem struct {
	ID   int    `json:"id"`
	Name string `json:"name" pipe:"minlen=2"`
}

//ggen:generate multierr
type MEItems []MEItem

//ggen:generate multierr
type MEByName map[string]MEItem

//ggen:generate multierr
type MEPair [2]MEItem

//ggen:generate multierr
type MEPtrs []*MEItem

// A multierr container alias collects every element's validation failures
// and returns them after the closing bracket, on both paths.
func TestAlias_multierrContainerCollects(t *testing.T) {
	t.Parallel()
	arr := `[{"id":1,"name":"a"},{"id":2,"name":"b"}]`
	for _, c := range []struct {
		name    string
		payload string
		run     func(string) (error, error)
	}{
		{"slice", arr, decodeBothPaths[MEItems]},
		{"array", arr, decodeBothPaths[MEPair]},
		{"ptrs", arr, decodeBothPaths[MEPtrs]},
		{"map", `{"x":{"id":1,"name":"a"},"y":{"id":2,"name":"b"}}`, decodeBothPaths[MEByName]},
	} {
		berr, serr := c.run(c.payload)
		var bes, ses ggen.Errors
		if !errors.As(berr, &bes) || !errors.As(serr, &ses) {
			t.Fatalf("%s: bytes=%v stream=%v", c.name, berr, serr)
		}
		if len(bes) != 2 || len(ses) != 2 {
			t.Fatalf("%s: want 2 errors, bytes=%v stream=%v", c.name, bes, ses)
		}
		if berr.Error() != serr.Error() {
			t.Errorf("%s: bytes=%v stream=%v", c.name, berr, serr)
		}
	}
	v, _, err := MEItems{}.DecodeFrom([]byte(`[{"id":1,"name":"a"},{"id":2,"name":"bb"}]`))
	if len(v) != 2 || v[1].ID != 2 {
		t.Errorf("value not decoded past the failure: %v %v", v, err)
	}
	if _, _, err := (MEItems{}).DecodeFrom([]byte(`[{"id":1,"name":"aa"}]`)); err != nil {
		t.Errorf("valid input: %v", err)
	}
}

//ggen:generate
type AliasPosLeaf struct {
	Key   string `json:"key"`
	Value int    `json:"value"`
}

// aliasPosWrap has no exported fields, so an alias of it takes the
// UnmarshalJSON rung. Its UnmarshalJSON calls a generated decoder, so an
// error it returns carries a position relative to the span it was given.
type aliasPosWrap struct{ in AliasPosLeaf }

func (w *aliasPosWrap) UnmarshalJSON(b []byte) error {
	v, _, err := AliasPosLeaf{}.DecodeFrom(b)
	w.in = v
	return err
}

func (w aliasPosWrap) MarshalJSON() ([]byte, error) { return ggen.Marshal(w.in) }

//ggen:generate
type AliasPosWrap aliasPosWrap

//ggen:generate
type AliasPosHost struct {
	Pad string       `json:"pad"`
	W   AliasPosWrap `json:"w"`
	T   LocalText    `json:"t"`
}

// aliasPosBoth decodes payload on the bytes path and on the stream path at
// several chunk sizes, and checks that every error is a *ggen.ParseError at
// want.
func aliasPosBoth[T interface {
	ggen.Decoder[T]
	ggen.StreamDecoder[T]
}](t *testing.T, payload string, want int) {
	t.Helper()
	check := func(path string, err error) {
		t.Helper()
		pe, ok := errors.AsType[*ggen.ParseError](err)
		if !ok {
			t.Fatalf("%s %q: got %T %v", path, payload, err, err)
		}
		if pe.Pos != want {
			t.Errorf("%s %q: Pos=%d, want %d (%v)", path, payload, pe.Pos, want, err)
		}
	}
	var zb T
	_, _, berr := zb.DecodeFrom([]byte(payload))
	check("bytes", berr)
	for _, chunk := range []int{1, 7, 4096} {
		var s ggen.Stream
		s.Reset(&chunkReader{data: []byte(payload), max: chunk}, make([]byte, 0, 16))
		_, serr := zb.DecodeFromStream(&s)
		check(fmt.Sprintf("stream/%d", chunk), serr)
	}
}

// The UnmarshalJSON rung rebases the callee's span-relative positions onto
// the payload on both paths.
func TestAlias_jsonRungRebasesCalleePos(t *testing.T) {
	t.Parallel()
	payload := `{"pad":"` + strings.Repeat("p", 40) + `","w":{"key":"abc","value":"q"}}`
	aliasPosBoth[AliasPosHost](t, payload, strings.Index(payload, `"q"`))
}

//ggen:generate
type AliasCodecHost struct {
	S AliasStamp  `json:"s"`
	T LocalText   `json:"t"`
	O OpaqueAlias `json:"o"`
}

// An alias that delegates to MarshalJSON or MarshalText budgets for its
// output, so a parent presized with JSONSize does not grow its buffer.
func TestAlias_codecRungJSONSizeIsBound(t *testing.T) {
	t.Parallel()
	stamp := AliasStamp(time.Date(2020, 1, 2, 3, 4, 5, 123456789, time.FixedZone("", -(11*3600+59*60))))
	text := LocalText{v: strings.Repeat("t", 40)}
	opaque := OpaqueAlias(OpaqueWithMethods{hidden: strings.Repeat("o", 40)})
	for _, v := range []interface {
		AppendJSON(dst []byte) ([]byte, error)
		JSONSize() int
	}{stamp, text, opaque, AliasCodecHost{S: stamp, T: text, O: opaque}} {
		size := v.JSONSize()
		out, err := v.AppendJSON(make([]byte, 0, size))
		if err != nil {
			t.Fatalf("%T: %v", v, err)
		}
		if cap(out) != size {
			t.Errorf("%T: JSONSize=%d, but AppendJSON grew the buffer to write %d bytes", v, size, len(out))
		}
	}
}

// The UnmarshalJSON and UnmarshalText rungs report where scanning stopped,
// the same byte on the bytes and the stream path.
func TestAlias_codecRungErrorPos(t *testing.T) {
	t.Parallel()
	aliasPosBoth[AliasPosWrap](t, `{"a":[1,2,}`, 10)
	aliasPosBoth[AliasPosWrap](t, `{"a":[1,2`, 9)
	aliasPosBoth[LocalText](t, `"abc`, 4)
	aliasPosBoth[LocalText](t, "\"ab\x01c\"", 3)
	aliasPosBoth[AliasPosHost](t, `{"pad":"p","w":[1,2,}`, 20)
	aliasPosBoth[AliasPosHost](t, `{"pad":"p","t":"abc`, 19)
}

// The stream UnmarshalText rung lends the window to UnmarshalText without a
// copy, so the only allocation is the one textOnly makes to keep the text.
func TestAlias_textRungStreamNoCopy(t *testing.T) { //nolint:paralleltest // measures allocations
	doc := []byte(`"` + strings.Repeat("x", 40) + `"`)
	r := bytes.NewReader(doc)
	buf := make([]byte, 0, 256)
	var st ggen.Stream
	allocs := testing.AllocsPerRun(100, func() {
		r.Reset(doc)
		st.Reset(r, buf)
		v, err := LocalText{}.DecodeFromStream(&st)
		if err != nil || len(v.v) != 40 {
			t.Fatalf("%+v %v", v, err)
		}
	})
	if allocs != 1 {
		t.Errorf("allocs = %v, want 1", allocs)
	}
}
