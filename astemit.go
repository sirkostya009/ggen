package main

// AST construction helpers. The generator builds a `*ast.File` directly and
// writes it via `format.Node` so the intermediate text/format.Source step is
// gone. Helpers here are intentionally tiny — each one corresponds to a
// single AST node shape that recurs in render*. Most of generate.go's emit
// helpers build statement slices via these.
//
// During the staged migration, some render funcs still emit text into a
// bytes.Buffer. `bridgeBody` parses that text into an `*ast.BlockStmt` so
// the resulting func decl can sit alongside hand-built ones in the same
// file. Bridge usage shrinks as renderers are converted.

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"strconv"
)

var astFset = token.NewFileSet()

// astLineFile is a synthetic *token.File registered in astFset, used to
// give hand-built AST nodes monotonic line positions. format.Node looks
// at the line distance between consecutive ImportSpecs / top-level
// decls to decide whether to insert a blank line — without real
// positions, format.Node packs them with no gaps. Allocate generously:
// 1 M lines × 32 B/line = 32 MiB of synthetic source space, plenty of
// room for the per-decl gap-based spacing scheme even on huge codegen
// outputs (hundreds of structs, thousands of methods).
var (
	astLineFile *token.File
	astLineMax  = 1 << 20
)

func init() {
	astLineFile = astFset.AddFile("astemit", -1, astLineMax*32)
	lines := make([]int, astLineMax)
	for i := range lines {
		lines[i] = i * 32
	}
	astLineFile.SetLines(lines)
}

// posAtLine returns a token.Pos at line `n` (1-indexed) in the synthetic
// line file. Used to drive gofmt's line-gap heuristic for import groups.
// Out-of-range lines fall back to NoPos.
func posAtLine(n int) token.Pos {
	if n < 1 || n > astLineMax {
		return token.NoPos
	}
	return astLineFile.LineStart(n)
}

// id makes an *ast.Ident. token.NoPos is fine — format.Node handles it.
func id(name string) *ast.Ident { return ast.NewIdent(name) }

// sel makes an X.Sel expression.
func sel(x ast.Expr, name string) *ast.SelectorExpr {
	return &ast.SelectorExpr{X: x, Sel: id(name)}
}

// idSel makes a SelectorExpr from two strings: a.b. For longer chains use
// chained sel() calls.
func idSel(x, name string) *ast.SelectorExpr { return sel(id(x), name) }

// intLit makes an integer BasicLit.
func intLit(n int) *ast.BasicLit {
	return &ast.BasicLit{Kind: token.INT, Value: strconv.Itoa(n)}
}

// strLit makes a string BasicLit with quoted content.
func strLit(s string) *ast.BasicLit {
	return &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(s)}
}

// charLit makes a CHAR BasicLit like 'x'.
func charLit(c byte) *ast.BasicLit {
	return &ast.BasicLit{Kind: token.CHAR, Value: strconv.QuoteRune(rune(c))}
}

// call makes a CallExpr with positional args.
func call(fn ast.Expr, args ...ast.Expr) *ast.CallExpr {
	return &ast.CallExpr{Fun: fn, Args: args}
}

// idCall is a convenience for call(id(name), args...).
func idCall(name string, args ...ast.Expr) *ast.CallExpr { return call(id(name), args...) }

// index makes X[Index].
func index(x, i ast.Expr) *ast.IndexExpr { return &ast.IndexExpr{X: x, Index: i} }

// slice makes X[low:high].
func slice2(x, low, high ast.Expr) *ast.SliceExpr {
	return &ast.SliceExpr{X: x, Low: low, High: high}
}

// star makes *X (deref expression OR pointer type — same syntax).
func star(x ast.Expr) *ast.StarExpr { return &ast.StarExpr{X: x} }

// addr makes &X.
func addr(x ast.Expr) *ast.UnaryExpr {
	return &ast.UnaryExpr{Op: token.AND, X: x}
}

// not makes !X.
func not(x ast.Expr) *ast.UnaryExpr {
	return &ast.UnaryExpr{Op: token.NOT, X: x}
}

// paren makes (X).
func paren(x ast.Expr) *ast.ParenExpr { return &ast.ParenExpr{X: x} }

