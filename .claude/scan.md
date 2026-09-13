# scan — hand-rolled JSON scanner + streaming Stream

Runtime package. Bytes-path primitives + streaming `Stream`. Generated decode
methods call directly. No tokenizer, AST, reflection.

## Bytes path (`scan.go`)

`[]byte` primitives on `(data, pos)` → `(value, newPos, error)` or `(newPos,
error)`. Zero-alloc happy path, sentinel errors (`ggen.ErrBadObject` etc).
Primitives: `SkipSpace`, `String`, `Int64`, `Uint64`, `Float64`, `Bool`,
`ObjectOpen`, `ArrayOpen`, `SkipValue`.

- **Error-position contract (2026-08).** On error every bytes primitive returns
  the position where scanning STOPPED — a byte strictly inside `data` for a
  malformation, `len(data)` when it ran off the end — never a flat 0 (which made
  every runtime-call parse failure stamp `ParseError.Pos = 0`). **A control
  byte inside a string reports the CONTROL BYTE's own index** — every string
  scanner, both paths, all SIMD tiers: bytes `String` (open tail, clean span,
  and `stringSlow`'s pre-escape prefix), `skipString`, the stream
  `stringView`/`KeyView`/`stringSlow`, and the vector tiers, which get the
  index for free from their structural locate. The hot loops keep their
  boolean SWAR gates (`hasCtrlByte`/`ctrlOrHigh`/`checkSpan`); a cold byte
  walk `ctrlIndex` spells the position on the error branch only.
  `ErrInvalidUTF8` reports the span start instead — there is no single
  offending byte. The SIMD twins
  (`StringAVX*`, `SkipValueAVX*` + skip tiers) mirror the scalar positions
  byte-for-byte — pinned by the existing `*SIMD_Parity` differentials, which
  compare (value, pos, err) against scalar. The `Any*` families propagate the
  improved positions; `skipValueAt` is gone (it existed only because exported
  `SkipValue` normalized to 0 — `SkipValue` itself now preserves the give-up
  position, and `CaptureValue` reads it directly).
  **End-of-data malformations are final.** A `\uXXXX` escape whose buffered
  digits contain a non-hex byte reports the backslash (bytes) / leaves `Pos`
  on the backslash (stream); only a tail that can still become an escape —
  empty, `\`, `\u`, `\u`+hex digits, `uEscapePrefix(tail)` — reports
  `len(data)` (bytes) or refills (stream); when that refill DRAINS, the
  stream lands on the same absolute offset the bytes path reports — the end
  of the input, not the backslash (`"ab\` → 4, `"ab\u00` → 7, against
  `"ab\u00zz"` → 3, the backslash, on both), at every chunk and buffer size.
  The surrogate low-half probe
  follows the same rule on BOTH paths (a still-completable low half in
  validate mode reports `ErrInvalidUTF8` at `len(data)`; the stream's
  surrogate-pair lookahead refills only while `uEscapePrefix` holds, so a
  reader that DRAINS there was truncated, not lone, and the arm reports the
  end of what arrived instead of the byte past the high-surrogate escape).
  A control byte in an OPEN tail (no closing quote yet, no backslash) is
  `ErrBadString` at that byte (`hasCtrlByte(rest)` gates, `ctrlIndex(rest)`
  locates), never `ErrUnterminated` — `ErrUnterminated` means "ran off
  the end with no malformation seen". Every `\u` site used to demand its six
  bytes before looking at the digits already buffered, and the bytes string
  scanners located the closing quote before classifying the tail, so a
  malformed value near the end read as truncated: `CaptureValue` and every
  stream refill classify `pos == len` as "read again", which blocked a live
  reader forever on an irreparable value, and the bytes verdict split from
  the stream's (jsonv2 sides with the stream). The SIMD tiers
  (`classifyStructural`/`classifyStructural64`, the three `skipString*`)
  return `ErrBadString` unconditionally on a ctrl hit, at the scalar position.
  Pinned by `TestString_MalformedTailIsFinal`,
  `TestStreamString_LiveMalformedTailDoesNotHang`, the `"abc\x01` /
  `"\u12"` rows of `TestBytesStreamTruncationErrorParity`,
  `TestStreamString_ErrorPos` + `TestStreamSkipStringSIMD_ErrorPos` (the exact
  ctrl offset per scanner and per tier) and the SIMD parity lists.
  **`Bool` is an inlinable PROBE; `BoolEnd` carries the position.** `Bool`
  returns the literal START on failure (cost 54 of the 80 inline budget; any
  give-up walk or call inside it measured 95–117 and would de-inline it at
  every generated bool site). The give-up position — first byte breaking the
  literal, or `len(data)` for a proper prefix — comes from the exported cold
  `BoolEnd(data, i)` (`litEnd` over `"true"`/`"false"`, chosen by `data[i]`);
  `skipValue` (+ the SIMD skip tiers), all four `Any*` families and the
  generated bool sites (cli/CLAUDE.md opt #82) stamp it, as the null arm
  already does via `litEnd`. `Stream.Bool` mirrors it: `Pos` = the
  mismatching byte, or the window end when the reader drained mid-literal,
  rebased to 0 after the compacting head refill; a transient reader error
  keeps `Pos` on the literal head (the grow-only refill keeps it buffered) so
  a retry re-scans losslessly. Pinned by `TestBoolEnd_GiveUpPosition` +
  `TestStreamBool_ErrorPosMatchesBytes`.

- **Depth cap (`maxDepth` = 10000).** `SkipValue` and the four `Any` families
  (`Any`/`AnyNumber`/`AnyCopy`/`AnyNumberCopy`), their stream mirrors, and the
  SIMD skip tiers keep their public signatures but delegate to a `depth`-carrying
  core; each container OPEN checks `depth > maxDepth` → `ErrMaxDepth` (one
  predictable compare per `[`/`{`, nothing on scalars). Without it a few MB of
  `[[[[…` is a FATAL goroutine stack overflow, not a recoverable error. Generated
  decoders for self-referential structs thread the same counter (cli/CLAUDE.md
  opt #51). Pinned by `TestMaxDepth`.

- **`Int64`/`Uint64` unchecked digit prefix.** First ≤18 (int) / ≤19 (uint) digits
  accumulate with NO per-digit overflow check (`10^18-1 < MaxInt64 < |MinInt64|`,
  `10^19-1 < MaxUint64` — neither `*10+d` nor the value overflows in the prefix); a
  19th/20th digit resumes the checked loop, keeping `ErrNumberOverflow` identity +
  position bit-identical (leading-zero runs pushing significant digits past the
  window resume correctly — prefix accumulates 0). Generated bytes-path decoders
  emit the same prefix inline. Pinned by
  `TestInt64_OverflowBoundaryLattice`/`TestUint64_OverflowBoundaryLattice`. Stream
  `Int64`/`Uint64` carry the same prefix, tracking a `digits` counter across
  refills (`de = min(i+18-digits, len(buf))`); the checked loop only ever
  consumes once `digits` is saturated — an early unchecked exit means the buffer
  ran dry, so the checked loop no-ops into the refill.
- **`SkipValue`/`skipObject` skip strings via `skipString`** — decode-free
  mirror of the stream `skipString` (quote locate + bounded backslash probe +
  SWAR ctrl validate; escapes validated, never unescaped). Discarded escaped
  strings (RawMessage capture, `ignoreunknown`, `allowdups`) no longer pay
  `stringSlow`'s scratch alloc + `\uXXXX` decode.
- **`SkipValue` skips numbers via `skipNumber`** — one-pass RFC 8259 grammar
  validator (`-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?`), NOT `Float64`
  (discards — RawMessage capture, `ignoreunknown`, `allowdups` — need only the end
  position). Accept-set shifts toward stdlib/jsonv2 parity: range-overflow
  grammar-valid (`1e400`) skips OK; ParseFloat-isms JSON forbids (leading `+`,
  leading zero `01`, bare/trailing dot `.5`/`1.`) rejected. Malformed →
  `ErrBadNumber`, not `*strconv.NumError`. Stream mirror `(*Stream).skipNumber`.
  Pinned by `TestSkipNumber_AcceptSetMatchesJSONGrammar` (differential vs
  `encoding/json.Valid`) + `TestSkipNumber_StreamMatchesBytes`. Number-VALUE fields
  (`float64`, `json.Number`) use `Float64`/`Number`, which enforce the SAME
  grammar (cli/CLAUDE.md opt #52) — `Float64` inlines it (routing through the
  `skipNumber` CALL measured slower: "removing decode inliners" again),
  `Number` + the stream mirrors validate their assembled span via `skipNumber`.
  `Int64`/`Uint64` (and the inline codegen emitters) carry the leading-zero
  rule. Before #52 the skip path was strict while the value path was lax, so
  `{"raw":01}` rejected and `{"i":01}` accepted the same bytes.
- **`Float64` exact-short fast path.** Spans ≤ 16 bytes of the form
  `[-]digits[.digits][eE[+-]digits]` skip `strconv.ParseFloat`'s re-scan:
  `exactShort` accumulates a uint64 mantissa in one pass and, when
  `mant ≤ 2^53-1` and `|exp - fracDigits| ≤ 22`, both the mantissa and the
  power of ten are exact floats so the single IEEE multiply/divide is
  correctly rounded — bit-identical to ParseFloat, which is itself correctly
  rounded (Clinger 1990; same argument as strconv's `atof64exact`, which
  gates one bit tighter at 2^52 than exactness requires). Second dots, wider
  mantissas, or `|power| > 22` bail to ParseFloat. The ≤16 B gate structurally
  excludes shortest-form 17-significant-digit floats (≥ 18 chars) — the
  input class that sank the ungated fused variant (see backlog). −7.6%
  NoAlloc when it shipped; the 2026-08 exponent arm added −57% on
  scientific-notation spans and −32% on a mixed number run
  (`BenchmarkFloat64Forms`). Exponent-FREE spans branch on `exp == 0` and keep
  the original instruction sequence — the ≤16 B gate caps `frac` at 15, so
  they need no `|power|` range check; without that split the plain rows
  measured +5%. Declining an out-of-range exponent costs ~1.6 ns before
  ParseFloat takes over. The exponent digit loop clamps its `int` accumulator
  past 999 instead of accumulating unboundedly (found+fixed 2026-08) — any
  span this function accepts needs `|exp - frac| ≤ 22` with `frac ≤ 15`, so
  no legitimate exponent exceeds ~37, but on a 32-bit `int` (`GOARCH=386`) a
  pathological digit run like `1e4294967296` wrapped the accumulator back
  through zero instead of correctly bailing to ParseFloat, silently
  returning the wrong float. Pinned by `TestExactShort_ParseFloatDifferential`
  (1.5M randomized spans incl. a well-formed-exponent generator, bit-identity
  incl. -0 + accept/reject parity, plus must-accept/must-decline assertions —
  the differential only checks ACCEPTED results, so a fast path that silently
  always bailed would otherwise pass it).
- **`Float32` rounds the decimal once.** `Float32(data, i)` and
  `(*Stream).Float32()` duplicate `Float64`'s inline RFC 8259 walk (same
  "removing decode inliners" reason as `skipNumber`) and keep the ≤16 B
  `exactShort` fast path, but narrow its result only through `narrowExact`:
  accepted when the float64 is provably not a float32 rounding midpoint (bit
  test `mant & (1<<29-1) == 1<<28` on the `Float64bits`) and not
  float32-subnormal (midpoints there sit on a coarser grid), otherwise
  `strconv.ParseFloat(raw, 32)`; strconv's `ErrRange` maps to
  `ErrNumberOverflow`. Scanning as float64 and casting rounds twice, which
  lands one ulp off whenever the float64 result IS a midpoint — the shortest
  float64 forms of such values, i.e. what `json.Marshal` of a float64 emits
  (`1.0000000596046448` → `1` instead of `1.0000001`) — and turned
  `3.4028235677973366e38` (below the overflow midpoint 2^128−2^103) into
  +Inf. The stream mirror assembles the span like `Stream.Float64` then runs
  the same tail. Generated code calls them at every float32 site
  (cli/CLAUDE.md). Pinned by `TestFloat32_StdlibParity` (180k random
  float32-midpoint neighbourhoods, bytes + stream, bit-exact vs
  `strconv.ParseFloat(s, 32)`).
- **`ParseRFC3339(s)` (`rfc3339.go`)** — `time.Parse(time.RFC3339Nano, s)`
  plus jsonv2's four post-checks that `time.Parse` skips (two-digit hour, `.`
  fraction separator, zone hour < 24, zone minute < 60), checked in the order
  that keeps every index in range. Generated default / `format:RFC3339` /
  `RFC3339Nano` time sites call it on both paths; the stream path parses from
  `StringView` (no copy), and the checks are byte compares on the
  already-validated string. `AppendRFC3339` is the encode twin
  (.claude/encode.md). Pinned by `TestParseRFC3339_JSONv2Parity`.
- **`String()` zero-copy alias** via `unsafe.String(unsafe.SliceData(data[start:]),
  len)` when no escapes; falls back to `stringSlow` (`utf8.AppendRune` for `\uXXXX` +
  surrogates). `bytes.IndexByte` (SIMD) finds the closing `"`; a second IndexByte
  over the span detects a preceding `\`. With no closing quote and a backslash
  present, `stringUnterminated` runs `stringSlow`'s grammar without copying —
  `stringSlow` cannot succeed there — so a truncated `\u…`/trailing `\` that is
  still a valid escape prefix → `ErrBadString` at `len(data)`, and a tail that
  cannot become an escape reports the backslash (`uEscapeEnd`, see the
  error-position contract). **UTF-8 validated** (jsonv2
  parity, `ErrInvalidUTF8`): the clean span goes through `checkSpan` — the SWAR
  ctrl walk fused with a high-bit accumulate, so pure-ASCII spans never pay the
  `utf8.Valid` rune walk; only spans that actually contain ≥0x80 bytes run it.
  `stringSlow` rejects unpaired `\uXXXX` surrogates (v1 would silently emit
  U+FFFD) and gates a final `utf8.Valid(buf)` on RAW segment bytes having a
  high bit (escape outputs are valid by construction — `EscapeHeavy` stays on
  the no-walk path). Stream mirrors (`ctrlOrHigh` accumulates `high` across
  refill chunks, one `utf8.Valid` over the FULL contiguous span at return — a
  rune may straddle the chunk cursor, so per-chunk validation would
  false-error). SIMD tiers accumulate a lane OR (`acc = acc.Or(v)`, 1 VPOR/
  lane) and test it once at the '"' classify; stream tiers use the
  `structuralIndexHigh*` variants (skip tiers keep the plain ones — skipped
  spans are DELIBERATELY not UTF-8-validated, `ignoreunknown` stays
  permissive). Under `goexperiment.simd` the non-ASCII second pass runs
  `validUTF8x16` (`simd_utf8_amd64.go`) instead of `utf8.Valid` — the
  Lemire/simdjson range-lookup validator on 16-byte lanes (AVX1-safe VPSHUFB/
  VPALIGNR, shared by all three tiers since `classifyStructural` is): three
  nibble LUTs classify each (prev1, cur) byte pair, XOR against a
  saturating-sub "3rd/4th continuation required" mask cancels the one legal
  0x80 case, errors accumulate vectorially. Truncated runes: a partial final
  block loads zero-padded (the zeros fail the lead's successor check), and a
  rune dangling off a FULL final block is caught by one saturating sub against
  `utf8MaxIncomplete` (simdutf's check_eof — only the last three lanes are
  constrained, to `0xF0-1`/`0xE0-1`/`0xC0-1`). That replaced running a whole
  extra all-zero block through the classify to test three bytes. Worth
  −7…−12% on short spans and ~0 at 4 KB (`BenchmarkValidUTF8Kernel`, which
  measures the kernel WITHOUT the surrounding string scan — the end-to-end
  `StringAVX512` rows are too noisy to read a sub-ns delta from). NOTE the
  win is small because the per-call setup — 3 LUT loads + 5 broadcasts, paid
  once regardless of length — is what actually dominates a short span
  (~5-7 ns of a ~6 ns 16 B call); one block's classify is ~1 ns, so removing
  it can never be the 2× a "half the blocks" reading suggests. Pinned by
  `TestValidUTF8SIMD_EOFTruncation` (every proper prefix of every multibyte
  rune at every offset across the block seams, plus the complete-rune
  no-false-positive cases).
  **`validUTF8x64` (`simd_utf8x64_amd64.go`) — 64-lane sibling, avx512 tier
  only.** Same algorithm and accept set; the differences are width and how
  prev1/2/3 are obtained. simdutf needs cross-lane shuffles there
  (VPERMI2D/VPERM2I128, which archsimd has no non-grouped equivalent of above
  128 bits), but ggen always validates a CONTIGUOUS span, so prevN is just an
  unaligned load from `b[i-N:]` — three L1-hot loads replace a permute + three
  VPALIGNRs and shorten the dependency chain. VPSHUFB at 512 bits works per
  128-bit lane, exactly what a nibble LUT wants, so the tables are repeated 4×
  (`rep4`). The first block has no predecessor, so it runs one 16-lane classify
  with `prev = zero` and NO EOF check (the rune may continue into the wide
  region); the wide loop starts at 16, where every prevN load is in bounds.
  The FINAL block is pulled back to start at `len(b)-64` instead of a
  zero-padded `Part` load — each lane's classification is a pure function of
  its own four bytes ORed into `errAcc`, so re-classifying the overlap is
  idempotent, and a block ending exactly at `len(b)` hands a dangling rune to
  the same `check_eof` sub (one loop body, no copy, no mask, no duplicated
  tail). This is the no-over-read rule below: `LoadUint8x64Part` is an
  UNMASKED 64-byte load on go1.27.
  Kernel: 3× from ~1 KB (4 KB 314→104 ns, 13→39 GB/s), −35% at 128 B. Wired in
  via a `classifyStructural64` COPY + the avx512 stream core, because the
  shared `classifyStructural` links into avx/avx2 binaries that must execute no
  512-bit code (verified: `objdump` shows no zmm in the shared body). Both call
  sites pick the width THEMSELVES (`utf8x64MinLen` = 128) rather than letting
  `validUTF8x64` delegate — a short span bouncing through the extra
  non-inlinable frame measured +12% at 16 B. End-to-end `StringAVX512`:
  −33% at 256 B, −53% at 1 KB, −57% at 4 KB; ≤64 B flat. Pinned by
  `TestValidUTF8x64_Parity` (runes and invalid sequences planted at the
  head/wide seam, block ends and the gate; truncations across every tail
  remainder; 20k randomized) and the extended `FuzzValidUTF8SIMD`, which pads
  short inputs past the gate so the 64-lane path is actually exercised. ~6.5× over the scalar DFA (4 KB
  Cyrillic 3456→~530 ns; NoAlloc avx512 −37%, RuneGated −29%, ASCII rows
  flat). Accept-set parity with `utf8.Valid` pinned by
  `TestValidUTF8SIMD_Parity` (exhaustive 1-2-byte × boundary offsets +
  directed overlong/surrogate/too-large/truncation classes) +
  `FuzzValidUTF8SIMD`; `TestConcatShiftSemantics` pins the VPALIGNR operand
  order the prev1/2/3 shifts depend on. Scalar builds, `stringSlow`, and
  `CheckUTF8` keep `utf8.Valid`. Micro cost-split guards:
  `BenchmarkStringUTF8Cost{,AVX512}` (utf8cost bench files). Captured raw spans validate via `CheckUTF8` (SWAR high-bit
  detect, `utf8.Valid` from the first high word — sound because the preceding
  byte is ASCII, i.e. a rune boundary), emitted by codegen at
  `json.RawMessage`/`jsontext.Value` sites, bytes + stream; unpaired surrogate
  ESCAPES inside raw spans are ASCII text and pass (residual jsonv2
  divergence, see backlog). Encode side still does NOT validate (see
  .claude/encode.md). All string scanners take a trailing `validate bool`
  (generated code passes a literal; `allowinvalidutf8` structs pass false —
  raw bytes through, surrogate escapes → U+FFFD, no CheckUTF8 at raw sites).
  The branch is span-level (never in the per-byte loops) — measured flat.
- **`Detach(s, data)` — the `-copy` single-copy detacher.** Returns an owned copy
  of `s` when `s` aliases `data` (the `unsafe.String` clean-path result of
  `String`/`StringAVX*`), else `s` unchanged (a `stringSlow`-owned escape result,
  already detached). One clone, only when needed: the escape arm skips the
  redundant `strings.Clone` that unconditionally cloning every result would pay.
  A `uintptr` pointer-range test (`sp ∈ [dp, dp+len(data))`); sound because Go's
  GC is non-moving and a `stringSlow` scratch is a distinct heap allocation that
  never overlaps `data`. **Tier-agnostic — reuses the aliasing `String`/`StringAVX*`
  tier func directly, so there is NO `StringCopyAVX*` variant family.** Generated
  `-copy` decoders (scalar + SIMD copy fall) and `AnyCopy`/`AnyNumberCopy` call the
  tier func then `Detach`; `EscapeHeavy/ggen_copy` matches the aliasing `ggen` row
  (4 allocs) in both tiers. A zero-length alias returns the detached `""`
  (the empty header still carried a pointer into `data`, pinning the whole
  backing array under GC — reached `-copy` retained values via
  `AnyCopy`/`AnyNumberCopy` keys). Pinned by `TestDetach` (value +
  scribble-survival for aliased & owned inputs, the alloc contract: clone iff
  aliasing, and the empty-alias pointer-range check).
- **`StringAVX`/`StringAVX2`/`StringAVX512` — fused SIMD siblings of `String`**
  (`simd_amd64.go`, `//go:build goexperiment.simd`, `simd/archsimd`). One
  vector pass per 16/32/64 bytes classifies closing `"`, `\`, and control
  bytes simultaneously (fused classify → mask → `ToBits` → `TrailingZeros`) —
  replacing `String`'s three passes (IndexByte ×2 + SWAR ctrl). Instruction
  shape per tier: 16/32-lane ctrl test is `min(v,0x1F)==v` (VPMINUB+VPCMPEQB —
  unsigned `Less` is EMULATED below 512-bit and re-broadcasts 0x80 every
  iteration); the 64-lane variant ToBits each compare (KMOVQ) and ORs in
  scalar registers (512-bit `Mask.Or` round-trips the vector domain via
  VPMOVM2B+VPORD+VPMOVB2M). Both shaves measured −2.9% NoAlloc at avx512. Shared scalar `classifyStructural` tail keeps alias return /
  `stringSlow` handoff / error identity byte-identical to `String` (pinned by
  `TestStringSIMD_Parity`: fixed cases at every vector-phase alignment + 2000
  randomized bodies, all three tiers), including the non-copying
  `stringUnterminated` route for an escaped string with no closing quote
  (`TestStringSIMD_UnterminatedEscaped`: 0 allocations, parity at every lane
  offset). **No tier ever reads past
  `data[len(data)-1]`.** The 16/32-lane tiers take their tail through
  `Load*Part` (zero-fill, composed from scalar element loads); padding zeroes
  register as ctrl bytes, filtered by the `k < len(rest)` position check. The
  64-lane tier does NOT use `LoadUint8x64Part`: go1.27 archsimd lowers it to
  an unmasked full-width VMOVDQU64 plus a zeroing mask-move (`Uint8x64.Masked`
  is documented "Emulated" — a vector-domain AND after the load; the only
  k-masked 8-bit memory op is `StoreArrayMasked`), so it always reads 64
  bytes and faults when the input ends within 63 bytes of unmapped memory —
  mmap'd files at a page multiple, foreign buffers, arena edges; nearly every
  document's LAST string value took that tail. `StringAVX512`'s tail reloads
  the last 64 bytes of `rest` when the body spans a lane (a
  backward-overlapping full load, the encode tiers' idiom — the overlapped
  lanes were classified clean, so their mask bits are zero and the `acc` OR
  is idempotent) and otherwise copies the <64-byte remainder into a zeroed
  stack lane (padding still registers as ctrl bytes, filtered by the same
  position check). Every other 64-lane load (skip/space tiers, the stream
  `structuralIndex*` cores, encode) is bounded `j+64 <= len` or already
  overlapping. Pinned by `TestSIMD_NoOverRead` (`simd_overread_test.go`,
  unix): the parent re-executes the test binary as a child with
  `GGEN_OVERREAD_PROBE=1`, which places every input flush against a
  PROT_NONE page — lengths 0..130 (validators to 260) over every String /
  UTF-8 / skip / space / AppendString tier and the stream cores with a window
  whose capacity ends at the page edge — so a SIGSEGV surfaces as a normal
  test failure carrying the child's trace instead of killing the suite. NO
  runtime feature probing — generated code calls one tier directly (`ggen
  -simd`, see `cli/CLAUDE.md` opt #46); wrong CPU faults. 4.2× vs `String` on
  a 4 KiB clean string, ~1.1× at 8 B.
- **`stringSlow` rejects ctrl bytes in the pre-escape prefix.** The prefix
  `data[start:j]` (escape-free span before the first `\`) is `hasCtrlByte`-
  checked before copying — it used to land in scratch unvalidated, silently
  accepting `"a\x01b\nc"` that stdlib rejects (the no-escape path always
  checked). Stream `(*Stream).stringSlow` has the same guard. Pinned by
  `TestString_CtrlBeforeEscapeRejected`.
- **`stringSlow` copies raw bytes through a WINDOWED inner loop**
  (`escRunWindow` = 16 bytes per trip) with the escape dispatch hoisted into the
  outer loop, so the hot loop body is classify-and-copy and nothing else. Pure
  loop shape — no new kernel, same per-byte work, byte-identical output — worth
  **−5.9% EscapeHeavy / −27.6% EscapeSparse** (bytes; stream −6.0/−30.2%,
  allocs bit-identical). Bulk-copying the run instead (locate with IndexByte,
  `ctrlOrHigh`, one append — the simdjson `parse_string` shape) was implemented
  and measured **+73.6%** on EscapeHeavy, whose runs are 4-5 bytes; a
  length-gated hybrid of the two is worse than the plain window on the dense
  side. See `.claude/backlog.md`. Pinned by `TestStringSlow_RefDifferential`
  (200k randomized bodies vs the byte-at-a-time reference: value, end position,
  error identity, both validate modes).
- **`stringSlow` scratch cap = first-quote span (`closeIdx`)**, NOT the remaining
  payload (a payload-sized cap made M escaped strings allocate O(N·M)). Returns
  `unsafe.String` over write-once scratch — 1 alloc per escaped string. Stream
  `stringSlow` mirrors the alias return (cap bounded at 32). Pinned by
  `TestStringEscapeAllocBounded`.
- **`(*Stream).stringSlow` pairs surrogates through a STAGED peek.** After a
  high-surrogate `\uXXXX` the low-half lookahead runs through
  `(*Stream).ensureSpan(&j, n)` (compacts from `j`, rebases it, returns
  `ReadMore`'s error) in stages — 1 byte to test `\`, 2 to test `u`, then 6
  for the hex quad, and only while `uEscapePrefix` says the buffered tail can
  still become an escape — so every byte awaited is part of the low-surrogate
  escape and a live (never-EOF) reader that delivered the whole string is
  never asked for bytes past its closing quote. A drained reader at any
  stage leaves a lone surrogate exactly as the bytes path does
  (`ErrInvalidUTF8` under validate, U+FFFD otherwise); a transient reader
  error propagates raw. Pulling 6 bytes up front (needed so a pair whose low
  half straddles a refill did not split into two lone surrogates) hung
  forever on `"\ud83d"}` from a live reader — under `validate=false` even
  though the value is legal. All three SIMD `stringView*` cores share this
  `stringSlow`. `ensureSpan` is not inlinable (cost 86), so the `\X` and
  `\uXXXX` arms guard it behind an inline `j+n > len(s.buf)` compare and call
  only at a window edge (the same split as `refillSkip`); the cold surrogate
  arm calls it unconditionally. Pinned by
  `TestStreamStringSurrogateAcrossRefill` (surrogate at every offset × tiny
  bufs vs the bytes path), `TestStreamStringSurrogate_LiveReader` (liveReader
  + timeout, paired control must still assemble 😀) + escape seeds in
  `FuzzStreamEqualsBytes`.
- **`Any`/`AnyNumber` + `AnyCopy`/`AnyNumberCopy`.** `Any(data, i, validate)`
  decodes a value into a Go `any` with stdlib defaults (`null→nil`, bool,
  `number→float64`, `string`-alias, `[]any`, `map[string]any`); `AnyNumber` is
  the `json.Number` variant. `validate` is `String`'s UTF-8 switch applied to
  every string and object key in the value (span-level, the per-byte loops
  never test it); the stream twins `(*Stream).Any(validate)` /
  `AnyNumber(validate)` take the same flag, and generated code passes the
  `vArg` its string scans use, so `allowinvalidutf8` reaches `any` values.
  The `*Copy` siblings (generated bytes-path decoders emit them under
  `-copy`) are byte-for-byte the same walk but detach every string value AND object
  key via `Detach(String(…), data)` (one clone, skipped when the escape arm already
  owns — no `stringSlow`+`Clone` double-copy) and clone the `json.Number` span, so
  the returned tree holds no alias into `data`. Fast path duplicated, not parameterized, to keep `Any`/`AnyNumber`
  regalloc-pristine. Pinned by `TestAnyCopy_ParityAndDecoupled`
  / `TestAnyNumberCopy_ParityAndDecoupled`.

## Stream (`stream.go`)

`Stream` wraps `io.Reader` with a growable buffer (`buf []byte`, grown via
`append`). Cursor = exported `Pos int` — every primitive reads/writes `s.Pos`,
takes no cursor arg, returns none. Capture raw span: `span, err :=
s.CaptureValue()` (returns a buffer alias — copy if retained). The old
`start := s.Pos; s.SkipValue(); s.Bytes()[start:s.Pos]` slice dance is DEAD:
the skip tree compacts unconditionally now, which invalidates `start`.

**Stack-allocatable, no pool.** `var s ggen.Stream; s.Reset(r, buf)`. `Reset`
returns `*Stream`, so it chains into the Stream-taking decode helpers
(`ggen.NewStream(r, buf).Value[T]()`). Caller owns
`buf` lifecycle; `ggen.NewStream(r, buf)` is a heap-allocating shorthand. Old
`Acquire`/`Release` `sync.Pool` removed — it bundled implicit buffer-lifetime
assumptions and caused silent corruption when callers reused buf across decodes
(see backlog).

### Generic Stream methods (stream.go)

Go 1.27 allows METHODS to declare type parameters, so the stream decode entry
points sit on the Stream itself rather than as package functions:

```go
type StreamDecoder[T any] interface{ DecodeFromStream(s *Stream) (T, error) }

