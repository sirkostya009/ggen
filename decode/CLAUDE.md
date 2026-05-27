# decode — Decoder[T] interface + generic array walkers

Runtime package. Defines the `Decoder[T]` interface that every
generated struct method satisfies, plus generic slice walkers that
would otherwise be toil to reimplement at every call site.

## `decode/decode.go`

```go
type Decoder[T any] interface {
    DecodeFrom(data []byte) (T, int, error)             // returns bytes consumed
    DecodeStreamFrom(s *scan.Stream) (T, error)         // Stream owns cursor via s.Pos
}

// Array walkers — callers would otherwise have to reimplement the
// bracket / comma / element-dispatch loop.
func UnmarshalSlice[T Decoder[T]](data []byte) ([]T, error)
func ReadSlice[T Decoder[T]](r io.Reader) ([]T, error)               // io.ReadAll + UnmarshalSlice
func UnmarshalSliceStream[T Decoder[T]](r io.Reader, buf []byte) ([]T, []byte, error)
```

## Why no single-value entry points

Single-value wrappers (`Unmarshal[T]`, `Read[T]`, `UnmarshalStream[T]`,
`UnmarshalStreamRequest`, `UnmarshalStreamResponse`) were removed
because they were direct passthroughs to the generated method,
and the package surface is honest about what the user actually
pays for.

The generated method is callable directly with a zero-value
receiver:

```go
// bytes path
v, _, err := T{}.DecodeFrom(data)

// streaming path
var s scan.Stream
s.Reset(r, buf)              // buf nil OK
v, err := T{}.DecodeStreamFrom(&s)
```

For struct types and slice/map/array aliases the receiver is the
one-liner composite literal `T{}`; primitive aliases need an
explicit zero (`AliasInt(0)`, `AliasString("")`, `AliasBool(false)`,
…) or a `var zero T`.

The array walkers stay because reimplementing the bracket/comma
loop everywhere is real toil.

## Cursor convention

The bytes-path `DecodeFrom` takes a slice starting at the value's
first byte and returns how many bytes were consumed; the caller
advances its own cursor (`i += n` after reslicing as `data[i:]`).
Generated nested-struct calls follow this pattern internally.

The stream-path `DecodeStreamFrom` doesn't take or return a
cursor at all — the cursor is `s.Pos`, owned by the Stream and
advanced in-place by every scan primitive. Generated code that
needs to capture a raw span snapshots `s.Pos` before and reads
it again after.

## `decode/validators.go`

Helper predicates used in generated validation branches. Each
predicate maps 1:1 to a rule name (`isASCII`, `isAlphanum`,
`isHexadecimal`, …). Kept in this package so generated code in
external user packages can call them without a separate import.
