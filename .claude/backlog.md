# ggen backlog, tried-and-rejected, maybe-someday

Internal-only notes that should not clutter user-facing CLAUDE.md
files but are load-bearing for future generator/runtime work.

## Backlog (ideas worth pursuing, not yet scheduled)

- **Improve fuzz coverage.** Current fuzz surface (in
  `integrationtests/fuzz_test.go`) is three fuzzers over `Node`:
  `FuzzScanNoPanic` (panic safety on random bytes), `FuzzRoundtrip`
  (encode → decode fixed-point after one round), and `FuzzCompat`
  (ggen ↔ jsonv2 agreement when both accept). Gaps worth closing:
  per-feature fuzzers covering the corners `Node` doesn't reach —
`  alias types (primitive, struct, container variants), every
  validation rule (oneof, runes, ascii, email, …) with rule-specific
  generators, the streaming path (`UnmarshalStream` over a chunked
  reader with varied chunk sizes), `[N]T` strict-length arrays,
  `KindAny`/`KindRawJSON` edge cases (deep nesting, mixed
  null/array/object), `omitempty`/`omitzero` round-trip stability,
  multierr accumulation. Add per-fuzzer seeds for known-tricky
  inputs (truncated `\uXXXX`, surrogate pairs, `null` mid-value,
  trailing-garbage variants).

- **Add more CLI flags.** Specifically what's missing is TBD —
  candidates to consider when revisiting: a `-out-dir` for shared
  output (vs the current next-to-source layout), per-struct selectors
  beyond the trailing-name filter (`-only=Foo,Bar` style), and an
  explicit `-tag <tag>` to scope generation to one build-tag bucket.
  None are urgent; pick the ones that map to a real workflow before
  adding. (`-dry` shipped — parse + validate without writing any
  file; entry points in `check.go` are factored for the future
  `ggenvet` tool to reuse.)

- **Custom vet tool.** Ship a `ggenvet` (`go vet -vettool=ggenvet`)
  binary that catches misuses the compiler can't see. The biggest one
  is the **zero-copy aliasing footgun**: decoded strings alias the
  source `[]byte`, so mutating the input after `DecodeFrom` silently
  corrupts the decoded values. A flow-sensitive check that flags any
  write to a `data` arg (slice index assignment, `append` over the
  same backing, `copy(data, …)`) after it was passed to a ggen
  `DecodeFrom` would catch real bugs. Other candidates:
    - **stale generated file** — find a struct with `//ggen:generate`
      whose `<dir>_ggen.go` is missing the corresponding method set
      (e.g. field added after last regen).
    - **annotation/tag mismatch** — `ggen:"required"` on a pointer
      field marked `omitempty`; `ggen:"oneof=…"` whose values don't
      lex as the field's kind; `mod:"trim"` on a non-string.
    - **validation-rule applicability gaps** — extend the parse-time
      matrix into vet so misuses appear at `go vet` time instead of
      next codegen.
  Distribution shape: separate `ggenvet/` subpackage with its own
  `main.go` so users can `go install …/ggenvet@latest`. Reuses ggen's
  parse layer (`packages.Load` + the tag parser) so checks stay in
  sync with codegen rules.

- **`AppendAny` output prealloc via size precalc.** Bench data shows
  ggen ties or barely beats jsonv2/stdjson on typed slice marshal
  ([]int 712 vs 745 ns/op, []float64 2875 vs 2873 ns/op) despite
  beating them 2-4× on every map shape. Root cause: bench passes
  `nil` dst, so `AppendAny` runs the `append` growth chain (0 → 8 →
  16 → … → 1024), paying 7-8 allocs for output bytes that total
  ~330 B. stdjson hides this with a sync-pooled `encodeState`
  buffer; jsonv2 similarly pre-sizes via its own pool. Presized
  caller-owned buffer benchmarks (`BenchmarkAppendAny_Presized`)
  drop to 0 allocs and ggen wins by 1.85× on `[]int`. Options:
    - **Pre-walk for size**, like ggen-generated code does via
      `JSONSize()`. AppendAny has no compile-time type info so the
      walk is reflect-driven — fine for primitive maps/slices
      (typed range, no boxing), expensive for arbitrarily-nested
      `[]any` / `map[string]any` (recursive reflect descent).
      Heuristic: walk only when `cap(dst) == 0` AND the input is a
      concrete homogeneous container we already have a fast path
      for (typed slice/map); skip the walk for `[]any` /
      `map[string]any` / opaque interfaces where the bound is
      unbounded anyway. ~12 fast-path cases × ~5 lines each =
      cheap to add.
    - **Internal `sync.Pool` inside the encode package's `Marshal`**
      (NOT `AppendAny` itself — caller-owned `dst` semantics stay
      intact). Same trick stdlib uses. Wins on the implicit-buffer
      `Marshal(v)` shape, no change for callers who already pass
      a sized dst.
    - **Explicit hint API** `AppendAnySized(dst, v, hint)` — honest,
      no hidden state, caller picks the bound. Pairs naturally
      with ggen-codegen sites that already know the size.
  Pick when there's a real workload pinning slice marshal as a
  hotspot. Today the map wins dominate; slice tie is acceptable.

