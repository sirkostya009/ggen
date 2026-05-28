package main

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
	KindBytes       // []byte / [N]byte (JSON string: base64/hex/etc per format)
	KindTime        // time.Time
	KindDuration    // time.Duration
	KindNetIP       // net.IP
	KindNetipAddr   // netip.Addr
	KindNetipPrefix // netip.Prefix
	KindInt8        // int8
	KindInt16       // int16
	KindInt32       // int32
	KindUint        // uint
	KindUint8       // uint8 (not []byte; bare uint8 field)
	KindUint16      // uint16
	KindUint32      // uint32
	KindFloat32     // float32
	KindMap         // map[string]V — ElemType/ElemKind describe V
	KindArray       // [N]T — ArrayLen holds N; decode/encode as a JSON tuple
	KindRawJSON     // json.RawMessage / jsontext.Value — opaque JSON span, zero-copy
	KindURL         // net/url.URL — JSON string parsed via url.Parse / String()
	KindBigInt      // math/big.Int — JSON number (no quotes, arbitrary precision)
	KindBigFloat    // math/big.Float — JSON number string
	KindBigRat      // math/big.Rat — JSON string "num/denom"
	KindSQLNull     // database/sql.NullX — JSON value or null; spec via SQLNullSpec(GoType)
	KindAny         // any / interface{} — delegated to encoding/json on both ends
)

type ValidationRule struct {
	Name  string // "required", "minlen", "maxlen", "gte", "lte", or "@FuncName" for user funcs
	Value string // parameter value, e.g. "2" for minlen=2; empty for "required"

	// Custom rule resolution (only populated when Name starts with "@"):
	// the func is looked up via go/types at parse time and the call site is
	// emitted directly — no runtime registry. PkgImport is empty for
	// same-package funcs; otherwise it's the import path to add to the
	// generated file. PkgName is the canonical package identifier the
	// generated code uses to qualify the call (e.g. "thirdparty"); empty
	// for same-package.
	Custom    bool
	PkgImport string
	PkgName   string
	FuncName  string
}

// ModRule is an input-modification step applied after decoding but before
// validation (e.g., trim whitespace, lowercase).
type ModRule struct {
	Name  string // "trim", "lower", "upper", "trimleft", "trimright", "replace", or "@FuncName"
	Value string // parameter (empty for trim/lower/upper)

	// Custom mod resolution — same shape as ValidationRule's custom fields,
	// plus Fallible: true when the func returns (T, error). When Fallible,
	// the generator emits an error-propagation branch; the error surfaces as
	// a parse error (not a validation error) — same level as scan.ErrBadX.
	Custom    bool
	PkgImport string
	PkgName   string
	FuncName  string
	Fallible  bool
}

type FieldInfo struct {
	GoName          string
	StructName      string // owning struct's Go name; used in error diagnostics
	JSONName        string
	GoType          string // full Go type as string, e.g. "string", "[]int", "*Address"
	Kind            TypeKind
	ElemType        string // for slices/arrays: element type (e.g. "string" for []string)
	ElemKind        TypeKind
	ArrayLen        int                // for KindArray: fixed array length N (peel sets it on nested inners)
	ElemArrayLen    int                // when ElemKind == KindArray, N of the inner [N]T at this level
	ElemPointer     bool               // true when the slice/array element is `*T`; ElemType is the pointee
	Pointer         bool               // true when the field is *T; Kind describes the pointee
	PointeeType     string             // for pointer fields: pointee Go type ("string", "Address")
	Validation      []ValidationRule   // applies to the field (or whole slice)
	ElemValidation  []ValidationRule   // level-1 dive: rules (per-element for slices, per-value for maps)
	InnerValidation [][]ValidationRule // levels 2..N (one slice per extra `dive:`)
	KeyValidation   []ValidationRule   // rules after `keys:` — map keys only (always string)
	Mods            []ModRule          // input transforms applied after decode
	ElemMods        []ModRule          // level-1 dive: mods
	InnerMods       [][]ModRule        // levels 2..N
	KeyMods         []ModRule          // `keys:` mods — map keys only
	HintLen         int                // explicit preallocation hint for slices/maps; -1=unset (fall through to len/minlen/default), 0=user opt-out (no prealloc), N>0=use N as cap. Overrides len/minlen.
	Iface           FieldInterfaces    // statically detected method-set membership (TextMarshaler, ByteDecoder, ...)
	ElemIface       FieldInterfaces    // method-set probe on the slice/array/map element type (used by size estimators for struct elements)
	OmitEmpty       bool
	OmitZero        bool
	String          bool   // marshal/unmarshal the field as a JSON-quoted string
	Format          string // jsonv2 format flag ("RFC3339", "unix", "hex", ...)
	Inline          bool   // catch-all map: absorbs unknown JSON keys on decode, splices entries on encode
	MultiErr        bool   // propagated from parent struct: use errs collection
	AllowDups       bool   // propagated from parent struct: skip duplicate-key guard
	NoValidate      bool   // propagated from parent struct: skip validation + mods
	UseNumber       bool   // propagated from parent struct: scan numbers into json.Number on KindAny fields
	HTMLEscape      bool   // propagated from parent struct: HTML-safe escape <, >, & when emitting strings (default: literal, matches jsonv2)
	Ignored         bool
}

