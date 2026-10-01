package main

import (
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/gen/model"
)

// A named type over a primitive, such as `type Priority string`, has
// KindStruct where it is used. Every rule emitter must get the underlying kind
// through effectiveKind. Otherwise the check either does not compile, because
// of an unquoted value or a missing conversion, or emits no check at all.
func TestNamedPrimitive_RulesResolveUnderlying(t *testing.T) {
	gen := func(goType string, kind model.TypeKind, steps []model.Step, seed map[string]model.TypeKind) string {
		t.Helper()
		// generate() seeds the globals only when they are nil. Without this
		// reset, a second call would reuse the first call's namedKinds.
		generatedTypes, namedKinds, cyclicTypes = nil, nil, nil
		code, err := generate("p", []model.StructInfo{{
			Name: "V",
			Fields: []model.FieldInfo{{
				GoName: "S", JSONName: "s", GoType: goType, Kind: kind, Pipe: steps,
				NamedPrims: seed,
			}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return string(code)
	}
	strSeed := map[string]model.TypeKind{"Pri": model.KindString}
	intSeed := map[string]model.TypeKind{"Cnt": model.KindInt}

	t.Run("oneof_quotes_through_alias", func(t *testing.T) {
		s := gen("Pri", model.KindStruct, []model.Step{{V: model.ValidationRule{Name: "oneof", Value: "low|high"}}}, strSeed)
		if !strings.Contains(s, `case "low", "high":`) {
			t.Errorf("oneof on a named string must emit quoted cases:\n%s", s)
		}
	})

	t.Run("eq_neq_emit_at_all", func(t *testing.T) {
		// A branch like `if kind == KindString {…} else if IsNumeric(kind) {…}`
		// with no else would emit nothing for KindStruct.
		s := gen("Pri", model.KindStruct, []model.Step{
			{V: model.ValidationRule{Name: "eq", Value: "low"}},
			{V: model.ValidationRule{Name: "neq", Value: "high"}},
		}, strSeed)
		if !strings.Contains(s, "ggen.EqError") || !strings.Contains(s, "ggen.NeqError") {
			t.Errorf("eq/neq on a named string emitted no check:\n%s", s)
		}
		n := gen("Cnt", model.KindStruct, []model.Step{{V: model.ValidationRule{Name: "eq", Value: "3"}}}, intSeed)
		if !strings.Contains(n, "ggen.EqError") {
			t.Errorf("eq on a named int emitted no check:\n%s", n)
		}
	})

	t.Run("string_apis_are_cast", func(t *testing.T) {
		s := gen("Pri", model.KindStruct, []model.Step{
			{V: model.ValidationRule{Name: "maxrunes", Value: "4"}},
			{V: model.ValidationRule{Name: "contains", Value: "x"}},
			{V: model.ValidationRule{Name: "url"}},
		}, strSeed)
		for _, want := range []string{
			"utf8.RuneCountInString(string(result.S))",
			"strings.Contains(string(result.S)",
			"ggen.IsURL(string(result.S))",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("missing cast %q:\n%s", want, s)
			}
		}
	})

	t.Run("plain_string_field_is_not_cast", func(t *testing.T) {
		s := gen("string", model.KindString, []model.Step{{V: model.ValidationRule{Name: "url"}}}, nil)
		if strings.Contains(s, "string(result.S)") {
			t.Errorf("a plain string field must not get a redundant conversion:\n%s", s)
		}
	})
}

// zeroLit is used by `nullzero` and when a slice grows by one element before
// decoding into it. A named primitive is not a composite type, so `Pri{}`
// does not compile.
func TestNamedPrimitive_ZeroLit(t *testing.T) {
	prev := namedKinds
	namedKinds = map[string]model.TypeKind{"Pri": model.KindString, "Cnt": model.KindInt, "Flag": model.KindBool}
	defer func() { namedKinds = prev }()

	for _, tc := range []struct {
		typ  string
		kind model.TypeKind
		want string
	}{
		{"Pri", model.KindStruct, `Pri("")`},
		{"Cnt", model.KindStruct, "Cnt(0)"},
		{"Flag", model.KindStruct, "Flag(false)"},
		{"string", model.KindString, `""`},
		{"int", model.KindInt, "0"},
		{"Other", model.KindStruct, "Other{}"},
		{"[]int", model.KindSlice, "nil"},
	} {
		if got := zeroLit(tc.typ, tc.kind); got != tc.want {
			t.Errorf("zeroLit(%q, %v) = %q, want %q", tc.typ, tc.kind, got, tc.want)
		}
	}
}

// For a pointer to a container, the leaf decode gets the pointed-to value and
// appends to or fills whatever it is given. So the seed empties the leaf at
// every pointer depth. A pointer chain as a map value or a []**T element gets
// the same shape, because nothing resets it on entry.
func TestPointerContainer_LeafSeedEmptied(t *testing.T) {
	gen := func(goType string) string {
		t.Helper()
		generatedTypes, namedKinds, cyclicTypes = nil, nil, nil
		depth, leaf := model.PointerDepth(goType)
		code, err := generate("p", []model.StructInfo{{
			Name: "V",
			Fields: []model.FieldInfo{{
				GoName: "C", JSONName: "c", GoType: goType,
				Pointer: depth > 0, PointeeType: goType[1:],
				Kind:     model.ResolveKind(leaf),
				ElemType: "int", ElemKind: model.KindInt,
			}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(strings.Fields(string(code)), " ")
	}

	for _, tc := range []struct{ typ, want string }{
		{"*[]int", "if result.C != nil { v = (*result.C)[:0] }"},
		{"**[]int", "if result.C != nil && (*result.C) != nil { v = (*(*result.C))[:0] }"},
		{"*map[string]int", "if result.C != nil { v = (*result.C) clear(v) }"},
		{"**map[string]int", "if result.C != nil && (*result.C) != nil { v = (*(*result.C)) clear(v) }"},
	} {
		s := gen(tc.typ)
		if !strings.Contains(s, tc.want) {
			t.Errorf("%s: missing seed %q:\n%s", tc.typ, tc.want, s)
		}
		if strings.Contains(s, "(*result.C) = (*result.C)[:0]") || strings.Contains(s, "clear((*result.C))") {
			t.Errorf("%s: entry reset duplicates the seed:\n%s", tc.typ, s)
		}
	}

	// A pointer to a scalar is never seeded. The scan overwrites the leaf.
	if s := gen("*int"); strings.Contains(s, "v = (*result.C)") {
		t.Errorf("pointer-to-scalar must not be seeded:\n%s", s)
	}
}

// A field of a named primitive decodes and encodes inline, with the scan for
// the underlying type plus a conversion, instead of calling the type's own
// generated methods. The conversion costs nothing because the underlying type
// is the same. A call would lose the inline scan. A named type without an
// annotation has no generated methods, so a call would fall back to
// encoding/json.
func TestNamedPrimitive_InlineDecode(t *testing.T) {
	gen := func(alias model.StructInfo, field model.FieldInfo) string {
		t.Helper()
		generatedTypes, namedKinds, cyclicTypes = nil, nil, nil
		field.GoName, field.JSONName = "S", "s"
		code, err := generate("p", []model.StructInfo{alias, {Name: "V", Fields: []model.FieldInfo{field}}})
		if err != nil {
			t.Fatal(err)
		}
		return string(code)
	}
	aliasPri := model.StructInfo{Name: "Pri", IsAlias: true, AliasKind: model.KindString, AliasUnderlying: "string"}

	t.Run("scans_underlying_and_converts", func(t *testing.T) {
		s := gen(aliasPri, model.FieldInfo{GoType: "Pri", Kind: model.KindStruct, NamedPrims: map[string]model.TypeKind{"Pri": model.KindString}})
		for _, want := range []string{"var namedS string", "result.S = Pri(namedS)"} {
			if !strings.Contains(s, want) {
				t.Errorf("missing %q:\n%s", want, s)
			}
		}
		if strings.Contains(s, "result.S.DecodeFrom(") {
			t.Errorf("field still delegates to the alias decoder:\n%s", s)
		}
		if strings.Contains(s, "size += result.S.JSONSize()") || strings.Contains(s, "s.S.AppendJSON(") {
			t.Errorf("encode side still delegates:\n%s", s)
		}
	})

	t.Run("unannotated_skips_encoding_json", func(t *testing.T) {
		// There is no alias StructInfo, so the type has no generated methods.
		s := gen(model.StructInfo{Name: "Unused", Fields: nil},
			model.FieldInfo{GoType: "Tag", Kind: model.KindStruct, NamedPrims: map[string]model.TypeKind{"Tag": model.KindString}})
		if strings.Contains(s, "json.Unmarshal") || strings.Contains(s, "json.Marshal") {
			t.Errorf("unannotated named primitive still routes through encoding/json:\n%s", s)
		}
		if !strings.Contains(s, "result.S = Tag(namedS)") {
			t.Errorf("missing inline conversion:\n%s", s)
		}
	})

	t.Run("alias_with_own_flags_keeps_its_methods", func(t *testing.T) {
		// `//ggen:generate htmlescape type HtmlString string` is the documented
		// way to HTML-escape one type and not the rest. Inlining it with the
		// parent's flags would lose that.
		esc := aliasPri
		esc.Name, esc.HTMLEscape = "Esc", true
		s := gen(esc, model.FieldInfo{GoType: "Esc", Kind: model.KindStruct, NamedPrims: map[string]model.TypeKind{"Esc": model.KindString}})
		if !strings.Contains(s, "result.S.DecodeFrom(") {
			t.Errorf("flagged alias must keep its own decoder:\n%s", s)
		}
	})
}
