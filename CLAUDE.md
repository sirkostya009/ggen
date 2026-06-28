# ggen — zero-copy, zero-reflection JSON codegen for Go

Code generator. Parses annotated Go structs, emits methods on them. Hand-rolls a
byte scan over the caller's `[]byte` or `*scan.Stream`; the bytes path aliases
input via `unsafe.String` — no copy, no tokens, no AST. This file documents the
**CLI / codegen surface** and the *why* behind generated-code shape. Runtime
internals, benchmarks, and integration-test conventions live in each package's
own CLAUDE.md (see "Repo layout"). This is NOT the user-facing doc — that is
`README.md` / `SKILL.md`.

## Repo layout

```
schema/
├── go.work                                             ← workspace tying all four modules together
├── cli/                                                ← CLI module (github.com/sirkostya009/ggen/cli)
│   ├── main.go, parse.go, generate.go, tags.go, types.go   ← CLI (package main); tags.go = json tag only
│   ├── pipe.go                                             ← pipe:/hint: grammar (tokenize, ParsedPipe, Step/Variant, deriveBuckets)
│   ├── variants.go                                         ← multi-shape decode dispatch codegen (/ variants)
│   ├── introspect.go                                       ← go/types interface detection (TextAppender, TextMarshaler, …)
│   ├── alias.go                                            ← alias-type code emitters (decode + AppendJSON)
│   ├── applicability.go                                    ← parse-time rule/kind compatibility matrix
│   ├── customfunc.go                                       ← @Func resolution + signature classification (validator/mod/converter)
│   ├── check.go                                            ← `-dry` / future-ggenvet parse-only entry points
│   ├── log.go                                              ← cliLog: leveled logger with deferred flush
│   ├── parse_test.go, tags_test.go, cli_test.go, …         ← CLI tests
│   └── bench_test.go                                       ← BenchmarkGenerate (generator perf only)
├── decode/             → see decode/CLAUDE.md          ┐
├── decode/validation/  → see decode/validation/CLAUDE.md │ runtime library
├── encode/             → see encode/CLAUDE.md          │ (root module
├── scan/               → see scan/CLAUDE.md            ┘  github.com/sirkostya009/ggen)
├── integrationtests/   → see integrationtests/CLAUDE.md (own Go module)
├── bench/              → see bench/CLAUDE.md            (own Go module)
└── .claude/backlog.md  ← ideas worth pursuing, tried-and-rejected, maybe-someday
```

Four modules under one `go.work`: root (`github.com/sirkostya009/ggen` — runtime
library `decode`/`encode`/`scan` only, no external deps), `cli/` (the generator,
depends on `golang.org/x/tools`), `bench/`, `integrationtests/`. The CLI doesn't
import the runtime packages — it emits their import paths as string literals into
generated code.

## Generator CLI (`main` package)

### Invocation

```
ggen ./...                    every package matched by the pattern (module-scoped, as `go build`)
ggen <dir>                    one package
ggen <file.go> [Names...]     one file; optional struct name filter
```

Packages load via `golang.org/x/tools/go/packages` with full type info; interface
impls (TextMarshaler, ByteDecoder, JSONMarshaler, …) are picked up and emitted as
direct method calls — no runtime probing. If type info can't resolve (temp file,
no `go.mod`), falls back to AST-only mode and emits a plain `encoding/json`
fallback for cross-package types.

Run `ggen` with the same `GOEXPERIMENT` env as user code — files behind
`goexperiment.jsonv2` are otherwise invisible.

Pattern mode (`./...`, `./sub/...`, `...`) resolves via `packages.Load` —
module-scoped, workspace-aware, never crosses module bounds. A subdir with its
own `go.mod` is skipped (multi-module repos run ggen once per module). Test-only
packages (no non-`_test.go` files) are skipped in pattern mode, picked up in
single-package mode. Processing is post-order over the matched import subgraph
(deps first), sequential in topo order. Dot/underscore-prefix dirs, `vendor/`,
`testdata/`, `node_modules/` are skipped by `go list`.

### Flags (all opt-in, apply to every struct in the pass)

| Flag | Effect |
| --- | --- |
| `-o <path>` | override output path (single file / single dir only) |
| `-pkg <name>` | override package name in output |
| `-marshal` | emit `MarshalJSON` method |
| `-unmarshal` | emit `UnmarshalJSON` method |
| `-multierr` | accumulate validation failures into `validation.Errors`, returned at end of parse; parse errors still return immediately |
| `-allowdups` | allow duplicate keys, first-wins (later skipped). Default: `validation.DuplicateKeyError` |
| `-novalidate` | skip validation rules, required-field checks, mods |
| `-ignoreunknown` | silently skip unknown JSON keys. Default: `validation.UnknownKeyError`. Overridden when an inline map field is present |
| `-nullzero` | accept explicit JSON `null` on every non-pointer value field → Go zero. Default hard-errors (see null kind-gating). No-op on already-null-aware kinds |
| `-nosortkeys` | emit fields in Go declaration order. Default: alphabetical. Inline map fields stay last |
| `-usenumber` | decode JSON numbers into `any` fields as `json.Number` instead of `float64` (mirrors stdlib `UseNumber()`) |
| `-htmlescape` | opt INTO HTML-safe escaping (`<`, `>`, `&` → `\uXXXX`) on marshal. Default = literal |
| `-dry` | parse + validate annotated structs, surface every error, emit no file. Composes with `-v`. Rejects `-o`/`-pkg` |

### Per-struct annotations

A comment on a struct (or gen-decl) `//ggen:generate` (no space after `//`,
mirrors `//go:generate`) followed by space-separated tokens. Apply only to the
annotated struct:

`marshal`, `unmarshal`, `multierr`, `allowdups`, `novalidate`, `ignoreunknown`,
`nullzero`, `nosortkeys`, `usenumber`, `htmlescape`.

## Struct tags (on fields)

Field config is partitioned by role across three tags: `json:` (wire shape),
`pipe:` (decode→transform→validate pipeline), `hint:` (prealloc).

### `json:`

- `json:"name"` — JSON key name (field is ignored otherwise)
- `json:"-"` — field explicitly ignored
- `json:",inline"` — catch-all map for unknown keys. Type must be `map[string]V`
  (string-keyed); V may be `any`, a primitive, a ggen-annotated struct, or any
  other type (typed elems use the elem's fast path when available, else
  `encoding/json.Unmarshal` over the captured span). Overrides `ignoreunknown`.
  Entries spliced out on marshal
