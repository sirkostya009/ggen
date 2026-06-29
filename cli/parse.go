package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

// fileBuildConstraint returns the canonical //go:build expression for f (or "")
// normalized via constraint.Parse so equivalent forms collapse to one bucket
// key. Old-style `// +build` is honored; only comments before the package
// clause are inspected (Go's own rule).
func fileBuildConstraint(f *ast.File) string {
	if f == nil {
		return ""
	}
	var plus []constraint.Expr
	for _, cg := range f.Comments {
		// Build constraints must precede the package clause.
		if cg.End() >= f.Package {
			break
		}
		for _, c := range cg.List {
			text := c.Text
			if constraint.IsGoBuild(text) {
				if expr, err := constraint.Parse(text); err == nil {
					return expr.String()
				}
			} else if constraint.IsPlusBuild(text) {
				if expr, err := constraint.Parse(text); err == nil {
					plus = append(plus, expr)
				}
			}
		}
	}
	if len(plus) == 0 {
		return ""
	}
	// `// +build` lines AND together (intra-line OR already encoded by Parse).
	combined := plus[0]
	for _, e := range plus[1:] {
		combined = &constraint.AndExpr{X: combined, Y: e}
	}
	return combined.String()
}

type annotationFlags struct {
	marshal       bool
	unmarshal     bool
	multierr      bool
	allowdups     bool
	novalidate    bool
	ignoreunknown bool // opt in: silently skip unknown JSON keys (default: error)
	nullzero      bool // opt in: accept explicit JSON null on every non-pointer value field (null → Go zero)
	nosortkeys    bool // opt out: emit fields in declaration order instead of JSON-name sorted
	usenumber     bool // opt in: decode JSON numbers into `any` fields as json.Number instead of float64
	htmlescape    bool // opt in: HTML-safe escape <, >, & in emitted strings (default: literal, matches jsonv2)
	copy          bool // opt in: bytes-path DecodeFrom copies strings/RawMessage/any instead of aliasing data
}

type structSet struct {
	structs     map[string]*ast.StructType
	aliases     map[string]*ast.TypeSpec // top-level non-struct annotated types (alias of a primitive)
	order       []string
	annotations map[string]annotationFlags
	// fromTest is the set of struct names from *_test.go files — routes their
	// methods into *_ggen_test.go. Absence means non-test.
	fromTest map[string]struct{}
	pkgName  string

	// fileSet / typesInfo / typesPkg are populated by the packages-aware
	// loader; the generator uses them to detect interface impls on field types.
	fileSet   *token.FileSet
	typesInfo *types.Info
	typesPkg  *types.Package
	stdIfaces stdInterfaces
	// fieldExpr maps "<StructName>.<FieldName>" → its AST type expression, for
	// resolving the field's go/types.Type.
	fieldExpr map[string]ast.Expr
	// structFile maps struct name → its declaring *ast.File, for resolving
	// @pkg.Func references against the file's (file-scoped) imports.
	structFile map[string]*ast.File
	// structBuildTag maps struct name → its file's canonical //go:build
	// expression ("" for unconstrained), used for output bucketing.
	structBuildTag map[string]string
}

// loadStructs is the AST-only loader (temp files with no module context). No
// type info — FieldInterfaces flags stay zero, so the generator uses its
// runtime-probe cascade in cross-package paths.
func loadStructs(filenames []string) (*structSet, error) {
	set := &structSet{
		structs:        map[string]*ast.StructType{},
		aliases:        map[string]*ast.TypeSpec{},
		annotations:    map[string]annotationFlags{},
		fromTest:       map[string]struct{}{},
		fieldExpr:      map[string]ast.Expr{},
		structFile:     map[string]*ast.File{},
		structBuildTag: map[string]string{},
	}
	fset := token.NewFileSet()
	for _, filename := range filenames {
		af, err := parser.ParseFile(fset, filename, nil, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", filename, err)
		}
		if set.pkgName == "" {
			set.pkgName = af.Name.Name
		}
		isTest := strings.HasSuffix(filename, "_test.go")
		walkStructDecls(af, isTest, set)
	}
	set.fileSet = fset
	return set, nil
}

// loadDirWithTypes loads a package via packages.Load with full type info and
// walks its syntax for annotated structs. The structSet carries TypesInfo +
// std-interface refs so extractField can resolve interface flags at parse time.
func loadDirWithTypes(dir string) (*structSet, error) {
	cfg := &packages.Config{
		Dir: dir,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports,
		Tests: true,
	}
	pkgs, err := packages.Load(cfg, ".")
	if err != nil {
		return nil, fmt.Errorf("packages.Load %s: %w", dir, err)
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no packages loaded for %s", dir)
	}
	// With Tests=true, Load returns the base package plus a test variant; pick
	// the one with the largest Syntax slice (it has everything).
	var best *packages.Package
	for _, p := range pkgs {
		if best == nil || len(p.Syntax) > len(best.Syntax) {
			best = p
		}
	}
	if best == nil || best.Types == nil {
		return nil, fmt.Errorf("no usable package loaded for %s", dir)
	}
	set := &structSet{
		structs:        map[string]*ast.StructType{},
		aliases:        map[string]*ast.TypeSpec{},
		annotations:    map[string]annotationFlags{},
		fromTest:       map[string]struct{}{},
		fieldExpr:      map[string]ast.Expr{},
		structFile:     map[string]*ast.File{},
		structBuildTag: map[string]string{},
		pkgName:        best.Name,
		fileSet:        best.Fset,
		typesInfo:      best.TypesInfo,
		typesPkg:       best.Types,
		stdIfaces:      findStdInterfaces(pkgs),
	}
	for _, af := range best.Syntax {
		filename := best.Fset.Position(af.Pos()).Filename
		// Skip our own generated output.
		if strings.HasSuffix(filename, "_ggen.go") || strings.HasSuffix(filename, "_ggen_test.go") {
			continue
		}
		isTest := strings.HasSuffix(filename, "_test.go")
		walkStructDecls(af, isTest, set)
	}
	return set, nil
}

