// Typed validation errors returned by generated decoders. Malformed JSON is
// reported as a [ParseError]. The types here are only for values that parsed
// but broke a rule. Each rule has its own error type. Use errors.As to check
// for a specific one.
//
// In multierr mode the decoder returns [Errors], a flat list of failures.
// Each entry carries its own Path from the document root. When a nested
// struct fails, [Errors.Append] adds the outer segment to the front.

package ggen

import (
	"fmt"
	"strings"
	"unsafe"
)

// Error is implemented by every validation error type. Each concrete type
// also has PrependPath and AddPos methods. They are left out of the interface
// on purpose, so implementing Error does not require them. Code that needs
// them checks for them with a type assertion.
type Error interface {
	error
	Rule() Rule
}

// prepend returns segment followed by p, in a new backing array. Sibling
// errors may share p, so it must not be modified in place.
func prepend(p []string, segment string) []string {
	out := make([]string, len(p)+1)
	out[0] = segment
	copy(out[1:], p)
	return out
}

// Rule identifies which validation rule failed.
type Rule string

// Rule identifiers. Each matches its keyword in the `pipe` struct tag.
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
	URL          Rule = "url"
	Alphanum     Rule = "alphanum"
	Numeric      Rule = "numeric"
	Lower        Rule = "islower"
	Upper        Rule = "isupper"
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
func (e *RequiredError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *RequiredError) AddPos(d int)         { e.Pos += d }

type NotEmptyError struct {
	Pos  int
	Path []string
}

func (e *NotEmptyError) Error() string {
	return strings.Join(e.Path, ".") + ": must not be empty"
}
func (*NotEmptyError) Rule() Rule             { return NotEmpty }
func (e *NotEmptyError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *NotEmptyError) AddPos(d int)         { e.Pos += d }

// --- length ---

type LenError struct {
	Pos  int
	Path []string
	Want int
	Got  int
	// AtLeast marks Got as a lower bound. Decoding stopped at the first
	// element past Want instead of counting the rest.
	AtLeast bool
}

func (e *LenError) Error() string {
	if e.AtLeast {
		return fmt.Sprintf("%s: length at least %d != required %d", strings.Join(e.Path, "."), e.Got, e.Want)
	}
	return fmt.Sprintf("%s: length %d != required %d", strings.Join(e.Path, "."), e.Got, e.Want)
}
func (*LenError) Rule() Rule             { return Len }
func (e *LenError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *LenError) AddPos(d int)         { e.Pos += d }

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
func (e *MinLenError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *MinLenError) AddPos(d int)         { e.Pos += d }

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
func (e *MaxLenError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *MaxLenError) AddPos(d int)         { e.Pos += d }

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
func (e *RunesError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *RunesError) AddPos(d int)         { e.Pos += d }

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
func (e *MinRunesError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *MinRunesError) AddPos(d int)         { e.Pos += d }

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
func (e *MaxRunesError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *MaxRunesError) AddPos(d int)         { e.Pos += d }

// --- numeric range. Limit and Value are `any` so that a large integer is
// reported exactly. A float64 would round integers above 2^53.

type GTError struct {
	Pos   int
	Path  []string
	Limit any
	Value any
}

func (e *GTError) Error() string {
	return fmt.Sprintf("%s: value %v not greater than %v", strings.Join(e.Path, "."), e.Value, e.Limit)
}
func (*GTError) Rule() Rule             { return GT }
func (e *GTError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *GTError) AddPos(d int)         { e.Pos += d }

type GTEError struct {
	Pos   int
	Path  []string
	Limit any
	Value any
}

func (e *GTEError) Error() string {
	return fmt.Sprintf("%s: value %v below minimum %v", strings.Join(e.Path, "."), e.Value, e.Limit)
}
func (*GTEError) Rule() Rule             { return GTE }
func (e *GTEError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *GTEError) AddPos(d int)         { e.Pos += d }

type LTError struct {
	Pos   int
	Path  []string
	Limit any
	Value any
}

func (e *LTError) Error() string {
	return fmt.Sprintf("%s: value %v not less than %v", strings.Join(e.Path, "."), e.Value, e.Limit)
}
func (*LTError) Rule() Rule             { return LT }
func (e *LTError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *LTError) AddPos(d int)         { e.Pos += d }

