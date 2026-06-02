// Package validation holds typed validation errors emitted by ggen-generated
// parsers.
//
// Hard parse errors (malformed JSON, type mismatches) are wrapped in
// [decode.ParseError] and returned immediately. Only rule-level
// validation failures land in this package. Each rule has its own
// concrete error type; callers can type-switch via errors.As to
// inspect specific failures.
//
// In multierr mode the decoder returns [Errors] — a flat slice of
// validation failures. Every leaf carries its own Path as a slice of
// segments (Standard Schema convention): root-relative, with one entry
// per JSON object / array level. Nested-struct decodes have the outer
// segment prepended via [Append].
package validation

import (
	"fmt"
	"strings"
	"unsafe"
)

// Error is the interface satisfied by every validation failure type in
// this package. prependPath is private — only the codegen
// can mutate the leaf's Path.
type Error interface {
	error
	Rule() Rule
	prependPath(segment string)
}

// prepend returns p with segment as the new head. A fresh backing array
// is allocated so the original is undisturbed — leaf-shared slices
// would otherwise corrupt siblings on aliasing chains.
func prepend(p []string, segment string) []string {
	out := make([]string, len(p)+1)
	out[0] = segment
	copy(out[1:], p)
	return out
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
	MultiErr     Rule = "multierr"
)

// --- presence ---

type RequiredError struct{ Path []string }

