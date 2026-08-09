# bench — perf benchmarks vs jsonv2 / sonic / easyjson

## Benchmarks are a compass, NOT a target

Bench values are like tests: they exist to cover common cases or bugs found
spontaneously during development — they are NOT actual use cases and will
practically never coincide with a real-world workload. Treat a bench delta as a
directional signal of where performance is going, nothing more. NEVER optimize
against a benchmark's specific values — shaping code to a payload's particular
string lengths, key mix, or nesting is fitting to the fixture, not to users.
The numbers below are for science only.

## Files

- Types + payload builders live in per-family non-test files, each paired with
  its `_test.go`: `bench/mega.go` (Node family + DeepNested + MapHeavy),
  `bench/small.go` (Validated + Claim + ValidationHeavy + RuneGated + HTML),
  `bench/simple.go` (Account family), `bench/skip.go`, `bench/escape.go`;
  `bench/doc.go` holds the package doc + the deterministic `gen` counter
  source + `mustMarshal`. Non-test so easyjson's bootstrap (which compiles the
  non-test build) sees them; untagged, so ggen methods land in sibling
  non-test `<file>_ggen.go` files.
  Every value is built deterministically at init — no `math/rand`, no clock —
  so every run parses byte-identical payloads: multi-entry maps exist only in
  MapHeavy, whose payload is `canonicalize`d after encode (stdlib v1
  round-trip, sorted keys — see `doc.go`); every other map (Node's Props,
  map-shaped `any`) is single-entry, immune to Go's randomized iteration
  order — and small enough to stay out of the ns/op signal.
  Each family pairs ggen and easyjson types (see "easyjson method leakage")
  plus `Copy*` wire-identical mirrors carrying `//ggen:generate copy`, so the
  `ggen_copy` Unmarshal rows decode the same payloads through the copy-mode
  bytes path (`CopyNode`/`CopyAddr` for Mega, `CopyAccount` family for
  NoAlloc, `CopyValidated` for Small, `CopyClaim` for Tiny — every decode
  bench family carries a `ggen_copy` row).
- `bench/{mega,small,simple,skip,escape}_ggen.go` — generated ggen methods,
  one per annotated source (each carries `//go:generate ../ggen $GOFILE`,
  same as integrationtests). Regen: `(cd bench && GOEXPERIMENT=jsonv2 go
  generate .)`.
- `bench/{mega,small,simple}_easyjson.go` — generated easyjson methods.
  Regen: `easyjson bench/mega.go bench/small.go bench/simple.go`.
- `bench/mega_test.go` — 4-way Mega benches (jsonv2/sonic/easyjson/ggen) for
  Unmarshal/Marshal/Reader.
- `bench/small_test.go` — small-value (~2.9 KiB ValidPayload) Unmarshal + Reader.
- `bench/slowstream_test.go` — slow-reader benchmarks.
- `bench/simple_test.go` — `BenchmarkNoAlloc_Unmarshal` + `_Reader`.
- `bench/skip_test.go` — `BenchmarkSkipHeavy_Unmarshal` (compact/pretty
  envelope, ~100% skipped content via ignoreunknown).
- `bench/escape_test.go` — `BenchmarkEscapeHeavy_Unmarshal` +
  `BenchmarkEscapeSparse_Unmarshal` (escape-dense strings;
  exercises the unescape path).

## What each bench family measures

- **NoAlloc** (`Account`, wide denormalized record, all nested VALUE structs, no
  slice/map/pointer/`any`/`json.RawMessage`; profile strings are
  Ukrainian-localized Cyrillic, so this row also carries the decode UTF-8
  validation walk — opt #50. Control-checked sweep (2026-07): **+67.7% scalar
  / +38.7% avx512** vs pre-validation; the avx512 figure is low because the
  vector validator (`validUTF8x16`, scan/CLAUDE.md) replaces the scalar
  `utf8.Valid` second pass. Still 2.7-5× ahead of jsonv2, which validates too.
  NOTE this family's control rows drift 3.6-6% on the current box, so treat the
  precision as soft — the effect is far larger than the drift, so the direction
  is solid) — bytes decode makes zero allocs
  (strings alias input, structs decode in place, scalars land in receiver),
  vs easyjson's ~25 allocs (it copies strings out of the input); isolates scan +
  key-dispatch + per-field-assign. Warms up 64 iters. `_Reader`:
  `ggen_stream` starts each decode with a FRESH 512-byte buffer (< payload) so
  the stream genuinely refills + compacts mid-decode (a payload-sized/grown
  buffer reads in one shot, degenerating into the bytes path) — copies strings
  out of the buffer, so NOT zero-alloc.
