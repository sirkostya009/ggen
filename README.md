> [!CAUTION]
> Project is under active development. No release versions are available. Every new commit changes behaviour.

# ggen

zero-copy, zero-reflection JSON codegen for Go.

ggen parses structs and generates custom `DecodeFrom`, `DecodeFromStream`,
`JSONSize`, and `AppendJSON` methods. The decoder is a zero-copy byte scanner
with no token layer; the encoder pre-sizes appends into a single `[]byte`,
which you can presize yourself via the generated `JSONSize` method.

## benchmarks

Run over a ~5.6 MiB deep tree with full validation. See [mega_test.go](./bench/mega_test.go).

```sh
$ cd bench; go test -bench=BenchmarkMega -run=^$ -benchtime=500x -cpu=1 .
goos: linux
goarch: amd64
pkg: github.com/sirkostya009/ggen/bench
cpu: AMD RYZEN AI MAX+ 395 w/ Radeon 8060S
BenchmarkMega_Unmarshal/jsonv2         500   34726287 ns/op   168.88 MB/s   283 gc   17696991 B/op   316830 allocs/op
BenchmarkMega_Unmarshal/sonic          500   17790607 ns/op   329.65 MB/s   243 gc   20790020 B/op   137770 allocs/op
BenchmarkMega_Unmarshal/sonic_fast     500   17196652 ns/op   341.03 MB/s   241 gc   20790019 B/op   137770 allocs/op
BenchmarkMega_Unmarshal/easyjson       500   25844323 ns/op   226.92 MB/s   235 gc   16979473 B/op   245855 allocs/op
BenchmarkMega_Unmarshal/ggen           500   14185226 ns/op   413.43 MB/s   176 gc   11391569 B/op    64599 allocs/op
BenchmarkMega_Unmarshal/ggen_copy      500   17230444 ns/op   340.36 MB/s   213 gc   14087313 B/op   214407 allocs/op
BenchmarkMega_Marshal/jsonv2           500   15677143 ns/op   373.88 MB/s    86 gc    5976776 B/op     7407 allocs/op
BenchmarkMega_Marshal/sonic            500   13102979 ns/op   447.58 MB/s   496 gc   33621206 B/op     5112 allocs/op
BenchmarkMega_Marshal/sonic_fast       500   12607842 ns/op   465.16 MB/s   487 gc   33621071 B/op     5111 allocs/op
BenchmarkMega_Marshal/easyjson         500   11002786 ns/op   533.01 MB/s    96 gc    6116199 B/op     7586 allocs/op
BenchmarkMega_Marshal/ggen             500    9225406 ns/op   635.70 MB/s   167 gc   11894784 B/op        1 allocs/op
BenchmarkMega_Marshal/ggen_presized    500    6808990 ns/op   861.31 MB/s     0 gc          0 B/op        0 allocs/op
BenchmarkMega_Reader/jsonv2            500   35083440 ns/op   167.16 MB/s   284 gc   17697305 B/op   316830 allocs/op
BenchmarkMega_Reader/sonic             500   20048676 ns/op   292.52 MB/s   492 gc   38983453 B/op   137793 allocs/op
BenchmarkMega_Reader/sonic_fast        500   20148021 ns/op   291.08 MB/s   498 gc   38983457 B/op   137793 allocs/op
BenchmarkMega_Reader/easyjson          500   27880232 ns/op   210.35 MB/s   420 gc   31521405 B/op   245886 allocs/op
BenchmarkMega_Reader/ggen_stream       500   21151592 ns/op   277.27 MB/s   247 gc   17116509 B/op   251735 allocs/op
BenchmarkMega_Reader/ggen_readall      500   17636487 ns/op   332.53 MB/s   344 gc   25932124 B/op    64628 allocs/op
```

The fast decode numbers come from ggen's zero-copy strategy for strings and
`json.RawMessage`. Every time you pass a buffer to `DecodeFrom`, all strings
and raw byte spans are aliased into it — so **mutating the payload after decode
mutates the decoded struct too**. Be careful with that.

This has one side effect worth considering: if a decoded struct's strings are
held by something long-lived, the aliasing pins the whole backing buffer and
can balloon memory usage (Go's GC isn't compacting). Streams don't have this
problem — they always copy, reusing the underlying buffer.

