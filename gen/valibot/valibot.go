// Package valibot renders gen shapes as Valibot 1.x schemas.
package valibot

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

// HeaderFor decides the first line per output file; "" writes none.
func HeaderFor(fn func(f *gen.File) string) Option { return func(e *Emitter) { e.header = fn } }

// Func adds action to the pipe of every value checked or transformed by the
// Go function reference, as written in the tag without "@": "IsAdult" or
// "pkg.IsAdult". action is a pipe action, e.g. `v.check((x) => x >= 18)`.
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
// schema, e.g. "v.pipe(v.string(), v.uuid())".
func External(goPath, schema string) Option {
	return func(e *Emitter) { e.external[goPath] = schema }
}

// ExternalType sets the TypeScript type of an external Go type, which a
// recursive shape needs for its v.GenericSchema annotation. Without it the
// annotation follows the type's wire shape.
func ExternalType(goPath, tsType string) Option {
	return func(e *Emitter) { e.types.External[goPath] = tsType }
}

// ExternalFor renders an external type t, used in placed type p, as schema
// when fn returns ok, before External.
func ExternalFor(fn func(f *gen.File, p gen.Placed, t *gen.Type) (schema string, ok bool)) Option {
	return func(e *Emitter) { e.externalFor = fn }
}

// Runtime imports helpers from f, a file created with ts.Runtime, instead of
// inlining them into every file.
func Runtime(f *gen.File) Option { return func(e *Emitter) { e.runtime = f } }

// SchemaName derives the schema constant's name of placed type p, whose name
// stays the type's name, in file f. Default: the placed name.
func SchemaName(fn func(f *gen.File, p gen.Placed) string) Option {
	return func(e *Emitter) { e.schemaName = fn }
}

// Override renders a type itself when fn returns ok, before any other rule.
func Override(fn func(ctx *gen.Context, p gen.Placed, t *gen.Type) (schema string, ok bool)) Option {
	return func(e *Emitter) { e.override = fn }
}

// Emitter renders Valibot schemas: an exported schema constant and its
// inferred type per placed shape.
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

	placed gen.Placed // the shape Decl is rendering

	types   ts.Printer
	imports ts.Imports
	helpers js.Helpers
	objects bool // the file uses objectGuard
	body    strings.Builder
}

// objectGuard rejects arrays, which valibot's object and record schemas
// accept, keeping the wrapped schema's input type.
const objectGuard = `function ggenObject<T extends v.GenericSchema<object>>(schema: T) {
  const isObject = (x: unknown) => typeof x === "object" && x !== null && !Array.isArray(x);
  return v.pipe(v.custom<v.InferInput<T>>(isObject, "Invalid type: Expected Object"), schema);
}

`

// New returns a Valibot emitter.
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
	e.objects = false
	e.body.Reset()
	if h := e.header(ctx.File); h != "" {
		ctx.W.Header(h)
	}
	ctx.W.Import(`import * as v from "valibot";`)
	return nil
}

func (e *Emitter) Decl(ctx *gen.Context, p gen.Placed) error {
	e.placed = p
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
	if s.Recursive {
		var tsType string
		if s.Alias != nil {
			tsType = e.types.Type(ctx, s, s.Alias)
		} else {
			tsType = e.types.Object(ctx, s)
		}
		fmt.Fprintf(&e.body, "export type %s = %s;\nexport const %s: v.GenericSchema<%s> = %s;\n", p.Name, tsType, constName, p.Name, expr)
		return nil
	}
	fmt.Fprintf(&e.body, "export const %s = %s;\nexport type %s = v.InferOutput<typeof %s>;\n", constName, expr, p.Name, constName)
	return nil
}

