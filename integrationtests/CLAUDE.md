# integrationtests — feature / roundtrip / compat / fuzz tests

Separate Go module (`go.mod` with `replace github.com/sirkostya009/ggen => ../`).
Imports root packages as an external consumer, so tests hit the public API at
the boundary users see.

## Generated files & the `//go:generate` workflow

Each annotated source carries `//go:generate ../ggen $GOFILE`, emitting sibling
`<file>_ggen_test.go`; build tags propagate. Files behind opt-in tags (e.g.
`//go:build ggen_brokencodegen`) are skipped by default
`go generate` — pass `-tags=…`. Regenerate:

```sh
(cd integrationtests && go generate ./...)
```

Cross-file struct refs work first run: single-file mode seeds the known-types
set with every annotated name in the package, so codegen emits a direct
`Address{}.DecodeFrom(...)` not an encoding/json fallback.

## Test files

- `shared_test.go` — shared annotated structs (Address, Node, …) used across feature tests.
- `payloads_test.go` — `complexPayload`/`complexValue` (roundtrip/stdcompat); `megaPayload`/`megaValue` (1 MiB generated Node tree, fixed seed 1; used by `stdcompat_test.go` at scale).
- `<file>_ggen_test.go` — generated methods, one per annotated source.

Per-feature coverage:

| File                    | What it covers                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| ----------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `alias_test.go`         | Top-level type aliases — primitive, struct, slice/map/array, `[]byte`, `[N]byte` (base64 + strict length, same wire as a `[N]byte` field), leading whitespace across every alias shape, struct delegation tiers, single-alloc float budget, and element structs reached only through a container alias. Plus NAMED PRIMITIVES (`NamedPrims`, annotated and not): every rule resolves the underlying kind and casts through it — `oneof`, `eq`/`neq`, rune/substring/charset rules, `nullzero`. `R10IntroAlias`: an alias over an unannotated struct takes field introspection and resolves the underlying's `@Func` mods / validators AND `@Conv` variants in the ALIAS's package. `AliasTagsHost`: an omitted field of a named container type decodes to nil, like a plain slice. |
| `any_test.go`           | `any` / `interface{}` fields; usenumber mode. `R10DurationAny`: a value whose POINTER type carries the marshaler (`big.Rat`/`big.Float`) still marshals through it inside an `any`, directly or nested, and a `time.Duration` in an `any` emits the same units string a `Duration` field does (jsonv2 parity). |
| `copy_test.go`          | `-copy` / `//ggen:generate copy`: bytes-path decode copies strings / slice elems / map keys+values / `json.RawMessage` / any-embedded strings / `url.URL` components out of the input. Scribbles the source buffer after decode and asserts retained values survive (`AliasDoc` is the negative control proving the scribble is effective); deep-tree fingerprint check.                                                                                                                                       |
| `custom_test.go`        | `@FuncName` / `@pkg.FuncName` validators and mods.                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `decode_dups_test.go`   | `allowdups`: first-wins, `SkipValue` advance for later occurrences vs default `DuplicateKeyError`.                                                                                                                                                                                                                                                                                                                                                                                 |
| `dive_test.go`          | `inner:` / `keys:` pipe prefixes on slices / arrays / maps.                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `extra_test.go`         | Misc edge cases not fit elsewhere. `TestTuple_ZeroLength` (`R10ZeroTuple`): `[0]T` marshals as `[]` and decodes only `[]`. `TestTuple_ZeroLengthContainers` (`R10BZeroTupleContainers`): the same in every container position — slice element, array slot, map value, `map[string][0][N]T` — where the value emit names no loop variable and the decode has no slot to write. `TestR10WideBounds`: numeric bounds parse at the field's width and sign (a `uint64` bound above 2^63; distinct `int64` `oneof` parts above 2^53). `TestTuple_StrictTooMany` / `…TooFew` pin tuple overflow as a lower bound (`LenError{Want: N, Got: N+1, AtLeast: true}`, message "at least") and underflow as an exact count, on both paths; `maxlen=MaxInt64` compiles and keeps the width-default cap. `R10BParenTypes`: a field type written with the redundant parentheses gofmt keeps (`[]([]bool)`, `*(int)`, `([]byte)`) decodes and marshals as the bare type. |
| `fallback_test.go`      | `encoding/json` fallback for cross-package non-annotated types. Plus `CrossPkgShapes`: a cross-package GGEN type in every field position (value / slice / map / `*T` / `**T` / `[]*T` / `[N]T`) — pins both the foreign-import collection and the AppendJSON rung. `TestCrossPkg_unmarshalJSONRungRebasesPos` (`R10WrapHost`): an error from a nested decode reached through the `UnmarshalJSON` rung is rebased onto the payload. `TestSamePkgCodec_MethodsHonoured` (`R10TextCodec`/`R10JSONCodec`): a SAME-package unannotated struct with its own text / JSON codec pair is never generated as a dependency — the host calls its methods, like a foreign type (jsonv2 parity). |
| `hooks_test.go`         | Opt-in `MarshalJSON` / `UnmarshalJSON` hooks (`-marshal` / `-unmarshal`).                                                                                                                                                                                                                                                                                                                                                                                                          |
| `htmlescape_test.go`    | Literal default (jsonv2-shaped) + `htmlescape` opt-in (v1-shaped).                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `embed_test.go`         | `json:",embed"` catch-all map — unknown keys absorbed, overrides `ignoreunknown`, spliced out on marshal.                                                                                                                                                                                                                                                                                                                                                                         |
| `keyescape_test.go`     | Statically JSON-escaped wire-key name constants: quote-bearing name (jsonv2 byte parity), htmlescape name with `&` (v1 parity), backslash-bearing name (valid JSON + self-round-trip; name-spelling divergence vs jsonv2 pinned in backlog). |
| `jsonsize_test.go`      | `JSONSize()` worst-case upper-bound. **Houses every `JSONSize` cap-guard regardless of feature** — see below.                                                                                                                                                                                                                                                                                                                                                                      |
| `maps_test.go`          | String-keyed maps; key validators / mods; deep map values. `NestedMaps`: map-of-map at every value shape — each nesting level names its own key/value locals, bytes + stream; `TestNestedMaps_innerValuesRecycled` pins that a carried inner map keeps a returning key's slice backing on the bytes path while an omitted inner key does not survive. `NamedValMaps`: named-primitive map values marshal via the primCast'd ref (round-trip).                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `merge_test.go`         | Decode-into-receiver: omitted keys zeroed (every kind, both paths), slice `[:0]` reuse, map `clear()` reuse, carried slice-element and map-value allocations reused — struct/slice/map/`[]byte`/pointer values, plus `json.RawMessage` under `-copy` (bytes path; the stream path still clears), `null` → nil, `[]`/`{}` on non-nil vs nil receiver. **Contract pin.** Round 10 adds `TestMerge_crossPkgFallbackDecodesFresh` (a field decoded through `UnmarshalJSON` / `UnmarshalText` / `encoding/json` is ZEROED before the call at every position — value, pointer, swapped map pointee, fixed-array slot — so the merging fallback cannot resurrect an omitted key) and `TestMerge_pointerContainerLeavesReset` / `TestMerge_arrayContainerSlotsReset` (a slice/map leaf carried anywhere other than a top-level field — pointer map value, `[]**T` element, `*[N]map`, `[N][]byte` slot — is emptied before it refills; only the backing is recycled). |
| `mods_test.go`          | `pipe:` transforms — trim/tolower/toupper/trimleft/trimright/replace/clamp/`@Func`; islower/isupper case validators. `TestMods_pointerPipeDeclaredOrder` (`R10PtrPipeOrder`): value steps on a POINTER field run in DECLARED order, like on a value field. `TestMods_pointerCustomStepDeclaredOrder` (`R10BPtrPipeOrder` vs the value twin `R10BValPipeOrder`, plus a multierr carrier): a pipe INTERLEAVING `@Func` steps with built-ins keeps that order too — the `@Func` steps take the `*T`, the built-ins the pointee, and a `null` still reaches the `@Func` steps. `TestBigUint64Bounds_reportedExactly` (`R10BBigBounds`): a `gte`/`multiple`/`eq` bound above float64's exact integer range is reported exactly — `GTEError.Limit`, `MultipleError.Of` and `EqError.Want` carry the field's own kind, not a rounded float. |
| `native_test.go`        | `time.Time`, `time.Duration`, `net.IP`, `netip.Addr`/`Prefix`, `[]byte` encodings. `TestByteArray_TupleOfByteSlices` (`R10ByteSliceTuple`): a `[N][]byte` is a tuple of base64 strings, not N fixed-length ones. `TestByteArray_Pointer` (`R10BPtrByteArray`): a `*[N]byte` / `**[N]byte` is the encoded array behind a nullable rung, `format:` included. `TestNetTypes_emptyIsZero` (`R10NetZero`): zero `netip.Addr`/`Prefix` and nil `net.IP` marshal as `""`, and `""` decodes back to the zero value (`null` → nil for `net.IP` only). `TestNetip_ErrorDetachedFromBuffer`: a netip parse error retains its input, so the stream clones it off `s.buf`. `TestTime_RFC3339StrictParity` (`R10Time`): strict RFC 3339 both ways on the default / `RFC3339` / `RFC3339Nano` layouts (jsonv2 parity). |
| `omit_test.go`          | `omitempty` / `omitzero`. `TestOmitEmpty_JSONEmptyKinds` (`R10OmitEmpty`) pins the JSON-EMPTY rule (`null` / `""` / `[]` / `{}`) on the kinds whose empty encoding is not Go-zero-shaped — text kinds, `any`, pointers to empty values, `[0]T` — plus `omitzero` on a `[N]byte`; `In *R10OmitInner` is the struct case, where `omitempty` omits on nil alone and a non-nil pointer writes `{}` (the option is a generate-time error on a struct FIELD, pinned in cmd/ggen by `TestOmitEmptyOnStructField`). `TestStringTag_quotedTextTakesNumberGrammar`: the quoted text of a `,string` field takes the bare JSON number grammar, range and narrowing checks. |
| `pointer_test.go`       | `*T` — null ↔ nil, primitive + struct pointees, `[]*T` slab. `PointerStruct` composes per-pointee-kind single-level fields (`PtrName`/`PtrCount`/…); `NPtrStruct` embeds per-depth `**T`/`***T`/`****T`/`**struct`, all native; `NPtrContainersStruct` covers multi-level pointers in containers (`[]**T`, `[3]**T`, `[][]**T`) and pointer map values (`map[string]*T`/`**T`/`*Address`). Per-field JSONSize cap-guards in `jsonsize_test.go` (`PtrFieldPerKind`/`NPtrPerDepth`). `PtrContainers` covers POINTERS TO CONTAINERS at any depth (`*[]T`, `***[]T`, `*map`, `***map`, `**map[string]struct`): the receiver reset reaches through every level (a reused receiver replaces, never appends) and the element kind resolves past depth 1. |
| `read_test.go`          | Basic Read + unknown-key error & ignoreunknown opt-in. `TestRead_unknownKey_streamParity`: `UnknownKeyError.Pos` is the VALUE head on both paths, a key with no colon is the grammar error rather than an unknown key, and the multierr aggregate carries the same Pos. |
| `richtypes_test.go`     | UUID, decimal, big.Int/Float/Rat, sql.Null\*, json.RawMessage, jsontext.Value, url.URL. `RawOnly` isolates a raw-span field: a reused receiver's backing is refilled, so steady-state stream decode is allocation-free.                                                                                                                                                                                                                                                                                                                                                                                            |
| `roundtrip_test.go`     | Symmetric marshal → unmarshal → marshal stability.                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `scan_decode_test.go`   | Bytes-path + stream-path correctness (chunked reader, tiny-hint-forces-grow). `TestNarrowIntOverflow` (`NarrowInts`): narrow int/uint fields/map/slice/pointer reject out-of-range values (ErrNumberOverflow) matching encoding/json, bytes + stream — pins the codegen overflow guard vs the old silent truncation. `TestParseError_SliceStructural`/`TestParseError_ScanPrimitivePos`: bytes-path slice structural errors wrap in `*ggen.ParseError` (path + pos) and scan-primitive error positions surface end-to-end, bytes + stream. `TestParseError_StreamPosChunkInvariant`: **`ParseError.Pos` on the stream path EQUALS the bytes path's position — the give-up byte, where scanning stopped — and the sentinel matches, at every chunk size.** Stream scan primitives rebase `s.Pos` on every error return, so a compacting refill cannot leave a stale cursor behind. `TestParseError_CtrlInOpenTailParity`: a ctrl byte in a string that never closes is `ErrBadString` at the same offset on both paths. `TestParseError_BoolGiveUpPos` (`R10BoolShapes`): every generated `Bool` site stamps the give-up byte (`ggen.BoolEnd`) at field / slice / array / map positions. `TestTruncationSentinelParity_WireShape`: the value-head refill sentinel follows the WIRE shape, not the Go kind (pointer leaf, `sql.Null*` inner, a `,string` quote, a `format:array` bracket). `TestNullPeekSentinelParity`: an `n` that does not spell `null` is `ErrBadLiteral` at every null-accepting site. `TestNarrowFloat_RoundsDecimalOnce`: `float32` rounds the decimal ONCE at every site (field / slice / map / pointer / `,string`), bit-identical to both stdlibs.                                                                                                                                                                    |
| `seq_growth_test.go`    | Stream window bounds over long runs of GENERATED container-bearing values, whose emitted refills are grow-only: `Seq`, `Array` and `Slice` each drop consumed bytes once the window is full, so the buffer cannot ratchet.                                                                                                                                                                                                                                                                                                       |
| `sql_test.go`           | `database/sql.Null*` family wire shape. `TestSQLNull_NarrowOverflow`: narrow inners (`NullInt16` / `NullInt32` / `NullByte`) reject out-of-range values with `ggen.ErrNumberOverflow` on both paths instead of wrapping. |
| `stdcompat_test.go`     | Exhaustive ggen ↔ jsonv2 round-trip; re-marshaled via jsonv2, compared as parsed `any` (map order + nil/empty-slice noise normalized). Plus `exactWire` byte-identical checks (`F64Wire`/`F32Wire`/`AnyWire`) for shapes `crossCompat` normalization masks — float formatting (`1e+06` vs `1000000`) and any-string HTML escaping; single-field/single-key carriers keep order deterministic vs jsonv2.                                                                            |
| `allowinvalidutf8_test.go` | `-allowinvalidutf8` / the per-struct annotation: invalid bytes pass into string fields, keys and raw spans, unpaired surrogates → U+FFFD. `TestAllowInvalidUTF8_anyValues` (`R10PermissiveAny`) extends that to `any` values (float64 and usenumber shapes), `map[string]any` values and the `json:",embed"` catch-all, with the strict struct as control. |
| `nullzero_test.go`      | `nullzero` as a tag variant and as a whole-struct annotation, bytes + stream: `null` → zero value, strict rejection without it, validation still runs on the zeroed value. |
| `variants_test.go`      | `pipe:` decode-stage variants — `@Conv` converters selected by wire shape, element interleaving, bytes + stream, converter errors carrying path + pos. `TestVariants_R10ConverterInputs`: a converter INPUT resolves like a field of that type — a pointer input takes the pointer scan (so the variant claims `null` and is handed `nil`) and an unannotated named primitive resolves through its underlying kind. `TestVariants_pointerFieldValueSteps` (`R10BConvPtr`): a POINTER field with shape dispatch runs its value steps split by target too, the built-ins under a nil guard. |
| `whitespace_test.go`    | Whitespace tolerance around every structural position. |
| `wire_test.go`          | Wire-format fixtures for divergence-from-stdlib types (`url.URL`, `sql.Null*`).                                                                                                                                                                                                                                                                                                                                                                                                    |
| `fuzz_test.go`          | Fuzzers over `Node`, `BoundaryStruct`, `HugeStringStruct`, `PrimStruct` — see Fuzz section.                                                                                                                                                                                                                                                                                                                                                                                        |
| `brokencodegen_test.go` | Opt-in (behind `ggen_brokencodegen` tag) — codegen regressions worth pinning even when broken.                                                                                                                                                                                                                                                                                                                                                                                     |

