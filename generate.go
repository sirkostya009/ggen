package main

import (
	"bytes"
	"fmt"
	"go/format"
	"slices"
	"strconv"
	"strings"
	"text/template"
)

func generate(pkg string, structs []StructInfo) ([]byte, error) {
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
	data := buildTemplateData(pkg, structs)

	tmpl := template.Must(template.New("gen").Funcs(template.FuncMap{
		"tokenKind":       tokenKind,
		"tokenExtract":    tokenExtract,
		"kindName":        kindName,
		"validate":        renderValidation,
		"elemValidate":    renderElemValidation,
		"appendJSON":      renderAppendJSON,
		"sizeJSON":        renderSize,
		"hasElemRules":    hasElemRules,
		"readField":       renderReadField,
		"readBytes":       renderReadBytes,
		"readTime":        renderReadTime,
		"readDuration":    renderReadDuration,
		"readNetIP":       renderReadNetIP,
		"readNetipAddr":   renderReadNetipAddr,
		"readNetipPrefix": renderReadNetipPrefix,
		"decode":          renderDecode,
		"streamDecode":    renderStreamDecode,
	}).Parse(genTemplate))

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("executing template: %w", err)
	}
	src, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("formatting generated code: %w\n\nraw output:\n%s", err, buf.String())
	}
	return src, nil
}

type templateData struct {
	Package    string
	BuildTag   string // canonical //go:build expression, "" when unconstrained
	Imports    []string
	Structs    []StructInfo
	OneOfDecls []string
}

// preregisterOneOfs walks all rules (top-level, dive-elem, key, inner-nested)
// to populate the OneOf frozen-slice registry before template execution.
// Order-independent: registerOneOf dedupes by joined value, so re-walking
// during render returns the same name.
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

func buildTemplateData(pkg string, structs []StructInfo) templateData {
	preregisterOneOfs(structs)
	// All structs in a single generation pass share the same build tag —
	// the caller (generateDir) buckets them by BuildTag before invoking
	// generate(). Empty for unconstrained, non-empty for files behind a
	// //go:build directive.
	var tag string
	if len(structs) > 0 {
		tag = structs[0].BuildTag
	}
	d := templateData{Package: pkg, BuildTag: tag, Structs: structs, OneOfDecls: oneofRegistry.decls}
	imports := []string{
		"github.com/sirkostya009/ggen/encode",
		"github.com/sirkostya009/ggen/scan",
	}
	if usesUnsafe(structs) {
		imports = append(imports, "unsafe")
	}
	if usesDecodePackage(structs) {
		imports = append(imports, "github.com/sirkostya009/ggen/decode")
	}
	if usesFmt(structs) {
		imports = append(imports, "fmt")
	}
	if usesRuneValidators(structs) {
		imports = append(imports, "unicode/utf8")
	}
	if usesStrconv(structs) {
		imports = append(imports, "strconv")
	}
	if usesValidators(structs) {
		imports = append(imports, "github.com/sirkostya009/ggen/decode/validation")
	}
	if usesStringsPackage(structs) {
		imports = append(imports, "strings")
	}
	if usesJSONFallback(structs) {
		imports = append(imports, "encoding/json")
	}
	imports = append(imports, customTypeImports(structs)...)
	imports = append(imports, customFuncImports(structs)...)
	imports = append(imports, aliasUnderlyingImports(structs)...)
	slices.Sort(imports)
	d.Imports = imports
	return d
}

// usesDecodePackage reports whether generated code references the
// decode package. Triggers: opted-in UnmarshalJSON hook (calls
// decode.Unmarshal) and predicate-style string validators
// (decode.IsEmail, decode.IsURL, ...). Custom `@Func` rules emit a
// direct call into the user's package — no decode pkg involvement.
func usesDecodePackage(structs []StructInfo) bool {
	for _, s := range structs {
		if s.Unmarshal {
			return true
		}
		for _, f := range s.Fields {
			if rulesUseDecode(f.Validation) ||
				rulesUseDecode(f.ElemValidation) ||
				rulesUseDecode(f.KeyValidation) {
				return true
			}
			for _, inner := range f.InnerValidation {
				if rulesUseDecode(inner) {
					return true
				}
			}
		}
	}
	return false
}

// rulesUseDecode reports whether any rule in the list emits a decode.X
// reference (predicate funcs only — custom `@Func` rules call directly
// into user code, never via the decode package).
func rulesUseDecode(rules []ValidationRule) bool {
	for _, v := range rules {
		switch v.Name {
		case "email", "url", "ascii", "printable", "alphanum",
			"numeric", "lower", "upper", "hexadecimal":
			return true
		}
	}
	return false
}

func usesRuneValidators(structs []StructInfo) bool {
	for _, s := range structs {
		for _, f := range s.Fields {
			if rulesUseRunes(f.Validation) || rulesUseRunes(f.ElemValidation) {
				return true
			}
		}
	}
	return false
}

// usesJSONFallback reports whether any field will actually emit a
// `json.Marshal` / `json.Unmarshal` call in the generated code. A
// cross-package struct field that satisfies one of the fast-path
// interfaces (DecodeFrom, AppendJSON, MarshalJSON / UnmarshalJSON, the
// text-marshaler family) routes through the matching method instead —
// none of those reference `encoding/json` from the gen file. Only the
// resolved-but-unmatched and unresolved (AST-only) branches actually
// reach for stdlib json.
func usesJSONFallback(structs []StructInfo) bool {
	for _, s := range structs {
		for _, f := range s.Fields {
			if f.Kind == KindStruct {
				t := f.GoType
				if f.Pointer && f.PointeeType != "" {
					t = f.PointeeType
				}
				if !isGenerated(t) && fieldUsesJSONFallback(f) {
					return true
				}
			}
			if f.Kind == KindSlice && f.ElemKind == KindStruct && !isGenerated(f.ElemType) && fieldUsesJSONFallback(f) {
				return true
			}
			if f.Inline && (f.ElemType == "any" || f.ElemType == "interface{}") {
				return true
			}
			// Note: bare `any` fields used to import encoding/json, but encode
			// now goes through encode.AppendAny and decode through scan.Any —
			// neither references the json package from generated code.
		}
	}
	return false
}

// fieldUsesJSONFallback mirrors the dispatch ladder in
// renderCrossPkgStructDecode / renderCrossPkgStructAppend: a field needs
// the encoding/json import only when at least one direction (decode or
// encode) ends up in the default branch that actually calls
// `json.Marshal` / `json.Unmarshal`. Unresolved (AST-only) types always
// fall through to that branch.
func fieldUsesJSONFallback(f FieldInfo) bool {
	if !f.Iface.Resolved {
		return true
	}
	decodeViaJSON := !f.Iface.ByteDecoder && !f.Iface.JSONUnmarshaler && !f.Iface.TextUnmarshaler
	encodeViaJSON := !f.Iface.AppendJSON && !f.Iface.JSONMarshaler && !f.Iface.TextAppender && !f.Iface.TextMarshaler
	return decodeViaJSON || encodeViaJSON
}

// usesValidators reports whether any generated struct needs the validation package
// imported (any validation rule, required field, or the default duplicate-key
// guard all emit *validation.Error).
func usesValidators(structs []StructInfo) bool {
	for _, s := range structs {
		if s.IsAlias {
			// Alias codegen has no JSON-object dispatch, so the dup-key
			// and unknown-key guards aren't emitted; v1 also has no place
			// to attach validation rules to an alias. Validation pkg
			// stays out of an alias-only pass.
			continue
		}
		if !s.AllowDups {
			return true // duplicate-key guard emits validation.Error
		}
		if !s.IgnoreUnknown && !s.HasInline() {
			return true // default unknown-key path emits validation.Error
		}
		for _, f := range s.Fields {
			if f.IsRequired() || len(f.Validation) > 0 || len(f.ElemValidation) > 0 {
				return true
			}
		}
	}
	return false
}

// customFuncImports collects the import paths needed for cross-package
// `@pkg.Func` references discovered during parse. Returned paths get
// merged into the generated file's import block; codegen qualifies the
// call with the imported package's canonical name.
func customFuncImports(structs []StructInfo) []string {
	need := map[string]struct{}{}
	walkV := func(rules []ValidationRule) {
		for _, r := range rules {
			if r.Custom && r.PkgImport != "" {
				need[r.PkgImport] = struct{}{}
			}
		}
	}
	walkM := func(rules []ModRule) {
		for _, r := range rules {
			if r.Custom && r.PkgImport != "" {
				need[r.PkgImport] = struct{}{}
			}
		}
	}
	for _, s := range structs {
		for _, f := range s.Fields {
			walkV(f.Validation)
			walkV(f.ElemValidation)
			walkV(f.KeyValidation)
			for _, inner := range f.InnerValidation {
				walkV(inner)
			}
			walkM(f.Mods)
			walkM(f.ElemMods)
			walkM(f.KeyMods)
			for _, inner := range f.InnerMods {
				walkM(inner)
			}
		}
	}
	out := make([]string, 0, len(need))
	for p := range need {
		out = append(out, p)
	}
	return out
}

// customTypeImports returns the extra stdlib imports required by any field
// whose kind is one of the native types we recognize (time.Time, net.IP, etc.).
func customTypeImports(structs []StructInfo) []string {
	need := map[string]struct{}{}
	for _, s := range structs {
		for _, f := range s.Fields {
			switch f.Kind {
			case KindTime, KindDuration:
				need["time"] = struct{}{}
			case KindNetIP:
				need["net"] = struct{}{}
			case KindNetipAddr, KindNetipPrefix:
				need["net/netip"] = struct{}{}
			case KindBytes:
				switch f.Format {
				case "", "base64", "base64url":
					need["encoding/base64"] = struct{}{}
				case "base32", "base32hex":
					need["encoding/base32"] = struct{}{}
				case "base16", "hex":
					need["encoding/hex"] = struct{}{}
				}
			case KindURL:
				need["net/url"] = struct{}{}
			// KindBigInt/Float/Rat: generated code calls methods on the field
			// (SetString / Append / Parse) without ever naming the `big.*`
			// type — no math/big import required.
			case KindSQLNull:
				// sql.NullX literal initializers (`sql.NullString{...}`) reference
				// the package by name in generated code, so the import is required.
				need["database/sql"] = struct{}{}
				if spec, ok := SQLNullSpec(f.GoType); ok && spec.Inner == KindTime {
					need["time"] = struct{}{}
				}
			}
		}
	}
	out := make([]string, 0, len(need))
	for k := range need {
		out = append(out, k)
	}
	return out
}

// usesFmt reports whether the generated file references fmt. The
// scanner uses sentinel errors from the scan package, so fmt is only
// pulled in by net.IP error paths (net.ParseIP returns nil rather than
// an error, so we wrap with fmt.Errorf).
func usesFmt(structs []StructInfo) bool {
	for _, s := range structs {
		for _, f := range s.Fields {
			if f.Kind == KindNetIP {
				return true
			}
		}
	}
	return false
}

func usesStringsPackage(structs []StructInfo) bool {
	for _, s := range structs {
		for _, f := range s.Fields {
			if rulesUseStrings(f.Validation) || rulesUseStrings(f.ElemValidation) {
				return true
			}
			if len(f.Mods) > 0 || len(f.ElemMods) > 0 {
				return true // all mods call strings.*
			}
		}
	}
	return false
}

func rulesUseStrings(rules []ValidationRule) bool {
	for _, v := range rules {
		switch v.Name {
		case "starts", "ends", "contains":
			return true
		}
	}
	return false
}

func rulesUseRunes(rules []ValidationRule) bool {
	for _, v := range rules {
		switch v.Name {
		case "runes", "minrunes", "maxrunes":
			return true
		}
	}
	return false
}

func tokenKind(k TypeKind) string {
	switch k {
	case KindString:
		return `'"'`
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64,
		KindUint, KindUint8, KindUint16, KindUint32, KindUint64,
		KindFloat32, KindFloat64:
		return `'0'`
	case KindStruct:
		return `'{'`
	case KindSlice:
		return `'['`
	default:
		return `'"'`
	}
}

func tokenExtract(k TypeKind, tokVar string) string {
	switch k {
	case KindString:
		return tokVar + ".String()"
	case KindInt:
		return "int(" + tokVar + ".Int())"
	case KindInt8:
		return "int8(" + tokVar + ".Int())"
	case KindInt16:
		return "int16(" + tokVar + ".Int())"
	case KindInt32:
		return "int32(" + tokVar + ".Int())"
	case KindInt64:
		return tokVar + ".Int()"
	case KindUint:
		return "uint(" + tokVar + ".Uint())"
	case KindUint8:
		return "uint8(" + tokVar + ".Uint())"
	case KindUint16:
		return "uint16(" + tokVar + ".Uint())"
	case KindUint32:
		return "uint32(" + tokVar + ".Uint())"
	case KindUint64:
		return tokVar + ".Uint()"
	case KindFloat32:
		return "float32(" + tokVar + ".Float())"
	case KindFloat64:
		return tokVar + ".Float()"
	case KindBool:
		return tokVar + ".Bool()"
	default:
		return tokVar + ".String()"
	}
}

