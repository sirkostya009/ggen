package main

// AST builders for the leaf-level renderers. Each `renderXxxStmts`
// returns a self-contained `[]ast.Stmt` that the corresponding text-
// shaped `renderXxx` wraps via `writeStmts`. Once the parent renderers
// switch to consuming `[]ast.Stmt` directly, the text wrappers go away
// and the format.Node bridge in writeStmts collapses too.

import (
	"go/ast"
	"go/token"
)

// inlineSkipWSStmts emits the JSON whitespace-skip loop on posVar:
//
//	for posVar < len(data) && (data[posVar]==' '||'\t'||'\n'||'\r') { posVar++ }
func inlineSkipWSStmts(posVar string) []ast.Stmt {
	pv := id(posVar)
	dataAt := index(id("data"), pv)
	wsCond := paren(lor(
		binop(dataAt, token.EQL, charLit(' ')),
		binop(dataAt, token.EQL, charLit('\t')),
		binop(dataAt, token.EQL, charLit('\n')),
		binop(dataAt, token.EQL, charLit('\r')),
	))
	return []ast.Stmt{
		forCond(
			land(binop(pv, token.LSS, idCall("len", id("data"))), wsCond),
			inc(pv),
		),
	}
}

// inlineNullPeekStmts emits the 4-byte `null` literal check on posVar.
// On match, posVar is advanced by 4 inside the if body; nullBody and
// elseBody are spliced after the advance / into the else respectively.
// elseBody==nil produces a plain `if`; non-nil produces `if {} else {}`.
func inlineNullPeekStmts(posVar string, nullBody, elseBody []ast.Stmt) ast.Stmt {
	pv := id(posVar)
	cond := land(
		binop(binop(pv, token.ADD, intLit(4)), token.LEQ, idCall("len", id("data"))),
		binop(index(id("data"), pv), token.EQL, charLit('n')),
		binop(index(id("data"), binop(pv, token.ADD, intLit(1))), token.EQL, charLit('u')),
		binop(index(id("data"), binop(pv, token.ADD, intLit(2))), token.EQL, charLit('l')),
		binop(index(id("data"), binop(pv, token.ADD, intLit(3))), token.EQL, charLit('l')),
	)
	body := append([]ast.Stmt{incBy(posVar, 4)}, nullBody...)
	if elseBody == nil {
		return ifStmt(cond, body...)
	}
	return ifElse(cond, body, elseBody)
}

// inlineScanStringStmts emits the zero-copy string reader. Hot path
// aliases input via unsafe.String; escape sequences fall back to
// scan.String. Caller passes `dst` as an expression (struct field,
// slice slot, etc.) and the source / destination position vars by name.
func inlineScanStringStmts(posIn string, dst ast.Expr, posOut string) []ast.Stmt {
	posInId := id(posIn)
	posOutId := id(posOut)
	quote := charLit('"')
	backslash := charLit('\\')
	dataAtIn := index(id("data"), posInId)

	// outer guard: posIn >= len(data) || data[posIn] != '"'
	guard := lor(
		binop(posInId, token.GEQ, idCall("len", id("data"))),
		binop(dataAtIn, token.NEQ, quote),
	)

	// inner block:
	// ke := posIn + 1
	keInit := shortDecl(id("ke"), binop(posInId, token.ADD, intLit(1)))
	dataAtKe := index(id("data"), id("ke"))
	// for ke < len(data) && data[ke] != '"' && data[ke] != '\\' { ke++ }
	loop := forCond(
		land(
			binop(id("ke"), token.LSS, idCall("len", id("data"))),
			binop(dataAtKe, token.NEQ, quote),
			binop(dataAtKe, token.NEQ, backslash),
		),
		inc(id("ke")),
	)
	// if ke >= len(data) { return result, i, scan.ErrUnterminated }
	unterm := ifStmt(
		binop(id("ke"), token.GEQ, idCall("len", id("data"))),
		retResultIErrExpr(idSel("scan", "ErrUnterminated")),
	)
	// fast: dst = unsafe.String(unsafe.SliceData(data[posIn+1:]), ke-posIn-1)
	//       posOut = ke + 1
	fastAssign := assign(dst, unsafeStringAlias(
		binop(posInId, token.ADD, intLit(1)),
		binop(binop(id("ke"), token.SUB, posInId), token.SUB, intLit(1)),
	))
	fastAdvance := assign(posOutId, binop(id("ke"), token.ADD, intLit(1)))
	// slow: dst, posOut, err = scan.String(data, posIn); if err != nil { return ... }
	slowCall := assignN(
		[]ast.Expr{dst, posOutId, id("err")},
		call(idSel("scan", "String"), id("data"), posInId),
	)
	branch := ifElse(
		binop(dataAtKe, token.EQL, quote),
		[]ast.Stmt{fastAssign, fastAdvance},
		[]ast.Stmt{slowCall, ifErrReturn()},
	)

	return []ast.Stmt{
		ifStmt(guard, retResultIErrExpr(idSel("scan", "ErrExpectString"))),
		block(keInit, loop, unterm, branch),
	}
}

