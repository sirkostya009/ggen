# ggen — zero-copy, zero-reflection JSON codegen for Go

Code generator. Parses annotated Go structs, emits `Marshal` / `Unmarshal`
methods that beat every reflection-based library (jsonv2, sonic, easyjson).
All decode work is hand-rolled byte scanning over the caller's `[]byte`;
strings alias the input via `unsafe.String` — no copy, no tokens, no AST.

Module: `github.com/sirkostya009/ggen`. Binary: `ggen` (CLI). Go ≥ 1.26.

## Repo layout

```
schema/
├── main.go, parse.go, generate.go, tags.go, types.go   ← CLI (package main)
├── introspect.go                                       ← go/types-driven interface detection (TextAppender, TextMarshaler, …)
├── parse_test.go, tags_test.go                         ← CLI tests
├── shared_test.go                                      ← shared demo structs (Address, Node, …) used across feature tests
├── schema_ggen_test.go                                  ← generated decoders for the test structs
├── *_test.go                                           ← feature + roundtrip + compat + fuzz tests
├── fuzz_test.go                                        ← FuzzScanNoPanic / FuzzRoundtrip / FuzzCompat
├── decode/                                             ← runtime: Decoder interface + top-level generics
├── encode/                                             ← runtime: AppendString helpers + Marshal/Write/Slice generics
├── scan/                                               ← runtime: hand-rolled JSON scanner + streaming Stream type
├── decode/validation/                                  ← typed validation error structs (one per rule)
├── thirdparty/                                         ← non-annotated external type — exercises encoding/json fallback
├── thirdparty2/                                        ← annotated external type — exercises static analyzer pickup of cross-pkg generated decoder
└── bench/                                              ← separate Go module (own go.mod) — all benchmarks live here
```

`bench/` is its own **separate Go module** (`bench/go.mod` with
`replace github.com/sirkostya009/ggen => ../`). Two reasons:

1. easyjson's codegen bootstrap compiles a non-test build, which can't
   see types in `_test.go` files. The bench module has `types.go`
   (ggen- + easyjson-annotated Node) + both codegens side by side.
2. The reference codecs (`sonic`, `easyjson`) and their large
   transitive dep set (bytedance/gopkg, cloudwego/base64x,
   klauspost/cpuid, golang/asm, …) stay out of the root module's
   `go.mod`. End users `go get`ing `github.com/sirkostya009/ggen`
   pull only the minimal deps (uuid + `golang.org/x/tools`) and
   never see the benchmark world.

The root module's tests cover correctness; the bench module holds
**all** benchmarks (Mega payload + reader paths + retention +
slow-stream). The root has no `Benchmark*` funcs.

## Architecture — decode path

Only ONE decoder architecture now (the old jsontext-based path was deleted).
All generated `Unmarshal`/`DecodeFrom`/`UnmarshalStream`/`DecodeStreamFrom`
methods use the hand-rolled `scan` package directly.

**`scan` package** (`scan/scan.go`, `scan/stream.go`):

- `[]byte`-based: `SkipSpace`, `String`, `Int64`, `Uint64`, `Float64`, `Bool`,
  `ObjectOpen`, `ArrayOpen`, `SkipValue` — all operate on `(data, pos)`,
  return `(value, newPos, error)` or just `(newPos, error)`. Zero-alloc on
  the happy path. Sentinel errors (`scan.ErrBadObject` etc.).
- `String()` uses `unsafe.String(unsafe.SliceData(data[start:]), len)` for
  zero-copy aliasing when no escape sequences. Falls back to `stringSlow`
  with `utf8.AppendRune` for `\uXXXX` + surrogates.
- `Stream` type wraps `io.Reader` with a growable internal buffer
  (`buf []byte` grown via `append`). The single I/O primitive is
  `ReadMore(keep int) error`: one Read call per invocation, never
  loops. `keep` is the lowest offset the caller still needs — bytes
  before it are eligible for discard:
    - `keep == 0` grows without shifting (alloc bigger backing if
      currently full). Buffer offsets stay stable; aliases survive.
    - `keep == len(buf)` resets the buffer to `[:0]` and refills
      from offset 0. Same effect as a full compaction.
    - `0 < keep < len(buf)` performs an in-place `memmove` of
      `buf[keep:n]` down to `buf[0:n-keep]`, then reads into the
      freed tail. Aliases into the buffer are invalidated whenever
      `keep > 0` — the bytes physically move on the same backing
      array, so any string alias the caller still holds points at
      wrong content after the call.
  Internal Stream methods compact aggressively: `SkipSpace`,
  `ConsumeColon`, `Int64`/`Uint64`, and `String`/`KeyView` all pass
  a non-zero `keep` (current cursor `i`, or the value-start `start`
  for spans that need to outlast the loop) so the buffer stays
  bounded at roughly `max(chunk_size, single_value_size)` even
  across long streams. Each method updates its own cursors after
  the shift (`i = 0`, or `j -= start; start = 0` for the
  string-body case). The `noShift` mode disables this for `SkipValue`
  inside RawJSON capture and `json.Unmarshal` fallback spans, where
  the generated code needs stable absolute offsets to slice
  `s.Bytes()[start:i]`; bookkeeping branches in SkipSpace/etc check
  `s.noShift` before resetting the cursor.
  Generated code adds two more shift points at the dispatch-loop
  boundary: `ReadMore(i); i = 0` after `ObjectOpen+SkipSpace` and
  after the per-iteration value decode + SkipSpace. Each known-key
  case opens with `s.ConsumeColon(i)` — the alias from `KeyView` is
  no longer needed past dispatch, so the shift it triggers is safe.
  `UnknownKeyError` and the inline-catch-all map key both detach
  the alias with `strings.Clone(key)` so subsequent compactions
  don't corrupt the stored value.
  Each `(*Stream).X(i)` method does its own bounds check (`if i >=
  len(s.buf) { ... ReadMore(i) ... }`) and proceeds once one new
  byte has landed. Multi-byte literals (`true`, `false`, `null`,
  `\uXXXX`) are scanned **byte-by-byte**: each char triggers an
  individual bounds check + maybe ReadMore, and a mismatch fails
  fast without fetching the rest. This is the lazy-streaming
  property — parse-what-you-have, fetch one chunk only when truly
  stuck. See "tried and rejected" for the older `Ensure(p *int, n
  int)` + `Anchor`/`Unanchor` design that bulk-fetched N bytes via
  an internal Read loop.
- `Stream` is **stack-allocatable**, no internal pool: `var s scan.Stream;
  s.Reset(r, buf)`. Caller owns `buf` lifecycle. There used to be an
  `Acquire`/`Release` pair around a `sync.Pool` of Streams; the pool was
  removed because it bundled too many implicit-lifetime assumptions about
  the buffer and led to silent corruption when callers reused buf across
  decodes. Honest API now: caller knows what their buffer does.
- `Stream.String` and `Stream.Number` **copy** their content out via
  `string(s.buf[start:end])` rather than aliasing — owned values, safe
  with map keys, decoder output detached from buf. The bytes path still
  aliases (caller owns input there). The streaming path traded ~230K
  extra allocs/Mega for safety + simplicity. Under "tried and rejected"
  for why aliasing-on-stream + arena-compact was abandoned.

**`decode` package** (`decode/decode.go`):

```go
type Decoder[T any] interface {
    DecodeFrom(data []byte, i int) (T, int, error)
    Unmarshal(data []byte) (T, error)
    DecodeStreamFrom(s *scan.Stream, i int) (T, int, error)
}

func Unmarshal[T Decoder[T]](data []byte) (T, error)
func Read[T Decoder[T]](r io.Reader) (T, error)                 // io.ReadAll + Unmarshal
func UnmarshalSlice[T Decoder[T]](data []byte) ([]T, error)
func ReadSlice[T Decoder[T]](r io.Reader) ([]T, error)
// Streaming entry points — buf is a reusable working area; pass nil for
// fresh, or a pre-sized / pooled slice. Returned []byte is the
// (possibly grown) buffer; safe to recycle immediately since strings
// inside the value are owned copies.
func UnmarshalStream[T Decoder[T]](r io.Reader, buf []byte) (T, []byte, error)
func UnmarshalSliceStream[T Decoder[T]](r io.Reader, buf []byte) ([]T, []byte, error)
func UnmarshalStreamRequest[T Decoder[T]](req *http.Request) (T, []byte, error)
func UnmarshalStreamResponse[T Decoder[T]](resp *http.Response) (T, []byte, error)
```

**`encode` package** (`encode/encode.go`):

- `Marshaler` interface: `AppendJSON(dst []byte) ([]byte, error)` +
  `JSONSize() int`. Errors propagate from any nested encoder that can
  fail (nested AppendJSON, TextAppender, TextMarshaler, JSONMarshaler,
  `encoding/json.Marshal` fallback).
- `Marshal[T] (v Marshaler) ([]byte, error)`: `append` into
  `make([]byte, 0, v.JSONSize())`. Single alloc on the happy path.
- `MarshalString[T] (v Marshaler) (string, error)`: same but with
  `BytesToString` aliasing the buffer (zero extra alloc).
- `Write(w io.Writer, Marshaler) error` — pooled buffer, returns first
  non-nil error from AppendJSON or the writer.
- `MarshalSlice` / `MarshalSliceString` / `WriteSlice` / `AppendSlice` —
  all error-returning equivalents for `[]T` of `Marshaler`s.
- `JSONSize()` is an intentional upper-bound overshoot. Map estimate is
  per-entry — `4 + 2*len(key) + value-bound` (kind-derived) or flat 128
  for variable values; string is `len*2+2` for short-escape worst-case
  (`\n`, `\"`, `\\`, `\t`, `\b`, `\f` — every byte becomes 2). Control
  chars below 0x20 that have no short escape expand to `\uXXXX` (6×)
  and DO overflow the bound; we accept the one-time realloc on that
  pathological input since real-world payloads rarely contain raw
  control bytes. Constant per-field contributions are folded into the
  initial `size := N` at codegen time; only loops/len() emit runtime
  adds. `TestJSONSize_NoReallocOnWorstCase` pins the cap-guarantee on
  realistic worst-case input.
