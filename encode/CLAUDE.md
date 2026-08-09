# encode — Marshaler interface + AppendJSON helpers

Runtime package: the `Marshaler` interface every generated struct satisfies, the
buffer-append helpers generated code calls, and the `AppendAny` walker for `any`.

## Files

- `encode.go` — `Marshaler`, `Marshal`/`MarshalString`/`WriteTo` + slice variants, `BytesToString`.
- `string.go` — `AppendString` (HTML-safe) + `AppendStringNoHTML` (jsonv2 default).
- `any.go` — `AppendAny` walker + concrete-type fast paths.
- `url.go` — net/url.URL helpers.
- `netip_addr.go` — `AppendNetipAddr` (zone-aware netip.Addr string emit).

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
func AppendNetipAddr(dst []byte, a netip.Addr) []byte // addr text + closing `"`; zoned text re-escapes (zones are arbitrary bytes)
func CloseJSONString(dst []byte, from int) []byte     // close raw-appended text (TextAppender output), re-escaping iff dirty
func CloseJSONStringHTML(dst []byte, from int) []byte // htmlescape variant
func AppendStringNoHTML(dst []byte, s string) []byte // jsonv2-default variant
func AppendFloat(dst []byte, v float64, bitSize int) ([]byte, error) // stdlib-parity float format
func AppendAny(dst []byte, v any) ([]byte, error)     // any-walker, NoHTML escaping
func AppendAnyHTML(dst []byte, v any) ([]byte, error) // any-walker, HTML-safe escaping
```

## `AppendFloat` — stdlib-parity format selection

ES6 ToString, byte-for-byte v1 AND v2: `'f'` while the decimal exponent is in
[-6, 21), `'e'` otherwise, then trim the zero-padded negative exponent in place
(`1e-07` → `1e-7`). Bare `'g'` is a silent wire divergence (`1e6` → `1e+06` vs
stdlib `1000000`) — pinned by `TestAppendFloatStdlibParity`. Codegen `sizeFloat`
budget = 25 (longest 'f': sign + `0.` + 5 zeros + 17 digits). Duration
`format:sec` routes here.

## `Marshal` / `MarshalString` — generic, devirtualized

Generic over `T Marshaler` (not a plain `Marshaler` arg) so a concrete call (the
generated `MarshalJSON` hook, user concrete types) monomorphizes:
`JSONSize`/`AppendJSON` devirtualize, value not boxed. Caveat: can't be a bare
func value — `f := encode.Marshal` needs `encode.Marshal[T]`. An already-boxed
`Marshaler` still works (T = the interface). `WriteTo` / `WriteSliceTo` stay
NON-generic interface-arg: pooled-buffer + `io.Write` cost dwarfs one box, and
presizing their buffer from `JSONSize()` was rejected (pool converges to max
payload size, so the size walk is pure overhead).

## `MarshalSlice` / `AppendSlice` — per-item sizing, nil pointers

Output presized from `sliceJSONSize` = SUM of each item's `JSONSize()`, not
`len*zero.JSONSize()` (the zero size is only the constant-folded base; populated
items would undersize and run the growth chain). A nil ITEMS slice emits `null`, empty non-nil `[]` (stdlib
parity). Pointer- and interface-typed `T`: one-time
`reflect.TypeFor[T]().Kind()` probe + per-item nil check (nil interface or
typed-nil pointer inside it), nil elements emit `null` (stdlib parity; without
it the walkers panic on the nil element's promoted `JSONSize()`; nil slice/map
HEADERS inside an interface still call their own AppendJSON — their emitters
own nil semantics). Pinned by `TestMarshalSlicePointerElems` /
`TestMarshalSliceSingleAlloc`.

## Error propagation

`AppendJSON` returns `([]byte, error)`; errors propagate from any nested encoder
that can fail (nested AppendJSON, TextAppender, TextMarshaler, JSONMarshaler,
`encoding/json.Marshal` fallback), threaded through every nested call.
Pure-primitive structs declare `var err error; _ = err` (compiler elides).

## `JSONSize()` — upper-bound overshoot

Intentional overshoot. Map per-entry: `4 + 2*len(key) + value-bound`
(kind-derived) or flat 128 for variable values. String = `len*2+2` (short-escape
worst-case: `\n \" \\ \t \b \f`, each byte → 2). Constant per-field contributions
fold into `size := N` at codegen; only loops/`len()` emit runtime adds.
Pure-primitive structs collapse to `return N`.

