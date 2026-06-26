# encode — Marshaler interface + AppendJSON helpers

Runtime package. Defines `Marshaler` interface every generated struct
satisfies, plus buffer-append helpers generated code calls and
`AppendAny` runtime walker for `any` / `interface{}` fields.

## Files

- `encode/encode.go` — Marshaler interface, `Marshal`/`MarshalString`/`WriteTo` +
  slice variants, `BytesToString`.
- `encode/string.go` — `AppendString` (HTML-safe) + `AppendStringNoHTML`
  (jsonv2 default).
- `encode/any.go` — `AppendAny` runtime walker, concrete-type fast paths.
- `encode/url.go` — net/url.URL helpers for generated code.

## Surface

```go
type Marshaler interface {
    AppendJSON(dst []byte) ([]byte, error)
    JSONSize() int
}

func Marshal[T Marshaler](v T) ([]byte, error)        // append into make([]byte,0,v.JSONSize()) — 1 alloc
func MarshalString[T Marshaler](v T) (string, error)  // Marshal + BytesToString aliasing (no extra alloc)
func WriteTo(w io.Writer, v Marshaler) error          // pooled buffer; first non-nil error
func AppendSlice[T Marshaler](dst []byte, items []T) ([]byte, error)
func MarshalSlice[T Marshaler](items []T) ([]byte, error)
func MarshalSliceString[T Marshaler](items []T) (string, error)
func WriteSliceTo[T Marshaler](w io.Writer, items []T) error

func BytesToString(buf []byte) string                // unsafe.String over buffer
func AppendString(dst []byte, s string) []byte       // HTML-safe variant
func AppendStringNoHTML(dst []byte, s string) []byte // jsonv2-default variant
func AppendFloat(dst []byte, v float64, bitSize int) ([]byte, error) // stdlib-parity float format
func AppendAny(dst []byte, v any) ([]byte, error)     // any-walker, NoHTML escaping
func AppendAnyHTML(dst []byte, v any) ([]byte, error) // any-walker, HTML-safe escaping
```

## `AppendFloat` — stdlib-parity format selection

ES6 ToString semantics, matching v1 AND v2 byte-for-byte: `'f'` while the
decimal exponent sits in [-6, 21), `'e'` otherwise, then the zero-padded
negative exponent is trimmed in place (`1e-07` → `1e-7`). A bare `'g'` verb
was a silent wire divergence (`1e6` → `1e+06` vs stdlib `1000000`) — pinned
by `TestAppendFloatStdlibParity`, every row cross-checked against v1.
Codegen's `sizeFloat` budget is 25 (longest 'f' form: sign + `0.` + 5 zeros
+ 17 digits). The duration `format:sec` emit routes here too.

## `Marshal` / `MarshalString` — generic, devirtualized

Both are generic over `T Marshaler` (not a plain `Marshaler` arg) so a concrete
call (`encode.Marshal(claim)`, the generated `MarshalJSON` hook, user code with
concrete types) monomorphizes: the `JSONSize`/`AppendJSON` calls devirtualize
and the value is NOT boxed into the interface. Measured on `Tiny_Marshal/ggen`:
**2 → 1 allocs/op (the box), 320 → 224 B/op, −33% ns** (opt [10]). Caveat: a
generic function can't be taken as a bare func value — `f := encode.Marshal`
needs explicit instantiation `encode.Marshal[T]`. The interface `Marshaler`
type itself is unchanged; passing an already-boxed `Marshaler` still works
(T = the interface).

`WriteTo` / `WriteSliceTo` (renamed from `Write` / `WriteSlice`) stay
NON-generic interface-arg functions: their pooled-buffer + `io.Write` cost
dwarfs one box, and presizing their buffer from `JSONSize()` was tried and
**rejected** — the pool converges to the max payload size, so the size walk is
pure overhead (+3% tiny / +4% mega, a second full tree walk; see backlog).

## `MarshalSlice` / `AppendSlice` — per-item sizing, nil pointers

The output buffer is presized from `sliceJSONSize` — the SUM of each item's
`JSONSize()` — not `len(items) * zero.JSONSize()` (the zero value's size is
only the constant-folded base; populated items undersized massively and ran
the append growth chain). Pointer-typed `T` gets a one-time
`reflect.TypeFor[T]().Kind()` probe + per-item `IsNil` check: nil elements
emit `null` (stdlib parity) — previously `MarshalSlice[*T]` panicked on the
zero value's promoted `JSONSize()` before writing a single byte. Pinned by
`TestMarshalSlicePointerElems` / `TestMarshalSliceSingleAlloc`.

## Error propagation

`AppendJSON` returns `([]byte, error)`; errors propagate from any nested
encoder can fail (nested AppendJSON, TextAppender, TextMarshaler,
JSONMarshaler, `encoding/json.Marshal` fallback). Generator threads errors
through every nested call. Pure-primitive structs declare `var err error; _ =
err` (compiler elides — no runtime cost).

## `JSONSize()` — upper-bound overshoot

Intentional overshoot, not tight fit. Map estimate per-entry: `4 +
2*len(key) + value-bound` (kind-derived) or flat 128 for variable values.
String = `len*2+2` for short-escape worst-case (`\n`, `\"`, `\\`, `\t`, `\b`,
`\f` — every byte → 2).

**Pathological corner**: control chars below 0x20 with no short escape expand to
`\uXXXX` (6× per byte) and DO overflow bound. One-time realloc on that
input accepted — real payloads rarely have raw control bytes.
`TestJSONSize_NoReallocOnWorstCase` (in `integrationtests/`) pins
cap-guarantee on realistic worst-case input.

Constant per-field contributions folded into initial `size := N` at
codegen; only loops/`len()` emit runtime adds. Pure-primitive structs
collapse to `return N`.

