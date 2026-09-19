# Cross-language generation plan

Goal: a Go library for small, explicit scripts that generate type
declarations, structs or validation schemas in any other language from
ggen-annotated Go structs. JS comes first (TypeScript types, zod, valibot),
but nothing in the core may assume JS. Python, Rust, Kotlin, Swift, C# or JSON
Schema emitters must fit without changing the core.

Decisions taken: visitor interface, granularity A (the emitter walks whole
shapes), a new module `gen`, parser extracted first, scripts are explicit
with no auto-placement, unplaced enum types inline their values, enum consts
are collected from every package (loaded or imported), step 5 is Kotlin,
step 6 is Swift, and every emitter is checked against ggen's own decoder and
encoder (step 7).

## Principle: the script decides, the library never guesses

A script states, in code:

- which packages to parse;
- which types to emit, selected however it likes (package, name, doc
  directive, a list);
- whether each type is emitted as its input shape (what decoding accepts), its
  output shape (what encoding emits), or both;
- which file every type goes to, and the name it gets there;
- which emitter renders each file.

There is no flag parsing, no default output directory, no file naming
convention and no "emit everything annotated" mode in the library. A reference
to a type the script did not place is an error naming both types, never a
silent fallback.

## Module

New module `github.com/sirkostya009/ggen/gen` under `gen/`.

```
gen/
├── model/        parser, moved out of cli/ (step 1)
├── kinds.go      Mode, GoKind, Wire, Unknown, Op (step 2)
├── load.go       Load, Set, Package, TypeDecl (step 2)
├── shape.go      Shape, Field, Type, Enum, Rule, lowering (step 2)
├── output.go     Output, File, Place, Emitter, Context, Writer, Report (step 2)
├── ts/           TypeScript types emitter and the shared helpers file (step 3)
├── zod/          zod emitter (step 4)
├── valibot/      valibot emitter (step 4)
├── internal/js/  helpers that reproduce Go semantics in TypeScript (step 4)
├── kotlin/       kotlinx.serialization emitter (step 5)
└── swift/        Swift Codable emitter (step 6)
```

`integrationtests/gen/` holds the differential tests (step 7).

The script-facing package is the module root, `gen`. `cli/` requires `gen` for
`gen/model`. Emitters are packages, so a script imports only the ones it uses.

**Installability:** `go install github.com/sirkostya009/ggen/cli@latest`
refuses a module with `replace` directives, so `cli/go.mod` requires `gen` by
the pseudo-version of a pushed commit instead. No tag is needed for that; a
`gen/v0.x.y` tag only changes the spelling of the requirement.

## Step 1 — extract the parser (DONE, awaiting review)

- Move the parse layer (`parse.go types.go tags.go pipe.go applicability.go
customfunc.go introspect.go` + their tests) to `gen/model`.
- Export only what `cli` needs. Emitter-only globals stay in `cli`.
- Keep doc comments on types and fields, including directive lines, so
  scripts can select by them (`//schema:in`). ggen itself still reads only
  `//ggen:generate`. Directives are free-form; the examples use `schema:in` /
  `schema:out`, but the prefix and words are each project's choice.
- Also record every const declared with a named string or integer type
  (`const RoleAdmin Role = "admin"`), in the type's own package, any other
  loaded package, and the exported scope of imported packages, so the model
  can expose the type's values as an enum.
- No behaviour change: regenerate every module and require byte-identical
  `_ggen.go` output; all cli tests pass.

Stop for review.

## Step 2 — the explicit core (DONE, awaiting review)

Built as sketched below, with these differences:

- `Context.Later(ref, mode)` reports a forward reference within the file
  (a recursive type), which lazy schemas need; placement order is otherwise
  resolved by `Write`.
- `Report.Add(where, msg)` beside `Unsupported`.
- `Rule.FuncPkg` (the `@Func` import path) and `Field.Directives`.
- `Type.Empty` is set in input too: the decoder accepts `""` for those types.
- `GoKind` also has `Addr`, `Prefix`, `BigFloat`, `BigRat`, `RawJSON`,
  `Number`; the `url` rule is `URLRule` (the kind is `URL`); transforms are
  `ToLower`/`ToUpper`, the checks `IsLower`/`IsUpper`.
- `Output.File` returns the existing file for a repeated path.
- Formats are string constants on `Type` (`FormatDateTime`, `FormatIPZone`,
  `FormatIntString`, …, `"time:" + layout`).
