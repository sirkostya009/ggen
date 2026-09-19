// Package kotlin renders gen shapes as Kotlin data classes for
// kotlinx.serialization.
package kotlin

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/sirkostya009/ggen/gen"
	"github.com/sirkostya009/ggen/gen/internal/names"
)

// Option configures an Emitter.
type Option func(*Emitter)

// Header writes line at the top of every file, e.g. a generated-code marker.
func Header(line string) Option { return HeaderFor(func(*gen.File) string { return line }) }

// HeaderFor decides the first line per output file; "" writes none.
func HeaderFor(fn func(f *gen.File) string) Option { return func(e *Emitter) { e.header = fn } }

// IgnoreUnknown marks every class @JsonIgnoreUnknownKeys, whatever the Go
// decoder does with an undeclared key. A generated client outlives the server
// version it was built against: the Go side describes a server that rejects
// unknown keys, not a client that must tolerate a new field.
func IgnoreUnknown() Option { return func(e *Emitter) { e.ignoreAll = true } }

// External renders the Go type goPath ("github.com/google/uuid.UUID") as the
// Kotlin type kotlinType, adding imports to files that use it.
func External(goPath, kotlinType string, imports ...string) Option {
	return func(e *Emitter) { e.external[goPath] = external{kotlinType, "", imports} }
}

// ExternalWith is External for a type kotlinx has no serializer for: the
// annotation goes on the type itself, "@Contextual" or
// "@Serializable(with = InstantSerializer::class)".
func ExternalWith(goPath, kotlinType, annotation string, imports ...string) Option {
	return func(e *Emitter) { e.external[goPath] = external{kotlinType, annotation, imports} }
}

// ExternalFor renders an external type t, used in placed type p, as
// kotlinType with imports when fn returns ok, before External. Like every
// ...For option, fn sees the output file (f.Path), the placed name, and the Go
// type through p.Shape.Decl: its package, source file, name and directives.
func ExternalFor(fn func(f *gen.File, p gen.Placed, t *gen.Type) (kotlinType string, imports []string, ok bool)) Option {
	return func(e *Emitter) { e.externalFor = fn }
}

type external struct {
	typ        string
	annotation string
	imports    []string
}

// Emitter renders kotlinx.serialization data classes. Every file it renders
// declares the same package.
type Emitter struct {
	pkg         string
	header      func(*gen.File) string
	ignoreAll   bool
	external    map[string]external
	externalFor func(*gen.File, gen.Placed, *gen.Type) (string, []string, bool)

	placed  gen.Placed // the shape Decl is rendering
	imports map[string]struct{}
	aliases map[string]string // qualified name → local name
	locals  map[string]struct{}
	optIn   bool
	body    strings.Builder
}

// New returns an emitter whose files declare package pkg.
func New(pkg string, opts ...Option) *Emitter {
	e := &Emitter{pkg: pkg, external: map[string]external{}, header: func(*gen.File) string { return "" }}
	for _, o := range opts {
		o(e)
	}
	return e
}

func (e *Emitter) Begin(ctx *gen.Context) error {
	e.imports = map[string]struct{}{}
	e.aliases = map[string]string{}
	e.locals = map[string]struct{}{}
	for _, p := range ctx.File.Placed() {
		e.locals[p.Name] = struct{}{}
	}
	e.optIn = false
	e.body.Reset()
	return nil
}

func (e *Emitter) Decl(ctx *gen.Context, p gen.Placed) error {
	if !isIdent(p.Name) {
		return fmt.Errorf("%q is not a valid Kotlin class name", p.Name)
	}
	e.placed = p
	s := p.Shape
	if e.body.Len() > 0 {
		e.body.WriteByte('\n')
	}
	e.body.WriteString(kdoc(s.Decl.Doc, ""))
	switch {
	case s.Decl.Enum != nil:
		e.enum(ctx, s, p.Name)
	case s.Alias != nil:
		fmt.Fprintf(&e.body, "typealias %s = %s\n", p.Name, e.typ(ctx, s, s.Alias, declName(s)))
	default:
		e.class(ctx, s, p.Name)
	}
	return nil
}

