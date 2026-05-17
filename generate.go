package main

import (
	"bytes"
	"fmt"
	"go/format"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// filePool recycles the bytes.Buffer that holds the entire rendered
// template before goimports runs over it. One per generate() call —
// pooling matters when the binary processes many packages in one walk.
var filePool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// smallPool recycles bytes.Buffer for the per-renderer temp buffers.
// bytes.Buffer.Reset() truncates buf to [:0] so the underlying array's
// capacity carries across Get/Put cycles — first call after a Put hits
// the warm-cap fast path with no grow allocs. bytes.Buffer.String()
// returns a fresh copy of the bytes (unlike strings.Builder.String()
// which aliases), so reusing the buf after Put can't corrupt prior
// callers' returned strings.
var smallPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func getSmall() *bytes.Buffer {
	b := smallPool.Get().(*bytes.Buffer)
	b.Reset()
	return b
}

func putSmall(b *bytes.Buffer) { smallPool.Put(b) }

func getFileBuf() *bytes.Buffer {
	b := filePool.Get().(*bytes.Buffer)
	b.Reset()
	return b
}

func putFileBuf(b *bytes.Buffer) { filePool.Put(b) }

// generate renders the full set of structs into Go source. The bytes
// path materializes the result in memory (used by tests and the
// formatted-output path that hands the buffer to go/format.Source).
// The streaming sibling generateTo writes the prelude and each
// per-struct body to w as separate calls — saving one large copy
// when format.Source is disabled.
func generate(pkg string, structs []StructInfo) ([]byte, error) {
	var buf bytes.Buffer
	if err := generateTo(&buf, pkg, structs); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func generateTo(w io.Writer, pkg string, structs []StructInfo) error {
	resetOneofRegistry()
	// `generatedTypes` is populated by the caller before invoking generate.
	// It reflects every struct in the same Go package the caller is about
	// to emit (across ALL build-tag buckets, not just this one), so a
	// nested-struct reference that crosses bucket boundaries still routes
	// to a direct DecodeFrom call instead of falling back to encoding/json.
	// Falls back to per-bucket population when the caller didn't seed it.
	if generatedTypes == nil {
		generatedTypes = make(map[string]struct{}, len(structs))
		generatedAliasKinds = make(map[string]TypeKind)
		for _, s := range structs {
			generatedTypes[s.Name] = struct{}{}
			if s.IsAlias && kindPrimitiveName(s.AliasKind) != "" {
				generatedAliasKinds[s.Name] = s.AliasKind
			}
		}
	}
	// Default: emit struct fields alphabetically by JSON name. This is a
	// codegen-time reorder of s.Fields — zero runtime cost, deterministic
	// wire output, compresses better. Opt out with //ggen:generate nosortkeys.
	// Inline fields stay at the end (they splice map entries at marshal time
	// and need to come after the fixed fields to keep comma emission tidy).
	for i := range structs {
		if structs[i].NoSort {
			continue
		}
		slices.SortStableFunc(structs[i].Fields, func(a, b FieldInfo) int {
			switch {
			case a.Inline && !b.Inline:
				return 1
			case !a.Inline && b.Inline:
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

	// Render each struct into its own pool-borrowed buffer. The bodies
	// concatenate into the file (or stream straight to it) in struct
	// order after the prelude. Per-struct buffers (rather than one big
	// shared one) keep this loop straightforward and let the streaming
	// noFormat path avoid an extra copy.
	bodies := make([]*bytes.Buffer, len(structs))
	for i := range structs {
		b := getSmall()
		renderStructMethods(b, structs[i])
		bodies[i] = b
	}

	// Imports come from StructInfo features, not a body scan — so the
	// full prelude (build-tag + generated marker + package decl +
	// sorted import block) can be emitted before any struct body.
	prelude := buildPrelude(pkg, tag, collectImports(structs))

	if noFormat {
		// Stream prelude → oneOf decls → each per-struct body straight
		// to w. Each Write is its own syscall (for *os.File) — the
		// caller doesn't pay for one big buf-into-file copy.
		if _, err := w.Write(prelude); err != nil {
			return err
		}
		for _, decl := range oneofRegistry.decls {
			if _, err := io.WriteString(w, decl+"\n"); err != nil {
				return err
			}
		}
		for _, body := range bodies {
			_, err := w.Write(body.Bytes())
			putSmall(body)
			if err != nil {
				return err
			}
		}
		return nil
	}

	// format.Source needs the whole file in one []byte. Build a single
	// buffer, format it, then write the formatted result to w.
	buf := getFileBuf()
	defer putFileBuf(buf)
	buf.Write(prelude)
	for _, decl := range oneofRegistry.decls {
		buf.WriteString(decl)
		buf.WriteByte('\n')
	}
	for _, body := range bodies {
		buf.Write(body.Bytes())
		putSmall(body)
	}
	src, err := format.Source(buf.Bytes())
	if err != nil {
		return fmt.Errorf("formatting generated code: %w\n\nraw output:\n%s", err, buf.Bytes())
	}
	_, err = w.Write(src)
	return err
}

// buildPrelude emits the file header (build-tag, generated marker,
// package decl, import block). Returns the assembled []byte. Caller
// concatenates with the rendered body before formatting.
func buildPrelude(pkg, buildTag string, imports []string) []byte {
	// Worst-case 256 bytes for the fixed parts (marker, package) + 64
	// bytes per import path. Pre-sizes the underlying array so the
	// appends don't trigger the geometric grow chain.
	cap := 256 + len(imports)*64 + len(buildTag)
	out := make([]byte, 0, cap)
	out = append(out, "// Code generated by ggen; DO NOT EDIT.\n\n"...)
	if buildTag != "" {
		out = append(out, "//go:build "...)
		out = append(out, buildTag...)
		out = append(out, "\n\n"...)
	}
	out = append(out, "package "...)
	out = append(out, pkg...)
	out = append(out, "\n\n"...)
	return appendImportBlock(out, imports)
}

// renderStructMethods writes the full method set for a single struct
// directly to buf. The set is fixed (DecodeFrom + DecodeStreamFrom +
// JSONSize + AppendJSON, plus optional MarshalJSON / UnmarshalJSON
// hooks) so a hand-rolled write is cheaper than the prior text/template
// dispatch through reflect.Value.Call.
func renderStructMethods(buf *bytes.Buffer, s StructInfo) {
	fmt.Fprintf(buf, "func (%s) DecodeFrom(data []byte, i int) (%s, int, error) {\n", s.Name, s.Name)
	renderDecode(buf, s)
	buf.WriteString("}\n\n")

	fmt.Fprintf(buf, "func (%s) DecodeStreamFrom(s *scan.Stream, i int) (%s, int, error) {\n", s.Name, s.Name)
	renderStreamDecode(buf, s)
	buf.WriteString("}\n\n")

	fmt.Fprintf(buf, "func (s %s) JSONSize() int {\n", s.Name)
	renderSize(buf, s)
	buf.WriteString("}\n\n")

	fmt.Fprintf(buf, "func (s %s) AppendJSON(dst []byte) ([]byte, error) {\n", s.Name)
	renderAppendJSON(buf, s)
	buf.WriteString("}\n\n")

	if s.Marshal {
		fmt.Fprintf(buf, "func (s %s) MarshalJSON() ([]byte, error) {\n\treturn encode.Marshal(s)\n}\n\n", s.Name)
	}
	if s.Unmarshal {
		fmt.Fprintf(buf, "func (s *%s) UnmarshalJSON(data []byte) error {\n\tv, err := decode.Unmarshal[%s](data)\n\tif err != nil {\n\t\treturn err\n\t}\n\t*s = v\n\treturn nil\n}\n\n", s.Name, s.Name)
	}
}

// collectImports walks structs and returns the sorted set of import
// paths the generated file will reference. Driven entirely off
// StructInfo features (kinds, validation rules, mod rules, alias
// underlyings, custom-func packages) — no body-scan required, so the
// prelude can be assembled before tmpl.Execute and the body can be
// appended in place.
func collectImports(structs []StructInfo) []string {
	need := map[string]struct{}{
		// scan + encode are emitted by every generated method. They're
		// always referenced: scan.X (or s.X for stream) in DecodeFrom,
		// encode.AppendStringNoHTML / encode.AppendAny in AppendJSON.
		"github.com/sirkostya009/ggen/scan":   {},
		"github.com/sirkostya009/ggen/encode": {},
		// strconv: every primitive marshal uses strconv.AppendBool/
		// AppendInt/AppendUint/AppendFloat. Even a "string only" struct
		// hits strconv via AppendString helpers internally? No — but
		// the JSONSize / AppendJSON code path always loads strconv for
		// `,"":` length and primitive append calls; safer to always
		// include. format.Source would strip if unused, but we don't
		// run it for free — keep precise.
	}
	add := func(p string) {
		if p != "" {
			need[p] = struct{}{}
		}
	}

	// Per-feature walk: each match flips on its imports. Stream
	// decode + AppendJSON branches share the same kind dispatch as
	// the byte path, so any flag set here covers both.
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
		// Skip when IsAlias was flipped off during parse (field
		// introspection mode) — the generated code drives the alias's
		// exported fields directly and never names the underlying.
		if s.IsAlias && s.AliasKind == KindStruct {
			add(s.AliasUnderlyingImport)
		}
		// streamUnknownKey emits strings.Clone(key) in every mode
		// except pure -ignoreunknown (no inline catch-all). Add the
		// import here so the generated file links.
		if !s.IsAlias && (s.InlineField().Inline || !s.IgnoreUnknown) {
			add("strings")
		}
		for _, f := range s.Fields {
			collectFieldImports(f, add, &anyString, &anyValidation, &anyBytes, &anyRequired)
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
	if anyMarshal {
		// MarshalJSON hook is `return encode.Marshal(s)` — encode is
		// already in. No extra path needed.
		_ = anyMarshal
	}
	if anyUnmarshal {
		// UnmarshalJSON hook uses decode.Unmarshal[T].
		add("github.com/sirkostya009/ggen/decode")
	}
	if anyValidation || anyRequired {
		add("github.com/sirkostya009/ggen/decode/validation")
	}
	// strconv: needed for every primitive append on encode, every
	// strconv.Parse* on string-tag decode. Treat as always-on since
	// every realistic struct has at least one numeric or bool field
	// at AppendJSON time even if no decode-side fields use it. Strictly
	// precise checks would walk per-kind; the marginal benefit (skipping
	// strconv for pure-string structs) isn't worth the complexity.
	add("strconv")
	// unsafe: every inline string scan uses unsafe.String + unsafe.SliceData.
	// Any string field/key triggers it. Also used by big.Int/Float/Rat
	// SetString paths.
	if anyString {
		add("unsafe")
	}
	out := make([]string, 0, len(need))
	for p := range need {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
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
			case "email", "url", "ascii", "printable", "alphanum",
				"numeric", "lower", "upper", "hexadecimal":
				// decode.IsEmail / decode.IsURL / decode.IsASCII / ...
				add("github.com/sirkostya009/ggen/decode")
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
			case "trim", "lower", "upper", "trimleft", "trimright", "replace":
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
		// Resolved + has a known method → uses that method, no json.
		if iface.Resolved &&
			(iface.ByteDecoder || iface.JSONUnmarshaler || iface.TextUnmarshaler ||
				iface.AppendJSON || iface.JSONMarshaler || iface.TextAppender ||
				iface.TextMarshaler) {
			return
		}
		// AST-only or no method → encoding/json fallback.
		add("encoding/json")
	}
	switch f.Kind {
	case KindString:
		*anyString = true
	case KindTime, KindDuration:
		add("time")
	case KindNetIP:
		add("net")
		add("fmt")
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
		if spec, ok := SQLNullSpec(f.GoType); ok {
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
		crossPkgStruct(f.GoType, f.Iface)
	case KindSlice, KindArray:
		// Slice / array elements may themselves be cross-pkg structs
		// (json.Marshal / json.Unmarshal fallback in
		// emitSliceElement and emitByteSliceRead).
		if f.ElemKind == KindStruct {
			crossPkgStruct(f.ElemType, f.ElemIface)
		}
	case KindMap:
		// map[K]V where V is a cross-pkg struct also takes the
		// json.Unmarshal fallback (renderMap KindStruct branch).
		if f.ElemKind == KindStruct {
			crossPkgStruct(f.ElemType, f.ElemIface)
		}
	case KindAny:
		// encode.AppendAny handles marshal — encode already in.
	case KindRawJSON:
		// RawMessage decode uses scan.SkipValue (already), data[start:i]
		// slice — no extra import.
	}
	if f.String {
		// json:",string" tag: strconv.Parse* on decode, strconv.AppendX
		// on encode (already in). Also inline string scan.
		*anyString = true
	}
}

// appendImportBlock appends a Go `import (...)` block to out, with
// stdlib paths first, blank line, then third-party. Caller guarantees
// paths are sorted alphabetically; the in-category order is preserved
// after partitioning. Empty list appends nothing. Stdlib detection
// mirrors gofmt convention: paths whose first segment has no `.` are
// stdlib.
func appendImportBlock(out []byte, paths []string) []byte {
	if len(paths) == 0 {
		return out
	}
	// Partition first — the sorted input interleaves "encoding/json"
	// with "github.com/..." with "net" etc. alphabetically, so a
	// single-pass scan can't tell where the stdlib group ends.
	stdlib, third := splitStdlibThird(paths)
	out = append(out, "import (\n"...)
	for _, p := range stdlib {
		out = appendQuotedLine(out, p)
	}
	if len(stdlib) > 0 && len(third) > 0 {
		out = append(out, '\n')
	}
	for _, p := range third {
		out = appendQuotedLine(out, p)
	}
	return append(out, ")\n\n"...)
}

// splitStdlibThird partitions paths into (stdlib, third-party),
// preserving the relative order within each group. Caller passes a
// list sorted alphabetically across both groups; each group remains
// sorted in the output.
func splitStdlibThird(paths []string) (stdlib, third []string) {
	stdlib = make([]string, 0, len(paths))
	for _, p := range paths {
		if isThirdParty(p) {
			third = append(third, p)
		} else {
			stdlib = append(stdlib, p)
		}
	}
	return stdlib, third
}

func isThirdParty(p string) bool {
	seg, _, _ := strings.Cut(p, "/")
	return strings.ContainsRune(seg, '.')
}

func appendQuotedLine(out []byte, p string) []byte {
	out = append(out, '\t', '"')
	out = append(out, p...)
	return append(out, '"', '\n')
}

// preregisterOneOfs walks all rules (top-level, dive-elem, key, inner-nested)
// to populate the OneOf frozen-slice registry before render. Order-independent:
// registerOneOf dedupes by joined value, so re-walking during render returns
// the same name. Required for safe parallel struct rendering — once the
// registry is fully populated up front, render-time lookups are pure map reads.
func preregisterOneOfs(structs []StructInfo) {
	walk := func(rules []ValidationRule) {
		for _, v := range rules {
			if v.Name == "oneof" {
				registerOneOf(strings.Split(v.Value, "|"))
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

// renderMods emits post-decode transformation code against ref using the
// field's Mods list. String mods (trim/lower/...) and numeric mods (clamp)
// live here. Unknown names are skipped so validation tests can detect them.
//
// goType + kind let renderMods detect aliased types (e.g. `type AliasString
// string`) and wrap stdlib calls with casts to the underlying primitive,
// since `strings.TrimSpace` won't accept an `AliasString` directly.
// Pass goType="" or goType==<primitive name> when no cast is needed.
func renderMods(mods []ModRule, ref, goType string, kind TypeKind) string {
	if len(mods) == 0 {
		return ""
	}
	// If the field is typed as a primitive alias generated in this same
	// pass (e.g. `type AliasString string`), the field's declared kind
	// is KindStruct (resolveKind doesn't know the underlying), but mods
	// still need to operate on the primitive. Look up the alias's
	// underlying kind in generatedAliasKinds and let the cast logic kick in.
	if k, ok := generatedAliasKinds[goType]; ok {
		kind = k
	}
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
	b := getSmall()
	defer putSmall(b)
	for _, m := range mods {
		if m.Custom {
			call := m.FuncName
			if m.PkgName != "" {
				call = m.PkgName + "." + m.FuncName
			}
			if m.Fallible {
				// Errors propagate as parse errors (same level as
				// scan.ErrBadX), not validation errors — they signal that
				// the input was unprocessable, not just invalid.
				fmt.Fprintf(b, "if v, err := %s(%s); err != nil {\n\treturn result, i, err\n} else {\n\t%s = v\n}\n",
					call, ref, ref)
			} else {
				fmt.Fprintf(b, "%s = %s(%s)\n", ref, call, ref)
			}
			continue
		}
		switch m.Name {
		case "trim":
			fmt.Fprintf(b, "%s = %s\n", ref, wrap(fmt.Sprintf("strings.TrimSpace(%s)", asPrim(ref))))
		case "lower":
			fmt.Fprintf(b, "%s = %s\n", ref, wrap(fmt.Sprintf("strings.ToLower(%s)", asPrim(ref))))
		case "upper":
			fmt.Fprintf(b, "%s = %s\n", ref, wrap(fmt.Sprintf("strings.ToUpper(%s)", asPrim(ref))))
		case "trimleft":
			fmt.Fprintf(b, "%s = %s\n", ref, wrap(fmt.Sprintf("strings.TrimPrefix(%s, %s)", asPrim(ref), strconv.Quote(m.Value))))
		case "trimright":
			fmt.Fprintf(b, "%s = %s\n", ref, wrap(fmt.Sprintf("strings.TrimSuffix(%s, %s)", asPrim(ref), strconv.Quote(m.Value))))
		case "replace":
			parts := strings.SplitN(m.Value, "|", 2)
			if len(parts) == 2 {
				fmt.Fprintf(b, "%s = %s\n", ref,
					wrap(fmt.Sprintf("strings.ReplaceAll(%s, %s, %s)", asPrim(ref), strconv.Quote(parts[0]), strconv.Quote(parts[1]))))
			}
		case "clamp":
			// clamp=lo|hi — bound a numeric (or string, lexicographic) value
			// into [lo, hi]. The comparison operates on the field directly;
			// for aliased numerics, the constants are cast to the field's
			// type so `<` / `>` are comparable.
			lo, hi, ok := strings.Cut(m.Value, "|")
			if !ok {
				continue
			}
			lo = strings.TrimSpace(lo)
			hi = strings.TrimSpace(hi)
			if lo != "" {
				fmt.Fprintf(b, "if %s < %s { %s = %s }\n", ref, wrap(lo), ref, wrap(lo))
			}
			if hi != "" {
				fmt.Fprintf(b, "if %s > %s { %s = %s }\n", ref, wrap(hi), ref, wrap(hi))
			}
		}
	}
	return b.String()
}

// kindPrimitiveName returns the Go literal name for a primitive TypeKind,
// or "" for kinds that aren't a single primitive token. Used by renderMods
// to cast aliased fields through their underlying primitive on stdlib calls.
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

// arrayLenErr builds a typed *validation.LenError literal for the strict
// fixed-array element-count check.
func arrayLenErr(field string, want int, gotExpr string) string {
	return fmt.Sprintf("&validation.LenError{Field: %q, Want: %d, Got: %s}", field, want, gotExpr)
}

// requiredErr builds a typed *validation.RequiredError literal.
func requiredErr(field string) string {
	return fmt.Sprintf("&validation.RequiredError{Field: %q}", field)
}

// oneofRegistry collects unique allowed-string sets emitted as
// package-level frozen slices, so OneOfError construction never
// allocates the slice itself. Reset at the start of each Generate call.
var oneofRegistry struct {
	names map[string]string // joined "a|b|c" → var name
	decls []string
}

func resetOneofRegistry() {
	oneofRegistry.names = map[string]string{}
	oneofRegistry.decls = nil
}

func registerOneOf(parts []string) string {
	key := strings.Join(parts, "\x00")
	if name, ok := oneofRegistry.names[key]; ok {
		return name
	}
	name := fmt.Sprintf("ggenOneof%d", len(oneofRegistry.names))
	oneofRegistry.names[key] = name
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = strconv.Quote(p)
	}
	oneofRegistry.decls = append(oneofRegistry.decls,
		fmt.Sprintf("var %s = []string{%s}", name, strings.Join(quoted, ", ")))
	return name
}

// renderValidationOn emits Go source for validation checks against the
// variable named by ref. jsonName appears in error messages; kind selects
// type-appropriate comparisons; multiErr switches the failure action from
// early return to `errs = append(errs, ...)`.
func renderValidationOn(rules []ValidationRule, ref, jsonName string, kind TypeKind, multiErr bool) string {
	b := getSmall()
	defer putSmall(b)

	// onErr wraps a `&validation.XError{...}` literal into the appropriate
	// failure action: append in multierr mode, early return otherwise.
	onErr := func(errExpr string) string {
		if multiErr {
			return "errs = append(errs, " + errExpr + ")"
		}
		return "return result, " + errExpr
	}

	// vstr is the source expression for the offending Value field of a
	// pattern/length error. For string-typed refs it's just `ref`. For
	// numeric refs that need to be cast to string for the error (none of
	// today's patterns), this would adapt — currently a no-op pass-through.

	for _, v := range rules {
		switch v.Name {
		case "required", "optional":
			// required handled separately; optional is a no-op marker

		case "notempty":
			fmt.Fprintf(b, "if len(%s) == 0 {\n\t%s\n}\n", ref,
				onErr(fmt.Sprintf("&validation.NotEmptyError{Field: %q}", jsonName)))

		case "len":
			fmt.Fprintf(b, "if len(%s) != %s {\n\t%s\n}\n", ref, v.Value,
				onErr(fmt.Sprintf("&validation.LenError{Field: %q, Want: %s, Got: len(%s)}", jsonName, v.Value, ref)))
		case "minlen":
			fmt.Fprintf(b, "if len(%s) < %s {\n\t%s\n}\n", ref, v.Value,
				onErr(fmt.Sprintf("&validation.MinLenError{Field: %q, Limit: %s, Got: len(%s)}", jsonName, v.Value, ref)))
		case "maxlen":
			fmt.Fprintf(b, "if len(%s) > %s {\n\t%s\n}\n", ref, v.Value,
				onErr(fmt.Sprintf("&validation.MaxLenError{Field: %q, Limit: %s, Got: len(%s)}", jsonName, v.Value, ref)))

		case "runes":
			fmt.Fprintf(b, "if utf8.RuneCountInString(%s) != %s {\n\t%s\n}\n", ref, v.Value,
				onErr(fmt.Sprintf("&validation.RunesError{Field: %q, Want: %s, Got: utf8.RuneCountInString(%s)}", jsonName, v.Value, ref)))
		case "minrunes":
			fmt.Fprintf(b, "if utf8.RuneCountInString(%s) < %s {\n\t%s\n}\n", ref, v.Value,
				onErr(fmt.Sprintf("&validation.MinRunesError{Field: %q, Limit: %s, Got: utf8.RuneCountInString(%s)}", jsonName, v.Value, ref)))
		case "maxrunes":
			fmt.Fprintf(b, "if utf8.RuneCountInString(%s) > %s {\n\t%s\n}\n", ref, v.Value,
				onErr(fmt.Sprintf("&validation.MaxRunesError{Field: %q, Limit: %s, Got: utf8.RuneCountInString(%s)}", jsonName, v.Value, ref)))

		case "gt":
			fmt.Fprintf(b, "if %s <= %s {\n\t%s\n}\n", ref, v.Value,
				onErr(fmt.Sprintf("&validation.GTError{Field: %q, Limit: %s, Value: %s}", jsonName, v.Value, ref)))
		case "gte":
			fmt.Fprintf(b, "if %s < %s {\n\t%s\n}\n", ref, v.Value,
				onErr(fmt.Sprintf("&validation.GTEError{Field: %q, Limit: %s, Value: %s}", jsonName, v.Value, ref)))
		case "lt":
			fmt.Fprintf(b, "if %s >= %s {\n\t%s\n}\n", ref, v.Value,
				onErr(fmt.Sprintf("&validation.LTError{Field: %q, Limit: %s, Value: %s}", jsonName, v.Value, ref)))
		case "lte":
			fmt.Fprintf(b, "if %s > %s {\n\t%s\n}\n", ref, v.Value,
				onErr(fmt.Sprintf("&validation.LTEError{Field: %q, Limit: %s, Value: %s}", jsonName, v.Value, ref)))

		case "eq":
			if kind == KindString {
				fmt.Fprintf(b, "if %s != %q {\n\t%s\n}\n",
					ref, v.Value,
					onErr(fmt.Sprintf("&validation.EqError{Field: %q, Want: %q, Value: %s}", jsonName, v.Value, ref)))
			} else if isNumeric(kind) {
				fmt.Fprintf(b, "if %s != %s {\n\t%s\n}\n",
					ref, v.Value,
					onErr(fmt.Sprintf("&validation.EqError{Field: %q, Want: %s, Value: %s}", jsonName, v.Value, ref)))
			}
		case "neq":
			if kind == KindString {
				fmt.Fprintf(b, "if %s == %q {\n\t%s\n}\n",
					ref, v.Value,
					onErr(fmt.Sprintf("&validation.NeqError{Field: %q, Want: %q, Value: %s}", jsonName, v.Value, ref)))
			} else if isNumeric(kind) {
				fmt.Fprintf(b, "if %s == %s {\n\t%s\n}\n",
					ref, v.Value,
					onErr(fmt.Sprintf("&validation.NeqError{Field: %q, Want: %s, Value: %s}", jsonName, v.Value, ref)))
			}

		case "oneof":
			cases := renderOneofCases(kind, v.Value)
			parts := strings.Split(v.Value, "|")
			varName := registerOneOf(parts)
			fmt.Fprintf(b, "switch %s {\ncase %s:\ndefault:\n\t%s\n}\n",
				ref, cases,
				onErr(fmt.Sprintf("&validation.OneOfError{Field: %q, Allowed: %s, Value: %s}", jsonName, varName, ref)))

		case "email":
			fmt.Fprintf(b, "if !decode.IsEmail(%s) {\n\t%s\n}\n", ref,
				onErr(fmt.Sprintf("&validation.EmailError{Field: %q, Value: %s}", jsonName, ref)))
		case "url":
			fmt.Fprintf(b, "if !decode.IsURL(%s) {\n\t%s\n}\n", ref,
				onErr(fmt.Sprintf("&validation.URLError{Field: %q, Value: %s}", jsonName, ref)))

		case "ascii":
			fmt.Fprintf(b, "if !decode.IsASCII(%s) {\n\t%s\n}\n", ref,
				onErr(fmt.Sprintf("&validation.ASCIIError{Field: %q, Value: %s}", jsonName, ref)))
		case "printable":
			fmt.Fprintf(b, "if !decode.IsPrintable(%s) {\n\t%s\n}\n", ref,
				onErr(fmt.Sprintf("&validation.PrintableError{Field: %q, Value: %s}", jsonName, ref)))
		case "alphanum":
			fmt.Fprintf(b, "if !decode.IsAlphanum(%s) {\n\t%s\n}\n", ref,
				onErr(fmt.Sprintf("&validation.AlphanumError{Field: %q, Value: %s}", jsonName, ref)))
		case "numeric":
			fmt.Fprintf(b, "if !decode.IsNumeric(%s) {\n\t%s\n}\n", ref,
				onErr(fmt.Sprintf("&validation.NumericError{Field: %q, Value: %s}", jsonName, ref)))
		case "lower":
			fmt.Fprintf(b, "if !decode.IsLower(%s) {\n\t%s\n}\n", ref,
				onErr(fmt.Sprintf("&validation.LowerError{Field: %q, Value: %s}", jsonName, ref)))
		case "upper":
			fmt.Fprintf(b, "if !decode.IsUpper(%s) {\n\t%s\n}\n", ref,
				onErr(fmt.Sprintf("&validation.UpperError{Field: %q, Value: %s}", jsonName, ref)))
		case "hexadecimal":
			fmt.Fprintf(b, "if !decode.IsHex(%s) {\n\t%s\n}\n", ref,
				onErr(fmt.Sprintf("&validation.HexadecimalError{Field: %q, Value: %s}", jsonName, ref)))

		case "starts":
			fmt.Fprintf(b, "if !strings.HasPrefix(%s, %q) {\n\t%s\n}\n",
				ref, v.Value,
				onErr(fmt.Sprintf("&validation.StartsError{Field: %q, Want: %q, Value: %s}", jsonName, v.Value, ref)))
		case "ends":
			fmt.Fprintf(b, "if !strings.HasSuffix(%s, %q) {\n\t%s\n}\n",
				ref, v.Value,
				onErr(fmt.Sprintf("&validation.EndsError{Field: %q, Want: %q, Value: %s}", jsonName, v.Value, ref)))
		case "contains":
			fmt.Fprintf(b, "if !strings.Contains(%s, %q) {\n\t%s\n}\n",
				ref, v.Value,
				onErr(fmt.Sprintf("&validation.ContainsError{Field: %q, Want: %q, Value: %s}", jsonName, v.Value, ref)))

		case "multiple":
			fmt.Fprintf(b, "if %s %% %s != 0 {\n\t%s\n}\n", ref, v.Value,
				onErr(fmt.Sprintf("&validation.MultipleError{Field: %q, Of: %s, Value: %s}", jsonName, v.Value, ref)))

		default:
			// `@FuncName` (or `@pkg.FuncName`) — user-defined validator
			// resolved at parse time. Direct call into the user's function;
			// non-nil return wraps as validation.CustomError.
			if v.Custom {
				call := v.FuncName
				if v.PkgName != "" {
					call = v.PkgName + "." + v.FuncName
				}
				fmt.Fprintf(b, "if err := %s(%s); err != nil {\n\t%s\n}\n",
					call, ref,
					onErr(fmt.Sprintf("&validation.CustomError{Field: %q, Name: %q, Cause: err}", jsonName, v.Name)))
			}
			// Unknown non-custom names are silently ignored — keeps the
			// parser tolerant of forward-compatible rule additions.
		}
	}
	return b.String()
}

func renderOneofCases(kind TypeKind, raw string) string {
	parts := strings.Split(raw, "|")
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

// renderAppendJSON emits the body of an AppendJSON method: a series of
// append/helper calls that emit the struct as JSON into dst.
// fieldIsConditional reports whether the field's marshal emission depends on
// a runtime check (omitempty/omitzero). Fields without skip conditions are
// always emitted.
func fieldIsConditional(f FieldInfo) bool {
	if f.Inline {
		return true
	}
	if !f.OmitEmpty && !f.OmitZero {
		return false
	}
	return fieldSkipExpr(f, "s."+f.GoName) != ""
}

// fieldSkipExpr returns a Go boolean expression that evaluates to true when
// the field should be EMITTED. Empty string means "always emit".
func fieldSkipExpr(f FieldInfo, ref string) string {
	// omitempty: skip if JSON-empty (null, "", [], {}).
	// omitzero: skip if Go zero value.
	var emitConds []string
	if f.OmitEmpty {
		if f.Pointer {
			emitConds = append(emitConds, fmt.Sprintf("%s != nil", ref))
		} else {
			switch f.Kind {
			case KindString:
				emitConds = append(emitConds, fmt.Sprintf("%s != \"\"", ref))
			case KindSlice, KindMap:
				emitConds = append(emitConds, fmt.Sprintf("len(%s) > 0", ref))
			case KindRawJSON:
				// Underlying []byte; nil/empty → skip. Strict v2 also
				// considers literal `null`/`[]`/`{}` content JSON-empty;
				// we don't peek into the bytes — nil/len-0 is the bound.
				emitConds = append(emitConds, fmt.Sprintf("len(%s) > 0", ref))
			case KindAny:
				emitConds = append(emitConds, fmt.Sprintf("%s != nil", ref))
			case KindSQLNull:
				// jsonv2: NullX.MarshalJSON returns `null` when !Valid,
				// which is JSON-empty → omit. Match by emitting only when Valid.
				emitConds = append(emitConds, fmt.Sprintf("%s.Valid", ref))
			case KindBigInt, KindBigFloat, KindBigRat:
				// jsonv2: big.X.MarshalJSON of zero returns `"0"` (or `"0/1"`
				// for Rat) which is JSON-empty (zero number). Skip the zero.
				emitConds = append(emitConds, fmt.Sprintf("%s.Sign() != 0", ref))
			}
		}
	}
	if f.OmitZero {
		if f.Pointer {
			emitConds = append(emitConds, fmt.Sprintf("%s != nil", ref))
		} else {
			switch f.Kind {
			case KindString:
				emitConds = append(emitConds, fmt.Sprintf("%s != \"\"", ref))
			case KindBool:
				emitConds = append(emitConds, ref)
			case KindInt, KindInt8, KindInt16, KindInt32, KindInt64,
				KindUint, KindUint8, KindUint16, KindUint32, KindUint64,
				KindFloat32, KindFloat64:
				emitConds = append(emitConds, fmt.Sprintf("%s != 0", ref))
			case KindSlice, KindBytes, KindMap, KindRawJSON:
				// jsonv2 omitzero: Go-zero of a slice/map is nil, not empty.
				// `make([]T, 0)` is non-nil and must be emitted.
				emitConds = append(emitConds, fmt.Sprintf("%s != nil", ref))
			case KindStruct:
				emitConds = append(emitConds, fmt.Sprintf("%s != (%s{})", ref, f.GoType))
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
				// Go-zero for sql.NullX is the all-fields-zero literal,
				// which is comparable (Valid: false, X: zero).
				emitConds = append(emitConds, fmt.Sprintf("%s != (%s{})", ref, f.GoType))
			case KindURL:
				// url.URL is a comparable struct.
				emitConds = append(emitConds, fmt.Sprintf("%s != (%s{})", ref, f.GoType))
			case KindBigInt, KindBigFloat, KindBigRat:
				// big.Int/Float/Rat have unexported slices, not comparable.
				// Sign() returns 0 for the zero value — cheap and reliable.
				emitConds = append(emitConds, fmt.Sprintf("%s.Sign() != 0", ref))
			case KindArray:
				// [N]T comparable when element kind is comparable. Use the
				// type literal for the all-zero comparison.
				emitConds = append(emitConds, fmt.Sprintf("%s != (%s{})", ref, f.GoType))
			}
		}
	}
	if len(emitConds) == 0 {
		return ""
	}
	// Join with && (if any skip rule says skip, skip).
	return strings.Join(slices.Compact(emitConds), " && ")
}

func renderAppendJSON(b *bytes.Buffer, s StructInfo) {
	if s.IsAlias {
		b.WriteString(renderAliasAppendJSON(s))
		return
	}
	// coalesceConstAppends operates on the assembled body string — fold
	// adjacent constant-byte appends post-emit, then ship the result
	// into the caller's builder.
	var body bytes.Buffer
	renderAppendJSONBody(&body, s)
	b.WriteString(coalesceConstAppends(body.String()))
}

// coalesceConstAppends merges adjacent constant-bytes append lines into a
// single append. Recognizes both `dst = append(dst, ...)` and the
// terminating `return append(dst, ...), nil` form, so the trailing `}` of
// a struct gets folded into the prior `]`/`}` etc.
//
// Single-byte payloads emit as `'X'` (char literal); multi-byte as
// `"…"...` (string spread). The compiler treats them identically but
// char-literal form is more idiomatic for one byte.
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
	// `'a', 'b', '\n'` — split by `,` is wrong when the literal IS a
	// comma. Walk char literals manually, respecting `\X` escapes.
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

	// `var err error` is declared up front so emitters can do
	// `dst, err = X.AppendJSON(dst); if err != nil { return dst, err }`
	// without having to redeclare per call site. Marked with `_ = err` if
	// no fallible call ends up using it (e.g. struct of pure primitives).
	b.WriteString("var err error\n_ = err\n")

	// Detect if any field is conditional; if none, use the path that
	// hard-codes `{` into the first field's prefix and commas before the rest.
	anyConditional := slices.ContainsFunc(s.Fields, fieldIsConditional)

	if !anyConditional {
		for i, f := range s.Fields {
			prefix := `,"` + f.JSONName + `":`
			if i == 0 {
				prefix = `{"` + f.JSONName + `":`
			}
			ref := "s." + f.GoName
			if newPrefix, code, ok := foldLeadingQuote(f, ref, prefix); ok {
				fmt.Fprintf(b, "dst = append(dst, %q...)\n", newPrefix)
				b.WriteString(code)
			} else {
				fmt.Fprintf(b, "dst = append(dst, %q...)\n", prefix)
				b.WriteString(renderAppendValue(f, ref))
			}
		}
		b.WriteString("return append(dst, '}'), nil")
		return
	}

	// Conditional path: track emissions via len(dst) vs start.
	b.WriteString("dst = append(dst, '{')\nstart := len(dst)\n")
	for _, f := range s.Fields {
		ref := "s." + f.GoName
		if f.Inline {
			valEmit := "if dst, err = encode.AppendAny(dst, v); err != nil { return dst, err }\n"
			if f.ElemType == "jsontext.Value" {
				valEmit = "dst = append(dst, v...)\n"
			}
			fmt.Fprintf(b, `{
for k, v := range %[1]s {
if len(dst) > start { dst = append(dst, ',') }
dst = append(dst, '"')
dst = %[2]s(dst, k)
dst = append(dst, ':')
%[3]s}
}
`, ref, appendStrFn(f.HTMLEscape), valEmit)
			continue
		}
		emit := fieldSkipExpr(f, ref)
		if emit != "" {
			fmt.Fprintf(b, "if %s {\n", emit)
		}
		b.WriteString("if len(dst) > start { dst = append(dst, ',') }\n")
		prefix := `"` + f.JSONName + `":`
		if newPrefix, code, ok := foldLeadingQuote(f, ref, prefix); ok {
			fmt.Fprintf(b, "dst = append(dst, %q...)\n", newPrefix)
			b.WriteString(code)
		} else {
			fmt.Fprintf(b, "dst = append(dst, %q...)\n", prefix)
			b.WriteString(renderAppendValue(f, ref))
		}
		if emit != "" {
			b.WriteString("}\n")
		}
	}
	b.WriteString("return append(dst, '}'), nil")
}

func renderAppendValue(f FieldInfo, ref string) string {
	if f.Pointer {
		// null when nil; otherwise recurse into the pointee via dereference.
		inner := f
		inner.Pointer = false
		if inner.PointeeType != "" {
			inner.GoType = inner.PointeeType
		}
		innerRef := "(*" + ref + ")"
		return fmt.Sprintf(`if %s == nil {
	dst = append(dst, 'n', 'u', 'l', 'l')
} else {
	%s}
`, ref, renderAppendValue(inner, innerRef))
	}
	if f.String {
		switch f.Kind {
		case KindBool:
			return fmt.Sprintf("if %s { dst = append(dst, '\"', 't', 'r', 'u', 'e', '\"') } else { dst = append(dst, '\"', 'f', 'a', 'l', 's', 'e', '\"') }\n", ref)
		case KindInt:
			return fmt.Sprintf("dst = append(dst, '\"')\ndst = strconv.AppendInt(dst, int64(%s), 10)\ndst = append(dst, '\"')\n", ref)
		case KindInt64:
			return fmt.Sprintf("dst = append(dst, '\"')\ndst = strconv.AppendInt(dst, %s, 10)\ndst = append(dst, '\"')\n", ref)
		case KindUint64:
			return fmt.Sprintf("dst = append(dst, '\"')\ndst = strconv.AppendUint(dst, %s, 10)\ndst = append(dst, '\"')\n", ref)
		case KindFloat64:
			return fmt.Sprintf("dst = append(dst, '\"')\ndst = strconv.AppendFloat(dst, %s, 'g', -1, 64)\ndst = append(dst, '\"')\n", ref)
		}
		// unknown/invalid combo — fall through to default
	}
	switch f.Kind {
	case KindString:
		return fmt.Sprintf("dst = append(dst, '\"')\ndst =%s(dst, %s)\n", appendStrFn(f.HTMLEscape), ref)
	case KindBool:
		return fmt.Sprintf("dst = strconv.AppendBool(dst, %s)\n", ref)
	case KindInt, KindInt8, KindInt16, KindInt32:
		return fmt.Sprintf("dst = strconv.AppendInt(dst, int64(%s), 10)\n", ref)
	case KindInt64:
		return fmt.Sprintf("dst = strconv.AppendInt(dst, %s, 10)\n", ref)
	case KindUint, KindUint8, KindUint16, KindUint32:
		return fmt.Sprintf("dst = strconv.AppendUint(dst, uint64(%s), 10)\n", ref)
	case KindUint64:
		return fmt.Sprintf("dst = strconv.AppendUint(dst, %s, 10)\n", ref)
	case KindFloat32:
		return fmt.Sprintf("dst = strconv.AppendFloat(dst, float64(%s), 'g', -1, 64)\n", ref)
	case KindFloat64:
		return fmt.Sprintf("dst = strconv.AppendFloat(dst, %s, 'g', -1, 64)\n", ref)
	case KindStruct:
		if isGenerated(f.GoType) {
			return fmt.Sprintf("if dst, err = %s.AppendJSON(dst); err != nil { return dst, err }\n", ref)
		}
		return renderCrossPkgStructAppend(f, ref)
	case KindSlice, KindArray:
		b := getSmall()
		defer putSmall(b)
		renderAppendSlice(b, f, ref)
		return b.String()
	case KindMap:
		b := getSmall()
		defer putSmall(b)
		renderAppendMap(b, f, ref)
		return b.String()
	case KindBytes:
		return renderAppendBytes(f, ref)
	case KindTime:
		return renderAppendTime(f, ref)
	case KindDuration:
		return renderAppendDuration(f, ref)
	case KindNetIP, KindNetipAddr, KindNetipPrefix:
		// All three implement encoding.TextAppender (Go 1.24+). One
		// uniform emit path — same as the cross-pkg TextAppender branch.
		return fmt.Sprintf(`dst = append(dst, '"')
if dst, err = %s.AppendText(dst); err != nil { return dst, err }
dst = append(dst, '"')
`, ref)
	case KindRawJSON:
		// Emit raw bytes verbatim (or "null" if empty/nil).
		return fmt.Sprintf(`if len(%s) == 0 {
	dst = append(dst, 'n', 'u', 'l', 'l')
} else {
	dst = append(dst, %s...)
}
`, ref, ref)
	case KindURL:
		return fmt.Sprintf("dst = append(dst, '\"')\ndst = encode.AppendURL(dst, %s)\ndst = append(dst, '\"')\n", ref)
	case KindBigInt:
		// big.Int.Append takes (buf, base) and appends in place — no alloc.
		return fmt.Sprintf("dst = (&%s).Append(dst, 10)\n", ref)
	case KindBigFloat:
		// big.Float as JSON string — matches jsonv2's expected wire format.
		// big.Float.Append: (buf, format byte, prec int).
		return fmt.Sprintf("dst = append(dst, '\"')\ndst = (&%s).Append(dst, 'g', -1)\ndst = append(dst, '\"')\n", ref)
	case KindBigRat:
		// Rat is JSON-stringified ("num/denom" or just "n" when whole).
		// AppendText cuts ~3 allocs vs %s(dst, (&r).RatString()) — same
		// wire shape (collapses to integer when IsInt()), but writes
		// straight into dst instead of materializing a fresh string.
		return fmt.Sprintf("dst = append(dst, '\"')\nif dst, err = (&%s).AppendText(dst); err != nil { return dst, err }\ndst = append(dst, '\"')\n", ref)
	case KindSQLNull:
		spec, ok := SQLNullSpec(f.GoType)
		if !ok {
			return ""
		}
		// Build the inner-value emit for the .X field — reuse renderAppendValue
		// with a synthetic FieldInfo whose Kind is the inner kind.
		innerField := FieldInfo{Kind: spec.Inner, GoType: spec.Type, Format: f.Format}
		innerEmit := renderAppendValue(innerField, ref+"."+spec.Field)
		return fmt.Sprintf(`if !%s.Valid {
	dst = append(dst, 'n', 'u', 'l', 'l')
} else {
	%s}
`, ref, innerEmit)
	case KindAny:
		// AppendAny type-switches on common runtime types (primitives,
		// []any, map[string]any) before falling back to encoding/json.
		return fmt.Sprintf("if dst, err = encode.AppendAny(dst, %s); err != nil { return dst, err }\n", ref)
	}
	return ""
}

// renderAppendBytes emits marshal code for a []byte field based on format.
// Inlines the stdlib AppendEncode call between quote bytes — the
// previously-helper-wrapped versions saved no work over the inlined form.
func renderAppendBytes(f FieldInfo, ref string) string {
	switch f.Format {
	case "", "base64":
		return fmt.Sprintf("dst = append(dst, '\"')\ndst =base64.StdEncoding.AppendEncode(dst, %s)\ndst =append(dst, '\"')\n", ref)
	case "base64url":
		return fmt.Sprintf("dst = append(dst, '\"')\ndst =base64.URLEncoding.AppendEncode(dst, %s)\ndst =append(dst, '\"')\n", ref)
	case "base32":
		return fmt.Sprintf("dst = append(dst, '\"')\ndst =base32.StdEncoding.AppendEncode(dst, %s)\ndst =append(dst, '\"')\n", ref)
	case "base32hex":
		return fmt.Sprintf("dst = append(dst, '\"')\ndst =base32.HexEncoding.AppendEncode(dst, %s)\ndst =append(dst, '\"')\n", ref)
	case "base16", "hex":
		return fmt.Sprintf("dst = append(dst, '\"')\ndst =hex.AppendEncode(dst, %s)\ndst =append(dst, '\"')\n", ref)
	case "array":
		return fmt.Sprintf(`dst = append(dst, '[')
for i, b := range %s {
	if i > 0 { dst = append(dst, ',') }
	dst = strconv.AppendUint(dst, uint64(b), 10)
}
dst = append(dst, ']')
`, ref)
	}
	// Unknown format: fall back to base64.
	return fmt.Sprintf("dst = append(dst, '\"')\ndst =base64.StdEncoding.AppendEncode(dst, %s)\ndst =append(dst, '\"')\n", ref)
}

// timeLayoutExpr returns the Go expression for a format value on a time.Time
// field (e.g. "RFC3339" → time.RFC3339, 'custom' → "custom" literal). It also
// returns whether the format targets a numeric encoding (unix family).
// timeFormatSize returns the JSONSize byte budget for a `time.Time`
// field with the given `format:` option, including surrounding quotes
// (or none for numeric `unix*` variants). Unknown / custom layouts
// fall back to `len(format) + 6` — output length tracks the layout
// string closely (literal text passes through verbatim) with a small
// slack for `_2` style space-pads and timezone offset width.
func timeFormatSize(format string) int {
	switch format {
	case "unix":
		// Worst case: sign + 10-digit unix seconds + `.` + 9-digit
		// nanos = 21 chars. Plus a small slack for the float
		// formatter's exponent path if it ever kicks in (it doesn't
		// at 'f' format, but be defensive).
		return 24
	case "unixmilli", "unixmicro", "unixnano":
		return sizeInt // int64 digits, no quotes
	case "", "RFC3339Nano":
		return 35 + 2
	case "RFC3339":
		return 25 + 2
	case "ANSIC":
		return 24 + 2
	case "UnixDate":
		// "MST" → up to 5-char offset when zone has no name.
		return 30 + 2
	case "RubyDate":
		return 30 + 2
	case "RFC822":
		// "MST" → up to 5-char offset when zone has no name.
		return 21 + 2
	case "RFC822Z":
		return 21 + 2
	case "RFC850":
		// "MST" → up to 5-char offset when zone has no name.
		return 32 + 2
	case "RFC1123":
		// "MST" → up to 5-char offset when zone has no name.
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
	return len(format) + 6
}

// durationFormatSize returns the JSONSize byte budget for a
// `time.Duration` field. Numeric formats (sec/milli/micro/nano) emit
// an int with no quotes; the default `units` format renders as a
// quoted "NhNmNs" string capped at ~25 chars + quotes.
func durationFormatSize(format string) int {
	switch format {
	case "sec", "milli", "micro", "nano":
		return sizeInt
	}
	return 25 + 2 // "<NhNmNs>" worst case
}

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
	// Recognized time-package constant names.
	switch format {
	case "Layout", "ANSIC", "UnixDate", "RubyDate",
		"RFC822", "RFC822Z", "RFC850", "RFC1123", "RFC1123Z",
		"RFC3339", "RFC3339Nano", "Kitchen",
		"Stamp", "StampMilli", "StampMicro", "StampNano",
		"DateTime", "DateOnly", "TimeOnly":
		return "time." + format, ""
	}
	// Custom layout — emit as a string literal.
	return strconv.Quote(format), ""
}

func renderAppendTime(f FieldInfo, ref string) string {
	layout, numeric := timeLayoutExpr(f.Format)
	if numeric != "" {
		// `format:unix` is the seconds unit — sub-second nanos must
		// emit as a fractional decimal to match jsonv2's wire format
		// (jsonv2 emits e.g. `1778762096.789` when nanos != 0).
		// `unixmilli`/`unixmicro`/`unixnano` work at integer granularity
		// of their respective units, so plain AppendInt is correct.
		if numeric == "Unix" {
			return fmt.Sprintf("dst = strconv.AppendFloat(dst, float64(%s.UnixNano())/1e9, 'f', -1, 64)\n", ref)
		}
		return fmt.Sprintf("dst = strconv.AppendInt(dst, %s.%s(), 10)\n", ref, numeric)
	}
	return fmt.Sprintf("dst = append(dst, '\"')\ndst = %s.AppendFormat(dst, %s)\ndst = append(dst, '\"')\n", ref, layout)
}

func renderAppendDuration(f FieldInfo, ref string) string {
	switch f.Format {
	case "sec":
		return fmt.Sprintf("dst = strconv.AppendFloat(dst, %s.Seconds(), 'g', -1, 64)\n", ref)
	case "milli":
		return fmt.Sprintf("dst = strconv.AppendInt(dst, %s.Milliseconds(), 10)\n", ref)
	case "micro":
		return fmt.Sprintf("dst = strconv.AppendInt(dst, %s.Microseconds(), 10)\n", ref)
	case "nano":
		return fmt.Sprintf("dst = strconv.AppendInt(dst, %s.Nanoseconds(), 10)\n", ref)
	case "units":
		return fmt.Sprintf("dst = append(dst, '\"')\ndst =%s(dst, %s.String())\n", appendStrFn(f.HTMLEscape), ref)
	}
	// jsonv2 requires an explicit format for Duration; fall back to units string.
	return fmt.Sprintf("dst = %s(dst, %s.String())\n", appendStrFn(f.HTMLEscape), ref)
}

// structHasAppendFormatTime reports whether any field of s emits via
// time.Time.AppendFormat (i.e. a non-numeric time format). Used by
// renderSize to reserve the 64-byte headroom Go's stdlib AppendFormat
// requires via its internal slices.Grow call.
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

// renderSize emits the body of a JSONSize method: sums the exact (or
// worst-case) byte count needed to serialize the struct as JSON. Per-field
// contributions split into a compile-time constant (folded into the
// initial `size := N`) and runtime code (loops, len(), recursive calls).
// Two passes over the field list — first to total the constant, then to
// emit runtime adds — would re-invoke sizeContrib redundantly, so we
// collect runtime additions into a sibling builder during the constant
// pass and flush them after the `size := N` line is written.
func renderSize(b *bytes.Buffer, s StructInfo) {
	if s.IsAlias {
		b.WriteString(renderAliasSize(s))
		return
	}
	// Fixed overhead: braces + per-field key bytes + separating commas.
	// omitempty/omitzero fields move their key+value contribution out
	// of the constant and into a runtime `if <emit> { size += ... }`
	// guard, so a zero-valued OmitStruct doesn't reserve room for
	// fields that won't ship.
	fixed := 2 // { and }
	// time.AppendFormat internally calls slices.Grow(b, max(layout, 64))
	// per invocation. Without 64-byte headroom at every call site the
	// runtime doubles the backing array — defeating the single-alloc
	// Marshal contract. Reserve one shared 64-byte tail at struct level
	// for any non-numeric time-format field; subsequent AppendFormat
	// calls in the same Marshal benefit from the same slack since their
	// actual content writes consume less than 64 bytes each.
	if structHasAppendFormatTime(s) {
		fixed += 64
	}
	named := 0
	var runtime bytes.Buffer
	for _, f := range s.Fields {
		if f.Inline {
			// Inline catch-all: name/colon/comma budgeted per-entry
			// in sizeMapContrib. No fixed key bytes here.
			ref := "s." + f.GoName
			_, code := sizeContrib(f, ref)
			runtime.WriteString(code)
			continue
		}
		ref := "s." + f.GoName
		emit := fieldSkipExpr(f, ref)
		// For pointer fields whose emit predicate is the nil check,
		// the outer guard already proves non-nil. Pass the
		// dereferenced inner to sizeContrib so it doesn't emit a
		// redundant `if ref == nil { 4 } else { ... }` inside the
		// `if ref != nil { ... }` we're already in.
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
			fixed += len(f.JSONName) + 3 // "name":
			if named > 0 {
				fixed++ // comma
			}
			named++
			fixed += n
			runtime.WriteString(code)
			continue
		}
		// Omit-eligible field: pessimistic worst-case includes a
		// leading comma (cheaper than threading "is this the first
		// emitted field" through the runtime guard).
		fmt.Fprintf(&runtime, "if %s {\nsize += %d\n", emit, len(f.JSONName)+4+n)
		runtime.WriteString(code)
		runtime.WriteString("}\n")
	}
	fmt.Fprintf(b, "size := %d\n", fixed)
	b.WriteString(runtime.String())
	b.WriteString("return size")
}

// appendStrFn returns the encode-pkg helper to call for emitting a JSON
// string body + closing `"`. Default is the raw, jsonv2-shaped variant
// that emits `<`, `>`, `&` literally; when the field's parent struct
// opts in via the `htmlescape` annotation / `-htmlescape` flag, codegen
// switches to the HTML-safe escaper that matches stdlib `encoding/json` v1.
//
// The CALLER must have written the opening `"` already — folded into a
// constant prefix at struct-field top level, or explicitly via
// `dst = append(dst, '"')` at slice/map/standalone sites.
func appendStrFn(htmlEscape bool) string {
	if htmlEscape {
		return "encode.AppendString"
	}
	return "encode.AppendStringNoHTML"
}

// foldLeadingQuote checks whether the given field's value emit begins
// with a JSON `"` byte. If so, it returns the prefix with `"` appended
// and the value-emit code with the opening quote elided. Caller can
// emit `prefix + code` instead of `oldPrefix + standardValueEmit` to
// save one byte-append op per field.
func foldLeadingQuote(f FieldInfo, ref, prefix string) (newPrefix, code string, ok bool) {
	if f.Pointer {
		return prefix, "", false // pointer may emit "null"
	}
	switch f.Kind {
	case KindString:
		return prefix + `"`, fmt.Sprintf("dst = %s(dst, %s)\n", appendStrFn(f.HTMLEscape), ref), true
	case KindNetIP, KindNetipAddr, KindNetipPrefix:
		return prefix + `"`, fmt.Sprintf(`if dst, err = %s.AppendText(dst); err != nil { return dst, err }
dst = append(dst, '"')
`, ref), true
	case KindBytes:
		switch f.Format {
		case "", "base64":
			return prefix + `"`, fmt.Sprintf("dst = base64.StdEncoding.AppendEncode(dst, %s)\ndst =append(dst, '\"')\n", ref), true
		case "base64url":
			return prefix + `"`, fmt.Sprintf("dst = base64.URLEncoding.AppendEncode(dst, %s)\ndst =append(dst, '\"')\n", ref), true
		case "base32":
			return prefix + `"`, fmt.Sprintf("dst = base32.StdEncoding.AppendEncode(dst, %s)\ndst =append(dst, '\"')\n", ref), true
		case "base32hex":
			return prefix + `"`, fmt.Sprintf("dst = base32.HexEncoding.AppendEncode(dst, %s)\ndst =append(dst, '\"')\n", ref), true
		case "base16", "hex":
			return prefix + `"`, fmt.Sprintf("dst = hex.AppendEncode(dst, %s)\ndst =append(dst, '\"')\n", ref), true
		}
	case KindURL:
		// encode.AppendURL replicates url.URL.String semantics with
		// byte-append — zero alloc (the stdlib AppendBinary internally
		// calls String, which builds a strings.Builder buffer).
		// URL output never contains `"` or `\` (both percent-encoded),
		// so safe to drop between JSON quotes without escaping.
		return prefix + `"`, fmt.Sprintf("dst = encode.AppendURL(dst, %s)\ndst = append(dst, '\"')\n", ref), true
	case KindBigRat:
		return prefix + `"`, fmt.Sprintf("if dst, err = (&%s).AppendText(dst); err != nil { return dst, err }\ndst = append(dst, '\"')\n", ref), true
	case KindBigFloat:
		return prefix + `"`, fmt.Sprintf("dst = (&%s).Append(dst, 'g', -1)\ndst = append(dst, '\"')\n", ref), true
	}
	return prefix, "", false
}

// Worst-case byte budgets for primitive encodings.
const (
	sizeBool    = 5  // "false"
	sizeInt     = 20 // "-9223372036854775808"
	sizeUint    = 20 // "18446744073709551615"
	sizeFloat   = 24 // IEEE-754 shortest round-trip printing
	sizeStrMult = 2  // 2× covers short escapes (\n, \", \\, …). Control
	// chars expand to \uXXXX (6×) but are rare in real payloads; we
	// accept the one-time realloc on pathological input.
	sizeStrPad = 2 // surrounding quotes
)

// sizeContrib returns (constN, runtimeCode) — the constant byte count
// known at codegen time, and the runtime statements that compute the
// variable contribution. constN is folded into the initial `size := N`
// at the top level; runtimeCode is emitted as-is.
func sizeContrib(f FieldInfo, ref string) (int, string) {
	if f.Pointer {
		inner := f
		inner.Pointer = false
		if inner.PointeeType != "" {
			inner.GoType = inner.PointeeType
		}
		innerN, innerCode := sizeContrib(inner, "(*"+ref+")")
		b := getSmall()
		defer putSmall(b)
		fmt.Fprintf(b, "if %s == nil { size += 4 } else {\n", ref)
		if innerN > 0 {
			fmt.Fprintf(b, "size += %d\n", innerN)
		}
		b.WriteString(innerCode)
		b.WriteString("}\n")
		return 0, b.String()
	}
	switch f.Kind {
	case KindString:
		return sizeStrPad, fmt.Sprintf("size += len(%s)*%d\n", ref, sizeStrMult)
	case KindBool:
		return sizeBool, ""
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		return sizeInt, ""
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		return sizeUint, ""
	case KindFloat32, KindFloat64:
		return sizeFloat, ""
	case KindStruct:
		if isGenerated(f.GoType) || f.Iface.JSONSize {
			return 0, fmt.Sprintf("size += %s.JSONSize()\n", ref)
		}
		return 128, ""
	case KindSlice, KindArray:
		return sizeSliceContrib(f, ref, 0)
	case KindMap:
		return sizeMapContrib(f, ref)
	case KindBytes:
		switch f.Format {
		case "array":
			// JSON array of numbers: each byte → up to 3 digits + comma,
			// minus 1 for missing trailing comma. Upper-bound with *4 and
			// the +2 const covers `[` + `]`.
			return 2, fmt.Sprintf("size += len(%s)*4\n", ref)
		case "base16", "hex":
			// Exactly 2× input.
			return 2, fmt.Sprintf("size += len(%s)*2\n", ref)
		case "base32", "base32hex":
			// 8/5 of input rounded up to a block of 8. ((n+4)/5)*8.
			return 2, fmt.Sprintf("size += ((len(%s)+4)/5)*8\n", ref)
		}
		// base64 (default) / base64url: ((n+2)/3)*4.
		return 2, fmt.Sprintf("size += ((len(%s)+2)/3)*4\n", ref)
	case KindTime:
		return timeFormatSize(f.Format), ""
	case KindDuration:
		return durationFormatSize(f.Format), ""
	case KindNetIP:
		// net.IP is []byte, but ParseIP returns 16 bytes even for IPv4,
		// so a raw `len(ip)==4` check is dead code. Use To4() — returns
		// non-nil only when the address actually fits in 4 octets.
		return 2, fmt.Sprintf("if %s.To4() != nil { size += 15 } else if len(%s) != 0 { size += 39 } else { size += 2 }\n", ref, ref)
	case KindNetipAddr:
		// netip.Addr: Is4() splits v4 vs v6 budget.
		return 2, fmt.Sprintf("if %s.Is4() { size += 15 } else { size += 39 }\n", ref)
	case KindNetipPrefix:
		// netip.Prefix is Addr + /N: +4 for "/128" worst case.
		return 2, fmt.Sprintf("if %s.Addr().Is4() { size += 19 } else { size += 43 }\n", ref)
	case KindRawJSON:
		return 0, fmt.Sprintf("if n := len(%s); n > 0 { size += n } else { size += 4 }\n", ref)
	case KindURL:
		// Sum the components instead of reserving a flat 256. The +8
		// const covers `"` + `://` + `?` + `#` + closing `"`. Path and
		// Fragment are stored decoded; String() re-percent-encodes
		// non-ASCII bytes (3 chars per byte worst case) so we multiply
		// by 3. Host and RawQuery emit as-is. User info reads
		// Username/Password directly (no alloc) and multiplies by 3
		// for percent-escape worst case.
		return 8, fmt.Sprintf("size += len(%s.Scheme) + len(%s.Host)*3 + len(%s.Path)*3 + len(%s.RawQuery) + len(%s.Fragment)*3 + len(%s.Opaque)\nif %s.User != nil { pw, _ := %s.User.Password(); size += (len(%s.User.Username()) + len(pw))*3 + 2 }\n",
			ref, ref, ref, ref, ref, ref, ref, ref, ref)
	case KindBigInt:
		// log10(2^bits) ≈ bits * 0.302. Add sign + safety. BitLen is cheap.
		return 4, fmt.Sprintf("size += %s.BitLen()/3\n", ref)
	case KindBigFloat:
		// Mantissa + exponent representation + surrounding quotes;
		// precision-derived bound.
		return 66, ""
	case KindBigRat:
		// Two big.Ints + slash + quotes. Approximate by num+denom bit lengths.
		return 8, fmt.Sprintf("size += (%s.Num().BitLen() + %s.Denom().BitLen())/3\n", ref, ref)
	case KindSQLNull:
		spec, ok := SQLNullSpec(f.GoType)
		if !ok {
			return 0, ""
		}
		innerField := FieldInfo{Kind: spec.Inner, GoType: spec.Type, Format: f.Format}
		return sizeContrib(innerField, ref+"."+spec.Field)
	case KindAny:
		// Conservative — no introspection at codegen time. 256 covers most
		// scalar/object payloads; deeply nested any[] with many keys can
		// overshoot, but JSONSize is upper-bound by design.
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
	switch f.ElemKind {
	case KindString:
		fmt.Fprintf(b, "for %s := range %s { size += len(%s[%s])*%d + %d }\n",
			ivar, ref, ref, ivar, sizeStrMult, sizeStrPad)
	case KindBool:
		fmt.Fprintf(b, "size += len(%s) * %d\n", ref, sizeBool)
	case KindInt, KindInt64, KindInt8, KindInt16, KindInt32:
		fmt.Fprintf(b, "size += len(%s) * %d\n", ref, sizeInt)
	case KindUint, KindUint64, KindUint8, KindUint16, KindUint32:
		fmt.Fprintf(b, "size += len(%s) * %d\n", ref, sizeUint)
	case KindFloat32, KindFloat64:
		fmt.Fprintf(b, "size += len(%s) * %d\n", ref, sizeFloat)
	case KindStruct:
		if isGenerated(f.ElemType) || f.ElemIface.JSONSize {
			if f.ElemPointer {
				// `[]*T` / `[N]*T`: nil elements contribute `null` (4 bytes),
				// non-nil deref-and-call.
				fmt.Fprintf(b, "for %s := range %s {\nif %s[%s] == nil { size += 4 } else { size += (*%s[%s]).JSONSize() }\n}\n",
					ivar, ref, ref, ivar, ref, ivar)
			} else {
				fmt.Fprintf(b, "for %s := range %s { size += %s[%s].JSONSize() }\n",
					ivar, ref, ref, ivar)
			}
		} else {
			fmt.Fprintf(b, "size += len(%s) * 128\n", ref)
		}
	case KindSlice, KindArray:
		fmt.Fprintf(b, "for %s := range %s {\n", ivar, ref)
		innerN, innerCode := sizeSliceContrib(peelSliceField(f), fmt.Sprintf("%s[%s]", ref, ivar), depth+1)
		if innerN > 0 {
			fmt.Fprintf(b, "size += %d\n", innerN)
		}
		b.WriteString(innerCode)
		b.WriteString("}\n")
	}
	return 2, b.String() // brackets
}

// sizeMapContrib emits the size contribution for a `map[string]V` field.
// Per-entry overhead is `"k":v,` = 4 fixed bytes (one extra comma overcounted
// on the last entry — kept for simplicity, still upper bound), plus 2*len(k)
// for the key (escape worst case), plus a kind-derived value budget. The
// 2-byte braces are returned as a constant for the caller to fold.
func sizeMapContrib(f FieldInfo, ref string) (int, string) {
	b := getSmall()
	defer putSmall(b)
	const perEntryFixed = 4

	// Try to lift the value contribution out of the loop when it's a
	// constant per-entry size — saves one map iteration over keys-only.
	if v, ok := constSizePerEntry(f.ElemKind, f.Format); ok {
		fmt.Fprintf(b, "size += len(%s) * %d\n", ref, perEntryFixed+v)
		fmt.Fprintf(b, "for k := range %s { size += len(k) * %d }\n", ref, sizeStrMult)
		return 2, b.String()
	}

	// Variable per-entry: one combined loop over k,v.
	fmt.Fprintf(b, "size += len(%s) * %d\n", ref, perEntryFixed)
	fmt.Fprintf(b, "for k, v := range %s {\n", ref)
	fmt.Fprintf(b, "size += len(k) * %d\n", sizeStrMult)
	switch f.ElemKind {
	case KindString:
		fmt.Fprintf(b, "size += len(v)*%d + %d\n", sizeStrMult, sizeStrPad)
	case KindStruct:
		if isGenerated(f.ElemType) || f.ElemIface.JSONSize {
			b.WriteString("size += v.JSONSize()\n")
		} else {
			b.WriteString("size += 128\n")
		}
	case KindBigInt:
		b.WriteString("size += v.BitLen()/3 + 4\n")
	case KindBigRat:
		b.WriteString("size += (v.Num().BitLen() + v.Denom().BitLen())/3 + 8\n")
	case KindNetIP:
		b.WriteString("if v.To4() != nil { size += 17 } else if len(v) != 0 { size += 41 } else { size += 4 }\n")
	case KindNetipAddr:
		b.WriteString("if v.Is4() { size += 17 } else { size += 41 }\n")
	case KindNetipPrefix:
		b.WriteString("if v.Addr().Is4() { size += 21 } else { size += 45 }\n")
	case KindURL:
		b.WriteString("size += len(v.Scheme) + len(v.Host)*3 + len(v.Path)*3 + len(v.RawQuery) + len(v.Fragment)*3 + len(v.Opaque) + 8\n")
		b.WriteString("if v.User != nil { pw, _ := v.User.Password(); size += (len(v.User.Username()) + len(pw))*3 + 2 }\n")
	default:
		// Anything else (nested slice/map, KindAny, …) falls back to the
		// legacy flat estimate. Refining further is doable but rarely worth
		// it — these compositions are uncommon as direct map values.
		b.WriteString("size += 128\n")
	}
	b.WriteString("}\n")
	return 2, b.String()
}

// constSizePerEntry reports whether a value of the given kind has a
// known fixed upper-bound size, and returns that size. Used to lift the
// value contribution out of the per-entry loop in renderSizeMap.
// `format` is honored for KindTime / KindDuration so e.g. a
// `map[string]time.Time` with `format:Kitchen` reserves ~9 bytes per
// entry instead of the RFC3339Nano-sized 37.
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
	case KindBigFloat:
		return 66, true // +2 for surrounding quotes
	case KindAny:
		return 64, true
	}
	return 0, false
}

func renderAppendSlice(b *bytes.Buffer, f FieldInfo, ref string) {
	emitAppendSlice(b, f, ref, 0)
}

// emitAppendSlice is the recursive marshal counterpart to emitByteSliceRead.
// Nested slices peel one [] off per level; loop vars carry a depth suffix
// (i0, v0 at the outermost, i1, v1 one level in, …) to avoid collisions.
//
// nil-slice handling: a nil slice serializes as JSON `null` to match stdlib
// `encoding/json` v1/v2. Empty non-nil slice still serializes as `[]`.
// Fixed-length arrays can't be nil so they skip the check.
func emitAppendSlice(b *bytes.Buffer, f FieldInfo, ref string, depth int) {
	vvar := fmt.Sprintf("v%d", depth)
	if f.Kind == KindSlice {
		fmt.Fprintf(b, "if %s == nil {\ndst = append(dst, \"null\"...)\n} else {\n", ref)
	}
	fmt.Fprintf(b, "dst = append(dst, '[')\nif len(%s) > 0 {\n", ref)
	// First element: no leading comma. Refer to it directly as `ref[0]`
	// to keep the emit primitive — saves declaring the loop var twice.
	emitSliceElement(b, f, fmt.Sprintf("%s[0]", ref), depth)
	// Rest: comma first, then element. Iterating over `ref[1:]` lifts the
	// `if i > 0` branch out of every iteration.
	fmt.Fprintf(b, "for _, %s := range %s[1:] {\ndst = append(dst, ',')\n", vvar, ref)
	emitSliceElement(b, f, vvar, depth)
	b.WriteString("}\n}\ndst = append(dst, ']')\n")
	if f.Kind == KindSlice {
		b.WriteString("}\n")
	}
}

// emitSliceElement emits the marshal code for one slice element at the
// given source expression. Shared between the first-element and loop-body
// emits in emitAppendSlice so the per-iteration `if i > 0` check is gone.
func emitSliceElement(b *bytes.Buffer, f FieldInfo, vref string, depth int) {
	if f.ElemPointer {
		// nil pointer element → null. Else emit as if it were a value
		// (Go auto-derefs the pointer for value-receiver method calls).
		fmt.Fprintf(b, "if %s == nil {\ndst = append(dst, \"null\"...)\n} else {\n", vref)
		// Recurse with ElemPointer cleared on a copy of f so the nested
		// emit doesn't re-trigger the nil-check.
		nf := f
		nf.ElemPointer = false
		emitSliceElement(b, nf, "(*"+vref+")", depth)
		b.WriteString("}\n")
		return
	}
	switch f.ElemKind {
	case KindString:
		// Split into two lines so the coalescer pass can fold the `'"'`
		// with whatever const append precedes (e.g. the slice's leading
		// `,` becomes `","...`).
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
		fmt.Fprintf(b, "dst = strconv.AppendFloat(dst, float64(%s), 'g', -1, 32)\n", vref)
	case KindFloat64:
		fmt.Fprintf(b, "dst = strconv.AppendFloat(dst, %s, 'g', -1, 64)\n", vref)
	case KindStruct:
		if isGenerated(f.ElemType) {
			fmt.Fprintf(b, "if dst, err = %s.AppendJSON(dst); err != nil { return dst, err }\n", vref)
		} else {
			fmt.Fprintf(b, `{
	bs, err := json.Marshal(%s)
	if err != nil { return dst, err }
	dst = append(dst, bs...)
}
`, vref)
		}
	case KindSlice, KindArray:
		emitAppendSlice(b, peelSliceField(f), vref, depth+1)
	}
}

// renderAppendMap emits marshal code for a map[string]V field. Iteration
// order is Go's randomized map order — deterministic roundtrip via
// unmarshal, but wire output is not stable. Wrapped in a block scope so
// adjacent maps don't collide on the `first` variable.
//
// nil map → null (matches stdlib). Empty non-nil → {}. JSON `{` lives
// OUTSIDE the Go scoping block so coalesceConstAppends can merge it with
// the preceding `"key":` prefix. Same for the closing `}` and whatever
// comes after.
func renderAppendMap(b *bytes.Buffer, f FieldInfo, ref string) {
	appendStr := appendStrFn(f.HTMLEscape)
	fmt.Fprintf(b, `if %[1]s == nil {
dst = append(dst, "null"...)
} else {
dst = append(dst, '{')
{
first := true
for k, v := range %[1]s {
if first { first = false
dst =append(dst, '"') } else { dst = append(dst, ",\""...) }
dst = %[2]s(dst, k)
dst = append(dst, ':')
`, ref, appendStr)
	switch f.ElemKind {
	case KindString:
		// Two separate append lines so coalesce sees the `'"'` and merges
		// it with the preceding `':'` into `":\""...`.
		fmt.Fprintf(b, "dst = append(dst, '\"')\ndst = %s(dst, v)\n", appendStr)
	case KindBool:
		b.WriteString("dst = strconv.AppendBool(dst, v)\n")
	case KindInt:
		b.WriteString("dst = strconv.AppendInt(dst, int64(v), 10)\n")
	case KindInt64:
		b.WriteString("dst = strconv.AppendInt(dst, v, 10)\n")
	case KindUint64:
		b.WriteString("dst = strconv.AppendUint(dst, v, 10)\n")
	case KindFloat64:
		b.WriteString("dst = strconv.AppendFloat(dst, v, 'g', -1, 64)\n")
	case KindStruct:
		if isGenerated(f.ElemType) {
			b.WriteString("if dst, err = v.AppendJSON(dst); err != nil { return dst, err }\n")
		} else {
			b.WriteString(`{
	bs, err := json.Marshal(v)
	if err != nil { return dst, err }
	dst = append(dst, bs...)
}
`)
		}
	case KindAny:
		b.WriteString("if dst, err = encode.AppendAny(dst, v); err != nil { return dst, err }\n")
	}
	b.WriteString("}\n}\ndst = append(dst, '}')\n}\n")
}

// defaultPreallocCap is the small constant cap applied to slices /
// maps when no explicit sizing hint is present. Sized to absorb most
// real-world short collections without triggering the 1→2→4→...
// growth chain that leaves orphan backings inflating retained heap.
const defaultPreallocCap = 4

// userPreallocHint extracts an explicit user-provided sizing hint
// from the hintlen / len / minlen ladder. Returns -1 when no hint
// is set, so callers can distinguish "user said 0" (opt-out via
// hintlen=0) from "user said nothing" (fall through to caller's
// kind-based default).
//
// Precedence:
//  1. `hintlen=N`:
//     N >  0 → that many entries
//     N == 0 → user opt-out, force zero (overrides len/minlen)
//     N <  0 → sentinel "unset" — fall through
//  2. `len=N`    — exact count, no waste
//  3. `minlen=N` — floor; growth still works above
//
// `maxlen` is intentionally NOT used — see "tried and rejected"
// in CLAUDE.md. Pre-sizing to the worst-case bound retains
// over-allocated capacity per held value (~100 KB/item on Mega).
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

// mapPreallocCap returns the cap for `make(map[K]V, cap)` from the
// user's sizing hints. Returns 0 when no hint is set — maps don't
// get a kind-based default because Go's `makemap` lazy-allocates
// for hints < 8 (so small defaults are wasted) and the per-bucket
// over-allocation cost is too high to justify a higher default for
// the typical "small or empty map" case. Users with predictable
// sizes can opt in via `ggen:"hintlen=N"`.
func mapPreallocCap(f FieldInfo) int {
	if n := userPreallocHint(f); n >= 0 {
		return n
	}
	return 0
}

// preallocCap returns the initial capacities for a slice field's
// two backing allocations:
//   - slice: cap for `make([]E, 0, slice)` — the field's own slice.
//     Returns 0 to mean "do not emit a make() prealloc". The empty-
//     array branch always emits `[]E{}` regardless (stdlib parity).
//   - slab: cap for `make([]T, 0, slab)` when `f.ElemPointer` is
//     set — the contiguous backing storage for `[]*T` element
//     pointers. Caller ignores when ElemPointer is false.
//
// Precedence — explicit hints win and apply to both caps equally:
//  1. `hintlen=N` (N >= 0)
//  2. `len=N`
//  3. `minlen=N`
//
// With no explicit hint, the kind drives the default:
//   - pointer-element (`[]*T`): both default to defaultPreallocCap.
//     Slice slot waste is 8 B/slot (trivial); slab default avoids
//     the orphan-trail growth chain (0→1→2→4→8 leaves prior backings
//     alive via held `*T` pointers).
//   - heavy non-pointer (struct/slice/map/array): both 0. Over-cap
//     × element-size would explode retained heap.
//   - primitive: slice = defaultPreallocCap (clamped by maxlen if
//     smaller); slab = 0 (unused — primitives have no slab).
//
// `maxlen` is intentionally NOT used as a generous prealloc hint —
// it's an upper bound, not an expected size, and treating it as one
// was the residency villain. See "tried and rejected" in CLAUDE.md.
func preallocCap(f FieldInfo) (slice, slab int) {
	if n := userPreallocHint(f); n >= 0 {
		return n, n
	}
	switch f.ElemKind {
	case KindSlice, KindMap:
		// Element is a slice header (24 B) or map handle (8 B) — both
		// bounded slot sizes. Safe to prealloc: over-cap waste per
		// unused slot is small (cap=4 → ≤96 B waste), and starting
		// from cap=0 forces the 0→1→2→4→8 grow chain whose orphan
		// trail dwarfs the over-cap waste.
		return defaultPreallocCap, 0
	case KindStruct, KindArray:
		if f.ElemPointer {
			// `[]*T` — slice slot is 8 B, slab slot is sizeof(T).
			// Slab default avoids the orphan-trail growth chain
			// (held `*T` pointers anchor into prior backings).
			return defaultPreallocCap, defaultPreallocCap
		}
		// `[]T` value-stored — sizeof(T) could be anything. Prealloc
		// × element-size would explode retained heap on big structs.
		// Start nil, grow via append.
		return 0, 0
	}
	def := defaultPreallocCap
	if v, ok := f.HasRule("maxlen"); ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n < def {
			def = n
		}
	}
	return def, 0
}

// inlineSkipWS emits an inline whitespace-skipping loop that mutates posVar
// directly. Avoids the function-call overhead of scan.SkipSpace, which shows
// up heavily in profiles (>5× hits per field) once other work is tight.
func inlineSkipWS(b *bytes.Buffer, posVar string) {
	fmt.Fprintf(b,
		"for %s < len(data) && (data[%s] == ' ' || data[%s] == '\\t' || data[%s] == '\\n' || data[%s] == '\\r') { %s++ }\n",
		posVar, posVar, posVar, posVar, posVar, posVar)
}

// inlineNullPeek emits an inline `null` literal check on posVar. On a match,
// posVar is advanced 4 bytes inside the `if` body — no `np` / `ok` locals.
// Leaves the body open; caller appends its null-branch statements then the
// `} else {` for the non-null branch (or just `}` if no else is needed).
func inlineNullPeek(b *bytes.Buffer, posVar string) {
	fmt.Fprintf(b,
		"if %s+4 <= len(data) && data[%s] == 'n' && data[%s+1] == 'u' && data[%s+2] == 'l' && data[%s+3] == 'l' {\n%s += 4\n",
		posVar, posVar, posVar, posVar, posVar, posVar)
}

// zeroLit returns a Go expression for the zero value of an elem type, used
// as the pre-grow placeholder when slice-appending before in-place decode.
// Slice/map element kinds use plain `nil` — the recursive emit overwrites
// the slot before any other code observes it, so allocating an empty
// composite literal `[][]string{}` is wasted. Primitives can't use `T{}`
// composite-literal syntax; arrays/structs need it.
func zeroLit(elemType string, kind TypeKind) string {
	switch kind {
	case KindString:
		return `""`
	case KindBool:
		return "false"
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64,
		KindUint, KindUint8, KindUint16, KindUint32, KindUint64,
		KindFloat32, KindFloat64:
		return "0"
	case KindSlice, KindMap:
		return "nil"
	default:
		return elemType + "{}"
	}
}

// inlineScanInt64 emits an inline signed-int scanner that assigns into dst
// (via castFn if non-empty, e.g. "int") and advances posVar. Avoids the
// scan.Int64 call for the hot int fields.
func inlineScanInt64(b *bytes.Buffer, posVar, dst, castFn string) {
	assign := ""
	switch {
	case castFn != "":
		assign = dst + " = " + castFn + "(n)"
	case dst != "n":
		assign = dst + " = n"
	}
	fmt.Fprintf(b, `{
	neg := false
	if %s < len(data) && data[%s] == '-' { neg = true; %s++ }
	if %s >= len(data) || data[%s] < '0' || data[%s] > '9' { return result, i, scan.ErrBadNumber }
	var n int64
	for %s < len(data) && data[%s] >= '0' && data[%s] <= '9' {
		n = n*10 + int64(data[%s]-'0')
		%s++
	}
	if %s < len(data) {
		c := data[%s]
		if c == '.' || c == 'e' || c == 'E' { return result, i, scan.ErrBadNumber }
	}
	if neg { n = -n }
	%s
}
`, posVar, posVar, posVar,
		posVar, posVar, posVar,
		posVar, posVar, posVar, posVar, posVar,
		posVar, posVar,
		assign)
}

// inlineScanUint64 is the unsigned counterpart of inlineScanInt64.
func inlineScanUint64(b *bytes.Buffer, posVar, dst, castFn string) {
	assign := ""
	switch {
	case castFn != "":
		assign = dst + " = " + castFn + "(n)"
	case dst != "n":
		assign = dst + " = n"
	}
	fmt.Fprintf(b, `{
	if %s >= len(data) || data[%s] < '0' || data[%s] > '9' { return result, i, scan.ErrBadNumber }
	var n uint64
	for %s < len(data) && data[%s] >= '0' && data[%s] <= '9' {
		n = n*10 + uint64(data[%s]-'0')
		%s++
	}
	%s
}
`, posVar, posVar, posVar,
		posVar, posVar, posVar, posVar, posVar,
		assign)
}

// inlineScanString emits a zero-copy string reader that assigns into dst and
// advances posOut past the closing quote. The hot path aliases the input via
// unsafe.String; escape sequences fall back to scan.String (allocates).
// Used for both keys and value fields — in typical payloads neither have
// escapes so the loop stays tight.
//
// Only one local (`ke`, the scan cursor) is kept; the string start is
// `posIn + 1` inline. posIn must not be mutated before the slow path runs
// (it falls back via `scan.String(data, posIn)`), so we scan with `ke` and
// only update `posOut` once the result is known. Slow path reuses the
// function-scope `err` from renderDecode's hoist — no local `iserr`.
func inlineScanString(b *bytes.Buffer, posIn, dst, posOut string) {
	fmt.Fprintf(b, `if %s >= len(data) || data[%s] != '"' { return result, i, scan.ErrExpectString }
{
	ke := %s + 1
	for ke < len(data) && data[ke] != '"' && data[ke] != '\\' { ke++ }
	if ke >= len(data) { return result, i, scan.ErrUnterminated }
	if data[ke] == '"' {
		%s = unsafe.String(unsafe.SliceData(data[%s+1:]), ke-%s-1)
		%s = ke + 1
	} else {
		%s, %s, err = scan.String(data, %s)
		if err != nil { return result, i, err }
	}
}
`, posIn, posIn, posIn, dst, posIn, posIn, posOut, dst, posOut, posIn)
}

// renderDecode emits the body of DecodeFrom: a loop that reads each
// JSON key, dispatches to per-field scan code, and handles ',' / '}'.
// Zero-copy (strings alias the input) and zero-alloc on the happy path.
// Dispatch is length-first so missing keys reject with a single int compare
// instead of a string compare per case. Whitespace skipping is inlined at
// each hot-path site to avoid the ~5ns/call overhead dominating runtime.
func renderDecode(b *bytes.Buffer, s StructInfo) {
	if s.IsAlias {
		b.WriteString(renderAliasDecode(s))
		return
	}
	b.WriteString("var result " + s.Name + "\n")
	// Function-scope err shared by every sub-render — slice/map/SQL-null
	// emitters use `=` to reassign instead of declaring local `var err
	// error` per block. `_ = err` keeps it kosher when no sub-render
	// actually touches err (pure-primitive struct).
	b.WriteString("var err error\n_ = err\n")
	if s.MultiErr {
		b.WriteString("var errs validation.Errors\n")
	}

	// Per-field "seen" tracking serves two masters: required-field
	// post-loop checks (when validation is on) and duplicate-key guard
	// (when AllowDups is off). Narrow structs use per-field bools (1 byte
	// each, fit in registers); wide structs (>seenBitmaskThreshold) use
	// a packed bitmask to cut stack/cache pressure on deep recursion.
	if useSeenBitmask(s) {
		b.WriteString(seenDecl(s))
	} else {
		for _, f := range s.Fields {
			if f.Inline {
				continue
			}
			if needsSeen(f) {
				fmt.Fprintf(b, "seen%s := false\n", f.GoName)
			}
		}
	}

	inlineSkipWS(b, "i")
	b.WriteString("if i >= len(data) || data[i] != '{' { return result, i, scan.ErrBadObject }\ni++\n")
	inlineSkipWS(b, "i")
	b.WriteString("if i < len(data) && data[i] == '}' {\n")
	renderPostLoop(b, s)
	b.WriteString("return result, i + 1, nil\n}\nfor {\nvar key string\n")
	inlineScanString(b, "i", "key", "i")
	inlineSkipWS(b, "i")
	b.WriteString("if i >= len(data) || data[i] != ':' { return result, i, scan.ErrBadObject }\ni++\n")
	inlineSkipWS(b, "i")
	b.WriteString(renderDispatch(s))
	inlineSkipWS(b, "i")
	b.WriteString("if i >= len(data) { return result, i, scan.ErrBadObject }\nif data[i] == ',' { i++; ")
	inlineSkipWS(b, "i")
	b.WriteString("continue }\nif data[i] == '}' {\n")
	renderPostLoop(b, s)
	b.WriteString("return result, i + 1, nil\n}\nreturn result, i, scan.ErrBadObject\n}")
}

// renderPostLoop emits end-of-parse bookkeeping: required-field checks
// (when validation is on) and the multierr flush (when MultiErr is on).
// Called at every success exit inside DecodeFrom / DecodeStreamFrom.
func renderPostLoop(b *bytes.Buffer, s StructInfo) {
	if !s.NoValidate {
		for _, f := range s.Fields {
			if !f.IsRequired() || f.Inline {
				continue
			}
			errExpr := requiredErr(f.JSONName)
			notSeen := seenNotAccess(s, f)
			if s.MultiErr {
				fmt.Fprintf(b, "if %s { errs = append(errs, %s) }\n", notSeen, errExpr)
			} else {
				fmt.Fprintf(b, "if %s { return result, i, %s }\n", notSeen, errExpr)
			}
		}
	}
	if s.MultiErr {
		b.WriteString("if len(errs) > 0 { return result, i, errs }\n")
	}
}

// renderDispatch emits a length-first switch on len(key). For each length
// with ≥1 field, emits a nested string switch. Single-field lengths skip the
// nested switch and go direct to the field handler.
func renderDispatch(s StructInfo) string {
	byLen := map[int][]FieldInfo{}
	var lens []int
	for _, f := range s.Fields {
		if f.Inline {
			continue
		}
		n := len(f.JSONName)
		if _, seen := byLen[n]; !seen {
			lens = append(lens, n)
		}
		byLen[n] = append(byLen[n], f)
	}
	slices.Sort(lens)

	// emitField wraps the parse code with seen-tracking and dup handling.
	// Three shapes share the same `if seen { … } else { set; parse }`
	// skeleton — what changes is the seen-branch:
	//   - AllowDups: skip the duplicate value via scan.SkipValue
	//     (first-wins — the second occurrence's value is dropped).
	//   - MultiErr: log a DuplicateKeyError AND skip, so partial decode
	//     stays intact for the rest of the multierr accumulation.
	//   - default: error out immediately.
	emitField := func(b *bytes.Buffer, f FieldInfo, parse string) {
		if f.Inline || !needsSeen(f) {
			b.WriteString(parse)
			return
		}
		set := seenSet(s, f)
		seen := seenAccess(s, f)
		if s.AllowDups {
			fmt.Fprintf(b, `if %s {
	i, err = scan.SkipValue(data, i)
	if err != nil { return result, i, err }
} else {
	%s%s
}
`, seen, set, parse)
			return
		}
		if s.MultiErr {
			fmt.Fprintf(b, `if %s {
	errs = append(errs, &validation.DuplicateKeyError{Field: %q})
	i, err = scan.SkipValue(data, i)
	if err != nil { return result, i, err }
} else {
	%s%s
}
`, seen, f.JSONName, set, parse)
			return
		}
		fmt.Fprintf(b, `if %s { return result, i, &validation.DuplicateKeyError{Field: %q} }
%s%s`, seen, f.JSONName, set, parse)
	}

	b := getSmall()
	defer putSmall(b)
	b.WriteString("switch len(key) {\n")
	for _, n := range lens {
		fs := byLen[n]
		fmt.Fprintf(b, "case %d:\n", n)
		if len(fs) == 1 {
			f := fs[0]
			fmt.Fprintf(b, "if key == %q {\n", f.JSONName)
			emitField(b, f, captureRenderField(f, "result."+f.GoName, "i"))
			b.WriteString("} else {\n")
			b.WriteString(unknownKey(s, "i"))
			b.WriteString("}\n")
			continue
		}
		b.WriteString("switch key {\n")
		for _, f := range fs {
			fmt.Fprintf(b, "case %q:\n", f.JSONName)
			emitField(b, f, captureRenderField(f, "result."+f.GoName, "i"))
		}
		b.WriteString("default:\n")
		b.WriteString(unknownKey(s, "i"))
		b.WriteString("}\n")
	}
	b.WriteString("default:\n")
	b.WriteString(unknownKey(s, "i"))
	b.WriteString("}\n")
	return b.String()
}

// keyValidateAndMod emits mods and validation on the map key variable
// (keyRef). The rules come from the `keys:` tag bucket. Error-return shape
// is the 3-tuple `(result, 0, err)` that map/slice readers use, so any
// `return result, err` baked into renderValidationOn is rewritten.
func keyValidateAndMod(f FieldInfo, keyRef string) string {
	if f.NoValidate || (len(f.KeyMods) == 0 && len(f.KeyValidation) == 0) {
		return ""
	}
	var out bytes.Buffer
	if len(f.KeyMods) > 0 {
		out.WriteString(renderMods(f.KeyMods, keyRef, "string", KindString))
	}
	if len(f.KeyValidation) > 0 {
		code := renderValidationOn(f.KeyValidation, keyRef, f.JSONName+".key", KindString, f.MultiErr)
		if !f.MultiErr {
			code = strings.ReplaceAll(code, "return result, ", "return result, i, ")
		}
		out.WriteString(code)
	}
	return out.String()
}

// renderMap emits map[string]V decode. Accepts `null` → leave field nil
// (matches stdlib `encoding/json`).
// renderMap emits map decode for the byte path. err is from the
// DecodeFrom function-body scope; no local decl needed. Empty `{}` ->
// non-nil empty (stdlib parity); else fresh make() with optional sizing
// hint. The surrounding DecodeFrom's `var result T` builds fresh, so
// ref is always nil here — no reuse branch to emit. Maps don't expose
// interior pointers (`&m[k]` is illegal), so unlike slices we can't
// decode struct elems directly into the slot. We decode into `mv` (or
// the inline scanner's `mn` for primitives) and assign at the end.
func renderMap(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	makeExpr := fmt.Sprintf("make(%s)", f.GoType)
	if cap := mapPreallocCap(f); cap > 0 {
		makeExpr = fmt.Sprintf("make(%s, %d)", f.GoType, cap)
	}
	b.WriteString("{\n")
	inlineSkipWS(b, posVar)
	inlineNullPeek(b, posVar)
	fmt.Fprintf(b, `} else {
	if %[1]s >= len(data) || data[%[1]s] != '{' { return result, i, scan.ErrBadObject }
	%[1]s++
`, posVar)
	inlineSkipWS(b, posVar)
	fmt.Fprintf(b, `	if %[1]s < len(data) && data[%[1]s] == '}' {
		%[2]s = %[3]s{}
	} else {
		%[2]s = %[4]s
	}
	for %[1]s < len(data) && data[%[1]s] != '}' {
		var mk string
`, posVar, ref, f.GoType, makeExpr)
	inlineScanString(b, posVar, "mk", posVar)
	b.WriteString(keyValidateAndMod(f, "mk"))
	inlineSkipWS(b, posVar)
	fmt.Fprintf(b, `		if %[1]s >= len(data) || data[%[1]s] != ':' { return result, i, scan.ErrBadObject }
		%[1]s++
`, posVar)
	inlineSkipWS(b, posVar)

	mapTarget := fmt.Sprintf("%s[mk]", ref)
	switch f.ElemKind {
	case KindString:
		b.WriteString("var mv string\n")
		inlineScanString(b, posVar, "mv", posVar)
		fmt.Fprintf(b, "%s = mv\n", mapTarget)
	case KindBool:
		fmt.Fprintf(b, `mv, mk2, err := scan.Bool(data, %s)
if err != nil { return result, i, err }
%s = mv
%s = mk2
`, posVar, mapTarget, posVar)
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		assign := fmt.Sprintf("%s = mn", mapTarget)
		if f.ElemType != "int64" {
			assign = fmt.Sprintf("%s = %s(mn)", mapTarget, f.ElemType)
		}
		b.WriteString("var mn int64\n")
		inlineScanInt64(b, posVar, "mn", "")
		fmt.Fprintf(b, "%s\n", assign)
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		assign := fmt.Sprintf("%s = mn", mapTarget)
		if f.ElemType != "uint64" {
			assign = fmt.Sprintf("%s = %s(mn)", mapTarget, f.ElemType)
		}
		b.WriteString("var mn uint64\n")
		inlineScanUint64(b, posVar, "mn", "")
		fmt.Fprintf(b, "%s\n", assign)
	case KindFloat32, KindFloat64:
		assign := fmt.Sprintf("%s = mv", mapTarget)
		if f.ElemKind == KindFloat32 {
			assign = fmt.Sprintf("%s = float32(mv)", mapTarget)
		}
		fmt.Fprintf(b, `mv, mk2, err := scan.Float64(data, %s)
if err != nil { return result, i, err }
%s
%s = mk2
`, posVar, assign, posVar)
	case KindStruct:
		if isGenerated(f.ElemType) {
			// `var mv` doubles as the value-receiver source and the
			// stored value. `:=` redeclares mv in same scope (since
			// it was just declared), creates fresh mk2 + err.
			fmt.Fprintf(b, `var mv %s
mv, mk2, err := mv.DecodeFrom(data, %s)
if err != nil { return result, i, err }
%s = mv
%s = mk2
`, f.ElemType, posVar, mapTarget, posVar)
		} else {
			fmt.Fprintf(b, `start := %s
mk2, err := scan.SkipValue(data, start)
if err != nil { return result, i, err }
var mv %s
if err := json.Unmarshal(data[start:mk2], &mv); err != nil { return result, i, err }
%s = mv
%s = mk2
`, posVar, f.ElemType, mapTarget, posVar)
		}
	default:
		fmt.Fprintf(b, `mk2, err := scan.SkipValue(data, %s)
if err != nil { return result, i, err }
%s = mk2
`, posVar, posVar)
	}
	// dive-mods on mv — only for string elem; patch ref in the mod output.
	if len(f.ElemMods) > 0 {
		patched := strings.ReplaceAll(renderMods(f.ElemMods, "mvx", f.ElemType, f.ElemKind), "mvx", mapTarget)
		b.WriteString(patched)
	}
	if len(f.ElemValidation) > 0 {
		code := renderValidationOn(f.ElemValidation, mapTarget, f.JSONName+".value", f.ElemKind, f.MultiErr)
		code = strings.ReplaceAll(code, "return result, &validation.", "return result, i, &validation.")
		b.WriteString(code)
	}
	inlineSkipWS(b, posVar)
	fmt.Fprintf(b, `		if %[1]s < len(data) && data[%[1]s] == ',' { %[1]s++; `, posVar)
	inlineSkipWS(b, posVar)
	fmt.Fprintf(b, `			continue }
		break
	}
	if %[1]s >= len(data) || data[%[1]s] != '}' { return result, i, scan.ErrBadObject }
	%[1]s++
}
}
`, posVar)
}

// renderBytes emits bytes decode for (base64/hex/array).
func renderBytes(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	if f.Format == "array" {
		b.WriteString("{\n")
		inlineSkipWS(b, posVar)
		fmt.Fprintf(b, `if %[1]s >= len(data) || data[%[1]s] != '[' { return result, i, scan.ErrBadArray }
%[1]s++
`, posVar)
		inlineSkipWS(b, posVar)
		fmt.Fprintf(b, `var v uint64
for %[1]s < len(data) && data[%[1]s] != ']' {
	v, %[1]s, err = scan.Uint64(data, %[1]s)
	if err != nil { return result, i, err }
	%[2]s = append(%[2]s, byte(v))
`, posVar, ref)
		inlineSkipWS(b, posVar)
		fmt.Fprintf(b, `	if %[1]s < len(data) && data[%[1]s] == ',' { %[1]s++; `, posVar)
		inlineSkipWS(b, posVar)
		fmt.Fprintf(b, ` continue }
	break
}
if %[1]s >= len(data) || data[%[1]s] != ']' { return result, i, scan.ErrBadArray }
%[1]s++
}
`, posVar)
		return
	}
	// AppendDecode form skips the `[]byte(s)` copy DecodeString does
	// internally. We pre-size dst via DecodedLen so the single
	// allocation is exact and AppendDecode never grows it.
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
	if enc == "" {
		// hex.AppendDecode + hex.DecodedLen.
		b.WriteString("{\n\tvar s string\n\t")
		inlineScanString(b, posVar, "s", posVar)
		fmt.Fprintf(b, `	%s = make([]byte, 0, hex.DecodedLen(len(s)))
	%s, err = hex.AppendDecode(%s, unsafe.Slice(unsafe.StringData(s), len(s)))
	if err != nil { return result, i, err }
}
`, ref, ref, ref)
		return
	}
	b.WriteString("{\n\tvar s string\n\t")
	inlineScanString(b, posVar, "s", posVar)
	fmt.Fprintf(b, `	%s = make([]byte, 0, %s(len(s)))
	%s, err = %s.AppendDecode(%s, unsafe.Slice(unsafe.StringData(s), len(s)))
	if err != nil { return result, i, err }
}
`, ref, dlen, ref, enc, ref)
}

// renderTime emits time.Time decode decode.
func renderTime(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	layout, numeric := timeLayoutExpr(f.Format)
	if numeric != "" {
		// `format:unix` reads a JSON number that may carry fractional
		// seconds (jsonv2 emits float when nanos != 0). Parse as float
		// and split into (sec, nsec) so the wire is fully round-trip
		// safe. Other unix* variants are integer-granular by unit.
		if numeric == "Unix" {
			fmt.Fprintf(b, `{
	var f float64
	f, %s, err = scan.Float64(data, %s)
	if err != nil { return result, i, err }
	sec := int64(f)
	nsec := int64((f - float64(sec)) * 1e9)
	%s = time.Unix(sec, nsec)
}
`, posVar, posVar, ref)
			return
		}
		ctor := map[string]string{
			"UnixMilli": "time.UnixMilli(n)",
			"UnixMicro": "time.UnixMicro(n)",
			"UnixNano":  "time.Unix(0, n)",
		}[numeric]
		fmt.Fprintf(b, `{
	var n int64
	n, %s, err = scan.Int64(data, %s)
	if err != nil { return result, i, err }
	%s = %s
}
`, posVar, posVar, ref, ctor)
		return
	}
	b.WriteString("{\nvar s string\n")
	inlineScanString(b, posVar, "s", posVar)
	fmt.Fprintf(b, `%s, err = time.Parse(%s, s)
if err != nil { return result, i, err }
}
`, ref, layout)
}

// renderDuration emits time.Duration decode decode.
func renderDuration(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	switch f.Format {
	case "sec":
		fmt.Fprintf(b, `{
	var v float64
	v, %s, err = scan.Float64(data, %s)
	if err != nil { return result, i, err }
	%s = time.Duration(v * float64(time.Second))
}
`, posVar, posVar, ref)
		return
	case "milli", "micro", "nano":
		unit := map[string]string{
			"milli": "time.Millisecond",
			"micro": "time.Microsecond",
			"nano":  "time.Nanosecond",
		}[f.Format]
		fmt.Fprintf(b, `{
	var n int64
	n, %s, err = scan.Int64(data, %s)
	if err != nil { return result, i, err }
	%s = time.Duration(n) * %s
}
`, posVar, posVar, ref, unit)
		return
	}
	b.WriteString("{\nvar s string\n")
	inlineScanString(b, posVar, "s", posVar)
	fmt.Fprintf(b, `%s, err = time.ParseDuration(s)
if err != nil { return result, i, err }
}
`, ref)
}

// renderNetIP / renderNetipAddr / renderNetipPrefix.
func renderNetIP(b *bytes.Buffer, ref, posVar string) {
	b.WriteString("{\nvar s string\n")
	inlineScanString(b, posVar, "s", posVar)
	fmt.Fprintf(b, `%[1]s = net.ParseIP(s)
if %[1]s == nil { return result, i, fmt.Errorf("invalid IP") }
}
`, ref)
}

func renderNetipAddr(b *bytes.Buffer, ref, posVar string) {
	b.WriteString("{\nvar s string\n")
	inlineScanString(b, posVar, "s", posVar)
	fmt.Fprintf(b, `%s, err = netip.ParseAddr(s)
if err != nil { return result, i, err }
}
`, ref)
}

func renderNetipPrefix(b *bytes.Buffer, ref, posVar string) {
	b.WriteString("{\nvar s string\n")
	inlineScanString(b, posVar, "s", posVar)
	fmt.Fprintf(b, `%s, err = netip.ParsePrefix(s)
if err != nil { return result, i, err }
}
`, ref)
}

// renderCrossPkgStructDecode emits the decode body for a cross-package /
// unannotated struct field. When f.Iface is resolved (packages-aware
// loader gave us full type info), branches directly to the appropriate
// method based on the type's actual interface implementations — zero
// runtime probes. Without resolution (AST-only mode), falls through to
// the runtime-probe cascade so the generator still produces correct code.
func renderCrossPkgStructDecode(f FieldInfo, ref, posVar string) string {
	if f.Iface.Resolved {
		switch {
		case f.Iface.ByteDecoder:
			// Type was generated by ggen in another package — call
			// DecodeFrom directly. Same shape as the in-pass branch.
			return fmt.Sprintf(`{
	%s, %s, err = %s.DecodeFrom(data, %s)
	if err != nil { return result, i, err }
}
`, ref, posVar, ref, posVar)

		case f.Iface.JSONUnmarshaler:
			// Has UnmarshalJSON — capture raw JSON span and call it.
			// Avoids reflection-based json.Unmarshal setup.
			return fmt.Sprintf(`{
	start := %s
	%s, err = scan.SkipValue(data, start)
	if err != nil { return result, i, err }
	if err = %s.UnmarshalJSON(data[start:%s]); err != nil { return result, i, err }
}
`, posVar, posVar, ref, posVar)

		case f.Iface.TextUnmarshaler:
			// Type encodes as a JSON string — scan it, alias into []byte
			// without copying, hand to UnmarshalText.
			return fmt.Sprintf(`{
	var ts string
	ts, %s, err = scan.String(data, %s)
	if err != nil { return result, i, err }
	if err = %s.UnmarshalText(unsafe.Slice(unsafe.StringData(ts), len(ts))); err != nil { return result, i, err }
}
`, posVar, posVar, ref)

		default:
			// Static analysis says: implements none of our hot paths.
			// Skip all runtime probes; go straight to encoding/json.
			return fmt.Sprintf(`{
	start := %s
	%s, err = scan.SkipValue(data, start)
	if err != nil { return result, i, err }
	if err = json.Unmarshal(data[start:%s], &%s); err != nil { return result, i, err }
}
`, posVar, posVar, posVar, ref)
		}
	}
	// Unresolved (AST-only path, e.g. tests with temp dirs lacking go.mod)
	// — generator can't tell what the type implements, so just emit a
	// plain encoding/json fallback. Stdlib's reflective decoder handles
	// MarshalJSON / UnmarshalText hooks on its own.
	return fmt.Sprintf(`{
	start := %s
	%s, err = scan.SkipValue(data, start)
	if err != nil { return result, i, err }
	if err = json.Unmarshal(data[start:%s], &%s); err != nil { return result, i, err }
}
`, posVar, posVar, posVar, ref)
}

// renderCrossPkgStructAppend is the marshal counterpart for cross-package
// struct fields. Resolved → static branch on what the type implements.
// Unresolved → runtime cascade.
func renderCrossPkgStructAppend(f FieldInfo, ref string) string {
	if f.Iface.Resolved {
		switch {
		case f.Iface.AppendJSON:
			// Same shape as in-pass branch — direct AppendJSON call.
			return fmt.Sprintf("if dst, err = %s.AppendJSON(dst); err != nil { return dst, err }\n", ref)

		case f.Iface.JSONMarshaler:
			return fmt.Sprintf(`{
	var b []byte
	b, err = %s.MarshalJSON()
	if err != nil { return dst, err }
	dst = append(dst, b...)
}
`, ref)

		case f.Iface.TextAppender:
			// Direct AppendText is preferred over MarshalText: no fresh
			// []byte alloc, no intermediate string. Go 1.24+ encoding.TextAppender.
			return fmt.Sprintf(`dst = append(dst, '"')
if dst, err = %s.AppendText(dst); err != nil { return dst, err }
dst = append(dst, '"')
`, ref)

		case f.Iface.TextMarshaler:
			return fmt.Sprintf(`{
	var t []byte
	t, err = %s.MarshalText()
	if err != nil { return dst, err }
	dst = append(dst, '"')
	dst = %s(dst, encode.BytesToString(t))
}
`, ref, appendStrFn(f.HTMLEscape))

		default:
			return fmt.Sprintf(`{
	var b []byte
	b, err = json.Marshal(%s)
	if err != nil { return dst, err }
	dst = append(dst, b...)
}
`, ref)
		}
	}
	// Unresolved (AST-only) — plain encoding/json fallback.
	return fmt.Sprintf(`{
	var b []byte
	b, err = json.Marshal(%s)
	if err != nil { return dst, err }
	dst = append(dst, b...)
}
`, ref)
}

// renderCrossPkgStructStreamDecode is the streaming counterpart of
// renderCrossPkgStructDecode. Same static-vs-runtime branching.
func renderCrossPkgStructStreamDecode(f FieldInfo, ref, posVar string) string {
	if f.Iface.Resolved {
		switch {
		case f.Iface.StreamDecoder:
			return fmt.Sprintf(`{
	%s, %s, err = %s.DecodeStreamFrom(s, %s)
	if err != nil { return result, i, err }
}
`, ref, posVar, ref, posVar)

		case f.Iface.JSONUnmarshaler:
			return fmt.Sprintf(`{
	start := %s
	prevPin := s.Shift
	s.Shift = false
	%s, err = s.SkipValue(start)
	s.Shift = prevPin
	if err != nil { return result, i, err }
	if err = %s.UnmarshalJSON(s.Bytes()[start:%s]); err != nil { return result, i, err }
}
`, posVar, posVar, ref, posVar)

		case f.Iface.TextUnmarshaler:
			return fmt.Sprintf(`{
	var ts string
	ts, %s, err = s.String(%s)
	if err != nil { return result, i, err }
	if err = %s.UnmarshalText(unsafe.Slice(unsafe.StringData(ts), len(ts))); err != nil { return result, i, err }
}
`, posVar, posVar, ref)

		default:
			return fmt.Sprintf(`{
	start := %s
	prevPin := s.Shift
	s.Shift = false
	%s, err = s.SkipValue(start)
	s.Shift = prevPin
	if err != nil { return result, i, err }
	if err = json.Unmarshal(s.Bytes()[start:%s], &%s); err != nil { return result, i, err }
}
`, posVar, posVar, posVar, ref)
		}
	}
	// Unresolved (AST-only) — plain encoding/json fallback.
	return fmt.Sprintf(`{
	start := %s
	prevPin := s.Shift
	s.Shift = false
	%s, err = s.SkipValue(start)
	s.Shift = prevPin
	if err != nil { return result, i, err }
	if err = json.Unmarshal(s.Bytes()[start:%s], &%s); err != nil { return result, i, err }
}
`, posVar, posVar, posVar, ref)
}

// renderRawJSON aliases data[start:end] into the field — zero copy.
// Works for both json.RawMessage and jsontext.Value because both have
// underlying type []byte.
func renderRawJSON(b *bytes.Buffer, ref, posVar string) {
	fmt.Fprintf(b, `{
	start := %s
	%s, err = scan.SkipValue(data, start)
	if err != nil { return result, i, err }
	%s = data[start:%s]
}
`, posVar, posVar, ref, posVar)
}

// renderURL parses a JSON string via url.Parse. The dereference is
// safe because Parse returns a non-nil *URL on success.
func renderURL(b *bytes.Buffer, ref, posVar string) {
	b.WriteString("{\nvar s string\n")
	inlineScanString(b, posVar, "s", posVar)
	fmt.Fprintf(b, `var u *url.URL
u, err = url.Parse(s)
if err != nil { return result, i, err }
%s = *u
}
`, ref)
}

// renderBigInt reads a bare JSON number, hands the raw bytes to
// big.Int.SetString. The number can be arbitrarily long — no overflow.
// The literal is aliased through unsafe.String — SetString reads it left
// to right and copies the digits into its own internal storage, so the
// alias is dead by the time data could be mutated.
func renderBigInt(b *bytes.Buffer, ref, posVar string) {
	fmt.Fprintf(b, `{
	start := %s
	%s, err = scan.SkipValue(data, start)
	if err != nil { return result, i, err }
	if _, ok := (&%s).SetString(unsafe.String(unsafe.SliceData(data[start:]), %s-start), 10); !ok {
		return result, i, scan.ErrBadNumber
	}
}
`, posVar, posVar, ref, posVar)
}

// renderBigFloat reads a JSON-string-wrapped numeric literal into big.Float
// at the default precision. Wrapping matches jsonv2's wire format for
// big.Float; bare numbers are not accepted (use big.Int or float64 for those).
func renderBigFloat(b *bytes.Buffer, ref, posVar string) {
	b.WriteString("{\nvar s string\n")
	inlineScanString(b, posVar, "s", posVar)
	fmt.Fprintf(b, `if _, _, err := (&%s).Parse(s, 10); err != nil {
	return result, i, err
}
}
`, ref)
}

// renderBigRat reads a JSON string of the form "num" or "num/denom"
// and feeds it to big.Rat.SetString. Lossless — fractions stay exact.
func renderBigRat(b *bytes.Buffer, ref, posVar string) {
	b.WriteString("{\nvar s string\n")
	inlineScanString(b, posVar, "s", posVar)
	fmt.Fprintf(b, `if _, ok := (&%s).SetString(s); !ok {
	return result, i, scan.ErrBadNumber
}
}
`, ref)
}

// renderSQLNull emits decode for a database/sql.NullX field. Probes for
// `null` first; on a value, parses with the inner-kind primitive and sets
// Valid=true. The outer-scope local is `nv` (null-inner-value) — chosen
// to avoid collisions with the `v` that the time/string sub-renderers
// declare internally.
func renderSQLNull(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	spec, ok := SQLNullSpec(f.GoType)
	if !ok {
		return
	}
	var inner bytes.Buffer
	switch spec.Inner {
	case KindString:
		inner.WriteString("var nv string\n")
		inlineScanString(&inner, posVar, "nv", posVar)
	case KindBool:
		inner.WriteString("var nv bool\n")
		fmt.Fprintf(&inner, "nv, %s, err = scan.Bool(data, %s)\n", posVar, posVar)
		inner.WriteString("if err != nil { return result, i, err }\n")
	case KindInt64:
		inner.WriteString("var nv int64\n")
		inlineScanInt64(&inner, posVar, "nv", "")
	case KindInt32, KindInt16:
		fmt.Fprintf(&inner, "var nv %s\n", spec.Type)
		inlineScanInt64(&inner, posVar, "nv", spec.Type)
	case KindUint8:
		fmt.Fprintf(&inner, "var nv %s\n", spec.Type)
		inlineScanUint64(&inner, posVar, "nv", spec.Type)
	case KindFloat64:
		inner.WriteString("var nv float64\n")
		fmt.Fprintf(&inner, "nv, %s, err = scan.Float64(data, %s)\n", posVar, posVar)
		inner.WriteString("if err != nil { return result, i, err }\n")
	case KindTime:
		tf := FieldInfo{Format: f.Format}
		inner.WriteString("var nv time.Time\n")
		renderTime(&inner, tf, "nv", posVar)
	}
	fmt.Fprintf(b, `{
	if %s+4 <= len(data) && data[%s] == 'n' && data[%s+1] == 'u' && data[%s+2] == 'l' && data[%s+3] == 'l' {
		%s = sql.%s{}
		%s += 4
	} else {
		%s
		%s = sql.%s{%s: nv, Valid: true}
	}
}
`, posVar, posVar, posVar, posVar, posVar,
		ref, sqlTypeName(f.GoType), posVar,
		inner.String(),
		ref, sqlTypeName(f.GoType), spec.Field)
}

// sqlTypeName returns the bare type name from a `sql.NullX` qualified name.
func sqlTypeName(goType string) string {
	if i := strings.IndexByte(goType, '.'); i >= 0 {
		return goType[i+1:]
	}
	return goType
}

// renderAny captures the raw JSON span and hands it to encoding/json,
// which decodes into Go's reflective any (map/slice/string/float64/bool/nil).
// Slow path on purpose — a hand-rolled any decoder is a separate project.
func renderAny(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	fn := "scan.Any"
	if f.UseNumber {
		fn = "scan.AnyNumber"
	}
	fmt.Fprintf(b, `{
	%s, %s, err = %s(data, %s)
	if err != nil { return result, i, err }
}
`, ref, posVar, fn, posVar)
}

// needsSeen reports whether a seen<Field> bool is required for f inside
// struct s. The flag is always tracked except for inline catch-all maps:
//   - default mode uses it as a duplicate-key guard (error on second hit).
//   - AllowDups mode uses it to skip subsequent occurrences (first-wins
//     semantics — the first key-value pair is parsed, later ones are
//     advanced past via scan.SkipValue without being decoded).
//   - validation mode uses it for the required-field post-loop check.
//
// Inline fields don't take this path; they're absorbed into the catch-all
// map regardless of how many times their JSON key appears.
func needsSeen(f FieldInfo) bool {
	return !f.Inline
}

// seenBitmaskThreshold is the field count above which the codegen swaps
// per-field `bool` locals for a packed bitmask. Bools are simpler and
// fit in registers for narrow structs; for wide structs, the cumulative
// stack frame (1 byte/field) and cache pressure dominate, and a single
// uint64 (or array thereof for >64 fields) wins.
const seenBitmaskThreshold = 32

// useSeenBitmask reports whether struct s should use the packed-bitmask
// shape for its seen-tracking instead of per-field bools.
func useSeenBitmask(s StructInfo) bool {
	if len(s.Fields) <= seenBitmaskThreshold {
		return false
	}
	for _, f := range s.Fields {
		if !f.Inline && needsSeen(f) {
			return true
		}
	}
	return false
}

// seenBitIndex assigns a stable bit index to f based on its position in
// s.Fields. Inline fields are skipped (they don't need seen tracking) but
// still occupy an index — small waste, simpler addressing.
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

// seenAccess returns the read expression for f's seen bit. In bool mode
// it's the local `seen<GoName>`; in bitmask mode it's `seen & (1<<N)
// != 0` (or the array-indexed form for >64 fields).
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

// unknownKey emits code for the default branch of the key
// switch. Three modes, in precedence order:
//  1. Struct has an inline catch-all map field — absorb the unknown key/value.
//  2. s.IgnoreUnknown — SkipValue and continue.
//  3. Default — return a validation.Error{UnknownKey}.
//
// `key` must already be populated by inlineScanString.
func unknownKey(s StructInfo, posVar string) string {
	if inline := s.InlineField(); inline.Inline {
		anyFn := "scan.Any"
		if s.UseNumber {
			anyFn = "scan.AnyNumber"
		}
		return fmt.Sprintf(`if result.%s == nil { result.%s = make(%s) }
result.%s[key], %s, err = %s(data, %s)
if err != nil { return result, i, err }
`, inline.GoName, inline.GoName, inline.GoType, inline.GoName, posVar, anyFn, posVar)
	}
	if s.IgnoreUnknown {
		return fmt.Sprintf(`%s, err = scan.SkipValue(data, %s)
if err != nil { return result, i, err }
`, posVar, posVar)
	}
	if s.MultiErr {
		return fmt.Sprintf(`errs = append(errs, &validation.UnknownKeyError{Field: key})
%s, err = scan.SkipValue(data, %s)
if err != nil { return result, i, err }
`, posVar, posVar)
	}
	return "return result, i, &validation.UnknownKeyError{Field: key}\n"
}

// validateAndMod emits mods + validation for a field inline in the decoder body.
// Reuses renderMods / renderValidationOn; patches stop-on-first return shape
// from "(T, error)" to "(T, int, error)". When f.MultiErr is on, the reused
// code appends to an `errs` slice instead of returning, so no patch is
// needed. Skipped entirely when f.NoValidate.
func validateAndMod(f FieldInfo, ref string) string {
	var out bytes.Buffer
	if len(f.Mods) > 0 {
		out.WriteString(renderMods(f.Mods, ref, f.GoType, f.Kind))
	}
	if len(f.Validation) > 0 {
		code := renderValidationOn(f.Validation, ref, f.JSONName, f.Kind, f.MultiErr)
		if !f.MultiErr {
			code = strings.ReplaceAll(code, "return result, ", "return result, i, ")
		}
		out.WriteString(code)
	}
	return out.String()
}

// renderField emits the body of a single case: read the value via scan
// helpers, assign into ref, advance pos (`posVar`) to the byte after the
// parsed value. Appends mods + validation unless the struct is novalidate.
// renderStringTag handles json:",string" — reads a quoted JSON string
// and parses its textual contents via strconv into the field's numeric /
// bool type.
func renderStringTag(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	b.WriteString("{\nvar sv string\n")
	inlineScanString(b, posVar, "sv", posVar)
	switch f.Kind {
	case KindBool:
		fmt.Fprintf(b, `switch sv {
case "true": %s = true
case "false": %s = false
default: return result, i, scan.ErrBadBool
}
`, ref, ref)
	case KindFloat32, KindFloat64:
		b.WriteString("f, err := strconv.ParseFloat(sv, 64)\nif err != nil { return result, i, err }\n")
		if f.Kind == KindFloat32 {
			fmt.Fprintf(b, "%s = float32(f)\n", ref)
		} else {
			fmt.Fprintf(b, "%s = f\n", ref)
		}
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		b.WriteString("u, err := strconv.ParseUint(sv, 10, 64)\nif err != nil { return result, i, err }\n")
		if f.Kind == KindUint64 {
			fmt.Fprintf(b, "%s = u\n", ref)
		} else {
			fmt.Fprintf(b, "%s = %s(u)\n", ref, f.GoType)
		}
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		b.WriteString("n, err := strconv.ParseInt(sv, 10, 64)\nif err != nil { return result, i, err }\n")
		if f.Kind == KindInt64 {
			fmt.Fprintf(b, "%s = n\n", ref)
		} else {
			fmt.Fprintf(b, "%s = %s(n)\n", ref, f.GoType)
		}
	case KindString:
		fmt.Fprintf(b, "%s = sv\n", ref)
	}
	b.WriteString("}\n")
}

// captureRenderField runs renderField into a fresh builder and returns
// the result as a string. Used by renderDispatch's emitField wrapper,
// which composes the per-field body into seen-guard wrappers via %s.
func captureRenderField(f FieldInfo, ref, posVar string) string {
	b := getSmall()
	defer putSmall(b)
	renderField(b, f, ref, posVar)
	return b.String()
}

func renderField(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	if f.String {
		renderStringTag(b, f, ref, posVar)
		if !f.NoValidate {
			b.WriteString(validateAndMod(f, ref))
		}
		return
	}
	if f.Pointer {
		// null → nil; otherwise decode into a stack-local of the pointee
		// type and take its address. Strip Pointer for the inner recursion.
		inner := f
		inner.Pointer = false
		if inner.PointeeType != "" {
			inner.GoType = inner.PointeeType
		}
		// Custom `@Func` rules apply to the field's exact type — `*T` for
		// pointer fields. Built-in rules (gte, minlen, …) operate on the
		// deref'd value as before, since they're typed against the
		// underlying kind. Partition here, run @-rules on `ref` (the
		// pointer) AFTER the if/else.
		builtinV, customV := partitionCustomValidation(f.Validation)
		builtinM, customM := partitionCustomMods(f.Mods)
		inner.Validation = builtinV
		inner.Mods = builtinM
		fmt.Fprintf(b, `if %s+4 <= len(data) && data[%s] == 'n' && data[%s+1] == 'u' && data[%s+2] == 'l' && data[%s+3] == 'l' {
	%s = 4 + %s
	%s = nil
} else {
	var v %s
	`, posVar, posVar, posVar, posVar, posVar,
			posVar, posVar,
			ref,
			inner.GoType)
		renderField(b, inner, "v", posVar)
		fmt.Fprintf(b, `	%s = &v
}
`, ref)
		if !f.NoValidate && (len(customV) > 0 || len(customM) > 0) {
			outer := f
			outer.Validation = customV
			outer.Mods = customM
			b.WriteString(validateAndMod(outer, ref))
		}
		return
	}
	switch f.Kind {
	case KindString:
		inlineScanString(b, posVar, ref, posVar)
	case KindBool:
		fmt.Fprintf(b, "%s, %s, err = scan.Bool(data, %s)\n", ref, posVar, posVar)
		b.WriteString("if err != nil { return result, i, err }\n")
	case KindInt, KindInt8, KindInt16, KindInt32:
		inlineScanInt64(b, posVar, ref, f.GoType)
	case KindInt64:
		inlineScanInt64(b, posVar, ref, "")
	case KindUint, KindUint8, KindUint16, KindUint32:
		inlineScanUint64(b, posVar, ref, f.GoType)
	case KindUint64:
		inlineScanUint64(b, posVar, ref, "")
	case KindFloat32:
		fmt.Fprintf(b, `var fv float64
fv, %s, err = scan.Float64(data, %s)
if err != nil { return result, i, err }
%s = float32(fv)
`, posVar, posVar, ref)
	case KindFloat64:
		fmt.Fprintf(b, `%s, %s, err = scan.Float64(data, %s)
if err != nil { return result, i, err }
`, ref, posVar, posVar)
	case KindStruct:
		if isGenerated(f.GoType) {
			// Receiver is the existing field — value-receiver method
			// reads the field's current value (zero on fresh decode) and
			// returns a fresh value we write back into the field.
			fmt.Fprintf(b, `%s, %s, err = %s.DecodeFrom(data, %s)
if err != nil { return result, i, err }
`, ref, posVar, ref, posVar)
		} else {
			b.WriteString(renderCrossPkgStructDecode(f, ref, posVar))
		}
	case KindSlice:
		renderSlice(b, f, ref, posVar)
	case KindArray:
		emitByteArrayRead(b, f, ref, posVar, 0)
	case KindMap:
		renderMap(b, f, ref, posVar)
	case KindBytes:
		renderBytes(b, f, ref, posVar)
	case KindTime:
		renderTime(b, f, ref, posVar)
	case KindDuration:
		renderDuration(b, f, ref, posVar)
	case KindNetIP:
		renderNetIP(b, ref, posVar)
	case KindNetipAddr:
		renderNetipAddr(b, ref, posVar)
	case KindNetipPrefix:
		renderNetipPrefix(b, ref, posVar)
	case KindRawJSON:
		renderRawJSON(b, ref, posVar)
	case KindURL:
		renderURL(b, ref, posVar)
	case KindBigInt:
		renderBigInt(b, ref, posVar)
	case KindBigFloat:
		renderBigFloat(b, ref, posVar)
	case KindBigRat:
		renderBigRat(b, ref, posVar)
	case KindSQLNull:
		renderSQLNull(b, f, ref, posVar)
	case KindAny:
		renderAny(b, f, ref, posVar)
	default:
		fmt.Fprintf(b, `k, err := scan.SkipValue(data, %s)
if err != nil { return result, i, err }
%s = k
`, posVar, posVar)
	}
	// Post-decode: mods then validation. The `seen<GoName>` bool is set
	// by the caller (renderDispatch emits it), not here.
	if !f.NoValidate {
		b.WriteString(validateAndMod(f, ref))
	}
}

// renderSlice is the depth-0 entry point into the recursive slice emitter.
// See emitByteSliceRead for the bulk of the work.
func renderSlice(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	emitByteSliceRead(b, f, ref, posVar, 0)
}

// peelSliceField returns the FieldInfo describing one level down for a
// slice-of-slice or slice-of-array element. Mod / validation rules shift up
// by one level (InnerValidation[0] → ElemValidation, etc.). The outer
// container's kind (slice or array) is inferred from f.ElemKind at the
// parent call — this helper is invoked after the parent confirms ElemKind
// is either KindSlice or KindArray.
func peelSliceField(f FieldInfo) FieldInfo {
	innerGoType := f.ElemType
	innerElem, innerKind, innerLen := stripOneContainer(innerGoType)
	inner := FieldInfo{
		GoType:     innerGoType,
		Kind:       f.ElemKind, // KindSlice or KindArray — the layer we're now inside
		ArrayLen:   f.ElemArrayLen,
		ElemType:   innerElem,
		ElemKind:   innerKind,
		JSONName:   f.JSONName + "[]",
		MultiErr:   f.MultiErr,
		NoValidate: f.NoValidate,
		AllowDups:  f.AllowDups,
		// HintLen=-1 means "unset" — preallocCap then falls through to
		// len/minlen/kind-based default. Without this the zero-value 0
		// reads as "user opt-out (no prealloc)" and the inner slice gets
		// cap=0, forcing the 1→2→4→8 growth chain on every nested row.
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
	return inner
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

// emitByteArrayRead is a thin wrapper around emitByteSliceRead for top-level
// [N]T fields. The shared emitter picks the array path based on f.Kind.
func emitByteArrayRead(b *bytes.Buffer, f FieldInfo, dst, posVar string, depth int) {
	emitByteSliceRead(b, f, dst, posVar, depth)
}

// emitByteSliceRead emits a JSON array reader against `data` into `dst`,
// advancing the caller's position variable `posVar`. Handles both slices
// (f.Kind == KindSlice) and fixed-length arrays (f.Kind == KindArray):
//   - slices: pre-sized via preallocCap, appended to
//   - arrays: strict tuple semantics — the JSON array must have exactly
//     f.ArrayLen elements or the decode errors with validation.Error{Len}
//
// All locals carry the `depth` suffix (k0, ev0 at the outermost call; k1,
// ev1 one level in; …) so nested arrays/slices don't collide on names.
func emitByteSliceRead(b *bytes.Buffer, f FieldInfo, dst, posVar string, depth int) {
	isArray := f.Kind == KindArray
	arrayN := f.ArrayLen
	// kvar threads through the caller's position variable directly — no
	// separate `kN := posVar` alias. The whole nest of slice/array decoders
	// shares one position counter (typically `j` at the top level), each
	// inner level advancing it past the bytes it consumed. Only the data
	// locals (`evN`, `idxN`, `slabN`) keep depth suffixes.
	kvar := posVar
	evvar := fmt.Sprintf("ev%d", depth)
	ivar := fmt.Sprintf("idx%d", depth)
	b.WriteString("{\n")
	inlineSkipWS(b, kvar)
	// `null` → leave slice nil and consume the literal. Arrays don't accept
	// null (no nil array values in Go); they still error on non-`[` input.
	if !isArray {
		inlineNullPeek(b, kvar)
		b.WriteString("} else {\n")
	}
	fmt.Fprintf(b, "if %s >= len(data) || data[%s] != '[' { return result, i, scan.ErrBadArray }\n", kvar, kvar)
	fmt.Fprintf(b, "%s++\n", kvar)
	inlineSkipWS(b, kvar)
	slabVar := fmt.Sprintf("slab%d", depth)
	if isArray {
		fmt.Fprintf(b, "var %s int\n", ivar)
		// For arrays of pointers, allocate the slab as an exact-sized
		// heap slice. `[N]E` would land on the stack, then `&slab[i]`
		// at the assignment site forces the whole array to escape — net
		// is still a heap alloc, but copied via the stack frame first.
		// `make([]E, N)` skips the stack hop and avoids the cache-line
		// thrash for large E.
		if f.ElemPointer {
			fmt.Fprintf(b, "%s := make([]%s, %d)\n", slabVar, f.ElemType, arrayN)
		}
	} else {
		sCap, slCap := preallocCap(f)
		if f.ElemPointer {
			fmt.Fprintf(b, "var %s []%s\n", slabVar, f.ElemType)
		}
		fmt.Fprintf(b, "if %s < len(data) && data[%s] == ']' {\n", kvar, kvar)
		fmt.Fprintf(b, "%s = %s{}\n", dst, f.GoType)
		fmt.Fprintf(b, "} else {\n")
		if sCap > 0 {
			fmt.Fprintf(b, "%s = make(%s, 0, %d)\n", dst, f.GoType, sCap)
		} else {
			fmt.Fprintf(b, "%s = %s{}\n", dst, f.GoType)
		}
		if f.ElemPointer {
			fmt.Fprintf(b, "%s = make([]%s, 0, %d)\n", slabVar, f.ElemType, slCap)
		}
		fmt.Fprintf(b, "}\n")
	}
	// Every elem kind decodes IN PLACE into the destination slot — no
	// `var ev0 T` temporary. The slot is:
	//   - [N]*T (slab+array):  `slab[ivar]`
	//   - [N]T  (dst+array):   `dst[ivar]`
	//   - []*T  (slab+slice):  pre-grow slab `append(slab, T{})`,
	//                          target `slab[len-1]`
	//   - []T   (dst+slice):   pre-grow dst   `append(dst, T{})`,
	//                          target `dst[len-1]`
	// `var err error` is hoisted once above the loop for the struct
	// case (byte path has no outer err scope); container recursion has
	// its own err handling.
	directStruct := f.ElemKind == KindStruct && isGenerated(f.ElemType)
	_ = evvar // legacy depth-suffixed name no longer used at this scope
	// err is now hoisted at the DecodeFrom function-body level (renderDecode);
	// no local declaration needed here.
	fmt.Fprintf(b, "for %s < len(data) && data[%s] != ']' {\n", kvar, kvar)
	if isArray {
		// Strict tuple: reject when the JSON array has more elements than
		// the Go [N]T can hold.
		fmt.Fprintf(b, "if %s >= %d { return result, i, %s }\n",
			ivar, arrayN,
			arrayLenErr(f.JSONName, arrayN, ivar))
	}
	if f.ElemPointer {
		// `null` element → nil pointer. Skip the parse + slab work.
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
		b.WriteString("continue }\nbreak\n}\n")
	}
	// Compute the in-place target. For slice cases we pre-grow the
	// slice/slab here so the slot exists before scan writes into it.
	var target string
	switch {
	case isArray && f.ElemPointer:
		target = fmt.Sprintf("%s[%s]", slabVar, ivar)
	case isArray:
		target = fmt.Sprintf("%s[%s]", dst, ivar)
	case f.ElemPointer:
		// Pointer slab holds the pointee type (e.g. []Addr for []*Addr);
		// pre-grow with the pointee's zero value.
		fmt.Fprintf(b, "%s = append(%s, %s)\n", slabVar, slabVar, zeroLit(f.ElemType, f.ElemKind))
		target = fmt.Sprintf("%s[len(%s)-1]", slabVar, slabVar)
	default:
		fmt.Fprintf(b, "%s = append(%s, %s)\n", dst, dst, zeroLit(f.ElemType, f.ElemKind))
		target = fmt.Sprintf("%s[len(%s)-1]", dst, dst)
	}
	switch f.ElemKind {
	case KindString:
		inlineScanString(b, kvar, target, kvar)
	case KindBool:
		fmt.Fprintf(b, "%s, %s, err = scan.Bool(data, %s)\n", target, kvar, kvar)
		b.WriteString("if err != nil { return result, i, err }\n")
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		castFn := ""
		if f.ElemType != "int64" {
			castFn = f.ElemType
		}
		inlineScanInt64(b, kvar, target, castFn)
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		castFn := ""
		if f.ElemType != "uint64" {
			castFn = f.ElemType
		}
		inlineScanUint64(b, kvar, target, castFn)
	case KindFloat32, KindFloat64:
		if f.ElemKind == KindFloat64 {
			fmt.Fprintf(b, "%s, %s, err = scan.Float64(data, %s)\n", target, kvar, kvar)
			b.WriteString("if err != nil { return result, i, err }\n")
		} else {
			b.WriteString("var fv float64\n")
			fmt.Fprintf(b, "fv, %s, err = scan.Float64(data, %s)\n", kvar, kvar)
			b.WriteString("if err != nil { return result, i, err }\n")
			fmt.Fprintf(b, "%s = float32(fv)\n", target)
		}
	case KindStruct:
		if directStruct {
			fmt.Fprintf(b, "%s, %s, err = %s.DecodeFrom(data, %s)\n", target, kvar, target, kvar)
			b.WriteString("if err != nil { return result, i, err }\n")
		} else {
			fmt.Fprintf(b, "%s, err = scan.SkipValue(data, %s)\n", kvar, kvar)
			b.WriteString("if err != nil { return result, i, err }\n")
		}
	case KindSlice, KindArray:
		// Nested container — recurse, peeling one outer [] / [N] off.
		// The recursive emit writes into target (the slot itself).
		emitByteSliceRead(b, peelSliceField(f), target, kvar, depth+1)
	}
	if len(f.ElemMods) > 0 {
		b.WriteString(renderMods(f.ElemMods, target, f.ElemType, f.ElemKind))
	}
	if len(f.ElemValidation) > 0 {
		code := renderValidationOn(f.ElemValidation, target, f.JSONName+"[]", f.ElemKind, f.MultiErr)
		if !f.MultiErr {
			code = strings.ReplaceAll(code, "return result, ", "return result, i, ")
		}
		b.WriteString(code)
	}
	switch {
	case isArray && f.ElemPointer:
		// Slab slot already decoded in-place; publish its address.
		fmt.Fprintf(b, "%s[%s] = &%s[%s]\n", dst, ivar, slabVar, ivar)
		fmt.Fprintf(b, "%s++\n", ivar)
	case isArray:
		// dst[ivar] already decoded in-place.
		fmt.Fprintf(b, "%s++\n", ivar)
	case f.ElemPointer:
		// Slab tail decoded in-place via append+index above; publish addr.
		fmt.Fprintf(b, "%s = append(%s, &%s[len(%s)-1])\n", dst, dst, slabVar, slabVar)
	default:
		// dst tail already decoded in-place via append+index above.
	}
	inlineSkipWS(b, kvar)
	fmt.Fprintf(b, "if %s < len(data) && data[%s] == ',' { %s++; ", kvar, kvar, kvar)
	inlineSkipWS(b, kvar)
	b.WriteString("continue }\n")
	b.WriteString("break\n")
	b.WriteString("}\n")
	fmt.Fprintf(b, "if %s >= len(data) || data[%s] != ']' { return result, i, scan.ErrBadArray }\n", kvar, kvar)
	if isArray {
		fmt.Fprintf(b, "if %s != %d { return result, i, %s }\n",
			ivar, arrayN,
			arrayLenErr(f.JSONName, arrayN, ivar))
	}
	// kvar == posVar (no alias); just step past the closing `]`.
	fmt.Fprintf(b, "%s++\n", posVar)
	if !isArray {
		b.WriteString("}\n") // close else (null-check)
	}
	b.WriteString("}\n") // close outer block
}

// renderStreamDecode emits the streaming counterpart of renderDecode.
// Uses scan.Stream methods which pull more bytes on demand. Buffer backing
// array is fixed-capacity and never reallocates — zero-copy string aliases
// stay valid for the lifetime of the Stream.
func renderStreamDecode(b *bytes.Buffer, s StructInfo) {
	if s.IsAlias {
		b.WriteString(renderAliasStreamDecode(s))
		return
	}
	renderStreamDecodeStruct(b, s)
}

func renderStreamDecodeStruct(b *bytes.Buffer, s StructInfo) {
	b.WriteString("var result " + s.Name + "\n")
	if s.MultiErr {
		b.WriteString("var errs validation.Errors\n")
	}
	if useSeenBitmask(s) {
		b.WriteString(seenDecl(s))
	} else {
		for _, f := range s.Fields {
			if f.Inline {
				continue
			}
			if needsSeen(f) {
				fmt.Fprintf(b, "seen%s := false\n", f.GoName)
			}
		}
	}
	var pl bytes.Buffer
	renderPostLoop(&pl, s)
	plStr := pl.String()
	// Dispatch happens IMMEDIATELY after KeyView so the alias is still
	// valid when the switch compares it against constants. Each case
	// body opens with s.ConsumeColon — that's where the buffer may
	// compact (via SkipSpace's internal shift) and where the alias
	// dies. Cases that capture key (inline catch-all, UnknownKeyError
	// in multierr/immediate-return paths) must clone BEFORE
	// ConsumeColon.
	fmt.Fprintf(b, `i, err := s.ObjectOpen(i)
if err != nil { return result, i, err }
i, err = s.SkipSpace(i)
if err != nil { return result, i, err }
if i >= len(s.Bytes()) { if err = s.ReadMore(i); err != nil { return result, i, err }; i = 0 }
if s.Bytes()[i] == '}' {
%sreturn result, i + 1, nil
}
for {
	var key string
	key, i, err = s.KeyView(i)
	if err != nil { return result, i, err }
	%s	i, err = s.SkipSpace(i)
	if err != nil { return result, i, err }
	if i >= len(s.Bytes()) { if err = s.ReadMore(i); err != nil { return result, i, err }; i = 0 }
	c := s.Bytes()[i]
	if c == ',' {
		i, err = s.SkipSpace(i + 1)
		if err != nil { return result, i, err }
		continue
	}
	if c == '}' {
%s		return result, i + 1, nil
	}
	return result, i, scan.ErrBadObject
}`, plStr, renderStreamDispatch(s), plStr)
}

func renderStreamDispatch(s StructInfo) string {
	byLen := map[int][]FieldInfo{}
	var lens []int
	for _, f := range s.Fields {
		if f.Inline {
			continue
		}
		n := len(f.JSONName)
		if _, seen := byLen[n]; !seen {
			lens = append(lens, n)
		}
		byLen[n] = append(byLen[n], f)
	}
	slices.Sort(lens)

	// Each known-key case opens with s.ConsumeColon — the alias is no
	// longer needed past this point, so the shift it triggers is safe.
	emitField := func(b *bytes.Buffer, f FieldInfo, parse string) {
		b.WriteString("i, err = s.ConsumeColon(i)\nif err != nil { return result, i, err }\n")
		if f.Inline || !needsSeen(f) {
			b.WriteString(parse)
			return
		}
		set := seenSet(s, f)
		seen := seenAccess(s, f)
		if s.AllowDups {
			fmt.Fprintf(b, `if %s {
	i, err = s.SkipValue(i)
	if err != nil { return result, i, err }
} else {
	%s%s
}
`, seen, set, parse)
			return
		}
		if s.MultiErr {
			fmt.Fprintf(b, `if %s {
	errs = append(errs, &validation.DuplicateKeyError{Field: %q})
	i, err = s.SkipValue(i)
	if err != nil { return result, i, err }
} else {
	%s%s
}
`, seen, f.JSONName, set, parse)
			return
		}
		fmt.Fprintf(b, `if %s { return result, i, &validation.DuplicateKeyError{Field: %q} }
%s%s`, seen, f.JSONName, set, parse)
	}

	b := getSmall()
	defer putSmall(b)
	b.WriteString("switch len(key) {\n")
	for _, n := range lens {
		fs := byLen[n]
		fmt.Fprintf(b, "case %d:\n", n)
		if len(fs) == 1 {
			f := fs[0]
			fmt.Fprintf(b, "if key == %q {\n", f.JSONName)
			emitField(b, f, renderStreamField(f, "result."+f.GoName, "i"))
			b.WriteString("} else {\n")
			b.WriteString(streamUnknownKey(s, "i"))
			b.WriteString("}\n")
			continue
		}
		b.WriteString("switch key {\n")
		for _, f := range fs {
			fmt.Fprintf(b, "case %q:\n", f.JSONName)
			emitField(b, f, renderStreamField(f, "result."+f.GoName, "i"))
		}
		b.WriteString("default:\n")
		b.WriteString(streamUnknownKey(s, "i"))
		b.WriteString("}\n")
	}
	b.WriteString("default:\n")
	b.WriteString(streamUnknownKey(s, "i"))
	b.WriteString("}\n")
	return b.String()
}

// renderStreamMap emits map decode for the stream path.
// err comes from the function-body scope (ObjectOpen at the top of the
// regular DecodeStreamFrom, or the synthetic decl in
// renderAliasContainerDecode for alias containers). `null` -> leave nil
// and consume the literal. Empty `{}` -> non-nil empty; else fresh
// make(). Map keys must be detached copies (the map holds the string
// header; the buffer can be overwritten on next ReadMore), so we use
// s.String which copies.
func renderStreamMap(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	makeExpr := fmt.Sprintf("make(%s)", f.GoType)
	if cap := mapPreallocCap(f); cap > 0 {
		makeExpr = fmt.Sprintf("make(%s, %d)", f.GoType, cap)
	}
	fmt.Fprintf(b, `{
%[1]s, err = s.SkipSpace(%[1]s)
if err != nil { return result, i, err }
if %[1]s >= len(s.Bytes()) { if err = s.ReadMore(0); err != nil { return result, i, err } }
if s.Bytes()[%[1]s] == 'n' {
	for ki := 1; ki < 4; ki++ {
		if %[1]s+ki >= len(s.Bytes()) { if err = s.ReadMore(0); err != nil { return result, i, err } }
		if s.Bytes()[%[1]s+ki] != "null"[ki] { return result, i, scan.ErrBadLiteral }
	}
	%[1]s += 4
} else {
	%[1]s, err = s.ObjectOpen(%[1]s)
	if err != nil { return result, i, err }
	%[1]s, err = s.SkipSpace(%[1]s)
	if err != nil { return result, i, err }
	if %[1]s >= len(s.Bytes()) { if err = s.ReadMore(0); err != nil { return result, i, err } }
	if s.Bytes()[%[1]s] == '}' {
		%[2]s = %[3]s{}
	} else {
		%[2]s = %[4]s
	}
	for s.Bytes()[%[1]s] != '}' {
		var mk string
		mk, %[1]s, err = s.String(%[1]s)
		if err != nil { return result, i, err }
		%[5]s		%[1]s, err = s.SkipSpace(%[1]s)
		if err != nil { return result, i, err }
		if %[1]s >= len(s.Bytes()) { if err = s.ReadMore(0); err != nil { return result, i, err } }
		if s.Bytes()[%[1]s] != ':' { return result, i, scan.ErrBadObject }
		%[1]s, err = s.SkipSpace(%[1]s + 1)
		if err != nil { return result, i, err }
`, posVar, ref, f.GoType, makeExpr, keyValidateAndMod(f, "mk"))

	mapTarget := fmt.Sprintf("%s[mk]", ref)
	switch f.ElemKind {
	case KindString:
		fmt.Fprintf(b, `var mv string
mv, %s, err = s.String(%s)
if err != nil { return result, i, err }
%s = mv
`, posVar, posVar, mapTarget)
	case KindBool:
		fmt.Fprintf(b, `var mv bool
mv, %s, err = s.Bool(%s)
if err != nil { return result, i, err }
%s = mv
`, posVar, posVar, mapTarget)
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		assign := fmt.Sprintf("%s = mn", mapTarget)
		if f.ElemType != "int64" {
			assign = fmt.Sprintf("%s = %s(mn)", mapTarget, f.ElemType)
		}
		fmt.Fprintf(b, `var mn int64
mn, %s, err = s.Int64(%s)
if err != nil { return result, i, err }
%s
`, posVar, posVar, assign)
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		assign := fmt.Sprintf("%s = mn", mapTarget)
		if f.ElemType != "uint64" {
			assign = fmt.Sprintf("%s = %s(mn)", mapTarget, f.ElemType)
		}
		fmt.Fprintf(b, `var mn uint64
mn, %s, err = s.Uint64(%s)
if err != nil { return result, i, err }
%s
`, posVar, posVar, assign)
	case KindFloat32, KindFloat64:
		assign := fmt.Sprintf("%s = mv", mapTarget)
		if f.ElemKind == KindFloat32 {
			assign = fmt.Sprintf("%s = float32(mv)", mapTarget)
		}
		fmt.Fprintf(b, `var mv float64
mv, %s, err = s.Float64(%s)
if err != nil { return result, i, err }
%s
`, posVar, posVar, assign)
	case KindStruct:
		if isGenerated(f.ElemType) {
			fmt.Fprintf(b, `var mv %s
mv, %s, err = mv.DecodeStreamFrom(s, %s)
if err != nil { return result, i, err }
%s = mv
`, f.ElemType, posVar, posVar, mapTarget)
		} else {
			fmt.Fprintf(b, `start := %s
prevPin := s.Shift
	s.Shift = false
%s, err = s.SkipValue(start)
s.Shift = prevPin
if err != nil { return result, i, err }
var mv %s
if err := json.Unmarshal(s.Bytes()[start:%s], &mv); err != nil { return result, i, err }
%s = mv
`, posVar, posVar, f.ElemType, posVar, mapTarget)
		}
	default:
		fmt.Fprintf(b, `%s, err = s.SkipValue(%s)
if err != nil { return result, i, err }
`, posVar, posVar)
	}
	if len(f.ElemMods) > 0 {
		patched := strings.ReplaceAll(renderMods(f.ElemMods, "mvx", f.ElemType, f.ElemKind), "mvx", mapTarget)
		b.WriteString(patched)
	}
	if len(f.ElemValidation) > 0 {
		code := renderValidationOn(f.ElemValidation, mapTarget, f.JSONName+".value", f.ElemKind, f.MultiErr)
		code = strings.ReplaceAll(code, "return result, &validation.", "return result, i, &validation.")
		b.WriteString(code)
	}
	fmt.Fprintf(b, `		%[1]s, err = s.SkipSpace(%[1]s)
		if err != nil { return result, i, err }
		if %[1]s >= len(s.Bytes()) { if err = s.ReadMore(0); err != nil { return result, i, err } }
		if s.Bytes()[%[1]s] == ',' { %[1]s, err = s.SkipSpace(%[1]s + 1); if err != nil { return result, i, err }; continue }
		break
	}
	if s.Bytes()[%[1]s] != '}' { return result, i, scan.ErrBadObject }
	%[1]s++
}
}
`, posVar)
}

// --- stream native-type renderers ---

func renderStreamBytes(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	if f.Format == "array" {
		fmt.Fprintf(b, `{
	%s, err = s.ArrayOpen(%s)
	if err != nil { return result, i, err }
	%s, err = s.SkipSpace(%s)
	if err != nil { return result, i, err }
	if %s >= len(s.Bytes()) { if err = s.ReadMore(0); err != nil { return result, i, err } }
	for s.Bytes()[%s] != ']' {
		var v uint64
		v, %s, err = s.Uint64(%s)
		if err != nil { return result, i, err }
		%s = append(%s, byte(v))
		%s, err = s.SkipSpace(%s)
		if err != nil { return result, i, err }
		if %s >= len(s.Bytes()) { if err = s.ReadMore(0); err != nil { return result, i, err } }
		if s.Bytes()[%s] == ',' { %s, err = s.SkipSpace(%s + 1); if err != nil { return result, i, err }; continue }
		break
	}
	if s.Bytes()[%s] != ']' { return result, i, scan.ErrBadArray }
	%s++
}
`, posVar, posVar, posVar, posVar, posVar, posVar, posVar, posVar, ref, ref, posVar, posVar, posVar, posVar, posVar, posVar, posVar, posVar)
		return
	}
	// AppendDecode path: skips the `[]byte(v)` copy DecodeString does.
	// v is already a copied string owned by the decoder (stream path
	// can't alias), so converting back to []byte via unsafe is sound
	// for the duration of the decode call.
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
	if enc == "" {
		fmt.Fprintf(b, `{
	var v string
	v, %s, err = s.String(%s)
	if err != nil { return result, i, err }
	%s = make([]byte, 0, hex.DecodedLen(len(v)))
	%s, err = hex.AppendDecode(%s, unsafe.Slice(unsafe.StringData(v), len(v)))
	if err != nil { return result, i, err }
}
`, posVar, posVar, ref, ref, ref)
		return
	}
	fmt.Fprintf(b, `{
	var v string
	v, %s, err = s.String(%s)
	if err != nil { return result, i, err }
	%s = make([]byte, 0, %s(len(v)))
	%s, err = %s.AppendDecode(%s, unsafe.Slice(unsafe.StringData(v), len(v)))
	if err != nil { return result, i, err }
}
`, posVar, posVar, ref, dlen, ref, enc, ref)
}

func renderStreamTime(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	layout, numeric := timeLayoutExpr(f.Format)
	if numeric != "" {
		if numeric == "Unix" {
			fmt.Fprintf(b, `{
	var f float64
	f, %s, err = s.Float64(%s)
	if err != nil { return result, i, err }
	sec := int64(f)
	nsec := int64((f - float64(sec)) * 1e9)
	%s = time.Unix(sec, nsec)
}
`, posVar, posVar, ref)
			return
		}
		ctor := map[string]string{
			"UnixMilli": "time.UnixMilli(n)",
			"UnixMicro": "time.UnixMicro(n)",
			"UnixNano":  "time.Unix(0, n)",
		}[numeric]
		fmt.Fprintf(b, `{
	var n int64
	n, %s, err = s.Int64(%s)
	if err != nil { return result, i, err }
	%s = %s
}
`, posVar, posVar, ref, ctor)
		return
	}
	fmt.Fprintf(b, `{
	var v string
	v, %s, err = s.String(%s)
	if err != nil { return result, i, err }
	%s, err = time.Parse(%s, v)
	if err != nil { return result, i, err }
}
`, posVar, posVar, ref, layout)
}

func renderStreamDuration(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	switch f.Format {
	case "sec":
		fmt.Fprintf(b, `{
	var v float64
	v, %s, err = s.Float64(%s)
	if err != nil { return result, i, err }
	%s = time.Duration(v * float64(time.Second))
}
`, posVar, posVar, ref)
		return
	case "milli", "micro", "nano":
		unit := map[string]string{
			"milli": "time.Millisecond",
			"micro": "time.Microsecond",
			"nano":  "time.Nanosecond",
		}[f.Format]
		fmt.Fprintf(b, `{
	var n int64
	n, %s, err = s.Int64(%s)
	if err != nil { return result, i, err }
	%s = time.Duration(n) * %s
}
`, posVar, posVar, ref, unit)
		return
	}
	fmt.Fprintf(b, `{
	var v string
	v, %s, err = s.String(%s)
	if err != nil { return result, i, err }
	%s, err = time.ParseDuration(v)
	if err != nil { return result, i, err }
}
`, posVar, posVar, ref)
}

func renderStreamNetIP(b *bytes.Buffer, ref, posVar string) {
	fmt.Fprintf(b, `{
	var v string
	v, %s, err = s.String(%s)
	if err != nil { return result, i, err }
	%s = net.ParseIP(v)
	if %s == nil { return result, i, fmt.Errorf("invalid IP") }
}
`, posVar, posVar, ref, ref)
}

func renderStreamNetipAddr(b *bytes.Buffer, ref, posVar string) {
	fmt.Fprintf(b, `{
	var v string
	v, %s, err = s.String(%s)
	if err != nil { return result, i, err }
	%s, err = netip.ParseAddr(v)
	if err != nil { return result, i, err }
}
`, posVar, posVar, ref)
}

func renderStreamNetipPrefix(b *bytes.Buffer, ref, posVar string) {
	fmt.Fprintf(b, `{
	var v string
	v, %s, err = s.String(%s)
	if err != nil { return result, i, err }
	%s, err = netip.ParsePrefix(v)
	if err != nil { return result, i, err }
}
`, posVar, posVar, ref)
}

// renderStreamRawJSON copies the stream's buffer span into the
// field. Stream methods compact in-place via ReadMore(keep>0), which
// would invalidate the absolute `start` offset — pin the buffer with
// s.Shift = false for the duration of SkipValue so the captured
// span stays valid.
func renderStreamRawJSON(b *bytes.Buffer, ref, posVar string) {
	fmt.Fprintf(b, `{
	start := %s
	prevPin := s.Shift
	s.Shift = false
	%s, err = s.SkipValue(start)
	s.Shift = prevPin
	if err != nil { return result, i, err }
	raw := s.Bytes()[start:%s]
	%s = append(make([]byte, 0, len(raw)), raw...)
}
`, posVar, posVar, posVar, ref)
}

func renderStreamURL(b *bytes.Buffer, ref, posVar string) {
	fmt.Fprintf(b, `{
	var v string
	v, %s, err = s.String(%s)
	if err != nil { return result, i, err }
	u, err := url.Parse(v)
	if err != nil { return result, i, err }
	%s = *u
}
`, posVar, posVar, ref)
}

func renderStreamBigInt(b *bytes.Buffer, ref, posVar string) {
	fmt.Fprintf(b, `{
	start := %s
	prevPin := s.Shift
	s.Shift = false
	%s, err = s.SkipValue(start)
	s.Shift = prevPin
	if err != nil { return result, i, err }
	buf := s.Bytes()
	if _, ok := (&%s).SetString(unsafe.String(unsafe.SliceData(buf[start:]), %s-start), 10); !ok {
		return result, i, scan.ErrBadNumber
	}
}
`, posVar, posVar, ref, posVar)
}

func renderStreamBigFloat(b *bytes.Buffer, ref, posVar string) {
	fmt.Fprintf(b, `{
	var v string
	v, %s, err = s.String(%s)
	if err != nil { return result, i, err }
	if _, _, err := (&%s).Parse(v, 10); err != nil {
		return result, i, err
	}
}
`, posVar, posVar, ref)
}

func renderStreamBigRat(b *bytes.Buffer, ref, posVar string) {
	fmt.Fprintf(b, `{
	var v string
	v, %s, err = s.String(%s)
	if err != nil { return result, i, err }
	if _, ok := (&%s).SetString(v); !ok {
		return result, i, scan.ErrBadNumber
	}
}
`, posVar, posVar, ref)
}

// renderStreamSQLNull is the streaming counterpart of renderSQLNull.
// For non-null values it pre-declares a single intermediate matching the
// scan helper's return type and uses `=` multi-assign so `posVar` is
// updated directly (no `sk`/`ik`/`fk` shuffle). The cast (if any) is
// inlined inside the `sql.NullX{Field: …, Valid: true}` literal.
func renderStreamSQLNull(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	spec, ok := SQLNullSpec(f.GoType)
	if !ok {
		return
	}
	var inner bytes.Buffer
	// valExpr is the value placed in the NullX literal — usually just
	// the intermediate name, possibly wrapped in a cast.
	var valExpr string
	// scanTmpl: declare nv, call the right scan method, check err. Each
	// branch differs only by the inner type and the s.Method() called.
	scanTmpl := func(typ, method string) {
		fmt.Fprintf(&inner, `var nv %[1]s
nv, %[2]s, err = s.%[3]s(%[2]s)
if err != nil { return result, i, err }
`, typ, posVar, method)
	}
	switch spec.Inner {
	case KindString:
		scanTmpl("string", "String")
		valExpr = "nv"
	case KindBool:
		scanTmpl("bool", "Bool")
		valExpr = "nv"
	case KindInt64, KindInt32, KindInt16:
		scanTmpl("int64", "Int64")
		valExpr = "nv"
		if spec.Type != "int64" {
			valExpr = fmt.Sprintf("%s(nv)", spec.Type)
		}
	case KindUint8:
		scanTmpl("uint64", "Uint64")
		valExpr = fmt.Sprintf("%s(nv)", spec.Type)
	case KindFloat64:
		scanTmpl("float64", "Float64")
		valExpr = "nv"
	case KindTime:
		tf := FieldInfo{Format: f.Format}
		inner.WriteString("var nv time.Time\n")
		renderStreamTime(&inner, tf, "nv", posVar)
		valExpr = "nv"
	}
	fmt.Fprintf(b, `{
	if %[1]s >= len(s.Bytes()) { if err = s.ReadMore(0); err != nil { return result, i, err } }
	if s.Bytes()[%[1]s] == 'n' {
		for ki := 1; ki < 4; ki++ {
			if %[1]s+ki >= len(s.Bytes()) { if err = s.ReadMore(0); err != nil { return result, i, err } }
			if s.Bytes()[%[1]s+ki] != "null"[ki] { return result, i, scan.ErrBadLiteral }
		}
		%[2]s = sql.%[3]s{}
		%[1]s += 4
	} else {
		%[4]s
		%[2]s = sql.%[3]s{%[5]s: %[6]s, Valid: true}
	}
}
`, posVar, ref, sqlTypeName(f.GoType), inner.String(), spec.Field, valExpr)
}

func renderStreamAny(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	fn := "s.Any"
	if f.UseNumber {
		fn = "s.AnyNumber"
	}
	fmt.Fprintf(b, `{
	%s, %s, err = %s(%s)
	if err != nil { return result, i, err }
}
`, ref, posVar, fn, posVar)
}

// streamUnknownKey is the stream-scanner counterpart of unknownKey.
// `key` is an alias into the stream buffer (Stream.KeyView is
// zero-copy on the no-escape happy path). ConsumeColon shifts the
// buffer and invalidates the alias, so any value that survives past
// that shift — stored map keys, retained errors — must hold an owned
// copy, captured BEFORE ConsumeColon runs. The immediate-return
// branch is safe to read the alias directly since the function exits
// before any subsequent Stream call.
func streamUnknownKey(s StructInfo, posVar string) string {
	if inline := s.InlineField(); inline.Inline {
		anyFn := "s.Any"
		if s.UseNumber {
			anyFn = "s.AnyNumber"
		}
		return fmt.Sprintf(`ownKey := strings.Clone(key)
%s, err = s.ConsumeColon(%s)
if err != nil { return result, i, err }
if result.%s == nil { result.%s = make(%s) }
result.%s[ownKey], %s, err = %s(%s)
if err != nil { return result, i, err }
`, posVar, posVar, inline.GoName, inline.GoName, inline.GoType, inline.GoName, posVar, anyFn, posVar)
	}
	if s.IgnoreUnknown {
		return fmt.Sprintf(`%s, err = s.ConsumeColon(%s)
if err != nil { return result, i, err }
%s, err = s.SkipValue(%s)
if err != nil { return result, i, err }
`, posVar, posVar, posVar, posVar)
	}
	if s.MultiErr {
		return fmt.Sprintf(`errs = append(errs, &validation.UnknownKeyError{Field: strings.Clone(key)})
%s, err = s.ConsumeColon(%s)
if err != nil { return result, i, err }
%s, err = s.SkipValue(%s)
if err != nil { return result, i, err }
`, posVar, posVar, posVar, posVar)
	}
	return "return result, i, &validation.UnknownKeyError{Field: strings.Clone(key)}\n"
}

func renderStreamStringTag(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	b.WriteString("{\n")
	// Use KeyView (alias into the stream buffer) instead of String
	// (allocating copy). The aliased string only needs to outlive the
	// strconv.ParseX / switch below — no caller dependency afterward.
	// For KindString we DO need a copy since `ref` is stored in the
	// decoded value: explicit `string(sv)` before assignment.
	b.WriteString("var sv string\n")
	fmt.Fprintf(b, "sv, %s, err = s.KeyView(%s)\n", posVar, posVar)
	b.WriteString("if err != nil { return result, i, err }\n")
	switch f.Kind {
	case KindBool:
		fmt.Fprintf(b, `switch sv {
case "true": %s = true
case "false": %s = false
default: return result, i, scan.ErrBadBool
}
`, ref, ref)
	case KindFloat32, KindFloat64:
		b.WriteString("f, err := strconv.ParseFloat(sv, 64)\nif err != nil { return result, i, err }\n")
		if f.Kind == KindFloat32 {
			fmt.Fprintf(b, "%s = float32(f)\n", ref)
		} else {
			fmt.Fprintf(b, "%s = f\n", ref)
		}
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		b.WriteString("u, err := strconv.ParseUint(sv, 10, 64)\nif err != nil { return result, i, err }\n")
		if f.Kind == KindUint64 {
			fmt.Fprintf(b, "%s = u\n", ref)
		} else {
			fmt.Fprintf(b, "%s = %s(u)\n", ref, f.GoType)
		}
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		b.WriteString("n, err := strconv.ParseInt(sv, 10, 64)\nif err != nil { return result, i, err }\n")
		if f.Kind == KindInt64 {
			fmt.Fprintf(b, "%s = n\n", ref)
		} else {
			fmt.Fprintf(b, "%s = %s(n)\n", ref, f.GoType)
		}
	case KindString:
		// KeyView aliases the buffer; copy to detach.
		fmt.Fprintf(b, "%s = string(sv)\n", ref)
	}
	b.WriteString("}\n")
}

func renderStreamField(f FieldInfo, ref, posVar string) string {
	if f.String {
		var out bytes.Buffer
		renderStreamStringTag(&out, f, ref, posVar)
		if !f.NoValidate {
			out.WriteString(validateAndMod(f, ref))
		}
		return out.String()
	}
	if f.Pointer {
		inner := f
		inner.Pointer = false
		if inner.PointeeType != "" {
			inner.GoType = inner.PointeeType
		}
		// See the bytes-path comment: custom `@Func` rules want the `*T`
		// pointer; built-ins want the deref'd value. Partition both lists.
		builtinV, customV := partitionCustomValidation(f.Validation)
		builtinM, customM := partitionCustomMods(f.Mods)
		inner.Validation = builtinV
		inner.Mods = builtinM
		block := fmt.Sprintf(`if %[1]s >= len(s.Bytes()) { if err = s.ReadMore(0); err != nil { return result, i, err } }
if s.Bytes()[%[1]s] == 'n' {
	for ki := 1; ki < 4; ki++ {
		if %[1]s+ki >= len(s.Bytes()) { if err = s.ReadMore(0); err != nil { return result, i, err } }
		if s.Bytes()[%[1]s+ki] != "null"[ki] { return result, i, scan.ErrBadLiteral }
	}
	%[1]s = 4 + %[1]s
	%[2]s = nil
} else {
	var v %[3]s
	%[4]s
	%[2]s = &v
}
`, posVar, ref, inner.GoType, renderStreamField(inner, "v", posVar))
		if !f.NoValidate && (len(customV) > 0 || len(customM) > 0) {
			outer := f
			outer.Validation = customV
			outer.Mods = customM
			block += validateAndMod(outer, ref)
		}
		return block
	}
	b := getSmall()
	defer putSmall(b)
	// primScan: direct LHS multi-assign + err check. Shape is identical
	// across String/Bool/Int64/Uint64/Float64 — only the s.X method name
	// differs.
	primScan := func(method string) {
		fmt.Fprintf(b, `%[1]s, %[2]s, err = s.%[3]s(%[2]s)
if err != nil { return result, i, err }
`, ref, posVar, method)
	}
	// widenedScan: read a wide intermediate (`iv`/`uv`/`fv`) and cast
	// down. Used for narrow ints, narrow uints, and float32. wideType is
	// the wide scan return type (int64/uint64/float64), wideVar is the
	// local name, method is the s.X call, castTo is the destination type
	// (or "float32" for the Float32 case).
	widenedScan := func(wideType, wideVar, method, castTo string) {
		fmt.Fprintf(b, `var %[1]s %[2]s
%[1]s, %[3]s, err = s.%[4]s(%[3]s)
if err != nil { return result, i, err }
%[5]s = %[6]s(%[1]s)
`, wideVar, wideType, posVar, method, ref, castTo)
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
			fmt.Fprintf(b, `%[1]s, %[2]s, err = %[1]s.DecodeStreamFrom(s, %[2]s)
if err != nil { return result, i, err }
`, ref, posVar)
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
		renderStreamNetIP(b, ref, posVar)
	case KindNetipAddr:
		renderStreamNetipAddr(b, ref, posVar)
	case KindNetipPrefix:
		renderStreamNetipPrefix(b, ref, posVar)
	case KindRawJSON:
		renderStreamRawJSON(b, ref, posVar)
	case KindURL:
		renderStreamURL(b, ref, posVar)
	case KindBigInt:
		renderStreamBigInt(b, ref, posVar)
	case KindBigFloat:
		renderStreamBigFloat(b, ref, posVar)
	case KindBigRat:
		renderStreamBigRat(b, ref, posVar)
	case KindSQLNull:
		renderStreamSQLNull(b, f, ref, posVar)
	case KindAny:
		renderStreamAny(b, f, ref, posVar)
	default:
		fmt.Fprintf(b, `k, err := s.SkipValue(%[1]s)
if err != nil { return result, i, err }
%[1]s = k
`, posVar)
	}
	if !f.NoValidate {
		b.WriteString(validateAndMod(f, ref))
	}
	return b.String()
}

func renderStreamSlice(b *bytes.Buffer, f FieldInfo, ref, posVar string) {
	emitStreamSliceRead(b, f, ref, posVar, 0)
}

// emitStreamSliceRead is the streaming counterpart of emitByteSliceRead.
// Handles both slices and fixed-length arrays (KindArray → strict tuple:
// JSON array length must equal f.ArrayLen). All locals carry a depth suffix
// (kN, evN, errN, idxN) so nested decoders don't clobber each other.
func emitStreamSliceRead(b *bytes.Buffer, f FieldInfo, dst, posVar string, depth int) {
	isArray := f.Kind == KindArray
	arrayN := f.ArrayLen
	// kvar threads through the caller's position variable directly (no
	// `kN := posVar` alias), and errvar reuses the function-body `err`
	// declared by ObjectOpen — no per-emitter `var errN error`. Both
	// hold across recursion because all uses check err immediately, so
	// shared single-variable access between depths is safe.
	kvar := posVar
	errvar := "err"
	evvar := fmt.Sprintf("ev%d", depth)
	ivar := fmt.Sprintf("idx%d", depth)
	slabVar := fmt.Sprintf("slab%d", depth)
	b.WriteString("{\n")
	// err comes from the function-body scope (ObjectOpen / alias-container
	// synthetic decl). No per-depth declaration needed.
	if !isArray {
		// `null` → leave slice nil and consume the literal.
		fmt.Fprintf(b, `%[1]s, %[2]s = s.SkipSpace(%[1]s)
if %[2]s != nil { return result, i, %[2]s }
if %[1]s >= len(s.Bytes()) { if %[2]s = s.ReadMore(0); %[2]s != nil { return result, i, %[2]s } }
if s.Bytes()[%[1]s] == 'n' {
	for ki := 1; ki < 4; ki++ {
		if %[1]s+ki >= len(s.Bytes()) { if %[2]s = s.ReadMore(0); %[2]s != nil { return result, i, %[2]s } }
		if s.Bytes()[%[1]s+ki] != "null"[ki] { return result, i, scan.ErrBadLiteral }
	}
	%[1]s += 4
} else {
`, posVar, errvar)
	}
	fmt.Fprintf(b, `%[1]s, %[2]s = s.ArrayOpen(%[3]s)
if %[2]s != nil { return result, i, %[2]s }
%[1]s, %[2]s = s.SkipSpace(%[1]s)
if %[2]s != nil { return result, i, %[2]s }
if %[1]s >= len(s.Bytes()) { if %[2]s = s.ReadMore(0); %[2]s != nil { return result, i, %[2]s } }
`, kvar, errvar, posVar)
	if isArray {
		fmt.Fprintf(b, "var %s int\n", ivar)
		if f.ElemPointer {
			fmt.Fprintf(b, "%s := make([]%s, %d)\n", slabVar, f.ElemType, arrayN)
		}
	} else {
		sCap, slCap := preallocCap(f)
		// Empty `[]` → non-nil empty (stdlib parity); else fresh make()
		// with prealloc. See emitByteSliceRead for the same shape.
		if f.ElemPointer {
			fmt.Fprintf(b, "var %s []%s\n", slabVar, f.ElemType)
		}
		makeExpr := fmt.Sprintf("%s{}", f.GoType)
		if sCap > 0 {
			makeExpr = fmt.Sprintf("make(%s, 0, %d)", f.GoType, sCap)
		}
		fmt.Fprintf(b, `if s.Bytes()[%[1]s] == ']' {
	%[2]s = %[3]s{}
} else {
	%[2]s = %[4]s
`, kvar, dst, f.GoType, makeExpr)
		if f.ElemPointer {
			fmt.Fprintf(b, "%s = make([]%s, 0, %d)\n", slabVar, f.ElemType, slCap)
		}
		b.WriteString("}\n")
	}
	// See emitByteSliceRead for the rationale: generated struct elems
	// decode IN PLACE into the destination slot. The outer errvar is
	// already declared by ArrayOpen above, so we don't hoist a fresh
	// err here.
	directStruct := f.ElemKind == KindStruct && isGenerated(f.ElemType)
	fmt.Fprintf(b, "for s.Bytes()[%s] != ']' {\n", kvar)
	if isArray {
		fmt.Fprintf(b, "if %s >= %d { return result, i, %s }\n",
			ivar, arrayN,
			arrayLenErr(f.JSONName, arrayN, ivar))
	}
	if f.ElemPointer {
		// `null` element → nil pointer. Skip the parse + slab work.
		nilAssign := fmt.Sprintf("%s = append(%s, nil)\n", dst, dst)
		if isArray {
			nilAssign = fmt.Sprintf("%s[%s] = nil\n%s++\n", dst, ivar, ivar)
		}
		fmt.Fprintf(b, `if %[1]s >= len(s.Bytes()) { if %[2]s = s.ReadMore(0); %[2]s != nil { return result, i, %[2]s } }
if s.Bytes()[%[1]s] == 'n' {
	for ki := 1; ki < 4; ki++ {
		if %[1]s+ki >= len(s.Bytes()) { if %[2]s = s.ReadMore(0); %[2]s != nil { return result, i, %[2]s } }
		if s.Bytes()[%[1]s+ki] != "null"[ki] { return result, i, scan.ErrBadLiteral }
	}
	%[1]s += 4
	%[3]s	%[1]s, %[2]s = s.SkipSpace(%[1]s)
	if %[2]s != nil { return result, i, %[2]s }
	if %[1]s >= len(s.Bytes()) { if %[2]s = s.ReadMore(0); %[2]s != nil { return result, i, %[2]s } }
	if s.Bytes()[%[1]s] == ',' { %[1]s, %[2]s = s.SkipSpace(%[1]s + 1); if %[2]s != nil { return result, i, %[2]s }; continue }
	break
}
`, kvar, errvar, nilAssign)
	}
	_ = evvar // legacy depth-suffixed name no longer used at this scope
	// Compute the in-place target. For slice cases we pre-grow the
	// slice/slab here so the slot exists before scan writes into it.
	var target string
	switch {
	case isArray && f.ElemPointer:
		target = fmt.Sprintf("%s[%s]", slabVar, ivar)
	case isArray:
		target = fmt.Sprintf("%s[%s]", dst, ivar)
	case f.ElemPointer:
		fmt.Fprintf(b, "%s = append(%s, %s)\n", slabVar, slabVar, zeroLit(f.ElemType, f.ElemKind))
		target = fmt.Sprintf("%s[len(%s)-1]", slabVar, slabVar)
	default:
		fmt.Fprintf(b, "%s = append(%s, %s)\n", dst, dst, zeroLit(f.ElemType, f.ElemKind))
		target = fmt.Sprintf("%s[len(%s)-1]", dst, dst)
	}
	switch f.ElemKind {
	case KindString:
		// Direct LHS assign — target, kvar, errvar all already declared.
		fmt.Fprintf(b, `%[1]s, %[2]s, %[3]s = s.String(%[2]s)
if %[3]s != nil { return result, i, %[3]s }
`, target, kvar, errvar)
	case KindBool:
		fmt.Fprintf(b, `%[1]s, %[2]s, %[3]s = s.Bool(%[2]s)
if %[3]s != nil { return result, i, %[3]s }
`, target, kvar, errvar)
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		if f.ElemType == "int64" {
			fmt.Fprintf(b, `%[1]s, %[2]s, %[3]s = s.Int64(%[2]s)
if %[3]s != nil { return result, i, %[3]s }
`, target, kvar, errvar)
		} else {
			fmt.Fprintf(b, `var iv int64
iv, %[2]s, %[3]s = s.Int64(%[2]s)
if %[3]s != nil { return result, i, %[3]s }
%[1]s = %[4]s(iv)
`, target, kvar, errvar, f.ElemType)
		}
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		if f.ElemType == "uint64" {
			fmt.Fprintf(b, `%[1]s, %[2]s, %[3]s = s.Uint64(%[2]s)
if %[3]s != nil { return result, i, %[3]s }
`, target, kvar, errvar)
		} else {
			fmt.Fprintf(b, `var uv uint64
uv, %[2]s, %[3]s = s.Uint64(%[2]s)
if %[3]s != nil { return result, i, %[3]s }
%[1]s = %[4]s(uv)
`, target, kvar, errvar, f.ElemType)
		}
	case KindFloat32, KindFloat64:
		if f.ElemKind == KindFloat64 {
			fmt.Fprintf(b, `%[1]s, %[2]s, %[3]s = s.Float64(%[2]s)
if %[3]s != nil { return result, i, %[3]s }
`, target, kvar, errvar)
		} else {
			fmt.Fprintf(b, `var fv float64
fv, %[2]s, %[3]s = s.Float64(%[2]s)
if %[3]s != nil { return result, i, %[3]s }
%[1]s = float32(fv)
`, target, kvar, errvar)
		}
	case KindStruct:
		if directStruct {
			fmt.Fprintf(b, `%[1]s, %[2]s, %[3]s = %[1]s.DecodeStreamFrom(s, %[2]s)
if %[3]s != nil { return result, i, %[3]s }
`, target, kvar, errvar)
		}
	case KindSlice, KindArray:
		emitStreamSliceRead(b, peelSliceField(f), target, kvar, depth+1)
	}
	if len(f.ElemMods) > 0 {
		b.WriteString(renderMods(f.ElemMods, target, f.ElemType, f.ElemKind))
	}
	if len(f.ElemValidation) > 0 {
		code := renderValidationOn(f.ElemValidation, target, f.JSONName+"[]", f.ElemKind, f.MultiErr)
		if !f.MultiErr {
			code = strings.ReplaceAll(code, "return result, ", "return result, i, ")
		}
		b.WriteString(code)
	}
	switch {
	case isArray && f.ElemPointer:
		// Slab slot already decoded in-place; publish its address.
		fmt.Fprintf(b, "%[1]s[%[2]s] = &%[3]s[%[2]s]\n%[2]s++\n", dst, ivar, slabVar)
	case isArray:
		// dst[ivar] already decoded in-place.
		fmt.Fprintf(b, "%s++\n", ivar)
	case f.ElemPointer:
		// Slab tail decoded in-place via append+index above; publish addr.
		fmt.Fprintf(b, "%[1]s = append(%[1]s, &%[2]s[len(%[2]s)-1])\n", dst, slabVar)
	default:
		// dst tail already decoded in-place via append+index above.
	}
	fmt.Fprintf(b, `%[1]s, %[2]s = s.SkipSpace(%[1]s)
if %[2]s != nil { return result, i, %[2]s }
if %[1]s >= len(s.Bytes()) { if %[2]s = s.ReadMore(0); %[2]s != nil { return result, i, %[2]s } }
if s.Bytes()[%[1]s] == ',' { %[1]s, %[2]s = s.SkipSpace(%[1]s + 1); if %[2]s != nil { return result, i, %[2]s }; continue }
break
}
if s.Bytes()[%[1]s] != ']' { return result, i, scan.ErrBadArray }
`, kvar, errvar)
	if isArray {
		fmt.Fprintf(b, "if %s != %d { return result, i, %s }\n",
			ivar, arrayN,
			arrayLenErr(f.JSONName, arrayN, ivar))
	}
	// kvar == posVar (no alias); just step past the closing `]`.
	fmt.Fprintf(b, "%s++\n", posVar)
	if !isArray {
		b.WriteString("}\n") // close else (null-check)
	}
	b.WriteString("}\n") // close outer block
}