// walkStructDecls registers every top-level struct type in af. Shared by both
// loaders so behavior is identical regardless of how the AST was produced.
func walkStructDecls(af *ast.File, isTest bool, set *structSet) {
	tag := fileBuildConstraint(af)
	for _, decl := range af.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts := spec.(*ast.TypeSpec)
			name := ts.Name.Name
			if _, dup := set.structs[name]; dup {
				continue
			}
			if _, dup := set.aliases[name]; dup {
				continue
			}
			if st, ok := ts.Type.(*ast.StructType); ok {
				set.structs[name] = st
				set.order = append(set.order, name)
				set.structFile[name] = af
				set.structBuildTag[name] = tag
				if isTest {
					set.fromTest[name] = struct{}{}
				}
				if flags, ok := parseAnnotation(gd.Doc, ts.Doc); ok {
					set.annotations[name] = flags
				}
				// Stash field type expressions for later type-info lookup.
				for _, field := range st.Fields.List {
					for _, ident := range field.Names {
						set.fieldExpr[name+"."+ident.Name] = field.Type
					}
				}
				continue
			}
			// Non-struct top-level type. Register every alias (even
			// unannotated) so an explicit name filter can target it; the
			// annotation gate only decides auto-generation without a filter.
			// Unsupported underlyings error lazily in extractAlias.
			flags, annotated := parseAnnotation(gd.Doc, ts.Doc)
			set.aliases[name] = ts
			set.order = append(set.order, name)
			set.structFile[name] = af
			set.structBuildTag[name] = tag
			if isTest {
				set.fromTest[name] = struct{}{}
			}
			if annotated {
				set.annotations[name] = flags
			}
		}
	}
}

// parseAnnotation looks for a "//ggen:generate" directive (optionally followed
// by whitespace-separated flags) and returns the flags + whether it was present.
func parseAnnotation(groups ...*ast.CommentGroup) (annotationFlags, bool) {
	for _, cg := range groups {
		if cg == nil {
			continue
		}
		for _, c := range cg.List {
			text := strings.TrimPrefix(c.Text, "//")
			text = strings.TrimRight(text, " \t")
			if text == "ggen:generate" {
				return annotationFlags{}, true
			}
			rest, ok := strings.CutPrefix(text, "ggen:generate ")
			if !ok {
				continue
			}
			rest = strings.TrimLeft(rest, " \t")
			var flags annotationFlags
			for tok := range strings.FieldsSeq(rest) {
				switch tok {
				case "marshal":
					flags.marshal = true
				case "unmarshal":
					flags.unmarshal = true
				case "multierr":
					flags.multierr = true
				case "allowdups":
					flags.allowdups = true
				case "novalidate":
					flags.novalidate = true
				case "ignoreunknown":
					flags.ignoreunknown = true
				case "nullzero":
					flags.nullzero = true
				case "nosortkeys":
					flags.nosortkeys = true
				case "usenumber":
					flags.usenumber = true
				case "htmlescape":
					flags.htmlescape = true
				case "copy":
					flags.copy = true
				}
			}
			return flags, true
		}
	}
	return annotationFlags{}, false
}

// resolve builds StructInfo for the requested structs plus any referenced
// struct types reachable from them (BFS).
func (s *structSet) resolve(wanted []string) ([]StructInfo, error) {
	return s.resolveFiltered(wanted, nil)
}

// resolveFiltered walks wanted + transitive struct references, expanding only
// deps whose names pass allowExpand (nil = expand everything, package mode;
// single-file mode passes inFile to keep sibling-declared deps out). The roots
// in `wanted` are always emitted.
func (s *structSet) resolveFiltered(wanted []string, allowExpand func(string) bool) ([]StructInfo, error) {
	gen := make(map[string]struct{}, len(wanted))
	queue := slices.Clone(wanted)
	var generated []string
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if _, ok := gen[name]; ok {
			continue
		}
		if st, ok := s.structs[name]; ok {
			gen[name] = struct{}{}
			generated = append(generated, name)
			for _, f := range st.Fields.List {
				if len(f.Names) == 0 {
					continue
				}
				if ref := referencedStructName(f.Type, s.structs); ref != "" {
					if _, seen := gen[ref]; seen {
						continue
					}
					if allowExpand != nil && !allowExpand(ref) {
						continue
					}
					queue = append(queue, ref)
				}
			}
			continue
		}
		if _, ok := s.aliases[name]; ok {
			gen[name] = struct{}{}
			generated = append(generated, name)
			continue
		}
		return nil, fmt.Errorf("type %s not found", name)
	}

	var result []StructInfo
	var errs []error
	for _, name := range generated {
		var info StructInfo
		var err error
		if alias, ok := s.aliases[name]; ok {
			info, err = s.extractAlias(name, alias)
		} else {
			info, err = s.extractStruct(name, s.structs[name])
		}
		if err != nil {
			// Gather rather than bail — one struct's broken tag shouldn't hide
			// the next one's.
			errs = append(errs, err)
			continue
		}
		if flags, ok := s.annotations[name]; ok {
			info.Marshal = flags.marshal
			info.Unmarshal = flags.unmarshal
			info.MultiErr = flags.multierr
			info.AllowDups = flags.allowdups
			info.NoValidate = flags.novalidate
			info.IgnoreUnknown = flags.ignoreunknown
			info.NullZero = flags.nullzero
			info.NoSort = flags.nosortkeys
			info.UseNumber = flags.usenumber
			info.HTMLEscape = flags.htmlescape
			info.Copy = flags.copy
		}
		_, info.Test = s.fromTest[name]
		info.BuildTag = s.structBuildTag[name]
		result = append(result, info)
	}
	if len(errs) > 0 {
		return result, errors.Join(errs...)
	}
	return result, nil
}

