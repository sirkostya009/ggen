package ggen

import (
	"errors"
	"testing"
)

// Collect must prepend the outer path segment to a single leaf error, not
// only to a nested Errors aggregate, and shift both by d.
func TestErrorsCollect_PrependsLeafPath(t *testing.T) {
	t.Parallel()
	var es Errors
	if !es.Collect("outer", 10, &LenError{Pos: 1, Keys: []string{"inner"}, Want: 5, Got: 3}) {
		t.Fatal("Collect rejected a ValidationError")
	}
	le, ok := es[0].(*LenError)
	if !ok || len(le.Keys) != 2 || le.Keys[0] != "outer" || le.Keys[1] != "inner" || le.Pos != 11 {
		t.Errorf("leaf = %+v, want [outer inner] at 11", le)
	}
	// A nested aggregate is flattened and each leaf gets the prefix.
	var parent Errors
	parent.Collect("p", 0, Errors{&NotEmptyError{Keys: []string{"q"}}})
	ne := parent[0].(*NotEmptyError)
	if len(ne.Keys) != 2 || ne.Keys[0] != "p" {
		t.Errorf("nested path = %v", ne.Keys)
	}
	if parent.Collect("p", 0, &ParseError{Err: ErrBadString}) || len(parent) != 1 {
		t.Errorf("Collect took a parse error: %v", parent)
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
	es := Errors{&CustomError{Cause: errors.New("")}}
	if got := es.Error(); got != "" {
		t.Errorf("got %q", got)
	}
}

// A ModError from a nested mod arrives already wrapped in a *ParseError.
// When the outer decoder shifts the position to the full payload offset, the
// shift must also reach the ModError inside.
func TestParseErrShiftCascadesToModError(t *testing.T) {
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
		{LenError{Keys: []string{"a", "b"}, Want: 2, Got: 1}, "a.b: length 1 != required 2"},
		{LenError{Keys: []string{"a", "b"}, Want: 2, Got: 3, AtLeast: true}, "a.b: length at least 3 != required 2"},
	} {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("Error() = %q, want %q", got, tc.want)
		}
	}
}

// Every error type reports its path and message apart, so a caller can put
// an issue next to the field it is about without parsing Error().
func TestPathMessage(t *testing.T) {
	t.Parallel()
	for _, err := range []ValidationError{
		&RequiredError{Keys: []string{"tags[]"}},
		&NotEmptyError{Keys: []string{"tags[]"}},
		&LenError{Keys: []string{"tags[]"}},
		&MinLenError{Keys: []string{"tags[]"}},
		&MaxLenError{Keys: []string{"tags[]"}},
		&RunesError{Keys: []string{"tags[]"}},
		&MinRunesError{Keys: []string{"tags[]"}},
		&MaxRunesError{Keys: []string{"tags[]"}},
		&GTError{Keys: []string{"tags[]"}},
		&GTEError{Keys: []string{"tags[]"}},
		&LTError{Keys: []string{"tags[]"}},
		&LTEError{Keys: []string{"tags[]"}},
		&EqError{Keys: []string{"tags[]"}},
		&NeqError{Keys: []string{"tags[]"}},
		&OneOfError{Keys: []string{"tags[]"}},
		&URLError{Keys: []string{"tags[]"}},
		&AlphanumError{Keys: []string{"tags[]"}},
		&NumericError{Keys: []string{"tags[]"}},
		&LowerError{Keys: []string{"tags[]"}},
		&UpperError{Keys: []string{"tags[]"}},
		&HexadecimalError{Keys: []string{"tags[]"}},
		&StartsError{Keys: []string{"tags[]"}},
		&EndsError{Keys: []string{"tags[]"}},
		&ContainsError{Keys: []string{"tags[]"}},
		&MultipleError{Keys: []string{"tags[]"}},
		&DuplicateKeyError{Keys: []string{"tags[]"}},
		&UnknownKeyError{Keys: []string{"tags[]"}},
		&CustomError{Keys: []string{"tags[]"}, Cause: errors.New("bad slug")},
		&PredicateError{Keys: []string{"tags[]"}},
	} {
		if p := err.Path(); len(p) != 1 || p[0] != "tags[]" {
			t.Errorf("%T.Path() = %v", err, p)
		}
		if msg := err.Message(); msg == "" || err.Error() != "tags[]: "+msg {
			t.Errorf("%T: Error() = %q, Message() = %q", err, err.Error(), msg)
		}
	}
	pe := &ParseError{Keys: []string{"tags[]"}, Err: ErrBadString}
	if p := pe.Path(); len(p) != 1 || p[0] != "tags[]" || pe.Message() != ErrBadString.Error() {
		t.Errorf("ParseError: Path() = %v, Message() = %q", p, pe.Message())
	}
}
