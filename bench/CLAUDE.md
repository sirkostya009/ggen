# bench — perf benchmarks vs jsonv2 / sonic / easyjson

## Files

- `bench/types.go` — ggen-annotated `Node` + easyjson-annotated `NodePlain` /
  `AddrPlain` (see "easyjson method leakage").
- `bench/bench_ggen.go` — generated ggen methods. Regen:
  `(cd bench && GOEXPERIMENT=jsonv2 ../ggen ./...)`.
- `bench/types_easyjson.go` — generated easyjson methods. Regen:
  `easyjson bench/types.go`.
- `bench/mega_test.go` — 4-way Mega benches (jsonv2/sonic/easyjson/ggen) for
  Unmarshal, Marshal, Reader. Hosts `BenchmarkRetention`.
- `bench/small_test.go` — small-value (~2.9 KiB ValidPayload) Unmarshal + Reader.
- `bench/slowstream_test.go` — slow-reader benchmarks.

## BenchmarkMega

Three table-driven benches: `BenchmarkMega_Unmarshal`, `BenchmarkMega_Marshal`,
`BenchmarkMega_Reader` (includes `ggen_ReadAllUnmarshal` — `io.ReadAll` then
bytes-path decode, cheapest "I have an io.Reader" pattern).

Inner loop runs under `b.RunParallel` so `-cpu=1` is serial and `-cpu=N` is
N-way parallel on same code path. Stateful codecs (Reader, Stream buf) get
per-goroutine state via a `setup` closure in `runBench`. Each sub-bench wraps
`runtime.ReadMemStats` and reports `heap_KB` (live heap at StopTimer),
`total_KB` (alloc delta over the timed region), `gc` (NumGC delta), `gc/op`
(per-iter GC rate) on top of standard `ns/op` + `B/op` + `allocs/op`.

## BenchmarkSmall

At ~2.9 KiB the decoded value is small enough that per-call buffer
management/streaming overhead is visible rather than drowned by tree-walk cost.
Two ggen-stream Reader rows (512-byte initial buf vs payload-sized buf) isolate
the buffer-grow chain from steady-state throughput.

## BenchmarkSlowStream

Slow-reader benches (`slowReader`, geometric-decay delays). Two tables:
`BenchmarkSlowStream_Valid` (stdjson, easyjson, ggen_stream, ggen_readall) and
`BenchmarkSlowStream_Invalid` (ggen_stream, ggen_readall, jsonv2-baseline on a
payload that **fails ggen validation early**). Same `runBench` harness, so
`-cpu=N` scales near-linearly (concurrent slow connections overlap their
sleeps). The **Invalid** group is where streaming pays off: fail-fast bails as
soon as the bad field is seen, ReadAll must drain the body first — ~67 ms
(stream) vs ~78 ms (readall) on the malformed payload.

## BenchmarkRetention

Each goroutine holds its produced `*Node` in a local sink;
sinks merge after `b.RunParallel`; GC ×2; snapshot
`runtime.MemStats.HeapInuse` delta / `b.N` = `retain_KB/op`. `HeapInuse` is
process-global so works in parallel. Best run with a fixed iter count
(`-benchtime=1000x`) for comparable numbers.

## easyjson method leakage

`//easyjson:json` generates `MarshalJSON`/`UnmarshalJSON` on the target type.
The stdlib reflection codecs (`jsonv2`, `encoding/json`) AND **sonic** all check
`json.Marshaler`/`json.Unmarshaler` before reflecting — so any type carrying
easyjson methods silently routes every "reflection" codec through easyjson's
fast path, and the row labelled `jsonv2`/`sonic` ends up measuring easyjson.

**Pattern:** keep ggen and easyjson on SEPARATE types sharing the wire shape.
Feed the "Plain" (ggen-only) struct to reflection codecs, the "Easy" struct to
the easyjson row.

```go
//ggen:generate
type Claim struct { Sub string `json:"sub"`; ... }

//easyjson:json
type EasyClaim struct { Sub string `json:"sub"`; ... }   // same fields
```