**Pathological corner**: control chars below 0x20 with no short escape expand to
`\uXXXX` (6× per byte) and DO overflow the bound — one-time realloc on that input
accepted (real payloads rarely have raw control bytes). Cap guarantee on
realistic worst-case pinned by `TestJSONSize_NoReallocOnWorstCase` (integrationtests/).

## `AppendString` / `AppendStringNoHTML`

Both write **escaped body + closing `"`**; CALLER writes opening `"` (codegen
folds into the `"key":"` prefix at struct top level, else emits explicit `dst =
append(dst, '"')` at slice/map/standalone sites).

- `AppendString` — HTML-safe, escapes `<`, `>`, `&` to `\uXXXX` (stdlib v1).
  Codegen routes here on `htmlescape` / `-htmlescape`.
- `AppendStringNoHTML` — default, standard escapes only, emits `<`, `>`, `&`
  literally (stdlib jsonv2).

**Neither validates UTF-8** — invalid bytes are emitted raw (invalid-UTF-8
JSON), where stdlib v1 replaces with U+FFFD and jsonv2 replaces + errors.
Deliberate: skipping validation avoids the per-rune DecodeRune walk (jsontext's
`AppendQuote` is 2.6× slower on non-ASCII because of it — see
`bench/stdappend_test.go`); divergence only fires on already-corrupt input,
i.e. the caller's own strings. NOTE the DECODE side is the opposite: it
REJECTS invalid UTF-8 with `scan.ErrInvalidUTF8` (jsonv2 parity — see
scan/CLAUDE.md). Documented as a README/SKILL.md pitfall. Known v1-parity hole in `AppendString`:
v1 defaults also include `EscapeForJS` (U+2028/U+2029 → `\u2028`/`\u2029`); we
emit those runes raw. Both forms are legal JSON — wire bytes just differ.

