// Package ts renders gen shapes as TypeScript type declarations.
package ts

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/sirkostya009/ggen/gen"
	"github.com/sirkostya009/ggen/gen/internal/js"
)

// Option configures an Emitter.
type Option func(*Emitter)

// ImportExt appends ext to every import specifier, e.g. ".js" for NodeNext
// module resolution.
func ImportExt(ext string) Option { return func(e *Emitter) { e.importExt = ext } }

// Header writes line at the top of every file, e.g. a generated-code marker.
func Header(line string) Option { return HeaderFor(func(*gen.File) string { return line }) }

// HeaderFor decides the first line per output file; "" writes none.
func HeaderFor(fn func(f *gen.File) string) Option { return func(e *Emitter) { e.header = fn } }

// Indent sets one indentation level. Default two spaces.
func Indent(s string) Option { return func(e *Emitter) { e.Printer.Indent = s } }

// External renders the Go type goPath ("github.com/google/uuid.UUID") as
// tsType wherever a shape references it.
func External(goPath, tsType string) Option {
	return func(e *Emitter) { e.Printer.External[goPath] = tsType }
}

// ExternalFor renders an external type t, used in placed type p, as tsType
// when fn returns ok, before External. Like every ...For option, fn sees the
// output file (f.Path), the placed name, and the Go type through
// p.Shape.Decl: its package, source file, name and directives.
func ExternalFor(fn func(f *gen.File, p gen.Placed, t *gen.Type) (tsType string, ok bool)) Option {
	return func(e *Emitter) { e.externalFor = fn }
}

// TypeAliases declares objects with `type X = {…}` instead of `interface X`.
func TypeAliases() Option {
	return TypeAliasesFor(func(*gen.File, gen.Placed) bool { return true })
}

// TypeAliasesFor decides per placed type whether its object is a type alias.
func TypeAliasesFor(fn func(f *gen.File, p gen.Placed) bool) Option {
	return func(e *Emitter) { e.aliases = fn }
}

// Emitter renders TypeScript type declarations.
type Emitter struct {
	Printer     Printer
	importExt   string
	header      func(*gen.File) string
	aliases     func(*gen.File, gen.Placed) bool
	externalFor func(*gen.File, gen.Placed, *gen.Type) (string, bool)

	placed  gen.Placed
	imports Imports
	written bool
}

