package gen

import (
	"encoding/json"
	"fmt"
	"go/types"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/sirkostya009/ggen/gen/model"
)

// Shape is a TypeDecl seen in one direction. It is plain data: a script may
// edit it before placing it.
type Shape struct {
	Decl      *TypeDecl
	Mode      Mode
	Fields    []*Field // a struct's declared keys, nil for an alias or enum type
	Alias     *Type    // the underlying type of an alias or enum type, nil for a struct
	Recursive bool     // reaches itself through references
	Unknown   Unknown
	Rest      *Type // value type of undeclared keys when Unknown is Collect

	errs []error // surfaced by File.Place
}

// Err records a problem with this shape. File.Place surfaces it, and Write
// then fails. A script that edits a shape may use it too.
func (s *Shape) Err(err error) { s.errs = append(s.errs, err) }

// Field is one declared key.
type Field struct {
	Shape      *Shape
	GoName     string
	JSONName   string
	Doc        string
	Directives []string
	Type       *Type
	// Optional means the key may be absent in input (no `required`). In
	// output, it means the encoder may leave it out (omitempty, omitzero).
	Optional bool
}

// Type is a value's type: its Go side and its JSON side.
type Type struct {
	GoType string   // Go spelling relative to the declaring package: "[]string", "*User", "uuid.UUID"
	Name   string   // declared name of a named type ("Role", "Time"), empty for an unnamed type
	Pkg    *Package // package of a named type, nil for an unnamed or predeclared type
	File   string   // declaring file of a named type in a loaded package

	Go     GoKind
	Wire   Wire
	Format string // one of the Format constants, or ""

	Elem *Type // Slice or Array element, Map value, or a byte of Bytes under format:array
	Key  *Type // Map key
	Len  int   // Array length, or for Bytes the decoded length of a [N]byte (0 for []byte)

	Ref      *TypeDecl // a generated type or an enum type. An emitter may inline Enum instead of an enum Ref.
	External bool      // a named type ggen does not generate, identified by Pkg.Path + "." + Name

	Rules []Rule  // input only, in declared order, applied to non-null values
	OneOf []*Type // input only: other shapes a converter accepts in place of this one
	Enum  *Enum   // closed set of values

	// Converted means a converter turns this JSON shape into the Go value, so
	// Rules describe the converted value and do not constrain this shape.
	Converted bool
	Nullable  bool // JSON null is accepted (input) or can be emitted (output)
	Empty     bool // "" is accepted or can be emitted for a formatted string (IP, Addr, Prefix, IPNet, TCPAddr, UDPAddr, URL)
}

// Formats a Type can carry.
const (
	FormatDateTime     = "date-time"      // RFC 3339 string
	FormatUnix         = "unix"           // number of seconds, may be fractional
	FormatUnixMilli    = "unixmilli"      // integer
	FormatUnixMicro    = "unixmicro"      // integer
	FormatUnixNano     = "unixnano"       // integer
	FormatDuration     = "duration"       // Go duration string, "1h30m"
	FormatDurationSec  = "duration-sec"   // number of seconds, may be fractional
	FormatDurationMs   = "duration-milli" // integer
	FormatDurationUs   = "duration-micro" // integer
	FormatDurationNs   = "duration-nano"  // integer
	FormatBase64       = "base64"
	FormatBase64URL    = "base64url"
	FormatBase32       = "base32"
	FormatBase32Hex    = "base32hex"
	FormatHex          = "hex"
	FormatIP           = "ip"      // IPv4 or IPv6, no zone
	FormatIPZone       = "ip-zone" // IPv4, or IPv6 with an optional %zone
	FormatCIDR         = "cidr"
	FormatIPPort       = "ip-port"       // "ip:port" with a bracketed IPv6 address and an optional %zone, or ":port"
	FormatURI          = "uri"           // anything url.Parse accepts
	FormatIntString    = "int-string"    // `,string` signed integer
	FormatUintString   = "uint-string"   // `,string` unsigned integer
	FormatNumberString = "number-string" // `,string` float
	FormatBigFloat     = "big-float"     // math/big.Float: a decimal with an e or p exponent, or Inf
	FormatRational     = "rational"      // math/big.Rat: an integer or decimal with base prefixes, or "num/denom"
	// FormatTimeLayoutPrefix starts a custom time layout: "time:" followed by
	// the Go layout, e.g. "time:2006-01-02".
	FormatTimeLayoutPrefix = "time:"
)

// Enum is a closed set of values.
type Enum struct {
	Decl   *TypeDecl   // the named type whose constants these are, nil for a oneof
	Values []EnumValue // declaration order
	// Zero means the Go zero value ("" or 0) can be emitted in output although
	// it is not one of Values.
	Zero bool
}

// EnumValue is one member.
type EnumValue struct {
	Name  string // constant name, "RoleAdmin", empty for a oneof
	Value string // JSON literal: `"admin"`, `2`
	// Text is the decoded value of a string constant, so a target can quote
	// it its own way. It is empty for a number.
	Text string
}

