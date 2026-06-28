package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/types"
	"strconv"
	"strings"
)

// customFunc holds the resolution for a `@Func` / `@pkg.Func` reference,
// stamped onto the Validation/ModRule so codegen emits a direct call.
type customFunc struct {
	PkgImport string // import path; "" for same-package
	PkgName   string // canonical name to qualify the call in generated code; "" for same-package
	FuncName  string // bare function name (no "@", no "pkg.")
	Fallible  bool   // mods only: true when signature is `func(T) (T, error)`
}

// lookupFunc resolves a `@Func` / `@pkg.Func` reference (the part after the
// `@`) to its *types.Func plus the import path / package name the generated
// call must qualify with. Shared by classifyValueFunc and classifyConverter.
func lookupFunc(ref string, file *ast.File, pkg *types.Package) (fn *types.Func, pkgImp, pkgName string, err error) {
	if ref == "" {
		return nil, "", "", fmt.Errorf("empty @ reference")
	}
	pkgPart, funcPart, hasDot := strings.Cut(ref, ".")
	if !hasDot {
		funcPart = pkgPart
		pkgPart = ""
	}
	if pkgPart == "" {
		if pkg == nil {
			return nil, "", "", fmt.Errorf("no package context (run ggen with a Go module so type info is available)")
		}
		obj := pkg.Scope().Lookup(funcPart)
		if obj == nil {
			return nil, "", "", fmt.Errorf("func %s not found in package %s", funcPart, pkg.Name())
		}
		f, ok := obj.(*types.Func)
		if !ok {
			return nil, "", "", fmt.Errorf("%s is not a function (got %T)", funcPart, obj)
		}
		return f, "", "", nil
	}
	target, importPath, err := lookupCrossPkg(pkgPart, file, pkg)
	if err != nil {
		return nil, "", "", fmt.Errorf("%w", err)
	}
	obj := target.Scope().Lookup(funcPart)
	if obj == nil {
		return nil, "", "", fmt.Errorf("func %s not found in package %s", funcPart, target.Path())
	}
	f, ok := obj.(*types.Func)
	if !ok {
		return nil, "", "", fmt.Errorf("%s is not a function (got %T)", funcPart, obj)
	}
	return f, importPath, target.Name(), nil
}

// pipeRole classifies a value-stage `@Func`: validator, mod (pure or
// fallible, in-type == out-type), or converter (in != out — only legal in the
// decode stage, rejected here).
type pipeRole uint8

const (
	roleValidator pipeRole = iota
	roleMod
	roleConverter
)

