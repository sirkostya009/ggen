package ggen

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// intT is a minimal Decoder over []byte for the slice walker tests.
type intT int64

func (intT) DecodeFrom(data []byte) (intT, int, error) {
	v, n, err := Int64(data, 0)
	return intT(v), n, err
}

// TestUnmarshalSliceEmptyNonNil checks that [] decodes to a non-nil empty
// slice. Generated slice fields and jsonv2 behave the same way. The Stream
// version of this check is in TestStreamMethods.
func TestUnmarshalSliceEmptyNonNil(t *testing.T) {
	t.Parallel()
	got, err := UnmarshalSlice[intT]([]byte(" [ ] "))
	if err != nil {
		t.Fatalf("UnmarshalSlice: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("UnmarshalSlice([]) = %#v, want non-nil empty", got)
	}
	if got, err := UnmarshalSlice[intT]([]byte("[1,2]")); err != nil || len(got) != 2 {
		t.Errorf("UnmarshalSlice([1,2]) = %v, %v", got, err)
	}
}

// testElem is a minimal Decoder[T] for the slice walker tests.
type testElem string

func (testElem) DecodeFrom(data []byte) (testElem, int, error) {
	v, n, err := String(data, 0, true)
	return testElem(v), n, err
}

// UnmarshalSlice must reject anything but whitespace after the closing ']'.
// It consumes the whole input like jsonv2, so the caller has no other way to
// notice the leftover bytes.
func TestUnmarshalSlice_TrailingGarbage(t *testing.T) {
	t.Parallel()
	for _, in := range []string{`[]x`, `[] ]`, `["a"]]]`, `["a"] {"junk":1}`} {
		if _, err := UnmarshalSlice[testElem]([]byte(in)); !errors.Is(err, ErrTrailingData) {
			t.Errorf("%q: got %v, want ErrTrailingData", in, err)
		}
	}
	for _, in := range []string{`[]`, ` [ ] `, `["a","b"]`, `["a"] `} {
		if _, err := UnmarshalSlice[testElem]([]byte(in)); err != nil {
			t.Errorf("%q: unexpected error %v", in, err)
		}
	}
}

func TestParseError_ErrorString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		pe   *ParseError
		want string
	}{
		{
			"with field",
			&ParseError{Keys: []string{"addr", "street"}, Pos: 42, Err: ErrBadString},
			"parse error at addr.street (pos 42): " + ErrBadString.Error(),
		},
		{"no field", &ParseError{Pos: 7, Err: ErrBadObject}, "parse error (pos 7): " + ErrBadObject.Error()},
		{"zero pos", &ParseError{Keys: []string{"x"}, Err: ErrBadNumber}, "parse error at x (pos 0): " + ErrBadNumber.Error()},
		{"nil err", &ParseError{Keys: []string{"y"}, Pos: 1}, "parse error at y (pos 1)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := c.pe.Error(); got != c.want {
				t.Errorf("Error() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestParseError_Unwrap(t *testing.T) {
	t.Parallel()
	pe := &ParseError{Err: ErrBadString}
	if got := pe.Unwrap(); got != ErrBadString {
		t.Errorf("Unwrap() = %v, want %v", got, ErrBadString)
	}
	if !errors.Is(pe, ErrBadString) {
		t.Error("errors.Is should propagate through Unwrap")
	}
}

func TestNewParseErr_NilErr(t *testing.T) {
	t.Parallel()
	if got := NewParseErr("x", 1, nil); got != nil {
		t.Errorf("NewParseErr(_, _, nil) = %v, want nil", got)
	}
}

func TestNewParseErr_ValidationPassthrough(t *testing.T) {
	t.Parallel()
	cases := []error{
		&MinLenError{Keys: []string{"x"}, Limit: 5, Got: 1},
		Errors{&RequiredError{Keys: []string{"x"}}},
	}
	for i, in := range cases {
		got := NewParseErr("outer", 10, in)
		if _, ok := errors.AsType[*ParseError](got); ok {
			t.Errorf("case %d: validation error wrapped in ParseError: %v", i, got)
		}
	}
}

func TestNewParseErr_RawErrorWraps(t *testing.T) {
	t.Parallel()
	got := NewParseErr("name", 12, ErrBadString)
	var pe *ParseError
	if !errors.As(got, &pe) {
		t.Fatalf("expected wrap, got %T", got)
	}
	if len(pe.Keys) != 1 || pe.Keys[0] != "name" || pe.Pos != 12 || pe.Err != ErrBadString {
		t.Errorf("ParseError = %+v", pe)
	}
}

func TestNewParseErr_ChainPrefixesField(t *testing.T) {
	t.Parallel()
	// The inner error is already a *ParseError. The outer call prepends its
	// segment to the Path.
	inner := &ParseError{Keys: []string{"zip"}, Pos: 7, Err: ErrBadNumber}
	got := NewParseErr("addr", 99, inner)
	var pe *ParseError
	if !errors.As(got, &pe) {
		t.Fatal("expected *ParseError")
	}
	if pe != inner {
		t.Error("wrap should reuse inner, not allocate new")
	}
	if strings.Join(pe.Keys, ".") != "addr.zip" {
		t.Errorf("Path = %v, want [addr zip]", pe.Keys)
	}
	// Pos stays at the innermost error site.
	if pe.Pos != 7 {
		t.Errorf("Pos = %d, want 7 (deepest site)", pe.Pos)
	}
}

func TestNewParseErr_ChainEmptyFieldNoChange(t *testing.T) {
	t.Parallel()
	inner := &ParseError{Keys: []string{"zip"}, Err: ErrBadNumber}
	_ = NewParseErr("", 0, inner)
	if strings.Join(inner.Keys, ".") != "zip" {
		t.Errorf("Path mutated to %v on empty outer field", inner.Keys)
	}
}

func TestNewParseErr_ChainEmptyInnerField(t *testing.T) {
	t.Parallel()
	inner := &ParseError{Pos: 3, Err: ErrBadNumber}
	_ = NewParseErr("addr", 0, inner)
	if strings.Join(inner.Keys, ".") != "addr" {
		t.Errorf("Path = %v, want [addr]", inner.Keys)
	}
}

// stubElem is a tiny Decoder that accepts only "{...}". It drives
// UnmarshalSlice without needing generated code.
type stubElem struct{}

func (stubElem) DecodeFrom(data []byte) (stubElem, int, error) {
	if len(data) > 0 && data[0] == '{' {
		end := bytes.IndexByte(data, '}')
		if end < 0 {
			return stubElem{}, 0, ErrBadObject
		}
		return stubElem{}, end + 1, nil
	}
	return stubElem{}, 0, ErrBadObject
}

func TestUnmarshalSlice_WrapsBracketError(t *testing.T) {
	t.Parallel()
	_, err := UnmarshalSlice[stubElem]([]byte(`not-an-array`))
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %T, want *ParseError", err)
	}
	if strings.Join(pe.Keys, ".") != "[]" {
		t.Errorf("Path = %v, want [\"[]\"]", pe.Keys)
	}
	if !errors.Is(err, ErrBadArray) {
		t.Errorf("errors.Is(err, ErrBadArray) = false: %v", err)
	}
}

func TestUnmarshalSlice_ElementErrorCarriesIndex(t *testing.T) {
	t.Parallel()
	// Element 0 decodes. Element 1 fails because stubElem requires '{'. The
	// failing element's index must appear in Path.
	_, err := UnmarshalSlice[stubElem]([]byte(`[{},BAD]`))
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %T, want *ParseError", err)
	}
	if strings.Join(pe.Keys, ".") != "[1]" {
		t.Errorf("Path = %v, want [\"[1]\"]", pe.Keys)
	}
}

