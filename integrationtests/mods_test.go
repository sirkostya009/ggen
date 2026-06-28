package integrationtests

//go:generate ../ggen $GOFILE

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/decode/validation"
	_ "github.com/sirkostya009/ggen/integrationtests/thirdparty"
)

// ModStruct exercises pipe mods: transforms applied after decode, before
// validation.
//
//ggen:generate
type ModStruct struct {
	Email string   `json:"email" pipe:"trim lower email"`
	Tags  []string `json:"tags" pipe:"inner:(trim lower)"`
	SKU   string   `json:"sku" pipe:"trimleft=SKU-"`
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
	Email string `json:"email" pipe:"required @RejectShort email minlen=10"`
}

// FallibleModMultierrStruct: same fields in multierr mode — a fallible mod
// still returns a single parse error, not aggregated.
//
//ggen:generate multierr
type FallibleModMultierrStruct struct {
	Email string `json:"email" pipe:"required @RejectShort email minlen=10"`
}

func RejectShort(s string) (string, error) {
	if len(s) < 3 {
		return "", fmt.Errorf("rejected by mod")
	}
	return s, nil
}

// The mod runs first, so its error surfaces ahead of the email/minlen
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
	if strings.Contains(err.Error(), "valid email") || strings.Contains(err.Error(), "below minimum") {
		t.Errorf("validation ran despite mod failure: %v", err)
	}
}

// In multierr mode a fallible mod still returns a single parse error, not
// validation.Errors.
func TestFallibleMod_multierrStillShortCircuits(t *testing.T) {
	t.Parallel()
	_, _, err := FallibleModMultierrStruct{}.DecodeFrom([]byte(`{"email":"x"}`))
	if err == nil {
		t.Fatal("expected mod rejection in multierr mode")
	}
	if _, ok := err.(validation.Errors); ok {
		t.Errorf("mod error wrapped into validation.Errors; should be a plain parse error: %v", err)
	}
	if !strings.Contains(err.Error(), "rejected by mod") {
		t.Errorf("expected mod error in multierr mode, got: %v", err)
	}
}

// A passing mod is a gate, not a bypass — downstream validation still runs.
func TestFallibleMod_passLetsValidationRun(t *testing.T) {
	t.Parallel()
	// "abcdef" passes RejectShort (len >= 3) but fails minlen=10 + email.
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
	Name  string                    `json:"name" pipe:"required minlen=2"`
	Code  int                       `json:"code" pipe:"gte=0 lte=100"`
}

func TestNestedMultierr_drainsInnerValidationErrors(t *testing.T) {
	t.Parallel()
	// "abcdef" passes the mod but fails inner minlen=10 + email; outer also
	// fails on name (required/minlen=2) and code (lte=100). Inner returns a
	// validation.Errors aggregate the outer must drain.
	in := []byte(`{"inner":{"email":"abcdef"},"name":"","code":200}`)
	_, _, err := NestedMultierrStruct{}.DecodeFrom(in)
	if err == nil {
		t.Fatal("expected aggregated errors")
	}
	leaves, ok := err.(validation.Errors)
	if !ok {
		t.Fatalf("err = %T, want validation.Errors; got: %v", err, err)
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
}

// pathOf reads the Path slice off a validation error, via the pathSegments
// interface if present, else a type switch over the leaf types this test uses.
func pathOf(e validation.Error) []string {
	type pathed interface{ pathSegments() []string }
	if p, ok := e.(pathed); ok {
		return p.pathSegments()
	}
	switch v := e.(type) {
	case *validation.EmailError:
		return v.Path
	case *validation.MinLenError:
		return v.Path
	case *validation.LTEError:
		return v.Path
	case *validation.GTEError:
		return v.Path
	case *validation.RequiredError:
		return v.Path
	}
	return nil
}

// An inner parse error returns immediately (not drained), wrapped in
// *decode.ParseError.
func TestNestedMultierr_innerParseErrorReturnsEarly(t *testing.T) {
	t.Parallel()
	// Inner email is a number, not a string.
	in := []byte(`{"inner":{"email":123},"name":"valid","code":50}`)
	_, _, err := NestedMultierrStruct{}.DecodeFrom(in)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if _, ok := err.(validation.Errors); ok {
		t.Errorf("parse error wrapped in validation.Errors: %v", err)
	}
}
