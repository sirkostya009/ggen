package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// checkRuleApplicability rejects validation/mod tags whose semantics
// don't make sense for the field's Go kind — e.g. `ascii` on an int,
// `clamp` on a string, `hintlen` on a bool. Without these checks the
// generator silently emits broken Go (`decode.IsASCII(result.N)` for
// an int field) and the user only finds out at compile time on the
// generated file.
//
// Every reject returns a *richError (typed as error) so both loggers
// can render it richly: concise mode appends BotHint in parens for
// agents/CI, pretty mode emits a code excerpt with caret + a Note:
// line carrying UserHint for humans. The two hints are deliberately
// distinct — see log.go's Logger doc.
//
// Called from extractField / extractFieldFromTypes after kind
// resolution. KindStruct fields are skipped: they may be aliases over
// primitives or carry custom marshalers, so we can't decide
// applicability without resolved type info that isn't always
// available in AST-only mode.
func checkRuleApplicability(fi FieldInfo) error {
	// Render as `<StructName>.<GoName>` — fully-qualified Go path that
	// uniquely identifies the field in source, makes the error
	// grep-pable, and matches how Go itself would refer to the
	// field in compile-time errors. Falls back to bare GoName when
	// StructName isn't set (defensive — every code path that builds
	// FieldInfo today populates it).
	desc := fi.GoName
	if fi.StructName != "" {
		desc = fi.StructName + "." + fi.GoName
	}

	// Gather errors across every phase rather than short-circuiting.
	// A field with `ggen:"ascii" mod:"trim"` on an int has TWO bugs;
	// surfacing only the first hides the second until the user fixes
	// it, then re-runs and discovers more — bad UX.
	var errs []error
	collect := func(err error) { errs = append(errs, err) }

	// Outer rules apply to the field itself. For pointer fields the
	// rule operates on the dereferenced pointee, so fi.Kind (which
	// resolveKind populates from PointeeType) is the right anchor.
	collect(checkValRules(fi.Validation, "ggen", fi.Kind, fi.GoType, desc))
	collect(checkModRules(fi.Mods, "mod", fi.Kind, fi.GoType, desc))

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
	collect(checkValRules(fi.KeyValidation, "ggen keys:", KindString, "string", desc+" key"))
	collect(checkModRules(fi.KeyMods, "mod keys:", KindString, "string", desc+" key"))

	// dive: only valid on slice/array/map/[]byte.
	hasDive := len(fi.ElemValidation) > 0 || len(fi.ElemMods) > 0 ||
		len(fi.InnerValidation) > 0 || len(fi.InnerMods) > 0
	if hasDive && !canDive(fi.Kind) {
		collect(&richError{
			Msg:      desc + ": `dive:` tag prefix is only valid on slice/array/map fields (got " + fi.GoType + ")",
			CodeSpan: "dive:",
			BotHint:  "expected slice/array/map field",
			UserHint: "`dive:` only works with slice/array/map",
		})
	}
	collect(checkValRules(fi.ElemValidation, "ggen dive:", fi.ElemKind, fi.ElemType, desc+" element"))
	collect(checkModRules(fi.ElemMods, "mod dive:", fi.ElemKind, fi.ElemType, desc+" element"))

	// hintlen only meaningful on growable containers (slice/map).
	if fi.HintLen >= 0 && fi.Kind != KindSlice && fi.Kind != KindMap {
		collect(&richError{
			Msg:      desc + ": `hintlen` is only valid on slice/map fields (got " + fi.GoType + ")",
			CodeSpan: "hintlen",
			BotHint:  "expected slice or map field",
			UserHint: "`hintlen` is a prealloc capacity hint; only slice/map have capacity to size",
		})
	}

	return errors.Join(errs...)
}

// canDive reports whether dive: can peel one level off this kind.
func canDive(k TypeKind) bool {
	return k == KindSlice || k == KindArray || k == KindMap || k == KindBytes
}

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

// isUnknownValRule reports whether the rule name is NOT in the
// recognized validation-rule set. Keep in sync with the switch in
// checkOneValRule. Used to short-circuit the rest of the rule-list
// checks once a typo is found.
func isUnknownValRule(name string) bool {
	switch name {
	case "", "required", "optional",
		"notempty",
		"len", "minlen", "maxlen",
		"runes", "minrunes", "maxrunes",
		"gt", "gte", "lt", "lte",
		"multiple",
		"eq", "neq", "oneof",
		"email", "url", "ascii", "printable", "alphanum", "numeric",
		"lower", "upper", "hexadecimal",
		"starts", "ends", "contains":
		return false
	}
	return true
}

