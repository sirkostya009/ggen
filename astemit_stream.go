package main

// AST emitters for the stream-path renderers. Hand-built ast.Node trees
// throughout — no bridgeStmts, no parser detours. Stream methods on
// scan.Stream are invoked as `s.<method>(args)` and follow uniform
// shapes captured by the helpers below.

import (
	"fmt"
	"go/ast"
	"go/token"
)

// --- stream primitive builders ----------------------------------------

// streamCall builds `s.<method>(args...)`.
func streamCall(method string, args ...ast.Expr) *ast.CallExpr {
	return call(idSel("s", method), args...)
}

// streamBytes builds `s.Bytes()`.
func streamBytes() ast.Expr { return streamCall("Bytes") }

// streamBytesAt builds `s.Bytes()[<pv>]`.
func streamBytesAt(pv ast.Expr) ast.Expr {
	return index(streamBytes(), pv)
}

// streamReadMore builds the `if pv >= len(s.Bytes()) { … s.ReadMore(0) … }`
// guard. Each stream scanner pulls more bytes via this idiom before reading.
func streamReadMore(posVar string) ast.Stmt {
	pv := id(posVar)
	return ifStmt(
		binop(pv, token.GEQ, idCall("len", streamBytes())),
		&ast.IfStmt{
			Init: assign(id("err"), streamCall("ReadMore", intLit(0))),
			Cond: binop(id("err"), token.NEQ, id("nil")),
			Body: block(retResultIErr()),
		},
	)
}

// streamSkipSpace emits `pv, err = s.SkipSpace(pv); if err != nil { return result, i, err }`.
func streamSkipSpace(posVar string) []ast.Stmt {
	pv := id(posVar)
	return []ast.Stmt{
		assignN([]ast.Expr{pv, id("err")}, streamCall("SkipSpace", pv)),
		ifErrReturn(),
	}
}

// streamSkipSpaceFrom emits `pv, err = s.SkipSpace(<from>)` — used for the
// post-comma SkipSpace(pv + 1) idiom.
func streamSkipSpaceFrom(posVar string, from ast.Expr) []ast.Stmt {
	pv := id(posVar)
	return []ast.Stmt{
		assignN([]ast.Expr{pv, id("err")}, streamCall("SkipSpace", from)),
		ifErrReturn(),
	}
}

// streamScan emits `lhs..., pv, err = s.<method>(<args>); if err != nil { return ... }`.
func streamScan(lhs []ast.Expr, method string, posVar string, args ...ast.Expr) []ast.Stmt {
	pv := id(posVar)
	all := append(append([]ast.Expr{}, lhs...), pv, id("err"))
	return []ast.Stmt{
		&ast.AssignStmt{Lhs: all, Tok: token.ASSIGN, Rhs: []ast.Expr{streamCall(method, args...)}},
		ifErrReturn(),
	}
}

// streamNullPeek builds the multi-byte `null` literal check used by stream
// scanners. Stream can't peek 4 bytes at once (buffer may need ReadMore
// between bytes), so it scans byte-by-byte: confirm 'n', then loop ki:=1..3
// confirming the remaining "null" chars, ReadMore as needed, advance pv+=4
// on success, run nullBody. Non-null path runs elseBody.
func streamNullPeek(posVar string, nullBody, elseBody []ast.Stmt) ast.Stmt {
	pv := id(posVar)
	loop := forLoop(
		shortDecl(id("ki"), intLit(1)),
		binop(id("ki"), token.LSS, intLit(4)),
		inc(id("ki")),
		ifStmt(
			binop(binop(pv, token.ADD, id("ki")), token.GEQ, idCall("len", streamBytes())),
			&ast.IfStmt{
				Init: assign(id("err"), streamCall("ReadMore", intLit(0))),
				Cond: binop(id("err"), token.NEQ, id("nil")),
				Body: block(retResultIErr()),
			},
		),
		ifStmt(
			binop(
				index(streamBytes(), binop(pv, token.ADD, id("ki"))),
				token.NEQ,
				index(&ast.BasicLit{Kind: token.STRING, Value: `"null"`}, id("ki")),
			),
			retResultIErrExpr(idSel("scan", "ErrBadLiteral")),
		),
	)
	full := append([]ast.Stmt{loop, incBy(posVar, 4)}, nullBody...)
	cond := binop(streamBytesAt(pv), token.EQL, charLit('n'))
	return &ast.IfStmt{
		Cond: cond,
		Body: block(full...),
		Else: blockOf(elseBody),
	}
}

// streamCommaOrBreak emits the standard `,` / break tail at the end of a
// stream loop body: SkipSpace, ReadMore, if `,` → SkipSpace(pv+1) + continue,
// else break.
func streamCommaOrBreak(posVar string) []ast.Stmt {
	pv := id(posVar)
	cont := streamSkipSpaceFrom(posVar, binop(pv, token.ADD, intLit(1)))
	cont = append(cont, &ast.BranchStmt{Tok: token.CONTINUE})
	out := streamSkipSpace(posVar)
	out = append(out, streamReadMore(posVar))
	out = append(out,
		ifStmt(
			binop(streamBytesAt(pv), token.EQL, charLit(',')),
			cont...,
		),
		&ast.BranchStmt{Tok: token.BREAK},
	)
	return out
}

// --- stream native-type renderers -------------------------------------

// renderStreamBytesStmts mirrors renderStreamBytes (hand-built AST).
func renderStreamBytesStmts(f FieldInfo, ref string, posVar string) []ast.Stmt {
	pv := id(posVar)
	r := func() ast.Expr { return parseExpr(ref) }

	if f.Format == "array" {
		body := []ast.Stmt{}
		body = append(body, streamScan(nil, "ArrayOpen", posVar, pv)...)
		body = append(body, streamSkipSpace(posVar)...)
		body = append(body, streamReadMore(posVar))

		loop := []ast.Stmt{varDecl("v", "uint64")}
		loop = append(loop, streamScan([]ast.Expr{id("v")}, "Uint64", posVar, pv)...)
		loop = append(loop, assign(r(), call(id("append"), r(), call(id("byte"), id("v")))))
		loop = append(loop, streamCommaOrBreak(posVar)...)
		body = append(body, forCond(
			binop(streamBytesAt(pv), token.NEQ, charLit(']')),
			loop...,
		))
		body = append(body,
			ifStmt(
				binop(streamBytesAt(pv), token.NEQ, charLit(']')),
				retResultIErrExpr(idSel("scan", "ErrBadArray")),
			),
			inc(pv),
		)
		return []ast.Stmt{block(body...)}
	}

	// base64 / base32 / hex paths.
	var encDecode, encDecLen ast.Expr
	switch f.Format {
	case "base64url":
		encDecode = sel(idSel("base64", "URLEncoding"), "AppendDecode")
		encDecLen = sel(idSel("base64", "URLEncoding"), "DecodedLen")
	case "base32":
		encDecode = sel(idSel("base32", "StdEncoding"), "AppendDecode")
		encDecLen = sel(idSel("base32", "StdEncoding"), "DecodedLen")
	case "base32hex":
		encDecode = sel(idSel("base32", "HexEncoding"), "AppendDecode")
		encDecLen = sel(idSel("base32", "HexEncoding"), "DecodedLen")
	case "base16", "hex":
		encDecode = idSel("hex", "AppendDecode")
		encDecLen = idSel("hex", "DecodedLen")
	default:
		encDecode = sel(idSel("base64", "StdEncoding"), "AppendDecode")
		encDecLen = sel(idSel("base64", "StdEncoding"), "DecodedLen")
	}
	sliceCall := call(idSel("unsafe", "Slice"),
		call(idSel("unsafe", "StringData"), id("v")),
		idCall("len", id("v")),
	)
	body := []ast.Stmt{varDecl("v", "string")}
	body = append(body, streamScan([]ast.Expr{id("v")}, "String", posVar, pv)...)
	body = append(body, assign(r(),
		call(id("make"), &ast.ArrayType{Elt: id("byte")}, intLit(0), call(encDecLen, idCall("len", id("v")))),
	))
	body = append(body, assignN([]ast.Expr{r(), id("err")},
		call(encDecode, r(), sliceCall),
	), ifErrReturn())
	return []ast.Stmt{block(body...)}
}

// renderStreamTimeStmts mirrors renderStreamTime.
func renderStreamTimeStmts(f FieldInfo, ref string, posVar string) []ast.Stmt {
	pv := id(posVar)
	r := func() ast.Expr { return parseExpr(ref) }
	layout, numeric := timeLayoutExpr(f.Format)
	if numeric == "Unix" {
		body := []ast.Stmt{varDecl("f", "float64")}
		body = append(body, streamScan([]ast.Expr{id("f")}, "Float64", posVar, pv)...)
		body = append(body, shortDecl(id("sec"), call(id("int64"), id("f"))))
		body = append(body, shortDecl(id("nsec"), call(id("int64"),
			binop(
				paren(binop(id("f"), token.SUB, call(id("float64"), id("sec")))),
				token.MUL, parseExpr("1e9"),
			),
		)))
		body = append(body, assign(r(), call(idSel("time", "Unix"), id("sec"), id("nsec"))))
		return []ast.Stmt{block(body...)}
	}
	if numeric != "" {
		var ctor ast.Expr
		switch numeric {
		case "UnixMilli":
			ctor = call(idSel("time", "UnixMilli"), id("n"))
		case "UnixMicro":
			ctor = call(idSel("time", "UnixMicro"), id("n"))
		case "UnixNano":
			ctor = call(idSel("time", "Unix"), intLit(0), id("n"))
		}
		body := []ast.Stmt{varDecl("n", "int64")}
		body = append(body, streamScan([]ast.Expr{id("n")}, "Int64", posVar, pv)...)
		body = append(body, assign(r(), ctor))
		return []ast.Stmt{block(body...)}
	}
	body := []ast.Stmt{varDecl("v", "string")}
	body = append(body, streamScan([]ast.Expr{id("v")}, "String", posVar, pv)...)
	body = append(body, assignN([]ast.Expr{r(), id("err")},
		call(idSel("time", "Parse"), parseExpr(layout), id("v")),
	), ifErrReturn())
	return []ast.Stmt{block(body...)}
}

