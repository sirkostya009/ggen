# ggen

zero-copy, zero-reflection JSON codegen for Go.

ggen parses structs and generates custom `DecodeFrom`, `DecodeStreamFrom`,
`JSONSize`, and `AppendJSON` methods. Decoder is a zero-copy byte
scanner with no token layer and encoder pre-sizes appends into a single `[]byte`,
presizing which is also possible using the generated `JSONSize` method.

## benchmarks

`cd bench && go test -bench=BenchmarkMega -run=^$ -benchtime=500x -cpu=1 .`
over a ~5.6 MiB deep tree with full validation.
See [mega_test.go](./bench/mega_test.go).

The bench inner loop runs under `b.RunParallel`, so `-cpu=1` measures single-thread
throughput (the numbers below) and `-cpu=N` measures N-way parallel saturation —
the same code, just steered by the standard go test flag. Each sub-bench wraps
`runtime.ReadMemStats` and reports `heap_KB` (live heap at StopTimer),
`total_KB` (alloc delta), `gc` (NumGC delta), and `gc/op` (per-iter GC rate — reads
as "fraction of an iter that triggers a GC").

```
goos: linux
goarch: amd64
pkg: github.com/sirkostya009/ggen/bench
cpu: AMD RYZEN AI MAX+ 395 w/ Radeon 8060S
BenchmarkMega_Unmarshal/jsonv2         500   52662320 ns/op   111.36 MB/s   309 gc   0.6180 gc/op   68632 heap_KB    8641128 total_KB   17697030 B/op   316830 allocs/op
BenchmarkMega_Unmarshal/sonic          500   24985770 ns/op   234.72 MB/s   256 gc   0.5120 gc/op   64782 heap_KB   10151379 total_KB   20790024 B/op   137771 allocs/op
BenchmarkMega_Unmarshal/easyjson       500   39286730 ns/op   149.28 MB/s   251 gc   0.5020 gc/op   55695 heap_KB    8291199 total_KB   16980374 B/op   245856 allocs/op
BenchmarkMega_Unmarshal/ggen           500   23376103 ns/op   250.88 MB/s   201 gc   0.4020 gc/op   72381 heap_KB    7037938 total_KB   14413697 B/op   101927 allocs/op
BenchmarkMega_Marshal/jsonv2           500   22991583 ns/op   254.94 MB/s    84 gc   0.1680 gc/op   59851 heap_KB    2918348 total_KB    5976775 B/op     7407 allocs/op
BenchmarkMega_Marshal/sonic            500   19442392 ns/op   301.64 MB/s   491 gc   0.9820 gc/op   55672 heap_KB   16416600 total_KB   33621197 B/op     5112 allocs/op
BenchmarkMega_Marshal/easyjson         500   16449044 ns/op   356.53 MB/s    99 gc   0.1980 gc/op   59966 heap_KB    2986452 total_KB    6116253 B/op     7586 allocs/op
BenchmarkMega_Marshal/ggen             500   14346641 ns/op   408.78 MB/s   165 gc   0.3300 gc/op   47465 heap_KB    5821180 total_KB   11921776 B/op     2185 allocs/op
BenchmarkMega_Reader/jsonv2            500   53413537 ns/op   109.80 MB/s   326 gc   0.6520 gc/op   52906 heap_KB    8641136 total_KB   17697045 B/op   316830 allocs/op
BenchmarkMega_Reader/sonic             500   28270170 ns/op   207.45 MB/s   489 gc   0.9780 gc/op   58889 heap_KB   19034887 total_KB   38983448 B/op   137793 allocs/op
BenchmarkMega_Reader/easyjson          500   40322120 ns/op   145.44 MB/s   499 gc   0.9980 gc/op   49577 heap_KB   15391237 total_KB   31521253 B/op   245887 allocs/op
BenchmarkMega_Reader/ggen_stream       500   32611826 ns/op   179.83 MB/s   203 gc   0.4060 gc/op   88408 heap_KB    8731813 total_KB   17882753 B/op   268385 allocs/op
BenchmarkMega_Reader/ggen_readall      500   24680078 ns/op   237.63 MB/s   473 gc   0.9460 gc/op   70078 heap_KB   14137818 total_KB   28954250 B/op   101956 allocs/op
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

Another use case that needed attempting to tackle was slow network streams.

Benchmarking against a faked network stream `io.Reader` that simulates a
connection warming up fast: 1500 → 800 byte chunks (linear over the
first 20 reads), 52 ms → 1.2 ms per-Read delay (geometric decay — each
read shaves 75% off the remaining gap, so the floor is hit in 5 reads) yields:

```
BenchmarkSlowStream_Valid/stdjson          100  164753533 ns/op   0.22 MB/s   119037 B/op   1995 allocs/op
BenchmarkSlowStream_Valid/sonic            100  141603145 ns/op   0.25 MB/s   126977 B/op    830 allocs/op
BenchmarkSlowStream_Valid/easyjson         100  158278628 ns/op   0.23 MB/s   187486 B/op   1542 allocs/op
BenchmarkSlowStream_Valid/ggen_stream      100  141261564 ns/op   0.25 MB/s   104053 B/op   1654 allocs/op
BenchmarkSlowStream_Valid/ggen_readall     100  157971044 ns/op   0.23 MB/s   171509 B/op    623 allocs/op
BenchmarkSlowStream_Invalid/ggen_stream    100   66388640 ns/op   0.04 MB/s     3218 B/op      4 allocs/op
BenchmarkSlowStream_Invalid/ggen_readall   100   77730957 ns/op   0.04 MB/s     7362 B/op      9 allocs/op
BenchmarkSlowStream_Invalid/jsonv2         100   82124208 ns/op   0.04 MB/s    16770 B/op     25 allocs/op
BenchmarkSlowStream_Invalid/sonic          100   66320363 ns/op   0.04 MB/s     3421 B/op      6 allocs/op
```

`Invalid` benchmarks exercise the fail-fast advantage. `ggen` validation
works with streams just fine and rejects invalid payloads — parser bails after
reading just enough bytes to scan it. `ReadAll` variants pay the full read
latency before validation can begin; `jsonv2` is the baseline (no validation,
reads the full body). ~12 ms gap between `ggen_stream` and `ggen_readall` —
on bigger payloads or slower networks it grows linearly.

On multi-core runs the slowstream would scale near-linearly as concurrent slow
connections overlap their sleeps.

See [slowstream_test.go](./bench/slowstream_test.go).

### memory residency

Another kind of benchmarking that Go's default bench tool doesn't easily
expose is memory retained _after_ all parsing is done, per element.

`BenchmarkRetention` evaluates memory utilized by Node structs after all
parsing and garbage collection has settled - a slightly unrealistic
picture but its a showcase of how byte-aliasing can help curb memory usage
avoiding expensive copying of strings and raw bytes all the time.

```
BenchmarkRetention/stdjson         100   5516776 ns/op    6.51 MB/s   694.0 abi.Type.Pointers/op    22.04 maps.newTable/op    39.05 maps.newarray/op   127.0 reflect.unsafe_New/op    93.84 retain_KB/op    9.164 retain_MiB    2.675 retain×payload   695.3 v2.makeString/op   102471 B/op   1913 allocs/op
BenchmarkRetention/sonic           100   2485804 ns/op   14.45 MB/s   270.0 sonic.decode/op    17.01 maps.NewEmptyMap/op    22.05 maps.newTable/op    39.06 maps.newarray/op   121.5 retain_KB/op   11.87 retain_MiB    3.464 retain×payload   431.0 rt.GrowSlice/op   126905 B/op   829 allocs/op
BenchmarkRetention/easyjson        100   4355139 ns/op    8.25 MB/s   455.0 easyjsonDecode/op    19.00 jlexer.Interface/op   726.0 jlexer.consume/op    22.04 maps.newTable/op    39.05 maps.newarray/op    91.84 retain_KB/op    8.969 retain_MiB    2.618 retain×payload   187506 B/op   1543 allocs/op
BenchmarkRetention/ggen_stream     100   8698085 ns/op    4.13 MB/s   372.0 Node.DecodeStreamFrom/op    22.04 maps.newTable/op    39.05 maps.newarray/op    95.52 retain_KB/op    9.328 retain_MiB    2.723 retain×payload   121.0 Stream.Bytes/op   813.0 Stream.String/op
BenchmarkRetention/ggen_bytes      100   2141999 ns/op   16.77 MB/s   482.0 Node.DecodeFrom/op    17.01 maps.NewEmptyMap/op    22.04 maps.newTable/op    39.05 maps.newarray/op    76.72 retain_KB/op    7.492 retain_MiB    2.187 retain×payload    15.00 scan.Any/op
BenchmarkRetention/ggen_readall    100   2237747 ns/op   16.05 MB/s   482.0 Node.DecodeFrom/op    15.00 io.ReadAll/op    17.01 maps.NewEmptyMap/op    22.04 maps.newTable/op    39.05 maps.newarray/op   117.6 retain_KB/op   11.48 retain_MiB    3.353 retain×payload
```

Run with `GGEN_BENCH_TOPALLOCS=1` env var to surface the top-5
allocation sites for each benchmark.

## usage

Primary use case that is in mind for ggen is HTTP servers. Current
Stream implementation is a bit lacking in memory performance but still
marginally faster strategy for slower networks especially when if you
get lots of invalid payloads that can be discared in parse-time.

Install the CLI and pull in the runtime subpackages your generated code
will import:

```sh
go install github.com/sirkostya009/ggen@latest
go get github.com/sirkostya009/ggen
```

Annotate a struct with `//ggen:generate` and run `ggen` over its package:

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
ggen .                # current package
ggen ./...            # walk recursively (skips dot/underscore dirs, vendor, testdata, node_modules)
ggen path/to/file.go  # one file; optional struct-name filter as trailing args
```

The generated file lives next to your sources. Output naming follows the
input: a package gets `<dir>_ggen.go` (and `<dir>_ggen_test.go` if any
annotated struct was declared in a `_test.go` file); a single file gets
`<base>_ggen.go`. The `-o` flag overrides the path for single-file or
single-package mode.

If a source file has a `//go:build` constraint, ggen carries that
constraint into a separate output file: a struct in `tagged.go`
guarded by `//go:build foo` ends up in `<dir>_foo_ggen.go` with the same
`//go:build foo` header, so unconstrained builds aren't broken by an
"undefined: Tagged" reference. Multi-term constraints (e.g.
`//go:build foo && bar`) get a slugified filename
(`<dir>_foo_bar_ggen.go`) and the original expression preserved verbatim
in the header.

