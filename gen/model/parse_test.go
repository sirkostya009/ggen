package model

import (
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeGoFile(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "test.go")
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
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

	res, err := ParseFile(file, []string{"Foo"})

	structs, pkg := res.Structs, res.PkgName
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

	res, err := ParseFile(file, []string{"Parent"})

	structs := res.Structs
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
// name filter must fail, not emit code for every exported struct.
func TestParseFile_noAnnotationNoFilter_Errors(t *testing.T) {
	t.Parallel()
	src := `package test
type A struct { X int ` + "`" + `json:"x"` + "`" + ` }
type B struct { Y int ` + "`" + `json:"y"` + "`" + ` }
`
	file := writeGoFile(t, src)
	_, err := ParseFile(file, nil)
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
	res, err := ParseFile(file, []string{"A"})
	structs := res.Structs
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
	if _, err := ParseFile(file, []string{"Foo"}); err == nil {
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
	res, err := ParseFile(file, []string{"Derived"})
	structs := res.Structs
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
	// Both promoted and own fields should appear.
	_, hasID := names["id"]
	_, hasName := names["name"]
	if !hasID || !hasName {
		t.Errorf("expected promoted fields, got %v", names)
	}
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

	pkg, err := ParsePackage(dir)
	structs := pkg.Structs
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Name != "pkg" {
		t.Errorf("pkg = %q", pkg.Name)
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
	pkg, err := ParsePackage(dir)
	structs := pkg.Structs
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
	writeFile(t, dir, "a_ggen.go", `// Code generated by ggen; DO NOT EDIT.

package p
// this should be ignored - contains bogus syntax we do not parse
func broken {`)
	writeFile(t, dir, "a_ggen_test.go", `// Code generated by ggen; DO NOT EDIT.

package p
// also ignored
func also broken {`)

	pkg, err := ParsePackage(dir)
	structs := pkg.Structs
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
	pkg, err := ParsePackage(dir)
	structs := pkg.Structs
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
	pkg, err := ParsePackage(dir)
	structs := pkg.Structs
	if err != nil {
		t.Fatal(err)
	}
	if len(structs) != 0 {
		t.Errorf("got %d structs, want 0 (no annotations)", len(structs))
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSQLNullGeneric covers the string-based path, ResolveKind and
// SQLNullSpec, for the generic sql.Null[T]. Built-in primitive inner types
// become KindSQLNull with the V field. Other inner types return false and
// stay on the encoding/json fallback. The go/types path, which takes any
// inner T, is tested in integrationtests.
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
			if k := ResolveKind(c.goType); k != KindSQLNull {
				t.Fatalf("ResolveKind(%q) = %v, want KindSQLNull", c.goType, k)
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

	// The string path does not classify non-primitive or malformed inner
	// types.
	for _, goType := range []string{"sql.Null[netip.Addr]", "sql.Null[[]byte]", "sql.Null[]", "sql.NullFoo"} {
		t.Run("reject/"+goType, func(t *testing.T) {
			t.Parallel()
			if k := ResolveKind(goType); k == KindSQLNull {
				t.Errorf("ResolveKind(%q) = KindSQLNull, want fallback", goType)
			}
			if _, ok := SQLNullSpec(goType); ok {
				t.Errorf("SQLNullSpec(%q) ok, want false", goType)
			}
		})
	}
}

// Parser regression tests.
func TestParseFile_round9(t *testing.T) {
	t.Parallel()
	t.Run("tab_after_directive", func(t *testing.T) {
		t.Parallel()
		// Like go:generate, the directive accepts tabs as separators. A tab
		// must not silently drop the whole annotation.
		file := writeGoFile(t, "package test\n//ggen:generate\tmarshal\ntype A struct{ X int `json:\"x\"` }\n")
		res, err := ParseFile(file, nil)
		structs := res.Structs
		if err != nil {
			t.Fatal(err)
		}
		if len(structs) != 1 || !structs[0].Marshal {
			t.Fatalf("tab-separated annotation dropped: %+v", structs)
		}
	})

	t.Run("depth_dominance", func(t *testing.T) {
		t.Parallel()
		// A promoted field at depth 1 beats one at depth 2, by the
		// encoding/json dominant-field rule. Neither may be dropped, or the
		// output would be `{}`.
		src := `package test
//ggen:generate
type Top struct {
	A
	B
}
type A struct{ X int ` + "`json:\"x\"`" + ` }
type B struct{ C }
type C struct{ X int ` + "`json:\"x\"`" + ` }
`
		file := writeGoFile(t, src)
		res, err := ParseFile(file, []string{"Top"})
		structs := res.Structs
		if err != nil {
			t.Fatal(err)
		}
		var top *StructInfo
		for i := range structs {
			if structs[i].Name == "Top" {
				top = &structs[i]
			}
		}
		if top == nil {
			t.Fatal("Top not returned")
		}
		var xs []FieldInfo
		for _, f := range top.Fields {
			if f.JSONName == "x" {
				xs = append(xs, f)
			}
		}
		if len(xs) != 1 || xs[0].StructName != "A" {
			t.Fatalf("want exactly A.X to survive, got %+v", xs)
		}
	})

	t.Run("cyclic_embedding_errors", func(t *testing.T) {
		t.Parallel()
		// Invalid Go, but ggen reads the syntax first. It must report an
		// error, not overflow the generator's stack.
		src := `package test
//ggen:generate
type A struct{ B; X int ` + "`json:\"x\"`" + ` }
type B struct{ A }
`
		file := writeGoFile(t, src)
		_, err := ParseFile(file, []string{"A"})
		if err == nil || !strings.Contains(err.Error(), "cyclic embedding") {
			t.Fatalf("want cyclic-embedding diagnostic, got %v", err)
		}
	})

	t.Run("ignored_embedding_promotes_nothing", func(t *testing.T) {
		t.Parallel()
		// An ignored embedding is skipped whatever its type, and a tag
		// without a json key does not stop promotion.
		src := `package test
import "sync"
//ggen:generate
type Top struct {
	Skip ` + "`json:\"-\"`" + `
	*sync.Mutex ` + "`json:\"-\"`" + `
	Keep ` + "`db:\"keep\"`" + `
	X int ` + "`json:\"x\"`" + `
}
type Skip struct{ S Dep ` + "`json:\"s\"`" + ` }
type Dep struct{ D int ` + "`json:\"d\"`" + ` }
type Keep struct{ K int ` + "`json:\"k\"`" + ` }
`
		file := writeGoFile(t, src)
		res, err := ParseFile(file, []string{"Top"})
		if err != nil {
			t.Fatal(err)
		}
		for _, st := range res.Structs {
			if st.Name == "Dep" || st.Name == "Skip" {
				t.Fatalf("%s reached only through an ignored embedding was generated", st.Name)
			}
			if st.Name != "Top" {
				continue
			}
			var names []string
			for _, f := range st.Fields {
				names = append(names, f.JSONName)
			}
			if got := strings.Join(names, ","); got != "k,x" {
				t.Fatalf("want fields k,x, got %s", got)
			}
		}
	})

	t.Run("named_embedding_errors", func(t *testing.T) {
		t.Parallel()
		src := `package test
//ggen:generate
type Top struct{ Inner ` + "`json:\"inner\"`" + ` }
type Inner struct{ N int ` + "`json:\"n\"`" + ` }
`
		file := writeGoFile(t, src)
		_, err := ParseFile(file, []string{"Top"})
		if err == nil || !strings.Contains(err.Error(), "json-tagged embedded field") {
			t.Fatalf("want json-tagged embedding diagnostic, got %v", err)
		}
	})

	t.Run("map_value_sibling_generated", func(t *testing.T) {
		t.Parallel()
		// A struct reached only through map[string]Inner must be generated,
		// not left to the json.Unmarshal fallback.
		src := `package test
//ggen:generate
type Outer struct{ M map[string]Inner ` + "`json:\"m\"`" + ` }
type Inner struct{ N int ` + "`json:\"n\"`" + ` }
`
		file := writeGoFile(t, src)
		res, err := ParseFile(file, []string{"Outer"})
		structs := res.Structs
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, s := range structs {
			if s.Name == "Inner" {
				found = true
			}
		}
		if !found {
			t.Fatalf("map-valued Inner not queued: %+v", structs)
		}
	})
}

// A `//go:build ignore` helper of another package does not decide the AST
// loader's package name, whatever its position in directory order.
func TestLoadStructs_IgnoredFileDoesNotNamePackage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tool := filepath.Join(dir, "gen_tool.go")
	src := filepath.Join(dir, "w_windows.go")
	if err := os.WriteFile(tool, []byte("//go:build ignore\n\npackage main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("package w\n\n//ggen:generate\ntype W struct {\n\tX int `json:\"x\"`\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base, _, err := loadStructs([]string{tool, src})
	if err != nil {
		t.Fatal(err)
	}
	if base.pkgName != "w" || len(base.annotatedList()) != 1 {
		t.Fatalf("pkgName %q, annotated %v; want w with W", base.pkgName, base.annotatedList())
	}
}

// A file's name constrains it the way go/build reads it. The `_ggen` suffix
// of the output name hides that constraint, so the output bucket key must
// include it.
func TestFilenameConstraint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		src, file, want string
	}{
		{"package p\n", "os_linux.go", "linux"},
		{"package p\n", "cpu_amd64.go", "amd64"},
		{"package p\n", "os_linux_amd64.go", "linux && amd64"},
		{"package p\n", "os_linux_test.go", "linux"},
		{"package p\n", "os_linux_amd64_test.go", "linux && amd64"},
		{"package p\n", "linux.go", ""},
		{"package p\n", "amd64.go", ""},
		{"package p\n", "not_an_os.go", ""},
		{"package p\n", "p_linux_ggen.go", ""},
		{"//go:build cgo\n\npackage p\n", "os_darwin.go", "cgo && darwin"},
		{"// +build foo\n\npackage p\n", "cpu_arm64.go", "foo && arm64"},
		{"// +build foo\n\n// Package p.\npackage p\n", "a.go", "foo"},
		{"// +build foo\npackage p\n", "a.go", ""},
		{"// Package p.\n// +build foo\npackage p\n", "a.go", ""},
		{"/* x */\n// +build foo\n\npackage p\n", "a.go", ""},
		{"//go:build foo\npackage p\n", "a.go", "foo"},
		{"package p\n\nimport \"C\"\n", "native.go", "cgo"},
		{"package p\n\nimport \"C\"\n", "native_linux.go", "linux && cgo"},
	}
	for _, tc := range cases {
		af, err := parser.ParseFile(token.NewFileSet(), tc.file, tc.src, parser.ParseComments|parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		if got := fileBuildConstraint(af, filepath.Join("some", "dir", tc.file), false); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.file, got, tc.want)
		}
	}
}

func TestParseFile_round10(t *testing.T) {
	t.Parallel()
	embedFields := func(t *testing.T, structs []StructInfo, name string) []FieldInfo {
		t.Helper()
		for _, st := range structs {
			if st.Name != name {
				continue
			}
			var out []FieldInfo
			for _, f := range st.Fields {
				if f.Embed {
					out = append(out, f)
				}
			}
			return out
		}
		t.Fatalf("%s not returned", name)
		return nil
	}

	t.Run("embed_own_dominates_promoted", func(t *testing.T) {
		t.Parallel()
		// jsonv2 keeps the shallowest catch-all. The emitters read exactly
		// one, so the promoted one must be dropped.
		src := `package test
type Inner struct{ Extra map[string]any ` + "`json:\",embed\"`" + ` }
//ggen:generate
type Outer struct {
	Inner
	More map[string]any ` + "`json:\",embed\"`" + `
}
`
		res, err := ParseFile(writeGoFile(t, src), []string{"Outer"})
		structs := res.Structs
		if err != nil {
			t.Fatal(err)
		}
		if fs := embedFields(t, structs, "Outer"); len(fs) != 1 || fs[0].GoName != "More" {
			t.Fatalf("want exactly the own catch-all More, got %+v", fs)
		}
	})

	t.Run("embed_promoted_tie_drops_both", func(t *testing.T) {
		t.Parallel()
		src := `package test
//ggen:generate
type Top struct {
	A
	B
	N int ` + "`json:\"n\"`" + `
}
type A struct{ Extra map[string]any ` + "`json:\",embed\"`" + ` }
type B struct{ More map[string]any ` + "`json:\",embed\"`" + ` }
`
		res, err := ParseFile(writeGoFile(t, src), []string{"Top"})
		structs := res.Structs
		if err != nil {
			t.Fatal(err)
		}
		if fs := embedFields(t, structs, "Top"); len(fs) != 0 {
			t.Fatalf("a same-depth promoted tie keeps no catch-all, got %+v", fs)
		}
	})

	t.Run("embed_two_own_rejected", func(t *testing.T) {
		t.Parallel()
		src := `package test
//ggen:generate
type Two struct {
	Extra map[string]any ` + "`json:\",embed\"`" + `
	More  map[string]any ` + "`json:\",embed\"`" + `
}
`
		_, err := ParseFile(writeGoFile(t, src), []string{"Two"})
		if err == nil || !strings.Contains(err.Error(), "cannot both be the json:\",embed\" catch-all map") {
			t.Fatalf("want two-catch-all diagnostic, got %v", err)
		}
	})

	t.Run("promoted_go_name_clash_rejected", func(t *testing.T) {
		t.Parallel()
		// Different JSON names but one Go name. encoding/json keeps both.
		// ggen cannot access them by name, so it must report an error
		// instead of dropping the pair.
		src := `package test
//ggen:generate
type Parent struct {
	E1
	E2
}
type E1 struct{ A int ` + "`json:\"a1\"`" + ` }
type E2 struct{ A int ` + "`json:\"a2\"`" + ` }
`
		_, err := ParseFile(writeGoFile(t, src), []string{"Parent"})
		if err == nil || !strings.Contains(err.Error(), "share Go name A") {
			t.Fatalf("want Go-name clash diagnostic, got %v", err)
		}
	})

	t.Run("shapeless_field_types_rejected", func(t *testing.T) {
		t.Parallel()
		for _, decl := range []string{
			"S struct{ X int }",
			"P *struct{ X int }",
			"L []struct{ X int }",
			"M map[string]struct{ X int }",
			"F func()",
			"C chan int",
		} {
			src := "package test\n//ggen:generate\ntype A struct{ " + decl + " `json:\"v\"` }\n"
			_, err := ParseFile(writeGoFile(t, src), []string{"A"})
			if err == nil || !strings.Contains(err.Error(), "field types are not supported") {
				t.Errorf("%s: want unsupported-type diagnostic, got %v", decl, err)
			}
		}
		// `json:"-"` drops the field before its type is judged.
		src := "package test\n//ggen:generate\ntype A struct{ V int `json:\"v\"`; F func() `json:\"-\"` }\n"
		if _, err := ParseFile(writeGoFile(t, src), []string{"A"}); err != nil {
			t.Errorf("ignored func field: %v", err)
		}
		// Interfaces are accepted. `interface{}` is `any`, and one with
		// methods marshals as whatever value it holds.
		for _, decl := range []string{"V interface{}", "V interface{ M() }"} {
			src := "package test\n//ggen:generate\ntype A struct{ " + decl + " `json:\"v\"` }\n"
			if _, err := ParseFile(writeGoFile(t, src), []string{"A"}); err != nil {
				t.Errorf("%s field: %v", decl, err)
			}
		}
	})
}

func TestParsePackage_docsDirectivesConsts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/m\n\ngo 1.27\n")
	writeFile(t, dir, "m.go", `package m

import "net/http"

// Role is the access level.
type Role string

const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

type Plan int

const PlanPro Plan = 2

// User is a person.
//
//ggen:generate
//schema:out
type User struct {
	// Role is documented above the field.
	Role Role `+"`json:\"role\"`"+`
	Plan Plan `+"`json:\"plan\"`"+` // Plan has a trailing comment.
	Code int  `+"`json:\"code\"`"+`
}

var _ = http.StatusOK
`)
	pkg, err := ParsePackage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Structs) != 1 {
		t.Fatalf("structs = %d", len(pkg.Structs))
	}
	u := pkg.Structs[0]
	if u.Doc != "User is a person." || !slices.Equal(u.Directives, []string{"ggen:generate", "schema:out"}) {
		t.Errorf("doc %q directives %q", u.Doc, u.Directives)
	}
	if u.File == "" || u.Type == nil || pkg.Path != "example.com/m" {
		t.Errorf("file %q type %v path %q", u.File, u.Type, pkg.Path)
	}
	docs := map[string]string{}
	for _, f := range u.Fields {
		docs[f.JSONName] = f.Doc
		if f.Type == nil {
			t.Errorf("%s: nil Type", f.JSONName)
		}
	}
	if docs["role"] != "Role is documented above the field." || docs["plan"] != "Plan has a trailing comment." || docs["code"] != "" {
		t.Errorf("field docs %q", docs)
	}
	want := map[string][]Const{
		"example.com/m.Role": {{Pkg: "example.com/m", Name: "RoleAdmin", Value: `"admin"`}, {Pkg: "example.com/m", Name: "RoleMember", Value: `"member"`}},
		"example.com/m.Plan": {{Pkg: "example.com/m", Name: "PlanPro", Value: "2"}},
	}
	for k, v := range want {
		if !slices.Equal(pkg.Consts[k], v) {
			t.Errorf("%s = %v, want %v", k, pkg.Consts[k], v)
		}
	}
	if d := pkg.TypeDocs["Role"]; d.Doc != "Role is the access level." || len(d.Directives) != 0 {
		t.Errorf("Role doc %+v", d)
	}
	if _, ok := pkg.TypeDocs["Plan"]; ok || len(pkg.TypeDocs) != 1 {
		t.Errorf("type docs %v", pkg.TypeDocs)
	}
	// Constants reached through an import: net/http's ConnState.
	if cs := pkg.Consts["net/http.ConnState"]; len(cs) == 0 || cs[0].Name != "StateNew" || cs[0].Value != "0" {
		t.Errorf("imported consts = %v", cs)
	}
}

// A type written through an `=` alias is classified as the type it names, at
// every position. The fields of a struct alias keep the kinds of their named
// types, as on a struct field: net.IP is not looked through to its []byte.
func TestParsePackage_eqAliasKinds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/m\n\ngo 1.27\n")
	writeFile(t, dir, "m.go", `package m

import (
	"encoding/json"
	"net"
	"time"
)

type Level string

type (
	Addr  = net.TCPAddr
	Dur   = time.Duration
	IDs   = []string
	IntP  = *int
	Count = int
	Lvl   = Level
)

//ggen:generate
type S struct {
	A  Addr            `+"`json:\"a\"`"+`
	PA *Addr           `+"`json:\"pa\"`"+`
	SD []Dur           `+"`json:\"sd\"`"+`
	MD map[string]Dur  `+"`json:\"md\"`"+`
	I  IDs             `+"`json:\"i\"`"+`
	P  IntP            `+"`json:\"p\"`"+`
	C  Count           `+"`json:\"c\"`"+`
	L  []Lvl           `+"`json:\"l\"`"+`
	R  json.RawMessage `+"`json:\"r\"`"+`
}

//ggen:generate
type CIDR net.IPNet
`)
	pkg, err := ParsePackage(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]FieldInfo{}
	for _, s := range pkg.Structs {
		for _, f := range s.Fields {
			got[s.Name+"."+f.GoName] = f
		}
	}
	for _, c := range []struct {
		field, goType string
		kind          TypeKind
		pointer       bool
		elemType      string
		elemKind      TypeKind
	}{
		{"S.A", "net.TCPAddr", KindTCPAddr, false, "", 0},
		{"S.PA", "*net.TCPAddr", KindTCPAddr, true, "", 0},
		{"S.SD", "[]time.Duration", KindSlice, false, "time.Duration", KindDuration},
		{"S.MD", "map[string]time.Duration", KindMap, false, "time.Duration", KindDuration},
		{"S.I", "[]string", KindSlice, false, "string", KindString},
		{"S.P", "*int", KindInt, true, "", 0},
		{"S.C", "int", KindInt, false, "", 0},
		{"S.L", "[]Level", KindSlice, false, "Level", KindStruct},
		{"S.R", "json.RawMessage", KindRawJSON, false, "", 0},
		{"CIDR.IP", "net.IP", KindNetIP, false, "", 0},
		{"CIDR.Mask", "net.IPMask", KindStruct, false, "", 0},
	} {
		f, ok := got[c.field]
		if !ok {
			t.Errorf("%s missing", c.field)
			continue
		}
		if f.GoType != c.goType || f.Kind != c.kind || f.Pointer != c.pointer || f.ElemType != c.elemType || f.ElemKind != c.elemKind {
			t.Errorf("%s = {%q %v pointer=%v elem %q %v}, want {%q %v pointer=%v elem %q %v}", c.field,
				f.GoType, f.Kind, f.Pointer, f.ElemType, f.ElemKind, c.goType, c.kind, c.pointer, c.elemType, c.elemKind)
		}
	}
	if imps := got["S.A"].TypeImports; len(imps) != 1 || imps[0].Path != "net" {
		t.Errorf("S.A imports %v, want net", imps)
	}
	if k, ok := got["S.L"].NamedPrims["Level"]; !ok || k != KindString {
		t.Errorf("S.L named primitives %v, want Level as a string", got["S.L"].NamedPrims)
	}
}