If you need the bytes path to copy too — to reuse/mutate the input buffer right
after decoding, or to avoid pinning a large buffer — generate with `-copy` (or
`//ggen:generate copy` per type). Decoded strings, `json.RawMessage`, and
strings inside `any` fields are then copied out of the input instead of aliased,
giving the decoded value an independent lifetime (at the cost of more allocs).

### slow-network streaming

Another use case is slow or long network streams. Benchmarked against a faked
`io.Reader` simulating a connection warming up fast:

```sh
$ cd bench; go test -bench=BenchmarkSlowStream -run=^$ -benchtime=100x .
BenchmarkSlowStream_Valid/stdjson          100  164467955 ns/op   0.22 MB/s   119005 B/op   1995 allocs/op
BenchmarkSlowStream_Valid/sonic            100  141136378 ns/op   0.25 MB/s   126978 B/op    830 allocs/op
BenchmarkSlowStream_Valid/sonic_fast       100  141211472 ns/op   0.25 MB/s   125912 B/op    830 allocs/op
BenchmarkSlowStream_Valid/easyjson         100  158087304 ns/op   0.23 MB/s   187455 B/op   1542 allocs/op
BenchmarkSlowStream_Valid/ggen_stream      100  141295185 ns/op   0.25 MB/s    99309 B/op   1554 allocs/op
BenchmarkSlowStream_Valid/ggen_readall     100  158119251 ns/op   0.23 MB/s   155157 B/op    415 allocs/op
BenchmarkSlowStream_Invalid/ggen_stream    100   66512301 ns/op   0.04 MB/s     3186 B/op      4 allocs/op
BenchmarkSlowStream_Invalid/ggen_readall   100   77878279 ns/op   0.04 MB/s     7394 B/op     10 allocs/op
BenchmarkSlowStream_Invalid/jsonv2         100   82081722 ns/op   0.04 MB/s    16770 B/op     25 allocs/op
BenchmarkSlowStream_Invalid/sonic          100   66431747 ns/op   0.04 MB/s     3421 B/op      6 allocs/op
BenchmarkSlowStream_Invalid/sonic_fast     100   66364213 ns/op   0.04 MB/s     3421 B/op      6 allocs/op
```

The `Invalid` benchmarks exercise the fail-fast advantage — ggen validation
works over streams and rejects invalid payloads without reading the full
payload. See [slowstream_test.go](./bench/slowstream_test.go).

## usage

The primary use case for ggen is HTTP servers. The Stream implementation is a
bit lacking in memory performance, but it's still a faster strategy for slower
networks — especially when you get lots of invalid payloads that can be
discarded at parse time.

Install the CLI and pull in the runtime subpackages your generated code will
import:

```sh
go install github.com/sirkostya009/ggen/cli@latest # CLI binary
go get github.com/sirkostya009/ggen                 # runtime subpackages
```

Annotate a struct with `//ggen:generate` and run the CLI:

```go
package api

//ggen:generate
type User struct {
    ID    int      `json:"id"`
    Name  string   `json:"name"   pipe:"required minlen=1 maxlen=64"`
    Email string   `json:"email"  pipe:"trim lower contains=@"`
    Tags  []string `json:"tags,omitempty" pipe:"inner:notempty"`
}
```

```sh
ggen .               # current package
ggen ./...           # every package matched by the pattern — module-scoped, same as `go build ./...`
ggen ./pkg/...       # subtree pattern (relative paths must start with `./`)
ggen path/to/file.go # one file; optional struct-name filter as trailing args
```

The generated file lives next to the source. Output naming follows the input:
a package run gets `<dir>_ggen.go` (and `<dir>_ggen_test.go` for annotated
structs declared in `_test.go` files). A single file gets `<base>_ggen.go`. The
`-o` flag overrides the path for single-file or single-package mode.

For input files with a `//go:build` constraint, ggen carries that constraint
into a separate output file: a struct in `tagged.go` guarded by `//go:build
foo` ends up in `<dir>_foo_ggen.go` with the same `//go:build foo` header, so
unconstrained builds aren't broken by an "undefined: Tagged" reference.
Multi-term constraints (e.g. `//go:build foo && bar`) get a slugified filename
(`<dir>_foo_bar_ggen.go`) with the original expression preserved verbatim in
the header.

