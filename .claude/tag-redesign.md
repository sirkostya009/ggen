# ggen struct-tag redesign — design spec (IMPLEMENTED 2026-06-25)

Status: **implemented.** Hard break — `ggen:`/`mod:` no longer exist. The whole
corpus + README/SKILL/CLAUDE are migrated; legacy parsers/resolvers deleted.
Open follow-ups: foreign-package converter inputs, top-level alias-type pipe
support. Captures the 2026-06-24 brainstorm.
Supersedes the three-tag grab-bag flagged in `.claude/backlog.md`
("Reconsider the struct-field tag design as a whole"). One decision left: keep
or drop the raw-bytes escape hatch (open item 1).

## Goal

Kill the `json:` grab-bag + the `ggen:`/`mod:` split. Replace with tags
partitioned by **role**, a single ordered+staged `pipe:` that owns all
decode/transform/validate, and a typed multi-shape decode stage so a field can
accept several JSON shapes and funnel them into one Go type — **with ggen always
doing the JSON parse** and custom funcs staying pure, type-safe transforms.

## Tags (partitioned by role)

| tag | role | sequenced? |
| --- | --- | --- |
| `json:` | wire shape (stdlib/jsonv2) | — |
| `pipe:` | decode + transform + validate | yes — stages |
| `hint:` | prealloc capacity only | no, `inner:`-addressable |

```go
F string `json:"name,omitempty" pipe:"trim minlen=1 @Check" hint:"32"`
```

- **`json:`** — `name`, `omitempty`, `omitzero`, `string`, `inline`,
  `format:X` (last), `-`. Unchanged; stays stdlib/jsonv2-compatible.
- **`hint:`** — replaces `ggen:"hintlen=N"`. Just the prealloc capacity.
  Per-level via `inner:`: `hint:"32 inner:8"` (outer cap 32, inner-row cap 8).
  Lifted at parse, order-independent. (`nullzero` is NOT here — it became a
  decode-stage variant, see below. The old generic `ggen:` bucket is gone.)

Decision rule for any future token: stdlib wire → `json`; runs/decodes →
`pipe`; prealloc → `hint`. No catch-all bucket left to dump into.

## `pipe:` grammar

```
pipe        := stage ( "~" stage )*
first stage := variant ( "/" variant )*        // decode: JSON-shape dispatch
later stage := step ( WS step )*                // transform/validate on the value
                with inner:(…) / keys:(…) groups for container levels
```

**`~` is optional sugar.** It is only needed to force a stage boundary the
auto-detector wouldn't infer. Without a `~`, the decode stage is the leading
run of *variant keywords* — `.`, `nullzero`, or a `/`-joined converter list —
and everything after is value steps. So:

- `nullzero gte=0 lte=150` — bare `nullzero` variant + two value validators.
- `required trim minlen=1 maxlen=10` — presence (lifted) + value steps.
- `@Check minlen=1` — a **lone** leading `@Func` is a value step (validator),
  NOT a converter. A converter must be signalled with `/` (`@Conv / .`), a
  leading `.` (`./@Conv`), or an explicit `~` (`@Conv ~ …`) — i.e. the dot/slash
  is only needed once you bring in a parse func.

### Tokens — each one job

| token | role |
| --- | --- |
| WS (space/tab) | step separator within a stage |
| `/` | variant alternatives — **one per JSON shape** |
| `~` | stage separator (decode → value → …) |
| `.` | native-`T` variant (the plain value) |
| `nullzero` | null-shape variant → `zero(T)` |
| `@Func` / `@pkg.Func` | converter / mod / validator (classified by signature) |
| `inner:` | scope one container level down — bare = 1 step, `inner:(…)` = group |
| `keys:` | map-key scope — bare = 1 step, `keys:(…)` = group |
| `( )` | group the steps belonging to an `inner:`/`keys:` level; nest to go deeper |

**Reader signal:** a `/`-variant list or an explicit `~` means custom/multi-
shape parsing is happening. A leading `nullzero`/`.` is a bare decode variant.
Otherwise it's a plain linear pipe on the natively-decoded value.
(The old `;` level-pop is retired — parentheses bound each level explicitly.)

### Lexing — whitespace-separated, optional single quotes

- Steps are separated by whitespace (no commas).
- Structural glyphs (`/ ~ ( )`, plus the `inner:`/`keys:` prefixes) are
  significant with or without surrounding spaces: `./@FromStr` and
  `. / @FromStr` lex identically.
- A rule value or inline message **may** be single-quoted; quotes are
  **required only if the span contains whitespace**. Unquoted spans end at the
  next whitespace/glyph; quoted spans are literal until the closing `'`.
  A literal `'` inside a quoted span is `\'` (rare).

```go
pipe:"trim minlen=1 contains='foo bar' @Check:'value is bad' @Even:must_be_even"
```

## Decode stage (`/` variants)