// binop makes X op Y.
func binop(x ast.Expr, op token.Token, y ast.Expr) *ast.BinaryExpr {
	return &ast.BinaryExpr{X: x, Op: op, Y: y}
}

// land chains exprs with && (short-circuit and).
func land(xs ...ast.Expr) ast.Expr {
	if len(xs) == 0 {
		return nil
	}
	out := xs[0]
	for _, x := range xs[1:] {
		out = binop(out, token.LAND, x)
	}
	return out
}

// lor chains exprs with || (short-circuit or).
func lor(xs ...ast.Expr) ast.Expr {
	if len(xs) == 0 {
		return nil
	}
	out := xs[0]
	for _, x := range xs[1:] {
		out = binop(out, token.LOR, x)
	}
	return out
}

// assign makes lhs = rhs (one each).
func assign(lhs, rhs ast.Expr) *ast.AssignStmt {
	return &ast.AssignStmt{Lhs: []ast.Expr{lhs}, Tok: token.ASSIGN, Rhs: []ast.Expr{rhs}}
}

// assignN makes lhs... = rhs... for multi-value calls.
func assignN(lhs []ast.Expr, rhs ...ast.Expr) *ast.AssignStmt {
	return &ast.AssignStmt{Lhs: lhs, Tok: token.ASSIGN, Rhs: rhs}
}

// shortDecl makes lhs := rhs.
func shortDecl(lhs, rhs ast.Expr) *ast.AssignStmt {
	return &ast.AssignStmt{Lhs: []ast.Expr{lhs}, Tok: token.DEFINE, Rhs: []ast.Expr{rhs}}
}

// shortDeclN makes lhs... := rhs... (e.g. v, err := f()).
func shortDeclN(lhs []ast.Expr, rhs ...ast.Expr) *ast.AssignStmt {
	return &ast.AssignStmt{Lhs: lhs, Tok: token.DEFINE, Rhs: rhs}
}

// inc makes X++.
func inc(x ast.Expr) *ast.IncDecStmt {
	return &ast.IncDecStmt{X: x, Tok: token.INC}
}

// incBy emits `posVar += n` as an AssignStmt.
func incBy(name string, n int) *ast.AssignStmt {
	return &ast.AssignStmt{Lhs: []ast.Expr{id(name)}, Tok: token.ADD_ASSIGN, Rhs: []ast.Expr{intLit(n)}}
}

// retStmt makes `return results...`.
func retStmt(results ...ast.Expr) *ast.ReturnStmt {
	return &ast.ReturnStmt{Results: results}
}

// block wraps stmts in a BlockStmt.
func block(stmts ...ast.Stmt) *ast.BlockStmt {
	return &ast.BlockStmt{List: stmts}
}

// blockOf is like block but takes a slice (saves variadic spread).
func blockOf(stmts []ast.Stmt) *ast.BlockStmt {
	return &ast.BlockStmt{List: stmts}
}

// nextSpreadLine is the line counter used by spreadBlockPositions to
// hand out distinct lines to FuncDecl bodies. Bumped by ~1000 per
// FuncDecl so each body's stmts can occupy a contiguous range without
// colliding with the next decl.
var nextSpreadLine = 5000

// spreadBlockPositions assigns synthetic line positions to a
// hand-built *ast.BlockStmt body and its top-level statements. format.
// Node compacts single-statement function bodies onto one line when
// the block's Pos and End resolve to the same line; spreading the
// Lbrace, each stmt, and the Rbrace across distinct lines forces the
// multi-line printer rules. Idempotent if Lbrace already has a valid
// position (so calling twice is a no-op).
//
// Only top-level stmts in `b.List` get explicit line positions —
// nested control flow (if/for/switch) gets default printer layout,
// which is correct without per-stmt assignment.
func spreadBlockPositions(b *ast.BlockStmt) {
	if b == nil || b.Lbrace.IsValid() {
		return
	}
	const gap = 1000
	base := nextSpreadLine
	nextSpreadLine += gap
	b.Lbrace = posAtLine(base)
	for i, s := range b.List {
		setStmtPos(s, posAtLine(base+1+i))
	}
	b.Rbrace = posAtLine(base + 1 + len(b.List))
}