// A generic type with several type arguments is spelled as Go writes it at
// every position, a type argument from a package imported under another name
// included.
func TestParsePackage_multiArgGenerics(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/m\n\ngo 1.27\n")
	if err := os.Mkdir(filepath.Join(dir, "leaf"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "leaf/leaf.go", "package leaf\n\ntype Name string\n")
	writeFile(t, dir, "m.go", `package m

import lf "example.com/m/leaf"

type Pair[K comparable, V any] struct {
	Key K
	Val V
}

//ggen:generate
type S struct {
	V Pair[string, int]            `+"`json:\"v\"`"+`
	L []Pair[string, int]          `+"`json:\"l\"`"+`
	P *Pair[string, int]           `+"`json:\"p\"`"+`
	M map[string]Pair[string, int] `+"`json:\"m\"`"+`
	Q []Pair[lf.Name, int]         `+"`json:\"q\"`"+`
}
`)
	pkg, err := ParsePackage(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]FieldInfo{}
	for _, f := range pkg.Structs[0].Fields {
		got[f.GoName] = f
	}
	for _, c := range []struct{ field, goType, elemType string }{
		{"V", "Pair[string, int]", ""},
		{"L", "[]Pair[string, int]", "Pair[string, int]"},
		{"P", "*Pair[string, int]", ""},
		{"M", "map[string]Pair[string, int]", "Pair[string, int]"},
		{"Q", "[]Pair[leaf.Name, int]", "Pair[leaf.Name, int]"},
	} {
		if f := got[c.field]; f.GoType != c.goType || f.ElemType != c.elemType {
			t.Errorf("%s = %q elem %q, want %q elem %q", c.field, f.GoType, f.ElemType, c.goType, c.elemType)
		}
	}
	if p := got["P"]; p.PointeeType != "Pair[string, int]" {
		t.Errorf("P pointee = %q", p.PointeeType)
	}
}

// A struct tag written as an interpreted string reads like a raw one.
func TestParseFile_interpretedStringTag(t *testing.T) {
	t.Parallel()
	file := writeGoFile(
		t,
		"package test\n\ntype Foo struct {\n\tA string \"json:\\\"alpha,omitempty\\\" pipe:\\\"minlen=2\\\"\"\n\tB string \"json:\\\"-\\\"\"\n}\n",
	)
	res, err := ParseFile(file, []string{"Foo"})
	if err != nil {
		t.Fatal(err)
	}
	fs := res.Structs[0].Fields
	if len(fs) != 1 || fs[0].JSONName != "alpha" || !fs[0].OmitEmpty || len(fs[0].Validation) != 1 {
		t.Errorf("fields = %+v", fs)
	}
}

// A map alias keyed by a named string type is refused, as a map field with
// that key is. A key spelled through an `=` alias of string is accepted.
func TestParsePackage_mapAliasNamedKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/m\n\ngo 1.27\n")
	writeFile(t, dir, "m.go", `package m

type Key string

type Str = string

//ggen:generate
type ByKey map[Key]int
`)
	if _, err := ParsePackage(dir); err == nil || !strings.Contains(err.Error(), "map alias key must be string, got Key") {
		t.Fatalf("named key: err = %v", err)
	}
	writeFile(t, dir, "m.go", `package m

type Str = string

//ggen:generate
type ByStr map[Str]int
`)
	if _, err := ParsePackage(dir); err != nil {
		t.Fatalf("= alias key: %v", err)
	}
}

