> [!CAUTION]
> Project is under active developemnt. No release versions are available. Every new commit changes behaviour.

# ggen

zero-copy, zero-reflection JSON codegen for Go.

ggen parses structs and generates custom `DecodeFrom`, `DecodeFromStream`,
`JSONSize`, and `AppendJSON` methods. Decoder is a zero-copy byte
scanner with no token layer and encoder pre-sizes appends into a single `[]byte`,
presizing which is also possible using the generated `JSONSize` method.

## benchmarks

Run over a ~5.6 MiB deep tree with full validation. See [mega_test.go](./bench/mega_test.go).

```sh
$ cd bench; go test -bench=BenchmarkMega -run=^$ -benchtime=500x -cpu=1 .
goos: linux
goarch: amd64
pkg: github.com/sirkostya009/ggen/bench
cpu: AMD RYZEN AI MAX+ 395 w/ Radeon 8060S
BenchmarkMega_Unmarshal/jsonv2         500   52563352 ns/op   111.57 MB/s   308 gc   0.6160 gc/op   60781 heap_KB    8640909 total_KB   17696582 B/op   316829 allocs/op
BenchmarkMega_Unmarshal/sonic          500   25693318 ns/op   228.25 MB/s   260 gc   0.5200 gc/op   80755 heap_KB   10151380 total_KB   20790025 B/op   137771 allocs/op
BenchmarkMega_Unmarshal/sonic_fast     500   25248718 ns/op   232.27 MB/s   255 gc   0.5100 gc/op   78844 heap_KB   10151379 total_KB   20790023 B/op   137771 allocs/op
BenchmarkMega_Unmarshal/easyjson       500   39989115 ns/op   146.66 MB/s   257 gc   0.5140 gc/op   69709 heap_KB    8290738 total_KB   16979432 B/op   245856 allocs/op
BenchmarkMega_Unmarshal/ggen           500   25787169 ns/op   227.42 MB/s   214 gc   0.4280 gc/op   64692 heap_KB    7037938 total_KB   14413697 B/op   101927 allocs/op
BenchmarkMega_Marshal/jsonv2           500   23065207 ns/op   254.12 MB/s    84 gc   0.1680 gc/op   48578 heap_KB    2918348 total_KB    5976775 B/op     7407 allocs/op
BenchmarkMega_Marshal/sonic            500   19257913 ns/op   304.53 MB/s   500 gc   1.0000 gc/op   62333 heap_KB   16416610 total_KB   33621216 B/op     5113 allocs/op
BenchmarkMega_Marshal/sonic_fast       500   18545559 ns/op   316.23 MB/s   490 gc   0.9800 gc/op   56003 heap_KB   16416539 total_KB   33621070 B/op     5111 allocs/op
BenchmarkMega_Marshal/easyjson         500   16526613 ns/op   354.86 MB/s    97 gc   0.1940 gc/op   59856 heap_KB    2986436 total_KB    6116221 B/op     7586 allocs/op
BenchmarkMega_Marshal/ggen             500   14572928 ns/op   402.43 MB/s   167 gc   0.3340 gc/op   58834 heap_KB    5772125 total_KB   11821312 B/op        2 allocs/op
BenchmarkMega_Marshal/ggen_presized    500   10667889 ns/op   549.75 MB/s     0 gc   0.0000 gc/op   35747 heap_KB          0.16 total_KB       0 B/op        0 allocs/op
BenchmarkMega_Reader/jsonv2            500   53395526 ns/op   109.83 MB/s   328 gc   0.6560 gc/op   54902 heap_KB    8640917 total_KB   17696597 B/op   316829 allocs/op
BenchmarkMega_Reader/sonic             500   29465497 ns/op   199.03 MB/s   495 gc   0.9900 gc/op   58855 heap_KB   19034886 total_KB   38983446 B/op   137793 allocs/op
BenchmarkMega_Reader/sonic_fast        500   28380260 ns/op   206.64 MB/s   492 gc   0.9840 gc/op   58855 heap_KB   19034888 total_KB   38983450 B/op   137793 allocs/op
BenchmarkMega_Reader/easyjson          500   40422677 ns/op   145.08 MB/s   499 gc   0.9980 gc/op   54989 heap_KB   15390776 total_KB   31520308 B/op   245887 allocs/op
BenchmarkMega_Reader/ggen_stream       500  118994229 ns/op    49.28 MB/s   276 gc   0.5520 gc/op   62869 heap_KB    8607363 total_KB   17627880 B/op   256588 allocs/op
BenchmarkMega_Reader/ggen_readall      500   28062206 ns/op   208.99 MB/s   464 gc   0.9280 gc/op   52558 heap_KB   14137818 total_KB   28954251 B/op   101956 allocs/op
```

