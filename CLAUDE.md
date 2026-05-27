# ggen — zero-copy, zero-reflection JSON codegen for Go

Code generator. Parses annotated Go structs, emits methods on annotated structs.
Hand-rolled byte scanning over caller's `[]byte` or `*scan.Stream`;
bytes path alias input via `unsafe.String` — no copy, no tokens, no AST.

This file documents the **CLI / codegen surface**. Runtime package internals,
benchmarks, integration-test conventions, and the backlog live under each
package's own CLAUDE.md (see "Repo layout").

## Repo layout

```
schema/
├── main.go, parse.go, generate.go, tags.go, types.go   ← CLI (package main)
├── introspect.go                                       ← go/types interface detection (TextAppender, TextMarshaler, …)
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

`bench/` and `integrationtests/` are **separate Go modules**. The root
module's tests are unit tests and CLI integration tests + generator bench
(`BenchmarkGenerate`); other benches live in `bench/`,
post-generation integration tests in `integrationtests/`.

## Generator CLI (`main` package)

### Invocation

```
ggen ./...                    every package matched by the pattern (module-scoped, as `go build`)
ggen <dir>                    one package
ggen <file.go> [Names...]     one file; optional struct name filter
```

Packages load via `golang.org/x/tools/go/packages` with full type info,
including interface implementations (TextMarshaler, ByteDecoder,
JSONMarshaler, …) are picked up and emitted as direct method calls - no
runtime probing. If can't resolve (temp file with no `go.mod`),
falls back to AST-only mode and emits a plain `encoding/json`
fallback for cross-package types.

Run `ggen` with same `GOEXPERIMENT` env as user's code. Files behind
`goexperiment.jsonv2` are otherwise invisible.

Pattern mode (`./...`, `./sub/...`, `...`) resolves via `packages.Load` —
module-scoped, workspace-aware, never crosses module boundaries. A
subdirectory with `go.mod` is skipped; multi-module repos run ggen once
per module. Test-only
packages (no non-`_test.go` files) are skipped in pattern mode, picked up in
single-package mode. Processing is post-order over the matched import
subgraph (deps first). Transitive deps outside the matched set are left alone.
Work runs sequentially in topo order (parse cost dominates; codegen is globally
locked). Dot/underscore-prefixed dirs, `vendor/`, `testdata/`,
`node_modules/` are skipped by `go list` — no custom skip rule in ggen.

### Flags (all opt-in, apply to every struct in the pass)

- `-o <path>` — override output path (single file / single dir only)
- `-pkg <name>` — override package name in output
- `-marshal` — emit `MarshalJSON` method
- `-unmarshal` — emit `UnmarshalJSON` method
- `-multierr` — accumulate validation failures into `validation.Errors`,
  return at the end of parse, parse errors always return right away
- `-allowdups` — allow duplicate keys in payload, first-wins - next occurences
  are skipped. Default: error with `validation.DuplicateKeyError`
- `-novalidate` — skip validation rules, required-field checks, mods
- `-ignoreunknown` — silently skip unknown JSON keys. Default: error with
  `validation.UnknownKeyError`. Overridden when an inline map field is present
- `-nosortkeys` — emit fields in Go declaration order. Default: sorted
  alphabetically. Inline map fields stay last
- `-usenumber` — decode JSON numbers into `any` fields as `json.Number`
  instead of `float64`. Mirrors stdlib `UseNumber()`
- `-htmlescape` — opt INTO HTML-safe escaping (`<`, `>`, `&` → `\uXXXX`) on
  marshal. Default is literal
- `-dry` — parse + validate annotated structs, surface every error, emit
  no file. Composes with `-v` (prints `ok <path> (N structs)` per package).
  Rejects `-o` / `-pkg`

### Per-struct annotations

Comment on the struct (or gen-decl) is a `//ggen:generate` directive (no
space after `//`, mirroring `//go:generate`).
Space-separated tokens after:

- `marshal`
- `unmarshal`
- `multierr`
- `allowdups`
- `novalidate`
- `ignoreunknown`
- `nosortkeys`
- `usenumber`
- `htmlescape`

Annotations apply only to a struct.

## Struct tags (on fields)

- `json:"name"` — JSON key name (field ignored otherwise)
- `json:"-"` — field set to ignored explicitly
- `json:",inline"` — catch-all map for unknown keys. Type must be
  `map[string]any`. Overrides `ignoreunknown`. Entries spliced out on marshal