// A name filter also emits the annotated non-struct types its roots reference
// and the annotated struct under a struct-alias root, because the emitted
// code calls their methods. Unannotated ones stay out.
func TestParseFile_filterEmitsAliasDeps(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/m\n\ngo 1.27\n")
	writeFile(t, dir, "m.go", `package m
//ggen:generate
type A struct {
	B Bs `+"`json:\"b\"`"+`
	T Tg `+"`json:\"t\"`"+`
	P []Tg `+"`json:\"p\"`"+`
	U Plain `+"`json:\"u\"`"+`
}
//ggen:generate
type Bs struct{ X int `+"`json:\"x\"`"+` }
//ggen:generate
type Tg []string
type Plain []int
//ggen:generate
type Inner struct{ X int `+"`json:\"x\"`"+` }
//ggen:generate
type L Inner
type Loose struct{ X int `+"`json:\"x\"`"+` }
//ggen:generate
type LL Loose
`)
	for _, c := range []struct {
		root string
		want []string
	}{
		{"A", []string{"A", "Bs", "Tg"}},
		{"L", []string{"L", "Inner"}},
		{"LL", []string{"LL"}},
	} {
		res, err := ParseFile(filepath.Join(dir, "m.go"), []string{c.root})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, s := range res.Structs {
			got = append(got, s.Name)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.root, got, c.want)
		}
	}
}

