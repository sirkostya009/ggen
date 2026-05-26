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
├── alias.go                                            ← alias-type code emitters (decode + AppendJSON)
├── applicability.go                                    ← parse-time rule/kind compatibility matrix
├── customfunc.go                                       ← @Func resolution for custom validators / mods
├── check.go                                            ← `-dry` / future-ggenvet parse-only entry points
├── log.go                                              ← cliLog: leveled logger with deferred flush
├── parse_test.go, tags_test.go                         ← CLI tests
├── shared_test.go (in integrationtests/)               ← shared demo structs (Address, Node, …) used across feature tests
├── bench_test.go                                       ← BenchmarkGenerate over a representative fixture (generator perf only)
├── decode/                                             ← runtime: Decoder interface + top-level generics
├── encode/                                             ← runtime: AppendString helpers + Marshal/Write/Slice generics
├── scan/                                               ← runtime: hand-rolled JSON scanner + streaming Stream type
├── decode/validation/                                  ← typed validation error structs (one per rule)
├── integrationtests/                                   ← separate Go module — every feature/roundtrip/compat/fuzz test
│   ├── thirdparty/                                     ← non-annotated external type — exercises encoding/json fallback
│   └── thirdparty2/                                    ← annotated external type — exercises static analyzer pickup of cross-pkg generated decoder
└── bench/                                              ← separate Go module — Mega/Small/SlowStream/Retention benchmarks
```

`bench/` and `integrationtests/` are each their own **separate Go
module** (each has its own `go.mod` with `replace github.com/sirkostya009/ggen => ../`).

`bench/` exists separately for two reasons:

1. easyjson's codegen bootstrap compiles a non-test build, which can't
   see types in `_test.go` files. The bench module has `types.go`
   (ggen- + easyjson-annotated Node) + both codegens side by side.
2. The reference codecs (`sonic`, `easyjson`) and their large
   transitive dep set (bytedance/gopkg, cloudwego/base64x,
   klauspost/cpuid, golang/asm, …) stay out of the root module's
   `go.mod`. End users `go get`ing `github.com/sirkostya009/ggen`
   pull only the minimal deps (uuid + `golang.org/x/tools`) and
   never see the benchmark world.

`integrationtests/` exists separately so the feature tests import the
root packages as an external consumer would — the test surface
exercises the public API at the same boundary users hit.

The root module's tests cover the CLI itself (parse/tag/applicability/
log) plus one generator bench (`BenchmarkGenerate`). All other benches
live in `bench/`. All feature/roundtrip/compat/fuzz tests live in
`integrationtests/`.

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
  (`buf []byte` grown via `append`). The cursor lives in the
  exported `Pos int` field — every scan primitive
  (`s.SkipSpace()`, `s.String()`, `s.KeyView()`, `s.Int64()`,
  `s.SkipValue()`, …) reads from `s.Pos` and writes it back before
  returning. Methods take no cursor argument and never return one;
  the position state is in the Stream itself. Generated code that
  needs to capture a raw span reads `s.Pos` directly
  (`start := s.Pos; s.SkipValue(); raw := s.Bytes()[start:s.Pos]`).
  The single I/O primitive is `ReadMore(keep int) error`: one Read
  call per invocation, never loops. `keep` is the lowest offset the
  caller still needs — bytes before it are eligible for discard:
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
  a non-zero `keep` (current local cursor, or the value-start `start`
  for spans that need to outlast the loop) so the buffer stays
  bounded at roughly `max(chunk_size, single_value_size)` even
  across long streams. Each method updates its own locals after the
  shift (`i = 0` for the entry cursor, or `j -= start; start = 0`
  for the string-body case), then writes the final position into
  `s.Pos` before return. The `Shift` field (defaults to true via
  `Reset`) gets flipped off around `SkipValue` inside RawJSON
  capture and `json.Unmarshal` fallback spans, where the generated
  code needs stable absolute offsets to slice
  `s.Bytes()[start:s.Pos]`; bookkeeping branches in SkipSpace/etc
  check `s.Shift` before resetting the cursor.
  Generated code adds two more shift points at the dispatch-loop
  boundary: `ReadMore(s.Pos); s.Pos = 0` after `ObjectOpen+SkipSpace`
  and after the per-iteration value decode + SkipSpace. Each
  known-key case opens with `s.ConsumeColon()` — the alias from
  `KeyView` is no longer needed past dispatch, so the shift it
  triggers is safe. `UnknownKeyError` and the inline-catch-all map
  key both detach the alias with `strings.Clone(key)` so subsequent
  compactions don't corrupt the stored value.
  Each `(*Stream).X()` method does its own bounds check
  (`if s.Pos >= len(s.buf) { ... ReadMore(s.Pos) ... }`) and proceeds
  once one new byte has landed. Multi-byte literals (`true`,
  `false`, `null`, `\uXXXX`) are scanned **byte-by-byte**: each char
  triggers an individual bounds check + maybe ReadMore, and a
  mismatch fails fast without fetching the rest. This is the
  lazy-streaming property — parse-what-you-have, fetch one chunk
  only when truly stuck. See "tried and rejected" for the older
  `Ensure(p *int, n int)` + `Anchor`/`Unanchor` design that
  bulk-fetched N bytes via an internal Read loop.
- `Stream` is **stack-allocatable**, no internal pool: `var s scan.Stream;
  s.Reset(r, buf)`. Caller owns `buf` lifecycle. `scan.NewStream(r, buf)`
  is the heap-allocating shorthand for callers who don't care about the
  one-time alloc and want a single-expression form. There used to be an
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
    DecodeFrom(data []byte) (T, int, error)             // returns bytes consumed
    DecodeStreamFrom(s *scan.Stream) (T, error)         // Stream owns cursor via s.Pos
}

// Array walkers — callers would otherwise have to reimplement the
// bracket / comma / element-dispatch loop.
func UnmarshalSlice[T Decoder[T]](data []byte) ([]T, error)
func ReadSlice[T Decoder[T]](r io.Reader) ([]T, error)               // io.ReadAll + UnmarshalSlice
func UnmarshalSliceStream[T Decoder[T]](r io.Reader, buf []byte) ([]T, []byte, error)
```

