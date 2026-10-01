package model

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// checkRuleApplicability rejects validation rules and mods that do not fit
// the field's Go kind, such as `alphanum` on an int. Without this check the
// generator would emit broken Go that the user only sees at compile time.
// Each rejection is a *RichError, rendered by cmd/ggen/log.go. It runs after
// kinds are resolved, and judges a named primitive by its underlying kind.
// resolved is true on the go/types run, where NamedPrims is filled. There a
// kind that is still KindStruct is a real struct or opaque type, and rules
// that need a specific kind are rejected, because none of their code would
// compile against it. Before that run, and in AST-only mode, KindStruct
// positions are skipped for later.
func checkRuleApplicability(fi FieldInfo, resolved bool) error {
	desc := fi.GoName
	if fi.StructName != "" {
		desc = fi.StructName + "." + fi.GoName
	}
	// Judge `[N]byte` by how it decodes, as one base64 string, whether or
	// not the caller has folded it to KindBytes yet. The go/types run happens
	// before the fold, and an unfolded `[3]byte` reads as an array of uint8
	// with elements to check.
	foldByteArray(&fi)

	// Collect errors from every check instead of stopping at the first. Two
	// problems on one field should both show up in a single run.
	var errs []error
	collect := func(err error) { errs = append(errs, err) }

	// A named primitive reads as KindStruct here, but the generator resolves
	// it through namedKinds and emits real primitive rule code. So judge it by
	// its underlying kind, like effectiveKind does at render time. Unresolved
	// types stay KindStruct, and checkVal and checkMod below decide whether to
	// skip or reject them.
	eff := func(typ string, kind TypeKind) TypeKind {
		if kind == KindStruct {
			// A pointer field has GoType "*Priority", but NamedPrims is
			// keyed by the pointee. Strip the stars, or every rule on a
			// pointer to a named primitive would skip these checks.
			if k, ok := fi.NamedPrims[strings.TrimLeft(typ, "*")]; ok {
				return k
			}
		}
		return kind
	}

	// json:",string" is only for numbers, as in jsonv2. bool is accepted as a
	// documented no-op, because jsonv2 does not quote bools. A string field is
	// rejected with an error instead of a silent no-op that would match
	// neither encoding/json nor jsonv2. Named primitives are rejected too,
	// because the code emitted for the string option does not convert types.
	if fi.String {
		switch fi.Kind { // for a pointer, fi.Kind is already the pointee kind
		case KindBool,
			KindInt, KindInt8, KindInt16, KindInt32, KindInt64,
			KindUint, KindUint8, KindUint16, KindUint32, KindUint64,
			KindFloat32, KindFloat64:
		default:
			collect(&RichError{
				Msg:      desc + ": `json:\",string\"` is only valid on numeric fields (got " + fi.GoType + ")",
				CodeSpan: ",string",
				BotHint:  "string-tag quotes numerics only (jsonv2 defaults)",
				UserHint: "`,string` quotes numeric values, like encoding/json/v2; drop it",
			})
		}
	}

	// `omitempty` skips a value that encodes as null, "", [] or {}. ggen does
	// not act on a struct's `{}`, so a struct field is always emitted. Reject
	// the option instead of accepting a silent no-op. Pointers are not
	// affected, since nil still omits. Kinds with their own JSON shape, such as
	// time.Time, url.URL, sql.Null* and big.*, never reach KindStruct.
	if fi.OmitEmpty && fi.UnderlyingStruct && eff(fi.GoType, fi.Kind) == KindStruct {
		collect(&RichError{
			Msg:      desc + ": `omitempty` is not applicable to a struct field (got " + fi.GoType + ")",
			CodeSpan: "omitempty",
			BotHint:  "omitempty never omits a struct; the option would be a no-op",
			UserHint: "use `omitzero` to omit the Go zero value, or make the field a pointer so nil omits",
		})
	}

	// checkVal and checkMod skip opaque KindStruct positions until the
	// go/types run resolves named primitives. When resolved is true they
	// reject them.
	checkVal := func(rules []ValidationRule, source string, kind TypeKind, typeName, fieldDesc string) {
		if kind == KindStruct && !resolved {
			return
		}
		collect(checkValRules(rules, source, kind, typeName, fieldDesc))
	}
	checkMod := func(mods []ModRule, source string, kind TypeKind, typeName, fieldDesc string) {
		if kind == KindStruct && !resolved {
			return
		}
		collect(checkModRules(mods, source, kind, typeName, fieldDesc))
	}

	// Outer rules apply to the field itself. For a pointer, fi.Kind is the
	// pointee kind, which is the right one to check against.
	checkVal(fi.Validation, "pipe", eff(fi.GoType, fi.Kind), fi.GoType, desc)
	checkMod(fi.Mods, "pipe", eff(fi.GoType, fi.Kind), fi.GoType, desc)

	// keys: is only valid on maps. The key kind is always string.
	if len(fi.KeyValidation) > 0 || len(fi.KeyMods) > 0 {
		if fi.Kind != KindMap {
			collect(&RichError{
				Msg:      desc + ": `keys:` tag prefix is only valid on map[string]V fields (got " + fi.GoType + ")",
				CodeSpan: "keys:",
				BotHint:  "expected map[string]V field",
				UserHint: "`keys:` only works with `map[string]V`",
			})
		}
	}
	checkVal(fi.KeyValidation, "pipe keys:", KindString, "string", desc+" key")
	checkMod(fi.KeyMods, "pipe keys:", KindString, "string", desc+" key")

	// inner: is only valid on slices, arrays and maps. `[]byte` and `[N]byte`
	// decode as one base64 string with no element loop, so an element step
	// there would never run. Only `[N]byte json:",format:array"` keeps real
	// elements.
	hasDive := len(fi.ElemValidation) > 0 || len(fi.ElemMods) > 0 ||
		len(fi.InnerValidation) > 0 || len(fi.InnerMods) > 0
	if hasDive && !canDive(fi.Kind) {
		userHint := "`inner:` only works with slice/array/map"
		if fi.Kind == KindBytes {
			userHint = "a byte slice/array decodes as one base64 string, so there are no elements to dive into; `[N]byte` with `format:array` keeps them"
		}
		collect(&RichError{
			Msg:      desc + ": `inner:` tag prefix is only valid on slice/array/map fields (got " + fi.GoType + ")",
			CodeSpan: "inner:",
			BotHint:  "expected slice/array/map field",
			UserHint: userHint,
		})
	} else {
		// Element rules are judged only when the field has an element type to
		// judge them against. Otherwise the single error above says it all.
		checkVal(fi.ElemValidation, "pipe inner:", eff(fi.ElemType, fi.ElemKind), fi.ElemType, desc+" element")
		checkMod(fi.ElemMods, "pipe inner:", eff(fi.ElemType, fi.ElemKind), fi.ElemType, desc+" element")

		// Levels 2 and deeper, as in `inner:(inner:(...))`, remove one element
		// type per level. A wrong rule deep in the nesting is judged the same
		// way as one at level 1.
		levelType := fi.ElemType
		for li := range max(len(fi.InnerValidation), len(fi.InnerMods)) {
			var levelKind TypeKind
			var ok bool
			levelType, levelKind, ok = peelTypeOnce(levelType)
			ldesc := fmt.Sprintf("%s element (depth %d)", desc, li+2)
			if !ok {
				collect(&RichError{
					Msg:      fmt.Sprintf("%s: `inner:` nested %d deep, but %s has no element at that depth", desc, li+2, fi.GoType),
					CodeSpan: "inner:",
					BotHint:  "more inner: levels than container nesting",
					UserHint: "remove the extra `inner:` level",
				})
				break
			}
			lk := eff(levelType, levelKind)
			if li < len(fi.InnerValidation) {
				checkVal(fi.InnerValidation[li], "pipe inner:", lk, levelType, ldesc)
			}
			if li < len(fi.InnerMods) {
				checkMod(fi.InnerMods[li], "pipe inner:", lk, levelType, ldesc)
			}
		}
	}

	collect(checkFormat(fi, desc))

	// hint only makes sense on containers that grow: slices and maps.
	if fi.HintLen >= 0 && fi.Kind != KindSlice && fi.Kind != KindMap {
		collect(&RichError{
			Msg:      desc + ": `hint` is only valid on slice/map fields (got " + fi.GoType + ")",
			CodeSpan: "hint",
			BotHint:  "expected slice or map field",
			UserHint: "`hint` is a prealloc capacity hint; only slice/map have capacity to size",
		})
	}
	// hint: inner levels must also be slices or maps. A level with no
	// capacity to size gives a hint nothing to do.
	hintLevelType := fi.ElemType
	hintLevelKind := fi.ElemKind
	for li, h := range fi.HintLevels {
		if li > 0 {
			var ok bool
			hintLevelType, hintLevelKind, ok = peelTypeOnce(hintLevelType)
			if !ok {
				hintLevelKind = 0
			}
		}
		if h < 0 {
			continue
		}
		if lk := eff(hintLevelType, hintLevelKind); lk != KindSlice && lk != KindMap {
			collect(&RichError{
				Msg:      fmt.Sprintf("%s: `hint` `inner:` level %d is only valid on a slice/map element (got %s)", desc, li+1, fi.GoType),
				CodeSpan: "inner:",
				BotHint:  "hint inner: level deeper than the container nests, or on a non-container element",
				UserHint: "remove the extra `inner:` hint level",
			})
			break
		}
	}

	return errors.Join(errs...)
}

