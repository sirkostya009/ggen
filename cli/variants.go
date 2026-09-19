package main

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/sirkostya009/ggen/gen/model"
)

// Multi-shape decode dispatch (the `pipe:` `/` variants). When a field's
// decode stage carries a converter variant, ggen peeks the JSON first byte and
// routes to the single variant claiming it: `.` native, `nullzero` (null →
// zero), or `@Conv` (scan input W, call func(W) → T). Encode is untouched.

// nativeVariantField strips f to a pure-decode copy for the native case body:
// no variants, no null-as-zero, no outer pipe (the outer value stage runs once
// after dispatch). Container-level dive/keys rules are kept.
func nativeVariantField(f model.FieldInfo) model.FieldInfo {
	nf := f
	nf.Variants = nil
	nf.NullZero = false
	nf.Pipe = nil
	nf.Validation = nil
	nf.Mods = nil
	return nf
}

// converterInputField builds the synthetic FieldInfo describing a converter's
// input type W, so renderField/renderStreamField can scan it into a temp. It
// is shaped like a field of type W: a pointer input takes the pointer path
// (null → nil, else a fresh leaf — the temp is a known-nil local, hence
// TargetNil) and NamedPrims carries W's named-primitive resolution.
func converterInputField(f model.FieldInfo, v model.Variant) model.FieldInfo {
	in := model.FieldInfo{
		GoName:           f.GoName,
		StructName:       f.StructName,
		JSONName:         f.JSONName,
		GoType:           v.InType,
		Kind:             v.InKind,
		NamedPrims:       f.NamedPrims,
		Copy:             f.Copy,
		AllowInvalidUTF8: f.AllowInvalidUTF8,
	}
	if v.InPointer {
		in.Pointer = true
		in.PointeeType = strings.TrimPrefix(v.InType, "*")
		in.TargetNil = true
	}
	return in
}

func convCall(v model.Variant) string {
	if v.PkgName != "" {
		return v.PkgName + "." + v.FuncName
	}
	return v.FuncName
}

// renderVariantDispatch emits the bytes-path shape dispatch for f into ref,
// advancing posVar past the consumed value. The outer value stage runs after
// (caller's validateAndMod).
func renderVariantDispatch(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	field := fieldLit(f)
	inlineSkipWS(b, posVar)
	fmt.Fprintf(b, "if %s >= len(data) {\nreturn result, %s, ggen.NewParseErr(%s, %s, ggen.ErrUnexpectedEnd)\n}\n", posVar, posVar, field, posVar)
	fmt.Fprintf(b, "switch data[%s] {\n", posVar)
	for idx, v := range f.Variants {
		labels := strings.Join(model.VariantCaseBytes(f, v, effectiveKind), ", ")
		fmt.Fprintf(b, "case %s:\n", labels)
		switch v.Kind {
		case model.VariantNullZero:
			fmt.Fprintf(
				b,
				"if %s+4 > len(data) || data[%s+1] != 'u' || data[%s+2] != 'l' || data[%s+3] != 'l' {\nreturn result, %s, ggen.NewParseErr(%s, %s, ggen.ErrBadLiteral)\n}\n%s += 4\n%s = %s\n",
				posVar,
				posVar,
				posVar,
				posVar,
				posVar,
				field,
				posVar,
				posVar,
				ref,
				zeroLit(f.GoType, f.Kind),
			)
		case model.VariantNative:
			renderField(b, nativeVariantField(f), ref, posVar)
		case model.VariantConvert:
			tmp := fmt.Sprintf("conv%d", idx)
			fmt.Fprintf(b, "var %s %s\n", tmp, v.InType)
			renderField(b, converterInputField(f, v), tmp, posVar)
			emitConvAssign(b, v, field, ref, tmp, posVar)
		}
	}
	fmt.Fprintf(b, "default:\nreturn result, %s, ggen.NewParseErr(%s, %s, ggen.ErrBadValue)\n}\n", posVar, field, posVar)
}