// setStmtPos pins the leading position of a top-level statement so
// format.Node sees the body as multi-line. Each stmt type exposes its
// "start" position differently; the switch covers the ones that
// actually appear at the top of FuncDecl bodies.
func setStmtPos(s ast.Stmt, pos token.Pos) {
	if !pos.IsValid() {
		return
	}
	switch s := s.(type) {
	case *ast.AssignStmt:
		s.TokPos = pos
	case *ast.ReturnStmt:
		s.Return = pos
	case *ast.IfStmt:
		s.If = pos
	case *ast.ForStmt:
		s.For = pos
	case *ast.RangeStmt:
		s.For = pos
	case *ast.SwitchStmt:
		s.Switch = pos
	case *ast.BlockStmt:
		s.Lbrace = pos
	case *ast.DeclStmt:
		if gd, ok := s.Decl.(*ast.GenDecl); ok {
			gd.TokPos = pos
		}
	case *ast.ExprStmt:
		// position lives on the expression
	}
}

// ifStmt makes if cond { then... }.
func ifStmt(cond ast.Expr, then ...ast.Stmt) *ast.IfStmt {
	return &ast.IfStmt{Cond: cond, Body: block(then...)}
}

// ifElse makes if cond { then } else { otherwise }.
func ifElse(cond ast.Expr, then, otherwise []ast.Stmt) *ast.IfStmt {
	return &ast.IfStmt{Cond: cond, Body: blockOf(then), Else: blockOf(otherwise)}
}

// forStmt makes a plain `for cond { body... }` (cond-only, no init/post).
func forCond(cond ast.Expr, body ...ast.Stmt) *ast.ForStmt {
	return &ast.ForStmt{Cond: cond, Body: block(body...)}
}

// forLoop makes `for init; cond; post { body... }`.
func forLoop(init ast.Stmt, cond ast.Expr, post ast.Stmt, body ...ast.Stmt) *ast.ForStmt {
	return &ast.ForStmt{Init: init, Cond: cond, Post: post, Body: block(body...)}
}

// forInfinite makes `for { body... }`.
func forInfinite(body ...ast.Stmt) *ast.ForStmt {
	return &ast.ForStmt{Body: block(body...)}
}

// varDecl makes `var name type` (no initializer).
func varDecl(name, typ string) *ast.DeclStmt {
	return &ast.DeclStmt{Decl: &ast.GenDecl{
		Tok:   token.VAR,
		Specs: []ast.Spec{&ast.ValueSpec{Names: []*ast.Ident{id(name)}, Type: typeFromString(typ)}},
	}}
}

// varDeclExpr makes `var name type` where the type is already an ast.Expr.
func varDeclExpr(name string, typ ast.Expr) *ast.DeclStmt {
	return &ast.DeclStmt{Decl: &ast.GenDecl{
		Tok:   token.VAR,
		Specs: []ast.Spec{&ast.ValueSpec{Names: []*ast.Ident{id(name)}, Type: typ}},
	}}
}

// typeFromString parses a Go type expression string ("[]int", "*Foo",
// "map[string]X") into an ast.Expr. Tiny shortcut — these strings are
// already in field.GoType / ElemType / etc., not worth re-deriving by
// hand.
func typeFromString(s string) ast.Expr {
	e, err := parser.ParseExpr(s)
	if err != nil {
		panic(fmt.Sprintf("typeFromString(%q): %v", s, err))
	}
	return e
}

// parseExpr is a public alias for parser.ParseExpr that panics on error.
// Used wherever a known-good Go expression source is more readable than a
// hand-built AST node (e.g. compile-time constants like `len(data)`).
//
// parser.ParseExpr uses its own internal *token.FileSet — the positions
// it stamps onto the returned tree are integers from that fset, NOT
// astFset. When format.Node later looks them up in astFset, those
// integers can coincidentally land inside other registered files
// (parsed method bodies, the synthetic line file) and resolve to
// nonsense line numbers — which the printer's line-gap heuristic then
// interprets as "wrap this binary expression across lines." Strip the
// positions by walking the parsed tree once and zeroing every Pos
// field, so format.Node uses its own default layout instead.
func parseExpr(s string) ast.Expr {
	e, err := parser.ParseExpr(s)
	if err != nil {
		panic(fmt.Sprintf("parseExpr(%q): %v", s, err))
	}
	zeroPositions(e)
	return e
}

