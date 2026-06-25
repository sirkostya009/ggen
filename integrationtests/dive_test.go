package integrationtests

//go:generate ../ggen $GOFILE

import (
	"fmt"
	"strings"
	"testing"
)

// DiveStruct exercises inner: per-element validation, rune-count length
// checks, and a custom @Func validator resolved at codegen time.
//
//ggen:generate
type DiveStruct struct {
	// slice length 1..3, each element 2..10 runes
	Tags []string `json:"tags" pipe:"minlen=1 maxlen=3 inner:(minrunes=2 maxrunes=10)"`
	// rune bounds so "héllo" (5 runes, 6 bytes) passes
	Title string `json:"title" pipe:"minrunes=1 maxrunes=5"`
	// each score 0..100
	Scores []int `json:"scores" pipe:"inner:(gte=0 lte=100)"`
	// @EvenOnly resolved statically against this package
	Count int `json:"count" pipe:"@EvenOnly"`
}

// EvenOnly is the custom validator for DiveStruct.Count.
func EvenOnly(n int) error {
	if n%2 != 0 {
		return fmt.Errorf("%d is not even", n)
	}
	return nil
}

func TestDive_valid(t *testing.T) {
	input := `{"tags":["go","rust"],"title":"héllo","scores":[10,50,100]}`
	got, _, err := (DiveStruct{}).DecodeFrom([]byte(input))
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
	_, _, err := (DiveStruct{}).DecodeFrom([]byte(input))
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
	_, _, err := (DiveStruct{}).DecodeFrom([]byte(input))
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
	if _, _, err := (DiveStruct{}).DecodeFrom([]byte(input)); err != nil {
		t.Errorf("5-rune multibyte title should pass: %v", err)
	}

	// 6 runes should fail maxrunes=5
	input2 := `{"tags":["ok"],"title":"héllos","scores":[]}`
	if _, _, err := (DiveStruct{}).DecodeFrom([]byte(input2)); err == nil {
		t.Error("6-rune title should fail maxrunes=5")
	}
}

func TestTags_sliceLengthVsElement(t *testing.T) {
	// 4 tags > maxlen=3 → slice-level error
	input := `{"tags":["aa","bb","cc","dd"],"title":"hi","scores":[]}`
	_, _, err := (DiveStruct{}).DecodeFrom([]byte(input))
	if err == nil {
		t.Fatal("expected slice-length error")
	}
	if !strings.Contains(err.Error(), "exceeds maximum") {
		t.Errorf("error = %q", err.Error())
	}
}

// CustomDiveStruct exercises @Func resolution at the container-aware sites:
// slice element (inner:) and map key (keys:) for both validators and mods,
// plus a pointer field where the func must accept *T.
//
//ggen:generate
type CustomDiveStruct struct {
	Tags   []string       `json:"tags" pipe:"inner:@NotBlank"`
	Trim   []string       `json:"trim" pipe:"inner:@TrimSpace"`
	Lookup map[string]int `json:"lookup" pipe:"keys:@KeyShape"`
	Mixed  map[string]int `json:"mixed" pipe:"keys:@LowerKey"`
	Ptr    *int           `json:"ptr" pipe:"@PointerCheck"`
}

// NotBlank is invoked once per slice element via `inner:@NotBlank`.
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

// PointerCheck verifies a pointer field passes *T, not T.
func PointerCheck(p *int) error {
	if p != nil && *p < 0 {
		return fmt.Errorf("negative")
	}
	return nil
}

// inner:@NotBlank rejects a blank slice element.
func TestCustomDive_sliceValidator(t *testing.T) {
	in := []byte(`{"tags":["ok","   "],"trim":[],"lookup":{},"mixed":{},"ptr":null}`)
	_, _, err := (CustomDiveStruct{}).DecodeFrom(in)
	if err == nil {
		t.Fatal("expected blank-element error")
	}
	if !strings.Contains(err.Error(), "blank element") {
		t.Errorf("error = %q, want NotBlank message", err.Error())
	}
}

// inner:@TrimSpace runs on each slice element.
func TestCustomDive_sliceMod(t *testing.T) {
	in := []byte(`{"tags":["ok"],"trim":["  hi  ","  bye"],"lookup":{},"mixed":{},"ptr":null}`)
	got, _, err := (CustomDiveStruct{}).DecodeFrom(in)
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

// keys:@KeyShape rejects a key failing the predicate.
func TestCustomDive_keysValidator(t *testing.T) {
	in := []byte(`{"tags":["ok"],"trim":[],"lookup":{"BAD!":1},"mixed":{},"ptr":null}`)
	_, _, err := (CustomDiveStruct{}).DecodeFrom(in)
	if err == nil {
		t.Fatal("expected bad-key error")
	}
	if !strings.Contains(err.Error(), "bad key") {
		t.Errorf("error = %q, want KeyShape message", err.Error())
	}
}

// keys:@LowerKey lowercases each key before insertion.
func TestCustomDive_keysMod(t *testing.T) {
	in := []byte(`{"tags":["ok"],"trim":[],"lookup":{},"mixed":{"FOO":1,"Bar":2},"ptr":null}`)
	got, _, err := (CustomDiveStruct{}).DecodeFrom(in)
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

// @PointerCheck is func(*int) error — resolver respects the exact *T type.
func TestCustomDive_pointerField(t *testing.T) {
	// Negative value rejected.
	bad := []byte(`{"tags":["ok"],"trim":[],"lookup":{},"mixed":{},"ptr":-5}`)
	if _, _, err := (CustomDiveStruct{}).DecodeFrom(bad); err == nil {
		t.Error("expected negative-value error from PointerCheck")
	}
	// Positive value accepted.
	good := []byte(`{"tags":["ok"],"trim":[],"lookup":{},"mixed":{},"ptr":5}`)
	got, _, err := (CustomDiveStruct{}).DecodeFrom(good)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.Ptr == nil || *got.Ptr != 5 {
		t.Errorf("Ptr = %v, want 5", got.Ptr)
	}
	// nil accepted (PointerCheck handles nil itself).
	null := []byte(`{"tags":["ok"],"trim":[],"lookup":{},"mixed":{},"ptr":null}`)
	if _, _, err := (CustomDiveStruct{}).DecodeFrom(null); err != nil {
		t.Errorf("nil pointer rejected: %v", err)
	}
}