// renderVariantDispatchStream is the stream-path counterpart.
func renderVariantDispatchStream(f model.FieldInfo, ref, posVar string) string {
	b := getSmall()
	defer putSmall(b)
	field := fieldLit(f)
	b.WriteString(streamReadMore(field, "0", false, "ggen.ErrUnexpectedEnd"))
	b.WriteString("switch s.Bytes()[s.Pos] {\n")
	for idx, v := range f.Variants {
		labels := strings.Join(model.VariantCaseBytes(f, v, effectiveKind), ", ")
		fmt.Fprintf(b, "case %s:\n", labels)
		switch v.Kind {
		case model.VariantNullZero:
			rmKi := strings.Replace(streamReadMore(field, "0", false, "ggen.ErrBadLiteral"), "if s.Pos >=", "if s.Pos+ki >=", 1)
			fmt.Fprintf(
				b,
				"for ki := 1; ki < 4; ki++ {\n%sif s.Bytes()[s.Pos+ki] != \"null\"[ki] {\nreturn result, ggen.NewParseErr(%s, s.Offset(), ggen.ErrBadLiteral)\n}\n}\ns.Pos += 4\n%s = %s\n",
				rmKi,
				field,
				ref,
				zeroLit(f.GoType, f.Kind),
			)
		case model.VariantNative:
			b.WriteString(renderStreamField(nativeVariantField(f), ref, posVar))
		case model.VariantConvert:
			tmp := fmt.Sprintf("conv%d", idx)
			fmt.Fprintf(b, "var %s %s\n", tmp, v.InType)
			b.WriteString(renderStreamField(converterInputField(f, v), tmp, posVar))
			emitConvAssignStream(b, v, field, ref, tmp)
		}
	}
	fmt.Fprintf(b, "default:\nreturn result, ggen.NewParseErr(%s, s.Offset(), ggen.ErrBadValue)\n}\n", field)
	return b.String()
}

// emitConvAssign emits the converter call + assignment for the bytes path.
func emitConvAssign(b *bytes.Buffer, v model.Variant, field, ref, tmp, posVar string) {
	call := convCall(v)
	if !v.Fallible {
		fmt.Fprintf(b, "%s = %s(%s)\n", ref, call, tmp)
		return
	}
	if v.BoolForm {
		// ModError is a parse error — wrap so it carries the field path like
		// every other decode failure (mod_error.go doc).
		modErr := fmt.Sprintf("ggen.NewParseErr(%s, %s, &ggen.ModError{%sName: %q, Msg: %q, Value: %s})", field, posVar, posLit(posVar), v.FuncName, v.Msg, tmp)
		fmt.Fprintf(b, "if cv, ok := %s(%s); !ok {\nreturn result, %s, %s\n} else {\n%s = cv\n}\n", call, tmp, posVar, modErr, ref)
		return
	}
	// A converter's own error is foreign — wrap it so it carries the field
	// path and offset every other decode failure does (errors.As still
	// reaches the converter's error through the ParseError).
	fmt.Fprintf(
		b,
		"if cv, err := %s(%s); err != nil {\nreturn result, %s, ggen.NewParseErr(%s, %s, err)\n} else {\n%s = cv\n}\n",
		call,
		tmp,
		posVar,
		field,
		posVar,
		ref,
	)
}

// emitConvAssignStream is the stream-path counterpart (2-tuple returns).
func emitConvAssignStream(b *bytes.Buffer, v model.Variant, field, ref, tmp string) {
	call := convCall(v)
	if !v.Fallible {
		fmt.Fprintf(b, "%s = %s(%s)\n", ref, call, tmp)
		return
	}
	if v.BoolForm {
		modErr := fmt.Sprintf("ggen.NewParseErr(%s, s.Offset(), &ggen.ModError{Pos: s.Offset(), Name: %q, Msg: %q, Value: %s})", field, v.FuncName, v.Msg, tmp)
		fmt.Fprintf(b, "if cv, ok := %s(%s); !ok {\nreturn result, %s\n} else {\n%s = cv\n}\n", call, tmp, modErr, ref)
		return
	}
	fmt.Fprintf(b, "if cv, err := %s(%s); err != nil {\nreturn result, ggen.NewParseErr(%s, s.Offset(), err)\n} else {\n%s = cv\n}\n", call, tmp, field, ref)
}
