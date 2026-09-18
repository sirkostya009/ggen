package gen

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
)

// Output is a root directory and the files a script creates in it. Nothing is
// written until Write.
type Output struct {
	dir    string
	files  []*File
	byPath map[string]*File
	placed map[placeKey]placement
	cyclic map[placeKey]struct{} // placements on a cycle with another placement
	errs   []error
	report Report
}

type placeKey struct {
	decl *TypeDecl
	mode Mode
}

type placement struct {
	file *File
	name string
}

// File is one output file: a path, the emitter that renders it, and the
// shapes placed in it.
type File struct {
	Path string // relative to the Output directory, slash-separated

	out     *Output
	emitter Emitter
	placed  []Placed
}

// Placed is a shape placed in a file under a name.
type Placed struct {
	Shape *Shape
	Name  string
}

// Emitter renders one file. Decl is called once per placed shape, in
// dependency order within the file.
type Emitter interface {
	Begin(ctx *Context) error
	Decl(ctx *Context, p Placed) error
	End(ctx *Context) error
}

// NewOutput starts an output rooted at dir.
func NewOutput(dir string) *Output {
	return &Output{
		dir:    dir,
		byPath: map[string]*File{},
		placed: map[placeKey]placement{},
	}
}

// File returns the file at path, creating it with e. Asking for an existing
// path returns that file; a different emitter for it is an error at Write.
func (o *Output) File(p string, e Emitter) *File {
	p = path.Clean(filepath.ToSlash(p))
	if e == nil {
		o.errs = append(o.errs, fmt.Errorf("%s: nil emitter", p))
	}
	if path.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../") || filepath.IsAbs(filepath.FromSlash(p)) {
		o.errs = append(o.errs, fmt.Errorf("%s: path leaves the output directory", p))
	}
	if f, ok := o.byPath[p]; ok {
		if e == nil || f.emitter == nil {
			return f
		}
		if reflect.TypeOf(e) != reflect.TypeOf(f.emitter) || reflect.TypeOf(e).Comparable() && f.emitter != e {
			o.errs = append(o.errs, fmt.Errorf("%s: requested with two different emitters", p))
		}
		return f
	}
	f := &File{Path: p, out: o, emitter: e}
	o.byPath[p] = f
	o.files = append(o.files, f)
	return f
}

// Emitter returns the emitter that renders the file.
func (f *File) Emitter() Emitter { return f.emitter }

// Placed returns the shapes placed in the file, in placement order.
func (f *File) Placed() []Placed { return f.placed }

// Place puts a shape into the file under name. A type may be placed once per
// mode in an Output, and a name once per file; violations fail Write.
func (f *File) Place(s *Shape, name string) *File {
	o := f.out
	for _, err := range s.errs {
		o.errs = append(o.errs, fmt.Errorf("%s: %w", f.Path, err))
	}
	s.errs = nil
	key := placeKey{s.Decl, s.Mode}
	if prev, ok := o.placed[key]; ok {
		o.errs = append(o.errs, fmt.Errorf("%s: %s (%s) is already placed in %s as %s", f.Path, declName(s.Decl), s.Mode, prev.file.Path, prev.name))
		return f
	}
	for _, p := range f.placed {
		if p.Name == name {
			o.errs = append(o.errs, fmt.Errorf("%s: name %s is used by both %s (%s) and %s (%s)", f.Path, name, declName(p.Shape.Decl), p.Shape.Mode, declName(s.Decl), s.Mode))
			return f
		}
	}
	o.placed[key] = placement{f, name}
	f.placed = append(f.placed, Placed{s, name})
	return f
}

// Report is what emitters could not express, collected over one Write.
func (o *Output) Report() *Report { return &o.report }

// Report collects issues emitters raise.
type Report struct {
	Issues []Issue
}

// Issue is one thing an emitter could not express.
type Issue struct {
	Where string // "api.CreateUser (input).age"
	Msg   string
}