// parseFile loads structs from a single file (annotated structs when wanted is
// empty). Tries the packages-aware loader, falling back to AST-only for files
// outside a resolvable module (e.g. test temp files).
//
// The third return (siblings) is every annotated name across the whole package,
// used in single-file mode to seed generatedTypes so a cross-file reference
// routes to a direct DecodeFrom before sibling _ggen files exist. Nil in the
// AST-only degraded mode (siblings unknown).
func parseFile(filename string, wanted []string) ([]StructInfo, string, map[string]struct{}, error) {
	dir := filepath.Dir(filename)
	set, err := loadDirWithTypes(dir)
	degraded := false
	// Degrade to AST-only when Load failed or came back empty (orphan files
	// outside a module). Static interface detection is off in that mode.
	if err != nil || (len(set.structs) == 0 && len(set.aliases) == 0) {
		set, err = loadStructs([]string{filename})
		if err != nil {
			return nil, "", nil, err
		}
		degraded = true
	}
	// loadDirWithTypes loads the whole package; single-file mode emits only
	// types declared in `filename`. Filter via structFile.
	absFile, _ := filepath.Abs(filename)
	inFile := func(name string) bool {
		af, ok := set.structFile[name]
		if !ok {
			return false
		}
		declFile := set.fileSet.Position(af.Pos()).Filename
		if declFile == filename {
			return true
		}
		if absDecl, err := filepath.Abs(declFile); err == nil && absDecl == absFile {
			return true
		}
		return false
	}
	if len(wanted) == 0 {
		for _, n := range set.order {
			if _, ok := set.annotations[n]; ok && inFile(n) {
				wanted = append(wanted, n)
			}
		}
		if len(wanted) == 0 {
			// No name filter and no annotated struct in this file. Error
			// loudly. richError so the pretty logger shows the escape hatch;
			// no source position — this is file-level.
			return nil, set.pkgName, nil, &richError{
				Msg:      fmt.Sprintf("%s: no //ggen:generate-annotated struct found in file", relPath(filename)),
				BotHint:  "missing //ggen:generate directive",
				UserHint: fmt.Sprintf("Add `//ggen:generate` above each struct you want generated, or pass struct names explicitly: `ggen %s Name1 Name2 ...`.", filepath.Base(filename)),
			}
		}
	}
	// Gate expansion to types in `filename`; sibling-declared deps are emitted
	// by their own pass (else duplicate method declarations).
	structs, err := set.resolveFiltered(wanted, inFile)
	if err != nil {
		// Position-carrying errors already prefix the filename; don't
		// double-prefix. Only prefix bare errors lacking location info.
		if _, ok := errors.AsType[*richError](err); ok {
			return nil, "", nil, err
		}
		return nil, "", nil, fmt.Errorf("%s: %w", filename, err)
	}
	var siblings map[string]struct{}
	if !degraded {
		siblings = make(map[string]struct{}, len(set.annotations))
		for n := range set.annotations {
			siblings[n] = struct{}{}
		}
	}
	return structs, set.pkgName, siblings, nil
}

// parsePackage loads every eligible .go file in dir and generates only for
// structs annotated with //ggen:generate (plus any they transitively referenced).
func parsePackage(dir string) ([]StructInfo, string, error) {
	set, err := loadDirWithTypes(dir)
	if err != nil || (len(set.structs) == 0 && len(set.aliases) == 0) {
		// AST-only fallback.
		files, ferr := eligibleFiles(dir)
		if ferr != nil {
			return nil, "", ferr
		}
		if len(files) == 0 {
			return nil, "", nil
		}
		set, err = loadStructs(files)
		if err != nil {
			return nil, "", err
		}
	}
	wanted := set.annotatedList()
	if len(wanted) == 0 {
		return nil, set.pkgName, nil
	}
	structs, err := set.resolve(wanted)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", dir, err)
	}
	return structs, set.pkgName, nil
}

func (s *structSet) annotatedList() []string {
	var out []string
	for _, n := range s.order {
		if _, ok := s.annotations[n]; ok {
			out = append(out, n)
		}
	}
	return out
}

func eligibleFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		// Skip generator output (both library and test flavours).
		if strings.HasSuffix(name, "_ggen.go") || strings.HasSuffix(name, "_ggen_test.go") {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	return files, nil
}

// extractAlias builds a StructInfo for a top-level non-struct annotated type
// (primitive, struct, or slice/map/array alias). Channels, interfaces, and
// funcs are rejected. Struct/container aliases need Go type info.
func (s *structSet) extractAlias(name string, ts *ast.TypeSpec) (StructInfo, error) {
	info := StructInfo{Name: name, IsAlias: true}

	// Type-info path (primitives + structs). Lookup via typesInfo.Defs; the
	// underlying gives the shape, the AST the literal text + import path.
	if s.typesInfo != nil {
		if obj, ok := s.typesInfo.Defs[ts.Name].(*types.TypeName); ok && obj != nil {
			return s.extractAliasFromTypes(name, obj.Type(), ts.Type)
		}
	}

	// AST-only fallback: primitive aliases + containers of primitives only.
	switch tt := ts.Type.(type) {
	case *ast.InterfaceType:
		return info, fmt.Errorf("type %s: unsupported alias underlying (interface) — JSON has no shape for it", name)
	case *ast.ChanType:
		return info, fmt.Errorf("type %s: unsupported alias underlying (channel) — JSON has no shape for it", name)
	case *ast.FuncType:
		return info, fmt.Errorf("type %s: unsupported alias underlying (function) — JSON has no shape for it", name)
	case *ast.ArrayType:
		return s.extractContainerAliasAST(name, tt)
	case *ast.MapType:
		return s.extractMapAliasAST(name, tt)
	}
	ident, ok := ts.Type.(*ast.Ident)
	if !ok {
		return info, fmt.Errorf("type %s: alias of %T requires Go module context (run ggen inside a Go module)", name, ts.Type)
	}
	kind := resolveKind(ident.Name)
	if !isSupportedAliasPrimitive(kind) {
		return info, fmt.Errorf("type %s: unsupported alias underlying type %q (primitives, structs, slices/maps/arrays of primitives accepted)", name, ident.Name)
	}
	info.AliasKind = kind
	info.AliasUnderlying = ident.Name
	return info, nil
}

