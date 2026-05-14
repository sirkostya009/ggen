package main

import (
	"fmt"
	"strings"
)

// renderAliasDecode emits the body of DecodeFrom for a primitive alias
// (e.g. `type HtmlString string`), a struct alias (`type LocalUUID
// uuid.UUID`), or a container alias (`type Tags []string`,
// `type Lookup map[string]int`, `type Tuple [3]int`).
func renderAliasDecode(s StructInfo) string {
	if s.AliasKind == KindStruct {
		return renderAliasStructDecode(s, false)
	}
	if s.AliasKind == KindSlice || s.AliasKind == KindMap || s.AliasKind == KindArray || s.AliasKind == KindBytes {
		return renderAliasContainerDecode(s, false)
	}
	var b strings.Builder
	b.WriteString("var result " + s.Name + "\n")
	switch s.AliasKind {
	case KindString:
		b.WriteString("v, k, err := scan.String(data, i)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "result = %s(v)\nreturn result, k, nil\n", s.Name)
	case KindBool:
		b.WriteString("v, k, err := scan.Bool(data, i)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "result = %s(v)\nreturn result, k, nil\n", s.Name)
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		b.WriteString("v, k, err := scan.Int64(data, i)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "result = %s(v)\nreturn result, k, nil\n", s.Name)
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		b.WriteString("v, k, err := scan.Uint64(data, i)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "result = %s(v)\nreturn result, k, nil\n", s.Name)
	case KindFloat32, KindFloat64:
		b.WriteString("v, k, err := scan.Float64(data, i)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "result = %s(v)\nreturn result, k, nil\n", s.Name)
	}
	return b.String()
}

// renderAliasStreamDecode is the io.Reader counterpart of
// renderAliasDecode.
func renderAliasStreamDecode(s StructInfo) string {
	if s.AliasKind == KindStruct {
		return renderAliasStructDecode(s, true)
	}
	if s.AliasKind == KindSlice || s.AliasKind == KindMap || s.AliasKind == KindArray || s.AliasKind == KindBytes {
		return renderAliasContainerDecode(s, true)
	}
	var b strings.Builder
	b.WriteString("var result " + s.Name + "\n")
	switch s.AliasKind {
	case KindString:
		b.WriteString("v, k, err := _s.String(i)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "result = %s(v)\nreturn result, k, nil\n", s.Name)
	case KindBool:
		b.WriteString("v, k, err := _s.Bool(i)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "result = %s(v)\nreturn result, k, nil\n", s.Name)
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		b.WriteString("v, k, err := _s.Int64(i)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "result = %s(v)\nreturn result, k, nil\n", s.Name)
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		b.WriteString("v, k, err := _s.Uint64(i)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "result = %s(v)\nreturn result, k, nil\n", s.Name)
	case KindFloat32, KindFloat64:
		b.WriteString("v, k, err := _s.Float64(i)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "result = %s(v)\nreturn result, k, nil\n", s.Name)
	}
	return b.String()
}

// renderAliasSize returns an upper-bound JSONSize body for the alias.
// Strings dominate length; numerics are bounded by their textual width.
func renderAliasSize(s StructInfo) string {
	if s.AliasKind == KindStruct {
		return renderAliasStructSize()
	}
	if s.AliasKind == KindSlice || s.AliasKind == KindMap || s.AliasKind == KindArray || s.AliasKind == KindBytes {
		return renderAliasContainerSize(s)
	}
	switch s.AliasKind {
	case KindString:
		// `*2 + 2` — worst-case escape ratio plus the surrounding quotes.
		return "return len(string(s))*2 + 2\n"
	case KindBool:
		return "return 5\n" // "false"
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		return "return 20\n" // -9223372036854775808
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		return "return 20\n" // 18446744073709551615
	case KindFloat32, KindFloat64:
		return "return 24\n" // strconv.AppendFloat 'g' max
	}
	return "return 0\n"
}

// renderAliasAppendJSON emits the body of AppendJSON for a primitive
// or struct alias.
func renderAliasAppendJSON(s StructInfo) string {
	if s.AliasKind == KindStruct {
		return renderAliasStructAppendJSON(s)
	}
	if s.AliasKind == KindSlice || s.AliasKind == KindMap || s.AliasKind == KindArray || s.AliasKind == KindBytes {
		return renderAliasContainerAppendJSON(s)
	}
	var b strings.Builder
	switch s.AliasKind {
	case KindString:
		// CALLER side: emit the opening quote, then the body via the
		// chosen string-append helper (which writes the closing quote).
		b.WriteString("dst = append(dst, '\"')\n")
		fmt.Fprintf(&b, "dst = %s(dst, string(s))\n", appendStrFn(s.HTMLEscape))
		b.WriteString("return dst, nil\n")
	case KindBool:
		fmt.Fprintf(&b, "return strconv.AppendBool(dst, bool(s)), nil\n")
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		fmt.Fprintf(&b, "return strconv.AppendInt(dst, int64(s), 10), nil\n")
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		fmt.Fprintf(&b, "return strconv.AppendUint(dst, uint64(s), 10), nil\n")
	case KindFloat32:
		fmt.Fprintf(&b, "return strconv.AppendFloat(dst, float64(s), 'g', -1, 32), nil\n")
	case KindFloat64:
		fmt.Fprintf(&b, "return strconv.AppendFloat(dst, float64(s), 'g', -1, 64), nil\n")
	}
	return b.String()
}

// renderAliasStructDecode emits DecodeFrom (or DecodeStreamFrom when
// stream==true) for a struct alias. Picks the cheapest delegation path
// the underlying type's method set supports:
//   - ggen-shaped DecodeFrom on the underlying → call directly
//   - JSONUnmarshaler → SkipValue + UnmarshalJSON over the raw span
//   - TextUnmarshaler → scan.String + UnmarshalText
//
// The receiver is value-typed so we declare a fresh `_u` of the
// underlying type, drive its Unmarshal* method, then cast back to the
// alias type. No allocation beyond what the underlying method itself
// performs.
func renderAliasStructDecode(s StructInfo, stream bool) string {
	var b strings.Builder
	b.WriteString("var result " + s.Name + "\n")
	switch {
	case s.AliasIface.ByteDecoder && !stream:
		fmt.Fprintf(&b, "var _u %s\n", s.AliasUnderlying)
		b.WriteString("v, k, err := _u.DecodeFrom(data, i)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "result = %s(v)\nreturn result, k, nil\n", s.Name)
	case s.AliasIface.StreamDecoder && stream:
		fmt.Fprintf(&b, "var _u %s\n", s.AliasUnderlying)
		b.WriteString("v, k, err := _u.DecodeStreamFrom(_s, i)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "result = %s(v)\nreturn result, k, nil\n", s.Name)
	case s.AliasIface.JSONUnmarshaler:
		if stream {
			b.WriteString("_start := i\n_k, err := _s.SkipValue(_start)\n")
			b.WriteString("if err != nil { return result, 0, err }\n")
			fmt.Fprintf(&b, "var _u %s\n", s.AliasUnderlying)
			b.WriteString("if err := _u.UnmarshalJSON(_s.Bytes()[_start:_k]); err != nil { return result, 0, err }\n")
		} else {
			b.WriteString("_start := i\n_k, err := scan.SkipValue(data, _start)\n")
			b.WriteString("if err != nil { return result, 0, err }\n")
			fmt.Fprintf(&b, "var _u %s\n", s.AliasUnderlying)
			b.WriteString("if err := _u.UnmarshalJSON(data[_start:_k]); err != nil { return result, 0, err }\n")
		}
		fmt.Fprintf(&b, "result = %s(_u)\nreturn result, _k, nil\n", s.Name)
	case s.AliasIface.TextUnmarshaler:
		if stream {
			b.WriteString("_ts, _tj, err := _s.String(i)\n")
		} else {
			b.WriteString("_ts, _tj, err := scan.String(data, i)\n")
		}
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "var _u %s\n", s.AliasUnderlying)
		b.WriteString("if err := _u.UnmarshalText(unsafe.Slice(unsafe.StringData(_ts), len(_ts))); err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "result = %s(_u)\nreturn result, _tj, nil\n", s.Name)
	default:
		// extractAlias should have rejected this case via aliasCanDelegate.
		b.WriteString("// no decode path — ggen could not find a Marshal/Unmarshal pair\n")
		b.WriteString("return result, 0, nil\n")
	}
	return b.String()
}

// renderAliasStructAppendJSON emits AppendJSON for a struct alias.
// Same delegation ladder as decode but for the encode direction:
// AppendJSON > MarshalJSON > AppendText (Go 1.24+, zero alloc) >
// MarshalText (one alloc — the lib's []byte return).
func renderAliasStructAppendJSON(s StructInfo) string {
	var b strings.Builder
	switch {
	case s.AliasIface.AppendJSON:
		fmt.Fprintf(&b, "_u := %s(s)\n", s.AliasUnderlying)
		b.WriteString("return _u.AppendJSON(dst)\n")
	case s.AliasIface.JSONMarshaler:
		fmt.Fprintf(&b, "_u := %s(s)\n", s.AliasUnderlying)
		b.WriteString("_b, err := _u.MarshalJSON()\n")
		b.WriteString("if err != nil { return dst, err }\n")
		b.WriteString("return append(dst, _b...), nil\n")
	case s.AliasIface.TextAppender:
		fmt.Fprintf(&b, "_u := %s(s)\n", s.AliasUnderlying)
		b.WriteString("dst = append(dst, '\"')\n")
		b.WriteString("var err error\n")
		b.WriteString("if dst, err = _u.AppendText(dst); err != nil { return dst, err }\n")
		b.WriteString("return append(dst, '\"'), nil\n")
	case s.AliasIface.TextMarshaler:
		fmt.Fprintf(&b, "_u := %s(s)\n", s.AliasUnderlying)
		b.WriteString("_t, err := _u.MarshalText()\n")
		b.WriteString("if err != nil { return dst, err }\n")
		b.WriteString("dst = append(dst, '\"')\n")
		fmt.Fprintf(&b, "dst = %s(dst, encode.BytesToString(_t))\n", appendStrFn(s.HTMLEscape))
		b.WriteString("return dst, nil\n")
	default:
		b.WriteString("return dst, nil\n")
	}
	return b.String()
}

// renderAliasStructSize returns a JSONSize upper bound for a struct
// alias. Without inspecting the underlying value, the safest bound is
// a large constant. Picked 128 as a round number — same flat fallback
// the per-field map estimate uses for nested structs.
func renderAliasStructSize() string {
	return "return 128\n"
}

// renderAliasContainerDecode dispatches a slice/map/array alias into
// the existing field-level emitters with `result` as ref. The shape
// FieldInfo (s.AliasField) carries Kind/ElemType/ElemKind/ArrayLen so
// the emitters know what to scan into. Container aliases reuse the
// full slice/map/array machinery — empty-peek bypass, hint-len cap,
// pointer-elem slab, dive validation, the lot.
func renderAliasContainerDecode(s StructInfo, stream bool) string {
	var b strings.Builder
	b.WriteString("var result " + s.Name + "\n")
	f := s.AliasField
	f.GoType = s.Name
	switch s.AliasKind {
	case KindSlice:
		if stream {
			b.WriteString(renderStreamSlice(f, "result", "i"))
		} else {
			b.WriteString(renderSlice(f, "result", "i"))
		}
	case KindArray:
		// emit{Byte,Stream}SliceRead handle both KindSlice and
		// KindArray internally via f.Kind / f.ArrayLen.
		if stream {
			b.WriteString(emitStreamSliceRead(f, "result", "i", 0))
		} else {
			b.WriteString(emitByteArrayRead(f, "result", "i", 0))
		}
	case KindMap:
		if stream {
			b.WriteString(renderStreamMap(f, "result", "i"))
		} else {
			b.WriteString(renderMap(f, "result", "i"))
		}
	case KindBytes:
		if stream {
			b.WriteString(renderStreamBytes(f, "result", "i"))
		} else {
			b.WriteString(renderBytes(f, "result", "i"))
		}
	}
	b.WriteString("return result, i, nil\n")
	return b.String()
}

// renderAliasContainerAppendJSON emits the encode body for slice/map/
// array aliases by reusing the field-level append helpers, with `s`
// as ref (`s` is the value-receiver name in AppendJSON).
func renderAliasContainerAppendJSON(s StructInfo) string {
	var b strings.Builder
	f := s.AliasField
	f.GoType = s.Name
	switch s.AliasKind {
	case KindSlice, KindArray:
		// renderAppendSlice handles both via f.Kind / f.ArrayLen.
		b.WriteString(renderAppendSlice(f, "s"))
	case KindMap:
		b.WriteString(renderAppendMap(f, "s"))
	case KindBytes:
		b.WriteString(renderAppendBytes(f, "s"))
	}
	b.WriteString("return dst, nil\n")
	return b.String()
}

// renderAliasContainerSize: upper-bound JSONSize for container aliases.
// Conservative — flat 1024 is plenty for typical small payloads, and
// the encoder pre-grows when the actual body exceeds the hint anyway.
// Tightening this requires per-element walks (slow at marshal time).
func renderAliasContainerSize(s StructInfo) string {
	switch s.AliasKind {
	case KindBytes:
		// `[]byte` base64 expansion is ~4/3 + padding + quotes
		return "return len(s)*4/3 + 8\n"
	}
	return "return 1024\n"
}

// aliasUnderlyingImports returns import paths needed by struct-alias
// codegen. Delegation paths emit a literal cast to the underlying type
// (e.g. `_u := uuid.UUID(s)`), so when the underlying lives in a foreign
// package, that package must be imported by the generated file.
// Same-package and stdlib-basic underlyings contribute "".
func aliasUnderlyingImports(structs []StructInfo) []string {
	seen := map[string]struct{}{}
	for _, s := range structs {
		if !s.IsAlias || s.AliasKind != KindStruct {
			continue
		}
		if s.AliasUnderlyingImport != "" {
			seen[s.AliasUnderlyingImport] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	return out
}