// zeroPositions walks an ast.Node and clears every position field it
// can reach. Each ast.Node type has its own set of Pos fields; the
// switch covers the ones produced by parser.ParseExpr (the expression
// node types). format.Node then treats every position as token.NoPos
// and applies its default layout rules instead of source-preserving
// ones.
func zeroPositions(n ast.Node) {
	if n == nil {
		return
	}
	ast.Inspect(n, func(x ast.Node) bool {
		switch x := x.(type) {
		case *ast.Ident:
			x.NamePos = token.NoPos
		case *ast.BasicLit:
			x.ValuePos = token.NoPos
		case *ast.BinaryExpr:
			x.OpPos = token.NoPos
		case *ast.UnaryExpr:
			x.OpPos = token.NoPos
		case *ast.StarExpr:
			x.Star = token.NoPos
		case *ast.ParenExpr:
			x.Lparen = token.NoPos
			x.Rparen = token.NoPos
		case *ast.IndexExpr:
			x.Lbrack = token.NoPos
			x.Rbrack = token.NoPos
		case *ast.SliceExpr:
			x.Lbrack = token.NoPos
			x.Rbrack = token.NoPos
		case *ast.CallExpr:
			x.Lparen = token.NoPos
			x.Rparen = token.NoPos
			// NOTE: do NOT zero x.Ellipsis — its IsValid() bit is what
			// tells the printer to emit `...` for variadic calls. Zeroing
			// it silently strips the spread, turning `append(b, raw...)`
			// into a type-mismatched `append(b, raw)`.
		case *ast.SelectorExpr:
			// Sel is *ast.Ident — handled by the recursion.
		case *ast.CompositeLit:
			x.Lbrace = token.NoPos
			x.Rbrace = token.NoPos
		case *ast.TypeAssertExpr:
			x.Lparen = token.NoPos
			x.Rparen = token.NoPos
		case *ast.KeyValueExpr:
			x.Colon = token.NoPos
		case *ast.ArrayType:
			x.Lbrack = token.NoPos
		case *ast.MapType:
			x.Map = token.NoPos
		case *ast.FuncType:
			x.Func = token.NoPos
		case *ast.ChanType:
			x.Begin = token.NoPos
			x.Arrow = token.NoPos
		case *ast.InterfaceType:
			x.Interface = token.NoPos
		case *ast.StructType:
			x.Struct = token.NoPos

		// Statements
		case *ast.AssignStmt:
			x.TokPos = token.NoPos
		case *ast.ReturnStmt:
			x.Return = token.NoPos
		case *ast.IfStmt:
			x.If = token.NoPos
		case *ast.ForStmt:
			x.For = token.NoPos
		case *ast.RangeStmt:
			x.For = token.NoPos
			x.TokPos = token.NoPos
		case *ast.SwitchStmt:
			x.Switch = token.NoPos
		case *ast.TypeSwitchStmt:
			x.Switch = token.NoPos
		case *ast.CaseClause:
			x.Case = token.NoPos
			x.Colon = token.NoPos
		case *ast.BlockStmt:
			x.Lbrace = token.NoPos
			x.Rbrace = token.NoPos
		case *ast.BranchStmt:
			x.TokPos = token.NoPos
		case *ast.IncDecStmt:
			x.TokPos = token.NoPos
		case *ast.DeferStmt:
			x.Defer = token.NoPos
		case *ast.GoStmt:
			x.Go = token.NoPos
		case *ast.SelectStmt:
			x.Select = token.NoPos
		case *ast.LabeledStmt:
			x.Colon = token.NoPos
		case *ast.SendStmt:
			x.Arrow = token.NoPos
		case *ast.DeclStmt:
			if gd, ok := x.Decl.(*ast.GenDecl); ok {
				gd.TokPos = token.NoPos
				gd.Lparen = token.NoPos
				gd.Rparen = token.NoPos
			}
		case *ast.GenDecl:
			x.TokPos = token.NoPos
			x.Lparen = token.NoPos
			x.Rparen = token.NoPos
		case *ast.ValueSpec:
			// Names/Values handled by recursion.
		}
		return true
	})
}