type LTEError struct {
	Pos   int
	Path  []string
	Limit any
	Value any
}

func (e *LTEError) Error() string {
	return fmt.Sprintf("%s: value %v exceeds maximum %v", strings.Join(e.Path, "."), e.Value, e.Limit)
}
func (*LTEError) Rule() Rule             { return LTE }
func (e *LTEError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *LTEError) AddPos(d int)         { e.Pos += d }

// --- equality. Want and Value are `any` to cover string and numeric fields.

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
func (e *EqError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *EqError) AddPos(d int)         { e.Pos += d }

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
func (e *NeqError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *NeqError) AddPos(d int)         { e.Pos += d }

// --- oneof. Allowed points to a shared package-level slice. The error does
// not own it, so do not modify it.

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
func (e *OneOfError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *OneOfError) AddPos(d int)         { e.Pos += d }

// --- format predicates ---

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
func (e *URLError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *URLError) AddPos(d int)         { e.Pos += d }

type AlphanumError struct {
	Pos   int
	Path  []string
	Value string
}

func (e *AlphanumError) Error() string {
	return fmt.Sprintf("%s: %q must be alphanumeric", strings.Join(e.Path, "."), e.Value)
}
func (*AlphanumError) Rule() Rule             { return Alphanum }
func (e *AlphanumError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *AlphanumError) AddPos(d int)         { e.Pos += d }

type NumericError struct {
	Pos   int
	Path  []string
	Value string
}

func (e *NumericError) Error() string {
	return fmt.Sprintf("%s: %q must be all digits", strings.Join(e.Path, "."), e.Value)
}
func (*NumericError) Rule() Rule             { return Numeric }
func (e *NumericError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *NumericError) AddPos(d int)         { e.Pos += d }

type LowerError struct {
	Pos   int
	Path  []string
	Value string
}

func (e *LowerError) Error() string {
	return fmt.Sprintf("%s: %q contains uppercase letters", strings.Join(e.Path, "."), e.Value)
}
func (*LowerError) Rule() Rule             { return Lower }
func (e *LowerError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *LowerError) AddPos(d int)         { e.Pos += d }

type UpperError struct {
	Pos   int
	Path  []string
	Value string
}

func (e *UpperError) Error() string {
	return fmt.Sprintf("%s: %q contains lowercase letters", strings.Join(e.Path, "."), e.Value)
}
func (*UpperError) Rule() Rule             { return Upper }
func (e *UpperError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *UpperError) AddPos(d int)         { e.Pos += d }

type HexadecimalError struct {
	Pos   int
	Path  []string
	Value string
}

func (e *HexadecimalError) Error() string {
	return fmt.Sprintf("%s: %q is not hexadecimal", strings.Join(e.Path, "."), e.Value)
}
func (*HexadecimalError) Rule() Rule             { return Hexadecimal }
func (e *HexadecimalError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *HexadecimalError) AddPos(d int)         { e.Pos += d }

// --- starts, ends, contains ---

type StartsError struct {
	Pos         int
	Path        []string
	Want, Value string
}

func (e *StartsError) Error() string {
	return fmt.Sprintf("%s: %q does not start with %q", strings.Join(e.Path, "."), e.Value, e.Want)
}
func (*StartsError) Rule() Rule             { return Starts }
func (e *StartsError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *StartsError) AddPos(d int)         { e.Pos += d }

type EndsError struct {
	Pos         int
	Path        []string
	Want, Value string
}

func (e *EndsError) Error() string {
	return fmt.Sprintf("%s: %q does not end with %q", strings.Join(e.Path, "."), e.Value, e.Want)
}
func (*EndsError) Rule() Rule             { return Ends }
func (e *EndsError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *EndsError) AddPos(d int)         { e.Pos += d }

type ContainsError struct {
	Pos         int
	Path        []string
	Want, Value string
}

func (e *ContainsError) Error() string {
	return fmt.Sprintf("%s: %q does not contain %q", strings.Join(e.Path, "."), e.Value, e.Want)
}
func (*ContainsError) Rule() Rule             { return Contains }
func (e *ContainsError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *ContainsError) AddPos(d int)         { e.Pos += d }

