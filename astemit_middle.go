package main

// Mid-level AST renderers: mods, validation, string-tag wrap, unknown-key
// dispatch tail, required/array-len typed error literals. Each is a self-
// contained `*Stmts` (or `*Expr`) function that builds AST nodes and
// either returns them or is consumed by callers in the same module.

import (
	"go/ast"
	"go/token"
	"strings"
)

// requiredErrLit builds `&validation.RequiredError{Field: <jsonName>}` —
// the typed error literal emitted at required-field post-loop checks.
func requiredErrLit(jsonName string) ast.Expr {
	return addr(&ast.CompositeLit{
		Type: idSel("validation", "RequiredError"),
		Elts: []ast.Expr{
			&ast.KeyValueExpr{Key: id("Field"), Value: strLit(jsonName)},
		},
	})
}

// arrayLenErrExpr builds `&validation.LenError{Field, Want, Got}` for
// strict fixed-array element-count failures.
func arrayLenErrExpr(field string, want int, got ast.Expr) ast.Expr {
	return addr(&ast.CompositeLit{
		Type: idSel("validation", "LenError"),
		Elts: []ast.Expr{
			&ast.KeyValueExpr{Key: id("Field"), Value: strLit(field)},
			&ast.KeyValueExpr{Key: id("Want"), Value: intLit(want)},
			&ast.KeyValueExpr{Key: id("Got"), Value: got},
		},
	})
}

// onErrAction builds the failure-emit statement for a validation rule.
// multiErr→append to errs; otherwise early-return with the 2- or 3-tuple
// shape selected by posVar (empty=2-tuple at struct level, non-empty=3-
// tuple inside a slice/map element decode).
func onErrAction(errExpr ast.Expr, multiErr bool, posVar string) ast.Stmt {
	if multiErr {
		return assign(id("errs"), call(id("append"), id("errs"), errExpr))
	}
	if posVar == "" {
		return retStmt(id("result"), errExpr)
	}
	return retStmt(id("result"), id(posVar), errExpr)
}

// fieldKV builds a struct-literal field initializer: Key: Value.
func fieldKV(k string, v ast.Expr) *ast.KeyValueExpr {
	return &ast.KeyValueExpr{Key: id(k), Value: v}
}

// renderModsStmts emits post-decode transformation code against ref using
// the field's Mods list. Mirrors renderMods but produces []ast.Stmt.
// goType + kind handle aliased-primitive casts (so e.g. strings.TrimSpace
// accepts an `AliasString` via cast through `string`).
func renderModsStmts(mods []ModRule, ref ast.Expr, goType string, kind TypeKind) []ast.Stmt {
	if len(mods) == 0 {
		return nil
	}
	if k, ok := generatedAliasKinds[goType]; ok {
		kind = k
	}
	prim := kindPrimitiveName(kind)
	cast := goType != "" && prim != "" && goType != prim
	wrap := func(rhs ast.Expr) ast.Expr {
		if cast {
			return call(id(goType), rhs)
		}
		return rhs
	}
	asPrim := func(e ast.Expr) ast.Expr {
		if cast {
			return call(id(prim), e)
		}
		return e
	}
	r := func() ast.Expr { return cloneExpr(ref) }

	var out []ast.Stmt
	for _, m := range mods {
		if m.Custom {
			fnExpr := ast.Expr(id(m.FuncName))
			if m.PkgName != "" {
				fnExpr = idSel(m.PkgName, m.FuncName)
			}
			if m.Fallible {
				// if v, err := Fn(ref); err != nil { return ... } else { ref = v }
				out = append(out, &ast.IfStmt{
					Init: shortDeclN(
						[]ast.Expr{id("v"), id("err")},
						call(fnExpr, r()),
					),
					Cond: binop(id("err"), token.NEQ, id("nil")),
					Body: block(retResultIErr()),
					Else: block(assign(r(), id("v"))),
				})
			} else {
				out = append(out, assign(r(), call(fnExpr, r())))
			}
			continue
		}
		switch m.Name {
		case "trim":
			out = append(out, assign(r(), wrap(call(idSel("strings", "TrimSpace"), asPrim(r())))))
		case "lower":
			out = append(out, assign(r(), wrap(call(idSel("strings", "ToLower"), asPrim(r())))))
		case "upper":
			out = append(out, assign(r(), wrap(call(idSel("strings", "ToUpper"), asPrim(r())))))
		case "trimleft":
			out = append(out, assign(r(), wrap(call(idSel("strings", "TrimPrefix"), asPrim(r()), strLit(m.Value)))))
		case "trimright":
			out = append(out, assign(r(), wrap(call(idSel("strings", "TrimSuffix"), asPrim(r()), strLit(m.Value)))))
		case "replace":
			parts := strings.SplitN(m.Value, "|", 2)
			if len(parts) == 2 {
				out = append(out, assign(r(),
					wrap(call(idSel("strings", "ReplaceAll"), asPrim(r()), strLit(parts[0]), strLit(parts[1]))),
				))
			}
		case "clamp":
			lo, hi, ok := strings.Cut(m.Value, "|")
			if !ok {
				continue
			}
			lo = strings.TrimSpace(lo)
			hi = strings.TrimSpace(hi)
			if lo != "" {
				out = append(out, ifStmt(
					binop(r(), token.LSS, wrap(parseExpr(lo))),
					assign(r(), wrap(parseExpr(lo))),
				))
			}
			if hi != "" {
				out = append(out, ifStmt(
					binop(r(), token.GTR, wrap(parseExpr(hi))),
					assign(r(), wrap(parseExpr(hi))),
				))
			}
		}
	}
	return out
}