- `json:"name,omitempty"` — not marshaled when JSON-empty (null, "", [], {})
- `json:"name,omitzero"` — not marshaled when Go-zero value
- `json:"name,string"` — wrap primitive as JSON string on marshal, unwrap on
  unmarshal. Supports primitive types only, similar to stdlib
- `json:"name,format:X"` — format hint for native types (see Kinds).
  **jsonv2 requirement: `format:X` must be LAST in the tag.**

- `ggen:"..."` — validation rules, comma-separated. Three mode prefixes
  partition the rule list (any order):
    - (default, no prefix) — applies to the field itself (or whole
      slice/map for container fields)
    - `dive:` — rules after apply to the next level down. Each additional
      `dive:` peels one more level (`[][]T`: first dive → each `[]T`, second
      → each `T`). Works for arbitrarily-deep slices
    - `keys:` — rules after apply to map keys only (always `string`)

    Example: `ggen:"minlen=1,dive:maxlen=10,dive:required,keys:minrunes=2,maxrunes=32"`.
    Rules:
    - `required`, `optional`, `notempty`
    - `len=N`, `minlen=N`, `maxlen=N` (len on strings/slices/maps)
    - `hintlen=N` — **preallocation hint, NOT validation**. Lifted out at
      parse time. Overrides `len`/`minlen` for sizing
      `make([]T,0,N)` / `make(map,N)`. Use when payload lands near N but the
      validation bound is much larger or absent
    - `runes=N`, `minrunes=N`, `maxrunes=N` (utf8.RuneCountInString)
    - `gt=N`, `gte=N`, `lt=N`, `lte=N`, `eq=N`, `neq=N` (numeric; or string
      eq/neq on strings)
    - `oneof=a|b|c` (strings or numerics)
    - `email`, `url`, `ascii`, `printable`, `alphanum`, `numeric`, `lower`,
      `upper`, `hexadecimal` (strings only)
    - `starts=X`, `ends=X`, `contains=X` (strings only)
    - `multiple=N` (ints only)
    - `@FuncName` / `@pkg.FuncName` — custom validator resolved at codegen
      time. Signature `func(T) error`. Looked up w/ `packages.Load`,
      type-checked at generate time. Cross-package via source file's import
      block; file-scoped aliases and blank imports work too

- `mod:"..."` — input transforms, applied after decode, before validation.
  Same `dive:` / `keys:` prefixes.

    Rules:
    - String: `trim`, `lower`, `upper`, `trimleft=X`, `trimright=X`,
      `replace=old|new`
    - Numeric: `clamp=lo|hi` — rounds into `[lo,hi]`; either bound may be empty
      (`clamp=0|` floors at 0, `clamp=|100` caps at 100)
    - `@FuncName` / `@pkg.FuncName` — user transform. **pure** `func(T) T`
      emits `field = Func(field)`; **fallible** `func(T) (T, error)`
      propagates as a parse error (early return), NOT validation.
      Pure vs fallible detected in generate-time. Same lookup as validators
    - Mods can break zero-copy for the modified field (force a new string)

### Rule applicability (parse-time)

`applicability.go` rejects mismatched rules at parse time (with clear message).

Cases covered exhaustively in `TestCLI/InvalidRuleApplication` — add a table
entry there when a new rule lands.

## Generated methods (per annotated struct T)

```go
// DecodeFrom is a zero-copy parser. Strings and RawMessage are alised into data
func (result T) DecodeFrom(data []byte) (T, int, error)
// DecodeStreamFrom is a buffered io.Reader wrapper with an intermediate buffer.
// Useful for slow streams or lower memory usage. Break zero-copying - all strings
// and json.RawMessage are copied from payload.
func (result T) DecodeStreamFrom(s *scan.Stream) (T, error)
// JSONSize precalulcates size of JSON payload of T in bytes
func (s T) JSONSize() int
// AppendJSON appends a payload string to dst. Errors on invalid numbers (like NaN)
func (s T) AppendJSON(dst []byte) ([]byte, error)
```

**Cursor convention.** Bytes-path `DecodeFrom` takes a slice starting at the
value's first byte and returns bytes consumed; the caller advances its own
cursor (`i += n` after reslicing `data[i:]`). Stream-path `DecodeStreamFrom`
takes/returns no cursor — the cursor is `s.Pos`, owned by the Stream and
advanced in-place by every scan primitive. To capture a raw span:
`start := s.Pos; s.SkipValue(); raw := s.Bytes()[start:s.Pos]`.

