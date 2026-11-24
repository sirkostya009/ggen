# validation — typed rule errors + format predicates

Validation half of the single runtime package `ggen` (validation is not a sub-concern
of parsing — hard parse failures wrap in `ggen.ParseError`; only rule-level
failures land here). One concrete error struct per validation rule (plus
`ValidationError` interface + `Errors` slice). Codegen emits typed literals at the
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
type ValidationError interface { error; Rule() Rule; Path() []string; Message() string }
type Rule string                              // const enum of rule names
type Errors []ValidationError                 // multierr return; Unwrap() []error; not a ValidationError itself
```

`Rule` constants: `Required`, `NotEmpty`, `Len`, `MinLen`, `MaxLen`, `Runes`,
`MinRunes`, `MaxRunes`, `GT`, `GTE`, `LT`, `LTE`, `Eq`, `Neq`, `OneOf`,
`URL`, `Alphanum`, `Numeric`, `Lower` (= "islower"), `Upper` (= "isupper"),
`Hexadecimal`, `Starts`, `Ends`, `Contains`, `Multiple`, `DuplicateKey`,
`UnknownKey`, `Custom`, `Predicate`.

`Error()` is `joinError(Keys, Message())`: `"a.b: " + Message()`, or just
`Message()` for an empty path. `Path()`/`Message()` exist so a caller (rr's
Standard Schema `{message, path}` issues) can place each failure next to its
form field without parsing `Error()`. The field is `Keys` because a method
can't share the field's name. `*ParseError` has the same `Keys` field and
`Path()`/`Message()` (Message = `Err`'s text) but no `Rule`, so it is not a
`ValidationError`.

## `Pos` (failure location)

Every concrete error carries a `Pos int` (first field) — the byte offset of
the failure **relative to the full payload**, injected by codegen right after
the struct-literal opening brace (`withPos`/`posLit` in `generate.go`, wrapping
the `onErr` closure plus the standalone required / array-len / dup-key /
unknown-key literals):

- **Bytes path** — the cursor `i`, a true index into `data`. NESTED decoders
  run on `data[i:]`, so their errors surface sub-slice-relative and the call
  site rebases them by the value start (`ggen.NewParseErrShift`, or
  `Errors.Collect`'s shift argument in the multierr drain). Both go through
  the unexported `relocate(e, segment, d)` over the interface's `at()`;
  `NewParseErrShift`'s `shiftPos` also walks into a `*ParseError`'s cause,
  since a fallible mod's `ModError` is born pre-wrapped and would otherwise
  keep a sub-slice-relative `Pos` (`TestParseErrShiftCascadesToModError`). Pinned by `TestNestedValidationPath_Complete` +
  `TestNestedMultierr_drainsInnerValidationErrors` (bytes == stream).
- **Stream path** — `ggen.Stream.Offset()` (= `consumed + Pos`), NOT the raw
  buffer-relative `s.Pos`: the stream buffer compacts as it slides, so only
  `Offset()` stays relative to the whole payload. Already global at every
  depth — stream call sites keep plain `NewParseErr`, with one exception: the
  cross-package `UnmarshalJSON` rung and the struct alias's `UnmarshalJSON`
  rung run their callee on a CAPTURED span, so they wrap with `NewParseErrShift(field, s.Offset(), len(span), err)` to rebase
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

## A bare `Errors` means the value was read whole

Only a multierr decoder returns `Errors`, and only at its closing brace.
`NewParseErr` cuts a nested `Errors` down to its first entry, because a
decoder that calls it stops in the middle of its own value: a fail-fast
parent of a multierr child hands up one error. The same holds in the other
direction: a multierr decoder that stops early (a fail-fast child failed)
returns a single error, and `Errors.Collect` takes only a bare `Errors`, so
its multierr parent stops as well instead of reading on from inside the
child (`TestMultierr_NoCursorDesyncOnStoppedMultierrCallee`). `Stream.Array` and
`Stream.Seq` rely on that to yield an element with its `Errors` and go on
to the next one instead of ending. `UnmarshalSlice` and `Stream.Slice` read
on past such an element, collect every element's `Errors` (index-prefixed on
the bytes path) and return them with the whole slice after `]`, so a decoder
built on `Slice` keeps the rule too. Pinned by the `TestMultierr_*` tests
around `DrainFailFast` in `mods_test.go`.

## Concrete error structs (one per rule)

Pointer-receiver structs, all implement `ggen.ValidationError`. Each carries
a `Pos int` and a root-relative `Keys []string` (both first). The interface
is sealed by the unexported `at() (*int, *[]string, **pathBuf)`, which hands
the runtime pointers to both and to the path buffer: `relocate` shifts `Pos`
and prepends a key through it, so `NewParseErr`, `NewParseErrShift`,
`Errors.Collect` and `UnmarshalSlice` need no type assertions. `Keys` is the
tail of a `pathBuf` that has free slots in front and doubles when full
(`prepend`), so a path of d segments costs O(d) to build, and under multierr
each collected failure pays O(d) on its way out. A recursive type lets the
input pick d up to the depth cap, so a per-level copy would make one failure
quadratic and a multierr report cubic. Keys that are not
the buffer's current tail (a literal, a caller's slice, the view of an error
copied by value) are copied first and never written to. `ParseError` keeps
its `Keys` the same way. Pinned by `TestRelocate_PathGrowsInFront` +
`TestRelocate_PathAllocs`. Shapes below list the remaining
fields:

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
  frozen package-level slice (see "Frozen OneOf and Keys slices")
- **patterns**: `URLError`/`AlphanumError`/`NumericError`/`LowerError`/
  `UpperError`/`HexadecimalError{Value string}`. `alphanum`, `numeric` and
  `hexadecimal` reject `""` (`IsAlphanum`/`IsNumeric`/`IsHex` need at least
  one byte), `islower`/`isupper` accept it
- **prefix/suffix/contains**: `StartsError`/`EndsError`/`ContainsError{Want, Value string}`
- **other**: `MultipleError{Of any, Value any}`, `DuplicateKeyError`,
  `UnknownKeyError`, `CustomError{Name string, Value any, Cause error}` (exposes
  `Unwrap()`; `Name` is the bare func identifier). Custom bool-form validators
  fail with `PredicateError{Name, Msg, Value}`; fallible bool-form mods fail with
  `ggen.ModError` (a parse error, documented in `.claude/decode.md`, not here)

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
    // minlen.Keys, minlen.Limit, minlen.Got
}
```

Use the typed pointer struct, or `errors.AsType[ggen.ValidationError](err)` for
the rule, path and message.
`Errors` (from `multierr`) and `CustomError` implement `Unwrap()`.

## Frozen OneOf and Keys slices

`OneOfError.Allowed` points to a deduped package-level frozen `[]string`
(`var ggenOneof_<scope>_N = []string{...}`) emitted once per unique allowed-set,
and every constant one-key `Keys` path (validation rules, `required`,
duplicate keys) to a `var ggenKeys_<scope>_N = []string{"key"}`, so error
construction allocates neither slice. `prepend` never writes to a `Keys` it
does not own as a `pathBuf` tail, so sharing is safe (see cmd/ggen/CLAUDE.md optimization
#13).