// renderStreamDurationStmts mirrors renderStreamDuration.
func renderStreamDurationStmts(f FieldInfo, ref string, posVar string) []ast.Stmt {
	pv := id(posVar)
	r := func() ast.Expr { return parseExpr(ref) }
	switch f.Format {
	case "sec":
		body := []ast.Stmt{varDecl("v", "float64")}
		body = append(body, streamScan([]ast.Expr{id("v")}, "Float64", posVar, pv)...)
		body = append(body, assign(r(), call(idSel("time", "Duration"),
			binop(id("v"), token.MUL, call(id("float64"), idSel("time", "Second"))),
		)))
		return []ast.Stmt{block(body...)}
	case "milli", "micro", "nano":
		unitMap := map[string]string{
			"milli": "Millisecond",
			"micro": "Microsecond",
			"nano":  "Nanosecond",
		}
		body := []ast.Stmt{varDecl("n", "int64")}
		body = append(body, streamScan([]ast.Expr{id("n")}, "Int64", posVar, pv)...)
		body = append(body, assign(r(), binop(
			call(idSel("time", "Duration"), id("n")),
			token.MUL, idSel("time", unitMap[f.Format]),
		)))
		return []ast.Stmt{block(body...)}
	}
	body := []ast.Stmt{varDecl("v", "string")}
	body = append(body, streamScan([]ast.Expr{id("v")}, "String", posVar, pv)...)
	body = append(body, assignN([]ast.Expr{r(), id("err")},
		call(idSel("time", "ParseDuration"), id("v")),
	), ifErrReturn())
	return []ast.Stmt{block(body...)}
}

// renderStreamNetIPStmts / NetipAddr / NetipPrefix.
func renderStreamNetIPStmts(ref string, posVar string) []ast.Stmt {
	pv := id(posVar)
	r := func() ast.Expr { return parseExpr(ref) }
	body := []ast.Stmt{varDecl("v", "string")}
	body = append(body, streamScan([]ast.Expr{id("v")}, "String", posVar, pv)...)
	body = append(body, assign(r(), call(idSel("net", "ParseIP"), id("v"))))
	body = append(body, ifStmt(
		binop(r(), token.EQL, id("nil")),
		retResultIErrExpr(call(idSel("fmt", "Errorf"), strLit("invalid IP"))),
	))
	return []ast.Stmt{block(body...)}
}

func renderStreamNetipAddrStmts(ref string, posVar string) []ast.Stmt {
	pv := id(posVar)
	r := func() ast.Expr { return parseExpr(ref) }
	body := []ast.Stmt{varDecl("v", "string")}
	body = append(body, streamScan([]ast.Expr{id("v")}, "String", posVar, pv)...)
	body = append(body, assignN([]ast.Expr{r(), id("err")},
		call(idSel("netip", "ParseAddr"), id("v")),
	), ifErrReturn())
	return []ast.Stmt{block(body...)}
}

func renderStreamNetipPrefixStmts(ref string, posVar string) []ast.Stmt {
	pv := id(posVar)
	r := func() ast.Expr { return parseExpr(ref) }
	body := []ast.Stmt{varDecl("v", "string")}
	body = append(body, streamScan([]ast.Expr{id("v")}, "String", posVar, pv)...)
	body = append(body, assignN([]ast.Expr{r(), id("err")},
		call(idSel("netip", "ParsePrefix"), id("v")),
	), ifErrReturn())
	return []ast.Stmt{block(body...)}
}

// renderStreamRawJSONStmts pins the buffer, SkipValue, copy out.
func renderStreamRawJSONStmts(ref string, posVar string) []ast.Stmt {
	pv := id(posVar)
	r := func() ast.Expr { return parseExpr(ref) }
	return []ast.Stmt{block(
		shortDecl(id("start"), pv),
		shortDecl(id("prevPin"), idSel("s", "Shift")),
		assign(idSel("s", "Shift"), id("false")),
		assignN([]ast.Expr{pv, id("err")}, streamCall("SkipValue", id("start"))),
		assign(idSel("s", "Shift"), id("prevPin")),
		ifErrReturn(),
		shortDecl(id("raw"), slice2(streamBytes(), id("start"), pv)),
		assign(r(), call(id("append"),
			call(id("make"), &ast.ArrayType{Elt: id("byte")}, intLit(0), idCall("len", id("raw"))),
			&ast.BasicLit{Kind: token.STRING, Value: "raw..."},
		)),
	)}
}

// renderStreamURLStmts mirrors renderStreamURL.
func renderStreamURLStmts(ref string, posVar string) []ast.Stmt {
	pv := id(posVar)
	r := func() ast.Expr { return parseExpr(ref) }
	body := []ast.Stmt{varDecl("v", "string")}
	body = append(body, streamScan([]ast.Expr{id("v")}, "String", posVar, pv)...)
	body = append(body,
		shortDeclN(
			[]ast.Expr{id("u"), id("err")},
			call(idSel("url", "Parse"), id("v")),
		),
		ifErrReturn(),
		assign(r(), star(id("u"))),
	)
	return []ast.Stmt{block(body...)}
}

// renderStreamBigIntStmts mirrors renderStreamBigInt.
func renderStreamBigIntStmts(ref string, posVar string) []ast.Stmt {
	pv := id(posVar)
	r := func() ast.Expr { return parseExpr(ref) }
	setStr := call(
		sel(paren(addr(r())), "SetString"),
		call(idSel("unsafe", "String"),
			call(idSel("unsafe", "SliceData"), slice2(id("buf"), id("start"), nil)),
			binop(pv, token.SUB, id("start")),
		),
		intLit(10),
	)
	return []ast.Stmt{block(
		shortDecl(id("start"), pv),
		shortDecl(id("prevPin"), idSel("s", "Shift")),
		assign(idSel("s", "Shift"), id("false")),
		assignN([]ast.Expr{pv, id("err")}, streamCall("SkipValue", id("start"))),
		assign(idSel("s", "Shift"), id("prevPin")),
		ifErrReturn(),
		shortDecl(id("buf"), streamBytes()),
		&ast.IfStmt{
			Init: shortDeclN([]ast.Expr{id("_"), id("ok")}, setStr),
			Cond: not(id("ok")),
			Body: block(retResultIErrExpr(idSel("scan", "ErrBadNumber"))),
		},
	)}
}

// renderStreamBigFloatStmts mirrors renderStreamBigFloat.
func renderStreamBigFloatStmts(ref string, posVar string) []ast.Stmt {
	pv := id(posVar)
	r := func() ast.Expr { return parseExpr(ref) }
	parseCall := call(sel(paren(addr(r())), "Parse"), id("v"), intLit(10))
	body := []ast.Stmt{varDecl("v", "string")}
	body = append(body, streamScan([]ast.Expr{id("v")}, "String", posVar, pv)...)
	body = append(body, &ast.IfStmt{
		Init: shortDeclN([]ast.Expr{id("_"), id("_"), id("err")}, parseCall),
		Cond: binop(id("err"), token.NEQ, id("nil")),
		Body: block(retResultIErr()),
	})
	return []ast.Stmt{block(body...)}
}

// renderStreamBigRatStmts mirrors renderStreamBigRat.
func renderStreamBigRatStmts(ref string, posVar string) []ast.Stmt {
	pv := id(posVar)
	r := func() ast.Expr { return parseExpr(ref) }
	setStr := call(sel(paren(addr(r())), "SetString"), id("v"))
	body := []ast.Stmt{varDecl("v", "string")}
	body = append(body, streamScan([]ast.Expr{id("v")}, "String", posVar, pv)...)
	body = append(body, &ast.IfStmt{
		Init: shortDeclN([]ast.Expr{id("_"), id("ok")}, setStr),
		Cond: not(id("ok")),
		Body: block(retResultIErrExpr(idSel("scan", "ErrBadNumber"))),
	})
	return []ast.Stmt{block(body...)}
}

// renderStreamSQLNullStmts mirrors renderStreamSQLNull.
func renderStreamSQLNullStmts(f FieldInfo, ref string, posVar string) []ast.Stmt {
	spec, ok := SQLNullSpec(f.GoType)
	if !ok {
		return nil
	}
	pv := id(posVar)
	r := func() ast.Expr { return parseExpr(ref) }
	nv := id("nv")

	// elseBody: read inner value, assign sql.X{Field: nv-or-cast, Valid: true}
	var elseBody []ast.Stmt
	var valExpr ast.Expr
	switch spec.Inner {
	case KindString:
		elseBody = append(elseBody, varDecl("nv", "string"))
		elseBody = append(elseBody, streamScan([]ast.Expr{nv}, "String", posVar, pv)...)
		valExpr = nv
	case KindBool:
		elseBody = append(elseBody, varDecl("nv", "bool"))
		elseBody = append(elseBody, streamScan([]ast.Expr{nv}, "Bool", posVar, pv)...)
		valExpr = nv
	case KindInt64, KindInt32, KindInt16:
		elseBody = append(elseBody, varDecl("nv", "int64"))
		elseBody = append(elseBody, streamScan([]ast.Expr{nv}, "Int64", posVar, pv)...)
		if spec.Type != "int64" {
			valExpr = call(id(spec.Type), nv)
		} else {
			valExpr = nv
		}
	case KindUint8:
		elseBody = append(elseBody, varDecl("nv", "uint64"))
		elseBody = append(elseBody, streamScan([]ast.Expr{nv}, "Uint64", posVar, pv)...)
		valExpr = call(id(spec.Type), nv)
	case KindFloat64:
		elseBody = append(elseBody, varDecl("nv", "float64"))
		elseBody = append(elseBody, streamScan([]ast.Expr{nv}, "Float64", posVar, pv)...)
		valExpr = nv
	case KindTime:
		tf := FieldInfo{Format: f.Format}
		elseBody = append(elseBody, varDecl("nv", "time.Time"))
		elseBody = append(elseBody, renderStreamTimeStmts(tf, "nv", posVar)...)
		valExpr = nv
	}
	validLit := &ast.CompositeLit{
		Type: idSel("sql", sqlTypeName(f.GoType)),
		Elts: []ast.Expr{
			&ast.KeyValueExpr{Key: id(spec.Field), Value: valExpr},
			&ast.KeyValueExpr{Key: id("Valid"), Value: id("true")},
		},
	}
	elseBody = append(elseBody, assign(r(), validLit))

	nullBranch := []ast.Stmt{assign(r(), &ast.CompositeLit{Type: idSel("sql", sqlTypeName(f.GoType))})}
	body := []ast.Stmt{streamReadMore(posVar), streamNullPeek(posVar, nullBranch, elseBody)}
	return []ast.Stmt{block(body...)}
}

