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

- **Stream skip-tree separator/colon/literal refills still grow-only.** The
  per-iteration ','/']'/'}' bound checks in (*Stream).skipArray/skipObject,
  the byte-by-byte literal loops (Bool, the 'null' arms), and anyObject's
  colon check refill with ReadMore(0); at those points len == cap (readers
  fill the window), so each refill DOUBLES the buffer for bytes that are
  being discarded. Compacting needs per-site cursor rebases (the literal
  loops hold j-relative offsets — same class as the 2026-07 skip-tree
  compaction pass, which deliberately skipped these). Perf only, house-rule
  bench-gated (SkipHeavy stream rows + B/op).

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

- **Single-copy `-copy` escape strings — SHIPPED (both tiers, via `scan.Detach`).**
  Was: escaped retained strings under `-copy` double-allocated (`scan.String`/
  `StringAVX*` escape arm → `stringSlow` owned scratch, then a redundant
  `strings.Clone`). Fixed with `scan.Detach(s, data)` — a tier-agnostic helper
  that clones IFF `s` aliases `data` (a pointer-range test; the `stringSlow`
  escape result is a distinct heap alloc → skipped, non-moving GC makes the test
  sound). Copy-mode codegen (scalar + SIMD fall) reuses the SAME aliasing tier
  func then calls `Detach`; `AnyCopy`/`AnyNumberCopy` do likewise. Reuses the AVX
  tier functions directly — NO per-tier `StringCopyAVX*`. `EscapeHeavy/ggen_copy`
  now equals the aliasing `ggen` row in both tiers (scalar 4 allocs, avx512 4
  allocs). See opt #49. (Same pass fixed a pre-existing SIMD gap: `StringAVX*`'s
  `classifyStructural` sized `stringSlow` off the first quote, not the real
  unescaped close via `stringSpanEnd` — SIMD escape decode 44→4 allocs.)

Rejected from past hunts, do not retry without a new argument: **[17]** positional
next-key predictor (payload-order-dependent), **[23]** indexed marshal loop
(go1.26 already folds the range copy) + pointer-receiver cores (vetoed — public
surface pinned by `Decoder[T]`).

