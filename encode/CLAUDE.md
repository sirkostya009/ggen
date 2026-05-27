# encode — Marshaler interface + AppendJSON helpers

Runtime package. Defines the `Marshaler` interface that every
generated struct method satisfies, plus the buffer-append helpers
generated code calls into and the `AppendAny` runtime walker for
`any` / `interface{}` fields.

## Files

- `encode/encode.go` — Marshaler interface, `Marshal`/`MarshalString`/
  `Write` + slice variants, `BytesToString`.
- `encode/string.go` — `AppendString` (HTML-safe) and
  `AppendStringNoHTML` (stdlib jsonv2 default).
- `encode/any.go` — `AppendAny` runtime walker with concrete-type
  fast paths.
- `encode/url.go` — net/url.URL helpers used by generated code.

## Surface

```go
type Marshaler interface {
    AppendJSON(dst []byte) ([]byte, error)
    JSONSize() int
}

func Marshal[T] (v Marshaler) ([]byte, error)
func MarshalString[T] (v Marshaler) (string, error)
func Write(w io.Writer, v Marshaler) error
func MarshalSlice / MarshalSliceString / WriteSlice / AppendSlice  // []T of Marshaler

func BytesToString(buf []byte) string                              // unsafe.String over buffer
func AppendString(dst []byte, s string) []byte                     // HTML-safe variant
func AppendStringNoHTML(dst []byte, s string) []byte               // jsonv2-default variant
```

### `Marshal[T]`

`append` into `make([]byte, 0, v.JSONSize())`. Single alloc on the
happy path.

### `MarshalString[T]`

Same as `Marshal` but with `BytesToString` aliasing the buffer
(zero extra alloc).

### `Write`

Pooled buffer, returns first non-nil error from AppendJSON or the
writer.

### Slice variants

`MarshalSlice` / `MarshalSliceString` / `WriteSlice` / `AppendSlice`
— error-returning equivalents for `[]T` of `Marshaler`s.

## Error propagation

`AppendJSON` returns `([]byte, error)`. Errors propagate from any
nested encoder that can fail: nested AppendJSON, TextAppender,
TextMarshaler, JSONMarshaler, `encoding/json.Marshal` fallback.

Generator threads errors through every nested call. Pure-primitive
structs declare `var err error; _ = err` and never use it; the
compiler elides the variable so there's no runtime cost when
nothing can fail.

## `JSONSize()` — upper-bound overshoot

Intentional overshoot, not a tight fit. Map estimate is per-entry:
`4 + 2*len(key) + value-bound` (kind-derived) or flat 128 for
variable values. String is `len*2+2` for short-escape worst-case
(`\n`, `\"`, `\\`, `\t`, `\b`, `\f` — every byte becomes 2).

**Pathological corner**: control chars below 0x20 with no short
escape expand to `\uXXXX` (6× per byte) and DO overflow the
bound. The one-time realloc on that input is accepted — real-
world payloads rarely contain raw control bytes.
`TestJSONSize_NoReallocOnWorstCase` (in `integrationtests/`)
pins the cap-guarantee on realistic worst-case input.

Constant per-field contributions are folded into the initial
`size := N` at codegen time; only loops/`len()` emit runtime
adds. Pure-primitive structs collapse to a single `return N`.

## `AppendString` / `AppendStringNoHTML`

Both write the **escaped string body + closing `"`**. The CALLER
is responsible for the opening `"` — codegen folds it into the
constant `"key":"` prefix at struct-field top level, or emits an
explicit `dst = append(dst, '"')` at slice/map/standalone sites.

- `AppendString` — HTML-safe variant. Escapes `<`, `>`, `&` to
  `\uXXXX`. Matches stdlib v1. Codegen routes here when the parent
  struct opts in via `htmlescape` annotation or `-htmlescape` flag.
- `AppendStringNoHTML` — default variant. Standard JSON escapes
  only, emits `<`, `>`, `&` literally. Matches stdlib jsonv2
  (which dropped HTML escaping as a default).

## `AppendAny` — runtime walker for `any` fields

Type-switches over runtime primitives, homogeneous primitive
slices/maps, and a small set of concrete stdlib types **before**
falling into reflection. Each concrete case avoids a reflection
allocation hop.

### Switch ordering rules (don't break these)

Order of cases in the type switch is load-bearing for performance:

1. **Concrete primitives** (`string`, `bool`, `int*`, `uint*`,
   `float*`, `nil`).
2. **Homogeneous primitive slices** (`[]int*`, `[]uint16/32/64`,
   `[]float*`, `[]bool`, `[]string`, `[]any`).
   Skip `[]uint8` — that's `[]byte`, which must stay on the
   base64 reflect.Slice path.
3. **Homogeneous string-keyed primitive maps** (`map[string]int*`,
   `map[string]uint*`, `map[string]float*`, `map[string]bool`,
   `map[string]string`, `map[string]any`). These dispatch to
   generic helpers (`appendMapInt[V]`, `appendSliceFloat[V]`, …)
   so the body is one strconv call per entry, no reflect
   overhead.
4. **Concrete stdlib hooks** — `json.RawMessage` (verbatim
   passthrough), `time.Time` (AppendText), pointer-to-primitive
   (`*string`, `*bool`, `*int*`, `*uint*`, `*float*` — nil → `null`).
   These sit **before** the `case json.Marshaler` block so they
   win the type switch over the interface dispatch.
5. **Interface fallbacks** — ggen `Marshaler` / `json.Marshaler`
   / `encoding.TextAppender` / `encoding.TextMarshaler`.
6. **Reflection** — slices/arrays/maps/pointers/structs (with
   json-tag parsing for struct walking), keeping nested ggen
   `Marshaler` / `TextAppender` types on the fast path with no
   `json.Marshal` cliff.

### `usenumber` mode

With the `usenumber` flag/anno, the generator emits
`scan.AnyNumber` for decode (numbers → `json.Number` aliased over
the input via `unsafe.String`, zero-alloc/zero-copy on the happy
path); encode-side `json.Number` is a string newtype that
`AppendAny` handles via the standard string case.

### Adding a new concrete case

When adding a fast-path case, place it **before** the matching
interface case. For example: `time.Time` implements
`json.Marshaler` via its `MarshalJSON`, so the concrete `time.Time`
case must come before `case json.Marshaler`. Likewise pointer-to-
primitive cases must come before `case reflect.Pointer` (since the
reflect.Pointer fallback would otherwise box via
`rv.Elem().Interface()`).

## Tests

- `encode/appendany_test.go` — `AppendAny` correctness + per-
  shape `BenchmarkAppendAny` / `BenchmarkAppendAny_Presized`.
  Lives next to the implementation, not in `integrationtests/`,
  so it has direct unexported-symbol access and runs without
  the integrationtests module setup.
- `encode/url_test.go` — net/url.URL encode helpers.
