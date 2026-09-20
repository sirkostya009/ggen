# gen — parser library and cross-language export

Module `github.com/sirkostya009/ggen/gen`. This file is the implementation
doc; `gen/SKILL.md` is the user-facing surface for the same thing, and the two
move together.

Holds the parts of ggen that other programs import: the parser (`model`) and
the language-neutral export core (package `gen`, the module root) that scripts
use to generate declarations or schemas in other languages, and the emitters
for specific targets (`ts`, `zod`, `valibot`, `kotlin`, `swift`; plan:
`.claude/schema-plan.md`). The `ggen` binary in `cmd/ggen/` depends on this module;
this module never depends on `cmd/ggen`.

## Layout

```
gen/
├── kinds.go    Mode, GoKind, Wire, Unknown, Op
├── load.go     Load, Set, Package, TypeDecl, enum types, recursion
├── shape.go    Shape, Field, Type, Enum, Rule; lowering per mode; Strict
├── output.go   Output, File, Place, Emitter, Context, Writer, Report, Write
├── model/      the parser
├── ts/         TypeScript types emitter; its Printer and Imports serve every TS-based emitter
├── zod/        Zod 4 emitter
├── valibot/    Valibot 1.x emitter
├── kotlin/     kotlinx.serialization data classes
├── swift/      Swift Codable structs, classes and enums
├── internal/js literals, identifiers, doc comments, Go-exact runtime helpers
└── testdata/   api + other: the fixture every test loads
```

## The core (`gen`)

A script loads packages, picks types, places each type's `In()` or `Out()`
shape into files it names, and calls `Write`. Nothing is implicit: no default
paths, names or modes, and a reference to a generated type that was not placed
fails `Write`. Emitters receive plain data and write every character of the
target themselves; the core has no opinion about syntax.

### Loading

- `Load(patterns...)` walks the patterns with `model.WalkPackages` and parses
  each directory. Every `ParsePackage` call type-checks on its own, so the
  same Go type reached from two packages is two distinct `types.Object`
  values: the core keys types by import path + name, never by identity, and
  resolves positions with the file set of the load that produced them
  (`TypeDecl.fset`).
- Test-file types are skipped.
- `Annotated` is "carries `//ggen:generate` itself"; reached structs are
  generated too but not annotated.
