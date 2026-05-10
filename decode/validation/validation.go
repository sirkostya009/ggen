// Package validation holds typed validation errors emitted by ggen-generated
// parsers.
//
// Hard parse errors (malformed JSON, type mismatches) are plain fmt.Errorf
// values returned immediately. Only rule-level validation failures go
// through this package. Each rule has its own concrete error type;
// callers can type-switch via errors.As to inspect specific failures.
//
// In multierr mode, every failing rule appends an [Error] to an [Errors]
// slice that is returned as the final error; hard errors still return
// immediately.
package validation

import (
	"fmt"
	"strings"
	"unsafe"
)

// Error is the interface satisfied by every validation failure type in
// this package.
type Error interface {
	error
	Rule() Rule
}

// Rule identifies which validation rule failed.
type Rule string

// Known rule identifiers. Each matches the corresponding keyword in the
// `ggen` struct tag.
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
)

// --- presence ---

type RequiredError struct{ Field string }

func (e *RequiredError) Error() string { return fmt.Sprintf("missing required field %q", e.Field) }
func (*RequiredError) Rule() Rule      { return Required }

type NotEmptyError struct{ Field string }

func (e *NotEmptyError) Error() string { return fmt.Sprintf("%s: must not be empty", e.Field) }
func (*NotEmptyError) Rule() Rule      { return NotEmpty }

// --- length ---

type LenError struct {
	Field string
	Want  int
	Got   int
}

func (e *LenError) Error() string {
	return fmt.Sprintf("%s: length %d != required %d", e.Field, e.Got, e.Want)
}
func (*LenError) Rule() Rule { return Len }

type MinLenError struct {
	Field string
	Limit int
	Got   int
}

func (e *MinLenError) Error() string {
	return fmt.Sprintf("%s: length %d below minimum %d", e.Field, e.Got, e.Limit)
}
func (*MinLenError) Rule() Rule { return MinLen }

type MaxLenError struct {
	Field string
	Limit int
	Got   int
}

func (e *MaxLenError) Error() string {
	return fmt.Sprintf("%s: length %d exceeds maximum %d", e.Field, e.Got, e.Limit)
}
func (*MaxLenError) Rule() Rule { return MaxLen }

// --- runes ---

type RunesError struct {
	Field string
	Want  int
	Got   int
}

func (e *RunesError) Error() string {
	return fmt.Sprintf("%s: rune count %d != required %d", e.Field, e.Got, e.Want)
}
func (*RunesError) Rule() Rule { return Runes }

type MinRunesError struct {
	Field string
	Limit int
	Got   int
}

func (e *MinRunesError) Error() string {
	return fmt.Sprintf("%s: rune count %d below minimum %d", e.Field, e.Got, e.Limit)
}
func (*MinRunesError) Rule() Rule { return MinRunes }

type MaxRunesError struct {
	Field string
	Limit int
	Got   int
}

func (e *MaxRunesError) Error() string {
	return fmt.Sprintf("%s: rune count %d exceeds maximum %d", e.Field, e.Got, e.Limit)
}
func (*MaxRunesError) Rule() Rule { return MaxRunes }

// --- numeric range. Limit kept as float64 to cover int/uint/float fields
// with a single struct shape. Value is `any` to preserve the originating
// numeric type for inspection (boxing only happens on the failure path).

type GTError struct {
	Field string
	Limit float64
	Value any
}

func (e *GTError) Error() string {
	return fmt.Sprintf("%s: value %v not greater than %v", e.Field, e.Value, e.Limit)
}
func (*GTError) Rule() Rule { return GT }

type GTEError struct {
	Field string
	Limit float64
	Value any
}

func (e *GTEError) Error() string {
	return fmt.Sprintf("%s: value %v below minimum %v", e.Field, e.Value, e.Limit)
}
func (*GTEError) Rule() Rule { return GTE }

type LTError struct {
	Field string
	Limit float64
	Value any
}

func (e *LTError) Error() string {
	return fmt.Sprintf("%s: value %v not less than %v", e.Field, e.Value, e.Limit)
}
func (*LTError) Rule() Rule { return LT }

type LTEError struct {
	Field string
	Limit float64
	Value any
}

func (e *LTEError) Error() string {
	return fmt.Sprintf("%s: value %v exceeds maximum %v", e.Field, e.Value, e.Limit)
}
func (*LTEError) Rule() Rule { return LTE }

// --- equality. Want/Value typed `any`: same-shape struct for both
// string and numeric fields; the codegen picks the literal at the
// failure site.

type EqError struct {
	Field string
	Want  any
	Value any
}

func (e *EqError) Error() string {
	return fmt.Sprintf("%s: value %v != %v", e.Field, e.Value, e.Want)
}
func (*EqError) Rule() Rule { return Eq }

type NeqError struct {
	Field string
	Want  any
	Value any
}

func (e *NeqError) Error() string {
	return fmt.Sprintf("%s: value must not equal %v", e.Field, e.Want)
}
func (*NeqError) Rule() Rule { return Neq }

