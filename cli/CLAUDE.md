# ggen CLI — generator / codegen surface

The `cli/` module (`github.com/sirkostya009/ggen/cli`, package `main`) is the
code generator: it parses annotated Go structs and emits their
`DecodeFrom`/`DecodeFromStream`/`JSONSize`/`AppendJSON` methods. This file
documents the **CLI / codegen surface** and the _why_ behind generated-code
shape. The CLI does NOT import the runtime packages — it emits their import
paths as string literals into generated code.

## Files

- `main.go`, `parse.go`, `generate.go`, `tags.go`, `types.go` — CLI (package
  `main`); `tags.go` = json tag only
- `pipe.go` — `pipe:`/`hint:` grammar (tokenize, `ParsedPipe`, `Step`/`Variant`,
  `deriveBuckets`)
- `variants.go` — multi-shape decode dispatch codegen (`/` variants)
- `introspect.go` — go/types interface detection (TextAppender, TextMarshaler, …)
- `alias.go` — alias-type code emitters (decode + AppendJSON)
- `applicability.go` — parse-time rule/kind compatibility matrix
- `customfunc.go` — `@Func` resolution + signature classification (validator/mod/converter)
- `check.go` — `-dry` / future-ggenvet parse-only entry points
- `log.go` — `cliLog`: leveled logger with deferred flush
- `parse_test.go`, `tags_test.go`, `pipe_test.go`, `applicability_test.go`,
  `cli_test.go`, `log_test.go` — CLI tests; `bench_test.go` = `BenchmarkGenerate`
  (generator perf only)

## Generator CLI (`main` package)

### Invocation

```
ggen ./...                    every package matched by the pattern (module-scoped, as `go build`)
ggen <dir>                    one package
ggen <dir> ./sub/... <dir2>   several targets in one run (each processed, like `go build`)
ggen <file.go> [Names...]     one file; optional struct name filter
```

**Every positional is a target** in dir/pattern mode. They used to be silently
dropped after the first — so the repo's own
`ggen ./decode/... ./encode/... ./scan/...` regen line only ever visited
`decode`. A leading FILE still takes the rest as a struct-name filter (the one
shape where trailing args are names, not targets); a file in any later
position is a loud error, and `-o` is rejected with multiple targets.

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

