// Package gen describes ggen-annotated Go types and their JSON contract for
// scripts that generate declarations or schemas in other languages.
//
// A script loads packages, picks types, places each type's input or output
// shape into files it names, and writes. Emitters turn placed shapes into
// text; the package knows nothing about any target language.
package gen

// Mode is the direction a Shape describes.
type Mode uint8

const (
	// In is what the generated decoder accepts.
	In Mode = iota
	// Out is what the generated encoder emits.
	Out
)

func (m Mode) String() string {
	if m == Out {
		return "output"
	}
	return "input"
}

// GoKind is the Go-side kind of a Type, after pointers are peeled.
type GoKind uint8

const (
	String GoKind = iota
	Bool
	Int
	Int8
	Int16
	Int32
	Int64
	Uint
	Uint8
	Uint16
	Uint32
	Uint64
	Float32
	Float64
	Time     // time.Time
	Duration // time.Duration
	Bytes    // []byte, [N]byte
	IP       // net.IP
	Addr     // netip.Addr
	Prefix   // netip.Prefix
	URL      // net/url.URL
	BigInt   // math/big.Int
	BigFloat // math/big.Float
	BigRat   // math/big.Rat
	RawJSON  // json.RawMessage, jsontext.Value
	Number   // json.Number
	Slice
	Array
	Map
	Struct
	Any
)

var goKindNames = [...]string{
	String: "string", Bool: "bool", Int: "int", Int8: "int8", Int16: "int16", Int32: "int32", Int64: "int64",
	Uint: "uint", Uint8: "uint8", Uint16: "uint16", Uint32: "uint32", Uint64: "uint64",
	Float32: "float32", Float64: "float64", Time: "time", Duration: "duration", Bytes: "bytes",
	IP: "ip", Addr: "addr", Prefix: "prefix", URL: "url", BigInt: "bigint", BigFloat: "bigfloat",
	BigRat: "bigrat", RawJSON: "rawjson", Number: "number", Slice: "slice", Array: "array",
	Map: "map", Struct: "struct", Any: "any",
}

func (k GoKind) String() string { return goKindNames[k] }

// Wire is the JSON shape of a Type.
type Wire uint8

const (
	WireString Wire = iota
	WireInteger
	WireNumber
	WireBool
	WireArray
	WireObject
	WireAny
)

func (w Wire) String() string {
	return [...]string{"string", "integer", "number", "bool", "array", "object", "any"}[w]
}

// Unknown is an object's policy for keys it does not declare.
type Unknown uint8

const (
	Reject  Unknown = iota // an undeclared key is an error
	Ignore                 // undeclared keys are skipped
	Collect                // undeclared keys go to the json:",embed" map; see Shape.Rest
)

func (u Unknown) String() string {
	return [...]string{"reject", "ignore", "collect"}[u]
}

// Op identifies a Rule.
type Op uint8

const (
	NotEmpty Op = iota
	Len         // byte length of a string, decoded length of bytes, item count of an array or map
	MinLen
	MaxLen
	Runes // code point count
	MinRunes
	MaxRunes
	GT
	GTE
	LT
	LTE
	Eq
	Neq
	Multiple
	OneOf // a oneof the Type could not narrow to an Enum, because a transform precedes it
	URLRule
	Alphanum
	Numeric
	Hex
	IsLower // contains no uppercase letter
	IsUpper // contains no lowercase letter
	Starts
	Ends
	Contains
	Trim
	TrimPrefix
	TrimSuffix
	ToLower
	ToUpper
	Replace
	Clamp
	Func
)

var opNames = [...]string{
	NotEmpty: "notempty", Len: "len", MinLen: "minlen", MaxLen: "maxlen", Runes: "runes",
	MinRunes: "minrunes", MaxRunes: "maxrunes", GT: "gt", GTE: "gte", LT: "lt", LTE: "lte",
	Eq: "eq", Neq: "neq", Multiple: "multiple", OneOf: "oneof", URLRule: "url",
	Alphanum: "alphanum", Numeric: "numeric", Hex: "hexadecimal", IsLower: "islower",
	IsUpper: "isupper", Starts: "starts", Ends: "ends", Contains: "contains", Trim: "trim",
	TrimPrefix: "trimleft", TrimSuffix: "trimright", ToLower: "tolower", ToUpper: "toupper",
	Replace: "replace", Clamp: "clamp", Func: "func",
}

// String is the rule's name as written in a pipe: tag.
func (o Op) String() string { return opNames[o] }