// isUnknownMod reports whether the mod name is NOT in the recognized
// mod-rule set. Mirror of isUnknownValRule for mods.
func isUnknownMod(name string) bool {
	switch name {
	case "", "trim", "lower", "upper",
		"trimleft", "trimright",
		"replace", "clamp":
		return false
	}
	return true
}

// tagAnchor derives the struct-tag opening sequence from an
// applicability source string. Used as the Anchor for richErrors
// whose CodeSpan is too short to uniquely identify the offending
// position (single-char rule names collide with json tag values).
//
// Source strings are "ggen" / "ggen keys:" / "ggen dive:" /
// "mod" / "mod keys:" / "mod dive:". The first space-separated
// word names the struct tag — `ggen:"...` or `mod:"...` —
// which gives the search a unique fixed prefix to land inside.
func tagAnchor(source string) string {
	if i := strings.IndexByte(source, ' '); i >= 0 {
		source = source[:i]
	}
	if source == "" {
		return ""
	}
	return source + `:"`
}

func checkValRules(rules []ValidationRule, source string, kind TypeKind, typeName, fieldDesc string) error {
	// Opaque kinds: user-declared structs may be aliases or carry
	// custom marshalers we can't introspect at parse time. Skip.
	if kind == KindStruct {
		return nil
	}
	// First pass: collect unknown-rule errors. If any rule name is
	// unrecognized, the user has a typo — surfacing kind-mismatch /
	// bad-parameter diagnostics on the OTHER rules in the same list is
	// noise (the typo'd rule may be hiding context, and a "fix this
	// typo first" UX is cleaner than a 5-line wall of errors).
	//
	// Anchor disambiguates short rule names (single-char `b`, etc.)
	// that would otherwise collide with json tag values earlier on the
	// source line. tagAnchor derives the source-line anchor from the
	// applicability source string (e.g. `ggen:"` for ggen rules,
	// `mod:"` for mod rules), forcing the column search inside the
	// right struct tag.
	anchor := tagAnchor(source)
	var unknown []error
	for _, r := range rules {
		if r.Custom || strings.HasPrefix(r.Name, "@") {
			continue
		}
		if isUnknownValRule(r.Name) {
			unknown = append(unknown, &richError{
				Msg:      fmt.Sprintf("%s: `%s` is not a known validation rule", fieldDesc, r.Name),
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
		return needInt(r, fieldDesc)

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
		return needInt(r, fieldDesc)

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
		return needFloat(r, fieldDesc)

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
		return needInt(r, fieldDesc)

	case "eq", "neq":
		switch {
		case isNumeric(kind):
			return needFloat(r, fieldDesc)
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
			// Pick an example matching the field's actual kind — the
			// generic both-flavor hint forces the user to mentally
			// filter to the half that applies.
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
		if isNumeric(kind) {
			for p := range strings.SplitSeq(r.Value, "|") {
				if _, err := strconv.ParseFloat(strings.TrimSpace(p), 64); err != nil {
					return &richError{
						Msg:      fmt.Sprintf("%s: `oneof=%s` part %q is not a valid number", fieldDesc, r.Value, p),
						CodeSpan: p,
						BotHint:  "non-numeric part in numeric oneof list",
						UserHint: "for numeric fields every `oneof=` part must parse as a number",
					}
				}
			}
		}
		return nil

	case "email", "url", "ascii", "printable", "alphanum", "numeric",
		"lower", "upper", "hexadecimal":
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
	// `@FuncName` references are resolved later in resolveCustomRules
	// — they don't have a static kind/value contract for us to check
	// here, so let them through.
	if strings.HasPrefix(r.Name, "@") {
		return nil
	}
	// Unknown rule name — reject. Hints intentionally omitted: the
	// message itself is the full diagnostic ("`b` is not a known
	// validation rule"); a hint listing every known rule is noise
	// for both humans and bots.
	return &richError{
		Msg:      fmt.Sprintf("%s: `%s` is not a known validation rule", fieldDesc, r.Name),
		CodeSpan: r.Name,
	}
}

func checkModRules(mods []ModRule, source string, kind TypeKind, typeName, fieldDesc string) error {
	if kind == KindStruct {
		return nil
	}
	// Same short-circuit as checkValRules: unknown mod names suppress
	// other mod diagnostics on the same list. See the comment there
	// for the rationale.
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
	case "trim", "lower", "upper":
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
		old, _, ok := strings.Cut(m.Value, "|")
		if !ok || old == "" {
			// Msg refers to the mod by name only (`replace`); the
			// source-line CodeSpan still covers `replace=<value>`
			// so the highlight pins the full broken token. Two
			// distinct concerns: clean prose vs precise pointing.
			return &richError{
				Msg:      fmt.Sprintf("%s: `replace` requires `old|new` form (old cannot be empty)", fieldDesc),
				CodeSpan: "replace=" + m.Value,
				BotHint:  "malformed replace parameter",
				UserHint: "use `replace=old|new`, e.g. `replace=foo|bar`",
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
		lo, hi, ok := strings.Cut(m.Value, "|")
		if !ok {
			return &richError{
				Msg:      fmt.Sprintf("%s: `clamp` is missing the lo`|`hi separator", fieldDesc),
				CodeSpan: "clamp=" + m.Value,
				BotHint:  "malformed clamp parameter",
				UserHint: "use `clamp=lo|hi`, leave either bound empty for one-sided: `clamp=0|` or `clamp=|100`",
			}
		}
		lo, hi = strings.TrimSpace(lo), strings.TrimSpace(hi)
		if lo == "" && hi == "" {
			return &richError{
				Msg:      fmt.Sprintf("%s: `clamp` requires at least one of lo or hi", fieldDesc),
				CodeSpan: "clamp",
				BotHint:  "clamp with no bounds",
				UserHint: "provide at least one bound, e.g. `clamp=0|100` or `clamp=|100`",
			}
		}
		if lo != "" {
			if _, err := strconv.ParseFloat(lo, 64); err != nil {
				return &richError{
					Msg:      fmt.Sprintf("%s: `clamp` lo %q is not a valid number", fieldDesc, lo),
					CodeSpan: lo,
					BotHint:  "non-numeric clamp lo bound",
					UserHint: "both bounds must be numeric, e.g. `clamp=0|100`",
				}
			}
		}
		if hi != "" {
			if _, err := strconv.ParseFloat(hi, 64); err != nil {
				return &richError{
					Msg:      fmt.Sprintf("%s: `clamp` hi %q is not a valid number", fieldDesc, hi),
					CodeSpan: hi,
					BotHint:  "non-numeric clamp hi bound",
					UserHint: "both bounds must be numeric, e.g. `clamp=0|100`",
				}
			}
		}
		return nil
	}
	// `@FuncName` mods resolve later — pass through here.
	if strings.HasPrefix(m.Name, "@") {
		return nil
	}
	// Unknown mod name — reject without a hint (the msg itself is
	// the full diagnostic; listing every known mod is noise).
	return &richError{
		Msg:      fmt.Sprintf("%s: `%s` is not a known mod", fieldDesc, m.Name),
		CodeSpan: m.Name,
	}
}

// mismatch builds the kind-mismatch *richError for validation rules.
// Message shape: "<Struct>.<Field>: <rule> is inapplicable to <type>".
// We drop the "ggen rule" / "mod" tag-source word — the rule name
// itself disambiguates ggen tags from mods (no collisions in the
// rule namespace), and the position prefix already grounds the line.
// requiredKind / botHint / userTail still build the gray hint.
func mismatch(r ValidationRule, source, fieldDesc, typeName, requiredKind, botHint, userTail string) *richError {
	_ = source // dive: / keys: prefixes are encoded in CodeSpan instead
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

func needInt(r ValidationRule, fieldDesc string) error {
	v := strings.TrimSpace(r.Value)
	if v == "" {
		return &richError{
			Msg:      fmt.Sprintf("%s: `%s` requires an integer value", fieldDesc, r.Name),
			CodeSpan: r.Name,
			BotHint:  "missing integer parameter",
			UserHint: fmt.Sprintf("provide an integer like `%s=5`", r.Name),
		}
	}
	if _, err := strconv.Atoi(v); err != nil {
		return &richError{
			Msg:      fmt.Sprintf("%s: `%s=%s` value is not a valid integer", fieldDesc, r.Name, r.Value),
			CodeSpan: r.Name + "=" + r.Value,
			BotHint:  "non-integer parameter for integer rule",
			UserHint: fmt.Sprintf("use a whole-number value like `%s=5` (no decimals, no letters)", r.Name),
		}
	}
	return nil
}

func needFloat(r ValidationRule, fieldDesc string) error {
	v := strings.TrimSpace(r.Value)
	if v == "" {
		return &richError{
			Msg:      fmt.Sprintf("%s: `%s` requires a numeric value", fieldDesc, r.Name),
			CodeSpan: r.Name,
			BotHint:  "missing numeric parameter",
			UserHint: fmt.Sprintf("provide a numeric value like `%s=5` or `%s=1.5`", r.Name, r.Name),
		}
	}
	if _, err := strconv.ParseFloat(v, 64); err != nil {
		return &richError{
			Msg:      fmt.Sprintf("%s: `%s=%s` value is not a valid number", fieldDesc, r.Name, r.Value),
			CodeSpan: r.Name + "=" + r.Value,
			BotHint:  "non-numeric parameter for numeric rule",
			UserHint: fmt.Sprintf("use a numeric value like `%s=5` or `%s=1.5`", r.Name, r.Name),
		}
	}
	return nil
}