- Test-file types are not loaded. Enum types have no `Doc` (their declaring
  file's syntax is not kept).

### Concepts

| concept    | what it is                                                                                                  |
| ---------- | ----------------------------------------------------------------------------------------------------------- |
| `Set`      | the packages a script loaded, by pattern. Lookup by import path and type name                               |
| `TypeDecl` | one Go type ggen generates, or a named enum type it references. Package, source file, name, doc, directives |
| `Shape`    | a `TypeDecl` seen in one direction: `t.In()` or `t.Out()`. Fields, presence, rules for that mode            |
| `Output`   | a root directory and the files a script creates in it                                                       |
| `File`     | one output file: a path, the emitter that renders it, the shapes placed in it with their names              |
| `Emitter`  | the visitor: renders one file's placed shapes                                                               |

The core describes Go types and their JSON contract. It knows nothing about
any target language:

| core provides                                                     | emitter decides                                          |
| ----------------------------------------------------------------- | -------------------------------------------------------- |
| Go names, JSON names, doc comments, directives                    | target identifiers, casing, reserved-word escaping       |
| Go kind and width (`int32`, `uint64`, `float32`, `time.Time`, …)  | the target type (`i32`, `Int`, `number`, `z.int()`)      |
| wire shape (number, string with format, array, map, object, null) | how strictly to encode it                                |
| containers, pointers, fixed arrays, map key/value                 | `Option<T>`, `T?`, `T \| null`, `Box<T>`                 |
| presence per mode, null acceptance, unknown keys                  | optional fields, defaults, strictness                    |
| ordered validation/transform rules as plain data (input only)     | whether and how to express each rule                     |
| enums: values from `oneof`, or from a named type's consts         | union, `enum`, `Literal[...]`, or a plain string         |
| where every referenced shape was placed (file + name)             | how to import it                                         |
| recursion, dependency order within a file                         | `Box<T>`, `indirect`, lazy schemas, forward declarations |

Each type carries both the Go type and the wire shape, because they differ
(`time.Time` is a string on the wire; `,string` makes an int a string) and
targets want different ones.

### API sketch

```go
package gen

// Load parses exactly the packages matched by patterns.
func Load(patterns ...string) (*Set, error)

type Set struct{ Packages []*Package }

func (s *Set) Package(path string) *Package
func (s *Set) Type(pkgPath, name string) *TypeDecl // nil if ggen does not generate it
func (s *Set) Types() []*TypeDecl                    // every package, declaration order
func (s *Set) Enums() []*TypeDecl                    // named enum types the loaded types reference

// Package is one Go package. Every named type in the loaded packages points
// at one shared *Package; a package that was only imported (uuid, time) gets
// one too, with no Types.
type Package struct {
	Path, Name string
	Loaded     bool        // matched by a Load pattern
	Types      []*TypeDecl // nil unless Loaded
}

type TypeDecl struct {
	Pkg        *Package
	File       string // source file, e.g. "api/user.go"
	Name, Doc  string
	Directives []string // "schema:in" from a `//schema:in` line
	Enum       *Enum    // a named string/integer type with consts; nil otherwise
	Annotated  bool     // carries //ggen:generate itself, vs reached from one that does
}

// In and Out build a fresh Shape on every call. A Shape is plain data the
// script may edit before placing it.
func (t *TypeDecl) In() *Shape
func (t *TypeDecl) Out() *Shape
func (t *TypeDecl) Has(directive string) bool

func (s *Shape) Field(jsonName string) *Field
func (s *Shape) Strict() *Shape                    // Out: clears every zero-value widening, at any depth
func (s *Shape) StrictFields(json ...string) *Shape // the same for named fields only

type Shape struct {
	Decl      *TypeDecl
	Mode      Mode     // In | Out
	Fields    []*Field // struct; nil for an alias
	Alias     *Type    // `//ggen:generate type Tags []string`
	Recursive bool
	Unknown   Unknown // Reject | Ignore | Collect
	Rest      *Type   // value type of collected unknown keys (json:",embed")
}

type Field struct {
	Shape                 *Shape
	GoName, JSONName, Doc string
	Type                  *Type
	Optional              bool // In: key may be absent; Out: omitempty/omitzero
}