- **Mega** (`_Unmarshal`/`_Marshal`/`_Reader`, ~4.4 MiB deep `Node` tree).
  `_Reader` includes `ggen_ReadAllUnmarshal` (`io.ReadAll` then bytes decode —
  cheapest io.Reader pattern). Inner loop under `b.RunParallel` (`-cpu=1`
  serial, `-cpu=N` N-way, same path); stateful codecs get per-goroutine state
  via the `setup` closure in `runBench`. Reports `gc` (NumGC delta) alongside
  ns/op·B/op·allocs/op.
- **Small** (~2.9 KiB) — per-call buffer/streaming overhead is visible. Two
  ggen-stream Reader rows: `_512` allocates a FRESH 512 B buffer per iteration
  (< payload) so the grow/refill/compaction chain runs every call; `_full` reuses
  a payload-sized buffer (steady-state, no grow). `_512` must NOT carry the grown
  buffer forward — that settles it at payload size and degenerates into the
  one-shot bytes path (the chain would run on 1 of N iterations; fixed 2026-07,
  matches `NoAlloc_Reader/ggen_stream`).
- **EscapeHeavy** (`EscapeDoc`, ~19 KiB, ~12% escapes: `\n \" \\ \uXXXX` +
  surrogate pairs) — the ONLY tier that drives the unescape path (`scan.stringSlow`,
  `\uXXXX` + surrogate assembly, scratch alloc); no other decode payload
  carries escapes. (Non-ASCII content is a separate axis: the NoAlloc/Account
  profile strings are Ukrainian-localized Cyrillic and RuneGated is multi-byte
  by design, so those two rows — NOT this one — carry the decode UTF-8
  validation cost, opt #50; Mega/Small/Tiny values are asciiLetters.) Its correctness guard (ggen bytes + stream == jsonv2) found a
  real stream bug: surrogate pairs straddling a refill boundary corrupted (😀 →
  ��, see scan/CLAUDE.md). **sonic caveat:** unlike the skip tier, decode must
  produce the unescaped value, but sonic's escape handling still differs
  (ConfigFastest is lax) — read the sonic rows as context, not a like-for-like
  race; jsonv2 is the honest baseline.
- **EscapeSparse** (`EscapeDoc` again, ~21 KiB, prose density: ~90 raw bytes per
  escape instead of ~7) — the other end of the unescape axis. `stringSlow`
  splits its work by raw-RUN length, so EscapeHeavy alone can only show half the
  tradeoff: the 2026-08 windowed-copy-loop change reads −5.9% on Heavy but
  −27.6% here, and the bulk-copy shapes it replaced won on this row while
  costing +73% on Heavy (see `.claude/backlog.md`). Always move the two rows
  together — a change that helps one can wreck the other. Its jsonv2 row is a
  noisier control than Heavy's (drifted ~10% between two identical-binary
  passes); the sonic rows held flat, so cross-check both before trusting a
  delta.
- **ValidationHeavy** (short fields) — per-field validation cost
  (`ggen_validated` vs `ggen_noval` + non-validating jsonv2/sonic/easyjson).
  **RuneGated** companion (~8 KB strings): one ggen row isolating opt #44's
  byte-length gates that skip the O(len) `utf8.RuneCountInString` walk —
  `LongRunes` (2048 4-byte runes) clears `minrunes`/`maxrunes` via `len` gates,
  `AsciiRunes` via tier-c (alphanum-precedes → count is `len`).
- **SlowStream** (`slowReader`, geometric-decay delays). Tables `_Valid`
  (stdjson, easyjson, ggen_stream, ggen_readall) and `_Invalid` (ggen_stream,
  ggen_readall, jsonv2-baseline on a payload failing ggen validation early),
  same `runBench` harness (`-cpu=N` scales near-linearly). Invalid is where
  streaming pays: fail-fast bails on first bad field, ReadAll must drain first.

## easyjson method leakage

`//easyjson:json` generates `MarshalJSON`/`UnmarshalJSON` on the target type.
`jsonv2`, `encoding/json`, AND **sonic** all check
`json.Marshaler`/`json.Unmarshaler` before reflecting, so any type carrying
easyjson methods silently routes every "reflection" codec through the easyjson
fast path — a row labelled `jsonv2`/`sonic` ends up measuring easyjson.