- Constants merge across every loaded package (a package may add constants to
  another package's type). A named string or integer type with at least one
  constant becomes an enum `TypeDecl`, created when a loaded type first
  references it. `Load` lowers every type once in both modes so `Enums()` is
  complete and recursion is known before the script runs.

### Lowering rules

Every rule below mirrors what the generated decoder or encoder does.

- **Presence.** Input: a key is optional unless `required` (or the struct is
  `novalidate`). Output: optional exactly under `omitempty` / `omitzero`.
- **Null, input.** Pointers, `sql.Null*`, `any`, raw JSON accept null. A nil
  container (slice, map, `[]byte`, `net.IP`) decodes null as empty and then
  meets its rules, so it is `Nullable` only when no rule rejects empty
  (`notempty`, `minlen>0`, `len≠0`). Rules on a `Type` apply to non-null
  values, which is how a pointer skips them.
- **Null and zero, output.** A nil-able type is `Nullable`; `net.IP` emits
  `""` instead, as do `netip.Addr`/`Prefix` and `url.URL` (`Empty`). An enum
  admits the Go zero value (`Enum.Zero`) unless a constant equals it.
  `omitzero` clears all three on the field; `omitempty` clears `Nullable`,
  `Empty` and a string enum's `Zero` (`0` is not JSON-empty). `Strict` /
  `StrictFields` clear them at every depth on script request.
- **`oneof`.** Narrows the type to an `Enum` (no `Decl`, no `Ref`) when no
  transform precedes it in input, and always in output with the zero widening;
  after a transform in input it stays a `OneOf` rule. Every other rule is
  input-only.
- **Enums from constants** are a closed set in both modes, although the
  decoder accepts any value of the underlying type: the closed set is the
  script's assertion, same as `Strict`.
- **Named types.** A generated type → `Ref` with its Go and wire kind only;
  its fields live on its own shape. JSON methods → `External`, wire `Any`;
  text methods → `External`, wire `String`; a plain foreign struct →
  `External`, wire `Object`; a named primitive or container without codec
  methods → its underlying shape with `Name`/`Pkg` kept.
- **Converters.** Each `@Conv` input shape is appended to `OneOf` (without
  rules — they run after conversion); `nullzero` sets `Nullable`.
- **`json.Number`** is a number in output. In input it also carries the
  quoted spelling in `OneOf` and is `Nullable`, because the decoder hands the
  span to `encoding/json`, which reads `"12"` and null.
- **Pointers do not open a pipe level**; slices, arrays and maps do. Map key
  rules come from `keys:`.
- `,string` keeps the Go kind (width) and switches the wire to string with an
  `int-string` / `uint-string` / `number-string` format.

### Output

- `File(path, emitter)` returns the existing file for a path; asking with a
  different emitter fails `Write`.
- `Place` fails `Write` when the same type is placed twice in one mode, or a
  name repeats in a file.
- `Write` validates references, orders each file's placements so a shape
  follows the shapes of its file it references (cycles keep placement order;
  `Context.Later` tells an emitter the reference points forward), renders every
  file, and writes only if every file rendered.
- `Context.Lookup` / `Resolve` / `FileOf` / `Rel` give an emitter the name and
  file of a referenced shape; an unplaced enum type is the one soft reference
  and should be inlined from `Type.Enum`.
- `Writer` renders header lines, then import lines (each deduplicated), then
  the body; `File.Placed` lets an emitter see every name of its file up front,
  and `File.Emitter` lets it read another file's configuration (Kotlin reads
  the package of the file a reference lives in).
- `Report` collects what an emitter could not express; `Write` resets it.

### Emitter options

Every option that makes a decision has a `…For` form taking a function, so a
script can decide per type, per file, per package or per directive; the plain
option is shorthand for a function returning a constant. A per-type decision is
`func(f *gen.File, p gen.Placed) T`: `f.Path` is the output file, `p.Name` the
placed name, and `p.Shape.Decl` the Go type with its package, source file, name
and directives. A per-file decision is `func(f *gen.File) T`. An emitter that
consults a per-type function while rendering a reference passes the file and
placement of the referenced type (`Context.FileOf`, `Context.Lookup`), so the
answer never depends on which file asked. Keyed mappings (`External`, `Func`)
stay maps, with a `…For` function consulted first. Settings that must hold for
a whole TypeScript project (`ImportExt`, `Indent`) stay plain.

## `ts` — TypeScript types

`ts.New(opts...)` renders placed shapes as type declarations, no runtime code.

