---
name: ggen
description: Drive ggen CLI. Generate zero-copy, zero-reflection JSON encode/decode for annotated Go structs. Use when user want faster JSON codec than `encoding/json`, validation baked into decode, compile-time-checked custom validators/transforms. Cover when invoke ggen, flag→intent map, annotation surface, regen-after-edit workflow.
---

# ggen — JSON codegen for Go

ggen parse annotated Go structs. Emit `DecodeFrom`, `DecodeFromStream`, `JSONSize`, `AppendJSON`. Generated code = hand-rolled byte scan. No reflection, no token layer. Bytes path (`DecodeFrom` over caller `[]byte`) strings alias input via `unsafe.String` — zero-copy. Stream path (`DecodeFromStream` over `*scan.Stream`) copy strings out of intermediate buffer so buffer compact safely. See _Stream is not zero-copy_ below.

Module: `github.com/sirkostya009/ggen`. Binary: `ggen`. Go ≥ 1.26.

## When to reach for ggen

Use ggen when ANY hold:

- Hot-path JSON decode/encode where `encoding/json` (v1 or v2) show in CPU/alloc profiles.
- Validation belong at decode time (length, range, regex-light patterns, custom `func(T) error`) — ggen fold into parser. Invalid payloads short-circuit before allocating full value.
- Long/slow streams + validation required AND invalid payloads frequent enough that fail-fast mid-body (vs finish read first) save real bandwidth/CPU.

Skip ggen when:

- Wire shape need `encoding/json` v1 quirks ggen diverge from (URL struct-dump, `sql.NullX` `{Valid:…}` wrapper).

## Install

```sh
go install github.com/sirkostya009/ggen/cli@latest # CLI binary
go get github.com/sirkostya009/ggen                 # runtime subpackages
```

## Invocation

```sh
ggen .                 # current package
ggen ./...             # every package matched by the pattern — module-scoped, same as `go build ./...`
ggen ./pkg/...         # subtree pattern (relative paths must start with `./`)
ggen <dir>             # one package
ggen <file.go>         # one file
ggen <file.go> Foo Bar # one file, only structs named Foo or Bar, will fully overwrite existing <file_ggen.go> file
```

Test-only packages (no non-`_test.go` files) skipped in pattern mode. Invoke `ggen <dir>` directly when target only has `_test.go` sources.

**Run ggen under same `GOEXPERIMENT` as user build** — `packages.Load` honor build tags. Files behind `goexperiment.jsonv2` invisible without it. Repos using jsonv2:

```sh
GOEXPERIMENT=jsonv2 ggen ./...
```

## Agent-mode output (do not truncate)

ggen auto-detect when driven by coding agent. Switch logger from pretty multi-line/ANSI-colored (humans) to concise one-line-per-record. Concise mode also fires under CI or non-TTY stderr.

Any non-empty value of these env vars enable:

- `AI_AGENT` (generic cross-vendor)
- `CLAUDECODE` (Anthropic's Claude Code)
- `CURSOR_TRACE_ID` (Cursor IDE)
- `AIDER_AUTO_COMMITS` (Aider)
- `CI` / `GITHUB_ACTIONS` / `GITLAB_CI` / `CIRCLECI` / `JENKINS_HOME` /
  `BUILDKITE` / `TRAVIS` / `APPVEYOR` / `TF_BUILD` /
  `TEAMCITY_VERSION` / `CONTINUOUS_INTEGRATION`
- non-TTY stderr (piped/redirected)

Each line self-contained, pattern: `<level>: [file:line:col:] <msg> [(hint)]`. Levels: `inf:` / `dbg:` / `trc:` / `err:`. Every line signal — **do not truncate** (`head` / `tail` / `grep -v`).

## Output file naming

- Package mode: `<dir-basename>_ggen.go` (and `_ggen_test.go` if annotated struct in `_test.go`).
- Single-file mode: `<basename>_ggen.go`.
- Source with `//go:build foo`: land in `<dir>_foo_ggen.go`, constraint preserved. Multi-term constraints get slugified filename (`//go:build foo && bar` → `<dir>_foo_bar_ggen.go`).
- `-o <path>` override path in single-file or single-package mode.

## Flags (global) and per-struct annotations (local)

Most flags have matching annotation token (no leading dash). Annotations space-separated after `//ggen:generate`.

| CLI flag         | annotation      | effect                                                                                                                                                                                                                                                                                   |
| ---------------- | --------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `-o <path>`      | —               | override output path (single-file / single-package only)                                                                                                                                                                                                                                 |
| `-pkg <name>`    | —               | override the package name in the generated file                                                                                                                                                                                                                                          |
| `-marshal`       | `marshal`       | also emit `MarshalJSON` so the type satisfies `encoding/json.Marshaler`                                                                                                                                                                                                                  |
| `-unmarshal`     | `unmarshal`     | also emit `UnmarshalJSON` for `encoding/json.Unmarshaler`                                                                                                                                                                                                                                |
| `-multierr`      | `multierr`      | accumulate every validation failure into `validation.Errors` (slice) instead of returning on the first                                                                                                                                                                                   |
| `-allowdups`     | `allowdups`     | accept duplicate JSON keys, first-wins (default: error on second occurrence)                                                                                                                                                                                                             |
| `-novalidate`    | `novalidate`    | drop validation, required-field checks, and mods                                                                                                                                                                                                                                         |
| `-ignoreunknown` | `ignoreunknown` | silently drop unknown JSON keys (default: error). Overridden when an inline catch-all map is present                                                                                                                                                                                     |
| `-nullzero`      | `nullzero`      | accept explicit JSON `null` on every non-pointer value field, decoding it to the Go zero (default: error). A per-field `nullzero` decode variant in `pipe:` opts in one field                                                                                                            |
| `-nosortkeys`    | `nosortkeys`    | emit struct fields in declaration order (default: alphabetical, compresses better)                                                                                                                                                                                                       |
| `-usenumber`     | `usenumber`     | decode JSON numbers in `any` fields as `json.Number` instead of `float64`                                                                                                                                                                                                                |
| `-htmlescape`    | `htmlescape`    | escape `<`, `>`, `&` to `\uXXXX` (default: literal, matches `encoding/json` v2)                                                                                                                                                                                                          |
| `-allowinvalidutf8` | `allowinvalidutf8` | skip decode UTF-8 validation: invalid bytes pass raw into strings/keys/raw spans, unpaired surrogates → U+FFFD (default: reject, jsonv2 parity). Grammar checks stay |
| `-copy`          | `copy`          | bytes-path `DecodeFrom` copies strings / `json.RawMessage` / any-embedded strings out of the input instead of aliasing it, so input is safe to store long term after decode                                                                                                              |
| `-dry`           | —               | parse + validate every annotated struct, surface all errors, emit no file. Rejects `-o`/`-pkg`                                                                                                                                                                                           |
| `-simd <tier>`   | —               | SIMD tier for string scans + marshal escape scans: `off`/`avx`/`avx2`/`avx512`. `GOEXPERIMENT=simd` at ggen invocation auto-selects `avx`; wider tiers are explicit opt-ins. Tier is baked in at generate time; output requires `GOEXPERIMENT=simd` to build + a matching CPU |
| `-v`             | —               | info-level progress (e.g. `wrote <file>`)                                                                                                                                                                                                                                                |
| `-vv`            | —               | debug-level: per-package / per-struct diagnostics                                                                                                                                                                                                                                        |
| `-vvv`           | —               | trace-level diagnostics                                                                                                                                                                                                                                                                  |

CLI flags apply to all structs in pass. Annotations apply to struct they on.

Verbosity flags `-v`, `-vv`, `-vvv` for troubleshooting only. CLI always report descriptive errors.

## Struct annotation

Trigger: `//ggen:generate` (no space between `//` and `ggen`, mirror `//go:generate`). Goes on struct or top-level type alias.

```go
//ggen:generate
type User struct {
	ID    int      `json:"id"`
	Name  string   `json:"name"   pipe:"required minlen=1 maxlen=64"`
	Email string   `json:"email"  pipe:"trim lower"`
	Tags  []string `json:"tags,omitempty" pipe:"inner:notempty"`
}

//ggen:generate marshal unmarshal multierr
type Order struct { /* ... */ }
```

## Field tags

### `json:"..."` — same as stdlib, plus

- `json:",inline"` — field = catch-all map for unknown keys. Type must be a string-keyed map (`map[string]V`); V may be `any`, a primitive, a ggen-annotated struct, or any other type (typed elems dispatch through the elem's fast path or `encoding/json.Unmarshal` over the captured span). Overrides `-ignoreunknown`.
- `json:"name,omitempty"` — skip on marshal when JSON-empty.
- `json:"name,omitzero"` — skip on marshal when Go-zero.
- `json:"name,string"` — wrap primitive as JSON string (unwrap on decode).
- `json:"name,format:X"` — format hint for native types (see kinds below). MUST be last option in tag (jsonv2 rule).

Only exported fields read/written, same as `encoding/json`.

### `pipe:"..."` — decode, transform, validate

One ordered, whitespace-separated pipeline: presence, an optional decode stage,
then value steps (mods + validators) that run **in declared order**. Values
with spaces are single-quoted. `|` is an intra-rule arg separator.

```go
Name  string	`json:"name"  pipe:"required trim minlen=1 maxlen=50"`
Email string	`json:"email" pipe:"trim lower contains=@"`
Aliases map[string][]string	`json:"aliases" pipe:"keys:(minrunes=2 maxrunes=32) inner:maxlen=10 inner:notempty"`
```

**Presence:** `required` (key must appear → `RequiredError`) / `optional`
(default, explicitly states "may be absent"). Position-independent. Absent → Go zero.

**Decode stage (`/` variants):** by default a field decodes from its type's
natural JSON shape. List `/`-separated variants to accept more (one per shape):

- `.` — native decode of the field type.
- `nullzero` — accept JSON `null` → Go zero. Allows non-pointer values to
  accept `null` values as Go-zero.
- `@Conv` — converter `func(W) T` / `(T,error)` / `(T,bool)`; ggen scans input
  `W` (primitive or ggen struct), then calls it. Needs a `/`, leading `.`, or
  `~` to read as a converter. Encode is unaffected (marshals as native T).

```go
Age   int `json:"age"   pipe:". / @AtoiStrict gte=0 lte=150"` // AtoiStrict(string)(int,error)
Price int `json:"price" pipe:". / @FromMoney"`                // FromMoney(Money) int
```

**Value steps** run in declared order. Validators:

| step                                                          | applies to    | checks                      |
| ------------------------------------------------------------- | ------------- | --------------------------- |
| `notempty`                                                    | str/container | non-empty / non-zero length |
| `len=N`, `minlen=N`, `maxlen=N`                               | str/container | byte length / element count |
| `runes=N`, `minrunes=N`, `maxrunes=N`                         | string        | utf8 rune count             |
| `gt=N`, `gte=N`, `lt=N`, `lte=N`                              | numeric       | comparison                  |
| `eq=X`, `neq=X`                                               | str/numeric   | equality                    |
| `multiple=N`                                                  | integer       | `% N == 0`                  |
| `oneof=a\|b\|c`                                               | str/numeric   | one of the alternatives     |
| `url`, `alphanum`, `numeric`, `lower`, `upper`, `hexadecimal` | string        | character-class predicate   |
| `starts=X`, `ends=X`, `contains=X`                            | string        | substring test              |

Mods (transforms):

| step                        | applies to | effect                                                                      |
| --------------------------- | ---------- | --------------------------------------------------------------------------- |
| `trim`, `lower`, `upper`    | string     | whitespace / case                                                           |
| `trimleft=X`, `trimright=X` | string     | strip prefix / suffix                                                       |
| `replace=old\|new`          | string     | substring replace                                                           |
| `clamp=lo\|hi`              | numeric    | bound into `[lo,hi]` (either side may be empty: `clamp=0\|`, `clamp=\|100`) |

Container levels: `inner:` scopes to one level down, `keys:` to map keys. A
bare prefix takes one step (`inner:trim`); parenthesize several
(`inner:(trim maxlen=20)`); nest groups to go deeper
(`inner:(minlen=1 inner:(gte=0 lte=100))`). Steps outside any group apply to
the whole container.

**Custom funcs** (`@FuncName` / `@pkg.FuncName`, classified by signature):

| signature            | role                                                |
| -------------------- | --------------------------------------------------- |
| `func(T) error`      | validator → `CustomError{Value, Cause}`             |
| `func(T) bool`       | validator → `PredicateError` (message-capable)      |
| `func(T) T`          | mod (pure)                                          |
| `func(T) (T, error)` | mod (fallible; error → parse error)                 |
| `func(T) (T, bool)`  | mod (fallible; false → `ModError`; message-capable) |
| `func(W) T` (W ≠ T)  | converter (decode-stage variant only)               |

`func(bool) bool` is rejected. Bool forms take an inline message:
`@MustBeEven:'value must be even'`. Cross-package via `@pkg.Func` (resolves
through the source file's imports; blank imports work).

```go
//ggen:generate
type Box struct { N int `json:"n" pipe:"@EvenOnly"` }
func EvenOnly(n int) error { if n%2 != 0 { return errors.New("must be even") }; return nil }
```

Applicability is checked at parse time (string-only rules on non-strings,
numeric on non-numerics, `inner:` on non-containers, `keys:` on non-maps, etc.)
with a clear diagnostic; each value step is gated against the working type at
its level.

### `hint:"..."` — preallocation

`hint:"N"` → `make([]T, 0, N)` (slice/map only), overriding the default cap 4
and `minlen`-derived caps. Per-level: `hint:"32 inner:8"`. `hint:"0"` disables;
negative is a generate-time error.

#### Inspecting errors

```go
var e *validation.MinLenError
if errors.As(err, &e) {
	// e.Path, e.Limit, e.Got
	// e.Pos — failure byte offset, relative to the full payload
}
```

In `multierr` mode generated code return `validation.Errors` (`[]validation.Error`). Implement `Unwrap() []error`.

Parse failures (malformed JSON, wrong primitive type) wrap in `*decode.ParseError` carrying `Field` (dotted JSON path), `Pos` (byte offset), `Err` (underlying `scan.ErrX` sentinel). `errors.Is(err, scan.ErrBadString)` keeps working through the wrap. Validation errors NOT wrapped — typed pointers stay reachable. `ParseError.Error()` only renders the `parse error at <field> (pos <n>)` prefix; `errors.Unwrap` for the underlying message.

## Supported field kinds

- Primitives: `string`, `bool`, `int*`, `uint*`, `float*`, plus `*T` for any (`null` ↔ `nil`).
  Nested ptrs `**T`/`***T`/... also native: `null` → nil outer, otherwise value parse first and missing levels alloc'd.
- `[]T`, `map[string]V` (string keys only), `[N]T` (strict element count — mismatch → `validation.LenError`).
- `[]*T` / `[N]*T` of structs — single slab backing, ~log(N) allocs vs N. Multi-level elements (`[]**T`, `[N]**T`) and pointer map values (`map[string]*V`, `**V`, …) decode natively through the same null/alloc cascade as scalar pointer fields.
- Nested struct (same package: direct call; cross-package: see below).
- Embedded struct — fields promoted to parent JSON object.
- `any` / `interface{}` — full stdlib-compatible decode shape, plus `usenumber` for `json.Number` numbers.
- `[]byte` — `format:base64` (default), `base64url`, `base32`, `base32hex`, `base16`/`hex`, `array`. `null` ↔ `nil` (nil marshals as `null`, empty non-nil as `""`/`[]`).
- `time.Time` — `format:RFC3339Nano` (default), `RFC3339`, `unix`, `unixmilli`, `unixmicro`, `unixnano`, other `time.X` constants, or custom layout `format:'2006-01-02'`.
- `time.Duration` — `format:units` (default, `"1h30m"`), `sec`, `milli`, `micro`, `nano`.
- `net.IP`, `netip.Addr`, `netip.Prefix` — text form.
- `json.RawMessage` / `jsontext.Value` — opaque, zero-copy alias.
- `net/url.URL` — JSON string (NOT struct dump — wire divergence from stdlib).
- `math/big.Int` (JSON number), `big.Float` / `big.Rat` (JSON string — wire divergence from stdlib).
- `database/sql.Null*` and generic `sql.Null[T]` (Go 1.22, any inner `T` ggen handles as a field — primitive, `time.Time`, `uuid.UUID`, named types, …) — inner value or `null` (NOT `{Valid:…}` — wire divergence from stdlib).
- Any type implementing `encoding.TextAppender` / `TextMarshaler` / `TextUnmarshaler` — auto-picked (`google/uuid`, `gofrs/uuid/v5`, `shopspring/decimal`, `oklog/ulid`, `segmentio/ksuid`, `rs/xid`, `net/mail.Address`, custom enums, etc.).

### Cross-package types

For fields whose type live outside package being generated, ggen probe method set at codegen and emit first available:

| direction | ladder                                                                                |
| --------- | ------------------------------------------------------------------------------------- |
| decode    | `DecodeFrom` → `UnmarshalJSON` → `UnmarshalText` → `encoding/json.Unmarshal`          |
| encode    | `AppendJSON` → `MarshalJSON` → `AppendText` → `MarshalText` → `encoding/json.Marshal` |

### Type aliases

`//ggen:generate` on named top-level type works too. Strategy picked from underlying type shape and method set:

| flavor                           | example                 | strategy                                                                      |
| -------------------------------- | ----------------------- | ----------------------------------------------------------------------------- |
| primitive                        | `type Count int`        | scan + cast                                                                   |
| struct (exported fields)         | `type Comment Inner`    | field introspection — treats the alias like a regular struct                  |
| struct (has `DecodeFrom`)        | `type X HasGgenMethods` | cast + delegate                                                               |
| struct (opaque + Marshaler/Text) | `type Local time.Time`  | delegate to underlying's `MarshalJSON`/`MarshalText` (`AppendText` preferred) |
| container                        | `type Tags []string`    | same emitters as slice/map/array fields                                       |

Aliases of channels, interfaces, functions rejected at generate time.

## Generated method surface

```go
func (result T) DecodeFrom(data []byte) (T, int, error)
func (result T) DecodeFromStream(s *scan.Stream) (T, error)
func (s T) JSONSize() int
func (s T) AppendJSON(dst []byte) ([]byte, error)
```

With `marshal` / `unmarshal` annotations:

```go
func (s T)  MarshalJSON() ([]byte, error)
func (s *T) UnmarshalJSON(data []byte) error
```

### Stream is not zero-copy

`DecodeFromStream` take `*scan.Stream` wrapping `io.Reader` behind user-provided `[]byte` buffer. Buffer sit between reader and parser — chunks land there via `Read`, parser scan out of it, compaction recycle space mid-decode so buffer stays bounded to roughly `max(chunk_size, single_value_size)` across long streams.

Strings, `json.Number`, `json.RawMessage` values **copied** out of buffer, not aliased — buffer not grown unless must, aliases won't stick. Trade-off: ~2–3× more allocs than bytes path. Stream instead capable of recycling user-provided buf for extremely large payloads.

Bytes-path (`DecodeFrom`) still zero-copy via `unsafe.String` into caller `data` — see pitfalls below.

### Decode-into-receiver (merge)

Decoders parse values into method non-pointer receiver. Non-nil slices/maps reuse capacity, values overwritten — nested slices too: re-decoding a `[][]T` (any depth) reuses the inner rows' backing arrays, not just the outer slice. Non-nil pointer fields reuse the pointee (struct pointees merge omitted fields; `null` nils the field). Niche, useful for reusing capacity of slice/map/pointer fields when same object reused for multiple (not necessarily _different_) payloads.

NOT 100% compatible with stdlib — ggen diverges in three ways: ALL containers are reset, regardless of presence (blank payload → blank slate, capacity kept), a PRESENT map key replaces the whole map (clear+refill; stdlib merges entries into it), and an explicit `null` on a non-pointer scalar/native field ERRORS (stdlib zeroes it — only pointer/slice/map/`[]byte`/`sql.Null*`/raw fields accept `null`). Scalars-persist-on-omit, slice-replace, null→nil for slice/map/pointer, nested-struct merge, and `*T`/`**T` reuse all match stdlib.

```go
u, _, err := existing.DecodeFrom(payload)
```

Generic helper funcs in `decode` package no merge semantics.

Call from user code:

```go
import (
	"github.com/sirkostya009/ggen/decode"
	"github.com/sirkostya009/ggen/encode"
	"github.com/sirkostya009/ggen/scan"
)

// single value — call the generated method directly with a zero-value receiver
u, _, err := User{}.DecodeFrom(payload)
out, err := encode.Marshal(u)
// primitive aliases (`type UserID uint64`): use a typed zero
// id, _, err := UserID(0).DecodeFrom(payload)

// slices (loop helpers — saves caller from reimplementing the array walk)
users, err := decode.UnmarshalSlice[User](payload)
out, err = encode.MarshalSlice(users)
out, err = encode.AppendSlice(out[:0], users) // can use just AppendSlice to reuse buffers

// streaming single value — caller owns the scan.Stream
s := scan.NewStream(req.Body, nil)  // or pre-sized buf, e.g. make([]byte, 0, hint)
u, err = User{}.DecodeFromStream(s)
// s.Bytes() is now recyclable
// (use `var s scan.Stream; s.Reset(...)` to stack-allocate)

// streaming array
users, buf, err := decode.UnmarshalSliceStream[User](req.Body, buf[:0])
```

## Regen workflow

After editing any annotated struct (add/remove fields, change tags, add new `//ggen:generate` types, change CLI flags):

```sh
ggen ./...
```

Or wire into `go generate`. One directive per package enough:

```go
//go:generate ggen .
```

Per-file scope works too (use `$GOFILE` for source basename):

```go
//go:generate ggen $GOFILE
```

Build tag propagation: struct in file behind `//go:build foo` land in `<dir>_foo_ggen.go` with same constraint. Unconstrained builds not broken.

## Pitfalls

1. **Zero-copy aliasing.** Decoded strings (and `json.RawMessage` / `jsontext.Value`) alias source `[]byte`. Mutating input after `DecodeFrom` silently corrupt decoded values. Streaming path copy strings, safe to recycle buffer between calls.
2. **Long-lived references can balloon heap.** A short string field from a large payload stays referenced (cached, stored in a struct held forever) → Go's non-compacting GC keep the entire backing buffer alive. For long-lived data, copy field (`s := string([]byte(decoded.X))`) or use streaming.
3. **Wire-shape divergences from stdlib** for `net/url.URL` (string, not struct dump) and `sql.Null*` (inner-or-null, not `{Valid:…}` wrapper). Round-trip through ggen fine. Pipe through stdlib `encoding/json` reshape value.
4. **AST-only fallback when no `go.mod`.** When `packages.Load` cannot resolve types (e.g. temp file with no module context), ggen fall back to AST-only mode and emit `encoding/json` for cross-package types. Slower but correct.
5. **Build under right `GOEXPERIMENT`.** Files behind `goexperiment.jsonv2` invisible without `GOEXPERIMENT=jsonv2 ggen ./...`.
6. **Test files first-class inputs.** Annotated structs in `_test.go` files route to `_ggen_test.go` so methods don't bundle into library build.
7. **`hint:` only safe prealloc hint.** Don't expect `maxlen` to size container — it doesn't (intentional, retained-heap reasons). Use `hint:"N"` when know typical size.
8. **Decode reject invalid UTF-8; encode not validate.** Decode: malformed UTF-8 bytes or unpaired `\uXXXX` surrogates in string values/keys → `scan.ErrInvalidUTF8` (jsonv2 parity; v1 instead silently replace with U+FFFD). ASCII strings pay nothing. Captured raw spans (`json.RawMessage`/`jsontext.Value`) byte-validated too. Exceptions: skipped content (`ignoreunknown`) grammar-checked only; unpaired surrogate ESCAPE inside raw span pass (ASCII text there; jsonv2 reject). Opt out per struct: `allowinvalidutf8`. Encode: invalid bytes in struct strings emitted raw onto wire (v1 replace, v2 replace+error) — validate at boundary if populating structs from untrusted bytes.
9. **Container nesting capped at `scan.MaxDepth` (10000, jsonv2 parity).** Deeper nesting → `scan.ErrMaxDepth`, NOT a stack-overflow crash. Applies to every recursive path (nested containers, `any`, `ignoreunknown` skip, RawMessage, self-referential structs), bytes + stream. Untrusted deeply-nested input is safe.

## Common user intents → flags

| User says                                                 | Reach for                                                                                             |
| --------------------------------------------------------- | ----------------------------------------------------------------------------------------------------- |
| "still want `json.Marshal(u)` to work"                    | `-marshal` (and/or `-unmarshal`)                                                                      |
| "collect all errors, not just the first"                  | `-multierr`                                                                                           |
| "skip unknown keys silently"                              | `-ignoreunknown` or a `json:",inline"` catch-all map                                                  |
| "accept `null` on a scalar instead of erroring"           | a `nullzero` decode variant in `pipe:` per field, or `-nullzero` / `//ggen:generate nullzero` for all |
| "fastest possible decode, I trust the input"              | `-novalidate` (+ `-allowinvalidutf8` if input may carry non-UTF-8 strings)                            |
| "payload has broken UTF-8 / lone surrogates, decode anyway" | `-allowinvalidutf8` or `//ggen:generate allowinvalidutf8` per struct                                |
| "wire output embedded directly in HTML"                   | `-htmlescape` (or per-type via alias `//ggen:generate htmlescape`)                                    |
| "exact-precision numbers (big ints, no float64)"          | `-usenumber` for `any` fields; or use `math/big.Int`                                                  |
| "duplicate keys should be accepted (first wins)"          | `-allowdups`                                                                                          |
| "keep field order matching declaration"                   | `-nosortkeys`                                                                                         |
| "i want only some strings to have html escaping"          | `//ggen:generate htmlescape` `type HTMLString string`                                                 |
| "this struct has json tags but I want it to parse faster" | `//ggen:generate` `type Alias OtherStruct`                                                            |
| "validate annotations in CI without writing files"        | `-dry` (parse + validate every annotated struct, surface every error, emit no file)                   |
| string field is going to be stored in global state        | `-copy` to avoid keeping a reference to potentially huge payload on a bytes path                      |
