// Package validation holds typed validation errors emitted by ggen-generated
// parsers. Hard parse errors are wrapped in [decode.ParseError]; only
// rule-level failures land here. Each rule has its own concrete error type;
// type-switch via errors.As to inspect specific failures.
//
// In multierr mode the decoder returns [Errors], a flat slice of failures.
// Every leaf carries its own root-relative Path; nested-struct decodes have
// the outer segment prepended via [Append].
package validation

import (
	"fmt"
	"strings"
	"unsafe"
)

// Error is satisfied by every validation failure type.
type Error interface {
	error
	Rule() Rule
	prependPath(segment string)
}

// prepend returns p with segment as the new head, on a fresh backing array
// so leaf-shared slices don't corrupt siblings.
func prepend(p []string, segment string) []string {
	out := make([]string, len(p)+1)
	out[0] = segment
	copy(out[1:], p)
	return out
}

// Rule identifies which validation rule failed.
type Rule string

// Known rule identifiers, matching the keyword in the `pipe` struct tag.
const (
	Required     Rule = "required"
	NotEmpty     Rule = "notempty"
	Len          Rule = "len"
	MinLen       Rule = "minlen"
	MaxLen       Rule = "maxlen"
	Runes        Rule = "runes"
	MinRunes     Rule = "minrunes"
	MaxRunes     Rule = "maxrunes"
	GT           Rule = "gt"
	GTE          Rule = "gte"
	LT           Rule = "lt"
	LTE          Rule = "lte"
	Eq           Rule = "eq"
	Neq          Rule = "neq"
	OneOf        Rule = "oneof"
	Email        Rule = "email"
	URL          Rule = "url"
	ASCII        Rule = "ascii"
	Printable    Rule = "printable"
	Alphanum     Rule = "alphanum"
	Numeric      Rule = "numeric"
	Lower        Rule = "lower"
	Upper        Rule = "upper"
	Hexadecimal  Rule = "hexadecimal"
	Starts       Rule = "starts"
	Ends         Rule = "ends"
	Contains     Rule = "contains"
	Multiple     Rule = "multiple"
	DuplicateKey Rule = "duplicate"
	UnknownKey   Rule = "unknown"
	Custom       Rule = "custom"
	Predicate    Rule = "predicate"
	MultiErr     Rule = "multierr"
)

// --- presence ---

type RequiredError struct {
	Pos  int
	Path []string
}

