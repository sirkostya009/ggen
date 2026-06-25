package main

import (
	"bytes"
	"fmt"
	"strings"
)

// Multi-shape decode dispatch (the `pipe:` `/` variants). When a field's
// decode stage carries a converter variant, ggen peeks the JSON first byte and
// routes to the single variant claiming it: `.` native, `nullzero` (null →
// zero), or `@Conv` (scan input W, call func(W) → T). Encode is untouched.

// fieldHasConverter reports whether f needs shape-dispatch decode. Native/
// nullzero-only fields take the ordinary decode path + NullZero flag.
func fieldHasConverter(f FieldInfo) bool {
	for _, v := range f.Variants {
		if v.Kind == VariantConvert {
			return true
		}
	}
	return false
}

// kindShapeBytes returns the JSON first-byte case labels a kind's natural wire
// shape claims, as Go rune literals. Empty => the kind has no single shape
// (any / raw) and cannot participate in shape dispatch.
func kindShapeBytes(k TypeKind, format string) []string {
	switch k {
	case KindString, KindTime, KindDuration, KindNetIP, KindNetipAddr,
		KindNetipPrefix, KindURL, KindBigFloat, KindBigRat:
		return []string{"'\"'"}
	case KindBytes:
		if format == "array" {
			return []string{"'['"}
		}
		return []string{"'\"'"}
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64,
		KindUint, KindUint8, KindUint16, KindUint32, KindUint64,
		KindFloat32, KindFloat64, KindBigInt:
		return []string{"'-'", "'0'", "'1'", "'2'", "'3'", "'4'", "'5'", "'6'", "'7'", "'8'", "'9'"}
	case KindBool:
		return []string{"'t'", "'f'"}
	case KindStruct, KindMap:
		return []string{"'{'"}
	case KindSlice, KindArray:
		return []string{"'['"}
	}
	return nil
}

// variantCaseBytes returns the case labels a single variant claims.
func variantCaseBytes(f FieldInfo, v Variant) []string {
	switch v.Kind {
	case VariantNullZero:
		return []string{"'n'"}
	case VariantNative:
		return kindShapeBytes(f.Kind, f.Format)
	case VariantConvert:
		return kindShapeBytes(v.InKind, "")
	}
	return nil
}

// checkVariantShapes verifies the decode variants on f claim disjoint JSON
// shapes (one variant per shape) and that each shape-dispatchable variant
// resolves to a concrete first byte. Returns a *richError on conflict.
func checkVariantShapes(f FieldInfo) error {
	if !fieldHasConverter(f) {
		return nil
	}
	seen := map[string]string{} // case-byte → variant label
	label := func(v Variant) string {
		switch v.Kind {
		case VariantNullZero:
			return "nullzero"
		case VariantNative:
			return "native (" + f.GoType + ")"
		default:
			return "@" + v.FuncName
		}
	}
	for _, v := range f.Variants {
		bs := variantCaseBytes(f, v)
		if len(bs) == 0 {
			return &richError{
				Msg:      fmt.Sprintf("%s.%s: decode variant %s has no single JSON shape to dispatch on", f.StructName, f.GoName, label(v)),
				CodeSpan: "@" + v.FuncName,
			}
		}
		for _, c := range bs {
			if prev, dup := seen[c]; dup {
				return &richError{
					Msg:      fmt.Sprintf("%s.%s: decode variants %s and %s both claim the same JSON shape", f.StructName, f.GoName, prev, label(v)),
					CodeSpan: "@" + v.FuncName,
				}
			}
			seen[c] = label(v)
		}
	}
	return nil
}

// nativeVariantField strips f to a pure-decode copy for the native case body:
// no variants, no null-as-zero, no outer pipe (the outer value stage runs once
// after dispatch). Container-level dive/keys rules are kept.
func nativeVariantField(f FieldInfo) FieldInfo {
	nf := f
	nf.Variants = nil
	nf.NullZero = false
	nf.Pipe = nil
	nf.Validation = nil
	nf.Mods = nil
	return nf
}

// converterInputField builds the synthetic FieldInfo describing a converter's
// input type W, so renderField/renderStreamField can scan it into a temp.
func converterInputField(f FieldInfo, v Variant) FieldInfo {
	return FieldInfo{
		GoName:     f.GoName,
		StructName: f.StructName,
		JSONName:   f.JSONName,
		GoType:     v.InType,
		Kind:       v.InKind,
	}
}

func convCall(v Variant) string {
	if v.PkgName != "" {
		return v.PkgName + "." + v.FuncName
	}
	return v.FuncName
}

