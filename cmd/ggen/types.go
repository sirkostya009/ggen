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
)

type ValidationRule struct {
	Name  string // "required", "minlen", "maxlen", "min", "max"
	Value string // parameter value, e.g. "2" for minlen=2; empty for "required"
}

type FieldInfo struct {
	GoName     string
	JSONName   string
	GoType     string // full Go type as string, e.g. "string", "[]int", "SomeStruct"
	Kind       TypeKind
	ElemType   string // for slices: element type (e.g. "string" for []string)
	ElemKind   TypeKind
	Validation []ValidationRule
	Ignored    bool
}

type StructInfo struct {
	Name    string
	Fields  []FieldInfo
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
