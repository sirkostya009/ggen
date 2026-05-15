package integrationtests

// Top-level primitive aliases — `type X <primitive>` annotated with
// //ggen:generate. Each alias gets the same method surface as a struct
// (DecodeFrom / DecodeStreamFrom / JSONSize / AppendJSON) so it can be
// fed to decode.Unmarshal / encode.Marshal directly.

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/decode"
	"github.com/sirkostya009/ggen/decode/validation"
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
	in := AliasString("hello")
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"hello"` {
		t.Errorf("marshal = %q, want %q", out, `"hello"`)
	}
	got, err := decode.Unmarshal[AliasString](out)
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Errorf("roundtrip = %q, want %q", got, in)
	}
}

// TestAlias_String_DefaultIsLiteral: default mode emits `<>&` literally —
// ggen's wire shape matches encoding/json/v2 (which dropped HTML
// escaping as a default).
func TestAlias_String_DefaultIsLiteral(t *testing.T) {
	out, _ := encode.Marshal(AliasString(`<a href="x">tom & jerry</a>`))
	for _, lit := range []string{"<", ">", "&"} {
		if !strings.Contains(string(out), lit) {
			t.Errorf("default mode escaped %q in alias output: %s", lit, out)
		}
	}
}

// TestAlias_String_HtmlescapeOptIn: the `htmlescape` annotation on a
// string alias flips the codegen to the v1-style \uXXXX escaper.
func TestAlias_String_HtmlescapeOptIn(t *testing.T) {
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
	in := AliasInt(-42)
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "-42" {
		t.Errorf("marshal = %q, want -42", out)
	}
	got, err := decode.Unmarshal[AliasInt]([]byte("-42"))
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Errorf("roundtrip = %d, want -42", got)
	}
}

func TestAlias_Uint64_Roundtrip(t *testing.T) {
	in := AliasUint64(18446744073709551615) // max uint64
	out, _ := encode.Marshal(in)
	got, err := decode.Unmarshal[AliasUint64](out)
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Errorf("roundtrip = %d, want %d", got, in)
	}
}

func TestAlias_Float64_Roundtrip(t *testing.T) {
	in := AliasFloat64(3.14159)
	out, _ := encode.Marshal(in)
	got, err := decode.Unmarshal[AliasFloat64](out)
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Errorf("roundtrip = %v, want %v", got, in)
	}
}

func TestAlias_Bool_Roundtrip(t *testing.T) {
	for _, in := range []AliasBool{true, false} {
		out, _ := encode.Marshal(in)
		got, err := decode.Unmarshal[AliasBool](out)
		if err != nil {
			t.Fatalf("%v: %v", in, err)
		}
		if got != in {
			t.Errorf("roundtrip = %v, want %v", got, in)
		}
	}
}

// TestAlias_String_RejectsNonString: feeding a JSON number to a string
// alias must surface scan.ErrBadString, not a silent type coercion.
func TestAlias_String_RejectsNonString(t *testing.T) {
	if _, err := decode.Unmarshal[AliasString]([]byte("42")); err == nil {
		t.Error("expected scan error on number → string-alias")
	}
}

// TestAlias_String_ZeroCopy verifies that converting a scanned string to
// the alias type doesn't allocate a fresh string — the named-type cast
// is a label change at compile time, not a memcpy. We mutate the input
// buffer after decoding; if the alias still points into it, the decoded
// value reflects the mutation. (DON'T do this in production code — it's
// the documented hazard zero-copy aliasing buys you.)
func TestAlias_String_ZeroCopy(t *testing.T) {
	in := []byte(`"alpha"`)
	got, err := decode.Unmarshal[AliasString](in)
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

// PlainInner has NO Marshal/Unmarshal methods — ggen falls into the
// field-introspection path: walk the underlying *types.Struct, synth
// FieldInfo per exported field, treat the alias as a regular struct.
type PlainInner struct {
	Title string `json:"title"`
	Count int    `json:"count"`
}

//ggen:generate
type PlainAlias PlainInner

// TestAlias_StructIntrospect_NoMethods: alias of a method-less struct.
// ggen synthesizes fields from the underlying *types.Struct and emits
// regular per-field encode/decode. The wire shape is the natural JSON
// object — no delegation involved.
func TestAlias_StructIntrospect_NoMethods(t *testing.T) {
	in := PlainAlias{Title: "hello", Count: 42}
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"count":42,"title":"hello"}` {
		t.Errorf("marshal = %s, want struct-shaped JSON", out)
	}
	got, err := decode.Unmarshal[PlainAlias](out)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "hello" || got.Count != 42 {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

// SamePkgInner is the underlying for SamePkgAlias — defined in the
// SAME package as the alias. Has MarshalJSON / UnmarshalJSON, but ggen
// IGNORES those when the struct has exported fields and instead emits
// a hand-rolled decoder/encoder driven off field introspection (faster
// than method delegation for plain structs).
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

// CrossPkgTaggedAlias names a type whose underlying lives in another
// package — `thirdparty.Tagged`. Underlying has MarshalText/UnmarshalText
// AND exported fields; ggen prefers introspection over text delegation
// because hand-rolled struct codegen is faster than text-method calls.
//
//ggen:generate
type CrossPkgTaggedAlias thirdparty.Tagged

// TestAlias_StructIntrospect_CrossPkg: thirdparty.Tagged has exported
// fields, so introspection wins over its TextMarshaler/TextUnmarshaler.
// Wire shape is the field-driven JSON object, NOT the canonical
// "name#tag" text form.
func TestAlias_StructIntrospect_CrossPkg(t *testing.T) {
	in := CrossPkgTaggedAlias(thirdparty.Tagged{Name: "alice", Tag: "admin"})
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"Name":"alice","Tag":"admin"}` {
		t.Errorf("marshal = %s, want field-introspected JSON object", out)
	}
	got, err := decode.Unmarshal[CrossPkgTaggedAlias](out)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "alice" || got.Tag != "admin" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

// TestAlias_StructIntrospect_SamePkg: SamePkgInner has exported fields,
// so the alias introspects them rather than delegating to its
// MarshalJSON/UnmarshalJSON. Wire shape is the introspected object
// (`{"X":...,"Y":...}` — uppercase, since the underlying has no json tags).
func TestAlias_StructIntrospect_SamePkg(t *testing.T) {
	in := SamePkgAlias(SamePkgInner{X: 42, Y: "hello"})
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"X":42,"Y":"hello"}` {
		t.Errorf("marshal = %s, want field-introspected JSON object", out)
	}
	got, err := decode.Unmarshal[SamePkgAlias](out)
	if err != nil {
		t.Fatal(err)
	}
	if got.X != 42 || got.Y != "hello" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

// OpaqueWithMethods has no exported fields but does carry JSON
// methods — mirrors the time.Time / opaque-thirdparty shape. Aliases
// of such types fall back to JSON/Text delegation (the only viable
// path) since introspection would yield an empty struct.
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

// TestAlias_StructDelegation_OpaqueFallback: when the underlying has
// no exported fields, ggen falls back to JSON/Text method delegation —
// introspection would generate nothing useful.
func TestAlias_StructDelegation_OpaqueFallback(t *testing.T) {
	in := OpaqueAlias(OpaqueWithMethods{hidden: "secret"})
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"secret"` {
		t.Errorf("marshal = %s, want delegated MarshalJSON output", out)
	}
	got, err := decode.Unmarshal[OpaqueAlias](out)
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

// TestAlias_Slice_Roundtrip: slice alias roundtrips through the same
// codegen path as a regular slice field — JSON-array wire shape, no
// special wrapping.
func TestAlias_Slice_Roundtrip(t *testing.T) {
	in := AliasTags{"go", "rust", "zig"}
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `["go","rust","zig"]` {
		t.Errorf("marshal = %s, want JSON array", out)
	}
	got, err := decode.Unmarshal[AliasTags](out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "go" || got[1] != "rust" || got[2] != "zig" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

// TestAlias_Map_Roundtrip: map alias — JSON object wire shape with
// string keys.
func TestAlias_Map_Roundtrip(t *testing.T) {
	in := AliasLookup{"alpha": 1, "beta": 2}
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decode.Unmarshal[AliasLookup](out)
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if len(got) != 2 || got["alpha"] != 1 || got["beta"] != 2 {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

// TestAlias_Array_Roundtrip: array alias — JSON tuple with strict
// element count enforced on decode.
func TestAlias_Array_Roundtrip(t *testing.T) {
	in := AliasTuple{10, 20, 30}
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `[10,20,30]` {
		t.Errorf("marshal = %s, want [10,20,30]", out)
	}
	got, err := decode.Unmarshal[AliasTuple](out)
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Errorf("roundtrip = %v, want %v", got, in)
	}
}

// TestAlias_Array_StrictLen: decoding into a fixed-length array alias
// errors when the JSON tuple has the wrong element count — same
// validation.LenError as the field-level array path.
func TestAlias_Array_StrictLen(t *testing.T) {
	if _, err := decode.Unmarshal[AliasTuple]([]byte(`[1,2]`)); err == nil {
		t.Error("expected LenError on short tuple")
	}
	if _, err := decode.Unmarshal[AliasTuple]([]byte(`[1,2,3,4]`)); err == nil {
		t.Error("expected LenError on long tuple")
	}
}

// TestAlias_String_ZeroAllocations measures that decoding a string-alias
// from a byte slice with no escape sequences does ZERO allocations —
// confirming the scan path's unsafe.String alias plus the named-type
// cast are both alloc-free.
func TestAlias_String_ZeroAllocations(t *testing.T) {
	in := []byte(`"some-typical-html-payload-here"`)
	allocs := testing.AllocsPerRun(100, func() {
		v, err := decode.Unmarshal[AliasString](in)
		if err != nil {
			t.Fatal(err)
		}
		// Use the value so the compiler can't elide the call.
		if len(v) == 0 {
			t.Fatal("empty")
		}
	})
	if allocs != 0 {
		t.Errorf("expected 0 allocs for string-alias decode, got %v", allocs)
	}
}

// AliasFieldExample exercises ggen validation rules and mod transforms
// against fields whose declared types are top-level primitive aliases.
// The codegen casts mod calls through the underlying primitive
// (`AliasString` ↔ `string`, `AliasInt` ↔ `int`) so stdlib helpers
// (`strings.TrimSpace`, comparison operators) accept the value.
//
//ggen:generate
type AliasFieldExample struct {
	Body  AliasString `json:"body" ggen:"required,minlen=2,maxlen=10" mod:"trim,lower"`
	Count AliasInt    `json:"count" ggen:"gte=1,lte=100" mod:"clamp=1|100"`
}

func TestAlias_Field_ValidationAndMods(t *testing.T) {
	// trim + lower run before validation; the value the validator sees
	// is the post-mod one.
	got, err := decode.Unmarshal[AliasFieldExample]([]byte(`{"body":"  HI  ","count":5}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(got.Body) != "hi" {
		t.Errorf("trim+lower on AliasString: got %q, want %q", got.Body, "hi")
	}

	// clamp pulls 500 back into [1,100]; gte/lte then pass on the clamped value.
	got, err = decode.Unmarshal[AliasFieldExample]([]byte(`{"body":"hello","count":500}`))
	if err != nil {
		t.Fatalf("clamp+validate: %v", err)
	}
	if int(got.Count) != 100 {
		t.Errorf("clamp on AliasInt: got %d, want 100", got.Count)
	}

	// minlen fires post-trim — `" a "` → `"a"` → length 1, below the limit.
	_, err = decode.Unmarshal[AliasFieldExample]([]byte(`{"body":" a ","count":5}`))
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