// funcDecl assembles a method `func (recv recvName) name(params) (results) { body }`.
// `recv` is the receiver type expression (e.g. `id("Foo")` or `star(id("Foo"))`).
// Pass nil recv for a plain function.
func funcDecl(recv ast.Expr, recvName, name string, params, results []*ast.Field, body *ast.BlockStmt) *ast.FuncDecl {
	d := &ast.FuncDecl{
		Name: id(name),
		Type: &ast.FuncType{
			Params:  &ast.FieldList{List: params},
			Results: &ast.FieldList{List: results},
		},
		Body: body,
	}
	if recv != nil {
		recvField := &ast.Field{Type: recv}
		if recvName != "" {
			recvField.Names = []*ast.Ident{id(recvName)}
		}
		d.Recv = &ast.FieldList{List: []*ast.Field{recvField}}
	}
	return d
}

// param makes a named parameter `name type`.
func param(name string, typ ast.Expr) *ast.Field {
	return &ast.Field{Names: []*ast.Ident{id(name)}, Type: typ}
}

// result makes an unnamed result `type`.
func result(typ ast.Expr) *ast.Field { return &ast.Field{Type: typ} }

// writeStmts writes []ast.Stmt as Go source into b. Bridge between
// AST-built renderers and text-emitting parents during the staged
// migration. Each stmt is format.Node'd individually so callers don't
// need to wrap in a BlockStmt.
func writeStmts(b *bytes.Buffer, stmts ...ast.Stmt) {
	for _, s := range stmts {
		if err := format.Node(b, astFset, s); err != nil {
			panic(fmt.Sprintf("writeStmts: %v", err))
		}
		b.WriteByte('\n')
	}
}

// writeExpr writes one expression as Go source into b.
func writeExpr(b *bytes.Buffer, e ast.Expr) {
	if err := format.Node(b, astFset, e); err != nil {
		panic(fmt.Sprintf("writeExpr: %v", err))
	}
}

// exprText prints an ast.Expr to its Go-source representation. Used to
// bridge AST expressions back to the text-receiving render helpers
// (renderSlice / renderMap / emitByteArrayRead / renderCrossPkg*) that
// haven't been converted yet.
func exprText(e ast.Expr) string {
	var buf bytes.Buffer
	writeExpr(&buf, e)
	return buf.String()
}

// cloneExpr produces a fresh ast.Expr equivalent to e by serializing to
// text and re-parsing. Used wherever the same logical expression must
// appear multiple times in the AST — format.Node's position tracking
// gets confused if the same node instance is visited more than once,
// causing weird line breaks in deeply-indented contexts.
func cloneExpr(e ast.Expr) ast.Expr {
	return parseExpr(exprText(e))
}

// retResultIErr returns the canonical `return result, i, err` statement
// used at almost every error exit in generated decode bodies.
func retResultIErr() *ast.ReturnStmt {
	return retStmt(id("result"), id("i"), id("err"))
}

// retResultIErrExpr is like retResultIErr but lets the caller specify
// what's returned in the error slot (e.g. a typed validation error
// literal). Position is always `i`.
func retResultIErrExpr(errExpr ast.Expr) *ast.ReturnStmt {
	return retStmt(id("result"), id("i"), errExpr)
}

// ifErrReturn builds `if err != nil { return result, i, err }`.
func ifErrReturn() *ast.IfStmt {
	return ifStmt(binop(id("err"), token.NEQ, id("nil")), retResultIErr())
}

// unsafeStringAlias builds `unsafe.String(unsafe.SliceData(data[start:]), lenExpr)`.
// The string aliases the data slice starting at `start`; lifetime is the
// caller's input data.
func unsafeStringAlias(start, lenExpr ast.Expr) ast.Expr {
	sliceData := call(idSel("unsafe", "SliceData"),
		slice2(id("data"), start, nil))
	return call(idSel("unsafe", "String"), sliceData, lenExpr)
}