// validationErrLit builds `&validation.<Name>Error{...elts}`.
func validationErrLit(name string, elts ...ast.Expr) ast.Expr {
	return addr(&ast.CompositeLit{
		Type: idSel("validation", name),
		Elts: elts,
	})
}

// renderValidationOnStmts mirrors renderValidationOn but builds AST. Each
// rule emits an `if <fail-cond> { onErr }` shape. posVar selects the
// return-tuple shape used by onErr; multiErr swaps return for append.
//
// Every reference to the field expression (`ref`) and the typed-error
// "Field" literal goes through a fresh clone — format.Node's position
// tracking breaks when the same ast.Node instance appears more than
// once in the emitted tree, causing weird line wraps in deeply-nested
// contexts.
func renderValidationOnStmts(rules []ValidationRule, ref ast.Expr, jsonName string, kind TypeKind, multiErr bool, posVar string) []ast.Stmt {
	if len(rules) == 0 {
		return nil
	}
	emit := func(failCond ast.Expr, errExpr ast.Expr) ast.Stmt {
		return ifStmt(failCond, onErrAction(errExpr, multiErr, posVar))
	}
	r := func() ast.Expr { return cloneExpr(ref) }
	fldKV := func() *ast.KeyValueExpr { return fieldKV("Field", strLit(jsonName)) }
	lenOf := func() ast.Expr { return idCall("len", r()) }
	runesOf := func() ast.Expr {
		return call(idSel("utf8", "RuneCountInString"), r())
	}

	var out []ast.Stmt
	for _, v := range rules {
		switch v.Name {
		case "required", "optional":
			// Handled outside this routine.

		case "notempty":
			out = append(out, emit(
				binop(lenOf(), token.EQL, intLit(0)),
				validationErrLit("NotEmptyError", fldKV()),
			))

		case "len":
			out = append(out, emit(
				binop(lenOf(), token.NEQ, parseExpr(v.Value)),
				validationErrLit("LenError",
					fldKV(),
					fieldKV("Want", parseExpr(v.Value)),
					fieldKV("Got", lenOf()),
				),
			))
		case "minlen":
			out = append(out, emit(
				binop(lenOf(), token.LSS, parseExpr(v.Value)),
				validationErrLit("MinLenError",
					fldKV(),
					fieldKV("Limit", parseExpr(v.Value)),
					fieldKV("Got", lenOf()),
				),
			))
		case "maxlen":
			out = append(out, emit(
				binop(lenOf(), token.GTR, parseExpr(v.Value)),
				validationErrLit("MaxLenError",
					fldKV(),
					fieldKV("Limit", parseExpr(v.Value)),
					fieldKV("Got", lenOf()),
				),
			))

		case "runes":
			out = append(out, emit(
				binop(runesOf(), token.NEQ, parseExpr(v.Value)),
				validationErrLit("RunesError",
					fldKV(),
					fieldKV("Want", parseExpr(v.Value)),
					fieldKV("Got", runesOf()),
				),
			))
		case "minrunes":
			out = append(out, emit(
				binop(runesOf(), token.LSS, parseExpr(v.Value)),
				validationErrLit("MinRunesError",
					fldKV(),
					fieldKV("Limit", parseExpr(v.Value)),
					fieldKV("Got", runesOf()),
				),
			))
		case "maxrunes":
			out = append(out, emit(
				binop(runesOf(), token.GTR, parseExpr(v.Value)),
				validationErrLit("MaxRunesError",
					fldKV(),
					fieldKV("Limit", parseExpr(v.Value)),
					fieldKV("Got", runesOf()),
				),
			))

		case "gt":
			out = append(out, emit(
				binop(r(), token.LEQ, parseExpr(v.Value)),
				validationErrLit("GTError",
					fldKV(),
					fieldKV("Limit", parseExpr(v.Value)),
					fieldKV("Value", r()),
				),
			))
		case "gte":
			out = append(out, emit(
				binop(r(), token.LSS, parseExpr(v.Value)),
				validationErrLit("GTEError",
					fldKV(),
					fieldKV("Limit", parseExpr(v.Value)),
					fieldKV("Value", r()),
				),
			))
		case "lt":
			out = append(out, emit(
				binop(r(), token.GEQ, parseExpr(v.Value)),
				validationErrLit("LTError",
					fldKV(),
					fieldKV("Limit", parseExpr(v.Value)),
					fieldKV("Value", r()),
				),
			))
		case "lte":
			out = append(out, emit(
				binop(r(), token.GTR, parseExpr(v.Value)),
				validationErrLit("LTEError",
					fldKV(),
					fieldKV("Limit", parseExpr(v.Value)),
					fieldKV("Value", r()),
				),
			))

		case "eq":
			if kind == KindString {
				out = append(out, emit(
					binop(r(), token.NEQ, strLit(v.Value)),
					validationErrLit("EqError",
						fldKV(),
						fieldKV("Want", strLit(v.Value)),
						fieldKV("Value", r()),
					),
				))
			} else if isNumeric(kind) {
				out = append(out, emit(
					binop(r(), token.NEQ, parseExpr(v.Value)),
					validationErrLit("EqError",
						fldKV(),
						fieldKV("Want", parseExpr(v.Value)),
						fieldKV("Value", r()),
					),
				))
			}
		case "neq":
			if kind == KindString {
				out = append(out, emit(
					binop(r(), token.EQL, strLit(v.Value)),
					validationErrLit("NeqError",
						fldKV(),
						fieldKV("Want", strLit(v.Value)),
						fieldKV("Value", r()),
					),
				))
			} else if isNumeric(kind) {
				out = append(out, emit(
					binop(r(), token.EQL, parseExpr(v.Value)),
					validationErrLit("NeqError",
						fldKV(),
						fieldKV("Want", parseExpr(v.Value)),
						fieldKV("Value", r()),
					),
				))
			}

		case "oneof":
			parts := strings.Split(v.Value, "|")
			name := registerOneOf(parts)
			caseExprs := make([]ast.Expr, 0, len(parts))
			for _, p := range parts {
				if kind == KindString {
					caseExprs = append(caseExprs, strLit(p))
				} else {
					caseExprs = append(caseExprs, parseExpr(p))
				}
			}
			out = append(out, &ast.SwitchStmt{
				Tag: r(),
				Body: block(
					&ast.CaseClause{List: caseExprs},
					&ast.CaseClause{
						List: nil,
						Body: []ast.Stmt{onErrAction(
							validationErrLit("OneOfError",
								fldKV(),
								fieldKV("Allowed", id(name)),
								fieldKV("Value", r()),
							),
							multiErr, posVar,
						)},
					},
				),
			})

		case "email":
			out = append(out, emit(
				not(call(idSel("decode", "IsEmail"), r())),
				validationErrLit("EmailError", fldKV(), fieldKV("Value", r())),
			))
		case "url":
			out = append(out, emit(
				not(call(idSel("decode", "IsURL"), r())),
				validationErrLit("URLError", fldKV(), fieldKV("Value", r())),
			))
		case "ascii":
			out = append(out, emit(
				not(call(idSel("decode", "IsASCII"), r())),
				validationErrLit("ASCIIError", fldKV(), fieldKV("Value", r())),
			))
		case "printable":
			out = append(out, emit(
				not(call(idSel("decode", "IsPrintable"), r())),
				validationErrLit("PrintableError", fldKV(), fieldKV("Value", r())),
			))
		case "alphanum":
			out = append(out, emit(
				not(call(idSel("decode", "IsAlphanum"), r())),
				validationErrLit("AlphanumError", fldKV(), fieldKV("Value", r())),
			))
		case "numeric":
			out = append(out, emit(
				not(call(idSel("decode", "IsNumeric"), r())),
				validationErrLit("NumericError", fldKV(), fieldKV("Value", r())),
			))
		case "lower":
			out = append(out, emit(
				not(call(idSel("decode", "IsLower"), r())),
				validationErrLit("LowerError", fldKV(), fieldKV("Value", r())),
			))
		case "upper":
			out = append(out, emit(
				not(call(idSel("decode", "IsUpper"), r())),
				validationErrLit("UpperError", fldKV(), fieldKV("Value", r())),
			))
		case "hexadecimal":
			out = append(out, emit(
				not(call(idSel("decode", "IsHex"), r())),
				validationErrLit("HexadecimalError", fldKV(), fieldKV("Value", r())),
			))

		case "starts":
			out = append(out, emit(
				not(call(idSel("strings", "HasPrefix"), r(), strLit(v.Value))),
				validationErrLit("StartsError",
					fldKV(),
					fieldKV("Want", strLit(v.Value)),
					fieldKV("Value", r()),
				),
			))
		case "ends":
			out = append(out, emit(
				not(call(idSel("strings", "HasSuffix"), r(), strLit(v.Value))),
				validationErrLit("EndsError",
					fldKV(),
					fieldKV("Want", strLit(v.Value)),
					fieldKV("Value", r()),
				),
			))
		case "contains":
			out = append(out, emit(
				not(call(idSel("strings", "Contains"), r(), strLit(v.Value))),
				validationErrLit("ContainsError",
					fldKV(),
					fieldKV("Want", strLit(v.Value)),
					fieldKV("Value", r()),
				),
			))

		case "multiple":
			out = append(out, emit(
				binop(binop(r(), token.REM, parseExpr(v.Value)), token.NEQ, intLit(0)),
				validationErrLit("MultipleError",
					fldKV(),
					fieldKV("Of", parseExpr(v.Value)),
					fieldKV("Value", r()),
				),
			))
		}

		// Custom validator (@FuncName / @pkg.FuncName).
		if v.Custom {
			fnExpr := ast.Expr(id(v.FuncName))
			if v.PkgName != "" {
				fnExpr = idSel(v.PkgName, v.FuncName)
			}
			out = append(out, &ast.IfStmt{
				Init: shortDecl(id("err"), call(fnExpr, r())),
				Cond: binop(id("err"), token.NEQ, id("nil")),
				Body: block(onErrAction(
					validationErrLit("CustomError",
						fldKV(),
						fieldKV("Name", strLit(v.Name)),
						fieldKV("Cause", id("err")),
					),
					multiErr, posVar,
				)),
			})
		}
	}
	return out
}