ggen peeks the incoming JSON shape (first byte) and routes to the single variant
claiming that shape. **ggen always does the JSON parse**; funcs never see raw
bytes (except the optional `[]byte` escape hatch — open item).

- **`.`** — native decode of `T`; claims `T`'s natural wire shape.
- **`nullzero`** — claims JSON `null`, produces `zero(T)`. The only way to
  accept null on a non-pointer value (a typed pipe can't express nullability via
  a pointer). Composes with `inner:` for free — an element pipe carrying a
  `nullzero` variant IS element-level null acceptance. Whole-struct/CLI form
  (`-nullzero`, `//ggen:generate nullzero`) injects it into every field.
- **`@Conv`** — `func(W)(T,error)` / `func(W)(T,bool)`. ggen scans the
  converter's **input type `W`** natively (the "last parser" param type), then
  calls the func. `W` may be a primitive **or a ggen-decodable struct/slice/map**
  — in which case ggen **delegates to its decode machinery**
  (`DecodeFrom` → `UnmarshalJSON` → `UnmarshalText` → `encoding/json`) to build
  `W`, then converts. Intermediate structs' own `pipe:` tags run during that
  decode — pipes nest naturally.

### Dispatch rules

- **One variant per JSON shape.** Six shapes: null / bool / number / string /
  array / object.
- **At most one object-rooted variant** (struct/map input) and one
  array-rooted (slice/array input) — two of either = codegen error (can't
  peek-distinguish two objects without content dispatch, which we don't do).
- **Unmatched incoming shape → hard parse error.** No implicit `any` catch-all;
  enumerate the shapes you accept.
- **`any`-rooted variant** is legal but must be the *sole* variant (it overlaps
  all shapes).
- A plain linear pipe = the single implicit `.` variant rooted at `T` (today's
  behavior). `.` is never required for ordinary parsing.

## Value stage(s) (after `~`)

Operate on the produced value; the **working type flows** through the pipe.
Pre-conversion steps run at the intermediate type, post-conversion at the
final — type-safe at each hop. `applicability.go` gates each builtin against the
**current working type**, not the field type.

`inner:`-groups manage container levels within a stage; steps outside a group
apply to the whole container:

```go
Tags []string `pipe:"./@SplitCSV ~ minlen=1 inner:(trim maxlen=20) maxlen=100"`
//              array→native | string→split  ~ slice-pre · per-elem · slice-post
```

| position | phase |
| --- | --- |
| before any group | container pre (before loop) |
| inside `inner:(…)` | per element (in loop) |
| after the group | container post (after loop) — **new capability** |

Cross-level order = 3 phases (before-loop / in-loop / after-loop), NOT literal
per-iteration interleave (the element loop is atomic). Within a level, order is
literal. Each `inner:`/`keys:` scope is bounded by its parentheses (or is a
single step when bare).

## Three orthogonal axes

A field has three independent concerns the design must NOT conflate:

| axis | states | handled by |
| --- | --- | --- |
| **presence** | key absent / present | `required`, `optional` |
| **null shape** | value is `null` / not | `nullzero` variant (decode stage) |
| **value** | the decoded content | value-stage steps |

- **`required` / `optional` are presence, not value steps.** They assert on the
  JSON *key*, evaluated at object-close via the seen-flag — if the key is
  absent the pipe never runs (no value to feed it). They are **lifted at parse,
  position-independent** (like `hint`), but read first by convention (before the
  first `~`). `optional` absent → zero value, value stage skipped.
- **Absent ≠ null.** A field can be `required` (key must appear) AND `nullzero`
  (its value may be `null` → zero). Drop `nullzero` and a literal `null` on a
  present key hard-errors.
- **`notempty` is NOT presence** — it's a value validator (non-empty
  string/slice), runs in the value stage normally.
- Possible future symmetry (not building now): an absent-handler supplying a
  default the way `nullzero` supplies the zero. Today `required` errors on
  absent; the default is just the zero value.

## Builtin vocabulary (value-stage steps)

**Presence (lifted, object-close):** `required`, `optional`.

**Validators:** `notempty`; `len/minlen/maxlen=N`;
`runes/minrunes/maxrunes=N`; `gt/gte/lt/lte/eq/neq=N`; `multiple=N`;
`oneof=a|b|c`; `email`, `url`, `ascii`, `printable`, `alphanum`, `numeric`,
`lower`, `upper`, `hexadecimal`; `starts/ends/contains=X`.

**Mods:** `trim`, `lower`, `upper`, `trimleft/trimright=X`, `replace=old|new`;
`clamp=lo|hi` (either bound omittable).

(`|` stays the intra-rule arg separator — that's why the pipe step separator is
whitespace, and variant-alternatives use `/`, not `|`.)

## Custom func signatures (finalized)

A function is one of two roles — **transform** or **validator** — detected
purely from its signature. The detection is total because of one rule:

> **A single `bool` or `error` return is ALWAYS a validator.** A transform that
> *produces* a bool/error uses the two-return form.

