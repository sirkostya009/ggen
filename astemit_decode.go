package main

// AST emitters for the top-level byte-path decoder: function body of
// DecodeFrom (renderDecodeBodyStmts), the per-field length-first key
// dispatch (renderDispatchStmts), the post-loop required-field / multierr
// flush (renderPostLoopStmts), and the small seen-bit helpers used at
// both call sites.

import (
	"go/ast"
	"go/token"
	"slices"
)

// decodeFromFuncDecl assembles the full *ast.FuncDecl for DecodeFrom on
// a non-alias struct. Signature: `func (T) DecodeFrom(data []byte, i int)
// (T, int, error)`. Body is renderDecodeBodyStmts(s). Alias structs go
// through the text path (alias.go) — caller checks s.IsAlias before
// using this builder.
func decodeFromFuncDecl(s StructInfo) *ast.FuncDecl {
	params := []*ast.Field{
		param("data", &ast.ArrayType{Elt: id("byte")}),
		param("i", id("int")),
	}
	results := []*ast.Field{
		result(id(s.Name)),
		result(id("int")),
		result(id("error")),
	}
	var body []ast.Stmt
	if s.IsAlias {
		body = renderAliasDecodeStmts(s)
	} else {
		body = renderDecodeBodyStmts(s)
	}
	return funcDecl(
		id(s.Name), "", "DecodeFrom",
		params, results,
		blockOf(body),
	)
}

// renderDecodeBodyStmts produces the function body for DecodeFrom on a
// struct (non-alias). Mirror of renderDecode's text-emit branch from
// "var result T" through the closing trailer return.
func renderDecodeBodyStmts(s StructInfo) []ast.Stmt {
	body := []ast.Stmt{
		varDecl("result", s.Name),
		varDecl("err", "error"),
		assign(id("_"), id("err")),
	}
	if s.MultiErr {
		body = append(body, varDeclExpr("errs", idSel("validation", "Errors")))
	}
	body = append(body, seenInitStmts(s)...)

	body = append(body, inlineSkipWSStmts("i")...)
	body = append(body,
		ifStmt(
			lor(
				binop(id("i"), token.GEQ, idCall("len", id("data"))),
				binop(index(id("data"), id("i")), token.NEQ, charLit('{')),
			),
			retResultIErrExpr(idSel("scan", "ErrBadObject")),
		),
		inc(id("i")),
	)
	body = append(body, inlineSkipWSStmts("i")...)

	// Empty object fast path.
	emptyBranch := renderPostLoopStmts(s)
	emptyBranch = append(emptyBranch, retStmt(
		id("result"), binop(id("i"), token.ADD, intLit(1)), id("nil"),
	))
	body = append(body, ifStmt(
		land(
			binop(id("i"), token.LSS, idCall("len", id("data"))),
			binop(index(id("data"), id("i")), token.EQL, charLit('}')),
		),
		emptyBranch...,
	))

	// for { var key; scan; ':'; dispatch; ','|'}' }
	loopBody := []ast.Stmt{varDecl("key", "string")}
	loopBody = append(loopBody, inlineScanStringStmts("i", id("key"), "i")...)
	loopBody = append(loopBody, inlineSkipWSStmts("i")...)
	loopBody = append(loopBody,
		ifStmt(
			lor(
				binop(id("i"), token.GEQ, idCall("len", id("data"))),
				binop(index(id("data"), id("i")), token.NEQ, charLit(':')),
			),
			retResultIErrExpr(idSel("scan", "ErrBadObject")),
		),
		inc(id("i")),
	)
	loopBody = append(loopBody, inlineSkipWSStmts("i")...)
	loopBody = append(loopBody, renderDispatchStmts(s)...)
	loopBody = append(loopBody, inlineSkipWSStmts("i")...)
	loopBody = append(loopBody, ifStmt(
		binop(id("i"), token.GEQ, idCall("len", id("data"))),
		retResultIErrExpr(idSel("scan", "ErrBadObject")),
	))

	commaArm := []ast.Stmt{inc(id("i"))}
	commaArm = append(commaArm, inlineSkipWSStmts("i")...)
	commaArm = append(commaArm, &ast.BranchStmt{Tok: token.CONTINUE})
	loopBody = append(loopBody, ifStmt(
		binop(index(id("data"), id("i")), token.EQL, charLit(',')),
		commaArm...,
	))

	closeArm := renderPostLoopStmts(s)
	closeArm = append(closeArm, retStmt(
		id("result"), binop(id("i"), token.ADD, intLit(1)), id("nil"),
	))
	loopBody = append(loopBody, ifStmt(
		binop(index(id("data"), id("i")), token.EQL, charLit('}')),
		closeArm...,
	))

	loopBody = append(loopBody, retResultIErrExpr(idSel("scan", "ErrBadObject")))

	body = append(body, forInfinite(loopBody...))
	return body
}