Single-value entry points were intentionally NOT added: the generated
method is callable directly with a zero-value receiver. For struct
types and slice/map/array aliases the receiver is the one-liner
composite literal `T{}`; primitive aliases need an explicit zero
(`AliasInt(0)`, `AliasString("")`, `AliasBool(false)`, …) or a
`var zero T`. Stream callers construct the Stream themselves:

```go
// bytes path
v, _, err := T{}.DecodeFrom(data)

// streaming path
var s scan.Stream
s.Reset(r, buf)              // buf nil OK
v, err := T{}.DecodeStreamFrom(&s)
// caller may recycle s.Bytes() afterwards
```

The previous one-line wrappers (`Unmarshal[T]`, `Read[T]`,
`UnmarshalStream[T]`, `UnmarshalStreamRequest`,
`UnmarshalStreamResponse`) were removed: they were direct passthroughs
to the generated method, and the package surface is honest about what
the user actually pays for. The array walkers stay because
reimplementing the bracket/comma loop everywhere is real toil.

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
ggen ./...                    process every package matched by the pattern (module-scoped, same as `go build`)
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

The load Mode intentionally omits `packages.NeedDeps`. Imported package
signatures come from compiled export data (gcexportdata) instead of
re-typechecking every transitive dep from source. Method-set lookups on
imported types still work — export data carries methods. Peak RSS on
`ggen ./...` drops from ~1.4 GB to ~50 MB for projects with heavy import
graphs (sonic / easyjson / jsonv2). When the export data is unavailable
(rare — fresh checkout with no `go build` ever), the AST-only fallback
emits `encoding/json` for the affected type. Soft degradation, not a
hard failure.

Pattern mode (`./...`, `./sub/...`, `...`) resolves the pattern via
`packages.Load` — same dispatch as `go build <pattern>` / `go test
<pattern>`. Module-scoped and workspace-aware; never crosses module
boundaries. A subdirectory carrying its own `go.mod` is a separate
module and is skipped under the parent's `./...`, same as `go build`;
multi-module repos invoke ggen once per module (or wire a `go.work`
+ per-module loop) the way they already do for `go build` / `go test`.
Test-only packages (no non-`_test.go` files) are skipped — the
discovery `Mode` doesn't set `Tests: true`, mirroring `go list`'s
default. Single-package mode (`ggen <dir>`) still picks them up.

Processing order is post-order over the matched import subgraph (deps
first within the matched set) so each parent's `parsePackage` reads
its already-generated child `_ggen.go` and routes cross-package field
types through direct `DecodeFrom` / `AppendJSON` calls instead of the
`encoding/json` fallback. Transitive deps outside the matched set are
left alone — they're someone else's run. Pattern-mode work runs
sequentially in topo order; the per-pkg parse cost dominates and the
generator's global lock already serializes the codegen phase, so the
old depth-keyed goroutine fanout is gone.

Dot- and underscore-prefixed dirs (`.git`, `_build`, …), `vendor/`,
`testdata/`, and `node_modules/` are skipped automatically — `go list`
already filters them out under any pattern. No custom skip rule lives
in ggen anymore.

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
- `-dry` — dry run: parse and validate every annotated struct, surface
  every error, emit no file. Routes through `check.go`'s `checkPackage`
  / `checkFile` (parse-only entry points designed to be reused by a
  future `ggenvet` static-analysis binary — no codegen, no internal
  buffer, no I/O beyond what the parser already does). Composes with
  verbosity: `ggen -dry -v ./...` prints `ok <path> (N structs)` per
  package on success. Rejects `-o` / `-pkg` (neither has meaning when
  nothing is written).

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
  homogeneous primitive slices (`[]int*`, `[]uint16/32/64`, `[]float*`,
  `[]bool`, `[]string`, `[]any`), homogeneous string-keyed primitive
  maps (`map[string]int*`, `map[string]uint*`, `map[string]float*`,
  `map[string]bool`, `map[string]string`, `map[string]any`),
  `json.RawMessage` (verbatim passthrough), `time.Time` (AppendText),
  and pointer-to-primitive (`*string`, `*bool`, `*int*`, `*uint*`,
  `*float*` — nil → `null`) before falling into reflection for
  slices/arrays/maps/pointers/structs (with json-tag parsing for
  struct walking), keeping nested ggen `Marshaler` / `TextAppender`
  types on the fast path with no `json.Marshal` cliff. Skips
  `[]uint8` from the slice fast path so `[]byte` routes through the
  reflect.Slice base64 emitter.
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
func (result T) DecodeFrom(data []byte) (T, int, error)          // returns bytes consumed; satisfies decode.Decoder[T]
func (result T) DecodeStreamFrom(s *scan.Stream) (T, error)      // Stream owns cursor via s.Pos

