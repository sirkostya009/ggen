package model

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

// fileBuildConstraint returns the canonical build expression for the source
// file f (or ""): its //go:build line (old-style `// +build` honored; only
// comments before the package clause count — Go's own rule) ANDed with the
// constraint its NAME implies. Normalized via constraint.Parse so equivalent
// forms collapse to one bucket key.
func fileBuildConstraint(f *ast.File, filename string) string {
	if f == nil {
		return ""
	}
	var expr constraint.Expr
	var plus []constraint.Expr
	for _, cg := range f.Comments {
		if cg.End() >= f.Package {
			break
		}
		for _, c := range cg.List {
			text := c.Text
			if constraint.IsGoBuild(text) {
				if e, err := constraint.Parse(text); err == nil {
					expr = e
					plus = nil
				}
			} else if expr == nil && constraint.IsPlusBuild(text) {
				if e, err := constraint.Parse(text); err == nil {
					plus = append(plus, e)
				}
			}
		}
	}
	// `// +build` lines AND together (intra-line OR already encoded by Parse).
	for _, e := range plus {
		expr = andExpr(expr, e)
	}
	expr = andExpr(expr, filenameConstraint(filename, f))
	if expr == nil {
		return ""
	}
	return expr.String()
}

func andExpr(x, y constraint.Expr) constraint.Expr {
	switch {
	case x == nil:
		return y
	case y == nil:
		return x
	}
	return &constraint.AndExpr{X: x, Y: y}
}

// filenameConstraint is the constraint a Go file carries without any
// comment: the `_GOOS`, `_GOARCH` or `_GOOS_GOARCH` suffix of its name
// (go/build's rule — the part before the first `_` never counts, a trailing
// `_test` is stripped first) and `cgo` when it imports "C". Without it a
// struct declared in `os_linux.go` landed in the unconstrained output and
// every other GOOS failed to compile the generated methods.
func filenameConstraint(filename string, f *ast.File) constraint.Expr {
	var expr constraint.Expr
	name, _, _ := strings.Cut(filepath.Base(filename), ".")
	if i := strings.IndexByte(name, '_'); i >= 0 {
		l := strings.Split(name[i:], "_")
		if n := len(l); n > 0 && l[n-1] == "test" {
			l = l[:n-1]
		}
		n := len(l)
		switch {
		case n >= 2 && knownOS(l[n-2]) && knownArch(l[n-1]):
			expr = &constraint.AndExpr{X: &constraint.TagExpr{Tag: l[n-2]}, Y: &constraint.TagExpr{Tag: l[n-1]}}
		case n >= 1 && (knownOS(l[n-1]) || knownArch(l[n-1])):
			expr = &constraint.TagExpr{Tag: l[n-1]}
		}
	}
	for _, imp := range f.Imports {
		if imp.Path.Value == `"C"` {
			expr = andExpr(expr, &constraint.TagExpr{Tag: "cgo"})
			break
		}
	}
	return expr
}

// knownOS / knownArch mirror internal/syslist (the lists go/build uses for
// filename-implied constraints).
func knownOS(s string) bool {
	switch s {
	case "aix", "android", "darwin", "dragonfly", "freebsd", "hurd", "illumos",
		"ios", "js", "linux", "nacl", "netbsd", "openbsd", "plan9", "solaris",
		"wasip1", "windows", "zos":
		return true
	}
	return false
}

func knownArch(s string) bool {
	switch s {
	case "386", "amd64", "amd64p32", "arm", "armbe", "arm64", "arm64be",
		"loong64", "mips", "mipsle", "mips64", "mips64le", "mips64p32",
		"mips64p32le", "ppc", "ppc64", "ppc64le", "riscv", "riscv64", "s390",
		"s390x", "sparc", "sparc64", "wasm":
		return true
	}
	return false
}

type Flags struct {
	Marshal          bool
	Unmarshal        bool
	MultiErr         bool
	AllowDups        bool
	NoValidate       bool
	IgnoreUnknown    bool // opt in: silently skip unknown JSON keys (default: error)
	NullZero         bool // opt in: accept explicit JSON null on every non-pointer value field (null → Go zero)
	NoSortKeys       bool // opt out: emit fields in declaration order instead of JSON-name sorted
	UseNumber        bool // opt in: decode JSON numbers into `any` fields as json.Number instead of float64
	HTMLEscape       bool // opt in: HTML-safe escape <, >, & in emitted strings (default: literal, matches jsonv2)
	Copy             bool // opt in: bytes-path DecodeFrom copies strings/RawMessage/any instead of aliasing data
	AllowInvalidUTF8 bool // opt out: skip decode UTF-8 validation for this struct
}

type structSet struct {
	structs     map[string]*ast.StructType
	aliases     map[string]*ast.TypeSpec // top-level non-struct annotated types (alias of a primitive)
	order       []string
	annotations map[string]Flags
	// declErr holds the diagnostic for a type the walk registered by name
	// only — a generic or `=` alias declaration, or a struct whose
	// annotation carries an unknown token. Surfaced when the type is asked
	// for, so a typo never silently drops a hook or a struct.
	declErr map[string]error
	// fromTest is the set of struct names from *_test.go files — routes their
	// methods into *_ggen_test.go. Absence means non-test.
	fromTest map[string]struct{}
	// docs and directives come from a type's doc comment: Text() with the
	// `//word:...` directive lines removed, and those lines by themselves.
	docs       map[string]string
	directives map[string][]string
	pkgName    string
	pkgPath    string
	// xtest marks the external test package (`package foo_test`) of a
	// directory: its output carries that package clause and its own file.
	xtest bool
	// files is every source file the set was walked from (as the FileSet
	// spells them), so single-file mode can tell which set declares a file.
	files map[string]struct{}

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
	// pkgQual / qualPath assign every foreign package the pass spells one
	// identifier (import path ↔ qualifier), see qualifierFor.
	pkgQual  map[string]string
	qualPath map[string]string
	// libPkg is the directory's base package when this set is its external
	// test package. One invocation generates both, so a reference into it is
	// judged by what that pass emits (see inspect).
	libPkg *structSet
	// perFile marks single-file mode: each source file is its own pass, so
	// passTypes is the union of what every file's pass emits.
	perFile bool
	// passGen memoizes passTypes.
	passGen map[string]struct{}
}

func newStructSet() *structSet {
	return &structSet{
		structs:        map[string]*ast.StructType{},
		aliases:        map[string]*ast.TypeSpec{},
		annotations:    map[string]Flags{},
		declErr:        map[string]error{},
		fromTest:       map[string]struct{}{},
		docs:           map[string]string{},
		directives:     map[string][]string{},
		files:          map[string]struct{}{},
		fieldExpr:      map[string]ast.Expr{},
		structFile:     map[string]*ast.File{},
		structBuildTag: map[string]string{},
		pkgQual:        map[string]string{},
		qualPath:       map[string]string{},
	}
}

func (s *structSet) empty() bool {
	return len(s.structs) == 0 && len(s.aliases) == 0 && len(s.declErr) == 0
}

// declaresFile reports whether the set was walked from filename.
func (s *structSet) declaresFile(filename string) bool {
	if _, ok := s.files[filename]; ok {
		return true
	}
	abs, err := filepath.Abs(filename)
	if err != nil {
		return false
	}
	for f := range s.files {
		if fa, err := filepath.Abs(f); err == nil && fa == abs {
			return true
		}
	}
	return false
}

// loadStructs is the AST-only loader (temp files with no module context). No
// type info — FieldInterfaces flags stay zero, so the generator uses its
// runtime-probe cascade in cross-package paths. Files of the directory's
// external test package (`package foo_test`) form the second set.
func loadStructs(filenames []string) (base, xtest *structSet, err error) {
	fset := token.NewFileSet()
	files := make([]*ast.File, 0, len(filenames))
	// The base package name comes from a library file when there is one,
	// else from a test file that is not itself the external package.
	pkgName, pkgRank := "", 3
	for _, filename := range filenames {
		af, err := parser.ParseFile(fset, filename, nil, parser.ParseComments)
		if err != nil {
			return nil, nil, fmt.Errorf("parsing %s: %w", filename, err)
		}
		files = append(files, af)
		rank := 0
		if strings.HasSuffix(filename, "_test.go") {
			rank = 1
			if strings.HasSuffix(af.Name.Name, "_test") {
				rank = 2
			}
		}
		if rank < pkgRank {
			pkgName, pkgRank = af.Name.Name, rank
		}
	}
	base = newStructSet()
	base.pkgName = pkgName
	base.fileSet = fset
	for i, af := range files {
		set := base
		if af.Name.Name != pkgName {
			if af.Name.Name != pkgName+"_test" {
				continue
			}
			if xtest == nil {
				xtest = newStructSet()
				xtest.pkgName = af.Name.Name
				xtest.xtest = true
				xtest.fileSet = fset
			}
			set = xtest
		}
		walkStructDecls(af, filenames[i], set)
	}
	return base, xtest, nil
}

// loadDirWithTypes loads a package via packages.Load with full type info and
// walks its syntax for annotated structs. The structSet carries TypesInfo +
// std-interface refs so extractField can resolve interface flags at parse time.
//
// With Tests=true, Load returns up to four variants of the directory: the
// plain package, its recompilation with internal test files (ForTest ==
// PkgPath), the external `<pkg>_test` package, and the synthetic test main.
// The base set is the test recompilation (a superset of the plain one); the
// external test package is a distinct type-checked package and gets its own
// set.
func loadDirWithTypes(dir string) (base, xtest *structSet, err error) {
	cfg := &packages.Config{
		Dir: dir,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports |
			packages.NeedForTest,
		Tests: true,
	}
	pkgs, err := packages.Load(cfg, ".")
	if err != nil {
		return nil, nil, fmt.Errorf("packages.Load %s: %w", dir, err)
	}
	if len(pkgs) == 0 {
		return nil, nil, fmt.Errorf("no packages loaded for %s", dir)
	}
	var plain, internal, external *packages.Package
	for _, p := range pkgs {
		if p.Types == nil {
			continue
		}
		switch {
		case p.ForTest == "":
			// The synthetic test main sits at `<plain>.test`.
			if plain == nil || p.PkgPath+".test" == plain.PkgPath {
				plain = p
			}
		case p.PkgPath == p.ForTest:
			internal = p
		case p.Name != "main":
			external = p
		}
	}
	basePkg := internal
	if basePkg == nil {
		basePkg = plain
	}
	if basePkg == nil {
		return nil, nil, fmt.Errorf("no usable package loaded for %s", dir)
	}
	std := findStdInterfaces(pkgs)
	base = newTypedStructSet(basePkg, std)
	if external != nil {
		xtest = newTypedStructSet(external, std)
		xtest.xtest = true
	}
	return base, xtest, nil
}