// renderStreamAnyStmts mirrors renderStreamAny.
func renderStreamAnyStmts(f FieldInfo, ref string, posVar string) []ast.Stmt {
	pv := id(posVar)
	r := func() ast.Expr { return parseExpr(ref) }
	method := "Any"
	if f.UseNumber {
		method = "AnyNumber"
	}
	return []ast.Stmt{block(
		assignN([]ast.Expr{r(), pv, id("err")}, streamCall(method, pv)),
		ifErrReturn(),
	)}
}

// streamUnknownKeyStmts mirrors streamUnknownKey.
func streamUnknownKeyStmts(s StructInfo, posVar string) []ast.Stmt {
	pv := id(posVar)
	if inline := s.InlineField(); inline.Inline {
		anyMethod := "Any"
		if s.UseNumber {
			anyMethod = "AnyNumber"
		}
		mapAccess := idSel("result", inline.GoName)
		mapIdx := &ast.IndexExpr{X: mapAccess, Index: id("ownKey")}
		return []ast.Stmt{
			shortDecl(id("ownKey"), call(idSel("strings", "Clone"), id("key"))),
			assignN([]ast.Expr{pv, id("err")}, streamCall("ConsumeColon", pv)),
			ifErrReturn(),
			ifStmt(binop(mapAccess, token.EQL, id("nil")),
				assign(mapAccess, call(id("make"), parseExpr(inline.GoType))),
			),
			assignN([]ast.Expr{mapIdx, pv, id("err")}, streamCall(anyMethod, pv)),
			ifErrReturn(),
		}
	}
	if s.IgnoreUnknown {
		out := []ast.Stmt{}
		out = append(out, assignN([]ast.Expr{pv, id("err")}, streamCall("ConsumeColon", pv)))
		out = append(out, ifErrReturn())
		out = append(out, assignN([]ast.Expr{pv, id("err")}, streamCall("SkipValue", pv)))
		out = append(out, ifErrReturn())
		return out
	}
	if s.MultiErr {
		out := []ast.Stmt{
			assign(id("errs"), call(id("append"), id("errs"),
				validationErrLit("UnknownKeyError",
					fieldKV("Field", call(idSel("strings", "Clone"), id("key"))),
				),
			)),
			assignN([]ast.Expr{pv, id("err")}, streamCall("ConsumeColon", pv)),
			ifErrReturn(),
			assignN([]ast.Expr{pv, id("err")}, streamCall("SkipValue", pv)),
			ifErrReturn(),
		}
		return out
	}
	return []ast.Stmt{
		retResultIErrExpr(validationErrLit("UnknownKeyError",
			fieldKV("Field", call(idSel("strings", "Clone"), id("key"))),
		)),
	}
}

// renderStreamStringTagStmts mirrors renderStreamStringTag.
func renderStreamStringTagStmts(f FieldInfo, ref string, posVar string) []ast.Stmt {
	pv := id(posVar)
	r := func() ast.Expr { return parseExpr(ref) }
	body := []ast.Stmt{varDecl("sv", "string")}
	body = append(body, streamScan([]ast.Expr{id("sv")}, "KeyView", posVar, pv)...)

	switch f.Kind {
	case KindBool:
		body = append(body, &ast.SwitchStmt{
			Tag: id("sv"),
			Body: block(
				&ast.CaseClause{
					List: []ast.Expr{strLit("true")},
					Body: []ast.Stmt{assign(r(), id("true"))},
				},
				&ast.CaseClause{
					List: []ast.Expr{strLit("false")},
					Body: []ast.Stmt{assign(r(), id("false"))},
				},
				&ast.CaseClause{
					List: nil,
					Body: []ast.Stmt{retResultIErrExpr(idSel("scan", "ErrBadBool"))},
				},
			),
		})
	case KindFloat32, KindFloat64:
		body = append(body,
			shortDeclN([]ast.Expr{id("f"), id("err")},
				call(idSel("strconv", "ParseFloat"), id("sv"), intLit(64)),
			),
			ifErrReturn(),
		)
		if f.Kind == KindFloat32 {
			body = append(body, assign(r(), call(id("float32"), id("f"))))
		} else {
			body = append(body, assign(r(), id("f")))
		}
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		body = append(body,
			shortDeclN([]ast.Expr{id("u"), id("err")},
				call(idSel("strconv", "ParseUint"), id("sv"), intLit(10), intLit(64)),
			),
			ifErrReturn(),
		)
		if f.Kind == KindUint64 {
			body = append(body, assign(r(), id("u")))
		} else {
			body = append(body, assign(r(), call(id(f.GoType), id("u"))))
		}
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		body = append(body,
			shortDeclN([]ast.Expr{id("n"), id("err")},
				call(idSel("strconv", "ParseInt"), id("sv"), intLit(10), intLit(64)),
			),
			ifErrReturn(),
		)
		if f.Kind == KindInt64 {
			body = append(body, assign(r(), id("n")))
		} else {
			body = append(body, assign(r(), call(id(f.GoType), id("n"))))
		}
	case KindString:
		body = append(body, assign(r(), call(id("string"), id("sv"))))
	}
	return []ast.Stmt{block(body...)}
}

// --- stream containers: map, slice/array ------------------------------

