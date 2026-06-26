# TODO

- **Perf-review findings (2026-06-11 audit) — verified, not yet landed.** All
  prototyped + A/B'd at Mega scale, then reverted; numbers below are measured,
  not estimated. (`skipString` bounded backslash probe — the 5.1× stream fix —
  already landed.) Suggested order: stream pair → decode pair → encode.
    - **Stream `s.buf` hoist. LANDED 2026-06-18.** Nested-loop restructure
      (`buf := s.buf; for i < len(buf)`, refill in outer loop) registerizes the
      slice header across `skipSpaceSlow`/`Int64`/`Uint64`/`Float64`/`Number`
      (`scan/stream.go`). Measured **−2.0% Mega_Reader/ggen_stream** (p=0.000,
      n=30, core-pinned, ±1% spread), throughput +2.1%, allocs/B byte-identical.
      Behavior byte-identical (Shift-gated cursor reset preserved). See
      scan/CLAUDE.md "Buffer-header hoist in refill loops".
    - **Exact-cap comma pre-count for flat numeric/bool SLICES, bytes path.
      LANDED 2026-06-22.** `bytes.Count(',')+1` → `make([]E,0,cnt)`,
      `scalarCountable` gate, `userPreallocHint < 0`, every depth via
      peelSliceField, reused slots keep backing (nil-guard). **Maps
      deliberately excluded** — a valid object key with N colons would inflate
      a `:`-count into an N-sized make (memory amplification on well-formed
      input); the slice case is immune (scalars can't carry `,`). Map runtime
      sizing → the buffer-then-build track below. Measured interleaved
      core-pinned across 2 `-randlayout` seeds: Mega_Unmarshal **−10% wall,
      −36.6% allocs, −21% B/op**; readall −4%; marshal control flat. See
      CLAUDE.md opt #42.
    - **Hoist nested-container slot into reuse-seeded depth-local. LANDED
      2026-06-22.** `rowN := <slot>` (seeded from the carried slot, slice
      reset `[:0]`); recurse into rowN; `target = rowN` after inner `]`. Outer
      slice-of-slice grows by reslicing within cap so the carried inner header
      survives → inner row BACKINGS reused on decode-into-receiver (extends the
      top-level `[:0]` contract one level down; pinned by
      `TestMerge_nestedSliceBackingReused`). Both bytes + stream emitters.
      asm-verified 4→2 barrier sites; **−1.8..−4.85% Mega_Reader/ggen_stream**
      (allocs flat on fresh decode), composes with the comma pre-count above.
      See CLAUDE.md opt #43.
    - **Grammar-only `skipNumber`. LANDED 2026-06-23.** `SkipValue` skips
      numbers via a one-pass RFC 8259 grammar validator
      (`-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?`) instead of
      `Float64`/`ParseFloat` — discarded numbers (RawMessage, `ignoreunknown`,
      `allowdups` skips) only need the end position. Bytes `scan.skipNumber` +
      stream `(*Stream).skipNumber` (single chokepoint: SkipValue's number
      case; skipArray/skipObject route through it). **Accept-set decided =
      strict JSON grammar**, both shifts toward stdlib parity: range-overflow
      (`1e400`) now skips OK; ParseFloat-isms (`+1`, `01`, `1.`, `.5`) now
      rejected; error identity `*NumError` → `ErrBadNumber` at skip sites.
      Measured interleaved core-pinned (`taskset -c 4`, `-cpu=1`, n=12):
      **−3.04% Mega_Unmarshal** (p=0.000, +3.13% throughput, −1 GC) / **−1.10%
      Mega_Reader/ggen_stream** (p=0.020; stream is copy/ReadMore-dominated so
      smaller), allocs/B byte-identical on both, beating the ~1-1.5% estimate
      (Mega's `Raw` field is number-heavy). Accept-set pinned by
      `TestSkipNumber_AcceptSetMatchesJSONGrammar` (differential vs
      `encoding/json.Valid`) + `TestSkipNumber_StreamMatchesBytes`; fuzz-clean
      (FuzzCompat ggen↔jsonv2 + FuzzStreamEqualsBytes, ~20M execs). See
      scan/CLAUDE.md.

- **Open perf candidates (2026-06-13 hunt — UNMEASURED, prune freely).** Source-
  verified, no A/B run. The four LANDED items above + the rejected float / stream-
  slab / map-buffer items below exhaust the 2026-06-11 audit; these are the lower-
  confidence remainder, condensed from the now-deleted (never-committed)
  `.claude/perf-candidates.md` — the source-line citations below are the only
  surviving detail, so re-derive mechanism from them. House rule: nothing lands
  without an interleaved core-pinned benchstat — Mega is memory-latency-bound, so
  CPU-only shaves routinely vanish in wall clock.

    *validation (highest-value unmined):*
    - **[25] rune-rule byte-length gating + ASCII subsumption. LANDED 2026-06-26
      (all three tiers).** From `ceil(B/4) <= R <= B` for a B-byte UTF-8 string:
      (b) byte-length gates resolve fail-free/pass-free in O(1), walk only the
      ambiguous band (`minrunes=N` band `[N,4N-3)`, `maxrunes=N` band `(N,4N]`,
      `runes=N` band `[N,4N]`; `minrunes=1` collapses to an empty-string check);
      (c) when an ASCII-implying rule (alphanum/numeric/ascii/hex) PASSED earlier
      in the same run, `R == len` so the walk is dropped entirely. Tier (c) gated
      on `asciiSeen && !multiErr` in declared order (position-sensitive; skipped
      under multierr where a failed earlier rule doesn't stop reaching the rune
      rule on non-ASCII input). The intermediate hoist (a) was superseded by the
      gates — `emitRuneRule` (gating) + `emitValRun` (asciiSeen tracking) in
      generate.go; the `rcVar` hoist param on `renderOneVal` was removed. Wire-
      identical (same accept/reject, error type, `Got`). Measured interleaved
      core-pinned (n=12): **flat on ValidationHeavy** (short strings — the skipped
      walk is cheap) but **−46.8% RuneGated_Unmarshal** (p=0.000, +88% throughput,
      ~8 KB strings of 2048 four-byte runes) — allocs/B byte-identical. Opt #44;
      pinned by `TestGenerate_runeGates` + `BenchmarkRuneGated_Unmarshal`.
    - **[4] single-pass IsEmail LANDED 2026-06-26; class-bitmask predicates
      REJECTED (see Tried Rejected).** IsEmail now tracks the last in-domain `.`
      inline during the `@` scan — one pass, not the old scan-then-rescan-domain.
      **−33.3% head-to-head** (`BenchmarkIsEmail` single_pass vs two_pass, one
      binary so zero layout confound, 64 KB email, p=0.000 n=12); never worse on
      short. Wire-identical, parity-pinned (`TestIsEmail_SinglePassParity`,
      `TestCharsetPredicates_Parity` — first tests for these predicates). The
      256-byte class-table half was prototyped and rejected (regresses long
      strings — see Tried Rejected). IsASCII SWAR untried.
    - **[26] container maxlen early-bail inside element loops.** maxlen is validated
      only AFTER the loop, so a 10M-elem payload vs maxlen=64 fully decodes+ALLOCATES
      before failing. Loop-top `if len(dst)==MAX { MaxLenError }` caps work at MAX+1
      (multierr: append once, `SkipValue` the rest). DoS-hardening, ~0 on valid input.
      SEMANTICS DECISION REQUIRED: `MaxLenError.Got` becomes MAX+1, multierr stops
      collecting dive errors past the bound. Touches every container emitter.

    *encode:*
    - **[11] AppendAny reflect.Map scratch. LANDED 2026-06-27.** The `reflect.Map`
      walk reused `iter.Key()`/`iter.Value()` (a fresh reflect.Value per entry, 2
      allocs/entry) → now two addressable scratch Values via `Value.SetIterKey`/
      `SetIterValue`, so per-entry Value allocs become 2 fixed. Deterministic
      alloc measure (32-entry `named_map_int` bench): **71 → 9 allocs/op (−87%),
      −32% ns** — a named-map-of-primitive now ~0 alloc/entry (value read off the
      Value, no box). Only `any` fields reach this (generated code emits direct
      map code). The `ConvertibleTo`-to-concrete tier was unneeded — the scratch
      already 0-allocs primitive-value maps. Wire-identical (`TestAppendAny_
      ReflectHeavy` vs jsonv2). encode/any.go.
    - **[12] AppendAny boxing: tier (a) LANDED 2026-06-27, tier (b) REJECTED.**
      (a) Concrete composite-element slice cases `[]time.Time` + `[]json.RawMessage`
      handled wholesale at the top switch → elements skip the reflect.Slice
      per-element `rv.Interface()` box (32-elem `time_slice`: **40 → 8 allocs/op
      (−80%), −41% ns**). (b) Reading primitive STRUCT fields off the reflect.Value
      in `appendStruct` (vs `appendAny(fv.Interface())`) measured **dead flat**
      (`struct_slice` 41 → 41 allocs) — gc already elides the primitive-field
      interface box via inlining/escape analysis, so it was reverted. `[][]string`
      (tier-a example) skipped as arbitrary. Pinned by `TestAppendAny_ReflectHeavy`
      + the new bench shapes.
    - **[10] remainder: generic `Marshal` LANDED 2026-06-27; `Write` presize
      REJECTED.** `Marshal`/`MarshalString` are now generic over `T Marshaler`
      (was a plain `Marshaler` arg) → a concrete call devirtualizes and skips
      the interface box: **Tiny_Marshal/ggen 2 → 1 allocs/op, 320 → 224 B/op,
      −33% ns** (deterministic). Generated `MarshalJSON` hook (`encode.Marshal(s)`)
      infers T, unchanged. Caveat: can't be a bare func value (`f :=
      encode.Marshal` needs `[T]`). Presizing `WriteTo`/`WriteSliceTo`'s pooled
      buffer from JSONSize was built + A/B'd and **regressed** (+3% tiny / +4%
      mega) — the pool converges to the max payload size, so the size walk is
      pure overhead (a second full tree walk at mega); reverted. (`Write`/
      `WriteSlice` renamed to `WriteTo`/`WriteSliceTo` this pass.)
      Generic `Marshal[T]` resolves the [10] interface-box note in full.
    - **[14] remainder**: 256-byte shared escape table for AppendString/NoHTML —
      cache-resident payloads only (Mega marshal proved wall-flat, SWAR precedent).

    *bytes decode (likely flat on Mega — cheap probes / hygiene):*
    - **[19] localized scan cursor + lower seen-bitmask threshold.** Inline int/uint
      scanners loop on the function-lifetime cursor (per-digit frame store); loop on a
      short-lived local `p`, sync at error returns. Drop `seenBitmaskThreshold` 32→12-16
      for recursive structs (Node: 15 bool slots → one register `_seen uint64`). One-
      line probes; likely store-buffer-hidden on Mega. Error-return cursor sync is the
      correctness gate.
    - **[18] redundant leading WS-skip + dead identical empty-peek arms.**
      emitByteSliceRead/renderMap re-skip WS every caller already skipped (the alias
      path is the one real dependency → needs a `wsDone` flag); `sCap==0` empty-peek
      emits two byte-identical arms. Wire-identical hygiene; expect flat. WS-variant
      fixtures are the gate, not ns/op.
    - **[16] remainder**: do-while element-loop restructure (drop the redundant loop-top
      `]`/`}` re-check) — the trailing-comma correctness half already landed.

    *stream (low-confidence remainder — stream is copy/ReadMore-dominated):*
    - **[9] `[512]byte` inline Stream scratch re-measure.** The escape-analysis blocker
      (the since-removed generic UnmarshalStream wrapper) is gone — but the self-
      referential `s.buf = s.scratch[:0]` store may still heap the now-~576B struct
      under field-insensitive EA. RUN THE `-gcflags=-m` CHECK FIRST; stop if it trips.
      Win is −1 alloc on nil-buf one-shot decodes only (Mega/recycled flat); needs a
      no-copy contract.
    - **[6] fused `Stream.Key()`** (key + colon + post-colon WS in one call, grow-only
      refill keeps the alias). DEPRIORITIZED — [5b] showed separator handling is cheap;
      only a small incremental call reduction over the landed two-tier SkipSpace.
      Revisit only if a key-fusion micro-bench justifies.
    - **[20] whole-literal buffered null-peek compare.** Stream null peek is a per-byte
      loop with a refill branch, duplicated ~8× in Node.DecodeFromStream. Gate on
      `s.Pos+4<=len` for a straight 3-byte compare, or outline a cold helper. ~0 perf
      (runs only on actual nulls); code-size cleanup only.

    Rejected from this hunt (do not retry without a new argument): **[17]** positional
    next-key predictor (Validated +4.64%, payload-order-dependent), **[23]** indexed
    marshal loop (go1.26 already folds the range copy) + pointer-receiver cores
    (vetoed — public surface pinned by `Decoder[T]`).

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

- **Position context on `validation.*` errors — RESOLVED 2026-06-23.** Every
  error now carries a single `Pos int` (first field, not a grown interface /
  sibling type — keeps `errors.As` on the concrete pointers): the byte offset
  of the failure **relative to the full payload**, identical on bytes + stream.
  Bytes path uses the cursor `i`; stream path uses `scan.Stream.Offset()` (a
  new `consumed` counter + `Offset()` method, `consumed` incremented per
  compacting `ReadMore`) — NOT the raw window-relative `s.Pos`, which the
  sliding/compacting buffer invalidates. (A two-field `Pos`+`BufPos` shape was
  built first and rejected — a single full-payload offset is the contract; the
  caller already has the input to slice from it.) Codegen injects via
  `withPos`/`posLit` in `generate.go` (wraps `onErr` + standalone
  required/array-len/dup-key/unknown-key literals). Pinned by
  `TestValidationError_Pos`. Open follow-up if ever wanted: `Snippet []byte`
  around the failure offset (rejected for now — caller has the input + `Pos`).

- **Struct-field tag design — RESOLVED 2026-06-25 (hard break).** The
  `ggen:`/`mod:` split is gone; field config is now partitioned by ROLE across
  three tags: `json:` (wire shape — name/omitempty/omitzero/string/inline/
  format, stdlib-compatible), `pipe:` (one ordered decode→transform→validate
  pipeline), `hint:` (prealloc only). The `pipe:` tag interleaves mods +
  validators in declared order, lifts presence (`required`/`optional`), moves
  `nullzero` to a decode-stage variant, adds multi-shape `/` decode dispatch
  (native / nullzero / `@Conv` converters) and a `;` after-loop level pop.
  Custom funcs are classified by signature (validator / mod / converter, bool
  forms). Design spec: `.claude/tag-redesign.md`. Implemented across
  `pipe.go` (grammar + model), `variants.go` (shape dispatch), `customfunc.go`
  (classification), unified `renderPipe` in `generate.go`; legacy parsers/
  resolvers deleted; whole corpus + docs migrated. Open follow-ups: foreign-
  package converter inputs (import plumbing); top-level alias-type pipe support.

- **Revisit `validation.CustomError` shape.** Today `{Field, Name string,
  Cause error}` + `Unwrap()`. Rough edges: `Name` doubles as rule identifier
  and user-facing label (split into `Rule` + `Name`); no `Value any` field like
  other typed errors (can't expose what validator rejected); `Cause` bare
  `error` (typed sub-interface could improve `errors.As`). Pick when
  concrete report-shape ask exists.

- **`null` on non-pointer value kinds — RESOLVED 2026-06-23 via opt-in
  `nullzero`.** Default stays strict-reject (consistent with UnknownKeyError /
  strict-array / DuplicateKey — "use a pointer for nullable"); the parity gap is
  now closable per-field with `json:",nullzero"` or per-struct/globally with
  `//ggen:generate nullzero` / `-nullzero`, which decode an explicit `null` to
  the Go zero value. This is option (c) — the opt-in middle ground between the
  documented stances (a) keep-strict and (b) accept-everywhere. Implemented as
  the `inlineNullPeek`-style 4-byte check (stream: `emitStreamNullZero`) at the
  top of `renderField`/`renderStreamField`, gated by `nullZeroApplies` (set +
  `AtDispatch` + a kind that would otherwise reject null); flat-break when no
  field rules, else-wrap so validation runs on the zero. Struct fields only (not
  top-level aliases). Pinned in `integrationtests/nullzero_test.go` +
  `cli_test.go`. (`[]byte` sub-case had already RESOLVED separately — KindBytes
  accepts `null` → nil and marshals nil as `null`.) Open follow-up if ever
  wanted: extend `nullzero` to top-level alias types (needs a non-dispatch null
  branch in the alias renderers).


