package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/types"
	"strconv"
	"strings"
)

// customFunc holds the resolution for a `@Func` or `@pkg.Func` reference
// found in a `ggen:"..."` or `mod:"..."` tag. Result is stamped onto the
// Validation/ModRule so codegen emits a direct call without consulting the
// runtime registry (which has been removed entirely).
type customFunc struct {
	PkgImport string // import path; "" for same-package
	PkgName   string // canonical name to qualify the call in generated code; "" for same-package
	FuncName  string // bare function name (no "@", no "pkg.")
	Fallible  bool   // mods only: true when signature is `func(T) (T, error)`
}

// resolveCustomFunc looks up `ref` (the part after `@`) and validates the
// signature against `fieldType`. Returns a populated customFunc or an error
// suitable for surfacing back to the user.
//
// Supports:
//   - same-package: `Func` → looked up in pkg.Scope().
//   - cross-package via file-scoped alias: `alias.Func` → file.Imports walked
//     for an import whose alias matches `alias`.
//   - cross-package via canonical package name: `pkgname.Func` → walks
//     pkg.Imports() looking for a *types.Package whose Name() matches.
//     Lets users blank-import (`_ "path/to/lib"`) and reference by the
//     library's natural package name without aliasing.
//
// `isMod` toggles signature flavor: validators must be `func(T) error`;
// mods accept either `func(T) T` (pure) or `func(T) (T, error)` (fallible).
// `errType` is the well-known `error` interface looked up once at parse pass
// (typesInfo carries it via the universe scope).
func resolveCustomFunc(ref string, fieldType types.Type, file *ast.File, pkg *types.Package, isMod bool) (customFunc, error) {
	if ref == "" {
		return customFunc{}, fmt.Errorf("empty @ reference")
	}
	pkgPart, funcPart, hasDot := strings.Cut(ref, ".")
	if !hasDot {
		funcPart = pkgPart
		pkgPart = ""
	}

	var (
		fn      *types.Func
		pkgImp  string
		pkgName string
	)
	if pkgPart == "" {
		// Same package.
		if pkg == nil {
			return customFunc{}, fmt.Errorf("no package context (run ggen with a Go module so type info is available)")
		}
		obj := pkg.Scope().Lookup(funcPart)
		if obj == nil {
			return customFunc{}, fmt.Errorf("func %s not found in package %s", funcPart, pkg.Name())
		}
		f, ok := obj.(*types.Func)
		if !ok {
			return customFunc{}, fmt.Errorf("%s is not a function (got %T)", funcPart, obj)
		}
		fn = f
	} else {
		target, importPath, err := lookupCrossPkg(pkgPart, file, pkg)
		if err != nil {
			return customFunc{}, fmt.Errorf("%w", err)
		}
		obj := target.Scope().Lookup(funcPart)
		if obj == nil {
			return customFunc{}, fmt.Errorf("func %s not found in package %s", funcPart, target.Path())
		}
		f, ok := obj.(*types.Func)
		if !ok {
			return customFunc{}, fmt.Errorf("%s is not a function (got %T)", funcPart, obj)
		}
		fn = f
		pkgImp = importPath
		pkgName = target.Name()
	}

	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		return customFunc{}, fmt.Errorf("not a function signature")
	}
	if sig.Recv() != nil {
		return customFunc{}, fmt.Errorf("must be a top-level function, not a method")
	}
	if sig.Params().Len() != 1 {
		return customFunc{}, fmt.Errorf("must take exactly one parameter (the field value)")
	}
	paramType := sig.Params().At(0).Type()
	// Accept generic params (`func[T constraint](T) error`) when the
	// field type satisfies the type-param's constraint. The generated
	// code calls Func(field) and Go inferences T = fieldType at the
	// call site, so as long as fieldType is in the constraint's type
	// set the call type-checks.
	paramTypeParam, paramIsGeneric := paramType.(*types.TypeParam)
	if paramIsGeneric {
		if !satisfiesConstraint(fieldType, paramTypeParam) {
			return customFunc{}, fmt.Errorf("field type %s does not satisfy constraint %s on param T", fieldType, paramTypeParam.Constraint())
		}
	} else if !types.Identical(paramType, fieldType) {
		return customFunc{}, fmt.Errorf("param type %s does not match field type %s", paramType, fieldType)
	}

	// resultMatchesField reports whether r either equals fieldType
	// directly OR is the same type parameter as the input (so the
	// instantiation aligns: `func[T](T) T` returns fieldType).
	resultMatchesField := func(r types.Type) bool {
		if types.Identical(r, fieldType) {
			return true
		}
		if rtp, ok := r.(*types.TypeParam); ok && paramIsGeneric {
			return rtp == paramTypeParam
		}
		return false
	}

	results := sig.Results()
	out := customFunc{
		PkgImport: pkgImp,
		PkgName:   pkgName,
		FuncName:  funcPart,
	}
	if isMod {
		switch results.Len() {
		case 1:
			if !resultMatchesField(results.At(0).Type()) {
				return customFunc{}, fmt.Errorf("mod result type %s does not match field type %s", results.At(0).Type(), fieldType)
			}
		case 2:
			if !resultMatchesField(results.At(0).Type()) {
				return customFunc{}, fmt.Errorf("fallible mod first result %s does not match field type %s", results.At(0).Type(), fieldType)
			}
			if !isErrorType(results.At(1).Type()) {
				return customFunc{}, fmt.Errorf("fallible mod second result must be error, got %s", results.At(1).Type())
			}
			out.Fallible = true
		default:
			return customFunc{}, fmt.Errorf("mod must return T or (T, error), got %d results", results.Len())
		}
	} else {
		if results.Len() != 1 {
			return customFunc{}, fmt.Errorf("validator must return error, got %d results", results.Len())
		}
		if !isErrorType(results.At(0).Type()) {
			return customFunc{}, fmt.Errorf("validator must return error, got %s", results.At(0).Type())
		}
	}
	return out, nil
}

