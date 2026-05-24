// Shared annotated structs used across multiple test files. Feature-specific
// structs (HookedStruct, PointerStruct, NativeTypes, OmitStruct, …) live next
// to the test functions that exercise them.

package integrationtests

//go:generate ../ggen $GOFILE

//ggen:generate
type Address struct {
	Street  string `json:"street" ggen:"required,minlen=1,maxlen=200"`
	City    string `json:"city" ggen:"required,notempty"`
	ZipCode string `json:"zipCode" ggen:"required,len=5"`
}

// Node is a recursive tree node used for the ~1 MB deep-payload benchmark.
// Shape mirrors what sonic uses in their macro benchmarks: a mix of scalars,
// slices, maps, and a self-referential child array.
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

// WideStruct exercises the bitmask-seen-flag path. The codegen swaps
// per-field bools for a packed bitmask once the field count crosses
// seenBitmaskThreshold (32). This struct has 40 required fields so
// every dispatch site uses bit-set/bit-test instead of bool stores.
//
//ggen:generate
type WideStruct struct {
	F1  string `json:"f1" ggen:"required"`
	F2  string `json:"f2" ggen:"required"`
	F3  string `json:"f3" ggen:"required"`
	F4  string `json:"f4" ggen:"required"`
	F5  string `json:"f5" ggen:"required"`
	F6  string `json:"f6" ggen:"required"`
	F7  string `json:"f7" ggen:"required"`
	F8  string `json:"f8" ggen:"required"`
	F9  string `json:"f9" ggen:"required"`
	F10 string `json:"f10" ggen:"required"`
	F11 string `json:"f11" ggen:"required"`
	F12 string `json:"f12" ggen:"required"`
	F13 string `json:"f13" ggen:"required"`
	F14 string `json:"f14" ggen:"required"`
	F15 string `json:"f15" ggen:"required"`
	F16 string `json:"f16" ggen:"required"`
	F17 string `json:"f17" ggen:"required"`
	F18 string `json:"f18" ggen:"required"`
	F19 string `json:"f19" ggen:"required"`
	F20 string `json:"f20" ggen:"required"`
	F21 string `json:"f21" ggen:"required"`
	F22 string `json:"f22" ggen:"required"`
	F23 string `json:"f23" ggen:"required"`
	F24 string `json:"f24" ggen:"required"`
	F25 string `json:"f25" ggen:"required"`
	F26 string `json:"f26" ggen:"required"`
	F27 string `json:"f27" ggen:"required"`
	F28 string `json:"f28" ggen:"required"`
	F29 string `json:"f29" ggen:"required"`
	F30 string `json:"f30" ggen:"required"`
	F31 string `json:"f31" ggen:"required"`
	F32 string `json:"f32" ggen:"required"`
	F33 string `json:"f33" ggen:"required"`
	F34 string `json:"f34" ggen:"required"`
	F35 string `json:"f35" ggen:"required"`
	F36 string `json:"f36" ggen:"required"`
	F37 string `json:"f37" ggen:"required"`
	F38 string `json:"f38" ggen:"required"`
	F39 string `json:"f39" ggen:"required"`
	F40 string `json:"f40" ggen:"required"`
}

// PtrSliceStruct exercises the slab-allocated `[]*T` and `[N]*T` paths:
// element pointers come from a single backing slab so N allocs collapse
// to ~log(N). nil elements decode to nil pointers (no slab slot used).
//
//ggen:generate
type PtrSliceStruct struct {
	Items []*Address  `json:"items"`
	Tuple [3]*Address `json:"tuple"`
	Nodes []*Node     `json:"nodes"`
}
