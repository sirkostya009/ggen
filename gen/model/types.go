package model

import (
	"go/types"
	"strconv"
	"strings"
)

type TypeKind int

const (
	KindString TypeKind = iota
	KindInt
	KindInt64
	KindUint64
	KindFloat64
	KindBool
	KindSlice
	KindStruct
	KindBytes       // []byte or [N]byte, a JSON string in base64, hex or another format
	KindTime        // time.Time
	KindDuration    // time.Duration
	KindNetIP       // net.IP
	KindNetipAddr   // netip.Addr
	KindNetipPrefix // netip.Prefix
	KindInt8        // int8
	KindInt16       // int16
	KindInt32       // int32
	KindUint        // uint
	KindUint8       // a plain uint8 field, not part of []byte
	KindUint16      // uint16
	KindUint32      // uint32
	KindFloat32     // float32
	KindMap         // map[string]V, ElemType and ElemKind describe V
	KindArray       // [N]T, ArrayLen holds N, a JSON array of exactly N items
	KindRawJSON     // json.RawMessage or jsontext.Value, a raw JSON span kept as is
	KindURL         // net/url.URL, a JSON string read with url.Parse, written with String()
	KindBigInt      // math/big.Int, an unquoted JSON number of any precision
	KindBigFloat    // math/big.Float, a JSON string such as "3.14"
	KindBigRat      // math/big.Rat, a JSON string "num/denom"
	KindSQLNull     // database/sql.NullX, a JSON value or null, see SQLNullSpec
	KindAny         // any or interface{}, decoded to the same shapes encoding/json produces
)

type ValidationRule struct {
	Name  string // a rule name such as "required", "minlen" or "gte", or "@FuncName" for user funcs
	Value string // the parameter, for example "2" for minlen=2, empty for "required"

	// A custom rule has a Name starting with "@". It is resolved with go/types
	// and emitted as a direct call. PkgImport and PkgName are empty for a
	// function in the same package. Otherwise they hold the import path and
	// the qualifier the generated call uses.
	Custom    bool
	PkgImport string
	PkgName   string
	FuncName  string

	// BoolForm marks a `func(T) bool` validator, as opposed to
	// `func(T) error`. A false result produces a ggen.PredicateError. Msg is
	// the optional inline message, as in `@Even:'must be even'`. Only the bool
	// form takes one.
	BoolForm bool
	Msg      string
}

// ModRule is a step that changes a decoded value before validation, for
// example trimming whitespace or lowercasing.
type ModRule struct {
	Name  string // "trim", "tolower", "toupper", "trimleft", "trimright", "replace", "clamp", or "@FuncName"
	Value string // the parameter, empty for trim, tolower and toupper

	// A custom mod is resolved like a custom ValidationRule. Fallible marks a
	// function that returns (T, error) or (T, bool). Its failure is reported
	// as a parse error, not a validation error.
	Custom    bool
	PkgImport string
	PkgName   string
	FuncName  string
	Fallible  bool // returns (T, error) or (T, bool)

	// BoolForm marks `func(T) (T, bool)`, as opposed to the error form. A
	// false result produces a ggen.ModError. Msg is the optional inline
	// message. Only the bool form takes one.
	BoolForm bool
	Msg      string
}

