# integrationtests — feature / roundtrip / compat / fuzz tests

Separate Go module (own `go.mod` with `replace github.com/sirkostya009/ggen =>
../`). Imports root packages as external consumer, so test surface exercises public API at boundary users hit.

## Generated files & the `//go:generate` workflow

Each annotated source carries `//go:generate ../ggen $GOFILE`, produces sibling `<file>_ggen_test.go`. Build tags propagate to generated file. Files behind opt-in tags (e.g. `//go:build goexperiment.jsonv2 &&
ggen_brokencodegen`) skipped by default `go generate` — pass `-tags=…` to opt in. Regenerate:

```sh
(cd integrationtests && GOEXPERIMENT=jsonv2 go generate ./...)
```

Cross-file struct refs (e.g. `pointer_test.go` field of type `Address` declared in `shared_test.go`) work first run because single-file mode seeds known-types set with every annotated name in package, so codegen emits direct `Address{}.DecodeFrom(...)` not encoding/json fallback.

## Test files

- `shared_test.go` — shared annotated structs (Address, Node, …) used across feature tests.
- `payloads_test.go` — `complexPayload`/`complexValue` (roundtrip/stdcompat), `megaPayload`/`megaValue` (1 MiB generated Node tree, fixed seed 1; used by `stdcompat_test.go` at scale).
- `<file>_ggen_test.go` — generated methods, one per annotated source.

Per-feature coverage files:

| File                    | What it covers                                                                                                                                                                         |
| ----------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `alias_test.go`         | Top-level type aliases — primitive, struct, slice/map/array, `[]byte`, struct delegation tiers.                                                                                        |
| `any_test.go`           | `any` / `interface{}` fields; usenumber mode.                                                                                                                                          |
| `custom_test.go`        | `@FuncName` / `@pkg.FuncName` validators and mods.                                                                                                                                     |
| `decode_dups_test.go`   | `allowdups`: first-wins, `SkipValue` advance for later occurrences vs default `DuplicateKeyError`.                                                                                     |
| `dive_test.go`          | `dive:` / `keys:` tag prefixes on slices / arrays / maps.                                                                                                                              |
| `extra_test.go`         | Misc edge cases not fit elsewhere.                                                                                                                                              |
| `fallback_test.go`      | `encoding/json` fallback for cross-package non-annotated types.                                                                                                                        |
| `hooks_test.go`         | Opt-in `MarshalJSON` / `UnmarshalJSON` hooks (`-marshal` / `-unmarshal`).                                                                                                              |
| `htmlescape_test.go`    | Literal default (jsonv2-shaped) + `htmlescape` opt-in (v1-shaped).                                                                                                                     |
| `inline_test.go`        | `json:",inline"` catch-all map — unknown keys absorbed, overrides `ignoreunknown`, spliced out on marshal.                                                                             |
| `jsonsize_test.go`      | `JSONSize()` worst-case upper-bound. **Houses every `JSONSize` cap-guard regardless of feature** — see below.                                                                          |
| `maps_test.go`          | String-keyed maps; key validators / mods; deep map values.                                                                                                                             |
| `merge_test.go`         | Decode-into-receiver: scalar persistence, slice `[:0]` reuse, map `clear()` reuse, `null` → nil, `[]`/`{}` on non-nil vs nil receiver. **Contract pin.**                               |
| `mods_test.go`          | `mod:"…"` transforms — trim/lower/upper/trimleft/trimright/replace/clamp/`@Func`.                                                                                                      |
| `native_test.go`        | `time.Time`, `time.Duration`, `net.IP`, `netip.Addr`/`Prefix`, `[]byte` encodings.                                                                                                     |
| `omit_test.go`          | `omitempty` / `omitzero`.                                                                                                                                                              |
| `pointer_test.go`       | `*T` — null ↔ nil, primitive + struct pointees, `[]*T` slab. `NPtrStruct` exercises `**T`/`***T`/… scalar via json fallback; slice/array variants gated on n-pointer backlog item. |
| `read_test.go`          | Basic Read + unknown-key error & ignoreunknown opt-in.                                                                                                                                 |
| `richtypes_test.go`     | UUID, decimal, big.Int/Float/Rat, sql.Null\*, json.RawMessage, jsontext.Value, url.URL.                                                                                                |
| `roundtrip_test.go`     | Symmetric marshal → unmarshal → marshal stability.                                                                                                                                     |
| `scan_decode_test.go`   | Bytes-path + stream-path correctness (chunked reader, tiny-hint-forces-grow).                                                                                                          |
| `sql_test.go`           | `database/sql.Null*` family wire shape.                                                                                                                                                |
| `stdcompat_test.go`     | Exhaustive ggen ↔ jsonv2 round-trip; re-marshaled via jsonv2, compared as parsed `any` (map order + nil/empty-slice noise normalized).                                                 |
| `wire_test.go`          | Wire-format fixtures for divergence-from-stdlib types (`url.URL`, `sql.Null*`).                                                                                                        |
| `fuzz_test.go`          | Three fuzzers over `Node` — see Fuzz section.                                                                                                                                          |
| `brokencodegen_test.go` | Opt-in (behind `ggen_brokencodegen` tag) — codegen regressions worth pinning even when broken.                                                                                         |

