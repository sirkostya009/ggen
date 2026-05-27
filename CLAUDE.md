# ggen — zero-copy, zero-reflection JSON codegen for Go

Code generator. Parses annotated Go structs, emits `Marshal` / `Unmarshal`
methods that beat every reflection-based library (jsonv2, sonic, easyjson).
All decode work is hand-rolled byte scanning over the caller's `[]byte`;
strings alias the input via `unsafe.String` — no copy, no tokens, no AST.

Module: `github.com/sirkostya009/ggen`. Binary: `ggen` (CLI). Go ≥ 1.26.

This file documents the **CLI / codegen surface**. Runtime package
internals, benchmarks, integration-test conventions, and the
backlog live under each package's own CLAUDE.md (see "Repo layout"
below).

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
├── parse_test.go, tags_test.go, cli_test.go, …         ← CLI tests
├── bench_test.go                                       ← BenchmarkGenerate (generator perf only)
├── decode/             → see decode/CLAUDE.md
├── decode/validation/  → see decode/validation/CLAUDE.md
├── encode/             → see encode/CLAUDE.md
├── scan/               → see scan/CLAUDE.md
├── integrationtests/   → see integrationtests/CLAUDE.md (own Go module)
├── bench/              → see bench/CLAUDE.md            (own Go module)
└── .claude/backlog.md  ← ideas worth pursuing, tried-and-rejected, maybe-someday
```

`bench/` and `integrationtests/` are each their own **separate Go
module** (each has its own `go.mod` with `replace
github.com/sirkostya009/ggen => ../`). See those subdirectories'
CLAUDE.md files for the rationale.

The root module's tests cover the CLI itself (parse/tag/
applicability/log) plus one generator bench (`BenchmarkGenerate`).
All other benches live in `bench/`. All feature/roundtrip/compat/
fuzz tests live in `integrationtests/`.

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

**Important: run `ggen` with the same `GOEXPERIMENT` env that the
user's code is built with** — packages.Load honors build tags, so
files behind `goexperiment.jsonv2` are otherwise invisible.

The load Mode intentionally omits `packages.NeedDeps`. Imported
package signatures come from compiled export data (gcexportdata)
instead of re-typechecking every transitive dep from source.
Method-set lookups on imported types still work — export data
carries methods. Peak RSS on `ggen ./...` drops from ~1.4 GB to
~50 MB for projects with heavy import graphs (sonic / easyjson /
jsonv2). When the export data is unavailable (rare — fresh
checkout with no `go build` ever), the AST-only fallback emits
`encoding/json` for the affected type. Soft degradation, not a
hard failure.

Pattern mode (`./...`, `./sub/...`, `...`) resolves the pattern
via `packages.Load` — same dispatch as `go build <pattern>` /
`go test <pattern>`. Module-scoped and workspace-aware; never
crosses module boundaries. A subdirectory carrying its own
`go.mod` is a separate module and is skipped under the parent's
`./...`, same as `go build`; multi-module repos invoke ggen once
per module (or wire a `go.work` + per-module loop) the way they
already do for `go build` / `go test`. Test-only packages (no
non-`_test.go` files) are skipped — the discovery `Mode` doesn't
set `Tests: true`, mirroring `go list`'s default. Single-package
mode (`ggen <dir>`) still picks them up.

Processing order is post-order over the matched import subgraph
(deps first within the matched set) so each parent's
`parsePackage` reads its already-generated child `_ggen.go` and
routes cross-package field types through direct `DecodeFrom` /
`AppendJSON` calls instead of the `encoding/json` fallback.
Transitive deps outside the matched set are left alone — they're
someone else's run. Pattern-mode work runs sequentially in topo
order; the per-pkg parse cost dominates and the generator's
global lock already serializes the codegen phase, so the old
depth-keyed goroutine fanout is gone.

Dot- and underscore-prefixed dirs (`.git`, `_build`, …),
`vendor/`, `testdata/`, and `node_modules/` are skipped
automatically — `go list` already filters them out under any
pattern. No custom skip rule lives in ggen anymore.

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
  through `encode.AppendAny` — see `encode/CLAUDE.md` for the type-switch
  ordering rules.
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

Generated structs implement `decode.Decoder[T]` (see
`decode/CLAUDE.md`) and `encode.Marshaler` (see `encode/CLAUDE.md`)
— that's the entire surface. Top-level entry points
(`UnmarshalSlice`, `ReadSlice`, `UnmarshalSliceStream`, `Marshal`,
`MarshalString`, `Write`, `MarshalSlice`, `MarshalSliceString`,
`WriteSlice`, `AppendSlice`) live in the `decode` and `encode`
packages as generic functions. The generated file stays small:
4 methods per struct (plus 0–2 opt-in JSON hooks).

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
  work — see @./.claude/backlog.md.
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
    duplication. See `decode/validation/CLAUDE.md` for the typed-error
    surface.
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
    JSONSize budgets the nil-as-null case directly: slice and map
    fields reserve 4 bytes (`null`) instead of 2 (`[]` / `{}`) so
    the cap covers both arms. `sql.Null*` does the same — the
    `!Valid` branch emits `null` (4) which exceeds the inner-value
    constant for thin inner kinds (notably KindString = 2 for the
    surrounding quotes), so the constant is widened to `max(inner,
    4)`. Arrays keep the 2-byte budget — they can't be nil.
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
    decoded value, inflating mheap span retention. `KeyView` aliases
    on the happy path; the alias stays valid even if buf grows
    because GC pins the old backing. Falls back to `stringSlow` for
    escape sequences. Key strings never escape the dispatch frame,
    so safety holds. See `scan/CLAUDE.md` for the KeyView contract.
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
    slices/maps.** See `encode/CLAUDE.md` for the full type-switch
    ordering rules. Net wins on 32-entry shapes:
    `map[string]int` 4403 → 1579 ns/op (2.8×, 71 → 7 allocs);
    `map[string]bool` 3449 → 944 ns/op (3.7×, 71 → 7 allocs);
    `map[string]float64` 6459 → 3417 ns/op (1.9×, 72 → 8 allocs).
    Outpaces both stdjson v1 and jsonv2 on every map shape after
    the fix.
35. **`AppendAny` concrete cases for `json.RawMessage`, `time.Time`,
    and pointer-to-primitive.** All three would otherwise route via
    the `case json.Marshaler:` branch or `reflect.Pointer` fallback.
    Concrete cases pre-empt both. Measured wins (vs jsonv2, 32-byte
    shapes): `json.RawMessage` 227 → 28 ns/op (8.1×, 2 → 1 alloc);
    `time.Time` 181 → 117 ns/op (1.55×); `*int` 70 → 26 ns/op
    (2.7×); `*bool` 83 → 19 ns/op (4.4×). See `encode/CLAUDE.md`
    for case ordering — concrete cases MUST sit before the
    interface dispatches.

## Design decisions (the why)

1. **`unsafe.String` aliases are safe across buffer growth.** Go's
   GC is non-moving (mark-and-sweep, no compaction). An alias into
   the OLD backing array keeps that array live — GC walks string
   headers and marks the pointed-to memory. Stream can `append`-
   grow freely; aliases from previous values remain valid. (Detail
   in `scan/CLAUDE.md`.)
2. **Struct fields are sorted alphabetically at codegen time**
   (default). Zero runtime cost. Deterministic wire output,
   compresses better.
3. **No runtime reflection anywhere.** Even the cross-package struct
   fallback uses `encoding/json.Unmarshal` (which reflects) only for
   types NOT in the generation pass.
4. **`Stream` is stack-allocatable; no pool.** `var s scan.Stream;
   s.Reset(r, buf)` and the caller owns `buf`'s lifecycle. (Detail
   in `scan/CLAUDE.md`.)
5. **Custom validators / mods are codegen-time function injection.**
   Tags like `ggen:"@EvenOnly"` and `mod:"@Squash"` are resolved
   via `packages.Load` at parse time — the generator looks up the
   named function, validates its signature against the field's
   exact go/types type, and emits a direct call. No runtime
   registry, no `func(any) any` boxing, zero allocs from the rule
   itself. Cross-package via `@pkg.FuncName` resolves through the
   source file's import block; blank imports
   (`_ "path/to/lib"`) work for libs the user pulls in solely for
   ggen's benefit. Validator errors wrap as `validation.CustomError
   {Name: "@FuncName", Cause: err}`; fallible mod errors
   (`func(T) (T, error)`) propagate as parse errors (same level
   as `scan.ErrBadX`), not validation.

## Test files (root module only)

The root module's tests cover the CLI itself. Per-package tests
live next to their implementation (`encode/appendany_test.go`,
`encode/url_test.go`, `scan/string_test.go`, `scan/number_test.go`,
`scan/stream_test.go`, `scan/any_test.go`). Feature / roundtrip
/ compat / fuzz tests live under `integrationtests/` — see
`integrationtests/CLAUDE.md`. Benchmarks live under `bench/` —
see `bench/CLAUDE.md`.

- `parse_test.go` — annotation parsing, tag parsing, rule
  extraction, cross-package symbol resolution. Also hosts the
  test-only `generate(pkg, structs) ([]byte, error)` wrapper that
  materializes `generateTo`'s output into bytes; production code
  paths in main.go call `generateTo` directly against the
  destination `*os.File`.
- `tags_test.go` — `json:"…"`, `ggen:"…"`, `mod:"…"` tag parser
  unit tests, including dive/keys prefix handling.
- `applicability_test.go` — rule-applicability matrix (string-only
  validators rejected on numeric fields, etc.).
- `cli_test.go` — CLI integration: builds the ggen binary in
  TestMain, exercises file-naming contract (single-file/dir,
  test/non-test, `-o` override), `./...` walk + dot/underscore-dir
  skip, and per-flag effects on generated output (`-marshal`,
  `-unmarshal`, `-pkg`, `-novalidate`, `-ignoreunknown`,
  `-htmlescape`, name filter).
- `bench_test.go` — `BenchmarkGenerate` cycles `generate()` over
  a representative fixture to track allocs/op across generator
  refactors.
- `log_test.go` — Logger level + sink behaviour.

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

ggen is module-scoped — `./...` from the root visits ONLY root-
module packages. `bench/` and `integrationtests/` each carry their
own `go.mod` and must be regen'd from inside (one invocation per
module), the same way `go build ./...` already behaves on this
repo. The whole project regen is therefore a small loop rather
than a single command.

In `integrationtests/`, each annotated source file carries its own
`//go:generate ../ggen $GOFILE` directive and emits a sibling
`<file>_ggen_test.go`. See `integrationtests/CLAUDE.md`.

`go install github.com/sirkostya009/ggen@latest` gives users the
CLI binary. The subpackages (`decode`, `decode/validation`,
`encode`, `scan`) are importable by their generated code.

## Working with this file

**ALWAYS** keep this file up-to-date after making changes:

- **CLI / annotation flags**
- **Codegen behaviour**
- **Wire format**
- **Generated method surface**
- And so on...

Benchmark numbers go in `bench/CLAUDE.md`. Test-suite layout goes
in `integrationtests/CLAUDE.md`. Per-package runtime details
(scan primitives, encode helpers, decode interface, validation
errors) go in the matching package CLAUDE.md. Backlog /
tried-and-rejected go in `.claude/backlog.md`.

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
  `## generated methods` AND the relevant SKILL section. Also
  update the matching package CLAUDE.md.

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