// TestMaxDepth checks the nesting limit on every recursive runtime path:
// SkipValue and the Any functions, for both []byte input and Stream. Without
// the limit, a few MB of "[[[[" overflows the goroutine stack. That is a
// fatal crash that recover cannot catch. jsonv2 has a similar limit.
func TestMaxDepth(t *testing.T) {
	t.Parallel()
	deep := func(n int) []byte {
		return []byte(strings.Repeat("[", n) + strings.Repeat("]", n))
	}
	deepObj := func(n int) []byte {
		return []byte(strings.Repeat(`{"k":`, n) + "1" + strings.Repeat("}", n))
	}
	over := deep(maxDepth + 1)
	overObj := deepObj(maxDepth + 1)
	under := deep(maxDepth)

	if _, err := SkipValue(over, 0); !errors.Is(err, ErrMaxDepth) {
		t.Errorf("SkipValue deep array: want ErrMaxDepth, got %v", err)
	}
	if _, err := SkipValue(overObj, 0); !errors.Is(err, ErrMaxDepth) {
		t.Errorf("SkipValue deep object: want ErrMaxDepth, got %v", err)
	}
	if j, err := SkipValue(under, 0); err != nil || j != len(under) {
		t.Errorf("SkipValue at cap: err=%v j=%d", err, j)
	}
	if _, _, err := Any(over, 0, true); !errors.Is(err, ErrMaxDepth) {
		t.Errorf("Any deep array: want ErrMaxDepth, got %v", err)
	}
	if _, _, err := AnyNumber(overObj, 0, true); !errors.Is(err, ErrMaxDepth) {
		t.Errorf("AnyNumber deep object: want ErrMaxDepth, got %v", err)
	}
	if _, _, err := AnyCopy(over, 0, true); !errors.Is(err, ErrMaxDepth) {
		t.Errorf("AnyCopy deep array: want ErrMaxDepth, got %v", err)
	}
	if _, _, err := AnyNumberCopy(over, 0, true); !errors.Is(err, ErrMaxDepth) {
		t.Errorf("AnyNumberCopy deep array: want ErrMaxDepth, got %v", err)
	}
	if v, _, err := Any(under, 0, true); err != nil || v == nil {
		t.Errorf("Any at cap: err=%v", err)
	}

	var s Stream
	s.Reset(strings.NewReader(string(over)), make([]byte, 0, 1024))
	if err := s.SkipValue(); !errors.Is(err, ErrMaxDepth) {
		t.Errorf("stream SkipValue: want ErrMaxDepth, got %v", err)
	}
	s.Reset(strings.NewReader(string(over)), make([]byte, 0, 1024))
	if _, err := s.Any(true); !errors.Is(err, ErrMaxDepth) {
		t.Errorf("stream Any: want ErrMaxDepth, got %v", err)
	}
	s.Reset(strings.NewReader(string(overObj)), make([]byte, 0, 1024))
	if _, err := s.AnyNumber(true); !errors.Is(err, ErrMaxDepth) {
		t.Errorf("stream AnyNumber: want ErrMaxDepth, got %v", err)
	}
	s.Reset(strings.NewReader(string(under)), make([]byte, 0, 1024))
	if err := s.SkipValue(); err != nil {
		t.Errorf("stream SkipValue at cap: %v", err)
	}
}