// validateAndModStmts is the AST-emitting counterpart of validateAndMod.
// Skipped entirely when f.NoValidate.
func validateAndModStmts(f FieldInfo, ref ast.Expr) []ast.Stmt {
	var out []ast.Stmt
	if len(f.Mods) > 0 {
		out = append(out, renderModsStmts(f.Mods, ref, f.GoType, f.Kind)...)
	}
	if len(f.Validation) > 0 {
		out = append(out, renderValidationOnStmts(f.Validation, ref, f.JSONName, f.Kind, f.MultiErr, "i")...)
	}
	return out
}

// unknownKeyStmts emits the default branch of the field-name switch.
// Three modes: inline catch-all absorbs the unknown pair into a map;
// IgnoreUnknown silently skips; default returns an UnknownKeyError (or
// appends one in multierr mode).
func unknownKeyStmts(s StructInfo, posVar string) []ast.Stmt {
	pv := id(posVar)
	if inline := s.InlineField(); inline.Inline {
		anyFn := idSel("scan", "Any")
		if s.UseNumber {
			anyFn = idSel("scan", "AnyNumber")
		}
		// if result.X == nil { result.X = make(<GoType>) }
		mapAccess := idSel("result", inline.GoName)
		makeStmt := assign(mapAccess, call(id("make"), parseExpr(inline.GoType)))
		// result.X[key], posVar, err = scan.Any(data, posVar)
		mapIdx := &ast.IndexExpr{X: mapAccess, Index: id("key")}
		return []ast.Stmt{
			ifStmt(binop(mapAccess, token.EQL, id("nil")), makeStmt),
			assignN(
				[]ast.Expr{mapIdx, pv, id("err")},
				call(anyFn, id("data"), pv),
			),
			ifErrReturn(),
		}
	}
	if s.IgnoreUnknown {
		return []ast.Stmt{
			assignN(
				[]ast.Expr{pv, id("err")},
				call(idSel("scan", "SkipValue"), id("data"), pv),
			),
			ifErrReturn(),
		}
	}
	if s.MultiErr {
		return []ast.Stmt{
			assign(id("errs"), call(id("append"), id("errs"),
				validationErrLit("UnknownKeyError",
					fieldKV("Field", id("key")),
				),
			)),
			assignN(
				[]ast.Expr{pv, id("err")},
				call(idSel("scan", "SkipValue"), id("data"), pv),
			),
			ifErrReturn(),
		}
	}
	return []ast.Stmt{
		retResultIErrExpr(validationErrLit("UnknownKeyError",
			fieldKV("Field", id("key")),
		)),
	}
}

