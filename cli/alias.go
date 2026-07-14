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
	const wrap = `if err != nil { return result, i, decode.NewParseErr("", i, err) }`
	switch s.AliasKind {
	case KindString:
		fmt.Fprintf(b, "var v string\nv, i, err = "+scanStringFn+"(data, i, "+vArgS(s)+")\n%s\nresult = %s(v)\n", wrap, s.Name)
	case KindBool:
		fmt.Fprintf(b, "var v bool\nv, i, err = scan.Bool(data, i)\n%s\nresult = %s(v)\n", wrap, s.Name)
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		fmt.Fprintf(b, "var v int64\nv, i, err = scan.Int64(data, i)\n%s\nresult = %s(v)\n", wrap, s.Name)
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		fmt.Fprintf(b, "var v uint64\nv, i, err = scan.Uint64(data, i)\n%s\nresult = %s(v)\n", wrap, s.Name)
	case KindFloat32, KindFloat64:
		fmt.Fprintf(b, "var v float64\nv, i, err = scan.Float64(data, i)\n%s\nresult = %s(v)\n", wrap, s.Name)
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
	const wrap = `if err != nil { return result, decode.NewParseErr("", s.Pos, err) }`
	switch s.AliasKind {
	case KindString:
		fmt.Fprintf(b, "var v string\nv, err = s.String("+vArgS(s)+")\n%s\nresult = %s(v)\n", wrap, s.Name)
	case KindBool:
		fmt.Fprintf(b, "var v bool\nv, err = s.Bool()\n%s\nresult = %s(v)\n", wrap, s.Name)
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		fmt.Fprintf(b, "var v int64\nv, err = s.Int64()\n%s\nresult = %s(v)\n", wrap, s.Name)
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		fmt.Fprintf(b, "var v uint64\nv, err = s.Uint64()\n%s\nresult = %s(v)\n", wrap, s.Name)
	case KindFloat32, KindFloat64:
		fmt.Fprintf(b, "var v float64\nv, err = s.Float64()\n%s\nresult = %s(v)\n", wrap, s.Name)
	}
	b.WriteString("return result, nil\n")
}