// scanThenAssign builds the common scaffolding for leaf renderers that
// read a JSON string into a temporary `s`, then perform a single Parse
// or Set call:
//
//	{
//	    var s string
//	    <inline string scan into s>
//	    <stmts>
//	}
//
// Callers append their type-specific stmts after the string scan.
func scanThenAssign(posVar string, stmts ...ast.Stmt) []ast.Stmt {
	body := []ast.Stmt{varDecl("s", "string")}
	body = append(body, inlineScanStringStmts(posVar, id("s"), posVar)...)
	body = append(body, stmts...)
	return []ast.Stmt{block(body...)}
}

// renderRawJSONStmts captures the raw bytes of one JSON value into ref.
// Used for json.RawMessage / jsontext.Value fields — zero-copy alias.
func renderRawJSONStmts(ref ast.Expr, posVar string) []ast.Stmt {
	return []ast.Stmt{block(
		shortDecl(id("start"), id(posVar)),
		assignN(
			[]ast.Expr{id(posVar), id("err")},
			call(idSel("scan", "SkipValue"), id("data"), id("start")),
		),
		ifErrReturn(),
		assign(ref, slice2(id("data"), id("start"), id(posVar))),
	)}
}

// renderURLStmts parses a JSON string via url.Parse and stores the
// dereferenced value into ref.
func renderURLStmts(ref ast.Expr, posVar string) []ast.Stmt {
	return scanThenAssign(posVar,
		varDeclExpr("u", star(idSel("url", "URL"))),
		assignN(
			[]ast.Expr{id("u"), id("err")},
			call(idSel("url", "Parse"), id("s")),
		),
		ifErrReturn(),
		assign(ref, star(id("u"))),
	)
}

// renderBigIntStmts reads a bare JSON number and feeds the aliased
// literal to (*big.Int).SetString. The alias dies before SetString
// returns — SetString copies digits into its own backing storage.
func renderBigIntStmts(ref ast.Expr, posVar string) []ast.Stmt {
	setString := call(
		sel(paren(addr(ref)), "SetString"),
		unsafeStringAlias(id("start"), binop(id(posVar), token.SUB, id("start"))),
		intLit(10),
	)
	return []ast.Stmt{block(
		shortDecl(id("start"), id(posVar)),
		assignN(
			[]ast.Expr{id(posVar), id("err")},
			call(idSel("scan", "SkipValue"), id("data"), id("start")),
		),
		ifErrReturn(),
		&ast.IfStmt{
			Init: shortDeclN([]ast.Expr{id("_"), id("ok")}, setString),
			Cond: not(id("ok")),
			Body: block(retResultIErrExpr(idSel("scan", "ErrBadNumber"))),
		},
	)}
}

// renderBigFloatStmts reads a JSON-string-wrapped numeric literal into
// big.Float. Wrapping matches jsonv2 wire format.
func renderBigFloatStmts(ref ast.Expr, posVar string) []ast.Stmt {
	parse := call(sel(paren(addr(ref)), "Parse"), id("s"), intLit(10))
	return scanThenAssign(posVar,
		&ast.IfStmt{
			Init: shortDeclN([]ast.Expr{id("_"), id("_"), id("err")}, parse),
			Cond: binop(id("err"), token.NEQ, id("nil")),
			Body: block(retResultIErr()),
		},
	)
}

// renderBigRatStmts reads "num" or "num/denom" as a JSON string and
// feeds it to (*big.Rat).SetString — lossless fractional rep.
func renderBigRatStmts(ref ast.Expr, posVar string) []ast.Stmt {
	setString := call(sel(paren(addr(ref)), "SetString"), id("s"))
	return scanThenAssign(posVar,
		&ast.IfStmt{
			Init: shortDeclN([]ast.Expr{id("_"), id("ok")}, setString),
			Cond: not(id("ok")),
			Body: block(retResultIErrExpr(idSel("scan", "ErrBadNumber"))),
		},
	)
}

