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

// fileBuildConstraint returns the canonical //go:build expression for f,
// or "" when no build constraint is present. The result is normalized via
// constraint.Parse + Expr.String() so semantically equivalent forms (e.g.
// `foo && bar` and ` foo  &&  bar `) collapse into the same bucket key.
//
// Old-style `// +build` comments are also honored — they're folded into
// an equivalent expression by go/build/constraint when the new-style line
// is absent. The parser only inspects comments before the package clause,
// matching Go's own constraint-recognition rules.
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
	// `// +build` lines are AND'd together (terms within a line are OR'd —
	// constraint.Parse already encodes that into the per-line Expr).
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
	nosortkeys    bool // opt out: emit fields in declaration order instead of JSON-name sorted
	usenumber     bool // opt in: decode JSON numbers into `any` fields as json.Number instead of float64
	htmlescape    bool // opt in: HTML-safe escape <, >, & in emitted strings (default: literal, matches jsonv2)
}

type structSet struct {
	structs     map[string]*ast.StructType
	aliases     map[string]*ast.TypeSpec // top-level non-struct annotated types (alias of a primitive)
	order       []string
	annotations map[string]annotationFlags
	// fromTest is the set of struct names declared in a *_test.go file. Used
	// to route generated methods into *_ggen_test.go so they don't bundle with
	// library builds. Absence means non-test.
	fromTest map[string]struct{}
	pkgName  string

	// fileSet / typesInfo / typesPkg are populated when the structs were
	// loaded via golang.org/x/tools/go/packages with type info. Used by
	// the generator at codegen time to detect interface implementations
	// (TextMarshaler, ByteDecoder, etc.) on field types.
	fileSet   *token.FileSet
	typesInfo *types.Info
	typesPkg  *types.Package
	stdIfaces stdInterfaces
	// fieldExpr maps "<StructName>.<FieldName>" → the AST type expression
	// for that field, so extractField can look it up in typesInfo to
	// resolve the field's go/types.Type.
	fieldExpr map[string]ast.Expr
	// structFile maps struct name → the *ast.File it was declared in. Used
	// to resolve @pkg.Func references via the file's imports (aliased imports
	// are file-scoped, not package-scoped).
	structFile map[string]*ast.File
	// structBuildTag maps struct name → canonical //go:build expression of
	// its source file (or "" for unconstrained). Generation buckets structs
	// by this so a tagged struct ends up in its own _ggen.go file with the
	// matching constraint header.
	structBuildTag map[string]string
}

// loadStructs is the AST-only loader. Used by tests that exercise the
// parser via temporary files without a full module context. No type info
// — FieldInterfaces flags stay zero so the generator falls back to the
// dynamic-probe cascade in cross-package paths.
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

// loadDirWithTypes loads a Go package from disk via packages.Load with
// full type info, then walks its syntax to collect annotated struct
// definitions. The resulting structSet carries TypesInfo + std-interface
// references so extractField can resolve each field's interface
// implementation flags at parse time.
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
	// With Tests=true, packages.Load typically returns the base package
	// plus a "test" variant that includes _test.go files. Pick the
	// variant with the largest Syntax slice — that one has everything.
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
		// Skip generated files and our own output to keep the same
		// behavior as the AST loader.
		if strings.HasSuffix(filename, "_ggen.go") || strings.HasSuffix(filename, "_ggen_test.go") {
			continue
		}
		isTest := strings.HasSuffix(filename, "_test.go")
		walkStructDecls(af, isTest, set)
	}
	return set, nil
}

// walkStructDecls registers every top-level struct type declared in af.
// Shared by both loaders (AST-only and packages-aware) so behavior stays
// identical regardless of how the AST was produced.
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
				// Stash each field's type expression for later type-info lookup.
				for _, field := range st.Fields.List {
					for _, ident := range field.Names {
						set.fieldExpr[name+"."+ident.Name] = field.Type
					}
				}
				continue
			}
			// Non-struct top-level type. Register every alias so an
			// explicit name filter (`ggen file.go Foo`) can target it
			// via the underlying introspection path — even when the
			// alias itself is unannotated. The annotation gate only
			// decides whether the alias auto-generates without a
			// filter (annotatedList()); BFS over fields doesn't walk
			// aliases, so unannotated entries can't be picked up by
			// accident. Unsupported underlyings (interface/chan/func)
			// still error lazily inside extractAlias when targeted.
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

