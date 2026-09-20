---
name: ggen
description: Drive ggen CLI. Generate zero-copy, zero-reflection JSON encode/decode for annotated Go structs. Use when user want faster JSON codec than `encoding/json`, validation baked into decode, compile-time-checked custom validators/transforms. Cover when invoke ggen, flag→intent map, annotation surface, regen-after-edit workflow.
---

# ggen — JSON codegen for Go

ggen parse annotated Go structs. Emit `DecodeFrom`, `DecodeFromStream`, `JSONSize`, `AppendJSON`. Generated code = hand-rolled byte scan. No reflection, no token layer. Bytes path (`DecodeFrom` over caller `[]byte`) strings alias input via `unsafe.String` — zero-copy. Stream path (`DecodeFromStream` over `*ggen.Stream`) copy strings out of intermediate buffer so buffer compact safely. See _Stream is not zero-copy_ below.

Module: `github.com/sirkostya009/ggen`. Binary: `ggen`. Go ≥ 1.27.

## When to reach for ggen

Use ggen when ANY hold:

- Hot-path JSON decode/encode where `encoding/json` (v1 or v2) show in CPU/alloc profiles.
- Validation belong at decode time (length, range, regex-light patterns, custom `func(T) error`) — ggen fold into parser. Invalid payloads short-circuit before allocating full value.
- Long/slow streams + validation required AND invalid payloads frequent enough that fail-fast mid-body (vs finish read first) save real bandwidth/CPU.

Skip ggen when:

- Wire shape need `encoding/json` v1 quirks ggen diverge from (URL struct-dump, `sql.NullX` `{Valid:…}` wrapper).

## Install

```sh
go install github.com/sirkostya009/ggen/cmd/ggen@latest # CLI binary
go get github.com/sirkostya009/ggen                 # runtime package
```

## Invocation

```sh
ggen .                 # current package
ggen ./...             # every package matched by the pattern — module-scoped, same as `go build ./...`
ggen ./pkg/...         # subtree pattern (relative paths must start with `./`)
ggen ./a ./b/...       # several targets in one run — one load, dependencies generated first regardless of order
ggen <dir>             # one package
ggen <file.go>         # one file
ggen <file.go> Foo Bar # one file, only structs named Foo or Bar, will fully overwrite existing <file_ggen.go> file
```

Test-only packages (no non-`_test.go` files) skipped in pattern mode. Invoke `ggen <dir>` directly (also inside a multi-target run) when target only has `_test.go` sources. `-o` / `-pkg` accepted only with a single file or a single plain dir.

Every mode reports every annotated struct's errors before exiting; a failed run never touches an existing generated file. Unknown `//ggen:generate` token = error listing the known ones.

**Run ggen under same `GOEXPERIMENT` as user build** — `packages.Load` honor build tags. Files behind an experiment tag (e.g. `goexperiment.simd`) invisible without it:

```sh
GOEXPERIMENT=simd ggen ./...
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

- Package mode: `<dir-basename>_ggen.go` (and `_ggen_test.go` if annotated struct in `_test.go`; `_xtest_ggen_test.go` under `package <pkg>_test` for the external test package — `-pkg X` makes that `X_test`).
- Single-file mode: `<basename>_ggen.go`; an external test file → `<basename>_ggen_test.go` under `package <pkg>_test`.
- Source with `//go:build foo`: land in `<dir>_foo_ggen.go`, constraint preserved. Multi-term constraints get slugified filename (`//go:build foo && bar` → `<dir>_foo_bar_ggen.go`).
- File-name constraints count too: `os_linux.go`, `cpu_amd64.go`, `x_linux_amd64.go`, a file importing `"C"` → `<dir>_linux_ggen.go` etc. with a matching `//go:build` header (ANDed with any explicit line).
- `-o <path>` override path in single-file or single-package mode.

## Flags (global) and per-struct annotations (local)

Most flags have matching annotation token (no leading dash). Annotations space-separated after `//ggen:generate`.

