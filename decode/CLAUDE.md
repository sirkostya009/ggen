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

## `decode/validators.go`

Helper predicates for generated validation branches. Each maps 1:1 to rule name (`isASCII`, `isAlphanum`, `isHexadecimal`, …).

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
- `validation.Error` / `validation.Errors` → pass through unchanged (typed pointers stay reachable via `errors.As`)
- already a `*ParseError` (deeper wrap) → **mutates** its `Field` in place, prepending the outer field name (`"addr"` + inner `"zip"` → `"addr.zip"`); `Pos` left at the deeper site
- raw error → wraps as `&ParseError{Field, Pos, Err: err}`

`Error()` renders `parse error[ at <field>] (pos <n>): <cause>` from a preallocated `[]byte` returned as an `unsafe.String` alias; the cause's `Error()` is called exactly once and appended directly, so chained wraps stay linear.

Slice walkers (`UnmarshalSlice` / `UnmarshalSliceStream`) wrap their OWN bracket/comma `scan.ErrBadArray` failures in `*ParseError`; element-level errors come back already wrapped from the inner `DecodeFrom`.
