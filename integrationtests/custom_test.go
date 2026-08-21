package integrationtests

//go:generate ../ggen $GOFILE

import (
	"errors"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen"
)

// MultiErrStruct collects all validation failures rather than stopping first.
//
//ggen:generate multierr
type MultiErrStruct struct {
	Name string `json:"name" pipe:"required minlen=1 maxlen=5"`
	Age  int    `json:"age" pipe:"gte=0 lte=100"`
	Role string `json:"role" pipe:"oneof=admin|user|guest"`
}

func TestCustomValidator_pass(t *testing.T) {
	t.Parallel()
	in := []byte(`{"tags":["go","rust"],"title":"ok","scores":[50],"count":4}`)
	got, _, err := DiveStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.Count != 4 {
		t.Errorf("Count = %d", got.Count)
	}
}

func TestCustomValidator_fail(t *testing.T) {
	t.Parallel()
	in := []byte(`{"tags":["go","rust"],"title":"ok","scores":[50],"count":5}`)
	_, _, err := DiveStruct{}.DecodeFrom(in)
	if err == nil {
		t.Fatal("expected custom-validator error")
	}
	if !strings.Contains(err.Error(), "not even") {
		t.Errorf("error = %q, want 'not even'", err.Error())
	}
}

func TestAggregate_allErrors(t *testing.T) {
	t.Parallel()
	// All three fields violate: Name too long, Age negative, Role not in set.
	in := []byte(`{"name":"longname","age":-1,"role":"pirate"}`)
	_, _, err := MultiErrStruct{}.DecodeFrom(in)
	if err == nil {
		t.Fatal("expected aggregated errors")
	}
	msg := err.Error()
	for _, want := range []string{"exceeds maximum", "below minimum", "not in allowed set"} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error missing %q:\n%s", want, msg)
		}
	}
	// ggen.Errors implements Unwrap() []error — walk via errors.As.
	var ves ggen.Errors
	if !errors.As(err, &ves) {
		t.Fatalf("expected ggen.Errors, got %T: %v", err, err)
	}
	if len(ves) < 3 {
		t.Errorf("expected >= 3 errors, got %d:\n%s", len(ves), msg)
	}
}

func TestAggregate_pass(t *testing.T) {
	t.Parallel()
	in := []byte(`{"name":"ok","age":30,"role":"user"}`)
	got, _, err := MultiErrStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.Name != "ok" {
		t.Errorf("Name = %q", got.Name)
	}
}

// CustomBothStruct carries a custom @Func mod and validator on one field.
//
//ggen:generate
type CustomBothStruct struct {
	Tags []string `json:"tags" pipe:"inner:(@TrimSpace @NotBlank)"`
}

// Mod (trim) runs before validator (NotBlank): "  ok  " passes, "   " fails.
func TestCustomBoth_modThenValidator(t *testing.T) {
	t.Parallel()
	good := []byte(`{"tags":["  hello  ","  world  "]}`)
	got, _, err := CustomBothStruct{}.DecodeFrom(good)
	if err != nil {
		t.Fatalf("unmarshal good: %v", err)
	}
	want := []string{"hello", "world"}
	if len(got.Tags) != len(want) || got.Tags[0] != want[0] || got.Tags[1] != want[1] {
		t.Errorf("Tags = %v, want trimmed %v", got.Tags, want)
	}

	bad := []byte(`{"tags":["ok","     "]}`)
	if _, _, err := (CustomBothStruct{}).DecodeFrom(bad); err == nil {
		t.Fatal("expected blank-after-trim error")
	}
}