You can of course use ggen along wuth `go generate` — one `//go:generate`
directive per package is enough, no need to put one above every annotated
struct:

```go
//go:generate ggen .
```

Or, if you'd rather scope generated output per file, put a `//go:generate`
line in every file that has annotated structs and let single-file mode
handle the naming. `go generate` exposes the source file's basename
via `$GOFILE`, so the same line works in every file:

```go
//go:generate ggen $GOFILE

// can of course override default naming as well:
//go:generate ggen $GOFILE -o file_ggen.go
```

Apart from the byte-scan and append primitives, the runtime packages
also expose top-level helpers you'd actually use from your own code
— generic `Unmarshal` / `Marshal` over the generated methods, plus
streaming and slice-of-T variants:

```go
import (
    "github.com/sirkostya009/ggen/decode"
    "github.com/sirkostya009/ggen/encode"
)

// single value
u, err := decode.Unmarshal[User](payload)
out, err := encode.Marshal(u)

// JSON array of T → []T
users, err := decode.UnmarshalSlice[User](payload)
out, err := encode.MarshalSlice(users)

// streaming (single value or array, with reusable buf)
u, buf, err := decode.UnmarshalStream[User](req.Body, nil)
users, buf, err := decode.UnmarshalSliceStream[User](req.Body, buf[:0])
```

