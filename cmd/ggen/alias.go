package main

import (
	"bytes"
	"fmt"

	"github.com/sirkostya009/ggen/gen/model"
)

// renderAliasDecode emits the body of DecodeFrom for an alias type. That is a
// primitive alias such as `type HtmlString string`, a struct alias such as
// `type LocalUUID uuid.UUID`, or a container alias such as
// `type Tags []string`, `type Lookup map[string]int` or `type Tuple [3]int`.
func renderAliasDecode(b *bytes.Buffer, s model.StructInfo) {
	if s.AliasKind == model.KindStruct {
		renderAliasStructDecode(b, s, false)
		return
	}
	if s.AliasKind == model.KindSlice || s.AliasKind == model.KindMap || s.AliasKind == model.KindArray || s.AliasKind == model.KindBytes {
		renderAliasContainerDecode(b, s, false)
		return
	}
	const wrap = `if err != nil { return result, i, ggen.NewParseErr("", i, err) }`
	// Leading whitespace is legal before any top-level value.
	inlineSkipWS(b, "i")
	//exhaustive:ignore not every kind applies here
	switch s.AliasKind {
	case model.KindString:
		// Under copy, Detach clones the string only when it aliases data,
		// meaning it points into the input bytes with no copy. A string that
		// had escapes is already a fresh copy. Struct fields use the same
		// shape.
		detach := ""
		if s.Copy {
			detach = "v = ggen.Detach(v, data)\n"
		}
		fmt.Fprintf(b, "var v string\nv, i, err = "+scanStringFn+"(data, i, "+vArgS(s)+")\n%s\n%sresult = %s(v)\n", wrap, detach, s.Name)
	case model.KindBool:
		fmt.Fprintf(b, "var v bool\nv, i, err = ggen.Bool(data, i)\nif err != nil { i = ggen.BoolEnd(data, i) }\n%s\nresult = %s(v)\n", wrap, s.Name)
	case model.KindInt, model.KindInt8, model.KindInt16, model.KindInt32, model.KindInt64:
		guard := narrowIntGuard("v", s.AliasUnderlying, `return result, i, ggen.NewParseErr("", i, ggen.ErrNumberOverflow)`)
		fmt.Fprintf(b, "var v int64\nv, i, err = ggen.Int64(data, i)\n%s\n%sresult = %s(v)\n", wrap, guard, s.Name)
	case model.KindUint, model.KindUint8, model.KindUint16, model.KindUint32, model.KindUint64:
		guard := narrowIntGuard("v", s.AliasUnderlying, `return result, i, ggen.NewParseErr("", i, ggen.ErrNumberOverflow)`)
		fmt.Fprintf(b, "var v uint64\nv, i, err = ggen.Uint64(data, i)\n%s\n%sresult = %s(v)\n", wrap, guard, s.Name)
	case model.KindFloat32, model.KindFloat64:
		fmt.Fprintf(b, "var v %s\nv, i, err = ggen.%s(data, i)\n%s\nresult = %s(v)\n", s.AliasUnderlying, floatScanFn(s.AliasKind), wrap, s.Name)
	}
	b.WriteString("return result, i, nil\n")
}

