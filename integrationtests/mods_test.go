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

// ModStruct exercises pipe mods. A mod is a transform that runs after the
// field decodes and before validation.
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

	// Without the prefix the value passes through unchanged.
	in2 := []byte(`{"sku":"XYZ"}`)
	got2, _, _ := ModStruct{}.DecodeFrom(in2)
	if got2.SKU != "XYZ" {
		t.Errorf("SKU = %q", got2.SKU)
	}
}

// FallibleModStruct pairs a fallible mod with validators that would also
// fail. The mod error stops decoding and is returned as a parse error.
//
//ggen:generate
type FallibleModStruct struct {
	Email string `json:"email" pipe:"required @RejectShort contains=@ minlen=10"`
}

// FallibleModMultierrStruct has the same fields in multierr mode. A fallible
// mod still returns a single parse error, not an aggregate.
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

// A fallible mod's error comes from user code, so it is wrapped like every
// other decode failure. The wrapper carries the field path and the payload
// offset and is reachable via errors.As, so the caller knows which field
// failed.
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

// A mod that passes does not skip validation. The validators after it still
// run.
func TestFallibleMod_passLetsValidationRun(t *testing.T) {
	t.Parallel()
	// "abcdef" passes RejectShort (len >= 3) but fails contains=@ and
	// minlen=10.
	_, _, err := FallibleModStruct{}.DecodeFrom([]byte(`{"email":"abcdef"}`))
	if err == nil {
		t.Fatal("expected validation error after mod passed")
	}
	if strings.Contains(err.Error(), "rejected by mod") {
		t.Errorf("mod ran on already-passing input: %v", err)
	}
}

// CrossPkgModStruct exercises @pkg.Func references to validators and mods in
// the sibling thirdparty package, one kind per field.
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

// NestedMultierrStruct wraps a multierr struct and adds its own rules. The
// outer struct must collect the inner validation errors into one aggregate,
// not stop at the first one.
//
//ggen:generate multierr
type NestedMultierrStruct struct {
	Inner FallibleModMultierrStruct `json:"inner"`
	Name  string                    `json:"name"  pipe:"required minlen=2"`
	Code  int                       `json:"code"  pipe:"gte=0 lte=100"`
}

func TestNestedMultierr_drainsInnerValidationErrors(t *testing.T) {
	t.Parallel()
	// "abcdef" passes the mod but fails the inner contains=@ and minlen=10.
	// The outer struct also fails on name (required, minlen=2) and on code
	// (lte=100). The inner struct returns a ggen.Errors aggregate that the
	// outer must merge into its own.
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
	// Inner errors get the outer field name prepended to their path, giving
	// ["inner","email"]. Outer errors keep a one-segment path.
	buckets := map[string]int{}
	for _, e := range leaves {
		buckets[strings.Join(pathOf(e), ".")]++
	}
	for _, want := range []string{"inner.email", "name", "code"} {
		if buckets[want] == 0 {
			t.Errorf("missing %q leaf (chaining/grouping broken): %v", want, leaves)
		}
	}
	// Merged inner errors carry offsets into the full payload. The nested
	// call site shifts each one with ggen.ShiftPos.
	var ce *ggen.ContainsError
	if !errors.As(err, &ce) {
		t.Fatalf("no ContainsError leaf: %v", leaves)
	}
	if want := bytes.Index(in, []byte(`"abcdef"`)) + len(`"abcdef"`); ce.Pos != want {
		t.Errorf("inner leaf Pos = %d, want %d (full-payload offset)", ce.Pos, want)
	}
}

// pathOf returns the Path of a validation error. It uses the pathSegments
// interface if the error has it, else a type switch over the error types this
// test uses.
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

// An inner parse error returns immediately, wrapped in *ggen.ParseError. It
// is not collected into the aggregate.
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

// QuotedParts checks quoted `|`-separated parts. Quotes wrap one part, so the
// part can hold a space or a literal pipe. The quotes themselves must not
// end up in the oneof values or the replace arguments. For example the
// allowed value is `New York`, not `'New York'`.
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

// CaseRules checks the islower and isupper validators. They reject text in the
// wrong case. Characters that have no case pass. The matching mods that change
// case are tolower and toupper.
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

// DrainInner is a non-multierr nested struct. It returns at its first
// validation failure, in the middle of its object. A multierr parent must not
// collect that error and keep parsing, because the cursor is no longer at the
// end of the inner value. The inner object's remaining keys would be read as
// the parent's keys (a bogus `unknown key "b"`). The parent collects and
// continues only when the callee is multierr itself, because then the callee
// consumes its whole value.
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
	// A single-error callee stops the parse cleanly on failure. No key of the
	// inner object may show up as an unknown key of the outer one.
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

	// A multierr callee consumes the whole value, so the parent still
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

// DrainFailFast is a single-error parent of a multierr callee. It stops in
// the middle of its own object, so it must hand up one error, not the
// callee's ggen.Errors: a bare Errors would tell Stream.Array the element was
// read whole.
//
//ggen:generate
type DrainFailFast struct {
	IME  DrainInnerME `json:"ime"`
	Name string       `json:"name"`
}