- **Wrap parse errors in `decode.ParseError` with position context.**
  Today `scan.ErrBadString` / `ErrBadObject` / `ErrBadNumber` /
  `ErrUnexpectedEnd` are bare sentinels with no `where` and no
  `what`. A user gets `"ggen: bad string"` and has to bisect the
  payload by hand. Wrap them at the call site with:
    - **byte offset** — `pos int` from the scanner state at the
      moment of failure. Cheap; already in scope.
    - **field path** — `field string` (or `[]string` for nested
      struct/slice positions) accumulated as the generated
      dispatch loop descends. Codegen emits the field name in
      each case body; would need to thread a path-stack
      argument through `DecodeFrom` recursion (cost: extra
      param on the hot path — measure carefully).
    - **nearby bytes window** — `snippet []byte` around `pos`
      (e.g. ±32 B), aliased into the input via `unsafe.String`
      so no copy. Lets the error message render `... abc <here>
      def ...` style.
    - **rule** — which scan primitive failed (`"string"`,
      `"object-close"`, `"number"`). Maps 1:1 to the existing
      sentinel; just promote it from package-level var to a
      field on the wrapper.
  Shape: `type ParseError struct { Field, Rule string; Pos int;
  Snippet []byte; Err error }` with `Unwrap()` returning `Err` so
  `errors.Is(err, scan.ErrBadString)` keeps working. Field-path
  threading is the cost driver; if measurements show a regression
  on the hot path, keep the path optional (zero-cost when nil) and
  let users opt in via a build tag or runtime flag.

- **Position context on `validation.*` errors.** Same idea, one
  layer up. Today `MinLenError{Field, Limit, Got}` etc. carry
  the logical field name but not the byte offset where the
  bad value was scanned. Adding `Pos int` (and maybe `Snippet
  []byte`) would let consumers underline the offending region
  the same way the parse-error wrap above does for scanner
  failures. Generated code already has `pos`/`s.Pos` in scope
  when it raises the error — threading it into the literal
  struct is one extra field per call site. Wire-shape
  implication: the validation.Error interface grows or gets a
  sibling `PositionedError interface { error; Pos() int }` so
  consumers can probe without breaking existing match patterns.
  Pair with the parse-error wrap above so a single fail-site
  logging format covers both error kinds.

- **Revisit `validation.CustomError` shape.** Today it carries
  `{Field, Name string, Cause error}` and exposes `Unwrap()`. Specifics
  TBD, but the current shape has rough edges worth a pass:
    - `Name` doubles as the rule identifier in messages ("validation
      %q failed") and as the registry key — separating those (e.g.
      `Rule string` for the rule name vs `Name string` for the
      user-facing label) would let downstream consumers match on rule
      identity without string comparison.
    - No `Value any` field like the other typed errors carry, so a
      `CustomError` doesn't expose what the user's validator rejected.
      Adding one would unify the inspect-failure pattern across all
      `validation.Error` types.
    - `Cause` is `error` but in practice it's almost always the user
      function's return — a typed sub-interface could make the
      `errors.As` shape more useful.
  Pick the angle when there's a concrete report-shape ask.

