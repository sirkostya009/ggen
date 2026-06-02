package integrationtests

//go:generate ../ggen $GOFILE

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/decode/validation"
	_ "github.com/sirkostya009/ggen/integrationtests/thirdparty"
)

// ModStruct exercises the mod tag: input transforms applied after decode and
// before validation (so validation sees the normalized value).
//
//ggen:generate
type ModStruct struct {
	// Lowercase + trim → validates after normalization.
	Email string `json:"email" ggen:"email" mod:"trim,lower"`
	// Each tag is trimmed before being kept.
	Tags []string `json:"tags" mod:"dive:trim,lower"`
	// Strip a known prefix.
	SKU string `json:"sku" mod:"trimleft=SKU-"`
}

func TestMods_trimLowerEmail(t *testing.T) {
	// Raw has leading/trailing space and uppercase; mods normalize before validation.
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
	in := []byte(`{"sku":"SKU-ABC123"}`)
	got, _, err := ModStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.SKU != "ABC123" {
		t.Errorf("SKU = %q", got.SKU)
	}

	// Without the prefix, pass-through.
	in2 := []byte(`{"sku":"XYZ"}`)
	got2, _, _ := ModStruct{}.DecodeFrom(in2)
	if got2.SKU != "XYZ" {
		t.Errorf("SKU = %q", got2.SKU)
	}
}

// FallibleModStruct pairs a fallible custom mod with built-in validators
// that would also fail on the same input. Tests below assert that mod
// errors short-circuit BEFORE validation runs — they're parse-level
// failures, not validation failures, so they bypass the multierr
// aggregation path entirely.
//
//ggen:generate
type FallibleModStruct struct {
	Email string `json:"email" mod:"@RejectShort" ggen:"required,email,minlen=10"`
}

// FallibleModMultierrStruct is the same field set in multierr mode:
// validation rules accumulate into validation.Errors, but a fallible
// mod must NOT — it returns immediately as a single parse error.
//
//ggen:generate multierr
type FallibleModMultierrStruct struct {
	Email string `json:"email" mod:"@RejectShort" ggen:"required,email,minlen=10"`
}

func RejectShort(s string) (string, error) {
	if len(s) < 3 {
		return "", fmt.Errorf("rejected by mod")
	}
	return s, nil
}

// TestFallibleMod_shortCircuitsValidation: input is too short — both the
// mod and the email/minlen validators would fail, but the mod runs first
// and its error is what surfaces.
func TestFallibleMod_shortCircuitsValidation(t *testing.T) {
	_, _, err := FallibleModStruct{}.DecodeFrom([]byte(`{"email":"x"}`))
	if err == nil {
		t.Fatal("expected mod rejection")
	}
	if !strings.Contains(err.Error(), "rejected by mod") {
		t.Errorf("expected mod error, got: %v", err)
	}
	// Validation messages from `email` / `minlen` would surface as
	// "not a valid email" / "below minimum"; mod error must beat them.
	if strings.Contains(err.Error(), "valid email") || strings.Contains(err.Error(), "below minimum") {
		t.Errorf("validation ran despite mod failure: %v", err)
	}
}