- `BytesToString(buf []byte) string` — unsafe.String over buffer.
- `AppendString(dst, s)` — escaped string body + closing `"`. The CALLER
  is responsible for the opening `"`: codegen folds it into the constant
  `"key":"` prefix at struct-field top level, or emits an explicit
  `dst = append(dst, '"')` at slice/map/standalone sites. HTML-safe
  variant — escapes `<`, `>`, `&` to `\uXXXX`, matches stdlib v1. Codegen
  routes here when the parent struct opts in via `htmlescape` /
  `-htmlescape`.
- `AppendStringNoHTML(dst, s)` — default variant: standard JSON escapes
  only, emits `<`, `>`, `&` literally. Matches stdlib jsonv2 (which
  dropped HTML escaping as a default).

That's the entire encode surface — base64/base32/hex/bytes-array and
net.IP/netip helpers are inlined directly in generated code (just
`base64.StdEncoding.AppendEncode(...)` / `ip.AppendText(...)` between
`'"'` byte appends), so the encode package stays small.

**`validation` package** (`decode/validation/validation.go` — subpackage of `decode`):

- `type Error interface { error; Rule() Rule }` — interface satisfied by
  every typed failure struct in this package.
- `type Rule string` — constants: `Required`, `NotEmpty`, `Len`, `MinLen`,
  `MaxLen`, `Runes`, `MinRunes`, `MaxRunes`, `GT`, `GTE`, `LT`, `LTE`, `Eq`,
  `Neq`, `OneOf`, `Email`, `URL`, `ASCII`, `Printable`, `Alphanum`,
  `Numeric`, `Lower`, `Upper`, `Hexadecimal`, `Starts`, `Ends`, `Contains`,
  `Multiple`, `DuplicateKey`, `UnknownKey`, `Custom`.
- One concrete pointer-receiver struct per rule, all implementing
  `validation.Error`:
    - presence: `RequiredError{Field}`, `NotEmptyError{Field}`
    - length: `LenError{Field,Want,Got int}`, `MinLenError`/`MaxLenError{Field,Limit,Got int}`
    - runes: `RunesError`, `MinRunesError`, `MaxRunesError` (same shape as length)
    - numeric range: `GTError`/`GTEError`/`LTError`/`LTEError{Field, Limit float64, Value any}`
    - equality: `EqError`/`NeqError{Field, Want any, Value any}` (used for both string and numeric fields)
    - oneof: `OneOfError{Field, Allowed []string, Value any}` — `Allowed` always points to a frozen package-level slice emitted by codegen (no per-error alloc, see optimization #16)
    - patterns: `EmailError`/`URLError`/`ASCIIError`/`PrintableError`/
      `AlphanumError`/`NumericError`/`LowerError`/`UpperError`/`HexadecimalError{Field, Value string}`
      (`URLError` also has `Cause error` + `Unwrap()`)
    - prefix/suffix/contains: `StartsError`/`EndsError`/`ContainsError{Field, Want, Value string}`
    - other: `MultipleError{Field, Of float64, Value any}`,
      `DuplicateKeyError{Field}`, `UnknownKeyError{Field}`,
      `CustomError{Field, Name string, Cause error}` (exposes `Unwrap()`)
- `type Errors []Error` — slice of interface, returned in multierr mode.
  `Unwrap() []error` supports `errors.Is`/`errors.As`.
- Inspect a specific failure with `errors.As(err, &validation.MinLenError{})`
  etc.; the legacy `Rule` field is gone — use the typed pointer instead, or
  `err.(validation.Error).Rule()` if you only need the name.

## Generator CLI (`main` package)

### Invocation

```
ggen ./...                    walk all packages from cwd
ggen <dir>                    one package
ggen <file.go> [Names...]     single file; optional struct name filter
```

The CLI loads packages via `golang.org/x/tools/go/packages` with full type
info, so each field's interface implementations (TextMarshaler,
ByteDecoder, JSONMarshaler, …) are detected at generate time and the
generator emits hardcoded method calls instead of runtime probes. When
the loader can't resolve types (e.g., a temp file with no `go.mod`), the
generator falls back to AST-only mode and emits a plain `encoding/json`
fallback for cross-package types.
**Important: run `ggen` with the same `GOEXPERIMENT` env that the user's
code is built with** — packages.Load honors build tags, so files behind
`goexperiment.jsonv2` are otherwise invisible.

### Flags (all opt-in, apply to every struct in the pass)

- `-o <path>` — override output path (single file / single dir only)
- `-pkg <name>` — override package name in output
- `-marshal` — emit `MarshalJSON` hook (satisfies `encoding/json.Marshaler`)
- `-unmarshal` — emit `UnmarshalJSON` hook (satisfies `encoding/json.Unmarshaler`)
- `-multierr` — accumulate all validation failures into `validation.Errors`
  instead of returning on first. Appends instead of returns.
- `-allowdups` — accept duplicate JSON keys with first-wins semantics
  (later occurrences advance past via `scan.SkipValue` without being
  decoded). Default: error on second occurrence with
  `validation.DuplicateKeyError`.
- `-novalidate` — skip validation rules, required-field checks, and mods.
  Fastest path. Trade-off: user is responsible for correctness.
- `-ignoreunknown` — silently skip unknown JSON keys. Default: error with
  `validation.UnknownKeyError`. Overridden when an inline map field
  is present (absorbs unknowns into the map).
- `-nosortkeys` — emit struct fields in Go declaration order. Default: sort
  alphabetically by JSON name at codegen time (zero runtime cost,
  compression-friendly). Inline fields stay last.
- `-usenumber` — decode JSON numbers into `any` fields as `json.Number`
  (string newtype, zero-copy aliased over input) instead of `float64`.
  Mirrors stdlib's `json.Decoder.UseNumber()`. Use when source data has
  int64s above 2^53 or you need exact-digit preservation.
- `-htmlescape` — opt INTO HTML-safe escaping (`<`, `>`, `&` → `\uXXXX`)
  on marshal. Default is literal, matching stdlib `encoding/json` v2
  (which dropped HTML escaping as a default). Set this when the JSON
  payload is being embedded directly in HTML and the consumer doesn't
  escape on its own.

### Per-struct annotations

Comment on the struct (or gen-decl) is a `//ggen:generate` directive
(no space between `//` and `ggen`, mirroring `//go:generate`).
Space-separated tokens after:

- `marshal` — same as `-marshal` but per-struct
- `unmarshal` — same as `-unmarshal`
- `multierr`
- `allowdups`
- `novalidate`
- `ignoreunknown`
- `nosortkeys`
- `usenumber`
- `htmlescape`

CLI flags OR struct annotations trigger. Struct annotations are local
per-struct; CLI flags apply to all.

### Struct tags (on fields)

- `json:"name"` — JSON key name. Required for useful output.
- `json:"-"` — field ignored entirely.
- `json:",inline"` — catch-all map for unknown keys. Field type must be
  `map[string]any` or `map[string]interface{}`. Unknown keys absorbed into
  the map (overrides `ignoreunknown`). Entries spliced out on marshal.
- `json:"name,omitempty"` — skip on marshal when JSON-empty (null, "", [], {}).
- `json:"name,omitzero"` — skip on marshal when Go-zero value.
- `json:"name,string"` — wrap primitive as JSON string on marshal, unwrap on
  unmarshal. Supports string/bool/int*/uint*/float\*.
- `json:"name,format:X"` — format hint for native types (see Kinds below).
  **jsonv2 requirement: `format:X` must be the LAST option in the tag.**

- `ggen:"..."` — validation rules, comma-separated. Three mode prefixes
  partition the rule list:
    - (no prefix, default) — applies to the field itself (or whole slice /
      map for container fields).
    - `dive:` — rules after apply to the next level down. Each additional
      `dive:` peels one more level: for `[][]T` the first dive targets each
      `[]T`, the second targets each `T`. Works for arbitrarily-deep slices.
    - `keys:` — rules after apply to map keys only (always `string`, so any
      string-category rule is valid).
      Prefixes can appear in any order. Example:
      `ggen:"minlen=1,dive:maxlen=10,dive:required,keys:minrunes=2,maxrunes=32"`.
      Supported rules:
    - `required`, `optional`, `notempty`
    - `len=N`, `minlen=N`, `maxlen=N` (just calls len on strings/slices/maps)
    - `hintlen=N` — _preallocation hint_, NOT a validation rule. Lifted out
      of the tag at parse time. Overrides `len`/`maxlen`/`minlen` for sizing
      `make([]T, 0, N)` and `make(map, N)`. Useful when the payload is
      expected to land near N but the validation-derived bound is much
      larger (or absent).
    - `runes=N`, `minrunes=N`, `maxrunes=N` (utf8.RuneCountInString)
    - `gt=N`, `gte=N`, `lt=N`, `lte=N`, `eq=N`, `neq=N` (numeric; or string eq/neq on strings)
    - `oneof=a|b|c`
    - `email`, `url`, `ascii`, `printable`, `alphanum`, `numeric`,
      `lower`, `upper`, `hexadecimal`
    - `starts=X`, `ends=X`, `contains=X`
    - `multiple=N`
    - `@FuncName` / `@pkg.FuncName` — user-defined validator resolved at
      codegen time. Signature must be `func(T) error` where `T` is the
      exact field type (incl. `*T` for pointer fields). Looked up via
      `packages.Load`; non-nil return wraps as `validation.CustomError{
      Name: "@FuncName", Cause: err}`. No runtime registry — direct call
      site, zero alloc, type-checked at generate time. Cross-package
      lookup uses the source file's import block; file-scoped aliases
      and blank imports (`_ "path/to/lib"`) both resolve.

- `mod:"..."` — input transforms, applied after decode and before validation.
  Same `dive:` / `keys:` prefixes as `ggen`. Rules:
    - String: `trim`, `lower`, `upper`, `trimleft=X`, `trimright=X`,
      `replace=old|new`.
    - Numeric: `clamp=lo|hi` — rounds the value into `[lo, hi]`. Either
      bound can be empty to clamp only one side (`clamp=0|` floors at 0 with
      no upper cap; `clamp=|100` caps at 100 with no floor).
    - `@FuncName` / `@pkg.FuncName` — user-defined transform. Two
      signatures supported:
        - **pure**: `func(T) T` — generated code emits `field = Func(field)`
        - **fallible**: `func(T) (T, error)` — non-nil error propagates as
          a parse error (early return, same level as `scan.ErrBadString`),
          NOT a validation error
      Pure vs fallible is detected from the function signature at codegen
      time. Same lookup rules as custom validators (cross-pkg via source
      file imports). Zero alloc, no registry, type-checked.
      Note: mods break zero-copy for the modified field (force a new string).