func TestParsePackage_nestedMapKeyRejected(t *testing.T) {
	t.Parallel()
	for name, decl := range map[string]string{
		"alias_map_value":  "type T map[string]map[Key]int",
		"alias_slice_elem": "type T []map[int]string",
		"alias_array_ptr":  "type T [2]*map[Key]int",
		"field_slice_elem": "type T struct{ A []map[Key]int `json:\"a\"` }",
		"field_map_ptr":    "type T struct{ B map[string]*map[int]int `json:\"b\"` }",
		"field_eq_alias":   "type IM = map[int]string\n//ggen:generate\ntype T struct{ C []IM `json:\"c\"` }",
		"field_array_deep": "type T struct{ D [1][]map[Key]bool `json:\"d\"` }",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeFile(t, dir, "go.mod", "module example.com/m\n\ngo 1.27\n")
			writeFile(t, dir, "m.go", "package m\n\ntype Key string\n\n//ggen:generate\n"+decl+"\n")
			_, err := ParsePackage(dir)
			if err == nil || !strings.Contains(err.Error(), "map key must be string") {
				t.Fatalf("err = %v, want a map key rejection", err)
			}
		})
	}
}

func TestParsePackage_brokenPackage(t *testing.T) {
	t.Parallel()
	const root = "package m\n\n//ggen:generate\ntype A struct {\n\tX     int `json:\"x\"`\n\tInner B   `json:\"b\"`\n}\n"
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"syntax error", map[string]string{"b.go": "package m\n\ntype B struct {\n\tY int json:\"y\"\n}\n"}, "b.go:4:"},
		{"undefined type", map[string]string{"b.go": "package m\n\ntype B = Missing\n"}, "undefined: Missing"},
		{"failed import", map[string]string{"b.go": "package m\n\nimport \"example.com/m/nope\"\n\ntype B = nope.T\n"}, "nope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeFile(t, dir, "go.mod", "module example.com/m\n\ngo 1.27\n")
			writeFile(t, dir, "a.go", root)
			for name, src := range tc.files {
				writeFile(t, dir, name, src)
			}
			for _, parse := range []func() error{
				func() error { _, err := ParsePackage(dir); return err },
				func() error { _, err := ParseFile(filepath.Join(dir, "a.go"), nil); return err },
			} {
				err := parse()
				if _, ok := errors.AsType[*BrokenPackageError](err); !ok || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("err = %v, want a BrokenPackageError mentioning %q", err, tc.want)
				}
			}
		})
	}
}

