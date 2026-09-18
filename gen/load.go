package gen

import (
	"encoding/json"
	"errors"
	"go/token"
	"go/types"
	"slices"
	"strings"
	"sync"

	"github.com/sirkostya009/ggen/gen/model"
)

// Set is the packages a script loaded.
type Set struct {
	Packages []*Package // loaded packages, in dependency order

	mu       sync.Mutex           // guards the maps below: lowering fills them lazily
	pkgs     map[string]*Package  // every package a type refers to, loaded or not
	decls    map[string]*TypeDecl // "path.Name" → generated type or enum type
	consts   map[string][]model.Const
	enums    []*TypeDecl
	unloaded map[string]struct{} // ggen-generated types reached in a package no pattern matched
}

// Package is one Go package. Every named type in the loaded packages points at
// one shared *Package; a package that was only imported gets one too, with no
// Types.
type Package struct {
	Path, Name string
	Loaded     bool        // matched by a Load pattern
	Types      []*TypeDecl // generated types, declaration order; nil unless Loaded
}

// TypeDecl is one Go type ggen generates, or a named string or integer type
// with constants that a loaded type references.
type TypeDecl struct {
	Pkg        *Package
	File       string // declaring source file
	Name, Doc  string
	Directives []string // "schema:in" from a `//schema:in` line
	Enum       *Enum    // set for an enum type; nil for a generated type
	Annotated  bool     // carries //ggen:generate itself, vs reached from a type that does

	set       *Set
	fset      *token.FileSet // positions of the load that produced info or named
	info      model.StructInfo
	named     *types.Named // set for an enum type that is not generated
	missing   bool         // Set.Type found no such type
	recursive bool
}