// renderStreamMapStmts mirrors renderStreamMap. Null peek; else
// ObjectOpen, SkipSpace, empty-or-make branch, per-entry loop (scan key,
// key validation, colon, scan value, dive mods/validation, comma-or-end).
func renderStreamMapStmts(f FieldInfo, ref, posVar string) []ast.Stmt {
	pv := id(posVar)
	r := func() ast.Expr { return parseExpr(ref) }

	var makeExpr ast.Expr
	if cap := mapPreallocCap(f); cap > 0 {
		makeExpr = call(id("make"), parseExpr(f.GoType), intLit(cap))
	} else {
		makeExpr = call(id("make"), parseExpr(f.GoType))
	}
	mapTarget := func() ast.Expr {
		return &ast.IndexExpr{X: r(), Index: id("mk")}
	}

	// value scan
	var valueScan []ast.Stmt
	switch f.ElemKind {
	case KindString:
		valueScan = []ast.Stmt{varDecl("mv", "string")}
		valueScan = append(valueScan, streamScan([]ast.Expr{id("mv")}, "String", posVar, pv)...)
		valueScan = append(valueScan, assign(mapTarget(), id("mv")))
	case KindBool:
		valueScan = []ast.Stmt{varDecl("mv", "bool")}
		valueScan = append(valueScan, streamScan([]ast.Expr{id("mv")}, "Bool", posVar, pv)...)
		valueScan = append(valueScan, assign(mapTarget(), id("mv")))
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		valueScan = []ast.Stmt{varDecl("mn", "int64")}
		valueScan = append(valueScan, streamScan([]ast.Expr{id("mn")}, "Int64", posVar, pv)...)
		if f.ElemType != "int64" {
			valueScan = append(valueScan, assign(mapTarget(), call(id(f.ElemType), id("mn"))))
		} else {
			valueScan = append(valueScan, assign(mapTarget(), id("mn")))
		}
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		valueScan = []ast.Stmt{varDecl("mn", "uint64")}
		valueScan = append(valueScan, streamScan([]ast.Expr{id("mn")}, "Uint64", posVar, pv)...)
		if f.ElemType != "uint64" {
			valueScan = append(valueScan, assign(mapTarget(), call(id(f.ElemType), id("mn"))))
		} else {
			valueScan = append(valueScan, assign(mapTarget(), id("mn")))
		}
	case KindFloat32, KindFloat64:
		valueScan = []ast.Stmt{varDecl("mv", "float64")}
		valueScan = append(valueScan, streamScan([]ast.Expr{id("mv")}, "Float64", posVar, pv)...)
		if f.ElemKind == KindFloat32 {
			valueScan = append(valueScan, assign(mapTarget(), call(id("float32"), id("mv"))))
		} else {
			valueScan = append(valueScan, assign(mapTarget(), id("mv")))
		}
	case KindStruct:
		if isGenerated(f.ElemType) {
			// mv.DecodeStreamFrom(s, posVar) — receiver is mv, not s
			valueScan = []ast.Stmt{varDecl("mv", f.ElemType)}
			valueScan = append(valueScan,
				assignN(
					[]ast.Expr{id("mv"), pv, id("err")},
					call(sel(id("mv"), "DecodeStreamFrom"), id("s"), pv),
				),
				ifErrReturn(),
				assign(mapTarget(), id("mv")),
			)
		} else {
			valueScan = []ast.Stmt{
				shortDecl(id("start"), pv),
				shortDecl(id("prevPin"), idSel("s", "Shift")),
				assign(idSel("s", "Shift"), id("false")),
				assignN([]ast.Expr{pv, id("err")}, streamCall("SkipValue", id("start"))),
				assign(idSel("s", "Shift"), id("prevPin")),
				ifErrReturn(),
				varDecl("mv", f.ElemType),
				&ast.IfStmt{
					Init: shortDecl(id("err"), call(idSel("json", "Unmarshal"),
						slice2(streamBytes(), id("start"), pv),
						addr(id("mv")),
					)),
					Cond: binop(id("err"), token.NEQ, id("nil")),
					Body: block(retResultIErr()),
				},
				assign(mapTarget(), id("mv")),
			}
		}
	default:
		valueScan = []ast.Stmt{}
		valueScan = append(valueScan, assignN([]ast.Expr{pv, id("err")}, streamCall("SkipValue", pv)))
		valueScan = append(valueScan, ifErrReturn())
	}

	if len(f.ElemMods) > 0 {
		valueScan = append(valueScan, renderModsStmts(f.ElemMods, mapTarget(), f.ElemType, f.ElemKind)...)
	}
	if len(f.ElemValidation) > 0 {
		valueScan = append(valueScan, renderValidationOnStmts(f.ElemValidation, mapTarget(), f.JSONName+".value", f.ElemKind, f.MultiErr, "i")...)
	}

	// per-entry loop body
	loopBody := []ast.Stmt{varDecl("mk", "string")}
	loopBody = append(loopBody, streamScan([]ast.Expr{id("mk")}, "String", posVar, pv)...)
	if !f.NoValidate {
		if len(f.KeyMods) > 0 {
			loopBody = append(loopBody, renderModsStmts(f.KeyMods, id("mk"), "string", KindString)...)
		}
		if len(f.KeyValidation) > 0 {
			loopBody = append(loopBody, renderValidationOnStmts(f.KeyValidation, id("mk"), f.JSONName+".key", KindString, f.MultiErr, "i")...)
		}
	}
	loopBody = append(loopBody, streamSkipSpace(posVar)...)
	loopBody = append(loopBody, streamReadMore(posVar))
	loopBody = append(loopBody, ifStmt(
		binop(streamBytesAt(pv), token.NEQ, charLit(':')),
		retResultIErrExpr(idSel("scan", "ErrBadObject")),
	))
	loopBody = append(loopBody, streamSkipSpaceFrom(posVar, binop(pv, token.ADD, intLit(1)))...)
	loopBody = append(loopBody, valueScan...)
	loopBody = append(loopBody, streamSkipSpace(posVar)...)
	loopBody = append(loopBody, streamReadMore(posVar))
	contArm := streamSkipSpaceFrom(posVar, binop(pv, token.ADD, intLit(1)))
	contArm = append(contArm, &ast.BranchStmt{Tok: token.CONTINUE})
	loopBody = append(loopBody,
		ifStmt(binop(streamBytesAt(pv), token.EQL, charLit(',')), contArm...),
		&ast.BranchStmt{Tok: token.BREAK},
	)

	// non-null path
	nonNull := []ast.Stmt{}
	nonNull = append(nonNull, assignN([]ast.Expr{pv, id("err")}, streamCall("ObjectOpen", pv)))
	nonNull = append(nonNull, ifErrReturn())
	nonNull = append(nonNull, streamSkipSpace(posVar)...)
	nonNull = append(nonNull, streamReadMore(posVar))
	nonNull = append(nonNull, ifElse(
		binop(streamBytesAt(pv), token.EQL, charLit('}')),
		[]ast.Stmt{assign(r(), &ast.CompositeLit{Type: parseExpr(f.GoType)})},
		[]ast.Stmt{assign(r(), makeExpr)},
	))
	nonNull = append(nonNull, forCond(
		binop(streamBytesAt(pv), token.NEQ, charLit('}')),
		loopBody...,
	))
	nonNull = append(nonNull, ifStmt(
		binop(streamBytesAt(pv), token.NEQ, charLit('}')),
		retResultIErrExpr(idSel("scan", "ErrBadObject")),
	), inc(pv))

	// outer: SkipSpace, ReadMore, NullPeek(nil, nonNull)
	body := streamSkipSpace(posVar)
	body = append(body, streamReadMore(posVar))
	body = append(body, streamNullPeek(posVar, nil, nonNull))
	return []ast.Stmt{block(body...)}
}

// emitStreamSliceReadStmts mirrors emitStreamSliceRead. Recursive.
func emitStreamSliceReadStmts(f FieldInfo, dst string, posVar string, depth int) []ast.Stmt {
	pv := id(posVar)
	d := func() ast.Expr { return parseExpr(dst) }
	isArray := f.Kind == KindArray
	arrayN := f.ArrayLen
	ivar := id(fmt.Sprintf("idx%d", depth))
	slabVar := id(fmt.Sprintf("slab%d", depth))
	directStruct := f.ElemKind == KindStruct && isGenerated(f.ElemType)

	elementLoop := func() []ast.Stmt {
		loop := []ast.Stmt{}
		if isArray {
			loop = append(loop, ifStmt(
				binop(ivar, token.GEQ, intLit(arrayN)),
				retResultIErrExpr(arrayLenErrExpr(f.JSONName, arrayN, ivar)),
			))
		}
		if f.ElemPointer {
			ptrNullBody := []ast.Stmt{}
			if isArray {
				ptrNullBody = append(ptrNullBody, assign(index(d(), ivar), id("nil")), inc(ivar))
			} else {
				ptrNullBody = append(ptrNullBody, assign(d(), call(id("append"), d(), id("nil"))))
			}
			ptrNullBody = append(ptrNullBody, streamCommaOrBreak(posVar)...)
			loop = append(loop, streamReadMore(posVar), streamNullPeek(posVar, ptrNullBody, nil))
		}
		var target ast.Expr
		switch {
		case isArray && f.ElemPointer:
			target = index(slabVar, ivar)
		case isArray:
			target = index(d(), ivar)
		case f.ElemPointer:
			loop = append(loop, assign(slabVar, call(id("append"), slabVar, zeroLitExpr(f.ElemType, f.ElemKind))))
			target = index(slabVar, binop(idCall("len", slabVar), token.SUB, intLit(1)))
		default:
			loop = append(loop, assign(d(), call(id("append"), d(), zeroLitExpr(f.ElemType, f.ElemKind))))
			target = index(d(), binop(idCall("len", d()), token.SUB, intLit(1)))
		}
		switch f.ElemKind {
		case KindString:
			loop = append(loop, streamScan([]ast.Expr{target}, "String", posVar, pv)...)
		case KindBool:
			loop = append(loop, streamScan([]ast.Expr{target}, "Bool", posVar, pv)...)
		case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
			if f.ElemType == "int64" {
				loop = append(loop, streamScan([]ast.Expr{target}, "Int64", posVar, pv)...)
			} else {
				loop = append(loop, varDecl("iv", "int64"))
				loop = append(loop, streamScan([]ast.Expr{id("iv")}, "Int64", posVar, pv)...)
				loop = append(loop, assign(target, call(id(f.ElemType), id("iv"))))
			}
		case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
			if f.ElemType == "uint64" {
				loop = append(loop, streamScan([]ast.Expr{target}, "Uint64", posVar, pv)...)
			} else {
				loop = append(loop, varDecl("uv", "uint64"))
				loop = append(loop, streamScan([]ast.Expr{id("uv")}, "Uint64", posVar, pv)...)
				loop = append(loop, assign(target, call(id(f.ElemType), id("uv"))))
			}
		case KindFloat32, KindFloat64:
			if f.ElemKind == KindFloat64 {
				loop = append(loop, streamScan([]ast.Expr{target}, "Float64", posVar, pv)...)
			} else {
				loop = append(loop, varDecl("fv", "float64"))
				loop = append(loop, streamScan([]ast.Expr{id("fv")}, "Float64", posVar, pv)...)
				loop = append(loop, assign(target, call(id("float32"), id("fv"))))
			}
		case KindStruct:
			if directStruct {
				// target.DecodeStreamFrom(s, posVar) — receiver is the slot
				loop = append(loop,
					assignN(
						[]ast.Expr{target, pv, id("err")},
						call(sel(target, "DecodeStreamFrom"), id("s"), pv),
					),
					ifErrReturn(),
				)
			}
		case KindSlice, KindArray:
			loop = append(loop, emitStreamSliceReadStmts(peelSliceField(f), exprText(target), posVar, depth+1)...)
		}
		if len(f.ElemMods) > 0 {
			loop = append(loop, renderModsStmts(f.ElemMods, target, f.ElemType, f.ElemKind)...)
		}
		if len(f.ElemValidation) > 0 {
			loop = append(loop, renderValidationOnStmts(f.ElemValidation, target, f.JSONName+"[]", f.ElemKind, f.MultiErr, "i")...)
		}
		switch {
		case isArray && f.ElemPointer:
			loop = append(loop, assign(index(d(), ivar), addr(index(slabVar, ivar))), inc(ivar))
		case isArray:
			loop = append(loop, inc(ivar))
		case f.ElemPointer:
			loop = append(loop, assign(d(),
				call(id("append"), d(), addr(index(slabVar, binop(idCall("len", slabVar), token.SUB, intLit(1))))),
			))
		}
		loop = append(loop, streamCommaOrBreak(posVar)...)
		return loop
	}

	forLoop := forCond(
		binop(streamBytesAt(pv), token.NEQ, charLit(']')),
		elementLoop()...,
	)

	if isArray {
		body := []ast.Stmt{}
		body = append(body, assignN([]ast.Expr{pv, id("err")}, streamCall("ArrayOpen", pv)))
		body = append(body, ifErrReturn())
		body = append(body, streamSkipSpace(posVar)...)
		body = append(body, streamReadMore(posVar))
		body = append(body, varDecl(ivar.Name, "int"))
		if f.ElemPointer {
			body = append(body, &ast.AssignStmt{
				Lhs: []ast.Expr{slabVar}, Tok: token.DEFINE,
				Rhs: []ast.Expr{call(id("make"), &ast.ArrayType{Elt: parseExpr(f.ElemType)}, intLit(arrayN))},
			})
		}
		body = append(body, forLoop)
		body = append(body,
			ifStmt(binop(streamBytesAt(pv), token.NEQ, charLit(']')),
				retResultIErrExpr(idSel("scan", "ErrBadArray"))),
			ifStmt(binop(ivar, token.NEQ, intLit(arrayN)),
				retResultIErrExpr(arrayLenErrExpr(f.JSONName, arrayN, ivar))),
			inc(pv),
		)
		return []ast.Stmt{block(body...)}
	}

	// slice path: null peek + non-null content
	sCap, slCap := preallocCap(f)
	nonNull := []ast.Stmt{}
	nonNull = append(nonNull, assignN([]ast.Expr{pv, id("err")}, streamCall("ArrayOpen", pv)))
	nonNull = append(nonNull, ifErrReturn())
	nonNull = append(nonNull, streamSkipSpace(posVar)...)
	nonNull = append(nonNull, streamReadMore(posVar))
	if f.ElemPointer {
		nonNull = append(nonNull, varDeclExpr(slabVar.Name, &ast.ArrayType{Elt: parseExpr(f.ElemType)}))
	}
	emptyStmts := []ast.Stmt{assign(d(), &ast.CompositeLit{Type: parseExpr(f.GoType)})}
	var nonEmptyStmts []ast.Stmt
	if sCap > 0 {
		nonEmptyStmts = []ast.Stmt{assign(d(), call(id("make"), parseExpr(f.GoType), intLit(0), intLit(sCap)))}
	} else {
		nonEmptyStmts = []ast.Stmt{assign(d(), &ast.CompositeLit{Type: parseExpr(f.GoType)})}
	}
	if f.ElemPointer {
		nonEmptyStmts = append(nonEmptyStmts, assign(slabVar,
			call(id("make"), &ast.ArrayType{Elt: parseExpr(f.ElemType)}, intLit(0), intLit(slCap)),
		))
	}
	nonNull = append(nonNull, ifElse(
		binop(streamBytesAt(pv), token.EQL, charLit(']')),
		emptyStmts, nonEmptyStmts,
	))
	nonNull = append(nonNull, forLoop)
	nonNull = append(nonNull, ifStmt(
		binop(streamBytesAt(pv), token.NEQ, charLit(']')),
		retResultIErrExpr(idSel("scan", "ErrBadArray")),
	), inc(pv))

	body := streamSkipSpace(posVar)
	body = append(body, streamReadMore(posVar))
	body = append(body, streamNullPeek(posVar, nil, nonNull))
	return []ast.Stmt{block(body...)}
}