type Type struct {
	GoType   string    // Go spelling as written: "[]string", "*User", "map[string]int", "time.Time", "uuid.UUID"
	Name     string    // declared name of a named type: "Role", "User", "Time", "UUID"; "" for unnamed types
	Pkg      *Package  // package of a named type; nil for unnamed types
	File     string    // source file of a named type in a loaded package; "" otherwise
	Go       GoKind    // String, Bool, Int…Uint64, Float32, Float64, Time, Duration, Bytes, IP, URL, BigInt, Slice, Array, Map, Struct, Any
	Wire     Wire      // String, Integer, Number, Bool, Array, Object, Any
	Format   string    // "date-time", "duration", "base64", "ip", "int-string", …
	Elem     *Type     // Slice, Array, Map value
	Key      *Type     // Map key
	Len      int       // Array
	Ref      *TypeDecl // a generated type or a loaded enum type; resolve its placement with ctx.Resolve
	Rules    []Rule    // In only; ordered
	OneOf    []*Type   // In only; converter variants
	Enum     *Enum     // closed set of values; see "Enums"
	Nullable bool      // JSON null is accepted (In) or can be emitted (Out)
	Empty    bool      // "" can be emitted for a formatted string (net.IP, netip, url.URL zero values); Out only
	External bool      // a named type ggen does not generate (uuid.UUID): Pkg.Path + "." + Name is its key
}

type Enum struct {
	Decl   *TypeDecl   // the named type when the values come from its consts; nil for `oneof`
	Values []EnumValue // declaration order
	Zero   bool        // the Go zero value can appear although it is not in Values; see "Zero values"
}

type EnumValue struct {
	Name  string // const name, "RoleAdmin"; empty for `oneof`
	Value string // JSON literal: `"admin"`, `2`
}

type Rule struct {
	Op        Op       // MinLen, MaxLen, GTE, OneOf, Trim, Lower, Func, …
	Args      []string // ["3"], ["admin","member"], ["_","-"]
	Func      string   // Op == Func: "IsAdult", "pkg.IsAdult"
	Msg       string
	Transform bool
}

// Output is a root directory. Nothing is written until Write.
func NewOutput(dir string) *Output

func (o *Output) File(path string, e Emitter) *File // path relative to dir
func (o *Output) Write() error                      // renders every file, then writes; all or nothing

type File struct{ Path string }

// Place puts a shape into the file under name. Placing the same shape twice
// in one Output is an error.
func (f *File) Place(s *Shape, name string) *File

// Emitter renders one file. Decl is called once per placed shape, in
// dependency order within the file.
type Emitter interface {
	Begin(ctx *Context) error
	Decl(ctx *Context, p Placed) error
	End(ctx *Context) error
}

type Placed struct {
	Shape *Shape
	Name  string
}

type Context struct {
	File   *File
	W      *Writer // body buffer: Printf, Import(line) deduplicated above the body
	Report *Report
}

