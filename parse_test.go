package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// generate is a test-only convenience over generateTo: it materializes
// the output into a returned []byte. Production code paths in main.go
// (generateDir / generateSingleFile) call generateTo directly against
// the destination *os.File so there is no intermediate copy. Tests and
// benchmarks need the bytes in memory to assert on or roundtrip, so the
// helper lives here and stays out of release binaries.
func generate(pkg string, structs []StructInfo) ([]byte, error) {
	var buf bytes.Buffer
	if err := generateTo(&buf, pkg, structs); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeGoFile(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "test.go")
	if err := os.WriteFile(file, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestParseFile(t *testing.T) {
	src := `package test

type Foo struct {
	Name    string  ` + "`" + `json:"name" ggen:"required,minlen=1"` + "`" + `
	Age     int     ` + "`" + `json:"age" ggen:"gte=0,lte=150"` + "`" + `
	Ignored string  ` + "`" + `json:"-"` + "`" + `
	Items   []string ` + "`" + `json:"items"` + "`" + `
}
`
	file := writeGoFile(t, src)

	structs, pkg, err := parseFile(file, []string{"Foo"})
	if err != nil {
		t.Fatal(err)
	}
	if pkg != "test" {
		t.Errorf("pkg = %q, want test", pkg)
	}
	if len(structs) != 1 {
		t.Fatalf("got %d structs, want 1", len(structs))
	}

	s := structs[0]
	if s.Name != "Foo" {
		t.Errorf("Name = %q, want Foo", s.Name)
	}
	if len(s.Fields) != 3 {
		t.Fatalf("got %d fields, want 3", len(s.Fields))
	}
	if !s.Fields[0].IsRequired() {
		t.Error("Name should be required")
	}
	if s.Fields[2].ElemType != "string" || s.Fields[2].ElemKind != KindString {
		t.Errorf("Items elem = %+v", s.Fields[2])
	}
}

func TestParseFile_autoDiscoverSubStructs(t *testing.T) {
	src := `package test

type Parent struct {
	Child Child ` + "`" + `json:"child"` + "`" + `
	Kids  []Kid ` + "`" + `json:"kids"` + "`" + `
}

type Child struct {
	Name string ` + "`" + `json:"name"` + "`" + `
}

type Kid struct {
	Age int ` + "`" + `json:"age"` + "`" + `
}

type Unrelated struct {
	X int
}
`
	file := writeGoFile(t, src)

	structs, _, err := parseFile(file, []string{"Parent"})
	if err != nil {
		t.Fatal(err)
	}

	names := make(map[string]struct{}, len(structs))
	for _, s := range structs {
		names[s.Name] = struct{}{}
	}

	if _, ok := names["Parent"]; !ok {
		t.Error("Parent not generated")
	}
	if _, ok := names["Child"]; !ok {
		t.Error("Child (field type) not auto-discovered")
	}
	if _, ok := names["Kid"]; !ok {
		t.Error("Kid (slice elem type) not auto-discovered")
	}
	if _, ok := names["Unrelated"]; ok {
		t.Error("Unrelated should not be generated")
	}
}

// parseFile in single-file mode requires either a //ggen:generate
// annotation OR an explicit struct-name filter. Calling with neither
// must surface a helpful diagnostic instead of silently emitting code
// for every exported struct (the old behaviour was a footgun: scratch
// files with a stale ggen: tag would unintentionally trip the
// applicability check on an unrelated struct).
func TestParseFile_noAnnotationNoFilter_Errors(t *testing.T) {
	src := `package test
type A struct { X int ` + "`" + `json:"x"` + "`" + ` }
type B struct { Y int ` + "`" + `json:"y"` + "`" + ` }
`
	file := writeGoFile(t, src)
	_, _, err := parseFile(file, nil)
	if err == nil {
		t.Fatal("expected error when no annotation and no name filter")
	}
	for _, want := range []string{"//ggen:generate", filepath.Base(file)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing substring %q", err.Error(), want)
		}
	}
}

// Explicit name filter still works without any //ggen:generate
// annotation — opt-in via positional args remains the escape hatch.
func TestParseFile_explicitNamesOverrideMissingAnnotation(t *testing.T) {
	src := `package test
type A struct { X int ` + "`" + `json:"x"` + "`" + ` }
type B struct { Y int ` + "`" + `json:"y"` + "`" + ` }
`
	file := writeGoFile(t, src)
	structs, _, err := parseFile(file, []string{"A"})
	if err != nil {
		t.Fatal(err)
	}
	if len(structs) != 1 || structs[0].Name != "A" {
		t.Fatalf("expected only A, got: %+v", structs)
	}
}

func TestParseFile_notFound(t *testing.T) {
	file := writeGoFile(t, "package test\ntype Bar struct{}\n")
	if _, _, err := parseFile(file, []string{"Foo"}); err == nil {
		t.Fatal("expected error for missing struct")
	}
}

func TestParseFile_embedded(t *testing.T) {
	src := `package test
type Base struct {
	ID int ` + "`" + `json:"id"` + "`" + `
}
type Derived struct {
	Base
	Name string ` + "`" + `json:"name"` + "`" + `
}
`
	file := writeGoFile(t, src)
	structs, _, err := parseFile(file, []string{"Derived"})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(structs) == 0 {
		t.Fatal("no struct returned")
	}
	names := map[string]struct{}{}
	for _, f := range structs[0].Fields {
		names[f.JSONName] = struct{}{}
	}
	// Both promoted and explicit fields should appear.
	_, hasID := names["id"]
	_, hasName := names["name"]
	if !hasID || !hasName {
		t.Errorf("expected promoted fields, got %v", names)
	}
}

func TestGenerate_basic(t *testing.T) {
	structs := []StructInfo{{
		Name: "TestStruct",
		Fields: []FieldInfo{
			{GoName: "Name", JSONName: "name", GoType: "string", Kind: KindString,
				Validation: []ValidationRule{{Name: "required"}}},
			{GoName: "Count", JSONName: "count", GoType: "int", Kind: KindInt},
		},
	}}
	code, err := generate("testpkg", structs)
	if err != nil {
		t.Fatal(err)
	}
	s := string(code)
	for _, want := range []string{
		"package testpkg",
		"func (TestStruct) DecodeFrom",
		"seenName",
		`validation.Required`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in output", want)
		}
	}
}

