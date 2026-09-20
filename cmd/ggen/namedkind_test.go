package main

import (
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/gen/model"
)

// A named type over a primitive (`type Priority string`) reports KindStruct at
// its use sites. Every rule emitter has to resolve the underlying kind through
// effectiveKind, or the check either fails to compile (raw token / uncast
// argument) or — worse — emits nothing at all.
func TestNamedPrimitive_RulesResolveUnderlying(t *testing.T) {
	gen := func(goType string, kind model.TypeKind, steps []model.Step, seed map[string]model.TypeKind) string {
		t.Helper()
		// generate() only seeds the globals when they are nil, so a second call
		// in the same test would otherwise reuse the first call's namedKinds.
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
		// The regression: `if kind == KindString {…} else if IsNumeric(kind) {…}`
		// with no else silently dropped the rule for KindStruct.
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

// zeroLit backs `nullzero` and the slice-element pre-grow. A named primitive is
// not a composite type, so `Pri{}` does not compile.
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

// A pointer to a container hands its pointee to the leaf decode, which
// appends into / fills whatever it is given, so the seed empties the leaf at
// every depth — the same shape a pointer chain gets as a map value or a
// []**T element, where no entry reset exists.
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

	// A pointer to a scalar is never seeded — the scan overwrites the leaf.
	if s := gen("*int"); strings.Contains(s, "v = (*result.C)") {
		t.Errorf("pointer-to-scalar must not be seeded:\n%s", s)
	}
}

// A field of a named primitive decodes/encodes INLINE — the underlying scan
// plus a conversion — instead of calling the type's own generated methods.
// The conversion is free (identical underlying type), the call was not: it
// forfeits the inline window scan and, for an UNANNOTATED named type, there
// were no methods at all so the field fell through to encoding/json.
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
		// No alias StructInfo at all — the type carries no generated methods.
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
		// way to escape one type and not the rest — inlining it with the
		// PARENT's flags would silently drop that.
		esc := aliasPri
		esc.Name, esc.HTMLEscape = "Esc", true
		s := gen(esc, model.FieldInfo{GoType: "Esc", Kind: model.KindStruct, NamedPrims: map[string]model.TypeKind{"Esc": model.KindString}})
		if !strings.Contains(s, "result.S.DecodeFrom(") {
			t.Errorf("flagged alias must keep its own decoder:\n%s", s)
		}
	})
}
