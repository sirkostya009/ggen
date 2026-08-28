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
)

// smallPool recycles bytes.Buffer for the per-renderer temp buffers.
// bytes.Buffer.String() copies, so reusing the buf after Put can't corrupt
// prior callers' returned strings.
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

// generateTo renders the full set of structs into Go source and writes it to w.
// cyclicTypes marks generated structs that sit on a type-graph cycle
// (directly or mutually self-referential). Their decode methods get a
// depth-guarded core + thin public shim so payload nesting is bounded by
// maxDepth (10000) instead of the goroutine stack (a few MB of `{"kids":[…`
// is otherwise a fatal, unrecoverable stack overflow). Seeded by the caller
// alongside generatedTypes; computed from the bucket as a fallback.
var cyclicTypes map[string]struct{}

// multiErrTypes marks generated structs whose decoders ACCUMULATE validation
// failures (multierr) and therefore always consume the whole value before
// returning. A parent's multierr drain may only continue past a nested
// decode's validation error when the callee is in this set — a single-error
// callee returns mid-value, so continuing would parse from a desynced cursor
// (bogus ErrBadArray/ErrBadObject masking the real failure, or silently
// swallowed trailing fields). Seeded alongside generatedTypes.
var multiErrTypes map[string]struct{}

// seedMultiErrTypes fills multiErrTypes from the pass's structs.
func seedMultiErrTypes(structs []StructInfo) map[string]struct{} {
	out := make(map[string]struct{}, len(structs))
	for _, s := range structs {
		if s.MultiErr {
			out[s.Name] = struct{}{}
		}
	}
	return out
}

var goIdentRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// computeCyclicTypes over-approximates the reference graph by scraping type
// identifiers out of each field's type strings and intersecting with the
// generated set — an extra hit only costs a struct an unnecessary (correct)
// depth shim, so precision is not load-bearing.
func computeCyclicTypes(structs []StructInfo) map[string]struct{} {
	names := make(map[string]struct{}, len(structs))
	for _, s := range structs {
		names[s.Name] = struct{}{}
	}
	adj := make(map[string]map[string]struct{}, len(structs))
	var collect func(f FieldInfo, out map[string]struct{})
	collect = func(f FieldInfo, out map[string]struct{}) {
		for _, t := range []string{f.GoType, f.ElemType, f.PointeeType} {
			for _, id := range goIdentRe.FindAllString(t, -1) {
				if _, ok := names[id]; ok {
					out[id] = struct{}{}
				}
			}
		}
		if f.SQLNullInner != nil {
			collect(*f.SQLNullInner, out)
		}
	}
	for _, s := range structs {
		out := make(map[string]struct{})
		for _, f := range s.Fields {
			collect(f, out)
		}
		if s.IsAlias {
			for _, id := range goIdentRe.FindAllString(s.AliasUnderlying, -1) {
				if _, ok := names[id]; ok {
					out[id] = struct{}{}
				}
			}
		}
		adj[s.Name] = out
	}
	cyc := make(map[string]struct{})
	for name := range adj {
		// name is cyclic iff it can reach itself.
		seen := map[string]struct{}{}
		stack := make([]string, 0, 8)
		for n := range adj[name] {
			stack = append(stack, n)
		}
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if n == name {
				cyc[name] = struct{}{}
				stack = nil
				break
			}
			if _, ok := seen[n]; ok {
				continue
			}
			seen[n] = struct{}{}
			for m := range adj[n] {
				stack = append(stack, m)
			}
		}
	}
	return cyc
}

func isCyclic(typeName string) bool {
	_, ok := cyclicTypes[typeName]
	return ok
}

// decodeCallFor renders the bytes-path nested-decode call for a generated
// callee: cyclic types go through the depth core (uniform `depth+1` — every
// decoder has `depth` in scope, a const 0 in acyclic ones).
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