func (e *Emitter) End(ctx *gen.Context) error {
	if h := e.header(ctx.File); h != "" {
		ctx.W.Header(h)
	}
	if e.optIn {
		ctx.W.Header("@file:OptIn(ExperimentalSerializationApi::class)")
		e.imports["kotlinx.serialization.ExperimentalSerializationApi"] = struct{}{}
	}
	ctx.W.Header("package " + e.pkg)
	names := make([]string, 0, len(e.imports))
	for imp := range e.imports {
		names = append(names, imp)
	}
	slices.Sort(names)
	for _, imp := range names {
		ctx.W.Import("import " + imp)
	}
	ctx.W.WriteString(e.body.String())
	return nil
}

func declName(s *gen.Shape) string {
	return s.Decl.Pkg.Name + "." + s.Decl.Name + " (" + s.Mode.String() + ")"
}

func (e *Emitter) use(imp string) { e.imports[imp] = struct{}{} }

func (e *Emitter) class(ctx *gen.Context, s *gen.Shape, name string) {
	e.use("kotlinx.serialization.Serializable")
	switch {
	case s.Unknown == gen.Collect:
		e.ignoreUnknown()
		ctx.Report.Add(declName(s), "kotlinx.serialization has no catch-all map; undeclared keys are dropped")
	case s.Unknown == gen.Ignore, e.ignoreAll:
		e.ignoreUnknown()
	}
	if len(s.Fields) == 0 {
		// An object is a singleton, so == holds; a class with no members
		// would compare by identity and break every data class holding one.
		fmt.Fprintf(&e.body, "@Serializable\nobject %s\n", name)
		return
	}
	fmt.Fprintf(&e.body, "@Serializable\ndata class %s(\n", name)
	seen := map[string]string{} // property → the JSON name that took it
	for _, f := range s.Fields {
		where := declName(s) + "." + f.JSONName
		e.body.WriteString(kdoc(f.Doc, "    "))
		prop := names.LowerCamel(f.GoName)
		if first, dup := seen[prop]; dup {
			ctx.Report.Add(where, "fields "+first+" and "+f.JSONName+" both spell the property "+prop+"; rename one")
		}
		seen[prop] = f.JSONName
		typ := e.typ(ctx, s, f.Type, where)
		e.body.WriteString("    ")
		if f.JSONName != prop {
			e.use("kotlinx.serialization.SerialName")
			fmt.Fprintf(&e.body, "@SerialName(%s) ", quote(f.JSONName))
		}
		zero := ""
		if f.Optional {
			if zero = e.zero(f.Type, typ); zero == "" {
				typ, zero = strings.TrimSuffix(typ, "?")+"?", "null"
			}
		}
		fmt.Fprintf(&e.body, "val %s: %s", escape(prop), typ)
		if zero != "" {
			e.body.WriteString(" = ")
			e.body.WriteString(zero)
		}
		e.body.WriteString(",\n")
	}
	e.body.WriteString(")\n")
}

func (e *Emitter) ignoreUnknown() {
	e.use("kotlinx.serialization.json.JsonIgnoreUnknownKeys")
	e.optIn = true
	e.body.WriteString("@JsonIgnoreUnknownKeys\n")
}

func (e *Emitter) enum(ctx *gen.Context, s *gen.Shape, name string) {
	t := s.Alias
	if t.Wire != gen.WireString {
		ctx.Report.Add(declName(s), "kotlinx.serialization enums are JSON strings; integer values render as "+primitive(t))
		fmt.Fprintf(&e.body, "typealias %s = %s\n", name, primitive(t))
		return
	}
	e.use("kotlinx.serialization.Serializable")
	e.use("kotlinx.serialization.SerialName")
	fmt.Fprintf(&e.body, "@Serializable\nenum class %s {\n", name)
	seen := map[string]struct{}{}
	for _, v := range t.Enum.Values {
		entry := entryName(s.Decl.Name, v)
		if _, dup := seen[entry]; dup {
			ctx.Report.Add(declName(s), "two constants both spell the entry "+entry+"; rename one")
			continue
		}
		seen[entry] = struct{}{}
		fmt.Fprintf(&e.body, "    @SerialName(%s)\n    %s,\n", quote(v.Text), entry)
	}
	e.body.WriteString("}\n")
}

// typ renders t, marking it nullable when JSON null can occur.
func (e *Emitter) typ(ctx *gen.Context, s *gen.Shape, t *gen.Type, where string) string {
	out := e.base(ctx, s, t, where)
	if t.Nullable && out != "JsonElement" {
		out += "?"
	}
	return out
}

