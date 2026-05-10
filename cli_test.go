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
		mustHaveFile(t, filepath.Join(dir, "msg_gen.go"))
		mustNotHaveFile(t, filepath.Join(dir, "msg_gen_test.go"))
	})

	t.Run("SingleFile_Test_OutputName", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "msg_test.go"), minimalStruct)
		if out, err := runCLI(t, bin, dir, "msg_test.go"); err != nil {
			t.Fatalf("ggen msg_test.go: %v\n%s", err, out)
		}
		mustHaveFile(t, filepath.Join(dir, "msg_gen_test.go"))
		mustNotHaveFile(t, filepath.Join(dir, "msg_gen.go"))
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
		mustNotHaveFile(t, filepath.Join(dir, "msg_gen.go"))
	})

	t.Run("Directory_NonTest_OutputName", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()
		dir := filepath.Join(base, "fixture")
		writeFixture(t, filepath.Join(dir, "msg.go"), minimalStruct)
		if out, err := runCLI(t, bin, dir, "."); err != nil {
			t.Fatalf("ggen .: %v\n%s", err, out)
		}
		mustHaveFile(t, filepath.Join(dir, "fixture_gen.go"))
		mustNotHaveFile(t, filepath.Join(dir, "fixture_gen_test.go"))
	})

	t.Run("Directory_Test_OutputName", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()
		dir := filepath.Join(base, "fixture")
		writeFixture(t, filepath.Join(dir, "msg_test.go"), minimalStruct)
		if out, err := runCLI(t, bin, dir, "."); err != nil {
			t.Fatalf("ggen .: %v\n%s", err, out)
		}
		mustHaveFile(t, filepath.Join(dir, "fixture_gen_test.go"))
		mustNotHaveFile(t, filepath.Join(dir, "fixture_gen.go"))
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
		mustHaveFile(t, filepath.Join(dir, "fixture_gen.go"))
		mustHaveFile(t, filepath.Join(dir, "fixture_gen_test.go"))

		// Content routing: Msg (declared in msg.go) belongs to the library
		// build; MsgT (declared in msg_test.go) belongs to the test build.
		// They must land in their respective _gen files and not bleed across
		// — otherwise a `go test` build would pull duplicate methods or
		// `go build` would import test-only types.
		nonTest := mustReadOutput(t, filepath.Join(dir, "fixture_gen.go"))
		test := mustReadOutput(t, filepath.Join(dir, "fixture_gen_test.go"))
		if !strings.Contains(nonTest, "Msg) DecodeFrom") {
			t.Errorf("Msg missing from fixture_gen.go:\n%s", nonTest)
		}
		if strings.Contains(nonTest, "MsgT) DecodeFrom") {
			t.Errorf("MsgT leaked into fixture_gen.go:\n%s", nonTest)
		}
		if !strings.Contains(test, "MsgT) DecodeFrom") {
			t.Errorf("MsgT missing from fixture_gen_test.go:\n%s", test)
		}
		if strings.Contains(test, "Msg) DecodeFrom") {
			t.Errorf("Msg leaked into fixture_gen_test.go:\n%s", test)
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
		mustNotHaveFile(t, filepath.Join(dir, "fixture_gen.go"))
		mustNotHaveFile(t, filepath.Join(dir, "fixture_gen_test.go"))
	})

	t.Run("Directory_BuildTag_BucketsIntoSeparateFiles", func(t *testing.T) {
		t.Parallel()
		// Two files in the same package: one untagged, one behind
		// `//go:build foo`. The tagged struct must NOT pollute the
		// untagged gen file (would compile-break builds without `foo`),
		// so the generator emits a separate `<dir>_foo_gen.go` carrying
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

		// Untagged bucket → fixture_gen.go, Plain inside, no //go:build header.
		plain := filepath.Join(dir, "fixture_gen.go")
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

		// Tagged bucket → fixture_foo_gen.go, Tagged inside, with the header.
		tagged := filepath.Join(dir, "fixture_foo_gen.go")
		mustHaveFile(t, tagged)
		taggedBody := mustReadOutput(t, tagged)
		if !strings.HasPrefix(taggedBody, "//go:build foo\n") {
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
		path := filepath.Join(dir, "fixture_foo_bar_gen.go")
		mustHaveFile(t, path)
		body := mustReadOutput(t, path)
		if !strings.HasPrefix(body, "//go:build foo && bar\n") {
			t.Errorf("expected literal '//go:build foo && bar' header, got:\n%s", body)
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
		mustHaveFile(t, filepath.Join(a, "a_gen.go"))
		mustHaveFile(t, filepath.Join(b, "b_gen.go"))
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
		mustHaveFile(t, filepath.Join(visible, "visible_gen.go"))
		mustNotHaveFile(t, filepath.Join(hidden, ".hidden_gen.go"))
		mustNotHaveFile(t, filepath.Join(skipped, "_skipped_gen.go"))
	})

	// --- flag plumbing into generated output ---

	t.Run("PkgOverride", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "msg.go"), minimalStruct)
		if out, err := runCLI(t, bin, dir, "-pkg", "renamed", "msg.go"); err != nil {
			t.Fatalf("ggen -pkg: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(dir, "msg_gen.go"))
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
		body := mustReadOutput(t, filepath.Join(dir, "msg_gen.go"))
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
		body := mustReadOutput(t, filepath.Join(dir, "msg_gen.go"))
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
		body := mustReadOutput(t, filepath.Join(dir, "msg_gen.go"))
		if !strings.Contains(body, "MinLenError") || !strings.Contains(body, "RequiredError") {
			t.Fatalf("expected MinLenError + RequiredError in default output, got:\n%s", body)
		}
		// -novalidate: rules elided; no validation.* error symbols anywhere.
		if out, err := runCLI(t, bin, dir, "-novalidate", "msg.go"); err != nil {
			t.Fatalf("ggen -novalidate: %v\n%s", err, out)
		}
		body = mustReadOutput(t, filepath.Join(dir, "msg_gen.go"))
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
		body := mustReadOutput(t, filepath.Join(dir, "msg_gen.go"))
		if !strings.Contains(body, "AppendStringNoHTML") {
			t.Fatalf("expected AppendStringNoHTML by default, got:\n%s", body)
		}

		// -htmlescape → escaping appender (and not the no-html one).
		if out, err := runCLI(t, bin, dir, "-htmlescape", "msg.go"); err != nil {
			t.Fatalf("ggen -htmlescape: %v\n%s", err, out)
		}
		body = mustReadOutput(t, filepath.Join(dir, "msg_gen.go"))
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
		body := mustReadOutput(t, filepath.Join(dir, "msg_gen.go"))
		if !strings.Contains(body, "UnknownKeyError") {
			t.Fatalf("expected UnknownKeyError in default output, got:\n%s", body)
		}
		// -ignoreunknown: error site silently drops unknowns.
		if out, err := runCLI(t, bin, dir, "-ignoreunknown", "msg.go"); err != nil {
			t.Fatalf("ggen -ignoreunknown: %v\n%s", err, out)
		}
		body = mustReadOutput(t, filepath.Join(dir, "msg_gen.go"))
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
		body := mustReadOutput(t, filepath.Join(dir, "msg_gen.go"))
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
		body := mustReadOutput(t, filepath.Join(base, "consumer", "consumer_gen.go"))

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

	t.Run("CrossPkgText_RealWorldThirdParty", func(t *testing.T) {
		t.Parallel()
		// Real-world flow: a temp module pulls in github.com/sirkostya009/ggen
		// as a "third-party library" via a replace directive and references
		// thirdparty.Tagged. That type defines MarshalText / UnmarshalText
		// on its method set with NO `import "encoding"`, NO `var _
		// encoding.TextMarshaler = ...` assertion — pure structural
		// satisfaction. The generator must detect it via types.Implements
		// against std interfaces it loads on its own, with no help from the
		// producer's import graph.
		schemaRoot, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		base := t.TempDir()
		writeFixture(t, filepath.Join(base, "go.mod"), `module consumertest

go 1.26

require github.com/sirkostya009/ggen v0.0.0-00010101000000-000000000000

replace github.com/sirkostya009/ggen => `+schemaRoot+`
`)
		writeFixture(t, filepath.Join(base, "consumer", "msg.go"), `package consumer

import "github.com/sirkostya009/ggen/thirdparty"

//ggen:generate
type Msg struct {
	Tag thirdparty.Tagged `+"`"+`json:"tag"`+"`"+`
}
`)
		if out, err := runCLI(t, bin, base, "./consumer"); err != nil {
			t.Fatalf("ggen ./consumer: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(base, "consumer", "consumer_gen.go"))

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
		// go.work flow: the consumer module and the schema repo are both
		// workspace members. The workspace overrides the require/replace
		// resolution chain — `import "github.com/sirkostya009/ggen/..."`
		// from the consumer resolves directly to the local schema repo via
		// the use directive. Same structural-detection assertions as the
		// replace-directive variant; this proves the generator's loader
		// honors workspace mode (packages.Load picks up GOFLAGS / go.work
		// the same way `go build` does).
		schemaRoot, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		base := t.TempDir()
		consumerDir := filepath.Join(base, "consumer")
		writeFixture(t, filepath.Join(base, "go.work"), `go 1.26

use (
	./consumer
	`+schemaRoot+`
)
`)
		writeFixture(t, filepath.Join(consumerDir, "go.mod"), `module consumertest

go 1.26
`)
		writeFixture(t, filepath.Join(consumerDir, "msg.go"), `package consumer

import "github.com/sirkostya009/ggen/thirdparty"

//ggen:generate
type Msg struct {
	Tag thirdparty.Tagged `+"`"+`json:"tag"`+"`"+`
}
`)
		if out, err := runCLI(t, bin, base, "./consumer"); err != nil {
			t.Fatalf("ggen ./consumer: %v\n%s", err, out)
		}
		body := mustReadOutput(t, filepath.Join(consumerDir, "consumer_gen.go"))

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
		body := mustReadOutput(t, filepath.Join(consumerDir, "consumer_gen.go"))
		// Without type info, the generator can't resolve interfaces — every
		// generated method falls back to encoding/json against result.Tag /
		// s.Tag specifically. Field-bound match confirms the json fallback
		// landed at the correct call site and not some adjacent emission.
		if got := strings.Count(body, "json.Unmarshal(data[_start:_k], &result.Tag)"); got != 1 {
			t.Errorf("expected 1 json.Unmarshal on &result.Tag in DecodeFrom, got %d:\n%s", got, body)
		}
		if got := strings.Count(body, "json.Unmarshal(_s.Bytes()[_start:_k], &result.Tag)"); got != 1 {
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
		body := mustReadOutput(t, filepath.Join(dir, "customfunc_gen.go"))
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
		body := mustReadOutput(t, filepath.Join(dir, "puremod_gen.go"))
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
		body := mustReadOutput(t, filepath.Join(dir, "fallible_gen.go"))
		if !strings.Contains(body, "if _v, _err := Reject(result.S); _err != nil") {
			t.Errorf("expected fallible mod with err-prop branch, got:\n%s", body)
		}
		if !strings.Contains(body, "return result, 0, _err") {
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
		body := mustReadOutput(t, filepath.Join(consumerDir, "consumer_gen.go"))
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
		body := mustReadOutput(t, filepath.Join(consumerDir, "consumer_gen.go"))
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
		body := mustReadOutput(t, filepath.Join(dir, "msg_gen.go"))
		if !strings.Contains(body, "(HtmlString) DecodeFrom") {
			t.Errorf("HtmlString.DecodeFrom missing:\n%s", body)
		}
		if !strings.Contains(body, "result = HtmlString(v)") {
			t.Errorf("HtmlString cast missing:\n%s", body)
		}
		if !strings.Contains(body, "(Count) DecodeFrom") {
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
		body := mustReadOutput(t, filepath.Join(dir, "msg_gen.go"))
		for _, want := range []string{
			") DecodeFrom(data []byte, i int) (Tags",
			"s Tags) AppendJSON",
			") DecodeFrom(data []byte, i int) (Lookup",
			"s Lookup) AppendJSON",
			") DecodeFrom(data []byte, i int) (Tuple",
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
}
