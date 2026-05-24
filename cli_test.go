package main

// CLI integration tests. The whole CLI surface is exercised under one
// parent test (TestCLI) so the binary build + temp-dir holding it can ride
// on the parent's t.TempDir() — auto-cleaned when TestCLI returns. Each
// `t.Run` subtest gets its own t.TempDir() for fixtures.
//
// Fixtures land in temp dirs outside any Go module — packages.Load returns
// empty, the generator falls into its AST-only path, and output paths are
// derived from filename / dir basename only. That's exactly the surface
// these tests pin down: no module, no type info, just CLI dispatch +
// post-codegen file write.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildCLI compiles ggen into the parent test's TempDir so cleanup rides
// on the standard t.TempDir() lifecycle. Returns the binary path. Called
// once at the start of TestCLI; subtests reuse via closure.
func buildCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "ggen")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build ggen: %v", err)
	}
	return bin
}

// runCLI invokes the built ggen binary inside dir with the given args and
// returns combined stdout+stderr. The caller decides whether the err is
// expected (usage / -o-with-both-groups / etc.).
func runCLI(t *testing.T, bin, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOEXPERIMENT=jsonv2")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()
	return buf.String(), runErr
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustReadOutput(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func mustHaveFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file %s: %v", path, err)
	}
}

func mustNotHaveFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("unexpected file present: %s", path)
	}
}

const minimalStruct = `package fixture

//ggen:generate
type Msg struct {
	Text string ` + "`" + `json:"text"` + "`" + `
}
`