**Decode-into-receiver semantics.** The receiver passed in IS the merge source.
Scalar fields persist across JSON omission (stdlib-merge shape); container
fields are reset at the top of DecodeFrom so the decoder never appends over
carried-in data:

- slices and `[]byte`: `if X != nil { X = X[:0] }` at entry (backing array
  reused; `make(...)` only when `X == nil`)
- `map[K]V`: `if X != nil { clear(X) }` at entry (buckets reused; `make` only
  when nil)
- nested struct: `result.X, _, _ = result.X.DecodeFrom(...)` — value-receiver
  takes the existing value as merge source automatically
- pointer `*T`: currently always allocates a fresh pointee
  (`var v T; ... result.X = &v`); receiver pointer discarded.
  Decode-into-receiver merge on the pointee is backlog
- fixed arrays `[N]T`: every slot overwritten or strict-length-errors, no entry
  reset needed

JSON `null` for slice/map sets `result.X = nil` (stdlib v1/v2 parity). JSON
`[]`/`{}` on a non-nil receiver keeps the `[:0]`'d / cleared container; on a
nil receiver allocates an empty non-nil container.

Call with a zero-value receiver for fresh decode (`T{}.DecodeFrom(data)` for
struct/slice/map/array; `var zero T; zero.DecodeFrom(data)` for primitive
aliases). To merge into an existing value, call its `DecodeFrom` directly.

Runtime entry points (call from user code):

```go
// bytes path — single value
T{}.DecodeFrom(data)                       // (T, int, error)

// stream path — single value
var s scan.Stream
s.Reset(r, buf)
T{}.DecodeStreamFrom(&s)                   // (T, error); recycle s.Bytes()

// array walkers
decode.UnmarshalSlice[T](data)             // ([]T, error)
decode.ReadSlice[T](r)                     // ([]T, error)
decode.UnmarshalSliceStream[T](r, buf)     // ([]T, []byte, error)

// encode package
encode.Marshal(t)            encode.MarshalString(t)          encode.Write(w, t)
encode.MarshalSlice(items)   encode.MarshalSliceString(items) encode.WriteSlice(w, items)
encode.AppendSlice(dst, items)
```

Opt-in (`//ggen:generate marshal` / `unmarshal`):

```go
func (s T) MarshalJSON() ([]byte, error)     // wraps encode.Marshal(s)
func (s *T) UnmarshalJSON(data []byte) error // inlines var zero T; zero.DecodeFrom(data)
```

## Top-level type aliases

Annotated named types (`//ggen:generate type T <underlying>`) get the same
method surface as a struct (DecodeFrom, DecodeStreamFrom, JSONSize,
AppendJSON), driven by `renderAlias*` helpers in `alias.go`. Top-level
renderers dispatch to alias paths when `s.IsAlias` is set (except struct
aliases that fall back to field introspection, which set `IsAlias=false` and
route through regular struct codegen).

Accepted underlying kinds:

- **primitive** (`string`, `bool`, `int*`, `uint*`, `float*`): scan via
  `scan.X` / `_s.X`, cast to alias. `htmlescape` flips the string-append helper
- **struct** (`type LocalUUID uuid.UUID`): methods don't propagate from the
  RHS, so probing uses `inspectType` on the RHS named type. Three-step ladder:
    1. _ggen-method delegation_ — if underlying has AppendJSON+DecodeFrom: cast
       → underlying.Method() → cast back. Cheapest
    2. _field introspection_ — plain struct with ≥1 exported field: walk
       `*types.Struct` via go/types, synthesize a FieldInfo per exported field
       (`extractFieldFromTypes`), `IsAlias` flips false, regular struct codegen
       runs. Field access via `result.X` is sound (identical memory layout).
       **Preferred over JSON/Text marshaler delegation even when those exist** —
       hand-rolled struct codegen beats reflective marshaler calls
    3. _JSON/Text marshaler delegation_ — opaque struct (no exported fields,
       e.g. `time.Time`) with a JSON or Text marshaler pair: cast → drive method
       → cast back

    Wire-shape implication: an alias of a struct with both exported fields AND a
    custom MarshalJSON uses the introspected field shape, NOT the underlying's
    MarshalJSON. For the underlying's exact shape, declare it with no exported
    fields (forces delegation) or write your own marshal hook

- **slice / map / array** (`type Tags []string`, `type Lookup
map[string]int`, `type Tuple [3]int`): synthetic FieldInfo (ElemType,
  ElemKind, ArrayLen) handed to the field-level emitters with `result`
  (decode) / `s` (encode) as ref. All field-level features carry over —
  strict-length arrays, slabbed `[]*T`, dive validation