- **Decode-into-receiver merge on `*T` pointee.** Today every pointer
  field always allocates a fresh pointee via `var v T; ... result.X =
  &v` — the receiver's existing `*T` is discarded. The other field
  kinds (scalar, slice, map, nested struct) already honor the
  receiver-as-merge-source contract (`scalars persist`, `slices
  re-use backing via [:0]`, `maps clear()` and reuse buckets,
  `nested struct value-receivers carry the existing value`). Pointer
  fields are the one hole: re-decoding into the same `*T` allocates
  a brand-new pointee every time instead of writing into the
  existing one. Fix shape: in renderField's pointer block, emit
  something like `if result.X == nil { var v T; result.X = &v };
  *result.X, _, _ = (*result.X).DecodeFrom(data)` for ggen-typed
  pointee, with the equivalent for primitive pointees (read scan
  value, write through `*result.X`). Trade-off: JSON `null` still
  must set `result.X = nil`, so the pre-existing pointee gets
  dropped — caller's `*T` lifetime becomes load-bearing on whether
  the input contained the field. Probably the right default
  anyway (matches stdlib merge semantics) but worth pinning a
  test case in integrationtests/merge_test.go before shipping.
  Pick when someone shows up with a real receiver-reuse hot path
  where the per-decode pointee alloc is a problem.

- **`sql.Null[T]` (Go 1.22 generic form) fast path.** Today the
  type doesn't match `SQLNullSpec` (string-based lookup against
  `sql.NullString` / `sql.NullInt64` / …), so every field of type
  `sql.Null[int]` / `sql.Null[time.Time]` / `sql.Null[bool]` /
  etc. falls through the field-level emitter into the
  `encoding/json` reflective fallback for BOTH decode and
  marshal. Two consequences:
    1. **Wire-shape divergence inside the sql.Null* family.**
       Legacy `sql.NullX` ships ggen's driver-convention "inner
       value or null" shape (see "Wire-format divergences from
       stdlib" in root CLAUDE.md). The generic form falls through
       to stdlib, which has no MarshalJSON on `sql.Null[T]` and
       reflects out `{"V":val,"Valid":true}` — the plain-struct
       dump. Same library, two different wire shapes depending on
       whether the user picked the new generic or the old concrete
       type. Silent footgun.
    2. **Slow path.** No inline scan, no AppendText. Single 128
       byte budget for JSONSize regardless of inner kind.
  Fix shape: extend `SQLNullSpec` (or add a sibling
  `SQLNullGenericSpec`) to recognize the `sql.Null[…]` pattern,
  parse out the inner type token, resolve it through the existing
  `resolveKind`, and return a `SQLNullKind{Field: "V", Inner:
  innerKind, Type: innerType}`. The current `KindSQLNull` codegen
  paths (decode / stream-decode / JSONSize / AppendJSON) already
  thread `spec.Field` / `spec.Inner` / `spec.Type` correctly, so
  one parse-time change unlocks all four. Add per-inner-kind test
  struct + JSONSize coverage mirroring the legacy
  `SQLNull*Struct` split in integrationtests/sql_test.go. Open
  questions for whoever picks this up:
    - Scope of accepted inner kinds. Whitelist (string / int* /
      uint* / float* / bool / time.Time matching legacy Null*) is
      tightest; full `resolveKind` reach means inner kinds like
      `netip.Addr`, `[]byte`, `sql.Null[sql.NullString]` need
      thought.
    - Cross-package generic type strings — `pkg.Null[T]` would
      need the same lookup if any external package re-exports a
      Null-like generic; probably out of scope, the stdlib type
      is the only realistic target.

- **Multi-level pointer (`**T`, `***T`, …) inside slice/array
  elements.** Scalar fields (`*****int`) and map values
  (`map[string]**T`) work today via the `encoding/json` fallback —
  the field-level emitter sees the chained-pointer type, can't
  recognize an inner JSON shape, and routes through
  `json.Unmarshal` over a `scan.SkipValue` span. Slice and array
  elements take the slab fast path instead, and the slab emitter
  assumes the peeled element is a "real" type, not another
  pointer:
    - `[]**int`: the slab is typed `[]*int`; the pre-grow line
      becomes `slab = append(slab, *int{})` — invalid Go,
      compilation fails.
    - `[3]**int`: the slab compiles (`make([]*int, 3)` + per-
      index assignment) but the inner scan is a bare
      `scan.SkipValue` with no actual decode — every element ends
      up pointing at a zero-valued `*int(nil)` silently.
  Fix shape: detect "ElemPointer && peeled ElemType still begins
  with `*`" (or equivalently ElemKind=KindStruct + non-generated +
  no Text/JSON marshaler on the underlying) and route the per-
  element decode through a json.Unmarshal fallback that targets
  `*ElemType`, the same way the map value path already does. The
  slab path stays for depth-1 cases. Coverage gap pinned by
  `TestNPtr_*` in integrationtests/pointer_test.go (scalar field
  + map value only); the slice/array variants are intentionally
  absent until this lands.

