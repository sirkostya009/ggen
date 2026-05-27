# decode/validation — typed validation error structs

Runtime subpackage. One concrete error struct per validation rule (plus the
`Error` interface and `Errors` slice). Generated code emits typed literals
directly at the failure site — no field-stuffed generic error, no per-error
rule-name comparison at use sites.

## Surface

```go
type Error interface { error; Rule() Rule }
type Rule string                              // const enum of rule names
type Errors []Error                           // multierr return; Unwrap() []error
```

`Rule` constants: `Required`, `NotEmpty`, `Len`, `MinLen`, `MaxLen`, `Runes`,
`MinRunes`, `MaxRunes`, `GT`, `GTE`, `LT`, `LTE`, `Eq`, `Neq`, `OneOf`,
`Email`, `URL`, `ASCII`, `Printable`, `Alphanum`, `Numeric`, `Lower`, `Upper`,
`Hexadecimal`, `Starts`, `Ends`, `Contains`, `Multiple`, `DuplicateKey`,
`UnknownKey`, `Custom`.

## Concrete error structs (one per rule)

Pointer-receiver structs, all implementing `validation.Error`:

- **presence**: `RequiredError{Field}`, `NotEmptyError{Field}`
- **length**: `LenError{Field,Want,Got int}`, `MinLenError`/`MaxLenError
  {Field,Limit,Got int}`
- **runes**: `RunesError`, `MinRunesError`, `MaxRunesError` (same shape as length)
- **numeric range**: `GTError`/`GTEError`/`LTError`/`LTEError{Field, Limit
  float64, Value any}`
- **equality**: `EqError`/`NeqError{Field, Want any, Value any}` (string + numeric)
- **oneof**: `OneOfError{Field, Allowed []string, Value any}` — `Allowed`
  always points to a frozen package-level slice emitted by codegen (no per-error
  alloc; see root CLAUDE.md optimization #16)
- **patterns**: `EmailError`/`URLError`/`ASCIIError`/`PrintableError`/
  `AlphanumError`/`NumericError`/`LowerError`/`UpperError`/`HexadecimalError
  {Field, Value string}` (`URLError` also has `Cause error` + `Unwrap()`)
- **prefix/suffix/contains**: `StartsError`/`EndsError`/`ContainsError{Field,
  Want, Value string}`
- **other**: `MultipleError{Field, Of float64, Value any}`,
  `DuplicateKeyError{Field}`, `UnknownKeyError{Field}`, `CustomError{Field,
  Name string, Cause error}` (exposes `Unwrap()`)

## Inspecting failures

```go
_, _, err := T{}.DecodeFrom(data)
var minlen *validation.MinLenError
if errors.As(err, &minlen) {
    // minlen.Field, minlen.Limit, minlen.Got
}
```

Use the typed pointer struct, or `err.(validation.Error).Rule()` to get name.
`Errors` (returned in `multierr`) and `CustomError` implement Unwrap().

## Frozen OneOf slices

`OneOfError.Allowed` always points to a deduped package-level frozen `[]string`
(`var _oneof_N = []string{...}`) emitted once per unique allowed-set. Error
construction never allocates the allowed slice — important for hot-path
validation failures.