// extractContainerAliasAST handles `type T []E` / `type T [N]E` without type
// info. Element must be a primitive ident; anything richer needs go/types.
func (s *structSet) extractContainerAliasAST(name string, at *ast.ArrayType) (StructInfo, error) {
	info := StructInfo{Name: name, IsAlias: true}
	elemIdent, ok := at.Elt.(*ast.Ident)
	if !ok {
		return info, fmt.Errorf("type %s: container alias with non-primitive element requires Go module context", name)
	}
	elemKind := resolveKind(elemIdent.Name)
	if !isSupportedAliasPrimitive(elemKind) && elemKind != KindBytes {
		return info, fmt.Errorf("type %s: unsupported element kind %q for container alias", name, elemIdent.Name)
	}
	if at.Len == nil {
		// Slice. `[]byte`/`[]uint8` collapses to KindBytes.
		if elemKindIsBytes(elemIdent.Name) {
			info.AliasKind = KindBytes
			info.AliasUnderlying = "[]" + elemIdent.Name
			info.AliasField = FieldInfo{Kind: KindBytes, GoType: name}
			return info, nil
		}
		info.AliasKind = KindSlice
		info.AliasUnderlying = "[]" + elemIdent.Name
		info.AliasField = FieldInfo{Kind: KindSlice, ElemType: elemIdent.Name, ElemKind: elemKind}
		return info, nil
	}
	// Array — Len is an *ast.BasicLit INT.
	lit, ok := at.Len.(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return info, fmt.Errorf("type %s: array alias length must be an integer literal", name)
	}
	n, err := strconv.Atoi(lit.Value)
	if err != nil || n < 0 {
		return info, fmt.Errorf("type %s: invalid array length %q", name, lit.Value)
	}
	info.AliasKind = KindArray
	info.AliasUnderlying = fmt.Sprintf("[%d]%s", n, elemIdent.Name)
	info.AliasField = FieldInfo{Kind: KindArray, ArrayLen: n, ElemType: elemIdent.Name, ElemKind: elemKind}
	return info, nil
}

// extractMapAliasAST handles `type T map[string]V` without type info.
// V must be a primitive ident; key must be string.
func (s *structSet) extractMapAliasAST(name string, mt *ast.MapType) (StructInfo, error) {
	info := StructInfo{Name: name, IsAlias: true}
	keyIdent, ok := mt.Key.(*ast.Ident)
	if !ok || keyIdent.Name != "string" {
		return info, fmt.Errorf("type %s: map alias key must be string", name)
	}
	valIdent, ok := mt.Value.(*ast.Ident)
	if !ok {
		return info, fmt.Errorf("type %s: map alias with non-primitive value requires Go module context", name)
	}
	valKind := resolveKind(valIdent.Name)
	if !isSupportedAliasPrimitive(valKind) {
		return info, fmt.Errorf("type %s: unsupported map value kind %q", name, valIdent.Name)
	}
	info.AliasKind = KindMap
	info.AliasUnderlying = "map[string]" + valIdent.Name
	info.AliasField = FieldInfo{Kind: KindMap, ElemType: valIdent.Name, ElemKind: valKind}
	return info, nil
}

// extractAliasFromTypes derives the alias's kind via go/types, splitting on the
// underlying shape (Basic / Struct / Slice / Map / Array). `rhs` is the AST the
// user wrote, used to recover the underlying's literal name + import path.
func (s *structSet) extractAliasFromTypes(name string, t types.Type, rhs ast.Expr) (StructInfo, error) {
	info := StructInfo{Name: name, IsAlias: true}
	underlying := t.Underlying()

	if basic, ok := underlying.(*types.Basic); ok {
		kind := resolveKind(basic.Name())
		if !isSupportedAliasPrimitive(kind) {
			return info, fmt.Errorf("type %s: unsupported alias underlying %q", name, basic.Name())
		}
		info.AliasKind = kind
		info.AliasUnderlying = basic.Name()
		return info, nil
	}

	if _, ok := underlying.(*types.Struct); ok {
		info.AliasKind = KindStruct
		info.AliasUnderlying = exprToString(rhs)
		// Probe the RHS named type's method set, NOT t — methods don't
		// propagate from `Inner` to `type Local Inner`. Recover the underlying
		// *types.TypeName via typesInfo.Uses.
		var underlyingNamed types.Type
		switch e := rhs.(type) {
		case *ast.Ident:
			if obj, ok := s.typesInfo.Uses[e].(*types.TypeName); ok {
				underlyingNamed = obj.Type()
			}
		case *ast.SelectorExpr:
			if obj, ok := s.typesInfo.Uses[e.Sel].(*types.TypeName); ok {
				underlyingNamed = obj.Type()
				if pkgIdent, ok := e.X.(*ast.Ident); ok {
					if pkgName, ok := s.typesInfo.Uses[pkgIdent].(*types.PkgName); ok {
						info.AliasUnderlyingImport = pkgName.Imported().Path()
					}
				}
			}
		}
		if underlyingNamed != nil {
			info.AliasIface = inspectType(underlyingNamed, s.stdIfaces)
		}
		// Dispatch ladder:
		//   1. ggen-shaped methods on the underlying — delegate (fastest).
		//   2. Exported fields — introspect + hand-roll (beats marshaler
		//      delegation; gives thirdparty structs locally-annotated speed).
		//   3. Opaque struct (no exported fields) with a marshaler pair — delegate.
		//   4. Nothing usable — error.
		if info.AliasIface.AppendJSON && info.AliasIface.ByteDecoder {
			return info, nil
		}
		structType := underlying.(*types.Struct)
		hasExported := false
		for field := range structType.Fields() {
			if field.Exported() {
				hasExported = true
				break
			}
		}
		if hasExported {
			// Synthesize a FieldInfo per exported field and treat the alias as
			// a plain struct (IsAlias→false) — same memory layout, so
			// `result.X` access is sound.
			for i := 0; i < structType.NumFields(); i++ {
				fv := structType.Field(i)
				if !fv.Exported() {
					continue
				}
				fi, err := s.extractFieldFromTypes(name, fv, structType.Tag(i))
				if err != nil {
					return info, fmt.Errorf("type %s: field %s: %w", name, fv.Name(), err)
				}
				if fi.Ignored {
					continue
				}
				info.Fields = append(info.Fields, fi)
			}
			info.IsAlias = false // regular struct codegen
			return info, nil
		}
		if aliasCanDelegate(info.AliasIface) {
			return info, nil
		}
		return info, fmt.Errorf("type %s: underlying struct has no exported fields and no marshal/unmarshal methods to delegate to", name)
	}

	// Slice / Map / Array container aliases — synthesize the shape FieldInfo.
	switch tt := underlying.(type) {
	case *types.Slice:
		info.AliasKind = KindSlice
		info.AliasUnderlying = exprToString(rhs)
		f := s.aliasContainerField(tt.Elem(), 0)
		if elemKindIsBytes(f.ElemType) {
			// `type Bytes []byte` collapses to KindBytes (base64).
			info.AliasKind = KindBytes
			info.AliasField = FieldInfo{Kind: KindBytes, GoType: info.AliasUnderlying}
			return info, nil
		}
		f.Kind = KindSlice
		info.AliasField = f
		return info, nil
	case *types.Map:
		if basic, ok := tt.Key().Underlying().(*types.Basic); !ok || basic.Kind() != types.String {
			return info, fmt.Errorf("type %s: map alias key must be string, got %s", name, tt.Key())
		}
		info.AliasKind = KindMap
		info.AliasUnderlying = exprToString(rhs)
		f := s.aliasContainerField(tt.Elem(), 0)
		f.Kind = KindMap
		info.AliasField = f
		return info, nil
	case *types.Array:
		info.AliasKind = KindArray
		info.AliasUnderlying = exprToString(rhs)
		f := s.aliasContainerField(tt.Elem(), int(tt.Len()))
		f.Kind = KindArray
		f.ArrayLen = int(tt.Len())
		info.AliasField = f
		return info, nil
	}

	return info, fmt.Errorf("type %s: alias of %s not supported", name, t)
}

