# SIMD optimization plan — bytes decode path (+ marshal follow-up)

Produced 2026-07-03 from a 14-agent analysis/adversarial-verify pass grounded in
a line-level CPU profile of `BenchmarkNoAlloc_Unmarshal/ggen` (avx512 tier,
1719 ns/op, core-pinned). Phase 1 (`-simd` string-scan tier, opt #46) already
shipped; this plan covers everything else.

**STATUS 2026-07-06: all four ship-track items EXECUTED.** P2 −7.6% NoAlloc;
P3 −2.9%; P1 −22% NoAlloc / −8.8% Mega (plus a short-key scalar-window gate
that flipped Tiny from +15% to flat); P4 landed length-gated (macro-flat,
3.6–10× micro on ≥64 B strings — repo marshal benches carry no long strings).
Headline scalar→avx512: NoAlloc −42%, Small −86%, Mega −8.4%, Tiny flat.
Canonical numbers in bench/CLAUDE.md; design docs in cli/CLAUDE.md #46,
.claude/scan.md, .claude/encode.md. Deviations from the plan as written:
P1 needed a bounded scalar tail for strings within one lane of payload end
(near-end tier calls cost Tiny +26%), and the hoisted `_ggenQ/_ggenBS/_ggenSP`
broadcasts were dropped in favor of per-site broadcasts (gc CSE confirmed
sufficient, avoids unused-var plumbing); P4's ungated form regressed
everything (Mega_Marshal +6.5%, Tiny_Marshal +14%) — the sub-lane scalar
delegate gate is load-bearing.

## Ground truth (profile, avx512 tier, 550ms total samples)

| hot spot | share | notes |
| --- | --- | --- |
| object-KEY scan preludes (24B scalar loops) | ~80-90ms flat | biggest single item |
| strconv ParseFloat (readFloat re-scan) | ~40ms cum | span is scanned TWICE today |
| `ggen.StringAVX512` + `classifyStructural` | ~40ms cum | already tiered |
| integer digit loops | ~30ms flat | payload is 10-11-digit, NOT 19-20 |
| SkipSpace + `switch key` + seen-bitmask | ~60ms flat | mostly already optimal |

Payload reality checks that changed verdicts: AccountValue floats are SHORT
(4-9 byte spans, all exactly representable — not 17-digit randoms); its ints
top out at 10-11 digits (no snowflake IDs).

## Ship-track (survived adversarial review)

Execution order: **P2 → P3 → P1 → P4.** Each lands only on an interleaved
core-pinned benchstat (canonical invocation, NoAlloc AND Mega) at the avx512
tier, plus parity fuzz. Stacked NoAlloc expectation: 1719 → ~1450-1550 ns/op
before P4's marshal win.

### P2 — length-gated exact-only float fast path (scalar, no SIMD) — SHIPPED

- **Win:** measured **−9.77% NoAlloc wall** (p=0.029, interleaved core-pinned,
  prototype baseline 2379→2147 ns); verifier projects 4-7% at the 1719 ns
  avx512 baseline — re-measure there before landing.
- **Design:** runtime-only, `scan.go`: (a) `exactPow10 [23]float64`
  table; (b) `exactShort(s []byte) (float64, bool)` ~40 LOC — single pass over
  the already-located span, uint64 mantissa + frac count, accepts only
  `[-]digits[.digits]`, bails on e/E / second dot / mant≥2^52; (c) 3-line gate
  in `Float64` between locate and ParseFloat: `if i-start <= 16 { ... }`.
  Exactness inherits stdlib `atof64exact`'s proof (mant < 2^52 exact, frac ≤ 15
  exact pow10, one correctly-rounded IEEE divide).
- **Why the old rejection doesn't apply:** the backlog rejected the UN-GATED
  fused variant whose cost was unconditional mantissa accumulation on 17-digit
  floats (+16-23% there). Shortest-form 17-sig-digit floats are ≥ 18 chars, so
  the ≤16 B gate structurally excludes them — same "the gate is the point"
  precedent as the SWAR string scan.