### Rule applicability (parse-time)

`applicability.go` rejects mismatched validation/mod rules at parse
time so the user gets a clear diagnostic instead of a downstream Go
compile error in the generated file. The matrix lives in
`checkOneValRule` / `checkOneModRule`:

- String-only validators (`email`, `url`, `ascii`, `printable`,
  `alphanum`, `numeric`, `lower`, `upper`, `hexadecimal`, `starts`,
  `ends`, `contains`, `runes`, `minrunes`, `maxrunes`) — KindString only.
- Numeric validators (`gt`, `gte`, `lt`, `lte`) — any numeric kind.
- `multiple` — integer kinds only (modulo `%` doesn't compile on floats).
- `eq`/`neq`/`oneof` — string or numeric.
- `len`/`minlen`/`maxlen`/`notempty` — any len-able kind
  (string/slice/array/map/[]byte).
- String mods (`trim`, `lower`, `upper`, `trimleft`, `trimright`,
  `replace`) — KindString.
- `clamp` — numeric.
- `hintlen` — slice/map only.
- `dive:` only on slice/array/map/[]byte; `keys:` only on maps.
- KindStruct fields are SKIPPED — they may be primitive aliases or
  carry custom marshalers we can't type-check at parse time.
- Custom `@Func` rules are skipped by applicability; `resolveCustomRules`
  has already validated signature against the field's exact go/types type.
- Numeric/integer value parameters (`gt=abc`, `len=1.5`, `minlen=`)
  are rejected with their own diagnostics — same parse-time path.

Cases covered exhaustively in `TestCLI/InvalidRuleApplication` —
add a table entry there when a new rule lands.

### Top-level type aliases

Annotated named types (`//ggen:generate type T <underlying>`) get the
same method surface as a struct (DecodeFrom, DecodeStreamFrom, JSONSize,
AppendJSON). What the body looks like depends on the underlying kind,
all driven by `renderAlias*` helpers in `alias.go`. The top-level
renderers (`renderDecode`, `renderStreamDecode`, `renderSize`,
`renderAppendJSON`) dispatch to alias paths when `s.IsAlias` is set
(except for struct aliases that fall back to field introspection — see
below — which sets `IsAlias=false` and routes through the regular
struct codegen).

Accepted underlying kinds, with the codegen path for each:

- **primitive** (`string`, `bool`, `int*`, `uint*`, `float*`): scan via
  `scan.X` / `_s.X`, cast to alias type. `htmlescape` opt-in flips the
  string-append helper.
- **struct** (`type LocalUUID uuid.UUID`, `type Local Inner`): three-
  step ladder. Methods don't propagate from `Inner` to `type Local
  Inner`, so probing uses `inspectType` on the RHS named type
  (resolved via `typesInfo.Uses[ident]`).
    1. *ggen-method delegation*: if underlying already has
       AppendJSON+DecodeFrom, emit cast → underlying.Method() → cast
       back. Cheapest path — the underlying already has hand-rolled
       fast code.
    2. *field introspection*: if the underlying is a plain struct
       with at least one exported field, walk the underlying
       `*types.Struct` via go/types and synthesize a FieldInfo per
       exported field via `extractFieldFromTypes`. `IsAlias` flips to
       false and the regular struct codegen runs. Field access via
       `result.X` is sound because the alias and the underlying have
       identical memory layout. **Preferred over JSON/Text marshaler
       delegation** even when those methods exist — hand-rolled
       struct codegen is faster than reflective JSON marshaler calls,
       and this is the codepath that turns a thirdparty struct into
       a fast ggen-decoded type.
    3. *JSON/Text marshaler delegation*: opaque struct (no exported
       fields, e.g. `time.Time`) with a JSON or Text marshaler pair —
       cast to underlying, drive its method, cast back. Same pattern
       as ggen delegation but with stdlib hooks.

  Wire-shape implication: an alias of a struct with both exported
  fields and a custom MarshalJSON will use the introspected
  field-driven shape, NOT the underlying's MarshalJSON output. If
  you want the underlying's exact wire shape, declare the struct
  with no exported fields (force fallback to delegation) or write
  your own marshal hook.
- **slice / map / array** (`type Tags []string`, `type Lookup
  map[string]int`, `type Tuple [3]int`): build a synthetic FieldInfo
  capturing the container shape (ElemType, ElemKind, ArrayLen) and
  hand it to the existing field-level emitters (`renderSlice`,
  `renderStreamSlice`, `emitByteArrayRead`, `emitStreamSliceRead`,
  `renderMap`, `renderStreamMap`, `renderAppendSlice`,
  `renderAppendMap`) with `result` (decode) or `s` (encode) as ref.
  All the field-level features carry over — strict-length arrays,
  slabbed `[]*T`, dive validation if you ever attached a tag.
- **`[]byte` alias**: collapses to KindBytes, routes through base64.

Rejected: channel, interface, function — no sensible JSON shape.

Validation rules and mods don't apply to primitive aliases (no field
tag site to attach them to). Struct-level annotations like
`htmlescape` / `marshal` / `unmarshal` apply where they make sense;
`allowdups`, `ignoreunknown`, `multierr`, `novalidate` are no-ops
since aliases have no JSON-object dispatch (except for struct aliases
in introspection mode, which inherit them like regular structs).

Import handling: `aliasUnderlyingImports` collects the foreign-pkg
path when a struct alias delegates to a non-stdlib underlying (so the
generated file can write `_u := uuid.UUID(s)`). Field-introspection
mode types come out via `types.RelativeTo(s.typesPkg)` so foreign
field types render as `pkgname.X`; the field-level cross-pkg dispatch
takes care of any methods on those types.

### Supported Go Kinds (per field)

- `string`, `bool`
- `int`, `int8`, `int16`, `int32`, `int64`, `uint`, `uint8`, `uint16`, `uint32`, `uint64`
- `float32`, `float64`
- Pointer to any of the above (`*T`) — null ↔ nil
- `[]T` (slice), `map[string]V` (string-keyed only)
- `[]*T` / `[N]*T` (slice or array of pointer-to-struct) — element
  pointers come from a single backing slab (`make([]T, 0, cap)` for
  slices, `[N]T` for arrays) so N allocs collapse to ~log(N) on
  decode. Nil elements decode as nil pointers; on encode nil → `null`.
- Nested struct (same-package: direct `DecodeFrom` call; cross-package:
  fallback to `encoding/json.Unmarshal` over a raw-bytes span captured via
  `scan.SkipValue`)
- Embedded struct (unnamed field) — fields promoted to parent's JSON object
- `time.Time` — `format:unix` (int), `unixmilli`, `unixmicro`, `unixnano`,
  `RFC3339`, `RFC3339Nano` (default), other `time.X` constants, or a custom
  layout string like `format:'2006-01-02'`
- `time.Duration` — `format:sec` (float seconds), `milli`, `micro`, `nano`,
  `units` (default, parses `"1h30m"`)
- `net.IP`, `netip.Addr`, `netip.Prefix` — text form. Marshal goes through
  `encoding.TextAppender` (Go 1.24+ `AppendText(dst []byte) ([]byte, error)`)
  for zero-alloc emit; decode uses kind-specific parsers (`net.ParseIP`,
  `netip.ParseAddr`, `netip.ParsePrefix`).
- `[]byte` — `format:base64` (default), `base64url`, `base32`, `base32hex`,
  `base16`/`hex`, `array` (JSON array of numbers)
- `json.RawMessage` / `jsontext.Value` — opaque JSON span, captured via
  `scan.SkipValue` and aliased into the field. Zero-copy decode, raw
  passthrough on encode (or `null` if empty/nil).
- `net/url.URL` — JSON string, decoded via `url.Parse`, encoded via `String()`.
- `math/big.Int` / `big.Float` / `big.Rat` — arbitrary-precision numeric
  types. `big.Int` is a JSON number (no quotes). `big.Float` and `big.Rat`
  are JSON strings (`"3.14"` / `"3/2"`) — wrapping prevents `float64`
  precision loss at the consumer and matches jsonv2's wire format for
  `big.Float`. All encoded via in-place `Append` (zero alloc on marshal),
  parsed via `SetString` / `Parse`. Useful for crypto / financial values
  that don't fit in int64.
- UUID and other text-encoded types — no dedicated kind. The static
  analyzer picks up any field whose type implements
  `encoding.TextAppender` / `TextMarshaler` / `TextUnmarshaler` and
  routes through the cross-package dispatch. Covers `google/uuid`,
  `gofrs/uuid/v5`, `shopspring/decimal`, `oklog/ulid`, `segmentio/ksuid`,
  `rs/xid`, `net/mail.Address`, custom enum-strings, etc. — anything
  that implements those stdlib interfaces. No special-case code, no
  per-lib imports in generated output, no per-alias detection fragility.
  Decode emits `ref.UnmarshalText(unsafe.Slice(...))`, alloc-free.
  Marshal prefers `AppendText(dst)` (zero alloc) and falls back to
  `MarshalText() + AppendString` (one alloc — the lib's MarshalText
  return) when the type only implements the older interface.
- `database/sql.Null*` family (`NullString`, `NullInt64`, `NullInt32`,
  `NullInt16`, `NullByte`, `NullBool`, `NullFloat64`, `NullTime`). Decode
  probes `null` literal first → `Valid=false`; otherwise reads the inner
  value with the kind-appropriate scanner and constructs the literal with
  `Valid=true`. Encode emits `null` when `!Valid`, the inner value
  otherwise.
- `any` / `interface{}` — decode is hand-rolled via `scan.Any` /
  `(*Stream).Any`, matching stdlib `encoding/json` defaults: `null→nil`,
  `true/false→bool`, `number→float64`, `string→string` (zero-copy alias),
  `array→[]any`, `object→map[string]any`. With the `usenumber` flag/anno,
  the generator emits `scan.AnyNumber` instead — numbers become
  `json.Number` aliased over the input via `unsafe.String` (zero-alloc,
  zero-copy on the happy path; same fast path as `String`). Encode goes
  through `encode.AppendAny` — type-switches over runtime primitives,
  falls into reflection for slices/arrays/maps/pointers/structs (with
  json-tag parsing for struct walking), keeping nested ggen `Marshaler`
  / `TextAppender` types on the fast path with no `json.Marshal` cliff.
- `[N]T` (fixed-length array) — JSON tuple with **strict count**: decode
  errors with `validation.LenError{Want: N}` when the JSON array
  has more or fewer than N elements. Combines freely with slices: `[N]T`,
  `[][N]T`, `[N][]T`, `[N][M]T`, `[][N][M]T` all work and nest through the
  same recursive emitter used for `[][]T`. `[]byte` stays `KindBytes`
  (base64 path) — only non-byte arrays get tuple treatment.

### Wire-format divergences from stdlib

Two field kinds intentionally diverge from `encoding/json` (v1) and
`encoding/json/v2`. ggen-generated marshal output is *not* a strict subset
of either stdlib path's output for these types — feed-through-stdlib will
reshape the value, and decode of stdlib-produced JSON likewise won't work
for these fields. Round-trip within ggen itself is fine.

| Kind          | ggen wire             | stdlib wire (v1 + v2)                                    |
| ------------- | --------------------- | -------------------------------------------------------- |
| `net/url.URL` | `"https://x/p?q=1"`   | `{"Scheme":"https","Host":"x","Path":"/p", … 11 fields}` |
| `sql.Null*`   | inner value or `null` | `{"<Inner>":val,"Valid":true}` (plain struct, no hook)   |

Reasons:
- **`url.URL`**: stdlib's struct dump is unusable for any external API
  consumer; every web service uses URL-as-string. ggen ships the
  ergonomic shape.
- **`sql.Null*`**: stdlib leaks the `Valid` flag onto the wire — every
  database driver and downstream consumer expects "value or null"
  semantics. ggen ships the driver convention.

These are stable design decisions; the corresponding fixtures are
excluded from `stdcompat_test.go` cross-compat with this file as the
reference for why.

### Build tag propagation

Generator reads `//go:build <expr>` from each source file individually
and BUCKETS annotated structs by their constraint. Each (tag, isTest)
bucket emits its own gen file with the matching header — so a struct
in `tagged.go` (behind `//go:build foo`) never lands in the
unconstrained `<dir>_ggen.go` (which would compile-break unconstrained
builds with "undefined: Tagged"). Old-style `// +build` lines are also
honored; multi-term expressions are canonicalized via
`go/build/constraint.Parse` so equivalent forms bucket together.

Filenames: untagged buckets keep the legacy `<dir>_ggen.go` /
`<dir>_ggen_test.go` naming; tagged buckets become
`<dir>_<slug>_ggen.go` where the slug collapses non-alnum runs into
single underscores (`goexperiment.jsonv2` → `goexperiment_jsonv2`,
`foo && bar` → `foo_bar`). Cross-bucket struct references in the same
package still route through the direct DecodeFrom — `generatedTypes`
is seeded with the union of all buckets before any single bucket runs
through codegen.

### Output file naming

- Package mode (untagged bucket): `<dir-basename>_ggen.go` for non-test
  annotations, `<dir-basename>_ggen_test.go` for test-only. If both
  exist, emits both.
- Package mode (tagged bucket): `<dir-basename>_<tag-slug>_ggen.go`
  (or `_ggen_test.go`); each (tag, isTest) bucket emits its own file.
- Single-file mode: `<basename>_ggen.go` or `<basename>_ggen_test.go`.
  The source file's `//go:build` line, if any, is preserved in the
  generated header.
- `_test.go` sources allowed — the generator treats them as first-class
  inputs. Test-only struct annotations route output to `_ggen_test.go` so
  the generated methods don't bundle with library builds.

## Generated methods (per annotated struct T)

Generated structs implement `decode.Decoder[T]` and `encode.Marshaler` —
that's the entire surface. Top-level entry points (Unmarshal, Read,
UnmarshalSlice, UnmarshalStream, Marshal, MarshalString, Write,
MarshalSlice, MarshalSliceString, WriteSlice, AppendSlice) live in the
`decode` and `encode` packages as generic functions. The generated file
stays small: 4 methods per struct (plus 0–2 opt-in JSON hooks).

