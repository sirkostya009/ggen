package main

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// checkRuleApplicability rejects validation/mod tags that don't fit the
// field's Go kind (e.g. `ascii` on an int) — otherwise the generator emits
// broken Go the user only hits at compile time. Each reject is a *richError
// (see log.go). Called after kind resolution; named primitives are judged by
// their underlying kind. `resolved` marks the go/types re-run (NamedPrims
// populated): a still-KindStruct kind there is a genuine struct/opaque type
// and kinded rules reject — no rule emit path compiles against it. Before
// resolution (and in AST-only mode) KindStruct positions defer.
func checkRuleApplicability(fi FieldInfo, resolved bool) error {
	desc := fi.GoName
	if fi.StructName != "" {
		desc = fi.StructName + "." + fi.GoName
	}
	// Judge `[N]byte` by the shape it decodes as (one base64 string), whether
	// or not the caller has folded it yet — the go/types re-run precedes the
	// fold, and an unfolded `[3]byte` reads as a diveable array of uint8.
	foldByteArray(&fi)

	// Gather across every phase rather than short-circuiting — two bugs on one
	// field should both surface in a single run.
	var errs []error
	collect := func(err error) { errs = append(errs, err) }

	// A named primitive reports KindStruct here, but generate resolves it via
	// namedKinds and emits REAL primitive rule code — judge it by the
	// underlying kind (parse-time twin of effectiveKind). Unresolved types
	// stay KindStruct; checkVal/checkMod below decide defer-vs-reject.
	eff := func(typ string, kind TypeKind) TypeKind {
		if kind == KindStruct {
			// Pointer fields spell GoType "*Priority" while NamedPrims keys
			// the pointee — strip the stars or every rule on a
			// pointer-to-named-primitive bypasses the matrix.
			if k, ok := fi.NamedPrims[strings.TrimLeft(typ, "*")]; ok {
				return k
			}
		}
		return kind
	}

	// json:",string" is numerics-only (jsonv2 defaults; bool is tolerated as
	// a documented no-op since v2 dropped bool quoting). A string field is a
	// loud reject rather than a silent no-op that matches neither v1 nor v2;
	// so are named primitives — the string-tag emit does not cast.
	if fi.String {
		switch fi.Kind { // for pointers fi.Kind is already the pointee kind
		case KindBool,
			KindInt, KindInt8, KindInt16, KindInt32, KindInt64,
			KindUint, KindUint8, KindUint16, KindUint32, KindUint64,
			KindFloat32, KindFloat64:
		default:
			collect(&richError{
				Msg:      desc + ": `json:\",string\"` is only valid on numeric fields (got " + fi.GoType + ")",
				CodeSpan: ",string",
				BotHint:  "string-tag quotes numerics only (jsonv2 defaults)",
				UserHint: "`,string` quotes numeric values, like encoding/json/v2; drop it",
			})
		}
	}

	// `omitempty` omits a value encoding as null, "", [] or {}, and a struct's
	// `{}` is the one of those ggen does not act on: a struct field is always
	// emitted. Reject rather than accept a silent no-op. Pointers are untouched
	// — nil still omits — and the kinds with a wire shape of their own
	// (time.Time, url.URL, sql.Null*, big.*, …) never reach KindStruct.
	if fi.OmitEmpty && fi.UnderlyingStruct && eff(fi.GoType, fi.Kind) == KindStruct {
		collect(&richError{
			Msg:      desc + ": `omitempty` is not applicable to a struct field (got " + fi.GoType + ")",
			CodeSpan: "omitempty",
			BotHint:  "omitempty never omits a struct; the option would be a no-op",
			UserHint: "use `omitzero` to omit the Go zero value, or make the field a pointer so nil omits",
		})
	}

	// checkVal/checkMod defer opaque (KindStruct) positions until the
	// go/types re-run resolves named primitives; resolved => reject there.
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

	// Outer rules apply to the field itself. For pointers fi.Kind is the
	// pointee kind, the right anchor.
	checkVal(fi.Validation, "pipe", eff(fi.GoType, fi.Kind), fi.GoType, desc)
	checkMod(fi.Mods, "pipe", eff(fi.GoType, fi.Kind), fi.GoType, desc)

	// keys: only valid on maps. Keyed kind itself is always string.
	if len(fi.KeyValidation) > 0 || len(fi.KeyMods) > 0 {
		if fi.Kind != KindMap {
			collect(&richError{
				Msg:      desc + ": `keys:` tag prefix is only valid on map[string]V fields (got " + fi.GoType + ")",
				CodeSpan: "keys:",
				BotHint:  "expected map[string]V field",
				UserHint: "`keys:` only works with `map[string]V`",
			})
		}
	}
	checkVal(fi.KeyValidation, "pipe keys:", KindString, "string", desc+" key")
	checkMod(fi.KeyMods, "pipe keys:", KindString, "string", desc+" key")

	// inner: only valid on slice/array/map. `[]byte`/`[N]byte` decode as one
	// base64 string with no element loop, so an element step there never
	// runs; only `[N]byte json:",format:array"` keeps real elements.
	hasDive := len(fi.ElemValidation) > 0 || len(fi.ElemMods) > 0 ||
		len(fi.InnerValidation) > 0 || len(fi.InnerMods) > 0
	if hasDive && !canDive(fi.Kind) {
		userHint := "`inner:` only works with slice/array/map"
		if fi.Kind == KindBytes {
			userHint = "a byte slice/array decodes as one base64 string, so there are no elements to dive into; `[N]byte` with `format:array` keeps them"
		}
		collect(&richError{
			Msg:      desc + ": `inner:` tag prefix is only valid on slice/array/map fields (got " + fi.GoType + ")",
			CodeSpan: "inner:",
			BotHint:  "expected slice/array/map field",
			UserHint: userHint,
		})
	} else {
		// Element rules are judged only where the field HAS an element type
		// to name them against; above, the one diagnostic is the whole story.
		checkVal(fi.ElemValidation, "pipe inner:", eff(fi.ElemType, fi.ElemKind), fi.ElemType, desc+" element")
		checkMod(fi.ElemMods, "pipe inner:", eff(fi.ElemType, fi.ElemKind), fi.ElemType, desc+" element")

		// Levels >= 2 (`inner:(inner:(...))`) peel the element type per
		// level, so a rule mismatched deep in the nest is judged on the same
		// terms as one at level 1.
		levelType := fi.ElemType
		for li := 0; li < max(len(fi.InnerValidation), len(fi.InnerMods)); li++ {
			var levelKind TypeKind
			var ok bool
			levelType, levelKind, ok = peelTypeOnce(levelType)
			ldesc := fmt.Sprintf("%s element (depth %d)", desc, li+2)
			if !ok {
				collect(&richError{
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

	// hint only meaningful on growable containers (slice/map).
	if fi.HintLen >= 0 && fi.Kind != KindSlice && fi.Kind != KindMap {
		collect(&richError{
			Msg:      desc + ": `hint` is only valid on slice/map fields (got " + fi.GoType + ")",
			CodeSpan: "hint",
			BotHint:  "expected slice or map field",
			UserHint: "`hint` is a prealloc capacity hint; only slice/map have capacity to size",
		})
	}
	// hint: inner levels must land on a growable level too — a level with no
	// capacity to size has nothing for a hint to do.
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
			collect(&richError{
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

// checkFormat rejects a `format:` the emitters don't recognize. Every emit
// switch has a silent default arm, so without this a typo (`format:base64ur`)
// would reach the wire as the default encoding with no diagnostic anywhere.
// time.Time is deliberately open-ended: an unrecognized value there is a
// custom Go layout (see the custom-layout support), so only the closed sets
// are policed.
func checkFormat(fi FieldInfo, desc string) error {
	if fi.Format == "" {
		return nil
	}
	kind := fi.Kind
	if kind == KindSlice || kind == KindArray {
		// A container carries format: down to its elements. `[N]byte` is
		// already KindBytes (ArrayLen > 0), so it needs no peel.
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
		return nil // any other value is a custom layout
	default:
		return &richError{
			Msg:      fmt.Sprintf("%s: `format:%s` is not applicable to %s", desc, fi.Format, fi.GoType),
			CodeSpan: "format:" + fi.Format,
			BotHint:  "format: only applies to []byte, time.Time and time.Duration",
			UserHint: "`format:` selects a []byte encoding, a time layout, or a duration unit; drop it here",
		}
	}
	if slices.Contains(valid, fi.Format) {
		return nil
	}
	return &richError{
		Msg:      fmt.Sprintf("%s: unknown `format:%s` for %s", desc, fi.Format, fi.GoType),
		CodeSpan: "format:" + fi.Format,
		BotHint:  "unrecognized format value; emitters would silently use the default",
		UserHint: "valid values here: " + strings.Join(valid, ", "),
	}
}

// formatElemKind resolves the element kind a container's format: applies to.
// A fixed byte array (`[N]byte`) is a []byte-shaped value, not a container of
// formatted elements — it takes the same encodings.
func formatElemKind(fi FieldInfo) (TypeKind, bool) {
	// `format:array` keeps a byte array at KindArray, and a pointer to one
	// spells its levels in GoType — peel both to reach the byte array.
	if _, leaf := pointerDepth(fi.GoType); isByteArrayType(leaf) {
		return KindBytes, true
	}
	if fi.ElemKind == KindBytes || fi.ElemKind == KindTime || fi.ElemKind == KindDuration {
		return fi.ElemKind, true
	}
	return 0, false
}

// isByteArrayType reports whether typ spells a fixed byte array.
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

// peelTypeOnce strips one container level off a type string (mirroring the
// stripOneContainer + map-value + pointer peels the emitters apply), for the
// per-level applicability walk.
func peelTypeOnce(typ string) (string, TypeKind, bool) {
	switch {
	case strings.HasPrefix(typ, "map[string]"):
		typ = strings.TrimPrefix(typ, "map[string]")
	default:
		inner, _, _ := stripOneContainer(typ)
		if inner == typ {
			return "", 0, false
		}
		typ = inner
	}
	typ = strings.TrimPrefix(typ, "*")
	return typ, resolveKind(typ), true
}

// canDive reports whether inner: can peel one level off this kind.
func canDive(k TypeKind) bool {
	return k == KindSlice || k == KindArray || k == KindMap
}

// maxPrealloc caps every value the emitters paste into a make() capacity
// (`hint:`, and `len`/`minlen` on a slice or map): the largest int a 32-bit
// target can hold. Past it the value either overflows the constant at compile
// time or panics with `makeslice: cap out of range` on the first payload
// carrying the key — a generate-time diagnostic instead.
const maxPrealloc = math.MaxInt32

// isLenKind reports whether len() is meaningful on this kind, so
// `len`/`minlen`/`maxlen`/`notempty` make sense.
func isLenKind(k TypeKind) bool {
	return k == KindString || k == KindSlice || k == KindArray ||
		k == KindMap || k == KindBytes
}

// isIntegralNumeric reports whether modulo (`%`) is legal on the kind —
// `multiple=N` emits `x % N`, which the Go compiler rejects on floats.
func isIntegralNumeric(k TypeKind) bool {
	switch k {
	case KindInt, KindInt8, KindInt16, KindInt32, KindInt64,
		KindUint, KindUint8, KindUint16, KindUint32, KindUint64:
		return true
	}
	return false
}

// isUnknownValRule reports whether name is NOT a recognized validation rule.
// Keep in sync with checkOneValRule's switch.
func isUnknownValRule(name string) bool {
	switch name {
	case "", "required", "optional",
		"notempty",
		"len", "minlen", "maxlen",
		"runes", "minrunes", "maxrunes",
		"gt", "gte", "lt", "lte",
		"multiple",
		"eq", "neq", "oneof",
		"url", "alphanum", "numeric", "hexadecimal",
		"islower", "isupper",
		"starts", "ends", "contains":
		return false
	}
	return true
}

// isUnknownMod reports whether the mod name is NOT in the recognized
// mod-rule set. Mirror of isUnknownValRule for mods.
func isUnknownMod(name string) bool {
	switch name {
	case "", "trim", "tolower", "toupper",
		"trimleft", "trimright",
		"replace", "clamp":
		return false
	}
	return true
}

// tagAnchor derives the struct-tag opening (`pipe:"`) from a source string
// like "pipe inner:" — used as a richError Anchor when the CodeSpan is too
// short to locate alone (single-char rule names collide with json values).
func tagAnchor(source string) string {
	if i := strings.IndexByte(source, ' '); i >= 0 {
		source = source[:i]
	}
	if source == "" {
		return ""
	}
	return source + `:"`
}

// KindStruct is NOT skipped: the caller already resolved named primitives via
// eff(), so a struct/opaque kind here has no compilable rule emit — every
// kind-checked rule rejects (required/optional/@Func still pass).
func checkValRules(rules []ValidationRule, source string, kind TypeKind, typeName, fieldDesc string) error {
	// First pass: collect unknown-rule errors. An unknown name means a typo;
	// kind-mismatch diagnostics on the other rules would just be noise, so
	// surface the typo alone. Anchor disambiguates short rule names.
	anchor := tagAnchor(source)
	var unknown []error
	for _, r := range rules {
		if r.Custom || strings.HasPrefix(r.Name, "@") {
			continue
		}
		if isUnknownValRule(r.Name) {
			unknown = append(unknown, &richError{
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
			if isNumeric(kind) {
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
		// `len`/`minlen` on a growable container double as its prealloc hint.
		if r.Name != "maxlen" && (kind == KindSlice || kind == KindMap) {
			if n, _ := strconv.Atoi(strings.TrimSpace(r.Value)); n > maxPrealloc {
				return &richError{
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
		if !isNumeric(kind) {
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
			if !isNumeric(kind) {
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
		// The emit is `ref % N != 0` — a zero divisor is a compile error in
		// the generated file, and a negative one is meaningless.
		if _, unsigned := kindIntBits(kind); n == 0 || (!unsigned && int64(n) < 0) {
			return &richError{
				Msg:      fmt.Sprintf("%s: `multiple=%s` requires a positive integer", fieldDesc, r.Value),
				CodeSpan: "multiple=" + r.Value,
				BotHint:  "non-positive modulo divisor",
				UserHint: "`multiple=N` needs N >= 1",
			}
		}
		return nil

	case "eq", "neq":
		switch {
		case isNumeric(kind):
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
		if kind != KindString && !isNumeric(kind) {
			return mismatch(r, source, fieldDesc, typeName,
				"a string or numeric type",
				"expected string or numeric",
				"use a custom validator instead")
		}
		if r.Value == "" {
			// Example matching the field's kind.
			example := "oneof=admin|user|guest"
			if isNumeric(kind) {
				example = "oneof=1|2|3"
			}
			return &richError{
				Msg:      fmt.Sprintf("%s: `oneof` requires a `|`-separated list of allowed values", fieldDesc),
				CodeSpan: "oneof",
				BotHint:  "empty oneof list",
				UserHint: fmt.Sprintf("provide values like `%s`", example),
			}
		}
		// Duplicates emit duplicate switch cases — a compile error in the
		// generated file. Numeric parts compare by VALUE (1 vs 1.0 vs +1); an
		// integral kind keys on the integer itself, since a float64 key
		// merges distinct integers above 2^53.
		dupErr := func(p string) error {
			return &richError{
				Msg:      fmt.Sprintf("%s: `oneof=%s` part %q is a duplicate", fieldDesc, r.Value, p),
				CodeSpan: p,
				BotHint:  "duplicate oneof part emits duplicate switch cases",
				UserHint: "every `oneof=` part must be unique",
			}
		}
		if isNumeric(kind) {
			seen := map[uint64]struct{}{}
			for _, p := range splitPipeParts(r.Value) {
				p = strings.TrimSpace(p)
				key, err := numericPartKey(p, kind)
				if err != nil {
					return &richError{
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
			for _, p := range splitPipeParts(r.Value) {
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
			return &richError{
				Msg:      fmt.Sprintf("%s: `%s` requires a non-empty value", fieldDesc, r.Name),
				CodeSpan: r.Name,
				BotHint:  "missing substring for " + r.Name,
				UserHint: fmt.Sprintf("provide a value like `%s=foo`", r.Name),
			}
		}
		return nil
	}
	// `@FuncName` references resolve later; pass through.
	if strings.HasPrefix(r.Name, "@") {
		return nil
	}
	// Unknown rule — reject; hints omitted (the message is the full diagnostic).
	return &richError{
		Msg:      fmt.Sprintf("%s: `%s` is not a known validation rule%s", fieldDesc, r.Name, renamedCaseHint(r.Name)),
		CodeSpan: r.Name,
	}
}

// renamedCaseHint points old bare `lower`/`upper` steps at their split
// replacements (2026-08: `tolower`/`toupper` mods, `islower`/`isupper`
// validators).
func renamedCaseHint(name string) string {
	switch name {
	case "lower", "upper":
		return fmt.Sprintf(" — it split into `to%[1]s` (transform) and `is%[1]s` (validator)", name)
	}
	return ""
}

// KindStruct is NOT skipped — same reasoning as checkValRules.
func checkModRules(mods []ModRule, source string, kind TypeKind, typeName, fieldDesc string) error {
	// Same unknown-name short-circuit as checkValRules.
	anchor := tagAnchor(source)
	var unknown []error
	for _, m := range mods {
		if m.Custom || strings.HasPrefix(m.Name, "@") {
			continue
		}
		if isUnknownMod(m.Name) {
			unknown = append(unknown, &richError{
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
			return &richError{
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
		parts := splitPipeParts(m.Value)
		if len(parts) != 2 || parts[0] == "" {
			return &richError{
				Msg:      fmt.Sprintf("%s: `replace` requires `old|new` form (old cannot be empty)", fieldDesc),
				CodeSpan: "replace=" + m.Value,
				BotHint:  "malformed replace parameter",
				UserHint: "use `replace=old|new`, e.g. `replace=foo|bar`; quote a part containing `|`: `replace='a|b'|c`",
			}
		}
		return nil

	case "clamp":
		if !isNumeric(kind) {
			return mismatchMod(m, source, fieldDesc, typeName,
				"a numeric type",
				"expected numeric type",
				"for strings use `trim` / `replace` instead")
		}
		cparts := splitPipeParts(m.Value)
		if len(cparts) != 2 {
			return &richError{
				Msg:      fmt.Sprintf("%s: `clamp` needs exactly one lo`|`hi separator", fieldDesc),
				CodeSpan: "clamp=" + m.Value,
				BotHint:  "malformed clamp parameter",
				UserHint: "use `clamp=lo|hi`, leave either bound empty for one-sided: `clamp=0|` or `clamp=|100`",
			}
		}
		lo, hi := strings.TrimSpace(cparts[0]), strings.TrimSpace(cparts[1])
		if lo == "" && hi == "" {
			return &richError{
				Msg:      fmt.Sprintf("%s: `clamp` requires at least one of lo or hi", fieldDesc),
				CodeSpan: "clamp",
				BotHint:  "clamp with no bounds",
				UserHint: "provide at least one bound, e.g. `clamp=0|100` or `clamp=|100`",
			}
		}
		checkBound := func(name, v string) error {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return &richError{
					Msg:      fmt.Sprintf("%s: `clamp` %s %q is not a valid number", fieldDesc, name, v),
					CodeSpan: v,
					BotHint:  "non-numeric clamp " + name + " bound",
					UserHint: "both bounds must be numeric, e.g. `clamp=0|100`",
				}
			}
			// Bounds are pasted verbatim into Go comparisons — NaN/Inf have
			// no constant spelling, and a fractional bound truncates against
			// an integer kind; both fail the generated build.
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return &richError{
					Msg:      fmt.Sprintf("%s: `clamp` %s %q — NaN/Inf are not valid bounds", fieldDesc, name, v),
					CodeSpan: v,
					BotHint:  "NaN/Inf bound has no Go constant spelling",
					UserHint: "use finite numeric bounds",
				}
			}
			if isIntegralNumeric(kind) {
				if _, err := parseIntBound(v, kind); err != nil && !errors.Is(err, strconv.ErrRange) {
					return &richError{
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
	// `@FuncName` mods resolve later; pass through.
	if strings.HasPrefix(m.Name, "@") {
		return nil
	}
	// Unknown mod — reject; hint omitted (the message is the full diagnostic).
	return &richError{
		Msg:      fmt.Sprintf("%s: `%s` is not a known mod", fieldDesc, m.Name),
		CodeSpan: m.Name,
	}
}

// mismatch builds the kind-mismatch *richError for validation rules:
// "<Struct>.<Field>: <rule> is inapplicable to <type>".
func mismatch(r ValidationRule, source, fieldDesc, typeName, requiredKind, botHint, userTail string) *richError {
	_ = source
	return &richError{
		Msg:      fmt.Sprintf("%s: `%s` is inapplicable to %s", fieldDesc, r.Name, typeName),
		CodeSpan: r.Name,
		BotHint:  botHint,
		UserHint: fmt.Sprintf("`%s` requires %s; %s", r.Name, requiredKind, userTail),
	}
}

func mismatchMod(m ModRule, source, fieldDesc, typeName, requiredKind, botHint, userTail string) *richError {
	_ = source
	return &richError{
		Msg:      fmt.Sprintf("%s: `%s` is inapplicable to %s", fieldDesc, m.Name, typeName),
		CodeSpan: m.Name,
		BotHint:  botHint,
		UserHint: fmt.Sprintf("`%s` requires %s. %s", m.Name, requiredKind, userTail),
	}
}

// kindIntBits returns (bits, unsigned) for integral kinds, (0, false)
// otherwise.
func kindIntBits(kind TypeKind) (int, bool) {
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

// parseIntBound parses v as an integer constant of the kind's width and sign,
// returning its 64-bit two's-complement pattern (signed kinds sign-extend).
// The error is strconv's: ErrSyntax for a non-integer spelling, ErrRange when
// the value does not fit. Not Atoi: that caps at MaxInt64 and would call every
// uint64 bound from 2^63 up fractional.
func parseIntBound(v string, kind TypeKind) (uint64, error) {
	bits, unsigned := kindIntBits(kind)
	if unsigned {
		// A negative integer is a sign problem, not a spelling one.
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

// numericPartKey is the dedupe key of one numeric `oneof` part: the integer
// itself on an integral kind (an integer-valued float spelling such as `1.0`
// folds onto it), the float64 value otherwise (−0 folded onto 0).
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
// sign — bounds are pasted verbatim into Go comparisons against the field, so
// `uint8 >= -1` or `int8 <= 300` is a constant-overflow compile error in the
// generated file.
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

func boundRangeErr(fieldDesc, ruleSpelling, v string) *richError {
	return &richError{
		Msg:      fmt.Sprintf("%s: `%s` bound %q is out of the field type's range", fieldDesc, ruleSpelling, v),
		CodeSpan: v,
		BotHint:  "out-of-range bound is a constant-overflow compile error in the generated file",
		UserHint: "use a bound the field's type can hold (mind sign for unsigned kinds)",
	}
}

// needNonNegInt is needInt for the length/count rules, where a negative bound
// is nonsense: `maxlen=-1` compiles to a comparison no value can satisfy, so
// the field rejects everything with no diagnostic anywhere.
func needNonNegInt(r ValidationRule, fieldDesc string) error {
	if err := needInt(r, fieldDesc); err != nil {
		return err
	}
	if n, _ := strconv.Atoi(strings.TrimSpace(r.Value)); n < 0 {
		return &richError{
			Msg:      fmt.Sprintf("%s: `%s=%s` requires a non-negative integer", fieldDesc, r.Name, r.Value),
			CodeSpan: r.Name + "=" + r.Value,
			BotHint:  "negative length/count bound",
			UserHint: fmt.Sprintf("lengths and counts are never negative; use `%s=0` or a positive value", r.Name),
		}
	}
	return nil
}

// needInt checks a count parameter (lengths, rune counts) — a plain int, no
// kind involved.
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

func missingIntErr(r ValidationRule, fieldDesc string) *richError {
	return &richError{
		Msg:      fmt.Sprintf("%s: `%s` requires an integer value", fieldDesc, r.Name),
		CodeSpan: r.Name,
		BotHint:  "missing integer parameter",
		UserHint: fmt.Sprintf("provide an integer like `%s=5`", r.Name),
	}
}

func notIntErr(r ValidationRule, fieldDesc string) *richError {
	return &richError{
		Msg:      fmt.Sprintf("%s: `%s=%s` value is not a valid integer", fieldDesc, r.Name, r.Value),
		CodeSpan: r.Name + "=" + r.Value,
		BotHint:  "non-integer parameter for integer rule",
		UserHint: fmt.Sprintf("use a whole-number value like `%s=5` (no decimals, no letters)", r.Name),
	}
}

func needFloat(r ValidationRule, fieldDesc string, kind TypeKind) error {
	v := strings.TrimSpace(r.Value)
	if v == "" {
		return &richError{
			Msg:      fmt.Sprintf("%s: `%s` requires a numeric value", fieldDesc, r.Name),
			CodeSpan: r.Name,
			BotHint:  "missing numeric parameter",
			UserHint: fmt.Sprintf("provide a numeric value like `%s=5` or `%s=1.5`", r.Name, r.Name),
		}
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return &richError{
			Msg:      fmt.Sprintf("%s: `%s=%s` value is not a valid number", fieldDesc, r.Name, r.Value),
			CodeSpan: r.Name + "=" + r.Value,
			BotHint:  "non-numeric parameter for numeric rule",
			UserHint: fmt.Sprintf("use a numeric value like `%s=5` or `%s=1.5`", r.Name, r.Name),
		}
	}
	// The value is pasted verbatim into a Go comparison: NaN/Inf spellings
	// are not Go constants, and a fractional bound against an integer kind
	// is an untyped-float truncation error — both fail the generated build.
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return &richError{
			Msg:      fmt.Sprintf("%s: `%s=%s` — NaN/Inf are not valid bounds", fieldDesc, r.Name, r.Value),
			CodeSpan: r.Name + "=" + r.Value,
			BotHint:  "NaN/Inf bound has no Go constant spelling",
			UserHint: "use a finite numeric bound",
		}
	}
	if isIntegralNumeric(kind) {
		if _, err := parseIntBound(v, kind); err != nil && !errors.Is(err, strconv.ErrRange) {
			return &richError{
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