- **Evidence:** fuzz-verified bit-identical over 27.6M execs incl. error
  identity; Mega flat.

### P3 — shipped-scanner instruction shaves (simd_amd64.go) — SHIPPED

- **Win:** est. 1-3% NoAlloc; trivial diff; also cleans up P1's emitted shape.
- **Findings (asm-verified):**
  - AVX/AVX2 unsigned `Less` is **emulated** (xor-0x80 + VPCMPGTB) and the
    0x80 broadcast is re-materialized EVERY loop iteration. Workaround:
    `v.Min(Broadcast(K)).Equal(v)` ⇔ `v <= K` — VPMINUB+VPCMPEQB, 2 instrs,
    hoistable (probe: loop body 9→6 instrs). Use `Min(0x1F).Equal(v)` shape
    for the ctrl-byte test in StringAVX/StringAVX2.
  - AVX512 `Mask.Or` round-trips through the vector domain
    (VPMOVM2B+VPORD+VPMOVB2M, not KORQ) — ~4 extra instrs/iter in
    StringAVX512. OR in the vector domain, single `ToBits` at the end.
- Re-run `TestStringSIMD_Parity` + interleaved A/B.

### P1 — inline vector string/key scan in generated code (the big one) — SHIPPED

- **Win:** micro 2.0× on the exact replaced shape (8.4 → 4.18 ns/key, Account
  key mix); est. **6-12% NoAlloc wall**. Attacks the #1 profile item.
- **Design** (`cli/generate.go`, simd tiers only; scalar tier + stream
  unchanged, ~70-100 LOC):
  1. `inlineScanStringVar`: replace the 24-byte scalar prelude with an inline
     vector site (~16 emitted lines): guard `KE+W <= len(data)`; full
     `LoadUint8x{16,32}Slice` (**never `Load*SlicePart` inline — it is a real
     CALL, not an intrinsic**); fused Equal/Equal/Less classify → ToBits →
     TrailingZeros; quote hit → alias (or `string(...)` under -copy);
     otherwise fall back to the existing `ggen.StringAVX*` call (guard-fail,
     escape, ctrl, span > lane−1, near payload end). Error identity
     byte-identical — fallback rescans from posIn like today.
  2. `renderDecode`: hoist `_ggenQ/_ggenBS/_ggenSP` broadcasts once after the
     `result = recv` prologue (gc CSEs per-site broadcasts anyway —
     belt-and-braces; skip for alias renderers to avoid unused vars).
  3. Lane by tier: avx → Uint8x16 (spans ≤15B inline), avx2/avx512 → Uint8x32
     (≤31B; measured same speed as 16B, halves fallbacks; covers all Account
     keys). Do NOT use 64B inline.
  4. Generated imports gain `simd/archsimd` + `math/bits` when tier on.