// renderAliasStreamDecode is the stream counterpart of renderAliasDecode.
func renderAliasStreamDecode(b *bytes.Buffer, s model.StructInfo) {
	if s.AliasKind == model.KindStruct {
		renderAliasStructDecode(b, s, true)
		return
	}
	if s.AliasKind == model.KindSlice || s.AliasKind == model.KindMap || s.AliasKind == model.KindArray || s.AliasKind == model.KindBytes {
		renderAliasContainerDecode(b, s, true)
		return
	}
	const wrap = `if err != nil { return result, ggen.NewParseErr("", s.Offset(), err) }`
	b.WriteString("err = s.SkipSpace()\n" + wrap + "\n")
	//exhaustive:ignore not every kind applies here
	switch s.AliasKind {
	case model.KindString:
		fmt.Fprintf(b, "var v string\nv, err = s.String("+vArgS(s)+")\n%s\nresult = %s(v)\n", wrap, s.Name)
	case model.KindBool:
		fmt.Fprintf(b, "var v bool\nv, err = s.Bool()\n%s\nresult = %s(v)\n", wrap, s.Name)
	case model.KindInt, model.KindInt8, model.KindInt16, model.KindInt32, model.KindInt64:
		guard := narrowIntGuard("v", s.AliasUnderlying, `return result, ggen.NewParseErr("", s.Offset(), ggen.ErrNumberOverflow)`)
		fmt.Fprintf(b, "var v int64\nv, err = s.Int64()\n%s\n%sresult = %s(v)\n", wrap, guard, s.Name)
	case model.KindUint, model.KindUint8, model.KindUint16, model.KindUint32, model.KindUint64:
		guard := narrowIntGuard("v", s.AliasUnderlying, `return result, ggen.NewParseErr("", s.Offset(), ggen.ErrNumberOverflow)`)
		fmt.Fprintf(b, "var v uint64\nv, err = s.Uint64()\n%s\n%sresult = %s(v)\n", wrap, guard, s.Name)
	case model.KindFloat32, model.KindFloat64:
		fmt.Fprintf(b, "var v %s\nv, err = s.%s()\n%s\nresult = %s(v)\n", s.AliasUnderlying, floatScanFn(s.AliasKind), wrap, s.Name)
	}
	b.WriteString("return result, nil\n")
}