- **Raw-span surrogate-escape validation (residual jsonv2 divergence).**
  Decode-side UTF-8 validation SHIPPED for every string-producing path AND
  captured raw spans (`scan.CheckUTF8` at RawMessage/jsontext.Value sites —
  cli/CLAUDE.md opt #50). Two DECIDED exceptions (2026-07): skipped spans
  (`ignoreunknown`/`SkipValue`) stay grammar-checked only — intentional, keeps
  the skip tiers on the plain non-accumulating kernels; and unpaired `\uXXXX`
  surrogate ESCAPES inside a raw span pass (they're ASCII text there; jsonv2
  escape-parses raw strings and rejects — pinned as a divergence in
  `TestRawCaptureInvalidUTF8`). Closing the latter means surrogate-pairing
  escape parsing inside the skip walk under capture — only bother if a real
  consumer feeds captured raw spans back into a strict v2 decoder.

- **Vectorized UTF-8 validation — SHIPPED for the SIMD tiers (2026-07).**
  `validUTF8x16` (scan/simd_utf8_amd64.go, Lemire/simdjson nibble-LUT
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

- **Claw back EscapeHeavy's +12.7% (control-tight regression, and it flipped a
  sonic loss from 0.93× → 0.82×).** Suspect is concrete: `stringSlow` now does
  `rawHi |= c` PER BYTE inside the escape copy loop (to decide whether the
  final `utf8.Valid(buf)` is needed), where the pre-escape prefix already gets
  the bulk SWAR treatment via `ctrlOrHigh`. Fix shape: validate raw RUNS in
  bulk instead of accumulating per byte — the loop copies byte-at-a-time
  between escapes, so hoist each raw run (scan to the next `\` or `"` with
  IndexByte, `ctrlOrHigh` the run, bulk-append it) rather than touching each
  byte twice. That also restores bulk copying, which the current per-byte
  append lost. Measure with the EscapeHeavy family (its avx512 control is
  0.5% — the most trustworthy row in the suite).

## Tooling / coverage

- **Improve fuzz coverage.** Current surface (`integrationtests/fuzz_test.go`):
  three fuzzers over `Node` — `FuzzScanNoPanic` (panic safety), `FuzzRoundtrip`
  (encode→decode fixed-point), `FuzzCompat` (ggen ↔ jsonv2 agreement). Gaps:
  per-feature fuzzers for alias types, every validation rule (oneof/runes/
  alphanum/…) with rule-specific generators, streaming path (chunked reader, varied
  chunk sizes), `[N]T` strict-length arrays, `KindAny`/`KindRawJSON` edge cases,
  `omitempty`/`omitzero` round-trip, multierr accumulation. Add seeds for tricky
  inputs (truncated `\uXXXX`, surrogate pairs, `null` mid-value, trailing-garbage).

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

- **MaxDepth boundary offset vs jsonv2 (documented divergence, 2026-08).**
  The depth cap counts only containers inside the any/skip/raw subtree —
  acyclic generated-struct levels enclosing it are uncounted (`const _depth =
  0`), so at the EXACT 10000 boundary ggen accepts a document jsonv2 rejects
  (`{"a":` + 10000×`[`). Boundary-exact parity means threading real depth
  through every acyclic struct — codegen churn + a runtime add on shapes the
  cap exists to protect from pathology, for one-off-by-K at a 10000 cliff.
  DECIDED not worth it; the cap's purpose (no stack overflow) is unaffected.

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
  every error type but NOT in the `Error` interface, so `decode.NewParseErr`,
  `Errors.Append`, and `Errors.PrependPath` each assert
  `interface{ PrependPath(string) }` inline. Candidates: put `PrependPath` in
  the interface outright (external implementors just gain a required method —
  breaking is fine here), or restructure the ~20 error structs around a shared
  embedded base (`Pos int; Path []string` + one `PrependPath` impl) which also
  kills the 20 copy-pasted one-liners. Fold into the CustomError-shape revisit
  below if both happen at once.

- **Revisit `validation.CustomError` shape.** Today `{Field, Name string, Cause
  error}` + `Unwrap()`. Rough edges: `Name` doubles as rule identifier and
  user-facing label (split into `Rule` + `Name`); no `Value any` field like the
  other typed errors (can't expose what the validator rejected); `Cause` is a bare
  `error` (a typed sub-interface could improve `errors.As`). Pick when a concrete
  report-shape ask exists.

- **`validation.*` error position follow-up.** Errors carry `Pos int` (full-payload
  byte offset). Open if ever wanted: a `Snippet []byte` around the failure offset
  (rejected for now — the caller has the input + `Pos`).

- **`pipe:` tag follow-ups.** Foreign-package converter inputs (import plumbing);
  top-level alias-type `pipe:` support (needs a non-dispatch null branch in the
  alias renderers); CONTAINER converter inputs (`@Conv` with W = []T/map) —
  currently rejected at parse (2026-08, was silently-broken codegen before), to
  support: populate converterInputField's ElemType/ElemKind/ElemIface from
  go/types — the dedicated-kind element delegation (R3/R4, cbf0949/816fd3c)
  makes the emit side workable now.

- **`nullzero` follow-up.** Extend the per-field `nullzero` decode variant to
  top-level alias types (needs a non-dispatch null branch in the alias renderers).

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

# Tried Rejected

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
  The public surface is pinned by `decode.Decoder[T]` (`DecodeFrom(data) (T, int,
  error)`) and `T{}.DecodeFrom(data)` ergonomics — a bare `*T` receiver breaks the
  generic walkers. The copies it removes are cold-path stack writes
  (store-buffer-absorbed); likely measures flat while carrying large churn. Only
  `DeepNested` might show it. Prototype + asm-confirm + interleaved A/B BEFORE the
  codegen rewrite.

- **Direct-write `encode.AppendInt/AppendUint` replacing strconv.** Implemented
  (digit count via `bits.Len64`+pow10, backward two-digit fill, parity-fuzzed).
  Wins on small ints but PAR on large — not worth ~15 emit sites + a custom
  formatter for a small-int-only win. strconv keeps base-10 paths.

- **Static comma fusion past one conditional field in `renderAppendJSONBody`.**
  Per-field comma state machine so fields after an omitempty/omitzero guard keep
  fused `,"key":` constants. Dead flat — the predictable compare+branch+1-byte
  appends are pipeline-hidden under escape-scan/memmove. Wire bytes identical, so
  not worth it for cleanliness either. Follow-on to opt #20; don't redo.

- **KindBytes inline string scan → `scan.String` call.** Regressed Mega — another
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
  scan/CLAUDE.md.

- **`inlineNullPeek` → uint32 compare.** Mechanism real (`scan.Null` ships it) but
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
  ScanString) for plain `scan.X(...)` calls. Per-call overhead is noise, but macro
  Unmarshal regressed ~15-20% — inlining matters for register allocation across
  adjacent ops, ICache, and compound BCE the compiler only does with the body in
  scope. Don't trust per-call micro-benches for hot-loop inlining.

- **Stream-path `_s.SkipSpace` inliner** (`inlineStreamSkipWS`). Saved the
  method-dispatch frame but kept `_s.Ensure(j+1)` in the loop — `Ensure` cold path
  dominates Stream throughput, so the normalized win was noise. Don't retry without
  tackling Ensure overhead.

- **Inlining `scan.Bool` / `scan.Float64`.** Call frame fully amortized by body
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
  `decode.Iter[T](r io.Reader, buf []byte) iter.Seq2[T, error]` (Go 1.23
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
  Per-struct `AsReader()` returning resumable state + `encode.Reader[T](v)`
  exposing `io.Reader`. Suspends mid-marshal so peak memory = the caller's `p
  []byte` instead of `JSONSize()`. Only matters when a single payload is too big to
  materialize — `JSONSize()` fits comfortably in RAM for everything we care about.
  Shelved unless multi-GB request bodies show up; `bytes.NewReader(Marshal(v))` is
  a one-liner users can write.

- **SIMD phase 3 (phases 1+2 SHIPPED — see cli/CLAUDE.md #46, scan/CLAUDE.md,
  encode/CLAUDE.md, `.claude/simd-plan.md`, bench numbers in bench/CLAUDE.md).**
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
     `scan/simd_stream_amd64.go`); Mega_Reader −5.2%, Small_Reader −20/−26%.
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