// renderNetIPStmts parses a JSON string via net.ParseIP. Failure is
// signaled by nil (no error from ParseIP), so the typed sentinel below
// is what surfaces — historical behaviour preserved.
func renderNetIPStmts(ref ast.Expr, posVar string) []ast.Stmt {
	return scanThenAssign(posVar,
		assign(ref, call(idSel("net", "ParseIP"), id("s"))),
		ifStmt(
			binop(ref, token.EQL, id("nil")),
			retResultIErrExpr(call(idSel("fmt", "Errorf"), strLit("invalid IP"))),
		),
	)
}

// renderNetipAddrStmts parses a JSON string via netip.ParseAddr.
func renderNetipAddrStmts(ref ast.Expr, posVar string) []ast.Stmt {
	return scanThenAssign(posVar,
		assignN([]ast.Expr{ref, id("err")}, call(idSel("netip", "ParseAddr"), id("s"))),
		ifErrReturn(),
	)
}

// renderNetipPrefixStmts parses a JSON string via netip.ParsePrefix.
func renderNetipPrefixStmts(ref ast.Expr, posVar string) []ast.Stmt {
	return scanThenAssign(posVar,
		assignN([]ast.Expr{ref, id("err")}, call(idSel("netip", "ParsePrefix"), id("s"))),
		ifErrReturn(),
	)
}

// inlineScanInt64Stmts emits an inline signed-integer scanner. Reads a
// JSON int into a local `n int64`, optionally casts via castFn, and
// stores into dst. Skip-cast path (castFn=="") writes n straight to dst.
// Trailing `.` / `e` / `E` rejects the value as a float in disguise.
func inlineScanInt64Stmts(posVar string, dst ast.Expr, castFn string) []ast.Stmt {
	pv := id(posVar)
	dataAt := index(id("data"), pv)
	digit := func(op token.Token, lit byte) ast.Expr {
		return binop(dataAt, op, charLit(lit))
	}

	var finalAssign ast.Stmt
	if castFn != "" {
		finalAssign = assign(dst, call(id(castFn), id("n")))
	} else {
		finalAssign = assign(dst, id("n"))
	}

	return []ast.Stmt{block(
		// neg := false
		shortDecl(id("neg"), id("false")),
		// if pv < len(data) && data[pv] == '-' { neg = true; pv++ }
		ifStmt(
			land(binop(pv, token.LSS, idCall("len", id("data"))), digit(token.EQL, '-')),
			assign(id("neg"), id("true")),
			inc(pv),
		),
		// if pv >= len(data) || data[pv] < '0' || data[pv] > '9' { return ... }
		ifStmt(
			lor(
				binop(pv, token.GEQ, idCall("len", id("data"))),
				digit(token.LSS, '0'),
				digit(token.GTR, '9'),
			),
			retResultIErrExpr(idSel("scan", "ErrBadNumber")),
		),
		// var n int64
		varDecl("n", "int64"),
		// for pv < len(data) && data[pv] >= '0' && data[pv] <= '9' { n = n*10 + int64(data[pv]-'0'); pv++ }
		forCond(
			land(
				binop(pv, token.LSS, idCall("len", id("data"))),
				digit(token.GEQ, '0'),
				digit(token.LEQ, '9'),
			),
			assign(id("n"), binop(
				binop(id("n"), token.MUL, intLit(10)),
				token.ADD,
				call(id("int64"), binop(dataAt, token.SUB, charLit('0'))),
			)),
			inc(pv),
		),
		// if pv < len(data) { c := data[pv]; if c=='.'||'e'||'E' { return ... } }
		ifStmt(
			binop(pv, token.LSS, idCall("len", id("data"))),
			shortDecl(id("c"), dataAt),
			ifStmt(
				lor(
					binop(id("c"), token.EQL, charLit('.')),
					binop(id("c"), token.EQL, charLit('e')),
					binop(id("c"), token.EQL, charLit('E')),
				),
				retResultIErrExpr(idSel("scan", "ErrBadNumber")),
			),
		),
		// if neg { n = -n }
		ifStmt(id("neg"), assign(id("n"), &ast.UnaryExpr{Op: token.SUB, X: id("n")})),
		finalAssign,
	)}
}

