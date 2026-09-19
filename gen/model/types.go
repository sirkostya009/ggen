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

	// Custom rule resolution (Name starts with "@"): looked up via go/types,
	// emitted as a direct call. PkgImport/PkgName empty for same-package;
	// otherwise the import path + qualifier the generated call uses.
	Custom    bool
	PkgImport string
	PkgName   string
	FuncName  string

	// BoolForm marks a `func(T) bool` validator (vs `func(T) error`); a false
	// return emits ggen.PredicateError. Msg is the optional inline
	// message (`@Even:'must be even'`), bool-form only.
	BoolForm bool
	Msg      string
}

// ModRule is an input-modification step applied after decoding but before
// validation (e.g., trim whitespace, lowercase).
type ModRule struct {
	Name  string // "trim", "lower", "upper", "trimleft", "trimright", "replace", or "@FuncName"
	Value string // parameter (empty for trim/lower/upper)

	// Custom mod resolution — same shape as ValidationRule's, plus Fallible
	// (func returns (T, error)/(T, bool)); a fallible error surfaces as a parse
	// error, not a validation error.
	Custom    bool
	PkgImport string
	PkgName   string
	FuncName  string
	Fallible  bool // returns a two-tuple (T, error) or (T, bool)

	// BoolForm marks `func(T) (T, bool)` (vs the error form); a false return
	// emits ggen.ModError. Msg is the optional inline message, bool-form only.
	BoolForm bool
	Msg      string
}

