// Package swift renders gen shapes as Swift Codable types.
package swift

import (
	"fmt"
	"slices"
	"strconv"
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

// Public declares every type, property and initializer public, for types
// that live in a library module, and makes a NewRuntime file public too.
func Public() Option {
	return func(e *Emitter) {
		e.public = func(*gen.File, gen.Placed) bool { return true }
		e.publicRuntime = true
	}
}

// PublicFor decides per placed type whether it and its members are public.
func PublicFor(fn func(f *gen.File, p gen.Placed) bool) Option {
	return func(e *Emitter) { e.public = fn }
}

// External renders the Go type goPath ("github.com/google/uuid.UUID") as the
// Swift type swiftType, importing modules in files that use it.
func External(goPath, swiftType string, modules ...string) Option {
	return func(e *Emitter) { e.external[goPath] = external{swiftType, modules} }
}

// Runtime names the file created with NewRuntime, which holds JSONValue for
// values of any shape and the Indirect property wrapper.
func Runtime(f *gen.File) Option { return func(e *Emitter) { e.runtime = f } }

// Protocols sets what every struct and enum conforms to besides Codable.
// Default Hashable, which Swift synthesizes along with Equatable. Every
// mapped External type must conform too; Protocols() drops them all.
func Protocols(names ...string) Option {
	return ProtocolsFor(func(*gen.File, gen.Placed) []string { return names })
}

// ProtocolsFor decides the protocols besides Codable per placed type. Like
// every ...For option, fn sees the output file (f.Path), the placed name, and
// the Go type through p.Shape.Decl: its package, source file, name and
// directives.
func ProtocolsFor(fn func(f *gen.File, p gen.Placed) []string) Option {
	return func(e *Emitter) { e.protocols = fn }
}

// Indirect names the property wrapper that boxes each field closing a
// reference cycle, so recursive types stay structs. The default, Indirect,
// comes from the NewRuntime file; a wrapper of your own must decode an
// absent key into a nil Optional. With "", a recursive type is a final class
// conforming to Codable alone, left for you to extend.
func Indirect(wrapper string) Option {
	return IndirectFor(func(*gen.File, gen.Placed) string { return wrapper })
}

// IndirectFor decides the wrapper, or "" for a final class, per placed
// recursive type.
func IndirectFor(fn func(f *gen.File, p gen.Placed) string) Option {
	return func(e *Emitter) { e.indirect = fn }
}

// ExternalFor renders an external type t, used in placed type p, as
// swiftType, importing modules, when fn returns ok, before External.
func ExternalFor(fn func(f *gen.File, p gen.Placed, t *gen.Type) (swiftType string, modules []string, ok bool)) Option {
	return func(e *Emitter) { e.externalFor = fn }
}

type external struct {
	typ     string
	modules []string
}

// Emitter renders Swift Codable structs, classes and enums. Every file it
// renders belongs to the same module.
type Emitter struct {
	module        string
	header        func(*gen.File) string
	public        func(*gen.File, gen.Placed) bool
	publicRuntime bool
	external      map[string]external
	externalFor   func(*gen.File, gen.Placed, *gen.Type) (string, []string, bool)
	runtime       *gen.File
	protocols     func(*gen.File, gen.Placed) []string
	indirect      func(*gen.File, gen.Placed) string

	placed  gen.Placed // the shape Decl is rendering
	imports map[string]struct{}
	locals  map[string]struct{}
	body    strings.Builder
}

// New returns an emitter whose files belong to module.
func New(module string, opts ...Option) *Emitter {
	e := &Emitter{module: module, external: map[string]external{}}
	for _, o := range []Option{Header(""), PublicFor(func(*gen.File, gen.Placed) bool { return false }), Protocols("Hashable"), Indirect("Indirect")} {
		o(e)
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

func (e *Emitter) Begin(ctx *gen.Context) error {
	e.imports = map[string]struct{}{}
	e.locals = map[string]struct{}{}
	for _, name := range [...]string{"JSONValue", "Indirect", "Data", "Decimal"} {
		e.locals[name] = struct{}{} // the runtime's and Foundation's own names
	}
	for _, p := range ctx.File.Placed() {
		if _, taken := e.locals[p.Name]; taken && p.Name != "Data" && p.Name != "Decimal" {
			// a placed name wins over the runtime's, but then the runtime
			// type it shadows is unreachable from this file
			ctx.Report.Add(declName(p.Shape), "shadows the runtime type "+p.Name)
		}
		e.locals[p.Name] = struct{}{}
	}
	e.body.Reset()
	return nil
}

func (e *Emitter) Decl(ctx *gen.Context, p gen.Placed) error {
	if !isIdent(p.Name) {
		return fmt.Errorf("%q is not a valid Swift type name", p.Name)
	}
	e.placed = p
	s := p.Shape
	if e.body.Len() > 0 {
		e.body.WriteByte('\n')
	}
	e.body.WriteString(doc(s.Decl.Doc, ""))
	switch {
	case s.Decl.Enum != nil:
		e.enum(ctx, p)
	case s.Alias != nil:
		fmt.Fprintf(&e.body, "%stypealias %s = %s\n", e.access(ctx, p), p.Name, e.base(ctx, s, s.Alias, declName(s)))
	default:
		e.object(ctx, p)
	}
	return nil
}

func (e *Emitter) End(ctx *gen.Context) error {
	if h := e.header(ctx.File); h != "" {
		ctx.W.Header(h)
	}
	names := make([]string, 0, len(e.imports))
	for m := range e.imports {
		names = append(names, m)
	}
	slices.Sort(names)
	for _, m := range names {
		ctx.W.Import("import " + m)
	}
	ctx.W.WriteString(e.body.String())
	return nil
}

func (e *Emitter) access(ctx *gen.Context, p gen.Placed) string {
	if e.public(ctx.File, p) {
		return "public "
	}
	return ""
}

func declName(s *gen.Shape) string {
	return s.Decl.Pkg.Name + "." + s.Decl.Name + " (" + s.Mode.String() + ")"
}

type property struct {
	name, jsonName, typ, doc string
	optional, boxed          bool
}

// conformances is the protocol list of a placed struct or enum.
func (e *Emitter) conformances(ctx *gen.Context, p gen.Placed, first ...string) string {
	return strings.Join(slices.Concat(first, []string{"Codable"}, e.protocols(ctx.File, p)), ", ")
}

// closesCycle reports whether a field holds a recursive struct inline, which
// a struct cannot do without a box. Arrays and maps already store elsewhere,
// and a class is a reference.
func closesCycle(ctx *gen.Context, s *gen.Shape, t *gen.Type) bool {
	if t.Ref == nil || t.Ref.Enum != nil {
		return false
	}
	pl, ok := ctx.Lookup(t.Ref, s.Mode)
	if !ok || !pl.Shape.Recursive || pl.Shape.Alias != nil {
		return false
	}
	f := ctx.FileOf(t.Ref, s.Mode)
	other, ok := f.Emitter().(*Emitter)
	return !ok || other.indirect(f, pl) != ""
}

func (e *Emitter) object(ctx *gen.Context, pl gen.Placed) {
	s, name := pl.Shape, pl.Name
	if s.Unknown == gen.Collect {
		ctx.Report.Add(declName(s), "JSONDecoder has no catch-all map; undeclared keys are dropped")
	}
	wrapper := e.indirect(ctx.File, pl)
	access := e.access(ctx, pl)
	props := make([]property, len(s.Fields))
	for i, f := range s.Fields {
		where := declName(s) + "." + f.JSONName
		t := e.typ(ctx, s, f.Type, where)
		if f.Optional && !strings.HasSuffix(t, "?") {
			t += "?"
		}
		if !f.Optional && f.Type.Nullable && s.Mode == gen.In && t != "JSONValue" {
			ctx.Report.Add(where, "the key is required, but JSONEncoder leaves out a nil value")
		}
		props[i] = property{names.LowerCamel(f.GoName), f.JSONName, t, f.Doc, strings.HasSuffix(t, "?"), false}
		for _, prev := range props[:i] {
			if prev.name == props[i].name {
				ctx.Report.Add(where, "fields "+prev.jsonName+" and "+f.JSONName+" both spell the property "+prev.name+"; rename one")
			}
		}
		if s.Recursive && wrapper != "" && closesCycle(ctx, s, f.Type) {
			props[i].boxed = true
			if wrapper == "Indirect" {
				e.fromRuntime(ctx, where, "Indirect")
			}
		}
	}
	class := s.Recursive && wrapper == ""
	if class {
		if protos := e.protocols(ctx.File, pl); len(protos) > 0 {
			ctx.Report.Add(declName(s), "a class conforms to Codable alone: Swift synthesizes "+strings.Join(protos, ", ")+" for a struct only, so write them in an extension")
			if slices.Contains(protos, "Sendable") {
				ctx.Report.Add(declName(s), "Sendable is not one of them: a class whose properties are mutable cannot conform to it at all")
			}
		}
		fmt.Fprintf(&e.body, "%sfinal class %s: Codable {\n", access, name)
	} else {
		fmt.Fprintf(&e.body, "%sstruct %s: %s {\n", access, name, e.conformances(ctx, pl))
	}
	renamed := false
	for _, p := range props {
		e.body.WriteString(doc(p.doc, "    "))
		e.body.WriteString("    ")
		if p.boxed {
			fmt.Fprintf(&e.body, "@%s ", wrapper)
		}
		fmt.Fprintf(&e.body, "%svar %s: %s", access, escape(p.name), p.typ)
		if p.boxed && p.optional {
			e.body.WriteString(" = nil")
		}
		e.body.WriteByte('\n')
		renamed = renamed || p.name != p.jsonName
	}
	if class || access != "" {
		e.init(access, props)
	}
	if renamed {
		e.body.WriteString("\n    enum CodingKeys: String, CodingKey {\n")
		for _, p := range props {
			fmt.Fprintf(&e.body, "        case %s", escape(p.name))
			if p.name != p.jsonName {
				fmt.Fprintf(&e.body, " = %s", quote(p.jsonName))
			}
			e.body.WriteByte('\n')
		}
		e.body.WriteString("    }\n")
	}
	e.body.WriteString("}\n")
}

// init writes the memberwise initializer a class or a public struct lacks.
func (e *Emitter) init(access string, props []property) {
	// A keyword-named property takes a label plus a plain internal name:
	// `init(\`self\` selfValue: T)`, since the parameter would otherwise
	// shadow self in the body.
	params := make([]string, len(props))
	args := make([]string, len(props))
	for i, p := range props {
		args[i] = p.name
		params[i] = escape(p.name) + ": " + p.typ
		if isKeyword(p.name) {
			args[i] = p.name + "Value"
			params[i] = escape(p.name) + " " + args[i] + ": " + p.typ
		}
		if p.optional {
			params[i] += " = nil"
		}
	}
	fmt.Fprintf(&e.body, "\n    %sinit(%s) {\n", access, strings.Join(params, ", "))
	for i, p := range props {
		fmt.Fprintf(&e.body, "        self.%s = %s\n", escape(p.name), args[i])
	}
	e.body.WriteString("    }\n")
}

func (e *Emitter) enum(ctx *gen.Context, pl gen.Placed) {
	s, name := pl.Shape, pl.Name
	t := s.Alias
	fmt.Fprintf(&e.body, "%senum %s: %s {\n", e.access(ctx, pl), name, e.conformances(ctx, pl, primitive(t)))
	seen := map[string]struct{}{}
	for _, v := range t.Enum.Values {
		c := caseName(s.Decl.Name, v)
		if _, dup := seen[c]; dup {
			ctx.Report.Add(declName(s), "two constants both spell the case "+c+"; rename one")
			continue
		}
		seen[c] = struct{}{}
		raw := v.Value
		if t.Wire == gen.WireString {
			raw = quote(v.Text)
			if v.Text == c {
				raw = ""
			}
		}
		fmt.Fprintf(&e.body, "    case %s", escape(c))
		if raw != "" {
			fmt.Fprintf(&e.body, " = %s", raw)
		}
		e.body.WriteByte('\n')
	}
	e.body.WriteString("}\n")
}

// typ renders t, optional when JSON null can occur. A typealias never is
// optional itself: a reference to it is, when its type admits null.
func (e *Emitter) typ(ctx *gen.Context, s *gen.Shape, t *gen.Type, where string) string {
	out := e.base(ctx, s, t, where)
	nullable := t.Nullable
	if t.Ref != nil {
		if pl, ok := ctx.Lookup(t.Ref, s.Mode); ok && pl.Shape.Alias != nil {
			nullable = nullable || pl.Shape.Alias.Nullable
		}
	}
	if nullable && out != "JSONValue" {
		out += "?"
	}
	return out
}

func (e *Emitter) base(ctx *gen.Context, s *gen.Shape, t *gen.Type, where string) string {
	if len(t.OneOf) > 0 {
		ctx.Report.Add(where, "a converter accepts several JSON shapes; rendered as JSONValue")
		return e.jsonValue(ctx, where)
	}
	if t.External {
		if e.externalFor != nil {
			if typ, modules, ok := e.externalFor(ctx.File, e.placed, t); ok {
				for _, m := range modules {
					e.imports[m] = struct{}{}
				}
				return typ
			}
		}
		if ext, ok := e.external[t.Pkg.Path+"."+t.Name]; ok {
			for _, m := range ext.modules {
				e.imports[m] = struct{}{}
			}
			return ext.typ
		}
		if t.Wire != gen.WireString {
			ctx.Report.Add(where, t.GoType+" is external with an unknown shape; map it with swift.External or swift.ExternalFor")
			return e.jsonValue(ctx, where)
		}
	}
	if t.Enum != nil && t.Ref == nil {
		ctx.Report.Add(where, "a closed set of "+strconv.Itoa(len(t.Enum.Values))+" values is not a named type here; rendered as "+primitive(t))
	}
	if t.Ref != nil {
		if pl, ok := ctx.Lookup(t.Ref, s.Mode); ok {
			if t.Enum == nil || !t.Enum.Zero {
				return e.ref(ctx, t.Ref, s.Mode, pl.Name)
			}
			ctx.Report.Add(where, "the enum admits Go's zero value, which no Swift enum case holds; rendered as "+primitive(t)+" (Strict keeps the enum)")
		}
	}
	switch t.Wire {
	case gen.WireString:
		if t.Go == gen.Bytes && t.Format == gen.FormatBase64 {
			e.imports["Foundation"] = struct{}{}
			return "Data"
		}
		return "String"
	case gen.WireInteger, gen.WireNumber:
		if t.Go == gen.BigInt || t.Go == gen.Number {
			e.imports["Foundation"] = struct{}{}
			return "Decimal"
		}
		return primitive(t)
	case gen.WireBool:
		return "Bool"
	case gen.WireArray:
		if t.Go == gen.Array || (t.Go == gen.Bytes && t.Len > 0) {
			ctx.Report.Add(where, fmt.Sprintf("Swift has no fixed-length array: the %d-element limit Go enforces is dropped", t.Len))
		}
		return "[" + e.typ(ctx, s, t.Elem, where) + "]"
	case gen.WireObject:
		if t.Go == gen.Map {
			return "[String: " + e.typ(ctx, s, t.Elem, where) + "]"
		}
	}
	return e.jsonValue(ctx, where)
}

func (e *Emitter) jsonValue(ctx *gen.Context, where string) string {
	return e.fromRuntime(ctx, where, "JSONValue")
}

// checkRuntimeAccess reports a public type whose runtime file is internal:
// Swift refuses a public property whose type, or whose property wrapper, is
// less visible.
func (e *Emitter) checkRuntimeAccess(ctx *gen.Context, pl gen.Placed, name string) {
	if !e.public(ctx.File, pl) || e.runtime == nil {
		return
	}
	if r, ok := e.runtime.Emitter().(runtime); ok && !r.public {
		ctx.Report.Add(declName(pl.Shape), "is public but "+name+" comes from an internal runtime file: pass swift.Public() to swift.NewRuntime too")
	}
}

// fromRuntime returns name, a declaration of the NewRuntime file, importing
// its module when it is another one.
func (e *Emitter) fromRuntime(ctx *gen.Context, where, name string) string {
	if e.runtime == nil {
		ctx.Report.Add(where, "uses "+name+", which needs a swift.NewRuntime file passed with swift.Runtime")
		return name
	}
	if other, ok := e.runtime.Emitter().(runtime); ok {
		if other.module != e.module {
			e.imports[other.module] = struct{}{}
		}
		if !other.public && e.public(ctx.File, e.placed) {
			ctx.Report.Add(where, "is public but "+name+" comes from an internal runtime file: pass swift.Public() to swift.NewRuntime too")
		}
	}
	return name
}

// primitive is the Swift type of a scalar by its wire and Go kind.
func primitive(t *gen.Type) string {
	switch t.Wire {
	case gen.WireString:
		return "String"
	case gen.WireBool:
		return "Bool"
	case gen.WireNumber:
		if t.Go == gen.Float32 {
			return "Float"
		}
		return "Double"
	}
	if p, ok := map[gen.GoKind]string{
		gen.Int8: "Int8", gen.Int16: "Int16", gen.Int32: "Int32", gen.Int: "Int", gen.Int64: "Int64",
		gen.Uint8: "UInt8", gen.Uint16: "UInt16", gen.Uint32: "UInt32", gen.Uint: "UInt", gen.Uint64: "UInt64",
	}[t.Go]; ok {
		return p
	}
	return "Int64"
}

// ref returns the name for a placed shape, importing its module and
// qualifying the name on a clash when it lives in another one.
func (e *Emitter) ref(ctx *gen.Context, d *gen.TypeDecl, mode gen.Mode, name string) string {
	f := ctx.FileOf(d, mode)
	other, ok := f.Emitter().(*Emitter)
	if !ok || other.module == e.module {
		return name
	}
	e.imports[other.module] = struct{}{}
	if pl, found := ctx.Lookup(d, mode); found && !other.public(f, pl) {
		ctx.Report.Add(d.Pkg.Name+"."+d.Name, "is referenced from module "+e.module+" but is not public in "+other.module)
	}
	if _, clash := e.locals[name]; clash {
		return other.module + "." + name
	}
	e.locals[name] = struct{}{} // another module cannot export it again unqualified
	return name
}

// caseName is an enum case: RoleAdmin in type Role → admin, or the value in
// lowerCamel when the constant name does not extend the type name.
func caseName(typeName string, v gen.EnumValue) string {
	base, ok := strings.CutPrefix(v.Name, typeName)
	if !ok || base == "" {
		base = strings.Trim(v.Value, `"`)
	}
	var b strings.Builder
	upper := false
	for _, c := range base {
		switch {
		case unicode.IsLetter(c) || unicode.IsDigit(c):
			if upper && b.Len() > 0 {
				c = unicode.ToUpper(c)
			}
			b.WriteRune(c)
			upper = false
		default:
			upper = true
		}
	}
	out := names.LowerCamel(b.String())
	if out == "" || unicode.IsDigit(rune(out[0])) {
		out = "_" + out
	}
	return out
}

var keywords = map[string]struct{}{
	"associatedtype": {}, "class": {}, "deinit": {}, "enum": {}, "extension": {}, "fileprivate": {}, "func": {},
	"import": {}, "init": {}, "inout": {}, "internal": {}, "let": {}, "open": {}, "operator": {}, "private": {},
	"precedencegroup": {}, "protocol": {}, "public": {}, "rethrows": {}, "static": {}, "struct": {},
	"subscript": {}, "typealias": {}, "var": {}, "break": {}, "case": {}, "catch": {}, "continue": {},
	"default": {}, "defer": {}, "do": {}, "else": {}, "fallthrough": {}, "for": {}, "guard": {}, "if": {},
	"in": {}, "repeat": {}, "return": {}, "throw": {}, "switch": {}, "where": {}, "while": {}, "Any": {},
	"as": {}, "await": {}, "false": {}, "is": {}, "nil": {}, "self": {}, "Self": {}, "super": {}, "throws": {},
	"true": {}, "try": {},
}

func isIdent(s string) bool {
	_, kw := keywords[s]
	return !kw && names.Ident(s)
}

func isKeyword(name string) bool {
	_, kw := keywords[name]
	return kw
}

func escape(name string) string {
	if _, kw := keywords[name]; kw {
		return "`" + name + "`"
	}
	return name
}

// quote renders s as a Swift string literal.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range s {
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteRune(c)
		case c == '\n':
			b.WriteString(`\n`)
		case c < 0x20 || c == 0x7f:
			b.WriteString(`\u{` + strconv.FormatInt(int64(c), 16) + `}`)
		default:
			b.WriteRune(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func doc(text, indent string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	var b strings.Builder
	for l := range strings.SplitSeq(text, "\n") {
		b.WriteString(strings.TrimRight(indent+"/// "+l, " "))
		b.WriteByte('\n')
	}
	return b.String()
}

// NewRuntime returns the emitter of a file declaring, in module, JSONValue, a
// Codable JSON value of any shape, and Indirect, the property wrapper that
// boxes recursive fields. Pass the file to swift.Runtime.
func NewRuntime(module string, opts ...Option) gen.Emitter {
	e := New(module, opts...)
	return runtime{module, e.header, e.publicRuntime}
}

type runtime struct {
	module string
	header func(*gen.File) string
	public bool
}

func (r runtime) Begin(ctx *gen.Context) error {
	if h := r.header(ctx.File); h != "" {
		ctx.W.Header(h)
	}
	return nil
}

func (runtime) Decl(*gen.Context, gen.Placed) error {
	return fmt.Errorf("a runtime file holds no types")
}

func (r runtime) End(ctx *gen.Context) error {
	access := ""
	if r.public {
		access = "public "
	}
	ctx.W.WriteString(strings.ReplaceAll(jsonValue+"\n"+indirect, "ACCESS ", access))
	return nil
}

const jsonValue = `/// A JSON value of any shape. An integer keeps its own
/// case: Double would round anything past 2^53, which Go reads exactly.
ACCESS enum JSONValue: Codable, Hashable, Sendable {
    case null
    case bool(Bool)
    case int(Int64)
    case uint(UInt64)
    case number(Double)
    case string(String)
    case array([JSONValue])
    case object([String: JSONValue])

    ACCESS init(from decoder: any Decoder) throws {
        if let c = try? decoder.container(keyedBy: JSONKey.self) {
            var object: [String: JSONValue] = [:]
            for key in c.allKeys {
                object[key.stringValue] = try c.decode(JSONValue.self, forKey: key)
            }
            self = .object(object)
            return
        }
        if var c = try? decoder.unkeyedContainer() {
            var array: [JSONValue] = []
            while !c.isAtEnd {
                array.append(try c.decode(JSONValue.self))
            }
            self = .array(array)
            return
        }
        let c = try decoder.singleValueContainer()
        if c.decodeNil() {
            self = .null
        } else if let b = try? c.decode(Bool.self) {
            self = .bool(b)
        } else if let s = try? c.decode(String.self) {
            self = .string(s)
        } else if let i = try? c.decode(Int64.self) {
            self = .int(i)
        } else if let u = try? c.decode(UInt64.self) {
            self = .uint(u)
        } else {
            // Not nil, not a bool, string or integer: a double, and a value
            // too large for one is an error rather than a silent null.
            self = .number(try c.decode(Double.self))
        }
    }

    ACCESS func encode(to encoder: any Encoder) throws {
        var c = encoder.singleValueContainer()
        switch self {
        case .null: try c.encodeNil()
        case .bool(let b): try c.encode(b)
        case .int(let i): try c.encode(i)
        case .uint(let u): try c.encode(u)
        case .number(let n): try c.encode(n)
        case .string(let s): try c.encode(s)
        case .array(let a): try c.encode(a)
        case .object(let o): try c.encode(o)
        }
    }
}

private struct JSONKey: CodingKey {
    var stringValue: String
    var intValue: Int? { nil }
    init(stringValue: String) { self.stringValue = stringValue }
    init?(intValue: Int) { nil }
}
`

const indirect = `/// Indirect boxes a value so a struct can hold its own type.
@propertyWrapper
ACCESS enum Indirect<Value> {
    indirect case value(Value)

    ACCESS init(wrappedValue: Value) {
        self = .value(wrappedValue)
    }

    ACCESS var wrappedValue: Value {
        get { switch self { case .value(let v): v } }
        set { self = .value(newValue) }
    }
}

extension Indirect: Equatable where Value: Equatable {}
extension Indirect: Hashable where Value: Hashable {}
extension Indirect: Sendable where Value: Sendable {}

extension Indirect: Codable where Value: Codable {
    ACCESS init(from decoder: any Decoder) throws {
        self.init(wrappedValue: try Value(from: decoder))
    }

    ACCESS func encode(to encoder: any Encoder) throws {
        try wrappedValue.encode(to: encoder)
    }
}

extension KeyedDecodingContainer {
    ACCESS func decode<T: Codable>(_: Indirect<T?>.Type, forKey key: Key) throws -> Indirect<T?> {
        Indirect(wrappedValue: try decodeIfPresent(T.self, forKey: key))
    }
}

extension KeyedEncodingContainer {
    ACCESS mutating func encode<T: Codable>(_ value: Indirect<T?>, forKey key: Key) throws {
        try encodeIfPresent(value.wrappedValue, forKey: key)
    }
}
`
