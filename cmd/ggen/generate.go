package main

import (
	"bytes"
	"fmt"
	"go/format"
	"io"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/sirkostya009/ggen/gen/model"
)

// smallPool recycles the bytes.Buffer values that renderers use as scratch
// space. bytes.Buffer.String copies, so reusing a buffer after Put cannot
// corrupt a string an earlier caller already got back.
var smallPool = sync.Pool{New: func() any {
	b := new(bytes.Buffer)
	b.Grow(512)
	return b
}}

func getSmall() *bytes.Buffer {
	b := smallPool.Get().(*bytes.Buffer)
	b.Reset()
	return b
}

func putSmall(b *bytes.Buffer) { smallPool.Put(b) }

// cyclicTypes marks generated structs that sit on a cycle in the type graph,
// that is, structs that refer to themselves directly or through other types.
// Their decode methods get a depth-guarded core plus a thin public wrapper.
// This caps payload nesting at maxDepth (10000). Without the cap, a few MB of
// `{"kids":[…` would overflow the goroutine stack, which is a fatal error
// that recover cannot catch. The caller seeds it together with
// generatedTypes. If it is unset, it is computed from the bucket.
var cyclicTypes map[string]struct{}

// multiErrTypes marks generated structs whose decoders collect validation
// failures (multierr) and so always consume the whole value before they
// return. A parent's multierr drain may continue past a nested decode's
// validation error only when the callee is in this set. A single-error callee
// returns in the middle of the value. Continuing would then parse from the
// wrong position, which either reports a bogus ErrBadArray or ErrBadObject
// that hides the real failure, or silently drops the trailing fields. Seeded
// together with generatedTypes.
var multiErrTypes map[string]struct{}

// seedMultiErrTypes fills multiErrTypes from the pass's structs.
func seedMultiErrTypes(structs []model.StructInfo) map[string]struct{} {
	out := make(map[string]struct{}, len(structs))
	for _, s := range structs {
		if s.MultiErr {
			out[s.Name] = struct{}{}
		}
	}
	return out
}

func isCyclic(typeName string) bool {
	_, ok := cyclicTypes[typeName]
	return ok
}

// decodeCallFor renders the bytes-path call that decodes a nested generated
// struct. A cyclic callee goes through its depth core with `depth+1`. Every
// decoder has `depth` in scope. In an acyclic decoder it is a constant 0.
func decodeCallFor(typeName string) string {
	if isCyclic(typeName) {
		return "decodeFromDepth(data[%[2]s:], depth+1)"
	}
	return "DecodeFrom(data[%[2]s:])"
}

// streamDecodeCallFor is decodeCallFor for the stream path.
func streamDecodeCallFor(typeName string) string {
	if isCyclic(typeName) {
		return "decodeFromStreamDepth(s, depth+1)"
	}
	return "DecodeFromStream(s)"
}

// generateTo renders the full set of structs into Go source and writes it to w.
func generateTo(w io.Writer, pkg, scope string, structs []model.StructInfo) error {
	resetOneofRegistry(scope)
	resetCapRegistry()
	// The caller seeds generatedTypes with every struct of the Go package
	// across all build-tag buckets. A nested struct from another bucket then
	// still gets a direct DecodeFrom call. Unseeded, it falls back to this
	// bucket only.
	if generatedTypes == nil {
		generatedTypes = make(map[string]struct{}, len(structs))
		namedKinds = make(map[string]model.TypeKind)
		for _, s := range structs {
			generatedTypes[s.Name] = struct{}{}
		}
		seedNamedKinds(structs)
	}
	if generatedFields == nil {
		generatedFields = seedGeneratedFields(structs)
	}
	if multiErrTypes == nil {
		multiErrTypes = seedMultiErrTypes(structs)
	}
	if cyclicTypes == nil {
		cyclicTypes = model.CyclicTypes(structs)
	}
	// Sort fields alphabetically by JSON name (opt out with nosortkeys).
	// Embed catch-all fields stay at the end, which keeps comma handling
	// simple.
	for i := range structs {
		if structs[i].NoSort {
			continue
		}
		slices.SortStableFunc(structs[i].Fields, func(a, b model.FieldInfo) int {
			switch {
			case a.Embed && !b.Embed:
				return 1
			case !a.Embed && b.Embed:
				return -1
			}
			if a.JSONName < b.JSONName {
				return -1
			}
			if a.JSONName > b.JSONName {
				return 1
			}
			return 0
		})
	}
	preregisterOneOfs(structs)

	var tag string
	if len(structs) > 0 {
		tag = structs[0].BuildTag
	}

	count := 0
	buf := &bytes.Buffer{}
	// Render each struct into its own buffer. The bodies are joined after the
	// prelude.
	bodies := make([][]byte, len(structs))
	for i := range structs {
		renderStructMethods(buf, structs[i])
		count += buf.Len()
		bodies[i] = bytes.Clone(buf.Bytes())
		buf.Reset()
	}

	// The cap and oneof declarations at the top of the file can name element
	// types that no method body names, as in
	// `unsafe.Sizeof(*new(json.RawMessage))`. Scan them too, or their imports
	// go missing.
	scanBufs := bodies
	for _, decl := range capRegistry.decls {
		scanBufs = append(scanBufs, []byte(decl))
	}
	for _, decl := range oneofRegistry.decls {
		scanBufs = append(scanBufs, []byte(decl))
	}
	stdlib, third := collectImports(structs, scanBufs)

	// format.Source needs the whole file in one []byte. Grow the buffer by the
	// total body size up front so the copy loop does not regrow it many times.
	writePrelude(buf, pkg, tag, stdlib, third)
	for _, decl := range capRegistry.decls {
		buf.WriteString(decl)
		buf.WriteByte('\n')
	}
	for _, decl := range oneofRegistry.decls {
		buf.WriteString(decl)
		buf.WriteByte('\n')
	}
	buf.Grow(count)
	for _, body := range bodies {
		buf.Write(body)
	}
	src, err := format.Source(buf.Bytes())
	if err != nil {
		return fmt.Errorf("formatting generated code: %w\n\nraw output:\n%s", err, buf.Bytes())
	}
	_, err = w.Write(src)
	return err
}

// seedGeneratedFields maps every generated struct that encodes as a JSON
// object to its fields. Aliases are left out because their methods delegate.
// An entry here means "encodes as an object with exactly these fields".
func seedGeneratedFields(structs []model.StructInfo) map[string][]model.FieldInfo {
	out := make(map[string][]model.FieldInfo, len(structs))
	for _, s := range structs {
		if !s.IsAlias {
			out[s.Name] = s.Fields
		}
	}
	return out
}

// importSpec is one line of the import block. Alias is set only when the
// qualifier differs from the package's declared name (see qualifierFor).
type importSpec struct {
	Path  string
	Alias string
}

// writePrelude writes the file header into buf: the generated-code marker,
// the build tag, the package clause and the import block.
func writePrelude(buf *bytes.Buffer, pkg, buildTag string, stdlib, third []importSpec) {
	buf.WriteString("// Code generated by ggen; DO NOT EDIT.\n\n")
	if buildTag != "" {
		buf.WriteString("//go:build ")
		buf.WriteString(buildTag)
		buf.WriteString("\n\n")
	}
	buf.WriteString("package ")
	buf.WriteString(pkg)
	buf.WriteString("\n\n")
	if len(stdlib) == 0 && len(third) == 0 {
		return
	}
	buf.WriteString("import (\n")
	writeImport := func(s importSpec) {
		buf.WriteByte('\t')
		if s.Alias != "" {
			buf.WriteString(s.Alias)
			buf.WriteByte(' ')
		}
		buf.WriteByte('"')
		buf.WriteString(s.Path)
		buf.WriteByte('"')
		buf.WriteByte('\n')
	}
	for _, s := range stdlib {
		writeImport(s)
	}
	if len(stdlib) > 0 && len(third) > 0 {
		buf.WriteByte('\n')
	}
	for _, s := range third {
		writeImport(s)
	}
	buf.WriteString(")\n\n")
}

// renderStructMethods writes the full method set for one struct: DecodeFrom,
// DecodeFromStream, JSONSize and AppendJSON, plus the optional MarshalJSON
// and UnmarshalJSON hooks.
func renderStructMethods(buf *bytes.Buffer, s model.StructInfo) {
	// Cap constant names include the struct they were registered under, as in
	// ggenCap_<Struct>_<Field>_<elemType>. See capName.
	currentStructName = s.Name
	renderDecode(buf, s)

	renderStreamDecode(buf, s)

	renderSize(buf, s)

	renderAppendJSON(buf, s)

	if s.Marshal {
		fmt.Fprintf(buf, "func (s %s) MarshalJSON() ([]byte, error) {\n\treturn ggen.Marshal(s)\n}\n\n", s.Name)
	}
	if s.Unmarshal {
		fmt.Fprintf(
			buf,
			"func (s *%s) UnmarshalJSON(data []byte) error {\n\tvar zero %s\n\tv, _, err := zero.DecodeFrom(data)\n\tif err != nil {\n\t\treturn err\n\t}\n\t*s = v\n\treturn nil\n}\n\n",
			s.Name,
			s.Name,
		)
	}
}

// collectImports returns the sorted imports the generated file uses. It
// combines a walk over the StructInfo features with a scan of the rendered
// bodies.
func collectImports(structs []model.StructInfo, bodies [][]byte) ([]importSpec, []importSpec) {
	// Maps import path to explicit qualifier. "" uses the declared name.
	need := map[string]string{
		// The ggen runtime package is named by every generated method.
		"github.com/sirkostya009/ggen": "",
	}
	add := func(p string) {
		if p == "" {
			return
		}
		if _, ok := need[p]; !ok {
			need[p] = ""
		}
	}
	addImport := func(ti model.TypeImport) {
		if ti.Path == "" {
			return
		}
		if ti.Explicit {
			need[ti.Path] = ti.Name
		} else {
			add(ti.Path)
		}
	}
	// Candidate foreign packages keyed by qualifier. Only the ones a rendered
	// body actually names are imported. See scanBodiesForForeignImports.
	foreign := map[string]model.TypeImport{}

	// Walk the features. Each feature found turns on its imports.
	walkCustomV := func(rules []model.ValidationRule) {
		for _, r := range rules {
			if r.Custom {
				add(r.PkgImport)
			}
		}
	}
	walkCustomM := func(rules []model.ModRule) {
		for _, r := range rules {
			if r.Custom {
				add(r.PkgImport)
			}
		}
	}
	for _, s := range structs {
		// A struct alias over a type from another package, such as
		// `type Local uuid.UUID`, needs that package's import.
		if s.IsAlias && s.AliasKind == model.KindStruct {
			addImport(s.AliasUnderlyingImport)
		}
		// streamUnknownKey always clones the key that KeyView returns,
		// because that key aliases the Stream's buffer.
		if !s.IsAlias {
			add("strings")
		}
		// A container alias keeps its shape in AliasField, not Fields.
		if s.IsAlias {
			//exhaustive:ignore not every kind applies here
			switch s.AliasKind {
			case model.KindBytes, model.KindSlice, model.KindArray, model.KindMap:
				collectFieldImports(s.AliasField, addImport)
				for _, ti := range s.AliasField.TypeImports {
					foreign[ti.Name] = ti
				}
			}
		}
		for _, f := range s.Fields {
			collectFieldImports(f, addImport)
			for _, ti := range f.TypeImports {
				foreign[ti.Name] = ti
			}
			walkCustomV(f.Validation)
			walkCustomV(f.ElemValidation)
			walkCustomV(f.KeyValidation)
			for _, inner := range f.InnerValidation {
				walkCustomV(inner)
			}
			walkCustomM(f.Mods)
			walkCustomM(f.ElemMods)
			walkCustomM(f.KeyMods)
			for _, inner := range f.InnerMods {
				walkCustomM(inner)
			}
		}
	}
	// Stdlib helper imports come from what was emitted. Scanning the rendered
	// bodies for the qualified token is exact, and it avoids a per-kind walk
	// over containers nested to any depth.
	scanBodiesForStdImports(bodies, add)
	scanBodiesForForeignImports(bodies, foreign, addImport)
	out := make([]importSpec, 0, len(need))
	third := make([]importSpec, 0, len(need))
	for p, alias := range need {
		spec := importSpec{Path: p, Alias: alias}
		if isThirdParty(p) {
			third = append(third, spec)
		} else {
			out = append(out, spec)
		}
	}
	byPath := func(a, b importSpec) int { return strings.Compare(a.Path, b.Path) }
	slices.SortFunc(out, byPath)
	slices.SortFunc(third, byPath)
	return out, third
}

// namesQualifier reports whether body contains the package qualifier tok,
// such as `time.`, at an identifier boundary. A plain substring test would
// also match the end of a field name. For example `result.Uptime.DecodeFrom(…)`
// contains `time.`, and the file would import a package it never uses.
func namesQualifier(body, tok []byte) bool {
	for {
		i := bytes.Index(body, tok)
		if i < 0 {
			return false
		}
		if i == 0 || !isIdentByte(body[i-1]) {
			return true
		}
		body = body[i+1:]
	}
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// scanBodiesForStdImports searches the rendered struct bodies for stdlib
// qualifiers and calls add with the matching import path. A token is dropped
// from the search after its first match.
func scanBodiesForStdImports(bodies [][]byte, add func(string)) {
	table := []struct {
		token []byte
		path  string
	}{
		{[]byte("strconv."), "strconv"},
		{[]byte("reflect."), "reflect"},
		{[]byte("archsimd."), "simd/archsimd"},
		{[]byte("bits."), "math/bits"},
		{[]byte("math."), "math"},
		{[]byte("unsafe."), "unsafe"},
		{[]byte("strings."), "strings"},
		{[]byte("utf8."), "unicode/utf8"},
		{[]byte("bytes."), "bytes"},
		{[]byte("fmt."), "fmt"},
		{[]byte("time."), "time"},
		// Element type names in prealloc constants and element temps can
		// name these even when no method body does. Structs in test files
		// are parsed from the AST only, so TypeImports cannot cover them.
		{[]byte("jsontext."), "encoding/json/jsontext"},
		{[]byte("json."), "encoding/json"},
		{[]byte("big."), "math/big"},
	}
	for _, body := range bodies {
		for i := range table {
			if table[i].path == "" {
				continue
			}
			if namesQualifier(body, table[i].token) {
				add(table[i].path)
				table[i].path = ""
			}
		}
	}
}

// scanBodiesForForeignImports adds the foreign packages whose qualifier a
// rendered body actually names. A type from another package shows up in the
// generated file in one of two ways. It can be named in a declaration, such as
// `var v leaf.Leaf`, `make(map[string]leaf.Leaf)` or `append(x, leaf.Leaf{})`.
// This covers every container, pointer and map value position. Or it is never
// named at all: a plain value field only writes `result.X` and calls methods
// on it. An unconditional import would fail the second case with "imported
// and not used". So the candidates are filtered like the stdlib helpers.
func scanBodiesForForeignImports(bodies [][]byte, cands map[string]model.TypeImport, add func(model.TypeImport)) {
	for name, ti := range cands {
		tok := []byte(name + ".")
		for _, body := range bodies {
			if namesQualifier(body, tok) {
				add(ti)
				break
			}
		}
	}
}

// collectFieldImports adds per-field imports.
func collectFieldImports(f model.FieldInfo, addImport func(model.TypeImport)) {
	add := func(p string) { addImport(model.TypeImport{Path: p}) }
	walkValidation := func(rules []model.ValidationRule) {
		for _, v := range rules {
			switch v.Name {
			case "runes", "minrunes", "maxrunes":
				add("unicode/utf8")
			case "starts", "ends", "contains":
				add("strings")
			case "url", "alphanum",
				"numeric", "hexadecimal",
				"islower", "isupper":
				add("github.com/sirkostya009/ggen")
			}
		}
	}
	walkValidation(f.Validation)
	walkValidation(f.ElemValidation)
	walkValidation(f.KeyValidation)
	for _, inner := range f.InnerValidation {
		walkValidation(inner)
	}
	walkMods := func(rules []model.ModRule) {
		for _, m := range rules {
			switch m.Name {
			case "trim", "tolower", "toupper", "trimleft", "trimright", "replace":
				add("strings")
			}
		}
	}
	walkMods(f.Mods)
	walkMods(f.ElemMods)
	walkMods(f.KeyMods)
	for _, inner := range f.InnerMods {
		walkMods(inner)
	}
	// crossPkgStruct handles KindStruct fields that this pass does not
	// generate. They fall back to json.Unmarshal and json.Marshal when no
	// ggen, text or JSON method is found.
	crossPkgStruct := func(typ string, iface model.FieldInterfaces) {
		if isGenerated(typ) {
			return
		}
		// A named primitive is decoded and encoded inline. It never reaches
		// encoding/json.
		if _, _, ok := inlineNamedPrim(model.FieldInfo{
			GoType: typ, Kind: model.KindStruct, HTMLEscape: f.HTMLEscape, Copy: f.Copy,
			AllowInvalidUTF8: f.AllowInvalidUTF8, NoValidate: f.NoValidate,
		}); ok {
			return
		}
		// A resolved type with a known method uses that method, not json.
		if iface.Resolved &&
			(iface.ByteDecoder || iface.JSONUnmarshaler || iface.TextUnmarshaler ||
				iface.AppendJSON || iface.JSONMarshaler || iface.TextAppender ||
				iface.TextMarshaler) {
			return
		}
		add("encoding/json")
	}
	//exhaustive:ignore not every kind applies here
	switch f.Kind {
	case model.KindTime, model.KindDuration:
		add("time")
	case model.KindNetIP:
		add("net")
	case model.KindNetipAddr, model.KindNetipPrefix:
		add("net/netip")
	case model.KindURL:
		add("net/url")
	case model.KindBytes:
		switch f.Format {
		case "", "base64", "base64url":
			add("encoding/base64")
		case "base32", "base32hex":
			add("encoding/base32")
		case "base16", "hex":
			add("encoding/hex")
		}
	case model.KindSQLNull:
		add("database/sql")
		if f.SQLNullInner != nil {
			// Add the foreign packages named in the type, then recurse for
			// the inner type's own imports.
			for _, imp := range f.SQLNullImports {
				addImport(imp)
			}
			collectFieldImports(*f.SQLNullInner, addImport)
		} else if spec, ok := model.SQLNullSpec(f.GoType); ok {
			//exhaustive:ignore not every kind applies here
			if spec.Inner == model.KindTime {
				add("time")
			}
		}
	case model.KindStruct:
		// For a *T field f.GoType is "*T", but isGenerated matches bare
		// names. Use PointeeType so the lookup by struct name works.
		typ := f.GoType
		if f.Pointer {
			typ = f.PointeeType
		}
		if f.Pointer && len(typ) > 0 && typ[0] == '*' {
			// For a multi-level pointer such as `**T`, only the leaf kind
			// decides the imports. Recurse on a synthetic leaf field.
			leaf := f
			leaf.Pointer = false
			leaf.PointeeType = ""
			_, leaf.GoType = model.PointerDepth(f.GoType)
			leaf.Kind = model.ResolveKind(leaf.GoType)
			collectFieldImports(leaf, addImport)
		} else {
			crossPkgStruct(typ, f.Iface)
		}
	case model.KindSlice, model.KindArray, model.KindMap:
		// For pointer elements and pointer map values, only the leaf kind
		// decides the imports, as above. Non-pointer struct elements from
		// another package keep the json fallback.
		if et, eDepth := elemPtrType(f); eDepth > 0 {
			leaf := elemPtrField(f, f.JSONName)
			leaf.Pointer = false
			leaf.PointeeType = ""
			_, leaf.GoType = model.PointerDepth(et)
			collectFieldImports(leaf, addImport)
		} else if f.ElemKind == model.KindStruct {
			crossPkgStruct(f.ElemType, f.ElemIface)
		} else {
			//exhaustive:ignore not every kind applies here
			switch f.ElemKind {
			case model.KindSlice, model.KindArray:
				// Nested container. The innermost element kind decides the
				// imports.
				collectFieldImports(peelSliceField(f), addImport)
			case model.KindTime, model.KindDuration, model.KindBytes, model.KindRawJSON, model.KindNetIP, model.KindNetipAddr,
				model.KindNetipPrefix, model.KindURL, model.KindBigInt, model.KindBigFloat, model.KindBigRat, model.KindSQLNull, model.KindAny, model.KindMap:
				// An element with a dedicated kind uses the field emitters, so
				// it needs the same imports as a field of that kind.
				collectFieldImports(sliceElemField(f), addImport)
			}
		}
	case model.KindAny:
	case model.KindRawJSON:
	}
}

func isThirdParty(p string) bool {
	seg, _, _ := strings.Cut(p, "/")
	return strings.ContainsRune(seg, '.')
}

// preregisterOneOfs walks all rules and fills the oneof registry before
// rendering starts. The registry holds one package-level slice per unique
// allowed set. Registering first numbers the slices in the order the rules
// are declared, whatever order rendering later reaches them in.
func preregisterOneOfs(structs []model.StructInfo) {
	walk := func(rules []model.ValidationRule) {
		for _, v := range rules {
			if v.Name == "oneof" {
				registerOneOf(model.SplitPipeParts(v.Value))
			}
		}
	}
	for _, s := range structs {
		for _, f := range s.Fields {
			walk(f.Validation)
			walk(f.ElemValidation)
			walk(f.KeyValidation)
			for _, inner := range f.InnerValidation {
				walk(inner)
			}
		}
	}
}

// scalarCountable reports whether a flat container with element kind k can be
// sized exactly by counting delimiters in the bytes-path input. This holds
// only for number and bool elements, because their text never contains `,`,
// `]`, `:` or `}`.
func scalarCountable(k model.TypeKind) bool { return model.IsNumeric(k) || k == model.KindBool }

// renderOneMod emits one mod, a value transform, against ref. posVar picks the
// return shape for a mod that can fail. "" means the stream path, which
// returns two values. Anything else means the bytes path, which returns
// three. A custom mod has one of three forms:
//   - pure, `func(T) T`
//   - error form, `func(T) (T, error)`, where an error becomes a parse error
//   - bool form, `func(T) (T, bool)`, where false becomes a ggen.ModError
//     parse error
func renderOneMod(b *bytes.Buffer, m model.ModRule, field, ref, goType string, kind model.TypeKind, posVar string) {
	kind = effectiveKind(goType, kind)
	prim := kindPrimitiveName(kind)
	cast := goType != "" && prim != "" && goType != prim
	wrap := func(rhs string) string {
		if cast {
			return fmt.Sprintf("%s(%s)", goType, rhs)
		}
		return rhs
	}
	asPrim := func(expr string) string {
		if cast {
			return fmt.Sprintf("%s(%s)", prim, expr)
		}
		return expr
	}
	{
		if m.Custom {
			call := m.FuncName
			if m.PkgName != "" {
				call = m.PkgName + "." + m.FuncName
			}
			if m.Fallible {
				// Errors propagate as parse errors, not validation errors.
				ret := func(errExpr string) string {
					if posVar != "" {
						return "return result, " + posVar + ", " + errExpr
					}
					return "return result, " + errExpr
				}
				if m.BoolForm {
					// ModError is a parse error. Wrap it so it carries the
					// field path like every other decode failure, as
					// mod_error.go documents.
					pos := posVar
					if pos == "" {
						pos = "s.Offset()"
					}
					modErr := fmt.Sprintf("ggen.NewParseErr(%s, %s, &ggen.ModError{%sName: %q, Msg: %q, Value: %s})",
						field, pos, posLit(posVar), strings.TrimPrefix(m.Name, "@"), m.Msg, ref)
					fmt.Fprintf(b, "if v, ok := %s(%s); !ok {\n\t%s\n} else {\n\t%s = v\n}\n",
						call, ref, ret(modErr), ref)
				} else {
					// The mod's own error is not a ggen error. Wrap it so it
					// carries the field path and offset, like every other
					// decode failure. errors.As still reaches the mod's error.
					pos := posVar
					if pos == "" {
						pos = "s.Offset()"
					}
					wrapped := fmt.Sprintf("ggen.NewParseErr(%s, %s, err)", field, pos)
					fmt.Fprintf(b, "if v, err := %s(%s); err != nil {\n\t%s\n} else {\n\t%s = v\n}\n",
						call, ref, ret(wrapped), ref)
				}
			} else {
				fmt.Fprintf(b, "%s = %s(%s)\n", ref, call, ref)
			}
			return
		}
		switch m.Name {
		case "trim":
			fmt.Fprintf(b, "%s = %s\n", ref, wrap(fmt.Sprintf("strings.TrimSpace(%s)", asPrim(ref))))
		case "tolower":
			fmt.Fprintf(b, "%s = %s\n", ref, wrap(fmt.Sprintf("strings.ToLower(%s)", asPrim(ref))))
		case "toupper":
			fmt.Fprintf(b, "%s = %s\n", ref, wrap(fmt.Sprintf("strings.ToUpper(%s)", asPrim(ref))))
		case "trimleft":
			fmt.Fprintf(b, "%s = %s\n", ref, wrap(fmt.Sprintf("strings.TrimPrefix(%s, %s)", asPrim(ref), strconv.Quote(m.Value))))
		case "trimright":
			fmt.Fprintf(b, "%s = %s\n", ref, wrap(fmt.Sprintf("strings.TrimSuffix(%s, %s)", asPrim(ref), strconv.Quote(m.Value))))
		case "replace":
			parts := model.SplitPipeParts(m.Value)
			if len(parts) == 2 {
				fmt.Fprintf(b, "%s = %s\n", ref,
					wrap(fmt.Sprintf("strings.ReplaceAll(%s, %s, %s)", asPrim(ref), strconv.Quote(parts[0]), strconv.Quote(parts[1]))))
			}
		case "clamp":
			// clamp=lo|hi limits the value to [lo, hi]. The constants are
			// converted to the field's type so `<` and `>` compile for named
			// number types.
			cparts := model.SplitPipeParts(m.Value)
			if len(cparts) != 2 {
				return
			}
			lo := strings.TrimSpace(cparts[0])
			hi := strings.TrimSpace(cparts[1])
			if lo != "" {
				fmt.Fprintf(b, "if %s < %s { %s = %s }\n", ref, wrap(lo), ref, wrap(lo))
			}
			if hi != "" {
				fmt.Fprintf(b, "if %s > %s { %s = %s }\n", ref, wrap(hi), ref, wrap(hi))
			}
		}
	}
}

// seedNamedKinds fills namedKinds from two sources. The first is this pass's
// own primitive aliases, such as `//ggen:generate type Count int`. The second
// is the named primitives the parser resolved from go/types on each field's
// type. That also covers types the user never annotated, and types from other
// packages such as `leaf.Name`.
func seedNamedKinds(structs []model.StructInfo) {
	aliasFlags = map[string]aliasCodegen{}
	for _, s := range structs {
		if s.IsAlias && kindPrimitiveName(s.AliasKind) != "" {
			namedKinds[s.Name] = s.AliasKind
			aliasFlags[s.Name] = aliasCodegen{
				HTMLEscape:       s.HTMLEscape,
				Copy:             s.Copy,
				AllowInvalidUTF8: s.AllowInvalidUTF8,
				NoValidate:       s.NoValidate,
			}
		}
		for _, f := range s.Fields {
			maps.Copy(namedKinds, f.NamedPrims)
		}
	}
}

// inlineNamedPrim reports whether a field of a named primitive type should be
// decoded and encoded inline. Inline means: scan the underlying primitive and
// convert at the assignment. The other choice is to call the type's own
// generated methods, or encoding/json when it has none.
//
// `type B S; type S string` resolves straight to `string`. go/types'
// Underlying walks the whole chain, so a chain of named types costs nothing.
//
// The check is about behaviour, not structure. An annotated alias can carry
// its own flags. `//ggen:generate htmlescape type HtmlString string` is the
// documented way to escape one type and not the rest. `copy`,
// `allowinvalidutf8` and `novalidate` also change what its body emits.
// Inlining with the parent's flags would silently change that behaviour, so
// such aliases keep the call. A flag set on the command line applies to both
// sides equally, so it never blocks inlining.
func inlineNamedPrim(f model.FieldInfo) (prim string, kind model.TypeKind, ok bool) {
	// KindStruct means "a named type with no special handling". Any other
	// kind, such as KindDuration, KindTime or KindBytes, has its own wire
	// shape and its own emitter, even if a primitive is underneath.
	if f.Kind != model.KindStruct {
		return "", 0, false
	}
	k, isNamed := namedKinds[f.GoType]
	if !isNamed {
		return "", 0, false
	}
	// A type generated in another pass keeps its own methods. Its flags are
	// not visible here, since aliasFlags only covers this pass. Inlining would
	// apply the parent's flags and could silently drop, for example, an
	// `htmlescape` the other package was generated with. Its DecodeFrom and
	// AppendJSON already apply its own choices.
	if !isGenerated(f.GoType) && f.Iface.Resolved && (f.Iface.ByteDecoder || f.Iface.AppendJSON) {
		return "", 0, false
	}
	prim = kindPrimitiveName(k)
	if prim == "" || prim == f.GoType {
		return "", 0, false
	}
	if a, generated := aliasFlags[f.GoType]; generated {
		if a.HTMLEscape != f.HTMLEscape || a.Copy != f.Copy ||
			a.AllowInvalidUTF8 != f.AllowInvalidUTF8 || a.NoValidate != f.NoValidate {
			return "", 0, false
		}
	}
	return prim, k, true
}

// namedPrimInner turns f into a field of the underlying primitive for the
// inline scan. The value steps stay on the outer field so they run against
// the named type. Rules like `oneof` and `eq` resolve its kind through
// effectiveKind. The inner render only produces the raw value.
func namedPrimInner(f model.FieldInfo, prim string, kind model.TypeKind) model.FieldInfo {
	inner := f
	inner.GoType = prim
	inner.Kind = kind
	inner.Validation = nil
	inner.Mods = nil
	inner.Pipe = nil
	inner.Levels = nil
	inner.NamedPrims = nil
	inner.Iface = model.FieldInterfaces{}
	inner.AtDispatch = false
	return inner
}

// effectiveKind maps a named type over a primitive to that primitive's kind.
// Both an annotated `//ggen:generate type Priority string` and a plain
// `type Priority string` the user never annotated are in namedKinds. Fields
// of the annotated one report KindStruct, because it gets a full method set.
// Every rule emitter must call this. Without it a named string reaches
// `if kind == KindString` as KindStruct. The rule then emits a raw token such
// as `case low:`, or for eq and neq it emits nothing at all.
func effectiveKind(goType string, kind model.TypeKind) model.TypeKind {
	if k, ok := namedKinds[goType]; ok {
		return k
	}
	return kind
}

// primCast converts ref to its underlying primitive when goType is a named
// type. The emitted code needs this wherever it passes the value to an API
// that takes a string, such as `utf8.RuneCountInString`, `strings.HasPrefix`
// or `ggen.IsURL`, or stores it in an error field of type string. Comparisons
// against untyped constants, as in len, gte, eq and oneof, need no
// conversion. So callers wrap only where the target type is concrete.
func primCast(goType string, kind model.TypeKind, ref string) string {
	prim := kindPrimitiveName(kind)
	if goType == "" || prim == "" || goType == prim {
		return ref
	}
	return prim + "(" + ref + ")"
}

// kindPrimitiveName returns the Go literal name for a primitive TypeKind,
// or "" for kinds that aren't a single primitive token.
func kindPrimitiveName(k model.TypeKind) string {
	//exhaustive:ignore not every kind applies here
	switch k {
	case model.KindString:
		return "string"
	case model.KindBool:
		return "bool"
	case model.KindInt:
		return "int"
	case model.KindInt8:
		return "int8"
	case model.KindInt16:
		return "int16"
	case model.KindInt32:
		return "int32"
	case model.KindInt64:
		return "int64"
	case model.KindUint:
		return "uint"
	case model.KindUint8:
		return "uint8"
	case model.KindUint16:
		return "uint16"
	case model.KindUint32:
		return "uint32"
	case model.KindUint64:
		return "uint64"
	case model.KindFloat32:
		return "float32"
	case model.KindFloat64:
		return "float64"
	}
	return ""
}

// numBound writes a numeric rule's bound for an error field of type `any`.
// The literal is converted to the field's own kind. An untyped literal would
// default to int, which overflows above MaxInt64, or to float64, which
// reports a rounded value.
func numBound(kind model.TypeKind, value string) string {
	prim := kindPrimitiveName(kind)
	if prim == "" {
		return value
	}
	return prim + "(" + value + ")"
}

// posLit builds the Pos field for a validation error literal. Pos is the byte
// offset of the failure from the start of the whole payload. posVar is the
// bytes-path cursor. "" selects the stream path, which uses s.Offset() because
// s.Pos shifts when the Stream drops consumed bytes from its buffer.
func posLit(posVar string) string {
	if posVar == "" {
		return "Pos: s.Offset(), "
	}
	return fmt.Sprintf("Pos: %s, ", posVar)
}

// withPos injects posLit right after the opening brace of a
// `&ggen.XError{...}` literal so every error carries its position.
func withPos(errExpr, posVar string) string {
	k := strings.IndexByte(errExpr, '{')
	return errExpr[:k+1] + posLit(posVar) + errExpr[k+1:]
}

// byteArrayLen returns N for a `[N]byte` field that is encoded as base64
// through the KindBytes path, and 0 otherwise. jsonv2 encodes byte arrays as
// base64 and rejects the v1 form, an array of numbers. The decoded length
// must be exactly N.
func byteArrayLen(f model.FieldInfo) int {
	if f.Kind == model.KindBytes && f.ArrayLen > 0 {
		return f.ArrayLen
	}
	return 0
}

// leafKind is the kind of a pointer field's pointee. In the type string
// `[N]byte` reads as a plain array. Only Kind and ArrayLen record that it is
// encoded as base64. So a byte-array pointee keeps KindBytes.
func leafKind(f model.FieldInfo, leafType string) model.TypeKind {
	if byteArrayLen(f) > 0 {
		return model.KindBytes
	}
	return model.ResolveKind(leafType)
}

// arrayLenErr builds a typed *ggen.LenError literal for the strict element
// count check of a fixed-length array.
func arrayLenErr(field string, want int, gotExpr, posVar string) string {
	return withPos(fmt.Sprintf("&ggen.LenError{Path: []string{%q}, Want: %d, Got: %s}", field, want, gotExpr), posVar)
}

// tupleOverflowErr is the LenError for a tuple that stops at its first element
// past want. The true length is only known to be at least want+1.
func tupleOverflowErr(field string, want int, posVar string) string {
	return withPos(fmt.Sprintf("&ggen.LenError{Path: []string{%q}, Want: %d, Got: %d, AtLeast: true}", field, want, want+1), posVar)
}

// requiredErr builds a typed *ggen.RequiredError literal.
func requiredErr(field string) string {
	return fmt.Sprintf("&ggen.RequiredError{Path: []string{%q}}", field)
}

// oneofRegistry collects the unique sets of allowed strings. Each set is
// emitted once as a package-level slice that is never modified, so building a
// OneOfError never allocates the slice.
var oneofRegistry struct {
	names map[string]string // parts joined by "\x00", mapped to the var name
	decls []string
	// prefix keeps the slice names unique across the output files of one Go
	// package. Single-file mode (`ggen $GOFILE`, one generated file per
	// source) restarts the counter for each file. Without the prefix, two
	// sources in one package that both use `oneof` would declare the same
	// name. It comes from the output file name (see emitScope), so it is
	// stable across runs.
	prefix string
}

func resetOneofRegistry(scope string) {
	oneofRegistry.names = map[string]string{}
	oneofRegistry.decls = nil
	oneofRegistry.prefix = emitScope(scope)
}

// emitScope derives the registry name prefix from the output file name, so
// adding or removing structs never renames anything. Only the `_ggen` marker
// is dropped. `p_ggen.go` and `p_ggen_test.go` are two files of one package,
// so the test file keeps its `_test` and the scopes are `p` and `p_test`.
func emitScope(scope string) string {
	base := strings.TrimSuffix(filepath.Base(scope), ".go")
	if stem, ok := strings.CutSuffix(base, "_ggen_test"); ok {
		base = stem + "_test"
	} else {
		base = strings.TrimSuffix(base, "_ggen")
	}
	return sanitizeIdent(base)
}

// sanitizeIdent maps a type or file name onto identifier characters. Letters
// and digits are kept, `*` becomes Ptr, and every other rune becomes exactly
// one `_`. Runs of `_` are not collapsed, so distinct inputs stay distinct:
// `[]int` becomes `__int`, not `int`. A collision would redeclare a constant,
// which fails loudly at compile time.
func sanitizeIdent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '*':
			b.WriteString("Ptr")
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func registerOneOf(parts []string) string {
	key := strings.Join(parts, "\x00")
	if name, ok := oneofRegistry.names[key]; ok {
		return name
	}
	name := fmt.Sprintf("ggenOneof_%s_%d", oneofRegistry.prefix, len(oneofRegistry.names))
	oneofRegistry.names[key] = name
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = strconv.Quote(p)
	}
	oneofRegistry.decls = append(oneofRegistry.decls,
		fmt.Sprintf("var %s = []string{%s}", name, strings.Join(quoted, ", ")))
	return name
}

// renderValidationOn emits validation checks against ref. jsonName appears in
// the errors. kind selects comparisons that fit the type. With multiErr set, a
// failure is appended to errs instead of returned. posVar picks the return
// shape: "" returns two values, anything else returns three.
func renderValidationOn(b *bytes.Buffer, rules []model.ValidationRule, ref, jsonName, goType string, kind model.TypeKind, multiErr bool, posVar string) {
	onErr := func(errExpr string) string {
		errExpr = withPos(errExpr, posVar)
		if multiErr {
			return "errs = append(errs, " + errExpr + ")"
		}
		if posVar == "" {
			return "return result, " + errExpr
		}
		return "return result, " + posVar + ", " + errExpr
	}

	emitValRun(b, rules, ref, jsonName, goType, kind, multiErr, onErr)
}

func isRuneRule(name string) bool {
	return name == "runes" || name == "minrunes" || name == "maxrunes"
}

// asciiImplyingRule reports whether passing this validator guarantees the
// value is pure ASCII. A rune is one Unicode character. In UTF-8 it takes 1 to
// 4 bytes, and an ASCII rune takes exactly 1. So for ASCII text
// utf8.RuneCountInString equals len. A rune rule that comes after one of
// these rules in the same run can skip counting.
func asciiImplyingRule(name string) bool {
	switch name {
	case "alphanum", "numeric", "hexadecimal":
		return true
	}
	return false
}

// emitValRun emits a run of validators against a ref that does not change.
// Callers split runs at mods, because mods change ref. The rune count rules
// runes, minrunes and maxrunes go through emitRuneRule. It checks the byte
// length first to avoid the O(len) utf8.RuneCountInString walk where it can.
// If a rule that implies ASCII already passed earlier in this run, and
// multierr is off, the count is exactly len and the walk is dropped. Under
// multierr a failed earlier rule does not stop the run, so the value may not
// be ASCII.
func emitValRun(b *bytes.Buffer, run []model.ValidationRule, ref, jsonName, goType string, kind model.TypeKind, multiErr bool, onErr func(string) string) {
	kind = effectiveKind(goType, kind)
	asciiSeen := false
	for _, v := range run {
		if isRuneRule(v.Name) {
			emitRuneRule(b, v, ref, jsonName, goType, kind, onErr, asciiSeen && !multiErr)
		} else {
			renderOneVal(b, v, ref, jsonName, goType, kind, onErr)
		}
		if asciiImplyingRule(v.Name) {
			asciiSeen = true
		}
	}
}

// emitRuneRule emits one rune count rule: runes, minrunes or maxrunes. With
// useLen the count is len and no walk is emitted. Otherwise byte length
// checks decide the cases that surely pass or surely fail in O(1). A string
// of B bytes holds between B/4 and B runes, so only lengths in the band
// between those bounds need the walk. The Got field of the failure literal
// reports the real rune count.
func emitRuneRule(b *bytes.Buffer, v model.ValidationRule, ref, jsonName, goType string, kind model.TypeKind, onErr func(string) string, useLen bool) {
	errLit := func(got string) string {
		switch v.Name {
		case "runes":
			return fmt.Sprintf("&ggen.RunesError{Path: []string{%q}, Want: %s, Got: %s}", jsonName, v.Value, got)
		case "minrunes":
			return fmt.Sprintf("&ggen.MinRunesError{Path: []string{%q}, Limit: %s, Got: %s}", jsonName, v.Value, got)
		default: // maxrunes
			return fmt.Sprintf("&ggen.MaxRunesError{Path: []string{%q}, Limit: %s, Got: %s}", jsonName, v.Value, got)
		}
	}
	// utf8 takes a plain string, so a named string type needs a conversion.
	walk := fmt.Sprintf("utf8.RuneCountInString(%s)", primCast(goType, kind, ref))

	// Every byte is a rune, so the count is len(ref) and no walk is needed.
	if useLen {
		l := fmt.Sprintf("len(%s)", ref)
		op := map[string]string{"runes": "!=", "minrunes": "<", "maxrunes": ">"}[v.Name]
		fmt.Fprintf(b, "if %s %s %s {\n\t%s\n}\n", l, op, v.Value, onErr(errLit(l)))
		return
	}

	// Byte length checks. If the bound does not parse as an integer, fall back
	// to always walking.
	n, err := strconv.Atoi(v.Value)
	if err != nil {
		op := map[string]string{"runes": "!=", "minrunes": "<", "maxrunes": ">"}[v.Name]
		fmt.Fprintf(b, "if %s %s %s {\n\t%s\n}\n", walk, op, v.Value, onErr(errLit(walk)))
		return
	}
	switch v.Name {
	case "minrunes":
		// Rule: runes >= n. Fails for sure below length n. Passes for sure
		// at length 4n-3 and above. Walk only in between.
		fmt.Fprintf(b, "if len(%s) < %d {\n\t%s\n}", ref, n, onErr(errLit(walk)))
		if 4*n-3 > n {
			fmt.Fprintf(b, " else if len(%s) < %d {\n\tif rc := %s; rc < %d {\n\t\t%s\n\t}\n}", ref, 4*n-3, walk, n, onErr(errLit("rc")))
		}
		b.WriteString("\n")
	case "maxrunes":
		// Rule: runes <= n. Passes for sure at length n and below. Fails for
		// sure above length 4n. Walk only in between.
		fmt.Fprintf(b, "if len(%s) > %d {\n\t%s\n}", ref, 4*n, onErr(errLit(walk)))
		if n >= 1 {
			fmt.Fprintf(b, " else if len(%s) > %d {\n\tif rc := %s; rc > %d {\n\t\t%s\n\t}\n}", ref, n, walk, n, onErr(errLit("rc")))
		}
		b.WriteString("\n")
	case "runes":
		// Rule: runes == n. Fails for sure outside lengths [n, 4n]. Walk
		// inside that band.
		fmt.Fprintf(b, "if len(%s) < %d || len(%s) > %d {\n\t%s\n} else if rc := %s; rc != %d {\n\t%s\n}\n",
			ref, n, ref, 4*n, onErr(errLit(walk)), walk, n, onErr(errLit("rc")))
	}
}

// renderOneVal emits one validation check against ref. onErr turns a typed
// error literal into the right failure action. Built-in validators are
// chosen by name. A custom `@Func` validator calls the user's function. Its
// error form wraps the error in CustomError. Its bool form (BoolForm) emits a
// PredicateError when the function returns false. The rune count rules
// runes, minrunes and maxrunes are not handled here. emitValRun sends them to
// emitRuneRule.
func renderOneVal(b *bytes.Buffer, v model.ValidationRule, ref, jsonName, goType string, kind model.TypeKind, onErr func(string) string) {
	kind = effectiveKind(goType, kind)
	// str is ref converted to a plain string. It is used by the checks and
	// error fields that need a string, not an untyped constant comparison.
	str := primCast(goType, kind, ref)
	switch v.Name {
	case "required", "optional":
		// required is handled elsewhere. optional is only a marker.

	case "notempty":
		fmt.Fprintf(b, "if len(%s) == 0 {\n\t%s\n}\n", ref,
			onErr(fmt.Sprintf("&ggen.NotEmptyError{Path: []string{%q}}", jsonName)))

	case "len":
		fmt.Fprintf(b, "if len(%s) != %s {\n\t%s\n}\n", ref, v.Value,
			onErr(fmt.Sprintf("&ggen.LenError{Path: []string{%q}, Want: %s, Got: len(%s)}", jsonName, v.Value, ref)))
	case "minlen":
		fmt.Fprintf(b, "if len(%s) < %s {\n\t%s\n}\n", ref, v.Value,
			onErr(fmt.Sprintf("&ggen.MinLenError{Path: []string{%q}, Limit: %s, Got: len(%s)}", jsonName, v.Value, ref)))
	case "maxlen":
		fmt.Fprintf(b, "if len(%s) > %s {\n\t%s\n}\n", ref, v.Value,
			onErr(fmt.Sprintf("&ggen.MaxLenError{Path: []string{%q}, Limit: %s, Got: len(%s)}", jsonName, v.Value, ref)))

	case "gt":
		fmt.Fprintf(b, "if %s <= %s {\n\t%s\n}\n", ref, v.Value,
			onErr(fmt.Sprintf("&ggen.GTError{Path: []string{%q}, Limit: %s, Value: %s}", jsonName, numBound(kind, v.Value), ref)))
	case "gte":
		fmt.Fprintf(b, "if %s < %s {\n\t%s\n}\n", ref, v.Value,
			onErr(fmt.Sprintf("&ggen.GTEError{Path: []string{%q}, Limit: %s, Value: %s}", jsonName, numBound(kind, v.Value), ref)))
	case "lt":
		fmt.Fprintf(b, "if %s >= %s {\n\t%s\n}\n", ref, v.Value,
			onErr(fmt.Sprintf("&ggen.LTError{Path: []string{%q}, Limit: %s, Value: %s}", jsonName, numBound(kind, v.Value), ref)))
	case "lte":
		fmt.Fprintf(b, "if %s > %s {\n\t%s\n}\n", ref, v.Value,
			onErr(fmt.Sprintf("&ggen.LTEError{Path: []string{%q}, Limit: %s, Value: %s}", jsonName, numBound(kind, v.Value), ref)))

	case "eq":
		if kind == model.KindString {
			fmt.Fprintf(b, "if %s != %q {\n\t%s\n}\n",
				ref, v.Value,
				onErr(fmt.Sprintf("&ggen.EqError{Path: []string{%q}, Want: %q, Value: %s}", jsonName, v.Value, ref)))
		} else if model.IsNumeric(kind) {
			fmt.Fprintf(b, "if %s != %s {\n\t%s\n}\n",
				ref, v.Value,
				onErr(fmt.Sprintf("&ggen.EqError{Path: []string{%q}, Want: %s, Value: %s}", jsonName, numBound(kind, v.Value), ref)))
		}
	case "neq":
		if kind == model.KindString {
			fmt.Fprintf(b, "if %s == %q {\n\t%s\n}\n",
				ref, v.Value,
				onErr(fmt.Sprintf("&ggen.NeqError{Path: []string{%q}, Want: %q, Value: %s}", jsonName, v.Value, ref)))
		} else if model.IsNumeric(kind) {
			fmt.Fprintf(b, "if %s == %s {\n\t%s\n}\n",
				ref, v.Value,
				onErr(fmt.Sprintf("&ggen.NeqError{Path: []string{%q}, Want: %s, Value: %s}", jsonName, numBound(kind, v.Value), ref)))
		}

	case "oneof":
		cases := renderOneofCases(kind, v.Value)
		varName := registerOneOf(model.SplitPipeParts(v.Value))
		fmt.Fprintf(b, "switch %s {\ncase %s:\ndefault:\n\t%s\n}\n",
			ref, cases,
			onErr(fmt.Sprintf("&ggen.OneOfError{Path: []string{%q}, Allowed: %s, Value: %s}", jsonName, varName, ref)))

	case "url":
		fmt.Fprintf(b, "if !ggen.IsURL(%s) {\n\t%s\n}\n", str,
			onErr(fmt.Sprintf("&ggen.URLError{Path: []string{%q}, Value: %s}", jsonName, str)))

	case "alphanum":
		fmt.Fprintf(b, "if !ggen.IsAlphanum(%s) {\n\t%s\n}\n", str,
			onErr(fmt.Sprintf("&ggen.AlphanumError{Path: []string{%q}, Value: %s}", jsonName, str)))
	case "numeric":
		fmt.Fprintf(b, "if !ggen.IsNumeric(%s) {\n\t%s\n}\n", str,
			onErr(fmt.Sprintf("&ggen.NumericError{Path: []string{%q}, Value: %s}", jsonName, str)))
	case "islower":
		fmt.Fprintf(b, "if !ggen.IsLower(%s) {\n\t%s\n}\n", str,
			onErr(fmt.Sprintf("&ggen.LowerError{Path: []string{%q}, Value: %s}", jsonName, str)))
	case "isupper":
		fmt.Fprintf(b, "if !ggen.IsUpper(%s) {\n\t%s\n}\n", str,
			onErr(fmt.Sprintf("&ggen.UpperError{Path: []string{%q}, Value: %s}", jsonName, str)))
	case "hexadecimal":
		fmt.Fprintf(b, "if !ggen.IsHex(%s) {\n\t%s\n}\n", str,
			onErr(fmt.Sprintf("&ggen.HexadecimalError{Path: []string{%q}, Value: %s}", jsonName, str)))

	case "starts":
		fmt.Fprintf(b, "if !strings.HasPrefix(%s, %q) {\n\t%s\n}\n",
			str, v.Value,
			onErr(fmt.Sprintf("&ggen.StartsError{Path: []string{%q}, Want: %q, Value: %s}", jsonName, v.Value, str)))
	case "ends":
		fmt.Fprintf(b, "if !strings.HasSuffix(%s, %q) {\n\t%s\n}\n",
			str, v.Value,
			onErr(fmt.Sprintf("&ggen.EndsError{Path: []string{%q}, Want: %q, Value: %s}", jsonName, v.Value, str)))
	case "contains":
		fmt.Fprintf(b, "if !strings.Contains(%s, %q) {\n\t%s\n}\n",
			str, v.Value,
			onErr(fmt.Sprintf("&ggen.ContainsError{Path: []string{%q}, Want: %q, Value: %s}", jsonName, v.Value, str)))

	case "multiple":
		fmt.Fprintf(b, "if %s %% %s != 0 {\n\t%s\n}\n", ref, v.Value,
			onErr(fmt.Sprintf("&ggen.MultipleError{Path: []string{%q}, Of: %s, Value: %s}", jsonName, numBound(kind, v.Value), ref)))

	default:
		// `@FuncName` is a user-defined validator, resolved at parse time.
		if v.Custom {
			call := v.FuncName
			if v.PkgName != "" {
				call = v.PkgName + "." + v.FuncName
			}
			if v.BoolForm {
				fmt.Fprintf(
					b,
					"if !%s(%s) {\n\t%s\n}\n",
					call,
					ref,
					onErr(
						fmt.Sprintf(
							"&ggen.PredicateError{Path: []string{%q}, Name: %q, Msg: %q, Value: %s}",
							jsonName,
							strings.TrimPrefix(v.Name, "@"),
							v.Msg,
							ref,
						),
					),
				)
			} else {
				fmt.Fprintf(
					b,
					"if err := %s(%s); err != nil {\n\t%s\n}\n",
					call,
					ref,
					onErr(
						fmt.Sprintf("&ggen.CustomError{Path: []string{%q}, Name: %q, Value: %s, Cause: err}", jsonName, strings.TrimPrefix(v.Name, "@"), ref),
					),
				)
			}
		}
		// Unknown non-custom names are silently ignored.
	}
}

func renderOneofCases(kind model.TypeKind, raw string) string {
	parts := model.SplitPipeParts(raw)
	out := make([]string, len(parts))
	for i, p := range parts {
		if kind == model.KindString {
			out[i] = strconv.Quote(p)
		} else {
			out[i] = p
		}
	}
	return strings.Join(out, ", ")
}

// fieldIsConditional reports whether marshaling the field depends on a runtime
// check: an omitempty or omitzero guard, or the entries of an embed catch-all
// map.
func fieldIsConditional(f model.FieldInfo) bool {
	if f.Embed {
		return true
	}
	if !f.OmitEmpty && !f.OmitZero {
		return false
	}
	return fieldSkipExpr(f, "s."+f.GoName) != ""
}

// fieldSkipExpr returns a Go boolean expression that is true when the field
// should be written. omitempty skips a value that encodes as empty JSON.
// omitzero skips a Go zero value. An empty string means "always write".
func fieldSkipExpr(f model.FieldInfo, ref string) string {
	var emitConds []string
	// A named primitive reports KindStruct where it is used. Without resolving
	// it, omitzero would emit `ref != (Score{})`, which does not compile, and
	// omitempty would have no case and silently drop the option.
	kind := effectiveKind(f.GoType, f.Kind)
	if f.OmitEmpty {
		if f.Pointer {
			// nil encodes as null. A pointee that encodes as empty is
			// omitted too.
			emitConds = append(emitConds, ref+" != nil")
			depth, leafType := model.PointerDepth(f.GoType)
			leaf := f
			leaf.Pointer, leaf.PointeeType, leaf.GoType = false, "", leafType
			leaf.Kind = leafKind(f, leafType)
			if c := omitEmptyCond(leaf, derefStr(ref, depth)); c != "" {
				for k := 1; k < depth; k++ {
					emitConds = append(emitConds, derefStr(ref, k)+" != nil")
				}
				emitConds = append(emitConds, c)
			}
		} else if c := omitEmptyCond(f, ref); c != "" {
			emitConds = append(emitConds, c)
		}
	}
	if f.OmitZero {
		if f.Pointer {
			emitConds = append(emitConds, ref+" != nil")
		} else {
			switch kind {
			case model.KindString:
				emitConds = append(emitConds, ref+" != \"\"")
			case model.KindBool:
				emitConds = append(emitConds, ref)
			case model.KindInt, model.KindInt8, model.KindInt16, model.KindInt32, model.KindInt64,
				model.KindUint, model.KindUint8, model.KindUint16, model.KindUint32, model.KindUint64,
				model.KindFloat32, model.KindFloat64:
				emitConds = append(emitConds, ref+" != 0")
			case model.KindSlice, model.KindMap, model.KindRawJSON:
				// The Go zero value is nil. `make([]T, 0)` is not nil and must
				// be written.
				emitConds = append(emitConds, ref+" != nil")
			case model.KindBytes:
				// A [N]byte uses the base64 path, but it is an array and can
				// never be nil.
				if byteArrayLen(f) > 0 {
					emitConds = append(emitConds, zeroCompare(f, ref))
				} else {
					emitConds = append(emitConds, ref+" != nil")
				}
			case model.KindStruct:
				emitConds = append(emitConds, zeroCompare(f, ref))
			case model.KindTime:
				emitConds = append(emitConds, fmt.Sprintf("!%s.IsZero()", ref))
			case model.KindDuration:
				emitConds = append(emitConds, ref+" != 0")
			case model.KindNetIP:
				emitConds = append(emitConds, ref+" != nil")
			case model.KindNetipAddr, model.KindNetipPrefix:
				emitConds = append(emitConds, ref+".IsValid()")
			case model.KindAny:
				emitConds = append(emitConds, ref+" != nil")
			case model.KindSQLNull:
				emitConds = append(emitConds, zeroCompare(f, ref))
			case model.KindURL:
				emitConds = append(emitConds, fmt.Sprintf("%s != (%s{})", ref, f.GoType))
			case model.KindBigInt, model.KindBigFloat, model.KindBigRat:
				// big types are not comparable because they hold unexported
				// slices. Sign() is 0 for the zero value.
				emitConds = append(emitConds, ref+".Sign() != 0")
			case model.KindArray:
				emitConds = append(emitConds, zeroCompare(f, ref))
			}
		}
	}
	if len(emitConds) == 0 {
		return ""
	}
	return strings.Join(slices.Compact(emitConds), " && ")
}

// omitEmptyCond is the omitempty half of fieldSkipExpr for a non-pointer
// field. It returns an expression that is true when ref encodes as something
// other than null, "", [] or {}. It returns "" when the kind can never encode
// as empty, and "false" for a `[0]T`, which always encodes as []. Numbers,
// bools, times, durations and big types always carry a value, so they never
// get a guard. A zero big.Int encodes as `0`, which is not empty.
func omitEmptyCond(f model.FieldInfo, ref string) string {
	//exhaustive:ignore not every kind applies here
	switch effectiveKind(f.GoType, f.Kind) {
	case model.KindString:
		return ref + " != \"\""
	case model.KindBytes:
		// nil encodes as null and empty as "". A [N]byte is N base64 bytes,
		// so it is never empty.
		if byteArrayLen(f) > 0 {
			return ""
		}
		return fmt.Sprintf("len(%s) > 0", ref)
	case model.KindSlice, model.KindMap, model.KindRawJSON:
		return fmt.Sprintf("len(%s) > 0", ref)
	case model.KindArray:
		if f.ArrayLen == 0 {
			return "false"
		}
	case model.KindNetIP:
		return fmt.Sprintf("len(%s) > 0", ref)
	case model.KindNetipAddr, model.KindNetipPrefix:
		return ref + ".IsValid()"
	case model.KindURL:
		return fmt.Sprintf("%s != (%s{})", ref, f.GoType)
	case model.KindAny:
		return fmt.Sprintf("!ggen.AnyIsEmpty(%s)", ref)
	case model.KindSQLNull:
		// A value with Valid false marshals as `null`.
		return ref + ".Valid"
	}
	return ""
}

// zeroCompare emits the omitzero "is not zero" test. A comparable type gets
// `ref != (T{})`. Other types use reflect's IsZero, because `!= (T{})` does
// not compile for a struct that holds a slice or map field.
func zeroCompare(f model.FieldInfo, ref string) string {
	if f.NotComparable {
		return fmt.Sprintf("!reflect.ValueOf(%s).IsZero()", ref)
	}
	return fmt.Sprintf("%s != (%s{})", ref, f.GoType)
}

func renderAppendJSON(b *bytes.Buffer, s model.StructInfo) {
	fmt.Fprintf(b, "func (s %s) AppendJSON(dst []byte) ([]byte, error) {\n", s.Name)
	if s.IsAlias {
		renderAliasAppendJSON(b, s)
		b.WriteString("}\n\n")
		return
	}
	// coalesceConstAppends merges adjacent constant appends after rendering.
	var body bytes.Buffer
	renderAppendJSONBody(&body, s)
	b.WriteString(coalesceConstAppends(body.String()))
	b.WriteString("}\n\n")
}

// coalesceConstAppends merges adjacent append lines that add constant bytes
// into one append. It recognizes both `dst = append(dst, ...)` and the final
// `return append(dst, ...), nil`. One byte is written as a rune literal, more
// bytes as a spread string literal.
func coalesceConstAppends(src string) string {
	lines := strings.Split(src, "\n")
	var out []string
	var pending []byte
	var indent string
	flushAs := func(prefix, suffix string) {
		if len(pending) == 0 {
			return
		}
		out = append(out, indent+prefix+formatAppendArgs(pending)+suffix)
		pending = nil
	}
	flush := func() { flushAs("dst = append(dst, ", ")") }
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if bs, ok := parseConstAppend(trimmed); ok {
			if len(pending) == 0 {
				indent = line[:len(line)-len(trimmed)]
			}
			pending = append(pending, bs...)
			continue
		}
		if bs, ok := parseConstReturnAppend(trimmed); ok {
			if len(pending) == 0 {
				indent = line[:len(line)-len(trimmed)]
			}
			pending = append(pending, bs...)
			flushAs("return append(dst, ", "), nil")
			continue
		}
		flush()
		out = append(out, line)
	}
	flush()
	return strings.Join(out, "\n")
}

// formatAppendArgs renders the bytes as the arguments of an append call. One
// byte becomes a rune literal, more bytes become a spread string literal.
func formatAppendArgs(p []byte) string {
	if len(p) == 1 {
		return strconv.QuoteRune(rune(p[0]))
	}
	return fmt.Sprintf("%q...", string(p))
}

// parseConstAppend recognizes `dst = append(dst, X)` where X is `"…"...`,
// `'a'` or `'a', 'b', …`, all constant bytes. It returns those bytes, or
// false when X is not constant.
func parseConstAppend(line string) ([]byte, bool) {
	const prefix = "dst = append(dst,"
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, ")") {
		return nil, false
	}
	return parseConstAppendArgs(line[len(prefix) : len(line)-1])
}

// parseConstReturnAppend is the variant for the final return. It matches
// `return append(dst, X), nil` and returns X's bytes when X is constant.
func parseConstReturnAppend(line string) ([]byte, bool) {
	const prefix = "return append(dst,"
	const suffix = "), nil"
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, suffix) {
		return nil, false
	}
	return parseConstAppendArgs(line[len(prefix) : len(line)-len(suffix)])
}

func parseConstAppendArgs(args string) ([]byte, bool) {
	rest := strings.TrimSpace(args)
	if before, ok := strings.CutSuffix(rest, "..."); ok {
		s := strings.TrimSpace(before)
		if !strings.HasPrefix(s, `"`) {
			return nil, false
		}
		unq, err := strconv.Unquote(s)
		if err != nil {
			return nil, false
		}
		return []byte(unq), true
	}
	// A list like `'a', 'b', '\n'` cannot be split on `,`, because a literal
	// may itself be a comma. Walk the rune literals by hand and honor `\X`
	// escapes.
	var out []byte
	for len(rest) > 0 {
		if rest[0] != '\'' {
			return nil, false
		}
		i := 1
		for i < len(rest) {
			if rest[i] == '\\' {
				i += 2
				continue
			}
			if rest[i] == '\'' {
				break
			}
			i++
		}
		if i >= len(rest) {
			return nil, false
		}
		unq, err := strconv.Unquote(rest[:i+1])
		if err != nil || len(unq) != 1 {
			return nil, false
		}
		out = append(out, unq[0])
		rest = strings.TrimSpace(rest[i+1:])
		if len(rest) == 0 {
			break
		}
		if rest[0] != ',' {
			return nil, false
		}
		rest = strings.TrimSpace(rest[1:])
	}
	return out, true
}

func renderAppendJSONBody(b *bytes.Buffer, s model.StructInfo) {
	if len(s.Fields) == 0 {
		b.WriteString("return append(dst, '{', '}'), nil")
		return
	}

	// err is declared once up front so all emitters share it. `_ = err`
	// covers structs of plain primitives, where nothing can fail.
	b.WriteString("var err error\n_ = err\n")

	// With no conditional fields, `{` goes into the first field's constant
	// prefix and the other fields get a leading comma.
	anyConditional := slices.ContainsFunc(s.Fields, fieldIsConditional)

	if !anyConditional {
		for i, f := range s.Fields {
			name := escapeJSONName(f.JSONName, f.HTMLEscape)
			prefix := `,"` + name + `":`
			if i == 0 {
				prefix = `{"` + name + `":`
			}
			ref := "s." + f.GoName
			if newPrefix, code, ok := foldLeadingQuote(f, ref, prefix); ok {
				fmt.Fprintf(b, "dst = append(dst, %q...)\n", newPrefix)
				b.WriteString(code)
			} else {
				fmt.Fprintf(b, "dst = append(dst, %q...)\n", prefix)
				renderAppendValue(b, f, ref)
			}
		}
		b.WriteString("return append(dst, '}'), nil")
		return
	}

	// With conditional fields, compare len(dst) with start to know whether a
	// field was already written and a comma is needed.
	b.WriteString("dst = append(dst, '{')\nstart := len(dst)\n")
	for _, f := range s.Fields {
		ref := "s." + f.GoName
		if f.Embed {
			valEmit := inlineValueEmit(f)
			fmt.Fprintf(b, `for k, v := range %[1]s {
if len(dst) > start { dst = append(dst, ',') }
dst = append(dst, '"')
dst = %[2]s(dst, k)
dst = append(dst, ':')
%[3]s}
`, ref, appendStrFn(f.HTMLEscape), valEmit)
			continue
		}
		emit := fieldSkipExpr(f, ref)
		// The guard of a pointer field already checks for nil. Remove one
		// pointer level so renderAppendValue does not emit a dead
		// `if ref == nil` branch inside it.
		av, aref := f, ref
		if emit != "" && f.Pointer {
			av.GoType = strings.TrimPrefix(av.GoType, "*")
			av.Pointer = strings.HasPrefix(av.GoType, "*")
			av.PointeeType = strings.TrimPrefix(av.GoType, "*")
			if !av.Pointer {
				av.PointeeType = ""
			}
			aref = "(*" + ref + ")"
		}
		if emit != "" {
			fmt.Fprintf(b, "if %s {\n", emit)
		}
		b.WriteString("if len(dst) > start { dst = append(dst, ',') }\n")
		prefix := `"` + escapeJSONName(f.JSONName, f.HTMLEscape) + `":`
		if newPrefix, code, ok := foldLeadingQuote(av, aref, prefix); ok {
			fmt.Fprintf(b, "dst = append(dst, %q...)\n", newPrefix)
			b.WriteString(code)
		} else {
			fmt.Fprintf(b, "dst = append(dst, %q...)\n", prefix)
			renderAppendValue(b, av, aref)
		}
		if emit != "" {
			b.WriteString("}\n")
		}
	}
	b.WriteString("return append(dst, '}'), nil")
}

// inlineValueEmit returns the marshal code for the value of one entry of an
// embed catch-all map. The loop variable is `v`. A few element kinds get
// their own code to avoid converting the value to `any`. Everything else goes
// through AppendAny.
func inlineValueEmit(f model.FieldInfo) string {
	//exhaustive:ignore not every kind applies here
	switch f.ElemKind {
	case model.KindString:
		return fmt.Sprintf("dst = append(dst, '\"')\ndst = %s(dst, v)\n", appendStrFn(f.HTMLEscape))
	case model.KindStruct:
		if isGenerated(f.ElemType) {
			return "if dst, err = v.AppendJSON(dst); err != nil { return dst, err }\n"
		}
	case model.KindRawJSON:
		// jsontext.Value and json.RawMessage are copied as is. An empty
		// value becomes `null`, because appending nothing would produce
		// invalid JSON. The field-level raw emitter and AppendAny do the same.
		return "if len(v) == 0 { dst = append(dst, \"null\"...) } else { dst = append(dst, v...) }\n"
	}
	if f.ElemType == "jsontext.Value" {
		return "if len(v) == 0 { dst = append(dst, \"null\"...) } else { dst = append(dst, v...) }\n"
	}
	return fmt.Sprintf("if dst, err = %s(dst, v); err != nil { return dst, err }\n", appendAnyFn(f.HTMLEscape))
}

func renderAppendValue(b *bytes.Buffer, f model.FieldInfo, ref string) {
	// A named primitive appends the underlying value directly, as in
	// `AppendStringNoHTML(dst, string(s.X))`, instead of calling the alias's
	// AppendJSON. This lets a string alias fold its opening quote into the
	// `"key":"` constant (see opt #16 in cmd/ggen/CLAUDE.md). It also keeps an
	// unannotated named type off the json.Marshal fallback. inlineNamedPrim
	// makes sure an alias generated with its own htmlescape or copy flags
	// keeps its own method.
	if prim, kind, ok := inlineNamedPrim(f); ok && !f.Pointer && !f.String {
		renderAppendValue(b, namedPrimInner(f, prim, kind), primCast(f.GoType, kind, ref))
		return
	}
	if f.Pointer {
		// A nil at any pointer level writes `null`. The code is a flat
		// else-if chain with one branch per level. Only a fully non-nil chain
		// reaches the leaf.
		depth, leafType := model.PointerDepth(f.GoType)
		leaf := f
		leaf.Pointer = false
		leaf.PointeeType = ""
		leaf.GoType = leafType
		leaf.Kind = leafKind(f, leafType)
		for k := range depth {
			kw := "if"
			if k > 0 {
				kw = "} else if"
			}
			fmt.Fprintf(b, "%s %s == nil {\n\tdst = append(dst, 'n', 'u', 'l', 'l')\n", kw, derefStr(ref, k))
		}
		b.WriteString("} else {\n\t")
		renderAppendValue(b, leaf, derefStr(ref, depth))
		b.WriteString("}\n")
		return
	}
	if f.String {
		//exhaustive:ignore not every kind applies here
		switch f.Kind {
		case model.KindBool:
			// jsonv2 does not quote a bool for `,string`. It stays bare and
			// falls through to the plain emit.
		case model.KindInt, model.KindInt8, model.KindInt16, model.KindInt32:
			fmt.Fprintf(b, "dst = append(dst, '\"')\ndst = strconv.AppendInt(dst, int64(%s), 10)\ndst = append(dst, '\"')\n", ref)
			return
		case model.KindInt64:
			fmt.Fprintf(b, "dst = append(dst, '\"')\ndst = strconv.AppendInt(dst, %s, 10)\ndst = append(dst, '\"')\n", ref)
			return
		case model.KindUint, model.KindUint8, model.KindUint16, model.KindUint32:
			fmt.Fprintf(b, "dst = append(dst, '\"')\ndst = strconv.AppendUint(dst, uint64(%s), 10)\ndst = append(dst, '\"')\n", ref)
			return
		case model.KindUint64:
			fmt.Fprintf(b, "dst = append(dst, '\"')\ndst = strconv.AppendUint(dst, %s, 10)\ndst = append(dst, '\"')\n", ref)
			return
		case model.KindFloat32:
			fmt.Fprintf(
				b,
				"dst = append(dst, '\"')\nif dst, err = ggen.AppendFloat(dst, float64(%s), 32); err != nil { return dst, err }\ndst = append(dst, '\"')\n",
				ref,
			)
			return
		case model.KindFloat64:
			fmt.Fprintf(
				b,
				"dst = append(dst, '\"')\nif dst, err = ggen.AppendFloat(dst, %s, 64); err != nil { return dst, err }\ndst = append(dst, '\"')\n",
				ref,
			)
			return
		}
		// Any other kind falls through to the plain emit below.
	}
	switch f.Kind {
	case model.KindString:
		fmt.Fprintf(b, "dst = append(dst, '\"')\ndst =%s(dst, %s)\n", appendStrFn(f.HTMLEscape), ref)
	case model.KindBool:
		fmt.Fprintf(b, "dst = strconv.AppendBool(dst, %s)\n", ref)
	case model.KindInt, model.KindInt8, model.KindInt16, model.KindInt32:
		fmt.Fprintf(b, "dst = strconv.AppendInt(dst, int64(%s), 10)\n", ref)
	case model.KindInt64:
		fmt.Fprintf(b, "dst = strconv.AppendInt(dst, %s, 10)\n", ref)
	case model.KindUint, model.KindUint8, model.KindUint16, model.KindUint32:
		fmt.Fprintf(b, "dst = strconv.AppendUint(dst, uint64(%s), 10)\n", ref)
	case model.KindUint64:
		fmt.Fprintf(b, "dst = strconv.AppendUint(dst, %s, 10)\n", ref)
	case model.KindFloat32:
		fmt.Fprintf(b, "if dst, err = ggen.AppendFloat(dst, float64(%s), 32); err != nil { return dst, err }\n", ref)
	case model.KindFloat64:
		fmt.Fprintf(b, "if dst, err = ggen.AppendFloat(dst, %s, 64); err != nil { return dst, err }\n", ref)
	case model.KindStruct:
		if isGenerated(f.GoType) {
			fmt.Fprintf(b, "if dst, err = %s.AppendJSON(dst); err != nil { return dst, err }\n", ref)
		} else {
			b.WriteString(renderCrossPkgStructAppend(f, ref))
		}
	case model.KindSlice, model.KindArray:
		renderAppendSlice(b, f, ref)
	case model.KindMap:
		renderAppendMap(b, f, ref)
	case model.KindBytes:
		renderAppendBytes(b, f, ref)
	case model.KindTime:
		renderAppendTime(b, f, ref)
	case model.KindDuration:
		renderAppendDuration(b, f, ref)
	case model.KindNetIP, model.KindNetipPrefix:
		// Both implement encoding.TextAppender (Go 1.24+). Their text never
		// holds bytes that need escaping. It has no IPv6 zone, because
		// PrefixFrom strips the zone from a prefix.
		fmt.Fprintf(b, `dst = append(dst, '"')
if dst, err = %s.AppendText(dst); err != nil { return dst, err }
dst = append(dst, '"')
`, ref)
	case model.KindNetipAddr:
		// An address with an IPv6 zone may need escaping.
		// ggen.AppendNetipAddr handles it.
		fmt.Fprintf(b, "dst = append(dst, '\"')\ndst = %s(dst, %s)\n", appendNetipAddrFn(f.HTMLEscape), ref)
	case model.KindRawJSON:
		// Write the raw bytes as is, or "null" when empty or nil.
		fmt.Fprintf(b, `if len(%s) == 0 {
	dst = append(dst, 'n', 'u', 'l', 'l')
} else {
	dst = append(dst, %s...)
}
`, ref, ref)
	case model.KindURL:
		fmt.Fprintf(b, "dst = append(dst, '\"')\ndst = %s(dst, %s)\n", appendURLFn(f.HTMLEscape), ref)
	case model.KindBigInt:
		fmt.Fprintf(b, "dst = (&%s).Append(dst, 10)\n", ref)
	case model.KindBigFloat:
		// big.Float is a JSON string, as in jsonv2.
		fmt.Fprintf(b, "dst = append(dst, '\"')\ndst = (&%s).Append(dst, 'g', -1)\ndst = append(dst, '\"')\n", ref)
	case model.KindBigRat:
		// A JSON string "num/denom", or "n" for a whole number, written by
		// AppendText.
		fmt.Fprintf(b, "dst = append(dst, '\"')\nif dst, err = (&%s).AppendText(dst); err != nil { return dst, err }\ndst = append(dst, '\"')\n", ref)
	case model.KindSQLNull:
		if f.SQLNullInner != nil {
			inner := sqlNullInnerField(f)
			fmt.Fprintf(b, "if !%s.Valid {\n\tdst = append(dst, 'n', 'u', 'l', 'l')\n} else {\n\t", ref)
			renderAppendValue(b, inner, ref+".V")
			b.WriteString("}\n")
			return
		}
		spec, ok := model.SQLNullSpec(f.GoType)
		if !ok {
			return
		}
		innerField := model.FieldInfo{Kind: spec.Inner, GoType: spec.Type, Format: f.Format}
		fmt.Fprintf(b, "if !%s.Valid {\n\tdst = append(dst, 'n', 'u', 'l', 'l')\n} else {\n\t", ref)
		renderAppendValue(b, innerField, ref+"."+spec.Field)
		b.WriteString("}\n")
	case model.KindAny:
		fmt.Fprintf(b, "if dst, err = %s(dst, %s); err != nil { return dst, err }\n", appendAnyFn(f.HTMLEscape), ref)
	}
}

// renderAppendBytes emits marshal code for a []byte field in its format.
func renderAppendBytes(b *bytes.Buffer, f model.FieldInfo, ref string) {
	if byteArrayLen(f) > 0 {
		renderAppendBytesValue(b, f, ref)
		return
	}
	// A nil []byte writes null, as stdlib v1 does. Decode turns null back
	// into nil.
	fmt.Fprintf(b, "if %s == nil {\ndst = append(dst, \"null\"...)\n} else {\n", ref)
	renderAppendBytesValue(b, f, ref)
	b.WriteString("}\n")
}

func renderAppendBytesValue(b *bytes.Buffer, f model.FieldInfo, ref string) {
	if byteArrayLen(f) > 0 {
		ref += "[:]" // the encoders take a slice
	}
	switch f.Format {
	case "", "base64":
		fmt.Fprintf(b, "dst = append(dst, '\"')\ndst =base64.StdEncoding.AppendEncode(dst, %s)\ndst =append(dst, '\"')\n", ref)
	case "base64url":
		fmt.Fprintf(b, "dst = append(dst, '\"')\ndst =base64.URLEncoding.AppendEncode(dst, %s)\ndst =append(dst, '\"')\n", ref)
	case "base32":
		fmt.Fprintf(b, "dst = append(dst, '\"')\ndst =base32.StdEncoding.AppendEncode(dst, %s)\ndst =append(dst, '\"')\n", ref)
	case "base32hex":
		fmt.Fprintf(b, "dst = append(dst, '\"')\ndst =base32.HexEncoding.AppendEncode(dst, %s)\ndst =append(dst, '\"')\n", ref)
	case "base16", "hex":
		fmt.Fprintf(b, "dst = append(dst, '\"')\ndst =hex.AppendEncode(dst, %s)\ndst =append(dst, '\"')\n", ref)
	case "array":
		fmt.Fprintf(b, `dst = append(dst, '[')
for i, b := range %s {
	if i > 0 { dst = append(dst, ',') }
	dst = strconv.AppendUint(dst, uint64(b), 10)
}
dst = append(dst, ']')
`, ref)
	default:
		// An unknown format falls back to base64.
		fmt.Fprintf(b, "dst = append(dst, '\"')\ndst =base64.StdEncoding.AppendEncode(dst, %s)\ndst =append(dst, '\"')\n", ref)
	}
}

// timeFormatSize returns the JSONSize byte budget for a `time.Time` field. It
// includes the surrounding quotes. The numeric `unix*` formats have none. A
// custom layout gets len(format)+6, plus 5 more for each layout byte that
// needs escaping.
func timeFormatSize(format string) int {
	switch format {
	case "unix":
		// Sign, 19 digits of seconds, `.` and 9 digits of nanoseconds make
		// 30 bytes. The rest is slack.
		return 32
	case "unixmilli", "unixmicro", "unixnano":
		return sizeInt // int64 digits, no quotes
	case "", "RFC3339Nano":
		return 35 + 2
	case "RFC3339":
		return 25 + 2
	case "ANSIC":
		return 24 + 2
	case "UnixDate":
		return 30 + 2
	case "RubyDate":
		return 30 + 2
	case "RFC822":
		return 21 + 2
	case "RFC822Z":
		return 21 + 2
	case "RFC850":
		return 32 + 2
	case "RFC1123":
		return 31 + 2
	case "RFC1123Z":
		return 31 + 2
	case "Kitchen":
		return 7 + 2
	case "Stamp":
		return 15 + 2
	case "StampMilli":
		return 19 + 2
	case "StampMicro":
		return 22 + 2
	case "StampNano":
		return 25 + 2
	case "DateTime":
		return 19 + 2
	case "DateOnly":
		return 10 + 2
	case "TimeOnly":
		return 8 + 2
	case "Layout":
		return 26 + 2
	}
	// A custom layout's literal characters are copied to the output and then
	// JSON-escaped. Budget the worst case for each byte that needs it: a
	// control byte becomes \uXXXX, which is 6 bytes.
	n := len(format) + 6
	for i := range len(format) {
		if c := format[i]; c == '"' || c == '\\' || c < 0x20 {
			n += 5
		}
	}
	return n
}

// durationFormatSize returns the JSONSize byte budget for a `time.Duration`
// field. Numeric formats write an integer with no quotes. The default
// `units` format writes a quoted "NhNmNs" string.
func durationFormatSize(format string) int {
	switch format {
	case "sec", "milli", "micro", "nano":
		return sizeInt
	}
	return 25 + 2 // "<NhNmNs>" worst case
}

// timeLayoutExpr returns the Go layout expression for a time.Time format. For
// example "RFC3339" gives time.RFC3339, and a custom layout gives a quoted
// string literal. For the unix formats it instead returns the name of the
// time.Time method that gives the number.
func timeLayoutExpr(format string) (layout string, numeric string) {
	switch format {
	case "":
		return "time.RFC3339Nano", ""
	case "unix":
		return "", "Unix"
	case "unixmilli":
		return "", "UnixMilli"
	case "unixmicro":
		return "", "UnixMicro"
	case "unixnano":
		return "", "UnixNano"
	}
	switch format {
	case "Layout", "ANSIC", "UnixDate", "RubyDate",
		"RFC822", "RFC822Z", "RFC850", "RFC1123", "RFC1123Z",
		"RFC3339", "RFC3339Nano", "Kitchen",
		"Stamp", "StampMilli", "StampMicro", "StampNano",
		"DateTime", "DateOnly", "TimeOnly":
		return "time." + format, ""
	}
	return strconv.Quote(format), ""
}

func renderAppendTime(b *bytes.Buffer, f model.FieldInfo, ref string) {
	layout, numeric := timeLayoutExpr(f.Format)
	if numeric != "" {
		// `format:unix` writes a decimal with a fraction so that nanoseconds
		// survive a round trip, as in jsonv2. AppendUnixSeconds works from
		// Unix() and Nanosecond(). float64(UnixNano()) would overflow outside
		// the int64 nanosecond range and lose precision below 100ns. The
		// other unix* formats are whole integers.
		if numeric == "Unix" {
			fmt.Fprintf(b, "dst = ggen.AppendUnixSeconds(dst, %s)\n", ref)
			return
		}
		fmt.Fprintf(b, "dst = strconv.AppendInt(dst, %s.%s(), 10)\n", ref, numeric)
		return
	}
	// AppendFormat copies the non-token characters of a custom layout as is.
	// A layout with `"`, `\` or a control byte would then give invalid or
	// silently wrong JSON between the quotes. So close the string through the
	// same helper the TextAppender sites use, which escapes only when needed.
	// Named time.X constants are fixed ASCII text and keep the fast path.
	if isCustomTimeLayout(f.Format) {
		// The temp name carries the field name, because two custom-layout
		// fields share the scope of the struct body. renderAppendMap names its
		// first-entry flag the same way.
		tmp := "tf" + sanitizeIdent(f.GoName)
		fmt.Fprintf(b, "dst = append(dst, '\"')\n%s := len(dst)\ndst = %s.AppendFormat(dst, %s)\ndst = %s(dst, %s)\n",
			tmp, ref, layout, closeStrFn(f.HTMLEscape), tmp)
		return
	}
	if isRFC3339Layout(f.Format) {
		// RFC 3339 cannot express every time.Time, for example a year outside
		// [0,9999] or a zone hour of 24 or more. The helper returns an error
		// for those instead of writing a string ParseRFC3339 would reject.
		fmt.Fprintf(
			b,
			"dst = append(dst, '\"')\nif dst, err = ggen.AppendRFC3339(dst, %s, %s); err != nil { return dst, err }\ndst = append(dst, '\"')\n",
			ref,
			layout,
		)
		return
	}
	fmt.Fprintf(b, "dst = append(dst, '\"')\ndst = %s.AppendFormat(dst, %s)\ndst = append(dst, '\"')\n", ref, layout)
}

// isRFC3339Layout reports whether format is one of the RFC 3339 layouts,
// which include the default. These encode and decode through the strict ggen
// helpers.
func isRFC3339Layout(format string) bool {
	return format == "" || format == "RFC3339" || format == "RFC3339Nano"
}

// timeParseExpr renders the parse call for a string-layout time field.
func timeParseExpr(format, layout, src string) string {
	if isRFC3339Layout(format) {
		return "ggen.ParseRFC3339(" + src + ")"
	}
	return fmt.Sprintf("time.Parse(%s, %s)", layout, src)
}

// isCustomTimeLayout reports whether format is a layout string written by the
// user, not a named time.X constant or a numeric format. Its text is copied
// to the output and may need JSON escaping.
func isCustomTimeLayout(format string) bool {
	layout, numeric := timeLayoutExpr(format)
	return numeric == "" && !strings.HasPrefix(layout, "time.")
}

func renderAppendDuration(b *bytes.Buffer, f model.FieldInfo, ref string) {
	switch f.Format {
	case "sec":
		// Seconds() is always finite. The error branch never runs but keeps
		// the code uniform.
		fmt.Fprintf(b, "if dst, err = ggen.AppendFloat(dst, %s.Seconds(), 64); err != nil { return dst, err }\n", ref)
	case "milli":
		fmt.Fprintf(b, "dst = strconv.AppendInt(dst, %s.Milliseconds(), 10)\n", ref)
	case "micro":
		fmt.Fprintf(b, "dst = strconv.AppendInt(dst, %s.Microseconds(), 10)\n", ref)
	case "nano":
		fmt.Fprintf(b, "dst = strconv.AppendInt(dst, %s.Nanoseconds(), 10)\n", ref)
	default:
		// "" (no format) and "units". Unknown formats also use units.
		fmt.Fprintf(b, "dst = append(dst, '\"')\ndst =%s(dst, %s.String())\n", appendStrFn(f.HTMLEscape), ref)
	}
}

// structHasAppendFormatTime reports whether any field is written through
// time.Time.AppendFormat, that is, a time field with a non-numeric format.
// renderSize then reserves 64 extra bytes for the slices.Grow call inside the
// stdlib.
func structHasAppendFormatTime(s model.StructInfo) bool {
	for _, f := range s.Fields {
		if f.Kind != model.KindTime {
			continue
		}
		_, numeric := timeLayoutExpr(f.Format)
		if numeric == "" {
			return true
		}
	}
	return false
}

// renderSize emits a JSONSize method. It sums the worst-case number of bytes
// needed to serialize the struct. Each field's share splits into a constant,
// known at generate time and folded into `size := N`, and runtime code. The
// runtime code is collected in a separate buffer during the loop and written
// after the constant.
func renderSize(b *bytes.Buffer, s model.StructInfo) {
	fmt.Fprintf(b, "func (s %s) JSONSize() int {\n", s.Name)
	if s.IsAlias {
		b.WriteString(renderAliasSize(s))
		b.WriteString("}\n\n")
		return
	}
	// The fixed part is the braces, each field's key bytes and the commas.
	// For omitempty and omitzero fields the key and value move out of the
	// constant into a runtime guard.
	fixed := 2 // { and }
	// time.AppendFormat calls slices.Grow(b, max(layout, 64)) on each call.
	// Reserve one shared 64-byte tail so Marshal still allocates only once.
	if structHasAppendFormatTime(s) {
		fixed += 64
	}
	named := 0
	var runtime bytes.Buffer
	for _, f := range s.Fields {
		if f.Embed {
			// Embed catch-all map. sizeMapContrib budgets the key, colon and
			// comma of each entry.
			ref := "s." + f.GoName
			_, code := sizeContrib(f, ref)
			runtime.WriteString(code)
			continue
		}
		ref := "s." + f.GoName
		emit := fieldSkipExpr(f, ref)
		// The guard of a pointer field already checks for nil. Pass the
		// dereferenced value so sizeContrib does not emit a second
		// `if ref == nil` inside the guard.
		sizeField, sizeRef := f, ref
		if emit != "" && f.Pointer {
			sizeField.Pointer = false
			if f.PointeeType != "" {
				sizeField.GoType = f.PointeeType
			}
			sizeRef = "(*" + ref + ")"
		}
		n, code := sizeContrib(sizeField, sizeRef)
		if emit == "" {
			fixed += len(escapeJSONName(f.JSONName, f.HTMLEscape)) + 3 // "name":
			if named > 0 {
				fixed++ // comma
			}
			named++
			fixed += n
			runtime.WriteString(code)
			continue
		}
		// A field that may be omitted. The worst case includes a leading
		// comma.
		fmt.Fprintf(&runtime, "if %s {\nsize += %d\n", emit, len(escapeJSONName(f.JSONName, f.HTMLEscape))+4+n)
		runtime.WriteString(code)
		runtime.WriteString("}\n")
	}
	fmt.Fprintf(b, "size := %d\n", fixed)
	_, _ = runtime.WriteTo(b)
	b.WriteString("return size\n}\n\n")
}

// escapeJSONName JSON-escapes a field name once, at generate time, for the
// constant `"name":` prefix. It escapes quotes, backslashes and control
// bytes. With htmlEscape it also escapes <, > and &, because jsonv2 escapes
// names the same way as values. Without this, a tag name holding `"` would
// produce invalid JSON with a nil error.
func escapeJSONName(name string, htmlEscape bool) string {
	var b strings.Builder
	for i := range len(name) {
		c := name[i]
		switch {
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			b.WriteString(`\\`)
		case c < 0x20:
			switch c {
			case '\b':
				b.WriteString(`\b`)
			case '\f':
				b.WriteString(`\f`)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			default:
				fmt.Fprintf(&b, `\u%04x`, c)
			}
		case htmlEscape && (c == '<' || c == '>' || c == '&'):
			fmt.Fprintf(&b, `\u%04x`, c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// appendStrFn returns the runtime helper that writes a JSON string body and
// the closing `"`. By default `<`, `>` and `&` are written as is, as jsonv2
// does. htmlescape switches to the HTML-safe escaper, as encoding/json v1
// does. The caller must already have written the opening `"`.
func appendStrFn(htmlEscape bool) string {
	if htmlEscape {
		return "ggen.AppendString" + simdSuffix
	}
	return "ggen.AppendStringNoHTML" + simdSuffix
}

// closeStrFn returns the runtime helper that closes a JSON string after raw
// text, such as TextAppender output, was appended after the opening quote. It
// escapes the text again only if it holds bytes that need escaping.
func closeStrFn(htmlEscape bool) string {
	if htmlEscape {
		return "ggen.CloseJSONStringHTML"
	}
	return "ggen.CloseJSONString"
}

// appendURLFn and appendNetipAddrFn pick the helper variant for the escape
// mode, so htmlescape works here as it does for every other string.
func appendURLFn(htmlEscape bool) string {
	if htmlEscape {
		return "ggen.AppendURLHTML"
	}
	return "ggen.AppendURL"
}

func appendNetipAddrFn(htmlEscape bool) string {
	if htmlEscape {
		return "ggen.AppendNetipAddrHTML"
	}
	return "ggen.AppendNetipAddr"
}

// emitNoCloseAfterComma emits the bytes-path guard in the comma branch of an
// element loop. A closing bracket or brace, or the end of input, right after a
// comma is invalid JSON.
func emitNoCloseAfterComma(b *bytes.Buffer, field, posVar string, closer byte) {
	sentinel := "ggen.ErrBadArray"
	if closer == '}' {
		sentinel = "ggen.ErrBadObject"
	}
	fmt.Fprintf(
		b,
		"if %[1]s >= len(data) || data[%[1]s] == '%[2]c' { return result, %[1]s, ggen.NewParseErr(%[4]s, %[1]s, %[3]s) }\n",
		posVar,
		closer,
		sentinel,
		field,
	)
}

// streamNoCloseAfterComma is the stream version of emitNoCloseAfterComma. The
// `s.Pos >= len(...)` check also catches the end of input right after the
// comma. SkipSpace returns nil at the end of input, and the top of the loop
// would otherwise index out of range.
func streamNoCloseAfterComma(field string, closer byte) string {
	sentinel := "ggen.ErrBadArray"
	if closer == '}' {
		sentinel = "ggen.ErrBadObject"
	}
	return fmt.Sprintf(
		"if s.Pos >= len(s.Bytes()) || s.Bytes()[s.Pos] == '%c' { return result, ggen.NewParseErr(%s, s.Offset(), %s) }\n",
		closer,
		field,
		sentinel,
	)
}

// appendAnyFn is appendStrFn for `any` values. An htmlescape struct writes its
// `any` values through the HTML-safe variant, so nested strings are escaped
// the same way as its string fields.
func appendAnyFn(htmlEscape bool) string {
	if htmlEscape {
		return "ggen.AppendAnyHTML"
	}
	return "ggen.AppendAny"
}

// anySizeFn is the JSONSize counterpart of appendAnyFn.
func anySizeFn(htmlEscape bool) string {
	if htmlEscape {
		return "ggen.AnySizeHTML"
	}
	return "ggen.AnySize"
}

// foldLeadingQuote checks whether the field's value code starts by writing a
// `"`. If so, it returns the prefix with `"` added and the value code without
// its opening quote. This saves one append per field.
func foldLeadingQuote(f model.FieldInfo, ref, prefix string) (newPrefix, code string, ok bool) {
	if f.Pointer {
		return prefix, "", false // pointer may emit "null"
	}
	//exhaustive:ignore not every kind applies here
	switch f.Kind {
	case model.KindString:
		return prefix + `"`, fmt.Sprintf("dst = %s(dst, %s)\n", appendStrFn(f.HTMLEscape), ref), true
	case model.KindNetIP, model.KindNetipPrefix:
		return prefix + `"`, fmt.Sprintf(`if dst, err = %s.AppendText(dst); err != nil { return dst, err }
dst = append(dst, '"')
`, ref), true
	case model.KindNetipAddr:
		// An address with an IPv6 zone may need escaping.
		// ggen.AppendNetipAddr handles it.
		return prefix + `"`, fmt.Sprintf("dst = %s(dst, %s)\n", appendNetipAddrFn(f.HTMLEscape), ref), true
	case model.KindBytes:
		// No fold. A nil []byte writes `null`, which has no opening quote.
	case model.KindURL:
		// AppendURL writes the body and the closing quote. It escapes when
		// RawQuery, Opaque or Host hold bytes a JSON string cannot hold raw.
		return prefix + `"`, fmt.Sprintf("dst = %s(dst, %s)\n", appendURLFn(f.HTMLEscape), ref), true
	case model.KindBigRat:
		return prefix + `"`, fmt.Sprintf("if dst, err = (&%s).AppendText(dst); err != nil { return dst, err }\ndst = append(dst, '\"')\n", ref), true
	case model.KindBigFloat:
		return prefix + `"`, fmt.Sprintf("dst = (&%s).Append(dst, 'g', -1)\ndst = append(dst, '\"')\n", ref), true
	}
	return prefix, "", false
}

// Worst-case byte budgets for primitive encodings.
const (
	sizeBool  = 5  // "false"
	sizeInt   = 20 // "-9223372036854775808"
	sizeUint  = 20 // "18446744073709551615"
	sizeFloat = 25 // shortest round-trip 'f' form: sign, "0.", 5 zeros, 17 digits
	// sizeStrMult covers the short escapes \n, \" and \\, which double a
	// byte. Other control bytes grow 6 times (\u0001). The decoder rejects
	// them raw, but a \u0001 escape decodes to one, so a string holding them
	// can outgrow the budget and make AppendJSON grow its buffer once.
	sizeStrMult     = 2
	sizeStrMultHTML = 6 // htmlescape writes <, > and & as \uXXXX, 6 bytes
	sizeStrPad      = 2 // surrounding quotes
)

// strMult returns the per-byte string size multiplier for the given
// HTML-escape mode.
func strMult(htmlEscape bool) int {
	if htmlEscape {
		return sizeStrMultHTML
	}
	return sizeStrMult
}

// sizeContrib returns the byte count known at generate time and the runtime
// statements that add the variable part. At the top level the constant is
// folded into the initial `size := N`. The runtime code is emitted as is.
func sizeContrib(f model.FieldInfo, ref string) (int, string) {
	if f.Pointer {
		// A nil at any level budgets `null`, 4 bytes. A fully non-nil chain
		// budgets the leaf. The code is a flat else-if chain with one branch
		// per level.
		depth, leafType := model.PointerDepth(f.GoType)
		leaf := f
		leaf.Pointer = false
		leaf.PointeeType = ""
		leaf.GoType = leafType
		leaf.Kind = leafKind(f, leafType)
		leafN, leafCode := sizeContrib(leaf, derefStr(ref, depth))
		b := getSmall()
		defer putSmall(b)
		for k := range depth {
			kw := "if"
			if k > 0 {
				kw = " else if"
			}
			fmt.Fprintf(b, "%s %s == nil { size += 4 }", kw, derefStr(ref, k))
		}
		b.WriteString(" else {\n")
		if leafN > 0 {
			fmt.Fprintf(b, "size += %d\n", leafN)
		}
		b.WriteString(leafCode)
		b.WriteString("}\n")
		return 0, b.String()
	}
	// json:",string" adds two quote bytes that the kind's budget leaves out.
	// KindBool is excluded, because jsonv2 writes a bare bool even with
	// `,string`.
	if f.String {
		//exhaustive:ignore not every kind applies here
		switch f.Kind {
		case model.KindInt, model.KindInt8, model.KindInt16, model.KindInt32, model.KindInt64,
			model.KindUint, model.KindUint8, model.KindUint16, model.KindUint32, model.KindUint64,
			model.KindFloat32, model.KindFloat64:
			n, code := sizeContribKind(f, ref)
			return n + 2, code
		}
	}
	return sizeContribKind(f, ref)
}

func sizeContribKind(f model.FieldInfo, ref string) (int, string) {
	switch f.Kind {
	case model.KindString:
		return sizeStrPad, fmt.Sprintf("size += len(%s)*%d\n", ref, strMult(f.HTMLEscape))
	case model.KindBool:
		return sizeBool, ""
	case model.KindInt, model.KindInt8, model.KindInt16, model.KindInt32, model.KindInt64:
		return sizeInt, ""
	case model.KindUint, model.KindUint8, model.KindUint16, model.KindUint32, model.KindUint64:
		return sizeUint, ""
	case model.KindFloat32, model.KindFloat64:
		return sizeFloat, ""
	case model.KindStruct:
		// A named primitive budgets the underlying type instead of calling
		// JSONSize(), because the value emitter writes the primitive inline
		// (see renderAppendValue).
		if prim, kind, ok := inlineNamedPrim(f); ok {
			return sizeContribKind(namedPrimInner(f, prim, kind), primCast(f.GoType, kind, ref))
		}
		if isGenerated(f.GoType) || f.Iface.JSONSize {
			return 0, fmt.Sprintf("size += %s.JSONSize()\n", ref)
		}
		return 128, ""
	case model.KindSlice, model.KindArray:
		return sizeSliceContrib(f, ref, 0)
	case model.KindMap:
		return sizeMapContrib(f, ref)
	case model.KindBytes:
		// The constant 4 covers the brackets or quotes, and also a nil value
		// written as null.
		switch f.Format {
		case "array":
			// Each byte is at most 3 digits plus a comma, so 4 bytes.
			return 4, fmt.Sprintf("size += len(%s)*4\n", ref)
		case "base16", "hex":
			return 4, fmt.Sprintf("size += len(%s)*2\n", ref)
		case "base32", "base32hex":
			return 4, fmt.Sprintf("size += ((len(%s)+4)/5)*8\n", ref)
		}
		// base64, the default, and base64url.
		return 4, fmt.Sprintf("size += ((len(%s)+2)/3)*4\n", ref)
	case model.KindTime:
		return timeFormatSize(f.Format), ""
	case model.KindDuration:
		return durationFormatSize(f.Format), ""
	case model.KindNetIP:
		// ParseIP returns 16 bytes even for IPv4. To4() is non-nil only for
		// addresses that fit in 4 bytes.
		return 2, fmt.Sprintf("if %s.To4() != nil { size += 15 } else if len(%s) != 0 { size += 39 } else { size += 2 }\n", ref, ref)
	case model.KindNetipAddr:
		// An IPv6 zone adds the '%' separator plus the zone text, budgeted
		// with the string multiplier (2, or 6 with htmlescape). A raw control
		// byte can exceed that, the same known gap as in the string budget.
		return 2, fmt.Sprintf(
			"if %[1]s.Is4() { size += 15 } else { size += 39 }\nif z := len(%[1]s.Zone()); z > 0 { size += 1 + z*%[2]d }\n",
			ref,
			strMult(f.HTMLEscape),
		)
	case model.KindNetipPrefix:
		// The address plus /N. The worst case "/128" adds 4.
		return 2, fmt.Sprintf("if %s.Addr().Is4() { size += 19 } else { size += 43 }\n", ref)
	case model.KindRawJSON:
		return 0, fmt.Sprintf("if n := len(%s); n > 0 { size += n } else { size += 4 }\n", ref)
	case model.KindURL:
		// Sum of the components. The constant 8 covers the quotes, `://`, `?`
		// and `#`. Decoded parts such as Path, Fragment and the user info are
		// multiplied by 3 for percent-escaping.
		return 8, fmt.Sprintf(
			"size += len(%s.Scheme) + len(%s.Host)*3 + len(%s.Path)*3 + len(%s.RawQuery)*2 + len(%s.Fragment)*3 + len(%s.Opaque)*2\nif %s.User != nil { pw, _ := %s.User.Password(); size += (len(%s.User.Username()) + len(pw))*3 + 2 }\n",
			ref,
			ref,
			ref,
			ref,
			ref,
			ref,
			ref,
			ref,
			ref,
		)
	case model.KindBigInt:
		// log10(2^bits) is about bits/3. The constant covers the sign and some
		// slack.
		return 4, fmt.Sprintf("size += %s.BitLen()/3\n", ref)
	case model.KindBigFloat:
		// With 'g' and -1 the digit count grows with the mantissa precision,
		// which the user can raise with SetPrec. It is about
		// Prec()*log10(2), which is at most Prec()/3. A flat constant would be
		// too small for high precision and Marshal would allocate twice. The
		// constant 24 covers the sign, '.', "e-123456789" and the quotes.
		return 24, fmt.Sprintf("size += int(%s.Prec())/3\n", ref)
	case model.KindBigRat:
		return 8, fmt.Sprintf("size += (%s.Num().BitLen() + %s.Denom().BitLen())/3\n", ref, ref)
	case model.KindSQLNull:
		if f.SQLNullInner != nil {
			inner := sqlNullInnerField(f)
			innerN, code := sizeContrib(inner, ref+".V")
			return max(innerN, 4), code // room for "null" when !Valid
		}
		spec, ok := model.SQLNullSpec(f.GoType)
		if !ok {
			return 0, ""
		}
		innerField := model.FieldInfo{Kind: spec.Inner, GoType: spec.Type, Format: f.Format}
		innerN, code := sizeContrib(innerField, ref+"."+spec.Field)
		// Leave room for "null" (4 bytes) when !Valid. An inner budget below
		// 4 would be too small otherwise.
		return max(innerN, 4), code
	case model.KindAny:
		return 0, fmt.Sprintf("size += %s(%s)\n", anySizeFn(f.HTMLEscape), ref)
	}
	return 0, ""
}

// sizeSliceContrib emits the size contribution for a slice or array field.
// The brackets are returned as a constant for the caller to fold. The
// per-element sizes go into the runtime code. Nested slices recurse with a
// loop variable named after the depth, so names do not collide.
func sizeSliceContrib(f model.FieldInfo, ref string, depth int) (int, string) {
	ivar := fmt.Sprintf("i%d", depth)
	b := getSmall()
	defer putSmall(b)
	fmt.Fprintf(b, "if n := len(%s); n > 0 { size += n - 1 }\n", ref)
	// A named-primitive element is written as its underlying type (see
	// renderAppendSlice). Budget the underlying type instead of calling
	// JSONSize().
	if _, kind, ok := inlineNamedPrim(elemAsField(f)); ok && !f.ElemPointer {
		f.ElemKind = kind
	}
	switch {
	case f.ElemPointer:
		// Pointer element at any depth. sizeContrib budgets `null` for a nil
		// at any level and the dereferenced leaf otherwise.
		leafN, leafCode := sizeContrib(elemPtrField(f, f.JSONName+"[]"), fmt.Sprintf("%s[%s]", ref, ivar))
		fmt.Fprintf(b, "for %s := range %s {\n", ivar, ref)
		if leafN > 0 {
			fmt.Fprintf(b, "size += %d\n", leafN)
		}
		b.WriteString(leafCode)
		b.WriteString("}\n")
	case f.ElemKind == model.KindString:
		fmt.Fprintf(b, "for %s := range %s { size += len(%s[%s])*%d + %d }\n",
			ivar, ref, ref, ivar, strMult(f.HTMLEscape), sizeStrPad)
	case f.ElemKind == model.KindBool:
		fmt.Fprintf(b, "size += len(%s) * %d\n", ref, sizeBool)
	case f.ElemKind == model.KindInt, f.ElemKind == model.KindInt64, f.ElemKind == model.KindInt8, f.ElemKind == model.KindInt16, f.ElemKind == model.KindInt32:
		fmt.Fprintf(b, "size += len(%s) * %d\n", ref, sizeInt)
	case f.ElemKind == model.KindUint,
		f.ElemKind == model.KindUint64,
		f.ElemKind == model.KindUint8,
		f.ElemKind == model.KindUint16,
		f.ElemKind == model.KindUint32:
		fmt.Fprintf(b, "size += len(%s) * %d\n", ref, sizeUint)
	case f.ElemKind == model.KindFloat32, f.ElemKind == model.KindFloat64:
		fmt.Fprintf(b, "size += len(%s) * %d\n", ref, sizeFloat)
	case f.ElemKind == model.KindStruct:
		if isGenerated(f.ElemType) || f.ElemIface.JSONSize {
			fmt.Fprintf(b, "for %s := range %s { size += %s[%s].JSONSize() }\n",
				ivar, ref, ref, ivar)
		} else {
			fmt.Fprintf(b, "size += len(%s) * 128\n", ref)
		}
	case f.ElemKind == model.KindSlice, f.ElemKind == model.KindArray:
		fmt.Fprintf(b, "for %s := range %s {\n", ivar, ref)
		innerN, innerCode := sizeSliceContrib(peelSliceField(f), fmt.Sprintf("%s[%s]", ref, ivar), depth+1)
		if innerN > 0 {
			fmt.Fprintf(b, "size += %d\n", innerN)
		}
		b.WriteString(innerCode)
		b.WriteString("}\n")
	default:
		// An element with a dedicated kind uses the same budget as a field
		// of that kind.
		elemN, elemCode := sizeContrib(sliceElemField(f), fmt.Sprintf("%s[%s]", ref, ivar))
		if elemCode == "" {
			fmt.Fprintf(b, "size += len(%s) * %d\n", ref, elemN)
		} else {
			fmt.Fprintf(b, "for %s := range %s {\n", ivar, ref)
			if elemN > 0 {
				fmt.Fprintf(b, "size += %d\n", elemN)
			}
			b.WriteString(elemCode)
			b.WriteString("}\n")
		}
	}
	// A nil slice writes "null", 4 bytes, which is wider than `[]`. Reserve
	// the larger one. Arrays cannot be nil, so they keep 2 bytes for the
	// brackets.
	if f.Kind == model.KindArray {
		return 2, b.String()
	}
	return 4, b.String()
}

// sizeMapContrib emits the size contribution for a `map[string]V` field.
// Each entry `"k":v,` costs 4 fixed bytes, plus the key length times the
// string multiplier, plus the value budget. The comma is counted for the last
// entry too, which keeps it an upper bound.
func sizeMapContrib(f model.FieldInfo, ref string) (int, string) {
	b := getSmall()
	defer putSmall(b)
	const perEntryFixed = 4

	mult := strMult(f.HTMLEscape)
	// When the value has a constant size, move it out of the loop. The loop
	// then only walks the keys.
	_, eDepth := elemPtrType(f)
	if v, ok := constSizePerEntry(f.ElemKind, f.Format); ok && eDepth == 0 {
		fmt.Fprintf(b, "size += len(%s) * %d\n", ref, perEntryFixed+v)
		fmt.Fprintf(b, "for k := range %s { size += len(k) * %d }\n", ref, mult)
		return 4, b.String() // a nil map writes "null", wider than `{}`
	}

	if eDepth > 0 {
		// Pointer value at any depth. sizeContrib budgets `null` for a nil at
		// any level and the dereferenced leaf otherwise.
		leafN, leafCode := sizeContrib(elemPtrField(f, f.JSONName+".value"), "v")
		fmt.Fprintf(b, "size += len(%s) * %d\n", ref, perEntryFixed)
		fmt.Fprintf(b, "for k, v := range %s {\n", ref)
		fmt.Fprintf(b, "size += len(k) * %d\n", mult)
		if leafN > 0 {
			fmt.Fprintf(b, "size += %d\n", leafN)
		}
		b.WriteString(leafCode)
		b.WriteString("}\n")
		return 4, b.String()
	}

	// The value budget comes from sizeContrib, the same function fields and
	// slice elements use. Named primitives resolve inside it. A budget that is
	// only a constant keeps the keys-only loop, so `v` is never declared and
	// left unused.
	vf := sliceElemField(f)
	vf.JSONName = f.JSONName + ".value"
	vN, vCode := sizeContrib(vf, "v")
	if vCode == "" {
		fmt.Fprintf(b, "size += len(%s) * %d\n", ref, perEntryFixed+vN)
		fmt.Fprintf(b, "for k := range %s { size += len(k) * %d }\n", ref, mult)
		return 4, b.String()
	}
	fmt.Fprintf(b, "size += len(%s) * %d\n", ref, perEntryFixed)
	fmt.Fprintf(b, "for k, v := range %s {\n", ref)
	fmt.Fprintf(b, "size += len(k) * %d\n", mult)
	if vN > 0 {
		fmt.Fprintf(b, "size += %d\n", vN)
	}
	b.WriteString(vCode)
	b.WriteString("}\n")
	return 4, b.String() // a nil map writes "null"
}

// constSizePerEntry reports whether a value of the given kind has a fixed
// upper bound on its size, and returns it. format is used for KindTime and
// KindDuration so the budget matches the actual layout. Kinds sized at
// runtime, such as KindAny and KindBigFloat, report false and take the
// per-entry sizeContrib loop.
func constSizePerEntry(kind model.TypeKind, format string) (int, bool) {
	//exhaustive:ignore not every kind applies here
	switch kind {
	case model.KindBool:
		return sizeBool, true
	case model.KindInt, model.KindInt8, model.KindInt16, model.KindInt32, model.KindInt64:
		return sizeInt, true
	case model.KindUint, model.KindUint8, model.KindUint16, model.KindUint32, model.KindUint64:
		return sizeUint, true
	case model.KindFloat32, model.KindFloat64:
		return sizeFloat, true
	case model.KindTime:
		return timeFormatSize(format), true
	case model.KindDuration:
		return durationFormatSize(format), true
	}
	return 0, false
}

// constEmptyElem reports whether a container's element or value is a `[0]T`,
// which always marshals as the constant `[]`. Its code never uses the loop
// variable, so the loop must not declare one. The check reads the element
// type, which is where sliceElemField gets the element's ArrayLen from. Map
// values have no ElemArrayLen.
func constEmptyElem(f model.FieldInfo) bool {
	return f.ElemKind == model.KindArray && !f.ElemPointer && model.ArrayLenFromType(f.ElemType) == 0
}

func renderAppendSlice(b *bytes.Buffer, f model.FieldInfo, ref string) {
	emitAppendSlice(b, f, ref, 0)
}

// emitAppendSlice is the recursive marshal counterpart of emitByteSliceRead.
// Each nesting level removes one [] and names its loop variables after the
// depth. A nil slice writes `null` and an empty non-nil slice writes `[]`, as
// in stdlib v1 and v2. Arrays skip the nil check.
func emitAppendSlice(b *bytes.Buffer, f model.FieldInfo, ref string, depth int) {
	if f.Kind == model.KindArray && f.ArrayLen == 0 {
		// `[0]T`. Writing the first element separately would index a
		// zero-length array.
		b.WriteString("dst = append(dst, \"[]\"...)\n")
		return
	}
	vvar := fmt.Sprintf("v%d", depth)
	if f.Kind == model.KindSlice {
		fmt.Fprintf(b, "if %s == nil {\ndst = append(dst, \"null\"...)\n} else {\n", ref)
	}
	fmt.Fprintf(b, "dst = append(dst, '[')\nif len(%s) > 0 {\n", ref)
	// The first element gets no leading comma. Looping over ref[1:] keeps an
	// `if i > 0` check out of the loop.
	emitSliceElement(b, f, ref+"[0]", depth)
	if constEmptyElem(f) {
		// `[][0]T`. Every element is the constant `[]`, so a named loop
		// variable would be unused.
		fmt.Fprintf(b, "for range %s[1:] {\ndst = append(dst, \",[]\"...)\n", ref)
	} else {
		fmt.Fprintf(b, "for _, %s := range %s[1:] {\ndst = append(dst, ',')\n", vvar, ref)
		emitSliceElement(b, f, vvar, depth)
	}
	b.WriteString("}\n}\ndst = append(dst, ']')\n")
	if f.Kind == model.KindSlice {
		b.WriteString("}\n")
	}
}

// emitSliceElement emits the marshal code for one slice element.
// emitAppendSlice uses it for the first element and for the loop body.
func emitSliceElement(b *bytes.Buffer, f model.FieldInfo, vref string, depth int) {
	// A named-primitive element appends the underlying value, converted
	// where it is read.
	if prim, kind, ok := inlineNamedPrim(elemAsField(f)); ok && !f.ElemPointer {
		nf := f
		nf.ElemType, nf.ElemKind = prim, kind
		emitSliceElement(b, nf, primCast(f.ElemType, kind, vref), depth)
		return
	}
	if f.ElemPointer {
		// A nil at any pointer level writes null. Emit a flat else-if chain,
		// then recurse on the dereferenced leaf.
		_, ptrDepth := elemPtrType(f)
		for k := range ptrDepth {
			kw := "if"
			if k > 0 {
				kw = "} else if"
			}
			fmt.Fprintf(b, "%s %s == nil {\ndst = append(dst, \"null\"...)\n", kw, derefStr(vref, k))
		}
		b.WriteString("} else {\n")
		nf := f
		nf.ElemPointer = false
		_, nf.ElemType = model.PointerDepth(f.ElemType)
		nf.ElemKind = model.ResolveKind(nf.ElemType)
		if nf.ElemKind == model.KindArray {
			nf.ElemArrayLen = model.ArrayLenFromType(nf.ElemType)
		}
		emitSliceElement(b, nf, derefStr(vref, ptrDepth), depth)
		b.WriteString("}\n")
		return
	}
	switch f.ElemKind {
	case model.KindString:
		// Two lines, so coalesceConstAppends merges the `'"'` with the
		// constant before it.
		fmt.Fprintf(b, "dst = append(dst, '\"')\ndst = %s(dst, %s)\n", appendStrFn(f.HTMLEscape), vref)
	case model.KindBool:
		fmt.Fprintf(b, "dst = strconv.AppendBool(dst, %s)\n", vref)
	case model.KindInt, model.KindInt8, model.KindInt16, model.KindInt32:
		fmt.Fprintf(b, "dst = strconv.AppendInt(dst, int64(%s), 10)\n", vref)
	case model.KindInt64:
		fmt.Fprintf(b, "dst = strconv.AppendInt(dst, %s, 10)\n", vref)
	case model.KindUint, model.KindUint8, model.KindUint16, model.KindUint32:
		fmt.Fprintf(b, "dst = strconv.AppendUint(dst, uint64(%s), 10)\n", vref)
	case model.KindUint64:
		fmt.Fprintf(b, "dst = strconv.AppendUint(dst, %s, 10)\n", vref)
	case model.KindFloat32:
		fmt.Fprintf(b, "if dst, err = ggen.AppendFloat(dst, float64(%s), 32); err != nil { return dst, err }\n", vref)
	case model.KindFloat64:
		fmt.Fprintf(b, "if dst, err = ggen.AppendFloat(dst, %s, 64); err != nil { return dst, err }\n", vref)
	case model.KindStruct:
		if isGenerated(f.ElemType) {
			fmt.Fprintf(b, "if dst, err = %s.AppendJSON(dst); err != nil { return dst, err }\n", vref)
		} else {
			// Use the same fallback order as the field level: AppendJSON,
			// MarshalJSON, AppendText, MarshalText, then encoding/json. So an
			// element generated by ggen in another package marshals the way
			// it decodes. The braces matter: the branches that declare temps
			// are emitted once for the first element and once in the loop.
			fmt.Fprintf(b, "{\n%s}\n", renderCrossPkgStructAppend(sliceElemField(f), vref))
		}
	case model.KindSlice, model.KindArray:
		emitAppendSlice(b, peelSliceField(f), vref, depth+1)
	default:
		// An element with a dedicated kind uses the same value emitter as a
		// field of that kind.
		renderAppendValue(b, sliceElemField(f), vref)
	}
}

// renderAppendMap emits marshal code for a map[string]V field. Entries come in
// Go's random map order, so the output order is not stable. A nil map writes
// null and an empty non-nil map writes {}. The first-entry flag is named after
// the field so names do not collide.
func renderAppendMap(b *bytes.Buffer, f model.FieldInfo, ref string) {
	appendStr := appendStrFn(f.HTMLEscape)
	first := "first" + strings.ReplaceAll(f.GoName, ".", "")
	// `map[string][0]T`. Every value is the constant `[]`, so the loop takes
	// only the keys.
	iter := "k, v"
	if constEmptyElem(f) {
		iter = "k"
	}
	fmt.Fprintf(b, `if %[1]s == nil {
dst = append(dst, "null"...)
} else {
dst = append(dst, '{')
%[3]s := true
for %[4]s := range %[1]s {
if %[3]s { %[3]s = false
dst =append(dst, '"') } else { dst = append(dst, ",\""...) }
dst = %[2]s(dst, k)
dst = append(dst, ':')
`, ref, appendStr, first, iter)
	if _, eDepth := elemPtrType(f); eDepth > 0 {
		// Pointer value at any depth. renderAppendValue handles nil and the
		// leaf.
		renderAppendValue(b, elemPtrField(f, f.JSONName+".value"), "v")
		b.WriteString("}\ndst = append(dst, '}')\n}\n")
		return
	}
	// A named-primitive value appends the underlying value. The loop variable
	// `v` has the named type, so it is converted where it is used.
	vref := "v"
	if prim, kind, ok := inlineNamedPrim(elemAsField(f)); ok {
		vref = primCast(f.ElemType, kind, "v")
		f.ElemType, f.ElemKind = prim, kind
	}
	switch f.ElemKind {
	case model.KindString:
		// Two lines, so coalesceConstAppends merges the `'"'` with the `':'`
		// before it.
		fmt.Fprintf(b, "dst = append(dst, '\"')\ndst = %s(dst, %s)\n", appendStr, vref)
	case model.KindBool:
		fmt.Fprintf(b, "dst = strconv.AppendBool(dst, %s)\n", vref)
	case model.KindInt, model.KindInt8, model.KindInt16, model.KindInt32:
		b.WriteString("dst = strconv.AppendInt(dst, int64(v), 10)\n")
	case model.KindInt64:
		fmt.Fprintf(b, "dst = strconv.AppendInt(dst, %s, 10)\n", vref)
	case model.KindUint, model.KindUint8, model.KindUint16, model.KindUint32:
		b.WriteString("dst = strconv.AppendUint(dst, uint64(v), 10)\n")
	case model.KindUint64:
		fmt.Fprintf(b, "dst = strconv.AppendUint(dst, %s, 10)\n", vref)
	case model.KindFloat32:
		b.WriteString("if dst, err = ggen.AppendFloat(dst, float64(v), 32); err != nil { return dst, err }\n")
	case model.KindFloat64:
		fmt.Fprintf(b, "if dst, err = ggen.AppendFloat(dst, %s, 64); err != nil { return dst, err }\n", vref)
	case model.KindStruct:
		if isGenerated(f.ElemType) {
			b.WriteString("if dst, err = v.AppendJSON(dst); err != nil { return dst, err }\n")
		} else {
			// The loop body is its own scope, so no braces are needed. The
			// fallback order matches the field level, so a value generated by
			// ggen in another package marshals the way it decodes.
			b.WriteString(renderCrossPkgStructAppend(sliceElemField(f), vref))
		}
	case model.KindAny:
		fmt.Fprintf(b, "if dst, err = %s(dst, v); err != nil { return dst, err }\n", appendAnyFn(f.HTMLEscape))
	default:
		// A value with a dedicated kind uses the same value emitter as a
		// field of that kind.
		vf := sliceElemField(f)
		vf.JSONName = f.JSONName + ".value"
		renderAppendValue(b, vf, vref)
	}
	b.WriteString("}\ndst = append(dst, '}')\n}\n")
}

// userPreallocHint returns an explicit sizing hint, taken from hint:, len or
// minlen, in that order. Each of them beats the width-based default in
// preallocCap. The tag states what the payload holds, which is better than a
// guess from the element size. It returns -1 when there is no hint, so callers
// can tell a hint of 0 (opt out) from no hint. `maxlen` is not used here. It
// is a bound, not an expected size.
func userPreallocHint(f model.FieldInfo) int {
	if f.HintLen >= 0 {
		return f.HintLen
	}
	if v, ok := f.HasRule("len"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	if v, ok := f.HasRule("minlen"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return -1
}

// mapPreallocCap returns the size for `make(map[K]V, cap)` from the user's
// sizing hints, or 0 when there is none. Maps get no default by kind. makemap
// already delays allocation below 8 entries, and a bigger default would
// over-allocate in the common case.
func mapPreallocCap(f model.FieldInfo) int {
	if n := userPreallocHint(f); n >= 0 {
		return n
	}
	return 0
}

// ownsAllocations reports whether decoding into an existing value of the
// named generated struct reuses any memory. A struct of plain scalars owns
// nothing, because its decode overwrites every field anyway. Reading the old
// value back would then gain nothing and still cost the map swap.
func ownsAllocations(typeName string, seen map[string]struct{}) bool {
	fields, ok := generatedFields[typeName]
	if !ok {
		return false
	}
	if seen == nil {
		seen = map[string]struct{}{}
	}
	if _, cycle := seen[typeName]; cycle {
		return false // a self-reference proves nothing on its own
	}
	seen[typeName] = struct{}{}
	for _, f := range fields {
		if f.Pointer {
			return true
		}
		//exhaustive:ignore not every kind applies here
		switch f.Kind {
		case model.KindSlice, model.KindMap, model.KindBytes, model.KindAny, model.KindRawJSON:
			return true
		case model.KindStruct:
			if isGenerated(f.GoType) && ownsAllocations(f.GoType, seen) {
				return true
			}
		}
	}
	return false
}

// reusesMapValues reports whether f's map decode should decode each entry
// into that entry's previous value. The decode reads the existing map and
// fills a new one. That drops the keys the payload left out without tracking
// which keys it saw. The cost is one map allocation, so it only pays when the
// values own memory worth reusing.
//
// Struct values must be generated by ggen. Their decoder resets the value it
// is given (see opt #74 in cmd/ggen/CLAUDE.md). A value decoded by
// encoding/json or an UnmarshalJSON method would merge into it instead. Slice
// and map values keep their own backing array and are reset where they are
// seeded.
func reusesMapValues(f model.FieldInfo) bool {
	// unknownKey fills the catch-all map, not renderMap. No swap is emitted
	// for it, so its clear() at entry must stay.
	if f.Kind != model.KindMap || f.Embed {
		return false
	}
	// A pointer value reuses its pointee. The decode walks the existing
	// pointer chain instead of allocating a new one per entry. The exception
	// is a leaf that would merge into the old value instead of resetting it.
	if _, d := elemPtrType(f); d > 0 {
		return elemPtrReusable(f)
	}
	//exhaustive:ignore not every kind applies here
	switch f.ElemKind {
	case model.KindSlice, model.KindMap, model.KindBytes:
		return true
	case model.KindRawJSON:
		// A raw span aliases the input unless -copy is on. Aliasing means the
		// value points into the caller's input bytes with no copy. Then the
		// value owns nothing and the swap would only cost time.
		return f.Copy
	case model.KindStruct:
		return isGenerated(f.ElemType) && ownsAllocations(f.ElemType, nil)
	}
	return false
}

// preallocCap returns the initial capacities for a slice field's two backing
// allocations, as Go expressions. The first is for the field's own slice,
// `make([]E, 0, slice)`, where "0" means no prealloc. The second is for the
// contiguous slab of a `[]*T`, `make([]T, 0, slab)`, and is ignored otherwise.
// An explicit hint (hint:, len or minlen) applies to both and beats
// everything else.
//
// With no hint the capacity comes from the element's width, not its kind. It
// is a package-level constant computed from `unsafe.Sizeof(*new(E))` (see
// capForSize), so the compiler folds it to a literal. The rule, which
// internal/prealloc documents and tests, is: as many elements as fit in 80
// bytes, else in 512 bytes, else 1.
//
// `maxlen` is a bound, not a size hint. It only takes over when that many
// elements fit in the 512-byte budget (see capFor).
func preallocCap(f model.FieldInfo) (slice, slab string) {
	if n := userPreallocHint(f); n >= 0 {
		return strconv.Itoa(n), strconv.Itoa(n)
	}
	// `*new(T)` is a zero value of any type, unlike `T{}`, and unsafe.Sizeof
	// never evaluates its operand.
	// `maxlen=N` is an exact upper bound on the element count. When N
	// elements fit the span budget, it beats the width guess. The slice can
	// never outgrow it, and it never allocates more than the payload may
	// hold.
	maxlen := -1
	if v, ok := f.HasRule("maxlen"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxlen = n
		}
	}
	scope := capScope(f)
	elem := capFor(scope, f.ElemType, maxlen)
	if f.ElemPointer {
		// `[]*T`. The slice holds pointers and the slab holds T values.
		return capFor(scope, "*"+f.ElemType, maxlen), elem
	}
	//exhaustive:ignore not every kind applies here
	switch f.ElemKind {
	case model.KindSlice, model.KindMap:
		// The element is a slice header or a map handle. The slab is unused.
		return elem, "0"
	case model.KindStruct, model.KindArray:
		// The width is known at compile time, so even a wide element gets a
		// real, bounded capacity.
		return elem, "0"
	}
	return elem, "0"
}

// capScope names the cap constants a field registers. The name is the struct
// being rendered plus the field's Go name. A top-level container alias has no
// field name, and its struct part is the alias name. Struct names are unique
// in a package, so the names cannot collide across files.
func capScope(f model.FieldInfo) string {
	if f.GoName == "" {
		return currentStructName
	}
	return currentStructName + "_" + sanitizeIdent(f.GoName)
}

var currentStructName string

// capFor returns the cap constant for elemType. With a usable maxlen it
// registers a constant that picks maxlen when that many elements fit the span
// budget, and the width default otherwise.
func capFor(scope, elemType string, maxlen int) string {
	base := capForSize(scope, elemType)
	// Above the budget, the constant below would multiply a huge maxlen into
	// a typed int. That overflows at compile time for wide elements.
	if maxlen < 0 || maxlen > spanBudgetMax {
		return base
	}
	key := fmt.Sprintf("%s\x00%s/%d", scope, elemType, maxlen)
	if name, ok := capRegistry.names[key]; ok {
		return name
	}
	name := capName(scope, fmt.Sprintf("%s_%d", elemType, maxlen))
	capRegistry.names[key] = name
	// fits is 1 when maxlen elements stay within the 512-byte span budget.
	fits := fmt.Sprintf("(1 - min((%d*%s)/(%s+1), 1))", maxlen, sizeExpr(elemType), spanBudgetExpr)
	capRegistry.decls = append(capRegistry.decls, fmt.Sprintf(
		"// %s: prealloc cap for []%s — its maxlen=%d bound when that many\n"+
			"// elements fit a 512-byte span, else the width default.\nconst %s = %s*%d + (1-%s)*%s",
		name, elemType, maxlen, name, fits, maxlen, fits, base))
	return name
}

// capForSize returns the name of a package-level constant that holds the
// default capacity for elemType, based on its width. It registers the
// constant on first use.
//
// It must be a constant expression, not a call to prealloc.Cap. Measured on
// the bench module, gc inlines that helper only into small functions. Most
// emitted sites kept a real call, because the generated DecodeFrom is far over
// the inliner's size budget. A constant expression is folded by the compiler
// before inlining is even considered.
//
// The expression is prealloc.Cap's rule written without branches. sel is 1
// when at least 2 elements fit in 80 bytes, and 0 otherwise:
//
//	S   = max(sizeof(E), 1)          // 1 keeps a zero-width element from dividing by zero
//	A   = 80 / S                     // elements that fit in 80 bytes
//	B   = spanBudget / S             // elements that fit in one span budget
//	sel = min(A, 2) / 2              // 1 when A >= 2, else 0
//	cap = sel*A + (1-sel)*max(B, 1)
func capForSize(scope, elemType string) string {
	key := scope + "\x00" + elemType
	if name, ok := capRegistry.names[key]; ok {
		return name
	}
	name := capName(scope, elemType)
	capRegistry.names[key] = name
	a := fmt.Sprintf("(%d/%s)", fastAllocMax, sizeExpr(elemType))
	b := fmt.Sprintf("(%s/%s)", spanBudgetExpr, sizeExpr(elemType))
	sel := fmt.Sprintf("(min(%s, 2)/2)", a)
	capRegistry.decls = append(capRegistry.decls, fmt.Sprintf(
		"// Tries to fit >2 elements in 80 bytes, then 512 bytes - never goes above that.\n"+
			"const %s = %s*%s + (1-%s)*max(%s, 1)",
		name, sel, a, sel, b))
	return name
}

// fastAllocMax matches internal/prealloc. The emitted constants must follow
// the same rule that the helper documents and tests. It is go1.27's cutoff
// for the size-specialized malloc paths, and has no effect on go1.26. The
// cutoff is inclusive: an 80-byte allocation still takes the fast path (see
// prealloc.Cap). So the tier divides by 80. A 40-byte element then gets 2
// elements in the fast tier.
const fastAllocMax = 80

// spanBudgetExpr is the Go runtime's own size boundary, written so the
// generated code computes it for its own target instead of using this host's
// value. The value is `goarch.PtrSize * goarch.PtrBits`, which is 8 times
// PtrSize squared: 512 on 64-bit and 128 on 32-bit. A hardcoded 512 would
// over-budget a 32-bit target 4 times. The main gain of staying under it is
// that the allocation needs no malloc header. See internal/prealloc.
const spanBudgetExpr = "(8*int(unsafe.Sizeof(uintptr(0)))*int(unsafe.Sizeof(uintptr(0))))"

// spanBudgetMax is spanBudgetExpr's 64-bit value, the largest on any target.
// A maxlen above it cannot fit at any element width.
const spanBudgetMax = 512

func sizeExpr(elemType string) string {
	return fmt.Sprintf("max(int(unsafe.Sizeof(*new(%s))), 1)", elemType)
}

func capName(scope, key string) string {
	return fmt.Sprintf("ggenCap_%s_%s", scope, sanitizeIdent(key))
}

// capRegistry collects the prealloc constants emitted at the top of the file,
// deduplicated by scope and element type. The names carry the struct and
// field (see capScope), so two sources in one package cannot collide.
var capRegistry struct {
	names map[string]string
	decls []string
}

func resetCapRegistry() {
	capRegistry.names = map[string]string{}
	capRegistry.decls = nil
}

// fieldLit returns the JSON name of f as a Go string literal. It is the
// `field` argument passed to ggen.NewParseErr at every error return.
func fieldLit(f model.FieldInfo) string { return strconv.Quote(f.JSONName) }

// inlineSkipWS emits an inline whitespace-skipping loop that mutates posVar
// directly, avoiding the ggen.SkipSpace call overhead.
func inlineSkipWS(b *bytes.Buffer, posVar string) {
	// SIMD tier. SIMD means vector instructions that process 16, 32 or 64
	// bytes with one CPU instruction. Consume one whitespace byte inline.
	// Compact JSON exits after one compare, and a single space between tokens
	// needs no call. A run of 2 or more bytes goes to the vector skipper, so
	// pretty-printed indentation is skipped many bytes at a time.
	if simdSuffix != "" {
		fmt.Fprintf(
			b,
			"if %[1]s < len(data) && data[%[1]s] <= ' ' && (data[%[1]s] == ' ' || data[%[1]s] == '\\t' || data[%[1]s] == '\\n' || data[%[1]s] == '\\r') {\n%[1]s++\nif %[1]s < len(data) && data[%[1]s] <= ' ' { %[1]s = ggen.SkipSpace%[2]s(data, %[1]s) }\n}\n",
			posVar,
			simdSuffix,
		)
		return
	}
	// `data[i] <= ' '` runs before the 4-way test, so compact JSON exits after
	// one compare. The accepted set is the same, because every JSON whitespace
	// byte is <= ' '.
	fmt.Fprintf(b,
		"for %s < len(data) && data[%s] <= ' ' && (data[%s] == ' ' || data[%s] == '\\t' || data[%s] == '\\n' || data[%s] == '\\r') { %s++ }\n",
		posVar, posVar, posVar, posVar, posVar, posVar, posVar)
}

// inlineNullPeek emits an inline `null` check at posVar that advances it 4
// bytes on a match. It leaves the `if` body open for the caller's null branch
// and `} else`. An `n` that does not start `null` is ErrBadLiteral. The stream
// path and the pipe variants report the same error for the same bytes.
func inlineNullPeek(b *bytes.Buffer, posVar, field string) {
	fmt.Fprintf(
		b,
		"if %[1]s < len(data) && data[%[1]s] == 'n' {\nif %[1]s+4 > len(data) || data[%[1]s+1] != 'u' || data[%[1]s+2] != 'l' || data[%[1]s+3] != 'l' { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrBadLiteral) }\n%[1]s += 4\n",
		posVar,
		field,
	)
}

// zeroAssign resets ref before a fallback decoder that merges into its
// target. A plain field is zeroed through its struct's zero value, so the
// code never names the field's type. That type may come from a package this
// file never imports, or have no name at all, such as an anonymous interface.
// Element and pointer-leaf targets use the type with pointers removed, which
// is already named where they are declared.
func zeroAssign(f model.FieldInfo, ref string) string {
	if f.StructName != "" && ref == "result."+f.GoName {
		return fmt.Sprintf("%s = (%s{}).%s\n", ref, f.StructName, f.GoName)
	}
	_, leaf := model.PointerDepth(f.GoType)
	return fmt.Sprintf("%s = *new(%s)\n", ref, leaf)
}

// zeroLit returns the zero-value expression for an element type. It is the
// placeholder appended before an element is decoded in place. Slices and maps
// use `nil`, which is overwritten before anyone reads it. Arrays and structs
// need a `T{}` composite literal.
func zeroLit(elemType string, kind model.TypeKind) string {
	// Pointer types carry the pointee's kind, such as KindInt for `*int`, so
	// the conversion below would emit `*int(0)`. Every pointer zeroes to nil.
	if strings.HasPrefix(elemType, "*") {
		return "nil"
	}
	// A named primitive such as `type Priority string` reports KindStruct
	// where it is used, and `Priority{}` is not a valid literal for it.
	// Resolve the underlying kind and convert the primitive zero instead.
	kind = effectiveKind(elemType, kind)
	var zero string
	switch kind {
	case model.KindString:
		zero = `""`
	case model.KindBool:
		zero = "false"
	case model.KindInt, model.KindInt8, model.KindInt16, model.KindInt32, model.KindInt64,
		model.KindUint, model.KindUint8, model.KindUint16, model.KindUint32, model.KindUint64,
		model.KindFloat32, model.KindFloat64:
		zero = "0"
	case model.KindSlice, model.KindMap:
		return "nil"
	case model.KindAny, model.KindBytes, model.KindRawJSON, model.KindNetIP:
		// `any{}` and composite literals of slice-based kinds are invalid or
		// wasteful. Their zero is nil.
		return "nil"
	case model.KindDuration:
		// time.Duration is a named int64, so `time.Duration{}` does not
		// compile.
		return "0"
	default:
		return elemType + "{}"
	}
	if prim := kindPrimitiveName(kind); elemType != "" && elemType != prim {
		return elemType + "(" + zero + ")"
	}
	return zero
}

// kindNarrowName maps a narrow integer kind to its builtin type name for
// narrowIntGuard. f.GoType may be a named type with the same bounds.
func kindNarrowName(k model.TypeKind) string {
	//exhaustive:ignore not every kind applies here
	switch k {
	case model.KindInt8:
		return "int8"
	case model.KindInt16:
		return "int16"
	case model.KindInt32:
		return "int32"
	case model.KindUint8:
		return "uint8"
	case model.KindUint16:
		return "uint16"
	case model.KindUint32:
		return "uint32"
	}
	return ""
}

// narrowIntBounds returns the min and max bounds, as literals, of an integer
// type narrower than 64 bits. A plain Go conversion to such a type silently
// truncates. ok is false for int, int64, uint and uint64. Those are 64-bit on
// the target platforms, so the scanner's MaxInt64 or MaxUint64 bound already
// covers them.
func narrowIntBounds(typ string) (lo, hi string, ok bool) {
	switch typ {
	case "int8":
		return "math.MinInt8", "math.MaxInt8", true
	case "int16":
		return "math.MinInt16", "math.MaxInt16", true
	case "int32":
		return "math.MinInt32", "math.MaxInt32", true
	case "uint8":
		return "", "math.MaxUint8", true
	case "uint16":
		return "", "math.MaxUint16", true
	case "uint32":
		return "", "math.MaxUint32", true
	}
	return "", "", false
}

// narrowIntGuard emits a range check before the truncating conversion to a
// narrow integer type. An out-of-range value returns ErrNumberOverflow instead
// of silently wrapping, as when 300 stored in a uint8 becomes 44. Both
// encoding/json v1 and jsonv2 reject such values too. wideVar holds the
// scanned int64 or uint64. errRet is the caller's overflow return. The result
// is empty for types that are not narrow. On valid input the check costs one
// well-predicted compare.
func narrowIntGuard(wideVar, typ, errRet string) string {
	lo, hi, ok := narrowIntBounds(typ)
	if !ok {
		return ""
	}
	if lo == "" {
		return fmt.Sprintf("if %s > %s { %s }\n", wideVar, hi, errRet)
	}
	return fmt.Sprintf("if %s < %s || %s > %s { %s }\n", wideVar, lo, wideVar, hi, errRet)
}

// floatScanFn names the runtime scanner for a float kind. float32 has its own
// scanner, ggen.Float32 or Stream.Float32, instead of a float64 scan that is
// then converted. The conversion would round the decimal twice. That can be
// off by one in the last bit when the value lies halfway between two float32
// values, and it rejects some valid values close to MaxFloat32.
func floatScanFn(k model.TypeKind) string {
	if k == model.KindFloat32 {
		return "Float32"
	}
	return "Float64"
}

// foldTailAssign removes a temp that is written once. When body ends with
// `tmp = EXPR` and tmp appears nowhere else in it, the final assignment is
// dropped. EXPR is returned so the caller can place it directly where the
// value is used, such as in a composite literal or a conversion. This removes
// chains like `var nv T; …; nv = float32(fv); {V: nv}`. A multi-value
// assignment at the end (`tmp, i, err = …`) or a writer with branches, such
// as an inline string scan, does not match and keeps its temp.
func foldTailAssign(body, tmp string) (rest, expr string, ok bool) {
	trimmed := strings.TrimRight(body, "\n")
	nl := strings.LastIndexByte(trimmed, '\n')
	last := trimmed[nl+1:]
	if !strings.HasPrefix(last, tmp+" = ") {
		return body, "", false
	}
	rest = trimmed[:nl+1]
	if regexp.MustCompile(`\b` + regexp.QuoteMeta(tmp) + `\b`).MatchString(rest) {
		return body, "", false
	}
	return rest, strings.TrimPrefix(last, tmp+" = "), true
}

// inlineScanInt64 emits an inline signed-integer scanner that assigns to dst,
// through castFn when it is not empty, and advances posVar. It detects
// overflow. field is the JSON path expression for ggen.NewParseErr: a quoted
// literal, `""` at a boundary, or a runtime expression such as `key` in the
// unknown-key handlers.
func inlineScanInt64(b *bytes.Buffer, posVar, dst, castFn, field string) {
	assign := ""
	switch {
	case castFn != "":
		errRet := fmt.Sprintf("return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrNumberOverflow)", posVar, field)
		assign = narrowIntGuard("n", castFn, errRet) + dst + " = " + castFn + "(n)"
	case dst != "n":
		assign = dst + " = n"
	}
	inlineScanInt64Stmt(b, posVar, field, assign)
}

// inlineScanInt64Stmt emits the inline signed-integer scan, then `stmt`, with
// the parsed value in `n` (int64). It adds no braces, so `neg`, `limit`, `u`
// and `n` are declared in the caller's scope. Only one number scan fits in a
// scope.
func inlineScanInt64Stmt(b *bytes.Buffer, posVar, field, stmt string) {
	fmt.Fprintf(b, `neg := false
if %[1]s < len(data) && data[%[1]s] == '-' { neg = true; %[1]s++ }
if %[1]s >= len(data) || data[%[1]s] < '0' || data[%[1]s] > '9' { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrBadNumber) }
if data[%[1]s] == '0' && %[1]s+1 < len(data) && data[%[1]s+1] >= '0' && data[%[1]s+1] <= '9' { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrBadNumber) }
limit := uint64(math.MaxInt64)
if neg { limit = ggen.SignedNeg }
var u uint64
de := %[1]s + 18
if de > len(data) { de = len(data) }
for %[1]s < de && data[%[1]s] >= '0' && data[%[1]s] <= '9' {
	u = u*10 + uint64(data[%[1]s]-'0')
	%[1]s++
}
for %[1]s < len(data) && data[%[1]s] >= '0' && data[%[1]s] <= '9' {
	d := uint64(data[%[1]s]-'0')
	if u > limit/10 || (u == limit/10 && d > limit%%10) {
		return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrNumberOverflow)
	}
	u = u*10 + d
	%[1]s++
}
if %[1]s < len(data) {
	c := data[%[1]s]
	if c == '.' || c == 'e' || c == 'E' { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrBadNumber) }
}
var n int64
if neg {
	if u == ggen.SignedNeg { n = math.MinInt64 } else { n = -int64(u) }
} else {
	n = int64(u)
}
%[3]s
`, posVar, field, stmt)
}

// inlineScanUint64 is the unsigned counterpart of inlineScanInt64.
func inlineScanUint64(b *bytes.Buffer, posVar, dst, castFn, field string) {
	assign := ""
	switch {
	case castFn != "":
		errRet := fmt.Sprintf("return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrNumberOverflow)", posVar, field)
		assign = narrowIntGuard("n", castFn, errRet) + dst + " = " + castFn + "(n)"
	case dst != "n":
		assign = dst + " = n"
	}
	inlineScanUint64Stmt(b, posVar, field, assign)
}

// inlineScanUint64Stmt is the unsigned counterpart of inlineScanInt64Stmt.
// The parsed value is in `n` (uint64) when `stmt` runs. Like
// inlineScanInt64Stmt it adds no braces.
func inlineScanUint64Stmt(b *bytes.Buffer, posVar, field, stmt string) {
	fmt.Fprintf(b, `if %[1]s >= len(data) || data[%[1]s] < '0' || data[%[1]s] > '9' { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrBadNumber) }
if data[%[1]s] == '0' && %[1]s+1 < len(data) && data[%[1]s+1] >= '0' && data[%[1]s+1] <= '9' { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrBadNumber) }
var n uint64
de := %[1]s + 19
if de > len(data) { de = len(data) }
for %[1]s < de && data[%[1]s] >= '0' && data[%[1]s] <= '9' {
	n = n*10 + uint64(data[%[1]s]-'0')
	%[1]s++
}
for %[1]s < len(data) && data[%[1]s] >= '0' && data[%[1]s] <= '9' {
	d := uint64(data[%[1]s]-'0')
	if n > ggen.Uint64Limit/10 || (n == ggen.Uint64Limit/10 && d > ggen.Uint64Limit%%10) {
		return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrNumberOverflow)
	}
	n = n*10 + d
	%[1]s++
}
if %[1]s < len(data) {
	c := data[%[1]s]
	if c == '.' || c == 'e' || c == 'E' { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrBadNumber) }
}
%[3]s
`, posVar, field, stmt)
}

// scalarStringWindow limits the inline byte loop of the scalar-tier string
// scan. A string that runs past this many bytes, or hits an escape or a
// control byte, is handed to ggen.String. Its bytes.IndexByte search uses
// vector instructions, which beats walking a long body one byte at a time.
// The size keeps keys and short values inline while long strings such as
// bios, URLs and descriptions take the vector path.
const scalarStringWindow = 32

// vArg renders the validate argument for string scan calls. It is "false"
// when the struct opted out with allowinvalidutf8, and "true" otherwise.
func vArg(f model.FieldInfo) string {
	if f.AllowInvalidUTF8 {
		return "false"
	}
	return "true"
}

// vArgS is vArg for a whole struct, used by the dispatch key scans.
func vArgS(s model.StructInfo) string {
	if s.AllowInvalidUTF8 {
		return "false"
	}
	return "true"
}

// inlineScanString emits a string reader that assigns to dst and moves posOut
// past the closing quote. The fast path aliases the input with unsafe.String:
// the string points into the caller's bytes and nothing is copied. With cp
// set (-copy mode) it copies with string(...) instead, so the result no
// longer refers to data. Escapes fall back to ggen.String, and under cp the
// result goes through ggen.Detach, which copies only if it still aliases
// data.
//
// The loop also stops on a control byte (below 0x20), which RFC 8259 and
// jsonv2 reject. With validation on, it stops on any non-ASCII byte (0x80 or
// above) too. The inline path does not check UTF-8, so such strings go to
// ggen.String, which does and returns ErrInvalidUTF8 as jsonv2 would. Short
// ASCII strings, the common case, stay inline.
//
// No braces are emitted, so `ke` is declared in the caller's scope.
// renderMap's value scan uses inlineScanStringVar so it does not collide with
// the key scan's `ke`. cp is set only where the string is kept after the
// scan: field values, map keys and values, and slice or array elements.
// Strings that only feed a parser, such as time, url, netip and big values,
// keep aliasing, because the parser's result owns its memory.
func inlineScanString(b *bytes.Buffer, posIn, dst, posOut, field string, cp bool, validate bool) {
	inlineScanStringVar(b, posIn, dst, posOut, field, "ke", cp, validate)
}

// maxJSONNameLen returns the length of the longest declared JSON key on s, or
// 0 when it has no named fields. Embed catch-all maps hold arbitrary keys and
// are left out.
func maxJSONNameLen(s model.StructInfo) int {
	n := 0
	for _, f := range s.Fields {
		if f.Embed {
			continue
		}
		n = max(n, len(f.JSONName))
	}
	return n
}

// inlineScanStringVar is inlineScanString with a name chosen by the caller
// for the local that holds the closing quote position.
func inlineScanStringVar(b *bytes.Buffer, posIn, dst, posOut, field, ke string, cp bool, validate bool) {
	inlineScanStringWin(b, posIn, dst, posOut, field, ke, cp, validate, 0)
}

// inlineScanStringWin is inlineScanStringVar with control over the scalar
// byte window.
//
// On the SIMD tier, window > 0 replaces the inline vector test with a bounded
// scalar loop over the first `window` bytes. The dispatch key scan uses this
// when every declared key is short, because a few predictable scalar steps
// beat the chain of dependent vector instructions. Any other window emits the
// vector form.
//
// On the scalar tier, window < 0 emits an unbounded loop, window == 0 uses
// scalarStringWindow, and window > 0 uses that many bytes.
func inlineScanStringWin(b *bytes.Buffer, posIn, dst, posOut, field, ke string, cp bool, validate bool, window int) {
	// With allowinvalidutf8 the code skips UTF-8 checks: the loops do not
	// stop at bytes >= 0x80, the vector test only looks for control bytes,
	// and the runtime calls get validate=false.
	vLit, win80 := "true", " && data[%[5]s] < 0x80"
	if !validate {
		vLit, win80 = "false", ""
	}
	// SIMD tier. An inline vector test, with no call, handles any string
	// shorter than one vector. A lane is one byte slot inside a vector. The
	// code loads the bytes, compares every lane at once and gets a bitmask:
	// one bit per byte, set where the test matched. TrailingZeros of the mask
	// gives the first byte that is '"', '\', a control byte or non-ASCII.
	// The test (v-0x20) >= 0x60 catches both control bytes and bytes >= 0x80
	// in one step.
	//
	// If that byte is the closing quote, the string is aliased or copied
	// inline. Anything else falls to the ggen.StringAVX* call: an escape, a
	// control byte, non-ASCII that needs UTF-8 checks, a string as long as the
	// vector, or a string so close to the end of input that a full vector
	// load would read past it. The call restarts at posIn, so the errors are
	// exactly the same.
	//
	// Only full-vector loads are emitted, because Load*Part is a real call,
	// not a single instruction. Broadcast constants are emitted at each site.
	// The compiler merges them across sites and moves them out of loops.
	//
	// In cp mode the fast path copies inline with string(data[…]). The
	// fallback calls the same aliasing ggen.StringAVX* function and then
	// ggen.Detach, which copies only when the result aliases data. An escaped
	// string is already a fresh copy, so it is not copied twice.
	if scanStringFn != "ggen.String" {
		lane, vecT, tzFn := 32, "Uint8x32", "TrailingZeros32"
		if scanStringFn == "ggen.StringAVX" {
			lane, vecT, tzFn = 16, "Uint8x16", "TrailingZeros16"
		}
		hot := "unsafe.String(unsafe.SliceData(data[%[1]s+1:]), %[5]s-%[1]s-1)"
		fall := `%[3]s, %[4]s, err = ` + scanStringFn + `(data, %[1]s, ` + vLit + `)
	if err != nil { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, err) }`
		if cp {
			hot = "string(data[%[1]s+1:%[5]s])"
			fall += "\n\t" + `%[3]s = ggen.Detach(%[3]s, data)`
		}
		if window > 0 {
			fmt.Fprintf(b, `if %[1]s >= len(data) || data[%[1]s] != '"' { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrExpectString) }
%[5]s := %[1]s + 1
%[5]sw := %[5]s + %[6]d
if %[5]sw > len(data) { %[5]sw = len(data) }
for %[5]s < %[5]sw && data[%[5]s] != '"' && data[%[5]s] != '\\' && data[%[5]s] >= 0x20`+win80+` { %[5]s++ }
if %[5]s < len(data) && data[%[5]s] == '"' {
	%[3]s = `+hot+`
	%[4]s = %[5]s + 1
} else {
	`+fall+`
}
`, posIn, field, dst, posOut, ke, window)
			return
		}
		classify := `%[5]sD := %[5]sV.Sub(archsimd.Broadcast%[7]s(0x20))
	%[5]sM := %[5]sV.Equal(archsimd.Broadcast%[7]s('"')).Or(%[5]sV.Equal(archsimd.Broadcast%[7]s('\\'))).Or(%[5]sD.Max(archsimd.Broadcast%[7]s(0x60)).Equal(%[5]sD)).ToBits()`
		if !validate {
			// Without validation, test for control bytes only. Bytes >= 0x80
			// stay on the inline path, since nothing needs to check them.
			classify = `%[5]sM := %[5]sV.Equal(archsimd.Broadcast%[7]s('"')).Or(%[5]sV.Equal(archsimd.Broadcast%[7]s('\\'))).Or(%[5]sV.Min(archsimd.Broadcast%[7]s(0x1F)).Equal(%[5]sV)).ToBits()`
		}
		fmt.Fprintf(b, `if %[1]s >= len(data) || data[%[1]s] != '"' { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrExpectString) }
%[5]s := %[1]s + 1
if %[5]s+%[6]d <= len(data) {
	%[5]sV := archsimd.Load%[7]s(data[%[5]s:])
	`+classify+`
	if %[5]sM != 0 { %[5]s += bits.%[8]s(%[5]sM) }
} else {
	for %[5]s < len(data) && data[%[5]s] != '"' && data[%[5]s] != '\\' && data[%[5]s] >= 0x20`+win80+` { %[5]s++ }
}
if %[5]s < len(data) && data[%[5]s] == '"' {
	%[3]s = `+hot+`
	%[4]s = %[5]s + 1
} else {
	`+fall+`
}
`, posIn, field, dst, posOut, ke, lane, vecT, tzFn)
		return
	}
	// Scalar tier. window < 0 emits an unbounded loop. That is for dispatch
	// keys, which are short and matched against known names. A bound would
	// only add setup per key with no long string to hand off, and it measured
	// slower on tiny structs. window >= 0 bounds the inline byte loop. A
	// string that runs past it, or hits an escape or control byte, goes to
	// ggen.String. Its bytes.IndexByte search uses vector instructions and is
	// much faster on long strings. ggen.String decides the errors
	// (ErrUnterminated, ErrBadString). The bounded loop only handles a clean
	// string that ends in a quote. It has the same shape as the SIMD
	// window > 0 template above.
	hot := "unsafe.String(unsafe.SliceData(data[%[1]s+1:]), %[5]s-%[1]s-1)"
	if cp {
		hot = "string(data[%[1]s+1:%[5]s])"
	}
	// Anything other than a clean closing quote falls to ggen.String: the end
	// of input, a control byte, an escape or a byte >= 0x80. ggen.String
	// decides both the error and its position. Returning early here would
	// report the opening quote's offset, unlike the SIMD tier, which always
	// falls back.
	if window < 0 {
		fmt.Fprintf(b, `if %[1]s >= len(data) || data[%[1]s] != '"' { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrExpectString) }
%[5]s := %[1]s + 1
for %[5]s < len(data) && data[%[5]s] != '"' && data[%[5]s] != '\\' && data[%[5]s] >= 0x20`+win80+` { %[5]s++ }
if %[5]s < len(data) && data[%[5]s] == '"' {
	%[3]s = `+hot+`
	%[4]s = %[5]s + 1
} else {
	%[3]s, %[4]s, err = ggen.String(data, %[1]s, `+vLit+`)
	if err != nil { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, err) }
}
`, posIn, field, dst, posOut, ke)
		return
	}
	fall := `%[3]s, %[4]s, err = ggen.String(data, %[1]s, ` + vLit + `)
	if err != nil { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, err) }`
	if cp {
		fall += "\n\t" + `%[3]s = ggen.Detach(%[3]s, data)`
	}
	win := window
	if win == 0 {
		win = scalarStringWindow
	}
	fmt.Fprintf(b, `if %[1]s >= len(data) || data[%[1]s] != '"' { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrExpectString) }
%[5]s := %[1]s + 1
%[5]sw := %[5]s + %[6]d
if %[5]sw > len(data) { %[5]sw = len(data) }
for %[5]s < %[5]sw && data[%[5]s] != '"' && data[%[5]s] != '\\' && data[%[5]s] >= 0x20`+win80+` { %[5]s++ }
if %[5]s < len(data) && data[%[5]s] == '"' {
	%[3]s = `+hot+`
	%[4]s = %[5]s + 1
} else {
	`+fall+`
}
`, posIn, field, dst, posOut, ke, win)
}

// emitReceiverReset emits the container resets at the top of DecodeFrom and
// DecodeFromStream. The decoder decodes into its receiver, so containers must
// be reset before it appends to them, or it would append after the old data.
// Slices and []byte get `[:0]` and maps get `clear`, which keeps the backing
// memory. The reset always runs, so an empty payload gives an empty result
// that still has its capacity. Fixed arrays and pointer fields are skipped.
// emitPointerSeed handles pointer fields.
func emitReceiverReset(b *bytes.Buffer, s model.StructInfo, bytesPath bool) {
	for _, f := range s.Fields {
		if f.Pointer {
			continue // emitPointerSeed empties the leaf it hands over
		}
		ref := "result." + f.GoName
		//exhaustive:ignore not every kind applies here
		switch f.Kind {
		case model.KindSlice, model.KindBytes:
			if byteArrayLen(f) > 0 {
				continue // a fixed array has no header to reset
			}
			fmt.Fprintf(b, "if %[1]s != nil { %[1]s = %[1]s[:0] }\n", ref)
		case model.KindMap:
			if bytesPath && reusesMapValues(f) {
				// The bytes decode reads this map while it fills a new one,
				// so clearing it here would destroy the values it reuses.
				// The stream path does not swap maps, so it still clears.
				continue
			}
			fmt.Fprintf(b, "if %[1]s != nil { clear(%[1]s) }\n", ref)
		}
	}
}

// leafResets reports whether a pointer leaf of kind k and type t can decode
// into memory the receiver already holds. A primitive is fully overwritten by
// its scan, and a generated struct resets itself (see opt #74 in
// cmd/ggen/CLAUDE.md), so neither keeps old data. A value decoded by
// encoding/json or an UnmarshalJSON method would merge into the old value
// instead.
func leafResets(k model.TypeKind, t string) bool {
	return k != model.KindStruct || isGenerated(t)
}

// elemPtrReusable reports whether a multi-level pointer element can decode
// into the pointer chain the receiver already holds in that slot.
func elemPtrReusable(f model.FieldInfo) bool {
	// A multi-level element reports KindStruct and keeps the remaining
	// pointer levels in ElemType. Resolve the leaf before checking it.
	et, _ := elemPtrType(f)
	_, leaf := model.PointerDepth(et)
	return leafResets(model.ResolveKind(leaf), leaf)
}

// emitArraySlotBlank empties a fixed-array slot before its element is decoded
// into it. A generated struct resets itself, and the nested reader resets a
// slice row with [:0]. Every element that merges into what it is given is
// blanked here so the slot decodes fresh. That covers structs decoded by
// encoding/json, maps, and []byte, whose AppendDecode appends. A bytes-path
// map whose values are swapped (see opt #76 in cmd/ggen/CLAUDE.md) reads the
// old entries, so it is handed over as is.
func emitArraySlotBlank(b *bytes.Buffer, f model.FieldInfo, slot string, bytesPath bool) {
	//exhaustive:ignore not every kind applies here
	switch f.ElemKind {
	case model.KindStruct:
		if !isGenerated(f.ElemType) {
			fmt.Fprintf(b, "%s = %s\n", slot, zeroLit(f.ElemType, f.ElemKind))
		}
	case model.KindMap:
		if !bytesPath || !reusesMapValues(sliceElemField(f)) {
			fmt.Fprintf(b, "clear(%s)\n", slot)
		}
	case model.KindBytes:
		if f.ElemArrayLen == 0 {
			fmt.Fprintf(b, "%[1]s = %[1]s[:0]\n", slot)
		}
	}
}

// emitElemGrow grows dst by one slot so the element can be decoded in place.
// A generated struct element resets itself on decode. So when there is spare
// capacity, the grow hands it the old element and its inner allocations are
// reused. Every other element kind is grown with a zero value.
func emitElemGrow(b *bytes.Buffer, dst string, f model.FieldInfo, directStruct bool) {
	if directStruct {
		fmt.Fprintf(b, "if len(%[1]s) < cap(%[1]s) { %[1]s = %[1]s[:len(%[1]s)+1] } else { %[1]s = append(%[1]s, %[2]s) }\n",
			dst, zeroLit(f.ElemType, f.ElemKind))
		return
	}
	fmt.Fprintf(b, "%s = append(%s, %s)\n", dst, dst, zeroLit(f.ElemType, f.ElemKind))
}

// fieldZeroLit is the zero-value expression for a whole field. zeroLit is
// meant for elements and maps every slice-based kind to nil, which a
// `[N]byte` field cannot use.
//
// A type named with a package qualifier is written as the field of a zero
// struct instead, `(T{}).X`. The generated file may not import that package,
// because a value field otherwise only writes `result.X`. Also, a named
// primitive or interface from another package has no composite literal.
func fieldZeroLit(s model.StructInfo, f model.FieldInfo) string {
	if f.Pointer {
		return "nil"
	}
	lit := zeroLit(f.GoType, f.Kind)
	if f.Kind == model.KindBytes && byteArrayLen(f) > 0 {
		lit = f.GoType + "{}"
	}
	// A named slice or map also reads as KindStruct, and `Tags{}` is an
	// empty non-nil value, not its zero.
	if strings.Contains(lit, ".") || (lit == f.GoType+"{}" && f.Kind == model.KindStruct && !f.UnderlyingStruct) {
		return "(" + s.Name + "{})." + f.GoName
	}
	return lit
}

// needsOmittedZero reports whether f must be zeroed when its key never
// appeared. Containers are excluded. emitReceiverReset already emptied them,
// and they keep their capacity for reuse.
func needsOmittedZero(f model.FieldInfo) bool {
	if f.Embed || !needsSeen(f) {
		return false
	}
	if f.Pointer {
		return true
	}
	//exhaustive:ignore not every kind applies here
	switch f.Kind {
	case model.KindSlice, model.KindMap:
		return false
	case model.KindBytes:
		// `[]byte` is a container. `[N]byte` is a value that is not reset at
		// entry.
		return byteArrayLen(f) > 0
	}
	return true
}

// emitOmittedZero zeroes every field the payload did not set, at the end of a
// successful decode. Decoding into a receiver gives the same result as a
// fresh decode. Only container capacity and element allocations are reused.
// Pointers are cleared here instead of at entry, so a key that is present can
// still reuse the old pointer chain.
func emitOmittedZero(b *bytes.Buffer, s model.StructInfo, bytesPath bool) {
	for _, f := range s.Fields {
		// A map that the bytes decode swaps is not cleared at entry. The swap
		// empties it, but the swap only runs when the key is present. So an
		// omitted key must empty it here, or the old entries would survive a
		// payload that never named them.
		if bytesPath && !f.Pointer && f.Kind == model.KindMap && reusesMapValues(f) {
			fmt.Fprintf(b, "if %s { clear(result.%s) }\n", seenNotAccess(s, f), f.GoName)
			continue
		}
		if !needsOmittedZero(f) {
			continue
		}
		fmt.Fprintf(b, "if %s { result.%s = %s }\n", seenNotAccess(s, f), f.GoName, fieldZeroLit(s, f))
	}
}

// renderDecode emits DecodeFrom: a loop that reads each JSON key, jumps to
// that field's scan code and handles ',' and '}'. On valid input it does not
// copy or allocate. Whitespace skipping is inlined at each hot site.
func renderDecode(bOut *bytes.Buffer, s model.StructInfo) {
	// Render into a temp buffer so the SIMD tier can then replace the
	// ggen.SkipValue call. The token is unique, so the rewrite cannot hit
	// anything else. The SIMD skipper skips whitespace runs with vector
	// instructions and scans strings in the same pass.
	var scratch bytes.Buffer
	b := bOut
	if simdSuffix != "" {
		b = &scratch
	}
	defer func() {
		if simdSuffix != "" {
			bOut.WriteString(strings.ReplaceAll(scratch.String(), "ggen.SkipValue(", "ggen.SkipValue"+simdSuffix+"("))
		}
	}()
	// Named results live in the caller's return slot, so every
	// `return result, …` just sets registers and returns, with no struct
	// copy. The `result = recv` line starts from the receiver's value.
	if isCyclic(s.Name) {
		// A self-referential type caps payload nesting at maxDepth (10000)
		// with a core that takes the depth. The public method is a thin
		// wrapper that starts at depth 0, so the API and acyclic callers do
		// not change.
		fmt.Fprintf(b, "func (recv %s) DecodeFrom(data []byte) (%s, int, error) {\n\treturn recv.decodeFromDepth(data, 0)\n}\n\n", s.Name, s.Name)
		fmt.Fprintf(b, "func (recv %s) decodeFromDepth(data []byte, depth int) (result %s, i int, err error) {\n", s.Name, s.Name)
		b.WriteString("result = recv\n")
		b.WriteString("if depth > 10000 { // runtime maxDepth\n\treturn result, 0, ggen.ErrMaxDepth\n}\n")
		renderDecodeBody(b, s)
		return
	}
	fmt.Fprintf(b, "func (recv %s) DecodeFrom(data []byte) (result %s, i int, err error) {\n", s.Name, s.Name)
	b.WriteString("result = recv\n")
	if len(cyclicTypes) == 0 {
		renderDecodeBody(b, s)
		return
	}
	// Only structs that call into a cyclic nested type need `depth` in scope,
	// because decodeCallFor emits `depth+1` there. Render first and check, so
	// the common acyclic struct does not carry an unused constant.
	var rest bytes.Buffer
	renderDecodeBody(&rest, s)
	if strings.Contains(rest.String(), "depth+1") {
		b.WriteString("const depth = 0\n")
	}
	b.Write(rest.Bytes())
}

// renderDecodeBody emits everything after the start of DecodeFrom, which is
// `result = recv` plus the depth check for cyclic types. That is either the
// alias delegation or the full object loop. The cyclic and acyclic forms
// above share it.
func renderDecodeBody(b *bytes.Buffer, s model.StructInfo) {
	if s.IsAlias {
		renderAliasDecode(b, s)
		b.WriteString("}\n\n")
		return
	}
	emitReceiverReset(b, s, true)
	if s.MultiErr {
		b.WriteString("var errs ggen.Errors\n")
	}

	// "Seen" tracking per field, for the required-field checks after the loop
	// and the duplicate-key guard. Narrow structs use one bool per field. Wide
	// structs use a packed bitmask to save stack space and cache.
	if useSeenBitmask(s) {
		b.WriteString(seenDecl(s))
	} else {
		for _, f := range s.Fields {
			if f.Embed {
				continue
			}
			if needsSeen(f) {
				fmt.Fprintf(b, "seen%s := false\n", f.GoName)
			}
		}
	}

	inlineSkipWS(b, "i")
	b.WriteString(`if i >= len(data) || data[i] != '{' { return result, i, ggen.NewParseErr("", i, ggen.ErrBadObject) }
i++
`)
	inlineSkipWS(b, "i")
	b.WriteString("if i < len(data) && data[i] == '}' {\ni++\n")
	renderPostLoop(b, s)
	b.WriteString("return result, i, nil\n}\nfor {\nvar key string\n")
	// The dispatch key is only matched and then dropped. In copy mode the key
	// is copied where it is stored: the embed catch-all map and
	// UnknownKeyError.
	// On the SIMD tier, when every key is 5 bytes or shorter, a predictable
	// scalar loop of at most 5 steps beats the chain of dependent vector
	// instructions. So the key scan gets a bounded scalar window sized to the
	// longest declared key. Longer unknown keys fall to the tier call.
	if maxKey := maxJSONNameLen(s); scanStringFn != "ggen.String" && maxKey <= 5 {
		inlineScanStringWin(b, "i", "key", "i", `""`, "ke", false, !s.AllowInvalidUTF8, maxKey+1)
	} else {
		// Dispatch keys are short and matched against known names. Scan
		// without a bound (window -1) so tiny structs do not pay for window
		// setup on each key. On the SIMD tier this still uses the inline
		// vector test.
		inlineScanStringWin(b, "i", "key", "i", `""`, "ke", false, !s.AllowInvalidUTF8, -1)
	}
	inlineSkipWS(b, "i")
	b.WriteString(`if i >= len(data) || data[i] != ':' { return result, i, ggen.NewParseErr("", i, ggen.ErrBadObject) }
i++
`)
	inlineSkipWS(b, "i")
	renderDispatch(b, s)
	inlineSkipWS(b, "i")
	b.WriteString(`if i >= len(data) { return result, i, ggen.NewParseErr("", i, ggen.ErrBadObject) }
if data[i] == ',' { i++; `)
	inlineSkipWS(b, "i")
	b.WriteString("continue }\nif data[i] == '}' {\ni++\n")
	renderPostLoop(b, s)
	b.WriteString(`return result, i, nil
}
return result, i, ggen.NewParseErr("", i, ggen.ErrBadObject)
}`)
	b.WriteString("}\n\n")
}

// renderPostLoop emits the work done at the end of a parse: zeroing omitted
// fields, the required-field checks when validation is on, and returning the
// collected errors when MultiErr is on. It runs at every successful exit of
// DecodeFrom.
func renderPostLoop(b *bytes.Buffer, s model.StructInfo) {
	renderPostLoopShape(b, s, false)
}

// renderStreamPostLoop is the stream-path counterpart for DecodeFromStream.
// It emits the same checks but returns `return result, X`, with no position,
// because the Stream owns s.Pos.
func renderStreamPostLoop(b *bytes.Buffer, s model.StructInfo) {
	renderPostLoopShape(b, s, true)
}

func renderPostLoopShape(b *bytes.Buffer, s model.StructInfo, stream bool) {
	emitOmittedZero(b, s, !stream)
	retShape := "return result, i, %s"
	errsShape := "if len(errs) > 0 { return result, i, errs }\n"
	posVar := "i"
	if stream {
		retShape = "return result, %s"
		errsShape = "if len(errs) > 0 { return result, errs }\n"
		posVar = ""
	}
	if !s.NoValidate {
		for _, f := range s.Fields {
			if !f.IsRequired() || f.Embed {
				continue
			}
			errExpr := withPos(requiredErr(f.JSONName), posVar)
			notSeen := seenNotAccess(s, f)
			if s.MultiErr {
				fmt.Fprintf(b, "if %s { errs = append(errs, %s) }\n", notSeen, errExpr)
			} else {
				fmt.Fprintf(b, "if %s { "+retShape+" }\n", notSeen, errExpr)
			}
		}
	}
	if s.MultiErr {
		b.WriteString(errsShape)
	}
}

// renderDispatch emits a flat string switch on key. The compiler turns it into
// a binary search grouped by length, or jump tables.
func renderDispatch(b *bytes.Buffer, s model.StructInfo) {
	// emitField wraps a field's parse code with seen tracking and duplicate
	// handling. What happens on a duplicate depends on the mode. AllowDups
	// skips it, so the first value wins. MultiErr records a DuplicateKeyError
	// and skips. The default returns the error at once.
	emitField := func(b *bytes.Buffer, f model.FieldInfo) {
		f.AtDispatch = true
		if f.Embed || !needsSeen(f) {
			renderField(b, f, "result."+f.GoName, "i")
			return
		}
		set := seenSet(s, f)
		seen := seenAccess(s, f)
		chk := bytesErrCheck(fieldLit(f), "i")
		if s.AllowDups {
			fmt.Fprintf(b, `if %s {
	i, err = ggen.SkipValue(data, i)
	%s} else {
	%s`, seen, chk, set)
			renderField(b, f, "result."+f.GoName, "i")
			b.WriteString("}\n")
			return
		}
		if s.MultiErr {
			fmt.Fprintf(b, `if %[1]s {
	errs = append(errs, &ggen.DuplicateKeyError{Pos: i, Path: []string{%[2]q}})
	i, err = ggen.SkipValue(data, i)
	%[3]s} else {
	%[4]s`, seen, f.JSONName, chk, set)
			renderField(b, f, "result."+f.GoName, "i")
			b.WriteString("}\n")
			return
		}
		fmt.Fprintf(b, `if %s { return result, i, &ggen.DuplicateKeyError{Pos: i, Path: []string{%q}} }
%s`, seen, f.JSONName, set)
		renderField(b, f, "result."+f.GoName, "i")
	}

	b.WriteString("switch key {\n")
	for _, f := range s.Fields {
		if f.Embed {
			continue
		}
		fmt.Fprintf(b, "case %q:\n", f.JSONName)
		emitField(b, f)
	}
	b.WriteString("default:\n")
	b.WriteString(unknownKey(s, "i"))
	b.WriteString("}\n")
}

// keyValidateAndMod emits the mods and validators from the `keys:` tag
// section against the map key keyRef. It is for the bytes path, which
// returns three values.
func keyValidateAndMod(b *bytes.Buffer, f model.FieldInfo, keyRef string) {
	if f.NoValidate {
		return
	}
	renderPipe(b, fieldKeyPipe(f), keyRef, f.JSONName+".key", "string", model.KindString, f.MultiErr, "i")
}

// keyValidateAndModStream is the stream-path counterpart of
// keyValidateAndMod. It emits the two-value `return result, err` form.
func keyValidateAndModStream(b *bytes.Buffer, f model.FieldInfo, keyRef string) {
	if f.NoValidate {
		return
	}
	renderPipe(b, fieldKeyPipe(f), keyRef, f.JSONName+".key", "string", model.KindString, f.MultiErr, "")
}

// mapNest tracks how deeply renderMap and renderStreamMap are nested, so each
// level names its own key and value locals. A map value that is itself a map
// comes back in through renderField. Without a suffix the inner level would
// shadow `mk` and `mv`, and its store would become `mv[mk]` on its own value.
// The output for map[string]map[string]V would then not compile.
var mapNest int

// mapLocals returns this nesting level's names for the key, the value, the old
// map and the reuse flag. The outermost level uses the bare names.
func mapLocals() (mk, mv, carried, reuse string) {
	suffix := ""
	if mapNest > 1 {
		suffix = strconv.Itoa(mapNest - 1)
	}
	return "mk" + suffix, "mv" + suffix, "carried" + suffix, "reuse" + suffix
}

// renderMap emits the bytes-path decode of a map[string]V. `null` gives nil
// and an empty `{}` gives a non-nil empty map, as in stdlib. Unless the
// values are reused, emitReceiverReset already cleared a non-nil map, so a
// new map is made only when ref is nil. A map has no addressable entries
// (`&m[k]` is illegal), so struct values decode into a temp that is then
// stored.
func renderMap(b *bytes.Buffer, f model.FieldInfo, ref, posVar string, topLevel bool) {
	mapNest++
	defer func() { mapNest-- }()
	mkVar, mvVar, carriedVar, reuseVar := mapLocals()
	field := fieldLit(f)
	makeExpr := fmt.Sprintf("make(%s)", f.GoType)
	if hint := mapPreallocCap(f); hint > 0 {
		makeExpr = fmt.Sprintf("make(%s, %d)", f.GoType, hint)
	}
	// At dispatch level the null branch breaks out to the comma handling
	// instead of nesting the object read in an else.
	flat := nullBreakOK(f)
	// No leading whitespace skip. The caller already skipped it, and the
	// alias path adds an explicit skip before calling.
	inlineNullPeek(b, posVar, field)
	fmt.Fprintf(b, "%s = nil\n", ref)
	switch {
	case flat:
		b.WriteString("break\n}\n")
	case topLevel:
		// The alias is the whole value, so null returns at once. No else is
		// needed, because the non-null path also returns at the closing
		// brace.
		fmt.Fprintf(b, "return result, %s, nil\n}\n", posVar)
	default:
		b.WriteString("} else {\n")
	}
	fmt.Fprintf(b, `if %[1]s >= len(data) || data[%[1]s] != '{' { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrBadObject) }
	%[1]s++
`, posVar, field)
	inlineSkipWS(b, posVar)
	// Allocate only when nil. A cleared map reuses its buckets. Maps are not
	// sized by counting `:` in the input, because a key full of colons would
	// inflate the count even in valid JSON. Slices can count commas safely.
	if reusesMapValues(f) {
		// Read the old map and fill a new one. Each entry decodes into its
		// previous value, and a key the payload leaves out is never copied
		// over, so no seen-set is needed to drop it. The map is always new
		// here, so the nil checks below are not needed.
		size := "len(" + carriedVar + ")"
		if hint := mapPreallocCap(f); hint > 0 {
			size = fmt.Sprintf("max(len(%s), %d)", carriedVar, hint)
		}
		// `reuse` moves the decision out of the entry loop. A fresh receiver
		// has no map, and a lookup in a nil map is still a real runtime call.
		// The common decode into a zero value must not pay it per entry.
		fmt.Fprintf(b, `	%[4]s := %[1]s
	%[5]s := len(%[4]s) != 0
	%[1]s = make(%[2]s, %[3]s)
`, ref, f.GoType, size, carriedVar, reuseVar)
	} else {
		fmt.Fprintf(b, `	if %[1]s < len(data) && data[%[1]s] == '}' {
		if %[2]s == nil { %[2]s = %[3]s{} }
	} else {
		if %[2]s == nil { %[2]s = %[4]s }
	}
`, posVar, ref, f.GoType, makeExpr)
	}
	fmt.Fprintf(b, `	if %[1]s < len(data) && data[%[1]s] != '}' {
	for {
		var %[5]s string
`, posVar, ref, f.GoType, makeExpr, mkVar)
	inlineScanString(b, posVar, mkVar, posVar, field, f.Copy, !f.AllowInvalidUTF8)
	keyValidateAndMod(b, f, mkVar)
	inlineSkipWS(b, posVar)
	fmt.Fprintf(b, `		if %[1]s >= len(data) || data[%[1]s] != ':' { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrBadObject) }
		%[1]s++
`, posVar, field)
	inlineSkipWS(b, posVar)

	mapTarget := fmt.Sprintf("%s[%s]", ref, mkVar)
	if _, eDepth := elemPtrType(f); eDepth > 0 {
		// Pointer value at any depth. The pointer code parses the leaf first,
		// then assigns into the map slot. With TargetNil it only assigns, so a
		// map index, which is not addressable, works as the target.
		pf := elemPtrField(f, f.JSONName+".value")
		if reusesMapValues(f) {
			// Decode through an addressable local seeded from the old map, so
			// the pointer code reuses that entry's pointer chain. A map index
			// is not addressable, which TargetNil works around below.
			et, _ := elemPtrType(f)
			fmt.Fprintf(b, "{\nvar %[1]s %[2]s\nif %[3]s { %[1]s = %[4]s[%[5]s] }\n", mvVar, et, reuseVar, carriedVar, mkVar)
			renderField(b, pf, mvVar, posVar)
			fmt.Fprintf(b, "%s = %s\n}\n", mapTarget, mvVar)
		} else {
			pf.TargetNil = true
			renderField(b, pf, mapTarget, posVar)
		}
		inlineSkipWS(b, posVar)
		fmt.Fprintf(b, `		if %[1]s < len(data) && data[%[1]s] == ',' { %[1]s++; `, posVar)
		inlineSkipWS(b, posVar)
		emitNoCloseAfterComma(b, field, posVar, '}')
		fmt.Fprintf(b, `			continue }
		break
	}
	}
	if %[1]s >= len(data) || data[%[1]s] != '}' { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrBadObject) }
	%[1]s++
`, posVar, field)
		if topLevel {
			fmt.Fprintf(b, "return result, %s, nil\n", posVar)
		}
		if !flat && !topLevel {
			b.WriteString("}\n") // close the else of the null check
		}
		return
	}
	// A named-primitive value, as in `map[string]Priority`, scans the
	// underlying type into a temp and converts it into the slot. See
	// renderField.
	valCast, valTarget := "", ""
	savedElemType, savedElemKind := f.ElemType, f.ElemKind
	if prim, kind, ok := inlineNamedPrim(elemAsField(f)); ok {
		valCast, valTarget = f.ElemType, mapTarget
		mapTarget = "namedVal"
		fmt.Fprintf(b, "var %s %s\n", mapTarget, prim)
		f.ElemType, f.ElemKind = prim, kind
	}
	switch f.ElemKind {
	case model.KindString:
		// Scan straight into the slot. The end variable is `ve`, because the
		// key scan already declared `ke`.
		inlineScanStringVar(b, posVar, mapTarget, posVar, field, "ve", f.Copy, !f.AllowInvalidUTF8)
	case model.KindBool:
		fmt.Fprintf(b, `%[2]s, %[1]s, err = ggen.Bool(data, %[1]s)
if err != nil { %[1]s = ggen.BoolEnd(data, %[1]s); return result, %[1]s, ggen.NewParseErr(%[3]s, %[1]s, err) }
`, posVar, mapTarget, field)
	case model.KindInt, model.KindInt8, model.KindInt16, model.KindInt32, model.KindInt64:
		castFn := ""
		if f.ElemType != "int64" {
			castFn = f.ElemType
		}
		inlineScanInt64(b, posVar, mapTarget, castFn, field)
	case model.KindUint, model.KindUint8, model.KindUint16, model.KindUint32, model.KindUint64:
		castFn := ""
		if f.ElemType != "uint64" {
			castFn = f.ElemType
		}
		inlineScanUint64(b, posVar, mapTarget, castFn, field)
	case model.KindFloat32, model.KindFloat64:
		fmt.Fprintf(b, `%[2]s, %[1]s, err = ggen.%[4]s(data, %[1]s)
if err != nil { return result, %[1]s, ggen.NewParseErr(%[3]s, %[1]s, err) }
`, posVar, mapTarget, field, floatScanFn(f.ElemKind))
	case model.KindStruct:
		if isGenerated(f.ElemType) {
			// `mv` is both the receiver of the decode and the stored value.
			// `consumed`, the number of bytes read, advances posVar. When the
			// old map is being read, seed from it and not from the map being
			// filled, so a key repeated in one payload decodes fresh each time.
			decl := "var " + mvVar + " " + f.ElemType + "\n"
			if reusesMapValues(f) {
				decl += "if " + reuseVar + " { " + mvVar + " = " + carriedVar + "[" + mkVar + "] }\n"
			}
			fmt.Fprintf(b, decl+`var consumed int
%[5]s, consumed, err = %[5]s.`+decodeCallFor(f.ElemType)+`
%[2]s += consumed
%[4]s%[3]s = %[5]s
`, f.ElemType, posVar, mapTarget, nestedDecodeErrCheck(fieldLit(f), calleeTypeOf(f), f.MultiErr, true, "consumed"), mvVar)
		} else {
			// A value type from another package uses the fallback order: its
			// own DecodeFrom, UnmarshalJSON or UnmarshalText, and encoding/json
			// only as the last choice.
			fmt.Fprintf(b, "var %s %s\n", mvVar, f.ElemType)
			b.WriteString(renderCrossPkgStructDecode(elemAsField(f), mvVar, posVar))
			fmt.Fprintf(b, "%s = %s\n", mapTarget, mvVar)
		}
	default:
		// A value with a dedicated kind, such as time, any, bytes, raw or a
		// slice, decodes into a fresh temp through the field-level emitter
		// and is then stored. The braces matter: the key scan declared `ke` in
		// this scope, and the field emitters declare their own locals.
		vf := sliceElemField(f)
		vf.JSONName = f.JSONName + ".value"
		// A container value has its own backing memory. Seed it from the old
		// map and empty it. The field emitter fills it from there and
		// allocates only when it is nil.
		switch {
		case reusesMapValues(f) && (f.ElemKind == model.KindSlice || f.ElemKind == model.KindBytes):
			// []byte decodes with AppendDecode, so the seed must be emptied,
			// or the decoded bytes would land after the old ones.
			fmt.Fprintf(b, "{\nvar %[1]s %[2]s\nif %[3]s { %[1]s = %[4]s[%[5]s][:0] }\n", mvVar, f.ElemType, reuseVar, carriedVar, mkVar)
		case reusesMapValues(f) && f.ElemKind == model.KindRawJSON:
			// Under -copy, renderRawJSON itself appends to mv[:0].
			fmt.Fprintf(b, "{\nvar %[1]s %[2]s\nif %[3]s { %[1]s = %[4]s[%[5]s] }\n", mvVar, f.ElemType, reuseVar, carriedVar, mkVar)
		case reusesMapValues(f) && f.ElemKind == model.KindMap:
			// An inner level that swaps reads the seed's entries to reuse
			// their values. Only a fill in place needs the seed emptied.
			seed := "{\nvar %[1]s %[2]s\nif %[3]s { %[1]s = %[4]s[%[5]s]; clear(%[1]s) }\n"
			if reusesMapValues(vf) {
				seed = "{\nvar %[1]s %[2]s\nif %[3]s { %[1]s = %[4]s[%[5]s] }\n"
			}
			fmt.Fprintf(b, seed, mvVar, f.ElemType, reuseVar, carriedVar, mkVar)
		default:
			fmt.Fprintf(b, "{\nvar %s %s\n", mvVar, f.ElemType)
		}
		renderField(b, vf, mvVar, posVar)
		fmt.Fprintf(b, "%s = %s\n}\n", mapTarget, mvVar)
	}
	if valCast != "" {
		fmt.Fprintf(b, "%s = %s(%s)\n", valTarget, valCast, mapTarget)
		mapTarget, f.ElemType, f.ElemKind = valTarget, savedElemType, savedElemKind
	}
	// The value's own steps: mods and validators in declared order.
	renderPipe(b, elemSteps(f), mapTarget, f.JSONName+".value", f.ElemType, f.ElemKind, f.MultiErr, "i")
	inlineSkipWS(b, posVar)
	fmt.Fprintf(b, `		if %[1]s < len(data) && data[%[1]s] == ',' { %[1]s++; `, posVar)
	inlineSkipWS(b, posVar)
	emitNoCloseAfterComma(b, field, posVar, '}')
	fmt.Fprintf(b, `			continue }
		break
	}
	}
	if %[1]s >= len(data) || data[%[1]s] != '}' { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrBadObject) }
	%[1]s++
`, posVar, field)
	if topLevel {
		fmt.Fprintf(b, "return result, %s, nil\n", posVar)
	}
	if !flat && !topLevel {
		b.WriteString("}\n") // close the else of the null check
	}
}

// renderBytes emits the decode of a []byte field in base64, hex or array
// form. JSON `null` sets the field to nil, as in stdlib v1 and v2.
func renderBytes(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	if byteArrayLen(f) > 0 {
		// A fixed array is never nil, so it has no `null` form, like every
		// other [N]T.
		renderBytesValue(b, f, ref, posVar)
		return
	}
	inlineNullPeek(b, posVar, fieldLit(f))
	fmt.Fprintf(b, "%s = nil\n", ref)
	if nullBreakOK(f) {
		b.WriteString("break\n}\n")
		renderBytesValue(b, f, ref, posVar)
		return
	}
	b.WriteString("} else {\n")
	renderBytesValue(b, f, ref, posVar)
	b.WriteString("}\n")
}

// emitEmptyBytesNonNil makes an empty JSON value ("" or []) decode to an
// empty non-nil slice on a nil receiver, like every other container. The
// decoders alone would return nil: AppendDecode(nil, "") is nil, and an
// immediate ']' appends nothing. A nil slice would marshal back as null.
func emitEmptyBytesNonNil(b *bytes.Buffer, ref string) {
	fmt.Fprintf(b, "if %[1]s == nil { %[1]s = []byte{} }\n", ref)
}

func renderBytesValue(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	field := fieldLit(f)
	if f.Format == "array" {
		// The temp is `u`, not `v`, because a pointer-leaf caller declares
		// `var v []byte` in this scope.
		inlineSkipWS(b, posVar)
		fmt.Fprintf(b, `if %[1]s >= len(data) || data[%[1]s] != '[' { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrBadArray) }
%[1]s++
`, posVar, field)
		inlineSkipWS(b, posVar)
		fmt.Fprintf(b, `var u uint64
for %[1]s < len(data) && data[%[1]s] != ']' {
	u, %[1]s, err = ggen.Uint64(data, %[1]s)
	if err != nil { return result, %[1]s, ggen.NewParseErr(%[3]s, %[1]s, err) }
	if u > 255 { return result, %[1]s, ggen.NewParseErr(%[3]s, %[1]s, ggen.ErrNumberOverflow) }
	%[2]s = append(%[2]s, byte(u))
`, posVar, ref, field)
		inlineSkipWS(b, posVar)
		fmt.Fprintf(b, `	if %[1]s < len(data) && data[%[1]s] == ',' { %[1]s++; `, posVar)
		inlineSkipWS(b, posVar)
		emitNoCloseAfterComma(b, field, posVar, ']')
		fmt.Fprintf(b, ` continue }
	break
}
if %[1]s >= len(data) || data[%[1]s] != ']' { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrBadArray) }
%[1]s++
`, posVar, field)
		emitEmptyBytesNonNil(b, ref)
		return
	}
	// AppendDecode avoids the `[]byte(s)` copy that DecodeString makes. dst is
	// sized up front with DecodedLen, so the one allocation is exact.
	enc := "base64.StdEncoding"
	dlen := "base64.StdEncoding.DecodedLen"
	switch f.Format {
	case "base64url":
		enc = "base64.URLEncoding"
		dlen = "base64.URLEncoding.DecodedLen"
	case "base32":
		enc = "base32.StdEncoding"
		dlen = "base32.StdEncoding.DecodedLen"
	case "base32hex":
		enc = "base32.HexEncoding"
		dlen = "base32.HexEncoding.DecodedLen"
	case "base16", "hex":
		enc = "" // hex does not have the Encoding API
	}
	if n := byteArrayLen(f); n > 0 {
		// Fixed array. Decode into a scratch array of the same size and
		// require exactly N bytes, like the strict count of a tuple. A longer
		// payload grows past the scratch capacity and fails the length check.
		dec := enc + ".AppendDecode"
		if enc == "" {
			dec = "hex.AppendDecode"
		}
		tmp := "buf" + sanitizeIdent(f.GoName)
		b.WriteString("var s string\n")
		inlineScanString(b, posVar, "s", posVar, field, false, !f.AllowInvalidUTF8)
		fmt.Fprintf(b, `var %[1]s [%[2]d]byte
var %[1]sd []byte
%[1]sd, err = %[3]s(%[1]s[:0], unsafe.Slice(unsafe.StringData(s), len(s)))
if err != nil { return result, %[4]s, ggen.NewParseErr(%[5]s, %[4]s, err) }
if len(%[1]sd) != %[2]d { return result, %[4]s, %[6]s }
copy(%[7]s[:], %[1]sd)
`, tmp, n, dec, posVar, field,
			arrayLenErr(f.JSONName, n, "len("+tmp+"d)", posVar), ref)
		return
	}
	if enc == "" {
		// Hex. ref was already reset to [:0]. Allocate only when its capacity
		// is too small.
		b.WriteString("var s string\n")
		inlineScanString(b, posVar, "s", posVar, field, false, !f.AllowInvalidUTF8)
		fmt.Fprintf(b, `if cap(%[1]s) < hex.DecodedLen(len(s)) { %[1]s = make([]byte, 0, hex.DecodedLen(len(s))) }
%[1]s, err = hex.AppendDecode(%[1]s, unsafe.Slice(unsafe.StringData(s), len(s)))
if err != nil { return result, %[2]s, ggen.NewParseErr(%[3]s, %[2]s, err) }
`, ref, posVar, field)
		emitEmptyBytesNonNil(b, ref)
		return
	}
	b.WriteString("var s string\n")
	inlineScanString(b, posVar, "s", posVar, field, false, !f.AllowInvalidUTF8)
	fmt.Fprintf(b, `if cap(%[1]s) < %[2]s(len(s)) { %[1]s = make([]byte, 0, %[2]s(len(s))) }
%[1]s, err = %[3]s.AppendDecode(%[1]s, unsafe.Slice(unsafe.StringData(s), len(s)))
if err != nil { return result, %[4]s, ggen.NewParseErr(%[5]s, %[4]s, err) }
`, ref, dlen, enc, posVar, field)
	emitEmptyBytesNonNil(b, ref)
}

// renderTime emits time.Time decode.
func renderTime(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	field := fieldLit(f)
	layout, numeric := timeLayoutExpr(f.Format)
	if numeric != "" {
		// `format:unix` reads a number that may have a fraction and splits it
		// into seconds and nanoseconds, so the value round-trips. The other
		// unix* formats are whole integers.
		if numeric == "Unix" {
			fmt.Fprintf(b, `var f float64
f, %[1]s, err = ggen.Float64(data, %[1]s)
if err != nil { return result, %[1]s, ggen.NewParseErr(%[3]s, %[1]s, err) }
sec := int64(f)
nsec := int64((f - float64(sec)) * 1e9)
%[2]s = time.Unix(sec, nsec)
`, posVar, ref, field)
			return
		}
		ctor := map[string]string{
			"UnixMilli": "time.UnixMilli(n)",
			"UnixMicro": "time.UnixMicro(n)",
			"UnixNano":  "time.Unix(0, n)",
		}[numeric]
		fmt.Fprintf(b, `var n int64
n, %[1]s, err = ggen.Int64(data, %[1]s)
if err != nil { return result, %[1]s, ggen.NewParseErr(%[4]s, %[1]s, err) }
%[2]s = %[3]s
`, posVar, ref, ctor, field)
		return
	}
	b.WriteString("var s string\n")
	inlineScanString(b, posVar, "s", posVar, field, false, !f.AllowInvalidUTF8)
	fmt.Fprintf(b, `%[1]s, err = %[2]s
if err != nil { return result, %[3]s, ggen.NewParseErr(%[4]s, %[3]s, err) }
`, ref, timeParseExpr(f.Format, layout, "s"), posVar, field)
}

// renderDuration emits time.Duration decode.
func renderDuration(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	field := fieldLit(f)
	switch f.Format {
	case "sec":
		// The temp is `f`, not `v`, because a pointer-leaf caller declares
		// `var v time.Duration` in this scope.
		fmt.Fprintf(b, `var f float64
f, %[1]s, err = ggen.Float64(data, %[1]s)
if err != nil { return result, %[1]s, ggen.NewParseErr(%[3]s, %[1]s, err) }
%[2]s = time.Duration(f * float64(time.Second))
`, posVar, ref, field)
		return
	case "milli", "micro", "nano":
		unit := map[string]string{
			"milli": "time.Millisecond",
			"micro": "time.Microsecond",
			"nano":  "time.Nanosecond",
		}[f.Format]
		fmt.Fprintf(b, `var n int64
n, %[1]s, err = ggen.Int64(data, %[1]s)
if err != nil { return result, %[1]s, ggen.NewParseErr(%[4]s, %[1]s, err) }
%[2]s = time.Duration(n) * %[3]s
`, posVar, ref, unit, field)
		return
	}
	b.WriteString("var s string\n")
	inlineScanString(b, posVar, "s", posVar, field, false, !f.AllowInvalidUTF8)
	fmt.Fprintf(b, `%[1]s, err = time.ParseDuration(s)
if err != nil { return result, %[2]s, ggen.NewParseErr(%[3]s, %[2]s, err) }
`, ref, posVar, field)
}

// renderNetIP, renderNetipAddr and renderNetipPrefix read "" as the zero
// value. That is what a zero address marshals to, and what the types' own
// UnmarshalText accept, so a field that was never set round-trips. net.IP is a
// byte slice, so null gives nil, like []byte.
func renderNetIP(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	field := fieldLit(f)
	inlineNullPeek(b, posVar, field)
	fmt.Fprintf(b, "%s = nil\n", ref)
	flat := nullBreakOK(f)
	if flat {
		b.WriteString("break\n}\n")
	} else {
		b.WriteString("} else {\n")
	}
	b.WriteString("var s string\n")
	inlineScanString(b, posVar, "s", posVar, field, false, !f.AllowInvalidUTF8)
	// net.ParseIP copies into a new IP, so the scanned string can alias the
	// input. The error literal keeps the string, so under -copy it is
	// detached. The stream version clones it for the same reason.
	text := "s"
	if f.Copy {
		text = "ggen.Detach(s, data)"
	}
	fmt.Fprintf(b, `if s == "" { %[1]s = nil } else {
%[1]s = net.ParseIP(s)
if %[1]s == nil { return result, %[2]s, ggen.NewParseErr(%[3]s, %[2]s, &net.ParseError{Type: "IP address", Text: %[4]s}) }
}
`, ref, posVar, field, text)
	if !flat {
		b.WriteString("}\n")
	}
}

func renderNetipAddr(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	field := fieldLit(f)
	b.WriteString("var s string\n")
	inlineScanString(b, posVar, "s", posVar, field, false, !f.AllowInvalidUTF8)
	fmt.Fprintf(b, `if s == "" { %[1]s = netip.Addr{} } else {
%[1]s, err = netip.ParseAddr(s)
if err != nil { return result, %[2]s, ggen.NewParseErr(%[3]s, %[2]s, err) }
}
`, ref, posVar, field)
}

func renderNetipPrefix(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	field := fieldLit(f)
	b.WriteString("var s string\n")
	inlineScanString(b, posVar, "s", posVar, field, false, !f.AllowInvalidUTF8)
	fmt.Fprintf(b, `if s == "" { %[1]s = netip.Prefix{} } else {
%[1]s, err = netip.ParsePrefix(s)
if err != nil { return result, %[2]s, ggen.NewParseErr(%[3]s, %[2]s, err) }
}
`, ref, posVar, field)
}

// elemAsField reshapes f so its element looks like the field itself. The
// fallback code for types from other packages needs this view when a slice,
// array or map element has such a type. Element-level pipes are left out,
// because the caller emits them.
func elemAsField(f model.FieldInfo) model.FieldInfo {
	ef := f
	ef.GoType = f.ElemType
	ef.Kind = f.ElemKind
	ef.Iface = f.ElemIface
	ef.Pointer = false
	ef.PointeeType = ""
	ef.ElemType = ""
	ef.ElemKind = model.KindString
	return ef
}

// sliceElemField adapts elemAsField so an element can go through the full
// field emitters. This is for elements with a dedicated kind: time, duration,
// bytes, raw, netip, url, big, sql.Null, any and map. The element's value
// rules run separately through elemSteps after the decode. The element sits
// inside a loop and never at dispatch level. So the outer field's rules and
// dispatch flags must not carry over. Map and container elements get their
// own element shape back, which elemAsField cleared.
func sliceElemField(f model.FieldInfo) model.FieldInfo {
	ef := elemAsField(f)
	ef.JSONName = f.JSONName + "[]"
	ef.AtDispatch = false
	ef.NullZero = false
	ef.String = false
	ef.Embed = false
	ef.OmitEmpty, ef.OmitZero = false, false
	ef.Validation, ef.Mods, ef.Pipe = nil, nil, nil
	ef.ElemValidation, ef.ElemMods = nil, nil
	ef.InnerValidation, ef.InnerMods = nil, nil
	ef.KeyValidation, ef.KeyMods, ef.KeyPipe = nil, nil, nil
	ef.Levels, ef.HintLevels = nil, nil
	ef.HintLen = -1
	ef.ElemPointer = false
	// The outer [N] must not carry over, or the element of a `[N][]byte`
	// would decode as a `[N]byte`.
	ef.ArrayLen, ef.ElemArrayLen = 0, 0
	ef.NullDone = false
	//exhaustive:ignore not every kind applies here
	switch ef.Kind {
	case model.KindMap:
		// Do what the parser does: pointer stars stay on ElemType. renderMap
		// routes pointer values through elemPtrType.
		ef.ElemType = strings.TrimPrefix(ef.GoType, "map[string]")
		ef.ElemKind = model.ResolveKind(ef.ElemType)
	case model.KindSlice, model.KindArray:
		// Rebuild the container's own element shape, which elemAsField
		// cleared. Remove a pointer the same way peelSliceField does.
		inner, kind, n := model.StripOneContainer(ef.GoType)
		if strings.HasPrefix(inner, "*") {
			ef.ElemPointer = true
			inner = inner[1:]
			kind = model.ResolveKind(inner)
		}
		ef.ElemType, ef.ElemKind = inner, kind
		if ef.ElemKind == model.KindArray {
			ef.ElemArrayLen = model.ArrayLenFromType(ef.ElemType)
		}
		if ef.Kind == model.KindArray {
			ef.ArrayLen = n
		}
	}
	return ef
}

// renderCrossPkgStructDecode emits the decode code for a struct field whose
// type comes from another package or is not annotated. When f.Iface is
// resolved, the code calls the method the type implements, chosen at
// generate time with no runtime checks. When it is unresolved (AST-only
// mode), it falls back to encoding/json.
func renderCrossPkgStructDecode(f model.FieldInfo, ref, posVar string) string {
	chk := bytesErrCheck(fieldLit(f), posVar)
	// Every decoder other than ggen's merges into its target, so the target
	// is zeroed first. Decoding into a receiver must give what a fresh decode
	// would.
	zero := zeroAssign(f, ref)
	if f.Iface.Resolved {
		switch {
		case f.Iface.ByteDecoder:
			// Generated by ggen in another package. Call DecodeFrom directly.
			return fmt.Sprintf(`var consumed int
%[1]s, consumed, err = %[1]s.DecodeFrom(data[%[2]s:])
%[2]s += consumed
%[3]s`, ref, posVar, nestedDecodeErrCheck(fieldLit(f), calleeTypeOf(f), f.MultiErr, true, "consumed"))

		case f.Iface.JSONUnmarshaler:
			// The callee sees a sub-slice. A positional error it returns, for
			// example from a wrapper that calls another package's ggen
			// decoder, is shifted to a payload offset like every nested
			// decode.
			return fmt.Sprintf(`start := %[1]s
%[1]s, err = ggen.SkipValue(data, start)
%[3]s%[4]serr = %[2]s.UnmarshalJSON(data[start:%[1]s])
if err != nil { return result, %[1]s, ggen.NewParseErrShift(%[5]s, %[1]s, %[1]s-start, err) }
`, posVar, ref, chk, zero, fieldLit(f))

		case f.Iface.TextUnmarshaler:
			return fmt.Sprintf(`var ts string
ts, %[1]s, err = `+scanStringFn+`(data, %[1]s, `+vArg(f)+`)
%[3]s%[4]serr = %[2]s.UnmarshalText(unsafe.Slice(unsafe.StringData(ts), len(ts)))
%[3]s`, posVar, ref, chk, zero)
		}
	}
	// Unresolved (AST-only) or no known method. Fall back to encoding/json.
	return fmt.Sprintf(`start := %[1]s
%[1]s, err = ggen.SkipValue(data, start)
%[3]s%[4]serr = json.Unmarshal(data[start:%[1]s], &%[2]s)
%[3]s`, posVar, ref, chk, zero)
}

// renderCrossPkgStructAppend is the marshal counterpart for struct fields
// from other packages. When resolved, the code calls the method the type
// implements, chosen at generate time. When unresolved, it uses
// encoding/json.
func renderCrossPkgStructAppend(f model.FieldInfo, ref string) string {
	// The temp name carries the field name. Fields are emitted in the scope
	// of the function, so two such fields would collide on a shared name.
	tmp := "b" + strings.ReplaceAll(f.GoName, ".", "")
	if f.Iface.Resolved {
		switch {
		case f.Iface.AppendJSON:
			return fmt.Sprintf("if dst, err = %s.AppendJSON(dst); err != nil { return dst, err }\n", ref)

		case f.Iface.JSONMarshaler:
			// An empty result is an error, as in stdlib. A plain append would
			// write `"k":` with no value and a nil error.
			return fmt.Sprintf(`var %[1]s []byte
%[1]s, err = %[2]s.MarshalJSON()
if err != nil { return dst, err }
if len(%[1]s) == 0 { return dst, ggen.ErrEmptyMarshalJSON }
dst = append(dst, %[1]s...)
`, tmp, ref)

		case f.Iface.TextAppender:
			// AppendText (Go 1.24+) is preferred over MarshalText because it
			// does not allocate. Close through the escape checker, because the
			// text may hold `"`, `\` or control bytes.
			ta := "ta" + strings.ReplaceAll(f.GoName, ".", "")
			return fmt.Sprintf(`dst = append(dst, '"')
%[1]s := len(dst)
if dst, err = %[2]s.AppendText(dst); err != nil { return dst, err }
dst = %[3]s(dst, %[1]s)
`, ta, ref, closeStrFn(f.HTMLEscape))

		case f.Iface.TextMarshaler:
			return fmt.Sprintf(`var %[1]s []byte
%[1]s, err = %[2]s.MarshalText()
if err != nil { return dst, err }
dst = append(dst, '"')
dst = %[3]s(dst, ggen.BytesToString(%[1]s))
`, tmp, ref, appendStrFn(f.HTMLEscape))

		default:
			return fmt.Sprintf(`var %[1]s []byte
%[1]s, err = json.Marshal(%[2]s)
if err != nil { return dst, err }
dst = append(dst, %[1]s...)
`, tmp, ref)
		}
	}
	// Unresolved (AST-only). Fall back to encoding/json.
	return fmt.Sprintf(`var %[1]s []byte
%[1]s, err = json.Marshal(%[2]s)
if err != nil { return dst, err }
dst = append(dst, %[1]s...)
`, tmp, ref)
}

// renderCrossPkgStructStreamDecode is the stream counterpart of
// renderCrossPkgStructDecode, with the same choice of method.
func renderCrossPkgStructStreamDecode(f model.FieldInfo, ref, posVar string) string {
	_ = posVar
	chk := streamErrCheck(fieldLit(f))
	zero := zeroAssign(f, ref) // see renderCrossPkgStructDecode
	if f.Iface.Resolved {
		switch {
		case f.Iface.StreamDecoder:
			return fmt.Sprintf(`%[1]s, err = %[1]s.DecodeFromStream(s)
%[2]s`, ref, nestedDecodeErrCheck(fieldLit(f), calleeTypeOf(f), f.MultiErr, false, ""))

		case f.Iface.JSONUnmarshaler:
			// The captured span is a sub-slice that starts at
			// Offset()-len(span), so the callee's positions are shifted the
			// same way as on the bytes path.
			return fmt.Sprintf(`span, err := s.CaptureValue()
%[2]s%[3]serr = %[1]s.UnmarshalJSON(span)
if err != nil { return result, ggen.NewParseErrShift(%[4]s, s.Offset(), len(span), err) }
`, ref, chk, zero, fieldLit(f))

		case f.Iface.TextUnmarshaler:
			// StringView returns a string that aliases the Stream's buffer.
			// That is safe here: by the encoding contract UnmarshalText must
			// not keep its argument, and the bytes path passes an aliased
			// slice too. See Stream.StringView.
			return fmt.Sprintf(`var ts string
ts, err = s.StringView(`+vArg(f)+`)
%[2]s%[3]serr = %[1]s.UnmarshalText(unsafe.Slice(unsafe.StringData(ts), len(ts)))
%[2]s`, ref, chk, zero)
		}
	}
	// Unresolved (AST-only) or no known method. Fall back to encoding/json.
	return fmt.Sprintf(`span, err := s.CaptureValue()
%[2]s%[3]serr = json.Unmarshal(span, &%[1]s)
%[2]s`, ref, chk, zero)
}

// renderRawJSON captures the raw bytes of the value. By default the field
// aliases data[start:end], with no copy. Under -copy the bytes are copied
// into the field's own backing array, which is reused, so the value survives
// later changes to data. It works for json.RawMessage and jsontext.Value,
// which are both []byte underneath.
func renderRawJSON(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	chk := bytesErrCheck(fieldLit(f), posVar)
	span := "data[start:%[1]s]"
	if f.Copy {
		// Append to the existing backing array, reused across decodes. A nil
		// field gets a new one.
		span = "append(%[2]s[:0], data[start:%[1]s]...)"
	}
	check := "err = ggen.CheckUTF8(data[start:%[1]s])\n%[3]s"
	if f.AllowInvalidUTF8 {
		check = ""
	}
	fmt.Fprintf(b, `start := %[1]s
%[1]s, err = ggen.SkipValue(data, start)
%[3]s`+check+`%[2]s = `+span+`
`, posVar, ref, chk)
}

// renderURL parses a JSON string with url.Parse. The dereference is safe
// because Parse returns a non-nil *URL on success. The scan honors f.Copy.
// url.Parse slices its input into Host, Path, RawQuery and Fragment, so an
// aliasing scan would leave the stored URL pointing into the caller's buffer.
func renderURL(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	field := fieldLit(f)
	b.WriteString("var s string\n")
	inlineScanString(b, posVar, "s", posVar, field, f.Copy, !f.AllowInvalidUTF8)
	fmt.Fprintf(b, `var u *url.URL
u, err = url.Parse(s)
if err != nil { return result, %[2]s, ggen.NewParseErr(%[3]s, %[2]s, err) }
%[1]s = *u
`, ref, posVar, field)
}

// renderBigInt reads a bare JSON number with big.Int.SetString, which has no
// overflow and no length limit. The digits are aliased with unsafe.String.
// SetString copies them into its own storage, so the alias does not outlive
// the call.
func renderBigInt(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	field := fieldLit(f)
	fmt.Fprintf(b, `start := %[1]s
%[1]s, err = ggen.SkipValue(data, start)
if err != nil { return result, %[1]s, ggen.NewParseErr(%[3]s, %[1]s, err) }
if _, ok := (&%[2]s).SetString(unsafe.String(unsafe.SliceData(data[start:]), %[1]s-start), 10); !ok {
	return result, %[1]s, ggen.NewParseErr(%[3]s, %[1]s, ggen.ErrBadNumber)
}
`, posVar, ref, field)
}

// renderBigFloat reads a number written inside a JSON string into a
// big.Float at the default precision. The quotes match jsonv2's format for
// big.Float. Bare numbers are not accepted. Use big.Int or float64 for those.
func renderBigFloat(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	field := fieldLit(f)
	b.WriteString("var s string\n")
	inlineScanString(b, posVar, "s", posVar, field, false, !f.AllowInvalidUTF8)
	fmt.Fprintf(b, `if _, _, err := (&%[1]s).Parse(s, 10); err != nil {
	return result, %[2]s, ggen.NewParseErr(%[3]s, %[2]s, err)
}
`, ref, posVar, field)
}

// renderBigRat reads a JSON string of the form "num" or "num/denom" and passes
// it to big.Rat.SetString. Nothing is lost: fractions stay exact.
func renderBigRat(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	field := fieldLit(f)
	b.WriteString("var s string\n")
	inlineScanString(b, posVar, "s", posVar, field, false, !f.AllowInvalidUTF8)
	fmt.Fprintf(b, `if _, ok := (&%[1]s).SetString(s); !ok {
	return result, %[2]s, ggen.NewParseErr(%[3]s, %[2]s, ggen.ErrBadNumber)
}
`, ref, posVar, field)
}

// sqlNullInnerField returns a copy of a generic sql.Null[T]'s inner
// FieldInfo, ready to render. That is the synthetic V field with the parent's
// struct flags applied. The parser builds the inner field before the flags
// are propagated, so they are applied here.
func sqlNullInnerField(f model.FieldInfo) model.FieldInfo {
	inner := *f.SQLNullInner
	inner.JSONName = f.JSONName
	inner.MultiErr = f.MultiErr
	inner.NoValidate = f.NoValidate
	inner.UseNumber = f.UseNumber
	inner.HTMLEscape = f.HTMLEscape
	inner.Copy = f.Copy
	inner.AllowInvalidUTF8 = f.AllowInvalidUTF8
	inner.AtDispatch = false
	return inner
}

// renderSQLNull emits the decode of a database/sql.NullX field. It checks for
// `null` first. Otherwise it parses the inner value and sets Valid to true.
func renderSQLNull(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	if f.SQLNullInner != nil {
		// Generic sql.Null[T]. null gives the zero value. Otherwise decode V
		// into a temp and store the {V, Valid: true} literal.
		inner := sqlNullInnerField(f)
		var dec bytes.Buffer
		renderField(&dec, inner, "nv", posVar)
		body, valExpr := dec.String(), "nv"
		if rest, expr, ok := foldTailAssign(body, "nv"); ok {
			body, valExpr = rest, expr
		} else {
			body = "var nv " + inner.GoType + "\n" + body
		}
		inlineNullPeek(b, posVar, fieldLit(f))
		fmt.Fprintf(b, `%[1]s = sql.%[2]s{}
} else {
	%[3]s
	%[1]s = sql.%[2]s{V: %[4]s, Valid: true}
}
`, ref, sqlTypeName(f.GoType), body, valExpr)
		return
	}
	spec, ok := model.SQLNullSpec(f.GoType)
	if !ok {
		return
	}
	field := fieldLit(f)
	var inner bytes.Buffer
	valExpr := "nv"
	declType := spec.Type
	//exhaustive:ignore not every kind applies here
	switch spec.Inner {
	case model.KindString:
		declType = "string"
		inlineScanString(&inner, posVar, "nv", posVar, field, f.Copy, !f.AllowInvalidUTF8)
	case model.KindBool:
		declType = "bool"
		fmt.Fprintf(&inner, "nv, %[1]s, err = ggen.Bool(data, %[1]s)\n", posVar)
		fmt.Fprintf(&inner, "if err != nil { %[1]s = ggen.BoolEnd(data, %[1]s); return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, err) }\n", posVar, field)
	case model.KindInt, model.KindInt8, model.KindInt16, model.KindInt32, model.KindInt64:
		cast := spec.Type
		if spec.Type == "int64" {
			cast = ""
		}
		inlineScanInt64(&inner, posVar, "nv", cast, field)
	case model.KindUint, model.KindUint8, model.KindUint16, model.KindUint32, model.KindUint64:
		cast := spec.Type
		if spec.Type == "uint64" {
			cast = ""
		}
		inlineScanUint64(&inner, posVar, "nv", cast, field)
	case model.KindFloat32, model.KindFloat64:
		declType = kindPrimitiveName(spec.Inner)
		fmt.Fprintf(&inner, "nv, %[1]s, err = ggen.%[2]s(data, %[1]s)\n", posVar, floatScanFn(spec.Inner))
		fmt.Fprintf(&inner, "if err != nil { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, err) }\n", posVar, field)
		if spec.Type != declType {
			valExpr = spec.Type + "(nv)"
		}
	case model.KindTime:
		tf := model.FieldInfo{
			JSONName: f.JSONName, Format: f.Format,
			Copy: f.Copy, AllowInvalidUTF8: f.AllowInvalidUTF8, MultiErr: f.MultiErr,
		}
		declType = "time.Time"
		renderTime(&inner, tf, "nv", posVar)
	}
	body := inner.String()
	if valExpr == "nv" {
		if rest, expr, ok := foldTailAssign(body, "nv"); ok {
			body, valExpr = rest, expr
		} else {
			body = "var nv " + declType + "\n" + body
		}
	} else {
		body = "var nv " + declType + "\n" + body
	}
	inlineNullPeek(b, posVar, field)
	fmt.Fprintf(b, `%[1]s = sql.%[2]s{}
} else {
	%[3]s
	%[1]s = sql.%[2]s{%[4]s: %[5]s, Valid: true}
}
`, ref, sqlTypeName(f.GoType), body, spec.Field, valExpr)
}

// sqlTypeName returns the bare type name from a `sql.NullX` qualified name.
func sqlTypeName(goType string) string {
	if _, after, ok := strings.Cut(goType, "."); ok {
		return after
	}
	return goType
}

// renderAny decodes into an `any` with ggen.Any or ggen.AnyNumber. Under
// -copy the Copy variants clone every nested string, both values and object
// keys, so the result survives later changes to data.
func renderAny(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	fn := "ggen.Any"
	if f.UseNumber {
		fn = "ggen.AnyNumber"
	}
	if f.Copy {
		fn += "Copy"
	}
	chk := bytesErrCheck(fieldLit(f), posVar)
	fmt.Fprintf(b, `%[1]s, %[2]s, err = %[3]s(data, %[2]s, %[5]s)
%[4]s`, ref, posVar, fn, chk, vArg(f))
}

// needsSeen reports whether f needs a seen<Field> flag. Every field does
// except an embed catch-all map. The flag drives the duplicate-key guard, the
// first-wins skip and the required-field check after the loop.
func needsSeen(f model.FieldInfo) bool {
	return !f.Embed
}

// seenBitmaskThreshold is the field count above which the generated code uses
// a packed bitmask instead of one `bool` local per field. It saves stack and
// cache on wide structs.
const seenBitmaskThreshold = 32

// useSeenBitmask reports whether struct s should track seen fields in a
// packed bitmask instead of one bool per field.
func useSeenBitmask(s model.StructInfo) bool {
	if len(s.Fields) <= seenBitmaskThreshold {
		return false
	}
	for _, f := range s.Fields {
		if !f.Embed && needsSeen(f) {
			return true
		}
	}
	return false
}

// seenBitIndex gives f a stable bit index, its position in s.Fields. An embed
// catch-all field still takes an index, which keeps the indexing simple.
func seenBitIndex(s model.StructInfo, f model.FieldInfo) int {
	for i, ff := range s.Fields {
		if ff.GoName == f.GoName {
			return i
		}
	}
	return -1
}

func seenWordCount(s model.StructInfo) int {
	return (len(s.Fields) + 63) / 64
}

// seenDecl returns the declaration of the bitmask. It returns an empty string
// when one bool per field is used, and the caller then declares the bools.
func seenDecl(s model.StructInfo) string {
	if !useSeenBitmask(s) {
		return ""
	}
	if seenWordCount(s) == 1 {
		return "var seen uint64\n"
	}
	return fmt.Sprintf("var seen [%d]uint64\n", seenWordCount(s))
}

// seenAccess returns the expression that reads f's seen flag: the bool local
// or a bitmask test.
func seenAccess(s model.StructInfo, f model.FieldInfo) string {
	if !useSeenBitmask(s) {
		return "seen" + f.GoName
	}
	bit := seenBitIndex(s, f)
	if seenWordCount(s) == 1 {
		return fmt.Sprintf("seen&(1<<%d) != 0", bit)
	}
	return fmt.Sprintf("seen[%d]&(1<<%d) != 0", bit/64, bit%64)
}

// seenNotAccess is the negation of seenAccess. It is true when the flag is not
// set.
func seenNotAccess(s model.StructInfo, f model.FieldInfo) string {
	if !useSeenBitmask(s) {
		return "!seen" + f.GoName
	}
	bit := seenBitIndex(s, f)
	if seenWordCount(s) == 1 {
		return fmt.Sprintf("seen&(1<<%d) == 0", bit)
	}
	return fmt.Sprintf("seen[%d]&(1<<%d) == 0", bit/64, bit%64)
}

// seenSet returns a statement that sets f's seen flag. It ends with a newline
// so callers can concatenate it freely.
func seenSet(s model.StructInfo, f model.FieldInfo) string {
	if !useSeenBitmask(s) {
		return "seen" + f.GoName + " = true\n"
	}
	bit := seenBitIndex(s, f)
	if seenWordCount(s) == 1 {
		return fmt.Sprintf("seen |= 1<<%d\n", bit)
	}
	return fmt.Sprintf("seen[%d] |= 1<<%d\n", bit/64, bit%64)
}

// unknownKey emits the default branch of the key switch. In order of
// precedence: an embed catch-all map stores the entry, IgnoreUnknown skips the
// value, and otherwise it is an UnknownKeyError. On the bytes path `key`
// aliases data and stays valid for the whole function. So by default it goes
// straight into the stored map key or ggen.NewParseErr, with no clone. Under
// -copy every kept `key` is cloned (keyExpr), and string and `any` values of
// the catch-all map are copied too. The stored entries then survive later
// changes to data, as on the stream path.
func unknownKey(s model.StructInfo, posVar string) string {
	keyExpr := "key"
	if s.Copy {
		keyExpr = "strings.Clone(key)"
	}
	chk := bytesErrCheck(keyExpr, posVar)
	if embedded := s.EmbedField(); embedded.Embed {
		initMap := fmt.Sprintf("if result.%s == nil { result.%s = make(%s) }\n",
			embedded.GoName, embedded.GoName, embedded.GoType)
		//exhaustive:ignore not every kind applies here
		switch embedded.ElemKind {
		case model.KindAny:
			anyFn := "ggen.Any"
			if s.UseNumber {
				anyFn = "ggen.AnyNumber"
			}
			if s.Copy {
				anyFn += "Copy"
			}
			return initMap + fmt.Sprintf(`result.%[1]s[%[5]s], %[2]s, err = %[3]s(data, %[2]s, %[6]s)
%[4]s`, embedded.GoName, posVar, anyFn, chk, keyExpr, vArg(embedded))
		case model.KindString:
			if s.Copy {
				// Call the tier scanner, then ggen.Detach, which clones the
				// value only when it aliases data. An escaped string is already
				// a fresh copy, so it is not copied twice. The key is cloned
				// through keyExpr.
				return initMap + fmt.Sprintf(`var strVal string
strVal, %[1]s, err = `+scanStringFn+`(data, %[1]s, `+vArg(embedded)+`)
%[3]sresult.%[2]s[%[4]s] = ggen.Detach(strVal, data)
`, posVar, embedded.GoName, chk, keyExpr)
			}
			return initMap + fmt.Sprintf(`result.%[2]s[key], %[1]s, err = `+scanStringFn+`(data, %[1]s, `+vArg(embedded)+`)
%[3]s`, posVar, embedded.GoName, chk)
		case model.KindStruct:
			if isGenerated(embedded.ElemType) {
				return initMap + fmt.Sprintf(`var entry %[1]s
var entryConsumed int
entry, entryConsumed, err = entry.`+decodeCallFor(embedded.ElemType)+`
%[2]s += entryConsumed
%[4]sresult.%[3]s[%[5]s] = entry
`, embedded.ElemType, posVar, embedded.GoName, nestedDecodeErrCheck(keyExpr, embedded.ElemType, s.MultiErr, true, "entryConsumed"), keyExpr)
			}
		}
		return initMap + fmt.Sprintf(`spanStart := %[1]s
%[1]s, err = ggen.SkipValue(data, %[1]s)
%[4]svar entry %[2]s
if err = json.Unmarshal(data[spanStart:%[1]s], &entry); err != nil { return result, %[1]s, ggen.NewParseErr(%[5]s, %[1]s, err) }
result.%[3]s[%[5]s] = entry
`, posVar, embedded.ElemType, embedded.GoName, chk, keyExpr)
	}
	if s.IgnoreUnknown {
		return fmt.Sprintf(`%[1]s, err = ggen.SkipValue(data, %[1]s)
%[2]s`, posVar, chk)
	}
	if s.MultiErr {
		return fmt.Sprintf(`errs = append(errs, &ggen.UnknownKeyError{Pos: %[1]s, Path: []string{%[3]s}})
%[1]s, err = ggen.SkipValue(data, %[1]s)
%[2]s`, posVar, chk, keyExpr)
	}
	return fmt.Sprintf("return result, %[1]s, &ggen.UnknownKeyError{Pos: %[1]s, Path: []string{%[2]s}}\n", posVar, keyExpr)
}

// renderPipe emits an ordered list of value steps against ref: mods and
// validators mixed, in declared order. posVar picks the return shape: "i" for
// the bytes path and "" for the stream path.
func renderPipe(b *bytes.Buffer, steps []model.Step, ref, jsonName, goType string, kind model.TypeKind, multiErr bool, posVar string) {
	if len(steps) == 0 {
		return
	}
	onErr := func(errExpr string) string {
		errExpr = withPos(errExpr, posVar)
		if multiErr {
			return "errs = append(errs, " + errExpr + ")"
		}
		if posVar == "" {
			return "return result, " + errExpr
		}
		return "return result, " + posVar + ", " + errExpr
	}
	for p := 0; p < len(steps); {
		if steps[p].IsMod {
			renderOneMod(b, steps[p].M, strconv.Quote(jsonName), ref, goType, kind, posVar)
			p++
			continue
		}
		// The longest run of validators against the current, unchanged ref.
		q := p
		run := make([]model.ValidationRule, 0, len(steps)-p)
		for q < len(steps) && !steps[q].IsMod {
			run = append(run, steps[q].V)
			q++
		}
		emitValRun(b, run, ref, jsonName, goType, kind, multiErr, onErr)
		p = q
	}
}

// fieldPipe returns the ordered value steps for a field. It is f.Pipe when
// set. Otherwise it is built from the separate Mods and Validation lists,
// which covers synthetic pointer-leaf fields.
func fieldPipe(f model.FieldInfo) []model.Step {
	if f.Pipe != nil {
		return f.Pipe
	}
	return model.StepsFromLegacy(f.Mods, f.Validation)
}

func fieldKeyPipe(f model.FieldInfo) []model.Step {
	if f.KeyPipe != nil {
		return f.KeyPipe
	}
	return model.StepsFromLegacy(f.KeyMods, f.KeyValidation)
}

func validateAndMod(b *bytes.Buffer, f model.FieldInfo, ref string) {
	renderPipe(b, fieldPipe(f), ref, f.JSONName, f.GoType, f.Kind, f.MultiErr, "i")
}

// emitPointerPipe emits a pointer field's value steps as one ordered pass.
// Each `@Func` step takes the field's own `*T`. Each built-in step takes the
// dereferenced leaf. Every step stays at its declared position. posVar is ""
// on the stream path. guardNil wraps the built-in groups in a nil check. The
// converter dispatch needs that, because the pointer may still be nil there.
// The plain pointer path emits after the pointer chain is assigned, where
// every level is non-nil.
func emitPointerPipe(b *bytes.Buffer, f model.FieldInfo, ref string, depth int, leafType string, leafK model.TypeKind, posVar string, guardNil bool) {
	steps := fieldPipe(f)
	deref := derefStr(ref, depth)
	for i := 0; i < len(steps); {
		custom := model.StepIsCustom(steps[i])
		j := i + 1
		for j < len(steps) && model.StepIsCustom(steps[j]) == custom {
			j++
		}
		if custom {
			renderPipe(b, steps[i:j], ref, f.JSONName, f.GoType, f.Kind, f.MultiErr, posVar)
		} else {
			if guardNil {
				fmt.Fprintf(b, "if %s {\n", ptrNonNilCond(ref, depth))
			}
			renderPipe(b, steps[i:j], deref, f.JSONName, leafType, leafK, f.MultiErr, posVar)
			if guardNil {
				b.WriteString("}\n")
			}
		}
		i = j
	}
}

// ptrNonNilCond is the condition under which derefStr(ref, depth) is safe:
// every level of the chain is non-nil.
func ptrNonNilCond(ref string, depth int) string {
	conds := make([]string, 0, depth)
	for k := range depth {
		conds = append(conds, derefStr(ref, k)+" != nil")
	}
	return strings.Join(conds, " && ")
}

// emitFieldPipe runs a field's value steps. A pointer field splits its steps
// between the pointer and its leaf. Every other field runs them on ref.
func emitFieldPipe(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	if !f.Pointer {
		renderPipe(b, fieldPipe(f), ref, f.JSONName, f.GoType, f.Kind, f.MultiErr, posVar)
		return
	}
	depth, leafType := model.PointerDepth(f.GoType)
	emitPointerPipe(b, f, ref, depth, leafType, leafKind(f, leafType), posVar, true)
}

// validateAndModStream is the stream-path counterpart of validateAndMod. It
// emits the two-value `return result, err` form, because the Stream owns
// s.Pos.
func validateAndModStream(b *bytes.Buffer, f model.FieldInfo, ref string) {
	renderPipe(b, fieldPipe(f), ref, f.JSONName, f.GoType, f.Kind, f.MultiErr, "")
}

// renderStringTag handles json:",string". It reads a quoted JSON string and
// parses its content into the field's number or bool type.
func renderStringTag(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	field := fieldLit(f)
	// No braces, so `sv`, `qn` and `qe` are declared in the caller's scope.
	b.WriteString("var sv string\n")
	// Only a string field would keep sv, and the parser rejects `,string` on
	// a string field. Numbers and bools parse sv into a separate value, so no
	// copy is needed for them.
	inlineScanString(b, posVar, "sv", posVar, field, f.Copy && f.Kind == model.KindString, !f.AllowInvalidUTF8)
	if f.Kind == model.KindBool {
		fmt.Fprintf(b, `switch sv {
case "true": %[1]s = true
case "false": %[1]s = false
default: return result, %[2]s, ggen.NewParseErr(%[3]s, %[2]s, ggen.ErrBadBool)
}
`, ref, posVar, field)
		return
	}
	renderQuotedNumber(b, f, ref, fmt.Sprintf("return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ", posVar, field))
}

// renderQuotedNumber parses the `,string` text in sv, without its quotes,
// with the bytes-path scanners. So the quoted form gets exactly the same
// grammar, range and narrowing checks as a bare number. strconv would accept
// Go literal forms such as NaN, Infinity, +1, 01 and 1_0. jsonv2 accepts none
// of them, and a NaN or Inf read that way could not be marshaled back.
// retPrefix is the start of the caller's error return, up to the wrapped
// error.
func renderQuotedNumber(b *bytes.Buffer, f model.FieldInfo, ref, retPrefix string) {
	var scan, wide string
	switch f.Kind {
	case model.KindFloat32, model.KindFloat64:
		scan, wide = floatScanFn(f.Kind), kindPrimitiveName(f.Kind)
	case model.KindUint, model.KindUint8, model.KindUint16, model.KindUint32, model.KindUint64:
		scan, wide = "Uint64", "uint64"
	default:
		scan, wide = "Int64", "int64"
	}
	fmt.Fprintf(b, `var qn %[1]s
var qe int
qn, qe, err = ggen.%[2]s(unsafe.Slice(unsafe.StringData(sv), len(sv)), 0)
if err == nil && qe != len(sv) { err = ggen.ErrBadNumber }
if err != nil { %[3]serr) }
`, wide, scan, retPrefix)
	b.WriteString(narrowIntGuard("qn", kindNarrowName(f.Kind), retPrefix+"ggen.ErrNumberOverflow)"))
	if f.GoType == wide {
		fmt.Fprintf(b, "%s = qn\n", ref)
	} else {
		fmt.Fprintf(b, "%s = %s(qn)\n", ref, f.GoType)
	}
}

// elemPtrType returns the full element type with every pointer level, and the
// total pointer depth, where 0 means not a pointer. For slices and arrays the
// parser moves one `*` into ElemPointer. Map values and nested elements keep
// their stars on ElemType.
func elemPtrType(f model.FieldInfo) (string, int) {
	et := f.ElemType
	if f.ElemPointer {
		et = "*" + et
	}
	depth, _ := model.PointerDepth(et)
	return et, depth
}

// elemPtrField builds the FieldInfo that a pointer element decodes through.
// It uses the same code as a pointer field: parse the leaf first, then assign
// the pointer chain. Element rules come along. jsonName is the path used in
// errors, such as "x[]" or "x.value".
func elemPtrField(f model.FieldInfo, jsonName string) model.FieldInfo {
	et, _ := elemPtrType(f)
	_, leafType := model.PointerDepth(et)
	pf := model.FieldInfo{
		GoType:           et,
		Pointer:          true,
		PointeeType:      et[1:],
		Kind:             model.ResolveKind(leafType),
		JSONName:         jsonName,
		Format:           f.Format,
		HTMLEscape:       f.HTMLEscape,
		MultiErr:         f.MultiErr,
		NoValidate:       f.NoValidate,
		AllowDups:        f.AllowDups,
		UseNumber:        f.UseNumber,
		Copy:             f.Copy,
		AllowInvalidUTF8: f.AllowInvalidUTF8,
		Validation:       f.ElemValidation,
		Mods:             f.ElemMods,
		Iface:            f.ElemIface,
	}
	// A container leaf needs its own Elem* fields set. The parser fills them
	// only for the outermost container.
	//exhaustive:ignore not every kind applies here
	switch pf.Kind {
	case model.KindSlice, model.KindArray:
		pf.ArrayLen = model.ArrayLenFromType(leafType)
		elem, kind, _ := model.StripOneContainer(leafType)
		if strings.HasPrefix(elem, "*") {
			pf.ElemPointer = true
			elem = elem[1:]
			kind = model.ResolveKind(elem)
		}
		pf.ElemType, pf.ElemKind = elem, kind
		if kind == model.KindArray {
			pf.ElemArrayLen = model.ArrayLenFromType(elem)
		}
	case model.KindMap:
		pf.ElemType = strings.TrimPrefix(leafType, "map[string]")
		pf.ElemKind = model.ResolveKind(pf.ElemType)
	}
	return pf
}

// derefStr returns ref dereferenced k times, with parentheses. For example
// derefStr("x",0)=="x", derefStr("x",1)=="(*x)" and derefStr("x",2)=="(*(*x))".
func derefStr(ref string, k int) string {
	for ; k > 0; k-- {
		ref = "(*" + ref + ")"
	}
	return ref
}

// newChain returns `new(new(…expr))` with n `new(` calls. It wraps expr in n
// pointer levels with the Go 1.26 `new(expr)` form. For n==1 it returns
// `new(expr)`.
func newChain(expr string, n int) string {
	return strings.Repeat("new(", n) + expr + strings.Repeat(")", n)
}

// widenedLeafCast maps a narrow integer leaf kind to the wide type its scanner
// produces, int64 or uint64, plus the conversion back. The pointer decode
// scans into a wide temp and converts where it assigns. It returns ("",0,"")
// when no widening is needed. Floats are not widened, because float32 has its
// own scanner.
func widenedLeafCast(k model.TypeKind, leafGoType string) (wideType string, wideKind model.TypeKind, cast string) {
	//exhaustive:ignore not every kind applies here
	switch k {
	case model.KindInt, model.KindInt8, model.KindInt16, model.KindInt32:
		return "int64", model.KindInt64, leafGoType
	case model.KindUint, model.KindUint8, model.KindUint16, model.KindUint32:
		return "uint64", model.KindUint64, leafGoType
	}
	return "", 0, ""
}

// leafMerges reports whether a pointer leaf gains from being seeded with the
// receiver's existing value before the decode. Structs, slices, arrays and
// maps can reuse memory. Primitives are simply overwritten.
func leafMerges(k model.TypeKind) bool {
	//exhaustive:ignore not every kind applies here
	switch k {
	case model.KindStruct, model.KindSlice, model.KindArray, model.KindMap:
		return true
	}
	return false
}

// nullBreakOK reports whether a field's `null` branch can `break` out of the
// key switch instead of nesting the whole value decode in an else. The code
// must sit directly inside the key switch (AtDispatch), and the field must
// have no rules that run after the value. A break would skip the
// validateAndMod and custom @Func calls that follow the value code.
func nullBreakOK(f model.FieldInfo) bool {
	return f.AtDispatch && (f.NoValidate || (len(f.Validation) == 0 && len(f.Mods) == 0))
}

// nullZeroApplies reports whether the `nullzero` option adds a branch that
// turns null into the zero value. Only non-pointer fields at dispatch level
// whose kind would otherwise reject `null` qualify. For kinds that already
// accept null it does nothing.
func nullZeroApplies(f model.FieldInfo) bool {
	if !f.NullZero || f.Pointer || !f.AtDispatch {
		return false
	}
	//exhaustive:ignore not every kind applies here
	switch f.Kind {
	case model.KindSlice, model.KindMap, model.KindBytes, model.KindNetIP, model.KindRawJSON, model.KindSQLNull, model.KindAny:
		return false
	}
	return true
}

// emitStreamNullZero is the stream-path counterpart of the bytes-path
// inlineNullPeek plus zero assignment used for `nullzero`. It emits the
// `null` check over the Stream's window, the part of the reader's data
// currently in its buffer. Like the stream pointer branch, it calls ReadMore
// to refill when bytes are missing. On a match it sets ref to its Go zero,
// then either breaks (flat) or opens an else.
func emitStreamNullZero(b *bytes.Buffer, ref, zero, field string, flat bool, sentinel string) {
	rm := streamReadMore(field, "0", false, sentinel)
	rmKi := strings.Replace(streamReadMore(field, "0", false, "ggen.ErrBadLiteral"), "if s.Pos >=", "if s.Pos+ki >=", 1)
	fmt.Fprintf(b, `%[2]sif s.Bytes()[s.Pos] == 'n' {
	for ki := 1; ki < 4; ki++ {
		%[3]sif s.Bytes()[s.Pos+ki] != "null"[ki] { return result, ggen.NewParseErr(%[4]s, s.Offset(), ggen.ErrBadLiteral) }
	}
	s.Pos += 4
	%[1]s = %[5]s
`, ref, rm, rmKi, field, zero)
	if flat {
		b.WriteString("break\n}\n")
	} else {
		b.WriteString("} else {\n")
	}
}

// emitPointerSeed hands the receiver's existing leaf to the decode temp `v`
// when the whole pointer chain is already allocated, so the leaf decode reuses
// its memory. A struct resets itself and an array blanks its slots. A slice or
// map leaf is emptied here, because the container emitters append to or fill
// whatever they get, and the reset at entry covers only plain top-level
// fields. A bytes-path map whose values are swapped (see opt #76 in
// cmd/ggen/CLAUDE.md) is handed over as is, because the swap reads the old
// entries. It does nothing for primitive leaves, or for a leaf that would
// merge old data instead of resetting (see leafResets).
func emitPointerSeed(b *bytes.Buffer, ref string, depth int, leaf model.FieldInfo, bytesPath bool) {
	if !leafMerges(leaf.Kind) || !leafResets(leaf.Kind, leaf.GoType) {
		return
	}
	conds := make([]string, depth)
	for k := range depth {
		conds[k] = derefStr(ref, k) + " != nil"
	}
	seed := derefStr(ref, depth)
	if leaf.Kind == model.KindSlice {
		seed += "[:0]"
	}
	fmt.Fprintf(b, "if %s {\nv = %s\n", strings.Join(conds, " && "), seed)
	if leaf.Kind == model.KindMap && (!bytesPath || !reusesMapValues(leaf)) {
		b.WriteString("clear(v)\n")
	}
	b.WriteString("}\n")
}

// emitPointerAssign writes the assignments that follow the parse of a pointer
// field with depth levels. The leaf is already decoded into `v`. It reuses
// the non-nil start of the chain and allocates with `new(…)` only from the
// first nil level down. valExpr is `v` or a conversion such as `int(v)`.
func emitPointerAssign(b *bytes.Buffer, ref string, depth int, valExpr string) {
	for k := range depth {
		dk := derefStr(ref, k)
		kw := "if"
		if k > 0 {
			kw = "} else if"
		}
		fmt.Fprintf(b, "%s %s == nil {\n%s = %s\n", kw, dk, dk, newChain(valExpr, depth-k))
	}
	fmt.Fprintf(b, "} else {\n%s = %s\n}\n", derefStr(ref, depth), valExpr)
}

func renderField(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	// Converter variant. Look at the JSON shape and pick a branch. The
	// dispatch handles null, the native decode and the converter itself. The
	// field's value steps run after it.
	if model.FieldHasConverter(f) {
		renderVariantDispatch(b, f, ref, posVar)
		if !f.NoValidate {
			emitFieldPipe(b, f, ref, "i")
		}
		return
	}
	// A named primitive, as in `type B S; type S string`, scans the
	// underlying type into a temp and converts where it assigns. Calling the
	// alias's own DecodeFrom would cost a `ggen.String` call per field and lose
	// the inline window scan (see opt #47 in cmd/ggen/CLAUDE.md). An
	// unannotated named type has no methods to call at all, so it would fall
	// to SkipValue and encoding/json. The conversion is free: the underlying
	// type is identical, so the compiler emits no instruction for it. This
	// runs for the leaf after the pointer is removed, so `*Named` reaches it
	// through the leaf recursion.
	if prim, kind, ok := inlineNamedPrim(f); ok && !f.Pointer && !f.String {
		// `nullzero` stays on the outer field. It depends on AtDispatch, which
		// only the outer field has, and its zero literal uses the named type.
		nz := nullZeroApplies(f)
		flat := nz && nullBreakOK(f)
		if nz {
			inlineNullPeek(b, posVar, fieldLit(f))
			fmt.Fprintf(b, "%s = %s\n", ref, zeroLit(f.GoType, f.Kind))
			if flat {
				b.WriteString("break\n}\n")
			} else {
				b.WriteString("} else {\n")
			}
		}
		tmp := "named" + f.GoName
		var nb bytes.Buffer
		renderField(&nb, namedPrimInner(f, prim, kind), tmp, posVar)
		if rest, expr, ok := foldTailAssign(nb.String(), tmp); ok {
			// A temp written once folds straight into the conversion.
			b.WriteString(rest)
			fmt.Fprintf(b, "%s = %s(%s)\n", ref, f.GoType, expr)
		} else {
			fmt.Fprintf(b, "var %s %s\n", tmp, prim)
			b.WriteString(nb.String())
			fmt.Fprintf(b, "%s = %s(%s)\n", ref, f.GoType, tmp)
		}
		if nz && !flat {
			b.WriteString("}\n")
		}
		if !f.NoValidate {
			validateAndMod(b, f, ref)
		}
		return
	}
	// Pointers come first. A `*T` field with json:",string" must decode into T
	// and then take its address. The string-tag branch on `*T` would emit
	// broken code. The pointer block recurses with the leaf type T.
	if f.Pointer {
		// Custom `@Func` rules apply to the pointer type. Built-in rules apply
		// to the dereferenced leaf. A pipe that mixes both runs as one ordered
		// pass after the assignment (emitPointerPipe). A pipe with only one
		// kind keeps it on its own target: the leaf's steps run with its
		// decode, and the @ rules run on `ref` after the null branch joins
		// back.
		builtinV, customV := model.PartitionCustomValidation(f.Validation)
		builtinM, customM := model.PartitionCustomMods(f.Mods)
		builtinSteps, customSteps := model.SplitCustomSteps(fieldPipe(f))
		ordered := !f.NoValidate && len(builtinSteps) > 0 && len(customSteps) > 0
		// Decode into the receiver at any pointer depth. null sets the outer
		// pointer to nil, as stdlib does. Otherwise decode the leaf into the
		// temp `v` first, so a parse failure changes nothing. Then the
		// assignments reuse the non-nil start of the chain and allocate only
		// the nil rest. A leaf that can reuse memory is seeded first.
		depth, leafType := model.PointerDepth(f.GoType)
		leaf := f
		leaf.Pointer = false
		leaf.PointeeType = ""
		leaf.GoType = leafType
		leaf.Kind = leafKind(f, leafType)
		leafK := leaf.Kind
		leaf.Validation = builtinV
		leaf.Mods = builtinM
		leaf.Pipe = builtinSteps
		if ordered {
			leaf.Validation, leaf.Mods, leaf.Pipe = nil, nil, nil
		}
		leaf.AtDispatch = false
		leaf.TargetNil = false
		// A narrow integer leaf scans into the wide type and converts where
		// it assigns.
		scanType, castFn := leafType, ""
		if wide, wideKind, cast := widenedLeafCast(leaf.Kind, leafType); wide != "" {
			leaf.Kind, leaf.GoType, scanType, castFn = wideKind, wide, wide, cast
		}
		// At dispatch level the null branch breaks out to the comma handling.
		flat := nullBreakOK(f)
		inlineNullPeek(b, posVar, fieldLit(f))
		fmt.Fprintf(b, "%s = nil\n", ref)
		if ordered {
			// `@Func` steps take `*T`, so a nil pointer still reaches them.
			// The built-in steps have no leaf here.
			renderPipe(b, customSteps, ref, f.JSONName, f.GoType, f.Kind, f.MultiErr, posVar)
		}
		if flat {
			b.WriteString("break\n}\n")
		} else {
			b.WriteString("} else {\n")
		}
		// Fast path. The inline integer scanner already produces `n`, so a
		// leaf with no built-in rules needs no temp. The assignments use `n`.
		if (leaf.Kind == model.KindInt64 || leaf.Kind == model.KindUint64) && !f.String && len(builtinV) == 0 && len(builtinM) == 0 {
			valExpr := "n"
			if castFn != "" {
				valExpr = castFn + "(n)"
			}
			var cascade bytes.Buffer
			// Guard the narrowing conversion, so an out-of-range leaf is an
			// overflow error and does not wrap.
			errRet := fmt.Sprintf("return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrNumberOverflow)", posVar, fieldLit(f))
			cascade.WriteString(narrowIntGuard("n", castFn, errRet))
			if f.TargetNil {
				fmt.Fprintf(&cascade, "%s = %s\n", ref, newChain(valExpr, depth))
			} else {
				emitPointerAssign(&cascade, ref, depth, valExpr)
			}
			stmt := strings.TrimRight(cascade.String(), "\n")
			if leaf.Kind == model.KindInt64 {
				inlineScanInt64Stmt(b, posVar, fieldLit(f), stmt)
			} else {
				inlineScanUint64Stmt(b, posVar, fieldLit(f), stmt)
			}
		} else {
			valExpr := "v"
			if castFn != "" {
				valExpr = castFn + "(v)"
			}
			fmt.Fprintf(b, "var v %s\n", scanType)
			if !f.TargetNil {
				emitPointerSeed(b, ref, depth, leaf, true)
			}
			renderField(b, leaf, "v", posVar)
			b.WriteString(
				narrowIntGuard("v", castFn, fmt.Sprintf("return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrNumberOverflow)", posVar, fieldLit(f))),
			)
			if f.TargetNil {
				// The target is a `var x *T` known to be nil, so assign a new
				// chain directly.
				fmt.Fprintf(b, "%s = %s\n", ref, newChain(valExpr, depth))
			} else {
				emitPointerAssign(b, ref, depth, valExpr)
			}
		}
		if ordered {
			emitPointerPipe(b, f, ref, depth, leafType, leafK, posVar, false)
		}
		if !flat {
			b.WriteString("}\n")
		}
		if !ordered && !f.NoValidate && (len(customV) > 0 || len(customM) > 0) {
			outer := f
			outer.Validation = customV
			outer.Mods = customM
			outer.Pipe = customSteps
			validateAndMod(b, outer, ref)
		}
		return
	}
	// `nullzero` makes a non-pointer value field accept an explicit JSON null
	// as the Go zero value. At dispatch level the null branch breaks out.
	// Otherwise the value decode nests in an else, so validateAndMod also runs
	// on the zero.
	nz := nullZeroApplies(f)
	flat := nz && nullBreakOK(f)
	if nz {
		inlineNullPeek(b, posVar, fieldLit(f))
		fmt.Fprintf(b, "%s = %s\n", ref, zeroLit(f.GoType, f.Kind))
		if flat {
			b.WriteString("break\n}\n")
		} else {
			b.WriteString("} else {\n")
		}
	}
	// jsonv2 does not quote a bool for `,string`, so a bool takes the bare
	// decode.
	if f.String && f.Kind != model.KindBool {
		renderStringTag(b, f, ref, posVar)
		if nz && !flat {
			b.WriteString("}\n")
		}
		if !f.NoValidate {
			validateAndMod(b, f, ref)
		}
		return
	}
	field := fieldLit(f)
	switch f.Kind {
	case model.KindString:
		inlineScanString(b, posVar, ref, posVar, field, f.Copy, !f.AllowInvalidUTF8)
	case model.KindBool:
		fmt.Fprintf(b, "%[1]s, %[2]s, err = ggen.Bool(data, %[2]s)\n", ref, posVar)
		fmt.Fprintf(b, "if err != nil { %[1]s = ggen.BoolEnd(data, %[1]s); return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, err) }\n", posVar, field)
	case model.KindInt, model.KindInt8, model.KindInt16, model.KindInt32:
		inlineScanInt64(b, posVar, ref, f.GoType, field)
	case model.KindInt64:
		inlineScanInt64(b, posVar, ref, "", field)
	case model.KindUint, model.KindUint8, model.KindUint16, model.KindUint32:
		inlineScanUint64(b, posVar, ref, f.GoType, field)
	case model.KindUint64:
		inlineScanUint64(b, posVar, ref, "", field)
	case model.KindFloat32, model.KindFloat64:
		fmt.Fprintf(b, `%[1]s, %[2]s, err = ggen.%[4]s(data, %[2]s)
if err != nil { return result, %[2]s, ggen.NewParseErr(%[3]s, %[2]s, err) }
`, ref, posVar, field, floatScanFn(f.Kind))
	case model.KindStruct:
		if isGenerated(f.GoType) {
			// DecodeFrom is called on the field's current value, which it
			// resets while reusing its allocations. `consumed`, the number of
			// bytes read, advances posVar. Cyclic types go through the depth
			// core, so payload nesting stays bounded.
			fmt.Fprintf(b, `var consumed int
%[1]s, consumed, err = %[1]s.`+decodeCallFor(f.GoType)+`
%[2]s += consumed
%[3]s`, ref, posVar, nestedDecodeErrCheck(fieldLit(f), calleeTypeOf(f), f.MultiErr, true, "consumed"))
		} else {
			b.WriteString(renderCrossPkgStructDecode(f, ref, posVar))
		}
	case model.KindSlice:
		renderSlice(b, f, ref, posVar)
	case model.KindArray:
		emitByteArrayRead(b, f, ref, posVar, 0)
	case model.KindMap:
		renderMap(b, f, ref, posVar, false)
	case model.KindBytes:
		renderBytes(b, f, ref, posVar)
	case model.KindTime:
		renderTime(b, f, ref, posVar)
	case model.KindDuration:
		renderDuration(b, f, ref, posVar)
	case model.KindNetIP:
		renderNetIP(b, f, ref, posVar)
	case model.KindNetipAddr:
		renderNetipAddr(b, f, ref, posVar)
	case model.KindNetipPrefix:
		renderNetipPrefix(b, f, ref, posVar)
	case model.KindRawJSON:
		renderRawJSON(b, f, ref, posVar)
	case model.KindURL:
		renderURL(b, f, ref, posVar)
	case model.KindBigInt:
		renderBigInt(b, f, ref, posVar)
	case model.KindBigFloat:
		renderBigFloat(b, f, ref, posVar)
	case model.KindBigRat:
		renderBigRat(b, f, ref, posVar)
	case model.KindSQLNull:
		renderSQLNull(b, f, ref, posVar)
	case model.KindAny:
		renderAny(b, f, ref, posVar)
	default:
		fmt.Fprintf(b, `k, err := ggen.SkipValue(data, %[1]s)
%[2]s%[1]s = k
`, posVar, bytesErrCheck(fieldLit(f), posVar))
	}
	if nz && !flat {
		b.WriteString("}\n")
	}
	if !f.NoValidate {
		validateAndMod(b, f, ref)
	}
}

// renderSlice is the depth-0 entry into the recursive slice emitter for a
// struct field. It is not used for a top-level alias.
func renderSlice(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	emitByteSliceRead(b, f, ref, posVar, 0, false)
}

// peelSliceField returns the FieldInfo one level down, for the element of a
// slice of slices or a slice of arrays. Rules move up one level, so
// InnerValidation[0] becomes ElemValidation, and so on. It is called only
// when f.ElemKind is KindSlice or KindArray.
func peelSliceField(f model.FieldInfo) model.FieldInfo {
	innerGoType := f.ElemType
	innerElem, innerKind, innerLen := model.StripOneContainer(innerGoType)
	// Do what the parser does: one leading `*` moves into ElemPointer, and
	// any other stars stay on ElemType for the multi-level pointer code.
	innerPointer := false
	if strings.HasPrefix(innerElem, "*") {
		innerPointer = true
		innerElem = innerElem[1:]
		innerKind = model.ResolveKind(innerElem)
		if innerKind == model.KindArray {
			innerLen = model.ArrayLenFromType(innerElem)
		}
	}
	inner := model.FieldInfo{
		GoType:           innerGoType,
		GoName:           f.GoName,   // cap-const names keep the field attribution
		Kind:             f.ElemKind, // the layer one level down
		ArrayLen:         f.ElemArrayLen,
		ElemPointer:      innerPointer,
		ElemType:         innerElem,
		ElemKind:         innerKind,
		JSONName:         f.JSONName + "[]",
		MultiErr:         f.MultiErr,
		NoValidate:       f.NoValidate,
		AllowDups:        f.AllowDups,
		Copy:             f.Copy,
		AllowInvalidUTF8: f.AllowInvalidUTF8,
		// HintLen=-1 means "unset", so preallocCap uses its default. The zero
		// value 0 would mean opt out.
		HintLen: -1,
	}
	if innerKind == model.KindArray {
		inner.ElemArrayLen = innerLen
	}
	if len(f.InnerValidation) > 0 {
		inner.ElemValidation = f.InnerValidation[0]
		if len(f.InnerValidation) > 1 {
			inner.InnerValidation = f.InnerValidation[1:]
		}
	}
	if len(f.InnerMods) > 0 {
		inner.ElemMods = f.InnerMods[0]
		if len(f.InnerMods) > 1 {
			inner.InnerMods = f.InnerMods[1:]
		}
	}
	// Ordered steps per level. This level's element is the next level down.
	if len(f.Levels) > 1 {
		inner.Levels = f.Levels[1:]
	}
	// Prealloc hints per level shift down the same way. HintLevels[0] sizes
	// the rows of this level.
	if len(f.HintLevels) > 0 {
		inner.HintLen = f.HintLevels[0]
		inner.HintLevels = f.HintLevels[1:]
	}
	return inner
}

// elemSteps returns the ordered value steps for each element, the first inner
// level. It is Levels[0] when present. Otherwise it is built from the
// separate ElemMods and ElemValidation lists.
func elemSteps(f model.FieldInfo) []model.Step {
	if len(f.Levels) > 0 {
		return f.Levels[0]
	}
	return model.StepsFromLegacy(f.ElemMods, f.ElemValidation)
}

// emitByteArrayRead is a thin wrapper around emitByteSliceRead for [N]T
// struct fields. It is not used for a top-level alias.
func emitByteArrayRead(b *bytes.Buffer, f model.FieldInfo, dst, posVar string, depth int) {
	emitByteSliceRead(b, f, dst, posVar, depth, false)
}

// emitByteSliceRead emits a reader for a JSON array in `data` that fills
// `dst` and advances posVar. It handles slices, which are pre-sized with
// preallocCap and appended to, and fixed-length arrays, which are strict
// tuples: exactly f.ArrayLen elements or a LenError. Data locals such as evN,
// idxN and slabN end in the depth, so names do not collide. topLevel marks a
// decode of the whole value, a top-level container alias. Nothing follows the
// array, so each exit returns directly. It is false for struct fields and
// nested elements, whose caller has more to parse.
func emitByteSliceRead(b *bytes.Buffer, f model.FieldInfo, dst, posVar string, depth int, topLevel bool) {
	isArray := f.Kind == model.KindArray
	arrayN := f.ArrayLen
	// A multi-level pointer element such as `[]**T` gets no slab. Each
	// element runs the pointer code: parse the leaf, then assign the chain. A
	// `[]*T` keeps the faster slab path.
	_, elemDepth := elemPtrType(f)
	mptr := elemDepth >= 2
	// kvar is posVar itself. The whole nest shares one position counter, and
	// each level advances it past the bytes it read.
	kvar := posVar
	evvar := fmt.Sprintf("ev%d", depth)
	ivar := fmt.Sprintf("idx%d", depth)
	// No leading whitespace skip. Every place a value starts, after `:`, `[`
	// or `,`, already skipped it, and the alias path skips it explicitly.
	// `null` sets the slice to nil. Arrays do not accept null. At dispatch
	// level the null branch breaks out. A nested slot whose parent already
	// handled the null (NullDone) skips the check.
	flat := !isArray && nullBreakOK(f)
	if !isArray && !f.NullDone {
		inlineNullPeek(b, kvar, fieldLit(f))
		fmt.Fprintf(b, "%s = nil\n", dst)
		switch {
		case flat:
			b.WriteString("break\n}\n")
		case topLevel:
			// The alias is the whole value, so null returns at once. No else
			// is needed, because the non-null path also returns at the closing
			// bracket.
			fmt.Fprintf(b, "return result, %s, nil\n}\n", kvar)
		default:
			b.WriteString("} else {\n")
		}
	}
	fmt.Fprintf(
		b,
		"if %[1]s >= len(data) || data[%[1]s] != '[' { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrBadArray) }\n",
		kvar,
		fieldLit(f),
	)
	fmt.Fprintf(b, "%s++\n", kvar)
	inlineSkipWS(b, kvar)
	if isArray && arrayN == 0 {
		// `[0]T` has no slots, so only `[]` parses and there is nothing to
		// store. An element loop would index a zero-length array.
		fmt.Fprintf(b, "if %[1]s < len(data) && data[%[1]s] != ']' { return result, %[1]s, %[2]s }\n",
			kvar, tupleOverflowErr(f.JSONName, 0, kvar))
		fmt.Fprintf(
			b,
			"if %[1]s >= len(data) || data[%[1]s] != ']' { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrBadArray) }\n",
			kvar,
			fieldLit(f),
		)
		fmt.Fprintf(b, "%s++\n", posVar)
		if topLevel {
			fmt.Fprintf(b, "return result, %s, nil\n", posVar)
		}
		return
	}
	slabVar := fmt.Sprintf("slab%d", depth)
	if isArray {
		fmt.Fprintf(b, "var %s int\n", ivar)
		// An array of pointers gets a heap slab of exact size. A stack
		// `[N]E` would escape through `&slab[i]` anyway, so `make([]E, N)`
		// skips that step.
		if f.ElemPointer && !mptr {
			fmt.Fprintf(b, "%s := make([]%s, %d)\n", slabVar, f.ElemType, arrayN)
		}
	} else {
		sCap, slCap := preallocCap(f)
		countable := !f.ElemPointer && scalarCountable(f.ElemKind) && userPreallocHint(f) < 0
		// When the non-empty branch would also only do `dst = T{}`, with no
		// count, no capacity and no slab, both branches of the empty check
		// are the same. So emit one `if dst == nil` and skip the check.
		if !f.ElemPointer && !countable && sCap == "0" {
			fmt.Fprintf(b, "if %s == nil { %s = %s{} }\n", dst, dst, f.GoType)
		} else {
			if f.ElemPointer && !mptr {
				fmt.Fprintf(b, "var %s []%s\n", slabVar, f.ElemType)
			}
			// dst is the receiver's slice. It is nil when fresh, or reset to
			// [:0] with its backing kept. Allocate only when it is nil.
			fmt.Fprintf(b, "if %s < len(data) && data[%s] == ']' {\n", kvar, kvar)
			fmt.Fprintf(b, "if %s == nil { %s = %s{} }\n", dst, dst, f.GoType)
			fmt.Fprintf(b, "} else {\n")
			switch {
			case countable:
				// Exact capacity from one scan for delimiters. Number and bool
				// elements never contain `,` or `]`. This avoids repeated growth
				// without over-allocating. A reused slice keeps its backing.
				// Malformed input falls back to capacity 1.
				cntVar := fmt.Sprintf("cnt%d", depth)
				fmt.Fprintf(b, `if %[1]s == nil {
%[4]s := 1
if e := bytes.IndexByte(data[%[2]s:], ']'); e >= 0 { %[4]s = bytes.Count(data[%[2]s:%[2]s+e], []byte{','}) + 1 }
%[1]s = make(%[3]s, 0, %[4]s)
}
`, dst, kvar, f.GoType, cntVar)
			case sCap != "0":
				fmt.Fprintf(b, "if %s == nil { %s = make(%s, 0, %s) }\n", dst, dst, f.GoType, sCap)
			default:
				fmt.Fprintf(b, "if %s == nil { %s = %s{} }\n", dst, dst, f.GoType)
			}
			if f.ElemPointer && !mptr {
				fmt.Fprintf(b, "%s = make([]%s, 0, %s)\n", slabVar, f.ElemType, slCap)
			}
			fmt.Fprintf(b, "}\n")
		}
	}
	// Every element kind decodes in place into its final slot: slab[ivar] or
	// dst[ivar] for arrays, and the newly grown last slot of slab or dst for
	// slices. err is declared once at the top of DecodeFrom.
	directStruct := f.ElemKind == model.KindStruct && isGenerated(f.ElemType)
	_ = evvar // unused at this scope
	// A do-while loop. This one guard catches the empty `[]` and truncated
	// input, which keeps the ErrBadArray below. Inside, the comma handling's
	// continue and break drive the loop, so checking `data != ']'` for each
	// element is not needed.
	fmt.Fprintf(b, "if %s < len(data) && data[%s] != ']' {\nfor {\n", kvar, kvar)
	if isArray {
		// Strict tuple. Reject extra elements. Got counts the one extra
		// element, so the error reads as too many, unlike the too-few case.
		fmt.Fprintf(b, "if %s >= %d { return result, i, %s }\n",
			ivar, arrayN,
			tupleOverflowErr(f.JSONName, arrayN, "i"))
	}
	if f.ElemPointer && !mptr {
		// A `null` element is a nil pointer. Skip the parse and the slab.
		inlineNullPeek(b, kvar, fieldLit(f))
		if isArray {
			fmt.Fprintf(b, "%s[%s] = nil\n", dst, ivar)
			fmt.Fprintf(b, "%s++\n", ivar)
		} else {
			fmt.Fprintf(b, "%s = append(%s, nil)\n", dst, dst)
		}
		inlineSkipWS(b, kvar)
		fmt.Fprintf(b, "if %s < len(data) && data[%s] == ',' { %s++; ", kvar, kvar, kvar)
		inlineSkipWS(b, kvar)
		emitNoCloseAfterComma(b, fieldLit(f), kvar, ']')
		b.WriteString("continue }\nbreak\n}\n")
	}
	// Pick the slot to decode into. For slices, the slot is grown here.
	var target string
	switch {
	case isArray && mptr:
		target = fmt.Sprintf("%s[%s]", dst, ivar)
	case mptr:
		// Reslice within capacity so the old pointer chain stays in the slot
		// and can be reused. A grow past capacity starts with nil.
		if elemPtrReusable(f) {
			fmt.Fprintf(b, "if len(%[1]s) < cap(%[1]s) { %[1]s = %[1]s[:len(%[1]s)+1] } else { %[1]s = append(%[1]s, nil) }\n", dst)
		} else {
			fmt.Fprintf(b, "%s = append(%s, nil)\n", dst, dst)
		}
		target = fmt.Sprintf("%s[len(%s)-1]", dst, dst)
	case isArray && f.ElemPointer:
		target = fmt.Sprintf("%s[%s]", slabVar, ivar)
	case isArray:
		target = fmt.Sprintf("%s[%s]", dst, ivar)
		emitArraySlotBlank(b, f, target, true)
	case f.ElemPointer:
		// The slab holds the pointee type. Grow it by one slot.
		emitElemGrow(b, slabVar, f, directStruct)
		target = fmt.Sprintf("%s[len(%s)-1]", slabVar, slabVar)
	case f.ElemKind == model.KindSlice:
		// Slice element. Reslice within capacity so the old inner slice and
		// its backing stay for reuse. Only a grow past capacity adds a nil
		// slot.
		fmt.Fprintf(b, "if len(%[1]s) < cap(%[1]s) { %[1]s = %[1]s[:len(%[1]s)+1] } else { %[1]s = append(%[1]s, nil) }\n", dst)
		target = fmt.Sprintf("%s[len(%s)-1]", dst, dst)
	default:
		emitElemGrow(b, dst, f, directStruct)
		target = fmt.Sprintf("%s[len(%s)-1]", dst, dst)
	}
	field := fieldLit(f)
	errCheck := fmt.Sprintf("if err != nil { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, err) }\n", kvar, field)
	if mptr {
		// Element rules run inside the pointer code. There is no slab and no
		// step after the decode here. A slice slot reuses the old pointer
		// chain when elemPtrReusable allows it, and otherwise gets a new one
		// (TargetNil).
		pf := elemPtrField(f, f.JSONName+"[]")
		// An array slot always gets a new chain and only assigns, because
		// every array slot must be overwritten, never merged. This is pinned
		// by TestMerge_ArraySlotsOverwrite.
		pf.TargetNil = isArray || !elemPtrReusable(f)
		renderField(b, pf, target, kvar)
	} else {
		// A named-primitive element, as in `[]Priority`, scans the underlying
		// type into a temp and converts it into the slot, like the field
		// level. The element steps below still run against the named type.
		elemCast, elemTarget := "", ""
		savedElemType, savedElemKind := f.ElemType, f.ElemKind
		if prim, kind, ok := inlineNamedPrim(elemAsField(f)); ok {
			elemCast, elemTarget = f.ElemType, target
			target = fmt.Sprintf("namedElem%d", depth)
			fmt.Fprintf(b, "var %s %s\n", target, prim)
			f.ElemType, f.ElemKind = prim, kind
		}
		switch f.ElemKind {
		case model.KindString:
			inlineScanString(b, kvar, target, kvar, field, f.Copy, !f.AllowInvalidUTF8)
		case model.KindBool:
			fmt.Fprintf(b, "%s, %s, err = ggen.Bool(data, %s)\n", target, kvar, kvar)
			fmt.Fprintf(b, "if err != nil { %[1]s = ggen.BoolEnd(data, %[1]s) }\n", kvar)
			b.WriteString(errCheck)
		case model.KindInt, model.KindInt8, model.KindInt16, model.KindInt32, model.KindInt64:
			castFn := ""
			if f.ElemType != "int64" {
				castFn = f.ElemType
			}
			inlineScanInt64(b, kvar, target, castFn, field)
		case model.KindUint, model.KindUint8, model.KindUint16, model.KindUint32, model.KindUint64:
			castFn := ""
			if f.ElemType != "uint64" {
				castFn = f.ElemType
			}
			inlineScanUint64(b, kvar, target, castFn, field)
		case model.KindFloat32, model.KindFloat64:
			fmt.Fprintf(b, "%s, %s, err = ggen.%s(data, %s)\n", target, kvar, floatScanFn(f.ElemKind), kvar)
			b.WriteString(errCheck)
		case model.KindStruct:
			if directStruct {
				fmt.Fprintf(b, `var consumed int
%[1]s, consumed, err = %[1]s.`+decodeCallFor(f.ElemType)+`
%[2]s += consumed
`, target, kvar)
				b.WriteString(nestedDecodeErrCheck(fieldLit(f), calleeTypeOf(f), f.MultiErr, true, "consumed"))
			} else {
				// An element type from another package, or one with its own
				// methods, uses the same fallback order as the field level.
				b.WriteString(renderCrossPkgStructDecode(elemAsField(f), target, kvar))
			}
		case model.KindSlice, model.KindArray:
			// Nested container. Recurse with one outer [] or [N] removed.
			inner := peelSliceField(f)
			if inner.Kind == model.KindSlice && len(f.ElemMods) == 0 {
				// A `null` element gives a nil slot. Handle it here so the
				// inner body is not nested in an else, like the nil-element
				// fast path of []*T.
				inlineNullPeek(b, kvar, fieldLit(f))
				// The slot may hold an old slice from the receiver, so always
				// set it to nil.
				fmt.Fprintf(b, "%s = nil\n", target)
				if len(f.ElemValidation) > 0 {
					renderValidationOn(b, f.ElemValidation, target, f.JSONName+"[]", f.ElemType, f.ElemKind, f.MultiErr, "i")
				}
				if isArray {
					fmt.Fprintf(b, "%s++\n", ivar)
				}
				inlineSkipWS(b, kvar)
				fmt.Fprintf(b, "if %s < len(data) && data[%s] == ',' { %s++; ", kvar, kvar, kvar)
				inlineSkipWS(b, kvar)
				emitNoCloseAfterComma(b, fieldLit(f), kvar, ']')
				b.WriteString("continue }\nbreak\n}\n")
				inner.NullDone = true
			}
			// Copy the nested slot into a local `rowN` for this depth. The
			// inner loop then updates a local slice header instead of writing
			// into the parent's backing array for each element, which would
			// cost a GC write barrier each time. `target = rowN` stores the
			// finished row. The local starts from the old slot, reset to
			// [:0], so the inner decode reuses its backing array.
			row := fmt.Sprintf("row%d", depth)
			fmt.Fprintf(b, "%s := %s\n", row, target)
			if inner.Kind == model.KindSlice {
				fmt.Fprintf(b, "if %[1]s != nil { %[1]s = %[1]s[:0] }\n", row)
			}
			emitByteSliceRead(b, inner, row, kvar, depth+1, false)
			fmt.Fprintf(b, "%s = %s\n", target, row)
		default:
			// An element with a dedicated kind, such as time, duration,
			// bytes, raw, netip, url, big, sql.Null, any or map, uses the
			// field-level emitter with a cleaned-up FieldInfo.
			renderField(b, sliceElemField(f), target, kvar)
		}
		if elemCast != "" {
			fmt.Fprintf(b, "%s = %s(%s)\n", elemTarget, elemCast, target)
			target, f.ElemType, f.ElemKind = elemTarget, savedElemType, savedElemKind
		}
		renderPipe(b, elemSteps(f), target, f.JSONName+"[]", f.ElemType, f.ElemKind, f.MultiErr, "i")
	}
	switch {
	case isArray && f.ElemPointer && !mptr:
		// The slab slot was decoded in place. Store its address.
		fmt.Fprintf(b, "%s[%s] = &%s[%s]\n", dst, ivar, slabVar, ivar)
		fmt.Fprintf(b, "%s++\n", ivar)
	case isArray:
		fmt.Fprintf(b, "%s++\n", ivar)
	case f.ElemPointer && !mptr:
		// The last slab slot was decoded in place. Append its address.
		fmt.Fprintf(b, "%s = append(%s, &%s[len(%s)-1])\n", dst, dst, slabVar, slabVar)
	default:
	}
	inlineSkipWS(b, kvar)
	fmt.Fprintf(b, "if %s < len(data) && data[%s] == ',' { %s++; ", kvar, kvar, kvar)
	inlineSkipWS(b, kvar)
	emitNoCloseAfterComma(b, fieldLit(f), kvar, ']')
	b.WriteString("continue }\n")
	b.WriteString("break\n")
	b.WriteString("}\n}\n") // close for{} and the non-empty guard
	fmt.Fprintf(
		b,
		"if %[1]s >= len(data) || data[%[1]s] != ']' { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrBadArray) }\n",
		kvar,
		fieldLit(f),
	)
	if isArray {
		fmt.Fprintf(b, "if %s != %d { return result, i, %s }\n",
			ivar, arrayN,
			arrayLenErr(f.JSONName, arrayN, ivar, "i"))
	}
	fmt.Fprintf(b, "%s++\n", posVar)
	if topLevel {
		// The alias is the whole value. The array is read and nothing
		// follows, so return at once.
		fmt.Fprintf(b, "return result, %s, nil\n", posVar)
	}
	if !isArray && !flat && !f.NullDone && !topLevel {
		b.WriteString("}\n") // close the else of the null check
	}
}

// renderStreamDecode emits DecodeFromStream, the stream counterpart of
// renderDecode. It uses ggen.Stream methods that read more bytes when needed.
func renderStreamDecode(b *bytes.Buffer, s model.StructInfo) {
	// Named results avoid a struct copy on return, as in DecodeFrom above.
	var body bytes.Buffer
	if isCyclic(s.Name) {
		fmt.Fprintf(&body, "func (recv %s) DecodeFromStream(s *ggen.Stream) (%s, error) {\n\treturn recv.decodeFromStreamDepth(s, 0)\n}\n\n", s.Name, s.Name)
		fmt.Fprintf(&body, "func (recv %s) decodeFromStreamDepth(s *ggen.Stream, depth int) (result %s, err error) {\n", s.Name, s.Name)
	} else {
		fmt.Fprintf(&body, "func (recv %s) DecodeFromStream(s *ggen.Stream) (result %s, err error) {\n", s.Name, s.Name)
	}
	body.WriteString("result = recv\n")
	if isCyclic(s.Name) {
		body.WriteString("if depth > 10000 { // runtime maxDepth\n\treturn result, ggen.ErrMaxDepth\n}\n")
		if s.IsAlias {
			renderAliasStreamDecode(&body, s)
		} else {
			renderStreamDecodeStruct(&body, s)
		}
	} else {
		// Only structs that call into a cyclic nested type need `depth` in
		// scope, because streamDecodeCallFor emits `depth+1` there. Render
		// first and check, so the common acyclic struct does not carry an
		// unused constant.
		var rest bytes.Buffer
		if s.IsAlias {
			renderAliasStreamDecode(&rest, s)
		} else {
			renderStreamDecodeStruct(&rest, s)
		}
		if strings.Contains(rest.String(), "depth+1") {
			body.WriteString("const depth = 0\n")
		}
		body.Write(rest.Bytes())
	}
	body.WriteString("}\n\n")
	b.WriteString(tierStreamStringCalls(body.String(), s))
}

// tierStreamStringCalls rewrites the scan calls in a stream decode body to
// the chosen SIMD tier. `= s.String()`, `StringView`, `KeyView` and the skip
// calls become the Stream methods for that tier. Only the assignment form is
// matched, so nothing else can match, and encode bodies never pass through
// here. On structs where every key is 5 bytes or shorter, KeyView keeps its
// scalar version, for the same reason as the bytes-path key window: a few
// scalar steps beat the dependent vector instructions on short keys.
func tierStreamStringCalls(body string, s model.StructInfo) string {
	if simdSuffix == "" {
		return body
	}
	body = strings.ReplaceAll(body, "= s.String(", "= s.String"+simdSuffix+"(")
	body = strings.ReplaceAll(body, "= s.StringView(", "= s.StringView"+simdSuffix+"(")
	// The skip calls and the dispatch whitespace skip. On compact input the
	// tier versions take the same fast paths as the scalar ones. On input
	// with much whitespace they skip it with vector instructions.
	body = strings.ReplaceAll(body, "= s.SkipValue()", "= s.SkipValue"+simdSuffix+"()")
	body = strings.ReplaceAll(body, "= s.SkipSpace()", "= s.SkipSpace"+simdSuffix+"()")
	body = strings.ReplaceAll(body, "= s.CaptureValue()", "= s.CaptureValue"+simdSuffix+"()")
	if maxJSONNameLen(s) > 5 {
		body = strings.ReplaceAll(body, "= s.KeyView(", "= s.KeyView"+simdSuffix+"(")
	}
	return body
}

func renderStreamDecodeStruct(b *bytes.Buffer, s model.StructInfo) {
	emitReceiverReset(b, s, false)
	if s.MultiErr {
		b.WriteString("var errs ggen.Errors\n")
	}
	if useSeenBitmask(s) {
		b.WriteString(seenDecl(s))
	} else {
		for _, f := range s.Fields {
			if f.Embed {
				continue
			}
			if needsSeen(f) {
				fmt.Fprintf(b, "seen%s := false\n", f.GoName)
			}
		}
	}
	var pl bytes.Buffer
	renderStreamPostLoop(&pl, s)
	plStr := pl.String()
	// The dispatch runs right after KeyView, so the key, which aliases the
	// Stream's buffer, is still valid for the switch. ConsumeColon, at the
	// start of each case, may compact the buffer: it moves the unread bytes
	// to the front, which breaks the alias. So a case that keeps the key must
	// clone it first.
	chk := streamErrCheck(`""`)
	// When the reader has no more data, a refill returns the same error the
	// bytes path reports for the same truncated input. At a key position `{`
	// expects a name. After a value it expects ',' or '}'. Other reader
	// errors are returned as they are.
	rmore := func(sentinel string) string {
		return fmt.Sprintf(
			`if s.Pos >= len(s.Bytes()) { if err = s.ReadMore(s.Pos); err != nil { return result, ggen.NewParseErr("", s.Offset(), ggen.NotEOF(err, %s)) }; s.Pos = 0 }
`,
			sentinel,
		)
	}
	rmoreKey := rmore("ggen.ErrExpectString")
	rmoreSep := rmore("ggen.ErrBadObject")
	badObj := `return result, ggen.NewParseErr("", s.Offset(), ggen.ErrBadObject)`
	fmt.Fprintf(b, `err = s.ObjectOpen()
%[1]serr = s.SkipSpace()
%[1]s%[5]sif s.Bytes()[s.Pos] == '}' {
s.Pos++
%[2]sreturn result, nil
}
for {
	var key string
	key, err = s.KeyView(`+vArgS(s)+`)
	%[1]s%[3]s
	err = s.SkipSpace()
	%[1]s%[6]sc := s.Bytes()[s.Pos]
	if c == ',' {
		s.Pos++
		err = s.SkipSpace()
		%[1]scontinue
	}
	if c == '}' {
		s.Pos++
%[2]s		return result, nil
	}
	%[4]s
}`, chk, plStr, renderStreamDispatch(s), badObj, rmoreKey, rmoreSep)
}

func renderStreamDispatch(s model.StructInfo) string {
	// Each known-key case starts with s.ConsumeColon. The key alias is not
	// needed after that point, so the buffer shift it may cause is safe.
	emitField := func(b *bytes.Buffer, f model.FieldInfo, parse string) {
		field := fieldLit(f)
		chk := streamErrCheck(field)
		fmt.Fprintf(b, "err = s.ConsumeColon()\n%s", chk)
		if f.Embed || !needsSeen(f) {
			b.WriteString(parse)
			return
		}
		set := seenSet(s, f)
		seen := seenAccess(s, f)
		if s.AllowDups {
			fmt.Fprintf(b, `if %[1]s {
	err = s.SkipValue()
	%[4]s} else {
	%[2]s%[3]s
}
`, seen, set, parse, chk)
			return
		}
		if s.MultiErr {
			fmt.Fprintf(b, `if %[1]s {
	errs = append(errs, &ggen.DuplicateKeyError{Pos: s.Offset(), Path: []string{%[2]q}})
	err = s.SkipValue()
	%[5]s} else {
	%[3]s%[4]s
}
`, seen, f.JSONName, set, parse, chk)
			return
		}
		fmt.Fprintf(b, `if %s { return result, &ggen.DuplicateKeyError{Pos: s.Offset(), Path: []string{%q}} }
%s%s`, seen, f.JSONName, set, parse)
	}

	b := getSmall()
	defer putSmall(b)
	b.WriteString("switch key {\n")
	for _, f := range s.Fields {
		if f.Embed {
			continue
		}
		f.AtDispatch = true
		fmt.Fprintf(b, "case %q:\n", f.JSONName)
		emitField(b, f, renderStreamField(f, "result."+f.GoName, "s.Pos"))
	}
	b.WriteString("default:\n")
	b.WriteString(streamUnknownKey(s, "s.Pos"))
	b.WriteString("}\n")
	return b.String()
}

// renderStreamMap emits the stream-path decode of a map. `null` gives nil and
// an empty `{}` gives a non-nil empty map. A new map is made only when ref is
// nil. Map keys must be their own copies, because ReadMore can overwrite the
// buffer, so the keys are read with s.String.
func renderStreamMap(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	mapNest++
	defer func() { mapNest-- }()
	mkVar, mvVar, _, _ := mapLocals()
	field := fieldLit(f)
	chk := streamErrCheck(field)
	rm := streamReadMore(field, "0", false, "ggen.ErrBadObject")
	rmKi := strings.Replace(streamReadMore(field, "0", false, "ggen.ErrBadLiteral"), "if s.Pos >=", "if s.Pos+ki >=", 1)
	badLit := fmt.Sprintf("return result, ggen.NewParseErr(%s, s.Offset(), ggen.ErrBadLiteral)", field)
	badObj := fmt.Sprintf("return result, ggen.NewParseErr(%s, s.Offset(), ggen.ErrBadObject)", field)
	makeExpr := fmt.Sprintf("make(%s)", f.GoType)
	if hint := mapPreallocCap(f); hint > 0 {
		makeExpr = fmt.Sprintf("make(%s, %d)", f.GoType, hint)
	}
	// At dispatch level the null branch breaks out to the comma handling.
	flat := nullBreakOK(f)
	fmt.Fprintf(b, `err = s.SkipSpace()
%[3]s%[4]sif s.Bytes()[s.Pos] == 'n' {
	for ki := 1; ki < 4; ki++ {
		%[5]sif s.Bytes()[s.Pos+ki] != "null"[ki] { %[2]s }
	}
	s.Pos += 4
	%[1]s = nil
`, ref, badLit, chk, rm, rmKi)
	if flat {
		b.WriteString("break\n}\n")
	} else {
		b.WriteString("} else {\n")
	}
	fmt.Fprintf(b, `	err = s.ObjectOpen()
	%[4]serr = s.SkipSpace()
	%[4]s%[6]sif s.Bytes()[s.Pos] == '}' {
		if %[1]s == nil { %[1]s = %[2]s{} }
	} else {
		if %[1]s == nil { %[1]s = %[3]s }
	}
	for s.Bytes()[s.Pos] != '}' {
		var %[7]s string
		%[7]s, err = s.String(`+vArg(f)+`)
		%[4]s`, ref, f.GoType, makeExpr, chk, badLit, rm, mkVar)
	keyValidateAndModStream(b, f, "mk")
	fmt.Fprintf(b, `		err = s.SkipSpace()
		%[1]s%[3]sif s.Bytes()[s.Pos] != ':' { %[2]s }
		s.Pos++
		err = s.SkipSpace()
		%[1]s`, chk, badObj, rm)
	_ = posVar

	mapTarget := fmt.Sprintf("%s[%s]", ref, mkVar)
	if _, eDepth := elemPtrType(f); eDepth > 0 {
		// Pointer value at any depth. The same pointer code as the bytes path
		// assigns into the map slot. TargetNil makes it only assign.
		pf := elemPtrField(f, f.JSONName+".value")
		pf.TargetNil = true
		b.WriteString(renderStreamField(pf, mapTarget, posVar))
		fmt.Fprintf(b, `		err = s.SkipSpace()
		%[1]s%[3]sif s.Bytes()[s.Pos] == ',' { s.Pos++; err = s.SkipSpace(); %[1]s%[4]scontinue }
		break
	}
	if s.Bytes()[s.Pos] != '}' { %[2]s }
	s.Pos++
`, chk, badObj, rm, streamNoCloseAfterComma(field, '}'))
		if !flat {
			b.WriteString("}\n") // close the else of the null check
		}
		return
	}
	// A named-primitive value. See the bytes version in renderMap.
	valCast, valTarget := "", ""
	savedElemType, savedElemKind := f.ElemType, f.ElemKind
	if prim, kind, ok := inlineNamedPrim(elemAsField(f)); ok {
		valCast, valTarget = f.ElemType, mapTarget
		mapTarget = "namedVal"
		fmt.Fprintf(b, "var %s %s\n", mapTarget, prim)
		f.ElemType, f.ElemKind = prim, kind
	}
	switch f.ElemKind {
	case model.KindString:
		fmt.Fprintf(b, `%s, err = s.String(%s)
%s`, mapTarget, vArg(f), chk)
	case model.KindBool:
		fmt.Fprintf(b, `%s, err = s.Bool()
%s`, mapTarget, chk)
	case model.KindInt, model.KindInt8, model.KindInt16, model.KindInt32, model.KindInt64:
		if f.ElemType == "int64" {
			fmt.Fprintf(b, `%s, err = s.Int64()
%s`, mapTarget, chk)
		} else {
			g := narrowIntGuard("iv", f.ElemType, fmt.Sprintf("return result, ggen.NewParseErr(%s, s.Offset(), ggen.ErrNumberOverflow)", field))
			fmt.Fprintf(b, `var iv int64
iv, err = s.Int64()
%[3]s%[4]s%[1]s = %[2]s(iv)
`, mapTarget, f.ElemType, chk, g)
		}
	case model.KindUint, model.KindUint8, model.KindUint16, model.KindUint32, model.KindUint64:
		if f.ElemType == "uint64" {
			fmt.Fprintf(b, `%s, err = s.Uint64()
%s`, mapTarget, chk)
		} else {
			g := narrowIntGuard("uv", f.ElemType, fmt.Sprintf("return result, ggen.NewParseErr(%s, s.Offset(), ggen.ErrNumberOverflow)", field))
			fmt.Fprintf(b, `var uv uint64
uv, err = s.Uint64()
%[3]s%[4]s%[1]s = %[2]s(uv)
`, mapTarget, f.ElemType, chk, g)
		}
	case model.KindFloat32, model.KindFloat64:
		fmt.Fprintf(b, `%s, err = s.%s()
%s`, mapTarget, floatScanFn(f.ElemKind), chk)
	case model.KindStruct:
		if isGenerated(f.ElemType) {
			fmt.Fprintf(b, `var %[4]s %[1]s
%[4]s, err = %[4]s.`+streamDecodeCallFor(f.ElemType)+`
%[2]s%[3]s = %[4]s
`, f.ElemType, nestedDecodeErrCheck(fieldLit(f), calleeTypeOf(f), f.MultiErr, false, ""), mapTarget, mvVar)
		} else {
			fmt.Fprintf(b, "var %s %s\n", mvVar, f.ElemType)
			b.WriteString(renderCrossPkgStructStreamDecode(elemAsField(f), mvVar, ""))
			fmt.Fprintf(b, "%s = %s\n", mapTarget, mvVar)
		}
	default:
		// A value with a dedicated kind uses the field-level emitter, as in
		// renderMap. The braces keep its locals in their own scope, for the
		// same reason.
		vf := sliceElemField(f)
		vf.JSONName = f.JSONName + ".value"
		fmt.Fprintf(b, "{\nvar %s %s\n", mvVar, f.ElemType)
		b.WriteString(renderStreamField(vf, mvVar, "s.Pos"))
		fmt.Fprintf(b, "%s = %s\n}\n", mapTarget, mvVar)
	}
	if valCast != "" {
		fmt.Fprintf(b, "%s = %s(%s)\n", valTarget, valCast, mapTarget)
		mapTarget, f.ElemType, f.ElemKind = valTarget, savedElemType, savedElemKind
	}
	renderPipe(b, elemSteps(f), mapTarget, f.JSONName+".value", f.ElemType, f.ElemKind, f.MultiErr, "")
	fmt.Fprintf(b, `		err = s.SkipSpace()
		%[1]s%[3]sif s.Bytes()[s.Pos] == ',' { s.Pos++; err = s.SkipSpace(); %[1]s%[4]scontinue }
		break
	}
	if s.Bytes()[s.Pos] != '}' { %[2]s }
	s.Pos++
`, chk, badObj, rm, streamNoCloseAfterComma(field, '}'))
	if !flat {
		b.WriteString("}\n") // close the else of the null check
	}
}

// streamErrCheck returns the `if err != nil { return ... }` check after a
// stream scan call, with the field literal written in.
func streamErrCheck(field string) string {
	return fmt.Sprintf("if err != nil { return result, ggen.NewParseErr(%s, s.Offset(), err) }\n", field)
}

// bytesErrCheck is the bytes-path equivalent of streamErrCheck, with the
// three-value `return result, pos, err` form.
func bytesErrCheck(field, posVar string) string {
	return fmt.Sprintf("if err != nil { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, err) }\n", posVar, field)
}

// streamReadMore emits the check that refills the Stream's buffer when it is
// used up. keep controls compaction: `0` grows the buffer, and `s.Pos` drops
// the bytes already read. resetPos adds `s.Pos = 0` after a non-zero keep.
// When sentinel is set and the reader has no more data, the refill returns
// the same error the bytes path reports for the same truncated input (see
// opt #60 in cmd/ggen/CLAUDE.md). Other reader errors pass through as they
// are, via ggen.NotEOF.
//
//nolint:unparam // keep is per-site by design (see backlog)
func streamReadMore(field, keep string, resetPos bool, sentinel string) string {
	reset := ""
	if resetPos {
		reset = "; s.Pos = 0"
	}
	errExpr := "err"
	if sentinel != "" {
		errExpr = "ggen.NotEOF(err, " + sentinel + ")"
	}
	return fmt.Sprintf(
		"if s.Pos >= len(s.Bytes()) { if err = s.ReadMore(%s); err != nil { return result, ggen.NewParseErr(%s, s.Offset(), %s) }%s }\n",
		keep,
		field,
		errExpr,
		reset,
	)
}

// truncSentinel maps a value kind to the error the bytes path reports when the
// input ends where that value should start.
func truncSentinel(k model.TypeKind) string {
	//exhaustive:ignore not every kind applies here
	switch k {
	case model.KindInt, model.KindInt8, model.KindInt16, model.KindInt32, model.KindInt64,
		model.KindUint, model.KindUint8, model.KindUint16, model.KindUint32, model.KindUint64,
		model.KindFloat32, model.KindFloat64, model.KindBigInt:
		return "ggen.ErrBadNumber"
	case model.KindBool:
		return "ggen.ErrBadBool"
	case model.KindString, model.KindBytes, model.KindTime, model.KindDuration, model.KindNetIP,
		model.KindNetipAddr, model.KindNetipPrefix, model.KindURL, model.KindBigFloat, model.KindBigRat:
		return "ggen.ErrExpectString"
	case model.KindMap, model.KindStruct:
		return "ggen.ErrBadObject"
	case model.KindSlice, model.KindArray:
		return "ggen.ErrBadArray"
	}
	return "ggen.ErrUnexpectedEnd"
}

// headSentinel is truncSentinel for a field. The error follows the JSON shape
// expected at the start of the value, not the Go kind. A pointer chain
// decodes its leaf, a sql.Null* decodes its inner value, a `,string` value
// starts with a quote, and a format:array []byte is a JSON array.
func headSentinel(f model.FieldInfo) string {
	goType, kind := f.GoType, f.Kind
	if f.Pointer {
		_, goType = model.PointerDepth(f.GoType)
		kind = leafKind(f, goType)
	}
	if kind == model.KindSQLNull {
		if spec, ok := model.SQLNullSpec(goType); ok {
			goType, kind = spec.Type, spec.Inner
		}
	}
	if f.String && kind != model.KindBool {
		return "ggen.ErrExpectString"
	}
	if kind == model.KindBytes && f.Format == "array" {
		return "ggen.ErrBadArray"
	}
	return truncSentinel(effectiveKind(goType, kind))
}

// calleeTypeOf names the type whose DecodeFrom a field's nested decode calls.
// That is the element type for containers, and otherwise the field's own type
// without pointers.
func calleeTypeOf(f model.FieldInfo) string {
	//exhaustive:ignore not every kind applies here
	switch f.Kind {
	case model.KindSlice, model.KindArray, model.KindMap:
		return f.ElemType
	}
	if f.ElemType != "" && (f.ElemKind == model.KindStruct) {
		return f.ElemType
	}
	if f.PointeeType != "" {
		return f.PointeeType
	}
	return f.GoType
}

// calleeDrains reports whether a nested callee of type t reads the whole
// value before it reports a validation failure. Only a multierr callee does.
// A single-error callee returns at its first failure, in the middle of the
// value. A parent that recorded that error and kept going would continue
// from the wrong position.
func calleeDrains(t string) bool {
	_, ok := multiErrTypes[strings.TrimLeft(t, "*")]
	return ok
}

// nestedDecodeErrCheck emits the `if err != nil { … }` check after a nested
// DecodeFrom call. The outer field name goes into the wrap, so it is
// prepended to the inner *ParseError's path: "addr" and "street" give
// "addr.street". In multierr mode, validation failures are collected with
// Errors.Append, and parse errors return at once. bytesPath picks the
// three-value or two-value return.
//
// callee is the callee's type name, or "" when unknown. It decides whether
// the multierr collection is allowed (see calleeDrains).
//
// nVar names the bytes-path local that holds the bytes read, `consumed` or
// `entryConsumed`. The cursor was already advanced by it before this check,
// so the nested value started at i-nVar. The callee's error positions are
// relative to its sub-slice and are shifted by that amount
// (NewParseErrShift, ShiftPos). Stream positions are already payload offsets.
func nestedDecodeErrCheck(field, callee string, multierr, bytesPath bool, nVar string) string {
	wrap := fmt.Sprintf("ggen.NewParseErr(%s, s.Offset(), err)", field)
	ret := "return result, " + wrap
	drain := fmt.Sprintf("errs.Append(%s, verr)", field)
	if bytesPath {
		wrap = fmt.Sprintf("ggen.NewParseErrShift(%s, i, %s, err)", field, nVar)
		ret = "return result, i, " + wrap
		drain = fmt.Sprintf("errs.Append(%s, ggen.ShiftPos(verr, i-%s))", field, nVar)
	}
	// Collecting the error and continuing is only safe when the callee read
	// the whole value. Otherwise its error stops this decoder too.
	if !multierr || !calleeDrains(callee) {
		return fmt.Sprintf("if err != nil { %s }\n", ret)
	}
	return fmt.Sprintf(`if err != nil {
	if verr, ok := err.(ggen.Error); ok {
		%[1]s
	} else {
		%[2]s
	}
}
`, drain, ret)
}

// --- stream native-type renderers ---

func renderStreamBytes(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	if byteArrayLen(f) > 0 {
		renderStreamBytesValue(b, f, ref, posVar) // no `null` form, see renderBytes
		return
	}
	field := fieldLit(f)
	rm := streamReadMore(field, "0", false, headSentinel(f))
	rmKi := strings.Replace(streamReadMore(field, "0", false, "ggen.ErrBadLiteral"), "if s.Pos >=", "if s.Pos+ki >=", 1)
	// `null` sets the field to nil (see renderBytes).
	fmt.Fprintf(b, `%[3]sif s.Bytes()[s.Pos] == 'n' {
	for ki := 1; ki < 4; ki++ {
		%[4]sif s.Bytes()[s.Pos+ki] != "null"[ki] { return result, ggen.NewParseErr(%[2]s, s.Offset(), ggen.ErrBadLiteral) }
	}
	s.Pos += 4
	%[1]s = nil
`, ref, field, rm, rmKi)
	if nullBreakOK(f) {
		b.WriteString("break\n}\n")
		renderStreamBytesValue(b, f, ref, posVar)
		return
	}
	b.WriteString("} else {\n")
	renderStreamBytesValue(b, f, ref, posVar)
	b.WriteString("}\n")
}

func renderStreamBytesValue(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	_ = posVar // the stream path uses s.Pos directly
	field := fieldLit(f)
	chk := streamErrCheck(field)
	rm := streamReadMore(field, "0", false, "ggen.ErrBadArray")
	badArr := fmt.Sprintf("return result, ggen.NewParseErr(%s, s.Offset(), ggen.ErrBadArray)", field)
	if f.Format == "array" {
		// The temp is `u`, not `v`, because a pointer-leaf caller declares
		// `var v []byte` in the same scope.
		fmt.Fprintf(b, `err = s.ArrayOpen()
%[2]serr = s.SkipSpace()
%[2]s%[4]sfor s.Bytes()[s.Pos] != ']' {
	var u uint64
	u, err = s.Uint64()
	%[2]sif u > 255 { return result, ggen.NewParseErr(%[6]s, s.Offset(), ggen.ErrNumberOverflow) }
	%[1]s = append(%[1]s, byte(u))
	err = s.SkipSpace()
	%[2]s%[4]sif s.Bytes()[s.Pos] == ',' { s.Pos++; err = s.SkipSpace(); %[2]s%[5]scontinue }
	break
}
if s.Bytes()[s.Pos] != ']' { %[3]s }
s.Pos++
`, ref, chk, badArr, rm, streamNoCloseAfterComma(field, ']'), field)
		emitEmptyBytesNonNil(b, ref)
		return
	}
	enc := "base64.StdEncoding"
	dlen := "base64.StdEncoding.DecodedLen"
	switch f.Format {
	case "base64url":
		enc = "base64.URLEncoding"
		dlen = "base64.URLEncoding.DecodedLen"
	case "base32":
		enc = "base32.StdEncoding"
		dlen = "base32.StdEncoding.DecodedLen"
	case "base32hex":
		enc = "base32.HexEncoding"
		dlen = "base32.HexEncoding.DecodedLen"
	case "base16", "hex":
		enc = ""
	}
	if n := byteArrayLen(f); n > 0 {
		dec := enc + ".AppendDecode"
		if enc == "" {
			dec = "hex.AppendDecode"
		}
		tmp := "buf" + sanitizeIdent(f.GoName)
		fmt.Fprintf(b, `var sv string
sv, err = s.StringView(`+vArg(f)+`)
%[2]svar %[1]s [%[3]d]byte
var %[1]sd []byte
%[1]sd, err = %[4]s(%[1]s[:0], unsafe.Slice(unsafe.StringData(sv), len(sv)))
%[2]sif len(%[1]sd) != %[3]d { return result, %[5]s }
copy(%[6]s[:], %[1]sd)
`, tmp, chk, n, dec, arrayLenErr(f.JSONName, n, "len("+tmp+"d)", "s.Offset()"), ref)
		return
	}
	// The temp is `sv`, not `v`, because a pointer-leaf caller declares
	// `var v []byte` here. StringView aliases s.buf. AppendDecode uses it
	// before the next Stream call and keeps no bytes. See Stream.StringView.
	if enc == "" {
		fmt.Fprintf(b, `var sv string
sv, err = s.StringView(`+vArg(f)+`)
%[2]sif cap(%[1]s) < hex.DecodedLen(len(sv)) { %[1]s = make([]byte, 0, hex.DecodedLen(len(sv))) }
%[1]s, err = hex.AppendDecode(%[1]s, unsafe.Slice(unsafe.StringData(sv), len(sv)))
%[2]s`, ref, chk)
		emitEmptyBytesNonNil(b, ref)
		return
	}
	fmt.Fprintf(b, `var sv string
sv, err = s.StringView(`+vArg(f)+`)
%[4]sif cap(%[1]s) < %[2]s(len(sv)) { %[1]s = make([]byte, 0, %[2]s(len(sv))) }
%[1]s, err = %[3]s.AppendDecode(%[1]s, unsafe.Slice(unsafe.StringData(sv), len(sv)))
%[4]s`, ref, dlen, enc, chk)
	emitEmptyBytesNonNil(b, ref)
}

func renderStreamTime(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	_ = posVar
	field := fieldLit(f)
	chk := streamErrCheck(field)
	layout, numeric := timeLayoutExpr(f.Format)
	if numeric != "" {
		if numeric == "Unix" {
			fmt.Fprintf(b, `var f float64
f, err = s.Float64()
`+chk+`
sec := int64(f)
nsec := int64((f - float64(sec)) * 1e9)
%s = time.Unix(sec, nsec)
`, ref)
			return
		}
		ctor := map[string]string{
			"UnixMilli": "time.UnixMilli(n)",
			"UnixMicro": "time.UnixMicro(n)",
			"UnixNano":  "time.Unix(0, n)",
		}[numeric]
		fmt.Fprintf(b, `var n int64
n, err = s.Int64()
%[3]s%[1]s = %[2]s
`, ref, ctor, chk)
		return
	}
	// The temp is `sv`, not `v`, because a pointer-leaf caller declares
	// `var v time.Time` here. StringView aliases s.buf, and time.Parse keeps
	// none of it. See Stream.StringView.
	fmt.Fprintf(b, `var sv string
sv, err = s.StringView(`+vArg(f)+`)
%[3]s%[1]s, err = %[2]s
%[3]s`, ref, timeParseExpr(f.Format, layout, "sv"), chk)
}

func renderStreamDuration(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	_ = posVar
	field := fieldLit(f)
	chk := streamErrCheck(field)
	// The temps are `f` and `sv`, not `v`, because a pointer-leaf caller
	// declares `var v time.Duration` here.
	switch f.Format {
	case "sec":
		fmt.Fprintf(b, `var f float64
f, err = s.Float64()
%[2]s%[1]s = time.Duration(f * float64(time.Second))
`, ref, chk)
		return
	case "milli", "micro", "nano":
		unit := map[string]string{
			"milli": "time.Millisecond",
			"micro": "time.Microsecond",
			"nano":  "time.Nanosecond",
		}[f.Format]
		fmt.Fprintf(b, `var n int64
n, err = s.Int64()
%[3]s%[1]s = time.Duration(n) * %[2]s
`, ref, unit, chk)
		return
	}
	// StringView is safe, because ParseDuration keeps none of its input. See
	// Stream.StringView.
	fmt.Fprintf(b, `var sv string
sv, err = s.StringView(`+vArg(f)+`)
%[2]s%[1]s, err = time.ParseDuration(sv)
%[2]s`, ref, chk)
}

// renderStreamNetIP and the netip functions below are the stream versions of
// renderNetIP, renderNetipAddr and renderNetipPrefix. "" is the zero value,
// and null sets a net.IP to nil.
func renderStreamNetIP(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	_ = posVar
	field := fieldLit(f)
	chk := streamErrCheck(field)
	flat := nullBreakOK(f)
	emitStreamNullZero(b, ref, "nil", field, flat, "ggen.ErrExpectString")
	// StringView is safe for the parse, because net.ParseIP copies into a new
	// IP. The error literal keeps sv, so it is cloned. string(sv) on a string
	// converts nothing and does not copy. strings.Clone does copy.
	fmt.Fprintf(b, `var sv string
sv, err = s.StringView(`+vArg(f)+`)
%[2]sif sv == "" { %[1]s = nil } else {
%[1]s = net.ParseIP(sv)
if %[1]s == nil { return result, ggen.NewParseErr(%[3]s, s.Offset(), &net.ParseError{Type: "IP address", Text: strings.Clone(sv)}) }
}
`, ref, chk, field)
	if !flat {
		b.WriteString("}\n")
	}
}

func renderStreamNetipAddr(b *bytes.Buffer, f model.FieldInfo, ref string) {
	renderStreamNetipParse(b, f, ref, "netip.ParseAddr", "netip.Addr{}")
}

func renderStreamNetipPrefix(b *bytes.Buffer, f model.FieldInfo, ref string) {
	renderStreamNetipParse(b, f, ref, "netip.ParsePrefix", "netip.Prefix{}")
}

// renderStreamNetipParse scans with StringView. netip.Addr and netip.Prefix
// are value types, and the unique package copies zones, so a successful
// parse keeps no input bytes. "" is the zero value, which is what the encoder
// writes for it. The error value keeps the input string as is
// (parseAddrError.in, quoted by Error()). So the error branch parses a
// detached copy again, and the buffer can be reused as soon as decode
// returns.
func renderStreamNetipParse(b *bytes.Buffer, f model.FieldInfo, ref, parse, zero string) {
	field := fieldLit(f)
	fmt.Fprintf(b, `var sv string
sv, err = s.StringView(`+vArg(f)+`)
%[3]sif sv == "" { %[1]s = %[5]s } else {
%[1]s, err = %[2]s(sv)
if err != nil { _, err = %[2]s(strings.Clone(sv)); return result, ggen.NewParseErr(%[4]s, s.Offset(), err) }
}
`, ref, parse, streamErrCheck(field), field, zero)
}

// renderStreamRawJSON copies the captured bytes into the field. CaptureValue
// returns a slice that aliases the buffer, so the append detaches it. The
// receiver's backing array is reused when it has room, like the bytes path in
// copy mode.
func renderStreamRawJSON(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	_ = posVar
	chk := streamErrCheck(fieldLit(f))
	check := "err = ggen.CheckUTF8(span)\n%[2]s"
	if f.AllowInvalidUTF8 {
		check = ""
	}
	fmt.Fprintf(b, `span, err := s.CaptureValue()
%[2]s`+check+`%[1]s = append(%[1]s[:0], span...)
`, ref, chk)
}

func renderStreamURL(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	_ = posVar
	chk := streamErrCheck(fieldLit(f))
	fmt.Fprintf(b, `var sv string
sv, err = s.String(`+vArg(f)+`)
%[2]su, err := url.Parse(sv)
%[2]s%[1]s = *u
`, ref, chk)
}

func renderStreamBigInt(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	_ = posVar
	field := fieldLit(f)
	chk := streamErrCheck(field)
	fmt.Fprintf(b, `span, err := s.CaptureValue()
%[2]sif _, ok := (&%[1]s).SetString(unsafe.String(unsafe.SliceData(span), len(span)), 10); !ok {
	return result, ggen.NewParseErr(%[3]s, s.Offset(), ggen.ErrBadNumber)
}
`, ref, chk, field)
}

func renderStreamBigFloat(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	_ = posVar
	field := fieldLit(f)
	chk := streamErrCheck(field)
	// StringView is safe, because (*big.Float).Parse keeps none of its input.
	// See Stream.StringView.
	fmt.Fprintf(b, `var sv string
sv, err = s.StringView(`+vArg(f)+`)
%[2]sif _, _, err := (&%[1]s).Parse(sv, 10); err != nil {
	return result, ggen.NewParseErr(%[3]s, s.Offset(), err)
}
`, ref, chk, field)
}

func renderStreamBigRat(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	_ = posVar
	field := fieldLit(f)
	chk := streamErrCheck(field)
	// StringView is safe, because (*big.Rat).SetString keeps none of its
	// input. See Stream.StringView.
	fmt.Fprintf(b, `var sv string
sv, err = s.StringView(`+vArg(f)+`)
%[2]sif _, ok := (&%[1]s).SetString(sv); !ok {
	return result, ggen.NewParseErr(%[3]s, s.Offset(), ggen.ErrBadNumber)
}
`, ref, chk, field)
}

// renderStreamSQLNull is the stream counterpart of renderSQLNull.
func renderStreamSQLNull(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	_ = posVar
	if f.SQLNullInner != nil {
		inner := sqlNullInnerField(f)
		field := fieldLit(f)
		rm := streamReadMore(field, "0", false, headSentinel(inner))
		rmKi := strings.Replace(streamReadMore(field, "0", false, "ggen.ErrBadLiteral"), "if s.Pos >=", "if s.Pos+ki >=", 1)
		dec := renderStreamField(inner, "nv", posVar)
		body, valExpr := dec, "nv"
		if rest, expr, ok := foldTailAssign(body, "nv"); ok {
			body, valExpr = rest, expr
		} else {
			body = "var nv " + inner.GoType + "\n" + body
		}
		fmt.Fprintf(b, `%[5]sif s.Bytes()[s.Pos] == 'n' {
	for ki := 1; ki < 4; ki++ {
		%[7]sif s.Bytes()[s.Pos+ki] != "null"[ki] { return result, ggen.NewParseErr(%[6]s, s.Offset(), ggen.ErrBadLiteral) }
	}
	%[1]s = sql.%[2]s{}
	s.Pos += 4
} else {
	%[3]s
	%[1]s = sql.%[2]s{V: %[4]s, Valid: true}
}
`, ref, sqlTypeName(f.GoType), body, valExpr, rm, field, rmKi)
		return
	}
	spec, ok := model.SQLNullSpec(f.GoType)
	if !ok {
		return
	}
	field := fieldLit(f)
	chk := streamErrCheck(field)
	overflow := fmt.Sprintf("return result, ggen.NewParseErr(%s, s.Offset(), ggen.ErrNumberOverflow)", field)
	var inner bytes.Buffer
	var valExpr string
	scanTmpl := func(typ, method string) {
		args := ""
		if method == "String" {
			args = vArg(f)
		}
		fmt.Fprintf(&inner, `var nv %[1]s
nv, err = s.%[2]s(%[4]s)
%[3]s`, typ, method, chk, args)
	}
	//exhaustive:ignore not every kind applies here
	switch spec.Inner {
	case model.KindString:
		scanTmpl("string", "String")
		valExpr = "nv"
	case model.KindBool:
		scanTmpl("bool", "Bool")
		valExpr = "nv"
	case model.KindInt, model.KindInt8, model.KindInt16, model.KindInt32, model.KindInt64:
		scanTmpl("int64", "Int64")
		valExpr = "nv"
		if spec.Type != "int64" {
			inner.WriteString(narrowIntGuard("nv", spec.Type, overflow))
			valExpr = spec.Type + "(nv)"
		}
	case model.KindUint, model.KindUint8, model.KindUint16, model.KindUint32, model.KindUint64:
		scanTmpl("uint64", "Uint64")
		valExpr = "nv"
		if spec.Type != "uint64" {
			inner.WriteString(narrowIntGuard("nv", spec.Type, overflow))
			valExpr = spec.Type + "(nv)"
		}
	case model.KindFloat32, model.KindFloat64:
		prim := kindPrimitiveName(spec.Inner)
		scanTmpl(prim, floatScanFn(spec.Inner))
		valExpr = "nv"
		if spec.Type != prim {
			valExpr = spec.Type + "(nv)"
		}
	case model.KindTime:
		tf := model.FieldInfo{
			JSONName: f.JSONName, Format: f.Format,
			Copy: f.Copy, AllowInvalidUTF8: f.AllowInvalidUTF8, MultiErr: f.MultiErr,
		}
		inner.WriteString("var nv time.Time\n")
		renderStreamTime(&inner, tf, "nv", posVar)
		valExpr = "nv"
	}
	rm := streamReadMore(field, "0", false, truncSentinel(spec.Inner))
	rmKi := strings.Replace(streamReadMore(field, "0", false, "ggen.ErrBadLiteral"), "if s.Pos >=", "if s.Pos+ki >=", 1)
	fmt.Fprintf(b, `%[6]sif s.Bytes()[s.Pos] == 'n' {
	for ki := 1; ki < 4; ki++ {
		%[8]sif s.Bytes()[s.Pos+ki] != "null"[ki] { return result, ggen.NewParseErr(%[7]s, s.Offset(), ggen.ErrBadLiteral) }
	}
	%[1]s = sql.%[2]s{}
	s.Pos += 4
} else {
	%[3]s
	%[1]s = sql.%[2]s{%[4]s: %[5]s, Valid: true}
}
`, ref, sqlTypeName(f.GoType), inner.String(), spec.Field, valExpr, rm, field, rmKi)
	_ = chk
}

func renderStreamAny(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	_ = posVar
	chk := streamErrCheck(fieldLit(f))
	fn := "s.Any"
	if f.UseNumber {
		fn = "s.AnyNumber"
	}
	fmt.Fprintf(b, `%[1]s, err = %[2]s(%[4]s)
%[3]s`, ref, fn, chk, vArg(f))
}

// streamUnknownKey is the stream counterpart of unknownKey. `key` aliases the
// Stream's buffer, and ConsumeColon may shift that buffer. So any use of the
// key after that point, such as a stored key or an error that keeps it, needs
// a clone made before ConsumeColon. Every branch reads the colon first. A
// missing colon then gives the same error as on the bytes path, and
// UnknownKeyError.Pos is the start of the value, as there.
func streamUnknownKey(s model.StructInfo, posVar string) string {
	_ = posVar
	if embedded := s.EmbedField(); embedded.Embed {
		chk := streamErrCheck("ownKey")
		prelude := fmt.Sprintf(`ownKey := strings.Clone(key)
err = s.ConsumeColon()
%[3]sif result.%[1]s == nil { result.%[1]s = make(%[2]s) }
`, embedded.GoName, embedded.GoType, chk)
		//exhaustive:ignore not every kind applies here
		switch embedded.ElemKind {
		case model.KindAny:
			anyFn := "s.Any"
			if s.UseNumber {
				anyFn = "s.AnyNumber"
			}
			return prelude + fmt.Sprintf(`result.%[1]s[ownKey], err = %[2]s(%[4]s)
%[3]s`, embedded.GoName, anyFn, chk, vArg(embedded))
		case model.KindString:
			return prelude + fmt.Sprintf(`result.%[1]s[ownKey], err = s.String(`+vArg(embedded)+`)
%[2]s`, embedded.GoName, chk)
		case model.KindStruct:
			if isGenerated(embedded.ElemType) {
				return prelude + fmt.Sprintf(`var entry %[1]s
entry, err = entry.`+streamDecodeCallFor(embedded.ElemType)+`
%[3]sresult.%[2]s[ownKey] = entry
`, embedded.ElemType, embedded.GoName, nestedDecodeErrCheck("ownKey", embedded.ElemType, s.MultiErr, false, ""))
			}
		}
		return prelude + fmt.Sprintf(`span, err := s.CaptureValue()
%[3]svar entry %[1]s
if err = json.Unmarshal(span, &entry); err != nil { return result, ggen.NewParseErr(ownKey, s.Offset(), err) }
result.%[2]s[ownKey] = entry
`, embedded.ElemType, embedded.GoName, chk)
	}
	if s.IgnoreUnknown {
		chk := streamErrCheck("ownKey")
		return fmt.Sprintf(`ownKey := strings.Clone(key)
err = s.ConsumeColon()
%[1]serr = s.SkipValue()
%[1]s`, chk)
	}
	chk := streamErrCheck("ownKey")
	if s.MultiErr {
		return fmt.Sprintf(`ownKey := strings.Clone(key)
err = s.ConsumeColon()
%[1]serrs = append(errs, &ggen.UnknownKeyError{Pos: s.Offset(), Path: []string{ownKey}})
err = s.SkipValue()
%[1]s`, chk)
	}
	return fmt.Sprintf(`ownKey := strings.Clone(key)
err = s.ConsumeColon()
%sreturn result, &ggen.UnknownKeyError{Pos: s.Offset(), Path: []string{ownKey}}
`, chk)
}

func renderStreamStringTag(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	_ = posVar // the stream path uses s.Pos directly
	field := fieldLit(f)
	chk := streamErrCheck(field)
	// No braces, as in renderStringTag. The parse uses KeyView's alias before
	// anything can shift the buffer.
	b.WriteString("var sv string\n")
	fmt.Fprintf(b, "sv, err = s.KeyView(%s)\n", vArg(f))
	b.WriteString(chk)
	if f.Kind == model.KindBool {
		fmt.Fprintf(b, `switch sv {
case "true": %[1]s = true
case "false": %[1]s = false
default: return result, ggen.NewParseErr(%[2]s, s.Offset(), ggen.ErrBadBool)
}
`, ref, field)
		return
	}
	renderQuotedNumber(b, f, ref, fmt.Sprintf("return result, ggen.NewParseErr(%s, s.Offset(), ", field))
}

func renderStreamField(f model.FieldInfo, ref, posVar string) string {
	// Converter variants come first, as in renderField. The converter's result
	// already has the field's type, which may be a named type. Sending a
	// named-primitive field through the temp of the underlying type below
	// would assign `cv`, of type T, into a temp of T's underlying type.
	if model.FieldHasConverter(f) {
		out := renderVariantDispatchStream(f, ref, posVar)
		if !f.NoValidate {
			var vb bytes.Buffer
			emitFieldPipe(&vb, f, ref, "")
			out += vb.String()
		}
		return out
	}
	// A named primitive, as in renderField. Read the underlying type and
	// convert where it assigns.
	if prim, kind, ok := inlineNamedPrim(f); ok && !f.Pointer && !f.String {
		var out string
		nz := nullZeroApplies(f)
		flat := nz && nullBreakOK(f)
		if nz {
			var nb bytes.Buffer
			emitStreamNullZero(&nb, ref, zeroLit(f.GoType, f.Kind), fieldLit(f), flat, headSentinel(f))
			out = nb.String()
		}
		tmp := "named" + f.GoName
		if rest, expr, ok := foldTailAssign(renderStreamField(namedPrimInner(f, prim, kind), tmp, posVar), tmp); ok {
			// A temp written once folds straight into the conversion.
			out += rest
			out += fmt.Sprintf("%s = %s(%s)\n", ref, f.GoType, expr)
		} else {
			out += fmt.Sprintf("var %s %s\n", tmp, prim)
			out += renderStreamField(namedPrimInner(f, prim, kind), tmp, posVar)
			out += fmt.Sprintf("%s = %s(%s)\n", ref, f.GoType, tmp)
		}
		if nz && !flat {
			out += "}\n"
		}
		if !f.NoValidate {
			var vb bytes.Buffer
			validateAndModStream(&vb, f, ref)
			out += vb.String()
		}
		return out
	}
	// As in renderField, `,string` on a bool does nothing, to match jsonv2.
	// Pointer fields skip this and go to the pointer code below. Its
	// recursion on the leaf comes back here, because the string-tag branch on
	// `*T` would emit broken code.
	if f.String && f.Kind != model.KindBool && !f.Pointer {
		var out bytes.Buffer
		nz := nullZeroApplies(f)
		flat := nz && nullBreakOK(f)
		if nz {
			emitStreamNullZero(&out, ref, zeroLit(f.GoType, f.Kind), fieldLit(f), flat, headSentinel(f))
		}
		renderStreamStringTag(&out, f, ref, posVar)
		if nz && !flat {
			out.WriteString("}\n")
		}
		if !f.NoValidate {
			validateAndModStream(&out, f, ref)
		}
		return out.String()
	}
	b := getSmall()
	defer putSmall(b)
	field := fieldLit(f)
	chk := streamErrCheck(field)
	if f.Pointer {
		// As in renderField, custom @Func rules take the pointer and built-in
		// rules take the leaf. A pipe that mixes both runs as one ordered pass.
		builtinV, customV := model.PartitionCustomValidation(f.Validation)
		builtinM, customM := model.PartitionCustomMods(f.Mods)
		builtinSteps, customSteps := model.SplitCustomSteps(fieldPipe(f))
		ordered := !f.NoValidate && len(builtinSteps) > 0 && len(customSteps) > 0
		rm := streamReadMore(field, "0", false, headSentinel(f))
		rmKi := strings.Replace(streamReadMore(field, "0", false, "ggen.ErrBadLiteral"), "if s.Pos >=", "if s.Pos+ki >=", 1)
		// Decode into the receiver at any depth. See renderField.
		depth, leafType := model.PointerDepth(f.GoType)
		leaf := f
		leaf.Pointer = false
		leaf.PointeeType = ""
		leaf.GoType = leafType
		leaf.Kind = leafKind(f, leafType)
		leafK := leaf.Kind
		leaf.Validation = builtinV
		leaf.Mods = builtinM
		leaf.Pipe = builtinSteps
		if ordered {
			leaf.Validation, leaf.Mods, leaf.Pipe = nil, nil, nil
		}
		leaf.AtDispatch = false
		leaf.TargetNil = false
		// A narrow integer leaf scans into a wide temp and converts where it
		// assigns.
		scanType, valExpr, narrowCast := leafType, "v", ""
		if wide, wideKind, cast := widenedLeafCast(leaf.Kind, leafType); wide != "" {
			leaf.Kind, leaf.GoType, scanType, valExpr, narrowCast = wideKind, wide, wide, cast+"(v)", cast
		}
		// At dispatch level the null branch breaks out to the comma handling.
		flat := nullBreakOK(f)
		fmt.Fprintf(b, `%[2]sif s.Bytes()[s.Pos] == 'n' {
	for ki := 1; ki < 4; ki++ {
		%[3]sif s.Bytes()[s.Pos+ki] != "null"[ki] { return result, ggen.NewParseErr(%[4]s, s.Offset(), ggen.ErrBadLiteral) }
	}
	s.Pos += 4
	%[1]s = nil
`, ref, rm, rmKi, field)
		if ordered {
			// As in renderField, a nil pointer still reaches the @Func steps.
			renderPipe(b, customSteps, ref, f.JSONName, f.GoType, f.Kind, f.MultiErr, "")
		}
		if flat {
			b.WriteString("break\n}\n")
		} else {
			b.WriteString("} else {\n")
		}
		fmt.Fprintf(b, "var v %s\n", scanType)
		if !f.TargetNil {
			emitPointerSeed(b, ref, depth, leaf, false)
		}
		b.WriteString(renderStreamField(leaf, "v", posVar))
		b.WriteString(narrowIntGuard("v", narrowCast, fmt.Sprintf("return result, ggen.NewParseErr(%s, s.Offset(), ggen.ErrNumberOverflow)", field)))
		if f.TargetNil {
			// The target is a `var x *T` known to be nil, so assign a new
			// chain directly.
			fmt.Fprintf(b, "%s = %s\n", ref, newChain(valExpr, depth))
		} else {
			emitPointerAssign(b, ref, depth, valExpr)
		}
		if ordered {
			emitPointerPipe(b, f, ref, depth, leafType, leafK, "", false)
		}
		if !flat {
			b.WriteString("}\n")
		}
		if !ordered && !f.NoValidate && (len(customV) > 0 || len(customM) > 0) {
			outer := f
			outer.Validation = customV
			outer.Mods = customM
			outer.Pipe = customSteps
			validateAndModStream(b, outer, ref)
		}
		return b.String()
	}
	primScan := func(method string) {
		args := ""
		if method == "String" {
			args = vArg(f)
		}
		fmt.Fprintf(b, `%[1]s, err = s.%[2]s(%[4]s)
%[3]s`, ref, method, chk, args)
	}
	widenedScan := func(wideType, wideVar, method, castTo string) {
		guard := ""
		errRet := fmt.Sprintf("return result, ggen.NewParseErr(%s, s.Offset(), ggen.ErrNumberOverflow)", field)
		if method == "Int64" || method == "Uint64" {
			guard = narrowIntGuard(wideVar, castTo, errRet)
		}
		fmt.Fprintf(b, `var %[1]s %[2]s
%[1]s, err = s.%[3]s()
%[6]s%[7]s%[4]s = %[5]s(%[1]s)
`, wideVar, wideType, method, ref, castTo, chk, guard)
	}
	// `nullzero`, as in renderField.
	nz := nullZeroApplies(f)
	flat := nz && nullBreakOK(f)
	if nz {
		emitStreamNullZero(b, ref, zeroLit(f.GoType, f.Kind), field, flat, headSentinel(f))
	}
	switch f.Kind {
	case model.KindString:
		primScan("String")
	case model.KindBool:
		primScan("Bool")
	case model.KindInt, model.KindInt8, model.KindInt16, model.KindInt32:
		widenedScan("int64", "iv", "Int64", f.GoType)
	case model.KindInt64:
		primScan("Int64")
	case model.KindUint, model.KindUint8, model.KindUint16, model.KindUint32:
		widenedScan("uint64", "uv", "Uint64", f.GoType)
	case model.KindUint64:
		primScan("Uint64")
	case model.KindFloat32:
		primScan("Float32")
	case model.KindFloat64:
		primScan("Float64")
	case model.KindStruct:
		if isGenerated(f.GoType) {
			fmt.Fprintf(b, `%[1]s, err = %[1]s.`+streamDecodeCallFor(f.GoType)+`
%[2]s`, ref, nestedDecodeErrCheck(fieldLit(f), calleeTypeOf(f), f.MultiErr, false, ""))
		} else {
			b.WriteString(renderCrossPkgStructStreamDecode(f, ref, posVar))
		}
	case model.KindSlice:
		renderStreamSlice(b, f, ref, posVar)
	case model.KindArray:
		emitStreamSliceRead(b, f, ref, posVar, 0)
	case model.KindMap:
		renderStreamMap(b, f, ref, posVar)
	case model.KindBytes:
		renderStreamBytes(b, f, ref, posVar)
	case model.KindTime:
		renderStreamTime(b, f, ref, posVar)
	case model.KindDuration:
		renderStreamDuration(b, f, ref, posVar)
	case model.KindNetIP:
		renderStreamNetIP(b, f, ref, posVar)
	case model.KindNetipAddr:
		renderStreamNetipAddr(b, f, ref)
	case model.KindNetipPrefix:
		renderStreamNetipPrefix(b, f, ref)
	case model.KindRawJSON:
		renderStreamRawJSON(b, f, ref, posVar)
	case model.KindURL:
		renderStreamURL(b, f, ref, posVar)
	case model.KindBigInt:
		renderStreamBigInt(b, f, ref, posVar)
	case model.KindBigFloat:
		renderStreamBigFloat(b, f, ref, posVar)
	case model.KindBigRat:
		renderStreamBigRat(b, f, ref, posVar)
	case model.KindSQLNull:
		renderStreamSQLNull(b, f, ref, posVar)
	case model.KindAny:
		renderStreamAny(b, f, ref, posVar)
	default:
		fmt.Fprintf(b, `err = s.SkipValue()
%s`, chk)
	}
	if nz && !flat {
		b.WriteString("}\n")
	}
	if !f.NoValidate {
		validateAndModStream(b, f, ref)
	}
	return b.String()
}

func renderStreamSlice(b *bytes.Buffer, f model.FieldInfo, ref, posVar string) {
	emitStreamSliceRead(b, f, ref, posVar, 0)
}

// emitStreamSliceRead is the stream counterpart of emitByteSliceRead.
func emitStreamSliceRead(b *bytes.Buffer, f model.FieldInfo, dst, posVar string, depth int) {
	_ = posVar // the stream path uses s.Pos directly
	isArray := f.Kind == model.KindArray
	arrayN := f.ArrayLen
	// A multi-level pointer element uses the same pointer code as the bytes
	// path. Only `[]*T` gets a slab.
	_, elemDepth := elemPtrType(f)
	mptr := elemDepth >= 2
	ivar := fmt.Sprintf("idx%d", depth)
	slabVar := fmt.Sprintf("slab%d", depth)
	field := fieldLit(f)
	chk := streamErrCheck(field)
	rm := streamReadMore(field, "0", false, "ggen.ErrBadArray")
	rmKi := strings.Replace(streamReadMore(field, "0", false, "ggen.ErrBadLiteral"), "if s.Pos >=", "if s.Pos+ki >=", 1)
	// At dispatch level the null branch breaks out. A nested slot whose
	// parent already handled the null (NullDone) skips the check.
	flat := !isArray && nullBreakOK(f)
	if !isArray && !f.NullDone {
		fmt.Fprintf(b, `err = s.SkipSpace()
%[2]s%[4]sif s.Bytes()[s.Pos] == 'n' {
	for ki := 1; ki < 4; ki++ {
		%[5]sif s.Bytes()[s.Pos+ki] != "null"[ki] { return result, ggen.NewParseErr(%[3]s, s.Offset(), ggen.ErrBadLiteral) }
	}
	s.Pos += 4
	%[1]s = nil
`, dst, chk, field, rm, rmKi)
		if flat {
			b.WriteString("break\n}\n")
		} else {
			b.WriteString("} else {\n")
		}
	}
	fmt.Fprintf(b, `err = s.ArrayOpen()
%[1]serr = s.SkipSpace()
%[1]s%[2]s`, chk, rm)
	if isArray && arrayN == 0 {
		// No slots, so `[]` is the only tuple that fits. See
		// emitByteSliceRead.
		fmt.Fprintf(b, "if s.Bytes()[s.Pos] != ']' { return result, %s }\n", tupleOverflowErr(f.JSONName, 0, ""))
		b.WriteString("s.Pos++\n")
		return
	}
	if isArray {
		fmt.Fprintf(b, "var %s int\n", ivar)
		if f.ElemPointer && !mptr {
			fmt.Fprintf(b, "%s := make([]%s, %d)\n", slabVar, f.ElemType, arrayN)
		}
	} else {
		sCap, slCap := preallocCap(f)
		// dst is the receiver's slice. It is nil when fresh, or reset to [:0]
		// with its backing kept. Allocate only when it is nil.
		if f.ElemPointer && !mptr {
			fmt.Fprintf(b, "var %s []%s\n", slabVar, f.ElemType)
		}
		makeExpr := f.GoType + "{}"
		if sCap != "0" {
			makeExpr = fmt.Sprintf("make(%s, 0, %s)", f.GoType, sCap)
		}
		fmt.Fprintf(b, `if s.Bytes()[s.Pos] == ']' {
	if %[1]s == nil { %[1]s = %[2]s{} }
} else {
	if %[1]s == nil { %[1]s = %[3]s }
`, dst, f.GoType, makeExpr)
		if f.ElemPointer && !mptr {
			fmt.Fprintf(b, "%s = make([]%s, 0, %s)\n", slabVar, f.ElemType, slCap)
		}
		b.WriteString("}\n")
	}
	// Generated struct elements decode in place. See emitByteSliceRead.
	directStruct := f.ElemKind == model.KindStruct && isGenerated(f.ElemType)
	b.WriteString("for s.Bytes()[s.Pos] != ']' {\n")
	if isArray {
		// Reject extra elements. See emitByteSliceRead.
		fmt.Fprintf(b, "if %s >= %d { return result, %s }\n",
			ivar, arrayN,
			tupleOverflowErr(f.JSONName, arrayN, ""))
	}
	if f.ElemPointer && !mptr {
		// A `null` element is a nil pointer. Skip the parse and the slab.
		nilAssign := fmt.Sprintf("%s = append(%s, nil)\n", dst, dst)
		if isArray {
			nilAssign = fmt.Sprintf("%s[%s] = nil\n%s++\n", dst, ivar, ivar)
		}
		fmt.Fprintf(b, `%[4]sif s.Bytes()[s.Pos] == 'n' {
	for ki := 1; ki < 4; ki++ {
		%[5]sif s.Bytes()[s.Pos+ki] != "null"[ki] { return result, ggen.NewParseErr(%[3]s, s.Offset(), ggen.ErrBadLiteral) }
	}
	s.Pos += 4
	%[1]s	err = s.SkipSpace()
	%[2]s%[4]sif s.Bytes()[s.Pos] == ',' { s.Pos++; err = s.SkipSpace(); %[2]s%[6]scontinue }
	break
}
`, nilAssign, chk, field, rm, rmKi, streamNoCloseAfterComma(field, ']'))
	}
	// Pick the slot to decode into. For slices, the slot is grown here.
	var target string
	switch {
	case isArray && mptr:
		target = fmt.Sprintf("%s[%s]", dst, ivar)
	case mptr:
		// Reslice within capacity so the old pointer chain stays in the slot
		// and can be reused. A grow past capacity starts with nil.
		if elemPtrReusable(f) {
			fmt.Fprintf(b, "if len(%[1]s) < cap(%[1]s) { %[1]s = %[1]s[:len(%[1]s)+1] } else { %[1]s = append(%[1]s, nil) }\n", dst)
		} else {
			fmt.Fprintf(b, "%s = append(%s, nil)\n", dst, dst)
		}
		target = fmt.Sprintf("%s[len(%s)-1]", dst, dst)
	case isArray && f.ElemPointer:
		target = fmt.Sprintf("%s[%s]", slabVar, ivar)
	case isArray:
		target = fmt.Sprintf("%s[%s]", dst, ivar)
		emitArraySlotBlank(b, f, target, false)
	case f.ElemPointer:
		emitElemGrow(b, slabVar, f, directStruct)
		target = fmt.Sprintf("%s[len(%s)-1]", slabVar, slabVar)
	case f.ElemKind == model.KindSlice:
		// Reslice within capacity to keep the old inner slice. See
		// emitByteSliceRead.
		fmt.Fprintf(b, "if len(%[1]s) < cap(%[1]s) { %[1]s = %[1]s[:len(%[1]s)+1] } else { %[1]s = append(%[1]s, nil) }\n", dst)
		target = fmt.Sprintf("%s[len(%s)-1]", dst, dst)
	default:
		emitElemGrow(b, dst, f, directStruct)
		target = fmt.Sprintf("%s[len(%s)-1]", dst, dst)
	}
	if mptr {
		// Element rules run inside the pointer code. There is no slab and no
		// step after the decode here. A slice slot reuses the old pointer
		// chain when elemPtrReusable allows it, and otherwise gets a new one
		// (TargetNil).
		pf := elemPtrField(f, f.JSONName+"[]")
		// An array slot always gets a new chain and only assigns, because
		// every array slot must be overwritten, never merged. This is pinned
		// by TestMerge_ArraySlotsOverwrite.
		pf.TargetNil = isArray || !elemPtrReusable(f)
		b.WriteString(renderStreamField(pf, target, "s.Pos"))
	} else {
		// A named-primitive element. See emitByteSliceRead.
		elemCast, elemTarget := "", ""
		savedElemType, savedElemKind := f.ElemType, f.ElemKind
		if prim, kind, ok := inlineNamedPrim(elemAsField(f)); ok {
			elemCast, elemTarget = f.ElemType, target
			target = fmt.Sprintf("namedElem%d", depth)
			fmt.Fprintf(b, "var %s %s\n", target, prim)
			f.ElemType, f.ElemKind = prim, kind
		}
		switch f.ElemKind {
		case model.KindString:
			fmt.Fprintf(b, `%s, err = s.String(%s)
%s`, target, vArg(f), chk)
		case model.KindBool:
			fmt.Fprintf(b, `%s, err = s.Bool()
%s`, target, chk)
		case model.KindInt, model.KindInt8, model.KindInt16, model.KindInt32, model.KindInt64:
			if f.ElemType == "int64" {
				fmt.Fprintf(b, `%s, err = s.Int64()
%s`, target, chk)
			} else {
				g := narrowIntGuard("iv", f.ElemType, fmt.Sprintf("return result, ggen.NewParseErr(%s, s.Offset(), ggen.ErrNumberOverflow)", field))
				fmt.Fprintf(b, `var iv int64
iv, err = s.Int64()
%[3]s%[4]s%[1]s = %[2]s(iv)
`, target, f.ElemType, chk, g)
			}
		case model.KindUint, model.KindUint8, model.KindUint16, model.KindUint32, model.KindUint64:
			if f.ElemType == "uint64" {
				fmt.Fprintf(b, `%s, err = s.Uint64()
%s`, target, chk)
			} else {
				g := narrowIntGuard("uv", f.ElemType, fmt.Sprintf("return result, ggen.NewParseErr(%s, s.Offset(), ggen.ErrNumberOverflow)", field))
				fmt.Fprintf(b, `var uv uint64
uv, err = s.Uint64()
%[3]s%[4]s%[1]s = %[2]s(uv)
`, target, f.ElemType, chk, g)
			}
		case model.KindFloat32, model.KindFloat64:
			fmt.Fprintf(b, `%s, err = s.%s()
%s`, target, floatScanFn(f.ElemKind), chk)
		case model.KindStruct:
			if directStruct {
				fmt.Fprintf(b, `%[1]s, err = %[1]s.`+streamDecodeCallFor(f.ElemType)+`
%[2]s`, target, nestedDecodeErrCheck(fieldLit(f), calleeTypeOf(f), f.MultiErr, false, ""))
			} else {
				// An element type from another package uses the same
				// fallback order as the field level.
				b.WriteString(renderCrossPkgStructStreamDecode(elemAsField(f), target, ""))
			}
		case model.KindSlice, model.KindArray:
			inner := peelSliceField(f)
			if inner.Kind == model.KindSlice && len(f.ElemMods) == 0 {
				// A `null` element gives a nil slot. Handle it here so the
				// inner body is not nested in an else, like the nil-element
				// fast path of []*T.
				fmt.Fprintf(b, `%[2]sif s.Bytes()[s.Pos] == 'n' {
	for ki := 1; ki < 4; ki++ {
		%[3]sif s.Bytes()[s.Pos+ki] != "null"[ki] { return result, ggen.NewParseErr(%[1]s, s.Offset(), ggen.ErrBadLiteral) }
	}
	s.Pos += 4
`, field, rm, rmKi)
				// The slot may hold an old slice from the receiver, so always
				// set it to nil.
				fmt.Fprintf(b, "%s = nil\n", target)
				if len(f.ElemValidation) > 0 {
					renderValidationOn(b, f.ElemValidation, target, f.JSONName+"[]", f.ElemType, f.ElemKind, f.MultiErr, "")
				}
				if isArray {
					fmt.Fprintf(b, "%s++\n", ivar)
				}
				fmt.Fprintf(b, `err = s.SkipSpace()
%[1]s%[2]sif s.Bytes()[s.Pos] == ',' { s.Pos++; err = s.SkipSpace(); %[1]s%[3]scontinue }
break
}
`, chk, rm, streamNoCloseAfterComma(field, ']'))
				inner.NullDone = true
			}
			// Copy the nested slot into a local `rowN`, seeded from the old
			// slot so its backing array is reused, and store it back with
			// `target = rowN`. See emitByteSliceRead.
			row := fmt.Sprintf("row%d", depth)
			fmt.Fprintf(b, "%s := %s\n", row, target)
			if inner.Kind == model.KindSlice {
				fmt.Fprintf(b, "if %[1]s != nil { %[1]s = %[1]s[:0] }\n", row)
			}
			emitStreamSliceRead(b, inner, row, "s.Pos", depth+1)
			fmt.Fprintf(b, "%s = %s\n", target, row)
		default:
			// An element with a dedicated kind uses the field-level emitter.
			// See the bytes version in emitByteSliceRead.
			b.WriteString(renderStreamField(sliceElemField(f), target, "s.Pos"))
		}
		if elemCast != "" {
			fmt.Fprintf(b, "%s = %s(%s)\n", elemTarget, elemCast, target)
			target, f.ElemType, f.ElemKind = elemTarget, savedElemType, savedElemKind
		}
		renderPipe(b, elemSteps(f), target, f.JSONName+"[]", f.ElemType, f.ElemKind, f.MultiErr, "")
	}
	switch {
	case isArray && f.ElemPointer && !mptr:
		// The slab slot was decoded in place. Store its address.
		fmt.Fprintf(b, "%[1]s[%[2]s] = &%[3]s[%[2]s]\n%[2]s++\n", dst, ivar, slabVar)
	case isArray:
		fmt.Fprintf(b, "%s++\n", ivar)
	case f.ElemPointer && !mptr:
		// The last slab slot was decoded in place. Append its address.
		fmt.Fprintf(b, "%[1]s = append(%[1]s, &%[2]s[len(%[2]s)-1])\n", dst, slabVar)
	default:
	}
	fmt.Fprintf(b, `err = s.SkipSpace()
%[1]s%[3]sif s.Bytes()[s.Pos] == ',' { s.Pos++; err = s.SkipSpace(); %[1]s%[4]scontinue }
break
}
if s.Bytes()[s.Pos] != ']' { return result, ggen.NewParseErr(%[2]s, s.Offset(), ggen.ErrBadArray) }
`, chk, field, rm, streamNoCloseAfterComma(field, ']'))
	if isArray {
		fmt.Fprintf(b, "if %s != %d { return result, %s }\n",
			ivar, arrayN,
			arrayLenErr(f.JSONName, arrayN, ivar, ""))
	}
	b.WriteString("s.Pos++\n")
	if !isArray && !flat && !f.NullDone {
		b.WriteString("}\n") // close the else of the null check
	}
}

// generatedTypes is the set of struct names this generation pass emits code
// for. The emitters check it to decide between a direct DecodeFrom or
// AppendJSON call and the fallback for types from other packages or
// unannotated types.
var generatedTypes map[string]struct{}

// generatedFields holds the fields of each generated struct that encodes as a
// JSON object, next to generatedTypes. Aliases are left out because their
// methods delegate. ownsAllocations uses it to ask whether decoding into an
// existing value would reuse any memory, or only overwrite scalars. It lives
// as long as generatedTypes.
var generatedFields map[string][]model.FieldInfo

// namedKinds maps every named type in the pass whose underlying type is a
// primitive to that primitive's kind, such as "Count" to KindInt. It holds
// both annotated ones, which get a full method set so their fields report
// KindStruct, and plain ones the user never annotated. Rule and mod codegen
// reads it through effectiveKind, so the checks compile and convert through
// the underlying type. Struct and container aliases are not in it.
var namedKinds map[string]model.TypeKind

// aliasCodegen records the flags that change codegen, as each annotated
// primitive alias was generated with them. A field of such a type is decoded
// inline, scanning the underlying primitive and converting at the assignment,
// instead of calling the alias's own methods. That happens only when the
// alias would have emitted the same code as the inline path.
// `//ggen:generate htmlescape type HtmlString string` is the documented way
// to escape one type and not the rest. `copy` and `allowinvalidutf8` also
// change what a string decode emits. Inlining those with the parent's flags
// would silently drop the alias's behaviour.
type aliasCodegen struct {
	HTMLEscape       bool
	Copy             bool
	AllowInvalidUTF8 bool
	NoValidate       bool
}

var aliasFlags map[string]aliasCodegen

// isGenerated reports whether name is a struct in the current generation pass.
// Small wrapper to keep call sites readable.
func isGenerated(name string) bool {
	_, ok := generatedTypes[name]
	return ok
}
