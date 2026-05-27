# integrationtests — feature / roundtrip / compat / fuzz tests

Separate Go module (its own `go.mod` with
`replace github.com/sirkostya009/ggen => ../`). Imports the root
packages as an external consumer would, so the test surface
exercises the public API at the same boundary users hit.

## Generated files & the `//go:generate` workflow

Each annotated source file carries
`//go:generate ../ggen $GOFILE` and produces a sibling
`<file>_ggen_test.go`. Build tags on the source propagate to the
generated file. Files behind opt-in tags (e.g.
`//go:build goexperiment.jsonv2 && ggen_brokencodegen`) are
skipped by the default `go generate` invocation — pass `-tags=…`
to opt in.

To regenerate after editing tags or annotations:

```sh
(cd integrationtests && GOEXPERIMENT=jsonv2 go generate ./...)
```

Cross-file struct references (e.g. `pointer_test.go` field of type
`Address` declared in `shared_test.go`) work on first run because
single-file mode seeds the generator's known-types set with every
annotated name in the package, so the codegen emits a direct
`Address{}.DecodeFrom(...)` call rather than the encoding/json
fallback.

## Test files

- `shared_test.go` — shared annotated structs (Address, Node, …)
  used across the feature tests.
- `payloads_test.go` — `complexPayload` + `complexValue` (used by
  roundtrip / stdcompat tests) and `megaPayload` / `megaValue`
  (1 MiB generated Node tree, fixed seed 1; used by
  `stdcompat_test.go` to exercise cross-compat at scale).
- `<file>_ggen_test.go` — generated methods, one file per
  annotated source.

Per-feature coverage files:

| File                       | What it covers                                                                                                                                                                                                              |
| -------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `alias_test.go`            | Top-level type aliases — primitive, struct, slice/map/array, `[]byte`, struct delegation tiers.                                                                                                                            |
| `any_test.go`              | `any` / `interface{}` fields; usenumber mode.                                                                                                                                                                              |
| `custom_test.go`           | `@FuncName` / `@pkg.FuncName` validators and mods.                                                                                                                                                                          |
| `decode_dups_test.go`      | `allowdups` semantics: first-wins, `SkipValue` advance for later occurrences vs default `DuplicateKeyError`.                                                                                                               |
| `dive_test.go`             | `dive:` / `keys:` tag prefixes on slices / arrays / maps.                                                                                                                                                                  |
| `extra_test.go`            | Misc edge cases that don't fit elsewhere.                                                                                                                                                                                  |
| `fallback_test.go`         | `encoding/json` fallback for cross-package non-annotated types.                                                                                                                                                            |
| `hooks_test.go`            | Opt-in `MarshalJSON` / `UnmarshalJSON` hooks (`-marshal` / `-unmarshal`).                                                                                                                                                  |
| `htmlescape_test.go`       | Literal default (jsonv2-shaped) + `htmlescape` opt-in (v1-shaped).                                                                                                                                                         |
| `inline_test.go`           | `json:",inline"` catch-all map field — unknown keys absorbed, overrides `ignoreunknown`, spliced out on marshal.                                                                                                            |
| `jsonsize_test.go`         | `JSONSize()` worst-case upper-bound coverage. **Houses every `JSONSize` cap-guard regardless of which feature the struct exercises** — see "JSONSize tests live in jsonsize_test.go" below.                                  |
| `maps_test.go`             | String-keyed maps; key validators / mods; deep map values.                                                                                                                                                                 |
| `merge_test.go`            | Decode-into-receiver: scalar persistence across omitted JSON fields, slice `[:0]` reuse, map `clear()` reuse, JSON `null` → nil container, JSON `[]`/`{}` on non-nil vs nil receiver. **Pins the user-facing contract.**     |
| `mods_test.go`             | `mod:"…"` input transforms — `trim`, `lower`, `upper`, `trimleft`, `trimright`, `replace`, `clamp`, custom `@Func`.                                                                                                       |
| `native_test.go`           | `time.Time`, `time.Duration`, `net.IP`, `netip.Addr`/`Prefix`, `[]byte` encodings.                                                                                                                                         |
| `omit_test.go`             | `omitempty` / `omitzero` semantics.                                                                                                                                                                                        |
| `pointer_test.go`          | `*T` fields — null ↔ nil, primitive + struct pointees, `[]*T` slab allocation. `NPtrStruct` exercises multi-level pointer scalar fields (`**T` / `***T` / …) via the json fallback; slice/array variants gated on the n-pointer backlog item. |
| `read_test.go`             | Basic Read tests + unknown-key error & ignoreunknown opt-in.                                                                                                                                                               |
| `richtypes_test.go`        | UUID, decimal, big.Int/Float/Rat, sql.Null*, json.RawMessage, jsontext.Value, url.URL.                                                                                                                                     |
| `roundtrip_test.go`        | Symmetric marshal → unmarshal → marshal stability.                                                                                                                                                                          |
| `scan_decode_test.go`      | Bytes-path + stream-path correctness (chunked-reader, tiny-hint-forces-grow).                                                                                                                                              |
| `sql_test.go`              | `database/sql.Null*` family wire shape.                                                                                                                                                                                    |
| `stdcompat_test.go`        | Exhaustive cross-compat: for every annotated struct, ggen ↔ jsonv2 round-trip; results re-marshaled via jsonv2 and compared as parsed `any` (map order and nil/empty-slice noise normalized).                              |
| `wire_test.go`             | Wire-format fixtures for the divergence-from-stdlib types (`url.URL`, `sql.Null*`).                                                                                                                                       |
| `fuzz_test.go`             | Three fuzzers over `Node` — see Fuzz section below.                                                                                                                                                                        |
| `brokencodegen_test.go`    | Opt-in (behind `ggen_brokencodegen` build tag) — codegen regressions worth pinning even when broken.                                                                                                                       |