// renderPostLoopStmts emits end-of-parse bookkeeping for the byte path:
// required-field presence checks, then the multierr flush. Skipped for
// novalidate.
func renderPostLoopStmts(s StructInfo) []ast.Stmt {
	var out []ast.Stmt
	if !s.NoValidate {
		for _, f := range s.Fields {
			if !f.IsRequired() || f.Inline {
				continue
			}
			err := requiredErrLit(f.JSONName)
			notSeen := seenNotAccessExpr(s, f)
			if s.MultiErr {
				out = append(out, ifStmt(notSeen,
					assign(id("errs"), call(id("append"), id("errs"), err)),
				))
			} else {
				out = append(out, ifStmt(notSeen, retResultIErrExpr(err)))
			}
		}
	}
	if s.MultiErr {
		out = append(out, ifStmt(
			binop(idCall("len", id("errs")), token.GTR, intLit(0)),
			retStmt(id("result"), id("i"), id("errs")),
		))
	}
	return out
}

// renderDispatchStmts emits a length-first switch on len(key). Single-
// field lengths collapse to an if/else against the JSON name; multi-
// field lengths emit a nested switch on key.
func renderDispatchStmts(s StructInfo) []ast.Stmt {
	byLen := make(map[int][]FieldInfo, len(s.Fields))
	lens := make([]int, 0, len(s.Fields))
	for _, f := range s.Fields {
		if f.Inline {
			continue
		}
		n := len(f.JSONName)
		if _, seen := byLen[n]; !seen {
			lens = append(lens, n)
		}
		byLen[n] = append(byLen[n], f)
	}
	slices.Sort(lens)

	var cases []ast.Stmt
	for _, n := range lens {
		fs := byLen[n]
		var caseBody []ast.Stmt
		if len(fs) == 1 {
			f := fs[0]
			caseBody = []ast.Stmt{ifElse(
				binop(id("key"), token.EQL, strLit(f.JSONName)),
				emitFieldDispatchStmts(s, f),
				unknownKeyStmts(s, "i"),
			)}
		} else {
			innerCases := make([]ast.Stmt, 0, len(fs)+1)
			for _, f := range fs {
				innerCases = append(innerCases, &ast.CaseClause{
					List: []ast.Expr{strLit(f.JSONName)},
					Body: emitFieldDispatchStmts(s, f),
				})
			}
			innerCases = append(innerCases, &ast.CaseClause{
				List: nil,
				Body: unknownKeyStmts(s, "i"),
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
		Body: unknownKeyStmts(s, "i"),
	})

	return []ast.Stmt{&ast.SwitchStmt{
		Tag:  idCall("len", id("key")),
		Body: blockOf(cases),
	}}
}

// emitFieldDispatchStmts wraps a single field's decode with seen-tracking
// and the duplicate-key handling chosen by the struct's AllowDups /
// MultiErr settings.
func emitFieldDispatchStmts(s StructInfo, f FieldInfo) []ast.Stmt {
	ref := parseExpr("result." + f.GoName)
	if f.Inline || !needsSeen(f) {
		return renderFieldStmts(f, ref, "i")
	}
	set := seenSetStmts(s, f)
	seen := seenAccessExpr(s, f)
	fieldBody := renderFieldStmts(f, ref, "i")

	if s.AllowDups {
		// if seen { i, err = SkipValue; if err { return } } else { set; decode }
		dupBody := []ast.Stmt{
			assignN(
				[]ast.Expr{id("i"), id("err")},
				call(idSel("scan", "SkipValue"), id("data"), id("i")),
			),
			ifErrReturn(),
		}
		elseBody := append(append([]ast.Stmt{}, set...), fieldBody...)
		return []ast.Stmt{ifElse(seen, dupBody, elseBody)}
	}
	if s.MultiErr {
		dupBody := []ast.Stmt{
			assign(id("errs"), call(id("append"), id("errs"),
				validationErrLit("DuplicateKeyError", fieldKV("Field", strLit(f.JSONName))),
			)),
			assignN(
				[]ast.Expr{id("i"), id("err")},
				call(idSel("scan", "SkipValue"), id("data"), id("i")),
			),
			ifErrReturn(),
		}
		elseBody := append(append([]ast.Stmt{}, set...), fieldBody...)
		return []ast.Stmt{ifElse(seen, dupBody, elseBody)}
	}
	// Default: hard error on second occurrence.
	out := []ast.Stmt{
		ifStmt(seen, retResultIErrExpr(
			validationErrLit("DuplicateKeyError", fieldKV("Field", strLit(f.JSONName))),
		)),
	}
	out = append(out, set...)
	out = append(out, fieldBody...)
	return out
}

// seenInitStmts emits the per-struct seen-flag declarations: either a
// per-field `seenX := false` (bool mode) or a bitmask `var seen uint64`
// / `var seen [N]uint64` (wide structs).
func seenInitStmts(s StructInfo) []ast.Stmt {
	if useSeenBitmask(s) {
		if seenWordCount(s) == 1 {
			return []ast.Stmt{varDecl("seen", "uint64")}
		}
		arr := &ast.ArrayType{Len: intLit(seenWordCount(s)), Elt: id("uint64")}
		return []ast.Stmt{varDeclExpr("seen", arr)}
	}
	var out []ast.Stmt
	for _, f := range s.Fields {
		if f.Inline {
			continue
		}
		if needsSeen(f) {
			out = append(out, shortDecl(id("seen"+f.GoName), id("false")))
		}
	}
	return out
}

// seenAccessExpr is the AST counterpart of seenAccess — the read
// expression for f's seen bit.
func seenAccessExpr(s StructInfo, f FieldInfo) ast.Expr {
	if !useSeenBitmask(s) {
		return id("seen" + f.GoName)
	}
	bit := seenBitIndex(s, f)
	mask := binop(intLit(1), token.SHL, intLit(bit%64))
	if seenWordCount(s) == 1 {
		return binop(
			binop(id("seen"), token.AND, mask),
			token.NEQ, intLit(0),
		)
	}
	word := index(id("seen"), intLit(bit/64))
	return binop(
		binop(word, token.AND, mask),
		token.NEQ, intLit(0),
	)
}

// seenNotAccessExpr is the AST counterpart of seenNotAccess — true when
// f's seen bit is unset.
func seenNotAccessExpr(s StructInfo, f FieldInfo) ast.Expr {
	if !useSeenBitmask(s) {
		return not(id("seen" + f.GoName))
	}
	bit := seenBitIndex(s, f)
	mask := binop(intLit(1), token.SHL, intLit(bit%64))
	if seenWordCount(s) == 1 {
		return binop(
			binop(id("seen"), token.AND, mask),
			token.EQL, intLit(0),
		)
	}
	word := index(id("seen"), intLit(bit/64))
	return binop(
		binop(word, token.AND, mask),
		token.EQL, intLit(0),
	)
}

// seenSetStmts is the AST counterpart of seenSet — a single statement
// that marks f's seen bit.
func seenSetStmts(s StructInfo, f FieldInfo) []ast.Stmt {
	if !useSeenBitmask(s) {
		return []ast.Stmt{assign(id("seen"+f.GoName), id("true"))}
	}
	bit := seenBitIndex(s, f)
	mask := binop(intLit(1), token.SHL, intLit(bit%64))
	if seenWordCount(s) == 1 {
		return []ast.Stmt{&ast.AssignStmt{
			Lhs: []ast.Expr{id("seen")},
			Tok: token.OR_ASSIGN,
			Rhs: []ast.Expr{mask},
		}}
	}
	return []ast.Stmt{&ast.AssignStmt{
		Lhs: []ast.Expr{index(id("seen"), intLit(bit/64))},
		Tok: token.OR_ASSIGN,
		Rhs: []ast.Expr{mask},
	}}
}