// classifyValueFunc resolves a value-stage `@Func` against the working type wt
// and classifies it from its signature (a single bool or error return is
// always a validator):
//
//	func(T) error         → validator
//	func(T) bool          → validator (message-capable)   [func(bool)bool banned]
//	func(T) T             → mod (pure)
//	func(T) (T, error)    → mod (fallible, error)
//	func(T) (T, bool)     → mod (fallible, message-capable)
//	func(T) U  (U != T)   → converter — illegal in a value stage
func classifyValueFunc(ref string, wt types.Type, file *ast.File, pkg *types.Package) (role pipeRole, cf customFunc, boolForm bool, err error) {
	fn, pkgImp, pkgName, err := lookupFunc(ref, file, pkg)
	if err != nil {
		return 0, customFunc{}, false, err
	}
	_, funcPart, hasDot := strings.Cut(ref, ".")
	if !hasDot {
		funcPart = ref
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		return 0, customFunc{}, false, fmt.Errorf("not a function signature")
	}
	if sig.Recv() != nil {
		return 0, customFunc{}, false, fmt.Errorf("must be a top-level function, not a method")
	}
	if sig.Params().Len() != 1 {
		return 0, customFunc{}, false, fmt.Errorf("must take exactly one parameter (the value)")
	}
	cf = customFunc{PkgImport: pkgImp, PkgName: pkgName, FuncName: funcPart}

	paramType := sig.Params().At(0).Type()
	paramTP, paramGeneric := paramType.(*types.TypeParam)
	if paramGeneric {
		if !satisfiesConstraint(wt, paramTP) {
			return 0, customFunc{}, false, fmt.Errorf("value type %s does not satisfy constraint %s on param", wt, paramTP.Constraint())
		}
	} else if !types.Identical(paramType, wt) {
		return 0, customFunc{}, false, fmt.Errorf("param type %s does not match value type %s", paramType, wt)
	}
	matchesWT := func(r types.Type) bool {
		if types.Identical(r, wt) {
			return true
		}
		if rtp, ok := r.(*types.TypeParam); ok && paramGeneric {
			return rtp == paramTP
		}
		return false
	}
	wtIsBool := false
	if b, ok := wt.Underlying().(*types.Basic); ok && b.Kind() == types.Bool {
		wtIsBool = true
	}

	res := sig.Results()
	switch res.Len() {
	case 1:
		rt := res.At(0).Type()
		switch {
		case isErrorType(rt):
			return roleValidator, cf, false, nil
		case isBoolType(rt):
			// func(bool)bool is banned (ambiguous; use func(bool) error).
			if wtIsBool {
				return 0, customFunc{}, false, fmt.Errorf("func(bool) bool is banned — use func(bool) error to validate a bool field")
			}
			return roleValidator, cf, true, nil
		case matchesWT(rt):
			return roleMod, cf, false, nil // pure mod
		default:
			return roleConverter, cf, false, fmt.Errorf("converter (func(%s) %s) is only valid as a decode-stage variant, not a value step", wt, rt)
		}
	case 2:
		rt, st := res.At(0).Type(), res.At(1).Type()
		if !matchesWT(rt) {
			return roleConverter, cf, false, fmt.Errorf("converter (func(%s) (%s, …)) is only valid as a decode-stage variant, not a value step", wt, rt)
		}
		cf.Fallible = true
		switch {
		case isErrorType(st):
			return roleMod, cf, false, nil
		case isBoolType(st):
			return roleMod, cf, true, nil
		default:
			return 0, customFunc{}, false, fmt.Errorf("fallible mod second result must be error or bool, got %s", st)
		}
	}
	return 0, customFunc{}, false, fmt.Errorf("must return one of: error, bool, T, (T, error), (T, bool); got %d results", res.Len())
}

// classifyConverter resolves a decode-stage converter variant `@Conv`. Unlike
// a value func it is OUTPUT-anchored: the first result must equal the field
// type T; the single parameter W is the type ggen scans natively (its wire
// shape decides the JSON shape this variant claims). Returns W's Go type
// literal. W must be builtin or same-package; foreign inputs are unsupported.
func classifyConverter(ref string, fieldType types.Type, qualifier types.Qualifier, file *ast.File, pkg *types.Package) (cf customFunc, inType string, boolForm bool, err error) {
	fn, pkgImp, pkgName, err := lookupFunc(ref, file, pkg)
	if err != nil {
		return customFunc{}, "", false, err
	}
	_, funcPart, hasDot := strings.Cut(ref, ".")
	if !hasDot {
		funcPart = ref
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		return customFunc{}, "", false, fmt.Errorf("not a function signature")
	}
	if sig.Recv() != nil {
		return customFunc{}, "", false, fmt.Errorf("must be a top-level function, not a method")
	}
	if sig.Params().Len() != 1 {
		return customFunc{}, "", false, fmt.Errorf("converter must take exactly one parameter (the scanned input)")
	}
	w := sig.Params().At(0).Type()
	if named, isNamed := w.(*types.Named); isNamed && named.Obj().Pkg() != nil && named.Obj().Pkg() != pkg {
		return customFunc{}, "", false, fmt.Errorf("converter input %s is from another package; only builtin or same-package input types are supported", w)
	}
	cf = customFunc{PkgImport: pkgImp, PkgName: pkgName, FuncName: funcPart}
	res := sig.Results()
	switch res.Len() {
	case 1:
		if !types.Identical(res.At(0).Type(), fieldType) {
			return customFunc{}, "", false, fmt.Errorf("converter output %s must equal field type %s", res.At(0).Type(), fieldType)
		}
	case 2:
		if !types.Identical(res.At(0).Type(), fieldType) {
			return customFunc{}, "", false, fmt.Errorf("converter first result %s must equal field type %s", res.At(0).Type(), fieldType)
		}
		cf.Fallible = true
		switch {
		case isErrorType(res.At(1).Type()):
		case isBoolType(res.At(1).Type()):
			boolForm = true
		default:
			return customFunc{}, "", false, fmt.Errorf("converter second result must be error or bool, got %s", res.At(1).Type())
		}
	default:
		return customFunc{}, "", false, fmt.Errorf("converter must return T or (T, error) or (T, bool), got %d results", res.Len())
	}
	return cf, types.TypeString(w, qualifier), boolForm, nil
}