### Shared decode helpers

Bytes-vs-stream parity assertions go through the helpers in
`scan_decode_test.go`, which decode ONE payload on both paths and hand back
both errors so a test compares sentinel and `Pos` directly:

- `decodeBothPaths[T]` — bytes `DecodeFrom` + a stream over the whole payload.
- `decodeBothChunked[T](payload, chunk)` — same, with the stream fed `chunk`
  bytes per `Read` (`chunkReader`) through a 64-byte window, so the window
  compacts mid-value and a stale cursor would show as a chunk-dependent `Pos`.
- `decodeBothPathsChunked[T](payload, chunk)` — the 16-byte-window twin, for
  payloads that must compact several times inside one value.

Use one of these rather than open-coding a `ggen.Stream`: the parity contract
they pin (same sentinel, same `Pos`, any chunk size) is the reason the round-10
error-position tests exist.

### `gen/` — differential test for the schema emitters

`gen/fixture.go` (+ `gen/other/`) is a NON-test fixture, because `gen.Load`
skips test files; one `//go:generate ../../ggen ./...` line generates the
whole tree, `./other` first, since a host generated before its cross-package
dependency falls back to `encoding/json`. The fixture aims at coverage, not
realism: `Scalars`, `Stdlib`, `Containers`, `Rules`, `Node`/`Pair` and `More`
together carry every Go kind, every `format:`, every rule, a catch-all map, an
embedded struct, a quoted JSON name, an integer enum, a `json:"-"` field and a
pointer to a map.