// aliasContainerField builds a partial FieldInfo for a container alias's
// element type. The caller sets ArrayLen for arrays.
func (s *structSet) aliasContainerField(elem types.Type, _ int) FieldInfo {
	qualifier := types.RelativeTo(s.typesPkg)
	fi := FieldInfo{}
	if pe, ok := elem.(*types.Pointer); ok {
		fi.ElemPointer = true
		elem = pe.Elem()
	}
	fi.ElemType = types.TypeString(elem, qualifier)
	fi.ElemKind = resolveKind(fi.ElemType)
	fi.Iface = inspectType(elem, s.stdIfaces)
	return fi
}

// elemKindIsBytes reports whether the Go type literal is the byte type — so
// `[]byte`/`[]uint8` aliases route through the base64 codec.
func elemKindIsBytes(elem string) bool {
	return elem == "byte" || elem == "uint8"
}

// aliasCanDelegate reports whether the underlying has a marshal+unmarshal pair
// to delegate to — both directions must reach a call site, not just one.
func aliasCanDelegate(f FieldInterfaces) bool {
	if f.AppendJSON && f.ByteDecoder {
		return true
	}
	if f.JSONMarshaler && f.JSONUnmarshaler {
		return true
	}
	if (f.TextAppender || f.TextMarshaler) && f.TextUnmarshaler {
		return true
	}
	return false
}

func isSupportedAliasPrimitive(k TypeKind) bool {
	switch k {
	case KindString, KindBool,
		KindInt, KindInt8, KindInt16, KindInt32, KindInt64,
		KindUint, KindUint8, KindUint16, KindUint32, KindUint64,
		KindFloat32, KindFloat64:
		return true
	}
	return false
}

// extractFieldFromTypes builds a FieldInfo entirely from go/types data — the
// type-driven mirror of extractField, used for foreign/method-less struct
// alias fields where no *ast.Field is available.
func (s *structSet) extractFieldFromTypes(structName string, field *types.Var, tag string) (FieldInfo, error) {
	fi := FieldInfo{GoName: field.Name(), StructName: structName}
	qualifier := types.RelativeTo(s.typesPkg)
	fi.GoType = types.TypeString(field.Type(), qualifier)

	rt := reflect.StructTag(tag)
	jsonName, opts, ignored := parseJSONTag(rt.Get("json"))
	if ignored {
		fi.Ignored = true
		return fi, nil
	}
	if jsonName != "" {
		fi.JSONName = jsonName
	} else {
		fi.JSONName = fi.GoName
	}
	fi.OmitEmpty = opts.OmitEmpty
	fi.OmitZero = opts.OmitZero
	fi.String = opts.String
	fi.Format = opts.Format
	fi.Inline = opts.Inline

	if err := applyPipeTags(&fi, rt, fi.GoName); err != nil {
		return fi, err
	}

	t := field.Type()
	if ptr, ok := t.(*types.Pointer); ok {
		fi.Pointer = true
		fi.PointeeType = types.TypeString(ptr.Elem(), qualifier)
		fi.Kind = resolveKind(fi.PointeeType)
		t = ptr.Elem()
	} else {
		fi.Kind = resolveKind(fi.GoType)
	}

	switch tt := t.Underlying().(type) {
	case *types.Map:
		if basic, ok := tt.Key().Underlying().(*types.Basic); !ok || basic.Kind() != types.String {
			return fi, fmt.Errorf("map key must be string, got %s", tt.Key())
		}
		fi.Kind = KindMap
		fi.ElemType = types.TypeString(tt.Elem(), qualifier)
		fi.ElemKind = resolveKind(fi.ElemType)
		fi.ElemIface = inspectType(tt.Elem(), s.stdIfaces)
	case *types.Slice:
		// `[]byte` and `[]uint8` were already classified as KindBytes
		// by resolveKind — leave that alone so base64/hex/array paths
		// stay in play.
		if fi.Kind != KindBytes {
			fi.Kind = KindSlice
			elem := tt.Elem()
			if pe, ok := elem.(*types.Pointer); ok {
				fi.ElemPointer = true
				elem = pe.Elem()
			}
			fi.ElemType = types.TypeString(elem, qualifier)
			fi.ElemKind = resolveKind(fi.ElemType)
			fi.ElemIface = inspectType(elem, s.stdIfaces)
		}
	case *types.Array:
		if fi.Kind != KindBytes {
			fi.Kind = KindArray
			fi.ArrayLen = int(tt.Len())
			elem := tt.Elem()
			if pe, ok := elem.(*types.Pointer); ok {
				fi.ElemPointer = true
				elem = pe.Elem()
			}
			fi.ElemType = types.TypeString(elem, qualifier)
			fi.ElemKind = resolveKind(fi.ElemType)
			fi.ElemIface = inspectType(elem, s.stdIfaces)
		}
	}

	if fi.Inline {
		if fi.Kind != KindMap {
			return fi, fmt.Errorf("json:\",inline\" requires a map[string]T field, got %s", fi.GoType)
		}
	}

	fi.Iface = inspectType(field.Type(), s.stdIfaces)
	if err := checkRuleApplicability(fi); err != nil {
		return fi, attachPosition(err, s.fileSet.Position(field.Pos()))
	}
	return fi, nil
}

