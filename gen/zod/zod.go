// Package zod renders gen shapes as Zod 4 schemas.
package zod

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/sirkostya009/ggen/gen"
	"github.com/sirkostya009/ggen/gen/internal/js"
	"github.com/sirkostya009/ggen/gen/ts"
)

// Option configures an Emitter.
type Option func(*Emitter)

// ImportExt appends ext to every import specifier, e.g. ".js" for NodeNext
// module resolution.
func ImportExt(ext string) Option { return func(e *Emitter) { e.importExt = ext } }

// Header writes line at the top of every file, e.g. a generated-code marker.
func Header(line string) Option { return HeaderFor(func(*gen.File) string { return line }) }

// HeaderFor decides the first line per output file. Returning "" writes no
// header.
func HeaderFor(fn func(f *gen.File) string) Option { return func(e *Emitter) { e.header = fn } }

// Func appends action to the schema of every value checked or transformed by
// the Go function reference, as written in the tag without "@": "IsAdult" or
// "pkg.IsAdult". action is a method chain, e.g. `.refine((x) => x >= 18)`.
// The reference may also be the function's import path and name,
// "example.com/rules.IsAdult", when two packages share a function name.
func Func(ref, action string) Option { return func(e *Emitter) { e.funcs[ref] = action } }

// FuncFor decides the action for an @Func rule per placed type, before Func.
// Like every ...For option, fn sees the output file (f.Path), the placed name,
// and the Go type through p.Shape.Decl: its package, source file, name and
// directives.
func FuncFor(fn func(f *gen.File, p gen.Placed, r gen.Rule) (action string, ok bool)) Option {
	return func(e *Emitter) { e.funcFor = fn }
}

// External renders the Go type goPath ("github.com/google/uuid.UUID") as
// schema, e.g. "z.uuid()".
func External(goPath, schema string) Option {
	return func(e *Emitter) { e.external[goPath] = schema }
}

// ExternalFor renders an external type t, used in placed type p, as schema
// when fn returns ok, before External.
func ExternalFor(fn func(f *gen.File, p gen.Placed, t *gen.Type) (schema string, ok bool)) Option {
	return func(e *Emitter) { e.externalFor = fn }
}

// ExternalType sets the TypeScript type of an external Go type, which a
// recursive shape needs for its z.ZodType annotation. Without it the
// annotation follows the type's wire shape.
func ExternalType(goPath, tsType string) Option {
	return func(e *Emitter) { e.types.External[goPath] = tsType }
}

// Runtime imports helpers from f, a file created with ts.Runtime, instead of
// inlining them into every file.
func Runtime(f *gen.File) Option { return func(e *Emitter) { e.runtime = f } }

// SchemaName derives the name of the schema constant for placed type p in
// file f. The placed name stays the type's name. The default schema name is
// the placed name.
func SchemaName(fn func(f *gen.File, p gen.Placed) string) Option {
	return func(e *Emitter) { e.schemaName = fn }
}

// Override renders a type itself when fn returns ok, before any other rule.
// For example, it can render int64 fields as bigint.
func Override(fn func(ctx *gen.Context, p gen.Placed, t *gen.Type) (schema string, ok bool)) Option {
	return func(e *Emitter) { e.override = fn }
}

// Emitter renders Zod 4 schemas: an exported schema constant and its inferred
// type per placed shape.
type Emitter struct {
	importExt   string
	header      func(*gen.File) string
	runtime     *gen.File
	funcs       map[string]string
	funcFor     func(*gen.File, gen.Placed, gen.Rule) (string, bool)
	external    map[string]string
	externalFor func(*gen.File, gen.Placed, *gen.Type) (string, bool)
	schemaName  func(*gen.File, gen.Placed) string
	override    func(*gen.Context, gen.Placed, *gen.Type) (string, bool)

	placed  gen.Placed // the shape Decl is rendering
	forward bool       // the field being rendered references a type this file emits later
	cyclic  bool       // the shape being rendered is on a cycle with another type

	types   ts.Printer
	imports ts.Imports
	helpers js.Helpers
	body    strings.Builder
}

