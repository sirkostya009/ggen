# bench — perf benchmarks vs jsonv2 / sonic / easyjson

Separate Go module (its own `go.mod` with
`replace github.com/sirkostya009/ggen => ../`). Holds all
performance benchmarks and the reference-codec dep set
(sonic, easyjson, bytedance/gopkg, cloudwego/base64x,
klauspost/cpuid, golang/asm, …).

## Files

- `bench/types.go` — ggen-annotated `Node` + easyjson-annotated
  `NodePlain` / `AddrPlain` (see "easyjson method leakage" below
  for why parallel types).
- `bench/bench_ggen.go` — generated ggen methods for the types in
  `types.go`. Regen with
  `(cd bench && GOEXPERIMENT=jsonv2 ../ggen ./...)`.
- `bench/types_easyjson.go` — generated easyjson methods. Regen
  with `easyjson bench/types.go`.
- `bench/mega_test.go` — 4-way Mega benchmarks (jsonv2 / sonic /
  easyjson / ggen) for Unmarshal, Marshal, Reader. Hosts
  `BenchmarkRetention`.
- `bench/small_test.go` — small-value (~2.9 KiB ValidPayload)
  variants of Unmarshal + Reader.
- `bench/slowstream_test.go` — slow-reader benchmarks.

## Mega benchmark layout

Three table-driven benches:

- `BenchmarkMega_Unmarshal`
- `BenchmarkMega_Marshal`
- `BenchmarkMega_Reader` (includes `ggen_ReadAllUnmarshal` —
  `io.ReadAll` then bytes-path decode, the cheapest "I have an
  io.Reader" pattern)

Inner loop runs under `b.RunParallel`, so `-cpu=1` runs serial and
`-cpu=N` runs N-way parallel — same code path. Stateful codecs
(Reader, Stream buf) get per-goroutine state via a `setup` closure
in the `runBench` helper. Each sub-bench wraps
`runtime.ReadMemStats` and reports `heap_KB` (live heap at
StopTimer), `total_KB` (alloc delta over the timed region), `gc`
(NumGC delta), and `gc/op` (per-iter GC rate) on top of the
standard `ns/op` + `B/op` + `allocs/op`.

## Small benchmark layout

At ~2.9 KiB the decoded value is small enough that per-call buffer
management / streaming overhead is visible rather than drowned by
tree-walk cost. Two ggen-stream Reader rows (512-byte initial buf
vs payload-sized buf) isolate the buffer-grow chain from steady-
state throughput.

## SlowStream layout — where streaming actually pays off

Slow-reader benchmarks (`slowReader` with geometric-decay delays).
Two table-driven benches:

- `BenchmarkSlowStream_Valid` — stdjson, easyjson, ggen_stream,
  ggen_readall on a valid payload.
- `BenchmarkSlowStream_Invalid` — ggen_stream, ggen_readall,
  jsonv2-baseline on a payload that **fails ggen validation
  early**.

Same `runBench` harness as mega, so `-cpu=N` scales near-linearly
(concurrent slow connections overlap their sleeps — useful for
"N slow clients hitting one parser" sims).

The **Invalid** group is where streaming pays off: fail-fast bails
as soon as the bad field is seen, ReadAll has to drain the body
first. Measured ~67 ms (stream) vs ~78 ms (readall) on the
malformed payload.

## Retention bench (`BenchmarkRetention` in mega_test.go)

Replaces the old `TestResidency`. Each goroutine holds its
produced `*Node` values in a local sink; sinks merge after
`b.RunParallel`; GC × 2; snapshot `runtime.MemStats.HeapInuse`
delta divided by `b.N` gives `retain_KB/op`. `HeapInuse` is
process-global so the technique works in parallel. Best run with
a fixed iter count (`-benchtime=1000x`) for comparable per-codec
numbers.

## Cross-codec bench hygiene: easyjson method leakage

`//easyjson:json` generates `MarshalJSON` / `UnmarshalJSON` on the
target type. The standard library's reflection-based codecs
(`jsonv2`, stdlib `encoding/json`) and **sonic** ALL check the
`json.Marshaler` / `json.Unmarshaler` interfaces before falling
back to reflection — so any type carrying easyjson methods
silently routes every "reflection" codec through easyjson's
hand-rolled fast path. The bench row labelled `jsonv2` or `sonic`
ends up measuring easyjson, not the codec it claims to.

**Pattern:** keep ggen and easyjson on SEPARATE types that share
the wire shape. The bench feeds the "Plain" (ggen-only) struct to
the reflection codecs and the "Easy" struct to the easyjson row.

```go
//ggen:generate
type Claim struct { Sub string `json:"sub"`; ... }

//easyjson:json
type EasyClaim struct { Sub string `json:"sub"`; ... }   // same fields
```

`NodePlain` / `AddrPlain` exist for the same reason at the mega
level (self-referential field types meant the simpler
`type AddrPlain Addr` pattern wasn't enough — see existing
comments in `bench/types.go`). For non-recursive structs (Claim,
ValidationHeavy, HTMLPlain) a parallel struct declaration is the
cleanest approach.

**Symptom when this is forgotten**: the supposedly-reflection
bench row matches easyjson's allocs and ns/op almost exactly,
when it should be 3-10× slower. Anything similar in a new bench
→ check the type doesn't carry easyjson methods.

ggen's own `AppendJSON` / `DecodeFrom` methods do NOT trip the
same hazard — they're not `json.Marshaler` / `json.Unmarshaler`.
Only the stdlib-interface methods (which easyjson emits, and
which ggen's `//ggen:generate marshal` / `unmarshal` opt-ins also
emit) cause cross-codec pickup. If a struct opts into ggen's
marshal/unmarshal hooks, the same isolation pattern applies.

## Benchmarks (~5.6 MiB deep Node tree, full validation)

AMD Ryzen AI MAX+ 395 (mitigations off), Go 1.26, GOEXPERIMENT=jsonv2.
Bench harness uses `b.RunParallel`, default `-cpu=NumCPU` (32-thread
aggregate throughput); for single-thread numbers run with `-cpu=1`.
Node carries scalars, slices, string-keyed maps, fixed-length
tuples, slices of pointers (slab path), nested slices, pointer
fields, time, bytes (base64), `any`, and `json.RawMessage` — the
full breadth of real-world API response shapes.

### Unmarshal

| path       | ns/op       | B/op    | allocs     | MB/s     |
| ---------- | ----------- | ------- | ---------- | -------- |
| jsonv2     | 4042 K      | 17.7 MB | 316832     | 1451     |
| sonic      | 2038 K      | 20.8 MB | 137770     | 2878     |
| sonic_fast | 1971 K      | 20.8 MB | 137770     | 2976     |
| easyjson   | 3158 K      | 17.0 MB | 245856     | 1857     |
| **ggen**   | **2245 K**  | 14.4 MB | **101927** | **2612** |

### Marshal

| path              | ns/op      | B/op    | allocs   | MB/s      |
| ----------------- | ---------- | ------- | -------- | --------- |
| jsonv2            | 1286 K     | 6.7 MB  | 7409     | 4559      |
| sonic             | 989 K      | 33.6 MB | 5116     | 5927      |
| sonic_fast        | 952 K      | 33.6 MB | 5113     | 6161      |
| easyjson          | 962 K      | 6.2 MB  | 7597     | 6095      |
| **ggen**          | 655 K      | 11.8 MB | **2**    | 8951      |
| **ggen_presized** | **564 K**  | **1 B** | **0**    | **10393** |

`ggen_presized` is the same `AppendJSON` codepath but the caller
reuses a pre-sized buffer across calls (`make([]byte, 0,
v.JSONSize())` once outside the hot loop) — zero allocs, zero GC
pressure, ~14% faster than the convenience `encode.Marshal(v)`
path. The 2 allocs on the plain `ggen` row are the per-call output
buffer + 1 misc; everything else is appended in place. At Mega
scale (~5.6 MiB) this beats the nearest competitor (sonic_fast) by
~1.7× on wall clock and by ~33000× on allocated bytes.

### Reader input (streaming)

| path                         | ns/op  | B/op    | allocs |
| ---------------------------- | ------ | ------- | ------ |
| jsonv2.UnmarshalRead         | 4237 K | 17.7 MB | 316834 |
| sonic.NewDecoder             | 2358 K | 39.0 MB | 137791 |
| sonic_fast.NewDecoder        | 2311 K | 39.0 MB | 137790 |
| easyjson.UnmarshalFromReader | 3204 K | 31.5 MB | 245886 |
| **ggen UnmarshalStream**     | 8094 K | 17.8 MB | 256589 |
| **ggen ReadAllUnmarshal**    | 2274 K | 29.0 MB | 101956 |

ggen Stream copies strings during parse (each scanned string is
its own heap alloc), which is why it loses ground on alloc count.
The win returns on **Marshal** (1.47× faster than easyjson) and
the **bytes-only path** (1.45× faster than easyjson). The
cleanest "I have an io.Reader" pattern is `ReadAllUnmarshal` —
same shape as the bytes path, comparable wall clock at the cost
of one `io.ReadAll` buffer.

### Residency (retained heap per decoded item, slowPayload ~36 KiB)

| codec            | per-item   | factor over JSON payload |
| ---------------- | ---------- | ------------------------ |
| **ggen_bytes**   | 66.1 KiB   | 1.89× (lowest)           |
| easyjson         | 78.3 KiB   | 2.23×                    |
| stdjson          | 79.5 KiB   | 2.27×                    |
| **ggen_stream**  | 87.0 KiB   | 2.48×                    |
| ggen_readall     | 107.1 KiB  | 3.05×                    |
| sonic            | 111.3 KiB  | 3.17×                    |
| sonic_fast       | 112.0 KiB  | 3.19×                    |

Single biggest residency win was **dropping `maxlen=N` as a
prealloc hint** (cut bytes-path retention from 163 → 65 KiB/item).
See `.claude/backlog.md` for the full thread (arena codegen,
inline scratch buf, alias-mode + pool reuse — none of those moved
the residency needle, only the maxlen change did).

On the tiny complex payload (~440 bytes): Unmarshal ~415 ns,
2 allocs, ~1 GB/s — still the fastest.

### `B/op` notes

- **Marshal (`ggen` row):** B/op ≈ output buffer size (~11.8 MB =
  the marshalled wire bytes themselves). Only 2 allocs/op — the
  output buffer + 1 misc. The `JSONSize()` upper bound is what
  sizes that one allocation; per map entry costs `4 + 2*len(k) +
  value-bound`, or a flat 128-byte fallback for nested/struct
  values. Down from a flat `128 * len` (~2.4× overshoot pre-
  tighten). For the truly-zero-alloc shape see `ggen_presized`
  (4 B/op, 0 allocs).
- **Marshal (`ggen_presized` row):** caller-owned buffer + ggen's
  AppendAny concrete-type fast paths for every primitive shape
  (`[]any` / `[]string` / `[]int*` / `[]uint16/32/64` / `[]float*`
  / `[]bool`, plus `map[string]any/string/int*/uint*/float*/bool`
  — concrete-type cases that bypass reflect.MapIter boxing) →
  net zero allocations per marshal, zero GC pressure.
- **Unmarshal:** ggen reports higher B/op than easyjson (6.1 MB
  vs 3.3 MB for ~970 KB input) because `unsafe.String` aliases
  keep the entire input buffer alive — the GC accounts the input
  as a live allocation per iteration. Allocs are still ~3.4× lower
  than easyjson (18 K vs 61 K).

## Running benchmarks

```sh
(cd bench && GOEXPERIMENT=jsonv2 go test -run=^$ -bench=. ./...)
# fixed iter count for comparable retention numbers:
(cd bench && GOEXPERIMENT=jsonv2 go test -run=^$ -bench=Retention -benchtime=1000x ./...)
```

`./...` from the root does NOT cross module boundaries — `cd`
into `bench/` first.
