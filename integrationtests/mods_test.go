package integrationtests

//go:generate ../ggen $GOFILE

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen"
	_ "github.com/sirkostya009/ggen/integrationtests/thirdparty"
)

// ModStruct exercises pipe mods: transforms applied after decode, before
// validation.
//
//ggen:generate
type ModStruct struct {
	Email string   `json:"email" pipe:"trim tolower"`
	Tags  []string `json:"tags"  pipe:"inner:(trim tolower)"`
	SKU   string   `json:"sku"   pipe:"trimleft=SKU-"`
}

func TestMods_trimLowerEmail(t *testing.T) {
	t.Parallel()
	in := []byte(`{"email":"  Foo@Bar.COM  "}`)
	got, _, err := ModStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.Email != "foo@bar.com" {
		t.Errorf("Email = %q, want %q", got.Email, "foo@bar.com")
	}
}

func TestMods_diveTagTrim(t *testing.T) {
	t.Parallel()
	in := []byte(`{"tags":["  Go  ","  Rust  ","  C++  "]}`)
	got, _, err := ModStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	for i, want := range []string{"go", "rust", "c++"} {
		if got.Tags[i] != want {
			t.Errorf("Tags[%d] = %q, want %q", i, got.Tags[i], want)
		}
	}
}

func TestMods_trimleft(t *testing.T) {
	t.Parallel()
	in := []byte(`{"sku":"SKU-ABC123"}`)
	got, _, err := ModStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.SKU != "ABC123" {
		t.Errorf("SKU = %q", got.SKU)
	}

	// No prefix → pass-through.
	in2 := []byte(`{"sku":"XYZ"}`)
	got2, _, _ := ModStruct{}.DecodeFrom(in2)
	if got2.SKU != "XYZ" {
		t.Errorf("SKU = %q", got2.SKU)
	}
}

// FallibleModStruct pairs a fallible mod with validators that would also fail;
// the mod error short-circuits as a parse error.
//
//ggen:generate
type FallibleModStruct struct {
	Email string `json:"email" pipe:"required @RejectShort contains=@ minlen=10"`
}

// FallibleModMultierrStruct: same fields in multierr mode — a fallible mod
// still returns a single parse error, not aggregated.
//
//ggen:generate multierr
type FallibleModMultierrStruct struct {
	Email string `json:"email" pipe:"required @RejectShort contains=@ minlen=10"`
}

func RejectShort(s string) (string, error) {
	if len(s) < 3 {
		return "", errors.New("rejected by mod")
	}
	return s, nil
}

// A fallible mod's own error is foreign, so it gets wrapped like every other
// decode failure: field path + payload offset, reachable via errors.As. It
// used to propagate bare, leaving the caller no idea which field failed.
func TestFallibleMod_errorCarriesPathAndPos(t *testing.T) {
	t.Parallel()
	_, _, err := FallibleModStruct{}.DecodeFrom([]byte(`{"email":"ab"}`))
	var pe *ggen.ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("got %T %v, want *ggen.ParseError", err, err)
	}
	if len(pe.Path) == 0 || pe.Path[0] != "email" {
		t.Errorf("Path = %v, want [email]", pe.Path)
	}
	if pe.Pos <= 0 {
		t.Errorf("Pos = %d, want a real offset", pe.Pos)
	}
	if !strings.Contains(err.Error(), "rejected by mod") {
		t.Errorf("wrapping lost the mod message: %v", err)
	}
}

// The mod runs first, so its error surfaces ahead of the contains/minlen
// validators that would also fail.
func TestFallibleMod_shortCircuitsValidation(t *testing.T) {
	t.Parallel()
	_, _, err := FallibleModStruct{}.DecodeFrom([]byte(`{"email":"x"}`))
	if err == nil {
		t.Fatal("expected mod rejection")
	}
	if !strings.Contains(err.Error(), "rejected by mod") {
		t.Errorf("expected mod error, got: %v", err)
	}
	// Validation messages must not appear.
	if strings.Contains(err.Error(), "does not contain") || strings.Contains(err.Error(), "below minimum") {
		t.Errorf("validation ran despite mod failure: %v", err)
	}
}