`TestDifferential` loads both packages, places every type's `In()` and `Out()`
into zod and valibot files, type-checks them with `tsc --strict`, then tries
every probe value at every field (and as every non-object value): the
generated Go decoder and each input schema must agree on accept/reject,
transformed values must match Go's, Go must read a schema's parsed value as it
read the probe, and whatever Go accepts must marshal to JSON the output
schemas accept. Disagreements a schema cannot avoid are the `divergences`
predicates (JSON numbers, closed enum sets, converter checks); each must
explain at least one case, so a stale one fails. Needs `node` on PATH and
`npm ci` at the repo root, whose workspace pins zod, valibot and typescript
for every JavaScript lane (`gen/js/package.json` is the member that names the
three); skips otherwise.

`TestDifferentialTS` type-checks every value Go accepts against the `ts` types
themselves: an accepted input is assigned to the input type and Go's output to
the output type, as a literal, so a tuple, an enum member and a narrowed
string have to fit. A `tsc` error line maps back to its case. A probe carrying
a key the input type does not declare is left out, because the literal
assignment also runs TypeScript's excess-property check and an unknown key is
the decoder's business. It takes its typescript from `GGEN_NODE_MODULES` or the
root install, like the lane above.

`TestDifferentialSwift` renders the Swift types of every fixture type and, for
every case Go accepts, requires that the Swift output type decodes Go's output
and re-encodes the same value, and that Go reads what the Swift input type
re-encodes as the value it read directly. `TestDifferentialKotlin` does the
same with kotlinx.serialization data classes and needs `GGEN_KOTLIN_HOME`.
Both share `checkTypedLane`, with per-language `laneDivergence` lists that must
each explain a disagreement: Swift's `Decimal` range and swift-foundation's
refusal of a literal outside the target's float range (underflow included),
kotlinx's refusal of a non-finite number, and neither language having a
catch-all map. The re-encoding is compared by value with `sameValue`, which
reads a key holding null as absent on both sides: Swift and Kotlin leave a nil
property out where Go writes null, and Go reads an absent key as that same
zero.