// A package that calls methods ggen has not generated yet, or whose stale
// output no longer compiles, still loads.
func TestParsePackage_bootstrapTypeErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/m\n\ngo 1.27\n")
	writeFile(
		t,
		dir,
		"a.go",
		"package m\n\n//ggen:generate\ntype A struct {\n\tX int `json:\"x\"`\n}\n\nvar _ = A{}.JSONSize\n\nfunc size(a A) int { return a.JSONSize() }\n",
	)
	writeFile(t, dir, "a_test.go", "package m\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) { _, _, _ = A{}.DecodeFrom(nil) }\n")
	writeFile(t, dir, "x_test.go", "package m_test\n\nimport (\n\t\"testing\"\n\n\t\"example.com/m\"\n)\n\nfunc TestX(t *testing.T) { _ = m.A{}.JSONSize() }\n")
	writeFile(t, dir, "a_ggen.go", "// Code generated by ggen; DO NOT EDIT.\n\npackage m\n\nfunc (a A) Old() int { return a.Gone }\n")
	pkg, err := ParsePackage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Structs) != 1 {
		t.Fatalf("got %d structs, want 1", len(pkg.Structs))
	}
	if _, err := ParseFile(filepath.Join(dir, "a.go"), nil); err != nil {
		t.Fatal(err)
	}
}

