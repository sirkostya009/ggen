# ggen — zero-copy, zero-reflection JSON codegen for Go

Code generator. Parses annotated Go structs, emits methods on annotated structs.
Hand-rolls byte scan over caller's `[]byte` or `*scan.Stream`;
bytes path alias input via `unsafe.String` — no copy, no tokens, no AST.

This file documents **CLI / codegen surface**. Runtime internals,
benchmarks, integration-test conventions, backlog live under each
package's own CLAUDE.md (see "Repo layout").

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

Four modules under one `go.work`: root (`github.com/sirkostya009/ggen` —
runtime library `decode`/`encode`/`scan` only, no external deps), `cli/`
(the generator, depends on `golang.org/x/tools`), `bench/`, `integrationtests/`.
The CLI doesn't import the runtime packages — it emits their import paths as
string literals into generated code. CLI tests = unit tests + CLI integration
tests + generator bench (`BenchmarkGenerate`), all under `cli/`; other benches
in `bench/`, post-gen integration tests in `integrationtests/`.

## Generator CLI (`main` package)

### Invocation

```
ggen ./...                    every package matched by the pattern (module-scoped, as `go build`)
ggen <dir>                    one package
ggen <file.go> [Names...]     one file; optional struct name filter
```

Packages load via `golang.org/x/tools/go/packages` with full type info,
interface impls (TextMarshaler, ByteDecoder, JSONMarshaler, …) picked up,
emitted as direct method calls — no runtime probing. If can't resolve
(temp file no `go.mod`), falls back to AST-only mode, emits plain
`encoding/json` fallback for cross-package types.

Run `ggen` with same `GOEXPERIMENT` env as user code. Files behind
`goexperiment.jsonv2` otherwise invisible.

Pattern mode (`./...`, `./sub/...`, `...`) resolves via `packages.Load` —
module-scoped, workspace-aware, never crosses module bounds. Subdir
with `go.mod` skipped; multi-module repos run ggen once per module. Test-only
packages (no non-`_test.go` files) skipped in pattern mode, picked up in
single-package mode. Processing = post-order over matched import
subgraph (deps first). Transitive deps outside matched set left alone.
Work runs sequential in topo order (parse cost dominates; codegen globally
locked). Dot/underscore-prefix dirs, `vendor/`, `testdata/`,
`node_modules/` skipped by `go list` — no custom skip rule in ggen.

### Flags (all opt-in, apply to every struct in pass)

- `-o <path>` — override output path (single file / single dir only)
- `-pkg <name>` — override package name in output
- `-marshal` — emit `MarshalJSON` method
- `-unmarshal` — emit `UnmarshalJSON` method
- `-multierr` — accumulate validation failures into `validation.Errors`,
  return at end of parse, parse errors always return immediately
- `-allowdups` — allow duplicate keys in payload, first-wins — next occurrences
  skipped. Default: error with `validation.DuplicateKeyError`
- `-novalidate` — skip validation rules, required-field checks, mods
- `-ignoreunknown` — silently skip unknown JSON keys. Default: error with
  `validation.UnknownKeyError`. Overridden when inline map field present