| signature | role |
| --- | --- |
| `func(A) error` | validator |
| `func(A) bool` | validator (message-capable) |
| `func(A) B` (B not bool/error) | transform — **mod** if A==B, **converter** if A≠B; infallible |
| `func(A) (B, error)` | transform, fallible (error) |
| `func(A) (B, bool)` | transform, fallible (message-capable) |

- **mod vs converter is not a separate signature** — it's just whether
  in-type == out-type. A converter (A≠B) shifts the working type; as the first
  decode-stage step it sets which JSON kind ggen scans (its input type `A`).
- The only carve-out from the rule: you cannot write an infallible transform
  that outputs `bool`/`error` (use the two-return form). Acceptable — rare.
- **`func(bool) bool` is banned** (codegen error). It's the degenerate case of
  the rule above — a `bool`-input/`bool`-output func that can't be a mod
  (single-`bool`-return is reserved for validators) and reads ambiguously as a
  validator. To validate a `bool` field, use `func(bool) error`.
- `A`/`B` type-checked at codegen against the working type at that position.
  Under `inner:` → element type; under `keys:` → `string`. A converter input `A`
  may be a ggen-decodable struct/slice/map (delegates to its decoder).
- Resolution: same-pkg by name, `@pkg.Func` cross-pkg via the source file's
  import block (blank/aliased imports OK).
- Fixed principle: ggen does the JSON parse; funcs are pure typed transforms.

### Failure semantics

| outcome | kind | under `-multierr` |
| --- | --- | --- |
| validator fails (`error`≠nil / `bool`=false) | validation | **accumulates** |
| mod/convert fails (`(_,error)`≠nil / `(_,bool)`=false) | parse | **immediate return** |

**Mods can't be collected, validators can.** Consistent with "parse errors
always return immediately."

### Inline messages (bool-forms only)

```go
pipe:"@MustBeEven:'value must be even'"
```

- Message = the span after the first `:` (single-quoted if it has spaces).
- Valid **only** on `func(_)bool` / `func(_)(_,bool)` (they carry no error).
  On error-forms or pure mods → codegen error.
- New runtime types: `validation.PredicateError{Name,Value,Pos,Msg}`
  (validation, accumulates) and `validation.ModError{Name,Value,Pos,Msg}`
  (parse, immediate). Empty `Msg` → generated default text.

## New capabilities beyond consolidation

1. Interleaved mod/validator order in one namespace.
2. Multi-shape decode → one Go type via `/` variants (loose-int,
   struct-or-scalar) — fully type-safe, ggen owns parsing.
3. After-loop container validation (steps outside the `inner:(…)` group).
4. Per-level `hint:`; element-level null acceptance (`nullzero` under `inner:`).
5. Struct-input converters delegate to ggen's own generated decoders.

## Open items

1. **`[]byte`/`json.RawMessage` escape hatch** — the one non-type-safe head:
   `func([]byte)(T,error)` gets the raw JSON span (via `SkipValue`) and parses
   it itself. Needed only for content-discriminated unions (two object layouts
   distinguished by a `"type"` field) — the shape-dispatch variant system can't
   do that. **Recommendation: drop it** (undercuts "ggen always parses"; use a
   `json.RawMessage` field + plain Go for content-discrimination). Last call.

## Resolved

- **Migration: hard break.** `ggen:`/`mod:` stop existing — no aliases, no
  deprecation window. `ggen:` retired entirely (sizing → `hint:`, validators →
  `pipe:`, `nullzero` → decode-stage variant).
- **Signature set: finalized** (see "Custom func signatures"). The open
  question was the `bool`/`error` return overload — resolved by "single
  bool/error return is always a validator; transforms producing them use the
  two-return form." Adds the infallible converter `func(A)B`.
- **Level scoping: `inner:(…)` groups, not a `;` pop.** Superseded the original
  `;` design — parentheses bound each level explicitly and nest for depth
  (`inner:(a inner:(b))`); a bare `inner:x` is a one-step shorthand.

## Examples

```go
type User struct {
	// required key, value trimmed + length-checked (no ~ needed)
	Name string `json:"name" pipe:"required trim minlen=1 maxlen=50"`

	// optional key (absent → zero), validated only if present
	Bio string `json:"bio" pipe:"optional trim maxlen=500"`

	// required key; null→0 (bare nullzero) or native number; then range-checked
	Age int `json:"age" pipe:"required nullzero gte=0 lte=150"`

	// required key; scalar or {amount,scale} object via the Money decoder
	Price int `json:"price" pipe:"required ~ ./@FromMoney ~ gte=0"`  // @FromMoney(Money)(int,error)

	// optional; per-element trim+bound, then whole-slice cap after the loop
	Tags []string `json:"tags" pipe:"optional ~ inner:(trim maxlen=20) maxlen=100" hint:"8"`
}

// plain — no custom parsing, no special glyphs
Name string `json:"name" pipe:"trim minlen=1 maxlen=50"`

// per-level prealloc
Matrix [][]int `hint:"32 inner:8"`
```