func (e *Emitter) base(ctx *gen.Context, s *gen.Shape, t *gen.Type, where string) string {
	if len(t.OneOf) > 0 {
		ctx.Report.Add(where, "a converter accepts several JSON shapes; rendered as JsonElement")
		e.use("kotlinx.serialization.json.JsonElement")
		return "JsonElement"
	}
	if t.External {
		if e.externalFor != nil {
			if typ, imports, ok := e.externalFor(ctx.File, e.placed, t); ok {
				for _, imp := range imports {
					e.use(imp)
				}
				return typ
			}
		}
		if ext, ok := e.external[t.Pkg.Path+"."+t.Name]; ok {
			for _, imp := range ext.imports {
				e.use(imp)
			}
			if ext.annotation != "" {
				return ext.annotation + " " + ext.typ
			}
			return ext.typ
		}
		if t.Wire != gen.WireString {
			ctx.Report.Add(where, t.GoType+" is external with an unknown shape; map it with kotlin.External or kotlin.ExternalFor")
			e.use("kotlinx.serialization.json.JsonElement")
			return "JsonElement"
		}
	}
	if t.Enum != nil && t.Ref == nil {
		ctx.Report.Add(where, fmt.Sprintf("a closed set of %d values is not a named type here; rendered as %s", len(t.Enum.Values), primitive(t)))
	}
	if t.Ref != nil {
		if pl, ok := ctx.Lookup(t.Ref, s.Mode); ok {
			if t.Enum == nil || !t.Enum.Zero {
				return e.ref(ctx, t.Ref, s.Mode, pl.Name)
			}
			ctx.Report.Add(where, "the enum admits Go's zero value, which no Kotlin enum entry holds; rendered as "+primitive(t)+" (Strict keeps the enum)")
		}
	}
	//exhaustive:ignore not every kind applies here
	switch t.Wire {
	case gen.WireString, gen.WireInteger, gen.WireNumber, gen.WireBool:
		if t.Go == gen.BigInt || t.Go == gen.Number {
			e.use("kotlinx.serialization.json.JsonPrimitive")
			return "JsonPrimitive"
		}
		return primitive(t)
	case gen.WireArray:
		if t.Go == gen.Array || (t.Go == gen.Bytes && t.Len > 0) {
			ctx.Report.Add(where, fmt.Sprintf("Kotlin has no fixed-length array: the %d-element limit Go enforces is dropped", t.Len))
		}
		return "List<" + e.typ(ctx, s, t.Elem, where) + ">"
	case gen.WireObject:
		if t.Go == gen.Map {
			return "Map<String, " + e.typ(ctx, s, t.Elem, where) + ">"
		}
	}
	e.use("kotlinx.serialization.json.JsonElement")
	return "JsonElement"
}

// primitive is the Kotlin type of a scalar by its wire and Go kind.
func primitive(t *gen.Type) string {
	//exhaustive:ignore not every kind applies here
	switch t.Wire {
	case gen.WireString:
		return "String"
	case gen.WireBool:
		return "Boolean"
	case gen.WireNumber:
		if t.Go == gen.Float32 {
			return "Float"
		}
		return "Double"
	}
	return map[gen.GoKind]string{
		gen.Int8: "Byte", gen.Int16: "Short", gen.Int32: "Int", gen.Int: "Long", gen.Int64: "Long",
		gen.Uint8: "UByte", gen.Uint16: "UShort", gen.Uint32: "UInt", gen.Uint: "ULong", gen.Uint64: "ULong",
		gen.Time: "Long", gen.Duration: "Long",
	}[t.Go]
}

