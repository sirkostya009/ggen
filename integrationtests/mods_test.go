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