| CLI flag            | annotation         | effect                                                                                                                                                                                                                                                                        |
| ------------------- | ------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `-o <path>`         | —                  | override output path (single-file / single-package only; rejected with several targets or a pattern)                                                                                                                                                                          |
| `-pkg <name>`       | —                  | override the package name in the generated file (same scope as `-o`)                                                                                                                                                                                                           |
| `-marshal`          | `marshal`          | also emit `MarshalJSON` so the type satisfies `encoding/json.Marshaler`                                                                                                                                                                                                       |
| `-unmarshal`        | `unmarshal`        | also emit `UnmarshalJSON` for `encoding/json.Unmarshaler`                                                                                                                                                                                                                     |
| `-multierr`         | `multierr`         | accumulate every validation failure into `ggen.Errors` (slice) instead of returning on the first                                                                                                                                                                        |
| `-allowdups`        | `allowdups`        | accept duplicate JSON keys, first-wins (default: error on second occurrence)                                                                                                                                                                                                  |
| `-novalidate`       | `novalidate`       | drop validation, required-field checks, and mods                                                                                                                                                                                                                              |
| `-ignoreunknown`    | `ignoreunknown`    | silently drop unknown JSON keys (default: error). Overridden when an embedded fallback map is present                                                                                                                                                                          |
| `-nullzero`         | `nullzero`         | accept explicit JSON `null` on every non-pointer value field, decoding it to the Go zero (default: error). A per-field `nullzero` decode variant in `pipe:` opts in one field                                                                                                 |
| `-nosortkeys`       | `nosortkeys`       | emit struct fields in declaration order (default: alphabetical, compresses better)                                                                                                                                                                                            |
| `-usenumber`        | `usenumber`        | decode JSON numbers in `any` fields as `json.Number` instead of `float64`                                                                                                                                                                                                     |
| `-htmlescape`       | `htmlescape`       | escape `<`, `>`, `&` to `\uXXXX` (default: literal, matches `encoding/json` v2)                                                                                                                                                                                               |
| `-allowinvalidutf8` | `allowinvalidutf8` | skip decode UTF-8 validation: invalid bytes pass raw into strings/keys/raw spans (inside `any` values and the `,embed` catch-all too), unpaired surrogates → U+FFFD (default: reject, jsonv2 parity). Grammar checks stay                                                       |
| `-copy`             | `copy`             | bytes-path `DecodeFrom` copies strings / `json.RawMessage` / any-embedded strings out of the input instead of aliasing it, so input is safe to store long term after decode                                                                                                   |
| `-dry`              | —                  | parse + validate every annotated struct, surface all errors, emit no file. Rejects `-o`/`-pkg`                                                                                                                                                                                |
| `-simd <tier>`      | —                  | SIMD tier for string scans + marshal escape scans: `off`/`avx`/`avx2`/`avx512`. `GOEXPERIMENT=simd` at ggen invocation auto-selects `avx`; wider tiers are explicit opt-ins. Tier is baked in at generate time; output requires `GOEXPERIMENT=simd` to build + a matching CPU |
| `-v`                | —                  | info-level progress (e.g. `wrote <file>`)                                                                                                                                                                                                                                     |
| `-vv`               | —                  | debug-level: per-package / per-struct diagnostics                                                                                                                                                                                                                             |
| `-vvv`              | —                  | trace-level diagnostics                                                                                                                                                                                                                                                       |

CLI flags apply to all structs in pass. Annotations apply to struct they on.

Verbosity flags `-v`, `-vv`, `-vvv` for troubleshooting only. CLI always report descriptive errors.

## Struct annotation

Trigger: `//ggen:generate` (no space between `//` and `ggen`, mirror `//go:generate`). Goes on struct or top-level type alias.

```go
//ggen:generate
type User struct {
	ID    int      `json:"id"`
	Name  string   `json:"name"   pipe:"required minlen=1 maxlen=64"`
	Email string   `json:"email"  pipe:"trim tolower"`
	Tags  []string `json:"tags,omitempty" pipe:"inner:notempty"`
}

//ggen:generate marshal unmarshal multierr
type Order struct { /* ... */ }
```

## Field tags

### `json:"..."` — same as stdlib, plus

