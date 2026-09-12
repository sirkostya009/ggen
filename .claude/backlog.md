# TODO

House rule: nothing lands without a core-pinned before/after bench (one pass per
side, NEVER alternating-pair loops) — Mega is memory-latency-bound, so CPU-only
shaves routinely vanish in wall clock.

## Open perf candidates (source-verified, UNMEASURED — prune freely)

- **[26] container maxlen early-bail inside element loops.** maxlen is validated
  only AFTER the loop, so a 10M-elem payload vs maxlen=64 fully decodes +
  ALLOCATES before failing. Loop-top `if len(dst)==MAX { MaxLenError }` caps work
  at MAX+1 (multierr: append once, `SkipValue` the rest). DoS-hardening, ~0 on
  valid input. SEMANTICS DECISION REQUIRED: `MaxLenError.Got` becomes MAX+1,
  multierr stops collecting dive errors past the bound. Touches every container
  emitter.

- **GENERATED stream container-loop refills still grow-only (round-7 find,
  distinct from the runtime skip-tree item below).** `streamReadMore` is
  emitted with `keep="0"` at ~8 container-loop sites (generate.go array-entry,
  element-separator, and null-literal refills — visible in
  `bench/small_ggen.go`); readers fill the window, so these land with
  `len == cap` and DOUBLE the buffer. The dispatch loop already compacts
  (`ReadMore(s.Pos); s.Pos = 0`); stream element values are owned copies and
  `KeyView` aliases are dead past dispatch, so compaction is believed safe at
  most sites — the null-literal loop holds `s.Pos`-relative offsets needing a
  rebase. Same class as the skip-tree compaction (8.4 MB → 127 KB/op). B/op
  win on long arrays/maps through small buffers; wall-clock likely
  small-to-flat. UNMEASURED, per-site alias-safety audit not completed —
  house-rule bench + safety pass before landing.

- **Stream skip-tree separator/colon/literal refills still grow-only.** The
  per-iteration ','/']'/'}' bound checks in (*Stream).skipArray/skipObject,
  the byte-by-byte literal loops (Bool, the 'null' arms), and anyObject's
  colon check refill with ReadMore(0); at those points len == cap (readers
  fill the window), so each refill DOUBLES the buffer for bytes that are
  being discarded. Compacting needs per-site cursor rebases (the literal
  loops hold j-relative offsets — same class as the 2026-07 skip-tree
  compaction pass, which deliberately skipped these). Perf only, house-rule
  bench-gated (SkipHeavy stream rows + B/op).

Round-10 audit candidates (2026-09). Prepared patches live outside the tree in
`~/audit-round10/work/perf_{scan,stream,gen}.patch` — nothing landed, all
UNMEASURED (the box was on powersave during the audit); house-rule A/B before
any of them moves:

