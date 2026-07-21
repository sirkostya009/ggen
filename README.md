> [!CAUTION]
> Project is under active development. No release versions are available. Every new commit changes behaviour.

# ggen

zero-copy, zero-reflection JSON codegen for Go.

ggen parses structs and generates custom `DecodeFrom`, `DecodeFromStream`,
`JSONSize`, and `AppendJSON` methods. The decoder is a zero-copy byte scanner
with no token layer; the encoder pre-sizes appends into a single `[]byte`,
which you can presize yourself via the generated `JSONSize` method.

## benchmarks

Run over a ~4.4 MiB deep tree with full validation. See [mega_test.go](./bench/mega_test.go).

```sh
$ cd bench; go test -bench=BenchmarkMega -run=^$ -benchtime=500x -cpu=1 .
goos: linux
goarch: amd64
pkg: github.com/sirkostya009/ggen/bench
cpu: AMD RYZEN AI MAX+ 395 w/ Radeon 8060S
BenchmarkMega_Unmarshal/jsonv2         500   25140128 ns/op   185.38 MB/s   155 gc   13556011 B/op   234397 allocs/op
BenchmarkMega_Unmarshal/sonic          500   12062786 ns/op   386.35 MB/s   150 gc   16619826 B/op   127484 allocs/op
BenchmarkMega_Unmarshal/sonic_fast     500   12003873 ns/op   388.24 MB/s   149 gc   16619826 B/op   127484 allocs/op
BenchmarkMega_Unmarshal/easyjson       500   18557929 ns/op   251.13 MB/s   147 gc   12853609 B/op   164283 allocs/op
BenchmarkMega_Unmarshal/ggen           500   10238159 ns/op   455.20 MB/s    85 gc    8434184 B/op    54990 allocs/op
BenchmarkMega_Unmarshal/ggen_copy      500   11902418 ns/op   391.55 MB/s    98 gc    9979393 B/op   132894 allocs/op
BenchmarkMega_Marshal/jsonv2           500   11129385 ns/op   418.46 MB/s    50 gc    4774166 B/op     7497 allocs/op
BenchmarkMega_Marshal/sonic            500    8342389 ns/op   558.65 MB/s   223 gc   26870597 B/op     5107 allocs/op
BenchmarkMega_Marshal/sonic_fast       500    7731154 ns/op   602.81 MB/s   225 gc   26870542 B/op     5106 allocs/op
BenchmarkMega_Marshal/easyjson         500    7667069 ns/op   607.85 MB/s    53 gc    4876654 B/op     6914 allocs/op
BenchmarkMega_Marshal/ggen             500    5984014 ns/op   778.81 MB/s    97 gc    9691136 B/op        1 allocs/op
BenchmarkMega_Marshal/ggen_presized    500    4910092 ns/op   949.15 MB/s     0 gc          0 B/op        0 allocs/op
BenchmarkMega_Reader/jsonv2            500   25774731 ns/op   180.81 MB/s   158 gc   13556022 B/op   234397 allocs/op
BenchmarkMega_Reader/sonic             500   15905907 ns/op   293.00 MB/s   276 gc   34812950 B/op   127506 allocs/op
BenchmarkMega_Reader/sonic_fast        500   15724834 ns/op   296.37 MB/s   276 gc   34812950 B/op   127506 allocs/op
BenchmarkMega_Reader/easyjson          500   20135747 ns/op   231.45 MB/s   249 gc   23314832 B/op   164313 allocs/op
BenchmarkMega_Reader/ggen_stream       500   15054303 ns/op   309.57 MB/s   125 gc   12974142 B/op   169760 allocs/op
BenchmarkMega_Reader/ggen_readall      500   12134934 ns/op   384.05 MB/s   162 gc   18895123 B/op    55018 allocs/op
```

### simd

Code generated with `-simd=avx512` (see the flag table below) against sonic.