You can use ggen with `go generate` — one `//go:generate` directive per package
is enough, no need to put one above every annotated struct:

```go
//go:generate ggen .
```

Or, to scope generated output per file, put a `//go:generate ggen $GOFILE` line
in every file with annotated structs and let single-file mode handle naming:

```go
//go:generate ggen $GOFILE

// can of course override default naming as well:
//go:generate ggen $GOFILE -o file_ggen.go
```

### generated methods

For every annotated struct `T`:

```go
func (T) DecodeFrom(data []byte) (T, int, error)      // returns bytes consumed
func (T) DecodeFromStream(s *scan.Stream) (T, error)  // Stream owns cursor via s.Pos
func (T) JSONSize() int
func (T) AppendJSON(dst []byte) ([]byte, error)
```

With the `marshal` and `unmarshal` annotations set, respectively:

```go
func (T)  MarshalJSON() ([]byte, error)
func (*T) UnmarshalJSON([]byte) error
```

Use the generated methods directly on values or in place:

```go
// parse from []byte
u, _, err := User{}.DecodeFrom(payload)

// preallocate a []byte buf, then write to it
buf := make([]byte, 0, u.JSONSize())
buf, err = u.AppendJSON(buf)

// wrap an io.Reader in a stream with a preallocated intermediate buf
s := scan.NewStream(r, buf)
// lazily decode from stream
u, err = u.DecodeFromStream(s)
// once finished you can reuse buf for another stream
```

#### merge semantics

Like the stdlib, ggen has merge semantics, but deliberately different ones:

- Non-nil slices, maps, and pointer fields are **reused** — useful as an
  optimization on paths where you repeatedly parse the same object shape. Reuse
  reaches nested slices too: re-decoding a `[][]T` (any depth) reuses the inner
  rows' backing arrays, not just the outer slice.
- **All containers are reset regardless of presence** in the payload (the
  stdlib keeps an omitted container) — a blank payload gives a blank slate while
  keeping capacity.
- A JSON `null` aimed at a slice/map/pointer field nils it. A `null` aimed at a
  non-pointer scalar or struct is a **parse error by default** — use a pointer
  for a nullable scalar, or opt the field into stdlib-style null handling with a
  `nullzero` decode variant in its `pipe:` tag (decodes `null` to the Go zero
  value). `-nullzero` / `//ggen:generate nullzero` turns that on for every value
  field at once.

### runtime packages

Beyond the byte-scan and append primitives, the `encode` / `decode` packages
expose a small set of helpers for the patterns you'd actually use:

```go
import (
    "github.com/sirkostya009/ggen/decode"
    "github.com/sirkostya009/ggen/encode"
    "github.com/sirkostya009/ggen/scan"
)

// single value
out, err := encode.Marshal(u)

// JSON array of T → []T
users, err := decode.UnmarshalSlice[User](payload)
out, err := encode.MarshalSlice(users)

// streaming array
users, buf, err := decode.UnmarshalSliceStream[User](req.Body, buf[:0])
```

### flags and annotations

Most CLI flags have a matching per-struct annotation token (no leading dash).
Flags apply globally to the whole pass; annotations apply locally to a struct.
Multiple annotation tokens are space-separated: `//ggen:generate marshal
unmarshal multierr`.