// Rule is one step of a `pipe:` tag.
type Rule struct {
	Op        Op
	Args      []string // parsed arguments: ["3"], ["a", "c d"], ["_", "-"]
	Func      string   // for Op Func, the reference as written without "@": "IsAdult" or "pkg.IsAdult"
	FuncPkg   string   // for Op Func, the import path of the function's package
	Msg       string   // inline message of a bool-form @Func
	Transform bool     // changes the value instead of checking it
}

// In builds the input shape: what the generated decoder accepts.
func (t *TypeDecl) In() *Shape { return t.shape(In) }

// Out builds the output shape: what the generated encoder emits.
func (t *TypeDecl) Out() *Shape { return t.shape(Out) }

// Field returns the field with the JSON name, or nil.
func (s *Shape) Field(jsonName string) *Field {
	for _, f := range s.Fields {
		if f.JSONName == jsonName {
			return f
		}
	}
	return nil
}

// Strict clears every zero-value widening of an output shape at every depth:
// Nullable, Empty and Enum.Zero. Optional is untouched. It is a no-op on an
// input shape.
func (s *Shape) Strict() *Shape {
	if s.Mode != Out {
		return s
	}
	for _, f := range s.Fields {
		strict(f.Type)
	}
	strict(s.Alias)
	strict(s.Rest)
	return s
}

// StrictFields is Strict for the named fields only.
func (s *Shape) StrictFields(jsonNames ...string) *Shape {
	if s.Mode != Out {
		return s
	}
	for _, name := range jsonNames {
		f := s.Field(name)
		if f == nil {
			s.Err(fmt.Errorf("StrictFields: %s has no field %q", declName(s.Decl), name))
			continue
		}
		strict(f.Type)
	}
	return s
}

func strict(t *Type) {
	if t == nil {
		return
	}
	t.Nullable = false
	t.Empty = false
	if t.Enum != nil {
		t.Enum.Zero = false
	}
	strict(t.Elem)
	strict(t.Key)
	for _, o := range t.OneOf {
		strict(o)
	}
}

func (t *TypeDecl) shape(m Mode) *Shape {
	s := &Shape{Decl: t, Mode: m, Recursive: t.recursive}
	if t.missing {
		s.Err(fmt.Errorf("%s: not loaded (no such type in the loaded packages)", declName(t)))
		return s
	}
	l := &lowerer{set: t.set, decl: t, mode: m}
	if t.named != nil {
		s.Alias = l.enumAlias()
		return s
	}
	info := t.info
	if info.IsAlias {
		if info.AliasKind == model.KindStruct {
			s.Alias = l.delegatingAlias()
		} else if n, ok := info.Type.(*types.Named); ok {
			s.Alias = l.typ(n.Underlying(), &fieldCtx{noSteps: true}, -1)
		}
		// An annotated named string or integer type with constants is also an
		// enum type, and its own shape is the closed set.
		if t.Enum != nil && s.Alias != nil {
			s.Alias.Enum = &Enum{Decl: t, Values: slices.Clone(t.Enum.Values)}
			s.Alias.Enum.Zero = m == Out && !hasZero(s.Alias.Enum.Values, s.Alias.Wire)
		}
		return s
	}
	if info.IgnoreUnknown && m == In {
		s.Unknown = Ignore
	}
	for _, fi := range info.Fields {
		if fi.Embed {
			s.Unknown = Collect
			if mt, ok := types.Unalias(fi.Type).Underlying().(*types.Map); ok {
				s.Rest = l.typ(mt.Elem(), &fieldCtx{f: fi, noSteps: true}, 0)
			}
			continue
		}
		f := &Field{
			Shape:      s,
			GoName:     fi.GoName,
			JSONName:   fi.JSONName,
			Doc:        fi.Doc,
			Directives: fi.Directives,
			Type:       l.field(fi),
		}
		if m == In {
			f.Optional = info.NoValidate || !fi.IsRequired()
		} else {
			f.Optional = fi.OmitEmpty || fi.OmitZero
		}
		s.Fields = append(s.Fields, f)
	}
	return s
}

type lowerer struct {
	set  *Set
	decl *TypeDecl
	mode Mode
}

type fieldCtx struct {
	f       model.FieldInfo
	noSteps bool
}

func (c *fieldCtx) steps(lvl int) []model.Step {
	if c.noSteps {
		return nil
	}
	if lvl < 0 {
		if c.f.Pipe != nil {
			return c.f.Pipe
		}
		return model.StepsFromLegacy(c.f.Mods, c.f.Validation)
	}
	if lvl < len(c.f.Levels) {
		return c.f.Levels[lvl]
	}
	if lvl == 0 && len(c.f.Levels) == 0 {
		return model.StepsFromLegacy(c.f.ElemMods, c.f.ElemValidation)
	}
	return nil
}

func (c *fieldCtx) keySteps() []model.Step {
	if c.noSteps {
		return nil
	}
	if c.f.KeyPipe != nil {
		return c.f.KeyPipe
	}
	return model.StepsFromLegacy(c.f.KeyMods, c.f.KeyValidation)
}

func (l *lowerer) qualifier(p *types.Package) string {
	if p.Path() == l.decl.Pkg.Path {
		return ""
	}
	return p.Name()
}