- `-nullzero` — accept an explicit JSON `null` on every non-pointer value
  field, decoding it to the Go zero value. Default: hard-error (see "null
  acceptance is kind-gated"). A per-field `nullzero` decode variant in `pipe:`
  opts in one field; no-op on already-null-aware kinds
- `-nosortkeys` — emit fields in Go declaration order. Default: sorted
  alphabetically. Inline map fields stay last
- `-usenumber` — decode JSON numbers into `any` fields as `json.Number`
  instead of `float64`. Mirrors stdlib `UseNumber()`
- `-htmlescape` — opt INTO HTML-safe escaping (`<`, `>`, `&` → `\uXXXX`) on
  marshal. Default = literal
- `-dry` — parse + validate annotated structs, surface every error, emit
  no file. Composes with `-v` (prints `ok <path> (N structs)` per package).
  Rejects `-o` / `-pkg`

### Per-struct annotations

Comment on struct (or gen-decl) = `//ggen:generate` directive (no
space after `//`, mirrors `//go:generate`).
Space-separated tokens after:

- `marshal`
- `unmarshal`
- `multierr`
- `allowdups`
- `novalidate`
- `ignoreunknown`
- `nullzero`
- `nosortkeys`
- `usenumber`
- `htmlescape`

Annotations apply only to a struct.

## Struct tags (on fields)

- `json:"name"` — JSON key name (field ignored otherwise)
- `json:"-"` — field set ignored explicitly
- `json:",inline"` — catch-all map for unknown keys. Type must be
  `map[string]V` (string-keyed); V may be `any`, a primitive, a ggen-annotated
  struct, or any other type (typed elems decode via the elem's fast path when
  available, else fall back to `encoding/json.Unmarshal` over the captured
  span). Overrides `ignoreunknown`. Entries spliced out on marshal
- `json:"name,omitempty"` — not marshaled when JSON-empty (null, "", [], {})
- `json:"name,omitzero"` — not marshaled when Go-zero value
- `json:"name,string"` — wrap primitive as JSON string on marshal, unwrap on
  unmarshal. Primitives only, like stdlib
- `json:"name,format:X"` — format hint for native types (see Kinds).
  **jsonv2 requirement: `format:X` must be LAST in tag.**
  (No `nullzero` json opt — it became a `pipe:` decode variant, below.)

- `pipe:"..."` — the unified decode/transform/validate pipeline (replaced the
  old `ggen:`/`mod:` split; those tags no longer exist). One ordered,
  whitespace-separated step list parsed in `pipe.go` into a `ParsedPipe`
  (Presence / Variants / Outer / Keys / Levels). Conceptually:

    ```
    pipe        := stage ( "~" stage )*
    first stage := variant ( "/" variant )*    // decode: JSON-shape dispatch
    later stage := step ( WS step )*            // value steps, inner:/keys: levels
    ```

    - **Presence** (lifted, position-independent): `required` → object-close
      seen check (`RequiredError`, via `IsRequired()` which now also reads
      `FieldInfo.Presence`); `optional` is a marker. Absent key → Go zero.
    - **Decode stage** — `/`-separated variants, one per JSON shape; ggen peeks
      the first byte and routes (`variants.go`). `~` is optional sugar: with no
      `~` the decode stage is the leading run of variant keywords
      (`leadingDecodeExtent`). Variants:
        - `.` — native decode of the field type T.
        - `nullzero` — JSON `null` → `zero(T)` (sets `FieldInfo.NullZero`;
          reuses the existing null-as-zero emit). Bare `nullzero` needs no `.`.
        - `@Conv` — converter `func(W)T` / `func(W)(T,error)` / `func(W)(T,bool)`,
          OUTPUT-anchored (result == T). ggen scans input `W` (primitive or
          ggen-decodable struct → delegates to its `DecodeFrom`) and converts.
          Same emit on bytes + stream; encode is untouched (marshals native T).
          A lone leading `@Func` is a value step, NOT a converter — needs `/`,
          a leading `.`, or `~`. Variants must claim disjoint shapes
          (`checkVariantShapes`).
    - **Value steps** (after the decode stage): mods + validators interleaved
      **in declared order** — a unified ordered `[]Step` per level, emitted by
      `renderPipe` (which dispatches each step to `renderOneVal`/`renderOneMod`).
        - `inner:` scopes to one container level down, `keys:` to map keys. A
          bare prefix takes ONE step (`inner:trim`); parentheses group several
          (`inner:(trim maxlen=20)`); groups nest for deeper levels
          (`inner:(a inner:(b))`). Parsed recursively by `parseScope`/
          `parsePrefixEntry`/`matchParen` (pipe.go). Levels carried as
          `FieldInfo.Levels [][]Step` (`Levels[0]` = per-elem), peeled by
          `peelSliceField`, emitted via `elemSteps`. (No `;` — retired.)
        - validators: `notempty`; `len/minlen/maxlen=N`; `runes/minrunes/
          maxrunes=N`; `gt/gte/lt/lte/eq/neq=N`; `multiple=N`; `oneof=a|b|c`;
          `email`/`url`/`ascii`/`printable`/`alphanum`/`numeric`/`lower`/`upper`/
          `hexadecimal`; `starts/ends/contains=X`.
        - mods: `trim`, `lower`, `upper`, `trimleft=X`, `trimright=X`,
          `replace=old|new`, `clamp=lo|hi`.
    - **Custom funcs** (`@FuncName` / `@pkg.FuncName`) — classified by signature
      in `customfunc.go` (`classifyValueFunc` for value steps, `classifyConverter`
      for variants), type-checked against the working type at that level:
      `func(T)error`→validator (`CustomError`), `func(T)bool`→validator
      (`PredicateError`, message-capable), `func(T)T`→pure mod,
      `func(T)(T,error)`→fallible mod (parse error), `func(T)(T,bool)`→fallible
      mod (`ModError`, message-capable). `func(bool)bool` is rejected. Bool
      forms carry an inline message `@Even:'must be even'`. Cross-package via
      source-file imports; blank imports work.

- **Lexing/quoting** (`tokenizePipe`): steps WS-separated; structural glyphs
  `/ ~ ( )` significant with/without spaces (plus the `inner:`/`keys:` word
  prefixes); a value/message may be single-quoted, required only when it
  contains whitespace; `\'` is a literal quote. (A leftover `;` errors with a
  pointer to `inner:(…)`.)

- `hint:"..."` — prealloc capacity only (replaced `ggen:"hintlen=N"`).
  `hint:"N"` → `make([]T,0,N)`; per-level via `inner:` (`hint:"32 inner:8"`).
  Lifted, order-independent (`FieldInfo.HintLen` / `HintLevels`). `hint:"0"`
  opts out; negative is a parse error.

### Internal model

`FieldInfo` keeps the legacy split buckets (`Validation`/`Mods`/`Elem*`/`Inner*`/
`Key*`) as the SOURCE for order-independent consumers (import-collection walks,
`peelSliceField`, the pointer-leaf partition) — they are DERIVED from the
ordered `Pipe`/`KeyPipe`/`Levels` by `deriveBuckets`. The ordered step lists are
the source of truth for emit ORDER at the value-stage sites; `fieldPipe`/
`elemSteps` fall back to `stepsFromLegacy(mods, vals)` for synthetic fields that
set only buckets.

### Rule applicability (parse-time)

`applicability.go` rejects mismatched rules against the working type (clear
message); per-level gating (elem kind under `inner:`, `string` under `keys:`).
Cases covered exhaustively in `TestCLI/InvalidRuleApplication`.

## Generated methods (per annotated struct T)

```go
// DecodeFrom is a zero-copy parser. Strings and RawMessage are alised into data
func (result T) DecodeFrom(data []byte) (T, int, error)
// DecodeFromStream is a buffered io.Reader wrapper with an intermediate buffer.
// Useful for slow streams or lower memory usage. Break zero-copying - all strings
// and json.RawMessage are copied from payload.
func (result T) DecodeFromStream(s *scan.Stream) (T, error)
// JSONSize precalulcates size of JSON payload of T in bytes
func (s T) JSONSize() int
// AppendJSON appends a payload string to dst. Errors on invalid numbers (like NaN)
func (s T) AppendJSON(dst []byte) ([]byte, error)
```

**Cursor convention.** Bytes-path `DecodeFrom` takes slice starting at
value's first byte, returns bytes consumed; caller advances own
cursor (`i += n` after reslicing `data[i:]`). Stream-path `DecodeFromStream`
takes/returns no cursor — cursor = `s.Pos`, owned by Stream and
advanced in-place by every scan primitive. To capture raw span:
`start := s.Pos; s.SkipValue(); raw := s.Bytes()[start:s.Pos]`.

**Decode-into-receiver semantics.** Receiver passed in IS merge source.
Scalar fields persist across JSON omission (stdlib-merge shape); container
fields reset at top of DecodeFrom so decoder never appends over
carried-in data (deliberately unconditional — blank payload → blank slate,
capacity kept; lazy per-key reset was implemented and reverted, see backlog
"Tried Rejected"):

- slices and `[]byte`: `if X != nil { X = X[:0] }` at entry (backing array
  reused; `make(...)` only when `X == nil`). For a slice-of-slice (`[][]T`,
  any depth), the **inner row backings are reused too**: the outer grows by
  reslicing within cap (keeping the carried inner header in the slot) and each
  row is seeded `rowN := slot; if rowN != nil { rowN = rowN[:0] }` so the inner
  decode reuses its backing; a past-cap/fresh slot reads back nil and allocates
  anew (opt #43; pinned by `TestMerge_nestedSliceBackingReused`)
- `map[K]V`: `if X != nil { clear(X) }` at entry (buckets reused; `make` only
  when nil)
- nested struct: `result.X, _, _ = result.X.DecodeFrom(...)` — value-receiver
  takes existing value as merge source auto
- pointer `*T` / `**T` / `***T` / … (any depth): **parse-first** cascade.
  `null` → `result.X = nil` (drops a carried-in chain, stdlib merge parity).
  Otherwise decode the leaf into a stack `var v Leaf` FIRST — a parse failure
  returns before any mutation, so no chain is ever allocated for a value that
  never landed. On success an assign cascade REUSES the non-nil prefix of the
  receiver's pointer chain and allocates `new(new(…v))` (Go-1.26 `new(expr)`,
  one `new(` per still-nil `*`) only from the first nil level down; a
  fully-allocated chain takes the final `else` (`(*(*X)) = v`). A mergeable
  leaf (struct/slice/map/array) is seeded from the carried-in value first
  (`if X != nil && (*X) != nil { v = (*(*X)) }`) so it still merges; primitive
  leaves skip the seed. A widened numeric leaf (int/int8/16/32, uint…, float32)
  scans into a wide temp (`var v int64`) and casts at the assign site
  (`new(int(v))`) instead of carrying a separate conversion var — drops the
  stream path's widening temp entirely (`widenedLeafCast`). Bytes-path fast
  path: an int/uint leaf with no built-in mods/validation skips the temp
  altogether — the inline scanner already materializes `n`, so the assign
  cascade runs in-block off `int(n)` (`inlineScanInt64Stmt`/`…Uint64Stmt`).
  Fresh-nil targets (map-value temp `mv`, pre-grown `[]**T` slot) set
  `FieldInfo.TargetNil` — seed skipped, cascade collapses to one straight
  `ref = new(new(…))`; `[N]**T` slots keep the full cascade (array elems carry
  the receiver's chain). Leaf decodes natively at every depth — NO
  encoding/json fallback. Helpers:
  `pointerDepth`, `derefStr`, `newChain`, `widenedLeafCast`, `emitPointerSeed`,
  `emitPointerAssign`. Same emit for bytes + stream paths
- fixed arrays `[N]T`: every slot overwritten or strict-length-errors, no entry
  reset needed

JSON `null` for slice/map sets `result.X = nil` (stdlib v1/v2 parity). JSON
`[]`/`{}` on non-nil receiver keeps `[:0]`'d / cleared container; on
nil receiver allocates empty non-nil container.

**`null` acceptance is kind-gated (diverges from stdlib).** ggen emits a 4-byte
`null` peek only for: pointer (`*T`), slice (KindSlice), map (KindMap),
`[]byte` (KindBytes — null ↔ nil, nil marshals as `null`), `sql.Null*`, and
raw-message (`json.RawMessage`/`jsontext.Value`) fields. Every
other kind — non-pointer scalars (`int`/`bool`/`string`/`float`),
`time.Time`, `time.Duration`, `net.IP`/`netip.*`, `url.URL`, `big.*`, UUID, and
other text/number kinds — has NO null branch, so an explicit JSON `null`
hard-errors the parse (`scan: expected string` / `invalid number` / …). stdlib
v1/v2 instead accept `null` everywhere (zero the field / no-op). Consistent with
ggen's other strict defaults (UnknownKeyError, strict array length,
DuplicateKeyError, trailing-comma rejection) — for a nullable scalar, use a
pointer. Decode-into-receiver divergences are pinned in
`integrationtests/stdcompat_test.go`
(`TestStdCompatMerge_IntentionalDivergences`).

**`nullzero` opts a value field into null-as-zero.** A `nullzero` decode
variant in `pipe:` (per field) / `-nullzero` / `//ggen:generate nullzero`
(whole struct) make a non-pointer value field accept an explicit JSON `null`,
decoding it to the Go zero value instead of erroring — the documented middle
ground between the strict-reject default and stdlib's accept-everywhere. Gated
by
`nullZeroApplies` (set + `AtDispatch` + a kind that would otherwise reject
null; the already-null-aware kinds above stay no-ops). Emit mirrors the
pointer/slice null branch (opt #34): a 4-byte `null` peek (`inlineNullPeek`
bytes / `emitStreamNullZero` stream) sets `ref = <zeroLit>` then `break`s out
of the dispatch case when no field rules follow (flat, `nullBreakOK`), else
nests the value decode in an `else` so the shared `validateAndMod` runs on
either the decoded value or the zero (so `nullzero` + `minlen=1` on a string
still rejects `null`→`""`). Per-field tag ORs onto the struct/CLI flag in
`applyCLIFlags`. Struct fields only — not top-level alias types. Decode-only;
encode is untouched. Pinned in `integrationtests/nullzero_test.go` +
`cli_test.go` (`NullZeroFlag_AcceptsNullIntoValueField`).

**Trailing commas are rejected (stdlib parity).** Every element-loop comma
branch (slice / map / tuple / nested / pointer-elem / `[]byte` format:array,
bytes + stream) emits a guard after the comma's WS skip: container close or
EOF right after a comma → `scan.ErrBadArray`/`ErrBadObject`
(`emitNoCloseAfterComma` / `streamNoCloseAfterComma` in `generate.go`). The
EOF half also fixes a stream-path panic: `(*Stream).SkipSpace` returns nil at
EOF with `Pos == len(buf)`, and the element loop's top used to index
`s.Bytes()[s.Pos]` unguarded on input truncated right after a comma. Pinned in
`integrationtests/scan_decode_test.go` (`TestTrailingCommaRejected`,
`TestTruncatedAfterComma`).

**Decode-into-receiver vs stdlib merge — divergences.** ggen's receiver-merge
is NOT a drop-in for stdlib merge: (1) container fields reset at decode entry,
so an OMITTED slice/map key is emptied (stdlib retains it); (2) a present map
key REPLACES (clear+refill) rather than merging entries (stdlib retains
receiver-only keys); (3) scalar `null` errors (above). Scalars-persist-on-omit,
slice-replace-on-present, null→nil for slice/map/pointer, nested-struct merge,
and `*T`/`**T` reuse all MATCH stdlib — pinned in `TestStdCompatMerge_Parity`.

Call with zero-value receiver for fresh decode (`T{}.DecodeFrom(data)` for
struct/slice/map/array; `var zero T; zero.DecodeFrom(data)` for primitive
aliases). To merge into existing value, call its `DecodeFrom` directly.

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

Annotated named types (`//ggen:generate type T <underlying>`) get same
method surface as struct (DecodeFrom, DecodeFromStream, JSONSize,
AppendJSON), driven by `renderAlias*` helpers in `alias.go`. Top-level
renderers dispatch to alias paths when `s.IsAlias` set (except struct
aliases that fall back to field introspection, which set `IsAlias=false` and
route through regular struct codegen).

Accepted underlying kinds:

- **primitive** (`string`, `bool`, `int*`, `uint*`, `float*`): scan via
  `scan.X` / `_s.X`, cast to alias. `htmlescape` flips string-append helper
- **struct** (`type LocalUUID uuid.UUID`): methods don't propagate from
  RHS, so probing uses `inspectType` on RHS named type. Three-step ladder:
    1. _ggen-method delegation_ — if underlying has AppendJSON+DecodeFrom: cast
       → underlying.Method() → cast back. Cheapest
    2. _field introspection_ — plain struct with ≥1 exported field: walk
       `*types.Struct` via go/types, synthesize FieldInfo per exported field
       (`extractFieldFromTypes`), `IsAlias` flips false, regular struct codegen
       runs. Field access via `result.X` sound (identical memory layout).
       **Preferred over JSON/Text marshaler delegation even when those exist** —
       hand-rolled struct codegen beats reflective marshaler calls
    3. _JSON/Text marshaler delegation_ — opaque struct (no exported fields,
       e.g. `time.Time`) with JSON or Text marshaler pair: cast → drive method
       → cast back

    Wire-shape implication: alias of struct with both exported fields AND
    custom MarshalJSON uses introspected field shape, NOT underlying's
    MarshalJSON. For underlying's exact shape, declare with no exported
    fields (forces delegation) or write own marshal hook

- **slice / map / array** (`type Tags []string`, `type Lookup
map[string]int`, `type Tuple [3]int`): synthetic FieldInfo (ElemType,
  ElemKind, ArrayLen) handed to field-level emitters with `result`
  (decode) / `s` (encode) as ref. All field-level features carry over —
  strict-length arrays, slabbed `[]*T`, inner validation
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
- Pointer to any of above (`*T`) — null ↔ nil. Multi-level (`**T`, `***T`, …)
  also native: decode parses the leaf first then builds/reuses the chain
  (`new(new(v))` for the nil tail), encode derefs level-by-level (intermediate
  nil → `null`). No reflective fallback
- `[]T` (slice), `map[string]V` (string-keyed only; pointer values native,
  any depth)
- `[]*T` / `[N]*T` (slice/array of pointer-to-struct) — element pointers come
  from single backing slab (`make([]T,0,cap)` slices, `[N]T` arrays) so N
  allocs collapse to ~log(N). Nil elements → nil pointers; encode nil → `null`.
  Multi-level elements (`[]**T`, `[N]***T`, nested `[][]**T`) skip the slab and
  run the scalar pointer cascade per element (`elemPtrField` → `renderField`);
  pointer map values (`map[string]*V` / `**V` / …) decode the same way into a
  temp then store — no `encoding/json` fallback at any depth
- Nested struct (generate-time probing of `DecodeFrom`/`UnmarshalText`/`UnmarshalJSON` and `AppendJSON`/`AppendText`/`MarshalText`/`MarshalJSON` methods; with default-stdlib fallback)
- Embedded struct (unnamed field) — fields promoted to parent's JSON object
- `time.Time` — `format:unix`/`unixmilli`/`unixmicro`/`unixnano`/`RFC3339`/
  `RFC3339Nano` + custom (jsonv2 supported) + other `time.X` constants
- `time.Duration` — `format:sec`/`milli`/`micro`/`nano`/`units` (default,
  parses `"1h30m"`)
- `net.IP`, `netip.Addr`, `netip.Prefix` — text form. Marshal via
  `encoding.TextAppender` decode via `net.ParseIP`/`netip.ParseAddr`/`netip.ParsePrefix`
- `[]byte` — `format:base64` (default)/`base64url`/`base32`/`base32hex`/
  `base16`(`hex`)/`array` (JSON array of numbers). `null` ↔ `nil`: decode
  accepts `null` → nil, nil marshals as `null` (empty non-nil → `""`/`[]`);
  no opening-quote fold (foldLeadingQuote skips KindBytes)
- `json.RawMessage` / `jsontext.Value` — opaque span via `scan.SkipValue`,
  aliased into field. Raw passthrough on encode (`null` if empty/nil)
- `net/url.URL` — JSON string, `url.Parse` / `encode.AppendURL`
- `math/big.Int`/`big.Float`/`big.Rat` — `big.Int` a JSON number, `big.Float`
  / `big.Rat` JSON strings (`"3.14"` / `"3/2"`; wrapping prevents float64
  precision loss, matches jsonv2). Encoded via in-place `Append` (zero alloc),
  parsed via `SetString`/`Parse`
- Other types — no dedicated kind. Any field whose type
  implements `encoding.TextAppender` / `TextMarshaler` / `TextUnmarshaler`
  routes through those methods. Marshal prefers `AppendText(dst)` (zero alloc),
  falls back to `MarshalText() + AppendString` (one alloc). Can also declare
  AppendJSON method for ggen to pick up (highest precedence).
- `database/sql.Null*` (`NullString`, `NullInt64`, `NullInt32`, `NullInt16`,
  `NullByte`, `NullBool`, `NullFloat64`, `NullTime`) **and the generic
  `sql.Null[T]`** (Go 1.22; inner field is always `V`). Decode probes `null`
  first → `Valid=false`; else reads inner value, sets `Valid=true`. Encode
  `null` when `!Valid`, inner value otherwise — wire shape is always
  inner-or-null, never the `{"V":…,"Valid":…}` struct dump. The named flavors
  use the string-keyed `SQLNullSpec` (`Field`/`Inner`/`Type`).
  **Generic `sql.Null[T]` supports ANY inner `T` ggen can render as a field.**
  With go/types info (`sqlNullGenericInfo` in `parse.go`), the parser builds a
  synthetic `FieldInfo` for `T` (via `extractFieldFromTypes` on a synthesized
  `V` var, name-qualifying foreign types and dropping the spurious
  underlying-peel for named types so `uuid.UUID`/`net.IP` keep their text path)
  and stashes it on `FieldInfo.SQLNullInner` (+ `SQLNullImports` for the type
  literals). The decode/encode/size renderers delegate the `V` slot to the
  standard field emitters (`renderField`/`renderStreamField`/`renderAppendValue`/
  `sizeContrib`) via that inner `FieldInfo`, so `sql.Null[T]` gets exactly the
  wire (and fast path) of a bare field of type `T`: primitives/`time.Time`/
  `netip`/marshaler types go native, anything else uses `T`'s own
  `encoding/json` fallback (which still emits the bare inner value, NOT the
  struct). Parent struct flags (`MultiErr`/`NoValidate`/`UseNumber`/`HTMLEscape`)
  copy onto the inner at render time (`sqlNullInnerField`). The AST-only loader
  (no go/types) keeps just the built-in-primitive generic forms via the
  string-based `SQLNullSpec` + `isSupportedSQLNullInner` gate; custom inners
  there fall back to `encoding/json` on the whole value
- `any` / `interface{}` — decode via `scan.Any` / `(*Stream).Any`, stdlib
  defaults: `null→nil`, `bool`, `number→float64`, `string` (zero-copy alias),
  `array→[]any`, `object→map[string]any`. With `usenumber`, emits
  `scan.AnyNumber` (numbers → `json.Number` aliased via `unsafe.String`).
  Encode via `encode.AppendAny` (type-switch ordering — see `encode/CLAUDE.md`)
- `[N]T` (fixed-length array) — JSON tuple with **strict count**: decode errors
  with `validation.LenError{Want:N}` when count ≠ N. Combines/nests freely:
  `[N]T`, `[][N]T`, `[N][]T`, `[N][M]T`, `[][N][M]T` via same recursive
  emitter as `[][]T`. `[]byte` stays KindBytes (base64) — only non-byte arrays
  get tuple treatment

### Wire-format divergences from stdlib

Two kinds intentionally diverge from `encoding/json` v1 + v2. ggen marshal
output _not_ subset of either for these — feed-through-stdlib reshapes
value, decode of stdlib JSON won't work for these fields. Round-trip
within ggen fine.

| Kind          | ggen wire             | stdlib wire (v1 + v2)                                    |
| ------------- | --------------------- | -------------------------------------------------------- |
| `net/url.URL` | `"https://x/p?q=1"`   | `{"Scheme":"https","Host":"x","Path":"/p", … 11 fields}` |
| `sql.Null*`   | inner value or `null` | `{"<Inner>":val,"Valid":true}` (plain struct, no hook)   |

## Output file naming

- Package mode, untagged: `<dir>_ggen.go` (non-test) / `<dir>_ggen_test.go`
  (test-only); both if both exist
- Package mode, tagged: `<dir>_<tag-slug>_ggen.go` per (tag, isTest) bucket
- Single-file: `<basename>_ggen.go` / `_ggen_test.go`; source `//go:build` line
  preserved in header
- `_test.go` sources = first-class inputs; test-only struct annotations route
  to `_ggen_test.go`

### Build tag propagation

Generator reads `//go:build <expr>` per source file, BUCKETS annotated
structs by constraint. Each (tag, isTest) bucket emits own gen file with
matching header — struct in `tagged.go` (behind `//go:build foo`) never
lands in unconstrained `<dir>_ggen.go`. Old-style `// +build` honored;
multi-term exprs canonicalized via `go/build/constraint.Parse`. Cross-bucket
struct refs in same package still route through direct DecodeFrom —
`generatedTypes` seeded with union of all buckets before codegen.

Filenames: untagged buckets keep `<dir>_ggen.go` / `<dir>_ggen_test.go`;
tagged buckets become `<dir>_<slug>_ggen.go` (slug collapses non-alnum runs to
single underscores: `goexperiment.jsonv2` → `goexperiment_jsonv2`,
`foo && bar` → `foo_bar`).

## Optimizations applied in codegen (nothing at runtime)

1. **Flat `switch key` dispatch.** One string switch over all JSON names —
   gc lowers it to length-grouped binary search / jump tables itself. Manual
   length-first outer `switch len(key)` benched equal (narrow) to -5.7%
   slower (100-field) vs flat; removed (see backlog "Tried Rejected").
2. **Slice cap from tag hint.** `preallocCap` picks initial cap for
   `make([]T,0,N)`. Precedence: `hintlen=N` > `len=N` >
   `max(minlen, default)` > default (`defaultPreallocCap = 4`). Maps via
   `mapPreallocCap` (no minlen — weak signal on maps).
3. **Field marshal order sorted by JSON name** at codegen time (alphabetical).
   `-nosortkeys` opts back to declaration order.
4. **Inlined scan primitives in hot path.** Raw byte-compare loops for
   `SkipSpace`, `String` (zero-copy happy path), `Int64`, `Uint64` emitted into
   each case body — no function-call overhead.
5. **Mod + validation after field read.** `renderMods` → `renderValidationOn`
   write directly into parent buffer; `renderValidationOn`'s `posVar`
   param emits right return shape inline (`return result, err` top level,
   `return result, i, err` mid-stream).
6. **Pointer fields** emit 4-byte `null` peek → nil branch, else stack-local
   `var v <Pointee>` + recursive inner read + `&v`. Dispatch order in
   `renderField` = pointer-first → string-tag → kind switch (string-tag-first
   would emit broken `result.X = *int(n)`); pointer-first recurses with
   `inner.GoType = PointeeType`.
7. **Cross-package struct fallback (statically dispatched).** Method-set
   membership checked via `go/types` at codegen time; emits single hardcoded
   call — zero runtime probes/itab lookups. Decode order: `DecodeFrom` →
   `UnmarshalJSON` → `UnmarshalText` → `encoding/json`. Marshal mirror:
   `AppendJSON` → `MarshalJSON` → `AppendText` (Go 1.24+, zero alloc) →
   `MarshalText` → `encoding/json`. When type info unavailable (AST-only
   loader, bare temp dirs in tests), emits plain `encoding/json` fallback.
8. **Inline map catch-all.** Unknown keys absorbed into a `map[string]V`. V
   dispatches: `any` → `scan.Any`/`s.Any`; `string` → `scan.String`/`s.String`
   (alias-mode on bytes); ggen-annotated struct → `T{}.DecodeFrom`/
   `T{}.DecodeFromStream`; anything else → `scan.SkipValue` + `json.Unmarshal`
   over the captured span.
9. **Marshal output cap.** `JSONSize()` upper bound → single
   `make([]byte,0,cap)` + `AppendJSON`. 1 alloc per top-level Marshal.
10. **Recursive nested-container emitter.** `emitByteSliceRead` /
    `emitStreamSliceRead` / `emitAppendSlice` / `emitSizeSlice` take depth
    param and unify slice+array. When `ElemKind` = KindSlice/KindArray they
    recurse via `peelSliceField(f)` + `stripOneContainer(typ)` (strips one
    `[]`/`[N]` off `ElemType`, shifts `InnerValidation[0] → ElemValidation`,
    `[1:] → InnerValidation`). Arrays carry N via `ElemArrayLen` for
    strict-count at every level. All locals (`kN`, `evN`, `errN`, `iN`, `vN`,
    `_idxN`) carry depth suffix.
11. **Map-key mods + validation.** `keyValidateAndMod` runs right after key
    read (before `:`), so key rules/mods short-circuit invalid keys before
    value decodes.
12. **Marshal error propagation.** `AppendJSON` returns `([]byte, error)`;
    threaded through every nested call (struct/slice/map AppendJSON, cross-pkg
    JSON/Text Marshaler, TextAppender, `json.Marshal`). Pure-primitive structs
    declare `var err error; _ = err` (compiler elides it — no runtime cost).
13. **Typed validation errors + frozen OneOf slices.** Each rule has own
    pointer-receiver error struct (`MinLenError`, `OneOfError`, …); generator
    emits typed literal at failure site. `OneOfError.Allowed` points to
    deduped package-level frozen `[]string` (`var _oneof_N = []string{...}`)
    emitted once per unique allowed-set. `EqError`/`NeqError` use `Want any`
    (one struct for string + numeric). Every error also carries a `Pos int`
    (first field) stamped at the failure site — the byte offset relative to the
    full payload: bytes cursor `i`, or `s.Offset()` on the stream path (NOT the
    raw buffer-relative `s.Pos`, which the compacting window invalidates).
    Injected uniformly by `withPos`/`posLit` (wraps `renderValidationOn`'s
    `onErr` + the standalone required / array-len / dup-key / unknown-key
    literals). See `decode/validation/CLAUDE.md`.
14. **Parse-error wrapping at every error return.** Codegen embeds the
    JSON field name as a compile-time literal in each `return result, …`
    site: `return result, i, decode.NewParseErr("street", i, err)`. The
    field literal comes from `/*ggen-field:EXPR*/` marker comments
    emitted by the dispatch (one per known-field branch, `key` /
    `strings.Clone(key)` for unknown-key handlers, empty reset at
    dispatch-loop boundaries). After full-body rendering, a textual
    post-pass (`wrapErrReturns` /
    `rewriteErrReturnsBody` in `parseerr_postpass.go`) walks the body,
    tracks the active marker, wraps every non-nil error return, and
    strips the markers. Zero runtime cost on the happy path — no defer,
    no `_field` state var, no extra call when err is nil. `NewParseErr`
    constructs `*decode.ParseError{Field, Pos, Err}` for raw sentinels,
    passes `validation.Error` / `Errors` through untouched, and
    **chains** when err is already a `*ParseError` — prepending the
    outer field so a nested struct surface ends up with paths like
    `addr.street`. `errors.Is(err, scan.ErrBadString)` keeps working
    via `Unwrap()`. `ParseError.Error()` sizes off the fixed prefix and
    calls `e.Err.Error()` exactly once so chained prints stay linear.
15. **Constant-folded `JSONSize()`.** Each field size splits into
    compile-time constant (folded into `size := N`) and runtime expression
    (loops, len(), recursive calls). Pure-primitive structs collapse to
    `return N`.
16. **Opening-quote folding.** At struct-field top level, when value emit
    begins with `"` (string, URL, big.Rat, time/RFC3339, duration/units,
    base64/hex bytes, net.IP/netip), opening quote folds into constant
    `"key":` → `"key":"`; value emitter writes only body + closing `"`.
17. **First-element-then-rest slice loop.** First element emitted directly (no
    leading comma), iterate `slice[1:]` with comma-prepend — lifts per-iter
    `if i > 0` out of loop.
18. **`bytes.IndexByte` string scan.** `scan.String` / `(*Stream).String` /
    `KeyView` / `skipString` locate closing `"` via `bytes.IndexByte`
    (SIMD), then second IndexByte — bounded to the closing quote — detects
    any preceding `\` (unbounded probe in `skipString` made `SkipValue`
    quadratic on buffered payloads; fixed). Wins on long strings; truncated
    `\u…`/trailing `\` falls through to `stringSlow` → `ErrBadString`.
19. **Empty-container peek bypass.** Slice/map decode peek for `]`/`}` before
    allocating — empty `[]`/`{}` keep field nil, skip `make`.
20. **Adjacent-constant-append coalescing.** Post-render peephole over
    `renderAppendJSON` merges adjacent `dst = append(dst, ...)` lines whose args
    are all compile-time byte literals into one append (`,"key":` + `[` →
    `,"key":[`; trailing `]` + return `'}'` → `return append(dst, "]}"...),
nil`). Single-byte → `'X'`, multi-byte → `"…"...`. ~5% on struct-heavy
    Marshal.
21. **nil slice/map → JSON `null`** (accepted on decode). Stdlib v1/v2 parity:
    nil → `null`, empty non-nil → `[]`/`{}`. Decode accepts `null`, leaves nil.
    Fixed arrays don't accept `null`. JSONSize budgets nil-as-null case
    directly — slice/map reserve 4 bytes (`null`) not 2; `sql.Null*` widens its
    inner constant to `max(inner, 4)`; arrays keep 2 (can't be nil). ~4% on
    Marshal but required for parity.
22. **Slab-allocated `[]*T` / `[N]*T` decode (depth-1 only).** Multi-level
    pointer elements route through the per-element cascade instead. Slices:
    one backing slab
    `make([]T,0,cap)`, append element pointers as `&_slab[len-1]`. Arrays:
    `make([]T,N)` (heap, exact-sized — stack `[N]T` would escape via
    `&_slab[i]`). N per-element heap allocs → ~log(N) (slice) / 1 (array). When
    slice slab grows past cap, prior `*T` keep orphan backing alive
    (~2× worst-case memory, no per-element alloc storm). Null elements skip
    slab (nil pointer).
23. **`preallocCap` returns `(slice, slab int)`** — one switch over `f.ElemKind`
    decides both `make([]E,0,slice)` and `make([]T,0,slab)`. Defaults absent
    explicit hint: `[]*T` both `defaultPreallocCap` (slab slot = sizeof(T) —
    avoids orphan-trail growth); `[][]T`/`[]map` slice=default, slab=0 (bounded
    element slot); `[]T`/`[][N]T` both 0 (element could be huge — prealloc ×
    elemsize would explode heap); primitive slice=default clamped by maxlen,
    slab=0. Empty `[]` always emits `result.X = []T{}`; prealloc only in
    non-empty arm.
24. **Stream key dispatch via `Stream.KeyView`.** Object-field keys read once,
    matched, discarded. Old `_s.String()` allocated heap string per key
    (~200 throwaway allocs/value); `KeyView` aliases on happy path (alias
    stays valid through buf growth — GC pins old backing). Falls back to
    `stringSlow` for escapes. Keys never escape dispatch frame. See
    `scan/CLAUDE.md`.
25. **`peelSliceField` initializes `HintLen=-1`.** Nested-slice recursion used
    to inherit Go's zero `HintLen=0`, which `preallocCap` reads as "opt-out",
    so every nested row started cap=0 and walked the 1→2→4→8 chain. Now `-1`
    ("unset") falls through to kind defaults. Biggest alloc cut in residency
    work — Matrix `[][]int` inner rows 494k → 274k allocs/1000 iters.
26. **Bitmask seen-flag tracking for wide structs.** Per-field `bool` locals
    for ≤32 fields (default threshold); above that `var _seen uint64`
    (or `[N]uint64` for >64) cuts frame from N bytes to 8/⌈N/64⌉. Wins only
    on wide + recursive structs; below threshold, bools stay.
27. **In-place decode for every elem kind.** Slice/array elem decode writes
    directly into final slot: `[N]*T` → `_slab[ivar]`; `[N]T` → `dst[ivar]`;
    `[]*T` → pre-grow `append(_slab, zero(T))`, target `_slab[len-1]`; `[]T` →
    pre-grow `append(dst, zero(T))`, target `dst[len-1]`. Structs: bytes path
    `var _n int; slot, _n, err = slot.DecodeFrom(data[k:]); k += _n`; stream
    path `slot, err = slot.DecodeFromStream(s)`. Primitives: slot = assign
    target (`slot = _bv`, `slot = int(_n)`). No `var ev0`/`_z`/`_sv`, no
    post-decode `dst[ivar] = ev0`. `inlineScanInt64`/`Uint64` receive
    `target`+`castFn`. Pre-grow uses `zeroLit` (`""`/`false`/`0`/`T{}`).
28. **Position-var pass-through; no `kN := posVar` alias.** Slice/array decoders
    thread caller's position var directly (top-level `j`, parent's `k`).
    Each inner advances SAME counter; outer continues from it. Only data
    locals (`evN`, `_idxN`, `_slabN`) keep depth suffixes.
29. **Inline `null` peek; no `_np`/`_ok` locals.** 4-byte `null` check emitted
    byte-by-byte inline at call site. Via `inlineNullPeek(posVar)` in
    `generate.go`.
30. **Single position cursor in dispatch loop.** No separate `j := i` cursor —
    every step (key scan, colon, value decode, comma/`}`) advances `i` directly.
    Stream path mirrors via `s.Pos`.
31. **Single local in `inlineScanString` (`ke` only), emitted brace-less.**
    Start = `posIn+1` inline; slow-path fallback reads from unchanged
    `posIn`. Slice expr `data[posIn+1:]` len `ke - posIn - 1`. `ke` lands in
    the caller's scope — every call site owns its scope (case body, elem
    loop, `{ var s … }` wrapper); only renderMap's value scan adds explicit
    braces (the key scan in the same loop body already declared `ke`;
    shadowing across nested scopes is fine). renderBytes likewise emits no
    wrapper block of its own — `renderBytesValue`'s `{ var s … }` is the
    only layer.
32. **Concrete-type fast paths in `AppendAny` for typed primitive slices/maps.**
    See `encode/CLAUDE.md` for ordering. 32-entry wins: `map[string]int`
    4403→1579 ns/op (71→7 allocs); `map[string]bool` 3449→944; `map[string]