```sh
BenchmarkMega_Unmarshal/sonic          500     11936692 ns/op    390.43 MB/s   148 gc   16619825 B/op   127484 allocs/op
BenchmarkMega_Unmarshal/sonic_fast     500     11919286 ns/op    391.00 MB/s   148 gc   16619826 B/op   127484 allocs/op
BenchmarkMega_Unmarshal/ggen           500      9310070 ns/op    500.58 MB/s    77 gc    8434184 B/op    54990 allocs/op
BenchmarkMega_Unmarshal/ggen_copy      500     11599721 ns/op    401.77 MB/s    96 gc    9979393 B/op   132894 allocs/op
BenchmarkNoAlloc_Unmarshal/sonic       500         2478 ns/op   1108.77 MB/s     0 gc       3744 B/op        3 allocs/op
BenchmarkNoAlloc_Unmarshal/sonic_fast  500         2332 ns/op   1178.59 MB/s     0 gc       3744 B/op        3 allocs/op
BenchmarkNoAlloc_Unmarshal/ggen        500         1078 ns/op   2549.44 MB/s     0 gc          0 B/op        0 allocs/op
BenchmarkNoAlloc_Unmarshal/ggen_copy   500         1579 ns/op   1740.87 MB/s     0 gc       1864 B/op       25 allocs/op
BenchmarkSmall_Unmarshal/sonic         500        414.5 ns/op   7001.92 MB/s     0 gc       3280 B/op        5 allocs/op
BenchmarkSmall_Unmarshal/sonic_fast    500        388.7 ns/op   7465.72 MB/s     0 gc       3280 B/op        5 allocs/op
BenchmarkSmall_Unmarshal/ggen          500        131.2 ns/op  22121.26 MB/s     0 gc         64 B/op        1 allocs/op
BenchmarkSmall_Unmarshal/ggen_copy     500        264.9 ns/op  10955.16 MB/s     0 gc       3192 B/op        8 allocs/op
BenchmarkTiny_Unmarshal/sonic          500        322.4 ns/op    397.04 MB/s     0 gc        256 B/op        3 allocs/op
BenchmarkTiny_Unmarshal/sonic_fast     500        306.9 ns/op    417.08 MB/s     0 gc        256 B/op        3 allocs/op
BenchmarkTiny_Unmarshal/ggen           500        69.95 ns/op   1829.88 MB/s     0 gc          0 B/op        0 allocs/op
BenchmarkTiny_Unmarshal/ggen_copy      500        125.6 ns/op   1018.98 MB/s     0 gc         56 B/op        4 allocs/op
```

ggen decodes 2.2× faster than sonic's fastest config on the flat record and
4.4× on the tiny object — at zero allocations. When copying payload instead
of aliasing (`ggen_copy` benchmarks) it still stays ahead of sonic by 1.5×
on the flat record and 2.4× on the tiny object.
Small benefits most: the benchmark is dominated by one long string, decoded at
22 GB/s, ~19× sonic.

So ggen is still faster than sonic while also pertaining most of stdlib's
decoding safety features like unicode and grammar validation.

## install

Install the CLI and pull in the runtime subpackages your generated code will
import:

```sh
go install github.com/sirkostya009/ggen/cli@latest # CLI binary
go get github.com/sirkostya009/ggen                # runtime subpackages
```

## usage

The primary use case for ggen is HTTP servers. The Stream implementation is the
preferred interface for decoding, it was designed to be used with reusable buffers -
something you can pool and recycle to benefit from an even lower memory usage.

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

| pattern                            | output                  | notes                                                  |
| ---------------------------------- | ----------------------- | ------------------------------------------------------ |
| `ggen .` / `ggen ./...`            | `<dir>_ggen.go`         | annotated structs in `_test.go` → `<dir>_ggen_test.go` |
| `ggen path/to/file.go`             | `<base>_ggen.go`        | `-o` overrides (single-file or single-package only)    |
| source has `//go:build foo`        | `<dir>_foo_ggen.go`     | same `//go:build foo` header carried over              |
| source has `//go:build foo && bar` | `<dir>_foo_bar_ggen.go` | slugified name, original expression kept verbatim      |

Build-constrained structs go to their own file so unconstrained builds aren't
broken by an "undefined: Tagged" reference.

You can use leave a single `//go:generate ggen .` directive per package so that
ggen runs on `go generate ./...`:

Or, to scope generated output per file, put a `//go:generate ggen $GOFILE` line
in every file with annotated structs and let single-file mode handle naming:

> [!NOTE] Using flags is possible too: `//go:generate ggen $GOFILE -o name.go`

### generated methods

For every annotated struct `T`:

```go
func (T) DecodeFrom(data []byte) (T, int, error)      // returns bytes consumed
func (T) DecodeFromStream(s *scan.Stream) (T, error)  // Stream owns cursor via s.Pos
func (T) JSONSize() int
func (T) AppendJSON(dst []byte) ([]byte, error)
```

