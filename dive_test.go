package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/decode"
)

// DiveStruct exercises per-element (dive) validation, rune-counting length
// checks (multi-byte Unicode aware), and a user-defined custom validator
// resolved at codegen time via the `@Func` syntax.
//
//ggen:generate
type DiveStruct struct {
	// Tags: slice length 1..3, each element 2..10 runes (not bytes).
	Tags []string `json:"tags" ggen:"minlen=1,maxlen=3,dive:minrunes=2,maxrunes=10"`
	// Title: rune-count bounds so "héllo" (5 runes, 6 bytes) is accepted.
	Title string `json:"title" ggen:"minrunes=1,maxrunes=5"`
	// Scores: each score must be 0..100 inclusive.
	Scores []int `json:"scores" ggen:"dive:gte=0,lte=100"`
	// Count: custom validator resolved statically — `@EvenOnly` looks up
	// the EvenOnly function in this package at parse time, validates the
	// signature is `func(int) error`, and emits a direct call.
	Count int `json:"count" ggen:"@EvenOnly"`
}

// EvenOnly is the custom validator referenced by DiveStruct.Count via
// `ggen:"@EvenOnly"`. The generator resolves the symbol at codegen time
// (no runtime registry) and checks the signature against the field type.
func EvenOnly(n int) error {
	if n%2 != 0 {
		return fmt.Errorf("%d is not even", n)
	}
	return nil
}

func TestDive_valid(t *testing.T) {
	input := `{"tags":["go","rust"],"title":"héllo","scores":[10,50,100]}`
	got, err := decode.Unmarshal[DiveStruct]([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Tags) != 2 {
		t.Errorf("Tags len = %d, want 2", len(got.Tags))
	}
	if got.Title != "héllo" {
		t.Errorf("Title = %q", got.Title)
	}
}

func TestDive_tagTooShort(t *testing.T) {
	// "x" is 1 rune, below minrunes=2 → per-element error
	input := `{"tags":["ok","x"],"title":"hi","scores":[]}`
	_, err := decode.Unmarshal[DiveStruct]([]byte(input))
	if err == nil {
		t.Fatal("expected error for too-short tag element")
	}
	if !strings.Contains(err.Error(), "below minimum") {
		t.Errorf("error = %q, want 'below minimum'", err.Error())
	}
}

func TestDive_scoreOutOfRange(t *testing.T) {
	// 101 violates lte=100 per element
	input := `{"tags":["ok"],"title":"hi","scores":[50,101]}`
	_, err := decode.Unmarshal[DiveStruct]([]byte(input))
	if err == nil {
		t.Fatal("expected error for out-of-range score")
	}
	if !strings.Contains(err.Error(), "exceeds maximum") {
		t.Errorf("error = %q, want 'exceeds maximum'", err.Error())
	}
}

func TestRuneCount_byteVsRune(t *testing.T) {
	// "héllo" is 5 runes but 6 bytes. With rune-count bounds (1..5), it passes.
	input := `{"tags":["ok"],"title":"héllo","scores":[]}`
	if _, err := decode.Unmarshal[DiveStruct]([]byte(input)); err != nil {
		t.Errorf("5-rune multibyte title should pass: %v", err)
	}

	// 6 runes should fail maxrunes=5
	input2 := `{"tags":["ok"],"title":"héllos","scores":[]}`
	if _, err := decode.Unmarshal[DiveStruct]([]byte(input2)); err == nil {
		t.Error("6-rune title should fail maxrunes=5")
	}
}

func TestTags_sliceLengthVsElement(t *testing.T) {
	// 4 tags > maxlen=3 → slice-level error
	input := `{"tags":["aa","bb","cc","dd"],"title":"hi","scores":[]}`
	_, err := decode.Unmarshal[DiveStruct]([]byte(input))
	if err == nil {
		t.Fatal("expected slice-length error")
	}
	if !strings.Contains(err.Error(), "exceeds maximum") {
		t.Errorf("error = %q", err.Error())
	}
}

// CustomDiveStruct exercises `@Func` resolution at the four
// container-aware sites: slice element (`dive:`) for validators and
// mods, and map key (`keys:`) for validators and mods. Plus a pointer
// field where the func must accept `*T`. Each rule references a
// dedicated function below so a missed call site shows up as test
// failure rather than silent codegen drift.
//
//ggen:generate
type CustomDiveStruct struct {
	Tags   []string       `json:"tags" ggen:"dive:@NotBlank"`
	Trim   []string       `json:"trim" mod:"dive:@TrimSpace"`
	Lookup map[string]int `json:"lookup" ggen:"keys:@KeyShape"`
	Mixed  map[string]int `json:"mixed" mod:"keys:@LowerKey"`
	Ptr    *int           `json:"ptr" ggen:"@PointerCheck"`
}

// NotBlank is invoked once per slice element via `dive:@NotBlank`.
func NotBlank(s string) error {
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("blank element")
	}
	return nil
}