func (s T) JSONSize() int                                         // upper-bound for one-alloc Marshal
func (s T) AppendJSON(dst []byte) ([]byte, error)                 // core marshal — propagates nested errors
```

**Cursor convention.** The bytes-path `DecodeFrom` takes a slice
starting at the value's first byte and returns how many bytes were
consumed; the caller advances its own cursor (`i += n` after
reslicing as `data[i:]`). Generated nested-struct calls follow this
pattern internally. The stream-path `DecodeStreamFrom` doesn't take
or return a cursor at all — the cursor is `s.Pos`, owned by the
Stream and advanced in-place by every scan primitive
(`s.SkipSpace()`, `s.KeyView()`, `s.String()`, …). Generated code
that needs to capture a raw span snapshots `s.Pos` before and reads
it again after (`start := s.Pos; s.SkipValue(); raw := s.Bytes()[start:s.Pos]`).

**Decode-into-receiver semantics.** The receiver passed in IS the merge
source. Scalar fields persist across JSON omission (stdlib-merge
shape); container fields are reset at the top of DecodeFrom so the
decoder never appends over data carried in from the caller. Concretely:

- slices and `[]byte` fields: `if X != nil { X = X[:0] }` at entry.
  Backing array is reused — a 200-cap slice that gets a 50-element
  JSON array decodes without allocating a new array. The slice
  decoder itself only emits `make(...)` when `X == nil`.
- `map[K]V` fields: `if X != nil { clear(X) }` at entry. Map bucket
  array reused; only nil maps trigger `make(map, cap)`.
- nested struct fields: recursion is `result.X, _, _ = result.X.DecodeFrom(...)`.
  The value-receiver method takes the existing value as the merge
  source automatically — no special-case codegen.
- pointer fields (`*T`): currently always allocate a fresh pointee
  via `var v T; ... result.X = &v`. The receiver pointer is
  discarded. Decode-into-receiver merge on the pointee is future
  work — see the renderField pointer block.
- fixed-length arrays (`[N]T`): every slot gets overwritten or
  errors via strict-length check, so no entry reset is needed.

JSON `null` for a slice/map field sets `result.X = nil` (matches
stdlib v1/v2). JSON `[]` / `{}` on a non-nil receiver keeps the
`[:0]`'d / cleared container; on a nil receiver, allocates an empty
non-nil container (also stdlib parity).

Call the generated methods with a zero-value receiver for fresh
decode (`T{}.DecodeFrom(data)` for struct/slice/map/array;
`var zero T; zero.DecodeFrom(data)` for primitive aliases). To merge
into an existing value, call its `DecodeFrom` directly.

Runtime entry points (call these from user code):

```go
// bytes path — single value
T{}.DecodeFrom(data)                       // (T, int, error)

// stream path — single value
var s scan.Stream
s.Reset(r, buf)
T{}.DecodeStreamFrom(&s)                   // (T, error); recycle s.Bytes()

// array walkers (decode package — keep what reimplementation would cost)
decode.UnmarshalSlice[T](data)             // ([]T, error)
decode.ReadSlice[T](r)                     // ([]T, error)
decode.UnmarshalSliceStream[T](r, buf)     // ([]T, []byte, error)

// encode package (unchanged)
encode.Marshal(t)            encode.MarshalString(t)        encode.Write(w, t)
encode.MarshalSlice(items)   encode.MarshalSliceString(items) encode.WriteSlice(w, items)
encode.AppendSlice(dst, items)
```

Opt-in (via `//ggen:generate marshal` / `//ggen:generate unmarshal`):

```go
func (s T) MarshalJSON() ([]byte, error)                         // wraps encode.Marshal(s)
func (s *T) UnmarshalJSON(data []byte) error                     // inlines `var zero T; zero.DecodeFrom(data)`
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
   both write directly into the parent buffer; `renderValidationOn` takes
   a `posVar` parameter that emits the right return shape inline
   (`return result, err` at top level, `return result, i, err` mid-stream
   for slice/map element readers) — no post-processing pass.
9. **Pointer fields** emit a 4-byte `null` peek → nil branch, else stack-
   local `var v <PointeeType>` + recursive inner read + `&v`. Dispatch
   order in `renderField` is pointer-first → string-tag → kind switch:
   running the `,string` branch before the pointer block would emit the
   broken `result.X = *<PointeeType>(n)` (e.g. `*int(n)`); pointer-first
   recurses with `inner.GoType = PointeeType` and the string-tag branch
   runs against the pointee type, assigning to the stack-local instead.
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
    discarded. The old codegen used `_s.String()` which allocates a
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
    receivers ignore content) and write target on return: bytes path
    emits `var _n int; slot, _n, err = slot.DecodeFrom(data[k:]); k += _n`;
    stream path emits `slot, err = slot.DecodeStreamFrom(s)` since
    `s.Pos` advances internally. For primitive scans
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
    iteration `i = j` sync. Stream path mirrors via `s.Pos`: every
    primitive (`s.KeyView()`, `s.ConsumeColon()`, `s.SkipValue()`,
    …) advances the same field, no per-call cursor passed in.
33. **Single local in `inlineScanString` (`_ke` only).** The
    inline string scanner used two locals (`_ks` start, `_ke`
    cursor). Now only `_ke` is kept — the start is `posIn+1` inline,
    and the slow-path fallback (`scan.String(data, posIn)`) still
    reads from the unchanged `posIn`, so no separate "save the
    original" var is needed. Slice expression becomes
    `data[posIn+1:]` with length `_ke - posIn - 1`; equivalent to
    `data[_ks:]` with `_ke - _ks`. Compiler folds the arithmetic.
34. **Concrete-type fast paths in `AppendAny` for typed primitive
    slices/maps.** The type switch in `encode.AppendAny` previously
    only matched `[]any` / `[]string` / `map[string]any` /
    `map[string]string`; anything else (`[]int`, `[]float64`,
    `map[string]int64`, `map[string]bool`, …) fell through to the
    reflect.Slice / reflect.Map path. The reflect path is alloc-free
    on the *value* (appendReflectValue uses `rv.Int()` / `rv.Float()`
    directly), but `reflect.MapRange` boxes per entry — ~2 allocs/
    entry → 64 allocs on a 32-entry map. Added cases for every
    primitive map shape (`map[string]int8/16/32/64`, `uint*`,
    `float32/float64`, `bool`) and every typed slice shape (`[]int*`,
    `[]uint16/32/64`, `[]float*`, `[]bool`). Each case dispatches to a
    generic helper (`appendMapInt[V]`, `appendSliceFloat[V]`, …) so
    the body is one strconv call per entry, no reflect overhead.
    Skipped `[]uint8` — that's `[]byte`, which must stay on the
    base64 reflect.Slice path. Measured on 32-entry shapes,
    encode/appendany_test.go: `map[string]int` 4403 → 1579 ns/op
    (2.8×), 71 → 7 allocs; `map[string]bool` 3449 → 944 ns/op
    (3.7×), 71 → 7 allocs; `map[string]float64` 6459 → 3417 ns/op
    (1.9×), 72 → 8 allocs. Outpaces both stdjson v1 and jsonv2 on
    every map shape after the fix.
35. **`AppendAny` concrete cases for `json.RawMessage`, `time.Time`,
    and pointer-to-primitive.** All three would otherwise route via
    the `case json.Marshaler:` branch (RawMessage and time.Time both
    implement `MarshalJSON`; pointer-to-primitive falls into
    `reflect.Pointer` → recurse-on-Elem). The interface dispatch
    costs a `MarshalJSON() ([]byte, error)` heap alloc per call;
    `reflect.Pointer` adds a `rv.Elem().Interface()` box per
    dereference. Concrete cases pre-empt both:
      - `json.RawMessage`: nil/empty → `null`, otherwise `append(dst,
        x...)` (bytes are assumed valid JSON, pass through verbatim).
      - `time.Time`: opens `"`, calls `x.AppendText(dst)` (Go 1.24+
        TextAppender — same RFC3339Nano wire shape as MarshalJSON),
        closes `"`.
      - `*string` / `*bool` / `*int*` / `*uint*` / `*float*`: nil →
        `null`, otherwise inline-emit. Int/uint variants share a
        generic `appendPtrInt[V]` / `appendPtrUint[V]` helper.
    Cases sit before the `case Marshaler` block so they win the type
    switch over the interface dispatches. Measured wins (vs jsonv2,
    32-byte shapes): `json.RawMessage` 227 → 28 ns/op (8.1×, 2 → 1
    alloc); `time.Time` 181 → 117 ns/op (1.55×); `*int` 70 → 26
    ns/op (2.7×); `*bool` 83 → 19 ns/op (4.4×).