// checkFormat rejects a `format:` value the emitters do not know. Every emit
// switch has a silent default case, so without this a typo such as
// `format:base64ur` would quietly use the default encoding. time.Time
// accepts any value on purpose, because an unknown value there is a custom
// Go time layout. So only the closed sets are checked.
func checkFormat(fi FieldInfo, desc string) error {
	if fi.Format == "" {
		return nil
	}
	kind := fi.Kind
	if kind == KindSlice || kind == KindArray {
		// A container passes format: down to its elements. `[N]byte` is
		// already KindBytes with ArrayLen > 0, so it needs no unwrapping.
		if k, ok := formatElemKind(fi); ok {
			kind = k
		}
	}
	var valid []string
	switch kind {
	case KindBytes:
		valid = []string{"base64", "base64url", "base32", "base32hex", "base16", "hex", "array"}
	case KindDuration:
		valid = []string{"sec", "milli", "micro", "nano", "units"}
	case KindTime:
		return nil // any other value is a custom time layout
	default:
		return &RichError{
			Msg:      fmt.Sprintf("%s: `format:%s` is not applicable to %s", desc, fi.Format, fi.GoType),
			CodeSpan: "format:" + fi.Format,
			BotHint:  "format: only applies to []byte, time.Time and time.Duration",
			UserHint: "`format:` selects a []byte encoding, a time layout, or a duration unit; drop it here",
		}
	}
	if slices.Contains(valid, fi.Format) {
		return nil
	}
	return &RichError{
		Msg:      fmt.Sprintf("%s: unknown `format:%s` for %s", desc, fi.Format, fi.GoType),
		CodeSpan: "format:" + fi.Format,
		BotHint:  "unrecognized format value; emitters would silently use the default",
		UserHint: "valid values here: " + strings.Join(valid, ", "),
	}
}