func (i Issue) String() string { return i.Where + ": " + i.Msg }

// Add records an issue at where.
func (r *Report) Add(where, msg string) {
	r.Issues = append(r.Issues, Issue{where, msg})
}

// Unsupported records that rule r on field f has no twin in the target.
func (r *Report) Unsupported(f *Field, rule Rule, msg string) {
	r.Add(declName(f.Shape.Decl)+"."+f.JSONName, rule.Op.String()+": "+msg)
}

func declName(d *TypeDecl) string { return d.Pkg.Name + "." + d.Name }

// shapeName is a shape in a report: "api.Edges (output)", so the same type
// placed in both modes raises two distinguishable issues.
func shapeName(s *Shape) string { return declName(s.Decl) + " (" + s.Mode.String() + ")" }

// Write validates every placement, renders every file, and writes them all.
// Any error leaves the directory untouched.
func (o *Output) Write() error {
	errs := slices.Clone(o.errs)
	for _, f := range o.files {
		errs = append(errs, f.checkRefs()...)
	}
	errs = append(errs, o.checkCycles()...)
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	o.report = Report{}
	rendered := make([][]byte, len(o.files))
	for i, f := range o.files {
		body, err := f.render()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f.Path, err))
			continue
		}
		rendered[i] = body
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	// Stage every file, then rename: a failure halfway leaves no half-written
	// output behind.
	type staged struct{ tmp, dst string }
	var writes []staged
	cleanup := func() {
		for _, w := range writes {
			os.Remove(w.tmp)
		}
	}
	for i, f := range o.files {
		dst := filepath.Join(o.dir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			cleanup()
			return err
		}
		if old, err := os.ReadFile(dst); err == nil && bytes.Equal(old, rendered[i]) {
			continue
		}
		tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*")
		if err != nil {
			cleanup()
			return err
		}
		_, err = tmp.Write(rendered[i])
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Chmod(tmp.Name(), 0o644)
		}
		if err != nil {
			os.Remove(tmp.Name())
			cleanup()
			return err
		}
		writes = append(writes, staged{tmp.Name(), dst})
	}
	for _, w := range writes {
		if err := os.Rename(w.tmp, w.dst); err != nil {
			cleanup()
			return err
		}
	}
	return nil
}

// checkRefs reports every reference from this file's shapes to a generated
// type not placed in the same mode. Enum types are soft references.
func (f *File) checkRefs() []error {
	var errs []error
	seen := map[placeKey]struct{}{}
	for _, p := range f.placed {
		walkShapeRefs(p.Shape, func(r *TypeDecl) {
			key := placeKey{r, p.Shape.Mode}
			if r.Enum != nil && r.named != nil || r == p.Shape.Decl {
				return
			}
			if _, dup := seen[key]; dup {
				return
			}
			pl, ok := f.out.placed[key]
			if !ok {
				seen[key] = struct{}{}
				errs = append(errs, fmt.Errorf("%s: %s (%s) references %s (%s), which is not placed in this output",
					f.Path, p.Name, p.Shape.Mode, declName(r), p.Shape.Mode))
				return
			}
			if reflect.TypeOf(pl.file.emitter) != reflect.TypeOf(f.emitter) {
				seen[key] = struct{}{}
				errs = append(errs, fmt.Errorf("%s: %s (%s) references %s (%s), which is placed in %s, rendered by a different emitter",
					f.Path, p.Name, p.Shape.Mode, declName(r), p.Shape.Mode, pl.file.Path))
			}
		})
	}
	return errs
}

// Cyclic reports whether the placement of ref in mode sits on a reference
// cycle with another placed type, as opposed to referencing only itself. A
// target may need a different shape for the two: TypeScript infers a
// self-recursive schema on its own but not a mutual one.
func (c *Context) Cyclic(ref *TypeDecl, mode Mode) bool {
	_, ok := c.File.out.cyclic[placeKey{ref, mode}]
	return ok
}