type FieldInfo struct {
	Doc        string     // doc comment, or the trailing comment, without directive lines
	Directives []string   // `//word:...` lines of the doc comment, without the slashes
	Type       types.Type // resolved go/types type, nil in the AST-only fallback mode
	GoName     string
	StructName string // Go name of the owning struct, used in error messages
	EmbedDepth int    // embedding levels between the declaring struct and the parent, 0 for the parent's own field. When names clash the shallowest field wins
	JSONName   string
	GoType     string // full Go type as text, for example "string", "[]int", "*Address"
	Kind       TypeKind
	ElemType   string // element type of a slice or array, for example "string" for []string
	ElemKind   TypeKind
	ArrayLen   int // N of a KindArray field. The container peel also sets it on nested levels
	// N of the inner [N]T at this level when ElemKind == KindArray. Set for
	// slice and array elements. A map value leaves it 0, read N from ElemType.
	ElemArrayLen    int
	ElemPointer     bool               // the slice or array element is `*T`, ElemType is the pointee
	Pointer         bool               // the field is *T, Kind describes the pointee
	PointeeType     string             // Go type of the pointee for pointer fields, such as "string" or "Address"
	Validation      []ValidationRule   // rules on the field, or on a container as a whole
	ElemValidation  []ValidationRule   // first nested level: rules per slice element or per map value
	InnerValidation [][]ValidationRule // levels 2 and deeper, one slice per extra level
	KeyValidation   []ValidationRule   // rules after `keys:`, map keys only, which are always strings
	Mods            []ModRule          // value changes applied after decode
	ElemMods        []ModRule          // first nested level mods
	InnerMods       [][]ModRule        // levels 2 and deeper
	KeyMods         []ModRule          // `keys:` mods, map keys only

	// The ordered pipeline from the `pipe:` tag. Pipe, KeyPipe and Levels are
	// the source of truth for the order of value steps, with mods and
	// validators mixed per level. The separate rule and mod lists above are
	// derived from them for code that does not care about order.
	Presence Presence  // required or optional, taken from the pipe
	Variants []Variant // decode stage. nil means the plain decode of the field type
	Pipe     []Step    // outer value steps on the whole field or container, after decode
	KeyPipe  []Step    // map key value steps
	Levels   [][]Step  // nested levels: Levels[0] per element, Levels[1] one level deeper, and so on
	// explicit prealloc size for slices and maps. -1 is unset and falls back
	// to len, minlen or the default. 0 turns prealloc off. N>0 uses N as the
	// capacity. Takes priority over len and minlen.
	HintLen    int
	HintLevels []int           // prealloc sizes per nested level from the `hint:` tag, -1 is unset. HintLevels[0] sizes the first nested level
	Iface      FieldInterfaces // methods and interfaces the field type implements, such as TextMarshaler or ByteDecoder
	ElemIface  FieldInterfaces // the same for the slice, array or map element type, used by size estimates of struct elements
	OmitEmpty  bool
	OmitZero   bool
	// on decode, accept an explicit JSON null on this non-pointer field and
	// set it to its Go zero value instead of failing. Has no effect on kinds
	// that already accept null: pointer, slice, map, []byte, sql.Null*, raw
	// JSON and any
	NullZero   bool
	String     bool   // marshal and unmarshal the field as a quoted JSON string
	Format     string // jsonv2 format option, such as "RFC3339", "unix" or "hex"
	Embed      bool   // catch-all map: takes unknown keys on decode, splices its entries into the object on encode
	MultiErr   bool   // copied from the parent struct: collect errors instead of stopping
	AllowDups  bool   // copied from the parent struct: skip the duplicate-key check
	NoValidate bool   // copied from the parent struct: skip validation and mods
	UseNumber  bool   // copied from the parent struct: read numbers in KindAny fields as json.Number
	HTMLEscape bool   // copied from the parent struct: escape <, > and & in strings. Off by default, as in jsonv2
	Copy       bool   // copied from the parent struct: the bytes path copies kept strings, RawMessage and any values instead of pointing into the input
	// copied from the parent struct: skip UTF-8 validation on decode. Strings
	// keep the raw bytes, and raw spans are not checked. An unpaired
	// surrogate escape, one half of a UTF-16 pair with no partner, becomes
	// U+FFFD, the replacement character
	AllowInvalidUTF8 bool
	Ignored          bool
	NotComparable    bool // the field type is not comparable, so omitzero needs a reflect zero check because `!= (T{})` would not compile
	// the field's own type, with pointers not removed, has a struct as its
	// underlying type. Only go/types can tell a struct apart from named
	// containers and foreign array types, which also read as KindStruct
	UnderlyingStruct bool

	// SQLNullInner is set for a generic database/sql.Null[T]. It is a
	// synthetic FieldInfo for T. Renderers hand the V field to the normal
	// emitters with it, so sql.Null[T] has the JSON shape of T, or null. The
	// named sql.NullX types use SQLNullSpec and leave this nil.
	// SQLNullImports holds the foreign imports that the emitted type names
	// refer to.
	SQLNullInner   *FieldInfo
	SQLNullImports []TypeImport

	// TypeImports holds every foreign package named anywhere in the field's
	// type: the type itself, a slice or array element, a map key or value, a
	// pointee, or a type argument. The emitters write those type names as is,
	// as in `var v leaf.Leaf` or `map[string]leaf.Leaf{}`. Without these the
	// generated file would name a package it does not import. They are only
	// candidates. A value field of a foreign type may only ever be written as
	// `result.X`, so collectImports keeps only the ones whose qualifier appears
	// in a rendered body. Filled from go/types. Empty on the AST-only path,
	// where foreign types go through encoding/json.
	TypeImports []TypeImport

	// NamedPrims records every named type in the field's type whose
	// underlying type is a primitive. That covers the field type itself, a
	// slice or array element, a map key or value, and a pointee. It is merged
	// into namedKinds before code generation, so rules and mods on
	// `type Priority string` work through `string` whether or not Priority is
	// annotated.
	NamedPrims map[string]TypeKind

	// The flags below are used by the code generator only. The parser never
	// sets them.
	// the value code sits directly inside the key switch, so a `null` match
	// can `break` to the comma handling instead of wrapping the whole value
	// decode in an else
	AtDispatch bool
	// the decode target is a newly declared nil local, such as a map value
	// temp or a pre-grown []**T slot. Skip reusing the old value and allocate
	// the pointer chain directly
	TargetNil bool
	// the parent element loop already read a `null` for this slot. The nested
	// container code skips its own null check, so its body is not wrapped in
	// an else
	NullDone bool
}