`TestUnloaded` pins what a script sees when it loads a package whose
ggen-generated dependency it did not load.

`gen/test-toolchains.sh` installs every toolchain these tests need, runs them,
and fails on a SKIPPED test: every lane skips when its toolchain is missing,
so a skip under the script means a lane silently stopped running.

### `thirdparty/` and `thirdparty2/`

- `thirdparty/` — non-annotated external type; exercises `encoding/json` fallback for cross-package types ggen can't see.
- `thirdparty2/` — annotated external type; exercises static-analyzer pickup of a cross-package generated decoder. Regenerated with `go generate`. `Wrap` carries a hand-written `UnmarshalJSON` and no `DecodeFrom`, so a host package reaches it through the `UnmarshalJSON` rung (`TestCrossPkg_unmarshalJSONRungRebasesPos`).

## Fuzz

Fuzzers in `fuzz_test.go`:

- `FuzzStreamEqualsBytes` — bytes vs stream path agreement across chunk sizes (`Node`); seeds from `fuzzSeeds` (incl. `\uXXXX` / surrogate-pair escape seeds — the escape decode path was previously uncovered, which hid a stream surrogate-refill bug). On a marshaled-byte mismatch it re-checks order-insensitively (parse both to `any`, `reflect.DeepEqual`) before failing, so nondeterministic map key order isn't a false divergence.
- `FuzzBoundaryNoPanic` / `FuzzStreamHugeStringNoPanic` — panic safety on NaN/Inf/overflow/lone-surrogate (`BoundaryStruct`) and multi-MiB strings through tiny bufs (`HugeStringStruct`).
- `FuzzPrimitivesCompat` — fuzzes typed VALUES, not payload bytes. `PrimStruct` carries one field per primitive kind (bool, every int/uint width, float32/64, string); fuzzed values are stdlib-marshaled into a well-formed payload, then ggen and jsonv2 must decode it identically (both accept/reject, equal structs on success). Drives value-parsers across their full domain. Invalid-UTF-8 strings can't round-trip jsonv2.Marshal, so they route to a REJECT-PARITY branch instead (hand-built payload; both ggen — with `ggen.ErrInvalidUTF8` — and jsonv2 must refuse; the old skip here was the blind spot that hid the pass-through-invalid-UTF-8 bug). Skips NaN/Inf floats (no JSON form). `float32` decodes through `ggen.Float32`, which rounds the decimal once at 32 bits like stdlib; `TestNarrowFloat_RoundsDecimalOnce` (scan_decode_test.go) pins the midpoint tokens a double rounding gets wrong.