// checkCycles reports a reference cycle whose members are not all in one
// file. Splitting one across files makes the generated modules import each
// other, which most targets cannot load, and Context.Later only marks a
// forward reference within a file.
func (o *Output) checkCycles() []error {
	type node struct {
		file *File
		p    Placed
	}
	var keys []placeKey
	nodes := map[placeKey]node{}
	for _, f := range o.files {
		for _, p := range f.placed {
			k := placeKey{p.Shape.Decl, p.Shape.Mode}
			if _, dup := nodes[k]; dup {
				continue
			}
			nodes[k] = node{f, p}
			keys = append(keys, k)
		}
	}
	// Transitive reachability over references that stay inside this output.
	reach := map[placeKey]map[placeKey]struct{}{}
	for _, k := range keys {
		seen := map[placeKey]struct{}{}
		stack := []placeKey{k}
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			walkShapeRefs(nodes[cur].p.Shape, func(r *TypeDecl) {
				d := placeKey{r, k.mode}
				if _, ok := nodes[d]; !ok {
					return
				}
				if _, done := seen[d]; done {
					return
				}
				seen[d] = struct{}{}
				stack = append(stack, d)
			})
		}
		reach[k] = seen
	}
	var errs []error
	grouped := map[placeKey]struct{}{}
	for _, k := range keys {
		if _, done := grouped[k]; done {
			continue
		}
		if _, self := reach[k][k]; !self {
			continue // not on a cycle
		}
		cycle := []placeKey{k}
		for _, j := range keys {
			if j == k {
				continue
			}
			if _, there := reach[k][j]; !there {
				continue
			}
			if _, back := reach[j][k]; back {
				cycle = append(cycle, j)
			}
		}
		var files []string
		var names []string
		for _, c := range cycle {
			grouped[c] = struct{}{}
			if len(cycle) > 1 {
				if o.cyclic == nil {
					o.cyclic = map[placeKey]struct{}{}
				}
				o.cyclic[c] = struct{}{}
			}
			names = append(names, nodes[c].p.Name)
			if !slices.Contains(files, nodes[c].file.Path) {
				files = append(files, nodes[c].file.Path)
			}
		}
		if len(files) < 2 {
			continue
		}
		slices.Sort(files)
		errs = append(errs, fmt.Errorf("%s (%s) reference each other across %s: a reference cycle must be placed in one file",
			strings.Join(names, ", "), k.mode, strings.Join(files, ", ")))
	}
	slices.SortFunc(errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
	return errs
}

func (f *File) render() ([]byte, error) {
	ctx := &Context{File: f, W: &Writer{}, Report: &f.out.report, emitted: map[placeKey]struct{}{}}
	if err := f.emitter.Begin(ctx); err != nil {
		return nil, err
	}
	for _, p := range f.ordered() {
		if err := f.emitter.Decl(ctx, p); err != nil {
			return nil, fmt.Errorf("%s: %w", p.Name, err)
		}
		ctx.emitted[placeKey{p.Shape.Decl, p.Shape.Mode}] = struct{}{}
	}
	if err := f.emitter.End(ctx); err != nil {
		return nil, err
	}
	return ctx.W.bytes(), nil
}

// ordered sorts the file's placements so a shape comes after the shapes of
// this file it references, keeping placement order otherwise. Members of a
// reference cycle keep their relative placement order.
func (f *File) ordered() []Placed {
	index := map[placeKey]int{}
	for i, p := range f.placed {
		index[placeKey{p.Shape.Decl, p.Shape.Mode}] = i
	}
	deps := make([][]int, len(f.placed))
	for i, p := range f.placed {
		walkShapeRefs(p.Shape, func(r *TypeDecl) {
			if j, ok := index[placeKey{r, p.Shape.Mode}]; ok && j != i && !slices.Contains(deps[i], j) {
				deps[i] = append(deps[i], j)
			}
		})
	}
	const (
		unvisited = iota
		visiting
		done
	)
	state := make([]uint8, len(f.placed))
	out := make([]Placed, 0, len(f.placed))
	var visit func(i int)
	visit = func(i int) {
		if state[i] != unvisited {
			return
		}
		state[i] = visiting
		for _, j := range deps[i] {
			visit(j)
		}
		state[i] = done
		out = append(out, f.placed[i])
	}
	for i := range f.placed {
		visit(i)
	}
	return out
}