func (s *Stream) Value[T StreamDecoder[T]](rcv ...T) (T, error)
func (s *Stream) Slice[T StreamDecoder[T]](rcv ...[]T) ([]T, error)
func (s *Stream) Array[T StreamDecoder[T]](rcv ...T) iter.Seq2[T, error]
func (s *Stream) Seq[T StreamDecoder[T]](rcv ...T) iter.Seq2[T, error]
```

`Reset` returns `*Stream` so the whole thing is one expression:
`ggen.NewStream(r, buf).Slice[User]()`. `NewStream` is the shorthand to reach
for; `Reset` on an existing Stream chains identically when recycling one, but
note its receiver must be addressable (`(&Stream{})` / `new(Stream)`, never
`Stream{}` — `Reset` has a pointer receiver).

`StreamDecoder` is declared HERE, not in `decode`, because the methods constrain
on it and `scan` cannot import `decode` (decode imports scan). `ggen.Decoder`
is the bytes-only counterpart; generated structs satisfy both.

Both methods leave the cursor just past what they read, so the caller keeps
reading — consecutive top-level values, or whatever follows an array. The caller
owns the Stream throughout.

`Slice` mirrors the bytes walker's contracts — non-nil empty slice for `[]`,
`prealloc.Cap` sizing (`internal/prealloc`, one spec shared with the bytes
walker), and
the post-comma `SkipSpace` that scalar/alias element decoders rely on. It does
NOT reject trailing data (probing would block a live reader) and its
bracket/comma errors are bare `scan` sentinels rather than `*ParseError` —
`NewParseErr` lives in decode, out of reach. Element errors arrive already
wrapped from the generated decoder; only the `[N]` index segment is absent.
Pinned by `TestStreamMethods` (`stream_test.go`, over minimal
hand-written `StreamDecoder` types).

**`rcv` buffer reuse.** Both `Value` and `Slice` take an optional receiver to
decode INTO. It works because generated decoders open with `result = recv` and
reset containers keeping capacity (`clear(m)`, `sl = sl[:0]`) — so handing back
a previously decoded value recycles its maps and slices. `Slice` is NOT a blind
append: it truncates the outer slice to keep the backing array AND decodes
element i into `rcv[0][i]`, so each element's own containers are reused too.
Element `i` is read BEFORE the `append` that overwrites that slot, which is safe
only because `result` and `prev` share a backing array — do not reorder. Steady
state is measured at ZERO allocations per decode (`TestStreamBufferReuse`, which
also pins that a no-rcv call stays independent). The receiver only lends its
memory: the result equals a fresh decode of the payload — omitted fields are
zeroed, containers emptied and refilled keeping capacity — which the `Value`
godoc states and `TestMerge_omittedKeysZeroEveryKind` pins through
`Value(rcv)` as well as `DecodeFromStream`.

**`Array` — one array, lazily.** Same grammar and element decoding as `Slice`,
but it yields through `iter.Seq2` instead of accumulating, so a million-element
array costs one element of memory. The range ends when the closing bracket is
consumed and the cursor is left past it, so the Stream reads on; a malformed
array yields one error and stops. Breaking out early leaves the cursor INSIDE
the array — the end position is unknowable without walking it — so an abandoned
Stream is only good for closing. It carries the same one-value reuse and
full-window compaction as `Seq`, which is what keeps a long array through a
small buffer bounded and allocation-free. Pinned by `TestStreamArray`
(Slice agreement, empty array, read-on-after, malformed, non-array, 5000
elements through a 64 B buffer), `TestStreamArrayNoAlloc`, and
`TestArrayBufferStaysBounded` (integrationtests — generated elements, where the
emitted refills are grow-only and the compaction is what holds the bound).

Pick `Slice` when the result is the point (you want the `[]T`), `Array` when the
elements are consumed and discarded.

`Slice` carries the same `len == cap` compaction gate as the two lazy walkers.
It accumulates every element, but the WINDOW still has to drop what it has
consumed: the emitted container refills are grow-only, so without the gate a
long array of container-bearing elements ratchets the buffer (422 B elements
through a 1 KiB buffer reached 4 KiB). Pinned by `TestSliceBufferStaysBounded`.

**`Seq` — unbounded iteration.** `iter.Seq2[T, error]` over consecutive
top-level values (concatenated JSON / NDJSON), running as long as the reader
produces. A drained reader AT A VALUE BOUNDARY ends the range cleanly with no
error yielded; anything else yields one error and stops. It never reads past a
completed value before delivering it — the refill for the next value happens on
the next pull — so a quiet socket cannot stall an element that already arrived
(the same liveness rule as `CaptureValue`). Breaking out of the range leaves the
Stream positioned, so another method can continue from there. It REUSES ONE
VALUE for the whole run — declared before the loop, seeded from `rcv` when
given, and reassigned to each decoded element so the next decode recycles its
containers; a long stream settles at zero allocations per element. The cost is
aliasing: a yielded value is valid only until the next pull, so consumers must
copy anything they retain past the loop body (strings are owned, only
maps/slices alias). The value lives inside the returned closure, so ranging the
same Seq twice starts fresh each time. Pinned by `TestStreamSeq` (multi-value,
blank input, one-error-then-stop, break-then-continue, rcv container reuse, and
a zero-alloc steady-state drain).

**Absolute offset — `Offset()`/`consumed`.** `Pos` is **buffer-relative** — resets
toward 0 as a compacting `ReadMore` (keep > 0) slides the window. Unexported
`consumed int` accumulates every discarded prefix (`+= keep`, or `+= len(buf)` on
full discard), so `buf[0]` always sits at absolute offset `consumed`. `Offset()`
returns `consumed + Pos` = absolute cursor, stable across the whole stream. `Reset`
zeroes `consumed`. Generated decoders use it to
stamp `validation.*Error.Pos` with a full-payload-relative offset (raw `Pos` is
wrong once the window compacts) — see `.claude/validation.md`.

**`ReadMore(keep int) error` — the only I/O primitive.** One Read per call, never
loops. `keep` = lowest offset the caller still needs; bytes before may be discarded:

- `keep == 0` — grow without shift (bigger backing if full). Offsets stable, aliases
  survive.
- `keep == len(buf)` — reset to `[:0]`, refill from 0 (full compaction).
- `0 < keep < len(buf)` — in-place `memmove` `buf[keep:n]` → `buf[0:n-keep]`, read
  into freed tail. **Aliases into buffer invalidated when `keep > 0`.**

**Stateless — Stream carries NO reader state (no sticky error, no EOF flag);
the reader is its own state.** Data+`io.EOF` on one Read delivers the bytes
(nil); the next call re-Reads the drained reader, gets `(0, io.EOF)` again
(io.Reader EOF is stable across the ecosystem), and surfaces
`io.ErrUnexpectedEOF` — pinned by `TestStream_EOFAfterContent` (bytes.Reader +
`iotest.DataErrReader`). A failing reader re-fails on the next call — every
ReadMore error path in every primitive aborts or exits its loop, and the
number-scanner "swallow" (`break` on refill error) only fires with the buffer
exhausted, so the next primitive re-hits the reader; a transient error that
loses no bytes even resumes losslessly (stickiness would have killed a decode
the reader could finish). ReadMore itself surfaces an error only when the
Read made NO progress: bytes delivered alongside an error are appended and
the error deferred to the reader's next call (io.Reader contract — the
number-scanner swallow used to return a TRUNCATED value with fresh digits
sitting unread in the buffer). The old sticky `Err`/`EOF` fields were deleted with
the `Shift` flag — the only cross-refill EOF consumer is `CaptureValue`, which
tracks it as a local (`ReadMore == io.ErrUnexpectedEOF` ⇒ drained).

**Reader errors are never silently swallowed or relabeled.** `ReadMore`
returns `io.ErrUnexpectedEOF` for a drained window and the reader's own error
otherwise, and every refill site now distinguishes the two:

- The number scanners (`Int64`/`Uint64`/`Float64`/`Number`, plus `refillSkip`
  behind `skipNumber`) used to `break` on ANY refill error and return the
  digits scanned so far with a NIL error — `s.Int64()` on `"12345"` with one
  transient reader hiccup returned `1234, nil`. Silent at top level (an alias
  decoder returns `s.Int64()` directly), a bogus grammar error one frame up.
  They now propagate anything that is not `io.ErrUnexpectedEOF`; `skipNumber`
  threads the error out of `refillSkip` and prefers it over `ErrBadNumber`.
  `refillSkip` also returns false immediately once that error is recorded, so
  the fraction/exponent/sign gates cannot issue fresh blocking `Read`s behind
  a reader that already failed — every sibling value scanner returns at once,
  and a `SkipValue` that kept reading defeated deadline-based cancellation.
- The string/key/bool/literal/skip refill arms returned a GRAMMAR sentinel
  (`ErrUnterminated`/`ErrBadString`/`ErrBadBool`/`ErrBadLiteral`/`ErrBadArray`/
  `ErrBadObject`) for a reader hiccup, destroying error identity. All 15 route
  through `notEOF(err, sentinel)`, which keeps a real error and maps only the
  drained case to the sentinel. `NotEOF` is the exported wrapper — generated
  stream decoders' dispatch-loop refills need the same mapping (they returned
  the raw reader error, so a truncated object surfaced `io.ErrUnexpectedEOF`
  where the bytes path reported a grammar sentinel; cli/CLAUDE.md #60).

Pinned by `TestStreamTransientErrorNeverSilentNorMislabeled` (every value
primitive × every byte position, plus a drained-window and grammar-error
control) — it fails 44 ways against the old code.

**Drained-truncation sentinels match the bytes path (2026-08).** Which grammar
sentinel a drained refill maps to is now pinned to the error the BYTES path
returns for the same truncated input — at EVERY refill site, value heads
included: the `Any`/`AnyNumber` walkers' head and container-entry refills
(`ErrUnexpectedEnd` / `ErrExpectString` / `ErrBadArray` / `ErrBadObject` per
site), the null arm (`ErrBadLiteral`, was raw EOF), stream `SkipValue`'s array
entry (`ErrUnexpectedEnd`, was `ErrBadArray`), `stringView`/`KeyView` head
(`ErrExpectString`), and the value-primitive heads that used to return the raw
`ReadMore` error — `Int64`/`Uint64`/`Float64`/`Number` (`ErrBadNumber`, the
`-` arm too), `Bool` (`ErrBadBool`), `ConsumeColon` (`ErrBadObject`),
`skipString` scalar + all three SIMD tiers (`ErrExpectString`). All still
route through `notEOF`, so transient reader errors propagate raw.
`skipObject` reports `ErrBadObject` at ANY key position via an ERROR-PATH
relabel of the key `skipString`'s `ErrExpectString` — sound because
`skipString` returns that sentinel only at the key head (drained window or a
non-quote byte), both of which the bytes twin rejects as `ErrBadObject` —
not a per-key bound check — the pre-check shape it replaced added a compare
to the hot skip loop in all four tiers for an error-identity-only gain. Pinned by `TestBytesStreamTruncationErrorParity` — a bytes-vs-
stream error-identity battery over truncated inputs at chunk sizes 1/3/64,
covering `SkipValue`, `Any`, `AnyNumber`, and `Int64("-")`; the SIMD stream
skip tiers mirror the same mapping (stream-skip parity tests).

**Stream numbers are maximal-munch, like the bytes path.** The refill loop
collects a loose `[0-9.eE+-]` span (it doubles as the extent finder), then
`skipNumber` grammar-checks it — and the GRAMMAR end is authoritative, not the
loose scan's. Erroring when the two disagreed made `1.5.5` / `1e5e` / `01`
reject on the stream where the bytes path returns `1.5` / `100000` / `0` and
leaves the rest to the caller. Pinned by
`TestStreamNumberMaximalMunchMatchesBytes`.

**Aggressive compaction inside Stream methods.** `SkipSpace`, `ConsumeColon`,
`Int64`/`Uint64`, `String`/`KeyView`, `Float64`/`Number` pass non-zero `keep`
(current cursor, or value-start `start` for spans outlasting the loop) so the
buffer stays bounded ~`max(chunk_size, value_size)`; each updates locals after
compaction (`i = 0`, or `j -= start; start = 0` for the string/number body) then
writes final `s.Pos`. **Every ERROR return past a compaction rebases `s.Pos`
too** — it is buffer-relative, so a pre-compaction cursor reads as inflated by
the discarded prefix and can exceed `len(buf)`; generated stream decoders stamp
it straight into `ParseError.Pos`. That includes the five value-primitive
HEADS (`Int64`/`Uint64`/`Float64`/`Number`/`Bool`): a head refill at `Pos ==
len(buf) > 0` (right after a string that ended exactly at the window edge)
takes `ReadMore`'s full-discard branch BEFORE the Read, so the failed-refill
return rebases to 0 rather than doubling `Offset()` (6 on a 3-byte document).
**The stream error position IS the bytes-path error position** — every
stream primitive leaves `Offset()` on the byte its bytes twin returns, at
every chunk size: the number scanners report the stop cursor (after a bare
`-`, at the leading-zero digit, at the `.`/`e`, at the overflowing digit;
`Float64`/`Number` use `skipNumber`'s grammar stop, `s.Pos = start + end`, and
`s.Pos = i` for an empty span); `Bool` the give-up byte (`BoolEnd`, above);
`String`/`KeyView`/`StringView` the control byte for `ErrBadString`, the span
start for `ErrInvalidUTF8`, and `len(buf)` — the end of what arrived —
for `ErrUnterminated` and for a truncated `\X`/`\uXXXX` escape (`stringSlow`
and `skipString`, like the bytes `len(data)`; the truncated high-surrogate
tail lands here too); `SkipValue` the give-up byte
(every `skipNumber` exit writes the rebased cursor, literals via
`skipLiteral` report `litEnd`); `CaptureValue` the bytes skip's give-up byte
(`s.Pos = end` on both final error returns; `s.Pos = 0` only for a transient
error after the compacting refill). Only a transient reader error keeps the
number scanners on the value head (`ReadMore(start)`, spans are bounded at
~20 bytes), which is what makes ReadMore's "a transient error that loses no
bytes resumes losslessly" contract hold for them: a retry re-scans the intact
span instead of a `-` or the digits already folded into the accumulator
having been discarded. Stream `skipString` full-discards windows as it scans,
so it cannot name the span head — which is why the ctrl verdict is reported
at the OFFENDING BYTE: that is the one position both paths can name at every
chunk size (`TestStreamString_ErrorPos` pins the exact offset). Pinned by
`TestStreamErrorPos_MatchesBytes` (every primitive × malformed inputs ×
chunks 1/5/7/64 after a consumed prefix: err identity, `Offset() ==` bytes
pos, `Pos <= len(buf)`, `Offset() <= len(doc)`), `TestStreamNumberLosslessRetry`,
`TestStreamString_ErrorPos`, `TestStreamStringSlow_ErrorPos`, and end-to-end
through generated decoders by `TestParseError_StreamPosChunkInvariant`
(integ). `Float64`/`Number` USED to refill mid-number with grow-only
`ReadMore(0)`; since readers fill the whole window, every mid-number window edge
landed with `len == cap` and DOUBLED the buffer (a 64 B buffer ballooned to 1 MB
on a 50k-short-float stream). They now compact from `start` + rebase `i` like
`stringView` (pinned by `TestFloatNumberBufBounded`) — same class as the skip-tree
compaction fix. The escape decoder `stringSlow` got
the same treatment: it copies into an owned scratch and aliases THAT (not `s.buf`),
so every refill compacts from the cursor `j` + rebases — the `\X`/`\uXXXX`/
surrogate arms through `ensureSpan(&j, n)`, which compacts from the cursor,
rebases and returns `ReadMore`'s error, behind an inline bound check on the
two hot arms (the helper costs 86, not inlinable) and unconditionally in the
cold surrogate arm; the `\uXXXX` arm refills only while `uEscapePrefix` holds
— grow-only ballooned a multi-MB escaped string (a 64 B buffer → 256 KB on a
200 KB escaped string; pinned by `TestStreamStringSlowBufBounded`). `Float64`
also gained the ≤16 B
`exactShort` gate the bytes path has (skips `strconv.ParseFloat`'s re-scan;
bit-identical).

**`CaptureValue() ([]byte, error)` — raw-span capture, no mode flag.** RawJSON
capture, `json.Unmarshal`/`UnmarshalJSON` fallback, and `big.Int` all need the
value's raw bytes contiguous to hand off. Rather than a `Shift` field that flipped
compaction OFF so the stream skip's window could be sliced (the OLD design — a
one-bit mode threaded through ~65 `if s.Shift` rebase gates + 8 codegen save/restore
sites), `CaptureValue` grows the window (grow-only `ReadMore(0)`, which needs no
flag) until the value is whole, then locates its end with the **bytes-path**
`SkipValue` — reused verbatim, so there is NO streaming capture-skip and no new
SIMD skip: tiers are thin wrappers (`CaptureValueAVX{,2,512}`) calling the bytes
`SkipValueAVX*`. It trusts the skip's end when a byte past it is buffered
(`end < len(buf)`), the reader has drained (local `eof`, set when ReadMore
returns `io.ErrUnexpectedEOF`), **or the value is self-delimiting** (first
non-WS byte is not `-`/digit — closing quote/bracket/fixed literal make the
end final even at the window edge); only a NUMBER `123` at the edge refills,
since it could continue `1234`. A skip ERROR is classified rather than
assumed truncated: `SkipValue` preserves the position where it gave up (SIMD
tiers scalar-identical by parity test), and a failure at a byte
STRICTLY INSIDE the window is final — no byte that has not arrived can repair a
malformation sitting before bytes already held. `ErrMaxDepth` is final wherever
it lands (the bracket run may end exactly at the window edge, but no arriving
byte can un-exceed the cap). Only an off-the-end failure keeps reading. Without this a live (never-EOF) reader that had delivered a
complete but malformed value blocked in `Read` FOREVER; pinned by
`TestStreamCaptureValue_LiveMalformedDoesNotHang` plus a truncated-value
control that must still wait. The FIRST refill compacts the consumed prefix (`ReadMore(start)`,
rebase `start = 0`) so the window grows for the value only, never dragging dead
bytes through each doubling; entry deliberately does not compact (the value may
already be fully buffered and ReadMore always Reads — could block a live socket).
Exactly ONE Read per re-skip: once the arrived bytes may complete the value, a
second Read on a momentarily-drained live reader (socket with the value fully
delivered, nothing in flight) would block forever — the old fill-spare-
capacity-then-re-skip loop was that hang. Eager readers (file, bytes.Reader)
fill whatever tail a refill freed: after the first, compacting refill that is
only the `start` bytes the prefix occupied when the window was full, so that
one Read can be tiny and its re-skip wasted before the next refill takes the
doubling arm; from there on their skips land on doubling windows (O(n)). A
short-read regime re-skips per delivery — the price of liveness. On a skip
error the give-up byte is left in `Pos` (`s.Pos = end`), so `Offset()` names
the malformed byte on the stream exactly as the bytes skip does. Returns a
buffer alias (valid until the next Stream op — RawMessage copies
it, `json.Unmarshal`/`SetString` consume it in place). The whole stream skip now
compacts unconditionally — the `if s.Shift` gates are gone, replaced by plain
rebases. Pinned by `TestStreamCaptureValue` (correctness × chunk sizes, trailing
input, prefix-compaction cap bound, EOF errors, live-reader liveness incl. the
number-at-edge exception) + the bytes-vs-stream fuzzer.

**Dispatch-loop shift points.** Generated code adds two: `ReadMore(s.Pos); s.Pos =
0` after `ObjectOpen+SkipSpace`, and after per-iteration value decode + SkipSpace.
Each known-key case opens with `s.ConsumeColon()` — the `KeyView` alias is unneeded
past dispatch, so shift is safe. `UnknownKeyError` + inline-catch-all map key detach
the alias via `strings.Clone(key)`.

**Lazy bounds checks.** Each method bounds-checks itself (`if s.Pos >= len(s.buf) {
… ReadMore(s.Pos) … }`), proceeds once **one** new byte lands. Multi-byte literals
(`true`, `false`, `null`, `\uXXXX`) scan **byte-by-byte**: each char → individual
check + maybe ReadMore, mismatch fails fast without fetching the rest. The
skip tree's literal arms — scalar `SkipValue`'s true/false/null, the three
`SkipValueAVX*` tiers and both `Any` walkers' null arm — share
`(*Stream).skipLiteral(want, sentinel)`, which leaves `Pos` on the give-up
byte like the bytes-path `litEnd` (`len` for a truncated prefix, else the
first mismatch); `Bool` keeps its own loop with the same give-up contract.
(Old `Ensure`/`Anchor`/`Unanchor` bulk-fetch in backlog "Tried Rejected".)

**Inlinable two-tier `SkipSpace`.** A tiny inlinable shell (`if s.Pos < len(s.buf)
&& s.buf[s.Pos] > ' ' { return nil }; return s.skipSpaceSlow()`) over the full loop
in `skipSpaceSlow`. Compact JSON (dominant) returns inline; whitespace/control
bytes/EOF refill hit the slow path. The shell inlines into callers
(`ConsumeColon`/`ObjectOpen`/`ArrayOpen` + generated dispatch), eliding the call +
`s.Pos`/`s.buf` reloads. The exact no-temp shell shape is load-bearing: it inlines
at cost 77 (budget 80); an `i := s.Pos` temp variant costs 81 and does NOT inline.
The budget applies only to callers under 5000 IR nodes: gc classifies larger
functions as "big" and inlines only callees costing ≤ 20 into them
(`inlineBigFunctionNodes`/`inlineBigFunctionMaxCost`, cmd/compile
inl.go), so inside the six largest generated decoders — Mega's
`Node`/`CopyNode` `decodeFromDepth` + `decodeFromStreamDepth`, NoAlloc's
`Account`/`CopyAccount.DecodeFrom` — this shell (77), `Bool` (54), `Detach`
(66) and `StringView` (71) are real CALLs while `Offset` (6), `Bytes` (3) and
`NotEOF` (8) still inline; every other generated file inlines 100%. A CALL
node costs 57 in the inliner's model, so no shell containing a call can ever
get under 20 there. The backlog's stream-tier rejections (the `_s.SkipSpace`
inliner, the window-gated int loops, `ggen.Bool` inlining) predict flat for
emitting a guard into those decoders — do not chase it without a house-rule
A/B. Slow path stays byte-exact, including the EOF-returns-nil-at-`Pos==len`
quirk generated code relies on.

**Buffer-header hoist in refill loops.** `skipSpaceSlow`, `Int64`, `Uint64`,
`Float64`, `Number` hoist `buf := s.buf` and run a nested loop (`for i < len(buf)`)
so the hot scan compares against a registerized `len(buf)` instead of reloading the
`s.buf` header through the `*Stream` pointer each iteration; refill (outer loop)
reloads `buf = s.buf` after the compaction + `i = 0` rebase.

### Stream copies vs bytes-path aliases

`Stream.String` + `Stream.Number` return **owned copies** — safe as map keys,
slice/struct string fields, output detached from buf. `Stream.String` is a thin
shell over the internal `stringView() (v string, owned bool, err error)`: happy
path alias→clone = one copy (same as old `string(s.buf[start:end])`); escape path
returns the already-owned `stringSlow` result directly (`owned=true`, no second
clone). `StringView` drops the flag. Bytes path still aliases caller-owned input
directly.

**`StringView` — alias for transiently-consumed value strings.** Sibling of
`Stream.String`/`KeyView` returning an `unsafe.String` alias into `s.buf` (no scalar
prelude — value strings are often long, where SIMD `IndexByte` wins). Escapes fall
back to `stringSlow` (owned copy). Generated decoders call it where the value string
is **fully consumed before the next stream op AND retains none of its bytes past that
point**:

- base64/base32/hex `[]byte` (decoded into independent dst by `AppendDecode`)
- `time.Time`/`time.Duration` text formats (parse → value; error builds fresh string)
- `net.IP` (`ParseIP` copies; the `&net.ParseError{Text:…}` error literal
  detaches via `strings.Clone(sv)` — `string(sv)` of a string is an identity
  conversion, NOT a copy, a round-9 find)
- `netip.Addr`/`netip.Prefix` (value types, zones deep-copied by `unique`;
  the error branch re-parses a detached `strings.Clone(sv)` like `net.IP`'s
  clone, because `parseAddrError`/`parsePrefixError` retain the input string
  and quote it in `Error()` — pinned by `TestNetip_ErrorDetachedFromBuffer`)
- `big.Float`/`big.Rat` (parse into receiver, like `big.Int`'s span alias)
- cross-pkg `TextUnmarshaler` (encoding contract forbids retaining the arg)

An ERROR value must not retain the alias either: the buffer is recyclable the
moment decode returns (the documented pooled-buffer usage), so a message that
quoted the alias mutated once the buffer was reused. **NOT** for `url.URL` (`url.Parse` slices `Path`/`RawQuery`
out of input → stored value would alias `s.buf`), plain `string` fields, map keys, or
map/slice string elements (all outlive the scan → `Stream.String` copy). Body is a
near-duplicate of `Stream.String`'s scan (alias vs copy) — keep in sync.

**`KeyView` — alias for dispatch keys.** Sibling of `Stream.String`, aliases via
`unsafe.String(unsafe.SliceData(s.buf[start:]), end-start)` on the happy path, else
`stringSlow`. Used in generated object-field dispatch where the key is read, matched,
discarded — alias never escapes the dispatch frame. Drops ~200 throwaway heap strings
per decoded value to zero. The alias survives buffer growth (GC pins the old backing),
but a non-zero `keep` in a subsequent `ReadMore` WILL move bytes + corrupt live
aliases (see Aggressive compaction); dispatch sites detach via `strings.Clone` before
any shift-triggering path (UnknownKeyError, inline-catch-all map key).

Scalar prelude: `KeyView` opens with a bounded scalar pass over the first
`stringPreludeWindow` (24) buffered bytes that finds the closing quote while
validating backslash/control in one loop — sparing the two `bytes.IndexByte` setups
that lose to a scalar loop on the short spans dispatch keys occupy. A key longer than
the window, an escape, or a not-yet-buffered quote falls through to the `IndexByte`
loop, which RESUMES at the window end (`j = we`) so the validated prefix is never
re-scanned. Error identity byte-identical. NOT applied to `Stream.String`.

### Stream SIMD tiers (`simd_stream_amd64.go`, `//go:build goexperiment.simd`)