### flags and annotations

Every flag has a matching per-struct annotation token (no leading dash).
Flags apply globally to the whole pass, annotations apply locally.
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

## struct tags

### `json:"..."`

The standard Go stdlib json/jsonv2 tags work as-is, including the field
selection rule: only exported fields are encoded and decoded. Unexported
fields are skipped silently (no decode wiring, never appear in marshal
output) — same as `encoding/json`. Extras worth knowing:

- `json:",inline"` — the field becomes a catch-all map for unknown keys.
  The Go type must be `map[string]any`. This overrides `-ignoreunknown`,
  and on marshal the entries are spliced into the parent object.
- `format:X` — type-specific format hint (see _supported kinds_ below).
  Per jsonv2, this MUST be the last option in the tag.

### `ggen:"..."` — validation rules

Comma-separated rules, with three optional mode prefixes:

- `(no prefix)` applies the rule to the field itself, or to the whole
  slice/map/array when the field is a container.
- `dive:` applies subsequent rules to the next nested level. Each extra
  `dive:` peels another layer — for `[][]T`, the first dive targets each
  `[]T`, the second targets each `T`.
- `keys:` applies rules to map keys only.

For example:

```go
Aliases map[string][]Email `json:"aliases" ggen:"keys:minrunes=2,maxrunes=32,minlen=1,dive:maxlen=10,dive:@CheckEmail"`
```

