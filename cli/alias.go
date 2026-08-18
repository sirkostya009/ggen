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
	// Leading whitespace is legal before any top-level value; container and
	// struct aliases already skip it via ArrayOpen/ObjectOpen.
	inlineSkipWS(b, "i")
	switch s.AliasKind {
	case KindString:
		// copy: Detach clones iff the scan result aliases data (escape-path
		// results already own their bytes) — same shape as struct fields.
		detach := ""
		if s.Copy {
			detach = "v = scan.Detach(v, data)\n"
		}
		fmt.Fprintf(b, "var v string\nv, i, err = "+scanStringFn+"(data, i, "+vArgS(s)+")\n%s\n%sresult = %s(v)\n", wrap, detach, s.Name)
	case KindBool:
		fmt.Fprintf(b, "var v bool\nv, i, err = scan.Bool(data, i)\n%s\nresult = %s(v)\n", wrap, s.Name)
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		guard := narrowIntGuard("v", s.AliasUnderlying, `return result, i, decode.NewParseErr("", i, scan.ErrNumberOverflow)`)
		fmt.Fprintf(b, "var v int64\nv, i, err = scan.Int64(data, i)\n%s\n%sresult = %s(v)\n", wrap, guard, s.Name)
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		guard := narrowIntGuard("v", s.AliasUnderlying, `return result, i, decode.NewParseErr("", i, scan.ErrNumberOverflow)`)
		fmt.Fprintf(b, "var v uint64\nv, i, err = scan.Uint64(data, i)\n%s\n%sresult = %s(v)\n", wrap, guard, s.Name)
	case KindFloat32, KindFloat64:
		guard := narrowFloatGuard("v", s.AliasUnderlying, `return result, i, decode.NewParseErr("", i, scan.ErrNumberOverflow)`)
		fmt.Fprintf(b, "var v float64\nv, i, err = scan.Float64(data, i)\n%s\n%sresult = %s(v)\n", wrap, guard, s.Name)
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
	const wrap = `if err != nil { return result, decode.NewParseErr("", s.Offset(), err) }`
	b.WriteString("err = s.SkipSpace()\n" + wrap + "\n")
	switch s.AliasKind {
	case KindString:
		fmt.Fprintf(b, "var v string\nv, err = s.String("+vArgS(s)+")\n%s\nresult = %s(v)\n", wrap, s.Name)
	case KindBool:
		fmt.Fprintf(b, "var v bool\nv, err = s.Bool()\n%s\nresult = %s(v)\n", wrap, s.Name)
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		guard := narrowIntGuard("v", s.AliasUnderlying, `return result, decode.NewParseErr("", s.Offset(), scan.ErrNumberOverflow)`)
		fmt.Fprintf(b, "var v int64\nv, err = s.Int64()\n%s\n%sresult = %s(v)\n", wrap, guard, s.Name)
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		guard := narrowIntGuard("v", s.AliasUnderlying, `return result, decode.NewParseErr("", s.Offset(), scan.ErrNumberOverflow)`)
		fmt.Fprintf(b, "var v uint64\nv, err = s.Uint64()\n%s\n%sresult = %s(v)\n", wrap, guard, s.Name)
	case KindFloat32, KindFloat64:
		guard := narrowFloatGuard("v", s.AliasUnderlying, `return result, decode.NewParseErr("", s.Offset(), scan.ErrNumberOverflow)`)
		fmt.Fprintf(b, "var v float64\nv, err = s.Float64()\n%s\n%sresult = %s(v)\n", wrap, guard, s.Name)
	}
	b.WriteString("return result, nil\n")
}

