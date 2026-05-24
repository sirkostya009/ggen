package integrationtests

import (
	"errors"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/decode"
	"github.com/sirkostya009/ggen/decode/validation"
)

// MultiErrStruct collects all validation failures into a single joined error
// rather than stopping at the first.
//
//ggen:generate multierr
type MultiErrStruct struct {
	Name string `json:"name" ggen:"required,minlen=1,maxlen=5"`
	Age  int    `json:"age" ggen:"gte=0,lte=100"`
	Role string `json:"role" ggen:"oneof=admin|user|guest"`
}

func TestCustomValidator_pass(t *testing.T) {
	in := []byte(`{"tags":["go","rust"],"title":"ok","scores":[50],"count":4}`)
	got, err := decode.Unmarshal[DiveStruct](in)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.Count != 4 {
		t.Errorf("Count = %d", got.Count)
	}
}

func TestCustomValidator_fail(t *testing.T) {
	in := []byte(`{"tags":["go","rust"],"title":"ok","scores":[50],"count":5}`)
	_, err := decode.Unmarshal[DiveStruct](in)
	if err == nil {
		t.Fatal("expected custom-validator error")
	}
	if !strings.Contains(err.Error(), "not even") {
		t.Errorf("error = %q, want 'not even'", err.Error())
	}
}

func TestAggregate_allErrors(t *testing.T) {
	// All three fields violate: Name too long, Age negative, Role not in set.
	in := []byte(`{"name":"longname","age":-1,"role":"pirate"}`)
	_, err := decode.Unmarshal[MultiErrStruct](in)
	if err == nil {
		t.Fatal("expected aggregated errors")
	}
	msg := err.Error()
	for _, want := range []string{"exceeds maximum", "below minimum", "not in allowed set"} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error missing %q:\n%s", want, msg)
		}
	}
	// validation.Errors implements Unwrap() []error — walk via errors.As.
	var ves validation.Errors
	if !errors.As(err, &ves) {
		t.Fatalf("expected validation.Errors, got %T: %v", err, err)
	}
	if len(ves) < 3 {
		t.Errorf("expected >= 3 errors, got %d:\n%s", len(ves), msg)
	}
}

func TestAggregate_pass(t *testing.T) {
	in := []byte(`{"name":"ok","age":30,"role":"user"}`)
	got, err := decode.Unmarshal[MultiErrStruct](in)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.Name != "ok" {
		t.Errorf("Name = %q", got.Name)
	}
}

// CustomBothStruct exercises a single field carrying BOTH a custom @Func
// validator AND a custom @Func mod. They must both run, mod first
// (transforms the input) then validator (checks the transformed value).
//
//ggen:generate
type CustomBothStruct struct {
	Tags []string `json:"tags" mod:"dive:@TrimSpace" ggen:"dive:@NotBlank"`
}

// TestCustomBoth_modThenValidator: mod runs first (trims), then validator
// checks the result. Input `"  ok  "` → trim → `"ok"` → passes NotBlank.
// Input `"   "` → trim → `""` → fails NotBlank.
func TestCustomBoth_modThenValidator(t *testing.T) {
	good := []byte(`{"tags":["  hello  ","  world  "]}`)
	got, err := decode.Unmarshal[CustomBothStruct](good)
	if err != nil {
		t.Fatalf("unmarshal good: %v", err)
	}
	want := []string{"hello", "world"}
	if len(got.Tags) != len(want) || got.Tags[0] != want[0] || got.Tags[1] != want[1] {
		t.Errorf("Tags = %v, want trimmed %v", got.Tags, want)
	}

	bad := []byte(`{"tags":["ok","     "]}`)
	if _, err := decode.Unmarshal[CustomBothStruct](bad); err == nil {
		t.Fatal("expected blank-after-trim error")
	}
}