type StructInfo struct {
	Name          string
	Fields        []FieldInfo
	BuildTag      string // canonical //go:build expression from the source file (empty when unconstrained)
	Marshal       bool   // emit json.Marshaler / json.MarshalerTo hooks
	Unmarshal     bool   // emit json.Unmarshaler / json.UnmarshalerFrom hooks
	MultiErr      bool   // collect validation errors instead of stopping on first
	AllowDups     bool   // do NOT error on duplicate JSON keys (opt-out of default)
	NoValidate    bool   // skip validation rules, required-field checks, and mods
	IgnoreUnknown bool   // skip unknown JSON keys silently (default: emit validation.Error{UnknownKey})
	NoSort        bool   // opt out of codegen-time struct-field sort by JSON name
	UseNumber     bool   // decode JSON numbers into `any` fields as json.Number instead of float64
	HTMLEscape    bool   // HTML-safe escape <, >, & in emitted strings (default: literal, matches jsonv2)
	Test          bool   // declared in a *_test.go file — route output to *_ggen_test.go

	// IsAlias marks a top-level named type that aliases a primitive or
	// (with type info) a struct, e.g. `type HtmlString string`,
	// `type Count int`, `type LocalUUID uuid.UUID`. Aliases generate the
	// same method surface as structs (DecodeFrom / DecodeFromStream /
	// JSONSize / AppendJSON) but the bodies are specialized:
	//
	//   - Primitive alias: AliasKind ∈ {KindString, KindBool, KindIntN,
	//     KindUintN, KindFloatN}; AliasUnderlying is the basic type name
	//     ("string", "int64", …). Bodies read/write the primitive and
	//     cast.
	//   - Struct alias: AliasKind == KindStruct. If the underlying type
	//     implements one of the standard marshal/unmarshal interfaces,
	//     AliasIface flags that. Codegen prefers method delegation
	//     (cast → underlying.Method() → cast back) over field-level
	//     introspection.
	IsAlias               bool
	AliasKind             TypeKind
	AliasUnderlying       string          // Go type literal for the underlying (e.g. "string", "uuid.UUID")
	AliasUnderlyingImport string          // import path when the underlying is from a foreign package; "" for same-pkg / stdlib basic types
	AliasIface            FieldInterfaces // method-set probe on the underlying struct (KindStruct aliases only)

	// AliasField captures the container shape for slice/map/array
	// aliases (`type Tags []string`, `type Lookup map[string]int`,
	// `type Tuple [3]int`). Only the Kind / ElemType / ElemKind /
	// ArrayLen / ElemPointer fields matter; the rest of FieldInfo is
	// inert. Codegen reuses the existing slice/map/array emitters by
	// passing this synthetic FieldInfo with `result` as ref.
	AliasField FieldInfo
}

// SQLNullKind describes a database/sql.NullX type: which concrete inner Go
// kind it carries and the field name on the struct (e.g. NullString.String,
// NullInt64.Int64). Renderers reuse the inner-kind primitives for parsing.
type SQLNullKind struct {
	Field string   // exported field name on the struct (String, Int64, …)
	Inner TypeKind // concrete kind of that field
	Type  string   // Go type of the inner field for casting (string, int64, time.Time, …)
}

// SQLNullSpec returns the spec for a known sql.NullX type name. Lookup is
// by full type string ("sql.NullString" etc.).
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
		return SQLNullKind{Field: "Byte", Inner: KindUint8, Type: "byte"}, true
	case "sql.NullBool":
		return SQLNullKind{Field: "Bool", Inner: KindBool, Type: "bool"}, true
	case "sql.NullFloat64":
		return SQLNullKind{Field: "Float64", Inner: KindFloat64, Type: "float64"}, true
	case "sql.NullTime":
		return SQLNullKind{Field: "Time", Inner: KindTime, Type: "time.Time"}, true
	}
	return SQLNullKind{}, false
}

// generatedTypes is the set of struct names that this generation pass is
// emitting code for. renderReadStruct / renderAppendValue consult it to decide
// whether to emit a direct DecodeFrom/AppendJSON call or a jsonv2 fallback
// (for cross-package / unannotated types).
var generatedTypes map[string]struct{}

// generatedAliasKinds maps each primitive-aliased type in the current
// generation pass to the kind of its underlying (e.g. "AliasString" →
// KindString, "Count" → KindInt). Lets the field-level codegen detect
// when a field's declared type is an aliased primitive so it can cast
// through the underlying for stdlib calls (strings.TrimSpace, etc.).
// Only populated for IsAlias && primitive-kind aliases; struct/container
// aliases are absent (their kind isn't a single primitive token).
var generatedAliasKinds map[string]TypeKind

// isGenerated reports whether name is a struct in the current generation pass.
// Small wrapper to keep call sites readable.
func isGenerated(name string) bool {
	_, ok := generatedTypes[name]
	return ok
}

func (f FieldInfo) IsRequired() bool {
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

// HasInline reports whether any field on s is a catch-all inline map.
func (s StructInfo) HasInline() bool {
	for _, f := range s.Fields {
		if f.Inline {
			return true
		}
	}
	return false
}

// InlineField returns the first inline field on s, or an empty FieldInfo if
// none. Called from the template — keep it value-returning.
func (s StructInfo) InlineField() FieldInfo {
	for _, f := range s.Fields {
		if f.Inline {
			return f
		}
	}
	return FieldInfo{}
}