`marshal` and `unmarshal` annotations generate methods that make struct implement
`json.Marshaler` and `json.Unmarshaler` respectively:

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

Like stdlib, ggen has merge semantics, but deliberately different ones:

| case                                      | ggen                 | stdlib               | notes                                                                                     |
| ----------------------------------------- | -------------------- | -------------------- | ----------------------------------------------------------------------------------------- |
| non-nil slice / map / pointer field       | reused               | reused               | reuse reaches nested slices — `[][]T` at any depth reuses the inner rows' arrays          |
| key omitted from payload, container field | reset, capacity kept | container kept as-is | a blank payload gives a blank slate                                                       |
| `null` → slice / map / pointer            | nil'd                | nil'd                | —                                                                                         |
| `null` → non-pointer scalar or struct     | parse error          | Go zero value        | use a pointer, a per-field `nullzero` decode variant, or `-nullzero` for all value fields |

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

Most CLI flags have a matching per-struct annotation token without a leading dash.
Flags apply globally to the whole pass; annotations apply locally to a struct.
Multiple annotation tokens are space-separated: `//ggen:generate marshal
unmarshal multierr`.

| CLI flag            | struct annotation  | effect                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| ------------------- | ------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `-o <path>`         | —                  | override output path (single-file or single-package mode only)                                                                                                                                                                                                                                                                                                                                                                                                       |
| `-pkg <name>`       | —                  | override the package name in the generated file                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `-marshal`          | `marshal`          | also emit a `MarshalJSON` hook so the type satisfies `encoding/json.Marshaler`                                                                                                                                                                                                                                                                                                                                                                                       |
| `-unmarshal`        | `unmarshal`        | also emit an `UnmarshalJSON` hook for `encoding/json.Unmarshaler`                                                                                                                                                                                                                                                                                                                                                                                                    |
| `-multierr`         | `multierr`         | accumulate every validation failure into `validation.Errors` instead of returning on the first one                                                                                                                                                                                                                                                                                                                                                                   |
| `-allowdups`        | `allowdups`        | accept duplicate JSON keys with first-wins semantics — the first occurrence is parsed, later ones are skipped via `scan.SkipValue` without being decoded (default: error on the second hit)                                                                                                                                                                                                                                                                          |
| `-novalidate`       | `novalidate`       | drop validation, required-field checks, and mods entirely — fastest decode path                                                                                                                                                                                                                                                                                                                                                                                      |
| `-ignoreunknown`    | `ignoreunknown`    | silently drop unknown JSON keys (default: error). overridden by an inline catch-all map field                                                                                                                                                                                                                                                                                                                                                                        |
| `-nullzero`         | `nullzero`         | accept an explicit JSON `null` on every non-pointer value field, decoding it to the Go zero value (default: error). a per-field `nullzero` decode variant in `pipe:` opts in a single field                                                                                                                                                                                                                                                                          |
| `-nosortkeys`       | `nosortkeys`       | emit struct fields in declaration order (default: alphabetical by JSON name, compresses better)                                                                                                                                                                                                                                                                                                                                                                      |
| `-usenumber`        | `usenumber`        | decode numbers in `any` fields as `json.Number` instead of `float64`                                                                                                                                                                                                                                                                                                                                                                                                 |
| `-htmlescape`       | `htmlescape`       | escape `<`, `>`, `&` to `\uXXXX` for safe embedding in HTML (default: literal, matches `encoding/json` v2 — v2 dropped HTML escaping as a default)                                                                                                                                                                                                                                                                                                                   |
| `-allowinvalidutf8` | `allowinvalidutf8` | skip decode-side UTF-8 validation for this struct: invalid bytes flow into string fields / keys / raw spans untouched, unpaired `\uXXXX` surrogates decode to U+FFFD (default: reject with `scan.ErrInvalidUTF8`, jsonv2 parity). Grammar checks unaffected. Decode-only                                                                                                                                                                                             |
| `-copy`             | `copy`             | copy decoded strings and `json.RawMessage` (and strings inside `any` fields) out of the input buffer instead of aliasing it, so you may reuse or mutate the input after `DecodeFrom` returns (default: zero-copy aliasing — faster, but the input must stay alive and unmodified for as long as the decoded values are used). Decode-only; allocates more                                                                                                            |
| `-dry`              | —                  | parse and validate every annotated struct, surface every error, emit no file. Useful in CI/pre-commit to fail fast on broken tags or annotations. Rejects `-o` / `-pkg`                                                                                                                                                                                                                                                                                              |
| `-simd <tier>`      | —                  | SIMD tier for string scans and marshal escape scans: `off`, `avx`, `avx2`, `avx512`. Running ggen under `GOEXPERIMENT=simd` auto-selects `avx`; `avx2`/`avx512` are explicit opt-ins (and require the env var). The tier is baked into the generated code — no runtime CPU probing or branching — so generated code `GOEXPERIMENT=simd` to build and a CPU with that instruction set to run. You can get from 1% to 90% performance improvement depending on payload |

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
Name  string   `json:"name"  pipe:"required trim notempty maxlen=50"`
Email string   `json:"email" pipe:"trim lower contains=@"`
Auth  []string `json:"tags"  pipe:"optional inner:starts='Bearer ' minlen=1"`
Num   int      `json:"age"   pipe:"required gte=10 clamp=10|100"`
```

#### presence — `required` / `optional`

`required` asserts the JSON key is present, `optional` is an explicit
"may be absent" marker. They are position-independent
but prefer writing them first by convention. Presence is separate from the
value: a `required` field whose value is `null` still errors unless you also
accept `null`. An absent key leaves the field untouched — zero value or cleared
merge target.

#### multiple JSON shapes

ggen supports accepting different JSON shapes into a single target field. To do
so just type your pipeline with a slash separated list of functions. ggen is going
to automatically infer accepted JSON values based on function's input parameter,
and will try to use those should it encounter the appropriate value.

Separate multiple with a slash /:

- `.` — native decode of the field type (the plain value).
- `nullzero` — accept JSON `null`, producing the Go zero value. This is how a
  non-pointer value field opts into `null`. Bare `nullzero` needs no `.`.
- `@Conv` — any converter of signature `func(W) T` / `func(W) (T, error)` /
  `func(W) (T, bool)`. `W` may be a primitive **or a ggen-decodable struct**.

```go
// number natively, OR a string-encoded number; then range-checked
Age   int `json:"age"   pipe:". / @AtoiStrict gte=0 lte=150"`   // AtoiStrict(string)(int,error)
// number natively, OR a {amount,...} object via the Money decoder
Price int `json:"price" pipe:". / @FromMoney gte=0"`            // FromMoney(Money) int
// null→0, a number, or a string
Opt   int `json:"opt"   pipe:"nullzero / . / @AtoiStrict"`
```

You'll need to split the converter from the rest of pipe by either using
multiple of them separate by a `/` or just one closed with a `~`.

#### pipeline

Everything after the decode stage operates on field value. One thing to note is
**pipeline runs in declared order**, so `gte=5 clamp=0|10` rejects anything below
5 before clamping, while `clamp=5|10 gte=5` clamps first — and then never fails,
since the clamped value is always in range.

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

#### `inner:` / `keys:`

`inner:` scopes steps to one container level down. `keys:` scopes to map keys.
A bare prefix takes exactly one step: `inner:trim`. Group several in parentheses:
`inner:(trim maxlen=20)`. Nest the groups to go deeper: `inner:(minlen=1 inner:(gte=0 lte=100))`.
All that isn't paired with `inner:` applies to struct field container value directly.

```go
Tags []string `json:"tags" pipe:"inner:(trim maxlen=20) maxlen=100"`
//                               per-element trim+bound then whole-slice cap
Lookup map[string]int `json:"lookup" pipe:"keys:minrunes=2 inner:gte=0"`
```

#### custom funcs

You can use custom validators and custom transformers with ggen. They're introspected
at codegen time and are invoked directly from within the generated methods.

| signature            | role                                                             |
| -------------------- | ---------------------------------------------------------------- |
| `func(T) error`      | validator → `CustomError{Name, Value, Cause}`                    |
| `func(T) bool`       | validator → `PredicateError` (false = fail; message-capable)     |
| `func(T) T`          | mod (pure transform)                                             |
| `func(T) (T, error)` | mod (fallible; non-nil error → parse error, even under multierr) |
| `func(T) (T, bool)`  | mod (fallible; false → `ModError` parse error; message-capable)  |
| `func(W) T` (W ≠ T)  | converter (decode-stage variant only — see above)                |

`func(bool) bool` is rejected (use `func(bool) error` instead).

The bool forms can additionally take an inline message: `@MustBeEven:'value must be even'`.

```go
//ggen:generate
type Box struct {
	N int `json:"n" pipe:"@EvenOnly"`                  // func(int) error → validator
	M int `json:"m" pipe:"@MustBeEven:'must be even'"` // func(int) bool
}
func EvenOnly(n int) error { if n%2 != 0 { return errors.New("must be even") }; return nil }
func MustBeEven(n int) bool { return n%2 == 0 }
```

Cross-package references like `@pkg.FuncName` resolve through the source file's
import block — file-scoped aliases and blank imports (`_ "path"`) both work.

### `hint:"..."`

`hint:"N"` tells ggen to preallocate the tagret slice or map with N capacity,
overriding default 4 or minlen-derived preallocation hint. Can pair with `inner`
mechanics described above. Setting N to 0 disabled preallocation completely.
Negative values are not allowed.

### inspecting errors

```go
var minlen *validation.MinLenError
if errors.As(err, &minlen) {
	// minlen.Path  — root-relative path segments, e.g. ["addr", "zip"]
	// minlen.Limit, minlen.Got
	// minlen.Pos   — byte offset of the failure
}
```

> [!NOTE] In `-multierr` mode the generated code returns `validation.Errors`, a
> `validation.Error` slice instead of stopping at first failure.

Low-level parse failures such as malformed JSON or unexpected JSON value come back
wrapped in `*decode.ParseError` that also carries some parsing metadata:

```go
var pe *decode.ParseError
if errors.As(err, &pe) {
	// pe.Path  — root-relative path segments, e.g. ["addr", "street"]
	// pe.Pos   — byte offset
	// pe.Err   — underlying sentinel (scan.ErrBadString, scan.ErrBadObject, …)
}