Always emitted:

```go
func (T) DecodeFrom(data []byte, i int) (T, int, error)          // recursive entry; satisfies decode.Decoder[T]
func (T) DecodeStreamFrom(s *scan.Stream, i int) (T, int, error) // io.Reader-backed counterpart

func (s T) JSONSize() int                                        // upper-bound for one-alloc Marshal
func (s T) AppendJSON(dst []byte) ([]byte, error)                // core marshal — propagates nested errors
```

Top-level wrappers in the runtime libraries (call these from user code):

```go
decode.Unmarshal[T](data)              decode.UnmarshalSlice[T](data)
decode.Read[T](r)                      decode.ReadSlice[T](r)
decode.UnmarshalStream[T](r, buf)      decode.UnmarshalSliceStream[T](r, buf)
decode.UnmarshalStreamRequest[T](req)  decode.UnmarshalStreamResponse[T](resp)
// stream variants return (T, []byte, error) — caller owns buf, safe to recycle

encode.Marshal(t)            encode.MarshalString(t)        encode.Write(w, t)
encode.MarshalSlice(items)   encode.MarshalSliceString(items) encode.WriteSlice(w, items)
encode.AppendSlice(dst, items)
```

Opt-in (via `//ggen:generate marshal` / `//ggen:generate unmarshal`):

```go
func (s T) MarshalJSON() ([]byte, error)                         // wraps encode.Marshal(s)
func (s *T) UnmarshalJSON(data []byte) error                     // wraps decode.Unmarshal[T](data)
```

## Optimizations applied in codegen (nothing at runtime)

1. **Length-first key dispatch.** Switch on `len(key)` before switching on
   content — wrong lengths reject with one int compare instead of N string
   compares. Nested switches only for lengths with ≥2 fields.
2. **Slice cap from tag hint.** `preallocCap` picks an initial capacity for
   `make([]T, 0, N)`. Precedence: explicit `hintlen=N` > `len=N` >
   `maxlen=N` > `max(minlen, default)` > default (8 primitives, 4 structs).
   Maps get the same treatment via `mapPreallocCap` (without `minlen`, which
   is a weak signal on maps). Eliminates growth reallocs on the hot path.
3. **Field order sorted by JSON name** at codegen time (alphabetical).
   `-nosortkeys` opts back to declaration order.
4. **Inlined scan primitives in hot path.** Generator emits raw byte-compare
   loops for `SkipSpace`, `String` (zero-copy happy path), `Int64`, `Uint64`
   directly into each case body. No function-call overhead.
5. **Duplicate-key guard.** `seen<Field> bool` per non-inline field when
   `!AllowDups`. Check-before-set emits `validation.DuplicateKey` on repeat.
6. **Required-field tracking.** Post-loop `if !seen<Field> { err }` per
   required field. Combined with dup guard — same booleans serve both.
7. **Multierr accumulation.** Conditional `var errs validation.Errors` + all
   validation/dup/required/unknown branches `errs = append(errs, ...)`
   instead of returning. Single `if len(errs) > 0 { return errs }` at
   success paths.
8. **Mod + validation after field read.** `renderMods` → `renderValidationOn`
   (regular path's helper, post-processed to rewrite `return result, err`
   into `return result, 0, err` for the `(T, int, error)` shape).
9. **Pointer fields** emit a 4-byte `null` peek → nil branch, else stack-
   local `var _v <PointeeType>` + recursive inner read + `&_v`.
10. **Cross-package struct fallback (statically dispatched).** The
    generator loads the user's package via `golang.org/x/tools/go/packages`
    with full type info, then for each cross-package field type checks
    method-set membership via `go/types` at codegen time. Based on what
    the type actually implements, it emits a single hardcoded method
    call — zero runtime probes, zero itab lookups. Decode preference order:
    `DecodeFrom` (ggen-generated in another package) → `UnmarshalJSON`
    (stdlib hook) → `UnmarshalText` (text-encoded types: decimal libs,
    ulid/ksuid/xid, mail.Address, custom enum-strings) → `encoding/json`.
    Marshal mirror: `AppendJSON` → `MarshalJSON` → `AppendText` (Go 1.24+
    `encoding.TextAppender`, zero alloc) → `MarshalText` → `encoding/json`.
    Fallback path: when type info isn't available (AST-only loader, used
    in tests with bare temp dirs), the generator emits a plain
    `encoding/json` fallback. Stdlib handles MarshalJSON / UnmarshalText /
    etc. hooks via reflection on its own — slower, but correct for any
    type, and only hit by tests that don't have a full Go module context.
11. **Inline map catch-all.** Unknown keys absorbed via `encoding/json.Unmarshal`
    over captured raw span. Map value type must be `any` / `interface{}`.
12. **Marshal output cap.** `JSONSize()` returns upper bound so `Marshal()`
    uses a single `make([]byte, 0, cap)` + `AppendJSON`. 1 alloc per top-level
    Marshal.
13. **Recursive nested-container emitter.** `emitByteSliceRead` /
    `emitStreamSliceRead` / `emitAppendSlice` / `emitSizeSlice` take a depth
    parameter and unify slice + array handling. When `ElemKind` is
    `KindSlice` or `KindArray`, they recurse via `peelSliceField(f)` +
    `stripOneContainer(typ)`, which strips one `[]` or `[N]` prefix off
    `ElemType` and shifts `InnerValidation[0] → ElemValidation`,
    `InnerValidation[1:] → InnerValidation`. Arrays also carry the N forward
    via `ElemArrayLen` so strict tuple-count checks fire at every level.
    All locals (`kN`, `evN`, `errN`, `iN`, `vN`, `_idxN`) carry the depth
    suffix so nested `[][]T`, `[N][M]T`, `[][N][]T`, etc. don't collide.
14. **Map-key mods + validation.** `keyValidateAndMod` runs immediately
    after the key is read (before `:`), so key-typed rules and mods (from
    `ggen:"keys:..."` / `mod:"keys:..."`) apply before the value is even
    decoded — invalid keys short-circuit.