// formatElemKind returns the element kind that a container's format:
// applies to. A fixed byte array `[N]byte` is treated like []byte, not as a
// container of formatted elements, and takes the same encodings.
func formatElemKind(fi FieldInfo) (TypeKind, bool) {
	// `format:array` keeps a byte array at KindArray, and a pointer to one
	// shows its levels in GoType. Remove both to reach the byte array.
	if _, leaf := PointerDepth(fi.GoType); isByteArrayType(leaf) {
		return KindBytes, true
	}
	if fi.ElemKind == KindBytes || fi.ElemKind == KindTime || fi.ElemKind == KindDuration {
		return fi.ElemKind, true
	}
	return 0, false
}

// isByteArrayType reports whether typ is a fixed byte array type.
func isByteArrayType(typ string) bool {
	inner, ok := strings.CutPrefix(typ, "[")
	if !ok {
		return false
	}
	n, elem, ok := strings.Cut(inner, "]")
	if !ok || n == "" {
		return false
	}
	if _, err := strconv.Atoi(n); err != nil {
		return false
	}
	return elem == "byte" || elem == "uint8"
}

// peelTypeOnce removes one container level from a type string, for the
// per-level checks. It does what the emitters do: StripOneContainer, then
// map values, then one pointer.
func peelTypeOnce(typ string) (string, TypeKind, bool) {
	switch {
	case strings.HasPrefix(typ, "map[string]"):
		typ = strings.TrimPrefix(typ, "map[string]")
	default:
		inner, _, _ := StripOneContainer(typ)
		if inner == typ {
			return "", 0, false
		}
		typ = inner
	}
	typ = strings.TrimPrefix(typ, "*")
	return typ, ResolveKind(typ), true
}

// canDive reports whether inner: can step into the elements of this kind.
func canDive(k TypeKind) bool {
	return k == KindSlice || k == KindArray || k == KindMap
}

// maxPrealloc limits every value the emitters write into a make() capacity:
// `hint:`, and `len` or `minlen` on a slice or map. It is the largest int a
// 32-bit target can hold. A larger value either overflows the constant at
// compile time or panics with `makeslice: cap out of range` on the first
// input that has the key. Checking it gives an error at generate time
// instead.
const maxPrealloc = math.MaxInt32

// isLenKind reports whether len() works on this kind, so `len`, `minlen`,
// `maxlen` and `notempty` make sense.
func isLenKind(k TypeKind) bool {
	return k == KindString || k == KindSlice || k == KindArray ||
		k == KindMap || k == KindBytes
}

// isIntegralNumeric reports whether the kind is an integer. `multiple=N`
// emits `x % N`, which the Go compiler rejects on floats.
func isIntegralNumeric(k TypeKind) bool {
	//exhaustive:ignore not every kind applies here
	switch k {
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64,
		KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		return true
	}
	return false
}

// valRuleNames lists every known validation rule. The switch in
// checkOneValRule handles exactly these.
var valRuleNames = []string{
	"required", "optional",
	"notempty",
	"len", "minlen", "maxlen",
	"runes", "minrunes", "maxrunes",
	"gt", "gte", "lt", "lte",
	"multiple",
	"eq", "neq", "oneof",
	"url", "alphanum", "numeric", "hexadecimal",
	"islower", "isupper",
	"starts", "ends", "contains",
}

// modRuleNames lists every known mod rule.
var modRuleNames = []string{
	"trim", "tolower", "toupper",
	"trimleft", "trimright",
	"replace", "clamp",
}