// zero is the Kotlin literal of Go's zero value for a rendered type, which is
// what an absent key decodes to on the Go side; "" for a class or enum type,
// which has no such literal.
// zero is the Kotlin literal of what Go's zero value encodes to, which is
// what an absent key means on the Go side. kotlinx omits a default-valued
// property, so the two agree in both directions. It returns "" when the wire
// form of the zero is not a literal of the rendered type — a formatted time
// ("0001-01-01"), a duration ("0s"), a fixed-size byte array — and the field
// becomes nullable instead, since a wrong default is worse than a null.
func (e *Emitter) zero(t *gen.Type, typ string) string {
	if strings.HasSuffix(typ, "?") {
		return "null"
	}
	//exhaustive:ignore not every kind applies here
	switch t.Go {
	case gen.Time, gen.Duration, gen.IP, gen.Addr, gen.Prefix, gen.URL, gen.BigInt, gen.BigFloat, gen.BigRat:
		return "" // a formatted zero is not the type's own zero literal
	case gen.Bytes:
		if t.Len > 0 || t.Wire == gen.WireString {
			return "" // base64 of N zero bytes, not ""
		}
	case gen.Array:
		return "" // a fixed-length array encodes N zero elements, not []
	}
	if t.Enum != nil && t.Ref != nil {
		return "" // an enum class has no member for Go's zero value
	}
	switch {
	case strings.HasPrefix(typ, "List<"):
		return "emptyList()"
	case strings.HasPrefix(typ, "Map<"):
		return "emptyMap()"
	}
	switch typ {
	case "String":
		return `""`
	case "Boolean":
		return "false"
	case "Byte", "Short", "Int":
		return "0"
	case "Long":
		return "0L"
	case "UByte", "UShort", "UInt":
		return "0u"
	case "ULong":
		return "0uL"
	case "Float":
		return "0f"
	case "Double":
		return "0.0"
	case "JsonElement":
		e.use("kotlinx.serialization.json.JsonNull")
		return "JsonNull"
	}
	return ""
}

// ref returns the name for a placed shape, importing it from another
// package when needed.
func (e *Emitter) ref(ctx *gen.Context, d *gen.TypeDecl, mode gen.Mode, name string) string {
	f := ctx.FileOf(d, mode)
	other, ok := f.Emitter().(*Emitter)
	if !ok || other.pkg == e.pkg {
		return name
	}
	qualified := other.pkg + "." + name
	if local, ok := e.aliases[qualified]; ok {
		return local
	}
	local := name
	if _, clash := e.locals[local]; clash {
		last := other.pkg[strings.LastIndexByte(other.pkg, '.')+1:]
		local = upperFirst(last) + name
		e.use(qualified + " as " + local)
	} else {
		e.use(qualified)
	}
	e.aliases[qualified] = local
	e.locals[local] = struct{}{}
	return local
}

// entryName is an enum entry: RoleAdmin in type Role → ADMIN, or the value
// when the constant name is unknown.
func entryName(typeName string, v gen.EnumValue) string {
	base := strings.TrimPrefix(v.Name, typeName)
	if base == "" {
		base = strings.Trim(v.Value, `"`)
	}
	var b strings.Builder
	rs := []rune(base)
	for i, c := range rs {
		switch {
		case unicode.IsUpper(c) && i > 0 && (unicode.IsLower(rs[i-1]) || (i+1 < len(rs) && unicode.IsLower(rs[i+1]))):
			b.WriteByte('_')
			b.WriteRune(c)
		case unicode.IsLetter(c) || unicode.IsDigit(c):
			b.WriteRune(unicode.ToUpper(c))
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" || unicode.IsDigit(rune(out[0])) {
		out = "V_" + out
	}
	return out
}

var keywords = map[string]struct{}{
	"as": {}, "break": {}, "class": {}, "continue": {}, "do": {}, "else": {}, "false": {}, "for": {},
	"fun": {}, "if": {}, "in": {}, "interface": {}, "is": {}, "null": {}, "object": {}, "package": {},
	"return": {}, "super": {}, "this": {}, "throw": {}, "true": {}, "try": {}, "typealias": {},
	"typeof": {}, "val": {}, "var": {}, "when": {}, "while": {},
}

func isIdent(s string) bool {
	_, kw := keywords[s]
	return !kw && names.Ident(s)
}

func escape(name string) string {
	if _, kw := keywords[name]; kw {
		return "`" + name + "`"
	}
	return name
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '\\' || r == '"' || r == '$':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func kdoc(text, indent string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	text = strings.ReplaceAll(text, "*/", `*\/`)
	lines := strings.Split(text, "\n")
	if len(lines) == 1 {
		return indent + "/** " + lines[0] + " */\n"
	}
	var b strings.Builder
	b.WriteString(indent)
	b.WriteString("/**\n")
	for _, l := range lines {
		b.WriteString(strings.TrimRight(indent+" * "+l, " "))
		b.WriteByte('\n')
	}
	b.WriteString(indent)
	b.WriteString(" */\n")
	return b.String()
}
