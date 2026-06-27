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
- `bench/simple_test.go` — `BenchmarkNoAlloc_Unmarshal` + `_Reader`. The
  `Account` family + `AccountValue`/`AccountPayload` + the easyjson-only
  `Easy*` mirror live in `types.go` (untagged, so ggen methods land in
  `bench_ggen.go`; `easyjson bench/types.go` regenerates `types_easyjson.go`).

## BenchmarkNoAlloc

`Account` (`types.go`) is a wide denormalized user record — profile + address +
employer + settings flattened into nested VALUE structs. Carries no
slice/map/pointer/`any`/`json.RawMessage` kind, so the bytes-path decode makes
**zero allocations** (strings alias the input — incl. the large multilingual
non-ASCII fields — nested structs decode in place, scalars land in the
receiver). Isolates the raw scan + key-dispatch + per-field-assign loop with the
allocator out of the picture — the complement to the alloc-heavy Mega tree. Both
benches warm up (64 iters) before timing.

- `_Unmarshal` — bytes path (jsonv2/sonic/sonic_fast/easyjson/ggen). ggen ~3×
  the throughput of sonic, ~4.5× jsonv2, ~3.5× easyjson here, at 0 B/op vs
  theirs. easyjson copies strings out of the input → ~25 allocs.
- `_Reader` — Reader path (jsonv2/sonic/easyjson/ggen_stream/ggen_readall).
  `ggen_stream` starts each decode with a FRESH 512-byte buffer (< payload) so
  the stream genuinely refills + compacts mid-decode — a payload-sized or
  carried-forward grown buffer reads everything in one shot and degenerates into
  the bytes path. It also copies strings out of the buffer, so it is NOT
  zero-alloc; still the fastest Reader row.

## BenchmarkMega

Three table-driven benches: `BenchmarkMega_Unmarshal`, `BenchmarkMega_Marshal`,
`BenchmarkMega_Reader` (includes `ggen_ReadAllUnmarshal` — `io.ReadAll` then
bytes-path decode, cheapest "I have an io.Reader" pattern).

Inner loop runs under `b.RunParallel`. `-cpu=1` serial, `-cpu=N` N-way parallel, same code path. Stateful codecs (Reader, Stream buf) get per-goroutine state via `setup` closure in `runBench`. Each sub-bench wraps `runtime.ReadMemStats`, reports `gc` (NumGC delta over timed region) on top of standard `ns/op` + `B/op` + `allocs/op`.

## BenchmarkSmall

At ~2.9 KiB decoded value small enough that per-call buffer management/streaming overhead visible, not drowned by tree-walk cost. Two ggen-stream Reader rows (512-byte initial buf vs payload-sized buf) isolate buffer-grow chain from steady-state throughput.

## BenchmarkValidationHeavy / BenchmarkRuneGated

`ValidationHeavy` (short fields) measures per-field validation cost vs codecs
that skip it (`ggen_validated` vs `ggen_noval` + non-validating jsonv2/sonic/
easyjson rows). `RuneGated` is the companion for **long** strings (~8 KB): a
single ggen row isolating rune-rule validation where opt #44's byte-length
gates skip the O(len) `utf8.RuneCountInString` walk — `LongRunes` (2048 genuine
4-byte runes) clears its `minrunes/maxrunes` bound via pure `len` gates,
`AsciiRunes` via tier-c (alphanum-precedes → count is `len`). The gates are flat
on `ValidationHeavy` (short — the skipped walk is cheap) but −46.8% here.

## BenchmarkSlowStream

Slow-reader benches (`slowReader`, geometric-decay delays). Two tables:
`BenchmarkSlowStream_Valid` (stdjson, easyjson, ggen_stream, ggen_readall) and
`BenchmarkSlowStream_Invalid` (ggen_stream, ggen_readall, jsonv2-baseline on payload that **fails ggen validation early**). Same `runBench` harness, so
`-cpu=N` scales near-linearly (concurrent slow connections overlap sleeps). **Invalid** group = where streaming pays off: fail-fast bails as soon as bad field seen, ReadAll must drain body first — ~67 ms (stream) vs ~78 ms (readall) on malformed payload.

## BenchmarkRetention

Each goroutine holds produced `*Node` in local sink; sinks merge after `b.RunParallel`; GC ×2; snapshot `runtime.MemStats.HeapInuse` delta / `b.N` = `retain_KB/op`. `HeapInuse` process-global, works in parallel. Best run with fixed iter count (`-benchtime=1000x`) for comparable numbers.

## easyjson method leakage

`//easyjson:json` generates `MarshalJSON`/`UnmarshalJSON` on target type. Stdlib reflection codecs (`jsonv2`, `encoding/json`) AND **sonic** all check `json.Marshaler`/`json.Unmarshaler` before reflecting — any type carrying easyjson methods silently routes every "reflection" codec through easyjson fast path, row labelled `jsonv2`/`sonic` ends up measuring easyjson.

**Pattern:** keep ggen and easyjson on SEPARATE types sharing wire shape. Feed "Plain" (ggen-only) struct to reflection codecs, "Easy" struct to easyjson row.

