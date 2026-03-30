package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
)

func parseStructs(filename string, structNames []string) ([]StructInfo, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", filename, err)
	}

	nameSet := make(map[string]bool, len(structNames))
	for _, n := range structNames {
		nameSet[n] = true
	}

	var structs []StructInfo
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.TYPE {
			continue
		}
		for _, spec := range genDecl.Specs {
			typeSpec := spec.(*ast.TypeSpec)
			if !nameSet[typeSpec.Name.Name] {
				continue
			}
			structType, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				return nil, fmt.Errorf("%s is not a struct type", typeSpec.Name.Name)
			}
			info, err := extractStruct(typeSpec.Name.Name, structType)
			if err != nil {
				return nil, err
			}
			structs = append(structs, info)
			delete(nameSet, typeSpec.Name.Name)
		}
	}

	if len(nameSet) > 0 {
		var missing []string
		for n := range nameSet {
			missing = append(missing, n)
		}
		return nil, fmt.Errorf("structs not found: %s", strings.Join(missing, ", "))
	}

	return structs, nil
}

func extractStruct(name string, st *ast.StructType) (StructInfo, error) {
	info := StructInfo{Name: name}
	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			return info, fmt.Errorf("embedded fields not supported in %s", name)
		}
		for _, ident := range field.Names {
			if !ident.IsExported() {
				continue
			}
			fi, err := extractField(ident.Name, field)
			if err != nil {
				return info, fmt.Errorf("field %s.%s: %w", name, ident.Name, err)
			}
			if fi.Ignored {
				continue
			}
			info.Fields = append(info.Fields, fi)
		}
	}
	return info, nil
}

func extractField(goName string, field *ast.Field) (FieldInfo, error) {
	fi := FieldInfo{GoName: goName}

	// parse tags
	if field.Tag != nil {
		tag := reflect.StructTag(strings.Trim(field.Tag.Value, "`"))
		jsonName, ignored := parseJSONTag(tag.Get("json"))
		if ignored {
			fi.Ignored = true
			return fi, nil
		}
		if jsonName != "" {
			fi.JSONName = jsonName
		}
		fi.Validation = parseValidationTag(tag.Get("jsonvalidate"))
	}
	if fi.JSONName == "" {
		fi.JSONName = goName
	}

	// resolve type
	goType := exprToString(field.Type)
	fi.GoType = goType
	fi.Kind = resolveKind(goType)

	if fi.Kind == KindSlice {
		arr, ok := field.Type.(*ast.ArrayType)
		if ok {
			fi.ElemType = exprToString(arr.Elt)
			fi.ElemKind = resolveKind(fi.ElemType)
		}
	}

	return fi, nil
}

func exprToString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return exprToString(e.X) + "." + e.Sel.Name
	case *ast.ArrayType:
		if e.Len == nil {
			return "[]" + exprToString(e.Elt)
		}
		return fmt.Sprintf("[%s]%s", exprToString(e.Len), exprToString(e.Elt))
	case *ast.StarExpr:
		return "*" + exprToString(e.X)
	case *ast.BasicLit:
		return e.Value
	default:
		return fmt.Sprintf("%T", expr)
	}
}

func resolveKind(goType string) TypeKind {
	switch goType {
	case "string":
		return KindString
	case "int":
		return KindInt
	case "int64":
		return KindInt64
	case "uint64":
		return KindUint64
	case "float64":
		return KindFloat64
	case "bool":
		return KindBool
	default:
		if strings.HasPrefix(goType, "[]") {
			return KindSlice
		}
		return KindStruct
	}
}