- **`[]byte` alias**: collapses to KindBytes, base64 path

Rejected: channel, interface, function — no sensible JSON shape.

`htmlescape` / `marshal` / `unmarshal` annotations apply to all aliases;
`allowdups`, `ignoreunknown`, `multierr`, `novalidate` apply to struct aliases.

Foreign package handling: `aliasUnderlyingImports` collects foreign-pkg.
Field-introspection types render via `types.RelativeTo(s.typesPkg)`.

## Supported Go Kinds (per field)

- `string`, `bool`
- `int`/`int8`/`int16`/`int32`/`int64`, `uint`/`uint8`/`uint16`/`uint32`/`uint64`
- `float32`, `float64`
- Pointer to any of the above (`*T`) — null ↔ nil
- `[]T` (slice), `map[string]V` (string-keyed only)
- `[]*T` / `[N]*T` (slice/array of pointer-to-struct) — element pointers come
  from a single backing slab (`make([]T,0,cap)` slices, `[N]T` arrays) so N
  allocs collapse to ~log(N). Nil elements → nil pointers; encode nil → `null`
- Nested struct (generate-time probing of `DecodeFrom`/`UnmarshalText`/`UnmarshalJSON` and `AppendJSON`/`AppendText`/`MarshalText`/`MarshalJSON` methods; with default-stdlib fallback)
- Embedded struct (unnamed field) — fields promoted to parent's JSON object
- `time.Time` — `format:unix`/`unixmilli`/`unixmicro`/`unixnano`/`RFC3339`/
  `RFC3339Nano` + custom (jsonv2 supported) and other `time.X` constants
- `time.Duration` — `format:sec`/`milli`/`micro`/`nano`/`units` (default,
  parses `"1h30m"`)
- `net.IP`, `netip.Addr`, `netip.Prefix` — text form. Marshal via
  `encoding.TextAppender` decode via `net.ParseIP`/`netip.ParseAddr`/`netip.ParsePrefix`
- `[]byte` — `format:base64` (default)/`base64url`/`base32`/`base32hex`/
  `base16`(`hex`)/`array` (JSON array of numbers)
- `json.RawMessage` / `jsontext.Value` — opaque span via `scan.SkipValue`,
  aliased into the field. Raw passthrough on encode (`null` if empty/nil)
- `net/url.URL` — JSON string, `url.Parse` / `encode.AppendURL`
- `math/big.Int`/`big.Float`/`big.Rat` — `big.Int` a JSON number, `big.Float`
  / `big.Rat` JSON strings (`"3.14"` / `"3/2"`; wrapping prevents float64
  precision loss, matches jsonv2). Encoded via in-place `Append` (zero alloc),
  parsed via `SetString`/`Parse`
- Other types — no dedicated kind. Any field whose type
  implements `encoding.TextAppender` / `TextMarshaler` / `TextUnmarshaler`
  routes through those methods. Marshal prefers `AppendText(dst)` (zero alloc),
  falls back to `MarshalText() + AppendString` (one alloc). Can also declare a
  AppendJSON method for ggen to pick up (highest precedence).
- `database/sql.Null*` (`NullString`, `NullInt64`, `NullInt32`, `NullInt16`,
  `NullByte`, `NullBool`, `NullFloat64`, `NullTime`). Decode probes `null`
  first → `Valid=false`; else reads inner value, sets `Valid=true`. Encode
  `null` when `!Valid`, inner value otherwise
- `any` / `interface{}` — decode via `scan.Any` / `(*Stream).Any`, stdlib
  defaults: `null→nil`, `bool`, `number→float64`, `string` (zero-copy alias),
  `array→[]any`, `object→map[string]any`. With `usenumber`, emits
  `scan.AnyNumber` (numbers → `json.Number` aliased via `unsafe.String`).
  Encode via `encode.AppendAny` (type-switch ordering — see `encode/CLAUDE.md`)
- `[N]T` (fixed-length array) — JSON tuple with **strict count**: decode errors
  with `validation.LenError{Want:N}` when count ≠ N. Combines/nests freely:
  `[N]T`, `[][N]T`, `[N][]T`, `[N][M]T`, `[][N][M]T` via the same recursive
  emitter as `[][]T`. `[]byte` stays KindBytes (base64) — only non-byte arrays
  get tuple treatment