// renderAliasSize returns a JSONSize body for the alias that gives an upper
// bound on the encoded size.
func renderAliasSize(s model.StructInfo) string {
	switch s.AliasKind {
	case model.KindString:
		// Worst case every byte is escaped, plus 2 quotes. An escape takes 2
		// bytes, or 6 under htmlescape, where `<` becomes `\u003c`.
		return fmt.Sprintf("return len(string(s))*%d + 2\n", strMult(s.HTMLEscape))
	case model.KindBool:
		return "return 5\n" // "false"
	case model.KindInt, model.KindInt8, model.KindInt16, model.KindInt32, model.KindInt64:
		return "return 20\n" // -9223372036854775808
	case model.KindUint, model.KindUint8, model.KindUint16, model.KindUint32, model.KindUint64:
		return "return 20\n" // 18446744073709551615
	case model.KindFloat32, model.KindFloat64:
		return fmt.Sprintf("return %d\n", sizeFloat)
	case model.KindBytes:
		// base64 grows the bytes by about 4/3, plus padding and quotes.
		return "return len(s)*4/3 + 8\n"
	case model.KindSlice, model.KindMap, model.KindArray:
		// Use the same per-kind sizing as a struct field of this shape. A
		// fixed number would be too small for large containers and too big
		// for small ones.
		f := aliasContainerField(s)
		n, code := sizeContrib(f, "s")
		return fmt.Sprintf("size := %d\n%sreturn size\n", n, code)
	case model.KindStruct:
		switch {
		case s.AliasIface.AppendJSON && s.AliasIface.JSONSize:
			// The underlying type has ggen methods, so use its own bound.
			return fmt.Sprintf("return %s(s).JSONSize()\n", s.AliasUnderlying)
		case s.AliasIface.JSONMarshaler, s.AliasIface.TextMarshaler:
			// MarshalJSON and MarshalText allocate their own result. A nonzero
			// size here would only add a make() allocation before that one,
			// and a third one when the result does not fit.
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
func renderAliasAppendJSON(b *bytes.Buffer, s model.StructInfo) {
	if s.AliasKind == model.KindStruct {
		renderAliasStructAppendJSON(b, s)
		return
	}
	if s.AliasKind == model.KindSlice || s.AliasKind == model.KindMap || s.AliasKind == model.KindArray || s.AliasKind == model.KindBytes {
		renderAliasContainerAppendJSON(b, s)
		return
	}
	//exhaustive:ignore not every kind applies here
	switch s.AliasKind {
	case model.KindString:
		// Write the opening quote here. The helper writes the body and the
		// closing quote.
		fmt.Fprintf(b, "dst = append(dst, '\"')\ndst = %s(dst, string(s))\nreturn dst, nil\n", appendStrFn(s.HTMLEscape))
	case model.KindBool:
		b.WriteString("return strconv.AppendBool(dst, bool(s)), nil\n")
	case model.KindInt, model.KindInt8, model.KindInt16, model.KindInt32, model.KindInt64:
		b.WriteString("return strconv.AppendInt(dst, int64(s), 10), nil\n")
	case model.KindUint, model.KindUint8, model.KindUint16, model.KindUint32, model.KindUint64:
		b.WriteString("return strconv.AppendUint(dst, uint64(s), 10), nil\n")
	case model.KindFloat32:
		// ggen.AppendFloat formats like the standard library. It returns an
		// error for NaN and Inf instead of writing invalid JSON. Struct float
		// fields use it too.
		b.WriteString("return ggen.AppendFloat(dst, float64(s), 32)\n")
	case model.KindFloat64:
		b.WriteString("return ggen.AppendFloat(dst, float64(s), 64)\n")
	}
}

// renderAliasStructDecode emits DecodeFrom, or DecodeFromStream when stream
// is set, for a struct alias. It picks the cheapest method the underlying
// type has, in this order:
//   - a ggen DecodeFrom
//   - UnmarshalJSON, on the span found by SkipValue
//   - UnmarshalText, on the scanned JSON string
//
// It decodes into a fresh `u` of the underlying type, then converts it back.
func renderAliasStructDecode(b *bytes.Buffer, s model.StructInfo, stream bool) {
	switch {
	case s.AliasIface.ByteDecoder && !stream:
		// If the underlying type is part of a type cycle, pass the depth on.
		// Otherwise going through the alias would reset the depth budget.
		call := "DecodeFrom(data[i:])"
		if isCyclic(s.AliasUnderlying) {
			call = "decodeFromDepth(data[i:], depth+1)"
		}
		fmt.Fprintf(b, `var u %[1]s
v, consumed, err := u.`+call+`
i += consumed
if err != nil { return result, i, ggen.NewParseErrShift("", i, consumed, err) }
result = %[2]s(v)
return result, i, nil
`, s.AliasUnderlying, s.Name)
	case s.AliasIface.StreamDecoder && stream:
		call := "DecodeFromStream(s)"
		if isCyclic(s.AliasUnderlying) {
			call = "decodeFromStreamDepth(s, depth+1)"
		}
		fmt.Fprintf(b, `var u %[1]s
v, err := u.`+call+`
if err != nil { return result, ggen.NewParseErr("", s.Offset(), err) }
result = %[2]s(v)
return result, nil
`, s.AliasUnderlying, s.Name)
	case s.AliasIface.JSONUnmarshaler:
		if stream {
			fmt.Fprintf(b, `span, err := s.CaptureValue()
if err != nil { return result, ggen.NewParseErr("", s.Offset(), err) }
var u %[1]s
if err := u.UnmarshalJSON(span); err != nil { return result, ggen.NewParseErr("", s.Offset(), err) }
result = %[2]s(u)
return result, nil
`, s.AliasUnderlying, s.Name)
		} else {
			fmt.Fprintf(b, `start := i
k, err := ggen.SkipValue(data, start)
if err != nil { return result, i, ggen.NewParseErr("", i, err) }
var u %[1]s
if err := u.UnmarshalJSON(data[start:k]); err != nil { return result, i, ggen.NewParseErr("", i, err) }
result = %[2]s(u)
return result, k, nil
`, s.AliasUnderlying, s.Name)
		}
	case s.AliasIface.TextUnmarshaler:
		// In the cases above, DecodeFrom and SkipValue skip leading
		// whitespace. The string scans here do not, so skip it first.
		if stream {
			b.WriteString(`if err := s.SkipSpace(); err != nil { return result, ggen.NewParseErr("", s.Offset(), err) }` + "\n")
			fmt.Fprintf(b, `ts, err := s.String(`+vArgS(s)+`)
if err != nil { return result, ggen.NewParseErr("", s.Offset(), err) }
var u %[1]s
if err := u.UnmarshalText(unsafe.Slice(unsafe.StringData(ts), len(ts))); err != nil { return result, ggen.NewParseErr("", s.Offset(), err) }
result = %[2]s(u)
return result, nil
`, s.AliasUnderlying, s.Name)
		} else {
			inlineSkipWS(b, "i")
			fmt.Fprintf(b, `ts, tj, err := `+scanStringFn+`(data, i, `+vArgS(s)+`)
if err != nil { return result, i, ggen.NewParseErr("", i, err) }
var u %[1]s
if err := u.UnmarshalText(unsafe.Slice(unsafe.StringData(ts), len(ts))); err != nil { return result, tj, ggen.NewParseErr("", tj, err) }
result = %[2]s(u)
return result, tj, nil
`, s.AliasUnderlying, s.Name)
		}
	default:
		// The parser rejects this case (see aliasCanDelegate in gen/model).
		if stream {
			b.WriteString("// no decode path — ggen could not find a Marshal/Unmarshal pair\nreturn result, nil\n")
		} else {
			b.WriteString("// no decode path — ggen could not find a Marshal/Unmarshal pair\nreturn result, i, nil\n")
		}
	}
}

// renderAliasStructAppendJSON emits AppendJSON for a struct alias. Like the
// decode side, it picks the first method the underlying type has, in this
// order: AppendJSON, MarshalJSON, AppendText (Go 1.24+, no allocation), then
// MarshalText (one allocation for the returned []byte).
func renderAliasStructAppendJSON(b *bytes.Buffer, s model.StructInfo) {
	switch {
	case s.AliasIface.AppendJSON:
		fmt.Fprintf(b, "u := %s(s)\nreturn u.AppendJSON(dst)\n", s.AliasUnderlying)
	case s.AliasIface.JSONMarshaler:
		// An empty result is an error, as in the standard library. Types from
		// other packages get the same check.
		fmt.Fprintf(b, `bs, err := %s(s).MarshalJSON()
if err != nil { return dst, err }
if len(bs) == 0 { return dst, ggen.ErrEmptyMarshalJSON }
return append(dst, bs...), nil
`, s.AliasUnderlying)
	case s.AliasIface.TextAppender:
		fmt.Fprintf(b, `u := %s(s)
dst = append(dst, '"')
ts := len(dst)
var err error
if dst, err = u.AppendText(dst); err != nil { return dst, err }
return %s(dst, ts), nil
`, s.AliasUnderlying, closeStrFn(s.HTMLEscape))
	case s.AliasIface.TextMarshaler:
		fmt.Fprintf(b, `u := %s(s)
t, err := u.MarshalText()
if err != nil { return dst, err }
dst = append(dst, '"')
dst = %s(dst, ggen.BytesToString(t))
return dst, nil
`, s.AliasUnderlying, appendStrFn(s.HTMLEscape))
	default:
		b.WriteString("return dst, nil\n")
	}
}

// aliasContainerField returns s.AliasField with the struct-level flags
// copied onto it. The parser builds the alias field before annotation and CLI
// flags are applied, and that step only walks Fields. Reading the field as is
// would lose flags such as copy, htmlescape, allowinvalidutf8 and multierr.
func aliasContainerField(s model.StructInfo) model.FieldInfo {
	f := s.AliasField
	f.GoType = s.Name
	f.MultiErr = s.MultiErr
	f.NoValidate = s.NoValidate
	f.AllowDups = s.AllowDups
	f.UseNumber = s.UseNumber
	f.HTMLEscape = s.HTMLEscape
	f.Copy = s.Copy
	f.AllowInvalidUTF8 = s.AllowInvalidUTF8
	return f
}

// renderAliasContainerDecode sends a slice, map or array alias to the
// field-level emitters, with `result` as the target. s.AliasField describes
// the shape. Every container feature of fields works here too, such as the
// empty check, the capacity hint, the slab and dive rules.
func renderAliasContainerDecode(b *bytes.Buffer, s model.StructInfo, stream bool) {
	// The receiver is the container itself. Reset it before decoding so new
	// elements do not land after old data. An array needs no reset, because
	// every slot is overwritten.
	//exhaustive:ignore not every kind applies here
	switch s.AliasKind {
	case model.KindSlice, model.KindBytes:
		// A [N]byte folded into KindBytes is still an array. It cannot be
		// nil or resliced.
		if s.AliasField.ArrayLen == 0 {
			b.WriteString("if result != nil { result = result[:0] }\n")
		}
	case model.KindMap:
		b.WriteString("if result != nil { clear(result) }\n")
	}
	f := aliasContainerField(s)
	// The stream cursor is s.Pos. The bytes path uses the argument `i`.
	posVar := "i"
	if stream {
		posVar = "s.Pos"
	} else {
		// This is a top-level value. Nothing has skipped leading whitespace
		// yet and the container emitters do not, so skip it here.
		inlineSkipWS(b, posVar)
	}
	// The bytes path passes topLevel=true. The container emitter then returns
	// at each exit, such as null or the closing bracket, instead of falling
	// through, so no trailing return is needed. The stream path keeps the
	// trailing return.
	//exhaustive:ignore not every kind applies here
	switch s.AliasKind {
	case model.KindSlice:
		if stream {
			renderStreamSlice(b, f, "result", posVar)
		} else {
			emitByteSliceRead(b, f, "result", posVar, 0, true)
		}
	case model.KindArray:
		// emitByteSliceRead and emitStreamSliceRead handle both slices and
		// arrays, using f.Kind and f.ArrayLen.
		if stream {
			emitStreamSliceRead(b, f, "result", posVar, 0)
		} else {
			emitByteSliceRead(b, f, "result", posVar, 0, true)
		}
	case model.KindMap:
		if stream {
			renderStreamMap(b, f, "result", posVar)
		} else {
			renderMap(b, f, "result", posVar, true)
		}
	case model.KindBytes:
		if stream {
			// The other stream kinds reach their first byte through
			// ArrayOpen or ObjectOpen, which skip whitespace.
			// renderStreamBytes reads s.Pos directly.
			b.WriteString(`if err := s.SkipSpace(); err != nil { return result, ggen.NewParseErr("", s.Offset(), err) }` + "\n")
			renderStreamBytes(b, f, "result", posVar)
		} else {
			renderBytes(b, f, "result", posVar)
		}
	}
	// On the bytes path, slices, arrays and maps return by themselves because
	// of topLevel. Only the stream path and bytes KindBytes need the trailing
	// return.
	if stream {
		b.WriteString("return result, nil\n")
	} else if s.AliasKind == model.KindBytes {
		b.WriteString("return result, i, nil\n")
	}
}

// renderAliasContainerAppendJSON emits the encode body for a slice, map or
// array alias. It uses the field-level append helpers, with the receiver `s`
// as the value.
func renderAliasContainerAppendJSON(b *bytes.Buffer, s model.StructInfo) {
	f := aliasContainerField(s)
	// Struct bodies declare this shared err too. A delegating element, such
	// as a nested ggen struct or a marshaler alias, emits `dst, err = …`, and
	// this path has no field loop that declares it.
	b.WriteString("var err error\n_ = err\n")
	//exhaustive:ignore not every kind applies here
	switch s.AliasKind {
	case model.KindSlice, model.KindArray:
		// renderAppendSlice handles both, using f.Kind and f.ArrayLen.
		renderAppendSlice(b, f, "s")
	case model.KindMap:
		renderAppendMap(b, f, "s")
	case model.KindBytes:
		renderAppendBytes(b, f, "s")
	}
	b.WriteString("return dst, nil\n")
}