func newTypedStructSet(p *packages.Package, std stdInterfaces) *structSet {
	set := newStructSet()
	set.pkgName = p.Name
	set.pkgPath = p.PkgPath
	set.fileSet = p.Fset
	set.typesInfo = p.TypesInfo
	set.typesPkg = p.Types
	set.stdIfaces = std
	for _, af := range p.Syntax {
		filename := p.Fset.Position(af.Pos()).Filename
		// Skip our own generated output.
		if isGenFile(filename) {
			continue
		}
		walkStructDecls(af, filename, set)
	}
	return set
}

// isGenFile reports whether filename is ggen output (library or test flavour).
func isGenFile(filename string) bool {
	return strings.HasSuffix(filename, GenSuffix) || strings.HasSuffix(filename, GenTestSuffix)
}

// walkStructDecls registers every top-level type in af. Shared by both
// loaders so behavior is identical regardless of how the AST was produced.
func walkStructDecls(af *ast.File, filename string, set *structSet) {
	set.files[filename] = struct{}{}
	tag := fileBuildConstraint(af, filename)
	isTest := strings.HasSuffix(filename, "_test.go")
	for _, decl := range af.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts := spec.(*ast.TypeSpec)
			ts.Type = unparenType(ts.Type)
			name := ts.Name.Name
			if _, dup := set.structs[name]; dup {
				continue
			}
			if _, dup := set.aliases[name]; dup {
				continue
			}
			if _, dup := set.declErr[name]; dup {
				continue
			}
			flags, annotated, annErr := parseAnnotation(set.fileSet, gd.Doc, ts.Doc)
			set.order = append(set.order, name)
			set.structFile[name] = af
			set.structBuildTag[name] = tag
			if isTest {
				set.fromTest[name] = struct{}{}
			}
			doc := ts.Doc
			if doc == nil && len(gd.Specs) == 1 {
				doc = gd.Doc
			}
			set.docs[name], set.directives[name] = splitDoc(doc)
			if annotated {
				set.annotations[name] = flags
			}
			// A parameterized type or an `=` alias can never carry the
			// generated method set: the first needs an instantiation, the
			// second denotes a type that is not this declaration's own.
			// Registered by name only, so a reference to it takes the
			// cross-package ladder and asking for it reports why.
			if rejectErr := rejectedTypeDecl(set.fileSet, ts); rejectErr != nil {
				set.declErr[name] = rejectErr
				continue
			}
			if annErr != nil {
				set.declErr[name] = annErr
			}
			if st, ok := ts.Type.(*ast.StructType); ok {
				set.structs[name] = st
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
			set.aliases[name] = ts
		}
	}
}

// unparenType drops the parentheses Go allows inside a type expression
// (`[]([]bool)`, `*(int)`), so every switch over the type AST meets the
// composite node directly. It rewires children in place rather than building
// new nodes, keeping the go/types lookups that are keyed on them valid.
func unparenType(expr ast.Expr) ast.Expr {
	expr = ast.Unparen(expr)
	switch e := expr.(type) {
	case *ast.StarExpr:
		e.X = unparenType(e.X)
	case *ast.ArrayType:
		e.Elt = unparenType(e.Elt)
	case *ast.MapType:
		e.Key = unparenType(e.Key)
		e.Value = unparenType(e.Value)
	case *ast.ChanType:
		e.Value = unparenType(e.Value)
	case *ast.IndexExpr:
		e.X = unparenType(e.X)
		e.Index = unparenType(e.Index)
	case *ast.IndexListExpr:
		e.X = unparenType(e.X)
		for i, idx := range e.Indices {
			e.Indices[i] = unparenType(idx)
		}
	case *ast.StructType:
		if e.Fields != nil {
			for _, f := range e.Fields.List {
				f.Type = unparenType(f.Type)
			}
		}
	}
	return expr
}

func rejectedTypeDecl(fset *token.FileSet, ts *ast.TypeSpec) error {
	name := ts.Name.Name
	switch {
	case ts.TypeParams != nil:
		return &RichError{
			Pos:      fset.Position(ts.Name.Pos()),
			Msg:      fmt.Sprintf("type %s: generic types are not supported", name),
			CodeSpan: name,
			BotHint:  "methods cannot be generated for an uninstantiated type",
			UserHint: fmt.Sprintf("Declare a defined type over an instantiation (`type %sOfInt %s[int]`) and annotate that instead.", name, name),
		}
	case ts.Assign.IsValid():
		return &RichError{
			Pos:      fset.Position(ts.Name.Pos()),
			Msg:      fmt.Sprintf("type %s = %s: alias declarations cannot carry methods", name, exprToString(ts.Type)),
			CodeSpan: name,
			BotHint:  "an alias names another type, methods would land on that type",
			UserHint: fmt.Sprintf("Declare a defined type (`type %s %s`) so the generated methods have a receiver of their own.", name, exprToString(ts.Type)),
		}
	}
	return nil
}

// annotationTokens is every word a //ggen:generate directive accepts.
const annotationTokens = "marshal, unmarshal, multierr, allowdups, novalidate, ignoreunknown, nullzero, nosortkeys, " +
	"usenumber, htmlescape, copy, allowinvalidutf8"

// parseAnnotation looks for a "//ggen:generate" directive (optionally followed
// by whitespace-separated flags) and returns the flags + whether it was
// present. A word outside annotationTokens is an error, not a no-op.
// splitDoc returns a comment group's text and its directive lines
// (`//schema:in` → "schema:in"), which ast.CommentGroup.Text drops.
func splitDoc(cg *ast.CommentGroup) (text string, directives []string) {
	if cg == nil {
		return "", nil
	}
	for _, c := range cg.List {
		line := strings.TrimPrefix(c.Text, "//")
		if isDirective(line) {
			directives = append(directives, strings.TrimSpace(line))
		}
	}
	return strings.TrimSpace(cg.Text()), directives
}

// isDirective mirrors go/ast's rule for `//word:` comment lines.
func isDirective(c string) bool {
	if strings.HasPrefix(c, "line ") {
		return true
	}
	colon := strings.IndexByte(c, ':')
	if colon <= 0 || colon+1 >= len(c) {
		return false
	}
	for i := 0; i <= colon+1; i++ {
		if i == colon {
			continue
		}
		b := c[i]
		if ('a' > b || b > 'z') && ('0' > b || b > '9') {
			return false
		}
	}
	return true
}

func parseAnnotation(fset *token.FileSet, groups ...*ast.CommentGroup) (Flags, bool, error) {
	for _, cg := range groups {
		if cg == nil {
			continue
		}
		for _, c := range cg.List {
			text := strings.TrimPrefix(c.Text, "//")
			text = strings.TrimRight(text, " \t")
			rest, ok := strings.CutPrefix(text, "ggen:generate")
			// Any whitespace separates tokens (go:generate accepts tabs
			// too); a longer identifier ("ggen:generatex") does not match.
			if !ok || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
				continue
			}
			rest = strings.TrimLeft(rest, " \t")
			if rest == "" {
				return Flags{}, true, nil
			}
			var flags Flags
			var errs []error
			for tok := range strings.FieldsSeq(rest) {
				switch tok {
				case "marshal":
					flags.Marshal = true
				case "unmarshal":
					flags.Unmarshal = true
				case "multierr":
					flags.MultiErr = true
				case "allowdups":
					flags.AllowDups = true
				case "novalidate":
					flags.NoValidate = true
				case "ignoreunknown":
					flags.IgnoreUnknown = true
				case "nullzero":
					flags.NullZero = true
				case "nosortkeys":
					flags.NoSortKeys = true
				case "usenumber":
					flags.UseNumber = true
				case "htmlescape":
					flags.HTMLEscape = true
				case "copy":
					flags.Copy = true
				case "allowinvalidutf8":
					flags.AllowInvalidUTF8 = true
				default:
					errs = append(errs, &RichError{
						Pos:      fset.Position(c.Pos()),
						Msg:      fmt.Sprintf("unknown //ggen:generate token %q", tok),
						CodeSpan: tok,
						BotHint:  "unrecognized annotation token",
						UserHint: "Known tokens: " + annotationTokens + ".",
					})
				}
			}
			return flags, true, errors.Join(errs...)
		}
	}
	return Flags{}, false, nil
}

// resolve builds StructInfo for the requested structs plus any referenced
// struct types reachable from them (BFS).
func (s *structSet) resolve(wanted []string) ([]StructInfo, error) {
	return s.resolveFiltered(wanted, nil)
}

