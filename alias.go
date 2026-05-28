package main

import (
	"bytes"
	"fmt"
)

// renderAliasDecode emits the body of DecodeFrom for a primitive alias
// (e.g. `type HtmlString string`), a struct alias (`type LocalUUID
// uuid.UUID`), or a container alias (`type Tags []string`,
// `type Lookup map[string]int`, `type Tuple [3]int`).
func renderAliasDecode(b *bytes.Buffer, s StructInfo) {
	if s.AliasKind == KindStruct {
		renderAliasStructDecode(b, s, false)
		return
	}
	if s.AliasKind == KindSlice || s.AliasKind == KindMap || s.AliasKind == KindArray || s.AliasKind == KindBytes {
		renderAliasContainerDecode(b, s, false)
		return
	}
	b.WriteString("var err error\n_ = err\n")
	switch s.AliasKind {
	case KindString:
		fmt.Fprintf(b, "var v string\nv, i, err = scan.String(data, i)\nif err != nil { return result, i, err }\nresult = %s(v)\n", s.Name)
	case KindBool:
		fmt.Fprintf(b, "var v bool\nv, i, err = scan.Bool(data, i)\nif err != nil { return result, i, err }\nresult = %s(v)\n", s.Name)
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		fmt.Fprintf(b, "var v int64\nv, i, err = scan.Int64(data, i)\nif err != nil { return result, i, err }\nresult = %s(v)\n", s.Name)
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		fmt.Fprintf(b, "var v uint64\nv, i, err = scan.Uint64(data, i)\nif err != nil { return result, i, err }\nresult = %s(v)\n", s.Name)
	case KindFloat32, KindFloat64:
		fmt.Fprintf(b, "var v float64\nv, i, err = scan.Float64(data, i)\nif err != nil { return result, i, err }\nresult = %s(v)\n", s.Name)
	}
	b.WriteString("return result, i, nil\n")
}

// renderAliasStreamDecode is the io.Reader counterpart of
// renderAliasDecode.
func renderAliasStreamDecode(b *bytes.Buffer, s StructInfo) {
	if s.AliasKind == KindStruct {
		renderAliasStructDecode(b, s, true)
		return
	}
	if s.AliasKind == KindSlice || s.AliasKind == KindMap || s.AliasKind == KindArray || s.AliasKind == KindBytes {
		renderAliasContainerDecode(b, s, true)
		return
	}
	b.WriteString("var err error\n_ = err\n")
	switch s.AliasKind {
	case KindString:
		fmt.Fprintf(b, "var v string\nv, err = s.String()\nif err != nil { return result, err }\nresult = %s(v)\n", s.Name)
	case KindBool:
		fmt.Fprintf(b, "var v bool\nv, err = s.Bool()\nif err != nil { return result, err }\nresult = %s(v)\n", s.Name)
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		fmt.Fprintf(b, "var v int64\nv, err = s.Int64()\nif err != nil { return result, err }\nresult = %s(v)\n", s.Name)
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		fmt.Fprintf(b, "var v uint64\nv, err = s.Uint64()\nif err != nil { return result, err }\nresult = %s(v)\n", s.Name)
	case KindFloat32, KindFloat64:
		fmt.Fprintf(b, "var v float64\nv, err = s.Float64()\nif err != nil { return result, err }\nresult = %s(v)\n", s.Name)
	}
	b.WriteString("return result, nil\n")
}

// renderAliasSize returns an upper-bound JSONSize body for the alias.
// Strings dominate length; numerics are bounded by their textual width.
func renderAliasSize(s StructInfo) string {
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
	case KindBytes:
		// `[]byte` base64 expansion is ~4/3 + padding + quotes
		return "return len(s)*4/3 + 8\n"
	case KindSlice, KindMap, KindArray:
		return "return 1024\n"
	case KindStruct:
		switch {
		case s.AliasIface.JSONMarshaler, s.AliasIface.TextMarshaler:
			return "return 0\n"
		default:
			return "return 128\n"
		}
	default:
		return "return 0\n"
	}
}