// New returns a Zod 4 emitter.
func New(opts ...Option) *Emitter {
	e := &Emitter{
		funcs:      map[string]string{},
		external:   map[string]string{},
		header:     func(*gen.File) string { return "" },
		schemaName: func(_ *gen.File, p gen.Placed) string { return p.Name },
		types:      ts.Printer{Indent: "  ", External: map[string]string{}, Quiet: true},
	}
	e.types.Ref = func(ctx *gen.Context, d *gen.TypeDecl, mode gen.Mode, name string) string {
		return e.imports.TypeName(ctx, d, mode, name)
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

func (e *Emitter) Begin(ctx *gen.Context) error {
	e.imports.Reset(ctx)
	for _, p := range ctx.File.Placed() {
		e.imports.Reserve(e.schemaName(ctx.File, p))
	}
	e.helpers = js.Helpers{}
	e.body.Reset()
	if h := e.header(ctx.File); h != "" {
		ctx.W.Header(h)
	}
	ctx.W.Import(`import { z } from "zod";`)
	return nil
}

func (e *Emitter) Decl(ctx *gen.Context, p gen.Placed) error {
	e.placed = p
	// Neither an alias nor a catch-all has a getter to defer a reference to
	// itself through.
	e.cyclic = ctx.Cyclic(p.Shape.Decl, p.Shape.Mode) || p.Shape.Recursive && (p.Shape.Alias != nil || p.Shape.Rest != nil)
	constName := e.schemaName(ctx.File, p)
	for _, n := range []string{p.Name, constName} {
		if !js.IsIdent(n) {
			return fmt.Errorf("%q is not a valid TypeScript name", n)
		}
	}
	s := p.Shape
	var expr string
	if s.Alias != nil {
		expr = e.typ(ctx, s, s.Alias, ts.DeclName(s))
	} else {
		expr = e.object(ctx, s)
	}
	if e.body.Len() > 0 {
		e.body.WriteByte('\n')
	}
	e.body.WriteString(js.Doc(s.Decl.Doc, ""))
	if s.Recursive || e.cyclic {
		// TypeScript cannot infer a schema that references itself once the
		// reference is wrapped more than once (z.array(X.nullable())), nor two
		// schemas that reference each other, so the type is written out.
		var tsType string
		if s.Alias != nil {
			tsType = e.types.Type(ctx, s, s.Alias)
		} else {
			tsType = e.types.Object(ctx, s)
		}
		fmt.Fprintf(&e.body, "export type %s = %s;\nexport const %s: z.ZodType<%s> = %s;\n", p.Name, tsType, constName, p.Name, expr)
		return nil
	}
	fmt.Fprintf(&e.body, "export const %s = %s;\nexport type %s = z.output<typeof %s>;\n", constName, expr, p.Name, constName)
	return nil
}

func (e *Emitter) End(ctx *gen.Context) error {
	e.imports.Write(ctx, "import", e.importExt)
	if names := e.helpers.Direct(); e.runtime != nil && len(names) > 0 {
		ctx.W.Import("import { " + strings.Join(names, ", ") + " } from " + js.Quote(ctx.Rel(e.runtime)+e.importExt) + ";")
	} else {
		ctx.W.WriteString(e.helpers.Source())
	}
	ctx.W.WriteString(e.body.String())
	return nil
}

func (e *Emitter) object(ctx *gen.Context, s *gen.Shape) string {
	var b strings.Builder
	if s.Unknown == gen.Reject {
		b.WriteString("z.strictObject({\n")
	} else {
		b.WriteString("z.object({\n")
	}
	for _, f := range s.Fields {
		where := ts.DeclName(s) + "." + f.JSONName
		switch {
		case f.JSONName == "__proto__":
			ctx.Report.Add(where, "zod never checks a field named __proto__ and drops it from the parsed value")
		case js.PrototypeMember(f.JSONName):
			ctx.Report.Add(where, "Object.prototype defines "+f.JSONName+", and zod tests key presence with `in`, so the field can never be absent")
		}
		field := e.typ(ctx, s, f.Type, where)
		if f.Optional {
			field += ".optional()"
		}
		b.WriteString(js.Doc(f.Doc, "  "))
		b.WriteString("  ")
		// A reference this file has not emitted yet needs a getter: it defers
		// the lookup without the type annotation z.lazy would force.
		if e.forward {
			fmt.Fprintf(&b, "get %s() {\n    return %s;\n  },\n", js.PropKey(f.JSONName), field)
			e.forward = false
			continue
		}
		b.WriteString(js.PropKey(f.JSONName))
		b.WriteString(": ")
		b.WriteString(field)
		b.WriteString(",\n")
	}
	b.WriteString("})")
	if s.Unknown == gen.Collect && s.Rest != nil {
		b.WriteString(".catchall(")
		b.WriteString(e.typ(ctx, s, s.Rest, ts.DeclName(s)))
		b.WriteByte(')')
	}
	return b.String()
}

// kind tells which methods a rendered schema has.
type kind uint8

const (
	other kind = iota
	str
	num
	arr
)

func (e *Emitter) typ(ctx *gen.Context, s *gen.Shape, t *gen.Type, where string) string {
	if e.override != nil {
		if out, ok := e.override(ctx, e.placed, t); ok {
			return e.wrap(ctx, s, t, out, where)
		}
	}
	out, k := e.base(ctx, s, t, where)
	out = e.rules(ctx, t, out, k, where)
	if len(t.OneOf) > 0 {
		parts := make([]string, 1, 1+len(t.OneOf))
		parts[0] = out
		for _, o := range t.OneOf {
			parts = append(parts, e.typ(ctx, s, o, where))
		}
		out = "z.union([" + strings.Join(parts, ", ") + "])"
	}
	if t.Nullable && t.Wire != gen.WireAny {
		out += ".nullable()"
	}
	return out
}

// wrap adds what every type carries regardless of how its base rendered: the
// converter union and null.
func (e *Emitter) wrap(ctx *gen.Context, s *gen.Shape, t *gen.Type, out string, where string) string {
	if len(t.OneOf) > 0 {
		parts := make([]string, 1, 1+len(t.OneOf))
		parts[0] = out
		for _, o := range t.OneOf {
			parts = append(parts, e.typ(ctx, s, o, where))
		}
		out = "z.union([" + strings.Join(parts, ", ") + "])"
	}
	if t.Nullable && t.Wire != gen.WireAny {
		out += ".nullable()"
	}
	return out
}

func (e *Emitter) base(ctx *gen.Context, s *gen.Shape, t *gen.Type, where string) (string, kind) {
	if t.External {
		if e.externalFor != nil {
			if schema, ok := e.externalFor(ctx.File, e.placed, t); ok {
				return schema, other
			}
		}
		if schema, ok := e.external[t.Pkg.Path+"."+t.Name]; ok {
			return schema, other
		}
		if t.Wire != gen.WireString {
			ctx.Report.Add(where, t.GoType+" is external with an unknown shape; map it with zod.External or zod.ExternalFor")
		}
	}
	if t.Ref != nil {
		if pl, ok := ctx.Lookup(t.Ref, s.Mode); ok {
			ref := e.imports.Name(ctx, t.Ref, s.Mode, e.schemaName(ctx.FileOf(t.Ref, s.Mode), pl))
			if ctx.Later(t.Ref, s.Mode) {
				if e.cyclic {
					ref = "z.lazy(() => " + ref + ")"
				} else {
					e.forward = true
				}
			}
			if t.Enum != nil && t.Enum.Zero {
				return "z.union([" + ref + ", z.literal(" + ts.ZeroLiteral(t) + ")])", other
			}
			return ref, other
		}
	}
	if t.Enum != nil {
		vals := make([]string, 0, len(t.Enum.Values)+1)
		for _, v := range t.Enum.Values {
			vals = append(vals, v.Value)
		}
		if t.Enum.Zero {
			vals = append(vals, ts.ZeroLiteral(t))
		}
		switch {
		case len(vals) == 1:
			return "z.literal(" + vals[0] + ")", other
		case t.Wire == gen.WireString:
			return "z.enum([" + strings.Join(vals, ", ") + "])", other
		}
		return "z.literal([" + strings.Join(vals, ", ") + "])", other
	}
	//exhaustive:ignore not every kind applies here
	switch t.Wire {
	case gen.WireString:
		return e.str(t)
	case gen.WireInteger:
		if t.Format == gen.FormatUnixMicro || t.Format == gen.FormatUnixNano {
			ctx.Report.Add(where, "a "+t.Format+" time passes the integers JavaScript holds exactly; the schema asserts no more than an integer")
			return "z.number().refine(Number.isInteger)", num
		}
		return integer(t), num
	case gen.WireNumber:
		if t.Go == gen.Float32 {
			// Go rounds the double to a float32 and rejects it when that
			// overflows, which is exactly what Math.fround reports.
			return "z.number()" + refine("Number.isFinite(Math.fround(x))", "out of range for a 32-bit float"), num
		}
		switch t.Format {
		case gen.FormatUnix:
			// Go reads the seconds into an int64.
			return "z.number().gte(-9223372036854775808).lt(9223372036854775808)", num
		case gen.FormatDurationSec:
			// The int64 nanosecond range ends at ±9223372036.854775807,
			// which rounds to a double Go rejects.
			return "z.number().gt(-9223372036.854776).lt(9223372036.854776)", num
		}
		return "z.number()", num
	case gen.WireBool:
		return "z.boolean()", other
	case gen.WireArray:
		elem := e.typ(ctx, s, t.Elem, where)
		if t.Go == gen.Array || (t.Go == gen.Bytes && t.Len > 0) {
			parts := make([]string, t.Len)
			for i := range parts {
				parts[i] = elem
			}
			return "z.tuple([" + strings.Join(parts, ", ") + "])", other
		}
		return "z.array(" + elem + ")", arr
	case gen.WireObject:
		if t.Go == gen.Map {
			key := e.typ(ctx, s, t.Key, where)
			elem := e.typ(ctx, s, t.Elem, where)
			if t.Key.Enum != nil {
				// A record keyed by an enum or a literal requires every member,
				// where Go checks only the keys present.
				return "z.partialRecord(" + key + ", " + elem + ")", other
			}
			return "z.record(" + key + ", " + elem + ")", other
		}
		return "z.record(z.string(), z.unknown())", other
	}
	return "z.unknown()", other
}

func integer(t *gen.Type) string {
	bits, unsigned := 64, false
	//exhaustive:ignore not every kind applies here
	switch t.Go {
	case gen.BigInt:
		return "z.number().refine(Number.isInteger)"
	case gen.Int8, gen.Uint8:
		bits = 8
	case gen.Int16, gen.Uint16:
		bits = 16
	case gen.Int32, gen.Uint32:
		bits = 32
	}
	//exhaustive:ignore not every kind applies here
	switch t.Go {
	case gen.Uint, gen.Uint8, gen.Uint16, gen.Uint32, gen.Uint64:
		unsigned = true
	}
	lo, hi, ok := js.IntRange(bits, unsigned)
	switch {
	case !ok:
		return "z.int()"
	case bits == 64:
		return "z.int().gte(0)" + negZero
	case unsigned:
		return "z.int().gte(0).lte(" + hi + ")" + negZero
	}
	return "z.int().gte(" + lo + ").lte(" + hi + ")"
}

// negZero rejects -0, which passes gte(0) but is not a Go uint.
var negZero = refine("!Object.is(x, -0)", "must not be negative zero")

func (e *Emitter) str(t *gen.Type) (string, kind) {
	predicate := func(fn, msg string) (string, kind) {
		s := "z.string().refine(" + e.helpers.Use(fn) + ", " + js.Quote(msg) + ")"
		if t.Empty {
			return `z.literal("").or(` + s + ")", other
		}
		return s, str
	}
	switch {
	case t.Format == gen.FormatDateTime:
		return "z.iso.datetime({ offset: true })", str
	case t.Format == gen.FormatBase64:
		out := "z.base64()"
		if t.Len > 0 {
			out += fmt.Sprintf(`.refine((x) => %s(x, 6) === %d, "must decode to %d bytes")`, e.helpers.Use("ggenDecoded"), t.Len, t.Len)
		}
		return out, str
	case t.Format == gen.FormatIP:
		if t.Empty {
			return `z.literal("").or(z.ipv4()).or(z.ipv6())`, other
		}
		return "z.ipv4().or(z.ipv6())", other
	case t.Format == gen.FormatCIDR:
		// z.cidrv6() accepts a prefix length such as 64.5 or NaN, which Go
		// rejects.
		return predicate("ggenIsPrefix", "must be an IP prefix")
	case t.Format == gen.FormatIPZone:
		return predicate("ggenIsAddr", "must be an IP address")
	case t.Format == gen.FormatIPPort:
		return predicate("ggenIsAddrPort", "must be an IP address and port")
	case strings.HasPrefix(t.Format, gen.FormatTimeLayoutPrefix):
		layout := strings.TrimPrefix(t.Format, gen.FormatTimeLayoutPrefix)
		name, oneArg := e.helpers.TimeCheck(layout)
		call := name + "(" + js.Quote(layout) + ", x)"
		if oneArg {
			call = name + "(x)"
		}
		return "z.string()" + refine(call, "must be a time formatted as "+layout), str
	case t.Format == gen.FormatURI:
		return "z.string().refine(" + e.helpers.Use("ggenParsesURL") + `, "must be a URL")`, str
	case t.Format == gen.FormatBigFloat:
		return "z.string().refine(" + e.helpers.Use("ggenIsBigFloat") + `, "must be a number")`, str
	case t.Format == gen.FormatRational:
		return "z.string().refine(" + e.helpers.Use("ggenIsRational") + `, "must be a rational number")`, str
	}
	var check js.Check
	ok := true
	if t.Format == gen.FormatNumberString {
		check = e.helpers.NumberCheck(numberBits(t.Go))
	} else {
		check, ok = e.helpers.FormatCheck(t.Format)
	}
	if !ok {
		return "z.string()", str
	}
	out := "z.string()"
	if check.Func {
		out += refine(check.Name+"(x)", "must be a "+formatWord(t.Format))
	} else {
		out += ".regex(" + check.Name + ")"
	}
	if check.Bits > 0 && t.Len > 0 {
		out += fmt.Sprintf(`.refine((x) => %s(x, %d) === %d, "must decode to %d bytes")`, e.helpers.Use("ggenDecoded"), check.Bits, t.Len, t.Len)
	}
	if t.Format == gen.FormatIntString || t.Format == gen.FormatUintString {
		// Go parses the quoted text at the field's own width, so the schema
		// checks the range at every width, 64 bits included.
		lo, hi := intBounds(t.Go, t.Format == gen.FormatUintString)
		out += refine(fmt.Sprintf("%s(x, %sn, %sn)", e.helpers.Use("ggenFits"), lo, hi), "out of range")
	}
	return out, str
}

// numberBits is the width Go parses a quoted number of kind k at, 0 for
// json.Number, which encoding/json checks for its grammar only.
func numberBits(k gen.GoKind) int {
	//exhaustive:ignore only the number-string kinds apply here
	switch k {
	case gen.Float32:
		return 32
	case gen.Number:
		return 0
	}
	return 64
}

// formatWord names a format in a message.
func formatWord(format string) string {
	switch format {
	case gen.FormatDuration:
		return "duration"
	case gen.FormatNumberString:
		return "number"
	}
	return format
}

// intBounds returns the exact range of a Go integer kind, as decimal literals.
func intBounds(k gen.GoKind, forceUnsigned bool) (lo, hi string) {
	bits := 64
	//exhaustive:ignore not every kind applies here
	switch k {
	case gen.Int8, gen.Uint8:
		bits = 8
	case gen.Int16, gen.Uint16:
		bits = 16
	case gen.Int32, gen.Uint32:
		bits = 32
	}
	unsigned := forceUnsigned
	//exhaustive:ignore not every kind applies here
	switch k {
	case gen.Uint, gen.Uint8, gen.Uint16, gen.Uint32, gen.Uint64:
		unsigned = true
	}
	if unsigned {
		return "0", new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(bits)), big.NewInt(1)).String()
	}
	half := new(big.Int).Lsh(big.NewInt(1), uint(bits-1))
	return new(big.Int).Neg(half).String(), new(big.Int).Sub(half, big.NewInt(1)).String()
}

