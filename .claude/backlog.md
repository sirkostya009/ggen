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

- **Generated stream null-literal peeks still refill grow-only.** The
  `'n'` arms of generated stream container loops read the rest of `null`
  byte by byte and refill with `ReadMore(0)`. Readers fill the window, so a
  `null` cut by a full window DOUBLES the buffer. They are the only live
  grow-only sites among the generated `ReadMore(0)` refills: an audit of all
  2157 in bench + integrationtests found 612 null loops, and the rest follow
  a `SkipSpace` (which refills itself, so they fire only on an empty window at
  EOF) or are dead. Instrumented runs over the `*_Reader` and SkipHeavy rows
  saw every generated grow event come from a null loop (`mega_ggen.go`'s
  `'n'` arms). Compacting needs `ReadMore(s.Pos)` plus a rebase of the
  loop's `s.Pos`-relative offsets. B/op win on null-heavy streams through
  small buffers, wall clock likely flat. House-rule bench before landing.

Round-10 audit candidates (2026-09). Prepared patches live outside the tree in
`~/audit-round10/work/perf_{scan,stream,gen}.patch`, all UNMEASURED (the box
was on powersave during the audit); house-rule A/B before any of them moves:

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
- `AppendAny` omitempty is write-then-unwrite (jsonv2's slow path only).
  jsonv2 also has a fast pre-check (`len==0` / `IsNil` for string/map/array/
  slice/pointer/interface kinds, gated on the type having no custom
  marshaler); adding it would save boxing + emitting the empty value + the
  key on the reflect path, at the cost of a per-field "plain type" flag.
  A patch (`~/audit-2026-11/perf/perf-encode-3.patch`) read omitempty/ggen
  −14…15% in two passes (39614 → 33687, 39662 → 34167 ns/op, B/op and
  allocs unchanged), but the jsonv2 control moved ±6.4%, so neither pass is
  valid. Re-run the A/B before landing it.
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

- **Larger prealloc span budget for pointer-free elements (UNMEASURED).** The
  512 B tier of the width ladder (cmd/ggen opt #2) is
  `gc.MinSizeForMallocHeader`, and it only binds POINTERFUL objects:
  `roundupsize` / `mallocgcSmallScanHeader` add the 8-byte type header
  before the size-class lookup only when `!noscan`. Verified on go1.27 with
  a 64 B element at cap 9 (576 B): pointerful allocates 640, pointer-free
  576. A noscan backing has no heap bitmap and no header at any size, so
  for it 512 is a conservatism choice, not a runtime boundary. The next real
  one is `maxSmallSize` (32 KB). Fitting caps to size classes instead buys
  nothing: 80 and 512 are themselves classes, so `floor(budget/S)*S`
  rounded up to its class never has room for another element (checked for
  every S in 1..1024). Shape if pursued: the generator decides
  pointerfulness from go/types (arch-independent) and emits a wider budget
  for noscan elements only, while pointerful ones keep `spanBudgetExpr`.
  Only reaches element types wider than 40 B made entirely of numbers and
  bools, because narrower ones take the 80 B tier and most decoded structs
  hold a string. Cost is up-front memory on short payloads (the DeepNested
  +2× B/op trade). No target number yet. Wait for a workload with wide
  all-scalar slice elements, then run a house-rule A/B with B/op.

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
  cmd/ggen/CLAUDE.md opt #50). Two DECIDED exceptions (2026-07): skipped spans
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
  (2026-07, see bench/CLAUDE.md): cumulative pre-UTF8 →
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
  see cmd/ggen/CLAUDE.md opt #50): permissive structs emit pre-validation code
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

- **`AnySize`'s reflect walk re-checks and re-boxes every named element
  (UNMEASURED).** `reflectValueSize` tests `rv.Type().PkgPath()` per
  element although the element type is loop-invariant (`AppendAny`'s twin
  classifies it once per container through `elemInfo`), and sends a named or
  struct element back through `anySize(rv.Interface())`, which copies it to
  the heap: one allocation per element of a `[]Generated` or `[]NamedInt`.
  Mirroring `elemInfo` on the size side is the fix shape. A larger reshape of
  both walkers is in Tried Rejected ("Addressable-element reshape of the
  reflect walkers"). Bench: `BenchmarkAnyElems`-style rows from that patch.

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

- **integrationtests/gen: the converter divergence keys on `OneOf` (2026-10).**
  The "a converter's own checks run only in Go" predicate matches only when
  `len(c.t.OneOf) > 0`, so a converter-only field (no native variant: `t` is
  `converted[0]` and `OneOf` is empty) is not covered and would fail
  TestDifferential. `c.t.Converted` is the accurate marker. Such fields stay
  out of the fixture and are pinned in gen_test.go instead.

- **integrationtests/gen compares little below the first container level
  (2026-10).** The only rule below the first container level is
  `Containers.Nested` (`inner:(inner:lte=1)` on a `[][]float64`). No subject
  has a step below a map value, a step on a pointer element or the
  `novalidate` annotation, and the only catch-all, `Containers.Extra`, is a
  `map[string]string`, so those decoder paths and the routed catch-all values
  are never compared with gen. The unknown-key probe `zzz` gets no type even
  when the subject has `Shape.Rest`, so no divergence predicate can match a
  catch-all value. The enum closed-set divergence and the TS lane's
  `outsideEnum` look only at the top-level type, so a `[]Role` or
  `map[string]Level` field reports a documented divergence as unexplained. Add
  a trimmed set of nested and catch-all subjects, type `zzz` by `Shape.Rest`,
  and walk `Elem` / `Key` in both enum checks.

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
  `AppendUnixSeconds`, `AppendDurationSeconds`, `AppendNumber`,
  `AppendRawHTML`, `AppendNetipAddr*`, `AppendURL*`, `AppendRFC3339`), the
  `-simd` inline-classify tables and prealloc counters (`Vec*`,
  `ElemCountAVX*`),
  and glue (`NotEOF`, `Detach`, `CheckUTF8`, `SignedNeg`, `Uint64Limit`,
  `NewParseErr*` (`NewParseErrOpaque` included), `UnixSeconds`,
  `DurationSeconds`, `Errors.Collect`, `BoolEnd` / `NullEnd` — the bool and
  null error-branch give-up positions, `AnySize`/`AnySizeHTML` — the `any` JSONSize budget,
  `AnyIsEmpty` / `RawIsEmpty` — the
  `omitempty` predicates for `any` and raw JSON, `Float32`, `ParseRFC3339`; the `Any*`
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
  the three surface docs (README / SKILL.md / cmd/ggen/CLAUDE.md).

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

- **Revisit `ggen.CustomError` shape.** Today `{Pos int, Keys []string, Name
  string, Value any, Cause error}` + `Unwrap()`. Rough edges: `Name` doubles as
  rule identifier and user-facing label (split into `Rule` + `Name`); `Cause`
  is a bare `error` (a typed sub-interface could improve `errors.As`). Pick when a concrete
  report-shape ask exists.

- **`validation.*` error position follow-up.** Errors carry `Pos int` (full-payload
  byte offset). Open if ever wanted: a `Snippet []byte` around the failure offset
  (rejected for now — the caller has the input + `Pos`).

- **`pipe:` tag follow-ups.** Foreign-package converter inputs
  (`classifyConverter` refuses a W declared in another package, an `=` alias
  of one included. `resolvePipeCustoms` already spells W through
  `pkgQualifier`, and the `var convN W` temp would then need W's package in
  the field's `TypeImports`); `@pkg.Func` references (`customfunc.go`
  `lookupFunc` → `PkgName`) still spell the DECLARED package name and import
  by path, while the type-side qualifier table renames a package to `leaf2`
  when two packages of the pass share a declared name — a converter/validator
  from such a package would mismatch its import line (route `PkgName` through
  `structSet.qualifierFor` if anyone hits it); converter pointer inputs deeper
  than one level (`func(**int) T`) take the multi-level pointer field path but
  are untested (only single-level pointer inputs are pinned); top-level
  alias-type `pipe:` support (needs a non-dispatch null branch in the alias
  renderers); CONTAINER converter inputs (`@Conv` with W = []T/map) —
  currently rejected at parse (2026-08, was silently-broken codegen before),
  to support: populate converterInputField's ElemType/ElemKind/ElemIface from
  go/types — the dedicated-kind element delegation (R3/R4, cbf0949/816fd3c)
  makes the emit side workable now. Cosmetic: `checkVariantShapes`'
  diagnostics come out double-qualified ("Doc.M: Doc.M: decode variants … both
  claim the same JSON shape") — the message already carries Struct.Field and
  `resolvePipeCustoms`' caller prefixes it again via `qualifyRichErrors`.

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

- **Generated `omitzero` ignores an `IsZero() bool` method (2026-09).** The
  guard for a user struct is the structural `zeroCompare`, where jsonv2,
  v1 1.24+ and `AppendAny` all call an `IsZero() bool` method when the type
  has one — `KindTime` is the only kind ggen honours it for. An IsZero arm
  when go/types says the type has the method is easy. Decide: fix the
  emitter, or pin the divergence in the three surface docs. Related decision
  already taken: `omitempty` on `url.URL` tests `!= (url.URL{})` — a non-zero
  URL that still stringifies to `""` (e.g. only `OmitHost` set) is emitted as
  `""`; exact parity would need `String()` (allocates) or a per-component
  test, not worth it. The same rule bites `big.Int` / `big.Float` /
  `big.Rat` (2026-11): omitzero tests `Sign() != 0`, so a numerically zero
  value that is not the zero value (a computed 0, a precision-set
  `big.Float`, `-0`) is omitted where jsonv2's struct-zero test writes it.
  Decide alongside the IsZero arm.

- **`case:ignore` could be implemented rather than refused.** A
  `strings.EqualFold` fallback arm after the exact-match switch (jsonv2
  semantics: exact match first, then case-insensitive). Refused for now
  under the no-silent-no-op rule; no consumer has asked.

- **Tag-grammar leftovers (2026-09).** `boundFits` (and the gt/gte/lt/lte
  gate) rejects an integer-valued float spelling like `gt=1.0` on an int
  field as out of range although Go accepts the constant (`oneof=1|1.0` is
  caught as a duplicate first) — over-strict, harmless. `[0]byte` is unsupported: `foldByteArray` folds it onto KindBytes
  with `ArrayLen 0`, indistinguishable from `[]byte`, so the nil-check emit
  does not compile; jsonv2 base64s it as `""` — either skip the fold for a
  zero-length array (then it is a `[]` tuple, diverging from v2) or teach the
  byte-array emitters N==0; degenerate shape. `[N][M]byte` elements resolve
  as KindArray and take the numeric-tuple route rather than base64, unlike a
  `[M]byte` field — worth a look if anyone uses that shape. Map values and
  slice elements of `[N]byte` do the same (2026-10): `map[string][2]byte`
  writes `{"a":[255,1]}` where jsonv2 and a `[2]byte` field write `"/wE="`.
  ggen round-trips its own output, but the `inner:` levels below a map
  (cmd/ggen opt #91) then apply to the single bytes, and inside a
  `json:",embed"` catch-all container the shape does not round-trip (the
  catch-all residuals item below). `json:"-,omitempty"` (2026-11) is
  rejected at generate time "like jsonv2", but stable jsonv2 and
  `encoding/json` accept it as the name `-`, and the spelling the
  diagnostic suggests, `json:"'-'"`, is the one stable jsonv2 rejects
  (quoted names, see the Go 1.27 entry). Decide: accept it as `-`, or fix
  the diagnostic.

- **Output-name residuals (2026-09).** `emitScope` maps every non-alnum rune
  to `_` via `sanitizeIdent`, so `a-b.go` and `a_b.go` in one package still
  share the oneof scope `a_b` (loud at compile time as a redeclaration);
  making the mapping injective would rename every `ggenCap`/`ggenOneof` in
  every generated file for a layout nobody has. The cap-constant scope
  `Struct_Field` has the same hole within one package (2026-11):
  `User.Roles_List` and `User_Roles.List` both declare
  `ggenCap_User_Roles_List_string`, a redeclaration across the package's
  generated files. Same rename, same call. Single-file mode layout hole,
  now explicit in `passTypes`: a dependency declared in file B and reached
  only from file A's root is generated by nobody (A filters to its own file,
  B has no root reaching it) and stays on the json fallback; package mode
  generates it — `passTypes` is the seam if package-wide single-file
  generation is ever wanted. AST-only (degraded, no go/types) mode still
  spells foreign types by the source's local import name and cannot alias a
  colliding `json`/`time` user package — cross-package types route to
  `encoding/json` there anyway. The external-test bucket + `-pkg X` →
  `package X_test`, and the hook half of `structSet.inspect`'s stale-method
  masking (`MarshalJSON`/`UnmarshalJSON` from a previous run's generated file),
  are unpinned by tests.

- **`AppendAny` accepts text-marshaler map keys of any kind; the generator
  does not (2026-09).** `type K int` with `MarshalText` marshals through
  `AppendAny` (jsonv2 + v1 both do); the decided rejection
  (`TestAppendAny_NonStringMapKey`) now covers only key types WITHOUT a text
  marshaler. The generator still rejects any named key type at parse
  (`map key must be string`), so the accepted shape is reachable only
  through `AppendAny`.

- **`JSONSize` undercounts strings holding control bytes.** `sizeStrMult`
  is 2×, which covers `\n`/`\"`/`\\`, but a control byte other than
  `\b\f\n\r\t` encodes as `\u00XX`, 6×. Raw control bytes are rejected on
  decode, yet a `\u0001` escape decodes to one, as does any Go value built in
  code, so such a value breaks the no-grow contract (one buffer growth, output
  correct). `AnySize`'s 2× has the same hole. A flat 6× triples the
  reservation for every string. Alternatives: count control bytes at runtime
  in the size loop (a scan per string), or accept the miss. Decide with a
  house-rule A/B on Mega_Marshal B/op before changing.

- **A non-generated foreign struct still budgets a flat 128 in `JSONSize`**
  (`sizeContribKind`, and `len×128` for its slice elements), so a large one
  breaks the no-grow contract the `any` sizing (cmd/ggen opt #88) now keeps.
  `AnySize` is not a drop-in: those fields marshal through `encoding/json`,
  whose default HTML escaping expands `<>&` 6× where `AnySize`'s non-HTML
  factor is 2×. Needs a `json.Marshal`-shaped sizer, or `AnySizeHTML` as a
  conservative stand-in. A `json:",embed"` catch-all value of such a struct
  (the `embedViaFields` route) shares the budget: `thirdparty.Endpoint` with
  a long IPv6 zone writes 156 bytes.

- **`Float64` surfaces `*strconv.NumError` (ErrRange) for an out-of-range
  float64 (`1e400`) while `Float32` maps the same condition to
  `ggen.ErrNumberOverflow`** (the identity `TestNarrowFloatOverflow` pins).
  Unifying `Float64` on `ErrNumberOverflow` is a small breaking change worth
  doing if error identity is ever tidied.

- **archsimd masked byte loads — re-audit trigger.** If archsimd ever exposes
  a fault-suppressing k-masked byte load from memory (VMOVDQU8 with a
  k-mask, which AVX-512BW guarantees suppresses faults on masked-off lanes),
  the stack-copy arm in `StringAVX512`'s tail can go. On go1.27 the only
  masked memory op for 8-bit lanes is `StoreArrayMasked`; `Uint8x64.Masked`
  is documented "Emulated" (a vector-domain AND after an unmasked load),
  which is exactly why `LoadUint8x64Part` over-reads.
  `LoadUint8x16Part`/`LoadInt8x32Part` stay safe (scalar 8/4/2/1-byte
  element loads).

- **gc internal compiler error on a dead store into a zero-length array
  (Go 1.27.1, worth filing upstream).** Assigning into an element of a
  `[0][3]int` — the shape a `map[string][0][3]int` decode loop produced —
  makes the compiler abort with "can SSA LHS mv[idx0] but not RHS" instead of
  dead-storing it. Nothing in ggen is blocked (the `[0]T` readers have no
  element loop at all, cmd/ggen opt #85), but the toolchain bug stands.
  Minimal repro (two shapes, array-variable and call-result RHS) with a drafted upstream report at `~/audit-round10/ice/REPORT.md`.

Found during the 2026-10 review and its fixes, all unfixed:

- **The import scan counts a selector as a qualifier (theoretical).** An
  identifier followed by `.` in a chain (`.Field.`) is collected by
  `bodyQualifiers`, so a foreign package named like a field could be imported
  for nothing.

- **`json:",embed"` catch-all residuals.** (a) Values `embedValueRoute` leaves
  on `embedViaAny`, such as numbers, bools, `time.Time`, netip, `big.*`,
  `url.URL`, `sql.Null*` and named primitives, decode through `SkipValue` +
  `encoding/json` v1, which reads `null` into the zero value. Numbers, bools
  and named primitives encode inline, the rest through `AppendAny`, which
  writes `url.URL` and `sql.Null*` as struct objects. gen's `Shape.Rest` lowers every catch-all value in the field form,
  so the exported schemas reject the `null` Go accepts and the objects Go
  writes, and accept the URL text Go rejects. `JSONSize` budgets the field
  form too, so it undersizes `url.URL` / `sql.Null*` values and containers of
  them (`url.URL` 94 reserved vs 188 written, `sql.NullString` 25 vs 44,
  `sql.NullTime` 56 vs 68, `[]url.URL` 98 vs 190), one buffer growth per
  Marshal. Either decode and encode these values with the field emitters
  (`url.URL` / `sql.Null*` through `embedViaFields` is a wire change), or make
  `Shape.Rest` mirror the route (nullable for every non-pointer value
  `encoding/json` reads, `url.URL` / `sql.Null*` as their v1 objects) and
  budget `AppendAny`-written values with `AnySize`. (b) A `[N]byte` nested in
  a catch-all container (`[][16]byte`, a `map[string][4]byte` value,
  `[]*[4]byte`) does not round-trip: `AppendAny` writes base64, while
  `encoding/json`, which reads it, and the container emitters both expect a
  number array. Found in the 2026-11 audit, same routes:
  (c) a string, `any` or number below a container
  (`map[string]map[string]string`, `map[string][]any`,
  `map[string][2]string`) also takes `embedViaAny`, so v1 turns a lone
  surrogate escape into U+FFFD with a nil error (the top-level
  `map[string]any` catch-all rejects it, as README promises), ignores
  `usenumber`, and zero-pads a short `[N]T`. Raw invalid UTF-8 is rejected
  first, by `ggen.CheckUTF8` over the span. Routing these innermost kinds
  through `embedViaFieldDecode` would fix all three.
  (d) `embedValueRoute` has no `KindSQLNull` arm, so a generic
  `sql.Null[T]` value (`map[string]sql.Null[time.Duration]`) is written by
  `AppendAny` as `{"V":"1s","Valid":true}`, which v1 rejects on the way
  back: the struct cannot decode its own output.
  (e) a nil `[]byte` / `[]Leaf` / map value is written as `""` / `[]` / `{}`
  through `AppendAny`, where a map field of the same value type writes
  `null`, so nil-ness does not round-trip.
  (f) neither the generated splice nor `AppendAny` checks catch-all keys
  against the declared JSON names, so a colliding key is written as a
  duplicate member with a nil error. Decide whether to skip it, error, or
  document it.

- **`encoding/json`'s fallback reads a named `[]byte` without a codec in two
  forms.** A `net.IPMask` in a plain struct or a struct alias also accepts a
  JSON number array (`[1,2]`), while gen lowers it as a base64 string, so the
  schemas reject the array. Found with `type Subnet net.IPNet` as a
  TestDifferential subject, which was dropped rather than pinned as a
  divergence.

- **A defined named container without a codec goes through `encoding/json` in
  Go, but gen lowers its underlying shape.** `type Durs []time.Duration` or
  `type Peers []net.TCPAddr` (also `url.URL`, `sql.Null*` or `[N]byte`
  elements) is `KindStruct` in the model, so cmd/ggen reads and writes it with
  `encoding/json`: durations as int64 nanoseconds, a `TCPAddr` as an
  `{"IP","Port","Zone"}` object. gen lowers it through its underlying type
  (duration strings, `ip:port` strings), so the schemas and Go disagree both
  ways. An `=` alias (`type DursAlias = []time.Duration`) takes the native
  code and agrees. Decode and encode unannotated named containers natively, as
  `inlineNamedPrim` does for named primitives, or lower them in gen as
  `External` values with `encoding/json` semantics. Two more costs of the
  same route (2026-11): `JSONSize` budgets such a container a flat 128
  whatever its length (`type IDs []int64`, `type Attrs map[string]string`),
  so a long one grows the buffer. And an unannotated same-package defined
  type over a STRUCT (`type Line Item`, used as a field, pointer, element or
  map value) is not generated either, so the wire bypasses every `pipe:`
  rule on `Item` and ggen's unknown-key, duplicate-key and UTF-8 checks,
  with no diagnostic. Generating it through the struct-alias introspection
  path, or at least warning when a same-package type falls to the
  `encoding/json` rung, is the choice.

- **Struct aliases and embedded fields.** `//ggen:generate type OA Outer` over
  `type Outer struct{ Inner; B int }` writes `{"B":2,"Inner":{"A":1}}` where
  `encoding/json` writes `{"A":1,"B":2}`: `extractAliasFromTypes` treats the
  embedded field as named and does not promote it. An embedded `=` alias
  field (`type AA = Inner; struct{ AA; B int }`) is rejected ("embedded type
  AA in Emb is not supported: only a type declared as a struct literal in
  this package can be embedded") where `encoding/json` promotes `Inner`'s
  fields.

- **Pointer map values are judged inconsistently at generate time.**
  `map[string]*int` with `inner:gte=0` is rejected ("`gte` is inapplicable
  to *int") while a `*int` field, `[]*int` elements and
  `map[string]map[string]*int` with `inner:(inner:gte=0)` accept the rule and
  apply it to the pointee. `map[string]*NamedInt` passes because `eff`
  strips stars only for the named-primitive lookup. `map[string]*[]int` and
  `[]**[]int` with two `inner:` levels fail "no element at that depth":
  `peelTypeOnce` does not strip a leading pointer before peeling, while
  `diveElemType` does. Both are loud, not silent, and aligning them touches
  the gen export too.

- **`omitempty` is a silent no-op on a named type with marshal methods, and
  in AST-only mode.** `namedEmptyCond` gives no guard to a type with
  `MarshalJSON`, `MarshalText`, `AppendText` or `AppendJSON`, and none when
  the field has no go/types type. So a named container with its own codec,
  and every named container, interface and foreign type in AST-only mode,
  is written even when it encodes as `null`, `""`, `[]` or `{}`. Reject it,
  or test the written bytes. `opaqueField` rejects `omitempty` on a type
  generated code cannot name.

- **A pointer pipe on `null` skips built-ins after a nil-filling `@Func`
  mod (contract question).** On a JSON `null` only the `@Func` half of a
  pointer pipe runs, so `pipe:"@DefaultZero gte=1"` on a `*int` (or the same
  steps under `inner:` on a `[]*int`) accepts `null` as 0 while rejecting an
  explicit 0. Deliberate today (the built-ins have no leaf on a `null`), but
  arguably not declared order.

- **gen describes annotations, not CLI flags.** `gen.Load` never applies
  `model.Flags`, so a decoder generated with `-nullzero`, `-ignoreunknown`
  or `-novalidate` is described as if the flag were off (documented in
  gen/SKILL.md). If a consumer needs it, `Load` could take the flags and
  apply them.

- **gen output `Optional` over-approximates `omitempty` (harmless).** Out
  mode sets `Field.Optional` for `omitempty` on kinds with no empty test
  (numbers, bool, time, `[N]T`, a pointer to such a leaf past its nil
  check), which the encoder never omits: a superset, inexact against
  "encoder may omit".

- **`nullzero` in the schemas skips the transforms Go runs on the zero
  value.** A `nullzero` field (the variant or the struct annotation) is
  `Nullable` when `zeroPasses`, and zod / valibot render `Nullable` after the
  rules, so a `null` stays `null` in the parsed value. Go turns it into the
  zero value and then runs the field's mods: `clamp=1|5` yields 1, `trim` /
  `toupper` yield `""`. Render the null branch as null → zero piped into the
  field's own schema (zod `z.null().transform(() => zero).pipe(inner)`, or the
  valibot equivalent), and pin it in the differential fixture.

- **gen's `delegatingAlias` has two minor gaps.** A delegating alias over a
  codec target ggen does not generate (`Token` over `Secret` in
  `gen/testdata/api`, or a foreign uuid-like type) leaves `Type.File` empty:
  the delegating path has no `types.Object` for the target. On the foreign
  path a package seen first through an alias is created with `Name` =
  `AliasUnderlyingImport.Name`, the generated code's qualifier, which can be a
  rename such as `leaf2` rather than the declared name.

- **Kotlin: an optional output field with a zero-literal pointee
  (unverified).** `*string` / `*int` under `omitzero` render with a default
  (`String = ""`), so the `""` Go writes for a non-nil pointer to `""` is
  dropped on re-encode and Go reads back nil. Needs a kotlinc run
  (gen/test-toolchains.sh).

- **A very wide struct is expensive to compile.** Every field of a struct
  decodes inline in one function, so a struct of about 100 fields covering
  every container shape generated a 50k-line `DecodeFromStream` and a 43k-line
  `DecodeFrom`, and gc needed more than 34 GB to compile it (observed in the
  2026-10 review). Splitting the per-field decode of very wide structs into
  helper functions would bound compile memory. Measure the runtime cost first
  (house-rule A/B).

Found during the 2026-11 audit, unfixed (reproducers in
`~/audit-2026-11/repro/`):

- **A cyclic Go value through generated marshal code is a fatal stack
  overflow.** Generated `AppendJSON` / `JSONSize` of a self-referential
  struct recurse with no depth cap, and `AppendAny` / `AnySize` restart
  their depth at 0 at every generated `Marshaler`. A ring (`n.Next = n`) or a
  parent link stored in an `any` field kills the process instead of
  returning the `ErrMaxDepth` README and SKILL.md promise. Fix shape:
  depth-threaded generated cores for cyclic types (`appendJSONDepth` /
  `jsonSizeDepth`) plus an entry point `AppendAny` can hand its depth to,
  or a visited-pointer guard. Decide the generated method surface, or
  document that the cap stops at a `Marshaler`.

- **Self-referential decoders use stack per level in proportion to
  `sizeof(T)`.** By-value receivers, results and child temps cost several
  × `sizeof(T)` per level, so for a T above ~12 KB (inline arrays) a ~90 KB
  document below the 10000 cap overflows the stack, fatally. Decide: a
  size-aware cap at generate time (`min(10000, budget/(k*sizeof(T)))` via
  go/types sizes), or decode the child in place through a pointer.

- **`AnyIsEmpty` judges by kind and length.** It ignores marshal methods and
  `json.Number`, so an `any,omitempty` field drops `json.Number("")`
  (`AppendAny` writes `0`), an empty named set whose `MarshalJSON` writes
  `{"count":0}` and a named string with `MarshalText`, although README says a
  type with its own JSON or text methods is always written. A named interface field (`net.Addr`) under omitempty uses
  it too: a zero `*net.TCPAddr` counts as empty because `AppendAny` writes
  `""`, while the field itself is written by `encoding/json` as an object.
  Decide: test the written bytes (one encode per omitempty check) or keep
  the kind test and document it.

- **`Marshal`, `MarshalString` and `WriteTo` panic on a nil `*T` or a nil
  `Marshaler`**, where `AppendSlice`, `AppendAny` and `encoding/json` write
  `null`. Decide: write `null` (one nilable check per call) or return an
  error.

- **`AppendFloat(dst, v, 32)` writes `+Inf` / `-Inf` with a nil error for a
  finite float64 beyond the float32 range.** Generated code passes float32
  values only, so this is the exported ABI's contract. Decide: an error, or
  a documented precondition.

- **An escaped string pins its raw-span-sized scratch.** Bytes `stringSlow`
  returns the scratch it sized from the raw span as the string, so an
  escaped string holds 2-6.5× its decoded length. The exact-size result
  regressed short escaped strings (Tried Rejected, "Exact-size result for
  escaped strings"). Decide whether memory or short-string speed wins, e.g.
  trim only when the slack is large.

- **`CloseJSONString{,HTML}` heap-copy a dirty body over 32 bytes**
  (`string(dst[from:])`) before re-escaping it, one 48-64 B allocation per
  dirty value. A patch that drops the copy (`~/audit-2026-11/perf/rt-strings-4.patch`)
  goes to 0 allocs, but both A/B passes had invalid controls: URL rows read
  −24…−40% and the escape-dense `text_backslashes` row +15% / +25%. Needs a
  valid A/B, and probably a density gate.

- **Error messages render values with bare `%v`.** Validation and mod
  errors print `Value` / `Limit` that way: floats in exponent form, payload
  control bytes raw, pointers as addresses. Unknown-key names also reach
  `Error()` raw. Decide one rendering (quote strings, `'f'` floats,
  dereference pointers).

- **`PredicateError` / `CustomError` keep the field itself as `Value`**, so a
  kept error changes after the next `Seq` / `Array` step or a decode into the
  same receiver. Decide: copy on the error path (deep for slices and maps),
  or document `Value` as a view.

- **multierr collects without a cap.** One heap-allocated error per failing
  element, unknown key or duplicate key: ~89× (bytes) / ~106× (stream) the
  input in allocation, and an `Error()` string 15× the input. Decide a cap
  and what `Error()` reports past it.

- **`big.Int` / `big.Float` / `big.Rat` decode is unbounded.** The digits and
  exponent go straight to math/big: `big.Rat` `"1e1000000"` ×50 (612 B)
  allocates 108 MB, and `big.Int` digit counts cost quadratic time. jsonv2
  behaves the same (inherited from math/big). Decide digit and exponent
  limits (a new error), or document it.

- **`len` / `minlen` prealloc is unbounded by the input.** The emitted
  `make(T, 0, N)` runs before any element is read: `minlen=1e7` makes a 9 B
  payload allocate 80 MB, and multierr pays it per short row. `maxPrealloc`
  bounds the count, not count × `sizeof(elem)`, so on GOARCH=386
  `minlen=MaxInt32` panics in `makeslice`. Decide: bound the hint by the
  input length or bytes, or make `len` / `minlen` validation-only.

- **Raw JSON fields and the `MarshalJSON` rung are copied without
  validation.** A `json.RawMessage` / `jsontext.Value` holding `1,"admin":true`,
  or a `MarshalJSON` returning `{"a":`, writes invalid JSON or injected
  members with a nil error. jsonv2 validates both. Decide: validate (one scan
  per value on the encode path) or document it as the caller's job.

- **Decode into a receiver keeps `json:"-"` and unexported fields**, also
  those of an unrelated earlier record in a reused `[]T` slot. That
  contradicts the fresh-decode contract of receiver reuse. Decide: zero
  them in the receiver reset, or document them as carried.

- **Generated code collides with package-scope names.** It calls the
  predeclared `min` / `max` / `new` / `clear` unqualified (a package with its
  own pre-1.21 `func max(a, b int) int` fails to build, a same-signature
  `clear` would be called silently), imports its helper packages (`math`,
  `strconv`, `strings`, `unsafe`, `encoding/json`, ...) unaliased with no
  package-scope check, and names parameters and locals (`data`, `result`,
  `key`, `s`, `n`, `i`) that shadow a same-package type or `@Func` spelled
  bare in the body. ggen exits 0 and the build fails. Decide: reject at
  generate time, or rename the generated identifiers (`ggenData`, aliased
  imports, cap constants without `min` / `max`), which removes the class.

- **Package-mode regeneration never prunes outputs it no longer writes.**
  Moving a struct out of a tagged file leaves the tagged bucket's old output
  behind, with duplicate method declarations, although SKILL.md says
  rerunning ggen is enough after adding or removing types. Now that generated files carry
  `model.GenHeader`, ggen could delete the ones it recognises in the
  directory. Decide whether deleting files is in scope.

- **Single-file mode with CLI `-multierr` treats a sibling file's multierr
  callee as fail-fast**, so the parent returns only the first error. Applying
  the CLI flags to the package fixes the reproducer but is unsound when the
  sibling files are generated by their own `//go:generate` lines with other
  flags: a fail-fast callee would be drained mid-value. Decide: package-wide
  flags in single-file mode, or accept the lost errors.

- **Parse-error labels of container elements depend on the element type and
  the error family.** Parse errors at inline scalar elements, map values and
  generated-struct elements carry the bare container name (`tags`, `byKey`,
  `items.sku`), where rule errors and delegated element kinds use `tags[]` /
  `byPtr.value`, and `T` vs `*T` map values differ. README and SKILL.md
  define `tags[]` for validation errors only and no map-value label. Decide
  the label scheme, then make every site follow it.

- **`notempty` / `len` / `minlen` / `maxlen` on `[N]T` / `[N]byte` are type
  constants**: always pass or always reject, and accepted anyway. Two
  defensible fixes: reject every length rule on `[N]T`, or compare the bound
  with N and reject only the always-failing ones. The gen fixture
  `Nulls.Pair` (`[2]int maxlen=1`) relies on the always-failing shape for
  `TestNullZero`.

- **`format:unixnano` writes `t.UnixNano()` unchecked.** The zero
  `time.Time` and instants outside ~1678..2262 write a wrapped number with a
  nil error, which ggen reads back as another date (zero time →
  1754-08-30). Refusing them breaks `Marshal` of every unset time field.
  Writing the exact wide decimal needs a decoder that reads counts past
  int64. Decide which.

- **A struct alias over a named non-struct codec type does not delegate.**
  README and cmd/ggen/CLAUDE.md promise `type LocalUUID uuid.UUID` delegates
  to the UUID's text methods, but the code (and jsonv2, where methods do not
  carry over) writes `[16]byte` as base64. Delegating changes the wire of
  every such alias (`net.IP`, `slog.Level`, `uuid.UUID`), needs the
  struct ladder's same-pass fixed point, and gen's `delegatedWire` finds an
  alias's target only by its struct. Decide: change the code, or the docs.

- **gen schemas accept unpaired surrogates.** zod / valibot string schemas
  accept `"\ud800"` in fields, elements and map keys, which the Go decoder
  rejects as invalid UTF-8. Matching needs a well-formedness check on every
  string leaf and key plus a deep walk of `any` values, and since
  `allowinvalidutf8` set by CLI flag is not in the export, the schemas of a
  flag-built decoder would then reject input it accepts. Decide.

- **gen map schemas drop prototype-named keys.** zod and valibot drop
  `__proto__` (valibot also `constructor` / `prototype`) record keys by
  design, without checking their values or key rules. Go keeps them.
  Matching means wrapping every record in a helper that validates and
  re-adds those keys. Decide: restore, reject, or document.

- **gen lowers a non-empty interface field as any JSON value.** An
  `fmt.Stringer`, `error` or `interface{ M() }` field is `WireAny` on input,
  but the decoder accepts only `null`. The model cannot say "null only".
  Decide: a new `Wire` kind (all five emitters), a marker flag, or a report
  that the field is not describable.

- **A carried `big.Float` keeps its precision and rounding mode.** Decode
  into a receiver parses into the carried value, as `UnmarshalText` does in
  `encoding/json` and jsonv2, so a `SetPrec` / `SetMode` made by user code
  survives the decode, against the fresh-decode contract of receiver reuse.
  Decide: emit `SetPrec(0).SetMode(big.ToNearestEven)` before the parse (a
  call on the success path of every `big.Float` value), or document that
  both are kept.

- **The `encoding/json` fallback rung is not UTF-8 exact.** A no-codec
  foreign struct and an `embedViaAny` catch-all value decode through v1.
  In strict mode `ggen.CheckUTF8` rejects raw invalid bytes first, but an
  unpaired `\uXXXX` surrogate escape still decodes to U+FFFD (the hole
  "Raw-span surrogate-escape validation" describes). Under
  `allowinvalidutf8` v1 rewrites raw invalid bytes to U+FFFD, where the
  native kinds pass them through. Decide: an escape-parsing validator for
  the strict half, and document or accept the permissive half.

- **`AppendAny` writes a struct tag name that is not valid UTF-8 raw.** The
  generator rejects such a `json` tag, but `collectFields` on the reflect
  path parses tag names on its own and has no error channel for them, so
  the key goes out as invalid JSON. Decide: return an error from
  `AppendAny`, or skip the field.

- **An unknown key followed by a missing colon carries a different error
  path on the two decode paths.** Both report `ErrBadObject` at the same
  position, but the bytes path leaves the path empty while the stream path
  labels it with the key (`streamUnknownKey`'s `ConsumeColon` check uses
  `ownKey`). Applies to every struct. Pick one label and make both follow.

- **One slab per `[][]number` container (measured trade).** Each counted
  row of a `[][]int` / `[][]float64` / `[][]bool` is its own `make`. A
  per-container row slab (`~/audit-2026-11/perf/perf-decode-alloc-2.patch`)
  measured Mega_Unmarshal/ggen −2.6% (14.72 → 14.33 ms), allocs −23.9%
  (45981 → 35002), but B/op +7.1% (8.75 → 9.37 MB) and GC cycles 93 → 113.
  ggen_copy is flat. Decide whether allocation count or bytes matter more.

- **Error literals emitted inline at every site (measured trade).**
  `DuplicateKeyError` / `UnknownKeyError` composite literals repeat at every
  field and decoder error site. Routing them through two exported
  constructors, `ggen.NewDuplicateKeyErr` / `ggen.NewUnknownKeyErr`
  (`~/audit-2026-11/perf/perf-generator-2.patch`), shrinks generated text by
  5% (integrationtests) to 9.3% (bench), time flat, B/op and allocs
  identical. Decide whether two more ABI exports are worth it (see "Shrink
  the exported runtime API").

# Tried Rejected

- **`checkSpan` starting `utf8.Valid` at the first high-bit word — REJECTED,
  measured (2026-11).** The `CheckUTF8` shape: skip the ASCII prefix, hand
  the tail to `utf8.Valid`. Under valid controls (+1.0%) RuneGated_Unmarshal
  +17.6% (10849 → 12753 ns) and Small_Unmarshal/ggen +12.0% (629.1 →
  704.3 ns), both reproduced from a first pass. NoAlloc flat, B/op and allocs
  identical. The tail handoff runs `hasCtrlByte` as its own pass before
  `utf8.Valid`, so an all-non-ASCII span is walked twice where the fused SWAR
  loop walked it once, and the per-word early-exit branch slows Small's long
  ASCII Bio. Don't retry unless the control check stays fused and the word
  loop gains no exit branch.

- **Reshaping `validUTF8x16` to stop spilling its LUTs — REJECTED, measured
  (2026-11).** HEAD spills the LUTs and constants (0xb0 frame) because
  `LoadUint8x16Part` is called inside the block loop, and rebuilds constants
  with broadcasts. The reshape regressed avx512 NoAlloc_Reader/ggen_stream
  +11.0% (3307 → 3672 ns, control +2.3%) and +12.4% on the rerun (3294 →
  3702, control −0.4%). NoAlloc_Unmarshal/ggen −1.5% / −3.1% sat inside the
  ~3% layout band. B/op and allocs unchanged. Don't retry without an
  objdump-level argument for why the stream row pays.

- **Splat-table constants in the avx2/avx512 runtime kernels — REJECTED,
  measured (2026-11).** Replacing the per-call `MOVL+VMOVQ+VPBROADCASTB`
  constant setup in the runtime kernels with table loads made no target row
  faster under tight controls (jsonv2 −0.03% / −0.17%): SkipHeavy pretty/ggen
  +0.84%, pretty/ggen_stream +2.23%, and the guard rows regressed,
  compact/ggen +3.4% and compact/ggen_stream +8.8%. The 3-5% instruction-count
  drop did not reach wall clock: a table load per call costs more than the
  broadcast immediates on short compact spans. The avx tier loads from tables
  because AVX1 has no `VPBROADCASTB`, and the generated inline classify loads
  the `Vec*` tables, which measured a win. Don't retry in
  the runtime kernels.

- **Compacting refill in `(*Stream).skipLiteral` — REJECTED, measured
  (2026-11).** A skipped `true`/`false`/`null` cut by a full window refills
  with `ReadMore(0)` and doubles the buffer. Compacting it keeps the
  SkipHeavy stream window at 4 KiB (B/op 258100 → 4148 compact, 127028 →
  4148 pretty, allocs 8/7 → 3), but wall clock rises on compact in both
  lanes, +3.9% scalar and +3.4% avx512 (jsonv2 controls within 1%). Pretty
  +3.0% / +0.9%, Mega_Reader about −1% (noise). The bounded window costs
  more refills (memmove + Read) than the doubling did. Don't retry unless
  residency matters more than SkipHeavy stream time, or with a gate that
  compacts only when the consumed prefix is large.

- **Per-element `JSONSize` budget for integer slices — REJECTED, measured
  (2026-11).** A flat 20 bytes per int64 element makes Mega reserve 1.96× its
  output. Budgeting each element by its digits cut Mega_Marshal/ggen B/op
  9158656 → 5857280 (−36%, GC 92 → 67), but wall clock rose +3.5% scalar /
  +3.2% avx512 against jsonv2 controls of +1.9% / +1.7%, about +1.5% net in
  both lanes. ggen_presized (no `JSONSize`) is flat. The extra pass over every
  int costs more than the smaller reservation saves. Don't retry unless
  memory matters more than marshal time.

- **Exact-size result for escaped strings in bytes `stringSlow` — REJECTED,
  measured (2026-11).** Returning a right-sized copy instead of the
  raw-span-sized scratch drops B/op at equal allocs (EscapeHeavy 19456 →
  12288, EscapeUnicodeShort 280 → 112, EscapeUnicodeLong 21504 → 8192), but
  short escaped strings, which take the 128 B stack-scratch-plus-copy path,
  slow down: EscapeUnicodeShort/ggen scalar +9.6% at 5000x (control −1.8%),
  avx512 +3.4% (copy +8.3%). EscapeUnicodeLong is 2.5-4% faster, Mega_Unmarshal/ggen
  +2.1-2.5%. The memory defect stays open (2026-11 audit block). Don't retry
  the unconditional copy.

- **Addressable-element reshape of the reflect walkers — REJECTED, measured
  (2026-11).** Reshaping `appendReflectValue` / `reflectValueSize` to stop
  boxing addressable elements won big on its targets (`[]generated` −52%,
  65 → 1 allocs, `map[string]generated` −30%, 69 → 5, `[]named_int` −83%,
  65 → 1), but guard rows slowed in both passes: `[]*generated` through the
  reshaped function +9.0% / +5.8%, `[]any` / `map[string]any` +4.7…6.3%
  with unchanged code (partly layout drift), and the reshape calls
  `netKindOf(rv.Type())` per element. `AppendAny` classifies the element type
  once per container instead (`elemInfo`). The `AnySize` half is the open perf
  candidate above. Don't retry the per-element shape.

- **Starting `stringSpanEnd` at the quote the caller already found — flat,
  measured (2026-11).** `String`/`StringAVX*`'s escape path re-scans
  `data[start:firstQuote]`, 44 B per EscapeHeavy decode and 340 B per
  EscapeSparse, roughly 10 ns. B/op and allocs identical. ns/op moved both
  ways beyond what the mechanism allows (EscapeHeavy/ggen −1.2%, then +4.6%),
  and EscapeSparse failed its control twice (a layout difference). Could land
  as a cleanup, not as a win.

- **Reading the pointer word instead of reflect in `nilItem` — flat, measured
  (2026-11).** `AppendSlice`/`MarshalSlice` test every element of a pointer
  `T` for nil through `reflect.ValueOf`/`IsNil`, twice in `MarshalSlice`. The
  direct read cut `nilItem`'s profile share 15.7% → 2.9%, but
  MarshalSlice_Marshal/ggen_ptr moved only −2.65% (10855 → 10567 ns) with
  the jsonv2 control +2.45% the other way and the untouched ggen_value guard
  +1.35%. B/op and allocs identical. Profile share is not wall clock.

- **`VZEROUPPER` before returning from the avx2/avx512 tiers — flat on
  Zen 5, measured (2026-11).** No wide tier or generated wide inline scan
  clears the upper bits before SSE code runs, which costs an AVX-SSE
  transition on Intel only. Here NoAlloc_Unmarshal/ggen +0.29% (control
  −0.46%). The Small control was off >3% twice, so Small is not judged.
  Re-measure only on Intel hardware.

- **Fusing the grow into `ReadMore`'s compaction arm — flat, measured
  (2026-11).** With a small `keep` on a full window, the compaction arm
  memmoves the window for a `keep`-byte Read and then grows anyway (Read
  destinations `[512 17 512 1024 2048]` on Small_Reader_512). Allocating the
  doubled backing when the post-compaction tail is under `cap/4` read
  Small_Reader/ggen_stream_512 −6.7% scalar (control +1.2%) and +0.7% avx512
  (control −1.4%), +3.3% in an invalid first pass. That row varies
  2409-2632 ns across passes of the same binary, and ggen_stream_full, which
  the patch cannot reach, moved −8.3%, so its noise floor is 5-9%. B/op and
  allocs identical. The gain is fewer `read()` calls and one memmove saved,
  which this bench cannot see. Retry only with a syscall-counting bench or a
  real file/socket reader.

- **Appending scalar slice elements directly instead of pre-grow + re-index —
  REJECTED, measured flat (2026-09).** `row = append(row, int(n))` in place of
  `append(row, 0)` then `row[len(row)-1] = int(n)` on the bytes path removes a
  dead zero store and one bounds check per element (the `mega_ggen.go:868`
  IsInBounds goes). Two core-pinned 500x passes: Mega_Unmarshal/ggen scalar
  −0.3% / −0.5%, ggen_copy −0.5% / −1.3% (controls 0.1% / 0.4%) — at noise,
  as predicted for ~2 instructions over ~280k elements. The simd pair was
  unreadable: the untouched jsonv2 control ran 4.1% / 4.0% faster in the
  patched binary on BOTH passes, a reproducible layout difference between the
  two builds. Don't retry for speed; if it lands it lands as a codegen
  simplification, not a perf change.

- **Sizing the stream `stringSlow` scratch from the raw span — REJECTED,
  measured (2026-09).** `make([]byte, 0, max(32, stringSpanEnd(s.buf,
  start)-start))` in place of the fixed 32 B, mirroring the bytes twin. Cuts
  allocations a third (EscapeHeavy ggen_stream 38 → 27, EscapeSparse 42 → 30)
  but is slower and allocates MORE bytes: EscapeHeavy/ggen_stream +13.1%
  scalar (control 2.4%) / +5.7% simd (control 0.3%), B/op 34224 → 37344;
  EscapeSparse +2.4% (control 0.1%), B/op 66608 → 70192; Mega_Reader flat. On a
  512 B window the closing quote of a long string is rarely buffered, so the
  hint falls back to the window remainder for every escaped string, short ones
  included — a larger zeroed `make` plus an extra `stringSpanEnd` walk over
  bytes the copy touches anyway, in exchange for skipping cheap growth
  appends. Fewer allocations is not the metric; don't retry without a hint
  that never over-sizes short strings.

- **Bounds-check-free SWAR string kernels — REJECTED, measured (2026-09).**
  Two shapes, both one core-pinned 500x pass per side with jsonv2 controls.
  (1) Head-reslice all four kernels (`p := b; for len(p) >= 8 { …; p = p[8:] }`)
  removes the 12 bounds checks but makes the POINTER loop-carried: `p[8:]`
  must not point past the allocation, so every iteration computes
  `ptr += cap > 0 ? 8 : 0` (NEG/SAR/AND/ADD) and the next load waits on it,
  where the indexed loop carries only a 1-cycle `i += 8` and its bounds
  checks are predicted and ~free. `checkSpan` on Small's 2800 B string:
  Small_Unmarshal/ggen 622.8 → 700.4 ns/op (+12.5%, reproduced at 200000x).
  The reshape also dropped `ctrlOrHigh` under the inline budget (86 → 75),
  and Mega_Reader read −4.2% that pass. (2) Keeping the indexed word loop and
  folding the tail into `hi` alone (`for _, c := range b[i:]` —
  `ctrlOrHigh` 86 → 70, inlined at all 6 sites, codegen otherwise base) did
  NOT reproduce any stream win (Mega_Reader/ggen_stream +1.2%, control 0.35%)
  and read EscapeHeavy/ggen +7.0% (control 1.9%), where the inlined call grows
  the hot bytes `stringSlow`. Don't retry: the removed branches were the cheap
  part, and inlining `ctrlOrHigh` has no measured beneficiary.

- **Folding the bool give-up walk into `ggen.Bool` — REJECTED, measured
  (2026-09, inline cost).** `Bool` costs 54; every variant carrying the
  give-up position — `litEnd` folded in (117), a call to a cold `boolEnd`
  (114), unsafe loads (112), a true-only shell + slow call (95–96), a 4-byte
  switch (108) — exceeds the 80 budget, and `Bool` IS inlined at all 12
  reported generated call sites in bench today. The position lives in the
  cold exported `BoolEnd` called on the error branch only (cmd/ggen/CLAUDE.md
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
  instead (cmd/ggen/CLAUDE.md opt #78); revisit only if a consumer has such a
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
  `len == cap` gate) were taken with in-process
  `go test -bench` and no discarded warmup. Re-measured per the house rules
  (prebuilt `go test -c` binaries, warmup discarded, one
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

- **SIMD phase 3 (phases 1+2 SHIPPED — see cmd/ggen/CLAUDE.md #46, .claude/scan.md,
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