// New returns a TypeScript types emitter.
func New(opts ...Option) *Emitter {
	e := &Emitter{
		Printer: Printer{Indent: "  ", External: map[string]string{}},
		header:  func(*gen.File) string { return "" },
		aliases: func(*gen.File, gen.Placed) bool { return false },
	}
	e.Printer.Ref = func(ctx *gen.Context, d *gen.TypeDecl, mode gen.Mode, name string) string {
		return e.imports.TypeName(ctx, d, mode, name)
	}
	e.Printer.ExternalFor = func(ctx *gen.Context, t *gen.Type) (string, bool) {
		if e.externalFor == nil {
			return "", false
		}
		return e.externalFor(ctx.File, e.placed, t)
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

func (e *Emitter) Begin(ctx *gen.Context) error {
	e.imports.Reset(ctx)
	e.written = false
	if h := e.header(ctx.File); h != "" {
		ctx.W.Header(h)
	}
	return nil
}

func (e *Emitter) Decl(ctx *gen.Context, p gen.Placed) error {
	if !js.IsIdent(p.Name) {
		return fmt.Errorf("%q is not a valid TypeScript type name", p.Name)
	}
	if e.written {
		ctx.W.WriteString("\n")
	}
	e.written = true
	e.placed = p
	s := p.Shape
	ctx.W.WriteString(js.Doc(s.Decl.Doc, ""))
	switch {
	case s.Alias != nil:
		ctx.W.Printf("export type %s = %s;\n", p.Name, e.Printer.Type(ctx, s, s.Alias))
	case e.aliases(ctx.File, p) || (s.Unknown == gen.Collect && s.Rest != nil):
		ctx.W.Printf("export type %s = %s;\n", p.Name, e.Printer.Object(ctx, s))
	default:
		ctx.W.Printf("export interface %s %s\n", p.Name, e.Printer.Object(ctx, s))
	}
	return nil
}

func (e *Emitter) End(ctx *gen.Context) error {
	e.imports.Write(ctx, "import type", e.importExt)
	return nil
}

// Printer renders gen types as TypeScript types. Emitters of other
// TypeScript-based targets use it for the types they have to spell out.
type Printer struct {
	Indent   string
	External map[string]string // Go path → TypeScript type
	// ExternalFor, when set, maps an external type before External.
	ExternalFor func(ctx *gen.Context, t *gen.Type) (string, bool)
	// Quiet suppresses the report of unmapped external types, for an emitter
	// that reports them itself.
	Quiet bool
	// Ref returns the name to write for a reference to a placed shape.
	Ref func(ctx *gen.Context, d *gen.TypeDecl, mode gen.Mode, name string) string
}

// Object renders a struct shape's object type: `{ … }`, intersected with a
// Record when the shape collects unknown keys.
func (p *Printer) Object(ctx *gen.Context, s *gen.Shape) string {
	var b strings.Builder
	b.WriteString("{\n")
	for _, f := range s.Fields {
		b.WriteString(js.Doc(f.Doc, p.Indent))
		b.WriteString(p.Indent)
		b.WriteString(js.PropKey(f.JSONName))
		if f.Optional {
			b.WriteByte('?')
		}
		b.WriteString(": ")
		b.WriteString(p.Type(ctx, s, f.Type))
		b.WriteString(";\n")
	}
	b.WriteByte('}')
	if s.Unknown == gen.Collect && s.Rest != nil {
		// Every declared field's type joins the index signature: without it no
		// object literal can satisfy both, since TypeScript checks a declared
		// property against the signature.
		var parts []string
		add := func(ts string) {
			for _, one := range splitUnion(ts) {
				if !slices.Contains(parts, one) {
					parts = append(parts, one)
				}
			}
		}
		add(p.Type(ctx, s, s.Rest))
		for _, f := range s.Fields {
			add(p.Type(ctx, s, f.Type))
		}
		value := strings.Join(parts, " | ")
		if slices.Contains(parts, "unknown") {
			value = "unknown" // a union with unknown is unknown
		}
		b.WriteString(" & Record<string, ")
		b.WriteString(value)
		b.WriteByte('>')
	}
	return b.String()
}

// splitUnion splits a rendered type at its top-level members, leaving a `|`
// inside brackets or a string literal where it is.
func splitUnion(t string) []string {
	var parts []string
	depth, start, quoted := 0, 0, false
	for i := 0; i < len(t); i++ {
		switch c := t[i]; {
		case quoted:
			switch c {
			case '\\':
				i++
			case '"':
				quoted = false
			}
		case c == '"':
			quoted = true
		case c == '(' || c == '[' || c == '{' || c == '<':
			depth++
		case c == ')' || c == ']' || c == '}' || c == '>':
			depth--
		case c == '|' && depth == 0:
			parts = append(parts, strings.TrimSpace(t[start:i]))
			start = i + 1
		}
	}
	return append(parts, strings.TrimSpace(t[start:]))
}

// Type renders t, a type inside shape s.
func (p *Printer) Type(ctx *gen.Context, s *gen.Shape, t *gen.Type) string {
	out := p.base(ctx, s, t)
	if len(t.OneOf) > 0 {
		parts := []string{out}
		for _, o := range t.OneOf {
			if r := p.Type(ctx, s, o); !slices.Contains(parts, r) {
				parts = append(parts, r)
			}
		}
		out = strings.Join(parts, " | ")
	}
	if t.Enum != nil && t.Enum.Zero {
		out += " | " + ZeroLiteral(t)
	}
	if t.Nullable && t.Wire != gen.WireAny {
		out += " | null"
	}
	return out
}

// ZeroLiteral is the JSON literal of an enum type's Go zero value.
func ZeroLiteral(t *gen.Type) string {
	if t.Wire == gen.WireString {
		return `""`
	}
	return "0"
}

func (p *Printer) base(ctx *gen.Context, s *gen.Shape, t *gen.Type) string {
	if t.External {
		if p.ExternalFor != nil {
			if ts, ok := p.ExternalFor(ctx, t); ok {
				return ts
			}
		}
		if ts, ok := p.External[t.Pkg.Path+"."+t.Name]; ok {
			return ts
		}
		if t.Wire != gen.WireString && !p.Quiet {
			ctx.Report.Add(DeclName(s), t.GoType+" is external with an unknown shape; map it with an External or ExternalFor option")
		}
	}
	if t.Ref != nil {
		if pl, ok := ctx.Lookup(t.Ref, s.Mode); ok {
			return p.Ref(ctx, t.Ref, s.Mode, pl.Name)
		}
	}
	if t.Enum != nil {
		vals := make([]string, len(t.Enum.Values))
		for i, v := range t.Enum.Values {
			vals[i] = v.Value
		}
		return strings.Join(vals, " | ")
	}
	//exhaustive:ignore not every kind applies here
	switch t.Wire {
	case gen.WireString:
		return "string"
	case gen.WireInteger, gen.WireNumber:
		return "number"
	case gen.WireBool:
		return "boolean"
	case gen.WireArray:
		elem := p.Type(ctx, s, t.Elem)
		if t.Go == gen.Array || (t.Go == gen.Bytes && t.Len > 0) {
			parts := make([]string, t.Len)
			for i := range parts {
				parts[i] = elem
			}
			return "[" + strings.Join(parts, ", ") + "]"
		}
		return js.Paren(elem) + "[]"
	case gen.WireObject:
		if t.Go == gen.Map {
			return "Record<string, " + p.Type(ctx, s, t.Elem) + ">"
		}
		return "Record<string, unknown>"
	}
	return "unknown"
}

// DeclName is "pkg.Type", the prefix report entries use.
func DeclName(s *gen.Shape) string { return s.Decl.Pkg.Name + "." + s.Decl.Name }

// Imports plans the names a file imports from its sibling files.
type Imports struct {
	byFile map[*gen.File]map[string]imported // file → exported name → how it is imported
	locals map[string]struct{}
}

// imported is one name a file imports.
type imported struct {
	local    string
	typeOnly bool // a type declaration has no value binding to import
}

// Reset starts a file, reserving the names placed in it.
func (im *Imports) Reset(ctx *gen.Context) {
	im.byFile = map[*gen.File]map[string]imported{}
	im.locals = map[string]struct{}{}
	for _, p := range ctx.File.Placed() {
		im.locals[p.Name] = struct{}{}
	}
}

// Reserve marks a local name as taken.
func (im *Imports) Reserve(name string) { im.locals[name] = struct{}{} }

// Name returns the local name for exported name of the file d is placed in,
// planning a value import when that is another file. A name that clashes with
// one already in scope is imported as `<file stem>_<name>`, then with a
// number until it is free.
func (im *Imports) Name(ctx *gen.Context, d *gen.TypeDecl, mode gen.Mode, name string) string {
	return im.name(ctx, d, mode, name, false)
}

// TypeName is Name for a type declaration, which is imported with `type` so
// the specifier survives type stripping and isolated modules.
func (im *Imports) TypeName(ctx *gen.Context, d *gen.TypeDecl, mode gen.Mode, name string) string {
	return im.name(ctx, d, mode, name, true)
}

func (im *Imports) name(ctx *gen.Context, d *gen.TypeDecl, mode gen.Mode, name string, typeOnly bool) string {
	f := ctx.FileOf(d, mode)
	if f == ctx.File {
		return name
	}
	names := im.byFile[f]
	if names == nil {
		names = map[string]imported{}
		im.byFile[f] = names
	}
	if prev, ok := names[name]; ok {
		// A name needed as a value anywhere is imported as a value.
		if prev.typeOnly && !typeOnly {
			prev.typeOnly = false
			names[name] = prev
		}
		return prev.local
	}
	local := name
	if _, clash := im.locals[local]; clash {
		local = js.Ident(js.FileStem(f.Path)) + "_" + name
		for i := 2; ; i++ {
			if _, clash := im.locals[local]; !clash {
				break
			}
			local = js.Ident(js.FileStem(f.Path)) + strconv.Itoa(i) + "_" + name
		}
	}
	names[name] = imported{local, typeOnly}
	im.locals[local] = struct{}{}
	return local
}

// Write adds one `<keyword> { … } from "…";` line per imported file. Under a
// plain `import`, a name that is only a type is marked `type`, which a
// type-stripping runtime needs and which `verbatimModuleSyntax` requires.
func (im *Imports) Write(ctx *gen.Context, keyword, ext string) {
	files := make([]*gen.File, 0, len(im.byFile))
	for f := range im.byFile {
		files = append(files, f)
	}
	slices.SortFunc(files, func(a, b *gen.File) int { return strings.Compare(a.Path, b.Path) })
	typeImport := strings.Contains(keyword, "type")
	for _, f := range files {
		names := make([]string, 0, len(im.byFile[f]))
		for name, imp := range im.byFile[f] {
			spec := name
			if name != imp.local {
				spec = name + " as " + imp.local
			}
			if imp.typeOnly && !typeImport {
				spec = "type " + spec
			}
			names = append(names, spec)
		}
		slices.Sort(names)
		ctx.W.Import(keyword + " { " + strings.Join(names, ", ") + " } from " + js.Quote(ctx.Rel(f)+ext) + ";")
	}
}

// Runtime returns the emitter of a shared runtime file: every helper the
// schema emitters use, exported once. Pass the file to zod.Runtime or
// valibot.Runtime so their files import helpers from it instead of inlining
// them. header, when not empty, is the file's first line.
func Runtime(header string) gen.Emitter { return runtime{header} }

type runtime struct{ header string }

func (r runtime) Begin(ctx *gen.Context) error {
	if r.header != "" {
		ctx.W.Header(r.header)
	}
	return nil
}

func (runtime) Decl(*gen.Context, gen.Placed) error {
	return errors.New("a runtime file holds no types")
}

func (runtime) End(ctx *gen.Context) error {
	ctx.W.WriteString(strings.TrimSuffix(js.Runtime(), "\n"))
	return nil
}