// renderStreamSliceStmts is the depth-0 wrapper.
func renderStreamSliceStmts(f FieldInfo, ref, posVar string) []ast.Stmt {
	return emitStreamSliceReadStmts(f, ref, posVar, 0)
}

// renderStreamFieldStmts is the stream-path equivalent of renderFieldStmts.
// Returns the decode body for a single field as []ast.Stmt.
func renderStreamFieldStmts(f FieldInfo, ref, posVar string) []ast.Stmt {
	pv := id(posVar)
	r := func() ast.Expr { return parseExpr(ref) }

	if f.String {
		out := renderStreamStringTagStmts(f, ref, posVar)
		if !f.NoValidate {
			out = append(out, validateAndModStmts(f, r())...)
		}
		return out
	}
	if f.Pointer {
		inner := f
		inner.Pointer = false
		if inner.PointeeType != "" {
			inner.GoType = inner.PointeeType
		}
		builtinV, customV := partitionCustomValidation(f.Validation)
		builtinM, customM := partitionCustomMods(f.Mods)
		inner.Validation = builtinV
		inner.Mods = builtinM
		innerStmts := renderStreamFieldStmts(inner, "v", posVar)

		nullBranch := []ast.Stmt{
			assign(id(posVar), binop(intLit(4), token.ADD, id(posVar))),
			assign(r(), id("nil")),
		}
		elseBody := []ast.Stmt{varDecl("v", inner.GoType)}
		elseBody = append(elseBody, innerStmts...)
		elseBody = append(elseBody, assign(r(), addr(id("v"))))

		out := []ast.Stmt{streamReadMore(posVar), streamNullPeek(posVar, nullBranch, elseBody)}
		if !f.NoValidate && (len(customV) > 0 || len(customM) > 0) {
			outer := f
			outer.Validation = customV
			outer.Mods = customM
			out = append(out, validateAndModStmts(outer, r())...)
		}
		return out
	}

	var out []ast.Stmt
	primScan := func(method string) {
		out = streamScan([]ast.Expr{r()}, method, posVar, pv)
	}
	widenedScan := func(wideType, wideVar, method, castTo string) {
		out = []ast.Stmt{varDecl(wideVar, wideType)}
		out = append(out, streamScan([]ast.Expr{id(wideVar)}, method, posVar, pv)...)
		out = append(out, assign(r(), call(id(castTo), id(wideVar))))
	}

	switch f.Kind {
	case KindString:
		primScan("String")
	case KindBool:
		primScan("Bool")
	case KindInt, KindInt8, KindInt16, KindInt32:
		widenedScan("int64", "iv", "Int64", f.GoType)
	case KindInt64:
		primScan("Int64")
	case KindUint, KindUint8, KindUint16, KindUint32:
		widenedScan("uint64", "uv", "Uint64", f.GoType)
	case KindUint64:
		primScan("Uint64")
	case KindFloat32:
		widenedScan("float64", "fv", "Float64", "float32")
	case KindFloat64:
		primScan("Float64")
	case KindStruct:
		if isGenerated(f.GoType) {
			// ref.DecodeStreamFrom(s, posVar) — receiver is the field.
			out = []ast.Stmt{
				assignN(
					[]ast.Expr{r(), pv, id("err")},
					call(sel(r(), "DecodeStreamFrom"), id("s"), pv),
				),
				ifErrReturn(),
			}
		} else {
			out = renderCrossPkgStructStreamDecodeStmts(f, ref, posVar)
		}
	case KindSlice:
		out = renderStreamSliceStmts(f, ref, posVar)
	case KindArray:
		out = emitStreamSliceReadStmts(f, ref, posVar, 0)
	case KindMap:
		out = renderStreamMapStmts(f, ref, posVar)
	case KindBytes:
		out = renderStreamBytesStmts(f, ref, posVar)
	case KindTime:
		out = renderStreamTimeStmts(f, ref, posVar)
	case KindDuration:
		out = renderStreamDurationStmts(f, ref, posVar)
	case KindNetIP:
		out = renderStreamNetIPStmts(ref, posVar)
	case KindNetipAddr:
		out = renderStreamNetipAddrStmts(ref, posVar)
	case KindNetipPrefix:
		out = renderStreamNetipPrefixStmts(ref, posVar)
	case KindRawJSON:
		out = renderStreamRawJSONStmts(ref, posVar)
	case KindURL:
		out = renderStreamURLStmts(ref, posVar)
	case KindBigInt:
		out = renderStreamBigIntStmts(ref, posVar)
	case KindBigFloat:
		out = renderStreamBigFloatStmts(ref, posVar)
	case KindBigRat:
		out = renderStreamBigRatStmts(ref, posVar)
	case KindSQLNull:
		out = renderStreamSQLNullStmts(f, ref, posVar)
	case KindAny:
		out = renderStreamAnyStmts(f, ref, posVar)
	default:
		out = []ast.Stmt{
			shortDeclN([]ast.Expr{id("k"), id("err")}, streamCall("SkipValue", pv)),
			ifErrReturn(),
			assign(pv, id("k")),
		}
	}

	if !f.NoValidate {
		out = append(out, validateAndModStmts(f, r())...)
	}
	return out
}

