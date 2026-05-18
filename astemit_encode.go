package main

// AST emitters for the encode/marshal path: JSONSize body, AppendJSON
// body, the per-field append helpers, slice/map emitters, and the
// leading-quote fold. Each renderer mirrors its text-emitting
// counterpart in generate.go one-to-one — the text emitters now wrap
// the Stmts versions via writeStmts.

import (
	"fmt"
	"go/ast"
	"go/token"
)

// sizeAdd builds `size += rhs`.
func sizeAdd(rhs ast.Expr) ast.Stmt {
	return &ast.AssignStmt{
		Lhs: []ast.Expr{id("size")},
		Tok: token.ADD_ASSIGN,
		Rhs: []ast.Expr{rhs},
	}
}

// sizeAddConst builds `size += <n>` for a literal int.
func sizeAddConst(n int) ast.Stmt {
	return sizeAdd(intLit(n))
}

// dstAppend builds `dst = append(dst, args...)`.
func dstAppend(args ...ast.Expr) ast.Stmt {
	return assign(id("dst"), call(id("append"), append([]ast.Expr{id("dst")}, args...)...))
}

// dstAppendBytes builds `dst = append(dst, "literal"...)`.
func dstAppendBytes(s string) ast.Stmt {
	return dstAppend(&ast.BasicLit{Kind: token.STRING, Value: fmt.Sprintf("%q", s) + "..."})
}

// dstReturnAppendBytes builds `return append(dst, "literal"...), nil`.
func dstReturnAppendBytes(s string) ast.Stmt {
	return retStmt(call(id("append"), id("dst"),
		&ast.BasicLit{Kind: token.STRING, Value: fmt.Sprintf("%q", s) + "..."}), id("nil"))
}

// dstAssignCall builds `dst = pkg.Fn(dst, args...)`.
func dstAssignCall(fn ast.Expr, args ...ast.Expr) ast.Stmt {
	return assign(id("dst"), call(fn, append([]ast.Expr{id("dst")}, args...)...))
}

// dstAssignCallReturnErr builds `if dst, err = pkg.Fn(dst, args...); err != nil { return dst, err }`.
func dstAssignCallReturnErr(fn ast.Expr, args ...ast.Expr) ast.Stmt {
	return &ast.IfStmt{
		Init: &ast.AssignStmt{
			Lhs: []ast.Expr{id("dst"), id("err")},
			Tok: token.ASSIGN,
			Rhs: []ast.Expr{call(fn, append([]ast.Expr{id("dst")}, args...)...)},
		},
		Cond: binop(id("err"), token.NEQ, id("nil")),
		Body: block(retStmt(id("dst"), id("err"))),
	}
}

// retDstErr builds `return dst, err`.
func retDstErr() ast.Stmt { return retStmt(id("dst"), id("err")) }

// sizeStrPad / sizeStrMult etc. are declared in generate.go.