Reads as: keys must have character (rune) count from 2 to 32, the map
itself must have at least one entry, each value slice may be at most 10
elements and each `Email` is checked by a user-defined `CheckEmail` func.

Each rule maps to a typed error in `decode/validation`:

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
  immediately as a parse error (early return), even in `-multierr` mode.
  Validation never runs after a mod failure on the same field — mods
  guard the value, validators evaluate it.

```go
//ggen:generate
type Profile struct {
    Email string `json:"email" mod:"@Squash,trim,lower"`
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
| primitive | `string`, `bool`, `int*`, `uint*`, `float*`                      | scalar | `*T` for any of these — `null` ↔ `nil`                                             |
| slice     | `[]T`                                                            | array  | nil → `null`; `[]*T` decodes into a single contiguous slab (N allocs → ~log N)     |
| array     | `[N]T`                                                           | tuple  | strict element count — mismatch → `validation.LenError`; `[N]*T` uses a fixed slab |
| map       | `map[string]V`                                                   | object | string keys only                                                                   |
| struct    | named / embedded                                                 | object | embedded fields are promoted, same as `encoding/json`                              |
| cross-pkg | foreign struct / named type                                      | varies | static method-set probe at codegen — see _cross-package interfaces_ below          |
| alias     | `//ggen:generate type X ...` (see [type aliases](#type-aliases)) | varies | full method surface generated; strategy picked from the underlying type            |

### cross-package interfaces

For any field whose type is defined outside the package being
generated, ggen probes the method set at codegen time via
`go/types` and emits a direct call. No runtime reflection, no itab
lookups. The picked method is the first one available in each
direction:

| direction | ladder                                                                                |
| --------- | ------------------------------------------------------------------------------------- |
| decode    | `DecodeFrom` → `UnmarshalJSON` → `UnmarshalText` → `encoding/json.Unmarshal`          |
| encode    | `AppendJSON` → `MarshalJSON` → `AppendText` → `MarshalText` → `encoding/json.Marshal` |

This is what picks up `google/uuid`, `gofrs/uuid/v5`,
`shopspring/decimal`, `oklog/ulid`, `segmentio/ksuid`, `rs/xid`,
`net/mail.Address` — they all implement `TextMarshaler`/`TextUnmarshaler`
(some also `encoding.TextAppender` for zero-alloc encode), so the ladder
routes them through the text path automatically. Decode goes through
`unsafe.Slice` so `UnmarshalText` doesn't allocate the input.
`AppendText` is preferred on encode whenever the type exposes it.

`encoding/json.Unmarshal` and `json.Marshal` are used only as fallback
if none of the aforementioned methods are present on the custom type.