The bytes-path contiguity precondition fails across refills, but each buffered
WINDOW is contiguous — so the stream tiers keep `stringView`'s
refill/compaction loop bit-identical and only swap the three-pass locate
(IndexByte ×2 + `hasCtrlByte` SWAR) for one fused pass: per-tier
`structuralIndexAVX{,2,512}(b) int` returns the first `"`/`\`/ctrl index (or
-1 → refill), classified by a switch identical to the scalar arms. Per tier:
a `stringView*` core (three near-identical copies — the tier callee must be a
direct call, no func-pointer dispatch) + thin `String*`/`StringView*`/
`KeyView*` shells with the scalar trio's exact contracts (owned copy / alias /
alias). `KeyView*` drops the scalar prelude — ggen keeps plain `KeyView` for
all-short-key structs (same ≤5-byte gate as the bytes-path key window) and
swaps call NAMES at generate time otherwise (`tierStreamStringCalls`, an
assignment-shaped rewrite over the stream-decode body only). Measured at
avx512: Mega_Reader −5.2%, NoAlloc_Reader −7.7%, Small_Reader −20/−26%.
Parity pinned by `TestStreamStringSIMD_Parity` (lane-seam bodies × chunked
readers 1..64 B forcing mid-string refills, all tiers, error identity).

**Refill error identity (found+fixed 2026-08, round 7).** All three
`stringViewAVX*` cores returned the RAW `ReadMore` error at the head refill
(scalar: `NotEOF(err, ErrExpectString)`) and mapped EVERY mid-string refill
failure to `ErrUnterminated` (scalar: `NotEOF(err, ErrUnterminated)` — a
transient reader error was relabeled as malformed JSON). The parity test only
feeds complete payloads, so both slipped through; now routed through `NotEOF`
like the scalar path and pinned by
`TestStreamStringSIMD_RefillErrorIdentity`. The cores also mirror the scalar
core's four `s.Pos` writes on error exactly — head refill failure `i = 0;
s.Pos = i`, non-quote head `s.Pos = i`, mid-string refill failure `s.Pos =
len(s.buf)`, `ErrBadString`/`ErrInvalidUTF8` `s.Pos = start` — so `Offset()`
stays rebased after a compacting `ReadMore` (without the writes an
unterminated 106-byte string reported 11 where scalar said 106, and every
`-simd` stream decoder stamps that into `ParseError.Pos` via
`tierStreamStringCalls`). Pinned by `TestStreamStringSIMD_ErrorPos`
(String* tiers × malformed/truncated bodies × chunk 5/7/64 vs scalar
`Offset()`, sibling of `TestStreamSkipStringSIMD_ErrorPos`).

### Skip-tree SIMD tiers (`simd_skip_amd64.go`, `//go:build goexperiment.simd`)