func kindName(k TypeKind) string {
	switch k {
	case KindString:
		return "string"
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64,
		KindUint, KindUint8, KindUint16, KindUint32, KindUint64,
		KindFloat32, KindFloat64:
		return "number"
	case KindBool:
		return "bool"
	case KindStruct:
		return "object"
	case KindSlice:
		return "array"
	default:
		return "unknown"
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

// renderValidation emits Go source for all validation checks on a field,
// excluding "required" (which is tracked separately via the seen<Field> flag).
func renderValidation(f FieldInfo) string {
	return renderValidationOn(f.Validation, "result."+f.GoName, f.JSONName, f.Kind, f.MultiErr)
}

// renderElemValidation emits validation for a slice element (loop variable
// `elem`). Called from the slice-read template when ElemValidation is set.
func renderElemValidation(f FieldInfo) string {
	return renderValidationOn(f.ElemValidation, "elem", f.JSONName+"[]", f.ElemKind, f.MultiErr)
}

// hasElemRules reports whether a field has per-element validation rules.
func hasElemRules(f FieldInfo) bool { return len(f.ElemValidation) > 0 }

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
	var b strings.Builder
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
				fmt.Fprintf(&b, "if _v, _err := %s(%s); _err != nil {\n\treturn result, 0, _err\n} else {\n\t%s = _v\n}\n",
					call, ref, ref)
			} else {
				fmt.Fprintf(&b, "%s = %s(%s)\n", ref, call, ref)
			}
			continue
		}
		switch m.Name {
		case "trim":
			fmt.Fprintf(&b, "%s = %s\n", ref, wrap(fmt.Sprintf("strings.TrimSpace(%s)", asPrim(ref))))
		case "lower":
			fmt.Fprintf(&b, "%s = %s\n", ref, wrap(fmt.Sprintf("strings.ToLower(%s)", asPrim(ref))))
		case "upper":
			fmt.Fprintf(&b, "%s = %s\n", ref, wrap(fmt.Sprintf("strings.ToUpper(%s)", asPrim(ref))))
		case "trimleft":
			fmt.Fprintf(&b, "%s = %s\n", ref, wrap(fmt.Sprintf("strings.TrimPrefix(%s, %s)", asPrim(ref), strconv.Quote(m.Value))))
		case "trimright":
			fmt.Fprintf(&b, "%s = %s\n", ref, wrap(fmt.Sprintf("strings.TrimSuffix(%s, %s)", asPrim(ref), strconv.Quote(m.Value))))
		case "replace":
			parts := strings.SplitN(m.Value, "|", 2)
			if len(parts) == 2 {
				fmt.Fprintf(&b, "%s = %s\n", ref,
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
				fmt.Fprintf(&b, "if %s < %s { %s = %s }\n", ref, wrap(lo), ref, wrap(lo))
			}
			if hi != "" {
				fmt.Fprintf(&b, "if %s > %s { %s = %s }\n", ref, wrap(hi), ref, wrap(hi))
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

// duplicateKeyErr builds a typed *validation.DuplicateKeyError literal.
func duplicateKeyErr(field string) string {
	return fmt.Sprintf("&validation.DuplicateKeyError{Field: %q}", field)
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
	name := fmt.Sprintf("_oneof_%d", len(oneofRegistry.names))
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
	var b strings.Builder

	// onErr wraps a `&validation.XError{...}` literal into the appropriate
	// failure action: append in multierr mode, early return otherwise.
	onErr := func(errExpr string) string {
		if multiErr {
			return "errs = append(errs, " + errExpr + ")"
		}
		return "return result, " + errExpr
	}

	// q quotes a string literal.
	q := func(s string) string { return strconv.Quote(s) }

	// vstr is the source expression for the offending Value field of a
	// pattern/length error. For string-typed refs it's just `ref`. For
	// numeric refs that need to be cast to string for the error (none of
	// today's patterns), this would adapt — currently a no-op pass-through.

	for _, v := range rules {
		switch v.Name {
		case "required", "optional":
			// required handled separately; optional is a no-op marker

		case "notempty":
			fmt.Fprintf(&b, "if len(%s) == 0 {\n\t%s\n}\n", ref,
				onErr(fmt.Sprintf("&validation.NotEmptyError{Field: %q}", jsonName)))

		case "len":
			fmt.Fprintf(&b, "if len(%s) != %s {\n\t%s\n}\n", ref, v.Value,
				onErr(fmt.Sprintf("&validation.LenError{Field: %q, Want: %s, Got: len(%s)}", jsonName, v.Value, ref)))
		case "minlen":
			fmt.Fprintf(&b, "if len(%s) < %s {\n\t%s\n}\n", ref, v.Value,
				onErr(fmt.Sprintf("&validation.MinLenError{Field: %q, Limit: %s, Got: len(%s)}", jsonName, v.Value, ref)))
		case "maxlen":
			fmt.Fprintf(&b, "if len(%s) > %s {\n\t%s\n}\n", ref, v.Value,
				onErr(fmt.Sprintf("&validation.MaxLenError{Field: %q, Limit: %s, Got: len(%s)}", jsonName, v.Value, ref)))

		case "runes":
			fmt.Fprintf(&b, "if utf8.RuneCountInString(%s) != %s {\n\t%s\n}\n", ref, v.Value,
				onErr(fmt.Sprintf("&validation.RunesError{Field: %q, Want: %s, Got: utf8.RuneCountInString(%s)}", jsonName, v.Value, ref)))
		case "minrunes":
			fmt.Fprintf(&b, "if utf8.RuneCountInString(%s) < %s {\n\t%s\n}\n", ref, v.Value,
				onErr(fmt.Sprintf("&validation.MinRunesError{Field: %q, Limit: %s, Got: utf8.RuneCountInString(%s)}", jsonName, v.Value, ref)))
		case "maxrunes":
			fmt.Fprintf(&b, "if utf8.RuneCountInString(%s) > %s {\n\t%s\n}\n", ref, v.Value,
				onErr(fmt.Sprintf("&validation.MaxRunesError{Field: %q, Limit: %s, Got: utf8.RuneCountInString(%s)}", jsonName, v.Value, ref)))

		case "gt":
			fmt.Fprintf(&b, "if %s <= %s {\n\t%s\n}\n", ref, v.Value,
				onErr(fmt.Sprintf("&validation.GTError{Field: %q, Limit: %s, Value: %s}", jsonName, v.Value, ref)))
		case "gte":
			fmt.Fprintf(&b, "if %s < %s {\n\t%s\n}\n", ref, v.Value,
				onErr(fmt.Sprintf("&validation.GTEError{Field: %q, Limit: %s, Value: %s}", jsonName, v.Value, ref)))
		case "lt":
			fmt.Fprintf(&b, "if %s >= %s {\n\t%s\n}\n", ref, v.Value,
				onErr(fmt.Sprintf("&validation.LTError{Field: %q, Limit: %s, Value: %s}", jsonName, v.Value, ref)))
		case "lte":
			fmt.Fprintf(&b, "if %s > %s {\n\t%s\n}\n", ref, v.Value,
				onErr(fmt.Sprintf("&validation.LTEError{Field: %q, Limit: %s, Value: %s}", jsonName, v.Value, ref)))

		case "eq":
			if kind == KindString {
				fmt.Fprintf(&b, "if %s != %s {\n\t%s\n}\n",
					ref, q(v.Value),
					onErr(fmt.Sprintf("&validation.EqError{Field: %q, Want: %s, Value: %s}", jsonName, q(v.Value), ref)))
			} else if isNumeric(kind) {
				fmt.Fprintf(&b, "if %s != %s {\n\t%s\n}\n",
					ref, v.Value,
					onErr(fmt.Sprintf("&validation.EqError{Field: %q, Want: %s, Value: %s}", jsonName, v.Value, ref)))
			}
		case "neq":
			if kind == KindString {
				fmt.Fprintf(&b, "if %s == %s {\n\t%s\n}\n",
					ref, q(v.Value),
					onErr(fmt.Sprintf("&validation.NeqError{Field: %q, Want: %s, Value: %s}", jsonName, q(v.Value), ref)))
			} else if isNumeric(kind) {
				fmt.Fprintf(&b, "if %s == %s {\n\t%s\n}\n",
					ref, v.Value,
					onErr(fmt.Sprintf("&validation.NeqError{Field: %q, Want: %s, Value: %s}", jsonName, v.Value, ref)))
			}

		case "oneof":
			cases := renderOneofCases(kind, v.Value)
			parts := strings.Split(v.Value, "|")
			varName := registerOneOf(parts)
			fmt.Fprintf(&b, "switch %s {\ncase %s:\ndefault:\n\t%s\n}\n",
				ref, cases,
				onErr(fmt.Sprintf("&validation.OneOfError{Field: %q, Allowed: %s, Value: %s}", jsonName, varName, ref)))

		case "email":
			fmt.Fprintf(&b, "if !decode.IsEmail(%s) {\n\t%s\n}\n", ref,
				onErr(fmt.Sprintf("&validation.EmailError{Field: %q, Value: %s}", jsonName, ref)))
		case "url":
			fmt.Fprintf(&b, "if !decode.IsURL(%s) {\n\t%s\n}\n", ref,
				onErr(fmt.Sprintf("&validation.URLError{Field: %q, Value: %s}", jsonName, ref)))

		case "ascii":
			fmt.Fprintf(&b, "if !decode.IsASCII(%s) {\n\t%s\n}\n", ref,
				onErr(fmt.Sprintf("&validation.ASCIIError{Field: %q, Value: %s}", jsonName, ref)))
		case "printable":
			fmt.Fprintf(&b, "if !decode.IsPrintable(%s) {\n\t%s\n}\n", ref,
				onErr(fmt.Sprintf("&validation.PrintableError{Field: %q, Value: %s}", jsonName, ref)))
		case "alphanum":
			fmt.Fprintf(&b, "if !decode.IsAlphanum(%s) {\n\t%s\n}\n", ref,
				onErr(fmt.Sprintf("&validation.AlphanumError{Field: %q, Value: %s}", jsonName, ref)))
		case "numeric":
			fmt.Fprintf(&b, "if !decode.IsNumeric(%s) {\n\t%s\n}\n", ref,
				onErr(fmt.Sprintf("&validation.NumericError{Field: %q, Value: %s}", jsonName, ref)))
		case "lower":
			fmt.Fprintf(&b, "if !decode.IsLower(%s) {\n\t%s\n}\n", ref,
				onErr(fmt.Sprintf("&validation.LowerError{Field: %q, Value: %s}", jsonName, ref)))
		case "upper":
			fmt.Fprintf(&b, "if !decode.IsUpper(%s) {\n\t%s\n}\n", ref,
				onErr(fmt.Sprintf("&validation.UpperError{Field: %q, Value: %s}", jsonName, ref)))
		case "hexadecimal":
			fmt.Fprintf(&b, "if !decode.IsHex(%s) {\n\t%s\n}\n", ref,
				onErr(fmt.Sprintf("&validation.HexadecimalError{Field: %q, Value: %s}", jsonName, ref)))

		case "starts":
			fmt.Fprintf(&b, "if !strings.HasPrefix(%s, %s) {\n\t%s\n}\n",
				ref, q(v.Value),
				onErr(fmt.Sprintf("&validation.StartsError{Field: %q, Want: %s, Value: %s}", jsonName, q(v.Value), ref)))
		case "ends":
			fmt.Fprintf(&b, "if !strings.HasSuffix(%s, %s) {\n\t%s\n}\n",
				ref, q(v.Value),
				onErr(fmt.Sprintf("&validation.EndsError{Field: %q, Want: %s, Value: %s}", jsonName, q(v.Value), ref)))
		case "contains":
			fmt.Fprintf(&b, "if !strings.Contains(%s, %s) {\n\t%s\n}\n",
				ref, q(v.Value),
				onErr(fmt.Sprintf("&validation.ContainsError{Field: %q, Want: %s, Value: %s}", jsonName, q(v.Value), ref)))

		case "multiple":
			fmt.Fprintf(&b, "if %s %% %s != 0 {\n\t%s\n}\n", ref, v.Value,
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
				fmt.Fprintf(&b, "if err := %s(%s); err != nil {\n\t%s\n}\n",
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
	return strings.Join(uniqueStrings(emitConds), " && ")
}

func uniqueStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	var out []string
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func renderAppendJSON(s StructInfo) string {
	if s.IsAlias {
		return renderAliasAppendJSON(s)
	}
	body := renderAppendJSONBody(s)
	return coalesceConstAppends(body)
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
	if strings.HasSuffix(rest, "...") {
		s := strings.TrimSpace(strings.TrimSuffix(rest, "..."))
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

func renderAppendJSONBody(s StructInfo) string {
	var b strings.Builder
	if len(s.Fields) == 0 {
		b.WriteString("return append(dst, '{', '}'), nil")
		return b.String()
	}

	// `var err error` is declared up front so emitters can do
	// `dst, err = X.AppendJSON(dst); if err != nil { return dst, err }`
	// without having to redeclare per call site. Marked with `_ = err` if
	// no fallible call ends up using it (e.g. struct of pure primitives).
	b.WriteString("var err error\n_ = err\n")

	// Detect if any field is conditional; if none, use the path that
	// hard-codes `{` into the first field's prefix and commas before the rest.
	anyConditional := false
	for _, f := range s.Fields {
		if fieldIsConditional(f) {
			anyConditional = true
			break
		}
	}

	if !anyConditional {
		for i, f := range s.Fields {
			prefix := `,"` + f.JSONName + `":`
			if i == 0 {
				prefix = `{"` + f.JSONName + `":`
			}
			ref := "s." + f.GoName
			if newPrefix, code, ok := foldLeadingQuote(f, ref, prefix); ok {
				fmt.Fprintf(&b, "dst = append(dst, %q...)\n", newPrefix)
				b.WriteString(code)
			} else {
				fmt.Fprintf(&b, "dst = append(dst, %q...)\n", prefix)
				b.WriteString(renderAppendValue(f, ref))
			}
		}
		b.WriteString("return append(dst, '}'), nil")
		return b.String()
	}

	// Conditional path: track emissions via len(dst) vs start.
	b.WriteString("dst = append(dst, '{')\n")
	b.WriteString("start := len(dst)\n")
	for _, f := range s.Fields {
		ref := "s." + f.GoName
		if f.Inline {
			b.WriteString(renderAppendInline(f, ref))
			continue
		}
		emit := fieldSkipExpr(f, ref)
		if emit != "" {
			fmt.Fprintf(&b, "if %s {\n", emit)
		}
		b.WriteString("if len(dst) > start { dst = append(dst, ',') }\n")
		prefix := `"` + f.JSONName + `":`
		if newPrefix, code, ok := foldLeadingQuote(f, ref, prefix); ok {
			fmt.Fprintf(&b, "dst = append(dst, %q...)\n", newPrefix)
			b.WriteString(code)
		} else {
			fmt.Fprintf(&b, "dst = append(dst, %q...)\n", prefix)
			b.WriteString(renderAppendValue(f, ref))
		}
		if emit != "" {
			b.WriteString("}\n")
		}
	}
	b.WriteString("return append(dst, '}'), nil")
	return b.String()
}

// renderAppendInline emits marshal code for a catch-all inline map: iterate
// entries, emit each as a top-level JSON key/value with comma separators based
// on whether anything has been emitted yet (tracked via len(dst) > start).
func renderAppendInline(f FieldInfo, ref string) string {
	var b strings.Builder
	b.WriteString("{\n")
	fmt.Fprintf(&b, "for _k, _v := range %s {\n", ref)
	b.WriteString("if len(dst) > start { dst = append(dst, ',') }\n")
	b.WriteString("dst = append(dst, '\"')\n")
	fmt.Fprintf(&b, "dst = %s(dst, _k)\n", appendStrFn(f.HTMLEscape))
	b.WriteString("dst = append(dst, ':')\n")
	switch f.ElemType {
	case "jsontext.Value":
		b.WriteString("dst = append(dst, _v...)\n")
	default: // any / interface{}
		b.WriteString("if dst, err = encode.AppendAny(dst, _v); err != nil { return dst, err }\n")
	}
	b.WriteString("}\n}\n")
	return b.String()
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
		return renderAppendSlice(f, ref)
	case KindMap:
		return renderAppendMap(f, ref)
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
		return fmt.Sprintf("dst = append(dst, '\"')\ndst =%s(dst, %s.String())\n", appendStrFn(f.HTMLEscape), ref)
	case KindBigInt:
		// big.Int.Append takes (buf, base) and appends in place — no alloc.
		return fmt.Sprintf("dst = (&%s).Append(dst, 10)\n", ref)
	case KindBigFloat:
		// big.Float as JSON string — matches jsonv2's expected wire format.
		// big.Float.Append: (buf, format byte, prec int).
		return fmt.Sprintf("dst = append(dst, '\"')\ndst = (&%s).Append(dst, 'g', -1)\ndst = append(dst, '\"')\n", ref)
	case KindBigRat:
		// Rat is JSON-stringified ("num/denom"). RatString avoids the slash
		// when the value is a whole number.
		return fmt.Sprintf("dst = append(dst, '\"')\ndst =%s(dst, (&%s).RatString())\n", appendStrFn(f.HTMLEscape), ref)
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
for _i, _b := range %s {
	if _i > 0 { dst = append(dst, ',') }
	dst = strconv.AppendUint(dst, uint64(_b), 10)
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
		return fmt.Sprintf("dst = strconv.AppendInt(dst, %s.%s(), 10)\n", ref, numeric)
	}
	return fmt.Sprintf("dst = append(dst, '\"')\ndst = %s.AppendFormat(dst, %s)\ndst = append(dst, '\"')\n", ref, layout)
}

// renderReadField emits the full unmarshal code block for a single field.
// It handles:
//   - duplicate-key guard (unless AllowDups is set)
//   - pointer wrapping (null → nil, non-null → allocate + read pointee)
//   - json:",string" wrapping
//   - dispatch by Kind to the appropriate read primitive
//
// The emitted code reads from dec and assigns to result.<GoName>.
func renderReadField(f FieldInfo) string {
	var prefix string
	if !f.AllowDups {
		errExpr := duplicateKeyErr(f.JSONName)
		var guard string
		if f.MultiErr {
			guard = "errs = append(errs, " + errExpr + ")"
		} else {
			guard = "return result, " + errExpr
		}
		prefix = fmt.Sprintf("if seen%s { %s }\nseen%s = true\n", f.GoName, guard, f.GoName)
	} else if f.IsRequired() {
		// Required tracking still needed.
		prefix = fmt.Sprintf("seen%s = true\n", f.GoName)
	}
	var body string
	if f.Pointer {
		body = renderReadPointer(f)
	} else {
		body = renderReadNonPointer(f)
	}
	// Mods run after decode, before validation (so validation sees the
	// transformed value). Only applies to non-pointer fields; for pointers the
	// *deref would be needed and is uncommon — skip for now.
	if !f.Pointer && len(f.Mods) > 0 {
		body += renderMods(f.Mods, "result."+f.GoName, f.GoType, f.Kind)
	}
	return prefix + body
}

func renderReadPointer(f FieldInfo) string {
	ref := "result." + f.GoName
	// Strip Pointer for the inner render, and use a scratch variable `v`.
	inner := f
	inner.Pointer = false
	if inner.PointeeType != "" {
		inner.GoType = inner.PointeeType
	}
	innerRef := "v"
	innerCode := renderReadBlockInto(inner, innerRef)
	return fmt.Sprintf(`if dec.PeekKind() == 'n' {
	if _, err := dec.ReadToken(); err != nil {
		return result, fmt.Errorf(%q, err)
	}
	%s = nil
} else {
	var v %s
	%s
	%s = &v
}
`,
		"reading "+`"`+f.JSONName+`"`+": %w",
		ref,
		inner.GoType,
		innerCode,
		ref)
}

// renderReadNonPointer emits read-and-assign for a non-pointer field,
// writing into result.<GoName>.
func renderReadNonPointer(f FieldInfo) string {
	return renderReadBlockInto(f, "result."+f.GoName)
}

// renderReadBlockInto emits read code that assigns into ref (which must be
// addressable-by-assignment, e.g., "result.Foo" or "v").
func renderReadBlockInto(f FieldInfo, ref string) string {
	switch {
	case f.String:
		return renderReadStringTag(f, ref)
	}
	switch f.Kind {
	case KindString:
		return renderReadPrimString(f, ref)
	case KindBool:
		return renderReadPrimBool(f, ref)
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64,
		KindUint, KindUint8, KindUint16, KindUint32, KindUint64,
		KindFloat32, KindFloat64:
		return renderReadPrimNumber(f, ref)
	case KindStruct:
		return renderReadStruct(f, ref)
	case KindSlice:
		return renderReadSlice(f, ref)
	case KindMap:
		return renderReadMap(f, ref)
	case KindBytes:
		return renderReadBytesInto(f, ref)
	case KindTime:
		return renderReadTimeInto(f, ref)
	case KindDuration:
		return renderReadDurationInto(f, ref)
	case KindNetIP:
		return renderReadNetIPInto(f, ref)
	case KindNetipAddr:
		return renderReadNetipAddrInto(f, ref)
	case KindNetipPrefix:
		return renderReadNetipPrefixInto(f, ref)
	}
	return ""
}

func renderReadPrimString(f FieldInfo, ref string) string {
	return fmt.Sprintf(`valTok, err := dec.ReadToken()
if err != nil { return result, fmt.Errorf(%q, err) }
if valTok.Kind() != '"' { return result, fmt.Errorf(%q, valTok.Kind()) }
%s = valTok.String()
`,
		"reading "+`"`+f.JSONName+`"`+": %w",
		"expected string for "+`"`+f.JSONName+`"`+", got %v",
		ref)
}

func renderReadPrimBool(f FieldInfo, ref string) string {
	return fmt.Sprintf(`valTok, err := dec.ReadToken()
if err != nil { return result, fmt.Errorf(%q, err) }
if valTok.Kind() != 't' && valTok.Kind() != 'f' { return result, fmt.Errorf(%q, valTok.Kind()) }
%s = valTok.Bool()
`,
		"reading "+`"`+f.JSONName+`"`+": %w",
		"expected bool for "+`"`+f.JSONName+`"`+", got %v",
		ref)
}

func renderReadPrimNumber(f FieldInfo, ref string) string {
	extract := tokenExtract(f.Kind, "valTok")
	return fmt.Sprintf(`valTok, err := dec.ReadToken()
if err != nil { return result, fmt.Errorf(%q, err) }
if valTok.Kind() != '0' { return result, fmt.Errorf(%q, valTok.Kind()) }
%s = %s
`,
		"reading "+`"`+f.JSONName+`"`+": %w",
		"expected number for "+`"`+f.JSONName+`"`+", got %v",
		ref, extract)
}

func renderReadStruct(f FieldInfo, ref string) string {
	wrap := "decoding " + `"` + f.JSONName + `"` + ": %w"
	if isGenerated(f.GoType) {
		return fmt.Sprintf(`nested, err := %s{}.DecodeFrom(dec)
if err != nil { return result, fmt.Errorf(%q, err) }
%s = nested
`, f.GoType, wrap, ref)
	}
	// Fallback for types not in this generation pass (e.g., cross-package):
	// delegate to json.UnmarshalDecode, which picks up any UnmarshalerFrom
	// implementation and otherwise uses reflection.
	return fmt.Sprintf(`if err := json.UnmarshalDecode(dec, &%s); err != nil {
	return result, fmt.Errorf(%q, err)
}
`, ref, wrap)
}

func renderReadStringTag(f FieldInfo, ref string) string {
	// json:",string" — read a JSON string and parse its contents.
	j := `"` + f.JSONName + `"`
	base := fmt.Sprintf(`valTok, err := dec.ReadToken()
if err != nil { return result, fmt.Errorf(%q, err) }
if valTok.Kind() != '"' { return result, fmt.Errorf(%q, valTok.Kind()) }
strVal := valTok.String()
`,
		"reading "+j+": %w",
		"expected string-wrapped value for "+j+", got %v")
	switch f.Kind {
	case KindBool:
		return base + fmt.Sprintf(`switch strVal {
case "true": %s = true
case "false": %s = false
default: return result, fmt.Errorf(%q, strVal)
}
`, ref, ref, j+": %q is not a valid bool")
	case KindFloat64:
		return base + fmt.Sprintf(`f, err := strconv.ParseFloat(strVal, 64)
if err != nil { return result, fmt.Errorf(%q, err) }
%s = f
`, j+": %w", ref)
	case KindUint64:
		return base + fmt.Sprintf(`u, err := strconv.ParseUint(strVal, 10, 64)
if err != nil { return result, fmt.Errorf(%q, err) }
%s = u
`, j+": %w", ref)
	case KindInt, KindInt64:
		cast := "n"
		if f.Kind == KindInt {
			cast = "int(n)"
		}
		return base + fmt.Sprintf(`n, err := strconv.ParseInt(strVal, 10, 64)
if err != nil { return result, fmt.Errorf(%q, err) }
%s = %s
`, j+": %w", ref, cast)
	}
	return base
}

// Slice/bytes/time/duration/netip readers are thin wrappers over the existing
// helpers, but re-exposed so renderReadBlockInto can call them with a custom
// ref (for pointer-to-value allocation).

func renderReadSlice(f FieldInfo, ref string) string {
	// For slices, call the existing template logic is awkward from Go; so we
	// inline a minimal reader that mirrors it.
	j := `"` + f.JSONName + `"`
	var b strings.Builder
	fmt.Fprintf(&b, `arrTok, err := dec.ReadToken()
if err != nil { return result, fmt.Errorf(%q, err) }
if arrTok.Kind() != '[' { return result, fmt.Errorf(%q, arrTok.Kind()) }
for dec.PeekKind() != ']' {
`,
		"reading "+j+": %w",
		"expected '[' for "+j+", got %v")
	if f.ElemKind == KindStruct {
		if isGenerated(f.ElemType) {
			fmt.Fprintf(&b, `	elem, err := %s{}.DecodeFrom(dec)
	if err != nil { return result, fmt.Errorf(%q, err) }
`, f.ElemType, "decoding element of "+j+": %w")
		} else {
			fmt.Fprintf(&b, `	var elem %s
	if err := json.UnmarshalDecode(dec, &elem); err != nil {
		return result, fmt.Errorf(%q, err)
	}
`, f.ElemType, "decoding element of "+j+": %w")
		}
	} else {
		fmt.Fprintf(&b, `	elemTok, err := dec.ReadToken()
	if err != nil { return result, fmt.Errorf(%q, err) }
`, "reading element of "+j+": %w")
		if f.ElemKind == KindBool {
			fmt.Fprintf(&b, `	if elemTok.Kind() != 't' && elemTok.Kind() != 'f' { return result, fmt.Errorf(%q, elemTok.Kind()) }
`, "expected bool in "+j+", got %v")
		} else {
			fmt.Fprintf(&b, `	if elemTok.Kind() != %s { return result, fmt.Errorf(%q, elemTok.Kind()) }
`, tokenKind(f.ElemKind), "expected "+kindName(f.ElemKind)+" in "+j+", got %v")
		}
		fmt.Fprintf(&b, "\telem := %s\n", tokenExtract(f.ElemKind, "elemTok"))
	}
	if len(f.ElemMods) > 0 {
		b.WriteString(renderMods(f.ElemMods, "elem", f.ElemType, f.ElemKind))
	}
	if len(f.ElemValidation) > 0 {
		b.WriteString(renderElemValidation(f))
	}
	fmt.Fprintf(&b, "\t%s = append(%s, elem)\n", ref, ref)
	fmt.Fprintf(&b, `}
if _, err := dec.ReadToken(); err != nil { return result, fmt.Errorf(%q, err) }
`, "expected ']' for "+j+": %w")
	return b.String()
}

// renderReadMap emits unmarshal code for map[string]V. Dive-applied mods
// and validation run per-value.
func renderReadMap(f FieldInfo, ref string) string {
	j := `"` + f.JSONName + `"`
	var b strings.Builder
	b.WriteString("{\n") // scope block to isolate locals across adjacent maps
	fmt.Fprintf(&b, `objTok, err := dec.ReadToken()
if err != nil { return result, fmt.Errorf(%q, err) }
if objTok.Kind() != '{' { return result, fmt.Errorf(%q, objTok.Kind()) }
if %s == nil { %s = make(map[string]%s) }
for dec.PeekKind() != '}' {
	keyTok, err := dec.ReadToken()
	if err != nil { return result, fmt.Errorf(%q, err) }
	if keyTok.Kind() != '"' { return result, fmt.Errorf(%q, keyTok.Kind()) }
	_k := keyTok.String()
`,
		"reading "+j+": %w",
		"expected '{' for "+j+", got %v",
		ref, ref, f.ElemType,
		"reading key of "+j+": %w",
		"expected string key in "+j+", got %v")

	// Read value into _v based on element kind.
	if f.ElemKind == KindStruct {
		if isGenerated(f.ElemType) {
			fmt.Fprintf(&b, `	_v, err := %s{}.DecodeFrom(dec)
	if err != nil { return result, fmt.Errorf(%q, err) }
`, f.ElemType, "decoding value of "+j+": %w")
		} else {
			fmt.Fprintf(&b, `	var _v %s
	if err := json.UnmarshalDecode(dec, &_v); err != nil {
		return result, fmt.Errorf(%q, err)
	}
`, f.ElemType, "decoding value of "+j+": %w")
		}
	} else {
		fmt.Fprintf(&b, `	valTok, err := dec.ReadToken()
	if err != nil { return result, fmt.Errorf(%q, err) }
`, "reading value of "+j+": %w")
		if f.ElemKind == KindBool {
			fmt.Fprintf(&b, `	if valTok.Kind() != 't' && valTok.Kind() != 'f' { return result, fmt.Errorf(%q, valTok.Kind()) }
`, "expected bool in "+j+", got %v")
		} else {
			fmt.Fprintf(&b, `	if valTok.Kind() != %s { return result, fmt.Errorf(%q, valTok.Kind()) }
`, tokenKind(f.ElemKind), "expected "+kindName(f.ElemKind)+" in "+j+", got %v")
		}
		fmt.Fprintf(&b, "\t_v := %s\n", tokenExtract(f.ElemKind, "valTok"))
	}

	// dive: mods + validation apply to _v.
	if len(f.ElemMods) > 0 {
		b.WriteString(renderMods(f.ElemMods, "_v", f.ElemType, f.ElemKind))
	}
	if len(f.ElemValidation) > 0 {
		b.WriteString(renderValidationOn(f.ElemValidation, "_v", f.JSONName+"[]", f.ElemKind, f.MultiErr))
	}

	fmt.Fprintf(&b, "\t%s[_k] = _v\n", ref)
	fmt.Fprintf(&b, `}
if _, err := dec.ReadToken(); err != nil { return result, fmt.Errorf(%q, err) }
`, "expected '}' for "+j+": %w")
	b.WriteString("}\n") // close scope
	return b.String()
}

func renderReadBytesInto(f FieldInfo, ref string) string {
	saved := f.GoName
	// swap goName so the helper's `result.<GoName>` becomes `ref` — easier: rewrite.
	_ = saved
	return rewriteRef(renderReadBytes(f), "result."+f.GoName, ref)
}

func renderReadTimeInto(f FieldInfo, ref string) string {
	return rewriteRef(renderReadTime(f), "result."+f.GoName, ref)
}

func renderReadDurationInto(f FieldInfo, ref string) string {
	return rewriteRef(renderReadDuration(f), "result."+f.GoName, ref)
}

func renderReadNetIPInto(f FieldInfo, ref string) string {
	return rewriteRef(renderReadNetIP(f), "result."+f.GoName, ref)
}

func renderReadNetipAddrInto(f FieldInfo, ref string) string {
	return rewriteRef(renderReadNetipAddr(f), "result."+f.GoName, ref)
}

func renderReadNetipPrefixInto(f FieldInfo, ref string) string {
	return rewriteRef(renderReadNetipPrefix(f), "result."+f.GoName, ref)
}

// rewriteRef replaces occurrences of old with new in the generated Go code.
// Used by the *Into wrappers to redirect assignments to a scratch variable
// when rendering pointer-wrapped reads.
func rewriteRef(src, old, new string) string {
	return strings.ReplaceAll(src, old, new)
}

// renderReadBytes emits unmarshal code for a []byte field based on format.
func renderReadBytes(f FieldInfo) string {
	ref := "result." + f.GoName
	j := `"` + f.JSONName + `"`
	if f.Format == "array" {
		return fmt.Sprintf(`arrTok, err := dec.ReadToken()
if err != nil { return result, fmt.Errorf(%q, err) }
if arrTok.Kind() != '[' { return result, fmt.Errorf(%q, arrTok.Kind()) }
for dec.PeekKind() != ']' {
	elemTok, err := dec.ReadToken()
	if err != nil { return result, fmt.Errorf(%q, err) }
	if elemTok.Kind() != '0' { return result, fmt.Errorf(%q, elemTok.Kind()) }
	%s = append(%s, byte(elemTok.Uint()))
}
if _, err := dec.ReadToken(); err != nil { return result, fmt.Errorf(%q, err) }
`,
			"reading "+j+": %w",
			"expected '[' for "+j+", got %v",
			"reading element of "+j+": %w",
			"expected number in "+j+", got %v",
			ref, ref,
			"expected ']' for "+j+": %w")
	}
	parser := "base64.StdEncoding.DecodeString"
	switch f.Format {
	case "base64url":
		parser = "base64.URLEncoding.DecodeString"
	case "base32":
		parser = "base32.StdEncoding.DecodeString"
	case "base32hex":
		parser = "base32.HexEncoding.DecodeString"
	case "base16", "hex":
		parser = "hex.DecodeString"
	}
	return fmt.Sprintf(`valTok, err := dec.ReadToken()
if err != nil { return result, fmt.Errorf(%q, err) }
if valTok.Kind() != '"' { return result, fmt.Errorf(%q, valTok.Kind()) }
%s, err = %s(valTok.String())
if err != nil { return result, fmt.Errorf(%q, err) }
`,
		"reading "+j+": %w",
		"expected string for "+j+", got %v",
		ref, parser,
		j+": %w")
}

// renderReadTime emits unmarshal code for a time.Time field based on format.
func renderReadTime(f FieldInfo) string {
	ref := "result." + f.GoName
	j := `"` + f.JSONName + `"`
	layout, numeric := timeLayoutExpr(f.Format)
	if numeric != "" {
		// numeric unix family → read number token
		ctor := map[string]string{
			"Unix":      "time.Unix(valTok.Int(), 0)",
			"UnixMilli": "time.UnixMilli(valTok.Int())",
			"UnixMicro": "time.UnixMicro(valTok.Int())",
			"UnixNano":  "time.Unix(0, valTok.Int())",
		}[numeric]
		return fmt.Sprintf(`valTok, err := dec.ReadToken()
if err != nil { return result, fmt.Errorf(%q, err) }
if valTok.Kind() != '0' { return result, fmt.Errorf(%q, valTok.Kind()) }
%s = %s
`,
			"reading "+j+": %w",
			"expected number for "+j+", got %v",
			ref, ctor)
	}
	return fmt.Sprintf(`valTok, err := dec.ReadToken()
if err != nil { return result, fmt.Errorf(%q, err) }
if valTok.Kind() != '"' { return result, fmt.Errorf(%q, valTok.Kind()) }
%s, err = time.Parse(%s, valTok.String())
if err != nil { return result, fmt.Errorf(%q, err) }
`,
		"reading "+j+": %w",
		"expected string for "+j+", got %v",
		ref, layout,
		j+": %w")
}

// renderReadDuration emits unmarshal code for a time.Duration field.
func renderReadDuration(f FieldInfo) string {
	ref := "result." + f.GoName
	j := `"` + f.JSONName + `"`
	numRead := func(assign string) string {
		return fmt.Sprintf(`valTok, err := dec.ReadToken()
if err != nil { return result, fmt.Errorf(%q, err) }
if valTok.Kind() != '0' { return result, fmt.Errorf(%q, valTok.Kind()) }
%s
`,
			"reading "+j+": %w",
			"expected number for "+j+", got %v",
			assign)
	}
	switch f.Format {
	case "sec":
		return numRead(fmt.Sprintf("%s = time.Duration(valTok.Float() * float64(time.Second))", ref))
	case "milli":
		return numRead(fmt.Sprintf("%s = time.Duration(valTok.Int()) * time.Millisecond", ref))
	case "micro":
		return numRead(fmt.Sprintf("%s = time.Duration(valTok.Int()) * time.Microsecond", ref))
	case "nano":
		return numRead(fmt.Sprintf("%s = time.Duration(valTok.Int())", ref))
	}
	// units / default — parse string like "1h30m"
	return fmt.Sprintf(`valTok, err := dec.ReadToken()
if err != nil { return result, fmt.Errorf(%q, err) }
if valTok.Kind() != '"' { return result, fmt.Errorf(%q, valTok.Kind()) }
%s, err = time.ParseDuration(valTok.String())
if err != nil { return result, fmt.Errorf(%q, err) }
`,
		"reading "+j+": %w",
		"expected string for "+j+", got %v",
		ref,
		j+": %w")
}

// renderReadNetIP / renderReadNetipAddr / renderReadNetipPrefix share a shape.
func renderReadNetIP(f FieldInfo) string {
	ref := "result." + f.GoName
	j := `"` + f.JSONName + `"`
	return fmt.Sprintf(`valTok, err := dec.ReadToken()
if err != nil { return result, fmt.Errorf(%q, err) }
if valTok.Kind() != '"' { return result, fmt.Errorf(%q, valTok.Kind()) }
%s = net.ParseIP(valTok.String())
if %s == nil { return result, fmt.Errorf(%q) }
`,
		"reading "+j+": %w",
		"expected string for "+j+", got %v",
		ref, ref,
		j+": invalid IP")
}

func renderReadNetipAddr(f FieldInfo) string {
	ref := "result." + f.GoName
	j := `"` + f.JSONName + `"`
	return fmt.Sprintf(`valTok, err := dec.ReadToken()
if err != nil { return result, fmt.Errorf(%q, err) }
if valTok.Kind() != '"' { return result, fmt.Errorf(%q, valTok.Kind()) }
%s, err = netip.ParseAddr(valTok.String())
if err != nil { return result, fmt.Errorf(%q, err) }
`,
		"reading "+j+": %w",
		"expected string for "+j+", got %v",
		ref,
		j+": %w")
}

func renderReadNetipPrefix(f FieldInfo) string {
	ref := "result." + f.GoName
	j := `"` + f.JSONName + `"`
	return fmt.Sprintf(`valTok, err := dec.ReadToken()
if err != nil { return result, fmt.Errorf(%q, err) }
if valTok.Kind() != '"' { return result, fmt.Errorf(%q, valTok.Kind()) }
%s, err = netip.ParsePrefix(valTok.String())
if err != nil { return result, fmt.Errorf(%q, err) }
`,
		"reading "+j+": %w",
		"expected string for "+j+", got %v",
		ref,
		j+": %w")
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

// renderSize emits the body of a JSONSize method: sums the exact (or
// worst-case) byte count needed to serialize the struct as JSON. Per-field
// contributions are split into a compile-time constant (folded into the
// initial `size := N`) and runtime code (loops, len(), recursive calls).
func renderSize(s StructInfo) string {
	if s.IsAlias {
		return renderAliasSize(s)
	}
	var b strings.Builder
	// Fixed overhead: braces + per-field key bytes + separating commas.
	fixed := 2 // { and }
	named := 0
	for _, f := range s.Fields {
		if f.Inline {
			continue // name/colon/comma budgeted per-entry in map size
		}
		fixed += len(f.JSONName) + 3 // "name":
		if named > 0 {
			fixed++ // comma
		}
		named++
	}
	var runtime strings.Builder
	for _, f := range s.Fields {
		ref := "s." + f.GoName
		n, code := sizeContrib(f, ref)
		fixed += n
		runtime.WriteString(code)
	}
	fmt.Fprintf(&b, "size := %d\n", fixed)
	b.WriteString(runtime.String())
	b.WriteString("return size")
	return b.String()
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
		return prefix + `"`, fmt.Sprintf("dst = %s(dst, %s.String())\n", appendStrFn(f.HTMLEscape), ref), true
	case KindBigRat:
		return prefix + `"`, fmt.Sprintf("dst = %s(dst, (&%s).RatString())\n", appendStrFn(f.HTMLEscape), ref), true
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
	sizeStrMult = 2  // 2× for escape worst case
	sizeStrPad  = 2  // surrounding quotes
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
		var b strings.Builder
		fmt.Fprintf(&b, "if %s == nil { size += 4 } else {\n", ref)
		if innerN > 0 {
			fmt.Fprintf(&b, "size += %d\n", innerN)
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
		if isGenerated(f.GoType) {
			return 0, fmt.Sprintf("size += %s.JSONSize()\n", ref)
		}
		return 128, ""
	case KindSlice, KindArray:
		return sizeSliceContrib(f, ref, 0)
	case KindMap:
		return sizeMapContrib(f, ref)
	case KindBytes:
		// base64 ≈ 4/3 of input + padding + quotes; array ≈ 4 per byte + brackets.
		if f.Format == "array" {
			return 2, fmt.Sprintf("size += len(%s)*4\n", ref)
		}
		// Worst case: hex = 2x, base64 ≈ 1.34x. Use 2x + quotes as upper bound.
		return 2, fmt.Sprintf("size += len(%s)*2\n", ref)
	case KindTime:
		// RFC3339Nano max ~35 chars including quotes; unix number up to 20.
		return 40, ""
	case KindDuration:
		// Number or "NhMm..." string; 32 upper bound.
		return 32, ""
	case KindNetIP, KindNetipAddr:
		// IPv6 max "xxxx:...:xxxx" + zone = ~50 bytes + quotes.
		return 54, ""
	case KindNetipPrefix:
		// IPv6 prefix "addr/128" = ~54 bytes + quotes.
		return 58, ""
	case KindRawJSON:
		return 0, fmt.Sprintf("if _n := len(%s); _n > 0 { size += _n } else { size += 4 }\n", ref)
	case KindURL:
		// URL string + quotes. 1 KB conservative upper bound — most URLs
		// are well under, but path/query can grow.
		return 1024, ""
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
	var b strings.Builder
	fmt.Fprintf(&b, "if _n := len(%s); _n > 0 { size += _n - 1 }\n", ref)
	switch f.ElemKind {
	case KindString:
		fmt.Fprintf(&b, "for %s := range %s { size += len(%s[%s])*%d + %d }\n",
			ivar, ref, ref, ivar, sizeStrMult, sizeStrPad)
	case KindBool:
		fmt.Fprintf(&b, "size += len(%s) * %d\n", ref, sizeBool)
	case KindInt, KindInt64, KindInt8, KindInt16, KindInt32:
		fmt.Fprintf(&b, "size += len(%s) * %d\n", ref, sizeInt)
	case KindUint, KindUint64, KindUint8, KindUint16, KindUint32:
		fmt.Fprintf(&b, "size += len(%s) * %d\n", ref, sizeUint)
	case KindFloat32, KindFloat64:
		fmt.Fprintf(&b, "size += len(%s) * %d\n", ref, sizeFloat)
	case KindStruct:
		if isGenerated(f.ElemType) {
			if f.ElemPointer {
				// `[]*T` / `[N]*T`: nil elements contribute `null` (4 bytes),
				// non-nil deref-and-call.
				fmt.Fprintf(&b, "for %s := range %s {\nif %s[%s] == nil { size += 4 } else { size += (*%s[%s]).JSONSize() }\n}\n",
					ivar, ref, ref, ivar, ref, ivar)
			} else {
				fmt.Fprintf(&b, "for %s := range %s { size += %s[%s].JSONSize() }\n",
					ivar, ref, ref, ivar)
			}
		} else {
			fmt.Fprintf(&b, "size += len(%s) * 128\n", ref)
		}
	case KindSlice, KindArray:
		fmt.Fprintf(&b, "for %s := range %s {\n", ivar, ref)
		innerN, innerCode := sizeSliceContrib(peelSliceField(f), fmt.Sprintf("%s[%s]", ref, ivar), depth+1)
		if innerN > 0 {
			fmt.Fprintf(&b, "size += %d\n", innerN)
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
	var b strings.Builder
	const perEntryFixed = 4

	// Try to lift the value contribution out of the loop when it's a
	// constant per-entry size — saves one map iteration over keys-only.
	if v, ok := constSizePerEntry(f.ElemKind); ok {
		fmt.Fprintf(&b, "size += len(%s) * %d\n", ref, perEntryFixed+v)
		fmt.Fprintf(&b, "for _k := range %s { size += len(_k) * %d }\n", ref, sizeStrMult)
		return 2, b.String()
	}

	// Variable per-entry: one combined loop over k,v.
	fmt.Fprintf(&b, "size += len(%s) * %d\n", ref, perEntryFixed)
	fmt.Fprintf(&b, "for _k, _v := range %s {\n", ref)
	fmt.Fprintf(&b, "size += len(_k) * %d\n", sizeStrMult)
	switch f.ElemKind {
	case KindString:
		fmt.Fprintf(&b, "size += len(_v)*%d + %d\n", sizeStrMult, sizeStrPad)
	case KindStruct:
		if isGenerated(f.ElemType) {
			b.WriteString("size += _v.JSONSize()\n")
		} else {
			b.WriteString("size += 128\n")
		}
	case KindBigInt:
		b.WriteString("size += _v.BitLen()/3 + 4\n")
	case KindBigRat:
		b.WriteString("size += (_v.Num().BitLen() + _v.Denom().BitLen())/3 + 8\n")
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
func constSizePerEntry(kind TypeKind) (int, bool) {
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
		return 40, true
	case KindDuration:
		return 32, true
	case KindNetIP, KindNetipAddr:
		return 54, true
	case KindNetipPrefix:
		return 58, true
	case KindBigFloat:
		return 66, true // +2 for surrounding quotes
	case KindURL:
		return 1024, true
	case KindAny:
		return 256, true
	}
	return 0, false
}

func renderAppendSlice(f FieldInfo, ref string) string {
	return emitAppendSlice(f, ref, 0)
}

// emitAppendSlice is the recursive marshal counterpart to emitByteSliceRead.
// Nested slices peel one [] off per level; loop vars carry a depth suffix
// (i0, v0 at the outermost, i1, v1 one level in, …) to avoid collisions.
//
// nil-slice handling: a nil slice serializes as JSON `null` to match stdlib
// `encoding/json` v1/v2. Empty non-nil slice still serializes as `[]`.
// Fixed-length arrays can't be nil so they skip the check.
func emitAppendSlice(f FieldInfo, ref string, depth int) string {
	vvar := fmt.Sprintf("v%d", depth)
	var b strings.Builder
	if f.Kind == KindSlice {
		fmt.Fprintf(&b, "if %s == nil {\ndst = append(dst, \"null\"...)\n} else {\n", ref)
	}
	b.WriteString("dst = append(dst, '[')\n")
	fmt.Fprintf(&b, "if len(%s) > 0 {\n", ref)
	// First element: no leading comma. Refer to it directly as `ref[0]`
	// to keep the emit primitive — saves declaring the loop var twice.
	b.WriteString(emitSliceElement(f, fmt.Sprintf("%s[0]", ref), depth))
	// Rest: comma first, then element. Iterating over `ref[1:]` lifts the
	// `if i > 0` branch out of every iteration.
	fmt.Fprintf(&b, "for _, %s := range %s[1:] {\n", vvar, ref)
	b.WriteString("dst = append(dst, ',')\n")
	b.WriteString(emitSliceElement(f, vvar, depth))
	b.WriteString("}\n")
	b.WriteString("}\n")
	b.WriteString("dst = append(dst, ']')\n")
	if f.Kind == KindSlice {
		b.WriteString("}\n")
	}
	return b.String()
}

// emitSliceElement emits the marshal code for one slice element at the
// given source expression. Shared between the first-element and loop-body
// emits in emitAppendSlice so the per-iteration `if i > 0` check is gone.
func emitSliceElement(f FieldInfo, vref string, depth int) string {
	var b strings.Builder
	if f.ElemPointer {
		// nil pointer element → null. Else emit as if it were a value
		// (Go auto-derefs the pointer for value-receiver method calls).
		fmt.Fprintf(&b, "if %s == nil {\ndst = append(dst, \"null\"...)\n} else {\n", vref)
		// Recurse with ElemPointer cleared on a copy of f so the nested
		// emit doesn't re-trigger the nil-check.
		nf := f
		nf.ElemPointer = false
		b.WriteString(emitSliceElement(nf, "(*"+vref+")", depth))
		b.WriteString("}\n")
		return b.String()
	}
	switch f.ElemKind {
	case KindString:
		// Split into two lines so the coalescer pass can fold the `'"'`
		// with whatever const append precedes (e.g. the slice's leading
		// `,` becomes `","...`).
		b.WriteString("dst = append(dst, '\"')\n")
		fmt.Fprintf(&b, "dst = %s(dst, %s)\n", appendStrFn(f.HTMLEscape), vref)
	case KindBool:
		fmt.Fprintf(&b, "dst = strconv.AppendBool(dst, %s)\n", vref)
	case KindInt, KindInt8, KindInt16, KindInt32:
		fmt.Fprintf(&b, "dst = strconv.AppendInt(dst, int64(%s), 10)\n", vref)
	case KindInt64:
		fmt.Fprintf(&b, "dst = strconv.AppendInt(dst, %s, 10)\n", vref)
	case KindUint, KindUint8, KindUint16, KindUint32:
		fmt.Fprintf(&b, "dst = strconv.AppendUint(dst, uint64(%s), 10)\n", vref)
	case KindUint64:
		fmt.Fprintf(&b, "dst = strconv.AppendUint(dst, %s, 10)\n", vref)
	case KindFloat32:
		fmt.Fprintf(&b, "dst = strconv.AppendFloat(dst, float64(%s), 'g', -1, 32)\n", vref)
	case KindFloat64:
		fmt.Fprintf(&b, "dst = strconv.AppendFloat(dst, %s, 'g', -1, 64)\n", vref)
	case KindStruct:
		if isGenerated(f.ElemType) {
			fmt.Fprintf(&b, "if dst, err = %s.AppendJSON(dst); err != nil { return dst, err }\n", vref)
		} else {
			fmt.Fprintf(&b, `{
	_b, _err := json.Marshal(%s)
	if _err != nil { return dst, _err }
	dst = append(dst, _b...)
}
`, vref)
		}
	case KindSlice, KindArray:
		b.WriteString(emitAppendSlice(peelSliceField(f), vref, depth+1))
	}
	return b.String()
}

// renderAppendMap emits marshal code for a map[string]V field. Iteration
// order is Go's randomized map order — deterministic roundtrip via
// unmarshal, but wire output is not stable. Wrapped in a block scope so
// adjacent maps don't collide on the `_first` variable.
func renderAppendMap(f FieldInfo, ref string) string {
	var b strings.Builder
	// nil map → null (matches stdlib). Empty non-nil → {}.
	fmt.Fprintf(&b, "if %s == nil {\ndst = append(dst, \"null\"...)\n} else {\n", ref)
	// JSON `{` lives OUTSIDE the Go scoping block so coalesceConstAppends
	// can merge it with the preceding `"key":` prefix. Same for the
	// closing `}` and whatever comes after.
	b.WriteString("dst = append(dst, '{')\n")
	b.WriteString("{\n")
	fmt.Fprintf(&b, "_first := true\n")
	fmt.Fprintf(&b, "for _k, _v := range %s {\n", ref)
	// One conditional with two append shapes: `,"` on the not-first
	// branch (so coalesce keeps them together) and a bare `"` on the
	// first iteration where _first transitions to false.
	b.WriteString("if _first { _first = false\ndst =append(dst, '\"') } else { dst = append(dst, \",\\\"\"...) }\n")
	fmt.Fprintf(&b, "dst = %s(dst, _k)\n", appendStrFn(f.HTMLEscape))
	b.WriteString("dst = append(dst, ':')\n")
	switch f.ElemKind {
	case KindString:
		// Two separate append lines so coalesce sees the `'"'` and merges
		// it with the preceding `':'` into `":\""...`.
		b.WriteString("dst = append(dst, '\"')\n")
		fmt.Fprintf(&b, "dst = %s(dst, _v)\n", appendStrFn(f.HTMLEscape))
	case KindBool:
		b.WriteString("dst = strconv.AppendBool(dst, _v)\n")
	case KindInt:
		b.WriteString("dst = strconv.AppendInt(dst, int64(_v), 10)\n")
	case KindInt64:
		b.WriteString("dst = strconv.AppendInt(dst, _v, 10)\n")
	case KindUint64:
		b.WriteString("dst = strconv.AppendUint(dst, _v, 10)\n")
	case KindFloat64:
		b.WriteString("dst = strconv.AppendFloat(dst, _v, 'g', -1, 64)\n")
	case KindStruct:
		if isGenerated(f.ElemType) {
			b.WriteString("if dst, err = _v.AppendJSON(dst); err != nil { return dst, err }\n")
		} else {
			b.WriteString(`{
	_b, _err := json.Marshal(_v)
	if _err != nil { return dst, _err }
	dst = append(dst, _b...)
}
`)
		}
	case KindAny:
		b.WriteString("if dst, err = encode.AppendAny(dst, _v); err != nil { return dst, err }\n")
	}
	b.WriteString("}\n") // close for
	b.WriteString("}\n") // close Go scope
	b.WriteString("dst = append(dst, '}')\n")
	b.WriteString("}\n") // close else (nil-map check)
	return b.String()
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
//       N >  0 → that many entries
//       N == 0 → user opt-out, force zero (overrides len/minlen)
//       N <  0 → sentinel "unset" — fall through
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
func inlineSkipWS(posVar string) string {
	return fmt.Sprintf(
		"for %s < len(data) && (data[%s] == ' ' || data[%s] == '\\t' || data[%s] == '\\n' || data[%s] == '\\r') { %s++ }\n",
		posVar, posVar, posVar, posVar, posVar, posVar)
}

// inlineScanInt64 emits an inline signed-int scanner that assigns into dst
// (via castFn if non-empty, e.g. "int") and advances posVar. Avoids the
// scan.Int64 call for the hot int fields.
func inlineScanInt64(posVar, dst, castFn string) string {
	assign := ""
	switch {
	case castFn != "":
		assign = dst + " = " + castFn + "(_n)"
	case dst != "_n":
		assign = dst + " = _n"
	}
	return fmt.Sprintf(`{
	_neg := false
	if %s < len(data) && data[%s] == '-' { _neg = true; %s++ }
	if %s >= len(data) || data[%s] < '0' || data[%s] > '9' { return result, 0, scan.ErrBadNumber }
	var _n int64
	for %s < len(data) && data[%s] >= '0' && data[%s] <= '9' {
		_n = _n*10 + int64(data[%s]-'0')
		%s++
	}
	if %s < len(data) {
		_c := data[%s]
		if _c == '.' || _c == 'e' || _c == 'E' { return result, 0, scan.ErrBadNumber }
	}
	if _neg { _n = -_n }
	%s
}
`, posVar, posVar, posVar,
		posVar, posVar, posVar,
		posVar, posVar, posVar, posVar, posVar,
		posVar, posVar,
		assign)
}

// inlineScanUint64 is the unsigned counterpart of inlineScanInt64.
func inlineScanUint64(posVar, dst, castFn string) string {
	assign := ""
	switch {
	case castFn != "":
		assign = dst + " = " + castFn + "(_n)"
	case dst != "_n":
		assign = dst + " = _n"
	}
	return fmt.Sprintf(`{
	if %s >= len(data) || data[%s] < '0' || data[%s] > '9' { return result, 0, scan.ErrBadNumber }
	var _n uint64
	for %s < len(data) && data[%s] >= '0' && data[%s] <= '9' {
		_n = _n*10 + uint64(data[%s]-'0')
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
func inlineScanString(posIn, dst, posOut string) string {
	// Inner-scope local names (`_isv`, `_isj`, `_iserr`) are deliberately
	// chosen to avoid colliding with caller-side locals named `_sv` etc.
	// Otherwise `:= scan.String(...)` shadows the caller's `_sv` and the
	// follow-up assignment becomes a `_sv = _sv` self-assign.
	return fmt.Sprintf(`if %s >= len(data) || data[%s] != '"' { return result, 0, scan.ErrExpectString }
{
	_ks := %s + 1
	_ke := _ks
	for _ke < len(data) && data[_ke] != '"' && data[_ke] != '\\' { _ke++ }
	if _ke >= len(data) { return result, 0, scan.ErrUnterminated }
	if data[_ke] == '"' {
		%s = unsafe.String(unsafe.SliceData(data[_ks:]), _ke-_ks)
		%s = _ke + 1
	} else {
		_isv, _isj, _iserr := scan.String(data, %s)
		if _iserr != nil { return result, 0, _iserr }
		%s = _isv
		%s = _isj
	}
}
`, posIn, posIn, posIn, dst, posOut, posIn, dst, posOut)
}

// renderDecode emits the body of DecodeFrom: a loop that reads each
// JSON key, dispatches to per-field scan code, and handles ',' / '}'.
// Zero-copy (strings alias the input) and zero-alloc on the happy path.
// Dispatch is length-first so missing keys reject with a single int compare
// instead of a string compare per case. Whitespace skipping is inlined at
// each hot-path site to avoid the ~5ns/call overhead dominating runtime.
func renderDecode(s StructInfo) string {
	if s.IsAlias {
		return renderAliasDecode(s)
	}
	var b strings.Builder
	b.WriteString("var result " + s.Name + "\n")
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
				fmt.Fprintf(&b, "seen%s := false\n", f.GoName)
			}
		}
	}

	b.WriteString(inlineSkipWS("i"))
	b.WriteString("if i >= len(data) || data[i] != '{' { return result, 0, scan.ErrBadObject }\n")
	b.WriteString("i++\n")
	b.WriteString(inlineSkipWS("i"))
	b.WriteString("if i < len(data) && data[i] == '}' {\n")
	b.WriteString(renderPostLoop(s))
	b.WriteString("return result, i + 1, nil\n}\n")
	b.WriteString("for {\n")
	b.WriteString("var key string\n")
	b.WriteString("j := i\n")
	b.WriteString(inlineScanString("i", "key", "j"))
	b.WriteString(inlineSkipWS("j"))
	b.WriteString("if j >= len(data) || data[j] != ':' { return result, 0, scan.ErrBadObject }\n")
	b.WriteString("j++\n")
	b.WriteString(inlineSkipWS("j"))
	b.WriteString(renderDispatch(s))
	b.WriteString(inlineSkipWS("j"))
	b.WriteString("if j >= len(data) { return result, 0, scan.ErrBadObject }\n")
	b.WriteString("if data[j] == ',' { j++; i = j; ")
	b.WriteString(inlineSkipWS("i"))
	b.WriteString("continue }\n")
	b.WriteString("if data[j] == '}' {\n")
	b.WriteString(renderPostLoop(s))
	b.WriteString("return result, j + 1, nil\n}\n")
	b.WriteString("return result, 0, scan.ErrBadObject\n")
	b.WriteString("}\n")
	return b.String()
}

// renderPostLoop emits end-of-parse bookkeeping: required-field checks
// (when validation is on) and the multierr flush (when MultiErr is on).
// Called at every success exit inside DecodeFrom / DecodeStreamFrom.
func renderPostLoop(s StructInfo) string {
	var b strings.Builder
	if !s.NoValidate {
		for _, f := range s.Fields {
			if !f.IsRequired() || f.Inline {
				continue
			}
			errExpr := requiredErr(f.JSONName)
			notSeen := seenNotAccess(s, f)
			if s.MultiErr {
				fmt.Fprintf(&b, "if %s { errs = append(errs, %s) }\n", notSeen, errExpr)
			} else {
				fmt.Fprintf(&b, "if %s { return result, 0, %s }\n", notSeen, errExpr)
			}
		}
	}
	if s.MultiErr {
		b.WriteString("if len(errs) > 0 { return result, 0, errs }\n")
	}
	return b.String()
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
	emitField := func(b *strings.Builder, f FieldInfo, parse string) {
		if f.Inline || !needsSeen(f) {
			b.WriteString(parse)
			return
		}
		set := seenSet(s, f)
		seen := seenAccess(s, f)
		if s.AllowDups {
			fmt.Fprintf(b, `if %s {
	_skipJ, _skipErr := scan.SkipValue(data, j)
	if _skipErr != nil { return result, 0, _skipErr }
	j = _skipJ
} else {
	%s%s
}
`, seen, set, parse)
			return
		}
		if s.MultiErr {
			fmt.Fprintf(b, `if %s {
	errs = append(errs, &validation.DuplicateKeyError{Field: %q})
	_skipJ, _skipErr := scan.SkipValue(data, j)
	if _skipErr != nil { return result, 0, _skipErr }
	j = _skipJ
} else {
	%s%s
}
`, seen, f.JSONName, set, parse)
			return
		}
		fmt.Fprintf(b, `if %s { return result, 0, &validation.DuplicateKeyError{Field: %q} }
%s%s`, seen, f.JSONName, set, parse)
	}

	var b strings.Builder
	b.WriteString("switch len(key) {\n")
	for _, n := range lens {
		fs := byLen[n]
		fmt.Fprintf(&b, "case %d:\n", n)
		if len(fs) == 1 {
			f := fs[0]
			fmt.Fprintf(&b, "if key == %q {\n", f.JSONName)
			emitField(&b, f, renderField(f, "result."+f.GoName, "j"))
			b.WriteString("} else {\n")
			b.WriteString(unknownKey(s, "j"))
			b.WriteString("}\n")
			continue
		}
		b.WriteString("switch key {\n")
		for _, f := range fs {
			fmt.Fprintf(&b, "case %q:\n", f.JSONName)
			emitField(&b, f, renderField(f, "result."+f.GoName, "j"))
		}
		b.WriteString("default:\n")
		b.WriteString(unknownKey(s, "j"))
		b.WriteString("}\n")
	}
	b.WriteString("default:\n")
	b.WriteString(unknownKey(s, "j"))
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
	var out strings.Builder
	if len(f.KeyMods) > 0 {
		out.WriteString(renderMods(f.KeyMods, keyRef, "string", KindString))
	}
	if len(f.KeyValidation) > 0 {
		code := renderValidationOn(f.KeyValidation, keyRef, f.JSONName+".key", KindString, f.MultiErr)
		if !f.MultiErr {
			code = strings.ReplaceAll(code, "return result, ", "return result, 0, ")
		}
		out.WriteString(code)
	}
	return out.String()
}

// renderMap emits map[string]V decode. Accepts `null` → leave field nil
// (matches stdlib `encoding/json`).
func renderMap(f FieldInfo, ref, posVar string) string {
	var b strings.Builder
	b.WriteString("{\n")
	fmt.Fprintf(&b, "k := %s\n", posVar)
	b.WriteString(inlineSkipWS("k"))
	fmt.Fprintf(&b, "if _np, _ok := scan.Null(data, k); _ok {\n%s = _np\n} else {\n", posVar)
	b.WriteString("if k >= len(data) || data[k] != '{' { return result, 0, scan.ErrBadObject }\n")
	b.WriteString("k++\n")
	b.WriteString(inlineSkipWS("k"))
	// Empty `{}` → non-nil empty (stdlib parity); else fresh make()
	// with optional sizing hint. The surrounding DecodeFrom's
	// `var result T` builds fresh, so ref is always nil here — no
	// reuse branch to emit.
	fmt.Fprintf(&b, "if k < len(data) && data[k] == '}' {\n")
	fmt.Fprintf(&b, "%s = %s{}\n", ref, f.GoType)
	fmt.Fprintf(&b, "} else {\n")
	if cap := mapPreallocCap(f); cap > 0 {
		fmt.Fprintf(&b, "%s = make(%s, %d)\n", ref, f.GoType, cap)
	} else {
		fmt.Fprintf(&b, "%s = make(%s)\n", ref, f.GoType)
	}
	fmt.Fprintf(&b, "}\n")
	b.WriteString("for k < len(data) && data[k] != '}' {\n")
	b.WriteString("var _mk string\n")
	b.WriteString(inlineScanString("k", "_mk", "k"))
	b.WriteString(keyValidateAndMod(f, "_mk"))
	b.WriteString(inlineSkipWS("k"))
	b.WriteString("if k >= len(data) || data[k] != ':' { return result, 0, scan.ErrBadObject }\n")
	b.WriteString("k++\n")
	b.WriteString(inlineSkipWS("k"))
	switch f.ElemKind {
	case KindString:
		b.WriteString("var _mv string\n")
		b.WriteString(inlineScanString("k", "_mv", "k"))
		fmt.Fprintf(&b, "%s[_mk] = _mv\n", ref)
	case KindBool:
		fmt.Fprintf(&b, "_mv, _mk2, err := scan.Bool(data, k)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "%s[_mk] = _mv\nk = _mk2\n", ref)
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		b.WriteString("var _mn int64\n")
		b.WriteString(inlineScanInt64("k", "_mn", ""))
		if f.ElemType == "int64" {
			fmt.Fprintf(&b, "%s[_mk] = _mn\n", ref)
		} else {
			fmt.Fprintf(&b, "%s[_mk] = %s(_mn)\n", ref, f.ElemType)
		}
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		b.WriteString("var _mn uint64\n")
		b.WriteString(inlineScanUint64("k", "_mn", ""))
		if f.ElemType == "uint64" {
			fmt.Fprintf(&b, "%s[_mk] = _mn\n", ref)
		} else {
			fmt.Fprintf(&b, "%s[_mk] = %s(_mn)\n", ref, f.ElemType)
		}
	case KindFloat32, KindFloat64:
		b.WriteString("_mv, _mk2, err := scan.Float64(data, k)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		if f.ElemKind == KindFloat32 {
			fmt.Fprintf(&b, "%s[_mk] = float32(_mv)\n", ref)
		} else {
			fmt.Fprintf(&b, "%s[_mk] = _mv\n", ref)
		}
		b.WriteString("k = _mk2\n")
	case KindStruct:
		if isGenerated(f.ElemType) {
			// var _z is a stack-allocated zero value used purely as a
			// type carrier for the value-receiver method call — works
			// for both struct types and primitive aliases (where `T{}`
			// composite-literal would be a compile error).
			fmt.Fprintf(&b, "var _z %s\n_mv, _mk2, err := _z.DecodeFrom(data, k)\n", f.ElemType)
			b.WriteString("if err != nil { return result, 0, err }\n")
			fmt.Fprintf(&b, "%s[_mk] = _mv\nk = _mk2\n", ref)
		} else {
			fmt.Fprintf(&b, `_start := k
_mk2, err := scan.SkipValue(data, _start)
if err != nil { return result, 0, err }
var _mv %s
if err := json.Unmarshal(data[_start:_mk2], &_mv); err != nil { return result, 0, err }
%s[_mk] = _mv
k = _mk2
`, f.ElemType, ref)
		}
	default:
		b.WriteString("_mk2, err := scan.SkipValue(data, k)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		b.WriteString("k = _mk2\n")
	}
	// dive-mods on _mv — only for string elem; patch ref in the mod output.
	if len(f.ElemMods) > 0 {
		patched := strings.ReplaceAll(renderMods(f.ElemMods, "_mvx", f.ElemType, f.ElemKind), "_mvx", fmt.Sprintf("%s[_mk]", ref))
		b.WriteString(patched)
	}
	if len(f.ElemValidation) > 0 {
		code := renderValidationOn(f.ElemValidation, fmt.Sprintf("%s[_mk]", ref), f.JSONName+".value", f.ElemKind, f.MultiErr)
		code = strings.ReplaceAll(code, "return result, &validation.", "return result, 0, &validation.")
		b.WriteString(code)
	}
	b.WriteString(inlineSkipWS("k"))
	b.WriteString("if k < len(data) && data[k] == ',' { k++; ")
	b.WriteString(inlineSkipWS("k"))
	b.WriteString("continue }\n")
	b.WriteString("break\n")
	b.WriteString("}\n")
	b.WriteString("if k >= len(data) || data[k] != '}' { return result, 0, scan.ErrBadObject }\n")
	fmt.Fprintf(&b, "%s = k + 1\n", posVar)
	b.WriteString("}\n") // close else (null-check)
	b.WriteString("}\n") // close outer block
	return b.String()
}

// renderBytes emits bytes decode for (base64/hex/array).
func renderBytes(f FieldInfo, ref, posVar string) string {
	if f.Format == "array" {
		return fmt.Sprintf(`{
	k := %s
	%s
	if k >= len(data) || data[k] != '[' { return result, 0, scan.ErrBadArray }
	k++
	%s
	for k < len(data) && data[k] != ']' {
		_v, _k, err := scan.Uint64(data, k)
		if err != nil { return result, 0, err }
		%s = append(%s, byte(_v))
		k = _k
		%s
		if k < len(data) && data[k] == ',' { k++; %s continue }
		break
	}
	if k >= len(data) || data[k] != ']' { return result, 0, scan.ErrBadArray }
	%s = k + 1
}
`, posVar, inlineSkipWS("k"), inlineSkipWS("k"), ref, ref, inlineSkipWS("k"), inlineSkipWS("k"), posVar)
	}
	parser := "base64.StdEncoding.DecodeString"
	switch f.Format {
	case "base64url":
		parser = "base64.URLEncoding.DecodeString"
	case "base32":
		parser = "base32.StdEncoding.DecodeString"
	case "base32hex":
		parser = "base32.HexEncoding.DecodeString"
	case "base16", "hex":
		parser = "hex.DecodeString"
	}
	return fmt.Sprintf(`{
	var _s string
	%s
	var err error
	%s, err = %s(_s)
	if err != nil { return result, 0, err }
}
`, inlineScanString(posVar, "_s", posVar), ref, parser)
}

// renderTime emits time.Time decode decode.
func renderTime(f FieldInfo, ref, posVar string) string {
	layout, numeric := timeLayoutExpr(f.Format)
	if numeric != "" {
		ctor := map[string]string{
			"Unix":      "time.Unix(_n, 0)",
			"UnixMilli": "time.UnixMilli(_n)",
			"UnixMicro": "time.UnixMicro(_n)",
			"UnixNano":  "time.Unix(0, _n)",
		}[numeric]
		return fmt.Sprintf(`{
	_n, _k, err := scan.Int64(data, %s)
	if err != nil { return result, 0, err }
	%s = %s
	%s = _k
}
`, posVar, ref, ctor, posVar)
	}
	return fmt.Sprintf(`{
	var _s string
	%s
	var err error
	%s, err = time.Parse(%s, _s)
	if err != nil { return result, 0, err }
}
`, inlineScanString(posVar, "_s", posVar), ref, layout)
}

// renderDuration emits time.Duration decode decode.
func renderDuration(f FieldInfo, ref, posVar string) string {
	switch f.Format {
	case "sec":
		return fmt.Sprintf(`{
	_v, _k, err := scan.Float64(data, %s)
	if err != nil { return result, 0, err }
	%s = time.Duration(_v * float64(time.Second))
	%s = _k
}
`, posVar, ref, posVar)
	case "milli", "micro", "nano":
		unit := map[string]string{
			"milli": "time.Millisecond",
			"micro": "time.Microsecond",
			"nano":  "time.Nanosecond",
		}[f.Format]
		return fmt.Sprintf(`{
	_n, _k, err := scan.Int64(data, %s)
	if err != nil { return result, 0, err }
	%s = time.Duration(_n) * %s
	%s = _k
}
`, posVar, ref, unit, posVar)
	}
	return fmt.Sprintf(`{
	var _s string
	%s
	var err error
	%s, err = time.ParseDuration(_s)
	if err != nil { return result, 0, err }
}
`, inlineScanString(posVar, "_s", posVar), ref)
}

// renderNetIP / renderNetipAddr / renderNetipPrefix.
func renderNetIP(ref, posVar string) string {
	return fmt.Sprintf(`{
	var _s string
	%s
	%s = net.ParseIP(_s)
	if %s == nil { return result, 0, fmt.Errorf("invalid IP") }
}
`, inlineScanString(posVar, "_s", posVar), ref, ref)
}

func renderNetipAddr(ref, posVar string) string {
	return fmt.Sprintf(`{
	var _s string
	%s
	var err error
	%s, err = netip.ParseAddr(_s)
	if err != nil { return result, 0, err }
}
`, inlineScanString(posVar, "_s", posVar), ref)
}

func renderNetipPrefix(ref, posVar string) string {
	return fmt.Sprintf(`{
	var _s string
	%s
	var err error
	%s, err = netip.ParsePrefix(_s)
	if err != nil { return result, 0, err }
}
`, inlineScanString(posVar, "_s", posVar), ref)
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
	_v, _k, _err := %s.DecodeFrom(data, %s)
	if _err != nil { return result, 0, _err }
	%s = _v
	%s = _k
}
`, ref, posVar, ref, posVar)

		case f.Iface.JSONUnmarshaler:
			// Has UnmarshalJSON — capture raw JSON span and call it.
			// Avoids reflection-based json.Unmarshal setup.
			return fmt.Sprintf(`{
	_start := %s
	_k, _err := scan.SkipValue(data, _start)
	if _err != nil { return result, 0, _err }
	if _err := %s.UnmarshalJSON(data[_start:_k]); _err != nil { return result, 0, _err }
	%s = _k
}
`, posVar, ref, posVar)

		case f.Iface.TextUnmarshaler:
			// Type encodes as a JSON string — scan it, alias into []byte
			// without copying, hand to UnmarshalText.
			return fmt.Sprintf(`{
	_ts, _tj, _terr := scan.String(data, %s)
	if _terr != nil { return result, 0, _terr }
	if _err := %s.UnmarshalText(unsafe.Slice(unsafe.StringData(_ts), len(_ts))); _err != nil { return result, 0, _err }
	%s = _tj
}
`, posVar, ref, posVar)

		default:
			// Static analysis says: implements none of our hot paths.
			// Skip all runtime probes; go straight to encoding/json.
			return fmt.Sprintf(`{
	_start := %s
	_k, _err := scan.SkipValue(data, _start)
	if _err != nil { return result, 0, _err }
	if _err := json.Unmarshal(data[_start:_k], &%s); _err != nil { return result, 0, _err }
	%s = _k
}
`, posVar, ref, posVar)
		}
	}
	// Unresolved (AST-only path, e.g. tests with temp dirs lacking go.mod)
	// — generator can't tell what the type implements, so just emit a
	// plain encoding/json fallback. Stdlib's reflective decoder handles
	// MarshalJSON / UnmarshalText hooks on its own.
	return fmt.Sprintf(`{
	_start := %s
	_k, _err := scan.SkipValue(data, _start)
	if _err != nil { return result, 0, _err }
	if _err := json.Unmarshal(data[_start:_k], &%s); _err != nil { return result, 0, _err }
	%s = _k
}
`, posVar, ref, posVar)
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
	_b, _err := %s.MarshalJSON()
	if _err != nil { return dst, _err }
	dst = append(dst, _b...)
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
	_t, _err := %s.MarshalText()
	if _err != nil { return dst, _err }
	dst = append(dst, '"')
	dst = %s(dst, encode.BytesToString(_t))
}
`, ref, appendStrFn(f.HTMLEscape))

		default:
			return fmt.Sprintf(`{
	_b, _err := json.Marshal(%s)
	if _err != nil { return dst, _err }
	dst = append(dst, _b...)
}
`, ref)
		}
	}
	// Unresolved (AST-only) — plain encoding/json fallback.
	return fmt.Sprintf(`{
	_b, _err := json.Marshal(%s)
	if _err != nil { return dst, _err }
	dst = append(dst, _b...)
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
	_v, _k, _err := %s.DecodeStreamFrom(_s, %s)
	if _err != nil { return result, 0, _err }
	%s = _v
	%s = _k
}
`, ref, posVar, ref, posVar)

		case f.Iface.JSONUnmarshaler:
			return fmt.Sprintf(`{
	_start := %s
	_k, _err := _s.SkipValue(_start)
	if _err != nil { return result, 0, _err }
	if _err := %s.UnmarshalJSON(_s.Bytes()[_start:_k]); _err != nil { return result, 0, _err }
	%s = _k
}
`, posVar, ref, posVar)

		case f.Iface.TextUnmarshaler:
			return fmt.Sprintf(`{
	_ts, _tj, _terr := _s.String(%s)
	if _terr != nil { return result, 0, _terr }
	if _err := %s.UnmarshalText(unsafe.Slice(unsafe.StringData(_ts), len(_ts))); _err != nil { return result, 0, _err }
	%s = _tj
}
`, posVar, ref, posVar)

		default:
			return fmt.Sprintf(`{
	_start := %s
	_k, _err := _s.SkipValue(_start)
	if _err != nil { return result, 0, _err }
	if _err := json.Unmarshal(_s.Bytes()[_start:_k], &%s); _err != nil { return result, 0, _err }
	%s = _k
}
`, posVar, ref, posVar)
		}
	}
	// Unresolved (AST-only) — plain encoding/json fallback.
	return fmt.Sprintf(`{
	_start := %s
	_k, _err := _s.SkipValue(_start)
	if _err != nil { return result, 0, _err }
	if _err := json.Unmarshal(_s.Bytes()[_start:_k], &%s); _err != nil { return result, 0, _err }
	%s = _k
}
`, posVar, ref, posVar)
}

// renderRawJSON aliases data[start:end] into the field — zero copy.
// Works for both json.RawMessage and jsontext.Value because both have
// underlying type []byte.
func renderRawJSON(ref, posVar string) string {
	return fmt.Sprintf(`{
	_start := %s
	_k, err := scan.SkipValue(data, _start)
	if err != nil { return result, 0, err }
	%s = data[_start:_k]
	%s = _k
}
`, posVar, ref, posVar)
}

// renderURL parses a JSON string via url.Parse. The dereference is
// safe because Parse returns a non-nil *URL on success.
func renderURL(ref, posVar string) string {
	return fmt.Sprintf(`{
	var _s string
	%s
	_u, _err := url.Parse(_s)
	if _err != nil { return result, 0, _err }
	%s = *_u
}
`, inlineScanString(posVar, "_s", posVar), ref)
}

// renderBigInt reads a bare JSON number, hands the raw bytes to
// big.Int.SetString. The number can be arbitrarily long — no overflow.
// The literal is aliased through unsafe.String — SetString reads it left
// to right and copies the digits into its own internal storage, so the
// alias is dead by the time data could be mutated.
func renderBigInt(ref, posVar string) string {
	return fmt.Sprintf(`{
	_start := %s
	_k, err := scan.SkipValue(data, _start)
	if err != nil { return result, 0, err }
	if _, _ok := (&%s).SetString(unsafe.String(unsafe.SliceData(data[_start:]), _k-_start), 10); !_ok {
		return result, 0, scan.ErrBadNumber
	}
	%s = _k
}
`, posVar, ref, posVar)
}

// renderBigFloat reads a JSON-string-wrapped numeric literal into big.Float
// at the default precision. Wrapping matches jsonv2's wire format for
// big.Float; bare numbers are not accepted (use big.Int or float64 for those).
func renderBigFloat(ref, posVar string) string {
	return fmt.Sprintf(`{
	var _s string
	%s
	if _, _, _err := (&%s).Parse(_s, 10); _err != nil {
		return result, 0, _err
	}
}
`, inlineScanString(posVar, "_s", posVar), ref)
}

// renderBigRat reads a JSON string of the form "num" or "num/denom"
// and feeds it to big.Rat.SetString. Lossless — fractions stay exact.
func renderBigRat(ref, posVar string) string {
	return fmt.Sprintf(`{
	var _s string
	%s
	if _, _ok := (&%s).SetString(_s); !_ok {
		return result, 0, scan.ErrBadNumber
	}
}
`, inlineScanString(posVar, "_s", posVar), ref)
}

// renderSQLNull emits decode for a database/sql.NullX field. Probes for
// `null` first; on a value, parses with the inner-kind primitive and sets
// Valid=true. The outer-scope local is `_nv` (null-inner-value) — chosen
// to avoid collisions with the `_v` that the time/string sub-renderers
// declare internally.
func renderSQLNull(f FieldInfo, ref, posVar string) string {
	spec, ok := SQLNullSpec(f.GoType)
	if !ok {
		return ""
	}
	var inner strings.Builder
	switch spec.Inner {
	case KindString:
		fmt.Fprintf(&inner, "var _nv string\n%s", inlineScanString(posVar, "_nv", posVar))
	case KindBool:
		fmt.Fprintf(&inner, "_b, _bk, err := scan.Bool(data, %s)\n", posVar)
		inner.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&inner, "_nv := _b\n%s = _bk\n", posVar)
	case KindInt64:
		inner.WriteString("var _nv int64\n")
		inner.WriteString(inlineScanInt64(posVar, "_nv", ""))
	case KindInt32, KindInt16:
		fmt.Fprintf(&inner, "var _nv %s\n", spec.Type)
		inner.WriteString(inlineScanInt64(posVar, "_nv", spec.Type))
	case KindUint8:
		fmt.Fprintf(&inner, "var _nv %s\n", spec.Type)
		inner.WriteString(inlineScanUint64(posVar, "_nv", spec.Type))
	case KindFloat64:
		fmt.Fprintf(&inner, "_fv, _fk, err := scan.Float64(data, %s)\n", posVar)
		inner.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&inner, "_nv := _fv\n%s = _fk\n", posVar)
	case KindTime:
		tf := FieldInfo{Format: f.Format}
		inner.WriteString("var _nv time.Time\n")
		inner.WriteString(renderTime(tf, "_nv", posVar))
	}
	return fmt.Sprintf(`{
	if %s+4 <= len(data) && data[%s] == 'n' && data[%s+1] == 'u' && data[%s+2] == 'l' && data[%s+3] == 'l' {
		%s = sql.%s{}
		%s += 4
	} else {
		%s
		%s = sql.%s{%s: _nv, Valid: true}
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
func renderAny(f FieldInfo, ref, posVar string) string {
	fn := "scan.Any"
	if f.UseNumber {
		fn = "scan.AnyNumber"
	}
	return fmt.Sprintf(`{
	_v, _k, err := %s(data, %s)
	if err != nil { return result, 0, err }
	%s = _v
	%s = _k
}
`, fn, posVar, ref, posVar)
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
		return "var _seen uint64\n"
	}
	return fmt.Sprintf("var _seen [%d]uint64\n", seenWordCount(s))
}

// seenAccess returns the read expression for f's seen bit. In bool mode
// it's the local `seen<GoName>`; in bitmask mode it's `_seen & (1<<N)
// != 0` (or the array-indexed form for >64 fields).
func seenAccess(s StructInfo, f FieldInfo) string {
	if !useSeenBitmask(s) {
		return "seen" + f.GoName
	}
	bit := seenBitIndex(s, f)
	if seenWordCount(s) == 1 {
		return fmt.Sprintf("_seen&(1<<%d) != 0", bit)
	}
	return fmt.Sprintf("_seen[%d]&(1<<%d) != 0", bit/64, bit%64)
}

// seenNotAccess is the negated counterpart of seenAccess — true when the
// bit is unset.
func seenNotAccess(s StructInfo, f FieldInfo) string {
	if !useSeenBitmask(s) {
		return "!seen" + f.GoName
	}
	bit := seenBitIndex(s, f)
	if seenWordCount(s) == 1 {
		return fmt.Sprintf("_seen&(1<<%d) == 0", bit)
	}
	return fmt.Sprintf("_seen[%d]&(1<<%d) == 0", bit/64, bit%64)
}

// seenSet emits a statement that marks f's seen bit. Trailing newline
// is intentional so callers can concatenate freely.
func seenSet(s StructInfo, f FieldInfo) string {
	if !useSeenBitmask(s) {
		return "seen" + f.GoName + " = true\n"
	}
	bit := seenBitIndex(s, f)
	if seenWordCount(s) == 1 {
		return fmt.Sprintf("_seen |= 1<<%d\n", bit)
	}
	return fmt.Sprintf("_seen[%d] |= 1<<%d\n", bit/64, bit%64)
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
		return fmt.Sprintf(`if result.%s == nil { result.%s = make(%s) }
_start := %s
_k, err := scan.SkipValue(data, _start)
if err != nil { return result, 0, err }
var _v any
if err := json.Unmarshal(data[_start:_k], &_v); err != nil { return result, 0, err }
result.%s[key] = _v
%s = _k
`, inline.GoName, inline.GoName, inline.GoType, posVar, inline.GoName, posVar)
	}
	if s.IgnoreUnknown {
		return fmt.Sprintf(`k, err := scan.SkipValue(data, %s)
if err != nil { return result, 0, err }
%s = k
`, posVar, posVar)
	}
	if s.MultiErr {
		return fmt.Sprintf(`errs = append(errs, &validation.UnknownKeyError{Field: key})
k, err := scan.SkipValue(data, %s)
if err != nil { return result, 0, err }
%s = k
`, posVar, posVar)
	}
	return "return result, 0, &validation.UnknownKeyError{Field: key}\n"
}

// validateAndMod emits mods + validation for a field inline in the decoder body.
// Reuses renderMods / renderValidationOn; patches stop-on-first return shape
// from "(T, error)" to "(T, int, error)". When f.MultiErr is on, the reused
// code appends to an `errs` slice instead of returning, so no patch is
// needed. Skipped entirely when f.NoValidate.
func validateAndMod(f FieldInfo, ref string) string {
	var out strings.Builder
	if len(f.Mods) > 0 {
		out.WriteString(renderMods(f.Mods, ref, f.GoType, f.Kind))
	}
	if len(f.Validation) > 0 {
		code := renderValidationOn(f.Validation, ref, f.JSONName, f.Kind, f.MultiErr)
		if !f.MultiErr {
			code = strings.ReplaceAll(code, "return result, ", "return result, 0, ")
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
func renderStringTag(f FieldInfo, ref, posVar string) string {
	var b strings.Builder
	b.WriteString("{\n")
	b.WriteString("var _sv string\n")
	b.WriteString(inlineScanString(posVar, "_sv", posVar))
	switch f.Kind {
	case KindBool:
		fmt.Fprintf(&b, `switch _sv {
case "true": %s = true
case "false": %s = false
default: return result, 0, scan.ErrBadBool
}
`, ref, ref)
	case KindFloat32, KindFloat64:
		b.WriteString("_f, err := strconv.ParseFloat(_sv, 64)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		if f.Kind == KindFloat32 {
			fmt.Fprintf(&b, "%s = float32(_f)\n", ref)
		} else {
			fmt.Fprintf(&b, "%s = _f\n", ref)
		}
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		b.WriteString("_u, err := strconv.ParseUint(_sv, 10, 64)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		if f.Kind == KindUint64 {
			fmt.Fprintf(&b, "%s = _u\n", ref)
		} else {
			fmt.Fprintf(&b, "%s = %s(_u)\n", ref, f.GoType)
		}
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		b.WriteString("_n, err := strconv.ParseInt(_sv, 10, 64)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		if f.Kind == KindInt64 {
			fmt.Fprintf(&b, "%s = _n\n", ref)
		} else {
			fmt.Fprintf(&b, "%s = %s(_n)\n", ref, f.GoType)
		}
	case KindString:
		fmt.Fprintf(&b, "%s = _sv\n", ref)
	}
	b.WriteString("}\n")
	return b.String()
}

func renderField(f FieldInfo, ref, posVar string) string {
	if f.String {
		var out strings.Builder
		out.WriteString(renderStringTag(f, ref, posVar))
		if !f.NoValidate {
			out.WriteString(validateAndMod(f, ref))
		}
		return out.String()
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
		block := fmt.Sprintf(`if %s+4 <= len(data) && data[%s] == 'n' && data[%s+1] == 'u' && data[%s+2] == 'l' && data[%s+3] == 'l' {
	%s = 4 + %s
	%s = nil
} else {
	var _v %s
	%s
	%s = &_v
}
`, posVar, posVar, posVar, posVar, posVar,
			posVar, posVar,
			ref,
			inner.GoType,
			renderField(inner, "_v", posVar),
			ref)
		if !f.NoValidate && (len(customV) > 0 || len(customM) > 0) {
			outer := f
			outer.Validation = customV
			outer.Mods = customM
			block += validateAndMod(outer, ref)
		}
		return block
	}
	var b strings.Builder
	switch f.Kind {
	case KindString:
		b.WriteString(inlineScanString(posVar, ref, posVar))
	case KindBool:
		fmt.Fprintf(&b, "v, k, err := scan.Bool(data, %s)\n", posVar)
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "%s = v\n%s = k\n", ref, posVar)
	case KindInt, KindInt8, KindInt16, KindInt32:
		b.WriteString(inlineScanInt64(posVar, ref, f.GoType))
	case KindInt64:
		b.WriteString(inlineScanInt64(posVar, ref, ""))
	case KindUint, KindUint8, KindUint16, KindUint32:
		b.WriteString(inlineScanUint64(posVar, ref, f.GoType))
	case KindUint64:
		b.WriteString(inlineScanUint64(posVar, ref, ""))
	case KindFloat32:
		fmt.Fprintf(&b, "v, k, err := scan.Float64(data, %s)\n", posVar)
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "%s = float32(v)\n%s = k\n", ref, posVar)
	case KindFloat64:
		fmt.Fprintf(&b, "v, k, err := scan.Float64(data, %s)\n", posVar)
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "%s = v\n%s = k\n", ref, posVar)
	case KindStruct:
		if isGenerated(f.GoType) {
			// Receiver is the existing field — already on the stack as
			// part of `result`, no `T{}` literal (which wouldn't compile
			// for primitive aliases) and no new var needed.
			fmt.Fprintf(&b, "v, k, err := %s.DecodeFrom(data, %s)\n", ref, posVar)
			b.WriteString("if err != nil { return result, 0, err }\n")
			fmt.Fprintf(&b, "%s = v\n%s = k\n", ref, posVar)
		} else {
			b.WriteString(renderCrossPkgStructDecode(f, ref, posVar))
		}
	case KindSlice:
		b.WriteString(renderSlice(f, ref, posVar))
	case KindArray:
		b.WriteString(emitByteArrayRead(f, ref, posVar, 0))
	case KindMap:
		b.WriteString(renderMap(f, ref, posVar))
	case KindBytes:
		b.WriteString(renderBytes(f, ref, posVar))
	case KindTime:
		b.WriteString(renderTime(f, ref, posVar))
	case KindDuration:
		b.WriteString(renderDuration(f, ref, posVar))
	case KindNetIP:
		b.WriteString(renderNetIP(ref, posVar))
	case KindNetipAddr:
		b.WriteString(renderNetipAddr(ref, posVar))
	case KindNetipPrefix:
		b.WriteString(renderNetipPrefix(ref, posVar))
	case KindRawJSON:
		b.WriteString(renderRawJSON(ref, posVar))
	case KindURL:
		b.WriteString(renderURL(ref, posVar))
	case KindBigInt:
		b.WriteString(renderBigInt(ref, posVar))
	case KindBigFloat:
		b.WriteString(renderBigFloat(ref, posVar))
	case KindBigRat:
		b.WriteString(renderBigRat(ref, posVar))
	case KindSQLNull:
		b.WriteString(renderSQLNull(f, ref, posVar))
	case KindAny:
		b.WriteString(renderAny(f, ref, posVar))
	default:
		fmt.Fprintf(&b, "k, err := scan.SkipValue(data, %s)\n", posVar)
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "%s = k\n", posVar)
	}
	// Post-decode: mods then validation. The `seen<GoName>` bool is set
	// by the caller (renderDispatch emits it), not here.
	if !f.NoValidate {
		b.WriteString(validateAndMod(f, ref))
	}
	return b.String()
}

// renderSlice is the depth-0 entry point into the recursive slice emitter.
// See emitByteSliceRead for the bulk of the work.
func renderSlice(f FieldInfo, ref, posVar string) string {
	return emitByteSliceRead(f, ref, posVar, 0)
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
func emitByteArrayRead(f FieldInfo, dst, posVar string, depth int) string {
	return emitByteSliceRead(f, dst, posVar, depth)
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
func emitByteSliceRead(f FieldInfo, dst, posVar string, depth int) string {
	isArray := f.Kind == KindArray
	arrayN := f.ArrayLen
	kvar := fmt.Sprintf("k%d", depth)
	evvar := fmt.Sprintf("ev%d", depth)
	ivar := fmt.Sprintf("_idx%d", depth)

	var b strings.Builder
	b.WriteString("{\n")
	fmt.Fprintf(&b, "%s := %s\n", kvar, posVar)
	b.WriteString(inlineSkipWS(kvar))
	// `null` → leave slice nil and consume the literal. Arrays don't accept
	// null (no nil array values in Go); they still error on non-`[` input.
	if !isArray {
		fmt.Fprintf(&b, "if _np, _ok := scan.Null(data, %s); _ok {\n%s = _np\n} else {\n", kvar, posVar)
	}
	fmt.Fprintf(&b, "if %s >= len(data) || data[%s] != '[' { return result, 0, scan.ErrBadArray }\n", kvar, kvar)
	fmt.Fprintf(&b, "%s++\n", kvar)
	b.WriteString(inlineSkipWS(kvar))
	slabVar := fmt.Sprintf("_slab%d", depth)
	if isArray {
		fmt.Fprintf(&b, "var %s int\n", ivar)
		// For arrays of pointers, we still need a slab to back the
		// fixed-length array's element pointers.
		if f.ElemPointer {
			fmt.Fprintf(&b, "var %s [%d]%s\n", slabVar, arrayN, f.ElemType)
		}
	} else {
		sCap, slCap := preallocCap(f)
		// Empty `[]` → non-nil empty slice (stdlib parity); non-empty
		// → fresh make() with prealloc. `null` is consumed earlier and
		// leaves dst nil. `dst` is always zero (the surrounding
		// DecodeFrom's `var result T` builds fresh) so there is no
		// reuse branch to emit. Slab declared outside the branch so
		// the loop body below sees it.
		if f.ElemPointer {
			fmt.Fprintf(&b, "var %s []%s\n", slabVar, f.ElemType)
		}
		fmt.Fprintf(&b, "if %s < len(data) && data[%s] == ']' {\n", kvar, kvar)
		fmt.Fprintf(&b, "%s = %s{}\n", dst, f.GoType)
		fmt.Fprintf(&b, "} else {\n")
		if sCap > 0 {
			fmt.Fprintf(&b, "%s = make(%s, 0, %d)\n", dst, f.GoType, sCap)
		} else {
			fmt.Fprintf(&b, "%s = %s{}\n", dst, f.GoType)
		}
		if f.ElemPointer {
			fmt.Fprintf(&b, "%s = make([]%s, 0, %d)\n", slabVar, f.ElemType, slCap)
		}
		fmt.Fprintf(&b, "}\n")
	}
	fmt.Fprintf(&b, "for %s < len(data) && data[%s] != ']' {\n", kvar, kvar)
	if isArray {
		// Strict tuple: reject when the JSON array has more elements than
		// the Go [N]T can hold.
		fmt.Fprintf(&b, "if %s >= %d { return result, 0, %s }\n",
			ivar, arrayN,
			arrayLenErr(f.JSONName, arrayN, ivar))
	}
	if f.ElemPointer {
		// `null` element → nil pointer. Skip the parse + slab work.
		fmt.Fprintf(&b, "if _np, _ok := scan.Null(data, %s); _ok {\n", kvar)
		fmt.Fprintf(&b, "%s = _np\n", kvar)
		if isArray {
			fmt.Fprintf(&b, "%s[%s] = nil\n", dst, ivar)
			fmt.Fprintf(&b, "%s++\n", ivar)
		} else {
			fmt.Fprintf(&b, "%s = append(%s, nil)\n", dst, dst)
		}
		b.WriteString(inlineSkipWS(kvar))
		fmt.Fprintf(&b, "if %s < len(data) && data[%s] == ',' { %s++; ", kvar, kvar, kvar)
		b.WriteString(inlineSkipWS(kvar))
		b.WriteString("continue }\nbreak\n}\n")
	}
	fmt.Fprintf(&b, "var %s %s\n", evvar, f.ElemType)
	switch f.ElemKind {
	case KindString:
		b.WriteString(inlineScanString(kvar, evvar, kvar))
	case KindBool:
		fmt.Fprintf(&b, "_bv, _ek, err := scan.Bool(data, %s)\n", kvar)
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "%s = _bv\n%s = _ek\n", evvar, kvar)
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		b.WriteString("var _ev int64\n")
		b.WriteString(inlineScanInt64(kvar, "_ev", ""))
		if f.ElemType == "int64" {
			fmt.Fprintf(&b, "%s = _ev\n", evvar)
		} else {
			fmt.Fprintf(&b, "%s = %s(_ev)\n", evvar, f.ElemType)
		}
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		b.WriteString("var _ev uint64\n")
		b.WriteString(inlineScanUint64(kvar, "_ev", ""))
		if f.ElemType == "uint64" {
			fmt.Fprintf(&b, "%s = _ev\n", evvar)
		} else {
			fmt.Fprintf(&b, "%s = %s(_ev)\n", evvar, f.ElemType)
		}
	case KindFloat32, KindFloat64:
		fmt.Fprintf(&b, "_fv, _ek, err := scan.Float64(data, %s)\n", kvar)
		b.WriteString("if err != nil { return result, 0, err }\n")
		if f.ElemKind == KindFloat32 {
			fmt.Fprintf(&b, "%s = float32(_fv)\n", evvar)
		} else {
			fmt.Fprintf(&b, "%s = _fv\n", evvar)
		}
		fmt.Fprintf(&b, "%s = _ek\n", kvar)
	case KindStruct:
		if isGenerated(f.ElemType) {
			fmt.Fprintf(&b, "var _z %s\n_sv, _ek, err := _z.DecodeFrom(data, %s)\n", f.ElemType, kvar)
			b.WriteString("if err != nil { return result, 0, err }\n")
			fmt.Fprintf(&b, "%s = _sv\n%s = _ek\n", evvar, kvar)
		} else {
			fmt.Fprintf(&b, "_ek, err := scan.SkipValue(data, %s)\n", kvar)
			b.WriteString("if err != nil { return result, 0, err }\n")
			fmt.Fprintf(&b, "%s = _ek\n", kvar)
		}
	case KindSlice, KindArray:
		// Nested container — recurse, peeling one outer [] / [N] off.
		b.WriteString(emitByteSliceRead(peelSliceField(f), evvar, kvar, depth+1))
	}
	if len(f.ElemMods) > 0 {
		b.WriteString(renderMods(f.ElemMods, evvar, f.ElemType, f.ElemKind))
	}
	if len(f.ElemValidation) > 0 {
		code := renderValidationOn(f.ElemValidation, evvar, f.JSONName+"[]", f.ElemKind, f.MultiErr)
		if !f.MultiErr {
			code = strings.ReplaceAll(code, "return result, ", "return result, 0, ")
		}
		b.WriteString(code)
	}
	if isArray {
		if f.ElemPointer {
			// Stash element value in slab[i], pointer goes into the array.
			fmt.Fprintf(&b, "%s[%s] = %s\n", slabVar, ivar, evvar)
			fmt.Fprintf(&b, "%s[%s] = &%s[%s]\n", dst, ivar, slabVar, ivar)
		} else {
			fmt.Fprintf(&b, "%s[%s] = %s\n", dst, ivar, evvar)
		}
		fmt.Fprintf(&b, "%s++\n", ivar)
	} else {
		if f.ElemPointer {
			// Append to the slab, then take an interior pointer to the
			// freshly-added element. If the slab grows, prior pointers
			// keep the orphan backing alive — semantically correct, just
			// uses ~2× the slab memory in the worst case.
			fmt.Fprintf(&b, "%s = append(%s, %s)\n", slabVar, slabVar, evvar)
			fmt.Fprintf(&b, "%s = append(%s, &%s[len(%s)-1])\n", dst, dst, slabVar, slabVar)
		} else {
			fmt.Fprintf(&b, "%s = append(%s, %s)\n", dst, dst, evvar)
		}
	}
	b.WriteString(inlineSkipWS(kvar))
	fmt.Fprintf(&b, "if %s < len(data) && data[%s] == ',' { %s++; ", kvar, kvar, kvar)
	b.WriteString(inlineSkipWS(kvar))
	b.WriteString("continue }\n")
	b.WriteString("break\n")
	b.WriteString("}\n")
	fmt.Fprintf(&b, "if %s >= len(data) || data[%s] != ']' { return result, 0, scan.ErrBadArray }\n", kvar, kvar)
	if isArray {
		fmt.Fprintf(&b, "if %s != %d { return result, 0, %s }\n",
			ivar, arrayN,
			arrayLenErr(f.JSONName, arrayN, ivar))
	}
	fmt.Fprintf(&b, "%s = %s + 1\n", posVar, kvar)
	if !isArray {
		b.WriteString("}\n") // close else (null-check)
	}
	b.WriteString("}\n") // close outer block
	return b.String()
}

// renderStreamDecode emits the streaming counterpart of renderDecode.
// Uses scan.Stream methods which pull more bytes on demand. Buffer backing
// array is fixed-capacity and never reallocates — zero-copy string aliases
// stay valid for the lifetime of the Stream.
func renderStreamDecode(s StructInfo) string {
	if s.IsAlias {
		return renderAliasStreamDecode(s)
	}
	return renderStreamDecodeStruct(s)
}

func renderStreamDecodeStruct(s StructInfo) string {
	var b strings.Builder
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
				fmt.Fprintf(&b, "seen%s := false\n", f.GoName)
			}
		}
	}
	b.WriteString("i, err := _s.ObjectOpen(i)\n")
	b.WriteString("if err != nil { return result, 0, err }\n")
	b.WriteString("i, err = _s.SkipSpace(i)\n")
	b.WriteString("if err != nil { return result, 0, err }\n")
	b.WriteString("if i >= len(_s.Bytes()) { if err = _s.ReadMore(); err != nil { return result, 0, err } }\n")
	b.WriteString("if _s.Bytes()[i] == '}' {\n")
	b.WriteString(renderPostLoop(s))
	b.WriteString("return result, i + 1, nil\n}\n")
	b.WriteString("for {\n")
	b.WriteString("key, j, err := _s.KeyView(i)\n")
	b.WriteString("if err != nil { return result, 0, err }\n")
	b.WriteString("j, err = _s.SkipSpace(j)\n")
	b.WriteString("if err != nil { return result, 0, err }\n")
	b.WriteString("if j >= len(_s.Bytes()) { if err = _s.ReadMore(); err != nil { return result, 0, err } }\n")
	b.WriteString("if _s.Bytes()[j] != ':' { return result, 0, scan.ErrBadObject }\n")
	b.WriteString("j, err = _s.SkipSpace(j + 1)\n")
	b.WriteString("if err != nil { return result, 0, err }\n")
	b.WriteString(renderStreamDispatch(s))
	b.WriteString("j, err = _s.SkipSpace(j)\n")
	b.WriteString("if err != nil { return result, 0, err }\n")
	b.WriteString("if j >= len(_s.Bytes()) { if err = _s.ReadMore(); err != nil { return result, 0, err } }\n")
	b.WriteString("c := _s.Bytes()[j]\n")
	b.WriteString("if c == ',' { i = j + 1; ")
	b.WriteString("i, err = _s.SkipSpace(i)\n")
	b.WriteString("if err != nil { return result, 0, err }\n")
	b.WriteString("continue }\n")
	b.WriteString("if c == '}' {\n")
	b.WriteString(renderPostLoop(s))
	b.WriteString("return result, j + 1, nil\n}\n")
	b.WriteString("return result, 0, scan.ErrBadObject\n")
	b.WriteString("}\n")
	return b.String()
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

	emitField := func(b *strings.Builder, f FieldInfo, parse string) {
		if f.Inline || !needsSeen(f) {
			b.WriteString(parse)
			return
		}
		set := seenSet(s, f)
		seen := seenAccess(s, f)
		if s.AllowDups {
			fmt.Fprintf(b, `if %s {
	_skipJ, _skipErr := _s.SkipValue(j)
	if _skipErr != nil { return result, 0, _skipErr }
	j = _skipJ
} else {
	%s%s
}
`, seen, set, parse)
			return
		}
		if s.MultiErr {
			fmt.Fprintf(b, `if %s {
	errs = append(errs, &validation.DuplicateKeyError{Field: %q})
	_skipJ, _skipErr := _s.SkipValue(j)
	if _skipErr != nil { return result, 0, _skipErr }
	j = _skipJ
} else {
	%s%s
}
`, seen, f.JSONName, set, parse)
			return
		}
		fmt.Fprintf(b, `if %s { return result, 0, &validation.DuplicateKeyError{Field: %q} }
%s%s`, seen, f.JSONName, set, parse)
	}

	var b strings.Builder
	b.WriteString("switch len(key) {\n")
	for _, n := range lens {
		fs := byLen[n]
		fmt.Fprintf(&b, "case %d:\n", n)
		if len(fs) == 1 {
			f := fs[0]
			fmt.Fprintf(&b, "if key == %q {\n", f.JSONName)
			emitField(&b, f, renderStreamField(f, "result."+f.GoName, "j"))
			b.WriteString("} else {\n")
			b.WriteString(streamUnknownKey(s, "j"))
			b.WriteString("}\n")
			continue
		}
		b.WriteString("switch key {\n")
		for _, f := range fs {
			fmt.Fprintf(&b, "case %q:\n", f.JSONName)
			emitField(&b, f, renderStreamField(f, "result."+f.GoName, "j"))
		}
		b.WriteString("default:\n")
		b.WriteString(streamUnknownKey(s, "j"))
		b.WriteString("}\n")
	}
	b.WriteString("default:\n")
	b.WriteString(streamUnknownKey(s, "j"))
	b.WriteString("}\n")
	return b.String()
}

// renderStreamMap emits map decode for the stream path.
func renderStreamMap(f FieldInfo, ref, posVar string) string {
	var b strings.Builder
	b.WriteString("{\n")
	// `null` → leave nil and consume the literal. Probe via the buffered
	// span; the surrounding ObjectOpen would otherwise error on the `n`.
	fmt.Fprintf(&b, "_k0, err := _s.SkipSpace(%s)\n", posVar)
	b.WriteString("if err != nil { return result, 0, err }\n")
	b.WriteString("if _k0 >= len(_s.Bytes()) { if err = _s.ReadMore(); err != nil { return result, 0, err } }\n")
	b.WriteString("if _s.Bytes()[_k0] == 'n' {\n")
	b.WriteString("for _ki := 1; _ki < 4; _ki++ {\n")
	b.WriteString("if _k0+_ki >= len(_s.Bytes()) { if err = _s.ReadMore(); err != nil { return result, 0, err } }\n")
	b.WriteString("if _s.Bytes()[_k0+_ki] != \"null\"[_ki] { return result, 0, scan.ErrBadLiteral }\n")
	b.WriteString("}\n")
	fmt.Fprintf(&b, "%s = _k0 + 4\n", posVar)
	b.WriteString("} else {\n")
	fmt.Fprintf(&b, "k, err := _s.ObjectOpen(_k0)\n")
	b.WriteString("if err != nil { return result, 0, err }\n")
	b.WriteString("k, err = _s.SkipSpace(k)\n")
	b.WriteString("if err != nil { return result, 0, err }\n")
	b.WriteString("if k >= len(_s.Bytes()) { if err = _s.ReadMore(); err != nil { return result, 0, err } }\n")
	// Empty `{}` → non-nil empty; else fresh make(). See renderMap.
	fmt.Fprintf(&b, "if _s.Bytes()[k] == '}' {\n")
	fmt.Fprintf(&b, "%s = %s{}\n", ref, f.GoType)
	fmt.Fprintf(&b, "} else {\n")
	if cap := mapPreallocCap(f); cap > 0 {
		fmt.Fprintf(&b, "%s = make(%s, %d)\n", ref, f.GoType, cap)
	} else {
		fmt.Fprintf(&b, "%s = make(%s)\n", ref, f.GoType)
	}
	fmt.Fprintf(&b, "}\n")
	b.WriteString("for _s.Bytes()[k] != '}' {\n")
	b.WriteString("_mk, _k2, err := _s.String(k)\n")
	b.WriteString("if err != nil { return result, 0, err }\n")
	b.WriteString(keyValidateAndMod(f, "_mk"))
	b.WriteString("k, err = _s.SkipSpace(_k2)\n")
	b.WriteString("if err != nil { return result, 0, err }\n")
	b.WriteString("if k >= len(_s.Bytes()) { if err = _s.ReadMore(); err != nil { return result, 0, err } }\n")
	b.WriteString("if _s.Bytes()[k] != ':' { return result, 0, scan.ErrBadObject }\n")
	b.WriteString("k, err = _s.SkipSpace(k + 1)\n")
	b.WriteString("if err != nil { return result, 0, err }\n")
	switch f.ElemKind {
	case KindString:
		b.WriteString("_mv, _k3, err := _s.String(k)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "%s[_mk] = _mv\nk = _k3\n", ref)
	case KindBool:
		b.WriteString("_mv, _k3, err := _s.Bool(k)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "%s[_mk] = _mv\nk = _k3\n", ref)
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		b.WriteString("_mn, _k3, err := _s.Int64(k)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		if f.ElemType == "int64" {
			fmt.Fprintf(&b, "%s[_mk] = _mn\n", ref)
		} else {
			fmt.Fprintf(&b, "%s[_mk] = %s(_mn)\n", ref, f.ElemType)
		}
		b.WriteString("k = _k3\n")
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		b.WriteString("_mn, _k3, err := _s.Uint64(k)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		if f.ElemType == "uint64" {
			fmt.Fprintf(&b, "%s[_mk] = _mn\n", ref)
		} else {
			fmt.Fprintf(&b, "%s[_mk] = %s(_mn)\n", ref, f.ElemType)
		}
		b.WriteString("k = _k3\n")
	case KindFloat32, KindFloat64:
		b.WriteString("_mv, _k3, err := _s.Float64(k)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		if f.ElemKind == KindFloat32 {
			fmt.Fprintf(&b, "%s[_mk] = float32(_mv)\n", ref)
		} else {
			fmt.Fprintf(&b, "%s[_mk] = _mv\n", ref)
		}
		b.WriteString("k = _k3\n")
	case KindStruct:
		if isGenerated(f.ElemType) {
			fmt.Fprintf(&b, "var _z %s\n_mv, _k3, err := _z.DecodeStreamFrom(_s, k)\n", f.ElemType)
			b.WriteString("if err != nil { return result, 0, err }\n")
			fmt.Fprintf(&b, "%s[_mk] = _mv\nk = _k3\n", ref)
		} else {
			fmt.Fprintf(&b, `_start := k
_k3, err := _s.SkipValue(_start)
if err != nil { return result, 0, err }
var _mv %s
if err := json.Unmarshal(_s.Bytes()[_start:_k3], &_mv); err != nil { return result, 0, err }
%s[_mk] = _mv
k = _k3
`, f.ElemType, ref)
		}
	default:
		b.WriteString("_k3, err := _s.SkipValue(k)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		b.WriteString("k = _k3\n")
	}
	if len(f.ElemMods) > 0 {
		patched := strings.ReplaceAll(renderMods(f.ElemMods, "_mvx", f.ElemType, f.ElemKind), "_mvx", fmt.Sprintf("%s[_mk]", ref))
		b.WriteString(patched)
	}
	if len(f.ElemValidation) > 0 {
		code := renderValidationOn(f.ElemValidation, fmt.Sprintf("%s[_mk]", ref), f.JSONName+".value", f.ElemKind, f.MultiErr)
		code = strings.ReplaceAll(code, "return result, &validation.", "return result, 0, &validation.")
		b.WriteString(code)
	}
	b.WriteString("k, err = _s.SkipSpace(k)\n")
	b.WriteString("if err != nil { return result, 0, err }\n")
	b.WriteString("if k >= len(_s.Bytes()) { if err = _s.ReadMore(); err != nil { return result, 0, err } }\n")
	b.WriteString("if _s.Bytes()[k] == ',' { k, err = _s.SkipSpace(k + 1); if err != nil { return result, 0, err }; continue }\n")
	b.WriteString("break\n")
	b.WriteString("}\n")
	b.WriteString("if _s.Bytes()[k] != '}' { return result, 0, scan.ErrBadObject }\n")
	fmt.Fprintf(&b, "%s = k + 1\n", posVar)
	b.WriteString("}\n") // close else (null-check)
	b.WriteString("}\n") // close outer block
	return b.String()
}

// --- stream native-type renderers ---

func renderStreamBytes(f FieldInfo, ref, posVar string) string {
	if f.Format == "array" {
		return fmt.Sprintf(`{
	k, err := _s.ArrayOpen(%s)
	if err != nil { return result, 0, err }
	k, err = _s.SkipSpace(k)
	if err != nil { return result, 0, err }
	if k >= len(_s.Bytes()) { if err = _s.ReadMore(); err != nil { return result, 0, err } }
	for _s.Bytes()[k] != ']' {
		_v, _k, err := _s.Uint64(k)
		if err != nil { return result, 0, err }
		%s = append(%s, byte(_v))
		k, err = _s.SkipSpace(_k)
		if err != nil { return result, 0, err }
		if k >= len(_s.Bytes()) { if err = _s.ReadMore(); err != nil { return result, 0, err } }
		if _s.Bytes()[k] == ',' { k, err = _s.SkipSpace(k + 1); if err != nil { return result, 0, err }; continue }
		break
	}
	if _s.Bytes()[k] != ']' { return result, 0, scan.ErrBadArray }
	%s = k + 1
}
`, posVar, ref, ref, posVar)
	}
	parser := "base64.StdEncoding.DecodeString"
	switch f.Format {
	case "base64url":
		parser = "base64.URLEncoding.DecodeString"
	case "base32":
		parser = "base32.StdEncoding.DecodeString"
	case "base32hex":
		parser = "base32.HexEncoding.DecodeString"
	case "base16", "hex":
		parser = "hex.DecodeString"
	}
	return fmt.Sprintf(`{
	_v, _k, err := _s.String(%s)
	if err != nil { return result, 0, err }
	%s, err = %s(_v)
	if err != nil { return result, 0, err }
	%s = _k
}
`, posVar, ref, parser, posVar)
}

func renderStreamTime(f FieldInfo, ref, posVar string) string {
	layout, numeric := timeLayoutExpr(f.Format)
	if numeric != "" {
		ctor := map[string]string{
			"Unix":      "time.Unix(_n, 0)",
			"UnixMilli": "time.UnixMilli(_n)",
			"UnixMicro": "time.UnixMicro(_n)",
			"UnixNano":  "time.Unix(0, _n)",
		}[numeric]
		return fmt.Sprintf(`{
	_n, _k, err := _s.Int64(%s)
	if err != nil { return result, 0, err }
	%s = %s
	%s = _k
}
`, posVar, ref, ctor, posVar)
	}
	return fmt.Sprintf(`{
	_v, _k, err := _s.String(%s)
	if err != nil { return result, 0, err }
	%s, err = time.Parse(%s, _v)
	if err != nil { return result, 0, err }
	%s = _k
}
`, posVar, ref, layout, posVar)
}

func renderStreamDuration(f FieldInfo, ref, posVar string) string {
	switch f.Format {
	case "sec":
		return fmt.Sprintf(`{
	_v, _k, err := _s.Float64(%s)
	if err != nil { return result, 0, err }
	%s = time.Duration(_v * float64(time.Second))
	%s = _k
}
`, posVar, ref, posVar)
	case "milli", "micro", "nano":
		unit := map[string]string{
			"milli": "time.Millisecond",
			"micro": "time.Microsecond",
			"nano":  "time.Nanosecond",
		}[f.Format]
		return fmt.Sprintf(`{
	_n, _k, err := _s.Int64(%s)
	if err != nil { return result, 0, err }
	%s = time.Duration(_n) * %s
	%s = _k
}
`, posVar, ref, unit, posVar)
	}
	return fmt.Sprintf(`{
	_v, _k, err := _s.String(%s)
	if err != nil { return result, 0, err }
	%s, err = time.ParseDuration(_v)
	if err != nil { return result, 0, err }
	%s = _k
}
`, posVar, ref, posVar)
}

func renderStreamNetIP(ref, posVar string) string {
	return fmt.Sprintf(`{
	_v, _k, err := _s.String(%s)
	if err != nil { return result, 0, err }
	%s = net.ParseIP(_v)
	if %s == nil { return result, 0, fmt.Errorf("invalid IP") }
	%s = _k
}
`, posVar, ref, ref, posVar)
}

func renderStreamNetipAddr(ref, posVar string) string {
	return fmt.Sprintf(`{
	_v, _k, err := _s.String(%s)
	if err != nil { return result, 0, err }
	%s, err = netip.ParseAddr(_v)
	if err != nil { return result, 0, err }
	%s = _k
}
`, posVar, ref, posVar)
}

func renderStreamNetipPrefix(ref, posVar string) string {
	return fmt.Sprintf(`{
	_v, _k, err := _s.String(%s)
	if err != nil { return result, 0, err }
	%s, err = netip.ParsePrefix(_v)
	if err != nil { return result, 0, err }
	%s = _k
}
`, posVar, ref, posVar)
}

// renderStreamRawJSON copies the stream's buffer span into the
// field. ReadMore never shifts, so _start stays valid against the
// buffer for the duration of SkipValue.
func renderStreamRawJSON(ref, posVar string) string {
	return fmt.Sprintf(`{
	_start := %s
	_k, err := _s.SkipValue(_start)
	if err != nil { return result, 0, err }
	_raw := _s.Bytes()[_start:_k]
	%s = append(make([]byte, 0, len(_raw)), _raw...)
	%s = _k
}
`, posVar, ref, posVar)
}

func renderStreamURL(ref, posVar string) string {
	return fmt.Sprintf(`{
	_v, _k, err := _s.String(%s)
	if err != nil { return result, 0, err }
	_u, _err := url.Parse(_v)
	if _err != nil { return result, 0, _err }
	%s = *_u
	%s = _k
}
`, posVar, ref, posVar)
}

func renderStreamBigInt(ref, posVar string) string {
	return fmt.Sprintf(`{
	_start := %s
	_k, err := _s.SkipValue(_start)
	if err != nil { return result, 0, err }
	_buf := _s.Bytes()
	if _, _ok := (&%s).SetString(unsafe.String(unsafe.SliceData(_buf[_start:]), _k-_start), 10); !_ok {
		return result, 0, scan.ErrBadNumber
	}
	%s = _k
}
`, posVar, ref, posVar)
}

func renderStreamBigFloat(ref, posVar string) string {
	return fmt.Sprintf(`{
	_v, _k, err := _s.String(%s)
	if err != nil { return result, 0, err }
	if _, _, _err := (&%s).Parse(_v, 10); _err != nil {
		return result, 0, _err
	}
	%s = _k
}
`, posVar, ref, posVar)
}

func renderStreamBigRat(ref, posVar string) string {
	return fmt.Sprintf(`{
	_v, _k, err := _s.String(%s)
	if err != nil { return result, 0, err }
	if _, _ok := (&%s).SetString(_v); !_ok {
		return result, 0, scan.ErrBadNumber
	}
	%s = _k
}
`, posVar, ref, posVar)
}

// renderStreamSQLNull is the streaming counterpart of renderSQLNull.
func renderStreamSQLNull(f FieldInfo, ref, posVar string) string {
	spec, ok := SQLNullSpec(f.GoType)
	if !ok {
		return ""
	}
	var inner strings.Builder
	switch spec.Inner {
	case KindString:
		fmt.Fprintf(&inner, "_sv, _sk, err := _s.String(%s)\n", posVar)
		inner.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&inner, "_nv := _sv\n%s = _sk\n", posVar)
	case KindBool:
		fmt.Fprintf(&inner, "_bv, _bk, err := _s.Bool(%s)\n", posVar)
		inner.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&inner, "_nv := _bv\n%s = _bk\n", posVar)
	case KindInt64, KindInt32, KindInt16:
		fmt.Fprintf(&inner, "_iv, _ik, err := _s.Int64(%s)\n", posVar)
		inner.WriteString("if err != nil { return result, 0, err }\n")
		if spec.Type == "int64" {
			inner.WriteString("_nv := _iv\n")
		} else {
			fmt.Fprintf(&inner, "_nv := %s(_iv)\n", spec.Type)
		}
		fmt.Fprintf(&inner, "%s = _ik\n", posVar)
	case KindUint8:
		fmt.Fprintf(&inner, "_uv, _uk, err := _s.Uint64(%s)\n", posVar)
		inner.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&inner, "_nv := %s(_uv)\n%s = _uk\n", spec.Type, posVar)
	case KindFloat64:
		fmt.Fprintf(&inner, "_fv, _fk, err := _s.Float64(%s)\n", posVar)
		inner.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&inner, "_nv := _fv\n%s = _fk\n", posVar)
	case KindTime:
		tf := FieldInfo{Format: f.Format}
		inner.WriteString("var _nv time.Time\n")
		inner.WriteString(renderStreamTime(tf, "_nv", posVar))
	}
	return fmt.Sprintf(`{
	if %s >= len(_s.Bytes()) { if err = _s.ReadMore(); err != nil { return result, 0, err } }
	if _s.Bytes()[%s] == 'n' {
		for _ki := 1; _ki < 4; _ki++ {
			if %s+_ki >= len(_s.Bytes()) { if err = _s.ReadMore(); err != nil { return result, 0, err } }
			if _s.Bytes()[%s+_ki] != "null"[_ki] { return result, 0, scan.ErrBadLiteral }
		}
		%s = sql.%s{}
		%s += 4
	} else {
		%s
		%s = sql.%s{%s: _nv, Valid: true}
	}
}
`, posVar, posVar, posVar, posVar,
		ref, sqlTypeName(f.GoType), posVar,
		inner.String(),
		ref, sqlTypeName(f.GoType), spec.Field)
}

func renderStreamAny(f FieldInfo, ref, posVar string) string {
	fn := "_s.Any"
	if f.UseNumber {
		fn = "_s.AnyNumber"
	}
	return fmt.Sprintf(`{
	_v, _k, err := %s(%s)
	if err != nil { return result, 0, err }
	%s = _v
	%s = _k
}
`, fn, posVar, ref, posVar)
}

// streamUnknownKey is the stream-scanner counterpart of unknownKey.
func streamUnknownKey(s StructInfo, posVar string) string {
	if inline := s.InlineField(); inline.Inline {
		return fmt.Sprintf(`if result.%s == nil { result.%s = make(%s) }
_start := %s
_k, err := _s.SkipValue(_start)
if err != nil { return result, 0, err }
var _v any
if err := json.Unmarshal(_s.Bytes()[_start:_k], &_v); err != nil { return result, 0, err }
result.%s[key] = _v
%s = _k
`, inline.GoName, inline.GoName, inline.GoType, posVar, inline.GoName, posVar)
	}
	if s.IgnoreUnknown {
		return fmt.Sprintf(`k, err := _s.SkipValue(%s)
if err != nil { return result, 0, err }
%s = k
`, posVar, posVar)
	}
	if s.MultiErr {
		return fmt.Sprintf(`errs = append(errs, &validation.UnknownKeyError{Field: key})
k, err := _s.SkipValue(%s)
if err != nil { return result, 0, err }
%s = k
`, posVar, posVar)
	}
	return "return result, 0, &validation.UnknownKeyError{Field: key}\n"
}

func renderStreamStringTag(f FieldInfo, ref, posVar string) string {
	var b strings.Builder
	b.WriteString("{\n")
	fmt.Fprintf(&b, "_sv, _k, err := _s.String(%s)\n", posVar)
	b.WriteString("if err != nil { return result, 0, err }\n")
	switch f.Kind {
	case KindBool:
		fmt.Fprintf(&b, `switch _sv {
case "true": %s = true
case "false": %s = false
default: return result, 0, scan.ErrBadBool
}
`, ref, ref)
	case KindFloat32, KindFloat64:
		b.WriteString("_f, err := strconv.ParseFloat(_sv, 64)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		if f.Kind == KindFloat32 {
			fmt.Fprintf(&b, "%s = float32(_f)\n", ref)
		} else {
			fmt.Fprintf(&b, "%s = _f\n", ref)
		}
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		b.WriteString("_u, err := strconv.ParseUint(_sv, 10, 64)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		if f.Kind == KindUint64 {
			fmt.Fprintf(&b, "%s = _u\n", ref)
		} else {
			fmt.Fprintf(&b, "%s = %s(_u)\n", ref, f.GoType)
		}
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		b.WriteString("_n, err := strconv.ParseInt(_sv, 10, 64)\n")
		b.WriteString("if err != nil { return result, 0, err }\n")
		if f.Kind == KindInt64 {
			fmt.Fprintf(&b, "%s = _n\n", ref)
		} else {
			fmt.Fprintf(&b, "%s = %s(_n)\n", ref, f.GoType)
		}
	case KindString:
		fmt.Fprintf(&b, "%s = _sv\n", ref)
	}
	fmt.Fprintf(&b, "%s = _k\n", posVar)
	b.WriteString("}\n")
	return b.String()
}

func renderStreamField(f FieldInfo, ref, posVar string) string {
	if f.String {
		var out strings.Builder
		out.WriteString(renderStreamStringTag(f, ref, posVar))
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
		block := fmt.Sprintf(`if %s >= len(_s.Bytes()) { if err = _s.ReadMore(); err != nil { return result, 0, err } }
if _s.Bytes()[%s] == 'n' {
	for _ki := 1; _ki < 4; _ki++ {
		if %s+_ki >= len(_s.Bytes()) { if err = _s.ReadMore(); err != nil { return result, 0, err } }
		if _s.Bytes()[%s+_ki] != "null"[_ki] { return result, 0, scan.ErrBadLiteral }
	}
	%s = 4 + %s
	%s = nil
} else {
	var _v %s
	%s
	%s = &_v
}
`, posVar,
			posVar, posVar, posVar,
			posVar, posVar,
			ref,
			inner.GoType,
			renderStreamField(inner, "_v", posVar),
			ref)
		if !f.NoValidate && (len(customV) > 0 || len(customM) > 0) {
			outer := f
			outer.Validation = customV
			outer.Mods = customM
			block += validateAndMod(outer, ref)
		}
		return block
	}
	var b strings.Builder
	switch f.Kind {
	case KindString:
		fmt.Fprintf(&b, "v, k, err := _s.String(%s)\n", posVar)
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "%s = v\n%s = k\n", ref, posVar)
	case KindBool:
		fmt.Fprintf(&b, "v, k, err := _s.Bool(%s)\n", posVar)
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "%s = v\n%s = k\n", ref, posVar)
	case KindInt, KindInt8, KindInt16, KindInt32:
		fmt.Fprintf(&b, "v, k, err := _s.Int64(%s)\n", posVar)
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "%s = %s(v)\n%s = k\n", ref, f.GoType, posVar)
	case KindInt64:
		fmt.Fprintf(&b, "v, k, err := _s.Int64(%s)\n", posVar)
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "%s = v\n%s = k\n", ref, posVar)
	case KindUint, KindUint8, KindUint16, KindUint32:
		fmt.Fprintf(&b, "v, k, err := _s.Uint64(%s)\n", posVar)
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "%s = %s(v)\n%s = k\n", ref, f.GoType, posVar)
	case KindUint64:
		fmt.Fprintf(&b, "v, k, err := _s.Uint64(%s)\n", posVar)
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "%s = v\n%s = k\n", ref, posVar)
	case KindFloat32:
		fmt.Fprintf(&b, "v, k, err := _s.Float64(%s)\n", posVar)
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "%s = float32(v)\n%s = k\n", ref, posVar)
	case KindFloat64:
		fmt.Fprintf(&b, "v, k, err := _s.Float64(%s)\n", posVar)
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "%s = v\n%s = k\n", ref, posVar)
	case KindStruct:
		if isGenerated(f.GoType) {
			fmt.Fprintf(&b, "v, k, err := %s.DecodeStreamFrom(_s, %s)\n", ref, posVar)
			b.WriteString("if err != nil { return result, 0, err }\n")
			fmt.Fprintf(&b, "%s = v\n%s = k\n", ref, posVar)
		} else {
			b.WriteString(renderCrossPkgStructStreamDecode(f, ref, posVar))
		}
	case KindSlice:
		b.WriteString(renderStreamSlice(f, ref, posVar))
	case KindArray:
		b.WriteString(emitStreamSliceRead(f, ref, posVar, 0))
	case KindMap:
		b.WriteString(renderStreamMap(f, ref, posVar))
	case KindBytes:
		b.WriteString(renderStreamBytes(f, ref, posVar))
	case KindTime:
		b.WriteString(renderStreamTime(f, ref, posVar))
	case KindDuration:
		b.WriteString(renderStreamDuration(f, ref, posVar))
	case KindNetIP:
		b.WriteString(renderStreamNetIP(ref, posVar))
	case KindNetipAddr:
		b.WriteString(renderStreamNetipAddr(ref, posVar))
	case KindNetipPrefix:
		b.WriteString(renderStreamNetipPrefix(ref, posVar))
	case KindRawJSON:
		b.WriteString(renderStreamRawJSON(ref, posVar))
	case KindURL:
		b.WriteString(renderStreamURL(ref, posVar))
	case KindBigInt:
		b.WriteString(renderStreamBigInt(ref, posVar))
	case KindBigFloat:
		b.WriteString(renderStreamBigFloat(ref, posVar))
	case KindBigRat:
		b.WriteString(renderStreamBigRat(ref, posVar))
	case KindSQLNull:
		b.WriteString(renderStreamSQLNull(f, ref, posVar))
	case KindAny:
		b.WriteString(renderStreamAny(f, ref, posVar))
	default:
		fmt.Fprintf(&b, "k, err := _s.SkipValue(%s)\n", posVar)
		b.WriteString("if err != nil { return result, 0, err }\n")
		fmt.Fprintf(&b, "%s = k\n", posVar)
	}
	if !f.NoValidate {
		b.WriteString(validateAndMod(f, ref))
	}
	return b.String()
}

func renderStreamSlice(f FieldInfo, ref, posVar string) string {
	return emitStreamSliceRead(f, ref, posVar, 0)
}

// emitStreamSliceRead is the streaming counterpart of emitByteSliceRead.
// Handles both slices and fixed-length arrays (KindArray → strict tuple:
// JSON array length must equal f.ArrayLen). All locals carry a depth suffix
// (kN, evN, errN, _idxN) so nested decoders don't clobber each other.
func emitStreamSliceRead(f FieldInfo, dst, posVar string, depth int) string {
	isArray := f.Kind == KindArray
	arrayN := f.ArrayLen
	kvar := fmt.Sprintf("k%d", depth)
	evvar := fmt.Sprintf("ev%d", depth)
	errvar := fmt.Sprintf("err%d", depth)
	ivar := fmt.Sprintf("_idx%d", depth)

	var b strings.Builder
	b.WriteString("{\n")
	if !isArray {
		// `null` → leave slice nil and consume the literal.
		fmt.Fprintf(&b, "var %s error\n", errvar)
		fmt.Fprintf(&b, "%s, %s = _s.SkipSpace(%s)\n", posVar, errvar, posVar)
		fmt.Fprintf(&b, "if %s != nil { return result, 0, %s }\n", errvar, errvar)
		fmt.Fprintf(&b, "if %s >= len(_s.Bytes()) { if %s = _s.ReadMore(); %s != nil { return result, 0, %s } }\n", posVar, errvar, errvar, errvar)
		fmt.Fprintf(&b, "if _s.Bytes()[%s] == 'n' {\n", posVar)
		fmt.Fprintf(&b, "for _ki := 1; _ki < 4; _ki++ {\n")
		fmt.Fprintf(&b, "if %s+_ki >= len(_s.Bytes()) { if %s = _s.ReadMore(); %s != nil { return result, 0, %s } }\n", posVar, errvar, errvar, errvar)
		fmt.Fprintf(&b, "if _s.Bytes()[%s+_ki] != \"null\"[_ki] { return result, 0, scan.ErrBadLiteral }\n", posVar)
		fmt.Fprintf(&b, "}\n")
		fmt.Fprintf(&b, "%s += 4\n", posVar)
		b.WriteString("} else {\n")
	}
	fmt.Fprintf(&b, "%s, %s := _s.ArrayOpen(%s)\n", kvar, errvar, posVar)
	fmt.Fprintf(&b, "if %s != nil { return result, 0, %s }\n", errvar, errvar)
	fmt.Fprintf(&b, "%s, %s = _s.SkipSpace(%s)\n", kvar, errvar, kvar)
	fmt.Fprintf(&b, "if %s != nil { return result, 0, %s }\n", errvar, errvar)
	fmt.Fprintf(&b, "if %s >= len(_s.Bytes()) { if %s = _s.ReadMore(); %s != nil { return result, 0, %s } }\n", kvar, errvar, errvar, errvar)
	slabVar := fmt.Sprintf("_slab%d", depth)
	if isArray {
		fmt.Fprintf(&b, "var %s int\n", ivar)
		if f.ElemPointer {
			fmt.Fprintf(&b, "var %s [%d]%s\n", slabVar, arrayN, f.ElemType)
		}
	} else {
		sCap, slCap := preallocCap(f)
		// Empty `[]` → non-nil empty (stdlib parity); else fresh make()
		// with prealloc. See emitByteSliceRead for the same shape.
		if f.ElemPointer {
			fmt.Fprintf(&b, "var %s []%s\n", slabVar, f.ElemType)
		}
		fmt.Fprintf(&b, "if _s.Bytes()[%s] == ']' {\n", kvar)
		fmt.Fprintf(&b, "%s = %s{}\n", dst, f.GoType)
		fmt.Fprintf(&b, "} else {\n")
		if sCap > 0 {
			fmt.Fprintf(&b, "%s = make(%s, 0, %d)\n", dst, f.GoType, sCap)
		} else {
			fmt.Fprintf(&b, "%s = %s{}\n", dst, f.GoType)
		}
		if f.ElemPointer {
			fmt.Fprintf(&b, "%s = make([]%s, 0, %d)\n", slabVar, f.ElemType, slCap)
		}
		fmt.Fprintf(&b, "}\n")
	}
	fmt.Fprintf(&b, "for _s.Bytes()[%s] != ']' {\n", kvar)
	if isArray {
		fmt.Fprintf(&b, "if %s >= %d { return result, 0, %s }\n",
			ivar, arrayN,
			arrayLenErr(f.JSONName, arrayN, ivar))
	}
	if f.ElemPointer {
		// `null` element → nil pointer. Skip the parse + slab work.
		fmt.Fprintf(&b, "if %s >= len(_s.Bytes()) { if %s = _s.ReadMore(); %s != nil { return result, 0, %s } }\n", kvar, errvar, errvar, errvar)
		fmt.Fprintf(&b, "if _s.Bytes()[%s] == 'n' {\n", kvar)
		fmt.Fprintf(&b, "for _ki := 1; _ki < 4; _ki++ {\n")
		fmt.Fprintf(&b, "if %s+_ki >= len(_s.Bytes()) { if %s = _s.ReadMore(); %s != nil { return result, 0, %s } }\n", kvar, errvar, errvar, errvar)
		fmt.Fprintf(&b, "if _s.Bytes()[%s+_ki] != \"null\"[_ki] { return result, 0, scan.ErrBadLiteral }\n", kvar)
		fmt.Fprintf(&b, "}\n")
		fmt.Fprintf(&b, "%s += 4\n", kvar)
		if isArray {
			fmt.Fprintf(&b, "%s[%s] = nil\n", dst, ivar)
			fmt.Fprintf(&b, "%s++\n", ivar)
		} else {
			fmt.Fprintf(&b, "%s = append(%s, nil)\n", dst, dst)
		}
		fmt.Fprintf(&b, "%s, %s = _s.SkipSpace(%s)\n", kvar, errvar, kvar)
		fmt.Fprintf(&b, "if %s != nil { return result, 0, %s }\n", errvar, errvar)
		fmt.Fprintf(&b, "if %s >= len(_s.Bytes()) { if %s = _s.ReadMore(); %s != nil { return result, 0, %s } }\n", kvar, errvar, errvar, errvar)
		fmt.Fprintf(&b, "if _s.Bytes()[%s] == ',' { %s, %s = _s.SkipSpace(%s + 1); ", kvar, kvar, errvar, kvar)
		fmt.Fprintf(&b, "if %s != nil { return result, 0, %s }; continue }\n", errvar, errvar)
		b.WriteString("break\n}\n")
	}
	fmt.Fprintf(&b, "var %s %s\n", evvar, f.ElemType)
	switch f.ElemKind {
	case KindString:
		fmt.Fprintf(&b, "_sv, _ek, %s := _s.String(%s)\n", errvar, kvar)
		fmt.Fprintf(&b, "if %s != nil { return result, 0, %s }\n", errvar, errvar)
		fmt.Fprintf(&b, "%s = _sv\n%s = _ek\n", evvar, kvar)
	case KindBool:
		fmt.Fprintf(&b, "_bv, _ek, %s := _s.Bool(%s)\n", errvar, kvar)
		fmt.Fprintf(&b, "if %s != nil { return result, 0, %s }\n", errvar, errvar)
		fmt.Fprintf(&b, "%s = _bv\n%s = _ek\n", evvar, kvar)
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64:
		fmt.Fprintf(&b, "_iv, _ek, %s := _s.Int64(%s)\n", errvar, kvar)
		fmt.Fprintf(&b, "if %s != nil { return result, 0, %s }\n", errvar, errvar)
		if f.ElemType == "int64" {
			fmt.Fprintf(&b, "%s = _iv\n", evvar)
		} else {
			fmt.Fprintf(&b, "%s = %s(_iv)\n", evvar, f.ElemType)
		}
		fmt.Fprintf(&b, "%s = _ek\n", kvar)
	case KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		fmt.Fprintf(&b, "_uv, _ek, %s := _s.Uint64(%s)\n", errvar, kvar)
		fmt.Fprintf(&b, "if %s != nil { return result, 0, %s }\n", errvar, errvar)
		if f.ElemType == "uint64" {
			fmt.Fprintf(&b, "%s = _uv\n", evvar)
		} else {
			fmt.Fprintf(&b, "%s = %s(_uv)\n", evvar, f.ElemType)
		}
		fmt.Fprintf(&b, "%s = _ek\n", kvar)
	case KindFloat32, KindFloat64:
		fmt.Fprintf(&b, "_fv, _ek, %s := _s.Float64(%s)\n", errvar, kvar)
		fmt.Fprintf(&b, "if %s != nil { return result, 0, %s }\n", errvar, errvar)
		if f.ElemKind == KindFloat32 {
			fmt.Fprintf(&b, "%s = float32(_fv)\n", evvar)
		} else {
			fmt.Fprintf(&b, "%s = _fv\n", evvar)
		}
		fmt.Fprintf(&b, "%s = _ek\n", kvar)
	case KindStruct:
		if isGenerated(f.ElemType) {
			fmt.Fprintf(&b, "var _z %s\n_sv, _ek, %s := _z.DecodeStreamFrom(_s, %s)\n", f.ElemType, errvar, kvar)
			fmt.Fprintf(&b, "if %s != nil { return result, 0, %s }\n", errvar, errvar)
			fmt.Fprintf(&b, "%s = _sv\n%s = _ek\n", evvar, kvar)
		}
	case KindSlice, KindArray:
		b.WriteString(emitStreamSliceRead(peelSliceField(f), evvar, kvar, depth+1))
	}
	if len(f.ElemMods) > 0 {
		b.WriteString(renderMods(f.ElemMods, evvar, f.ElemType, f.ElemKind))
	}
	if len(f.ElemValidation) > 0 {
		code := renderValidationOn(f.ElemValidation, evvar, f.JSONName+"[]", f.ElemKind, f.MultiErr)
		if !f.MultiErr {
			code = strings.ReplaceAll(code, "return result, ", "return result, 0, ")
		}
		b.WriteString(code)
	}
	if isArray {
		if f.ElemPointer {
			fmt.Fprintf(&b, "%s[%s] = %s\n", slabVar, ivar, evvar)
			fmt.Fprintf(&b, "%s[%s] = &%s[%s]\n", dst, ivar, slabVar, ivar)
		} else {
			fmt.Fprintf(&b, "%s[%s] = %s\n", dst, ivar, evvar)
		}
		fmt.Fprintf(&b, "%s++\n", ivar)
	} else {
		if f.ElemPointer {
			fmt.Fprintf(&b, "%s = append(%s, %s)\n", slabVar, slabVar, evvar)
			fmt.Fprintf(&b, "%s = append(%s, &%s[len(%s)-1])\n", dst, dst, slabVar, slabVar)
		} else {
			fmt.Fprintf(&b, "%s = append(%s, %s)\n", dst, dst, evvar)
		}
	}
	fmt.Fprintf(&b, "%s, %s = _s.SkipSpace(%s)\n", kvar, errvar, kvar)
	fmt.Fprintf(&b, "if %s != nil { return result, 0, %s }\n", errvar, errvar)
	fmt.Fprintf(&b, "if %s >= len(_s.Bytes()) { if %s = _s.ReadMore(); %s != nil { return result, 0, %s } }\n", kvar, errvar, errvar, errvar)
	fmt.Fprintf(&b, "if _s.Bytes()[%s] == ',' { %s, %s = _s.SkipSpace(%s + 1); ", kvar, kvar, errvar, kvar)
	fmt.Fprintf(&b, "if %s != nil { return result, 0, %s }; continue }\n", errvar, errvar)
	b.WriteString("break\n")
	b.WriteString("}\n")
	fmt.Fprintf(&b, "if _s.Bytes()[%s] != ']' { return result, 0, scan.ErrBadArray }\n", kvar)
	if isArray {
		fmt.Fprintf(&b, "if %s != %d { return result, 0, %s }\n",
			ivar, arrayN,
			arrayLenErr(f.JSONName, arrayN, ivar))
	}
	fmt.Fprintf(&b, "%s = %s + 1\n", posVar, kvar)
	if !isArray {
		b.WriteString("}\n") // close else (null-check)
	}
	b.WriteString("}\n") // close outer block
	return b.String()
}

const genTemplate = `{{if .BuildTag}}//go:build {{.BuildTag}}

{{end}}// Code generated by ggen; DO NOT EDIT.

package {{.Package}}

import (
{{- range .Imports}}
	{{printf "%q" .}}
{{- end}}
)

{{range .OneOfDecls}}
{{.}}
{{end}}
{{range .Structs}}
// DecodeFrom decodes one {{.Name}} out of data starting at i and returns
// the decoded value, the position past the last consumed byte, and any
// error. Strings inside the returned value alias data via unsafe.String —
// callers MUST NOT mutate data while the value is in use.
//
// For top-level use, prefer decode.Unmarshal[{{.Name}}](data) (or
// decode.UnmarshalSlice / Read / UnmarshalStream variants from the
// decode package) — those are convenience wrappers around DecodeFrom.
func ({{.Name}}) DecodeFrom(data []byte, i int) ({{.Name}}, int, error) {
	{{decode .}}
}

// DecodeStreamFrom is the io.Reader-backed counterpart of DecodeFrom,
// pulling bytes from a *scan.Stream. Use decode.UnmarshalStream to drive
// it from the top level.
func ({{.Name}}) DecodeStreamFrom(_s *scan.Stream, i int) ({{.Name}}, int, error) {
	{{streamDecode .}}
}

// JSONSize returns an upper bound on the marshaled size, used by
// encode.Marshal to pre-size the buffer in a single allocation.
func (s {{.Name}}) JSONSize() int {
	{{sizeJSON .}}
}

// AppendJSON appends the JSON encoding of s to dst. This is the core
// marshal primitive; for top-level use, prefer encode.Marshal(s) /
// encode.Write(w, s) / encode.MarshalSlice(items) from the encode package.
func (s {{.Name}}) AppendJSON(dst []byte) ([]byte, error) {
	{{appendJSON .}}
}

{{if .Marshal}}
// MarshalJSON implements json.Marshaler (encoding/json and encoding/json/v2).
func (s {{.Name}}) MarshalJSON() ([]byte, error) {
	return encode.Marshal(s)
}
{{end}}

{{if .Unmarshal}}
// UnmarshalJSON implements json.Unmarshaler (encoding/json and encoding/json/v2).
// Note: overwrites the receiver; does not implement merge semantics.
func (s *{{.Name}}) UnmarshalJSON(data []byte) error {
	v, err := decode.Unmarshal[{{.Name}}](data)
	if err != nil {
		return err
	}
	*s = v
	return nil
}
{{end}}
{{end}}
`