// The wrap is transparent to errors.Is — the underlying scan sentinel
// is still reachable:
if errors.Is(err, scan.ErrBadString) { ... }
```

## supported kinds

| category  | go types                                                         | wire   | notes                                                                                      |
| --------- | ---------------------------------------------------------------- | ------ | ------------------------------------------------------------------------------------------ |
| primitive | `string`, `bool`, `int*`, `uint*`, `float*`                      | scalar | `*T` for any of these — `null` ↔ `nil`; multi-level `**T`/… also supported                 |
| slice     | `[]T`                                                            | array  | nil → `null`; `[]*T` decodes into a single contiguous `[]T` slab. `[]**T`/… also supported |
| array     | `[N]T`                                                           | tuple  | strict element count — mismatch → `validation.LenError`; `[N]*T` uses a fixed `[N]T` slab  |
| map       | `map[string]V`                                                   | object | string keys only. `map[string]*V` / `**V` / … values decode natively, `null` ↔ `nil`       |
| struct    | named / embedded                                                 | object | embedded fields are promoted, same as `encoding/json`                                      |
| cross-pkg | foreign struct / named type                                      | varies | static method-set probe at codegen — see _cross-package interfaces_ below                  |
| alias     | `//ggen:generate type X ...` (see [type aliases](#type-aliases)) | varies | full method surface generated; strategy picked from the underlying type                    |