func TestMultierr_FailFastParent_HandsUpOneError(t *testing.T) {
	t.Parallel()
	in := `{"ime":{"a":"x","c":1},"name":"n"}`
	_, _, err := DrainFailFast{}.DecodeFrom([]byte(in))
	_, serr := ggen.NewStream(strings.NewReader(in), nil).Value[DrainFailFast]()
	for _, err := range []error{err, serr} {
		if _, ok := err.(ggen.Errors); ok {
			t.Fatalf("fail-fast parent returned ggen.Errors: %v", err)
		}
		me, ok := errors.AsType[*ggen.MinLenError](err)
		if !ok || strings.Join(me.Path, ".") != "ime.a" {
			t.Errorf("want MinLenError at ime.a, got %v", err)
		}
	}
}

func TestMultierr_StreamArray_GoesOnPastInvalidElement(t *testing.T) {
	t.Parallel()
	in := `[{"a":"x","b":"one"}, {"a":"long","b":"two"}, {"b":"three","c":1}]`
	var bs []string
	var fails []int
	for v, err := range ggen.NewStream(strings.NewReader(in), make([]byte, 0, 8)).Array[DrainInnerME]() {
		if err != nil {
			errs, ok := err.(ggen.Errors)
			if !ok {
				t.Fatalf("want ggen.Errors, got %T: %v", err, err)
			}
			fails = append(fails, len(errs))
		} else {
			fails = append(fails, 0)
		}
		bs = append(bs, v.B)
	}
	if !slices.Equal(bs, []string{"one", "two", "three"}) {
		t.Errorf("yielded values = %q", bs)
	}
	// the third has an unknown key and an empty a, which is not checked
	if !slices.Equal(fails, []int{1, 0, 1}) {
		t.Errorf("errors per element = %v, want [1 0 1]", fails)
	}
}

func TestMultierr_StreamSeq_GoesOnPastInvalidValue(t *testing.T) {
	t.Parallel()
	in := "{\"a\":\"x\",\"b\":\"one\"}\n{\"a\":\"long\",\"b\":\"two\"}\n"
	var bs []string
	var failed int
	for v, err := range ggen.NewStream(strings.NewReader(in), nil).Seq[DrainInnerME]() {
		if err != nil {
			failed++
		}
		bs = append(bs, v.B)
	}
	if !slices.Equal(bs, []string{"one", "two"}) || failed != 1 {
		t.Errorf("yielded %q with %d failed, want [one two] with 1", bs, failed)
	}
}

// An element whose parent stopped mid-object ends the range, rather than its
// tail being read as the next element.
func TestMultierr_StreamArray_StopsOnFailFastElement(t *testing.T) {
	t.Parallel()
	in := `[{"ime":{"a":"x"},"name":"n"}, {"name":"m"}]`
	var errs []error
	for _, err := range ggen.NewStream(strings.NewReader(in), nil).Array[DrainFailFast]() {
		errs = append(errs, err)
	}
	if len(errs) != 1 {
		t.Fatalf("yielded %d times, want 1: %v", len(errs), errs)
	}
	if _, ok := errors.AsType[*ggen.MinLenError](errs[0]); !ok {
		t.Errorf("want MinLenError, got %v", errs[0])
	}
}

// A top-level slice keeps every failure of the element it stopped on.
func TestMultierr_UnmarshalSlice_KeepsElementErrors(t *testing.T) {
	t.Parallel()
	in := `[{"a":"long"}, {"a":"x","c":1}]`
	_, err := ggen.UnmarshalSlice[DrainInnerME]([]byte(in))
	errs, ok := err.(ggen.Errors)
	if !ok || len(errs) != 2 {
		t.Fatalf("want 2 ggen.Errors, got %T: %v", err, err)
	}
	me, ok := errors.AsType[*ggen.MinLenError](err)
	if !ok || strings.Join(me.Path, ".") != "[1].a" {
		t.Errorf("want MinLenError at [1].a, got %v", err)
	}
	if want := strings.Index(in, `"x"`) + len(`"x"`); me.Pos != want {
		t.Errorf("Pos = %d, want %d (full-payload offset)", me.Pos, want)
	}
}

// R10PtrPipeOrder checks that pipe steps on a pointer field run in declared
// order, as they do on a value field. Mods must not all run before the
// validators. Otherwise `gte=0 clamp=0|5` would clamp -1 into range before
// the check, and `minlen=3 trim` would measure the trimmed text.
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

// R10BPtrPipeOrder checks a pointer field that mixes `@Func` steps with
// built-in ones. The whole pipeline runs in declared order, as it does on the
// value-typed twin. `@Func` steps take the field type (`*T`). Built-in steps
// take the value it points to.
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
	// multierr collects the result of the ordered steps instead of stopping
	// at the first failure.
	if _, _, err := (R10BPtrPipeOrderME{}).DecodeFrom([]byte(`{"c":1}`)); err != nil {
		t.Errorf("multierr: @zpAdd1 must run before gte=2: %v", err)
	}
	if _, _, err := (R10BPtrPipeOrderME{}).DecodeFrom([]byte(`{"c":0}`)); err == nil {
		t.Error("multierr: gte=2 must still reject 0+1")
	}
}

// R10BBigBounds checks numeric bounds above the range where float64 holds
// every integer exactly. The bound must be reported as written. The error's
// Limit, Of and Want carry the field's own kind, so nothing is rounded on the
// way into the message.
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