**SIMD tiers** (`simd_amd64.go`, `//go:build goexperiment.simd`):
`AppendString{,NoHTML}{AVX,AVX2,AVX512}` — same caller contract, fused vector
needs-escape classify per 16/32/64 bytes (Equal `"`/`\` + ctrl via
min(v,0x1F)==v below 512-bit / native unsigned Less + scalar-register mask OR
at 512; HTML variants add Equal `<`/`>`/`&`), set bits iterated `m &= m-1`
with clean spans bulk-appended between them. **Length-gated:** strings shorter
than one lane delegate straight to the scalar walk — without the gate the
broadcast setup + call shape REGRESSED every repo marshal bench (Mega +6.5%,
Tiny +14%); gated they are macro-flat and ~3.6×/10× faster at 64 B/≥256 B
(BenchmarkEscapeScan). The sub-lane tail is vectorized by an **overlapping
reload** of the last full lane (`s[len-lane:]`, always in bounds behind the
length gate) whose mask is right-shifted by `lane-rem`, dropping the bits for
bytes the main loop already emitted — simdjson's builder trick, and it needs no
caller-buffer padding. It replaced a per-byte table walk over up to lane-1
bytes: −49% on the 2800 B `BenchmarkEscapeScan/avx512` row (rem 48) and −37…−77%
across `BenchmarkEscapeTailRem` (rem 16…63); rem 0 unaffected. Macro is
untouched by construction — sub-lane strings never reach these functions, and
no repo marshal bench carries ≥64 B strings. `Load*SlicePart` stays unused: it
is a real CALL and its zero padding would classify as ctrl and emit spurious
escapes. Overlap correctness (a byte classified in BOTH the main loop and the
reload must be emitted exactly once) is pinned exhaustively by
`TestAppendStringSIMD_OverlapTailParity` — every escape byte at every position
for every length in [lane, 3·lane], all six tiers. ggen emits the tier names when run
under `-simd` (shared `simdSuffix`, see cli/CLAUDE.md opt #46); no runtime
probing. Byte-parity pinned by `TestAppendStringSIMD_Parity` (lane-seam
directed cases + 3000 randomized bodies, all six functions).

The hot-scan escape test is a `[256]bool` table lookup (`needEscapeNoHTML` /
`needEscapeHTML`), not a comparison chain: the table wins because one independent
L1 load pipelines across bytes, beating the dependent `&&` chain (3-deep NoHTML /
6-deep HTML). (A branchless uint64 register-bitmap was tried and is slower; see
backlog Tried Rejected.) `TestAppendString_TableParity` pins byte-parity vs a
comparison-chain reference over all 256 bytes.

## `AppendAny` — runtime walker for `any` fields

Type-switches over runtime primitives, homogeneous primitive slices/maps, and a
small set of concrete stdlib types **before** falling to reflection.

### Escaping

One walker (`appendAny`) parameterized by `escapeFn` (`AppendString` /
`AppendStringNoHTML`) threaded through every helper, so nested strings AND map
keys escape consistently. `AppendAny` defaults to NoHTML (jsonv2 parity),
`AppendAnyHTML` is the v1 variant; codegen picks via `appendAnyFn(f.HTMLEscape)`.
Pinned by `TestAppendAny_NoHTMLEscapeDefault`.

### Switch ordering rules (don't break)

Case order is load-bearing — concrete cases MUST precede the interface dispatches
that would otherwise catch them:

1. **Concrete primitives** (`string`, `bool`, `int*`, `uint*`, `float*`, `nil`).
2. **Homogeneous primitive slices** (`[]int*`, `[]uint16/32/64`, `[]float*`,
   `[]bool`, `[]string`, `[]any`). Skip `[]uint8` — that's `[]byte`, stays on the
   base64 reflect.Slice path. Plus two concrete COMPOSITE-element slices
   (`[]time.Time`, `[]json.RawMessage`) handled wholesale so elements skip the
   reflect.Slice per-element `rv.Interface()` box.
3. **Homogeneous string-keyed primitive maps** (`map[string]int*`/`uint*`/
   `float*`/`bool`/`string`/`any`) → generic helpers (`appendMapInt[V]`,
   `appendSliceFloat[V]`, …): one strconv per entry, no reflect.
4. **Concrete stdlib hooks** — `json.RawMessage` (verbatim), `time.Time`
   (AppendText), pointer-to-primitive (`*string`/`*bool`/`*int*`/`*uint*`/
   `*float*`, nil → `null`). MUST sit before `case json.Marshaler`.
5. **Interface fallbacks** — ggen `Marshaler` / `json.Marshaler` /
   `encoding.TextAppender` / `encoding.TextMarshaler`.
6. **Reflection** — slices/arrays/maps/pointers/structs (json-tag parsing for
   struct walking), keeping nested ggen `Marshaler` / `TextAppender` on the fast
   path. The `reflect.Map` walk reuses two addressable scratch `reflect.Value`s
   via `Value.SetIterKey`/`SetIterValue` (vs `iter.Key()`/`iter.Value()` which
   allocate a fresh Value per entry). Only `any` fields reach this.

### `usenumber` mode

Decode emits `scan.AnyNumber` (numbers → `json.Number` aliased over input via
`unsafe.String`); encode-side `json.Number` is a string newtype handled by the
standard string case.

### Adding a new concrete case

Place **before** the matching interface case. `time.Time` implements
`json.Marshaler`, so concrete `time.Time` precedes `case json.Marshaler`;
pointer-to-primitive precedes `case reflect.Pointer` (else the fallback boxes via
`rv.Elem().Interface()`).

## Tests

- `appendany_test.go` — `AppendAny` correctness + `BenchmarkAppendAny` /
  `_Presized`. Next to the impl for unexported-symbol access.
- `url_test.go` — net/url.URL encode helpers.