// renderStringTagStmts emits json:",string"-tagged decode. Reads a JSON
// string into `sv`, then parses its contents via strconv into the
// field's numeric / bool type.
func renderStringTagStmts(f FieldInfo, ref ast.Expr, posVar string) []ast.Stmt {
	body := []ast.Stmt{varDecl("sv", "string")}
	body = append(body, inlineScanStringStmts(posVar, id("sv"), posVar)...)

	switch f.Kind {
	case KindBool:
		body = append(body, &ast.SwitchStmt{
			Tag: id("sv"),
			Body: block(
				&ast.CaseClause{
					List: []ast.Expr{strLit("true")},
					Body: []ast.Stmt{assign(ref, id("true"))},
				},
				&ast.CaseClause{
					List: []ast.Expr{strLit("false")},
					Body: []ast.Stmt{assign(ref, id("false"))},
				},
				&ast.CaseClause{
					List: nil,
					Body: []ast.Stmt{retResultIErrExpr(idSel("scan", "ErrBadBool"))},
				},
			),
		})
	case KindFloat32, KindFloat64:
		body = append(body,
			shortDeclN(
				[]ast.Expr{id("f"), id("err")},
				call(idSel("strconv", "ParseFloat"), id("sv"), intLit(64)),
			),
			ifErrReturn(),
		)
		if f.Kind == KindFloat32 {
			body = append(body, assign(ref, call(id("float32"), id("f"))))
		} else {
			body = append(body, assign(ref, id("f")))
		}
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		body = append(body,
			shortDeclN(
				[]ast.Expr{id("u"), id("err")},
				call(idSel("strconv", "ParseUint"), id("sv"), intLit(10), intLit(64)),
			),
			ifErrReturn(),
		)
		if f.Kind == KindUint64 {
			body = append(body, assign(ref, id("u")))
		} else {
			body = append(body, assign(ref, call(id(f.GoType), id("u"))))
		}
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		body = append(body,
			shortDeclN(
				[]ast.Expr{id("n"), id("err")},
				call(idSel("strconv", "ParseInt"), id("sv"), intLit(10), intLit(64)),
			),
			ifErrReturn(),
		)
		if f.Kind == KindInt64 {
			body = append(body, assign(ref, id("n")))
		} else {
			body = append(body, assign(ref, call(id(f.GoType), id("n"))))
		}
	case KindString:
		body = append(body, assign(ref, id("sv")))
	}

	return []ast.Stmt{block(body...)}
}