// ValidationRuleNames lists every validation rule the tag grammar accepts,
// so a consumer can check that its own mapping is complete.
func ValidationRuleNames() []string { return slices.Clone(valRuleNames) }

// ModRuleNames lists every mod rule the tag grammar accepts.
func ModRuleNames() []string { return slices.Clone(modRuleNames) }

// isUnknownValRule reports whether name is not a known validation rule.
func isUnknownValRule(name string) bool {
	return name != "" && !slices.Contains(valRuleNames, name)
}

// isUnknownMod reports whether name is not a known mod rule. It is the
// mod version of isUnknownValRule.
func isUnknownMod(name string) bool {
	return name != "" && !slices.Contains(modRuleNames, name)
}

// tagAnchor turns a source string such as "pipe inner:" into the start of
// its struct tag, `pipe:"`. It is used as a RichError Anchor when the
// CodeSpan is too short to find alone, since short rule names can also
// appear in the json tag.
func tagAnchor(source string) string {
	if i := strings.IndexByte(source, ' '); i >= 0 {
		source = source[:i]
	}
	if source == "" {
		return ""
	}
	return source + `:"`
}

// checkValRules checks validation rules against kind. KindStruct is not
// skipped. The caller already resolved named primitives with eff(), so a
// struct or opaque kind here has no rule code that would compile. Every rule
// that needs a specific kind is rejected. required, optional and @Func still
// pass.
func checkValRules(rules []ValidationRule, source string, kind TypeKind, typeName, fieldDesc string) error {
	// First pass: collect errors for unknown rules. An unknown name means a
	// typo, and kind errors on the other rules would only add noise, so
	// report the typo alone. Anchor helps locate short rule names.
	anchor := tagAnchor(source)
	var unknown []error
	for _, r := range rules {
		if r.Custom || strings.HasPrefix(r.Name, "@") {
			continue
		}
		if isUnknownValRule(r.Name) {
			unknown = append(unknown, &RichError{
				Msg:      fmt.Sprintf("%s: `%s` is not a known validation rule%s", fieldDesc, r.Name, renamedCaseHint(r.Name)),
				CodeSpan: r.Name,
				Anchor:   anchor,
			})
		}
	}
	if len(unknown) > 0 {
		return errors.Join(unknown...)
	}
	var errs []error
	for _, r := range rules {
		if r.Custom {
			continue
		}
		if err := checkOneValRule(r, source, kind, typeName, fieldDesc); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func checkOneValRule(r ValidationRule, source string, kind TypeKind, typeName, fieldDesc string) error {
	switch r.Name {
	case "", "required", "optional":
		return nil

	case "notempty":
		if !isLenKind(kind) {
			return mismatch(r, source, fieldDesc, typeName,
				"a string/slice/array/map/[]byte",
				"expected string/slice/array/map/[]byte",
				"only available to container types; use `required` to enforce presence")
		}
		return nil

	case "len", "minlen", "maxlen":
		if !isLenKind(kind) {
			userHint := "only available to container types"
			if IsNumeric(kind) {
				userHint += "; for numeric bounds use `gt`/`gte`/`lt`/`lte` instead"
			}
			return mismatch(r, source, fieldDesc, typeName,
				"a string/slice/array/map/[]byte",
				"expected string/slice/array/map/[]byte",
				userHint)
		}
		if err := needNonNegInt(r, fieldDesc); err != nil {
			return err
		}
		// `len` and `minlen` on a slice or map also set its prealloc size.
		if r.Name != "maxlen" && (kind == KindSlice || kind == KindMap) {
			if n, _ := strconv.Atoi(strings.TrimSpace(r.Value)); n > maxPrealloc {
				return &RichError{
					Msg:      fmt.Sprintf("%s: `%s=%s` exceeds the %d prealloc ceiling", fieldDesc, r.Name, r.Value, maxPrealloc),
					CodeSpan: r.Name + "=" + r.Value,
					BotHint:  "len/minlen size the container's make(); an unallocatable capacity panics at decode",
					UserHint: "use a bound the decoder can preallocate, or `hint:\"0\"` to keep the bound without the prealloc",
				}
			}
		}
		return nil

	case "runes", "minrunes", "maxrunes":
		if kind != KindString {
			userHint := "rune-count rules require `string`"
			if isLenKind(kind) {
				userHint = "for length bounds use `len`/`minlen`/`maxlen`"
			}
			return mismatch(r, source, fieldDesc, typeName,
				"a string",
				"rune count requires string",
				userHint)
		}
		return needNonNegInt(r, fieldDesc)

	case "gt", "gte", "lt", "lte":
		if !IsNumeric(kind) {
			userHint := ""
			if isLenKind(kind) {
				userHint = "for length bounds use `len`/`minlen`/`maxlen`"
			}
			return mismatch(r, source, fieldDesc, typeName,
				"a numeric type",
				"expected numeric type",
				userHint)
		}
		return needFloat(r, fieldDesc, kind)

	case "multiple":
		if !isIntegralNumeric(kind) {
			userHint := "use `gt`/`gte`/`lt`/`lte` for non-integer bounds"
			if !IsNumeric(kind) {
				userHint = "modulo divisibility only makes sense on integers"
			}
			return mismatch(r, source, fieldDesc, typeName,
				"an integer type",
				"modulo (multiple=N) only valid on integer types",
				userHint)
		}
		v := strings.TrimSpace(r.Value)
		if v == "" {
			return missingIntErr(r, fieldDesc)
		}
		n, err := parseIntBound(v, kind)
		if errors.Is(err, strconv.ErrRange) {
			return boundRangeErr(fieldDesc, "multiple="+r.Value, v)
		}
		if err != nil {
			return notIntErr(r, fieldDesc)
		}
		// The emitted check is `ref % N != 0`. A zero divisor is a compile
		// error in the generated file, and a negative one has no meaning.
		if _, unsigned := kindIntBits(kind); n == 0 || (!unsigned && int64(n) < 0) {
			return &RichError{
				Msg:      fmt.Sprintf("%s: `multiple=%s` requires a positive integer", fieldDesc, r.Value),
				CodeSpan: "multiple=" + r.Value,
				BotHint:  "non-positive modulo divisor",
				UserHint: "`multiple=N` needs N >= 1",
			}
		}
		return nil

	case "eq", "neq":
		switch {
		case IsNumeric(kind):
			return needFloat(r, fieldDesc, kind)
		case kind == KindString:
			return nil
		default:
			return mismatch(r, source, fieldDesc, typeName,
				"a string or numeric type",
				"expected string or numeric",
				"use a custom validator instead")
		}

	case "oneof":
		if kind != KindString && !IsNumeric(kind) {
			return mismatch(r, source, fieldDesc, typeName,
				"a string or numeric type",
				"expected string or numeric",
				"use a custom validator instead")
		}
		if r.Value == "" {
			// An example that matches the field's kind.
			example := "oneof=admin|user|guest"
			if IsNumeric(kind) {
				example = "oneof=1|2|3"
			}
			return &RichError{
				Msg:      fieldDesc + ": `oneof` requires a `|`-separated list of allowed values",
				CodeSpan: "oneof",
				BotHint:  "empty oneof list",
				UserHint: fmt.Sprintf("provide values like `%s`", example),
			}
		}
		// Duplicates would emit duplicate switch cases, which is a compile
		// error in the generated file. Numeric parts compare by value, so 1,
		// 1.0 and +1 are equal. An integer kind compares the integer itself,
		// because a float64 key would merge different integers above 2^53.
		dupErr := func(p string) error {
			return &RichError{
				Msg:      fmt.Sprintf("%s: `oneof=%s` part %q is a duplicate", fieldDesc, r.Value, p),
				CodeSpan: p,
				BotHint:  "duplicate oneof part emits duplicate switch cases",
				UserHint: "every `oneof=` part must be unique",
			}
		}
		if IsNumeric(kind) {
			seen := map[uint64]struct{}{}
			for _, p := range SplitPipeParts(r.Value) {
				p = strings.TrimSpace(p)
				key, err := numericPartKey(p, kind)
				if err != nil {
					return &RichError{
						Msg:      fmt.Sprintf("%s: `oneof=%s` part %q is not a valid number", fieldDesc, r.Value, p),
						CodeSpan: p,
						BotHint:  "non-numeric part in numeric oneof list",
						UserHint: "for numeric fields every `oneof=` part must parse as a number",
					}
				}
				if _, dup := seen[key]; dup {
					return dupErr(p)
				}
				seen[key] = struct{}{}
				if !boundFits(p, kind) {
					return boundRangeErr(fieldDesc, "oneof="+r.Value, p)
				}
			}
		} else {
			seen := map[string]struct{}{}
			for _, p := range SplitPipeParts(r.Value) {
				if _, dup := seen[p]; dup {
					return dupErr(p)
				}
				seen[p] = struct{}{}
			}
		}
		return nil

	case "url", "alphanum", "numeric", "hexadecimal",
		"islower", "isupper":
		if kind != KindString {
			return mismatch(r, source, fieldDesc, typeName,
				"a string",
				"expected string",
				"only applicable to strings")
		}
		return nil

	case "starts", "ends", "contains":
		if kind != KindString {
			return mismatch(r, source, fieldDesc, typeName,
				"a string",
				"expected string",
				"only applicable to strings")
		}
		if r.Value == "" {
			return &RichError{
				Msg:      fmt.Sprintf("%s: `%s` requires a non-empty value", fieldDesc, r.Name),
				CodeSpan: r.Name,
				BotHint:  "missing substring for " + r.Name,
				UserHint: fmt.Sprintf("provide a value like `%s=foo`", r.Name),
			}
		}
		return nil
	}
	// `@FuncName` references are resolved later, so let them pass.
	if strings.HasPrefix(r.Name, "@") {
		return nil
	}
	// Unknown rule. Reject it. The message says everything, so no hints.
	return &RichError{
		Msg:      fmt.Sprintf("%s: `%s` is not a known validation rule%s", fieldDesc, r.Name, renamedCaseHint(r.Name)),
		CodeSpan: r.Name,
	}
}

// renamedCaseHint returns a hint for the bare steps `lower` and `upper`. It
// points at the `tolower` and `toupper` mods and the `islower` and
// `isupper` validators.
func renamedCaseHint(name string) string {
	switch name {
	case "lower", "upper":
		return fmt.Sprintf(" — it split into `to%[1]s` (transform) and `is%[1]s` (validator)", name)
	}
	return ""
}

// checkModRules checks mods against kind. KindStruct is not skipped, for the
// same reason as in checkValRules.
func checkModRules(mods []ModRule, source string, kind TypeKind, typeName, fieldDesc string) error {
	// Report unknown names alone, as checkValRules does.
	anchor := tagAnchor(source)
	var unknown []error
	for _, m := range mods {
		if m.Custom || strings.HasPrefix(m.Name, "@") {
			continue
		}
		if isUnknownMod(m.Name) {
			unknown = append(unknown, &RichError{
				Msg:      fmt.Sprintf("%s: `%s` is not a known mod", fieldDesc, m.Name),
				CodeSpan: m.Name,
				Anchor:   anchor,
			})
		}
	}
	if len(unknown) > 0 {
		return errors.Join(unknown...)
	}
	var errs []error
	for _, m := range mods {
		if m.Custom {
			continue
		}
		if err := checkOneModRule(m, source, kind, typeName, fieldDesc); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func checkOneModRule(m ModRule, source string, kind TypeKind, typeName, fieldDesc string) error {
	switch m.Name {
	case "trim", "tolower", "toupper":
		if kind != KindString {
			return mismatchMod(m, source, fieldDesc, typeName,
				"a string",
				"expected string",
				"drop the mod or change the field to `string`")
		}
		return nil

	case "trimleft", "trimright":
		if kind != KindString {
			return mismatchMod(m, source, fieldDesc, typeName,
				"a string",
				"expected string",
				"drop the mod or change the field to `string`")
		}
		if m.Value == "" {
			return &RichError{
				Msg:      fmt.Sprintf("%s: `%s` requires a non-empty value", fieldDesc, m.Name),
				CodeSpan: m.Name,
				BotHint:  "missing prefix/suffix to trim",
				UserHint: fmt.Sprintf("provide the substring to strip, e.g. `%s=SKU-`", m.Name),
			}
		}
		return nil

	case "replace":
		if kind != KindString {
			return mismatchMod(m, source, fieldDesc, typeName,
				"a string",
				"expected string",
				"drop the mod or change the field to `string`")
		}
		parts := SplitPipeParts(m.Value)
		if len(parts) != 2 || parts[0] == "" {
			return &RichError{
				Msg:      fieldDesc + ": `replace` requires `old|new` form (old cannot be empty)",
				CodeSpan: "replace=" + m.Value,
				BotHint:  "malformed replace parameter",
				UserHint: "use `replace=old|new`, e.g. `replace=foo|bar`; quote a part containing `|`: `replace='a|b'|c`",
			}
		}
		return nil

	case "clamp":
		if !IsNumeric(kind) {
			return mismatchMod(m, source, fieldDesc, typeName,
				"a numeric type",
				"expected numeric type",
				"for strings use `trim` / `replace` instead")
		}
		cparts := SplitPipeParts(m.Value)
		if len(cparts) != 2 {
			return &RichError{
				Msg:      fieldDesc + ": `clamp` needs exactly one lo`|`hi separator",
				CodeSpan: "clamp=" + m.Value,
				BotHint:  "malformed clamp parameter",
				UserHint: "use `clamp=lo|hi`, leave either bound empty for one-sided: `clamp=0|` or `clamp=|100`",
			}
		}
		lo, hi := strings.TrimSpace(cparts[0]), strings.TrimSpace(cparts[1])
		if lo == "" && hi == "" {
			return &RichError{
				Msg:      fieldDesc + ": `clamp` requires at least one of lo or hi",
				CodeSpan: "clamp",
				BotHint:  "clamp with no bounds",
				UserHint: "provide at least one bound, e.g. `clamp=0|100` or `clamp=|100`",
			}
		}
		checkBound := func(name, v string) error {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return &RichError{
					Msg:      fmt.Sprintf("%s: `clamp` %s %q is not a valid number", fieldDesc, name, v),
					CodeSpan: v,
					BotHint:  "non-numeric clamp " + name + " bound",
					UserHint: "both bounds must be numeric, e.g. `clamp=0|100`",
				}
			}
			// Bounds are written as is into Go comparisons. NaN and Inf
			// have no Go constant form, and a fractional bound does not fit an
			// integer kind. Both break the build of the generated file.
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return &RichError{
					Msg:      fmt.Sprintf("%s: `clamp` %s %q — NaN/Inf are not valid bounds", fieldDesc, name, v),
					CodeSpan: v,
					BotHint:  "NaN/Inf bound has no Go constant spelling",
					UserHint: "use finite numeric bounds",
				}
			}
			if isIntegralNumeric(kind) {
				if _, err := parseIntBound(v, kind); err != nil && !errors.Is(err, strconv.ErrRange) {
					return &RichError{
						Msg:      fmt.Sprintf("%s: `clamp` %s %q — integer field needs integer bounds", fieldDesc, name, v),
						CodeSpan: v,
						BotHint:  "fractional clamp bound against integral kind fails the generated build",
						UserHint: "use whole-number bounds, or make the field a float",
					}
				}
			}
			if !boundFits(v, kind) {
				return boundRangeErr(fieldDesc, "clamp="+m.Value, v)
			}
			return nil
		}
		if lo != "" {
			if err := checkBound("lo", lo); err != nil {
				return err
			}
		}
		if hi != "" {
			if err := checkBound("hi", hi); err != nil {
				return err
			}
		}
		return nil
	}
	// `@FuncName` mods are resolved later, so let them pass.
	if strings.HasPrefix(m.Name, "@") {
		return nil
	}
	// Unknown mod. Reject it. The message says everything, so no hint.
	return &RichError{
		Msg:      fmt.Sprintf("%s: `%s` is not a known mod", fieldDesc, m.Name),
		CodeSpan: m.Name,
	}
}

// mismatch builds the *RichError for a validation rule on the wrong kind:
// "<Struct>.<Field>: <rule> is inapplicable to <type>".
func mismatch(r ValidationRule, source, fieldDesc, typeName, requiredKind, botHint, userTail string) *RichError {
	_ = source
	return &RichError{
		Msg:      fmt.Sprintf("%s: `%s` is inapplicable to %s", fieldDesc, r.Name, typeName),
		CodeSpan: r.Name,
		BotHint:  botHint,
		UserHint: fmt.Sprintf("`%s` requires %s; %s", r.Name, requiredKind, userTail),
	}
}

func mismatchMod(m ModRule, source, fieldDesc, typeName, requiredKind, botHint, userTail string) *RichError {
	_ = source
	return &RichError{
		Msg:      fmt.Sprintf("%s: `%s` is inapplicable to %s", fieldDesc, m.Name, typeName),
		CodeSpan: m.Name,
		BotHint:  botHint,
		UserHint: fmt.Sprintf("`%s` requires %s. %s", m.Name, requiredKind, userTail),
	}
}

// kindIntBits returns the bit width and signedness of an integer kind, and
// (0, false) for any other kind.
func kindIntBits(kind TypeKind) (int, bool) {
	//exhaustive:ignore not every kind applies here
	switch kind {
	case KindInt, KindInt64:
		return 64, false
	case KindInt8:
		return 8, false
	case KindInt16:
		return 16, false
	case KindInt32:
		return 32, false
	case KindUint, KindUint64:
		return 64, true
	case KindUint8:
		return 8, true
	case KindUint16:
		return 16, true
	case KindUint32:
		return 32, true
	}
	return 0, false
}

// parseIntBound parses v as an integer constant of the kind's width and
// sign. It returns the value as a 64-bit two's-complement bit pattern, with
// signed kinds sign-extended. The error comes from strconv: ErrSyntax when v
// is not an integer, ErrRange when it does not fit. It does not use Atoi,
// because Atoi stops at MaxInt64 and would report every uint64 bound from
// 2^63 up as not an integer.
func parseIntBound(v string, kind TypeKind) (uint64, error) {
	bits, unsigned := kindIntBits(kind)
	if unsigned {
		// A negative integer is out of range, not a syntax error.
		if rest, neg := strings.CutPrefix(v, "-"); neg {
			if _, err := strconv.ParseUint(rest, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
				return 0, strconv.ErrRange
			}
		}
		return strconv.ParseUint(v, 10, bits)
	}
	n, err := strconv.ParseInt(v, 10, bits)
	return uint64(n), err
}

// numericPartKey returns the key used to find duplicate numeric `oneof`
// parts. For an integer kind it is the integer itself, and a float spelling
// of a whole number such as `1.0` maps to the same key. For other kinds it
// is the float64 value, with -0 mapped to 0.
func numericPartKey(p string, kind TypeKind) (uint64, error) {
	if isIntegralNumeric(kind) {
		if n, err := parseIntBound(p, kind); err == nil {
			return n, nil
		}
	}
	f, err := strconv.ParseFloat(p, 64)
	if err != nil {
		return 0, err
	}
	if isIntegralNumeric(kind) && f == math.Trunc(f) {
		if _, unsigned := kindIntBits(kind); unsigned {
			return uint64(f), nil
		}
		return uint64(int64(f)), nil
	}
	if f == 0 {
		f = 0
	}
	return math.Float64bits(f), nil
}

// boundFits reports whether the numeric literal v fits the kind's range and
// sign. Bounds are written as is into Go comparisons with the field, so
// `uint8 >= -1` or `int8 <= 300` would be a constant overflow compile error
// in the generated file.
func boundFits(v string, kind TypeKind) bool {
	if isIntegralNumeric(kind) {
		_, err := parseIntBound(v, kind)
		return err == nil
	}
	if kind == KindFloat32 {
		f, err := strconv.ParseFloat(v, 64)
		return err == nil && !math.IsInf(float64(float32(f)), 0)
	}
	return true
}

func boundRangeErr(fieldDesc, ruleSpelling, v string) *RichError {
	return &RichError{
		Msg:      fmt.Sprintf("%s: `%s` bound %q is out of the field type's range", fieldDesc, ruleSpelling, v),
		CodeSpan: v,
		BotHint:  "out-of-range bound is a constant-overflow compile error in the generated file",
		UserHint: "use a bound the field's type can hold (mind sign for unsigned kinds)",
	}
}

// needNonNegInt is needInt for the length and count rules, where a negative
// bound makes no sense. `maxlen=-1` compiles to a check no value can pass, so
// the field would reject every input with no error at generate time.
func needNonNegInt(r ValidationRule, fieldDesc string) error {
	if err := needInt(r, fieldDesc); err != nil {
		return err
	}
	if n, _ := strconv.Atoi(strings.TrimSpace(r.Value)); n < 0 {
		return &RichError{
			Msg:      fmt.Sprintf("%s: `%s=%s` requires a non-negative integer", fieldDesc, r.Name, r.Value),
			CodeSpan: r.Name + "=" + r.Value,
			BotHint:  "negative length/count bound",
			UserHint: fmt.Sprintf("lengths and counts are never negative; use `%s=0` or a positive value", r.Name),
		}
	}
	return nil
}

// needInt checks a count parameter, such as a length or rune count. It is a
// plain int and does not depend on the field kind.
func needInt(r ValidationRule, fieldDesc string) error {
	v := strings.TrimSpace(r.Value)
	if v == "" {
		return missingIntErr(r, fieldDesc)
	}
	if _, err := strconv.Atoi(v); err != nil {
		return notIntErr(r, fieldDesc)
	}
	return nil
}

func missingIntErr(r ValidationRule, fieldDesc string) *RichError {
	return &RichError{
		Msg:      fmt.Sprintf("%s: `%s` requires an integer value", fieldDesc, r.Name),
		CodeSpan: r.Name,
		BotHint:  "missing integer parameter",
		UserHint: fmt.Sprintf("provide an integer like `%s=5`", r.Name),
	}
}

func notIntErr(r ValidationRule, fieldDesc string) *RichError {
	return &RichError{
		Msg:      fmt.Sprintf("%s: `%s=%s` value is not a valid integer", fieldDesc, r.Name, r.Value),
		CodeSpan: r.Name + "=" + r.Value,
		BotHint:  "non-integer parameter for integer rule",
		UserHint: fmt.Sprintf("use a whole-number value like `%s=5` (no decimals, no letters)", r.Name),
	}
}

func needFloat(r ValidationRule, fieldDesc string, kind TypeKind) error {
	v := strings.TrimSpace(r.Value)
	if v == "" {
		return &RichError{
			Msg:      fmt.Sprintf("%s: `%s` requires a numeric value", fieldDesc, r.Name),
			CodeSpan: r.Name,
			BotHint:  "missing numeric parameter",
			UserHint: fmt.Sprintf("provide a numeric value like `%s=5` or `%s=1.5`", r.Name, r.Name),
		}
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return &RichError{
			Msg:      fmt.Sprintf("%s: `%s=%s` value is not a valid number", fieldDesc, r.Name, r.Value),
			CodeSpan: r.Name + "=" + r.Value,
			BotHint:  "non-numeric parameter for numeric rule",
			UserHint: fmt.Sprintf("use a numeric value like `%s=5` or `%s=1.5`", r.Name, r.Name),
		}
	}
	// The value is written as is into a Go comparison. NaN and Inf are not
	// Go constants, and a fractional bound against an integer kind is a
	// constant truncation error. Both break the build of the generated file.
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return &RichError{
			Msg:      fmt.Sprintf("%s: `%s=%s` — NaN/Inf are not valid bounds", fieldDesc, r.Name, r.Value),
			CodeSpan: r.Name + "=" + r.Value,
			BotHint:  "NaN/Inf bound has no Go constant spelling",
			UserHint: "use a finite numeric bound",
		}
	}
	if isIntegralNumeric(kind) {
		if _, err := parseIntBound(v, kind); err != nil && !errors.Is(err, strconv.ErrRange) {
			return &RichError{
				Msg:      fmt.Sprintf("%s: `%s=%s` — integer field needs an integer bound", fieldDesc, r.Name, r.Value),
				CodeSpan: r.Name + "=" + r.Value,
				BotHint:  "fractional bound against integral kind fails the generated build",
				UserHint: "use a whole number, or make the field a float",
			}
		}
	}
	if !boundFits(v, kind) {
		return boundRangeErr(fieldDesc, r.Name+"="+r.Value, v)
	}
	return nil
}