### `thirdparty/` and `thirdparty2/`

- `thirdparty/` — non-annotated external type. Exercises
  `encoding/json` fallback for cross-package types ggen can't see.
- `thirdparty2/` — annotated external type. Exercises the static
  analyzer pickup of cross-package generated decoder. Regen with
  `(cd integrationtests && GOEXPERIMENT=jsonv2 ../ggen ./thirdparty2)`.

## Fuzz tests

Three fuzzers over `Node` in `fuzz_test.go`:

- `FuzzScanNoPanic` — panic safety on random bytes.
- `FuzzRoundtrip` — encode → decode is a fixed point after one round.
- `FuzzCompat` — when both ggen and jsonv2 accept input, decoded
  values must agree via `sameWire`.

Run a target with `go test -run=^$ -fuzz=FuzzX -fuzztime=30s` from
inside `integrationtests/`. Known accept/reject drifts the compat
target deliberately ignores:
- top-level `null`
- trailing garbage
- invalid UTF-8 inside strings

Gaps + planned coverage live in `.claude/backlog.md` under
"Improve fuzz coverage".

## Running tests

`./...` from the root does NOT cross module boundaries; cd in
first:

```sh
(cd integrationtests && GOEXPERIMENT=jsonv2 go test ./...)
```

The `GOEXPERIMENT=jsonv2` env is required because annotated
structures use `encoding/json/v2` import paths and stdcompat tests
compare against jsonv2.

## Adding new tests

Before writing a new test, do this in order:

1. **Audit existing tests.** `grep` the codebase for similar
   assertions, struct annotations, or feature names. The table
   above is your first stop. Common cases — Address, Node,
   slice/map shapes — are covered; the test you're imagining may
   already exist in spirit.
