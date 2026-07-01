package integrationtests

//go:generate ../ggen $GOFILE

// Top-level type aliases — each gets the full struct method surface.

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/validation"
	"github.com/sirkostya009/ggen/encode"
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
	Body  AliasString `json:"body" pipe:"required trim lower minlen=2 maxlen=10"`
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