`NodePlain`/`AddrPlain` exist for the same reason at the mega level
(self-referential field types meant `type AddrPlain Addr` wasn't enough — see
`bench/types.go`). For non-recursive structs a parallel struct declaration is
cleanest.

**Symptom when forgotten**: the supposedly-reflection row matches easyjson's
allocs and ns/op almost exactly when it should be 3-10× slower. ggen's own
`AppendJSON`/`DecodeFrom` do NOT trip this — they're not `json.Marshaler`/
`Unmarshaler`. Only stdlib-interface methods cause cross-codec pickup; if a
struct opts into ggen's `marshal`/`unmarshal` hooks, the same isolation applies.

## Benchmarks (~5.6 MiB deep Node tree, full validation)

AMD Ryzen AI MAX+ 395, Go 1.26, GOEXPERIMENT=jsonv2. Node carries scalars,
slices, string-keyed maps, fixed-length tuples, slices of pointers
(slab path), nested slices, pointer fields, time, bytes (base64), `any`,
and `json.RawMessage`.

Numbers after running all, no cpu limit (32 threads):

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

`ggen_presized` is the same `AppendJSON` codepath with a once pre-sized buffer
— zero allocs, zero GC, ~14% faster than `encode.Marshal(v)`. The 2 allocs on
plain `ggen` are the per-call output buffer + 1 misc.
At Mega scale this beats sonic_fast ~1.7× on wall clock and ~33000× on allocated bytes.

### Reader input (streaming)

| path                         | ns/op  | B/op    | allocs |
| ---------------------------- | ------ | ------- | ------ |
| jsonv2.UnmarshalRead         | 4237 K | 17.7 MB | 316834 |
| sonic.NewDecoder             | 2358 K | 39.0 MB | 137791 |
| sonic_fast.NewDecoder        | 2311 K | 39.0 MB | 137790 |
| easyjson.UnmarshalFromReader | 3204 K | 31.5 MB | 245886 |
| **ggen UnmarshalStream**     | 8094 K | 17.8 MB | 256589 |
| **ggen ReadAllUnmarshal**    | 2274 K | 29.0 MB | 101956 |

ggen Stream copies strings during parse (each scanned string is its own heap
alloc), which is why it loses on alloc count. The win returns on **Marshal**
(1.47× faster than easyjson) and the **bytes-only path** (1.45× faster). The
cleanest "I have an io.Reader" pattern is `ReadAllUnmarshal` — same shape as the
bytes path, comparable wall clock at the cost of one `io.ReadAll` buffer.

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

Run with `GGEN_BENCH_TOPALLOCS=1` to surface top-5 allocation sites.

### `B/op` notes

- **Marshal (`ggen`):** B/op ≈ output buffer size (~11.8 MB = the marshalled
  wire bytes). Only 2 allocs/op (output buffer + 1 misc); `JSONSize()` sizes
  that one allocation (per map entry `4 + 2*len(k) + value-bound`, or flat 128
  for nested/struct). Down from a flat `128 * len` (~2.4× overshoot
  pre-tighten). For zero-alloc see `ggen_presized`.
- **Marshal (`ggen_presized`):** caller-owned buffer + AppendAny concrete-type
  fast paths for every primitive shape (`[]any`/`[]string`/`[]int*`/`[]uint16/
32/64`/`[]float*`/`[]bool`, `map[string]any/string/int*/uint*/float*/bool` —
  bypass reflect.MapIter boxing) → zero allocations, zero GC.
- **Unmarshal:** ggen reports higher B/op than easyjson (6.1 MB vs 3.3 MB for
  ~970 KB input) because `unsafe.String` aliases keep the entire input buffer
  alive (GC accounts it as a live allocation per iteration). Allocs still ~3.4×
  lower than easyjson (18 K vs 61 K).

## Running benchmarks

```sh
(cd bench && GOEXPERIMENT=jsonv2 go test -run=^$ -bench=. ./...)
# fixed iter count for comparable retention numbers:
(cd bench && GOEXPERIMENT=jsonv2 go test -run=^$ -bench=Retention -benchtime=1000x ./...)
```

`./...` from root does NOT cross module boundaries — `cd` into `bench/` first.
