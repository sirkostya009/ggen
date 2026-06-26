package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// generate is a test-only convenience over generateTo that returns the
// output as a []byte for in-memory assertions.
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
	t.Parallel()
	src := `package test

type Foo struct {
	Name    string  ` + "`" + `json:"name" pipe:"required minlen=1"` + "`" + `
	Age     int     ` + "`" + `json:"age" pipe:"gte=0 lte=150"` + "`" + `
	Ignored string  ` + "`" + `json:"-"` + "`" + `
	Items   []string ` + "`" + `json:"items"` + "`" + `
}
`
	file := writeGoFile(t, src)

	structs, pkg, _, err := parseFile(file, []string{"Foo"})
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
	t.Parallel()
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

	structs, _, _, err := parseFile(file, []string{"Parent"})
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

// Single-file mode with neither a //ggen:generate annotation nor an explicit
// name filter must error, not emit code for every exported struct.
func TestParseFile_noAnnotationNoFilter_Errors(t *testing.T) {
	t.Parallel()
	src := `package test
type A struct { X int ` + "`" + `json:"x"` + "`" + ` }
type B struct { Y int ` + "`" + `json:"y"` + "`" + ` }
`
	file := writeGoFile(t, src)
	_, _, _, err := parseFile(file, nil)
	if err == nil {
		t.Fatal("expected error when no annotation and no name filter")
	}
	for _, want := range []string{"//ggen:generate", filepath.Base(file)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing substring %q", err.Error(), want)
		}
	}
}

// An explicit name filter works without any //ggen:generate annotation.
func TestParseFile_explicitNamesOverrideMissingAnnotation(t *testing.T) {
	t.Parallel()
	src := `package test
type A struct { X int ` + "`" + `json:"x"` + "`" + ` }
type B struct { Y int ` + "`" + `json:"y"` + "`" + ` }
`
	file := writeGoFile(t, src)
	structs, _, _, err := parseFile(file, []string{"A"})
	if err != nil {
		t.Fatal(err)
	}
	if len(structs) != 1 || structs[0].Name != "A" {
		t.Fatalf("expected only A, got: %+v", structs)
	}
}

func TestParseFile_notFound(t *testing.T) {
	t.Parallel()
	file := writeGoFile(t, "package test\ntype Bar struct{}\n")
	if _, _, _, err := parseFile(file, []string{"Foo"}); err == nil {
		t.Fatal("expected error for missing struct")
	}
}