// In multierr mode a fallible mod still returns a single parse error, not
// ggen.Errors.
func TestFallibleMod_multierrStillShortCircuits(t *testing.T) {
	t.Parallel()
	_, _, err := FallibleModMultierrStruct{}.DecodeFrom([]byte(`{"email":"x"}`))
	if err == nil {
		t.Fatal("expected mod rejection in multierr mode")
	}
	if _, ok := err.(ggen.Errors); ok {
		t.Errorf("mod error wrapped into ggen.Errors; should be a plain parse error: %v", err)
	}
	if !strings.Contains(err.Error(), "rejected by mod") {
		t.Errorf("expected mod error in multierr mode, got: %v", err)
	}
}

// A passing mod is a gate, not a bypass — downstream validation still runs.
func TestFallibleMod_passLetsValidationRun(t *testing.T) {
	t.Parallel()
	// "abcdef" passes RejectShort (len >= 3) but fails contains=@ + minlen=10.
	_, _, err := FallibleModStruct{}.DecodeFrom([]byte(`{"email":"abcdef"}`))
	if err == nil {
		t.Fatal("expected validation error after mod passed")
	}
	if strings.Contains(err.Error(), "rejected by mod") {
		t.Errorf("mod ran on already-passing input: %v", err)
	}
}

// CrossPkgModStruct exercises @pkg.Func resolution for validators and mods
// from the sibling thirdparty package, one flavor per field.
//
//ggen:generate
type CrossPkgModStruct struct {
	// Pure mod: prefixes with '#'.
	Tag string `json:"tag" pipe:"@thirdparty.PrefixHash"`
	// Fallible mod: empty input is a parse error.
	NonEmpty string `json:"nonEmpty" pipe:"@thirdparty.ParseNonEmpty"`
	// Validator: all-uppercase ASCII.
	Code string `json:"code" pipe:"@thirdparty.ValidateUpper"`
}

func TestCrossPkgMod_pureMod(t *testing.T) {
	t.Parallel()
	got, _, err := CrossPkgModStruct{}.DecodeFrom([]byte(`{"tag":"x","nonEmpty":"y","code":"OK"}`))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Tag != "#x" {
		t.Errorf("Tag = %q, want #x (mod prefix not applied)", got.Tag)
	}
}

func TestCrossPkgMod_fallibleModRejects(t *testing.T) {
	t.Parallel()
	_, _, err := CrossPkgModStruct{}.DecodeFrom([]byte(`{"tag":"x","nonEmpty":"","code":"OK"}`))
	if err == nil {
		t.Fatal("expected fallible-mod rejection")
	}
	if !strings.Contains(err.Error(), "empty value") {
		t.Errorf("expected cross-pkg mod error, got: %v", err)
	}
}

func TestCrossPkgValidator_rejects(t *testing.T) {
	t.Parallel()
	_, _, err := CrossPkgModStruct{}.DecodeFrom([]byte(`{"tag":"x","nonEmpty":"y","code":"lowercase"}`))
	if err == nil {
		t.Fatal("expected cross-pkg validator rejection")
	}
	if !strings.Contains(err.Error(), "must be uppercase") {
		t.Errorf("expected cross-pkg validator error, got: %v", err)
	}
}

func TestCrossPkgValidator_accepts(t *testing.T) {
	t.Parallel()
	got, _, err := CrossPkgModStruct{}.DecodeFrom([]byte(`{"tag":"x","nonEmpty":"y","code":"OK"}`))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Code != "OK" {
		t.Errorf("Code = %q", got.Code)
	}
}

// NestedMultierrStruct wraps a multierr struct plus its own rules — the
// outer must drain inner validation errors into one aggregate, not
// short-circuit.
//
//ggen:generate multierr
type NestedMultierrStruct struct {
	Inner FallibleModMultierrStruct `json:"inner"`
	Name  string                    `json:"name"  pipe:"required minlen=2"`
	Code  int                       `json:"code"  pipe:"gte=0 lte=100"`
}