// sizeContribStmts is the AST counterpart of sizeContrib. Returns the
// constant part (folded into the initial `size := N`) and the runtime
// statements that compute the variable contribution.
func sizeContribStmts(f FieldInfo, ref string) (int, []ast.Stmt) {
	refExpr := parseExpr(ref)
	if f.Pointer {
		inner := f
		inner.Pointer = false
		if inner.PointeeType != "" {
			inner.GoType = inner.PointeeType
		}
		innerN, innerCode := sizeContribStmts(inner, "(*"+ref+")")
		var elseBody []ast.Stmt
		if innerN > 0 {
			elseBody = append(elseBody, sizeAddConst(innerN))
		}
		elseBody = append(elseBody, innerCode...)
		// if ref == nil { size += 4 } else { ... }
		return 0, []ast.Stmt{ifElse(
			binop(refExpr, token.EQL, id("nil")),
			[]ast.Stmt{sizeAddConst(4)},
			elseBody,
		)}
	}
	switch f.Kind {
	case KindString:
		return sizeStrPad, []ast.Stmt{
			sizeAdd(binop(idCall("len", parseExpr(ref)), token.MUL, intLit(sizeStrMult))),
		}
	case KindBool:
		return sizeBool, nil
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		return sizeInt, nil
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		return sizeUint, nil
	case KindFloat32, KindFloat64:
		return sizeFloat, nil
	case KindStruct:
		if isGenerated(f.GoType) || f.Iface.JSONSize {
			return 0, []ast.Stmt{sizeAdd(call(sel(parseExpr(ref), "JSONSize")))}
		}
		return 128, nil
	case KindSlice, KindArray:
		return sizeSliceContribStmts(f, ref, 0)
	case KindMap:
		return sizeMapContribStmts(f, ref)
	case KindBytes:
		switch f.Format {
		case "array":
			return 2, []ast.Stmt{sizeAdd(binop(idCall("len", parseExpr(ref)), token.MUL, intLit(4)))}
		case "base16", "hex":
			return 2, []ast.Stmt{sizeAdd(binop(idCall("len", parseExpr(ref)), token.MUL, intLit(2)))}
		case "base32", "base32hex":
			// ((len(ref)+4)/5)*8
			return 2, []ast.Stmt{sizeAdd(binop(
				paren(binop(paren(binop(idCall("len", parseExpr(ref)), token.ADD, intLit(4))), token.QUO, intLit(5))),
				token.MUL, intLit(8),
			))}
		}
		// base64 / base64url: ((n+2)/3)*4
		return 2, []ast.Stmt{sizeAdd(binop(
			paren(binop(paren(binop(idCall("len", parseExpr(ref)), token.ADD, intLit(2))), token.QUO, intLit(3))),
			token.MUL, intLit(4),
		))}
	case KindTime:
		return timeFormatSize(f.Format), nil
	case KindDuration:
		return durationFormatSize(f.Format), nil
	case KindNetIP:
		// if ref.To4() != nil { size += 15 } else if len(ref) != 0 { size += 39 } else { size += 2 }
		return 2, []ast.Stmt{
			&ast.IfStmt{
				Cond: binop(call(sel(parseExpr(ref), "To4")), token.NEQ, id("nil")),
				Body: block(sizeAddConst(15)),
				Else: &ast.IfStmt{
					Cond: binop(idCall("len", parseExpr(ref)), token.NEQ, intLit(0)),
					Body: block(sizeAddConst(39)),
					Else: block(sizeAddConst(2)),
				},
			},
		}
	case KindNetipAddr:
		return 2, []ast.Stmt{
			ifElse(call(sel(parseExpr(ref), "Is4")),
				[]ast.Stmt{sizeAddConst(15)},
				[]ast.Stmt{sizeAddConst(39)}),
		}
	case KindNetipPrefix:
		return 2, []ast.Stmt{
			ifElse(call(sel(call(sel(parseExpr(ref), "Addr")), "Is4")),
				[]ast.Stmt{sizeAddConst(19)},
				[]ast.Stmt{sizeAddConst(43)}),
		}
	case KindRawJSON:
		// if n := len(ref); n > 0 { size += n } else { size += 4 }
		return 0, []ast.Stmt{
			&ast.IfStmt{
				Init: shortDecl(id("n"), idCall("len", parseExpr(ref))),
				Cond: binop(id("n"), token.GTR, intLit(0)),
				Body: block(sizeAdd(id("n"))),
				Else: block(sizeAddConst(4)),
			},
		}
	case KindURL:
		// size += len(ref.Scheme) + len(ref.Host)*3 + len(ref.Path)*3 + len(ref.RawQuery) + len(ref.Fragment)*3 + len(ref.Opaque)
		urlLen := func(field string) ast.Expr {
			return idCall("len", sel(parseExpr(ref), field))
		}
		urlLen3 := func(field string) ast.Expr {
			return binop(urlLen(field), token.MUL, intLit(3))
		}
		sum := binop(
			binop(
				binop(
					binop(
						binop(urlLen("Scheme"), token.ADD, urlLen3("Host")),
						token.ADD, urlLen3("Path"),
					),
					token.ADD, urlLen("RawQuery"),
				),
				token.ADD, urlLen3("Fragment"),
			),
			token.ADD, urlLen("Opaque"),
		)
		// if ref.User != nil { pw, _ := ref.User.Password(); size += (len(ref.User.Username())+len(pw))*3 + 2 }
		userBlock := []ast.Stmt{
			shortDeclN(
				[]ast.Expr{id("pw"), id("_")},
				call(sel(sel(parseExpr(ref), "User"), "Password")),
			),
			sizeAdd(binop(
				binop(
					paren(binop(idCall("len", call(sel(sel(parseExpr(ref), "User"), "Username"))), token.ADD, idCall("len", id("pw")))),
					token.MUL, intLit(3),
				),
				token.ADD, intLit(2),
			)),
		}
		return 8, []ast.Stmt{
			sizeAdd(sum),
			ifStmt(
				binop(sel(parseExpr(ref), "User"), token.NEQ, id("nil")),
				userBlock...,
			),
		}
	case KindBigInt:
		return 4, []ast.Stmt{sizeAdd(binop(call(sel(parseExpr(ref), "BitLen")), token.QUO, intLit(3)))}
	case KindBigFloat:
		return 66, nil
	case KindBigRat:
		// size += (ref.Num().BitLen() + ref.Denom().BitLen())/3
		return 8, []ast.Stmt{sizeAdd(binop(
			paren(binop(
				call(sel(call(sel(parseExpr(ref), "Num")), "BitLen")),
				token.ADD,
				call(sel(call(sel(parseExpr(ref), "Denom")), "BitLen")),
			)),
			token.QUO, intLit(3),
		))}
	case KindSQLNull:
		spec, ok := SQLNullSpec(f.GoType)
		if !ok {
			return 0, nil
		}
		innerField := FieldInfo{Kind: spec.Inner, GoType: spec.Type, Format: f.Format}
		return sizeContribStmts(innerField, ref+"."+spec.Field)
	case KindAny:
		return 256, nil
	}
	return 0, nil
}

