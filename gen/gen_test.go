package gen

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	apiPath   = "github.com/sirkostya009/ggen/gen/testdata/api"
	otherPath = "github.com/sirkostya009/ggen/gen/testdata/other"
)

func load(t *testing.T) *Set {
	t.Helper()
	s, err := Load("./testdata/api", "./testdata/other")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func field(t *testing.T, s *Shape, name string) *Field {
	t.Helper()
	f := s.Field(name)
	if f == nil {
		t.Fatalf("%s has no field %s", s.Decl.Name, name)
	}
	return f
}

func ops(rules []Rule) []string {
	var out []string
	for _, r := range rules {
		out = append(out, r.Op.String()+strings.Join(r.Args, "|"))
	}
	return out
}

func enumValues(e *Enum) []string {
	var out []string
	for _, v := range e.Values {
		out = append(out, v.Value)
	}
	return out
}

func TestLoad(t *testing.T) {
	t.Parallel()
	s := load(t)
	if len(s.Packages) != 2 || s.Package(apiPath) == nil || s.Package("time") != nil {
		t.Fatalf("packages: %v", s.Packages)
	}
	var names []string
	for _, d := range s.Package(apiPath).Types {
		names = append(names, d.Name)
	}
	if want := []string{"CreateUser", "User", "Edges", "Tags", "Kind", "Tree", "Awkward", "Collides", "Address", "Leaf"}; !slices.Equal(names, want) {
		t.Errorf("api types = %v, want %v", names, want)
	}
	cu := s.Type(apiPath, "CreateUser")
	if !cu.Annotated || !cu.Has("schema:in") || cu.Has("schema:out") || cu.Doc != "CreateUser is a request." || filepath.Base(cu.File) != "api.go" {
		t.Errorf("CreateUser: annotated=%v directives=%v doc=%q file=%q", cu.Annotated, cu.Directives, cu.Doc, cu.File)
	}
	if s.Type(apiPath, "Address").Annotated {
		t.Error("Address is reached, not annotated")
	}
	if !s.Type(apiPath, "User").Out().Recursive || s.Type(apiPath, "CreateUser").In().Recursive {
		t.Error("recursion")
	}
	var enums []string
	for _, e := range s.Enums() {
		enums = append(enums, e.Pkg.Path+"."+e.Name)
	}
	slices.Sort(enums)
	want := []string{apiPath + ".Plan", apiPath + ".Role", otherPath + ".Currency"}
	if !slices.Equal(enums, want) {
		t.Errorf("enums = %v, want %v", enums, want)
	}
	if role := s.Type(apiPath, "Role"); role == nil || role.Enum == nil {
		t.Error("Set.Type finds enum types")
	}
}

func TestInputShapes(t *testing.T) {
	t.Parallel()
	s := load(t)
	in := s.Type(apiPath, "CreateUser").In()
	name := field(t, in, "name")
	if name.Optional || name.Doc != "Name is trimmed first." || !slices.Equal(ops(name.Type.Rules), []string{"trim", "minlen1", "maxlen64"}) {
		t.Errorf("name: optional=%v doc=%q rules=%v", name.Optional, name.Doc, ops(name.Type.Rules))
	}
	age := field(t, in, "age").Type
	if !age.Nullable || age.GoType != "*int" || len(age.Rules) != 2 || age.Rules[1].Func != "IsAdult" || age.Rules[1].Msg != "must be an adult" {
		t.Errorf("age: %+v", age)
	}
	if addr := field(t, in, "address"); !addr.Optional || addr.Type.Ref != s.Type(apiPath, "Address") {
		t.Errorf("address: %+v", addr)
	}

	e := s.Type(apiPath, "Edges").In()
	if e.Unknown != Collect || e.Rest == nil || e.Rest.Go != String {
		t.Errorf("edges unknown=%v rest=%+v", e.Unknown, e.Rest)
	}
	cases := []struct {
		field    string
		nullable bool
		check    func(*Type) bool
	}{
		{"list", false, func(t *Type) bool { return slices.Equal(ops(t.Elem.Rules), []string{"notempty"}) }},
		{"max", true, nil},
		{"plist", true, func(t *Type) bool { return slices.Equal(ops(t.Rules), []string{"minlen1"}) }},
		{"dict", true, func(t *Type) bool {
			return slices.Equal(ops(t.Key.Rules), []string{"minlen2"}) && slices.Equal(ops(t.Elem.Rules), []string{"gt0"})
		}},
		{"quoted", false, func(t *Type) bool { return t.Go == Int8 && t.Wire == WireString && t.Format == FormatIntString }},
		{"unix", false, func(t *Type) bool { return t.Wire == WireInteger && t.Format == FormatUnixMilli }},
		{"date", false, func(t *Type) bool { return t.Format == "time:2006-01-02" }},
		{"hex", true, func(t *Type) bool { return t.Go == Bytes && t.Format == FormatHex }},
		{"fixed", false, func(t *Type) bool { return t.Len == 4 && t.Format == FormatBase64 }},
		{"ip", true, func(t *Type) bool { return t.Empty && t.Format == FormatIP }},
		{"addr", false, func(t *Type) bool { return t.Empty && t.Format == FormatIPZone }},
		{"ns", true, func(t *Type) bool { return t.Go == String && t.GoType == "sql.NullString" }},
		{"raw", true, func(t *Type) bool { return t.Wire == WireAny && t.GoType == "json.RawMessage" }},
		{"lower", false, func(t *Type) bool {
			return t.Enum == nil && slices.Equal(ops(t.Rules), []string{"tolower", "oneofx|y"})
		}},
		{"nz", true, nil},
		{"conv", true, func(t *Type) bool { return len(t.OneOf) == 1 && t.OneOf[0].Go == String }},
		{"tags", false, func(t *Type) bool { return t.Ref == s.Type(apiPath, "Tags") }},
	}
	for _, c := range cases {
		ty := field(t, e, c.field).Type
		if ty.Nullable != c.nullable || (c.check != nil && !c.check(ty)) {
			t.Errorf("%s: nullable=%v %+v", c.field, ty.Nullable, ty)
		}
	}
	if s.Type(apiPath, "Edges").Out().Unknown != Collect {
		t.Error("output keeps the catch-all")
	}
}

func TestOutputShapes(t *testing.T) {
	t.Parallel()
	s := load(t)
	out := s.Type(apiPath, "User").Out()
	role := field(t, out, "role")
	if role.Optional || role.Type.Ref != s.Type(apiPath, "Role") || !slices.Equal(enumValues(role.Type.Enum), []string{`"admin"`, `"member"`}) ||
		!role.Type.Enum.Zero {
		t.Errorf("role: %+v %+v", role, role.Type.Enum)
	}
	if st := field(t, out, "status").Type; st.Enum == nil || !st.Enum.Zero || st.Enum.Decl != nil || len(st.Rules) != 0 {
		t.Errorf("status: %+v", st)
	}
	if plan := field(t, out, "plan"); !plan.Optional || plan.Type.Enum.Zero {
		t.Errorf("plan (omitzero): %+v %+v", plan, plan.Type.Enum)
	}
	if tags := field(t, out, "tags"); !tags.Optional || tags.Type.Nullable {
		t.Errorf("tags (omitempty): %+v", tags.Type)
	}
	if m := field(t, out, "manager"); !m.Optional || m.Type.Nullable {
		t.Errorf("manager (omitempty): %+v", m.Type)
	}
	if f := field(t, out, "friends").Type; !f.Nullable || !f.Elem.Nullable {
		t.Errorf("friends: %+v", f)
	}
	// net/http is not loaded, so its constants are not a closed set: an
	// imported flag type would reject every real value.
	if st := field(t, out, "state").Type; st.Enum != nil || st.Ref != nil || st.Wire != WireInteger {
		t.Errorf("state: net/http is not loaded, so no enum: %+v", st)
	}
	if lv := field(t, out, "level").Type; !lv.External || lv.Wire != WireAny || lv.Go != Int || lv.Pkg.Path != "log/slog" {
		t.Errorf("level: %+v", lv)
	}
	if p := field(t, out, "point").Type; !p.External || p.Go != Struct || p.Wire != WireObject {
		t.Errorf("point: %+v", p)
	}
	if cur := field(t, s.Type(otherPath, "Money").Out(), "currency").Type; !slices.Equal(enumValues(cur.Enum), []string{`"USD"`, `"EUR"`}) {
		t.Errorf("currency consts across packages: %v", enumValues(cur.Enum))
	}

	e := s.Type(apiPath, "Edges").Out()
	if l := field(t, e, "list").Type; !l.Nullable || len(l.Rules) != 0 || len(l.Elem.Rules) != 0 {
		t.Errorf("output list: %+v", l)
	}
	if ip := field(t, e, "ip").Type; ip.Nullable || !ip.Empty {
		t.Errorf("output ip: nil encodes as \"\": %+v", ip)
	}
	if lw := field(t, e, "lower").Type; lw.Enum == nil || !lw.Enum.Zero || len(lw.Rules) != 0 {
		t.Errorf("output lower: %+v", lw)
	}
	if nz := field(t, e, "nz").Type; nz.Nullable || nz.OneOf != nil {
		t.Errorf("output nz: %+v", nz)
	}
}

func TestStrict(t *testing.T) {
	t.Parallel()
	s := load(t)
	u := s.Type(apiPath, "User").Out().Strict()
	for _, name := range []string{"role", "status", "friends"} {
		ty := field(t, u, name).Type
		if ty.Nullable || (ty.Enum != nil && ty.Enum.Zero) || (ty.Elem != nil && ty.Elem.Nullable) {
			t.Errorf("%s still widened: %+v", name, ty)
		}
	}
	if !field(t, u, "tags").Optional {
		t.Error("Strict must not touch Optional")
	}
	one := s.Type(apiPath, "User").Out().StrictFields("role")
	if field(t, one, "role").Type.Enum.Zero || !field(t, one, "status").Type.Enum.Zero {
		t.Error("StrictFields")
	}
	in := s.Type(apiPath, "CreateUser").In().Strict()
	if !field(t, in, "age").Type.Nullable {
		t.Error("Strict is a no-op on input")
	}
	if a, b := s.Type(apiPath, "User").Out(), s.Type(apiPath, "User").Out(); a == b || !field(t, b, "role").Type.Enum.Zero {
		t.Error("Out builds a fresh shape")
	}
}

// refs renders one line per placed shape: its name, mode, and how each
// reference resolves from this file.
type refs struct{ fail bool }

func (refs) Begin(ctx *Context) error {
	ctx.W.Import("// " + ctx.File.Path)
	return nil
}

func (r refs) Decl(ctx *Context, p Placed) error {
	if r.fail {
		return errors.New("boom")
	}
	var parts []string
	seen := map[*TypeDecl]struct{}{}
	walkShapeRefs(p.Shape, func(d *TypeDecl) {
		if _, ok := seen[d]; ok {
			return
		}
		seen[d] = struct{}{}
		pl, ok := ctx.Lookup(d, p.Shape.Mode)
		switch {
		case !ok:
			parts = append(parts, d.Name+"=inline")
		case ctx.FileOf(d, p.Shape.Mode) == ctx.File:
			ref := pl.Name
			if ctx.Later(d, p.Shape.Mode) {
				ref += "(later)"
			}
			parts = append(parts, ref)
		default:
			parts = append(parts, ctx.Rel(ctx.FileOf(d, p.Shape.Mode))+"#"+pl.Name)
		}
	})
	ctx.W.Printf("%s %s [%s]\n", p.Name, p.Shape.Mode, strings.Join(parts, " "))
	return nil
}

func (refs) End(*Context) error { return nil }

func TestWrite(t *testing.T) {
	t.Parallel()
	s := load(t)
	dir := t.TempDir()
	out := NewOutput(dir)
	em := refs{}
	api := out.File("web/api.ts", em)
	// Placed before its dependencies: Write reorders.
	api.Place(s.Type(apiPath, "User").Out(), "User")
	api.Place(s.Type(apiPath, "Address").Out(), "Address")
	out.File("shared/money.ts", em).Place(s.Type(otherPath, "Money").Out(), "Money")
	req := out.File("web/requests.ts", em)
	req.Place(s.Type(apiPath, "CreateUser").In(), "CreateUser")
	req.Place(s.Type(apiPath, "Address").In(), "AddressInput")
	if out.File("web/api.ts", em) != api {
		t.Fatal("File returns the existing file for a path")
	}
	if err := out.Write(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "web/api.ts"))
	want := "// web/api.ts\n\nAddress output []\nUser output [Role=inline Plan=inline ../shared/money#Money Address User(later)]\n"
	if string(got) != want {
		t.Errorf("api.ts:\n%s\nwant:\n%s", got, want)
	}
	got, _ = os.ReadFile(filepath.Join(dir, "web/requests.ts"))
	want = "// web/requests.ts\n\nAddressInput input []\nCreateUser input [AddressInput]\n"
	if string(got) != want {
		t.Errorf("requests.ts:\n%s\nwant:\n%s", got, want)
	}
}