func (e *Emitter) End(ctx *gen.Context) error {
	e.imports.Write(ctx, "import", e.importExt)
	if names := e.helpers.Direct(); e.runtime != nil && len(names) > 0 {
		ctx.W.Import("import { " + strings.Join(names, ", ") + " } from " + js.Quote(ctx.Rel(e.runtime)+e.importExt) + ";")
	} else {
		ctx.W.WriteString(e.helpers.Source())
	}
	if e.objects {
		ctx.W.WriteString(objectGuard)
	}
	ctx.W.WriteString(e.body.String())
	return nil
}

func (e *Emitter) object(ctx *gen.Context, s *gen.Shape) string {
	var b strings.Builder
	rest := s.Unknown == gen.Collect && s.Rest != nil
	// valibot's object schemas accept an array whose own keys satisfy them,
	// which only an object with no required field (or one with a rest
	// schema) can. Guarding just those keeps the rest plain object schemas,
	// with v.pick, v.omit and .entries intact.
	guard := rest
	if !guard {
		guard = true
		for _, f := range s.Fields {
			if !f.Optional {
				guard = false
				break
			}
		}
	}
	if guard {
		e.objects = true
		b.WriteString("ggenObject(")
	}
	switch {
	case rest:
		b.WriteString("v.objectWithRest({\n")
	case s.Unknown == gen.Reject:
		b.WriteString("v.strictObject({\n")
	default:
		b.WriteString("v.object({\n")
	}
	for _, f := range s.Fields {
		where := ts.DeclName(s) + "." + f.JSONName
		if js.PrototypeMember(f.JSONName) {
			ctx.Report.Add(where, "Object.prototype defines "+f.JSONName+", and valibot tests key presence with `in`, so the field can never be absent")
		}
		b.WriteString(js.Doc(f.Doc, "  "))
		b.WriteString("  ")
		b.WriteString(js.PropKey(f.JSONName))
		b.WriteString(": ")
		field := e.typ(ctx, s, f.Type, where)
		if f.Optional {
			field = "v.optional(" + field + ")"
		}
		b.WriteString(field)
		b.WriteString(",\n")
	}
	b.WriteByte('}')
	if rest {
		b.WriteString(", ")
		b.WriteString(e.typ(ctx, s, s.Rest, ts.DeclName(s)))
	}
	b.WriteByte(')')
	if guard {
		b.WriteByte(')')
	}
	return b.String()
}

// schema is a head schema and the pipe actions applied to it.
type schema struct {
	head    string
	actions []string
	kind    kind
}

type kind uint8

const (
	other kind = iota
	str
	num
	arr
	obj // map
)

// maxPipe is how many items valibot's pipe overloads accept.
const maxPipe = 20

func (s schema) String() string {
	if len(s.actions) == 0 {
		return s.head
	}
	// A pipe past the overload ceiling nests: a schema is a valid pipe item,
	// so v.pipe(v.pipe(head, …), …) type-checks and behaves the same.
	out := s.head
	for rest := s.actions; len(rest) > 0; {
		n := min(len(rest), maxPipe-1)
		out = "v.pipe(" + out + ", " + strings.Join(rest[:n], ", ") + ")"
		rest = rest[n:]
	}
	return out
}

func pipe(head string, k kind, actions ...string) schema {
	return schema{head: head, actions: actions, kind: k}
}

func (e *Emitter) typ(ctx *gen.Context, s *gen.Shape, t *gen.Type, where string) string {
	if e.override != nil {
		if out, ok := e.override(ctx, e.placed, t); ok {
			return e.wrap(ctx, s, t, out, where)
		}
	}
	sc := e.base(ctx, s, t, where)
	e.rules(ctx, t, &sc, where)
	out := sc.String()
	if len(t.OneOf) > 0 {
		parts := []string{out}
		for _, o := range t.OneOf {
			parts = append(parts, e.typ(ctx, s, o, where))
		}
		out = "v.union([" + strings.Join(parts, ", ") + "])"
	}
	if t.Nullable && t.Wire != gen.WireAny {
		out = "v.nullable(" + out + ")"
	}
	return out
}