- `json:",embed"` — field = catch-all map for unknown keys. Type must be a plain string-keyed map (`map[string]V` — not `*map`, not a named map type); V may be `any`, a primitive, a ggen-annotated struct, or any other type (typed elems dispatch through the elem's fast path or `encoding/json.Unmarshal` over the captured span). Overrides `-ignoreunknown`. On marshal the entries land after every named field regardless of where the catch-all is declared (same order inside an `any` value). One per struct: own beats promoted, two side by side = generate-time error.
- `json:"name,omitempty"` — skip on marshal when the value ENCODES as `null`/`""`/`[]`, or is an empty map: empty string, `any` holding an empty value, nil pointer, pointer to any of these all skip; numbers/`false` never. `big.Int`/`Float`/`Rat` are never skipped: a zero one encodes as `0`/`"0"`, which is not JSON-empty. `[N]byte` never skips either.
- `omitempty` on a STRUCT field = generate-time error: ggen always writes a struct's object, so the option would be a no-op. Use `omitzero`, or a pointer (omits when nil; a non-nil pointer to a struct with nothing to emit writes `{}`).
- `json:"name,omitzero"` — skip on marshal when Go-zero. `time.Time` is asked `IsZero()` (zero time with a location still skips); other structs compare structurally.
- Name is verbatim (`json:" a"` → key ` a`, as stdlib). Whitespace-padded option (`json:"a, omitempty"`) = error. `case:ignore`/`case:strict` = error (ggen matches keys exactly). Misspelled options (`omitEmpty`, `Inline`, `Format:hex`) = error, as jsonv2.
- `json:"name,string"` — wrap numeric as JSON string (unwrap on decode).
  Numerics only, like encoding/json/v2 (`bool` is a no-op; anything else is a
  generate-time error).
- `json:"name,format:X"` — format hint for native types (see kinds below). MUST be last option in tag (jsonv2 rule). Quote values with special characters: `format:'Jan 2, 2006'`. Literal quote = `\\'` (a bare `\'` is an invalid Go string escape — ggen errors instead of dropping the tag). An unrecognized format, or one on a type that has none, is a generate-time error; `time.Time` accepts any custom Go layout. An unterminated `'` is rejected too.
- Quoted names (jsonv2): `json:"'a,b'"` → key `a,b`; `json:"'-'"` → key `-`. Only a bare `json:"-"` ignores the field; `json:"-,..."` is a generate-time error.

Only exported fields read/written, same as `encoding/json`.

### `pipe:"..."` — decode, transform, validate

One ordered, whitespace-separated pipeline: presence, an optional decode stage,
then value steps (mods + validators) that run **in declared order**. Values
with spaces are single-quoted. `|` is an intra-rule arg separator
(`oneof`/`replace`/`clamp`); quote per PART there — quotes scope one part and
protect a literal pipe: `oneof='New York'|LA`, `replace='a|b'|c`.

```go
Name  string	`json:"name"  pipe:"required trim minlen=1 maxlen=50"`
Email string	`json:"email" pipe:"trim tolower contains=@"`
Aliases map[string][]string	`json:"aliases" pipe:"keys:(minrunes=2 maxrunes=32) inner:maxlen=10 inner:notempty"`
```

**Presence:** `required` (key must appear → `RequiredError`) / `optional`
(default, explicitly states "may be absent"). Position-independent. Absent → Go zero.
Field-level only: under `inner:`/`keys:` (any spelling) they are rejected — an
element or map key is never absent.

**Decode stage (`/` variants):** by default a field decodes from its type's
natural JSON shape. List `/`-separated variants to accept more (one per shape):

- `.` — native decode of the field type.
- `nullzero` — accept JSON `null` → Go zero. Allows non-pointer values to
  accept `null` values as Go-zero.
- `@Conv` — converter `func(W) T` / `(T,error)` / `(T,bool)`; ggen scans input
  `W` (primitive, ggen struct, a pointer to either — then `null` is accepted
  and passed as `nil` — or a same-package named primitive), then calls it.
  Needs a `/`, leading `.`, or `~` to read as a converter. Encode is
  unaffected (marshals as native T).

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
| `url`, `alphanum`, `numeric`, `hexadecimal`                   | string        | character-class predicate   |
| `islower`, `isupper`                                          | string        | no wrong-case letters (caseless runes pass) |
| `starts=X`, `ends=X`, `contains=X`                            | string        | substring test              |

Mods (transforms):

| step                        | applies to | effect                                                                      |
| --------------------------- | ---------- | --------------------------------------------------------------------------- |
| `trim`, `tolower`, `toupper` | string    | whitespace / case                                                           |
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
numeric on non-numerics, `inner:` on non-containers — `[]byte`/`[N]byte`
included, they are one base64 string — `keys:` on non-maps, etc.)
with a clear diagnostic; each value step is gated against the working type at
its level. Struct/opaque types reject kinded rules too (named primitives
resolve to the underlying kind first; `required`/`optional`/`@Func` still
apply) — use a custom `@Func` validator for struct-typed fields. Numeric
bounds parse at the field's width and sign (`gte=9223372036854775808` on
`uint64` ok; `lte=300` on `int8`, `gte=-1` on `uint` = error); numeric
`oneof` parts dedupe by value, so integers above 2^53 stay distinct.