| Flag             | Effect                                                                                                                                                |
| ---------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------- |
| `-o <path>`      | override output path (single file / single dir only)                                                                                                  |
| `-pkg <name>`    | override package name in output                                                                                                                       |
| `-marshal`       | emit `MarshalJSON` method                                                                                                                             |
| `-unmarshal`     | emit `UnmarshalJSON` method                                                                                                                           |
| `-multierr`      | accumulate validation failures into `validation.Errors`, returned at end of parse; parse errors still return immediately. The drain past a NESTED decode is gated on the callee being multierr too (`multiErrTypes` / `calleeDrains`): a single-error callee returns mid-value, so continuing would resume from a desynced cursor — the inner object's remaining keys used to surface as the PARENT's unknown keys |
| `-allowdups`     | allow duplicate keys, first-wins (later skipped). Default: `validation.DuplicateKeyError`. NOTE the check is scoped to DECLARED keys (the per-field `seenX` flags) — dups inside skipped / `any` / raw / nested scopes are NOT detected, a decided divergence from jsonv2 (see backlog Tried Rejected) |
| `-novalidate`    | skip validation rules, required-field checks, mods                                                                                                    |
| `-ignoreunknown` | silently skip unknown JSON keys. Default: `validation.UnknownKeyError`. Overridden when an inline map field is present                                |
| `-nullzero`      | accept explicit JSON `null` on every non-pointer value field → Go zero. Default hard-errors (see null kind-gating). No-op on already-null-aware kinds |
| `-nosortkeys`    | emit fields in Go declaration order. Default: alphabetical. Inline map fields stay last                                                               |
| `-usenumber`     | decode JSON numbers into `any` fields as `json.Number` instead of `float64` (mirrors stdlib `UseNumber()`)                                            |
| `-htmlescape`    | opt INTO HTML-safe escaping (`<`, `>`, `&` → `\uXXXX`) on marshal. Default = literal                                                                  |
| `-allowinvalidutf8` | skip decode UTF-8 validation (opt #50) for every struct in the pass: string scans pass `validate=false` (raw bytes through, surrogates → U+FFFD), inline windows/classify revert to the pre-validation shapes, raw-span `CheckUTF8` not emitted. Decode-only |
| `-copy`          | bytes-path `DecodeFrom` copies retained strings / map keys+values / slice elems / `json.RawMessage` / any-embedded strings out of `data` instead of aliasing it. Decouples decoded values from the input buffer (matches the stream path's lifetime). Decode-only; alloc-heavier |
| `-dry`           | parse + validate annotated structs, surface every error, emit no file. Composes with `-v`. Rejects `-o`/`-pkg`                                        |
| `-simd <tier>`   | `off`/`avx`/`avx2`/`avx512` — bytes-path string-scan tier (see opt #46). Resolved by `resolveSIMD` (main.go): `GOEXPERIMENT=simd` in ggen's OWN env auto-selects `avx`; `avx`/`avx2`/`avx512` error without the env var (emitted code imports `simd/archsimd`, which only exists under the experiment). Generate-time only — sets `scanStringFn` (`"scan.String"` → `"scan.StringAVX2"` etc); no per-struct annotation |

### Per-struct annotations

A comment on a struct (or gen-decl) `//ggen:generate` (no space after `//`,
mirrors `//go:generate`) followed by space-separated tokens. Apply only to the
annotated struct:

`marshal`, `unmarshal`, `multierr`, `allowdups`, `novalidate`, `ignoreunknown`,
`nullzero`, `nosortkeys`, `usenumber`, `htmlescape`, `copy`, `allowinvalidutf8`.

## Struct tags (on fields)

Field config is partitioned by role across three tags: `json:` (wire shape),
`pipe:` (decode→transform→validate pipeline), `hint:` (prealloc).

### `json:`

- `json:"name"` — JSON key name (field is ignored otherwise). jsonv2 quoting:
  options split on commas OUTSIDE single quotes, so `json:"'a,b'"` names the
  field `a,b` and `format:'Jan 2, 2006'` survives its comma; `\'` = literal
  quote (`parseJSONTag`/`splitTagOpts`, tags.go)
- `json:"-"` — field explicitly ignored. `-` with options is a parse ERROR
  (jsonv2 parity — v1 read it as a field named `-`; use `json:"'-'"` for
  that). Empty options (`a,`, `a,,x`) also error; unknown option words pass
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
      `gt/gte/lt/lte/eq/neq=N`; `multiple=N`; `oneof=a|b|c`; `url`/`alphanum`/
      `numeric`/`hexadecimal`/`islower`/`isupper`; `starts/ends/contains=X`.
      (Bare `lower`/`upper` DIED in the 2026-08 split — they were ambiguously
      documented as both validator and mod while the parser always picked mod;
      now `tolower`/`toupper` transform and `islower`/`isupper` validate, and
      the old names error with a migration hint, `renamedCaseHint`.)
    - mods: `trim`, `tolower`, `toupper`, `trimleft=X`, `trimright=X`,
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
  `@Conv` converter INPUT types (`W` in `func(W) T`) are peeled of every
  pointer level before the foreign-package and container checks run — a
  `*[]int`/`*other.T` input used to dodge both — and `chan`/`func`/interface
  inputs are rejected outright alongside the existing slice/array/map
  rejection (none have a wire shape a converter call site can scan into).

**Lexing/quoting** (`tokenizePipe`): steps are WS-separated; structural glyphs
`/ ~ ( )` are significant with or without spaces (plus the `inner:`/`keys:` word
prefixes); a value/message may be single-quoted, required only when it contains
whitespace; a literal quote is `\\'` in SOURCE (the tag value is a
double-quoted Go string, so `reflect.StructTag.Get` unescapes it to `\'`
for the lexer — a bare `\'` makes Get return "" and every rule in the tag
vanish, which `checkTagReadable` now rejects at parse time; `checkTagReadable`
itself uses `Lookup`, not `Get`, so the legal empty `json:""` no longer
false-positives as unreadable). An unterminated `'` (no closing quote before
end-of-tag, e.g. `oneof='New York|LA`) is a parse ERROR — it used to be
silently auto-closed, which changes the rule's semantics (`'New York|LA'`
reads as one `oneof` part instead of two). The tokenizer
PRESERVES `\'` inside quoted spans; unescaping happens downstream in
`stripQuotes`/`splitPipeParts`, after the part split — unescaping earlier
handed `splitPipeParts` a bare quote it read as a delimiter toggle.
Multi-part values (`oneof`/`replace`/
`clamp`) quote per PART: `parseStep` skips the whole-value strip for them and
`splitPipeParts` splits on `|` OUTSIDE quotes then strips each part — so
`oneof='New York'|LA` protects the space and `replace='a|b'|c` a literal
pipe (a naive whole-strip + split used to leak quote chars into the allowed
set). `replace`/`clamp` require exactly 2 parts.

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
Cases covered in `TestCLI/InvalidRuleApplication`. KindStruct positions no
longer skip: `checkRuleApplicability(fi, resolved)` DEFERS opaque kinds on the
early AST pass and REJECTS them on the go/types re-run (`resolved=true`, after
`NamedPrims` resolves named primitives to their underlying kind) — the render
paths emit uncompilable comparisons (gt/trim on a struct) or, worst, NOTHING
(eq/neq) for a genuine struct kind. `required`/`optional`/`@Func` stay
kind-agnostic. AST-only mode keeps the historical skip (no type info to judge
by).

Numeric bound VALUES (`gt/gte/lt/lte/eq/neq`, `oneof`'s numeric parts,
`clamp`'s lo/hi) are also range- and sign-checked against the field's
declared kind (`boundFits`/`kindIntBits`), not just parsed as a valid
number — a bound literal is pasted verbatim into a Go comparison against the
field, so `uint gte=-1` or `int8 lte=300` used to pass parse and then fail
the GENERATED build with a constant-overflow error. `eff()` (the
pointer/named-primitive kind resolver every rule check goes through) strips
leading `*` before the `NamedPrims` lookup — `NamedPrims` is keyed by the
pointee's bare spelling, so a `*Priority` field used to miss the lookup
entirely and its rules bypassed the matrix rather than being checked against
the underlying primitive kind.

## Generated methods (per annotated struct T)

```go
// DecodeFrom is a zero-copy parser. Strings and RawMessage are aliased into data
// (unless -copy / //ggen:generate copy — then they are copied out, decoupling
// the result from data at the cost of per-string allocs, like the stream path).
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
every scan primitive. To capture a raw span (RawMessage, json.Unmarshal fallback,
big.Int): `span, err := s.CaptureValue()` — grows the window to buffer the whole
value, returns a buffer alias (copy it if retained; `json.Unmarshal`/`SetString`
consume it in place). Replaced the old `Shift=false` + `s.Bytes()[start:s.Pos]`
slice dance — see scan/CLAUDE.md.

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
- fixed arrays `[N]T`: every slot overwritten or strict-length-errors; no entry
  reset. A MERGEABLE element is blanked before the element decode (`dst[i] =
  T{}` for a struct elem; the multi-level pointer cascade builds a fresh chain
  via `TargetNil`) — decoding a struct in place through its value receiver
  otherwise MERGED the carried slot, so `[2]Inner` kept fields the payload
  omitted while `[]Inner` and `[2]*Inner` decoded fresh. jsonv2 zeroes; pinned
  by `TestMerge_ArraySlotsOverwrite`

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
`scan.ErrBadArray`/`ErrBadObject`, wrapped in `decode.NewParseErr` with field +
cursor on both paths (`emitNoCloseAfterComma` /
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
  `_s.X`, cast to alias. `htmlescape` flips the string-append helper. Float
  marshal routes through `encode.AppendFloat` (stdlib-parity `'f'`/`'e'`
  selection, errors on NaN/Inf) — it used to be a bare
  `strconv.AppendFloat(…, 'g', -1, …)`, which silently emitted the literal
  text `NaN`/`Inf` (invalid JSON, nil error) and used `'g'` formatting that
  diverges from every other float site in generated code (`1e6` vs `1000000`)
- **struct** (`type LocalUUID uuid.UUID`): methods don't propagate from the RHS,
  so probing uses `inspectType` on the RHS named type. Three-step ladder:
    1. _ggen-method delegation_ — if underlying has AppendJSON+DecodeFrom: cast →
       method → cast back (cheapest)
    2. _field introspection_ — plain struct with ≥1 exported field: walk
       `*types.Struct`, synthesize FieldInfo per exported field
       (`extractFieldFromTypes`), `IsAlias` flips false, regular struct codegen
       runs (field access via `result.X` is sound — identical layout). **Preferred
       over JSON/Text marshaler delegation even when those exist** — hand-rolled
       codegen beats reflective marshaler calls
    3. _JSON/Text marshaler delegation_ — opaque struct (no exported fields, e.g.
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
- `int`/`int8`/`int16`/`int32`/`int64`, `uint`/`uint8`/`uint16`/`uint32`/`uint64`,
  plus the builtin aliases `rune` (= int32) and `byte` (= uint8). Both resolve
  in `resolveKind`; without that a `[]rune` element fell to KindStruct and
  emitted `append(dst, rune{})` — an accepted annotation whose output did not
  compile
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
  `[]byte` stays KindBytes (base64), and `[N]byte` folds onto the SAME base64
  path (`foldByteArray`, parse.go) with a strict decoded-length check —
  jsonv2 base64s byte arrays and rejects the v1 number-array form, so ggen
  sides with v2 as everywhere else the two disagree. `format:array` opts back
  into the v1 tuple of numbers. Only non-byte arrays get tuple treatment by
  default

## Wire-format divergences from stdlib

Two kinds intentionally diverge from `encoding/json` v1 + v2. ggen marshal output
is _not_ a subset of either for these — feeding through stdlib reshapes the value,
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
2. **Slice cap from tag hint, then ELEMENT WIDTH.** `preallocCap` picks the
   initial cap for `make([]T,0,N)`. The 512 is the runtime's
   `gc.MinSizeForMallocHeader` (`goarch.PtrSize * goarch.PtrBits` = 8×PtrSize²,
   so 512 on 64-bit and **128 on 32-bit** — the emitted constant derives it
   from `unsafe.Sizeof(uintptr(0))` rather than hardcoding, and cross-builds
   for 386 clean). It is a go1.22 allocation-headers boundary, NOT a Green Tea
   one: staying under it means no 8-byte malloc header, which `roundupsize`
   adds *before* the size-class lookup — measured, a pointerful element at
   512 B allocates 512, at 576 B allocates 640. Green Tea (default from 1.26)
   reuses the same boundary for `gcUsesSpanInlineMarkBits`, but that half does
   not apply to NOSCAN backings (`tryDeferToSpanScan` fast-tracks them), which
   is most of what a decoder allocates. Precedence: `hintlen=N` > `len=N` >
   `minlen` > `maxlen=N` (only when `N × sizeof(E) <= 512`, since an exact
   upper bound beats a width guess and cannot over-allocate past what the
   payload may legally contain) > width ladder. That is the ONE narrow
   rehabilitation of maxlen-as-prealloc, which the backlog killed for
   unbounded retained over-allocation: the byte gate caps the damage at the
   same 512 bytes every slice already budgets. `Tags []string maxlen=64` →
   1024 B, refused, ladder gives 4; `Matrix [][]int maxlen=16` → 384 B,
   accepted, cap 16. The tail used to be a kind-blind
   `defaultPreallocCap = 4` for primitives and **0** for struct elements
   ("sizeof(T) unbounded; start nil, grow"); it is now derived from the element
   width, which the compiler knows: as many elements as fit strictly under
   80 bytes (go1.27 fast-alloc), else within a 512-byte Green Tea span, else 1.
   Spec + tests live in `decode.PreallocCap`; the emitted form is a
   package-level `const ggenCap_<prefix>_<n>` holding the same ladder written
   branchlessly over `unsafe.Sizeof(*new(E))`.
   **It has to be a constant, not a call**: measured on the bench module, gc
   inlines `decode.PreallocCap` only into small functions — 30 of 34 emitted
   sites kept a real `CALL`, because the enclosing generated `DecodeFrom` is
   thousands of nodes past the inliner budget. As a constant expression it
   folds in the frontend (`MOVL $2`), where the inliner never gets a vote.
   Maps keep `mapPreallocCap` (no byte budget to reason about).
   Measured (deterministic columns; wall clock on this box floats ±10% between
   builds — MapHeavy, whose code does not change, moved 12%):
   **Mega −15.1% allocs** (54990 → 46706) / +3.3% B/op, wall −4% across three
   passes; TMS import −7% allocs / +17% B/op; DeepNested (a 50-level chain
   whose every `[]Node` holds exactly ONE element, and whose `maxlen=16` ×
   256 B overshoots the span so the ladder stands) **+2× B/op** — the "at
   least 2 elements" floor is a deliberate memory-for-allocations trade, and
   `hint:"1"` opts a known one-element slice back out.
3. **Field marshal order sorted by JSON name** (alphabetical) at codegen time.
   `-nosortkeys` opts back to declaration order.
4. **Inlined scan primitives in hot path.** Raw byte-compare loops for
   `SkipSpace`, `String`, `Int64`, `Uint64` emitted into each case body — no
   call overhead.
5. **Mod + validation after field read.** `validateAndMod` → `renderPipe` (mods +
   validators interleaved in declared order, dispatching to `renderOneMod`/
   `emitValRun`) write into the parent buffer; `posVar` emits the right return
   shape inline.
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
9. **Marshal output cap.** `JSONSize()` upper bound → single `make([]byte,0,cap)` +
   `AppendJSON`. 1 alloc per top-level Marshal.
10. **Recursive nested-container emitter.** `emitByteSliceRead`/
    `emitStreamSliceRead`/`emitAppendSlice`/`sizeSliceContrib` take a depth param and
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
    `posLit`. See `validation/CLAUDE.md`.
14. **Parse-error wrapping at every error return.** Codegen embeds the JSON field
    name directly as the first arg of every error return:
    `return result, i, decode.NewParseErr("street", i, err)`. The field literal is
    written at emit time (no post-pass / no `/*ggen-field*/` markers) — each
    `inlineScan*`/dispatch site interpolates the quoted JSON-path literal into its
    `NewParseErr` call. Zero runtime cost on the happy path.
    `NewParseErr(segment, pos, err)` builds `*decode.ParseError{Path []string,
Pos, Err}` for raw sentinels (a one-segment path), prepends the segment onto a `validation.Error`'s Path (value passes through, reachable via `errors.As`), and **chains** when err is already a `*ParseError` —
    prepending the segment onto `pe.Path` (deeper `Pos` kept) so nested surfaces
    `Error()`-join to `addr.street`. Empty segment leaves the path untouched.
    `errors.Is(err, scan.ErrBadString)` works via `Unwrap()`; `ParseError.Error()`
    calls `e.Err.Error()` once so chained prints stay linear.
    **Bytes-path NESTED-decode sites use `NewParseErrShift(seg, i, _n, err)`**
    (2026-08): the callee ran on `data[i-_n:]` (the emit advances `i += _n`
    BEFORE the check), so its positional errors — chained `*ParseError`,
    validation typed errors, `validation.Errors` — are rebased by the value
    start via `AddPos` before wrapping, making every `Pos` a full-payload
    offset like the stream path's `s.Offset()`. The multierr drain rebases
    with `validation.ShiftPos(verr, i-_n)`. All emitted from ONE helper
    (`nestedDecodeErrCheck(field, multierr, bytesPath, nVar)`; `nVar` = the
    consumed-count local, `_n`/`_in`) + the alias delegation wrap +
    `decode.UnmarshalSlice`. Sentinel/foreign errors carry no `Pos` → the
    shift no-ops. Error path only; happy path unchanged.
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
    `fieldSkipExpr` (`generate.go`) builds the non-pointer per-kind guards:
    `omitempty` had a `KindSlice, KindMap` case for "len > 0" that `[]byte`
    (`KindBytes`) never matched — a nil/empty `omitempty` `[]byte` field
    always emitted `"key":null` instead of being skipped; now `KindBytes`
    shares the slice/map arm. `omitzero` on `KindStruct`/`KindSQLNull`/
    `KindArray` emitted `ref != (T{})`, which does not compile when T holds a
    slice/map/func/chan field (Go comparability); `zeroCompare` now checks a
    new `FieldInfo.NotComparable` flag (`!types.Comparable(field.Type())`,
    set at both extraction paths in `parse.go`) and falls back to
    `!reflect.ValueOf(ref).IsZero()` for those — comparable structs still get
    the cheap `!=` form.
36. **Brace-less value emitters.** Decode value emitters write locals straight into
    the caller's scope — no `{ … }` wrapper per value (slice/array/map, time/
    duration/netip/url/big\*/raw/sqlnull/any/string-tag/struct/bytes, cross-pkg
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
    subsumption:** if an ASCII-implying rule (`alphanum`/`numeric`/`hexadecimal`)
    passed earlier in the same run, `R == len` exactly so the walk
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
46. **SIMD string-scan tier (bytes path, opt-in via `-simd`).** When
    `scanStringFn != "scan.String"`, `inlineScanStringVar` swaps its unbounded
    scalar hot loop for an **inline fused vector classify** (no call): one
    `LoadUint8x{16,32}Slice` + Equal/Equal/Min-Equal → ToBits → TrailingZeros
    finds the first structural byte; a quote hit takes the inline alias/copy
    path, anything else (escape, ctrl, span ≥ lane) falls to the direct
    `scan.StringAVX`/`AVX2`/`AVX512` call, which restarts at `posIn` — error
    identity byte-identical. Lane by tier: avx → 16 B, avx2/avx512 → 32 B
    inline (64 B inline never pays). Full-lane loads only — `Load*SlicePart`
    is a real CALL, not an intrinsic — so a string starting within one lane
    of the payload end takes a **bounded scalar tail loop** instead (< lane
    iterations; without it, tiny payloads whose fields all sit near EOF paid
    a tier call per string — measured Tiny +26%). Broadcast constants are
    emitted per site; gc CSEs them across sites and hoists them out of loops.
    **Short-key override:** for the dispatch KEY scan, when every declared
    JSON name is ≤ 5 bytes (`maxJSONNameLen`), the vector classify's
    dependency chain (~load+3 compares+movemask+tzcnt) loses to a handful of
    predictable scalar iterations, so `inlineScanStringWin` emits a bounded
    scalar window sized `maxKey+1` instead (unknown longer keys fall to the
    tier call). That flipped Tiny_Unmarshal from +15% to −8%. The 6 direct
    `scan.String` emit sites (alias, map value, TextUnmarshaler feed, …) swap
    the callee name. **Marshal side:** `appendStrFn` appends the same tier
    suffix (`simdSuffix`), routing every string-append site to
    `encode.AppendString{,NoHTML}{AVX,AVX2,AVX512}` — length-gated fused
    escape scans (see `encode/CLAUDE.md`). `-copy` detaches via
    `strings.Clone` around the tier call (escape-path strings are already
    owned — the double copy there is accepted `-copy` overhead). The tier is
    FIXED at generate time: no runtime CPU probing, no dispatch branch; wrong
    CPU ⇒ SIGILL, missing `GOEXPERIMENT=simd` ⇒ compile error — both loud,
    both the contract of the opt-in. Generated files pull `simd/archsimd` +
    `math/bits` via the body-scan import table. Numbers in `bench/CLAUDE.md`
    (headline: NoAlloc −22%, Mega −8.8%, Tiny −8% at avx512 vs the shipped
    prelude shape). **Stream side:** `tierStreamStringCalls` post-passes the
    rendered DecodeFromStream body (assignment-shaped rewrite, so encode
    bodies can't collide), swapping `= s.String()`/`StringView()`/`KeyView()`
    to the per-tier Stream methods (`scan/simd_stream_amd64.go` — fused
    locate per buffered window, refill loop unchanged). KeyView keeps the
    scalar prelude on all-short-key structs via the same `maxJSONNameLen ≤ 5`
    gate. Mega_Reader −5.2%, NoAlloc_Reader −7.7%, Small_Reader −20/−26% at
    avx512. **Skip side:** bytes decode bodies render through a scratch
    buffer and a post-pass rewrites `scan.SkipValue(` → the tier skip tree
    (`scan/simd_skip_amd64.go` — vector whitespace runs + fused skipString;
    SkipHeavy compact −21.6% / pretty −29.9%, Mega −8.3%); `inlineSkipWS`
    consumes one WS byte inline (compact and single-space payloads stay
    call-free) then hands 2+ runs to `scan.SkipSpace<tier>` (pretty
    full-decode −3.3%; costs Tiny ~+1%, accepted). Stream decode bodies get
    the same swaps via `tierStreamStringCalls` (`= s.SkipValue()` /
    `= s.SkipSpace()` → per-tier Stream methods,
    `scan/simd_skip_stream_amd64.go`): SkipHeavy ggen_stream compact −23.8% /
    pretty −22.6%, Mega_Reader flat. **Tier choice:** avx512
    is the default recommendation (Small −23%, NoAlloc −4% vs avx2); avx2
    wins skip-heavy pretty payloads by ~6% (skip lives on short spans where
    Zen5's double-pumped 512-bit ops cost 2× µops for no coverage gain).
    GFNI classify was explored and rejected — the structural/WS byte classes
    are not GF(2)-affine subspaces (kernel closure over {ctrl,'"','\\'}
    pulls in 0x20), and on the one expressible shape (ctrl detect) it
    measured slightly slower than Min/Equal.
47. **Scalar-tier bounded string-value scan (`scalarStringWindow` = 32).** On the
    default (non-`-simd`) build, `inlineScanStringWin`'s scalar branch used to walk
    every string body one byte at a time (3 compares/byte). It now bounds the
    inline loop to `scalarStringWindow` bytes and hands any span that runs past it
    (or hits an escape/ctrl byte) to `scan.String`, whose `bytes.IndexByte` locate
    is SIMD/AVX2 — so long strings (bios, URLs) ride the vectorized path while
    keys/short/medium values (the ≤32 B population that dominates real payloads)
    stay inline. `scan.String` is the error-identity source of truth
    (ErrUnterminated/ErrBadString); the bounded loop only fast-paths a clean
    quote-terminated span, so byte-parity holds (pinned by the bytes-vs-stream
    fuzzer + whitespace/escape tests). `-copy` detaches the handoff via
    `scan.Detach` (opt #49) — clones only when it aliased. window < 0 = unbounded
    original loop, used ONLY for the **dispatch key** scan: keys are short and
    matched against known field names, so a window bound is pure per-key setup with
    no long span to hand off (it regressed tiny structs +8%). Interleaved A/B vs
    the unbounded scalar tier: Small_Unmarshal −50.9%, NoAlloc_Unmarshal −19.0%,
    Tiny/Mega/MapHeavy/ValidationHeavy flat. Window sizing matters: 16 regressed
    Mega (medium strings paid a `scan.String` CALL per string); 32 keeps Mega's
    ≤63 B strings inline. The SIMD tier is untouched (window ≥ 0 still routes to
    the inline vector classify).
48. **Narrow-integer overflow guard (`narrowIntGuard`).** Fixed-width int fields
    smaller than 64-bit (`int8/16/32`, `uint8/16/32`) scan into a wide int64/uint64
    then cast. A bare cast silently TRUNCATES (`uint8` ← 300 = 44, nil error) —
    diverging from encoding/json v1 AND jsonv2, which both reject. Now every
    narrow cast is preceded by an in-range check returning `scan.ErrNumberOverflow`
    (one predicted compare on the happy path, ~0 cost). Emitted at ALL narrow
    sites: struct field, map value, slice/array element, and pointer leaf (fast +
    slow), bytes + stream (`inlineScanInt64`/`Uint64`, `widenedScan`, the stream
    map/slice widen branches, the pointer cascade). `int`/`uint` stay unguarded
    (64-bit on target platforms — no truncation). float32 gets the sibling
    guard (`narrowFloatGuard`, 2026-08): the float64 scan whose float32
    conversion lands on ±Inf returns ErrNumberOverflow — "converts to Inf"
    is exactly stdlib's reject boundary, NOT MaxFloat32 (which would wrongly
    reject values that round down to it). Same site coverage incl. aliases
    and `,string`. Pinned by `TestNarrowFloatOverflow` +
    `TestNarrowIntOverflow` (integrationtests, differential vs encoding/json over
    field/map/slice/pointer × bytes/stream). Same audit also fixed a MARSHAL bug:
    `renderAppendMap`'s value switch was missing `int8/16/32`, `uint/uint8/16/32`,
    `float32`, so `map[string]uint8` (etc.) marshaled `{"k":}` with no value —
    those kinds now emit the value.

    **`json:",string"` on a narrow int had NO guard (found+fixed 2026-08,
    audit round 4)** — `renderStringTag`/`renderStreamStringTag` parsed the
    quoted numeral with `strconv.ParseInt/ParseUint` at bit-width 64 and then
    bare-cast (`ref = int8(n)`), skipping `narrowIntGuard` entirely, so
    `"300"` into an `int8` field silently wrapped to `44` instead of
    returning `ErrNumberOverflow` — the float32 sibling on the same two
    functions already called `narrowFloatGuard`, which is what the coverage
    claim above was modeled on. Same-shape fix: both functions now call
    `narrowIntGuard` (via `kindNarrowName`, since the target may be a named
    type) before the narrowing cast, bytes + stream.

    A second, unrelated `,string` bug from the same round: on the bytes
    path, a `*int`/`*int64`-kind pointer field took the pointer-leaf FAST
    PATH (`cli/generate.go`'s inline int/uint scanner) unconditionally,
    which never checks `f.String` — a `*int` field tagged `,string` decoded
    a bare unquoted number and REJECTED the documented quoted wire form. The
    fast path now excludes `f.String` fields, falling through to the normal
    leaf recursion that reaches `renderStringTag`. And on the STREAM path,
    the string-tag branch used to run BEFORE the pointer peel (the bytes
    renderer already ordered Pointer first, with a comment explaining
    exactly why), so a `*int` field with `,string` emitted `result.X =
    *int(n)` — uncompilable. Stream now excludes `f.Pointer` from the
    string-tag branch too, so it falls into the pointer peel and re-enters
    the string-tag branch on the (non-pointer) leaf.
49. **Single-copy `-copy` escape strings (both tiers, via `scan.Detach`).** In
    `-copy` mode a RETAINED escaped string used to double-allocate: the fall path
    emitted `sv, i, err = scan.String(...)` (escape arm → `stringSlow` returns a
    fresh owned scratch) then `dst = strings.Clone(sv)` — the clone is redundant,
    `stringSlow` already owns the bytes. `scan.Detach(s, data)` (scan.go) clones
    IFF `s` aliases `data` (a `uintptr` pointer-range test; a `stringSlow` scratch
    is a distinct heap alloc → skipped, non-moving GC makes it sound) — so the
    copy fall calls the SAME aliasing tier func then `Detach`, dropping the clone
    on escapes. **Tier-agnostic: reuses `scan.String`/`StringAVX*` directly, NO
    per-tier `StringCopyAVX*` variant.** Wired at the copy fall
    (`inlineScanStringWin`, both scalar + SIMD blocks), the inline-map string value
    (`unknownKey`), and `scan.AnyCopy`/`AnyNumberCopy` (values + object keys).
    `EscapeHeavy/ggen_copy`: 8→4 allocs — identical to the aliasing `ggen` row at
    both scalar and avx512 (on escapes the -copy detach is FREE — stringSlow
    already owns). Same pass fixed a pre-existing SIMD gap: `StringAVX*`'s
    `classifyStructural` sized `stringSlow` off the first quote (an escaped `\"`),
    not the real unescaped close via `stringSpanEnd` — SIMD escape decode 44→4
    allocs. Wire-identical; pinned by `TestDetach` (scan), `TestStringSIMD_Parity`,
    `TestCopy_EscapedDecouples` (integ, every copy site), and
    `EscapeHeavy/ggen_copy`'s scribble guard.

50. **Decode-side UTF-8 validation (jsonv2 parity, `scan.ErrInvalidUTF8`).**
    Every string-PRODUCING decode path rejects malformed UTF-8 and unpaired
    `\uXXXX` surrogates (v1 would silently substitute U+FFFD; ggen sides with
    jsonv2, which errors). Runtime mechanics live in scan/CLAUDE.md (fused
    high-bit accumulate → `utf8.Valid` only on non-ASCII spans). Codegen side:
    every inline string fast path must BAIL to the validating runtime func on
    non-ASCII — the scalar windows (value window, dispatch-key unbounded loop,
    SIMD near-EOF tail) add `data[ke] < 0x80` to the loop condition (high byte
    exits → not '"' → `scan.String*` fall), and the inline vector classify
    folds ctrl AND ≥0x80 into ONE range term (`d := v.Sub(0x20); d.Max(0x60).
    Equal(d)` — `(v-0x20) >= 0x60 ⇔ v < 0x20 || v >= 0x80`), replacing the old
    Min-Equal ctrl term at the SAME op count, so ASCII fast paths pay ~zero
    (an unfused extra Max-Equal-Or term first measured DeepNested +13% at
    avx512; the fold brought it back to +2.8%). Captured raw spans
    (`renderRawJSON`/`renderStreamRawJSON`) emit `scan.CheckUTF8(span)` after
    the capture — byte-level validation, jsonv2 parity (Mega, which carries
    RawMessage fields, absorbs it inside its ~+2% delta). Skipped spans
    (`ignoreunknown`) are DELIBERATELY grammar-checked only; unpaired
    surrogate ESCAPES inside raw spans pass (ASCII text there — residual v2
    divergence, see backlog). Not tied to `-novalidate` (a parse correctness
    rule, not a validation rule); the opt-out is `allowinvalidutf8` (flag /
    annotation, htmlescape-style granularity): the runtime scanners take a
    `validate bool` (span-level branch, measured flat at both tiers — the
    per-byte loops never test it), permissive structs emit `false` plus the
    PRE-VALIDATION inline shapes (no `< 0x80` window bail, Min-Equal ctrl-only
    vector classify, no `CheckUTF8`), so their generated code is byte-identical
    to the pre-#50 emitter. Permissive semantics = raw bytes pass through
    (NOT v1's U+FFFD substitution — that would cost a copy on the alias path);
    unpaired surrogate escapes DO substitute U+FFFD (stringSlow owns its
    scratch anyway). `any` fields keep validating (scan.Any internals pin
    validate=true). Pinned by `TestAllowInvalidUTF8` (integ: every string
    shape + raw, bytes + stream, grammar-errors-still-reject, strict control). **Cost** — RE-MEASURED 2026-07 on a `performance`-profile box with a
    warmup pass and per-family **control rows** (jsonv2/sonic/easyjson, which
    ggen changes cannot affect; if a control drifts >3% that family's delta is
    not trustworthy — see bench/CLAUDE.md). Baseline = pre-UTF8 `03c6503`,
    cumulative through the depth cap (#51) and number grammar (#52), which are
    themselves flat-to-faster on these rows:
    - **Trustworthy** (control ≤2%): Mega_Unmarshal avx512 +1.8% (ctl 1.9% —
      i.e. flat), Mega_Reader +0.9…+2.3%, SkipHeavy −1.7…+3.3% (flat; skip
      paths aren't UTF-8-validated), MapHeavy −2.4/−3.1%, and **EscapeHeavy
      avx512 +12.7% / +13.2% copy with a 0.5% control** — the escape path
      gained a `utf8.Valid` over assembled output plus surrogate rejection, and
      this payload is ~12% escapes incl. surrogate pairs.
      (An earlier revision of this note claimed EscapeHeavy *IMPROVED* −26/−31%
      from the "surrogate-arm restructure". That was a bad-regime artifact —
      the box was under a capped power profile. It regressed; the number above
      is the control-checked one.)
    - **Real but control-drifted** (effect ≫ drift, so directionally solid,
      precision soft): the unicode-heavy rows paying the validation walk —
      NoAlloc **+67.7% scalar / +38.7% avx512** (its payload is
      Ukrainian-localized Cyrillic), RuneGated **+54.4% scalar / +9.5% avx512**
      (single-row bench — it has NO control row at all). The SIMD tiers use the
      vectorized `validUTF8x16` Lemire pass (scan/CLAUDE.md), which is why the
      avx512 penalties are far below the scalar ones. All still 2.7-5× ahead of
      jsonv2, which does the same validation.
    - **Not measurable on this box**: Small, Tiny, DeepNested, ValidationHeavy
      — controls drift 4-27%, so no ggen delta there means anything. Pinned by
    `TestString_InvalidUTF8Rejected`/`TestString_LoneSurrogateParity` (scan),
    `TestStreamStringInvalidUTF8` (stream, incl. rune-straddles-refill),
    SIMD parity corpora, `TestInvalidUTF8Rejected` (integ differential vs
    jsonv2), and `FuzzPrimitivesCompat`'s reject-parity branch (the old
    fuzz blind spot that hid this bug — it SKIPPED invalid-UTF-8 strings).

51. **Recursion depth cap (`scan.MaxDepth` = 10000, jsonv2 parity).** Every
    recursive decode path was unbounded → a few MB of `[[[[…` / `{"k":{"k":…`
    was a FATAL, unrecoverable goroutine stack overflow (not a `recover`-able
    panic — the process dies). Now capped:
    - **Runtime** (`scan.go`, `stream.go`, both SIMD skip files): `SkipValue`
      and the four `Any` families keep their public signatures but delegate to
      a `depth`-threaded core (`skipValue`/`anyValue`/`skipValueAVX*`/stream
      mirrors); each container-OPEN checks `depth > MaxDepth` → `ErrMaxDepth`.
      One predictable compare per `[`/`{`, nothing on scalar values.
    - **Codegen**: only SELF-REFERENTIAL structs change shape. `computeCyclicTypes`
      (generate.go) scrapes type identifiers out of every field's
      `GoType`/`ElemType`/`PointeeType`/`SQLNullInner`/alias-underlying and
      finds which generated types can reach themselves (over-approx — a false
      hit only costs an unneeded, still-correct shim). A cyclic struct T emits
      `DecodeFrom(data)` → `recv.decodeFromDepth(data, 0)` shim + a
      `decodeFromDepth(data, _depth int)` core that guards `_depth > MaxDepth`;
      nested calls into cyclic callees pass `_depth+1` (`decodeCallFor`/
      `streamDecodeCallFor`; alias delegation threads it too). ACYCLIC structs
      that reference a cyclic type get a folded `const _depth = 0` so the call
      site stays uniform; acyclic structs that DON'T (the common case) render
      their field body first and only emit the const if it scanned true
      (`strings.Contains(body, "_depth+1")`) — the const is otherwise dead
      weight (harmless either way; `go build` elides an unused `const`
      entirely, this is a codegen-output cleanliness fix, not a correctness
      one). Seeded/cleared alongside `generatedTypes` in `main.go` (+
      `bench_test`).

      **Single-file mode (`ggen file.go [Name...]`) used to run cycle
      detection over only the structs declared in that one file** — a
      cross-file `A↔B` cycle (A in `foo.go`, B in `bar.go`, same package)
      never entered `cyclicTypes`, so BOTH lost the depth-threaded core and
      its stack-overflow guard, silently. `parseFile` now resolves every
      struct in the package (`set.resolveFiltered` over `set.annotations`,
      best-effort — a sibling that fails to resolve falls back to the old
      per-file behavior) and runs `computeCyclicTypes` over the whole set;
      `generateSingleFile` seeds `cyclicTypes` from that instead of leaving
      it nil.
    - **Cost** (core-24, 500x, count=2, machine in `performance` profile, each
      A/B warmed first and validated with the **jsonv2 row as an in-run
      control** — see bench/CLAUDE.md): everything flat EXCEPT DeepNested (a
      50-level pure-recursion cache-resident microbench — the maximally
      depth-sensitive shape) at **+5.4% scalar** (8809→9294) and **+0.4%
      avx512** (10501→10548, i.e. flat). Mega (realistic ~4.4 MB tree, shallow
      nesting) is FLAT both tiers — the per-container compare vanishes against
      memory latency. Accepted: ~5% on pathologically-deep-but-legal recursion
      on one tier to turn a fatal process crash into a clean error. Pinned by
      `TestMaxDepth` (scan, every runtime path) + `TestMaxDepthNoCrash` (integ:
      recursive struct, `any`, ignoreunknown-skip, RawMessage, bytes + stream —
      the exact inputs that formerly crashed).
      **Measurement history (cautionary):** this was first reported as "+4.7%
      scalar / +10.3% avx512" with a mechanistic OOO-slack explanation for the
      avx512 amplification. The avx512 figure was pure machine artifact — the
      box was under a capped power profile (3 GHz ceiling / 2 GHz floor) and
      the first-measured binary ate the frequency ramp, inflating whichever
      side ran first by up to 50%. The invented mechanism explained noise. Only
      the scalar number survived re-measurement.

52. **Value-decoder number grammar (RFC 8259 / jsonv2 parity).** The VALUE
    decoders accepted Go-number-isms `strconv` allows but JSON forbids —
    leading zeros (`01`), bare/trailing dot (`.5`, `1.`), leading `+` — because
    `Float64` handed a loose `[0-9.eE+-]` span to `strconv.ParseFloat` (a Go
    parser, not a JSON one) and the int digit loops had no leading-zero rule.
    ggen was inconsistent WITH ITSELF: the SKIP path (`skipNumber`, used for
    RawMessage / `ignoreunknown`) was already strict, so `{"raw":01}` rejected
    while `{"i":01}` accepted the same bytes. Now:
    - `Int64`/`Uint64` (runtime) + BOTH inline codegen emitters
      (`inlineScanInt64Stmt`/`Uint64Stmt`) gained the leading-zero rule. The
      emitter half is REQUIRED, not optional — without it the generated inline
      fast path would accept `01` while its own `scan.Int64` fall path rejects
      it, recreating the asymmetry one level down.
    - `Float64` enforces the grammar INLINE (duplicated from `skipNumber` on
      purpose — see below); `Number` and the stream `Float64`/`Number` validate
      their assembled span via `skipNumber` (the stream refill loop doubles as
      the extent finder, so its span is only contiguous at the end).
    - All four `Any` families inherit it through `Float64`/`Number`.
    **Perf:** routing `Float64` through the `skipNumber` CALL measured
    consistently worse than inlining the grammar (20.8 vs 18.7 µs DeepNested,
    control-matched) — another instance of the backlog's "removing decode
    inliners" rejection, hence the deliberate duplication. Final shape is
    flat-to-FASTER (avx512 −1.5%, scalar ~−3% on DeepNested): the strict digit
    loops test 2 conditions per byte (`>='0' && <='9'`) where the loose scan
    tested 6, offsetting the added structural checks. Pinned by
    `TestNumberGrammarStrict` (integ: 19 cases × bytes / chunked stream / the
    skip+RawMessage path, differential vs jsonv2 — the skip row is what proves
    the asymmetry is gone) plus updated `scan` lattice + reference-differential
    tests (their zero-padded inputs are now correctly rejected).

53. **Custom time layouts close through the escape helper.** A named
    `time.X` constant is fixed, ASCII-safe text, but a CUSTOM layout's
    non-token characters are copied verbatim by `AppendFormat` — so a layout
    carrying `"` produced INVALID JSON and one carrying `\` a silent
    backspace escape. `renderAppendTime` routes custom layouts through
    `encode.CloseJSONString{,HTML}` (the same escape-on-dirty closer the
    TextAppender sites use, keyed off a field-suffixed `_tf<Field>` mark), and
    `timeFormatSize` budgets +5 per escape-needing layout byte. Named
    constants keep the raw append. Pinned by
    `TestFormat_CustomLayoutEscapes`.

54. **Unreadable struct tags are a generate-time error.** `reflect.StructTag.Get`
    unquotes the tag value with Go string rules, so an invalid escape (a bare
    `\'`) makes it return `""` — every `pipe:` rule in that tag vanished
    SILENTLY and the field emitted with no validation at all. `checkTagReadable`
    rejects a tag that spells `json:`/`pipe:`/`hint:` but reads back empty,
    naming the correct spelling (`\\'` in source). Same class as the
    accepted-tag-emits-broken-code rule, one layer up.

55. **Decode-variant shapes resolve named primitives.** `variantCaseBytes`
    fed `f.Kind` straight to `kindShapeBytes`, but a named primitive reports
    KindStruct at its use sites — so the NATIVE variant of a `type Score int`
    field claimed the object shape `{` and `{"s":42}` fell to the dispatch
    default, and a converter whose input W was a named primitive was
    unreachable the same way. `variantShapeKind` resolves through
    `FieldInfo.NamedPrims` (parse time, before `namedKinds` is seeded) then
    `effectiveKind` (render time).

56. **Wire-key name constants are JSON-escaped statically.**
    `renderAppendJSONBody` used to concatenate `f.JSONName` raw into the
    `,"name":` prefix (Go-escaped via `%q` only), so a quote-bearing name from
    the standard tag grammar (`json:"'q\"x'"`) emitted invalid JSON with a nil
    error, and a backslash-bearing one a silently different key.
    `escapeJSONName` escapes once at generate time (quote/backslash/control;
    `\b\f\n\r\t` shorthands, else `\uXXXX` — jsontext's spelling); htmlescape
    structs also escape `<>&` in names (v1/jsonv2 escape keys like values).
    `renderSize` budgets `len(escapedName)+3`. Decode dispatch already matched
    the unescaped name against unescaped wire keys — only the emit side was
    broken. Pinned by `integrationtests/keyescape_test.go` (jsonv2 byte parity
    for the quote name, v1 parity for the htmlescape name, validity +
    self-round-trip for the backslash name whose SPELLING divergence stays —
    see backlog).

57. **Bytes-path slice/array structural errors wrap in `NewParseErr`.** The
    `[`-open and `]`-close guards in `emitByteSliceRead` and every
    `emitNoCloseAfterComma` site returned BARE `scan.ErrBadArray`/`ErrBadObject`
    (no path, no pos, `errors.As[*decode.ParseError]` failed) while the stream
    twins and the map/bytes-value guards all wrapped. All bytes-path structural
    guards now emit `decode.NewParseErr(field, cursor, sentinel)` — including
    the `pipe:` variant dispatch in `variants.go` (`renderVariantDispatch`'s
    head/`null`-arm/default sentinels, which had the `field` literal computed
    and then discarded via `_ = field`). Pinned by
    `TestParseError_SliceStructural` + `TestParseError_VariantDispatch` (integ,
    bytes + stream). The entry-level `ErrMaxDepth` guard stays bare — it fires
    before any field is entered, so there is no path or position to carry. The same round's
    runtime half made scan primitives return the ERROR position instead of 0,
    so the existing `NewParseErr(field, i, err)` emit shape stamps a real pos
    with no codegen change (`TestParseError_ScanPrimitivePos`).

58. **`renderAppendMap` uses `vref` in the bool/int64/uint64/float64 arms.**
    Those four arms hardcoded `v`, so a named-primitive map value
    (`map[string]Flag`, `type Flag bool`) generated uncompilable
    `strconv.AppendBool(dst, v)`; the KindString arm and `emitSliceElement`
    already cast via `primCast`. Pinned by
    `TestMap_NamedPrimitiveValuesRoundTrip`.

59. **Stream error positions stamp `s.Offset()`, never the raw `s.Pos`.**
    ~165 emit sites per generated decoder interpolated the buffer-relative
    `s.Pos` into `decode.NewParseErr`, while the validation-error sites in the
    SAME function already used `s.Offset()`. `Pos` only equals the payload
    offset until a compacting refill slides the window, so the reported
    position collapsed toward 0 and CHANGED WITH THE CHUNK SIZE (`{"i8":128}`
    → 9 on bytes, 0/0/2 on stream at 1/3/7-byte chunks). Round-4 fixed the
    identical class inside `decode.UnmarshalSliceStream` but not the emitters;
    round-5's pin used a 16-byte payload, where the window never slides and
    `Pos == Offset()` by accident. Pinned by
    `TestParseError_StreamPosIsPayloadOffset` (9.5 KB payload × chunk sizes ×
    a 64-byte buffer).

60. **Generated stream refills map drained → the bytes-path sentinel.**
    The struct dispatch loop's `ReadMore` guards returned the RAW reader error,
    so truncation surfaced `io.ErrUnexpectedEOF` where the bytes path reported
    a grammar sentinel. The two guard positions now carry the sentinel the
    bytes path returns for the same truncation (`ErrExpectString` at a key,
    `ErrBadObject` past a value) via the newly exported `scan.NotEOF`, so
    transient reader errors still propagate raw. Sentinels AND positions now
    match bytes at every chunk size. One residual, deliberate: at the colon
    site the stream carries a field path and bytes does not — the check lives
    at different stages (bytes before dispatch, stream inside each case via
    `ConsumeColon`), so bytes has no field name there; sentinel and position
    agree.

61. **Foreign errors from converters and fallible mods wrap in `NewParseErr`.**
    An error-form `@Conv` / `@Mod` returned its own error bare — no path, no
    offset, `errors.As[*decode.ParseError]` false — while the bool forms built
    a typed `decode.ModError`. Both now wrap (the underlying error stays
    reachable through `errors.As`). Pinned by
    `TestVariant_converterErrorCarriesPathAndPos` +
    `TestFallibleMod_errorCarriesPathAndPos`.

62. **Empty `[]byte` wire decodes to an empty NON-nil slice.**
    `""` (base64/hex) and `[]` (`format:array`) left a nil receiver nil —
    `AppendDecode(nil, "")` is nil and an immediate `]` appends nothing — so
    `[]byte{}` marshalled `""`, decoded to nil, and re-marshalled `null`,
    breaking the round-trip fixed point that every other container honours
    (cli/CLAUDE.md's empty-non-nil rule). `emitEmptyBytesNonNil` closes all six
    arms (bytes + stream × base64/hex/array). Pinned by
    `TestBytes_emptyDecodesNonNil`.

63. **The scalar inline key scan falls to `scan.String` on any non-quote.**
    Its early-bails reported the OPENING QUOTE's offset for
    unterminated/ctrl-byte strings while the SIMD tier always falls and reports
    `scan.String`'s give-up position — the same struct built with and without
    `-simd` disagreed on error position (274 of 4000 random mutations). The
    fall is the documented error-identity source of truth, so the error arms
    now route there; the happy path drops a compare rather than gaining one.

64. **Parse-time rejections that used to be silent.** Three shapes were
    accepted and then miscompiled or silently ignored: an unterminated `'` in a
    json tag (the quote landed in the wire key), a negative `len`/`minlen`/
    `maxlen`/`runes` bound (`maxlen=-1` emits a validator no value can satisfy),
    and an unknown `format:` (a typo like `base64ur` fell through to the
    default encoding — wrong bytes, no diagnostic). All three now fail at
    generate time with a field+rule diagnostic. `format:` on `time.Time` stays
    open-ended (an unrecognized value there is a custom Go layout), and
    `[N]byte` is checked as `[]byte`. Rows in `cli_test.go`'s
    `InvalidRuleApplication` table.

65. **`omitempty` never omits a zero `big.Int`/`big.Float`/`big.Rat`.**
    The arm claimed a zero big value is "JSON-empty", but it encodes as `0` /
    `"0"` — v1 never omits a struct, and jsonv2 omits only `null`/`""`/`{}`/`[]`.
    The field silently vanished from the wire. Arm deleted. Pinned by
    `TestOmitEmpty_bigTypesNeverOmitted`.

66. **Primitive alias decoders skip leading whitespace.**
    `AliasString("").DecodeFrom([]byte(" \"x\""))` failed `ErrExpectString`
    while container and struct aliases accepted the same input (their
    `ArrayOpen`/`ObjectOpen` skip space). Whitespace is legal before any
    top-level value. Pinned by `TestAlias_leadingWhitespace`.

67. **Synthesized FieldInfos carry the parent's flags + hint levels (round 7).**
    Three drop classes in the synthetic-field constructors: (a)
    `peelSliceField` hardcoded `HintLen: -1` and never consulted
    `f.HintLevels`, so documented per-level hints (`hint:"32 inner:8"`) were
    parsed and silently never emitted — it now shifts `HintLevels[0]` into the
    peeled level's `HintLen` (mirroring the `Levels` shift); (b)
    `peelSliceField`, `elemPtrField`, and `sqlNullInnerField` omitted
    `AllowInvalidUTF8` (zero value = validate), so the opt-out silently
    stopped applying one container/pointer level down (`[][]string` inner
    rows re-validated while `[]string` didn't); (c) `converterInputField`
    built the `@Conv` input-W field with no `Copy` and no `AllowInvalidUTF8` —
    under `-copy` a converter-retained string ALIASED `data`, breaking the
    copy contract (buffer mutation after decode corrupted the converted
    value). Pinned by `TestGenerate_syntheticFieldFlagPropagation`.

## Named types, cross-package types (defects fixed 2026-07)

One missing lookup and one stale signature matcher made two whole type families
second-class. Both were found auditing a real request-schema package against
ggen; the fixes are pinned by `cli/namedkind_test.go`,
`integrationtests/namedprim_test.go`, `crosspkg_test.go`, `ptrcontainer_test.go`.

### `namedKinds` + `effectiveKind` (was `generatedAliasKinds`)

A named type over a primitive (`type Priority string`) reports **KindStruct** at
its use sites. `renderOneMod` resolved the underlying kind and cast through it;
nothing else did, so every VALIDATOR emitter saw KindStruct:

- `oneof` emitted its allowed values as bare identifiers (`case low, medium:`) —
  `renderOneofCases` quoted only for `kind == KindString`.
- rune / substring / charset rules passed the named value uncast into
  `utf8.RuneCountInString`, `strings.HasPrefix`, `validation.IsURL`, and into the
  string-typed `Value`/`Want` error fields.
- `eq`/`neq` were `if KindString {…} else if isNumeric {…}` **with no else** —
  the rule emitted nothing at all. Clean build, zero enforcement.
- `zeroLit` fell through to `elemType + "{}"`, so `nullzero` and the
  slice-element pre-grow emitted `Priority{}`.

`namedKinds` now carries every named primitive in the pass — annotated aliases
(from `StructInfo.AliasKind`) AND ones the user never annotated (resolved from
go/types in `structSet.namedPrims`, walking element / key / pointee / type-arg
positions, skipping types that carry their own JSON or text methods). Every rule
emitter resolves through `effectiveKind(goType, kind)` and wraps string-typed
arguments in `primCast`. `seedNamedKinds` fills the map at all three generate
entry points.

### Named primitives decode INLINE (`inlineNamedPrim`)

A field of a named primitive no longer calls anything: it scans the UNDERLYING
into a temp and converts at the assign (`var _nvX string; …; result.X = Pri(_nvX)`).
The conversion is free — identical underlying type, so gc emits no instruction
for it (asm-diffed: `utf8.RuneCountInString(string(p))` and the plain-string
form compile to byte-identical bodies). The CALL was not free: delegating to the
alias's `DecodeFrom` forfeits the inline window scan (opt #47) and pays a
`scan.String` per field, and an UNANNOTATED named type had no methods to call at
all so it fell to `SkipValue` + `encoding/json`.

Wired at every position, both paths plus encode and JSONSize: struct field
(`renderField` / `renderStreamField`), slice + array element
(`emitByteSliceRead` / `emitStreamSliceRead`, via a `_neN` temp around the elem
switch), map value (`renderMap` / stream twin, via `_nm`), `renderAppendValue`,
`emitSliceElement`, `renderAppendMap`, `sizeContribKind`, `sizeSliceContrib`,
and the map-size loop. Pointer fields reach it through the existing leaf
recursion. Alias depth is free: `type B S; type S string` resolves in one step
because go/types' `Underlying()` walks the whole chain.

Three gates, all load-bearing:

- **`f.Kind == KindStruct` only.** `time.Duration` is a named int64 and
  `net.IP` a named []byte; those carry a dedicated kind and their own wire
  shape. `collectNamedPrims` refuses to register any type whose own
  `resolveKind` is not KindStruct, and `inlineNamedPrim` re-checks.
- **An annotated alias's own flags must match the field's.**
  `//ggen:generate htmlescape type HtmlString string` is documented surface, and
  `copy` / `allowinvalidutf8` / `novalidate` likewise change what the alias body
  emits; inlining those with the PARENT's flags would silently swap behaviour,
  so `aliasFlags` keeps them on their own methods. A flag set globally on the
  CLI lands on both sides equally and never blocks inlining.

- **A type generated in ANOTHER pass keeps its methods.** `aliasFlags` only
  covers this pass, so a foreign alias's flags are invisible here and inlining
  would apply the parent's. `f.Iface.ByteDecoder || f.Iface.AppendJSON` +
  `!isGenerated` is the test; its own `DecodeFrom`/`AppendJSON` already encode
  whatever it was generated with. A foreign named primitive with NO methods is
  inlined like a local one.

  (Cross-package named primitives were invisible to `namedKinds` entirely until
  this landed: `collectNamedPrims` keyed them by `types.RelativeTo`, which
  spells a foreign type by full import path — `xpkg/leaf.Name` — while
  `FieldInfo.GoType` comes from the AST and reads `leaf.Name`. The key now uses
  a qualifier that returns `Pkg().Name()`, so the RULE family reaches foreign
  named primitives too.)

- **`json.Number` is excluded by name.** It is a named `string` whose wire shape
  is a NUMBER — the one stdlib type where "named over a primitive" does not
  imply "encodes like that primitive". Pinned by
  `TestNamedPrim_JSONNumberStaysNumeric`.

`nullzero` stays on the OUTER field (it is gated on `AtDispatch`, which only the
outer carries, and its zero literal is the named type's).

Measured on a 4-field struct: named 57.3 ns → 44.3 ns, exactly the plain-string
row (44.4 ns), 0 allocs throughout. On the TMS import shape the
annotated-vs-unannotated gap (was +15% / +600 B / +5 allocs on the widest
struct) is now zero on every axis.

### Cross-package types

- **`matchAppendJSON` tested `func([]byte) []byte`** while `renderAppendJSON`
  has emitted `([]byte, error)` for as long as the ladder existed, so
  `iface.AppendJSON` was ALWAYS false for a ggen type — rung 1 of the marshal
  ladder was dead code and every cross-package value fell to `json.Marshal`.
  Fixing it also activated rung 1 of the ALIAS ladder (`type X HasGgenMethods`
  → cast & delegate), which had never fired either; an alias carrying its own
  annotation (`allowdups`, `multierr`, …) now deliberately skips delegation
  (`reshapesCodegen`) because a delegating cast cannot honour it.
- **`inspectType` probed the pointer, not the pointee.** `*T`'s method set
  contains T's, and the ggen shapes are receiver-typed (`DecodeFrom` returns T),
  so every probe against `*T` failed. It now peels to the base type and
  synthesizes the pointer itself (which also fixes text/json *Unmarshalers*,
  which live on `*T`).
- **Foreign imports were never collected.** The import set is built
  feature-by-feature from `FieldInfo`, and nothing added the package of an
  element / pointee / map-value type — so `[]foreign.T`, `map[string]foreign.T`,
  `*foreign.T` all emitted a file naming a package it never imported.
  `FieldInfo.TypeImports` (from `structSet.foreignImports`, the same walk
  `collectTypeImports` does for `sql.Null[T]`) carries them, and
  `scanBodiesForForeignImports` keeps only the ones a rendered body actually
  spells — a plain VALUE field never names its type, so importing
  unconditionally would trip "imported and not used".
- **Slice/array elements had no cross-package rung at all**: the bytes path
  emitted a bare `scan.SkipValue` (every element of a `[]foreign.T` silently
  decoded to its zero value) and the stream path emitted nothing. Both now run
  the same ladder as the field level via `elemAsField`; map values run it too,
  instead of always reflecting over the captured span.

### Pointer-to-container at any depth

`emitReceiverReset` skipped pointer fields entirely, but the decode path only
allocates a pointee when the POINTER is nil — so a reused receiver appended into
the carried-in container (`*[]T` merged where `[]T` replaced). It now peels every
level, guards each, and resets through the final deref. Separately, the parse
layer peeled exactly ONE pointer level before the container switch that fills
`ElemType`/`ElemKind`, so at depth ≥ 2 the element kind stayed at its zero value
(KindString) and `**[]T` emitted a string scan into a T slot — both loaders now
peel to the innermost type.

### `oneof` frozen slices are scoped per output file

`ggenOneof0` restarted at 0 for each output file, so two sources in one package
that both used `oneof` declared the same package-level var twice. Names are now
readable and hash-free. Caps: `ggenCap_<Struct>_<Field>_<elemType>` (maxlen
variants suffix `_<N>`) — struct names are package-unique, so no file scope or
hash is needed; dedup narrows from per-file to per-field (a few duplicate
consts, zero runtime cost). Oneofs: `ggenOneof_<fileScope>_<n>` where fileScope
= output base minus `_ggen`/`_test`. Type spellings sanitize via
`sanitizeIdent` (alnum kept, `*` → Ptr, any other rune → exactly one `_`, no
collapsing so `[]int` → `__int` stays distinct from `int`). Cap names are
insertion-stable — adding/removing structs or fields never renames another
field's consts (the old struct-name-set hash prefix churned the whole file on
any set change). A collision would redeclare a const, loud at compile time.

## Design decisions (the why)

1. **`unsafe.String` boosts perf** by avoiding GC pressure — can backfire if
   parsed strings are referenced long-term after the input is mutated. The
   `-copy` mode (`StructInfo.Copy`, propagated to `FieldInfo.Copy` like
   `HTMLEscape`/`UseNumber` and through `peelSliceField`/`elemPtrField`/
   `sqlNullInnerField`) opts the BYTES path out of that aliasing, matching the
   stream path's lifetime. It changes only RETAINED-string sites:
   `inlineScanString`/`inlineScanStringVar` take a `cp bool` — when set the clean
   hot path emits `string(data[s:e])` instead of `unsafe.String(…)` and the
   escape/long-span fall calls the tier func then `scan.Detach` (opt #49 — clones
   only when the result aliased `data`, so the owned escape scratch isn't
   re-cloned; both scalar + SIMD, no per-tier variant); `renderRawJSON` emits
   `append(ref[:0], data[…]…)` (reused backing) instead of the alias; `renderAny`
   switches to `scan.AnyCopy`/`AnyNumberCopy` (detach every nested string + object
   key via `Detach`, no double-copy); `unknownKey` clones the inline-map key +
   detaches its string value via `Detach` and `strings.Clone`s the
   `UnknownKeyError` path segment. TRANSIENT scans stay
   aliasing (`cp=false`): the dispatch key (matched + discarded) and the
   parse-feeds for time/url/netip/big\*/`[]byte` (the conversion owns its
   output). Per-struct granularity ⇒ an inline-map / nested-struct VALUE only
   copies if that value's own struct also has `copy` (the whole-pass `-copy`
   flag covers every struct, so no gap there). Wire-identical to non-copy;
   alloc-heavier (one alloc per retained string, like the stream path).
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

## Test files (`cli/` module)

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