func TestCLI(t *testing.T) {
	t.Parallel()
	bin := buildCLI(t)

	t.Run("SingleFile_NonTest_OutputName", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "msg.go"), minimalStruct)
		if out, err := runCLI(t, bin, dir, "msg.go"); err != nil {
			t.Fatalf("ggen msg.go: %v\n%s", err, out)
		}
		mustHaveFile(t, filepath.Join(dir, "msg_ggen.go"))
		mustNotHaveFile(t, filepath.Join(dir, "msg_ggen_test.go"))
	})

	t.Run("SingleFile_Test_OutputName", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "msg_test.go"), minimalStruct)
		if out, err := runCLI(t, bin, dir, "msg_test.go"); err != nil {
			t.Fatalf("ggen msg_test.go: %v\n%s", err, out)
		}
		mustHaveFile(t, filepath.Join(dir, "msg_ggen_test.go"))
		mustNotHaveFile(t, filepath.Join(dir, "msg_ggen.go"))
	})

	t.Run("SingleFile_OutputOverride", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "msg.go"), minimalStruct)
		target := filepath.Join(dir, "custom_output.go")
		if out, err := runCLI(t, bin, dir, "-o", target, "msg.go"); err != nil {
			t.Fatalf("ggen -o: %v\n%s", err, out)
		}
		mustHaveFile(t, target)
		mustNotHaveFile(t, filepath.Join(dir, "msg_ggen.go"))
	})

	t.Run("InterspersedFlags_FlagAfterPositional", func(t *testing.T) {
		t.Parallel()
		// Stdlib `flag.Parse` stops at the first non-flag arg, so the
		// naive `ggen in.go -o out.go` form treats `-o out.go` as
		// struct-name filters. The interspersing loop in main() re-parses
		// around each positional so both orders work — match `go test`'s
		// behaviour.
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "msg.go"), minimalStruct)
		target := filepath.Join(dir, "after.go")
		if out, err := runCLI(t, bin, dir, "msg.go", "-o", target); err != nil {
			t.Fatalf("ggen msg.go -o ...: %v\n%s", err, out)
		}
		mustHaveFile(t, target)
		mustNotHaveFile(t, filepath.Join(dir, "msg_ggen.go"))
	})

	t.Run("InterspersedFlags_FlagBetweenPositionals", func(t *testing.T) {
		t.Parallel()
		// Two positionals (file + name filter) with a flag wedged between
		// them. Both positionals must reach generateSingleFile in order;
		// the flag must still take effect.
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "msg.go"), `package fixture

//ggen:generate
type Wanted struct {
	A string `+"`"+`json:"a"`+"`"+`
}

//ggen:generate
type Skipped struct {
	B string `+"`"+`json:"b"`+"`"+`
}
`)
		target := filepath.Join(dir, "between.go")
		if out, err := runCLI(t, bin, dir, "msg.go", "-o", target, "Wanted"); err != nil {
			t.Fatalf("ggen msg.go -o ... Wanted: %v\n%s", err, out)
		}
		mustHaveFile(t, target)
		body := mustReadOutput(t, target)
		if !strings.Contains(body, "Wanted) DecodeFrom") {
			t.Errorf("expected Wanted, got:\n%s", body)
		}
		if strings.Contains(body, "Skipped) DecodeFrom") {
			t.Errorf("Skipped leaked despite name filter:\n%s", body)
		}
	})

	t.Run("Verbosity_QuietSuppressesInfo", func(t *testing.T) {
		t.Parallel()
		// Default level is LevelQuiet — `wrote <file>` info lines must
		// be suppressed. Only errors should reach stderr at this level.
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "msg.go"), minimalStruct)
		out, err := runCLI(t, bin, dir, "msg.go")
		if err != nil {
			t.Fatalf("ggen msg.go: %v\n%s", err, out)
		}
		if strings.Contains(out, "wrote") {
			t.Errorf("default level (-v not set) must suppress 'wrote' info lines, got:\n%s", out)
		}
		mustHaveFile(t, filepath.Join(dir, "msg_ggen.go"))
	})

	t.Run("Verbosity_VShowsInfo", func(t *testing.T) {
		t.Parallel()
		// -v lifts the floor to LevelInfo so `wrote <file>` lines appear.
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "msg.go"), minimalStruct)
		out, err := runCLI(t, bin, dir, "-v", "msg.go")
		if err != nil {
			t.Fatalf("ggen -v msg.go: %v\n%s", err, out)
		}
		if !strings.Contains(out, "wrote") {
			t.Errorf("-v must surface 'wrote' info lines, got:\n%s", out)
		}
	})

	t.Run("Verbosity_VVShowsDebug", func(t *testing.T) {
		t.Parallel()
		// -vv lifts to LevelDebug. The dir-mode `parsing <pkg>` debug
		// line is the most stable marker.
		base := t.TempDir()
		writeFixture(t, filepath.Join(base, "msg.go"),
			strings.ReplaceAll(minimalStruct, "fixture", "vv"))
		out, err := runCLI(t, bin, base, "-vv", ".")
		if err != nil {
			t.Fatalf("ggen -vv .: %v\n%s", err, out)
		}
		if !strings.Contains(out, "dbg:") {
			t.Errorf("-vv must surface dbg: lines, got:\n%s", out)
		}
		if !strings.Contains(out, "wrote") {
			t.Errorf("-vv must also surface info (debug ≥ info), got:\n%s", out)
		}
	})

	t.Run("Verbosity_VVVShowsTrace", func(t *testing.T) {
		t.Parallel()
		// -vvv lifts to LevelTrace. Use a dir with no annotations so
		// the `no annotated structs in X; skipping` trace line fires.
		base := t.TempDir()
		sub := filepath.Join(base, "empty")
		writeFixture(t, filepath.Join(sub, "msg.go"), `package empty

type Bare struct {}
`)
		out, err := runCLI(t, bin, base, "-vvv", "./...")
		if err != nil {
			t.Fatalf("ggen -vvv ./...: %v\n%s", err, out)
		}
		if !strings.Contains(out, "trc:") {
			t.Errorf("-vvv must surface trc: lines, got:\n%s", out)
		}
	})

	t.Run("Verbosity_PositionedFlagAfterTarget", func(t *testing.T) {
		t.Parallel()
		// The interspersing loop must also handle verbosity flags placed
		// after a positional, since users will type `ggen msg.go -v`.
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "msg.go"), minimalStruct)
		out, err := runCLI(t, bin, dir, "msg.go", "-v")
		if err != nil {
			t.Fatalf("ggen msg.go -v: %v\n%s", err, out)
		}
		if !strings.Contains(out, "wrote") {
			t.Errorf("-v after positional must still take effect, got:\n%s", out)
		}
	})

	t.Run("InterspersedFlags_BoolFlagAfterPositional", func(t *testing.T) {
		t.Parallel()
		// Bool flags follow the same path. Pin -marshal AFTER the file
		// arg to make sure the interspersing loop also picks up valueless
		// flags (not just `-o <value>`).
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "msg.go"), minimalStruct)
		if out, err := runCLI(t, bin, dir, "msg.go", "-marshal"); err != nil {
			t.Fatalf("ggen msg.go -marshal: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(dir, "msg_ggen.go"))
		if !strings.Contains(body, "MarshalJSON()") {
			t.Errorf("-marshal didn't take effect when placed after positional:\n%s", body)
		}
	})

	t.Run("SingleFile_NoAnnotation_ErrorsWithHint", func(t *testing.T) {
		t.Parallel()
		// File without `//ggen:generate` and no positional name filter
		// must error out (not fall back to "all exported structs"). The
		// diagnostic must mention how to fix it.
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "msg.go"), `package fixture

type Msg struct {
	Text string `+"`"+`json:"text"`+"`"+`
}
`)
		out, err := runCLI(t, bin, dir, "msg.go")
		if err == nil {
			t.Fatalf("expected non-zero exit when file lacks annotation, got:\n%s", out)
		}
		for _, want := range []string{
			"no //ggen:generate-annotated struct",
			"msg.go",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("diagnostic missing %q, got:\n%s", want, out)
			}
		}
		mustNotHaveFile(t, filepath.Join(dir, "msg_ggen.go"))
	})

	t.Run("SingleFile_ExplicitNameOverridesMissingAnnotation", func(t *testing.T) {
		t.Parallel()
		// When the user names a struct explicitly, ggen processes it even
		// though there's no `//ggen:generate` directive. Positional names
		// are the escape hatch for the no-annotation case.
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "msg.go"), `package fixture

type Msg struct {
	Text string `+"`"+`json:"text"`+"`"+`
}
`)
		if out, err := runCLI(t, bin, dir, "msg.go", "Msg"); err != nil {
			t.Fatalf("ggen msg.go Msg: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(dir, "msg_ggen.go"))
		if !strings.Contains(body, "Msg) DecodeFrom") {
			t.Errorf("expected Msg.DecodeFrom, got:\n%s", body)
		}
	})

	t.Run("SingleFile_OnlyEmitsRequestedFileStructs", func(t *testing.T) {
		t.Parallel()
		// Single-file mode must restrict output to types declared in the
		// passed file, even when the directory IS a real Go module and
		// packages.Load returns the whole package's syntax tree. Without
		// this filter, `ggen one.go` inside a populated module dumps every
		// annotated type across all sibling files into one_ggen.go.
		base := t.TempDir()
		writeFixture(t, filepath.Join(base, "go.mod"), `module sfscope

go 1.26
`)
		writeFixture(t, filepath.Join(base, "one.go"), `package sfscope

//ggen:generate
type SoloA struct {
	A string `+"`"+`json:"a"`+"`"+`
}
`)
		writeFixture(t, filepath.Join(base, "two.go"), `package sfscope

//ggen:generate
type SoloB struct {
	B string `+"`"+`json:"b"`+"`"+`
}
`)
		if out, err := runCLI(t, bin, base, "one.go"); err != nil {
			t.Fatalf("ggen one.go: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(base, "one_ggen.go"))
		if !strings.Contains(body, "SoloA) DecodeFrom") {
			t.Errorf("expected SoloA in one_ggen.go, got:\n%s", body)
		}
		if strings.Contains(body, "SoloB) DecodeFrom") {
			t.Errorf("SoloB leaked into one_ggen.go from sibling two.go:\n%s", body)
		}
	})

	t.Run("Directory_NonTest_OutputName", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()
		dir := filepath.Join(base, "fixture")
		writeFixture(t, filepath.Join(dir, "msg.go"), minimalStruct)
		if out, err := runCLI(t, bin, dir, "."); err != nil {
			t.Fatalf("ggen .: %v\n%s", err, out)
		}
		mustHaveFile(t, filepath.Join(dir, "fixture_ggen.go"))
		mustNotHaveFile(t, filepath.Join(dir, "fixture_ggen_test.go"))
	})

	t.Run("Directory_Test_OutputName", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()
		dir := filepath.Join(base, "fixture")
		writeFixture(t, filepath.Join(dir, "msg_test.go"), minimalStruct)
		if out, err := runCLI(t, bin, dir, "."); err != nil {
			t.Fatalf("ggen .: %v\n%s", err, out)
		}
		mustHaveFile(t, filepath.Join(dir, "fixture_ggen_test.go"))
		mustNotHaveFile(t, filepath.Join(dir, "fixture_ggen.go"))
	})

	t.Run("Directory_Mixed_BothFiles", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()
		dir := filepath.Join(base, "fixture")
		writeFixture(t, filepath.Join(dir, "msg.go"), minimalStruct)
		// Different struct name avoids a duplicate declaration in the package.
		writeFixture(t, filepath.Join(dir, "msg_test.go"),
			strings.ReplaceAll(minimalStruct, "Msg", "MsgT"))
		if out, err := runCLI(t, bin, dir, "."); err != nil {
			t.Fatalf("ggen .: %v\n%s", err, out)
		}
		mustHaveFile(t, filepath.Join(dir, "fixture_ggen.go"))
		mustHaveFile(t, filepath.Join(dir, "fixture_ggen_test.go"))

		// Content routing: Msg (declared in msg.go) belongs to the library
		// build; MsgT (declared in msg_test.go) belongs to the test build.
		// They must land in their respective _gen files and not bleed across
		// — otherwise a `go test` build would pull duplicate methods or
		// `go build` would import test-only types.
		nonTest := mustReadOutput(t, filepath.Join(dir, "fixture_ggen.go"))
		test := mustReadOutput(t, filepath.Join(dir, "fixture_ggen_test.go"))
		if !strings.Contains(nonTest, "Msg) DecodeFrom") {
			t.Errorf("Msg missing from fixture_ggen.go:\n%s", nonTest)
		}
		if strings.Contains(nonTest, "MsgT) DecodeFrom") {
			t.Errorf("MsgT leaked into fixture_ggen.go:\n%s", nonTest)
		}
		if !strings.Contains(test, "MsgT) DecodeFrom") {
			t.Errorf("MsgT missing from fixture_ggen_test.go:\n%s", test)
		}
		if strings.Contains(test, "Msg) DecodeFrom") {
			t.Errorf("Msg leaked into fixture_ggen_test.go:\n%s", test)
		}
	})

	t.Run("Directory_NoAnnotations_NoOutput", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()
		dir := filepath.Join(base, "fixture")
		writeFixture(t, filepath.Join(dir, "msg.go"), `package fixture

type Msg struct{ Text string }
`)
		if out, err := runCLI(t, bin, dir, "."); err != nil {
			t.Fatalf("ggen .: %v\n%s", err, out)
		}
		mustNotHaveFile(t, filepath.Join(dir, "fixture_ggen.go"))
		mustNotHaveFile(t, filepath.Join(dir, "fixture_ggen_test.go"))
	})

	t.Run("Directory_BuildTag_BucketsIntoSeparateFiles", func(t *testing.T) {
		t.Parallel()
		// Two files in the same package: one untagged, one behind
		// `//go:build foo`. The tagged struct must NOT pollute the
		// untagged gen file (would compile-break builds without `foo`),
		// so the generator emits a separate `<dir>_foo_ggen.go` carrying
		// the matching //go:build header.
		base := t.TempDir()
		dir := filepath.Join(base, "fixture")
		writeFixture(t, filepath.Join(dir, "plain.go"), `package fixture

//ggen:generate
type Plain struct {
	A string `+"`"+`json:"a"`+"`"+`
}
`)
		writeFixture(t, filepath.Join(dir, "tagged.go"), `//go:build foo

package fixture

//ggen:generate
type Tagged struct {
	B string `+"`"+`json:"b"`+"`"+`
}
`)
		if out, err := runCLI(t, bin, dir, "."); err != nil {
			t.Fatalf("ggen .: %v\n%s", err, out)
		}

		// Untagged bucket → fixture_ggen.go, Plain inside, no //go:build header.
		plain := filepath.Join(dir, "fixture_ggen.go")
		mustHaveFile(t, plain)
		plainBody := mustReadOutput(t, plain)
		if strings.HasPrefix(plainBody, "//go:build") {
			t.Errorf("untagged bucket leaked a //go:build header:\n%s", plainBody)
		}
		if !strings.Contains(plainBody, "Plain) DecodeFrom") {
			t.Errorf("Plain missing from untagged file:\n%s", plainBody)
		}
		if strings.Contains(plainBody, "Tagged) DecodeFrom") {
			t.Errorf("Tagged leaked into untagged file (would compile-break without `foo`):\n%s", plainBody)
		}

		// Tagged bucket → fixture_foo_ggen.go, Tagged inside, with the header.
		tagged := filepath.Join(dir, "fixture_foo_ggen.go")
		mustHaveFile(t, tagged)
		taggedBody := mustReadOutput(t, tagged)
		if !strings.Contains(taggedBody, "//go:build foo\n") {
			t.Errorf("tagged file missing //go:build foo header:\n%s", taggedBody)
		}
		if !strings.Contains(taggedBody, "Tagged) DecodeFrom") {
			t.Errorf("Tagged missing from tagged file:\n%s", taggedBody)
		}
		if strings.Contains(taggedBody, "Plain) DecodeFrom") {
			t.Errorf("Plain leaked into tagged file:\n%s", taggedBody)
		}
	})

	t.Run("Directory_BuildTag_MultiTermExpression", func(t *testing.T) {
		t.Parallel()
		// `//go:build foo && bar` — multi-term constraint must canonicalize
		// into a slug like `foo_bar` for the filename, with the original
		// expression preserved verbatim in the //go:build header.
		base := t.TempDir()
		dir := filepath.Join(base, "fixture")
		writeFixture(t, filepath.Join(dir, "tagged.go"), `//go:build foo && bar

package fixture

//ggen:generate
type Tagged struct {
	B string `+"`"+`json:"b"`+"`"+`
}
`)
		if out, err := runCLI(t, bin, dir, "."); err != nil {
			t.Fatalf("ggen .: %v\n%s", err, out)
		}
		// Slug collapses ` && ` into a single underscore.
		path := filepath.Join(dir, "fixture_foo_bar_ggen.go")
		mustHaveFile(t, path)
		body := mustReadOutput(t, path)
		if !strings.Contains(body, "//go:build foo && bar\n") {
			t.Errorf("expected '//go:build foo && bar' header, got:\n%s", body)
		}
	})

	t.Run("Directory_OutputOverrideRejectsBothGroups", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()
		dir := filepath.Join(base, "fixture")
		writeFixture(t, filepath.Join(dir, "msg.go"), minimalStruct)
		writeFixture(t, filepath.Join(dir, "msg_test.go"),
			strings.ReplaceAll(minimalStruct, "Msg", "MsgT"))
		target := filepath.Join(dir, "custom.go")
		out, err := runCLI(t, bin, dir, "-o", target, ".")
		if err == nil {
			t.Fatalf("expected non-zero exit using -o with both groups\n%s", out)
		}
		if !strings.Contains(out, "-o cannot be used") {
			t.Fatalf("expected '-o cannot be used' diagnostic, got: %s", out)
		}
	})

	// --- walk mode ---

	t.Run("WalkSubpackages", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()
		a := filepath.Join(base, "a")
		b := filepath.Join(base, "b")
		writeFixture(t, filepath.Join(a, "msg.go"),
			strings.ReplaceAll(minimalStruct, "fixture", "a"))
		writeFixture(t, filepath.Join(b, "msg.go"),
			strings.ReplaceAll(strings.ReplaceAll(minimalStruct, "fixture", "b"), "Msg", "MsgB"))
		if out, err := runCLI(t, bin, base, "./..."); err != nil {
			t.Fatalf("ggen ./...: %v\n%s", err, out)
		}
		mustHaveFile(t, filepath.Join(a, "a_ggen.go"))
		mustHaveFile(t, filepath.Join(b, "b_ggen.go"))
	})

	t.Run("WalkSubModuleResolvesUnderItsOwnGoMod", func(t *testing.T) {
		t.Parallel()
		// `./...` from the root module must descend into a subdirectory
		// that has its OWN go.mod and successfully load it under that
		// sub-module's context — packages.Load must be invoked with
		// cfg.Dir = sub_dir so type info resolves. The custom-validator
		// `@NotEmpty` reference forces the type-info path: without it,
		// the AST-only fallback can't resolve @-funcs and the build
		// fails with "@Func references require Go module context".
		//
		// Regressing this (e.g. dropping cfg.Dir) leaves the sub-module
		// silently un-generated even though `ggen ./...` exits 0 — the
		// kind of bug a literal "exists in path" check misses; we
		// explicitly assert (a) basename in filename and (b) the
		// @NotEmpty call lands in the body.
		base := t.TempDir()
		writeFixture(t, filepath.Join(base, "go.mod"), "module rootmod\n\ngo 1.26\n")
		writeFixture(t, filepath.Join(base, "root.go"),
			strings.ReplaceAll(minimalStruct, "fixture", "rootmod"))
		sub := filepath.Join(base, "sub")
		writeFixture(t, filepath.Join(sub, "go.mod"), "module submod\n\ngo 1.26\n")
		writeFixture(t, filepath.Join(sub, "msg.go"), `package sub

//ggen:generate
type Item struct {
	Name string `+"`"+`json:"name" ggen:"@NotEmpty"`+"`"+`
}

func NotEmpty(s string) error {
	if s == "" {
		return fmt.Errorf("empty")
	}
	return nil
}
`)
		// fmt import — keep it simple, the validator references it.
		writeFixture(t, filepath.Join(sub, "helper.go"), `package sub

import "fmt"

var _ = fmt.Sprint
`)
		out, err := runCLI(t, bin, base, "./...")
		if err != nil {
			t.Fatalf("ggen ./...: %v\n%s", err, out)
		}
		// Sub-module's generated file must (a) exist with the right
		// basename and (b) contain the @-func call site — proving
		// type info was available during codegen.
		genPath := filepath.Join(sub, "sub_ggen.go")
		mustHaveFile(t, genPath)
		got := mustReadOutput(t, genPath)
		if !strings.Contains(got, "NotEmpty(") {
			t.Errorf("sub_ggen.go missing NotEmpty call — @-func didn't resolve:\n%s", got)
		}
		// Root module also generated.
		mustHaveFile(t, filepath.Join(base, filepath.Base(base)+"_ggen.go"))
	})

	t.Run("Dot_DoesNotRecurseIntoSubpackages", func(t *testing.T) {
		t.Parallel()
		// `.` is single-package mode — only the directly named dir is
		// processed. Subdirectories MUST NOT be touched, even when they
		// contain annotated structs. This is the canonical
		// `.` vs `./...` divergence and the easiest place for the dispatch
		// in walkTarget to regress silently.
		base := t.TempDir()
		writeFixture(t, filepath.Join(base, "top.go"),
			strings.ReplaceAll(minimalStruct, "fixture", "root"))
		sub := filepath.Join(base, "sub")
		writeFixture(t, filepath.Join(sub, "msg.go"),
			strings.ReplaceAll(strings.ReplaceAll(minimalStruct, "fixture", "sub"), "Msg", "SubMsg"))
		if out, err := runCLI(t, bin, base, "."); err != nil {
			t.Fatalf("ggen .: %v\n%s", err, out)
		}
		// Top-level processed; subdir untouched.
		mustHaveFile(t, filepath.Join(base, filepath.Base(base)+"_ggen.go"))
		mustNotHaveFile(t, filepath.Join(sub, "sub_ggen.go"))
	})

	t.Run("Walk_PathSlashEllipsis", func(t *testing.T) {
		t.Parallel()
		// `path/...` (relative path without leading `./`) is a third
		// accepted walk form — walkTarget strips `/...` and walks from
		// the prefix. Confirms the suffix-strip branch, distinct from
		// the literal `./...` / `...` switch above.
		base := t.TempDir()
		root := filepath.Join(base, "pkg")
		leaf := filepath.Join(root, "leaf")
		writeFixture(t, filepath.Join(root, "top.go"),
			strings.ReplaceAll(minimalStruct, "fixture", "pkg"))
		writeFixture(t, filepath.Join(leaf, "msg.go"),
			strings.ReplaceAll(strings.ReplaceAll(minimalStruct, "fixture", "leaf"), "Msg", "Leaf"))
		// Sibling dir that must NOT get processed — proves the prefix
		// scoping isn't ignored.
		sibling := filepath.Join(base, "other")
		writeFixture(t, filepath.Join(sibling, "msg.go"),
			strings.ReplaceAll(strings.ReplaceAll(minimalStruct, "fixture", "other"), "Msg", "Other"))
		if out, err := runCLI(t, bin, base, "pkg/..."); err != nil {
			t.Fatalf("ggen pkg/...: %v\n%s", err, out)
		}
		mustHaveFile(t, filepath.Join(root, "pkg_ggen.go"))
		mustHaveFile(t, filepath.Join(leaf, "leaf_ggen.go"))
		mustNotHaveFile(t, filepath.Join(sibling, "other_ggen.go"))
	})

	t.Run("SingleFile_GathersErrorsAcrossStructs", func(t *testing.T) {
		t.Parallel()
		// One file, two structs, both with broken applicability rules
		// (ascii on int / email on int). One invocation must surface
		// BOTH diagnostics, not just the first — parse-time errors are
		// accumulated via errors.Join and the logger unwraps the batch.
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "multi.go"), `package fixture

//ggen:generate
type Test1 struct {
	A int `+"`"+`json:"a" ggen:"ascii"`+"`"+`
}

//ggen:generate
type Test2 struct {
	B int `+"`"+`json:"b" ggen:"email"`+"`"+`
}
`)
		out, err := runCLI(t, bin, dir, "multi.go", "Test1", "Test2")
		if err == nil {
			t.Fatalf("expected non-zero exit, got:\n%s", out)
		}
		if !strings.Contains(out, "`ascii`") {
			t.Errorf("ascii diagnostic missing, got:\n%s", out)
		}
		if !strings.Contains(out, "`email`") {
			t.Errorf("email diagnostic missing, got:\n%s", out)
		}
	})

	t.Run("SingleFile_GathersErrorsAcrossFields", func(t *testing.T) {
		t.Parallel()
		// One struct with multiple bad fields. Each field's
		// applicability error must surface — extractStruct accumulates
		// per-field errors via errors.Join.
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "multi.go"), `package fixture

//ggen:generate
type Multi struct {
	A int `+"`"+`json:"a" ggen:"ascii"`+"`"+`
	B int `+"`"+`json:"b" ggen:"email"`+"`"+`
	C string `+"`"+`json:"c" ggen:"gt=0"`+"`"+`
}
`)
		out, err := runCLI(t, bin, dir, "multi.go")
		if err == nil {
			t.Fatalf("expected non-zero exit, got:\n%s", out)
		}
		for _, want := range []string{"`ascii`", "`email`", "`gt`"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing diagnostic %s, got:\n%s", want, out)
			}
		}
	})

	t.Run("SingleFile_GathersValAndModErrorsOnSameField", func(t *testing.T) {
		t.Parallel()
		// One field with BOTH a bad val rule AND a bad mod must surface
		// both diagnostics — checkRuleApplicability accumulates errors
		// across the val-phase + mod-phase + keys/dive/hintlen phases
		// rather than short-circuiting on the first failure.
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "multi.go"), `package fixture

//ggen:generate
type Bad struct {
	A int `+"`"+`json:"a" ggen:"ascii" mod:"trim"`+"`"+`
}
`)
		out, err := runCLI(t, bin, dir, "multi.go")
		if err == nil {
			t.Fatalf("expected non-zero exit, got:\n%s", out)
		}
		// ascii (val) and trim (mod) must BOTH appear, and each must
		// carry its own position prefix (attachPosition walks the tree
		// instead of stamping the first richError only).
		for _, want := range []string{"`ascii`", "`trim`"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing diagnostic for %s, got:\n%s", want, out)
			}
		}
		// Two distinct position prefixes on the SAME line+column —
		// proves both sub-errors got positions, not just the first.
		if strings.Count(out, "multi.go:") < 2 {
			t.Errorf("expected ≥2 position prefixes (val + mod), got:\n%s", out)
		}
	})

	t.Run("Walk_GathersErrorsAcrossPackages", func(t *testing.T) {
		t.Parallel()
		// Walk mode must NOT bail on the first failing package — every
		// package's errors get queued and flushed at exit, so users see
		// the full problem list in one run. Three subdirs, two with bad
		// applicability rules, one clean. After the run: both errors
		// must surface, the clean dir's `wrote` line must still appear,
		// and the exit code must be non-zero.
		base := t.TempDir()
		writeFixture(t, filepath.Join(base, "a", "msg.go"), `package a

//ggen:generate
type A struct {
	N int `+"`"+`json:"n" ggen:"ascii"`+"`"+`
}
`)
		writeFixture(t, filepath.Join(base, "b", "msg.go"), `package b

//ggen:generate
type B struct {
	N int `+"`"+`json:"n" ggen:"email"`+"`"+`
}
`)
		writeFixture(t, filepath.Join(base, "c", "msg.go"), `package c

//ggen:generate
type C struct {
	S string `+"`"+`json:"s"`+"`"+`
}
`)
		out, err := runCLI(t, bin, base, "-v", "./...")
		if err == nil {
			t.Fatalf("walk with broken packages must exit non-zero, got:\n%s", out)
		}
		// Both errored packages must appear in the final batch.
		if !strings.Contains(out, "`ascii`") {
			t.Errorf("package a's ascii error missing, got:\n%s", out)
		}
		if !strings.Contains(out, "`email`") {
			t.Errorf("package b's email error missing, got:\n%s", out)
		}
		// The clean package must still get its wrote line — proves the
		// walk continued past the broken ones.
		if !strings.Contains(out, "wrote") || !strings.Contains(out, "c_ggen.go") {
			t.Errorf("clean package c should still emit `wrote ./c/c_ggen.go`, got:\n%s", out)
		}
	})

	t.Run("Walk_RejectsOutputOverride", func(t *testing.T) {
		t.Parallel()
		// `-o` writes one file; walk mode writes one per package. The
		// combination has no sensible meaning, so reject it up front
		// instead of silently dropping the flag.
		base := t.TempDir()
		a := filepath.Join(base, "a")
		writeFixture(t, filepath.Join(a, "msg.go"),
			strings.ReplaceAll(minimalStruct, "fixture", "a"))
		out, err := runCLI(t, bin, base, "-o", filepath.Join(base, "out.go"), "./...")
		if err == nil {
			t.Fatalf("expected non-zero exit with -o + ./..., got:\n%s", out)
		}
		if !strings.Contains(out, "-o cannot be used with ./...") {
			t.Fatalf("expected '-o cannot be used with ./...' diagnostic, got: %s", out)
		}
	})

	t.Run("WalkSkipsDotAndUnderscoreDirs", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()
		hidden := filepath.Join(base, ".hidden")
		skipped := filepath.Join(base, "_skipped")
		visible := filepath.Join(base, "visible")
		writeFixture(t, filepath.Join(hidden, "msg.go"), minimalStruct)
		writeFixture(t, filepath.Join(skipped, "msg.go"), minimalStruct)
		writeFixture(t, filepath.Join(visible, "msg.go"),
			strings.ReplaceAll(minimalStruct, "fixture", "visible"))
		if out, err := runCLI(t, bin, base, "./..."); err != nil {
			t.Fatalf("ggen ./...: %v\n%s", err, out)
		}
		mustHaveFile(t, filepath.Join(visible, "visible_ggen.go"))
		mustNotHaveFile(t, filepath.Join(hidden, ".hidden_ggen.go"))
		mustNotHaveFile(t, filepath.Join(skipped, "_skipped_ggen.go"))
	})

	// --- flag plumbing into generated output ---

	t.Run("PkgOverride", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "msg.go"), minimalStruct)
		if out, err := runCLI(t, bin, dir, "-pkg", "renamed", "msg.go"); err != nil {
			t.Fatalf("ggen -pkg: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(dir, "msg_ggen.go"))
		if !strings.Contains(body, "package renamed") {
			t.Fatalf("expected 'package renamed' in output, got:\n%s", body)
		}
	})

	t.Run("MarshalFlag_EmitsHook", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "msg.go"), minimalStruct)
		if out, err := runCLI(t, bin, dir, "-marshal", "msg.go"); err != nil {
			t.Fatalf("ggen -marshal: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(dir, "msg_ggen.go"))
		if !strings.Contains(body, "MarshalJSON()") {
			t.Fatalf("expected MarshalJSON method in output, got:\n%s", body)
		}
	})

	t.Run("UnmarshalFlag_EmitsHook", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "msg.go"), minimalStruct)
		if out, err := runCLI(t, bin, dir, "-unmarshal", "msg.go"); err != nil {
			t.Fatalf("ggen -unmarshal: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(dir, "msg_ggen.go"))
		if !strings.Contains(body, "UnmarshalJSON(") {
			t.Fatalf("expected UnmarshalJSON method in output, got:\n%s", body)
		}
	})

	t.Run("NovalidateFlag_SkipsRules", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		src := filepath.Join(dir, "msg.go")
		writeFixture(t, src, `package fixture

//ggen:generate
type Validated struct {
	Name string `+"`"+`json:"name" ggen:"required,minlen=1"`+"`"+`
}
`)
		// Default: required + MinLen rules emit typed errors at validation sites.
		if out, err := runCLI(t, bin, dir, "msg.go"); err != nil {
			t.Fatalf("ggen default: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(dir, "msg_ggen.go"))
		if !strings.Contains(body, "MinLenError") || !strings.Contains(body, "RequiredError") {
			t.Fatalf("expected MinLenError + RequiredError in default output, got:\n%s", body)
		}
		// -novalidate: rules elided; no validation.* error symbols anywhere.
		if out, err := runCLI(t, bin, dir, "-novalidate", "msg.go"); err != nil {
			t.Fatalf("ggen -novalidate: %v\n%s", err, out)
		}
		body = mustReadOutput(t, filepath.Join(dir, "msg_ggen.go"))
		if strings.Contains(body, "MinLenError") || strings.Contains(body, "RequiredError") {
			t.Fatalf("expected no validation errors with -novalidate, got:\n%s", body)
		}
	})

	t.Run("HtmlescapeFlag_OptsIntoEscapeAppender", func(t *testing.T) {
		t.Parallel()
		// Default mode is jsonv2-shaped (literal `<`, `>`, `&`) → generated
		// code uses AppendStringNoHTML. `-htmlescape` opts back into v1 wire
		// shape and switches the call to the escaping AppendString helper.
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "msg.go"), minimalStruct)

		// Default → no-html appender.
		if out, err := runCLI(t, bin, dir, "msg.go"); err != nil {
			t.Fatalf("ggen default: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(dir, "msg_ggen.go"))
		if !strings.Contains(body, "AppendStringNoHTML") {
			t.Fatalf("expected AppendStringNoHTML by default, got:\n%s", body)
		}

		// -htmlescape → escaping appender (and not the no-html one).
		if out, err := runCLI(t, bin, dir, "-htmlescape", "msg.go"); err != nil {
			t.Fatalf("ggen -htmlescape: %v\n%s", err, out)
		}
		body = mustReadOutput(t, filepath.Join(dir, "msg_ggen.go"))
		if strings.Contains(body, "AppendStringNoHTML") {
			t.Fatalf("expected AppendString (not -NoHTML) with -htmlescape, got:\n%s", body)
		}
		if !strings.Contains(body, "encode.AppendString(") {
			t.Fatalf("expected encode.AppendString call, got:\n%s", body)
		}
	})

	t.Run("IgnoreUnknownFlag_DropsUnknownKeyError", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "msg.go"), minimalStruct)
		// Default: unknown keys produce UnknownKeyError.
		if out, err := runCLI(t, bin, dir, "msg.go"); err != nil {
			t.Fatalf("ggen default: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(dir, "msg_ggen.go"))
		if !strings.Contains(body, "UnknownKeyError") {
			t.Fatalf("expected UnknownKeyError in default output, got:\n%s", body)
		}
		// -ignoreunknown: error site silently drops unknowns.
		if out, err := runCLI(t, bin, dir, "-ignoreunknown", "msg.go"); err != nil {
			t.Fatalf("ggen -ignoreunknown: %v\n%s", err, out)
		}
		body = mustReadOutput(t, filepath.Join(dir, "msg_ggen.go"))
		if strings.Contains(body, "UnknownKeyError") {
			t.Fatalf("expected UnknownKeyError to be elided with -ignoreunknown, got:\n%s", body)
		}
	})

	// --- single-file name filter ---

	t.Run("NameFilter_OnlyEmitsRequestedStructs", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "msg.go"), `package fixture

//ggen:generate
type Wanted struct {
	A string `+"`"+`json:"a"`+"`"+`
}

//ggen:generate
type Skipped struct {
	B string `+"`"+`json:"b"`+"`"+`
}
`)
		if out, err := runCLI(t, bin, dir, "msg.go", "Wanted"); err != nil {
			t.Fatalf("ggen msg.go Wanted: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(dir, "msg_ggen.go"))
		if !strings.Contains(body, "Wanted) DecodeFrom") {
			t.Fatalf("expected DecodeFrom for Wanted, got:\n%s", body)
		}
		if strings.Contains(body, "Skipped) DecodeFrom") {
			t.Fatalf("Skipped should not be generated, got:\n%s", body)
		}
	})

	// --- cross-package type-info-driven dispatch ---

	t.Run("CrossPkgText_StaticDispatch", func(t *testing.T) {
		t.Parallel()
		// Real go.mod unlocks packages.Load + types.Implements. The consumer
		// references a type from a sibling package implementing TextAppender +
		// TextMarshaler + TextUnmarshaler. Generator must route through the
		// fast text path (AppendText / UnmarshalText) instead of json.Marshal.
		base := t.TempDir()
		writeFixture(t, filepath.Join(base, "go.mod"), `module crosspkgtest

go 1.26
`)
		writeFixture(t, filepath.Join(base, "ext", "text.go"), `package ext

import (
	"errors"
	"strings"
)

type Tagged struct {
	Name string
	Tag  string
}

func (t Tagged) AppendText(dst []byte) ([]byte, error) {
	dst = append(dst, t.Name...)
	dst = append(dst, '#')
	dst = append(dst, t.Tag...)
	return dst, nil
}

func (t Tagged) MarshalText() ([]byte, error) {
	return []byte(t.Name + "#" + t.Tag), nil
}

func (t *Tagged) UnmarshalText(b []byte) error {
	before, after, ok := strings.Cut(string(b), "#")
	if !ok {
		return errors.New("missing #")
	}
	t.Name, t.Tag = before, after
	return nil
}
`)
		writeFixture(t, filepath.Join(base, "consumer", "msg.go"), `package consumer

import "crosspkgtest/ext"

//ggen:generate
type Msg struct {
	ID  string     `+"`"+`json:"id"`+"`"+`
	Tag ext.Tagged `+"`"+`json:"tag"`+"`"+`
}
`)
		if out, err := runCLI(t, bin, base, "./consumer"); err != nil {
			t.Fatalf("ggen ./consumer: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(base, "consumer", "consumer_ggen.go"))

		// Each generated method must dispatch through the static text path
		// for the Tag field specifically (field-bound match — proves the
		// receiver of the call is exactly result.Tag / s.Tag, not some
		// random other call site).
		// DecodeFrom bytes-path: result.Tag.UnmarshalText(...)
		// DecodeStreamFrom: same call, distinguished by surrounding _s.String.
		// AppendJSON: s.Tag.AppendText(dst).
		if got := strings.Count(body, "result.Tag.UnmarshalText("); got != 2 {
			t.Errorf("expected 2 result.Tag.UnmarshalText calls (DecodeFrom + DecodeStreamFrom), got %d:\n%s", got, body)
		}
		if !strings.Contains(body, "s.Tag.AppendText(dst)") {
			t.Errorf("expected s.Tag.AppendText(dst) in AppendJSON, got:\n%s", body)
		}
		if strings.Contains(body, "json.Unmarshal(") {
			t.Errorf("decode path used json.Unmarshal — static text dispatch failed:\n%s", body)
		}
		if strings.Contains(body, "json.Marshal(") {
			t.Errorf("encode path used json.Marshal — static text dispatch failed:\n%s", body)
		}
	})

	// taggedExtPkg defines a TextMarshaler/Unmarshaler type with no
	// `import "encoding"` or interface assertion — purely structural
	// satisfaction. Shared across CrossPkgText_RealWorldThirdParty and
	// CrossPkgText_GoWorkspace which only differ in how the consumer
	// module is wired to the type.
	const taggedExtPkg = `package ext

type Tagged struct {
	Name string
	Tag  string
}

func (t Tagged) MarshalText() ([]byte, error) {
	return []byte(t.Name + "#" + t.Tag), nil
}

func (t *Tagged) UnmarshalText(b []byte) error {
	for i := 0; i < len(b); i++ {
		if b[i] == '#' {
			t.Name = string(b[:i])
			t.Tag = string(b[i+1:])
			return nil
		}
	}
	t.Name = string(b)
	return nil
}
`
	// Consumer source referencing the ext package via the temp module's
	// own path — no dependency on any sub-package of this repo. The
	// `tagged` import alias documents that the generator must resolve
	// the type via go/types method-set scan, not by name.
	const taggedConsumerSrc = `package consumer

import tagged "consumertest/ext"

//ggen:generate
type Msg struct {
	Tag tagged.Tagged ` + "`" + `json:"tag"` + "`" + `
}
`

	t.Run("CrossPkgText_RealWorldThirdParty", func(t *testing.T) {
		t.Parallel()
		// Real-world flow: a temp module references a type from another
		// package in the SAME consumer module — TextMarshaler /
		// UnmarshalText satisfied purely structurally (no `import
		// "encoding"`, no interface assertion). The generator must
		// detect it via types.Implements against std interfaces it
		// loads on its own. Exercises the type-info dispatch end to
		// end without depending on any sub-package of ggen itself.
		base := t.TempDir()
		writeFixture(t, filepath.Join(base, "go.mod"), `module consumertest

go 1.26
`)
		writeFixture(t, filepath.Join(base, "ext", "tagged.go"), taggedExtPkg)
		writeFixture(t, filepath.Join(base, "consumer", "msg.go"), taggedConsumerSrc)
		if out, err := runCLI(t, bin, base, "./consumer"); err != nil {
			t.Fatalf("ggen ./consumer: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(base, "consumer", "consumer_ggen.go"))

		// Tagged has MarshalText (value receiver) + UnmarshalText (pointer
		// receiver). No AppendText — encode side picks TextMarshaler branch.
		if got := strings.Count(body, "result.Tag.UnmarshalText("); got != 2 {
			t.Errorf("expected 2 result.Tag.UnmarshalText calls (DecodeFrom + DecodeStreamFrom), got %d:\n%s", got, body)
		}
		if !strings.Contains(body, "s.Tag.MarshalText()") {
			t.Errorf("expected s.Tag.MarshalText() in AppendJSON, got:\n%s", body)
		}
		if strings.Contains(body, "json.Unmarshal(") || strings.Contains(body, "json.Marshal(") {
			t.Errorf("real-world flow regressed to json fallback:\n%s", body)
		}
	})

	t.Run("CrossPkgText_GoWorkspace", func(t *testing.T) {
		t.Parallel()
		// go.work flow: the consumer module and the ext module are two
		// separate modules wired together via a workspace. Same
		// structural-detection assertions as the single-module variant;
		// this proves the generator's loader honors workspace mode
		// (packages.Load picks up GOFLAGS / go.work the same way
		// `go build` does).
		base := t.TempDir()
		consumerDir := filepath.Join(base, "consumer")
		extDir := filepath.Join(base, "ext")
		writeFixture(t, filepath.Join(base, "go.work"), `go 1.26

use (
	./consumer
	./ext
)
`)
		writeFixture(t, filepath.Join(extDir, "go.mod"), `module consumertest/ext

go 1.26
`)
		writeFixture(t, filepath.Join(extDir, "tagged.go"), taggedExtPkg)
		writeFixture(t, filepath.Join(consumerDir, "go.mod"), `module consumertest

go 1.26
`)
		writeFixture(t, filepath.Join(consumerDir, "msg.go"), taggedConsumerSrc)
		if out, err := runCLI(t, bin, base, "./consumer"); err != nil {
			t.Fatalf("ggen ./consumer: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(consumerDir, "consumer_ggen.go"))

		if got := strings.Count(body, "result.Tag.UnmarshalText("); got != 2 {
			t.Errorf("expected 2 result.Tag.UnmarshalText calls under go.work, got %d:\n%s", got, body)
		}
		if !strings.Contains(body, "s.Tag.MarshalText()") {
			t.Errorf("expected s.Tag.MarshalText() under go.work, got:\n%s", body)
		}
		if strings.Contains(body, "json.Unmarshal(") || strings.Contains(body, "json.Marshal(") {
			t.Errorf("workspace flow regressed to json fallback:\n%s", body)
		}
	})

	t.Run("CrossPkgText_FallbackWithoutModule", func(t *testing.T) {
		t.Parallel()
		// Mirror of CrossPkgText_StaticDispatch but WITHOUT a go.mod:
		// packages.Load returns nothing, generator falls into AST-only mode,
		// and the cross-package field type goes through the json fallback.
		// Pins the degraded-mode contract documented in parse.go.
		base := t.TempDir()
		writeFixture(t, filepath.Join(base, "ext", "text.go"), `package ext

type Tagged struct {
	Name string
	Tag  string
}

func (t Tagged) AppendText(dst []byte) ([]byte, error) { return append(dst, t.Name...), nil }
func (t *Tagged) UnmarshalText(b []byte) error         { t.Name = string(b); return nil }
`)
		consumerDir := filepath.Join(base, "consumer")
		writeFixture(t, filepath.Join(consumerDir, "msg.go"), `package consumer

import "crosspkgtest/ext"

//ggen:generate
type Msg struct {
	Tag ext.Tagged `+"`"+`json:"tag"`+"`"+`
}
`)
		if out, err := runCLI(t, bin, consumerDir, "."); err != nil {
			t.Fatalf("ggen .: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(consumerDir, "consumer_ggen.go"))
		// Without type info, the generator can't resolve interfaces — every
		// generated method falls back to encoding/json against result.Tag /
		// s.Tag specifically. Field-bound match confirms the json fallback
		// landed at the correct call site and not some adjacent emission.
		if got := strings.Count(body, "json.Unmarshal(data[start:i], &result.Tag)"); got != 1 {
			t.Errorf("expected 1 json.Unmarshal on &result.Tag in DecodeFrom, got %d:\n%s", got, body)
		}
		if got := strings.Count(body, "json.Unmarshal(s.Bytes()[start:s.Pos], &result.Tag)"); got != 1 {
			t.Errorf("expected 1 json.Unmarshal on &result.Tag in DecodeStreamFrom, got %d:\n%s", got, body)
		}
		if !strings.Contains(body, "json.Marshal(s.Tag)") {
			t.Errorf("expected json.Marshal(s.Tag) in AppendJSON, got:\n%s", body)
		}
		if strings.Contains(body, ".AppendText(") || strings.Contains(body, ".UnmarshalText(") {
			t.Errorf("expected no static text dispatch without go.mod, got:\n%s", body)
		}
	})

	// --- @Func custom validators / mods ---

	t.Run("CustomFunc_SamePackage_Validator", func(t *testing.T) {
		t.Parallel()
		// Same-package validator: `ggen:"@EvenOnly"` resolves to a top-level
		// function in the SAME package as the struct. No import needed.
		// Generated code calls EvenOnly(result.N) directly — no registry,
		// no any-boxing, no allocs.
		base := t.TempDir()
		dir := filepath.Join(base, "customfunc")
		writeFixture(t, filepath.Join(base, "go.mod"), `module customfunc

go 1.26
`)
		writeFixture(t, filepath.Join(dir, "msg.go"), `package customfunc

import "fmt"

//ggen:generate
type Msg struct {
	N int `+"`"+`json:"n" ggen:"@EvenOnly"`+"`"+`
}

func EvenOnly(n int) error {
	if n%2 != 0 {
		return fmt.Errorf("not even")
	}
	return nil
}
`)
		if out, err := runCLI(t, bin, base, "./customfunc"); err != nil {
			t.Fatalf("ggen ./customfunc: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(dir, "customfunc_ggen.go"))
		if !strings.Contains(body, "if err := EvenOnly(result.N); err != nil") {
			t.Errorf("expected direct EvenOnly call, got:\n%s", body)
		}
		if !strings.Contains(body, `Name: "@EvenOnly"`) {
			t.Errorf("expected CustomError stamped with @EvenOnly, got:\n%s", body)
		}
	})

	t.Run("CustomFunc_SamePackage_PureMod", func(t *testing.T) {
		t.Parallel()
		// Same-package pure mod: `mod:"@Squash"` is `func(string) string`.
		// Generated code emits direct assignment — no error-propagation
		// branch since the signature is non-fallible.
		base := t.TempDir()
		dir := filepath.Join(base, "puremod")
		writeFixture(t, filepath.Join(base, "go.mod"), `module puremod

go 1.26
`)
		writeFixture(t, filepath.Join(dir, "msg.go"), `package puremod

import "strings"

//ggen:generate
type Msg struct {
	S string `+"`"+`json:"s" mod:"@Squash"`+"`"+`
}

func Squash(s string) string { return strings.ReplaceAll(s, " ", "") }
`)
		if out, err := runCLI(t, bin, base, "./puremod"); err != nil {
			t.Fatalf("ggen ./puremod: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(dir, "puremod_ggen.go"))
		if !strings.Contains(body, "result.S = Squash(result.S)") {
			t.Errorf("expected pure mod assignment, got:\n%s", body)
		}
	})

	t.Run("CustomFunc_SamePackage_FallibleMod", func(t *testing.T) {
		t.Parallel()
		// Fallible mod: `func(T) (T, error)`. Generator detects the
		// 2-result signature and emits an error-propagation branch.
		// Errors surface as parse errors (early return), not validation.
		base := t.TempDir()
		dir := filepath.Join(base, "fallible")
		writeFixture(t, filepath.Join(base, "go.mod"), `module fallible

go 1.26
`)
		writeFixture(t, filepath.Join(dir, "msg.go"), `package fallible

import "fmt"

//ggen:generate
type Msg struct {
	S string `+"`"+`json:"s" mod:"@Reject"`+"`"+`
}

func Reject(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("empty")
	}
	return s, nil
}
`)
		if out, err := runCLI(t, bin, base, "./fallible"); err != nil {
			t.Fatalf("ggen ./fallible: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(dir, "fallible_ggen.go"))
		if !strings.Contains(body, "if v, err := Reject(result.S); err != nil") {
			t.Errorf("expected fallible mod with err-prop branch, got:\n%s", body)
		}
		if !strings.Contains(body, "return result, i, err") {
			t.Errorf("expected parse-error return on fallible mod, got:\n%s", body)
		}
	})

	t.Run("CustomFunc_CrossPackage", func(t *testing.T) {
		t.Parallel()
		// Cross-package: `@ext.Validate`. ggen runs from INSIDE the consumer
		// dir with arg `.` — it can't physically see ext/v.go through its
		// own loaders, only resolve the import via packages.Load walking up
		// to the module's go.mod. That's the realistic flow: users invoke
		// ggen on their own package, not on a parent dir holding peers.
		base := t.TempDir()
		writeFixture(t, filepath.Join(base, "go.mod"), `module customfunc

go 1.26
`)
		writeFixture(t, filepath.Join(base, "ext", "v.go"), `package ext

import "fmt"

func Validate(s string) error {
	if s == "" {
		return fmt.Errorf("empty")
	}
	return nil
}
`)
		consumerDir := filepath.Join(base, "consumer")
		writeFixture(t, filepath.Join(consumerDir, "msg.go"), `package consumer

import _ "customfunc/ext"

//ggen:generate
type Msg struct {
	S string `+"`"+`json:"s" ggen:"@ext.Validate"`+"`"+`
}
`)
		if out, err := runCLI(t, bin, consumerDir, "."); err != nil {
			t.Fatalf("ggen .: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(consumerDir, "consumer_ggen.go"))
		if !strings.Contains(body, "if err := ext.Validate(result.S); err != nil") {
			t.Errorf("expected qualified ext.Validate call, got:\n%s", body)
		}
		if !strings.Contains(body, `"customfunc/ext"`) {
			t.Errorf("expected customfunc/ext import in generated file, got:\n%s", body)
		}
	})

	t.Run("CustomFunc_CrossPackage_BlankImport_NameMismatch", func(t *testing.T) {
		t.Parallel()
		// Edge case: directory is `lib` but the package inside declares
		// `package crunchy`. User blank-imports with `_ "customfunc/lib"`
		// purely so ggen can resolve `@crunchy.Validate`. The resolver must:
		//   1. Notice the file's `_`-aliased import.
		//   2. Walk the import path → load the actual package via go/types.
		//   3. Match the tag prefix against the package's declared Name(),
		//      not the directory basename.
		// Generated file then qualifies the call as `crunchy.Validate(...)`
		// and adds the path-based import (no alias needed since `crunchy`
		// is the natural identifier for that package).
		base := t.TempDir()
		writeFixture(t, filepath.Join(base, "go.mod"), `module customfunc

go 1.26
`)
		writeFixture(t, filepath.Join(base, "lib", "v.go"), `package crunchy

import "fmt"

func Validate(s string) error {
	if s == "" {
		return fmt.Errorf("empty")
	}
	return nil
}
`)
		consumerDir := filepath.Join(base, "consumer")
		writeFixture(t, filepath.Join(consumerDir, "msg.go"), `package consumer

import _ "customfunc/lib"

//ggen:generate
type Msg struct {
	S string `+"`"+`json:"s" ggen:"@crunchy.Validate"`+"`"+`
}
`)
		if out, err := runCLI(t, bin, consumerDir, "."); err != nil {
			t.Fatalf("ggen .: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(consumerDir, "consumer_ggen.go"))
		if !strings.Contains(body, "if err := crunchy.Validate(result.S); err != nil") {
			t.Errorf("expected qualified crunchy.Validate call, got:\n%s", body)
		}
		if !strings.Contains(body, `"customfunc/lib"`) {
			t.Errorf("expected customfunc/lib import in generated file, got:\n%s", body)
		}
	})

	t.Run("CustomFunc_SignatureMismatch_Error", func(t *testing.T) {
		t.Parallel()
		// Signature mismatch: validator returns wrong type → generator
		// must reject at parse time, not silently emit broken code.
		base := t.TempDir()
		dir := filepath.Join(base, "badshape")
		writeFixture(t, filepath.Join(base, "go.mod"), `module badshape

go 1.26
`)
		writeFixture(t, filepath.Join(dir, "msg.go"), `package badshape

//ggen:generate
type Msg struct {
	S string `+"`"+`json:"s" ggen:"@WrongShape"`+"`"+`
}

// WrongShape returns string instead of error — invalid validator shape.
func WrongShape(s string) string { return s }
`)
		out, err := runCLI(t, bin, base, "./badshape")
		if err == nil {
			t.Fatalf("expected ggen to reject signature mismatch, got success:\n%s", out)
		}
		if !strings.Contains(out, "validator must return error") {
			t.Errorf("expected diagnostic about validator return type, got:\n%s", out)
		}
	})

	t.Run("CustomFunc_ParamMismatch_Error", func(t *testing.T) {
		t.Parallel()
		// Param-type mismatch: field is `int` but `@Foo` takes `string`.
		// Generator must reject at parse time. Locks down the
		// types.Identical(param, fieldType) check in resolveCustomFunc.
		base := t.TempDir()
		dir := filepath.Join(base, "paramshape")
		writeFixture(t, filepath.Join(base, "go.mod"), `module paramshape

go 1.26
`)
		writeFixture(t, filepath.Join(dir, "msg.go"), `package paramshape

//ggen:generate
type Msg struct {
	N int `+"`"+`json:"n" ggen:"@WrongParam"`+"`"+`
}

// WrongParam takes string instead of int — param mismatch.
func WrongParam(s string) error { return nil }
`)
		out, err := runCLI(t, bin, base, "./paramshape")
		if err == nil {
			t.Fatalf("expected ggen to reject param mismatch, got success:\n%s", out)
		}
		if !strings.Contains(out, "param type string does not match field type int") {
			t.Errorf("expected diagnostic about param type, got:\n%s", out)
		}
	})

	// --- top-level alias types ---

	t.Run("Alias_Primitive_Generates", func(t *testing.T) {
		t.Parallel()
		// `type HtmlString string` annotated with //ggen:generate should
		// produce DecodeFrom / AppendJSON methods on the named type.
		// Smoke test: compile-level check — the generated file exists,
		// declares the methods, and references the alias type literally.
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "msg.go"), `package fixture

//ggen:generate
type HtmlString string

//ggen:generate
type Count int
`)
		if out, err := runCLI(t, bin, dir, "msg.go"); err != nil {
			t.Fatalf("ggen msg.go: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(dir, "msg_ggen.go"))
		if !strings.Contains(body, "(result HtmlString) DecodeFrom") {
			t.Errorf("HtmlString.DecodeFrom missing:\n%s", body)
		}
		if !strings.Contains(body, "result = HtmlString(v)") {
			t.Errorf("HtmlString cast missing:\n%s", body)
		}
		if !strings.Contains(body, "(result Count) DecodeFrom") {
			t.Errorf("Count.DecodeFrom missing:\n%s", body)
		}
		if !strings.Contains(body, "strconv.AppendInt(dst, int64(s), 10)") {
			t.Errorf("Count AppendJSON missing strconv call:\n%s", body)
		}
	})

	t.Run("Alias_Container_Generates", func(t *testing.T) {
		t.Parallel()
		// Slice / map / array aliases over primitive elements must
		// generate through the AST-only fallback (no go.mod here).
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "msg.go"), `package fixture

//ggen:generate
type Tags []string

//ggen:generate
type Lookup map[string]int

//ggen:generate
type Tuple [3]int
`)
		if out, err := runCLI(t, bin, dir, "msg.go"); err != nil {
			t.Fatalf("ggen msg.go: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(dir, "msg_ggen.go"))
		for _, want := range []string{
			") DecodeFrom(data []byte) (Tags",
			"s Tags) AppendJSON",
			") DecodeFrom(data []byte) (Lookup",
			"s Lookup) AppendJSON",
			") DecodeFrom(data []byte) (Tuple",
			"s Tuple) AppendJSON",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("expected %q in generated body:\n%s", want, body)
			}
		}
	})

	t.Run("Alias_RejectsUnsupported", func(t *testing.T) {
		t.Parallel()
		// Interfaces, channels, funcs have no JSON wire shape — generator
		// must error at parse time rather than emit bogus code.
		cases := []struct {
			name string
			body string
		}{
			{"interface", "type X interface{ M() }"},
			{"chan", "type X chan int"},
			{"func", "type X func(int) int"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				writeFixture(t, filepath.Join(dir, "msg.go"), `package fixture

//ggen:generate
`+tc.body+`
`)
				out, err := runCLI(t, bin, dir, "msg.go")
				if err == nil {
					t.Fatalf("expected ggen to reject %s alias, got success:\n%s", tc.name, out)
				}
				if !strings.Contains(out, "unsupported") && !strings.Contains(out, "primitive") {
					t.Errorf("expected diagnostic about unsupported alias kind for %s, got: %s", tc.name, out)
				}
			})
		}
	})

	t.Run("CustomFunc_NotFound_Error", func(t *testing.T) {
		t.Parallel()
		// `@Missing` references a function that doesn't exist. Generator
		// must reject at parse time.
		base := t.TempDir()
		dir := filepath.Join(base, "missing")
		writeFixture(t, filepath.Join(base, "go.mod"), `module missing

go 1.26
`)
		writeFixture(t, filepath.Join(dir, "msg.go"), `package missing

//ggen:generate
type Msg struct {
	S string `+"`"+`json:"s" ggen:"@Missing"`+"`"+`
}
`)
		out, err := runCLI(t, bin, base, "./missing")
		if err == nil {
			t.Fatalf("expected ggen to reject missing func, got success:\n%s", out)
		}
		if !strings.Contains(out, "Missing not found") {
			t.Errorf("expected 'not found' diagnostic, got:\n%s", out)
		}
	})

	// --- usage / no-args ---

	t.Run("NoArgs_PrintsUsage", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		out, err := runCLI(t, bin, dir)
		if err == nil {
			t.Fatalf("expected non-zero exit when no args supplied\n%s", out)
		}
		if !strings.Contains(out, "usage:") {
			t.Fatalf("expected usage text, got: %s", out)
		}
	})

	// --- output formatting ---
	//
	// The rendered output is always run through go/format.Source so
	// generated files land gofmt-clean.

	t.Run("DefaultOutputIsGofmtClean", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "msg.go"), minimalStruct)
		if out, err := runCLI(t, bin, dir, "msg.go"); err != nil {
			t.Fatalf("ggen msg.go: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(dir, "msg_ggen.go"))
		// gofmt-clean signal: inside DecodeFrom's body, statements
		// emitted by the renderer get the same one-tab indent as the
		// first line. Look for `\n\tvar err error\n` — gofmt indents
		// it; an unformatted renderer would leave it flush-left.
		if !strings.Contains(body, "\n\tvar err error\n") {
			t.Errorf("default output not gofmt-clean (var err error not indented):\n%s", body)
		}
	})

	// --- rule applicability / value-shape rejection ---
	//
	// Every validation and mod rule that's coupled to a specific kind
	// (string rules, numeric rules, len-rules, …) plus every numeric
	// value parameter (`len=abc`, `gt=abc`, …) must be rejected at
	// parse time with a clear diagnostic — silently emitting broken
	// generated Go is the worst UX.
	t.Run("InvalidRuleApplication", func(t *testing.T) {
		t.Parallel()
		type bad struct {
			name     string // subtest name
			fieldGo  string // Go field declaration (e.g. `N int`)
			tag      string // full struct-tag content (everything between backticks)
			wantDiag string // substring expected somewhere in stderr+stdout
		}
		// Every entry produces a single-file fixture under a fresh
		// temp dir and runs ggen against it; the CLI must exit
		// non-zero and the diagnostic must contain `wantDiag`. Cases
		// are grouped by rule for grep-ability.
		cases := []bad{
			// ----- string-only rules on non-strings -----
			{"ascii_on_int", "N int", `json:"n" ggen:"ascii"`, "`ascii` is inapplicable to int"},
			{"email_on_int", "N int", `json:"n" ggen:"email"`, "`email` is inapplicable to int"},
			{"url_on_bool", "B bool", `json:"b" ggen:"url"`, "`url` is inapplicable to bool"},
			{"printable_on_float", "F float64", `json:"f" ggen:"printable"`, "`printable` is inapplicable to float64"},
			{"alphanum_on_int", "N int", `json:"n" ggen:"alphanum"`, "`alphanum` is inapplicable to int"},
			{"numericrule_on_int", "N int", `json:"n" ggen:"numeric"`, "`numeric` is inapplicable to int"},
			{"lower_on_int", "N int", `json:"n" ggen:"lower"`, "`lower` is inapplicable to int"},
			{"upper_on_int", "N int", `json:"n" ggen:"upper"`, "`upper` is inapplicable to int"},
			{"hexadecimal_on_int", "N int", `json:"n" ggen:"hexadecimal"`, "`hexadecimal` is inapplicable to int"},
			{"starts_on_int", "N int", `json:"n" ggen:"starts=foo"`, "`starts` is inapplicable to int"},
			{"ends_on_int", "N int", `json:"n" ggen:"ends=foo"`, "`ends` is inapplicable to int"},
			{"contains_on_int", "N int", `json:"n" ggen:"contains=foo"`, "`contains` is inapplicable to int"},
			{"runes_on_int", "N int", `json:"n" ggen:"runes=3"`, "`runes` is inapplicable to int"},
			{"minrunes_on_int", "N int", `json:"n" ggen:"minrunes=3"`, "`minrunes` is inapplicable to int"},
			{"maxrunes_on_int", "N int", `json:"n" ggen:"maxrunes=3"`, "`maxrunes` is inapplicable to int"},
			{"runes_on_bytes", "B []byte", `json:"b" ggen:"runes=3"`, "`runes` is inapplicable to []byte"},
			{"ascii_on_bytes", "B []byte", `json:"b" ggen:"ascii"`, "`ascii` is inapplicable to []byte"},
			{"starts_empty", "S string", `json:"s" ggen:"starts="`, `requires a non-empty value`},
			{"ends_empty", "S string", `json:"s" ggen:"ends="`, `requires a non-empty value`},
			{"contains_empty", "S string", `json:"s" ggen:"contains="`, `requires a non-empty value`},

			// ----- numeric-only rules on non-numerics -----
			{"gt_on_string", "S string", `json:"s" ggen:"gt=1"`, "`gt` is inapplicable to string"},
			{"gte_on_string", "S string", `json:"s" ggen:"gte=1"`, "`gte` is inapplicable to string"},
			{"lt_on_string", "S string", `json:"s" ggen:"lt=1"`, "`lt` is inapplicable to string"},
			{"lte_on_string", "S string", `json:"s" ggen:"lte=1"`, "`lte` is inapplicable to string"},
			{"gt_on_bool", "B bool", `json:"b" ggen:"gt=0"`, "`gt` is inapplicable to bool"},
			{"gt_on_slice", "X []int", `json:"x" ggen:"gt=0"`, "`gt` is inapplicable to []int"},
			{"gt_bad_value", "N int", `json:"n" ggen:"gt=abc"`, `value is not a valid number`},
			{"gte_bad_value", "N int", `json:"n" ggen:"gte=abc"`, `value is not a valid number`},
			{"lt_bad_value", "N int", `json:"n" ggen:"lt=abc"`, `value is not a valid number`},
			{"lte_bad_value", "N int", `json:"n" ggen:"lte=abc"`, `value is not a valid number`},
			{"gt_missing_value", "N int", `json:"n" ggen:"gt"`, `requires a numeric value`},

			// ----- multiple: integers only -----
			{"multiple_on_string", "S string", `json:"s" ggen:"multiple=2"`, "`multiple` is inapplicable to string"},
			{"multiple_on_float", "F float64", `json:"f" ggen:"multiple=2"`, "`multiple` is inapplicable to float64"},
			{"multiple_on_bool", "B bool", `json:"b" ggen:"multiple=2"`, "`multiple` is inapplicable to bool"},
			{"multiple_bad_value", "N int", `json:"n" ggen:"multiple=abc"`, `value is not a valid integer`},
			{"multiple_missing", "N int", `json:"n" ggen:"multiple"`, `requires an integer value`},

			// ----- len/minlen/maxlen/notempty on non-len-able kinds -----
			{"len_on_int", "N int", `json:"n" ggen:"len=5"`, "`len` is inapplicable to int"},
			{"len_on_bool", "B bool", `json:"b" ggen:"len=1"`, "`len` is inapplicable to bool"},
			{"len_on_float", "F float64", `json:"f" ggen:"len=1"`, "`len` is inapplicable to float64"},
			{"minlen_on_int", "N int", `json:"n" ggen:"minlen=1"`, "`minlen` is inapplicable to int"},
			{"maxlen_on_int", "N int", `json:"n" ggen:"maxlen=1"`, "`maxlen` is inapplicable to int"},
			{"notempty_on_int", "N int", `json:"n" ggen:"notempty"`, "`notempty` is inapplicable to int"},
			{"notempty_on_bool", "B bool", `json:"b" ggen:"notempty"`, "`notempty` is inapplicable to bool"},
			{"len_bad_value", "S string", `json:"s" ggen:"len=abc"`, `value is not a valid integer`},
			{"minlen_bad_value", "S string", `json:"s" ggen:"minlen=abc"`, `value is not a valid integer`},
			{"maxlen_bad_value", "S string", `json:"s" ggen:"maxlen=abc"`, `value is not a valid integer`},
			{"len_missing_value", "S string", `json:"s" ggen:"len"`, `requires an integer value`},
			{"len_float_value", "S string", `json:"s" ggen:"len=1.5"`, `value is not a valid integer`},
			{"runes_bad_value", "S string", `json:"s" ggen:"runes=abc"`, `value is not a valid integer`},
			{"minrunes_bad_value", "S string", `json:"s" ggen:"minrunes=abc"`, `value is not a valid integer`},

			// ----- eq / neq scope: not bool, not slice, not float-with-text -----
			{"eq_on_slice", "X []int", `json:"x" ggen:"eq=1"`, "`eq` is inapplicable to []int"},
			{"neq_on_slice", "X []int", `json:"x" ggen:"neq=1"`, "`neq` is inapplicable to []int"},
			{"eq_on_bool", "B bool", `json:"b" ggen:"eq=true"`, "`eq` is inapplicable to bool"},
			{"neq_on_bool", "B bool", `json:"b" ggen:"neq=true"`, "`neq` is inapplicable to bool"},
			{"eq_int_bad_value", "N int", `json:"n" ggen:"eq=abc"`, `value is not a valid number`},
			{"neq_int_bad_value", "N int", `json:"n" ggen:"neq=abc"`, `value is not a valid number`},

			// ----- oneof: must be string/numeric, non-empty, numeric parts numeric -----
			{"oneof_on_bool", "B bool", `json:"b" ggen:"oneof=true|false"`, "`oneof` is inapplicable to bool"},
			{"oneof_on_slice", "X []int", `json:"x" ggen:"oneof=1|2"`, "`oneof` is inapplicable to []int"},
			{"oneof_empty", "S string", `json:"s" ggen:"oneof"`, `oneof` + "` requires a "}, // matches "rule `oneof` requires a `|`-separated…"
			{"oneof_numeric_bad_part", "N int", `json:"n" ggen:"oneof=1|two|3"`, `part "two" is not a valid number`},

			// ----- hintlen restricted to slice/map -----
			{"hintlen_on_int", "N int", `json:"n" ggen:"hintlen=10"`, "`hintlen` is only valid on slice/map fields"},
			{"hintlen_on_string", "S string", `json:"s" ggen:"hintlen=10"`, "`hintlen` is only valid on slice/map fields"},
			{"hintlen_on_bool", "B bool", `json:"b" ggen:"hintlen=10"`, "`hintlen` is only valid on slice/map fields"},
			{"hintlen_on_array", "X [3]int", `json:"x" ggen:"hintlen=10"`, "`hintlen` is only valid on slice/map fields"},
			{"hintlen_zero_on_int", "N int", `json:"n" ggen:"hintlen=0"`, "`hintlen` is only valid on slice/map fields"},
			{"hintlen_negative", "X []int", `json:"x" ggen:"hintlen=-1"`, "hintlen=-1 must be ≥ 0"},
			{"hintlen_non_numeric", "X []int", `json:"x" ggen:"hintlen=abc"`, `hintlen="abc" is not a valid integer`},

			// ----- dive: only on slice/array/map -----
			{"dive_on_string", "S string", `json:"s" ggen:"dive:minlen=1"`, "`dive:` tag prefix is only valid on slice/array/map fields"},
			{"dive_on_int", "N int", `json:"n" ggen:"dive:gte=0"`, "`dive:` tag prefix is only valid on slice/array/map fields"},
			{"dive_mod_on_int", "N int", `json:"n" mod:"dive:trim"`, "`dive:` tag prefix is only valid on slice/array/map fields"},

			// ----- keys: only on maps -----
			{"keys_on_string", "S string", `json:"s" ggen:"keys:minlen=1"`, "`keys:` tag prefix is only valid on map[string]V fields"},
			{"keys_on_slice", "X []int", `json:"x" ggen:"keys:minlen=1"`, "`keys:` tag prefix is only valid on map[string]V fields"},
			{"keys_mod_on_string", "S string", `json:"s" mod:"keys:trim"`, "`keys:` tag prefix is only valid on map[string]V fields"},
			// keys: rules themselves are typed against KindString — verify
			// a non-string-applicable rule still gets rejected even though
			// the parent IS a map.
			{"keys_numeric_rule", "M map[string]int", `json:"m" ggen:"keys:gt=1"`, "`gt` is inapplicable to string"},

			// ----- dive: element kind mismatch -----
			// []int element is int — string rules invalid on the element.
			{"dive_ascii_on_int_elem", "X []int", `json:"x" ggen:"dive:ascii"`, "`ascii` is inapplicable to int"},
			{"dive_email_on_int_elem", "X []int", `json:"x" ggen:"dive:email"`, "`email` is inapplicable to int"},
			{"dive_len_on_int_elem", "X []int", `json:"x" ggen:"dive:len=3"`, "`len` is inapplicable to int"},
			// map[string]int value is int.
			{"dive_ascii_on_int_mapval", "M map[string]int", `json:"m" ggen:"dive:ascii"`, "`ascii` is inapplicable to int"},
			// []string element is string — numeric rules invalid.
			{"dive_gt_on_string_elem", "X []string", `json:"x" ggen:"dive:gt=1"`, "`gt` is inapplicable to string"},

			// ----- mods: string mods on numerics, numeric mods on strings -----
			{"trim_on_int", "N int", `json:"n" mod:"trim"`, "`trim` is inapplicable to int"},
			{"lower_mod_on_int", "N int", `json:"n" mod:"lower"`, "`lower` is inapplicable to int"},
			{"upper_mod_on_int", "N int", `json:"n" mod:"upper"`, "`upper` is inapplicable to int"},
			{"trimleft_on_int", "N int", `json:"n" mod:"trimleft=foo"`, "`trimleft` is inapplicable to int"},
			{"trimright_on_int", "N int", `json:"n" mod:"trimright=foo"`, "`trimright` is inapplicable to int"},
			{"replace_on_int", "N int", `json:"n" mod:"replace=a|b"`, "`replace` is inapplicable to int"},
			{"clamp_on_string", "S string", `json:"s" mod:"clamp=0|10"`, "`clamp` is inapplicable to string"},
			{"clamp_on_bool", "B bool", `json:"b" mod:"clamp=0|10"`, "`clamp` is inapplicable to bool"},
			{"clamp_on_slice", "X []int", `json:"x" mod:"clamp=0|10"`, "`clamp` is inapplicable to []int"},

			// ----- mods: parameter-shape rejection -----
			{"trimleft_empty", "S string", `json:"s" mod:"trimleft="`, "`trimleft` requires a non-empty value"},
			{"trimright_empty", "S string", `json:"s" mod:"trimright="`, "`trimright` requires a non-empty value"},
			{"replace_missing_pipe", "S string", `json:"s" mod:"replace=foo"`, "requires `old|new` form"},
			{"replace_empty_old", "S string", `json:"s" mod:"replace=|new"`, "requires `old|new` form"},
			{"clamp_missing_pipe", "N int", `json:"n" mod:"clamp=10"`, "is missing the lo`|`hi separator"},
			{"clamp_both_empty", "N int", `json:"n" mod:"clamp=|"`, "requires at least one of lo or hi"},
			{"clamp_bad_lo", "N int", `json:"n" mod:"clamp=abc|10"`, "lo \"abc\" is not a valid number"},
			{"clamp_bad_hi", "N int", `json:"n" mod:"clamp=0|abc"`, "hi \"abc\" is not a valid number"},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				src := `package fixture

//ggen:generate
type Msg struct {
	` + tc.fieldGo + " `" + tc.tag + "`" + `
}
`
				writeFixture(t, filepath.Join(dir, "msg.go"), src)
				out, err := runCLI(t, bin, dir, "msg.go")
				if err == nil {
					t.Fatalf("expected ggen to reject %s, got success:\n%s", tc.name, out)
				}
				if !strings.Contains(out, tc.wantDiag) {
					t.Errorf("diagnostic missing for %s\nwant substring: %q\ngot:\n%s",
						tc.name, tc.wantDiag, out)
				}
			})
		}
	})
}