### `hint:"..."` — preallocation

`hint:"N"` → `make([]T, 0, N)` (slice/map only), overriding `minlen`-derived
caps and the default. With no hint: `maxlen=N` when N elements fit 512 B,
else element width — as many as fit within 80 B, else within 512 B, else 1. Per-level: `hint:"32 inner:8"`. `hint:"0"` disables;
negative is a generate-time error, as is anything above 2147483647, the largest capacity that is a legal `int` on a 32-bit target (same ceiling for `len=N`/`minlen=N` on a slice/map — they size the allocation).

#### Inspecting errors

```go
var e *ggen.MinLenError
if errors.As(err, &e) {
	// e.Path, e.Limit, e.Got
	// e.Pos — failure byte offset, relative to the full payload
}
```

`GTError`/`GTEError`/`LTError`/`LTEError` carry the bound in `Limit`, `MultipleError` in `Of` — both `any`, spelled with the field's own type, so a `uint64` bound above 2^53 is reported exactly instead of rounded. Type-switch or print with `%v`.

In `multierr` mode generated code return `ggen.Errors` (`[]ggen.Error`). Implement `Unwrap() []error`.

Parse failures (malformed JSON, wrong primitive type) wrap in `*ggen.ParseError` carrying `Path` (root-relative segments, `[]string{"addr","street"}`), `Pos` (byte offset where scanning stopped — identical for `DecodeFrom` and `DecodeFromStream` at any buffer size), `Err` (underlying `ggen.ErrX` sentinel). `errors.Is(err, ggen.ErrBadString)` keeps working through the wrap. Validation errors NOT wrapped — typed pointers stay reachable. `ParseError.Error()` renders `parse error at <a.b.c> (pos <n>): <cause>`.

