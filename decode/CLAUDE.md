# decode — Decoder[T] interface + generic array walkers

Runtime package. Defines `Decoder[T]` interface every generated struct satisfies, plus generic slice walkers — otherwise toil to reimplement per call site.

## `decode/decode.go`

```go
type Decoder[T any] interface {
    DecodeFrom(data []byte) (T, int, error)             // returns bytes consumed
    DecodeFromStream(s *scan.Stream) (T, error)         // Stream owns cursor via s.Pos
}

// Array walkers — callers would otherwise reimplement the bracket/comma/
// element-dispatch loop.
func UnmarshalSlice[T Decoder[T]](data []byte) ([]T, error)
func ReadSlice[T Decoder[T]](r io.Reader) ([]T, error)               // io.ReadAll + UnmarshalSlice
func UnmarshalSliceStream[T Decoder[T]](r io.Reader, buf []byte) ([]T, []byte, error)
```

## `decode/mod_error.go`

```go
type ModError struct { Pos int; Name, Msg string; Value any }
func (e *ModError) Error() string
```

Failure of a fallible bool-form mod/converter (`func(W) (T, bool)`) whose bool
was false. It is a **parse error** (not a validation rule failure — it lives here,
not in `validation`), wrapped by `NewParseErr`. `Msg` is the optional inline tag
message (empty renders a default). Emitted at every bool-form mod site
(`renderOneMod` / the `variants.go` converter path).

## `decode/parse_error.go`

```go
type ParseError struct { Field string; Pos int; Err error }
func (e *ParseError) Error() string
func (e *ParseError) Unwrap() error

func NewParseErr(field string, pos int, err error) error
```

`ParseError` is what every generated `DecodeFrom` / `DecodeFromStream` returns for raw parse failures. `Field` = dotted JSON path through the document, `Pos` = byte offset within the data slice passed to the failing method, `Err` = the underlying `scan.ErrX` sentinel. `errors.Is(err, scan.ErrBadString)` works via `Unwrap()`.

`NewParseErr` is the call-site constructor at every error-return site in generated decoders. Codegen embeds `field` as a compile-time literal per branch (`"street"`, `"addr"`, …) or as a runtime expression for dynamic keys (`key` in the bytes path — aliased into the caller's data, safe on the error path; `strings.Clone(key)` for the stream path, since the underlying buffer may have compacted). Behaviour:

- nil err → nil (zero-cost happy path — no allocation, no field-name probe)
- `validation.Error` / `validation.Errors` → prepend the segment onto the Path (typed pointers stay reachable via `errors.As` — the value passes through, only its Path grows; used to pass untouched, which left nested fail-fast validation errors without their outer segments)
- already a `*ParseError` (deeper wrap) → **mutates** its `Field` in place, prepending the outer field name (`"addr"` + inner `"zip"` → `"addr.zip"`); `Pos` left at the deeper site
- raw error → wraps as `&ParseError{Field, Pos, Err: err}`

`Error()` renders `parse error[ at <field>] (pos <n>): <cause>` from a preallocated `[]byte` returned as an `unsafe.String` alias; the cause's `Error()` is called exactly once and appended directly, so chained wraps stay linear.

Slice walkers (`UnmarshalSlice` / `UnmarshalSliceStream`) wrap their OWN bracket/comma `scan.ErrBadArray` failures in `*ParseError`; element-level errors come back already wrapped from the inner `DecodeFrom`.

`UnmarshalSlice` (and `ReadSlice` through it) rejects trailing non-whitespace after the closing `]` with `scan.ErrTrailingData` (jsonv2 whole-input parity — `[1,2]]]` used to decode cleanly with no way to detect the remainder). `UnmarshalSliceStream` is exempt: probing for trailing bytes would block a live reader. The bytes walker also preallocates via the package's own `PreallocCap` ladder instead of growing from nil.

`UnmarshalSliceStream`'s comma branch used to do `s.Pos++; continue` straight into the next element's `DecodeFromStream` with no separator-whitespace skip — the bytes walker always `SkipSpace`s after its comma, but generated scalar/alias element decoders (`s.Int64()` etc.) don't skip leading whitespace themselves (only `ObjectOpen`-based struct decoders happen to), so `UnmarshalSliceStream[AliasInt](strings.NewReader("[1, 2]"), nil)` failed on the space where `UnmarshalSlice[AliasInt]([]byte("[1, 2]"))` decoded fine — same class as the 2026-07 stream `skipObject` comma-WS bug (scan/CLAUDE.md). Fixed: an `s.SkipSpace()` call after the comma, mirroring the bytes walker. Its error-position stamps also switched from `s.Pos` (buffer-relative — wrong once a compacting `ReadMore` slides the window, see `scan.Stream.Offset()` in scan/CLAUDE.md) to `s.Offset()`.