- `json:"name,omitempty"` — not marshaled when JSON-empty (null, "", [], {})
- `json:"name,omitzero"` — not marshaled when Go-zero
- `json:"name,string"` — wrap primitive as JSON string on marshal, unwrap on
  unmarshal. Primitives only, like stdlib
- `json:"name,format:X"` — format hint for native types (see Kinds). **jsonv2
  requirement: `format:X` must be LAST in the tag.**

### `pipe:` — decode/transform/validate pipeline

One ordered, whitespace-separated step list parsed in `pipe.go` into a
`ParsedPipe` (Presence / Variants / Outer / Keys / Levels). Grammar:

```
pipe        := stage ( "~" stage )*
first stage := variant ( "/" variant )*    // decode: JSON-shape dispatch
later stage := step ( WS step )*            // value steps, inner:/keys: levels
```

- **Presence** (lifted, position-independent): `required` → object-close-seen
  check (`RequiredError`, via `IsRequired()` reading `FieldInfo.Presence`);
  `optional` is a marker. Absent key → Go zero.
- **Decode stage** — `/`-separated variants, one per JSON shape; ggen peeks the
  first byte and routes (`variants.go`). `~` is optional sugar: with no `~` the
  decode stage is the leading run of variant keywords (`leadingDecodeExtent`).
  Variants:
    - `.` — native decode of the field type T.
    - `nullzero` — JSON `null` → `zero(T)` (sets `FieldInfo.NullZero`). Bare
      `nullzero` needs no `.`.
    - `@Conv` — converter `func(W)T` / `func(W)(T,error)` / `func(W)(T,bool)`,
      OUTPUT-anchored (result == T). ggen scans input `W` (primitive or
      ggen-decodable struct → delegates to its `DecodeFrom`) and converts. Same
      emit on bytes + stream; encode is untouched (marshals native T). A lone
      leading `@Func` is a value step, NOT a converter — needs `/`, a leading
      `.`, or `~`. Variants must claim disjoint shapes (`checkVariantShapes`).
- **Value steps** (after the decode stage): mods + validators interleaved **in
  declared order** — a unified ordered `[]Step` per level, emitted by
  `renderPipe` (dispatching each to `renderOneVal`/`renderOneMod`).
    - `inner:` scopes one container level down, `keys:` to map keys. A bare prefix
      takes ONE step (`inner:trim`); parentheses group several
      (`inner:(trim maxlen=20)`); groups nest for deeper levels
      (`inner:(a inner:(b))`). Parsed recursively (`parseScope`/`parsePrefixEntry`/
      `matchParen`). Levels carried as `FieldInfo.Levels [][]Step` (`Levels[0]` =
      per-elem), peeled by `peelSliceField`, emitted via `elemSteps`.
    - validators: `notempty`; `len/minlen/maxlen=N`; `runes/minrunes/maxrunes=N`;
      `gt/gte/lt/lte/eq/neq=N`; `multiple=N`; `oneof=a|b|c`; `email`/`url`/`ascii`/
      `printable`/`alphanum`/`numeric`/`lower`/`upper`/`hexadecimal`;
      `starts/ends/contains=X`.
    - mods: `trim`, `lower`, `upper`, `trimleft=X`, `trimright=X`,
      `replace=old|new`, `clamp=lo|hi`.
- **Custom funcs** (`@FuncName` / `@pkg.FuncName`) — classified by signature in
  `customfunc.go` (`classifyValueFunc` for value steps, `classifyConverter` for
  variants), type-checked against the working type at that level:
  `func(T)error`→validator (`CustomError`), `func(T)bool`→validator
  (`PredicateError`, message-capable), `func(T)T`→pure mod, `func(T)(T,error)`→
  fallible mod (parse error), `func(T)(T,bool)`→fallible mod (`ModError`,
  message-capable). `func(bool)bool` is rejected. Bool forms carry an inline
  message `@Even:'must be even'`. Cross-package via source-file imports; blank
  imports work.

**Lexing/quoting** (`tokenizePipe`): steps are WS-separated; structural glyphs
`/ ~ ( )` are significant with or without spaces (plus the `inner:`/`keys:` word
prefixes); a value/message may be single-quoted, required only when it contains
whitespace; `\'` is a literal quote.

### `hint:` — prealloc capacity only

`hint:"N"` → `make([]T,0,N)`; per-level via `inner:` (`hint:"32 inner:8"`).
Lifted, order-independent (`FieldInfo.HintLen` / `HintLevels`). `hint:"0"` opts
out; negative is a parse error.

### Internal model

`FieldInfo` keeps legacy split buckets (`Validation`/`Mods`/`Elem*`/`Inner*`/
`Key*`) as the source for order-independent consumers (import-collection walks,
`peelSliceField`, the pointer-leaf partition) — they are DERIVED from the ordered
`Pipe`/`KeyPipe`/`Levels` by `deriveBuckets`. The ordered step lists are the
source of truth for emit ORDER at value-stage sites; `fieldPipe`/`elemSteps` fall
back to `stepsFromLegacy(mods, vals)` for synthetic fields that set only buckets.

### Rule applicability (parse-time)

`applicability.go` rejects mismatched rules against the working type (clear
message); per-level gating (elem kind under `inner:`, `string` under `keys:`).
Cases covered in `TestCLI/InvalidRuleApplication`.

## Generated methods (per annotated struct T)

```go
// DecodeFrom is a zero-copy parser. Strings and RawMessage are aliased into data
func (result T) DecodeFrom(data []byte) (T, int, error)
// DecodeFromStream is a buffered io.Reader wrapper with an intermediate buffer.
// Useful for slow streams or lower memory usage. Breaks zero-copying — all strings
// and json.RawMessage are copied from payload.
func (result T) DecodeFromStream(s *scan.Stream) (T, error)
// JSONSize precalculates size of JSON payload of T in bytes
func (s T) JSONSize() int
// AppendJSON appends a payload string to dst. Errors on invalid numbers (like NaN)
func (s T) AppendJSON(dst []byte) ([]byte, error)
```

**Cursor convention.** Bytes-path `DecodeFrom` takes a slice starting at the
value's first byte and returns bytes consumed; caller advances its own cursor
(`i += n` after reslicing `data[i:]`). Stream-path `DecodeFromStream` takes/returns
no cursor — the cursor is `s.Pos`, owned by the Stream and advanced in-place by
every scan primitive. To capture a raw span: `start := s.Pos; s.SkipValue(); raw
:= s.Bytes()[start:s.Pos]`.