// renderStreamDispatchStmts emits the length-first key-dispatch switch for
// the stream-path DecodeStreamFrom.
func renderStreamDispatchStmts(s StructInfo) []ast.Stmt {
	// Group fields by JSON-name length.
	byLen := map[int][]FieldInfo{}
	var lens []int
	for _, f := range s.Fields {
		if f.Inline {
			continue
		}
		n := len(f.JSONName)
		if _, ok := byLen[n]; !ok {
			lens = append(lens, n)
		}
		byLen[n] = append(byLen[n], f)
	}
	// Sort lens ascending — small ints.
	for i := 1; i < len(lens); i++ {
		for j := i; j > 0 && lens[j-1] > lens[j]; j-- {
			lens[j-1], lens[j] = lens[j], lens[j-1]
		}
	}

	emitField := func(f FieldInfo) []ast.Stmt {
		ref := "result." + f.GoName
		// stream key dispatch: ConsumeColon, then field decode.
		body := []ast.Stmt{}
		body = append(body, assignN([]ast.Expr{id("i"), id("err")}, streamCall("ConsumeColon", id("i"))))
		body = append(body, ifErrReturn())
		if !needsSeen(f) {
			body = append(body, renderStreamFieldStmts(f, ref, "i")...)
			return body
		}
		set := seenSetStmts(s, f)
		seen := seenAccessExpr(s, f)
		decodeBody := renderStreamFieldStmts(f, ref, "i")
		if s.AllowDups {
			skipBody := []ast.Stmt{
				assignN([]ast.Expr{id("i"), id("err")}, streamCall("SkipValue", id("i"))),
				ifErrReturn(),
			}
			elseBody := append(append([]ast.Stmt{}, set...), decodeBody...)
			body = append(body, ifElse(seen, skipBody, elseBody))
			return body
		}
		if s.MultiErr {
			skipBody := []ast.Stmt{
				assign(id("errs"), call(id("append"), id("errs"),
					validationErrLit("DuplicateKeyError", fieldKV("Field", strLit(f.JSONName))),
				)),
				assignN([]ast.Expr{id("i"), id("err")}, streamCall("SkipValue", id("i"))),
				ifErrReturn(),
			}
			elseBody := append(append([]ast.Stmt{}, set...), decodeBody...)
			body = append(body, ifElse(seen, skipBody, elseBody))
			return body
		}
		body = append(body, ifStmt(seen,
			retResultIErrExpr(validationErrLit("DuplicateKeyError", fieldKV("Field", strLit(f.JSONName)))),
		))
		body = append(body, set...)
		body = append(body, decodeBody...)
		return body
	}

	var cases []ast.Stmt
	for _, n := range lens {
		fs := byLen[n]
		var caseBody []ast.Stmt
		if len(fs) == 1 {
			f := fs[0]
			caseBody = []ast.Stmt{ifElse(
				binop(id("key"), token.EQL, strLit(f.JSONName)),
				emitField(f),
				streamUnknownKeyStmts(s, "i"),
			)}
		} else {
			innerCases := []ast.Stmt{}
			for _, f := range fs {
				innerCases = append(innerCases, &ast.CaseClause{
					List: []ast.Expr{strLit(f.JSONName)},
					Body: emitField(f),
				})
			}
			innerCases = append(innerCases, &ast.CaseClause{
				List: nil,
				Body: streamUnknownKeyStmts(s, "i"),
			})
			caseBody = []ast.Stmt{&ast.SwitchStmt{
				Tag:  id("key"),
				Body: blockOf(innerCases),
			}}
		}
		cases = append(cases, &ast.CaseClause{
			List: []ast.Expr{intLit(n)},
			Body: caseBody,
		})
	}
	cases = append(cases, &ast.CaseClause{
		List: nil,
		Body: streamUnknownKeyStmts(s, "i"),
	})
	return []ast.Stmt{&ast.SwitchStmt{
		Tag:  idCall("len", id("key")),
		Body: blockOf(cases),
	}}
}

// renderStreamDecodeStructStmts produces the function body for DecodeStreamFrom
// on a non-alias struct.
func renderStreamDecodeStructStmts(s StructInfo) []ast.Stmt {
	body := []ast.Stmt{varDecl("result", s.Name)}
	if s.MultiErr {
		body = append(body, varDeclExpr("errs", idSel("validation", "Errors")))
	}
	body = append(body, seenInitStmts(s)...)
	// i, err := s.ObjectOpen(i); if err... ; i, err = s.SkipSpace(i); if err...
	body = append(body, shortDeclN([]ast.Expr{id("i"), id("err")}, streamCall("ObjectOpen", id("i"))))
	body = append(body, ifErrReturn())
	body = append(body, assignN([]ast.Expr{id("i"), id("err")}, streamCall("SkipSpace", id("i"))))
	body = append(body, ifErrReturn())
	body = append(body, streamReadMore("i"))

	// Empty-object fast path.
	emptyBranch := renderPostLoopStmts(s)
	emptyBranch = append(emptyBranch, retStmt(id("result"), binop(id("i"), token.ADD, intLit(1)), id("nil")))
	body = append(body, ifStmt(
		binop(streamBytesAt(id("i")), token.EQL, charLit('}')),
		emptyBranch...,
	))

	// Main loop
	loopBody := []ast.Stmt{varDecl("key", "string")}
	loopBody = append(loopBody, assignN(
		[]ast.Expr{id("key"), id("i"), id("err")},
		streamCall("KeyView", id("i")),
	))
	loopBody = append(loopBody, ifErrReturn())
	loopBody = append(loopBody, renderStreamDispatchStmts(s)...)
	loopBody = append(loopBody, streamSkipSpace("i")...)
	loopBody = append(loopBody, streamReadMore("i"))
	// c := s.Bytes()[i]
	loopBody = append(loopBody, shortDecl(id("c"), streamBytesAt(id("i"))))
	// if c == ',' { i, err = SkipSpace(i+1); if err...; continue }
	commaArm := streamSkipSpaceFrom("i", binop(id("i"), token.ADD, intLit(1)))
	commaArm = append(commaArm, &ast.BranchStmt{Tok: token.CONTINUE})
	loopBody = append(loopBody, ifStmt(
		binop(id("c"), token.EQL, charLit(',')),
		commaArm...,
	))
	// if c == '}' { postLoop; return result, i+1, nil }
	closeBranch := renderPostLoopStmts(s)
	closeBranch = append(closeBranch, retStmt(id("result"), binop(id("i"), token.ADD, intLit(1)), id("nil")))
	loopBody = append(loopBody, ifStmt(
		binop(id("c"), token.EQL, charLit('}')),
		closeBranch...,
	))
	// fallback
	loopBody = append(loopBody, retResultIErrExpr(idSel("scan", "ErrBadObject")))

	body = append(body, forInfinite(loopBody...))
	return body
}

// --- Cross-pkg renderers (AST) ----------------------------------------

// renderCrossPkgStructDecodeStmts emits the byte-path decode body for a
// cross-package struct field. Mirrors renderCrossPkgStructDecode without
// going through bridgeStmts.
func renderCrossPkgStructDecodeStmts(f FieldInfo, ref, posVar string) []ast.Stmt {
	pv := id(posVar)
	r := func() ast.Expr { return parseExpr(ref) }
	if f.Iface.Resolved {
		switch {
		case f.Iface.ByteDecoder:
			return []ast.Stmt{block(
				assignN([]ast.Expr{r(), pv, id("err")},
					call(sel(r(), "DecodeFrom"), id("data"), pv)),
				ifErrReturn(),
			)}
		case f.Iface.JSONUnmarshaler:
			return []ast.Stmt{block(
				shortDecl(id("start"), pv),
				assignN([]ast.Expr{pv, id("err")},
					call(idSel("scan", "SkipValue"), id("data"), id("start"))),
				ifErrReturn(),
				&ast.IfStmt{
					Init: assign(id("err"),
						call(sel(r(), "UnmarshalJSON"),
							slice2(id("data"), id("start"), pv))),
					Cond: binop(id("err"), token.NEQ, id("nil")),
					Body: block(retResultIErr()),
				},
			)}
		case f.Iface.TextUnmarshaler:
			return []ast.Stmt{block(
				varDecl("ts", "string"),
				assignN([]ast.Expr{id("ts"), pv, id("err")},
					call(idSel("scan", "String"), id("data"), pv)),
				ifErrReturn(),
				&ast.IfStmt{
					Init: assign(id("err"),
						call(sel(r(), "UnmarshalText"),
							call(idSel("unsafe", "Slice"),
								call(idSel("unsafe", "StringData"), id("ts")),
								idCall("len", id("ts")),
							),
						)),
					Cond: binop(id("err"), token.NEQ, id("nil")),
					Body: block(retResultIErr()),
				},
			)}
		default:
			// json.Unmarshal fallback.
			return []ast.Stmt{block(
				shortDecl(id("start"), pv),
				assignN([]ast.Expr{pv, id("err")},
					call(idSel("scan", "SkipValue"), id("data"), id("start"))),
				ifErrReturn(),
				&ast.IfStmt{
					Init: assign(id("err"),
						call(idSel("json", "Unmarshal"),
							slice2(id("data"), id("start"), pv),
							addr(r()),
						)),
					Cond: binop(id("err"), token.NEQ, id("nil")),
					Body: block(retResultIErr()),
				},
			)}
		}
	}
	// AST-only mode: runtime-probe cascade via encoding/json.
	return []ast.Stmt{block(
		shortDecl(id("start"), pv),
		assignN([]ast.Expr{pv, id("err")},
			call(idSel("scan", "SkipValue"), id("data"), id("start"))),
		ifErrReturn(),
		&ast.IfStmt{
			Init: assign(id("err"),
				call(idSel("json", "Unmarshal"),
					slice2(id("data"), id("start"), pv),
					addr(r()),
				)),
			Cond: binop(id("err"), token.NEQ, id("nil")),
			Body: block(retResultIErr()),
		},
	)}
}