func (e *RequiredError) Error() string {
	return fmt.Sprintf("missing required field %q", strings.Join(e.Path, "."))
}
func (*RequiredError) Rule() Rule             { return Required }
func (e *RequiredError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type NotEmptyError struct {
	Pos  int
	Path []string
}

func (e *NotEmptyError) Error() string {
	return fmt.Sprintf("%s: must not be empty", strings.Join(e.Path, "."))
}
func (*NotEmptyError) Rule() Rule             { return NotEmpty }
func (e *NotEmptyError) prependPath(s string) { e.Path = prepend(e.Path, s) }

// --- length ---

type LenError struct {
	Pos  int
	Path []string
	Want int
	Got  int
}

func (e *LenError) Error() string {
	return fmt.Sprintf("%s: length %d != required %d", strings.Join(e.Path, "."), e.Got, e.Want)
}
func (*LenError) Rule() Rule             { return Len }
func (e *LenError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type MinLenError struct {
	Pos   int
	Path  []string
	Limit int
	Got   int
}

func (e *MinLenError) Error() string {
	return fmt.Sprintf("%s: length %d below minimum %d", strings.Join(e.Path, "."), e.Got, e.Limit)
}
func (*MinLenError) Rule() Rule             { return MinLen }
func (e *MinLenError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type MaxLenError struct {
	Pos   int
	Path  []string
	Limit int
	Got   int
}

func (e *MaxLenError) Error() string {
	return fmt.Sprintf("%s: length %d exceeds maximum %d", strings.Join(e.Path, "."), e.Got, e.Limit)
}
func (*MaxLenError) Rule() Rule             { return MaxLen }
func (e *MaxLenError) prependPath(s string) { e.Path = prepend(e.Path, s) }

// --- runes ---

type RunesError struct {
	Pos  int
	Path []string
	Want int
	Got  int
}

func (e *RunesError) Error() string {
	return fmt.Sprintf("%s: rune count %d != required %d", strings.Join(e.Path, "."), e.Got, e.Want)
}
func (*RunesError) Rule() Rule             { return Runes }
func (e *RunesError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type MinRunesError struct {
	Pos   int
	Path  []string
	Limit int
	Got   int
}

func (e *MinRunesError) Error() string {
	return fmt.Sprintf("%s: rune count %d below minimum %d", strings.Join(e.Path, "."), e.Got, e.Limit)
}
func (*MinRunesError) Rule() Rule             { return MinRunes }
func (e *MinRunesError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type MaxRunesError struct {
	Pos   int
	Path  []string
	Limit int
	Got   int
}

func (e *MaxRunesError) Error() string {
	return fmt.Sprintf("%s: rune count %d exceeds maximum %d", strings.Join(e.Path, "."), e.Got, e.Limit)
}
func (*MaxRunesError) Rule() Rule             { return MaxRunes }
func (e *MaxRunesError) prependPath(s string) { e.Path = prepend(e.Path, s) }

// --- numeric range. Limit is float64; Value holds the originating numeric type.

type GTError struct {
	Pos   int
	Path  []string
	Limit float64
	Value any
}

func (e *GTError) Error() string {
	return fmt.Sprintf("%s: value %v not greater than %v", strings.Join(e.Path, "."), e.Value, e.Limit)
}
func (*GTError) Rule() Rule             { return GT }
func (e *GTError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type GTEError struct {
	Pos   int
	Path  []string
	Limit float64
	Value any
}

func (e *GTEError) Error() string {
	return fmt.Sprintf("%s: value %v below minimum %v", strings.Join(e.Path, "."), e.Value, e.Limit)
}
func (*GTEError) Rule() Rule             { return GTE }
func (e *GTEError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type LTError struct {
	Pos   int
	Path  []string
	Limit float64
	Value any
}

func (e *LTError) Error() string {
	return fmt.Sprintf("%s: value %v not less than %v", strings.Join(e.Path, "."), e.Value, e.Limit)
}
func (*LTError) Rule() Rule             { return LT }
func (e *LTError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type LTEError struct {
	Pos   int
	Path  []string
	Limit float64
	Value any
}

func (e *LTEError) Error() string {
	return fmt.Sprintf("%s: value %v exceeds maximum %v", strings.Join(e.Path, "."), e.Value, e.Limit)
}
func (*LTEError) Rule() Rule             { return LTE }
func (e *LTEError) prependPath(s string) { e.Path = prepend(e.Path, s) }

// --- equality. Want/Value are `any` to cover both string and numeric fields.

type EqError struct {
	Pos   int
	Path  []string
	Want  any
	Value any
}

func (e *EqError) Error() string {
	return fmt.Sprintf("%s: value %v != %v", strings.Join(e.Path, "."), e.Value, e.Want)
}
func (*EqError) Rule() Rule             { return Eq }
func (e *EqError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type NeqError struct {
	Pos   int
	Path  []string
	Want  any
	Value any
}

func (e *NeqError) Error() string {
	return fmt.Sprintf("%s: value must not equal %v", strings.Join(e.Path, "."), e.Want)
}
func (*NeqError) Rule() Rule             { return Neq }
func (e *NeqError) prependPath(s string) { e.Path = prepend(e.Path, s) }

// --- oneof. Allowed points to a frozen package-level slice (not owned by the error).

type OneOfError struct {
	Pos     int
	Path    []string
	Allowed []string
	Value   any
}

func (e *OneOfError) Error() string {
	return fmt.Sprintf("%s: value %v not in allowed set [%s]", strings.Join(e.Path, "."), e.Value,
		strings.Join(e.Allowed, ", "))
}
func (*OneOfError) Rule() Rule             { return OneOf }
func (e *OneOfError) prependPath(s string) { e.Path = prepend(e.Path, s) }

// --- format predicates ---

type EmailError struct {
	Pos   int
	Path  []string
	Value string
}

func (e *EmailError) Error() string {
	return fmt.Sprintf("%s: %q is not a valid email", strings.Join(e.Path, "."), e.Value)
}
func (*EmailError) Rule() Rule             { return Email }
func (e *EmailError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type URLError struct {
	Pos   int
	Path  []string
	Value string
	Cause error
}

func (e *URLError) Error() string {
	return fmt.Sprintf("%s: %q is not a valid url", strings.Join(e.Path, "."), e.Value)
}
func (*URLError) Rule() Rule             { return URL }
func (e *URLError) Unwrap() error        { return e.Cause }
func (e *URLError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type ASCIIError struct {
	Pos   int
	Path  []string
	Value string
}

func (e *ASCIIError) Error() string {
	return fmt.Sprintf("%s: %q contains non-ASCII bytes", strings.Join(e.Path, "."), e.Value)
}
func (*ASCIIError) Rule() Rule             { return ASCII }
func (e *ASCIIError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type PrintableError struct {
	Pos   int
	Path  []string
	Value string
}

func (e *PrintableError) Error() string {
	return fmt.Sprintf("%s: %q contains non-printable bytes", strings.Join(e.Path, "."), e.Value)
}
func (*PrintableError) Rule() Rule             { return Printable }
func (e *PrintableError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type AlphanumError struct {
	Pos   int
	Path  []string
	Value string
}

func (e *AlphanumError) Error() string {
	return fmt.Sprintf("%s: %q must be alphanumeric", strings.Join(e.Path, "."), e.Value)
}
func (*AlphanumError) Rule() Rule             { return Alphanum }
func (e *AlphanumError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type NumericError struct {
	Pos   int
	Path  []string
	Value string
}

func (e *NumericError) Error() string {
	return fmt.Sprintf("%s: %q must be all digits", strings.Join(e.Path, "."), e.Value)
}
func (*NumericError) Rule() Rule             { return Numeric }
func (e *NumericError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type LowerError struct {
	Pos   int
	Path  []string
	Value string
}

func (e *LowerError) Error() string {
	return fmt.Sprintf("%s: %q contains uppercase letters", strings.Join(e.Path, "."), e.Value)
}
func (*LowerError) Rule() Rule             { return Lower }
func (e *LowerError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type UpperError struct {
	Pos   int
	Path  []string
	Value string
}

func (e *UpperError) Error() string {
	return fmt.Sprintf("%s: %q contains lowercase letters", strings.Join(e.Path, "."), e.Value)
}
func (*UpperError) Rule() Rule             { return Upper }
func (e *UpperError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type HexadecimalError struct {
	Pos   int
	Path  []string
	Value string
}

func (e *HexadecimalError) Error() string {
	return fmt.Sprintf("%s: %q is not hexadecimal", strings.Join(e.Path, "."), e.Value)
}
func (*HexadecimalError) Rule() Rule             { return Hexadecimal }
func (e *HexadecimalError) prependPath(s string) { e.Path = prepend(e.Path, s) }

// --- prefix/suffix/contains ---

type StartsError struct {
	Pos         int
	Path        []string
	Want, Value string
}

func (e *StartsError) Error() string {
	return fmt.Sprintf("%s: %q does not start with %q", strings.Join(e.Path, "."), e.Value, e.Want)
}
func (*StartsError) Rule() Rule             { return Starts }
func (e *StartsError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type EndsError struct {
	Pos         int
	Path        []string
	Want, Value string
}

func (e *EndsError) Error() string {
	return fmt.Sprintf("%s: %q does not end with %q", strings.Join(e.Path, "."), e.Value, e.Want)
}
func (*EndsError) Rule() Rule             { return Ends }
func (e *EndsError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type ContainsError struct {
	Pos         int
	Path        []string
	Want, Value string
}

func (e *ContainsError) Error() string {
	return fmt.Sprintf("%s: %q does not contain %q", strings.Join(e.Path, "."), e.Value, e.Want)
}
func (*ContainsError) Rule() Rule             { return Contains }
func (e *ContainsError) prependPath(s string) { e.Path = prepend(e.Path, s) }

// --- multiple ---

type MultipleError struct {
	Pos   int
	Path  []string
	Of    float64
	Value any
}

func (e *MultipleError) Error() string {
	return fmt.Sprintf("%s: %v is not a multiple of %v", strings.Join(e.Path, "."), e.Value, e.Of)
}
func (*MultipleError) Rule() Rule             { return Multiple }
func (e *MultipleError) prependPath(s string) { e.Path = prepend(e.Path, s) }

// --- key violations ---

type DuplicateKeyError struct {
	Pos  int
	Path []string
}

func (e *DuplicateKeyError) Error() string {
	return fmt.Sprintf("duplicate key %q", strings.Join(e.Path, "."))
}
func (*DuplicateKeyError) Rule() Rule             { return DuplicateKey }
func (e *DuplicateKeyError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type UnknownKeyError struct {
	Pos  int
	Path []string
}

func (e *UnknownKeyError) Error() string {
	return fmt.Sprintf("unknown key %q", strings.Join(e.Path, "."))
}
func (*UnknownKeyError) Rule() Rule             { return UnknownKey }
func (e *UnknownKeyError) prependPath(s string) { e.Path = prepend(e.Path, s) }

// --- custom ---

// CustomError is the failure of a custom error-form validator (`func(T) error`).
// Name is the func identifier (no `@`); Value is the rejected input; Cause is
// the error the validator returned.
type CustomError struct {
	Pos   int
	Path  []string
	Name  string
	Value any
	Cause error
}

func (e *CustomError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s (%s): %v", strings.Join(e.Path, "."), e.Name, e.Cause)
	}
	return fmt.Sprintf("%s: validation %q failed", strings.Join(e.Path, "."), e.Name)
}
func (*CustomError) Rule() Rule             { return Custom }
func (e *CustomError) Unwrap() error        { return e.Cause }
func (e *CustomError) prependPath(s string) { e.Path = prepend(e.Path, s) }

// PredicateError is the failure for a custom bool-form validator
// (`func(T) bool`). Name is the func identifier; Msg is the optional inline tag
// message (empty renders a default); Value is the rejected input.
type PredicateError struct {
	Pos   int
	Path  []string
	Name  string
	Msg   string
	Value any
}

func (e *PredicateError) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("%s: %s", strings.Join(e.Path, "."), e.Msg)
	}
	return fmt.Sprintf("%s: %q rejected %v", strings.Join(e.Path, "."), e.Name, e.Value)
}
func (*PredicateError) Rule() Rule             { return Predicate }
func (e *PredicateError) prependPath(s string) { e.Path = prepend(e.Path, s) }

// ModError is the failure of a fallible bool-form mod/converter
// (`func(W) (T, bool)`) whose bool was false. It is a parse error, wrapped by
// decode.NewParseErr. Msg is the optional inline tag message (empty renders a
// default).
type ModError struct {
	Pos   int
	Name  string
	Msg   string
	Value any
}

func (e *ModError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return fmt.Sprintf("transform %q rejected %v", e.Name, e.Value)
}

// --- aggregate ---

// Errors is a flat slice of validation failures from a multierr decoder.
// Each entry's Path is root-relative. Implements error and Unwrap() []error so
// errors.Is/As walk every leaf.
type Errors []Error

func (es Errors) Error() string {
	if len(es) == 0 {
		return ""
	}
	var buf []byte
	buf = append(buf, es[0].Error()...)
	for _, e := range es[1:] {
		buf = append(buf, "; "...)
		buf = append(buf, e.Error()...)
	}
	return unsafe.String(&buf[0], len(buf))
}

func (Errors) Rule() Rule { return MultiErr }

// prependPath propagates the segment into every leaf.
func (es Errors) prependPath(segment string) {
	for _, e := range es {
		e.prependPath(segment)
	}
}

func (es Errors) Unwrap() []error {
	out := make([]error, len(es))
	for i := range es {
		out[i] = es[i]
	}
	return out
}

// Append adds inner to es, prepending segment to its path. A nested [Errors]
// is flattened in.
func (es *Errors) Append(segment string, inner Error) {
	if nested, ok := inner.(Errors); ok {
		nested.prependPath(segment)
		*es = append(*es, nested...)
		return
	}
	*es = append(*es, inner)
}