func TestWriteErrors(t *testing.T) {
	t.Parallel()
	s := load(t)
	dir := t.TempDir()
	out := NewOutput(dir)
	f := out.File("a.ts", refs{})
	f.Place(s.Type(apiPath, "CreateUser").In(), "CreateUser")
	f.Place(s.Type(apiPath, "CreateUser").In(), "Again")
	f.Place(s.Type(apiPath, "User").Out(), "CreateUser")
	out.File("a.ts", &refs{})
	err := out.Write()
	for _, want := range []string{
		"a.ts: api.CreateUser (input) is already placed in a.ts as CreateUser",
		"a.ts: name CreateUser is used by both api.CreateUser (input) and api.User (output)",
		"a.ts: requested with two different emitters",
		"a.ts: CreateUser (input) references api.Address (input), which is not placed in this output",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}

	out = NewOutput(dir)
	out.File("ok.ts", refs{}).Place(s.Type(apiPath, "Address").Out(), "Address")
	out.File("bad.ts", refs{fail: true}).Place(s.Type(apiPath, "Address").In(), "Address")
	if err := out.Write(); err == nil || !strings.Contains(err.Error(), "bad.ts: Address: boom") {
		t.Errorf("render error: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a failed Write wrote %d files", len(entries))
	}
}

func TestReport(t *testing.T) {
	t.Parallel()
	s := load(t)
	out := NewOutput(t.TempDir())
	in := s.Type(apiPath, "CreateUser").In()
	out.File("a.ts", reporter{}).Place(in, "CreateUser").Place(s.Type(apiPath, "Address").In(), "Address")
	if err := out.Write(); err != nil {
		t.Fatal(err)
	}
	want := []string{"api.CreateUser.age: func: no twin"}
	var got []string
	for _, i := range out.Report().Issues {
		got = append(got, i.String())
	}
	if !slices.Equal(got, want) {
		t.Errorf("report = %v, want %v", got, want)
	}
}

