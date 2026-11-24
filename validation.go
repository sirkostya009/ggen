// Typed validation errors returned by generated decoders. Malformed JSON is
// reported as a [ParseError]. The types here are only for values that parsed
// but broke a rule. Each rule has its own error type. Use errors.As to check
// for a specific one.
//
// In multierr mode the decoder returns [Errors], a flat list of failures.
// Each entry carries its own Path from the document root. When a nested
// struct fails, [Errors.Collect] adds the outer segment to the front.

package ggen

import (
	"fmt"
	"strings"
	"unsafe"
)

// ValidationError is implemented by every validation error type, and only by
// them: the unexported at method seals it. Path lists the keys from the
// document root to the rejected value, Message says what is wrong with it,
// and Error joins the two as "a.b: message".
type ValidationError interface {
	error
	Rule() Rule
	Path() []string
	Message() string
	// at points at the error's Pos, Keys and path buffer, so nested decodes
	// can shift the position and put the outer key in front of the path.
	at() (pos *int, keys *[]string, head **pathBuf)
}

// relocate shifts e's Pos by d and puts segment in front of its path. An
// empty segment leaves the path unchanged.
func relocate(e ValidationError, segment string, d int) {
	pos, keys, head := e.at()
	*pos += d
	if segment != "" {
		prepend(keys, head, segment)
	}
}

// joinError renders "a.b: msg", or msg alone when the path is empty.
func joinError(keys []string, msg string) string {
	if len(keys) == 0 {
		return msg
	}
	return strings.Join(keys, ".") + ": " + msg
}

// pathBuf holds a path as the tail of buf, with free slots in front of it.
// off is the index of the path's first segment.
type pathBuf struct {
	buf []string
	off int
}

// prepend puts segment in front of *keys. The path lives at the end of a
// pathBuf that doubles when it fills, so a chain of prepends costs amortized
// O(1) each. Rebuilding the path at every nesting level is quadratic in the
// depth, which a recursive type lets the input choose.
//
// Keys that are not the current tail of *head are copied first: a literal,
// a slice the caller put there, or the view of an error that was copied by
// value. Sibling errors may share such a slice, so it is never written to.
func prepend(keys *[]string, head **pathBuf, segment string) {
	k, h := *keys, *head
	if h == nil || h.off == 0 || len(h.buf)-h.off != len(k) || (len(k) > 0 && &h.buf[h.off] != &k[0]) {
		h = &pathBuf{buf: make([]string, 2*len(k)+2)}
		h.off = len(h.buf) - len(k)
		copy(h.buf[h.off:], k)
		*head = h
	}
	h.off--
	h.buf[h.off] = segment
	*keys = h.buf[h.off:]
}

// Rule identifies which validation rule failed.
type Rule string

// Rule identifiers. Each matches its keyword in the `pipe` struct tag, except
// DuplicateKey, UnknownKey, Custom and Predicate, which name failures that
// have no keyword: a repeated key, an unknown key, an `@Func` validator that
// returned an error and one that returned false.
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
)

// --- presence ---

type RequiredError struct {
	Pos  int
	Keys []string
	head *pathBuf
}