// Context is an emitter's view of the file it renders.
type Context struct {
	File   *File
	W      *Writer
	Report *Report

	emitted map[placeKey]struct{}
}

// Resolve returns where a referenced type was placed in mode. Write has
// already failed for a missing generated type; for an unplaced enum type it
// returns the zero Placed, see Lookup.
func (c *Context) Resolve(ref *TypeDecl, mode Mode) Placed {
	p, _ := c.Lookup(ref, mode)
	return p
}

// Lookup returns where a referenced type was placed in mode, and whether it
// was. An emitter inlines an unplaced enum type's values instead.
func (c *Context) Lookup(ref *TypeDecl, mode Mode) (Placed, bool) {
	pl, ok := c.File.out.placed[placeKey{ref, mode}]
	if !ok {
		return Placed{}, false
	}
	for _, p := range pl.file.placed {
		if p.Shape.Decl == ref && p.Shape.Mode == mode {
			return p, true
		}
	}
	return Placed{}, false
}

// FileOf returns the file a referenced type was placed in, or nil.
func (c *Context) FileOf(ref *TypeDecl, mode Mode) *File {
	if pl, ok := c.File.out.placed[placeKey{ref, mode}]; ok {
		return pl.file
	}
	return nil
}

// Later reports whether ref is placed in this file but not yet emitted: a
// reference to it from the current declaration points forward, as in a
// recursive type.
func (c *Context) Later(ref *TypeDecl, mode Mode) bool {
	if c.FileOf(ref, mode) != c.File {
		return false
	}
	_, done := c.emitted[placeKey{ref, mode}]
	return !done
}

// Rel returns the slash-separated path from this file's directory to another
// file, without its extension, starting with "./" or "../":
// "./users" from "api/index.ts" to "api/users.ts".
func (c *Context) Rel(to *File) string {
	from := path.Dir(c.File.Path)
	target := strings.TrimSuffix(to.Path, path.Ext(to.Path))
	rel, err := filepath.Rel(filepath.FromSlash(from), filepath.FromSlash(target))
	if err != nil {
		return target
	}
	rel = filepath.ToSlash(rel)
	if !strings.HasPrefix(rel, "../") {
		rel = "./" + rel
	}
	return rel
}

// Writer is a file's text: header lines, then import lines (both
// deduplicated, in first-use order), then the body, each group separated by a
// blank line.
type Writer struct {
	header  []string
	imports []string
	body    bytes.Buffer
}

// Header adds a line at the top of the file once.
func (w *Writer) Header(line string) {
	if !slices.Contains(w.header, line) {
		w.header = append(w.header, line)
	}
}

// Printf appends to the body.
func (w *Writer) Printf(format string, args ...any) {
	fmt.Fprintf(&w.body, format, args...)
}

// WriteString appends to the body.
func (w *Writer) WriteString(s string) (int, error) {
	return w.body.WriteString(s)
}

// Import adds a line above the body once.
func (w *Writer) Import(line string) {
	if !slices.Contains(w.imports, line) {
		w.imports = append(w.imports, line)
	}
}

func (w *Writer) bytes() []byte {
	var b bytes.Buffer
	for _, group := range [...][]string{w.header, w.imports} {
		if len(group) == 0 {
			continue
		}
		for _, l := range group {
			b.WriteString(l)
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	b.Write(w.body.Bytes())
	return b.Bytes()
}