## Tried and rejected (don't re-attempt without new evidence)

- **Generator emitting `go/ast` nodes instead of text.** Full rewrite
  lives on the `ast-conversion` branch (commit `feadbba`). Each renderer
  returns `[]ast.Stmt`; `renderStructMethods` composes the four core
  methods (DecodeFrom, DecodeStreamFrom, JSONSize, AppendJSON) plus the
  optional Marshal/UnmarshalJSON hooks as `*ast.FuncDecl`s, then the file
  emits via `format.Node`. Generated code came out byte-identical.
  Rejected for three reasons:
    1. **Less readable.** Every `fmt.Fprintf(b, "if %s == nil {…", ref)`
       turns into an `&ast.IfStmt{Cond: &ast.BinaryExpr{…}, Body:
       &ast.BlockStmt{List: …}}` tree. Render code becomes pointer-
       struct boilerplate; you can't skim the rendered Go shape out of
       the generator source anymore.
    2. **Higher peak RAM.** AST nodes are pointer-heavy Go structs that
       survive until the whole file is printed.
    3. **Marginally slower codegen.** Small but consistent regression.
    4. **Slightly larger binary footprint.** Another unwanted thing.

  Kept on the branch in case the AST layer ever enables an `ast.Walk`-
  based optimization not feasible against text (e.g. replacing
  `coalesceConstAppends`), but no current use justifies the cost.

- **Pointer-arithmetic decoder / `unsafe.Add` byte loads** to eliminate
  bounds checks. Conversion of all four hot inliners (SkipWS,
  ScanInt64, ScanUint64, ScanString) plus all spot accesses brought
  bounds checks in `bench_ggen.go` from 59 → 18 (byte path: 0). Result:
  Unmarshal **regressed by ~10%** normalized vs reference libs. Why:
  modern AMD64 branch prediction makes never-taken bounds checks
  effectively free (~1 cycle, predicted), while `unsafe.Add` defeats
  Go's compound addressing-mode codegen — `data[i]` compiles to one
  `MOV (base)(idx*1)` instruction; the unsafe form takes 2–3
  instructions because the optimizer treats the unsafe load as
  opaque. Lost addressing mode + lost loop-invariant hoisting >
  bounds-check savings on this CPU. Verified across 5-run benches.
  Don't retry unless targeting a CPU where bounds-check branches mispredict.

- **Removing all decode-side inliners** (inlineSkipWS / inlineScanInt64
  / inlineScanUint64 / inlineScanString) and replacing them with plain
  `scan.X(...)` function calls. A `//go:noinline` micro-bench had shown
  per-call overhead at ~0.4 ns (12.65 vs 12.89 ns/op for Int64 across
  20 runs of 5 s each — basically indistinguishable). Macro result:
  Unmarshal **regressed ~15–20%** normalized. The single-call
  micro-bench understated the cost because in macro context, inlining
  matters for register allocation across adjacent ops, ICache pressure
  from N small fns called repeatedly, and codegen-level optimizations
  (branch hoisting, compound BCE) the compiler can only do with the
  body in scope. Don't trust per-call micro-benches for hot-loop
  inlining decisions.

- **Stream-path `_s.SkipSpace` inliner** (`inlineStreamSkipWS`). Most-
  hit method on the stream path (5+ per field). Inlining the body
  inline at every call site saved the method-dispatch frame but kept
  the `_s.Ensure(j+1)` call inside the loop. Raw bench showed +7%
  improvement; normalized via EasyJSON Reader as machine-state proxy,
  real gain was **~2% — within noise**. The `Ensure` cold path
  dominates Stream throughput, so eliminating method dispatch on
  SkipSpace specifically doesn't move the needle. Don't retry without
  also tackling Ensure overhead.

- **Inlining `scan.Bool` / `scan.Float64`**. `//go:noinline` micro-
  bench against an inlined-body equivalent showed the function-call
  frame is fully amortized by the body work for primitives at this
  size — measured difference was 0.24 ns of 12.6 ns total, with the
  call version slightly *faster* on average. No real win available.
  Same lesson as the inliners-removal entry above: don't chase
  per-call overhead in isolation.

