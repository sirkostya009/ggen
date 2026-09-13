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
- `parse_test.go`, `parseload_test.go`, `tags_test.go`, `pipe_test.go`,
  `applicability_test.go`, `cli_test.go`, `namedkind_test.go`, `log_test.go` —
  CLI tests; `bench_test.go` = `BenchmarkGenerate` (generator perf only)

## Generator CLI (`main` package)

### Invocation

```
ggen ./...                    every package matched by the pattern (module-scoped, as `go build`)
ggen <dir>                    one package
ggen <dir> ./sub/... <dir2>   several targets in one run (one load, post-order over the union)
ggen <file.go> [Names...]     one file; optional struct name filter
```

**Every positional is a target** in dir/pattern mode, and every dir and
pattern positional feeds ONE `walkPackages` call: a single `packages.Load`
over all of them, processed post-order over the union, so `ggen ./b ./a`,
`ggen ./a ./b` and `ggen ./b/... ./a/...` all emit the `./...` output — an
importer never runs before its dependency's `_ggen.go` exists (argv order
used to decide whether a cross-package field took generated methods or the
`encoding/json` rung). A single plain directory keeps the direct
`generateDir` path, the only multi-struct shape that honours `-o`/`-pkg`;
with several targets or a pattern both are rejected up front (`-o cannot be
used with ./...` / `… with multiple targets` — `-pkg` was previously ignored
under patterns and applied to every package under multiple dirs). A leading
FILE still takes the rest as a struct-name filter (the one shape where
trailing args are names, not targets); a file in any later position is a
loud error.

Packages load via `golang.org/x/tools/go/packages` with full type info; interface
impls (TextMarshaler, ByteDecoder, JSONMarshaler, …) are picked up and emitted as
direct method calls — no runtime probing. If type info can't resolve (temp file,
no `go.mod`), falls back to AST-only mode and emits a plain `encoding/json`
fallback for cross-package types.