type reporter struct{}

func (reporter) Begin(*Context) error { return nil }
func (reporter) End(*Context) error   { return nil }
func (reporter) Decl(ctx *Context, p Placed) error {
	for _, f := range p.Shape.Fields {
		for _, r := range f.Type.Rules {
			if r.Op == Func {
				ctx.Report.Unsupported(f, r, "no twin")
			}
		}
	}
	return nil
}

// TestAnnotatedEnum pins a generated alias that is also a closed set: it stays
// in Types, keeps its constants, and is not repeated in Enums.
func TestAnnotatedEnum(t *testing.T) {
	t.Parallel()
	s := load(t)
	d := s.Type(apiPath, "Kind")
	if d.Enum == nil || !slices.Equal(enumValues(d.Enum), []string{`"one"`, `"two"`}) {
		t.Fatalf("Kind enum = %v", d.Enum)
	}
	if !d.Annotated || !slices.Contains(s.Package(apiPath).Types, d) {
		t.Error("Kind is a generated type of its package")
	}
	if slices.Contains(s.Enums(), d) {
		t.Error("Kind is in Types, so it must not be in Enums too")
	}
	in := d.In().Alias
	if in == nil || in.Enum == nil || in.Enum.Decl != d {
		t.Fatalf("input alias = %#v", in)
	}
	if out := d.Out().Alias; out.Enum == nil || !out.Enum.Zero {
		t.Errorf("output alias admits the Go zero value: %#v", out.Enum)
	}
}

