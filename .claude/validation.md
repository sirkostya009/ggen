# validation — typed rule errors + format predicates

Runtime package, top-level sibling of `decode` (validation is not a sub-concern
of parsing — hard parse failures wrap in `ggen.ParseError`; only rule-level
failures land here). One concrete error struct per validation rule (plus
`Error` interface + `Errors` slice). Codegen emits typed literals at the
failure site — no field-stuffed generic error, no per-error rule-name compare
at the use site.

## `predicates.go`

Format predicates for generated validation branches, each 1:1 with a rule name:
`IsAlphanum`, `IsNumeric`, `IsHex`, `IsURL`, `IsLower`, `IsUpper`
(rules `islower`/`isupper`). Emitted as
`ggen.IsX(ref)` guards paired with the matching typed error
(`ggen.URLError`, …) — one package for both.

## Surface

```go
type Error interface { error; Rule() Rule }
type Rule string                              // const enum of rule names
type Errors []Error                           // multierr return; Unwrap() []error
```

`Rule` constants: `Required`, `NotEmpty`, `Len`, `MinLen`, `MaxLen`, `Runes`,
`MinRunes`, `MaxRunes`, `GT`, `GTE`, `LT`, `LTE`, `Eq`, `Neq`, `OneOf`,
`URL`, `Alphanum`, `Numeric`, `Lower` (= "islower"), `Upper` (= "isupper"),
`Hexadecimal`, `Starts`, `Ends`, `Contains`, `Multiple`, `DuplicateKey`,
`UnknownKey`, `Custom`, `Predicate`, `MultiErr`.

## `Pos` (failure location)

Every concrete error carries a `Pos int` (first field) — the byte offset of
the failure **relative to the full payload**, injected by codegen right after
the struct-literal opening brace (`withPos`/`posLit` in `generate.go`, wrapping
the `onErr` closure plus the standalone required / array-len / dup-key /
unknown-key literals):

- **Bytes path** — the cursor `i`, a true index into `data`. NESTED decoders
  run on `data[i:]`, so their errors surface sub-slice-relative and the call
  site rebases them by the value start (`ggen.NewParseErrShift`, or
  `ggen.ShiftPos` in the multierr drain) — every error type carries
  `AddPos(d int)` (sibling of `PrependPath`, `Errors` loops its leaves) to
  make that mechanical. `*ParseError.AddPos` also cascades into its wrapped
  cause, since a fallible mod's `ModError` is born pre-wrapped and would
  otherwise keep a sub-slice-relative `Pos`
  (`TestParseErrAddPosCascadesToModError`). Pinned by `TestNestedValidationPath_Complete` +
  `TestNestedMultierr_drainsInnerValidationErrors` (bytes == stream).
- **Stream path** — `ggen.Stream.Offset()` (= `consumed + Pos`), NOT the raw
  buffer-relative `s.Pos`: the stream buffer compacts as it slides, so only
  `Offset()` stays relative to the whole payload. Already global at every
  depth — stream call sites keep plain `NewParseErr`, with one exception: the
  cross-package `UnmarshalJSON` rung runs its callee on a CAPTURED span, so it
  wraps with `NewParseErrShift(field, s.Offset(), len(span), err)` to rebase
  the callee's span-relative positions (the span starts at
  `Offset()-len(span)`).

Validation runs *after* the value is scanned, so `Pos` lands just past the
offending value, not at its first byte. The aggregate `Errors` slice has no
`Pos` of its own — each leaf carries one. Pinned by
`integrationtests/scan_decode_test.go` (`TestValidationError_Pos`).

`UnknownKeyError.Pos` and `DuplicateKeyError.Pos` are the VALUE HEAD (after
the colon and whitespace) on both paths, in the single-error and multierr
arms alike — the stream builds the unknown-key error only after
`ConsumeColon`. `ParseError.Pos` is likewise identical on both paths at every
chunk size (.claude/scan.md, "Aggressive compaction"). Pinned by
`TestRead_unknownKey_streamParity`.

## Concrete error structs (one per rule)

Pointer-receiver structs, all implement `ggen.Error`. Each carries a
`Pos int` and a root-relative `Path []string` (both first) plus an exported
`PrependPath(segment)` — deliberately NOT part of the `Error` interface
(implementing `Error` doesn't require it; `ggen.NewParseErr` and
`Errors.Append` assert for it to complete nested paths). Shapes below list
the remaining fields:

- **presence**: `RequiredError`, `NotEmptyError`
- **length**: `LenError{Want, Got int; AtLeast bool}`, `MinLenError`/`MaxLenError{Limit, Got int}`.
  `AtLeast` marks `Got` as a lower bound and changes the message to "length at
  least N". A `[N]T` tuple sets it on overflow — the guard fires at the top of
  the element loop, before the extra element is counted, so `Got` is `N+1` —
  and leaves it false when too few, where `Got` is the exact count
- **runes**: `RunesError`, `MinRunesError`, `MaxRunesError` (same shape as length)
- **numeric range**: `GTError`/`GTEError`/`LTError`/`LTEError{Limit any, Value any}`
- **equality**: `EqError`/`NeqError{Want any, Value any}` (string + numeric)
- **oneof**: `OneOfError{Allowed []string, Value any}` — `Allowed` points to a
  frozen package-level slice (see "Frozen OneOf slices")
- **patterns**: `URLError`/`AlphanumError`/`NumericError`/`LowerError`/
  `UpperError`/`HexadecimalError{Value string}` (`URLError` also has
  `Cause error` + `Unwrap()`)
- **prefix/suffix/contains**: `StartsError`/`EndsError`/`ContainsError{Want, Value string}`
- **other**: `MultipleError{Of any, Value any}`, `DuplicateKeyError`,
  `UnknownKeyError`, `CustomError{Name string, Value any, Cause error}` (exposes
  `Unwrap()`; `Name` is the bare func identifier). Custom bool-form validators
  fail with `PredicateError{Name, Msg, Value}`; fallible bool-form mods fail with
  `ggen.ModError` (a parse error — lives in the `decode` package, not here)

Every numeric BOUND — `Limit` (gt/gte/lt/lte), `Of` (multiple), `Want`
(eq/neq) — is `any`, and codegen spells the literal with the FIELD's own kind
through one helper, `numBound(kind, value)`: `Limit: uint64(9223372036854775809)`,
`Limit: float64(1.5)`, `Want: uint64(18446744073709551615)`. Untyped, the
literal defaulted to `int` and overflowed above `MaxInt64`; as a `float64` a
bound past 2^53 was rounded and printed in exponent form
(`9.223372036854776e+18` for `gte=9223372036854775809`), which the width-aware
bound parsing made reachable. Read them with a type switch or `%v`. Pinned by
`TestNumericBoundLiteralsCarryFieldKind` (cmd/ggen) +
`TestBigUint64Bounds_reportedExactly` (integ).

## Inspecting failures

```go
_, _, err := T{}.DecodeFrom(data)
var minlen *ggen.MinLenError
if errors.As(err, &minlen) {
    // minlen.Path, minlen.Limit, minlen.Got
}
```

Use the typed pointer struct, or `err.(ggen.Error).Rule()` for the name.
`Errors` (from `multierr`) and `CustomError` implement `Unwrap()`.

## Frozen OneOf slices

`OneOfError.Allowed` points to a deduped package-level frozen `[]string`
(`var _oneof_N = []string{...}`) emitted once per unique allowed-set, so error
construction never allocates the allowed slice (see cmd/ggen/CLAUDE.md optimization
#13).