func (e *Emitter) base(ctx *gen.Context, s *gen.Shape, t *gen.Type, where string) schema {
	if t.External {
		if e.externalFor != nil {
			if sc, ok := e.externalFor(ctx.File, e.placed, t); ok {
				return pipe(sc, other)
			}
		}
		if sc, ok := e.external[t.Pkg.Path+"."+t.Name]; ok {
			return pipe(sc, other)
		}
		if t.Wire != gen.WireString {
			ctx.Report.Add(where, t.GoType+" is external with an unknown shape; map it with valibot.External or valibot.ExternalFor")
		}
	}
	if t.Ref != nil {
		if pl, ok := ctx.Lookup(t.Ref, s.Mode); ok {
			ref := e.imports.Name(ctx, t.Ref, s.Mode, e.schemaName(ctx.FileOf(t.Ref, s.Mode), pl))
			if ctx.Later(t.Ref, s.Mode) {
				ref = "v.lazy(() => " + ref + ")"
			}
			if t.Enum != nil && t.Enum.Zero {
				return pipe("v.union(["+ref+", v.literal("+ts.ZeroLiteral(t)+")])", other)
			}
			return pipe(ref, other)
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
		if len(vals) == 1 {
			return pipe("v.literal("+vals[0]+")", other)
		}
		return pipe("v.picklist(["+strings.Join(vals, ", ")+"])", other)
	}
	switch t.Wire {
	case gen.WireString:
		return e.str(t)
	case gen.WireInteger:
		if t.Format == gen.FormatUnixMicro || t.Format == gen.FormatUnixNano {
			ctx.Report.Add(where, "a "+t.Format+" time passes the integers JavaScript holds exactly; the schema asserts no more than an integer")
			return pipe("v.number()", num, "v.integer()")
		}
		return integer(t)
	case gen.WireNumber:
		if t.Go == gen.Float32 {
			// Go rounds the double to a float32 and rejects an overflow,
			// which is what Math.fround reports.
			return pipe("v.number()", num, check("Number.isFinite(Math.fround(x))", "out of range for a 32-bit float"))
		}
		return pipe("v.number()", num, "v.finite()")
	case gen.WireBool:
		return pipe("v.boolean()", other)
	case gen.WireArray:
		elem := e.typ(ctx, s, t.Elem, where)
		if t.Go == gen.Array || (t.Go == gen.Bytes && t.Len > 0) {
			parts := make([]string, t.Len)
			for i := range parts {
				parts[i] = elem
			}
			return pipe("v.strictTuple(["+strings.Join(parts, ", ")+"])", other)
		}
		return pipe("v.array("+elem+")", arr)
	case gen.WireObject:
		if t.Go == gen.Map {
			e.objects = true
			return pipe("ggenObject(v.record("+e.typ(ctx, s, t.Key, where)+", "+e.typ(ctx, s, t.Elem, where)+"))", obj)
		}
		e.objects = true
		return pipe("ggenObject(v.record(v.string(), v.unknown()))", obj)
	}
	return pipe("v.unknown()", other)
}

func widthOf(g gen.GoKind) (bits int, unsigned bool) {
	switch g {
	case gen.Int8, gen.Uint8:
		bits = 8
	case gen.Int16, gen.Uint16:
		bits = 16
	case gen.Int32, gen.Uint32:
		bits = 32
	default:
		bits = 64
	}
	switch g {
	case gen.Uint, gen.Uint8, gen.Uint16, gen.Uint32, gen.Uint64:
		unsigned = true
	}
	return bits, unsigned
}

func integer(t *gen.Type) schema {
	if t.Go == gen.BigInt {
		return pipe("v.number()", num, "v.integer()")
	}
	bits, unsigned := widthOf(t.Go)
	lo, hi, ok := js.IntRange(bits, unsigned)
	switch {
	case !ok:
		return pipe("v.number()", num, "v.safeInteger()")
	case bits == 64:
		// -0 passes minValue(0) but is not a Go uint.
		return pipe("v.number()", num, "v.safeInteger()", "v.minValue(0)", check("!Object.is(x, -0)", "must not be negative zero"))
	}
	return pipe("v.number()", num, "v.integer()", "v.minValue("+lo+")", "v.maxValue("+hi+")")
}

func check(cond, msg string) string {
	return "v.check((x) => " + cond + ", " + js.Quote(msg) + ")"
}

func transform(body string) string {
	return "v.transform((x) => " + body + ")"
}

func (e *Emitter) str(t *gen.Type) schema {
	predicate := func(fn, msg string) schema {
		sc := pipe("v.string()", str, "v.check("+fn+", "+js.Quote(msg)+")")
		if t.Empty {
			return pipe("v.union([v.pipe(v.string(), v.empty()), "+sc.String()+"], "+js.Quote("must be empty or "+strings.TrimPrefix(msg, "must be "))+")", other)
		}
		return sc
	}
	switch {
	case t.Format == gen.FormatBase64:
		sc := pipe("v.string()", str, "v.base64()")
		if t.Len > 0 {
			sc.actions = append(sc.actions, check(fmt.Sprintf("%s(x, 6) === %d", e.helpers.Use("ggenDecoded"), t.Len), fmt.Sprintf("must decode to %d bytes", t.Len)))
		}
		return sc
	case t.Format == gen.FormatIP:
		return predicate(e.helpers.Use("ggenIsIP"), "must be an IP address")
	case t.Format == gen.FormatIPZone:
		return predicate(e.helpers.Use("ggenIsAddr"), "must be an IP address")
	case t.Format == gen.FormatCIDR:
		return predicate(e.helpers.Use("ggenIsPrefix"), "must be an IP prefix")
	case t.Format == gen.FormatDateTime:
		return pipe("v.string()", str, "v.check("+e.helpers.Use("ggenIsDateTime")+`, "must be an RFC 3339 date-time")`)
	case strings.HasPrefix(t.Format, gen.FormatTimeLayoutPrefix):
		layout := strings.TrimPrefix(t.Format, gen.FormatTimeLayoutPrefix)
		name, oneArg := e.helpers.TimeCheck(layout)
		if oneArg {
			return pipe("v.string()", str, "v.check("+name+", "+js.Quote("must be a time formatted as "+layout)+")")
		}
		return pipe("v.string()", str, check(name+"("+js.Quote(layout)+", x)", "must be a time formatted as "+layout))
	case t.Format == gen.FormatURI:
		return pipe("v.string()", str, "v.check("+e.helpers.Use("ggenParsesURL")+`, "must be a URL")`)
	case t.Format == gen.FormatBigFloat:
		return pipe("v.string()", str, "v.check("+e.helpers.Use("ggenIsBigFloat")+`, "must be a number")`)
	case t.Format == gen.FormatRational:
		return pipe("v.string()", str, "v.check("+e.helpers.Use("ggenIsRational")+`, "must be a rational number")`)
	}
	c, ok := e.helpers.FormatCheck(t.Format)
	if !ok {
		return pipe("v.string()", str)
	}
	sc := pipe("v.string()", str, "v.regex("+c.Name+")")
	if c.Func {
		sc = pipe("v.string()", str, "v.check("+c.Name+", "+js.Quote("must be a "+formatWord(t.Format))+")")
	}
	if c.Bits > 0 && t.Len > 0 {
		sc.actions = append(sc.actions, check(fmt.Sprintf("%s(x, %d) === %d", e.helpers.Use("ggenDecoded"), c.Bits, t.Len), fmt.Sprintf("must decode to %d bytes", t.Len)))
	}
	if t.Format == gen.FormatIntString || t.Format == gen.FormatUintString {
		// Go parses the quoted text at the field's own width, 64 bits too.
		lo, hi := intBounds(t.Go, t.Format == gen.FormatUintString)
		sc.actions = append(sc.actions, check(fmt.Sprintf("%s(x, %sn, %sn)", e.helpers.Use("ggenFits"), lo, hi), "out of range"))
	}
	return sc
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

// intBounds is the exact range of a Go integer kind, as decimal literals.
func intBounds(k gen.GoKind, forceUnsigned bool) (lo, hi string) {
	bits, unsigned := widthOf(k)
	unsigned = unsigned || forceUnsigned
	if unsigned {
		return "0", new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(bits)), big.NewInt(1)).String()
	}
	half := new(big.Int).Lsh(big.NewInt(1), uint(bits-1))
	return new(big.Int).Neg(half).String(), new(big.Int).Sub(half, big.NewInt(1)).String()
}