func TestNestedMultierr_drainsInnerValidationErrors(t *testing.T) {
	t.Parallel()
	// "abcdef" passes the mod but fails inner contains=@ + minlen=10; outer also
	// fails on name (required/minlen=2) and code (lte=100). Inner returns a
	// ggen.Errors aggregate the outer must drain.
	in := []byte(`{"inner":{"email":"abcdef"},"name":"","code":200}`)
	_, _, err := NestedMultierrStruct{}.DecodeFrom(in)
	if err == nil {
		t.Fatal("expected aggregated errors")
	}
	leaves, ok := err.(ggen.Errors)
	if !ok {
		t.Fatalf("err = %T, want ggen.Errors; got: %v", err, err)
	}
	if len(leaves) < 4 {
		t.Errorf("got %d leaves, want >= 4 (inner+outer combined): %v", len(leaves), leaves)
	}
	// Inner leaves get the outer field prepended (Path ["inner","email"]);
	// outer leaves stay one segment deep.
	buckets := map[string]int{}
	for _, e := range leaves {
		buckets[strings.Join(pathOf(e), ".")]++
	}
	for _, want := range []string{"inner.email", "name", "code"} {
		if buckets[want] == 0 {
			t.Errorf("missing %q leaf (chaining/grouping broken): %v", want, leaves)
		}
	}
	// Drained inner leaves carry FULL-payload offsets — the multierr drain
	// rebases each via ggen.ShiftPos at the nested call site.
	var ce *ggen.ContainsError
	if !errors.As(err, &ce) {
		t.Fatalf("no ContainsError leaf: %v", leaves)
	}
	if want := bytes.Index(in, []byte(`"abcdef"`)) + len(`"abcdef"`); ce.Pos != want {
		t.Errorf("inner leaf Pos = %d, want %d (full-payload offset)", ce.Pos, want)
	}
}

// pathOf reads the Path slice off a validation error, via the pathSegments
// interface if present, else a type switch over the leaf types this test uses.
func pathOf(e ggen.Error) []string {
	type pathed interface{ pathSegments() []string }
	if p, ok := e.(pathed); ok {
		return p.pathSegments()
	}
	switch v := e.(type) {
	case *ggen.ContainsError:
		return v.Path
	case *ggen.MinLenError:
		return v.Path
	case *ggen.LTEError:
		return v.Path
	case *ggen.GTEError:
		return v.Path
	case *ggen.RequiredError:
		return v.Path
	}
	return nil
}

// An inner parse error returns immediately (not drained), wrapped in
// *ggen.ParseError.
func TestNestedMultierr_innerParseErrorReturnsEarly(t *testing.T) {
	t.Parallel()
	// Inner email is a number, not a string.
	in := []byte(`{"inner":{"email":123},"name":"valid","code":50}`)
	_, _, err := NestedMultierrStruct{}.DecodeFrom(in)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if _, ok := err.(ggen.Errors); ok {
		t.Errorf("parse error wrapped in ggen.Errors: %v", err)
	}
}

// Quoted `|`-parts: quotes scope one part (space or literal pipe inside),
// and the quote layer must NOT leak into the allowed set / replace args —
// a whole-value strip + naive split used to emit `'New York'` as the case.
//
//ggen:generate
type QuotedParts struct {
	City string `json:"city" pipe:"oneof='New York'|'Los Angeles'|LA"`
	Note string `json:"note" pipe:"replace='a|b'|AB"`
}

func TestQuotedPipeParts(t *testing.T) {
	t.Parallel()
	v, _, err := QuotedParts{}.DecodeFrom([]byte(`{"city":"New York","note":"xa|by"}`))
	if err != nil {
		t.Fatalf("quoted-space part must be allowed: %v", err)
	}
	if v.Note != "xABy" {
		t.Errorf("replace 'a|b' -> AB: got %q", v.Note)
	}
	if _, _, err = (QuotedParts{}).DecodeFrom([]byte(`{"city":"'New York'","note":""}`)); err == nil {
		t.Error("literal-quote value must be rejected (quote leak into allowed set)")
	}
	var oe *ggen.OneOfError
	_, _, err = QuotedParts{}.DecodeFrom([]byte(`{"city":"Boston","note":""}`))
	if !errors.As(err, &oe) {
		t.Fatalf("want OneOfError, got %v", err)
	}
	if want := []string{"New York", "Los Angeles", "LA"}; !slices.Equal(oe.Allowed, want) {
		t.Errorf("Allowed = %q, want %q", oe.Allowed, want)
	}
}