// Resolve finds where a referenced type was placed in the given mode. A
// missing placement fails Write with an error naming both types.
func (c *Context) Resolve(ref *TypeDecl, mode Mode) Placed
// Lookup is the soft form, for references an emitter can inline instead: an
// enum type that was not placed renders as its values.
func (c *Context) Lookup(ref *TypeDecl, mode Mode) (Placed, bool)
func (c *Context) FileOf(ref *TypeDecl, mode Mode) *File
func (c *Context) Rel(to *File) string // "./users" from "api/index.ts" to "api/users.ts", no extension
```

### Zero values on the output side

Go cannot tell "unset" from the zero value, and the encoder writes whatever
the value is. So an output shape is widened wherever the zero value has a
distinct wire form:

| Go type                                                    | zero on the wire  | widening in the model |
| ---------------------------------------------------------- | ----------------- | --------------------- |
| named string/int with consts, `oneof`                      | `""` / `0`        | `Enum.Zero`           |
| slice, map, `[]byte`, pointer, `any`, `json.RawMessage`    | `null`            | `Type.Nullable`       |
| `net.IP`, `netip.Addr`, `netip.Prefix`, `url.URL`          | `""`              | `Type.Empty`          |
| numbers, bools, plain strings, fixed arrays, value structs | an ordinary value | none                  |

The widening applies only when the zero value can reach the wire. `omitzero`
drops the key when the Go value is zero, so none of the three widenings apply
and the field is `Optional`. `omitempty` drops the key when the JSON form is
empty (`null`, `""`, `[]`, `{}`), so it clears `Nullable` and `Empty` and the
`Zero` of a string enum, but not of an integer enum (`0` is not JSON-empty).

Neither option gives "always present and never zero": both replace the zero
with an absent key, so the field turns `Optional`. Go has no spelling for
that guarantee, because the encoder cannot make it. Without either option
the model keeps the widening, because the library cannot know whether the
server always sets the field. A script that knows says so: `Strict()` clears
all three widenings at every depth, `StrictFields("role", "tags")` clears
them for named fields, or it edits a `Type` directly. `Strict` never changes
`Optional`: a key under `omitzero`/`omitempty` really can be absent. Input
shapes have no widening: `Nullable` there means the decoder accepts `null`,
which is real.

### Other places Go and JSON disagree

Everything else is either handled by the model as a plain fact of the wire
(`time.Time` formats, `[]byte` as base64, `[N]T` as a tuple, `,string`,
`sql.Null*` as `T | null`, converter unions, catch-all maps, promoted embedded
fields) or is a choice only the script can make:

| case                                                | what the model says                                                  | the script's choice                                                                                                                                           |
| --------------------------------------------------- | -------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `int64`, `uint64`, `int`, `big.Int` above 2^53      | `Wire: Integer`, `Go: Int64`; emitters render the plain integer type | nothing by default: the wire carries the exact digits and JS's `number` is JS's problem. A script that needs `bigint` overrides the field type or the emitter |
| `any`, `json.RawMessage`, an interface with methods | `Wire: Any`                                                          | `unknown`, or a type the script supplies for that field                                                                                                       |
| external types (`uuid.UUID`, `decimal.Decimal`)     | `External`, best-known wire shape                                    | map the Go path to a target type (emitter option)                                                                                                             |
| enum values built at runtime (`Role("guest")`)      | `Enum.Values` are the declared consts only                           | the closed set is an assertion, like `Strict()`                                                                                                               |
| integer enum whose consts include `0`               | `0` is a member; no `Zero` widening                                  | nothing                                                                                                                                                       |
| zero `time.Time` (`"0001-01-01T00:00:00Z"`)         | an ordinary `date-time` string                                       | nothing at the type level; `omitzero` if the key should vanish instead                                                                                        |

### Syntax belongs to the emitter

Schema libraries disagree on everything past the type names: zod chains
methods, valibot wraps a pipe, arktype is a string DSL, typebox takes an
options object, pydantic uses keyword arguments. The core has no concept a
target would have to fit: an emitter receives plain data and writes every
character itself. For `Name string \`pipe:"trim minlen=1 maxlen=64"\`` the
data is `Rules: [{Trim, Transform}, {MinLen, ["1"]}, {MaxLen, ["64"]}]`, and:

| target   | rendering                                                       | note                                        |
| -------- | --------------------------------------------------------------- | ------------------------------------------- |
| zod      | `z.string().trim().min(1).max(64)`                              | chain, declared order                        |
| valibot  | `v.pipe(v.string(), v.trim(), v.minBytes(1), v.maxBytes(64))`   | pipe, declared order                         |
| arktype  | `"string.trim \|> string >= 1 & string <= 64"` inside `type({…})` | string DSL with its own grammar             |
| typebox  | `Type.String({ minLength: 1, maxLength: 64 })`                  | options object; `trim` has no twin: reported |
| pydantic | `Field(min_length=1, max_length=64)` + `BeforeValidator(str.strip)` | keyword arguments plus a decorator       |

Recursion (`z.lazy`, `v.lazy`, an arktype scope, `Type.Recursive`, a Python
forward reference) and imports are emitter-local the same way. The core
promises only declared rule order, placement lookup for references, and a
`Report` for rules without a twin. Emitters therefore share nothing beyond
the model and the writer helpers; that is the price of no target ever being
a bad fit.

Stop for review.

## Step 3 — TypeScript types (DONE, awaiting review)

Package `gen/ts`, `ts.New(opts...)`. Options: `ImportExt`, `Header`, `Indent`,
`External(goPath, tsType)`, `TypeAliases`. Objects are interfaces, or a type
intersection with a catch-all map; numbers are `number`; placed enums are
referenced, unplaced ones inlined; clashing imports are aliased
`<file stem>_<Name>`; invalid or reserved placement names fail `Write`. A
catch-all map is `{…} & Record<string, T>`: an index signature would have to
widen to every field's type, so extra keys would accept any of them. The
core gained `Writer.Header` and `File.Placed` for it. Output is type-checked
with `tsc --strict` in the test.

## Step 4 — zod and valibot (DONE, awaiting review)

valibot: package `gen/valibot`, same options as zod, head schema + pipe
actions. Native actions where they match Go (byte lengths, entries, trim,
values); helpers for dates, IPs, hex and character classes, CIDR; `v.finite()`
on floats. Every object and record schema is wrapped in a `ggenObject(...)`
function written into the file, because valibot accepts arrays there. Same
golden, `tsc` and runtime tests as zod, all passing.