// Load parses exactly the packages matched by patterns (directories or
// `...` patterns, relative to the working directory).
func Load(patterns ...string) (*Set, error) {
	s := &Set{
		pkgs:     map[string]*Package{},
		decls:    map[string]*TypeDecl{},
		consts:   map[string][]model.Const{},
		unloaded: map[string]struct{}{},
	}
	var parsed []model.Package
	var errs []error
	err := model.WalkPackages(patterns, func(dir string) error {
		p, err := model.ParsePackage(dir)
		if err != nil {
			return err
		}
		if p.Types == nil {
			if len(p.Structs) == 0 {
				return nil // nothing to generate here; a pattern may match many packages
			}
			return errors.New(dir + ": no type information (is it inside a module?)")
		}
		parsed = append(parsed, p)
		return nil
	}, func(err error) { errs = append(errs, err) })
	if err != nil {
		return nil, err
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	for _, mp := range parsed {
		p := s.pkg(mp.Types)
		p.Loaded = true
		s.Packages = append(s.Packages, p)
		for k, cs := range mp.Consts {
			for _, c := range cs {
				if !slices.Contains(s.consts[k], c) {
					s.consts[k] = append(s.consts[k], c)
				}
			}
		}
		for _, info := range mp.Structs {
			if info.Test || info.XTest {
				continue
			}
			d := &TypeDecl{
				Pkg:        p,
				File:       info.File,
				Name:       info.Name,
				Doc:        info.Doc,
				Directives: info.Directives,
				Annotated:  slices.ContainsFunc(info.Directives, isAnnotation),
				set:        s,
				fset:       mp.Fset,
				info:       info,
			}
			p.Types = append(p.Types, d)
			s.decls[p.Path+"."+d.Name] = d
		}
	}
	// A generated named string or integer type with constants is an enum type
	// too; it stays in Types (not Enums), and its own shape is the closed set.
	for _, p := range s.Packages {
		for _, d := range p.Types {
			if d.info.IsAlias {
				if e := s.enumValues(p.Path+"."+d.Name, p.Path); e != nil {
					d.Enum = e
					e.Decl = d
				}
			}
		}
	}
	// Lower every type once, both ways, so every enum type any of them
	// references exists before a script asks for Enums, and recursion is known.
	refs := map[*TypeDecl][]*TypeDecl{}
	for _, p := range s.Packages {
		for _, d := range p.Types {
			add := func(r *TypeDecl) {
				if !slices.Contains(refs[d], r) {
					refs[d] = append(refs[d], r)
				}
			}
			walkShapeRefs(d.In(), add)
			walkShapeRefs(d.Out(), add)
		}
	}
	markRecursive(refs)
	return s, nil
}

func isAnnotation(directive string) bool {
	return directive == "ggen:generate" || strings.HasPrefix(directive, "ggen:generate ") || strings.HasPrefix(directive, "ggen:generate\t")
}

func (s *Set) pkg(tp *types.Package) *Package {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.pkgs[tp.Path()]; ok {
		return p
	}
	p := &Package{Path: tp.Path(), Name: tp.Name()}
	s.pkgs[tp.Path()] = p
	return p
}

// markUnloaded records a ggen-generated type reached in a package no pattern
// matched, whose fields are therefore unknown.
func (s *Set) markUnloaded(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unloaded[key] = struct{}{}
}

// Unloaded lists every ggen-generated type a loaded type references whose own
// package was not loaded, as "import/path.Name". Each is lowered as an
// external type with no fields; loading its package instead gives the full
// shape.
func (s *Set) Unloaded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.unloaded))
	for k := range s.unloaded {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// enumValues is the closed set of key, a "path.Name": every constant declared
// in the type's own package, then the exported constants other loaded
// packages declare for it, each in declaration order. Returns nil when the
// type has no constants.
func (s *Set) enumValues(key, ownPkg string) *Enum {
	var own, others []model.Const
	for _, c := range s.consts[key] {
		switch {
		case c.Pkg == ownPkg:
			own = append(own, c)
		case s.loadedPath(c.Pkg) && token.IsExported(c.Name):
			others = append(others, c)
		}
	}
	slices.SortStableFunc(others, func(a, b model.Const) int { return strings.Compare(a.Pkg, b.Pkg) })
	all := append(own, others...)
	if len(all) == 0 {
		return nil
	}
	e := &Enum{}
	for _, c := range all {
		v := EnumValue{Name: c.Name, Value: c.Value}
		if strings.HasPrefix(c.Value, `"`) {
			if err := json.Unmarshal([]byte(c.Value), &v.Text); err != nil {
				continue // the parser writes JSON; anything else is not a value
			}
		}
		e.Values = append(e.Values, v)
	}
	return e
}

func (s *Set) loadedPath(path string) bool {
	p, ok := s.pkgs[path]
	return ok && p.Loaded
}

// Package returns the loaded package with the import path, or nil.
func (s *Set) Package(path string) *Package {
	if p, ok := s.pkgs[path]; ok && p.Loaded {
		return p
	}
	return nil
}

// Type returns the generated type or enum type pkgPath.name. A type no
// pattern loaded comes back as a placeholder whose shapes carry the error,
// which File.Place surfaces and Write fails on.
func (s *Set) Type(pkgPath, name string) *TypeDecl {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.decls[pkgPath+"."+name]; ok {
		return d
	}
	return &TypeDecl{Pkg: &Package{Path: pkgPath, Name: pkgPath[strings.LastIndexByte(pkgPath, '/')+1:]}, Name: name, set: s, missing: true}
}

// Types returns every generated type of every loaded package.
func (s *Set) Types() []*TypeDecl {
	var out []*TypeDecl
	for _, p := range s.Packages {
		out = append(out, p.Types...)
	}
	return out
}

// Enums returns the enum types the loaded types reference, in first-reference
// order. A generated type that is itself an enum is in Types, not here.
func (s *Set) Enums() []*TypeDecl { return s.enums }

// Has reports whether the doc comment carries the directive, e.g.
// t.Has("schema:in") for a `//schema:in` line.
func (t *TypeDecl) Has(directive string) bool {
	return slices.Contains(t.Directives, directive)
}

// enumDecl returns the enum type for a named string or integer type with
// constants, creating it on first use; nil when the type has no constants.
func (s *Set) enumDecl(n *types.Named, fset *token.FileSet) *TypeDecl {
	obj := n.Obj()
	if obj.Pkg() == nil {
		return nil
	}
	key := obj.Pkg().Path() + "." + obj.Name()
	pkg := s.pkg(obj.Pkg()) // before the lock: pkg takes it too
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.decls[key]; ok {
		return d
	}
	// Constants of a package no pattern matched are not a closed set: an
	// imported flag or bitmask type (fs.FileMode) would reject every real
	// value. A script opts in by loading the package.
	if !pkg.Loaded {
		return nil
	}
	e := s.enumValues(key, obj.Pkg().Path())
	if e == nil {
		return nil
	}
	d := &TypeDecl{
		Pkg:   pkg,
		Name:  obj.Name(),
		Enum:  e,
		set:   s,
		fset:  fset,
		named: n,
	}
	e.Decl = d
	if d.Pkg.Loaded {
		d.File = fset.Position(obj.Pos()).Filename
	}
	s.decls[key] = d
	s.enums = append(s.enums, d)
	return d
}

// markRecursive flags every type that reaches itself through references.
func markRecursive(refs map[*TypeDecl][]*TypeDecl) {
	for d := range refs {
		seen := map[*TypeDecl]struct{}{}
		stack := slices.Clone(refs[d])
		for len(stack) > 0 {
			r := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if r == d {
				d.recursive = true
				break
			}
			if _, ok := seen[r]; ok {
				continue
			}
			seen[r] = struct{}{}
			stack = append(stack, refs[r]...)
		}
	}
}