// Earlier output is recognised by its header, whatever -o named it.
func TestParsePackage_genFileByHeader(t *testing.T) {
	t.Parallel()
	const methods = "\n\nfunc (B) JSONSize() int { return 0 }\n"
	for _, tc := range []struct {
		name, file, header string
		want               int
	}{
		{"header, any name", "codec.go", "// Code generated by ggen; DO NOT EDIT.\n\n", 2},
		{"gen name, no header", "b_ggen.go", "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeFile(t, dir, "go.mod", "module example.com/m\n\ngo 1.27\n")
			writeFile(t, dir, "a.go", "package m\n\n//ggen:generate\ntype A struct {\n\tInner B `json:\"b\"`\n}\n\ntype B struct {\n\tY int `json:\"y\"`\n}\n")
			writeFile(t, dir, tc.file, tc.header+"package m"+methods)
			pkg, err := ParsePackage(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(pkg.Structs) != tc.want {
				t.Fatalf("got %d structs, want %d", len(pkg.Structs), tc.want)
			}
		})
	}
}

// Every //ggen:generate line of a declaration counts: the group's and the
// spec's, and a second line on one type.
func TestParseFile_combinesDirectiveLines(t *testing.T) {
	t.Parallel()
	src := `package test

//ggen:generate
type (
	//ggen:generate marshal
	G1 struct {
		X int ` + "`" + `json:"x"` + "`" + `
	}
	G2 struct {
		X int ` + "`" + `json:"x"` + "`" + `
	}
)

//ggen:generate multierr
//ggen:generate unmarshal
type Two struct {
	X int ` + "`" + `json:"x"` + "`" + `
}
`
	res, err := ParseFile(writeGoFile(t, src), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]StructInfo, len(res.Structs))
	for _, s := range res.Structs {
		got[s.Name] = s
	}
	if g1 := got["G1"]; !g1.Marshal || g1.Unmarshal {
		t.Errorf("G1 Marshal=%v Unmarshal=%v, want true false", g1.Marshal, g1.Unmarshal)
	}
	if g2, ok := got["G2"]; !ok || g2.Marshal {
		t.Errorf("G2 present=%v Marshal=%v, want true false", ok, g2.Marshal)
	}
	if two := got["Two"]; !two.Unmarshal || !two.MultiErr {
		t.Errorf("Two Unmarshal=%v MultiErr=%v, want both", two.Unmarshal, two.MultiErr)
	}
}