// attachPosition stamps pos onto every *richError in the error tree (an
// errors.Join batch holds several), so each diagnostic carries file:line:col.
// A non-richError tree is wrapped in a thin position-carrying richError.
func attachPosition(err error, pos token.Position) error {
	if setPosOnAll(err, pos) {
		return err
	}
	return &richError{Pos: pos, Msg: err.Error(), Err: err}
}

// qualifyRichErrors prefixes every richError's Msg with `<prefix>: ` (mutating
// in place), so a Struct.Field qualifier lands on every sub-error of a batch.
// A tree with no richError is wrapped in a fresh one carrying the prefixed msg.
func qualifyRichErrors(err error, prefix string) error {
	if err == nil {
		return nil
	}
	if !applyQualifier(err, prefix) {
		return &richError{Msg: fmt.Sprintf("%s: %s", prefix, err.Error()), Err: err}
	}
	return err
}

func applyQualifier(err error, prefix string) bool {
	if err == nil {
		return false
	}
	found := false
	if re, ok := err.(*richError); ok {
		re.Msg = fmt.Sprintf("%s: %s", prefix, re.Msg)
		found = true
	}
	if u, ok := err.(interface{ Unwrap() []error }); ok {
		for _, sub := range u.Unwrap() {
			if applyQualifier(sub, prefix) {
				found = true
			}
		}
		return found
	}
	if u, ok := err.(interface{ Unwrap() error }); ok {
		if applyQualifier(u.Unwrap(), prefix) {
			found = true
		}
	}
	return found
}

// setPosOnAll fills any unset Pos on every *richError in the tree, returning
// true if the tree held any. The Column is refined from the field-decl column
// to the CodeSpan column so renderers point at the offending token.
func setPosOnAll(err error, pos token.Position) bool {
	if err == nil {
		return false
	}
	found := false
	if re, ok := err.(*richError); ok {
		if !re.Pos.IsValid() {
			refined := pos
			if re.CodeSpan != "" {
				if line, ok := readSourceLine(pos.Filename, pos.Line); ok {
					refined.Column = resolveSpanCol(line, pos.Column, re.CodeSpan, re.Anchor)
				}
			}
			re.Pos = refined
		}
		found = true
	}
	if u, ok := err.(interface{ Unwrap() []error }); ok {
		for _, sub := range u.Unwrap() {
			if setPosOnAll(sub, pos) {
				found = true
			}
		}
		return found
	}
	if u, ok := err.(interface{ Unwrap() error }); ok {
		if setPosOnAll(u.Unwrap(), pos) {
			found = true
		}
	}
	return found
}

func referencedStructName(expr ast.Expr, all map[string]*ast.StructType) string {
	switch e := expr.(type) {
	case *ast.Ident:
		if _, ok := all[e.Name]; ok {
			return e.Name
		}
	case *ast.ArrayType:
		return referencedStructName(e.Elt, all)
	case *ast.StarExpr:
		return referencedStructName(e.X, all)
	}
	return ""
}

func (s *structSet) extractStruct(name string, st *ast.StructType) (StructInfo, error) {
	info := StructInfo{Name: name}
	var errs []error
	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			// Embedded field: promote its fields into the parent.
			sub, err := s.extractEmbedded(name, field)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			info.Fields = append(info.Fields, sub...)
			continue
		}
		for _, ident := range field.Names {
			if !ident.IsExported() {
				continue
			}
			fi, extractErr := extractField(name, ident.Name, field)
			if extractErr != nil {
				errs = append(errs, attachPosition(extractErr, s.fileSet.Position(field.Pos())))
				// Don't continue — extractErr (usually applicability) leaves fi
				// populated, and a parallel @-ref error still needs surfacing,
				// so fall through to resolvePipeCustoms.
			}
			if fi.Ignored {
				continue
			}
			// With type info, resolve the field's go/types.Type and probe
			// interface impls statically for hardcoded method calls.
			var fieldType types.Type
			if s.typesInfo != nil {
				if expr, ok := s.fieldExpr[name+"."+ident.Name]; ok {
					if tv, ok := s.typesInfo.Types[expr]; ok {
						fieldType = tv.Type
						fi.Iface = inspectType(fieldType, s.stdIfaces)
					}
				}
			}
			// Generic sql.Null[T] (Go 1.22): treat the V slot as a bare T.
			// Needs go/types; the AST-only loader keeps primitives on the
			// SQLNullSpec path and custom inners on the encoding/json fallback.
			if inner, imps, ok := s.sqlNullGenericInfo(name, fieldType, &fi); ok {
				fi.Kind = KindSQLNull
				fi.SQLNullInner = inner
				fi.SQLNullImports = imps
			}
			// Resolve `@Func` references. Errors are richErrors with CodeSpan
			// set; qualify every sub-error (a batch may hold several) with the
			// Struct.Field prefix so CodeSpan/hints survive.
			if err := s.resolvePipeCustoms(name, &fi, fieldType); err != nil {
				qualified := qualifyRichErrors(err, fmt.Sprintf("%s.%s", name, ident.Name))
				errs = append(errs, attachPosition(qualified, s.fileSet.Position(field.Pos())))
				continue
			}
			// Append only on clean extraction — a parse-time problem means no
			// code should be emitted for the field.
			if extractErr == nil {
				info.Fields = append(info.Fields, fi)
			}
		}
	}
	return info, errors.Join(errs...)
}