Sentinels identical on both paths: an unescaped control byte inside a string → `ErrBadString` with `Pos` on the control byte itself, closing quote or not (`"ab\x01}` → 3); `n`/`t`/`f` not spelling its literal → `ErrBadLiteral`/`ErrBadBool`, `Pos` on the first wrong byte; `,string` number outside JSON grammar (`"NaN"`, `"+1"`, `"01"`) → `ErrBadNumber`; `[N]T` with too many elements → `LenError{Want: N, Got: N+1, AtLeast: true}` (decode stops at first extra element, `Got` = lower bound, message says "at least"; too few: exact count, `AtLeast` false). Malformed escape → `Pos` on its backslash (`"ab\u00zz"` → 3); input that ran out mid-escape → `Pos` at end of input (`"ab\` → 4, `"ab\u00` → 7); same on both paths, any chunk size.

## Supported field kinds

- Named types over a primitive (`type Priority string`, any alias depth) — scanned as the underlying + converted, at every position; the annotation is only needed for the METHODS or for per-type `htmlescape`/`copy` behaviour.
- Primitives: `string`, `bool`, `int*`, `uint*`, `float*`, plus `*T` for any (`null` ↔ `nil`).
  Nested ptrs `**T`/`***T`/... also native: `null` → nil outer, otherwise value parse first and missing levels alloc'd. `float32` decodes with one rounding, bit-identical to `encoding/json`.
- `[]T`, `map[string]V` (string keys only), `[N]T` (strict element count — mismatch → `ggen.LenError`; `[N][]byte` = tuple of base64 strings).
- `[0]T` — marshals as `[]`, decodes only `[]` (anything else → `ggen.LenError{Want: 0, Got: 1, AtLeast: true}`), in every position: field, slice element, array slot, map value, nested `[0][N]T`.
- `[]*T` / `[N]*T` of structs — single slab backing, ~log(N) allocs vs N. Multi-level elements (`[]**T`, `[N]**T`) and pointer map values (`map[string]*V`, `**V`, …) decode natively through the same null/alloc cascade as scalar pointer fields.
- Nested struct (same package: direct call — reached structs are generated too, through embedded fields at any depth; a same-package struct with its own `DecodeFrom`/`MarshalJSON`+`UnmarshalJSON`/`MarshalText`+`UnmarshalText` and no annotation takes the cross-package ladder instead; cross-package: see below).
- Embedded struct — fields promoted to parent JSON object. `json:"-"` on the embedding drops it entirely (any type); any other `json` tag on an embedding = generate-time error. Two promoted fields sharing a Go name but not a JSON name = generate-time error (stdlib keeps both).
- Anonymous struct / func / chan field types = generate-time error at any depth (a func or chan has no JSON shape at all — drop it with `json:"-"` or unexport it; an anonymous struct wants a named type). A field the tag ignores (`json:"-"`) is exempt whatever its type. Interface fields are supported: `any` gets the full decode shape below, one WITH methods (`io.Reader`, `interface{ M() }`) marshals its dynamic value and takes only `null` on decode, as `encoding/json` does. Generic types and `=` alias declarations rejected (`type IntBox Box[int]` works).
- `any` / `interface{}` — full stdlib-compatible decode shape, plus `usenumber` for `json.Number` numbers.
- `rune` (int32) / `byte` (uint8) decode as numbers, like their underlying kinds.
- `[N]byte`, `*[N]byte` (any pointer depth) — base64 string with a STRICT decoded length (jsonv2 parity; `encoding/json` v1 instead emits a number array and rejects the string). Honors the same `format:` set as `[]byte` at every depth; `format:array` opts into the v1 number-array shape. Wrong decoded length → `ggen.LenError`; `null` ↔ nil for the pointer forms (`*[8]byte` ↔ `"AQIDBAUGBwg="` or `null`).
- `[]byte` — `format:base64` (default), `base64url`, `base32`, `base32hex`, `base16`/`hex`, `array`. `null` ↔ `nil` (nil marshals as `null`, empty non-nil as `""`/`[]`).
- `time.Time` — `format:RFC3339Nano` (default), `RFC3339`, `unix`, `unixmilli`, `unixmicro`, `unixnano`, other `time.X` constants, or custom layout `format:'2006-01-02'`. Default/`RFC3339`/`RFC3339Nano` are strict RFC 3339 both ways (jsonv2 parity): year outside 0–9999 or zone hour ≥ 24 fails to marshal; `,` fraction, one-digit hour, zone hour ≥ 24 or zone minute ≥ 60 fails to parse (time.Parse/v1 accept those).
- `time.Duration` — `format:units` (default, `"1h30m"`), `sec`, `milli`, `micro`, `nano`. Inside `any` the same units string is emitted.
- `net.IP`, `netip.Addr`, `netip.Prefix` — text form. `""` ↔ zero value (nil `net.IP`); `null` → nil for `net.IP` only.
- `json.RawMessage` / `jsontext.Value` — opaque, zero-copy alias.
- `net/url.URL` — JSON string (NOT struct dump — wire divergence from stdlib).
- `math/big.Int` (JSON number), `big.Float` / `big.Rat` (JSON string — wire divergence from stdlib).
- `database/sql.Null*` and generic `sql.Null[T]` (Go 1.22, any inner `T` ggen handles as a field — primitive, `time.Time`, `uuid.UUID`, named types, …) — inner value or `null` (NOT `{Valid:…}` — wire divergence from stdlib). Narrow inners (`NullInt16`/`NullInt32`/`NullByte`) reject out-of-range with `ErrNumberOverflow`.
- Any type implementing `encoding.TextAppender` / `TextMarshaler` / `TextUnmarshaler` — auto-picked (`google/uuid`, `gofrs/uuid/v5`, `shopspring/decimal`, `oklog/ulid`, `segmentio/ksuid`, `rs/xid`, `net/mail.Address`, custom enums, etc.). Field types imported under an alias (`import lf "…/leaf"`) fine — generated code binds its own import names.
- `any` on marshal follows jsonv2: pointer-receiver `MarshalJSON`/`MarshalText`/`AppendText` reached for values wherever they sit (`big.Rat`/`big.Float` values inside `any` → `"1/2"`/`"1.5"`), map keys with a text marshaler emitted through it, `omitempty`/`omitzero` on reflected struct fields per the rules above (a struct member is written even when it comes out `{}`, matching generated code).

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
| struct (exported fields)         | `type Comment Inner`    | field introspection — treats the alias like a regular struct; `@Func`/`@Conv` in the underlying's tags honoured, resolved in the alias's package |
| struct (has `DecodeFrom`)        | `type X HasGgenMethods` | cast + delegate, also when the methods are generated in the same run (an alias carrying its own annotation introspects instead) |
| struct (opaque + Marshaler/Text) | `type Local time.Time`  | delegate to underlying's `MarshalJSON`/`MarshalText` (`AppendText` preferred) |
| container                        | `type Tags []string`    | same emitters as slice/map/array fields                                       |

Aliases of channels, interfaces, functions rejected at generate time.

## Generated method surface

```go
func (result T) DecodeFrom(data []byte) (T, int, error)
func (result T) DecodeFromStream(s *ggen.Stream) (T, error)
func (s T) JSONSize() int
func (s T) AppendJSON(dst []byte) ([]byte, error)
```

`JSONSize` = upper bound on `AppendJSON` output; presized buffer never grows. `any` fields measured from held value at call time. Control bytes in strings can exceed it (budget assumes valid JSON text).

With `marshal` / `unmarshal` annotations:

```go
func (s T)  MarshalJSON() ([]byte, error)
func (s *T) UnmarshalJSON(data []byte) error
```

### Stream is not zero-copy

`DecodeFromStream` take `*ggen.Stream` wrapping `io.Reader` behind user-provided `[]byte` buffer. Buffer sit between reader and parser — chunks land there via `Read`, parser scan out of it, compaction recycle space mid-decode so buffer stays bounded to roughly `max(chunk_size, single_value_size)` across long streams.

Strings, `json.Number`, `json.RawMessage` values **copied** out of buffer, not aliased — buffer not grown unless must, aliases won't stick. Trade-off: ~2–3× more allocs than bytes path. Stream instead capable of recycling user-provided buf for extremely large payloads.

Bytes-path (`DecodeFrom`) still zero-copy via `unsafe.String` into caller `data` — see pitfalls below.

### Decode-into-receiver (merge)

Decoders parse values into method non-pointer receiver. The RESULT is what a fresh decode would give — every field the payload omits comes back zeroed (`nil` for a pointer, empty-but-allocated for a container) — and the receiver is there for its MEMORY. Non-nil slices/maps reuse capacity (through pointer levels too, wherever the chain lives — `*[]T`/`**map[string]T` fields, `map[string]*[]T` values, `[]**map` elements — reset the pointee, never append into it), nested slices reuse the inner rows' backing arrays at any depth, a `[]Struct` reuses the allocations inside each carried element, and a present pointer key decodes into the carried pointee. A field going through `UnmarshalJSON`/`UnmarshalText`/`encoding/json` is zeroed before the call, so it too comes back fresh. Niche, useful when the same object is reused for multiple (not necessarily _different_) payloads.

NOT 100% compatible with stdlib — ggen diverges in three ways: an OMITTED key is zeroed/reset rather than left alone (blank payload → blank slate, capacity kept), a PRESENT map key replaces the whole map (clear+refill; stdlib merges entries into it), and an explicit `null` on a non-pointer scalar/native field ERRORS (stdlib zeroes it — only pointer/slice/map/`[]byte`/`sql.Null*`/raw fields accept `null`). Slice-replace, null→nil for slice/map/pointer, nested-struct merge, and `*T`/`**T` reuse on a present key all match stdlib.

```go
u, _, err := existing.DecodeFrom(payload)
```

Generic helper funcs (`Unmarshal*`) no merge semantics.

Call from user code:

```go
import "github.com/sirkostya009/ggen"

// single value — call the generated method directly with a zero-value receiver
u, _, err := User{}.DecodeFrom(payload)
out, err := ggen.Marshal(u)
// primitive aliases (`type UserID uint64`): use a typed zero
// id, _, err := UserID(0).DecodeFrom(payload)

// slices (loop helpers — saves caller from reimplementing the array walk)
users, err := ggen.UnmarshalSlice[User](payload)
out, err = ggen.MarshalSlice(users)
out, err = ggen.AppendSlice(out[:0], users) // can use just AppendSlice to reuse buffers

// streaming single value — caller owns the ggen.Stream
s := ggen.NewStream(req.Body, nil)  // or pre-sized buf, e.g. make([]byte, 0, hint)
u, err = User{}.DecodeFromStream(s)
// s.Bytes() is now recyclable
// (use `var s ggen.Stream; s.Reset(...)` to stack-allocate)

// streaming — Reset returns *Stream so it chains into the generic methods;
// neither rejects trailing data, so reading continues after.
s = ggen.NewStream(req.Body, buf[:0])
u, err = s.Value[User]()      // one value
users, err = s.Slice[User]()  // JSON array of T

// buffer reuse — pass a previous value/slice as rcv. Generated decoders seed
// from the receiver and reset containers keeping cap, so maps+slices recycle.
// Slice reuses EACH ELEMENT's containers too, not just the outer array.
// Steady state = 0 allocs. NOTE: a key the payload omits comes back zeroed.
u, err = s.Value(u)
users, err = s.Slice(users)

// lazy array iteration — Slice gathers []T, Array yields elements and keeps
// nothing. Cursor lands past the `]`, so the Stream reads on afterwards.
for item, err := range ggen.NewStream(r, buf[:0]).Array[Item]() { ... }

// unbounded iteration (NDJSON / concatenated values / socket). Reuses ONE
// value per run (optional seed warms it) → 0 allocs/element, but a yielded
// value is valid only until the next pull. Copy what you retain.
for ev, err := range ggen.NewStream(conn, buf[:0]).Seq[Event]() { ... }
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

Build tag propagation: struct in file behind `//go:build foo` land in `<dir>_foo_ggen.go` with same constraint; file-name constraints (`_linux.go`, `_amd64.go`, cgo) propagate the same way. Unconstrained builds not broken.