**Decode-into-receiver semantics.** The receiver passed in IS the merge source.
Scalar fields persist across JSON omission (stdlib-merge shape); container fields
reset at the top of DecodeFrom so the decoder never appends over carried-in data
(unconditional — blank payload → blank slate, capacity kept):

- slices and `[]byte`: `if X != nil { X = X[:0] }` at entry (backing reused;
  `make(...)` only when nil). For slice-of-slice (`[][]T`, any depth) the inner
  row backings are reused too: the outer grows by reslicing within cap (keeping
  the carried inner header) and each row is seeded `rowN := slot; if rowN != nil
  { rowN = rowN[:0] }`; a past-cap/fresh slot reads back nil and allocates anew
  (opt #43; pinned by `TestMerge_nestedSliceBackingReused`)
- `map[K]V`: `if X != nil { clear(X) }` at entry (buckets reused; `make` only when nil)
- nested struct: `result.X, _, _ = result.X.DecodeFrom(...)` — value-receiver
  takes the existing value as merge source
- pointer `*T` / `**T` / … (any depth): **parse-first** cascade. `null` →
  `result.X = nil` (drops a carried-in chain, stdlib parity). Otherwise the leaf
  is decoded into a stack temp FIRST — a parse failure returns before any
  mutation, so no chain is allocated for a value that never landed. On success an
  assign cascade reuses the non-nil prefix of the receiver's chain and allocates
  `new(new(…v))` only from the first nil level down. A mergeable leaf
  (struct/slice/map/array) is seeded from the carried-in value first so it still
  merges; primitive leaves skip the seed. Widened numeric leaves scan into a wide
  temp and cast at the assign site. The leaf decodes natively at every depth — NO
  encoding/json fallback. Same emit on bytes + stream paths
- fixed arrays `[N]T`: every slot overwritten or strict-length-errors; no entry reset

JSON `null` for slice/map sets `result.X = nil` (stdlib v1/v2 parity). JSON
`[]`/`{}` on a non-nil receiver keeps the `[:0]`'d / cleared container; on a nil
receiver allocates an empty non-nil container.

**`null` acceptance is kind-gated (diverges from stdlib).** ggen emits a 4-byte
`null` peek only for: pointer (`*T`), slice (KindSlice), map (KindMap), `[]byte`
(KindBytes — null ↔ nil, nil marshals as `null`), `sql.Null*`, and raw-message
(`json.RawMessage`/`jsontext.Value`) fields. Every other kind — non-pointer
scalars, `time.Time`, `time.Duration`, `net.IP`/`netip.*`, `url.URL`, `big.*`,
UUID, and other text/number kinds — has NO null branch, so an explicit JSON
`null` hard-errors the parse. stdlib v1/v2 instead accept `null` everywhere.
Consistent with ggen's other strict defaults (UnknownKeyError, strict array
length, DuplicateKeyError, trailing-comma rejection) — for a nullable scalar, use
a pointer. Pinned in `integrationtests/stdcompat_test.go`
(`TestStdCompatMerge_IntentionalDivergences`).