// reachable walks wanted + transitive struct references and returns the
// names to generate in discovery order, expanding only deps whose names pass
// allowExpand (nil = expand everything). Errors are per unresolvable name.
func (s *structSet) reachable(wanted []string, allowExpand func(string) bool) ([]string, []error) {
	gen := make(map[string]struct{}, len(wanted))
	queue := slices.Clone(wanted)
	var generated []string
	var errs []error
	expand := func(ref string) {
		if _, seen := gen[ref]; seen {
			return
		}
		if allowExpand != nil && !allowExpand(ref) {
			return
		}
		// An unannotated struct that already owns a wire shape is never
		// generated as a dependency — the field ladder calls its methods,
		// exactly as it does for the same type in another package.
		if _, annotated := s.annotations[ref]; !annotated && s.ownsCodec(ref) {
			return
		}
		queue = append(queue, ref)
	}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if _, ok := gen[name]; ok {
			continue
		}
		if st, ok := s.structs[name]; ok {
			gen[name] = struct{}{}
			generated = append(generated, name)
			s.structRefs(st, map[string]struct{}{name: {}}, expand)
			continue
		}
		if ts, ok := s.aliases[name]; ok {
			gen[name] = struct{}{}
			generated = append(generated, name)
			// A container/pointer alias reaches structs too — without this an
			// element type referenced ONLY through `type L []Inner` never got
			// generated methods and fell to the reflective json.Unmarshal
			// fallback (silently lenient semantics).
			switch ts.Type.(type) {
			case *ast.ArrayType, *ast.MapType, *ast.StarExpr:
				if ref := referencedStructName(ts.Type, s.structs); ref != "" {
					expand(ref)
				}
			}
			continue
		}
		if err, ok := s.declErr[name]; ok {
			errs = append(errs, err)
			continue
		}
		errs = append(errs, fmt.Errorf("type %s not found", name))
	}
	return generated, errs
}

// structRefs calls visit for every struct type st's fields name, descending
// into embedded same-package structs: their fields are promoted into the
// parent, so a struct reached only through them is decoded by the parent
// and needs its methods just the same.
func (s *structSet) structRefs(st *ast.StructType, seen map[string]struct{}, visit func(string)) {
	for _, f := range st.Fields.List {
		if len(f.Names) == 0 {
			id, ok := f.Type.(*ast.Ident)
			if !ok {
				continue
			}
			sub, ok := s.structs[id.Name]
			if !ok {
				continue
			}
			if _, cyclic := seen[id.Name]; cyclic {
				continue
			}
			seen[id.Name] = struct{}{}
			s.structRefs(sub, seen, visit)
			continue
		}
		if ref := referencedStructName(f.Type, s.structs); ref != "" {
			visit(ref)
		}
	}
}

// codecMethods are the method names that give a type a wire shape of its
// own: ggen's generated set and the JSON / text (un)marshalers.
var codecMethods = map[string]struct{}{
	"DecodeFrom": {}, "DecodeFromStream": {}, "AppendJSON": {}, "JSONSize": {},
	"MarshalJSON": {}, "UnmarshalJSON": {},
	"MarshalText": {}, "UnmarshalText": {}, "AppendText": {},
}

// ownsCodec reports whether the same-package type `name` declares a codec
// method outside generated output. Methods from a previous run's `_ggen.go`
// do not count — every generated type would otherwise vanish on the second
// run.
func (s *structSet) ownsCodec(name string) bool {
	if s.typesPkg == nil {
		return false
	}
	tn, ok := s.typesPkg.Scope().Lookup(name).(*types.TypeName)
	if !ok {
		return false
	}
	named, ok := tn.Type().(*types.Named)
	if !ok {
		return false
	}
	for m := range named.Methods() {
		if _, codec := codecMethods[m.Name()]; codec && !isGenFile(s.fileSet.Position(m.Pos()).Filename) {
			return true
		}
	}
	return false
}

// passTypes is every type the package's generation produces methods for.
// Package mode: the annotated roots plus everything reachable from them.
// Single-file mode: the union of what each file's own pass emits — its roots
// plus the reachable types declared in that same file. This is the truth a
// same-package reference is judged by; the on-disk method set is not (see
// inspect).
func (s *structSet) passTypes() map[string]struct{} {
	if s.passGen != nil {
		return s.passGen
	}
	s.passGen = map[string]struct{}{}
	add := func(names []string) {
		for _, n := range names {
			s.passGen[n] = struct{}{}
		}
	}
	if !s.perFile {
		names, _ := s.reachable(s.annotatedList(), nil)
		add(names)
		return s.passGen
	}
	byFile := map[*ast.File][]string{}
	var files []*ast.File
	for _, n := range s.annotatedList() {
		f := s.structFile[n]
		if _, seen := byFile[f]; !seen {
			files = append(files, f)
		}
		byFile[f] = append(byFile[f], n)
	}
	for _, f := range files {
		names, _ := s.reachable(byFile[f], func(name string) bool { return s.structFile[name] == f })
		add(names)
	}
	return s.passGen
}

// resolveFiltered walks wanted + transitive struct references, expanding only
// deps whose names pass allowExpand (nil = expand everything, package mode;
// single-file mode passes inFile to keep sibling-declared deps out). The roots
// in `wanted` are always emitted.
func (s *structSet) resolveFiltered(wanted []string, allowExpand func(string) bool) ([]StructInfo, error) {
	generated, errs := s.reachable(wanted, allowExpand)
	var result []StructInfo
	for _, name := range generated {
		var info StructInfo
		var err error
		if alias, ok := s.aliases[name]; ok {
			info, err = s.extractAlias(name, alias)
		} else {
			info, err = s.extractStruct(name, s.structs[name])
		}
		if declErr, ok := s.declErr[name]; ok {
			err = errors.Join(declErr, err)
		}
		if err != nil {
			// Gather rather than bail — one struct's broken tag shouldn't hide
			// the next one's.
			errs = append(errs, err)
			continue
		}
		if flags, ok := s.annotations[name]; ok {
			info.Marshal = flags.Marshal
			info.Unmarshal = flags.Unmarshal
			info.MultiErr = flags.MultiErr
			info.AllowDups = flags.AllowDups
			info.NoValidate = flags.NoValidate
			info.IgnoreUnknown = flags.IgnoreUnknown
			info.NullZero = flags.NullZero
			info.NoSort = flags.NoSortKeys
			info.UseNumber = flags.UseNumber
			info.HTMLEscape = flags.HTMLEscape
			info.Copy = flags.Copy
			info.AllowInvalidUTF8 = flags.AllowInvalidUTF8
		}
		_, info.Test = s.fromTest[name]
		info.XTest = s.xtest
		info.Doc = s.docs[name]
		info.Directives = s.directives[name]
		if af := s.structFile[name]; af != nil && s.fileSet != nil {
			info.File = s.fileSet.Position(af.Pos()).Filename
		}
		if s.typesPkg != nil {
			if obj := s.typesPkg.Scope().Lookup(name); obj != nil {
				info.Type = obj.Type()
			}
		}
		info.BuildTag = s.structBuildTag[name]
		result = append(result, info)
	}
	if len(errs) > 0 {
		return result, errors.Join(errs...)
	}
	return result, nil
}

// PrefixBare prefixes every position-less member of err's tree with
// `<prefix>: `; a RichError already carries file:line:col. A join stays a
// join, so the logger renders every member.
func PrefixBare(err error, prefix string) error {
	if err == nil {
		return nil
	}
	if m, ok := err.(interface{ Unwrap() []error }); ok {
		subs := m.Unwrap()
		out := make([]error, 0, len(subs))
		for _, sub := range subs {
			out = append(out, PrefixBare(sub, prefix))
		}
		return errors.Join(out...)
	}
	if _, ok := errors.AsType[*RichError](err); ok {
		return err
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

// FileParse is what ParseFile resolves for one file.
type FileParse struct {
	Structs []StructInfo
	PkgName string
	// Siblings is every type the package's per-file passes generate, so a
	// single-file consumer can treat a cross-file reference as generated
	// before the sibling's output exists. Nil in the AST-only degraded mode.
	Siblings map[string]struct{}
	// Package is every annotated struct of the package, resolved: cycle and
	// callee analyses need the whole package, not just this file. Nil when
	// degraded or when a sibling fails to resolve (best-effort).
	Package []StructInfo
}

// ParseFile loads structs from a single file (annotated structs when wanted is
// empty). Tries the packages-aware loader, falling back to AST-only for files
// outside a resolvable module (e.g. test temp files).
func ParseFile(filename string, wanted []string) (FileParse, error) {
	dir := filepath.Dir(filename)
	base, xtest, err := loadDirWithTypes(dir)
	degraded := false
	// Degrade to AST-only when Load failed or came back empty (orphan files
	// outside a module). Static interface detection is off in that mode.
	if err != nil || (base.empty() && (xtest == nil || xtest.empty())) {
		base, xtest, err = loadStructs([]string{filename})
		if err != nil {
			return FileParse{}, err
		}
		degraded = true
	}
	// The file belongs to exactly one of the directory's packages.
	set := base
	if xtest != nil && xtest.declaresFile(filename) {
		set = xtest
	}
	set.perFile = true
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
			// loudly. RichError so the pretty logger shows the escape hatch;
			// no source position — this is file-level.
			return FileParse{PkgName: set.pkgName}, &RichError{
				Msg:     RelPath(filename) + ": no //ggen:generate-annotated struct found in file",
				BotHint: "missing //ggen:generate directive",
				UserHint: fmt.Sprintf(
					"Add `//ggen:generate` above each struct you want generated, or pass struct names explicitly: `ggen %s Name1 Name2 ...`.",
					filepath.Base(filename),
				),
			}
		}
	}
	// Gate expansion to types in `filename`; sibling-declared deps are emitted
	// by their own pass (else duplicate method declarations).
	structs, err := set.resolveFiltered(wanted, inFile)
	if err != nil {
		return FileParse{}, PrefixBare(err, filename)
	}
	res := FileParse{Structs: structs, PkgName: set.pkgName}
	if !degraded {
		res.Siblings = maps.Clone(set.passTypes())
		if all, aerr := set.resolveFiltered(set.annotatedList(), func(string) bool { return true }); aerr == nil {
			res.Package = all
		}
	}
	return res, nil
}

// Package is one directory's resolved structs.
type Package struct {
	Dir  string
	Path string // import path; empty in the AST-only degraded mode
	Name string
	// Structs holds every generated type (annotated roots plus the structs
	// they reach), base package first, then the external test package's.
	Structs []StructInfo
	// Types is the type-checked package and Fset its positions; nil in the
	// AST-only degraded mode. Each ParsePackage call type-checks on its own, so
	// a type reached from two packages is two distinct types.Object values:
	// compare by import path and name, not by identity.
	Types *types.Package
	Fset  *token.FileSet
	// Consts lists every constant of a named string or integer type declared
	// in this package or in the exported scope of any package it imports,
	// transitively, keyed by "import/path.TypeName".
	Consts map[string][]Const
}