// satisfiesConstraint reports whether t is permitted as an
// instantiation of the type parameter tp. Uses types.Satisfies which
// performs the Go-spec type-set membership check (1.21+); for older
// Go versions or constraints expressed as plain method-set
// interfaces, falls back to types.Implements against the underlying
// interface.
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
		// Pass 1: file-scoped aliases. `import alias "path"` lets the user
		// refer to the package by `alias` regardless of its declared Name().
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
				// Blank import: no file-scoped name introduced. Match on
				// the package's declared name so users can pull a lib
				// purely for ggen's benefit.
				if target.Name() == pkgPart {
					return target, path, nil
				}
			}
		}
	}
	// Pass 2: any transitive import whose declared package name matches.
	// Picks up cases where the file-scoped lookup missed (e.g., the import
	// is in a different file of the same package).
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

// resolveCustomRules walks every validation/mod rule on fi and resolves
// the `@`-prefixed references. Mutates fi in place. Errors out if any
// `@`-rule is present without type info available — the generator can't
// validate signatures (or even resolve cross-package refs) in AST-only
// mode, so we'd be flying blind.
func (s *structSet) resolveCustomRules(structName string, fi *FieldInfo, fieldType types.Type) error {
	hasCustom := anyCustom(fi)
	if !hasCustom {
		return nil
	}
	if s.typesInfo == nil || fieldType == nil {
		return fmt.Errorf("`@Func` references require Go module context (run ggen inside a Go module so packages.Load can resolve types)")
	}
	file := s.structFile[structName]
	pkg := s.typesPkg

	// Accumulate across every @-ref location — a field with both an
	// unresolvable val ref AND an unresolvable mod ref (or many @-refs
	// across dive levels) needs all of them surfaced at once. Returning
	// on the first error means the user fixes one, re-runs, finds
	// another, fixes that, re-runs… painful UX.
	var errs []error
	collect := func(err error) { errs = append(errs, err) }

	// Outer rules see the field's type as-is (incl. *T for pointer fields).
	collect(resolveValidationRules(fi.Validation, fieldType, file, pkg))
	collect(resolveModRules(fi.Mods, fieldType, file, pkg))

	// Map keys are always string.
	keyT := types.Typ[types.String]
	collect(resolveValidationRules(fi.KeyValidation, keyT, file, pkg))
	collect(resolveModRules(fi.KeyMods, keyT, file, pkg))

	// Dive levels: peel one container per level.
	if len(fi.ElemValidation) > 0 || len(fi.ElemMods) > 0 || len(fi.InnerValidation) > 0 || len(fi.InnerMods) > 0 {
		elem, err := diveElemType(fieldType)
		if err != nil {
			collect(fmt.Errorf("dive: %w", err))
		} else {
			collect(resolveValidationRules(fi.ElemValidation, elem, file, pkg))
			collect(resolveModRules(fi.ElemMods, elem, file, pkg))
			inner := elem
			for i := range fi.InnerValidation {
				next, derr := diveElemType(inner)
				if derr != nil {
					collect(fmt.Errorf("dive level %d: %w", i+2, derr))
					break
				}
				inner = next
				collect(resolveValidationRules(fi.InnerValidation[i], inner, file, pkg))
				if i < len(fi.InnerMods) {
					collect(resolveModRules(fi.InnerMods[i], inner, file, pkg))
				}
			}
		}
	}
	return errors.Join(errs...)
}