func (l *lowerer) field(fi model.FieldInfo) *Type {
	if fi.Type == nil {
		return &Type{GoType: fi.GoType, Go: Any, Wire: WireAny}
	}
	c := &fieldCtx{f: fi, noSteps: l.decl.info.NoValidate}
	if l.mode == Out {
		return l.output(fi, c)
	}
	t := l.typ(fi.Type, c, -1)
	var converted []*Type
	native := len(fi.Variants) == 0
	nullZero := false
	for _, v := range fi.Variants {
		switch v.Kind {
		case model.VariantNative:
			native = true
		case model.VariantNullZero:
			nullZero = true
		case model.VariantConvert:
			if v.In != nil {
				in := l.typ(v.In, &fieldCtx{f: fi, noSteps: true}, -1)
				in.Converted = true
				converted = append(converted, in)
			}
		}
	}
	if !model.FieldHasConverter(fi) {
		// With no converter there is no dispatch, and nullzero is the flag of
		// the field or of its struct.
		nullZero = l.nullZeroApplies(fi)
	}
	nullZero = nullZero && zeroPasses(t)
	if !native && len(converted) > 0 {
		// The decoder never scans the field's own type: every accepted shape
		// comes from a converter, and the rules run on what it returns. Only
		// nullzero or a pointer input takes null.
		rules := t.Rules
		*t = *converted[0]
		t.Rules, t.Nullable, t.OneOf = rules, nullZero || t.Nullable, converted[1:]
		return t
	}
	t.Nullable = t.Nullable || nullZero
	t.OneOf = append(t.OneOf, converted...)
	return t
}

// nullZeroApplies mirrors cmd/ggen's: nullzero, set on the field or on its
// struct, adds a null branch only to a non-pointer whose kind does not decode
// null itself.
func (l *lowerer) nullZeroApplies(fi model.FieldInfo) bool {
	if fi.Pointer || !fi.NullZero && !l.decl.info.NullZero {
		return false
	}
	//exhaustive:ignore not every kind applies here
	switch fi.Kind {
	case model.KindSlice, model.KindMap, model.KindBytes, model.KindNetIP, model.KindRawJSON, model.KindSQLNull, model.KindAny:
		return false
	}
	return true
}

// output lowers an output field. omitempty and omitzero skip a field behind
// a guard (cmd/ggen fieldSkipExpr), and a widening goes only when the guard
// skips every value that carries it.
func (l *lowerer) output(fi model.FieldInfo, c *fieldCtx) *Type {
	null := isSQLNull(fi.Type)
	if p, ok := types.Unalias(fi.Type).(*types.Pointer); ok && (fi.OmitZero || fi.OmitEmpty) {
		// Both guards skip a nil pointer first, so the field writes whatever
		// its pointee writes.
		t := l.typ(p.Elem(), c, -1)
		t.GoType = types.TypeString(fi.Type, l.qualifier)
		if fi.OmitEmpty {
			omitEmpty(t, null)
		}
		return t
	}
	t := l.typ(fi.Type, c, -1)
	if fi.OmitZero && !null {
		// Only the Go zero value is skipped. Raw JSON and an any hold null
		// without being nil, and a non-nil empty IP or a non-zero URL is
		// written as "". A database/sql Null type keeps both widenings: an
		// invalid one with a value is written as null.
		t.Nullable = t.Nullable && (t.Go == RawJSON || t.Go == Any)
		t.Empty = t.Empty && t.Go != Addr && t.Go != Prefix
		if t.Enum != nil {
			t.Enum.Zero = false
		}
	}
	if fi.OmitEmpty {
		omitEmpty(t, null)
	}
	return t
}

// omitEmpty clears what omitempty skips (cmd/ggen omitEmptyCond). For a kind
// it tests, it skips an empty value and every nil pointer above it: a
// database/sql Null type, which null marks, is empty when not Valid, and raw
// JSON or an any can be null without being empty.
func omitEmpty(t *Type, null bool) {
	switch {
	case null:
		t.Nullable = false
	case t.Go == String:
		t.Nullable = false
		if t.Enum != nil {
			t.Enum.Zero = false
		}
	case t.Go == Slice, t.Go == Map, t.Go == IP, t.Go == Addr, t.Go == Prefix, t.Go == IPNet, t.Go == TCPAddr, t.Go == UDPAddr,
		(t.Go == Bytes || t.Go == Array) && t.Len == 0:
		t.Nullable, t.Empty = false, false
	case t.Go == URL:
		// A non-zero URL can still have empty text.
		t.Nullable = false
	}
}

// isSQLNull reports whether gt, under any pointers, is a database/sql Null
// type.
func isSQLNull(gt types.Type) bool {
	for {
		p, ok := types.Unalias(gt).(*types.Pointer)
		if !ok {
			return sqlNullInner(types.Unalias(gt)) != nil
		}
		gt = p.Elem()
	}
}