- **`[]*T` slab allocated fresh on every decode-into-receiver (bytes +
  stream).** The depth-1 `[]*T` arm emits `slabN = make([]E, 0, cap)`
  unconditionally in the non-empty arm while the receiver reset only
  reslices the pointer slice, so the carried pointees are orphaned and
  `append(dst, &slab[len-1])` overwrites the carried pointers — the one
  reuse shape that recycles neither pointee nor slab (`*T` fields, `[]T`
  elements and `[]**T` chains all reuse). Mega: 2426 of the 5337 allocs left
  on a reused-receiver decode are these slabs (45%); stream `Value(prev)`
  7 allocs/value where 6 are the inherent string copies. Fix shape: when
  `directStruct` and the carried `dst[k]` (within the old len) is non-nil,
  decode into `*dst[k]` (its `DecodeFrom` self-resets, opt #74's gate),
  allocating a slab lazily for nil/past-cap slots only. Bench: needs a
  `[]*T` `_reuse` row (only MapValues/MapValuesHeavy carry one today),
  Mega_Reader via Seq/Value(prev). Allocation counts proven, wall clock not
  — Mega is memory-bound.

- **Scalar slice elements pay a dead zero store + a len-1 re-index bounds
  check per element.** Opt #27's `dst = append(dst, <zero>)` pre-grow then
  `dst[len(dst)-1] = int(n)` is emitted for kinds whose value is ALREADY in a
  temp (the inline int/uint scanners) or an expression (the string fast
  path); gc cannot dead-store the zero across the inline scan nor prove
  `len >= 1` (check_bce: `mega_ggen.go:850 Found IsInBounds`, plus the tags
  element). `dst = append(dst, int(n))` is a pure restructure — one store,
  no re-index, the `panicBounds` site drops; the in-place multi-assign stays
  only for call-returning kinds (`Float64`) and the `ggen.String` fall path.
  ~280k elements per Mega_Unmarshal at ~2 instructions + 1 predicted branch
  each, so likely below the 3-6% control drift on Mega — the "never-taken
  bounds checks are ~free" caveat applies; would show, if at all, on a
  cache-resident numeric-array micro. Bench: Mega_Unmarshal, stream twin.

- **Nested map swap defeated by the outer seed's `clear()`.** For
  `map[string]map[string]V` with allocation-owning inner values,
  `renderMap`'s `reusesMapValues && ElemKind == KindMap` arm seeds `mv =
  carried[mk]; clear(mv)`, then the inner level emits `carried1 := mv;
  reuse1 := len(carried1) != 0; mv = make(..., len(carried1))` — `reuse1` is
  constantly false, every inner value allocates fresh AND the just-cleared
  bucket array is discarded by the make: strictly worse than either pure
  clear-and-fill or pure swap (an unnoticed interaction from the bcaa594
  nested-map compile fix; the clear IS right for an inner map that fills in
  place). Fix: `clear(mv)` only when `!reusesMapValues(sliceElemField(f))`,
  else seed bare so the inner swap reads the live entries. Bench: none
  covers nested maps — a nested MapValues variant, allocs/op + B/op on the
  reuse row; the reproducer is an allocation-identity test.

- **`String` copies an entire unterminated escaped string before returning
  `ErrUnterminated`.** In the no-closing-quote branch `closeIdx < 0` already
  proves the value cannot complete, yet with a backslash present it hands
  off to `stringSlow` (capHint `bsIdx+16`) purely to classify the error, and
  `stringSlow` appends every remaining payload byte through the growth chain
  before failing at `len(data)`: a truncated 8 MiB payload whose last string
  carries one escape allocates ~41.7 MB across ~50 mallocs to return the
  same `(pos, err)`. `classifyStructural`/`classifyStructural64` carry the
  same shape. Fix must be a NON-COPYING `stringSlow` walk (a `copy bool` /
  `capHint < 0` mode running the same ctrl/escape/hex/surrogate checks) —
  NOT a `skipString` call: skipped spans are deliberately not
  surrogate-validated, so `String("\ud800abc)` = `(7, ErrInvalidUTF8)` vs
  `skipString` = `(10, ErrUnterminated)`. Error-path only, memory
  amplification hardening (~5× the input in garbage per failure) rather than
  throughput; pin with `AllocsPerRun`. No bench family exercises truncated
  payloads.

- **SWAR string kernels carry two bounds checks per 8-byte word + one in the
  tail.** `checkSpan`/`ctrlOrHigh`/`CheckUTF8`/`hasCtrlByte` use
  `for ; i+8 <= len(b); i += 8 { x := Uint64(b[i:]) … }` + `for ; i < len(b);
  i++`; check_bce reports IsSliceInBounds + IsInBounds on the word load and
  IsInBounds on the tail index in all four. The head-reslice shape `p := b;
  for len(p) >= 8 { x := Uint64(p); …; p = p[8:] }; for _, c := range p`
  compiles with zero bounds checks (verified as a real package file — a
  `_test.go` twin proves nothing, `go build` skips it): ~25 → ~20
  instructions per word, one branch per tail byte; `b` must be kept for the
  trailing `utf8.Valid`. Scalar tier only (the SIMD tiers classify ctrl
  in-vector), and Mega/Small/NoAlloc strings are 4-13 B so the word loop runs
  0-1 iterations — expect noise-to-small; only long clean strings (scalar
  SkipHeavy via `hasCtrlByte`, RuneGated) could show it. Bench: NoAlloc /
  SkipHeavy scalar rows.

- **`ReadMore(keep)` with a small `keep` on a full window memmoves nearly the
  whole buffer to reclaim `keep` bytes, then Reads at most `keep` bytes
  before growing anyway.** The compaction arm always memmoves `buf[keep:]`
  down and Reads into the freed tail, which is exactly `keep` bytes when
  the window was full — the state every refill reaches with an eager
  reader. When the value head sits near the window head (`stringView`/
  `KeyView`/the number scanners/`CaptureValue`'s first `ReadMore(start)`),
  the first refill memmoves `len-keep` bytes, issues a tiny Read (a 2-byte
  `read()` on a file/socket), and the next refill has `start` rebased to 0
  and takes the doubling arm — so the memmove and the tiny Read bought
  nothing, and `CaptureValue` re-skips the value once more. Reproduced: Read
  destinations `[1024 2 1024 2048]` for a 3000 B string at offset 2; fires
  in situ on Small_Reader/ggen_stream_512 (the 2800 B Bio at payload offset
  17: `[512 17 512 1024 2048]`), cannot fire on Mega_Reader (raw snippets
  ≤ ~400 B vs a 4196 B window). Fix: fuse the grow into the compaction arm
  when the post-compaction tail is small (`cap-(len-keep) < cap/4`) —
  allocate the doubled backing and copy `buf[keep:]` into it instead of
  memmoving in place. Passed every bounded-buffer pin in the worktree, but
  it is a TRADEOFF: 2× residency for values between 3/4 and 1× of the
  window to save one memmove + one short Read (+ one re-skip); expected
  wall clock well under 1-2% on Small_Reader_512, the syscall count on real
  readers is the visible part. Bench: `*_Reader` rows.

- **gc's big-function inliner budget defeats the `SkipSpace`/`Bool`/`Detach`/
  `StringView` shells inside the six largest generated decoders** — the
  doc half is in .claude/scan.md ("Inlinable two-tier SkipSpace"); the perf
  half (emit a one-compare guard `if !(s.Pos < len(s.Bytes()) &&
  s.Bytes()[s.Pos] > ' ') { err = s.SkipSpace() }` into generated stream
  dispatch, distinct from the rejected `inlineStreamSkipWS` full-loop
  inliner which kept `Ensure` in the loop) is ALREADY covered by measured
  rejections below ("Stream-path `_s.SkipSpace` inliner", "Window-gated
  inline stream int digit loops", "Inlining `ggen.Bool`"): Mega_Reader is
  malloc/ReadMore-bound. The analogous evidence that it is not zero is the
  −9.7% SkipHeavy compact from the SAME shell inlining into the runtime
  skip tree. Don't emit the guard without a house-rule A/B on Mega_Reader /
  NoAlloc_Reader / Small_Reader.

- **Stream `stringSlow` scratch starts at a fixed 32 B.** `(*Stream).stringSlow`
  does `make([]byte, 0, 32)` regardless of what is buffered, then appends the
  raw prefix and every decoded byte; the bytes twin sizes the scratch
  exactly from `stringSpanEnd`. Every escaped string over ~32 B runs the
  growth chain on the stream: 9 scratch allocs + copies on the 4.8 KiB
  EscapeHeavy field vs 1 on bytes. Fix: pass a capHint — `stringSpanEnd(
  s.buf, start)-start` when the closing quote is already in the window
  (`stringView`/`KeyView` and the `stringViewAVX*` cores know whether
  IndexByte found it), else `len(s.buf)-start`, floored at 32. Caveat: the
  only rows exercising it (EscapeHeavy/EscapeSparse `ggen_stream`) use a
  512 B window, so the exact arm never fires there and the window-remainder
  hint trims the chain to ~5 allocs, not 1; the 9→1 win needs a window at
  least as large as the string. Bench: EscapeHeavy/EscapeSparse ggen_stream
  (allocs/op, B/op).

- **`(*Stream).skipNumber`'s cursor is address-taken for `refillSkip(*int,
  *error)`, so every digit iteration loads/stores it through the stack.**
  `refillSkip` (cost 97, not inlinable — it calls ReadMore) takes `&i`/`&rerr`
  at 10 sites, so the compiler cannot registerize them: the -S listing shows
  `MOVQ i+24(SP)` / `LEAQ 1(SI)` / `MOVQ DX, i+24(SP)` per digit in all three
  digit runs, where the bytes `skipNumber` and the stream `Int64`/`Float64`
  keep the cursor in a register (they hoist `buf := s.buf` and only call
  ReadMore at loop exit — scan.md "Buffer-header hoist"; `skipNumber` is the
  odd refill loop out). Fix: value-returning `refillSkip(i int, rerr error)
  (int, error, bool)`, the ten sites rewritten as `if i >= len(s.buf) { if
  i, rerr, ok = s.refillSkip(i, rerr); !ok { … } }`; asm-verified in a
  prototype: 0 address-of sites, cursor in BX, ~7 → 4 memory ops per digit,
  `refillSkip` stays out-of-line (cost 95) so the inline-check/cold-helper
  split is preserved; full root suite + simd + the Stream/Skip/Reader/Seq
  integrationtests passed. Distinct from the rejected window-gated inline
  int loops (generated Int64 sites) and the vector skipNumber tier (bytes
  path): no new kernel, no new call, only the address-taking removed. Reach:
  SkipHeavy `ggen_stream` rows (compact + pretty, scalar and avx512 — all
  tiers call `s.skipNumber()`); Mega_Reader does not reach it
  (`CaptureValue` uses the bytes SkipValue). Zen 3+ memory renaming can hide
  the SP-relative store→load latency, so the win may be small. Update the
  scan.md `refillSkip(&i)` sentence if it lands.

Smaller UNMEASURED notes from the round-10 fix wave (benches were forbidden
there; check when a bench pass is next scheduled):

- The avx512 no-over-read tail shapes are expected flat-to-faster: the
  overlapping reload replaces KMOVQ+VMOVDQU64+VMOVDQU8.Z with one VMOVDQU64
  and runs once per document (only last string values within 64 bytes of
  EOF that are ≥32 B or escape/non-ASCII bearing reach the tier call); the
  <64-byte stack copy runs only when the string body itself starts within
  64 bytes of EOF; `validUTF8x64` lost four masked loads and its duplicated
  tail body. NoAlloc/Small avx512 rows.
- `(*Stream).skipLiteral` is not inlinable (cost 141): scalar
  `skipValueDepth`'s null arm and the `anyValueDepth`/`anyNumberValueDepth`
  null arms went from an inline 3-byte loop to a call; the bool skip arms
  (scalar + tiers) swapped a `Bool()` call (cost 277) for the cheaper
  `skipLiteral` call; the tiers' null arm was already a call. Expected
  neutral on mixed payloads; if a SkipHeavy stream row or an `Any`
  null-heavy row moves, re-inline the null arm keeping the `s.Pos = pos`
  give-up writes.
- `(*Stream).ensureSpan` costs 86 (not inlinable). The `\X`/`\uXXXX` arms of
  `stringSlow` guard it with an inline `j+n > len(s.buf)` compare so the
  EscapeHeavy stream path pays only the compare it paid before and calls
  only at a window edge; the surrogate arm calls it unconditionally (cold).
  If EscapeHeavy ggen_stream moves, the guard is the first thing to check.
- Bytes `String`/`skipString` run `hasCtrlByte` over an unterminated
  no-backslash tail — error path only (an unterminated string is always an
  error), one SWAR walk over bytes IndexByte already touched.
- `AppendAny`'s pointer-marshaler re-dispatch: every NAMED value reaching the
  reflect fallback pays one `PkgPath()` + one `sync.Map` load (`needsAddr`);
  containers hoist it to once per container and structs to once per type,
  but a named-primitive element (`[]MyEnum`) re-enters the fallback per
  element and re-checks (cached false). Struct fields with a `*T`-only
  marshaler reached through the reflect fallback copy once per field
  (the `reflect.Pointer` arm boxes `rv.Elem().Interface()`, so the fields
  are never addressable) — avoiding it means a `reflect.Value` entry point
  beside the type switch; not worth it unless a profile shows it.
- `AppendAny` omitempty is write-then-unwrite (jsonv2's slow path only).
  jsonv2 also has a fast pre-check (`len==0` / `IsNil` for string/map/array/
  slice/pointer/interface kinds, gated on the type having no custom
  marshaler); adding it would save boxing + emitting the empty value + the
  key on the reflect path, at the cost of a per-field "plain type" flag.
  Candidate if `AppendAny` on omitempty-heavy structs ever shows up.
- Float number-span assembly is now duplicated: `Stream.Float32` mirrors
  `Stream.Float64`'s refill loop verbatim, and the bytes grammar walk exists
  three times (`skipNumber`, `Float64`, `Float32`). A shared stream
  `numberSpan()` helper is a plausible dedupe (the stream path is
  ReadMore-bound) but bench-gated; the bytes duplication is deliberate
  (`skipNumber` CALL cost DeepNested +25%).
- Pointer seed / map pointee reuse skips cross-package ggen-generated leaves
  (`*thirdparty2.T`): `leafResets` uses `isGenerated`, which is
  package-local, so a ByteDecoder-rung leaf allocates a fresh pointee under
  receiver reuse (correctness unaffected — the `DecodeFrom` rung resets
  itself per opt #74). Extending `leafResets` to accept
  `f.Iface.ByteDecoder && f.Iface.StreamDecoder` would restore it for
  `elemPtrReusable`, `reusesMapValues` and `emitPointerSeed` alike; needs the
  leaf's `Iface` threaded through `elemPtrReusable`.

- **`AppendAny` output prealloc via size precalc.** ggen ties/barely beats
  jsonv2/stdjson on typed slice marshal (`[]int`) but wins 2-4× on maps. Cause:
  bench passes `nil` dst, so `AppendAny` runs the growth chain (0→…→1024), 7-8
  allocs for ~330 B; stdjson/jsonv2 hide this with pooled buffers. Presized
  buffer benches drop to 0 allocs (ggen wins). Options: (a) reflect-driven
  pre-walk for size, only when `cap(dst)==0` AND input is a concrete homogeneous
  container with a fast path (skip `[]any`/`map[string]any` — unbounded); (b)
  internal `sync.Pool` inside encode's `Marshal` (NOT `AppendAny` — keep
  caller-owned dst); (c) explicit `AppendAnySized(dst, v, hint)`. Pick when a
  real workload pins slice marshal as a hotspot — map wins dominate today.

- **Single-copy `-copy` escape strings — SHIPPED (both tiers, via `ggen.Detach`).**
  Was: escaped retained strings under `-copy` double-allocated (`ggen.String`/
  `StringAVX*` escape arm → `stringSlow` owned scratch, then a redundant
  `strings.Clone`). Fixed with `ggen.Detach(s, data)` — a tier-agnostic helper
  that clones IFF `s` aliases `data` (a pointer-range test; the `stringSlow`
  escape result is a distinct heap alloc → skipped, non-moving GC makes the test
  sound). Copy-mode codegen (scalar + SIMD fall) reuses the SAME aliasing tier
  func then calls `Detach`; `AnyCopy`/`AnyNumberCopy` do likewise. Reuses the AVX
  tier functions directly — NO per-tier `StringCopyAVX*`. `EscapeHeavy/ggen_copy`
  now equals the aliasing `ggen` row in both tiers (scalar 4 allocs, avx512 4
  allocs). See opt #49. (Same pass fixed a pre-existing SIMD gap: `StringAVX*`'s
  `classifyStructural` sized `stringSlow` off the first quote, not the real
  unescaped close via `stringSpanEnd` — SIMD escape decode 44→4 allocs.)

- **simdjson stage-1 block-mask skip (the "SkipHeavy prize") — REJECTED,
  measured (2026-08).** Built in full for the avx512 tier: per 64-byte block,
  classify quote/backslash/ctrl + whitespace and structural via two nibble-LUT
  PSHUFBs (the `|0x20` curlify fold; its only false positives, 0x0C→`,` and
  0x1A→`:`, are control bytes filtered by `&^ mCtrl`), derive escaped bytes
  from the odd-backslash-run trick, `in_string = prefix_xor(unescaped quotes)`
  carried across blocks, then a scalar state machine over ONLY the structural
  bits (depth stack, comma/colon placement, key-must-be-string, no trailing
  comma; numbers/literals validated by the existing scalar validators at the
  token starts the masks hand it). Full validation retained; on any doubt it
  returned ok=false and the byte-wise tree re-ran to own the exact sentinel, so
  error identity was parity-by-construction. Correct (exhaustive LUT probe over
  all 256 bytes, prefix-xor vs naive, mutation/truncation/seam parity, a
  "must actually vouch" test that caught a real bug: token emission used raw
  `mQuote & inStr`, so an ESCAPED quote inside a string was emitted as a
  string-start token).
  **SkipHeavy compact +16.8%** (4.08 → 4.77 ms), pretty only −2.4%; controls
  <1.2%. Diagnosis is conclusive and kills the whole shape: with the token
  dispatch neutered entirely, the MASK COMPUTATION ALONE measured 4.26 ms on
  compact — already above the old tree's 4.08 ms TOTAL. The report's premise
  ("replace a per-structural-char scalar tree") was wrong about this baseline:
  ggen's skip tier ALREADY vectorizes exactly what the block walk was meant to
  accelerate — `skipString*` locates via `structuralIndex*` (~3 compares/64 B,
  and it exits at the closing quote) and `SkipSpace*` skips whitespace runs by
  lane. The block walk instead pays ~5 masks + 2 shuffles per 64 B over EVERY
  byte, including string interiors it cannot exit early from. simdjson wins
  this because stage 1 indexes the document ONCE and every later pass is
  index-driven; amortising stage-1 cost over a single skip does not pay.
  Don't retry unless the skip becomes index-driven across many values (i.e. a
  document-level index ggen deliberately does not build), or the baseline
  loses its vector string/whitespace scans.

- **Two-block software pipelining in `validUTF8x64` (simdjson `step<128>`) —
  REJECTED, measured (2026-08).** Unrolled the wide loop to 128 B/iteration with
  two INDEPENDENT error accumulators, on the theory that the per-block
  `errAcc` accumulate serialized the classify chains. Consistently 2-6% SLOWER
  (4 KB 104.3 → 110.0 ns, 16 KB 409.5 → 432.4, 192 B +5.6%). The loop was
  already ILP-rich — prevN come from four INDEPENDENT loads, so consecutive
  blocks share only a 1-cycle VPOR — and the real limiter is VPSHUFB
  throughput on the shuffle port (3 per block), which unrolling cannot raise.
  The bigger body only adds register pressure and code size. Don't retry on
  this kernel; the same "already ILP-saturated, shuffle-port-bound" argument
  likely applies to the other classify loops.
  METHODOLOGY NOTE: the first attempt at this A/B compared numbers across two
  separate `go test -bench` invocations and read a false ~10%; the SAME code
  re-measured 104 → 113 ns between runs. Only the two-built-binaries form
  (`go test -c`, one pinned pass each) reproduced the baseline exactly and
  showed the true 2-6%. In-process `-bench` runs at different `-benchtime`
  are NOT comparable on this micro.

Rejected from past hunts, do not retry without a new argument: **[17]** positional
next-key predictor (payload-order-dependent), **[23]** indexed marshal loop
(go1.26 already folds the range copy) + pointer-receiver cores (vetoed — public
surface pinned by `Decoder[T]`).

- **Raw-span surrogate-escape validation (residual jsonv2 divergence).**
  Decode-side UTF-8 validation SHIPPED for every string-producing path AND
  captured raw spans (`ggen.CheckUTF8` at RawMessage/jsontext.Value sites —
  cli/CLAUDE.md opt #50). Two DECIDED exceptions (2026-07): skipped spans
  (`ignoreunknown`/`SkipValue`) stay grammar-checked only — intentional, keeps
  the skip tiers on the plain non-accumulating kernels; and unpaired `\uXXXX`
  surrogate ESCAPES inside a raw span pass (they're ASCII text there; jsonv2
  escape-parses raw strings and rejects — pinned as a divergence in
  `TestRawCaptureInvalidUTF8`). Closing the latter means surrogate-pairing
  escape parsing inside the skip walk under capture — only bother if a real
  consumer feeds captured raw spans back into a strict v2 decoder.

- **Vectorized UTF-8 validation — SHIPPED for the SIMD tiers (2026-07).**
  `validUTF8x16` (simd_utf8_amd64.go, Lemire/simdjson nibble-LUT
  algorithm on 16-byte lanes, AVX1-safe) replaces the scalar `utf8.Valid`
  second pass in `classifyStructural` + the stream cores: ~6.5× on the
  validation component (4 KB Cyrillic 3456→~530 ns). Control-checked re-measure
  (2026-07, performance profile — see bench/CLAUDE.md): cumulative pre-UTF8 →
  now, NoAlloc +67.7% scalar / +38.7% avx512, RuneGated +54.4% / +9.5% — the
  vector pass is why the avx512 penalties are a fraction of the scalar ones.
  ASCII rows flat; EscapeHeavy regressed +12.7% avx512 (escape path gained a
  utf8.Valid + surrogate rejection — an earlier note wrongly claimed it
  improved). Fusing into the parser
  loop itself was measured NOT worth it — the two-pass separation costs <10%
  of the validation bill at any width (L1-hot span; see
  BenchmarkStringUTF8Cost{,AVX512}); the DFA/lookup work IS the bill, so
  vectorizing pass 2 ≈ full fusion without quote-masking/carry complexity.
  Remaining candidates: scalar tier keeps `utf8.Valid` (no experiment — a
  scalar fused walk would forfeit the IndexByte locate and regress ASCII);
  `stringSlow` final check + `CheckUTF8` raw spans also keep `utf8.Valid`
  (span-level cold-ish, could route to the vector under simd builds if a
  profile ever shows them); a 32/64-lane widening of validUTF8x16 (Grouped
  shuffles exist) if the 16-byte version ever bottlenecks. The `allowinvalidutf8` opt-out SHIPPED
  (2026-07, flag + per-struct annotation, htmlescape-style granularity —
  see cli/CLAUDE.md opt #50): permissive structs emit pre-validation code
  shapes + `validate=false` scanner calls; raw bytes pass through.

- **EscapeHeavy claw-back — SHIPPED, but NOT via the run hoist (2026-08).**
  What landed is a loop-SHAPE change with no new kernel: `stringSlow`'s raw-byte
  copy (bytes + stream) moved into a WINDOWED inner loop (`escRunWindow` = 16
  bytes per trip) with the escape dispatch hoisted into the outer loop, so the
  hot loop body is just classify-and-copy. Measured in situ (core-24 pinned,
  500x, sonic control flat both fixtures): EscapeHeavy ggen **−5.9%**, ggen_copy
  −5.8%, ggen_stream −6.0%; EscapeSparse ggen **−27.6%**, ggen_copy −26.2%,
  ggen_stream −30.2%. B/op + allocs bit-identical. A new `EscapeSparse` bench
  family (prose density, ~90 B runs) landed with it — the suite had only the
  ~12%-density fixture, so half the tradeoff space was invisible.

  The simdjson `parse_string` run hoist itself is **REJECTED, measured**: locate
  the next `\`/`"` with IndexByte, `ctrlOrHigh` the run, bulk-append it →
  EscapeHeavy **+73.6%** (24.3 → 44.0 µs), stream +65.9%. EscapeHeavy's runs are
  4-5 bytes (`word`, `text`, `quo`, `te`, `x`, `yz`), so every run paid an
  IndexByte + ctrlOrHigh + append CALL where the old loop ran 4 predictable
  iterations; the escaped `\"` also defeats the memoized quote candidate,
  forcing a re-locate per unit. A minimal variant (per-byte scan, defer only the
  copy to one bulk append per run) still cost **+20.5%** — `memmove` overhead
  beats per-byte append at 4-byte runs. A length-gated hybrid (per-byte window,
  then bulk for the remainder) was also built and measured: it wins on sparse
  but costs +6…18% on dense, i.e. strictly worse than the plain windowed loop
  that shipped, which wins on BOTH. Bulk copying is dead here at every density —
  same lesson as the SWAR int parse and the SWAR encode clean-span: **bulk
  kernels lose below ~16 byte spans, and above that the windowed loop already
  captures the win.** Don't retry any of the three shapes.

  Note the original +12.7% regression's cause was the added `utf8.Valid` +
  surrogate rejection, NOT the copy shape (EscapeHeavy's raw bytes are ASCII, so
  the `rawHi` gate already skips that walk) — the −5.9% above is a separate win,
  not a revert of that cost.

- **`checkSpan` re-walks the ASCII prefix through `utf8.Valid` (UNMEASURED, HELD).**
  `checkSpan` accumulates a high-bit flag and then validates from offset 0, so a
  long mostly-ASCII span with non-ASCII in the tail pays the SWAR walk plus a
  second full-length pass through `utf8.Valid`'s ASCII skip. The sibling
  `CheckUTF8` already does the cheaper thing and documents why it is sound: start
  at the first high-bit word, since the byte before it is ASCII, i.e. a rune
  boundary. Fix is to track that word in the existing loop. HELD, not landed:
  wall-clock only, and the box is firmware-capped at 3 GHz (`cpuinfo_max_freq`),
  where the house-rule A/B is invalid. Land only from an uncapped box.

- **`appendReflectValue` re-derives `PkgPath()` per element (UNMEASURED, HELD).**
  The named-vs-unnamed guard runs on every call, and the reflect slice/map loops
  call it once per element while the element type is loop-invariant (they already
  hoist `elemKind`). Two dynamic method calls plus a string compare per element to
  re-answer a constant question. Same hold reason as above.

- **`appendStruct` per-field boxing — NOT A DEFECT, measured (2026-08).**
  An audit flagged `appendAny(dst, fv.Interface(), ...)` as one heap allocation
  per struct field, to be routed through `appendReflectValue`'s kind fast path.
  The premise is false on current Go: the boxed value does not escape (`appendAny`
  only reads it), so escape analysis stack-allocates it. Probed both shapes with
  `AllocsPerRun` over a 9-field struct, with the entry box hoisted out of the
  measured closure and with values chosen to defeat the runtime's small-int cache:
  **0 allocs either way**. The routing change was written and reverted. Don't
  re-propose without an in-situ measurement showing a real difference.

## Tooling / coverage

- **Improve fuzz coverage.** Current surface (`integrationtests/fuzz_test.go`):
  `FuzzStreamEqualsBytes` (bytes vs stream agreement over `Node` at every chunk
  size), `FuzzBoundaryNoPanic` / `FuzzStreamHugeStringNoPanic` (panic safety),
  `FuzzPrimitivesCompat` (typed values, ggen ↔ jsonv2 agreement). Gaps:
  per-feature fuzzers for alias types, every validation rule (oneof/runes/
  alphanum/…) with rule-specific generators, `[N]T` strict-length arrays,
  `KindAny`/`KindRawJSON` edge cases,
  `omitempty`/`omitzero` round-trip, multierr accumulation. Add seeds for tricky
  inputs (truncated `\uXXXX`, surrogate pairs, `null` mid-value, trailing-garbage).

- **integrationtests has no `GOEXPERIMENT=simd` lane.** Nothing there is
  generated with `-simd`, so the end-to-end shape of a `-simd avx512` struct
  decoding a page-sized document is pinned only at the runtime level
  (`TestSIMD_NoOverRead` covers the faulting call, `StringAVX512`; the emitted
  inline classify only issues bound-checked full-lane loads). A simd-tagged
  integration file would need its own `//go:generate ../ggen -simd avx512
  $GOFILE` line and a `GOEXPERIMENT=simd` test invocation in the suite.

- **Add more CLI flags.** Candidates: `-out-dir` for shared output (vs
  next-to-source), per-struct selectors beyond the trailing-name filter
  (`-only=Foo,Bar`), explicit `-tag <tag>` to scope one build-tag bucket. None
  urgent. (`-dry` shipped; `check.go` entry points factored for future `ggenvet`.)

- **Custom vet tool.** Ship `ggenvet` (`go vet -vettool=ggenvet`) for misuses the
  compiler can't see. Biggest: **zero-copy aliasing footgun** — decoded strings
  alias source `[]byte`, mutating input after `DecodeFrom` silently corrupts
  values. Flow-sensitive check flags any write to the `data` arg (index assign,
  `append` over the same backing, `copy`) after passing it to `DecodeFrom`. Other
  checks: stale generated file (struct with `//ggen:generate` whose `_ggen.go`
  lacks the method set); annotation/tag mismatch (`required` on an `omitempty`
  pointer; `oneof` values that don't lex as the field kind; `trim` on non-string);
  extend the parse-time applicability matrix into vet. Shape: separate `ggenvet/`
  subpackage with own `main.go`, reuse ggen's parse layer so checks stay in sync.

## Open design questions

- **Shrink the exported runtime API (2026-08, post single-package merge).** Most
  of the root package's exported surface is emit ABI, not human API: the scan
  primitives (`Any*`, `Bool`/`Int64`/`Uint64`/`Float64`/`Number`/`String`/
  `Null`/`Expect`/`SkipSpace`/`SkipValue`/`ArrayOpen`/`ObjectOpen`), the SIMD
  tiers (`StringAVX*`, `SkipSpaceAVX*`, `SkipValueAVX*`, `AppendString*AVX*`),
  encode plumbing (`CloseJSONString*`, `BytesToString`, `AppendFloat`,
  `AppendUnixSeconds`, `AppendNetipAddr*`, `AppendURL*`, `AppendRFC3339`),
  and glue (`NotEOF`, `Detach`, `CheckUTF8`, `SignedNeg`, `Uint64Limit`,
  `NewParseErr*`, `ShiftPos`, `BoolEnd` — the bool error-branch give-up
  position, `AnyIsEmpty` — the
  `omitempty` predicate for `any`, `Float32`, `ParseRFC3339`; the `Any*`
  families also carry a `validate bool` that only generated code has a
  reason to pass). Exported only because generated code calls
  them; `internal/` cannot host any of it — generated files live in USER
  packages outside the `sirkostya009/ggen/` path prefix, so internal
  visibility excludes exactly the caller that matters (same reason the
  maxDepth guard is emitted as a literal). The human-facing core is small:
  `Marshal*`/`Unmarshal*`/`Read*`/`Write*`/`Append{Any,Slice,String}*`,
  `Stream` + its methods + `Value`/`Slice`/`Array`/`Seq`, the interfaces
  (`Decoder`/`StreamDecoder`/`Marshaler`), and the error types + sentinels.
  Options if it ever itches: doc-comment tiering ("generated-code ABI, do
  not call") + a curated go-doc example set; a `ggen/abi` subpackage the
  emitter qualifies (public but clearly named, halves the root's go doc);
  or accept it — codegen runtimes (easyjson/gogoproto) all carry this. No
  action until a consumer confuses the two surfaces.

- **Go 1.27 stable `encoding/json/v2` dropped features the experiment had —
  3 parity gaps, 9 failing tests, UNDECIDED (2026-08).** The 1.27 bump removed
  `goexperiment.jsonv2` (v2 + jsontext are stable, no flag). But the STABLE
  release is not the package the experiment shipped: it cut or tightened several
  things ggen still implements, so the ggen↔jsonv2 parity tests fail with the
  ORACLE moved, not ggen broken. ggen's own features all still work.

  | Feature | Stable jsonv2 | ggen | Failing tests |
  | --- | --- | --- | --- |
  | `format:` (time layouts, base64/base32/hex, nonfinite, emitnull/emitempty) | REMOVED, unreachable | fully supported | `TestStdCompat_NativeTypes`, `_TimeFormatsStdCompat`, `_PointerStruct`, `TestStdCompatMerge_Parity` (2 subtests) |
  | `,string` on bool / plain string | ERRORS (`invalid use of 'string' tag option`) | bool bare, string single-encoded | `TestStdCompat_StringTagStruct`, `TestAppendAny_Struct_StringOpt`, `TestAppendAny_NumberStringTag` |
  | Quoted names `json:"'a,b'"` | REJECTS (malformed tag, `'` invalid at option start) | supported | `TestKeyEscape_QuoteParityWithJSONv2`, `TestQuotedNames_roundtrip` |

  `format:` is the consequential one. Cut for the initial release per
  https://go.dev/issue/79071, pending typed struct tags
  (https://go.dev/issue/74472) as a more expressive replacement for the tag's
  bespoke mini-DSL. It survives ONLY as `ExperimentalSupportFormatTag`, which
  lives in `encoding/json/internal/jsonopts` — internal, explicitly documented
  as inaccessible to public code, and gated behind `goexperiment.jsonv2` on top
  of that. Verified empirically: ggen cannot opt back in, with or without the
  experiment flag. `format:` is a headline ggen feature AND the stdlib is likely
  to reintroduce it in a DIFFERENT shape once typed tags land, so following
  them now risks churning the user-facing tag surface twice.

  Each row decides independently: keep ggen's behaviour + pin a documented
  divergence (and stop the test consulting jsonv2 for that shape), or follow
  the stdlib. The catch-all-map row went the stdlib way — ggen spells it
  `json:",embed"` now (opt #75), which closed that gap. `,string` and quoted
  names are small, real breaking changes if followed. Whatever lands must propagate to
  the three surface docs (README / SKILL.md / cli/CLAUDE.md).

- **`ggen.NotEOF` leaks the drained-vs-transient mapping into generated code
  (2026-08).** Round-6 fix #60 needed the generated stream dispatch loop to map
  a drained refill to the bytes-path grammar sentinel, so `notEOF` was exported
  and now appears at ~378 generated call sites
  (`ggen.NotEOF(err, ggen.ErrExpectString)`). It works and it is honest — the
  emitted code genuinely does what the runtime primitives do — but it publishes
  an error-mapping helper that is really an internal policy, and every generated
  file now carries the idiom. Alternatives not taken: a `Stream` method doing
  guard-plus-map in one call (the two guard sites want DIFFERENT sentinels, so
  it is `NotEOF` with a receiver); `ReadMore` itself taking a sentinel (changes
  a core API for every caller to serve two emit sites); generated code comparing
  `io.ErrUnexpectedEOF` inline (an extra `io` import in every generated file).
  Revisit if a third consumer needs the mapping, or if the dispatch loop is ever
  restructured so one refill helper covers both positions.

- **`-unsafe` / "turbo" generator mode — trust the input completely, drop every
  guard.** Motivation: the 2026-07 jsonv2-parity work (opts #50-52) bought real
  correctness but cost real throughput on some shapes, and there are consumers
  who own both ends of the wire and want none of it. Idea: one flag /
  annotation that strips EVERY safety property at generate time, so the emitted
  code is the fastest thing that can still parse well-formed JSON. Candidates to
  drop, with what the control-checked sweep says each is worth:
    - UTF-8 validation (#50) → NoAlloc **−40% scalar / −28% avx512**,
      RuneGated −35%/−9%, EscapeHeavy −11-13%. Already exists as
      `allowinvalidutf8`; turbo would imply it.
    - Recursion depth cap (#51) → DeepNested −5% scalar. Accepts the
      stack-overflow DoS back.
    - Strict number grammar (#52) → ~0 (it measured flat-to-faster, so
      probably NOT worth dropping — keep it even in turbo).
    - Duplicate-key detection → drops the `seenX` flags + their branches
      entirely (today only declared keys are checked; see Tried Rejected).
    - Required-field / validation / mods → already `-novalidate`.
    - Grammar checks in skip: the `fastskip` idea (rolling quote parity +
      depth counter, no grammar validation) parked under SIMD phase 3 — this
      is where it belongs, and it's the biggest single win available:
      **SkipHeavy is ggen's worst row vs sonic (0.10-0.28×) purely because
      sonic_fast doesn't validate skipped content.**
    - Possibly: `unsafe` bounds elision on the hot cursor (needs care — an
      earlier pointer-arithmetic experiment REGRESSED, see Tried Rejected).
  Shape: composes as a single `//ggen:generate unsafe` that ORs on the existing
  opt-outs plus the new ones, so it is one switch rather than six. Must be
  loudly documented as "well-formed input only — malformed input is UB, may
  crash the process". Open question whether it also implies `-copy` off,
  `ignoreunknown`, etc. Worth doing only if a consumer actually asks; the
  headline number to chase is SkipHeavy, not the UTF-8 rows.

- **maxDepth boundary offset vs jsonv2 (documented divergence, 2026-08).**
  The depth cap counts only containers inside the any/skip/raw subtree —
  acyclic generated-struct levels enclosing it are uncounted (`const _depth =
  0`), so at the EXACT 10000 boundary ggen accepts a document jsonv2 rejects
  (`{"a":` + 10000×`[`). Boundary-exact parity means threading real depth
  through every acyclic struct — codegen churn + a runtime add on shapes the
  cap exists to protect from pathology, for one-off-by-K at a 10000 cliff.
  DECIDED not worth it; the cap's purpose (no stack overflow) is unaffected.

- **jsonv2 double-unescapes `\\` inside quoted tag sections (2026-08).**
  `json:"'a\\b'"` names the field `a`+U+0008 under jsonv2 — it applies Go
  escape semantics to the section, then again to the result. ggen's tag
  unquoting treats only `\'` as an escape, so the same tag names the field
  `a\b` (backslash + b) and a `format:'a\\b 2006'` layout emits those two
  characters, JSON-escaped. Since the round-5 name-escape fix (opt #56) wire
  KEYS are JSON-escaped statically too, so ggen's output is valid JSON and
  self-round-trips (pinned in `keyescape_test.go`); only the SPELLING of a
  backslash-bearing name/layout differs from jsonv2. Matching
  the quirk exactly means replicating a double-unescape nobody relies on —
  revisit only if a consumer feeds ggen tags to jsonv2 and compares keys.

- **base64 `StdEncoding` strips embedded `\r`/`\n` (minor jsonv2 divergence).**
  `{"b":"aG\nVsbG8="}` decodes to "hello" (Go stdlib base64 skips newlines by
  MIME leniency); jsonv2 rejects. Inherited from the stdlib default, one-line
  fix if wanted (pre-check the span for `\r`/`\n` before `AppendDecode`), but it
  costs a scan on every `[]byte` field for an input class nobody sends. Same
  audit batch.

  (The audit's other two finds SHIPPED: unbounded recursion depth → opt #51;
  value-decoder number grammar → opt #52. Its dup-key find was DECIDED as
  intentional — see Tried Rejected.)

- **Clean up validation error path-completion plumbing.** The 2026-08 path
  fixes left structural-assertion smell: `PrependPath` is an exported method on
  every error type but NOT in the `Error` interface, so `ggen.NewParseErr`,
  `Errors.Append`, and `Errors.PrependPath` each assert
  `interface{ PrependPath(string) }` inline. (2026-08: `AddPos` joined as a
  second such method — 29 more one-liners + `interface{ AddPos(int) }`
  asserts in `NewParseErrShift`/`ShiftPos`/`Errors.AddPos` — strengthening
  the shared-embedded-base case.) Candidates: put `PrependPath` in
  the interface outright (external implementors just gain a required method —
  breaking is fine here), or restructure the ~20 error structs around a shared
  embedded base (`Pos int; Path []string` + one `PrependPath` impl) which also
  kills the 20 copy-pasted one-liners. Fold into the CustomError-shape revisit
  below if both happen at once.

- **Revisit `ggen.CustomError` shape.** Today `{Field, Name string, Cause
  error}` + `Unwrap()`. Rough edges: `Name` doubles as rule identifier and
  user-facing label (split into `Rule` + `Name`); no `Value any` field like the
  other typed errors (can't expose what the validator rejected); `Cause` is a bare
  `error` (a typed sub-interface could improve `errors.As`). Pick when a concrete
  report-shape ask exists.

- **`validation.*` error position follow-up.** Errors carry `Pos int` (full-payload
  byte offset). Open if ever wanted: a `Snippet []byte` around the failure offset
  (rejected for now — the caller has the input + `Pos`).

- **`pipe:` tag follow-ups.** Foreign-package converter inputs (import
  plumbing — `classifyConverter` still spells W via `types.RelativeTo`, the
  full import path for a foreign W; the type-side `pkgQualifier` +
  `TypeImport` table from the round-10 qualifier work is the natural
  carrier); `@pkg.Func` references (`customfunc.go` `lookupFunc` → `PkgName`)
  still spell the DECLARED package name and import by path, while the
  type-side qualifier table renames a package to `leaf2` when two packages
  of the pass share a declared name — a converter/validator from such a
  package would mismatch its import line (route `PkgName` through
  `structSet.qualifierFor` if anyone hits it); converter pointer inputs
  deeper than one level (`func(**int) T`) take the multi-level pointer field
  path but are untested (only `*int`/`*int64` are pinned); top-level
  alias-type `pipe:` support (needs a non-dispatch null branch in the alias
  renderers); CONTAINER converter inputs (`@Conv` with W = []T/map) —
  currently rejected at parse (2026-08, was silently-broken codegen before),
  to support: populate converterInputField's ElemType/ElemKind/ElemIface from
  go/types — the dedicated-kind element delegation (R3/R4, cbf0949/816fd3c)
  makes the emit side workable now. Cosmetic: `checkVariantShapes`'
  diagnostics come out double-qualified ("Doc.M: Doc.M: decode variants …
  both claim the same JSON shape") — the message already carries
  Struct.Field and `resolvePipeCustoms`' caller prefixes it again via
  `qualifyRichErrors`.

- **`nullzero` follow-up.** Extend the per-field `nullzero` decode variant to
  top-level alias types (needs a non-dispatch null branch in the alias renderers).

- **Map value reuse: scalar-valued and stream-path maps still rebuild (2026-08).**
  Bytes-path maps whose values own allocations now read the carried map and
  fill a new one (opt #76) — heavy values measured −57% wall clock, 92% less
  garbage, GC eliminated. Two gaps remain, both deliberate:
    - **The stream path does not swap.** `emitReceiverReset` keeps its
      `clear()` there (and the pointer seed clears a stream map leaf), so
      stream decode still allocates every map value fresh — a pointer-to-map
      FIELD on the bytes path is handed to the swap intact through the seed
      and does reuse its values. The swap needs the same shape plus a check
      that nothing holds a buffer-relative alias across the entry loop; worth
      doing only if a stream consumer shows map values as a hotspot.
    - **Light values trade bytes for allocation count.** With small values the
      swap is flat on time, collapses allocs (513 → 9), but roughly DOUBLES
      B/op — a fresh map where `clear()` recycled buckets. Which side wins is
      decided by the size of the values at runtime, which the generator cannot
      see, so the gate (`reusesMapValues` + `ownsAllocations`) can only ask
      whether a value owns anything at all. A per-field opt-out (or opt-in)
      would let a caller who knows their values are tiny keep the old shape;
      no one has asked yet.

- **Consolidate the per-field behaviour knobs into one concept.** Today a field's
  shape is steered by four unrelated mechanisms with four different syntaxes and
  four different scopes: `pipe:` value steps (mods `@Func`/`trim`/`clamp`, run
  in declared order, decode-side only), `pipe:` decode-stage variants
  (`@Conv` converters + `nullzero`, selected by wire shape, need a `/`/`.`/`~`
  signal to not be read as a value step), struct-level annotations
  (`htmlescape`, `-nullzero`, `-copy` — global-or-per-struct, encode AND decode),
  and type aliases (`//ggen:generate htmlescape type HtmlString string` — the
  only way to get per-FIELD encode behaviour, and only by minting a type). The
  seams show: `nullzero` exists as both a struct annotation and a decode variant;
  `htmlescape` is per-struct or per-alias but never per-field; converters are
  decode-only with no encode counterpart, so a `@FromMoney` field silently
  marshals as its native type. Idea: one uniform per-field step vocabulary where
  a step declares its own stage (decode-shape / value / encode) and scope, so
  `htmlescape` is just an encode step, `nullzero` just a decode step, and a
  converter can carry an inverse for marshal. Open questions: does an encode
  step break the "wire shape is decided by the type, not the tag" invariant the
  alias design deliberately picked (README says so explicitly)? Is a bidirectional
  converter pair worth the tag syntax, or is an alias with methods the right
  answer? Does collapsing the stages cost the grammar its current
  ability to classify a step without consulting the signature? Big breaking
  change to the whole user-facing tag surface — only worth it if it comes out
  genuinely smaller to explain, not merely more uniform.

- **`structHasAppendFormatTime` only sees field-level KindTime (round-8 find,
  probe-negative).** The 64-byte AppendFormat headroom reservation skips
  `[]time.Time` / `map[string]time.Time` / `sql.NullTime` elements. A 3-elem
  `[]time.Time` probe did NOT realloc (AppendFormat grew within budget on
  current Go), so this is an inconsistency between the reservation rule and
  its coverage, not a demonstrated bug. Revisit only if a time-slice realloc
  ever shows up.

- **Generated `omitzero` ignores an `IsZero() bool` method (2026-09).** The
  guard for a user struct is the structural `zeroCompare`, where jsonv2,
  v1 1.24+ and `AppendAny` all call an `IsZero() bool` method when the type
  has one — `KindTime` is the only kind ggen honours it for. An IsZero arm
  when go/types says the type has the method is easy. Decide: fix the
  emitter, or pin the divergence in the three surface docs. Related decision
  already taken: `omitempty` on `url.URL` tests `!= (url.URL{})` — a non-zero
  URL that still stringifies to `""` (e.g. only `OmitHost` set) is emitted as
  `""`; exact parity would need `String()` (allocates) or a per-component
  test, not worth it.

- **`url.URL` `OmitHost` + `//`-path branch missing from `appendURLRaw`
  (2026-09, unfixed).** Go 1.27's `url.URL.String` escapes the first `/` of a
  path starting with `//` when `OmitHost && Host == "" && User == nil` so
  re-parsing does not turn the path into an authority; ggen emits it raw:
  `url.URL{Scheme:"file", OmitHost:true, Path:"//host/p"}` gives ggen
  `file://host/p` vs stdlib `file:%2F/host/p`. A few lines after the
  `hasPath` slash check; add a row to `TestAppendURL_Construction`.

- **`case:ignore` could be implemented rather than refused.** A
  `strings.EqualFold` fallback arm after the exact-match switch (jsonv2
  semantics: exact match first, then case-insensitive). Refused for now
  under the no-silent-no-op rule; no consumer has asked.

- **Tag-grammar leftovers (2026-09).** `format:` with an empty value
  (`json:"t,format:"`) is accepted with `Format=""` (silently no format)
  where jsonv2 errors "cannot have empty value for `format` tag option".
  `boundFits` (and the gt/gte/lt/lte gate) rejects an integer-valued float
  spelling like `gt=1.0` on an int field as out of range although Go accepts
  the constant (`oneof=1|1.0` is caught as a duplicate first) — over-strict,
  harmless. `[0]byte` is unsupported: `foldByteArray` folds it onto KindBytes
  with `ArrayLen 0`, indistinguishable from `[]byte`, so the nil-check emit
  does not compile; jsonv2 base64s it as `""` — either skip the fold for a
  zero-length array (then it is a `[]` tuple, diverging from v2) or teach the
  byte-array emitters N==0; degenerate shape. `[N][M]byte` elements resolve
  as KindArray and take the numeric-tuple route rather than base64, unlike a
  `[M]byte` field — worth a look if anyone uses that shape.

- **Output-name residuals (2026-09).** `emitScope` maps every non-alnum rune
  to `_` via `sanitizeIdent`, so `a-b.go` and `a_b.go` in one package still
  share the oneof scope `a_b` (loud at compile time as a redeclaration);
  making the mapping injective would rename every `ggenCap`/`ggenOneof` in
  every generated file for a layout nobody has. Single-file mode layout hole,
  now explicit in `passTypes`: a dependency declared in file B and reached
  only from file A's root is generated by nobody (A filters to its own file,
  B has no root reaching it) and stays on the json fallback; package mode
  generates it — `passTypes` is the seam if package-wide single-file
  generation is ever wanted. AST-only (degraded, no go/types) mode still
  spells foreign types by the source's local import name and cannot alias a
  colliding `json`/`time` user package — cross-package types route to
  `encoding/json` there anyway. The external-test bucket + `-pkg X` →
  `package X_test`, and the hook half of `structSet.inspect`'s stale-method
  masking (`MarshalJSON`/`UnmarshalJSON` from a previous run's `_ggen.go`),
  are unpinned by tests.

- **`AppendAny` accepts text-marshaler map keys of any kind; the generator
  does not (2026-09).** `type K int` with `MarshalText` marshals through
  `AppendAny` (jsonv2 + v1 both do); the decided rejection
  (`TestAppendAny_NonStringMapKey`) now covers only key types WITHOUT a text
  marshaler. The generator still rejects any named key type at parse
  (`map key must be string`), so the accepted shape is reachable only
  through `AppendAny`.

- **`Float64` surfaces `*strconv.NumError` (ErrRange) for an out-of-range
  float64 (`1e400`) while `Float32` maps the same condition to
  `ggen.ErrNumberOverflow`** (the identity `TestNarrowFloatOverflow` pins).
  Unifying `Float64` on `ErrNumberOverflow` is a small breaking change worth
  doing if error identity is ever tidied.

- **Generated `strings` import for a container ALIAS of `net.IP`/`netip`
  elements.** The netip error clone relies on the same plumbing as
  `renderStreamNetIP`'s clone — every non-alias struct adds `strings` for
  `streamUnknownKey`; a container alias of those elements that emits a
  stream decoder might lack the import. Pre-existing class, not verified,
  cheap to probe.

- **archsimd masked byte loads — re-audit trigger.** If archsimd ever exposes
  a fault-suppressing k-masked byte load from memory (VMOVDQU8 with a
  k-mask, which AVX-512BW guarantees suppresses faults on masked-off lanes),
  the stack-copy arm in `StringAVX512`'s tail can go. On go1.27 the only
  masked memory op for 8-bit lanes is `StoreArrayMasked`; `Uint8x64.Masked`
  is documented "Emulated" (a vector-domain AND after an unmasked load),
  which is exactly why `LoadUint8x64Part` over-reads.
  `LoadUint8x16Part`/`LoadInt8x32Part` stay safe (scalar 8/4/2/1-byte
  element loads).

- **A NAMED func or channel field type is silently broken (2026-09,
  unfixed).** A field whose type is `type Handler func()` is not rejected —
  the parse-level refusal covers type LITERALS, and a named type stops the
  walk — but every reset site emits `result.H = Handler{}` and the generated
  file does not compile, even when the named type carries a
  `MarshalJSON`/`UnmarshalJSON` pair. The fix is in the emitter, not the
  parser: the zero-value form already emitted for interfaces,
  `(Recv{}).Field`. Once that lands, the parse-time rejection of func/chan
  LITERALS could be revisited too — and the diagnostic's remedy hint could
  offer "declare a named type", which today it must not.

- **gc internal compiler error on a dead store into a zero-length array
  (Go 1.27.1, worth filing upstream).** Assigning into an element of a
  `[0][3]int` — the shape a `map[string][0][3]int` decode loop produced —
  makes the compiler abort with "can SSA LHS mv[idx0] but not RHS" instead of
  dead-storing it. Nothing in ggen is blocked (the `[0]T` readers have no
  element loop at all, cli opt #85), but the toolchain bug stands.
  Reproducer kept at `~/audit-round10/work/probe1`.

# Tried Rejected

- **Folding the bool give-up walk into `ggen.Bool` — REJECTED, measured
  (2026-09, inline cost).** `Bool` costs 54; every variant carrying the
  give-up position — `litEnd` folded in (117), a call to a cold `boolEnd`
  (114), unsafe loads (112), a true-only shell + slow call (95–96), a 4-byte
  switch (108) — exceeds the 80 budget, and `Bool` IS inlined at all 12
  reported generated call sites in bench today. The position lives in the
  cold exported `BoolEnd` called on the error branch only (cli/CLAUDE.md
  opt #82). Do not re-propose folding it in without a house-rule bench
  showing the de-inlined call is free.

- **The bytes verdict for a control byte in an unterminated string
  (`ErrUnterminated`) — REJECTED as the unification target (2026-09).**
  Bytes `String`/`skipString` located the closing quote first and, finding
  none and no backslash, returned `ErrUnterminated` without classifying the
  open tail, while every stream scanner judges each window before refilling
  and reported `ErrBadString` — `"abc\x01` split bytes vs stream at every
  chunk size, and even `CaptureValue` (bytes skip over the window) vs
  `SkipValue` on one Stream. Resolved TOWARD the stream/jsonv2 verdict (a
  ctrl byte before EOF is malformed whatever follows) by adding
  `hasCtrlByte` to the bytes no-quote arms: the bytes quirk was an ordering
  artifact, never a chosen contract, and deferring the stream's ctrl verdict
  to EOF would read unbounded input on malformed data and break the lazy
  fail-fast/liveness design. `ctrlHitErr`/`ctrlHitPos` deleted with it.

- **The kind sentinel for `n`-prefixed garbage at a null-accepting value —
  REJECTED as the unification target (2026-09).** The bytes null peek
  reported the field's kind sentinel (ErrBadArray/ErrBadObject/
  ErrExpectString/ErrBadBool/ErrBadNumber) for `nulx`/`n}`/`nope` while the
  stream reported `ErrBadLiteral`. Unified on `ErrBadLiteral` by moving the
  BYTES path: the stream's null-literal refill → ErrBadLiteral is the
  round-9 shape, the pipe variants already reported it on both paths, and
  jsonv2 reports a literal error there; the reshaped `inlineNullPeek` keeps
  the same first compare on the non-null path. Don't re-open in the other
  direction without a consumer that needs the kind sentinel.

- **Promoted-field Go-name clash resolved stdlib-style (keep both) —
  REJECTED for now (2026-09).** Full parity (keep `E1.A`/`E2.A` with distinct
  JSON names) needs an embedding path on `FieldInfo` and emitter changes so
  `result.<GoName>` / `seen<GoName>` / cap-const and temp names are spelled
  through the path (`result.E1.A`, `seenE1_A`). Rejected at generate time
  instead (cli/CLAUDE.md opt #78); revisit only if a consumer has such a
  layout — the shape is uncommon.

- **Duplicate-key detection in skipped / `any` / raw / nested scopes.** ggen's
  `DuplicateKeyError` comes from the per-field `seenX` flags, so it covers only
  DECLARED keys of the object being decoded; dups pass silently inside
  `ignoreunknown`-skipped objects, `any` values (map last-wins), RawMessage
  spans, and nested sub-objects. jsonv2 rejects dup names everywhere. DECIDED
  intentional (2026-07, user call): the contract is "the fields ggen actually
  DECODES are unambiguous; content it never interprets is not policed". A dup
  inside a skipped span cannot change the decoded value (the span is
  discarded), `any` last-wins matches Go map semantics, and raw spans are
  passed through verbatim for the consumer to judge. Enforcing it would need a
  seen-set per skipped/any/raw object scope — an allocation on paths that are
  currently allocation-free — to buy strictness with no effect on any decoded
  result. `-allowdups` already covers the half that DOES matter. Don't
  reintroduce without a concrete consumer that needs wire-level dup rejection.

- **UNCONDITIONAL per-value buffer compaction in `(*Stream).Seq` — REJECTED;
  the `len == cap` gate SHIPPED (2026-08).** Dropping consumed values before
  each decode is genuinely needed: generated container decoders refill
  grow-only (`ReadMore(0)`, 8 sites in `bench/small_ggen.go`, 75 in
  `mega_ggen.go`), and ReadMore doubles whenever `len == cap`, so a 422 B
  `Node` through a 1 KiB buffer ratcheted it to 4 KiB over 200 values. But
  compacting on EVERY value memmoves the unread remainder each element:
  20000 tiny values measured **2.9x at 4 KB (240 -> 686 us), 25x at 64 KB
  (235 us -> 5.95 ms), 25x at 512 KB**, plus +4% on the realistic Node stream
  at 64 KB. Shipped gate is `s.Pos > 0 && len(s.buf) == cap(s.buf)` — exactly
  ReadMore's own grow condition, so it fires only when the next refill would
  double. Cost with the gate is ~2-3% on the tiny micro and noise on Node.
  An extra `s.Pos*4 >= len(s.buf)` amortization term was tried: reproducibly
  worth only ~1.5pp on the tiny micro (1.7% vs 3.2% overhead) and nothing on
  Node, so it was dropped as not paying for the added concept.
  Gate fire rate, instrumented over 200 Node values: eager readers
  (`bytes.Reader`/file — ReadMore fills `buf[len:cap]` in one Read) hit
  `len == cap` at 200/200 value starts on a 64 B buffer, 150/200 at 1 KiB,
  59/200 at 8 KiB; CHUNKED readers (1/7/64 B) never reach cap, fire 0 times,
  and show no growth — the gate is firing exactly when growth is possible.
  Note the self-limiting shape: small buffers fire often but memmove little,
  large buffers memmove a lot but fire rarely.
  TWO METHODOLOGY FAILURES here, both mine, both worth remembering:
  (1) the first pass concluded "buys nothing" because it probed with a
  hand-written decoder calling `(*Stream).Slice` — which COMPACTS — so it
  never exercised the grow-only EMITTED refills that are the entire problem.
  Probe generated decoders, not runtime helpers, for stream buffer growth.
  (2) the first cost numbers (43x for unconditional, 1.55x for the
  `len == cap` gate) were taken under the power-saver profile with in-process
  `go test -bench` and no discarded warmup. Re-measured per the house rules
  (performance profile, prebuilt `go test -c` binaries, warmup discarded, one
  pinned pass each) the unconditional cost is 25x and the gated cost is 2-3% —
  the 1.55x was pure artifact and nearly cost a correct optimisation. The
  backlog already says in-process `-bench` runs are not comparable on micros;
  it applies here too.
  Pinned by `TestSeqBufferStaysBounded` (integrationtests, verified to fail
  without the compaction).

- **`VPERMB` whitespace classify in `skipSpaceAVX512Slow` (opt audit #10).**
  Replaced the avx512 4×Broadcast+4×Equal+4×ToBits+3×OR classify with one VPERMB
  LUT lookup (`wsClassLUT.Permute(v).Equal(v)`, `wsClassLUT[c&0x3F]==c` iff c is
  WS; correct per exhaustive 256-byte probe + SIMD parity). Measured across three
  indent widths (control-normalized against the compact row, which never runs the
  classify): 2-space +0.35% (loss), 4-space ~neutral, 8-space −1.16% (win). The
  effect is ~1% — comparable to the build-to-build code-LAYOUT noise (a 4-space
  A/B showed the whitespace-free compact row swinging +3.1% between the two
  separately-compiled binaries, i.e. pure layout drift). It only clears zero on a
  pathological 8-space/91%-whitespace/65 MB payload, and even there it's near the
  noise floor. The whitespace skip is memory-bandwidth-bound (loads dominate), so
  VPERMB's fewer µops don't convert to wall-clock, and its permute→equal latency
  loses to the 4×Equal chain's ILP on short (typical) runs. The shipped SkipSpace*
  inlinable shell (audit #4) already took the real compact win (−9.7%). Don't
  retry without a cache-resident, run-length-heavy payload where the classify
  (not the loads) is the bottleneck.

- **Vector number-skip tier `skipNumberAVX512` (opt audit #19).** Mirrored scalar
  `skipNumber`'s RFC-8259 grammar in a tier fn with the 3 digit runs vectorized via
  `skipDigitsAVX512` (8-byte scalar prefix gate → 64-byte range classify
  `v.GreaterEqual('0') & v.LessEqual('9')`). Byte-exact (SkipValue/SkipNumber SIMD
  parity + exhaustive per-byte probe pass). But interleaved A/B REGRESSED
  SkipHeavy/compact/ggen +11.36% at avx512. Root cause pinned by a probe: making
  `skipDigitsAVX512` a PURE-SCALAR loop (no vector) INLINES (small) and is flat/
  −1.4%; the vector loop makes it too big to inline, so it becomes ~3 non-inlinable
  CALLS per number. 0.48 × (skipNumber's 23.5% flat) ≈ +11.3%, matching the
  measurement — it was pure call overhead, not the vector. Even inlined (×3
  literal duplication), SkipHeavy's ≤19-digit numbers are too short: swapping ~10
  OOO-overlapped scalar iterations for one vector load+classify is break-even; the
  vector only wins at ≥40-digit runs, which real JSON doesn't have. Don't retry —
  the shape is wrong, and the inlining tax makes it strictly worse.

- **SWAR 8-byte clean-span in the scalar encode escape walk (opt audit #3).**
  Rewrote `AppendString`/`AppendStringNoHTML` to classify 8 bytes/iter via a
  uint64 load (`hasless(<0x20) | haszero(^'"') | haszero(^'\\')`, HTML +3 terms)
  instead of the per-byte `[256]bool` table probe. Byte-exact (pinned by an
  exhaustive escape-at-every-word-seam parity test). Direct micro WON big
  (`BenchmarkEscScalar` clean strings: −15% @8B … −48% @256B, geomean −33.6%,
  p=0.000), but interleaved Mega_Marshal REGRESSED +3.4% (ggen) / +3.1%
  (presized), p≤0.001. Same lesson as the SWAR int-parse rejection: the SWAR
  mask is a ~10-16-op dependency chain; in the memory-bound marshal walk the
  predicted per-byte loop OOO-overlaps with the tree walk and the table stays
  L1-hot, while Mega's strings are short-skewed (keys 4-11 B, tags 4-13 B) so
  SWAR barely engages yet adds per-call setup. Don't retry without a length gate
  AND an in-situ win (the micro is not the test).

- **Window-gated inline stream int digit loops (opt audit #7).** Emitted, at each
  struct-field stream int site, a gated inline scan over `s.Bytes()` (when
  `s.Pos+21 <= len`, local cursor committing `s.Pos` only on success — matching
  `(*Stream).Int64/Uint64` error identity + position; window-edge/leading-zero
  runs fall back to the refill-capable call). Correct: `FuzzStreamEqualsBytes`
  3.75M execs clean. But interleaved A/B showed NO win — NoAlloc_Reader/ggen_stream
  (the best case, most int fields) +4.75% (p=0.060, directional regression),
  Small_Reader stream flat. Stream Int64 is 13% flat of Mega_Reader but the tier
  is memory-bound (string-copy mallocs + ReadMore dominate — backlog already
  notes "not SIMD-addressable"), so removing the per-call bookkeeping doesn't move
  wall-clock and the extra generated code adds slight overhead. Consistent with
  the shipped stream tier measuring "Mega_Reader flat" and the rejected
  "Stream-path `_s.SkipSpace` inliner". Don't retry on this tier.

- **SWAR 8-digit integer parse (Lemire IsDigits8/Parse8) — in-situ.** Fully
  implemented 2026-07 across bytes `Int64`/`Uint64`, stream mirrors, and the
  emitted inline digit loops; bit-exact (a 400k-run reference differential
  caught an OR-vs-XOR classifier bug — `,`=0x2C passed the naive combined
  nibble check; the test survives as `TestInt_ReferenceDifferential`).
  Interleaved A/B REGRESSED both tiers: Mega +7.5% (avx512) / +4.7%
  (scalar) despite 18-19-digit Int63 IDs, NoAlloc +7-8.5%, Mega_Reader +2%;
  only scalar Tiny won (−4%). The 3-dependent-multiply Parse8 chain
  (~9-12 cy) + IsDigits8 + the extra loop branch lose to 8 predictable
  1-cycle scalar iterations that OOO-overlap with surrounding decode work —
  the standalone "−33% on 19-20-digit runs" microbench measured the parse
  function in isolation, where nothing competes for the pipeline. Same
  lesson as "removing decode inliners" and "direct-write AppendInt": don't
  trust per-call micro-benches for code embedded in the generated hot loop.
  Don't retry without an in-situ interleaved win.

- **Branchless register-bitmap escape predicate ([14] alternative).** Test "needs
  escape" via two uint64 bitmaps (escaped bytes all < 0x80): `(escLo>>c |
  escHi>>(c-64)) & 1`. ~2.9× slower than the `[256]bool` table. Asm shows it's
  branchless but ~14 instrs/byte vs the table's ~4 — one L1 load beats the ALU
  sequence. Lesson: a register bitmap only wins when it replaces MORE work than a
  load; for a single membership test the table's load is fewer ops. Don't retry
  without an asm argument for fewer instructions.

- **Inline leaf-struct AppendJSON at nested marshal sites ([22]).** Emit a small
  infallible leaf struct's append body inline (vs a call), coalescing the parent
  prefix with the leaf's `{"…":`. Flat on real Mega_Marshal; the only win was a
  contrived cache-resident `[]Pt` bench, and it added codegen branching. Not worth
  it. Don't re-propose [22].

- **256-byte class-bitmask charset predicates ([4] half).** Replace the range-check
  loops in IsAlphanum/IsNumeric/IsHex/IsLower/IsUpper with a shared
  `[256]uint8` class table (`table[c]&mask`). Regressed the long-string case — the
  per-byte L1 load can't hide behind the loop the way branch-predicted range
  comparisons (pure ALU, no memory traffic, perfectly predicted on valid input)
  do. Lesson: adding memory traffic to a tight branch-predictable loop loses. Keep
  the range checks.

- **Fused span-scan + Eisel-Lemire in `Float64`.** Walk the number span once,
  accumulate mantissa + exp10, run `eiselLemire64` vendored from Go's `strconv`
  (~830 LOC + 11 KB power-of-ten table), fall back to `strconv.ParseFloat` on a
  miss. A real measured win, but not worth permanently vendoring + maintaining a
  copy of the stdlib's internal EL implementation. If the stdlib ever exposes a
  fast float parser (or `ParseFloat` gets fast enough), revisit. Don't re-vendor EL.

- **Gated stream string slab.** Real alloc win but cut: chunk-size gating is
  load-bearing (un-gated regresses small payloads) and the `unsafe.String`-into-
  never-rewritten-chunks lifetime reasoning carries corruption-class risk not worth
  the alloc cut. Bytes path already aliases; stream alloc reduction isn't a priority.

- **Map decode buffer-then-build.** Buffer `pairs`, `make(map, len(pairs))` + fill
  at `}`. Micro-wins but B/op grows at high entry counts and an absolute regression
  at low counts; only pays on map-heavy schemas. Maps keep the unsized `make()`.

- **Map `:`-count prealloc (sibling of the slice comma-count, opt #42).** Sizing a
  `map[string]<numeric|bool>` from `bytes.Count(span, ':')+1` is a
  memory-amplification footgun on WELL-FORMED input — JSON object keys are strings,
  so one valid entry with N colons in the key inflates the count into an N-sized
  `make`. Unlike the slice comma-count (scalar elements can't carry `,`/`]`, so
  inflation requires malformed input that errors before the alloc matters), the map
  key is data-controlled on the happy path. Robust map sizing = runtime
  buffer-then-build, not a delimiter heuristic. Don't reintroduce a key-delimiter
  count.

- **Length-gated SWAR string-SPAN scan (closing-quote locate).** The control-byte
  VALIDATION half landed (SWAR `< 0x20`, length-gated). The closing-quote/backslash
  LOCATE half is deliberately NOT done: `bytes.IndexByte` is already SIMD/AVX2 and
  beats a SWAR span scan on long spans. Quote/backslash locate stays on
  `IndexByte`. Don't fold them into SWAR.

- **ConsumeColon fast-path header ([5b]).** Bespoke header on `(*Stream).
  ConsumeColon`. Dead flat — [5] already inlines `SkipSpace` into it, and the
  per-key separator is a negligible fraction of stream cost (string copies +
  ReadMore dominate). Pure code weight, no win.

- **Indexed marshal slice loop ([23] Layer 1).** Premise: `for _, v := range
  ref[1:]` copies each struct element into the range var AND again into the
  value-receiver `AppendJSON`. Wrong for go1.26 — range-by-value and indexed both
  emit one element copy/iteration; gc folds the range var straight into the
  receiver slot. The single remaining copy is the value-receiver arg pass,
  removable only by a pointer receiver. No-op; don't reintroduce.

- **Pointer-receiver decode cores ([23] Layer 2) — vetoed.** Replace value-receiver
  decode struct-copy traffic with `(*T).decodeFrom` cores + value-receiver shims.
  The public surface is pinned by `ggen.Decoder[T]` (`DecodeFrom(data) (T, int,
  error)`) and `T{}.DecodeFrom(data)` ergonomics — a bare `*T` receiver breaks the
  generic walkers. The copies it removes are cold-path stack writes
  (store-buffer-absorbed); likely measures flat while carrying large churn. Only
  `DeepNested` might show it. Prototype + asm-confirm + interleaved A/B BEFORE the
  codegen rewrite.

- **Direct-write `ggen.AppendInt/AppendUint` replacing strconv.** Implemented
  (digit count via `bits.Len64`+pow10, backward two-digit fill, parity-fuzzed).
  Wins on small ints but PAR on large — not worth ~15 emit sites + a custom
  formatter for a small-int-only win. strconv keeps base-10 paths.

- **Static comma fusion past one conditional field in `renderAppendJSONBody`.**
  Per-field comma state machine so fields after an omitempty/omitzero guard keep
  fused `,"key":` constants. Dead flat — the predictable compare+branch+1-byte
  appends are pipeline-hidden under escape-scan/memmove. Wire bytes identical, so
  not worth it for cleanliness either. Follow-on to opt #20; don't redo.

- **KindBytes inline string scan → `ggen.String` call.** Regressed Mega — another
  confirmation that replacing inline scan code with runtime calls loses regalloc/
  BCE context (generalizes the "removing decode inliners" rejection below).

- **Un-gated SWAR string scan at all decode sites.** Regressed Mega (register
  pressure in the large DecodeFrom). The length-gated variant works — the gate is
  the point.

- **Exact-only float fast path (span-fused, no Eisel-Lemire).** Regresses ~16%
  per-number on 17-digit floats: wasted mantissa accumulation + full ParseFloat
  redo. Half-measures on the number path are worse than nothing — ship fused
  Float64 only with the EL arm (which itself was then rejected, above).
  SUPERSEDED 2026-07 by the LENGTH-GATED variant (`scan.exactShort`, ≤16 B
  span gate in `Float64`) — the gate structurally excludes 17-digit floats
  (shortest form ≥ 18 chars), so the regression class never enters; −7.6%
  NoAlloc. Same "the gate is the point" precedent as the SWAR scan. See
  .claude/scan.md.

- **`inlineNullPeek` → uint32 compare.** Mechanism real (`ggen.Null` ships it) but
  the peeks are ~0.07% of decode; never a perf bet. Idiom cleanup at best.

- **Flat-CPU-share ⇒ wall-clock extrapolation (methodology).** Flat CPU share in a
  profile does not predict wall-clock — the Mega marshal walk is memory-latency-
  bound on a cold large tree. Profile shares alone don't justify landing;
  interleaved end-to-end A/Bs only.

- **Lazy per-key container reset (retain omitted slice/map keys).** Reset-at-entry
  is the WANTED contract — a blank/partial payload must yield a blank slate for
  containers while keeping capacity; stdlib's retain-on-omit merge is explicitly
  not a goal. Divergence pinned in
  `TestStdCompatMerge_IntentionalDivergences/omitted_container_reset_vs_retain`.

- **Length-first key dispatch (`switch len(key)` + nested `switch key`).** gc
  already lowers string switches to length-grouped binary search / jump tables —
  the manual outer switch only added a redundant layer the compiler can't see
  through. Flat-to-worse vs flat `switch key`. Don't reintroduce without re-running
  the 100-field mixed-length-name fixture.

- **Generator emitting `go/ast` nodes instead of text.** Full rewrite (branch
  `ast-conversion`, commit `feadbba`), output byte-identical. Rejected: less
  readable (pointer-struct trees you can't skim), higher peak RAM, marginally
  slower codegen, larger binary. Kept on branch in case `ast.Walk`-based
  optimization ever justifies it, but nothing does today.

- **Pointer-arithmetic decoder / `unsafe.Add` byte loads.** Cut bounds checks but
  Unmarshal regressed ~10%. Modern AMD64 makes never-taken bounds checks ~free
  (predicted), while `unsafe.Add` defeats compound addressing: `data[i]` is one
  `MOV (base)(idx*1)`, the unsafe form takes 2-3 (optimizer treats it as opaque,
  loses loop-invariant hoisting). Don't retry unless targeting a CPU where
  bounds-check branches mispredict.

- **Removing all decode-side inliners** (inlineSkipWS/ScanInt64/ScanUint64/
  ScanString) for plain `ggen.X(...)` calls. Per-call overhead is noise, but macro
  Unmarshal regressed ~15-20% — inlining matters for register allocation across
  adjacent ops, ICache, and compound BCE the compiler only does with the body in
  scope. Don't trust per-call micro-benches for hot-loop inlining.

- **Stream-path `_s.SkipSpace` inliner** (`inlineStreamSkipWS`). Saved the
  method-dispatch frame but kept `_s.Ensure(j+1)` in the loop — `Ensure` cold path
  dominates Stream throughput, so the normalized win was noise. Don't retry without
  tackling Ensure overhead.

- **Inlining `ggen.Bool` / `ggen.Float64`.** Call frame fully amortized by body
  work for primitives at this size; call version slightly faster. No win.

- **`Ensure(p *int, n int)` + `Anchor`/`Unanchor` for bounded streaming.** Original
  primitive bulk-fetched N bytes by looping `Read` internally, with window-shift +
  Anchor/Unanchor to freeze offsets across `SkipValue`. Killed by: (1) looping on
  Read is the antithesis of lazy streaming (`io.ReadAll` is simpler); (2) anchor +
  `*int` cursor adjustment was a stale-position bug source. Replaced with
  `ReadMore(keep int) error` (single Read, optional in-place compaction) +
  byte-by-byte literal scans. Don't reintroduce bulk-fetch without a fail-fast
  story that keeps lazy semantics.

- **Stream `Acquire`/`Release` pool with reused buffer.** Pooled `*Stream`,
  `Release` truncated `s.buf` to retain it. Combined with alias-mode strings this
  is **silent corruption**: the next `Acquire` reuses the buffer, `Read` overwrites
  bytes, and prior decoded values' aliased fields flip content. Caught by a
  two-payload probe; a residency bench missed it. Replaced with stack-allocated
  Stream + caller-owned buf + copy-mode strings.

- **`[512]byte` inline scratch in `Stream`.** Stack-resident scratch to avoid the
  buffer heap alloc on small payloads. Failed: escape analysis couldn't prove `&s`
  safe across `DecodeFromStream(&s)` inside the then-generic `UnmarshalStream[T]`
  wrapper, so the whole Stream heap-escaped. The wrapper is now gone and the call
  site is direct, so the constraint may no longer apply — worth re-measuring if a
  residency push needs small-payload alloc back.

- **Per-decode arena + `StreamArenaSize`/`StreamArenaCompact` codegen.** Parse with
  aliased strings, sum string bytes, allocate one exact-size arena, copy + rewrite
  headers. Allocs/B fell but residency and wall clock were unchanged — the gap was
  never per-string fragmentation, it was per-decode buffer retention + map-rebuild
  allocs (Go has no in-place key-rewrite). If retrying: prove the residency gain
  BEFORE shipping codegen.

- **`maxlen=N` as slice/map prealloc hint.** `maxlen=64` → `make([]T,0,64)` to skip
  the growth chain. Hidden cost: every retained value carried the over-allocated
  cap forever — killing it was the biggest single residency win in the whole
  exploration. Only `len`/`minlen`/`hint:` drive prealloc now. Don't reintroduce
  `maxlen` as a sizing hint without an opt-in mechanism (see `hint:`).

# Future

- **Lazy streaming iteration over an unending reader (iter.Seq).** User idea
  2026-08: parse values lazily off a never-ending stream (stdin, socket,
  NDJSON log tail) and yield them as they complete — shape sketch:
  `ggen.Iter[T](r io.Reader, buf []byte) iter.Seq2[T, error]` (Go 1.23
  range-over-func; a channel variant forces a goroutine + handoff cost and
  loses backpressure/cancellation ergonomics — Seq pulls on demand, caller
  breaks to stop). Two input shapes worth covering: elements of one huge
  array (`[a,b,c,…` — UnmarshalSliceStream's loop, yielded instead of
  appended) and concatenated/NDJSON top-level values (skip inter-value WS,
  decode, repeat). Builds on the existing Stream machinery; the CaptureValue
  liveness lesson applies (never Read past a completed value — deliver it
  first, refill on the NEXT pull, so a quiet socket can't stall a yielded
  element). Stream-path strings are already copies, so yielded values own
  their memory and the buffer recycles between pulls.

- **Validation-derived encode hints.** Use field rules for encode shortcuts:
  `alphanum` → skip the escape table; `lte=N` → fixed-width digit formatter instead of
  `strconv.AppendInt`; similar for `oneof`/`len`. Real wins on hot fields, but
  couples encode shape to decode-time validation — the same field would marshal
  differently based on its rules, blurring the marshal contract. (Decode-side
  prealloc already uses `len`/`hint:` — that's a `make` cap hint, not a wire-shape
  change.) Shelved unless a target schema makes the win concrete.

- **Streaming `io.Reader` over marshalled output (state-machine codegen).**
  Per-struct `AsReader()` returning resumable state + `ggen.Reader[T](v)`
  exposing `io.Reader`. Suspends mid-marshal so peak memory = the caller's `p
  []byte` instead of `JSONSize()`. Only matters when a single payload is too big to
  materialize — `JSONSize()` fits comfortably in RAM for everything we care about.
  Shelved unless multi-GB request bodies show up; `bytes.NewReader(Marshal(v))` is
  a one-liner users can write.

- **SIMD phase 3 (phases 1+2 SHIPPED — see cli/CLAUDE.md #46, .claude/scan.md,
  .claude/encode.md, `.claude/simd-plan.md`, bench numbers in bench/CLAUDE.md).**
  Phase 2 (2026-07) landed: exact-short float fast path (scalar, −7.6%
  NoAlloc), scanner instruction shaves (Min/Equal unsigned-compare trick,
  scalar-register mask OR at 512-bit, −2.9%), inline vector string/key
  classify in generated code (−22% NoAlloc, −8.8% Mega, short-key scalar
  window killed the old Tiny +7% floor), gated encode escape tiers
  (macro-flat, 3.6–10× micro on ≥64 B strings). Headline scalar→avx512:
  NoAlloc −42%, Small −86%, Mega −8.4%, Tiny flat. Remaining candidates:
  1. **`skipString`/`SkipValue` tier** — SHIPPED 2026-07 (with vector
     whitespace-run skip + inlineSkipWS handoff; SkipHeavy compact −21.6% /
     pretty −29.9%, Mega −8.3%, pretty full-decode −3.3%).
  2. **validation charset rules** (`IsAlphanum` etc.) — mechanism proven
     (17× at 8 KB, break-even ~10-12 B) but no in-repo beneficiary; scalar
     gates mandatory if revived.
  3. **integer SWAR digit parse** — REJECTED 2026-07 after full in-situ
     implementation + A/B (see Tried Rejected). The −33% microbench never
     survives the inline hot-loop context.
  4. **stream path** — SHIPPED 2026-07: per-window fused locate
     (`structuralIndexAVX*` + per-tier `stringView*` cores in
     `simd_stream_amd64.go`); Mega_Reader −5.2%, Small_Reader −20/−26%.
     Stream SKIP tier also shipped (`simd_skip_stream_amd64.go`) together
     with skip-tree compacting refills (grow-only ReadMore(0) doubled the
     buffer at every mid-number window edge): SkipHeavy ggen_stream compact
     −32.6% / pretty −25.6%, B/op 8.4 MB → 127 KB; flushed out + fixed the
     scalar stream skipObject comma-WS bug (pretty objects with 2+ keys
     failed ErrExpectString — stream skip had never seen indented input).
     Remaining stream cost is copy mallocs + ReadMore, and stream Int64
     (13.7% flat of Mega_Reader — scalar refill bookkeeping, not
     SIMD-addressable).
  AVX512-vs-AVX2 ANSWERED 2026-07: avx512 geomean −5% (Small −23%,
  NoAlloc −4%, Tiny −1%) — the default recommendation; avx2 wins skip-heavy
  pretty payloads by ~6% (short-span work, double-pumped 512-bit µop tax).
  GFNI classify REJECTED: the structural/WS byte classes are not GF(2)-affine
  subspaces (kernel closure over {ctrl,'"','\\'} pulls in 0x20 — a linear
  map cannot separate them), and on the one expressible shape (ctrl detect =
  top-3-bit select + Equal-zero) vgf2p8affineqb measured ~3% SLOWER than
  Min/Equal on Zen5. Don't retry without a class that IS an affine subspace.

- **Portable width-agnostic `simd` package (go1.27, proposal #78902) — re-audit
  when mask extraction lands.** Audited 2026-07 against go1.27rc2: replaces
  NOTHING yet. The mechanism IS the wanted "one kernel, auto width" shape —
  width-agnostic types (`Uint8s`/`Mask8s`), one width per execution auto-picked
  from hardware (AVX/AVX2/AVX512/NEON, ≥128-bit), pure-Go emulation floor,
  `GODEBUG=simd=N` pinning, early compiler rewrite/multiversioning to preserve
  inlining — but the v0 op set can't express a single ggen kernel. Fatal gaps:
  `Mask8s` = {And, Or, String, ToArch, ToInt8s} — NO movemask→scalar, no
  FirstTrue/AnyTrue/CountTrue, so the classify→`ToBits()`→`TrailingZeros`
  spine (~35 sites across all 6 simd files + the generate.go:2609 inline-
  classify template) is inexpressible (portable `ToBits` is a sign-reinterpret
  Int8s→Uint8s, NOT movemask); no PermuteOrZero/ConcatShiftBytesRight/IsZero/
  reductions, so `validUTF8x16` is out too. The classify HALF is fully covered
  (Broadcast/Load/LoadPart/Equal/Less/Min/Or/Xor/SubSaturated). The `ToArch()
  any` escape hatch = boxing + type-assert + build tags — arch-specific again,
  pointless. Why: v0 ops = intersection(wasm SIMD128 API, amd64 archsimd),
  scalable-across-widths only; movemask/shuffle are absent-not-rejected and
  expansion is promised — wasm itself has i8x16.bitmask/any_true/swizzle, so
  the intersection won't block them forever. RE-AUDIT TRIGGER: mask extraction
  (movemask / index-of-first-true) + byte shuffle landing in portable `simd`.
  Even then, weigh: one width-agnostic body erases the measured per-width
  instruction tuning (Min/Equal ctrl trick below 512 vs native unsigned Less +
  scalar-register mask OR at 512 — the −2.9% shave lives in exactly that
  divergence); the emulation floor sits far below the tuned scalar tier
  (IndexByte+SWAR), so the ladder stays vector-OR-scalar-tier; the real win is
  deleting the `-simd` tier plumbing (scanStringFn / tierStreamStringCalls /
  simdSuffix, 3× kernel copies, wrong-CPU faults) while GODEBUG still pins
  width for A/Bs. House-rule interleaved A/B applies.
  SEPARATE 1.27 NOTE (bites at any toolchain bump, portable pkg or not):
  1.27 REVISES archsimd's amd64 API — `Load*Slice` → `Load*`, `Load*SlicePart`
  → `Load*Part` (Load/LoadArray/LoadPart triple); doc scan also flagged
  possible PermuteOrZero receiver-set changes + a ConcatShiftBytesRight rename
  (page too big for a reliable remote scan — verify at bump time). All six
  simd files + the generate.go emitter strings break mechanically; go.work
  pins 1.26 so nothing is urgent. archsimd also GAINED arm64 NEON + wasm
  128-bit tiers: an arm64 tier is writable by porting the x16 kernels near-1:1
  (NEON TBL/EXT cover validUTF8x16's PSHUFB/VPALIGNR needs if archsimd exposes
  them) — archsimd, not the portable package, is the cross-arch path today.