// renderAliasSize returns an upper-bound JSONSize body for the alias.
func renderAliasSize(s StructInfo) string {
	switch s.AliasKind {
	case KindString:
		// *2 for worst-case escapes + 2 quotes.
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
		// Opening quote here; the helper writes the body + closing quote.
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

// renderAliasStructDecode emits DecodeFrom (or DecodeFromStream when stream)
// for a struct alias, picking the cheapest delegation the underlying supports:
// ggen-shaped DecodeFrom → JSONUnmarshaler (SkipValue + UnmarshalJSON) →
// TextUnmarshaler (scan.String + UnmarshalText). Drives a fresh `u` of the
// underlying type, then casts back.
func renderAliasStructDecode(b *bytes.Buffer, s StructInfo, stream bool) {
	switch {
	case s.AliasIface.ByteDecoder && !stream:
		fmt.Fprintf(b, `var u %[1]s
v, _n, err := u.DecodeFrom(data[i:])
i += _n
if err != nil { return result, i, decode.NewParseErr("", i, err) }
result = %[2]s(v)
return result, i, nil
`, s.AliasUnderlying, s.Name)
	case s.AliasIface.StreamDecoder && stream:
		fmt.Fprintf(b, `var u %[1]s
v, err := u.DecodeFromStream(s)
if err != nil { return result, decode.NewParseErr("", s.Pos, err) }
result = %[2]s(v)
return result, nil
`, s.AliasUnderlying, s.Name)
	case s.AliasIface.JSONUnmarshaler:
		if stream {
			fmt.Fprintf(b, `span, err := s.CaptureValue()
if err != nil { return result, decode.NewParseErr("", s.Pos, err) }
var u %[1]s
if err := u.UnmarshalJSON(span); err != nil { return result, decode.NewParseErr("", s.Pos, err) }
result = %[2]s(u)
return result, nil
`, s.AliasUnderlying, s.Name)
		} else {
			fmt.Fprintf(b, `start := i
k, err := scan.SkipValue(data, start)
if err != nil { return result, i, decode.NewParseErr("", i, err) }
var u %[1]s
if err := u.UnmarshalJSON(data[start:k]); err != nil { return result, i, decode.NewParseErr("", i, err) }
result = %[2]s(u)
return result, k, nil
`, s.AliasUnderlying, s.Name)
		}
	case s.AliasIface.TextUnmarshaler:
		if stream {
			fmt.Fprintf(b, `ts, err := s.String(`+vArgS(s)+`)
if err != nil { return result, decode.NewParseErr("", s.Pos, err) }
var u %[1]s
if err := u.UnmarshalText(unsafe.Slice(unsafe.StringData(ts), len(ts))); err != nil { return result, decode.NewParseErr("", s.Pos, err) }
result = %[2]s(u)
return result, nil
`, s.AliasUnderlying, s.Name)
		} else {
			fmt.Fprintf(b, `ts, tj, err := `+scanStringFn+`(data, i, `+vArgS(s)+`)
if err != nil { return result, i, decode.NewParseErr("", i, err) }
var u %[1]s
if err := u.UnmarshalText(unsafe.Slice(unsafe.StringData(ts), len(ts))); err != nil { return result, tj, decode.NewParseErr("", tj, err) }
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

// renderAliasContainerDecode dispatches a slice/map/array alias into the
// field-level emitters with `result` as ref; s.AliasField carries the shape.
// All the slice/map/array machinery (empty-peek, hint-len cap, slab, dive)
// carries over.
func renderAliasContainerDecode(b *bytes.Buffer, s StructInfo, stream bool) {
	// Receiver IS the container — reset before decode so we don't append over
	// carried-in data. KindArray has no nil state (every slot overwritten).
	switch s.AliasKind {
	case KindSlice, KindBytes:
		b.WriteString("if result != nil { result = result[:0] }\n")
	case KindMap:
		b.WriteString("if result != nil { clear(result) }\n")
	}
	f := s.AliasField
	f.GoType = s.Name
	// Stream cursor is s.Pos; bytes path uses the function-arg `i`.
	posVar := "i"
	if stream {
		posVar = "s.Pos"
	} else {
		// Top-level alias value: nothing skipped leading WS before this, and the
		// container emitters don't skip it themselves, so do it here.
		inlineSkipWS(b, posVar)
	}
	// Bytes path passes topLevel=true: the container emitter returns at each exit
	// (null / array close) instead of falling through, so the trailing return is
	// dropped. Stream path keeps the trailing return.
	switch s.AliasKind {
	case KindSlice:
		if stream {
			renderStreamSlice(b, f, "result", posVar)
		} else {
			emitByteSliceRead(b, f, "result", posVar, 0, true)
		}
	case KindArray:
		// emit{Byte,Stream}SliceRead handle both KindSlice and
		// KindArray internally via f.Kind / f.ArrayLen.
		if stream {
			emitStreamSliceRead(b, f, "result", posVar, 0)
		} else {
			emitByteSliceRead(b, f, "result", posVar, 0, true)
		}
	case KindMap:
		if stream {
			renderStreamMap(b, f, "result", posVar)
		} else {
			renderMap(b, f, "result", posVar, true)
		}
	case KindBytes:
		if stream {
			renderStreamBytes(b, f, "result", posVar)
		} else {
			renderBytes(b, f, "result", posVar)
		}
	}
	// Bytes slice/array/map self-return via topLevel; only stream (all kinds) and
	// bytes KindBytes need the trailing return.
	if stream {
		b.WriteString("return result, nil\n")
	} else if s.AliasKind == KindBytes {
		b.WriteString("return result, i, nil\n")
	}
}

// renderAliasContainerAppendJSON emits the encode body for slice/map/array
// aliases via the field-level append helpers, with `s` (the receiver) as ref.
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
