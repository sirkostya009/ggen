# ggen — zero-copy, zero-reflection JSON codegen for Go

Code generator. Parses annotated Go structs, emits methods on them. Hand-rolls a
byte scan over the caller's `[]byte` or `*scan.Stream`; the bytes path aliases
input via `unsafe.String` — no copy, no tokens, no AST.

This file is the **project map + repo-wide conventions**. The CLI / codegen
surface and the _why_ behind generated-code shape live in `cli/CLAUDE.md`;
runtime internals, benchmarks, and integration-test conventions live in each
package's own CLAUDE.md (see Repo layout). This is NOT the user-facing doc — that
is `README.md` / `SKILL.md`.

## Repo layout

```
schema/
├── go.work             ← workspace tying all four modules together
├── cli/                → see cli/CLAUDE.md            ← CLI module / generator (github.com/sirkostya009/ggen/cli, package main)
├── decode/             → see decode/CLAUDE.md           ┐
├── decode/validation/  → see decode/validation/CLAUDE.md │ runtime library
├── encode/             → see encode/CLAUDE.md           │ (root module
├── scan/               → see scan/CLAUDE.md             ┘  github.com/sirkostya009/ggen)
├── integrationtests/   → see integrationtests/CLAUDE.md  (own Go module)
├── bench/              → see bench/CLAUDE.md             (own Go module)
└── .claude/backlog.md  ← ideas worth pursuing, tried-and-rejected, maybe-someday
```

Four modules under one `go.work`: root (`github.com/sirkostya009/ggen` — runtime
library `decode`/`encode`/`scan` only, no external deps), `cli/` (the generator,
depends on `golang.org/x/tools`), `bench/`, `integrationtests/`. The CLI doesn't
import the runtime packages — it emits their import paths as string literals into
generated code.

## Conventions

### How to regenerate

Build the binary into the project dir (`./ggen`), never `/tmp` — it stays
discoverable, avoids cross-session collisions, and matches the test harness path.

```sh
go build -o ggen ./cli
./ggen ./decode/... ./encode/... ./scan/...
easyjson bench/types.go
GOEXPERIMENT=jsonv2 go generate work
```

The binary builds from the `cli/` module to project-root `./ggen` (so the
`../ggen` references in `bench/` and `integrationtests/` resolve). ggen is
module-scoped — `./...` visits only the invoked module's packages; `cli/`,
`bench/`, `integrationtests/` each carry their own `go.mod` and must be regen'd
from inside (one invocation per module). In `integrationtests/`, each annotated
source carries `//go:generate ../ggen $GOFILE` and emits a sibling
`<file>_ggen_test.go`.

### Keeping docs in sync

**The three surface docs move together.** Every change touching user-visible
surface (CLI/annotation flags, codegen behaviour, wire format, generated method
surface, field tag syntax, new Go kind/wire-shape, new runtime API, etc) must
propagate to all three in the same commit:

- `cli/CLAUDE.md` — implementation-detail doc (the _why_ behind CLI/codegen)
- `README.md` — user-facing surface (_what_/_how_)
- `SKILL.md` — user-facing surface (_what_/_how_)

Everything else routes to exactly one doc:

- benchmark numbers → `bench/CLAUDE.md`
- test-suite layout → `integrationtests/CLAUDE.md`
- per-package runtime details → the matching package CLAUDE.md
- backlog / tried-and-rejected → `.claude/backlog.md`

This root file = project map + repo-wide conventions only.

### README authoring rules

README is the user-facing front door: what ggen is,
what it does, how to use it, what numbers mean. NEVER spill implementation detail
into it (runtime/harness mechanism, `unsafe.String` aliasing, slab heuristics,
`KeyView` vs `String`, `preallocCap`/`peelSliceField` shape, pprof internals). DO
put in README: what each benchmark measures (one sentence), how to read each
metric, when a user would care, the bench table + interpretive paragraph, and
caveats affecting the user's choice (e.g. "strings alias the input, don't mutate
after decode"). If you write "internally", "implementation", "under the hood", or
name a private function / runtime API in README — stop; it belongs in
`cli/CLAUDE.md` or a code comment.

## Backlog

See @.claude/backlog.md