// Const is one typed constant, `const RoleAdmin Role = "admin"`.
type Const struct {
	Pkg   string // declaring package's import path
	Name  string
	Value string // JSON spelling: `"admin"`, `2`; a string is JSON-escaped
}

// namedConsts walks pkg and every package reachable through its imports.
// Constants declared in a test file are skipped: they are not part of the
// package's API.
func namedConsts(pkg *types.Package, fset *token.FileSet) map[string][]Const {
	if pkg == nil {
		return nil
	}
	out := map[string][]Const{}
	seen := map[*types.Package]struct{}{}
	var walk func(p *types.Package)
	walk = func(p *types.Package) {
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		scope := p.Scope()
		var consts []*types.Const
		for _, name := range scope.Names() {
			if c, ok := scope.Lookup(name).(*types.Const); ok && (p == pkg || c.Exported()) {
				consts = append(consts, c)
			}
		}
		// Scope names are sorted; enum values want declaration order.
		slices.SortFunc(consts, func(a, b *types.Const) int { return cmp.Compare(a.Pos(), b.Pos()) })
		for _, c := range consts {
			if fset != nil && strings.HasSuffix(fset.Position(c.Pos()).Filename, "_test.go") {
				continue
			}
			name := c.Name()
			named, ok := types.Unalias(c.Type()).(*types.Named)
			if !ok || named.Obj().Pkg() == nil {
				continue
			}
			basic, ok := named.Underlying().(*types.Basic)
			if !ok {
				continue
			}
			var val string
			switch {
			case basic.Info()&types.IsString != 0:
				b, err := json.Marshal(constant.StringVal(c.Val()))
				if err != nil {
					continue
				}
				val = string(b)
			case basic.Info()&types.IsInteger != 0:
				val = c.Val().ExactString()
			default:
				continue
			}
			key := named.Obj().Pkg().Path() + "." + named.Obj().Name()
			out[key] = append(out[key], Const{Pkg: p.Path(), Name: name, Value: val})
		}
		for _, imp := range p.Imports() {
			walk(imp)
		}
	}
	walk(pkg)
	if len(out) == 0 {
		return nil
	}
	return out
}