// renderAliasSize returns an upper-bound JSONSize body for the alias.
func renderAliasSize(s StructInfo) string {
	switch s.AliasKind {
	case KindString:
		// Worst-case escapes (×2; ×6 under htmlescape — `<` → <) + 2 quotes.
		return fmt.Sprintf("return len(string(s))*%d + 2\n", strMult(s.HTMLEscape))
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
		// Same per-kind machinery as a struct FIELD of this shape — the old
		// flat 1024 under-reserved any real container (growth chain past it)
		// and over-reserved small ones.
		f := aliasContainerField(s)
		n, code := sizeContrib(f, "s")
		return fmt.Sprintf("size := %d\n%sreturn size\n", n, code)
	case KindStruct:
		switch {
		case s.AliasIface.AppendJSON && s.AliasIface.JSONSize:
			// ggen-method delegation pairs with the underlying's real bound.
			return fmt.Sprintf("return %s(s).JSONSize()\n", s.AliasUnderlying)
		case s.AliasIface.JSONMarshaler, s.AliasIface.TextMarshaler:
			// Delegation allocates its own result; a nonzero budget here would
			// only add a make() alloc in front of it (and a third past-cap).
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
		// encode.AppendFloat: stdlib-parity format, errors on NaN/Inf instead
		// of emitting invalid JSON — same routing as struct float fields.
		b.WriteString("return encode.AppendFloat(dst, float64(s), 32)\n")
	case KindFloat64:
		b.WriteString("return encode.AppendFloat(dst, float64(s), 64)\n")
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
		// Cyclic underlying: thread depth through the delegation, else an
		// alias hop inside a type cycle would reset the budget.
		call := "DecodeFrom(data[i:])"
		if isCyclic(s.AliasUnderlying) {
			call = "decodeFromDepth(data[i:], _depth+1)"
		}
		fmt.Fprintf(b, `var u %[1]s
v, _n, err := u.`+call+`
i += _n
if err != nil { return result, i, decode.NewParseErrShift("", i, _n, err) }
result = %[2]s(v)
return result, i, nil
`, s.AliasUnderlying, s.Name)
	case s.AliasIface.StreamDecoder && stream:
		call := "DecodeFromStream(s)"
		if isCyclic(s.AliasUnderlying) {
			call = "decodeFromStreamDepth(s, _depth+1)"
		}
		fmt.Fprintf(b, `var u %[1]s
v, err := u.`+call+`
if err != nil { return result, decode.NewParseErr("", s.Offset(), err) }
result = %[2]s(v)
return result, nil
`, s.AliasUnderlying, s.Name)
	case s.AliasIface.JSONUnmarshaler:
		if stream {
			fmt.Fprintf(b, `span, err := s.CaptureValue()
if err != nil { return result, decode.NewParseErr("", s.Offset(), err) }
var u %[1]s
if err := u.UnmarshalJSON(span); err != nil { return result, decode.NewParseErr("", s.Offset(), err) }
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
if err != nil { return result, decode.NewParseErr("", s.Offset(), err) }
var u %[1]s
if err := u.UnmarshalText(unsafe.Slice(unsafe.StringData(ts), len(ts))); err != nil { return result, decode.NewParseErr("", s.Offset(), err) }
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
		// Empty result errors (stdlib parity), like the cross-package arm.
		fmt.Fprintf(b, `bs, err := %s(s).MarshalJSON()
if err != nil { return dst, err }
if len(bs) == 0 { return dst, encode.ErrEmptyMarshalJSON }
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
dst = %s(dst, encode.BytesToString(t))
return dst, nil
`, s.AliasUnderlying, appendStrFn(s.HTMLEscape))
	default:
		b.WriteString("return dst, nil\n")
	}
}

// aliasContainerField returns s.AliasField with the struct-level flags
// stamped on — the alias field is built at parse time, BEFORE annotation/CLI
// flag propagation (which only walks Fields), so reading it raw silently
// dropped copy/htmlescape/allowinvalidutf8/multierr on container aliases.
func aliasContainerField(s StructInfo) FieldInfo {
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
	f := aliasContainerField(s)
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
	f := aliasContainerField(s)
	// Same shared slot the struct body declares: a delegating element
	// (nested ggen struct, marshaler alias) emits `dst, err = …`, and this
	// path has no field loop to have declared it.
	b.WriteString("var err error\n_ = err\n")
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
