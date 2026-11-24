# decode — Decoder[T] interface + bytes array walkers

The BYTES-path `Decoder[T]` interface every generated struct satisfies, plus
the bytes slice walkers — otherwise toil to reimplement per call site. All of
it is in the root package `ggen`. The STREAM side is `ggen.StreamDecoder[T]`
plus the generic methods `(*ggen.Stream).Value` / `.Slice` / `.Array` / `.Seq`
in `stream.go` (.claude/scan.md), including the optional `rcv` buffer-reuse
receivers.

## `decode.go`

```go
type Decoder[T any] interface {
    DecodeFrom(data []byte) (T, int, error)             // returns bytes consumed
}

type Unmarshaler[T any] interface {                     // both paths
    Decoder[T]
    StreamDecoder[T]
}

// Bytes array walkers — callers would otherwise reimplement the bracket/comma/
// element-dispatch loop.
func UnmarshalSlice[T Decoder[T]](data []byte) ([]T, error)
func ReadSlice[T Decoder[T]](r io.Reader) ([]T, error)   // io.ReadAll + UnmarshalSlice
```

`Decoder` is bytes-ONLY, so a helper constrains on just the path it walks.
`ggen.StreamDecoder[T]` (`stream.go`) is the streaming counterpart, the
constraint of the `Stream` methods. Generated structs satisfy both, and
`Unmarshaler[T]` requires both.

Streaming an array is `ggen.NewStream(r, buf).Slice[T]()`, which leaves the
Stream positioned so the caller keeps reading after the array. Unlike
`UnmarshalSlice`, its element errors carry no `[N]` path segment. They are
already a `*ParseError` from the inner decoder.

The slice-prealloc ladder is `prealloc.Cap` (`internal/prealloc`), called by
`UnmarshalSlice` and the `Slice` method.
Generated code does not call it at all — the emitter inlines the ladder as a
CONSTANT EXPRESSION (see cmd/ggen/CLAUDE.md).

## `mod_error.go`

```go
type ModError struct { Pos int; Name, Msg string; Value any }
func (e *ModError) Error() string
```

Failure of a fallible bool-form mod/converter (`func(W) (T, bool)`) whose bool
was false. It is a **parse error**, not a validation rule failure, and is
wrapped by `NewParseErr`. `Msg` is the optional inline tag
message (empty renders a default). Emitted at every bool-form mod site
(`renderOneMod` / the `variants.go` converter path).

## `parse_error.go`

```go
type ParseError struct { Keys []string; Pos int; Err error; head *pathBuf }
func (e *ParseError) Error() string
func (e *ParseError) Unwrap() error
func (e *ParseError) Path() []string    // Keys
func (e *ParseError) Message() string   // the cause without path and position

func NewParseErr(segment string, pos int, err error) error
func NewParseErrShift(segment string, pos, n int, err error) error
func NewParseErrOpaque(segment string, pos int, err error) error
```

`ParseError` is what every generated `DecodeFrom` / `DecodeFromStream` returns for raw parse failures. `Keys` = the JSON keys from the root to the failing value (a container level folds into its field's entry, `tags[]` / `byID.value`, only `UnmarshalSlice` / `ReadSlice` add `[N]`), `Error()` joins them with dots. `Pos` = the byte offset where scanning STOPPED: into the data slice passed to the top-level `DecodeFrom` (nested sites rebase via `NewParseErrShift`), and `s.Offset()` (consumed + `s.Pos`) for `DecodeFromStream` — the same byte on the bytes and stream paths at any chunk size, since every stream primitive rebases onto its bytes twin's stop position (.claude/scan.md) — `Err` = the underlying `ggen.ErrX` sentinel. `errors.Is(err, ggen.ErrBadString)` works via `Unwrap()`.

`NewParseErrShift(segment, pos, n, err)` is the nested-decode variant: the callee consumed `n` bytes of `data[pos-n:]`, so it adds `pos-n` to every positional error the callee returned (`shiftPos` over `*ParseError` and its cause, `ModError`, `Errors`, the typed validation errors, and through `Unwrap` into whatever a hand-written decoder wrapped with `%w` or `errors.Join`) before wrapping, because the callee ran on a sub-slice. Bytes-path nested-decode sites use it (cmd/ggen/CLAUDE.md opt #14); on the stream path only the `UnmarshalJSON` rungs (cross-package field and struct alias) do, since their callee runs on a captured span rather than on the live buffer.

`NewParseErrOpaque(segment, pos, err)` wraps err as `&ParseError{Keys: {field}, Pos, Err: err}` without looking inside. Codegen uses it after callees that are not ggen decoders of this value (the `encoding/json` fallback and the embed catch-all's, `UnmarshalText`, error-form `@Conv` converters and fallible `@Func` mods): a ggen error they return (encoding/json handing up a nested generated `UnmarshalJSON`'s error unchanged, a converter that decodes JSON carried in a string) has Pos and Keys relative to an input the host cannot place, so adopting it would report an inner-span offset and a path missing the levels between.

`NewParseErr` is the call-site constructor at every error-return site in generated decoders. Codegen embeds `segment` as a compile-time literal per branch (`"street"`, `"addr"`, …) or as a runtime expression for dynamic keys (`key` in the bytes path — aliased into the caller's data, safe on the error path; `strings.Clone(key)` for the stream path, since the underlying buffer may have compacted). Behaviour:

- nil err → nil (zero-cost happy path — no allocation, no field-name probe)
- `ggen.ValidationError` / `ggen.Errors` (cut to its first entry) → prepend the segment onto the Keys (typed pointers stay reachable via `errors.As` — the value passes through, only its Keys grow, so nested fail-fast validation errors keep their outer segments)
- already a `*ParseError` (deeper wrap) → **mutates** its `Keys`, prepending the outer field name into the free slots in front of `head` (amortized O(1) per level, the buffer doubles when full) (`"addr"` + inner `"zip"` → `"addr.zip"`); `Pos` left at the deeper site. A `*ParseError` found only through a wrapper's `Unwrap` chain is extended the same way and the wrapper itself is returned, so a hand-written decoder's own error type stays reachable (`TestParseErrShiftReachesWrappedErrors`)
- raw error → wraps as `&ParseError{Keys, Pos, Err: err}`

`Error()` renders `parse error[ at <field>] (pos <n>): <cause>` from a preallocated `[]byte` returned as an `unsafe.String` alias; the cause's `Error()` is called exactly once and appended directly, so chained wraps stay linear.

`UnmarshalSlice` wraps its OWN bracket/comma `ggen.ErrBadArray` failures in `*ParseError`, including input that ends right after `[` (the stream twins' drained-read `ErrBadArray`, pinned by `TestSliceHelpersTruncatedAfterOpen`); element-level errors come back already wrapped from the inner `DecodeFrom`.

`UnmarshalSlice` (and `ReadSlice` through it) rejects trailing non-whitespace after the closing `]` with `ggen.ErrTrailingData`, so `[1,2]]]` is detectable (jsonv2 whole-input parity). It preallocates via the `prealloc.Cap` ladder, and returns a NON-nil empty slice for `[]` so the result is distinguishable from `null` (generated slice fields and jsonv2 agree) — pinned by `TestUnmarshalSliceEmptyNonNil`. `null` (what `MarshalSlice` writes for a nil slice) decodes to a nil slice; `Stream.Slice` and `Stream.Array` (an empty range) read it the same way through `arrayOpenOrNull`, and an `n`-prefixed non-literal is `ErrBadLiteral` on both paths at the `litEnd` give-up position — pinned by `TestSliceHelpersNull`.