// rules adds t's rules to sc as pipe actions.
func (e *Emitter) rules(ctx *gen.Context, t *gen.Type, sc *schema, where string) {
	if t.Converted && len(t.Rules) > 0 {
		ctx.Report.Add(where, "rules run on the converter's result and are not expressible in the schema")
		return
	}
	numericString := t.Format == gen.FormatIntString || t.Format == gen.FormatUintString || t.Format == gen.FormatNumberString || t.Format == gen.FormatBigFloat
	binary := t.Go == gen.Bytes && t.Wire == gen.WireString
	add := func(a string) { sc.actions = append(sc.actions, a) }
	for _, r := range t.Rules {
		arg := func(i int) string { return r.Args[i] }
		switch r.Op {
		case gen.NotEmpty:
			if sc.kind == obj {
				add("v.minEntries(1)")
			} else {
				add("v.nonEmpty()")
			}
		case gen.Len, gen.MinLen, gen.MaxLen:
			suffix := map[gen.Op]string{gen.Len: "", gen.MinLen: "min", gen.MaxLen: "max"}[r.Op]
			action := func(unit string) string {
				if suffix == "" {
					return "v." + strings.ToLower(unit[:1]) + unit[1:] + "(" + arg(0) + ")"
				}
				return "v." + suffix + unit + "(" + arg(0) + ")"
			}
			switch {
			case binary:
				op, word := lenOp(r.Op)
				add(check(fmt.Sprintf("%s(x, %d) %s %s", e.helpers.Use("ggenDecoded"), js.FormatBits(t.Format), op, arg(0)), word+" "+js.Count(arg(0), "byte")))
			case sc.kind == obj:
				add(action("Entries"))
			case sc.kind == arr || t.Wire == gen.WireArray:
				add(action("Length"))
			default:
				add(action("Bytes"))
			}
		case gen.Runes, gen.MinRunes, gen.MaxRunes:
			op, word := lenOp(map[gen.Op]gen.Op{gen.Runes: gen.Len, gen.MinRunes: gen.MinLen, gen.MaxRunes: gen.MaxLen}[r.Op])
			add(check(e.helpers.Use("ggenRunes")+"(x) "+op+" "+arg(0), word+" "+js.Count(arg(0), "character")))
		case gen.GT, gen.GTE, gen.LT, gen.LTE:
			if sc.kind == num {
				e.checkSafe(ctx, arg(0), where)
				add(map[gen.Op]string{gen.GT: "v.gtValue(", gen.GTE: "v.minValue(", gen.LT: "v.ltValue(", gen.LTE: "v.maxValue("}[r.Op] + arg(0) + ")")
			} else {
				op := map[gen.Op]string{gen.GT: ">", gen.GTE: ">=", gen.LT: "<", gen.LTE: "<="}[r.Op]
				add(check("Number(x) "+op+" "+arg(0), "must be "+op+" "+arg(0)))
			}
		case gen.Eq, gen.Neq:
			fn := "v.value("
			if r.Op == gen.Neq {
				fn = "v.notValue("
			}
			switch {
			case numericString:
				op, word := "===", "must be "
				if r.Op == gen.Neq {
					op, word = "!==", "must not be "
				}
				add(check("Number(x) "+op+" "+arg(0), word+arg(0)))
			case t.Wire == gen.WireString:
				add(fn + js.Quote(arg(0)) + ")")
			default:
				add(fn + arg(0) + ")")
			}
		case gen.Multiple:
			if sc.kind == num {
				add("v.multipleOf(" + arg(0) + ")")
			} else {
				add(check("Number(x) % "+arg(0)+" === 0", "must be a multiple of "+arg(0)))
			}
		case gen.OneOf:
			vals := make([]string, len(r.Args))
			for i, a := range r.Args {
				vals[i] = a
				if t.Wire == gen.WireString {
					vals[i] = js.Quote(a)
				}
			}
			// picklist narrows the type; values only checks it.
			add("v.picklist([" + strings.Join(vals, ", ") + "])")
		case gen.URLRule:
			add("v.check(" + e.helpers.Use("ggenIsURL") + `, "must be a URL")`)
		case gen.Alphanum, gen.Numeric, gen.Hex, gen.IsLower, gen.IsUpper:
			add("v.regex(" + js.ClassRegex(r.Op.String()) + ")")
		case gen.Starts, gen.Ends, gen.Contains:
			fn := map[gen.Op]string{gen.Starts: "v.startsWith(", gen.Ends: "v.endsWith(", gen.Contains: "v.includes("}[r.Op]
			add(fn + js.Quote(arg(0)) + ")")
		case gen.Trim, gen.ToLower, gen.ToUpper:
			// v.trim and the case actions are JavaScript's: they trim U+FEFF
			// and not U+0085, lowercase a final sigma to ς and expand ß to SS.
			add(transform(e.helpers.Use(map[gen.Op]string{gen.Trim: "ggenTrim", gen.ToLower: "ggenToLower", gen.ToUpper: "ggenToUpper"}[r.Op]) + "(x)"))
		case gen.TrimPrefix:
			add(transform(fmt.Sprintf("x.startsWith(%s) ? x.slice(%d) : x", js.Quote(arg(0)), js.UTF16Len(arg(0)))))
		case gen.TrimSuffix:
			add(transform(fmt.Sprintf("x.endsWith(%s) ? x.slice(0, -%d) : x", js.Quote(arg(0)), js.UTF16Len(arg(0)))))
		case gen.Replace:
			add(transform("x.replaceAll(" + js.Quote(arg(0)) + ", " + js.Quote(escapeDollar(arg(1))) + ")"))
		case gen.Clamp:
			add(transform("Math.min(Math.max(x, " + arg(0) + "), " + arg(1) + ")"))
		case gen.Func:
			if action, ok := e.funcAction(ctx, r); ok {
				add(action)
			} else {
				ctx.Report.Add(where, "@"+r.Func+" has no valibot twin; map it with valibot.Func")
			}
		default:
			ctx.Report.Add(where, r.Op.String()+" has no valibot twin")
		}
	}
}

// escapeDollar protects a replacement string from String.replaceAll, which
// reads $&, $1 and $$ in it; Go's strings.Replacer is literal.
func escapeDollar(s string) string { return strings.ReplaceAll(s, "$", "$$$$") }

// wrap adds what every type carries regardless of how its base rendered.
func (e *Emitter) wrap(ctx *gen.Context, s *gen.Shape, t *gen.Type, out string, where string) string {
	if len(t.OneOf) > 0 {
		parts := []string{out}
		for _, o := range t.OneOf {
			parts = append(parts, e.typ(ctx, s, o, where))
		}
		out = "v.union([" + strings.Join(parts, ", ") + "])"
	}
	if t.Nullable && t.Wire != gen.WireAny {
		out = "v.nullable(" + out + ")"
	}
	return out
}

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

func lenOp(op gen.Op) (cmp, word string) {
	switch op {
	case gen.MinLen:
		return ">=", "must be at least"
	case gen.MaxLen:
		return "<=", "must be at most"
	}
	return "===", "must be exactly"
}

// count spells "1 byte" / "2 bytes".

// funcAction is the twin of an @Func rule: FuncFor, then Func by the reference
// as written, then by import path and name.
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