// TestFallibleMod_multierrStillShortCircuits: even in multierr mode,
// a fallible mod is a parse error — it returns a single error
// immediately, NOT a validation.Errors slice. The multierr aggregation
// is only for validation-rule failures.
func TestFallibleMod_multierrStillShortCircuits(t *testing.T) {
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

// TestFallibleMod_passLetsValidationRun: when the mod ACCEPTS the value,
// downstream validation still runs and can fire its own errors. Confirms
// the mod is a gate, not a bypass.
func TestFallibleMod_passLetsValidationRun(t *testing.T) {
	// "abcdef" passes RejectShort (len >= 3) but fails minlen=10 + email.
	_, _, err := FallibleModStruct{}.DecodeFrom([]byte(`{"email":"abcdef"}`))
	if err == nil {
		t.Fatal("expected validation error after mod passed")
	}
	if strings.Contains(err.Error(), "rejected by mod") {
		t.Errorf("mod ran on already-passing input: %v", err)
	}
}

// CrossPkgModStruct exercises `@pkg.Func` resolution for both validators
// and mods. The functions live in the sibling `thirdparty` package; the
// codegen-time resolver walks this file's imports, picks up the
// non-aliased `thirdparty` package, and emits a direct call. Three
// flavors per direction so a regression in one (pure mod vs fallible
// mod vs validator) surfaces at that flavor.
//
//ggen:generate
type CrossPkgModStruct struct {
	// Pure mod: prefixes the input with '#'.
	Tag string `json:"tag" mod:"@thirdparty.PrefixHash"`
	// Fallible mod: rejects empty input as a parse error, not validation.
	NonEmpty string `json:"nonEmpty" mod:"@thirdparty.ParseNonEmpty"`
	// Validator: requires all-uppercase ASCII.
	Code string `json:"code" ggen:"@thirdparty.ValidateUpper"`
}

func TestCrossPkgMod_pureMod(t *testing.T) {
	got, _, err := CrossPkgModStruct{}.DecodeFrom([]byte(`{"tag":"x","nonEmpty":"y","code":"OK"}`))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Tag != "#x" {
		t.Errorf("Tag = %q, want #x (mod prefix not applied)", got.Tag)
	}
}

func TestCrossPkgMod_fallibleModRejects(t *testing.T) {
	_, _, err := CrossPkgModStruct{}.DecodeFrom([]byte(`{"tag":"x","nonEmpty":"","code":"OK"}`))
	if err == nil {
		t.Fatal("expected fallible-mod rejection")
	}
	if !strings.Contains(err.Error(), "empty value") {
		t.Errorf("expected cross-pkg mod error, got: %v", err)
	}
}

func TestCrossPkgValidator_rejects(t *testing.T) {
	_, _, err := CrossPkgModStruct{}.DecodeFrom([]byte(`{"tag":"x","nonEmpty":"y","code":"lowercase"}`))
	if err == nil {
		t.Fatal("expected cross-pkg validator rejection")
	}
	if !strings.Contains(err.Error(), "must be uppercase") {
		t.Errorf("expected cross-pkg validator error, got: %v", err)
	}
}

func TestCrossPkgValidator_accepts(t *testing.T) {
	got, _, err := CrossPkgModStruct{}.DecodeFrom([]byte(`{"tag":"x","nonEmpty":"y","code":"OK"}`))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Code != "OK" {
		t.Errorf("Code = %q", got.Code)
	}
}

// NestedMultierrStruct wraps another multierr struct (FallibleModMultierrStruct)
// as a field, plus its own validation rules. When DecodeFrom hits inner's
// validation errors, the outer must drain them into its own errs slice
// instead of short-circuiting — so the caller sees ALL failures in one
// validation.Errors aggregate (outer + inner).
//
//ggen:generate multierr
type NestedMultierrStruct struct {
	Inner FallibleModMultierrStruct `json:"inner"`
	Name  string                    `json:"name" ggen:"required,minlen=2"`
	Code  int                       `json:"code" ggen:"gte=0,lte=100"`
}

func TestNestedMultierr_drainsInnerValidationErrors(t *testing.T) {
	// inner.email = "x" -> mod accepts (len>=3? no, len==1 — wait RejectShort
	// rejects len<3 as a parse error). Use a value that passes the mod but
	// fails validation, so the inner returns a validation.Errors aggregate
	// rather than a single parse error.
	// "abcdef" passes RejectShort (len>=3), fails minlen=10 (len=6) AND fails
	// email pattern → inner returns validation.Errors{minlen, email}.
	// Outer also fails: name = "" (required + minlen=2), code = 200 (lte=100).
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
	// Nested-struct decodes prepend the outer field segment via
	// validation.Append; inner's email leaves surface with Path
	// ["inner","email"], outer leaves stay one segment deep.
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

// pathOf reads the Path slice off any concrete validation error via a
// shared shape — every leaf in the package embeds {Path []string} so a
// reflect-free type switch over the open set is too noisy; the
// errors.As fast path returns a pointer with the field we need.
func pathOf(e validation.Error) []string {
	type pathed interface{ pathSegments() []string }
	if p, ok := e.(pathed); ok {
		return p.pathSegments()
	}
	// Fallback: dig via the public Path field on every leaf type by
	// covering the ones used in this test.
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

// TestNestedMultierr_innerParseErrorReturnsEarly: when the inner decode hits
// a true parse error (malformed JSON), the outer should NOT drain it — it
// returns immediately, wrapped in *decode.ParseError.
func TestNestedMultierr_innerParseErrorReturnsEarly(t *testing.T) {
	// Inner email value is a number, not a string — scan.ErrExpectString.
	in := []byte(`{"inner":{"email":123},"name":"valid","code":50}`)
	_, _, err := NestedMultierrStruct{}.DecodeFrom(in)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if _, ok := err.(validation.Errors); ok {
		t.Errorf("parse error wrapped in validation.Errors: %v", err)
	}
}