### Wire-format divergences from stdlib

Two kinds intentionally diverge from `encoding/json` v1 and v2. ggen marshal
output is _not_ a subset of either for these — feed-through-stdlib reshapes the
value, and decode of stdlib JSON won't work for these fields. Round-trip
within ggen is fine.

| Kind          | ggen wire             | stdlib wire (v1 + v2)                                    |
| ------------- | --------------------- | -------------------------------------------------------- |
| `net/url.URL` | `"https://x/p?q=1"`   | `{"Scheme":"https","Host":"x","Path":"/p", … 11 fields}` |
| `sql.Null*`   | inner value or `null` | `{"<Inner>":val,"Valid":true}` (plain struct, no hook)   |

## Output file naming

- Package mode, untagged: `<dir>_ggen.go` (non-test) / `<dir>_ggen_test.go`
  (test-only); both if both exist
- Package mode, tagged: `<dir>_<tag-slug>_ggen.go` per (tag, isTest) bucket
- Single-file: `<basename>_ggen.go` / `_ggen_test.go`; source `//go:build` line
  preserved in the header
- `_test.go` sources are first-class inputs; test-only struct annotations route
  to `_ggen_test.go`

### Build tag propagation

The generator reads `//go:build <expr>` per source file and BUCKETS annotated
structs by constraint. Each (tag, isTest) bucket emits its own gen file with
the matching header — a struct in `tagged.go` (behind `//go:build foo`) never
lands in the unconstrained `<dir>_ggen.go`. Old-style `// +build` honored;
multi-term exprs canonicalized via `go/build/constraint.Parse`. Cross-bucket
struct references in the same package still route through direct DecodeFrom —
`generatedTypes` is seeded with the union of all buckets before codegen.

Filenames: untagged buckets keep `<dir>_ggen.go` / `<dir>_ggen_test.go`;
tagged buckets become `<dir>_<slug>_ggen.go` (slug collapses non-alnum runs to
single underscores: `goexperiment.jsonv2` → `goexperiment_jsonv2`,
`foo && bar` → `foo_bar`).

## Optimizations applied in codegen (nothing at runtime)

1. **Length-first key dispatch.** Switch on `len(key)` before content — wrong
   lengths reject with one int compare. Nested switches only for lengths with
   ≥2 fields.
2. **Slice cap from tag hint.** `preallocCap` picks initial cap for
   `make([]T,0,N)`. Precedence: `hintlen=N` > `len=N` >
   `max(minlen, default)` > default (8 primitives, 4 structs). Maps via
   `mapPreallocCap` (no minlen — weak signal on maps).
3. **Field marshal order sorted by JSON name** at codegen time (alphabetical).
   `-nosortkeys` opts back to declaration order.
4. **Inlined scan primitives in hot path.** Raw byte-compare loops for
   `SkipSpace`, `String` (zero-copy happy path), `Int64`, `Uint64` emitted into
   each case body — no function-call overhead.
5. **Mod + validation after field read.** `renderMods` → `renderValidationOn`
   write directly into the parent buffer; `renderValidationOn`'s `posVar`
   param emits the right return shape inline (`return result, err` top level,
   `return result, i, err` mid-stream).
6. **Pointer fields** emit a 4-byte `null` peek → nil branch, else stack-local
   `var v <Pointee>` + recursive inner read + `&v`. Dispatch order in
   `renderField` is pointer-first → string-tag → kind switch (string-tag-first
   would emit broken `result.X = *int(n)`); pointer-first recurses with
   `inner.GoType = PointeeType`.
7. **Cross-package struct fallback (statically dispatched).** Method-set
   membership checked via `go/types` at codegen time; emits a single hardcoded
   call — zero runtime probes/itab lookups. Decode order: `DecodeFrom` →
   `UnmarshalJSON` → `UnmarshalText` → `encoding/json`. Marshal mirror:
   `AppendJSON` → `MarshalJSON` → `AppendText` (Go 1.24+, zero alloc) →
   `MarshalText` → `encoding/json`. When type info is unavailable (AST-only
   loader, bare temp dirs in tests), emits a plain `encoding/json` fallback.
8. **Inline map catch-all.** Unknown keys absorbed via
   `encoding/json.Unmarshal` over a captured raw span. Value type must be `any`.
9. **Marshal output cap.** `JSONSize()` upper bound → single
   `make([]byte,0,cap)` + `AppendJSON`. 1 alloc per top-level Marshal.