type FieldInfo struct {
	Doc        string     // doc comment (or trailing comment), directive lines removed
	Directives []string   // `//word:...` lines of the doc comment, without the slashes
	Type       types.Type // resolved go/types type; nil in the AST-only degraded mode
	GoName     string
	StructName string // owning struct's Go name; used in error diagnostics
	EmbedDepth int    // embedding levels between the declaring struct and the parent (0 = the parent's own field); dominant-field rule keeps the shallowest
	JSONName   string
	GoType     string // full Go type as string, e.g. "string", "[]int", "*Address"
	Kind       TypeKind
	ElemType   string // for slices/arrays: element type (e.g. "string" for []string)
	ElemKind   TypeKind
	ArrayLen   int // for KindArray: fixed array length N (peel sets it on nested inners)
	// when ElemKind == KindArray, N of the inner [N]T at this level (slice/array elements; a map value
	// leaves it 0 — read N from ElemType)
	ElemArrayLen    int
	ElemPointer     bool               // true when the slice/array element is `*T`; ElemType is the pointee
	Pointer         bool               // true when the field is *T; Kind describes the pointee
	PointeeType     string             // for pointer fields: pointee Go type ("string", "Address")
	Validation      []ValidationRule   // applies to the field (or whole slice)
	ElemValidation  []ValidationRule   // level-1 inner: rules (per-element for slices, per-value for maps)
	InnerValidation [][]ValidationRule // levels 2..N (one slice per extra `inner:`)
	KeyValidation   []ValidationRule   // rules after `keys:` — map keys only (always string)
	Mods            []ModRule          // input transforms applied after decode
	ElemMods        []ModRule          // level-1 inner: mods
	InnerMods       [][]ModRule        // levels 2..N
	KeyMods         []ModRule          // `keys:` mods — map keys only

	// Unified ordered pipeline (the `pipe:` tag). Pipe/KeyPipe/Levels are the
	// SOURCE OF TRUTH for value-stage emit order (mods + validators interleaved
	// per level); the split buckets above are DERIVED for the order-independent
	// consumers.
	Presence Presence  // required / optional (lifted from the pipe)
	Variants []Variant // decode stage; nil => implicit native decode of the field type
	Pipe     []Step    // outer value steps (whole field / container, after decode)
	KeyPipe  []Step    // map-key value steps
	Levels   [][]Step  // dive levels: Levels[0] per-element, Levels[1] deeper, …
	// explicit preallocation hint for slices/maps; -1=unset (fall through to len/minlen/default),
	// 0=user opt-out (no prealloc), N>0=use N as cap. Overrides len/minlen.
	HintLen    int
	HintLevels []int           // per-dive prealloc hints from the `hint:` tag (entry -1 = unset); HintLevels[0] sizes the level-1 row, etc.
	Iface      FieldInterfaces // statically detected method-set membership (TextMarshaler, ByteDecoder, ...)
	ElemIface  FieldInterfaces // method-set probe on the slice/array/map element type (used by size estimators for struct elements)
	OmitEmpty  bool
	OmitZero   bool
	// decode: accept an explicit JSON null on this (non-pointer) value field, setting it to its Go
	// zero value instead of erroring. No-op on already-null-aware kinds
	// (pointer/slice/map/[]byte/sql.Null*/raw/any)
	NullZero   bool
	String     bool   // marshal/unmarshal the field as a JSON-quoted string
	Format     string // jsonv2 format flag ("RFC3339", "unix", "hex", ...)
	Embed      bool   // embedded fallback map: absorbs unknown JSON keys on decode, splices entries on encode
	MultiErr   bool   // propagated from parent struct: use errs collection
	AllowDups  bool   // propagated from parent struct: skip duplicate-key guard
	NoValidate bool   // propagated from parent struct: skip validation + mods
	UseNumber  bool   // propagated from parent struct: scan numbers into json.Number on KindAny fields
	HTMLEscape bool   // propagated from parent struct: HTML-safe escape <, >, & when emitting strings (default: literal, matches jsonv2)
	Copy       bool   // propagated from parent struct: bytes-path decode copies retained strings/RawMessage/any instead of aliasing data
	// propagated from parent struct: skip decode UTF-8 validation (strings pass raw bytes through,
	// unpaired surrogates → U+FFFD, raw spans unchecked)
	AllowInvalidUTF8 bool
	Ignored          bool
	NotComparable    bool // go/types says the field's type is not comparable (omitzero needs a reflect zero probe, `!= (T{})` would not compile)
	// go/types says the field's OWN type (pointers not peeled) has a struct underlying; only go/types
	// separates a struct from the named containers and foreign array types that also read as
	// KindStruct
	UnderlyingStruct bool

	// SQLNullInner, when non-nil, marks a generic database/sql.Null[T] (Go
	// 1.22): the synthetic FieldInfo for T. Renderers delegate the V slot to
	// the standard emitters with it, so sql.Null[T] gets the bare-T wire shape
	// ("T's wire or null"). Named sql.NullX flavors keep the SQLNullSpec path
	// (this stays nil). SQLNullImports holds the foreign imports the emitted
	// type literals reference.
	SQLNullInner   *FieldInfo
	SQLNullImports []TypeImport

	// TypeImports holds every foreign package named anywhere in the field's
	// type (the type itself, a slice/array element, a map key/value, a
	// pointee, a type argument). The emitters write those type names verbatim
	// — `var v leaf.Leaf`, `append(dst, leaf.Leaf{})`, `map[string]leaf.Leaf{}`
	// — so without these the generated file names a package it never imports.
	// They are CANDIDATES: a value field of a cross-package type only ever
	// writes `result.X`, so collectImports keeps the ones whose qualifier
	// actually appears in a rendered body. Populated from go/types; empty on
	// the AST-only path, where cross-package types route to encoding/json.
	TypeImports []TypeImport

	// NamedPrims records every named type this field's type mentions whose
	// underlying type is a primitive — the field type itself, a slice/array
	// element, a map key/value, a pointee. Merged into namedKinds before
	// codegen so rules and mods on `type Priority string` resolve and cast
	// through `string` whether or not the user annotated Priority.
	NamedPrims map[string]TypeKind

	// Codegen-internal flags — never set by the parse layer.
	// value emit sits directly inside the key-dispatch switch; a `null` match may `break` to the comma
	// handling instead of nesting the whole value decode in an else
	AtDispatch bool
	// decode target is a freshly-declared nil local (map-value temp, pre-grown []**T slot) — skip the
	// receiver seed and collapse the pointer assign cascade to a straight new-chain
	TargetNil bool
	// the parent element loop already consumed a `null` for this slot (nil-elem fast path) — the
	// nested container emitter skips its own null peek, so its body isn't wrapped in an else
	NullDone bool
}