- **`Ensure(p *int, n int) error` + `Anchor(p)` / `Unanchor()` for
  bounded streaming.** The original streaming primitive bulk-fetched N
  bytes at the call site by looping `Read` internally until satisfied,
  and supported a window-shift mode that let `Ensure` slide the buffer
  forward (dropping the prefix) under bounded-buffer mode; `Anchor` /
  `Unanchor` froze that shift across `SkipValue` so `RawJSON` /
  `json.Unmarshal` fallback could slice `_s.Bytes()[_start:_k]` after
  the fact. Two complaints killed it. (1) "Read in a for loop" inside
  `Ensure` is the antithesis of lazy streaming — if you're going to
  loop on `Read`, `io.ReadAll` is simpler and roughly the same. (2)
  The anchor mechanism plus `*int` cursor adjustment across shifts
  was a constant source of stale-position bugs (e.g. the Float64 /
  Number paths needed `&start` not `&i`, or the buffer would silently
  drop the digit prefix mid-parse). Replaced with `ReadMore(keep int)
  error` — single Read per call, optionally compacts in-place when
  the caller passes `keep > 0` — and **byte-by-byte multi-byte
  literal scans** at the parser level. The simpler `ReadMore()` /
  never-shift shape shipped briefly before this; the `keep` parameter
  came back specifically to bound buffer growth on long streams
  without resurrecting Ensure's bulk-fetch loop. Internal Stream
  methods still pass `keep=0` (grow-only) so caller cursors stay
  valid; only the top-level dispatch-loop bounds checks compact.
  Fail-fast is preserved (~67 ms vs ~78 ms ReadAll on invalid
  payload). Don't reintroduce a bulk-fetch primitive without a
  fail-fast story that doesn't regress lazy semantics.

- **Stream `Acquire`/`Release` pool with reused buffer.** Originally
  `scan.Acquire(r, hint)` returned a pooled `*Stream` and `Release`
  truncated `s.buf` to retain it for the next `Acquire`. Combined with
  alias-mode strings (`unsafe.String` into `s.buf`) this looks like a
  win in a microbench but is **silent corruption**: the next `Acquire`
  reuses the same buffer, the `Read` call overwrites bytes, and prior
  decoded values' aliased string fields still point at those locations
  — so e.g. `n1.Name` flips from `"FOO_TINY"` to `"BAR_VERY"` after a
  second decode. Caught by writing a two-payload correctness probe;
  the residency benchmark didn't catch it because content collisions
  happened to match. Replaced with stack-allocated Stream + caller-
  owned buf and copy-mode strings.

- **`[512]byte` inline scratch in `Stream`.** Idea: avoid the buffer
  heap alloc for small payloads by embedding a stack-resident scratch
  array in `Stream`, spilling to heap only when payload > 512. Doesn't
  work in the original tests because Go's escape analysis can't prove
  `&s` is safe across the `zero.DecodeStreamFrom(&s)` call inside
  what was then a generic `decode.UnmarshalStream[T]` wrapper — the
  generic dispatch defeated it, so the entire `Stream` (now including
  the 512-byte array) got heap-allocated. The wrapper is now gone and
  the call site is direct (`var s scan.Stream; ...; T{}.DecodeStreamFrom(&s)`),
  so the escape constraint may no longer apply — worth re-measuring
  if a new residency push needs the small-payload alloc back.

- **Per-decode arena + `StreamArenaSize`/`StreamArenaCompact` codegen.**
  Flow: parse with aliased strings (zero per-string alloc, fast),
  walk the decoded value to sum string bytes, allocate one arena
  `[]byte` of exactly that size, copy each string into the arena and
  rewrite headers via `unsafe.String`. Fully implemented across struct
  / slice / map / `any` / aliased-primitive fields. Result on Mega:
  alloc count dropped 347K → 128K (matches bytes path); B/op dropped
  24.6 → 19.7 MB; **residency unchanged at ~86 KiB/item**. Wall clock
  also unchanged — 2-walk overhead canceled out the per-string-copy
  savings. The residency stayed put because the gap was never
  per-string heap fragmentation; it was the per-decode buffer
  retention + map rebuild allocs (Go has no in-place key-rewrite
  primitive). Removed the codegen and `decode/arena.go` — added a lot
  of complexity for no measurable gain. If retries: prove the gain
  on residency BEFORE shipping the codegen, not after.

