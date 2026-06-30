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
			{GoName: "Code", JSONName: "code", GoType: "string", Kind: KindString,
				Validation: []ValidationRule{{Name: "alphanum"}}},
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
		`decode.IsAlphanum`,
		`decode.IsURL`,
		`case "a", "b", "c"`,
		`validation.GT`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in generated code", want)
		}
	}
}

// Rune rules avoid the utf8.RuneCountInString walk via byte-length gates, and
// drop it entirely when an ASCII-implying rule already passed in the same run
// (non-multierr).
func TestGenerate_runeGates(t *testing.T) {
	gen := func(multiErr bool, steps []Step) string {
		t.Helper()
		code, err := generate("p", []StructInfo{{
			Name:     "V",
			MultiErr: multiErr,
			Fields: []FieldInfo{{
				GoName: "S", JSONName: "s", GoType: "string", Kind: KindString,
				Pipe: steps, MultiErr: multiErr,
			}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return string(code)
	}

	t.Run("minrunes_gated", func(t *testing.T) {
		// minrunes=3: fail-free len<3, pass-free len>=9 (4*3-3), band [3,9) walks.
		s := gen(false, []Step{{V: ValidationRule{Name: "minrunes", Value: "3"}}})
		if !strings.Contains(s, "if len(result.S) < 3 {") {
			t.Errorf("missing fail-free len gate:\n%s", s)
		}
		if !strings.Contains(s, "else if len(result.S) < 9 {") {
			t.Errorf("missing ambiguous-band gate (4n-3=9):\n%s", s)
		}
	})

	t.Run("maxrunes_gated", func(t *testing.T) {
		// maxrunes=5: pass-free len<=5, fail-free len>20 (4*5), band (5,20] walks.
		s := gen(false, []Step{{V: ValidationRule{Name: "maxrunes", Value: "5"}}})
		if !strings.Contains(s, "if len(result.S) > 20 {") {
			t.Errorf("missing fail-free len gate (4n=20):\n%s", s)
		}
		if !strings.Contains(s, "else if len(result.S) > 5 {") {
			t.Errorf("missing ambiguous-band gate:\n%s", s)
		}
	})

	t.Run("minrunes_1_collapses", func(t *testing.T) {
		// minrunes=1: band [1,1) empty → only the len<1 (== empty) gate, no walk.
		s := gen(false, []Step{{V: ValidationRule{Name: "minrunes", Value: "1"}}})
		if !strings.Contains(s, "if len(result.S) < 1 {") {
			t.Errorf("minrunes=1 should be a bare len gate:\n%s", s)
		}
		// the only walk is the cold Got: in the fail branch — none in a band
		if strings.Contains(s, "else if") {
			t.Errorf("minrunes=1 should have no ambiguous band:\n%s", s)
		}
	})

	t.Run("ascii_precedes_drops_walk", func(t *testing.T) {
		// tier (c): alphanum before maxrunes (non-multierr) → count is len, no walk.
		s := gen(false, []Step{
			{V: ValidationRule{Name: "alphanum"}},
			{V: ValidationRule{Name: "maxrunes", Value: "16"}},
		})
		if strings.Contains(s, "utf8.RuneCountInString") {
			t.Errorf("ascii-preceded rune rule must not walk:\n%s", s)
		}
		if !strings.Contains(s, "if len(result.S) > 16 {") {
			t.Errorf("expected a direct len comparison:\n%s", s)
		}
	})

	t.Run("ascii_after_does_not_drop", func(t *testing.T) {
		// alphanum AFTER the rune rule can't license tier (c) — string isn't
		// known ASCII yet, so the gated walk stays.
		s := gen(false, []Step{
			{V: ValidationRule{Name: "maxrunes", Value: "16"}},
			{V: ValidationRule{Name: "alphanum"}},
		})
		if !strings.Contains(s, "utf8.RuneCountInString") {
			t.Errorf("rune rule before the ascii rule should still gate+walk:\n%s", s)
		}
	})

	t.Run("multierr_keeps_walk", func(t *testing.T) {
		// under multierr a failed alphanum doesn't stop reaching maxrunes on
		// non-ASCII input, so tier (c) is unsafe — gated walk stays.
		s := gen(true, []Step{
			{V: ValidationRule{Name: "alphanum"}},
			{V: ValidationRule{Name: "maxrunes", Value: "16"}},
		})
		if !strings.Contains(s, "utf8.RuneCountInString") {
			t.Errorf("multierr must keep the walk (tier c unsafe):\n%s", s)
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
// for generic sql.Null[T]: built-in primitive inners classify as KindSQLNull
// with the V field; non-primitive inners return false (left on the
// encoding/json fallback). The go/types path (arbitrary inner T) is exercised
// in integrationtests.
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