// typ lowers a go/types type at pipe level lvl. Level -1 is the field itself,
// and k is the k-th container level below it. Pointers do not open a level.
func (l *lowerer) typ(gt types.Type, c *fieldCtx, lvl int) *Type {
	spelled := types.TypeString(gt, l.qualifier)
	// An alias keeps its own name and package: json.RawMessage is spelled and
	// keyed as json.RawMessage even though it unaliases to jsontext.Value.
	alias, aliased := gt.(*types.Alias)
	gt = types.Unalias(gt)
	if p, ok := gt.(*types.Pointer); ok {
		t := l.typ(p.Elem(), c, lvl)
		t.GoType = spelled
		t.Nullable = true
		return t
	}
	if inner := sqlNullInner(gt); inner != nil {
		t := l.typ(inner, c, lvl)
		t.GoType = spelled
		t.Nullable = true
		if n, ok := gt.(*types.Named); ok {
			t.Name = n.Obj().Name()
			if n.Obj().Pkg() != nil {
				t.Pkg = l.set.pkg(n.Obj().Pkg())
			}
		}
		return t
	}
	t := &Type{GoType: spelled}
	nilable := l.shapeOf(t, gt, c, lvl)
	if aliased && alias.Obj().Pkg() != nil {
		t.Name, t.Pkg = alias.Obj().Name(), l.set.pkg(alias.Obj().Pkg())
	}
	l.applySteps(t, c.steps(lvl))
	switch {
	case l.mode == Out:
		t.Nullable = t.Nullable || nilable
	case nilable:
		t.Nullable = zeroPasses(t)
	}
	return t
}

// shapeOf fills t's Go and JSON sides and reports whether the Go zero value is
// nil (so JSON null decodes to it and encodes from it).
func (l *lowerer) shapeOf(t *Type, gt types.Type, c *fieldCtx, lvl int) (nilable bool) {
	switch x := gt.(type) {
	case *types.Named:
		return l.named(t, x, c, lvl)
	case *types.Basic:
		l.basic(t, x, c, lvl)
		return false
	case *types.Slice:
		if isByte(x.Elem()) {
			l.bytes(t, c, 0)
			return true
		}
		t.Go, t.Wire = Slice, WireArray
		t.Elem = l.typ(x.Elem(), c, lvl+1)
		return true
	case *types.Array:
		if isByte(x.Elem()) {
			l.bytes(t, c, int(x.Len()))
			return false
		}
		t.Go, t.Wire, t.Len = Array, WireArray, int(x.Len())
		t.Elem = l.typ(x.Elem(), c, lvl+1)
		return false
	case *types.Map:
		t.Go, t.Wire = Map, WireObject
		t.Key = &Type{GoType: types.TypeString(x.Key(), l.qualifier), Go: String, Wire: WireString}
		if lvl < 0 {
			// keys: steps belong to the field's own map, which pointers do not
			// hide. A map writes only the keys it holds, so a key enum has no
			// zero value to admit.
			l.applySteps(t.Key, c.keySteps())
			if t.Key.Enum != nil {
				t.Key.Enum.Zero = false
			}
		}
		t.Elem = l.typ(x.Elem(), c, lvl+1)
		return true
	case *types.Interface:
		t.Go, t.Wire = Any, WireAny
		return true
	}
	t.Go, t.Wire = Any, WireAny
	return false
}

func isByte(gt types.Type) bool {
	b, ok := types.Unalias(gt).(*types.Basic)
	return ok && b.Kind() == types.Byte
}

func (l *lowerer) bytes(t *Type, c *fieldCtx, n int) {
	t.Go, t.Len = Bytes, n
	switch c.f.Format {
	case "array":
		t.Wire = WireArray
		t.Elem = &Type{GoType: "byte", Go: Uint8, Wire: WireInteger}
		return
	case "base64url":
		t.Format = FormatBase64URL
	case "base32":
		t.Format = FormatBase32
	case "base32hex":
		t.Format = FormatBase32Hex
	case "base16", "hex":
		t.Format = FormatHex
	default:
		t.Format = FormatBase64
	}
	t.Wire = WireString
}

func (l *lowerer) basic(t *Type, b *types.Basic, c *fieldCtx, lvl int) {
	quoted := lvl < 0 && c.f.String
	//exhaustive:ignore not every kind applies here
	switch b.Kind() {
	case types.String:
		t.Go, t.Wire = String, WireString
		return
	case types.Bool:
		t.Go, t.Wire = Bool, WireBool
		return
	case types.Float32, types.Float64:
		t.Go, t.Wire = Float64, WireNumber
		if b.Kind() == types.Float32 {
			t.Go = Float32
		}
		if quoted {
			t.Wire, t.Format = WireString, FormatNumberString
		}
		return
	}
	kinds := map[types.BasicKind]GoKind{
		types.Int: Int, types.Int8: Int8, types.Int16: Int16, types.Int32: Int32, types.Int64: Int64,
		types.Uint: Uint, types.Uint8: Uint8, types.Uint16: Uint16, types.Uint32: Uint32, types.Uint64: Uint64,
		types.Uintptr: Uint64,
	}
	k, ok := kinds[b.Kind()]
	if !ok {
		t.Go, t.Wire = Any, WireAny
		return
	}
	t.Go, t.Wire = k, WireInteger
	if quoted {
		t.Wire, t.Format = WireString, FormatIntString
		if b.Info()&types.IsUnsigned != 0 {
			t.Format = FormatUintString
		}
	}
}