// --- oneof. Allowed is meant to point to a package-level "frozen" slice
// emitted by codegen — no per-error allocation.

type OneOfError struct {
	Field   string
	Allowed []string
	Value   any
}

func (e *OneOfError) Error() string {
	return fmt.Sprintf("%s: value %v not in allowed set [%s]", e.Field, e.Value,
		strings.Join(e.Allowed, ", "))
}
func (*OneOfError) Rule() Rule { return OneOf }

// --- format predicates ---

type EmailError struct{ Field, Value string }

func (e *EmailError) Error() string {
	return fmt.Sprintf("%s: %q is not a valid email", e.Field, e.Value)
}
func (*EmailError) Rule() Rule { return Email }

type URLError struct {
	Field, Value string
	Cause        error
}

func (e *URLError) Error() string {
	return fmt.Sprintf("%s: %q is not a valid url", e.Field, e.Value)
}
func (*URLError) Rule() Rule      { return URL }
func (e *URLError) Unwrap() error { return e.Cause }

type ASCIIError struct{ Field, Value string }

func (e *ASCIIError) Error() string {
	return fmt.Sprintf("%s: %q contains non-ASCII bytes", e.Field, e.Value)
}
func (*ASCIIError) Rule() Rule { return ASCII }

type PrintableError struct{ Field, Value string }

func (e *PrintableError) Error() string {
	return fmt.Sprintf("%s: %q contains non-printable bytes", e.Field, e.Value)
}
func (*PrintableError) Rule() Rule { return Printable }

type AlphanumError struct{ Field, Value string }

func (e *AlphanumError) Error() string {
	return fmt.Sprintf("%s: %q must be alphanumeric", e.Field, e.Value)
}
func (*AlphanumError) Rule() Rule { return Alphanum }

type NumericError struct{ Field, Value string }

func (e *NumericError) Error() string {
	return fmt.Sprintf("%s: %q must be all digits", e.Field, e.Value)
}
func (*NumericError) Rule() Rule { return Numeric }

type LowerError struct{ Field, Value string }

func (e *LowerError) Error() string {
	return fmt.Sprintf("%s: %q contains uppercase letters", e.Field, e.Value)
}
func (*LowerError) Rule() Rule { return Lower }

type UpperError struct{ Field, Value string }

func (e *UpperError) Error() string {
	return fmt.Sprintf("%s: %q contains lowercase letters", e.Field, e.Value)
}
func (*UpperError) Rule() Rule { return Upper }

type HexadecimalError struct{ Field, Value string }

func (e *HexadecimalError) Error() string {
	return fmt.Sprintf("%s: %q is not hexadecimal", e.Field, e.Value)
}
func (*HexadecimalError) Rule() Rule { return Hexadecimal }

// --- prefix/suffix/contains ---

type StartsError struct{ Field, Want, Value string }

func (e *StartsError) Error() string {
	return fmt.Sprintf("%s: %q does not start with %q", e.Field, e.Value, e.Want)
}
func (*StartsError) Rule() Rule { return Starts }

type EndsError struct{ Field, Want, Value string }

func (e *EndsError) Error() string {
	return fmt.Sprintf("%s: %q does not end with %q", e.Field, e.Value, e.Want)
}
func (*EndsError) Rule() Rule { return Ends }

type ContainsError struct{ Field, Want, Value string }

func (e *ContainsError) Error() string {
	return fmt.Sprintf("%s: %q does not contain %q", e.Field, e.Value, e.Want)
}
func (*ContainsError) Rule() Rule { return Contains }

// --- multiple ---

type MultipleError struct {
	Field string
	Of    float64
	Value any
}

func (e *MultipleError) Error() string {
	return fmt.Sprintf("%s: %v is not a multiple of %v", e.Field, e.Value, e.Of)
}
func (*MultipleError) Rule() Rule { return Multiple }

// --- key violations ---

type DuplicateKeyError struct{ Field string }

func (e *DuplicateKeyError) Error() string { return fmt.Sprintf("duplicate key %q", e.Field) }
func (*DuplicateKeyError) Rule() Rule      { return DuplicateKey }

type UnknownKeyError struct{ Field string }

func (e *UnknownKeyError) Error() string { return fmt.Sprintf("unknown key %q", e.Field) }
func (*UnknownKeyError) Rule() Rule      { return UnknownKey }

// --- custom ---

type CustomError struct {
	Field string
	Name  string
	Cause error
}

func (e *CustomError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s (%s): %v", e.Field, e.Name, e.Cause)
	}
	return fmt.Sprintf("%s: validation %q failed", e.Field, e.Name)
}
func (*CustomError) Rule() Rule      { return Custom }
func (e *CustomError) Unwrap() error { return e.Cause }

// --- aggregate ---

// Errors is a list of validation failures, returned from generated parsers in
// multierr mode. Implements error and the Unwrap() []error convention so
// errors.Is / errors.As / errors.Unwrap all work.
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

func (es Errors) Unwrap() []error {
	out := make([]error, len(es))
	for i, e := range es {
		out[i] = e
	}
	return out
}