**`nullzero` opts a value field into null-as-zero.** A `nullzero` decode variant
in `pipe:` (per field) / `-nullzero` / `//ggen:generate nullzero` (whole struct)
makes a non-pointer value field accept explicit JSON `null`, decoding it to the Go
zero value — the middle ground between strict-reject default and stdlib's
accept-everywhere. Gated by `nullZeroApplies` (set + `AtDispatch` + a kind that
would otherwise reject null; already-null-aware kinds stay no-ops). Emit mirrors
the pointer/slice null branch (opt #34): a 4-byte `null` peek sets `ref =
<zeroLit>` then `break`s out of the dispatch case when no field rules follow
(`nullBreakOK`), else nests the value decode in an `else` so the shared
`validateAndMod` runs on either the decoded value or the zero (so `nullzero` +
`minlen=1` on a string still rejects `null`→`""`). Per-field tag ORs onto the
struct/CLI flag in `applyCLIFlags`. Struct fields only — not top-level aliases.
Decode-only. Pinned in `integrationtests/nullzero_test.go` + `cli_test.go`.

**Trailing commas are rejected (stdlib parity).** Every element-loop comma branch
(slice/map/tuple/nested/pointer-elem/`[]byte` format:array, bytes + stream) emits
a guard after the comma's WS skip: container close or EOF right after a comma →
`scan.ErrBadArray`/`ErrBadObject` (`emitNoCloseAfterComma` /
`streamNoCloseAfterComma`). The EOF half also guards a stream-path index of
`s.Bytes()[s.Pos]` on input truncated right after a comma (`SkipSpace` returns nil
at EOF with `Pos == len(buf)`). Pinned in `scan_decode_test.go`.

**Decode-into-receiver vs stdlib merge — divergences.** NOT a drop-in: (1)
container fields reset at decode entry, so an OMITTED slice/map key is emptied
(stdlib retains it); (2) a present map key REPLACES (clear+refill) rather than
merging entries (stdlib retains receiver-only keys); (3) scalar `null` errors
(above). Scalars-persist-on-omit, slice-replace-on-present, null→nil for
slice/map/pointer, nested-struct merge, and `*T`/`**T` reuse all MATCH stdlib —
pinned in `TestStdCompatMerge_Parity`.

Call with a zero-value receiver for a fresh decode (`T{}.DecodeFrom(data)` for
struct/slice/map/array; `var zero T; zero.DecodeFrom(data)` for primitive
aliases). To merge into an existing value, call its `DecodeFrom` directly.

Runtime entry points (call from user code):

```go
// bytes path — single value
T{}.DecodeFrom(data)                       // (T, int, error)

// stream path — single value
var s scan.Stream
s.Reset(r, buf)
T{}.DecodeFromStream(&s)                   // (T, error); recycle s.Bytes()

// array walkers
decode.UnmarshalSlice[T](data)             // ([]T, error)
decode.ReadSlice[T](r)                     // ([]T, error)
decode.UnmarshalSliceStream[T](r, buf)     // ([]T, []byte, error)

// encode package
encode.Marshal(t)            encode.MarshalString(t)          encode.WriteTo(w, t)
encode.MarshalSlice(items)   encode.MarshalSliceString(items) encode.WriteSliceTo(w, items)
encode.AppendSlice(dst, items)
```

Opt-in (`//ggen:generate marshal` / `unmarshal`):

```go
func (s T) MarshalJSON() ([]byte, error)     // wraps encode.Marshal(s)
func (s *T) UnmarshalJSON(data []byte) error // inlines var zero T; zero.DecodeFrom(data)
```

## Top-level type aliases

Annotated named types (`//ggen:generate type T <underlying>`) get the same method
surface as a struct, driven by `renderAlias*` helpers in `alias.go`. Top-level
renderers dispatch to alias paths when `s.IsAlias` is set (except struct aliases
that fall back to field introspection, which set `IsAlias=false` and route through
regular struct codegen).

Accepted underlying kinds:

- **primitive** (`string`, `bool`, `int*`, `uint*`, `float*`): scan via `scan.X` /
  `_s.X`, cast to alias. `htmlescape` flips the string-append helper
- **struct** (`type LocalUUID uuid.UUID`): methods don't propagate from the RHS,
  so probing uses `inspectType` on the RHS named type. Three-step ladder:
    1. *ggen-method delegation* — if underlying has AppendJSON+DecodeFrom: cast →
       method → cast back (cheapest)
    2. *field introspection* — plain struct with ≥1 exported field: walk
       `*types.Struct`, synthesize FieldInfo per exported field
       (`extractFieldFromTypes`), `IsAlias` flips false, regular struct codegen
       runs (field access via `result.X` is sound — identical layout). **Preferred
       over JSON/Text marshaler delegation even when those exist** — hand-rolled
       codegen beats reflective marshaler calls
    3. *JSON/Text marshaler delegation* — opaque struct (no exported fields, e.g.
       `time.Time`) with a JSON or Text marshaler pair: cast → method → cast back

    Wire-shape implication: an alias of a struct with both exported fields AND a
    custom MarshalJSON uses the introspected field shape, NOT the underlying's
    MarshalJSON. For the underlying's exact shape, declare with no exported fields
    (forces delegation) or write your own marshal hook.
- **slice / map / array** (`type Tags []string`, `type Lookup map[string]int`,
  `type Tuple [3]int`): synthetic FieldInfo handed to field-level emitters with
  `result` (decode) / `s` (encode) as ref. All field-level features carry over.
- **`[]byte` alias**: collapses to KindBytes, base64 path.

Rejected: channel, interface, function — no sensible JSON shape.

`htmlescape`/`marshal`/`unmarshal` apply to all aliases; `allowdups`,
`ignoreunknown`, `multierr`, `novalidate` apply to struct aliases. Foreign-package
imports collected by `aliasUnderlyingImports`; field-introspection types render
via `types.RelativeTo(s.typesPkg)`.

## Supported Go kinds (per field)

- `string`, `bool`
- `int`/`int8`/`int16`/`int32`/`int64`, `uint`/`uint8`/`uint16`/`uint32`/`uint64`
- `float32`, `float64`
- Pointer to any of above (`*T`) — null ↔ nil. Multi-level (`**T`, …) also native:
  decode parses the leaf first then builds/reuses the chain, encode derefs
  level-by-level (intermediate nil → `null`). No reflective fallback
- `[]T` (slice), `map[string]V` (string-keyed only; pointer values native, any depth)
- `[]*T` / `[N]*T` (slice/array of pointer-to-struct) — element pointers come from
  a single backing slab so N allocs collapse to ~log(N). Nil elements → nil
  pointers; encode nil → `null`. Multi-level elements (`[]**T`, nested `[][]**T`)
  skip the slab and run the scalar pointer cascade per element; pointer map values
  (`map[string]*V`/`**V`/…) decode the same way — no `encoding/json` fallback at
  any depth
- Nested struct (generate-time probing of `DecodeFrom`/`UnmarshalJSON`/
  `UnmarshalText` and `AppendJSON`/`MarshalJSON`/`AppendText`/`MarshalText`; with
  default-stdlib fallback)
- Embedded struct (unnamed field) — fields promoted to parent's JSON object
- `time.Time` — `format:unix`/`unixmilli`/`unixmicro`/`unixnano`/`RFC3339`/
  `RFC3339Nano` + custom (jsonv2 supported) + other `time.X` constants
- `time.Duration` — `format:sec`/`milli`/`micro`/`nano`/`units` (default, parses `"1h30m"`)
- `net.IP`, `netip.Addr`, `netip.Prefix` — text form. Marshal via
  `encoding.TextAppender`, decode via `net.ParseIP`/`netip.ParseAddr`/`netip.ParsePrefix`
- `[]byte` — `format:base64` (default)/`base64url`/`base32`/`base32hex`/
  `base16`(`hex`)/`array` (JSON array of numbers). `null` ↔ `nil`: decode accepts
  `null` → nil, nil marshals as `null` (empty non-nil → `""`/`[]`); no
  opening-quote fold
- `json.RawMessage` / `jsontext.Value` — opaque span via `scan.SkipValue`, aliased
  into field. Raw passthrough on encode (`null` if empty/nil)
- `net/url.URL` — JSON string, `url.Parse` / `encode.AppendURL`
- `math/big.Int`/`big.Float`/`big.Rat` — `big.Int` a JSON number, `big.Float`/
  `big.Rat` JSON strings (`"3.14"`/`"3/2"`; wrapping prevents float64 precision
  loss, matches jsonv2). Encoded via in-place `Append` (zero alloc), parsed via
  `SetString`/`Parse`
- Other types — no dedicated kind. Any type implementing `encoding.TextAppender`/
  `TextMarshaler`/`TextUnmarshaler` routes through those methods. Marshal prefers
  `AppendText(dst)` (zero alloc), falls back to `MarshalText() + AppendString` (one
  alloc). A declared AppendJSON method takes highest precedence
- `database/sql.Null*` (`NullString`, `NullInt64`, `NullInt32`, `NullInt16`,
  `NullByte`, `NullBool`, `NullFloat64`, `NullTime`) **and the generic
  `sql.Null[T]`** (Go 1.22; inner field is always `V`). Decode probes `null` first
  → `Valid=false`, else reads the inner value and sets `Valid=true`. Encode `null`
  when `!Valid`, inner value otherwise — wire shape is always inner-or-null, never
  the `{"V":…,"Valid":…}` struct dump. Named flavors use the string-keyed
  `SQLNullSpec`. **Generic `sql.Null[T]` supports any inner `T` ggen can render as
  a field**: with go/types info the parser builds a synthetic `FieldInfo` for `T`
  (via `extractFieldFromTypes`) stashed on `FieldInfo.SQLNullInner`; the
  decode/encode/size renderers delegate the `V` slot to the standard field
  emitters, so `sql.Null[T]` gets exactly the wire (and fast path) of a bare `T`.
  Parent flags (`MultiErr`/`NoValidate`/`UseNumber`/`HTMLEscape`) copy onto the
  inner. The AST-only loader (no go/types) keeps only built-in-primitive generic
  forms (`SQLNullSpec` + `isSupportedSQLNullInner` gate); custom inners there fall
  back to `encoding/json` on the whole value
- `any` / `interface{}` — decode via `scan.Any` / `(*Stream).Any`, stdlib
  defaults: `null→nil`, `bool`, `number→float64`, `string` (zero-copy alias),
  `array→[]any`, `object→map[string]any`. With `usenumber`, `scan.AnyNumber`
  (numbers → `json.Number`). Encode via `encode.AppendAny` (type-switch ordering —
  see `encode/CLAUDE.md`)
- `[N]T` (fixed-length array) — JSON tuple with **strict count**: decode errors
  with `validation.LenError{Want:N}` when count ≠ N. Combines/nests freely
  (`[][N]T`, `[N][]T`, `[N][M]T`, …) via the same recursive emitter as `[][]T`.
  `[]byte` stays KindBytes (base64) — only non-byte arrays get tuple treatment

## Wire-format divergences from stdlib

Two kinds intentionally diverge from `encoding/json` v1 + v2. ggen marshal output
is *not* a subset of either for these — feeding through stdlib reshapes the value,
and decoding stdlib JSON won't work for these fields. Round-trip within ggen is
fine.

| Kind          | ggen wire             | stdlib wire (v1 + v2)                                    |
| ------------- | --------------------- | -------------------------------------------------------- |
| `net/url.URL` | `"https://x/p?q=1"`   | `{"Scheme":"https","Host":"x","Path":"/p", … 11 fields}` |
| `sql.Null*`   | inner value or `null` | `{"<Inner>":val,"Valid":true}` (plain struct, no hook)   |

## Output file naming + build tags

- Package mode, untagged: `<dir>_ggen.go` (non-test) / `<dir>_ggen_test.go`
  (test-only); both if both exist
- Package mode, tagged: `<dir>_<tag-slug>_ggen.go` per (tag, isTest) bucket
- Single-file: `<basename>_ggen.go` / `_ggen_test.go`; source `//go:build` line
  preserved in header
- `_test.go` sources are first-class inputs; test-only struct annotations route to
  `_ggen_test.go`

**Build tag propagation.** The generator reads `//go:build <expr>` per source file
and buckets annotated structs by constraint. Each (tag, isTest) bucket emits its
own gen file with a matching header — a struct in `tagged.go` (behind
`//go:build foo`) never lands in the unconstrained `<dir>_ggen.go`. Old-style
`// +build` honored; multi-term exprs canonicalized via
`go/build/constraint.Parse`. Cross-bucket struct refs in the same package still
route through direct DecodeFrom (`generatedTypes` seeded with the union of all
buckets first). Tagged-bucket slugs collapse non-alnum runs to single underscores
(`goexperiment.jsonv2` → `goexperiment_jsonv2`, `foo && bar` → `foo_bar`).

## Codegen optimizations (nothing at runtime)

Backlog and commit messages cite these by number — numbering is stable.

1. **Flat `switch key` dispatch.** One string switch over all JSON names — gc
   lowers it to length-grouped binary search / jump tables. (Manual length-first
   outer switch removed — see backlog.)
2. **Slice cap from tag hint.** `preallocCap` picks initial cap for
   `make([]T,0,N)`. Precedence: `hintlen=N` > `len=N` > `max(minlen, default)` >
   default (`defaultPreallocCap = 4`). Maps via `mapPreallocCap` (no minlen).
3. **Field marshal order sorted by JSON name** (alphabetical) at codegen time.
   `-nosortkeys` opts back to declaration order.
4. **Inlined scan primitives in hot path.** Raw byte-compare loops for
   `SkipSpace`, `String`, `Int64`, `Uint64` emitted into each case body — no
   call overhead.
5. **Mod + validation after field read.** `renderMods` → `renderValidationOn`
   write into the parent buffer; `posVar` emits the right return shape inline.
6. **Pointer fields** emit a 4-byte `null` peek → nil branch, else stack-local
   `var v <Pointee>` + recursive inner read + `&v`. Dispatch order in
   `renderField` = pointer-first → string-tag → kind switch (pointer-first
   recurses with `inner.GoType = PointeeType`).
7. **Cross-package struct fallback (statically dispatched).** Method-set
   membership checked via `go/types` at codegen time; emits a single hardcoded
   call. Decode order: `DecodeFrom` → `UnmarshalJSON` → `UnmarshalText` →
   `encoding/json`. Marshal mirror: `AppendJSON` → `MarshalJSON` → `AppendText` →
   `MarshalText` → `encoding/json`. No type info → plain `encoding/json` fallback.
8. **Inline map catch-all.** Unknown keys absorbed into `map[string]V`. V
   dispatches: `any` → `scan.Any`/`s.Any`; `string` → `scan.String`/`s.String`;
   ggen struct → its `DecodeFrom`/`DecodeFromStream`; else `scan.SkipValue` +
   `json.Unmarshal` over the captured span.
9. **Marshal output cap.** `JSONSize()` upper bound → single `make([]byte,0,cap)`
   + `AppendJSON`. 1 alloc per top-level Marshal.
10. **Recursive nested-container emitter.** `emitByteSliceRead`/
    `emitStreamSliceRead`/`emitAppendSlice`/`emitSizeSlice` take a depth param and
    unify slice+array. When `ElemKind` = KindSlice/KindArray they recurse via
    `peelSliceField(f)` + `stripOneContainer(typ)` (strips one `[]`/`[N]`, shifts
    inner validation down a level). Arrays carry N via `ElemArrayLen` for
    strict-count at every level. All locals carry a depth suffix.
11. **Map-key mods + validation.** `keyValidateAndMod` runs right after the key
    read (before `:`), short-circuiting invalid keys before the value decodes.
12. **Marshal error propagation.** `AppendJSON` returns `([]byte, error)` threaded
    through every nested call. Pure-primitive structs declare `var err error; _ =
    err` (elided by the compiler).
13. **Typed validation errors + frozen OneOf slices.** Each rule has its own
    pointer-receiver error struct; the generator emits a typed literal at the
    failure site. `OneOfError.Allowed` points to a deduped package-level frozen
    `[]string` emitted once per unique allowed-set. `EqError`/`NeqError` use `Want
    any`. Every error carries a `Pos int` (byte offset relative to the full
    payload — bytes cursor `i`, or `s.Offset()` on the stream path, NOT the raw
    `s.Pos` which the compacting window invalidates). Injected by `withPos`/
    `posLit`. See `decode/validation/CLAUDE.md`.
14. **Parse-error wrapping at every error return.** Codegen embeds the JSON field
    name as a literal: `return result, i, decode.NewParseErr("street", i, err)`.
    The field literal comes from `/*ggen-field:EXPR*/` markers emitted by the
    dispatch; a textual post-pass (`wrapErrReturns`/`rewriteErrReturnsBody` in
    `parseerr_postpass.go`) tracks the active marker, wraps every non-nil error
    return, and strips markers. Zero runtime cost on the happy path.
    `NewParseErr` builds `*decode.ParseError{Field, Pos, Err}` for raw sentinels,
    passes `validation.Error`/`Errors` through untouched, and **chains** when err
    is already a `*ParseError` — prepending the outer field so nested surfaces
    read `addr.street`. `errors.Is(err, scan.ErrBadString)` works via `Unwrap()`;
    `ParseError.Error()` calls `e.Err.Error()` once so chained prints stay linear.
15. **Constant-folded `JSONSize()`.** Each field size splits into a compile-time
    constant (folded into `size := N`) and a runtime expression. Pure-primitive
    structs collapse to `return N`.
16. **Opening-quote folding.** At struct-field top level, when a value emit begins
    with `"` (string, URL, big.Rat, time/RFC3339, duration/units, base64/hex
    bytes, net.IP/netip), the opening quote folds into the constant `"key":` →
    `"key":"`; the value emitter writes only body + closing `"`.
17. **First-element-then-rest slice loop.** First element emitted directly (no
    leading comma); iterate `slice[1:]` with comma-prepend — lifts the per-iter
    `if i > 0` out of the loop.
18. **`bytes.IndexByte` string scan.** `scan.String`/`(*Stream).String`/`KeyView`/
    `skipString` locate the closing `"` via `bytes.IndexByte` (SIMD), then a
    second IndexByte (bounded to the closing quote) detects a preceding `\`.
    Truncated `\u…`/trailing `\` falls through to `stringSlow` → `ErrBadString`.
19. **Empty-container peek bypass.** Slice/map decode peek for `]`/`}` before
    allocating — empty `[]`/`{}` keep the field nil, skip `make`.
20. **Adjacent-constant-append coalescing.** A post-render peephole over
    `renderAppendJSON` merges adjacent `dst = append(dst, ...)` lines whose args
    are all compile-time byte literals into one append (single-byte → `'X'`,
    multi-byte → `"…"...`).
21. **nil slice/map → JSON `null`** (accepted on decode). Stdlib parity: nil →
    `null`, empty non-nil → `[]`/`{}`. Fixed arrays don't accept `null`. JSONSize
    budgets the nil-as-null case (slice/map reserve 4 bytes; `sql.Null*` widens
    its inner constant to `max(inner, 4)`; arrays keep 2).
22. **Slab-allocated `[]*T` / `[N]*T` decode (depth-1 only).** One backing slab
    (`make([]T,0,cap)` slice / `make([]T,N)` array — heap, exact-sized, since a
    stack `[N]T` would escape via `&_slab[i]`); element pointers are `&_slab[…]`.
    N per-element heap allocs → ~log(N) (slice) / 1 (array). Past-cap slab growth
    orphans prior backing (no per-element alloc storm). Null elements skip the
    slab. Multi-level elements route through the per-element cascade instead.
23. **`preallocCap` returns `(slice, slab int)`** — one switch over `f.ElemKind`
    decides both makes. Defaults: `[]*T` both `defaultPreallocCap`; `[][]T`/
    `[]map` slice=default, slab=0; `[]T`/`[][N]T` both 0 (element could be huge);
    primitive slice=default clamped by maxlen, slab=0. Empty `[]` always emits
    `result.X = []T{}`; prealloc only in the non-empty arm.
24. **Stream key dispatch via `Stream.KeyView`.** Object-field keys read once,
    matched, discarded. `KeyView` aliases into `s.buf` on the happy path (alias
    survives buf growth — GC pins the old backing) vs the old per-key heap-string
    allocation. Falls back to `stringSlow` for escapes. See `scan/CLAUDE.md`.
25. **`peelSliceField` initializes `HintLen=-1`.** Nested-slice recursion used to
    inherit Go's zero `HintLen=0`, read by `preallocCap` as "opt-out" so every
    nested row started cap=0; `-1` ("unset") falls through to kind defaults.
26. **Bitmask seen-flag tracking for wide structs.** Per-field `bool` locals for
    ≤32 fields; above that `var _seen uint64` (or `[N]uint64` for >64) cuts the
    frame from N bytes to 8/⌈N/64⌉. Wins only on wide + recursive structs.
27. **In-place decode for every elem kind.** Slice/array elem decode writes
    directly into the final slot: `[N]*T` → `_slab[ivar]`; `[N]T` → `dst[ivar]`;
    `[]*T` → pre-grow `append(_slab, zero(T))`, target `_slab[len-1]`; `[]T` →
    pre-grow `append(dst, zero(T))`, target `dst[len-1]`. No `var ev0`/post-decode
    copy-back. `inlineScanInt64`/`Uint64` receive `target`+`castFn`; pre-grow uses
    `zeroLit`.
28. **Position-var pass-through; no `kN := posVar` alias.** Slice/array decoders
    thread the caller's position var directly; each inner advances the SAME
    counter and the outer continues from it. Only data locals keep depth suffixes.
29. **Inline `null` peek; no `_np`/`_ok` locals.** The 4-byte `null` check is
    emitted byte-by-byte at the call site via `inlineNullPeek(posVar)`.
30. **Single position cursor in dispatch loop.** No separate `j := i` — every step
    (key scan, colon, value decode, comma/`}`) advances `i` directly. Stream path
    mirrors via `s.Pos`.
31. **Single local in `inlineScanString` (`ke` only), brace-less.** Start =
    `posIn+1` inline; slow-path fallback reads from the unchanged `posIn`. `ke`
    lands in the caller's scope; only renderMap's value scan adds explicit braces.
32. **Concrete-type fast paths in `AppendAny` for typed primitive slices/maps.**
    See `encode/CLAUDE.md` for ordering. Outpaces stdjson v1 and jsonv2 on map
    shapes.
33. **`AppendAny` concrete cases for `json.RawMessage`, `time.Time`,
    pointer-to-primitive.** These pre-empt the `json.Marshaler` branch /
    `reflect.Pointer` fallback. Concrete cases MUST sit before interface dispatches.
34. **Dispatch-level `null` peek breaks, not nests.** A field-level null match
    inside the key-dispatch switch ends with `break` (straight to comma handling)
    instead of wrapping the whole value decode in an `else` —
    pointer/slice/map/`[]byte`, bytes + stream. Gated by `nullBreakOK`
    (`AtDispatch` + no field-level value steps). Nested-slice elements get the
    same flattening via `FieldInfo.NullDone`: the PARENT element loop consumes the
    null (nil slot + duplicated elem-validation + comma + `continue`) and the
    inner emitter skips its own peek. Map values / alias bodies keep the if/else.
35. **Omit-guard pointer peel on marshal.** `AppendJSON`'s omitempty/omitzero guard
    for a pointer field is exactly `X != nil`, so the value emit peels one pointer
    level (`renderAppendValue` on `(*X)`) — no dead `if X == nil { null }` rung.
36. **Brace-less value emitters.** Decode value emitters write locals straight into
    the caller's scope — no `{ … }` wrapper per value (slice/array/map, time/
    duration/netip/url/big*/raw/sqlnull/any/string-tag/struct/bytes, cross-pkg
    fallbacks, inline catch-all). Sound because every call site owns its scope.
    Stream emitters renamed `var v` temps (`sv`/`f`/`u`) so a pointer-leaf caller
    can declare `var v <leaf>` in the same scope; colliding locals get unique
    names (renderMap value scan `ve`, map-marshal first-entry flag
    `first<GoName>`, encode cross-pkg temps `b<GoName>`). The one brace kept: the
    slice-elem `bs := json.Marshal` encode fallback.
37. **Map values decode straight into `m[mk]`.** No `mv`/`mn` temps —
    string/bool/int/uint/float and pointer values multi-assign the map index
    directly (pointer cascade is assignment-only under TargetNil, so the
    unaddressable index is fine). Narrow numerics keep a wide cast temp; STRUCT
    values keep a fresh `var mv T` (direct decode would merge duplicate map keys
    instead of fresh-decoding each — stdlib parity).
38. **Named-result return slot.** `DecodeFrom`/`DecodeFromStream` emit as
    `func (recv T) DecodeFrom(...) (result T, _ int, _ error)` with a `result =
    recv` prologue. The named first result homes the value in the caller's return
    slot, so every `return result, …` is register-set + RET with no struct copy
    (vs an anonymous result, where each RET site copied the full receiver-sized
    struct). Happy path copy-neutral; merge semantics unchanged. Struct + alias,
    both paths. `parseerr` post-pass unaffected (return text still `return result, …`).
39. **Bounded unchecked digit-accumulation prefix (bytes path).** The inline
    int/uint scanners (`inlineScanInt64Stmt`/`Uint64Stmt`) and runtime `scan.Int64`/
    `Uint64` split the digit loop: the first ≤18 (int) / ≤19 (uint) digits
    accumulate with NO per-digit overflow check (`10^18-1 < MaxInt64 < |MinInt64|`,
    `10^19-1 < MaxUint64`). A 19th/20th digit resumes the original checked loop,
    keeping `ErrNumberOverflow` identity and error position bit-identical.
    Accept-set unchanged. Pinned by `TestInt64_OverflowBoundaryLattice` /
    `TestUint64_OverflowBoundaryLattice`. Stream keeps the checked loop (mid-number
    ReadMore refill complicates the window).
40. **Whitespace-skip `<= ' '` exit gate.** `inlineSkipWS` and `scan.SkipSpace`
    prepend `data[i] <= ' '` to the 4-way WS test. On compact JSON the dominant
    non-whitespace byte exits on one compare (gc lowers `<= ' '` to a single
    CMPB+JHI) instead of four. Boolean-identical accept set (control bytes < 0x20
    fall through to the unchanged 4-way and still stop the scan). Stream path
    already has its `> ' '` fast path.
41. **Stream `StringView` for transiently-consumed value strings.** A
    `Stream.String` sibling returning an `unsafe.String` alias into `s.buf` (no
    `KeyView` scalar prelude). Generated stream decoders use it where the value
    string is consumed before the next stream op and retains no bytes past that
    point: base64/base32/hex `[]byte`, `time.Time`/`time.Duration` text formats,
    `net.IP`, `netip.Addr`/`Prefix`, `big.Float`/`big.Rat`, cross-pkg
    `TextUnmarshaler`. NOT `url.URL` (`url.Parse` slices its input), plain `string`
    fields, map keys, or map/slice string elems — those outlive the scan and keep
    the copying `Stream.String` (itself `StringView` + `strings.Clone`). See
    scan/CLAUDE.md "Stream copies vs bytes-path aliases" / "`StringView`".
42. **Exact-cap comma pre-count for flat numeric/bool SLICES (bytes path).**
    Numeric/bool elements carry no `,` or `]`, so `bytes.Count(data[k:k+e], ',')+1`
    over the value span yields the precise element count before any `make`
    (`scalarCountable` gate, non-empty arm), killing the 1→2→4→8 growth chain with
    no over-cap residency. Gates on `userPreallocHint < 0`; a reused (non-nil) slot
    keeps its backing; applies at every depth via `peelSliceField`. String/struct/
    pointer elems excluded (delimiters inside quotes / nested objects). Malformed
    array with no `]` falls back to cap 1 and errors in the scan loop. Bytes only
    (stream has no full buffer). **NOT for maps:** object keys are strings that may
    contain `:`, so one valid colon-laden key would inflate a `:`-count into a huge
    `make` — a memory-amplification footgun on well-formed input (the slice case is
    immune since scalars can't carry the delimiter). Maps keep the unsized make().
43. **Nested-container slot hoisted into a reuse-seeded depth-local.** Instead of
    threading the parent slot expression (`dst[len(dst)-1]`/`dst[ivar]`) into the
    inner loop (re-evaluating the index + a parent-backing write barrier per
    element), `rowN := <slot>`; recurse into `rowN`; one `target = rowN` publishes
    the finished row — the inner loop writes a barrier-free local header. The row
    is seeded from the carried slot so its backing is reused: a slice-of-slice
    outer grows by reslicing within cap (carried inner header survives into the
    slot), `rowN := dst[len-1]; if rowN != nil { rowN = rowN[:0] }` resets len, and
    the inner make-guard skips the make on reuse. A `null` element nils the slot
    unconditionally. Both bytes + stream; composes with #42. Pinned by
    `TestMerge_nestedSliceBackingReused`.
44. **Byte-length-gated rune-count validation.** For a B-byte UTF-8 string the rune
    count R satisfies `ceil(B/4) <= R <= B`, so cheap `len` checks resolve the
    common cases without an `utf8.RuneCountInString` walk (`emitRuneRule`):
    `minrunes=N` fail-free `len<N`, pass-free `len>=4N-3`, walk only band
    `[N,4N-3)` (`N<=1` collapses to the empty-string check); `maxrunes=N` pass-free
    `len<=N`, fail-free `len>4N`, band `(N,4N]`; `runes=N` fail-free `len<N ||
    len>4N`, band `[N,4N]`. The failure literal's `Got` reports the real count
    (cold walk on the fail path, live `rc` inside a band). **Tier (c) — ASCII
    subsumption:** if an ASCII-implying rule (`alphanum`/`numeric`/`ascii`/
    `hexadecimal`) passed earlier in the same run, `R == len` exactly so the walk
    is dropped entirely. Gated on `asciiSeen && !multiErr` in declared order
    (charset rule must precede the rune rule; skipped under multierr where a failed
    earlier rule doesn't stop reaching the rune rule on non-ASCII input).
    Wire-identical. Pinned by `TestGenerate_runeGates`.
45. **Bytes-path container-loop hygiene (wire-identical, smaller generated code).**
    (a) **no leading WS skip** in `emitByteSliceRead`/`renderMap` — every value
    entry already skips WS; the top-level alias path (the one dependency) got an
    explicit skip in `renderAliasContainerDecode`. Stream KEEPS its leading
    `s.SkipSpace()` (double duty: WS + `ReadMore` buffer-ensure). (b) **collapsed
    empty-peek** when both arms would be byte-identical `dst = T{}` (`[]struct`,
    `[][N]T`). (c) **do-while element/map loop** — `for i<len && data[i]!=']' {…}`
    → `if … { for {…} }`: the per-iteration re-check was redundant (entry
    guaranteed non-`]` by the peel; later iterations by the post-comma `noClose`
    guard); the one-time guard preserves the empty/truncation `ErrBadArray` path.
    (d) **top-level alias early-return** — a container ALIAS is the whole value, so
    the bytes emitters take a `topLevel` flag (set only by
    `renderAliasContainerDecode`) and `return result, i, nil` at each exit instead
    of falling through. Pinned by `TestWhitespace_Tolerance` + the bytes-vs-stream
    fuzzer. Stream not done.

## Design decisions (the why)

1. **`unsafe.String` boosts perf** by avoiding GC pressure — can backfire if
   parsed strings are referenced long-term after the input is mutated.
2. **Struct fields sorted alphabetically at codegen time** (default). Zero runtime
   cost; deterministic, compresses better.
3. **No runtime reflection anywhere.** Even the cross-package fallback uses
   `encoding/json.Unmarshal` only for types NOT in the generation pass.
4. **Custom validators / mods / converters = codegen-time function injection.**
   `pipe:` steps (`@EvenOnly`, `@Squash`, `@Conv` decode variants) resolved via
   `packages.Load` at parse time — looked up, classified by signature,
   type-checked against the working type, emitted as a direct call. No runtime
   registry, no `func(any) any` boxing, zero alloc. Cross-package via
   `@pkg.FuncName` through source-file imports; blank imports work. Validator
   errors wrap as `validation.CustomError{Name, Value, Cause}` (or `PredicateError`
   for the bool form); fallible-mod errors propagate as parse errors (`ModError`
   for the bool form).

## Conventions

### How to regenerate

Build the binary into the project dir (`./ggen`), never `/tmp` — it stays
discoverable, avoids cross-session collisions, and matches the test harness path.

```sh
go build -o ggen ./cli
./ggen ./decode/... ./encode/... ./scan/...
easyjson bench/types.go
GOEXPERIMENT=jsonv2 go generate work
```

The binary builds from the `cli/` module to project-root `./ggen` (so the
`../ggen` references in `bench/` and `integrationtests/` resolve). ggen is
module-scoped — `./...` visits only the invoked module's packages; `cli/`,
`bench/`, `integrationtests/` each carry their own `go.mod` and must be regen'd
from inside (one invocation per module). In `integrationtests/`, each annotated
source carries `//go:generate ../ggen $GOFILE` and emits a sibling
`<file>_ggen_test.go`.