// ParsePackage loads every eligible .go file in dir and generates only for
// structs annotated with //ggen:generate (plus any they transitively
// referenced). Structs of the external test package come back with XTest
// set; Name and Path are the base package's.
func ParsePackage(dir string) (Package, error) {
	base, xtest, err := loadDirWithTypes(dir)
	if err != nil || (base.empty() && (xtest == nil || xtest.empty())) {
		// AST-only fallback.
		files, ferr := eligibleFiles(dir)
		if ferr != nil {
			return Package{}, ferr
		}
		if len(files) == 0 {
			return Package{Dir: dir}, nil
		}
		base, xtest, err = loadStructs(files)
		if err != nil {
			return Package{}, err
		}
	}
	if xtest != nil {
		// Package mode writes both packages' output in this invocation, so
		// the external test set judges a base-package reference by the base
		// pass. (Single-file mode does not: ParseFile leaves it unset.)
		xtest.libPkg = base
	}
	var structs []StructInfo
	var errs []error
	for _, set := range [...]*structSet{base, xtest} {
		if set == nil {
			continue
		}
		wanted := set.annotatedList()
		if len(wanted) == 0 {
			continue
		}
		got, err := set.resolve(wanted)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		structs = append(structs, got...)
	}
	if len(errs) > 0 {
		return Package{}, PrefixBare(errors.Join(errs...), dir)
	}
	return Package{
		Dir:     dir,
		Path:    base.pkgPath,
		Name:    base.pkgName,
		Structs: structs,
		Types:   base.typesPkg,
		Fset:    base.fileSet,
		Consts:  namedConsts(base.typesPkg, base.fileSet),
	}, nil
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
	kind := ResolveKind(ident.Name)
	if !isSupportedAliasPrimitive(kind) {
		return info, fmt.Errorf(
			"type %s: unsupported alias underlying type %q (primitives, structs, slices/maps/arrays of primitives accepted)",
			name,
			ident.Name,
		)
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
	elemKind := ResolveKind(elemIdent.Name)
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
	valKind := ResolveKind(valIdent.Name)
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
		kind := ResolveKind(basic.Name())
		if !isSupportedAliasPrimitive(kind) {
			return info, fmt.Errorf("type %s: unsupported alias underlying %q", name, basic.Name())
		}
		info.AliasKind = kind
		info.AliasUnderlying = basic.Name()
		return info, nil
	}

	if _, ok := underlying.(*types.Struct); ok {
		info.AliasKind = KindStruct
		info.AliasUnderlying = s.spell(rhs)
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
						info.AliasUnderlyingImport = s.typeImport(pkgName.Imported())
					}
				}
			}
		}
		if underlyingNamed != nil {
			info.AliasIface = s.inspect(underlyingNamed)
		}
		// A same-package underlying generated in THIS pass gets its ggen
		// methods from this run's output, which the type-check only sees
		// once a previous run wrote it — probing the method set made the
		// first run hand-roll the alias and every later run delegate.
		if id, ok := rhs.(*ast.Ident); ok {
			if _, inPass := s.passTypes()[id.Name]; inPass {
				info.AliasIface.AppendJSON, info.AliasIface.JSONSize = true, true
				info.AliasIface.ByteDecoder, info.AliasIface.StreamDecoder = true, true
			}
		}
		// Dispatch ladder:
		//   1. ggen-shaped methods on the underlying — delegate (fastest).
		//   2. Exported fields — introspect + hand-roll (beats marshaler
		//      delegation; gives thirdparty structs locally-annotated speed).
		//   3. Opaque struct (no exported fields) with a marshaler pair — delegate.
		//   4. Nothing usable — error.
		// Rung 1 only applies when the alias asks for the SAME behaviour the
		// underlying was generated with. Its own annotation tokens
		// (`allowdups`, `multierr`, `novalidate`, …) are a request to emit
		// something different, and a delegating cast cannot honour them —
		// fall through to field introspection instead of silently dropping
		// them. CLI-global flags aren't in this map, so they don't block it.
		if info.AliasIface.AppendJSON && info.AliasIface.ByteDecoder && !reshapesCodegen(s.annotations[name]) {
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
			for i := range structType.NumFields() {
				fv := structType.Field(i)
				if !fv.Exported() {
					continue
				}
				fi, err := s.extractFieldFromTypes(name, fv, structType.Tag(i))
				if err != nil {
					return info, fmt.Errorf("type %s: %w", name, err)
				}
				if fi.Ignored {
					continue
				}
				// `@Func` steps resolve against the alias's own file and
				// package, as on a struct's own fields.
				if err := s.resolvePipeCustoms(name, &fi, fv.Type()); err != nil {
					qualified := qualifyRichErrors(err, fmt.Sprintf("%s.%s", name, fv.Name()))
					return info, attachPosition(qualified, s.fileSet.Position(fv.Pos()))
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
		info.AliasUnderlying = s.spell(rhs)
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
		info.AliasUnderlying = s.spell(rhs)
		f := s.aliasContainerField(tt.Elem(), 0)
		f.Kind = KindMap
		info.AliasField = f
		return info, nil
	case *types.Array:
		info.AliasKind = KindArray
		info.AliasUnderlying = s.spell(rhs)
		f := s.aliasContainerField(tt.Elem(), int(tt.Len()))
		f.Kind = KindArray
		f.ArrayLen = int(tt.Len())
		// [N]byte folds onto the base64 path exactly as it does at field
		// position. An alias carries no struct tag, so there is no
		// `format:array` opt-out here.
		foldByteArray(&f)
		if f.Kind == KindBytes {
			info.AliasKind = KindBytes
		}
		info.AliasField = f
		return info, nil
	}

	return info, fmt.Errorf("type %s: alias of %s not supported", name, t)
}

// aliasContainerField builds a partial FieldInfo for a container alias's
// element type. The caller sets ArrayLen for arrays.
func (s *structSet) aliasContainerField(elem types.Type, _ int) FieldInfo {
	fi := FieldInfo{}
	if pe, ok := elem.(*types.Pointer); ok {
		fi.ElemPointer = true
		elem = pe.Elem()
	}
	fi.ElemType = types.TypeString(elem, s.pkgQualifier())
	fi.ElemKind = ResolveKind(fi.ElemType)
	fi.ElemIface = s.inspect(elem)
	fi.TypeImports = s.foreignImports(elem)
	return fi
}

// pkgQualifier spells a foreign type by the identifier qualifierFor assigns
// its package. types.RelativeTo elides only the current package and spells
// every other one by full import path, which does not parse where the
// generator pastes it into emitted code.
func (s *structSet) pkgQualifier() types.Qualifier {
	return s.qualifierFor
}

// qualifierFor is the identifier the generated file spells package p by:
// its declared name (`<name>_` when an emitter literal claims that name for
// another package), unless another package of the pass or a package-level
// identifier already claims it (then `name2`, `name3`, …). One qualifier per
// import path, whatever alias each source file imports it under — the
// AST-derived spellings (spell) and the go/types ones (pkgQualifier) go
// through the same table, so the type text and the import block agree.
func (s *structSet) qualifierFor(p *types.Package) string {
	if p == s.typesPkg {
		return ""
	}
	if q, ok := s.pkgQual[p.Path()]; ok {
		return q
	}
	base := p.Name()
	// A name an emitter literal spells (`json`, `time`, …) is claimed for
	// that package alone; any other package under it takes `<name>_`.
	if want, ok := emitterPackages[base]; ok && want != p.Path() {
		base += "_"
	}
	q := base
	for n := 2; s.qualifierTaken(q); n++ {
		q = base + strconv.Itoa(n)
	}
	s.pkgQual[p.Path()] = q
	s.qualPath[q] = p.Path()
	return q
}

func (s *structSet) qualifierTaken(q string) bool {
	if _, ok := s.qualPath[q]; ok {
		return true
	}
	return s.typesPkg != nil && s.typesPkg.Scope().Lookup(q) != nil
}

// typeImport describes p for the generated import block: its qualifier and
// whether that differs from the declared name (then `qualifier "path"`).
func (s *structSet) typeImport(p *types.Package) TypeImport {
	q := s.qualifierFor(p)
	return TypeImport{Path: p.Path(), Name: q, Explicit: q != p.Name()}
}

// spell renders a type expression the way the generated file must spell it:
// a package selector goes through qualifierFor, so a file-scoped import alias
// (`import lf "…/leaf"`, spelled `lf.Name` in the source) and the import
// block agree. AST-only mode has no type info and keeps the source text.
func (s *structSet) spell(expr ast.Expr) string {
	if s.typesInfo == nil {
		return exprToString(expr)
	}
	return exprToStringQ(expr, func(id *ast.Ident) (string, bool) {
		pn, ok := s.typesInfo.Uses[id].(*types.PkgName)
		if !ok {
			return "", false
		}
		return s.qualifierFor(pn.Imported()), true
	})
}

// inspect probes t's method set. Any package this invocation generates —
// the pass's own, and the base package when this set is its external test
// package — is judged by what the pass emits, not by what a previous run
// left on disk: the codec methods in that output are about to be rewritten,
// and the ones the pass will write are not there yet on a first run.
func (s *structSet) inspect(t types.Type) FieldInterfaces {
	iface := inspectType(t, s.stdIfaces)
	for {
		p, ok := t.(*types.Pointer)
		if !ok {
			break
		}
		t = p.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok {
		return iface
	}
	pkg := named.Obj().Pkg()
	if lib := s.libPkg; lib != nil && pkg == lib.typesPkg {
		pass := lib.passTypes()
		if _, inPass := pass[named.Obj().Name()]; inPass {
			iface.ByteDecoder, iface.StreamDecoder = true, true
			iface.AppendJSON, iface.JSONSize = true, true
			return iface
		}
		if len(pass) == 0 {
			// That package has no roots this run, so its output is left
			// alone and whatever it declares is real.
			return iface
		}
	} else if pkg != s.typesPkg {
		return iface
	}
	for m := range named.Methods() {
		if !isGenFile(s.fileSet.Position(m.Pos()).Filename) {
			continue
		}
		switch m.Name() {
		case "DecodeFrom":
			iface.ByteDecoder = false
		case "DecodeFromStream":
			iface.StreamDecoder = false
		case "AppendJSON":
			iface.AppendJSON = false
		case "JSONSize":
			iface.JSONSize = false
		case "MarshalJSON":
			iface.JSONMarshaler = false
		case "UnmarshalJSON":
			iface.JSONUnmarshaler = false
		}
	}
	return iface
}

// elemIface probes the container element / map value of t for interface impls,
// so slice/array/map elements run the same emit ladder as the field level.
func (s *structSet) elemIface(t types.Type) FieldInterfaces {
	for {
		p, ok := t.(*types.Pointer)
		if !ok {
			break
		}
		t = p.Elem()
	}
	var elem types.Type
	switch u := t.Underlying().(type) {
	case *types.Slice:
		elem = u.Elem()
	case *types.Array:
		elem = u.Elem()
	case *types.Map:
		elem = u.Elem()
	default:
		return FieldInterfaces{}
	}
	if pe, ok := elem.(*types.Pointer); ok {
		elem = pe.Elem()
	}
	return s.inspect(elem)
}

// elemKindIsBytes reports whether the Go type literal is the byte type — so
// `[]byte`/`[]uint8` aliases route through the base64 codec.
func elemKindIsBytes(elem string) bool {
	return elem == "byte" || elem == "uint8"
}

// reshapesCodegen reports whether a per-struct annotation changes the shape of
// the emitted code (as opposed to `marshal`/`unmarshal`, which only add hook
// methods around whatever the body already does).
func reshapesCodegen(a Flags) bool {
	return a.MultiErr || a.AllowDups || a.NoValidate || a.IgnoreUnknown ||
		a.NullZero || a.NoSortKeys || a.UseNumber || a.HTMLEscape || a.Copy ||
		a.AllowInvalidUTF8
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
	//exhaustive:ignore not every kind applies here
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
	rt := reflect.StructTag(tag)
	jsonName, opts, ignored, err := parseJSONTag(rt.Get("json"))
	if err != nil {
		return fi, fmt.Errorf("field %s: %w", field.Name(), err)
	}
	if ignored {
		fi.Ignored = true
		return fi, nil
	}
	if what := unsupportedFieldType(field.Type()); what != "" {
		// Plain, so the alias caller's `type X:` qualifier and the file
		// prefix both still reach the message: a foreign field has no
		// position in the source being generated.
		return fi, errors.New(unsupportedTypeMsg(field.Name(), what))
	}
	qualifier := s.pkgQualifier()
	fi.GoType = types.TypeString(field.Type(), qualifier)
	fi.Type = field.Type()
	fi.NotComparable = !types.Comparable(field.Type())
	fi.UnderlyingStruct = underlyingStruct(field.Type())

	if jsonName != "" {
		fi.JSONName = jsonName
	} else {
		fi.JSONName = fi.GoName
	}
	fi.OmitEmpty = opts.OmitEmpty
	fi.OmitZero = opts.OmitZero
	fi.String = opts.String
	fi.Format = opts.Format
	fi.Embed = opts.Embed

	if err := applyPipeTags(&fi, rt, fi.GoName); err != nil {
		return fi, err
	}

	t := field.Type()
	if ptr, ok := t.(*types.Pointer); ok {
		fi.Pointer = true
		fi.PointeeType = types.TypeString(ptr.Elem(), qualifier)
		fi.Kind = ResolveKind(fi.PointeeType)
		// Peel EVERY level: the container switch below is what fills
		// ElemType/ElemKind and it matches nothing against a pointer, so a
		// half-peeled `**[]T` would leave ElemKind at its zero value
		// (KindString) and emit a string scan into a T slot.
		for {
			p, ok := t.(*types.Pointer)
			if !ok {
				break
			}
			t = p.Elem()
		}
	} else {
		fi.Kind = ResolveKind(fi.GoType)
	}

	switch tt := t.Underlying().(type) {
	case *types.Map:
		if basic, ok := tt.Key().Underlying().(*types.Basic); !ok || basic.Kind() != types.String {
			return fi, fmt.Errorf("map key must be string, got %s", tt.Key())
		}
		fi.Kind = KindMap
		fi.ElemType = types.TypeString(tt.Elem(), qualifier)
		fi.ElemKind = ResolveKind(fi.ElemType)
		fi.ElemIface = s.inspect(tt.Elem())
	case *types.Slice:
		// `[]byte` and `[]uint8` were already classified as KindBytes
		// by ResolveKind — leave that alone so base64/hex/array paths
		// stay in play.
		if fi.Kind != KindBytes {
			fi.Kind = KindSlice
			elem := tt.Elem()
			if pe, ok := elem.(*types.Pointer); ok {
				fi.ElemPointer = true
				elem = pe.Elem()
			}
			fi.ElemType = types.TypeString(elem, qualifier)
			fi.ElemKind = ResolveKind(fi.ElemType)
			fi.ElemIface = s.inspect(elem)
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
			fi.ElemKind = ResolveKind(fi.ElemType)
			fi.ElemIface = s.inspect(elem)
		}
	}
	foldByteArray(&fi)

	if fi.Embed && (fi.Kind != KindMap || fi.Pointer) {
		return fi, embedKindError(fi.Pointer, fi.GoType)
	}

	fi.Iface = s.inspect(field.Type())
	if err := checkRuleApplicability(fi, false); err != nil {
		return fi, attachPosition(err, s.fileSet.Position(field.Pos()))
	}
	return fi, nil
}

// attachPosition stamps pos onto every *RichError in the error tree (an
// errors.Join batch holds several), so each diagnostic carries file:line:col.
// A non-RichError tree is wrapped in a thin position-carrying RichError.
func attachPosition(err error, pos token.Position) error {
	if setPosOnAll(err, pos) {
		return err
	}
	return &RichError{Pos: pos, Msg: err.Error(), Err: err}
}

// qualifyRichErrors prefixes every RichError's Msg with `<prefix>: ` (mutating
// in place), so a Struct.Field qualifier lands on every sub-error of a batch.
// A tree with no RichError is wrapped in a fresh one carrying the prefixed msg.
func qualifyRichErrors(err error, prefix string) error {
	if err == nil {
		return nil
	}
	if !applyQualifier(err, prefix) {
		return &RichError{Msg: fmt.Sprintf("%s: %s", prefix, err.Error()), Err: err}
	}
	return err
}

func applyQualifier(err error, prefix string) bool {
	if err == nil {
		return false
	}
	found := false
	if re, ok := err.(*RichError); ok {
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

// setPosOnAll fills any unset Pos on every *RichError in the tree, returning
// true if the tree held any. The Column is refined from the field-decl column
// to the CodeSpan column so renderers point at the offending token.
func setPosOnAll(err error, pos token.Position) bool {
	if err == nil {
		return false
	}
	found := false
	if re, ok := err.(*RichError); ok {
		if !re.Pos.IsValid() {
			refined := pos
			if re.CodeSpan != "" {
				if line, ok := ReadSourceLine(pos.Filename, pos.Line); ok {
					refined.Column = ResolveSpanCol(line, pos.Column, re.CodeSpan, re.Anchor)
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
	case *ast.MapType:
		// Map-valued references count too — without this a struct reached
		// only through map[string]Inner never entered generatedTypes and its
		// values fell to the reflective json.Unmarshal fallback.
		return referencedStructName(e.Value, all)
	}
	return ""
}

func (s *structSet) extractStruct(name string, st *ast.StructType) (StructInfo, error) {
	return s.extractStructSeen(name, st, map[string]struct{}{name: {}})
}

// extractStructSeen carries the embedding chain from the root struct so a
// cyclic embedding (invalid Go, but ggen walks syntax before type-checking)
// is a diagnostic instead of a fatal generator stack overflow.
func (s *structSet) extractStructSeen(name string, st *ast.StructType, seen map[string]struct{}) (StructInfo, error) {
	info := StructInfo{Name: name}
	var errs []error
	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			// Embedded field: promote its fields into the parent.
			sub, err := s.extractEmbedded(name, field, seen)
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
			fi, extractErr := extractField(name, ident.Name, field, s.spell)
			fi.Doc, fi.Directives = splitDoc(field.Doc)
			if fi.Doc == "" && field.Comment != nil {
				fi.Doc = strings.TrimSpace(field.Comment.Text())
			}
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
						fi.Type = tv.Type
						fi.NotComparable = !types.Comparable(fieldType)
						fi.UnderlyingStruct = underlyingStruct(fieldType)
						fi.Iface = s.inspect(fieldType)
						fi.ElemIface = s.elemIface(fieldType)
						fi.TypeImports = s.foreignImports(fieldType)
						fi.NamedPrims = s.namedPrims(fieldType)
						// Named primitives resolve only now — re-run the
						// applicability matrix so a kind-mismatched rule on a
						// `type P string` field rejects here instead of
						// emitting broken code (generate resolves the same
						// kinds via effectiveKind and emits real rule code).
						if extractErr == nil {
							if err := checkRuleApplicability(fi, true); err != nil {
								extractErr = err
								errs = append(errs, attachPosition(err, s.fileSet.Position(field.Pos())))
							}
						}
					}
				}
			}
			// A named-const / const-expr array length (`[size]int`) reads as 0
			// from the AST — resolve it via go/types or reject loudly; a
			// silent 0 emits a strict length-0 tuple that rejects everything.
			if hasNonLiteralArrayLen(field.Type) && !s.fixConstArrayLens(&fi, fieldType) {
				errs = append(errs, attachPosition(fmt.Errorf(
					"%s.%s: fixed-array length must be an integer literal here (constant lengths need type info, and pointer-wrapped const-length arrays are unsupported)",
					name,
					ident.Name,
				), s.fileSet.Position(field.Pos())))
				continue
			}
			foldByteArray(&fi)
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
	info.Fields = resolveFieldCollisions(name, info.Fields, &errs)
	return info, errors.Join(errs...)
}

// resolveFieldCollisions applies stdlib's dominant-field rule over the
// own+promoted field set: of the fields sharing a JSON name the shallowest
// wins, a tie between the parent's OWN fields is a hard error, and a tie
// between promoted fields drops the name (stdlib ambiguity — every ggen field
// is json-tagged, so stdlib's tagged tiebreak can never differentiate). The
// `json:",embed"` catch-all maps form one more such group: jsonv2 keeps the
// shallowest, refuses two in one struct, and uses none on a deeper tie.
// Survivors that still share a Go NAME are rejected — the emitters address a
// promoted field as `result.<GoName>`, which Go reports ambiguous, while
// stdlib addresses by index path and keeps both, so a silent drop would lose
// data the stdlib wire carries.
func resolveFieldCollisions(parent string, fields []FieldInfo, errs *[]error) []FieldInfo {
	drop := map[int]struct{}{}
	byName := map[string][]int{}
	var embeds []int
	for i, f := range fields {
		if f.Embed {
			embeds = append(embeds, i)
			continue
		}
		byName[f.JSONName] = append(byName[f.JSONName], i)
	}
	// dominate keeps the shallowest of idxs: a lone minimum wins, a tie at
	// depth 0 is the parent's own mistake (error; one is kept so a broken
	// emit doesn't pile on), a deeper tie drops them all.
	dominate := func(idxs []int, clash string) {
		minD := fields[idxs[0]].EmbedDepth
		for _, i := range idxs[1:] {
			minD = min(minD, fields[i].EmbedDepth)
		}
		var atMin []int
		for _, i := range idxs {
			if fields[i].EmbedDepth == minD {
				atMin = append(atMin, i)
			}
		}
		switch {
		case len(atMin) == 1:
			for _, i := range idxs {
				if i != atMin[0] {
					drop[i] = struct{}{}
				}
			}
		case minD == 0:
			*errs = append(*errs, fmt.Errorf("%s: fields %s and %s %s",
				parent, fields[atMin[0]].GoName, fields[atMin[1]].GoName, clash))
			for _, i := range idxs[1:] {
				drop[i] = struct{}{}
			}
		default:
			for _, i := range idxs {
				drop[i] = struct{}{}
			}
		}
	}
	for _, idxs := range byName {
		if len(idxs) > 1 {
			dominate(idxs, fmt.Sprintf("share JSON name %q", fields[idxs[0]].JSONName))
		}
	}
	if len(embeds) > 1 {
		dominate(embeds, "cannot both be the json:\",embed\" catch-all map")
	}
	byGo := map[string]int{}
	for i, f := range fields {
		if _, gone := drop[i]; gone {
			continue
		}
		if j, clash := byGo[f.GoName]; clash {
			*errs = append(*errs, fmt.Errorf(
				"%s: fields %s.%s (json %q) and %s.%s (json %q) share Go name %s — ggen addresses a promoted field by name "+
					"and cannot keep both; rename one or drop the embedding",
				parent,
				fields[j].StructName,
				fields[j].GoName,
				fields[j].JSONName,
				f.StructName,
				f.GoName,
				f.JSONName,
				f.GoName,
			))
			drop[i] = struct{}{}
			continue
		}
		byGo[f.GoName] = i
	}
	if len(drop) == 0 {
		return fields
	}
	out := fields[:0]
	for i, f := range fields {
		if _, gone := drop[i]; !gone {
			out = append(out, f)
		}
	}
	return out
}

// extractEmbedded resolves an embedded field and returns the promoted fields
// that should be appended to the parent. Supports only same-package named
// struct embeddings without a json tag (stdlib semantics).
func (s *structSet) extractEmbedded(parent string, field *ast.Field, seen map[string]struct{}) ([]FieldInfo, error) {
	if field.Tag != nil {
		return nil, fmt.Errorf("tagged embedded field in %s is not supported", parent)
	}
	var typeName string
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
	if _, cyclic := seen[typeName]; cyclic {
		return nil, fmt.Errorf("cyclic embedding of %s in %s", typeName, parent)
	}
	seen[typeName] = struct{}{}
	sub, err := s.extractStructSeen(typeName, st, seen)
	delete(seen, typeName)
	if err != nil {
		return nil, err
	}
	// Promoted fields sit one embedding level deeper than where they were
	// declared — the dominant-field rule keeps the shallowest on a clash.
	for i := range sub.Fields {
		sub.Fields[i].EmbedDepth++
	}
	return sub.Fields, nil
}

// checkTagReadable rejects a struct tag that NAMES one of ggen's keys but
// whose value reflect.StructTag.Get cannot read back. Get unquotes the
// double-quoted value with Go string rules, so an invalid escape (`\'` — the
// spelling the docs' "\' is a literal quote" invites inside a backquoted
// tag) makes Get return "" and every rule in that tag vanish SILENTLY,
// emitting a field with no validation at all. Inside a backquoted tag the
// literal quote is `\\'`, which Get unescapes to `\'` for the pipe lexer.
func checkTagReadable(tag reflect.StructTag, goName string) error {
	for _, key := range [...]string{"json", "pipe", "hint"} {
		// Lookup, not Get: a legal empty value (`json:""`) reads back as
		// ("", true); only a value Go tag syntax cannot parse reports !ok.
		if _, ok := tag.Lookup(key); strings.Contains(string(tag), key+`:"`) && !ok {
			return fmt.Errorf("field %s: `%s` tag is present but unreadable — reflect.StructTag.Get returned nothing, "+
				`which silently drops every rule in it (an invalid Go string escape? inside a backquoted tag a literal quote is \', not ')`, goName, key)
		}
	}
	return nil
}

// extractField builds a FieldInfo from the field's AST; spell renders type
// expressions (structSet.spell with type info, exprToString without).
func extractField(structName, goName string, field *ast.Field, spell func(ast.Expr) string) (FieldInfo, error) {
	// HintLen -1 = unset; a tagless field must not read as an explicit hint:"0"
	fi := FieldInfo{GoName: goName, StructName: structName, HintLen: -1}

	if field.Tag != nil {
		tag := reflect.StructTag(strings.Trim(field.Tag.Value, "`"))
		if err := checkTagReadable(tag, goName); err != nil {
			return fi, err
		}
		jsonName, opts, ignored, err := parseJSONTag(tag.Get("json"))
		if err != nil {
			return fi, fmt.Errorf("field %s: %w", goName, err)
		}
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
		fi.Embed = opts.Embed
		if err := applyPipeTags(&fi, tag, goName); err != nil {
			return fi, err
		}
	}
	if fi.JSONName == "" {
		fi.JSONName = goName
	}

	if what := unsupportedTypeExpr(field.Type); what != "" {
		return fi, unsupportedTypeError(goName, what)
	}
	goType := spell(field.Type)
	fi.GoType = goType

	// Pointer wrapping: Kind/ElemType describe the pointee.
	innerExpr := field.Type
	if star, ok := innerExpr.(*ast.StarExpr); ok {
		fi.Pointer = true
		innerExpr = star.X
		fi.PointeeType = spell(innerExpr)
		fi.Kind = ResolveKind(fi.PointeeType)
		// Peel EVERY level: the map/array branches below are what fill
		// ElemType/ElemKind and they match nothing against a StarExpr, so a
		// half-peeled `**[]T` would leave ElemKind at its zero value
		// (KindString) and emit a string scan into a T slot.
		for {
			star, ok := innerExpr.(*ast.StarExpr)
			if !ok {
				break
			}
			innerExpr = star.X
		}
	} else {
		fi.Kind = ResolveKind(goType)
	}

	// Map: restrict to string keys (JSON object names must be strings).
	if m, ok := innerExpr.(*ast.MapType); ok {
		keyIdent, isIdent := m.Key.(*ast.Ident)
		if !isIdent || keyIdent.Name != "string" {
			return fi, fmt.Errorf("map key must be string, got %s", exprToString(m.Key))
		}
		fi.Kind = KindMap
		fi.ElemType = spell(m.Value)
		fi.ElemKind = ResolveKind(fi.ElemType)
	}

	if fi.Embed && (fi.Kind != KindMap || fi.Pointer) {
		return fi, embedKindError(fi.Pointer, goType)
	}

	// `[]byte`/`[]uint8` stay KindBytes (ResolveKind) so they take the
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
			fi.ElemType = spell(elt)
			fi.ElemKind = ResolveKind(fi.ElemType)
			if fi.ElemKind == KindArray {
				fi.ElemArrayLen = ArrayLenFromType(fi.ElemType)
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
			fi.ElemType = spell(elt)
			fi.ElemKind = ResolveKind(fi.ElemType)
			if fi.ElemKind == KindArray {
				fi.ElemArrayLen = ArrayLenFromType(fi.ElemType)
			}
		}
	}

	if err := checkRuleApplicability(fi, false); err != nil {
		return fi, err
	}
	return fi, nil
}

// hasNonLiteralArrayLen reports whether the type expression contains a fixed
// array whose length is not a plain INT literal (`[size]T`, `[2*2]T`) — the
// AST extractor cannot evaluate those, so they must resolve via go/types or
// be rejected.
func hasNonLiteralArrayLen(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if arr, ok := n.(*ast.ArrayType); ok && arr.Len != nil {
			if lit, ok := arr.Len.(*ast.BasicLit); !ok || lit.Kind != token.INT {
				found = true
			}
		}
		return !found
	})
	return found
}

// fixConstArrayLens re-derives a container field's shape from go/types when
// its AST spelling carries a non-literal array length: types evaluates the
// constant, and types.TypeString re-spells every nested length as a literal,
// so the string-based depth recursion (StripOneContainer / ArrayLenFromType)
// sees real numbers. Returns false when the shape can't be re-derived —
// the caller rejects the field rather than emit a length-0 tuple.
func (s *structSet) fixConstArrayLens(fi *FieldInfo, t types.Type) bool {
	if t == nil {
		return false
	}
	qual := s.pkgQualifier()
	var elem types.Type
	switch c := t.Underlying().(type) {
	case *types.Array:
		if fi.Kind != KindArray {
			return false
		}
		fi.ArrayLen = int(c.Len())
		elem = c.Elem()
	case *types.Slice:
		if fi.Kind != KindSlice {
			return false
		}
		elem = c.Elem()
	case *types.Map:
		if fi.Kind != KindMap {
			return false
		}
		fi.GoType = types.TypeString(t, qual)
		// Map values keep their stars on ElemType (the pointer-cascade route).
		fi.ElemType = types.TypeString(c.Elem(), qual)
		fi.ElemKind = ResolveKind(fi.ElemType)
		return true
	default:
		return false
	}
	fi.GoType = types.TypeString(t, qual)
	// Mirror the AST extractor: one leading `*` is already on ElemPointer.
	if pe, ok := elem.(*types.Pointer); ok && fi.ElemPointer {
		elem = pe.Elem()
	}
	fi.ElemType = types.TypeString(elem, qual)
	fi.ElemKind = ResolveKind(fi.ElemType)
	if fi.ElemKind == KindArray {
		fi.ElemArrayLen = ArrayLenFromType(fi.ElemType)
	}
	return true
}

// foldByteArray collapses `[N]byte` / `[N]uint8` onto the KindBytes (base64)
// path, keeping N in ArrayLen so the emitters can enforce the exact decoded
// length. jsonv2 base64s a byte ARRAY and REJECTS the number-array form that
// encoding/json v1 emits — ggen sides with v2, as everywhere else the two
// disagree. `format:array` opts back into the v1 tuple-of-numbers shape (and
// every other `format:` — base64url/base32/hex — works too).
func foldByteArray(fi *FieldInfo) {
	if fi.Kind != KindArray || fi.ElemPointer || fi.Format == "array" {
		return
	}
	if fi.ElemKind != KindUint8 || (fi.ElemType != "byte" && fi.ElemType != "uint8") {
		return
	}
	fi.Kind = KindBytes
	fi.ElemKind, fi.ElemType = 0, ""
}

// ArrayLenFromType pulls N out of a "[N]T" type string, 0 on parse failure.
func ArrayLenFromType(typ string) int {
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

// exprToString spells a type expression as the source wrote it.
func exprToString(expr ast.Expr) string {
	return exprToStringQ(expr, nil)
}

// exprToStringQ is exprToString with a hook for the package identifier of a
// selector: pkgQual(ident) returns the qualifier to spell it by, or false to
// keep the source text.
func exprToStringQ(expr ast.Expr, pkgQual func(*ast.Ident) (string, bool)) string {
	sub := func(e ast.Expr) string { return exprToStringQ(e, pkgQual) }
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		if id, ok := e.X.(*ast.Ident); ok && pkgQual != nil {
			if q, ok := pkgQual(id); ok {
				return q + "." + e.Sel.Name
			}
		}
		return sub(e.X) + "." + e.Sel.Name
	case *ast.ArrayType:
		if e.Len == nil {
			return "[]" + sub(e.Elt)
		}
		return fmt.Sprintf("[%s]%s", sub(e.Len), sub(e.Elt))
	case *ast.StarExpr:
		return "*" + sub(e.X)
	case *ast.ParenExpr:
		return sub(e.X)
	case *ast.BasicLit:
		return e.Value
	case *ast.IndexExpr:
		// Generic instantiation with one type arg, e.g. sql.Null[int].
		return sub(e.X) + "[" + sub(e.Index) + "]"
	case *ast.MapType:
		return "map[" + sub(e.Key) + "]" + sub(e.Value)
	case *ast.InterfaceType:
		if e.Methods == nil || len(e.Methods.List) == 0 {
			return "any"
		}
		return fmt.Sprintf("%T", expr)
	default:
		return fmt.Sprintf("%T", expr)
	}
}

// embedKindError reports what `json:",embed"` needs, naming the pointer case
// only for a field that is one.
func embedKindError(pointer bool, goType string) error {
	if pointer {
		return fmt.Errorf("json:\",embed\" requires a map[string]T field (not a pointer to one), got %s", goType)
	}
	return fmt.Errorf("json:\",embed\" requires a map[string]T field, got %s", goType)
}

// unsupportedTypeExpr names the first type literal in expr that has no
// generated shape: an anonymous struct, a func, or a chan. Returns "" when
// the whole expression is usable. exprToString has no arm for any of the
// three — its %T fallback would put a Go AST type name into the emitted file.
func unsupportedTypeExpr(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.StructType:
		return "anonymous struct"
	case *ast.FuncType:
		return "func"
	case *ast.ChanType:
		return "chan"
	case *ast.StarExpr:
		return unsupportedTypeExpr(e.X)
	case *ast.ParenExpr:
		return unsupportedTypeExpr(e.X)
	case *ast.ArrayType:
		return unsupportedTypeExpr(e.Elt)
	case *ast.MapType:
		if what := unsupportedTypeExpr(e.Key); what != "" {
			return what
		}
		return unsupportedTypeExpr(e.Value)
	case *ast.IndexExpr:
		if what := unsupportedTypeExpr(e.X); what != "" {
			return what
		}
		return unsupportedTypeExpr(e.Index)
	case *ast.IndexListExpr:
		if what := unsupportedTypeExpr(e.X); what != "" {
			return what
		}
		for _, idx := range e.Indices {
			if what := unsupportedTypeExpr(idx); what != "" {
				return what
			}
		}
	}
	return ""
}

