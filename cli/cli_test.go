package main

// CLI integration tests, all under TestCLI so the built binary rides on the
// parent's t.TempDir(). Fixtures in temp dirs outside any module exercise the
// AST-only path with filename/basename-derived output paths.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildCLI compiles ggen into the parent test's TempDir and returns its path.
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

// runCLI invokes the built ggen binary inside dir and returns combined
// stdout+stderr plus the run error.
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

// writeGoMod drops a minimal go.mod into dir — walk-mode tests need module
// context for packages.Load.
func writeGoMod(t *testing.T, dir, module string) {
	t.Helper()
	writeFixture(t, filepath.Join(dir, "go.mod"), "module "+module+"\n\ngo 1.26\n")
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
		// flag.Parse stops at the first positional; main()'s interspersing
		// loop must still pick up `-o out.go` placed after the file arg.
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
		// A flag wedged between two positionals: both positionals must reach
		// generateSingleFile in order and the flag must still take effect.
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
		// Default level (-v not set) suppresses `wrote <file>` info lines.
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
		// -vv lifts to LevelDebug; the dir-mode `parsing <pkg>` line is the
		// stable marker.
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
		// -vvv lifts to LevelTrace; an annotation-free dir fires the
		// `no annotated structs` trace line.
		base := t.TempDir()
		writeGoMod(t, base, "vvvtrace")
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
		// Verbosity flags placed after a positional must still take effect.
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
		// Valueless bool flags after a positional must also be picked up.
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
		// No annotation and no name filter must error with a fix hint, not
		// fall back to all exported structs.
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
		// An explicit positional name processes a struct lacking an annotation.
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
		// Single-file mode restricts output to types declared in the passed
		// file, even inside a real module where packages.Load sees siblings.
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

		// Msg (msg.go) routes to the library build, MsgT (msg_test.go) to the
		// test build; neither may bleed into the other's _gen file.
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
		// A struct behind `//go:build foo` must land in a separate
		// `<dir>_foo_ggen.go` with the matching header, not pollute the
		// untagged gen file.
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

		// Untagged bucket: Plain in fixture_ggen.go, no //go:build header.
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

		// Tagged bucket: Tagged in fixture_foo_ggen.go, with the header.
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
		// Multi-term `//go:build foo && bar` canonicalizes to a `foo_bar`
		// filename slug; the header keeps the original expression verbatim.
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
		writeGoMod(t, base, "walksub")
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

	t.Run("WalkStopsAtSubModuleBoundary", func(t *testing.T) {
		t.Parallel()
		// `ggen ./...` is module-scoped: a subdirectory with its own go.mod
		// is a separate module and must not be visited by the parent's run.
		base := t.TempDir()
		writeGoMod(t, base, "rootmod")
		writeFixture(t, filepath.Join(base, "root.go"),
			strings.ReplaceAll(minimalStruct, "fixture", "rootmod"))
		sub := filepath.Join(base, "sub")
		writeGoMod(t, sub, "submod")
		writeFixture(t, filepath.Join(sub, "msg.go"),
			strings.ReplaceAll(strings.ReplaceAll(minimalStruct, "fixture", "sub"), "Msg", "SubMsg"))
		if out, err := runCLI(t, bin, base, "./..."); err != nil {
			t.Fatalf("ggen ./...: %v\n%s", err, out)
		}
		// Root module generated; sub-module skipped.
		mustHaveFile(t, filepath.Join(base, filepath.Base(base)+"_ggen.go"))
		mustNotHaveFile(t, filepath.Join(sub, "sub_ggen.go"))
	})

	t.Run("Dot_DoesNotRecurseIntoSubpackages", func(t *testing.T) {
		t.Parallel()
		// `.` is single-package mode: subdirectories must not be touched even
		// when they hold annotated structs (the `.` vs `./...` divergence).
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

	t.Run("Walk_RelativeSubtreePattern", func(t *testing.T) {
		t.Parallel()
		// `./pkg/...` scopes to a subtree; sibling dirs outside the prefix
		// are not processed.
		base := t.TempDir()
		writeGoMod(t, base, "subtree")
		root := filepath.Join(base, "pkg")
		leaf := filepath.Join(root, "leaf")
		writeFixture(t, filepath.Join(root, "top.go"),
			strings.ReplaceAll(minimalStruct, "fixture", "pkg"))
		writeFixture(t, filepath.Join(leaf, "msg.go"),
			strings.ReplaceAll(strings.ReplaceAll(minimalStruct, "fixture", "leaf"), "Msg", "Leaf"))
		// Sibling dir outside the prefix — must not be processed.
		sibling := filepath.Join(base, "other")
		writeFixture(t, filepath.Join(sibling, "msg.go"),
			strings.ReplaceAll(strings.ReplaceAll(minimalStruct, "fixture", "other"), "Msg", "Other"))
		if out, err := runCLI(t, bin, base, "./pkg/..."); err != nil {
			t.Fatalf("ggen ./pkg/...: %v\n%s", err, out)
		}
		mustHaveFile(t, filepath.Join(root, "pkg_ggen.go"))
		mustHaveFile(t, filepath.Join(leaf, "leaf_ggen.go"))
		mustNotHaveFile(t, filepath.Join(sibling, "other_ggen.go"))
	})

	t.Run("SingleFile_GathersErrorsAcrossStructs", func(t *testing.T) {
		t.Parallel()
		// Two structs with broken rules: one invocation must surface both
		// diagnostics, not just the first.
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "multi.go"), `package fixture

//ggen:generate
type Test1 struct {
	A int `+"`"+`json:"a" pipe:"alphanum"`+"`"+`
}

//ggen:generate
type Test2 struct {
	B int `+"`"+`json:"b" pipe:"numeric"`+"`"+`
}
`)
		out, err := runCLI(t, bin, dir, "multi.go", "Test1", "Test2")
		if err == nil {
			t.Fatalf("expected non-zero exit, got:\n%s", out)
		}
		if !strings.Contains(out, "`alphanum`") {
			t.Errorf("alphanum diagnostic missing, got:\n%s", out)
		}
		if !strings.Contains(out, "`numeric`") {
			t.Errorf("numeric diagnostic missing, got:\n%s", out)
		}
	})

	t.Run("SingleFile_GathersErrorsAcrossFields", func(t *testing.T) {
		t.Parallel()
		// Each bad field's applicability error must surface.
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "multi.go"), `package fixture

//ggen:generate
type Multi struct {
	A int `+"`"+`json:"a" pipe:"alphanum"`+"`"+`
	B int `+"`"+`json:"b" pipe:"numeric"`+"`"+`
	C string `+"`"+`json:"c" pipe:"gt=0"`+"`"+`
}
`)
		out, err := runCLI(t, bin, dir, "multi.go")
		if err == nil {
			t.Fatalf("expected non-zero exit, got:\n%s", out)
		}
		for _, want := range []string{"`alphanum`", "`numeric`", "`gt`"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing diagnostic %s, got:\n%s", want, out)
			}
		}
	})

	t.Run("SingleFile_GathersValAndModErrorsOnSameField", func(t *testing.T) {
		t.Parallel()
		// A field with both a bad val rule and a bad mod must surface both
		// diagnostics, not short-circuit on the first.
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "multi.go"), `package fixture

//ggen:generate
type Bad struct {
	A int `+"`"+`json:"a" pipe:"trim alphanum"`+"`"+`
}
`)
		out, err := runCLI(t, bin, dir, "multi.go")
		if err == nil {
			t.Fatalf("expected non-zero exit, got:\n%s", out)
		}
		// Both alphanum (val) and trim (mod) must appear, each with its own
		// position prefix.
		for _, want := range []string{"`alphanum`", "`trim`"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing diagnostic for %s, got:\n%s", want, out)
			}
		}
		// Two position prefixes — both sub-errors got positions.
		if strings.Count(out, "multi.go:") < 2 {
			t.Errorf("expected ≥2 position prefixes (val + mod), got:\n%s", out)
		}
	})

	t.Run("Walk_GathersErrorsAcrossPackages", func(t *testing.T) {
		t.Parallel()
		// Walk mode must not bail on the first failing package: two bad dirs
		// + one clean, all errors surface, the clean dir still writes, exit
		// is non-zero.
		base := t.TempDir()
		writeGoMod(t, base, "walkerrs")
		writeFixture(t, filepath.Join(base, "a", "msg.go"), `package a

//ggen:generate
type A struct {
	N int `+"`"+`json:"n" pipe:"alphanum"`+"`"+`
}
`)
		writeFixture(t, filepath.Join(base, "b", "msg.go"), `package b

//ggen:generate
type B struct {
	N int `+"`"+`json:"n" pipe:"numeric"`+"`"+`
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
		if !strings.Contains(out, "`alphanum`") {
			t.Errorf("package a's alphanum error missing, got:\n%s", out)
		}
		if !strings.Contains(out, "`numeric`") {
			t.Errorf("package b's numeric error missing, got:\n%s", out)
		}
		// The clean package still writes — the walk continued past the broken ones.
		if !strings.Contains(out, "wrote") || !strings.Contains(out, "c_ggen.go") {
			t.Errorf("clean package c should still emit `wrote ./c/c_ggen.go`, got:\n%s", out)
		}
	})

	t.Run("Walk_RejectsOutputOverride", func(t *testing.T) {
		t.Parallel()
		// `-o` (one file) is incompatible with pattern mode (one per package);
		// rejected up front, before packages.Load, so no go.mod is needed.
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

	// --- -dry: parse-only validation, no file emission ---

	t.Run("Dry_NoFileWritten", func(t *testing.T) {
		t.Parallel()
		// Each dispatch mode (single-file, directory, walk) must exit zero on
		// a clean fixture and leave no _ggen.go on disk.
		cases := []struct {
			name  string
			setup func(t *testing.T) (runDir string, args []string, mustAbsent []string)
		}{
			{
				name: "SingleFile",
				setup: func(t *testing.T) (string, []string, []string) {
					dir := t.TempDir()
					writeFixture(t, filepath.Join(dir, "msg.go"), minimalStruct)
					return dir, []string{"-dry", "msg.go"}, []string{filepath.Join(dir, "msg_ggen.go")}
				},
			},
			{
				name: "Directory",
				setup: func(t *testing.T) (string, []string, []string) {
					base := t.TempDir()
					dir := filepath.Join(base, "fixture")
					writeFixture(t, filepath.Join(dir, "msg.go"), minimalStruct)
					return dir, []string{"-dry", "."}, []string{filepath.Join(dir, "fixture_ggen.go")}
				},
			},
			{
				name: "Walk",
				setup: func(t *testing.T) (string, []string, []string) {
					base := t.TempDir()
					writeGoMod(t, base, "drywalk")
					a := filepath.Join(base, "a")
					b := filepath.Join(base, "b")
					writeFixture(t, filepath.Join(a, "msg.go"),
						strings.ReplaceAll(minimalStruct, "fixture", "a"))
					writeFixture(t, filepath.Join(b, "msg.go"),
						strings.ReplaceAll(strings.ReplaceAll(minimalStruct, "fixture", "b"), "Msg", "MsgB"))
					return base, []string{"-dry", "./..."}, []string{
						filepath.Join(a, "a_ggen.go"),
						filepath.Join(b, "b_ggen.go"),
					}
				},
			},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				runDir, args, absent := c.setup(t)
				if out, err := runCLI(t, bin, runDir, args...); err != nil {
					t.Fatalf("ggen %v: %v\n%s", args, err, out)
				}
				for _, p := range absent {
					mustNotHaveFile(t, p)
				}
			})
		}
	})

	t.Run("Dry_SurfacesAllErrors", func(t *testing.T) {
		t.Parallel()
		// Dry mode must surface every parse-time diagnostic, exit non-zero,
		// and leave nothing on disk — across both the per-field (single-file)
		// and per-package (walk) collection paths.
		cases := []struct {
			name  string
			setup func(t *testing.T) (runDir string, args, wantSubs, mustAbsent []string)
		}{
			{
				name: "SingleFile_AcrossFields",
				setup: func(t *testing.T) (string, []string, []string, []string) {
					dir := t.TempDir()
					writeFixture(t, filepath.Join(dir, "multi.go"), `package fixture

//ggen:generate
type Multi struct {
	A int `+"`"+`json:"a" pipe:"alphanum"`+"`"+`
	B int `+"`"+`json:"b" pipe:"numeric"`+"`"+`
	C string `+"`"+`json:"c" pipe:"gt=0"`+"`"+`
}
`)
					return dir, []string{"-dry", "multi.go"},
						[]string{"`alphanum`", "`numeric`", "`gt`"},
						[]string{filepath.Join(dir, "multi_ggen.go")}
				},
			},
			{
				name: "Walk_AcrossPackages",
				setup: func(t *testing.T) (string, []string, []string, []string) {
					base := t.TempDir()
					writeGoMod(t, base, "drywalkerrs")
					writeFixture(t, filepath.Join(base, "a", "msg.go"), `package a

//ggen:generate
type A struct {
	N int `+"`"+`json:"n" pipe:"alphanum"`+"`"+`
}
`)
					writeFixture(t, filepath.Join(base, "b", "msg.go"), `package b

//ggen:generate
type B struct {
	N int `+"`"+`json:"n" pipe:"numeric"`+"`"+`
}
`)
					return base, []string{"-dry", "./..."},
						[]string{"`alphanum`", "`numeric`"},
						[]string{
							filepath.Join(base, "a", "a_ggen.go"),
							filepath.Join(base, "b", "b_ggen.go"),
						}
				},
			},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				runDir, args, wantSubs, absent := c.setup(t)
				out, err := runCLI(t, bin, runDir, args...)
				if err == nil {
					t.Fatalf("expected non-zero exit, got:\n%s", out)
				}
				for _, want := range wantSubs {
					if !strings.Contains(out, want) {
						t.Errorf("missing diagnostic %s, got:\n%s", want, out)
					}
				}
				for _, p := range absent {
					mustNotHaveFile(t, p)
				}
			})
		}
	})

	t.Run("Dry_RejectsOutputOverride", func(t *testing.T) {
		t.Parallel()
		// -o and -pkg are dead in dry mode (nothing is written) — reject
		// up front.
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "msg.go"), minimalStruct)
		for _, args := range [][]string{
			{"-dry", "-o", filepath.Join(dir, "out.go"), "msg.go"},
			{"-dry", "-pkg", "renamed", "msg.go"},
		} {
			out, err := runCLI(t, bin, dir, args...)
			if err == nil {
				t.Fatalf("expected non-zero exit for %v, got:\n%s", args, out)
			}
			if !strings.Contains(out, "-dry") {
				t.Errorf("expected -dry rejection diagnostic for %v, got:\n%s", args, out)
			}
			mustNotHaveFile(t, filepath.Join(dir, "out.go"))
			mustNotHaveFile(t, filepath.Join(dir, "msg_ggen.go"))
		}
	})

	t.Run("WalkSkipsDotAndUnderscoreDirs", func(t *testing.T) {
		t.Parallel()
		// packages.Load inherits `go list`'s dot-/underscore-dir skip.
		base := t.TempDir()
		writeGoMod(t, base, "skipdirs")
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
	Name string `+"`"+`json:"name" pipe:"required minlen=1"`+"`"+`
}
`)
		// Default: rules emit typed errors.
		if out, err := runCLI(t, bin, dir, "msg.go"); err != nil {
			t.Fatalf("ggen default: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(dir, "msg_ggen.go"))
		if !strings.Contains(body, "MinLenError") || !strings.Contains(body, "RequiredError") {
			t.Fatalf("expected MinLenError + RequiredError in default output, got:\n%s", body)
		}
		// -novalidate: rules elided.
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
		// Default emits AppendStringNoHTML (literal `<`/`>`/`&`); -htmlescape
		// switches to the escaping AppendString.
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

	t.Run("NullZeroFlag_AcceptsNullIntoValueField", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "msg.go"), minimalStruct)
		// Default: a non-pointer scalar has no null branch (strict-reject).
		if out, err := runCLI(t, bin, dir, "msg.go"); err != nil {
			t.Fatalf("ggen default: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(dir, "msg_ggen.go"))
		if strings.Contains(body, `result.Text = ""`) {
			t.Fatalf("did not expect a null→zero branch by default, got:\n%s", body)
		}
		// -nullzero: an explicit null sets the field to its zero value.
		if out, err := runCLI(t, bin, dir, "-nullzero", "msg.go"); err != nil {
			t.Fatalf("ggen -nullzero: %v\n%s", err, out)
		}
		body = mustReadOutput(t, filepath.Join(dir, "msg_ggen.go"))
		if !strings.Contains(body, `result.Text = ""`) {
			t.Fatalf("expected null→zero branch with -nullzero, got:\n%s", body)
		}
		// The per-struct //ggen:generate nullzero annotation has the same effect.
		andir := t.TempDir()
		writeFixture(t, filepath.Join(andir, "msg.go"), `package fixture

//ggen:generate nullzero
type Msg struct {
	Text string `+"`"+`json:"text"`+"`"+`
}
`)
		if out, err := runCLI(t, bin, andir, "msg.go"); err != nil {
			t.Fatalf("ggen annotation: %v\n%s", err, out)
		}
		body = mustReadOutput(t, filepath.Join(andir, "msg_ggen.go"))
		if !strings.Contains(body, `result.Text = ""`) {
			t.Fatalf("expected null→zero branch from //ggen:generate nullzero, got:\n%s", body)
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
		// With a real go.mod, a sibling-package type implementing
		// TextAppender/TextMarshaler/TextUnmarshaler must route through the
		// text path (AppendText/UnmarshalText), not json.Marshal.
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

		// Field-bound match: the call receiver must be exactly result.Tag /
		// s.Tag (two UnmarshalText: DecodeFrom + DecodeFromStream).
		if got := strings.Count(body, "result.Tag.UnmarshalText("); got != 2 {
			t.Errorf("expected 2 result.Tag.UnmarshalText calls (DecodeFrom + DecodeFromStream), got %d:\n%s", got, body)
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

	// taggedExtPkg: a TextMarshaler/Unmarshaler type satisfied purely
	// structurally (no `import "encoding"`). Shared by the third-party and
	// go.work variants below.
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
	// Consumer source; the `tagged` import alias forces type resolution via
	// go/types method-set scan, not by name.
	const taggedConsumerSrc = `package consumer

import tagged "consumertest/ext"

//ggen:generate
type Msg struct {
	Tag tagged.Tagged ` + "`" + `json:"tag"` + "`" + `
}
`

	t.Run("CrossPkgText_RealWorldThirdParty", func(t *testing.T) {
		t.Parallel()
		// Type from another package in the same module, satisfying the text
		// interfaces structurally; detected via types.Implements.
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

		// No AppendText, so encode picks the MarshalText branch.
		if got := strings.Count(body, "result.Tag.UnmarshalText("); got != 2 {
			t.Errorf("expected 2 result.Tag.UnmarshalText calls (DecodeFrom + DecodeFromStream), got %d:\n%s", got, body)
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
		// Consumer and ext as separate modules wired via go.work — proves the
		// loader honors workspace mode.
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
		// Without a go.mod, AST-only mode routes the cross-package field type
		// through the json fallback (degraded-mode contract).
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
		// Field-bound match: the json fallback must land on result.Tag / s.Tag.
		if got := strings.Count(body, "json.Unmarshal(data[start:i], &result.Tag)"); got != 1 {
			t.Errorf("expected 1 json.Unmarshal on &result.Tag in DecodeFrom, got %d:\n%s", got, body)
		}
		if got := strings.Count(body, "json.Unmarshal(span, &result.Tag)"); got != 1 {
			t.Errorf("expected 1 json.Unmarshal on &result.Tag in DecodeFromStream, got %d:\n%s", got, body)
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
		// Same-package `pipe:"@EvenOnly"` resolves to a top-level function;
		// generated code calls EvenOnly(result.N) directly.
		base := t.TempDir()
		dir := filepath.Join(base, "customfunc")
		writeFixture(t, filepath.Join(base, "go.mod"), `module customfunc

go 1.26
`)
		writeFixture(t, filepath.Join(dir, "msg.go"), `package customfunc

import "fmt"

//ggen:generate
type Msg struct {
	N int `+"`"+`json:"n" pipe:"@EvenOnly"`+"`"+`
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
		if !strings.Contains(body, `Name: "EvenOnly", Value: result.N`) {
			t.Errorf("expected CustomError stamped with EvenOnly + Value, got:\n%s", body)
		}
	})

	t.Run("CustomFunc_SamePackage_PureMod", func(t *testing.T) {
		t.Parallel()
		// Pure mod `func(string) string` emits a direct assignment, no
		// error-propagation branch.
		base := t.TempDir()
		dir := filepath.Join(base, "puremod")
		writeFixture(t, filepath.Join(base, "go.mod"), `module puremod

go 1.26
`)
		writeFixture(t, filepath.Join(dir, "msg.go"), `package puremod

import "strings"

//ggen:generate
type Msg struct {
	S string `+"`"+`json:"s" pipe:"@Squash"`+"`"+`
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
		// Fallible mod `func(T) (T, error)`: error-propagation branch,
		// surfacing as a parse error (early return), not validation.
		base := t.TempDir()
		dir := filepath.Join(base, "fallible")
		writeFixture(t, filepath.Join(base, "go.mod"), `module fallible

go 1.26
`)
		writeFixture(t, filepath.Join(dir, "msg.go"), `package fallible

import "fmt"

//ggen:generate
type Msg struct {
	S string `+"`"+`json:"s" pipe:"@Reject"`+"`"+`
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
		if !strings.Contains(body, `return result, i, decode.NewParseErr("s", i, err)`) {
			t.Errorf("expected parse-error return on fallible mod, got:\n%s", body)
		}
	})

	t.Run("CustomFunc_CrossPackage", func(t *testing.T) {
		t.Parallel()
		// Cross-package `@ext.Validate`: ggen runs from inside the consumer
		// dir with `.`, resolving the import via packages.Load up to go.mod.
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
	S string `+"`"+`json:"s" pipe:"@ext.Validate"`+"`"+`
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
		// Dir `lib` declares `package crunchy`, blank-imported so ggen can
		// resolve `@crunchy.Validate`. The resolver must follow the
		// `_`-aliased import and match the tag prefix against the package's
		// declared Name(), not the directory basename.
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
	S string `+"`"+`json:"s" pipe:"@crunchy.Validate"`+"`"+`
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
		// Wrong validator signature must be rejected at parse time.
		base := t.TempDir()
		dir := filepath.Join(base, "badshape")
		writeFixture(t, filepath.Join(base, "go.mod"), `module badshape

go 1.26
`)
		writeFixture(t, filepath.Join(dir, "msg.go"), `package badshape

//ggen:generate
type Msg struct {
	S string `+"`"+`json:"s" pipe:"@WrongShape"`+"`"+`
}

// WrongShape's second result is int — fits none of error/bool, so it is
// neither a valid fallible mod nor anything else.
func WrongShape(s string) (string, int) { return s, 0 }
`)
		out, err := runCLI(t, bin, base, "./badshape")
		if err == nil {
			t.Fatalf("expected ggen to reject signature mismatch, got success:\n%s", out)
		}
		if !strings.Contains(out, "second result must be error or bool") {
			t.Errorf("expected diagnostic about fallible second result, got:\n%s", out)
		}
	})

	t.Run("CustomFunc_ParamMismatch_Error", func(t *testing.T) {
		t.Parallel()
		// Param-type mismatch (field int, func takes string) must be rejected
		// at parse time — the types.Identical check in resolveCustomFunc.
		base := t.TempDir()
		dir := filepath.Join(base, "paramshape")
		writeFixture(t, filepath.Join(base, "go.mod"), `module paramshape

go 1.26
`)
		writeFixture(t, filepath.Join(dir, "msg.go"), `package paramshape

//ggen:generate
type Msg struct {
	N int `+"`"+`json:"n" pipe:"@WrongParam"`+"`"+`
}

// WrongParam takes string instead of int — param mismatch.
func WrongParam(s string) error { return nil }
`)
		out, err := runCLI(t, bin, base, "./paramshape")
		if err == nil {
			t.Fatalf("expected ggen to reject param mismatch, got success:\n%s", out)
		}
		if !strings.Contains(out, "param type string does not match value type int") {
			t.Errorf("expected diagnostic about param type, got:\n%s", out)
		}
	})

	// --- top-level alias types ---

	t.Run("Alias_Primitive_Generates", func(t *testing.T) {
		t.Parallel()
		// A primitive alias gets DecodeFrom / AppendJSON on the named type.
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
		if !strings.Contains(body, "(recv HtmlString) DecodeFrom") {
			t.Errorf("HtmlString.DecodeFrom missing:\n%s", body)
		}
		if !strings.Contains(body, "result = HtmlString(v)") {
			t.Errorf("HtmlString cast missing:\n%s", body)
		}
		if !strings.Contains(body, "(recv Count) DecodeFrom") {
			t.Errorf("Count.DecodeFrom missing:\n%s", body)
		}
		if !strings.Contains(body, "strconv.AppendInt(dst, int64(s), 10)") {
			t.Errorf("Count AppendJSON missing strconv call:\n%s", body)
		}
	})

	t.Run("Alias_Container_Generates", func(t *testing.T) {
		t.Parallel()
		// Slice / map / array aliases generate through the AST-only fallback.
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
			") DecodeFrom(data []byte) (result Tags",
			"s Tags) AppendJSON",
			") DecodeFrom(data []byte) (result Lookup",
			"s Lookup) AppendJSON",
			") DecodeFrom(data []byte) (result Tuple",
			"s Tuple) AppendJSON",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("expected %q in generated body:\n%s", want, body)
			}
		}
	})

	t.Run("Alias_RejectsUnsupported", func(t *testing.T) {
		t.Parallel()
		// Interfaces, channels, funcs have no JSON wire shape — reject at
		// parse time.
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
		// `@Missing` references a nonexistent function — reject at parse time.
		base := t.TempDir()
		dir := filepath.Join(base, "missing")
		writeFixture(t, filepath.Join(base, "go.mod"), `module missing

go 1.26
`)
		writeFixture(t, filepath.Join(dir, "msg.go"), `package missing

//ggen:generate
type Msg struct {
	S string `+"`"+`json:"s" pipe:"@Missing"`+"`"+`
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

	// --- output formatting (always run through go/format.Source) ---

	t.Run("DefaultOutputIsGofmtClean", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "msg.go"), minimalStruct)
		if out, err := runCLI(t, bin, dir, "msg.go"); err != nil {
			t.Fatalf("ggen msg.go: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(dir, "msg_ggen.go"))
		// gofmt-clean signal: `\n\tvar err error\n` is one-tab indented; an
		// unformatted renderer would leave it flush-left.
		if !strings.Contains(body, "\n\tvar err error\n") {
			t.Errorf("default output not gofmt-clean (var err error not indented):\n%s", body)
		}
	})

	// --- rule applicability / value-shape rejection ---
	//
	// Kind-coupled rules and bad value parameters (`len=abc`, `gt=abc`, …)
	// must be rejected at parse time with a clear diagnostic.
	t.Run("FieldCollisions", func(t *testing.T) {
		t.Parallel()
		// A parent field shadowing an embedded one resolves stdlib-style
		// (own wins) — it used to emit duplicate seen-flags and switch
		// cases, a generated file that doesn't compile.
		t.Run("embedded_shadow_resolves", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeFixture(t, filepath.Join(dir, "msg.go"), `package fixture

type Base struct {
	ID   int    `+"`"+`json:"id"`+"`"+`
	Note string `+"`"+`json:"note"`+"`"+`
}

//ggen:generate
type Outer struct {
	Base
	ID int `+"`"+`json:"id"`+"`"+`
}
`)
			out, err := runCLI(t, bin, dir, "msg.go")
			if err != nil {
				t.Fatalf("ggen failed: %v\n%s", err, out)
			}
			gen, rerr := os.ReadFile(filepath.Join(dir, "msg_ggen.go"))
			if rerr != nil {
				t.Fatal(rerr)
			}
			if n := strings.Count(string(gen), "seenID := false"); n != 2 { // bytes + stream
				t.Errorf("seenID declared %d times, want 2 (one per decode path)\n", n)
			}
			if !strings.Contains(string(gen), "case \"note\":") {
				t.Errorf("promoted non-shadowed field lost")
			}
		})
		// Two of the parent's OWN fields sharing a JSON name is a hard error.
		t.Run("own_duplicate_tags_rejected", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeFixture(t, filepath.Join(dir, "msg.go"), `package fixture

//ggen:generate
type Msg struct {
	A int `+"`"+`json:"same"`+"`"+`
	B int `+"`"+`json:"same"`+"`"+`
}
`)
			out, err := runCLI(t, bin, dir, "msg.go")
			if err == nil {
				t.Fatalf("expected rejection, got success:\n%s", out)
			}
			if !strings.Contains(out, "share JSON name") {
				t.Errorf("diagnostic missing:\n%s", out)
			}
		})
	})

	t.Run("TypedFixtureRejections", func(t *testing.T) {
		t.Parallel()
		// These need go/types (a real module): converter input classification
		// and named-primitive rule applicability.
		writeMod := func(t *testing.T, dir string) {
			writeFixture(t, filepath.Join(dir, "go.mod"), "module fixture\n\ngo 1.26\n")
		}
		t.Run("converter_container_input", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeMod(t, dir)
			writeFixture(t, filepath.Join(dir, "msg.go"), `package fixture

func FromList(v []string) string { return "" }

//ggen:generate
type Msg struct {
	S string `+"`"+`json:"s" pipe:"@FromList/."`+"`"+`
}
`)
			out, err := runCLI(t, bin, dir, "msg.go")
			if err == nil {
				t.Fatalf("expected rejection, got success:\n%s", out)
			}
			if !strings.Contains(out, "container inputs are not supported") {
				t.Errorf("diagnostic missing:\n%s", out)
			}
		})
		t.Run("named_prim_rule_mismatch", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeMod(t, dir)
			writeFixture(t, filepath.Join(dir, "msg.go"), `package fixture

type Pri string

//ggen:generate
type Msg struct {
	P Pri `+"`"+`json:"p" pipe:"gt=1"`+"`"+`
}
`)
			out, err := runCLI(t, bin, dir, "msg.go")
			if err == nil {
				t.Fatalf("expected rejection, got success:\n%s", out)
			}
			if !strings.Contains(out, "inapplicable to Pri") {
				t.Errorf("diagnostic missing:\n%s", out)
			}
		})
	})

	t.Run("InvalidRuleApplication", func(t *testing.T) {
		t.Parallel()
		type bad struct {
			name     string // subtest name
			fieldGo  string // Go field declaration (e.g. `N int`)
			tag      string // full struct-tag content (everything between backticks)
			wantDiag string // substring expected somewhere in stderr+stdout
		}
		// Each entry runs ggen on a single-file fixture; the CLI must exit
		// non-zero and the diagnostic must contain wantDiag.
		cases := []bad{
			// ----- json tag grammar (jsonv2 parity) -----
			{"dash_with_options", "F string", `json:"-,"`, `use json:"-" to ignore the field`},
			{"trailing_comma_tag", "F string", `json:"f,"`, "empty option"},
			{"empty_option_tag", "F string", `json:"f,,omitempty"`, "empty option"},

			// ----- removed string validators are now unknown rules -----
			{"ascii_unknown", "S string", `json:"s" pipe:"ascii"`, "`ascii` is not a known validation rule"},
			{"email_unknown", "S string", `json:"s" pipe:"email"`, "`email` is not a known validation rule"},
			{"printable_unknown", "S string", `json:"s" pipe:"printable"`, "`printable` is not a known validation rule"},

			// ----- string-only rules on non-strings -----
			{"url_on_bool", "B bool", `json:"b" pipe:"url"`, "`url` is inapplicable to bool"},
			{"alphanum_on_int", "N int", `json:"n" pipe:"alphanum"`, "`alphanum` is inapplicable to int"},
			{"numericrule_on_int", "N int", `json:"n" pipe:"numeric"`, "`numeric` is inapplicable to int"},
			{"lower_on_int", "N int", `json:"n" pipe:"lower"`, "`lower` is inapplicable to int"},
			{"upper_on_int", "N int", `json:"n" pipe:"upper"`, "`upper` is inapplicable to int"},
			{"hexadecimal_on_int", "N int", `json:"n" pipe:"hexadecimal"`, "`hexadecimal` is inapplicable to int"},
			{"starts_on_int", "N int", `json:"n" pipe:"starts=foo"`, "`starts` is inapplicable to int"},
			{"ends_on_int", "N int", `json:"n" pipe:"ends=foo"`, "`ends` is inapplicable to int"},
			{"contains_on_int", "N int", `json:"n" pipe:"contains=foo"`, "`contains` is inapplicable to int"},
			{"runes_on_int", "N int", `json:"n" pipe:"runes=3"`, "`runes` is inapplicable to int"},
			{"minrunes_on_int", "N int", `json:"n" pipe:"minrunes=3"`, "`minrunes` is inapplicable to int"},
			{"maxrunes_on_int", "N int", `json:"n" pipe:"maxrunes=3"`, "`maxrunes` is inapplicable to int"},
			{"runes_on_bytes", "B []byte", `json:"b" pipe:"runes=3"`, "`runes` is inapplicable to []byte"},
			{"starts_empty", "S string", `json:"s" pipe:"starts="`, `requires a non-empty value`},
			{"ends_empty", "S string", `json:"s" pipe:"ends="`, `requires a non-empty value`},
			{"contains_empty", "S string", `json:"s" pipe:"contains="`, `requires a non-empty value`},

			// ----- numeric-only rules on non-numerics -----
			{"gt_on_string", "S string", `json:"s" pipe:"gt=1"`, "`gt` is inapplicable to string"},
			{"gte_on_string", "S string", `json:"s" pipe:"gte=1"`, "`gte` is inapplicable to string"},
			{"lt_on_string", "S string", `json:"s" pipe:"lt=1"`, "`lt` is inapplicable to string"},
			{"lte_on_string", "S string", `json:"s" pipe:"lte=1"`, "`lte` is inapplicable to string"},
			{"gt_on_bool", "B bool", `json:"b" pipe:"gt=0"`, "`gt` is inapplicable to bool"},
			{"gt_on_slice", "X []int", `json:"x" pipe:"gt=0"`, "`gt` is inapplicable to []int"},
			{"gt_bad_value", "N int", `json:"n" pipe:"gt=abc"`, `value is not a valid number`},
			{"gte_bad_value", "N int", `json:"n" pipe:"gte=abc"`, `value is not a valid number`},
			{"lt_bad_value", "N int", `json:"n" pipe:"lt=abc"`, `value is not a valid number`},
			{"lte_bad_value", "N int", `json:"n" pipe:"lte=abc"`, `value is not a valid number`},
			{"gt_missing_value", "N int", `json:"n" pipe:"gt"`, `requires a numeric value`},

			// ----- multiple: integers only -----
			{"multiple_on_string", "S string", `json:"s" pipe:"multiple=2"`, "`multiple` is inapplicable to string"},
			{"multiple_on_float", "F float64", `json:"f" pipe:"multiple=2"`, "`multiple` is inapplicable to float64"},
			{"multiple_on_bool", "B bool", `json:"b" pipe:"multiple=2"`, "`multiple` is inapplicable to bool"},
			{"multiple_bad_value", "N int", `json:"n" pipe:"multiple=abc"`, `value is not a valid integer`},
			{"multiple_missing", "N int", `json:"n" pipe:"multiple"`, `requires an integer value`},

			// ----- len/minlen/maxlen/notempty on non-len-able kinds -----
			{"len_on_int", "N int", `json:"n" pipe:"len=5"`, "`len` is inapplicable to int"},
			{"len_on_bool", "B bool", `json:"b" pipe:"len=1"`, "`len` is inapplicable to bool"},
			{"len_on_float", "F float64", `json:"f" pipe:"len=1"`, "`len` is inapplicable to float64"},
			{"minlen_on_int", "N int", `json:"n" pipe:"minlen=1"`, "`minlen` is inapplicable to int"},
			{"maxlen_on_int", "N int", `json:"n" pipe:"maxlen=1"`, "`maxlen` is inapplicable to int"},
			{"notempty_on_int", "N int", `json:"n" pipe:"notempty"`, "`notempty` is inapplicable to int"},
			{"notempty_on_bool", "B bool", `json:"b" pipe:"notempty"`, "`notempty` is inapplicable to bool"},
			{"len_bad_value", "S string", `json:"s" pipe:"len=abc"`, `value is not a valid integer`},
			{"minlen_bad_value", "S string", `json:"s" pipe:"minlen=abc"`, `value is not a valid integer`},
			{"maxlen_bad_value", "S string", `json:"s" pipe:"maxlen=abc"`, `value is not a valid integer`},
			{"len_missing_value", "S string", `json:"s" pipe:"len"`, `requires an integer value`},
			{"len_float_value", "S string", `json:"s" pipe:"len=1.5"`, `value is not a valid integer`},
			{"runes_bad_value", "S string", `json:"s" pipe:"runes=abc"`, `value is not a valid integer`},
			{"minrunes_bad_value", "S string", `json:"s" pipe:"minrunes=abc"`, `value is not a valid integer`},

			// ----- eq / neq scope: not bool, not slice, not float-with-text -----
			{"eq_on_slice", "X []int", `json:"x" pipe:"eq=1"`, "`eq` is inapplicable to []int"},
			{"neq_on_slice", "X []int", `json:"x" pipe:"neq=1"`, "`neq` is inapplicable to []int"},
			{"eq_on_bool", "B bool", `json:"b" pipe:"eq=true"`, "`eq` is inapplicable to bool"},
			{"neq_on_bool", "B bool", `json:"b" pipe:"neq=true"`, "`neq` is inapplicable to bool"},
			{"eq_int_bad_value", "N int", `json:"n" pipe:"eq=abc"`, `value is not a valid number`},
			{"neq_int_bad_value", "N int", `json:"n" pipe:"neq=abc"`, `value is not a valid number`},

			// ----- oneof: must be string/numeric, non-empty, numeric parts numeric -----
			{"oneof_on_bool", "B bool", `json:"b" pipe:"oneof=true|false"`, "`oneof` is inapplicable to bool"},
			{"oneof_on_slice", "X []int", `json:"x" pipe:"oneof=1|2"`, "`oneof` is inapplicable to []int"},
			{"oneof_empty", "S string", `json:"s" pipe:"oneof"`, `oneof` + "` requires a "}, // matches "rule `oneof` requires a `|`-separated…"
			{"oneof_numeric_bad_part", "N int", `json:"n" pipe:"oneof=1|two|3"`, `part "two" is not a valid number`},

			// ----- const-expr array length needs type info (AST-only mode) -----
			{"const_array_len", "A [size]int", `json:"a"`, "fixed-array length must be an integer literal"},

			// ----- hint restricted to slice/map -----
			{"hint_on_int", "N int", `json:"n" hint:"10"`, "`hint` is only valid on slice/map fields"},
			{"hint_on_string", "S string", `json:"s" hint:"10"`, "`hint` is only valid on slice/map fields"},
			{"hint_on_bool", "B bool", `json:"b" hint:"10"`, "`hint` is only valid on slice/map fields"},
			{"hint_on_array", "X [3]int", `json:"x" hint:"10"`, "`hint` is only valid on slice/map fields"},
			{"hint_zero_on_int", "N int", `json:"n" hint:"0"`, "`hint` is only valid on slice/map fields"},
			{"hint_negative", "X []int", `json:"x" hint:"-1"`, "must be ≥ 0"},
			{"hint_non_numeric", "X []int", `json:"x" hint:"abc"`, `is not a valid integer`},

			// ----- inner: only on slice/array/map -----
			{"dive_on_string", "S string", `json:"s" pipe:"inner:minlen=1"`, "`inner:` tag prefix is only valid on slice/array/map fields"},
			{"dive_on_int", "N int", `json:"n" pipe:"inner:gte=0"`, "`inner:` tag prefix is only valid on slice/array/map fields"},
			{"dive_mod_on_int", "N int", `json:"n" pipe:"inner:trim"`, "`inner:` tag prefix is only valid on slice/array/map fields"},

			// ----- keys: only on maps -----
			{"keys_on_string", "S string", `json:"s" pipe:"keys:minlen=1"`, "`keys:` tag prefix is only valid on map[string]V fields"},
			{"keys_on_slice", "X []int", `json:"x" pipe:"keys:minlen=1"`, "`keys:` tag prefix is only valid on map[string]V fields"},
			{"keys_mod_on_string", "S string", `json:"s" pipe:"keys:trim"`, "`keys:` tag prefix is only valid on map[string]V fields"},
			// keys: rules themselves are typed against KindString — verify
			// a non-string-applicable rule still gets rejected even though
			// the parent IS a map.
			{"keys_numeric_rule", "M map[string]int", `json:"m" pipe:"keys:gt=1"`, "`gt` is inapplicable to string"},

			// ----- inner: element kind mismatch -----
			// []int element is int — string rules invalid on the element.
			{"dive_alphanum_on_int_elem", "X []int", `json:"x" pipe:"inner:alphanum"`, "`alphanum` is inapplicable to int"},
			{"dive_numeric_on_int_elem", "X []int", `json:"x" pipe:"inner:numeric"`, "`numeric` is inapplicable to int"},
			{"dive_len_on_int_elem", "X []int", `json:"x" pipe:"inner:len=3"`, "`len` is inapplicable to int"},
			// map[string]int value is int.
			{"dive_alphanum_on_int_mapval", "M map[string]int", `json:"m" pipe:"inner:alphanum"`, "`alphanum` is inapplicable to int"},
			// []string element is string — numeric rules invalid.
			{"dive_gt_on_string_elem", "X []string", `json:"x" pipe:"inner:gt=1"`, "`gt` is inapplicable to string"},

			// ----- mods: string mods on numerics, numeric mods on strings -----
			{"trim_on_int", "N int", `json:"n" pipe:"trim"`, "`trim` is inapplicable to int"},
			{"lower_mod_on_int", "N int", `json:"n" pipe:"lower"`, "`lower` is inapplicable to int"},
			{"upper_mod_on_int", "N int", `json:"n" pipe:"upper"`, "`upper` is inapplicable to int"},
			{"trimleft_on_int", "N int", `json:"n" pipe:"trimleft=foo"`, "`trimleft` is inapplicable to int"},
			{"trimright_on_int", "N int", `json:"n" pipe:"trimright=foo"`, "`trimright` is inapplicable to int"},
			{"replace_on_int", "N int", `json:"n" pipe:"replace=a|b"`, "`replace` is inapplicable to int"},
			{"clamp_on_string", "S string", `json:"s" pipe:"clamp=0|10"`, "`clamp` is inapplicable to string"},
			{"clamp_on_bool", "B bool", `json:"b" pipe:"clamp=0|10"`, "`clamp` is inapplicable to bool"},
			{"clamp_on_slice", "X []int", `json:"x" pipe:"clamp=0|10"`, "`clamp` is inapplicable to []int"},

			// ----- mods: parameter-shape rejection -----
			{"trimleft_empty", "S string", `json:"s" pipe:"trimleft="`, "`trimleft` requires a non-empty value"},
			{"trimright_empty", "S string", `json:"s" pipe:"trimright="`, "`trimright` requires a non-empty value"},
			{"replace_missing_pipe", "S string", `json:"s" pipe:"replace=foo"`, "requires `old|new` form"},
			{"replace_empty_old", "S string", `json:"s" pipe:"replace=|new"`, "requires `old|new` form"},
			{"clamp_missing_pipe", "N int", `json:"n" pipe:"clamp=10"`, "needs exactly one lo`|`hi separator"},
			{"clamp_both_empty", "N int", `json:"n" pipe:"clamp=|"`, "requires at least one of lo or hi"},
			{"clamp_bad_lo", "N int", `json:"n" pipe:"clamp=abc|10"`, "lo \"abc\" is not a valid number"},
			{"clamp_bad_hi", "N int", `json:"n" pipe:"clamp=0|abc"`, "hi \"abc\" is not a valid number"},

			// ----- emitted-code-would-not-compile class (round-2 audit) -----
			{"multiple_zero", "N int", `json:"n" pipe:"multiple=0"`, "requires a positive integer"},
			{"multiple_negative", "N int", `json:"n" pipe:"multiple=-2"`, "requires a positive integer"},
			{"oneof_dup_string", "S string", `json:"s" pipe:"oneof=a|b|a"`, "is a duplicate"},
			{"oneof_dup_numeric_value", "N int", `json:"n" pipe:"oneof=1|1.0"`, "is a duplicate"},
			{"gt_fractional_on_int", "N int", `json:"n" pipe:"gt=1.5"`, "integer field needs an integer bound"},
			{"gt_inf", "F float64", `json:"f" pipe:"gt=Inf"`, "NaN/Inf are not valid bounds"},
			{"eq_nan", "F float64", `json:"f" pipe:"eq=NaN"`, "NaN/Inf are not valid bounds"},
			{"clamp_fractional_on_int", "N int", `json:"n" pipe:"clamp=0|1.5"`, "integer field needs integer bounds"},
			{"string_tag_on_slice", "X []int", `json:"x,string"`, "only valid on primitive fields"},
			{"string_tag_on_map", "M map[string]int", `json:"m,string"`, "only valid on primitive fields"},
			{"inner_depth2_mismatch", "M [][]int", `json:"m" pipe:"inner:(inner:(trim))"`, "is inapplicable to int"},
			{"inner_deeper_than_type", "X []int", `json:"x" pipe:"inner:(inner:(gte=0))"`, "no element at that depth"},
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