## Running tests

`./...` from root does NOT cross module boundaries; cd in first:

```sh
(cd integrationtests && go test ./...)
```

Annotated structs use `encoding/json/v2` import paths and stdcompat compares
against jsonv2 — both stable since Go 1.27, so no `GOEXPERIMENT` is needed.

## Adding new tests

In order:

1. **Audit existing tests** — `grep` for similar assertions / annotations / feature names; the table above is first stop. Common cases (Address, Node, slice/map shapes) are covered.
2. **Extend, don't duplicate** — prefer modifying a related test, refactoring into a table-driven loop where possible. The big `InvalidRuleApplication` table in root `cli_test.go` is the reference shape (one slice of `{name, input, wantSubstring}`, one `t.Run` per row, ~80 cases under one parent).
3. **Avoid new helpers** unless the same setup recurs ≥3 times — `runCLI`, `writeFixture`, `captured`, `mustHaveFile`, `writeGoFile` already exist in the root module; in this module `decodeBothPaths` / `decodeBothChunked` / `decodeBothPathsChunked` (above) cover bytes-vs-stream parity — check the matching `_test.go` for in-package helpers first.
4. **Pick host file** by feature area; don't fragment.
5. **Only create a new `*_test.go`** when a feature has no existing home (rare — new files dilute grep).
6. **Pick host struct** — `Address`, `Node`, `WideStruct`, `Multi`, `Bad` cover most combinations; reuse or extend before adding.
7. **Only add a new struct** when none carries the right field-kind/tag combination — annotated test structs go in the same file as the test, `shared_test.go` only when ≥2 files need it.

## JSONSize tests live in jsonsize_test.go

**ALL `JSONSize` cap-guard tests belong in `jsonsize_test.go`**, not in the
feature file where the struct is declared. The struct stays next to its feature
tests (e.g. `SQLNullStringStruct` in `sql_test.go`), but every
`TestJSONSize_*_NoRealloc` / `TestJSONSize_*PerType_*` table sits in
`jsonsize_test.go` alongside the existing `TimeFormats`/`URLStruct`/`TupleStruct`/
`HTMLEscapeStruct`/`InlineStruct`/`StringTagStruct`/`SQLNull*` tables.

Why: `JSONSize` is a single contract — "AppendJSON never grows beyond the cap I
reserved" — and one kind's regression usually breaks the budget for several
structs at once. One file means `go test -run TestJSONSize ./...` covers the
contract and budget helpers (`populatedSQLNull`, `richTypesWorst`,
`wideStructAllShort`, …) live beside their tests. Helpers used ONLY by JSONSize
follow the test into `jsonsize_test.go`; helpers shared with a roundtrip test
live in the feature file (or `shared_test.go`).
