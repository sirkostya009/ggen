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
  make that mechanical. Pinned by `TestNestedValidationPath_Complete` +
  `TestNestedMultierr_drainsInnerValidationErrors` (bytes == stream).
- **Stream path** — `ggen.Stream.Offset()` (= `consumed + Pos`), NOT the raw
  buffer-relative `s.Pos`: the stream buffer compacts as it slides, so only
  `Offset()` stays relative to the whole payload. Already global at every
  depth — stream call sites keep plain `NewParseErr`.

Validation runs *after* the value is scanned, so `Pos` lands just past the
offending value, not at its first byte. The aggregate `Errors` slice has no
`Pos` of its own — each leaf carries one. Pinned by
`integrationtests/scan_decode_test.go` (`TestValidationError_Pos`).

## Concrete error structs (one per rule)

Pointer-receiver structs, all implement `ggen.Error`. Each carries a
`Pos int` and a root-relative `Path []string` (both first) plus an exported
`PrependPath(segment)` — deliberately NOT part of the `Error` interface
(implementing `Error` doesn't require it; `ggen.NewParseErr` and
`Errors.Append` assert for it to complete nested paths). Shapes below list
the remaining fields:

- **presence**: `RequiredError`, `NotEmptyError`
- **length**: `LenError{Want, Got int}`, `MinLenError`/`MaxLenError{Limit, Got int}`
- **runes**: `RunesError`, `MinRunesError`, `MaxRunesError` (same shape as length)
- **numeric range**: `GTError`/`GTEError`/`LTError`/`LTEError{Limit float64, Value any}`
- **equality**: `EqError`/`NeqError{Want any, Value any}` (string + numeric)
- **oneof**: `OneOfError{Allowed []string, Value any}` — `Allowed` points to a
  frozen package-level slice (see "Frozen OneOf slices")
- **patterns**: `URLError`/`AlphanumError`/`NumericError`/`LowerError`/
  `UpperError`/`HexadecimalError{Value string}` (`URLError` also has
  `Cause error` + `Unwrap()`)
- **prefix/suffix/contains**: `StartsError`/`EndsError`/`ContainsError{Want, Value string}`
- **other**: `MultipleError{Of float64, Value any}`, `DuplicateKeyError`,
  `UnknownKeyError`, `CustomError{Name string, Value any, Cause error}` (exposes
  `Unwrap()`; `Name` is the bare func identifier). Custom bool-form validators
  fail with `PredicateError{Name, Msg, Value}`; fallible bool-form mods fail with
  `ggen.ModError` (a parse error — lives in the `decode` package, not here)

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
construction never allocates the allowed slice (see cli/CLAUDE.md optimization
#13).