- **`maxlen=N` as a slice/map prealloc hint.** Original codegen used
  `maxlen=64` to emit `make([]T, 0, 64)` so Mega's typical 5-26 element
  slices avoided the growth chain. Hidden cost: every retained value
  carried the over-allocated slice/map cap forever. Killing this
  hint (residency villain) cut per-item retention from 163 KiB →
  ~67 KiB on the bytes path — biggest single residency win in the
  whole exploration. Now only `len`/`minlen`/`hintlen` drive prealloc.
  Don't reintroduce `maxlen` as a sizing hint without an opt-in
  mechanism (see `hintlen` for the explicit-hint pattern).

## Maybe-someday (only if a real need shows up)

- **Hybrid key-dispatch strategy at codegen** — current length-first
  switch + if-chain wins for narrow structs (~3–5 cycles per dispatch
  on typical 2-3 candidates per length group). For wide structs where
  length groups balloon (>5 candidates), emit `switch key` so Go's
  compiler auto-hashes (≥7 cases triggers hash dispatch). Picking
  per-struct or per-length-group at codegen could squeeze out a few %
  on wide structs without regressing narrow ones. Postponed until
  someone shows up with a 50+ field schema where it matters.

- **Validation-derived encode hints (Trusted-ASCII, schema-bound
  numbers, etc.).** Use `ggen` validation tags to inform encode-side
  shortcuts: `ascii` → skip escape table on marshal; `lte=N` → emit a
  hand-rolled fixed-width digit formatter instead of `strconv.AppendInt`;
  similar for `oneof`/`len`. Real wins on hot fields, but couples
  encode shape to decode-time validation semantics — the same struct
  field would marshal differently based on its `ggen:` tag, which
  blurs the marshal contract. (Decode-side preallocation already uses
  `len`/`maxlen`/`hintlen` — see codegen optimization #2 — that's a
  one-way hint into a `make` cap, not a wire-shape change.) Shelved
  unless there's a target schema where the win is concrete.

- **Streaming `io.Reader` over marshalled output (state-machine codegen).**
  Idea: per-struct `AsReader()` returning a resumable state, plus an
  `encode.Reader[T](v)` driver that exposes it as `io.Reader` (or
  `io.ReadSeeker` for HTTP body replay). Suspends mid-marshal so peak
  memory = caller's `p []byte` instead of `JSONSize()`. Three granularity
  tiers considered (per-field, per-element, byte-level); per-field is
  cheapest at ~300 LOC of generator change but only matters when a single
  payload is too big to materialize. Real-world `JSONSize()` already fits
  comfortably in RAM for everything we care about — shelved unless
  someone shows up with multi-GB JSON request bodies. The trivial
  "bytes.NewReader over Marshal output" version is a one-liner the user
  can write themselves; no need to bake it in.

- **SIMD / AVX2 vectorization for hot scanning loops.** Sonic's
  decoder narrows the gap on Mega Unmarshal in part because bytedance
  hand-wrote AMD64 assembly that uses AVX2 for string-quote scanning,
  whitespace skipping, and number parsing. ggen currently does these
  byte-at-a-time in `scan/scan.go` and `scan/stream.go`. Candidates
  worth probing:
    - `bytes.IndexByte` (already SIMD-accelerated by Go runtime — we
      use it for the string-closing-quote scan; verify it's vectorizing
      on amd64).
    - inline AVX2-accelerated WS skip via `golang.org/x/sys/cpu` to
      detect support + Plan9 assembly stub.
    - number parsing: probably not worth — `strconv.ParseInt` /
      `strconv.ParseFloat` are already heavily tuned, and ggen's
      hand-rolled inline int scan beats the function call for the
      common case.
  Real shape of the win unclear: sonic's vector win is bundled with
  JIT and asm dispatch — pure SIMD-on-Go (no JIT) might claw back
  10-15% on string-heavy payloads at the cost of:
    - per-arch (amd64 / arm64) source duplication
    - `go vet`-incompatible asm files
    - Plan9 syntax maintenance burden
    - lost portability (ggen currently runs on any GOARCH)
  Try only if a target workload shows the byte-scan loop as the
  dominant cost in CPU profile, AND the codegen complexity is
  acceptable. Don't speculatively add asm files "to keep up with
  sonic"; the gap is small and ggen's portability is a feature.

- **Minimize root CLAUDE.md / split per package.** DONE — root keeps
  CLI / codegen reference, each package directory has its own
  CLAUDE.md, this backlog file lives under `.claude/`. Kept here as
  a record of the split.