### Test files (`cli/` module)

CLI tests live under `cli/`; per-package runtime tests next to implementation
(`encode/`, `scan/`); feature/roundtrip/compat/fuzz under `integrationtests/`;
benchmarks under `bench/`.

- `parse_test.go` — annotation/tag/rule parsing, cross-package symbol resolution.
  Hosts the test-only `generate(pkg, structs)` wrapper (production calls
  `generateTo` against an `*os.File`).
- `tags_test.go` — `json:` tag parser. `pipe:`/`hint:` parsing is in `pipe_test.go`.
- `applicability_test.go` — rule-applicability matrix.
- `cli_test.go` — CLI integration: binary built in TestMain, file-naming contract,
  `./...` walk + dir-skip, per-flag output effects.
- `bench_test.go` — `BenchmarkGenerate`.
- `log_test.go` — Logger level + sink behaviour.

### Keeping docs in sync

This file = implementation-detail doc (the *why*). `README.md` + `SKILL.md` =
user-facing surface (*what*/*how*). **All three move together** — every change
touching user-visible surface (CLI/annotation flags, codegen behaviour, wire
format, generated method surface, field tag syntax, new Go kind/wire-shape, new
runtime API) must propagate to **both** README and SKILL in the same commit.
Benchmark numbers → `bench/CLAUDE.md`; test-suite layout →
`integrationtests/CLAUDE.md`; per-package runtime details → matching package
CLAUDE.md; backlog / tried-and-rejected → `.claude/backlog.md`.

**README authoring rules.** README is the user-facing front door: what ggen is,
what it does, how to use it, what numbers mean. NEVER spill implementation detail
into it (runtime/harness mechanism, `unsafe.String` aliasing, slab heuristics,
`KeyView` vs `String`, `preallocCap`/`peelSliceField` shape, pprof internals). DO
put in README: what each benchmark measures (one sentence), how to read each
metric, when a user would care, the bench table + interpretive paragraph, and
caveats affecting the user's choice (e.g. "strings alias the input, don't mutate
after decode"). If you write "internally", "implementation", "under the hood", or
name a private function / runtime API in README — stop; it belongs in CLAUDE.md or
a code comment.

## Backlog

See @.claude/backlog.md