## Pitfalls

1. **Zero-copy aliasing.** Decoded strings (and `json.RawMessage` / `jsontext.Value`) alias source `[]byte`. Mutating input after `DecodeFrom` silently corrupt decoded values. Streaming path copy strings, safe to recycle buffer between calls. Decode never reads past the input slice on any SIMD tier — mmap'd files, page-aligned buffers, foreign memory need no padding.
2. **Long-lived references can balloon heap.** A short string field from a large payload stays referenced (cached, stored in a struct held forever) → Go's non-compacting GC keep the entire backing buffer alive. For long-lived data, copy field (`s := string([]byte(decoded.X))`) or use streaming.
3. **Wire-shape divergences from stdlib** for `net/url.URL` (string, not struct dump) and `sql.Null*` (inner-or-null, not `{Valid:…}` wrapper). Round-trip through ggen fine. Pipe through stdlib `encoding/json` reshape value.
4. **AST-only fallback when no `go.mod`.** When `packages.Load` cannot resolve types (e.g. temp file with no module context), ggen fall back to AST-only mode and emit `encoding/json` for cross-package types. Slower but correct.
5. **Build under right `GOEXPERIMENT`.** Files behind an experiment tag (e.g. `goexperiment.simd`) invisible without `GOEXPERIMENT=simd ggen ./...`. `encoding/json/v2` needs none — stable since Go 1.27.
6. **Test files first-class inputs.** Annotated structs in `_test.go` files route to `_ggen_test.go` so methods don't bundle into library build.
7. **`hint:` only safe prealloc hint.** Don't expect `maxlen` to size container — it doesn't (intentional, retained-heap reasons). Use `hint:"N"` when know typical size.
8. **Decode rejects invalid UTF-8; encode doesn't validate.** Decode: malformed UTF-8 bytes or unpaired `\uXXXX` surrogates in string values/keys → `ggen.ErrInvalidUTF8` (jsonv2 parity; v1 instead silently replace with U+FFFD). ASCII strings pay nothing. Captured raw spans (`json.RawMessage`/`jsontext.Value`) byte-validated too. Exceptions: skipped content (`ignoreunknown`) grammar-checked only; unpaired surrogate ESCAPE inside raw span pass (ASCII text there; jsonv2 reject). Opt out per struct: `allowinvalidutf8`. Encode: invalid bytes in struct strings emitted raw onto wire (v1 replace, v2 replace+error) — validate at boundary if populating structs from untrusted bytes.
9. **Dup-key detection covers DECLARED keys only.** `DuplicateKeyError` fires for repeated keys ggen actually decodes into fields. Dups inside skipped content (`ignoreunknown`), `any` values (map last-wins), RawMessage spans, and nested sub-objects pass silently — jsonv2 rejects those too. Intentional: the contract is "fields ggen DECODES are unambiguous", and a dup in a discarded span can't change the result. `-allowdups` opts out of the declared-key check.
10. **Nesting capped at 10000 levels (jsonv2 parity), decode AND encode.** Deeper → `ggen.ErrMaxDepth`, NOT a stack-overflow crash. Decode: every recursive path (nested containers, `any`, `ignoreunknown` skip, RawMessage, self-referential structs), bytes + stream — untrusted deeply-nested input is safe. Encode: `ggen.AppendAny`/`AppendAnyHTML` cap too, so an over-deep or CYCLIC `any` value (a slice/map/pointer that reaches itself) errors instead of killing the process. Anything ggen decodes it can re-encode.