func generateTo(w io.Writer, pkg, scope string, structs []StructInfo) error {
	resetOneofRegistry(scope)
	resetCapRegistry()
	// generatedTypes is seeded by the caller with every struct in the same Go
	// package across ALL build-tag buckets, so a cross-bucket nested-struct
	// reference still routes to a direct DecodeFrom. Per-bucket fallback when unseeded.
	if generatedTypes == nil {
		generatedTypes = make(map[string]struct{}, len(structs))
		namedKinds = make(map[string]TypeKind)
		for _, s := range structs {
			generatedTypes[s.Name] = struct{}{}
		}
		seedNamedKinds(structs)
	}
	if multiErrTypes == nil {
		multiErrTypes = seedMultiErrTypes(structs)
	}
	if cyclicTypes == nil {
		cyclicTypes = computeCyclicTypes(structs)
	}
	// Sort fields alphabetically by JSON name (opt out with nosortkeys).
	// Embedded fallback fields stay at the end so comma emission stays tidy.
	for i := range structs {
		if structs[i].NoSort {
			continue
		}
		slices.SortStableFunc(structs[i].Fields, func(a, b FieldInfo) int {
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
	// Render each struct into its own buffer; bodies concatenate after the prelude.
	bodies := make([][]byte, len(structs))
	for i := range structs {
		renderStructMethods(buf, structs[i])
		count += buf.Len()
		bodies[i] = bytes.Clone(buf.Bytes())
		buf.Reset()
	}

	// The cap/oneof preamble decls spell element TYPES (`unsafe.Sizeof(*new(
	// json.RawMessage))`) the method bodies may never name — scan them too or
	// their imports go missing.
	scanBufs := bodies
	for _, decl := range capRegistry.decls {
		scanBufs = append(scanBufs, []byte(decl))
	}
	for _, decl := range oneofRegistry.decls {
		scanBufs = append(scanBufs, []byte(decl))
	}
	stdlib, third := collectImports(structs, scanBufs)

	// format.Source needs the whole file in one []byte. Pre-grow to the body
	// sum so the concat loop avoids a geometric grow.
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

// writePrelude emits the file header (build-tag, generated marker,
// package decl, import block) into buf.
func writePrelude(buf *bytes.Buffer, pkg, buildTag string, stdlib, third []string) {
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
	writeQuotedLine := func(p string) {
		buf.WriteByte('\t')
		buf.WriteByte('"')
		buf.WriteString(p)
		buf.WriteByte('"')
		buf.WriteByte('\n')
	}
	for _, p := range stdlib {
		writeQuotedLine(p)
	}
	if len(stdlib) > 0 && len(third) > 0 {
		buf.WriteByte('\n')
	}
	for _, p := range third {
		writeQuotedLine(p)
	}
	buf.WriteString(")\n\n")
}

// renderStructMethods writes the full method set for a single struct:
// DecodeFrom + DecodeFromStream + JSONSize + AppendJSON, plus optional
// MarshalJSON / UnmarshalJSON hooks.
func renderStructMethods(buf *bytes.Buffer, s StructInfo) {
	// Cap-const names carry the struct they were registered under
	// (ggenCap_<Struct>_<Field>_<elemType>) — see capName.
	currentStructName = s.Name
	renderDecode(buf, s)

	renderStreamDecode(buf, s)

	renderSize(buf, s)

	renderAppendJSON(buf, s)

	if s.Marshal {
		fmt.Fprintf(buf, "func (s %s) MarshalJSON() ([]byte, error) {\n\treturn ggen.Marshal(s)\n}\n\n", s.Name)
	}
	if s.Unmarshal {
		fmt.Fprintf(buf, "func (s *%s) UnmarshalJSON(data []byte) error {\n\tvar zero %s\n\tv, _, err := zero.DecodeFrom(data)\n\tif err != nil {\n\t\treturn err\n\t}\n\t*s = v\n\treturn nil\n}\n\n", s.Name, s.Name)
	}
}

// collectImports walks structs and returns the sorted set of import paths the
// generated file will reference, driven off StructInfo features plus a body-scan.
func collectImports(structs []StructInfo, bodies [][]byte) ([]string, []string) {
	need := map[string]struct{}{
		// The ggen runtime package is named by every generated method.
		"github.com/sirkostya009/ggen": {},
	}
	add := func(p string) {
		if p != "" {
			need[p] = struct{}{}
		}
	}
	// Candidate foreign packages (qualifier → path), narrowed to the ones a
	// rendered body actually names — see scanBodiesForForeignImports.
	foreign := map[string]string{}

	// Per-feature walk: each match flips on its imports.
	anyMarshal, anyUnmarshal := false, false
	anyString, anyValidation, anyBytes, anyRequired := false, false, false, false
	walkCustomV := func(rules []ValidationRule) {
		for _, r := range rules {
			if r.Custom {
				add(r.PkgImport)
			}
		}
	}
	walkCustomM := func(rules []ModRule) {
		for _, r := range rules {
			if r.Custom {
				add(r.PkgImport)
			}
		}
	}
	for _, s := range structs {
		if s.Marshal {
			anyMarshal = true
		}
		if s.Unmarshal {
			anyUnmarshal = true
		}
		if s.MultiErr {
			anyValidation = true
		}
		// Struct alias delegating to a foreign-package underlying
		// (e.g. `type Local uuid.UUID`) needs that package's import.
		if s.IsAlias && s.AliasKind == KindStruct {
			add(s.AliasUnderlyingImport)
		}
		// streamUnknownKey clones the KeyView alias in every mode.
		if !s.IsAlias {
			add("strings")
		}
		// A container alias keeps its shape in AliasField, not Fields.
		if s.IsAlias {
			switch s.AliasKind {
			case KindBytes, KindSlice, KindArray, KindMap:
				collectFieldImports(s.AliasField, add, &anyString, &anyValidation, &anyBytes, &anyRequired)
				for _, ti := range s.AliasField.TypeImports {
					foreign[ti.Name] = ti.Path
				}
			}
		}
		for _, f := range s.Fields {
			collectFieldImports(f, add, &anyString, &anyValidation, &anyBytes, &anyRequired)
			for _, ti := range f.TypeImports {
				foreign[ti.Name] = ti.Path
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
	// anyMarshal/anyUnmarshal/anyValidation/anyRequired/anyString used to pick
	// runtime sub-packages; the single ggen import is unconditional now.
	_, _, _, _, _ = anyMarshal, anyUnmarshal, anyValidation, anyRequired, anyString
	// Stdlib-helper imports are emission-driven: scanning the rendered bodies
	// for the import-qualified token is exact and avoids a per-kind walk over
	// arbitrarily-nested container types.
	scanBodiesForStdImports(bodies, add)
	scanBodiesForForeignImports(bodies, foreign, add)
	out := make([]string, 0, len(need))
	third := make([]string, 0, len(need))
	for p := range need {
		if isThirdParty(p) {
			third = append(third, p)
		} else {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	slices.Sort(third)
	return out, third
}

// scanBodiesForStdImports inspects the rendered per-struct bodies for
// stdlib-qualified token prefixes and adds the matching import via add.
// Each token is dropped from the search set after the first match.
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
		// Element-type spellings (prealloc consts, elem temps) name these
		// even when no method body does; test-file structs parse AST-only,
		// so the TypeImports channel can't cover them.
		{[]byte("jsontext."), "encoding/json/jsontext"},
		{[]byte("json."), "encoding/json"},
		{[]byte("big."), "math/big"},
	}
	for _, body := range bodies {
		for i := range table {
			if table[i].path == "" {
				continue
			}
			if bytes.Contains(body, table[i].token) {
				add(table[i].path)
				table[i].path = ""
			}
		}
	}
}

// scanBodiesForForeignImports adds the foreign packages whose qualifier a
// rendered body actually names. A cross-package type reaches the generated
// file in two very different ways: spelled out in a declaration
// (`var v leaf.Leaf`, `make(map[string]leaf.Leaf)`, `append(x, leaf.Leaf{})`
// — every container, pointer and map-value position), or never named at all
// (a plain value field only ever writes `result.X` and calls methods on it).
// Importing unconditionally would break the second case with "imported and not
// used", so the candidate set is filtered the same way the stdlib helpers are.
func scanBodiesForForeignImports(bodies [][]byte, cands map[string]string, add func(string)) {
	for name, path := range cands {
		tok := []byte(name + ".")
		for _, body := range bodies {
			if bytes.Contains(body, tok) {
				add(path)
				break
			}
		}
	}
}

// collectFieldImports adds per-field imports. Flags get flipped for
// struct-wide checks (validation block, string-typed work) that the
// caller resolves into ggen subpackage imports afterwards.
func collectFieldImports(f FieldInfo, add func(string), anyString, anyValidation, anyBytes, anyRequired *bool) {
	if len(f.Validation) > 0 || len(f.ElemValidation) > 0 || len(f.KeyValidation) > 0 {
		*anyValidation = true
	}
	for _, inner := range f.InnerValidation {
		if len(inner) > 0 {
			*anyValidation = true
		}
	}
	if f.IsRequired() {
		*anyRequired = true
	}
	walkValidation := func(rules []ValidationRule) {
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
	walkMods := func(rules []ModRule) {
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
	// crossPkgStruct flags non-generated KindStruct fields: those go
	// through the json.Unmarshal / json.Marshal fallback when no
	// ggen/Text/JSON method is detected.
	crossPkgStruct := func(typ string, iface FieldInterfaces) {
		if isGenerated(typ) {
			return
		}
		// Named primitive decoded/encoded inline — never reaches encoding/json.
		if _, _, ok := inlineNamedPrim(FieldInfo{
			GoType: typ, Kind: KindStruct, HTMLEscape: f.HTMLEscape, Copy: f.Copy,
			AllowInvalidUTF8: f.AllowInvalidUTF8, NoValidate: f.NoValidate,
		}); ok {
			return
		}
		// Resolved + a known method → uses that method, no json.
		if iface.Resolved &&
			(iface.ByteDecoder || iface.JSONUnmarshaler || iface.TextUnmarshaler ||
				iface.AppendJSON || iface.JSONMarshaler || iface.TextAppender ||
				iface.TextMarshaler) {
			return
		}
		add("encoding/json")
	}
	switch f.Kind {
	case KindString:
		*anyString = true
	case KindTime, KindDuration:
		add("time")
	case KindNetIP:
		add("net")
		*anyString = true
	case KindNetipAddr, KindNetipPrefix:
		add("net/netip")
		*anyString = true
	case KindURL:
		add("net/url")
		*anyString = true
	case KindBytes:
		*anyBytes = true
		switch f.Format {
		case "", "base64", "base64url":
			add("encoding/base64")
		case "base32", "base32hex":
			add("encoding/base32")
		case "base16", "hex":
			add("encoding/hex")
		}
		if f.Format != "array" {
			*anyString = true
		}
	case KindSQLNull:
		add("database/sql")
		if f.SQLNullInner != nil {
			// Foreign-package imports for the type literals, plus the inner
			// type's own imports via recursion.
			for _, imp := range f.SQLNullImports {
				add(imp)
			}
			collectFieldImports(*f.SQLNullInner, add, anyString, anyValidation, anyBytes, anyRequired)
		} else if spec, ok := SQLNullSpec(f.GoType); ok {
			switch spec.Inner {
			case KindString:
				*anyString = true
			case KindTime:
				add("time")
			}
		}
	case KindBigInt, KindBigFloat, KindBigRat:
		*anyString = true
	case KindStruct:
		// For *T fields f.GoType is "*T" but isGenerated matches bare names —
		// use PointeeType so the cross-file struct-by-name check resolves.
		typ := f.GoType
		if f.Pointer {
			typ = f.PointeeType
		}
		if f.Pointer && len(typ) > 0 && typ[0] == '*' {
			// Multi-level pointer (`**T`, …): only the LEAF kind drives imports,
			// so recurse on a synthetic leaf field.
			leaf := f
			leaf.Pointer = false
			leaf.PointeeType = ""
			_, leaf.GoType = pointerDepth(f.GoType)
			leaf.Kind = resolveKind(leaf.GoType)
			collectFieldImports(leaf, add, anyString, anyValidation, anyBytes, anyRequired)
		} else {
			crossPkgStruct(typ, f.Iface)
		}
	case KindSlice, KindArray, KindMap:
		// Pointer elements / map values: only the LEAF kind drives imports
		// (as above). Non-pointer cross-pkg struct elems keep the json fallback.
		if et, eDepth := elemPtrType(f); eDepth > 0 {
			leaf := elemPtrField(f, f.JSONName)
			leaf.Pointer = false
			leaf.PointeeType = ""
			_, leaf.GoType = pointerDepth(et)
			collectFieldImports(leaf, add, anyString, anyValidation, anyBytes, anyRequired)
		} else if f.ElemKind == KindStruct {
			crossPkgStruct(f.ElemType, f.ElemIface)
		} else {
			switch f.ElemKind {
			case KindSlice, KindArray:
				// Nested container: the deep element's kind drives imports.
				collectFieldImports(peelSliceField(f), add, anyString, anyValidation, anyBytes, anyRequired)
			case KindTime, KindDuration, KindBytes, KindRawJSON, KindNetIP, KindNetipAddr,
				KindNetipPrefix, KindURL, KindBigInt, KindBigFloat, KindBigRat, KindSQLNull, KindAny, KindMap:
				// Dedicated-kind element delegates to the field emitters —
				// same imports as a field of that kind.
				collectFieldImports(sliceElemField(f), add, anyString, anyValidation, anyBytes, anyRequired)
			}
		}
	case KindAny:
	case KindRawJSON:
	}
	if f.String {
		// json:",string": strconv on decode/encode, inline string scan.
		*anyString = true
	}
}

func isThirdParty(p string) bool {
	seg, _, _ := strings.Cut(p, "/")
	return strings.ContainsRune(seg, '.')
}

// preregisterOneOfs walks all rules to populate the OneOf frozen-slice
// registry before render, so render-time lookups are pure map reads
// (required for safe parallel rendering). Dedup makes it order-independent.
func preregisterOneOfs(structs []StructInfo) {
	walk := func(rules []ValidationRule) {
		for _, v := range rules {
			if v.Name == "oneof" {
				registerOneOf(splitPipeParts(v.Value))
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

func isNumeric(k TypeKind) bool {
	switch k {
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64,
		KindUint, KindUint8, KindUint16, KindUint32, KindUint64,
		KindFloat32, KindFloat64:
		return true
	}
	return false
}

// scalarCountable reports whether a flat container of element kind k admits
// exact-cap sizing by counting delimiters in the bytes-path buffer. True only
// for numeric / bool elements (their textual form carries no `,`/`]`/`:`/`}`).
func scalarCountable(k TypeKind) bool { return isNumeric(k) || k == KindBool }

// renderOneMod emits one mod (transform) against ref. posVar selects the
// return shape for a fallible mod ("" = stream 2-tuple, else bytes 3-tuple). A
// custom mod is pure (`func(T)T`), error-form (`func(T)(T,error)`, parse error),
// or bool-form (`func(T)(T,bool)`, false → ggen.ModError parse error).
func renderOneMod(b *bytes.Buffer, m ModRule, field, ref, goType string, kind TypeKind, posVar string) {
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
					// ModError is a parse error — wrap so it carries the field
					// path like every other decode failure (mod_error.go doc).
					pos := posVar
					if pos == "" {
						pos = "s.Offset()"
					}
					modErr := fmt.Sprintf("ggen.NewParseErr(%s, %s, &ggen.ModError{%sName: %q, Msg: %q, Value: %s})",
						field, pos, posLit(posVar), strings.TrimPrefix(m.Name, "@"), m.Msg, ref)
					fmt.Fprintf(b, "if v, ok := %s(%s); !ok {\n\t%s\n} else {\n\t%s = v\n}\n",
						call, ref, ret(modErr), ref)
				} else {
					// The mod's own error is foreign: wrap it so it carries
					// the field path and offset, like every other decode
					// failure (errors.As still reaches the mod's error).
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
			parts := splitPipeParts(m.Value)
			if len(parts) == 2 {
				fmt.Fprintf(b, "%s = %s\n", ref,
					wrap(fmt.Sprintf("strings.ReplaceAll(%s, %s, %s)", asPrim(ref), strconv.Quote(parts[0]), strconv.Quote(parts[1]))))
			}
		case "clamp":
			// clamp=lo|hi — bound the value into [lo, hi]. Constants cast to
			// the field's type so `<`/`>` are comparable on aliased numerics.
			cparts := splitPipeParts(m.Value)
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

// effectiveKind resolves a named type whose underlying type is a primitive to
// that primitive's kind. Both `//ggen:generate type Priority string` (which
// gets a full method surface, so its FIELDS report KindStruct) and a plain
// `type Priority string` the user never annotated land in namedKinds. Every
// rule emitter must go through this: without it a named string reaches
// `if kind == KindString` as KindStruct, and the rule either emits a raw token
// (`case low:`) or, for eq/neq, nothing at all.
// seedNamedKinds fills namedKinds from both sources: the pass's own primitive
// aliases (`//ggen:generate type Count int`) and the named primitives the
// parse layer resolved from go/types on each field's type (which also covers
// types the user never annotated, and foreign ones like `leaf.Name`).
func seedNamedKinds(structs []StructInfo) {
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
// decoded/encoded INLINE — scan the underlying primitive, convert at the assign
// — instead of calling the type's own generated methods (or, when it has none,
// falling through to encoding/json).
//
// `type B S; type S string` resolves straight to `string`: go/types' Underlying
// walks the whole chain, so alias depth costs nothing here.
//
// The gate is behavioural, not structural. An ANNOTATED alias may carry flags
// of its own — `//ggen:generate htmlescape type HtmlString string` is the
// documented way to escape one type and not the rest, and `copy` /
// `allowinvalidutf8` / `novalidate` likewise change what its body emits.
// Inlining with the PARENT's flags would silently swap that behaviour, so those
// keep the call. A flag set globally on the CLI lands on both sides equally and
// so never blocks inlining.
func inlineNamedPrim(f FieldInfo) (prim string, kind TypeKind, ok bool) {
	// KindStruct is the generator's "named type I have no special handling
	// for". Anything else (KindDuration, KindTime, KindBytes, …) owns its wire
	// shape and keeps its own emitter, named primitive underneath or not.
	if f.Kind != KindStruct {
		return "", 0, false
	}
	k, isNamed := namedKinds[f.GoType]
	if !isNamed {
		return "", 0, false
	}
	// A type generated in ANOTHER pass keeps its own methods. Its flags are not
	// visible here (aliasFlags only covers this pass), so inlining would apply
	// the PARENT's — silently dropping, say, an `htmlescape` the other package
	// generated it with. Its DecodeFrom/AppendJSON already encode its choices.
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

// namedPrimInner reshapes f into the underlying primitive for the inline scan.
// The value steps stay on the OUTER field so they run against the named type
// (their `oneof`/`eq`/… resolve through effectiveKind); the inner render only
// produces the raw value.
func namedPrimInner(f FieldInfo, prim string, kind TypeKind) FieldInfo {
	inner := f
	inner.GoType = prim
	inner.Kind = kind
	inner.Validation = nil
	inner.Mods = nil
	inner.Pipe = nil
	inner.Levels = nil
	inner.NamedPrims = nil
	inner.Iface = FieldInterfaces{}
	inner.AtDispatch = false
	return inner
}

func effectiveKind(goType string, kind TypeKind) TypeKind {
	if k, ok := namedKinds[goType]; ok {
		return k
	}
	return kind
}

// primCast converts ref to its underlying primitive when goType is a named
// type — needed wherever the emitted code hands the value to a string-typed
// API (`utf8.RuneCountInString`, `strings.HasPrefix`, `ggen.IsURL`) or
// stores it in a string-typed error field. Comparisons against untyped
// constants (len/gte/eq/oneof) need no cast, so callers only wrap where the
// destination type is concrete.
func primCast(goType string, kind TypeKind, ref string) string {
	prim := kindPrimitiveName(kind)
	if goType == "" || prim == "" || goType == prim {
		return ref
	}
	return prim + "(" + ref + ")"
}

// kindPrimitiveName returns the Go literal name for a primitive TypeKind,
// or "" for kinds that aren't a single primitive token.
func kindPrimitiveName(k TypeKind) string {
	switch k {
	case KindString:
		return "string"
	case KindBool:
		return "bool"
	case KindInt:
		return "int"
	case KindInt8:
		return "int8"
	case KindInt16:
		return "int16"
	case KindInt32:
		return "int32"
	case KindInt64:
		return "int64"
	case KindUint:
		return "uint"
	case KindUint8:
		return "uint8"
	case KindUint16:
		return "uint16"
	case KindUint32:
		return "uint32"
	case KindUint64:
		return "uint64"
	case KindFloat32:
		return "float32"
	case KindFloat64:
		return "float64"
	}
	return ""
}

// posLit builds the Pos field for a validation error literal — the byte offset
// of the failure relative to the full payload. posVar is the bytes-path cursor;
// "" selects the stream path (s.Offset(), accounts for compaction).
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

// arrayLenErr builds a typed *ggen.LenError literal for the strict
// fixed-array element-count check.
// byteArrayLen returns N for a `[N]byte` field folded onto the KindBytes
// (base64) path, else 0. jsonv2 base64s byte ARRAYS and rejects the v1
// number-array form; the decoded length must be exactly N.
func byteArrayLen(f FieldInfo) int {
	if f.Kind == KindBytes && f.ArrayLen > 0 {
		return f.ArrayLen
	}
	return 0
}

func arrayLenErr(field string, want int, gotExpr, posVar string) string {
	return withPos(fmt.Sprintf("&ggen.LenError{Path: []string{%q}, Want: %d, Got: %s}", field, want, gotExpr), posVar)
}

// requiredErr builds a typed *ggen.RequiredError literal.
func requiredErr(field string) string {
	return fmt.Sprintf("&ggen.RequiredError{Path: []string{%q}}", field)
}

// oneofRegistry collects unique allowed-string sets emitted as package-level
// frozen slices, so OneOfError construction never allocates the slice.
var oneofRegistry struct {
	names map[string]string // joined "a|b|c" → var name
	decls []string
	// prefix keeps the frozen slices unique across output FILES of one Go
	// package. Single-file mode (`ggen $GOFILE`, one generated file per source)
	// restarts the counter per file, so two sources in the same package that
	// both use `oneof` used to declare ggenOneof0 twice — a redeclaration.
	// Derived from the struct set being emitted, so it is stable across runs.
	prefix string
}

func resetOneofRegistry(scope string) {
	oneofRegistry.names = map[string]string{}
	oneofRegistry.decls = nil
	oneofRegistry.prefix = emitScope(scope)
}

// emitScope derives the registry-name prefix from the OUTPUT FILENAME —
// distinct per file within a package by construction, and stable across
// struct additions/removals (the old struct-name-set hash rehashed every
// ggenCap/ggenOneof on any set change). The generated-file suffixes are
// noise, so `extra_ggen_test.go` scopes as just `extra`.
func emitScope(scope string) string {
	base := strings.TrimSuffix(filepath.Base(scope), ".go")
	base = strings.TrimSuffix(base, "_test")
	base = strings.TrimSuffix(base, "_ggen")
	return sanitizeIdent(base)
}

// sanitizeIdent maps an arbitrary type/file string onto identifier chars:
// alnum kept, `*` spelled Ptr, every other rune becomes exactly one `_` —
// no collapsing, so distinct keys stay distinct (`[]int` → `_entryConsumedt` vs
// `int`; a collision would redeclare the const, loud at compile time).
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
// errors; kind selects type-appropriate comparisons; multiErr appends instead
// of returning. posVar selects the return shape ("" 2-tuple, else 3-tuple).
func renderValidationOn(b *bytes.Buffer, rules []ValidationRule, ref, jsonName, goType string, kind TypeKind, multiErr bool, posVar string) {
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
// value is pure ASCII — so one byte per rune, i.e. utf8.RuneCountInString ==
// len. Lets a rune rule that FOLLOWS one of these in the same run drop the walk.
func asciiImplyingRule(name string) bool {
	switch name {
	case "alphanum", "numeric", "hexadecimal":
		return true
	}
	return false
}

// emitValRun emits a contiguous run of validators against an unchanged ref
// (callers split runs at mods, which mutate ref). Rune-count rules
// (runes/minrunes/maxrunes) emit through emitRuneRule, which avoids the O(len)
// utf8.RuneCountInString walk via byte-length gating. If an ASCII-implying rule
// already passed earlier in this run AND we're not in multierr mode, the count
// is exactly len and the walk is dropped entirely.
func emitValRun(b *bytes.Buffer, run []ValidationRule, ref, jsonName, goType string, kind TypeKind, multiErr bool, onErr func(string) string) {
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

// emitRuneRule emits a single rune-count rule (runes/minrunes/maxrunes).
// useLen drops the walk entirely (count is len). Otherwise byte-length gates
// resolve the fail-free and pass-free cases in O(1); only the ambiguous band
// walks. The failure literal's Got reports the real rune count.
func emitRuneRule(b *bytes.Buffer, v ValidationRule, ref, jsonName, goType string, kind TypeKind, onErr func(string) string, useLen bool) {
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
	// utf8 takes a real string; a named string type needs the conversion.
	walk := fmt.Sprintf("utf8.RuneCountInString(%s)", primCast(goType, kind, ref))

	// Every byte is a rune, so the count is len(ref) — no walk.
	if useLen {
		l := fmt.Sprintf("len(%s)", ref)
		op := map[string]string{"runes": "!=", "minrunes": "<", "maxrunes": ">"}[v.Name]
		fmt.Fprintf(b, "if %s %s %s {\n\t%s\n}\n", l, op, v.Value, onErr(errLit(l)))
		return
	}

	// Byte-length gate. On the off chance the value doesn't parse as an
	// integer, fall back to an unconditional walk.
	n, err := strconv.Atoi(v.Value)
	if err != nil {
		op := map[string]string{"runes": "!=", "minrunes": "<", "maxrunes": ">"}[v.Name]
		fmt.Fprintf(b, "if %s %s %s {\n\t%s\n}\n", walk, op, v.Value, onErr(errLit(walk)))
		return
	}
	switch v.Name {
	case "minrunes":
		// R >= n. Fail-free below len n, pass-free at/above 4n-3; walk the band.
		fmt.Fprintf(b, "if len(%s) < %d {\n\t%s\n}", ref, n, onErr(errLit(walk)))
		if 4*n-3 > n {
			fmt.Fprintf(b, " else if len(%s) < %d {\n\tif rc := %s; rc < %d {\n\t\t%s\n\t}\n}", ref, 4*n-3, walk, n, onErr(errLit("rc")))
		}
		b.WriteString("\n")
	case "maxrunes":
		// R <= n. Pass-free at/below len n, fail-free above 4n; walk the band.
		fmt.Fprintf(b, "if len(%s) > %d {\n\t%s\n}", ref, 4*n, onErr(errLit(walk)))
		if n >= 1 {
			fmt.Fprintf(b, " else if len(%s) > %d {\n\tif rc := %s; rc > %d {\n\t\t%s\n\t}\n}", ref, n, walk, n, onErr(errLit("rc")))
		}
		b.WriteString("\n")
	case "runes":
		// R == n. Fail-free outside [n, 4n]; walk the band.
		fmt.Fprintf(b, "if len(%s) < %d || len(%s) > %d {\n\t%s\n} else if rc := %s; rc != %d {\n\t%s\n}\n",
			ref, n, ref, 4*n, onErr(errLit(walk)), walk, n, onErr(errLit("rc")))
	}
}

// renderOneVal emits one validation check against ref. onErr wraps a typed
// error literal into the right failure action. Builtin validators dispatch by
// name; a custom (`@Func`) validator calls the user func — error form wraps
// CustomError, bool form (BoolForm) emits PredicateError on a false return.
// Rune-count rules (runes/minrunes/maxrunes) are NOT handled here — emitValRun
// routes them to emitRuneRule (byte-length gating).
func renderOneVal(b *bytes.Buffer, v ValidationRule, ref, jsonName, goType string, kind TypeKind, onErr func(string) string) {
	kind = effectiveKind(goType, kind)
	// str is ref converted to a plain string — for the checks and error fields
	// that are string-typed rather than untyped-constant comparisons.
	str := primCast(goType, kind, ref)
	switch v.Name {
	case "required", "optional":
		// required handled separately; optional is a no-op marker

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
			onErr(fmt.Sprintf("&ggen.GTError{Path: []string{%q}, Limit: %s, Value: %s}", jsonName, v.Value, ref)))
	case "gte":
		fmt.Fprintf(b, "if %s < %s {\n\t%s\n}\n", ref, v.Value,
			onErr(fmt.Sprintf("&ggen.GTEError{Path: []string{%q}, Limit: %s, Value: %s}", jsonName, v.Value, ref)))
	case "lt":
		fmt.Fprintf(b, "if %s >= %s {\n\t%s\n}\n", ref, v.Value,
			onErr(fmt.Sprintf("&ggen.LTError{Path: []string{%q}, Limit: %s, Value: %s}", jsonName, v.Value, ref)))
	case "lte":
		fmt.Fprintf(b, "if %s > %s {\n\t%s\n}\n", ref, v.Value,
			onErr(fmt.Sprintf("&ggen.LTEError{Path: []string{%q}, Limit: %s, Value: %s}", jsonName, v.Value, ref)))

	case "eq":
		if kind == KindString {
			fmt.Fprintf(b, "if %s != %q {\n\t%s\n}\n",
				ref, v.Value,
				onErr(fmt.Sprintf("&ggen.EqError{Path: []string{%q}, Want: %q, Value: %s}", jsonName, v.Value, ref)))
		} else if isNumeric(kind) {
			fmt.Fprintf(b, "if %s != %s {\n\t%s\n}\n",
				ref, v.Value,
				onErr(fmt.Sprintf("&ggen.EqError{Path: []string{%q}, Want: %s, Value: %s}", jsonName, v.Value, ref)))
		}
	case "neq":
		if kind == KindString {
			fmt.Fprintf(b, "if %s == %q {\n\t%s\n}\n",
				ref, v.Value,
				onErr(fmt.Sprintf("&ggen.NeqError{Path: []string{%q}, Want: %q, Value: %s}", jsonName, v.Value, ref)))
		} else if isNumeric(kind) {
			fmt.Fprintf(b, "if %s == %s {\n\t%s\n}\n",
				ref, v.Value,
				onErr(fmt.Sprintf("&ggen.NeqError{Path: []string{%q}, Want: %s, Value: %s}", jsonName, v.Value, ref)))
		}

	case "oneof":
		cases := renderOneofCases(kind, v.Value)
		varName := registerOneOf(splitPipeParts(v.Value))
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
			onErr(fmt.Sprintf("&ggen.MultipleError{Path: []string{%q}, Of: %s, Value: %s}", jsonName, v.Value, ref)))

	default:
		// `@FuncName` — user-defined validator resolved at parse time.
		if v.Custom {
			call := v.FuncName
			if v.PkgName != "" {
				call = v.PkgName + "." + v.FuncName
			}
			if v.BoolForm {
				fmt.Fprintf(b, "if !%s(%s) {\n\t%s\n}\n",
					call, ref,
					onErr(fmt.Sprintf("&ggen.PredicateError{Path: []string{%q}, Name: %q, Msg: %q, Value: %s}", jsonName, strings.TrimPrefix(v.Name, "@"), v.Msg, ref)))
			} else {
				fmt.Fprintf(b, "if err := %s(%s); err != nil {\n\t%s\n}\n",
					call, ref,
					onErr(fmt.Sprintf("&ggen.CustomError{Path: []string{%q}, Name: %q, Value: %s, Cause: err}", jsonName, strings.TrimPrefix(v.Name, "@"), ref)))
			}
		}
		// Unknown non-custom names are silently ignored.
	}
}

func renderOneofCases(kind TypeKind, raw string) string {
	parts := splitPipeParts(raw)
	out := make([]string, len(parts))
	for i, p := range parts {
		if kind == KindString {
			out[i] = strconv.Quote(p)
		} else {
			out[i] = p
		}
	}
	return strings.Join(out, ", ")
}

// fieldIsConditional reports whether the field's marshal emission depends on
// a runtime check (omitempty/omitzero).
func fieldIsConditional(f FieldInfo) bool {
	if f.Embed {
		return true
	}
	if !f.OmitEmpty && !f.OmitZero {
		return false
	}
	return fieldSkipExpr(f, "s."+f.GoName) != ""
}

// fieldSkipExpr returns a Go boolean expression that is true when the field
// should be EMITTED (omitempty: skip if JSON-empty; omitzero: skip if Go zero).
// Empty string means "always emit".
func fieldSkipExpr(f FieldInfo, ref string) string {
	var emitConds []string
	// A named primitive reports KindStruct at its use sites; unresolved, the
	// omitzero KindStruct arm emitted `ref != (Score{})` (does not compile)
	// and omitempty had no arm at all (option silently dropped).
	kind := effectiveKind(f.GoType, f.Kind)
	if f.OmitEmpty {
		if f.Pointer {
			emitConds = append(emitConds, fmt.Sprintf("%s != nil", ref))
		} else {
			switch kind {
			case KindString:
				emitConds = append(emitConds, fmt.Sprintf("%s != \"\"", ref))
			case KindSlice, KindBytes, KindMap:
				emitConds = append(emitConds, fmt.Sprintf("len(%s) > 0", ref))
			case KindRawJSON:
				// Underlying []byte; nil/empty → skip (no peek into content).
				emitConds = append(emitConds, fmt.Sprintf("len(%s) > 0", ref))
			case KindAny:
				emitConds = append(emitConds, fmt.Sprintf("%s != nil", ref))
			case KindSQLNull:
				// !Valid marshals as `null` (JSON-empty) → emit only when Valid.
				emitConds = append(emitConds, fmt.Sprintf("%s.Valid", ref))
				// big.Int/Float/Rat carry NO omitempty arm on purpose: a zero
				// one encodes as `0` / `"0"`, which is not a JSON-empty value
				// (v1 never omits a struct; jsonv2 omits only null/""/{}/[]).
				// Omitting it dropped a field whose wire form is meaningful.
			}
		}
	}
	if f.OmitZero {
		if f.Pointer {
			emitConds = append(emitConds, fmt.Sprintf("%s != nil", ref))
		} else {
			switch kind {
			case KindString:
				emitConds = append(emitConds, fmt.Sprintf("%s != \"\"", ref))
			case KindBool:
				emitConds = append(emitConds, ref)
			case KindInt, KindInt8, KindInt16, KindInt32, KindInt64,
				KindUint, KindUint8, KindUint16, KindUint32, KindUint64,
				KindFloat32, KindFloat64:
				emitConds = append(emitConds, fmt.Sprintf("%s != 0", ref))
			case KindSlice, KindBytes, KindMap, KindRawJSON:
				// Go-zero is nil; `make([]T, 0)` is non-nil and must be emitted.
				emitConds = append(emitConds, fmt.Sprintf("%s != nil", ref))
			case KindStruct:
				emitConds = append(emitConds, zeroCompare(f, ref))
			case KindTime:
				emitConds = append(emitConds, fmt.Sprintf("!%s.IsZero()", ref))
			case KindDuration:
				emitConds = append(emitConds, fmt.Sprintf("%s != 0", ref))
			case KindNetIP:
				emitConds = append(emitConds, fmt.Sprintf("%s != nil", ref))
			case KindNetipAddr, KindNetipPrefix:
				emitConds = append(emitConds, fmt.Sprintf("%s.IsValid()", ref))
			case KindAny:
				emitConds = append(emitConds, fmt.Sprintf("%s != nil", ref))
			case KindSQLNull:
				emitConds = append(emitConds, zeroCompare(f, ref))
			case KindURL:
				emitConds = append(emitConds, fmt.Sprintf("%s != (%s{})", ref, f.GoType))
			case KindBigInt, KindBigFloat, KindBigRat:
				// big.X isn't comparable (unexported slices); Sign()==0 for zero.
				emitConds = append(emitConds, fmt.Sprintf("%s.Sign() != 0", ref))
			case KindArray:
				emitConds = append(emitConds, zeroCompare(f, ref))
			}
		}
	}
	if len(emitConds) == 0 {
		return ""
	}
	return strings.Join(slices.Compact(emitConds), " && ")
}

// zeroCompare emits the omitzero is-nonzero test: `ref != (T{})` when the
// type is comparable, else a reflect deep-zero probe — `!= (T{})` on a struct
// carrying a slice/map field does not compile.
func zeroCompare(f FieldInfo, ref string) string {
	if f.NotComparable {
		return fmt.Sprintf("!reflect.ValueOf(%s).IsZero()", ref)
	}
	return fmt.Sprintf("%s != (%s{})", ref, f.GoType)
}

func renderAppendJSON(b *bytes.Buffer, s StructInfo) {
	fmt.Fprintf(b, "func (s %s) AppendJSON(dst []byte) ([]byte, error) {\n", s.Name)
	if s.IsAlias {
		renderAliasAppendJSON(b, s)
		b.WriteString("}\n\n")
		return
	}
	// coalesceConstAppends folds adjacent constant-byte appends post-emit.
	var body bytes.Buffer
	renderAppendJSONBody(&body, s)
	b.WriteString(coalesceConstAppends(body.String()))
	b.WriteString("}\n\n")
}

// coalesceConstAppends merges adjacent constant-byte append lines into a
// single append. Recognizes both `dst = append(dst, ...)` and the
// terminating `return append(dst, ...), nil` form. Single-byte payloads
// emit as a char literal, multi-byte as a string spread.
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

// formatAppendArgs renders the bytes as the `args` of an append call.
// Single byte → char literal; multi-byte → string spread.
func formatAppendArgs(p []byte) string {
	if len(p) == 1 {
		return strconv.QuoteRune(rune(p[0]))
	}
	return fmt.Sprintf("%q...", string(p))
}

// parseConstAppend recognizes `dst = append(dst, X)` where X is one of
// `"…"...`, `'a'`, or `'a', 'b', …` (all compile-time constant bytes).
// Returns the literal bytes; anything non-constant returns false.
func parseConstAppend(line string) ([]byte, bool) {
	const prefix = "dst = append(dst,"
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, ")") {
		return nil, false
	}
	return parseConstAppendArgs(line[len(prefix) : len(line)-1])
}

// parseConstReturnAppend is the trailing-return variant: matches
// `return append(dst, X), nil` and extracts X's bytes if constant.
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
	// `'a', 'b', '\n'` — can't split on `,` (the literal may BE a comma);
	// walk char literals manually, respecting `\X` escapes.
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

func renderAppendJSONBody(b *bytes.Buffer, s StructInfo) {
	if len(s.Fields) == 0 {
		b.WriteString("return append(dst, '{', '}'), nil")
		return
	}

	// err declared up front so emitters share one slot; `_ = err` covers the
	// pure-primitive case where nothing fallible uses it.
	b.WriteString("var err error\n_ = err\n")

	// No conditional fields → hard-code `{` into the first field's prefix
	// and prepend commas before the rest.
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

	// Conditional path: track emissions via len(dst) vs start.
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
		// Pointer field guarded by its nil-check: peel one level so
		// renderAppendValue doesn't emit a dead `if ref == nil` rung inside it.
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

// inlineValueEmit returns the embedded-fallback marshal code for one map
// entry's value (loop var `v`). Specializes a few elem kinds to skip the
// `any` boxing; everything else goes through AppendAny.
func inlineValueEmit(f FieldInfo) string {
	switch f.ElemKind {
	case KindString:
		return fmt.Sprintf("dst = append(dst, '\"')\ndst = %s(dst, v)\n", appendStrFn(f.HTMLEscape))
	case KindStruct:
		if isGenerated(f.ElemType) {
			return "if dst, err = v.AppendJSON(dst); err != nil { return dst, err }\n"
		}
	case KindRawJSON:
		// jsontext.Value / json.RawMessage: passthrough; empty → `null`
		// (appending nothing would corrupt the object — field-level raw
		// emit and AppendAny both null empties).
		return "if len(v) == 0 { dst = append(dst, \"null\"...) } else { dst = append(dst, v...) }\n"
	}
	if f.ElemType == "jsontext.Value" {
		return "if len(v) == 0 { dst = append(dst, \"null\"...) } else { dst = append(dst, v...) }\n"
	}
	return fmt.Sprintf("if dst, err = %s(dst, v); err != nil { return dst, err }\n", appendAnyFn(f.HTMLEscape))
}

func renderAppendValue(b *bytes.Buffer, f FieldInfo, ref string) {
	// Named primitive: append the underlying directly (`AppendStringNoHTML(dst,
	// string(s.X))`) instead of calling the alias's AppendJSON. Frees the
	// `"key":"` quote fold (opt #16) for string-underlying aliases, and gets an
	// UNANNOTATED named type off the json.Marshal fallback. Gated so an alias
	// generated with its own htmlescape/copy flags keeps its own method.
	if prim, kind, ok := inlineNamedPrim(f); ok && !f.Pointer && !f.String {
		renderAppendValue(b, namedPrimInner(f, prim, kind), primCast(f.GoType, kind, ref))
		return
	}
	if f.Pointer {
		// nil at any level → `null` (flat else-if ladder, one rung per level);
		// only a full chain reaches the leaf.
		depth, leafType := pointerDepth(f.GoType)
		leaf := f
		leaf.Pointer = false
		leaf.PointeeType = ""
		leaf.GoType = leafType
		leaf.Kind = resolveKind(leafType)
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
		switch f.Kind {
		case KindBool:
			// jsonv2 dropped `,string` for bool — stays bare; fall through.
		case KindInt, KindInt8, KindInt16, KindInt32:
			fmt.Fprintf(b, "dst = append(dst, '\"')\ndst = strconv.AppendInt(dst, int64(%s), 10)\ndst = append(dst, '\"')\n", ref)
			return
		case KindInt64:
			fmt.Fprintf(b, "dst = append(dst, '\"')\ndst = strconv.AppendInt(dst, %s, 10)\ndst = append(dst, '\"')\n", ref)
			return
		case KindUint, KindUint8, KindUint16, KindUint32:
			fmt.Fprintf(b, "dst = append(dst, '\"')\ndst = strconv.AppendUint(dst, uint64(%s), 10)\ndst = append(dst, '\"')\n", ref)
			return
		case KindUint64:
			fmt.Fprintf(b, "dst = append(dst, '\"')\ndst = strconv.AppendUint(dst, %s, 10)\ndst = append(dst, '\"')\n", ref)
			return
		case KindFloat32:
			fmt.Fprintf(b, "dst = append(dst, '\"')\nif dst, err = ggen.AppendFloat(dst, float64(%s), 32); err != nil { return dst, err }\ndst = append(dst, '\"')\n", ref)
			return
		case KindFloat64:
			fmt.Fprintf(b, "dst = append(dst, '\"')\nif dst, err = ggen.AppendFloat(dst, %s, 64); err != nil { return dst, err }\ndst = append(dst, '\"')\n", ref)
			return
		}
		// unknown/invalid combo — fall through to default
	}
	switch f.Kind {
	case KindString:
		fmt.Fprintf(b, "dst = append(dst, '\"')\ndst =%s(dst, %s)\n", appendStrFn(f.HTMLEscape), ref)
	case KindBool:
		fmt.Fprintf(b, "dst = strconv.AppendBool(dst, %s)\n", ref)
	case KindInt, KindInt8, KindInt16, KindInt32:
		fmt.Fprintf(b, "dst = strconv.AppendInt(dst, int64(%s), 10)\n", ref)
	case KindInt64:
		fmt.Fprintf(b, "dst = strconv.AppendInt(dst, %s, 10)\n", ref)
	case KindUint, KindUint8, KindUint16, KindUint32:
		fmt.Fprintf(b, "dst = strconv.AppendUint(dst, uint64(%s), 10)\n", ref)
	case KindUint64:
		fmt.Fprintf(b, "dst = strconv.AppendUint(dst, %s, 10)\n", ref)
	case KindFloat32:
		fmt.Fprintf(b, "if dst, err = ggen.AppendFloat(dst, float64(%s), 32); err != nil { return dst, err }\n", ref)
	case KindFloat64:
		fmt.Fprintf(b, "if dst, err = ggen.AppendFloat(dst, %s, 64); err != nil { return dst, err }\n", ref)
	case KindStruct:
		if isGenerated(f.GoType) {
			fmt.Fprintf(b, "if dst, err = %s.AppendJSON(dst); err != nil { return dst, err }\n", ref)
		} else {
			b.WriteString(renderCrossPkgStructAppend(f, ref))
		}
	case KindSlice, KindArray:
		renderAppendSlice(b, f, ref)
	case KindMap:
		renderAppendMap(b, f, ref)
	case KindBytes:
		renderAppendBytes(b, f, ref)
	case KindTime:
		renderAppendTime(b, f, ref)
	case KindDuration:
		renderAppendDuration(b, f, ref)
	case KindNetIP, KindNetipPrefix:
		// Both implement encoding.TextAppender (Go 1.24+); their text can
		// never carry escape-needing bytes (no zone — PrefixFrom strips it).
		fmt.Fprintf(b, `dst = append(dst, '"')
if dst, err = %s.AppendText(dst); err != nil { return dst, err }
dst = append(dst, '"')
`, ref)
	case KindNetipAddr:
		// Zoned text may need escaping — ggen.AppendNetipAddr handles it.
		fmt.Fprintf(b, "dst = append(dst, '\"')\ndst = %s(dst, %s)\n", appendNetipAddrFn(f.HTMLEscape), ref)
	case KindRawJSON:
		// Emit raw bytes verbatim (or "null" if empty/nil).
		fmt.Fprintf(b, `if len(%s) == 0 {
	dst = append(dst, 'n', 'u', 'l', 'l')
} else {
	dst = append(dst, %s...)
}
`, ref, ref)
	case KindURL:
		fmt.Fprintf(b, "dst = append(dst, '\"')\ndst = %s(dst, %s)\n", appendURLFn(f.HTMLEscape), ref)
	case KindBigInt:
		fmt.Fprintf(b, "dst = (&%s).Append(dst, 10)\n", ref)
	case KindBigFloat:
		// big.Float as JSON string — jsonv2 wire format.
		fmt.Fprintf(b, "dst = append(dst, '\"')\ndst = (&%s).Append(dst, 'g', -1)\ndst = append(dst, '\"')\n", ref)
	case KindBigRat:
		// JSON string "num/denom" (or "n" when whole), via AppendText.
		fmt.Fprintf(b, "dst = append(dst, '\"')\nif dst, err = (&%s).AppendText(dst); err != nil { return dst, err }\ndst = append(dst, '\"')\n", ref)
	case KindSQLNull:
		if f.SQLNullInner != nil {
			inner := sqlNullInnerField(f)
			fmt.Fprintf(b, "if !%s.Valid {\n\tdst = append(dst, 'n', 'u', 'l', 'l')\n} else {\n\t", ref)
			renderAppendValue(b, inner, ref+".V")
			b.WriteString("}\n")
			return
		}
		spec, ok := SQLNullSpec(f.GoType)
		if !ok {
			return
		}
		innerField := FieldInfo{Kind: spec.Inner, GoType: spec.Type, Format: f.Format}
		fmt.Fprintf(b, "if !%s.Valid {\n\tdst = append(dst, 'n', 'u', 'l', 'l')\n} else {\n\t", ref)
		renderAppendValue(b, innerField, ref+"."+spec.Field)
		b.WriteString("}\n")
	case KindAny:
		fmt.Fprintf(b, "if dst, err = %s(dst, %s); err != nil { return dst, err }\n", appendAnyFn(f.HTMLEscape), ref)
	}
}

// renderAppendBytes emits marshal code for a []byte field based on format.
func renderAppendBytes(b *bytes.Buffer, f FieldInfo, ref string) {
	if byteArrayLen(f) > 0 {
		renderAppendBytesValue(b, f, ref)
		return
	}
	// nil []byte → null (stdlib v1 parity; decode accepts null → nil).
	fmt.Fprintf(b, "if %s == nil {\ndst = append(dst, \"null\"...)\n} else {\n", ref)
	renderAppendBytesValue(b, f, ref)
	b.WriteString("}\n")
}

func renderAppendBytesValue(b *bytes.Buffer, f FieldInfo, ref string) {
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
		// Unknown format: fall back to base64.
		fmt.Fprintf(b, "dst = append(dst, '\"')\ndst =base64.StdEncoding.AppendEncode(dst, %s)\ndst =append(dst, '\"')\n", ref)
	}
}

// timeFormatSize returns the JSONSize byte budget for a `time.Time` field,
// including surrounding quotes (none for numeric `unix*` variants). Unknown /
// custom layouts fall back to len(format)+6.
func timeFormatSize(format string) int {
	switch format {
	case "unix":
		// sign + 19-digit seconds + `.` + 9-digit nanos = 30, plus slack.
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
	// Custom layout: its literal characters land in the output verbatim and
	// are then JSON-escaped, so budget the worst-case expansion (a control
	// byte becomes \uXXXX, 6 bytes) for the ones that need it.
	n := len(format) + 6
	for i := range len(format) {
		if c := format[i]; c == '"' || c == '\\' || c < 0x20 {
			n += 5
		}
	}
	return n
}

// durationFormatSize returns the JSONSize byte budget for a `time.Duration`
// field. Numeric formats emit an int (no quotes); default `units` is a quoted
// "NhNmNs" string.
func durationFormatSize(format string) int {
	switch format {
	case "sec", "milli", "micro", "nano":
		return sizeInt
	}
	return 25 + 2 // "<NhNmNs>" worst case
}

// timeLayoutExpr returns the Go layout expression for a time.Time format
// (e.g. "RFC3339" → time.RFC3339, custom → quoted literal) and, for the unix
// family, the numeric encoding name.
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

func renderAppendTime(b *bytes.Buffer, f FieldInfo, ref string) {
	layout, numeric := timeLayoutExpr(f.Format)
	if numeric != "" {
		// `format:unix` emits a fractional decimal so sub-second nanos
		// round-trip (jsonv2 parity). AppendUnixSeconds works from
		// Unix()+Nanosecond(), never float64(UnixNano()) — that overflowed
		// outside the int64-nano range and lost sub-100ns precision. The
		// other unix* units are integer-granular.
		if numeric == "Unix" {
			fmt.Fprintf(b, "dst = ggen.AppendUnixSeconds(dst, %s)\n", ref)
			return
		}
		fmt.Fprintf(b, "dst = strconv.AppendInt(dst, %s.%s(), 10)\n", ref, numeric)
		return
	}
	// A CUSTOM layout's non-token characters are copied verbatim by
	// AppendFormat, so a layout carrying `"` or `\` (or a control byte)
	// produced invalid — or silently corrupted — JSON when appended raw
	// between quotes. Close through the same escape-on-dirty helper the
	// TextAppender sites use. Named time.X constants are fixed, ASCII-safe
	// text and keep the raw fast path.
	if isCustomTimeLayout(f.Format) {
		// Field-suffixed temp: two custom-layout fields share the struct
		// body's scope (same reason renderAppendMap suffixes its first-entry
		// flag).
		tmp := "tf" + sanitizeIdent(f.GoName)
		fmt.Fprintf(b, "dst = append(dst, '\"')\n%s := len(dst)\ndst = %s.AppendFormat(dst, %s)\ndst = %s(dst, %s)\n",
			tmp, ref, layout, closeStrFn(f.HTMLEscape), tmp)
		return
	}
	fmt.Fprintf(b, "dst = append(dst, '\"')\ndst = %s.AppendFormat(dst, %s)\ndst = append(dst, '\"')\n", ref, layout)
}

// isCustomTimeLayout reports whether format is a user-supplied layout string
// rather than one of the named time.X constants / numeric forms — i.e. the
// text lands in the output verbatim and may need JSON escaping.
func isCustomTimeLayout(format string) bool {
	layout, numeric := timeLayoutExpr(format)
	return numeric == "" && !strings.HasPrefix(layout, "time.")
}

func renderAppendDuration(b *bytes.Buffer, f FieldInfo, ref string) {
	switch f.Format {
	case "sec":
		// Seconds() is always finite — the error arm is dead but uniform.
		fmt.Fprintf(b, "if dst, err = ggen.AppendFloat(dst, %s.Seconds(), 64); err != nil { return dst, err }\n", ref)
	case "milli":
		fmt.Fprintf(b, "dst = strconv.AppendInt(dst, %s.Milliseconds(), 10)\n", ref)
	case "micro":
		fmt.Fprintf(b, "dst = strconv.AppendInt(dst, %s.Microseconds(), 10)\n", ref)
	case "nano":
		fmt.Fprintf(b, "dst = strconv.AppendInt(dst, %s.Nanoseconds(), 10)\n", ref)
	default:
		// "" (bare field) and "units"; unknown formats fall back to units too.
		fmt.Fprintf(b, "dst = append(dst, '\"')\ndst =%s(dst, %s.String())\n", appendStrFn(f.HTMLEscape), ref)
	}
}

// structHasAppendFormatTime reports whether any field emits via
// time.Time.AppendFormat (non-numeric time format) — renderSize reserves
// 64-byte headroom for the stdlib's internal slices.Grow.
func structHasAppendFormatTime(s StructInfo) bool {
	for _, f := range s.Fields {
		if f.Kind != KindTime {
			continue
		}
		_, numeric := timeLayoutExpr(f.Format)
		if numeric == "" {
			return true
		}
	}
	return false
}

// renderSize emits the body of a JSONSize method: sums the worst-case byte
// count to serialize the struct. Per-field contributions split into a
// compile-time constant (folded into `size := N`) and runtime code; runtime
// adds collect into a sibling builder during the constant pass and flush after.
func renderSize(b *bytes.Buffer, s StructInfo) {
	fmt.Fprintf(b, "func (s %s) JSONSize() int {\n", s.Name)
	if s.IsAlias {
		b.WriteString(renderAliasSize(s))
		b.WriteString("}\n\n")
		return
	}
	// Fixed overhead: braces + per-field key bytes + commas. omitempty/omitzero
	// fields move their key+value out of the constant into a runtime guard.
	fixed := 2 // { and }
	// time.AppendFormat calls slices.Grow(b, max(layout, 64)) per invocation;
	// reserve one shared 64-byte tail to keep the single-alloc Marshal contract.
	if structHasAppendFormatTime(s) {
		fixed += 64
	}
	named := 0
	var runtime bytes.Buffer
	for _, f := range s.Fields {
		if f.Embed {
			// Embedded fallback: name/colon/comma budgeted per-entry in sizeMapContrib.
			ref := "s." + f.GoName
			_, code := sizeContrib(f, ref)
			runtime.WriteString(code)
			continue
		}
		ref := "s." + f.GoName
		emit := fieldSkipExpr(f, ref)
		// Pointer field guarded by its nil-check: pass the deref'd inner so
		// sizeContrib doesn't emit a redundant `if ref == nil` inside the guard.
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
		// Omit-eligible field: worst-case includes a leading comma.
		fmt.Fprintf(&runtime, "if %s {\nsize += %d\n", emit, len(escapeJSONName(f.JSONName, f.HTMLEscape))+4+n)
		runtime.WriteString(code)
		runtime.WriteString("}\n")
	}
	fmt.Fprintf(b, "size := %d\n", fixed)
	_, _ = runtime.WriteTo(b)
	b.WriteString("return size\n}\n\n")
}

// escapeJSONName JSON-escapes a field name once at generate time for embedding
// into the constant `"name":` wire prefix (quote/backslash/control bytes;
// htmlEscape also escapes <>& — jsonv2 escapes names like values). Without it
// a tag-grammar name carrying `"` emitted invalid JSON with a nil error.
func escapeJSONName(name string, htmlEscape bool) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
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

// appendStrFn returns the encode-pkg helper for emitting a JSON string body +
// closing `"`. Default emits `<`/`>`/`&` literally (jsonv2 shape); htmlescape
// switches to the HTML-safe escaper (encoding/json v1). The CALLER must have
// already written the opening `"`.
func appendStrFn(htmlEscape bool) string {
	if htmlEscape {
		return "ggen.AppendString" + simdSuffix
	}
	return "ggen.AppendStringNoHTML" + simdSuffix
}

// closeStrFn returns the encode-pkg closer for raw text appended between
// quotes (TextAppender output): closes the string, re-escaping iff dirty.
func closeStrFn(htmlEscape bool) string {
	if htmlEscape {
		return "ggen.CloseJSONStringHTML"
	}
	return "ggen.CloseJSONString"
}

// appendURLFn / appendNetipAddrFn pick the escape-set variant of the raw-text
// closers (htmlescape parity with every other string emitter).
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

// emitNoCloseAfterComma emits the bytes-path guard inside an element loop's
// comma branch: a container close (or EOF) right after a comma is invalid JSON.
func emitNoCloseAfterComma(b *bytes.Buffer, field, posVar string, close byte) {
	sentinel := "ggen.ErrBadArray"
	if close == '}' {
		sentinel = "ggen.ErrBadObject"
	}
	fmt.Fprintf(b, "if %[1]s >= len(data) || data[%[1]s] == '%[2]c' { return result, %[1]s, ggen.NewParseErr(%[4]s, %[1]s, %[3]s) }\n", posVar, close, sentinel, field)
}

// streamNoCloseAfterComma is emitNoCloseAfterComma's stream twin. The
// `s.Pos >= len(...)` half also catches EOF right after the comma (SkipSpace
// returns nil at EOF, and the loop top would otherwise index out of range).
func streamNoCloseAfterComma(field string, close byte) string {
	sentinel := "ggen.ErrBadArray"
	if close == '}' {
		sentinel = "ggen.ErrBadObject"
	}
	return fmt.Sprintf("if s.Pos >= len(s.Bytes()) || s.Bytes()[s.Pos] == '%c' { return result, ggen.NewParseErr(%s, s.Offset(), %s) }\n", close, field, sentinel)
}

// appendAnyFn mirrors appendStrFn for `any` values — htmlescape structs
// route their any-walk through the HTML-safe variant so nested strings
// escape consistently with sibling string fields.
func appendAnyFn(htmlEscape bool) string {
	if htmlEscape {
		return "ggen.AppendAnyHTML"
	}
	return "ggen.AppendAny"
}

// foldLeadingQuote checks whether the field's value emit begins with a JSON
// `"`. If so, it returns the prefix with `"` appended and the value-emit code
// with the opening quote elided — saves one byte-append op per field.
func foldLeadingQuote(f FieldInfo, ref, prefix string) (newPrefix, code string, ok bool) {
	if f.Pointer {
		return prefix, "", false // pointer may emit "null"
	}
	switch f.Kind {
	case KindString:
		return prefix + `"`, fmt.Sprintf("dst = %s(dst, %s)\n", appendStrFn(f.HTMLEscape), ref), true
	case KindNetIP, KindNetipPrefix:
		return prefix + `"`, fmt.Sprintf(`if dst, err = %s.AppendText(dst); err != nil { return dst, err }
dst = append(dst, '"')
`, ref), true
	case KindNetipAddr:
		// Zoned text may need escaping — ggen.AppendNetipAddr handles it.
		return prefix + `"`, fmt.Sprintf("dst = %s(dst, %s)\n", appendNetipAddrFn(f.HTMLEscape), ref), true
	case KindBytes:
		// No fold: a nil []byte emits `null` (no opening quote).
	case KindURL:
		// AppendURL writes body + closing quote, escaping when RawQuery/
		// Opaque/Host smuggled bytes a JSON string can't hold raw.
		return prefix + `"`, fmt.Sprintf("dst = %s(dst, %s)\n", appendURLFn(f.HTMLEscape), ref), true
	case KindBigRat:
		return prefix + `"`, fmt.Sprintf("if dst, err = (&%s).AppendText(dst); err != nil { return dst, err }\ndst = append(dst, '\"')\n", ref), true
	case KindBigFloat:
		return prefix + `"`, fmt.Sprintf("dst = (&%s).Append(dst, 'g', -1)\ndst = append(dst, '\"')\n", ref), true
	}
	return prefix, "", false
}

// Worst-case byte budgets for primitive encodings.
const (
	sizeBool  = 5  // "false"
	sizeInt   = 20 // "-9223372036854775808"
	sizeUint  = 20 // "18446744073709551615"
	sizeFloat = 25 // IEEE-754 shortest round-trip, 'f' form: sign + "0." + 5 zeros + 17 digits
	// sizeStrMult: 2× covers short escapes (\n, \", \\). Control chars expand
	// 6× but are rejected by the decoder, so the bound is tight for legal JSON.
	sizeStrMult     = 2
	sizeStrMultHTML = 6 // htmlescape expands <, >, & to \uXXXX (6×)
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

// sizeContrib returns (constN, runtimeCode) — the constant byte count
// known at codegen time, and the runtime statements that compute the
// variable contribution. constN is folded into the initial `size := N`
// at the top level; runtimeCode is emitted as-is.
func sizeContrib(f FieldInfo, ref string) (int, string) {
	if f.Pointer {
		// nil at any level budgets `null` (4 bytes); a full chain budgets the
		// leaf. Flat else-if ladder, one rung per level.
		depth, leafType := pointerDepth(f.GoType)
		leaf := f
		leaf.Pointer = false
		leaf.PointeeType = ""
		leaf.GoType = leafType
		leaf.Kind = resolveKind(leafType)
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
	// json:",string" adds two quote bytes the inner Kind budget omits.
	// KindBool excluded (jsonv2 emits bare bool even with `,string`).
	if f.String {
		switch f.Kind {
		case KindInt, KindInt8, KindInt16, KindInt32, KindInt64,
			KindUint, KindUint8, KindUint16, KindUint32, KindUint64,
			KindFloat32, KindFloat64:
			n, code := sizeContribKind(f, ref)
			return n + 2, code
		}
	}
	return sizeContribKind(f, ref)
}

func sizeContribKind(f FieldInfo, ref string) (int, string) {
	switch f.Kind {
	case KindString:
		return sizeStrPad, fmt.Sprintf("size += len(%s)*%d\n", ref, strMult(f.HTMLEscape))
	case KindBool:
		return sizeBool, ""
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		return sizeInt, ""
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		return sizeUint, ""
	case KindFloat32, KindFloat64:
		return sizeFloat, ""
	case KindStruct:
		// Named primitive: budget the underlying, not a JSONSize() call — the
		// value emitter writes the primitive inline (see renderAppendValue).
		if prim, kind, ok := inlineNamedPrim(f); ok {
			return sizeContribKind(namedPrimInner(f, prim, kind), primCast(f.GoType, kind, ref))
		}
		if isGenerated(f.GoType) || f.Iface.JSONSize {
			return 0, fmt.Sprintf("size += %s.JSONSize()\n", ref)
		}
		return 128, ""
	case KindSlice, KindArray:
		return sizeSliceContrib(f, ref, 0)
	case KindMap:
		return sizeMapContrib(f, ref)
	case KindBytes:
		// 4-byte const covers brackets/quotes AND the nil-as-null case.
		switch f.Format {
		case "array":
			// each byte → ≤3 digits + comma; upper-bound *4.
			return 4, fmt.Sprintf("size += len(%s)*4\n", ref)
		case "base16", "hex":
			return 4, fmt.Sprintf("size += len(%s)*2\n", ref)
		case "base32", "base32hex":
			return 4, fmt.Sprintf("size += ((len(%s)+4)/5)*8\n", ref)
		}
		// base64 (default) / base64url
		return 4, fmt.Sprintf("size += ((len(%s)+2)/3)*4\n", ref)
	case KindTime:
		return timeFormatSize(f.Format), ""
	case KindDuration:
		return durationFormatSize(f.Format), ""
	case KindNetIP:
		// ParseIP returns 16 bytes even for IPv4; To4() is non-nil only for
		// addresses that fit in 4 octets.
		return 2, fmt.Sprintf("if %s.To4() != nil { size += 15 } else if len(%s) != 0 { size += 39 } else { size += 2 }\n", ref, ref)
	case KindNetipAddr:
		// Zone: '%' separator + worst-case short escapes (×2 / ×6 html);
		// raw ctrl bytes overshoot like the string budget's documented corner.
		return 2, fmt.Sprintf("if %[1]s.Is4() { size += 15 } else { size += 39 }\nif z := len(%[1]s.Zone()); z > 0 { size += 1 + z*%[2]d }\n", ref, strMult(f.HTMLEscape))
	case KindNetipPrefix:
		// Addr + /N: +4 for "/128" worst case.
		return 2, fmt.Sprintf("if %s.Addr().Is4() { size += 19 } else { size += 43 }\n", ref)
	case KindRawJSON:
		return 0, fmt.Sprintf("if n := len(%s); n > 0 { size += n } else { size += 4 }\n", ref)
	case KindURL:
		// Component sum, not a flat 256. +8 covers `"`+`://`+`?`+`#`+closing
		// `"`. Decoded fields (Path/Fragment/userinfo) ×3 for percent-escape.
		return 8, fmt.Sprintf("size += len(%s.Scheme) + len(%s.Host)*3 + len(%s.Path)*3 + len(%s.RawQuery)*2 + len(%s.Fragment)*3 + len(%s.Opaque)*2\nif %s.User != nil { pw, _ := %s.User.Password(); size += (len(%s.User.Username()) + len(pw))*3 + 2 }\n",
			ref, ref, ref, ref, ref, ref, ref, ref, ref)
	case KindBigInt:
		// log10(2^bits) ≈ bits/3, plus sign/safety.
		return 4, fmt.Sprintf("size += %s.BitLen()/3\n", ref)
	case KindBigFloat:
		// 'g' -1 digit count scales with the value's mantissa precision
		// (user-settable via SetPrec): ≈ Prec()×log10(2) ≤ Prec()/3. A flat
		// const under-reserved raised-precision values, breaking the
		// single-alloc Marshal contract. +24: sign, '.', "e-123456789", quotes.
		return 24, fmt.Sprintf("size += int(%s.Prec())/3\n", ref)
	case KindBigRat:
		return 8, fmt.Sprintf("size += (%s.Num().BitLen() + %s.Denom().BitLen())/3\n", ref, ref)
	case KindSQLNull:
		if f.SQLNullInner != nil {
			inner := sqlNullInnerField(f)
			innerN, code := sizeContrib(inner, ref+".V")
			return max(innerN, 4), code // widen for !Valid → "null"
		}
		spec, ok := SQLNullSpec(f.GoType)
		if !ok {
			return 0, ""
		}
		innerField := FieldInfo{Kind: spec.Inner, GoType: spec.Type, Format: f.Format}
		innerN, code := sizeContrib(innerField, ref+"."+spec.Field)
		// widen for !Valid → "null" (4); inner budgets <4 under-reserve otherwise.
		return max(innerN, 4), code
	case KindAny:
		// Conservative upper bound; deeply nested any can overshoot.
		return 256, ""
	}
	return 0, ""
}

// sizeSliceContrib emits the size contribution for a slice/array field.
// The 2-byte brackets are returned as a constant for the caller to fold;
// per-element accounting goes into the runtime code. Nested slices recurse
// with a depth-suffixed loop variable to avoid name collisions.
func sizeSliceContrib(f FieldInfo, ref string, depth int) (int, string) {
	ivar := fmt.Sprintf("i%d", depth)
	b := getSmall()
	defer putSmall(b)
	fmt.Fprintf(b, "if n := len(%s); n > 0 { size += n - 1 }\n", ref)
	// A named-primitive element is emitted as its underlying (see
	// renderAppendSlice), so budget the underlying, not a JSONSize() call.
	if _, kind, ok := inlineNamedPrim(elemAsField(f)); ok && !f.ElemPointer {
		f.ElemKind = kind
	}
	switch {
	case f.ElemPointer:
		// Pointer element (any depth): sizeContrib's ladder budgets `null`
		// per nil level and the deref'd leaf otherwise.
		leafN, leafCode := sizeContrib(elemPtrField(f, f.JSONName+"[]"), fmt.Sprintf("%s[%s]", ref, ivar))
		fmt.Fprintf(b, "for %s := range %s {\n", ivar, ref)
		if leafN > 0 {
			fmt.Fprintf(b, "size += %d\n", leafN)
		}
		b.WriteString(leafCode)
		b.WriteString("}\n")
	case f.ElemKind == KindString:
		fmt.Fprintf(b, "for %s := range %s { size += len(%s[%s])*%d + %d }\n",
			ivar, ref, ref, ivar, strMult(f.HTMLEscape), sizeStrPad)
	case f.ElemKind == KindBool:
		fmt.Fprintf(b, "size += len(%s) * %d\n", ref, sizeBool)
	case f.ElemKind == KindInt, f.ElemKind == KindInt64, f.ElemKind == KindInt8, f.ElemKind == KindInt16, f.ElemKind == KindInt32:
		fmt.Fprintf(b, "size += len(%s) * %d\n", ref, sizeInt)
	case f.ElemKind == KindUint, f.ElemKind == KindUint64, f.ElemKind == KindUint8, f.ElemKind == KindUint16, f.ElemKind == KindUint32:
		fmt.Fprintf(b, "size += len(%s) * %d\n", ref, sizeUint)
	case f.ElemKind == KindFloat32, f.ElemKind == KindFloat64:
		fmt.Fprintf(b, "size += len(%s) * %d\n", ref, sizeFloat)
	case f.ElemKind == KindStruct:
		if isGenerated(f.ElemType) || f.ElemIface.JSONSize {
			fmt.Fprintf(b, "for %s := range %s { size += %s[%s].JSONSize() }\n",
				ivar, ref, ref, ivar)
		} else {
			fmt.Fprintf(b, "size += len(%s) * 128\n", ref)
		}
	case f.ElemKind == KindSlice, f.ElemKind == KindArray:
		fmt.Fprintf(b, "for %s := range %s {\n", ivar, ref)
		innerN, innerCode := sizeSliceContrib(peelSliceField(f), fmt.Sprintf("%s[%s]", ref, ivar), depth+1)
		if innerN > 0 {
			fmt.Fprintf(b, "size += %d\n", innerN)
		}
		b.WriteString(innerCode)
		b.WriteString("}\n")
	default:
		// Dedicated-kind element — same per-kind budget the field level uses.
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
	// nil slice → "null" (4 bytes) is wider than `[]` (2); reserve the max.
	// Arrays can't be nil, so they keep the 2-byte bracket budget.
	if f.Kind == KindArray {
		return 2, b.String()
	}
	return 4, b.String()
}

// sizeMapContrib emits the size contribution for a `map[string]V` field.
// Per-entry overhead is `"k":v,` = 4 fixed bytes (a comma overcounted on the
// last entry, still an upper bound) + 2*len(k) for the key + value budget.
func sizeMapContrib(f FieldInfo, ref string) (int, string) {
	b := getSmall()
	defer putSmall(b)
	const perEntryFixed = 4

	mult := strMult(f.HTMLEscape)
	// Lift the value out of the loop for a constant per-entry size — leaves
	// a keys-only loop.
	_, eDepth := elemPtrType(f)
	if v, ok := constSizePerEntry(f.ElemKind, f.Format); ok && eDepth == 0 {
		fmt.Fprintf(b, "size += len(%s) * %d\n", ref, perEntryFixed+v)
		fmt.Fprintf(b, "for k := range %s { size += len(k) * %d }\n", ref, mult)
		return 4, b.String() // nil-map → "null" (4) > `{}` (2)
	}

	if eDepth > 0 {
		// Pointer value (any depth): the Pointer ladder budgets `null` per nil
		// level and the deref'd leaf otherwise.
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

	// Per-entry value budget via the canonical per-kind machinery — the same
	// sizeContrib fields and slice elements use (named prims resolve inside
	// it). A constant-only budget keeps the keys-only loop so `v` is never
	// declared-and-unused.
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
	return 4, b.String() // nil-map → "null" (4)
}

// constSizePerEntry reports whether a value of the given kind has a known
// fixed upper-bound size, and returns it. format is honored for KindTime /
// KindDuration so the budget tracks the actual layout.
func constSizePerEntry(kind TypeKind, format string) (int, bool) {
	switch kind {
	case KindBool:
		return sizeBool, true
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		return sizeInt, true
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		return sizeUint, true
	case KindFloat32, KindFloat64:
		return sizeFloat, true
	case KindTime:
		return timeFormatSize(format), true
	case KindDuration:
		return durationFormatSize(format), true
	// KindBigFloat has NO fixed bound — digit count scales with the value's
	// user-settable precision (see sizeContribKind); callers fall to the
	// per-element sizeContrib loop.
	case KindAny:
		return 64, true
	}
	return 0, false
}

func renderAppendSlice(b *bytes.Buffer, f FieldInfo, ref string) {
	emitAppendSlice(b, f, ref, 0)
}

// emitAppendSlice is the recursive marshal counterpart to emitByteSliceRead.
// Nested slices peel one [] off per level; loop vars carry a depth suffix.
// nil slice → `null`, empty non-nil → `[]` (stdlib v1/v2); arrays skip the check.
func emitAppendSlice(b *bytes.Buffer, f FieldInfo, ref string, depth int) {
	vvar := fmt.Sprintf("v%d", depth)
	if f.Kind == KindSlice {
		fmt.Fprintf(b, "if %s == nil {\ndst = append(dst, \"null\"...)\n} else {\n", ref)
	}
	fmt.Fprintf(b, "dst = append(dst, '[')\nif len(%s) > 0 {\n", ref)
	// First element: no leading comma; iterating ref[1:] lifts the `if i > 0`
	// out of the loop.
	emitSliceElement(b, f, fmt.Sprintf("%s[0]", ref), depth)
	fmt.Fprintf(b, "for _, %s := range %s[1:] {\ndst = append(dst, ',')\n", vvar, ref)
	emitSliceElement(b, f, vvar, depth)
	b.WriteString("}\n}\ndst = append(dst, ']')\n")
	if f.Kind == KindSlice {
		b.WriteString("}\n")
	}
}

// emitSliceElement emits the marshal code for one slice element. Shared
// between the first-element and loop-body emits in emitAppendSlice.
func emitSliceElement(b *bytes.Buffer, f FieldInfo, vref string, depth int) {
	// Named-primitive element: append the underlying, converting at the read.
	if prim, kind, ok := inlineNamedPrim(elemAsField(f)); ok && !f.ElemPointer {
		nf := f
		nf.ElemType, nf.ElemKind = prim, kind
		emitSliceElement(b, nf, primCast(f.ElemType, kind, vref), depth)
		return
	}
	if f.ElemPointer {
		// nil at any pointer level → null; flat else-if ladder, then recurse
		// on the deref'd leaf.
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
		_, nf.ElemType = pointerDepth(f.ElemType)
		nf.ElemKind = resolveKind(nf.ElemType)
		if nf.ElemKind == KindArray {
			nf.ElemArrayLen = arrayLenFromType(nf.ElemType)
		}
		emitSliceElement(b, nf, derefStr(vref, ptrDepth), depth)
		b.WriteString("}\n")
		return
	}
	switch f.ElemKind {
	case KindString:
		// Two lines so the coalescer folds the `'"'` with the preceding const.
		fmt.Fprintf(b, "dst = append(dst, '\"')\ndst = %s(dst, %s)\n", appendStrFn(f.HTMLEscape), vref)
	case KindBool:
		fmt.Fprintf(b, "dst = strconv.AppendBool(dst, %s)\n", vref)
	case KindInt, KindInt8, KindInt16, KindInt32:
		fmt.Fprintf(b, "dst = strconv.AppendInt(dst, int64(%s), 10)\n", vref)
	case KindInt64:
		fmt.Fprintf(b, "dst = strconv.AppendInt(dst, %s, 10)\n", vref)
	case KindUint, KindUint8, KindUint16, KindUint32:
		fmt.Fprintf(b, "dst = strconv.AppendUint(dst, uint64(%s), 10)\n", vref)
	case KindUint64:
		fmt.Fprintf(b, "dst = strconv.AppendUint(dst, %s, 10)\n", vref)
	case KindFloat32:
		fmt.Fprintf(b, "if dst, err = ggen.AppendFloat(dst, float64(%s), 32); err != nil { return dst, err }\n", vref)
	case KindFloat64:
		fmt.Fprintf(b, "if dst, err = ggen.AppendFloat(dst, %s, 64); err != nil { return dst, err }\n", vref)
	case KindStruct:
		if isGenerated(f.ElemType) {
			fmt.Fprintf(b, "if dst, err = %s.AppendJSON(dst); err != nil { return dst, err }\n", vref)
		} else {
			// Same ladder as the field level (AppendJSON → MarshalJSON →
			// AppendText → MarshalText → encoding/json), so a foreign ggen
			// element marshals the way it decodes. Braced: the temp-declaring
			// arms emit once for the first element and once in the loop.
			fmt.Fprintf(b, "{\n%s}\n", renderCrossPkgStructAppend(sliceElemField(f), vref))
		}
	case KindSlice, KindArray:
		emitAppendSlice(b, peelSliceField(f), vref, depth+1)
	default:
		// Dedicated-kind element — same value emitter the field level uses.
		// Used to fall through and emit NOTHING (unused range var, no value).
		renderAppendValue(b, sliceElemField(f), vref)
	}
}

// renderAppendMap emits marshal code for a map[string]V field. Iteration order
// is Go's randomized map order (wire output not stable). nil map → null,
// empty non-nil → {}. The first-entry flag is field-suffixed to avoid collisions.
func renderAppendMap(b *bytes.Buffer, f FieldInfo, ref string) {
	appendStr := appendStrFn(f.HTMLEscape)
	first := "first" + strings.ReplaceAll(f.GoName, ".", "")
	fmt.Fprintf(b, `if %[1]s == nil {
dst = append(dst, "null"...)
} else {
dst = append(dst, '{')
%[3]s := true
for k, v := range %[1]s {
if %[3]s { %[3]s = false
dst =append(dst, '"') } else { dst = append(dst, ",\""...) }
dst = %[2]s(dst, k)
dst = append(dst, ':')
`, ref, appendStr, first)
	if _, eDepth := elemPtrType(f); eDepth > 0 {
		// Pointer value (any depth): renderAppendValue's ladder handles nil/leaf.
		renderAppendValue(b, elemPtrField(f, f.JSONName+".value"), "v")
		b.WriteString("}\ndst = append(dst, '}')\n}\n")
		return
	}
	// Named-primitive value: append the underlying (the range var `v` carries
	// the named type, so convert at the use site).
	vref := "v"
	if prim, kind, ok := inlineNamedPrim(elemAsField(f)); ok {
		vref = primCast(f.ElemType, kind, "v")
		f.ElemType, f.ElemKind = prim, kind
	}
	switch f.ElemKind {
	case KindString:
		// Two lines so coalesce merges the `'"'` with the preceding `':'`.
		fmt.Fprintf(b, "dst = append(dst, '\"')\ndst = %s(dst, %s)\n", appendStr, vref)
	case KindBool:
		fmt.Fprintf(b, "dst = strconv.AppendBool(dst, %s)\n", vref)
	case KindInt, KindInt8, KindInt16, KindInt32:
		b.WriteString("dst = strconv.AppendInt(dst, int64(v), 10)\n")
	case KindInt64:
		fmt.Fprintf(b, "dst = strconv.AppendInt(dst, %s, 10)\n", vref)
	case KindUint, KindUint8, KindUint16, KindUint32:
		b.WriteString("dst = strconv.AppendUint(dst, uint64(v), 10)\n")
	case KindUint64:
		fmt.Fprintf(b, "dst = strconv.AppendUint(dst, %s, 10)\n", vref)
	case KindFloat32:
		b.WriteString("if dst, err = ggen.AppendFloat(dst, float64(v), 32); err != nil { return dst, err }\n")
	case KindFloat64:
		fmt.Fprintf(b, "if dst, err = ggen.AppendFloat(dst, %s, 64); err != nil { return dst, err }\n", vref)
	case KindStruct:
		if isGenerated(f.ElemType) {
			b.WriteString("if dst, err = v.AppendJSON(dst); err != nil { return dst, err }\n")
		} else {
			// Loop-scoped — no wrapper needed. Same ladder as the field level,
			// so a foreign ggen value marshals the way it decodes.
			b.WriteString(renderCrossPkgStructAppend(sliceElemField(f), vref))
		}
	case KindAny:
		fmt.Fprintf(b, "if dst, err = %s(dst, v); err != nil { return dst, err }\n", appendAnyFn(f.HTMLEscape))
	default:
		// Dedicated-kind value — same value emitter the field level uses.
		// Used to fall through and emit `"k":` with no value (unused `v`).
		vf := sliceElemField(f)
		vf.JSONName = f.JSONName + ".value"
		renderAppendValue(b, vf, vref)
	}
	b.WriteString("}\ndst = append(dst, '}')\n}\n")
}

// userPreallocHint extracts an explicit sizing hint from the hintlen / len /
// minlen ladder, in that order. Every one of them outranks the width-derived
// default in preallocCap: the tag states what the payload actually holds,
// which always beats a guess from the element's size. Returns -1 for "unset"
// so callers distinguish hintlen=0 (opt-out) from no hint. `maxlen` is NOT
// used — it's a bound, not an expected size, and only ever clamps DOWN.
func userPreallocHint(f FieldInfo) int {
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

// mapPreallocCap returns the cap for `make(map[K]V, cap)` from the user's
// sizing hints, or 0 with no hint — maps get no kind-based default (makemap
// lazy-allocates below 8, and a bigger default over-allocates the common case).
func mapPreallocCap(f FieldInfo) int {
	if n := userPreallocHint(f); n >= 0 {
		return n
	}
	return 0
}

// ownsAllocations reports whether decoding into a carried value of the named
// generated struct recycles anything. A struct of plain scalars owns nothing —
// its decode overwrites every field either way — so reading the old value back
// would buy nothing while still costing the map swap.
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
		switch f.Kind {
		case KindSlice, KindMap, KindBytes, KindAny, KindRawJSON:
			return true
		case KindStruct:
			if isGenerated(f.GoType) && ownsAllocations(f.GoType, seen) {
				return true
			}
		}
	}
	return false
}

// reusesMapValues reports whether f's map decode should read each entry's
// previous value back as the decode target. The decode fills a NEW map while
// reading the carried one, which is what drops the keys the payload omitted
// without tracking which keys it saw — paid for by one map allocation, so it
// only makes sense when the values actually own something to recycle.
//
// Struct values must be ggen-generated: their decoder resets what it is handed
// (opt #74), while a value reaching encoding/json or an UnmarshalJSON rung
// would MERGE into it. Slice and map values carry their own backing and are
// reset at the seed site.
func reusesMapValues(f FieldInfo) bool {
	// The catch-all map is filled by unknownKey, not renderMap, so no swap is
	// emitted for it and its entry clear() has to stand.
	if f.Kind != KindMap || f.Embed {
		return false
	}
	// A pointer value recycles its pointee: the cascade decodes into the
	// carried chain instead of allocating a new one per entry.
	if _, d := elemPtrType(f); d > 0 {
		return true
	}
	switch f.ElemKind {
	case KindSlice, KindMap, KindBytes:
		return true
	case KindRawJSON:
		// A raw span ALIASES the input unless -copy is on, so without it the
		// value owns nothing and the swap would be pure cost.
		return f.Copy
	case KindStruct:
		return isGenerated(f.ElemType) && ownsAllocations(f.ElemType, nil)
	}
	return false
}

// preallocCap returns the initial caps for a slice field's two backing
// allocations, as Go EXPRESSIONS: the field's own slice
// (`make([]E,0,slice)`; "0" means no prealloc) and, for `[]*T`, the contiguous
// slab (`make([]T,0,slab)`, ignored otherwise). Explicit hints
// (hintlen/len/minlen) apply to both and still outrank everything.
//
// With no hint the cap comes from the ELEMENT WIDTH rather than its kind:
// `prealloc.Cap(unsafe.Sizeof(*new(E)))`, which folds to a literal at
// compile time (verified in asm — `MOVL $4`). See internal/prealloc for the
// ladder; the short version is "as many elements as fit under 80 bytes, else
// under 512, else 1". That replaced a flat 4 for primitives and, more
// importantly, a cap of ZERO for struct elements — those used to walk the
// 1→2→4→8 chain because "sizeof(T) is unbounded", which stopped being true the
// moment the cap became an expression the compiler evaluates.
//
// `maxlen` is still NOT a generous hint — it's a bound, so it only ever clamps
// the width guess DOWN.
func preallocCap(f FieldInfo) (slice, slab string) {
	if n := userPreallocHint(f); n >= 0 {
		return strconv.Itoa(n), strconv.Itoa(n)
	}
	// Default cap comes from the ELEMENT WIDTH, not the element kind:
	// `prealloc.Cap(unsafe.Sizeof(*new(T)))`. Both operands are
	// compile-time constants, so gc folds the call and its branches to a
	// literal — the emitted code carries `make([]T, 0, 4)`, not a call.
	// `*new(T)` (rather than `T{}`) spells a zero value of ANY type, and
	// unsafe.Sizeof never evaluates its operand.
	// `maxlen=N` is the EXACT upper bound on the element count, so when N
	// elements still fit the span budget it beats the width guess outright —
	// the slice can never outgrow it, and never over-allocates past what the
	// payload is allowed to contain.
	maxlen := -1
	if v, ok := f.HasRule("maxlen"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxlen = n
		}
	}
	scope := capScope(f)
	elem := capFor(scope, f.ElemType, maxlen)
	if f.ElemPointer {
		// `[]*T`: the slice holds pointers, the slab holds T values.
		return capFor(scope, "*"+f.ElemType, maxlen), elem
	}
	switch f.ElemKind {
	case KindSlice, KindMap:
		// Element is a slice header / map handle; the slab is unused.
		return elem, "0"
	case KindStruct, KindArray:
		// Was cap 0 ("sizeof(T) unbounded; start nil, grow") — the width is
		// known at compile time, so a wide element now gets a real, bounded cap
		// instead of the 1→2→4→8 chain.
		return elem, "0"
	}
	return elem, "0"
}

// capForSize returns the name of a package-level constant holding the
// width-driven default capacity for elemType, registering it on first use.
//
// It has to be a CONSTANT EXPRESSION, not a call to prealloc.Cap:
// measured on the bench module, gc inlines that helper only into small
// functions — 30 of 34 emitted sites kept a real `CALL` because the enclosing
// generated DecodeFrom is far past the inliner's budget. A constant folds in
// the frontend, where the inliner never gets a vote.
//
// The expression is prealloc.Cap's ladder written branchlessly, with
// `sel` as the 0/1 selector for "at least 2 elements fit under 80 bytes":
//
//	S   = max(sizeof(E), 1)          // 1 keeps a zero-width element from dividing by zero
//	A   = 79 / S                     // fits strictly under 80
//	B   = 512 / S                    // fits within one 512-byte span
//	sel = min(A, 2) / 2              // 1 when A >= 2, else 0
//	cap = sel*A + (1-sel)*max(B, 1)
//
// capScope names the cap consts a field registers: the struct being rendered
// plus the field's Go name (empty for top-level container aliases, whose
// struct part IS the alias name). Struct names are package-unique, so the
// names can't collide across files without any hashing.
func capScope(f FieldInfo) string {
	if f.GoName == "" {
		return currentStructName
	}
	return currentStructName + "_" + sanitizeIdent(f.GoName)
}

var currentStructName string

func capFor(scope, elemType string, maxlen int) string {
	base := capForSize(scope, elemType)
	if maxlen < 0 {
		return base
	}
	key := fmt.Sprintf("%s\x00%s/%d", scope, elemType, maxlen)
	if name, ok := capRegistry.names[key]; ok {
		return name
	}
	name := capName(scope, fmt.Sprintf("%s_%d", elemType, maxlen))
	capRegistry.names[key] = name
	// fits = 1 when maxlen elements stay within the 512-byte span budget.
	fits := fmt.Sprintf("(1 - min((%d*%s)/(%s+1), 1))", maxlen, sizeExpr(elemType), spanBudgetExpr)
	capRegistry.decls = append(capRegistry.decls, fmt.Sprintf(
		"// %s: prealloc cap for []%s — its maxlen=%d bound when that many\n"+
			"// elements fit a 512-byte span, else the width default.\nconst %s = %s*%d + (1-%s)*%s",
		name, elemType, maxlen, name, fits, maxlen, fits, base))
	return name
}

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

// fastAllocMax mirrors internal/prealloc — the emitted constants have to spell
// the same ladder the runtime helper documents and tests.
// fastAllocMax is go1.27's size-specialized-malloc cutoff; inert on go1.26.
// INCLUSIVE — verified against master: the tables are [specializedMallocMax+1]
// and both gates admit exactly 80 (see prealloc.Cap). So the tier divides
// by 80, not 79; the earlier 79 dropped one element for every width dividing 80
// and pushed a 40-byte element into the span tier (12 elements) where 2 fit the
// fast one exactly.
const fastAllocMax = 80

// spanBudgetExpr is the runtime's own boundary, spelled so the GENERATED code
// computes it for ITS target rather than inheriting this host's: the value is
// `goarch.PtrSize * goarch.PtrBits` = 8 × PtrSize² (512 on 64-bit, 128 on
// 32-bit). Hardcoding 512 would over-budget a 32-bit target by 4×. See
// internal/prealloc for what staying under it actually buys — the headline is
// "no malloc header", not the GC.
const spanBudgetExpr = "(8*int(unsafe.Sizeof(uintptr(0)))*int(unsafe.Sizeof(uintptr(0))))"

func sizeExpr(elemType string) string {
	return fmt.Sprintf("max(int(unsafe.Sizeof(*new(%s))), 1)", elemType)
}

func capName(scope, key string) string {
	return fmt.Sprintf("ggenCap_%s_%s", scope, sanitizeIdent(key))
}

// capRegistry collects the per-element-type prealloc constants emitted at the
// top of the file, deduped by element type. Shares oneofRegistry's per-file
// prefix so two sources in one package can't collide.
var capRegistry struct {
	names map[string]string
	decls []string
}

func resetCapRegistry() {
	capRegistry.names = map[string]string{}
	capRegistry.decls = nil
}

// fieldLit returns the JSON name of f as a Go string literal — the `field`
// argument embedded into ggen.NewParseErr at every error-return site.
func fieldLit(f FieldInfo) string { return strconv.Quote(f.JSONName) }

// inlineSkipWS emits an inline whitespace-skipping loop that mutates posVar
// directly, avoiding the ggen.SkipSpace call overhead.
func inlineSkipWS(b *bytes.Buffer, posVar string) {
	// SIMD tier: consume one whitespace byte inline (compact JSON exits on
	// one compare, a single separator space stays call-free), then hand a
	// 2+ run to the vector tier — pretty-printed indent runs skip a lane at
	// a time instead of byte-stepping.
	if simdSuffix != "" {
		fmt.Fprintf(b,
			"if %[1]s < len(data) && data[%[1]s] <= ' ' && (data[%[1]s] == ' ' || data[%[1]s] == '\\t' || data[%[1]s] == '\\n' || data[%[1]s] == '\\r') {\n%[1]s++\nif %[1]s < len(data) && data[%[1]s] <= ' ' { %[1]s = ggen.SkipSpace%[2]s(data, %[1]s) }\n}\n",
			posVar, simdSuffix)
		return
	}
	// `data[i] <= ' '` gates the 4-way test so compact JSON exits on one
	// compare. Boolean-identical accept set (every whitespace char is <= ' ').
	fmt.Fprintf(b,
		"for %s < len(data) && data[%s] <= ' ' && (data[%s] == ' ' || data[%s] == '\\t' || data[%s] == '\\n' || data[%s] == '\\r') { %s++ }\n",
		posVar, posVar, posVar, posVar, posVar, posVar, posVar)
}

// inlineNullPeek emits an inline `null` check on posVar, advancing it 4 bytes
// on a match. Leaves the `if` body open for the caller's null-branch + `} else`.
func inlineNullPeek(b *bytes.Buffer, posVar string) {
	fmt.Fprintf(b,
		"if %s+4 <= len(data) && data[%s] == 'n' && data[%s+1] == 'u' && data[%s+2] == 'l' && data[%s+3] == 'l' {\n%s += 4\n",
		posVar, posVar, posVar, posVar, posVar, posVar)
}

// zeroLit returns the zero-value expression for an elem type, the pre-grow
// placeholder for in-place decode. Slice/map use `nil` (overwritten before
// observed); arrays/structs need the `T{}` composite literal.
func zeroLit(elemType string, kind TypeKind) string {
	// Pointer types carry the POINTEE's kind (KindInt for `*int`), so the
	// conversion below would emit `*int(0)`. Every pointer zeroes to nil.
	if strings.HasPrefix(elemType, "*") {
		return "nil"
	}
	// A named primitive (`type Priority string`) reports KindStruct at its use
	// sites, and `Priority{}` is not a valid literal for it — resolve to the
	// underlying kind and convert the primitive zero instead.
	kind = effectiveKind(elemType, kind)
	zero := ""
	switch kind {
	case KindString:
		zero = `""`
	case KindBool:
		zero = "false"
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64,
		KindUint, KindUint8, KindUint16, KindUint32, KindUint64,
		KindFloat32, KindFloat64:
		zero = "0"
	case KindSlice, KindMap:
		return "nil"
	case KindAny, KindBytes, KindRawJSON, KindNetIP:
		// `any{}` / composite literals over slice-backed kinds are invalid
		// or wasteful — their zero is nil.
		return "nil"
	case KindDuration:
		// time.Duration is a named int64 — `time.Duration{}` doesn't compile.
		return "0"
	default:
		return elemType + "{}"
	}
	if prim := kindPrimitiveName(kind); elemType != "" && elemType != prim {
		return elemType + "(" + zero + ")"
	}
	return zero
}

// inlineScanInt64 emits an inline signed-int scanner that assigns into dst
// (via castFn if non-empty) and advances posVar, with overflow detection.
// field is the JSON-path expression for ggen.NewParseErr (a quoted literal,
// `""` for boundaries, or a runtime expr like `key` for unknown-key handlers).
// narrowIntBounds returns the (min, max) literal bounds a fixed-width integer
// type narrower than 64-bit imposes — the types a bare Go conversion silently
// truncates. ok=false for int/int64/uint/uint64 (64-bit on target platforms, so
// the MaxInt64/MaxUint64 scan bound already covers them).
// kindNarrowName maps a narrow integer kind to its builtin spelling for
// narrowIntGuard — f.GoType may be a named type whose bounds these are.
func kindNarrowName(k TypeKind) string {
	switch k {
	case KindInt8:
		return "int8"
	case KindInt16:
		return "int16"
	case KindInt32:
		return "int32"
	case KindUint8:
		return "uint8"
	case KindUint16:
		return "uint16"
	case KindUint32:
		return "uint32"
	}
	return ""
}

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

// narrowIntGuard emits an in-range check before the truncating cast to a narrow
// integer target: an out-of-range value returns ErrNumberOverflow instead of
// silently wrapping (`uint8` ← 300 = 44), matching encoding/json v1 + jsonv2
// which both reject. wideVar holds the scanned int64/uint64; errRet is the
// caller's overflow return. Empty for non-narrow types (one predicted compare
// on the happy path).
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

// narrowFloatGuard mirrors narrowIntGuard for float32 targets: a float64
// whose float32 conversion overflows to ±Inf returns ErrNumberOverflow —
// stdlib v1 and jsonv2 both reject out-of-range float32, and "converts to
// Inf" is exactly stdlib's rounding boundary (not MaxFloat32, which would
// wrongly reject values that round DOWN to it).
func narrowFloatGuard(wideVar, typ, errRet string) string {
	if typ != "float32" {
		return ""
	}
	return fmt.Sprintf("if math.IsInf(float64(float32(%s)), 0) { %s }\n", wideVar, errRet)
}

// foldTailAssign inlines a write-once temp: when body ends with `tmp = EXPR`
// and tmp appears nowhere else in it, the trailing assign is dropped and EXPR
// returned for the caller to place directly at the single consumer (composite
// literal / cast site) — kills the `var nv T; …; nv = float32(fv); {V: nv}`
// chains. Multi-assign tails (`tmp, i, err = …`) and branching writers
// (inline string scans) don't match and keep their temp.
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

// inlineScanInt64Stmt emits the inline signed-int scan, then `stmt` with the
// parsed value in `n` (int64). Brace-less: `neg`/`limit`/`u`/`n` land in the
// caller's scope (one numeric scan per scope).
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

// inlineScanUint64Stmt is the unsigned counterpart of inlineScanInt64Stmt:
// the parsed value lands in `n` (uint64) before `stmt` runs. Brace-less —
// see inlineScanInt64Stmt.
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

// inlineScanString emits a string reader that assigns into dst and advances
// posOut past the closing quote. The hot path aliases via unsafe.String (or
// copies via string(...) when cp is set — the -copy mode, so the result no
// longer references data); escapes fall back to ggen.String (already an owned
// copy). The loop also stops on any control byte (<0x20), which RFC 8259 /
// jsonv2 reject, and on any non-ASCII byte (>=0x80) — the inline fast paths
// alias without validating, so non-ASCII spans hand off to ggen.String*, which
// UTF-8-validates (ErrInvalidUTF8, jsonv2 parity). Pure-ASCII short strings —
// the dominant population — stay inline. Brace-less: `ke` lands in the caller's scope (renderMap's
// value scan uses inlineScanStringVar to avoid colliding with the key scan's
// `ke`). cp is set only at sites whose string is RETAINED past the scan (field
// value, map key/value, slice/array elem); transient parse-feeds (time, url,
// netip, big*) stay aliasing regardless — their conversion owns its output.
// scalarStringWindow bounds the scalar-tier inline string byte-scan: a span
// that runs past this many bytes (or hits an escape/ctrl) hands off to
// ggen.String's SIMD IndexByte locate instead of walking the whole body one
// byte at a time. Sized so keys and short values stay inline while long
// strings (bios, URLs, descriptions) take the vectorized path.
const scalarStringWindow = 32

// vArg renders the validate literal for string-scan calls: "false" when the
// struct opted out via allowinvalidutf8, else "true".
func vArg(f FieldInfo) string {
	if f.AllowInvalidUTF8 {
		return "false"
	}
	return "true"
}

// vArgS is vArg at struct level (dispatch-key scans).
func vArgS(s StructInfo) string {
	if s.AllowInvalidUTF8 {
		return "false"
	}
	return "true"
}

func inlineScanString(b *bytes.Buffer, posIn, dst, posOut, field string, cp bool, validate bool) {
	inlineScanStringVar(b, posIn, dst, posOut, field, "ke", cp, validate)
}

// maxJSONNameLen returns the longest declared JSON key name on s (0 when no
// named fields — embedded fallbacks carry arbitrary keys and are excluded).
func maxJSONNameLen(s StructInfo) int {
	n := 0
	for _, f := range s.Fields {
		if f.Embed {
			continue
		}
		n = max(n, len(f.JSONName))
	}
	return n
}

// inlineScanStringVar is inlineScanString with a caller-chosen name for the
// closing-quote cursor local.
func inlineScanStringVar(b *bytes.Buffer, posIn, dst, posOut, field, ke string, cp bool, validate bool) {
	inlineScanStringWin(b, posIn, dst, posOut, field, ke, cp, validate, 0)
}

// inlineScanStringWin is inlineScanStringVar with a scalar-window override for
// the SIMD tier: window > 0 swaps the inline vector classify for a bounded
// scalar loop over the first `window` bytes — used by the key-dispatch scan
// when every declared key is short enough that the vector dependency chain
// loses to a handful of predictable scalar iterations. window == 0 emits the
// vector shape. Ignored on the scalar tier.
func inlineScanStringWin(b *bytes.Buffer, posIn, dst, posOut, field, ke string, cp bool, validate bool, window int) {
	// allowinvalidutf8: permissive shapes are byte-identical to the
	// pre-validation emitter - no <0x80 window bail, Min-Equal ctrl-only
	// vector classify, validate=false at the runtime calls.
	vLit, win80 := "true", " && data[%[5]s] < 0x80"
	if !validate {
		vLit, win80 = "false", ""
	}
	// SIMD tier: an inline fused vector classify (no call) handles any string
	// shorter than one lane — one load + Equal/Equal/range classify → ToBits
	// → TrailingZeros finds the first structural byte ('"', '\', ctrl, or
	// non-ASCII: (v-0x20) >= 0x60 folds ctrl AND >=0x80 into one Sub+Max+Equal,
	// same op count as the old Min-Equal ctrl term alone). Quote hit → inline
	// alias/copy; anything else (escape, ctrl, non-ASCII needing UTF-8
	// validation, span ≥ lane, string near the payload end where a full-lane
	// load would overread) falls through to the fused ggen.StringAVX* call, which
	// restarts at posIn — error identity byte-identical. Full-lane loads
	// only: Load*Part is a real CALL, not an intrinsic. Broadcasts are
	// emitted per site; gc CSEs them across sites and hoists them out of
	// loops. In cp mode the happy path copies inline (string(data[…])); the
	// escape/long-span fall calls the SAME aliasing tier func and detaches via
	// ggen.Detach — one clone only when the result aliases data (a stringSlow
	// escape result is already owned, so Detach skips the clone, killing the
	// escape double-copy). Reuses the tier StringAVX* directly — no per-tier
	// StringCopy variant.
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
			// Permissive: old Min-Equal ctrl-only classify (high bytes stay
			// inline — no validation to route to).
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
	// Scalar tier. window < 0 = unbounded original loop (dispatch keys: short,
	// matched against known field names, so a window bound is pure per-key setup
	// with no long span to hand off — it regressed tiny structs). window >= 0 =
	// bound the inline byte loop and hand any string that runs past it (or hits
	// an escape/ctrl byte) to ggen.String, whose bytes.IndexByte locate is
	// SIMD/AVX2 and crushes the per-byte loop on long spans (bios, URLs). ggen.String
	// is the error-identity source of truth (ErrUnterminated/ErrBadString); the
	// bounded loop only fast-paths a clean quote-terminated span. Same shape as
	// the SIMD window>0 template above.
	hot := "unsafe.String(unsafe.SliceData(data[%[1]s+1:]), %[5]s-%[1]s-1)"
	if cp {
		hot = "string(data[%[1]s+1:%[5]s])"
	}
	// Everything that is not a clean closing quote (run off the end, ctrl
	// byte, escape, high byte) falls to ggen.String — the error-identity AND
	// error-position source of truth. Early-bailing here reported the opening
	// quote's offset and diverged from the SIMD tier, which always falls.
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

// emitReceiverReset emits the per-container reset at the top of DecodeFrom /
// DecodeFromStream. The decoder is decode-into-receiver, so containers MUST be
// reset (slices/[]byte/inline-map → `[:0]`/`clear`, backing reused) before the
// decoder appends over carried-in data. Deliberately UNCONDITIONAL — a blank
// payload yields a blank slate while keeping capacity. Arrays (strict-length)
// and pointer fields (handled in renderField) are skipped.
func emitReceiverReset(b *bytes.Buffer, s StructInfo, bytesPath bool) {
	for _, f := range s.Fields {
		ref := "result." + f.GoName
		if !f.Pointer {
			switch f.Kind {
			case KindSlice, KindBytes:
				if byteArrayLen(f) > 0 {
					continue // a fixed array has no header to reset
				}
				fmt.Fprintf(b, "if %[1]s != nil { %[1]s = %[1]s[:0] }\n", ref)
			case KindMap:
				if bytesPath && reusesMapValues(f) {
					// The bytes decode reads this map while filling a new one,
					// so clearing here would destroy the values it reuses. The
					// stream path does not swap, so it still needs the clear.
					continue
				}
				fmt.Fprintf(b, "if %[1]s != nil { clear(%[1]s) }\n", ref)
			}
			continue
		}
		// A pointer to a container reuses the pointee it was handed (the
		// decode path only allocates when the pointer itself is nil), so
		// without a reset here the element loop APPENDS to whatever the
		// receiver carried in — `*[]T` merged where `[]T` replaced. Peel every
		// level, guarding each, and reset through the final deref: `*[]T`,
		// `*****[]T`, `**map[string]T` all land here.
		depth, leaf := pointerDepth(f.GoType)
		leafKind := resolveKind(leaf)
		if depth == 0 || (leafKind != KindSlice && leafKind != KindMap && leafKind != KindBytes) {
			continue
		}
		guards := make([]string, 0, depth)
		for i := range depth {
			guards = append(guards, strings.Repeat("*", i)+ref+" != nil")
		}
		deref := "(" + strings.Repeat("*", depth) + ref + ")"
		// The innermost container may still be nil; `nil[:0]` and `clear(nil)`
		// are both fine, so only the pointer levels need guarding.
		if leafKind == KindMap {
			if bytesPath && reusesMapValues(f) {
				// Same as the non-pointer case: the decode swaps this map for
				// a fresh one and reads the carried entries, so clearing here
				// would empty what it reuses.
				continue
			}
			fmt.Fprintf(b, "if %s { clear(%s) }\n", strings.Join(guards, " && "), deref)
		} else {
			fmt.Fprintf(b, "if %s { %s = %s[:0] }\n", strings.Join(guards, " && "), deref, deref)
		}
	}
}

// elemPtrReusable reports whether a multi-level pointer ELEMENT can decode
// into the chain the receiver carried in that slot. A primitive leaf is fully
// overwritten by its scan and a generated struct leaf resets itself (opt #74),
// so neither can carry data forward; anything reaching encoding/json or an
// UnmarshalJSON rung would MERGE into the carried value instead.
func elemPtrReusable(f FieldInfo) bool {
	// A multi-level element reports KindStruct with the remaining pointer
	// levels still in ElemType, so resolve the LEAF before judging it.
	et, _ := elemPtrType(f)
	_, leaf := pointerDepth(et)
	if resolveKind(leaf) == KindStruct {
		return isGenerated(leaf)
	}
	return true
}

// emitElemGrow pre-grows dst by one slot for in-place element decode. A
// generated struct element resets itself on decode, so a within-cap grow hands
// it the carried element and its inner allocations get reused; every other
// element kind is pre-grown with a zero value.
func emitElemGrow(b *bytes.Buffer, dst string, f FieldInfo, directStruct bool) {
	if directStruct {
		fmt.Fprintf(b, "if len(%[1]s) < cap(%[1]s) { %[1]s = %[1]s[:len(%[1]s)+1] } else { %[1]s = append(%[1]s, %[2]s) }\n",
			dst, zeroLit(f.ElemType, f.ElemKind))
		return
	}
	fmt.Fprintf(b, "%s = append(%s, %s)\n", dst, dst, zeroLit(f.ElemType, f.ElemKind))
}

// fieldZeroLit is the zero-value expression for a whole FIELD. zeroLit is
// elem-oriented and maps every slice-backed kind to nil, which a `[N]byte`
// field cannot use.
//
// A type spelled with a package qualifier is written as the zero struct's own
// field (`(T{}).X`) instead: the qualifier may not be imported by the
// generated file (a value field otherwise only ever writes `result.X`), and a
// foreign named primitive or interface takes no composite literal anyway.
func fieldZeroLit(s StructInfo, f FieldInfo) string {
	if f.Pointer {
		return "nil"
	}
	lit := zeroLit(f.GoType, f.Kind)
	if f.Kind == KindBytes && byteArrayLen(f) > 0 {
		lit = f.GoType + "{}"
	}
	if strings.Contains(lit, ".") {
		return "(" + s.Name + "{})." + f.GoName
	}
	return lit
}

// needsOmittedZero reports whether f must be zeroed when its key never
// appeared. Containers are excluded: they are already emptied by
// emitReceiverReset and keep their capacity for reuse.
func needsOmittedZero(f FieldInfo) bool {
	if f.Embed || !needsSeen(f) {
		return false
	}
	if f.Pointer {
		return true
	}
	switch f.Kind {
	case KindSlice, KindMap:
		return false
	case KindBytes:
		// `[]byte` is a container; `[N]byte` is a value with no entry reset.
		return byteArrayLen(f) > 0
	}
	return true
}

// emitOmittedZero zeroes every field the payload did not set, at the end of a
// successful decode. Decode-into-receiver yields what a fresh decode would
// give — only container capacity and element allocations are recycled.
// Pointers are cleared HERE rather than at entry so a PRESENT key can still
// reuse the carried pointee chain.
func emitOmittedZero(b *bytes.Buffer, s StructInfo, bytesPath bool) {
	for _, f := range s.Fields {
		// A map the bytes decode SWAPS has no entry clear() — the swap is what
		// empties it, and it only runs when the key is present. An omitted key
		// therefore has to be emptied here, or the carried entries survive a
		// payload that never mentioned them.
		if bytesPath && !f.Pointer && f.Kind == KindMap && reusesMapValues(f) {
			fmt.Fprintf(b, "if %s { clear(result.%s) }\n", seenNotAccess(s, f), f.GoName)
			continue
		}
		if !needsOmittedZero(f) {
			continue
		}
		fmt.Fprintf(b, "if %s { result.%s = %s }\n", seenNotAccess(s, f), f.GoName, fieldZeroLit(s, f))
	}
}

// renderDecode emits the body of DecodeFrom: a loop reading each JSON key,
// dispatching to per-field scan code, handling ',' / '}'. Zero-copy and
// zero-alloc on the happy path; whitespace skipping inlined at each hot site.
func renderDecode(bOut *bytes.Buffer, s StructInfo) {
	// Rendered into a temp buffer so the SIMD tier can rewrite the
	// ggen.SkipValue callee post-emit (unique token, no collision risk) —
	// the tier skip tree vector-skips whitespace runs + fuses skipString.
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
	// Named results home the values in the caller's return slot, so every
	// `return result, …` is register-set + RET with no struct copy; the
	// `result = recv` prologue seeds the merge source.
	if isCyclic(s.Name) {
		// Self-referential type: bound payload nesting at maxDepth (10000) via a
		// depth-threaded core; the public method is a thin seed-0 shim so the
		// surface (and acyclic callers) stay unchanged.
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
	// Only structs that actually call into a cyclic nested type need `depth`
	// in scope (decodeCallFor emits `depth+1` there) — render first and gate
	// on that, so the common acyclic-package struct doesn't carry a dead const.
	var rest bytes.Buffer
	renderDecodeBody(&rest, s)
	if strings.Contains(rest.String(), "depth+1") {
		b.WriteString("const depth = 0\n")
	}
	b.Write(rest.Bytes())
}

// renderDecodeBody emits everything after the DecodeFrom prologue
// (result = recv [; depth check]) — the alias delegation or the full
// object-scan loop, shared by both the cyclic and acyclic prologues above.
func renderDecodeBody(b *bytes.Buffer, s StructInfo) {
	if s.IsAlias {
		renderAliasDecode(b, s)
		b.WriteString("}\n\n")
		return
	}
	emitReceiverReset(b, s, true)
	if s.MultiErr {
		b.WriteString("var errs ggen.Errors\n")
	}

	// Per-field "seen" tracking: required-field post-loop checks + duplicate-key
	// guard. Narrow structs use per-field bools; wide structs (>threshold) a
	// packed bitmask to cut stack/cache pressure.
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
	// The dispatch key is transient (matched + discarded). Copy-mode retention
	// is handled where the key is stored: embedded fallback map + UnknownKeyError.
	// SIMD tier, all-short-keys struct: the vector classify's dependency chain
	// (~load+3 compares+movemask+tzcnt) loses to a ≤5-iteration predictable
	// scalar loop, so key scans get a bounded scalar window sized to the
	// longest declared key instead (unknown longer keys fall to the tier call).
	if maxKey := maxJSONNameLen(s); scanStringFn != "ggen.String" && maxKey <= 5 {
		inlineScanStringWin(b, "i", "key", "i", `""`, "ke", false, !s.AllowInvalidUTF8, maxKey+1)
	} else {
		// Dispatch keys are short (matched against known field names): scan
		// unbounded (window -1) so tiny structs don't pay per-key window setup.
		// On the SIMD tier this still routes to the inline vector classify.
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

// renderPostLoop emits end-of-parse bookkeeping: required-field checks
// (when validation is on) and the multierr flush (when MultiErr is on).
// Called at every success exit inside DecodeFrom / DecodeFromStream.
func renderPostLoop(b *bytes.Buffer, s StructInfo) {
	renderPostLoopShape(b, s, false)
}

// renderStreamPostLoop is the stream-path counterpart — same checks, but
// emits `return result, X` (no position arg, since Stream owns s.Pos).
func renderStreamPostLoop(b *bytes.Buffer, s StructInfo) {
	renderPostLoopShape(b, s, true)
}

func renderPostLoopShape(b *bytes.Buffer, s StructInfo, stream bool) {
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

// renderDispatch emits a flat string switch on key (the compiler lowers it to
// length-grouped binary search / jump tables).
func renderDispatch(b *bytes.Buffer, s StructInfo) {
	// emitField wraps per-field parse code with seen-tracking + dup handling.
	// The seen-branch differs by mode: AllowDups skips (first-wins), MultiErr
	// logs DuplicateKeyError and skips, default errors immediately.
	emitField := func(b *bytes.Buffer, f FieldInfo) {
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

// keyValidateAndMod emits mods + validation on the map key (keyRef) from the
// `keys:` tag bucket, bytes path (3-tuple return).
func keyValidateAndMod(b *bytes.Buffer, f FieldInfo, keyRef string) {
	if f.NoValidate {
		return
	}
	renderPipe(b, fieldKeyPipe(f), keyRef, f.JSONName+".key", "string", KindString, f.MultiErr, "i")
}

// keyValidateAndModStream is the stream-path counterpart of
// keyValidateAndMod. Emits 2-tuple `return result, err` shapes.
func keyValidateAndModStream(b *bytes.Buffer, f FieldInfo, keyRef string) {
	if f.NoValidate {
		return
	}
	renderPipe(b, fieldKeyPipe(f), keyRef, f.JSONName+".key", "string", KindString, f.MultiErr, "")
}

// renderMap emits map[string]V decode for the byte path. `null` → nil,
// empty `{}` → non-nil empty (stdlib parity). emitReceiverReset already
// cleared a non-nil map, so allocation only happens when ref is nil. Maps
// can't expose interior pointers (`&m[k]` illegal), so struct elems decode
// into a temp and assign.
// mapNest tracks how deeply renderMap/renderStreamMap are nested so each level
// names its own key/value locals. A map VALUE that is itself a map re-enters
// through renderField, and without the suffix the inner level shadowed `mk`/`mv`
// and its store resolved to `mv[mk]` on its own value — uncompilable output for
// map[string]map[string]V.
var mapNest int

// mapLocals returns this nesting level's key, value, carried-map and reuse-flag
// names. The outermost level keeps the bare names so the common single-level
// output is unchanged.
func mapLocals() (mk, mv, carried, reuse string) {
	suffix := ""
	if mapNest > 1 {
		suffix = strconv.Itoa(mapNest - 1)
	}
	return "mk" + suffix, "mv" + suffix, "carried" + suffix, "reuse" + suffix
}

func renderMap(b *bytes.Buffer, f FieldInfo, ref, posVar string, topLevel bool) {
	mapNest++
	defer func() { mapNest-- }()
	mkVar, mvVar, carriedVar, reuseVar := mapLocals()
	field := fieldLit(f)
	makeExpr := fmt.Sprintf("make(%s)", f.GoType)
	if cap := mapPreallocCap(f); cap > 0 {
		makeExpr = fmt.Sprintf("make(%s, %d)", f.GoType, cap)
	}
	// At dispatch level the null branch breaks to the comma handling rather
	// than nesting the object read in an else.
	flat := nullBreakOK(f)
	// No leading WS skip — the value-entry already skipped it (the alias path
	// adds an explicit skip before calling).
	inlineNullPeek(b, posVar)
	fmt.Fprintf(b, "%s = nil\n", ref)
	if flat {
		b.WriteString("break\n}\n")
	} else if topLevel {
		// Whole-value alias: null → return directly; no else (the non-null path
		// also returns at the object close).
		fmt.Fprintf(b, "return result, %s, nil\n}\n", posVar)
	} else {
		b.WriteString("} else {\n")
	}
	fmt.Fprintf(b, `if %[1]s >= len(data) || data[%[1]s] != '{' { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrBadObject) }
	%[1]s++
`, posVar, field)
	inlineSkipWS(b, posVar)
	// Allocate only when nil; a cleared map reuses its buckets. No `:`-count
	// prealloc for maps — a colon-laden key would inflate the count (footgun
	// on well-formed input, unlike the slice comma-count).
	if reusesMapValues(f) {
		// Read the carried map, fill a fresh one: each entry's previous value
		// becomes its decode target, and a key the payload omits is simply
		// never carried across — no seen-set needed to drop it. The map is
		// always freshly made here, so the nil guards below are unnecessary.
		size := "len(" + carriedVar + ")"
		if cap := mapPreallocCap(f); cap > 0 {
			size = fmt.Sprintf("max(len(%s), %d)", carriedVar, cap)
		}
		// `reuse` hoists the decision out of the entry loop: a fresh receiver
		// carries no map, and a per-entry lookup against a nil one is a real
		// runtime call, so the common zero-value decode must not pay it.
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
		// Pointer value (any depth): the parse-first cascade decoded into the
		// map slot. TargetNil keeps the emit assignment-only, so the
		// unaddressable index is fine.
		pf := elemPtrField(f, f.JSONName+".value")
		if reusesMapValues(f) {
			// Decode through an addressable local seeded from the carried map,
			// so the cascade reuses that entry's pointer chain; a map index is
			// not addressable, which is what TargetNil works around below.
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
			b.WriteString("}\n") // close else (null-check)
		}
		return
	}
	// Named-primitive VALUE (`map[string]Priority`): scan the underlying into a
	// temp and convert into the slot — see renderField.
	valCast, valTarget := "", ""
	savedElemType, savedElemKind := f.ElemType, f.ElemKind
	if prim, kind, ok := inlineNamedPrim(elemAsField(f)); ok {
		valCast, valTarget = f.ElemType, mapTarget
		mapTarget = "namedVal"
		fmt.Fprintf(b, "var %s %s\n", mapTarget, prim)
		f.ElemType, f.ElemKind = prim, kind
	}
	switch f.ElemKind {
	case KindString:
		// Straight into the slot; end-var `ve` since the key scan owns `ke`.
		inlineScanStringVar(b, posVar, mapTarget, posVar, field, "ve", f.Copy, !f.AllowInvalidUTF8)
	case KindBool:
		fmt.Fprintf(b, `%[2]s, %[1]s, err = ggen.Bool(data, %[1]s)
if err != nil { return result, %[1]s, ggen.NewParseErr(%[3]s, %[1]s, err) }
`, posVar, mapTarget, field)
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		castFn := ""
		if f.ElemType != "int64" {
			castFn = f.ElemType
		}
		inlineScanInt64(b, posVar, mapTarget, castFn, field)
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		castFn := ""
		if f.ElemType != "uint64" {
			castFn = f.ElemType
		}
		inlineScanUint64(b, posVar, mapTarget, castFn, field)
	case KindFloat32, KindFloat64:
		if f.ElemKind == KindFloat64 {
			fmt.Fprintf(b, `%[2]s, %[1]s, err = ggen.Float64(data, %[1]s)
if err != nil { return result, %[1]s, ggen.NewParseErr(%[3]s, %[1]s, err) }
`, posVar, mapTarget, field)
		} else {
			fmt.Fprintf(b, `var fv float64
fv, %[1]s, err = ggen.Float64(data, %[1]s)
if err != nil { return result, %[1]s, ggen.NewParseErr(%[3]s, %[1]s, err) }
%[4]s%[2]s = float32(fv)
`, posVar, mapTarget, field,
				narrowFloatGuard("fv", "float32", fmt.Sprintf("return result, %s, ggen.NewParseErr(%s, %s, ggen.ErrNumberOverflow)", posVar, field, posVar)))
		}
	case KindStruct:
		if isGenerated(f.ElemType) {
			// `mv` is the value-receiver source and stored value; `consumed`
			// (bytes consumed) advances posVar. When the carried map is being
			// read, seed from IT rather than the map being filled, so repeated
			// keys in one payload each decode fresh.
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
			// Cross-package value: run the ladder (its own DecodeFrom /
			// UnmarshalJSON / UnmarshalText, encoding/json only as the last
			// rung) instead of always reflecting over the captured span.
			fmt.Fprintf(b, "var %s %s\n", mvVar, f.ElemType)
			b.WriteString(renderCrossPkgStructDecode(elemAsField(f), mvVar, posVar))
			fmt.Fprintf(b, "%s = %s\n", mapTarget, mvVar)
		}
	default:
		// Dedicated-kind value (time/any/bytes/raw/slice/…): decode into a
		// fresh temp via the field-level emitter, then store. The old arm
		// skipped the span without storing — `mk` went unused, so the file
		// didn't even compile. Braced: the key scan owns `ke` in this scope
		// and the delegated emitters declare their own.
		vf := sliceElemField(f)
		vf.JSONName = f.JSONName + ".value"
		// A container value carries its own backing, so seed it from the
		// carried map and empty it — the delegated emitter fills it from
		// there, and only allocates when it came back nil.
		switch {
		case reusesMapValues(f) && (f.ElemKind == KindSlice || f.ElemKind == KindBytes):
			// []byte decodes with AppendDecode, so the seed must be emptied or
			// the decoded bytes land after the carried ones.
			fmt.Fprintf(b, "{\nvar %[1]s %[2]s\nif %[3]s { %[1]s = %[4]s[%[5]s][:0] }\n", mvVar, f.ElemType, reuseVar, carriedVar, mkVar)
		case reusesMapValues(f) && f.ElemKind == KindRawJSON:
			// renderRawJSON appends over mv[:0] itself under -copy.
			fmt.Fprintf(b, "{\nvar %[1]s %[2]s\nif %[3]s { %[1]s = %[4]s[%[5]s] }\n", mvVar, f.ElemType, reuseVar, carriedVar, mkVar)
		case reusesMapValues(f) && f.ElemKind == KindMap:
			fmt.Fprintf(b, "{\nvar %[1]s %[2]s\nif %[3]s { %[1]s = %[4]s[%[5]s]; clear(%[1]s) }\n", mvVar, f.ElemType, reuseVar, carriedVar, mkVar)
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
	// inner value steps (mods + validators in declared order).
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
		b.WriteString("}\n") // close else (null-check)
	}
}

// renderBytes emits bytes decode (base64/hex/array). JSON `null` → nil out
// the field (stdlib v1/v2 parity).
func renderBytes(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	if byteArrayLen(f) > 0 {
		// A fixed array is never nil, so it has no `null` form (same rule as
		// every other [N]T).
		renderBytesValue(b, f, ref, posVar)
		return
	}
	inlineNullPeek(b, posVar)
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

// emitEmptyBytesNonNil keeps an empty wire value ("" / []) decoding to an
// empty NON-nil slice on a nil receiver, like every other container — the
// decoders return nil for it (AppendDecode(nil, "") is nil; an immediate ']'
// appends nothing), which would re-marshal as null.
func emitEmptyBytesNonNil(b *bytes.Buffer, ref string) {
	fmt.Fprintf(b, "if %[1]s == nil { %[1]s = []byte{} }\n", ref)
}

func renderBytesValue(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	field := fieldLit(f)
	if f.Format == "array" {
		// `u` not `v`: a pointer-leaf caller declares `var v []byte` here.
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
	// AppendDecode skips the `[]byte(s)` copy DecodeString makes; pre-size
	// dst via DecodedLen so the single allocation is exact.
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
		enc = "" // hex doesn't share the Encoding API
	}
	if n := byteArrayLen(f); n > 0 {
		// Fixed array: decode into a same-sized scratch (a longer payload
		// reallocates past its cap and trips the length check) and require
		// EXACTLY N bytes, the array analogue of the tuple's strict count.
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
		// hex path. ref was pre-reset to [:0]; realloc only when cap is short.
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
func renderTime(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	field := fieldLit(f)
	layout, numeric := timeLayoutExpr(f.Format)
	if numeric != "" {
		// `format:unix` reads a (possibly fractional) number, split into
		// (sec, nsec) for round-trip safety. Other unix* are integer-granular.
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
	fmt.Fprintf(b, `%[1]s, err = time.Parse(%[2]s, s)
if err != nil { return result, %[3]s, ggen.NewParseErr(%[4]s, %[3]s, err) }
`, ref, layout, posVar, field)
}

// renderDuration emits time.Duration decode.
func renderDuration(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	field := fieldLit(f)
	switch f.Format {
	case "sec":
		// `f` not `v`: a pointer-leaf caller declares `var v time.Duration` here.
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

// renderNetIP / renderNetipAddr / renderNetipPrefix.
func renderNetIP(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	field := fieldLit(f)
	b.WriteString("var s string\n")
	inlineScanString(b, posVar, "s", posVar, field, false, !f.AllowInvalidUTF8)
	// net.ParseIP copies into a fresh IP, so the scan feed stays aliasing; the
	// error literal RETAINS the string, so under -copy it detaches (the stream
	// mirror clones for the same reason).
	text := "s"
	if f.Copy {
		text = "ggen.Detach(s, data)"
	}
	fmt.Fprintf(b, `%[1]s = net.ParseIP(s)
if %[1]s == nil { return result, %[2]s, ggen.NewParseErr(%[3]s, %[2]s, &net.ParseError{Type: "IP address", Text: %[4]s}) }
`, ref, posVar, field, text)
}

func renderNetipAddr(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	field := fieldLit(f)
	b.WriteString("var s string\n")
	inlineScanString(b, posVar, "s", posVar, field, false, !f.AllowInvalidUTF8)
	fmt.Fprintf(b, `%[1]s, err = netip.ParseAddr(s)
if err != nil { return result, %[2]s, ggen.NewParseErr(%[3]s, %[2]s, err) }
`, ref, posVar, field)
}

func renderNetipPrefix(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	field := fieldLit(f)
	b.WriteString("var s string\n")
	inlineScanString(b, posVar, "s", posVar, field, false, !f.AllowInvalidUTF8)
	fmt.Fprintf(b, `%[1]s, err = netip.ParsePrefix(s)
if err != nil { return result, %[2]s, ggen.NewParseErr(%[3]s, %[2]s, err) }
`, ref, posVar, field)
}

// elemAsField reshapes f so its ELEMENT looks like the field itself — the view
// the cross-package ladder needs when a slice/array/map element carries the
// foreign type. Element-level pipes stay behind; the caller emits those.
func elemAsField(f FieldInfo) FieldInfo {
	ef := f
	ef.GoType = f.ElemType
	ef.Kind = f.ElemKind
	ef.Iface = f.ElemIface
	ef.Pointer = false
	ef.PointeeType = ""
	ef.ElemType = ""
	ef.ElemKind = KindString
	return ef
}

// sliceElemField adapts elemAsField for FULL field-emitter delegation
// (dedicated-kind elements: time/duration/bytes/raw/netip/url/big/sqlnull/
// any/map). Element value rules run separately via elemSteps after the value
// decode, and the element sits inside a loop — never at dispatch — so the
// outer field's rule buckets and dispatch flags must not leak in. Map
// elements get their value shape back (elemAsField zeroes it).
func sliceElemField(f FieldInfo) FieldInfo {
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
	ef.ElemArrayLen = 0
	ef.NullDone = false
	switch ef.Kind {
	case KindMap:
		// Mirror the parse layer: stars stay on ElemType (pointer values
		// route through elemPtrType inside renderMap).
		ef.ElemType = strings.TrimPrefix(ef.GoType, "map[string]")
		ef.ElemKind = resolveKind(ef.ElemType)
	case KindSlice, KindArray:
		// Re-derive the container's own element shape (elemAsField zeroed
		// it); mirror peelSliceField's pointer peel.
		inner, kind, n := stripOneContainer(ef.GoType)
		if strings.HasPrefix(inner, "*") {
			ef.ElemPointer = true
			inner = inner[1:]
			kind = resolveKind(inner)
		}
		ef.ElemType, ef.ElemKind = inner, kind
		if ef.ElemKind == KindArray {
			ef.ElemArrayLen = arrayLenFromType(ef.ElemType)
		}
		if ef.Kind == KindArray {
			ef.ArrayLen = n
		}
	}
	return ef
}

// renderCrossPkgStructDecode emits the decode body for a cross-package /
// unannotated struct field. Resolved f.Iface → a static branch on the type's
// implemented interface (zero runtime probes); unresolved (AST-only) → the
// encoding/json fallback.
func renderCrossPkgStructDecode(f FieldInfo, ref, posVar string) string {
	if f.Iface.Resolved {
		switch {
		case f.Iface.ByteDecoder:
			// ggen-generated in another package — call DecodeFrom directly.
			return fmt.Sprintf(`var consumed int
%[1]s, consumed, err = %[1]s.DecodeFrom(data[%[2]s:])
%[2]s += consumed
%[3]s`, ref, posVar, nestedDecodeErrCheck(fieldLit(f), calleeTypeOf(f), f.MultiErr, true, "consumed"))

		case f.Iface.JSONUnmarshaler:
			chk := bytesErrCheck(fieldLit(f), posVar)
			return fmt.Sprintf(`start := %[1]s
%[1]s, err = ggen.SkipValue(data, start)
%[3]serr = %[2]s.UnmarshalJSON(data[start:%[1]s])
%[3]s`, posVar, ref, chk)

		case f.Iface.TextUnmarshaler:
			chk := bytesErrCheck(fieldLit(f), posVar)
			return fmt.Sprintf(`var ts string
ts, %[1]s, err = `+scanStringFn+`(data, %[1]s, `+vArg(f)+`)
%[3]serr = %[2]s.UnmarshalText(unsafe.Slice(unsafe.StringData(ts), len(ts)))
%[3]s`, posVar, ref, chk)

		default:
			chk := bytesErrCheck(fieldLit(f), posVar)
			return fmt.Sprintf(`start := %[1]s
%[1]s, err = ggen.SkipValue(data, start)
%[3]serr = json.Unmarshal(data[start:%[1]s], &%[2]s)
%[3]s`, posVar, ref, chk)
		}
	}
	chk := bytesErrCheck(fieldLit(f), posVar)
	return fmt.Sprintf(`start := %[1]s
%[1]s, err = ggen.SkipValue(data, start)
%[3]serr = json.Unmarshal(data[start:%[1]s], &%[2]s)
%[3]s`, posVar, ref, chk)
}

// renderCrossPkgStructAppend is the marshal counterpart for cross-package
// struct fields. Resolved → static branch on what the type implements.
// Unresolved → runtime cascade.
func renderCrossPkgStructAppend(f FieldInfo, ref string) string {
	// Field-suffixed temp — fields emit at function scope, so two cross-pkg
	// fields would collide on a shared name.
	tmp := "b" + strings.ReplaceAll(f.GoName, ".", "")
	if f.Iface.Resolved {
		switch {
		case f.Iface.AppendJSON:
			return fmt.Sprintf("if dst, err = %s.AppendJSON(dst); err != nil { return dst, err }\n", ref)

		case f.Iface.JSONMarshaler:
			// Empty result errors (stdlib parity): a bare append would emit
			// `"k":` with a nil error.
			return fmt.Sprintf(`var %[1]s []byte
%[1]s, err = %[2]s.MarshalJSON()
if err != nil { return dst, err }
if len(%[1]s) == 0 { return dst, ggen.ErrEmptyMarshalJSON }
dst = append(dst, %[1]s...)
`, tmp, ref)

		case f.Iface.TextAppender:
			// AppendText (Go 1.24+) preferred over MarshalText — no alloc.
			// Close via the escape checker: the text may carry `"`/`\`/ctrl.
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
	// Unresolved (AST-only) — plain encoding/json fallback.
	return fmt.Sprintf(`var %[1]s []byte
%[1]s, err = json.Marshal(%[2]s)
if err != nil { return dst, err }
dst = append(dst, %[1]s...)
`, tmp, ref)
}

// renderCrossPkgStructStreamDecode is the streaming counterpart of
// renderCrossPkgStructDecode. Same static-vs-runtime branching.
func renderCrossPkgStructStreamDecode(f FieldInfo, ref, posVar string) string {
	_ = posVar
	chk := streamErrCheck(fieldLit(f))
	if f.Iface.Resolved {
		switch {
		case f.Iface.StreamDecoder:
			return fmt.Sprintf(`%[1]s, err = %[1]s.DecodeFromStream(s)
%[2]s`, ref, nestedDecodeErrCheck(fieldLit(f), calleeTypeOf(f), f.MultiErr, false, ""))

		case f.Iface.JSONUnmarshaler:
			return fmt.Sprintf(`span, err := s.CaptureValue()
%[2]serr = %[1]s.UnmarshalJSON(span)
%[2]s`, ref, chk)

		case f.Iface.TextUnmarshaler:
			// StringView: UnmarshalText must not retain its arg (encoding
			// contract), and the bytes path already passes an aliased slice
			// here — see Stream.StringView.
			return fmt.Sprintf(`var ts string
ts, err = s.StringView(`+vArg(f)+`)
%[2]serr = %[1]s.UnmarshalText(unsafe.Slice(unsafe.StringData(ts), len(ts)))
%[2]s`, ref, chk)

		default:
			return fmt.Sprintf(`span, err := s.CaptureValue()
%[2]serr = json.Unmarshal(span, &%[1]s)
%[2]s`, ref, chk)
		}
	}
	// Unresolved (AST-only) — plain encoding/json fallback.
	return fmt.Sprintf(`span, err := s.CaptureValue()
%[2]serr = json.Unmarshal(span, &%[1]s)
%[2]s`, ref, chk)
}

// renderRawJSON captures the value's raw span. Default aliases data[start:end]
// into the field — zero copy. Under -copy it copies the span into the field's
// own (reused) backing so the value survives a later mutation of data. Works
// for json.RawMessage and jsontext.Value (both underlying []byte).
func renderRawJSON(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	chk := bytesErrCheck(fieldLit(f), posVar)
	span := "data[start:%[1]s]"
	if f.Copy {
		// append over the existing backing (reused across decodes; nil → fresh).
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

// renderURL parses a JSON string via url.Parse. The dereference is
// safe because Parse returns a non-nil *URL on success. The scan honours
// f.Copy: url.Parse SLICES its input into Host/Path/RawQuery/Fragment, so an
// aliasing scan would leave the stored URL pointing at the caller's buffer.
func renderURL(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	field := fieldLit(f)
	b.WriteString("var s string\n")
	inlineScanString(b, posVar, "s", posVar, field, f.Copy, !f.AllowInvalidUTF8)
	fmt.Fprintf(b, `var u *url.URL
u, err = url.Parse(s)
if err != nil { return result, %[2]s, ggen.NewParseErr(%[3]s, %[2]s, err) }
%[1]s = *u
`, ref, posVar, field)
}

// renderBigInt reads a bare JSON number into big.Int.SetString (no overflow,
// arbitrary length). The literal is aliased via unsafe.String; SetString
// copies the digits into its own storage, so the alias is short-lived.
func renderBigInt(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	field := fieldLit(f)
	fmt.Fprintf(b, `start := %[1]s
%[1]s, err = ggen.SkipValue(data, start)
if err != nil { return result, %[1]s, ggen.NewParseErr(%[3]s, %[1]s, err) }
if _, ok := (&%[2]s).SetString(unsafe.String(unsafe.SliceData(data[start:]), %[1]s-start), 10); !ok {
	return result, %[1]s, ggen.NewParseErr(%[3]s, %[1]s, ggen.ErrBadNumber)
}
`, posVar, ref, field)
}

// renderBigFloat reads a JSON-string-wrapped numeric literal into big.Float
// at the default precision. Wrapping matches jsonv2's wire format for
// big.Float; bare numbers are not accepted (use big.Int or float64 for those).
func renderBigFloat(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	field := fieldLit(f)
	b.WriteString("var s string\n")
	inlineScanString(b, posVar, "s", posVar, field, false, !f.AllowInvalidUTF8)
	fmt.Fprintf(b, `if _, _, err := (&%[1]s).Parse(s, 10); err != nil {
	return result, %[2]s, ggen.NewParseErr(%[3]s, %[2]s, err)
}
`, ref, posVar, field)
}

// renderBigRat reads a JSON string of the form "num" or "num/denom"
// and feeds it to big.Rat.SetString. Lossless — fractions stay exact.
func renderBigRat(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	field := fieldLit(f)
	b.WriteString("var s string\n")
	inlineScanString(b, posVar, "s", posVar, field, false, !f.AllowInvalidUTF8)
	fmt.Fprintf(b, `if _, ok := (&%[1]s).SetString(s); !ok {
	return result, %[2]s, ggen.NewParseErr(%[3]s, %[2]s, ggen.ErrBadNumber)
}
`, ref, posVar, field)
}

// sqlNullInnerField returns a render-ready copy of a generic sql.Null[T]'s
// inner FieldInfo — the synthetic V field with the parent's struct flags
// applied (the inner is built at parse time, before flag propagation).
func sqlNullInnerField(f FieldInfo) FieldInfo {
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

// renderSQLNull emits decode for a database/sql.NullX field: probe `null`
// first, else parse the inner value and set Valid=true.
func renderSQLNull(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	if f.SQLNullInner != nil {
		// Generic sql.Null[T]: null → zero; else decode V into a temp and
		// publish the {V, Valid:true} literal.
		inner := sqlNullInnerField(f)
		var dec bytes.Buffer
		renderField(&dec, inner, "nv", posVar)
		body, valExpr := dec.String(), "nv"
		if rest, expr, ok := foldTailAssign(body, "nv"); ok {
			body, valExpr = rest, expr
		} else {
			body = "var nv " + inner.GoType + "\n" + body
		}
		fmt.Fprintf(b, `if %[1]s+4 <= len(data) && data[%[1]s] == 'n' && data[%[1]s+1] == 'u' && data[%[1]s+2] == 'l' && data[%[1]s+3] == 'l' {
	%[2]s = sql.%[3]s{}
	%[1]s += 4
} else {
	%[4]s
	%[2]s = sql.%[3]s{V: %[5]s, Valid: true}
}
`, posVar, ref, sqlTypeName(f.GoType), body, valExpr)
		return
	}
	spec, ok := SQLNullSpec(f.GoType)
	if !ok {
		return
	}
	field := fieldLit(f)
	var inner bytes.Buffer
	valExpr := "nv"
	declType := spec.Type
	switch spec.Inner {
	case KindString:
		declType = "string"
		inlineScanString(&inner, posVar, "nv", posVar, field, f.Copy, !f.AllowInvalidUTF8)
	case KindBool:
		declType = "bool"
		fmt.Fprintf(&inner, "nv, %[1]s, err = ggen.Bool(data, %[1]s)\n", posVar)
		fmt.Fprintf(&inner, "if err != nil { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, err) }\n", posVar, field)
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		cast := spec.Type
		if spec.Type == "int64" {
			cast = ""
		}
		inlineScanInt64(&inner, posVar, "nv", cast, field)
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		cast := spec.Type
		if spec.Type == "uint64" {
			cast = ""
		}
		inlineScanUint64(&inner, posVar, "nv", cast, field)
	case KindFloat32, KindFloat64:
		declType = "float64"
		fmt.Fprintf(&inner, "nv, %[1]s, err = ggen.Float64(data, %[1]s)\n", posVar)
		fmt.Fprintf(&inner, "if err != nil { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, err) }\n", posVar, field)
		if spec.Type != "float64" {
			valExpr = "float32(nv)"
		}
	case KindTime:
		tf := FieldInfo{JSONName: f.JSONName, Format: f.Format,
			Copy: f.Copy, AllowInvalidUTF8: f.AllowInvalidUTF8, MultiErr: f.MultiErr}
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
	fmt.Fprintf(b, `if %s+4 <= len(data) && data[%s] == 'n' && data[%s+1] == 'u' && data[%s+2] == 'l' && data[%s+3] == 'l' {
	%s = sql.%s{}
	%s += 4
} else {
	%s
	%s = sql.%s{%s: %s, Valid: true}
}
`, posVar, posVar, posVar, posVar, posVar,
		ref, sqlTypeName(f.GoType), posVar,
		body,
		ref, sqlTypeName(f.GoType), spec.Field, valExpr)
}

// sqlTypeName returns the bare type name from a `sql.NullX` qualified name.
func sqlTypeName(goType string) string {
	if _, after, ok := strings.Cut(goType, "."); ok {
		return after
	}
	return goType
}

// renderAny decodes into a reflective any via ggen.Any / ggen.AnyNumber. Under
// -copy the Copy variants clone every nested string (values + object keys) so
// the tree survives a later mutation of data.
func renderAny(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	fn := "ggen.Any"
	if f.UseNumber {
		fn = "ggen.AnyNumber"
	}
	if f.Copy {
		fn += "Copy"
	}
	chk := bytesErrCheck(fieldLit(f), posVar)
	fmt.Fprintf(b, `%[1]s, %[2]s, err = %[3]s(data, %[2]s)
%[4]s`, ref, posVar, fn, chk)
}

// needsSeen reports whether a seen<Field> flag is required for f (always
// except embedded fallback maps). Used for dup-key guard / first-wins skip /
// required-field post-loop check.
func needsSeen(f FieldInfo) bool {
	return !f.Embed
}

// seenBitmaskThreshold is the field count above which codegen swaps per-field
// `bool` locals for a packed bitmask (cheaper stack/cache on wide structs).
const seenBitmaskThreshold = 32

// useSeenBitmask reports whether struct s should use the packed-bitmask
// shape for its seen-tracking instead of per-field bools.
func useSeenBitmask(s StructInfo) bool {
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

// seenBitIndex assigns a stable bit index to f from its position in s.Fields.
// Embedded fallback fields still occupy an index (simpler addressing).
func seenBitIndex(s StructInfo, f FieldInfo) int {
	for i, ff := range s.Fields {
		if ff.GoName == f.GoName {
			return i
		}
	}
	return -1
}

func seenWordCount(s StructInfo) int {
	return (len(s.Fields) + 63) / 64
}

// seenDecl emits the declaration line(s) for the bitmask, or the empty
// string when bools-mode is in use (caller emits per-field bools).
func seenDecl(s StructInfo) string {
	if !useSeenBitmask(s) {
		return ""
	}
	if seenWordCount(s) == 1 {
		return "var seen uint64\n"
	}
	return fmt.Sprintf("var seen [%d]uint64\n", seenWordCount(s))
}

// seenAccess returns the read expression for f's seen bit (bool local or
// bitmask test).
func seenAccess(s StructInfo, f FieldInfo) string {
	if !useSeenBitmask(s) {
		return "seen" + f.GoName
	}
	bit := seenBitIndex(s, f)
	if seenWordCount(s) == 1 {
		return fmt.Sprintf("seen&(1<<%d) != 0", bit)
	}
	return fmt.Sprintf("seen[%d]&(1<<%d) != 0", bit/64, bit%64)
}

// seenNotAccess is the negated counterpart of seenAccess — true when the
// bit is unset.
func seenNotAccess(s StructInfo, f FieldInfo) string {
	if !useSeenBitmask(s) {
		return "!seen" + f.GoName
	}
	bit := seenBitIndex(s, f)
	if seenWordCount(s) == 1 {
		return fmt.Sprintf("seen&(1<<%d) == 0", bit)
	}
	return fmt.Sprintf("seen[%d]&(1<<%d) == 0", bit/64, bit%64)
}

// seenSet emits a statement that marks f's seen bit. Trailing newline
// is intentional so callers can concatenate freely.
func seenSet(s StructInfo, f FieldInfo) string {
	if !useSeenBitmask(s) {
		return "seen" + f.GoName + " = true\n"
	}
	bit := seenBitIndex(s, f)
	if seenWordCount(s) == 1 {
		return fmt.Sprintf("seen |= 1<<%d\n", bit)
	}
	return fmt.Sprintf("seen[%d] |= 1<<%d\n", bit/64, bit%64)
}

// unknownKey emits the default branch of the key switch, in precedence order:
// embedded fallback map → absorb; s.IgnoreUnknown → SkipValue; else
// UnknownKeyError. The bytes-path `key` aliases data and stays valid for the
// whole function, so by default it embeds directly into the stored map key /
// ggen.NewParseErr (no clone). Under -copy every retained `key` is cloned
// (keyExpr) and inline-map string/any VALUES are copied too, so the absorbed
// entries survive a later mutation of data — matching the stream path.
func unknownKey(s StructInfo, posVar string) string {
	keyExpr := "key"
	if s.Copy {
		keyExpr = "strings.Clone(key)"
	}
	chk := bytesErrCheck(keyExpr, posVar)
	if embedded := s.EmbedField(); embedded.Embed {
		initMap := fmt.Sprintf("if result.%s == nil { result.%s = make(%s) }\n",
			embedded.GoName, embedded.GoName, embedded.GoType)
		switch embedded.ElemKind {
		case KindAny:
			anyFn := "ggen.Any"
			if s.UseNumber {
				anyFn = "ggen.AnyNumber"
			}
			if s.Copy {
				anyFn += "Copy"
			}
			return initMap + fmt.Sprintf(`result.%[1]s[%[5]s], %[2]s, err = %[3]s(data, %[2]s)
%[4]s`, embedded.GoName, posVar, anyFn, chk, keyExpr)
		case KindString:
			if s.Copy {
				// tier func + ggen.Detach: reuse the tier locate, clone the value
				// only when it aliases data (an owned stringSlow escape result
				// skips the clone — no double-copy). Key stays cloned (keyExpr).
				return initMap + fmt.Sprintf(`var strVal string
strVal, %[1]s, err = `+scanStringFn+`(data, %[1]s, `+vArg(embedded)+`)
%[3]sresult.%[2]s[%[4]s] = ggen.Detach(strVal, data)
`, posVar, embedded.GoName, chk, keyExpr)
			}
			return initMap + fmt.Sprintf(`result.%[2]s[key], %[1]s, err = `+scanStringFn+`(data, %[1]s, `+vArg(embedded)+`)
%[3]s`, posVar, embedded.GoName, chk)
		case KindStruct:
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

// renderPipe emits an ordered value-stage step list (mods and validators
// interleaved in declared order) against ref. posVar selects the return shape
// ("i" bytes path, "" stream path).
func renderPipe(b *bytes.Buffer, steps []Step, ref, jsonName, goType string, kind TypeKind, multiErr bool, posVar string) {
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
		// maximal run of validators against the current (unmutated) ref
		q := p
		run := make([]ValidationRule, 0, len(steps)-p)
		for q < len(steps) && !steps[q].IsMod {
			run = append(run, steps[q].V)
			q++
		}
		emitValRun(b, run, ref, jsonName, goType, kind, multiErr, onErr)
		p = q
	}
}

// fieldPipe returns the ordered value steps for a field — f.Pipe when set,
// else derived from the legacy split buckets (covers synthetic pointer-leaf fields).
func fieldPipe(f FieldInfo) []Step {
	if f.Pipe != nil {
		return f.Pipe
	}
	return stepsFromLegacy(f.Mods, f.Validation)
}

func fieldKeyPipe(f FieldInfo) []Step {
	if f.KeyPipe != nil {
		return f.KeyPipe
	}
	return stepsFromLegacy(f.KeyMods, f.KeyValidation)
}

func validateAndMod(b *bytes.Buffer, f FieldInfo, ref string) {
	renderPipe(b, fieldPipe(f), ref, f.JSONName, f.GoType, f.Kind, f.MultiErr, "i")
}

// validateAndModStream is the stream-path counterpart of validateAndMod.
// Emits 2-tuple `return result, err` shape (Stream owns s.Pos).
func validateAndModStream(b *bytes.Buffer, f FieldInfo, ref string) {
	renderPipe(b, fieldPipe(f), ref, f.JSONName, f.GoType, f.Kind, f.MultiErr, "")
}

// renderStringTag handles json:",string" — reads a quoted JSON string and
// parses its contents via strconv into the field's numeric / bool type.
func renderStringTag(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	field := fieldLit(f)
	// Brace-less: `sv` / `u` / `n` / `f` land in the caller's scope. The `:=`
	// parse shadows the function-level err; checked immediately.
	b.WriteString("var sv string\n")
	// Only the KindString case retains sv (ref = sv); numeric `,string` parses
	// it into an independent value, so the copy is needed for strings only.
	inlineScanString(b, posVar, "sv", posVar, field, f.Copy && f.Kind == KindString, !f.AllowInvalidUTF8)
	errCheck := fmt.Sprintf("if err != nil { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, err) }\n", posVar, field)
	switch f.Kind {
	case KindBool:
		fmt.Fprintf(b, `switch sv {
case "true": %[1]s = true
case "false": %[1]s = false
default: return result, %[2]s, ggen.NewParseErr(%[3]s, %[2]s, ggen.ErrBadBool)
}
`, ref, posVar, field)
	case KindFloat32, KindFloat64:
		b.WriteString("f, err := strconv.ParseFloat(sv, 64)\n")
		b.WriteString(errCheck)
		if f.Kind == KindFloat32 {
			b.WriteString(narrowFloatGuard("f", "float32",
				fmt.Sprintf("return result, %s, ggen.NewParseErr(%s, %s, ggen.ErrNumberOverflow)", posVar, field, posVar)))
			fmt.Fprintf(b, "%s = float32(f)\n", ref)
		} else {
			fmt.Fprintf(b, "%s = f\n", ref)
		}
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		b.WriteString("u, err := strconv.ParseUint(sv, 10, 64)\n")
		b.WriteString(errCheck)
		if f.Kind == KindUint64 {
			fmt.Fprintf(b, "%s = u\n", ref)
		} else {
			b.WriteString(narrowIntGuard("u", kindNarrowName(f.Kind),
				fmt.Sprintf("return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrNumberOverflow)", posVar, field)))
			fmt.Fprintf(b, "%s = %s(u)\n", ref, f.GoType)
		}
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		b.WriteString("n, err := strconv.ParseInt(sv, 10, 64)\n")
		b.WriteString(errCheck)
		if f.Kind == KindInt64 {
			fmt.Fprintf(b, "%s = n\n", ref)
		} else {
			b.WriteString(narrowIntGuard("n", kindNarrowName(f.Kind),
				fmt.Sprintf("return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrNumberOverflow)", posVar, field)))
			fmt.Fprintf(b, "%s = %s(n)\n", ref, f.GoType)
		}
	}
}

// pointerDepth counts the leading `*` of a pointer field's GoType and
// returns (depth, leafType). depth==1 is a plain `*T`; depth>=2 is a
// multi-level pointer (`**T`, `***T`, …) whose leaf is the type left after
// stripping every `*`.
func pointerDepth(goType string) (int, string) {
	depth := 0
	for len(goType) > 0 && goType[0] == '*' {
		depth++
		goType = goType[1:]
	}
	return depth, goType
}

// elemPtrType returns the full element type including every pointer level,
// plus the total pointer depth (0 = not a pointer). The parse layer unwraps
// one `*` into ElemPointer for slices/arrays; map/nested keep their stars on ElemType.
func elemPtrType(f FieldInfo) (string, int) {
	et := f.ElemType
	if f.ElemPointer {
		et = "*" + et
	}
	depth, _ := pointerDepth(et)
	return et, depth
}

// elemPtrField synthesizes the FieldInfo a pointer-typed element decodes
// through — the parse-first cascade scalar pointer fields use. Elem-level
// rules ride along. jsonName is the validation-path suffix ("x[]" / "x.value").
func elemPtrField(f FieldInfo, jsonName string) FieldInfo {
	et, _ := elemPtrType(f)
	_, leafType := pointerDepth(et)
	pf := FieldInfo{
		GoType:           et,
		Pointer:          true,
		PointeeType:      et[1:],
		Kind:             resolveKind(leafType),
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
	// A container leaf needs its own Elem* populated — the parse layer only
	// fills these for the outermost container.
	switch pf.Kind {
	case KindSlice, KindArray:
		pf.ArrayLen = arrayLenFromType(leafType)
		elem, kind, _ := stripOneContainer(leafType)
		if strings.HasPrefix(elem, "*") {
			pf.ElemPointer = true
			elem = elem[1:]
			kind = resolveKind(elem)
		}
		pf.ElemType, pf.ElemKind = elem, kind
		if kind == KindArray {
			pf.ElemArrayLen = arrayLenFromType(elem)
		}
	case KindMap:
		pf.ElemType = strings.TrimPrefix(leafType, "map[string]")
		pf.ElemKind = resolveKind(pf.ElemType)
	}
	return pf
}

// derefStr returns ref dereferenced k times, parenthesized:
// derefStr("x",0)=="x", derefStr("x",1)=="(*x)", derefStr("x",2)=="(*(*x))".
func derefStr(ref string, k int) string {
	for ; k > 0; k-- {
		ref = "(*" + ref + ")"
	}
	return ref
}

// newChain returns `new(new(…expr))` with n `new(` calls — wraps expr in n
// pointer levels via the Go 1.26 `new(expr)` form. n==1 → `new(expr)`.
func newChain(expr string, n int) string {
	return strings.Repeat("new(", n) + expr + strings.Repeat(")", n)
}

// widenedLeafCast maps a narrow numeric leaf kind to the wide type its scanner
// produces (int64/uint64/float64) plus the cast back. The pointer decode scans
// into a wide temp and casts at the assign site. Returns ("",0,"") when no
// widening is needed.
func widenedLeafCast(k TypeKind, leafGoType string) (wideType string, wideKind TypeKind, cast string) {
	switch k {
	case KindInt, KindInt8, KindInt16, KindInt32:
		return "int64", KindInt64, leafGoType
	case KindUint, KindUint8, KindUint16, KindUint32:
		return "uint64", KindUint64, leafGoType
	case KindFloat32:
		return "float64", KindFloat64, leafGoType
	}
	return "", 0, ""
}

// leafMerges reports whether a pointer leaf benefits from being seeded with
// the receiver's carried-in value before decode (struct/slice/map merge;
// primitives just overwrite).
func leafMerges(k TypeKind) bool {
	switch k {
	case KindStruct, KindSlice, KindArray, KindMap:
		return true
	}
	return false
}

// nullBreakOK reports whether a field's `null` branch can `break` out of the
// key-dispatch switch instead of nesting the whole value decode in an else.
// Requires the emit site to sit directly inside the dispatch switch
// (AtDispatch) and no post-value field rules — a break would skip the
// validateAndMod / custom @Func calls that follow the value block.
func nullBreakOK(f FieldInfo) bool {
	return f.AtDispatch && (f.NoValidate || (len(f.Validation) == 0 && len(f.Mods) == 0))
}

// nullZeroApplies reports whether the `nullzero` opt-in adds a null→zero
// branch. Only dispatch-level non-pointer value kinds that would otherwise
// hard-error on `null` qualify; already-null-aware kinds are no-ops.
func nullZeroApplies(f FieldInfo) bool {
	if !f.NullZero || f.Pointer || !f.AtDispatch {
		return false
	}
	switch f.Kind {
	case KindSlice, KindMap, KindBytes, KindRawJSON, KindSQLNull, KindAny:
		return false
	}
	return true
}

// emitStreamNullZero is the stream-path counterpart of the bytes-path
// inlineNullPeek + zero assign used for `nullzero`. It emits the buffered
// `null` literal check (ReadMore-guarded, like the stream pointer branch),
// sets ref to its Go zero on a match, then breaks (flat) or opens an else.
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

// emitPointerSeed copies the receiver's existing leaf into the decode temp
// `v` when the whole pointer chain is already allocated — so a mergeable leaf
// merges against its carried-in value. No-op for non-mergeable leaves.
func emitPointerSeed(b *bytes.Buffer, ref string, depth int, leafKind TypeKind) {
	if !leafMerges(leafKind) {
		return
	}
	conds := make([]string, depth)
	for k := range depth {
		conds[k] = derefStr(ref, k) + " != nil"
	}
	fmt.Fprintf(b, "if %s {\nv = %s\n}\n", strings.Join(conds, " && "), derefStr(ref, depth))
}

// emitPointerAssign writes the post-parse assign cascade for a depth-level
// pointer field (leaf already decoded into `v`). REUSES the non-nil prefix of
// the chain, allocating `new(…)` only from the first nil level down. valExpr is
// `v` or a width cast like `int(v)`.
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

func renderField(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	// Converter variant: peek the JSON shape and route (handles null/native/
	// convert itself); the outer value stage runs after.
	if fieldHasConverter(f) {
		renderVariantDispatch(b, f, ref, posVar)
		if !f.NoValidate {
			validateAndMod(b, f, ref)
		}
		return
	}
	// Named primitive (`type B S; type S string`): scan the UNDERLYING into a
	// temp and convert at the assign. Delegating to the alias's own DecodeFrom
	// costs a `ggen.String` call per field — it forfeits the inline window scan
	// (opt #47) — and an UNANNOTATED named type had no methods to call at all,
	// so it fell through to SkipValue + encoding/json. The conversion itself is
	// free: identical underlying type, so gc emits no instruction for it.
	// Runs AFTER the pointer peel, so `*Named` reaches it via the leaf recursion.
	if prim, kind, ok := inlineNamedPrim(f); ok && !f.Pointer && !f.String {
		// `nullzero` stays on the OUTER field: it is gated on AtDispatch, which
		// only the outer carries, and its zero literal is the named type's.
		nz := nullZeroApplies(f)
		flat := nz && nullBreakOK(f)
		if nz {
			inlineNullPeek(b, posVar)
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
			// Write-once temp folds straight into the conversion.
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
	// Pointer FIRST: a `*T` field with json:",string" must decode into T then
	// take its address (the string-tag branch on `*T` would emit broken code).
	// The pointer block recurses with inner.GoType=T.
	if f.Pointer {
		// Custom `@Func` rules apply to the pointer type; built-in rules to the
		// deref'd leaf. Partition here; run @-rules on `ref` after the if/else.
		builtinV, customV := partitionCustomValidation(f.Validation)
		builtinM, customM := partitionCustomMods(f.Mods)
		// Decode-into-receiver, any pointer depth. null → nil outer (stdlib
		// merge parity). Else decode the leaf into temp `v` FIRST (no mutation
		// on parse failure), then the assign cascade reuses the non-nil prefix
		// and allocates only the nil tail; a mergeable leaf is seeded first.
		depth, leafType := pointerDepth(f.GoType)
		leaf := f
		leaf.Pointer = false
		leaf.PointeeType = ""
		leaf.GoType = leafType
		leaf.Kind = resolveKind(leafType)
		leaf.Validation = builtinV
		leaf.Mods = builtinM
		leaf.Pipe = nil // partitioned buckets are the source
		leaf.AtDispatch = false
		leaf.TargetNil = false
		// Widened numeric leaf: scan into the wide type, cast at the assign site.
		scanType, castFn := leafType, ""
		if wide, wideKind, cast := widenedLeafCast(leaf.Kind, leafType); wide != "" {
			leaf.Kind, leaf.GoType, scanType, castFn = wideKind, wide, wide, cast
		}
		// At dispatch level the null branch breaks to the comma handling.
		flat := nullBreakOK(f)
		inlineNullPeek(b, posVar)
		fmt.Fprintf(b, "%s = nil\n", ref)
		if flat {
			b.WriteString("break\n}\n")
		} else {
			b.WriteString("} else {\n")
		}
		// Fast path: the inline int/uint scanner materializes `n`, so a leaf
		// with no built-in rules skips the temp — cascade runs off `n`.
		if (leaf.Kind == KindInt64 || leaf.Kind == KindUint64) && !f.String && len(builtinV) == 0 && len(builtinM) == 0 {
			valExpr := "n"
			if castFn != "" {
				valExpr = castFn + "(n)"
			}
			var cascade bytes.Buffer
			// Guard the narrowing cast: an out-of-range leaf overflows, not wraps.
			errRet := fmt.Sprintf("return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrNumberOverflow)", posVar, fieldLit(f))
			cascade.WriteString(narrowIntGuard("n", castFn, errRet))
			if f.TargetNil {
				fmt.Fprintf(&cascade, "%s = %s\n", ref, newChain(valExpr, depth))
			} else {
				emitPointerAssign(&cascade, ref, depth, valExpr)
			}
			stmt := strings.TrimRight(cascade.String(), "\n")
			if leaf.Kind == KindInt64 {
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
				emitPointerSeed(b, ref, depth, leaf.Kind)
			}
			renderField(b, leaf, "v", posVar)
			b.WriteString(narrowIntGuard("v", castFn, fmt.Sprintf("return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrNumberOverflow)", posVar, fieldLit(f))))
			b.WriteString(narrowFloatGuard("v", castFn, fmt.Sprintf("return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrNumberOverflow)", posVar, fieldLit(f))))
			if f.TargetNil {
				// Target is a known-nil `var x *T` — straight new-chain assign.
				fmt.Fprintf(b, "%s = %s\n", ref, newChain(valExpr, depth))
			} else {
				emitPointerAssign(b, ref, depth, valExpr)
			}
		}
		if !flat {
			b.WriteString("}\n")
		}
		if !f.NoValidate && (len(customV) > 0 || len(customM) > 0) {
			outer := f
			outer.Validation = customV
			outer.Mods = customM
			outer.Pipe = nil // partitioned customs are the source
			validateAndMod(b, outer, ref)
		}
		return
	}
	// `nullzero`: accept an explicit JSON null → Go zero on a non-pointer value
	// field. Flat-break at dispatch level; else nest in an else so the shared
	// validateAndMod runs on the zero too.
	nz := nullZeroApplies(f)
	flat := nz && nullBreakOK(f)
	if nz {
		inlineNullPeek(b, posVar)
		fmt.Fprintf(b, "%s = %s\n", ref, zeroLit(f.GoType, f.Kind))
		if flat {
			b.WriteString("break\n}\n")
		} else {
			b.WriteString("} else {\n")
		}
	}
	// jsonv2 dropped `,string` for bool; fall through to the bare decode.
	if f.String && f.Kind != KindBool {
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
	case KindString:
		inlineScanString(b, posVar, ref, posVar, field, f.Copy, !f.AllowInvalidUTF8)
	case KindBool:
		fmt.Fprintf(b, "%[1]s, %[2]s, err = ggen.Bool(data, %[2]s)\n", ref, posVar)
		fmt.Fprintf(b, "if err != nil { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, err) }\n", posVar, field)
	case KindInt, KindInt8, KindInt16, KindInt32:
		inlineScanInt64(b, posVar, ref, f.GoType, field)
	case KindInt64:
		inlineScanInt64(b, posVar, ref, "", field)
	case KindUint, KindUint8, KindUint16, KindUint32:
		inlineScanUint64(b, posVar, ref, f.GoType, field)
	case KindUint64:
		inlineScanUint64(b, posVar, ref, "", field)
	case KindFloat32:
		fmt.Fprintf(b, `var fv float64
fv, %[1]s, err = ggen.Float64(data, %[1]s)
if err != nil { return result, %[1]s, ggen.NewParseErr(%[3]s, %[1]s, err) }
%[4]s%[2]s = float32(fv)
`, posVar, ref, field,
			narrowFloatGuard("fv", "float32", fmt.Sprintf("return result, %s, ggen.NewParseErr(%s, %s, ggen.ErrNumberOverflow)", posVar, field, posVar)))
	case KindFloat64:
		fmt.Fprintf(b, `%[1]s, %[2]s, err = ggen.Float64(data, %[2]s)
if err != nil { return result, %[2]s, ggen.NewParseErr(%[3]s, %[2]s, err) }
`, ref, posVar, field)
	case KindStruct:
		if isGenerated(f.GoType) {
			// Value-receiver DecodeFrom merges the field's current value;
			// `consumed` (bytes consumed) advances posVar. Cyclic types route to the
			// depth core so payload nesting stays bounded.
			fmt.Fprintf(b, `var consumed int
%[1]s, consumed, err = %[1]s.`+decodeCallFor(f.GoType)+`
%[2]s += consumed
%[3]s`, ref, posVar, nestedDecodeErrCheck(fieldLit(f), calleeTypeOf(f), f.MultiErr, true, "consumed"))
		} else {
			b.WriteString(renderCrossPkgStructDecode(f, ref, posVar))
		}
	case KindSlice:
		renderSlice(b, f, ref, posVar)
	case KindArray:
		emitByteArrayRead(b, f, ref, posVar, 0)
	case KindMap:
		renderMap(b, f, ref, posVar, false)
	case KindBytes:
		renderBytes(b, f, ref, posVar)
	case KindTime:
		renderTime(b, f, ref, posVar)
	case KindDuration:
		renderDuration(b, f, ref, posVar)
	case KindNetIP:
		renderNetIP(b, f, ref, posVar)
	case KindNetipAddr:
		renderNetipAddr(b, f, ref, posVar)
	case KindNetipPrefix:
		renderNetipPrefix(b, f, ref, posVar)
	case KindRawJSON:
		renderRawJSON(b, f, ref, posVar)
	case KindURL:
		renderURL(b, f, ref, posVar)
	case KindBigInt:
		renderBigInt(b, f, ref, posVar)
	case KindBigFloat:
		renderBigFloat(b, f, ref, posVar)
	case KindBigRat:
		renderBigRat(b, f, ref, posVar)
	case KindSQLNull:
		renderSQLNull(b, f, ref, posVar)
	case KindAny:
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

// renderSlice is the depth-0 entry point into the recursive slice emitter
// (struct-field path — not top-level).
func renderSlice(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	emitByteSliceRead(b, f, ref, posVar, 0, false)
}

// peelSliceField returns the FieldInfo one level down for a slice-of-slice or
// slice-of-array element. Rules shift up one level (InnerValidation[0] →
// ElemValidation, etc.). Called only when f.ElemKind is KindSlice / KindArray.
func peelSliceField(f FieldInfo) FieldInfo {
	innerGoType := f.ElemType
	innerElem, innerKind, innerLen := stripOneContainer(innerGoType)
	// Mirror the parse layer: one leading `*` unwraps into ElemPointer; any
	// remaining stars stay on ElemType for the multi-level route.
	innerPointer := false
	if strings.HasPrefix(innerElem, "*") {
		innerPointer = true
		innerElem = innerElem[1:]
		innerKind = resolveKind(innerElem)
		if innerKind == KindArray {
			innerLen = arrayLenFromType(innerElem)
		}
	}
	inner := FieldInfo{
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
		// HintLen=-1 ("unset") so preallocCap falls through to the kind
		// default — the zero-value 0 would read as opt-out.
		HintLen: -1,
	}
	if innerKind == KindArray {
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
	// Ordered per-level steps: this level's elem is the next inner level down.
	if len(f.Levels) > 1 {
		inner.Levels = f.Levels[1:]
	}
	// Per-level prealloc hints shift down the same way: HintLevels[0] sizes
	// this peeled level's rows.
	if len(f.HintLevels) > 0 {
		inner.HintLen = f.HintLevels[0]
		inner.HintLevels = f.HintLevels[1:]
	}
	return inner
}

// elemSteps returns the ordered per-element value steps (level-1 inner) —
// Levels[0] when present, else the derived split buckets.
func elemSteps(f FieldInfo) []Step {
	if len(f.Levels) > 0 {
		return f.Levels[0]
	}
	return stepsFromLegacy(f.ElemMods, f.ElemValidation)
}

// stripOneContainer peels off one layer of container (slice `[]` or array
// `[N]`) from a Go type string, returning the element type, its kind, and
// N (0 for slices / non-containers).
func stripOneContainer(typ string) (inner string, kind TypeKind, length int) {
	if strings.HasPrefix(typ, "[]") {
		inner = typ[2:]
		return inner, resolveKind(inner), 0
	}
	if len(typ) > 2 && typ[0] == '[' {
		if end := strings.IndexByte(typ, ']'); end > 1 {
			if n, err := strconv.Atoi(typ[1:end]); err == nil {
				inner = typ[end+1:]
				return inner, resolveKind(inner), n
			}
		}
	}
	return typ, resolveKind(typ), 0
}

// emitByteArrayRead is a thin wrapper around emitByteSliceRead for [N]T fields
// (struct-field path — not top-level).
func emitByteArrayRead(b *bytes.Buffer, f FieldInfo, dst, posVar string, depth int) {
	emitByteSliceRead(b, f, dst, posVar, depth, false)
}

// emitByteSliceRead emits a JSON array reader against `data` into `dst`,
// advancing posVar. Handles slices (pre-sized via preallocCap, appended) and
// fixed-length arrays (strict tuple: exactly f.ArrayLen elements or LenError).
// Data locals (evN, idxN, slabN) carry the depth suffix to avoid collisions.
// topLevel marks a whole-value decode (a top-level container alias): there's
// nothing to parse after the array, so each exit returns directly instead of
// falling through to a trailing `return`. Always false for struct fields and
// nested elements (their caller has more to parse).
func emitByteSliceRead(b *bytes.Buffer, f FieldInfo, dst, posVar string, depth int, topLevel bool) {
	isArray := f.Kind == KindArray
	arrayN := f.ArrayLen
	// Multi-level pointer element (`[]**T`, …): no slab — each element runs
	// the parse-first cascade. Depth-1 `[]*T` keeps the slab fast path.
	_, elemDepth := elemPtrType(f)
	mptr := elemDepth >= 2
	// kvar is posVar directly: the whole nest shares one position counter,
	// each level advancing it past the bytes it consumed.
	kvar := posVar
	evvar := fmt.Sprintf("ev%d", depth)
	ivar := fmt.Sprintf("idx%d", depth)
	// No leading WS skip — every value-entry site (after `:`/`[`/`,`, and the
	// alias path explicitly) already skipped it.
	// `null` → nil out the slice (arrays don't accept null). At dispatch level
	// flat-break; a nested slot whose parent consumed the null (NullDone)
	// skips the peek.
	flat := !isArray && nullBreakOK(f)
	if !isArray && !f.NullDone {
		inlineNullPeek(b, kvar)
		fmt.Fprintf(b, "%s = nil\n", dst)
		if flat {
			b.WriteString("break\n}\n")
		} else if topLevel {
			// Whole-value alias: null → return directly; no else needed since the
			// non-null path also returns at the array close.
			fmt.Fprintf(b, "return result, %s, nil\n}\n", kvar)
		} else {
			b.WriteString("} else {\n")
		}
	}
	fmt.Fprintf(b, "if %[1]s >= len(data) || data[%[1]s] != '[' { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrBadArray) }\n", kvar, fieldLit(f))
	fmt.Fprintf(b, "%s++\n", kvar)
	inlineSkipWS(b, kvar)
	slabVar := fmt.Sprintf("slab%d", depth)
	if isArray {
		fmt.Fprintf(b, "var %s int\n", ivar)
		// Arrays of pointers: an exact-sized heap slab. `[N]E` would escape
		// via `&slab[i]` anyway, so `make([]E, N)` skips the stack hop.
		if f.ElemPointer && !mptr {
			fmt.Fprintf(b, "%s := make([]%s, %d)\n", slabVar, f.ElemType, arrayN)
		}
	} else {
		sCap, slCap := preallocCap(f)
		countable := !f.ElemPointer && scalarCountable(f.ElemKind) && userPreallocHint(f) < 0
		// When the non-empty arm's prealloc would also be just `dst = T{}`
		// (no count, no cap, no slab), both empty-peek arms are byte-identical,
		// so emit one `if dst == nil` and skip the peek.
		if !f.ElemPointer && !countable && sCap == "0" {
			fmt.Fprintf(b, "if %s == nil { %s = %s{} }\n", dst, dst, f.GoType)
		} else {
			if f.ElemPointer && !mptr {
				fmt.Fprintf(b, "var %s []%s\n", slabVar, f.ElemType)
			}
			// dst is decode-into-receiver: nil (fresh) or [:0]'d (backing reused).
			// Allocate only when nil; otherwise reuse the caller's backing.
			fmt.Fprintf(b, "if %s < len(data) && data[%s] == ']' {\n", kvar, kvar)
			fmt.Fprintf(b, "if %s == nil { %s = %s{} }\n", dst, dst, f.GoType)
			fmt.Fprintf(b, "} else {\n")
			switch {
			case countable:
				// Exact-cap from a one-shot delimiter scan (scalar elems carry no
				// `,`/`]`): kills the growth chain with no over-cap cost.
				// Reused slots keep their backing; malformed input falls to cap 1.
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
	// Every elem kind decodes IN PLACE into the destination slot (slab[ivar] /
	// dst[ivar] for arrays; pre-grown slab/dst tail for slices). err is hoisted
	// at the DecodeFrom function level.
	directStruct := f.ElemKind == KindStruct && isGenerated(f.ElemType)
	_ = evvar // unused at this scope
	// Do-while: the empty `[]` and truncation cases are caught by this one-time
	// guard (preserving ErrBadArray below); inside, the comma handler's
	// continue/break drives iteration, so the per-element `data!=']'` re-check is
	// redundant.
	fmt.Fprintf(b, "if %s < len(data) && data[%s] != ']' {\nfor {\n", kvar, kvar)
	if isArray {
		// Strict tuple: reject excess elements.
		fmt.Fprintf(b, "if %s >= %d { return result, i, %s }\n",
			ivar, arrayN,
			arrayLenErr(f.JSONName, arrayN, ivar, "i"))
	}
	if f.ElemPointer && !mptr {
		// `null` element → nil pointer; skip the parse + slab work.
		inlineNullPeek(b, kvar)
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
	// Compute the in-place target; slice cases pre-grow the slot here.
	var target string
	switch {
	case isArray && mptr:
		target = fmt.Sprintf("%s[%s]", dst, ivar)
	case mptr:
		// Reslice within cap so the carried pointer chain survives into the
		// slot and the cascade can reuse it; a past-cap grow starts nil.
		if elemPtrReusable(f) {
			fmt.Fprintf(b, "if len(%[1]s) < cap(%[1]s) { %[1]s = %[1]s[:len(%[1]s)+1] } else { %[1]s = append(%[1]s, nil) }\n", dst)
		} else {
			fmt.Fprintf(b, "%s = append(%s, nil)\n", dst, dst)
		}
		target = fmt.Sprintf("%s[len(%s)-1]", dst, dst)
	case isArray && f.ElemPointer:
		target = fmt.Sprintf("%s[%s]", slabVar, ivar)
	case isArray:
		// A generated struct element resets itself, so the carried slot is
		// handed straight over and its inner allocations get reused. Any other
		// mergeable element (encoding/json, UnmarshalJSON/UnmarshalText) merges
		// into what it is given, so blank the slot first.
		if f.ElemKind == KindStruct && !f.ElemPointer && !directStruct {
			fmt.Fprintf(b, "%s[%s] = %s\n", dst, ivar, zeroLit(f.ElemType, f.ElemKind))
		}
		target = fmt.Sprintf("%s[%s]", dst, ivar)
	case f.ElemPointer:
		// Slab holds the pointee type; pre-grow the tail slot.
		emitElemGrow(b, slabVar, f, directStruct)
		target = fmt.Sprintf("%s[len(%s)-1]", slabVar, slabVar)
	case f.ElemKind == KindSlice:
		// Slice element: reslice within cap so the carried inner header/backing
		// survives for reuse; only a past-cap grow allocates a nil slot.
		fmt.Fprintf(b, "if len(%[1]s) < cap(%[1]s) { %[1]s = %[1]s[:len(%[1]s)+1] } else { %[1]s = append(%[1]s, nil) }\n", dst)
		target = fmt.Sprintf("%s[len(%s)-1]", dst, dst)
	default:
		emitElemGrow(b, dst, f, directStruct)
		target = fmt.Sprintf("%s[len(%s)-1]", dst, dst)
	}
	field := fieldLit(f)
	errCheck := fmt.Sprintf("if err != nil { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, err) }\n", kvar, field)
	if mptr {
		// Elem rules ride inside the cascade; no slab/post-decode here. Slice
		// slots are pre-grown nil (TargetNil); ARRAY slots are overwritten
		// too — the contract is "every slot overwritten", and seeding from
		// the carried chain merged `[2]**Inner` where `[]**Inner` and
		// `[2]*Inner` both decoded fresh.
		pf := elemPtrField(f, f.JSONName+"[]")
		// An ARRAY slot keeps the assignment-only cascade: its contract is
		// "every slot overwritten", pinned by TestMerge_ArraySlotsOverwrite.
		pf.TargetNil = isArray || !elemPtrReusable(f)
		renderField(b, pf, target, kvar)
	} else {
		// Named-primitive ELEMENT (`[]Priority`): scan the underlying into a
		// temp and convert into the slot, same as the field level. The elem
		// pipe below still runs against the named type.
		elemCast, elemTarget := "", ""
		savedElemType, savedElemKind := f.ElemType, f.ElemKind
		if prim, kind, ok := inlineNamedPrim(elemAsField(f)); ok {
			elemCast, elemTarget = f.ElemType, target
			target = fmt.Sprintf("namedElem%d", depth)
			fmt.Fprintf(b, "var %s %s\n", target, prim)
			f.ElemType, f.ElemKind = prim, kind
		}
		switch f.ElemKind {
		case KindString:
			inlineScanString(b, kvar, target, kvar, field, f.Copy, !f.AllowInvalidUTF8)
		case KindBool:
			fmt.Fprintf(b, "%s, %s, err = ggen.Bool(data, %s)\n", target, kvar, kvar)
			b.WriteString(errCheck)
		case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
			castFn := ""
			if f.ElemType != "int64" {
				castFn = f.ElemType
			}
			inlineScanInt64(b, kvar, target, castFn, field)
		case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
			castFn := ""
			if f.ElemType != "uint64" {
				castFn = f.ElemType
			}
			inlineScanUint64(b, kvar, target, castFn, field)
		case KindFloat32, KindFloat64:
			if f.ElemKind == KindFloat64 {
				fmt.Fprintf(b, "%s, %s, err = ggen.Float64(data, %s)\n", target, kvar, kvar)
				b.WriteString(errCheck)
			} else {
				b.WriteString("var fv float64\n")
				fmt.Fprintf(b, "fv, %s, err = ggen.Float64(data, %s)\n", kvar, kvar)
				b.WriteString(errCheck)
				b.WriteString(narrowFloatGuard("fv", "float32",
					fmt.Sprintf("return result, %s, ggen.NewParseErr(%s, %s, ggen.ErrNumberOverflow)", kvar, field, kvar)))
				fmt.Fprintf(b, "%s = float32(fv)\n", target)
			}
		case KindStruct:
			if directStruct {
				fmt.Fprintf(b, `var consumed int
%[1]s, consumed, err = %[1]s.`+decodeCallFor(f.ElemType)+`
%[2]s += consumed
`, target, kvar)
				b.WriteString(nestedDecodeErrCheck(fieldLit(f), calleeTypeOf(f), f.MultiErr, true, "consumed"))
			} else {
				// Cross-package / method-carrying element: same ladder the
				// field level uses. This used to be a bare SkipValue, which
				// silently decoded every element of a `[]foreign.T` to its
				// zero value.
				b.WriteString(renderCrossPkgStructDecode(elemAsField(f), target, kvar))
			}
		default:
			// Dedicated-kind element (time/duration/bytes/raw/netip/url/big/
			// sqlnull/any/map): the same field-level emitter the struct level
			// uses, with a sanitized FieldInfo. Used to fall through — the
			// pre-grown zero slot was never scanned, so every non-empty array
			// failed ErrBadArray (or the file didn't compile).
			renderField(b, sliceElemField(f), target, kvar)
		case KindSlice, KindArray:
			// Nested container — recurse, peeling one outer [] / [N] off.
			inner := peelSliceField(f)
			if inner.Kind == KindSlice && len(f.ElemMods) == 0 {
				// `null` elem → nil slot handled HERE so the inner body isn't
				// nested in an else (mirrors the []*T nil-elem fast path).
				inlineNullPeek(b, kvar)
				// Slot may carry a reused/receiver header — nil unconditionally.
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
			// Hoist the nested slot into a depth-local `rowN` so the inner loop
			// writes a local header (no per-element parent-backing barrier);
			// `target = rowN` publishes the finished row. Seed from the carried
			// slot ([:0] reset) so the inner decode reuses its backing.
			row := fmt.Sprintf("row%d", depth)
			fmt.Fprintf(b, "%s := %s\n", row, target)
			if inner.Kind == KindSlice {
				fmt.Fprintf(b, "if %[1]s != nil { %[1]s = %[1]s[:0] }\n", row)
			}
			emitByteSliceRead(b, inner, row, kvar, depth+1, false)
			fmt.Fprintf(b, "%s = %s\n", target, row)
		}
		if elemCast != "" {
			fmt.Fprintf(b, "%s = %s(%s)\n", elemTarget, elemCast, target)
			target, f.ElemType, f.ElemKind = elemTarget, savedElemType, savedElemKind
		}
		renderPipe(b, elemSteps(f), target, f.JSONName+"[]", f.ElemType, f.ElemKind, f.MultiErr, "i")
	}
	switch {
	case isArray && f.ElemPointer && !mptr:
		// Slab slot decoded in-place; publish its address.
		fmt.Fprintf(b, "%s[%s] = &%s[%s]\n", dst, ivar, slabVar, ivar)
		fmt.Fprintf(b, "%s++\n", ivar)
	case isArray:
		fmt.Fprintf(b, "%s++\n", ivar)
	case f.ElemPointer && !mptr:
		// Slab tail decoded in-place; publish addr.
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
	fmt.Fprintf(b, "if %[1]s >= len(data) || data[%[1]s] != ']' { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, ggen.ErrBadArray) }\n", kvar, fieldLit(f))
	if isArray {
		fmt.Fprintf(b, "if %s != %d { return result, i, %s }\n",
			ivar, arrayN,
			arrayLenErr(f.JSONName, arrayN, ivar, "i"))
	}
	fmt.Fprintf(b, "%s++\n", posVar)
	if topLevel {
		// Whole-value alias: array consumed, nothing follows → return directly.
		fmt.Fprintf(b, "return result, %s, nil\n", posVar)
	}
	if !isArray && !flat && !f.NullDone && !topLevel {
		b.WriteString("}\n") // close else (null-check)
	}
}

// renderStreamDecode emits the streaming counterpart of renderDecode, using
// ggen.Stream methods that pull more bytes on demand.
func renderStreamDecode(b *bytes.Buffer, s StructInfo) {
	// Named-result return slot — see DecodeFrom above.
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
		// Only structs that actually call into a cyclic nested type need
		// `depth` in scope (decodeCallFor emits `depth+1` there) — render
		// first and gate on that, so the common acyclic-package struct
		// doesn't carry a dead const.
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

// tierStreamStringCalls rewrites the stream-decode body's string-scan calls
// to the fixed SIMD tier when one is selected: `= s.String()` / `StringView`
// / `KeyView` become their fused per-tier Stream methods (assignment-shaped
// only, so nothing else can match; encode bodies are never passed through).
// KeyView keeps the scalar-prelude original on all-short-key structs — same
// gate and rationale as the bytes-path key window (the prelude beats the
// vector dependency chain on ≤5-byte keys).
func tierStreamStringCalls(body string, s StructInfo) string {
	if simdSuffix == "" {
		return body
	}
	body = strings.ReplaceAll(body, "= s.String(", "= s.String"+simdSuffix+"(")
	body = strings.ReplaceAll(body, "= s.StringView(", "= s.StringView"+simdSuffix+"(")
	// Skip tree + dispatch whitespace: same shells/fast paths as the scalar
	// pair on compact input, vector runs on whitespace-rich streams.
	body = strings.ReplaceAll(body, "= s.SkipValue()", "= s.SkipValue"+simdSuffix+"()")
	body = strings.ReplaceAll(body, "= s.SkipSpace()", "= s.SkipSpace"+simdSuffix+"()")
	body = strings.ReplaceAll(body, "= s.CaptureValue()", "= s.CaptureValue"+simdSuffix+"()")
	if maxJSONNameLen(s) > 5 {
		body = strings.ReplaceAll(body, "= s.KeyView(", "= s.KeyView"+simdSuffix+"(")
	}
	return body
}

func renderStreamDecodeStruct(b *bytes.Buffer, s StructInfo) {
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
	// Dispatch runs IMMEDIATELY after KeyView so the alias is still valid for
	// the switch. ConsumeColon (opening each case) may compact the buffer and
	// kill the alias, so cases that capture key must clone before it.
	chk := streamErrCheck(`""`)
	// A drained refill maps to the sentinel the BYTES path reports for the
	// same truncation: at the key position `{` wants a name, past a value it
	// wants ',' or '}'. Transient reader errors still propagate raw.
	rmore := func(sentinel string) string {
		return fmt.Sprintf(`if s.Pos >= len(s.Bytes()) { if err = s.ReadMore(s.Pos); err != nil { return result, ggen.NewParseErr("", s.Offset(), ggen.NotEOF(err, %s)) }; s.Pos = 0 }
`, sentinel)
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

func renderStreamDispatch(s StructInfo) string {
	// Each known-key case opens with s.ConsumeColon — the alias isn't needed
	// past this point, so the shift it triggers is safe.
	emitField := func(b *bytes.Buffer, f FieldInfo, parse string) {
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

// renderStreamMap emits map decode for the stream path. `null` → nil,
// empty `{}` → non-nil empty, else fresh make(). Map keys must be detached
// copies (the buffer can be overwritten on ReadMore), so we use s.String.
func renderStreamMap(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
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
	if cap := mapPreallocCap(f); cap > 0 {
		makeExpr = fmt.Sprintf("make(%s, %d)", f.GoType, cap)
	}
	// At dispatch level the null branch breaks to the comma handling.
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
		// Pointer value (any depth): same cascade as the bytes path, decoded
		// into the map slot (TargetNil keeps the emit assignment-only).
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
			b.WriteString("}\n") // close else (null-check)
		}
		return
	}
	// Named-primitive VALUE — see the bytes twin.
	valCast, valTarget := "", ""
	savedElemType, savedElemKind := f.ElemType, f.ElemKind
	if prim, kind, ok := inlineNamedPrim(elemAsField(f)); ok {
		valCast, valTarget = f.ElemType, mapTarget
		mapTarget = "namedVal"
		fmt.Fprintf(b, "var %s %s\n", mapTarget, prim)
		f.ElemType, f.ElemKind = prim, kind
	}
	switch f.ElemKind {
	case KindString:
		fmt.Fprintf(b, `%s, err = s.String(%s)
%s`, mapTarget, vArg(f), chk)
	case KindBool:
		fmt.Fprintf(b, `%s, err = s.Bool()
%s`, mapTarget, chk)
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
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
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
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
	case KindFloat32, KindFloat64:
		if f.ElemKind == KindFloat64 {
			fmt.Fprintf(b, `%s, err = s.Float64()
%s`, mapTarget, chk)
		} else {
			fmt.Fprintf(b, `var fv float64
fv, err = s.Float64()
%[2]s%[3]s%[1]s = float32(fv)
`, mapTarget, chk,
				narrowFloatGuard("fv", "float32", fmt.Sprintf("return result, ggen.NewParseErr(%s, s.Offset(), ggen.ErrNumberOverflow)", field)))
		}
	case KindStruct:
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
		// Dedicated-kind value — same field-level delegation as the bytes
		// twin (the old arm skipped the value, leaving `mk` unused). Braced
		// for the same scope hygiene.
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
		b.WriteString("}\n") // close else (null-check)
	}
}

// streamErrCheck returns the `if err != nil { return ... }` tail for a
// stream-side scan call, with the field literal embedded (no post-pass wrap).
func streamErrCheck(field string) string {
	return fmt.Sprintf("if err != nil { return result, ggen.NewParseErr(%s, s.Offset(), err) }\n", field)
}

// bytesErrCheck is the bytes-path equivalent of streamErrCheck, with the
// 3-tuple `return result, pos, err` shape.
func bytesErrCheck(field, posVar string) string {
	return fmt.Sprintf("if err != nil { return result, %[1]s, ggen.NewParseErr(%[2]s, %[1]s, err) }\n", posVar, field)
}

// streamReadMore emits the "buffer exhausted? pull more" guard. keep controls
// compaction (`0` = grow, `s.Pos` = discard consumed prefix); resetPos appends
// `s.Pos = 0` after a non-zero keep. sentinel, when non-empty, maps a DRAINED
// refill to the grammar sentinel the bytes path reports for the same
// truncation (round-6 #60, extended to every generated value-head refill);
// transient reader errors still propagate raw via ggen.NotEOF.
func streamReadMore(field, keep string, resetPos bool, sentinel string) string {
	reset := ""
	if resetPos {
		reset = "; s.Pos = 0"
	}
	errExpr := "err"
	if sentinel != "" {
		errExpr = "ggen.NotEOF(err, " + sentinel + ")"
	}
	return fmt.Sprintf("if s.Pos >= len(s.Bytes()) { if err = s.ReadMore(%s); err != nil { return result, ggen.NewParseErr(%s, s.Offset(), %s) }%s }\n", keep, field, errExpr, reset)
}

// truncSentinel maps a value kind to the grammar sentinel the BYTES path
// reports for input truncated at that value's head.
func truncSentinel(k TypeKind) string {
	switch k {
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64,
		KindUint, KindUint8, KindUint16, KindUint32, KindUint64,
		KindFloat32, KindFloat64, KindBigInt:
		return "ggen.ErrBadNumber"
	case KindBool:
		return "ggen.ErrBadBool"
	case KindString, KindBytes, KindTime, KindDuration, KindNetIP,
		KindNetipAddr, KindNetipPrefix, KindURL, KindBigFloat, KindBigRat:
		return "ggen.ErrExpectString"
	case KindMap, KindStruct:
		return "ggen.ErrBadObject"
	case KindSlice, KindArray:
		return "ggen.ErrBadArray"
	}
	return "ggen.ErrUnexpectedEnd"
}

// nestedDecodeErrCheck emits the `if err != nil { … }` tail after a nested
// DecodeFrom call. The outer field is woven into the wrap so the inner
// *ParseError's path is prepended ("addr" + "street" → "addr.street"). In
// multierr mode validation failures merge via Errors.Append; parse errors
// return early. bytesPath selects the 3-tuple vs 2-tuple shape.
// nVar names the bytes-path consumed-count local (`consumed`/`entryConsumed`) — the cursor
// was advanced by it BEFORE this check, so the nested value STARTED at
// i-nVar and the callee's sub-slice-relative error positions rebase by that
// (NewParseErrShift / ShiftPos). Stream positions are already global.
// calleeTypeOf names the type whose DecodeFrom a field's nested decode calls
// — the element type for containers, else the field's own (pointer-peeled).
func calleeTypeOf(f FieldInfo) string {
	switch f.Kind {
	case KindSlice, KindArray, KindMap:
		return f.ElemType
	}
	if f.ElemType != "" && (f.ElemKind == KindStruct) {
		return f.ElemType
	}
	if f.PointeeType != "" {
		return f.PointeeType
	}
	return f.GoType
}

// calleeDrains reports whether a nested callee of type t finishes the whole
// value before surfacing a validation failure — true only for a MULTIERR
// callee. A single-error callee returns at its FIRST failure, mid-value, so
// a parent that drained its error and kept going would resume from a
// desynced cursor.
func calleeDrains(t string) bool {
	_, ok := multiErrTypes[strings.TrimLeft(t, "*")]
	return ok
}

// nestedDecodeErrCheck emits the error handling after a nested decode call.
// callee is the callee's type name ("" when unknown), used to gate the
// multierr drain — see calleeDrains.
func nestedDecodeErrCheck(field, callee string, multierr, bytesPath bool, nVar string) string {
	wrap := fmt.Sprintf("ggen.NewParseErr(%s, s.Offset(), err)", field)
	ret := fmt.Sprintf("return result, %s", wrap)
	drain := fmt.Sprintf("errs.Append(%s, verr)", field)
	if bytesPath {
		wrap = fmt.Sprintf("ggen.NewParseErrShift(%s, i, %s, err)", field, nVar)
		ret = fmt.Sprintf("return result, i, %s", wrap)
		drain = fmt.Sprintf("errs.Append(%s, ggen.ShiftPos(verr, i-%s))", field, nVar)
	}
	// Draining is only sound when the callee finished the value; otherwise
	// its error is a hard stop for this decoder too.
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

func renderStreamBytes(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	if byteArrayLen(f) > 0 {
		renderStreamBytesValue(b, f, ref, posVar) // no `null` form — see renderBytes
		return
	}
	field := fieldLit(f)
	rm := streamReadMore(field, "0", false, "ggen.ErrExpectString")
	rmKi := strings.Replace(streamReadMore(field, "0", false, "ggen.ErrBadLiteral"), "if s.Pos >=", "if s.Pos+ki >=", 1)
	// `null` → nil out the field (see renderBytes).
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

func renderStreamBytesValue(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	_ = posVar // stream-path uses s.Pos directly
	field := fieldLit(f)
	chk := streamErrCheck(field)
	rm := streamReadMore(field, "0", false, "ggen.ErrBadArray")
	badArr := fmt.Sprintf("return result, ggen.NewParseErr(%s, s.Offset(), ggen.ErrBadArray)", field)
	if f.Format == "array" {
		// `u` not `v`: a pointer-leaf caller declares `var v []byte` in the
		// same scope.
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
	// `sv` not `v`: a pointer-leaf caller declares `var v []byte` here.
	// StringView aliases s.buf; AppendDecode consumes it before the next
	// stream op and retains no bytes — see Stream.StringView.
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

func renderStreamTime(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
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
	// `sv` not `v`: pointer-leaf caller declares `var v time.Time` here.
	// StringView aliases s.buf; time.Parse retains none of it — see Stream.StringView.
	fmt.Fprintf(b, `var sv string
sv, err = s.StringView(`+vArg(f)+`)
%[3]s%[1]s, err = time.Parse(%[2]s, sv)
%[3]s`, ref, layout, chk)
}

func renderStreamDuration(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	_ = posVar
	field := fieldLit(f)
	chk := streamErrCheck(field)
	// `f` / `sv` not `v`: pointer-leaf caller declares `var v time.Duration` here.
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
	// StringView: ParseDuration retains no input — see Stream.StringView.
	fmt.Fprintf(b, `var sv string
sv, err = s.StringView(`+vArg(f)+`)
%[2]s%[1]s, err = time.ParseDuration(sv)
%[2]s`, ref, chk)
}

func renderStreamNetIP(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	_ = posVar
	field := fieldLit(f)
	chk := streamErrCheck(field)
	// StringView: net.ParseIP copies into a fresh IP; the error literal
	// retains sv, so it clones — string(sv) on a string is an IDENTITY
	// conversion (no copy), strings.Clone is the real one.
	fmt.Fprintf(b, `var sv string
sv, err = s.StringView(`+vArg(f)+`)
%[2]s%[1]s = net.ParseIP(sv)
if %[1]s == nil { return result, ggen.NewParseErr(%[3]s, s.Offset(), &net.ParseError{Type: "IP address", Text: strings.Clone(sv)}) }
`, ref, chk, field)
}

func renderStreamNetipAddr(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	_ = posVar
	chk := streamErrCheck(fieldLit(f))
	// StringView: netip.Addr is a value type and zones are deep-copied by
	// unique — no input bytes retained on success — see Stream.StringView.
	fmt.Fprintf(b, `var sv string
sv, err = s.StringView(`+vArg(f)+`)
%[2]s%[1]s, err = netip.ParseAddr(sv)
%[2]s`, ref, chk)
}

func renderStreamNetipPrefix(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	_ = posVar
	chk := streamErrCheck(fieldLit(f))
	// StringView: netip.Prefix is a value type, no input bytes retained on
	// success — see Stream.StringView.
	fmt.Fprintf(b, `var sv string
sv, err = s.StringView(`+vArg(f)+`)
%[2]s%[1]s, err = netip.ParsePrefix(sv)
%[2]s`, ref, chk)
}

// renderStreamRawJSON copies the captured span into the field (CaptureValue
// returns a buffer alias, so the append detaches it). The receiver's backing is
// reused when it has the room, matching the bytes path's copy shape.
func renderStreamRawJSON(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
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

func renderStreamURL(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	_ = posVar
	chk := streamErrCheck(fieldLit(f))
	fmt.Fprintf(b, `var sv string
sv, err = s.String(`+vArg(f)+`)
%[2]su, err := url.Parse(sv)
%[2]s%[1]s = *u
`, ref, chk)
}

func renderStreamBigInt(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	_ = posVar
	field := fieldLit(f)
	chk := streamErrCheck(field)
	fmt.Fprintf(b, `span, err := s.CaptureValue()
%[2]sif _, ok := (&%[1]s).SetString(unsafe.String(unsafe.SliceData(span), len(span)), 10); !ok {
	return result, ggen.NewParseErr(%[3]s, s.Offset(), ggen.ErrBadNumber)
}
`, ref, chk, field)
}

func renderStreamBigFloat(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	_ = posVar
	field := fieldLit(f)
	chk := streamErrCheck(field)
	// StringView: (*big.Float).Parse retains no input — see Stream.StringView.
	fmt.Fprintf(b, `var sv string
sv, err = s.StringView(`+vArg(f)+`)
%[2]sif _, _, err := (&%[1]s).Parse(sv, 10); err != nil {
	return result, ggen.NewParseErr(%[3]s, s.Offset(), err)
}
`, ref, chk, field)
}

func renderStreamBigRat(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	_ = posVar
	field := fieldLit(f)
	chk := streamErrCheck(field)
	// StringView: (*big.Rat).SetString retains no input — see Stream.StringView.
	fmt.Fprintf(b, `var sv string
sv, err = s.StringView(`+vArg(f)+`)
%[2]sif _, ok := (&%[1]s).SetString(sv); !ok {
	return result, ggen.NewParseErr(%[3]s, s.Offset(), ggen.ErrBadNumber)
}
`, ref, chk, field)
}

// renderStreamSQLNull is the streaming counterpart of renderSQLNull.
func renderStreamSQLNull(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	_ = posVar
	if f.SQLNullInner != nil {
		inner := sqlNullInnerField(f)
		field := fieldLit(f)
		rm := streamReadMore(field, "0", false, truncSentinel(effectiveKind(inner.GoType, inner.Kind)))
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
	spec, ok := SQLNullSpec(f.GoType)
	if !ok {
		return
	}
	field := fieldLit(f)
	chk := streamErrCheck(field)
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
	switch spec.Inner {
	case KindString:
		scanTmpl("string", "String")
		valExpr = "nv"
	case KindBool:
		scanTmpl("bool", "Bool")
		valExpr = "nv"
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		scanTmpl("int64", "Int64")
		valExpr = "nv"
		if spec.Type != "int64" {
			valExpr = fmt.Sprintf("%s(nv)", spec.Type)
		}
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		scanTmpl("uint64", "Uint64")
		valExpr = "nv"
		if spec.Type != "uint64" {
			valExpr = fmt.Sprintf("%s(nv)", spec.Type)
		}
	case KindFloat32, KindFloat64:
		scanTmpl("float64", "Float64")
		valExpr = "nv"
		if spec.Type != "float64" {
			valExpr = fmt.Sprintf("%s(nv)", spec.Type)
		}
	case KindTime:
		tf := FieldInfo{JSONName: f.JSONName, Format: f.Format,
			Copy: f.Copy, AllowInvalidUTF8: f.AllowInvalidUTF8, MultiErr: f.MultiErr}
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

func renderStreamAny(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	_ = posVar
	chk := streamErrCheck(fieldLit(f))
	fn := "s.Any"
	if f.UseNumber {
		fn = "s.AnyNumber"
	}
	fmt.Fprintf(b, `%[1]s, err = %[2]s()
%[3]s`, ref, fn, chk)
}

// streamUnknownKey is the stream-scanner counterpart of unknownKey. `key`
// aliases the stream buffer; ConsumeColon shifts it, so any value surviving
// past that (stored keys, retained errors) must be cloned BEFORE ConsumeColon.
// The immediate-return branch reads the alias directly (function exits first).
func streamUnknownKey(s StructInfo, posVar string) string {
	_ = posVar
	if embedded := s.EmbedField(); embedded.Embed {
		chk := streamErrCheck("ownKey")
		prelude := fmt.Sprintf(`ownKey := strings.Clone(key)
err = s.ConsumeColon()
%[3]sif result.%[1]s == nil { result.%[1]s = make(%[2]s) }
`, embedded.GoName, embedded.GoType, chk)
		switch embedded.ElemKind {
		case KindAny:
			anyFn := "s.Any"
			if s.UseNumber {
				anyFn = "s.AnyNumber"
			}
			return prelude + fmt.Sprintf(`result.%[1]s[ownKey], err = %[2]s()
%[3]s`, embedded.GoName, anyFn, chk)
		case KindString:
			return prelude + fmt.Sprintf(`result.%[1]s[ownKey], err = s.String(`+vArg(embedded)+`)
%[2]s`, embedded.GoName, chk)
		case KindStruct:
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
	if s.MultiErr {
		chk := streamErrCheck("ownKey")
		return fmt.Sprintf(`ownKey := strings.Clone(key)
errs = append(errs, &ggen.UnknownKeyError{Pos: s.Offset(), Path: []string{ownKey}})
err = s.ConsumeColon()
%[1]serr = s.SkipValue()
%[1]s`, chk)
	}
	return "return result, &ggen.UnknownKeyError{Pos: s.Offset(), Path: []string{strings.Clone(key)}}\n"
}

func renderStreamStringTag(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	_ = posVar // stream-path uses s.Pos directly
	field := fieldLit(f)
	chk := streamErrCheck(field)
	// Brace-less — see renderStringTag.
	b.WriteString("var sv string\n")
	fmt.Fprintf(b, "sv, err = s.KeyView(%s)\n", vArg(f))
	b.WriteString(chk)
	switch f.Kind {
	case KindBool:
		fmt.Fprintf(b, `switch sv {
case "true": %[1]s = true
case "false": %[1]s = false
default: return result, ggen.NewParseErr(%[2]s, s.Offset(), ggen.ErrBadBool)
}
`, ref, field)
	case KindFloat32, KindFloat64:
		b.WriteString("f, err := strconv.ParseFloat(sv, 64)\n")
		b.WriteString(chk)
		if f.Kind == KindFloat32 {
			b.WriteString(narrowFloatGuard("f", "float32",
				fmt.Sprintf("return result, ggen.NewParseErr(%s, s.Offset(), ggen.ErrNumberOverflow)", field)))
			fmt.Fprintf(b, "%s = float32(f)\n", ref)
		} else {
			fmt.Fprintf(b, "%s = f\n", ref)
		}
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		b.WriteString("u, err := strconv.ParseUint(sv, 10, 64)\n")
		b.WriteString(chk)
		if f.Kind == KindUint64 {
			fmt.Fprintf(b, "%s = u\n", ref)
		} else {
			b.WriteString(narrowIntGuard("u", kindNarrowName(f.Kind),
				fmt.Sprintf("return result, ggen.NewParseErr(%s, s.Offset(), ggen.ErrNumberOverflow)", field)))
			fmt.Fprintf(b, "%s = %s(u)\n", ref, f.GoType)
		}
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		b.WriteString("n, err := strconv.ParseInt(sv, 10, 64)\n")
		b.WriteString(chk)
		if f.Kind == KindInt64 {
			fmt.Fprintf(b, "%s = n\n", ref)
		} else {
			b.WriteString(narrowIntGuard("n", kindNarrowName(f.Kind),
				fmt.Sprintf("return result, ggen.NewParseErr(%s, s.Offset(), ggen.ErrNumberOverflow)", field)))
			fmt.Fprintf(b, "%s = %s(n)\n", ref, f.GoType)
		}
	}
}

func renderStreamField(f FieldInfo, ref, posVar string) string {
	// Converter variant FIRST, exactly like renderField: the converter's
	// result already has the field's (possibly named) type, so routing a
	// named-primitive field through the underlying-typed temp below would
	// assign `cv` (type T) into a temp of T's underlying type.
	if fieldHasConverter(f) {
		out := renderVariantDispatchStream(f, ref, posVar)
		if !f.NoValidate {
			var vb bytes.Buffer
			validateAndModStream(&vb, f, ref)
			out += vb.String()
		}
		return out
	}
	// Named primitive — see renderField. Same shape on this path: read the
	// underlying, convert at the assign.
	if prim, kind, ok := inlineNamedPrim(f); ok && !f.Pointer && !f.String {
		var out string
		nz := nullZeroApplies(f)
		flat := nz && nullBreakOK(f)
		if nz {
			var nb bytes.Buffer
			emitStreamNullZero(&nb, ref, zeroLit(f.GoType, f.Kind), fieldLit(f), flat, truncSentinel(effectiveKind(f.GoType, f.Kind)))
			out = nb.String()
		}
		tmp := "named" + f.GoName
		if rest, expr, ok := foldTailAssign(renderStreamField(namedPrimInner(f, prim, kind), tmp, posVar), tmp); ok {
			// Write-once temp folds straight into the conversion.
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
	// See renderField — `,string` on bool is a no-op to match jsonv2. Pointer
	// fields fall through to the pointer peel below, whose leaf recursion
	// re-enters here (the string-tag branch on `*T` emits broken code).
	if f.String && f.Kind != KindBool && !f.Pointer {
		var out bytes.Buffer
		nz := nullZeroApplies(f)
		flat := nz && nullBreakOK(f)
		if nz {
			emitStreamNullZero(&out, ref, zeroLit(f.GoType, f.Kind), fieldLit(f), flat, truncSentinel(effectiveKind(f.GoType, f.Kind)))
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
		// See renderField: custom @Func rules want the pointer, built-ins the leaf.
		builtinV, customV := partitionCustomValidation(f.Validation)
		builtinM, customM := partitionCustomMods(f.Mods)
		rm := streamReadMore(field, "0", false, truncSentinel(effectiveKind(f.PointeeType, f.Kind)))
		rmKi := strings.Replace(streamReadMore(field, "0", false, "ggen.ErrBadLiteral"), "if s.Pos >=", "if s.Pos+ki >=", 1)
		// Decode-into-receiver, any depth — see renderField.
		depth, leafType := pointerDepth(f.GoType)
		leaf := f
		leaf.Pointer = false
		leaf.PointeeType = ""
		leaf.GoType = leafType
		leaf.Kind = resolveKind(leafType)
		leaf.Validation = builtinV
		leaf.Mods = builtinM
		leaf.Pipe = nil // partitioned buckets are the source
		leaf.AtDispatch = false
		leaf.TargetNil = false
		// Widened numeric leaf: scan into a wide temp, cast at the assign.
		scanType, valExpr, narrowCast := leafType, "v", ""
		if wide, wideKind, cast := widenedLeafCast(leaf.Kind, leafType); wide != "" {
			leaf.Kind, leaf.GoType, scanType, valExpr, narrowCast = wideKind, wide, wide, cast+"(v)", cast
		}
		// At dispatch level the null branch breaks to the comma handling.
		flat := nullBreakOK(f)
		fmt.Fprintf(b, `%[2]sif s.Bytes()[s.Pos] == 'n' {
	for ki := 1; ki < 4; ki++ {
		%[3]sif s.Bytes()[s.Pos+ki] != "null"[ki] { return result, ggen.NewParseErr(%[4]s, s.Offset(), ggen.ErrBadLiteral) }
	}
	s.Pos += 4
	%[1]s = nil
`, ref, rm, rmKi, field)
		if flat {
			b.WriteString("break\n}\n")
		} else {
			b.WriteString("} else {\n")
		}
		fmt.Fprintf(b, "var v %s\n", scanType)
		if !f.TargetNil {
			emitPointerSeed(b, ref, depth, leaf.Kind)
		}
		b.WriteString(renderStreamField(leaf, "v", posVar))
		b.WriteString(narrowIntGuard("v", narrowCast, fmt.Sprintf("return result, ggen.NewParseErr(%s, s.Offset(), ggen.ErrNumberOverflow)", field)))
		b.WriteString(narrowFloatGuard("v", narrowCast, fmt.Sprintf("return result, ggen.NewParseErr(%s, s.Offset(), ggen.ErrNumberOverflow)", field)))
		if f.TargetNil {
			// Target is a known-nil `var x *T` — straight new-chain assign.
			fmt.Fprintf(b, "%s = %s\n", ref, newChain(valExpr, depth))
		} else {
			emitPointerAssign(b, ref, depth, valExpr)
		}
		if !flat {
			b.WriteString("}\n")
		}
		if !f.NoValidate && (len(customV) > 0 || len(customM) > 0) {
			outer := f
			outer.Validation = customV
			outer.Mods = customM
			outer.Pipe = nil // partitioned customs are the source
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
		if method == "Float64" {
			guard = narrowFloatGuard(wideVar, castTo, errRet)
		}
		fmt.Fprintf(b, `var %[1]s %[2]s
%[1]s, err = s.%[3]s()
%[6]s%[7]s%[4]s = %[5]s(%[1]s)
`, wideVar, wideType, method, ref, castTo, chk, guard)
	}
	// `nullzero`: see renderField.
	nz := nullZeroApplies(f)
	flat := nz && nullBreakOK(f)
	if nz {
		emitStreamNullZero(b, ref, zeroLit(f.GoType, f.Kind), field, flat, truncSentinel(effectiveKind(f.GoType, f.Kind)))
	}
	switch f.Kind {
	case KindString:
		primScan("String")
	case KindBool:
		primScan("Bool")
	case KindInt, KindInt8, KindInt16, KindInt32:
		widenedScan("int64", "iv", "Int64", f.GoType)
	case KindInt64:
		primScan("Int64")
	case KindUint, KindUint8, KindUint16, KindUint32:
		widenedScan("uint64", "uv", "Uint64", f.GoType)
	case KindUint64:
		primScan("Uint64")
	case KindFloat32:
		widenedScan("float64", "fv", "Float64", "float32")
	case KindFloat64:
		primScan("Float64")
	case KindStruct:
		if isGenerated(f.GoType) {
			fmt.Fprintf(b, `%[1]s, err = %[1]s.`+streamDecodeCallFor(f.GoType)+`
%[2]s`, ref, nestedDecodeErrCheck(fieldLit(f), calleeTypeOf(f), f.MultiErr, false, ""))
		} else {
			b.WriteString(renderCrossPkgStructStreamDecode(f, ref, posVar))
		}
	case KindSlice:
		renderStreamSlice(b, f, ref, posVar)
	case KindArray:
		emitStreamSliceRead(b, f, ref, posVar, 0)
	case KindMap:
		renderStreamMap(b, f, ref, posVar)
	case KindBytes:
		renderStreamBytes(b, f, ref, posVar)
	case KindTime:
		renderStreamTime(b, f, ref, posVar)
	case KindDuration:
		renderStreamDuration(b, f, ref, posVar)
	case KindNetIP:
		renderStreamNetIP(b, f, ref, posVar)
	case KindNetipAddr:
		renderStreamNetipAddr(b, f, ref, posVar)
	case KindNetipPrefix:
		renderStreamNetipPrefix(b, f, ref, posVar)
	case KindRawJSON:
		renderStreamRawJSON(b, f, ref, posVar)
	case KindURL:
		renderStreamURL(b, f, ref, posVar)
	case KindBigInt:
		renderStreamBigInt(b, f, ref, posVar)
	case KindBigFloat:
		renderStreamBigFloat(b, f, ref, posVar)
	case KindBigRat:
		renderStreamBigRat(b, f, ref, posVar)
	case KindSQLNull:
		renderStreamSQLNull(b, f, ref, posVar)
	case KindAny:
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

func renderStreamSlice(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	emitStreamSliceRead(b, f, ref, posVar, 0)
}

// emitStreamSliceRead is the streaming counterpart of emitByteSliceRead.
func emitStreamSliceRead(b *bytes.Buffer, f FieldInfo, dst, posVar string, depth int) {
	_ = posVar // stream-path uses s.Pos directly
	isArray := f.Kind == KindArray
	arrayN := f.ArrayLen
	// Multi-level pointer element — same cascade as the bytes path; slab is
	// depth-1 only.
	_, elemDepth := elemPtrType(f)
	mptr := elemDepth >= 2
	ivar := fmt.Sprintf("idx%d", depth)
	slabVar := fmt.Sprintf("slab%d", depth)
	field := fieldLit(f)
	chk := streamErrCheck(field)
	rm := streamReadMore(field, "0", false, "ggen.ErrBadArray")
	rmKi := strings.Replace(streamReadMore(field, "0", false, "ggen.ErrBadLiteral"), "if s.Pos >=", "if s.Pos+ki >=", 1)
	// At dispatch level flat-break; a NullDone nested slot skips the peek.
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
	if isArray {
		fmt.Fprintf(b, "var %s int\n", ivar)
		if f.ElemPointer && !mptr {
			fmt.Fprintf(b, "%s := make([]%s, %d)\n", slabVar, f.ElemType, arrayN)
		}
	} else {
		sCap, slCap := preallocCap(f)
		// dst is decode-into-receiver: nil (fresh) or [:0]'d (backing reused).
		// Allocate only when nil.
		if f.ElemPointer && !mptr {
			fmt.Fprintf(b, "var %s []%s\n", slabVar, f.ElemType)
		}
		makeExpr := fmt.Sprintf("%s{}", f.GoType)
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
	// Generated struct elems decode IN PLACE — see emitByteSliceRead.
	directStruct := f.ElemKind == KindStruct && isGenerated(f.ElemType)
	b.WriteString("for s.Bytes()[s.Pos] != ']' {\n")
	if isArray {
		fmt.Fprintf(b, "if %s >= %d { return result, %s }\n",
			ivar, arrayN,
			arrayLenErr(f.JSONName, arrayN, ivar, ""))
	}
	if f.ElemPointer && !mptr {
		// `null` element → nil pointer; skip the parse + slab work.
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
	// Compute the in-place target; slice cases pre-grow the slot here.
	var target string
	switch {
	case isArray && mptr:
		target = fmt.Sprintf("%s[%s]", dst, ivar)
	case mptr:
		// Reslice within cap so the carried pointer chain survives into the
		// slot and the cascade can reuse it; a past-cap grow starts nil.
		if elemPtrReusable(f) {
			fmt.Fprintf(b, "if len(%[1]s) < cap(%[1]s) { %[1]s = %[1]s[:len(%[1]s)+1] } else { %[1]s = append(%[1]s, nil) }\n", dst)
		} else {
			fmt.Fprintf(b, "%s = append(%s, nil)\n", dst, dst)
		}
		target = fmt.Sprintf("%s[len(%s)-1]", dst, dst)
	case isArray && f.ElemPointer:
		target = fmt.Sprintf("%s[%s]", slabVar, ivar)
	case isArray:
		// Blank a mergeable slot first — see emitByteSliceRead.
		if f.ElemKind == KindStruct && !f.ElemPointer && !directStruct {
			fmt.Fprintf(b, "%s[%s] = %s\n", dst, ivar, zeroLit(f.ElemType, f.ElemKind))
		}
		target = fmt.Sprintf("%s[%s]", dst, ivar)
	case f.ElemPointer:
		emitElemGrow(b, slabVar, f, directStruct)
		target = fmt.Sprintf("%s[len(%s)-1]", slabVar, slabVar)
	case f.ElemKind == KindSlice:
		// Reslice within cap to keep the carried header — see emitByteSliceRead.
		fmt.Fprintf(b, "if len(%[1]s) < cap(%[1]s) { %[1]s = %[1]s[:len(%[1]s)+1] } else { %[1]s = append(%[1]s, nil) }\n", dst)
		target = fmt.Sprintf("%s[len(%s)-1]", dst, dst)
	default:
		emitElemGrow(b, dst, f, directStruct)
		target = fmt.Sprintf("%s[len(%s)-1]", dst, dst)
	}
	if mptr {
		// Elem rules ride inside the cascade; no slab/post-decode here. Slice
		// slots are pre-grown nil (TargetNil); ARRAY slots are overwritten
		// too — the contract is "every slot overwritten", and seeding from
		// the carried chain merged `[2]**Inner` where `[]**Inner` and
		// `[2]*Inner` both decoded fresh.
		pf := elemPtrField(f, f.JSONName+"[]")
		// An ARRAY slot keeps the assignment-only cascade: its contract is
		// "every slot overwritten", pinned by TestMerge_ArraySlotsOverwrite.
		pf.TargetNil = isArray || !elemPtrReusable(f)
		b.WriteString(renderStreamField(pf, target, "s.Pos"))
	} else {
		// Named-primitive ELEMENT — see emitByteSliceRead.
		elemCast, elemTarget := "", ""
		savedElemType, savedElemKind := f.ElemType, f.ElemKind
		if prim, kind, ok := inlineNamedPrim(elemAsField(f)); ok {
			elemCast, elemTarget = f.ElemType, target
			target = fmt.Sprintf("namedElem%d", depth)
			fmt.Fprintf(b, "var %s %s\n", target, prim)
			f.ElemType, f.ElemKind = prim, kind
		}
		switch f.ElemKind {
		case KindString:
			fmt.Fprintf(b, `%s, err = s.String(%s)
%s`, target, vArg(f), chk)
		case KindBool:
			fmt.Fprintf(b, `%s, err = s.Bool()
%s`, target, chk)
		case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
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
		case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
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
		case KindFloat32, KindFloat64:
			if f.ElemKind == KindFloat64 {
				fmt.Fprintf(b, `%s, err = s.Float64()
%s`, target, chk)
			} else {
				fmt.Fprintf(b, `var fv float64
fv, err = s.Float64()
%[2]s%[3]s%[1]s = float32(fv)
`, target, chk,
					narrowFloatGuard("fv", "float32", fmt.Sprintf("return result, ggen.NewParseErr(%s, s.Offset(), ggen.ErrNumberOverflow)", field)))
			}
		case KindStruct:
			if directStruct {
				fmt.Fprintf(b, `%[1]s, err = %[1]s.`+streamDecodeCallFor(f.ElemType)+`
%[2]s`, target, nestedDecodeErrCheck(fieldLit(f), calleeTypeOf(f), f.MultiErr, false, ""))
			} else {
				// Cross-package element: the bytes path used to skip it and
				// this path emitted nothing at all.
				b.WriteString(renderCrossPkgStructStreamDecode(elemAsField(f), target, ""))
			}
		case KindSlice, KindArray:
			inner := peelSliceField(f)
			if inner.Kind == KindSlice && len(f.ElemMods) == 0 {
				// `null` elem → nil slot handled HERE so the inner body isn't
				// nested in an else (mirrors the []*T nil-elem fast path).
				fmt.Fprintf(b, `%[2]sif s.Bytes()[s.Pos] == 'n' {
	for ki := 1; ki < 4; ki++ {
		%[3]sif s.Bytes()[s.Pos+ki] != "null"[ki] { return result, ggen.NewParseErr(%[1]s, s.Offset(), ggen.ErrBadLiteral) }
	}
	s.Pos += 4
`, field, rm, rmKi)
				// Slot may carry a reused/receiver header — nil unconditionally.
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
			// Hoist the nested slot into `rowN` (seeded from the carried slot
			// for backing reuse); publish with `target = rowN`. See emitByteSliceRead.
			row := fmt.Sprintf("row%d", depth)
			fmt.Fprintf(b, "%s := %s\n", row, target)
			if inner.Kind == KindSlice {
				fmt.Fprintf(b, "if %[1]s != nil { %[1]s = %[1]s[:0] }\n", row)
			}
			emitStreamSliceRead(b, inner, row, "s.Pos", depth+1)
			fmt.Fprintf(b, "%s = %s\n", target, row)
		default:
			// Dedicated-kind element — same field-level emitter the struct
			// level uses; see the bytes twin in emitByteSliceRead.
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
		// Slab slot decoded in-place; publish its address.
		fmt.Fprintf(b, "%[1]s[%[2]s] = &%[3]s[%[2]s]\n%[2]s++\n", dst, ivar, slabVar)
	case isArray:
		fmt.Fprintf(b, "%s++\n", ivar)
	case f.ElemPointer && !mptr:
		// Slab tail decoded in-place; publish addr.
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
		b.WriteString("}\n") // close else (null-check)
	}
}