// renderTimeStmts emits decode for a time.Time field. Three shapes: float
// (unix-fractional), int (unix-integer at milli/micro/nano), or string
// (Go time.Parse with a layout).
func renderTimeStmts(f FieldInfo, ref ast.Expr, posVar string) []ast.Stmt {
	layout, numeric := timeLayoutExpr(f.Format)
	pv := id(posVar)
	if numeric == "Unix" {
		return []ast.Stmt{block(
			varDecl("f", "float64"),
			assignN(
				[]ast.Expr{id("f"), pv, id("err")},
				call(idSel("scan", "Float64"), id("data"), pv),
			),
			ifErrReturn(),
			shortDecl(id("sec"), call(id("int64"), id("f"))),
			shortDecl(id("nsec"), call(id("int64"),
				binop(
					paren(binop(id("f"), token.SUB, call(id("float64"), id("sec")))),
					token.MUL,
					parseExpr("1e9"),
				),
			)),
			assign(ref, call(idSel("time", "Unix"), id("sec"), id("nsec"))),
		)}
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
		return []ast.Stmt{block(
			varDecl("n", "int64"),
			assignN(
				[]ast.Expr{id("n"), pv, id("err")},
				call(idSel("scan", "Int64"), id("data"), pv),
			),
			ifErrReturn(),
			assign(ref, ctor),
		)}
	}
	return scanThenAssign(posVar,
		assignN(
			[]ast.Expr{ref, id("err")},
			call(idSel("time", "Parse"), parseExpr(layout), id("s")),
		),
		ifErrReturn(),
	)
}

// renderDurationStmts emits decode for a time.Duration field. Four shapes:
// float seconds, int-units (milli/micro/nano), or units-string parse.
func renderDurationStmts(f FieldInfo, ref ast.Expr, posVar string) []ast.Stmt {
	pv := id(posVar)
	switch f.Format {
	case "sec":
		return []ast.Stmt{block(
			varDecl("v", "float64"),
			assignN(
				[]ast.Expr{id("v"), pv, id("err")},
				call(idSel("scan", "Float64"), id("data"), pv),
			),
			ifErrReturn(),
			assign(ref, call(idSel("time", "Duration"),
				binop(id("v"), token.MUL, call(id("float64"), idSel("time", "Second"))),
			)),
		)}
	case "milli", "micro", "nano":
		var unit ast.Expr
		switch f.Format {
		case "milli":
			unit = idSel("time", "Millisecond")
		case "micro":
			unit = idSel("time", "Microsecond")
		case "nano":
			unit = idSel("time", "Nanosecond")
		}
		return []ast.Stmt{block(
			varDecl("n", "int64"),
			assignN(
				[]ast.Expr{id("n"), pv, id("err")},
				call(idSel("scan", "Int64"), id("data"), pv),
			),
			ifErrReturn(),
			assign(ref, binop(call(idSel("time", "Duration"), id("n")), token.MUL, unit)),
		)}
	}
	return scanThenAssign(posVar,
		assignN(
			[]ast.Expr{ref, id("err")},
			call(idSel("time", "ParseDuration"), id("s")),
		),
		ifErrReturn(),
	)
}

