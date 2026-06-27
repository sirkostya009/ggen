# decode/validation — typed validation error structs

Runtime subpackage. One concrete error struct per validation rule (plus
`Error` interface + `Errors` slice). Codegen emits typed literals at failure
site — no field-stuffed generic error, no per-error rule-name compare at use site.

## Surface

```go
type Error interface { error; Rule() Rule }
type Rule string                              // const enum of rule names
type Errors []Error                           // multierr return; Unwrap() []error
```

## `Pos` (failure location)

Every concrete error struct carries a `Pos int` as its first field — the byte
offset of the failure **relative to the full payload**. The codegen injects it
right after the struct-literal opening brace at every emit site
(`withPos`/`posLit` in `generate.go` — wraps the `onErr` closure plus the
standalone required / array-len / dup-key / unknown-key literals):

- **Bytes path** — the cursor `i`, a true index into the `data` slice.
- **Stream path** — `scan.Stream.Offset()` (= `consumed + Pos`), NOT the raw
  buffer-relative `s.Pos`. The stream buffer compacts (discards consumed bytes)
  as it slides, so only `Offset()` stays relative to the whole payload.

Validation runs *after* the value is scanned, so the position lands just past
the offending value (not at its first byte). The aggregate `Errors` slice
carries no `Pos` of its own — each leaf has its own. Pinned by
`integrationtests/scan_decode_test.go` (`TestValidationError_Pos` — `Pos`
identical on bytes + stream despite stream-window compaction).

`Rule` constants: `Required`, `NotEmpty`, `Len`, `MinLen`, `MaxLen`, `Runes`,
`MinRunes`, `MaxRunes`, `GT`, `GTE`, `LT`, `LTE`, `Eq`, `Neq`, `OneOf`,
`Email`, `URL`, `ASCII`, `Printable`, `Alphanum`, `Numeric`, `Lower`, `Upper`,
`Hexadecimal`, `Starts`, `Ends`, `Contains`, `Multiple`, `DuplicateKey`,
`UnknownKey`, `Custom`.

## Concrete error structs (one per rule)

Pointer-receiver structs, all implement `validation.Error`. Each also carries
a `Pos int` (full-payload byte offset, first field) and a root-relative `Path
[]string` (the shapes below name `Field` historically — the actual field is
`Path`):

- **presence**: `RequiredError{Field}`, `NotEmptyError{Field}`
- **length**: `LenError{Field,Want,Got int}`, `MinLenError`/`MaxLenError
  {Field,Limit,Got int}`
- **runes**: `RunesError`, `MinRunesError`, `MaxRunesError` (same shape as length)
- **numeric range**: `GTError`/`GTEError`/`LTError`/`LTEError{Field, Limit
  float64, Value any}`
- **equality**: `EqError`/`NeqError{Field, Want any, Value any}` (string + numeric)
- **oneof**: `OneOfError{Field, Allowed []string, Value any}` — `Allowed`
  points to frozen package-level slice from codegen (no per-error
  alloc; see root CLAUDE.md optimization #16)
- **patterns**: `EmailError`/`URLError`/`ASCIIError`/`PrintableError`/
  `AlphanumError`/`NumericError`/`LowerError`/`UpperError`/`HexadecimalError
  {Field, Value string}` (`URLError` also has `Cause error` + `Unwrap()`)
- **prefix/suffix/contains**: `StartsError`/`EndsError`/`ContainsError{Field,
  Want, Value string}`
- **other**: `MultipleError{Field, Of float64, Value any}`,
  `DuplicateKeyError{Field}`, `UnknownKeyError{Field}`, `CustomError{Field,
  Name string, Value any, Cause error}` (exposes `Unwrap()`; `Name` is the
  bare func identifier, parallel to `PredicateError`)

## Inspecting failures

```go
_, _, err := T{}.DecodeFrom(data)
var minlen *validation.MinLenError
if errors.As(err, &minlen) {
    // minlen.Field, minlen.Limit, minlen.Got
}
```

Use typed pointer struct, or `err.(validation.Error).Rule()` for name.
`Errors` (from `multierr`) + `CustomError` implement Unwrap().

## Frozen OneOf slices

`OneOfError.Allowed` points to deduped package-level frozen `[]string`
(`var _oneof_N = []string{...}`) emitted once per unique allowed-set. Error
construction never allocates allowed slice — critical for hot-path
validation failures.