// islower/isupper VALIDATORS (split from the old bare lower/upper, which are
// now tolower/toupper mods): reject wrong-case content, caseless runes pass.
//
//ggen:generate
type CaseRules struct {
	Slug string `json:"slug" pipe:"islower"`
	Code string `json:"code" pipe:"isupper"`
}

func TestCaseValidators(t *testing.T) {
	t.Parallel()
	got, _, err := CaseRules{}.DecodeFrom([]byte(`{"slug":"abc-123","code":"ABC123"}`))
	if err != nil || got.Slug != "abc-123" {
		t.Fatalf("caseless+correct case must pass: %+v %v", got, err)
	}
	var le *ggen.LowerError
	_, _, err = CaseRules{}.DecodeFrom([]byte(`{"slug":"aBc","code":"OK"}`))
	if !errors.As(err, &le) || le.Value != "aBc" {
		t.Errorf("want LowerError{aBc}, got %v", err)
	}
	var ue *ggen.UpperError
	_, _, err = CaseRules{}.DecodeFrom([]byte(`{"slug":"ok","code":"AbC"}`))
	if !errors.As(err, &ue) || ue.Value != "AbC" {
		t.Errorf("want UpperError{AbC}, got %v", err)
	}
}

// A NON-multierr nested struct returns at its first validation failure —
// mid-value. The parent's multierr drain used to append that error and keep
// parsing from the desynced cursor, so the inner object's remaining keys
// were read as the PARENT's (a bogus `unknown key "b"`) and the parent's own
// later failures vanished. The drain is now gated on the callee actually
// consuming the value (i.e. being multierr itself).
//
//ggen:generate
type DrainInner struct {
	A string `json:"a" pipe:"minlen=3"`
	B string `json:"b"`
}

//ggen:generate multierr
type DrainInnerME struct {
	A string `json:"a" pipe:"minlen=3"`
	B string `json:"b"`
}

//ggen:generate multierr
type DrainOuter struct {
	I    DrainInner   `json:"i"`
	IME  DrainInnerME `json:"ime"`
	Name string       `json:"name" pipe:"minlen=2"`
	Tail int          `json:"tail" pipe:"lte=10"`
}

