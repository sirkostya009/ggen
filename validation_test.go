package ggen

import (
	"errors"
	"testing"
)

// Append must prepend the outer path segment to a single leaf error, not
// only to a nested Errors aggregate. This is what its doc comment promises.
func TestErrorsAppend_PrependsLeafPath(t *testing.T) {
	t.Parallel()
	var es Errors
	es.Append("outer", &LenError{Path: []string{"inner"}, Want: 5, Got: 3})
	le, ok := es[0].(*LenError)
	if !ok || len(le.Path) != 2 || le.Path[0] != "outer" || le.Path[1] != "inner" {
		t.Errorf("leaf path = %v, want [outer inner]", le)
	}
	// A nested aggregate is flattened and each leaf gets the prefix.
	var parent Errors
	parent.Append("p", Errors{&NotEmptyError{Path: []string{"q"}}})
	ne := parent[0].(*NotEmptyError)
	if len(ne.Path) != 2 || ne.Path[0] != "p" {
		t.Errorf("nested path = %v", ne.Path)
	}
}

// IsLower means "contains no uppercase" and IsUpper means "contains no
// lowercase". Characters without case pass both: digits, spaces,
// punctuation, and scripts like Japanese.
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

// Error must not panic when the only leaf renders an empty message.
func TestErrorsError_EmptyMessageLeaf(t *testing.T) {
	t.Parallel()
	es := Errors{Errors{}}
	if got := es.Error(); got != "" {
		t.Errorf("got %q", got)
	}
}

// A ModError from a nested mod arrives already wrapped in a *ParseError.
// When the outer decoder shifts the position to the full payload offset, the
// shift must also reach the ModError inside.
func TestParseErrAddPosCascadesToModError(t *testing.T) {
	t.Parallel()
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

// A minimum-length error says "at least" in its message. An exact-length
// error does not.
func TestLenError_Message(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  LenError
		want string
	}{
		{LenError{Path: []string{"a", "b"}, Want: 2, Got: 1}, "a.b: length 1 != required 2"},
		{LenError{Path: []string{"a", "b"}, Want: 2, Got: 3, AtLeast: true}, "a.b: length at least 3 != required 2"},
	} {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("Error() = %q, want %q", got, tc.want)
		}
	}
}
