// Shared annotated structs used across multiple test files. Feature-specific
// structs (HookedStruct, PointerStruct, NativeTypes, OmitStruct, …) live next
// to the test functions that exercise them.

package integrationtests

//go:generate ../ggen $GOFILE

//ggen:generate
type Address struct {
	Street  string `json:"street" pipe:"required minlen=1 maxlen=200"`
	City    string `json:"city" pipe:"required notempty"`
	ZipCode string `json:"zipCode" pipe:"required len=5"`
}

// Node is a recursive tree node mixing scalars, slices, maps, and a
// self-referential child array.
//
//ggen:generate
type Node struct {
	ID       int64             `json:"id"`
	Name     string            `json:"name"`
	Score    float64           `json:"score"`
	Active   bool              `json:"active"`
	Tags     []string          `json:"tags"`
	Props    map[string]string `json:"props"`
	Children []Node            `json:"children"`
}

// WideStruct exercises the bitmask seen-flag path: 40 required fields, past the
// 32-field threshold.
//
//ggen:generate
type WideStruct struct {
	F1  string `json:"f1" pipe:"required"`
	F2  string `json:"f2" pipe:"required"`
	F3  string `json:"f3" pipe:"required"`
	F4  string `json:"f4" pipe:"required"`
	F5  string `json:"f5" pipe:"required"`
	F6  string `json:"f6" pipe:"required"`
	F7  string `json:"f7" pipe:"required"`
	F8  string `json:"f8" pipe:"required"`
	F9  string `json:"f9" pipe:"required"`
	F10 string `json:"f10" pipe:"required"`
	F11 string `json:"f11" pipe:"required"`
	F12 string `json:"f12" pipe:"required"`
	F13 string `json:"f13" pipe:"required"`
	F14 string `json:"f14" pipe:"required"`
	F15 string `json:"f15" pipe:"required"`
	F16 string `json:"f16" pipe:"required"`
	F17 string `json:"f17" pipe:"required"`
	F18 string `json:"f18" pipe:"required"`
	F19 string `json:"f19" pipe:"required"`
	F20 string `json:"f20" pipe:"required"`
	F21 string `json:"f21" pipe:"required"`
	F22 string `json:"f22" pipe:"required"`
	F23 string `json:"f23" pipe:"required"`
	F24 string `json:"f24" pipe:"required"`
	F25 string `json:"f25" pipe:"required"`
	F26 string `json:"f26" pipe:"required"`
	F27 string `json:"f27" pipe:"required"`
	F28 string `json:"f28" pipe:"required"`
	F29 string `json:"f29" pipe:"required"`
	F30 string `json:"f30" pipe:"required"`
	F31 string `json:"f31" pipe:"required"`
	F32 string `json:"f32" pipe:"required"`
	F33 string `json:"f33" pipe:"required"`
	F34 string `json:"f34" pipe:"required"`
	F35 string `json:"f35" pipe:"required"`
	F36 string `json:"f36" pipe:"required"`
	F37 string `json:"f37" pipe:"required"`
	F38 string `json:"f38" pipe:"required"`
	F39 string `json:"f39" pipe:"required"`
	F40 string `json:"f40" pipe:"required"`
}