10. **Recursive nested-container emitter.** `emitByteSliceRead` /
    `emitStreamSliceRead` / `emitAppendSlice` / `emitSizeSlice` take a depth
    param and unify slice+array. When `ElemKind` is KindSlice/KindArray they
    recurse via `peelSliceField(f)` + `stripOneContainer(typ)` (strips one
    `[]`/`[N]` off `ElemType`, shifts `InnerValidation[0] → ElemValidation`,
    `[1:] → InnerValidation`). Arrays carry N via `ElemArrayLen` for
    strict-count at every level. All locals (`kN`, `evN`, `errN`, `iN`, `vN`,
    `_idxN`) carry the depth suffix.
11. **Map-key mods + validation.** `keyValidateAndMod` runs right after the key
    is read (before `:`), so key rules/mods short-circuit invalid keys before
    the value decodes.
12. **Marshal error propagation.** `AppendJSON` returns `([]byte, error)`;
    threaded through every nested call (struct/slice/map AppendJSON, cross-pkg
    JSON/Text Marshaler, TextAppender, `json.Marshal`). Pure-primitive structs
    declare `var err error; _ = err` (compiler elides it — no runtime cost).
13. **Typed validation errors + frozen OneOf slices.** Each rule has its own
    pointer-receiver error struct (`MinLenError`, `OneOfError`, …); generator
    emits the typed literal at the failure site. `OneOfError.Allowed` points to
    a deduped package-level frozen `[]string` (`var _oneof_N = []string{...}`)
    emitted once per unique allowed-set. `EqError`/`NeqError` use `Want any`
    (one struct for string + numeric). See `decode/validation/CLAUDE.md`.
14. **Constant-folded `JSONSize()`.** Each field's size splits into a
    compile-time constant (folded into `size := N`) and a runtime expression
    (loops, len(), recursive calls). Pure-primitive structs collapse to
    `return N`.
15. **Opening-quote folding.** At struct-field top level, when a value emit
    begins with `"` (string, URL, big.Rat, time/RFC3339, duration/units,
    base64/hex bytes, net.IP/netip), the opening quote folds into the constant
    `"key":` → `"key":"`; the value emitter writes only body + closing `"`.
16. **First-element-then-rest slice loop.** First element emitted directly (no
    leading comma), iterate `slice[1:]` with comma-prepend — lifts the per-iter
    `if i > 0` out of the loop.