func TestParseFile_unknownTokenOnSecondDirectiveLine(t *testing.T) {
	t.Parallel()
	src := `package test

//ggen:generate
//ggen:generate marhsal
type T struct {
	X int ` + "`" + `json:"x"` + "`" + `
}
`
	_, err := ParseFile(writeGoFile(t, src), nil)
	if err == nil || !strings.Contains(err.Error(), `test.go:4:17: unknown //ggen:generate token "marhsal"`) {
		t.Fatalf("err = %v, want the positioned unknown-token error", err)
	}
}

// A promoted field that a shallower member not on the wire shadows at the Go
// level is rejected: the emitters write `result.<GoName>`, which Go resolves
// to the shadowing member.
func TestParsePackage_shadowedPromotedField(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, src, want string }{
		{"ignored_field", `
type User struct {
	Password string ` + "`json:\"password\"`" + `
}

//ggen:generate
type Public struct {
	User
	Password string ` + "`json:\"-\"`" + `
}
`, "promoted field User.Password"},
		{"embedded_name", `
type Inner struct {
	A int ` + "`json:\"a\"`" + `
}

type Holder struct {
	Inner int ` + "`json:\"inner\"`" + `
}

//ggen:generate
type Shadow struct {
	Inner
	Holder
}
`, "promoted field Holder.Inner"},
		{"method", `
type Base struct {
	Name string ` + "`json:\"name\"`" + `
}

//ggen:generate
type Named struct {
	Base
}

func (Named) Name() string { return "" }
`, "promoted field Base.Name"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeFile(t, dir, "go.mod", "module example.com/m\n\ngo 1.27\n")
			writeFile(t, dir, "m.go", "package m\n"+c.src)
			_, err := ParsePackage(dir)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

// A promoted field Go resolves to keeps parsing.
func TestParsePackage_promotedFieldReachable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/m\n\ngo 1.27\n")
	writeFile(t, dir, "m.go", `package m

type Deep struct {
	ID int `+"`json:\"id\"`"+`
}

type Mid struct {
	Deep
	Note string `+"`json:\"note\"`"+`
}

//ggen:generate
type Top struct {
	Mid
}
`)
	pkg, err := ParsePackage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(pkg.Structs[0].Fields); n != 2 {
		t.Fatalf("got %d fields, want 2", n)
	}
}

// A byte slice behind any number of pointers is bytes, so the container
// options are refused on it as on `*[]byte`.
func TestParseFile_pointerPointerBytes(t *testing.T) {
	t.Parallel()
	src := "package test\n//ggen:generate\ntype B struct {\n\tA **[]byte `json:\"a\"`\n\tI **[]byte `json:\"i\" pipe:\"inner:lte=3\"`\n\tH ***[]uint8 `json:\"h\" hint:\"8\"`\n}\n"
	res, err := ParseFile(writeGoFile(t, src), []string{"B"})
	for _, want := range []string{
		"B.I: `inner:` tag prefix is only valid on slice/array/map fields",
		"B.H: `hint` is only valid on slice/map fields",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
	for _, st := range res.Structs {
		for _, f := range st.Fields {
			if f.GoName == "A" && f.Kind != KindBytes {
				t.Errorf("A: kind %v, want KindBytes", f.Kind)
			}
		}
	}
}

func TestArrayLenFromType_integerLiterals(t *testing.T) {
	t.Parallel()
	for typ, want := range map[string]int{
		"[3]int": 3, "[0x2]int": 2, "[010]int": 8, "[0o10]int": 8,
		"[1_0]int": 10, "[0b11]int": 3, "[n]int": 0, "[]int": 0,
	} {
		if got := ArrayLenFromType(typ); got != want {
			t.Errorf("ArrayLenFromType(%q) = %d, want %d", typ, got, want)
		}
		if _, _, got := StripOneContainer(typ); got != want {
			t.Errorf("StripOneContainer(%q) length = %d, want %d", typ, got, want)
		}
	}
}
