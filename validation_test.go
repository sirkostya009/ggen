package ggen

import (
	"errors"
	"testing"
)

// A fail-fast leaf nested under a multierr parent gets the outer segment —
// Append used to prepend only for nested Errors aggregates, contradicting
// its own doc comment.
func TestErrorsAppend_PrependsLeafPath(t *testing.T) {
	t.Parallel()
	var es Errors
	es.Append("outer", &LenError{Path: []string{"inner"}, Want: 5, Got: 3})
	le, ok := es[0].(*LenError)
	if !ok || len(le.Path) != 2 || le.Path[0] != "outer" || le.Path[1] != "inner" {
		t.Errorf("leaf path = %v, want [outer inner]", le)
	}
	// Nested aggregates keep flattening + prepending.
	var parent Errors
	parent.Append("p", Errors{&NotEmptyError{Path: []string{"q"}}})
	ne := parent[0].(*NotEmptyError)
	if len(ne.Path) != 2 || ne.Path[0] != "p" {
		t.Errorf("nested path = %v", ne.Path)
	}
}

// A single leaf rendering "" used to panic on &buf[0].
// The documented class is "contains no uppercase" — caseless characters
// (digits, spaces, punctuation, uncased scripts) pass.
func TestIsLowerIsUpper_CaselessPass(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"abc123", "hello world", "п'ять-5", "日本語"} {
		if !IsLower(s) {
			t.Errorf("IsLower(%q) = false, contains no uppercase", s)
		}
	}
	if IsLower("aBc") {
		t.Error("IsLower(aBc) = true")
	}
	for _, s := range []string{"ABC123", "HELLO WORLD", "日本語"} {
		if !IsUpper(s) {
			t.Errorf("IsUpper(%q) = false, contains no lowercase", s)
		}
	}
	if IsUpper("AbC") {
		t.Error("IsUpper(AbC) = true")
	}
}

func TestErrorsError_EmptyMessageLeaf(t *testing.T) {
	t.Parallel()
	es := Errors{Errors{}}
	if got := es.Error(); got != "" {
		t.Errorf("got %q", got)
	}
}

// A nested fallible mod's ModError is born pre-wrapped in a *ParseError, so
// the outer rebase has to cascade into the cause.
func TestParseErrAddPosCascadesToModError(t *testing.T) {
	inner := NewParseErr("f", 5, &ModError{Name: "clamp", Pos: 5})
	outer := NewParseErrShift("b", 105, 5, inner)

	var pe *ParseError
	if !errors.As(outer, &pe) || pe.Pos != 105 {
		t.Fatalf("outer Pos = %v, want 105", pe)
	}
	var me *ModError
	if !errors.As(outer, &me) {
		t.Fatal("no ModError")
	}
	if me.Pos != 105 {
		t.Errorf("ModError.Pos = %d, want 105", me.Pos)
	}
}