// TestCycleAcrossFiles pins that a cycle must live in one file, and that the
// shapes on it are reported as cyclic rather than merely recursive.
func TestCycleAcrossFiles(t *testing.T) {
	t.Parallel()
	s := load(t)
	out := NewOutput(t.TempDir())
	out.File("a.ts", refs{}).Place(s.Type(apiPath, "Tree").Out(), "Tree")
	out.File("b.ts", refs{}).Place(s.Type(apiPath, "Leaf").Out(), "Leaf")
	err := out.Write()
	if err == nil || !strings.Contains(err.Error(), "Tree") || !strings.Contains(err.Error(), "b.ts") {
		t.Fatalf("cycle across files: %v", err)
	}

	out = NewOutput(t.TempDir())
	var cyclic, selfOnly bool
	f := out.File("a.ts", cycles{&cyclic, &selfOnly})
	f.Place(s.Type(apiPath, "Tree").Out(), "Tree")
	f.Place(s.Type(apiPath, "Leaf").Out(), "Leaf")
	f.Place(s.Type(apiPath, "User").Out(), "User")
	f.Place(s.Type(apiPath, "Address").Out(), "Address")
	f.Place(s.Type(otherPath, "Money").Out(), "Money")
	if err := out.Write(); err != nil {
		t.Fatal(err)
	}
	if !cyclic {
		t.Error("Tree and Leaf are on a cycle with each other")
	}
	if selfOnly {
		t.Error("User references only itself, so it is recursive but not cyclic")
	}
}