## Benchmarks (~5.6 MiB deep Node tree, full validation)

AMD Ryzen AI MAX+ 395 (mitigations off), Go 1.26, GOEXPERIMENT=jsonv2.
Bench harness uses `b.RunParallel`, default `-cpu=NumCPU` (32-thread
aggregate throughput); for single-thread numbers run with `-cpu=1`.
Node carries scalars, slices, string-keyed maps, fixed-length tuples,
slices of pointers (slab path), nested slices, pointer fields, time,
bytes (base64), `any`, and `json.RawMessage` — the full breadth of
real-world API response shapes.

**Unmarshal:**

| path       | ns/op       | B/op    | allocs     | MB/s     |
| ---------- | ----------- | ------- | ---------- | -------- |
| jsonv2     | 4042 K      | 17.7 MB | 316832     | 1451     |
| sonic      | 2038 K      | 20.8 MB | 137770     | 2878     |
| sonic_fast | 1971 K      | 20.8 MB | 137770     | 2976     |
| easyjson   | 3158 K      | 17.0 MB | 245856     | 1857     |
| **ggen**   | **2245 K**  | 14.4 MB | **101927** | **2612** |

**Marshal:**

| path              | ns/op      | B/op    | allocs   | MB/s      |
| ----------------- | ---------- | ------- | -------- | --------- |
| jsonv2            | 1286 K     | 6.7 MB  | 7409     | 4559      |
| sonic             | 989 K      | 33.6 MB | 5116     | 5927      |
| sonic_fast        | 952 K      | 33.6 MB | 5113     | 6161      |
| easyjson          | 962 K      | 6.2 MB  | 7597     | 6095      |
| **ggen**          | 655 K      | 11.8 MB | **2**    | 8951      |
| **ggen_presized** | **564 K**  | **1 B** | **0**    | **10393** |

`ggen_presized` is the same `AppendJSON` codepath but the caller reuses
a pre-sized buffer across calls (`make([]byte, 0, v.JSONSize())` once
outside the hot loop) — zero allocs, zero GC pressure, ~14% faster
than the convenience `encode.Marshal(v)` path. The 2 allocs on the
plain `ggen` row are the per-call output buffer + 1 misc; everything
else is appended in place. At Mega scale (~5.6 MiB) this beats the
nearest competitor (sonic_fast) by ~1.7× on wall clock and by
~33000× on allocated bytes.

**Reader input (streaming):**

| path                         | ns/op  | B/op    | allocs |
| ---------------------------- | ------ | ------- | ------ |
| jsonv2.UnmarshalRead         | 4237 K | 17.7 MB | 316834 |
| sonic.NewDecoder             | 2358 K | 39.0 MB | 137791 |
| sonic_fast.NewDecoder        | 2311 K | 39.0 MB | 137790 |
| easyjson.UnmarshalFromReader | 3204 K | 31.5 MB | 245886 |
| **ggen UnmarshalStream**     | 8094 K | 17.8 MB | 256589 |
| **ggen ReadAllUnmarshal**    | 2274 K | 29.0 MB | 101956 |

ggen Stream copies strings during parse (each scanned string is its
own heap alloc), which is why it loses ground on alloc count. The win
returns on **Marshal** (1.47× faster than easyjson) and the
**bytes-only path** (1.45× faster than easyjson). The cleanest "I have
an io.Reader" pattern is `ReadAllUnmarshal` — same shape as the bytes
path, comparable wall clock at the cost of one `io.ReadAll` buffer.

**Where streaming actually pays off:** fail-fast on validation
errors. `BenchmarkSlowStream_Invalid/ggen_stream` rejects a malformed
payload after reading just enough bytes to decode the bad field
(~67 ms), vs `BenchmarkSlowStream_Invalid/ggen_readall` which has
to consume the whole body first (~78 ms). That ~11 ms gap is real;
on bigger payloads or slower readers it grows linearly.

**Residency (retained heap per decoded item, slowPayload ~36 KiB):**

| codec            | per-item   | factor over JSON payload |
| ---------------- | ---------- | ------------------------ |
| **ggen_bytes**   | 66.1 KiB   | 1.89× (lowest)           |
| easyjson         | 78.3 KiB   | 2.23×                    |
| stdjson          | 79.5 KiB   | 2.27×                    |
| **ggen_stream**  | 87.0 KiB   | 2.48×                    |
| ggen_readall     | 107.1 KiB  | 3.05×                    |
| sonic            | 111.3 KiB  | 3.17×                    |
| sonic_fast       | 112.0 KiB  | 3.19×                    |

