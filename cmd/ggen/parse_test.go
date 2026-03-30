package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseStructs(t *testing.T) {
	dir := t.TempDir()
	src := `package test

type Foo struct {
	Name    string  ` + "`" + `json:"name" jsonvalidate:"required,minlen=1"` + "`" + `
	Age     int     ` + "`" + `json:"age" jsonvalidate:"min=0,max=150"` + "`" + `
	Ignored string  ` + "`" + `json:"-"` + "`" + `
	Items   []string ` + "`" + `json:"items"` + "`" + `
}
`
	file := filepath.Join(dir, "test.go")
	if err := os.WriteFile(file, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	structs, err := parseStructs(file, []string{"Foo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(structs) != 1 {
		t.Fatalf("got %d structs, want 1", len(structs))
	}

	s := structs[0]
	if s.Name != "Foo" {
		t.Errorf("Name = %q, want %q", s.Name, "Foo")
	}
	// Ignored field should be excluded
	if len(s.Fields) != 3 {
		t.Fatalf("got %d fields, want 3 (Ignored should be excluded)", len(s.Fields))
	}

	// Name field
	f := s.Fields[0]
	if f.GoName != "Name" || f.JSONName != "name" || f.Kind != KindString {
		t.Errorf("field 0: got %+v", f)
	}
	if !f.IsRequired() {
		t.Error("Name should be required")
	}

	// Age field
	f = s.Fields[1]
	if f.GoName != "Age" || f.JSONName != "age" || f.Kind != KindInt {
		t.Errorf("field 1: got %+v", f)
	}

	// Items field
	f = s.Fields[2]
	if f.GoName != "Items" || f.JSONName != "items" || f.Kind != KindSlice {
		t.Errorf("field 2: got %+v", f)
	}
	if f.ElemType != "string" || f.ElemKind != KindString {
		t.Errorf("field 2 elem: type=%q kind=%d, want string/KindString", f.ElemType, f.ElemKind)
	}
}

func TestParseStructs_notFound(t *testing.T) {
	dir := t.TempDir()
	src := `package test
type Bar struct {}
`
	file := filepath.Join(dir, "test.go")
	if err := os.WriteFile(file, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := parseStructs(file, []string{"Foo"})
	if err == nil {
		t.Fatal("expected error for missing struct")
	}
}

func TestParseStructs_embedded(t *testing.T) {
	dir := t.TempDir()
	src := `package test
type Base struct { ID int }
type Derived struct {
	Base
	Name string
}
`
	file := filepath.Join(dir, "test.go")
	if err := os.WriteFile(file, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := parseStructs(file, []string{"Derived"})
	if err == nil {
		t.Fatal("expected error for embedded struct")
	}
}

func TestGenerate(t *testing.T) {
	structs := []StructInfo{{
		Name: "TestStruct",
		Fields: []FieldInfo{
			{GoName: "Name", JSONName: "name", GoType: "string", Kind: KindString,
				Validation: []ValidationRule{{Name: "required"}}},
			{GoName: "Count", JSONName: "count", GoType: "int", Kind: KindInt},
		},
	}}

	src, err := generate("testpkg", structs)
	if err != nil {
		t.Fatal(err)
	}

	code := string(src)
	// basic checks
	if len(code) == 0 {
		t.Fatal("empty output")
	}
	if !contains(code, "package testpkg") {
		t.Error("missing package declaration")
	}
	if !contains(code, "func (s TestStruct) ParseFrom") {
		t.Error("missing ParseFrom method")
	}
	if !contains(code, "func (s TestStruct) DecodeFrom") {
		t.Error("missing DecodeFrom method")
	}
	if !contains(code, "seenName") {
		t.Error("missing required field tracking for Name")
	}
	if !contains(code, `missing required field \"name\"`) {
		t.Error("missing required field check for Name")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && findSubstr(s, substr)
}

func findSubstr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