- A struct is `export interface`; with a `json:",embed"` catch-all it is
  `export type X = {…} & Record<string, Rest>` (an interface index signature
  would have to accept every declared field's type). `ts.TypeAliases()` uses
  `type` for every object, `ts.TypeAliasesFor` per type. Aliases and enum types
  are `export type`.
- Wire drives the TypeScript type: string, number (every integer width and
  big.Int — no bigint), boolean, `T[]`, a tuple for a fixed array (and a
  `[N]byte` under `format:array`), `Record<string, V>` for a map. An unknown
  shape is `unknown`, which already includes null.
- A reference to a placed shape uses its placement name, importing it with
  `import type` from `ctx.Rel(file)` + `ts.ImportExt`. A name that collides
  with a name placed in the importing file is aliased to `<file stem>_<Name>`.
  An unplaced enum type is inlined as its values.
- Output widenings: `Enum.Zero` appends `| ""` or `| 0`, `Nullable` appends
  `| null`; `Empty` changes nothing (the type is already `string`). Converter
  inputs (`OneOf`) join the native type in a union. Rules are not expressible
  in types and are dropped without a report.
- An external type renders by wire (`string`, `unknown`,
  `Record<string, unknown>`); `ts.External(goPath, tsType)` maps it,
  `ts.ExternalFor` per use, and an unmapped external of unknown shape is
  reported. `Printer.ExternalFor` is the hook the schema emitters use for
  their annotations.
- A placement name that is not a TypeScript identifier, or is a reserved word,
  fails `Write`; nothing is renamed silently.
- `ts.Header(line)` writes a first line (e.g. a generated-code marker),
  `ts.HeaderFor` per file;
  `ts.Indent` sets the indentation.
- `ts.Runtime(header)` is the emitter of a shared runtime file: every helper
  the schema emitters use, exported once. A script creates the file where it
  wants it and passes it to `zod.Runtime` / `valibot.Runtime`; their files
  then import the helpers they refer to instead of inlining them. Without it
  every file stays self-contained, inlining the helpers it uses.

`ts/ts_test.go` renders a requests/responses/shared layout from the fixture,
compares it with `ts/testdata` (`go test ./ts -update` rewrites it), and
type-checks the output with `tsc --strict`, from `GGEN_NODE_MODULES` or PATH,
and skips when there is neither.

## `zod` — Zod 4 schemas

`zod.New(opts...)` renders each placed shape as `export const X = …` plus
`export type X = z.output<typeof X>`. A recursive shape (`Shape.Recursive`) is
`export type X = <ts.Printer type>; export const X: z.ZodType<X> = …`, and a
reference that `Context.Later` reports forward is `z.lazy(() => X)`.

- Objects: `z.strictObject` (reject), `z.object` (ignore: zod strips),
  `.catchall(Rest)` (collect); `Optional` is `.optional()`, `Nullable`
  `.nullable()` after the rules (rules apply to non-null values). An `unknown`
  shape is never made nullable.
- Built-ins only where probing showed they match Go: `z.iso.datetime({ offset:
  true })` (strict RFC 3339), `z.base64()` (padded), `z.ipv4()`/`z.ipv6()`
  (no zone — `net.IP`), `z.cidrv4()`/`z.cidrv6()`. Everything else comes from
  `internal/js` helpers, pulled into a file on use: UTF-8 byte and rune
  lengths, decoded length of binary encodings, padded base64url/base32/hex,
  Go duration strings, `,string` number grammars (plus width ranges for narrow
  integers), the `url` rule as "scheme://rest", `netip.Addr` with an IPv6
  zone, and ports of the Go parsers behind text-typed values: `time.Parse`
  for custom layouts, `big.Float.Parse(s, 10)`, `big.Rat.SetString` and
  `url.Parse` (strict colons in http/https hosts, the go1.26+ default).
- Integers: `z.int()` with the Go width's range; 64-bit stays `z.int()` (safe
  integers) and big.Int `z.number().refine(Number.isInteger)`. A `unixmicro`
  or `unixnano` time asserts only integer-ness and is reported: every real
  value of one is past 2^53, so the safe-integer check would reject the whole
  range. `zod.Override` replaces the rendering of any type, e.g. for bigint.
- Transforms use `.overwrite`, so later checks keep chaining. String methods
  (`.min`, `.regex`, `.trim`, …) are used only on schemas that are ZodString;
  anything else gets `.refine`.
- Enums: placed → the constant name (with the output zero as
  `z.union([Role, z.literal("")])`), unplaced or `oneof` → `z.enum` for strings,
  `z.literal([…])` for numbers.