zod: package `gen/zod`. Options `Func`, `External`, `SchemaName`, `Override`,
`ImportExt`, `Header`. Library built-ins where they match Go, exact helpers
from `gen/internal/js` elsewhere (shared with valibot). `ts` exports its
`Printer` and `Imports` for recursive annotations and import planning.
Tested with golden files, `tsc --strict`, and a Node runtime check of inputs
with known Go verdicts.

Helpers (`gen/internal/js`): where a library built-in differs from Go, the
schema calls a helper instead. String values Go parses with a stdlib parser
get a port of that parser: `time.Parse` for custom layouts, strict RFC 3339
with day-of-month checks, `big.Float.Parse(s, 10)`, `big.Rat.SetString` and
`url.Parse`, next to the IP, CIDR, duration, number and binary-encoding
helpers. Each is run in Node against its Go counterpart on generated inputs
(every short string over a grammar alphabet, or random formatted times with
mutations). `ts.Runtime(header)` writes all helpers into one file, and
`zod.Runtime(f)` / `valibot.Runtime(f)` make schema files import from it;
without it a file inlines the helpers it uses.

Custom `@Func` mappings and external type mappings are emitter options. Stop
for review after each.

## Step 5 — a non-JS emitter as proof (Kotlin DONE, awaiting review)

Package `gen/kotlin`, kotlinx.serialization data classes, researched and
verified with kotlinc 2.4.20 + kotlinx.serialization 1.11. The only core change
it needed was `File.Emitter()`, to read the package of the file a reference
lives in. Everything else (defaults as Go zero values, enum classes, unknown
keys, integer widths, imports across packages) fit the existing model. An
optional time with a numeric format defaults to `null`, since its `0` is a
real time. kotlinx re-encodes `big.Int` values beyond `Long`/`ULong` as
doubles; an `External` mapping is the way out.

## Step 6 — Swift (DONE, awaiting review)

Package `gen/swift`, `swift.New(module, opts...)`, verified with Swift 6.3.3.
No core change. Structs and enums conform to `Codable` plus
`swift.Protocols(...)` (default `Hashable`). Optional and nullable fields are
`T?`, since `JSONDecoder` ignores property defaults. A field that holds a
recursive struct directly is boxed with `@Indirect`; `swift.Indirect(name)`
takes a wrapper of your own, and `swift.Indirect("")` renders recursive types as
`final class`es for you to extend. `JSONValue` (any shape) and `Indirect` live
in a `swift.NewRuntime(module)` file passed with `swift.Runtime(f)`.
`Public()` makes a library module's types public. Integer, float and base64
strictness match Go. Unknown keys are ignored and `1e2` decodes as an integer,
which only loosens a client.

### Per-type options

Every emitter option that makes a decision has a `…For` form: per type
`func(f *gen.File, p gen.Placed) T`, per file `func(f *gen.File) T`, with the
plain option as shorthand. That covers `HeaderFor`, `ts.TypeAliasesFor`,
`ExternalFor` everywhere, `zod`/`valibot` `FuncFor`, `SchemaName` and
`Override`, and `swift.ProtocolsFor`, `swift.PublicFor`, `swift.IndirectFor`.

## Step 7 — agreement with ggen (DONE, awaiting review)

`integrationtests/gen` has a non-test fixture (`fixture.go` + `other/`) with
generated ggen code, and three tests that place every type's `In()` and
`Out()` and try about 140 probe values at every field:

- `TestDifferential` (zod, valibot): Go and each input schema accept and
  reject the same inputs, transforms produce Go's values, Go reads a
  schema's parsed value as it read the probe, and the output schemas accept
  everything Go marshals.
- `TestDifferentialSwift`, `TestDifferentialKotlin`: the output types decode
  everything Go marshals, and Go reads what the input types re-encode as the
  value it read directly.

Disagreements a target cannot avoid are predicates with a reason, and each
must explain at least one case. JS: exponents and integers beyond 2^53 in
`JSON.parse`, closed enum sets, converter checks. Swift and Kotlin: no
catch-all map, and Kotlin's big integers. The tests also found a ggen bug:
an omitted field of a named slice or map type decoded to an empty value
instead of nil; `cli` now zeroes it, pinned by
`TestAlias_OmittedContainerFieldIsNil`.