func (l *lowerer) named(t *Type, n *types.Named, c *fieldCtx, lvl int) (nilable bool) {
	obj := n.Obj()
	t.Name = instantiatedName(n)
	if obj.Pkg() == nil {
		return l.shapeOf(t, n.Underlying(), c, lvl)
	}
	t.Pkg = l.set.pkg(obj.Pkg())
	if t.Pkg.Loaded {
		t.File = l.decl.fset.Position(obj.Pos()).Filename
	}
	key := obj.Pkg().Path() + "." + obj.Name()
	if handled, nilable := l.stdKind(t, key, c); handled {
		return nilable
	}
	if d := l.set.decls[key]; d != nil && d.named == nil {
		t.Ref = d
		l.refShape(t, n)
		if d.Enum != nil {
			t.Enum = &Enum{Decl: d, Values: slices.Clone(d.Enum.Values)}
			t.Enum.Zero = l.mode == Out && !hasZero(t.Enum.Values, t.Wire)
		}
		return false
	}
	if codec := methodsOf(n).codec(l.mode); codec != codecNone {
		sub := &Type{}
		l.shapeOf(sub, n.Underlying(), &fieldCtx{noSteps: true}, -1)
		t.Go, t.External = sub.Go, true
		if _, ok := n.Underlying().(*types.Struct); ok {
			t.Go, sub.Wire = Struct, WireObject
		}
		switch codec {
		case codecGgen:
			// ggen's own methods emit the type's structural shape. The script
			// did not load the package, so its fields stay unknown.
			t.Wire, t.Format = sub.Wire, sub.Format
			l.set.markUnloaded(key)
		case codecJSON:
			t.Wire = WireAny
		default:
			t.Wire = WireString
		}
		return false
	}
	switch n.Underlying().(type) {
	case *types.Basic:
		nilable = l.shapeOf(t, n.Underlying(), c, lvl)
		if d := l.set.enumDecl(n, l.decl.fset); d != nil {
			t.Ref = d
			t.Enum = &Enum{Decl: d, Values: slices.Clone(d.Enum.Values)}
			t.Enum.Zero = l.mode == Out && !hasZero(t.Enum.Values, t.Wire)
		}
		return nilable
	case *types.Slice, *types.Array, *types.Map, *types.Interface:
		return l.shapeOf(t, n.Underlying(), c, lvl)
	}
	t.Go, t.Wire, t.External = Struct, WireObject, true
	return false
}

// instantiatedName is the declared name of a named type, with the type
// arguments of a generic instantiation: "Box[int]". External types are keyed
// by Pkg.Path + "." + Name, so two instantiations stay distinct.
func instantiatedName(n *types.Named) string {
	args := n.TypeArgs()
	if args == nil || args.Len() == 0 {
		return n.Obj().Name()
	}
	parts := make([]string, args.Len())
	for i := range args.Len() {
		parts[i] = types.TypeString(args.At(i), func(p *types.Package) string { return p.Name() })
	}
	return n.Obj().Name() + "[" + strings.Join(parts, ",") + "]"
}

// stdKind fills t for a standard-library type ggen gives its own wire shape.
func (l *lowerer) stdKind(t *Type, key string, c *fieldCtx) (handled, nilable bool) {
	switch key {
	case "time.Time":
		t.Go = Time
		timeFormat(t, c.f.Format)
		return true, false
	case "time.Duration":
		t.Go = Duration
		switch c.f.Format {
		case "sec":
			t.Wire, t.Format = WireNumber, FormatDurationSec
		case "milli":
			t.Wire, t.Format = WireInteger, FormatDurationMs
		case "micro":
			t.Wire, t.Format = WireInteger, FormatDurationUs
		case "nano":
			t.Wire, t.Format = WireInteger, FormatDurationNs
		default:
			t.Wire, t.Format = WireString, FormatDuration
		}
		return true, false
	case "net.IP":
		t.Go, t.Wire, t.Format, t.Empty = IP, WireString, FormatIP, true
		return true, l.mode == In
	case "net/netip.Addr":
		t.Go, t.Wire, t.Format, t.Empty = Addr, WireString, FormatIPZone, true
		return true, false
	case "net/netip.Prefix":
		t.Go, t.Wire, t.Format, t.Empty = Prefix, WireString, FormatCIDR, true
		return true, false
	case "net.IPNet":
		t.Go, t.Wire, t.Format, t.Empty = IPNet, WireString, FormatCIDR, true
		return true, false
	case "net.TCPAddr", "net.UDPAddr":
		t.Go, t.Wire, t.Format, t.Empty = TCPAddr, WireString, FormatIPPort, true
		if key == "net.UDPAddr" {
			t.Go = UDPAddr
		}
		return true, false
	case "net/url.URL":
		t.Go, t.Wire, t.Format, t.Empty = URL, WireString, FormatURI, true
		return true, false
	case "math/big.Int":
		t.Go, t.Wire = BigInt, WireInteger
		return true, false
	case "math/big.Float":
		t.Go, t.Wire, t.Format = BigFloat, WireString, FormatBigFloat
		return true, false
	case "math/big.Rat":
		t.Go, t.Wire, t.Format = BigRat, WireString, FormatRational
		return true, false
	case "encoding/json.RawMessage", "encoding/json/jsontext.Value":
		t.Go, t.Wire = RawJSON, WireAny
		return true, true
	case "encoding/json.Number":
		t.Go, t.Wire = Number, WireNumber
		if l.mode == In {
			// The decoder hands the span to encoding/json, which reads a
			// quoted number too, and null as the zero value.
			t.OneOf = append(t.OneOf, &Type{Go: Number, Wire: WireString, Format: FormatNumberString})
		}
		return true, l.mode == In
	}
	return false, false
}