| CLI flag         | struct annotation | effect                                                                                                                                                                                                                                                                                                                                                    |
| ---------------- | ----------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `-o <path>`      | —                 | override output path (single-file or single-package mode only)                                                                                                                                                                                                                                                                                            |
| `-pkg <name>`    | —                 | override the package name in the generated file                                                                                                                                                                                                                                                                                                           |
| `-marshal`       | `marshal`         | also emit a `MarshalJSON` hook so the type satisfies `encoding/json.Marshaler`                                                                                                                                                                                                                                                                            |
| `-unmarshal`     | `unmarshal`       | also emit an `UnmarshalJSON` hook for `encoding/json.Unmarshaler`                                                                                                                                                                                                                                                                                         |
| `-multierr`      | `multierr`        | accumulate every validation failure into `validation.Errors` instead of returning on the first one                                                                                                                                                                                                                                                        |
| `-allowdups`     | `allowdups`       | accept duplicate JSON keys with first-wins semantics — the first occurrence is parsed, later ones are skipped via `scan.SkipValue` without being decoded (default: error on the second hit)                                                                                                                                                               |
| `-novalidate`    | `novalidate`      | drop validation, required-field checks, and mods entirely — fastest decode path                                                                                                                                                                                                                                                                           |
| `-ignoreunknown` | `ignoreunknown`   | silently drop unknown JSON keys (default: error). overridden by an inline catch-all map field                                                                                                                                                                                                                                                             |
| `-nullzero`      | `nullzero`        | accept an explicit JSON `null` on every non-pointer value field, decoding it to the Go zero value (default: error). a per-field `nullzero` decode variant in `pipe:` opts in a single field                                                                                                                                                               |
| `-nosortkeys`    | `nosortkeys`      | emit struct fields in declaration order (default: alphabetical by JSON name, compresses better)                                                                                                                                                                                                                                                           |
| `-usenumber`     | `usenumber`       | decode numbers in `any` fields as `json.Number` instead of `float64`                                                                                                                                                                                                                                                                                      |
| `-htmlescape`    | `htmlescape`      | escape `<`, `>`, `&` to `\uXXXX` for safe embedding in HTML (default: literal, matches `encoding/json` v2 — v2 dropped HTML escaping as a default)                                                                                                                                                                                                        |
| `-copy`          | `copy`            | copy decoded strings and `json.RawMessage` (and strings inside `any` fields) out of the input buffer instead of aliasing it, so you may reuse or mutate the input after `DecodeFrom` returns (default: zero-copy aliasing — faster, but the input must stay alive and unmodified for as long as the decoded values are used). Decode-only; allocates more |
| `-dry`           | —                 | parse and validate every annotated struct, surface every error, emit no file. Useful in CI/pre-commit to fail fast on broken tags or annotations. Rejects `-o` / `-pkg`                                                                                                                                                                                   |

## struct tags

### `json:"..."`

The standard stdlib json/jsonv2 tags work as-is, including the field selection
rule: only exported fields are encoded and decoded. Unexported fields are
skipped silently (no decode wiring, never appear in marshal output) — same as
`encoding/json`. Extras worth knowing:

- `json:",inline"` — the field becomes a catch-all map for unknown keys. The Go
  type must be a string-keyed map (`map[string]V`); V can be `any`, a primitive,
  a ggen-annotated struct, or any other concrete type. Typed values are decoded
  directly through the elem's fast path when one exists (string scan, generated
  `DecodeFrom`), or via `encoding/json` unmarshal of the captured span
  otherwise. Overrides `-ignoreunknown`; on marshal the entries are spliced into
  the parent object.
- `format:X` — type-specific format hint (see _supported kinds_ below). Per
  jsonv2, this MUST be the last option in the tag.

### `pipe:"..."` — decode, transform, validate

The `pipe:` tag is one ordered pipeline: presence, an optional decode stage
(which JSON shapes the field accepts), then value steps (mods and validators)
that run **in the order you write them**.

```go
Name  string `json:"name"  pipe:"required trim minlen=1 maxlen=50"`
Email string `json:"email" pipe:"trim lower contains=@"`
Tags  []string `json:"tags" pipe:"optional inner:notempty minlen=1"`
```

Steps are whitespace-separated. A value or message containing spaces is
single-quoted (`contains='foo bar'`). The intra-rule arg separator is `|`
(`oneof=a|b|c`, `clamp=0|100`).

#### presence — `required` / `optional`

`required` asserts the JSON key is present (checked at object close →
`RequiredError`); `optional` is an explicit "may be absent" marker. They are
position-independent (write them first by convention). Presence is separate from
the value: a `required` field whose value is `null` still errors unless you also
accept `null` (see `nullzero` below). An absent key leaves the Go zero value.

#### decode stage — JSON-shape variants (`/`)