// sizeSliceContribStmts emits the slice/array size contribution. Brackets
// count for the 2-byte constant; per-element work goes in the runtime.
func sizeSliceContribStmts(f FieldInfo, ref string, depth int) (int, []ast.Stmt) {
	ivar := fmt.Sprintf("i%d", depth)
	refExpr := parseExpr(ref)
	// if n := len(ref); n > 0 { size += n - 1 }
	out := []ast.Stmt{
		&ast.IfStmt{
			Init: shortDecl(id("n"), idCall("len", parseExpr(ref))),
			Cond: binop(id("n"), token.GTR, intLit(0)),
			Body: block(sizeAdd(binop(id("n"), token.SUB, intLit(1)))),
		},
	}
	switch f.ElemKind {
	case KindString:
		// for ivar := range ref { size += len(ref[ivar])*<mult> + <pad> }
		out = append(out, &ast.RangeStmt{
			Key: id(ivar),
			Tok: token.DEFINE,
			X:   parseExpr(ref),
			Body: block(sizeAdd(binop(
				binop(idCall("len", index(parseExpr(ref), id(ivar))), token.MUL, intLit(sizeStrMult)),
				token.ADD, intLit(sizeStrPad),
			))),
		})
	case KindBool:
		out = append(out, sizeAdd(binop(idCall("len", refExpr), token.MUL, intLit(sizeBool))))
	case KindInt, KindInt64, KindInt8, KindInt16, KindInt32:
		out = append(out, sizeAdd(binop(idCall("len", refExpr), token.MUL, intLit(sizeInt))))
	case KindUint, KindUint64, KindUint8, KindUint16, KindUint32:
		out = append(out, sizeAdd(binop(idCall("len", refExpr), token.MUL, intLit(sizeUint))))
	case KindFloat32, KindFloat64:
		out = append(out, sizeAdd(binop(idCall("len", refExpr), token.MUL, intLit(sizeFloat))))
	case KindStruct:
		if isGenerated(f.ElemType) || f.ElemIface.JSONSize {
			if f.ElemPointer {
				// for ivar := range ref { if ref[ivar] == nil { size += 4 } else { size += (*ref[ivar]).JSONSize() } }
				out = append(out, &ast.RangeStmt{
					Key: id(ivar),
					Tok: token.DEFINE,
					X:   parseExpr(ref),
					Body: block(ifElse(
						binop(index(parseExpr(ref), id(ivar)), token.EQL, id("nil")),
						[]ast.Stmt{sizeAddConst(4)},
						[]ast.Stmt{sizeAdd(call(sel(paren(star(index(parseExpr(ref), id(ivar)))), "JSONSize")))},
					)),
				})
			} else {
				out = append(out, &ast.RangeStmt{
					Key:  id(ivar),
					Tok:  token.DEFINE,
					X:    parseExpr(ref),
					Body: block(sizeAdd(call(sel(index(parseExpr(ref), id(ivar)), "JSONSize")))),
				})
			}
		} else {
			out = append(out, sizeAdd(binop(idCall("len", refExpr), token.MUL, intLit(128))))
		}
	case KindSlice, KindArray:
		innerN, innerCode := sizeSliceContribStmts(peelSliceField(f), fmt.Sprintf("%s[%s]", ref, ivar), depth+1)
		innerBody := []ast.Stmt{}
		if innerN > 0 {
			innerBody = append(innerBody, sizeAddConst(innerN))
		}
		innerBody = append(innerBody, innerCode...)
		out = append(out, &ast.RangeStmt{
			Key:  id(ivar),
			Tok:  token.DEFINE,
			X:    parseExpr(ref),
			Body: block(innerBody...),
		})
	}
	return 2, out
}

// sizeMapContribStmts emits the map size contribution.
func sizeMapContribStmts(f FieldInfo, ref string) (int, []ast.Stmt) {
	const perEntryFixed = 4
	var out []ast.Stmt

	if v, ok := constSizePerEntry(f.ElemKind, f.Format); ok {
		// size += len(ref) * (perEntryFixed + v)
		out = append(out, sizeAdd(binop(idCall("len", parseExpr(ref)), token.MUL, intLit(perEntryFixed+v))))
		// for k := range ref { size += len(k) * sizeStrMult }
		out = append(out, &ast.RangeStmt{
			Key:  id("k"),
			Tok:  token.DEFINE,
			X:    parseExpr(ref),
			Body: block(sizeAdd(binop(idCall("len", id("k")), token.MUL, intLit(sizeStrMult)))),
		})
		return 2, out
	}

	out = append(out, sizeAdd(binop(idCall("len", parseExpr(ref)), token.MUL, intLit(perEntryFixed))))
	loopBody := []ast.Stmt{
		sizeAdd(binop(idCall("len", id("k")), token.MUL, intLit(sizeStrMult))),
	}
	switch f.ElemKind {
	case KindString:
		loopBody = append(loopBody, sizeAdd(binop(
			binop(idCall("len", id("v")), token.MUL, intLit(sizeStrMult)),
			token.ADD, intLit(sizeStrPad),
		)))
	case KindStruct:
		if isGenerated(f.ElemType) || f.ElemIface.JSONSize {
			loopBody = append(loopBody, sizeAdd(call(sel(id("v"), "JSONSize"))))
		} else {
			loopBody = append(loopBody, sizeAddConst(128))
		}
	case KindBigInt:
		loopBody = append(loopBody, sizeAdd(binop(
			binop(call(sel(id("v"), "BitLen")), token.QUO, intLit(3)),
			token.ADD, intLit(4),
		)))
	case KindBigRat:
		loopBody = append(loopBody, sizeAdd(binop(
			binop(
				paren(binop(
					call(sel(call(sel(id("v"), "Num")), "BitLen")),
					token.ADD,
					call(sel(call(sel(id("v"), "Denom")), "BitLen")),
				)),
				token.QUO, intLit(3),
			),
			token.ADD, intLit(8),
		)))
	case KindNetIP:
		loopBody = append(loopBody, &ast.IfStmt{
			Cond: binop(call(sel(id("v"), "To4")), token.NEQ, id("nil")),
			Body: block(sizeAddConst(17)),
			Else: &ast.IfStmt{
				Cond: binop(idCall("len", id("v")), token.NEQ, intLit(0)),
				Body: block(sizeAddConst(41)),
				Else: block(sizeAddConst(4)),
			},
		})
	case KindNetipAddr:
		loopBody = append(loopBody, ifElse(call(sel(id("v"), "Is4")),
			[]ast.Stmt{sizeAddConst(17)},
			[]ast.Stmt{sizeAddConst(41)}))
	case KindNetipPrefix:
		loopBody = append(loopBody, ifElse(call(sel(call(sel(id("v"), "Addr")), "Is4")),
			[]ast.Stmt{sizeAddConst(21)},
			[]ast.Stmt{sizeAddConst(45)}))
	case KindURL:
		urlLen := func(field string) ast.Expr {
			return idCall("len", sel(id("v"), field))
		}
		urlLen3 := func(field string) ast.Expr {
			return binop(urlLen(field), token.MUL, intLit(3))
		}
		sum := binop(
			binop(
				binop(
					binop(
						binop(
							binop(urlLen("Scheme"), token.ADD, urlLen3("Host")),
							token.ADD, urlLen3("Path"),
						),
						token.ADD, urlLen("RawQuery"),
					),
					token.ADD, urlLen3("Fragment"),
				),
				token.ADD, urlLen("Opaque"),
			),
			token.ADD, intLit(8),
		)
		loopBody = append(loopBody, sizeAdd(sum))
		userBlock := []ast.Stmt{
			shortDeclN(
				[]ast.Expr{id("pw"), id("_")},
				call(sel(sel(id("v"), "User"), "Password")),
			),
			sizeAdd(binop(
				binop(
					paren(binop(
						idCall("len", call(sel(sel(id("v"), "User"), "Username"))),
						token.ADD, idCall("len", id("pw")),
					)),
					token.MUL, intLit(3),
				),
				token.ADD, intLit(2),
			)),
		}
		loopBody = append(loopBody, ifStmt(
			binop(sel(id("v"), "User"), token.NEQ, id("nil")),
			userBlock...,
		))
	default:
		loopBody = append(loopBody, sizeAddConst(128))
	}
	out = append(out, &ast.RangeStmt{
		Key:   id("k"),
		Value: id("v"),
		Tok:   token.DEFINE,
		X:     parseExpr(ref),
		Body:  blockOf(loopBody),
	})
	return 2, out
}