## Common user intents → flags

| User says                                                   | Reach for                                                                                             |
| ----------------------------------------------------------- | ----------------------------------------------------------------------------------------------------- |
| "still want `json.Marshal(u)` to work"                      | `-marshal` (and/or `-unmarshal`)                                                                      |
| "collect all errors, not just the first"                    | `-multierr`                                                                                           |
| "skip unknown keys silently"                                | `-ignoreunknown` or a `json:",embed"` catch-all map                                                  |
| "accept `null` on a scalar instead of erroring"             | a `nullzero` decode variant in `pipe:` per field, or `-nullzero` / `//ggen:generate nullzero` for all |
| "fastest possible decode, I trust the input"                | `-novalidate` (+ `-allowinvalidutf8` if input may carry non-UTF-8 strings)                            |
| "payload has broken UTF-8 / lone surrogates, decode anyway" | `-allowinvalidutf8` or `//ggen:generate allowinvalidutf8` per struct                                  |
| "wire output embedded directly in HTML"                     | `-htmlescape` (or per-type via alias `//ggen:generate htmlescape`)                                    |
| "exact-precision numbers (big ints, no float64)"            | `-usenumber` for `any` fields; or use `math/big.Int`                                                  |
| "duplicate keys should be accepted (first wins)"            | `-allowdups`                                                                                          |
| "keep field order matching declaration"                     | `-nosortkeys`                                                                                         |
| "i want only some strings to have html escaping"            | `//ggen:generate htmlescape` `type HTMLString string`                                                 |
| "this struct has json tags but I want it to parse faster"   | `//ggen:generate` `type Alias OtherStruct`                                                            |
| "validate annotations in CI without writing files"          | `-dry` (parse + validate every annotated struct, surface every error, emit no file)                   |
| string field is going to be stored in global state          | `-copy` to avoid keeping a reference to potentially huge payload on a bytes path                      |