## Usage examples

Proposed API, not built. Small string helpers (`kebab`, `snake`, `quote`,
`py`, `rs`, `schemaOf`) are elided.

The Go types the examples use:

```go
package api // github.com/me/app/api

//ggen:generate
//schema:in
type CreateUser struct {
	Name    string   `json:"name"  pipe:"required trim minlen=1 maxlen=64"`
	Email   string   `json:"email" pipe:"required tolower contains=@"`
	Age     *int     `json:"age"   pipe:"gte=0 @IsAdult"`
	Address Address  `json:"address"`
}

//ggen:generate
//schema:out
type User struct {
	ID      int64     `json:"id"`
	Name    string    `json:"name"`
	Role    Role      `json:"role"`
	Status  string    `json:"status" pipe:"oneof=active|banned"`
	Plan    Plan      `json:"plan,omitzero"`
	Tags    []string  `json:"tags,omitempty"`
	Created time.Time `json:"created"`
	Avatar  uuid.UUID `json:"avatar"`
	Address Address   `json:"address"`
	Manager *User     `json:"manager,omitempty"`
}

// Role is a plain named string with consts: the model reads it as an enum.
type Role string

const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

type Plan int

const (
	PlanFree Plan = iota + 1
	PlanPro
)

// Address is reached from both; ggen generates it without its own annotation.
type Address struct {
	City string `json:"city" pipe:"required"`
	Zip  string `json:"zip"  pipe:"numeric len=5"`
}
```

### 1. Smallest explicit script

One package, one file, every type as input, names unchanged.

`gen.Load("./api")` parses the package: every type ggen generates there. It
does not decide where anything goes. Putting all of them into `api.ts` is this
script's loop; example 4 splits the same set by source file instead.

```go
package main

import (
	"log"

	"github.com/sirkostya009/ggen/gen"
	"github.com/sirkostya009/ggen/gen/zod"
)

func main() {
	set, err := gen.Load("./api")
	if err != nil {
		log.Fatal(err)
	}
	out := gen.NewOutput("web/src/api")
	f := out.File("api.ts", zod.New())
	for _, t := range set.Types() {
		f.Place(t.In(), t.Name)
	}
	if err := out.Write(); err != nil {
		log.Fatal(err)
	}
}
```

```ts
// web/src/api/api.ts
import { z } from "zod";

// A const and a type sharing a name is legal TypeScript and the usual zod
// idiom: one lives in the value namespace, the other in the type namespace.
// When it would shadow a type the project already has, rename the schema; see
// example 6.
export const Address = z.strictObject({
	city: z.string(),
	zip: z
		.string()
		.regex(/^[0-9]+$/)
		.length(5)
		.optional(),
});
export type Address = z.output<typeof Address>;

export const CreateUser = z.strictObject({
	name: z.string().trim().min(1).max(64),
	email: z.string().toLowerCase().includes("@"),
	age: z.int().gte(0).nullable().optional(), // @IsAdult: reported, no twin
	address: Address.optional(),
});
// …User…
```

### 2. Input and output chosen by directive

Shared types appear in whichever modes something references them in, under
names the script picks.

```go
set, _ := gen.Load("./api")
out := gen.NewOutput("web/src/api")
requests := out.File("requests.ts", zod.New())
responses := out.File("responses.ts", ts.New())

for _, t := range set.Types() {
	switch {
	case t.Has("schema:in"):
		requests.Place(t.In(), t.Name)
	case t.Has("schema:out"):
		responses.Place(t.Out(), t.Name)
	}
}
addr := set.Type("github.com/me/app/api", "Address")
requests.Place(addr.In(), "AddressInput")
responses.Place(addr.Out(), "Address")

out.Write()
```

```ts
// requests.ts
export const AddressInput = z.strictObject({ city: z.string(), zip: /* … */ });
export const CreateUser = z.strictObject({ /* … */ address: AddressInput.optional() });

// responses.ts
export interface Address { city: string; zip: string }
export interface User {
  id: number;
  role: "admin" | "member" | ""; // "": Go can leave it unset; example 3b tightens this
  status: "active" | "banned" | "";
  plan?: 1 | 2;                  // omitzero: the key is absent instead of 0
  tags?: string[];               // omitempty never writes null
  address: Address;
  manager?: User;                // omitempty: a nil pointer is omitted, not null
  /* … */
}
```

Leaving out the `Address` placements fails `Write`:

```
requests.ts: CreateUser (input) references api.Address (input), which is not placed in this output
responses.ts: User (output) references api.Address (output), which is not placed in this output
```

