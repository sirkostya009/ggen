# TODO

- **Perf-review findings (2026-06-11 audit) — verified, not yet landed.** All
  prototyped + A/B'd at Mega scale, then reverted; numbers below are measured,
  not estimated. (`skipString` bounded backslash probe — the 5.1× stream fix —
  already landed.) Suggested order: stream pair → decode pair → encode.
    - **Stream `s.buf` hoist.** In-loop `ReadMore` forces slice-header
      reload per byte in `SkipSpace`/`Int64`/`Uint64`/`Float64`/`Number`
      (`scan/stream.go`). Nested-loop restructure (`buf := s.buf; for i <
      len(buf)`, refill in outer loop) registerizes header. Measured -3.1%
      stream wall (SkipSpace+Int64 only); extensions ~1-2% more. Distinct from
      rejected `inlineStreamSkipWS` (that kept Ensure in-loop; Ensure gone).
    - **Gated stream string slab.** `Stream.String`/`Number` bump-allocate
      <1024B strings from append-only chunks (`unsafe.String`), fresh chunk on
      overflow, dropped on Reset, never reused/rewritten — immune to the
      rejected-pool corruption class, not the rejected 2-walk arena. Measured:
      allocs 256,588 → 104,641 (readall parity), -3-6% wall;
      BenchmarkRetention parity (~86 KiB/item both ways). **Chunk-size gating
      mandatory**: un-gated 8KiB first chunk regresses small payloads 3.5×
      B/op, +30-90% ns (geometric 512→8KiB or payload-gated first chunk).
      Update scan/CLAUDE.md "owned copies" wording if landed.
    - **Exact-cap comma pre-count for flat numeric/bool slice elems, bytes
      path.** Elem kinds where `,`/`]` can't appear inside elem: `IndexByte(']')`
      + `bytes.Count(',')+1` before `make` (both SIMD). `generate.go`
      preallocCap + emitByteSliceRead non-empty arm; every depth via
      peelSliceField; hintlen/len/minlen keep precedence. Measured (Matrix
      only): allocs -36.6% (101,927→64,599), ns -7.5%, B/op -21%, residency
      improves (exact caps — opposite of rejected maxlen-as-prealloc). Bytes
      path ONLY (stream has no full buffer). Verify small_test no-regress.
    - **Hoist nested-container slot into depth-suffixed local.** Recursion
      threads `result.Matrix[len(result.Matrix)-1]` as inner dst → ~30 instr
      header churn + write barrier per inner elem (barriers ≈10% of decode
      samples). Emit `var rowN <inner>` / `rowN := target`, recurse on it,
      `target = rowN` after inner `]` before elem validation
      (`generate.go` bytes + stream emitters). Measured -3.4% serial Mega;
      micro -12% matrix-only, asm-verified (4→2 barrier sites). Composes with
      comma pre-count (both want row local).
    - **Map decode buffer-then-build (fresh-decode arm, primitive/string
      values).** 73% of mapassign time = Swiss-map growth from unsized `make`.
      Buffer `pairs` slice, at `}` emit `make(map, len(pairs))` + fill.
      Runtime-learned count — not the rejected tag-derived sizing. Measured
      micro -41-44% map build; ~3-5% Mega, 20-30% map-heavy. Cautions: B/op
      grows at n≥25 (geometric pairs growth or spill-to-map); ~45ns absolute
      regression at n=6-8; struct values stay direct (dup-key fresh-decode,
      opt #37).
    - **Length-gated SWAR string-span scan, decode.** SWAR prelude
      (`LittleEndian.Uint64` + escape mask + `TrailingZeros64`) emitted ONLY
      for provably-long fields (KindBytes base64; large/absent maxlen); keys +
      short fields keep byte loop. Measured: Small_Unmarshal -37%, Mega
      neutral. Un-gated all-sites variant regresses Mega +2% (register
      pressure) — gate is load-bearing.
    - **Grammar-only `skipNumber`.** SkipValue full-ParseFloats discarded
      numbers (~4.75% cum decode). Validating one-pass number state machine in
      SkipValue/skipArray/skipObject + stream mirror. Measured 4.5×/skipped
      number, ~1-1.5% Mega; multiplies on RawMessage-heavy/ignoreunknown.
      Decide accept-set edge ("1.", "-.5", 1e400 — ParseFloat hard-errors
      today, grammar skip would accept); error identity flips *NumError →
      ErrBadNumber at skip sites. Land with old-vs-new accept-set fuzz.
    - **Fused span-scan + Eisel-Lemire in `Float64`.** Span walk then
      ParseFloat re-scans (duplicate classification, ~4.65% cum). Accumulate
      mantissa+exp10 in existing loop; exact path + pure-Go EL (≤19-digit
      mantissa, ~60 LOC + 11KB pow10 table), ParseFloat fallback. Measured
      2-2.7×/number bit-exact over 200K+ values; Mega ~1%. EL variant ONLY —
      exact-only-without-EL regresses 16% (see Tried Rejected). BSD
      attribution for vendored EL.

- **Improve fuzz coverage.** Current surface (`integrationtests/fuzz_test.go`):
  three fuzzers over `Node` — `FuzzScanNoPanic` (panic safety),
  `FuzzRoundtrip` (encode→decode fixed-point), `FuzzCompat` (ggen ↔ jsonv2
  agreement). Gaps: per-feature fuzzers for alias types, every validation rule
  (oneof/runes/ascii/email/…) with rule-specific generators, streaming path
  (chunked reader, varied chunk sizes), `[N]T` strict-length arrays,
  `KindAny`/`KindRawJSON` edge cases, `omitempty`/`omitzero` round-trip,
  multierr accumulation. Add seeds for tricky inputs (truncated
  `\uXXXX`, surrogate pairs, `null` mid-value, trailing-garbage).

- **Add more CLI flags.** Candidates: `-out-dir` for shared output (vs
  next-to-source), per-struct selectors beyond trailing-name filter
  (`-only=Foo,Bar`), explicit `-tag <tag>` scope one build-tag bucket.
  None urgent. (`-dry` shipped; `check.go` entry points factored for future
  `ggenvet`.)

- **Custom vet tool.** Ship `ggenvet` (`go vet -vettool=ggenvet`) for misuses
  compiler can't see. Biggest: **zero-copy aliasing footgun** —
  decoded strings alias source `[]byte`, mutating input after
  `DecodeFrom` silently corrupts values. Flow-sensitive check flags any
  write to `data` arg (index assign, `append` over same backing, `copy`)
  after pass to `DecodeFrom`. Other checks: stale generated file
  (struct with `//ggen:generate` whose `_ggen.go` lacks method set);
  annotation/tag mismatch (`required` on `omitempty` pointer; `oneof` values
  don't lex as field kind; `trim` on non-string); extend
  parse-time applicability matrix into vet. Shape: separate `ggenvet/`
  subpackage with own `main.go` (`go install …/ggenvet@latest`), reuse
  ggen's parse layer so checks stay in sync.

- **`AppendAny` output prealloc via size precalc.** ggen ties/barely beats
  jsonv2/stdjson on typed slice marshal (`[]int` 712 vs 745 ns/op) despite
  winning 2-4× on maps. Cause: bench passes `nil` dst, so `AppendAny` runs
  growth chain (0→8→…→1024), 7-8 allocs for ~330 B; stdjson/jsonv2 hide this
  with pooled buffers. Presized buffer benches drop to 0 allocs (ggen wins
  1.85× on `[]int`). Options: (a) reflect-driven pre-walk for size, only when
  `cap(dst)==0` AND input is concrete homogeneous container with fast path
  (skip `[]any`/`map[string]any` — unbounded); (b) internal `sync.Pool` inside
  encode package's `Marshal` (NOT `AppendAny` — keep caller-owned dst); (c)
  explicit `AppendAnySized(dst, v, hint)`. Pick when real workload pins slice
  marshal as hotspot — map wins dominate today, slice tie acceptable.

- **Position context on `validation.*` errors.** Same idea one layer up. Add
  `Pos int` (maybe `Snippet []byte`) to `MinLenError` etc. — generated code
  already has `pos`/`s.Pos` in scope at failure site. Either grow
  `validation.Error` interface or add sibling `PositionedError interface {
  error; Pos() int }`. Pair with parse-error wrap above.

- **Revisit `validation.CustomError` shape.** Today `{Field, Name string,
  Cause error}` + `Unwrap()`. Rough edges: `Name` doubles as rule identifier
  and user-facing label (split into `Rule` + `Name`); no `Value any` field like
  other typed errors (can't expose what validator rejected); `Cause` bare
  `error` (typed sub-interface could improve `errors.As`). Pick when
  concrete report-shape ask exists.

- **`sql.Null[T]` (Go 1.22 generic form) fast path.** Doesn't match
  `SQLNullSpec` (string lookup against `sql.NullString`/…), so every
  `sql.Null[int]`/`[time.Time]`/… falls through to `encoding/json`
  reflective fallback for both decode and marshal. Two consequences: (1)
  **wire-shape divergence inside family** — legacy `sql.NullX` ships ggen's
  "value or null" shape, generic form reflects out `{"V":val,"Valid":true}`
  (silent footgun); (2) **slow path** — no inline scan, no AppendText, flat
  128-byte JSONSize. Fix: extend `SQLNullSpec` (or add `SQLNullGenericSpec`) to
  recognize `sql.Null[…]`, parse inner type, resolve via `resolveKind`,
  return `SQLNullKind{Field:"V", Inner, Type}` — four codegen paths already
  thread `spec.Field`/`Inner`/`Type` correctly, so one parse-time change
  unlocks all. Open questions: scope of accepted inner kinds (whitelist
  string/int*/uint*/float*/bool/time.Time is tightest); cross-package
  `pkg.Null[T]` probably out of scope. Mirror legacy `SQLNull*Struct` split
  in `integrationtests/sql_test.go`.

- **`null` on non-pointer value kinds — accept-as-zero vs strict-reject.**
  Surfaced by the merge audit (`TestStdCompatMerge_IntentionalDivergences`).
  ggen emits a `null` peek only for pointer / slice / map / `sql.Null*` /
  raw-message fields; every other kind (scalars, `[]byte`, time, duration,
  net/netip, url.URL, big.*, uuid, `,string` scalars) hard-errors on an explicit
  JSON `null`, whereas stdlib v1/v2 accept it (zero the field / no-op). Real
  parity gap: a payload stdlib accepts, ggen rejects. Two stances: (a) keep
  strict-reject (consistent with UnknownKeyError / strict-array / DuplicateKey
  defaults — "use a pointer for nullable") and just keep it documented; (b) emit
  a `null` peek for these kinds that zeroes the field (matches stdlib). If (b):
  add an `inlineNullPeek`-style 4-byte check at the top of each scalar/native
  value emit in `renderField`/`renderStreamField` that sets the field to its
  zero value and advances 4 — mirrors the existing pointer/slice null branch.
  Decide per ggen's strictness philosophy; (a) is the current default. Pinned as
  divergence until decided. (`[]byte` sub-case RESOLVED — KindBytes now
  accepts `null` → nil and marshals nil as `null`.)


# Tried Rejected

- **ConsumeColon fast-path header (`[5b]`).** After landing the two-tier
  inlinable `SkipSpace` (`[5]`), tried adding a bespoke header to
  `(*Stream).ConsumeColon` (`if s.Pos+1 < len && buf[Pos]==':' &&
  buf[Pos+1] > ' ' { Pos++; return nil }`). Measured **dead flat** across 3
  `-randlayout` seeds (p=0.97/0.35/0.11) on Mega_Reader. `[5]` already inlines
  `SkipSpace` into `ConsumeColon`, and the per-key separator handling is a
  negligible fraction of stream cost (string copies + ReadMore dominate).
  Reverted — pure code weight, no win.

- **Indexed marshal slice loop (`[23]` Layer 1).** Premise (from the perf-hunt
  note) was that `for _, v := range ref[1:]` copies each struct element into
  the range var AND again into the value-receiver `AppendJSON` — so an indexed
  `for i := …; ref[i].AppendJSON()` would drop one 256 B copy/element.
  **Wrong for go1.26**: a controlled `-S` A/B (256 B elem, value receiver)
  shows range-by-value and indexed BOTH emit exactly 16 wide-copy MOVUPS = ONE
  256 B copy/iteration — gc already folds the range var straight into the
  receiver slot; the range form is even 17 B larger (iterator bookkeeping). The
  single remaining copy is the value-receiver argument pass, removable only by a
  pointer receiver. No-op; don't reintroduce.

- **Pointer-receiver decode cores (`[23]` Layer 2) — not attempted, vetoed.**
  Would replace the value-receiver `DecodeFrom`/`DecodeFromStream` struct-copy
  traffic with unexported `(*T).decodeFrom` in-place cores + thin value-receiver
  shims (the public surface is pinned by `decode.Decoder[T]` requiring
  `DecodeFrom(data) (T, int, error)` and the `T{}.DecodeFrom(data)` ergonomics —
  a bare `*T` receiver breaks the generic walkers). Subsumes `[15]`. Deferred:
  the copies it removes are cold-path stack writes (store-buffer-absorbed) —
  `[15]` removing 98 of them measured wall-flat, so this likely measures flat
  too, while carrying large churn (return-shape rewrite through
  `parseerr_postpass`, every return site, stream mirror, shim-inline
  verification). Only `DeepNested` (50-level chain, compounded nested moves)
  might show it. Prototype on `Node` + asm-confirm copies vanish + interleaved
  A/B BEFORE committing to the codegen rewrite.

- **Direct-write `encode.AppendInt/AppendUint` replacing strconv.** Fully
  implemented (digit count via `bits.Len64`+pow10, in-cap `dst[:l+n]` extend,
  backward two-digit fill; parity-fuzzed 4M+ values incl. MinInt64/pow10
  boundaries) then dropped: micro re-measure showed -34% on small ints
  (6.9 vs 10.4 ns) but PAR on large (18.6 vs 19.3) — the review's -44%/-8.1%
  claim didn't hold across distributions, and ~15 emit sites + a custom
  formatter to maintain wasn't worth a small-int-only win. strconv keeps
  base-10 paths.

- **Static comma fusion past one conditional field in `renderAppendJSONBody`**
  (per-field comma state machine so fields after an omitempty/omitzero guard
  keep fused `,"key":` constants). Measured n=20 interleaved benchstat:
  presized Mega Marshal +0.5% p=0.678 — dead flat. ~14 predictable
  compare+branch+1-byte appends are pipeline-hidden under escape-scan/memmove.
  Wire bytes verified identical, so not worth doing for cleanliness either.
  Natural follow-on to optimization #20; don't redo.

- **KindBytes inline string scan → `scan.String` call.** Regressed Mega
  +0.8% — third independent confirmation that replacing inline scan code with
  runtime calls loses regalloc/BCE context (generalizes the "removing decode
  inliners" rejection below).

- **Un-gated SWAR string scan at all decode sites.** Regressed Mega +2.0%
  (register pressure in the 32.5KB DecodeFrom). Length-gated variant works —
  see TODO; the gate is the point.

- **Exact-only float fast path (span-fused, no Eisel-Lemire).** Regresses 16%
  per-number on 17-digit floats: wasted mantissa accumulation + full
  ParseFloat redo. Half-measures on the number path are worse than nothing —
  ship fused Float64 only with the EL arm (see TODO).

- **`inlineNullPeek` → uint32 compare.** Mechanism real (`scan.Null` ships
  it) but ~53K peeks × 0.35ns ≈ 0.07% of decode; never a perf bet. Idiom
  cleanup at best.

- **Flat-CPU-share ⇒ wall-clock extrapolation** (methodology, from SWAR-in-
  `AppendStringNoHTML` A/B: 24-28% flat CPU, measured -0.25% ±2.3% wall — the
  Mega marshal walk is memory-latency-bound on the cold 36MB tree). Profile
  shares alone don't justify landing; interleaved end-to-end A/Bs only.

- **Lazy per-key container reset (retain omitted slice/map keys).** Fully
  implemented (reset emitted at each field's dispatch branch — seen machinery
  guarantees single entry per decode; inline catch-all via `_inlineReset` flag
  on first absorbed unknown key) and reverted same day. Reason: reset-at-entry
  is the WANTED contract — a blank/partial payload must yield a blank slate
  for containers while keeping their capacity; stdlib's retain-on-omit merge
  is explicitly not a goal. Divergence stays pinned in
  `TestStdCompatMerge_IntentionalDivergences/omitted_container_reset_vs_retain`.

- **Length-first key dispatch (`switch len(key)` + nested `switch key`).**
  Shipped as original optimization #1, removed 2026-06: A/B vs flat
  `switch key` on 100-field mixed-name-length struct showed flat **-5.7%
  ns/op** (samelen-64 -1.4%), narrow structs (Node Mega, Claim Small)
  statistically equal (p>0.6, n=10). gc already lowers string switches to
  length-grouped binary search / jump tables — the manual outer switch only
  added a redundant layer the compiler can't see through. Resolves former
  Future item "Hybrid key-dispatch at codegen" (flat won outright; no
  per-struct hybrid needed). Bench fixture (deleted after decision):
  100-field struct, name lengths 3-13, ~10 fields per length bucket, all-keys
  payload, bytes-path DecodeFrom. Don't reintroduce without re-running it.

- **Generator emitting `go/ast` nodes instead of text.** Full rewrite on
  `ast-conversion` branch (commit `feadbba`); output byte-identical. Rejected:
  (1) less readable — every `fmt.Fprintf(b, "if %s == nil {…", ref)` becomes
  pointer-struct AST tree you can't skim; (2) higher peak RAM (pointer-heavy
  nodes survive until print); (3) marginally slower codegen; (4) larger binary.
  Kept on branch in case `ast.Walk`-based optimization ever justifies it
  (e.g. replacing `coalesceConstAppends`), but nothing does today.

- **Pointer-arithmetic decoder / `unsafe.Add` byte loads** to eliminate bounds
  checks. Cut bounds checks in `bench_ggen.go` 59→18 (byte path: 0), but
  Unmarshal **regressed ~10%**. Modern AMD64 makes never-taken bounds checks
  ~free (1 cycle, predicted), while `unsafe.Add` defeats compound addressing:
  `data[i]` is one `MOV (base)(idx*1)`, unsafe form takes 2-3 (optimizer
  treats as opaque, loses loop-invariant hoisting). Don't retry unless
  targeting CPU where bounds-check branches mispredict.

- **Removing all decode-side inliners** (inlineSkipWS/ScanInt64/ScanUint64/
  ScanString) for plain `scan.X(...)` calls. `//go:noinline` micro-bench
  showed per-call overhead ~0.4 ns (basically noise), but macro Unmarshal
  **regressed ~15-20%** — inlining matters for register allocation across
  adjacent ops, ICache, compound BCE compiler only does with body
  in scope. Don't trust per-call micro-benches for hot-loop inlining.

- **Stream-path `_s.SkipSpace` inliner** (`inlineStreamSkipWS`). Saved
  method-dispatch frame but kept `_s.Ensure(j+1)` in loop. Raw +7%,
  normalized via EasyJSON Reader **~2% (noise)** — `Ensure` cold path
  dominates Stream throughput. Don't retry without tackling Ensure overhead.

- **Inlining `scan.Bool` / `scan.Float64`.** `//go:noinline` micro-bench:
  call frame fully amortized by body work for primitives at this size (0.24 ns
  of 12.6, call version slightly faster). No win. Same lesson as above.

- **`Ensure(p *int, n int)` + `Anchor`/`Unanchor` for bounded streaming.**
  Original primitive bulk-fetched N bytes by looping `Read` internally, with
  window-shift mode + Anchor/Unanchor to freeze offsets across `SkipValue` for
  RawJSON/`json.Unmarshal` spans. Killed by: (1) "Read in for loop" is
  antithesis of lazy streaming (if you loop on Read, `io.ReadAll` is simpler);
  (2) anchor + `*int` cursor adjustment was stale-position bug source
  (Float64/Number needed `&start` not `&i` or prefix dropped mid-parse).
  Replaced with `ReadMore(keep int) error` (single Read, optional in-place
  compaction) + byte-by-byte multi-byte literal scans. `keep` param came
  back to bound buffer growth without resurrecting bulk-fetch loop; internal
  methods pass `keep=0` (grow-only), only dispatch-loop bounds checks
  compact. Fail-fast preserved (~67 ms vs ~78 ms ReadAll on invalid). Don't
  reintroduce bulk-fetch without fail-fast story that keeps lazy semantics.

- **Stream `Acquire`/`Release` pool with reused buffer.** Pooled `*Stream`,
  `Release` truncated `s.buf` to retain it. Combined with alias-mode strings
  (`unsafe.String` into `s.buf`) this is **silent corruption**: next
  `Acquire` reuses buffer, `Read` overwrites bytes, prior decoded values'
  aliased fields flip content (`n1.Name` `"FOO_TINY"` → `"BAR_VERY"`). Caught
  by two-payload probe; residency bench missed it (content collisions
  matched). Replaced with stack-allocated Stream + caller-owned buf + copy-mode
  strings.

- **`[512]byte` inline scratch in `Stream`.** Avoid buffer heap alloc for
  small payloads via stack-resident scratch array. Failed in original
  tests: escape analysis couldn't prove `&s` safe across
  `zero.DecodeFromStream(&s)` inside then-generic `decode.UnmarshalStream[T]`
  wrapper, so whole Stream (incl. array) heap-escaped. Wrapper now gone
  and call site direct, so constraint may no longer apply —
  worth re-measuring if residency push needs small-payload alloc back.

- **Per-decode arena + `StreamArenaSize`/`StreamArenaCompact` codegen.** Parse
  with aliased strings, walk value to sum string bytes, allocate one
  exact-size arena, copy + rewrite headers via `unsafe.String`. Fully
  implemented. Result on Mega: allocs 347K→128K, B/op 24.6→19.7 MB, but
  **residency unchanged at ~86 KiB/item** and wall clock unchanged (2-walk
  overhead canceled per-string-copy savings). Gap was never per-string
  fragmentation — was per-decode buffer retention + map rebuild allocs (Go
  has no in-place key-rewrite). Removed codegen + `decode/arena.go`. If
  retrying: prove residency gain BEFORE shipping codegen.

- **`maxlen=N` as slice/map prealloc hint.** Used `maxlen=64` to emit
  `make([]T,0,64)` so Mega's 5-26 element slices skipped growth chain.
  Hidden cost: every retained value carried over-allocated cap forever.
  Killing it cut per-item retention 163 → ~67 KiB on bytes path — **biggest
  single residency win in whole exploration**. Now only `len`/`minlen`/
  `hintlen` drive prealloc. Don't reintroduce `maxlen` as sizing hint without
  opt-in mechanism (see `hintlen`).

# Future

- **Validation-derived encode hints.** Use `ggen` tags for encode shortcuts:
  `ascii` → skip escape table; `lte=N` → fixed-width digit formatter instead of
  `strconv.AppendInt`; similar for `oneof`/`len`. Real wins on hot fields, but
  couples encode shape to decode-time validation — same field would marshal
  differently based on `ggen:` tag, blurs marshal contract.
  (Decode-side prealloc already uses `len`/`maxlen`/`hintlen` — that's `make`
  cap hint, not wire-shape change.) Shelved unless target schema makes
  win concrete.

- **Streaming `io.Reader` over marshalled output (state-machine codegen).**
  Per-struct `AsReader()` returning resumable state + `encode.Reader[T](v)`
  exposing `io.Reader`. Suspends mid-marshal so peak memory = caller's `p
  []byte` instead of `JSONSize()`. Three granularity tiers (per-field cheapest,
  ~300 LOC). Only matters when single payload too big to materialize —
  `JSONSize()` fits comfortably in RAM for everything we care about. Shelved
  unless multi-GB request bodies show up. Trivial
  `bytes.NewReader(Marshal(v))` is one-liner users can write.

- **SIMD / AVX2 vectorization for hot scanning loops.** Sonic narrows Mega
  Unmarshal gap partly via hand-written AMD64 AVX2 for quote-scan, WS-skip,
  number parse. ggen does these byte-at-a-time. Candidates: `bytes.IndexByte`
  (already SIMD; verify it vectorizes — used for closing-quote scan); AVX2
  WS skip via `golang.org/x/sys/cpu` + Plan9 asm; number parsing probably not
  worth (strconv is tuned, inline int scan beats call). Pure-SIMD-on-Go
  might claw back 10-15% on string-heavy payloads at cost of per-arch source
  duplication, `go vet`-incompatible asm, Plan9 maintenance, lost portability.
  Try only if profile shows byte-scan loop dominant AND codegen
  complexity acceptable. Don't speculatively add asm "to keep up with sonic";
  gap is small and portability is feature.