Fast numbers are explained by ggen's zero-copy strategy for strings and
`json.RawMessage`. Everytime you provide a buffer to `DecodeFrom` all strings
and raw byte arrays are aliased via unsafe typecasting. Any changes to
payload JSON strings will have result struct data changed too, be careful
with that.

This kind of approach has one major sideffect worth considering: if any of
the parsed struct's strings are referenced by something long-living in your
program, this may theoretically implode memory usage resulting into significantly
worse performance. Go's GC isn't compacting memory.

However, this is not the case with Streams - they always copy everything as
current implementation tries to reuse underlying bytes buffer.

### slow-network streaming

Another use case for ggen is slow or long network streams.

Benchmarking against a faked `io.Reader` that simulates a network
connection warming up fast yields:

```sh
$ cd bench; go test -bench=BenchmarkSlowStream -run=^$ -benchtime=100x .
BenchmarkSlowStream_Valid/stdjson          100  164793233 ns/op   0.22 MB/s   118973 B/op   1995 allocs/op
BenchmarkSlowStream_Valid/sonic            100  141190738 ns/op   0.25 MB/s   126977 B/op    830 allocs/op
BenchmarkSlowStream_Valid/sonic_fast       100  141227775 ns/op   0.25 MB/s   125913 B/op    830 allocs/op
BenchmarkSlowStream_Valid/easyjson         100  158137393 ns/op   0.23 MB/s   187438 B/op   1542 allocs/op
BenchmarkSlowStream_Valid/ggen_stream      100  141286440 ns/op   0.25 MB/s   102765 B/op   1585 allocs/op
BenchmarkSlowStream_Valid/ggen_readall     100  158017794 ns/op   0.23 MB/s   171509 B/op    623 allocs/op
BenchmarkSlowStream_Invalid/ggen_stream    100   66253581 ns/op   0.04 MB/s     3218 B/op      4 allocs/op
BenchmarkSlowStream_Invalid/ggen_readall   100   77715011 ns/op   0.04 MB/s     7362 B/op      9 allocs/op
BenchmarkSlowStream_Invalid/jsonv2         100   81940800 ns/op   0.04 MB/s    16770 B/op     25 allocs/op
BenchmarkSlowStream_Invalid/sonic          100   66258555 ns/op   0.04 MB/s     3421 B/op      6 allocs/op
BenchmarkSlowStream_Invalid/sonic_fast     100   66264328 ns/op   0.04 MB/s     3421 B/op      6 allocs/op
```

`Invalid` benchmarks exercise the fail-fast advantage - `ggen` validation
works with streams and rejects invalid payloads bailing on processing full
payload on failure.

See [slowstream_test.go](./bench/slowstream_test.go).

### memory residency

Another kind of benchmarking that Go's default bench tool doesn't easily
expose is memory retained _after_ all parsing is done, per element.

`BenchmarkRetention` evaluates memory utilized by Node structs after all
parsing and garbage collection has settled - a slightly unrealistic
picture but its a showcase of how byte-aliasing can help curb memory usage
avoiding expensive copying of strings and raw bytes all the time.