# Tried Rejected

- **Inline leaf-struct AppendJSON at nested marshal sites ([22]) — built,
  measured, reverted 2026-06-26.** Emitted a small infallible leaf struct's
  append body inline (vs a call) at field/slice-elem/map-value sites, coalescing
  the parent prefix with the leaf's `{"…":`. **Flat on Mega_Marshal** (memory-
  latency-bound deep tree) — the only win was −4.86% on a contrived cache-
  resident `[]Pt` marshal bench, not a real workload, and it added codegen
  branching + a synthetic bench. Not worth it. Don't re-propose [22].

- **256-byte class-bitmask charset predicates ([4] half) — built, measured,
  reverted 2026-06-26.** Replaced the range-check loops in IsAlphanum/IsNumeric/
  IsHex/IsPrintable/IsLower/IsUpper with one shared `[256]uint8` class table
  (`table[c]&mask`, one indexed load + AND per byte). **Regressed the long-string
  case: +12-27% on IsAlphanum over 8 KB** — the per-byte L1 load can't hide
  behind the loop the way the branch-predicted range comparisons (pure ALU, no
  memory traffic, perfectly predicted on valid input) do. Same lesson as the
  decode-inliner and SWAR-string rejections below: adding memory traffic to a
  tight branch-predictable loop loses. (Measurement was also badly layout-
  confounded — a no-op change showed ±27% on untouched code between builds; the
  clean signal came from a same-binary head-to-head.) Keep the range checks.
  The single-pass IsEmail half of [4] is a real win and LANDED — different
  mechanism (eliminates a whole rescan pass, not a per-byte swap).