### `thirdparty/` and `thirdparty2/`

- `thirdparty/` — non-annotated external type. Exercises `encoding/json` fallback for cross-package types ggen can't see.
- `thirdparty2/` — annotated external type. Exercises static-analyzer pickup of cross-package generated decoder. Regenerated with `go generate`.

## Fuzz

Three fuzzers over `Node` in `fuzz_test.go`: `FuzzScanNoPanic` (panic safety on random bytes), `FuzzRoundtrip` (encode → decode fixed point after one round), `FuzzCompat` (when both ggen and jsonv2 accept, decoded values agree via `sameWire`). Compat deliberately ignores accept/reject drift on: top-level `null`, trailing garbage, invalid UTF-8 inside strings.

## Running tests

`./...` from root does NOT cross module boundaries; cd in first:

```sh
(cd integrationtests && GOEXPERIMENT=jsonv2 go test ./...)
```

`GOEXPERIMENT=jsonv2` required — annotated structs use `encoding/json/v2` import paths, stdcompat compares against jsonv2.

## Adding new tests

In order:

1. **Audit existing tests** — `grep` for similar assertions / annotations / feature names; table above is first stop. Common cases (Address, Node, slice/map shapes) covered.
2. **Extend, don't duplicate** — prefer modifying related test, even refactoring it into table-driven loop (single assertion → `cases := []struct{…}{}`; inline subtest body → helper called from `for _, c := range cases { t.Run(c.name, …) }`; copy-pasted `t.Run` blocks → same loop). Big `InvalidRuleApplication` table in root `cli_test.go` is reference shape (one slice of `{name, input, wantSubstring}`, one `t.Run` per row, ~80 cases under one parent).
3. **Avoid new helpers** unless same setup recurs ≥3 times — `runCLI`, `writeFixture`, `captured`, `mustHaveFile`, `writeGoFile` already exist in root module; check matching `_test.go` for in-package helpers first.
4. **Pick host file** by feature area; don't fragment.
5. **Only create new `*_test.go`** when feature has no existing home (rare — new files duplicate boilerplate, dilute grep).
6. **Pick host struct** — `Address`, `Node`, `WideStruct`, `Multi`, `Bad` cover most combinations; reuse or extend before adding.
7. **Only add new struct** when none carries right field-kind/tag combination — annotated test structs go in same file as test, `shared_test.go` only when ≥2 files need it.

## JSONSize tests live in jsonsize_test.go

**ALL `JSONSize` cap-guard tests belong in `jsonsize_test.go`**, not in feature file where struct declared. Struct stays next to its feature tests (e.g. `SQLNullStringStruct` in `sql_test.go`), but every `TestJSONSize_*_NoRealloc` / `TestJSONSize_*PerType_*` table sits in `jsonsize_test.go` alongside existing `TimeFormats`/`URLStruct`/`TupleStruct`/`HTMLEscapeStruct`/`InlineStruct`/`StringTagStruct`/`SQLNull*` tables.

Why: `JSONSize` is single contract — "AppendJSON never grows beyond cap I reserved." Regressions in one kind usually break budget for several structs at once. Keeping every cap-guard in one file means `go test -run TestJSONSize
./...` covers contract, table stays grep-discoverable, budget helpers (`populatedSQLNull`, `richTypesWorst`, `wideStructAllShort`, …) live next to tests that consume them. Helpers that ONLY exist for JSONSize testing follow test into `jsonsize_test.go`; helpers used by both JSONSize AND roundtrip test live in feature file (or `shared_test.go`).