# encode — Marshaler interface + AppendJSON helpers

Runtime package. Defines the `Marshaler` interface every generated struct
satisfies, plus the buffer-append helpers generated code calls and the
`AppendAny` runtime walker for `any` / `interface{}` fields.

## Files

- `encode/encode.go` — Marshaler interface, `Marshal`/`MarshalString`/`Write` +
  slice variants, `BytesToString`.
- `encode/string.go` — `AppendString` (HTML-safe) + `AppendStringNoHTML`
  (jsonv2 default).
- `encode/any.go` — `AppendAny` runtime walker with concrete-type fast paths.
- `encode/url.go` — net/url.URL helpers used by generated code.

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
encoder that can fail (nested AppendJSON, TextAppender, TextMarshaler,
JSONMarshaler, `encoding/json.Marshal` fallback). The generator threads errors
through every nested call. Pure-primitive structs declare `var err error; _ =
err` (compiler elides it — no runtime cost).

## `JSONSize()` — upper-bound overshoot

Intentional overshoot, not a tight fit. Map estimate is per-entry: `4 +
2*len(key) + value-bound` (kind-derived) or flat 128 for variable values.
String is `len*2+2` for short-escape worst-case (`\n`, `\"`, `\\`, `\t`, `\b`,
`\f` — every byte becomes 2).

**Pathological corner**: control chars below 0x20 with no short escape expand to
`\uXXXX` (6× per byte) and DO overflow the bound. The one-time realloc on that
input is accepted — real payloads rarely contain raw control bytes.
`TestJSONSize_NoReallocOnWorstCase` (in `integrationtests/`) pins the
cap-guarantee on realistic worst-case input.

Constant per-field contributions are folded into the initial `size := N` at
codegen time; only loops/`len()` emit runtime adds. Pure-primitive structs
collapse to `return N`.

## `AppendString` / `AppendStringNoHTML`

Both write the **escaped body + closing `"`**. The CALLER writes the opening
`"` — codegen folds it into the constant `"key":"` prefix at struct-field top
level, or emits an explicit `dst = append(dst, '"')` at slice/map/standalone
sites.

- `AppendString` — HTML-safe, escapes `<`, `>`, `&` to `\uXXXX` (matches stdlib
  v1). Codegen routes here when the parent opts in via `htmlescape` / `-htmlescape`.
- `AppendStringNoHTML` — default, standard JSON escapes only, emits `<`, `>`,
  `&` literally (matches stdlib jsonv2).

## `AppendAny` — runtime walker for `any` fields

Type-switches over runtime primitives, homogeneous primitive slices/maps, and a
small set of concrete stdlib types **before** falling into reflection.

### Switch ordering rules (don't break these)

Case order is load-bearing for performance:

1. **Concrete primitives** (`string`, `bool`, `int*`, `uint*`, `float*`, `nil`).
2. **Homogeneous primitive slices** (`[]int*`, `[]uint16/32/64`, `[]float*`,
   `[]bool`, `[]string`, `[]any`). Skip `[]uint8` — that's `[]byte`, must stay
   on the base64 reflect.Slice path.
3. **Homogeneous string-keyed primitive maps** (`map[string]int*`/`uint*`/
   `float*`/`bool`/`string`/`any`) → generic helpers (`appendMapInt[V]`,
   `appendSliceFloat[V]`, …): one strconv call per entry, no reflect.
4. **Concrete stdlib hooks** — `json.RawMessage` (verbatim), `time.Time`
   (AppendText), pointer-to-primitive (`*string`/`*bool`/`*int*`/`*uint*`/
   `*float*`, nil → `null`). These sit **before** `case json.Marshaler` so they
   win the switch over interface dispatch.
5. **Interface fallbacks** — ggen `Marshaler` / `json.Marshaler` /
   `encoding.TextAppender` / `encoding.TextMarshaler`.
6. **Reflection** — slices/arrays/maps/pointers/structs (with json-tag parsing
   for struct walking), keeping nested ggen `Marshaler` / `TextAppender` on the
   fast path with no `json.Marshal` cliff.

### `usenumber` mode

With `usenumber`, the generator emits `scan.AnyNumber` for decode (numbers →
`json.Number` aliased over input via `unsafe.String`, zero-alloc/zero-copy
happy path); encode-side `json.Number` is a string newtype handled by the
standard string case.

### Adding a new concrete case

Place it **before** the matching interface case. `time.Time` implements
`json.Marshaler`, so the concrete `time.Time` case must precede `case
json.Marshaler`; pointer-to-primitive cases must precede `case reflect.Pointer`
(else the fallback boxes via `rv.Elem().Interface()`).

## Tests

- `encode/appendany_test.go` — `AppendAny` correctness + `BenchmarkAppendAny` /
  `BenchmarkAppendAny_Presized`. Lives next to the implementation for direct
  unexported-symbol access without the integrationtests module setup.
- `encode/url_test.go` — net/url.URL encode helpers.