- **Fused span-scan + Eisel-Lemire in `Float64` — built, measured, ditched
  2026-06-23.** Fully implemented and verified: shared `fastParseFloat` walked
  the number span once (permissive `[-+.eE0-9]`, cursor/consumed-length
  byte-identical), accumulated a ≤19-digit mantissa + exp10, ran `eiselLemire64`
  vendored verbatim from Go's BSD-licensed `strconv` (`eiselLemire64` +
  `detailedPowersOfTen` table, ~830 LOC in `scan/eisel_lemire.go`); any miss
  fell back to `strconv.ParseFloat` over the SAME span (accept-set provably
  unchanged). Measured interleaved core-pinned (n=12, `-cpu=1`): **−3.86%
  Mega_Unmarshal / −2.10% Mega_Reader** (both p=0.000), allocs/B byte-identical;
  fuzz-clean (13.9M float-parity + 6.4M stream-vs-bytes + 4.5M ggen-vs-jsonv2).
  NoAlloc was layout-lottery (sign flipped −0.6/+1.4/+13.6 across 3 `-randlayout`
  seeds; generated decoder byte-identical, so noise not regression). **Reverted
  by decision**: a real win, but not worth permanently vendoring + maintaining a
  copy of the stdlib's internal Eisel-Lemire implementation and its 11KB
  power-of-ten table inside this repo. If the standard library ever exposes a
  fast float parser (or `strconv.ParseFloat` itself gets fast enough that fusion
  is moot), revisit — until then `scan.Float64`/`Stream.Float64` stay on the
  plain span-scan + `strconv.ParseFloat`. Don't re-vendor EL.