// unsupportedTypeError rejects a field type with no generated shape. An
// anonymous struct cannot be spelled into the output at all; a func or a
// channel can, but `json.Marshal` rejects one for every value, so the whole
// struct's AppendJSON could never succeed.
func unsupportedTypeError(goName, what string) error {
	hint := "A func or channel has no JSON shape: exclude the field with `json:\"-\"`, or unexport it."
	if what == "anonymous struct" {
		hint = fmt.Sprintf("Declare a named struct type for %s and use that, or exclude the field with `json:\"-\"`.", goName)
	}
	return &RichError{
		Msg:      unsupportedTypeMsg(goName, what),
		CodeSpan: goName,
		BotHint:  "field type with no generated shape",
		UserHint: hint,
	}
}

func unsupportedTypeMsg(goName, what string) string {
	return fmt.Sprintf("field %s: %s field types are not supported", goName, what)
}

// unsupportedFieldType is unsupportedTypeExpr over go/types. A named type
// stops the walk: it is spelled by name and takes the kind its name resolves
// to, whatever it is defined over.
func unsupportedFieldType(t types.Type) string {
	switch x := types.Unalias(t).(type) {
	case *types.Struct:
		return "anonymous struct"
	case *types.Signature:
		return "func"
	case *types.Chan:
		return "chan"
	case *types.Pointer:
		return unsupportedFieldType(x.Elem())
	case *types.Slice:
		return unsupportedFieldType(x.Elem())
	case *types.Array:
		return unsupportedFieldType(x.Elem())
	case *types.Map:
		if what := unsupportedFieldType(x.Key()); what != "" {
			return what
		}
		return unsupportedFieldType(x.Elem())
	}
	return ""
}

