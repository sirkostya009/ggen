package main

// AST emitters for the byte-path slice and array decoders. The recursive
// emitter handles arbitrary nesting — `[][]T`, `[N]T`, `[][N][]T`, etc.
// Each call peels one container layer off via peelSliceField; locals at
// the current depth carry the depth suffix (idx0, slab0 at the outer
// call; idx1, slab1 one level in; …).

import (
	"fmt"
	"go/ast"
	"go/token"
)

// emitByteSliceReadStmts emits a JSON-array reader at `dst`, advancing
// the caller-owned position variable `posVar`. f.Kind ∈ {KindSlice,
// KindArray}. depth threads through nested calls so locals don't
// collide across levels.
//
// Output shape:
//   - slice: SkipWS, peek `null` (→ leave dst nil + advance 4), else
//     check `[`, prealloc dst (+ slab if elem is pointer), loop, ']'.
//   - array: SkipWS, check `[`, declare idx + (slab if elem pointer),
//     loop with strict bound enforcement, ']', exact-count check.
func emitByteSliceReadStmts(f FieldInfo, dst ast.Expr, posVar string, depth int) []ast.Stmt {
	isArray := f.Kind == KindArray
	arrayN := f.ArrayLen
	pv := id(posVar)
	dataAt := index(id("data"), pv)
	ivar := id(fmt.Sprintf("idx%d", depth))
	slabVar := id(fmt.Sprintf("slab%d", depth))

	loop := elementLoopStmts(f, dst, posVar, depth, ivar, slabVar)
	forLoop := forCond(
		land(binop(pv, token.LSS, idCall("len", id("data"))), binop(dataAt, token.NEQ, charLit(']'))),
		loop...,
	)

	bracketCheck := ifStmt(
		lor(
			binop(pv, token.GEQ, idCall("len", id("data"))),
			binop(dataAt, token.NEQ, charLit('[')),
		),
		retResultIErrExpr(idSel("scan", "ErrBadArray")),
	)
	closingCheck := ifStmt(
		lor(
			binop(pv, token.GEQ, idCall("len", id("data"))),
			binop(dataAt, token.NEQ, charLit(']')),
		),
		retResultIErrExpr(idSel("scan", "ErrBadArray")),
	)

	if isArray {
		body := inlineSkipWSStmts(posVar)
		body = append(body, bracketCheck, inc(pv))
		body = append(body, inlineSkipWSStmts(posVar)...)
		body = append(body, varDecl(ivar.Name, "int"))
		if f.ElemPointer {
			body = append(body, &ast.AssignStmt{
				Lhs: []ast.Expr{slabVar}, Tok: token.DEFINE,
				Rhs: []ast.Expr{call(id("make"),
					&ast.ArrayType{Elt: parseExpr(f.ElemType)},
					intLit(arrayN),
				)},
			})
		}
		body = append(body, forLoop, closingCheck,
			ifStmt(
				binop(ivar, token.NEQ, intLit(arrayN)),
				retResultIErrExpr(arrayLenErrExpr(f.JSONName, arrayN, ivar)),
			),
			inc(pv),
		)
		return []ast.Stmt{block(body...)}
	}

	// Slice path: optional null peek + bracketed object.
	sCap, slCap := preallocCap(f)
	nonNull := []ast.Stmt{bracketCheck, inc(pv)}
	nonNull = append(nonNull, inlineSkipWSStmts(posVar)...)
	if f.ElemPointer {
		nonNull = append(nonNull, varDeclExpr(slabVar.Name, &ast.ArrayType{Elt: parseExpr(f.ElemType)}))
	}
	emptyStmts := []ast.Stmt{assign(dst, &ast.CompositeLit{Type: parseExpr(f.GoType)})}
	var nonEmptyStmts []ast.Stmt
	if sCap > 0 {
		nonEmptyStmts = []ast.Stmt{assign(dst,
			call(id("make"), parseExpr(f.GoType), intLit(0), intLit(sCap)),
		)}
	} else {
		nonEmptyStmts = []ast.Stmt{assign(dst, &ast.CompositeLit{Type: parseExpr(f.GoType)})}
	}
	if f.ElemPointer {
		nonEmptyStmts = append(nonEmptyStmts, assign(slabVar,
			call(id("make"), &ast.ArrayType{Elt: parseExpr(f.ElemType)}, intLit(0), intLit(slCap)),
		))
	}
	nonNull = append(nonNull, ifElse(
		land(binop(pv, token.LSS, idCall("len", id("data"))), binop(dataAt, token.EQL, charLit(']'))),
		emptyStmts, nonEmptyStmts,
	))
	nonNull = append(nonNull, forLoop, closingCheck, inc(pv))

	body := inlineSkipWSStmts(posVar)
	body = append(body, inlineNullPeekStmts(posVar, nil, nonNull))
	return []ast.Stmt{block(body...)}
}