// TrimSpace is a pure mod called per slice element.
func TrimSpace(s string) string { return strings.TrimSpace(s) }

// KeyShape is invoked once per map key via `keys:@KeyShape`.
func KeyShape(s string) error {
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return fmt.Errorf("bad key %q", s)
		}
	}
	return nil
}

// LowerKey is a pure mod called per map key before insertion.
func LowerKey(s string) string { return strings.ToLower(s) }

// PointerCheck verifies that pointer fields receive `*T`, not `T`.
// nil-handling lives in the user's func by design.
func PointerCheck(p *int) error {
	if p != nil && *p < 0 {
		return fmt.Errorf("negative")
	}
	return nil
}

// TestCustomDive_sliceValidator: `dive:@NotBlank` rejects a blank
// element. Locks down the per-element call site for slices.
func TestCustomDive_sliceValidator(t *testing.T) {
	in := []byte(`{"tags":["ok","   "],"trim":[],"lookup":{},"mixed":{},"ptr":null}`)
	_, err := decode.Unmarshal[CustomDiveStruct](in)
	if err == nil {
		t.Fatal("expected blank-element error")
	}
	if !strings.Contains(err.Error(), "blank element") {
		t.Errorf("error = %q, want NotBlank message", err.Error())
	}
}

// TestCustomDive_sliceMod: `dive:@TrimSpace` runs on each element
// (visible because the trimmed result differs from the input).
func TestCustomDive_sliceMod(t *testing.T) {
	in := []byte(`{"tags":["ok"],"trim":["  hi  ","  bye"],"lookup":{},"mixed":{},"ptr":null}`)
	got, err := decode.Unmarshal[CustomDiveStruct](in)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	want := []string{"hi", "bye"}
	for i, v := range got.Trim {
		if v != want[i] {
			t.Errorf("Trim[%d] = %q, want %q", i, v, want[i])
		}
	}
}

// TestCustomDive_keysValidator: `keys:@KeyShape` rejects a key that
// fails the alphanumeric-lower predicate. Locks down the per-key
// call site for maps.
func TestCustomDive_keysValidator(t *testing.T) {
	in := []byte(`{"tags":["ok"],"trim":[],"lookup":{"BAD!":1},"mixed":{},"ptr":null}`)
	_, err := decode.Unmarshal[CustomDiveStruct](in)
	if err == nil {
		t.Fatal("expected bad-key error")
	}
	if !strings.Contains(err.Error(), "bad key") {
		t.Errorf("error = %q, want KeyShape message", err.Error())
	}
}

// TestCustomDive_keysMod: `mod:"keys:@LowerKey"` lowercases each key on
// the way in. Verifies the mod runs before insertion (output map has
// lowered keys).
func TestCustomDive_keysMod(t *testing.T) {
	in := []byte(`{"tags":["ok"],"trim":[],"lookup":{},"mixed":{"FOO":1,"Bar":2},"ptr":null}`)
	got, err := decode.Unmarshal[CustomDiveStruct](in)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.Mixed["foo"] != 1 || got.Mixed["bar"] != 2 {
		t.Errorf("Mixed = %v, want lowercased keys foo/bar", got.Mixed)
	}
	if _, present := got.Mixed["FOO"]; present {
		t.Errorf("uppercase key still present: %v", got.Mixed)
	}
}

// TestCustomDive_pointerField: `@PointerCheck` is `func(*int) error`,
// proving the resolver respects "exact field type" for pointer
// fields (the spec says `*T` field → mod takes `*T`).
func TestCustomDive_pointerField(t *testing.T) {
	// Negative value rejected.
	bad := []byte(`{"tags":["ok"],"trim":[],"lookup":{},"mixed":{},"ptr":-5}`)
	if _, err := decode.Unmarshal[CustomDiveStruct](bad); err == nil {
		t.Error("expected negative-value error from PointerCheck")
	}
	// Positive value accepted.
	good := []byte(`{"tags":["ok"],"trim":[],"lookup":{},"mixed":{},"ptr":5}`)
	got, err := decode.Unmarshal[CustomDiveStruct](good)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.Ptr == nil || *got.Ptr != 5 {
		t.Errorf("Ptr = %v, want 5", got.Ptr)
	}
	// nil accepted (PointerCheck handles nil itself).
	null := []byte(`{"tags":["ok"],"trim":[],"lookup":{},"mixed":{},"ptr":null}`)
	if _, err := decode.Unmarshal[CustomDiveStruct](null); err != nil {
		t.Errorf("nil pointer rejected: %v", err)
	}
}
