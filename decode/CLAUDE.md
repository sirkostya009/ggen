# decode — Decoder[T] interface + generic array walkers

Runtime package. Defines the `Decoder[T]` interface every generated struct
satisfies, plus generic slice walkers that would otherwise be toil to
reimplement at every call site.

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

Helper predicates used in generated validation branches, each mapping 1:1 to a
rule name (`isASCII`, `isAlphanum`, `isHexadecimal`, …).