// cycles records what Context.Cyclic answers for Tree and for User.
type cycles struct{ cyclic, selfOnly *bool }

func (cycles) Begin(*Context) error { return nil }
func (cycles) End(*Context) error   { return nil }
func (c cycles) Decl(ctx *Context, p Placed) error {
	switch p.Name {
	case "Tree":
		*c.cyclic = ctx.Cyclic(p.Shape.Decl, p.Shape.Mode)
	case "User":
		*c.selfOnly = ctx.Cyclic(p.Shape.Decl, p.Shape.Mode)
	}
	return nil
}

// TestOutputRejects pins what Output refuses before writing anything.
func TestOutputRejects(t *testing.T) {
	t.Parallel()
	s := load(t)
	dir := t.TempDir()
	for _, c := range []struct{ name, path, want string }{
		{"escaping path", "../escape.ts", "leaves the output directory"},
		{"absolute path", "/etc/escape.ts", "leaves the output directory"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			out := NewOutput(dir)
			out.File(c.path, refs{}).Place(s.Type(apiPath, "Address").Out(), "Address")
			if err := out.Write(); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: %v", c.path, err)
			}
		})
	}
	t.Run("nil emitter", func(t *testing.T) {
		t.Parallel()
		out := NewOutput(dir)
		out.File("a.ts", nil).Place(s.Type(apiPath, "Address").Out(), "Address")
		if err := out.Write(); err == nil || !strings.Contains(err.Error(), "emitter") {
			t.Errorf("nil emitter: %v", err)
		}
	})
	t.Run("type no pattern loaded", func(t *testing.T) {
		t.Parallel()
		d := s.Type(apiPath, "Nope")
		if d == nil {
			t.Fatal("Type returns a placeholder, never nil")
		}
		out := NewOutput(dir)
		out.File("a.ts", refs{}).Place(d.Out(), "Nope")
		if err := out.Write(); err == nil || !strings.Contains(err.Error(), "Nope") {
			t.Errorf("missing type: %v", err)
		}
	})
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a rejected Write wrote %d files", len(entries))
	}
}