`SkipValueAVX{,2,512}` + `skipArray*`/`skipObject*`/`skipString*`/`SkipSpace*`
— per-tier copies of the scalar skip tree whose only changes are (a)
`SkipSpace*`: an **inlinable shell** (`if i >= len || data[i] > ' ' { return i }`)
over a cold vector body (`skipSpaceAVX*Slow`) — scalar first-byte exit, then
whole-lane whitespace classify (`eq ' '|'\t'|'\n'|'\r'` → first zero bit = first
non-WS) — indent runs in pretty-printed JSON skip a lane at a time instead of
byte-stepping. The shell splits so the early-out inlines into the tree's 8
`SkipSpace*` call sites per tier: on compact (whitespace-free) JSON that early-out
was 22% flat of SkipHeavy/compact as a non-inlined call + prologue + ret (the
vector body unreachable there yet blocking inlining); splitting it recovered
−9.7% on the compact ggen row (avx512, interleaved n=8). (b)
`skipString*`: fused `structuralIndex*` locate + shared `skipStringTail`
escape switch (identical to scalar skipString's, `uEscapePrefix` gating
included). Error identity scalar-exact: a ctrl hit is `ErrBadString` at the
scan cursor whatever follows, as in scalar `skipString` (see the
error-position contract). ggen swaps `ggen.SkipValue` → tier in bytes decode bodies
and emits a guarded `ggen.SkipSpace*` handoff in `inlineSkipWS` (one WS byte
consumed inline so compact/single-space payloads stay call-free). Measured
at avx512 (SkipHeavy bench): compact −21.6%, pretty −29.9%; pretty
full-decode −3.3% from the inlineSkipWS handoff alone; Mega another −8.3%
(RawJSON capture rides the tier). Pinned by `TestSkipValueSIMD_Parity`
(truncations at every byte + malformed mutations + indent widths at lane
seams) and `TestSkipSpaceSIMD_Parity` (every run length 0..130).