By default a field decodes from its type's natural JSON shape. To accept more
than one shape, list `/`-separated variants; ggen peeks the incoming shape and
routes to the matching variant (one per JSON shape):

- `.` — native decode of the field type (the plain value).
- `nullzero` — accept JSON `null`, producing the Go zero value. This is how a
  non-pointer value field opts into `null`; bare `nullzero` needs no `.`.
- `@Conv` — a converter `func(W) T` / `func(W) (T, error)` / `func(W) (T, bool)`.
  ggen scans the input type `W` natively (W decides the shape this variant
  claims), then calls your func. `W` may be a primitive **or a ggen-decodable
  struct** — ggen builds it with its own decoder, then converts.

```go
// number natively, OR a string-encoded number; then range-checked
Age   int `json:"age"   pipe:". / @AtoiStrict gte=0 lte=150"`   // AtoiStrict(string)(int,error)
// number natively, OR a {amount,...} object via the Money decoder
Price int `json:"price" pipe:". / @FromMoney gte=0"`            // FromMoney(Money) int
// null→0, a number, or a string
Opt   int `json:"opt"   pipe:"nullzero / . / @AtoiStrict"`
```

A converter needs a signal — `/`, a leading `.` (`./@Conv`), or it's read as a
value step (below). Variants must claim distinct shapes (one object-rooted, one
array-rooted, etc.) or codegen errors. Encode is unaffected: the field always
marshals as its native type.

#### value steps — validators + mods, in order

Everything after the decode stage operates on the value. **Mods (transforms) and
validators run in declared order**, so `lte=10 @Double` validates the raw value
before doubling it, while `@Double lte=10` doubles first.

| validators                                                    | error                                          | checks                                         |
| ------------------------------------------------------------- | ---------------------------------------------- | ---------------------------------------------- |
| `notempty`                                                    | `NotEmptyError`                                | string non-empty / slice / map non-zero length |
| `len=N`, `minlen=N`, `maxlen=N`                               | `LenError`, `MinLenError`, `MaxLenError`       | byte-length / element-count bounds             |
| `runes=N`, `minrunes=N`, `maxrunes=N`                         | `RunesError`, `MinRunesError`, `MaxRunesError` | rune-count bounds (utf8 aware)                 |
| `gt=N`, `gte=N`, `lt=N`, `lte=N`                              | `GTError`, `GTEError`, `LTError`, `LTEError`   | numeric comparison                             |
| `eq=X`, `neq=X`                                               | `EqError`, `NeqError`                          | equality (numeric or string operand)           |
| `multiple=N`                                                  | `MultipleError`                                | numeric — multiple of N                        |
| `oneof=a\|b\|c`                                               | `OneOfError`                                   | one of the listed alternatives                 |
| `url`, `alphanum`, `numeric`, `lower`, `upper`, `hexadecimal` | `URLError`, `AlphanumError`, …                 | string-shape predicates                        |
| `starts=X`, `ends=X`, `contains=X`                            | `StartsError`, `EndsError`, `ContainsError`    | substring tests on strings                     |

| mods                                                                      | target  |
| ------------------------------------------------------------------------- | ------- |
| `trim`, `lower`, `upper`, `trimleft=X`, `trimright=X`, `replace=old\|new` | string  |
| `clamp=lo\|hi` (either side may be empty: `clamp=0\|`, `clamp=\|100`)     | numeric |

#### `inner:` / `keys:` — container levels

`inner:` scopes steps to one container level down (per element of a slice/array,
per value of a map); `keys:` scopes to map keys. A bare prefix takes exactly one
step (`inner:trim`); group several in parentheses (`inner:(trim maxlen=20)`);
nest the groups to go deeper (`inner:(minlen=1 inner:(gte=0 lte=100))`). Steps
written outside any group apply to the whole container.

```go
Tags []string `json:"tags" pipe:"inner:(trim maxlen=20) maxlen=100"`
//                                 per-element trim+bound | whole-slice cap
Lookup map[string]int `json:"lookup" pipe:"keys:minrunes=2 inner:gte=0"`
```

#### custom funcs — `@FuncName` / `@pkg.FuncName`

ggen looks up the function at codegen and classifies it by signature — no
runtime registry, no `any` boxing, and the Go compiler catches mismatches:

| signature            | role                                                             |
| -------------------- | ---------------------------------------------------------------- |
| `func(T) error`      | validator → `CustomError{Name, Value, Cause}`                    |
| `func(T) bool`       | validator → `PredicateError` (false = fail; message-capable)     |
| `func(T) T`          | mod (pure transform)                                             |
| `func(T) (T, error)` | mod (fallible; non-nil error → parse error, even under multierr) |
| `func(T) (T, bool)`  | mod (fallible; false → `ModError` parse error; message-capable)  |
| `func(W) T` (W ≠ T)  | converter (decode-stage variant only — see above)                |

`func(bool) bool` is rejected (ambiguous — use `func(bool) error`). The bool
forms take an inline message: `@MustBeEven:'value must be even'`.

```go
//ggen:generate
type Box struct {
    N int `json:"n" pipe:"@EvenOnly"`        // func(int) error → validator
    M int `json:"m" pipe:"@MustBeEven:'must be even'"` // func(int) bool
}
func EvenOnly(n int) error { if n%2 != 0 { return errors.New("must be even") }; return nil }
func MustBeEven(n int) bool { return n%2 == 0 }
```

Cross-package references (`@pkg.FuncName`) resolve through the source file's
import block — file-scoped aliases and blank imports (`_ "path"`) both work.

### `hint:"..."` — preallocation

`hint:"N"` preallocates a slice/map with cap N (`make([]T, 0, N)`), overriding
the default cap of 4 and any `minlen`-derived cap. Per-level via `inner:`:
`hint:"32 inner:8"` sizes the outer cap 32, inner rows cap 8. `hint:"0"`
disables preallocation; a negative hint is a generate-time error.

### inspecting errors

```go
var minlen *validation.MinLenError
if errors.As(err, &minlen) {
    // minlen.Path  — root-relative path segments, e.g. ["addr", "zip"]
    // minlen.Limit, minlen.Got
    // minlen.Pos   — byte offset of the failure, relative to the full payload
}
```

Every validation error carries a `Pos int` — the byte offset of the failure
relative to the full payload, the same on the bytes and stream paths. (On the
stream path it accounts for buffer compaction, so it stays meaningful across a
long stream rather than tracking the sliding `scan.Stream.Pos` window.)

> In `-multierr` mode the generated code returns `validation.Errors`
> (`[]validation.Error`) instead of stopping on the first failure. It implements
> `Unwrap() []error` so `errors.Is` / `errors.As` walk every accumulated error.

Low-level parse failures (malformed JSON, wrong primitive type) come back
wrapped in `*decode.ParseError`, which carries the JSON field path the decoder
was working on and a byte offset:

```go
var pe *decode.ParseError
if errors.As(err, &pe) {
    // pe.Field — dotted JSON path, e.g. "addr.street"
    // pe.Pos   — byte offset within the data slice passed to DecodeFrom
    // pe.Err   — underlying sentinel (scan.ErrBadString, scan.ErrBadObject, …)
}

// The wrap is transparent to errors.Is — the underlying scan sentinel
// is still reachable:
if errors.Is(err, scan.ErrBadString) { ... }
```

Validation errors are NOT wrapped: their typed pointers (`*validation.MinLenError`
etc.) remain directly reachable via `errors.As`. `ParseError.Error()` only
prints the `parse error at <field> (pos <n>)` prefix — call `errors.Unwrap` to
get the underlying message.

## supported kinds