// renderFieldStmts is the AST counterpart of renderField — the per-field
// dispatch hub. Each branch builds Go statements for one value-shape:
// pointer null-peek, string-tagged, the per-Kind switch, and the closing
// validate+mod tail. Container kinds (slice/array/map) and cross-package
// structs still emit text internally; bridgeStmts lifts that text into
// AST for inclusion here. Once those renderers convert too, the bridge
// calls disappear.
func renderFieldStmts(f FieldInfo, ref ast.Expr, posVar string) []ast.Stmt {
	if f.String {
		out := renderStringTagStmts(f, ref, posVar)
		if !f.NoValidate {
			out = append(out, validateAndModStmts(f, ref)...)
		}
		return out
	}
	if f.Pointer {
		// null → ref = nil + posVar += 4 (inline null peek).
		// non-null → var v Pointee; recurse into inner kind; ref = &v.
		// Custom @-rules apply to the *T (ref), so we partition and run
		// only built-ins on the inner; @-rules run after the if/else on
		// ref itself.
		inner := f
		inner.Pointer = false
		if inner.PointeeType != "" {
			inner.GoType = inner.PointeeType
		}
		builtinV, customV := partitionCustomValidation(f.Validation)
		builtinM, customM := partitionCustomMods(f.Mods)
		inner.Validation = builtinV
		inner.Mods = builtinM
		innerStmts := renderFieldStmts(inner, id("v"), posVar)

		nullBody := []ast.Stmt{assign(ref, id("nil"))}
		elseBody := []ast.Stmt{varDecl("v", inner.GoType)}
		elseBody = append(elseBody, innerStmts...)
		elseBody = append(elseBody, assign(ref, addr(id("v"))))

		out := []ast.Stmt{inlineNullPeekStmts(posVar, nullBody, elseBody)}
		if !f.NoValidate && (len(customV) > 0 || len(customM) > 0) {
			outer := f
			outer.Validation = customV
			outer.Mods = customM
			out = append(out, validateAndModStmts(outer, ref)...)
		}
		return out
	}

	pv := id(posVar)
	var out []ast.Stmt
	switch f.Kind {
	case KindString:
		out = inlineScanStringStmts(posVar, ref, posVar)
	case KindBool:
		out = []ast.Stmt{
			assignN(
				[]ast.Expr{ref, pv, id("err")},
				call(idSel("scan", "Bool"), id("data"), pv),
			),
			ifErrReturn(),
		}
	case KindInt, KindInt8, KindInt16, KindInt32:
		out = inlineScanInt64Stmts(posVar, ref, f.GoType)
	case KindInt64:
		out = inlineScanInt64Stmts(posVar, ref, "")
	case KindUint, KindUint8, KindUint16, KindUint32:
		out = inlineScanUint64Stmts(posVar, ref, f.GoType)
	case KindUint64:
		out = inlineScanUint64Stmts(posVar, ref, "")
	case KindFloat32:
		// var fv float64; fv,pv,err = scan.Float64(...); if err...; ref = float32(fv)
		out = []ast.Stmt{
			varDecl("fv", "float64"),
			assignN(
				[]ast.Expr{id("fv"), pv, id("err")},
				call(idSel("scan", "Float64"), id("data"), pv),
			),
			ifErrReturn(),
			assign(ref, call(id("float32"), id("fv"))),
		}
	case KindFloat64:
		out = []ast.Stmt{
			assignN(
				[]ast.Expr{ref, pv, id("err")},
				call(idSel("scan", "Float64"), id("data"), pv),
			),
			ifErrReturn(),
		}
	case KindStruct:
		if isGenerated(f.GoType) {
			out = []ast.Stmt{
				assignN(
					[]ast.Expr{ref, pv, id("err")},
					call(sel(ref, "DecodeFrom"), id("data"), pv),
				),
				ifErrReturn(),
			}
		} else {
			out = renderCrossPkgStructDecodeStmts(f, exprText(ref), posVar)
		}
	case KindSlice:
		out = emitByteSliceReadStmts(f, ref, posVar, 0)
	case KindArray:
		out = emitByteSliceReadStmts(f, ref, posVar, 0)
	case KindMap:
		out = renderMapStmts(f, ref, posVar)
	case KindBytes:
		out = renderBytesStmts(f, ref, posVar)
	case KindTime:
		out = renderTimeStmts(f, ref, posVar)
	case KindDuration:
		out = renderDurationStmts(f, ref, posVar)
	case KindNetIP:
		out = renderNetIPStmts(ref, posVar)
	case KindNetipAddr:
		out = renderNetipAddrStmts(ref, posVar)
	case KindNetipPrefix:
		out = renderNetipPrefixStmts(ref, posVar)
	case KindRawJSON:
		out = renderRawJSONStmts(ref, posVar)
	case KindURL:
		out = renderURLStmts(ref, posVar)
	case KindBigInt:
		out = renderBigIntStmts(ref, posVar)
	case KindBigFloat:
		out = renderBigFloatStmts(ref, posVar)
	case KindBigRat:
		out = renderBigRatStmts(ref, posVar)
	case KindSQLNull:
		out = renderSQLNullStmts(f, ref, posVar)
	case KindAny:
		out = renderAnyStmts(f, ref, posVar)
	default:
		// Unknown kind: skip the value. k+err declarations shadow.
		out = []ast.Stmt{
			shortDeclN(
				[]ast.Expr{id("k"), id("err")},
				call(idSel("scan", "SkipValue"), id("data"), pv),
			),
			ifErrReturn(),
			assign(pv, id("k")),
		}
	}

	if !f.NoValidate {
		out = append(out, validateAndModStmts(f, ref)...)
	}
	return out
}