15. **Marshal error propagation.** `AppendJSON` returns `([]byte, error)`.
    Generator threads errors through every nested call: in-pass struct
    AppendJSON, slice/map element AppendJSON, cross-pkg JSON/Text
    Marshaler hooks, TextAppender, `json.Marshal` fallback. No silent
    `null` swallowing on encode failure. Pure-primitive structs declare
    `var err error; _ = err` and never use it; the compiler elides the
    variable so there's no runtime cost when nothing can fail.
16. **Typed validation errors + frozen OneOf slices.** Every rule has its own
    pointer-receiver struct (`MinLenError`, `OneOfError`, …) implementing
    `validation.Error`. Generator emits the typed literal directly at the
    failure site — no `Rule`/`Param` field-stuffing, no per-error
    rule-name comparison at use sites. `OneOfError.Allowed` always points
    to a deduped package-level frozen `[]string` (`var _oneof_N = []string{...}`)
    emitted once per unique allowed-set, so error construction never
    allocates the allowed slice. `EqError`/`NeqError` use `Want any` so
    one struct serves both string and numeric fields without per-kind
    duplication.
17. **Constant-folded `JSONSize()`.** Each field's size contribution
    splits into a compile-time constant (folded into the initial
    `size := N`) and a runtime expression (loops, len(), recursive
    calls). Pure-primitive structs collapse to a single `return N`; only
    variable-size kinds (string, slice, map, nested struct) emit runtime
    adds.
18. **Opening-quote folding.** At struct-field top level, when a field's
    value emit begins with `"` (string, URL, big.Rat, time/RFC3339,
    duration/units, base64/hex bytes, net.IP/netip), the opening quote
    is folded into the constant `"key":` prefix → `"key":"`. The value
    emitter then writes only the body + closing `"`. Saves one
    byte-append op per quoted field.
19. **First-element-then-rest slice loop.** Slice marshal emits the
    first element directly (no leading comma) and iterates `slice[1:]`
    with a comma-prepend, lifting the per-iteration `if i > 0` branch
    out of every step.
