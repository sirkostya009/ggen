# scan — hand-rolled JSON scanner + streaming Stream

Runtime package. Bytes-path primitives + streaming `Stream` type. Generated decode methods call directly. No tokenizer, AST, reflection.

## Bytes path (`scan/scan.go`)

`[]byte` primitives on `(data, pos)` → `(value, newPos, error)` or `(newPos, error)`. Zero-alloc happy path, sentinel errors (`scan.ErrBadObject` etc).

- `SkipSpace`, `String`, `Int64`, `Uint64`, `Float64`, `Bool`, `ObjectOpen`,
  `ArrayOpen`, `SkipValue`.
- `String()` uses `unsafe.String(unsafe.SliceData(data[start:]), len)` zero-copy alias when no escapes. Falls back `stringSlow` w/ `utf8.AppendRune` for `\uXXXX` + surrogates.
- `bytes.IndexByte` (SIMD) finds closing `"`; second IndexByte over span detects preceding `\`. Truncated `\u…`/trailing `\` → `ErrBadString` via fallthrough to `stringSlow`.
- `stringSlow` scratch cap = first-quote span (`closeIdx`), NOT the remaining payload — a payload-sized cap made M escaped strings allocate O(N·M) bytes (same hazard class as the fixed `skipString` quadratic probe; an escaped `\"` past the hint just regrows geometrically). Returns `unsafe.String` over the write-once scratch — 1 alloc per escaped string, no exact-size second copy. Stream `stringSlow` mirrors the alias return (its cap was already bounded at 32). Pinned by `TestStringEscapeAllocBounded`.

## Stream (`scan/stream.go`)

`Stream` wraps `io.Reader` w/ growable buffer (`buf []byte`, grown via `append`). Cursor = exported `Pos int` field — every scan primitive (`s.SkipSpace()`, `s.String()`, `s.KeyView()`, `s.Int64()`, `s.SkipValue()`, …) reads from `s.Pos`, writes back. Methods take no cursor arg, never return one. Capture raw span:

```go
start := s.Pos
s.SkipValue()
raw := s.Bytes()[start:s.Pos]
```

### `ReadMore(keep int) error` — the only I/O primitive

One Read per call, never loops. `keep` = lowest offset caller still needs; bytes before may be discarded:

- `keep == 0` — grow without shift (bigger backing if full). Offsets stable, aliases survive.
- `keep == len(buf)` — reset to `[:0]`, refill from 0 (full compaction).
- `0 < keep < len(buf)` — in-place `memmove` `buf[keep:n]` → `buf[0:n-keep]`, read into freed tail. **Aliases into buffer invalidated when `keep > 0`** — bytes move on same backing.

### Aggressive compaction inside Stream methods

`SkipSpace`, `ConsumeColon`, `Int64`/`Uint64`, `String`/`KeyView` pass non-zero `keep` (current cursor, or value-start `start` for spans outlasting loop) so buffer stays bounded ~`max(chunk_size, value_size)` across long streams. Each updates locals after shift (`i = 0`, or `j -= start; start = 0` for string body), then writes final `s.Pos`.

`Shift` field (defaults true via `Reset`) flips off around `SkipValue` in RawJSON capture + `json.Unmarshal` fallback spans, where generated code needs stable absolute offsets to slice `s.Bytes()[start:s.Pos]`. Bookkeeping branches check `s.Shift` before resetting cursor — including `Int64`/`Uint64`'s mid-digit-loop refill (an ungated `i = 0` there re-read consumed digits under no-shift; latent until something decodes an integer inside a no-shift span — pinned by `TestIntegerScanNoShiftRefill`).

### Dispatch-loop shift points

Generated code adds two more shift points at dispatch-loop boundary: `ReadMore(s.Pos); s.Pos = 0` after `ObjectOpen+SkipSpace`, and after per-iteration value decode + SkipSpace. Each known-key case opens w/ `s.ConsumeColon()` — `KeyView` alias no longer needed past dispatch, shift safe. `UnknownKeyError` + inline-catch-all map key detach alias via `strings.Clone(key)`.

### Lazy bounds checks (one-byte-at-a-time refill)

Each method does own bounds check (`if s.Pos >= len(s.buf) { ... ReadMore
(s.Pos) ... }`), proceeds once **one** new byte lands. Multi-byte literals (`true`, `false`, `null`, `\uXXXX`) scan **byte-by-byte**: each char → individual check + maybe ReadMore, mismatch fails fast without fetching rest. Lazy-streaming property — parse what you have, fetch one chunk only when stuck. (Old `Ensure`/`Anchor`/`Unanchor` bulk-fetch in `.claude/backlog.md` "tried and rejected".)

### Stack-allocatable, no pool

```go
var s scan.Stream
s.Reset(r, buf)
```

Caller owns `buf` lifecycle. `scan.NewStream(r, buf)` = heap-allocating shorthand. Old `Acquire`/`Release` `sync.Pool` removed — bundled implicit buffer-lifetime assumptions, caused silent corruption when callers reused buf across decodes (see backlog).

### Stream copies vs bytes-path aliases

`Stream.String` + `Stream.Number` **copy** via `string(s.buf[start:end])` rather than alias — owned values, safe as map keys, output detached from buf. Bytes path still aliases (caller owns input there). Streaming traded ~230K extra allocs/Mega for safety + simplicity.

### `KeyView` — alias for dispatch keys

Sibling of `Stream.String`, aliases via `unsafe.String(unsafe.SliceData
(s.buf[start:]), end-start)` on happy path (no escapes), falls back `stringSlow`. Used in generated object-field dispatch where key read, matched, discarded — alias never escapes dispatch frame. Drops ~200 throwaway heap strings per decoded value to zero.

Alias survives buffer growth (GC pins old backing once string header references it), but non-zero `keep` in subsequent `ReadMore` WILL move bytes + corrupt live aliases — see "aggressive compaction". Dispatch sites detach via `strings.Clone` before any shift-triggering path (UnknownKeyError, inline-catch-all map key).

### `skipString` — bounded backslash probe

All three string scanners (`String`, `KeyView`, `skipString`) bound the backslash IndexByte to the closing quote; whole-tail probe only when quote not yet buffered. `skipString` once probed the full tail per skipped string — `SkipValue` went O(payload²) when buffer held whole payload (Shift=false RawJSON capture): bound cut Mega stream wall -67% serial (benchstat p=0.002, n=6; readall control flat), allocs identical.

## Design rationale

- **`unsafe.String` aliases safe across buffer growth.** Go GC non-moving (mark-and-sweep, no compaction). Alias into OLD backing keeps it live — GC walks string headers. Stream can `append`-grow freely; prior aliases stay valid.
- **`Stream` stack-allocatable; no pool.** See above.

## Tests

- `scan/any_test.go` — `Any`/`AnyNumber` stdlib parity + `BenchmarkAny_Shapes`.
- `scan/string_test.go`, `scan/number_test.go`, `scan/stream_test.go` — primitive correctness + edge cases.