// renderMapStmts emits map[string]V decode. `null` keeps the field nil
// (map zero value); else opens `{`, then per-entry: scan key, validate/
// mod key, `:`, then per-value scanner per ElemKind, dive mods/validation
// on the entry, `,`-or-`}` loop. mk2 is the post-value cursor returned by
// non-inline scanners (scan.Bool, scan.Float64, DecodeFrom) — assigned
// back to posVar after the entry stores into the map.
func renderMapStmts(f FieldInfo, ref ast.Expr, posVar string) []ast.Stmt {
	pv := id(posVar)
	dataAt := index(id("data"), pv)

	var makeExpr ast.Expr
	if cap := mapPreallocCap(f); cap > 0 {
		makeExpr = call(id("make"), parseExpr(f.GoType), intLit(cap))
	} else {
		makeExpr = call(id("make"), parseExpr(f.GoType))
	}
	mapTarget := &ast.IndexExpr{X: ref, Index: id("mk")}

	// Value scan by ElemKind. The non-inline scanners (Bool / Float64 /
	// DecodeFrom / cross-pkg) return their own post-cursor (mk2); the
	// inline scanners write directly to pv. We unify by always assigning
	// mapTarget at the end.
	var valueScan []ast.Stmt
	switch f.ElemKind {
	case KindString:
		valueScan = []ast.Stmt{varDecl("mv", "string")}
		valueScan = append(valueScan, inlineScanStringStmts(posVar, id("mv"), posVar)...)
		valueScan = append(valueScan, assign(mapTarget, id("mv")))
	case KindBool:
		valueScan = []ast.Stmt{
			shortDeclN(
				[]ast.Expr{id("mv"), id("mk2"), id("err")},
				call(idSel("scan", "Bool"), id("data"), pv),
			),
			ifErrReturn(),
			assign(mapTarget, id("mv")),
			assign(pv, id("mk2")),
		}
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		valueScan = []ast.Stmt{varDecl("mn", "int64")}
		valueScan = append(valueScan, inlineScanInt64Stmts(posVar, id("mn"), "")...)
		if f.ElemType != "int64" {
			valueScan = append(valueScan, assign(mapTarget, call(id(f.ElemType), id("mn"))))
		} else {
			valueScan = append(valueScan, assign(mapTarget, id("mn")))
		}
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		valueScan = []ast.Stmt{varDecl("mn", "uint64")}
		valueScan = append(valueScan, inlineScanUint64Stmts(posVar, id("mn"), "")...)
		if f.ElemType != "uint64" {
			valueScan = append(valueScan, assign(mapTarget, call(id(f.ElemType), id("mn"))))
		} else {
			valueScan = append(valueScan, assign(mapTarget, id("mn")))
		}
	case KindFloat32, KindFloat64:
		valueScan = []ast.Stmt{
			shortDeclN(
				[]ast.Expr{id("mv"), id("mk2"), id("err")},
				call(idSel("scan", "Float64"), id("data"), pv),
			),
			ifErrReturn(),
		}
		if f.ElemKind == KindFloat32 {
			valueScan = append(valueScan, assign(mapTarget, call(id("float32"), id("mv"))))
		} else {
			valueScan = append(valueScan, assign(mapTarget, id("mv")))
		}
		valueScan = append(valueScan, assign(pv, id("mk2")))
	case KindStruct:
		if isGenerated(f.ElemType) {
			// var mv T; mv, mk2, err := mv.DecodeFrom(data, pv)
			// (the := also re-declares mv in the same scope, with the new
			// value from DecodeFrom.)
			valueScan = []ast.Stmt{
				varDecl("mv", f.ElemType),
				shortDeclN(
					[]ast.Expr{id("mv"), id("mk2"), id("err")},
					call(sel(id("mv"), "DecodeFrom"), id("data"), pv),
				),
				ifErrReturn(),
				assign(mapTarget, id("mv")),
				assign(pv, id("mk2")),
			}
		} else {
			// Cross-package struct fallback via encoding/json over the raw span.
			valueScan = []ast.Stmt{
				shortDecl(id("start"), pv),
				shortDeclN(
					[]ast.Expr{id("mk2"), id("err")},
					call(idSel("scan", "SkipValue"), id("data"), id("start")),
				),
				ifErrReturn(),
				varDecl("mv", f.ElemType),
				&ast.IfStmt{
					Init: shortDecl(id("err"), call(
						idSel("json", "Unmarshal"),
						slice2(id("data"), id("start"), id("mk2")),
						addr(id("mv")),
					)),
					Cond: binop(id("err"), token.NEQ, id("nil")),
					Body: block(retResultIErr()),
				},
				assign(mapTarget, id("mv")),
				assign(pv, id("mk2")),
			}
		}
	default:
		valueScan = []ast.Stmt{
			shortDeclN(
				[]ast.Expr{id("mk2"), id("err")},
				call(idSel("scan", "SkipValue"), id("data"), pv),
			),
			ifErrReturn(),
			assign(pv, id("mk2")),
		}
	}

	if len(f.ElemMods) > 0 {
		valueScan = append(valueScan, renderModsStmts(f.ElemMods, mapTarget, f.ElemType, f.ElemKind)...)
	}
	if len(f.ElemValidation) > 0 {
		valueScan = append(valueScan, renderValidationOnStmts(f.ElemValidation, mapTarget, f.JSONName+".value", f.ElemKind, f.MultiErr, "i")...)
	}

	// Per-entry loop body: key scan + validate + ':' + value + ',' or break.
	loopBody := []ast.Stmt{varDecl("mk", "string")}
	loopBody = append(loopBody, inlineScanStringStmts(posVar, id("mk"), posVar)...)
	if !f.NoValidate {
		if len(f.KeyMods) > 0 {
			loopBody = append(loopBody, renderModsStmts(f.KeyMods, id("mk"), "string", KindString)...)
		}
		if len(f.KeyValidation) > 0 {
			loopBody = append(loopBody, renderValidationOnStmts(f.KeyValidation, id("mk"), f.JSONName+".key", KindString, f.MultiErr, "i")...)
		}
	}
	loopBody = append(loopBody, inlineSkipWSStmts(posVar)...)
	loopBody = append(loopBody, ifStmt(
		lor(
			binop(pv, token.GEQ, idCall("len", id("data"))),
			binop(dataAt, token.NEQ, charLit(':')),
		),
		retResultIErrExpr(idSel("scan", "ErrBadObject")),
	), inc(pv))
	loopBody = append(loopBody, inlineSkipWSStmts(posVar)...)
	loopBody = append(loopBody, valueScan...)
	loopBody = append(loopBody, inlineSkipWSStmts(posVar)...)
	continueArm := []ast.Stmt{inc(pv)}
	continueArm = append(continueArm, inlineSkipWSStmts(posVar)...)
	continueArm = append(continueArm, &ast.BranchStmt{Tok: token.CONTINUE})
	loopBody = append(loopBody,
		ifStmt(
			land(binop(pv, token.LSS, idCall("len", id("data"))), binop(dataAt, token.EQL, charLit(','))),
			continueArm...,
		),
		&ast.BranchStmt{Tok: token.BREAK},
	)

	emptyBranch := []ast.Stmt{assign(ref, &ast.CompositeLit{Type: parseExpr(f.GoType)})}
	makeBranch := []ast.Stmt{assign(ref, makeExpr)}

	elseBody := []ast.Stmt{
		ifStmt(
			lor(
				binop(pv, token.GEQ, idCall("len", id("data"))),
				binop(dataAt, token.NEQ, charLit('{')),
			),
			retResultIErrExpr(idSel("scan", "ErrBadObject")),
		),
		inc(pv),
	}
	elseBody = append(elseBody, inlineSkipWSStmts(posVar)...)
	elseBody = append(elseBody, ifElse(
		land(binop(pv, token.LSS, idCall("len", id("data"))), binop(dataAt, token.EQL, charLit('}'))),
		emptyBranch, makeBranch,
	))
	elseBody = append(elseBody, forCond(
		land(binop(pv, token.LSS, idCall("len", id("data"))), binop(dataAt, token.NEQ, charLit('}'))),
		loopBody...,
	))
	elseBody = append(elseBody,
		ifStmt(
			lor(
				binop(pv, token.GEQ, idCall("len", id("data"))),
				binop(dataAt, token.NEQ, charLit('}')),
			),
			retResultIErrExpr(idSel("scan", "ErrBadObject")),
		),
		inc(pv),
	)

	outer := inlineSkipWSStmts(posVar)
	outer = append(outer, inlineNullPeekStmts(posVar, nil, elseBody))
	return []ast.Stmt{block(outer...)}
}