20. **`bytes.IndexByte` string scan.** `scan.String` and
    `(*Stream).String` use `bytes.IndexByte` (SIMD-accelerated by Go's
    runtime) to locate the closing `"`, then a second IndexByte over
    the span detects any preceding `\`. Wins big on long strings
    (URLs, base64 blobs); break-even on short ones. Truncated `\u…`
    or trailing `\` still surfaces as `ErrBadString` via fallthrough
    to `stringSlow`.
21. **Empty-container peek bypass.** Slice and map decode peek for
    `]`/`}` before allocating — empty `[]` / `{}` keep the field nil
    and skip `make`. Saves an alloc per empty container in the
    payload (common for nullable maps and tag-list fields).
22. **Adjacent-constant-append coalescing.** Post-render peephole over
    `renderAppendJSON`'s body merges adjacent `dst = append(dst, ...)`
    lines whose arguments are all compile-time byte literals (string
    spreads, char literals) into one append. Picks up `,"key":` + `[`
    → `,"key":[`, `]` + `,"next":` → `],"next":`, and the trailing
    `]` + return `'}'` into a single `return append(dst, "]}"...), nil`.
    Single-byte payloads emit as `'X'`, multi-byte as `"…"...`. ~5%
    on Marshal of struct-heavy payloads.
23. **nil slice/map → JSON `null`** (and accepted on decode). Matches
    stdlib `encoding/json` v1/v2 wire shape: nil container serializes
    as `null`, empty non-nil as `[]` / `{}`. Decode accepts `null` and
    leaves the field nil. Fixed-length arrays don't accept `null`
    (no nil array values in Go). Cost: one `if x == nil` branch on
    encode and one 4-byte `null` peek on decode per slice/map field.
    Costs ~4% on Marshal but is required for stdlib parity.
24. **Slab-allocated `[]*T` / `[N]*T` decode.** For slices of
    pointer-to-struct, allocate one backing slab `make([]T, 0, cap)`
    and append element pointers as `&_slab[len-1]`. For pointer-arrays,
    `make([]T, N)` — heap-allocated, exact-sized. A stack `[N]T` would
    still escape (the per-element `&_slab[i]` forces it), so the heap
    slice skips the stack hop and avoids cache-line thrash for large
    T. Turns N per-element heap allocs into ~log(N) (slice) or 1
    (array). When the slice slab grows past cap, prior `*T` pointers
    keep the orphan backing alive — semantically correct, uses ~2×
    slab memory in the worst case but avoids the per-element alloc
    storm. Null elements skip the slab entirely (nil pointer).
25. **`preallocCap` returns `(slice, slab int)`** — single switch over
    `f.ElemKind` decides BOTH the slice cap (`make([]E, 0, slice)`)
    and the slab cap (`make([]T, 0, slab)` for `[]*T`). Defaults
    when no explicit hintlen/len/minlen:
      - `[]*T` (pointer-elem struct/array/slice/map): both default to
        `defaultPreallocCap`. Slice slot is 8 B, slab slot is sizeof(T)
        — slab default avoids the orphan-trail growth chain.
      - `[][]T` (slice of slice) / `[]map[K]V` (slice of map): slice
        cap defaults to `defaultPreallocCap`, slab=0. Element slot is
        bounded (24 B slice header or 8 B map handle); over-cap waste
        is small, no slab needed.
      - `[]T` (struct value-stored) / `[][N]T` (slice of fixed array):
        both 0. Element size could be anything — prealloc × element-
        size would explode retained heap on big structs.
      - primitive (`[]int`, `[]string`, etc): slice = `defaultPreallocCap`
        clamped by maxlen if smaller; slab=0.
    Empty `[]` always emits `result.X = []T{}` (stdlib parity — `null`
    → nil, `[]` → empty non-nil); slice/slab prealloc only in the
    non-empty arm.
26. **Stream key dispatch via `Stream.KeyView`.** Object-field switch
    keys are read once, matched against constant strings, then
    discarded. The old codegen used `_s.String(i)` which allocates a
    fresh heap string for each key — wasted ~200 throwaway allocs per
    decoded value, inflating mheap span retention. `KeyView` is a
    sibling method that aliases via `unsafe.String(unsafe.SliceData
    (s.buf[start:]), end-start)` on the happy path (no escapes); the
    alias stays valid even if buf grows because GC pins the old
    backing. Falls back to `stringSlow` for escape sequences. Key
    strings never escape the dispatch frame, so safety holds.
27. **`peelSliceField` initializes `HintLen=-1`.** Nested slice
    recursion (`[][]T`, `[N][]T`) builds an inner `FieldInfo` that
    used to inherit Go's zero-value `HintLen=0`, which `preallocCap`
    reads as "user opt-out, no prealloc". So every nested row started
    at cap=0 and walked the 1 → 2 → 4 → 8 growth chain. Fix: peel
    sets `HintLen=-1` ("unset") so the inner level falls through to
    kind-based defaults. Single biggest alloc cut in the residency
    investigation — Matrix `[][]int` inner rows dropped 494k → 274k
    allocs/1000 iters.
28. **Bitmask seen-flag tracking for wide structs.** Per-field `bool`
    locals fit in registers for narrow structs (≤32 fields, default
    threshold). Above that, `var _seen uint64` (or `[N]uint64` for >64
    fields) replaces the bool fan-out — same set/check ops (1-2 cycles
    each) but cuts the stack frame from N bytes to 8/N⌈/64⌉. Real wins
    only show up on wide + recursive structs where cumulative stack
    pressure and cache locality dominate; below threshold, bools stay.
29. **In-place decode for every elem kind.** Slice/array elem decode
    writes directly into the final destination slot, regardless of
    kind:
      - `[N]*T` (slab+array):  `_slab[ivar]`
      - `[N]T`  (dst+array):   `dst[ivar]`
      - `[]*T`  (slab+slice):  pre-grow `append(_slab, zero(T))`,
        target `_slab[len-1]`
      - `[]T`   (dst+slice):   pre-grow `append(dst, zero(T))`,
        target `dst[len-1]`
    For structs the slot serves as both receiver-source (value-
    receivers ignore content) and write target on return:
    `slot, k, err = slot.DecodeFrom(...)`. For primitive scans
    (`scan.Bool`, `_s.Int64`, inline int/string scanners) the slot
    is the assignment target: `slot = _bv` / `slot = int(_n)`. No
    `var ev0`, no `var _z`, no `var _sv` — saves one struct/primitive
    copy per element AND drops the post-decode `dst[ivar] = ev0` (or
    `append(dst, ev0)`) line. `inlineScanInt64`/`inlineScanUint64`
    receive `target` + `castFn` directly, so the cast happens at the
    final assignment site. Pre-grow uses `zeroLit(elemType, kind)`
    (`""`, `false`, `0`, or `T{}`) since composite literal `int{}`
    is illegal but `append([]int, 0)` works via untyped const.
    Mods/validation reference the target string, emitting e.g.
    `if len(dst[len(dst)-1]) > 16 { ... }` — same semantics, slightly
    longer expressions that the Go compiler folds.
30. **Position-var pass-through; no `kN := posVar` alias.** Slice and
    array decoders thread the caller's position variable directly
    (top-level `j`, or the parent's `k` in nested recursions) — no
    `kN := posVar` aliasing. Each inner slice/array advances the
    SAME counter; outer continues with that advanced position when
    the inner returns. Only data locals (`evN`, `_idxN`, `_slabN`)
    keep depth suffixes. Drops one assignment per array entry plus
    the trailing `posVar = kN + 1` sync.
31. **Inline `null` peek; no `_np`/`_ok` locals.** The 4-byte `null`
    literal check is emitted byte-by-byte inline at the call site
    (`if j+4 <= len(data) && data[j]=='n' && data[j+1]=='u' && ...
    { j += 4 }`). Saves the `_np`, `_ok` locals from
    `scan.Null(data, j)` and the post-call `j = _np` sync. Matches
    the same inline shape already used for pointer-to-struct fields
    (`*T parent` etc.). Exposed via the `inlineNullPeek(posVar)`
    helper in `generate.go`.
32. **Single position cursor in dispatch loop.** Object-field
    dispatch no longer maintains a separate `j := i` cursor — every
    step (key scan, colon, value decode, comma/`}` handling)
    advances `i` directly. Removes the `j` local plus the end-of-
    iteration `i = j` sync. Stream path mirrors: `_s.KeyView(i)`
    returns into `i` via `=` (caller pre-declares `var key string`).
33. **Single local in `inlineScanString` (`_ke` only).** The
    inline string scanner used two locals (`_ks` start, `_ke`
    cursor). Now only `_ke` is kept — the start is `posIn+1` inline,
    and the slow-path fallback (`scan.String(data, posIn)`) still
    reads from the unchanged `posIn`, so no separate "save the
    original" var is needed. Slice expression becomes
    `data[posIn+1:]` with length `_ke - posIn - 1`; equivalent to
    `data[_ks:]` with `_ke - _ks`. Compiler folds the arithmetic.

## Benchmarks (~5.6 MiB deep Node tree, full validation)

AMD Ryzen AI MAX+ 395 (mitigations off), Go 1.26, GOEXPERIMENT=jsonv2.
Node carries scalars, slices, string-keyed maps, fixed-length tuples,
slices of pointers (slab path), nested slices, pointer fields, time,
bytes (base64), `any`, and `json.RawMessage` — the full breadth of
real-world API response shapes.

**Unmarshal:**

| path     | ns/op       | B/op    | allocs     | MB/s    |
| -------- | ----------- | ------- | ---------- | ------- |
| jsonv2   | 29115 K     | 16990 K | 245929     | 201     |
| sonic    | 26008 K     | 22871 K | 245873     | 226     |
| easyjson | 21064 K     | 16983 K | 245859     | 278     |
| **ggen** | **11577 K** | 26268 K | **65716**  | **507** |

**Marshal:**

| path     | ns/op       | B/op       | allocs    | MB/s    |
| -------- | ----------- | ---------- | --------- | ------- |
| jsonv2   | 21385 K     | 27001 K    | 7667      | 274     |
| sonic    | 14749 K     | 12097 K    | 7608      | 398     |
| easyjson | 9143 K      | 6140 K     | 7590      | 642     |
| **ggen** | **7820 K**  | 11922 K    | **2185**  | **750** |

**Reader input (streaming):**

| path                         | ns/op    | B/op    | allocs  |
| ---------------------------- | -------- | ------- | ------- |
| jsonv2.UnmarshalRead         | 54 ms    | 33.8 MB | 246 K   |
| easyjson.UnmarshalFromReader | 37.3 ms  | 31.5 MB | 246 K   |
| **ggen UnmarshalStream**     | 34.5 ms  | 19.3 MB | 384 K   |
| **ggen ReadAllUnmarshal**    | 25.3 ms  | 29.9 MB | 153 K   |

ggen Stream copies strings during parse (each scanned string is its
own heap alloc — that's the 230K extra allocs over the bytes path),
which is why it loses ground on alloc count. The win returns on
**Marshal** (still 1.18× faster than easyjson) and the **bytes-only
path** (1.7× faster than easyjson). The cleanest "I have an
io.Reader" pattern is `ReadAllUnmarshal` — only 1.4 ms slower than
direct bytes decode and the same alloc count.

**Where streaming actually pays off:** fail-fast on validation
errors. `BenchmarkSlowStream_Invalid/ggen_stream` rejects a malformed
payload after reading just enough bytes to decode the bad field
(~67 ms), vs `BenchmarkSlowStream_Invalid/ggen_readall` which has
to consume the whole body first (~78 ms). That ~11 ms gap is real;
on bigger payloads or slower readers it grows linearly.

**Residency (retained heap per decoded item, 1000 alive):**

| codec           | per-item | factor over JSON payload |
| --------------- | -------- | ------------------------ |
| **ggen bytes**  | 66.2 KiB | 1.89× (lowest)           |
| easyjson        | 74.7 KiB | 2.13×                    |
| stdjson         | 80.1 KiB | 2.28×                    |
| **ggen stream** | 90.3 KiB | 2.58× (highest)          |

The single biggest residency win was **dropping `maxlen=N` as a
prealloc hint** — it cut bytes-path retention from 163 → 65 KiB/item.
See "tried and rejected" for the full thread (arena codegen,
inline scratch buf, alias-mode + pool reuse) — none of those moved
the residency needle, only the maxlen change did.

On the tiny complex payload (~440 bytes): Unmarshal ~415 ns, 2 allocs,
~1 GB/s — still the fastest.

`B/op` notes:

- **Marshal:** ggen overshoots easyjson by ~58% because `JSONSize()` is
  an upper bound: per map entry costs `4 + 2*len(k) + value-bound` (kind-
  derived), or a flat 128-byte fallback for nested/struct values. Down
  from a flat `128 * len` (~2.4× overshoot pre-tighten).
- **Unmarshal:** ggen reports higher B/op than easyjson (6.1 MB vs 3.3 MB
  for ~970 KB input) because `unsafe.String` aliases keep the entire
  input buffer alive — the GC accounts the input as a live allocation
  per iteration. Allocs are still ~3.4× lower than easyjson (18 K vs 61 K).

## Design decisions (the why)

1. **`unsafe.String` aliases are safe across buffer growth.** Go's GC is
   non-moving (mark-and-sweep, no compaction). An alias into the OLD
   backing array keeps that array live — GC walks string headers and marks
   the pointed-to memory. Stream can `append`-grow freely; aliases from
   previous values remain valid.
2. **Struct fields are sorted alphabetically at codegen time** (default).
   Zero runtime cost. Deterministic wire output, compresses better.
3. **No runtime reflection anywhere.** Even the cross-package struct fallback
   uses `encoding/json.Unmarshal` (which reflects) only for types NOT in
   the generation pass.
4. **Stream pool only pools the `*Stream` wrapper**, not its buffer. The
   buffer is fresh-allocated per `Init` and retained by aliases after return.
5. **Benchmarks live in a separate module (`bench/` with its own
   `go.mod`).** Two reasons. (a) easyjson's codegen bootstrap compiles
   a non-test build that can't see types in `_test.go` files, so the
   bench module has `types.go` with the annotated `Node`. (b) Keeping
   sonic / easyjson and their large transitive dep set (bytedance/gopkg,
   cloudwego/base64x, klauspost/cpuid, golang/asm, …) in a separate
   module means `go get github.com/sirkostya009/ggen` pulls only the
   minimal runtime deps (uuid + `golang.org/x/tools`). The bench module
   uses `replace github.com/sirkostya009/ggen => ../` for local
   development. Small duplication of the `Node` type across modules;
   acceptable.
6. **Custom validators / mods are codegen-time function injection.** Tags
   like `ggen:"@EvenOnly"` and `mod:"@Squash"` are resolved via
   `packages.Load` at parse time — the generator looks up the named
   function, validates its signature against the field's exact go/types
   type, and emits a direct call. No runtime registry, no `func(any) any`
   boxing, zero allocs from the rule itself. Cross-package via
   `@pkg.FuncName` resolves through the source file's import block;
   blank imports (`_ "path/to/lib"`) work for libs the user pulls in
   solely for ggen's benefit. Validator errors wrap as
   `validation.CustomError{Name: "@FuncName", Cause: err}`; fallible mod
   errors (`func(T) (T, error)`) propagate as parse errors (same level
   as `scan.ErrBadX`), not validation.

## Test files

- `shared_test.go` — shared annotated structs (Address, Node, …) used
  across the feature tests.
- `schema_ggen_test.go` — generated methods for the test structs. Test-only.
- `read_test.go` — basic Read tests + unknown-key error & ignoreunknown opt-in.
- `scan_decode_test.go` — bytes-path + stream-path correctness (including
  chunked-reader + tiny-hint-forces-grow).
- `payloads_test.go` — `complexPayload` + `complexValue` (used by
  roundtrip / stdcompat tests) and `megaPayload` / `megaValue`
  (1 MiB generated Node tree, fixed seed 1; used by `stdcompat_test.go`
  to exercise cross-compat at scale). No benchmark functions live in
  the root module — all benchmarks moved to `bench/`.
- `stdcompat_test.go` — exhaustive cross-compat: for every annotated
  struct, ggen-marshal → jsonv2-unmarshal AND jsonv2-marshal → ggen-unmarshal;
  results re-marshaled via jsonv2 and compared as parsed `any` (map order
  and nil/empty-slice noise normalized).
- `htmlescape_test.go` — verifies literal default (jsonv2-shaped) + `htmlescape` opt-in (v1-shaped).
- `fuzz_test.go` — three fuzzers over `Node`: panic safety, roundtrip
  fixed-point, jsonv2-compat.
- `roundtrip_test.go`, `custom_test.go`, `dive_test.go`, `extra_test.go`,
  `fallback_test.go`, `hooks_test.go`, `inline_test.go`, `maps_test.go`,
  `mods_test.go`, `native_test.go`, `omit_test.go`, `pointer_test.go`,
  `decode_dups_test.go`, `richtypes_test.go`, `wire_test.go` — feature
  coverage.
- `cli_test.go` — CLI integration: builds the ggen binary in TestMain,
  exercises file-naming contract (single-file/dir, test/non-test, `-o`
  override), `./...` walk + dot/underscore-dir skip, and per-flag
  effects on generated output (`-marshal`, `-unmarshal`, `-pkg`,
  `-novalidate`, `-ignoreunknown`, `-htmlescape`, name filter).
- `bench/mega_test.go` — 4-way mega benchmarks (jsonv2 / sonic /
  easyjson / ggen) collapsed into three table-driven benches:
  `BenchmarkMega_Unmarshal`, `BenchmarkMega_Marshal`, and
  `BenchmarkMega_Reader` (the Reader bench includes
  `ggen_ReadAllUnmarshal` — `io.ReadAll` then bytes-path decode, the
  cheapest "I have an io.Reader" pattern). Inner loop runs under
  `b.RunParallel`, so `-cpu=1` runs serial and `-cpu=N` runs N-way
  parallel — same code path. Stateful codecs (Reader, Stream buf)
  get per-goroutine state via a `setup` closure in the `runBench`
  helper. Each sub-bench wraps `runtime.ReadMemStats` and reports
  `heap_KB` (live heap at StopTimer), `total_KB` (alloc delta over
  the timed region), `gc` (NumGC delta), and `gc/op` (per-iter GC
  rate) on top of the standard `ns/op` + `B/op` + `allocs/op`.
- `bench/slowstream_test.go` — slow-reader benchmarks (`slowReader` with
  geometric-decay delays). Two table-driven benches:
  `BenchmarkSlowStream_Valid` (stdjson, easyjson, ggen_stream,
  ggen_readall on a valid payload) and `BenchmarkSlowStream_Invalid`
  (ggen_stream, ggen_readall, jsonv2-baseline on a payload that fails
  ggen validation early). Same runBench harness as mega, so `-cpu=N`
  scales near-linearly (concurrent slow connections overlap their
  sleeps — useful for "N slow clients hitting one parser" sims).
  The Invalid group is where streaming pays off: fail-fast bails as
  soon as the bad field is seen, ReadAll has to drain the body first.
- `BenchmarkRetention` in `bench/mega_test.go` — folded the old
  `TestResidency` into a parallel-safe bench. Each goroutine holds
  its produced `*Node` values in a local sink; sinks merge after
  `b.RunParallel`; GC × 2; snapshot `runtime.MemStats.HeapInuse`
  delta divided by `b.N` gives `retain_KB/op`. `HeapInuse` is
  process-global so the technique works in parallel. Best run with
  a fixed iter count (`-benchtime=1000x`) for comparable per-codec
  numbers.

Running tests: `GOEXPERIMENT=jsonv2 go test ./...` for the root module;
`(cd bench && GOEXPERIMENT=jsonv2 go test ./...)` for benchmarks
(separate module, not reached by root's `./...`).

## Adding new tests

Before writing a new test, do this in order:

1. **Audit existing tests.** `grep` the codebase for similar
   assertions, struct annotations, or feature names. The list under
   "Test files" above is your first stop. Common cases — Address,
   Node, slice/map shapes — are covered; the test you're imagining
   may already exist in spirit.
2. **Extend, don't duplicate.** When a related test already exists,
   PREFER modifying it to cover the new case — even when that
   means refactoring the existing test into a table-driven loop.
   Patterns to look for:
    - Single assertion that could become a `cases := []struct{…}{}`
      slice with one entry per scenario.
    - Inline subtest body that could be lifted into a helper called
      from a `for _, c := range cases { t.Run(c.name, …) }` loop.
    - Multiple `t.Run("X", …)` blocks with copy-pasted bodies —
      candidates for the same loop treatment.
   The applicability tests in `cli_test.go` (the big
   `InvalidRuleApplication` table) are the reference shape:
   one slice of `{name, input, wantSubstring}` triples, one
   `t.Run` per row. ~80 cases sit cleanly under one parent.
3. **Avoid creating new helpers** unless the same setup recurs
   ≥3 times. `runCLI`, `writeFixture`, `captured`, `mustHaveFile`,
   `writeGoFile` already exist; check the matching `_test.go` file
   for in-package helpers before writing your own.
4. **Pick the host file.** Each `*_test.go` corresponds to a feature
   area (dive, mods, inline, native, pointer, etc.). New tests
   belong in the file whose existing tests share the most context
   with the new case. Don't fragment.
5. **Only create a new `*_test.go` file** when the new feature has
   no existing home (rare). New files duplicate setup boilerplate
   and create grep dilution — avoid unless the test surface is
   genuinely new.
6. **Pick the host struct.** Look at `shared_test.go` and the
   annotated structs at the top of each feature test file.
   `Address`, `Node`, `WideStruct`, `Multi`, `Bad` etc. cover most
   feature combinations. Re-use the existing struct when its
   shape lets you exercise the new case. Can also extend existing
   structs with new fields if they fall under the same test category
   but don't have coverage yet.
7. **Only add a new struct** when no existing one carries the
   right field kind / tag combination. Annotated test structs go
   in the same file as the test that uses them; add to
   `shared_test.go` only when two or more files need it.

When the right approach is unclear, default to: adding new top level
test functions. After that considering merging newly added tests into
existing once the testing abstraction is vividly clear.

## How to regenerate

Build the binary into the project directory (`./ggen`), never `/tmp` or
other scratch paths. The binary is git-ignored; in-tree builds keep the
binary discoverable, prevent cross-session collisions when multiple
agents share the host, and match the path the test harness expects.

```sh
GOEXPERIMENT=jsonv2 go build -o ggen .
./ggen .              # regen schema_ggen_test.go
./ggen ./bench        # regen bench/bench_ggen.go (the binary still walks
                      # the bench dir even though it is a separate module —
                      # ggen reads source files, not module boundaries)