float64` 6459→3417. Outpaces stdjson v1 and jsonv2 on every map shape.
33. **`AppendAny` concrete cases for `json.RawMessage`, `time.Time`,
    pointer-to-primitive.** Concrete cases pre-empt `json.Marshaler` branch
    / `reflect.Pointer` fallback. Wins vs jsonv2, 32-byte shapes:
    `json.RawMessage` 227→28 ns/op (8.1×); `time.Time` 181→117; `*int` 70→26;
    `*bool` 83→19. Concrete cases MUST sit before interface dispatches.
34. **Dispatch-level `null` peek breaks, not nests.** A field-level null match
    inside the key-dispatch switch ends with `break` (straight to the comma
    handling) instead of wrapping the whole value decode in an `else` —
    pointer/slice/map/`[]byte` kinds, bytes + stream paths. Gated by
    `nullBreakOK` (`FieldInfo.AtDispatch` + no field-level `pipe:` value steps
    — a break would skip the post-value validateAndMod). Nested-slice
    elements get the same flattening via `FieldInfo.NullDone`: the PARENT
    element loop consumes the null (nil slot + duplicated elem-validation
    emit + comma handling + `continue`, mirroring the `[]*T` nil-elem fast
    path) and the inner emitter skips its own peek, so each recursion level's
    body sits one indent in, not two. Other nested emits (map values, alias
    bodies) keep the if/else.
35. **Omit-guard pointer peel on marshal.** `AppendJSON`'s
    omitempty/omitzero guard for a pointer field is exactly `X != nil`, so
    the value emit peels one pointer level (`renderAppendValue` on `(*X)`) —
    no dead `if X == nil { null }` rung inside `if X != nil`. Mirrors the
    pre-existing `renderSize` deref.
36. **Brace-less value emitters.** Decode value emitters write their locals
    straight into the caller's scope — no `{ … }` wrapper per value. Covers
    the slice/array/map emitters (locals depth-suffixed or loop-scoped),
    time/duration/netip/url/big*/raw/sqlnull/any/string-tag/struct/bytes,
    cross-pkg decode fallbacks, and the inline catch-all. Sound because every
    call site owns its scope (one field per case body, element loops, the
    pointer-leaf else); shadowing across nested scopes is legal. Stream
    emitters renamed their `var v` temps (`sv`/`f`/`u`) — a pointer-leaf
    caller declares `var v <leaf>` in the same scope, and the old shadowed
    `v` blocks were broken codegen for `*time.Time format:RFC3339`-style
    stream fields. Inline int/uint scanners are brace-less too
    (`neg`/`limit`/`u`/`n` — one numeric scan per scope everywhere). Locals
    that would collide get unique names instead of a scope: renderMap's
    value string scan uses end-var `ve` (`inlineScanStringVar`; the key scan
    owns `ke`), map marshal's first-entry flag is `first<GoName>`, encode
    cross-pkg temps are `b<GoName>` (fields emit at function scope). The one
    brace kept: the slice-elem `bs := json.Marshal` encode fallback —
    first-element-then-rest duplicates it at outer scope.
37. **Map values decode straight into `m[mk]`.** No `mv`/`mn` temps —
    string/bool/int/uint/float and pointer values multi-assign the map index
    directly (`m[mk], i, err = scan.X(data, i)`; pointer cascade is
    assignment-only under TargetNil, so the unaddressable index is fine).
    Narrow numerics keep a wide temp for the cast; STRUCT values keep the
    fresh `var mv T` — direct decode would merge duplicate map keys in one
    payload instead of fresh-decoding each occurrence (stdlib parity).
38. **Named-result return slot.** `DecodeFrom`/`DecodeFromStream` emit as
    `func (recv T) DecodeFrom(data []byte) (result T, _ int, _ error)` with a
    `result = recv` prologue (receiver renamed `recv`; bodies still write
    `result`). The named first result homes the value in the caller's return
    slot, so every `return result, …` is register-set + RET with NO struct
    copy — vs an anonymous result, where each of the ~99 RET sites copied the
    full receiver-sized struct (264 B for Node). Happy path is copy-neutral
    (the one entry `result = recv` replaces the one exit copy); merge
    semantics unchanged (`recv` IS the merge source, exactly as the old
    `result` receiver was). Shrinks `Node.DecodeFrom` text −23% (33.8→25.9 KB)
    and `DecodeFromStream` −32% (40.0→27.1 KB); wall flat-to-marginal across
    3 `-randlayout` seeds (memory-latency-bound walk caps it), allocs/B
    identical. Applies to struct + alias renderers, both paths. `parseerr`
    post-pass unaffected (return text still `return result, …`).
39. **Bounded unchecked digit-accumulation prefix (bytes path).** The inline
    int/uint scanners (`inlineScanInt64Stmt`/`inlineScanUint64Stmt`) and the
    runtime `scan.Int64`/`Uint64` split the digit loop in two: the first ≤18
    (int) / ≤19 (uint) digits accumulate with NO per-digit overflow check —
    `10^18-1 < MaxInt64 < |MinInt64|` and `10^19-1 < MaxUint64`, so neither the
    `u*10+d` accumulation nor the value can overflow within the prefix. A 19th
    (int) / 20th (uint) digit, if present, resumes the original checked loop,
    keeping `scan.ErrNumberOverflow` identity and error position bit-identical.
    Cuts the hot per-digit body from ~5 to ~2 compare+branches; leading-zero
    runs that push significant digits past the window resume correctly in the
    checked tail (the prefix accumulates 0). Measured −1.0% to −1.7%
    `Mega_Unmarshal` consistent across 3 `-randlayout` seeds (one of the few
    CPU shaves that beats the memory-latency-bound walk — digit bytes are
    sequential + cache-hot). Accept-set unchanged (no grammar edge decision).
    Boundary lattice pinned by `TestInt64_OverflowBoundaryLattice` /
    `TestUint64_OverflowBoundaryLattice`. Stream `Stream.Int64`/`Uint64` keep
    the checked loop (mid-number ReadMore refill complicates the window) — a
    backlog follow-up.
40. **Whitespace-skip `<= ' '` exit gate.** `inlineSkipWS` and `scan.SkipSpace`
    prepend `data[i] <= ' '` to the 4-way WS test (`==' '||=='\t'||=='\n'||
    =='\r'`). On compact JSON the loop never runs and the dominant
    non-whitespace byte exits on ONE compare (gc lowers `<= ' '` to a single
    CMPB+JHI, flags reused by `== ' '`) instead of four. Boolean-identical
    accept set (every WS char is <= ' '; control bytes <0x20 fall through to
    the unchanged 4-way and still stop the scan). Measured ~−1% Mega_Unmarshal
    on 2/3 `-randlayout` seeds (flat on the third, never regresses) and a
    consistent −3..−7% on the dispatch-bound `Tiny_Unmarshal` across all
    seeds; allocs/B identical. The stream path needs no change — its `> ' '`
    fast path (opt #5 / `SkipSpace` two-tier) already is this gate.
41. **Stream `StringView` for transiently-consumed value strings.** A
    `Stream.String` sibling that returns an `unsafe.String` alias into
    `s.buf` (no `KeyView` scalar prelude — value strings are often long).
    Generated stream decoders use it where the value string is consumed
    before the next stream op AND retains no bytes past that point:
    base64/base32/hex `[]byte` (`AppendDecode` into independent dst),
    `time.Time`/`time.Duration` text formats, `net.IP` (error literal
    clones), `netip.Addr`/`Prefix`, `big.Float`/`big.Rat`, cross-pkg
    `TextUnmarshaler` (encoding contract forbids arg retention; bytes path
    already aliases). NOT `url.URL` (`url.Parse` slices its input into
    `Path`/`RawQuery`), plain `string` fields, map keys, or map/slice
    string elems — those outlive the scan and keep the `Stream.String`
    copy. `Stream.String` itself is now `StringView` + `strings.Clone`
    (shared scanner; −0.9% vs the old standalone body, allocs identical).
    Measured −1.45% wall / −2.95% B/op / −1.89% allocs `Mega_Reader` with 2
    aliasable Node sites; text-heavy schemas gain more. See
    scan/CLAUDE.md "Stream copies vs bytes-path aliases" / "`StringView`".
42. **Exact-cap comma pre-count for flat numeric/bool SLICES (bytes path).**
    Numeric/bool elements carry no `,` or `]`, so a one-shot `bytes.IndexByte`
    + `bytes.Count` over the value span yields the precise element count before
    any `make` (`scalarCountable` gate, `emitByteSliceRead` non-empty arm):
    `cnt := bytes.Count(data[k:k+e], ',')+1` → `make([]E,0,cnt)`, killing the
    1→2→4→8 growth chain and its orphan trailing backings with NO over-cap
    residency cost (opposite of the rejected maxlen-as-hint). Gates on
    `userPreallocHint < 0` (hintlen/len/minlen keep precedence) and a reused
    (non-nil) slot keeps its backing (the `if dst == nil` guard skips the
    make); applies at every nest depth via `peelSliceField`. String/struct/
    pointer elems excluded (delimiters inside quotes / nested objects). A
    malformed array with no `]` falls back to cap 1 and errors in the scan
    loop. Stream path has no full buffer — bytes only. **NOT done for maps:**
    JSON object keys are strings that may contain `:`, so a single VALID entry
    with a colon-laden key would inflate a `:`-count into a huge `make()` — a
    memory-amplification footgun on well-formed input (the slice case is immune
    — scalar elements can't carry the delimiter, so inflation needs malformed
    input that errors at once). Maps keep the unsized make(); robust runtime
    sizing for maps is the backlog "buffer-then-build" track. Measured (Mega
    Node, Matrix `[][]int`): Unmarshal **−10% wall, −36.6% allocs, −21% B/op**,
    robust across 2 `-randlayout` seeds; composes with #43. Marshal is the
    control — generated marshal code byte-identical, wall flat (a one-off
    seed-1 +4% was pure code-layout noise on the memory-latency-bound cold-tree
    walk).
43. **Nested-container slot hoisted into a reuse-seeded depth-local.** The
    recursive slice/array emitters used to thread the parent slot expression
    (`dst[len(dst)-1]` / `dst[ivar]`) into the inner element loop as its dst,
    so every inner append/decode re-evaluated that index (len()/bounds) and
    took a parent-backing write barrier per element. Now `rowN := <slot>`;
    recurse into `rowN`; one `target = rowN` publishes the finished row — the
    inner loop writes a local slice header (barrier-free), and the
    `len(len(len(…)))` nest disappears from generated output. **The row is
    seeded from the carried slot so its backing is reused** (decode-into-
    receiver): a slice-of-slice outer grows by reslicing within cap
    (`if len < cap { dst = dst[:len+1] } else { dst = append(dst, nil) }`)
    instead of `append(dst, nil)`, so the carried inner header survives into
    the slot; `rowN := dst[len-1]; if rowN != nil { rowN = rowN[:0] }` then
    resets len (cap/backing kept) and the inner make-guard (`if rowN == nil`)
    skips the make on reuse and allocates only past-cap/fresh — mirroring the
    top-level `[:0]` receiver-reset one level down. A `null` element nils the
    slot unconditionally (it may carry a reused header). Both bytes
    (`emitByteSliceRead`) and stream (`emitStreamSliceRead`) paths. asm-verified
    4→2 barrier sites; **−1.8% to −4.85% Mega_Reader/ggen_stream** (allocs flat
    — pure codegen win on fresh decode), folds into the bytes-path number
    above; nested-row backing reuse pinned by
    `TestMerge_nestedSliceBackingReused`.
44. **Byte-length-gated rune-count validation.** `runes`/`minrunes`/`maxrunes`
    each emitted an unconditional `utf8.RuneCountInString` walk (O(len)) — a
    waste when `len(ref)` alone settles the rule. For a B-byte UTF-8 string the
    rune count R satisfies `ceil(B/4) <= R <= B`, so cheap `len` checks resolve
    the common cases without walking (`emitRuneRule`, `generate.go`):
      - `minrunes=N`: fail-free `len < N`; pass-free `len >= 4N-3`; walk only the
        band `[N, 4N-3)`. (`N<=1` → just the `len < N` check, no band — a bare
        empty-string test.)
      - `maxrunes=N`: pass-free `len <= N`; fail-free `len > 4N`; walk band `(N, 4N]`.
      - `runes=N`: fail-free `len < N || len > 4N`; walk band `[N, 4N]`.

    The failure literal's `Got` reports the real count — a cold walk on the
    fail path, the live `rc` inside a band; the happy path on a long string
    walks zero times. **Tier (c) — ASCII subsumption:** if an ASCII-implying
    rule (`alphanum`/`numeric`/`ascii`/`hexadecimal`, `asciiImplyingRule`) has
    already PASSED earlier in the same validator run, every byte is one rune so
    `R == len` exactly and the walk is dropped entirely (direct `len`
    comparison). Gated on `asciiSeen && !multiErr` in declared order:
    position-sensitive (the charset rule must precede the rune rule) and
    skipped under multierr (a failed earlier rule there doesn't stop us reaching
    the rune rule on non-ASCII input, so `len != R`). `emitValRun` walks the run
    tracking `asciiSeen`; rune rules route to `emitRuneRule`, everything else to
    `renderOneVal`. Wire-identical — same accept/reject, error type, and `Got`.
    Measured interleaved core-pinned (`taskset -c 4`, `-cpu=1`, n=12): **flat on
    `ValidationHeavy_Unmarshal`** (short fields — the walk it skips is cheap) but
    **−46.8% `RuneGated_Unmarshal`** (p=0.000, +88% throughput, ~8 KB strings —
    2048 four-byte runes — that clear their bound via the pass-free `len` gate /
    tier-c, skipping a full multi-byte UTF-8 decode) — allocs/B byte-identical on
    both. Pinned by `TestGenerate_runeGates` (gates per rule /
    `minrunes=1` collapse / tier-c drops the walk / ascii-after and multierr keep
    it). Implements all three tiers of backlog [25].
45. **Bytes-path container-loop hygiene (wire-identical, generated-code
    shrink).** Three cleanups to the slice/array/map decoders, backlog [18]/[16]:
    (a) **no leading WS skip** in `emitByteSliceRead`/`renderMap` — every value
    entry already skips WS (after `:` at the dispatch, after `[`/`,` in element
    loops), so the container's own leading skip was redundant. The top-level
    alias path had no preceding skip (its dependency), so the explicit skip moved
    to `renderAliasContainerDecode`. Stream path KEEPS its leading `s.SkipSpace()`
    — it does double duty (WS + `ReadMore` buffer-ensure), not safely removable.
    (b) **collapsed empty-peek** — a non-pointer slice whose non-empty arm would
    also be just `dst = T{}` (no comma-count, no cap, no slab: `[]struct`,
    `[][N]T`) had two byte-identical empty-peek arms; emits one `if dst == nil {
    dst = T{} }` and skips the `data[i]==']'` peek. (c) **do-while element/map
    loop** — `for i<len && data[i]!=']' {…}` → `if i<len && data[i]!=']' { for
    {…} }`: the per-iteration re-check was redundant (entry guaranteed non-`]` by
    the peel; subsequent iterations by the post-comma `noClose` guard), and the
    one-time guard preserves the empty-`[]`/truncation `ErrBadArray` path. Flat
    by design (the skipped work is ~1 compare/container on compact input) — the
    payoff is smaller generated code. Pinned by `TestWhitespace_Tolerance`
    (decodes WS-laden payloads across slice/nested-slice/map/array/nested-struct
    + top-level aliases, vs the compact form) and the bytes-vs-stream fuzzer.
    (d) **Top-level alias early-return** — a container ALIAS (`type Tags
    []string`) is the whole value, so the bytes emitters take a `topLevel` flag
    (true only from `renderAliasContainerDecode`) and `return result, i, nil` at
    each exit (null branch, array/map close) instead of falling through to a
    trailing return, which is then dropped as dead code. Zero runtime (the
    fall-through compiled identically) — purely cleaner generated source. Struct
    fields / nested elements pass `topLevel=false` (their caller has more to
    parse); stream path keeps its trailing return.

## Design decisions (the why)

1. \*\*`unsafe.String` boosts perf by avoiding GC pressure. Can backfire
   if parsed strings referenced long-term.
2. **Struct fields sorted alphabetically at codegen time** (default). Zero
   runtime cost; deterministic, compresses better.
3. **No runtime reflection anywhere.** Even cross-package fallback uses
   `encoding/json.Unmarshal` only for types NOT in generation pass.
4. **Custom validators / mods / converters = codegen-time function injection.**
   `pipe:` steps like `@EvenOnly` / `@Squash` (and `@Conv` decode variants)
   resolved via `packages.Load` at parse time — looked up, classified by
   signature, type-checked against the working type, emitted as a direct call.
   No runtime registry, no `func(any) any` boxing, zero alloc. Cross-package via
   `@pkg.FuncName` through source file imports; blank imports
   (`_ "path/to/lib"`) work. Validator errors wrap as
   `validation.CustomError{Name, Value, Cause}` (or `PredicateError` for bool form);
   fallible mod errors propagate as parse errors (`ModError` for bool form).

## Test files (`cli/` module)

Cover CLI itself, live under `cli/`. Per-package runtime tests live next to
implementation (`encode/`, `scan/`); feature/roundtrip/compat/fuzz under
`integrationtests/`; benchmarks under `bench/`.

- `parse_test.go` — annotation/tag/rule parsing, cross-package symbol
  resolution. Hosts test-only `generate(pkg, structs) ([]byte, error)`
  wrapper (production calls `generateTo` against destination `*os.File`).
- `tags_test.go` — `json:` tag parser. `pipe:`/`hint:` parsing (incl. inner/
  keys/group/variants) is in `pipe_test.go`.
- `applicability_test.go` — rule-applicability matrix.
- `cli_test.go` — CLI integration: binary built in TestMain, file-naming
  contract, `./...` walk + dot/underscore-dir skip, per-flag effects on output.
- `bench_test.go` — `BenchmarkGenerate` over representative fixture.
- `log_test.go` — Logger level + sink behaviour.

## How to regenerate

Build binary into project dir (`./ggen`), never `/tmp`.
Binary git-ignored; in-tree builds keep it discoverable, avoid
cross-session collisions, match test harness path.

```sh
go build -o ggen ./cli
./ggen ./decode/... ./encode/... ./scan/...
easyjson bench/types.go
GOEXPERIMENT=jsonv2 go generate work
```

Binary builds from the `cli/` module to project-root `./ggen` (so the
`../ggen` references in `bench/` and `integrationtests/` still resolve).
ggen module-scoped — `./...` visits ONLY the invoked module's packages.
`cli/`, `bench/`, `integrationtests/` each carry own `go.mod`, must be
regen'd from inside (one invocation per module), like `go build ./...`. In
`integrationtests/`, each annotated source carries `//go:generate ../ggen $GOFILE`
and emits sibling `<file>_ggen_test.go`.