17. **`bytes.IndexByte` string scan.** `scan.String` / `(*Stream).String`
    locate the closing `"` via `bytes.IndexByte` (SIMD), then a second
    IndexByte detects any preceding `\`. Wins on long strings; truncated
    `\u…`/trailing `\` falls through to `stringSlow` → `ErrBadString`.
18. **Empty-container peek bypass.** Slice/map decode peek for `]`/`}` before
    allocating — empty `[]`/`{}` keep the field nil, skip `make`.
19. **Adjacent-constant-append coalescing.** Post-render peephole over
    `renderAppendJSON` merges adjacent `dst = append(dst, ...)` lines whose args
    are all compile-time byte literals into one append (`,"key":` + `[` →
    `,"key":[`; trailing `]` + return `'}'` → `return append(dst, "]}"...),
nil`). Single-byte → `'X'`, multi-byte → `"…"...`. ~5% on struct-heavy
    Marshal.
20. **nil slice/map → JSON `null`** (accepted on decode). Stdlib v1/v2 parity:
    nil → `null`, empty non-nil → `[]`/`{}`. Decode accepts `null`, leaves nil.
    Fixed arrays don't accept `null`. JSONSize budgets the nil-as-null case
    directly — slice/map reserve 4 bytes (`null`) not 2; `sql.Null*` widens its
    inner constant to `max(inner, 4)`; arrays keep 2 (can't be nil). ~4% on
    Marshal but required for parity.
21. **Slab-allocated `[]*T` / `[N]*T` decode.** Slices: one backing slab
    `make([]T,0,cap)`, append element pointers as `&_slab[len-1]`. Arrays:
    `make([]T,N)` (heap, exact-sized — a stack `[N]T` would escape via
    `&_slab[i]`). N per-element heap allocs → ~log(N) (slice) / 1 (array). When
    a slice slab grows past cap, prior `*T` keep the orphan backing alive
    (~2× worst-case memory, no per-element alloc storm). Null elements skip the
    slab (nil pointer).
22. **`preallocCap` returns `(slice, slab int)`** — one switch over `f.ElemKind`
    decides both `make([]E,0,slice)` and `make([]T,0,slab)`. Defaults absent
    explicit hint: `[]*T` both `defaultPreallocCap` (slab slot is sizeof(T) —
    avoids orphan-trail growth); `[][]T`/`[]map` slice=default, slab=0 (bounded
    element slot); `[]T`/`[][N]T` both 0 (element could be huge — prealloc ×
    elemsize would explode heap); primitive slice=default clamped by maxlen,
    slab=0. Empty `[]` always emits `result.X = []T{}`; prealloc only in the
    non-empty arm.
23. **Stream key dispatch via `Stream.KeyView`.** Object-field keys read once,
    matched, discarded. The old `_s.String()` allocated a heap string per key
    (~200 throwaway allocs/value); `KeyView` aliases on the happy path (alias
    stays valid through buf growth — GC pins old backing). Falls back to
    `stringSlow` for escapes. Keys never escape the dispatch frame. See
    `scan/CLAUDE.md`.
24. **`peelSliceField` initializes `HintLen=-1`.** Nested-slice recursion used
    to inherit Go's zero `HintLen=0`, which `preallocCap` reads as "opt-out",
    so every nested row started cap=0 and walked the 1→2→4→8 chain. Now `-1`
    ("unset") falls through to kind defaults. Biggest alloc cut in the residency
    work — Matrix `[][]int` inner rows 494k → 274k allocs/1000 iters.
25. **Bitmask seen-flag tracking for wide structs.** Per-field `bool` locals
    for ≤32 fields (default threshold); above that `var _seen uint64`
    (or `[N]uint64` for >64) cuts the frame from N bytes to 8/⌈N/64⌉. Wins only
    on wide + recursive structs; below threshold, bools stay.
26. **In-place decode for every elem kind.** Slice/array elem decode writes
    directly into the final slot: `[N]*T` → `_slab[ivar]`; `[N]T` → `dst[ivar]`;
    `[]*T` → pre-grow `append(_slab, zero(T))`, target `_slab[len-1]`; `[]T` →
    pre-grow `append(dst, zero(T))`, target `dst[len-1]`. Structs: bytes path
    `var _n int; slot, _n, err = slot.DecodeFrom(data[k:]); k += _n`; stream
    path `slot, err = slot.DecodeStreamFrom(s)`. Primitives: slot is the assign
    target (`slot = _bv`, `slot = int(_n)`). No `var ev0`/`_z`/`_sv`, no
    post-decode `dst[ivar] = ev0`. `inlineScanInt64`/`Uint64` receive
    `target`+`castFn`. Pre-grow uses `zeroLit` (`""`/`false`/`0`/`T{}`).
27. **Position-var pass-through; no `kN := posVar` alias.** Slice/array decoders
    thread the caller's position var directly (top-level `j`, parent's `k`).
    Each inner advances the SAME counter; outer continues from it. Only data
    locals (`evN`, `_idxN`, `_slabN`) keep depth suffixes.
28. **Inline `null` peek; no `_np`/`_ok` locals.** 4-byte `null` check emitted
    byte-by-byte inline at the call site. Via `inlineNullPeek(posVar)` in
    `generate.go`.
29. **Single position cursor in dispatch loop.** No separate `j := i` cursor —
    every step (key scan, colon, value decode, comma/`}`) advances `i` directly.
    Stream path mirrors via `s.Pos`.
30. **Single local in `inlineScanString` (`_ke` only).** Start is `posIn+1`
    inline; slow-path fallback reads from unchanged `posIn`. Slice expr
    `data[posIn+1:]` len `_ke - posIn - 1`.
31. **Concrete-type fast paths in `AppendAny` for typed primitive slices/maps.**
    See `encode/CLAUDE.md` for ordering. 32-entry wins: `map[string]int`
    4403→1579 ns/op (71→7 allocs); `map[string]bool` 3449→944; `map[string]