type StructInfo struct {
	Doc           string     // doc comment, directive lines removed
	Directives    []string   // `//word:...` lines of the doc comment, without the slashes
	File          string     // declaring source file; empty in the AST-only degraded mode
	Type          types.Type // the declared named type; nil in the AST-only degraded mode
	Name          string
	Fields        []FieldInfo
	BuildTag      string // canonical //go:build expression from the source file (empty when unconstrained)
	Marshal       bool   // emit json.Marshaler / json.MarshalerTo hooks
	Unmarshal     bool   // emit json.Unmarshaler / json.UnmarshalerFrom hooks
	MultiErr      bool   // collect validation errors instead of stopping on first
	AllowDups     bool   // do NOT error on duplicate JSON keys (opt-out of default)
	NoValidate    bool   // skip validation rules, required-field checks, and mods
	IgnoreUnknown bool   // skip unknown JSON keys silently (default: emit ggen.Error{UnknownKey})
	NullZero      bool   // accept explicit JSON null on every non-pointer value field (null → Go zero); per-field json:",nullzero" can opt a single field in
	NoSort        bool   // opt out of codegen-time struct-field sort by JSON name
	UseNumber     bool   // decode JSON numbers into `any` fields as json.Number instead of float64
	HTMLEscape    bool   // HTML-safe escape <, >, & in emitted strings (default: literal, matches jsonv2)
	// bytes-path DecodeFrom copies retained strings/RawMessage/any out of data instead of aliasing it
	// (matches the stream path's lifetime semantics)
	Copy             bool
	AllowInvalidUTF8 bool // opt out of decode UTF-8 validation (jsonv2 parity) for this struct's strings + raw spans
	Test             bool // declared in a *_test.go file — route output to *_ggen_test.go
	XTest            bool // declared in the external test package (`package foo_test`) — own output file + package clause

	// IsAlias marks a top-level named type aliasing a primitive or struct
	// (`type Count int`, `type LocalUUID uuid.UUID`). Aliases get the same
	// method surface as structs with specialized bodies:
	//   - Primitive: AliasKind is a primitive kind, AliasUnderlying the basic
	//     type name; bodies read/write the primitive and cast.
	//   - Struct: AliasKind == KindStruct; AliasIface flags marshal/unmarshal
	//     support and codegen prefers method delegation over introspection.
	IsAlias               bool
	AliasKind             TypeKind
	AliasUnderlying       string          // Go type literal for the underlying (e.g. "string", "uuid.UUID")
	AliasUnderlyingImport TypeImport      // the underlying's package when foreign; zero for same-pkg / stdlib basic types
	AliasIface            FieldInterfaces // method-set probe on the underlying struct (KindStruct aliases only)

	// AliasField captures the container shape for slice/map/array aliases
	// (`type Tags []string`, `type Tuple [3]int`). Only Kind/ElemType/ElemKind/
	// ArrayLen/ElemPointer matter; codegen feeds it to the container emitters.
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
		return SQLNullKind{Field: "Byte", Inner: KindUint8, Type: "uint8"}, true
	case "sql.NullBool":
		return SQLNullKind{Field: "Bool", Inner: KindBool, Type: "bool"}, true
	case "sql.NullFloat64":
		return SQLNullKind{Field: "Float64", Inner: KindFloat64, Type: "float64"}, true
	case "sql.NullTime":
		return SQLNullKind{Field: "Time", Inner: KindTime, Type: "time.Time"}, true
	}
	// Generic sql.Null[T] (Go 1.22): inner field is always V. Gated to inners
	// the renderers handle.
	if inner, ok := sqlNullGenericInner(goType); ok {
		if k := ResolveKind(inner); isSupportedSQLNullInner(k) {
			return SQLNullKind{Field: "V", Inner: k, Type: inner}, true
		}
	}
	return SQLNullKind{}, false
}

// TypeImport is one foreign package a field's type names, with the qualifier
// the generated code spells it by. Explicit means the qualifier is not the
// package's declared name, so the import line must bind it: `Name "Path"`.
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

// EmbedField returns the first embedded fallback field on s, or an empty FieldInfo if
// none. Called from the template — keep it value-returning.
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
// returns (depth, leafType). depth==1 is a plain `*T`; depth>=2 is a
// multi-level pointer (`**T`, `***T`, …) whose leaf is the type left after
// stripping every `*`.
func PointerDepth(goType string) (int, string) {
	depth := 0
	for len(goType) > 0 && goType[0] == '*' {
		depth++
		goType = goType[1:]
	}
	return depth, goType
}

// StripOneContainer peels off one layer of container (slice `[]` or array
// `[N]`) from a Go type string, returning the element type, its kind, and
// N (0 for slices / non-containers).
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