// renderSizeBodyStmts produces the full body of JSONSize for a non-alias
// struct, mirroring renderSize's text-emit logic.
func renderSizeBodyStmts(s StructInfo) []ast.Stmt {
	fixed := 2 // { and }
	if structHasAppendFormatTime(s) {
		fixed += 64
	}
	named := 0
	var runtime []ast.Stmt
	for _, f := range s.Fields {
		if f.Inline {
			ref := "s." + f.GoName
			_, code := sizeContribStmts(f, ref)
			runtime = append(runtime, code...)
			continue
		}
		ref := "s." + f.GoName
		emit := fieldSkipExpr(f, ref)
		sizeField, sizeRef := f, ref
		if emit != "" && f.Pointer {
			sizeField.Pointer = false
			if f.PointeeType != "" {
				sizeField.GoType = f.PointeeType
			}
			sizeRef = "(*" + ref + ")"
		}
		n, code := sizeContribStmts(sizeField, sizeRef)
		if emit == "" {
			fixed += len(f.JSONName) + 3
			if named > 0 {
				fixed++
			}
			named++
			fixed += n
			runtime = append(runtime, code...)
			continue
		}
		guardBody := []ast.Stmt{sizeAddConst(len(f.JSONName) + 4 + n)}
		guardBody = append(guardBody, code...)
		runtime = append(runtime, ifStmt(parseExpr(emit), guardBody...))
	}
	body := []ast.Stmt{shortDecl(id("size"), intLit(fixed))}
	body = append(body, runtime...)
	body = append(body, retStmt(id("size")))
	return body
}

// foldLeadingQuoteAST mirrors foldLeadingQuote in AST form. Returns the
// new constant prefix (with `"` folded in), the value-emit statements
// (without the opening quote), and ok=false when the field's value emit
// doesn't lead with a quote.
func foldLeadingQuoteAST(f FieldInfo, ref string) (newPrefix string, code []ast.Stmt, ok bool) {
	if f.Pointer {
		return "", nil, false
	}
	switch f.Kind {
	case KindString:
		return `"`, []ast.Stmt{dstAssignCall(id(appendStrFn(f.HTMLEscape)), parseExpr(ref))}, true
	case KindNetIP, KindNetipAddr, KindNetipPrefix:
		return `"`, []ast.Stmt{
			dstAssignCallReturnErr(sel(parseExpr(ref), "AppendText")),
			dstAppend(charLit('"')),
		}, true
	case KindBytes:
		var enc ast.Expr
		switch f.Format {
		case "", "base64":
			enc = sel(idSel("base64", "StdEncoding"), "AppendEncode")
		case "base64url":
			enc = sel(idSel("base64", "URLEncoding"), "AppendEncode")
		case "base32":
			enc = sel(idSel("base32", "StdEncoding"), "AppendEncode")
		case "base32hex":
			enc = sel(idSel("base32", "HexEncoding"), "AppendEncode")
		case "base16", "hex":
			enc = idSel("hex", "AppendEncode")
		default:
			return "", nil, false
		}
		return `"`, []ast.Stmt{
			dstAssignCall(enc, parseExpr(ref)),
			dstAppend(charLit('"')),
		}, true
	case KindURL:
		return `"`, []ast.Stmt{
			dstAssignCall(idSel("encode", "AppendURL"), parseExpr(ref)),
			dstAppend(charLit('"')),
		}, true
	case KindBigRat:
		return `"`, []ast.Stmt{
			dstAssignCallReturnErr(sel(paren(addr(parseExpr(ref))), "AppendText")),
			dstAppend(charLit('"')),
		}, true
	case KindBigFloat:
		return `"`, []ast.Stmt{
			dstAssignCall(sel(paren(addr(parseExpr(ref))), "Append"),
				&ast.BasicLit{Kind: token.CHAR, Value: "'g'"}, intLit(-1)),
			dstAppend(charLit('"')),
		}, true
	}
	return "", nil, false
}