### type aliases

`//ggen:generate` on a named top-level type generates the full method
surface (`DecodeFrom`, `DecodeStreamFrom`, `JSONSize`, `AppendJSON`)
so the alias works directly with `decode.Unmarshal[T]` and
`encode.Marshal`. The codegen strategy is picked automatically from
the underlying type's shape and method set:

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

`any` (`interface{}`) also works and is similar to how standard json treats it.

#### divergences from stdlib

The `net/url.URL` and `sql.NullX` rows above ship a different wire shape
from `encoding/json` v1/v2 — ggen serializes them the way consumers
actually expect, not the way Go's struct-dump default produces:

| type          | ggen wire             | stdlib wire (v1 + v2)                          |
| ------------- | --------------------- | ---------------------------------------------- |
| `net/url.URL` | `"https://x/p?q=1"`   | `{"Scheme":"https","Host":"x", ... 11 fields}` |
| `sql.NullX`   | inner value or `null` | `{"<Inner>":val,"Valid":true}`                 |

Web services want URL-as-string, database drivers want null-or-value.
Round-trips through ggen are fine on both sides; piping the output
through stdlib `encoding/json` reshapes the value back to the
struct-dump form.

Another decode-side divergence worth flagging: **maps aren't merged**.
Stdlib's map decode is additive — if the destination map already has
entries, decoded keys are layered on top and prior entries survive.
ggen wipes the destination via `clear()` before decoding so the result
reflects only the incoming JSON - callers can provide their own map buffer.

| destination state | incoming  | stdlib map               | ggen map             |
| ----------------- | --------- | ------------------------ | -------------------- |
| nil               | `{}`      | nil                      | empty non-nil map    |
| nil               | `{"a":1}` | `{"a":1}`                | `{"a":1}`            |
| `{"x":9}`         | `{}`      | `{"x":9}` (preserved)    | empty (cleared)      |
| `{"x":9}`         | `{"a":1}` | `{"x":9,"a":1}` (merged) | `{"a":1}` (replaced) |
| any               | `null`    | nil                      | nil                  |

## streaming

`UnmarshalStreamRequest` and `UnmarshalStreamResponse` are the typical
streaming entry points — they pre-size the parse buffer from
`ContentLength` and feed the reader chunk-by-chunk. Combined with
per-field validation (default, no `-multierr`), an invalid request
errors out after a few fields' worth of bytes — the client doesn't
waste bandwidth finishing the upload, the server doesn't buffer the
rest. See [slow-network streaming](#slow-network-streaming) and
[memory residency](#memory-residency) above for when this actually
pays off vs `Unmarshal` over a `ReadAll` buffer.

```go
//ggen:generate
type CreateUser struct {
    Email string `json:"email" ggen:"required,email"`
    Bio   string `json:"bio"   ggen:"maxlen=4096"`
}

func handler(w http.ResponseWriter, r *http.Request) {
    in, _, err := decode.UnmarshalStreamRequest[CreateUser](r)
    if err != nil {
        // Returns immediately on the first validation failure —
        // rest of r.Body is never read.
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
    // ... use in ...
}
```

Note that using Request and Response helpers does not allow for
providing custom buffers to Stream instance.

## generated methods

For every annotated struct `T`:

```go
func (T) DecodeFrom(data []byte, i int) (T, int, error)
func (T) DecodeStreamFrom(s *scan.Stream, i int) (T, int, error)
func (T) JSONSize() int
func (T) AppendJSON(dst []byte) ([]byte, error)
```

Additional methods generated with `marshal` and `unmarshal` annotations set,
respectively:

```go
func (T)  MarshalJSON() ([]byte, error)
func (*T) UnmarshalJSON([]byte) error
```

## examples

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
by the model, steered by me. benchmarks are real and reproducible
(`./bench`), the perf claims are measured against jsonv2/sonic/easyjson
on the same payload on the same machine. don't take the numbers on faith;
clone, run `go test -bench .` in `./bench`, see for yourself.

## license

[MIT](LICENSE).