// extractEmbedded resolves an embedded field and returns the promoted fields
// that should be appended to the parent. Supports only same-package named
// struct embeddings without a json tag (stdlib semantics).
func (s *structSet) extractEmbedded(parent string, field *ast.Field) ([]FieldInfo, error) {
	if field.Tag != nil {
		return nil, fmt.Errorf("tagged embedded field in %s is not supported", parent)
	}
	typeName := ""
	switch t := field.Type.(type) {
	case *ast.Ident:
		typeName = t.Name
	case *ast.StarExpr:
		return nil, fmt.Errorf("pointer-embedded fields in %s are not supported", parent)
	case *ast.SelectorExpr:
		return nil, fmt.Errorf("cross-package embedded fields (%s) in %s are not supported",
			exprToString(field.Type), parent)
	default:
		return nil, fmt.Errorf("unsupported embedded type %T in %s", field.Type, parent)
	}
	st, ok := s.structs[typeName]
	if !ok {
		return nil, fmt.Errorf("embedded type %s in %s not found in this package", typeName, parent)
	}
	sub, err := s.extractStruct(typeName, st)
	if err != nil {
		return nil, err
	}
	return sub.Fields, nil
}

func extractField(structName, goName string, field *ast.Field) (FieldInfo, error) {
	fi := FieldInfo{GoName: goName, StructName: structName}

	if field.Tag != nil {
		tag := reflect.StructTag(strings.Trim(field.Tag.Value, "`"))
		jsonName, opts, ignored := parseJSONTag(tag.Get("json"))
		if ignored {
			fi.Ignored = true
			return fi, nil
		}
		if jsonName != "" {
			fi.JSONName = jsonName
		}
		fi.OmitEmpty = opts.OmitEmpty
		fi.OmitZero = opts.OmitZero
		fi.String = opts.String
		fi.Format = opts.Format
		fi.Inline = opts.Inline
		if err := applyPipeTags(&fi, tag, goName); err != nil {
			return fi, err
		}
	}
	if fi.JSONName == "" {
		fi.JSONName = goName
	}

	goType := exprToString(field.Type)
	fi.GoType = goType

	// Pointer wrapping: Kind/ElemType describe the pointee.
	innerExpr := field.Type
	if star, ok := innerExpr.(*ast.StarExpr); ok {
		fi.Pointer = true
		innerExpr = star.X
		fi.PointeeType = exprToString(innerExpr)
		fi.Kind = resolveKind(fi.PointeeType)
	} else {
		fi.Kind = resolveKind(goType)
	}

	// Map: restrict to string keys (JSON object names must be strings).
	if m, ok := innerExpr.(*ast.MapType); ok {
		keyIdent, isIdent := m.Key.(*ast.Ident)
		if !isIdent || keyIdent.Name != "string" {
			return fi, fmt.Errorf("map key must be string, got %s", exprToString(m.Key))
		}
		fi.Kind = KindMap
		fi.ElemType = exprToString(m.Value)
		fi.ElemKind = resolveKind(fi.ElemType)
	}

	if fi.Inline {
		if fi.Kind != KindMap {
			return fi, fmt.Errorf("json:\",inline\" requires a map[string]T field, got %s", goType)
		}
	}

	// `[]byte`/`[]uint8` stay KindBytes (resolveKind) so they take the
	// base64/hex/array path, not the generic slice writer.
	if arr, ok := innerExpr.(*ast.ArrayType); ok && fi.Kind != KindBytes {
		// `[]*T` / `[N]*T`: unwrap the star so ElemType is the pointee.
		elt := arr.Elt
		if star, ok := elt.(*ast.StarExpr); ok {
			fi.ElemPointer = true
			elt = star.X
		}
		if arr.Len == nil {
			fi.Kind = KindSlice
			fi.ElemType = exprToString(elt)
			fi.ElemKind = resolveKind(fi.ElemType)
			if fi.ElemKind == KindArray {
				fi.ElemArrayLen = arrayLenFromType(fi.ElemType)
			}
		} else {
			// Fixed-length array: [N]T — treated as a JSON tuple.
			n := 0
			if lit, ok := arr.Len.(*ast.BasicLit); ok && lit.Kind == token.INT {
				if parsed, err := strconv.Atoi(lit.Value); err == nil {
					n = parsed
				}
			}
			fi.Kind = KindArray
			fi.ArrayLen = n
			fi.ElemType = exprToString(elt)
			fi.ElemKind = resolveKind(fi.ElemType)
			if fi.ElemKind == KindArray {
				fi.ElemArrayLen = arrayLenFromType(fi.ElemType)
			}
		}
	}

	if err := checkRuleApplicability(fi); err != nil {
		return fi, err
	}
	return fi, nil
}

// arrayLenFromType pulls N out of a "[N]T" type string, 0 on parse failure.
func arrayLenFromType(typ string) int {
	if len(typ) < 3 || typ[0] != '[' {
		return 0
	}
	end := strings.IndexByte(typ, ']')
	if end < 2 {
		return 0
	}
	n, err := strconv.Atoi(typ[1:end])
	if err != nil {
		return 0
	}
	return n
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
	case *ast.IndexExpr:
		// Generic instantiation with one type arg, e.g. sql.Null[int].
		return exprToString(e.X) + "[" + exprToString(e.Index) + "]"
	case *ast.MapType:
		return "map[" + exprToString(e.Key) + "]" + exprToString(e.Value)
	case *ast.InterfaceType:
		if e.Methods == nil || len(e.Methods.List) == 0 {
			return "any"
		}
		return fmt.Sprintf("%T", expr)
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
	case "int8":
		return KindInt8
	case "int16":
		return KindInt16
	case "int32":
		return KindInt32
	case "int64":
		return KindInt64
	case "uint":
		return KindUint
	case "uint8":
		return KindUint8
	case "uint16":
		return KindUint16
	case "uint32":
		return KindUint32
	case "uint64":
		return KindUint64
	case "float32":
		return KindFloat32
	case "float64":
		return KindFloat64
	case "bool":
		return KindBool
	case "time.Time":
		return KindTime
	case "time.Duration":
		return KindDuration
	case "net.IP":
		return KindNetIP
	case "netip.Addr":
		return KindNetipAddr
	case "netip.Prefix":
		return KindNetipPrefix
	case "[]byte", "[]uint8":
		return KindBytes
	case "json.RawMessage", "jsontext.Value":
		return KindRawJSON
	case "url.URL":
		return KindURL
	case "big.Int":
		return KindBigInt
	case "big.Float":
		return KindBigFloat
	case "big.Rat":
		return KindBigRat
	case "sql.NullString", "sql.NullInt64", "sql.NullInt32", "sql.NullInt16",
		"sql.NullByte", "sql.NullBool", "sql.NullFloat64", "sql.NullTime":
		return KindSQLNull
	case "any", "interface{}":
		return KindAny
	default:
		// sql.Null[T] with a supported inner; unsupported inners fall through
		// to KindStruct → encoding/json fallback.
		if inner, ok := sqlNullGenericInner(goType); ok && isSupportedSQLNullInner(resolveKind(inner)) {
			return KindSQLNull
		}
		if strings.HasPrefix(goType, "[]") {
			return KindSlice
		}
		if strings.HasPrefix(goType, "map[") {
			return KindMap
		}
		// [N]T — fixed-length array.
		if len(goType) > 2 && goType[0] == '[' {
			if end := strings.IndexByte(goType, ']'); end > 1 {
				if _, err := strconv.Atoi(goType[1:end]); err == nil {
					return KindArray
				}
			}
		}
		return KindStruct
	}
}

