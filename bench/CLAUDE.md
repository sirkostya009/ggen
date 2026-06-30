# bench — perf benchmarks vs jsonv2 / sonic / easyjson

## Files

- `bench/types.go` — ggen-annotated `Node` + easyjson-annotated `NodePlain` /
  `AddrPlain` (see "easyjson method leakage"); plus the `Account` family
  (`AccountValue`/`AccountPayload`) + easyjson-only `Easy*` mirror; plus
  `CopyNode`/`CopyAddr` — wire-identical mirrors of `Node`/`Addr` carrying
  `//ggen:generate copy`, so the `ggen_copy` Unmarshal row decodes the same
  `MegaPayload` through the copy-mode bytes path. Untagged, so ggen methods land
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