func TestParseFile_embedded(t *testing.T) {
	t.Parallel()
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
	structs, _, _, err := parseFile(file, []string{"Derived"})
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
		"func (recv TestStruct) DecodeFrom",
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

// TestGenerate_runeCountHoist pins opt #25: >=2 rune rules against an
// unchanged ref share one hoisted utf8.RuneCountInString (rc); a lone rule
// stays inline; a string-mutating mod between rules splits the run so neither
// side hoists.
func TestGenerate_runeCountHoist(t *testing.T) {
	gen := func(steps []Step) string {
		t.Helper()
		code, err := generate("p", []StructInfo{{
			Name: "V",
			Fields: []FieldInfo{{
				GoName: "S", JSONName: "s", GoType: "string", Kind: KindString,
				Pipe: steps,
			}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return string(code)
	}

	t.Run("pair_hoisted", func(t *testing.T) {
		s := gen([]Step{
			{V: ValidationRule{Name: "minrunes", Value: "2"}},
			{V: ValidationRule{Name: "maxrunes", Value: "5"}},
		})
		// one rc per decode path (DecodeFrom + DecodeFromStream)
		if got := strings.Count(s, "rc := utf8.RuneCountInString(result.S)"); got != 2 {
			t.Errorf("want 2 hoisted rc, got %d:\n%s", got, s)
		}
		// the only RuneCountInString calls are the two hoists — no inline recompute
		if got := strings.Count(s, "utf8.RuneCountInString"); got != 2 {
			t.Errorf("want 2 total RuneCountInString (no inline recompute), got %d:\n%s", got, s)
		}
		if !strings.Contains(s, "Got: rc}") {
			t.Errorf("failure branch should reuse rc for Got:\n%s", s)
		}
	})

	t.Run("single_inline", func(t *testing.T) {
		s := gen([]Step{{V: ValidationRule{Name: "minrunes", Value: "2"}}})
		if strings.Contains(s, "rc := utf8.RuneCountInString") {
			t.Errorf("single rune rule should not hoist:\n%s", s)
		}
		// inline recomputes in both the condition and Got: — 2 per path,
		// 2 paths = 4 (no hoist, so no rc to reuse)
		if got := strings.Count(s, "utf8.RuneCountInString(result.S)"); got != 4 {
			t.Errorf("want 4 inline RuneCountInString, got %d:\n%s", got, s)
		}
	})

	t.Run("mod_splits_run", func(t *testing.T) {
		// trim mutates the string, so the rune rules straddle a mod and
		// can't share a count — each stays inline.
		s := gen([]Step{
			{V: ValidationRule{Name: "minrunes", Value: "2"}},
			{IsMod: true, M: ModRule{Name: "trim"}},
			{V: ValidationRule{Name: "maxrunes", Value: "5"}},
		})
		if strings.Contains(s, "rc := utf8.RuneCountInString") {
			t.Errorf("rune rules split by a mod must not hoist:\n%s", s)
		}
	})
}

func TestParsePackage_annotationFiltering(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

	structs, pkg, _, err := parseFile(goFile, []string{"Msg"})
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

// TestSQLNullGeneric covers the string-based path (resolveKind + SQLNullSpec)
// for generic sql.Null[T] — the AST-only loader's classification. Built-in
// primitive inners classify as KindSQLNull with the V field; non-primitive
// inners return false (left on the encoding/json fallback). The go/types path
// (arbitrary inner T) is exercised end-to-end in integrationtests.
func TestSQLNullGeneric(t *testing.T) {
	t.Parallel()
	supported := []struct {
		goType string
		inner  TypeKind
		typ    string
	}{
		{"sql.Null[string]", KindString, "string"},
		{"sql.Null[bool]", KindBool, "bool"},
		{"sql.Null[int]", KindInt, "int"},
		{"sql.Null[int64]", KindInt64, "int64"},
		{"sql.Null[uint64]", KindUint64, "uint64"},
		{"sql.Null[float32]", KindFloat32, "float32"},
		{"sql.Null[float64]", KindFloat64, "float64"},
		{"sql.Null[time.Time]", KindTime, "time.Time"},
	}
	for _, c := range supported {
		t.Run(c.goType, func(t *testing.T) {
			t.Parallel()
			if k := resolveKind(c.goType); k != KindSQLNull {
				t.Fatalf("resolveKind(%q) = %v, want KindSQLNull", c.goType, k)
			}
			spec, ok := SQLNullSpec(c.goType)
			if !ok {
				t.Fatalf("SQLNullSpec(%q) not ok", c.goType)
			}
			if spec.Field != "V" || spec.Inner != c.inner || spec.Type != c.typ {
				t.Errorf("SQLNullSpec(%q) = %+v, want {V %v %s}", c.goType, spec, c.inner, c.typ)
			}
		})
	}

	// Non-primitive / malformed inners are not classified by the string path.
	for _, goType := range []string{"sql.Null[netip.Addr]", "sql.Null[[]byte]", "sql.Null[]", "sql.NullFoo"} {
		t.Run("reject/"+goType, func(t *testing.T) {
			t.Parallel()
			if k := resolveKind(goType); k == KindSQLNull {
				t.Errorf("resolveKind(%q) = KindSQLNull, want fallback", goType)
			}
			if _, ok := SQLNullSpec(goType); ok {
				t.Errorf("SQLNullSpec(%q) ok, want false", goType)
			}
		})
	}
}