# easyjson for bench:
easyjson bench/types.go
```

`go install github.com/sirkostya009/ggen@latest` gives users the CLI binary.
The subpackages (`decode`, `decode/validation`, `encode`, `scan`) are importable by
their generated code.

## Backlog (ideas worth pursuing, not yet scheduled)

- **Decode-into-receiver mode** — switch `DecodeFrom` from its current
  "return a fresh `T`" shape to "merge JSON into the receiver and
  return it" (or expose a sibling `DecodeInto(dst *T, data, i)` method).
  Today the generated body declares `var result T` and ignores the
  receiver — so the slice/map field handlers can assume `result.X` is
  always nil and emit `result.X = make(...)` unconditionally. Brief
  "reuse the caller's backing" branches (`if dst != nil { dst =
  dst[:0] }` for slices, `clear(dst)` for maps) shipped briefly and
  were ripped out as dead code on 2026-05-13, see the
  `c90794b` → `<follow-up>` commit pair; the test that supposedly
  exercised them (`TestStdlibVsGgen_MapReplaceDivergence`) was bogus
  — both `decode.Unmarshal[Node]` and `Node.DecodeFrom` always build
  fresh, so the reuse branches were unreachable from any user
  codepath. If/when a decode-into-receiver mode lands, those reuse
  branches become live and the divergence test becomes meaningful.
  Surface options when revisiting:
    1. `result := receiver` at top of DecodeFrom — receiver state
       flows in, fields decoded by JSON overwrite it. Stdlib-merge
       semantics. Breaking for existing `var zero T; zero.DecodeFrom`
       callers because zero-value receivers no longer guarantee fresh
       output.
    2. New `DecodeInto(dst *T, data []byte, i int) (int, error)` —
       coexists with `DecodeFrom`. Codegen forks the field handlers
       to drive `dst.X` instead of `result.X` and the reuse branches
       come back. Heavier generator change.
    3. Opt-in `//ggen:generate decodeinto` annotation — emits the
       sibling method only for structs that want it. Lowest blast
       radius.
  Pick when there's a concrete consumer asking for the merge shape.

- **Refactor generator to emit `go/ast` nodes instead of text** — today
  every render function writes Go source as a `[]byte` via
  `fmt.Fprintf` / `WriteString` into a buffer, then `format.Source`
  parses that text and re-emits it. If the generator built
  `*ast.FuncDecl` / `ast.BlockStmt` directly, the format step could
  drop the parse half (~30% of `format.Source` cost — see profiling in
  the May session) and call `printer.Fprint` directly. Bigger payoff:
  no more careful whitespace bookkeeping in render code (no `\n`
  threading, no trailing-blank-line cosmetics), and Go's compiler
  catches malformed expressions at codegen time instead of at the
  format step. Costs: full rewrite of every `render*` helper (currently
  ~5k LOC of text-emitting code), every `fmt.Sprintf` template becomes
  an AST builder, and the buffer-drilling work just done becomes moot.
  Worth doing only if (a) the text-emit codebase becomes hard to
  maintain, or (b) profiling shows `format.Source`'s parse phase as a
  bottleneck on real workloads.

## Tried and rejected (don't re-attempt without new evidence)

- **Pointer-arithmetic decoder / `unsafe.Add` byte loads** to eliminate
  bounds checks. Conversion of all four hot inliners (SkipWS,
  ScanInt64, ScanUint64, ScanString) plus all spot accesses brought
  bounds checks in `bench_ggen.go` from 59 → 18 (byte path: 0). Result:
  Unmarshal **regressed by ~10%** normalized vs reference libs. Why:
  modern AMD64 branch prediction makes never-taken bounds checks
  effectively free (~1 cycle, predicted), while `unsafe.Add` defeats
  Go's compound addressing-mode codegen — `data[i]` compiles to one
  `MOV (base)(idx*1)` instruction; the unsafe form takes 2–3
  instructions because the optimizer treats the unsafe load as
  opaque. Lost addressing mode + lost loop-invariant hoisting >
  bounds-check savings on this CPU. Verified across 5-run benches.
  Don't retry unless targeting a CPU where bounds-check branches mispredict.

- **Removing all decode-side inliners** (inlineSkipWS / inlineScanInt64
  / inlineScanUint64 / inlineScanString) and replacing them with plain
  `scan.X(...)` function calls. A `//go:noinline` micro-bench had shown
  per-call overhead at ~0.4 ns (12.65 vs 12.89 ns/op for Int64 across
  20 runs of 5 s each — basically indistinguishable). Macro result:
  Unmarshal **regressed ~15–20%** normalized. The single-call
  micro-bench understated the cost because in macro context, inlining
  matters for register allocation across adjacent ops, ICache pressure
  from N small fns called repeatedly, and codegen-level optimizations
  (branch hoisting, compound BCE) the compiler can only do with the
  body in scope. Don't trust per-call micro-benches for hot-loop
  inlining decisions.

- **Stream-path `_s.SkipSpace` inliner** (`inlineStreamSkipWS`). Most-
  hit method on the stream path (5+ per field). Inlining the body
  inline at every call site saved the method-dispatch frame but kept
  the `_s.Ensure(j+1)` call inside the loop. Raw bench showed +7%
  improvement; normalized via EasyJSON Reader as machine-state proxy,
  real gain was **~2% — within noise**. The `Ensure` cold path
  dominates Stream throughput, so eliminating method dispatch on
  SkipSpace specifically doesn't move the needle. Don't retry without
  also tackling Ensure overhead.