The load asks for `NeedForTest` and keeps TWO variants of a directory: the
base package as its internal-test recompilation (`ForTest == PkgPath`, a
superset of the plain package — internal `_test.go` structs ride along) and
the external `package <pkg>_test` package, each its own `structSet` because
they are distinct type-checked packages whose bare names may collide; the
synthetic test main is dropped. External structs carry `StructInfo.XTest`,
emit as their own group with `package <pkg>_test` (`-pkg X` → `X_test`), and
see base-package types as FOREIGN (cross-package ladder — direct `pkg.T`
calls). One invocation writes BOTH packages, so the external set is handed
the base set as `structSet.libPkg` and judges a `pkg.T` reference by what the
base pass will EMIT, not by what a previous run left on disk: a type in
`lib.passTypes()` gets the ggen-shape flags outright, one the base pass does
NOT emit has its stale gen-file methods masked, and a base package with no
roots this run keeps whatever it declares. Output is a fixed point from run 1
— the direct rung used to appear only on run 2, and the symmetric case
emitted calls to methods the same run was deleting. `parseFile` picks
whichever set declares the file, so `ggen pkg/x_test.go` on an external test
file works (single-file mode leaves `libPkg` nil — that invocation does not
write the base package's output, so on-disk methods are the honest answer);
the AST-only loader splits files by package name the same way.

Run `ggen` with the same `GOEXPERIMENT` env as user code — files behind an
experiment tag (e.g. `goexperiment.simd`) are otherwise invisible.
`encoding/json/v2` is stable as of Go 1.27 and needs no experiment.

Pattern mode (`./...`, `./sub/...`, `...`) resolves via `packages.Load` —
module-scoped, workspace-aware, never crosses module bounds. A subdir with its
own `go.mod` is skipped (multi-module repos run ggen once per module). Test-only
packages (no non-`_test.go` files) are skipped when a pattern matched them,
visited when named outright (single-package mode or a multi-target run).
Processing is post-order over the matched import subgraph (deps first),
sequential in topo order; `walkPackages` addresses packages by `Package.Dir`.
Dot/underscore-prefix dirs, `vendor/`, `testdata/`, `node_modules/` are
skipped by `go list`.

Output is written only after the whole file rendered AND formatted
(`writeGenerated`: render into a `bytes.Buffer`, then `os.WriteFile`), so a
render or `format.Source` failure leaves the previous `_ggen.go` byte-identical
instead of a 0-byte file that breaks the package build. Package mode, `-dry`
and pattern mode report EVERY struct's parse error: `parsePackage` /
`walkPackages` used to wrap `resolveFiltered`'s `errors.Join` in a single
`%w`, which the logger's `unwrapMulti` cannot see through, so only the first
`richError` surfaced. `prefixBare(err, prefix)` rebuilds the join and
prefixes only position-less members (a `richError` already carries
`file:line:col`).

### Flags (all opt-in, apply to every struct in the pass)

| Flag             | Effect                                                                                                                                                |
| ---------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------- |
| `-o <path>`      | override output path (single file / single plain dir only; rejected with multiple targets or a pattern)                                              |
| `-pkg <name>`    | override package name in output (same scope as `-o`; the external test bucket becomes `<name>_test`)                                                 |
| `-marshal`       | emit `MarshalJSON` method                                                                                                                             |
| `-unmarshal`     | emit `UnmarshalJSON` method                                                                                                                           |
| `-multierr`      | accumulate validation failures into `ggen.Errors`, returned at end of parse; parse errors still return immediately. The drain past a NESTED decode is gated on the callee being multierr too (`multiErrTypes` / `calleeDrains`): a single-error callee returns mid-value, so continuing would resume from a desynced cursor — the inner object's remaining keys used to surface as the PARENT's unknown keys |
| `-allowdups`     | allow duplicate keys, first-wins (later skipped). Default: `ggen.DuplicateKeyError`. NOTE the check is scoped to DECLARED keys (the per-field `seenX` flags) — dups inside skipped / `any` / raw / nested scopes are NOT detected, a decided divergence from jsonv2 (see backlog Tried Rejected) |
| `-novalidate`    | skip validation rules, required-field checks, mods                                                                                                    |
| `-ignoreunknown` | silently skip unknown JSON keys. Default: `ggen.UnknownKeyError`. Overridden when an embedded fallback map field is present                                |
| `-nullzero`      | accept explicit JSON `null` on every non-pointer value field → Go zero. Default hard-errors (see null kind-gating). No-op on already-null-aware kinds |
| `-nosortkeys`    | emit fields in Go declaration order. Default: alphabetical. Embedded fallback map fields stay last                                                               |
| `-usenumber`     | decode JSON numbers into `any` fields as `json.Number` instead of `float64` (mirrors stdlib `UseNumber()`)                                            |
| `-htmlescape`    | opt INTO HTML-safe escaping (`<`, `>`, `&` → `\uXXXX`) on marshal. Default = literal                                                                  |
| `-allowinvalidutf8` | skip decode UTF-8 validation (opt #50) for every struct in the pass: string scans AND the `Any*` walkers pass `validate=false` (raw bytes through, surrogates → U+FFFD — `any` fields, `map[string]any` values and the `,embed` catch-all included), inline windows/classify revert to the pre-validation shapes, raw-span `CheckUTF8` not emitted. Decode-only |
| `-copy`          | bytes-path `DecodeFrom` copies retained strings / map keys+values / slice elems / `json.RawMessage` / any-embedded strings out of `data` instead of aliasing it. Decouples decoded values from the input buffer (matches the stream path's lifetime). Decode-only; alloc-heavier |
| `-dry`           | parse + validate annotated structs, surface every error, emit no file. Composes with `-v`. Rejects `-o`/`-pkg`                                        |
| `-simd <tier>`   | `off`/`avx`/`avx2`/`avx512` — bytes-path string-scan tier (see opt #46). Resolved by `resolveSIMD` (main.go): `GOEXPERIMENT=simd` in ggen's OWN env auto-selects `avx`; `avx`/`avx2`/`avx512` error without the env var (emitted code imports `simd/archsimd`, which only exists under the experiment). Generate-time only — sets `scanStringFn` (`"ggen.String"` → `"ggen.StringAVX2"` etc); no per-struct annotation |

### Per-struct annotations

A comment on a struct (or gen-decl) `//ggen:generate` (no space after `//`,
mirrors `//go:generate`) followed by space-separated tokens. Apply only to the
annotated struct:

`marshal`, `unmarshal`, `multierr`, `allowdups`, `novalidate`, `ignoreunknown`,
`nullzero`, `nosortkeys`, `usenumber`, `htmlescape`, `copy`, `allowinvalidutf8`.
Any other word is a positioned `richError` (`unknown //ggen:generate token
"marshl"`, hint listing the twelve); `parseAnnotation` returns one per unknown
token as a batch that `walkStructDecls` stores in `structSet.declErr` and
`resolveFiltered` joins into the struct's errors, so package mode, single-file
mode and `-dry` all fail loudly and emit nothing for that struct.

## Struct tags (on fields)

Field config is partitioned by role across three tags: `json:` (wire shape),
`pipe:` (decode→transform→validate pipeline), `hint:` (prealloc).

### `json:`

- `json:"name"` — JSON key name (field is ignored otherwise). Taken VERBATIM
  (then unquoted): `json:" a"` names the key ` a`, as v1 and stable v2 both
  do (jsonv2 reserves only `, \ ' " \`` in a name). jsonv2 quoting:
  options split on commas OUTSIDE single quotes, so `json:"'a,b'"` names the
  field `a,b` and `format:'Jan 2, 2006'` survives its comma; `\'` = literal
  quote (`parseJSONTag`/`splitTagOpts`, tags.go)
- `json:"-"` — field explicitly ignored. `-` with options is a parse ERROR
  (jsonv2 parity — v1 read it as a field named `-`; use `json:"'-'"` for
  that). Empty options (`a,`, `a,,x`) also error, and so does a
  whitespace-padded option (`a, omitempty`, `a,omitempty ` — jsonv2 refuses a
  non-letter at option start; the old TrimSpace produced a wire key the
  stdlib would not). Unknown option words pass, with three exception classes
  from `checkTagOptionWord`, which normalises the word before `:` the way
  jsonv2 does (lower-case, underscores dropped): `case` (`case:ignore`,
  `case:strict`, bare `case`) is rejected — ggen has no case-insensitive
  match arm, and a field silently matching only its exact spelling turns
  accepted payloads into `UnknownKeyError`; a mutant of
  embed/omitzero/omitempty/string/format (`omitEmpty`, `omit_empty`,
  `OMITZERO`, `String`, `Format:hex`, bare `format`) gets jsonv2's own
  wording ("invalid appearance of `omitEmpty` tag option; specify
  `omitempty` instead"); `inline` keeps its dedicated message (opt #75)
- `json:",embed"` — catch-all map for unknown keys. Type must be a PLAIN
  `map[string]V` (string-keyed; a pointer to one and a NAMED map type are
  both rejected — the catch-all emitters `make`/index/`range` the field
  itself, and the gate tests `fi.Kind != KindMap || fi.Pointer` because
  `Kind` reports the POINTEE kind for `*map`; stable jsonv2 accepts an
  unnamed pointer here, a documented divergence). Both the go/types and the
  AST site build the message through `embedKindError(pointer, goType)`, which
  appends "(not a pointer to one)" only for a field that IS one — a named map
  reads "requires a map[string]T field, got M". V may be `any`, a primitive, a ggen-annotated struct, or any
  other type (typed elems use the elem's fast path when available, else
  `encoding/json.Unmarshal` over the captured span). Overrides `ignoreunknown`.
  Entries spliced out on marshal. At most ONE per struct, resolved by
  `resolveFieldCollisions` with jsonv2's dominance rule (embed fields are
  excluded from the wire-name grouping): an own catch-all beats a promoted
  one, two own ones error (`fields Extra and More cannot both be the
  json:",embed" catch-all map`), a same-depth promoted tie uses none — every
  emitter reads `StructInfo.EmbedField()`, the first, so a second annotation
  was a silent decode no-op while marshal spliced both
- `json:"name,omitempty"` — not marshaled when JSON-empty (null, "", [], {}).
  A STRUCT field is a generate-time error (`checkRuleApplicability`,
  opt #87): ggen always writes a struct's object, so the option could only
  ever be a silent no-op there. `omitEmptyCond` covers every remaining kind
  whose empty encoding is not Go-zero-shaped: `net.IP` (`len > 0`),
  `netip.Addr`/`Prefix` (`IsValid()`), `url.URL` (`!= (url.URL{})` — judged
  on ggen's string wire, where the zero URL is `""`), `any` (runtime
  `ggen.AnyIsEmpty`: type switch over nil/string/[]any/map[string]any/
  primitives, reflect for other slices/maps/arrays/pointers, structs never
  inspected), `[0]T` (always omitted), `[N]byte` (never — N base64 bytes),
  and a pointer field peels every level and ANDs the leaf's guard
  (`p != nil && *p != ""`). A pointer to a STRUCT therefore omits on nil
  alone, and a non-nil one emits `{}`. `AppendAny` applies the same rules to
  reflected struct fields by writing the member and unwriting it when the
  value's bytes are `null`/`""`/`{}`/`[]`, except that a struct value is
  never unwritten (see .claude/encode.md)
- `json:"name,omitzero"` — not marshaled when Go-zero. Generated code asks
  `IsZero()` only for `time.Time`; a user struct compares structurally
  (`zeroCompare`, `!reflect.ValueOf(ref).IsZero()` when not comparable).
  `AppendAny` consults an `IsZero() bool` method on the field type, on `*T`,
  on a pointer field (nil is zero) or on an interface's dynamic value, as
  jsonv2 does — the generated-code half is a backlog decision
- `json:"name,string"` — wrap numeric as JSON string on marshal, unwrap on
  unmarshal. Numerics only (jsonv2 defaults; jsonv2 itself silently IGNORES
  the option on non-numerics — ggen rejects at generate time per the
  no-silent-no-op convention). `bool` is tolerated as a documented no-op
  (v2 dropped bool quoting); a STRING field is a generate-time error — it
  was a silent no-op that didn't even match v1's double-encoding
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
  `optional` is a marker. Absent key → Go zero. The lift is TOP-LEVEL only:
  pass 1 tracks paren depth and skips a word that directly follows a bare
  `inner:`/`keys:` prefix, so any presence word that reaches `parseStep` is
  nested and rejected (`required marks the field's own presence and is not
  valid under inner:/keys: — an element or map key is never absent`). The
  old lift took the word from ANYWHERE, so `inner:(required minlen=1)`
  silently made the OUTER key required while `inner:required` became a
  validator step that emitted nothing.
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
  `@Conv` converter INPUT types (`W` in `func(W) T`): `classifyConverter`
  returns W itself and `resolvePipeCustoms` peels every pointer level into
  `Variant.InPointer`, takes `InKind` from the BASE spelling (the full
  spelling `*int` read as KindStruct and claimed `{`), and
  `converterInputField` builds the scan temp exactly like a pointer FIELD
  (`Pointer`, `PointeeType`, `TargetNil` — `var convN *W` is a known-nil
  local); `variantCaseBytes` adds `'n'` for a pointer input, so `{"x":5}`
  scans into a fresh `*int` and `{"x":null}` hands the converter nil (pairing
  that with `nullzero` or a null-accepting native variant trips the existing
  shape-clash check). W is also merged into `FieldInfo.NamedPrims`
  (`s.namedPrims(W)`), so an unannotated same-package `type Score int` input
  resolves through its underlying kind — number shape, inline scan — like a
  field of that type (opt #55 covered only FIELDS of named primitives).
  `chan`/`func`/interface inputs are rejected outright alongside the
  slice/array/map rejection (none have a wire shape a converter call site can
  scan into). A POINTER field runs its value steps as ONE ordered pass
  emitted after the assign cascade (`emitPointerPipe`): maximal runs of
  `@Func` steps take the field's own `*T` — the type the func is declared
  for — and runs of built-in steps take the deref'd leaf, each at its
  DECLARED position, so `pipe:"@Add1 gte=2"` on a `*int` adds before it
  compares exactly as the value-typed twin does. `splitCustomSteps(fieldPipe(f))`
  (pipe.go) only decides WHETHER the pipe mixes the two kinds: a single-half
  pipe keeps the cheaper shape (built-ins ride along with the leaf's decode,
  `@Func` steps run on `ref` once the null branch rejoins). The custom half
  is emitted on the null arm too, so a `nil` still reaches a func typed for
  `*T`. The same split runs after converter shape-dispatch (`emitFieldPipe`),
  there with a nil guard around the built-in groups because a variant may
  leave the pointer nil. The legacy `Validation`/`Mods` buckets survive only
  as the int fast-path gate, since `stepsFromLegacy` emits ALL mods before
  ALL validators (`gte=0 clamp=0|5` on a `*int` clamped -1 into range before
  the check).

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
out; negative is a parse error, and so is anything above `maxPrealloc`
(`math.MaxInt32`, applicability.go) — the value is pasted verbatim into
`make()`, so a huge one compiled and the first payload carrying the key
panicked with `makeslice: cap out of range`. The ceiling is MaxInt32, not
`1<<31`, because everything it admits must also be a legal `int` constant on
a 32-bit target: `2147483648` compiled here and overflowed there. `checkOneValRule` applies the same ceiling to
`len=N`/`minlen=N` on slice/map kinds at any inner level (they drive
prealloc); `maxlen` and string lengths are bounds only and are unaffected.

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
`clamp`'s lo/hi, `multiple`) are also range- and sign-checked against the
field's declared kind, not just parsed as a valid number — a bound literal is
pasted verbatim into a Go comparison against the field, so `uint gte=-1` or
`int8 lte=300` used to pass parse and then fail the GENERATED build with a
constant-overflow error. The gate is `parseIntBound(v, kind)`
(`ParseInt`/`ParseUint` at the width and sign `kindIntBits` reports; a
negative literal on an unsigned kind is `ErrRange`), on which `boundFits` is
built: `ErrSyntax` → "integer field needs an integer bound", `ErrRange` →
"out of the field type's range". It replaced `strconv.Atoi`, which caps at
`MaxInt64` and called every `uint64` bound from 2^63 up "fractional".
Every numeric bound literal is spelled with the field's own kind by one
helper, `numBound(kind, value)` — `gt`/`gte`/`lt`/`lte` (`Limit any`),
`multiple` (`Of any`) and `eq`/`neq` (`Want any`) alike: as an untyped
literal it defaulted to `int` and overflowed above `MaxInt64`, and as a
`float64` (the old `Limit`/`Of` type) a bound above 2^53 was reported rounded
and in exponent notation (`gte=9223372036854775809` printed
`9.223372036854776e+18`). So a `uint64` field's bound emits
`Limit: uint64(9223372036854775809)`, a float field's `Limit: float64(1.5)`. Numeric `oneof` parts dedupe through
`numericPartKey`: integral kinds key on the integer value (an integer-valued
float spelling like `1.0`/`+1` folds onto it, so `1|1.0` is still a
duplicate), float kinds on the float64 bits with -0 folded onto 0 — keying
on float64 merged distinct integers above 2^53 although
`case 9007199254740993, 9007199254740992:` is legal Go. `inner:` needs a
real element loop: `canDive` excludes `KindBytes` and
`checkRuleApplicability` folds a `[N]byte` (`foldByteArray`) BEFORE judging
it, so `[]byte` and `[N]byte` get the "only valid on slice/array/map"
diagnostic with a byte-specific hint (their base64 emitters have no element
loop, so every element step there was a silent no-op) while `[N]byte
json:",format:array"` keeps its loop and its `inner:` rules. That diagnostic
is the WHOLE story for a non-diveable field: the level-1 element pass and the
`inner:` level loop run in the else arm, since a field with no element type
would otherwise have its element rules judged against an empty type name
(`foldByteArray` clears `ElemType`, so `[]byte` with `inner:gt=1` printed a
second line reading "`gt` is inapplicable to  (expected numeric type)"). The
rest of `checkRuleApplicability` still runs — `format:`/`hint:`/`keys:`
diagnostics on the same field surface in the same pass, which is the point of
gathering instead of returning early. `eff()` (the
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
func (result T) DecodeFromStream(s *ggen.Stream) (T, error)
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
slice dance — see .claude/scan.md.

**Decode-into-receiver semantics.** The receiver passed in is an ALLOCATION
source, not a merge source: the decoded result is what a fresh decode would
give, and only container capacity + element allocations are recycled out of it.
Two passes carry that. `emitReceiverReset` resets containers at the top of
DecodeFrom so the decoder never appends over carried-in data (unconditional —
blank payload → blank slate, capacity kept); `emitOmittedZero`, emitted from
`renderPostLoopShape` at every success return of both paths, zeroes every field
whose key never appeared (`if !seenX { result.X = <zero> }`, reading the same
seen flags the required-field checks use). Containers are NOT in the second
pass — they were already emptied and keep their backing.

- slices and `[]byte`: `if X != nil { X = X[:0] }` at entry (backing reused;
  `make(...)` only when nil). For slice-of-slice (`[][]T`, any depth) the inner
  row backings are reused too: the outer grows by reslicing within cap (keeping
  the carried inner header) and each row is seeded `rowN := slot; if rowN != nil
  { rowN = rowN[:0] }`; a past-cap/fresh slot reads back nil and allocates anew
  (opt #43; pinned by `TestMerge_nestedSliceBackingReused`)
- `map[K]V`: `if X != nil { clear(X) }` at entry (buckets reused; `make` only when nil)
- nested struct: `result.X, _, _ = result.X.DecodeFrom(...)` — value-receiver
  takes the existing value as merge source
- cross-package fallback rungs (`encoding/json`, `UnmarshalJSON`,
  `UnmarshalText`) decode INTO their target and `encoding/json` merges, so both
  paths zero the target before every non-ggen rung — at the plain value field,
  the pointer leaf, `map[string]*T` values and `[N]T` slots of foreign types.
  `zeroAssign` spells the literal inline through `zeroLit` over the
  pointer-peeled type (`ref` always denotes the leaf), so the reset costs no
  runtime call and no import the field's own type does not already need
- pointer `*T` / `**T` / … (any depth): **parse-first** cascade. `null` →
  `result.X = nil` (drops a carried-in chain, stdlib parity); an OMITTED key
  nils it in the end-of-decode pass, deliberately at the END so a present key
  still reuses the carried pointee chain. Otherwise the leaf
  is decoded into a stack temp FIRST — a parse failure returns before any
  mutation, so no chain is allocated for a value that never landed. On success an
  assign cascade reuses the non-nil prefix of the receiver's chain and allocates
  `new(new(…v))` only from the first nil level down. The SEED
  (`emitPointerSeed`) is where a pointer-reached container is emptied, at any
  depth and wherever the chain lives (top-level field, `map[string]*[]T` /
  `map[string]**map` value, `[]**[]T` element): a slice leaf is handed as
  `v = (*p)[:0]`, a map leaf as `v = *p; clear(v)`, both paths — except a
  bytes-path map whose values the opt #76 swap reads, which is handed INTACT
  (a `*map[string][]int` keeps its row backing across decodes). The container
  emitters append into / fill whatever they are given, and only plain
  top-level fields get an entry reset, so before the seed emptied the leaf a
  chain carried through a swapped map value or a within-cap resliced `[]**T`
  slot appended/merged. A generated-struct leaf is seeded as-is (its decode
  resets it); a leaf that would go through `encoding/json` or an
  `UnmarshalJSON` rung gets a fresh `v` instead, gated by
  `leafResets(kind, type)` (primitive, or generated struct) which
  `elemPtrReusable` and `reusesMapValues`' pointer arm share — those rungs
  MERGE, and `map[string]*T` had been seeding them from the carried map since
  the opt #76 swap. Primitive leaves skip the seed. Widened numeric leaves
  scan into a wide temp and cast at the assign site. The leaf decodes
  natively at every depth — NO encoding/json fallback. Same emit on bytes +
  stream paths. Pinned by `TestPointerContainer_LeafSeedEmptied` (cli),
  `TestMerge_pointerContainerLeavesReset` + `TestMerge_crossPkgFallbackDecodesFresh`
  (integ)
- fixed arrays `[N]T`: every slot decodes fresh or strict-length-errors; no
  entry reset, and an omitted key zeroes the whole array in the end-of-decode
  pass. A GENERATED struct element is handed the carried slot as-is (its own
  decode resets it — opt #74); every slot that would MERGE is blanked first by
  `emitArraySlotBlank`, shared by both slice readers: a non-generated struct
  `dst[i] = T{}`, a map `clear(dst[i])` (bytes-path swapped map handed
  intact), a `[]byte` `dst[i] = dst[i][:0]` (its `AppendDecode` would land
  after the carried bytes); slice rows are `[:0]`'d by the nested reader,
  RawJSON/any/time/netip/url/big/sqlnull slots assign. The multi-level pointer
  cascade builds a fresh chain via `TargetNil`. Pinned by
  `TestMerge_ArraySlotsOverwrite` + `TestMerge_arrayContainerSlotsReset`

JSON `null` for slice/map sets `result.X = nil` (stdlib v1/v2 parity). JSON
`[]`/`{}` on a non-nil receiver keeps the `[:0]`'d / cleared container; on a nil
receiver allocates an empty non-nil container.

**`null` acceptance is kind-gated (diverges from stdlib).** ggen emits a 4-byte
`null` peek only for: pointer (`*T`), slice (KindSlice), map (KindMap), `[]byte`
(KindBytes — null ↔ nil, nil marshals as `null`), `net.IP` (a byte slice —
same null ↔ nil arm, `inlineNullPeek` / `emitStreamNullZero`, flat break at
dispatch), `sql.Null*`, and raw-message (`json.RawMessage`/`jsontext.Value`)
fields. Every other kind — non-pointer scalars, `time.Time`, `time.Duration`,
`netip.*`, `url.URL`, `big.*`, UUID, and other text/number kinds — has NO
null branch, so an explicit JSON `null` hard-errors the parse. stdlib v1/v2
instead accept `null` everywhere. Consistent with ggen's other strict
defaults (UnknownKeyError, strict array length, DuplicateKeyError,
trailing-comma rejection) — for a nullable scalar, use a pointer. Pinned in
`integrationtests/stdcompat_test.go` (`TestStdCompatMerge_IntentionalDivergences`).
At every null-accepting site an `n` that does not spell `null` — including
input that ends mid-literal (`{"tags":nul`) — is `ggen.ErrBadLiteral` on BOTH
paths: `inlineNullPeek` emits `if i < len(data) && data[i] == 'n' { if i+4 >
len(data) || data[i+1] != 'u' || … { return …ErrBadLiteral }; i += 4; … }`,
so the non-null happy path still exits on the same first compare and the
extra checks run only on an `n` (the two hand-inlined copies in
`renderSQLNull` use the helper too). The bytes side moved to the stream's
sentinel: the stream's null-literal refill → `ErrBadLiteral` is the
round-9 shape, the pipe variants already reported it on both paths, and
jsonv2 reports a literal error there. Pinned by `TestNullPeekSentinelParity`.

**`nullzero` opts a value field into null-as-zero.** A `nullzero` decode variant
in `pipe:` (per field) / `-nullzero` / `//ggen:generate nullzero` (whole struct)
makes a non-pointer value field accept explicit JSON `null`, decoding it to the Go
zero value — the middle ground between strict-reject default and stdlib's
accept-everywhere. Gated by `nullZeroApplies` (set + `AtDispatch` + a kind that
would otherwise reject null; already-null-aware kinds stay no-ops — `KindNetIP`
is excluded like `[]byte`, and `variants.go`'s `nativeAcceptsNull` includes it
so variant shape checks see the same `'n'` claim). Emit mirrors
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
`ggen.ErrBadArray`/`ErrBadObject`, wrapped in `ggen.NewParseErr` with field +
cursor on both paths (`emitNoCloseAfterComma` /
`streamNoCloseAfterComma`). The EOF half also guards a stream-path index of
`s.Bytes()[s.Pos]` on input truncated right after a comma (`SkipSpace` returns nil
at EOF with `Pos == len(buf)`). Pinned in `scan_decode_test.go`.

**Decode-into-receiver vs stdlib merge — divergences.** NOT a drop-in: (1) a
key the payload omits is zeroed — emptied for containers, `nil` for pointers,
Go zero for everything else — where stdlib retains the receiver's value; (2) a
present map key REPLACES (clear+refill) rather than merging entries (stdlib
retains receiver-only keys); (3) scalar `null` errors (above).
Slice-replace-on-present, null→nil for slice/map/pointer, nested-struct merge,
and `*T`/`**T` reuse on a PRESENT key all MATCH stdlib — pinned in
`TestStdCompatMerge_Parity`; the omitted-key divergences are pinned in
`TestStdCompatMerge_IntentionalDivergences`.

Call with a zero-value receiver for a fresh decode (`T{}.DecodeFrom(data)` for
struct/slice/map/array; `var zero T; zero.DecodeFrom(data)` for primitive
aliases). To merge into an existing value, call its `DecodeFrom` directly.

Runtime entry points (call from user code):

```go
// bytes path — single value
T{}.DecodeFrom(data)                       // (T, int, error)

// stream path — Reset returns *Stream, so it chains into the generic methods
s := ggen.NewStream(r, buf)
s.Value[T]()                               // (T, error); recycle s.Bytes()
s.Slice[T]()                               // ([]T, error); cursor survives
s.Value(prev)  s.Slice(prevSlice)          // decode INTO — reuses containers
s.Seq[T](prev...)                          // iter.Seq2[T, error]; NDJSON/concat,
                                           // reuses one value per run
T{}.DecodeFromStream(s)                    // the method Value wraps

// Unmarshaler[T] = Decoder[T] + StreamDecoder[T] — constrain on it to pick
// the path at the call site; every generated struct satisfies it

// bytes array walkers
ggen.UnmarshalSlice[T](data)             // ([]T, error)
ggen.ReadSlice[T](r)                     // ([]T, error)

// encode side
ggen.Marshal(t)            ggen.MarshalString(t)          ggen.WriteTo(w, t)
ggen.MarshalSlice(items)   ggen.MarshalSliceString(items) ggen.WriteSliceTo(w, items)
ggen.AppendSlice(dst, items)
```

Opt-in (`//ggen:generate marshal` / `unmarshal`):

```go
func (s T) MarshalJSON() ([]byte, error)     // wraps ggen.Marshal(s)
func (s *T) UnmarshalJSON(data []byte) error // inlines var zero T; zero.DecodeFrom(data)
```

## Top-level type aliases

Annotated named types (`//ggen:generate type T <underlying>`) get the same method
surface as a struct, driven by `renderAlias*` helpers in `alias.go`. Top-level
renderers dispatch to alias paths when `s.IsAlias` is set (except struct aliases
that fall back to field introspection, which set `IsAlias=false` and route through
regular struct codegen).

Accepted underlying kinds:

- **primitive** (`string`, `bool`, `int*`, `uint*`, `float*`): scan via `ggen.X` /
  `_s.X`, cast to alias. `htmlescape` flips the string-append helper. Float
  marshal routes through `ggen.AppendFloat` (stdlib-parity `'f'`/`'e'`
  selection, errors on NaN/Inf) — it used to be a bare
  `strconv.AppendFloat(…, 'g', -1, …)`, which silently emitted the literal
  text `NaN`/`Inf` (invalid JSON, nil error) and used `'g'` formatting that
  diverges from every other float site in generated code (`1e6` vs `1000000`).
  `JSONSize` budgets the shared `sizeFloat` (25 — the widest `'f'` form), so
  `ggen.Marshal`'s exact presize still lands in one alloc
- **struct** (`type LocalUUID uuid.UUID`): methods don't propagate from the RHS,
  so probing uses `inspectType` on the RHS named type. Three-step ladder:
    1. _ggen-method delegation_ — if underlying has AppendJSON+DecodeFrom: cast →
       method → cast back (cheapest). Fires on EVERY run when the underlying is
       generated in the same pass: `structSet.passTypes()` memoizes what the
       pass generates (package mode: annotated roots + reachable; single-file
       mode: the union of what each file's own pass emits) and an `*ast.Ident`
       underlying in that set gets the ggen-shape flags from the pass, not
       from a previous run's `_ggen.go` — output is a fixed point (run 1 used
       to hand-roll the body and run 2 delegate). The delegating shape decodes
       via `var u Inner; u.DecodeFrom(...)`, no receiver-carried reuse for the
       alias
    2. _field introspection_ — plain struct with ≥1 exported field: walk
       `*types.Struct`, synthesize FieldInfo per exported field
       (`extractFieldFromTypes`), then `s.resolvePipeCustoms(name, &fi,
       fv.Type())` so `@Func` mods/validators and `@Conv` variants in the
       underlying's tags resolve against the ALIAS's file and package scope
       (a bare `@Func` written in a foreign package's tags fails loudly with
       "not found"); `IsAlias` flips false, regular struct codegen
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
- **`[]byte` / `[N]byte` alias**: collapses to KindBytes, base64 path — the
  byte-array fold (`foldByteArray`) runs at alias position too, so
  `type Digest [4]byte` carries the same wire shape and strict decoded-length
  check as a `[4]byte` FIELD. An alias has no struct tag, so `format:array`
  cannot opt back into the v1 number-array form there.

Rejected: channel, interface, function — no sensible JSON shape. Also
rejected by `rejectedTypeDecl` in `walkStructDecls`: a GENERIC type
declaration (`ts.TypeParams`; `type Box: generic types are not supported`,
hint: declare a defined type over an instantiation — `type IntBox Box[int]`
takes the struct-alias introspection rung) and an `=` ALIAS declaration
(`ts.Assign`; `type Foo = time.Time: alias declarations cannot carry
methods`, hint: use a defined type). Both are registered by NAME only
(order/structFile/annotations + `declErr`), never in `structs`/`aliases`, so
BFS/embedding cannot pull them in and a reference takes the fallback ladder;
unannotated ones are never generated. Before, the generic emitted
`func (recv Box) …` with `T{}` zeroing (`cannot use generic type … without
instantiation`) and the alias put methods on a non-local or
already-generated type.

`htmlescape`/`marshal`/`unmarshal` apply to all aliases; `allowdups`,
`ignoreunknown`, `multierr`, `novalidate` apply to struct aliases. Foreign-package
imports collected into `StructInfo.AliasUnderlyingImport` (a `TypeImport`); field-introspection
types render through `structSet.spell` / `pkgQualifier` — one qualifier per
import path per pass (see "Foreign type spelling" under Cross-package types).

## Supported Go kinds (per field)

- `string`, `bool`
- `int`/`int8`/`int16`/`int32`/`int64`, `uint`/`uint8`/`uint16`/`uint32`/`uint64`,
  plus the builtin aliases `rune` (= int32) and `byte` (= uint8). Both resolve
  in `resolveKind`; without that a `[]rune` element fell to KindStruct and
  emitted `append(dst, rune{})` — an accepted annotation whose output did not
  compile
- `float32`, `float64`. `float32` sites (field, slice/array element, map
  value, pointer leaf, `sql.Null[float32]`, alias, `,string`) scan through
  `ggen.Float32` / `(*Stream).Float32` on both paths (`floatScanFn(kind)`;
  `widenedLeafCast` leaves float32 leaves unwidened, and the generated file
  needs no `math` import at those sites). Scanning as float64 and casting
  rounds the decimal TWICE, which lands one ulp off whenever the float64
  result is a float32 rounding midpoint — the shortest float64 forms of such
  values, i.e. what `json.Marshal` of a float64 emits (`1.0000000596046448` →
  `1` instead of `1.0000001`) — and turned `3.4028235677973366e38` (below the
  overflow midpoint) into +Inf. Out of range still returns
  `ggen.ErrNumberOverflow`. Runtime shape in .claude/scan.md; pinned by
  `TestFloat32_StdlibParity` (root) + `TestNarrowFloat_RoundsDecimalOnce`
  (integ)
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
  `RFC3339Nano` + custom (jsonv2 supported) + other `time.X` constants. The
  default layout, `format:RFC3339` and `format:RFC3339Nano` are strict RFC
  3339 on both sides (`isRFC3339Layout` + `timeParseExpr`): `renderAppendTime`
  emits `ggen.AppendRFC3339(dst, ref, layout)`, which refuses a year outside
  [0,9999] or a zone hour ≥ 24 like `time.Time.AppendText` and jsonv2 do (a
  bare `AppendFormat` wrote a string ggen's own decoder rejected while v1,
  jsonv2 and `AppendAny` all erred); `renderTime`/`renderStreamTime` emit
  `ggen.ParseRFC3339(s)`, which parses with `RFC3339Nano` and applies
  jsonv2's four post-checks (two-digit hour, `.` separator, zone hour < 24,
  zone minute < 60) that `time.Parse` skips — siding with v2 where v1 and v2
  disagree. Other layouts keep `AppendFormat`/`time.Parse`. The 64-byte
  `AppendFormat` headroom reservation still applies (the helper formats
  through it). Pinned by `TestTime_RFC3339StrictParity` (integ) +
  `TestParseRFC3339_JSONv2Parity` (root)
- `time.Duration` — `format:sec`/`milli`/`micro`/`nano`/`units` (default, parses `"1h30m"`)
- `net.IP`, `netip.Addr`, `netip.Prefix` — text form. Marshal via
  `encoding.TextAppender`, decode via `net.ParseIP`/`netip.ParseAddr`/`netip.ParsePrefix`.
  All six renderers (bytes + stream) take `""` as the zero value
  (`if s == "" { ref = nil / netip.Addr{} / netip.Prefix{} } else { parse }`,
  mirroring the types' `UnmarshalText` and overwriting a carried receiver
  value) — encode emits the zero value as `""` (jsonv2 shape), so a never-set
  field could not be decoded back by ggen. `net.IP` also takes `null` → nil
  (byte-slice rule). The stream netip emitters are one `renderStreamNetipParse`:
  the error branch re-parses `strings.Clone(sv)` because `parseAddrError` /
  `parsePrefixError` retain the input string and quote it in `Error()`, and
  `sv` aliases `s.buf` (same shape as `renderStreamNetIP`'s clone). Pinned by
  `TestNetTypes_emptyIsZero` + `TestNetip_ErrorDetachedFromBuffer`
- `[]byte` — `format:base64` (default)/`base64url`/`base32`/`base32hex`/
  `base16`(`hex`)/`array` (JSON array of numbers). `null` ↔ `nil`: decode accepts
  `null` → nil, nil marshals as `null` (empty non-nil → `""`/`[]`); no
  opening-quote fold
- `json.RawMessage` / `jsontext.Value` — opaque span via `ggen.SkipValue`, aliased
  into field. Raw passthrough on encode (`null` if empty/nil)
- `net/url.URL` — JSON string, `url.Parse` / `ggen.AppendURL`
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
  `SQLNullSpec` (NullByte's inner is spelled `uint8`, the canonical name
  `narrowIntBounds` knows — as `byte` the bytes path emitted no guard);
  `renderStreamSQLNull`'s int/uint arms emit `narrowIntGuard` before the
  `int16(nv)`/`int32(nv)`/`uint8(nv)` cast, so NullInt16/NullInt32/NullByte
  reject out-of-range with `ErrNumberOverflow` on both paths (opt #48 family;
  pinned by `TestSQLNull_NarrowOverflow`). **Generic `sql.Null[T]` supports any inner `T` ggen can render as
  a field**: with go/types info the parser builds a synthetic `FieldInfo` for `T`
  (via `extractFieldFromTypes`) stashed on `FieldInfo.SQLNullInner`; the
  decode/encode/size renderers delegate the `V` slot to the standard field
  emitters, so `sql.Null[T]` gets exactly the wire (and fast path) of a bare `T`.
  Parent flags (`MultiErr`/`NoValidate`/`UseNumber`/`HTMLEscape`) copy onto the
  inner. The AST-only loader (no go/types) keeps only built-in-primitive generic
  forms (`SQLNullSpec` + `isSupportedSQLNullInner` gate); custom inners there fall
  back to `encoding/json` on the whole value
- `any` / `interface{}` — decode via `ggen.Any(data, i, validate)` /
  `(*Stream).Any(validate)`, stdlib defaults: `null→nil`, `bool`,
  `number→float64`, `string` (zero-copy alias), `array→[]any`,
  `object→map[string]any`. With `usenumber`, `ggen.AnyNumber` (numbers →
  `json.Number`). `validate` is the same `vArg(f)` the string scans pass, so
  `allowinvalidutf8` reaches every string and key inside the value
  (`renderAny`/`renderStreamAny`/`unknownKey`/`streamUnknownKey`).
  `AppendAny`/`AppendAnyHTML` are depth-capped like
  the decode side and return `ggen.ErrMaxDepth` past the cap, so a cyclic or
  over-deep `any` value is an error instead of a fatal stack overflow; the
  encode counter bumps per RECURSION level (containers, pointer/interface
  derefs, struct fields), which is what makes a pointer cycle terminate.
  Encode via `ggen.AppendAny` (type-switch ordering —
  see `.claude/encode.md`)
- `[N]T` (fixed-length array) — JSON tuple with **strict count**: decode errors
  with `ggen.LenError{Want:N, Got:…}` when count ≠ N — `Got` is the exact count
  when too few; when too many the guard fires at the top of the element loop
  while `idx == N`, before the extra element is counted, so the true length is
  only known to be ≥ N+1 and both paths emit `tupleOverflowErr` (`Got: N+1,
  AtLeast: true`, message "length at least N+1"). Counting the rest would
  walk the whole tail of input already known to be invalid. `[0]T` marshals as the constant `[]` at EVERY position
  — field, slice element, array slot, map value — and the loop that carries
  it binds no value variable (`for range ref[1:]` + `,[]` for a container of
  them, `for k := range ref` for a `map[string][0]T`), since the element emit
  names none; the first-element unroll would index `ref[0]`, compile-time out
  of bounds. The predicate is `constEmptyElem(f)`, which reads the element
  TYPE (`arrayLenFromType(f.ElemType)` — the same source `sliceElemField`
  derives an element's `ArrayLen` from) rather than `f.ElemArrayLen`, which
  parse populates for slice/array elements only: a map value leaves it 0 and
  a type-blind test would also fire for `map[string][3]int`. A POINTER
  element (`map[string]*[0]int`) is excluded — it still binds its variable,
  since it can be `null`. Decode is the
  mirror — a no-slot tuple read that takes `[]` and nothing else,
  `LenError{Want: 0, Got: 1, AtLeast: true}` for a first element (via
  `tupleOverflowErr`), identical error and
  position on both paths. There is no element loop at all, which also keeps
  the emitter clear of a store into a zero-length array — gc itself fails on
  one, with an internal compiler error (backlog). Combines/nests freely
  (`[][N]T`, `[N][]T`, `[N][M]T`, …) via the same recursive emitter as `[][]T`;
  `sliceElemField` resets `ef.ArrayLen` before its switch (the tuple length
  belongs to the OUTER field; the `KindArray` arm re-derives its own), so a
  `[N][]byte` element is a variable-length base64 string and `byteArrayLen`
  fires only for a real `[N]byte` field.
  `[]byte` stays KindBytes (base64), and `[N]byte` folds onto the SAME base64
  path (`foldByteArray`, parse.go) with a strict decoded-length check —
  jsonv2 base64s byte arrays and rejects the v1 number-array form, so ggen
  sides with v2 as everywhere else the two disagree. `format:array` opts back
  into the v1 tuple of numbers. Only non-byte arrays get tuple treatment by
  default.
  A POINTER to one (`*[N]byte`, any depth) takes the same wire shape behind
  the usual nullable rung. The fold lives in Kind + ArrayLen, which the type
  STRING cannot express (`"[8]byte"` resolves to a plain KindArray with no
  element info), so every pointer-leaf derivation goes through
  `leafKind(f, leafType)` instead of bare `resolveKind`: the omitempty
  condition, AppendJSON, JSONSize, the bytes decode, the stream decode and
  `headSentinel` all keep the pointee at KindBytes, which is why the
  truncation sentinel follows the WIRE shape (`ErrExpectString`, or
  `ErrBadArray` under `format:array`) rather than the Go kind. Without it the
  six sites emitted a tuple of STRINGS assigned into bytes plus an unused
  `encoding/base64` import. `formatElemKind` peels pointer levels before
  `isByteArrayType`, so the whole `format:` set applies to `*[N]byte` exactly
  as to `[N]byte`

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
- Package mode, external test package (`package <pkg>_test`):
  `<dir>_xtest_ggen_test.go` (tagged: `<dir>_<slug>_xtest_ggen_test.go`),
  its own group with its own `generatedTypes` seeding. `xtest` rather than
  `test` so it cannot collide with a `//go:build test` slug bucket
- Package mode, tagged: `<dir>_<tag-slug>_ggen.go` per (tag, isTest) bucket
- Single-file: `<basename>_ggen.go` / `_ggen_test.go`; source `//go:build` line
  preserved in header; an external test file writes `<basename>_ggen_test.go`
  under `package <pkg>_test`
- `_test.go` sources are first-class inputs; test-only struct annotations route to
  `_ggen_test.go`

**Build tag propagation.** The generator reads `//go:build <expr>` per source file
and buckets annotated structs by constraint. Each (tag, isTest) bucket emits its
own gen file with a matching header — a struct in `tagged.go` (behind
`//go:build foo`) never lands in the unconstrained `<dir>_ggen.go`. Old-style
`// +build` honored; multi-term exprs canonicalized via
`go/build/constraint.Parse`. `fileBuildConstraint(f, filename)` ANDs the
explicit expression with `filenameConstraint` — go/build's rule (ignore
everything before the first `_`, strip a trailing `_test`, match `_GOOS`,
`_GOARCH`, `_GOOS_GOARCH` against `knownOS`/`knownArch`, which mirror
`internal/syslist`) plus `cgo` for a file importing `"C"` — so `os_linux.go`
buckets as `linux` (`<dir>_linux_ggen.go` / `os_linux_ggen.go` with
`//go:build linux`; `!nope && linux` when both exist). Go's suffix rule reads
only the last one or two `_` components, so the `_ggen` suffix masks the
OS/arch suffix and the generated methods otherwise compiled into every GOOS
(`undefined: OnlyLinux` when cross-compiling). Cross-bucket struct refs in the
same package still route through direct DecodeFrom (`generatedTypes` seeded
with the union of all buckets first). Tagged-bucket slugs collapse non-alnum
runs to single underscores (`goexperiment.simd` → `goexperiment_simd`,
`foo && bar` → `foo_bar`).

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
   Spec + tests live in `prealloc.Cap` (`internal/prealloc`); the emitted form is a
   package-level `const ggenCap_<prefix>_<n>` holding the same ladder written
   branchlessly over `unsafe.Sizeof(*new(E))`.
   **It has to be a constant, not a call**: measured on the bench module, gc
   inlines `prealloc.Cap` only into small functions — 30 of 34 emitted
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
8. **Embedded fallback map.** Unknown keys absorbed into `map[string]V`. V
   dispatches: `any` → `ggen.Any`/`s.Any`; `string` → `ggen.String`/`s.String`;
   ggen struct → its `DecodeFrom`/`DecodeFromStream`; else `ggen.SkipValue` +
   `json.Unmarshal` over the captured span.
9. **Marshal output cap.** `JSONSize()` upper bound → single `make([]byte,0,cap)` +
   `AppendJSON`. 1 alloc per top-level Marshal. `any` values are sized at
   runtime (#88); a non-generated foreign struct is still a flat 128.
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
    `posLit`. See `.claude/validation.md`.
14. **Parse-error wrapping at every error return.** Codegen embeds the JSON field
    name directly as the first arg of every error return:
    `return result, i, ggen.NewParseErr("street", i, err)`. The field literal is
    written at emit time (no post-pass / no `/*ggen-field*/` markers) — each
    `inlineScan*`/dispatch site interpolates the quoted JSON-path literal into its
    `NewParseErr` call. Zero runtime cost on the happy path.
    `NewParseErr(segment, pos, err)` builds `*ggen.ParseError{Path []string,
Pos, Err}` for raw sentinels (a one-segment path), prepends the segment onto a `ggen.Error`'s Path (value passes through, reachable via `errors.As`), and **chains** when err is already a `*ParseError` —
    prepending the segment onto `pe.Path` (deeper `Pos` kept) so nested surfaces
    `Error()`-join to `addr.street`. Empty segment leaves the path untouched.
    `errors.Is(err, ggen.ErrBadString)` works via `Unwrap()`; `ParseError.Error()`
    calls `e.Err.Error()` once so chained prints stay linear.
    **Bytes-path NESTED-decode sites use `NewParseErrShift(seg, i, _n, err)`**
    (2026-08): the callee ran on `data[i-_n:]` (the emit advances `i += _n`
    BEFORE the check), so its positional errors — chained `*ParseError`,
    validation typed errors, `ggen.Errors` — are rebased by the value
    start via `AddPos` before wrapping, making every `Pos` a full-payload
    offset like the stream path's `s.Offset()`. The multierr drain rebases
    with `ggen.ShiftPos(verr, i-_n)`. All emitted from ONE helper
    (`nestedDecodeErrCheck(field, multierr, bytesPath, nVar)`; `nVar` = the
    consumed-count local, `_n`/`_in`) + the alias delegation wrap +
    `ggen.UnmarshalSlice`. Sentinel/foreign errors carry no `Pos` → the
    shift no-ops. Error path only; happy path unchanged. The cross-package
    `UnmarshalJSON` rung rebases too — a hand-written `UnmarshalJSON`
    delegating to another package's generated decoder returns a positional
    ggen error with a span-relative `Pos`: bytes wraps with
    `NewParseErrShift(field, i, i-start, err)`, and the stream rung is the ONE
    stream site using `NewParseErrShift` (`field, s.Offset(), len(span), err`
    — the captured span starts at `Offset()-len(span)`), since its callee ran
    on a captured sub-slice. Pinned by `TestCrossPkg_unmarshalJSONRungRebasesPos`.
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
18. **`bytes.IndexByte` string scan.** `ggen.String`/`(*Stream).String`/`KeyView`/
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
    allocation. Falls back to `stringSlow` for escapes. See `.claude/scan.md`.
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
    See `.claude/encode.md` for ordering. Outpaces stdjson v1 and jsonv2 on map
    shapes.
33. **`AppendAny` concrete cases for `json.RawMessage`, `time.Time`,
    `time.Duration`, pointer-to-primitive.** These pre-empt the `json.Marshaler`
    branch / `reflect.Pointer` fallback. Concrete cases MUST sit before
    interface dispatches. `time.Duration` emits the units string a bare
    `Duration` field carries by default, so one document holds one shape and
    the any-path form decodes back into a `Duration` field (the reflect Int64
    arm emitted bare nanoseconds; stable jsonv2 has no default Duration
    representation and errors — documented divergence, same as the field
    emitter). `quotableKind` excludes `time.Duration` by type identity so
    `,string` on a reflected Duration field cannot double-wrap the string
    wire. The walker's other jsonv2-parity rules (pointer-receiver
    marshalers reached for values, text-marshaler map keys, omitempty on
    encoded bytes, omitzero via `IsZero()`, the `embed` splice) are in
    .claude/encode.md.
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
    `fieldSkipExpr` (`generate.go`) builds the non-pointer per-kind guards;
    its `omitempty` half is `omitEmptyCond` (see the `json:` section for the
    per-kind guards, incl. `ggen.AnyIsEmpty` for `any` and the pointer peel
    that ANDs the leaf's guard — the old shape treated any pointer as
    `!= nil`, so pointers to empty values and zero
    `net.IP`/`netip.*`/`url.URL` were emitted although the documented rule
    omits them). A struct leaf contributes no guard, so `*T` stays a bare
    `!= nil`. `[]byte` (`KindBytes`)
    shares the slice/map "len > 0" arm; a `[N]byte` gets NO omitempty guard
    (N base64 bytes are never JSON-empty — the constant-true `len(arr) > 0` it
    used to emit was dead). `omitzero` on a `[N]byte` (KindBytes with
    `byteArrayLen > 0`) emits `zeroCompare` (`ref != ([16]byte{})`) — lumping
    it with the nillable kinds emitted `ref != nil` against an array, a type
    error; slices keep `!= nil`. `omitzero` on `KindStruct`/`KindSQLNull`/
    `KindArray` emitted `ref != (T{})`, which does not compile when T holds a
    slice/map/func/chan field (Go comparability); `zeroCompare` now checks a
    new `FieldInfo.NotComparable` flag (`!types.Comparable(field.Type())`,
    set at both extraction paths in `parse.go`) and falls back to
    `!reflect.ValueOf(ref).IsZero()` for those — comparable structs still get
    the cheap `!=` form. Pinned by `TestOmitEmpty_JSONEmptyKinds` +
    `TestAnyIsEmpty` + `TestGeneratedCompiles`.
36. **Brace-less value emitters.** Decode value emitters write locals straight into
    the caller's scope — no `{ … }` wrapper per value (slice/array/map, time/
    duration/netip/url/big\*/raw/sqlnull/any/string-tag/struct/bytes, cross-pkg
    fallbacks, embedded fallback). Sound because every call site owns its scope.
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
    int/uint scanners (`inlineScanInt64Stmt`/`Uint64Stmt`) and runtime `ggen.Int64`/
    `Uint64` split the digit loop: the first ≤18 (int) / ≤19 (uint) digits
    accumulate with NO per-digit overflow check (`10^18-1 < MaxInt64 < |MinInt64|`,
    `10^19-1 < MaxUint64`). A 19th/20th digit resumes the original checked loop,
    keeping `ErrNumberOverflow` identity and error position bit-identical.
    Accept-set unchanged. Pinned by `TestInt64_OverflowBoundaryLattice` /
    `TestUint64_OverflowBoundaryLattice`. Stream keeps the checked loop (mid-number
    ReadMore refill complicates the window).
40. **Whitespace-skip `<= ' '` exit gate.** `inlineSkipWS` and `ggen.SkipSpace`
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
    .claude/scan.md "Stream copies vs bytes-path aliases" / "`StringView`".
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
    `scanStringFn != "ggen.String"`, `inlineScanStringVar` swaps its unbounded
    scalar hot loop for an **inline fused vector classify** (no call): one
    `LoadUint8x{16,32}Slice` + Equal/Equal/Min-Equal → ToBits → TrailingZeros
    finds the first structural byte; a quote hit takes the inline alias/copy
    path, anything else (escape, ctrl, span ≥ lane) falls to the direct
    `ggen.StringAVX`/`AVX2`/`AVX512` call, which restarts at `posIn` — error
    identity byte-identical. Lane by tier: avx → 16 B, avx2/avx512 → 32 B
    inline (64 B inline never pays). Full-lane loads only, and only behind a
    bound check — `Load*Part` is a real CALL, not an intrinsic — so a string
    starting within one lane of the payload end takes a **bounded scalar
    tail loop** instead (< lane iterations; without it, tiny payloads whose
    fields all sit near EOF paid a tier call per string — measured Tiny
    +26%). INVARIANT: neither generated code nor any runtime tier ever reads
    past `data[len(data)-1]`. The runtime-side reason is that go1.27's
    `LoadUint8x64Part` is not fault-safe (an unmasked 64-byte load plus a
    zeroing mask-move — it reads 64 bytes and faults when the input ends
    within 63 bytes of unmapped memory, which mmap'd files and foreign buffers
    do), so the avx512 tails are built from an in-bounds backward-overlapping
    load or a zeroed stack copy (.claude/scan.md); any future emitted or
    runtime 64-lane tail must keep that shape. Pinned by
    `TestSIMD_NoOverRead` (guard-page probe over every tier, run in a child
    test binary). Broadcast constants are
    emitted per site; gc CSEs them across sites and hoists them out of loops.
    **Short-key override:** for the dispatch KEY scan, when every declared
    JSON name is ≤ 5 bytes (`maxJSONNameLen`), the vector classify's
    dependency chain (~load+3 compares+movemask+tzcnt) loses to a handful of
    predictable scalar iterations, so `inlineScanStringWin` emits a bounded
    scalar window sized `maxKey+1` instead (unknown longer keys fall to the
    tier call). That flipped Tiny_Unmarshal from +15% to −8%. The 6 direct
    `ggen.String` emit sites (alias, map value, TextUnmarshaler feed, …) swap
    the callee name. **Marshal side:** `appendStrFn` appends the same tier
    suffix (`simdSuffix`), routing every string-append site to
    `ggen.AppendString{,NoHTML}{AVX,AVX2,AVX512}` — length-gated fused
    escape scans (see `.claude/encode.md`). `-copy` detaches via
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
    to the per-tier Stream methods (`simd_stream_amd64.go` — fused
    locate per buffered window, refill loop unchanged). KeyView keeps the
    scalar prelude on all-short-key structs via the same `maxJSONNameLen ≤ 5`
    gate. Mega_Reader −5.2%, NoAlloc_Reader −7.7%, Small_Reader −20/−26% at
    avx512. **Skip side:** bytes decode bodies render through a scratch
    buffer and a post-pass rewrites `ggen.SkipValue(` → the tier skip tree
    (`simd_skip_amd64.go` — vector whitespace runs + fused skipString;
    SkipHeavy compact −21.6% / pretty −29.9%, Mega −8.3%); `inlineSkipWS`
    consumes one WS byte inline (compact and single-space payloads stay
    call-free) then hands 2+ runs to `ggen.SkipSpace<tier>` (pretty
    full-decode −3.3%; costs Tiny ~+1%, accepted). Stream decode bodies get
    the same swaps via `tierStreamStringCalls` (`= s.SkipValue()` /
    `= s.SkipSpace()` → per-tier Stream methods,
    `simd_skip_stream_amd64.go`): SkipHeavy ggen_stream compact −23.8% /
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
    (or hits an escape/ctrl byte) to `ggen.String`, whose `bytes.IndexByte` locate
    is SIMD/AVX2 — so long strings (bios, URLs) ride the vectorized path while
    keys/short/medium values (the ≤32 B population that dominates real payloads)
    stay inline. `ggen.String` is the error-identity source of truth
    (ErrUnterminated/ErrBadString); the bounded loop only fast-paths a clean
    quote-terminated span, so byte-parity holds (pinned by the bytes-vs-stream
    fuzzer + whitespace/escape tests). `-copy` detaches the handoff via
    `ggen.Detach` (opt #49) — clones only when it aliased. window < 0 = unbounded
    original loop, used ONLY for the **dispatch key** scan: keys are short and
    matched against known field names, so a window bound is pure per-key setup with
    no long span to hand off (it regressed tiny structs +8%). Interleaved A/B vs
    the unbounded scalar tier: Small_Unmarshal −50.9%, NoAlloc_Unmarshal −19.0%,
    Tiny/Mega/MapHeavy/ValidationHeavy flat. Window sizing matters: 16 regressed
    Mega (medium strings paid a `ggen.String` CALL per string); 32 keeps Mega's
    ≤63 B strings inline. The SIMD tier is untouched (window ≥ 0 still routes to
    the inline vector classify).
48. **Narrow-integer overflow guard (`narrowIntGuard`).** Fixed-width int fields
    smaller than 64-bit (`int8/16/32`, `uint8/16/32`) scan into a wide int64/uint64
    then cast. A bare cast silently TRUNCATES (`uint8` ← 300 = 44, nil error) —
    diverging from encoding/json v1 AND jsonv2, which both reject. Now every
    narrow cast is preceded by an in-range check returning `ggen.ErrNumberOverflow`
    (one predicted compare on the happy path, ~0 cost). Emitted at ALL narrow
    sites: struct field, map value, slice/array element, and pointer leaf (fast +
    slow), bytes + stream (`inlineScanInt64`/`Uint64`, `widenedScan`, the stream
    map/slice widen branches, the pointer cascade, `renderStreamSQLNull`'s
    int/uint arms). `int`/`uint` stay unguarded (64-bit on target platforms —
    no truncation). float32 needs no guard: it scans through `ggen.Float32`
    (see Supported Go kinds), whose range check maps strconv's `ErrRange` to
    `ErrNumberOverflow` — the boundary is a correctly rounded float32 parse,
    not a float64 cast landing on ±Inf. Pinned by `TestNarrowFloatOverflow` +
    `TestNarrowIntOverflow` (integrationtests, differential vs encoding/json over
    field/map/slice/pointer × bytes/stream). Same audit also fixed a MARSHAL bug:
    `renderAppendMap`'s value switch was missing `int8/16/32`, `uint/uint8/16/32`,
    `float32`, so `map[string]uint8` (etc.) marshaled `{"k":}` with no value —
    those kinds now emit the value.

    **`json:",string"` numerics inherit the bare form's grammar, range and
    narrowing checks** through one shared `renderQuotedNumber` (bytes +
    stream): the unquoted span goes through the bare-form scanners —
    `qn, qe, err = ggen.Float32/Float64/Int64/Uint64(unsafe.Slice(
    unsafe.StringData(sv), len(sv)), 0)`, then `if err == nil && qe != len(sv)
    { err = ggen.ErrBadNumber }`, then `narrowIntGuard` for narrow ints. It
    replaced `strconv.ParseFloat/ParseInt/ParseUint`, which accept Go-literal
    syntax (`NaN`, `Infinity`, `+1`, `01`, `1_0`, `1.`, `.5`) that jsonv2
    rejects — the same decode/skip asymmetry opt #52 closed for bare numbers,
    one level over, and a NaN/Inf admitted through the quoted form could not
    be marshaled back. Error identity is the ggen sentinels
    (`ErrBadNumber`/`ErrNumberOverflow`) instead of `*strconv.NumError`; Go
    1.27's legacy v1 is more lenient here (intentional divergence). Generated
    files drop the `strconv` import at these sites and gain `unsafe`. Earlier
    (round 4) the same two renderers bare-cast the wide parse (`ref =
    int8(n)`) with no guard, so `"300"` into an `int8` wrapped to `44`. Pinned
    by `TestStringTag_quotedTextTakesNumberGrammar`.

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
49. **Single-copy `-copy` escape strings (both tiers, via `ggen.Detach`).** In
    `-copy` mode a RETAINED escaped string used to double-allocate: the fall path
    emitted `sv, i, err = ggen.String(...)` (escape arm → `stringSlow` returns a
    fresh owned scratch) then `dst = strings.Clone(sv)` — the clone is redundant,
    `stringSlow` already owns the bytes. `ggen.Detach(s, data)` (scan.go) clones
    IFF `s` aliases `data` (a `uintptr` pointer-range test; a `stringSlow` scratch
    is a distinct heap alloc → skipped, non-moving GC makes it sound) — so the
    copy fall calls the SAME aliasing tier func then `Detach`, dropping the clone
    on escapes. **Tier-agnostic: reuses `ggen.String`/`StringAVX*` directly, NO
    per-tier `StringCopyAVX*` variant.** Wired at the copy fall
    (`inlineScanStringWin`, both scalar + SIMD blocks), the embedded-fallback string value
    (`unknownKey`), and `ggen.AnyCopy`/`AnyNumberCopy` (values + object keys).
    `EscapeHeavy/ggen_copy`: 8→4 allocs — identical to the aliasing `ggen` row at
    both scalar and avx512 (on escapes the -copy detach is FREE — stringSlow
    already owns). Same pass fixed a pre-existing SIMD gap: `StringAVX*`'s
    `classifyStructural` sized `stringSlow` off the first quote (an escaped `\"`),
    not the real unescaped close via `stringSpanEnd` — SIMD escape decode 44→4
    allocs. Wire-identical; pinned by `TestDetach` (scan), `TestStringSIMD_Parity`,
    `TestCopy_EscapedDecouples` (integ, every copy site), and
    `EscapeHeavy/ggen_copy`'s scribble guard.

50. **Decode-side UTF-8 validation (jsonv2 parity, `ggen.ErrInvalidUTF8`).**
    Every string-PRODUCING decode path rejects malformed UTF-8 and unpaired
    `\uXXXX` surrogates (v1 would silently substitute U+FFFD; ggen sides with
    jsonv2, which errors). Runtime mechanics live in .claude/scan.md (fused
    high-bit accumulate → `utf8.Valid` only on non-ASCII spans). Codegen side:
    every inline string fast path must BAIL to the validating runtime func on
    non-ASCII — the scalar windows (value window, dispatch-key unbounded loop,
    SIMD near-EOF tail) add `data[ke] < 0x80` to the loop condition (high byte
    exits → not '"' → `ggen.String*` fall), and the inline vector classify
    folds ctrl AND ≥0x80 into ONE range term (`d := v.Sub(0x20); d.Max(0x60).
    Equal(d)` — `(v-0x20) >= 0x60 ⇔ v < 0x20 || v >= 0x80`), replacing the old
    Min-Equal ctrl term at the SAME op count, so ASCII fast paths pay ~zero
    (an unfused extra Max-Equal-Or term first measured DeepNested +13% at
    avx512; the fold brought it back to +2.8%). Captured raw spans
    (`renderRawJSON`/`renderStreamRawJSON`) emit `ggen.CheckUTF8(span)` after
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
    scratch anyway). The four bytes `Any*` families and both stream walkers
    take the same `validate` (a span-level switch exactly like `String`'s —
    the per-byte loops never test it), and `renderAny`/`renderStreamAny`/
    `unknownKey`/`streamUnknownKey` pass `vArg`, so `any` fields, every
    string and key nested inside them, `map[string]any` values and the
    `json:",embed"` catch-all follow the struct's setting in the float64,
    usenumber and `-copy` shapes. Pinned by `TestAllowInvalidUTF8` +
    `TestAllowInvalidUTF8_anyValues` (integ: every string shape + raw + any,
    bytes + stream, grammar-errors-still-reject, strict control). **Cost** — RE-MEASURED 2026-07 on a `performance`-profile box with a
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
      vectorized `validUTF8x16` Lemire pass (.claude/scan.md), which is why the
      avx512 penalties are far below the scalar ones. All still 2.7-5× ahead of
      jsonv2, which does the same validation.
    - **Not measurable on this box**: Small, Tiny, DeepNested, ValidationHeavy
      — controls drift 4-27%, so no ggen delta there means anything. Pinned by
    `TestString_InvalidUTF8Rejected`/`TestString_LoneSurrogateParity` (scan),
    `TestStreamStringInvalidUTF8` (stream, incl. rune-straddles-refill),
    SIMD parity corpora, `TestInvalidUTF8Rejected` (integ differential vs
    jsonv2), and `FuzzPrimitivesCompat`'s reject-parity branch (the old
    fuzz blind spot that hid this bug — it SKIPPED invalid-UTF-8 strings).

51. **Recursion depth cap (`maxDepth` = 10000, jsonv2 parity).** Every
    recursive decode path was unbounded → a few MB of `[[[[…` / `{"k":{"k":…`
    was a FATAL, unrecoverable goroutine stack overflow (not a `recover`-able
    panic — the process dies). Now capped:
    - **Runtime** (`scan.go`, `stream.go`, both SIMD skip files): `SkipValue`
      and the four `Any` families keep their public signatures but delegate to
      a `depth`-threaded core (`skipValue`/`anyValue`/`skipValueAVX*`/stream
      mirrors); each container-OPEN checks `depth > maxDepth` → `ErrMaxDepth`.
      One predictable compare per `[`/`{`, nothing on scalar values.
    - **Codegen**: only SELF-REFERENTIAL structs change shape. `computeCyclicTypes`
      (generate.go) scrapes type identifiers out of every field's
      `GoType`/`ElemType`/`PointeeType`/`SQLNullInner`/alias-underlying and
      finds which generated types can reach themselves (over-approx — a false
      hit only costs an unneeded, still-correct shim). A cyclic struct T emits
      `DecodeFrom(data)` → `recv.decodeFromDepth(data, 0)` shim + a
      `decodeFromDepth(data, _depth int)` core that guards `_depth > maxDepth`;
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
      fast path would accept `01` while its own `ggen.Int64` fall path rejects
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
    `ggen.CloseJSONString{,HTML}` (the same escape-on-dirty closer the
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
    `FieldInfo.NamedPrims` (parse time, before `namedKinds` is seeded, pointer
    stars stripped so `*Score` resolves too) then `effectiveKind` (render
    time). A converter's input W is registered in `NamedPrims` by
    `resolvePipeCustoms` (see the `pipe:` section) — the field-side lookup
    alone never saw it.

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
    `emitNoCloseAfterComma` site returned BARE `ggen.ErrBadArray`/`ErrBadObject`
    (no path, no pos, `errors.As[*ggen.ParseError]` failed) while the stream
    twins and the map/bytes-value guards all wrapped. All bytes-path structural
    guards now emit `ggen.NewParseErr(field, cursor, sentinel)` — including
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
    `s.Pos` into `ggen.NewParseErr`, while the validation-error sites in the
    SAME function already used `s.Offset()`. `Pos` only equals the payload
    offset until a compacting refill slides the window, so the reported
    position collapsed toward 0 and CHANGED WITH THE CHUNK SIZE (`{"i8":128}`
    → 9 on bytes, 0/0/2 on stream at 1/3/7-byte chunks). Round-4 fixed the
    identical class inside the stream slice walker (then
    `ggen.UnmarshalSliceStream`, now `(*ggen.Stream).Slice`) but not the
    emitters;
    round-5's pin used a 16-byte payload, where the window never slides and
    `Pos == Offset()` by accident. Pinned by
    `TestParseError_StreamPosIsPayloadOffset` (9.5 KB payload × chunk sizes ×
    a 64-byte buffer).

60. **Generated stream refills map drained → the bytes-path sentinel.**
    The struct dispatch loop's `ReadMore` guards returned the RAW reader error,
    so truncation surfaced `io.ErrUnexpectedEOF` where the bytes path reported
    a grammar sentinel. The two guard positions now carry the sentinel the
    bytes path returns for the same truncation (`ErrExpectString` at a key,
    `ErrBadObject` past a value) via the newly exported `ggen.NotEOF`, so
    transient reader errors still propagate raw. Sentinels AND positions
    match bytes at every chunk size — the emitters stamp `s.Offset()`
    verbatim, so the runtime's rebase target IS the user-visible position, and
    every stream primitive now leaves `Offset()` on the byte its bytes twin
    returns (the stop cursor for numbers, the give-up byte for literals and
    skips, `len(buf)` for a value that ran off the end — .claude/scan.md
    "Aggressive compaction"). The value-head convention the stream used to
    rebase to was never chosen: `CaptureValue`'s span-head made a malformed
    `RawMessage` deep inside a large blob report its FIRST byte on the stream
    only (40 vs 7). `streamUnknownKey`'s single-error and multierr arms clone
    the key, consume the colon (with the usual error check) and only THEN
    build the `UnknownKeyError` / append it before `SkipValue`, so its `Pos`
    is the value head on both paths like `DuplicateKeyError`'s, and a key with
    no colon (`{"zz" 1}`) is `ErrBadObject` in a `*ParseError` whose field is
    the key on the stream too — they were the only per-key sites stamped
    before `ConsumeColon`. One residual, deliberate: at the colon site the
    stream carries a field path and bytes does not — the check lives at
    different stages (bytes before dispatch, stream inside each case via
    `ConsumeColon`), so bytes has no field name there; sentinel and position
    agree. Pinned by `TestParseError_StreamPosChunkInvariant` (table-driven
    over `decodeBothChunked[T]`, asserts `spe.Pos == bpe.Pos`) +
    `TestRead_unknownKey_streamParity`.

61. **Foreign errors from converters and fallible mods wrap in `NewParseErr`.**
    An error-form `@Conv` / `@Mod` returned its own error bare — no path, no
    offset, `errors.As[*ggen.ParseError]` false — while the bool forms built
    a typed `ggen.ModError`. Both now wrap (the underlying error stays
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

63. **The scalar inline key scan falls to `ggen.String` on any non-quote.**
    Its early-bails reported the OPENING QUOTE's offset for
    unterminated/ctrl-byte strings while the SIMD tier always falls and reports
    `ggen.String`'s give-up position — the same struct built with and without
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

66. **Every alias decoder skips leading whitespace.** Whitespace is legal
    before any top-level value. Slice/map/array aliases get it free from
    `ArrayOpen`/`ObjectOpen`, and the struct ladder's ByteDecoder /
    JSONUnmarshaler rungs from the delegated `DecodeFrom` / `SkipValue`; the
    three shapes that scan at the cursor — primitives, the `[]byte` alias's
    stream arm, and the struct ladder's TextUnmarshaler rung (both paths) —
    emit the skip explicitly. Pinned by `TestAlias_leadingWhitespace` +
    `TestAlias_leadingWhitespaceBytesAndText`.

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

68. **Round-8 fixes (alias flags, variant null, omit on named prims, unix
    time, big.Float size).** Six related defects:
    - **Container + primitive aliases dropped struct flags.** `AliasField` is
      built at parse time, BEFORE `applyCLIFlags` (which only walks `Fields`),
      so `copy`/`htmlescape`/`allowinvalidutf8`/`multierr` on
      `type Tags []string` were silently ignored — copy-mode elements still
      ALIASED the input (silent corruption class), htmlescape emitted NoHTML
      appends. `aliasContainerField` (alias.go) stamps struct flags at all
      render sites; the primitive string alias emits `ggen.Detach` under
      `copy`.
    - **Converter variants made JSON `null` a hard error on null-aware
      kinds.** `variantCaseBytes`'s native arm never claimed `'n'`, so
      `P *int` + `pipe:"./@Conv"` rejected `{"p":null}` that the plain field
      decodes to nil. Native now claims `'n'` for pointer/slice/map/bytes/raw
      (`nativeAcceptsNull`) unless an explicit `nullzero` variant claims it.
    - **`omitempty`/`omitzero` on named primitives.** `fieldSkipExpr` never
      resolved through `effectiveKind`, so a `type Score int` field hit the
      KindStruct arm: omitzero emitted `!= (Score{})` (does not compile),
      omitempty had no arm (option silently dropped, `{"s":""}` emitted where
      v1+jsonv2 omit).
    - **`hint:` inner levels now go through the applicability matrix** —
      `hint:"inner:8"` on a non-container element was parsed and silently
      ignored.
    - **Bool-form `ModError` wraps in `NewParseErr`** (all three emit sites —
      renderOneMod + both converter assigns), carrying the field path as
      mod_error.go always documented.
    - **`format:unix` marshal routes through `ggen.AppendUnixSeconds`**
      (exact seconds + fractional nanos from `Unix()`/`Nanosecond()`), not
      `float64(UnixNano())/1e9` — which silently emitted garbage outside the
      int64-nano range (~1678-2262) and lost sub-100ns precision. unix budget
      24 → 32 (19-digit seconds). And **big.Float `JSONSize` is
      precision-scaled** (`Prec()/3 + 24`, was flat 66 — undersized for
      user-raised precision, breaking the single-alloc Marshal contract);
      dropped from `constSizePerEntry` so container elements take the
      per-element loop.
    Pinned by `TestGenerate_round8Fixes` (cli) + `TestAppendUnixSeconds`
    (encode).

69. **Round-9 fixes (parse layer + stream emitters).** Nine defects, all
    reproduced by the audit:
    - **Map-valued sibling structs never queued** — `referencedStructName`
      had no `*ast.MapType` case, so a struct reached only through
      `map[string]Inner` missed `generatedTypes` and its values fell to the
      reflective `json.Unmarshal` fallback (silently lenient semantics).
    - **Embedded promotion lost depth** — a depth-1 promoted field clashing
      with a depth-2 one dropped BOTH (`{}` on the wire) where stdlib keeps
      the shallower. `FieldInfo.EmbedDepth` + depth-aware
      `resolveFieldCollisions` (min-depth wins; own tie errors; promoted tie
      drops; every ggen field is json-tagged, so stdlib's tagged tiebreak can
      never differentiate). The dominance rule applies to JSON names (and the
      `embed` catch-all) only — two surviving fields that share a GO name with
      distinct JSON names are a generate-time REJECTION, not a stdlib-style
      keep-both (opt #78).
    - **Cyclic embedding crashed the generator** (stack overflow) —
      `extractStructSeen` threads the embedding chain, diagnostic instead.
    - **A tab after `//ggen:generate` silently dropped the annotation**
      (go:generate accepts tabs) — any whitespace separates now.
    - **Single-file mode seeded `multiErrTypes` file-locally** — a
      cross-file multierr callee lost its drain branch (same class as the
      round-6 cross-file cycle fix); parseFile now returns a package-wide
      set unioned in.
    - **Stream `,string` STRING values retained a KeyView alias**
      (`string(sv)` is an identity conversion, no copy) — silent corruption
      on the next compacting refill. Fixed, then SUPERSEDED same round by a
      user call: `,string` on a string field is now a generate-time ERROR
      (see the `json:` tag section) and both KindString string-tag arms are
      deleted. The same identity-conversion misconception was fixed for real
      in `renderStreamNetIP`'s error literal (`strings.Clone`).
    - **Generated value-head refills leaked raw `io.ErrUnexpectedEOF`** —
      `streamReadMore` gained a sentinel param (`ggen.NotEOF`) and every
      site maps a drained refill to the bytes-path sentinel via
      `headSentinel(f)` over `truncSentinel` (the kind table); null-literal
      refills map to ErrBadLiteral (round-6 #60, extended from the dispatch
      loop to all emit sites). `headSentinel` follows the WIRE shape, not the
      Go kind: it resolves a pointer chain to its leaf (`**int` →
      ErrBadNumber, not ErrBadObject — parse resolves only one pointee level,
      so the kind read KindStruct), maps `KindSQLNull` through `SQLNullSpec`
      to the inner kind (`*sql.Null[int]` → ErrBadNumber, not
      ErrUnexpectedEnd), returns ErrExpectString for any non-bool `,string`
      field and ErrBadArray for a `format:array` `[]byte`
      (`renderStreamBytes` hardcoded ErrExpectString before its format branch
      ran). Used at the pointer-branch refill, all three nullzero sites, the
      generic sql.Null inner refill and `renderStreamBytes`. Pinned by
      `TestTruncationSentinelParity_WireShape`.
    - **`format:array` byte elements silently wrapped** (`300` → `44`, nil
      error, both paths) — now `> 255` → `ErrNumberOverflow` (the opt #48
      family's missed site).
    - **sql.NullTime's synthesized inner field dropped
      Copy/AllowInvalidUTF8/MultiErr** (both paths — opt #67 class).
    Pinned by `TestParseFile_round9` (cli) and
    `TestTruncationSentinelParity` / `TestStringTag_StreamStringDetached` /
    `TestFormatArray_ByteOverflow` (integrationtests).

70. **Round-10 fixes (aliasing under `-copy`, alias imports, stream key
    lifetime).**
    - **`url.URL` fields ignored `-copy`** — `renderURL` hardcoded the
      aliasing scan, and `url.Parse` SLICES Scheme/Host/Path/RawQuery/Fragment
      out of its argument, so a copy-mode struct still pointed at the caller's
      buffer (the stream path already used the copying `s.String` for exactly
      this reason). The scan now takes `f.Copy`. Pinned by
      `TestCopy_URLDecouples`.
    - **Container-alias stdlib imports were never collected** — `collectImports`
      walked only `s.Fields`, which a container alias leaves empty (its shape
      lives in `s.AliasField`), so a `[]byte` alias emitted
      `base64.StdEncoding.AppendEncode` into a file with no `encoding/base64`
      import. `collectFieldImports` now runs over `AliasField` for
      bytes/slice/array/map aliases.
    - **`streamUnknownKey` cloned the KeyView alias too late** — the
      ignoreunknown and multierr branches evaluated `strings.Clone(key)` inside
      the error checks, i.e. AFTER `ConsumeColon`/`SkipValue` had compacted the
      buffer out from under the alias, so a returned `*ParseError` carried
      shifted bytes as its path. Both clone into `ownKey` first, like the
      embedded-fallback branch. Pinned by
      `TestIgnoreUnknown_streamErrorKeepsKeyName`.

71. **Round-11 parse-layer fixes (element resolution).**
    - **`ElemIface` is populated by the primary struct parse path.** Only the
      struct-alias introspection path (`extractFieldFromTypes`) ever set it,
      so `elemAsField` handed the element ladder an empty probe and every
      slice/array/map element of a foreign ggen type fell to the bottom
      `encoding/json` rung while the SAME type at FIELD position took its
      `DecodeFrom` — ggen wire at one position, v1 wire at the other.
      `extractStructSeen` now probes the element / map value / pointee via
      `s.elemIface`. (`aliasContainerField` stored its element probe in
      `Iface`, the field slot, and is fixed to `ElemIface` too.)
    - **Foreign element types are spelled by package NAME.**
      `aliasContainerField` and `extractFieldFromTypes` used
      `types.RelativeTo`, which elides only the current package and spells
      every other one by full import path — `example.com/x/sub.Foreign` does
      not parse. Both take `structSet.pkgQualifier` now, the qualifier
      `collectNamedPrims` already used.
    - **A struct reached only through a container alias is queued for
      generation.** `resolveFiltered`'s alias branch never walked the alias's
      underlying type, so `type L []Inner` left `Inner` method-less and its
      elements decoded through the reflective fallback — which silently
      accepts unknown keys, case-insensitive names and dups. Slice/array/map/
      pointer underlyings now run `referencedStructName`.
    Pinned by `TestForeignElementCodegen` /
    `TestForeignGgenElementDecodesDirectly` (cli) and
    `TestAlias_elementStructGetsGenerated` (integ).

72. **Round-12 integration fixes (the halves round 11 left open).** Round 11
    fixed element RESOLUTION in the parse layer; three consumers of that data
    were still missing, so the resolved information went nowhere.
    - **A container alias's foreign element imports are collected.**
      `aliasContainerField` populates `AliasField.TypeImports`, but
      `collectImports`' alias branch harvested only the stdlib helpers
      (`collectFieldImports`), never the foreign packages — so
      `//ggen:generate type AliasThings []sub.Foreign` emitted a file naming
      `sub.Foreign` with no `sub` import and did not compile. The alias branch
      now drains `TypeImports` the way the `Fields` loop does.
    - **Slice/array elements and map values marshal through the cross-package
      ladder.** The encode arms tested only `isGenerated(f.ElemType)` (this
      pass) with no foreign rung, so a foreign ggen element DECODED via
      `DecodeFrom` (round 11) but MARSHALED via `json.Marshal` — the wire
      asymmetry round 11 set out to kill, surviving on the encode side.
      `emitSliceElement` and `renderAppendMap` now call
      `renderCrossPkgStructAppend(sliceElemField(f), vref)`, the same ladder
      the field level uses. The slice arm braces it: the temp-declaring rungs
      emit once for the first element and once inside the loop.
    - **`[N]byte` aliases fold onto the base64 path.** The `*types.Array` alias
      arm never ran `foldByteArray`, so `type Digest [4]byte` emitted a JSON
      number array while a `[4]byte` FIELD emitted base64 — one Go type, two
      wire shapes by position (and contradicting what README/SKILL already
      documented). The arm folds and flips `AliasKind` to KindBytes;
      `renderAliasContainerDecode`'s receiver reset is gated on
      `AliasField.ArrayLen == 0`, since a folded byte ARRAY has no nil state
      and cannot be resliced.
    Pinned by `TestAlias_byteArrayIsBase64` (integ) and the round-11 cli tests,
    which now also cover the encode side.

73. **Stream raw-span decode reuses the receiver's backing.**
    `renderStreamRawJSON` emitted `append(make([]byte, 0, len(span)), span...)`,
    a fresh allocation on every decode, while the bytes-path `-copy` twin
    already reused the carried backing. It now emits
    `append(ref[:0], span...)` — a nil receiver still allocates through
    `append`, and both shapes detach from `s.buf` equally, so the wire and the
    lifetime are unchanged. Steady-state re-decode into a reused receiver
    (`Seq`/`Value(prev)`) drops from 1 alloc per raw field per value to 0.
    Pinned by `TestRawJSON_StreamReusesReceiverBacking` (integ, `RawOnly`).

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
  `utf8.RuneCountInString`, `strings.HasPrefix`, `ggen.IsURL`, and into the
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
`ggen.String` per field, and an UNANNOTATED named type had no methods to call at
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
  emitted a bare `ggen.SkipValue` (every element of a `[]foreign.T` silently
  decoded to its zero value) and the stream path emitted nothing. Both now run
  the same ladder as the field level via `elemAsField`; map values run it too,
  instead of always reflecting over the captured span.
- **Foreign type spelling: one qualifier per import path per pass.**
  `structSet.qualifierFor` assigns each imported package the DECLARED
  package name unless another package of the pass or a package-level
  identifier already claims it (then `name2`, `name3`, …);
  `structSet.spell` renders AST type expressions through it (package
  identifiers resolved via `typesInfo.Uses`), and `pkgQualifier` /
  `collectTypeImports` / `fixConstArrayLens` use the same table.
  `TypeImport.Explicit` marks a qualifier that differs from the declared
  name, and `importSpec` in generate.go writes those as `leaf2 "…/bleaf"`
  lines; `AliasUnderlyingImport` and `SQLNullImports` are `TypeImport`-typed
  so they take the same path. Before, `FieldInfo.GoType`/`ElemType`/
  `PointeeType` were spelled from the SOURCE expression (`lf.Name` under
  `import lf "…/leaf"`) while the import scan matched the declared name, so
  the import was dropped (`undefined: lf`), a named string reached through
  an alias missed `namedPrims`, and two packages with one declared name
  collapsed in the qualifier map. Only the `@pkg.Func` side still spells the
  declared name (backlog).
- **A user package named like an emitter literal is aliased `<name>_`.**
  `emitterPackages` (generate.go) maps each qualifier the emitters spell
  (`ggen`, `archsimd`, `base32`, `base64`, `big`, `bits`, `bytes`, `fmt`,
  `hex`, `json`, `jsontext`, `math`, `net`, `netip`, `reflect`, `sql`,
  `strconv`, `strings`, `time`, `unsafe`, `url`, `utf8`) to the package it
  means; `structSet.qualifierFor` spells a colliding user package as
  `json_` (and a second one under the same name as `leaf2`), the parse layer
  routes every type spelling through it (`pkgQualifier` for go/types
  spellings, `collectTypeImports`, `structSet.spell` — an `exprToStringQ`
  with a package-ident hook that replaced `exprToString` in `extractField`
  and the alias-underlying branches, so AST-derived and go/types-derived
  spellings agree), `collectImports` carries path → alias
  and `writePrelude` emits `json_ "example.com/p3/json"`. Before, the stdlib
  scan added `encoding/json` AND the foreign scan the user path (`json
  redeclared`). The stdlib/foreign body scans (`scanBodiesForStdImports` /
  `scanBodiesForForeignImports`) match a qualifier only at an identifier
  boundary (`namesQualifier`: the preceding byte is not an identifier byte,
  or offset 0), so a field named `Uptime`/`Datetime`/`Habits`
  (`result.Uptime.DecodeFrom(` contains `time.`) does not pull in
  `time`/`bits`/`json`. Pinned by `TestGeneratedCompiles` (packages
  `uptime`, `jsonpkg`, `zeroarr`, `bytearr`) + `TestParseLoad/AliasedImport_SpelledByQualifier`.

### Pointer-to-container at any depth

The decode path only allocates a pointee when the POINTER is nil, and the
container emitters append into / fill whatever they are handed, so a
pointer-reached container has to be emptied by whoever hands it over. That
is the pointer SEED (`emitPointerSeed`, see the Decode-into-receiver pointer
bullet): `v = (*p)[:0]` for a slice leaf, `v = *p; clear(v)` for a map leaf,
at any depth and wherever the chain lives — `emitReceiverReset` has no
pointer branch, so the seed is the single place this happens and the entry
reset covers plain top-level containers only. Separately, the parse layer
peeled exactly ONE pointer level before the container switch that fills
`ElemType`/`ElemKind`, so at depth ≥ 2 the element kind stayed at its zero
value (KindString) and `**[]T` emitted a string scan into a T slot — both
loaders now peel to the innermost type.

### `oneof` frozen slices are scoped per output file

`ggenOneof0` restarted at 0 for each output file, so two sources in one package
that both used `oneof` declared the same package-level var twice. Names are now
readable and hash-free. Caps: `ggenCap_<Struct>_<Field>_<elemType>` (maxlen
variants suffix `_<N>`) — struct names are package-unique, so no file scope or
hash is needed; dedup narrows from per-file to per-field (a few duplicate
consts, zero runtime cost). Oneofs: `ggenOneof_<fileScope>_<n>` where fileScope
= the output base with `_ggen` removed and `_test` KEPT (`emitScope`:
`p_ggen.go` → `p`, `p_ggen_test.go` → `p_test`, `extra_ggen_test.go` →
`extra_test`) — package mode always produces exactly that pair of files and
`resetOneofRegistry` restarts the counter per output file, so stripping
`_test` too collapsed both onto one scope and redeclared `ggenOneof_<p>_0`.
Type spellings sanitize via
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
   escape/long-span fall calls the tier func then `ggen.Detach` (opt #49 — clones
   only when the result aliased `data`, so the owned escape scratch isn't
   re-cloned; both scalar + SIMD, no per-tier variant); `renderRawJSON` emits
   `append(ref[:0], data[…]…)` (reused backing) instead of the alias; `renderAny`
   switches to `ggen.AnyCopy`/`AnyNumberCopy` (detach every nested string + object
   key via `Detach`, no double-copy); `unknownKey` clones the embedded-fallback key +
   detaches its string value via `Detach` and `strings.Clone`s the
   `UnknownKeyError` path segment. TRANSIENT scans stay
   aliasing (`cp=false`): the dispatch key (matched + discarded) and the
   parse-feeds for time/url/netip/big\*/`[]byte` (the conversion owns its
   output). Per-struct granularity ⇒ an embedded-fallback / nested-struct VALUE only
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
   errors wrap as `ggen.CustomError{Name, Value, Cause}` (or `PredicateError`
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

74. **Carried element allocations reused in slice/array decode.** A GENERATED
    struct element's `DecodeFrom` fully resets the value it is handed (that is
    what the end-of-decode zeroing pass above buys), so the element
    slot no longer has to be blanked before decoding into it — the carried
    element's own slices/maps stay reachable and get reused. Array slots skip
    the `dst[i] = T{}` blanking; the `[]T` pre-grow becomes
    `if len(dst) < cap(dst) { dst = dst[:len(dst)+1] } else { dst = append(dst,
    T{}) }`, so a within-cap grow hands back the carried element (`emitElemGrow`,
    both paths). The depth-1 `[]*T` slab is NOT reused: its non-empty arm emits
    `slabN = make([]E, 0, cap)` unconditionally and the pointer slice is only
    resliced, so a within-cap grow there hands back a zero element of a fresh
    allocation and the carried pointees are orphaned — one alloc per decode
    per `[]*T` field on a reused receiver (backlog perf candidate). GATED on
    `directStruct` (`ElemKind == KindStruct &&
    isGenerated`): an element decoded through the reflective `encoding/json`
    fallback or an `UnmarshalJSON`/`UnmarshalText` rung MERGES into a non-zero
    value, which would resurrect stale data, and a cross-package generated type
    (not in the current pass) stays on the blanking shape too. Primitive
    elements are fully overwritten by their scan and were left alone.
    A multi-level POINTER element (`[]**T`) reuses too: the slot is resliced
    within cap so the carried chain survives, and its cascade drops
    `TargetNil` so it decodes THROUGH that chain instead of building a new one
    (`elemPtrReusable`). Note a multi-level element reports `KindStruct` with
    the remaining pointer levels still spelled in `ElemType`, so the gate
    resolves the LEAF before asking whether it self-resets — testing
    `ElemType` directly asks `isGenerated("*int")` and always answers no.
    ARRAY slots keep the assignment-only cascade: their contract is "every
    slot overwritten". Pinned by `TestMerge_sliceElementAllocationsReused`,
    `TestMerge_slicePointerChainReused` + `TestMerge_ArraySlotsOverwrite`.

75. **The catch-all map is `json:",embed"`.** Go 1.27's stable
    `encoding/json/v2` spells the tag option `embed`, not `inline`, so ggen
    follows it: an embedded fallback (jsonv2's term for a `map[~string]T` whose
    entries stand in for every object member the parent does not declare) is
    now written `json:",embed"`. The older `inline` spelling is a generate-time
    ERROR carrying the new one — jsonv2 lets unknown option words pass, which
    here would silently demote the catch-all to an ordinary field named after
    the Go field, exactly the silent-miscompile class the no-silent-no-op
    convention exists to prevent. Matching jsonv2, `embed` also rejects a JSON
    name (`json:"extra,embed"`) and any companion option, since its entries
    splice into the parent rather than sitting under a key. This closes the
    `,inline`/`,embed` row of the 1.27 parity gaps: `TestStdCompat_EmbedStruct`
    now agrees with jsonv2 byte-for-byte, where before jsonv2 nested the map
    under `"Extra"` while ggen flattened it. The `AppendAny` reflect walker
    keys its splice on the same `embed` option (`fieldInfo.embed`) and emits
    the entries AFTER every named member wherever the field sits in the
    declaration — the order generated `AppendJSON` already produced, and
    jsonv2's; there `,inline` is an ordinary unknown option and the map stays
    under its own key, as jsonv2 treats it.

76. **Map decode reads the carried map, fills a new one.** A `map[string]V`
    whose V owns allocations hoists `_mold := ref` and builds a
    fresh map, so each entry decodes into its PREVIOUS value — the allocations
    come back, the data does not (the value decoder resets what it is handed,
    opt #74). Keys the payload omits are never carried across, which is what
    makes this work without a seen-set: there is nothing to reconcile, because
    the new map only ever holds what the payload named.
    Three things the shape depends on:
    - **Seed from `_mold`, never from the map being filled** — otherwise a
      repeated key in one payload would merge into its own earlier occurrence
      instead of decoding fresh (`TestMerge_mapRepeatedKeyDecodesFresh`).
    - **`_reuse := len(_mold) != 0` is hoisted out of the entry loop.** A
      per-entry lookup against a nil map is a real `mapaccess` CALL, and
      without the hoist the common zero-value decode paid it on every entry:
      measured **+6.5…+9.6%** on fresh decode before the hoist, −1.1% after.
    - **Each nesting level names its own `mk`/`mv`/`carried`/`reuse`**
      (`mapNest` + `mapLocals`). A map VALUE that is itself a map re-enters
      through `renderField`, and with shared names the inner level shadowed the
      outer's, so `mapTarget` resolved to `mv[mk]` on the inner value —
      `map[string]map[string]V` never compiled. The outermost level keeps the
      bare names, so single-level output is unchanged.
    - **An omitted key still has to empty the map.** The swap is what empties a
      reusing map, and it only runs when the key is PRESENT, so
      `emitOmittedZero` clears it when the seen flag says the payload never
      named it. Without that, a carried map survived a payload that did not
      mention it.
    - **The skip must track where the swap is actually emitted.** The
      `clear()` is suppressed in `emitReceiverReset` while `renderMap` emits
      the swap, so anything decoding a map OUTSIDE `renderMap` still needs its
      clear: the stream emitter (`bytesPath` param —
      `TestMerge_mapStreamDropsOmittedKeys`) and the `json:",embed"` catch-all,
      which `unknownKey` fills (`TestEmbed_carriedMapDropsStaleKeys`, and why
      `reusesMapValues` excludes `f.Embed`). A pointer-to-map DOES swap, so its
      deref clear is skipped too (`TestPtrMap_valueReuseDropsOmittedKeys`) —
      otherwise it cleared the map the swap was about to read and allocated a
      replacement for nothing.
    What `reusesMapValues` accepts, i.e. what owns something worth recycling:
    a generated struct value that `ownsAllocations`; a slice or map value
    (seeded `_mold[mk][:0]` / `clear` — the map seed drops the `clear` when
    the inner level swaps too, since the swap reads the seed's live entries);
    a `[]byte` value (seeded `[:0]` — its
    `AppendDecode` would otherwise land after the carried bytes); a
    `json.RawMessage` value ONLY under `-copy`, since a raw span otherwise
    ALIASES the input and owns nothing; and a POINTER value at any depth,
    which recycles the pointee — that one decodes through an addressable local
    seeded from `_mold` and assigns the map index afterwards, because the
    `TargetNil` cascade the map slot otherwise uses cannot read a carried
    chain out of an unaddressable index.
    Value types reaching `encoding/json` or an `UnmarshalJSON` rung are
    excluded — unmarshalling into a non-zero value MERGES — and the pointer
    arm requires `elemPtrReusable(f)` (the `leafResets` gate), so a
    non-generated pointee takes the fresh-chain `TargetNil` path. Scalar-valued
    maps (`map[string]string`) are excluded too: they own nothing to recycle,
    so the swap would be pure cost. A pointer-to-map FIELD (`*map[string][]T`)
    on the bytes path is handed to the swap INTACT by the pointer seed, so its
    values are reused too (pinned by the `unsafe.SliceData` check in
    `TestMerge_pointerContainerLeavesReset`); the stream path clears at the
    seed. Numbers in bench/CLAUDE.md.

77. **Round-10 loader fixes (parse layer).** Beyond the Invocation-section
    items (both `go/packages` variants kept, the single post-order
    `walkPackages` over every target, `prefixBare` error reporting,
    `writeGenerated`) and the rejections documented under Per-struct
    annotations / Top-level type aliases (unknown tokens, generic types, `=`
    aliases):
    - **Same-package structs with their own codec are not field-synthesized.**
      `reachable()`'s expand step skips an UNANNOTATED same-package reference
      when `structSet.ownsCodec` reports a codec method declared outside ggen
      output (`DecodeFrom`/`DecodeFromStream`/`AppendJSON`/`JSONSize`,
      `MarshalJSON`/`UnmarshalJSON`, `MarshalText`/`UnmarshalText`/
      `AppendText`); the field then takes the same ladder as a foreign type
      (`FieldInterfaces` already populated), so `{"t":"T:x"}` matches jsonv2
      and a hand-written `DecodeFrom` is called rather than redeclared.
      Annotating the struct overrides this (explicit intent). The BFS used to
      queue every referenced struct with no method-set probe — behaviour
      flipped at the package boundary and the synthesized output
      ignored/duplicated the type's methods.
    - **Generated output is never evidence.** `structSet.inspect` masks
      ggen/JSON methods (incl. the `marshal`/`unmarshal` hook halves) that
      live in a previous run's `_ggen.go` for types of the package being
      generated; whether a same-package reference routes to a direct call is
      decided by `passTypes()` (what THIS pass generates), which also seeds
      `generatedTypes` in single-file mode (was: every annotated name).
      Trusting on-disk output made codegen a function of whether ggen's
      previous output existed — alias-rung flips, and a type that stopped
      being generated kept direct calls to methods about to be deleted for
      one run.
    - **Embedded-only dependencies are generated.** `structRefs` walks a
      struct's fields and descends recursively into embedded same-package
      structs (cycle-guarded), so a struct referenced only from inside an
      embedded struct's fields, at any depth, enters `generatedTypes`. The
      old BFS skipped `len(f.Names)==0` fields; `extractEmbedded` still
      promoted the field, which then decoded through the reflective
      `json.Unmarshal` fallback — pipe rules, required, unknown-key and
      duplicate-key checks silently dropped.
    Pinned by `TestParseLoad/*` (cli) + `TestSamePkgCodec_MethodsHonoured`,
    `TestAlias_StructIntrospect_CustomSteps` (integ).

78. **Round-10 field/tag rejections (parse layer).** Beyond the `json:`,
    `pipe:`, `hint:` and applicability items above (verbatim names, padded /
    `case:` / mutant options, one plain-map `embed`, nested presence words,
    `inner:` on byte kinds, width-aware bounds, `oneof` dedupe, the prealloc
    ceiling, pointer / named-primitive converter inputs):
    - **Shapeless field types are rejected at extraction.**
      `unsupportedTypeExpr` (AST path, `extractField`) and
      `unsupportedFieldType` (go/types path, `extractFieldFromTypes` — struct
      alias fields) walk pointers, slice/array elements and map key/value and
      refuse an anonymous struct, a func or a chan (a named type stops the
      walk). Each is a demonstrated defect, not a taste call: an anonymous
      struct literal emitted `result.An = nil` against a struct type and did
      not compile (`exprToString` had no arm for the literal and fell to
      fmt's `%T`, writing `*ast.StructType` into the generated file), while a
      func or chan field compiles and then makes AppendJSON fail for EVERY
      value (`json: unsupported type: chan int`) — a codec that can never
      succeed. The diagnostic is a `richError` carrying position + the remedy
      that actually works for that shape: `json:"-"` or unexporting for a
      func/chan (a NAMED func type is not a way out — see the backlog),
      declaring a named struct type for an anonymous struct. INTERFACES ARE
      NOT REJECTED, with methods or without: such a field is spelled by
      go/types, routes through the `encoding/json` rung, and marshals its
      dynamic value / refuses a non-null payload exactly as `encoding/json`
      does — and a NAMED interface (`io.Reader`) was never refused, so
      rejecting only the literal was arbitrary. Both paths judge the type
      AFTER the `json:"-"` gate, so an ignored field of any shape is exempt
      (the go/types site ran first, rejecting an ignored func field reached
      through a `type Local pkg.T` alias that the AST path let through, and
      double-prefixed its message as "field Fn: field Fn:").
    - **A promoted Go-name clash is a rejection.** After the JSON-name
      dominance pass, any two surviving fields sharing a Go name
      (promoted/promoted or own/promoted, any depth, distinct JSON names)
      error naming both ("fields E1.A (json "a1") and E2.A (json "a2") share
      Go name A — ggen addresses a promoted field by name and cannot keep
      both; rename one or drop the embedding"). stdlib keeps both (it
      addresses fields by index path) — documented divergence; full parity
      needs an embedding path on `FieldInfo` (backlog). The old Go-name
      grouping silently dropped a promoted tie and a promoted field shadowed
      by an own field with a DIFFERENT json name.
    Pinned by `TestCLI/InvalidRuleApplication/*`, `TestCLI/FieldCollisions/*`,
    `TestParseFile_round10/*`, `TestCheckOneValRule_ValueShape`,
    `TestParsePipeTagErrors` (cli) + `TestR10WideBounds`,
    `TestVariants_R10ConverterInputs` (integ).

79. **Round-10 marshal + CLI output fixes.** All detailed in place: the
    identifier-boundary import scan and the `<name>_` emitter-collision alias
    (Cross-package types), the `_test`-keeping oneof scope (oneof section),
    `omitEmptyCond` + `ggen.AnyIsEmpty` and the `[N]byte` omit guards (#35),
    `[0]T` / `[][0]T` (Supported Go kinds), strict RFC 3339 via
    `ggen.AppendRFC3339`/`ParseRFC3339` (`time.Time` kind), `writeGenerated`
    and the single topological `walkPackages` (Invocation). One item not
    covered elsewhere: `capFor` returns the width-default cap WITHOUT
    registering a maxlen const when `maxlen > spanBudgetMax` (512, the 64-bit
    span budget) — `fits*N + (1-fits)*base` is a typed-int constant expression
    whose `N*sizeof` overflowed at compile time for
    `maxlen=9223372036854775807`; `sizeof >= 1` makes `fits = 0` for every
    such N anyway, so nothing observable changes except that the file compiles
    (pinned by `PreallocWidths.MaxHuge` in `TestPrealloc_WidthDrivenCaps`).

80. **Round-10 decode fixes.** All detailed in place: `ggen.Float32` at every
    float32 site and `renderQuotedNumber` for `,string` (Supported Go kinds,
    #48), the inline zero before every non-ggen fallback rung + the `leafResets`
    gate (Decode-into-receiver pointer bullet, #76), `[N][]byte` via
    `sliceElemField`'s ArrayLen reset (`[N]T` kind), `""` ↔ zero for
    `net.IP`/`netip.*` and `null` → nil `net.IP` (net kinds, `null`
    kind-gating), the `UnmarshalJSON` rung's `NewParseErrShift` (#14),
    `validate` threaded through the `Any*` families (#50), and
    `splitCustomSteps` for pointer-field step order (`pipe:`). Pinned by
    `TestNarrowFloat_RoundsDecimalOnce`, `TestMerge_crossPkgFallbackDecodesFresh`,
    `TestByteArray_TupleOfByteSlices`, `TestNetTypes_emptyIsZero`,
    `TestCrossPkg_unmarshalJSONRungRebasesPos`, `TestAllowInvalidUTF8_anyValues`,
    `TestMods_pointerPipeDeclaredOrder`, `TestStringTag_quotedTextTakesNumberGrammar`
    (integ).

81. **Round-10 stream + receiver fixes.** All detailed in place: the pointer
    seed empties slice/map leaves and `emitArraySlotBlank` blanks merging
    array slots (Decode-into-receiver), `LenError{Got: N+1, AtLeast: true}` on tuple overflow
    (`[N]T` kind), `headSentinel` (#69), `renderStreamNetipParse`'s detached
    error re-parse (net kinds), the `SQLNullSpec` `uint8` spelling + stream
    `narrowIntGuard` (sql.Null kind), `UnknownKeyError` stamped after
    `ConsumeColon` + no-colon → `ErrBadObject` (#60), and `ErrBadLiteral` for
    n-garbage at every bytes null peek (`null` kind-gating).

82. **Bool error position: `ggen.BoolEnd` on the error branch only.**
    `ParseError.Pos` for a bad `true`/`false` must be the give-up byte
    (jsonv2's offset, and what the stream path stamps via `s.Offset()` —
    `Stream.Bool` leaves `Pos` on the mismatching byte, or the window end when
    the reader drained mid-literal), but `ggen.Bool` must stay an inlinable
    PROBE returning the literal start: it costs 54 of the 80 budget and is
    inlined at every generated bool site today, while every variant that
    carries the give-up walk — `litEnd` folded in (117), a call to a cold
    helper (114), unsafe loads (112), a true-only shell + slow call (95–96), a
    4-byte switch (108) — de-inlines it (`go build -gcflags=-m=2`, 2026-09).
    So the walk lives in the cold exported `BoolEnd(data, i)` (`litEnd` over
    `"true"`/`"false"`, chosen by `data[i]`) and the four bytes-path Bool emit
    sites (`renderMap`, `renderSQLNull`, `renderField`, the array/slice
    element site) plus the alias decoder emit `if err != nil { i =
    ggen.BoolEnd(data, i); return result, i, ggen.NewParseErr(field, i, err) }`
    (array/alias sites as a separate `if err != nil { i = ggen.BoolEnd(data, i)
    }` before the shared error check). `skipValue`, the SIMD skip tiers and
    the four `Any*` arms call it on their error branch too. No stream-side
    codegen change. Pinned by `TestBoolEnd_GiveUpPosition` (root) +
    `TestParseError_BoolGiveUpPos` over `R10BoolShapes` (integ: a bool at
    every emit site, bytes and stream at chunk 1/3/64).

83. **A pointer field's `pipe:` is ONE ordered pass.** Detailed in place under
    `pipe:`. The pointer arm used to emit the two halves at two different
    points — built-ins rode along with the leaf's decode, `@Func` steps ran on
    the pointer after the if/else — so `pipe:"@Add1 gte=2"` on a `*int`
    compared before it added and rejected 1, while the identical value-typed
    field accepted it. The docs' "value steps run in declared order" now needs
    no pointer exception. Two consequences of emitting after the cascade: a
    widened leaf (`*int8`) runs its built-ins AFTER the narrowing check, which
    is what the value twin does; and `splitCustomSteps` reads `fieldPipe(f)`,
    so a synthetic pointer-element field with only legacy buckets takes the
    same path (output-identical, verified by regen). Pinned by
    `TestMods_pointerCustomStepDeclaredOrder` (against the value twin
    `R10BValPipeOrder` and a multierr carrier) +
    `TestVariants_pointerFieldValueSteps`.

84. **`*[N]byte` at any pointer depth.** Detailed in place (Supported Go
    kinds). Six emit sites re-derived a pointee's kind from the type STRING,
    where the `[N]byte` → base64 fold is invisible, so the shape was accepted
    and emitted a tuple of strings assigned into bytes — it never compiled.
    `leafKind(f, leafType)` keeps KindBytes for a byte-array pointee at all
    of them, and `formatElemKind` peels pointer levels so the `format:` set
    applies as it does to `[N]byte` (it used to refuse with "`format:array`
    is not applicable to *[3]byte", which only made sense while the shape
    itself was unusable). Supporting it beats a new rejection to document:
    jsonv2 marshals `*[N]byte` as a base64 string or null and `*[]byte`
    already worked. Round-trips at any depth, strict decoded length, `null` ↔
    nil, omitempty/omitzero, `-copy`, carried-pointee reuse. Pinned by
    `TestByteArray_Pointer` + `TestJSONSize_PtrByteArray_NoRealloc`.

85. **`[0]T` in every container position.** Detailed in place (Supported Go
    kinds). `renderAppendMap` always wrote `for k, v := range ref` while a
    `[0]T` value's emit is the constant `[]` and names no `v`; the existing
    slice-side guard could not be reused because it read `f.ElemArrayLen`,
    which parse populates for slice/array elements only — hence the shared
    type-reading `constEmptyElem`. With marshal fixed the compiler reached
    the DECODE of `map[string][0][3]int` and hit a gc internal compiler error
    ("can SSA LHS mv[idx0] but not RHS") on the element loop's dead store
    into a zero-length array, so both tuple readers now emit a no-slot read
    for `ArrayLen == 0` — which also shrinks the existing `R10ZeroTuple` /
    `R10OmitEmpty` generated bodies by ~70 lines each. Pinned by
    `TestTuple_ZeroLengthContainers` +
    `TestJSONSize_ZeroTupleContainers_NoRealloc` + the `zeroarr` rows of
    `TestGeneratedCompiles`.

86. **Round-10 follow-up parse-layer fixes.** All detailed in place:
    `embedKindError` (`json:",embed"`), `maxPrealloc = MaxInt32` (`hint:`),
    the single diagnostic for element rules on a non-diveable field and the
    kind-spelled numeric bound literals (Rule applicability), the shapeless
    field-type rule (#78), and the external test package's first-run fixed
    point (Invocation). One more not covered elsewhere: a type expression
    written with the redundant parentheses Go and gofmt keep — `[]([]bool)`,
    `*(int)`, `map[string](*int)`, `([]byte)` — used to emit
    `[]*ast.ParenExpr` / `new(new(v))` or fall silently to the
    `encoding/json` rung, because ~10 AST switches destructure type
    expressions and none looked through `*ast.ParenExpr`. `walkStructDecls`
    now normalises once at the choke point every declaration passes through
    (`ts.Type = unparenType(ts.Type)`), a recursive strip that REWIRES
    children in place so the `typesInfo.Types` / `Uses` lookups keyed on those
    nodes stay valid; `exprToStringQ` gained a `ParenExpr` arm so the
    spelling function is total over type expressions. Pinned by
    `TestParseLoad/*`, `TestParseFile_round10/shapeless_field_types_rejected`,
    `TestCheckRuleApplicability_NonDiveableReportsOnce`,
    `TestCheckOneValRule_ValueShape`, `TestParseHintTag_Ceiling` and
    `TestNumericBoundLiteralsCarryFieldKind` (cli) +
    `TestR10BParenthesizedTypes`, `TestBigUint64Bounds_reportedExactly`
    (integ).

87. **`omitempty` on a struct field is refused at generate time.** ggen writes
    a struct as its object whatever its members do, so the option can only be
    a no-op there — a silent one, which the no-silent-no-op convention
    forbids. `checkRuleApplicability` (`applicability.go`, beside the
    `,string` and `format:` rejections, so it batches with them and honours
    `-dry`) rejects with ``T.Inner: `omitempty` is not applicable to a struct
    field (got Nested)`` and a Note pointing at `omitzero` or a pointer.
    Scoping needs go/types: a named primitive, a named container and a
    foreign array type (`uuid.UUID`) all read as `KindStruct` on the AST
    path, so the gate is a new `FieldInfo.UnderlyingStruct`
    (`t.Underlying().(*types.Struct)`, pointers NOT peeled — set at both
    extraction paths) ANDed with `effectiveKind(...) == KindStruct`, which
    keeps `time.Time`/`url.URL`/`sql.Null*`/`big.*`/`netip.*` (struct
    underlying, dedicated kind) out. The flag is false in AST-only (degraded)
    mode, so the check defers there exactly as the kinded rule matrix does.
    A field promoted from an embedded struct is judged where it is DECLARED,
    so the diagnostic names that struct. Consequences: `omitEmptyCond` has no
    struct arm, so `*T` under `omitempty` is a bare `!= nil` — a non-nil
    pointer to an all-omitted struct emits `{}`; and `AppendAny`, which
    cannot reject at runtime, skips the unwrite for a struct value
    (`structWire`, peeling pointers/interfaces so a nil one still omits).
    That is a deliberate divergence from jsonv2, which drops a struct
    encoding `{}`. Pinned by `TestOmitEmptyOnStructField` +
    `TestCheckRuleApplicability` rows (cli), `TestOmitEmpty_JSONEmptyKinds`
    (integ) and `TestAppendAny_OmitEmptyKeepsStruct` (root).
88. **`any` values are sized at runtime.** `sizeContribKind`'s `KindAny` arm
    emits `size += ggen.AnySize(ref)` (`AnySizeHTML` under `htmlescape`, via
    `anySizeFn`, the twin of `appendAnyFn`), and `constSizePerEntry` has no
    `KindAny` arm, so `map[string]any` values — the `,embed` catch-all
    included — take the per-entry `sizeContrib` loop. The flat budgets it
    replaces were guesses in both directions: 256 per field or element
    over-reserved ~250 B for a scalar and under-reserved 15× for a flat
    200-string slice (4009 written, 265 reserved); map values got 64. A
    budget that undershoots breaks #9 silently, since `append` just grows.
    `AnySize` mirrors `appendAny`'s dispatch and depth cap: typed leaves take
    the generator's own constants (int 20, float 25, bool 5, time 37,
    duration 27, strings `len×mult+2`), so a value costs the same inside an
    `any` as in a typed field; dynamic shapes are walked; a generated value
    reports its own `JSONSize`; a text or JSON marshaler is run to learn its
    length (it runs again in `AppendJSON` — the one place sizing allocates).
    Cost: `Marshal` walks each `any` twice, sizing then appending. Pinned by
    `TestAnySize_BoundsAppendAny` (root, every dispatch arm, both escape
    modes) and `TestJSONSize_AnyPositions_NoRealloc` (integ: field,
    omitempty field, slice element, map value, catch-all, htmlescape).