// renderCrossPkgStructAppendStmts emits the marshal body for a cross-pkg
// struct field.
func renderCrossPkgStructAppendStmts(f FieldInfo, ref string) []ast.Stmt {
	r := func() ast.Expr { return parseExpr(ref) }
	if f.Iface.Resolved {
		switch {
		case f.Iface.AppendJSON:
			return []ast.Stmt{
				&ast.IfStmt{
					Init: &ast.AssignStmt{
						Lhs: []ast.Expr{id("dst"), id("err")},
						Tok: token.ASSIGN,
						Rhs: []ast.Expr{call(sel(r(), "AppendJSON"), id("dst"))},
					},
					Cond: binop(id("err"), token.NEQ, id("nil")),
					Body: block(retDstErr()),
				},
			}
		case f.Iface.JSONMarshaler:
			return []ast.Stmt{block(
				shortDeclN([]ast.Expr{id("bs"), id("err")}, call(sel(r(), "MarshalJSON"))),
				ifStmt(binop(id("err"), token.NEQ, id("nil")), retDstErr()),
				dstAppend(&ast.BasicLit{Kind: token.STRING, Value: "bs..."}),
			)}
		case f.Iface.TextAppender:
			return []ast.Stmt{
				dstAppend(charLit('"')),
				&ast.IfStmt{
					Init: &ast.AssignStmt{
						Lhs: []ast.Expr{id("dst"), id("err")},
						Tok: token.ASSIGN,
						Rhs: []ast.Expr{call(sel(r(), "AppendText"), id("dst"))},
					},
					Cond: binop(id("err"), token.NEQ, id("nil")),
					Body: block(retDstErr()),
				},
				dstAppend(charLit('"')),
			}
		case f.Iface.TextMarshaler:
			return []ast.Stmt{block(
				varDecl("t", "[]byte"),
				assignN([]ast.Expr{id("t"), id("err")}, call(sel(r(), "MarshalText"))),
				ifStmt(binop(id("err"), token.NEQ, id("nil")), retDstErr()),
				dstAppend(charLit('"')),
				dstAssignCall(id(appendStrFn(f.HTMLEscape)),
					call(idSel("encode", "BytesToString"), id("t"))),
				dstAppend(charLit('"')),
			)}
		default:
			return crossPkgJSONMarshal(r())
		}
	}
	return crossPkgJSONMarshal(r())
}

func crossPkgJSONMarshal(ref ast.Expr) []ast.Stmt {
	return []ast.Stmt{block(
		shortDeclN([]ast.Expr{id("bs"), id("err")}, call(idSel("json", "Marshal"), ref)),
		ifStmt(binop(id("err"), token.NEQ, id("nil")), retDstErr()),
		dstAppend(&ast.BasicLit{Kind: token.STRING, Value: "bs..."}),
	)}
}

// renderCrossPkgStructStreamDecodeStmts emits the stream-path decode for
// a cross-package struct field.
func renderCrossPkgStructStreamDecodeStmts(f FieldInfo, ref, posVar string) []ast.Stmt {
	pv := id(posVar)
	r := func() ast.Expr { return parseExpr(ref) }
	if f.Iface.Resolved {
		switch {
		case f.Iface.StreamDecoder:
			return []ast.Stmt{block(
				assignN([]ast.Expr{r(), pv, id("err")},
					call(sel(r(), "DecodeStreamFrom"), id("s"), pv)),
				ifErrReturn(),
			)}
		case f.Iface.JSONUnmarshaler:
			return []ast.Stmt{block(
				shortDecl(id("start"), pv),
				shortDecl(id("prevPin"), idSel("s", "Shift")),
				assign(idSel("s", "Shift"), id("false")),
				assignN([]ast.Expr{pv, id("err")}, streamCall("SkipValue", id("start"))),
				assign(idSel("s", "Shift"), id("prevPin")),
				ifErrReturn(),
				&ast.IfStmt{
					Init: assign(id("err"),
						call(sel(r(), "UnmarshalJSON"),
							slice2(streamBytes(), id("start"), pv))),
					Cond: binop(id("err"), token.NEQ, id("nil")),
					Body: block(retResultIErr()),
				},
			)}
		case f.Iface.TextUnmarshaler:
			return []ast.Stmt{block(
				varDecl("ts", "string"),
				assignN([]ast.Expr{id("ts"), pv, id("err")}, streamCall("String", pv)),
				ifErrReturn(),
				&ast.IfStmt{
					Init: assign(id("err"),
						call(sel(r(), "UnmarshalText"),
							call(idSel("unsafe", "Slice"),
								call(idSel("unsafe", "StringData"), id("ts")),
								idCall("len", id("ts")),
							),
						)),
					Cond: binop(id("err"), token.NEQ, id("nil")),
					Body: block(retResultIErr()),
				},
			)}
		default:
			return crossPkgStreamJSONUnmarshal(r(), posVar)
		}
	}
	return crossPkgStreamJSONUnmarshal(r(), posVar)
}

func crossPkgStreamJSONUnmarshal(ref ast.Expr, posVar string) []ast.Stmt {
	pv := id(posVar)
	return []ast.Stmt{block(
		shortDecl(id("start"), pv),
		shortDecl(id("prevPin"), idSel("s", "Shift")),
		assign(idSel("s", "Shift"), id("false")),
		assignN([]ast.Expr{pv, id("err")}, streamCall("SkipValue", id("start"))),
		assign(idSel("s", "Shift"), id("prevPin")),
		ifErrReturn(),
		&ast.IfStmt{
			Init: assign(id("err"),
				call(idSel("json", "Unmarshal"),
					slice2(streamBytes(), id("start"), pv),
					addr(ref),
				)),
			Cond: binop(id("err"), token.NEQ, id("nil")),
			Body: block(retResultIErr()),
		},
	)}
}

// --- Alias renderers (AST) --------------------------------------------

// renderAliasDecodeStmts emits the byte-path DecodeFrom body for an
// alias type. Dispatches by AliasKind: struct → delegation ladder,
// container (slice/map/array/bytes) → reuse the field-level emitters,
// primitive → scan + cast back to alias type.
func renderAliasDecodeStmts(s StructInfo) []ast.Stmt {
	if s.AliasKind == KindStruct {
		return renderAliasStructDecodeStmts(s, false)
	}
	if s.AliasKind == KindSlice || s.AliasKind == KindMap || s.AliasKind == KindArray || s.AliasKind == KindBytes {
		return renderAliasContainerDecodeStmts(s, false)
	}
	return aliasPrimitiveDecodeStmts(s, false)
}

// renderAliasStreamDecodeStmts is the stream-path counterpart.
func renderAliasStreamDecodeStmts(s StructInfo) []ast.Stmt {
	if s.AliasKind == KindStruct {
		return renderAliasStructDecodeStmts(s, true)
	}
	if s.AliasKind == KindSlice || s.AliasKind == KindMap || s.AliasKind == KindArray || s.AliasKind == KindBytes {
		return renderAliasContainerDecodeStmts(s, true)
	}
	return aliasPrimitiveDecodeStmts(s, true)
}

// aliasPrimitiveDecodeStmts emits the primitive-alias DecodeFrom body:
// scan a primitive into `v`, cast back to alias type, return.
func aliasPrimitiveDecodeStmts(s StructInfo, stream bool) []ast.Stmt {
	body := []ast.Stmt{
		varDecl("result", s.Name),
		varDecl("err", "error"),
		assign(id("_"), id("err")),
	}
	var scanCall ast.Expr
	var tmpType string
	switch s.AliasKind {
	case KindString:
		tmpType = "string"
		scanCall = scanOrStream(stream, "String", []ast.Expr{id("data"), id("i")}, []ast.Expr{id("i")})
	case KindBool:
		tmpType = "bool"
		scanCall = scanOrStream(stream, "Bool", []ast.Expr{id("data"), id("i")}, []ast.Expr{id("i")})
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		tmpType = "int64"
		scanCall = scanOrStream(stream, "Int64", []ast.Expr{id("data"), id("i")}, []ast.Expr{id("i")})
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		tmpType = "uint64"
		scanCall = scanOrStream(stream, "Uint64", []ast.Expr{id("data"), id("i")}, []ast.Expr{id("i")})
	case KindFloat32, KindFloat64:
		tmpType = "float64"
		scanCall = scanOrStream(stream, "Float64", []ast.Expr{id("data"), id("i")}, []ast.Expr{id("i")})
	}
	body = append(body, varDecl("v", tmpType))
	body = append(body, assignN(
		[]ast.Expr{id("v"), id("i"), id("err")},
		scanCall,
	), ifErrReturn())
	body = append(body, assign(id("result"), call(id(s.Name), id("v"))))
	body = append(body, retStmt(id("result"), id("i"), id("nil")))
	return body
}

// scanOrStream picks `scan.Method(byteArgs...)` or `s.Method(streamArgs...)`.
func scanOrStream(stream bool, method string, byteArgs, streamArgs []ast.Expr) ast.Expr {
	if stream {
		return call(idSel("s", method), streamArgs...)
	}
	return call(idSel("scan", method), byteArgs...)
}