// renderAppendBytesStmts mirrors renderAppendBytes.
func renderAppendBytesStmts(f FieldInfo, ref string) []ast.Stmt {
	var encExpr ast.Expr
	switch f.Format {
	case "", "base64":
		encExpr = sel(idSel("base64", "StdEncoding"), "AppendEncode")
	case "base64url":
		encExpr = sel(idSel("base64", "URLEncoding"), "AppendEncode")
	case "base32":
		encExpr = sel(idSel("base32", "StdEncoding"), "AppendEncode")
	case "base32hex":
		encExpr = sel(idSel("base32", "HexEncoding"), "AppendEncode")
	case "base16", "hex":
		encExpr = idSel("hex", "AppendEncode")
	case "array":
		// dst = append(dst, '['); for i, b := range ref { if i > 0 { dst = append(dst, ',') }; dst = strconv.AppendUint(dst, uint64(b), 10) }; dst = append(dst, ']')
		loopBody := []ast.Stmt{
			ifStmt(
				binop(id("i"), token.GTR, intLit(0)),
				dstAppend(charLit(',')),
			),
			dstAssignCall(idSel("strconv", "AppendUint"),
				call(id("uint64"), id("b")), intLit(10)),
		}
		return []ast.Stmt{
			dstAppend(charLit('[')),
			&ast.RangeStmt{
				Key: id("i"), Value: id("b"), Tok: token.DEFINE,
				X: parseExpr(ref), Body: blockOf(loopBody),
			},
			dstAppend(charLit(']')),
		}
	default:
		encExpr = sel(idSel("base64", "StdEncoding"), "AppendEncode")
	}
	return []ast.Stmt{
		dstAppend(charLit('"')),
		dstAssignCall(encExpr, parseExpr(ref)),
		dstAppend(charLit('"')),
	}
}

// renderAppendTimeStmts mirrors renderAppendTime.
func renderAppendTimeStmts(f FieldInfo, ref string) []ast.Stmt {
	layout, numeric := timeLayoutExpr(f.Format)
	if numeric == "Unix" {
		// dst = strconv.AppendFloat(dst, float64(ref.UnixNano())/1e9, 'f', -1, 64)
		return []ast.Stmt{
			dstAssignCall(idSel("strconv", "AppendFloat"),
				binop(
					call(id("float64"), call(sel(parseExpr(ref), "UnixNano"))),
					token.QUO, parseExpr("1e9"),
				),
				&ast.BasicLit{Kind: token.CHAR, Value: "'f'"},
				intLit(-1),
				intLit(64),
			),
		}
	}
	if numeric != "" {
		return []ast.Stmt{
			dstAssignCall(idSel("strconv", "AppendInt"),
				call(sel(parseExpr(ref), numeric)),
				intLit(10),
			),
		}
	}
	return []ast.Stmt{
		dstAppend(charLit('"')),
		dstAssignCall(sel(parseExpr(ref), "AppendFormat"), parseExpr(layout)),
		dstAppend(charLit('"')),
	}
}

// renderAppendDurationStmts mirrors renderAppendDuration.
func renderAppendDurationStmts(f FieldInfo, ref string) []ast.Stmt {
	switch f.Format {
	case "sec":
		return []ast.Stmt{
			dstAssignCall(idSel("strconv", "AppendFloat"),
				call(sel(parseExpr(ref), "Seconds")),
				&ast.BasicLit{Kind: token.CHAR, Value: "'g'"}, intLit(-1), intLit(64),
			),
		}
	case "milli":
		return []ast.Stmt{
			dstAssignCall(idSel("strconv", "AppendInt"),
				call(sel(parseExpr(ref), "Milliseconds")), intLit(10),
			),
		}
	case "micro":
		return []ast.Stmt{
			dstAssignCall(idSel("strconv", "AppendInt"),
				call(sel(parseExpr(ref), "Microseconds")), intLit(10),
			),
		}
	case "nano":
		return []ast.Stmt{
			dstAssignCall(idSel("strconv", "AppendInt"),
				call(sel(parseExpr(ref), "Nanoseconds")), intLit(10),
			),
		}
	case "units":
		return []ast.Stmt{
			dstAppend(charLit('"')),
			dstAssignCall(id(appendStrFn(f.HTMLEscape)),
				call(sel(parseExpr(ref), "String")),
			),
		}
	}
	return []ast.Stmt{
		dstAssignCall(id(appendStrFn(f.HTMLEscape)),
			call(sel(parseExpr(ref), "String")),
		),
	}
}

