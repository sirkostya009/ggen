package main

import (
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
// Called from extractField / extractFieldFromTypes after kind
// resolution. KindStruct fields are skipped: they may be aliases over
// primitives or carry custom marshalers, so we can't decide
// applicability without resolved type info that isn't always
// available in AST-only mode.
func checkRuleApplicability(fi FieldInfo) error {
	desc := "field " + fi.JSONName

	// Outer rules apply to the field itself. For pointer fields the
	// rule operates on the dereferenced pointee, so fi.Kind (which
	// resolveKind populates from PointeeType) is the right anchor.
	if err := checkValRules(fi.Validation, "ggen", fi.Kind, fi.GoType, desc); err != nil {
		return err
	}
	if err := checkModRules(fi.Mods, "mod", fi.Kind, fi.GoType, desc); err != nil {
		return err
	}

	// keys: only valid on maps. The keyed kind itself is always string.
	if len(fi.KeyValidation) > 0 || len(fi.KeyMods) > 0 {
		if fi.Kind != KindMap {
			return fmt.Errorf("%s: `keys:` tag prefix is only valid on map[string]V fields (got %s)", desc, fi.GoType)
		}
	}
	if err := checkValRules(fi.KeyValidation, "ggen keys:", KindString, "string", desc+" key"); err != nil {
		return err
	}
	if err := checkModRules(fi.KeyMods, "mod keys:", KindString, "string", desc+" key"); err != nil {
		return err
	}

	// dive: only valid on slice / array / map (and []byte, which is a
	// len-able container too).
	hasDive := len(fi.ElemValidation) > 0 || len(fi.ElemMods) > 0 ||
		len(fi.InnerValidation) > 0 || len(fi.InnerMods) > 0
	if hasDive && !canDive(fi.Kind) {
		return fmt.Errorf("%s: `dive:` tag prefix is only valid on slice/array/map fields (got %s)", desc, fi.GoType)
	}
	// Level-0 dive: rules apply to the element kind. Deeper levels
	// (InnerValidation/InnerMods) aren't checked — they'd need a full
	// peel walk over nested container types, and the generator already
	// recurses through them via peelSliceField, surfacing structural
	// problems there.
	if err := checkValRules(fi.ElemValidation, "ggen dive:", fi.ElemKind, fi.ElemType, desc+" element"); err != nil {
		return err
	}
	if err := checkModRules(fi.ElemMods, "mod dive:", fi.ElemKind, fi.ElemType, desc+" element"); err != nil {
		return err
	}

	// hintlen is a sizing hint, only meaningful on growable containers.
	// HintLen == -1 is the "unset" sentinel; >= 0 means user opted in
	// (0 is the explicit "no prealloc" opt-out, also a user choice).
	if fi.HintLen >= 0 && fi.Kind != KindSlice && fi.Kind != KindMap {
		return fmt.Errorf("%s: `hintlen` is only valid on slice/map fields (got %s)", desc, fi.GoType)
	}

	return nil
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
// the generated code for `multiple=N` emits `x % N`, which the Go
// compiler rejects on floats.
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
	// custom marshalers we can't introspect at parse time. Skip rather
	// than false-reject.
	if kind == KindStruct {
		return nil
	}
	for _, r := range rules {
		if r.Custom {
			// resolveCustomRules has already type-checked the signature
			// against the field type; nothing else to verify here.
			continue
		}
		if err := checkOneValRule(r, source, kind, typeName, fieldDesc); err != nil {
			return err
		}
	}
	return nil
}

func checkOneValRule(r ValidationRule, source string, kind TypeKind, typeName, fieldDesc string) error {
	switch r.Name {
	case "", "required", "optional":
		return nil

	case "notempty":
		if !isLenKind(kind) {
			return rejectVal(r, source, fieldDesc, typeName, "expected string/slice/array/map/[]byte")
		}
		return nil

	case "len", "minlen", "maxlen":
		if !isLenKind(kind) {
			return rejectVal(r, source, fieldDesc, typeName, "expected string/slice/array/map/[]byte")
		}
		return needIntParam(r, source, fieldDesc)

	case "runes", "minrunes", "maxrunes":
		if kind != KindString {
			return rejectVal(r, source, fieldDesc, typeName, "rune count requires string")
		}
		return needIntParam(r, source, fieldDesc)

	case "gt", "gte", "lt", "lte":
		if !isNumeric(kind) {
			return rejectVal(r, source, fieldDesc, typeName, "expected numeric type")
		}
		return needFloatParam(r, source, fieldDesc)

	case "multiple":
		if !isIntegralNumeric(kind) {
			return rejectVal(r, source, fieldDesc, typeName, "modulo (multiple=N) only valid on integer types")
		}
		return needIntParam(r, source, fieldDesc)

	case "eq", "neq":
		if kind != KindString && !isNumeric(kind) {
			return rejectVal(r, source, fieldDesc, typeName, "expected string or numeric")
		}
		if isNumeric(kind) {
			return needFloatParam(r, source, fieldDesc)
		}
		return nil

	case "oneof":
		if kind != KindString && !isNumeric(kind) {
			return rejectVal(r, source, fieldDesc, typeName, "expected string or numeric")
		}
		if r.Value == "" {
			return fmt.Errorf("%s: %s rule `oneof` requires a `|`-separated list of allowed values", fieldDesc, source)
		}
		if isNumeric(kind) {
			for _, p := range strings.Split(r.Value, "|") {
				if _, err := strconv.ParseFloat(strings.TrimSpace(p), 64); err != nil {
					return fmt.Errorf("%s: %s rule `oneof=%s` part %q is not a valid number", fieldDesc, source, r.Value, p)
				}
			}
		}
		return nil

	case "email", "url", "ascii", "printable", "alphanum", "numeric",
		"lower", "upper", "hexadecimal":
		if kind != KindString {
			return rejectVal(r, source, fieldDesc, typeName, "expected string")
		}
		return nil

	case "starts", "ends", "contains":
		if kind != KindString {
			return rejectVal(r, source, fieldDesc, typeName, "expected string")
		}
		if r.Value == "" {
			return fmt.Errorf("%s: %s rule `%s` requires a non-empty value", fieldDesc, source, r.Name)
		}
		return nil
	}
	// Unknown rule names stay tolerated for forward-compat with new
	// rules added by callers without a coordinated codegen update.
	return nil
}

func checkModRules(mods []ModRule, source string, kind TypeKind, typeName, fieldDesc string) error {
	if kind == KindStruct {
		return nil
	}
	for _, m := range mods {
		if m.Custom {
			continue
		}
		if err := checkOneModRule(m, source, kind, typeName, fieldDesc); err != nil {
			return err
		}
	}
	return nil
}

func checkOneModRule(m ModRule, source string, kind TypeKind, typeName, fieldDesc string) error {
	switch m.Name {
	case "trim", "lower", "upper":
		if kind != KindString {
			return rejectMod(m, source, fieldDesc, typeName, "expected string")
		}
		return nil

	case "trimleft", "trimright":
		if kind != KindString {
			return rejectMod(m, source, fieldDesc, typeName, "expected string")
		}
		if m.Value == "" {
			return fmt.Errorf("%s: %s mod `%s` requires a non-empty value", fieldDesc, source, m.Name)
		}
		return nil

	case "replace":
		if kind != KindString {
			return rejectMod(m, source, fieldDesc, typeName, "expected string")
		}
		old, _, ok := strings.Cut(m.Value, "|")
		if !ok || old == "" {
			return fmt.Errorf("%s: %s mod `replace=%s` requires `old|new` form (non-empty old)", fieldDesc, source, m.Value)
		}
		return nil

	case "clamp":
		if !isNumeric(kind) {
			return rejectMod(m, source, fieldDesc, typeName, "expected numeric type")
		}
		lo, hi, ok := strings.Cut(m.Value, "|")
		if !ok {
			return fmt.Errorf("%s: %s mod `clamp=%s` requires `lo|hi` form (either bound may be empty)", fieldDesc, source, m.Value)
		}
		lo, hi = strings.TrimSpace(lo), strings.TrimSpace(hi)
		if lo == "" && hi == "" {
			return fmt.Errorf("%s: %s mod `clamp` requires at least one of lo or hi", fieldDesc, source)
		}
		if lo != "" {
			if _, err := strconv.ParseFloat(lo, 64); err != nil {
				return fmt.Errorf("%s: %s mod `clamp` lo %q is not a valid number", fieldDesc, source, lo)
			}
		}
		if hi != "" {
			if _, err := strconv.ParseFloat(hi, 64); err != nil {
				return fmt.Errorf("%s: %s mod `clamp` hi %q is not a valid number", fieldDesc, source, hi)
			}
		}
		return nil
	}
	return nil
}

func rejectVal(r ValidationRule, source, fieldDesc, typeName, reason string) error {
	return fmt.Errorf("%s: %s rule %q cannot be applied to %s (%s)",
		fieldDesc, source, r.Name, typeName, reason)
}

func rejectMod(m ModRule, source, fieldDesc, typeName, reason string) error {
	return fmt.Errorf("%s: %s mod %q cannot be applied to %s (%s)",
		fieldDesc, source, m.Name, typeName, reason)
}

func needIntParam(r ValidationRule, source, fieldDesc string) error {
	v := strings.TrimSpace(r.Value)
	if v == "" {
		return fmt.Errorf("%s: %s rule `%s` requires an integer value", fieldDesc, source, r.Name)
	}
	if _, err := strconv.Atoi(v); err != nil {
		return fmt.Errorf("%s: %s rule `%s=%s` value is not a valid integer", fieldDesc, source, r.Name, r.Value)
	}
	return nil
}

func needFloatParam(r ValidationRule, source, fieldDesc string) error {
	v := strings.TrimSpace(r.Value)
	if v == "" {
		return fmt.Errorf("%s: %s rule `%s` requires a numeric value", fieldDesc, source, r.Name)
	}
	if _, err := strconv.ParseFloat(v, 64); err != nil {
		return fmt.Errorf("%s: %s rule `%s=%s` value is not a valid number", fieldDesc, source, r.Name, r.Value)
	}
	return nil
}
