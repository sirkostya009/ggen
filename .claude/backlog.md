# TODO

- **Improve fuzz coverage.** Current surface (`integrationtests/fuzz_test.go`):
  three fuzzers over `Node` — `FuzzScanNoPanic` (panic safety),
  `FuzzRoundtrip` (encode→decode fixed-point), `FuzzCompat` (ggen ↔ jsonv2
  agreement). Gaps: per-feature fuzzers for alias types, every validation rule
  (oneof/runes/ascii/email/…) with rule-specific generators, the streaming path
  (chunked reader, varied chunk sizes), `[N]T` strict-length arrays,
  `KindAny`/`KindRawJSON` edge cases, `omitempty`/`omitzero` round-trip,
  multierr accumulation. Add seeds for known-tricky inputs (truncated
  `\uXXXX`, surrogate pairs, `null` mid-value, trailing-garbage).

- **Add more CLI flags.** Candidates: `-out-dir` for shared output (vs
  next-to-source), per-struct selectors beyond the trailing-name filter
  (`-only=Foo,Bar`), explicit `-tag <tag>` to scope to one build-tag bucket.
  None urgent. (`-dry` shipped; `check.go` entry points factored for a future
  `ggenvet`.)

- **Custom vet tool.** Ship `ggenvet` (`go vet -vettool=ggenvet`) for misuses
  the compiler can't see. Biggest: the **zero-copy aliasing footgun** —
  decoded strings alias the source `[]byte`, so mutating input after
  `DecodeFrom` silently corrupts values. A flow-sensitive check flagging any
  write to a `data` arg (index assign, `append` over same backing, `copy`)
  after it's passed to `DecodeFrom`. Other checks: stale generated file
  (struct with `//ggen:generate` whose `_ggen.go` lacks the method set);
  annotation/tag mismatch (`required` on `omitempty` pointer; `oneof` values
  that don't lex as the field kind; `trim` on non-string); extend the
  parse-time applicability matrix into vet. Shape: separate `ggenvet/`
  subpackage with own `main.go` (`go install …/ggenvet@latest`), reusing
  ggen's parse layer so checks stay in sync.

- **`AppendAny` output prealloc via size precalc.** ggen ties/barely beats
  jsonv2/stdjson on typed slice marshal (`[]int` 712 vs 745 ns/op) despite
  winning 2-4× on maps. Cause: bench passes `nil` dst, so `AppendAny` runs the
  growth chain (0→8→…→1024), 7-8 allocs for ~330 B; stdjson/jsonv2 hide this
  with pooled buffers. Presized buffer benches drop to 0 allocs (ggen wins
  1.85× on `[]int`). Options: (a) reflect-driven pre-walk for size, only when
  `cap(dst)==0` AND input is a concrete homogeneous container with a fast path
  (skip `[]any`/`map[string]any` — unbounded); (b) internal `sync.Pool` inside
  the encode package's `Marshal` (NOT `AppendAny` — keep caller-owned dst); (c)
  explicit `AppendAnySized(dst, v, hint)`. Pick when a real workload pins slice
  marshal as a hotspot — map wins dominate today, slice tie acceptable.

- **Wrap parse errors in `decode.ParseError` with position context.** Today
  `scan.ErrBadString`/`ErrBadObject`/`ErrBadNumber`/`ErrUnexpectedEnd` are bare
  sentinels — user gets `"ggen: bad string"` and bisects by hand. Wrap at the
  call site with: byte offset (`pos` from scanner state, already in scope);
  field path (accumulated as the dispatch loop descends — needs a path-stack
  arg threaded through `DecodeFrom`, measure the hot-path cost); nearby-bytes
  window (`±32 B` aliased via `unsafe.String`); rule (which primitive failed).
  Shape: `type ParseError struct { Field, Rule string; Pos int; Snippet
  []byte; Err error }` with `Unwrap()` so `errors.Is(err, scan.ErrBadString)`
  keeps working. Field-path threading is the cost driver — keep it optional
  (zero-cost when nil) if it regresses.

- **Position context on `validation.*` errors.** Same idea one layer up. Add
  `Pos int` (maybe `Snippet []byte`) to `MinLenError` etc. — generated code
  already has `pos`/`s.Pos` in scope at the failure site. Either grow the
  `validation.Error` interface or add a sibling `PositionedError interface {
  error; Pos() int }`. Pair with the parse-error wrap above.

- **Revisit `validation.CustomError` shape.** Today `{Field, Name string,
  Cause error}` + `Unwrap()`. Rough edges: `Name` doubles as rule identifier
  and user-facing label (split into `Rule` + `Name`); no `Value any` field like
  other typed errors (can't expose what the validator rejected); `Cause` is
  bare `error` (a typed sub-interface could improve `errors.As`). Pick when
  there's a concrete report-shape ask.

- **Decode-into-receiver merge on `*T` pointee.** Pointer fields always
  allocate a fresh pointee (`var v T; ... result.X = &v`), discarding the
  receiver's existing `*T` — the one hole in the receiver-as-merge-source
  contract (scalars/slices/maps/nested-structs already honor it). Fix:
  `if result.X == nil { var v T; result.X = &v }; *result.X, _, _ =
  (*result.X).DecodeFrom(data)` for ggen-typed pointee, equivalent for
  primitives. Trade-off: JSON `null` still sets `result.X = nil`, dropping the
  pre-existing pointee (matches stdlib merge). Pin a case in
  `integrationtests/merge_test.go`. Pick when a real receiver-reuse hot path
  shows the per-decode alloc is a problem.

- **`sql.Null[T]` (Go 1.22 generic form) fast path.** Doesn't match
  `SQLNullSpec` (string lookup against `sql.NullString`/…), so every
  `sql.Null[int]`/`[time.Time]`/… falls through to the `encoding/json`
  reflective fallback for both decode and marshal. Two consequences: (1)
  **wire-shape divergence inside the family** — legacy `sql.NullX` ships ggen's
  "value or null" shape, the generic form reflects out `{"V":val,"Valid":true}`
  (silent footgun); (2) **slow path** — no inline scan, no AppendText, flat
  128-byte JSONSize. Fix: extend `SQLNullSpec` (or add `SQLNullGenericSpec`) to
  recognize `sql.Null[…]`, parse the inner type, resolve via `resolveKind`,
  return `SQLNullKind{Field:"V", Inner, Type}` — the four codegen paths already
  thread `spec.Field`/`Inner`/`Type` correctly, so one parse-time change
  unlocks all. Open questions: scope of accepted inner kinds (whitelist
  string/int*/uint*/float*/bool/time.Time is tightest); cross-package
  `pkg.Null[T]` probably out of scope. Mirror the legacy `SQLNull*Struct` split
  in `integrationtests/sql_test.go`.

- **Multi-level pointer (`**T`, …) inside slice/array elements.** Scalar fields
  (`*****int`) and map values (`map[string]**T`) work via the `encoding/json`
  fallback. Slice/array elements take the slab fast path, which assumes the
  peeled element isn't another pointer: `[]**int` → slab typed `[]*int`,
  pre-grow becomes `append(slab, *int{})` (invalid Go, won't compile); `[3]**int`
  → compiles but inner scan is a bare `SkipValue`, every element silently nil.
  Fix: detect "ElemPointer && peeled ElemType still begins with `*`" and route
  per-element decode through a `json.Unmarshal` fallback targeting `*ElemType`
  (like the map value path); slab stays for depth-1. Coverage pinned by
  `TestNPtr_*` in `integrationtests/pointer_test.go` (scalar + map value only;
  slice/array variants absent until this lands).

# Tried Rejected

- **Generator emitting `go/ast` nodes instead of text.** Full rewrite on the
  `ast-conversion` branch (commit `feadbba`); output byte-identical. Rejected:
  (1) less readable — every `fmt.Fprintf(b, "if %s == nil {…", ref)` becomes a
  pointer-struct AST tree you can't skim; (2) higher peak RAM (pointer-heavy
  nodes survive until print); (3) marginally slower codegen; (4) larger binary.
  Kept on the branch in case an `ast.Walk`-based optimization ever justifies it
  (e.g. replacing `coalesceConstAppends`), but nothing does today.

- **Pointer-arithmetic decoder / `unsafe.Add` byte loads** to eliminate bounds
  checks. Cut bounds checks in `bench_ggen.go` 59→18 (byte path: 0), but
  Unmarshal **regressed ~10%**. Modern AMD64 makes never-taken bounds checks
  ~free (1 cycle, predicted), while `unsafe.Add` defeats compound addressing:
  `data[i]` is one `MOV (base)(idx*1)`, the unsafe form takes 2-3 (optimizer
  treats it as opaque, loses loop-invariant hoisting). Don't retry unless
  targeting a CPU where bounds-check branches mispredict.

- **Removing all decode-side inliners** (inlineSkipWS/ScanInt64/ScanUint64/
  ScanString) for plain `scan.X(...)` calls. A `//go:noinline` micro-bench
  showed per-call overhead ~0.4 ns (basically noise), but macro Unmarshal
  **regressed ~15-20%** — inlining matters for register allocation across
  adjacent ops, ICache, and compound BCE the compiler only does with the body
  in scope. Don't trust per-call micro-benches for hot-loop inlining.

- **Stream-path `_s.SkipSpace` inliner** (`inlineStreamSkipWS`). Saved the
  method-dispatch frame but kept `_s.Ensure(j+1)` in the loop. Raw +7%,
  normalized via EasyJSON Reader **~2% (noise)** — the `Ensure` cold path
  dominates Stream throughput. Don't retry without tackling Ensure overhead.

- **Inlining `scan.Bool` / `scan.Float64`.** `//go:noinline` micro-bench:
  call frame fully amortized by body work for primitives at this size (0.24 ns
  of 12.6, call version slightly faster). No win. Same lesson as above.

- **`Ensure(p *int, n int)` + `Anchor`/`Unanchor` for bounded streaming.**
  Original primitive bulk-fetched N bytes by looping `Read` internally, with a
  window-shift mode + Anchor/Unanchor to freeze offsets across `SkipValue` for
  RawJSON/`json.Unmarshal` spans. Killed by: (1) "Read in a for loop" is the
  antithesis of lazy streaming (if you loop on Read, `io.ReadAll` is simpler);
  (2) the anchor + `*int` cursor adjustment was a stale-position bug source
  (Float64/Number needed `&start` not `&i` or the prefix dropped mid-parse).
  Replaced with `ReadMore(keep int) error` (single Read, optional in-place
  compaction) + byte-by-byte multi-byte literal scans. The `keep` param came
  back to bound buffer growth without resurrecting the bulk-fetch loop; internal
  methods pass `keep=0` (grow-only), only the dispatch-loop bounds checks
  compact. Fail-fast preserved (~67 ms vs ~78 ms ReadAll on invalid). Don't
  reintroduce bulk-fetch without a fail-fast story that keeps lazy semantics.

- **Stream `Acquire`/`Release` pool with reused buffer.** Pooled `*Stream`,
  `Release` truncated `s.buf` to retain it. Combined with alias-mode strings
  (`unsafe.String` into `s.buf`) this is **silent corruption**: the next
  `Acquire` reuses the buffer, `Read` overwrites bytes, prior decoded values'
  aliased fields flip content (`n1.Name` `"FOO_TINY"` → `"BAR_VERY"`). Caught
  by a two-payload probe; the residency bench missed it (content collisions
  matched). Replaced with stack-allocated Stream + caller-owned buf + copy-mode
  strings.

- **`[512]byte` inline scratch in `Stream`.** Avoid the buffer heap alloc for
  small payloads via a stack-resident scratch array. Failed in the original
  tests: escape analysis couldn't prove `&s` safe across
  `zero.DecodeStreamFrom(&s)` inside the then-generic `decode.UnmarshalStream[T]`
  wrapper, so the whole Stream (incl. the array) heap-escaped. The wrapper is
  now gone and the call site is direct, so the constraint may no longer apply —
  worth re-measuring if a residency push needs small-payload alloc back.

- **Per-decode arena + `StreamArenaSize`/`StreamArenaCompact` codegen.** Parse
  with aliased strings, walk the value to sum string bytes, allocate one
  exact-size arena, copy + rewrite headers via `unsafe.String`. Fully
  implemented. Result on Mega: allocs 347K→128K, B/op 24.6→19.7 MB, but
  **residency unchanged at ~86 KiB/item** and wall clock unchanged (2-walk
  overhead canceled the per-string-copy savings). The gap was never per-string
  fragmentation — it was per-decode buffer retention + map rebuild allocs (Go
  has no in-place key-rewrite). Removed the codegen + `decode/arena.go`. If
  retrying: prove the residency gain BEFORE shipping the codegen.

- **`maxlen=N` as a slice/map prealloc hint.** Used `maxlen=64` to emit
  `make([]T,0,64)` so Mega's 5-26 element slices skipped the growth chain.
  Hidden cost: every retained value carried the over-allocated cap forever.
  Killing it cut per-item retention 163 → ~67 KiB on the bytes path — **biggest
  single residency win in the whole exploration**. Now only `len`/`minlen`/
  `hintlen` drive prealloc. Don't reintroduce `maxlen` as a sizing hint without
  an opt-in mechanism (see `hintlen`).

# Future

- **Hybrid key-dispatch at codegen.** Current length-first switch + if-chain
  wins for narrow structs. For wide structs where length groups balloon (>5
  candidates), emit `switch key` so Go's compiler auto-hashes (≥7 cases).
  Picking per-struct/per-length-group could squeeze a few % on wide structs
  without regressing narrow. Postponed until a 50+ field schema shows up.

- **Validation-derived encode hints.** Use `ggen` tags for encode shortcuts:
  `ascii` → skip escape table; `lte=N` → fixed-width digit formatter instead of
  `strconv.AppendInt`; similar for `oneof`/`len`. Real wins on hot fields, but
  couples encode shape to decode-time validation — the same field would marshal
  differently based on its `ggen:` tag, blurring the marshal contract.
  (Decode-side prealloc already uses `len`/`maxlen`/`hintlen` — that's a `make`
  cap hint, not a wire-shape change.) Shelved unless a target schema makes the
  win concrete.

- **Streaming `io.Reader` over marshalled output (state-machine codegen).**
  Per-struct `AsReader()` returning a resumable state + `encode.Reader[T](v)`
  exposing `io.Reader`. Suspends mid-marshal so peak memory = caller's `p
  []byte` instead of `JSONSize()`. Three granularity tiers (per-field cheapest,
  ~300 LOC). Only matters when a single payload is too big to materialize —
  `JSONSize()` fits comfortably in RAM for everything we care about. Shelved
  unless multi-GB request bodies show up. The trivial
  `bytes.NewReader(Marshal(v))` is a one-liner users can write.

- **SIMD / AVX2 vectorization for hot scanning loops.** Sonic narrows the Mega
  Unmarshal gap partly via hand-written AMD64 AVX2 for quote-scan, WS-skip,
  number parse. ggen does these byte-at-a-time. Candidates: `bytes.IndexByte`
  (already SIMD; verify it vectorizes — used for the closing-quote scan); AVX2
  WS skip via `golang.org/x/sys/cpu` + Plan9 asm; number parsing probably not
  worth (strconv is tuned, inline int scan beats the call). Pure-SIMD-on-Go
  might claw back 10-15% on string-heavy payloads at the cost of per-arch source
  duplication, `go vet`-incompatible asm, Plan9 maintenance, lost portability.
  Try only if a profile shows the byte-scan loop dominant AND the codegen
  complexity is acceptable. Don't speculatively add asm "to keep up with sonic";
  the gap is small and portability is a feature.
