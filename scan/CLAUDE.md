# scan — hand-rolled JSON scanner + streaming Stream

Runtime package. Bytes-path primitives + a streaming `Stream` type. All
generated decode methods call into this directly. No tokenizer, no AST, no
reflection.

## Bytes path (`scan/scan.go`)

`[]byte`-based primitives operating on `(data, pos)`, returning
`(value, newPos, error)` or `(newPos, error)`. Zero-alloc on the happy path,
sentinel errors (`scan.ErrBadObject` etc.).

- `SkipSpace`, `String`, `Int64`, `Uint64`, `Float64`, `Bool`, `ObjectOpen`,
  `ArrayOpen`, `SkipValue`.
- `String()` uses `unsafe.String(unsafe.SliceData(data[start:]), len)` for
  zero-copy aliasing when no escapes. Falls back to `stringSlow` with
  `utf8.AppendRune` for `\uXXXX` + surrogates.
- `bytes.IndexByte` (SIMD) locates the closing `"`; a second IndexByte over the
  span detects any preceding `\`. Truncated `\u…`/trailing `\` surfaces as
  `ErrBadString` via fallthrough to `stringSlow`.

## Stream (`scan/stream.go`)

`Stream` wraps `io.Reader` with a growable internal buffer (`buf []byte`, grown
via `append`). The cursor is the exported `Pos int` field — every scan
primitive (`s.SkipSpace()`, `s.String()`, `s.KeyView()`, `s.Int64()`,
`s.SkipValue()`, …) reads from `s.Pos` and writes it back. Methods take no
cursor argument and never return one. Capturing a raw span:

```go
start := s.Pos
s.SkipValue()
raw := s.Bytes()[start:s.Pos]
```

### `ReadMore(keep int) error` — the only I/O primitive

One Read call per invocation, never loops. `keep` is the lowest offset the
caller still needs; bytes before it may be discarded:

- `keep == 0` — grow without shifting (bigger backing if full). Offsets stable,
  aliases survive.
- `keep == len(buf)` — reset to `[:0]`, refill from 0 (full compaction).
- `0 < keep < len(buf)` — in-place `memmove` of `buf[keep:n]` down to
  `buf[0:n-keep]`, read into the freed tail. **Aliases into the buffer are
  invalidated whenever `keep > 0`** — bytes physically move on the same backing.

### Aggressive compaction inside Stream methods

`SkipSpace`, `ConsumeColon`, `Int64`/`Uint64`, `String`/`KeyView` pass a
non-zero `keep` (current cursor, or value-start `start` for spans that outlast
the loop) so the buffer stays bounded at ~`max(chunk_size, value_size)` across
long streams. Each updates its locals after the shift (`i = 0`, or
`j -= start; start = 0` for the string body), then writes the final `s.Pos`.

The `Shift` field (defaults true via `Reset`) flips off around `SkipValue` in
RawJSON capture and `json.Unmarshal` fallback spans, where generated code needs
stable absolute offsets to slice `s.Bytes()[start:s.Pos]`. Bookkeeping branches
check `s.Shift` before resetting the cursor.

### Dispatch-loop shift points

Generated code adds two more shift points at the dispatch-loop boundary:
`ReadMore(s.Pos); s.Pos = 0` after `ObjectOpen+SkipSpace`, and after the
per-iteration value decode + SkipSpace. Each known-key case opens with
`s.ConsumeColon()` — the `KeyView` alias is no longer needed past dispatch, so
the shift it triggers is safe. `UnknownKeyError` and the inline-catch-all map
key both detach the alias with `strings.Clone(key)`.

### Lazy bounds checks (one-byte-at-a-time refill)

Each method does its own bounds check (`if s.Pos >= len(s.buf) { ... ReadMore
(s.Pos) ... }`) and proceeds once **one** new byte lands. Multi-byte literals
(`true`, `false`, `null`, `\uXXXX`) scan **byte-by-byte**: each char triggers an
individual check + maybe ReadMore, a mismatch fails fast without fetching the
rest. This is the lazy-streaming property — parse what you have, fetch one chunk
only when truly stuck. (The old `Ensure`/`Anchor`/`Unanchor` bulk-fetch design
is in `.claude/backlog.md` "tried and rejected".)

### Stack-allocatable, no pool

```go
var s scan.Stream
s.Reset(r, buf)
```

Caller owns `buf` lifecycle. `scan.NewStream(r, buf)` is the heap-allocating
shorthand. The old `Acquire`/`Release` `sync.Pool` was removed — it bundled
implicit buffer-lifetime assumptions and caused silent corruption when callers
reused buf across decodes (see backlog).

### Stream copies vs bytes-path aliases

`Stream.String` and `Stream.Number` **copy** content via
`string(s.buf[start:end])` rather than aliasing — owned values, safe as map
keys, output detached from buf. The bytes path still aliases (caller owns input
there). Streaming traded ~230K extra allocs/Mega for safety + simplicity.

### `KeyView` — alias for dispatch keys

Sibling of `Stream.String` that aliases via `unsafe.String(unsafe.SliceData
(s.buf[start:]), end-start)` on the happy path (no escapes), falls back to
`stringSlow`. Used in generated object-field dispatch where the key is read,
matched, then discarded — the alias never escapes the dispatch frame. Drops
~200 throwaway heap strings per decoded value to zero.

The alias survives buffer growth (GC pins the old backing once a string header
references it), but a non-zero `keep` in subsequent `ReadMore` WILL move bytes
and corrupt live aliases — see "aggressive compaction". Dispatch sites detach
via `strings.Clone` before any shift-triggering path (UnknownKeyError,
inline-catch-all map key).

## Design rationale

- **`unsafe.String` aliases are safe across buffer growth.** Go's GC is
  non-moving (mark-and-sweep, no compaction). An alias into the OLD backing
  keeps it live — GC walks string headers. Stream can `append`-grow freely;
  prior aliases stay valid.
- **`Stream` is stack-allocatable; no pool.** See above.

## Tests

- `scan/any_test.go` — `Any`/`AnyNumber` stdlib parity + `BenchmarkAny_Shapes`.
- `scan/string_test.go`, `scan/number_test.go`, `scan/stream_test.go` —
  primitive correctness + edge cases.