```go
//ggen:generate
type Claim struct { Sub string `json:"sub"`; ... }

//easyjson:json
type EasyClaim struct { Sub string `json:"sub"`; ... }   // same fields
```

`NodePlain`/`AddrPlain` exist for same reason at mega level (self-referential field types meant `type AddrPlain Addr` not enough — see `bench/types.go`). For non-recursive structs, parallel struct declaration cleanest.

**Symptom when forgotten**: supposedly-reflection row matches easyjson allocs and ns/op almost exactly when should be 3-10× slower. ggen's own `AppendJSON`/`DecodeFrom` do NOT trip this — not `json.Marshaler`/`Unmarshaler`. Only stdlib-interface methods cause cross-codec pickup; if struct opts into ggen `marshal`/`unmarshal` hooks, same isolation applies.

## Benchmarks (~5.6 MiB deep Node tree, full validation)

AMD Ryzen AI MAX+ 395, Go 1.26, GOEXPERIMENT=jsonv2. Node carries scalars, slices, string-keyed maps, fixed-length tuples, slices of pointers (slab path), nested slices, pointer fields, time, bytes (base64), `any`, `json.RawMessage`.

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

`ggen_presized` = same `AppendJSON` codepath with once pre-sized buffer — zero allocs, zero GC, ~14% faster than `encode.Marshal(v)`. 2 allocs on plain `ggen` = per-call output buffer + 1 misc. At Mega scale beats sonic_fast ~1.7× wall clock, ~33000× allocated bytes.

### Reader input (streaming)

| path                         | ns/op  | B/op    | allocs |
| ---------------------------- | ------ | ------- | ------ |
| jsonv2.UnmarshalRead         | 4237 K | 17.7 MB | 316834 |
| sonic.NewDecoder             | 2358 K | 39.0 MB | 137791 |
| sonic_fast.NewDecoder        | 2311 K | 39.0 MB | 137790 |
| easyjson.UnmarshalFromReader | 3204 K | 31.5 MB | 245886 |
| **ggen UnmarshalStream**     | 2900 K | 17.7 MB | 256587 |
| **ggen ReadAllUnmarshal**    | 2274 K | 29.0 MB | 101956 |

ggen Stream copies strings during parse (each scanned string own heap alloc), why loses on alloc count; wall clock within ~1.3× of `ReadAllUnmarshal` at ~40% lower B/op (was 3.5× slower before `skipString` bounded its backslash probe — unbounded probe made `SkipValue` quadratic on buffered payloads). Win returns on **Marshal** (1.47× faster than easyjson) and **bytes-only path** (1.45× faster). Cleanest "I have an io.Reader" pattern = `ReadAllUnmarshal` — same shape as bytes path, comparable wall clock at cost of one `io.ReadAll` buffer.

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

Run `GGEN_BENCH_TOPALLOCS=1` to surface top-5 allocation sites.

### `B/op` notes

- **Marshal (`ggen`):** B/op ≈ output buffer size (~11.8 MB = marshalled wire bytes). Only 2 allocs/op (output buffer + 1 misc); `JSONSize()` sizes that one allocation (per map entry `4 + 2*len(k) + value-bound`, or flat 128 for nested/struct). Down from flat `128 * len` (~2.4× overshoot pre-tighten). Zero-alloc → see `ggen_presized`.
- **Marshal (`ggen_presized`):** caller-owned buffer + AppendAny concrete-type fast paths for every primitive shape (`[]any`/`[]string`/`[]int*`/`[]uint16/
32/64`/`[]float*`/`[]bool`, `map[string]any/string/int*/uint*/float*/bool` — bypass reflect.MapIter boxing) → zero allocations, zero GC.
- **Unmarshal:** ggen reports higher B/op than easyjson (6.1 MB vs 3.3 MB for ~970 KB input) because `unsafe.String` aliases keep entire input buffer alive (GC accounts as live allocation per iteration). Allocs still ~3.4× lower than easyjson (18 K vs 61 K).

## Running benchmarks

**ALWAYS pin to a dedicated core and disable parallelism** — every perf claim in
this repo must come from `GOMAXPROCS=1 taskset -c 25 … -cpu=1`. The default
multi-core, multi-GOMAXPROCS run is layout- and scheduler-noise-dominated and
will mislead (sub-1% deltas flip sign across runs). Use **core 25**.

```sh
(cd bench && GOMAXPROCS=1 taskset -c 25 GOEXPERIMENT=jsonv2 go test -run=^$ -bench=. -cpu=1 -count=12 ./...)
# fixed iter count for comparable retention numbers:
(cd bench && GOMAXPROCS=1 taskset -c 25 GOEXPERIMENT=jsonv2 go test -run=^$ -bench=Retention -benchtime=1000x -cpu=1 ./...)
```

For an A/B (old vs new), build both as test binaries and interleave them under
the same pin (`go test -c -o old.test` / `new.test`, then alternate runs), and
run `benchstat` over `-count=12`+ samples — never a single default-layout A/B.

`./...` from root does NOT cross module boundaries — `cd` into `bench/` first.