// escapeDollar protects a replacement string from String.replaceAll, which
// reads $&, $1 and $$ in it. Go's strings.Replacer treats them literally.
func escapeDollar(s string) string { return strings.ReplaceAll(s, "$", "$$$$") }

// checkSafe reports a numeric bound JavaScript cannot hold exactly.
func (e *Emitter) checkSafe(ctx *gen.Context, arg, where string) {
	n, ok := new(big.Int).SetString(arg, 10)
	if !ok {
		return
	}
	if n.CmpAbs(big.NewInt(1<<53)) >= 0 {
		ctx.Report.Add(where, "the bound "+arg+" is outside the integers JavaScript holds exactly; the check can never pass")
	}
}

func refine(cond, msg string) string {
	return ".refine((x) => " + cond + ", " + js.Quote(msg) + ")"
}

func overwrite(body string) string {
	return ".overwrite((x) => " + body + ")"
}

// rules appends t's rules to a rendered schema of kind k.
func (e *Emitter) rules(ctx *gen.Context, t *gen.Type, out string, k kind, where string) string {
	if t.Converted && len(t.Rules) > 0 {
		// The rules constrain what the Go converter returns, not the JSON the
		// schema sees, so there is nothing to check here.
		ctx.Report.Add(where, "rules run on the converter's result and are not expressible in the schema")
		return out
	}
	numericString := t.Format == gen.FormatIntString || t.Format == gen.FormatUintString || t.Format == gen.FormatNumberString || t.Format == gen.FormatBigFloat
	f32 := t.Go == gen.Float32
	for ri, r := range t.Rules {
		arg := func(i int) string { return r.Args[i] }
		switch r.Op {
		case gen.NotEmpty:
			switch {
			case k == str || k == arr:
				out += ".min(1)"
			case t.Go == gen.Map:
				out += refine(e.helpers.Use("ggenEntries")+"(x) > 0", "must not be empty")
			default:
				out += refine("x.length > 0", "must not be empty")
			}
		case gen.Len, gen.MinLen, gen.MaxLen:
			op, word := lenOp(r.Op)
			switch {
			case k == arr:
				out += map[gen.Op]string{gen.Len: ".length(", gen.MinLen: ".min(", gen.MaxLen: ".max("}[r.Op] + arg(0) + ")"
			case t.Go == gen.Map:
				out += refine(e.helpers.Use("ggenEntries")+"(x) "+op+" "+arg(0), word+" "+js.Count(arg(0), "entry"))
			case t.Go == gen.Bytes:
				out += refine(
					fmt.Sprintf("%s(x, %d) %s %s", e.helpers.Use("ggenDecoded"), js.FormatBits(t.Format), op, arg(0)),
					word+" "+js.Count(arg(0), "byte"),
				)
			case t.Wire == gen.WireString:
				out += refine(e.helpers.Use("ggenBytes")+"(x) "+op+" "+arg(0), word+" "+js.Count(arg(0), "byte"))
			default:
				out += refine("x.length "+op+" "+arg(0), word+" "+js.Count(arg(0), "item"))
			}
		case gen.Runes, gen.MinRunes, gen.MaxRunes:
			op, word := lenOp(map[gen.Op]gen.Op{gen.Runes: gen.Len, gen.MinRunes: gen.MinLen, gen.MaxRunes: gen.MaxLen}[r.Op])
			out += refine(e.helpers.Use("ggenRunes")+"(x) "+op+" "+arg(0), word+" "+js.Count(arg(0), "character"))
		case gen.GT, gen.GTE, gen.LT, gen.LTE:
			op := map[gen.Op]string{gen.GT: ">", gen.GTE: ">=", gen.LT: "<", gen.LTE: "<="}[r.Op]
			switch {
			case k == num && !f32:
				e.checkSafe(ctx, arg(0), where)
				out += "." + map[gen.Op]string{gen.GT: "gt", gen.GTE: "gte", gen.LT: "lt", gen.LTE: "lte"}[r.Op] + "(" + arg(0) + ")"
			case k == num:
				out += refine("Math.fround(x) "+op+" Math.fround("+arg(0)+")", "must be "+op+" "+arg(0))
			case numericString:
				n := e.helpers.NumberText(t.Format)
				out += refine(js.Fround(n.Val, f32)+" "+op+" "+js.Fround(n.Lit(arg(0)), f32), "must be "+op+" "+arg(0))
			default:
				out += refine(js.Fround("Number(x)", f32)+" "+op+" "+js.Fround(arg(0), f32), "must be "+op+" "+arg(0))
			}
		case gen.Eq, gen.Neq:
			op, word := "===", "must be "
			if r.Op == gen.Neq {
				op, word = "!==", "must not be "
			}
			switch {
			case t.Wire == gen.WireString && !numericString:
				out += refine("x "+op+" "+js.Quote(arg(0)), word+arg(0))
			case numericString:
				n := e.helpers.NumberText(t.Format)
				out += refine(js.Fround(n.Val, f32)+" "+op+" "+js.Fround(n.Lit(arg(0)), f32), word+arg(0))
			default:
				out += refine(js.Fround("x", f32)+" "+op+" "+js.Fround(arg(0), f32), word+arg(0))
			}
		case gen.Multiple:
			switch {
			case k == num:
				out += ".multipleOf(" + arg(0) + ")"
			case numericString:
				n := e.helpers.NumberText(t.Format)
				out += refine(n.Val+" % "+n.Lit(arg(0))+" === "+n.Zero(), "must be a multiple of "+arg(0))
			default:
				out += refine("Number(x) % "+arg(0)+" === 0", "must be a multiple of "+arg(0))
			}
		case gen.OneOf:
			vals := make([]string, len(r.Args))
			if numericString {
				// Go matches the parsed number, whatever its spelling.
				n := e.helpers.NumberText(t.Format)
				for i, a := range r.Args {
					vals[i] = js.Fround(n.Lit(a), f32)
				}
				out += refine("["+strings.Join(vals, ", ")+"].includes("+js.Fround(n.Val, f32)+")", "must be one of "+strings.Join(r.Args, ", "))
				break
			}
			for i, a := range r.Args {
				vals[i] = js.Fround(a, f32)
				if t.Wire == gen.WireString {
					vals[i] = js.Quote(a)
				}
			}
			switch {
			case f32:
				out += refine("["+strings.Join(vals, ", ")+"].includes(Math.fround(x))", "must be one of "+strings.Join(r.Args, ", "))
			case ri < len(t.Rules)-1:
				// A pipe would hide the base schema's methods from the rules
				// after it.
				out += refine("["+strings.Join(vals, ", ")+"].includes(x)", "must be one of "+strings.Join(r.Args, ", "))
			case t.Wire == gen.WireString:
				out += ".pipe(z.enum([" + strings.Join(vals, ", ") + "]))"
			default:
				out += ".pipe(z.literal([" + strings.Join(vals, ", ") + "]))"
			}
		case gen.URLRule:
			out += refine(e.helpers.Use("ggenIsURL")+"(x)", "must be a URL")
		case gen.Alphanum, gen.Numeric, gen.Hex, gen.IsLower, gen.IsUpper:
			re := js.ClassRegex(r.Op.String())
			if k == str {
				out += ".regex(" + re + ")"
			} else {
				out += refine(re+".test(x)", "invalid characters")
			}
		case gen.Starts, gen.Ends, gen.Contains:
			method := map[gen.Op]string{gen.Starts: "startsWith", gen.Ends: "endsWith", gen.Contains: "includes"}[r.Op]
			if k == str {
				out += "." + method + "(" + js.Quote(arg(0)) + ")"
			} else {
				out += refine("x."+method+"("+js.Quote(arg(0))+")", "must "+method+" "+arg(0))
			}
		case gen.Trim, gen.ToLower, gen.ToUpper:
			// JavaScript's own trim and case mapping differ from Go's: it
			// trims U+FEFF and not U+0085, lowercases a final sigma to ς and
			// expands ß to SS.
			helper := e.helpers.Use(map[gen.Op]string{gen.Trim: "ggenTrim", gen.ToLower: "ggenToLower", gen.ToUpper: "ggenToUpper"}[r.Op])
			out += overwrite(helper + "(x)")
		case gen.TrimPrefix:
			out += overwrite(fmt.Sprintf("x.startsWith(%s) ? x.slice(%d) : x", js.Quote(arg(0)), js.UTF16Len(arg(0))))
		case gen.TrimSuffix:
			out += overwrite(fmt.Sprintf("x.endsWith(%s) ? x.slice(0, -%d) : x", js.Quote(arg(0)), js.UTF16Len(arg(0))))
		case gen.Replace:
			out += overwrite("x.replaceAll(" + js.Quote(arg(0)) + ", " + js.Quote(escapeDollar(arg(1))) + ")")
		case gen.Clamp:
			if numericString {
				// Go clamps the parsed number and writes it back quoted.
				n := e.helpers.NumberText(t.Format)
				out += overwrite("String(" + js.Clamp(n.Val, n.Lit(arg(0)), n.Lit(arg(1)), n.Int) + ")")
			} else {
				out += overwrite(js.Clamp("x", arg(0), arg(1), false))
			}
		case gen.Func:
			if action, ok := e.funcAction(ctx, r); ok {
				out += action
			} else {
				ctx.Report.Add(where, "@"+r.Func+" has no zod twin; map it with zod.Func")
			}
		default:
			ctx.Report.Add(where, r.Op.String()+" has no zod twin")
		}
	}
	return out
}

func lenOp(op gen.Op) (cmp, word string) {
	//exhaustive:ignore not every kind applies here
	switch op {
	case gen.MinLen:
		return ">=", "must be at least"
	case gen.MaxLen:
		return "<=", "must be at most"
	}
	return "===", "must be exactly"
}

// funcAction returns the zod action for an @Func rule. It tries FuncFor, then
// Func by the reference as written, then Func by import path and name.
func (e *Emitter) funcAction(ctx *gen.Context, r gen.Rule) (string, bool) {
	if e.funcFor != nil {
		if action, ok := e.funcFor(ctx.File, e.placed, r); ok {
			return action, true
		}
	}
	if action, ok := e.funcs[r.Func]; ok {
		return action, true
	}
	action, ok := e.funcs[r.FuncPkg+"."+r.Func[strings.LastIndexByte(r.Func, '.')+1:]]
	return action, ok
}