```sh
$ cd bench; go test -bench=BenchmarkRetention -run=^$ -benchtime=100x .
BenchmarkRetention/stdjson         100   270798 ns/op   132.64 MB/s    97.36 retain_KB/op    9.508 retain_MiB    2.776 retain×payload   102551 B/op   1913 allocs/op
BenchmarkRetention/sonic           100   112227 ns/op   320.06 MB/s   120.20 retain_KB/op   11.740 retain_MiB    3.428 retain×payload   126905 B/op    829 allocs/op
BenchmarkRetention/sonic_fast      100   111367 ns/op   322.54 MB/s   120.20 retain_KB/op   11.740 retain_MiB    3.428 retain×payload   126904 B/op    829 allocs/op
BenchmarkRetention/easyjson        100   210303 ns/op   170.80 MB/s    94.96 retain_KB/op    9.273 retain_MiB    2.707 retain×payload   187618 B/op   1543 allocs/op
BenchmarkRetention/ggen_stream     100   193106 ns/op   186.01 MB/s    97.68 retain_KB/op    9.539 retain_MiB    2.785 retain×payload   103198 B/op   1587 allocs/op
BenchmarkRetention/ggen_bytes      100    97557 ns/op   368.19 MB/s    78.88 retain_KB/op    7.703 retain_MiB    2.249 retain×payload    82838 B/op    609 allocs/op
BenchmarkRetention/ggen_readall    100   121689 ns/op   295.18 MB/s   119.80 retain_KB/op   11.700 retain_MiB    3.414 retain×payload   171846 B/op    625 allocs/op
```

## usage

Primary use case that is in mind for ggen is HTTP servers. Current
Stream implementation is a bit lacking in memory performance but still
a faster strategy for slower networks especially when if you
get lots of invalid payloads that can be discared in parse-time.

Install the CLI and pull in the runtime subpackages your generated code
will import:

```sh
go install github.com/sirkostya009/ggen@latest
go get github.com/sirkostya009/ggen
```

Annotate a struct with `//ggen:generate` and run the cli.

```go
package api

//ggen:generate
type User struct {
    ID    int      `json:"id"`
    Name  string   `json:"name"   ggen:"required,minlen=1,maxlen=64"`
    Email string   `json:"email"  ggen:"email" mod:"trim,lower"`
    Tags  []string `json:"tags,omitempty" ggen:"dive:notempty"`
}
```

```sh
ggen .               # current package
ggen ./...           # every package matched by the pattern — module-scoped, same as `go build ./...`
ggen ./pkg/...       # subtree pattern (relative paths must start with `./`)
ggen path/to/file.go # one file; optional struct-name filter as trailing args
```

The generated file lives next to source. Output naming follows input:
a package run gets `<dir>_ggen.go` (and `<dir>_ggen_test.go` for any
annotated structs declared in `_test.go` files). A single file gets
`<base>_ggen.go`. The `-o` flag overrides path for single-file or
single-package mode.

For input files that have a `//go:build` constraint, ggen carries that
constraint into a separate output file: a struct in `tagged.go`
guarded by `//go:build foo` ends up in `<dir>_foo_ggen.go` with the same
`//go:build foo` header, so unconstrained builds aren't broken by an
"undefined: Tagged" reference. Multi-term constraints (e.g.
`//go:build foo && bar`) get a slugified filename
(`<dir>_foo_bar_ggen.go`) and the original expression preserved verbatim
in the header.

You can of course use ggen along with `go generate` — one `//go:generate`
directive per package is enough, no need to put one above every annotated
struct:

```go
//go:generate ggen .
```

Or, if you'd rather scope generated output per file, put a `//go:generate ggen $GOFILE`
line in every file that has annotated structs and let single-file mode
handle the naming:

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

Additional methods generated with `marshal` and `unmarshal` annotations set,
respectively:

```go
func (T)  MarshalJSON() ([]byte, error)
func (*T) UnmarshalJSON([]byte) error
```

Use generated methods directly on values or in-place values.

```go
// parse from []byte
u, _, err := User{}.DecodeFrom(payload)

// prealocate a []byte buf
buf := make([]byte, 0, u.JSONSize())
// write to []byte
buf, err = u.AppendJSON(buf)

// wrap an io.Reader in a stream with a preallocated intermediate buf
s := scan.NewStream(r, buf)
// lazily decode from stream
u, err = u.DecodeFromStream(s)
// once finished you can reutilize buf for another stream
```

Similar to stdlib, ggen also has merge semantics but they're deliberately made different:
non-nil slices, maps and pointer fields are reused, which can be used as an optimization
on paths where you need to repeatedly parse same object shape. A JSON `null` still nils
a slice/map/pointer field.

It is deliberately not identical though: all containers are reset regardless of
presence in payload (stdlib keeps them) — a blank payload gives you a blank
slate while keeping capacity — and a JSON `null` aimed at a non-pointer
scalar or struct is treated as a parse error — use a pointer if you need a
nullable scalar.

### runtime packages

