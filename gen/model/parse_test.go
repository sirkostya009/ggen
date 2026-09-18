package model

import (
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
// name filter must error, not emit code for every exported struct.
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
	// Both promoted and explicit fields should appear.
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
	writeFile(t, dir, "a_ggen.go", `package p
// this should be ignored - contains bogus syntax we do not parse
func broken {`)
	writeFile(t, dir, "a_ggen_test.go", `package p
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
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// TestSQLNullGeneric covers the string-based path (ResolveKind + SQLNullSpec)
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

	// Non-primitive / malformed inners are not classified by the string path.
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

// Round-9 parse-layer pins.
func TestParseFile_round9(t *testing.T) {
	t.Run("tab_after_directive", func(t *testing.T) {
		// go:generate accepts tab separators; a tab used to silently drop
		// the whole annotation (no output, exit 0).
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
		// A depth-1 promoted field beats a depth-2 one (stdlib dominant-field
		// rule); the flat resolver used to drop BOTH → `{}` on the wire.
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
		// Invalid Go, but ggen walks syntax first: must diagnose, not
		// stack-overflow the generator.
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

	t.Run("map_value_sibling_generated", func(t *testing.T) {
		// A struct reached only through map[string]Inner used to miss
		// generatedTypes and fall to the json.Unmarshal fallback.
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

// A file's name constrains it the way go/build reads it; `_ggen` in the
// output name masks that rule, so the bucket key has to carry it.
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
		{"package p\n\nimport \"C\"\n", "native.go", "cgo"},
		{"package p\n\nimport \"C\"\n", "native_linux.go", "linux && cgo"},
	}
	for _, tc := range cases {
		af, err := parser.ParseFile(token.NewFileSet(), tc.file, tc.src, parser.ParseComments|parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		if got := fileBuildConstraint(af, filepath.Join("some", "dir", tc.file)); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.file, got, tc.want)
		}
	}
}

func TestParseFile_round10(t *testing.T) {
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
		// jsonv2 keeps the shallowest catch-all; the emitters read exactly
		// one, so the promoted one must be gone.
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
		// Distinct JSON names, one Go name: stdlib keeps both, ggen cannot
		// address them and must say so instead of dropping the pair.
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
		// Interfaces keep their shape: `interface{}` is `any`, and one with
		// methods carries whatever its dynamic value marshals as.
		for _, decl := range []string{"V interface{}", "V interface{ M() }"} {
			src := "package test\n//ggen:generate\ntype A struct{ " + decl + " `json:\"v\"` }\n"
			if _, err := ParseFile(writeGoFile(t, src), []string{"A"}); err != nil {
				t.Errorf("%s field: %v", decl, err)
			}
		}
	})
}

func TestParsePackage_docsDirectivesConsts(t *testing.T) {
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
	// Reached through an import: net/http's ConnState consts.
	if cs := pkg.Consts["net/http.ConnState"]; len(cs) == 0 || cs[0].Name != "StateNew" || cs[0].Value != "0" {
		t.Errorf("imported consts = %v", cs)
	}
}