The single biggest residency win was **dropping `maxlen=N` as a
prealloc hint** — it cut bytes-path retention from 163 → 65 KiB/item.
See "tried and rejected" for the full thread (arena codegen,
inline scratch buf, alias-mode + pool reuse) — none of those moved
the residency needle, only the maxlen change did.

On the tiny complex payload (~440 bytes): Unmarshal ~415 ns, 2 allocs,
~1 GB/s — still the fastest.

`B/op` notes:

- **Marshal (`ggen` row):** B/op ≈ output buffer size (~11.8 MB =
  the marshalled wire bytes themselves). Only 2 allocs/op — the
  output buffer + 1 misc. The `JSONSize()` upper bound is what sizes
  that one allocation; per map entry costs `4 + 2*len(k) +
  value-bound`, or a flat 128-byte fallback for nested/struct values.
  Down from a flat `128 * len` (~2.4× overshoot pre-tighten). For
  the truly-zero-alloc shape see `ggen_presized` (4 B/op, 0 allocs).
- **Marshal (`ggen_presized` row):** caller-owned buffer + ggen's
  AppendAny concrete-type fast paths for every primitive shape — `[]any`
  / `[]string` / `[]int*` / `[]uint16/32/64` / `[]float*` / `[]bool`,
  plus `map[string]any` / `string` / `int*` / `uint*` / `float*` / `bool`
  (concrete-type cases that bypass reflect.MapIter boxing) — net zero
  allocations per marshal, zero GC pressure.
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
4. **`Stream` is stack-allocatable; no pool.** `var s scan.Stream; s.Reset(r, buf)`
   and the caller owns `buf`'s lifecycle. There used to be an
   `Acquire`/`Release` pair around a `sync.Pool` of Streams; the pool
   was removed because it bundled too many implicit-lifetime
   assumptions about the buffer and led to silent corruption when
   callers reused buf across decodes. Honest API now: the caller
   knows what their buffer does.
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

Tests live in three places: the root module (CLI / parse / tags),
`integrationtests/` (every feature test that decodes/encodes through
generated methods), and `bench/` (benchmarks). `integrationtests/` is
its own Go module — it imports the root packages as a normal external
consumer would, so the test surface exercises the public API at the
same boundary users hit.

Root module (`./`):

- `parse_test.go` — annotation parsing, tag parsing, rule extraction,
  cross-package symbol resolution. Also hosts the test-only
  `generate(pkg, structs) ([]byte, error)` wrapper that materializes
  `generateTo`'s output into bytes; production code paths in main.go
  call `generateTo` directly against the destination \*os.File.
- `tags_test.go` — `json:"…"`, `ggen:"…"`, `mod:"…"` tag parser unit
  tests, including dive/keys prefix handling.
- `applicability_test.go` — rule-applicability matrix (string-only
  validators rejected on numeric fields, etc.).
- `cli_test.go` — CLI integration: builds the ggen binary in TestMain,
  exercises file-naming contract (single-file/dir, test/non-test, `-o`
  override), `./...` walk + dot/underscore-dir skip, and per-flag
  effects on generated output (`-marshal`, `-unmarshal`, `-pkg`,
  `-novalidate`, `-ignoreunknown`, `-htmlescape`, name filter).
- `bench_test.go` — `BenchmarkGenerate` cycles `generate()` over a
  representative fixture to track allocs/op across generator refactors.
- `log_test.go` — Logger level + sink behaviour.
- `encode/appendany_test.go` — `AppendAny` correctness + per-shape
  `BenchmarkAppendAny` / `BenchmarkAppendAny_Presized`. Lives next to
  the implementation, not in `integrationtests/`, so it has direct
  unexported-symbol access and runs without the integrationtests
  module setup.
- `scan/any_test.go` — `Any` / `AnyNumber` stdlib parity tests plus
  per-shape `BenchmarkAny_Shapes` across the same input mix.

`integrationtests/` (own module, imports root packages as a consumer):

- `shared_test.go` — shared annotated structs (Address, Node, …) used
  across the feature tests.
- `<file>_ggen_test.go` — generated methods, one file per annotated
  source. Each annotated test file carries
  `//go:generate ../ggen $GOFILE` and produces a sibling
  `<file>_ggen_test.go` next to it. Build tags on the source
  propagate to the generated file. To regenerate after editing tags
  or annotations: `(cd integrationtests && go generate ./...)`.
  Cross-file struct references (e.g. `pointer_test.go` field of
  type `Address` declared in `shared_test.go`) work on first run
  because single-file mode seeds the generator's known-types set
  with every annotated name in the package, so the codegen emits
  a direct `Address{}.DecodeFrom(...)` call rather than the
  encoding/json fallback.
- `payloads_test.go` — `complexPayload` + `complexValue` (used by
  roundtrip / stdcompat tests) and `megaPayload` / `megaValue` (1 MiB
  generated Node tree, fixed seed 1; used by `stdcompat_test.go` to
  exercise cross-compat at scale).
- `read_test.go` — basic Read tests + unknown-key error & ignoreunknown
  opt-in.
- `scan_decode_test.go` — bytes-path + stream-path correctness
  (including chunked-reader + tiny-hint-forces-grow).
- `stdcompat_test.go` — exhaustive cross-compat: for every annotated
  struct, ggen-marshal → jsonv2-unmarshal AND jsonv2-marshal →
  ggen-unmarshal; results re-marshaled via jsonv2 and compared as
  parsed `any` (map order and nil/empty-slice noise normalized).
- `htmlescape_test.go` — verifies literal default (jsonv2-shaped) +
  `htmlescape` opt-in (v1-shaped).
- `fuzz_test.go` — three fuzzers over `Node`: panic safety, roundtrip
  fixed-point, jsonv2-compat.