// renderAliasStructDecodeStmts emits DecodeFrom (or DecodeStreamFrom) for
// a struct alias.
func renderAliasStructDecodeStmts(s StructInfo, stream bool) []ast.Stmt {
	body := []ast.Stmt{varDecl("result", s.Name)}
	underT := parseExpr(s.AliasUnderlying)
	switch {
	case s.AliasIface.ByteDecoder && !stream:
		body = append(body,
			varDeclExpr("u", underT),
			shortDeclN([]ast.Expr{id("v"), id("k"), id("err")},
				call(sel(id("u"), "DecodeFrom"), id("data"), id("i"))),
			ifErrReturn(),
			assign(id("result"), call(id(s.Name), id("v"))),
			retStmt(id("result"), id("k"), id("nil")),
		)
	case s.AliasIface.StreamDecoder && stream:
		body = append(body,
			varDeclExpr("u", underT),
			shortDeclN([]ast.Expr{id("v"), id("k"), id("err")},
				call(sel(id("u"), "DecodeStreamFrom"), id("s"), id("i"))),
			ifErrReturn(),
			assign(id("result"), call(id(s.Name), id("v"))),
			retStmt(id("result"), id("k"), id("nil")),
		)
	case s.AliasIface.JSONUnmarshaler:
		if stream {
			body = append(body,
				shortDecl(id("start"), id("i")),
				shortDecl(id("prevPin"), idSel("s", "Shift")),
				assign(idSel("s", "Shift"), id("false")),
				shortDeclN([]ast.Expr{id("k"), id("err")}, streamCall("SkipValue", id("start"))),
				assign(idSel("s", "Shift"), id("prevPin")),
				ifErrReturn(),
				varDeclExpr("u", underT),
				&ast.IfStmt{
					Init: assign(id("err"),
						call(sel(id("u"), "UnmarshalJSON"),
							slice2(streamBytes(), id("start"), id("k")))),
					Cond: binop(id("err"), token.NEQ, id("nil")),
					Body: block(retResultIErr()),
				},
				assign(id("result"), call(id(s.Name), id("u"))),
				retStmt(id("result"), id("k"), id("nil")),
			)
		} else {
			body = append(body,
				shortDecl(id("start"), id("i")),
				shortDeclN([]ast.Expr{id("k"), id("err")},
					call(idSel("scan", "SkipValue"), id("data"), id("start"))),
				ifErrReturn(),
				varDeclExpr("u", underT),
				&ast.IfStmt{
					Init: assign(id("err"),
						call(sel(id("u"), "UnmarshalJSON"),
							slice2(id("data"), id("start"), id("k")))),
					Cond: binop(id("err"), token.NEQ, id("nil")),
					Body: block(retResultIErr()),
				},
				assign(id("result"), call(id(s.Name), id("u"))),
				retStmt(id("result"), id("k"), id("nil")),
			)
		}
	case s.AliasIface.TextUnmarshaler:
		var scanCall ast.Expr
		if stream {
			scanCall = call(idSel("s", "String"), id("i"))
		} else {
			scanCall = call(idSel("scan", "String"), id("data"), id("i"))
		}
		body = append(body,
			shortDeclN([]ast.Expr{id("ts"), id("tj"), id("err")}, scanCall),
			ifErrReturn(),
			varDeclExpr("u", underT),
			&ast.IfStmt{
				Init: assign(id("err"),
					call(sel(id("u"), "UnmarshalText"),
						call(idSel("unsafe", "Slice"),
							call(idSel("unsafe", "StringData"), id("ts")),
							idCall("len", id("ts")),
						),
					)),
				Cond: binop(id("err"), token.NEQ, id("nil")),
				Body: block(retResultIErr()),
			},
			assign(id("result"), call(id(s.Name), id("u"))),
			retStmt(id("result"), id("tj"), id("nil")),
		)
	default:
		// No delegation path — should be rejected upstream. Return zero.
		body = append(body, retStmt(id("result"), id("i"), id("nil")))
	}
	return body
}

// renderAliasContainerDecodeStmts dispatches a slice/map/array/bytes alias
// into the existing AST emitters with `result` as ref.
func renderAliasContainerDecodeStmts(s StructInfo, stream bool) []ast.Stmt {
	body := []ast.Stmt{
		varDecl("result", s.Name),
		varDecl("err", "error"),
		assign(id("_"), id("err")),
	}
	f := s.AliasField
	f.GoType = s.Name
	switch s.AliasKind {
	case KindSlice:
		if stream {
			body = append(body, renderStreamSliceStmts(f, "result", "i")...)
		} else {
			body = append(body, emitByteSliceReadStmts(f, parseExpr("result"), "i", 0)...)
		}
	case KindArray:
		if stream {
			body = append(body, emitStreamSliceReadStmts(f, "result", "i", 0)...)
		} else {
			body = append(body, emitByteSliceReadStmts(f, parseExpr("result"), "i", 0)...)
		}
	case KindMap:
		if stream {
			body = append(body, renderStreamMapStmts(f, "result", "i")...)
		} else {
			body = append(body, renderMapStmts(f, parseExpr("result"), "i")...)
		}
	case KindBytes:
		if stream {
			body = append(body, renderStreamBytesStmts(f, "result", "i")...)
		} else {
			body = append(body, renderBytesStmts(f, parseExpr("result"), "i")...)
		}
	}
	body = append(body, retStmt(id("result"), id("i"), id("nil")))
	return body
}

// renderAliasSizeStmts emits the JSONSize body for an alias.
func renderAliasSizeStmts(s StructInfo) []ast.Stmt {
	switch s.AliasKind {
	case KindString:
		// return len(string(s))*2 + 2
		return []ast.Stmt{retStmt(
			binop(
				binop(idCall("len", call(id("string"), id("s"))), token.MUL, intLit(2)),
				token.ADD, intLit(2),
			),
		)}
	case KindBool:
		return []ast.Stmt{retStmt(intLit(5))}
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		return []ast.Stmt{retStmt(intLit(20))}
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		return []ast.Stmt{retStmt(intLit(20))}
	case KindFloat32, KindFloat64:
		return []ast.Stmt{retStmt(intLit(24))}
	case KindBytes:
		// return len(s)*4/3 + 8
		return []ast.Stmt{retStmt(
			binop(
				binop(binop(idCall("len", id("s")), token.MUL, intLit(4)), token.QUO, intLit(3)),
				token.ADD, intLit(8),
			),
		)}
	case KindSlice, KindMap, KindArray:
		return []ast.Stmt{retStmt(intLit(1024))}
	case KindStruct:
		if s.AliasIface.JSONMarshaler || s.AliasIface.TextMarshaler {
			return []ast.Stmt{retStmt(intLit(0))}
		}
		return []ast.Stmt{retStmt(intLit(128))}
	default:
		return []ast.Stmt{retStmt(intLit(0))}
	}
}

// renderAliasAppendJSONStmts emits the AppendJSON body for an alias.
func renderAliasAppendJSONStmts(s StructInfo) []ast.Stmt {
	if s.AliasKind == KindStruct {
		return renderAliasStructAppendJSONStmts(s)
	}
	if s.AliasKind == KindSlice || s.AliasKind == KindMap || s.AliasKind == KindArray || s.AliasKind == KindBytes {
		return renderAliasContainerAppendJSONStmts(s)
	}
	switch s.AliasKind {
	case KindString:
		return []ast.Stmt{
			dstAppend(charLit('"')),
			dstAssignCall(id(appendStrFn(s.HTMLEscape)), call(id("string"), id("s"))),
			retStmt(id("dst"), id("nil")),
		}
	case KindBool:
		return []ast.Stmt{retStmt(call(idSel("strconv", "AppendBool"), id("dst"), call(id("bool"), id("s"))), id("nil"))}
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		return []ast.Stmt{retStmt(call(idSel("strconv", "AppendInt"), id("dst"), call(id("int64"), id("s")), intLit(10)), id("nil"))}
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		return []ast.Stmt{retStmt(call(idSel("strconv", "AppendUint"), id("dst"), call(id("uint64"), id("s")), intLit(10)), id("nil"))}
	case KindFloat32:
		return []ast.Stmt{retStmt(call(idSel("strconv", "AppendFloat"),
			id("dst"), call(id("float64"), id("s")),
			&ast.BasicLit{Kind: token.CHAR, Value: "'g'"}, intLit(-1), intLit(32)),
			id("nil"))}
	case KindFloat64:
		return []ast.Stmt{retStmt(call(idSel("strconv", "AppendFloat"),
			id("dst"), call(id("float64"), id("s")),
			&ast.BasicLit{Kind: token.CHAR, Value: "'g'"}, intLit(-1), intLit(64)),
			id("nil"))}
	}
	return nil
}

// renderAliasStructAppendJSONStmts emits the marshal body for a struct alias.
func renderAliasStructAppendJSONStmts(s StructInfo) []ast.Stmt {
	switch {
	case s.AliasIface.AppendJSON:
		return []ast.Stmt{
			shortDecl(id("u"), call(id(s.AliasUnderlying), id("s"))),
			retStmt(call(sel(id("u"), "AppendJSON"), id("dst"))),
		}
	case s.AliasIface.JSONMarshaler:
		return []ast.Stmt{retStmt(call(sel(call(id(s.AliasUnderlying), id("s")), "MarshalJSON")))}
	case s.AliasIface.TextAppender:
		return []ast.Stmt{
			shortDecl(id("u"), call(id(s.AliasUnderlying), id("s"))),
			dstAppend(charLit('"')),
			varDecl("err", "error"),
			&ast.IfStmt{
				Init: &ast.AssignStmt{
					Lhs: []ast.Expr{id("dst"), id("err")},
					Tok: token.ASSIGN,
					Rhs: []ast.Expr{call(sel(id("u"), "AppendText"), id("dst"))},
				},
				Cond: binop(id("err"), token.NEQ, id("nil")),
				Body: block(retDstErr()),
			},
			retStmt(call(id("append"), id("dst"), charLit('"')), id("nil")),
		}
	case s.AliasIface.TextMarshaler:
		return []ast.Stmt{
			shortDecl(id("u"), call(id(s.AliasUnderlying), id("s"))),
			shortDeclN([]ast.Expr{id("t"), id("err")}, call(sel(id("u"), "MarshalText"))),
			ifStmt(binop(id("err"), token.NEQ, id("nil")), retDstErr()),
			dstAppend(charLit('"')),
			dstAssignCall(id(appendStrFn(s.HTMLEscape)),
				call(idSel("encode", "BytesToString"), id("t"))),
			retStmt(id("dst"), id("nil")),
		}
	}
	return []ast.Stmt{retStmt(id("dst"), id("nil"))}
}

// renderAliasContainerAppendJSONStmts emits the marshal body for a
// container alias (slice/map/array/bytes).
func renderAliasContainerAppendJSONStmts(s StructInfo) []ast.Stmt {
	f := s.AliasField
	f.GoType = s.Name
	var body []ast.Stmt
	switch s.AliasKind {
	case KindSlice, KindArray:
		body = emitAppendSliceStmts(f, "s", 0)
	case KindMap:
		body = renderAppendMapStmts(f, "s")
	case KindBytes:
		body = renderAppendBytesStmts(f, "s")
	}
	body = append(body, retStmt(id("dst"), id("nil")))
	return body
}

// --- Buffer-bridge helpers (kept for text-side render funcs still
// emitting via fmt.Fprintf in generate.go; they wrap the AST builders
// above so the buffer-side wrappers only do format.Node) --------------