// emitAppendSliceStmts mirrors emitAppendSlice (recursive).
func emitAppendSliceStmts(f FieldInfo, ref string, depth int) []ast.Stmt {
	vvar := fmt.Sprintf("v%d", depth)
	body := []ast.Stmt{
		dstAppend(charLit('[')),
	}
	// if len(ref) > 0 { first; for _, vN := range ref[1:] { dst = append(dst, ','); element } }
	firstAndLoop := []ast.Stmt{}
	firstAndLoop = append(firstAndLoop, emitSliceElementStmts(f, fmt.Sprintf("%s[0]", ref), depth)...)
	loopBody := []ast.Stmt{dstAppend(charLit(','))}
	loopBody = append(loopBody, emitSliceElementStmts(f, vvar, depth)...)
	firstAndLoop = append(firstAndLoop, &ast.RangeStmt{
		Key:   id("_"),
		Value: id(vvar),
		Tok:   token.DEFINE,
		X:     slice2(parseExpr(ref), intLit(1), nil),
		Body:  blockOf(loopBody),
	})
	body = append(body, ifStmt(
		binop(idCall("len", parseExpr(ref)), token.GTR, intLit(0)),
		firstAndLoop...,
	))
	body = append(body, dstAppend(charLit(']')))

	if f.Kind == KindSlice {
		// nil-slice → "null"
		return []ast.Stmt{ifElse(
			binop(parseExpr(ref), token.EQL, id("nil")),
			[]ast.Stmt{dstAppendBytes("null")},
			body,
		)}
	}
	return body
}

// emitSliceElementStmts mirrors emitSliceElement.
func emitSliceElementStmts(f FieldInfo, vref string, depth int) []ast.Stmt {
	if f.ElemPointer {
		nf := f
		nf.ElemPointer = false
		return []ast.Stmt{ifElse(
			binop(parseExpr(vref), token.EQL, id("nil")),
			[]ast.Stmt{dstAppendBytes("null")},
			emitSliceElementStmts(nf, "(*"+vref+")", depth),
		)}
	}
	switch f.ElemKind {
	case KindString:
		return []ast.Stmt{
			dstAppend(charLit('"')),
			dstAssignCall(id(appendStrFn(f.HTMLEscape)), parseExpr(vref)),
		}
	case KindBool:
		return []ast.Stmt{dstAssignCall(idSel("strconv", "AppendBool"), parseExpr(vref))}
	case KindInt, KindInt8, KindInt16, KindInt32:
		return []ast.Stmt{dstAssignCall(idSel("strconv", "AppendInt"),
			call(id("int64"), parseExpr(vref)), intLit(10),
		)}
	case KindInt64:
		return []ast.Stmt{dstAssignCall(idSel("strconv", "AppendInt"), parseExpr(vref), intLit(10))}
	case KindUint, KindUint8, KindUint16, KindUint32:
		return []ast.Stmt{dstAssignCall(idSel("strconv", "AppendUint"),
			call(id("uint64"), parseExpr(vref)), intLit(10),
		)}
	case KindUint64:
		return []ast.Stmt{dstAssignCall(idSel("strconv", "AppendUint"), parseExpr(vref), intLit(10))}
	case KindFloat32:
		return []ast.Stmt{dstAssignCall(idSel("strconv", "AppendFloat"),
			call(id("float64"), parseExpr(vref)),
			&ast.BasicLit{Kind: token.CHAR, Value: "'g'"}, intLit(-1), intLit(32),
		)}
	case KindFloat64:
		return []ast.Stmt{dstAssignCall(idSel("strconv", "AppendFloat"),
			parseExpr(vref),
			&ast.BasicLit{Kind: token.CHAR, Value: "'g'"}, intLit(-1), intLit(64),
		)}
	case KindStruct:
		if isGenerated(f.ElemType) {
			return []ast.Stmt{dstAssignCallReturnErr(sel(parseExpr(vref), "AppendJSON"))}
		}
		// Cross-package fallback via json.Marshal.
		return []ast.Stmt{block(
			shortDeclN(
				[]ast.Expr{id("bs"), id("err")},
				call(idSel("json", "Marshal"), parseExpr(vref)),
			),
			ifStmt(binop(id("err"), token.NEQ, id("nil")), retDstErr()),
			dstAppend(&ast.BasicLit{Kind: token.STRING, Value: "bs..."}),
		)}
	case KindSlice, KindArray:
		return emitAppendSliceStmts(peelSliceField(f), vref, depth+1)
	}
	return nil
}

// renderAppendMapStmts mirrors renderAppendMap.
func renderAppendMapStmts(f FieldInfo, ref string) []ast.Stmt {
	appendStr := appendStrFn(f.HTMLEscape)
	// inner element emit on `v`
	var valueEmit []ast.Stmt
	switch f.ElemKind {
	case KindString:
		valueEmit = []ast.Stmt{
			dstAppend(charLit('"')),
			dstAssignCall(id(appendStr), id("v")),
		}
	case KindBool:
		valueEmit = []ast.Stmt{dstAssignCall(idSel("strconv", "AppendBool"), id("v"))}
	case KindInt:
		valueEmit = []ast.Stmt{dstAssignCall(idSel("strconv", "AppendInt"), call(id("int64"), id("v")), intLit(10))}
	case KindInt64:
		valueEmit = []ast.Stmt{dstAssignCall(idSel("strconv", "AppendInt"), id("v"), intLit(10))}
	case KindUint64:
		valueEmit = []ast.Stmt{dstAssignCall(idSel("strconv", "AppendUint"), id("v"), intLit(10))}
	case KindFloat64:
		valueEmit = []ast.Stmt{dstAssignCall(idSel("strconv", "AppendFloat"),
			id("v"), &ast.BasicLit{Kind: token.CHAR, Value: "'g'"}, intLit(-1), intLit(64),
		)}
	case KindStruct:
		if isGenerated(f.ElemType) {
			valueEmit = []ast.Stmt{dstAssignCallReturnErr(sel(id("v"), "AppendJSON"))}
		} else {
			valueEmit = []ast.Stmt{block(
				shortDeclN(
					[]ast.Expr{id("bs"), id("err")},
					call(idSel("json", "Marshal"), id("v")),
				),
				ifStmt(binop(id("err"), token.NEQ, id("nil")), retDstErr()),
				dstAppend(&ast.BasicLit{Kind: token.STRING, Value: "bs..."}),
			)}
		}
	case KindAny:
		valueEmit = []ast.Stmt{dstAssignCallReturnErr(idSel("encode", "AppendAny"), id("v"))}
	}

	// for k, v := range ref { first/comma branch; dst = appendStr(dst, k); ':' ; valueEmit }
	loopBody := []ast.Stmt{
		ifElse(
			id("first"),
			[]ast.Stmt{assign(id("first"), id("false")), dstAppend(charLit('"'))},
			[]ast.Stmt{dstAppendBytes(`,"`)},
		),
		dstAssignCall(id(appendStr), id("k")),
		dstAppend(charLit(':')),
	}
	loopBody = append(loopBody, valueEmit...)

	nonNilBody := []ast.Stmt{
		dstAppend(charLit('{')),
		block(
			shortDecl(id("first"), id("true")),
			&ast.RangeStmt{
				Key:   id("k"),
				Value: id("v"),
				Tok:   token.DEFINE,
				X:     parseExpr(ref),
				Body:  blockOf(loopBody),
			},
		),
		dstAppend(charLit('}')),
	}

	return []ast.Stmt{ifElse(
		binop(parseExpr(ref), token.EQL, id("nil")),
		[]ast.Stmt{dstAppendBytes("null")},
		nonNilBody,
	)}
}