// elementLoopStmts builds the per-iteration body for emitByteSliceRead.
// On entry posVar points at the next element (or `]`); on exit it has
// consumed exactly that element. Handles ElemPointer null fast-path,
// pre-grow append for slice cases, ElemKind dispatch, dive mods/
// validation, and the ',' / `break` end-of-iteration choice.
func elementLoopStmts(f FieldInfo, dst ast.Expr, posVar string, depth int, ivar, slabVar *ast.Ident) []ast.Stmt {
	isArray := f.Kind == KindArray
	arrayN := f.ArrayLen
	pv := id(posVar)
	dataAt := index(id("data"), pv)
	directStruct := f.ElemKind == KindStruct && isGenerated(f.ElemType)

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
			ptrNullBody = append(ptrNullBody, assign(index(dst, ivar), id("nil")), inc(ivar))
		} else {
			ptrNullBody = append(ptrNullBody, assign(dst, call(id("append"), dst, id("nil"))))
		}
		ptrNullBody = append(ptrNullBody, inlineSkipWSStmts(posVar)...)
		ptrContArm := []ast.Stmt{inc(pv)}
		ptrContArm = append(ptrContArm, inlineSkipWSStmts(posVar)...)
		ptrContArm = append(ptrContArm, &ast.BranchStmt{Tok: token.CONTINUE})
		ptrNullBody = append(ptrNullBody, ifStmt(
			land(binop(pv, token.LSS, idCall("len", id("data"))), binop(dataAt, token.EQL, charLit(','))),
			ptrContArm...,
		), &ast.BranchStmt{Tok: token.BREAK})
		loop = append(loop, inlineNullPeekStmts(posVar, ptrNullBody, nil))
	}

	var target ast.Expr
	switch {
	case isArray && f.ElemPointer:
		target = index(slabVar, ivar)
	case isArray:
		target = index(dst, ivar)
	case f.ElemPointer:
		loop = append(loop, assign(slabVar, call(id("append"), slabVar, zeroLitExpr(f.ElemType, f.ElemKind))))
		target = index(slabVar, binop(idCall("len", slabVar), token.SUB, intLit(1)))
	default:
		loop = append(loop, assign(dst, call(id("append"), dst, zeroLitExpr(f.ElemType, f.ElemKind))))
		target = index(dst, binop(idCall("len", dst), token.SUB, intLit(1)))
	}

	switch f.ElemKind {
	case KindString:
		loop = append(loop, inlineScanStringStmts(posVar, target, posVar)...)
	case KindBool:
		loop = append(loop,
			assignN([]ast.Expr{target, pv, id("err")}, call(idSel("scan", "Bool"), id("data"), pv)),
			ifErrReturn(),
		)
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		castFn := ""
		if f.ElemType != "int64" {
			castFn = f.ElemType
		}
		loop = append(loop, inlineScanInt64Stmts(posVar, target, castFn)...)
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		castFn := ""
		if f.ElemType != "uint64" {
			castFn = f.ElemType
		}
		loop = append(loop, inlineScanUint64Stmts(posVar, target, castFn)...)
	case KindFloat32, KindFloat64:
		if f.ElemKind == KindFloat64 {
			loop = append(loop,
				assignN([]ast.Expr{target, pv, id("err")}, call(idSel("scan", "Float64"), id("data"), pv)),
				ifErrReturn(),
			)
		} else {
			loop = append(loop,
				varDecl("fv", "float64"),
				assignN([]ast.Expr{id("fv"), pv, id("err")}, call(idSel("scan", "Float64"), id("data"), pv)),
				ifErrReturn(),
				assign(target, call(id("float32"), id("fv"))),
			)
		}
	case KindStruct:
		if directStruct {
			loop = append(loop,
				assignN(
					[]ast.Expr{target, pv, id("err")},
					call(sel(target, "DecodeFrom"), id("data"), pv),
				),
				ifErrReturn(),
			)
		} else {
			loop = append(loop,
				assignN([]ast.Expr{pv, id("err")}, call(idSel("scan", "SkipValue"), id("data"), pv)),
				ifErrReturn(),
			)
		}
	case KindSlice, KindArray:
		loop = append(loop, emitByteSliceReadStmts(peelSliceField(f), target, posVar, depth+1)...)
	}

	if len(f.ElemMods) > 0 {
		loop = append(loop, renderModsStmts(f.ElemMods, target, f.ElemType, f.ElemKind)...)
	}
	if len(f.ElemValidation) > 0 {
		loop = append(loop, renderValidationOnStmts(f.ElemValidation, target, f.JSONName+"[]", f.ElemKind, f.MultiErr, "i")...)
	}

	switch {
	case isArray && f.ElemPointer:
		loop = append(loop,
			assign(index(dst, ivar), addr(index(slabVar, ivar))),
			inc(ivar),
		)
	case isArray:
		loop = append(loop, inc(ivar))
	case f.ElemPointer:
		loop = append(loop, assign(dst,
			call(id("append"), dst, addr(index(slabVar, binop(idCall("len", slabVar), token.SUB, intLit(1))))),
		))
	default:
		// dst tail already decoded in-place via append+index above.
	}

	loop = append(loop, inlineSkipWSStmts(posVar)...)
	contArm := []ast.Stmt{inc(pv)}
	contArm = append(contArm, inlineSkipWSStmts(posVar)...)
	contArm = append(contArm, &ast.BranchStmt{Tok: token.CONTINUE})
	loop = append(loop,
		ifStmt(
			land(binop(pv, token.LSS, idCall("len", id("data"))), binop(dataAt, token.EQL, charLit(','))),
			contArm...,
		),
		&ast.BranchStmt{Tok: token.BREAK},
	)
	return loop
}

// zeroLitExpr returns the AST expr for zeroLit's text result. Used as
// the append-pre-grow placeholder before in-place decode overwrites the
// slot.
func zeroLitExpr(elemType string, kind TypeKind) ast.Expr {
	return parseExpr(zeroLit(elemType, kind))
}
