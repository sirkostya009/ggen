# decode — Decoder[T] interface + bytes array walkers

Runtime package. Defines the BYTES-path `Decoder[T]` interface every generated
struct satisfies, plus the bytes slice walkers — otherwise toil to reimplement
per call site. The STREAM side lives in `scan`: `ggen.StreamDecoder[T]` plus the
generic methods `(*ggen.Stream).Value` / `.Slice` / `.Seq` (.claude/scan.md),
including the optional `rcv` buffer-reuse receivers.

## `decode.go`

```go
type Decoder[T any] interface {
    DecodeFrom(data []byte) (T, int, error)             // returns bytes consumed
}

// Bytes array walkers — callers would otherwise reimplement the bracket/comma/
// element-dispatch loop.
func UnmarshalSlice[T Decoder[T]](data []byte) ([]T, error)
func ReadSlice[T Decoder[T]](r io.Reader) ([]T, error)   // io.ReadAll + UnmarshalSlice
```

`Decoder` is bytes-ONLY, so a helper constrains on just the path it walks.
`ggen.StreamDecoder[T]` is the streaming counterpart, declared in `scan` because
the `Stream` methods constrain on it and `scan` cannot import `decode`.
Generated structs satisfy both; embed the pair to require both:

```go
type BothPaths[T any] interface {
    ggen.Decoder[T]
    ggen.StreamDecoder[T]
}
```

Streaming an array is `ggen.NewStream(r, buf).Slice[T]()`, which leaves the
Stream positioned so the caller keeps reading after the array. Its element
errors carry no `[N]` path segment — `NewParseErr` lives here and `scan` cannot
reach it — but they are already a `*ParseError` from the inner decoder.

The slice-prealloc ladder is `prealloc.Cap` (`internal/prealloc`), where the
`Slice`/`Seq` methods can reach it; `UnmarshalSlice` calls it directly.
Generated code does not call it at all — the emitter inlines the ladder as a
CONSTANT EXPRESSION (see cli/CLAUDE.md).

## `mod_error.go`

```go
type ModError struct { Pos int; Name, Msg string; Value any }
func (e *ModError) Error() string
```

Failure of a fallible bool-form mod/converter (`func(W) (T, bool)`) whose bool
was false. It is a **parse error** (not a validation rule failure — it lives here,
not in `validation`), wrapped by `NewParseErr`. `Msg` is the optional inline tag
message (empty renders a default). Emitted at every bool-form mod site
(`renderOneMod` / the `variants.go` converter path).

## `parse_error.go`

```go
type ParseError struct { Field string; Pos int; Err error }
func (e *ParseError) Error() string
func (e *ParseError) Unwrap() error

func NewParseErr(field string, pos int, err error) error
```

`ParseError` is what every generated `DecodeFrom` / `DecodeFromStream` returns for raw parse failures. `Field` = dotted JSON path through the document, `Pos` = byte offset within the data slice passed to the failing method, `Err` = the underlying `ggen.ErrX` sentinel. `errors.Is(err, ggen.ErrBadString)` works via `Unwrap()`.

`NewParseErr` is the call-site constructor at every error-return site in generated decoders. Codegen embeds `field` as a compile-time literal per branch (`"street"`, `"addr"`, …) or as a runtime expression for dynamic keys (`key` in the bytes path — aliased into the caller's data, safe on the error path; `strings.Clone(key)` for the stream path, since the underlying buffer may have compacted). Behaviour:

- nil err → nil (zero-cost happy path — no allocation, no field-name probe)
- `ggen.Error` / `ggen.Errors` → prepend the segment onto the Path (typed pointers stay reachable via `errors.As` — the value passes through, only its Path grows, so nested fail-fast validation errors keep their outer segments)
- already a `*ParseError` (deeper wrap) → **mutates** its `Field` in place, prepending the outer field name (`"addr"` + inner `"zip"` → `"addr.zip"`); `Pos` left at the deeper site
- raw error → wraps as `&ParseError{Field, Pos, Err: err}`

`Error()` renders `parse error[ at <field>] (pos <n>): <cause>` from a preallocated `[]byte` returned as an `unsafe.String` alias; the cause's `Error()` is called exactly once and appended directly, so chained wraps stay linear.

`UnmarshalSlice` wraps its OWN bracket/comma `ggen.ErrBadArray` failures in `*ParseError`; element-level errors come back already wrapped from the inner `DecodeFrom`.

`UnmarshalSlice` (and `ReadSlice` through it) rejects trailing non-whitespace after the closing `]` with `ggen.ErrTrailingData`, so `[1,2]]]` is detectable (jsonv2 whole-input parity). It preallocates via the `prealloc.Cap` ladder, and returns a NON-nil empty slice for `[]` so the result is distinguishable from `null` (generated slice fields and jsonv2 agree) — pinned by `TestUnmarshalSliceEmptyNonNil`.