- **Asm evidence:** 5.4KB probe function, 13 sites: 13 VMOVDQU + 13
  VPMOVMSKB, only 4 VPBROADCASTB total (CSE'd), constants register-resident on
  the hot path, zero CALLs; per-site ≈ 13-15 instrs.
- **Risks:** confirm regalloc on the real ~1000-line Account.DecodeFrom asm;
  A/B Mega for icache regression; avx (16B) tier may regress 16-24B strings
  that today's 24B prelude handled inline (option: two unrolled 16B checks);
  reserve `_ggen*` local names.

### P4 — encode escape-scan tier (marshal side) — SHIPPED (length-gated)

- **Win:** verifier measured the replaceable per-byte `[256]bool` walk at
  **14.5% of Mega_Marshal CPU** (AppendStringNoHTML = largest flat consumer);
  est. 5-9% marshal wall.
- **Design:** new `simd_amd64.go` (~180 LOC):
  `AppendString{,NoHTML}{AVX,AVX2,AVX512}(dst, s)`, same caller contract.
  Fused needs-escape classify (Equal `"`/`\` + Less 0x20; htmlescape adds
  Equal `<`/`>`/`&` = 6 masks); on hit iterate set bits (`m &= m-1`), bulk
  append clean spans, shared cold `appendEscapedByte` helper (existing 8-case
  switch). **Tail difference vs decode:** mask padding up front
  (`m &= 1<<(len-j) - 1`) because encode iterates ALL bits — padding zeroes
  classify as ctrl and would emit spurious escapes.
  Codegen: one-name swap — all 12 emit sites already route through
  `appendStrFn`. `JSONSize` needs nothing (len*2+2 formula, no walk).
- **Risk:** backlog names Mega marshal memory-latency-bound — part of the
  table-test time may be cache misses a vector load still pays. A/B decides;
  Tiny_Marshal must not regress.

## Parked (mechanism proven, no current beneficiary)

- **Integer SWAR digit parse** (pure uint64, no simd import). Gated 8-digit
  chunks, Lemire 3-multiply reduction, bit-exact over 28M fuzz execs; −33% on
  19-20-digit runs, −9-14% on 10-digit. BUT AccountValue's ints are 10-11
  digits → net ~0.3-0.4% end-to-end. Revive when a workload carries multiple
  19-20-digit fields (snowflake IDs, `format:unixnano`) — prototype at
  scratchpad/swarbench (session-local, re-derive from design above).
- **skipString/SkipValue tier.** Whole skip tree = 2.6% of Mega CPU (skip-heavy
  bench doesn't exist). Recorded design: shared `skipEscape` factored out of
  scalar skipString; per-tier `structuralIndexAVX*` + ~30-line skipStringAVX*;
  SkipValue tiering via 3 thin near-identical copies differing only in the
  skipString callee (no func-var dispatch — violates no-runtime-state).
- **Validation charset predicates.** Vector range compares are real (17× at
  8KB, break-even ~10-12B; class-table rejection doesn't apply — zero memory
  traffic) but no in-repo beneficiary outside the synthetic RuneGated bench.
  If revived: short-string scalar gates mandatory; the proposed SIMD rune
  count (len − continuation bytes) is WRONG on invalid UTF-8 — must not ship
  without full parity on arbitrary bytes.

## Rejected (do not retry without new evidence)

- **Structural stepping SIMD** (WS skip / colon / comma lookahead): each step
  consumes ONE byte; classify costs 8-10 uops vs 5-9 scalar. Amortizing needs
  a structural index = a tokenizer — architecturally rejected.
- **`switch key` vectorization:** gc already emits length jump-table + inline
  immediate compares, zero memequal calls. Nothing to beat.
- **Seen-bitmask:** 3 register-only uops/field. Optimal.
- **Scalar colon/comma fusion peephole:** honest prototype measured ~1.2-1.7%
  at the shipping tier — below the 2% bar. (Design recorded in the workflow
  output if a future profile changes the math.)

## archsimd landmines (reference for all future work)

- `Load*SlicePart` = real CALL, not intrinsic — never in inline hot paths;
  fine in runtime tier functions' tails.
- Unsigned byte compares emulated on 128/256-bit (re-broadcast 0x80 in-loop);
  use the Min/Equal trick. Native single-instr on 512-bit only.
- AVX512 mask And/Or round-trip through vector domain — combine in vector
  domain, one ToBits.
- Mask API is And/Or/ToBits only (no Not/Xor/AndNot).
- `DotProductPairsSaturated` (vpmaddubsw) + pair-multiply-add exist at 128-bit
  — the simdjson digit-parse ladder is available if the int candidate revives.
- GFNI bit-matrix classify: AVX512-GFNI only.

## Full analysis artifacts

Workflow output (14 agents, designs + adversarial verdicts + evidence):
session scratchpad `tasks/wx43pu89e.output`; probe asm dumps + prototypes in
the session scratchpad (`probe/`, `swarbench/`, `fuse.py` — session-local,
regenerate from the designs above if needed).