- `zod.Func(ref, action)` appends a chain for an `@Func`, keyed by the
  reference as written or by import path and name (`Rule.FuncPkg` is always
  set, the type's own package for an unqualified reference); `zod.FuncFor`
  decides per rule and placed type first. An unmapped one is reported and
  dropped. `zod.External(goPath, schema)` maps an external type (its
  TypeScript annotation is `unknown`), `zod.ExternalFor` per use; an unmapped
  one of unknown shape is reported. `zod.SchemaName(func(f, p) string)` derives
  the constant's name. `zod.Override(func(ctx, p, t))` renders any type itself.
  `zod.ImportExt`, `zod.Header` and `zod.HeaderFor` as in `ts`.

Helpers are plain functions where a regex would be unreadable: IPv4, IPv6
(embedded IPv4 only as the final part), `netip.Addr` zones and CIDR prefixes
are parsed; the time helper walks the layout the way `time.Parse` does
(ranges, day of month, day of year); the big-number helpers add the exponent
limits a regex cannot express. `internal/js` runs every such helper in Node
against its Go counterpart: `TestIPHelpersMatchGo`, `TestTimeHelperMatchesGo`
(random times formatted in 25 layouts, then mutated), and
`TestBigFloatHelperMatchesGo` / `TestRationalHelperMatchesGo` /
`TestURLHelperMatchesGo` (every string up to four characters over a grammar
alphabet, plus edge cases). A change to a helper must keep these green.

`zod/zod_test.go` compares a requests/responses/shared layout with
`zod/testdata/*.ts`, importing helpers from a `ts.Runtime` file, as
`valibot`'s test does; the runtime file itself is pinned once, by
`ts.TestRuntime` against `ts/testdata/shared/ggen.ts`, and `valibot`'s
`TestInlineHelpers` pins the inline mode. `TestRuntime` type-checks it with `tsc --strict` and runs
inputs whose accept/reject verdict matches the generated Go decoder through
the schemas in Node; it needs `GGEN_NODE_MODULES` (a `node_modules` with `zod`
and `typescript`) and `node` on PATH, and skips otherwise.

## `valibot` — Valibot 1.x schemas

`valibot.New(opts...)` mirrors `zod` (same options, same declaration shape:
`v.InferOutput`, `v.GenericSchema<X>` for a recursive shape, `v.lazy` for a
forward reference). Each type renders as a head schema plus pipe actions, so a
pipe is never nested in a pipe. Where it differs from `zod`, because Valibot's
built-ins differ from Go in other places:

- Native and exact: `v.bytes`/`v.minBytes`/`v.maxBytes` (UTF-8 bytes),
  `v.minLength`… for arrays, `v.minEntries`… for maps, `v.trim`,
  `v.toLowerCase`, `v.startsWith`, `v.values` for a post-transform `oneof`,
  `v.value`/`v.notValue` for `eq`/`neq`, `v.base64()`.
- Replaced by helpers: dates (`v.isoTimestamp` accepts a space separator and
  `+0100`; the helper also checks the day of month), IPs (`v.ip` accepts an IPv6 zone, which `net.IP` rejects), hex and
  the other character classes (`v.hexadecimal` accepts `0x`), CIDR (no
  built-in).
- Floats get `v.finite()`: `v.number()` accepts Infinity, which
  `JSON.parse("1e400")` produces and Go rejects.
- Objects: `v.strictObject` / `v.object` / `v.objectWithRest(shape, Rest)`;
  fixed arrays are `v.strictTuple`; `Optional` wraps as `v.optional(...)`.
  Every object and record schema accepts an array in Valibot, so each is
  wrapped in `ggenObject(...)`, a function written into the file (it needs
  `v`, so it cannot live in the shared runtime) that pipes a
  `v.custom<v.InferInput<T>>` array check before the schema and keeps its
  input and output types.
- The same options as `zod`. `valibot.Func(ref, action)` takes a pipe action, e.g.
  `v.check((x) => x >= 18, "must be an adult")`.

`valibot/valibot_test.go` runs the same layout, goldens, `tsc --strict` and the
same runtime inputs as `zod`, through `v.safeParse`.

## `kotlin` — kotlinx.serialization data classes

`kotlin.New(pkg, opts...)`: every file the emitter renders declares `pkg`; a
reference into a file of another `kotlin` emitter imports it from that
emitter's package, aliased `<Package tail><Name>` on a clash. Options:
`kotlin.Header` / `kotlin.HeaderFor`, `kotlin.External(goPath, type,
imports...)` / `kotlin.ExternalFor`. Types only, like
`ts`: rules are dropped.

What kotlinx.serialization 1.11 does, verified with kotlinc 2.4.20, and the
rendering it forces:

- An absent key fails decoding unless the property has a default, even for a
  nullable property. An optional field therefore defaults to Go's zero value
  (`0`, `0L`, `0u`, `""`, `false`, `emptyList()`, `emptyMap()`, `JsonNull`,
  `null` for a nullable type), so absent means the same on both sides; kotlinx
  omits default-valued properties when encoding, which is exactly what Go
  expects. A class or enum type has no zero literal: an optional one becomes
  nullable, defaulting to `null`. So does a time with a numeric format, whose
  `0` is 1970 rather than Go's zero time.
- Unknown keys fail decoding by default (`Reject`); `Ignore` renders
  `@JsonIgnoreUnknownKeys` with the file-level opt-in. There is no catch-all
  map: `Collect` ignores unknown keys and is reported.
- A string enum type is an `enum class` with `@SerialName` entries named from
  the constants (`RoleAdmin` → `ADMIN`). Enums are JSON strings in kotlinx, so
  an integer enum type is a `typealias` to its integer type, reported. A field
  whose enum still admits the zero value, and an unplaced or `oneof` enum,
  renders as the underlying type; the former is reported.
- Integers map by Go width (`Byte`…`Long`, `UByte`…`ULong`, whose ranges
  kotlinx enforces), floats to `Float`/`Double`, `big.Int` and `json.Number`
  to `JsonPrimitive` (which kotlinx re-encodes as a double beyond `Long` and
  `ULong`; map it with `External` to keep such values), `any` and raw JSON to `JsonElement`. A converter union is
  `JsonElement`, reported.
- Property names are lowerCamel Go names (`ID` → `id`, `URLPath` → `urlPath`)
  with `@SerialName` when the JSON name differs; keywords are backticked.
- Decoding with the default `Json` also accepts a quoted number or an exponent
  for an integer, which Go rejects. That only loosens what a Kotlin client
  accepts from a Go server, which never sends either.

`kotlin/kotlin_test.go` compares a two-package layout with `kotlin/testdata`.
`TestRuntime` compiles it with kotlinc and decodes inputs with known Go
verdicts; it needs `GGEN_KOTLIN_HOME`, a directory holding `kotlinc/`, a JRE
whose name starts with `jdk`, and `kotlinx-serialization-core-jvm.jar` and
`kotlinx-serialization-json-jvm.jar`, and skips otherwise.

## `swift` — Swift Codable types

`swift.New(module, opts...)`: every file the emitter renders belongs to
`module`; a reference into a file of another `swift` emitter imports that
module and qualifies the name (`Shared.User`) when it clashes with a local one.
Types only: rules are dropped.

What `JSONDecoder` / `JSONEncoder` do in Swift 6.3 (swift-foundation), verified
with swiftc, and the rendering it forces:

- A synthesized `init(from:)` fails on an absent key unless the property is
  Optional, and ignores property defaults. Every optional or nullable field is
  therefore `T?`; encoding leaves a nil property out, which Go reads as the
  zero value. A required nullable input field is reported: its nil is left out
  and Go rejects the missing key.
- Unknown keys are always ignored, so `Reject` is not enforced; `Collect` is
  reported (no catch-all). Duplicate keys: the first wins.
- Integers map by Go width (`Int8`…`Int64`, `Int`, `UInt*`) and are exact to
  64 bits with ranges enforced; `1e2` and `1.0` are accepted for an integer,
  which Go rejects. Floats are `Float`/`Double` (range enforced, no Infinity).
  `big.Int` and `json.Number` are `Decimal`, padded base64 `[]byte` is `Data`
  (strict padding, like Go), every other string format is `String`.
- Structs and enums conform to `Codable` plus `swift.Protocols(...)`, default
  `Hashable` (Swift synthesizes it and `Equatable`, which `JSONValue` and
  `Indirect` support too). A mapped External type must conform as well;
  `Protocols()` drops the extras.
- A struct cannot hold itself inline. In a recursive shape, each field whose
  type is a recursive struct held directly (not in an array or map, which store
  elsewhere) is boxed with `@Indirect var x: T? = nil`: an `indirect enum`
  property wrapper from the runtime file, conditionally `Codable`/`Hashable`,
  plus `KeyedDecodingContainer`/`KeyedEncodingContainer` overloads for
  `Indirect<T?>` so an absent key decodes to nil and nil is left out. The
  overloads must constrain `T: Codable`, not `Decodable`: otherwise they are no
  more specific than the stdlib's generic `decode` and the call is ambiguous.
  `swift.Indirect(name)` uses a wrapper of your own; `swift.Indirect("")`
  renders recursive shapes as `final class`es conforming to `Codable` alone,
  with a memberwise `init`, for you to extend. The protocols it drops are
  reported, `Sendable` separately: a class whose properties are mutable cannot
  conform to it even in an extension.
- `Public()` makes everything public, including a `NewRuntime` file, and gives
  structs an `init` (a synthesized one is internal). `swift.ProtocolsFor`,
  `swift.IndirectFor` and `swift.PublicFor` decide per type (a public type
  whose property uses the runtime's `Indirect` needs a public runtime file);
  `swift.HeaderFor` per file, `swift.ExternalFor` per use. A field is boxed
  only when the recursive type it holds renders as a struct, as that type's
  own `IndirectFor` answer says.
- An enum type is `enum X: String` or `enum X: Int…` with cases named from the
  constants (`RoleAdmin` → `admin`); raw values are enforced. An enum admitting
  Go's zero value renders as its raw type, reported, as in `kotlin`.
- A `typealias` is never optional itself; a reference to an alias whose type
  admits null is. Decode a nullable alias at the top level as `X?`.
- `any`, raw JSON, converter unions and unmapped externals of unknown shape
  are `JSONValue`, a Codable enum. It and `Indirect` are written by the
  `swift.NewRuntime(module)` emitter into a file of its own, passed with
  `swift.Runtime(f)`; without it each use is reported.

`swift/swift_test.go` compares a two-module layout with `swift/testdata`.
`TestRuntime` builds `Shared` as a static library and `API` against it, then
decodes inputs, round-trips them through `JSONEncoder` (the result must be
`==` the decoded value, and a nil boxed field must be left out), and
`TestManual` pins the `Indirect("")` + `Protocols()` rendering; `TestRuntime` needs
`GGEN_SWIFTC` (the swift.org Ubuntu 24.04 toolchain runs on Arch with
`libncursesw.so.6` symlinked as `libncurses.so.6` on `LD_LIBRARY_PATH`) and
skips otherwise.

## `model`

The parser the ggen binary and the export share: `//ggen:generate`
annotations, `json:` / `pipe:` / `hint:` tags, go/types resolution, the rule
applicability matrix, `@Func` classification. The tag grammar and every parse
decision are documented in `cmd/ggen/CLAUDE.md`; this file only covers what the
package adds for consumers other than the emitter.

- `ParsePackage(dir) (Package, error)` — every generated type of one
  directory (annotated roots plus what they reach), the type-checked
  `*types.Package`, and `Consts`: every constant of a named string or
  integer type declared in the package or in the exported scope of any
  package it imports transitively, keyed `"import/path.TypeName"`, in
  declaration order. That is the raw material for treating `type Role
  string` + its consts as an enum, and it deliberately crosses packages.
- `ParseFile(file, names) (FileParse, error)` — single-file mode.
- `WalkPackages(targets, act, report)` — the `./...` walker, post-order.
- `StructInfo` / `FieldInfo` carry, besides what the emitter needs, `Doc`
  (comment text with directive lines removed), `Directives` (the `//word:…`
  lines themselves, e.g. `schema:out`, which `ast.CommentGroup.Text` drops),
  the declaring `File`, and the resolved go/types `Type`. `Doc` on a field
  falls back to its trailing comment.
- `Flags` + `Flags.Apply` — the struct-shaping switches the binary exposes as
  flags, for consumers that want a decoder generated with them described the
  same way.

No package-level pass state: everything is scoped to a call, so one process
can parse many packages independently.

## Tests

Each emitter's `testdata` holds its goldens in the layout the emitter wrote
them, so their relative imports resolve where they sit. The repo-root
`package.json` is an npm workspace over `gen/ts`, `gen/zod`, `gen/valibot` and
`integrationtests/gen/js`: `npm ci` at the root installs one hoisted
`node_modules`. The root declares no dependency of its own; typescript, zod
and valibot are pinned once in its `overrides`, which is npm's only
single-source-of-truth mechanism (it has no `catalog:`). A member names what
its own output imports and asks for `"*"`, so the override decides the
version and `npm ls` marks it overridden. That
install is what the tests run against, through `GGEN_NODE_MODULES` or the root
path, and what an editor type-checks the goldens with, through the root
`tsconfig.json`. `zod/testdata/shared/ggen.ts` and its valibot twin are
re-exports of the pinned helpers file, not goldens.

`gen/test-toolchains.sh` downloads pinned Go, Node, JRE, kotlinc,
kotlinx.serialization and Swift into `~/.cache/ggen-toolchains` (override with
`GGEN_TOOLCHAINS`), runs `npm ci` at the repo root, sets
`GGEN_NODE_MODULES` / `GGEN_KOTLIN_HOME` / `GGEN_SWIFTC`, and runs `gen/...` and
`integrationtests/gen`, and fails when a test SKIPPED: each lane skips when its
toolchain is missing, and the script installs them all. It needs only bash,
curl, tar and unzip or python3 on Linux x86_64 or aarch64; arguments go to
`go test`.

`gen_test.go` loads `testdata/api` + `testdata/other` and pins loading, every
lowering rule above in both modes, `Strict`, file ordering, reference
resolution, every placement error, the all-or-nothing write and the report. It
also pins what `Output` refuses before writing anything (a path leaving the
output directory, a nil emitter, a type no pattern loaded), a generated alias
that is also a closed set, and a reference cycle: one spanning two files fails
`Write`, one inside a file is what `Context.Cyclic` reports. `internal/js` and
`internal/names` test their own printers, and every runtime helper that has a
Go counterpart runs in Node against it.

`model/*_test.go` are the parser tests that moved with the code;
`TestParsePackage_docsDirectivesConsts` pins the consumer-facing additions.
Regenerating every module after a parser change must leave `_ggen.go` output
byte-identical (`go build -o ggen ./cmd/ggen`, then the regen recipe in the root
CLAUDE.md).

`swift/swift_test.go` and `kotlin/kotlin_test.go` compile what they render:
Swift in language mode 6, including the `Indirect("")` classes and the
per-type `…For` rendering (against a stand-in module for the external type),
and Kotlin with an encode pass that requires the value to survive both the
default `Json` and `encodeDefaults = true`. Both pin the escaping an awkward
name needs, and the report for two fields that spell one property.

`integrationtests/gen` holds the differential test against real generated
code (see `integrationtests/CLAUDE.md`). When it finds a disagreement, fix the
emitter or helper; add a divergence only for what a schema cannot express.