- **Gated stream string slab — dropped by decision 2026-06-23.** Real measured
  win (allocs 256,588 → 104,641, −3..6% wall, Retention parity) but cut from
  the agenda: chunk-size gating is load-bearing (un-gated 8KiB first chunk
  regresses small payloads 3.5× B/op, +30-90% ns) and the gating + lifetime
  reasoning (`unsafe.String` into never-rewritten chunks, dropped on Reset)
  carries corruption-class risk not worth the alloc cut. Bytes path already
  aliases; stream alloc reduction not a current priority.

- **Map decode buffer-then-build — dropped by decision 2026-06-23.** The robust
  runtime map-sizing path (buffer `pairs`, `make(map, len(pairs))` + fill at
  `}`; measured micro −41..44%, ~3-5% Mega, 20-30% map-heavy) cut from the
  agenda. B/op grows at n≥25, ~45ns absolute regression at n=6-8, and it only
  pays on map-heavy schemas. Maps keep the unsized `make()`. The `:`-count
  heuristic stays rejected (below) regardless.

- **Length-gated SWAR string-SPAN scan (closing-quote locate) — not pursued.**
  The control-byte VALIDATION half landed in `a27a1ca` (SWAR `< 0x20` check,
  length-gated, −10..17% long-string stream decode). The remaining half — SWAR
  closing-quote/backslash locate via escape-mask + `TrailingZeros64` (the
  Small_Unmarshal −37% figure) — is deliberately NOT done: `bytes.IndexByte`
  is already SIMD/AVX2 and beats a SWAR span scan on long spans. Quote/backslash
  locate stays on `IndexByte`. Don't fold them into SWAR.

- **Map `:`-count prealloc (sibling of the slice comma-count, opt #42).**
  Shipped briefly alongside the slice count, then dropped same day: sizing a
  `map[string]<numeric|bool>` from `bytes.Count(span, ':')+1` is a
  memory-amplification footgun on WELL-FORMED input — JSON object keys are
  strings, so one valid entry `{"a::::…::": 1}` with N colons in the key
  inflates the count into an N-sized `make`. Unlike the slice comma-count
  (scalar elements can't contain `,`/`]`, so inflation requires malformed
  input that errors before the alloc matters), the map key is attacker/data
  controlled on the happy path. Robust map sizing = runtime buffer-then-build
  (see TODO), not a delimiter heuristic. Don't reintroduce a key-delimiter
  count.

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
