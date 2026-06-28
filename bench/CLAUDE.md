# bench — perf benchmarks vs jsonv2 / sonic / easyjson

## Files

- `bench/types.go` — ggen-annotated `Node` + easyjson-annotated `NodePlain` /
  `AddrPlain` (see "easyjson method leakage"); plus the `Account` family
  (`AccountValue`/`AccountPayload`) + easyjson-only `Easy*` mirror. Untagged, so
  ggen methods land in `bench_ggen.go`.
- `bench/bench_ggen.go` — generated ggen methods. Regen: `(cd bench &&
  GOEXPERIMENT=jsonv2 ../ggen ./...)`.
- `bench/types_easyjson.go` — generated easyjson methods. Regen: `easyjson
  bench/types.go`.
- `bench/mega_test.go` — 4-way Mega benches (jsonv2/sonic/easyjson/ggen) for
  Unmarshal/Marshal/Reader. Hosts `BenchmarkRetention`.
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
- **Retention** — each goroutine holds produced `*Node` in a local sink; sinks
  merge after `b.RunParallel`; GC ×2; `runtime.MemStats.HeapInuse` delta / `b.N`
  = `retain_KB/op` (process-global, parallel-safe). Run with `-benchtime=1000x`.

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
time, base64 bytes, `any`, `json.RawMessage`. Reference only (32 threads, no cpu
limit), not the core-pinned discipline below.

### Unmarshal

| path       | ns/op      | B/op    | allocs     | MB/s     |
| ---------- | ---------- | ------- | ---------- | -------- |
| jsonv2     | 4042 K     | 17.7 MB | 316832     | 1451     |
| sonic      | 2038 K     | 20.8 MB | 137770     | 2878     |
| sonic_fast | 1971 K     | 20.8 MB | 137770     | 2976     |
| easyjson   | 3158 K     | 17.0 MB | 245856     | 1857     |
| **ggen**   | **2245 K** | 14.4 MB | **101927** | **2612** |

### Marshal

| path              | ns/op     | B/op    | allocs | MB/s      |
| ----------------- | --------- | ------- | ------ | --------- |
| jsonv2            | 1286 K    | 6.7 MB  | 7409   | 4559      |
| sonic             | 989 K     | 33.6 MB | 5116   | 5927      |
| sonic_fast        | 952 K     | 33.6 MB | 5113   | 6161      |
| easyjson          | 962 K     | 6.2 MB  | 7597   | 6095      |
| **ggen**          | 655 K     | 11.8 MB | **2**  | 8951      |
| **ggen_presized** | **564 K** | **1 B** | **0**  | **10393** |

`ggen_presized` = same `AppendJSON`, once-pre-sized buffer (0 allocs, 0 GC). The
2 allocs on plain `ggen` = output buffer + 1 misc.

### Reader input (streaming)

| path                         | ns/op  | B/op    | allocs |
| ---------------------------- | ------ | ------- | ------ |
| jsonv2.UnmarshalRead         | 4237 K | 17.7 MB | 316834 |
| sonic.NewDecoder             | 2358 K | 39.0 MB | 137791 |
| sonic_fast.NewDecoder        | 2311 K | 39.0 MB | 137790 |
| easyjson.UnmarshalFromReader | 3204 K | 31.5 MB | 245886 |
| **ggen UnmarshalStream**     | 2900 K | 17.7 MB | 256587 |
| **ggen ReadAllUnmarshal**    | 2274 K | 29.0 MB | 101956 |

ggen Stream copies each scanned string to its own heap alloc (hence the alloc
count); `ReadAllUnmarshal` is the cleanest io.Reader pattern (bytes-path shape,
one `io.ReadAll` buffer).

### Residency (retained heap per decoded item, slowPayload ~36 KiB)

| codec           | per-item  | factor over JSON payload |
| --------------- | --------- | ------------------------ |
| **ggen_bytes**  | 66.1 KiB  | 1.89× (lowest)           |
| easyjson        | 78.3 KiB  | 2.23×                    |
| stdjson         | 79.5 KiB  | 2.27×                    |
| **ggen_stream** | 87.0 KiB  | 2.48×                    |
| ggen_readall    | 107.1 KiB | 3.05×                    |
| sonic           | 111.3 KiB | 3.17×                    |
| sonic_fast      | 112.0 KiB | 3.19×                    |

Run `GGEN_BENCH_TOPALLOCS=1` to surface the top-5 allocation sites.

### `B/op` notes

- **Marshal `ggen`:** B/op ≈ output buffer (the alloc `JSONSize()` sizes — per
  map entry `4 + 2*len(k) + value-bound`, else flat 128 for nested/struct) +
  1 misc.
- **Marshal `ggen_presized`:** caller-owned buffer + AppendAny concrete-type
  fast paths for every primitive shape (`[]any`/`[]string`/`[]int*`/
  `[]uint16/32/64`/`[]float*`/`[]bool`, `map[string]any/string/int*/uint*/
  float*/bool` — bypass reflect.MapIter boxing) → 0 allocs, 0 GC.
- **Unmarshal:** ggen B/op > easyjson because `unsafe.String` aliases keep the
  whole input buffer alive (counted live per iteration); allocs still ~3.4×
  below easyjson.

## Running benchmarks

**ALWAYS pin to a dedicated core and disable parallelism** — every perf claim
must come from `GOMAXPROCS=1 taskset -c 25 … -cpu=1`. The default multi-core run
is layout/scheduler-noise-dominated (sub-1% deltas flip sign). Use **core 25**.

```sh
(cd bench && GOMAXPROCS=1 taskset -c 25 GOEXPERIMENT=jsonv2 go test -run=^$ -bench=. -cpu=1 -count=12 ./...)
# fixed iter count for comparable retention numbers:
(cd bench && GOMAXPROCS=1 taskset -c 25 GOEXPERIMENT=jsonv2 go test -run=^$ -bench=Retention -benchtime=1000x -cpu=1 ./...)
```

For an A/B, build both as test binaries and interleave under the same pin
(`go test -c -o old.test` / `new.test`, alternate runs), then `benchstat` over
`-count=12`+ samples — never a single default-layout A/B. `./...` from root does
NOT cross module boundaries — `cd` into `bench/` first.