2. **Extend, don't duplicate.** When a related test already exists,
   PREFER modifying it to cover the new case — even when that
   means refactoring the existing test into a table-driven loop.
   Patterns to look for:
    - Single assertion that could become a `cases := []struct{…}{}`
      slice with one entry per scenario.
    - Inline subtest body that could be lifted into a helper
      called from a `for _, c := range cases { t.Run(c.name, …) }`
      loop.
    - Multiple `t.Run("X", …)` blocks with copy-pasted bodies —
      candidates for the same loop treatment.
   The applicability tests in the root `cli_test.go` (the big
   `InvalidRuleApplication` table) are the reference shape: one
   slice of `{name, input, wantSubstring}` triples, one `t.Run`
   per row. ~80 cases sit cleanly under one parent.
3. **Avoid creating new helpers** unless the same setup recurs
   ≥3 times. `runCLI`, `writeFixture`, `captured`, `mustHaveFile`,
   `writeGoFile` already exist in the root module; check the
   matching `_test.go` file for in-package helpers before writing
   your own.
4. **Pick the host file.** Each `*_test.go` corresponds to a
   feature area (dive, mods, inline, native, pointer, etc.). New
   tests belong in the file whose existing tests share the most
   context with the new case. Don't fragment.
5. **Only create a new `*_test.go` file** when the new feature has
   no existing home (rare). New files duplicate setup boilerplate
   and create grep dilution — avoid unless the test surface is
   genuinely new.
6. **Pick the host struct.** Look at `shared_test.go` and the
   annotated structs at the top of each feature test file.
   `Address`, `Node`, `WideStruct`, `Multi`, `Bad` etc. cover
   most feature combinations. Re-use the existing struct when
   its shape lets you exercise the new case. Can also extend
   existing structs with new fields if they fall under the same
   test category but don't have coverage yet.
7. **Only add a new struct** when no existing one carries the
   right field kind / tag combination. Annotated test structs go
   in the same file as the test that uses them; add to
   `shared_test.go` only when two or more files need it.

When the right approach is unclear, default to: adding new top-
level test functions. After that consider merging newly added
tests into existing once the testing abstraction is vividly clear.

## JSONSize tests live in jsonsize_test.go

**ALL `JSONSize` cap-guard tests belong in `jsonsize_test.go`,
not in the feature file where the struct itself is declared.**
The struct stays next to its feature tests (e.g.
`SQLNullStringStruct` in `sql_test.go`, `PtrSliceItemsStruct` in
`pointer_test.go`), but every `TestJSONSize_*_NoRealloc` /
per-type `TestJSONSize_*PerType_*` table sits in
`jsonsize_test.go` alongside the existing `TimeFormats` /
`URLStruct` / `TupleStruct` / `HTMLEscapeStruct` / `InlineStruct`
/ `StringTagStruct` / `SQLNull*` tables.

Why: `JSONSize` is a single contract — "AppendJSON never grows
beyond the cap I reserved." Regressions in one kind (a new
format, a new container layout, a wire-shape change for an
existing kind) usually break the budget for several structs at
once. Keeping every cap-guard in one file means a single
`go test -run TestJSONSize ./...` covers the contract, the
table-driven shape stays grep-discoverable, and budget-related
helpers (`populatedSQLNull`, `richTypesWorst`, `wideStructAllShort`,
…) live next to the tests that consume them. Splitting back into
the feature files fragments that signal and forces every cap
helper to either duplicate or migrate to `shared_test.go`.

Helpers that ONLY exist for JSONSize testing (build a
worst-case fixture, never used by wire-shape / roundtrip tests)
follow the test into `jsonsize_test.go`. Helpers used by both
worth-case JSONSize AND a roundtrip test live in the feature
file (or `shared_test.go` if cross-file).

## merge_test.go is a contract pin

`merge_test.go` pins the user-facing decode-into-receiver
contract: scalar persistence across omitted JSON fields, slice
backing-array reuse via `[:0]`, map `clear()` reuse, JSON `null`
→ nil container, JSON `[]` / `{}` on non-nil vs nil receiver.
Changes to the reset/merge codegen MUST keep these passing — do
not "fix" the tests to match new behavior without first updating
this doc and the root CLAUDE.md "Decode-into-receiver semantics"
section.