// renderBytesStmts emits decode for a []byte field. Format selector picks
// base64 (default), base64url, base32, base32hex, hex, or "array" (JSON
// array of byte-sized ints).
func renderBytesStmts(f FieldInfo, ref ast.Expr, posVar string) []ast.Stmt {
	pv := id(posVar)
	dataAtPv := index(id("data"), pv)

	if f.Format == "array" {
		// JSON array of ints — read open bracket, loop over uint64
		// values appending byte(v), then closing bracket.
		body := []ast.Stmt{}
		body = append(body, inlineSkipWSStmts(posVar)...)
		body = append(body,
			ifStmt(
				lor(
					binop(pv, token.GEQ, idCall("len", id("data"))),
					binop(dataAtPv, token.NEQ, charLit('[')),
				),
				retResultIErrExpr(idSel("scan", "ErrBadArray")),
			),
			inc(pv),
		)
		body = append(body, inlineSkipWSStmts(posVar)...)

		// inner-loop body
		loopBody := []ast.Stmt{
			assignN(
				[]ast.Expr{id("v"), pv, id("err")},
				call(idSel("scan", "Uint64"), id("data"), pv),
			),
			ifErrReturn(),
			assign(ref, call(id("append"), ref, call(id("byte"), id("v")))),
		}
		loopBody = append(loopBody, inlineSkipWSStmts(posVar)...)
		// if pv < len(data) && data[pv] == ',' { pv++; ws; continue }
		continueArm := []ast.Stmt{inc(pv)}
		continueArm = append(continueArm, inlineSkipWSStmts(posVar)...)
		continueArm = append(continueArm, &ast.BranchStmt{Tok: token.CONTINUE})
		loopBody = append(loopBody,
			ifStmt(
				land(binop(pv, token.LSS, idCall("len", id("data"))), binop(dataAtPv, token.EQL, charLit(','))),
				continueArm...,
			),
			&ast.BranchStmt{Tok: token.BREAK},
		)

		body = append(body,
			varDecl("v", "uint64"),
			forCond(
				land(binop(pv, token.LSS, idCall("len", id("data"))), binop(dataAtPv, token.NEQ, charLit(']'))),
				loopBody...,
			),
			ifStmt(
				lor(
					binop(pv, token.GEQ, idCall("len", id("data"))),
					binop(dataAtPv, token.NEQ, charLit(']')),
				),
				retResultIErrExpr(idSel("scan", "ErrBadArray")),
			),
			inc(pv),
		)
		return []ast.Stmt{block(body...)}
	}

	// base64 / base32 / hex paths.
	var encExpr ast.Expr    // e.g. base64.StdEncoding
	var decodedLen ast.Expr // e.g. base64.StdEncoding.DecodedLen
	hexPath := false
	switch f.Format {
	case "base64url":
		encExpr = idSel("base64", "URLEncoding")
		decodedLen = sel(idSel("base64", "URLEncoding"), "DecodedLen")
	case "base32":
		encExpr = idSel("base32", "StdEncoding")
		decodedLen = sel(idSel("base32", "StdEncoding"), "DecodedLen")
	case "base32hex":
		encExpr = idSel("base32", "HexEncoding")
		decodedLen = sel(idSel("base32", "HexEncoding"), "DecodedLen")
	case "base16", "hex":
		hexPath = true
		decodedLen = idSel("hex", "DecodedLen")
	default:
		encExpr = idSel("base64", "StdEncoding")
		decodedLen = sel(idSel("base64", "StdEncoding"), "DecodedLen")
	}

	// unsafe.Slice(unsafe.StringData(s), len(s)) — non-copy []byte view.
	sliceCall := call(idSel("unsafe", "Slice"),
		call(idSel("unsafe", "StringData"), id("s")),
		idCall("len", id("s")),
	)
	makeCall := call(id("make"),
		&ast.ArrayType{Elt: id("byte")},
		intLit(0),
		call(decodedLen, idCall("len", id("s"))),
	)
	var decodeCall ast.Expr
	if hexPath {
		decodeCall = call(idSel("hex", "AppendDecode"), ref, sliceCall)
	} else {
		decodeCall = call(sel(encExpr, "AppendDecode"), ref, sliceCall)
	}

	return scanThenAssign(posVar,
		assign(ref, makeCall),
		assignN([]ast.Expr{ref, id("err")}, decodeCall),
		ifErrReturn(),
	)
}