- `merge_test.go` — decode-into-receiver semantics: scalar persistence
  across omitted JSON fields, slice backing-array reuse via `[:0]`,
  map `clear()` reuse, JSON `null` → nil container, JSON `[]` / `{}`
  on non-nil vs nil receiver. Test pins the user-facing contract;
  changes to the reset/merge codegen MUST keep these passing.
- `alias_test.go`, `any_test.go`, `custom_test.go`,
  `decode_dups_test.go`, `dive_test.go`, `extra_test.go`,
  `fallback_test.go`, `hooks_test.go`, `inline_test.go`,
  `jsonsize_test.go`, `maps_test.go`, `mods_test.go`, `native_test.go`,
  `omit_test.go`, `pointer_test.go`, `richtypes_test.go`,
  `roundtrip_test.go`, `sql_test.go`, `wire_test.go` — per-feature
  coverage.
- `thirdparty/` + `thirdparty2/` — non-annotated and annotated external
  package fixtures (exercise cross-package generated-decoder pickup
  via `packages.Load` and the `encoding/json` fallback for unannotated
  types).
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
- `bench/small_test.go` — small-value (~2.9 KiB ValidPayload) variants
  of the Unmarshal + Reader paths. Companion to mega: at this size the
  decoded value is small enough that per-call buffer management /
  streaming overhead is visible rather than drowned by tree-walk cost.
  Two ggen-stream Reader rows (512-byte initial buf vs payload-sized
  buf) isolate the buffer-grow chain from steady-state throughput.
- `BenchmarkRetention` in `bench/mega_test.go` — folded the old
  `TestResidency` into a parallel-safe bench. Each goroutine holds
  its produced `*Node` values in a local sink; sinks merge after
  `b.RunParallel`; GC × 2; snapshot `runtime.MemStats.HeapInuse`
  delta divided by `b.N` gives `retain_KB/op`. `HeapInuse` is
  process-global so the technique works in parallel. Best run with
  a fixed iter count (`-benchtime=1000x`) for comparable per-codec
  numbers.

### Cross-codec bench hygiene: easyjson method leakage

`//easyjson:json` generates `MarshalJSON` / `UnmarshalJSON` on the
target type. The standard library's reflection-based codecs (`jsonv2`,
stdlib `encoding/json`) and sonic ALL check the
`json.Marshaler` / `json.Unmarshaler` interfaces before falling back to
reflection — so any type carrying easyjson methods silently routes
every "reflection" codec through easyjson's hand-rolled fast path.
The bench row labelled `jsonv2` or `sonic` ends up measuring easyjson,
not the codec it claims to.

**Pattern:** keep ggen and easyjson on SEPARATE types that share the
wire shape. The bench feeds the "Plain" (ggen-only) struct to the
reflection codecs and the "Easy" struct to the easyjson row.

```go
//ggen:generate
type Claim struct { Sub string `json:"sub"`; ... }

//easyjson:json
type EasyClaim struct { Sub string `json:"sub"`; ... }   // same fields
```