// --- multiple ---

type MultipleError struct {
	Pos   int
	Path  []string
	Of    any
	Value any
}

func (e *MultipleError) Error() string {
	return fmt.Sprintf("%s: %v is not a multiple of %v", strings.Join(e.Path, "."), e.Value, e.Of)
}
func (*MultipleError) Rule() Rule             { return Multiple }
func (e *MultipleError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *MultipleError) AddPos(d int)         { e.Pos += d }

// --- key violations ---

type DuplicateKeyError struct {
	Pos  int
	Path []string
}

func (e *DuplicateKeyError) Error() string {
	return fmt.Sprintf("duplicate key %q", strings.Join(e.Path, "."))
}
func (*DuplicateKeyError) Rule() Rule             { return DuplicateKey }
func (e *DuplicateKeyError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *DuplicateKeyError) AddPos(d int)         { e.Pos += d }

type UnknownKeyError struct {
	Pos  int
	Path []string
}

func (e *UnknownKeyError) Error() string {
	return fmt.Sprintf("unknown key %q", strings.Join(e.Path, "."))
}
func (*UnknownKeyError) Rule() Rule             { return UnknownKey }
func (e *UnknownKeyError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *UnknownKeyError) AddPos(d int)         { e.Pos += d }

// --- custom ---

// CustomError reports a failed custom validator of the form func(T) error.
// Name is the function name without the leading '@'. Value is the rejected
// input. Cause is the error the validator returned.
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
func (e *CustomError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *CustomError) AddPos(d int)         { e.Pos += d }

// PredicateError reports a failed custom validator of the form func(T) bool.
// Name is the function name. Msg is the optional message from the tag. When
// Msg is empty, Error prints a default message. Value is the rejected input.
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
func (e *PredicateError) PrependPath(s string) { e.Path = prepend(e.Path, s) }
func (e *PredicateError) AddPos(d int)         { e.Pos += d }

// --- aggregate ---

// Errors is a flat list of validation failures from a multierr decoder. Each
// entry's Path starts at the document root. Errors implements error and
// Unwrap() []error, so errors.Is and errors.As check every entry.
//
//nolint:recvcheck // append needs a pointer receiver
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
	if len(buf) == 0 {
		// A single entry that prints "" leaves buf nil, and &buf[0] would
		// panic.
		return ""
	}
	return unsafe.String(&buf[0], len(buf))
}

func (Errors) Rule() Rule { return MultiErr }

// AddPos shifts the byte offset of every entry by d. Nested decodes on the
// bytes path use it (see ShiftPos).
func (es Errors) AddPos(d int) {
	for _, e := range es {
		if ap, ok := e.(interface{ AddPos(d int) }); ok {
			ap.AddPos(d)
		}
	}
}

// ShiftPos shifts the Pos of e by d, if e has one, and returns e. Generated
// bytes-path code calls it on each error from a nested decoder. That decoder
// ran on data[start:], so its positions are offsets into the sub-slice until
// they are shifted by start. Stream positions already cover the whole input.
func ShiftPos(e Error, d int) Error {
	if ap, ok := e.(interface{ AddPos(d int) }); ok {
		ap.AddPos(d)
	}
	return e
}

// PrependPath adds segment to the front of every entry's path.
func (es Errors) PrependPath(segment string) {
	for _, e := range es {
		if p, ok := e.(interface{ PrependPath(segment string) }); ok {
			p.PrependPath(segment)
		}
	}
}

func (es Errors) Unwrap() []error {
	out := make([]error, len(es))
	for i := range es {
		out[i] = es[i]
	}
	return out
}

// Append adds inner to es and puts segment at the front of its path. If inner
// is itself an [Errors], its entries are added one by one.
func (es *Errors) Append(segment string, inner Error) {
	if nested, ok := inner.(Errors); ok {
		nested.PrependPath(segment)
		*es = append(*es, nested...)
		return
	}
	// A single error gets the segment too. This covers a fail-fast child
	// nested under a multierr parent, so its path includes the outer field.
	if p, ok := inner.(interface{ PrependPath(segment string) }); ok {
		p.PrependPath(segment)
	}
	*es = append(*es, inner)
}