func TestGenerate_newValidators(t *testing.T) {
	structs := []StructInfo{{
		Name: "V",
		Fields: []FieldInfo{
			{GoName: "Email", JSONName: "email", GoType: "string", Kind: KindString,
				Validation: []ValidationRule{{Name: "email"}}},
			{GoName: "URL", JSONName: "url", GoType: "string", Kind: KindString,
				Validation: []ValidationRule{{Name: "url"}}},
			{GoName: "Role", JSONName: "role", GoType: "string", Kind: KindString,
				Validation: []ValidationRule{{Name: "oneof", Value: "a|b|c"}}},
			{GoName: "N", JSONName: "n", GoType: "int", Kind: KindInt,
				Validation: []ValidationRule{
					{Name: "gte", Value: "0"},
					{Name: "lte", Value: "100"},
					{Name: "gt", Value: "5"},
				}},
		},
	}}
	code, err := generate("p", structs)
	if err != nil {
		t.Fatal(err)
	}
	s := string(code)
	for _, want := range []string{
		`decode.IsEmail`,
		`decode.IsURL`,
		`case "a", "b", "c"`,
		`validation.GT`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in generated code", want)
		}
	}
}

func TestParsePackage_annotationFiltering(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.go", `package pkg

//ggen:generate
type Wanted struct {
	X int `+"`"+`json:"x"`+"`"+`
}

type NotWanted struct {
	Y int `+"`"+`json:"y"`+"`"+`
}
`)
	writeFile(t, dir, "b.go", `package pkg

//ggen:generate
type AlsoWanted struct {
	Ref Wanted `+"`"+`json:"ref"`+"`"+`
	Z   string `+"`"+`json:"z"`+"`"+`
}
`)

	structs, pkg, err := parsePackage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if pkg != "pkg" {
		t.Errorf("pkg = %q", pkg)
	}
	names := map[string]struct{}{}
	for _, s := range structs {
		names[s.Name] = struct{}{}
	}
	_, hasWanted := names["Wanted"]
	_, hasAlso := names["AlsoWanted"]
	if !hasWanted || !hasAlso {
		t.Errorf("missing annotated structs: %v", names)
	}
	if _, ok := names["NotWanted"]; ok {
		t.Error("NotWanted should not be generated (no annotation, not referenced)")
	}
}

func TestParsePackage_crossFileAutoDiscover(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.go", `package p
//ggen:generate
type Root struct { Sub Sub `+"`"+`json:"sub"`+"`"+` }
`)
	writeFile(t, dir, "b.go", `package p
type Sub struct { N int `+"`"+`json:"n"`+"`"+` }
`)
	structs, _, err := parsePackage(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]struct{}{}
	for _, s := range structs {
		names[s.Name] = struct{}{}
	}
	if _, ok := names["Sub"]; !ok {
		t.Error("Sub should be auto-discovered across files")
	}
}

func TestParsePackage_skipsGenFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.go", `package p
//ggen:generate
type A struct { X int `+"`"+`json:"x"`+"`"+` }
`)
	writeFile(t, dir, "a_ggen.go", `package p
// this should be ignored - contains bogus syntax we do not parse
func broken {`)
	writeFile(t, dir, "a_ggen_test.go", `package p
// also ignored
func also broken {`)

	structs, _, err := parsePackage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(structs) != 1 {
		t.Errorf("got %d structs, want 1 (A only)", len(structs))
	}
}

func TestParsePackage_includesTestFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a_test.go", `package p
//ggen:generate
type A struct { X int `+"`"+`json:"x"`+"`"+` }
`)
	structs, _, err := parsePackage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(structs) != 1 || structs[0].Name != "A" {
		t.Fatalf("expected A from test file, got %+v", structs)
	}
	if !structs[0].Test {
		t.Error("expected Test=true for struct declared in _test.go")
	}
}

func TestParsePackage_noAnnotationsNoOutput(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.go", `package p
type A struct { X int `+"`"+`json:"x"`+"`"+` }
`)
	structs, _, err := parsePackage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(structs) != 0 {
		t.Errorf("got %d structs, want 0 (no annotations)", len(structs))
	}
}

func TestShouldSkipDir(t *testing.T) {
	skip := []string{".git", "_build", "vendor", "testdata", "node_modules"}
	for _, n := range skip {
		if !shouldSkipDir(n) {
			t.Errorf("shouldSkipDir(%q) = false, want true", n)
		}
	}
	keep := []string{"src", "example", "pkg"}
	for _, n := range keep {
		if shouldSkipDir(n) {
			t.Errorf("shouldSkipDir(%q) = true, want false", n)
		}
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestOutputWrittenNextToSource(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "nested", "pkg")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}

	src := `package pkg

type Msg struct {
	Text string ` + "`" + `json:"text"` + "`" + `
}
`
	goFile := filepath.Join(sub, "msg.go")
	if err := os.WriteFile(goFile, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	structs, pkg, err := parseFile(goFile, []string{"Msg"})
	if err != nil {
		t.Fatal(err)
	}
	code, err := generate(pkg, structs)
	if err != nil {
		t.Fatal(err)
	}

	outFile := strings.TrimSuffix(goFile, ".go") + "_ggen.go"
	if err := os.WriteFile(outFile, code, 0644); err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(sub, "msg_ggen.go")
	if outFile != want {
		t.Errorf("output path = %q, want %q", outFile, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("generated file not found at %q: %v", want, err)
	}
}