### Stream skip tiers (`simd_skip_stream_amd64.go`, `//go:build goexperiment.simd`)

`(*Stream).SkipValueAVX{,2,512}` + `skipArray*`/`skipObject*`/`skipString*`/
`SkipSpace*` — the stream mirror of the bytes skip tier: per-window
`structuralIndex*` locate in skipString (escape/refill arm factored into
`skipStringStreamTail`, cursor rebase identical to scalar), vector
whitespace-run skip in `SkipSpace*`'s slow path (same inlinable shell shape
as scalar `SkipSpace`), refill/compaction copied bit-exactly. Generated
stream decoders swap `= s.SkipValue()` / `= s.SkipSpace()` / `= s.CaptureValue()`
to a tier (`tierStreamStringCalls`). Measured at avx512: SkipHeavy ggen_stream
compact −32.6%, pretty −25.6%; Mega_Reader flat (copy mallocs dominate).
Pinned by `TestStreamSkipValueSIMD_Parity` (chunked readers ×
truncations at every byte), plus `TestStreamSkipStringSIMD_ErrorPos` and
`TestStreamCaptureValueSIMD_MaxDepthDoesNotHang` for the error-position
rebase and the `ErrMaxDepth` finality the tiers mirror from the scalar tree.

**Correctness fixes (found+fixed 2026-08, audit round 4) — the tiers now
actually match the scalar contracts this section already documented:**
`CaptureValueAVX{,2,512}` were missing the malformed-inside-window finality
check (see `CaptureValue` below) entirely — on any tier-skip error with the
window not yet drained, they went straight to `ReadMore` instead of
classifying the failure first, so a complete-but-malformed value on a live
(never-EOF) reader blocked in `Read` forever; they now bail the same way the
scalar path does, reading the give-up position the tier skip preserves
(`end < len(s.buf)` ⇒ final) — no re-walk.
`skipSpaceSlowAVX{,2,512}` rebased `s.Pos = i` after a compacting
`ReadMore(i)` instead of `s.Pos = 0` — `i` was the OLD (pre-compaction)
buffer length, a stale index once `ReadMore` truncated `s.buf` to `[:0]`
(`ReadMore` always fully discards here since `keep == len(s.buf)` exactly at
every call site), overcounting every subsequent `Offset()`. And the ~15
refill sites across `skipStringStreamTail`/`skipString*`/the literal arms/
`skipArray*`/`skipObject*` returned a bare grammar sentinel
(`ErrBadString`/`ErrUnterminated`/`ErrBadLiteral`/`ErrBadArray`/
`ErrBadObject`) on ANY `ReadMore` failure — including a transient reader
error unrelated to malformed JSON — instead of routing through `notEOF`
like the scalar tree already does (see "Reader errors are never silently
swallowed or relabeled" above); they now do.

**Skip-tree compaction (scalar + tiers, shipped with the tier).** The skip
tree used to refill with grow-only `ReadMore(0)`, and readers fill the whole
window, so every refill landed with `len == cap` and DOUBLED the buffer —
skipping a 5.9 MB blob through a 4 KiB stream buffer allocated 8.4 MB/op.
Skipped bytes are discardable, so every skip refill now compacts:
`SkipValue`/`skipArray`/`skipObject` bound checks pass `s.Pos` (== len(buf)
⇒ free full-discard, no memmove) + `s.Pos = 0` rebase; `skipString`'s
clean-window refill full-discards (`ReadMore(len(buf))`); `skipNumber`
refills via `refillSkip(i, rerr)` (cursor and recorded error passed and
returned BY VALUE, so the digit loops keep the cursor in a register) — the
hot `i < len(s.buf)` bounds check stays
inline, the cold helper compacts + rebases (a pointer-arg `hasByteAt`
variant broke inlining and cost +40% — the split is load-bearing), and
EVERY `skipNumber` exit writes the rebased `i` back to `s.Pos` (its error
returns used to leave the stale pre-compaction cursor, so generated
`ignoreunknown`/`allowdups` decoders stamped `ParseError.Pos` past the end of
the document — 35/36 on a 20-byte doc — for a malformed number under an
ignored key straddling a window edge).
8.4 MB/op → 127 KB/op, 12 → 6 allocs. NOTE: raw `Pos` across a `SkipValue`
is buffer-relative (like SkipSpace/Int64) — use `Offset()`;
`TestSkipNumber_StreamMatchesBytes` asserts via Offset. (RawJSON capture no
longer rides the stream skip — see `CaptureValue`, which uses the bytes skip.)

**Stream `skipObject` comma-WS bug (fixed 2026-07).** The comma branch did
`s.Pos++; continue` straight into the next key's skipString — no separator
whitespace skip (the bytes mirror always had `SkipSpace(data, i+1)`), so
skipping any pretty-printed object with 2+ keys failed `ErrExpectString`.
Never surfaced: stream skip had only ever run on compact payloads, and the
tier parity tests replicated the scalar bug faithfully. Caught by the
SkipHeavy pretty stream row; pinned by `TestStreamSkipValue_MatchesBytes`
(stream-vs-BYTES differential over indented objects + truncations — parity
against the scalar stream alone cannot catch shared bugs).

### `hasCtrlByte` — SWAR control-byte validation

Every string scanner (`ggen.String` bytes; `StringView`/`KeyView`/`skipString`
stream) validates a span carries no unescaped control byte (`< 0x20`, illegal in a
JSON string) after `bytes.IndexByte` locates the closing quote + backslash. Shared
`hasCtrlByte(b []byte) bool` (scan.go) scans **8 bytes/iteration via SWAR** — hasless
trick `(x-0x2020…20) &^ x & 0x8080…80` (a lane below `0x20` borrows into its high bit,
`&^ x` clears UTF-8 bytes `>= 0x80`, `& high` keeps the per-lane flag; non-zero ⇒
control byte present). A scalar tail handles the final `< 8` bytes. `KeyView`'s scalar
prelude (short keys `≤ stringPreludeWindow`) untouched; SWAR runs only post-prelude.

Conservative tier only — SWAR replaces the _control-char_ pass; quote + backslash
locate stay on SIMD `bytes.IndexByte` (folding backslash into SWAR loses to AVX2
`IndexByte` on long spans — rejected). Byte-identical: error identity (`ErrBadString`) +
`stringSlow` handoff at first `\` unchanged; pinned by
`TestHasCtrlByte_DifferentialExhaustive` (control byte at every position × word-phase
alignment vs naive loop) + `_Boundary` (0x1f vs 0x20 at the lane seam).

### `skipString` — bounded backslash probe

All three string scanners (`String`, `KeyView`, `skipString`) bound the backslash
IndexByte to the closing quote; a whole-tail probe runs only when the quote is not
yet buffered. `skipString` once probed the full tail per skipped string — `SkipValue`
went O(payload²) when the buffer held the whole payload (a large fully-buffered value).
The bound restores linearity; allocs identical.

## Design rationale

**`unsafe.String` aliases are safe across buffer growth.** Go GC is non-moving
(mark-and-sweep, no compaction); an alias into the OLD backing keeps it live (GC walks
string headers), so `Stream` can `append`-grow freely. One exception: a `keep > 0`
compacting `ReadMore` memmoves on the same backing.

## Tests

- `any_test.go` — `Any`/`AnyNumber` stdlib parity, `AnyCopy`/`AnyNumberCopy`
  parity + input-decouple checks, + `BenchmarkAny_Shapes`.
- `string_test.go`, `number_test.go`, `stream_test.go` — primitive
  correctness + edge cases; string_test also carries the `stringSlow`
  reference differential, the SWAR control-byte probes and the malformed-tail
  finality pins, number_test the skip-number grammar, the `Float32`
  differential and float-form benches, stream_test the bytes-vs-stream
  truncation parity, the bytes-vs-stream error-POSITION differential
  (`TestStreamErrorPos_MatchesBytes`) and the live-reader liveness pins
  (surrogate, malformed tail).
- `decode_test.go` — walkers, parse-error shapes, min-alloc pins, depth cap,
  `BoolEnd`.
- `rfc3339_test.go` — `ParseRFC3339`/`AppendRFC3339` jsonv2 accept/reject parity.
- `simd_test.go`, `simd_skip_test.go`, `simd_utf8_test.go` — per-tier parity
  and benches, one file per kernel family (goexperiment.simd); simd_test also
  carries the stream string tier error-position pin.
- `simd_overread_test.go` — the guard-page no-over-read probe over every tier
  (goexperiment.simd && unix, child test binary).
