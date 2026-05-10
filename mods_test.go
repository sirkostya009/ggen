package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/decode"
	"github.com/sirkostya009/ggen/decode/validation"
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
	got, err := decode.Unmarshal[ModStruct](in)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.Email != "foo@bar.com" {
		t.Errorf("Email = %q, want %q", got.Email, "foo@bar.com")
	}
}

func TestMods_diveTagTrim(t *testing.T) {
	in := []byte(`{"tags":["  Go  ","  Rust  ","  C++  "]}`)
	got, err := decode.Unmarshal[ModStruct](in)
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
	got, err := decode.Unmarshal[ModStruct](in)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.SKU != "ABC123" {
		t.Errorf("SKU = %q", got.SKU)
	}

	// Without the prefix, pass-through.
	in2 := []byte(`{"sku":"XYZ"}`)
	got2, _ := decode.Unmarshal[ModStruct](in2)
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
	_, err := decode.Unmarshal[FallibleModStruct]([]byte(`{"email":"x"}`))
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
	_, err := decode.Unmarshal[FallibleModMultierrStruct]([]byte(`{"email":"x"}`))
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
	_, err := decode.Unmarshal[FallibleModStruct]([]byte(`{"email":"abcdef"}`))
	if err == nil {
		t.Fatal("expected validation error after mod passed")
	}
	if strings.Contains(err.Error(), "rejected by mod") {
		t.Errorf("mod ran on already-passing input: %v", err)
	}
}