// sqlNullGenericInner extracts the inner type string T from a `sql.Null[T]`
// generic instantiation (Go 1.22). Returns ("", false) for any other type.
func sqlNullGenericInner(goType string) (string, bool) {
	const prefix = "sql.Null["
	if !strings.HasPrefix(goType, prefix) || !strings.HasSuffix(goType, "]") {
		return "", false
	}
	inner := goType[len(prefix) : len(goType)-1]
	return inner, inner != ""
}

// isSupportedSQLNullInner reports whether k may sit inside a generic sql.Null[T]
// on the AST-only path (anything else degrades to encoding/json). The go/types
// path (sqlNullGenericInfo) isn't gated by this — it handles any renderable T.
func isSupportedSQLNullInner(k TypeKind) bool {
	switch k {
	case KindString, KindBool,
		KindInt, KindInt8, KindInt16, KindInt32, KindInt64,
		KindUint, KindUint8, KindUint16, KindUint32, KindUint64,
		KindFloat32, KindFloat64, KindTime:
		return true
	}
	return false
}

// isStdSQLNull reports whether named is the generic database/sql.Null[T]
// (Go 1.22), i.e. a single-type-arg instantiation of database/sql.Null.
func isStdSQLNull(named *types.Named) bool {
	obj := named.Obj()
	return obj != nil && obj.Name() == "Null" && obj.Pkg() != nil &&
		obj.Pkg().Path() == "database/sql" && named.TypeArgs().Len() == 1
}

// sqlNullGenericInfo detects a generic sql.Null[T] field and builds the
// synthetic FieldInfo for T (via extractFieldFromTypes) plus the foreign
// imports the emitted type literals reference. ok=false for non-sql.Null or
// without type info. parent supplies the JSON name for inner diagnostics.
func (s *structSet) sqlNullGenericInfo(structName string, t types.Type, parent *FieldInfo) (*FieldInfo, []string, bool) {
	named, ok := t.(*types.Named)
	if !ok || !isStdSQLNull(named) {
		return nil, nil, false
	}
	innerType := named.TypeArgs().At(0)
	innerVar := types.NewVar(token.NoPos, s.typesPkg, "V", innerType)
	inner, err := s.extractFieldFromTypes(structName, innerVar, "")
	if err != nil {
		return nil, nil, false // unmodelable inner — stays on the fallback path
	}
	// extractFieldFromTypes peels a named type's underlying (uuid.UUID →
	// [16]byte). The AST path keeps a named type as KindStruct unless
	// resolveKind knows the name. Mirror that: trust resolveKind on the name
	// and drop the spurious element data.
	if _, isNamed := innerType.(*types.Named); isNamed {
		inner.Kind = resolveKind(inner.GoType)
		inner.ElemType, inner.ElemKind = "", 0
		inner.ArrayLen, inner.ElemArrayLen = 0, 0
		inner.ElemPointer = false
		inner.ElemIface = FieldInterfaces{}
	}
	inner.JSONName = parent.JSONName

	// extractFieldFromTypes qualifies foreign types by import PATH, but
	// generated code uses package NAME. Rewrite path → name and collect imports.
	pkgs := map[string]string{} // import path → package name
	s.collectTypeImports(innerType, pkgs)
	fix := func(str string) string {
		for path, name := range pkgs {
			str = strings.ReplaceAll(str, path+".", name+".")
		}
		return str
	}
	inner.GoType = fix(inner.GoType)
	inner.ElemType = fix(inner.ElemType)
	inner.PointeeType = fix(inner.PointeeType)

	imps := make([]string, 0, len(pkgs)+1)
	imps = append(imps, "database/sql")
	for p := range pkgs {
		imps = append(imps, p)
	}
	slices.Sort(imps)
	return &inner, imps, true
}

// collectTypeImports walks a types.Type and records (import path → package
// name) for every named type defined outside the package being generated.
// Used to import + name-qualify the inner type referenced by a sql.Null[T]
// type literal (and any nested element types, e.g. sql.Null[[]uuid.UUID] /
// sql.Null[map[string]decimal.Decimal]).
func (s *structSet) collectTypeImports(t types.Type, out map[string]string) {
	switch x := t.(type) {
	case *types.Named:
		if obj := x.Obj(); obj != nil && obj.Pkg() != nil && obj.Pkg() != s.typesPkg {
			out[obj.Pkg().Path()] = obj.Pkg().Name()
		}
		for i := range x.TypeArgs().Len() {
			s.collectTypeImports(x.TypeArgs().At(i), out)
		}
	case *types.Pointer:
		s.collectTypeImports(x.Elem(), out)
	case *types.Slice:
		s.collectTypeImports(x.Elem(), out)
	case *types.Array:
		s.collectTypeImports(x.Elem(), out)
	case *types.Map:
		s.collectTypeImports(x.Key(), out)
		s.collectTypeImports(x.Elem(), out)
	}
}
