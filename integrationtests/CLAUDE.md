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
| `alias_test.go`         | Top-level type aliases — primitive, struct, slice/map/array, `[]byte`, `[N]byte` (base64 + strict length, same wire as a `[N]byte` field), leading whitespace across every alias shape, struct delegation tiers, single-alloc float budget, and element structs reached only through a container alias. Plus NAMED PRIMITIVES (`NamedPrims`, annotated and not): every rule resolves the underlying kind and casts through it — `oneof`, `eq`/`neq`, rune/substring/charset rules, `nullzero`.                                                                                                                                                                                                                                                                                                                                                                                    |
| `any_test.go`           | `any` / `interface{}` fields; usenumber mode.                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `copy_test.go`          | `-copy` / `//ggen:generate copy`: bytes-path decode copies strings / slice elems / map keys+values / `json.RawMessage` / any-embedded strings / `url.URL` components out of the input. Scribbles the source buffer after decode and asserts retained values survive (`AliasDoc` is the negative control proving the scribble is effective); deep-tree fingerprint check.                                                                                                                                       |
| `custom_test.go`        | `@FuncName` / `@pkg.FuncName` validators and mods.                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `decode_dups_test.go`   | `allowdups`: first-wins, `SkipValue` advance for later occurrences vs default `DuplicateKeyError`.                                                                                                                                                                                                                                                                                                                                                                                 |
| `dive_test.go`          | `inner:` / `keys:` pipe prefixes on slices / arrays / maps.                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `extra_test.go`         | Misc edge cases not fit elsewhere.                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `fallback_test.go`      | `encoding/json` fallback for cross-package non-annotated types. Plus `CrossPkgShapes`: a cross-package GGEN type in every field position (value / slice / map / `*T` / `**T` / `[]*T` / `[N]T`) — pins both the foreign-import collection and the AppendJSON rung.                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `hooks_test.go`         | Opt-in `MarshalJSON` / `UnmarshalJSON` hooks (`-marshal` / `-unmarshal`).                                                                                                                                                                                                                                                                                                                                                                                                          |
| `htmlescape_test.go`    | Literal default (jsonv2-shaped) + `htmlescape` opt-in (v1-shaped).                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `embed_test.go`         | `json:",embed"` catch-all map — unknown keys absorbed, overrides `ignoreunknown`, spliced out on marshal.                                                                                                                                                                                                                                                                                                                                                                         |
| `keyescape_test.go`     | Statically JSON-escaped wire-key name constants: quote-bearing name (jsonv2 byte parity), htmlescape name with `&` (v1 parity), backslash-bearing name (valid JSON + self-round-trip; name-spelling divergence vs jsonv2 pinned in backlog). |
| `jsonsize_test.go`      | `JSONSize()` worst-case upper-bound. **Houses every `JSONSize` cap-guard regardless of feature** — see below.                                                                                                                                                                                                                                                                                                                                                                      |
| `maps_test.go`          | String-keyed maps; key validators / mods; deep map values. `NamedValMaps`: named-primitive map values marshal via the primCast'd ref (round-trip).                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `merge_test.go`         | Decode-into-receiver: omitted keys zeroed (every kind, both paths), slice `[:0]` reuse, map `clear()` reuse, carried slice-element and map-value allocations reused — struct/slice/map/`[]byte`/pointer values, plus `json.RawMessage` under `-copy` (bytes path; the stream path still clears), `null` → nil, `[]`/`{}` on non-nil vs nil receiver. **Contract pin.**                                                                                                                                                                                                                                                                                                                           |
| `mods_test.go`          | `pipe:` transforms — trim/tolower/toupper/trimleft/trimright/replace/clamp/`@Func`; islower/isupper case validators.                                                                                                                                                                                                                                                                                                                                                                                                    |
| `native_test.go`        | `time.Time`, `time.Duration`, `net.IP`, `netip.Addr`/`Prefix`, `[]byte` encodings.                                                                                                                                                                                                                                                                                                                                                                                                 |
| `omit_test.go`          | `omitempty` / `omitzero`.                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `pointer_test.go`       | `*T` — null ↔ nil, primitive + struct pointees, `[]*T` slab. `PointerStruct` composes per-pointee-kind single-level fields (`PtrName`/`PtrCount`/…); `NPtrStruct` embeds per-depth `**T`/`***T`/`****T`/`**struct`, all native; `NPtrContainersStruct` covers multi-level pointers in containers (`[]**T`, `[3]**T`, `[][]**T`) and pointer map values (`map[string]*T`/`**T`/`*Address`). Per-field JSONSize cap-guards in `jsonsize_test.go` (`PtrFieldPerKind`/`NPtrPerDepth`). `PtrContainers` covers POINTERS TO CONTAINERS at any depth (`*[]T`, `***[]T`, `*map`, `***map`, `**map[string]struct`): the receiver reset reaches through every level (a reused receiver replaces, never appends) and the element kind resolves past depth 1. |
| `read_test.go`          | Basic Read + unknown-key error & ignoreunknown opt-in.                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `richtypes_test.go`     | UUID, decimal, big.Int/Float/Rat, sql.Null\*, json.RawMessage, jsontext.Value, url.URL. `RawOnly` isolates a raw-span field: a reused receiver's backing is refilled, so steady-state stream decode is allocation-free.                                                                                                                                                                                                                                                                                                                                                                                            |
| `roundtrip_test.go`     | Symmetric marshal → unmarshal → marshal stability.                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `scan_decode_test.go`   | Bytes-path + stream-path correctness (chunked reader, tiny-hint-forces-grow). `TestNarrowIntOverflow` (`NarrowInts`): narrow int/uint fields/map/slice/pointer reject out-of-range values (ErrNumberOverflow) matching encoding/json, bytes + stream — pins the codegen overflow guard vs the old silent truncation. `TestParseError_SliceStructural`/`TestParseError_ScanPrimitivePos`: bytes-path slice structural errors wrap in `*ggen.ParseError` (path + pos) and scan-primitive error positions surface end-to-end, bytes + stream. `TestParseError_StreamPosChunkInvariant`: the reported offset and sentinel are the same at every chunk size and agree with the bytes path — stream scan primitives rebase onto the value head on error, so a compacting refill cannot leave a stale cursor behind.                                                                                                                                                                    |
| `seq_growth_test.go`    | Stream window bounds over long runs of GENERATED container-bearing values, whose emitted refills are grow-only: `Seq`, `Array` and `Slice` each drop consumed bytes once the window is full, so the buffer cannot ratchet.                                                                                                                                                                                                                                                                                                       |
| `sql_test.go`           | `database/sql.Null*` family wire shape.                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `stdcompat_test.go`     | Exhaustive ggen ↔ jsonv2 round-trip; re-marshaled via jsonv2, compared as parsed `any` (map order + nil/empty-slice noise normalized). Plus `exactWire` byte-identical checks (`F64Wire`/`F32Wire`/`AnyWire`) for shapes `crossCompat` normalization masks — float formatting (`1e+06` vs `1000000`) and any-string HTML escaping; single-field/single-key carriers keep order deterministic vs jsonv2.                                                                            |
| `wire_test.go`          | Wire-format fixtures for divergence-from-stdlib types (`url.URL`, `sql.Null*`).                                                                                                                                                                                                                                                                                                                                                                                                    |
| `fuzz_test.go`          | Fuzzers over `Node`, `BoundaryStruct`, `HugeStringStruct`, `PrimStruct` — see Fuzz section.                                                                                                                                                                                                                                                                                                                                                                                        |
| `brokencodegen_test.go` | Opt-in (behind `ggen_brokencodegen` tag) — codegen regressions worth pinning even when broken.                                                                                                                                                                                                                                                                                                                                                                                     |