float64` 6459→3417. Outpaces stdjson v1 and jsonv2 on every map shape.
32. **`AppendAny` concrete cases for `json.RawMessage`, `time.Time`,
    pointer-to-primitive.** Concrete cases pre-empt the `json.Marshaler` branch
    / `reflect.Pointer` fallback. Wins vs jsonv2, 32-byte shapes:
    `json.RawMessage` 227→28 ns/op (8.1×); `time.Time` 181→117; `*int` 70→26;
    `*bool` 83→19. Concrete cases MUST sit before the interface dispatches.

## Design decisions (the why)

1. \*\*`unsafe.String` boosts performance by avoiding GC pressure. Can backfire
   if parsed strings are referenced for a long time.
2. **Struct fields sorted alphabetically at codegen time** (default). Zero
   runtime cost; deterministic, compresses better.
3. **No runtime reflection anywhere.** Even the cross-package fallback uses
   `encoding/json.Unmarshal` only for types NOT in the generation pass.
4. **Custom validators / mods are codegen-time function injection.** Tags like
   `ggen:"@EvenOnly"` / `mod:"@Squash"` resolved via `packages.Load` at parse
   time — looked up, signature-validated against the field's exact go/types
   type, emitted as a direct call. No runtime registry, no `func(any) any`
   boxing, zero alloc. Cross-package via `@pkg.FuncName` through the source
   file's imports; blank imports (`_ "path/to/lib"`) work. Validator errors
   wrap as `validation.CustomError{Name, Cause}`; fallible mod errors
   propagate as parse errors.

## Test files (root module only)

Cover the CLI itself. Per-package tests live next to their implementation
(`encode/`, `scan/`); feature/roundtrip/compat/fuzz under `integrationtests/`;
benchmarks under `bench/`.

- `parse_test.go` — annotation/tag/rule parsing, cross-package symbol
  resolution. Hosts the test-only `generate(pkg, structs) ([]byte, error)`
  wrapper (production calls `generateTo` against the destination `*os.File`).
- `tags_test.go` — `json:`/`ggen:`/`mod:` tag parser, incl. dive/keys prefixes.
- `applicability_test.go` — rule-applicability matrix.
- `cli_test.go` — CLI integration: binary built in TestMain, file-naming
  contract, `./...` walk + dot/underscore-dir skip, per-flag effects on output.
- `bench_test.go` — `BenchmarkGenerate` over a representative fixture.
- `log_test.go` — Logger level + sink behaviour.

## How to regenerate

Build the binary into project directory (`./ggen`), never `/tmp`.
Binary is git-ignored; in-tree builds keep it discoverable, avoid
cross-session collisions, and match test harness path.

```sh
go build -o ggen .
./ggen .
(cd bench && GOEXPERIMENT=jsonv2 ../ggen .)
easyjson bench/types.go
(cd integrationtests && GOEXPERIMENT=jsonv2 go generate ./...)
```

ggen is module-scoped — `./...` from root visits ONLY root-module packages.
`bench/` and `integrationtests/` each carry their own `go.mod` and must be
regen'd from inside (one invocation per module), like `go build ./...`. In
`integrationtests/`, each annotated source carries `//go:generate ../ggen $GOFILE`
and emits a sibling `<file>_ggen_test.go`.

## Working with CLAUDE.md

**ALWAYS** keep this file up-to-date after changes to: CLI/annotation flags,
codegen behaviour, wire format, generated method surface, etc.

Benchmark numbers → `bench/CLAUDE.md`. Test-suite layout →
`integrationtests/CLAUDE.md`. Per-package runtime details → the matching
package CLAUDE.md. Backlog / tried-and-rejected → `.claude/backlog.md`.

### Sibling docs that MUST also be kept current

Every change touching a user-visible surface must propagate to **both**
`README.md` and `SKILL.md`:

- **CLI flag changes**
- **Annotation changes**
- **Field tag syntax change**
- **New Go kind / wire-shape change**
- **New runtime API**
- And so on...

Bundle the doc update in the same commit.

CLAUDE.md is the implementation detail doc (the _why_).
README and SKILL are the surface (_what_/_how_).
All three move together.

## README.md authoring rules

**NEVER spill technical / implementation details into README.md.** README is
the user-facing front door: what ggen is, what it does, how to use it, what the
numbers mean. CLAUDE.md is where implementation detail lives.

Do NOT add to README: runtime/harness mechanism (`runtime.GC()` cycles,
`HeapInuse`, `b.RunParallel`, sink merge, `b.ResetTimer`, …); internal codegen
detail (`unsafe.String` aliasing, slab heuristics, `KeyView` vs `String`,
`preallocCap` shape, `peelSliceField`, …); pprof internals.

DO put in README: what each benchmark measures (one sentence); how to read each
metric; when the user would care; the bench table + interpretive paragraph;
caveats that affect the user's choice (e.g. "strings alias the input, don't
mutate after decode").

If you write "internally", "implementation", "under the hood", or name a
private function / runtime API in README — stop. That belongs in CLAUDE.md or a
code comment.

## Backlog

See @.claude/backlog.md