## Working with CLAUDE.md

**ALWAYS** keep this file up-to-date after changes to: CLI/annotation flags,
codegen behaviour, wire format, generated method surface, etc.

Benchmark numbers → `bench/CLAUDE.md`. Test-suite layout →
`integrationtests/CLAUDE.md`. Per-package runtime details → matching
package CLAUDE.md. Backlog / tried-and-rejected → `.claude/backlog.md`.

### Sibling docs that MUST also be kept current

Every change touching user-visible surface must propagate to **both**
`README.md` and `SKILL.md`:

- **CLI flag changes**
- **Annotation changes**
- **Field tag syntax change**
- **New Go kind / wire-shape change**
- **New runtime API**
- Etc.

Bundle doc update in same commit.

CLAUDE.md = implementation detail doc (the _why_).
README + SKILL = surface (_what_/_how_).
All three move together.

## README.md authoring rules

**NEVER spill technical / implementation details into README.md.** README =
user-facing front door: what ggen is, what it does, how to use it, what
numbers mean. CLAUDE.md = where implementation detail lives.

Do NOT add to README: runtime/harness mechanism (`runtime.GC()` cycles,
`HeapInuse`, `b.RunParallel`, sink merge, `b.ResetTimer`, …); internal codegen
detail (`unsafe.String` aliasing, slab heuristics, `KeyView` vs `String`,
`preallocCap` shape, `peelSliceField`, …); pprof internals.

DO put in README: what each benchmark measures (one sentence); how to read each
metric; when user would care; bench table + interpretive paragraph;
caveats affecting user's choice (e.g. "strings alias the input, don't
mutate after decode").

If you write "internally", "implementation", "under the hood", or name
private function / runtime API in README — stop. Belongs in CLAUDE.md or
code comment.

## Backlog

See @.claude/backlog.md