### `thirdparty/` and `thirdparty2/`

- `thirdparty/` — non-annotated external type; exercises `encoding/json` fallback for cross-package types ggen can't see.
- `thirdparty2/` — annotated external type; exercises static-analyzer pickup of a cross-package generated decoder. Regenerated with `go generate`.

## Fuzz

Fuzzers in `fuzz_test.go`:

- `FuzzStreamEqualsBytes` — bytes vs stream path agreement across chunk sizes (`Node`); seeds from `fuzzSeeds` (incl. `\uXXXX` / surrogate-pair escape seeds — the escape decode path was previously uncovered, which hid a stream surrogate-refill bug). On a marshaled-byte mismatch it re-checks order-insensitively (parse both to `any`, `reflect.DeepEqual`) before failing, so nondeterministic map key order isn't a false divergence.
- `FuzzBoundaryNoPanic` / `FuzzStreamHugeStringNoPanic` — panic safety on NaN/Inf/overflow/lone-surrogate (`BoundaryStruct`) and multi-MiB strings through tiny bufs (`HugeStringStruct`).
- `FuzzPrimitivesCompat` — fuzzes typed VALUES, not payload bytes. `PrimStruct` carries one field per primitive kind (bool, every int/uint width, float32/64, string); fuzzed values are stdlib-marshaled into a well-formed payload, then ggen and jsonv2 must decode it identically (both accept/reject, equal structs on success). Drives value-parsers across their full domain. Invalid-UTF-8 strings can't round-trip jsonv2.Marshal, so they route to a REJECT-PARITY branch instead (hand-built payload; both ggen — with `ggen.ErrInvalidUTF8` — and jsonv2 must refuse; the old skip here was the blind spot that hid the pass-through-invalid-UTF-8 bug). Skips NaN/Inf floats (no JSON form). `float32` decodes via `ggen.Float64`+cast (double-round) vs stdlib's bitSize 32 — clean because jsonv2 emits shortest-round-tripping decimals.

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
3. **Avoid new helpers** unless the same setup recurs ≥3 times — `runCLI`, `writeFixture`, `captured`, `mustHaveFile`, `writeGoFile` already exist in the root module; check the matching `_test.go` for in-package helpers first.
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
