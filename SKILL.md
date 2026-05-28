---
name: ggen
description: Drive the ggen CLI to generate zero-copy, zero-reflection JSON encode/decode methods for annotated Go structs. Use when the user wants faster JSON codec performance than `encoding/json`, validation rules baked into decode, or compile-time-checked custom validators/transforms. Covers when to invoke ggen, which flags map to which user intents, the annotation surface, and the regen-after-edit workflow.
---

# ggen — JSON codegen for Go

ggen parses annotated Go structs and emits `DecodeFrom`,
`DecodeFromStream`, `JSONSize`, and `AppendJSON` methods. Generated
code is hand-rolled byte scanning, no reflection, no token layer. On
the bytes path (`DecodeFrom` over the caller's `[]byte`) strings alias
the input via `unsafe.String` — zero-copy. The stream path
(`DecodeFromStream` over `*scan.Stream`) copies strings out of an
intermediate buffer so the buffer can compact safely; see _Stream is
not zero-copy_ below.

Module: `github.com/sirkostya009/ggen`. Binary: `ggen`. Go ≥ 1.26.

## When to reach for ggen

Pick ggen when ANY of these hold:

- Hot-path JSON decode/encode where `encoding/json` (v1 or v2) shows up
  in CPU/alloc profiles.
- Validation rules belong at decode time (length, range, regex-light
  patterns, custom `func(T) error`) — ggen folds them into the parser
  so invalid payloads short-circuit before allocating the full value.
- Long or slow streams where validation is required AND invalid payloads
  show up often enough that fail-fast bailing mid-body (rather than
  finishing the read first) saves real bandwidth / CPU.

Skip ggen when:

- The struct's wire shape needs `encoding/json` v1 quirks ggen
  deliberately diverges from (URL struct-dump, `sql.NullX` `{Valid:…}`
  wrapper).

## Install

```sh
go install github.com/sirkostya009/ggen@latest # CLI binary
go get github.com/sirkostya009/ggen            # runtime subpackages
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

Test-only packages (no non-`_test.go` files) are skipped under pattern
mode — invoke `ggen <dir>` directly when the target only has
`_test.go` sources.

**Run ggen under the same `GOEXPERIMENT` as the user's build** —
`packages.Load` honors build tags, so files behind `goexperiment.jsonv2`
are invisible without it. For example, in repos using jsonv2:

```sh
GOEXPERIMENT=jsonv2 ggen ./...
```

## Agent-mode output (do not truncate)

ggen auto-detects when it's being driven by a coding agent and switches
its logger from the pretty multi-line / ANSI-coloured layout (humans)
to a concise one-line-per-record format. The concise mode is also
triggered under CI or any non-TTY stderr.

Any non-empty value of the following environment variables enable it:

- `AI_AGENT` (generic cross-vendor)
- `CLAUDECODE` (Anthropic's Claude Code)
- `CURSOR_TRACE_ID` (Cursor IDE)
- `AIDER_AUTO_COMMITS` (Aider)
- `CI` / `GITHUB_ACTIONS` / `GITLAB_CI` / `CIRCLECI` / `JENKINS_HOME` /
  `BUILDKITE` / `TRAVIS` / `APPVEYOR` / `TF_BUILD` /
  `TEAMCITY_VERSION` / `CONTINUOUS_INTEGRATION`
- non-TTY stderr (piped / redirected)

Each line is self-contained and follows pattern: `<level>: [file:line:col:] <msg> [(hint)]`.
Levels are `inf:` / `dbg:` / `trc:` / `err:`. Every line is signal — **do
not truncate** (`head` / `tail` / `grep -v`).

## Output file naming

- Package mode: `<dir-basename>_ggen.go` (and `_ggen_test.go` if any
  annotated struct lives in a `_test.go` file).
- Single-file mode: `<basename>_ggen.go`.
- Source with `//go:build foo`: lands in `<dir>_foo_ggen.go` with the
  same constraint preserved. Multi-term constraints get a slugified
  filename (`//go:build foo && bar` → `<dir>_foo_bar_ggen.go`).
- `-o <path>` overrides the path in single-file or single-package mode.

## Flags (global) and per-struct annotations (local)

Most flags have a matching annotation token (no leading dash).
Annotations are space-separated after `//ggen:generate`.

| CLI flag         | annotation      | effect                                                                                                 |
| ---------------- | --------------- | ------------------------------------------------------------------------------------------------------ |
| `-o <path>`      | —               | override output path (single-file / single-package only)                                               |
| `-pkg <name>`    | —               | override the package name in the generated file                                                        |
| `-marshal`       | `marshal`       | also emit `MarshalJSON` so the type satisfies `encoding/json.Marshaler`                                |
| `-unmarshal`     | `unmarshal`     | also emit `UnmarshalJSON` for `encoding/json.Unmarshaler`                                              |
| `-multierr`      | `multierr`      | accumulate every validation failure into `validation.Errors` (slice) instead of returning on the first |
| `-allowdups`     | `allowdups`     | accept duplicate JSON keys, first-wins (default: error on second occurrence)                           |
| `-novalidate`    | `novalidate`    | drop validation, required-field checks, and mods                                                       |
| `-ignoreunknown` | `ignoreunknown` | silently drop unknown JSON keys (default: error). Overridden when an inline catch-all map is present   |
| `-nosortkeys`    | `nosortkeys`    | emit struct fields in declaration order (default: alphabetical, compresses better)                     |
| `-usenumber`     | `usenumber`     | decode JSON numbers in `any` fields as `json.Number` instead of `float64`                              |
| `-htmlescape`    | `htmlescape`    | escape `<`, `>`, `&` to `\uXXXX` (default: literal, matches `encoding/json` v2)                        |
| `-dry`           | —               | parse + validate every annotated struct, surface all errors, emit no file. Rejects `-o`/`-pkg`         |
| `-v`             | —               | info-level progress (e.g. `wrote <file>`)                                                              |
| `-vv`            | —               | debug-level: per-package / per-struct diagnostics                                                      |
| `-vvv`           | —               | trace-level diagnostics                                                                                |

CLI flags apply to all structs in the pass. Annotations apply to the
struct they're on.

Use verbosity flags `-v`, `-vv`, `-vvv` for troubleshotting purposes only.
CLI always reports descriptive errors.

## Struct annotation

The trigger is `//ggen:generate` (no space between `//` and `ggen`,
mirroring `//go:generate`). Goes on the struct or on a top-level
type alias.

```go
//ggen:generate
type User struct {
	ID    int      `json:"id"`
	Name  string   `json:"name"   ggen:"required,minlen=1,maxlen=64"`
	Email string   `json:"email"  ggen:"email" mod:"trim,lower"`
	Tags  []string `json:"tags,omitempty" ggen:"dive:notempty"`
}

//ggen:generate marshal unmarshal multierr
type Order struct { /* ... */ }
```

## Field tags

### `json:"..."` — same as stdlib, plus

- `json:",inline"` — field becomes catch-all map for unknown keys.
  Field type MUST be `map[string]any`. Overrides `-ignoreunknown`.
- `json:"name,omitempty"` — skip on marshal when JSON-empty.
- `json:"name,omitzero"` — skip on marshal when Go-zero.
- `json:"name,string"` — wrap primitive as JSON string (and unwrap on
  decode).
- `json:"name,format:X"` — format hint for native types (see kinds
  below). MUST be the last option in the tag (jsonv2 rule).

Only exported fields are read/written, same as `encoding/json`.

### `ggen:"..."` — validation rules

Comma-separated rules with three optional mode prefixes:

- `(no prefix)` — applies to the field itself, or the whole
  slice/map/array for container fields.
- `dive:` — applies subsequent rules to the next nested level. Each
  extra `dive:` peels another layer.
- `keys:` — applies rules to map keys only (always strings).

Prefixes compose. Example:

```go
Aliases map[string][]string `json:"aliases" ggen:"keys:minrunes=2,maxrunes=32,minlen=1,dive:maxlen=10,dive:notempty"`
```

Rules:

| rule                                                                         | error type                                     | checks                                                              |
| ---------------------------------------------------------------------------- | ---------------------------------------------- | ------------------------------------------------------------------- |
| `required`                                                                   | `RequiredError`                                | field must be present in the JSON object                            |
| `optional`                                                                   | —                                              | marker; no runtime effect                                           |
| `notempty`                                                                   | `NotEmptyError`                                | string non-empty / container non-zero length                        |
| `len=N`, `minlen=N`, `maxlen=N`                                              | `LenError`, `MinLenError`, `MaxLenError`       | byte length (strings) / element count (containers)                  |
| `runes=N`, `minrunes=N`, `maxrunes=N`                                        | `RunesError`, `MinRunesError`, `MaxRunesError` | rune count (utf8 aware) — strings only                              |
| `gt=N`, `gte=N`, `lt=N`, `lte=N`                                             | `GTError`, `GTEError`, `LTError`, `LTEError`   | numeric comparison                                                  |
| `eq=X`, `neq=X`                                                              | `EqError`, `NeqError`                          | equality (string or numeric)                                        |
| `multiple=N`                                                                 | `MultipleError`                                | integer kinds — value `% N == 0`                                    |
| `oneof=a\|b\|c`                                                              | `OneOfError`                                   | value must equal one of the listed alternatives                     |
| `email`                                                                      | `EmailError`                                   | loose email shape                                                   |
| `url`                                                                        | `URLError`                                     | starts with `<scheme>://...`                                        |
| `ascii`, `printable`, `alphanum`, `numeric`, `lower`, `upper`, `hexadecimal` | matching `*Error`                              | character-class string checks                                       |
| `starts=X`, `ends=X`, `contains=X`                                           | `StartsError`, `EndsError`, `ContainsError`    | substring tests on strings                                          |
| `@FuncName`, `@pkg.FuncName`                                                 | `CustomError{Name, Cause}` (with `Unwrap()`)   | call a custom `func(T) error` — see _custom rules_ below            |
| `hintlen=N`                                                                  | —                                              | preallocation hint, NOT a rule — `make([]T, 0, N)` (slice/map only) |

#### Rule applicability

- String-only validators (`email`, `url`, `ascii`, `printable`,
  `alphanum`, `numeric`, `lower`, `upper`, `hexadecimal`, `starts`,
  `ends`, `contains`, `runes`, `minrunes`, `maxrunes`) — strings only.
- Numeric validators (`gt`, `gte`, `lt`, `lte`, `multiple`) — numeric
  kinds. `multiple` requires integer.
- `eq`/`neq`/`oneof` — string or numeric.
- `len`/`minlen`/`maxlen`/`notempty` — any len-able kind
  (string/slice/array/map/[]byte).
- `dive:` only on slice/array/map/[]byte; `keys:` only on maps.

Mismatches are rejected at parse time with a clear diagnostic.

#### Inspecting errors

```go
var e *validation.MinLenError
if errors.As(err, &e) {
	// e.Field, e.Limit, e.Got
}
```

In `multierr` mode the generated code returns `validation.Errors`
(`[]validation.Error`); it implements `Unwrap() []error`.

#### Custom rules

`@FuncName` looks up a `func(T) error` at codegen. `T` must be the
field's exact Go type (including `*T` for pointer fields). No runtime
registry. A non-nil return wraps as
`validation.CustomError{Name: "@FuncName", Cause: err}`.

```go
//ggen:generate
type Box struct { N int `json:"n" ggen:"@EvenOnly"` }

func EvenOnly(n int) error {
	if n%2 != 0 { return errors.New("must be even") }
	return nil
}
```

Cross-package: `@pkg.FuncName`. The resolver reads the source file's
import block, so file-scoped aliases and blank imports
(`_ "path"`) both work.

### `mod:"..."` — input transforms

Mods run after decode, before validation. Same `dive:`/`keys:`
prefixes.

| target  | mods                                                                      |
| ------- | ------------------------------------------------------------------------- |
| string  | `trim`, `lower`, `upper`, `trimleft=X`, `trimright=X`, `replace=old\|new` |
| numeric | `clamp=lo\|hi` (either side may be empty: `clamp=0\|`, `clamp=\|100`)     |
| custom  | `@FuncName` / `@pkg.FuncName`                                             |

Custom mods support two signatures:

- pure: `func(T) T` — emitted as `field = Func(field)`.
- fallible: `func(T) (T, error)` — non-nil error propagates as a
  parse error (early return), NOT validation. Validation never runs
  after a mod failure on the same field.

Mods like `replace` and custom mods may copy the string, breaking
zero-copy for that field.

## Supported field kinds

- Primitives: `string`, `bool`, `int*`, `uint*`, `float*`, plus `*T`
  for any of these (`null` ↔ `nil`).
- `[]T`, `map[string]V` (string keys only), `[N]T` (strict element
  count — mismatch → `validation.LenError`).
- `[]*T` / `[N]*T` of structs — single slab backing, ~log(N) allocs
  instead of N.
- Nested struct (same package: direct call; cross-package: see below).
- Embedded struct — fields promoted to parent's JSON object.
- `any` / `interface{}` — full stdlib-compatible decode shape, plus
  `usenumber` for `json.Number` numbers.
- `[]byte` — `format:base64` (default), `base64url`, `base32`,
  `base32hex`, `base16`/`hex`, `array`.
- `time.Time` — `format:RFC3339Nano` (default), `RFC3339`, `unix`,
  `unixmilli`, `unixmicro`, `unixnano`, other `time.X` constants, or
  a custom layout `format:'2006-01-02'`.
- `time.Duration` — `format:units` (default, `"1h30m"`), `sec`,
  `milli`, `micro`, `nano`.
- `net.IP`, `netip.Addr`, `netip.Prefix` — text form.
- `json.RawMessage` / `jsontext.Value` — opaque, zero-copy alias.
- `net/url.URL` — JSON string (NOT struct dump — wire divergence
  from stdlib).
- `math/big.Int` (JSON number), `big.Float` / `big.Rat` (JSON
  string — wire divergence from stdlib).
- `database/sql.Null*` — inner value or `null` (NOT `{Valid:…}` —
  wire divergence from stdlib).
- Any type implementing `encoding.TextAppender` / `TextMarshaler` /
  `TextUnmarshaler` — picked up automatically (`google/uuid`,
  `gofrs/uuid/v5`, `shopspring/decimal`, `oklog/ulid`,
  `segmentio/ksuid`, `rs/xid`, `net/mail.Address`, custom enums, etc.).

### Cross-package types

For fields whose type lives outside the package being generated,
ggen probes the method set at codegen and emits the first available:

| direction | ladder                                                                                |
| --------- | ------------------------------------------------------------------------------------- |
| decode    | `DecodeFrom` → `UnmarshalJSON` → `UnmarshalText` → `encoding/json.Unmarshal`          |
| encode    | `AppendJSON` → `MarshalJSON` → `AppendText` → `MarshalText` → `encoding/json.Marshal` |

### Type aliases

`//ggen:generate` on a named top-level type works too. Strategy is
picked from the underlying type's shape and method set:

| flavor                           | example                 | strategy                                                                      |
| -------------------------------- | ----------------------- | ----------------------------------------------------------------------------- |
| primitive                        | `type Count int`        | scan + cast                                                                   |
| struct (exported fields)         | `type Comment Inner`    | field introspection — treats the alias like a regular struct                  |
| struct (has `DecodeFrom`)        | `type X HasGgenMethods` | cast + delegate                                                               |
| struct (opaque + Marshaler/Text) | `type Local time.Time`  | delegate to underlying's `MarshalJSON`/`MarshalText` (`AppendText` preferred) |
| container                        | `type Tags []string`    | same emitters as slice/map/array fields                                       |

Aliases of channels, interfaces, and functions are rejected at
generate time.

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

`DecodeFromStream` takes a `*scan.Stream` that wraps an `io.Reader`
behind a user-provided `[]byte` buffer. The buffer sits between the
reader and the parser — chunks land there via `Read`, the parser
scans out of it, and compaction recycles space mid-decode so the
buffer stays bounded to roughly `max(chunk_size, single_value_size)`
across long streams.

Strings, `json.Number` and `json.RawMessage` values are **copied**
out of the buffer rather than aliased — the buffer's not grown unless
it has to, aliases won't stick. Trade-off: ~2–3× more allocs than
the bytes path, Stream instead is capable of recycling user-provided
buf for extremely large payloads.

The bytes-path (`DecodeFrom`) is still zero-copy via `unsafe.String`
into the caller's `data` — see pitfalls below.

### Decode-into-receiver (merge)

Decoders parse values into method non-pointer receiver. Non-nil
slices or maps would have their capacity reused, values overwritten.
Niche, useful for reusing capacity of slice/map fields in cases
the same object is reused for multiple (not necessarily _different_)
paylods.

```go
u, _, err := existing.DecodeFrom(payload)
```

Generic helper functions in `decode` package do not have merge
semantics.

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

After editing any annotated struct (adding/removing fields, changing
tags, adding new `//ggen:generate` types, changing CLI flags):

```sh
ggen ./...
```

Or wire into `go generate`. One directive per package is enough:

```go
//go:generate ggen .
```

Per-file scope works too (use `$GOFILE` for the source basename):

```go
//go:generate ggen $GOFILE
```

Build tag propagation: a struct in a file behind `//go:build foo`
lands in `<dir>_foo_ggen.go` with the same constraint, so unconstrained
builds aren't broken.

## Pitfalls

1. **Zero-copy aliasing.** Decoded strings (and `json.RawMessage` /
   `jsontext.Value`) alias the source `[]byte`. Mutating the input
   after `DecodeFrom` silently corrupts decoded values. The
   streaming path (`UnmarshalStream*`) copies strings, so it's safe
   to recycle the buffer between calls.
2. **Long-lived references can balloon heap.** If a short string
   field from a large payload stays referenced (cached, stored in a
   struct held forever), Go's non-compacting GC keeps the entire
   backing buffer alive. For long-lived data, copy the field
   (`s := string([]byte(decoded.X))`) or use streaming.
3. **Wire-shape divergences from stdlib** for `net/url.URL`
   (string, not struct dump) and `sql.Null*` (inner-or-null, not
   `{Valid:…}` wrapper). Round-trip through ggen is fine; piping
   through stdlib `encoding/json` reshapes the value.
4. **AST-only fallback when no `go.mod`.** When `packages.Load`
   can't resolve types (e.g. a temp file with no module context),
   ggen falls back to AST-only mode and emits `encoding/json` for
   cross-package types. Slower but correct.
5. **Build under the right `GOEXPERIMENT`.** Files behind
   `goexperiment.jsonv2` are invisible without
   `GOEXPERIMENT=jsonv2 ggen ./...`.
6. **Test files are first-class inputs.** Annotated structs in
   `_test.go` files route to `_ggen_test.go` so the methods don't
   bundle into the library build.
7. **`hintlen` is the only safe prealloc hint.** Don't expect
   `maxlen` to size the container — it doesn't (intentionally,
   retained-heap reasons). Use `hintlen=N` when you know the
   typical size; use `len=N` when the count is fixed.