`NodePlain` / `AddrPlain` exist for the same reason at the mega level
(self-referential field types meant the simpler `type AddrPlain Addr`
pattern wasn't enough — see the existing comments in `bench/types.go`).
For non-recursive structs (Claim, ValidationHeavy, HTMLPlain) a parallel
struct declaration is the cleanest approach.

Symptom when this is forgotten: the supposedly-reflection bench row
matches easyjson's allocs and ns/op almost exactly, when it should be
3-10× slower. Anything similar in a new bench → check the type doesn't
carry easyjson methods.

ggen's own `AppendJSON` / `DecodeFrom` methods do NOT trip the same
hazard — they're not `json.Marshaler` / `json.Unmarshaler`. Only the
stdlib-interface methods (which easyjson emits, and which ggen's
`//ggen:generate marshal` / `unmarshal` opt-ins also emit) cause
cross-codec pickup. If a struct opts into ggen's marshal/unmarshal
hooks, the same isolation pattern applies.

Running tests — each sub-module is reached by `cd`-ing into it since
`./...` from the root does not cross module boundaries:

```sh
GOEXPERIMENT=jsonv2 go test ./...
(cd integrationtests && GOEXPERIMENT=jsonv2 go test ./...)
(cd bench && GOEXPERIMENT=jsonv2 go test ./...)
```

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
go build -o ggen .
./ggen .
(cd bench && GOEXPERIMENT=jsonv2 ../ggen ./...)
(cd integrationtests && GOEXPERIMENT=jsonv2 ../ggen ./thirdparty2)
# easyjson for bench:
easyjson bench/types.go
# integrationtests is wired with //go:generate directives; from inside
# the sub-module, prefer the standard go-tooling entry point:
(cd integrationtests && GOEXPERIMENT=jsonv2 go generate ./...)
```

ggen is now module-scoped — `./...` from the root visits ONLY root-
module packages. `bench/` and `integrationtests/` each carry their own
`go.mod` and must be regen'd from inside (one invocation per module),
the same way `go build ./...` already behaves on this repo. The whole
project regen is therefore a small loop rather than a single command.

In `integrationtests/`, each annotated source file carries its own
`//go:generate ../ggen $GOFILE` directive and emits a sibling
`<file>_ggen_test.go`. Build tags on the source propagate to the
generated file. Files behind opt-in tags (e.g.
`//go:build goexperiment.jsonv2 && ggen_brokencodegen`) are skipped by
the default `go generate` invocation — pass `-tags=...` to opt in.

`go install github.com/sirkostya009/ggen@latest` gives users the CLI binary.
The subpackages (`decode`, `decode/validation`, `encode`, `scan`) are importable by
their generated code.

## Backlog (ideas worth pursuing, not yet scheduled)

- **Improve fuzz coverage.** Current fuzz surface (in
  `integrationtests/fuzz_test.go`) is three fuzzers over `Node`:
  `FuzzScanNoPanic` (panic safety on random bytes), `FuzzRoundtrip`
  (encode → decode fixed-point after one round), and `FuzzCompat`
  (ggen ↔ jsonv2 agreement when both accept). Gaps worth closing:
  per-feature fuzzers covering the corners `Node` doesn't reach —
`  alias types (primitive, struct, container variants), every
  validation rule (oneof, runes, ascii, email, …) with rule-specific
  generators, the streaming path (`UnmarshalStream` over a chunked
  reader with varied chunk sizes), `[N]T` strict-length arrays,
  `KindAny`/`KindRawJSON` edge cases (deep nesting, mixed
  null/array/object), `omitempty`/`omitzero` round-trip stability,
  multierr accumulation. Add per-fuzzer seeds for known-tricky
  inputs (truncated `\uXXXX`, surrogate pairs, `null` mid-value,
  trailing-garbage variants).

- **Add more CLI flags.** Specifically what's missing is TBD —
  candidates to consider when revisiting: a `-out-dir` for shared
  output (vs the current next-to-source layout), per-struct selectors
  beyond the trailing-name filter (`-only=Foo,Bar` style), and an
  explicit `-tag <tag>` to scope generation to one build-tag bucket.
  None are urgent; pick the ones that map to a real workflow before
  adding. (`-dry` shipped — parse + validate without writing any
  file; entry points in `check.go` are factored for the future
  `ggenvet` tool to reuse.)

- **Custom vet tool.** Ship a `ggenvet` (`go vet -vettool=ggenvet`)
  binary that catches misuses the compiler can't see. The biggest one
  is the **zero-copy aliasing footgun**: decoded strings alias the
  source `[]byte`, so mutating the input after `DecodeFrom` silently
  corrupts the decoded values. A flow-sensitive check that flags any
  write to a `data` arg (slice index assignment, `append` over the
  same backing, `copy(data, …)`) after it was passed to a ggen
  `DecodeFrom` would catch real bugs. Other candidates:
    - **stale generated file** — find a struct with `//ggen:generate`
      whose `<dir>_ggen.go` is missing the corresponding method set
      (e.g. field added after last regen).
    - **annotation/tag mismatch** — `ggen:"required"` on a pointer
      field marked `omitempty`; `ggen:"oneof=…"` whose values don't
      lex as the field's kind; `mod:"trim"` on a non-string.
    - **validation-rule applicability gaps** — extend the parse-time
      matrix into vet so misuses appear at `go vet` time instead of
      next codegen.
  Distribution shape: separate `ggenvet/` subpackage with its own
  `main.go` so users can `go install …/ggenvet@latest`. Reuses ggen's
  parse layer (`packages.Load` + the tag parser) so checks stay in
  sync with codegen rules.

- **`AppendAny` output prealloc via size precalc.** Bench data shows
  ggen ties or barely beats jsonv2/stdjson on typed slice marshal
  ([]int 712 vs 745 ns/op, []float64 2875 vs 2873 ns/op) despite
  beating them 2-4× on every map shape. Root cause: bench passes
  `nil` dst, so `AppendAny` runs the `append` growth chain (0 → 8 →
  16 → … → 1024), paying 7-8 allocs for output bytes that total
  ~330 B. stdjson hides this with a sync-pooled `encodeState`
  buffer; jsonv2 similarly pre-sizes via its own pool. Presized
  caller-owned buffer benchmarks (`BenchmarkAppendAny_Presized`)
  drop to 0 allocs and ggen wins by 1.85× on `[]int`. Options:
    - **Pre-walk for size**, like ggen-generated code does via
      `JSONSize()`. AppendAny has no compile-time type info so the
      walk is reflect-driven — fine for primitive maps/slices
      (typed range, no boxing), expensive for arbitrarily-nested
      `[]any` / `map[string]any` (recursive reflect descent).
      Heuristic: walk only when `cap(dst) == 0` AND the input is a
      concrete homogeneous container we already have a fast path
      for (typed slice/map); skip the walk for `[]any` /
      `map[string]any` / opaque interfaces where the bound is
      unbounded anyway. ~12 fast-path cases × ~5 lines each =
      cheap to add.
    - **Internal `sync.Pool` inside the encode package's `Marshal`**
      (NOT `AppendAny` itself — caller-owned `dst` semantics stay
      intact). Same trick stdlib uses. Wins on the implicit-buffer
      `Marshal(v)` shape, no change for callers who already pass
      a sized dst.
    - **Explicit hint API** `AppendAnySized(dst, v, hint)` — honest,
      no hidden state, caller picks the bound. Pairs naturally
      with ggen-codegen sites that already know the size.
  Pick when there's a real workload pinning slice marshal as a
  hotspot. Today the map wins dominate; slice tie is acceptable.

- **Wrap parse errors in `decode.ParseError` with position context.**
  Today `scan.ErrBadString` / `ErrBadObject` / `ErrBadNumber` /
  `ErrUnexpectedEnd` are bare sentinels with no `where` and no
  `what`. A user gets `"ggen: bad string"` and has to bisect the
  payload by hand. Wrap them at the call site with:
    - **byte offset** — `pos int` from the scanner state at the
      moment of failure. Cheap; already in scope.
    - **field path** — `field string` (or `[]string` for nested
      struct/slice positions) accumulated as the generated
      dispatch loop descends. Codegen emits the field name in
      each case body; would need to thread a path-stack
      argument through `DecodeFrom` recursion (cost: extra
      param on the hot path — measure carefully).
    - **nearby bytes window** — `snippet []byte` around `pos`
      (e.g. ±32 B), aliased into the input via `unsafe.String`
      so no copy. Lets the error message render `... abc <here>
      def ...` style.
    - **rule** — which scan primitive failed (`"string"`,
      `"object-close"`, `"number"`). Maps 1:1 to the existing
      sentinel; just promote it from package-level var to a
      field on the wrapper.
  Shape: `type ParseError struct { Field, Rule string; Pos int;
  Snippet []byte; Err error }` with `Unwrap()` returning `Err` so
  `errors.Is(err, scan.ErrBadString)` keeps working. Field-path
  threading is the cost driver; if measurements show a regression
  on the hot path, keep the path optional (zero-cost when nil) and
  let users opt in via a build tag or runtime flag.

- **Position context on `validation.*` errors.** Same idea, one
  layer up. Today `MinLenError{Field, Limit, Got}` etc. carry
  the logical field name but not the byte offset where the
  bad value was scanned. Adding `Pos int` (and maybe `Snippet
  []byte`) would let consumers underline the offending region
  the same way the parse-error wrap above does for scanner
  failures. Generated code already has `pos`/`s.Pos` in scope
  when it raises the error — threading it into the literal
  struct is one extra field per call site. Wire-shape
  implication: the validation.Error interface grows or gets a
  sibling `PositionedError interface { error; Pos() int }` so
  consumers can probe without breaking existing match patterns.
  Pair with the parse-error wrap above so a single fail-site
  logging format covers both error kinds.

- **Revisit `validation.CustomError` shape.** Today it carries
  `{Field, Name string, Cause error}` and exposes `Unwrap()`. Specifics
  TBD, but the current shape has rough edges worth a pass:
    - `Name` doubles as the rule identifier in messages ("validation
      %q failed") and as the registry key — separating those (e.g.
      `Rule string` for the rule name vs `Name string` for the
      user-facing label) would let downstream consumers match on rule
      identity without string comparison.
    - No `Value any` field like the other typed errors carry, so a
      `CustomError` doesn't expose what the user's validator rejected.
      Adding one would unify the inspect-failure pattern across all
      `validation.Error` types.
    - `Cause` is `error` but in practice it's almost always the user
      function's return — a typed sub-interface could make the
      `errors.As` shape more useful.
  Pick the angle when there's a concrete report-shape ask.

## Tried and rejected (don't re-attempt without new evidence)

- **Generator emitting `go/ast` nodes instead of text.** Full rewrite
  lives on the `ast-conversion` branch (commit `feadbba`). Each renderer
  returns `[]ast.Stmt`; `renderStructMethods` composes the four core
  methods (DecodeFrom, DecodeStreamFrom, JSONSize, AppendJSON) plus the
  optional Marshal/UnmarshalJSON hooks as `*ast.FuncDecl`s, then the file
  emits via `format.Node`. Generated code came out byte-identical.
  Rejected for three reasons:
    1. **Less readable.** Every `fmt.Fprintf(b, "if %s == nil {…", ref)`
       turns into an `&ast.IfStmt{Cond: &ast.BinaryExpr{…}, Body:
       &ast.BlockStmt{List: …}}` tree. Render code becomes pointer-
       struct boilerplate; you can't skim the rendered Go shape out of
       the generator source anymore.
    2. **Higher peak RAM.** AST nodes are pointer-heavy Go structs that
       survive until the whole file is printed.
    3. **Marginally slower codegen.** Small but consistent regression.
    4. **Slightly larger binary footprint.** Another unwanted thing.

  Kept on the branch in case the AST layer ever enables an `ast.Walk`-
  based optimization not feasible against text (e.g. replacing
  `coalesceConstAppends`), but no current use justifies the cost.

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
  work in the original tests because Go's escape analysis can't prove
  `&s` is safe across the `zero.DecodeStreamFrom(&s)` call inside
  what was then a generic `decode.UnmarshalStream[T]` wrapper — the
  generic dispatch defeated it, so the entire `Stream` (now including
  the 512-byte array) got heap-allocated. The wrapper is now gone and
  the call site is direct (`var s scan.Stream; ...; T{}.DecodeStreamFrom(&s)`),
  so the escape constraint may no longer apply — worth re-measuring
  if a new residency push needs the small-payload alloc back.

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
  decoder narrows the gap on Mega Unmarshal in part because bytedance
  hand-wrote AMD64 assembly that uses AVX2 for string-quote scanning,
  whitespace skipping, and number parsing. ggen currently does these
  byte-at-a-time in `scan/scan.go` and `scan/stream.go`. Candidates
  worth probing:
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

Fuzz tests live in `integrationtests/fuzz_test.go` — three targets
over `Node`: `FuzzScanNoPanic` (panic safety on random bytes),
`FuzzRoundtrip` (encode → decode is a fixed point after one round),
`FuzzCompat` (when both ggen and jsonv2 accept input, decoded values
must agree via `sameWire`). Run a target with `go test -run=^$
-fuzz=FuzzX -fuzztime=30s` from inside `integrationtests/`. Known
accept/reject drifts the compat target deliberately ignores:
top-level `null`, trailing garbage, invalid UTF-8 inside strings.

## Working with this file

**ALWAYS** keep this file up-to-date after making changes:

- **Update benchmarks**
- **CLI/Annotation flags**
- **Behaviour**
- **Usage**
- And so on...

### Sibling docs that MUST also be kept current

Every change that touches a user-visible surface must propagate to
**both** `README.md` and `SKILL.md` in the same PR — they're the user
and agent front doors respectively, and stale entries become bug
reports. Specifically:

- **CLI flag added / removed / renamed** → update the flag table in
  `README.md` (`### flags and annotations`) AND in `SKILL.md`
  (`## Flags (global) and per-struct annotations (local)`). For
  `SKILL.md`, also consider the `## Common user intents → flags`
  table — if there's a phrase a user would say to reach for the new
  flag, add it there.
- **Annotation token added / removed / renamed** → same two tables;
  the annotation column lives next to the CLI flag column.
- **Field tag syntax change** (`json:`, `ggen:`, `mod:`) → update
  `README.md`'s `## struct tags` section AND `SKILL.md`'s field-tag
  walkthrough.
- **New supported Go kind / wire-shape change** → README's
  `## supported kinds` AND SKILL's kind list.
- **New runtime API** in `decode` / `encode` / `scan` → README's
  `## generated methods` AND the relevant SKILL section.

When in doubt: if a future user could be confused by reading only the
README/SKILL and not seeing the new feature, the doc is stale. Do not
defer "I'll update the docs in a follow-up" — the follow-up never
ships. Bundle the doc update in the same commit as the code.

CLAUDE.md is the implementation log (the *why*); README and SKILL are
the surface (the *what* and *how*). All three move together.

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