type StructInfo struct {
	Doc           string     // doc comment without directive lines
	Directives    []string   // `//word:...` lines of the doc comment, without the slashes
	File          string     // declaring source file, empty in the AST-only fallback mode
	Type          types.Type // the declared named type, nil in the AST-only fallback mode
	Name          string
	Fields        []FieldInfo
	BuildTag      string // normalised //go:build expression of the source file, empty when there is none
	Marshal       bool   // emit json.Marshaler and json.MarshalerTo methods
	Unmarshal     bool   // emit json.Unmarshaler and json.UnmarshalerFrom methods
	MultiErr      bool   // collect validation errors instead of stopping at the first
	AllowDups     bool   // allow duplicate JSON keys, which are an error by default
	NoValidate    bool   // skip validation rules, required-field checks and mods
	IgnoreUnknown bool   // skip unknown JSON keys. By default they produce ggen.UnknownKeyError
	NullZero      bool   // accept JSON null on every non-pointer field and set the Go zero value. One field opts in alone with the `nullzero` pipe variant
	NoSort        bool   // keep the declared field order instead of sorting fields by JSON name
	UseNumber     bool   // decode JSON numbers in `any` fields as json.Number instead of float64
	HTMLEscape    bool   // escape <, > and & in emitted strings. Off by default, as in jsonv2
	// the bytes-path DecodeFrom copies kept strings, RawMessage and any values
	// out of the input instead of aliasing it, so they outlive the input like
	// stream-path values do
	Copy             bool
	AllowInvalidUTF8 bool // skip UTF-8 validation on decode for this struct's strings and raw spans
	Test             bool // declared in a *_test.go file, so output goes to *_ggen_test.go
	XTest            bool // declared in the external test package `package foo_test`, so it gets its own output file and package clause

	// IsAlias marks a top-level named type over a primitive, struct or
	// container, such as `type Count int` or `type LocalUUID uuid.UUID`. It
	// gets the same methods as a struct, with bodies made for its shape:
	//   - Primitive: AliasKind is a primitive kind and AliasUnderlying the
	//     basic type name. The bodies read or write the primitive and convert.
	//   - Struct: AliasKind == KindStruct. AliasIface says which marshal and
	//     unmarshal methods exist, and codegen calls those methods when it can.
	IsAlias               bool
	AliasKind             TypeKind
	AliasUnderlying       string          // Go type of the underlying, for example "string" or "uuid.UUID"
	AliasUnderlyingImport TypeImport      // the underlying's package when foreign, zero for the same package or a basic type
	AliasIface            FieldInterfaces // methods of the underlying struct, for KindStruct aliases only

	// AliasField holds the container shape of a slice, map or array alias,
	// such as `type Tags []string` or `type Tuple [3]int`. Only Kind,
	// ElemType, ElemKind, ArrayLen and ElemPointer are used. Codegen passes it
	// to the container emitters.
	AliasField FieldInfo
}

// SQLNullKind describes a database/sql.NullX type: the kind of the value it
// holds and the name of that field, as in NullString.String or
// NullInt64.Int64. Renderers parse the value with the code for that kind.
type SQLNullKind struct {
	Field string   // exported field name on the struct, such as String or Int64
	Inner TypeKind // kind of that field
	Type  string   // Go type of that field for conversions, such as string, int64 or time.Time
}