func ResolveKind(goType string) TypeKind {
	switch goType {
	case "string":
		return KindString
	case "int":
		return KindInt
	case "int8":
		return KindInt8
	case "int16":
		return KindInt16
	case "int32", "rune":
		return KindInt32
	case "int64":
		return KindInt64
	case "uint":
		return KindUint
	case "uint8", "byte":
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
		if inner, ok := sqlNullGenericInner(goType); ok && isSupportedSQLNullInner(ResolveKind(inner)) {
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
	//exhaustive:ignore not every kind applies here
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
func (s *structSet) sqlNullGenericInfo(structName string, t types.Type, parent *FieldInfo) (*FieldInfo, []TypeImport, bool) {
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
	// ResolveKind knows the name. Mirror that: trust ResolveKind on the name
	// and drop the spurious element data.
	if _, isNamed := innerType.(*types.Named); isNamed {
		inner.Kind = ResolveKind(inner.GoType)
		inner.ElemType, inner.ElemKind = "", 0
		inner.ArrayLen, inner.ElemArrayLen = 0, 0
		inner.ElemPointer = false
		inner.ElemIface = FieldInterfaces{}
	}
	inner.JSONName = parent.JSONName
	return &inner, s.foreignImports(innerType), true
}

// foreignImports returns the sorted import paths of every package outside the
// one being generated that t names — the field's own type plus every element,
// key, pointee and type argument. The emitters spell those types out verbatim,
// so each one has to reach the generated file's import block.
func (s *structSet) foreignImports(t types.Type) []TypeImport {
	if t == nil {
		return nil
	}
	pkgs := map[string]TypeImport{}
	s.collectTypeImports(t, pkgs)
	if len(pkgs) == 0 {
		return nil
	}
	out := slices.Collect(maps.Values(pkgs))
	slices.SortFunc(out, func(a, b TypeImport) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// underlyingStruct reports whether t is a struct type — named, literal, or
// foreign. Pointers are NOT peeled: `*T` is a nillable field, where omitempty
// means "omit nil". Only go/types can answer this; on the AST path a named
// container and a foreign array type read as KindStruct too.
func underlyingStruct(t types.Type) bool {
	if t == nil {
		return false
	}
	_, ok := t.Underlying().(*types.Struct)
	return ok
}

// namedPrims returns every named type t mentions whose underlying type is a
// primitive, keyed by the same type string the emitters spell (`Priority`,
// `leaf.Name`). Types that carry their own JSON or text methods are skipped —
// those keep their method-driven wire shape, so the underlying primitive is
// not what a rule would be checking.
func (s *structSet) namedPrims(t types.Type) map[string]TypeKind {
	if t == nil {
		return nil
	}
	out := map[string]TypeKind{}
	s.collectNamedPrims(t, out, map[types.Type]struct{}{})
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *structSet) collectNamedPrims(t types.Type, out map[string]TypeKind, seen map[types.Type]struct{}) {
	if t == nil {
		return
	}
	if _, ok := seen[t]; ok {
		return
	}
	seen[t] = struct{}{}
	switch x := t.(type) {
	case *types.Named:
		if b, ok := x.Underlying().(*types.Basic); ok {
			// Source-syntax name (`leaf.Name`, not `xpkg/leaf.Name`): the key
			// has to match FieldInfo.GoType, which the AST extractor builds
			// from the type EXPRESSION. types.RelativeTo only elides the
			// current package and spells every other one by full path.
			name := types.TypeString(t, s.pkgQualifier())
			// Types the generator gives a wire shape of their own (time.Duration
			// is a named int64, …) are NOT plain named primitives: ResolveKind
			// hands them a dedicated kind and their own emitters. Only
			// KindStruct means "ggen has no idea what this type is".
			// json.Number is a named STRING whose wire shape is a NUMBER — the
			// one stdlib type where "named over a primitive" does not imply
			// "encodes like that primitive". It keeps the encoding/json path.
			if full := types.TypeString(t, nil); full == "encoding/json.Number" || full == "encoding/json/v2.Number" {
				return
			}
			if k := ResolveKind(b.Name()); isSupportedAliasPrimitive(k) && ResolveKind(name) == KindStruct {
				if iface := s.inspect(t); !iface.JSONMarshaler && !iface.JSONUnmarshaler &&
					!iface.TextMarshaler && !iface.TextUnmarshaler && !iface.TextAppender {
					out[name] = k
				}
			}
		}
		for t := range x.TypeArgs().Types() {
			s.collectNamedPrims(t, out, seen)
		}
	case *types.Pointer:
		s.collectNamedPrims(x.Elem(), out, seen)
	case *types.Slice:
		s.collectNamedPrims(x.Elem(), out, seen)
	case *types.Array:
		s.collectNamedPrims(x.Elem(), out, seen)
	case *types.Map:
		s.collectNamedPrims(x.Key(), out, seen)
		s.collectNamedPrims(x.Elem(), out, seen)
	}
}

// collectTypeImports walks a types.Type and records, by import path, every
// package defining a named type it mentions outside the package being
// generated (nested element types included, e.g. sql.Null[[]uuid.UUID] /
// map[string]decimal.Decimal).
func (s *structSet) collectTypeImports(t types.Type, out map[string]TypeImport) {
	switch x := t.(type) {
	case *types.Named:
		if obj := x.Obj(); obj != nil && obj.Pkg() != nil && obj.Pkg() != s.typesPkg {
			out[obj.Pkg().Path()] = s.typeImport(obj.Pkg())
		}
		for t := range x.TypeArgs().Types() {
			s.collectTypeImports(t, out)
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

// emitterPackages maps every package qualifier the emitters spell as a
// literal (`json.RawMessage`, `time.Parse`, `netip.ParseAddr`, …) to the
// package it means. A user package declared under one of those names
// (`internal/json`) would collide with that literal, so its types are spelled
// through the `<name>_` alias instead — see qualifierFor.
var emitterPackages = map[string]string{
	"ggen":     "github.com/sirkostya009/ggen",
	"archsimd": "simd/archsimd",
	"base32":   "encoding/base32",
	"base64":   "encoding/base64",
	"big":      "math/big",
	"bits":     "math/bits",
	"bytes":    "bytes",
	"fmt":      "fmt",
	"hex":      "encoding/hex",
	"json":     "encoding/json",
	"jsontext": "encoding/json/jsontext",
	"math":     "math",
	"net":      "net",
	"netip":    "net/netip",
	"reflect":  "reflect",
	"sql":      "database/sql",
	"strconv":  "strconv",
	"strings":  "strings",
	"time":     "time",
	"unsafe":   "unsafe",
	"url":      "net/url",
	"utf8":     "unicode/utf8",
}

const (
	GenSuffix     = "_ggen.go"
	GenTestSuffix = "_ggen_test.go"
)

// Apply ORs f into each struct's flags and propagates the struct-level flags
// down to every field.
func (f Flags) Apply(structs []StructInfo) {
	for i := range structs {
		if f.Marshal {
			structs[i].Marshal = true
		}
		if f.Unmarshal {
			structs[i].Unmarshal = true
		}
		if f.MultiErr {
			structs[i].MultiErr = true
		}
		if f.AllowDups {
			structs[i].AllowDups = true
		}
		if f.NoValidate {
			structs[i].NoValidate = true
		}
		if f.IgnoreUnknown {
			structs[i].IgnoreUnknown = true
		}
		if f.NullZero {
			structs[i].NullZero = true
		}
		if f.NoSortKeys {
			structs[i].NoSort = true
		}
		if f.UseNumber {
			structs[i].UseNumber = true
		}
		if f.HTMLEscape {
			structs[i].HTMLEscape = true
		}
		if f.Copy {
			structs[i].Copy = true
		}
		if f.AllowInvalidUTF8 {
			structs[i].AllowInvalidUTF8 = true
		}
		for j := range structs[i].Fields {
			structs[i].Fields[j].MultiErr = structs[i].MultiErr
			structs[i].Fields[j].AllowDups = structs[i].AllowDups
			structs[i].Fields[j].NoValidate = structs[i].NoValidate
			structs[i].Fields[j].UseNumber = structs[i].UseNumber
			structs[i].Fields[j].HTMLEscape = structs[i].HTMLEscape
			structs[i].Fields[j].Copy = structs[i].Copy
			structs[i].Fields[j].AllowInvalidUTF8 = structs[i].AllowInvalidUTF8
			// OR, not assign: a per-field json:",nullzero" must survive when the
			// struct flag is off.
			structs[i].Fields[j].NullZero = structs[i].Fields[j].NullZero || structs[i].NullZero
		}
	}
}

// IsPattern reports whether target is a `...` package pattern.
func IsPattern(target string) bool { return strings.HasSuffix(target, "...") }

// WalkPackages resolves every target (dirs and `...` patterns) in one
// go/packages load and invokes `act` on each matched package's directory —
// module-scoped, never crossing module bounds (like `go build <patterns>`).
// Processing is post-order over the matched import subgraph, so a package's
// `_ggen.go` lands on disk before any matched importer runs and cross-package
// field types route through direct DecodeFrom/AppendJSON rather than
// encoding/json. Deps outside the matched set are left alone. A test-only
// package is skipped when a pattern matched it and visited when it was named
// outright. Per-package load errors and act's errors go to report; only a
// failed load of the whole target set is returned.
func WalkPackages(targets []string, act func(dir string) error, report func(error)) error {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedImports,
	}
	pkgs, err := packages.Load(cfg, targets...)
	if err != nil {
		return err
	}
	explicit := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		if !IsPattern(t) {
			if abs, err := filepath.Abs(t); err == nil {
				explicit[abs] = struct{}{}
			}
		}
	}
	// A broken-import package shouldn't hide its siblings.
	for _, p := range pkgs {
		for _, e := range p.Errors {
			report(fmt.Errorf("%s: %w", p.PkgPath, e))
		}
	}

	matched := make(map[string]bool, len(pkgs))
	for _, p := range pkgs {
		matched[p.PkgPath] = true
	}
	// Dedup by directory (packages.Load can return base + test variants for
	// the same dir; generateDir loads both via Tests:true, so visit once).
	visited := make(map[string]struct{}, len(pkgs))
	seenPath := make(map[string]struct{}, len(pkgs))
	var visit func(p *packages.Package)
	visit = func(p *packages.Package) {
		if _, ok := seenPath[p.PkgPath]; ok {
			return
		}
		seenPath[p.PkgPath] = struct{}{}
		for _, imp := range p.Imports {
			if matched[imp.PkgPath] {
				visit(imp)
			}
		}
		dir := p.Dir
		if dir == "" && len(p.GoFiles) > 0 {
			dir = filepath.Dir(p.GoFiles[0])
		}
		if dir == "" {
			return
		}
		if _, named := explicit[dir]; len(p.GoFiles) == 0 && !named {
			return
		}
		if _, ok := visited[dir]; ok {
			return
		}
		visited[dir] = struct{}{}
		if err := act(dir); err != nil {
			report(PrefixBare(err, "in "+dir))
		}
	}
	for _, p := range pkgs {
		visit(p)
	}
	return nil
}