// renderAliasAppendJSON emits the body of AppendJSON for a primitive
// or struct alias.
func renderAliasAppendJSON(b *bytes.Buffer, s StructInfo) {
	if s.AliasKind == KindStruct {
		renderAliasStructAppendJSON(b, s)
		return
	}
	if s.AliasKind == KindSlice || s.AliasKind == KindMap || s.AliasKind == KindArray || s.AliasKind == KindBytes {
		renderAliasContainerAppendJSON(b, s)
		return
	}
	switch s.AliasKind {
	case KindString:
		// CALLER side: emit the opening quote, then the body via the
		// chosen string-append helper (which writes the closing quote).
		fmt.Fprintf(b, "dst = append(dst, '\"')\ndst = %s(dst, string(s))\nreturn dst, nil\n", appendStrFn(s.HTMLEscape))
	case KindBool:
		b.WriteString("return strconv.AppendBool(dst, bool(s)), nil\n")
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		b.WriteString("return strconv.AppendInt(dst, int64(s), 10), nil\n")
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		b.WriteString("return strconv.AppendUint(dst, uint64(s), 10), nil\n")
	case KindFloat32:
		b.WriteString("return strconv.AppendFloat(dst, float64(s), 'g', -1, 32), nil\n")
	case KindFloat64:
		b.WriteString("return strconv.AppendFloat(dst, float64(s), 'g', -1, 64), nil\n")
	}
}

// renderAliasStructDecode emits DecodeFrom (or DecodeFromStream when
// stream==true) for a struct alias. Picks the cheapest delegation path
// the underlying type's method set supports:
//   - ggen-shaped DecodeFrom on the underlying → call directly
//   - JSONUnmarshaler → SkipValue + UnmarshalJSON over the raw span
//   - TextUnmarshaler → scan.String + UnmarshalText
//
// The receiver is value-typed so we declare a fresh `u` of the
// underlying type, drive its Unmarshal* method, then cast back to the
// alias type. No allocation beyond what the underlying method itself
// performs.
func renderAliasStructDecode(b *bytes.Buffer, s StructInfo, stream bool) {
	switch {
	case s.AliasIface.ByteDecoder && !stream:
		fmt.Fprintf(b, `var u %[1]s
v, _n, err := u.DecodeFrom(data[i:])
i += _n
if err != nil { return result, i, err }
result = %[2]s(v)
return result, i, nil
`, s.AliasUnderlying, s.Name)
	case s.AliasIface.StreamDecoder && stream:
		fmt.Fprintf(b, `var u %[1]s
v, err := u.DecodeFromStream(s)
if err != nil { return result, err }
result = %[2]s(v)
return result, nil
`, s.AliasUnderlying, s.Name)
	case s.AliasIface.JSONUnmarshaler:
		if stream {
			fmt.Fprintf(b, `start := s.Pos
prevPin := s.Shift
s.Shift = false
err := s.SkipValue()
s.Shift = prevPin
if err != nil { return result, err }
var u %[1]s
if err := u.UnmarshalJSON(s.Bytes()[start:s.Pos]); err != nil { return result, err }
result = %[2]s(u)
return result, nil
`, s.AliasUnderlying, s.Name)
		} else {
			fmt.Fprintf(b, `start := i
k, err := scan.SkipValue(data, start)
if err != nil { return result, i, err }
var u %[1]s
if err := u.UnmarshalJSON(data[start:k]); err != nil { return result, i, err }
result = %[2]s(u)
return result, k, nil
`, s.AliasUnderlying, s.Name)
		}
	case s.AliasIface.TextUnmarshaler:
		if stream {
			fmt.Fprintf(b, `ts, err := s.String()
if err != nil { return result, err }
var u %[1]s
if err := u.UnmarshalText(unsafe.Slice(unsafe.StringData(ts), len(ts))); err != nil { return result, err }
result = %[2]s(u)
return result, nil
`, s.AliasUnderlying, s.Name)
		} else {
			fmt.Fprintf(b, `ts, tj, err := scan.String(data, i)
if err != nil { return result, i, err }
var u %[1]s
if err := u.UnmarshalText(unsafe.Slice(unsafe.StringData(ts), len(ts))); err != nil { return result, i, err }
result = %[2]s(u)
return result, tj, nil
`, s.AliasUnderlying, s.Name)
		}
	default:
		// extractAlias should have rejected this case via aliasCanDelegate.
		if stream {
			b.WriteString("// no decode path — ggen could not find a Marshal/Unmarshal pair\nreturn result, nil\n")
		} else {
			b.WriteString("// no decode path — ggen could not find a Marshal/Unmarshal pair\nreturn result, i, nil\n")
		}
	}
}