- **Inlining `scan.Bool` / `scan.Float64`** (was item #2 in backlog).
  `//go:noinline` micro-bench against an inlined-body equivalent showed
  the function-call frame is fully amortized by the body work for
  primitives at this size — measured difference was 0.24 ns of 12.6 ns
  total, with the call version slightly *faster* on average. No real
  win available. Same lesson as item #2 above: don't chase per-call
  overhead in isolation.

- **`Ensure(p *int, n int) error` + `Anchor(p)` / `Unanchor()` for
  bounded streaming.** The original streaming primitive bulk-fetched N
  bytes at the call site by looping `Read` internally until satisfied,
  and supported a window-shift mode that let `Ensure` slide the buffer
  forward (dropping the prefix) under bounded-buffer mode; `Anchor` /
  `Unanchor` froze that shift across `SkipValue` so `RawJSON` /
  `json.Unmarshal` fallback could slice `_s.Bytes()[_start:_k]` after
  the fact. Two complaints killed it. (1) "Read in a for loop" inside
  `Ensure` is the antithesis of lazy streaming — if you're going to
  loop on `Read`, `io.ReadAll` is simpler and roughly the same. (2)
  The anchor mechanism plus `*int` cursor adjustment across shifts
  was a constant source of stale-position bugs (e.g. the Float64 /
  Number paths needed `&start` not `&i`, or the buffer would silently
  drop the digit prefix mid-parse). Replaced with `ReadMore(keep int)
  error` — single Read per call, optionally compacts in-place when
  the caller passes `keep > 0` — and **byte-by-byte multi-byte
  literal scans** at the parser level. The simpler `ReadMore()` /
  never-shift shape shipped briefly before this; the `keep` parameter
  came back specifically to bound buffer growth on long streams
  without resurrecting Ensure's bulk-fetch loop. Internal Stream
  methods still pass `keep=0` (grow-only) so caller cursors stay
  valid; only the top-level dispatch-loop bounds checks compact.
  Fail-fast is preserved (~67 ms vs ~78 ms ReadAll on invalid
  payload). Don't reintroduce a bulk-fetch primitive without a
  fail-fast story that doesn't regress lazy semantics.

- **Stream `Acquire`/`Release` pool with reused buffer.** Originally
  `scan.Acquire(r, hint)` returned a pooled `*Stream` and `Release`
  truncated `s.buf` to retain it for the next `Acquire`. Combined with
  alias-mode strings (`unsafe.String` into `s.buf`) this looks like a
  win in a microbench but is **silent corruption**: the next `Acquire`
  reuses the same buffer, the `Read` call overwrites bytes, and prior
  decoded values' aliased string fields still point at those locations
  — so e.g. `n1.Name` flips from `"FOO_TINY"` to `"BAR_VERY"` after a
  second decode. Caught by writing a two-payload correctness probe;
  the residency benchmark didn't catch it because content collisions
  happened to match. Replaced with stack-allocated Stream + caller-
  owned buf and copy-mode strings.

- **`[512]byte` inline scratch in `Stream`.** Idea: avoid the buffer
  heap alloc for small payloads by embedding a stack-resident scratch
  array in `Stream`, spilling to heap only when payload > 512. Doesn't
  work because Go's escape analysis can't prove `&s` is safe across the
  `zero.DecodeStreamFrom(&s, 0)` call inside `UnmarshalStream` — the
  generic function dispatch defeats it, so the entire `Stream` (now
  including the 512-byte array) gets heap-allocated. Net result: same
  alloc count, larger Stream object. The dream of stack-resident Stream
  needs a non-generic API (caller writes `var s scan.Stream` themselves
  and calls `Node{}.DecodeStreamFrom(&s, 0)` directly) — the convenient
  `decode.UnmarshalStream[T]` wrapper precludes it. Don't retry without
  also redesigning the user-facing API.

- **Per-decode arena + `StreamArenaSize`/`StreamArenaCompact` codegen.**
  Flow: parse with aliased strings (zero per-string alloc, fast),
  walk the decoded value to sum string bytes, allocate one arena
  `[]byte` of exactly that size, copy each string into the arena and
  rewrite headers via `unsafe.String`. Fully implemented across struct
  / slice / map / `any` / aliased-primitive fields. Result on Mega:
  alloc count dropped 347K → 128K (matches bytes path); B/op dropped
  24.6 → 19.7 MB; **residency unchanged at ~86 KiB/item**. Wall clock
  also unchanged — 2-walk overhead canceled out the per-string-copy
  savings. The residency stayed put because the gap was never
  per-string heap fragmentation; it was the per-decode buffer
  retention + map rebuild allocs (Go has no in-place key-rewrite
  primitive). Removed the codegen and `decode/arena.go` — added a lot
  of complexity for no measurable gain. If retries: prove the gain
  on residency BEFORE shipping the codegen, not after.

- **`maxlen=N` as a slice/map prealloc hint.** Original codegen used
  `maxlen=64` to emit `make([]T, 0, 64)` so Mega's typical 5-26 element
  slices avoided the growth chain. Hidden cost: every retained value
  carried the over-allocated slice/map cap forever. Killing this
  hint (residency villain) cut per-item retention from 163 KiB →
  ~67 KiB on the bytes path — biggest single residency win in the
  whole exploration. Now only `len`/`minlen`/`hintlen` drive prealloc.
  Don't reintroduce `maxlen` as a sizing hint without an opt-in
  mechanism (see `hintlen` for the explicit-hint pattern).

## Maybe-someday (only if a real need shows up)

- **Hybrid key-dispatch strategy at codegen** — current length-first
  switch + if-chain wins for narrow structs (~3–5 cycles per dispatch
  on typical 2-3 candidates per length group). For wide structs where
  length groups balloon (>5 candidates), emit `switch key` so Go's
  compiler auto-hashes (≥7 cases triggers hash dispatch). Picking
  per-struct or per-length-group at codegen could squeeze out a few %
  on wide structs without regressing narrow ones. Postponed until
  someone shows up with a 50+ field schema where it matters.

- **Validation-derived encode hints (Trusted-ASCII, schema-bound
  numbers, etc.).** Use `ggen` validation tags to inform encode-side
  shortcuts: `ascii` → skip escape table on marshal; `lte=N` → emit a
  hand-rolled fixed-width digit formatter instead of `strconv.AppendInt`;
  similar for `oneof`/`len`. Real wins on hot fields, but couples
  encode shape to decode-time validation semantics — the same struct
  field would marshal differently based on its `ggen:` tag, which
  blurs the marshal contract. (Decode-side preallocation already uses
  `len`/`maxlen`/`hintlen` — see optimization #2 — that's a one-way
  hint into a `make` cap, not a wire-shape change.) Shelved unless
  there's a target schema where the win is concrete.

- **Streaming `io.Reader` over marshalled output (state-machine codegen).**
  Idea: per-struct `AsReader()` returning a resumable state, plus an
  `encode.Reader[T](v)` driver that exposes it as `io.Reader` (or
  `io.ReadSeeker` for HTTP body replay). Suspends mid-marshal so peak
  memory = caller's `p []byte` instead of `JSONSize()`. Three granularity
  tiers considered (per-field, per-element, byte-level); per-field is
  cheapest at ~300 LOC of generator change but only matters when a single
  payload is too big to materialize. Real-world `JSONSize()` already fits
  comfortably in RAM for everything we care about — shelved unless
  someone shows up with multi-GB JSON request bodies. The trivial
  "bytes.NewReader over Marshal output" version is a one-liner the user
  can write themselves; no need to bake it in.

- **SIMD / AVX2 vectorization for hot scanning loops.** Sonic's
  decoder hits 235 MB/s on Mega Unmarshal vs ggen's 251 MB/s in part
  because bytedance hand-wrote AMD64 assembly that uses AVX2 for
  string-quote scanning, whitespace skipping, and number parsing.
  ggen currently does these byte-at-a-time in `scan/scan.go` and
  `scan/stream.go`. Candidates worth probing:
    - `bytes.IndexByte` (already SIMD-accelerated by Go runtime — we
      use it for the string-closing-quote scan; verify it's vectorizing
      on amd64).
    - inline AVX2-accelerated WS skip via `golang.org/x/sys/cpu` to
      detect support + Plan9 assembly stub.
    - number parsing: probably not worth — `strconv.ParseInt` /
      `strconv.ParseFloat` are already heavily tuned, and ggen's
      hand-rolled inline int scan beats the function call for the
      common case.
  Real shape of the win unclear: sonic's vector win is bundled with
  JIT and asm dispatch — pure SIMD-on-Go (no JIT) might claw back
  10-15% on string-heavy payloads at the cost of:
    - per-arch (amd64 / arm64) source duplication
    - `go vet`-incompatible asm files
    - Plan9 syntax maintenance burden
    - lost portability (ggen currently runs on any GOARCH)
  Try only if a target workload shows the byte-scan loop as the
  dominant cost in CPU profile, AND the codegen complexity is
  acceptable. Don't speculatively add asm files "to keep up with
  sonic"; the gap is small and ggen's portability is a feature.

Fuzz tests live in `fuzz_test.go` — three targets over `Node`:
`FuzzScanNoPanic` (panic safety on random bytes), `FuzzRoundtrip` (encode
→ decode is a fixed point after one round), `FuzzCompat` (when both ggen
and jsonv2 accept input, decoded values must agree via `sameWire`). Run a
target with `go test -run=^$ -fuzz=FuzzX -fuzztime=30s`. Known
accept/reject drifts the compat target deliberately ignores: top-level
`null`, trailing garbage, invalid UTF-8 inside strings.

## Working with this file

**ALWAYS** keep this file up-to-date after making changes:

- **Update benchmarks**
- **CLI/Annotation flags**
- **Behaviour**
- **Usage**
- And so on...

## README.md authoring rules

**NEVER spill technical / implementation details into README.md.**

README is the user-facing front door: what ggen is, what it does for
the user, how to use it, what the numbers mean. CLAUDE.md is where
implementation details to save times for agents lives.

Specifically — do NOT add to README:

- runtime / harness mechanism: `runtime.GC()` cycles, `HeapInuse`
  snapshots, `b.RunParallel`, `MemProfileRate`, sink merge patterns,
  goroutine-local state, `b.ResetTimer` placement, etc.
- internal codegen details: `unsafe.String` aliasing semantics,
  slab cap heuristics, `KeyView` vs `String`, `preallocCap`
  return shape, peelSliceField specifics, etc.
- pprof / profiler internals: stack-walking, sample-rate
  trade-offs, top-N attribution mechanics.

DO put in README:

- what each benchmark measures (one sentence)
- how to read each column / metric
- when the user would care about that number
- the bench output table + an interpretive paragraph if needed
- caveats that affect the user's choice (e.g. "strings alias the
  input, don't mutate after decode")

If you find yourself writing the word "internally", "implementation",
"under the hood", or naming a private function / runtime API in
README — stop. That paragraph belongs in CLAUDE.md or a code comment.
