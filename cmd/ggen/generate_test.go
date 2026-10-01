package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/gen/model"
)

// generate is a test helper around generateTo that returns the output as a
// []byte, so tests can check it in memory.
func generate(pkg string, structs []model.StructInfo) ([]byte, error) {
	var buf bytes.Buffer
	if err := generateTo(&buf, pkg, pkg, structs); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func TestGenerate_basic(t *testing.T) {
	structs := []model.StructInfo{{
		Name: "TestStruct",
		Fields: []model.FieldInfo{
			{
				GoName: "Name", JSONName: "name", GoType: "string", Kind: model.KindString,
				Validation: []model.ValidationRule{{Name: "required"}},
			},
			{GoName: "Count", JSONName: "count", GoType: "int", Kind: model.KindInt},
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
		`ggen.Required`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in output", want)
		}
	}
}

func TestGenerate_newValidators(t *testing.T) {
	structs := []model.StructInfo{{
		Name: "V",
		Fields: []model.FieldInfo{
			{
				GoName: "Code", JSONName: "code", GoType: "string", Kind: model.KindString,
				Validation: []model.ValidationRule{{Name: "alphanum"}},
			},
			{
				GoName: "URL", JSONName: "url", GoType: "string", Kind: model.KindString,
				Validation: []model.ValidationRule{{Name: "url"}},
			},
			{
				GoName: "Role", JSONName: "role", GoType: "string", Kind: model.KindString,
				Validation: []model.ValidationRule{{Name: "oneof", Value: "a|b|c"}},
			},
			{
				GoName: "N", JSONName: "n", GoType: "int", Kind: model.KindInt,
				Validation: []model.ValidationRule{
					{Name: "gte", Value: "0"},
					{Name: "lte", Value: "100"},
					{Name: "gt", Value: "5"},
				},
			},
		},
	}}
	code, err := generate("p", structs)
	if err != nil {
		t.Fatal(err)
	}
	s := string(code)
	for _, want := range []string{
		`ggen.IsAlphanum`,
		`ggen.IsURL`,
		`case "a", "b", "c"`,
		`ggen.GT`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in generated code", want)
		}
	}
}

// A rune is one Unicode character. In UTF-8 it takes 1 to 4 bytes, so the
// byte length of a string bounds its rune count. Rune rules use byte-length
// checks to skip the utf8.RuneCountInString walk when the length alone
// decides. Without multierr, they skip the walk entirely when an earlier rule
// already proved the string is ASCII, where runes equal bytes.
func TestGenerate_runeGates(t *testing.T) {
	gen := func(multiErr bool, steps []model.Step) string {
		t.Helper()
		code, err := generate("p", []model.StructInfo{{
			Name:     "V",
			MultiErr: multiErr,
			Fields: []model.FieldInfo{{
				GoName: "S", JSONName: "s", GoType: "string", Kind: model.KindString,
				Pipe: steps, MultiErr: multiErr,
			}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return string(code)
	}

	t.Run("minrunes_gated", func(t *testing.T) {
		// minrunes=3: len<3 always fails and len>=9 (4*3-3) always passes.
		// Only lengths in [3,9) count runes.
		s := gen(false, []model.Step{{V: model.ValidationRule{Name: "minrunes", Value: "3"}}})
		if !strings.Contains(s, "if len(result.S) < 3 {") {
			t.Errorf("missing fail-free len gate:\n%s", s)
		}
		if !strings.Contains(s, "else if len(result.S) < 9 {") {
			t.Errorf("missing ambiguous-band gate (4n-3=9):\n%s", s)
		}
	})

	t.Run("maxrunes_gated", func(t *testing.T) {
		// maxrunes=5: len<=5 always passes and len>20 (4*5) always fails.
		// Only lengths in (5,20] count runes.
		s := gen(false, []model.Step{{V: model.ValidationRule{Name: "maxrunes", Value: "5"}}})
		if !strings.Contains(s, "if len(result.S) > 20 {") {
			t.Errorf("missing fail-free len gate (4n=20):\n%s", s)
		}
		if !strings.Contains(s, "else if len(result.S) > 5 {") {
			t.Errorf("missing ambiguous-band gate:\n%s", s)
		}
	})

	t.Run("minrunes_1_collapses", func(t *testing.T) {
		// minrunes=1: the range [1,1) is empty, so only the len<1 check
		// (the empty string) is left, with no rune count.
		s := gen(false, []model.Step{{V: model.ValidationRule{Name: "minrunes", Value: "1"}}})
		if !strings.Contains(s, "if len(result.S) < 1 {") {
			t.Errorf("minrunes=1 should be a bare len gate:\n%s", s)
		}
		// The only rune count is for Got: in the fail branch, never in a
		// length range.
		if strings.Contains(s, "else if") {
			t.Errorf("minrunes=1 should have no ambiguous band:\n%s", s)
		}
	})

	t.Run("ascii_precedes_drops_walk", func(t *testing.T) {
		// alphanum runs before maxrunes without multierr, so the string is
		// ASCII and its rune count is its length. No walk.
		s := gen(false, []model.Step{
			{V: model.ValidationRule{Name: "alphanum"}},
			{V: model.ValidationRule{Name: "maxrunes", Value: "16"}},
		})
		if strings.Contains(s, "utf8.RuneCountInString") {
			t.Errorf("ascii-preceded rune rule must not walk:\n%s", s)
		}
		if !strings.Contains(s, "if len(result.S) > 16 {") {
			t.Errorf("expected a direct len comparison:\n%s", s)
		}
	})

	t.Run("ascii_after_does_not_drop", func(t *testing.T) {
		// alphanum after the rune rule proves nothing yet. The string is not
		// known to be ASCII, so the length checks and the walk stay.
		s := gen(false, []model.Step{
			{V: model.ValidationRule{Name: "maxrunes", Value: "16"}},
			{V: model.ValidationRule{Name: "alphanum"}},
		})
		if !strings.Contains(s, "utf8.RuneCountInString") {
			t.Errorf("rune rule before the ascii rule should still gate+walk:\n%s", s)
		}
	})

	t.Run("multierr_keeps_walk", func(t *testing.T) {
		// Under multierr, a failed alphanum does not stop maxrunes from
		// running on non-ASCII input. The ASCII shortcut is unsafe, so the
		// length checks and the walk stay.
		s := gen(true, []model.Step{
			{V: model.ValidationRule{Name: "alphanum"}},
			{V: model.ValidationRule{Name: "maxrunes", Value: "16"}},
		})
		if !strings.Contains(s, "utf8.RuneCountInString") {
			t.Errorf("multierr must keep the walk (tier c unsafe):\n%s", s)
		}
	})
}

func TestOutputWrittenNextToSource(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "nested", "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	src := `package pkg

type Msg struct {
	Text string ` + "`" + `json:"text"` + "`" + `
}
`
	goFile := filepath.Join(sub, "msg.go")
	if err := os.WriteFile(goFile, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := model.ParseFile(goFile, []string{"Msg"})

	structs, pkg := res.Structs, res.PkgName
	if err != nil {
		t.Fatal(err)
	}
	code, err := generate(pkg, structs)
	if err != nil {
		t.Fatal(err)
	}

	outFile := strings.TrimSuffix(goFile, ".go") + "_ggen.go"
	if err := os.WriteFile(outFile, code, 0o644); err != nil {
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

// The synthetic FieldInfos built for container elements and converter inputs
// must keep the parent's flags. That covers the inner levels of hint:, and
// allowinvalidutf8 and copy at every container level and on @Conv inputs.
func TestGenerate_syntheticFieldFlagPropagation(t *testing.T) {
	t.Run("hint_inner_levels", func(t *testing.T) {
		code, err := generate("p", []model.StructInfo{{
			Name: "H",
			Fields: []model.FieldInfo{{
				GoName: "Rows", JSONName: "rows", GoType: "[][]int",
				Kind: model.KindSlice, ElemKind: model.KindSlice, ElemType: "[]int",
				HintLen: 32, HintLevels: []int{8},
			}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		s := string(code)
		if !strings.Contains(s, "make([][]int, 0, 32)") {
			t.Errorf("outer hint not emitted:\n%s", s)
		}
		if !strings.Contains(s, "make([]int, 0, 8)") {
			t.Errorf("inner hint dropped by peelSliceField:\n%s", s)
		}
	})

	t.Run("allowinvalidutf8_nested_elem", func(t *testing.T) {
		code, err := generate("p", []model.StructInfo{{
			Name: "U", AllowInvalidUTF8: true,
			Fields: []model.FieldInfo{{
				GoName: "Nested", JSONName: "nested", GoType: "[][]string",
				Kind: model.KindSlice, ElemKind: model.KindSlice, ElemType: "[]string",
				HintLen: -1, AllowInvalidUTF8: true,
			}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(code), "ggen.String(data, i, true)") {
			t.Errorf("inner string elems re-validate UTF-8 despite allowinvalidutf8:\n%s", code)
		}
	})
}

// Checks on the emitted code for aliases, variants and omit rules.
func TestGenerate_round8Fixes(t *testing.T) {
	t.Run("container_alias_flags", func(t *testing.T) {
		// copy, htmlescape and allowinvalidutf8 must reach a slice alias.
		// AliasField is built before flags are applied, so they must be
		// copied onto it.
		code, err := generate("p", []model.StructInfo{{
			Name: "Tags", IsAlias: true, AliasKind: model.KindSlice,
			AliasUnderlying: "[]string", HTMLEscape: true, Copy: true, AllowInvalidUTF8: true,
			AliasField: model.FieldInfo{Kind: model.KindSlice, ElemType: "string", ElemKind: model.KindString, HintLen: -1},
		}})
		if err != nil {
			t.Fatal(err)
		}
		s := string(code)
		if strings.Contains(s, "unsafe.String(") {
			t.Errorf("copy dropped — element still aliases input:\n%s", s)
		}
		if strings.Contains(s, "ggen.String(data, i, true)") {
			t.Errorf("allowinvalidutf8 dropped — element still validates:\n%s", s)
		}
		if !strings.Contains(s, "ggen.AppendString(") {
			t.Errorf("htmlescape dropped — NoHTML append emitted:\n%s", s)
		}
	})

	t.Run("primitive_string_alias_copy", func(t *testing.T) {
		code, err := generate("p", []model.StructInfo{{
			Name: "S", IsAlias: true, AliasKind: model.KindString, AliasUnderlying: "string", Copy: true,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(code), "ggen.Detach(v, data)") {
			t.Errorf("copy alias missing Detach:\n%s", code)
		}
	})

	t.Run("converter_variant_keeps_native_null", func(t *testing.T) {
		// For *int with a converter variant, the native case must still
		// claim 'n' and decode null to nil, not return an error.
		code, err := generate("p", []model.StructInfo{{
			Name: "V",
			Fields: []model.FieldInfo{{
				GoName: "P", JSONName: "p", GoType: "*int", Kind: model.KindInt,
				Pointer: true, PointeeType: "int", HintLen: -1,
				Variants: []model.Variant{
					{Kind: model.VariantNative},
					{Kind: model.VariantConvert, FuncName: "FromStr", InType: "string", InKind: model.KindString},
				},
			}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(code), "'n'") {
			t.Errorf("native variant does not claim 'n':\n%s", code)
		}
	})

	t.Run("named_prim_omit", func(t *testing.T) {
		// A named primitive is KindStruct where it is used. omitzero must
		// compare it to its underlying zero value, not to a composite
		// literal, and omitempty must still apply.
		old := namedKinds
		namedKinds = map[string]model.TypeKind{"Score": model.KindInt, "Name": model.KindString}
		defer func() { namedKinds = old }()
		f := model.FieldInfo{GoName: "S", GoType: "Score", Kind: model.KindStruct, OmitZero: true}
		if got := fieldSkipExpr(f, "s.S"); got != "s.S != 0" {
			t.Errorf("omitzero named int: got %q", got)
		}
		f = model.FieldInfo{GoName: "N", GoType: "Name", Kind: model.KindStruct, OmitEmpty: true}
		if got := fieldSkipExpr(f, "s.N"); got != `s.N != ""` {
			t.Errorf("omitempty named string: got %q", got)
		}
	})
}
