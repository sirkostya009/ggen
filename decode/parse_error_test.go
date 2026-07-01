package decode

import (
	"errors"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/scan"
	"github.com/sirkostya009/ggen/validation"
)

func TestParseError_ErrorString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		pe   *ParseError
		want string
	}{
		{"with field", &ParseError{Path: []string{"addr", "street"}, Pos: 42, Err: scan.ErrBadString}, "parse error at addr.street (pos 42): " + scan.ErrBadString.Error()},
		{"no field", &ParseError{Pos: 7, Err: scan.ErrBadObject}, "parse error (pos 7): " + scan.ErrBadObject.Error()},
		{"zero pos", &ParseError{Path: []string{"x"}, Err: scan.ErrBadNumber}, "parse error at x (pos 0): " + scan.ErrBadNumber.Error()},
		{"nil err", &ParseError{Path: []string{"y"}, Pos: 1}, "parse error at y (pos 1)"},
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
	pe := &ParseError{Err: scan.ErrBadString}
	if got := pe.Unwrap(); got != scan.ErrBadString {
		t.Errorf("Unwrap() = %v, want %v", got, scan.ErrBadString)
	}
	if !errors.Is(pe, scan.ErrBadString) {
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
		&validation.MinLenError{Path: []string{"x"}, Limit: 5, Got: 1},
		validation.Errors{&validation.RequiredError{Path: []string{"x"}}},
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
	got := NewParseErr("name", 12, scan.ErrBadString)
	var pe *ParseError
	if !errors.As(got, &pe) {
		t.Fatalf("expected wrap, got %T", got)
	}
	if len(pe.Path) != 1 || pe.Path[0] != "name" || pe.Pos != 12 || pe.Err != scan.ErrBadString {
		t.Errorf("ParseError = %+v", pe)
	}
}

func TestNewParseErr_ChainPrefixesField(t *testing.T) {
	t.Parallel()
	// Inner already wrapped; outer prepends its segment onto the Path.
	inner := &ParseError{Path: []string{"zip"}, Pos: 7, Err: scan.ErrBadNumber}
	got := NewParseErr("addr", 99, inner)
	var pe *ParseError
	if !errors.As(got, &pe) {
		t.Fatal("expected *ParseError")
	}
	if pe != inner {
		t.Error("wrap should reuse inner, not allocate new")
	}
	if strings.Join(pe.Path, ".") != "addr.zip" {
		t.Errorf("Path = %v, want [addr zip]", pe.Path)
	}
	// Pos stays at the deeper site.
	if pe.Pos != 7 {
		t.Errorf("Pos = %d, want 7 (deepest site)", pe.Pos)
	}
}

func TestNewParseErr_ChainEmptyFieldNoChange(t *testing.T) {
	t.Parallel()
	inner := &ParseError{Path: []string{"zip"}, Err: scan.ErrBadNumber}
	_ = NewParseErr("", 0, inner)
	if strings.Join(inner.Path, ".") != "zip" {
		t.Errorf("Path mutated to %v on empty outer field", inner.Path)
	}
}

func TestNewParseErr_ChainEmptyInnerField(t *testing.T) {
	t.Parallel()
	inner := &ParseError{Pos: 3, Err: scan.ErrBadNumber}
	_ = NewParseErr("addr", 0, inner)
	if strings.Join(inner.Path, ".") != "addr" {
		t.Errorf("Path = %v, want [addr]", inner.Path)
	}
}

// trivial Decoder implementation used to drive UnmarshalSlice without
// pulling generated code into the decode package.
type stubElem struct{}

func (stubElem) DecodeFrom(data []byte) (stubElem, int, error) {
	if len(data) > 0 && data[0] == '{' {
		end := strings.IndexByte(string(data), '}')
		if end < 0 {
			return stubElem{}, 0, scan.ErrBadObject
		}
		return stubElem{}, end + 1, nil
	}
	return stubElem{}, 0, scan.ErrBadObject
}

func (stubElem) DecodeFromStream(s *scan.Stream) (stubElem, error) {
	return stubElem{}, nil
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
	if strings.Join(pe.Path, ".") != "[]" {
		t.Errorf("Path = %v, want [\"[]\"]", pe.Path)
	}
	if !errors.Is(err, scan.ErrBadArray) {
		t.Errorf("errors.Is(err, scan.ErrBadArray) = false: %v", err)
	}
}

func TestUnmarshalSlice_ElementErrorCarriesIndex(t *testing.T) {
	t.Parallel()
	// Element 0 OK ({}), element 1 errors (stubElem requires '{'). The
	// failing element's index should appear in Field.
	_, err := UnmarshalSlice[stubElem]([]byte(`[{},BAD]`))
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %T, want *ParseError", err)
	}
	if strings.Join(pe.Path, ".") != "[1]" {
		t.Errorf("Path = %v, want [\"[1]\"]", pe.Path)
	}
}