// refShape records the JSON side of a reference to a generated type. Its
// fields and rules live on the referenced type's own shape.
func (l *lowerer) refShape(t *Type, n *types.Named) {
	if l.aliasWire(t) {
		return
	}
	switch u := n.Underlying().(type) {
	case *types.Struct:
		t.Go, t.Wire = Struct, WireObject
	case *types.Slice:
		t.Go, t.Wire = Slice, WireArray
		if isByte(u.Elem()) {
			t.Go, t.Wire = Bytes, WireString
		}
	case *types.Array:
		t.Go, t.Wire = Array, WireArray
		if isByte(u.Elem()) {
			t.Go, t.Wire = Bytes, WireString
		}
	case *types.Map:
		t.Go, t.Wire = Map, WireObject
	default:
		sub := &Type{}
		l.shapeOf(sub, u, &fieldCtx{noSteps: true}, -1)
		t.Go, t.Wire, t.Format = sub.Go, sub.Wire, sub.Format
	}
}

// aliasWire copies the JSON side of a reference to a generated alias, which
// decides its own: `type LocalTime time.Time` is a string, not the object its
// underlying struct would suggest. It reports whether t.Ref is such an alias.
func (l *lowerer) aliasWire(t *Type) bool {
	if d := t.Ref; d != nil && d.info.IsAlias {
		if a := d.shape(l.mode).Alias; a != nil {
			t.Go, t.Wire, t.Format, t.Empty = a.Go, a.Wire, a.Format, a.Empty
			return true
		}
	}
	return false
}

func (l *lowerer) enumAlias() *Type {
	d := l.decl
	t := &Type{GoType: d.Name, Name: d.Name, Pkg: d.Pkg, File: d.File}
	l.shapeOf(t, d.named.Underlying(), &fieldCtx{noSteps: true}, -1)
	t.Enum = &Enum{Decl: d, Values: slices.Clone(d.Enum.Values)}
	return t
}

// delegatingAlias lowers a struct alias whose methods delegate to its target:
// a generated target is a reference to it, any other an external type of the
// target's package, which is the alias's own when the import path is empty.
func (l *lowerer) delegatingAlias() *Type {
	info := l.decl.info
	name := info.AliasUnderlying[strings.LastIndexByte(info.AliasUnderlying, '.')+1:]
	t := &Type{GoType: info.AliasUnderlying, Name: name, Pkg: l.decl.Pkg, Go: Struct, Wire: WireAny, External: true}
	if imp := info.AliasUnderlyingImport; imp.Path != "" {
		t.Pkg = l.set.pkgOf(imp.Path, imp.Name)
	}
	key := t.Pkg.Path + "." + name
	if d := l.set.decls[key]; d != nil && d.named == nil {
		t.Ref, t.File, t.External = d, d.File, false
		if !l.aliasWire(t) {
			t.Wire = WireObject
		}
		return t
	}
	if handled, _ := l.stdKind(t, key, &fieldCtx{f: l.aliasField()}); handled {
		return t
	}
	if !info.AliasIface.JSONMarshaler && !info.AliasIface.JSONUnmarshaler &&
		(info.AliasIface.TextMarshaler || info.AliasIface.TextUnmarshaler || info.AliasIface.TextAppender) {
		t.Wire = WireString
	}
	return t
}

// aliasField is the pseudo-field a top-level alias lowers as. It has no tag,
// so it has no format of its own.
func (l *lowerer) aliasField() model.FieldInfo { return model.FieldInfo{} }

func timeFormat(t *Type, format string) {
	switch format {
	case "", "RFC3339", "RFC3339Nano":
		t.Wire, t.Format = WireString, FormatDateTime
	case "unix":
		t.Wire, t.Format = WireNumber, FormatUnix
	case "unixmilli":
		t.Wire, t.Format = WireInteger, FormatUnixMilli
	case "unixmicro":
		t.Wire, t.Format = WireInteger, FormatUnixMicro
	case "unixnano":
		t.Wire, t.Format = WireInteger, FormatUnixNano
	default:
		layout := format
		if named, ok := timeLayouts[format]; ok {
			layout = named
		}
		t.Wire, t.Format = WireString, FormatTimeLayoutPrefix+layout
	}
}