func resolveValidationRules(rules []ValidationRule, fieldType types.Type, file *ast.File, pkg *types.Package) error {
	var errs []error
	for i := range rules {
		if !strings.HasPrefix(rules[i].Name, "@") {
			continue
		}
		ref := rules[i].Name[1:]
		cf, err := resolveCustomFunc(ref, fieldType, file, pkg, false)
		if err != nil {
			// Wrap into a richError with CodeSpan = the original
			// `@ref` token, so the pretty renderer's caret lands on
			// the offending tag text, not on the field declaration.
			errs = append(errs, &richError{Msg: err.Error(), CodeSpan: "@" + ref})
			continue
		}
		rules[i].Custom = true
		rules[i].PkgImport = cf.PkgImport
		rules[i].PkgName = cf.PkgName
		rules[i].FuncName = cf.FuncName
	}
	return errors.Join(errs...)
}

func resolveModRules(rules []ModRule, fieldType types.Type, file *ast.File, pkg *types.Package) error {
	var errs []error
	for i := range rules {
		if !strings.HasPrefix(rules[i].Name, "@") {
			continue
		}
		ref := rules[i].Name[1:]
		cf, err := resolveCustomFunc(ref, fieldType, file, pkg, true)
		if err != nil {
			errs = append(errs, &richError{Msg: err.Error(), CodeSpan: "@" + ref})
			continue
		}
		rules[i].Custom = true
		rules[i].PkgImport = cf.PkgImport
		rules[i].PkgName = cf.PkgName
		rules[i].FuncName = cf.FuncName
		rules[i].Fallible = cf.Fallible
	}
	return errors.Join(errs...)
}

// anyCustom reports whether any rule on fi is `@`-prefixed.
func anyCustom(fi *FieldInfo) bool {
	for _, r := range fi.Validation {
		if strings.HasPrefix(r.Name, "@") {
			return true
		}
	}
	for _, r := range fi.ElemValidation {
		if strings.HasPrefix(r.Name, "@") {
			return true
		}
	}
	for _, r := range fi.KeyValidation {
		if strings.HasPrefix(r.Name, "@") {
			return true
		}
	}
	for _, group := range fi.InnerValidation {
		for _, r := range group {
			if strings.HasPrefix(r.Name, "@") {
				return true
			}
		}
	}
	for _, r := range fi.Mods {
		if strings.HasPrefix(r.Name, "@") {
			return true
		}
	}
	for _, r := range fi.ElemMods {
		if strings.HasPrefix(r.Name, "@") {
			return true
		}
	}
	for _, r := range fi.KeyMods {
		if strings.HasPrefix(r.Name, "@") {
			return true
		}
	}
	for _, group := range fi.InnerMods {
		for _, r := range group {
			if strings.HasPrefix(r.Name, "@") {
				return true
			}
		}
	}
	return false
}

// partitionCustomValidation splits a rule list into built-in vs `@Func`
// rules. Used by the pointer-field codegen path: built-in rules continue
// to operate on the deref'd value (so `gte=0` keeps comparing the int),
// while `@Func` rules apply to the field's exact `*T` type per the
// "exact field type" spec for custom funcs.
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

// diveElemType peels one container layer off t. `dive:` rules apply to the
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