### 3. Both versions of one type, side by side

```go
user := set.Type("github.com/me/app/api", "User")
f := out.File("user.ts", zod.New())
f.Place(user.In(), "UserInput")
f.Place(user.Out(), "User")
```

```ts
export const UserInput = z.strictObject({
	/* rules, required keys, converter inputs */
});
export const User = z.strictObject({
	/* omitempty/omitzero keys optional, zero-value widenings, no rules */
});
```

### 3b. Tightening output: dropping the zero-value widenings

By default an output enum admits Go's zero value and a slice admits `null`,
because Go code can leave a field unset. When the server guarantees the
field is always set, the script says so, for the whole shape or for single
fields.

```go
role := set.Type("github.com/me/app/api", "Role")
responses.Place(role.Out(), "Role") // optional: unplaced, the values are inlined

user := set.Type("github.com/me/app/api", "User").Out().Strict()
responses.Place(user, "User")

// or per field:
u := set.Type("github.com/me/app/api", "User").Out().StrictFields("role")
// or directly:
u.Field("status").Type.Enum.Zero = false
```

```ts
export type Role = "admin" | "member";
export interface User {
	role: Role; // placed: referenced by name
	status: "active" | "banned"; // `oneof`: inlined
	/* … */
}
```

`Strict()` also turns a `[]string` field from `string[] | null` into
`string[]` and a `net.IP` from `"" | ip` into `ip`; see "Zero values on the
output side" for the full list. Other emitters read the same `Enum`: pydantic
renders `Literal["admin", "member"]`, Rust an `enum Role` with
`#[serde(rename = "admin")]` variants named from `EnumValue.Name`. Tightening
is a promise about Go's data the library cannot check, which is why the
script has to make it.

### 4. One file per type, names and paths from a convention the script owns

```go
out := gen.NewOutput("web/src/models")
for _, t := range set.Types() {
	if !t.Annotated {
		continue // helper types go to shared.ts below
	}
	out.File(kebab(t.Name)+".ts", ts.New()).Place(t.Out(), t.Name) // create-user.ts, user.ts
}
shared := out.File("shared.ts", ts.New())
for _, t := range set.Types() {
	if !t.Annotated {
		shared.Place(t.Out(), t.Name)
	}
}
out.Write()
```

```ts
// user.ts
import type { Address } from "./shared";
export interface User {
	/* … */ address: Address;
	manager?: User | null;
}
```

The TypeScript emitter builds `"./shared"` from `ctx.FileOf(ref, mode)` and
`ctx.Rel`; the script decided both paths.

Mirroring the Go source files instead, `api/user.go` → `user.ts`:

```go
for _, t := range set.Types() {
	name := strings.TrimSuffix(filepath.Base(t.File), ".go") + ".ts"
	out.File(name, ts.New()).Place(t.Out(), t.Name) // File returns the same *File for the same path
}
```

### 5. Several packages, each into its own directory

```go
set, _ := gen.Load("./api/...", "./billing")
out := gen.NewOutput("web/src/generated")
for _, p := range set.Packages {
	f := out.File(p.Name+"/index.ts", zod.New())
	for _, t := range p.Types {
		f.Place(t.In(), t.Name)
	}
}
out.Write()
```

A `billing.Invoice` referencing `api.User` imports it from `"../api"`.

### 6. Emitter options: `@Func` twins and external types

```go
v := zod.New(
	zod.Func("IsAdult", `.refine((x) => x >= 18, "must be an adult")`),
	zod.External("github.com/google/uuid.UUID", "z.uuid()"),
	zod.ImportExt(".js"),
	zod.SchemaName(func(name string) string { return name + "Schema" }),
)
out.File("api.ts", v).Place(user.In(), "UserInput")
```

```ts
export const UserInputSchema = z.strictObject({
	/* … */
});
export type UserInput = z.output<typeof UserInputSchema>;
```

The placement name stays the type's name; `SchemaName` only derives the
const's.

### 7. Reports the script acts on

```go
if err := out.Write(); err != nil {
	log.Fatal(err)
}
for _, issue := range out.Report().Issues {
	fmt.Fprintln(os.Stderr, issue) // api.CreateUser.age: @IsAdult has no zod twin (dropped)
}
if len(out.Report().Issues) > 0 && os.Getenv("CI") != "" {
	os.Exit(1)
}
```

### 8. From scratch: Python pydantic