// renderVariantDispatch emits the bytes-path shape dispatch for f into ref,
// advancing posVar past the consumed value. The outer value stage runs after
// (caller's validateAndMod).
func renderVariantDispatch(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	field := fieldLit(f)
	inlineSkipWS(b, posVar)
	fmt.Fprintf(b, "if %s >= len(data) {\nreturn result, %s, scan.ErrUnexpectedEnd\n}\n", posVar, posVar)
	fmt.Fprintf(b, "switch data[%s] {\n", posVar)
	for idx, v := range f.Variants {
		labels := strings.Join(variantCaseBytes(f, v), ", ")
		fmt.Fprintf(b, "case %s:\n", labels)
		switch v.Kind {
		case VariantNullZero:
			fmt.Fprintf(b, "if %s+4 > len(data) || data[%s+1] != 'u' || data[%s+2] != 'l' || data[%s+3] != 'l' {\nreturn result, %s, scan.ErrBadLiteral\n}\n%s += 4\n%s = %s\n",
				posVar, posVar, posVar, posVar, posVar, posVar, ref, zeroLit(f.GoType, f.Kind))
		case VariantNative:
			renderField(b, nativeVariantField(f), ref, posVar)
		case VariantConvert:
			tmp := fmt.Sprintf("_cv%d", idx)
			fmt.Fprintf(b, "var %s %s\n", tmp, v.InType)
			renderField(b, converterInputField(f, v), tmp, posVar)
			emitConvAssign(b, v, ref, tmp, posVar)
		}
	}
	fmt.Fprintf(b, "default:\nreturn result, %s, scan.ErrBadValue\n}\n", posVar)
	_ = field
}

// renderVariantDispatchStream is the stream-path counterpart.
func renderVariantDispatchStream(f FieldInfo, ref, posVar string) string {
	b := getSmall()
	defer putSmall(b)
	field := fieldLit(f)
	b.WriteString(streamReadMore(field, "0", false))
	b.WriteString("switch s.Bytes()[s.Pos] {\n")
	for idx, v := range f.Variants {
		labels := strings.Join(variantCaseBytes(f, v), ", ")
		fmt.Fprintf(b, "case %s:\n", labels)
		switch v.Kind {
		case VariantNullZero:
			rmKi := strings.Replace(streamReadMore(field, "0", false), "if s.Pos >=", "if s.Pos+ki >=", 1)
			fmt.Fprintf(b, "for ki := 1; ki < 4; ki++ {\n%sif s.Bytes()[s.Pos+ki] != \"null\"[ki] {\nreturn result, decode.NewParseErr(%s, s.Pos, scan.ErrBadLiteral)\n}\n}\ns.Pos += 4\n%s = %s\n",
				rmKi, field, ref, zeroLit(f.GoType, f.Kind))
		case VariantNative:
			b.WriteString(renderStreamField(nativeVariantField(f), ref, posVar))
		case VariantConvert:
			tmp := fmt.Sprintf("_cv%d", idx)
			fmt.Fprintf(b, "var %s %s\n", tmp, v.InType)
			b.WriteString(renderStreamField(converterInputField(f, v), tmp, posVar))
			emitConvAssignStream(b, v, ref, tmp)
		}
	}
	fmt.Fprintf(b, "default:\nreturn result, decode.NewParseErr(%s, s.Pos, scan.ErrBadValue)\n}\n", field)
	return b.String()
}

// emitConvAssign emits the converter call + assignment for the bytes path.
func emitConvAssign(b *bytes.Buffer, v Variant, ref, tmp, posVar string) {
	call := convCall(v)
	if !v.Fallible {
		fmt.Fprintf(b, "%s = %s(%s)\n", ref, call, tmp)
		return
	}
	if v.BoolForm {
		modErr := fmt.Sprintf("&validation.ModError{%sName: %q, Msg: %q, Value: %s}", posLit(posVar), v.FuncName, v.Msg, tmp)
		fmt.Fprintf(b, "if cv, ok := %s(%s); !ok {\nreturn result, %s, %s\n} else {\n%s = cv\n}\n", call, tmp, posVar, modErr, ref)
		return
	}
	fmt.Fprintf(b, "if cv, err := %s(%s); err != nil {\nreturn result, %s, err\n} else {\n%s = cv\n}\n", call, tmp, posVar, ref)
}

// emitConvAssignStream is the stream-path counterpart (2-tuple returns).
func emitConvAssignStream(b *bytes.Buffer, v Variant, ref, tmp string) {
	call := convCall(v)
	if !v.Fallible {
		fmt.Fprintf(b, "%s = %s(%s)\n", ref, call, tmp)
		return
	}
	if v.BoolForm {
		modErr := fmt.Sprintf("&validation.ModError{Pos: s.Offset(), Name: %q, Msg: %q, Value: %s}", v.FuncName, v.Msg, tmp)
		fmt.Fprintf(b, "if cv, ok := %s(%s); !ok {\nreturn result, %s\n} else {\n%s = cv\n}\n", call, tmp, modErr, ref)
		return
	}
	fmt.Fprintf(b, "if cv, err := %s(%s); err != nil {\nreturn result, err\n} else {\n%s = cv\n}\n", call, tmp, ref)
}