func (e *RequiredError) Message() string {
	return "missing required field"
}
func (*RequiredError) Rule() Rule                         { return Required }
func (e *RequiredError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *RequiredError) Path() []string                   { return e.Keys }
func (e *RequiredError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

type NotEmptyError struct {
	Pos  int
	Keys []string
	head *pathBuf
}

func (e *NotEmptyError) Message() string {
	return "must not be empty"
}
func (*NotEmptyError) Rule() Rule                         { return NotEmpty }
func (e *NotEmptyError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *NotEmptyError) Path() []string                   { return e.Keys }
func (e *NotEmptyError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

// --- length ---

type LenError struct {
	Pos  int
	Keys []string
	head *pathBuf
	Want int
	Got  int
	// AtLeast marks Got as a lower bound. Decoding stopped at the first
	// element past Want instead of counting the rest.
	AtLeast bool
}

func (e *LenError) Message() string {
	if e.AtLeast {
		return fmt.Sprintf("length at least %d != required %d", e.Got, e.Want)
	}
	return fmt.Sprintf("length %d != required %d", e.Got, e.Want)
}
func (*LenError) Rule() Rule                         { return Len }
func (e *LenError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *LenError) Path() []string                   { return e.Keys }
func (e *LenError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

type MinLenError struct {
	Pos   int
	Keys  []string
	head  *pathBuf
	Limit int
	Got   int
}

func (e *MinLenError) Message() string {
	return fmt.Sprintf("length %d below minimum %d", e.Got, e.Limit)
}
func (*MinLenError) Rule() Rule                         { return MinLen }
func (e *MinLenError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *MinLenError) Path() []string                   { return e.Keys }
func (e *MinLenError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

type MaxLenError struct {
	Pos   int
	Keys  []string
	head  *pathBuf
	Limit int
	Got   int
}

func (e *MaxLenError) Message() string {
	return fmt.Sprintf("length %d exceeds maximum %d", e.Got, e.Limit)
}
func (*MaxLenError) Rule() Rule                         { return MaxLen }
func (e *MaxLenError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *MaxLenError) Path() []string                   { return e.Keys }
func (e *MaxLenError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

// --- runes ---

type RunesError struct {
	Pos  int
	Keys []string
	head *pathBuf
	Want int
	Got  int
}

func (e *RunesError) Message() string {
	return fmt.Sprintf("rune count %d != required %d", e.Got, e.Want)
}
func (*RunesError) Rule() Rule                         { return Runes }
func (e *RunesError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *RunesError) Path() []string                   { return e.Keys }
func (e *RunesError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

type MinRunesError struct {
	Pos   int
	Keys  []string
	head  *pathBuf
	Limit int
	Got   int
}

func (e *MinRunesError) Message() string {
	return fmt.Sprintf("rune count %d below minimum %d", e.Got, e.Limit)
}
func (*MinRunesError) Rule() Rule                         { return MinRunes }
func (e *MinRunesError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *MinRunesError) Path() []string                   { return e.Keys }
func (e *MinRunesError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

type MaxRunesError struct {
	Pos   int
	Keys  []string
	head  *pathBuf
	Limit int
	Got   int
}

func (e *MaxRunesError) Message() string {
	return fmt.Sprintf("rune count %d exceeds maximum %d", e.Got, e.Limit)
}
func (*MaxRunesError) Rule() Rule                         { return MaxRunes }
func (e *MaxRunesError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *MaxRunesError) Path() []string                   { return e.Keys }
func (e *MaxRunesError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

// --- numeric range. Limit and Value are `any` so that a large integer is
// reported exactly. A float64 would round integers above 2^53.

type GTError struct {
	Pos   int
	Keys  []string
	head  *pathBuf
	Limit any
	Value any
}

func (e *GTError) Message() string {
	return fmt.Sprintf("value %v not greater than %v", e.Value, e.Limit)
}
func (*GTError) Rule() Rule                         { return GT }
func (e *GTError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *GTError) Path() []string                   { return e.Keys }
func (e *GTError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

type GTEError struct {
	Pos   int
	Keys  []string
	head  *pathBuf
	Limit any
	Value any
}

func (e *GTEError) Message() string {
	return fmt.Sprintf("value %v below minimum %v", e.Value, e.Limit)
}
func (*GTEError) Rule() Rule                         { return GTE }
func (e *GTEError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *GTEError) Path() []string                   { return e.Keys }
func (e *GTEError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

type LTError struct {
	Pos   int
	Keys  []string
	head  *pathBuf
	Limit any
	Value any
}

func (e *LTError) Message() string {
	return fmt.Sprintf("value %v not less than %v", e.Value, e.Limit)
}
func (*LTError) Rule() Rule                         { return LT }
func (e *LTError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *LTError) Path() []string                   { return e.Keys }
func (e *LTError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

type LTEError struct {
	Pos   int
	Keys  []string
	head  *pathBuf
	Limit any
	Value any
}

func (e *LTEError) Message() string {
	return fmt.Sprintf("value %v exceeds maximum %v", e.Value, e.Limit)
}
func (*LTEError) Rule() Rule                         { return LTE }
func (e *LTEError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *LTEError) Path() []string                   { return e.Keys }
func (e *LTEError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

// --- equality. Want and Value are `any` to cover string and numeric fields.

type EqError struct {
	Pos   int
	Keys  []string
	head  *pathBuf
	Want  any
	Value any
}

func (e *EqError) Message() string {
	return fmt.Sprintf("value %v != %v", e.Value, e.Want)
}
func (*EqError) Rule() Rule                         { return Eq }
func (e *EqError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *EqError) Path() []string                   { return e.Keys }
func (e *EqError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

type NeqError struct {
	Pos   int
	Keys  []string
	head  *pathBuf
	Want  any
	Value any
}

func (e *NeqError) Message() string {
	return fmt.Sprintf("value must not equal %v", e.Want)
}
func (*NeqError) Rule() Rule                         { return Neq }
func (e *NeqError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *NeqError) Path() []string                   { return e.Keys }
func (e *NeqError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

// --- oneof. Allowed points to a shared package-level slice. The error does
// not own it, so do not modify it.

type OneOfError struct {
	Pos     int
	Keys    []string
	head    *pathBuf
	Allowed []string
	Value   any
}

func (e *OneOfError) Message() string {
	return fmt.Sprintf("value %v not in allowed set [%s]", e.Value,
		strings.Join(e.Allowed, ", "))
}
func (*OneOfError) Rule() Rule                         { return OneOf }
func (e *OneOfError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *OneOfError) Path() []string                   { return e.Keys }
func (e *OneOfError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

// --- format predicates ---

type URLError struct {
	Pos   int
	Keys  []string
	head  *pathBuf
	Value string
}

func (e *URLError) Message() string {
	return fmt.Sprintf("%q is not a valid url", e.Value)
}
func (*URLError) Rule() Rule                         { return URL }
func (e *URLError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *URLError) Path() []string                   { return e.Keys }
func (e *URLError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

type AlphanumError struct {
	Pos   int
	Keys  []string
	head  *pathBuf
	Value string
}

func (e *AlphanumError) Message() string {
	return fmt.Sprintf("%q must be alphanumeric", e.Value)
}
func (*AlphanumError) Rule() Rule                         { return Alphanum }
func (e *AlphanumError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *AlphanumError) Path() []string                   { return e.Keys }
func (e *AlphanumError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

type NumericError struct {
	Pos   int
	Keys  []string
	head  *pathBuf
	Value string
}

func (e *NumericError) Message() string {
	return fmt.Sprintf("%q must be all digits", e.Value)
}
func (*NumericError) Rule() Rule                         { return Numeric }
func (e *NumericError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *NumericError) Path() []string                   { return e.Keys }
func (e *NumericError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

type LowerError struct {
	Pos   int
	Keys  []string
	head  *pathBuf
	Value string
}

func (e *LowerError) Message() string {
	return fmt.Sprintf("%q contains uppercase letters", e.Value)
}
func (*LowerError) Rule() Rule                         { return Lower }
func (e *LowerError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *LowerError) Path() []string                   { return e.Keys }
func (e *LowerError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

type UpperError struct {
	Pos   int
	Keys  []string
	head  *pathBuf
	Value string
}

func (e *UpperError) Message() string {
	return fmt.Sprintf("%q contains lowercase letters", e.Value)
}
func (*UpperError) Rule() Rule                         { return Upper }
func (e *UpperError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *UpperError) Path() []string                   { return e.Keys }
func (e *UpperError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

type HexadecimalError struct {
	Pos   int
	Keys  []string
	head  *pathBuf
	Value string
}

func (e *HexadecimalError) Message() string {
	return fmt.Sprintf("%q is not hexadecimal", e.Value)
}
func (*HexadecimalError) Rule() Rule                         { return Hexadecimal }
func (e *HexadecimalError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *HexadecimalError) Path() []string                   { return e.Keys }
func (e *HexadecimalError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

// --- starts, ends, contains ---

type StartsError struct {
	Pos         int
	Keys        []string
	head        *pathBuf
	Want, Value string
}

func (e *StartsError) Message() string {
	return fmt.Sprintf("%q does not start with %q", e.Value, e.Want)
}
func (*StartsError) Rule() Rule                         { return Starts }
func (e *StartsError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *StartsError) Path() []string                   { return e.Keys }
func (e *StartsError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

type EndsError struct {
	Pos         int
	Keys        []string
	head        *pathBuf
	Want, Value string
}

func (e *EndsError) Message() string {
	return fmt.Sprintf("%q does not end with %q", e.Value, e.Want)
}
func (*EndsError) Rule() Rule                         { return Ends }
func (e *EndsError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *EndsError) Path() []string                   { return e.Keys }
func (e *EndsError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

type ContainsError struct {
	Pos         int
	Keys        []string
	head        *pathBuf
	Want, Value string
}

func (e *ContainsError) Message() string {
	return fmt.Sprintf("%q does not contain %q", e.Value, e.Want)
}
func (*ContainsError) Rule() Rule                         { return Contains }
func (e *ContainsError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *ContainsError) Path() []string                   { return e.Keys }
func (e *ContainsError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

// --- multiple ---

type MultipleError struct {
	Pos   int
	Keys  []string
	head  *pathBuf
	Of    any
	Value any
}

func (e *MultipleError) Message() string {
	return fmt.Sprintf("%v is not a multiple of %v", e.Value, e.Of)
}
func (*MultipleError) Rule() Rule                         { return Multiple }
func (e *MultipleError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *MultipleError) Path() []string                   { return e.Keys }
func (e *MultipleError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

// --- key violations ---

type DuplicateKeyError struct {
	Pos  int
	Keys []string
	head *pathBuf
}

func (e *DuplicateKeyError) Message() string {
	return "duplicate key"
}
func (*DuplicateKeyError) Rule() Rule                         { return DuplicateKey }
func (e *DuplicateKeyError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *DuplicateKeyError) Path() []string                   { return e.Keys }
func (e *DuplicateKeyError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

type UnknownKeyError struct {
	Pos  int
	Keys []string
	head *pathBuf
}

func (e *UnknownKeyError) Message() string {
	return "unknown key"
}
func (*UnknownKeyError) Rule() Rule                         { return UnknownKey }
func (e *UnknownKeyError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *UnknownKeyError) Path() []string                   { return e.Keys }
func (e *UnknownKeyError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

// --- custom ---

// CustomError reports a failed custom validator of the form func(T) error.
// Name is the bare function name, without '@' or package. Value is the rejected
// input. Cause is the error the validator returned.
type CustomError struct {
	Pos   int
	Keys  []string
	head  *pathBuf
	Name  string
	Value any
	Cause error
}

func (e *CustomError) Message() string {
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return fmt.Sprintf("validation %q failed", e.Name)
}
func (*CustomError) Rule() Rule                         { return Custom }
func (e *CustomError) Unwrap() error                    { return e.Cause }
func (e *CustomError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *CustomError) Path() []string                   { return e.Keys }
func (e *CustomError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

// PredicateError reports a failed custom validator of the form func(T) bool.
// Name is the bare function name. Msg is the optional message from the tag. When
// Msg is empty, Message returns a default text. Value is the rejected input.
type PredicateError struct {
	Pos   int
	Keys  []string
	head  *pathBuf
	Name  string
	Msg   string
	Value any
}

func (e *PredicateError) Message() string {
	if e.Msg != "" {
		return e.Msg
	}
	return fmt.Sprintf("%q rejected %v", e.Name, e.Value)
}
func (*PredicateError) Rule() Rule                         { return Predicate }
func (e *PredicateError) Error() string                    { return joinError(e.Keys, e.Message()) }
func (e *PredicateError) Path() []string                   { return e.Keys }
func (e *PredicateError) at() (*int, *[]string, **pathBuf) { return &e.Pos, &e.Keys, &e.head }

// --- aggregate ---

// Errors is a flat list of validation failures from a multierr decoder. Each
// entry's Path starts at the document root. Errors is not a ValidationError
// itself. It implements error and Unwrap() []error, so errors.Is and
// errors.As check every entry.
//
//nolint:recvcheck // append needs a pointer receiver
type Errors []ValidationError

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

func (es Errors) Unwrap() []error {
	out := make([]error, len(es))
	for i := range es {
		out[i] = es[i]
	}
	return out
}

// Collect adds every entry of err to es when err is an [Errors], the error a
// multierr decoder returns after it read its whole value. Each entry gets
// segment at the front of its path and its Pos shifted by d. Generated
// bytes-path code passes the start of the nested value as d, because the
// nested decoder ran on data[start:] and its positions are offsets into that
// sub-slice. Stream positions already cover the whole input, so stream code
// passes 0. Collect adds nothing and reports false when err is anything else:
// a parse error, or a single [ValidationError], which a decoder returns when
// it stopped in the middle of its value.
func (es *Errors) Collect(segment string, d int, err error) bool {
	e, ok := err.(Errors)
	if !ok {
		return false
	}
	for _, v := range e {
		relocate(v, segment, d)
	}
	*es = append(*es, e...)
	return true
}
