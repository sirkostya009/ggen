package model

import (
	"errors"
	"fmt"
	"go/ast"
	"go/types"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// customFunc is the resolved form of a `@Func` or `@pkg.Func` reference. It is
// copied onto the ValidationRule, ModRule or Variant so codegen can emit a
// direct call.
type customFunc struct {
	PkgImport string // import path, "" for the same package
	PkgName   string // package name that qualifies the call in generated code, "" for the same package
	FuncName  string // bare function name, without "@" or "pkg."
	Fallible  bool   // mods and converters: the function returns a second error or bool result
}

// lookupFunc resolves a `@Func` or `@pkg.Func` reference, given without the
// `@`. It returns the *types.Func and the import path and package name the
// generated call must be qualified with. classifyValueFunc and
// classifyConverter both use it.
func lookupFunc(ref string, file *ast.File, pkg *types.Package) (fn *types.Func, pkgImp, pkgName string, err error) {
	if ref == "" {
		return nil, "", "", errors.New("empty @ reference")
	}
	pkgPart, funcPart, hasDot := strings.Cut(ref, ".")
	if !hasDot {
		funcPart = pkgPart
		pkgPart = ""
	}
	if pkgPart == "" {
		if pkg == nil {
			return nil, "", "", errors.New("no package context (run ggen with a Go module so type info is available)")
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

// pipeRole is what a value-stage `@Func` turns out to be. A validator checks
// the value. A mod returns a new value of the same type, and may also fail. A
// converter returns a different type. Converters are only allowed in the
// decode stage, so a value step rejects them.
type pipeRole uint8

const (
	roleValidator pipeRole = iota
	roleMod
	roleConverter
)

// classifyValueFunc resolves a value-stage `@Func` against wt, the type of
// the value at that point, and classifies it by its signature. A single bool
// or error result always means a validator:
//
//	func(T) error         validator
//	func(T) bool          validator, can take an inline message. func(bool) bool is not allowed
//	func(T) T             mod that cannot fail
//	func(T) (T, error)    mod that can fail with an error
//	func(T) (T, bool)     mod that can fail, can take an inline message
//	func(T) U, U != T     converter, not allowed in a value stage
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
		return 0, customFunc{}, false, errors.New("not a function signature")
	}
	if sig.Recv() != nil {
		return 0, customFunc{}, false, errors.New("must be a top-level function, not a method")
	}
	if sig.Params().Len() != 1 {
		return 0, customFunc{}, false, errors.New("must take exactly one parameter (the value)")
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
			// func(bool) bool is ambiguous, so it is not allowed. Use
			// func(bool) error instead.
			if wtIsBool {
				return 0, customFunc{}, false, errors.New("func(bool) bool is banned — use func(bool) error to validate a bool field")
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

// classifyConverter resolves a decode-stage converter variant `@Conv`. A
// value func is checked against its input, but a converter is checked
// against its output: the first result must be the field type T. The single
// parameter W is the type ggen decodes from JSON, and its JSON shape decides
// which input this variant handles. It returns W. W must be a builtin or
// same-package type, optionally behind pointers. A `*W` input handles W's
// shape plus null. Types from other packages are not supported.
func classifyConverter(ref string, fieldType types.Type, file *ast.File, pkg *types.Package) (cf customFunc, w types.Type, boolForm bool, err error) {
	fn, pkgImp, pkgName, err := lookupFunc(ref, file, pkg)
	if err != nil {
		return customFunc{}, nil, false, err
	}
	_, funcPart, hasDot := strings.Cut(ref, ".")
	if !hasDot {
		funcPart = ref
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		return customFunc{}, nil, false, errors.New("not a function signature")
	}
	if sig.Recv() != nil {
		return customFunc{}, nil, false, errors.New("must be a top-level function, not a method")
	}
	if sig.Params().Len() != 1 {
		return customFunc{}, nil, false, errors.New("converter must take exactly one parameter (the scanned input)")
	}
	w = sig.Params().At(0).Type()
	// Remove pointer levels first, so `*[]int` or `*other.T` cannot slip past
	// the checks below. A container behind a pointer still emits broken code.
	base := w
	for {
		p, isPtr := base.(*types.Pointer)
		if !isPtr {
			break
		}
		base = p.Elem()
	}
	if named, isNamed := base.(*types.Named); isNamed && named.Obj().Pkg() != nil && named.Obj().Pkg() != pkg {
		return customFunc{}, nil, false, fmt.Errorf("converter input %s is from another package; only builtin or same-package input types are supported", w)
	}
	// The input must be a primitive or a struct ggen can decode. A container
	// input would get a FieldInfo with no element shape, and the emitted code
	// would not compile or would decode wrongly. Channel, func and interface
	// inputs have no JSON shape at all.
	switch base.Underlying().(type) {
	case *types.Slice, *types.Array, *types.Map:
		return customFunc{}, nil, false, fmt.Errorf("converter input %s must be a primitive or a ggen-decodable struct — container inputs are not supported", w)
	case *types.Chan, *types.Signature, *types.Interface:
		return customFunc{}, nil, false, fmt.Errorf("converter input %s must be a primitive or a ggen-decodable struct", w)
	}
	cf = customFunc{PkgImport: pkgImp, PkgName: pkgName, FuncName: funcPart}
	res := sig.Results()
	switch res.Len() {
	case 1:
		if !types.Identical(res.At(0).Type(), fieldType) {
			return customFunc{}, nil, false, fmt.Errorf("converter output %s must equal field type %s", res.At(0).Type(), fieldType)
		}
	case 2:
		if !types.Identical(res.At(0).Type(), fieldType) {
			return customFunc{}, nil, false, fmt.Errorf("converter first result %s must equal field type %s", res.At(0).Type(), fieldType)
		}
		cf.Fallible = true
		switch {
		case isErrorType(res.At(1).Type()):
		case isBoolType(res.At(1).Type()):
			boolForm = true
		default:
			return customFunc{}, nil, false, fmt.Errorf("converter second result must be error or bool, got %s", res.At(1).Type())
		}
	default:
		return customFunc{}, nil, false, fmt.Errorf("converter must return T or (T, error) or (T, bool), got %d results", res.Len())
	}
	return cf, w, boolForm, nil
}

// isBoolType reports whether t is the predeclared bool.
func isBoolType(t types.Type) bool {
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Kind() == types.Bool
}

// satisfiesConstraint reports whether t can be used for the type parameter
// tp, meaning t is in the type set of tp's constraint.
func satisfiesConstraint(t types.Type, tp *types.TypeParam) bool {
	constraint := tp.Constraint()
	if iface, ok := constraint.Underlying().(*types.Interface); ok {
		return types.Satisfies(t, iface)
	}
	return false
}

// lookupCrossPkg resolves the import alias or package name written before
// the `.` in a `@pkg.Func` reference. It returns the package and its import
// path, so the generator can import it in the generated file.
func lookupCrossPkg(pkgPart string, file *ast.File, pkg *types.Package) (*types.Package, string, error) {
	if file != nil {
		// First pass: the imports of this file, including aliases such as
		// `import alias "path"`.
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
				// A blank import gives no name in the file. Match the package's
				// declared name.
				if target.Name() == pkgPart {
					return target, path, nil
				}
			}
		}
	}
	// Second pass: any import of the package whose declared name matches.
	// This finds an import that lives in another file of the same package.
	for _, imp := range pkg.Imports() {
		if imp.Name() == pkgPart {
			return imp, imp.Path(), nil
		}
	}
	return nil, "", fmt.Errorf("no import alias or package named %s in scope", pkgPart)
}

// findImportedPackage returns the package with the given import path from
// the imports of pkg.
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

// isErrorType reports whether t is the predeclared error interface.
func isErrorType(t types.Type) bool {
	if named, ok := t.(*types.Named); ok {
		return named.Obj().Name() == "error" && named.Obj().Pkg() == nil
	}
	return false
}

// pipeHasCustom reports whether fi's pipe has any `@` step left to classify.
// Until classifyValueFunc runs, such a step is stored as a validator step
// whose Name starts with "@".
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
	return slices.ContainsFunc(fi.Levels, has)
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

// resolvePipeCustoms resolves and classifies every `@` step of a field
// against the type of the value at that step's level. The signature decides
// whether a step is a mod or a validator. It then rebuilds the separate rule
// and mod lists from the pipe.
func (s *structSet) resolvePipeCustoms(structName string, fi *FieldInfo, fieldType types.Type) error {
	if !pipeHasCustom(fi) && !pipeHasConverter(fi) {
		deriveBuckets(fi)
		return nil
	}
	if s.typesInfo == nil || fieldType == nil {
		return errors.New("`@Func` references require Go module context (run ggen inside a Go module so packages.Load can resolve types)")
	}
	file := s.structFile[structName]
	pkg := s.typesPkg
	qualifier := s.pkgQualifier()
	var errs []error

	// Decode-stage converters: check that the output is T and record the
	// input W. The emitters decode W with the same code they use for fields,
	// so W is described the way a field of that type would be. Pointers are
	// removed, Kind comes from the base type, and a named primitive is added
	// to NamedPrims so the shape dispatch and the inline decode see its
	// underlying kind.
	for i := range fi.Variants {
		v := &fi.Variants[i]
		if v.Kind != VariantConvert {
			continue
		}
		cf, w, boolForm, err := classifyConverter(v.FuncName, fieldType, file, pkg)
		if err != nil {
			errs = append(errs, &RichError{Msg: err.Error(), CodeSpan: "@" + v.FuncName})
			continue
		}
		if v.Msg != "" && !boolForm {
			errs = append(
				errs,
				&RichError{
					Msg:      fmt.Sprintf("inline message on @%s requires a bool-form converter (func(W) (T, bool))", v.FuncName),
					CodeSpan: "@" + v.FuncName,
				},
			)
			continue
		}
		v.Custom = true
		v.PkgImport = cf.PkgImport
		v.PkgName = cf.PkgName
		v.FuncName = cf.FuncName
		v.Fallible = cf.Fallible
		v.BoolForm = boolForm
		v.InType = types.TypeString(w, qualifier)
		v.In = w
		base := w
		for {
			p, isPtr := base.(*types.Pointer)
			if !isPtr {
				break
			}
			v.InPointer = true
			base = p.Elem()
		}
		v.InKind = ResolveKind(types.TypeString(base, qualifier))
		if np := s.namedPrims(w); len(np) > 0 {
			if fi.NamedPrims == nil {
				fi.NamedPrims = map[string]TypeKind{}
			}
			maps.Copy(fi.NamedPrims, np)
		}
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
				errs = append(errs, &RichError{Msg: err.Error(), CodeSpan: name})
				continue
			}
			if msg != "" && !boolForm {
				errs = append(errs, &RichError{
					Msg:      fmt.Sprintf("inline message on %s requires a bool-form func (func(_) bool / func(_) (_, bool))", name),
					CodeSpan: name,
				})
				continue
			}
			//exhaustive:ignore not every kind applies here
			switch role {
			case roleValidator:
				st.IsMod = false
				st.V = ValidationRule{
					Name:      name,
					Custom:    true,
					PkgImport: cf.PkgImport,
					PkgName:   cf.PkgName,
					FuncName:  cf.FuncName,
					BoolForm:  boolForm,
					Msg:       msg,
				}
			case roleMod:
				st.IsMod = true
				st.M = ModRule{
					Name:      name,
					Custom:    true,
					PkgImport: cf.PkgImport,
					PkgName:   cf.PkgName,
					FuncName:  cf.FuncName,
					Fallible:  cf.Fallible,
					BoolForm:  boolForm,
					Msg:       msg,
				}
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

// PartitionCustomValidation splits a rule list into built-in and `@Func`
// rules. For a pointer field, codegen runs built-in rules on the value the
// pointer points to, but `@Func` rules on the `*T` field itself.
func PartitionCustomValidation(rules []ValidationRule) (builtin, custom []ValidationRule) {
	for _, r := range rules {
		if r.Custom {
			custom = append(custom, r)
		} else {
			builtin = append(builtin, r)
		}
	}
	return
}

// PartitionCustomMods is PartitionCustomValidation for mods.
func PartitionCustomMods(rules []ModRule) (builtin, custom []ModRule) {
	for _, r := range rules {
		if r.Custom {
			custom = append(custom, r)
		} else {
			builtin = append(builtin, r)
		}
	}
	return
}

// diveElemType removes one container layer from t. Nested pipe levels apply
// to the elements of slices, arrays and maps. This returns the type each
// level works on.
func diveElemType(t types.Type) (types.Type, error) {
	switch tt := t.Underlying().(type) {
	case *types.Slice:
		return tt.Elem(), nil
	case *types.Array:
		return tt.Elem(), nil
	case *types.Map:
		return tt.Elem(), nil
	case *types.Pointer:
		// A pointer to a container, such as `*[]T`. Remove both the pointer and
		// the container. Rare, but supported.
		return diveElemType(tt.Elem())
	}
	return nil, fmt.Errorf("type %s has no element to dive into", t)
}

// FieldHasConverter reports whether f needs a decode that picks a variant by
// the JSON shape. Fields with only the native and nullzero variants use the
// normal decode path plus the NullZero flag.
func FieldHasConverter(f FieldInfo) bool {
	for _, v := range f.Variants {
		if v.Kind == VariantConvert {
			return true
		}
	}
	return false
}

// kindShapeBytes returns the first JSON bytes a kind's normal shape can start
// with, as Go rune literals for case labels. An empty result means the kind
// has no single shape, as with any or raw JSON, and cannot take part in
// shape dispatch.
func kindShapeBytes(k TypeKind, format string) []string {
	//exhaustive:ignore not every kind applies here
	switch k {
	case KindString, KindTime, KindDuration, KindNetIP, KindNetipAddr,
		KindNetipPrefix, KindURL, KindBigFloat, KindBigRat:
		return []string{"'\"'"}
	case KindBytes:
		if format == "array" {
			return []string{"'['"}
		}
		return []string{"'\"'"}
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64,
		KindUint, KindUint8, KindUint16, KindUint32, KindUint64,
		KindFloat32, KindFloat64, KindBigInt:
		return []string{"'-'", "'0'", "'1'", "'2'", "'3'", "'4'", "'5'", "'6'", "'7'", "'8'", "'9'"}
	case KindBool:
		return []string{"'t'", "'f'"}
	case KindStruct, KindMap:
		return []string{"'{'"}
	case KindSlice, KindArray:
		return []string{"'['"}
	}
	return nil
}

// KindResolver maps a named type to the kind of its underlying primitive. It
// returns kind unchanged when goType is not a named primitive.
type KindResolver func(goType string, kind TypeKind) TypeKind

// variantShapeKind returns the kind whose JSON shape the type really has. A
// named primitive such as `type Score int` reads as KindStruct where it is
// used. Without this lookup it would claim the object shape `{` for a value
// that decodes as a number. The native variant, or a converter whose input W
// is a named primitive, could then never match. FieldInfo.NamedPrims is the
// source at parse time, and resolvePipeCustoms adds a converter's W there
// too. resolve, when not nil, is the fallback at render time for synthetic
// fields that have no NamedPrims. A pointer type resolves through its
// pointee.
func variantShapeKind(f FieldInfo, goType string, kind TypeKind, resolve KindResolver) TypeKind {
	goType = strings.TrimLeft(goType, "*")
	if k, ok := f.NamedPrims[goType]; ok {
		return k
	}
	if resolve != nil {
		return resolve(goType, kind)
	}
	return kind
}

// VariantCaseBytes returns the case labels a single variant claims.
func VariantCaseBytes(f FieldInfo, v Variant, resolve KindResolver) []string {
	switch v.Kind {
	case VariantNullZero:
		return []string{"'n'"}
	case VariantNative:
		bs := kindShapeBytes(variantShapeKind(f, f.GoType, f.Kind, resolve), f.Format)
		// Native kinds that accept null keep their null case. Without it, adding
		// a converter would make {"p":null} an error, while the plain field
		// decodes null to nil. An explicit nullzero variant claims 'n' itself.
		if nativeAcceptsNull(f, resolve) && !hasNullZeroVariant(f) {
			bs = append(bs, "'n'")
		}
		return bs
	case VariantConvert:
		bs := kindShapeBytes(variantShapeKind(f, v.InType, v.InKind, resolve), "")
		if v.InPointer {
			bs = append(bs, "'n'")
		}
		return bs
	}
	return nil
}

// nativeAcceptsNull reports whether f's native decode accepts null. That is
// true for pointers, slices, maps, []byte, net.IP and raw JSON.
func nativeAcceptsNull(f FieldInfo, resolve KindResolver) bool {
	if f.Pointer {
		return true
	}
	//exhaustive:ignore not every kind applies here
	switch variantShapeKind(f, f.GoType, f.Kind, resolve) {
	case KindSlice, KindMap, KindBytes, KindNetIP, KindRawJSON:
		return true
	}
	return false
}

func hasNullZeroVariant(f FieldInfo) bool {
	for _, v := range f.Variants {
		if v.Kind == VariantNullZero {
			return true
		}
	}
	return false
}

// checkVariantShapes checks that no two decode variants on f claim the same
// JSON shape, and that every variant has at least one first byte to match
// on. It returns a *RichError on a conflict.
func checkVariantShapes(f FieldInfo) error {
	if !FieldHasConverter(f) {
		return nil
	}
	seen := map[string]string{} // case byte to variant label
	label := func(v Variant) string {
		switch v.Kind {
		case VariantNullZero:
			return "nullzero"
		case VariantNative:
			return "native (" + f.GoType + ")"
		default:
			return "@" + v.FuncName
		}
	}
	for _, v := range f.Variants {
		bs := VariantCaseBytes(f, v, nil)
		if len(bs) == 0 {
			return &RichError{
				Msg:      fmt.Sprintf("%s.%s: decode variant %s has no single JSON shape to dispatch on", f.StructName, f.GoName, label(v)),
				CodeSpan: "@" + v.FuncName,
			}
		}
		for _, c := range bs {
			if prev, dup := seen[c]; dup {
				return &RichError{
					Msg:      fmt.Sprintf("%s.%s: decode variants %s and %s both claim the same JSON shape", f.StructName, f.GoName, prev, label(v)),
					CodeSpan: "@" + v.FuncName,
				}
			}
			seen[c] = label(v)
		}
	}
	return nil
}