| category  | go types                                                         | wire   | notes                                                                                            |
| --------- | ---------------------------------------------------------------- | ------ | ------------------------------------------------------------------------------------------------ |
| primitive | `string`, `bool`, `int*`, `uint*`, `float*`                      | scalar | `*T` for any of these — `null` ↔ `nil`; multi-level `**T`/… also native                          |
| slice     | `[]T`                                                            | array  | nil → `null`; `[]*T` decodes into a single contiguous slab (N allocs → ~log N); `[]**T`/… native |
| array     | `[N]T`                                                           | tuple  | strict element count — mismatch → `validation.LenError`; `[N]*T` uses a fixed slab               |
| map       | `map[string]V`                                                   | object | string keys only; `map[string]*V` / `**V` / … values decode natively, `null` ↔ `nil`             |
| struct    | named / embedded                                                 | object | embedded fields are promoted, same as `encoding/json`                                            |
| cross-pkg | foreign struct / named type                                      | varies | static method-set probe at codegen — see _cross-package interfaces_ below                        |
| alias     | `//ggen:generate type X ...` (see [type aliases](#type-aliases)) | varies | full method surface generated; strategy picked from the underlying type                          |

### cross-package interfaces

For any field whose type is defined outside the package being generated, ggen
probes the method set at codegen time and emits a direct call — no runtime
probing. The first method available in order is picked:

| direction | ladder                                                                                |
| --------- | ------------------------------------------------------------------------------------- |
| decode    | `DecodeFrom` → `UnmarshalJSON` → `UnmarshalText` → `encoding/json.Unmarshal`          |
| encode    | `AppendJSON` → `MarshalJSON` → `AppendText` → `MarshalText` → `encoding/json.Marshal` |

Types like `google/uuid` implement `TextMarshaler`/`TextUnmarshaler`, so the
ladder routes them through the text path automatically. This means that even
though a type might have JSON marshalling methods, its wire shape is still a JSON
string when serialized via ggen methods. `AppendText` is preferred over
`MarshalText`.

`encoding/json.Unmarshal` and `json.Marshal` are used only as a fallback when
none of the above methods are present on the custom type.

### type aliases

`//ggen:generate` on a named top-level type generates the full method surface
(`DecodeFrom`, `DecodeFromStream`, `JSONSize`, `AppendJSON`), so the alias is
decoded like any other ggen type. The codegen strategy is picked automatically
from the underlying type's shape and method set:

| flavor                           | example                 | strategy                                                                              |
| -------------------------------- | ----------------------- | ------------------------------------------------------------------------------------- |
| primitive                        | `type Count int`        | scan + cast; `htmlescape`/`marshal`/`unmarshal` annotations still apply               |
| struct (exported fields)         | `type Comment Inner`    | field introspection — treats the alias like a regular struct                          |
| struct (has `DecodeFrom`)        | `type X HasGgenMethods` | cast & delegate to the underlying's existing ggen methods                             |
| struct (opaque + Marshaler/Text) | `type Local time.Time`  | delegate to underlying's `MarshalJSON`/`MarshalText` (Go 1.24 `AppendText` preferred) |
| container                        | `type Tags []string`    | same emitters as slice/map/array fields — all field-level features apply              |

Aliases of channels, interfaces, and functions are rejected at generate time (no
sensible JSON shape for those).

> [!TIP]
> Pairing a primitive alias with `htmlescape` is a cheap way to split
> HTML-escaped strings from plain ones at the type level: tag only the fields
> whose values get embedded into HTML as `HtmlString`, leave the rest as plain
> `string`, and the literal fast path stays on for the bulk of your payload while
> escaping runs only where it matters.

```go
//ggen:generate htmlescape
type HtmlString string

//ggen:generate
type UserID int64

//ggen:generate
type Tags []string

//ggen:generate
type LocalUUID uuid.UUID  // delegates to uuid.UUID's TextMarshaler

//ggen:generate
type Comment struct {
    ID     UserID     `json:"id"     pipe:"gte=1"`                           // numeric alias, no quoting; gte runs against int
    Author string     `json:"author" pipe:"required trim minlen=1"`         // plain string, fast path
    Body   HtmlString `json:"body"   pipe:"required trim lower maxlen=4096"` // \uXXXX-escaped via the alias; mods cast through string
    Tags   Tags       `json:"tags"   pipe:"inner:notempty"`                 // inner: runs against each element
}
```

Each field's wire shape is decided by its type, not its tag — so the escaping
cost only lands on `Body`, not on `Author`.

### stdlib types

ggen treats a number of stdlib types as first-class, with special encoding and
decoding rules:

| type                   | wire                | format hints                                                                                                  |
| ---------------------- | ------------------- | ------------------------------------------------------------------------------------------------------------- |
| `time.Time`            | RFC3339Nano string  | `format:unix`, `unixmilli`, `unixmicro`, `unixnano`, `RFC3339`, custom layout `format:'2006-01-02'`           |
| `time.Duration`        | string `"1h30m"`    | `format:sec`, `milli`, `micro`, `nano`, `units` (default)                                                     |
| `[]byte`               | base64 string       | `format:base64` (default), `base64url`, `base32`, `base32hex`, `base16`/`hex`, `array`                        |
| `net.IP`               | text                | —                                                                                                             |
| `netip.Addr`           | text                | —                                                                                                             |
| `netip.Prefix`         | text                | —                                                                                                             |
| `json.RawMessage`      | passthrough         | zero-copy alias on decode                                                                                     |
| `jsontext.Value`       | passthrough         | zero-copy alias on decode                                                                                     |
| `net/url.URL`          | string              | `url.Parse` on decode, `String()` on encode                                                                   |
| `math/big.Int`         | JSON number         | arbitrary precision                                                                                           |
| `math/big.Float`       | JSON string         | arbitrary precision (matches jsonv2)                                                                          |
| `math/big.Rat`         | JSON string         | `"22/7"`                                                                                                      |
| `database/sql.NullX`   | inner value or null | `NullString`, `NullInt64`/`32`/`16`, `NullByte`, `NullBool`, `NullFloat64`, `NullTime`                        |
| `database/sql.Null[T]` | inner value or null | generic form (Go 1.22); any `T` ggen handles as a field (primitive, `time.Time`, `uuid.UUID`, named types, …) |

`any` also works, similar to how the standard json treats it.

#### divergences from stdlib

The `net/url.URL`, `sql.NullX`, and `sql.Null[T]` rows above ship a different
wire shape from `encoding/json` v1/v2 — ggen serializes them the way consumers
usually expect, diverging from stdlib's exported-field struct dump:

| type          | ggen wire             | stdlib wire (v1 + v2)                          |
| ------------- | --------------------- | ---------------------------------------------- |
| `net/url.URL` | `"https://x/p?q=1"`   | `{"Scheme":"https","Host":"x", ... 11 fields}` |
| `sql.NullX`   | inner value or `null` | `{"<Inner>":val,"Valid":true}`                 |

## examples

### streaming

The `DecodeFromStream` paths are meant for cases where you expect invalid
payloads and don't want to waste time parsing them in full, as well as for
potentially slow network streams.

```go
//ggen:generate
type CreateUser struct {
    Email string `json:"email" pipe:"required contains=@"`
    Bio   string `json:"bio"   pipe:"maxlen=4096"`
}

var bufPool = sync.Pool{New: func() any {
	return scan.NewStream(nil, make([]byte, 0, 512))
}}

func parseRequest[T decode.Decoder[T]](r *http.Request) (T, error) {
	var zero T
	s := bufPool.Get().(*scan.Stream)
	// grow the buf to match incoming content length (if available)
	b := slices.Grow(s.Bytes(), max(int(r.ContentLength), 0))
	// limit actual reader
	s.Reset(io.LimitReader(r.Body, 10<<20), b)
	// recycle the buf with stream
	defer bufPool.Put(s)
	return zero.DecodeFromStream(s)
}

func handler(w http.ResponseWriter, r *http.Request) {
	u, err := parseRequest[CreateUser](r)
	if err != nil {
		// build a proper error message and send it to the client
		parseFailure(w, err)
		return
	}
}
```

### nested validation

```go
//ggen:generate
type Order struct {
    Items []Item `json:"items" pipe:"required minlen=1 maxlen=100 inner:required"`
}

//ggen:generate
type Item struct {
    SKU string `json:"sku" pipe:"required len=12 alphanum upper"`
    Qty int    `json:"qty" pipe:"required gte=1 lte=999"`
}
```

### catch-all unknown keys

```go
//ggen:generate
type Event struct {
    Type string         `json:"type"`
    Data map[string]any `json:",inline"` // absorbs every unknown key
}
```

## colophon

this whole project was vibe coded with claude opus 4.7. every line of
the generator, the runtime libraries, the tests, the fuzzers — typed
by the model, steered by me. i didnt really care about code of the
generator, instead im laser focused on quality of the _generated_ code.

## license

[MIT](LICENSE).