var timeLayouts = map[string]string{
	"Layout": "01/02 03:04:05PM '06 -0700", "ANSIC": "Mon Jan _2 15:04:05 2006",
	"UnixDate": "Mon Jan _2 15:04:05 MST 2006", "RubyDate": "Mon Jan 02 15:04:05 -0700 2006",
	"RFC822": "02 Jan 06 15:04 MST", "RFC822Z": "02 Jan 06 15:04 -0700",
	"RFC850": "Monday, 02-Jan-06 15:04:05 MST", "RFC1123": "Mon, 02 Jan 2006 15:04:05 MST",
	"RFC1123Z": "Mon, 02 Jan 2006 15:04:05 -0700", "Kitchen": "3:04PM", "Stamp": "Jan _2 15:04:05",
	"StampMilli": "Jan _2 15:04:05.000", "StampMicro": "Jan _2 15:04:05.000000",
	"StampNano": "Jan _2 15:04:05.000000000", "DateTime": "2006-01-02 15:04:05",
	"DateOnly": "2006-01-02", "TimeOnly": "15:04:05",
}

// sqlNullInner returns the value type of a database/sql Null type.
func sqlNullInner(gt types.Type) types.Type {
	n, ok := gt.(*types.Named)
	if !ok || n.Obj().Pkg() == nil || n.Obj().Pkg().Path() != "database/sql" || !strings.HasPrefix(n.Obj().Name(), "Null") {
		return nil
	}
	st, ok := n.Underlying().(*types.Struct)
	if !ok || st.NumFields() != 2 {
		return nil
	}
	return st.Field(0).Type()
}

// codec names how a type encodes or decodes itself.
type codec int

const (
	codecNone codec = iota // no method: encoding/json walks the type
	codecJSON
	codecText
	codecGgen
)

// methodSet records each direction separately: a type with only UnmarshalText
// decodes from a string but encodes structurally.
type methodSet struct {
	marshalJSON, unmarshalJSON bool
	marshalText, unmarshalText bool
	encodeGgen, decodeGgen     bool
}

// codec returns how the type handles mode, or codecNone when that direction
// has no method of its own.
func (m methodSet) codec(mode Mode) codec {
	enc, js, text := m.encodeGgen, m.marshalJSON, m.marshalText
	if mode == In {
		enc, js, text = m.decodeGgen, m.unmarshalJSON, m.unmarshalText
	}
	switch {
	case enc:
		return codecGgen
	case js:
		return codecJSON
	case text:
		return codecText
	}
	return codecNone
}

func methodsOf(gt types.Type) methodSet {
	var m methodSet
	for _, ms := range [...]*types.MethodSet{types.NewMethodSet(gt), types.NewMethodSet(types.NewPointer(gt))} {
		for sel := range ms.Methods() {
			switch sel.Obj().Name() {
			case "MarshalJSON", "MarshalJSONTo":
				m.marshalJSON = true
			case "UnmarshalJSON", "UnmarshalJSONFrom":
				m.unmarshalJSON = true
			case "MarshalText", "AppendText":
				m.marshalText = true
			case "UnmarshalText":
				m.unmarshalText = true
			case "AppendJSON":
				m.encodeGgen = true
			case "DecodeFrom":
				m.decodeGgen = true
			}
		}
	}
	return m
}

func hasZero(values []EnumValue, w Wire) bool {
	for _, v := range values {
		if w == WireString && v.Value == `""` {
			return true
		}
		if w != WireString {
			if f, err := strconv.ParseFloat(v.Value, 64); err == nil && f == 0 {
				return true
			}
		}
	}
	return false
}

// applySteps converts a level's pipe steps onto t. A oneof narrows t to an
// Enum when no transform precedes it in input, and always in output, where it
// is the only step that survives.
func (l *lowerer) applySteps(t *Type, steps []model.Step) {
	transformed := false
	for _, st := range steps {
		if st.IsMod {
			transformed = true
			if l.mode == In {
				if r, ok := modRule(st.M); ok {
					t.Rules = append(t.Rules, l.funcPkg(r))
				}
			}
			continue
		}
		v := st.V
		if v.Name == "oneof" && (l.mode == Out || !transformed) && (t.Wire == WireString || t.Wire == WireInteger || t.Wire == WireNumber) && t.Format == "" {
			e := &Enum{}
			for _, p := range model.SplitPipeParts(v.Value) {
				ev := EnumValue{Value: p}
				if t.Wire == WireString {
					q, err := json.Marshal(p)
					if err != nil {
						continue
					}
					ev.Value, ev.Text = string(q), p
				}
				e.Values = append(e.Values, ev)
			}
			e.Zero = l.mode == Out && !hasZero(e.Values, t.Wire)
			t.Enum, t.Ref = e, nil
			continue
		}
		if l.mode == In {
			if r, ok := validatorRule(v); ok {
				t.Rules = append(t.Rules, l.funcPkg(r))
			}
		}
	}
}

// funcPkg fills in the package of an @Func declared next to the type.
func (l *lowerer) funcPkg(r Rule) Rule {
	if r.Op == Func && r.FuncPkg == "" {
		r.FuncPkg = l.decl.Pkg.Path
	}
	return r
}

// modOps and valOps map every rule name model accepts, except the two that
// describe presence rather than a value (`required`, `optional`), which
// become Field.Optional. TestRuleOpsCoverModel keeps them complete.
var modOps = map[string]Op{
	"trim": Trim, "tolower": ToLower, "toupper": ToUpper,
	"trimleft": TrimPrefix, "trimright": TrimSuffix,
	"replace": Replace, "clamp": Clamp,
}