```go
type pydantic struct{}

func (pydantic) Begin(ctx *gen.Context) error {
	ctx.W.Import("from __future__ import annotations")
	ctx.W.Import("from pydantic import BaseModel, ConfigDict, Field")
	return nil
}

func (pydantic) Decl(ctx *gen.Context, p gen.Placed) error {
	s := p.Shape
	ctx.W.Printf("class %s(BaseModel):\n", p.Name)
	if s.Unknown == gen.Reject {
		ctx.W.Printf("    model_config = ConfigDict(extra=\"forbid\")\n")
	}
	for _, f := range s.Fields {
		args := []string{"alias=" + quote(f.JSONName)}
		if f.Optional {
			args = append(args, "default=None")
		}
		for _, r := range f.Type.Rules {
			switch r.Op {
			case gen.MinLen:
				args = append(args, "min_length="+r.Args[0])
			case gen.MaxLen:
				args = append(args, "max_length="+r.Args[0])
			case gen.GTE:
				args = append(args, "ge="+r.Args[0])
			default:
				ctx.Report.Unsupported(f, r, "no pydantic Field argument")
			}
		}
		ctx.W.Printf("    %s: %s = Field(%s)\n", snake(f.GoName), py(ctx, f.Type, s.Mode), strings.Join(args, ", "))
	}
	ctx.W.Printf("\n")
	return nil
}

func (pydantic) End(*gen.Context) error { return nil }

// py maps a Type; for a Ref it imports the placed name:
//   pl := ctx.Resolve(t.Ref, mode)
//   if pl.File != ctx.File { ctx.W.Import("from ." + module(pl.File) + " import " + pl.Name) }
```

```go
set, _ := gen.Load("./api")
out := gen.NewOutput("clients/python/app")
models := out.File("models.py", pydantic{})
for _, t := range set.Types() {
	models.Place(t.Out(), t.Name)
}
out.Write()
```

### 9. From scratch: Rust serde, one module per Go package

```go
type rust struct{}

func (rust) Begin(ctx *gen.Context) error {
	ctx.W.Import("use serde::{Deserialize, Serialize};")
	return nil
}

func (rust) Decl(ctx *gen.Context, p gen.Placed) error {
	s := p.Shape
	ctx.W.Printf("#[derive(Debug, Clone, Serialize, Deserialize)]\n")
	if s.Unknown == gen.Reject {
		ctx.W.Printf("#[serde(deny_unknown_fields)]\n")
	}
	ctx.W.Printf("pub struct %s {\n", p.Name)
	for _, f := range s.Fields {
		ctx.W.Printf("    #[serde(rename = %q)]\n    pub %s: %s,\n", f.JSONName, snake(f.GoName), rs(ctx, p, f.Type))
	}
	ctx.W.Printf("}\n\n")
	return nil
}

func (rust) End(*gen.Context) error { return nil }

// rs: Int64 → i64, Time → chrono, Nullable → Option<…>, and a Ref back to the
// shape being declared (s.Recursive) → Box<…>.
```

```go
for _, p := range set.Packages {
	f := out.File("src/"+p.Name+".rs", rust{})
	for _, t := range p.Types {
		f.Place(t.Out(), t.Name)
	}
}
```

### 10. One artifact across everything: JSON Schema

The emitter collects in `Decl` and writes in `End`; the script puts every
shape into the single file.

```go
f := out.File("schema.json", &jsonSchema{})
for _, t := range set.Types() {
	f.Place(t.In(), t.Pkg.Name+"."+t.Name)
}
```

## Testing

- Step 1: byte-identical regen + existing tests.
- Steps 3–6: golden files, plus the target's own compiler or type checker on
  the output (`tsc --strict`, `kotlinc`, `swiftc`), gated on the toolchain:
  `GGEN_NODE_MODULES`, `GGEN_KOTLIN_HOME`, `GGEN_SWIFTC`.
- Placement errors: unplaced references, a shape placed twice, two shapes
  under one name in one file.
- Step 7: the differential tests against ggen's generated code, one lane per
  target (zod, valibot, the `ts` types, Swift, Kotlin). Their JS dependencies
  are pinned in the repo-root npm workspace; `gen/test-toolchains.sh` installs
  every toolchain, runs both suites and fails on a skipped lane.

## Open questions

- README and SKILL.md do not cover `gen` yet.
- Tagging `gen/v0.x.y`: optional, since cli installs from a pseudo-version.
  `integrationtests/go.mod` keeps its `replace`; nothing installs it.
