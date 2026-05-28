# encode — Marshaler interface + AppendJSON helpers

Runtime package. Defines `Marshaler` interface every generated struct
satisfies, plus buffer-append helpers generated code calls and
`AppendAny` runtime walker for `any` / `interface{}` fields.

## Files

- `encode/encode.go` — Marshaler interface, `Marshal`/`MarshalString`/`Write` +
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

func Marshal[T] (v Marshaler) ([]byte, error)       // append into make([]byte,0,v.JSONSize()) — 1 alloc
func MarshalString[T] (v Marshaler) (string, error) // Marshal + BytesToString aliasing (no extra alloc)
func Write(w io.Writer, v Marshaler) error          // pooled buffer; first non-nil error
func AppendSlice[T Marshaler](dst []byte, items []T) ([]byte, error)
func MarshalSlice[T Marshaler](items []T) ([]byte, error)
func MarshalSliceString[T Marshaler](items []T) (string, error)
func WriteSlice[T Marshaler](w io.Writer, items []T) error

func BytesToString(buf []byte) string                // unsafe.String over buffer
func AppendString(dst []byte, s string) []byte       // HTML-safe variant
func AppendStringNoHTML(dst []byte, s string) []byte // jsonv2-default variant
```

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

## `AppendAny` — runtime walker for `any` fields

Type-switches over runtime primitives, homogeneous primitive slices/maps, and
small set of concrete stdlib types **before** falling to reflection.

### Switch ordering rules (don't break)

Case order load-bearing for perf:

1. **Concrete primitives** (`string`, `bool`, `int*`, `uint*`, `float*`, `nil`).
2. **Homogeneous primitive slices** (`[]int*`, `[]uint16/32/64`, `[]float*`,
   `[]bool`, `[]string`, `[]any`). Skip `[]uint8` — that's `[]byte`, must stay
   on base64 reflect.Slice path.
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
   fast path with no `json.Marshal` cliff.

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
