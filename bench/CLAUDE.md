# bench — perf benchmarks vs jsonv2 / sonic / easyjson

## Files

- `bench/types.go` — ggen-annotated `Node` + easyjson-annotated `NodePlain` /
  `AddrPlain` (see "easyjson method leakage"); plus the `Account` family
  (`AccountValue`/`AccountPayload`) + easyjson-only `Easy*` mirror; plus
  `CopyNode`/`CopyAddr` — wire-identical mirrors of `Node`/`Addr` carrying
  `//ggen:generate copy`, so the `ggen_copy` Unmarshal row decodes the same
  `MegaPayload` through the copy-mode bytes path (same pattern:
  `CopyAccount` family for NoAlloc, `CopyValidated` for Small, `CopyClaim`
  for Tiny — every decode bench family carries a `ggen_copy` row). Untagged, so ggen methods land
  in `bench_ggen.go`.
- `bench/bench_ggen.go` — generated ggen methods. Regen: `(cd bench &&
  GOEXPERIMENT=jsonv2 ../ggen ./...)`.
- `bench/types_easyjson.go` — generated easyjson methods. Regen: `easyjson
  bench/types.go`.
- `bench/mega_test.go` — 4-way Mega benches (jsonv2/sonic/easyjson/ggen) for
  Unmarshal/Marshal/Reader.
- `bench/small_test.go` — small-value (~2.9 KiB ValidPayload) Unmarshal + Reader.
- `bench/slowstream_test.go` — slow-reader benchmarks.
- `bench/simple_test.go` — `BenchmarkNoAlloc_Unmarshal` + `_Reader`.
- `bench/skip_test.go` — `BenchmarkSkipHeavy_Unmarshal` (compact/pretty
  envelope, ~100% skipped content via ignoreunknown).

## What each bench family measures

- **NoAlloc** (`Account`, wide denormalized record, all nested VALUE structs, no
  slice/map/pointer/`any`/`json.RawMessage`) — bytes decode makes zero allocs
  (strings alias input, structs decode in place, scalars land in receiver),
  vs easyjson's ~25 allocs (it copies strings out of the input); isolates scan +
  key-dispatch + per-field-assign. Warms up 64 iters. `_Reader`:
  `ggen_stream` starts each decode with a FRESH 512-byte buffer (< payload) so
  the stream genuinely refills + compacts mid-decode (a payload-sized/grown
  buffer reads in one shot, degenerating into the bytes path) — copies strings
  out of the buffer, so NOT zero-alloc.
- **Mega** (`_Unmarshal`/`_Marshal`/`_Reader`, ~5.6 MiB deep `Node` tree).
  `_Reader` includes `ggen_ReadAllUnmarshal` (`io.ReadAll` then bytes decode —
  cheapest io.Reader pattern). Inner loop under `b.RunParallel` (`-cpu=1`
  serial, `-cpu=N` N-way, same path); stateful codecs get per-goroutine state
  via the `setup` closure in `runBench`. Reports `gc` (NumGC delta) alongside
  ns/op·B/op·allocs/op.
- **Small** (~2.9 KiB) — per-call buffer/streaming overhead is visible. Two
  ggen-stream Reader rows (512-byte vs payload-sized buf) isolate the
  buffer-grow chain from steady-state throughput.
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
field types meant `type AddrPlain Addr` wasn't enough — see `bench/types.go`);
for non-recursive structs a parallel struct declaration is cleanest.

**Symptom when forgotten:** a supposedly-reflection row matches easyjson's allocs
and ns/op almost exactly when it should be 3-10× slower. ggen's own
`AppendJSON`/`DecodeFrom` do NOT trip this (not `json.Marshaler`/`Unmarshaler`);
only stdlib-interface methods cause cross-codec pickup — same isolation applies
if a struct opts into ggen `marshal`/`unmarshal` hooks.

## Headline results (~5.6 MiB deep Node tree, full validation)

AMD Ryzen AI MAX+ 395, Go 1.26, GOEXPERIMENT=jsonv2. Node carries scalars,
slices, string-keyed maps, fixed tuples, slab `[]*T`, nested slices, pointers,
time, base64 bytes, `any`, `json.RawMessage`. Core-pinned per the discipline
below: `GOMAXPROCS=1 taskset -c 24 … -benchtime=500x -count=1 -cpu=1`.

### Unmarshal

| path       | ns/op       | B/op    | allocs    | MB/s    |
| ---------- | ----------- | ------- | --------- | ------- |
| jsonv2     | 34928 K     | 17.7 MB | 316830    | 168     |
| sonic      | 17198 K     | 20.8 MB | 137770    | 341     |
| sonic_fast | 16934 K     | 20.8 MB | 137770    | 346     |
| easyjson   | 26178 K     | 17.0 MB | 245855    | 224     |
| **ggen**   | **14697 K** | 11.4 MB | **64599** | **399** |

A sixth `ggen_copy` row (CopyNode, `-copy` mode) isolates the copy-out cost vs
the aliasing `ggen` row: every retained string / map key+value / slice elem /
`json.RawMessage` / any-embedded string becomes its own heap alloc (allocs jump
to the same order as the stream path), so B/op and allocs rise while ns/op is
broadly comparable. Numbers omitted here — interleave a core-pinned benchstat
(per the discipline above) before quoting any.

### Marshal

| path              | ns/op      | B/op    | allocs | MB/s    |
| ----------------- | ---------- | ------- | ------ | ------- |
| jsonv2            | 14896 K    | 6.0 MB  | 7407   | 393     |
| sonic             | 12353 K    | 33.6 MB | 5112   | 475     |
| sonic_fast        | 11882 K    | 33.6 MB | 5111   | 494     |
| easyjson          | 10773 K    | 6.1 MB  | 7586   | 544     |
| **ggen**          | 9623 K     | 11.9 MB | **1**  | 609     |
| **ggen_presized** | **6598 K** | **0 B** | **0**  | **889** |

`ggen_presized` = same `AppendJSON`, once-pre-sized buffer (0 allocs, 0 GC). The
1 alloc on plain `ggen` = output buffer.

### Reader input (streaming)

| path                         | ns/op   | B/op    | allocs |
| ---------------------------- | ------- | ------- | ------ |
| jsonv2.UnmarshalRead         | 36588 K | 17.7 MB | 316830 |
| sonic.NewDecoder             | 20779 K | 39.0 MB | 137793 |
| sonic_fast.NewDecoder        | 20126 K | 39.0 MB | 137794 |
| easyjson.UnmarshalFromReader | 30042 K | 31.5 MB | 245886 |
| **ggen UnmarshalStream**     | 22262 K | 17.1 MB | 251735 |
| **ggen ReadAllUnmarshal**    | 17686 K | 25.9 MB | 64628  |

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

**NEVER tail, head, grep, truncate, sample, or otherwise reduce the output IN
ANY SHAPE OR FORM.** Print every benchmark line in full, verbatim. Do not pipe
through `tail`/`head`/`grep`/`sed`/`awk` to "trim" rows, do not show "the
relevant ones" — show ALL of them. A truncated benchmark result is worse than
none: it hides regressions in the rows you dropped. If the output is long, it is
long; emit it whole.

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
`-count=1` is the rule, not a starting point. ONLY when explicitly asked for a
rigorous A/B do you build both sides as test binaries and interleave under the
same pin (`go test -c -o old.test` / `new.test`, alternate runs) + `benchstat`
over more samples — and even then you emit the full output, never truncated.
`./...` from root does NOT cross module boundaries — `cd` into `bench/` first.