// isBoolType reports whether t is the predeclared bool.
func isBoolType(t types.Type) bool {
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Kind() == types.Bool
}

// satisfiesConstraint reports whether t may instantiate the type parameter tp
// (type-set membership check against tp's constraint interface).
func satisfiesConstraint(t types.Type, tp *types.TypeParam) bool {
	constraint := tp.Constraint()
	if iface, ok := constraint.Underlying().(*types.Interface); ok {
		return types.Satisfies(t, iface)
	}
	return false
}

// lookupCrossPkg resolves an alias or package name written before the `.`
// in a `@pkg.Func` reference. Returns the target *types.Package plus its
// import path so the generator can add it to the generated file.
func lookupCrossPkg(pkgPart string, file *ast.File, pkg *types.Package) (*types.Package, string, error) {
	if file != nil {
		// Pass 1: file-scoped aliases (`import alias "path"`).
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			alias := ""
			if imp.Name != nil {
				alias = imp.Name.Name
			}
			target := findImportedPackage(pkg, path)
			if target == nil {
				continue
			}
			switch alias {
			case pkgPart:
				return target, path, nil
			case "":
				if target.Name() == pkgPart {
					return target, path, nil
				}
			case "_":
				// Blank import: no file-scoped name; match the declared name.
				if target.Name() == pkgPart {
					return target, path, nil
				}
			}
		}
	}
	// Pass 2: any transitive import whose declared name matches (catches an
	// import living in a different file of the same package).
	for _, imp := range pkg.Imports() {
		if imp.Name() == pkgPart {
			return imp, imp.Path(), nil
		}
	}
	return nil, "", fmt.Errorf("no import alias or package named %s in scope", pkgPart)
}

// findImportedPackage returns the *types.Package corresponding to the given
// import path among pkg's transitive imports.
func findImportedPackage(pkg *types.Package, path string) *types.Package {
	if pkg == nil {
		return nil
	}
	for _, imp := range pkg.Imports() {
		if imp.Path() == path {
			return imp
		}
	}
	return nil
}

// isErrorType reports whether t is the universe error interface.
func isErrorType(t types.Type) bool {
	if named, ok := t.(*types.Named); ok {
		return named.Obj().Name() == "error" && named.Obj().Pkg() == nil
	}
	return false
}

// pipeHasCustom reports whether any unclassified `@`-step is present on fi's
// pipe buckets (customs are parked as validator-shaped steps with Name "@…"
// until classifyValueFunc runs).
func pipeHasCustom(fi *FieldInfo) bool {
	has := func(steps []Step) bool {
		for _, s := range steps {
			if strings.HasPrefix(s.V.Name, "@") || strings.HasPrefix(s.M.Name, "@") {
				return true
			}
		}
		return false
	}
	if has(fi.Pipe) || has(fi.KeyPipe) {
		return true
	}
	for _, lv := range fi.Levels {
		if has(lv) {
			return true
		}
	}
	return false
}

// pipeHasConverter reports whether fi has a decode-stage converter variant.
func pipeHasConverter(fi *FieldInfo) bool {
	for _, v := range fi.Variants {
		if v.Kind == VariantConvert {
			return true
		}
	}
	return false
}