// renderAppendValueStmts mirrors renderAppendValue.
func renderAppendValueStmts(f FieldInfo, ref string) []ast.Stmt {
	if f.Pointer {
		inner := f
		inner.Pointer = false
		if inner.PointeeType != "" {
			inner.GoType = inner.PointeeType
		}
		innerRef := "(*" + ref + ")"
		return []ast.Stmt{ifElse(
			binop(parseExpr(ref), token.EQL, id("nil")),
			[]ast.Stmt{dstAppendBytes("null")},
			renderAppendValueStmts(inner, innerRef),
		)}
	}
	if f.String {
		switch f.Kind {
		case KindBool:
			return []ast.Stmt{ifElse(
				parseExpr(ref),
				[]ast.Stmt{dstAppendBytes(`"true"`)},
				[]ast.Stmt{dstAppendBytes(`"false"`)},
			)}
		case KindInt:
			return []ast.Stmt{
				dstAppend(charLit('"')),
				dstAssignCall(idSel("strconv", "AppendInt"), call(id("int64"), parseExpr(ref)), intLit(10)),
				dstAppend(charLit('"')),
			}
		case KindInt64:
			return []ast.Stmt{
				dstAppend(charLit('"')),
				dstAssignCall(idSel("strconv", "AppendInt"), parseExpr(ref), intLit(10)),
				dstAppend(charLit('"')),
			}
		case KindUint64:
			return []ast.Stmt{
				dstAppend(charLit('"')),
				dstAssignCall(idSel("strconv", "AppendUint"), parseExpr(ref), intLit(10)),
				dstAppend(charLit('"')),
			}
		case KindFloat64:
			return []ast.Stmt{
				dstAppend(charLit('"')),
				dstAssignCall(idSel("strconv", "AppendFloat"), parseExpr(ref),
					&ast.BasicLit{Kind: token.CHAR, Value: "'g'"}, intLit(-1), intLit(64)),
				dstAppend(charLit('"')),
			}
		}
	}
	switch f.Kind {
	case KindString:
		return []ast.Stmt{
			dstAppend(charLit('"')),
			dstAssignCall(id(appendStrFn(f.HTMLEscape)), parseExpr(ref)),
		}
	case KindBool:
		return []ast.Stmt{dstAssignCall(idSel("strconv", "AppendBool"), parseExpr(ref))}
	case KindInt, KindInt8, KindInt16, KindInt32:
		return []ast.Stmt{dstAssignCall(idSel("strconv", "AppendInt"), call(id("int64"), parseExpr(ref)), intLit(10))}
	case KindInt64:
		return []ast.Stmt{dstAssignCall(idSel("strconv", "AppendInt"), parseExpr(ref), intLit(10))}
	case KindUint, KindUint8, KindUint16, KindUint32:
		return []ast.Stmt{dstAssignCall(idSel("strconv", "AppendUint"), call(id("uint64"), parseExpr(ref)), intLit(10))}
	case KindUint64:
		return []ast.Stmt{dstAssignCall(idSel("strconv", "AppendUint"), parseExpr(ref), intLit(10))}
	case KindFloat32:
		return []ast.Stmt{dstAssignCall(idSel("strconv", "AppendFloat"),
			call(id("float64"), parseExpr(ref)),
			&ast.BasicLit{Kind: token.CHAR, Value: "'g'"}, intLit(-1), intLit(64),
		)}
	case KindFloat64:
		return []ast.Stmt{dstAssignCall(idSel("strconv", "AppendFloat"),
			parseExpr(ref),
			&ast.BasicLit{Kind: token.CHAR, Value: "'g'"}, intLit(-1), intLit(64),
		)}
	case KindStruct:
		if isGenerated(f.GoType) {
			return []ast.Stmt{dstAssignCallReturnErr(sel(parseExpr(ref), "AppendJSON"))}
		}
		return renderCrossPkgStructAppendStmts(f, ref)
	case KindSlice, KindArray:
		return emitAppendSliceStmts(f, ref, 0)
	case KindMap:
		return renderAppendMapStmts(f, ref)
	case KindBytes:
		return renderAppendBytesStmts(f, ref)
	case KindTime:
		return renderAppendTimeStmts(f, ref)
	case KindDuration:
		return renderAppendDurationStmts(f, ref)
	case KindNetIP, KindNetipAddr, KindNetipPrefix:
		return []ast.Stmt{
			dstAppend(charLit('"')),
			dstAssignCallReturnErr(sel(parseExpr(ref), "AppendText")),
			dstAppend(charLit('"')),
		}
	case KindRawJSON:
		return []ast.Stmt{ifElse(
			binop(idCall("len", parseExpr(ref)), token.EQL, intLit(0)),
			[]ast.Stmt{dstAppendBytes("null")},
			[]ast.Stmt{dstAppend(&ast.BasicLit{Kind: token.STRING, Value: ref + "..."})},
		)}
	case KindURL:
		return []ast.Stmt{
			dstAppend(charLit('"')),
			dstAssignCall(idSel("encode", "AppendURL"), parseExpr(ref)),
			dstAppend(charLit('"')),
		}
	case KindBigInt:
		return []ast.Stmt{dstAssignCall(sel(paren(addr(parseExpr(ref))), "Append"), intLit(10))}
	case KindBigFloat:
		return []ast.Stmt{
			dstAppend(charLit('"')),
			dstAssignCall(sel(paren(addr(parseExpr(ref))), "Append"),
				&ast.BasicLit{Kind: token.CHAR, Value: "'g'"}, intLit(-1),
			),
			dstAppend(charLit('"')),
		}
	case KindBigRat:
		return []ast.Stmt{
			dstAppend(charLit('"')),
			dstAssignCallReturnErr(sel(paren(addr(parseExpr(ref))), "AppendText")),
			dstAppend(charLit('"')),
		}
	case KindSQLNull:
		spec, ok := SQLNullSpec(f.GoType)
		if !ok {
			return nil
		}
		innerField := FieldInfo{Kind: spec.Inner, GoType: spec.Type, Format: f.Format}
		return []ast.Stmt{ifElse(
			not(sel(parseExpr(ref), "Valid")),
			[]ast.Stmt{dstAppendBytes("null")},
			renderAppendValueStmts(innerField, ref+"."+spec.Field),
		)}
	case KindAny:
		return []ast.Stmt{dstAssignCallReturnErr(idSel("encode", "AppendAny"), parseExpr(ref))}
	}
	return nil
}