**Pattern:** keep ggen and easyjson on SEPARATE types sharing a wire shape. Feed
the "Plain" (ggen-only) struct to reflection codecs, the "Easy" struct to the
easyjson row.

```go
//ggen:generate
type Claim struct { Sub string `json:"sub"`; ... }

//easyjson:json
type EasyClaim struct { Sub string `json:"sub"`; ... }   // same fields
```

`NodePlain`/`AddrPlain` exist for the same reason at mega level (self-referential
field types meant `type AddrPlain Addr` wasn't enough — see `bench/mega.go`);
for non-recursive structs a parallel struct declaration is cleanest.

**Symptom when forgotten:** a supposedly-reflection row matches easyjson's allocs
and ns/op almost exactly when it should be 3-10× slower. ggen's own
`AppendJSON`/`DecodeFrom` do NOT trip this (not `json.Marshaler`/`Unmarshaler`);
only stdlib-interface methods cause cross-codec pickup — same isolation applies
if a struct opts into ggen `marshal`/`unmarshal` hooks.

## Headline results (~4.4 MiB deep Node tree, full validation)

AMD Ryzen AI MAX+ 395, Go 1.26, GOEXPERIMENT=jsonv2; re-measured 2026-07 on
the deterministic payload (older quotes used the pre-deterministic fixture
and are not comparable). Node carries scalars, slices, single-entry
string-keyed maps, fixed tuples, slab `[]*T`, nested slices, pointers,
time, base64 bytes, `any`, `json.RawMessage`. Core-pinned per the discipline
below: `GOMAXPROCS=1 taskset -c 24 … -benchtime=500x -count=1 -cpu=1`.

### Unmarshal

| path       | ns/op       | B/op    | allocs    | MB/s    |
| ---------- | ----------- | ------- | --------- | ------- |
| jsonv2     | 24634 K     | 13.6 MB | 234397    | 189     |
| sonic      | 11872 K     | 16.6 MB | 127484    | 393     |
| sonic_fast | 11737 K     | 16.6 MB | 127484    | 397     |
| easyjson   | 18725 K     | 12.9 MB | 164283    | 249     |
| **ggen**   | **10065 K** | 8.4 MB  | **54990** | **463** |

A sixth `ggen_copy` row (CopyNode, `-copy` mode) isolates the copy-out cost vs
the aliasing `ggen` row: every retained string / map key+value / slice elem /
`json.RawMessage` / any-embedded string becomes its own heap alloc (allocs jump
to the same order as the stream path), so B/op and allocs rise while ns/op is
broadly comparable. Numbers omitted here — interleave a core-pinned benchstat
(per the discipline above) before quoting any.

### Marshal

| path              | ns/op      | B/op    | allocs | MB/s    |
| ----------------- | ---------- | ------- | ------ | ------- |
| jsonv2            | 10971 K    | 4.8 MB  | 7497   | 425     |
| sonic             | 8133 K     | 26.9 MB | 5107   | 573     |
| sonic_fast        | 7441 K     | 26.9 MB | 5106   | 626     |
| easyjson          | 7242 K     | 4.9 MB  | 6914   | 644     |
| **ggen**          | 5928 K     | 9.7 MB  | **1**  | 786     |
| **ggen_presized** | **4791 K** | **0 B** | **0**  | **973** |

`ggen_presized` = same `AppendJSON`, once-pre-sized buffer (0 allocs, 0 GC). The
1 alloc on plain `ggen` = output buffer.

### Reader input (streaming)

| path                         | ns/op   | B/op    | allocs |
| ---------------------------- | ------- | ------- | ------ |
| jsonv2.UnmarshalRead         | 25216 K | 13.6 MB | 234397 |
| sonic.NewDecoder             | 15829 K | 34.8 MB | 127506 |
| sonic_fast.NewDecoder        | 15675 K | 34.8 MB | 127506 |
| easyjson.UnmarshalFromReader | 20126 K | 23.3 MB | 164313 |
| **ggen UnmarshalStream**     | 14940 K | 13.0 MB | 169760 |
| **ggen ReadAllUnmarshal**    | 11853 K | 18.9 MB | 55018  |

ggen Stream copies each scanned string to its own heap alloc (hence the alloc
count); `ReadAllUnmarshal` is the cleanest io.Reader pattern (bytes-path shape,
one `io.ReadAll` buffer).

### `B/op` notes

- **Marshal `ggen`:** B/op ≈ output buffer (the single alloc `JSONSize()` sizes
  — per map entry `4 + 2*len(k) + value-bound`, else flat 128 for nested/struct).
- **Marshal `ggen_presized`:** caller-owned buffer + AppendAny concrete-type
  fast paths for every primitive shape (`[]any`/`[]string`/`[]int*`/
  `[]uint16/32/64`/`[]float*`/`[]bool`, `map[string]any/string/int*/uint*/
  float*/bool` — bypass reflect.MapIter boxing) → 0 allocs, 0 GC.
- **Unmarshal:** ggen B/op < easyjson, and allocs ~3.8× below easyjson — the
  `unsafe.String` aliases avoid copying every string out of the input.

### SIMD tier (`ggen -simd`, bytes path)

Generated-code A/B, both binaries `GOEXPERIMENT=jsonv2,simd` (same toolchain —
the experiment alone shifts codegen, so a scalar binary built WITHOUT it is a
confounded baseline), differing only in the generated file (scalar vs
`-simd=avx512`). Interleaved core-24 pinned runs (10× each side), benchstat
n=10, 2026-07:

| bench                 | scalar   | avx512   | delta        |
| --------------------- | -------- | -------- | ------------ |
| NoAlloc_Unmarshal/ggen| 1311 n   | 758 n    | −42.2%       |
| Small_Unmarshal/ggen  | 878 n    | 122 n    | −86.1%       |
| Mega_Unmarshal/ggen   | 14.49 m  | 13.27 m  | −8.4%        |
| Tiny_Unmarshal/ggen   | 62.95 n  | 62.81 n  | flat         |
| Mega_Marshal/ggen     | 10.12 m  | 10.45 m  | +3.2% (±7%)  |
| Tiny_Marshal/ggen     | 93.5 n   | 93.5 n   | flat         |

B/op + allocs identical to scalar in all rows. Small (~2.9 KiB, one 2800 B Bio
string) rides the fused tier scan at 22 GiB/s; NoAlloc stacks the inline
vector key/value classify (opt #46), the exact-short float fast path, and the
scanner instruction shaves. Tiny is flat because all-short-key structs emit a
bounded scalar key window instead of the vector classify — the old ~+7%
opt-in floor is gone. Mega_Marshal's +3.2% sits inside its ±7% run noise (a
direct controlled A/B of the gated encode tier alone measured flat, p=0.39);
the encode tiers are length-gated so sub-lane strings take the scalar walk —
their win only shows on ≥64 B strings (`BenchmarkEscapeScan` in encode/:
3.6× at 64 B, ~10× at 256 B+), which no repo marshal bench carries.

Per-change stacked deltas at the avx512 tier (each an interleaved n=10
benchstat vs the previous step): exact-short float −7.6% NoAlloc; scanner
shaves −2.9% NoAlloc; inline vector scan −22% NoAlloc / −8.8% Mega / −8%
Tiny.

**Stream tier** (fused per-window locate in `Stream.String*`/`StringView*`/
`KeyView*`, see scan/CLAUDE.md) — interleaved n=10 at avx512 vs the bytes-only
tier build: Mega_Reader/ggen_stream −5.2%, NoAlloc_Reader/ggen_stream −7.7%,
Small_Reader/ggen_stream_512 −19.8%, _full −26.5%. The remaining stream gap
vs bytes is string-copy mallocs + ReadMore, not scan work.

**Skip tier** (`SkipHeavy_Unmarshal`, `bench/skip_test.go`: SkipEnvelope with
`ignoreunknown` vs a Mega-sized blob under an unknown key; `compact` +
`pretty` = json.Indent 2-space) — scalar vs avx512, interleaved n=10:
compact −21.6%, pretty −29.9%; Mega_Unmarshal gained a further −8.3% (RawJSON
capture). The emitted `inlineSkipWS` tier handoff alone is −3.3% on a pretty
full Mega decode and ~+1% on Tiny (accepted). Codec context (verified
empirically 2026-07): sonic skips via a stored structural bitmap (6-21 MB
B/op per call); sonic_fast does NOT grammar-validate skipped content
(missing colons/commas, `truu`, bad escapes, ctrl bytes all pass) and
ConfigDefault still passes bad escapes + ctrl in skipped strings — at the
grammar-checking level (`sonic` row) ggen matches/beats it at 0 allocs.
A same-semantics on-the-fly block-skip (rolling quote parity + depth
counter, no stored bitmap) could reach sonic_fast throughput but drops
grammar validation of skipped spans — rejected as default, possible opt-in
(`fastskip`) if ever wanted.

**Stream skip tier + skip-tree compaction** (SkipHeavy `ggen_stream` row:
fresh 4 KiB buffer, the blob streams through refill + compaction) —
interleaved n=10 at avx512, tier + compacting refills vs scalar grow-only:
compact −32.6% (8.92 → 6.01 ms), pretty −25.6% (13.81 → 10.27 ms), B/op
8.4 MB → 127 KB (grow-only refills doubled the buffer every mid-number
window edge — see scan/CLAUDE.md), allocs 12 → 6; Mega_Reader flat
(string-copy mallocs dominate). The pretty stream row also flushed out a
scalar `(*Stream).skipObject` bug — no WS skip after the key-separator
comma — fixed + pinned by `TestStreamSkipValue_MatchesBytes`.

**avx2 vs avx512** (full tier set, interleaved n=10): avx512 geomean −5% —
Small −23.3%, NoAlloc −4.2%, Tiny −1%, Mega flat; avx2 wins
SkipHeavy/pretty by 5.75% (skip work is short-span-dominated, where Zen5's
double-pumped 512-bit ops cost 2× µops for no coverage gain). Recommend
avx512 by default, avx2 for skip-dominated workloads.

### Scalar-path optimizations (2026-07, interleaved n=6-8 core-24)

- **Scalar string window (cli/CLAUDE.md opt #47).** Default (non-`-simd`) build:
  Small_Unmarshal/ggen −50.9%, ggen_copy −44.8%; NoAlloc_Unmarshal/ggen −19.0%,
  ggen_copy −9.5%; Tiny_Unmarshal, Mega_Unmarshal, MapHeavy, ValidationHeavy all
  flat (Mega ggen p=0.065, ggen_copy p=0.093 — not significant). Closes most of
  the scalar↔avx512 gap for users who can't enable `GOEXPERIMENT=simd`.
- **SkipSpace* inlinable shell (scan/CLAUDE.md).** avx512 SkipHeavy/compact/ggen
  −9.7% (compact is whitespace-free, so this is pure call-overhead removal);
  B/op + allocs unchanged.
- **Stream `Float64`/`Number` compacting refill + `exactShort`.** Throughput
  within noise on the repo Reader benches (float fields are full-entropy, so
  `exactShort` rarely fires and the small payloads don't balloon), but a residency
  fix: a 50k-short-float stream through a 64 B buffer stayed at cap 64 vs
  ballooning to 1 MB before (`TestFloatNumberBufBounded`).

Two audit candidates were implemented + interleaved-A/B'd and REJECTED — see
`.claude/backlog.md` Tried Rejected: SWAR encode escape walk (micro −33% but
Mega_Marshal +3.4% in-situ) and window-gated inline stream int loops (no win,
NoAlloc_Reader +4.75%, memory-bound tier).

## Running benchmarks

**THE canonical invocation is ALWAYS `-benchtime=500x -count=1 -cpu=1`.** Not
`-count=5`, not `-count=8`, not `-count=12` — `-count=1`. One pass, fixed
iteration count, single-threaded. That is the run for every number quoted in any
doc, the README, or a chat reply. Anything else wastes minutes and is not what
this repo expects.

**Noise floor (measured 2026-07 on a non-busy box).** A core-pinned Mega
`-benchtime=500x -count=10` pass on an otherwise-idle machine spreads ~1%
min→max on the ggen rows (Unmarshal 0.8%, Marshal 0.8%, stream/readall ~1%);
reflection/codec rows sit at 1.5-3%, with Reader/sonic worst at ~4.4% (thermal
creep across the 20-minute sustained run — long hot passes also read ~2-5%
slower than a short count=1 pass). B/op and allocs/op repeat bit-identical
every sample — the payloads are deterministic, so any alloc delta is a real
code change, and ns/op deltas under ~1% are noise even on an idle box; expect
worse on a loaded one.

**NEVER tail, head, grep, truncate, sample, or otherwise reduce the output IN
ANY SHAPE OR FORM.** Print every benchmark line in full, verbatim. Do not pipe
through `tail`/`head`/`grep`/`sed`/`awk` to "trim" rows, do not show "the
relevant ones" — show ALL of them. A truncated benchmark result is worse than
none: it hides regressions in the rows you dropped. If the output is long, it is
long; emit it whole.

**ALWAYS check the machine's power profile, warm up, and use a CONTROL ROW.**
Learned the hard way (2026-07): under a capped power profile (`scaling_max_freq`
pinned to 3 GHz with a 2 GHz floor, core idling at 625 MHz) the FIRST binary
measured eats the frequency ramp and reads up to **50% slow** — which silently
inverts A/B results. DeepNested/ggen swung 16.8 → 24.6 µs on an UNCHANGED
binary; a depth-cap "+10.3% avx512 regression" and a number-grammar "+25%
regression" were both pure artifact, and one even got a plausible-sounding
mechanistic explanation written for it before re-measurement killed it.

Protocol, in order:

1. `cat /sys/firmware/acpi/platform_profile` and
   `/sys/devices/system/cpu/cpu24/cpufreq/{scaling_governor,scaling_max_freq}` —
   want `performance` and the full boost ceiling (~5187 MHz here, NOT 3000).
2. Run one throwaway pass and DISCARD it, so no side eats the ramp.
3. Include an **untouched third-party row as an in-run control** — the
   `jsonv2` row of the same bench family is ideal: ggen changes can't affect it,
   so if it differs between the two binaries, the comparison is INVALID and the
   ggen delta means nothing. Every A/B below is control-checked this way.
   (`-bench='DeepNested_Unmarshal/(jsonv2|ggen)$'` gets both rows.)

Only after the control matches is a delta real.

### Control-checked sweep, 2026-07 (pre-UTF8 `03c6503` → `ca1c7d9`)

Cumulative cost of the three jsonv2-parity fixes (UTF-8 validation #50,
recursion depth cap #51, number grammar #52), each family tagged with its own
worst control drift. **A family whose control drifts >3% cannot be measured on
this box** — quote nothing from it.

| family | scalar | avx512 | control | verdict |
| ------ | ------ | ------ | ------- | ------- |
| Mega_Unmarshal   | +4.8%* | +1.8%  | 1.9% (avx) | flat at avx512 |
| Mega_Reader      | +1.4…2.3% | +0.9…1.9% | 0.9-1.6% | flat |
| SkipHeavy (×4)   | −0.2…+3.3% | −1.7…+0.6% | 1.4-2.8% | flat |
| MapHeavy         | −2.4%  | −3.1%* | 0.4% (sca) | flat/better |
| **EscapeHeavy**  | +11.2%* | **+12.7%** | **0.5% (avx)** | **real regression** |
| NoAlloc          | +67.7%* | +38.7%* | 3.6-13.6% | real (effect ≫ drift) |
| RuneGated        | +54.4% | +9.5%  | NO CONTROL ROW | directional only |
| Small/Tiny/DeepNested/ValidationHeavy | — | — | 4-27% | **unmeasurable** |

`*` = that tier's control drifted >3%; the other tier's figure is the reliable one.

EscapeHeavy is the one genuine regression: the escape path gained a
`utf8.Valid` over assembled output + surrogate rejection, and its payload is
~12% escapes with surrogate pairs. NoAlloc/RuneGated pay the UTF-8 walk on
Cyrillic strings by design (the vector validator is why avx512 ≪ scalar there).

**ALWAYS pin to a dedicated core and disable parallelism** — every perf claim
must come from `GOMAXPROCS=1 taskset -c 24 … -cpu=1`. The default multi-core run
is layout/scheduler-noise-dominated (sub-1% deltas flip sign). Use **core 24**.
The env vars (`GOEXPERIMENT`, `GOMAXPROCS`) must precede `taskset` — `taskset`
treats the first non-flag token as the command, so `taskset … GOEXPERIMENT=… go`
tries to exec the env assignment.

```sh
(cd bench && GOEXPERIMENT=jsonv2 GOMAXPROCS=1 taskset -c 24 go test -run=^$ -bench=. -benchtime=500x -count=1 -cpu=1 .)
```

`-benchtime=500x` fixes the iteration count so rows are directly comparable.
`-count=1` is the rule, not a starting point. For an A/B, build both sides as
test binaries (`go test -c -o old.test` / `new.test`) and run each side ONCE
under the same pin — **NEVER loop alternating old/new pairs** (no
`for i in …; do old; new; done` marathons; they waste minutes for numbers that
are a compass either way). One pass per side, full output, compare directly.
`./...` from root does NOT cross module boundaries — `cd` into `bench/` first.