// renderAliasStructAppendJSON emits AppendJSON for a struct alias.
// Same delegation ladder as decode but for the encode direction:
// AppendJSON > MarshalJSON > AppendText (Go 1.24+, zero alloc) >
// MarshalText (one alloc — the lib's []byte return).
func renderAliasStructAppendJSON(b *bytes.Buffer, s StructInfo) {
	switch {
	case s.AliasIface.AppendJSON:
		fmt.Fprintf(b, "u := %s(s)\nreturn u.AppendJSON(dst)\n", s.AliasUnderlying)
	case s.AliasIface.JSONMarshaler:
		// dst is empty (JSONSize returns 0 for this branch) — return
		// MarshalJSON's slice and err directly to skip the redundant copy.
		fmt.Fprintf(b, "return %s(s).MarshalJSON()\n", s.AliasUnderlying)
	case s.AliasIface.TextAppender:
		fmt.Fprintf(b, `u := %s(s)
dst = append(dst, '"')
var err error
if dst, err = u.AppendText(dst); err != nil { return dst, err }
return append(dst, '"'), nil
`, s.AliasUnderlying)
	case s.AliasIface.TextMarshaler:
		fmt.Fprintf(b, `u := %s(s)
t, err := u.MarshalText()
if err != nil { return dst, err }
dst = append(dst, '"')
dst = %s(dst, encode.BytesToString(t))
return dst, nil
`, s.AliasUnderlying, appendStrFn(s.HTMLEscape))
	default:
		b.WriteString("return dst, nil\n")
	}
}

// renderAliasContainerDecode dispatches a slice/map/array alias into
// the existing field-level emitters with `result` as ref. The shape
// FieldInfo (s.AliasField) carries Kind/ElemType/ElemKind/ArrayLen so
// the emitters know what to scan into. Container aliases reuse the
// full slice/map/array machinery — empty-peek bypass, hint-len cap,
// pointer-elem slab, dive validation, the lot.
func renderAliasContainerDecode(b *bytes.Buffer, s StructInfo, stream bool) {
	// Hoist err for both byte- and stream-path container decoders. Byte
	// path doesn't have a function-scope err naturally (inline scanners
	// declare locally); stream's regular non-alias path gets err from
	// ObjectOpen but alias containers don't open an object first.
	b.WriteString("var err error\n_ = err\n")
	// Receiver IS the container — reset before decode so we don't append
	// over data carried in from the caller. KindArray has no nil state;
	// every slot gets overwritten or errors via strict-length decode.
	switch s.AliasKind {
	case KindSlice, KindBytes:
		b.WriteString("if result != nil { result = result[:0] }\n")
	case KindMap:
		b.WriteString("if result != nil { clear(result) }\n")
	}
	f := s.AliasField
	f.GoType = s.Name
	// Stream-path cursor is s.Pos (no separate `i` local); bytes-path uses
	// the function-arg `i`.
	posVar := "i"
	if stream {
		posVar = "s.Pos"
	}
	switch s.AliasKind {
	case KindSlice:
		if stream {
			renderStreamSlice(b, f, "result", posVar)
		} else {
			renderSlice(b, f, "result", posVar)
		}
	case KindArray:
		// emit{Byte,Stream}SliceRead handle both KindSlice and
		// KindArray internally via f.Kind / f.ArrayLen.
		if stream {
			emitStreamSliceRead(b, f, "result", posVar, 0)
		} else {
			emitByteArrayRead(b, f, "result", posVar, 0)
		}
	case KindMap:
		if stream {
			renderStreamMap(b, f, "result", posVar)
		} else {
			renderMap(b, f, "result", posVar)
		}
	case KindBytes:
		if stream {
			renderStreamBytes(b, f, "result", posVar)
		} else {
			renderBytes(b, f, "result", posVar)
		}
	}
	if stream {
		b.WriteString("return result, nil\n")
	} else {
		b.WriteString("return result, i, nil\n")
	}
}

// renderAliasContainerAppendJSON emits the encode body for slice/map/
// array aliases by reusing the field-level append helpers, with `s`
// as ref (`s` is the value-receiver name in AppendJSON).
func renderAliasContainerAppendJSON(b *bytes.Buffer, s StructInfo) {
	f := s.AliasField
	f.GoType = s.Name
	switch s.AliasKind {
	case KindSlice, KindArray:
		// renderAppendSlice handles both via f.Kind / f.ArrayLen.
		renderAppendSlice(b, f, "s")
	case KindMap:
		renderAppendMap(b, f, "s")
	case KindBytes:
		renderAppendBytes(b, f, "s")
	}
	b.WriteString("return dst, nil\n")
}