Apart from the byte-scan and append primitives, the runtime packages
also expose a small set of helpers in `encode` / `decode` for the
patterns you'd actually use from your own code.

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

Most CLI flags have matching per-struct annotation token (no leading dash).
Flags apply globally to the whole pass, annotations apply locally to the struct.
Multiple annotation tokens are space-separated: `//ggen:generate marshal
unmarshal multierr`.

| CLI flag         | struct annotation | effect                                                                                                                                                                                      |
| ---------------- | ----------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `-o <path>`      | —                 | override output path (single-file or single-package mode only)                                                                                                                              |
| `-pkg <name>`    | —                 | override the package name in the generated file                                                                                                                                             |
| `-marshal`       | `marshal`         | also emit a `MarshalJSON` hook so the type satisfies `encoding/json.Marshaler`                                                                                                              |
| `-unmarshal`     | `unmarshal`       | also emit an `UnmarshalJSON` hook for `encoding/json.Unmarshaler`                                                                                                                           |
| `-multierr`      | `multierr`        | accumulate every validation failure into `validation.Errors` instead of returning on the first one                                                                                          |
| `-allowdups`     | `allowdups`       | accept duplicate JSON keys with first-wins semantics — the first occurrence is parsed, later ones are skipped via `scan.SkipValue` without being decoded (default: error on the second hit) |
| `-novalidate`    | `novalidate`      | drop validation, required-field checks, and mods entirely — fastest decode path                                                                                                             |
| `-ignoreunknown` | `ignoreunknown`   | silently drop unknown JSON keys (default: error). overridden by an inline catch-all map field                                                                                               |
| `-nosortkeys`    | `nosortkeys`      | emit struct fields in declaration order (default: alphabetical by JSON name, compresses better)                                                                                             |
| `-usenumber`     | `usenumber`       | decode numbers in `any` fields as `json.Number` instead of `float64`                                                                                                                        |
| `-htmlescape`    | `htmlescape`      | escape `<`, `>`, `&` to `\uXXXX` for safe embedding in HTML (default: literal, matches `encoding/json` v2 — v2 dropped HTML escaping as a default)                                          |
| `-dry`           | —                 | parse and validate every annotated struct, surface every error, emit no file. Useful in CI/pre-commit to fail fast on broken tags or annotations. Rejects `-o` / `-pkg`                     |

## struct tags

### `json:"..."`

The standard Go stdlib json/jsonv2 tags work as-is, including the field
selection rule: only exported fields are encoded and decoded. Unexported
fields are skipped silently (no decode wiring, never appear in marshal
output) — same as `encoding/json`. Extras worth knowing:

- `json:",inline"` — the field becomes a catch-all map for unknown keys.
  The Go type must be a string-keyed map (`map[string]V`); V can be `any`,
  a primitive, a ggen-annotated struct, or any other concrete type.
  Typed values are decoded directly through the elem's fast path when one
  exists (string scan, generated `DecodeFrom`), or via `encoding/json`
  unmarshal of the captured span otherwise. Overrides `-ignoreunknown`;
  on marshal the entries are spliced into the parent object.
- `format:X` — type-specific format hint (see _supported kinds_ below).
  Per jsonv2, this MUST be the last option in the tag.

### `ggen:"..."` — validation rules

Validation rules run right after a value has been parsed and reshaped by
mods.