func TestMultierr_NoCursorDesyncOnSingleErrorCallee(t *testing.T) {
	t.Parallel()
	// Single-error callee: its failure stops the parse cleanly. No key of the
	// INNER object may surface as an unknown key of the OUTER.
	_, _, err := DrainOuter{}.DecodeFrom([]byte(`{"i":{"a":"x","b":"bee"},"name":"","tail":99}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	if msg := err.Error(); strings.Contains(msg, "unknown key") ||
		strings.Contains(msg, "invalid object") || strings.Contains(msg, "invalid array") {
		t.Errorf("cursor desync artifact: %v", msg)
	}
	if _, ok := errors.AsType[*ggen.MinLenError](err); !ok {
		t.Errorf("inner failure not reported: %v", err)
	}

	// A multierr callee DOES consume the whole value, so the parent still
	// collects its own field failures alongside the inner one.
	_, _, err = DrainOuter{}.DecodeFrom([]byte(`{"ime":{"a":"x","b":"bee"},"name":"","tail":99}`))
	errs, ok := err.(ggen.Errors)
	if !ok {
		t.Fatalf("want ggen.Errors, got %T: %v", err, err)
	}
	if len(errs) < 3 {
		t.Errorf("got %d leaves, want >= 3 (ime.a + name + tail): %v", len(errs), errs)
	}
}

// R10PtrPipeOrder: value steps on a POINTER field run in declared order like
// on a value field. The deref'd leaf used to run every mod before every
// validator, so `gte=0 clamp=0|5` clamped -1 into range and `minlen=3 trim`
// measured the trimmed text.
//
//ggen:generate
type R10PtrPipeOrder struct {
	V  int     `json:"v"  pipe:"gte=0 clamp=0|5"`
	P  *int    `json:"p"  pipe:"gte=0 clamp=0|5"`
	Cp *int    `json:"cp" pipe:"clamp=0|5 gte=1"`
	Q  string  `json:"q"  pipe:"minlen=3 trim"`
	Qp *string `json:"qp" pipe:"minlen=3 trim"`
}

//ggen:generate multierr
type R10PtrPipeOrderME struct {
	P *int `json:"p" pipe:"gte=0 clamp=0|5"`
}

func TestMods_pointerPipeDeclaredOrder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		ok   bool
		want func(v R10PtrPipeOrder) bool
	}{
		{`{"v":-1}`, false, nil},
		{`{"p":-1}`, false, nil},
		{`{"p":7}`, true, func(v R10PtrPipeOrder) bool { return *v.P == 5 }},
		{`{"cp":-1}`, false, nil},
		{`{"cp":3}`, true, func(v R10PtrPipeOrder) bool { return *v.Cp == 3 }},
		{`{"q":"  a  "}`, true, func(v R10PtrPipeOrder) bool { return v.Q == "a" }},
		{`{"qp":"  a  "}`, true, func(v R10PtrPipeOrder) bool { return *v.Qp == "a" }},
		{`{"qp":"ab"}`, false, nil},
	}
	for _, c := range cases {
		got, _, err := R10PtrPipeOrder{}.DecodeFrom([]byte(c.in))
		if (err == nil) != c.ok || (c.want != nil && err == nil && !c.want(got)) {
			t.Errorf("bytes %s: %+v (%v), want ok=%v", c.in, got, err, c.ok)
		}
		sgot, err := ggen.NewStream(strings.NewReader(c.in), nil).Value[R10PtrPipeOrder]()
		if (err == nil) != c.ok || (c.want != nil && err == nil && !c.want(sgot)) {
			t.Errorf("stream %s: %+v (%v), want ok=%v", c.in, sgot, err, c.ok)
		}
	}
	if _, _, err := (R10PtrPipeOrderME{}).DecodeFrom([]byte(`{"p":-1}`)); err == nil {
		t.Error("multierr: gte=0 bypassed by the later clamp")
	}
	if _, err := ggen.NewStream(strings.NewReader(`{"p":-1}`), nil).Value[R10PtrPipeOrderME](); err == nil {
		t.Error("multierr stream: gte=0 bypassed by the later clamp")
	}
}

// R10BPtrPipeOrder: a POINTER field interleaving `@Func` steps with built-in
// ones runs the whole pipeline in declared order, exactly as the value-typed
// twin does. `@Func` steps take the field type (`*T`); built-ins the leaf.
//
//ggen:generate
type R10BPtrPipeOrder struct {
	C *int    `json:"c" pipe:"@zpAdd1 gte=2"`
	E *string `json:"e" pipe:"@zpUpper oneof=AB"`
	F *int    `json:"f" pipe:"gte=2 @zpAdd1"`
}

//ggen:generate multierr
type R10BPtrPipeOrderME struct {
	C *int `json:"c" pipe:"@zpAdd1 gte=2"`
}

//ggen:generate
type R10BValPipeOrder struct {
	C int    `json:"c" pipe:"@zpAdd1V gte=2"`
	E string `json:"e" pipe:"@zpUpperV oneof=AB"`
	F int    `json:"f" pipe:"gte=2 @zpAdd1V"`
}

func zpAdd1(p *int) *int {
	if p == nil {
		return nil
	}
	n := *p + 1
	return &n
}

func zpUpper(p *string) *string {
	if p == nil {
		return nil
	}
	s := strings.ToUpper(*p)
	return &s
}

func zpAdd1V(n int) int { return n + 1 }

func zpUpperV(s string) string { return strings.ToUpper(s) }

func TestMods_pointerCustomStepDeclaredOrder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		ok   bool
		want func(p R10BPtrPipeOrder) bool
	}{
		{`{"c":1}`, true, func(p R10BPtrPipeOrder) bool { return *p.C == 2 }},
		{`{"c":0}`, false, nil},
		{`{"e":"ab"}`, true, func(p R10BPtrPipeOrder) bool { return *p.E == "AB" }},
		{`{"f":2}`, true, func(p R10BPtrPipeOrder) bool { return *p.F == 3 }},
		{`{"f":1}`, false, nil},
		{`{"c":null,"e":null,"f":null}`, true, func(p R10BPtrPipeOrder) bool { return p.C == nil && p.E == nil }},
	}
	for _, c := range cases {
		got, _, err := R10BPtrPipeOrder{}.DecodeFrom([]byte(c.in))
		if (err == nil) != c.ok || (c.want != nil && err == nil && !c.want(got)) {
			t.Errorf("bytes %s: %+v (%v), want ok=%v", c.in, got, err, c.ok)
		}
		sgot, err := ggen.NewStream(strings.NewReader(c.in), nil).Value[R10BPtrPipeOrder]()
		if (err == nil) != c.ok || (c.want != nil && err == nil && !c.want(sgot)) {
			t.Errorf("stream %s: %+v (%v), want ok=%v", c.in, sgot, err, c.ok)
		}
		// The value-typed twin must reach the same verdict.
		if !strings.Contains(c.in, "null") {
			vgot, _, verr := R10BValPipeOrder{}.DecodeFrom([]byte(c.in))
			if (verr == nil) != c.ok {
				t.Errorf("value %s: %+v (%v), want ok=%v", c.in, vgot, verr, c.ok)
			}
		}
	}
	// multierr collects the ordered verdict rather than short-circuiting.
	if _, _, err := (R10BPtrPipeOrderME{}).DecodeFrom([]byte(`{"c":1}`)); err != nil {
		t.Errorf("multierr: @zpAdd1 must run before gte=2: %v", err)
	}
	if _, _, err := (R10BPtrPipeOrderME{}).DecodeFrom([]byte(`{"c":0}`)); err == nil {
		t.Error("multierr: gte=2 must still reject 0+1")
	}
}

// A numeric bound above float64's exact integer range must be reported as
// written: the error's Limit/Of/Want carry the field's own kind, so nothing
// is rounded on the way into the message.
//
//ggen:generate
type R10BBigBounds struct {
	A uint64 `json:"a" pipe:"gte=9223372036854775809"`
	B uint64 `json:"b" pipe:"multiple=9223372036854775809"`
	C uint64 `json:"c" pipe:"eq=18446744073709551615"`
}

func TestBigUint64Bounds_reportedExactly(t *testing.T) {
	t.Parallel()
	const big = uint64(9223372036854775809)
	_, _, err := R10BBigBounds{}.DecodeFrom([]byte(`{"a":1,"b":1,"c":1}`))
	var ge *ggen.GTEError
	if !errors.As(err, &ge) {
		t.Fatalf("no GTEError: %v", err)
	}
	if ge.Limit != big || !strings.Contains(ge.Error(), "9223372036854775809") {
		t.Errorf("GTEError.Limit = %v (%T), message %q", ge.Limit, ge.Limit, ge)
	}

	_, _, err = R10BBigBounds{}.DecodeFrom([]byte(`{"a":9223372036854775809,"b":1,"c":1}`))
	var me *ggen.MultipleError
	if !errors.As(err, &me) {
		t.Fatalf("no MultipleError: %v", err)
	}
	if me.Of != big || !strings.Contains(me.Error(), "9223372036854775809") {
		t.Errorf("MultipleError.Of = %v (%T), message %q", me.Of, me.Of, me)
	}

	_, _, err = R10BBigBounds{}.DecodeFrom([]byte(`{"a":9223372036854775809,"b":0,"c":1}`))
	var ee *ggen.EqError
	if !errors.As(err, &ee) {
		t.Fatalf("no EqError: %v", err)
	}
	if ee.Want != uint64(18446744073709551615) {
		t.Errorf("EqError.Want = %v (%T)", ee.Want, ee.Want)
	}
}