var valOps = map[string]Op{
	"notempty": NotEmpty, "len": Len, "minlen": MinLen, "maxlen": MaxLen, "runes": Runes,
	"minrunes": MinRunes, "maxrunes": MaxRunes, "gt": GT, "gte": GTE, "lt": LT, "lte": LTE,
	"eq": Eq, "neq": Neq, "multiple": Multiple, "oneof": OneOf, "url": URLRule,
	"alphanum": Alphanum, "numeric": Numeric, "hexadecimal": Hex, "islower": IsLower,
	"isupper": IsUpper, "starts": Starts, "ends": Ends, "contains": Contains,
}

func modRule(m model.ModRule) (Rule, bool) {
	if m.Custom || strings.HasPrefix(m.Name, "@") {
		return Rule{Op: Func, Func: strings.TrimPrefix(m.Name, "@"), FuncPkg: m.PkgImport, Msg: m.Msg, Transform: true}, true
	}
	op, ok := modOps[m.Name]
	if !ok {
		return Rule{}, false
	}
	r := Rule{Op: op, Transform: true}
	//exhaustive:ignore not every kind applies here
	switch op {
	case TrimPrefix, TrimSuffix:
		if m.Value == "" {
			return Rule{}, false
		}
		r.Args = []string{m.Value}
	case Replace, Clamp:
		r.Args = model.SplitPipeParts(m.Value)
	}
	return r, true
}

func validatorRule(v model.ValidationRule) (Rule, bool) {
	if v.Custom || strings.HasPrefix(v.Name, "@") {
		return Rule{Op: Func, Func: strings.TrimPrefix(v.Name, "@"), FuncPkg: v.PkgImport, Msg: v.Msg}, true
	}
	op, ok := valOps[v.Name]
	if !ok {
		return Rule{}, false
	}
	r := Rule{Op: op}
	switch op {
	case NotEmpty, URLRule, Alphanum, Numeric, Hex, IsLower, IsUpper:
	case OneOf:
		r.Args = model.SplitPipeParts(v.Value)
	default:
		r.Args = []string{v.Value}
	}
	return r, true
}

// zeroPasses reports whether t's Go zero value, which null decodes to on a
// nil-able type or under nullzero, passes t's rules in declared order. Of the
// built-in transforms only clamp moves it. A custom function is assumed to
// pass and to keep the value.
func zeroPasses(t *Type) bool {
	if t.Enum != nil && t.Enum.Decl == nil && !hasZero(t.Enum.Values, t.Wire) {
		return false // a oneof, which no transform precedes
	}
	size := t.Len // of "", an empty container, or a fixed array
	var n float64
	for _, r := range t.Rules {
		pass := true
		//exhaustive:ignore not every kind applies here
		switch r.Op {
		case NotEmpty:
			pass = size > 0
		case Len, Runes:
			pass = size == ruleInt(r.Args[0])
		case MinLen, MinRunes:
			pass = size >= ruleInt(r.Args[0])
		case MaxLen, MaxRunes:
			pass = size <= ruleInt(r.Args[0])
		case GT:
			pass = n > ruleNum(r.Args[0])
		case GTE:
			pass = n >= ruleNum(r.Args[0])
		case LT:
			pass = n < ruleNum(r.Args[0])
		case LTE:
			pass = n <= ruleNum(r.Args[0])
		case Multiple:
			pass = math.Mod(n, ruleNum(r.Args[0])) == 0
		case Eq, Neq, OneOf:
			in := slices.ContainsFunc(r.Args, func(a string) bool {
				if t.Go == String {
					return a == ""
				}
				return ruleNum(a) == n
			})
			pass = in != (r.Op == Neq)
		case URLRule, Alphanum, Numeric, Hex, Starts, Ends, Contains:
			// "" is no URL and has no characters, and these substrings are
			// never empty.
			pass = false
		case Clamp:
			if strings.TrimSpace(r.Args[0]) != "" {
				n = max(n, ruleNum(r.Args[0]))
			}
			if strings.TrimSpace(r.Args[1]) != "" {
				n = min(n, ruleNum(r.Args[1]))
			}
		}
		if !pass {
			return false
		}
	}
	return true
}

// ruleInt and ruleNum read a rule argument, which the parser already checked.
func ruleInt(arg string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(arg))
	return n
}

func ruleNum(arg string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(arg), 64)
	return f
}

func walkShapeRefs(s *Shape, fn func(*TypeDecl)) {
	for _, f := range s.Fields {
		walkTypeRefs(f.Type, fn)
	}
	walkTypeRefs(s.Alias, fn)
	walkTypeRefs(s.Rest, fn)
}

func walkTypeRefs(t *Type, fn func(*TypeDecl)) {
	if t == nil {
		return
	}
	if t.Ref != nil {
		fn(t.Ref)
	}
	walkTypeRefs(t.Elem, fn)
	walkTypeRefs(t.Key, fn)
	for _, o := range t.OneOf {
		walkTypeRefs(o, fn)
	}
}