## `AppendString` / `AppendStringNoHTML`

Both write **escaped body + closing `"`**. CALLER writes opening
`"` — codegen folds into constant `"key":"` prefix at struct-field top
level, or emits explicit `dst = append(dst, '"')` at slice/map/standalone
sites.

- `AppendString` — HTML-safe, escapes `<`, `>`, `&` to `\uXXXX` (matches stdlib
  v1). Codegen routes here when parent opts in via `htmlescape` / `-htmlescape`.
- `AppendStringNoHTML` — default, standard JSON escapes only, emits `<`, `>`,
  `&` literally (matches stdlib jsonv2).

The hot scan's "does this byte need escaping?" test is a `[256]bool` table
lookup (`needEscapeNoHTML` / `needEscapeHTML`, built at init), not a chain of
byte comparisons (opt [14]). Counter to the [4] decode result (where a table
LOST to range checks), here the table WINS: the escape predicate is a 3-deep
(NoHTML) / 6-deep (HTML) **dependent** `&&` chain, and replacing it with one
**independent** L1 load lets the CPU pipeline across bytes. Three-way head-to-
head in one binary (clean/no-escape strings — the common, confound-free case),
clean-pinned `GOMAXPROCS=1` n=15: table **1239/1179 ns** vs comparison chain
**1688/2167** (−27% NoHTML, −46% HTML) vs a branchless uint64 register-bitmap
**3610/3574** (bitmap is ~2.9× slower — asm shows ~14 instrs/byte vs the table's
~4; see backlog Tried Rejected). Numbers from a one-time 3-way head-to-head
bench (since removed); `TestAppendString_TableParity` pins byte-for-byte parity
against an independent comparison-chain reference over all 256. Flat on Mega
marshal (memory-latency-bound) — the win is the cache-resident string-heavy case.

## `AppendAny` — runtime walker for `any` fields

Type-switches over runtime primitives, homogeneous primitive slices/maps, and
small set of concrete stdlib types **before** falling to reflection.

### Escaping

Both entry points share one walker (`appendAny`) parameterized by an
`escapeFn` (`AppendString` / `AppendStringNoHTML`) threaded through every
helper, so nested strings AND map keys escape consistently. `AppendAny`
defaults to NoHTML (jsonv2 parity — matches sibling generated string
fields; it used to hardcode the HTML-safe variant, a wire inconsistency);
`AppendAnyHTML` is the v1-shaped variant. Codegen picks via
`appendAnyFn(f.HTMLEscape)` — `htmlescape` structs route their `any`
fields/inline maps through `AppendAnyHTML`. Pinned by raw-byte comparison
in `TestAppendAny_NoHTMLEscapeDefault` (checkAny reparses and would mask
escaping drift).

### Switch ordering rules (don't break)

Case order load-bearing for perf:

1. **Concrete primitives** (`string`, `bool`, `int*`, `uint*`, `float*`, `nil`).
2. **Homogeneous primitive slices** (`[]int*`, `[]uint16/32/64`, `[]float*`,
   `[]bool`, `[]string`, `[]any`). Skip `[]uint8` — that's `[]byte`, must stay
   on base64 reflect.Slice path. Plus two concrete COMPOSITE-element slices
   (`[]time.Time`, `[]json.RawMessage`, opt [12]) handled wholesale so their
   elements skip the reflect.Slice path's per-element `rv.Interface()` box
   (−80% allocs / −41% ns on the 32-elem `time_slice` bench).
3. **Homogeneous string-keyed primitive maps** (`map[string]int*`/`uint*`/
   `float*`/`bool`/`string`/`any`) → generic helpers (`appendMapInt[V]`,
   `appendSliceFloat[V]`, …): one strconv call per entry, no reflect.
4. **Concrete stdlib hooks** — `json.RawMessage` (verbatim), `time.Time`
   (AppendText), pointer-to-primitive (`*string`/`*bool`/`*int*`/`*uint*`/
   `*float*`, nil → `null`). Sit **before** `case json.Marshaler` so they
   win switch over interface dispatch.
5. **Interface fallbacks** — ggen `Marshaler` / `json.Marshaler` /
   `encoding.TextAppender` / `encoding.TextMarshaler`.
6. **Reflection** — slices/arrays/maps/pointers/structs (with json-tag parsing
   for struct walking), keeping nested ggen `Marshaler` / `TextAppender` on
   fast path with no `json.Marshal` cliff. The `reflect.Map` walk reuses two
   addressable scratch `reflect.Value`s via `Value.SetIterKey`/`SetIterValue`
   (opt [11]) instead of `iter.Key()`/`iter.Value()`, which allocate a fresh
   Value per entry — turns 2 allocs/entry into 2 fixed (named-map-of-primitive
   drops to ~0/entry: −87% allocs / −32% ns on the 32-entry `named_map_int`
   bench). Only `any` fields reach this — generated code emits direct map code.

### `usenumber` mode

With `usenumber`, generator emits `scan.AnyNumber` for decode (numbers →
`json.Number` aliased over input via `unsafe.String`, zero-alloc/zero-copy
happy path); encode-side `json.Number` is string newtype handled by
standard string case.

### Adding a new concrete case

Place **before** matching interface case. `time.Time` implements
`json.Marshaler`, so concrete `time.Time` case must precede `case
json.Marshaler`; pointer-to-primitive cases must precede `case reflect.Pointer`
(else fallback boxes via `rv.Elem().Interface()`).

## Tests

- `encode/appendany_test.go` — `AppendAny` correctness + `BenchmarkAppendAny` /
  `BenchmarkAppendAny_Presized`. Lives next to implementation for direct
  unexported-symbol access without integrationtests module setup.
- `encode/url_test.go` — net/url.URL encode helpers.