### cross-package interfaces

For any field whose type is defined outside the package being generated, ggen
probes the method set at codegen time and emits a direct call — no runtime
probing. The first method available in order is picked:

| direction | ladder                                                                                            |
| --------- | ------------------------------------------------------------------------------------------------- |
| decode    | `DecodeFrom` (`DecodeFromStream`) → `UnmarshalJSON` → `UnmarshalText` → `encoding/json.Unmarshal` |
| encode    | `AppendJSON` → `MarshalJSON` → `AppendText` → `MarshalText` → `encoding/json.Marshal`             |

Types like `uuid.UUID` (Go 1.27's stdlib `uuid`, or `github.com/google/uuid`)
implement `TextMarshaler`/`TextUnmarshaler` and no JSON methods at all, so the
ladder routes them through the text path and their wire shape is a JSON string.
Both expose `AppendText`, which ggen prefers over `MarshalText` — it appends
into the caller's buffer instead of returning a fresh `[]byte`.

Note the ladder is ordered, not additive: a type carrying BOTH JSON and text
methods takes the JSON rung, so its wire shape is whatever `MarshalJSON`
produces, not a string.

`encoding/json.Unmarshal` and `json.Marshal` are used only as a fallback when
none of the above methods are present on the custom type.

### type aliases

`//ggen:generate` on a named top-level type generates the full method surface
(`DecodeFrom`, `DecodeFromStream`, `JSONSize`, `AppendJSON`), so the alias is
decoded like any other ggen type. The codegen strategy is picked automatically
from the underlying type's shape and method set:

| flavor                           | example                 | strategy                                                                 |
| -------------------------------- | ----------------------- | ------------------------------------------------------------------------ |
| primitive                        | `type Count int`        | scan + cast; `htmlescape`/`marshal`/`unmarshal` annotations still apply  |
| struct (exported fields)         | `type Comment Inner`    | field introspection — treats the alias like a regular struct             |
| struct (has `DecodeFrom`)        | `type X HasGgenMethods` | cast & delegate to the underlying's existing ggen methods                |
| struct (opaque + Marshaler/Text) | `type Local time.Time`  | delegate to underlying's `MarshalJSON`/`AppendText`                      |
| container                        | `type Tags []string`    | same emitters as slice/map/array fields — all field-level features apply |

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
type ID int64

//ggen:generate
type Tags []string

//ggen:generate
type LocalUUID uuid.UUID  // delegates to uuid.UUID's TextAppender

//ggen:generate
type Comment struct {
	ID     ID         `json:"id"     pipe:"gte=1"`                           // numeric alias, no quoting; gte runs against int
	Author string     `json:"author" pipe:"required trim minlen=1"`          // plain string, fast path
	Body   HtmlString `json:"body"   pipe:"required trim lower maxlen=4096"` // \uXXXX-escaped via the alias; mods cast through string
	Tags   Tags       `json:"tags"   pipe:"inner:notempty"`                  // inner: runs against each element
}
```

Each field's wire shape is decided by its type, not its tag — so the escaping
cost only lands on `Body`, not on `Author`.

### stdlib types

ggen treats a number of stdlib types as first-class, with special encoding and
decoding rules:

| type                               | wire                | notes                                                                                               |
| ---------------------------------- | ------------------- | --------------------------------------------------------------------------------------------------- |
| `time.Time`                        | string              | `format:unix`, `unixmilli`, `unixmicro`, `unixnano`, `RFC3339`, custom layout `format:'2006-01-02'` |
| `time.Duration`                    | string              | `format:sec`, `milli`, `micro`, `nano`, `units` (default)                                           |
| `[]byte`                           | base64 string       | `format:base64` (default), `base64url`, `base32`, `base32hex`, `base16`/`hex`, `array`              |
| `net.IP`                           | string              | —                                                                                                   |
| `netip.Addr`                       | string              | —                                                                                                   |
| `netip.Prefix`                     | string              | —                                                                                                   |
| `json.RawMessage`/`jsontext.Value` | passthrough         | —                                                                                                   |
| `net/url.URL`                      | string              | `url.Parse` on decode, `String()` on encode. incompatible with stdlib                               |
| `math/big.Int`                     | number              | arbitrary precision                                                                                 |
| `math/big.Float`                   | string              | arbitrary precision (matches stdlib)                                                                |
| `math/big.Rat`                     | string              | `"22/7"`                                                                                            |
| `database/sql.Null*`               | inner value or null | incompatible with stdlib                                                                            |
| `database/sql.Null[T]`             | inner value or null | ggen handles `T` as a field with extra nullzero semantics                                           |

`any` also works, similar to how the standard json treats it.

#### divergences from stdlib

The `net/url.URL`, `sql.Null*`\*, and `sql.Null[T]` rows above ship a different
wire shape from `encoding/json` v1/v2 — ggen serializes them the way consumers
usually expect, diverging from stdlib's exported-field struct dump:

| type                      | ggen wire             | stdlib wire (v1 + v2)                          |
| ------------------------- | --------------------- | ---------------------------------------------- |
| `net/url.URL`             | `"https://x/p?q=1"`   | `{"Scheme":"https","Host":"x", ... 11 fields}` |
| `sql.Null*`/`sql.Null[T]` | inner value or `null` | `{"<Inner>":val,"Valid":true}`                 |

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

this whole project was vibe coded with claude code. every line of
the generator, the runtime libraries, the tests, the fuzzers — typed
by the model, steered by me. i didnt really care about code of the
generator, instead laser focused on quality of the _generated_ code.

## license

[MIT](LICENSE).