| rule                                  | error                                          | what it checks                                                                        |
| ------------------------------------- | ---------------------------------------------- | ------------------------------------------------------------------------------------- |
| `required`                            | `RequiredError`                                | field MUST be present in the JSON object                                              |
| `optional`                            | —                                              | marker for explicit "this is optional"; doesn't actually do anything                  |
| `notempty`                            | `NotEmptyError`                                | string non-empty / slice / map non-zero length                                        |
| `len=N`, `minlen=N`, `maxlen=N`       | `LenError`, `MinLenError`, `MaxLenError`       | byte-length bounds for strings; element-count bounds for slices/maps/arrays           |
| `runes=N`, `minrunes=N`, `maxrunes=N` | `RunesError`, `MinRunesError`, `MaxRunesError` | rune-count bounds (utf8 aware — `héllo` is 5 runes, 6 bytes)                          |
| `gt=N`, `gte=N`, `lt=N`, `lte=N`      | `GTError`, `GTEError`, `LTError`, `LTEError`   | numeric comparison                                                                    |
| `eq=X`, `neq=X`                       | `EqError`, `NeqError`                          | equality — accepts numeric or string operand depending on the field's kind            |
| `multiple=N`                          | `MultipleError`                                | numeric — value must be a multiple of N                                               |
| `oneof=a\|b\|c`                       | `OneOfError`                                   | value must equal one of the listed alternatives                                       |
| `email`                               | `EmailError`                                   | loose email shape — single `@` between non-space runs, at least one `.` in the domain |
| `url`                                 | `URLError`                                     | starts with `<scheme>://...` (alpha + digit/`+-.` scheme chars)                       |
| `ascii`                               | `ASCIIError`                                   | every byte ≤ 0x7F                                                                     |
| `printable`                           | `PrintableError`                               | every byte is printable ASCII (≥ 0x20 and not DEL)                                    |
| `alphanum`                            | `AlphanumError`                                | only ASCII letters and digits                                                         |
| `numeric`                             | `NumericError`                                 | only ASCII digits 0–9                                                                 |
| `lower` / `upper`                     | `LowerError` / `UpperError`                    | no uppercase / no lowercase ASCII letters                                             |
| `hexadecimal`                         | `HexadecimalError`                             | only `0–9`, `a–f`, `A–F`                                                              |
| `starts=X`, `ends=X`, `contains=X`    | `StartsError`, `EndsError`, `ContainsError`    | substring tests on strings                                                            |
| `@FuncName` / `@pkg.FuncName`         | `CustomError` (with `Cause`)                   | call your own `func(T) error`. See _custom rules_ below                               |

#### `hintlen=N` — preallocation hint

By default ggen preallocates slices and maps with a default cap of 4.
Validations like `minlen` and `len` override the default cap. You can
directly hint to preallocate a more specific number by supplying `hintlen`.

`hintlen=10` would preallocate the map or slice by emitting something like:

```go
make([]int, 0, 10)
```

`hintlen=0` explicitly disables any preallocation.

`hintlen=-N` is a generate-time error.

#### inspecting errors

```go
var minlen *validation.MinLenError
if errors.As(err, &minlen) {
    // minlen.Field, minlen.Limit, minlen.Got
}
```

> In `-multierr` mode the generated code returns `validation.Errors`
> (`[]validation.Error`) instead of stopping on the first failure. It
> implements `Unwrap() []error` so `errors.Is` / `errors.As` walk every
> accumulated error.

Low-level parse failures (malformed JSON, wrong primitive type) come back
wrapped in `*decode.ParseError`, which carries the JSON field path the
decoder was working on and a byte offset:

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
prints the `parse error at <field> (pos <n>)` prefix — call `errors.Unwrap`
to get the underlying message.

#### custom rules

`@FuncName` references a function that ggen looks up at codegen. The
signature MUST be `func(T) error` where `T` is the field's exact Go type
(including `*T` for pointer fields). There is no runtime registry, no
`any` boxing — the generator emits a direct call and the Go compiler
catches mistakes. A non-nil return wraps as
`validation.CustomError{Name: "@FuncName", Cause: err}`.

```go
//ggen:generate
type Box struct {
    N int `json:"n" ggen:"@EvenOnly"`
}

func EvenOnly(n int) error {
    if n%2 != 0 {
        return errors.New("must be even integer")
    }
    return nil
}
```

For cross-package references, write `@pkg.FuncName`. The resolver looks
through the source file's import block, so file-scoped aliases
(`import alias "path"`) work, and so do blank imports
(`_ "path"`) — useful when you pull in a library purely so ggen can
resolve a name. The package's declared name is honored when it differs
from its directory basename.

#### `dive:` and `keys:`

You can apply validation to containers' inner elements using either
`dive:` or `keys:`.

`keys:` only applies to map keys, while `dive:` applies to slices, arrays
and maps as well. Same applicability rules apply.

#### example

```go
Aliases map[string][]Email `json:"aliases" ggen:"keys:minrunes=2,maxrunes=32,minlen=1,dive:maxlen=10,dive:@CheckEmail"`
```

Reads as: keys must have character (rune) count from 2 to 32, the map
itself must have at least one entry, each value slice may be at most 10
elements and each `Email` is checked by a user-defined `CheckEmail` func.

### `mod:"..."` — input transforms