// resolvePipeCustoms classifies and resolves every `@`-step on a pipe-tagged
// field against the working type at its level, then re-derives the split
// buckets. Mod vs validator is decided by signature here.
func (s *structSet) resolvePipeCustoms(structName string, fi *FieldInfo, fieldType types.Type) error {
	if !pipeHasCustom(fi) && !pipeHasConverter(fi) {
		deriveBuckets(fi)
		return nil
	}
	if s.typesInfo == nil || fieldType == nil {
		return fmt.Errorf("`@Func` references require Go module context (run ggen inside a Go module so packages.Load can resolve types)")
	}
	file := s.structFile[structName]
	pkg := s.typesPkg
	qualifier := types.RelativeTo(pkg)
	var errs []error

	// Decode-stage converter variants: resolve OUTPUT==T, capture input W.
	for i := range fi.Variants {
		v := &fi.Variants[i]
		if v.Kind != VariantConvert {
			continue
		}
		cf, inType, boolForm, err := classifyConverter(v.FuncName, fieldType, qualifier, file, pkg)
		if err != nil {
			errs = append(errs, &richError{Msg: err.Error(), CodeSpan: "@" + v.FuncName})
			continue
		}
		if v.Msg != "" && !boolForm {
			errs = append(errs, &richError{Msg: fmt.Sprintf("inline message on @%s requires a bool-form converter (func(W) (T, bool))", v.FuncName), CodeSpan: "@" + v.FuncName})
			continue
		}
		v.Custom = true
		v.PkgImport = cf.PkgImport
		v.PkgName = cf.PkgName
		v.FuncName = cf.FuncName
		v.Fallible = cf.Fallible
		v.BoolForm = boolForm
		v.InType = inType
		v.InKind = resolveKind(inType)
	}

	resolve := func(steps []Step, wt types.Type) {
		for i := range steps {
			st := &steps[i]
			name := st.V.Name
			if st.IsMod || !strings.HasPrefix(name, "@") {
				continue
			}
			msg := st.V.Msg
			role, cf, boolForm, err := classifyValueFunc(name[1:], wt, file, pkg)
			if err != nil {
				errs = append(errs, &richError{Msg: err.Error(), CodeSpan: name})
				continue
			}
			if msg != "" && !boolForm {
				errs = append(errs, &richError{
					Msg:      fmt.Sprintf("inline message on %s requires a bool-form func (func(_) bool / func(_) (_, bool))", name),
					CodeSpan: name,
				})
				continue
			}
			switch role {
			case roleValidator:
				st.IsMod = false
				st.V = ValidationRule{Name: name, Custom: true, PkgImport: cf.PkgImport, PkgName: cf.PkgName, FuncName: cf.FuncName, BoolForm: boolForm, Msg: msg}
			case roleMod:
				st.IsMod = true
				st.M = ModRule{Name: name, Custom: true, PkgImport: cf.PkgImport, PkgName: cf.PkgName, FuncName: cf.FuncName, Fallible: cf.Fallible, BoolForm: boolForm, Msg: msg}
				st.V = ValidationRule{}
			}
		}
	}

	resolve(fi.Pipe, fieldType)
	resolve(fi.KeyPipe, types.Typ[types.String])
	wt := fieldType
	for i := range fi.Levels {
		elem, err := diveElemType(wt)
		if err != nil {
			errs = append(errs, fmt.Errorf("dive level %d: %w", i+1, err))
			break
		}
		resolve(fi.Levels[i], elem)
		wt = elem
	}
	if err := checkVariantShapes(*fi); err != nil {
		errs = append(errs, err)
	}
	deriveBuckets(fi)
	return errors.Join(errs...)
}

// partitionCustomValidation splits a rule list into built-in vs `@Func` rules.
// Pointer-field codegen runs built-in rules on the deref'd value but `@Func`
// rules on the exact `*T` field type.
func partitionCustomValidation(rules []ValidationRule) (builtin, custom []ValidationRule) {
	for _, r := range rules {
		if r.Custom {
			custom = append(custom, r)
		} else {
			builtin = append(builtin, r)
		}
	}
	return
}

// partitionCustomMods is the mod-rule counterpart to partitionCustomValidation.
func partitionCustomMods(rules []ModRule) (builtin, custom []ModRule) {
	for _, r := range rules {
		if r.Custom {
			custom = append(custom, r)
		} else {
			builtin = append(builtin, r)
		}
	}
	return
}

// diveElemType peels one container layer off t. `inner:` rules apply to the
// elements of slices/arrays/maps; this helper computes the type each level
// will see at codegen time.
func diveElemType(t types.Type) (types.Type, error) {
	switch tt := t.Underlying().(type) {
	case *types.Slice:
		return tt.Elem(), nil
	case *types.Array:
		return tt.Elem(), nil
	case *types.Map:
		return tt.Elem(), nil
	case *types.Pointer:
		// `*[]T`-style pointer-to-container: peel both the pointer and the
		// container. Rare in practice, but supported.
		return diveElemType(tt.Elem())
	}
	return nil, fmt.Errorf("type %s has no element to dive into", t)
}
