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
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

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
				UserHint: "Remove the `keys:` prefix, or change the field to a `map[string]V`.",
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
			UserHint: "Remove the `dive:` prefix, or change the field to a slice/array/map.",
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
			UserHint: "Remove `hintlen`, or move it to a slice/map field. (`hintlen` is a prealloc capacity hint; nothing to size on scalars.)",
		})
	}

	if len(errs) == 0 {
		return nil
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

func checkValRules(rules []ValidationRule, source string, kind TypeKind, typeName, fieldDesc string) error {
	// Opaque kinds: user-declared structs may be aliases or carry
	// custom marshalers we can't introspect at parse time. Skip.
	if kind == KindStruct {
		return nil
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
	if len(errs) == 0 {
		return nil
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
				"Drop the rule or change the field to a len-able type.")
		}
		return nil

	case "len", "minlen", "maxlen":
		if !isLenKind(kind) {
			return mismatch(r, source, fieldDesc, typeName,
				"a string/slice/array/map/[]byte",
				"expected string/slice/array/map/[]byte",
				"For numeric bounds use `gt`/`gte`/`lt`/`lte` instead.")
		}
		return needInt(r, source, fieldDesc)

	case "runes", "minrunes", "maxrunes":
		if kind != KindString {
			return mismatch(r, source, fieldDesc, typeName,
				"a string",
				"rune count requires string",
				"For byte length use `len`/`minlen`/`maxlen`.")
		}
		return needInt(r, source, fieldDesc)

	case "gt", "gte", "lt", "lte":
		if !isNumeric(kind) {
			return mismatch(r, source, fieldDesc, typeName,
				"a numeric type",
				"expected numeric type",
				"For string/length bounds use the rule appropriate for this kind.")
		}
		return needFloat(r, source, fieldDesc)

	case "multiple":
		if !isIntegralNumeric(kind) {
			return mismatch(r, source, fieldDesc, typeName,
				"an integer type",
				"modulo (multiple=N) only valid on integer types",
				"Use `gt`/`gte`/`lt`/`lte` for non-integer bounds, or change the field to an integer kind.")
		}
		return needInt(r, source, fieldDesc)

	case "eq", "neq":
		if kind != KindString && !isNumeric(kind) {
			return mismatch(r, source, fieldDesc, typeName,
				"a string or numeric type",
				"expected string or numeric",
				"Drop the rule or use a custom `@FuncName` validator.")
		}
		if isNumeric(kind) {
			return needFloat(r, source, fieldDesc)
		}
		return nil

	case "oneof":
		if kind != KindString && !isNumeric(kind) {
			return mismatch(r, source, fieldDesc, typeName,
				"a string or numeric type",
				"expected string or numeric",
				"Drop the rule or use a custom `@FuncName` validator.")
		}
		if r.Value == "" {
			return &richError{
				Msg:      fmt.Sprintf("%s: `oneof` requires a `|`-separated list of allowed values", fieldDesc),
				CodeSpan: "oneof",
				BotHint:  "empty oneof list",
				UserHint: "Provide values like `oneof=admin|user|guest` (string) or `oneof=1|2|3` (numeric).",
			}
		}
		if isNumeric(kind) {
			for _, p := range strings.Split(r.Value, "|") {
				if _, err := strconv.ParseFloat(strings.TrimSpace(p), 64); err != nil {
					return &richError{
						Msg:      fmt.Sprintf("%s: `oneof=%s` part %q is not a valid number", fieldDesc, r.Value, p),
						CodeSpan: p,
						BotHint:  "non-numeric part in numeric oneof list",
						UserHint: "For numeric fields every `oneof=` part must parse as a number.",
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
				"Drop the rule or change the field to `string`.")
		}
		return nil

	case "starts", "ends", "contains":
		if kind != KindString {
			return mismatch(r, source, fieldDesc, typeName,
				"a string",
				"expected string",
				"Drop the rule or change the field to `string`.")
		}
		if r.Value == "" {
			return &richError{
				Msg:      fmt.Sprintf("%s: `%s` requires a non-empty value", fieldDesc, r.Name),
				CodeSpan: r.Name,
				BotHint:  "missing substring for " + r.Name,
				UserHint: fmt.Sprintf("Provide a value like `%s=foo`.", r.Name),
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
	var errs []error
	for _, m := range mods {
		if m.Custom {
			continue
		}
		if err := checkOneModRule(m, source, kind, typeName, fieldDesc); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil
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
				"Drop the mod or change the field to `string`.")
		}
		return nil

	case "trimleft", "trimright":
		if kind != KindString {
			return mismatchMod(m, source, fieldDesc, typeName,
				"a string",
				"expected string",
				"Drop the mod or change the field to `string`.")
		}
		if m.Value == "" {
			return &richError{
				Msg:      fmt.Sprintf("%s: `%s` requires a non-empty value", fieldDesc, m.Name),
				CodeSpan: m.Name,
				BotHint:  "missing prefix/suffix to trim",
				UserHint: fmt.Sprintf("Provide the substring to strip, e.g. `%s=SKU-`.", m.Name),
			}
		}
		return nil

	case "replace":
		if kind != KindString {
			return mismatchMod(m, source, fieldDesc, typeName,
				"a string",
				"expected string",
				"Drop the mod or change the field to `string`.")
		}
		old, _, ok := strings.Cut(m.Value, "|")
		if !ok || old == "" {
			// Msg refers to the mod by name only (`replace`); the
			// source-line CodeSpan still covers `replace=<value>`
			// so the highlight pins the full broken token. Two
			// distinct concerns: clean prose vs precise pointing.
			return &richError{
				Msg:      fmt.Sprintf("%s: `replace` requires `old|new` form (non-empty old)", fieldDesc),
				CodeSpan: "replace=" + m.Value,
				BotHint:  "malformed replace parameter",
				UserHint: "Use `replace=old|new`, e.g. `replace=foo|bar`.",
			}
		}
		return nil

	case "clamp":
		if !isNumeric(kind) {
			return mismatchMod(m, source, fieldDesc, typeName,
				"a numeric type",
				"expected numeric type",
				"Drop the mod; for strings use `trim` / `replace` instead.")
		}
		lo, hi, ok := strings.Cut(m.Value, "|")
		if !ok {
			return &richError{
				Msg:      fmt.Sprintf("%s: `clamp` is missing the `lo|hi` separator", fieldDesc),
				CodeSpan: "clamp=" + m.Value,
				BotHint:  "malformed clamp parameter",
				UserHint: "Use `clamp=lo|hi`. Leave either bound empty for one-sided: `clamp=0|` or `clamp=|100`.",
			}
		}
		lo, hi = strings.TrimSpace(lo), strings.TrimSpace(hi)
		if lo == "" && hi == "" {
			return &richError{
				Msg:      fmt.Sprintf("%s: `clamp` requires at least one of lo or hi", fieldDesc),
				CodeSpan: "clamp",
				BotHint:  "clamp with no bounds",
				UserHint: "Provide at least one bound, e.g. `clamp=0|100` or `clamp=|100`.",
			}
		}
		if lo != "" {
			if _, err := strconv.ParseFloat(lo, 64); err != nil {
				return &richError{
					Msg:      fmt.Sprintf("%s: `clamp` lo %q is not a valid number", fieldDesc, lo),
					CodeSpan: lo,
					BotHint:  "non-numeric clamp lo bound",
					UserHint: "Both bounds must be numeric, e.g. `clamp=0|100`.",
				}
			}
		}
		if hi != "" {
			if _, err := strconv.ParseFloat(hi, 64); err != nil {
				return &richError{
					Msg:      fmt.Sprintf("%s: `clamp` hi %q is not a valid number", fieldDesc, hi),
					CodeSpan: hi,
					BotHint:  "non-numeric clamp hi bound",
					UserHint: "Both bounds must be numeric, e.g. `clamp=0|100`.",
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
		UserHint: fmt.Sprintf("`%s` requires %s. %s", r.Name, requiredKind, userTail),
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

func needInt(r ValidationRule, source, fieldDesc string) error {
	v := strings.TrimSpace(r.Value)
	if v == "" {
		return &richError{
			Msg:      fmt.Sprintf("%s: `%s` requires an integer value", fieldDesc, r.Name),
			CodeSpan: r.Name,
			BotHint:  "missing integer parameter",
			UserHint: fmt.Sprintf("Provide an integer like `%s=5`.", r.Name),
		}
	}
	if _, err := strconv.Atoi(v); err != nil {
		return &richError{
			Msg:      fmt.Sprintf("%s: `%s=%s` value is not a valid integer", fieldDesc, r.Name, r.Value),
			CodeSpan: r.Name + "=" + r.Value,
			BotHint:  "non-integer parameter for integer rule",
			UserHint: fmt.Sprintf("Use a whole-number value like `%s=5` (no decimals, no letters).", r.Name),
		}
	}
	return nil
}

func needFloat(r ValidationRule, source, fieldDesc string) error {
	v := strings.TrimSpace(r.Value)
	if v == "" {
		return &richError{
			Msg:      fmt.Sprintf("%s: `%s` requires a numeric value", fieldDesc, r.Name),
			CodeSpan: r.Name,
			BotHint:  "missing numeric parameter",
			UserHint: fmt.Sprintf("Provide a numeric value like `%s=5` or `%s=1.5`.", r.Name, r.Name),
		}
	}
	if _, err := strconv.ParseFloat(v, 64); err != nil {
		return &richError{
			Msg:      fmt.Sprintf("%s: `%s=%s` value is not a valid number", fieldDesc, r.Name, r.Value),
			CodeSpan: r.Name + "=" + r.Value,
			BotHint:  "non-numeric parameter for numeric rule",
			UserHint: fmt.Sprintf("Use a numeric value like `%s=5` or `%s=1.5`.", r.Name, r.Name),
		}
	}
	return nil
}

// needIntParam and needFloatParam are legacy aliases kept for the
// applicability_test.go shape. The "Param" suffix predates the
// richError refactor — new code should call needInt / needFloat.
func needIntParam(r ValidationRule, source, fieldDesc string) error {
	return needInt(r, source, fieldDesc)
}

func needFloatParam(r ValidationRule, source, fieldDesc string) error {
	return needFloat(r, source, fieldDesc)
}