## Common user intents → flags

| User says                                                 | Reach for                                                                           |
| --------------------------------------------------------- | ----------------------------------------------------------------------------------- |
| "still want `json.Marshal(u)` to work"                    | `-marshal` (and/or `-unmarshal`)                                                    |
| "collect all errors, not just the first"                  | `-multierr`                                                                         |
| "skip unknown keys silently"                              | `-ignoreunknown` or a `json:",inline"` catch-all map                                |
| "fastest possible decode, I trust the input"              | `-novalidate`                                                                       |
| "wire output embedded directly in HTML"                   | `-htmlescape` (or per-type via alias `//ggen:generate htmlescape`)                  |
| "exact-precision numbers (big ints, no float64)"          | `-usenumber` for `any` fields; or use `math/big.Int`                                |
| "duplicate keys should be accepted (first wins)"          | `-allowdups`                                                                        |
| "keep field order matching declaration"                   | `-nosortkeys`                                                                       |
| "i want only some strings to have html escaping"          | `//ggen:generate htmlescape` `type HTMLString string`                               |
| "this struct has json tags but I want it to parse faster" | `//ggen:generate` `type Alias OtherStruct`                                          |
| "validate annotations in CI without writing files"        | `-dry` (parse + validate every annotated struct, surface every error, emit no file) |
