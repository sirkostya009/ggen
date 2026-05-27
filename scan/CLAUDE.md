# scan — hand-rolled JSON scanner + streaming Stream

Runtime package. Bytes-path primitives + a streaming `Stream` type.
All generated `Unmarshal`/`DecodeFrom`/`UnmarshalStream`/`DecodeStreamFrom`
methods call into this package directly. No tokenizer, no AST, no
reflection.

## Bytes path (`scan/scan.go`)

`[]byte`-based primitives — all operate on `(data, pos)`, return
`(value, newPos, error)` or just `(newPos, error)`. Zero-alloc on
the happy path. Sentinel errors (`scan.ErrBadObject` etc.).

- `SkipSpace`, `String`, `Int64`, `Uint64`, `Float64`, `Bool`,
  `ObjectOpen`, `ArrayOpen`, `SkipValue`.
- `String()` uses `unsafe.String(unsafe.SliceData(data[start:]), len)`
  for zero-copy aliasing when no escape sequences. Falls back to
  `stringSlow` with `utf8.AppendRune` for `\uXXXX` + surrogates.
- `bytes.IndexByte` (SIMD-accelerated by Go's runtime) locates the
  closing `"`; a second IndexByte over the span detects any
  preceding `\`. Truncated `\u…` or trailing `\` still surfaces as
  `ErrBadString` via fallthrough to `stringSlow`.

## Stream (`scan/stream.go`)

`Stream` wraps `io.Reader` with a growable internal buffer
(`buf []byte`, grown via `append`). The cursor lives in the
exported `Pos int` field — every scan primitive
(`s.SkipSpace()`, `s.String()`, `s.KeyView()`, `s.Int64()`,
`s.SkipValue()`, …) reads from `s.Pos` and writes it back before
returning. Methods take no cursor argument and never return one;
position state is in the Stream itself.

Generated code that needs to capture a raw span reads `s.Pos`
directly:

```go
start := s.Pos
s.SkipValue()
raw := s.Bytes()[start:s.Pos]
```

### `ReadMore(keep int) error` — the only I/O primitive

One Read call per invocation, never loops. `keep` is the lowest
offset the caller still needs — bytes before it are eligible for
discard:

- `keep == 0` grows without shifting (alloc bigger backing if
  currently full). Buffer offsets stay stable; aliases survive.
- `keep == len(buf)` resets the buffer to `[:0]` and refills from
  offset 0. Same effect as a full compaction.
- `0 < keep < len(buf)` performs an in-place `memmove` of
  `buf[keep:n]` down to `buf[0:n-keep]`, then reads into the freed
  tail. **Aliases into the buffer are invalidated whenever
  `keep > 0`** — the bytes physically move on the same backing
  array, so any string alias the caller still holds points at
  wrong content after the call.

### Aggressive compaction inside Stream methods

`SkipSpace`, `ConsumeColon`, `Int64`/`Uint64`, and `String`/`KeyView`
all pass a non-zero `keep` (current local cursor, or the value-start
`start` for spans that need to outlast the loop) so the buffer
stays bounded at roughly `max(chunk_size, single_value_size)` even
across long streams. Each method updates its own locals after the
shift (`i = 0` for the entry cursor, or `j -= start; start = 0` for
the string-body case), then writes the final position into `s.Pos`
before return.

The `Shift` field (defaults to true via `Reset`) gets flipped off
around `SkipValue` inside RawJSON capture and `json.Unmarshal`
fallback spans, where the generated code needs stable absolute
offsets to slice `s.Bytes()[start:s.Pos]`. Bookkeeping branches in
SkipSpace/etc check `s.Shift` before resetting the cursor.

### Dispatch-loop shift points

Generated code adds two more shift points at the dispatch-loop
boundary: `ReadMore(s.Pos); s.Pos = 0` after `ObjectOpen+SkipSpace`
and after the per-iteration value decode + SkipSpace. Each known-
key case opens with `s.ConsumeColon()` — the alias from `KeyView`
is no longer needed past dispatch, so the shift it triggers is
safe. `UnknownKeyError` and the inline-catch-all map key both
detach the alias with `strings.Clone(key)` so subsequent
compactions don't corrupt the stored value.

### Lazy bounds checks (one-byte-at-a-time refill)

Each `(*Stream).X()` method does its own bounds check
(`if s.Pos >= len(s.buf) { ... ReadMore(s.Pos) ... }`) and proceeds
once **one** new byte has landed. Multi-byte literals (`true`,
`false`, `null`, `\uXXXX`) are scanned **byte-by-byte**: each char
triggers an individual bounds check + maybe ReadMore, and a
mismatch fails fast without fetching the rest. This is the
lazy-streaming property — parse-what-you-have, fetch one chunk
only when truly stuck.

The old `Ensure(p *int, n int)` + `Anchor`/`Unanchor` design
(bulk-fetched N bytes via an internal Read loop) is in
`.claude/backlog.md` under "tried and rejected" — don't
reintroduce a bulk-fetch primitive without a fail-fast story
that doesn't regress lazy semantics.

### Stack-allocatable, no pool

```go
var s scan.Stream
s.Reset(r, buf)
```

Caller owns `buf` lifecycle. `scan.NewStream(r, buf)` is the heap-
allocating shorthand for callers who don't care about the one-time
alloc and want a single-expression form. There used to be an
`Acquire`/`Release` pair around a `sync.Pool` of Streams; the pool
was removed because it bundled too many implicit-lifetime
assumptions about the buffer and led to silent corruption when
callers reused buf across decodes. Honest API now: caller knows
what their buffer does.

### Stream copies vs bytes-path aliases

`Stream.String` and `Stream.Number` **copy** their content out via
`string(s.buf[start:end])` rather than aliasing — owned values,
safe with map keys, decoder output detached from buf. The bytes
path still aliases (caller owns input there). The streaming path
traded ~230K extra allocs/Mega for safety + simplicity. See
`.claude/backlog.md` "Stream Acquire/Release pool" for why
aliasing-on-stream + arena-compact was abandoned.

### `KeyView` — alias for dispatch keys

`Stream.KeyView` is a sibling of `Stream.String` that aliases via
`unsafe.String(unsafe.SliceData(s.buf[start:]), end-start)` on the
happy path (no escapes). Falls back to `stringSlow` for escape
sequences. Used in generated object-field dispatch where the key
is read, matched against constant strings, then discarded — the
alias never escapes the dispatch frame, so safety holds and the
~200 throwaway heap strings per decoded value drop to zero.

The alias survives buffer growth because GC pins the old backing
once any pointer (string header) still references it. But a
non-zero `keep` in subsequent `ReadMore` calls WILL move bytes on
the same backing array and corrupt live aliases — see "aggressive
compaction" above. The dispatch sites detach via `strings.Clone`
before any code path that could trigger a shift (UnknownKeyError,
inline-catch-all map key).

## Design rationale

- **`unsafe.String` aliases are safe across buffer growth.** Go's
  GC is non-moving (mark-and-sweep, no compaction). An alias into
  the OLD backing array keeps that array live — GC walks string
  headers and marks the pointed-to memory. Stream can `append`-
  grow freely; aliases from previous values remain valid.
- **`Stream` is stack-allocatable; no pool.** See above.

## Tests

- `scan/any_test.go` — `Any` / `AnyNumber` stdlib parity + per-
  shape `BenchmarkAny_Shapes`.
- `scan/string_test.go`, `scan/number_test.go`, `scan/stream_test.go`
  — primitive correctness + edge cases.