Mods run after the value is decoded but before validation, so validation
sees the normalized value. The same `dive:` and `keys:` prefixes apply.

| target  | mods                                                                      |
| ------- | ------------------------------------------------------------------------- |
| string  | `trim`, `lower`, `upper`, `trimleft=X`, `trimright=X`, `replace=old\|new` |
| numeric | `clamp=lo\|hi` (either side can be empty: `clamp=0\|`, `clamp=\|100`)     |
| custom  | `@FuncName` / `@pkg.FuncName`                                             |

Unlike custom validations, mods don't have to return an error, so you have two
options of what to provide:

- pure functions: `func(T) T`, emitted as `field = Func(field)`
- "errorable" functions: `func(T) (T, error)`. A non-nil error propagates
  immediately as a parse error (early return), even in `-multierr` mode -
  validators never get to run.

```go
//ggen:generate
type Profile struct {
    Email string `json:"email" mod:"@Squash,lower"`
}

func Squash(s string) string { return strings.ReplaceAll(s, " ", "") }
```

The same cross-package lookup rules apply as for custom validators.

> [!NOTE]
> Using some string mods like `replace` may copy the underlying string,
> resulting in an allocation. Same can occur with custom mods as well.

## supported kinds

| category  | go types                                                         | wire   | notes                                                                              |
| --------- | ---------------------------------------------------------------- | ------ | ---------------------------------------------------------------------------------- |
| primitive | `string`, `bool`, `int*`, `uint*`, `float*`                      | scalar | `*T` for any of these — `null` ↔ `nil`; multi-level `**T`/… also native            |
| slice     | `[]T`                                                            | array  | nil → `null`; `[]*T` decodes into a single contiguous slab (N allocs → ~log N); `[]**T`/… native |
| array     | `[N]T`                                                           | tuple  | strict element count — mismatch → `validation.LenError`; `[N]*T` uses a fixed slab |
| map       | `map[string]V`                                                   | object | string keys only; `map[string]*V` / `**V` / … values decode natively, `null` ↔ `nil` |
| struct    | named / embedded                                                 | object | embedded fields are promoted, same as `encoding/json`                              |
| cross-pkg | foreign struct / named type                                      | varies | static method-set probe at codegen — see _cross-package interfaces_ below          |
| alias     | `//ggen:generate type X ...` (see [type aliases](#type-aliases)) | varies | full method surface generated; strategy picked from the underlying type            |

### cross-package interfaces

For any field whose type is defined outside the package being generated,
ggen probes the method set at codegen time via and emits a direct call.
No runtime probing. Picked method is the first one available in order:

| direction | ladder                                                                                |
| --------- | ------------------------------------------------------------------------------------- |
| decode    | `DecodeFrom` → `UnmarshalText` → `UnmarshalJSON` → `encoding/json.Unmarshal`          |
| encode    | `AppendJSON` → `AppendText` → `MarshalText` → `MarshalJSON` → `encoding/json.Marshal` |

Types like `google/uuid` implement `TextMarshaler`/`TextUnmarshaler`,
so the ladder would route them through the text path automatically.
This means that even though a type might have JSON marshalling methods,
their wire shape would still be that of a JSON string if serialized via
ggen methods. `AppendText` is preferred over `MarshalText`.

`encoding/json.Unmarshal` and `json.Marshal` are used only as fallback
if none of the aforementioned methods are present on the custom type.

### type aliases

`//ggen:generate` on a named top-level type generates the full method
surface (`DecodeFrom`, `DecodeFromStream`, `JSONSize`, `AppendJSON`)
so the alias is decoded the same way as any other ggen type. The codegen
strategy is picked automatically from the underlying type's shape and
method set:

| flavor                           | example                 | strategy                                                                              |
| -------------------------------- | ----------------------- | ------------------------------------------------------------------------------------- |
| primitive                        | `type Count int`        | scan + cast; `htmlescape`/`marshal`/`unmarshal` annotations still apply               |
| struct (exported fields)         | `type Comment Inner`    | field introspection — treats the alias like a regular struct                          |
| struct (has `DecodeFrom`)        | `type X HasGgenMethods` | cast & delegate to the underlying's existing ggen methods                             |
| struct (opaque + Marshaler/Text) | `type Local time.Time`  | delegate to underlying's `MarshalJSON`/`MarshalText` (Go 1.24 `AppendText` preferred) |
| container                        | `type Tags []string`    | same emitters as slice/map/array fields — all field-level features apply              |

Aliases of channels, interfaces, and functions are rejected at generate
time (no sensible JSON shape for those).

> [!TIP]
> Pairing a primitive alias with `htmlescape` is a cheap way to split
> HTML-escaped strings from plain ones at the type level: tag only the
> fields whose values get embedded into HTML as `HtmlString`, leave
> the rest as plain `string`, and the literal fast path stays on for
> the bulk of your payload while escaping runs only where it matters.

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
    ID     UserID     `json:"id"     ggen:"gte=1"`                                   // numeric alias, no quoting; gte runs against int
    Author string     `json:"author" ggen:"required,minlen=1" mod:"trim"`            // plain string, fast path
    Body   HtmlString `json:"body"   ggen:"required,maxlen=4096" mod:"trim,lower"`   // \uXXXX-escaped via the alias; mods cast through string
    Tags   Tags       `json:"tags"   ggen:"dive:notempty"`                           // dive runs against each element
}
```

Each field's wire shape is decided by its type, not its tag — so the
escaping cost only lands on `Body`, not on `Author`.

### stdlib types

ggen treats a bunch of stdlib types as first-class with special encoding and decoding rules:

| type                 | wire                | format hints                                                                                        |
| -------------------- | ------------------- | --------------------------------------------------------------------------------------------------- |
| `time.Time`          | RFC3339Nano string  | `format:unix`, `unixmilli`, `unixmicro`, `unixnano`, `RFC3339`, custom layout `format:'2006-01-02'` |
| `time.Duration`      | string `"1h30m"`    | `format:sec`, `milli`, `micro`, `nano`, `units` (default)                                           |
| `[]byte`             | base64 string       | `format:base64` (default), `base64url`, `base32`, `base32hex`, `base16`/`hex`, `array`              |
| `net.IP`             | text                | —                                                                                                   |
| `netip.Addr`         | text                | —                                                                                                   |
| `netip.Prefix`       | text                | —                                                                                                   |
| `json.RawMessage`    | passthrough         | zero-copy alias on decode                                                                           |
| `jsontext.Value`     | passthrough         | zero-copy alias on decode                                                                           |
| `net/url.URL`        | string              | `url.Parse` on decode, `String()` on encode                                                         |
| `math/big.Int`       | JSON number         | arbitrary precision                                                                                 |
| `math/big.Float`     | JSON string         | arbitrary precision (matches jsonv2)                                                                |
| `math/big.Rat`       | JSON string         | `"22/7"`                                                                                            |
| `database/sql.NullX` | inner value or null | `NullString`, `NullInt64`/`32`/`16`, `NullByte`, `NullBool`, `NullFloat64`, `NullTime`              |

`any` also works and is similar to how standard json treats it.

#### divergences from stdlib

The `net/url.URL` and `sql.NullX` rows above ship a different wire shape
from `encoding/json` v1/v2 — ggen serializes them the way consumers would
usually expect, diverging from stdlib's exported-field struct dump:

| type          | ggen wire             | stdlib wire (v1 + v2)                          |
| ------------- | --------------------- | ---------------------------------------------- |
| `net/url.URL` | `"https://x/p?q=1"`   | `{"Scheme":"https","Host":"x", ... 11 fields}` |
| `sql.NullX`   | inner value or `null` | `{"<Inner>":val,"Valid":true}`                 |

## examples

### streaming

As mentioned previous `DecodeFromStream` paths are meant to be used
in cases where you expect invalid payloads and dont want to waste time
parsing them in full as well as parsing potentially slower network streams.

```go
//ggen:generate
type CreateUser struct {
    Email string `json:"email" ggen:"required,email"`
    Bio   string `json:"bio"   ggen:"maxlen=4096"`
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
    Items []Item `json:"items" ggen:"required,minlen=1,maxlen=100,dive:required"`
}

//ggen:generate
type Item struct {
    SKU string `json:"sku" ggen:"required,len=12,alphanum,upper"`
    Qty int    `json:"qty" ggen:"required,gte=1,lte=999"`
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
generator, instead im lazer focused on quality of the _generated_ code.

## license

[MIT](LICENSE).
