# TODO

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

- **Position context on `validation.*` errors.** Same idea one layer up. Add
  `Pos int` (maybe `Snippet []byte`) to `MinLenError` etc. — generated code
  already has `pos`/`s.Pos` in scope at failure site. Either grow
  `validation.Error` interface or add sibling `PositionedError interface {
  error; Pos() int }`. Pair with parse-error wrap above.

- **Revisit `validation.CustomError` shape.** Today `{Field, Name string,
  Cause error}` + `Unwrap()`. Rough edges: `Name` doubles as rule identifier
  and user-facing label (split into `Rule` + `Name`); no `Value any` field like
  other typed errors (can't expose what validator rejected); `Cause` bare
  `error` (typed sub-interface could improve `errors.As`). Pick when
  concrete report-shape ask exists.

- **`sql.Null[T]` (Go 1.22 generic form) fast path.** Doesn't match
  `SQLNullSpec` (string lookup against `sql.NullString`/…), so every
  `sql.Null[int]`/`[time.Time]`/… falls through to `encoding/json`
  reflective fallback for both decode and marshal. Two consequences: (1)
  **wire-shape divergence inside family** — legacy `sql.NullX` ships ggen's
  "value or null" shape, generic form reflects out `{"V":val,"Valid":true}`
  (silent footgun); (2) **slow path** — no inline scan, no AppendText, flat
  128-byte JSONSize. Fix: extend `SQLNullSpec` (or add `SQLNullGenericSpec`) to
  recognize `sql.Null[…]`, parse inner type, resolve via `resolveKind`,
  return `SQLNullKind{Field:"V", Inner, Type}` — four codegen paths already
  thread `spec.Field`/`Inner`/`Type` correctly, so one parse-time change
  unlocks all. Open questions: scope of accepted inner kinds (whitelist
  string/int*/uint*/float*/bool/time.Time is tightest); cross-package
  `pkg.Null[T]` probably out of scope. Mirror legacy `SQLNull*Struct` split
  in `integrationtests/sql_test.go`.

- **`null` on non-pointer value kinds — accept-as-zero vs strict-reject.**
  Surfaced by the merge audit (`TestStdCompatMerge_IntentionalDivergences`).
  ggen emits a `null` peek only for pointer / slice / map / `sql.Null*` /
  raw-message fields; every other kind (scalars, `[]byte`, time, duration,
  net/netip, url.URL, big.*, uuid, `,string` scalars) hard-errors on an explicit
  JSON `null`, whereas stdlib v1/v2 accept it (zero the field / no-op). Real
  parity gap: a payload stdlib accepts, ggen rejects. Two stances: (a) keep
  strict-reject (consistent with UnknownKeyError / strict-array / DuplicateKey
  defaults — "use a pointer for nullable") and just keep it documented; (b) emit
  a `null` peek for these kinds that zeroes the field (matches stdlib). If (b):
  add an `inlineNullPeek`-style 4-byte check at the top of each scalar/native
  value emit in `renderField`/`renderStreamField` that sets the field to its
  zero value and advances 4 — mirrors the existing pointer/slice null branch.
  Decide per ggen's strictness philosophy; (a) is the current default. Pinned as
  divergence until decided. (`[]byte` sub-case RESOLVED — KindBytes now
  accepts `null` → nil and marshals nil as `null`.)


# Tried Rejected

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