// SQLNullSpec returns the spec for a known sql.NullX type, looked up by the
// full type name such as "sql.NullString".
func SQLNullSpec(goType string) (SQLNullKind, bool) {
	switch goType {
	case "sql.NullString":
		return SQLNullKind{Field: "String", Inner: KindString, Type: "string"}, true
	case "sql.NullInt64":
		return SQLNullKind{Field: "Int64", Inner: KindInt64, Type: "int64"}, true
	case "sql.NullInt32":
		return SQLNullKind{Field: "Int32", Inner: KindInt32, Type: "int32"}, true
	case "sql.NullInt16":
		return SQLNullKind{Field: "Int16", Inner: KindInt16, Type: "int16"}, true
	case "sql.NullByte":
		return SQLNullKind{Field: "Byte", Inner: KindUint8, Type: "uint8"}, true
	case "sql.NullBool":
		return SQLNullKind{Field: "Bool", Inner: KindBool, Type: "bool"}, true
	case "sql.NullFloat64":
		return SQLNullKind{Field: "Float64", Inner: KindFloat64, Type: "float64"}, true
	case "sql.NullTime":
		return SQLNullKind{Field: "Time", Inner: KindTime, Type: "time.Time"}, true
	}
	// For the generic sql.Null[T] the value field is always V. Only inner
	// kinds the renderers support are accepted.
	if inner, ok := sqlNullGenericInner(goType); ok {
		if k := ResolveKind(inner); isSupportedSQLNullInner(k) {
			return SQLNullKind{Field: "V", Inner: k, Type: inner}, true
		}
	}
	return SQLNullKind{}, false
}

// TypeImport is one foreign package named in a field's type, with the
// qualifier the generated code uses for it. Explicit means the qualifier is
// not the package's declared name, so the import line must name it:
// `Name "Path"`.
type TypeImport struct {
	Path     string
	Name     string
	Explicit bool
}

func (f FieldInfo) IsRequired() bool {
	if f.Presence == PresenceRequired {
		return true
	}
	for _, v := range f.Validation {
		if v.Name == "required" {
			return true
		}
	}
	return false
}

func (f FieldInfo) HasRule(name string) (string, bool) {
	for _, v := range f.Validation {
		if v.Name == name {
			return v.Value, true
		}
	}
	return "", false
}

// HasEmbed reports whether any field on s is a catch-all embedded map.
func (s StructInfo) HasEmbed() bool {
	for _, f := range s.Fields {
		if f.Embed {
			return true
		}
	}
	return false
}

// EmbedField returns the first catch-all map field on s, or an empty
// FieldInfo if there is none. Templates call it, so it returns a value.
func (s StructInfo) EmbedField() FieldInfo {
	for _, f := range s.Fields {
		if f.Embed {
			return f
		}
	}
	return FieldInfo{}
}

func IsNumeric(k TypeKind) bool {
	//exhaustive:ignore not every kind applies here
	switch k {
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64,
		KindUint, KindUint8, KindUint16, KindUint32, KindUint64,
		KindFloat32, KindFloat64:
		return true
	}
	return false
}

// PointerDepth counts the leading `*` of a pointer field's GoType and
// returns the count and the type left after removing them. A depth of 1 is a
// plain `*T`. A depth of 2 or more is a multi-level pointer such as `**T`.
func PointerDepth(goType string) (int, string) {
	depth := 0
	for len(goType) > 0 && goType[0] == '*' {
		depth++
		goType = goType[1:]
	}
	return depth, goType
}

// StripOneContainer removes one slice `[]` or array `[N]` layer from a Go
// type string. It returns the element type, its kind, and N. N is 0 for a
// slice or a type that is not a container.
func StripOneContainer(typ string) (inner string, kind TypeKind, length int) {
	if strings.HasPrefix(typ, "[]") {
		inner = typ[2:]
		return inner, ResolveKind(inner), 0
	}
	if len(typ) > 2 && typ[0] == '[' {
		if end := strings.IndexByte(typ, ']'); end > 1 {
			if n, err := strconv.Atoi(typ[1:end]); err == nil {
				inner = typ[end+1:]
				return inner, ResolveKind(inner), n
			}
		}
	}
	return typ, ResolveKind(typ), 0
}