// TestBoolEnd_GiveUpPosition checks the error position for a broken true or
// false literal. Bool reports the start of the literal, which keeps it small
// enough to inline. BoolEnd finds where the scan stopped: the first byte that
// does not match, or len(data) when the input ends mid-literal. SkipValue and
// every Any function must report that BoolEnd position, the same way they do
// for a broken null.
func TestBoolEnd_GiveUpPosition(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		i    int
		want int
	}{
		{"tru", 0, 3},
		{"trux", 0, 3},
		{"t", 0, 1},
		{"fals", 0, 4},
		{"falsy", 0, 4},
		{"f", 0, 1},
		{"fx", 0, 1},
		{"  tru", 2, 5},
		{"[fals", 1, 5},
		{"x", 0, 0},
		{"", 0, 0},
	}
	for _, tc := range cases {
		data := []byte(tc.in)
		if _, p, err := Bool(data, tc.i); err != ErrBadBool || p != tc.i {
			t.Errorf("Bool(%q, %d) = (%d, %v), want (%d, ErrBadBool)", tc.in, tc.i, p, err, tc.i)
		}
		if got := BoolEnd(data, tc.i); got != tc.want {
			t.Errorf("BoolEnd(%q, %d) = %d, want %d", tc.in, tc.i, got, tc.want)
		}
		if tc.i >= len(data) || (data[tc.i] != 't' && data[tc.i] != 'f') {
			continue // SkipValue and Any never call Bool here
		}
		if p, err := SkipValue(data, tc.i); err != ErrBadBool || p != tc.want {
			t.Errorf("SkipValue(%q, %d) = (%d, %v), want (%d, ErrBadBool)", tc.in, tc.i, p, err, tc.want)
		}
		for name, fn := range map[string]func([]byte, int, bool) (any, int, error){
			"Any": Any, "AnyNumber": AnyNumber, "AnyCopy": AnyCopy, "AnyNumberCopy": AnyNumberCopy,
		} {
			if _, p, err := fn(data, tc.i, true); err != ErrBadBool || p != tc.want {
				t.Errorf("%s(%q, %d) = (%d, %v), want (%d, ErrBadBool)", name, tc.in, tc.i, p, err, tc.want)
			}
		}
	}
	for _, in := range []string{"nul", "nulx"} {
		sp, _ := SkipValue([]byte(in), 0)
		if _, ap, _ := Any([]byte(in), 0, true); ap != sp {
			t.Errorf("Any(%q) pos=%d, SkipValue pos=%d", in, ap, sp)
		}
	}
}