// renderSQLNullStmts emits decode for a database/sql.NullX field. Probes
// for `null` literal first (→ Valid=false); otherwise reads the inner
// kind via the kind-appropriate scanner and constructs the typed
// literal with Valid=true.
func renderSQLNullStmts(f FieldInfo, ref ast.Expr, posVar string) []ast.Stmt {
	spec, ok := SQLNullSpec(f.GoType)
	if !ok {
		return nil
	}
	pv := id(posVar)
	dataAtPv := index(id("data"), pv)
	nv := id("nv")

	// Null branch: ref = sql.X{}; pv += 4
	zeroLit := &ast.CompositeLit{Type: idSel("sql", sqlTypeName(f.GoType))}
	nullBranch := []ast.Stmt{
		assign(ref, zeroLit),
		incBy(posVar, 4),
	}

	// Else branch: declare nv, parse it, then assign struct literal.
	var elseBody []ast.Stmt
	switch spec.Inner {
	case KindString:
		elseBody = append(elseBody, varDecl("nv", "string"))
		elseBody = append(elseBody, inlineScanStringStmts(posVar, nv, posVar)...)
	case KindBool:
		elseBody = append(elseBody,
			varDecl("nv", "bool"),
			assignN(
				[]ast.Expr{nv, pv, id("err")},
				call(idSel("scan", "Bool"), id("data"), pv),
			),
			ifErrReturn(),
		)
	case KindInt64:
		elseBody = append(elseBody, varDecl("nv", "int64"))
		elseBody = append(elseBody, inlineScanInt64Stmts(posVar, nv, "")...)
	case KindInt32, KindInt16:
		elseBody = append(elseBody, varDecl("nv", spec.Type))
		elseBody = append(elseBody, inlineScanInt64Stmts(posVar, nv, spec.Type)...)
	case KindUint8:
		elseBody = append(elseBody, varDecl("nv", spec.Type))
		elseBody = append(elseBody, inlineScanUint64Stmts(posVar, nv, spec.Type)...)
	case KindFloat64:
		elseBody = append(elseBody,
			varDecl("nv", "float64"),
			assignN(
				[]ast.Expr{nv, pv, id("err")},
				call(idSel("scan", "Float64"), id("data"), pv),
			),
			ifErrReturn(),
		)
	case KindTime:
		elseBody = append(elseBody, varDecl("nv", "time.Time"))
		tf := FieldInfo{Format: f.Format}
		elseBody = append(elseBody, renderTimeStmts(tf, nv, posVar)...)
	}
	validLit := &ast.CompositeLit{
		Type: idSel("sql", sqlTypeName(f.GoType)),
		Elts: []ast.Expr{
			&ast.KeyValueExpr{Key: id(spec.Field), Value: nv},
			&ast.KeyValueExpr{Key: id("Valid"), Value: id("true")},
		},
	}
	elseBody = append(elseBody, assign(ref, validLit))

	// null peek: pv+4 <= len(data) && data[pv]=='n' && data[pv+1]=='u' && ...
	nullCond := land(
		binop(binop(pv, token.ADD, intLit(4)), token.LEQ, idCall("len", id("data"))),
		binop(dataAtPv, token.EQL, charLit('n')),
		binop(index(id("data"), binop(pv, token.ADD, intLit(1))), token.EQL, charLit('u')),
		binop(index(id("data"), binop(pv, token.ADD, intLit(2))), token.EQL, charLit('l')),
		binop(index(id("data"), binop(pv, token.ADD, intLit(3))), token.EQL, charLit('l')),
	)
	return []ast.Stmt{block(
		ifElse(nullCond, nullBranch, elseBody),
	)}
}

// renderAnyStmts emits scan.Any / scan.AnyNumber dispatch into ref.
// UseNumber routes numbers into json.Number (aliased zero-copy).
func renderAnyStmts(f FieldInfo, ref ast.Expr, posVar string) []ast.Stmt {
	pv := id(posVar)
	fn := idSel("scan", "Any")
	if f.UseNumber {
		fn = idSel("scan", "AnyNumber")
	}
	return []ast.Stmt{
		assignN([]ast.Expr{ref, pv, id("err")}, call(fn, id("data"), pv)),
		ifErrReturn(),
	}
}

// inlineScanUint64Stmts is the unsigned counterpart of
// inlineScanInt64Stmts — no neg branch, n is uint64, cast via uint64(...)
// in the digit add.
func inlineScanUint64Stmts(posVar string, dst ast.Expr, castFn string) []ast.Stmt {
	pv := id(posVar)
	dataAt := index(id("data"), pv)
	digit := func(op token.Token, lit byte) ast.Expr {
		return binop(dataAt, op, charLit(lit))
	}

	var finalAssign ast.Stmt
	if castFn != "" {
		finalAssign = assign(dst, call(id(castFn), id("n")))
	} else {
		finalAssign = assign(dst, id("n"))
	}

	return []ast.Stmt{block(
		ifStmt(
			lor(
				binop(pv, token.GEQ, idCall("len", id("data"))),
				digit(token.LSS, '0'),
				digit(token.GTR, '9'),
			),
			retResultIErrExpr(idSel("scan", "ErrBadNumber")),
		),
		varDecl("n", "uint64"),
		forCond(
			land(
				binop(pv, token.LSS, idCall("len", id("data"))),
				digit(token.GEQ, '0'),
				digit(token.LEQ, '9'),
			),
			assign(id("n"), binop(
				binop(id("n"), token.MUL, intLit(10)),
				token.ADD,
				call(id("uint64"), binop(dataAt, token.SUB, charLit('0'))),
			)),
			inc(pv),
		),
		finalAssign,
	)}
}
