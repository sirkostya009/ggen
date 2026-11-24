package ggen

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// Collect takes a nested Errors, prepends the outer path segment to each
// leaf and shifts it by d. A single ValidationError means the callee stopped
// mid-value, so Collect refuses it like a parse error.
func TestErrorsCollect_PrependsLeafPath(t *testing.T) {
	t.Parallel()
	var es Errors
	if !es.Collect("outer", 10, Errors{&LenError{Pos: 1, Keys: []string{"inner"}, Want: 5, Got: 3}}) {
		t.Fatal("Collect rejected an Errors")
	}
	le, ok := es[0].(*LenError)
	if !ok || len(le.Keys) != 2 || le.Keys[0] != "outer" || le.Keys[1] != "inner" || le.Pos != 11 {
		t.Errorf("leaf = %+v, want [outer inner] at 11", le)
	}
	if es.Collect("outer", 0, &LenError{Keys: []string{"inner"}}) || len(es) != 1 {
		t.Errorf("Collect took a single ValidationError: %v", es)
	}
	if es.Collect("p", 0, &ParseError{Err: ErrBadString}) || len(es) != 1 {
		t.Errorf("Collect took a parse error: %v", es)
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

// The character-class predicates need at least one byte. The case
// predicates test for an absence, so "" passes them.
func TestPredicates_EmptyString(t *testing.T) {
	t.Parallel()
	if IsAlphanum("") || IsNumeric("") || IsHex("") || IsURL("") {
		t.Error(`a character-class predicate accepts ""`)
	}
	if !IsLower("") || !IsUpper("") {
		t.Error(`a case predicate rejects ""`)
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

// A hand-written decoder that wraps a nested error with %w or errors.Join
// keeps its wrapper, and the *ParseError inside gets the outer segment and a
// payload offset, the same as a bare one.
func TestParseErrShiftReachesWrappedErrors(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("context")
	wraps := map[string]func(error) error{
		"bare": func(e error) error { return e },
		"%w":   func(e error) error { return fmt.Errorf("wrapped: %w", e) },
		"join": func(e error) error { return errors.Join(sentinel, e) },
	}
	for name, wrap := range wraps {
		in := wrap(&ParseError{Keys: []string{"value"}, Pos: 3, Err: ErrBadNumber})
		out := NewParseErrShift("w", 150, 50, in)
		if out != in {
			t.Errorf("%s: returned %v, want the callee's error itself", name, out)
		}
		pe, ok := errors.AsType[*ParseError](out)
		if !ok || pe.Pos != 103 || strings.Join(pe.Keys, ".") != "w.value" {
			t.Errorf("%s: got %+v, want w.value at 103", name, pe)
		}
	}
	mod := NewParseErr("value", 3, &ModError{Name: "m", Pos: 3})
	out := NewParseErrShift("w", 150, 50, fmt.Errorf("wrapped: %w", mod))
	if me, ok := errors.AsType[*ModError](out); !ok || me.Pos != 103 {
		t.Errorf("wrapped ModError = %+v, want Pos 103", me)
	}
	le := NewParseErrShift("w", 150, 50, fmt.Errorf("wrapped: %w", &LenError{Pos: 3}))
	if e, ok := errors.AsType[*LenError](le); !ok || e.Pos != 103 {
		t.Errorf("wrapped LenError = %+v, want Pos 103", e)
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

// A path grows in front without being copied at every level, and relocate
// never writes through keys it does not own: a slice two errors share, or the
// view of an error copied by value.
func TestRelocate_PathGrowsInFront(t *testing.T) {
	t.Parallel()
	backing := []string{"leaf", "canary"}
	a := &LenError{Keys: backing[:1]}
	b := &MinLenError{Keys: backing[:1]}
	for i := range 100 {
		relocate(a, "a"+strconv.Itoa(i), 1)
		relocate(b, "b"+strconv.Itoa(i), 1)
	}
	if backing[0] != "leaf" || backing[1] != "canary" {
		t.Errorf("shared keys written to: %v", backing)
	}
	if len(a.Keys) != 101 || a.Keys[0] != "a99" || a.Keys[99] != "a0" || a.Keys[100] != "leaf" || a.Pos != 100 {
		t.Errorf("a = %v at %d", a.Keys, a.Pos)
	}
	if len(b.Keys) != 101 || b.Keys[0] != "b99" || b.Keys[100] != "leaf" {
		t.Errorf("b = %v", b.Keys)
	}

	c := *a
	relocate(a, "x", 0)
	relocate(&c, "y", 0)
	if a.Keys[0] != "x" || a.Keys[1] != "a99" || c.Keys[0] != "y" || c.Keys[1] != "a99" || len(a.Keys) != 102 || len(c.Keys) != 102 {
		t.Errorf("copy shares the path: a = %v, c = %v", a.Keys[:2], c.Keys[:2])
	}

	pe := &ParseError{Keys: backing[:1], Err: ErrBadString}
	for range 100 {
		NewParseErr("p", 0, pe)
	}
	if len(pe.Keys) != 101 || pe.Keys[0] != "p" || pe.Keys[100] != "leaf" || backing[1] != "canary" {
		t.Errorf("pe = %v, backing = %v", pe.Keys, backing)
	}
}

func TestRelocate_PathAllocs(t *testing.T) { //nolint:paralleltest // measures allocations
	allocs := testing.AllocsPerRun(10, func() {
		e := &LenError{Keys: []string{"leaf"}}
		for range 4096 {
			relocate(e, "s", 0)
		}
	})
	if allocs > 32 {
		t.Errorf("%v allocations for a path of 4096 segments", allocs)
	}
}