// renderAppendJSONBodyStmts mirrors renderAppendJSONBody.
func renderAppendJSONBodyStmts(s StructInfo) []ast.Stmt {
	if len(s.Fields) == 0 {
		return []ast.Stmt{retStmt(
			call(id("append"), id("dst"), charLit('{'), charLit('}')),
			id("nil"),
		)}
	}
	body := []ast.Stmt{
		varDecl("err", "error"),
		assign(id("_"), id("err")),
	}
	anyConditional := false
	for _, f := range s.Fields {
		if fieldIsConditional(f) {
			anyConditional = true
			break
		}
	}
	if !anyConditional {
		for i, f := range s.Fields {
			prefix := `,"` + f.JSONName + `":`
			if i == 0 {
				prefix = `{"` + f.JSONName + `":`
			}
			ref := "s." + f.GoName
			if extra, code, ok := foldLeadingQuoteAST(f, ref); ok {
				body = append(body, dstAppendBytes(prefix+extra))
				body = append(body, code...)
			} else {
				body = append(body, dstAppendBytes(prefix))
				body = append(body, renderAppendValueStmts(f, ref)...)
			}
		}
		body = append(body, dstReturnAppendBytes("}"))
		return body
	}

	body = append(body,
		dstAppend(charLit('{')),
		shortDecl(id("start"), idCall("len", id("dst"))),
	)
	for _, f := range s.Fields {
		ref := "s." + f.GoName
		if f.Inline {
			var valEmit ast.Stmt
			if f.ElemType == "jsontext.Value" {
				valEmit = dstAppend(&ast.BasicLit{Kind: token.STRING, Value: "v..."})
			} else {
				valEmit = dstAssignCallReturnErr(idSel("encode", "AppendAny"), id("v"))
			}
			loopBody := []ast.Stmt{
				ifStmt(
					binop(idCall("len", id("dst")), token.GTR, id("start")),
					dstAppend(charLit(',')),
				),
				dstAppend(charLit('"')),
				dstAssignCall(id(appendStrFn(f.HTMLEscape)), id("k")),
				dstAppend(charLit(':')),
				valEmit,
			}
			body = append(body, block(
				&ast.RangeStmt{
					Key:   id("k"),
					Value: id("v"),
					Tok:   token.DEFINE,
					X:     parseExpr(ref),
					Body:  blockOf(loopBody),
				},
			))
			continue
		}
		emit := fieldSkipExpr(f, ref)
		var pieces []ast.Stmt
		pieces = append(pieces,
			ifStmt(
				binop(idCall("len", id("dst")), token.GTR, id("start")),
				dstAppend(charLit(',')),
			),
		)
		prefix := `"` + f.JSONName + `":`
		if extra, code, ok := foldLeadingQuoteAST(f, ref); ok {
			pieces = append(pieces, dstAppendBytes(prefix+extra))
			pieces = append(pieces, code...)
		} else {
			pieces = append(pieces, dstAppendBytes(prefix))
			pieces = append(pieces, renderAppendValueStmts(f, ref)...)
		}
		if emit != "" {
			body = append(body, ifStmt(parseExpr(emit), pieces...))
		} else {
			body = append(body, pieces...)
		}
	}
	body = append(body, dstReturnAppendBytes("}"))
	return body
}