func (e *RequiredError) Error() string {
	return fmt.Sprintf("missing required field %q", strings.Join(e.Path, "."))
}
func (*RequiredError) Rule() Rule             { return Required }
func (e *RequiredError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type NotEmptyError struct{ Path []string }

func (e *NotEmptyError) Error() string {
	return fmt.Sprintf("%s: must not be empty", strings.Join(e.Path, "."))
}
func (*NotEmptyError) Rule() Rule             { return NotEmpty }
func (e *NotEmptyError) prependPath(s string) { e.Path = prepend(e.Path, s) }

// --- length ---

type LenError struct {
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
	Path  []string
	Limit int
	Got   int
}

func (e *MaxRunesError) Error() string {
	return fmt.Sprintf("%s: rune count %d exceeds maximum %d", strings.Join(e.Path, "."), e.Got, e.Limit)
}
func (*MaxRunesError) Rule() Rule             { return MaxRunes }
func (e *MaxRunesError) prependPath(s string) { e.Path = prepend(e.Path, s) }

// --- numeric range. Limit kept as float64 to cover int/uint/float fields
// with a single struct shape. Value is `any` to preserve the originating
// numeric type for inspection (boxing only happens on the failure path).

type GTError struct {
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
	Path  []string
	Limit float64
	Value any
}

func (e *LTEError) Error() string {
	return fmt.Sprintf("%s: value %v exceeds maximum %v", strings.Join(e.Path, "."), e.Value, e.Limit)
}
func (*LTEError) Rule() Rule             { return LTE }
func (e *LTEError) prependPath(s string) { e.Path = prepend(e.Path, s) }

// --- equality. Want/Value typed `any`: same-shape struct for both
// string and numeric fields; the codegen picks the literal at the
// failure site.

type EqError struct {
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
	Path  []string
	Want  any
	Value any
}

func (e *NeqError) Error() string {
	return fmt.Sprintf("%s: value must not equal %v", strings.Join(e.Path, "."), e.Want)
}
func (*NeqError) Rule() Rule             { return Neq }
func (e *NeqError) prependPath(s string) { e.Path = prepend(e.Path, s) }

// --- oneof. Allowed is meant to point to a package-level "frozen" slice
// emitted by codegen — no per-error allocation.

type OneOfError struct {
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
	Path  []string
	Value string
}

func (e *EmailError) Error() string {
	return fmt.Sprintf("%s: %q is not a valid email", strings.Join(e.Path, "."), e.Value)
}
func (*EmailError) Rule() Rule             { return Email }
func (e *EmailError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type URLError struct {
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
	Path  []string
	Value string
}

func (e *ASCIIError) Error() string {
	return fmt.Sprintf("%s: %q contains non-ASCII bytes", strings.Join(e.Path, "."), e.Value)
}
func (*ASCIIError) Rule() Rule             { return ASCII }
func (e *ASCIIError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type PrintableError struct {
	Path  []string
	Value string
}

func (e *PrintableError) Error() string {
	return fmt.Sprintf("%s: %q contains non-printable bytes", strings.Join(e.Path, "."), e.Value)
}
func (*PrintableError) Rule() Rule             { return Printable }
func (e *PrintableError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type AlphanumError struct {
	Path  []string
	Value string
}

func (e *AlphanumError) Error() string {
	return fmt.Sprintf("%s: %q must be alphanumeric", strings.Join(e.Path, "."), e.Value)
}
func (*AlphanumError) Rule() Rule             { return Alphanum }
func (e *AlphanumError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type NumericError struct {
	Path  []string
	Value string
}

func (e *NumericError) Error() string {
	return fmt.Sprintf("%s: %q must be all digits", strings.Join(e.Path, "."), e.Value)
}
func (*NumericError) Rule() Rule             { return Numeric }
func (e *NumericError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type LowerError struct {
	Path  []string
	Value string
}

func (e *LowerError) Error() string {
	return fmt.Sprintf("%s: %q contains uppercase letters", strings.Join(e.Path, "."), e.Value)
}
func (*LowerError) Rule() Rule             { return Lower }
func (e *LowerError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type UpperError struct {
	Path  []string
	Value string
}

func (e *UpperError) Error() string {
	return fmt.Sprintf("%s: %q contains lowercase letters", strings.Join(e.Path, "."), e.Value)
}
func (*UpperError) Rule() Rule             { return Upper }
func (e *UpperError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type HexadecimalError struct {
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
	Path        []string
	Want, Value string
}

func (e *StartsError) Error() string {
	return fmt.Sprintf("%s: %q does not start with %q", strings.Join(e.Path, "."), e.Value, e.Want)
}
func (*StartsError) Rule() Rule             { return Starts }
func (e *StartsError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type EndsError struct {
	Path        []string
	Want, Value string
}

func (e *EndsError) Error() string {
	return fmt.Sprintf("%s: %q does not end with %q", strings.Join(e.Path, "."), e.Value, e.Want)
}
func (*EndsError) Rule() Rule             { return Ends }
func (e *EndsError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type ContainsError struct {
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

type DuplicateKeyError struct{ Path []string }

func (e *DuplicateKeyError) Error() string {
	return fmt.Sprintf("duplicate key %q", strings.Join(e.Path, "."))
}
func (*DuplicateKeyError) Rule() Rule             { return DuplicateKey }
func (e *DuplicateKeyError) prependPath(s string) { e.Path = prepend(e.Path, s) }

type UnknownKeyError struct{ Path []string }

func (e *UnknownKeyError) Error() string {
	return fmt.Sprintf("unknown key %q", strings.Join(e.Path, "."))
}
func (*UnknownKeyError) Rule() Rule             { return UnknownKey }
func (e *UnknownKeyError) prependPath(s string) { e.Path = prepend(e.Path, s) }

// --- custom ---

type CustomError struct {
	Path  []string
	Name  string
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

// --- aggregate ---

// Errors is a flat slice of validation failures, returned from every
// generated multierr decoder when at least one rule fired. Each entry's
// Path is root-relative — segments are prepended by [Append] as nested
// struct decodes bubble up. Implements error and Unwrap() []error so
// errors.Is / errors.As / errors.Unwrap walk every leaf.
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

// prependPath propagates the segment into every leaf, satisfying the
// [Error] interface so an aggregate can be passed where a single
// [Error] is expected (e.g. [decode.NewParseErr] validation passthrough).
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

// Append appends validation errors and prepending a segment
// cause [Errors] implements [Error]
func (es *Errors) Append(segment string, inner Error) {
	if nested, ok := inner.(Errors); ok {
		nested.prependPath(segment)
		*es = append(*es, nested...)
		return
	}
	*es = append(*es, inner)
}