// parseAnnotation looks for a "//ggen:generate" directive in the given comment
// groups, optionally followed by whitespace-separated flags (marshal,
// unmarshal, multierr, ...). Returns the parsed flags and whether the
// directive was present.
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
				case "nosortkeys":
					flags.nosortkeys = true
				case "usenumber":
					flags.usenumber = true
				case "htmlescape":
					flags.htmlescape = true
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

// resolveFiltered walks wanted + transitive struct references, but only
// expands transitive deps whose names pass allowExpand. A nil predicate
// preserves the legacy behavior (expand everything) used by package-mode
// generation. Single-file mode passes inFile so sibling-declared deps
// stay out of this file's output — their own generation pass owns them.
// The roots in `wanted` are always emitted regardless of allowExpand.
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
			// Gather rather than bail — one struct's broken tag
			// shouldn't hide problems in the next struct in the same
			// file / package. errors.Join wraps the batch; the logger
			// unwraps + renders each one separately.
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
			info.NoSort = flags.nosortkeys
			info.UseNumber = flags.usenumber
			info.HTMLEscape = flags.htmlescape
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

// parseFile loads structs from a single file. If wanted is empty, it uses
// annotated structs; if there are none, it falls back to all exported.
// Tries the packages-aware loader first (with type info); falls back to
// AST-only when the file's directory isn't a fully resolvable Go package
// (e.g., temp files in tests).
//
// The third return value (siblings) carries every annotated struct/alias
// name across the WHOLE package — even those declared in other files.
// Callers in single-file mode use this to seed generatedTypes so a
// cross-file struct reference routes to a direct DecodeFrom call instead
// of falling back to encoding/json on the first run (chicken-and-egg
// before sibling _ggen files exist on disk). Nil when the loader
// degraded to AST-only — siblings are unknown in that mode.
func parseFile(filename string, wanted []string) ([]StructInfo, string, map[string]struct{}, error) {
	dir := filepath.Dir(filename)
	set, err := loadDirWithTypes(dir)
	degraded := false
	// Degrade to AST-only when packages.Load failed OR succeeded but came
	// back empty (typical for orphan files outside a Go module — temp
	// dirs in tests, scratch files, etc.). Static interface detection is
	// off in that mode; the generator's runtime-probe cascade handles it.
	if err != nil || (len(set.structs) == 0 && len(set.aliases) == 0) {
		set, err = loadStructs([]string{filename})
		if err != nil {
			return nil, "", nil, err
		}
		degraded = true
	}
	// loadDirWithTypes loads the whole package — single-file mode must
	// only emit code for types declared in `filename`, not every annotated
	// type across the package. Filter via structFile (populated by
	// walkStructDecls with the *ast.File each type was declared in).
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
			// No explicit name filter AND no annotated struct in this
			// file. Earlier ggen versions silently fell back to "every
			// exported struct in the file" — that surprised users whose
			// scratch files happened to contain a struct with a stale
			// `ggen:` tag, since the applicability check would then
			// reject the unintended type. Be loud + helpful instead.
			//
			// Returned as a richError so the pretty logger renders the
			// Note: line with the explicit-name escape hatch. No source
			// position — this is a file-level error, not tied to any
			// particular line.
			return nil, set.pkgName, nil, &richError{
				Msg:      fmt.Sprintf("%s: no //ggen:generate-annotated struct found in file", relPath(filename)),
				BotHint:  "missing //ggen:generate directive",
				UserHint: fmt.Sprintf("Add `//ggen:generate` above each struct you want generated, or pass struct names explicitly: `ggen %s Name1 Name2 ...`.", filepath.Base(filename)),
			}
		}
	}
	// Gate transitive expansion to types declared in `filename`. Sibling-
	// declared structs (incl. transitively-referenced ones) get handled by
	// their own file's generation pass — emitting them here too would
	// produce duplicate method declarations across the package.
	structs, err := set.resolveFiltered(wanted, inFile)
	if err != nil {
		// Position-carrying errors already include the filename in their
		// `file:line:col:` prefix; double-prefixing would render as
		// `temp.go: temp.go:5:2: msg`. Pass them through untouched and
		// only prefix bare errors that lack source location info.
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
		// Degraded path: AST-only.
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

// extractAlias builds a StructInfo for a top-level non-struct annotated
// type. The accepted shapes:
//
//   - Primitive aliases — `type HtmlString string`, `type Count int`,
//     etc. Codegen emits scan + cast.
//   - Struct aliases (requires Go type info) — `type LocalUUID uuid.UUID`,
//     `type Local OtherStruct`. When the underlying struct implements a
//     marshal/unmarshal interface (TextMarshaler, JSONMarshaler, …),
//     codegen delegates to the underlying type's method via cast. The
//     no-method introspection path lands later.
//
// Slices, maps, arrays, channels, interfaces, and funcs are rejected
// for now (slice/map/array support is on the roadmap).
func (s *structSet) extractAlias(name string, ts *ast.TypeSpec) (StructInfo, error) {
	info := StructInfo{Name: name, IsAlias: true}

	// Type-info-driven path: covers primitives AND struct aliases.
	// Lookup goes through typesInfo.Defs (where type-spec definitions
	// land) rather than .Types (which is for expressions). The named
	// type's underlying determines the alias shape; the AST gives us
	// the literal text the user wrote (e.g. "uuid.UUID") so we can
	// emit casts that name the underlying type, plus its import path.
	if s.typesInfo != nil {
		if obj, ok := s.typesInfo.Defs[ts.Name].(*types.TypeName); ok && obj != nil {
			return s.extractAliasFromTypes(name, obj.Type(), ts.Type)
		}
	}

	// AST-only fallback. Without type info we can't resolve struct
	// underlyings or cross-package element types — but we can still
	// support primitive aliases and containers of primitives by
	// inspecting the AST directly. Non-JSON shapes get rejected
	// with a specific diagnostic.
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

// extractContainerAliasAST handles `type T []E` (slice) and
// `type T [N]E` (array) without type info. Element type must be a
// primitive ident — anything richer (foreign types, nested
// containers) needs the full go/types path.
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

// extractAliasFromTypes derives the alias's underlying kind via go/types.
// Splits on the underlying shape: *types.Basic for primitive aliases,
// *types.Struct for struct aliases (with method-set probing). The AST
// expr `rhs` is what the user wrote on the right-hand side of the
// `type Local <rhs>` declaration — used to recover the literal name
// of the underlying type (e.g. "Inner" or "uuid.UUID") since the
// types.Named for `Local` itself only carries its OWN name.
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
		// Underlying name + import path come from the RHS AST: a bare
		// Ident lives in the user's package; a SelectorExpr points to a
		// foreign package whose path we resolve via typesInfo.
		info.AliasUnderlying = exprToString(rhs)
		// Probe the RHS named type's method set, NOT t itself — methods
		// don't propagate from `Inner` to `type Local Inner`, so probing
		// `Local` would always return empty. Look up the RHS ident in
		// typesInfo.Uses to recover the underlying *types.TypeName.
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
		//   2. Struct has exported fields — introspect, generate fresh
		//      hand-rolled decode/encode against those fields. Faster
		//      than JSON/Text marshaler delegation, and the whole point
		//      of this codepath is to give thirdparty structs the same
		//      speed as locally-annotated ones.
		//   3. Opaque struct (no exported fields, e.g. time.Time) but
		//      has a JSON/Text marshaler pair — fall back to delegation.
		//   4. Nothing usable — error out.
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
			// Field-introspection mode: walk the underlying *types.Struct
			// and synthesize a FieldInfo per exported field, then treat
			// the alias like a plain struct (IsAlias→false). Field
			// access via `result.X` works because Go gives `Local` and
			// the underlying the same memory layout.
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
			info.IsAlias = false // route through the regular struct codegen
			return info, nil
		}
		if aliasCanDelegate(info.AliasIface) {
			return info, nil
		}
		return info, fmt.Errorf("type %s: underlying struct has no exported fields and no marshal/unmarshal methods to delegate to", name)
	}

	// Slice / Map / Array container aliases. Synthesize a FieldInfo
	// describing the shape; the alias renderers reuse the existing
	// slice/map/array emitters with `result` as ref.
	switch tt := underlying.(type) {
	case *types.Slice:
		info.AliasKind = KindSlice
		info.AliasUnderlying = exprToString(rhs)
		f := s.aliasContainerField(tt.Elem(), 0)
		if elemKindIsBytes(f.ElemType) {
			// `type Bytes []byte` collapses to KindBytes for base64
			// encoding (matching the field-level shorthand).
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

// aliasContainerField builds a partial FieldInfo describing the element
// type of a slice/map/array alias. ArrayLen is filled by the caller for
// array aliases.
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

// elemKindIsBytes reports whether the given Go type literal names the
// byte type — used to recognize `[]byte`/`[]uint8` aliases that should
// route through the base64 codec rather than the generic slice path.
func elemKindIsBytes(elem string) bool {
	return elem == "byte" || elem == "uint8"
}

// aliasCanDelegate reports whether a struct-alias's underlying type
// has at least one marshal+unmarshal method pair we can delegate to.
// One direction with no counterpart isn't enough — both sides of the
// roundtrip must reach a ggen-emitted call site.
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

// extractFieldFromTypes builds a FieldInfo entirely from go/types data.
// Used when the alias underlying is a struct from another package (or
// even our own) that we want to treat as if its fields were declared
// locally — method-less struct aliases, in particular. The
// type-driven path mirrors extractField's AST-driven logic but works
// without reference to the original *ast.Field, which we don't have
// for foreign-package types.
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

	vt, err := parseValidationTagE(rt.Get("ggen"))
	if err != nil {
		return fi, fmt.Errorf("field %s: %w", fi.GoName, err)
	}
	fi.Validation = vt.Outer
	fi.KeyValidation = vt.Keys
	fi.HintLen = vt.HintLen
	if len(vt.Levels) > 0 {
		fi.ElemValidation = vt.Levels[0]
	}
	if len(vt.Levels) > 1 {
		fi.InnerValidation = vt.Levels[1:]
	}
	mt := parseModTag(rt.Get("mod"))
	fi.Mods = mt.Outer
	fi.KeyMods = mt.Keys
	if len(mt.Levels) > 0 {
		fi.ElemMods = mt.Levels[0]
	}
	if len(mt.Levels) > 1 {
		fi.InnerMods = mt.Levels[1:]
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

// attachPosition stamps a source position onto every *richError in
// the error tree so each surfaced diagnostic carries file:line:col.
// errors.Join'd batches from applicability.go contain multiple
// independent richErrors — applying the position only to the first
// (errors.AsType returns the first match) would leave subsequent
// sub-errors position-less. setPosOnAll walks the tree recursively
// to fix every one.
//
// When err contains no richError at all, a thin wrapper carries the
// position and message.
func attachPosition(err error, pos token.Position) error {
	if setPosOnAll(err, pos) {
		return err
	}
	return &richError{Pos: pos, Msg: err.Error(), Err: err}
}

// qualifyRichErrors walks an error tree and prefixes every richError's
// Msg with `<prefix>: `. Used to stamp the Struct.Field qualifier onto
// every sub-error in an errors.Join batch from resolveCustomRules —
// when a field has two failing @-refs (e.g. unresolvable val + mod),
// both messages need the qualifier, not just the first one
// errors.AsType would surface.
//
// Returns the same err (mutating richErrors in place); non-richError
// nodes are unchanged. If the tree carries no richError at all,
// returns a fresh richError wrapping err with the prefixed message
// so the caller can still attach a position.
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

// setPosOnAll walks an error tree and fills in any unset Pos on every
// *richError encountered. Returns true if at least one richError was
// touched (or already present), false when the tree carries none.
//
// When stamping Pos, the Column is refined from the field-declaration
// column (what token.Position carries) to the column of the CodeSpan
// inside the source line — so both pretty and concise renderers point
// users at the offending token rather than at the field name. This
// requires reading the source line once per error tree branch; result
// is cached in readSourceLine.
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
				// Don't `continue` here: applicability failures (the
				// usual cause of extractErr) leave FieldInfo fully
				// populated, and a parallel `@FuncName` in a different
				// tag list (e.g. unknown ggen rule + unresolved mod
				// @ref) is a legitimately-separate error the user
				// needs to see. Fall through so resolveCustomRules
				// runs too. fi.Ignored skips this entire path so
				// guard it before continuing.
			}
			if fi.Ignored {
				continue
			}
			// When type info is available (packages-aware loader), resolve
			// the field's go/types.Type and probe interface implementation
			// statically. The generator uses these flags to emit hardcoded
			// method calls instead of runtime probes.
			var fieldType types.Type
			if s.typesInfo != nil {
				if expr, ok := s.fieldExpr[name+"."+ident.Name]; ok {
					if tv, ok := s.typesInfo.Types[expr]; ok {
						fieldType = tv.Type
						fi.Iface = inspectType(fieldType, s.stdIfaces)
					}
				}
			}
			// Generic database/sql.Null[T] (Go 1.22): decode/encode the V slot
			// like a bare field of type T. Needs go/types to resolve the inner
			// type; the AST-only loader leaves primitives on the SQLNullSpec
			// path (resolveKind already classified them) and custom inners on
			// the encoding/json fallback.
			if inner, imps, ok := s.sqlNullGenericInfo(name, fieldType, &fi); ok {
				fi.Kind = KindSQLNull
				fi.SQLNullInner = inner
				fi.SQLNullImports = imps
			}
			// Resolve any `@Func` references in validation/mod tags.
			// resolveCustomRules returns *richError with CodeSpan
			// already set to the `@ref` token so the pretty renderer
			// can point its caret at the offending tag substring.
			// We prepend the Struct.Field qualifier to the existing
			// Msg (instead of wrapping into a fresh richError) so
			// CodeSpan / BotHint / UserHint survive.
			if err := s.resolveCustomRules(name, &fi, fieldType); err != nil {
				// resolveCustomRules may return an errors.Join batch
				// of multiple richErrors (one per failed @-ref).
				// Qualify EVERY sub-richError's Msg with the
				// Struct.Field prefix — qualifying only the first
				// would leave later messages naked.
				qualified := qualifyRichErrors(err, fmt.Sprintf("%s.%s", name, ident.Name))
				errs = append(errs, attachPosition(qualified, s.fileSet.Position(field.Pos())))
				continue
			}
			// Only append a valid FieldInfo if extraction itself succeeded.
			// When extractErr was set, the field had a parse-time problem
			// (e.g. unknown rule) and the gen file shouldn't emit code
			// for it.
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
		vt, err := parseValidationTagE(tag.Get("ggen"))
		if err != nil {
			return fi, fmt.Errorf("field %s: %w", goName, err)
		}
		fi.Validation = vt.Outer
		fi.KeyValidation = vt.Keys
		fi.HintLen = vt.HintLen
		if len(vt.Levels) > 0 {
			fi.ElemValidation = vt.Levels[0]
		}
		if len(vt.Levels) > 1 {
			fi.InnerValidation = vt.Levels[1:]
		}
		mt := parseModTag(tag.Get("mod"))
		fi.Mods = mt.Outer
		fi.KeyMods = mt.Keys
		if len(mt.Levels) > 0 {
			fi.ElemMods = mt.Levels[0]
		}
		if len(mt.Levels) > 1 {
			fi.InnerMods = mt.Levels[1:]
		}
	}
	if fi.JSONName == "" {
		fi.JSONName = goName
	}

	goType := exprToString(field.Type)
	fi.GoType = goType

	// Detect pointer wrapping: Kind/ElemType describe the pointee.
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

	// `[]byte` / `[]uint8` were already classified as KindBytes by
	// resolveKind — leave that alone so marshalling goes through the
	// base64/hex/array format path rather than a generic slice writer.
	// (array peel block below populates fi.ElemKind for slices/arrays;
	// applicability check at the end of this function uses the final
	// kind data.)
	if arr, ok := innerExpr.(*ast.ArrayType); ok && fi.Kind != KindBytes {
		// `[]*T` / `[N]*T`: unwrap the star so ElemType is the pointee.
		// Decoders allocate a backing slab of T and take interior pointers.
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

// arrayLenFromType pulls the N out of a "[N]T" Go type string. Returns 0
// on parse failure (defensive; resolveKind only returns KindArray when the
// prefix matches, so this should always succeed).
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
		// sql.Null[T] (Go 1.22 generic form) with a supported inner kind.
		// Unsupported inners fall through to KindStruct → encoding/json
		// fallback, matching pre-generic behaviour.
		if inner, ok := sqlNullGenericInner(goType); ok && isSupportedSQLNullInner(resolveKind(inner)) {
			return KindSQLNull
		}
		if strings.HasPrefix(goType, "[]") {
			return KindSlice
		}
		if strings.HasPrefix(goType, "map[") {
			return KindMap
		}
		// [N]T — fixed-length array (JSON tuple).
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

// isSupportedSQLNullInner reports whether kind k may sit inside a generic
// sql.Null[T] on the AST-only (string-based) path. Mirrors the inner kinds the
// SQLNullSpec primitive path handles; anything else degrades to the
// encoding/json fallback there. The go/types path (sqlNullGenericInfo) is not
// gated by this — it delegates to the full field emitters, so any inner T ggen
// can render as a field works.
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

// sqlNullGenericInfo detects a generic database/sql.Null[T] field and builds
// the synthetic FieldInfo describing the inner type T (resolved through the
// same type-driven extractor used for embedded/foreign fields), plus the
// foreign-package imports the emitted `sql.Null[T]{…}` / `var nv T` type
// literals reference. Returns ok=false for any non-sql.Null type or when no
// type info is available. parent supplies the field's JSON name for inner
// error diagnostics.
func (s *structSet) sqlNullGenericInfo(structName string, t types.Type, parent *FieldInfo) (*FieldInfo, []string, bool) {
	named, ok := t.(*types.Named)
	if !ok || !isStdSQLNull(named) {
		return nil, nil, false
	}
	innerType := named.TypeArgs().At(0)
	innerVar := types.NewVar(token.NoPos, s.typesPkg, "V", innerType)
	inner, err := s.extractFieldFromTypes(structName, innerVar, "")
	if err != nil {
		// Inner type ggen can't model as a field — leave the field on the
		// fallback path (resolveKind already classified it).
		return nil, nil, false
	}
	// extractFieldFromTypes peels a named type's underlying (e.g. uuid.UUID →
	// [16]byte → KindArray, net.IP → []byte → KindSlice). The AST field path
	// never does this — a named type stays KindStruct (routed through its
	// Text/JSON marshaler) unless resolveKind recognizes the name (time.Time,
	// net.IP, …). Mirror that: for a named inner, trust resolveKind on the
	// type name and drop the spurious element data.
	if _, isNamed := innerType.(*types.Named); isNamed {
		inner.Kind = resolveKind(inner.GoType)
		inner.ElemType, inner.ElemKind = "", 0
		inner.ArrayLen, inner.ElemArrayLen = 0, 0
		inner.ElemPointer = false
		inner.ElemIface = FieldInterfaces{}
	}
	inner.JSONName = parent.JSONName

	// extractFieldFromTypes qualifies foreign types with their full import
	// PATH (types.RelativeTo), but generated code references them by package
	// NAME. Rewrite path → name in the emitted type-literal strings and
	// collect the imports.
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
