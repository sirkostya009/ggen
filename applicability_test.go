package main

import (
	"strings"
	"testing"
)

// Exhaustive unit tests for applicability.go. Two matrices drive the
// bulk: every val rule × every kind, and every mod rule × every kind.
// Per-rule shape errors (missing value, bad numeric, wrong form) are
// covered by dedicated tables. The orchestrator's structural checks
// (keys: on non-map, dive: on non-container, hintlen on non-slice/map,
// KindStruct skip) get their own subtest driven through FieldInfo.

// kindEntry is one row of the kind matrix.
type kindEntry struct {
	kind TypeKind
	name string // human label for failure messages; mirrors a Go type literal
}

// allKindEntries covers every TypeKind the field-level resolver can
// emit. KindStruct is included as a separate row so the "skip when
// opaque" path gets explicit coverage.
var allKindEntries = []kindEntry{
	{KindString, "string"},
	{KindBool, "bool"},
	{KindInt, "int"},
	{KindInt8, "int8"},
	{KindInt16, "int16"},
	{KindInt32, "int32"},
	{KindInt64, "int64"},
	{KindUint, "uint"},
	{KindUint8, "uint8"},
	{KindUint16, "uint16"},
	{KindUint32, "uint32"},
	{KindUint64, "uint64"},
	{KindFloat32, "float32"},
	{KindFloat64, "float64"},
	{KindBytes, "[]byte"},
	{KindSlice, "[]int"},
	{KindArray, "[3]int"},
	{KindMap, "map[string]int"},
	{KindTime, "time.Time"},
	{KindDuration, "time.Duration"},
	{KindNetIP, "net.IP"},
	{KindNetipAddr, "netip.Addr"},
	{KindNetipPrefix, "netip.Prefix"},
	{KindURL, "url.URL"},
	{KindBigInt, "big.Int"},
	{KindBigFloat, "big.Float"},
	{KindBigRat, "big.Rat"},
	{KindRawJSON, "json.RawMessage"},
	{KindAny, "any"},
	{KindSQLNull, "sql.NullString"},
}

// Acceptance predicates, declared once so the per-rule tables stay
// self-documenting (`accept: stringOnly` reads better than an inline
// lambda).
var (
	anyKind         = func(k TypeKind) bool { return true }
	stringOnly      = func(k TypeKind) bool { return k == KindString }
	numericOnly     = isNumeric
	integerOnly     = isIntegralNumeric
	lenable         = isLenKind
	stringOrNumeric = func(k TypeKind) bool { return k == KindString || isNumeric(k) }
)

// valSpec describes a validation rule + a sample value that should
// satisfy the rule's value-shape check for every accepted kind. Use a
// neutral value (`5`, `a|b|c`, `foo`) so the kind matrix never trips
// over value-shape rejection — that's a separate dimension covered by
// TestCheckOneValRule_ValueShape.
type valSpec struct {
	name   string
	value  string
	accept func(TypeKind) bool
}

var valSpecs = []valSpec{
	{"required", "", anyKind},
	{"optional", "", anyKind},
	{"notempty", "", lenable},
	{"len", "5", lenable},
	{"minlen", "1", lenable},
	{"maxlen", "10", lenable},
	{"runes", "3", stringOnly},
	{"minrunes", "1", stringOnly},
	{"maxrunes", "5", stringOnly},
	{"gt", "0", numericOnly},
	{"gte", "0", numericOnly},
	{"lt", "100", numericOnly},
	{"lte", "100", numericOnly},
	{"multiple", "2", integerOnly},
	// eq/neq are string-or-numeric. Use a value that's valid as a
	// numeric literal AND as a string literal so neither path trips.
	{"eq", "5", stringOrNumeric},
	{"neq", "5", stringOrNumeric},
	// oneof: numeric parts must be parseable; "1|2|3" works for both
	// string and numeric kinds.
	{"oneof", "1|2|3", stringOrNumeric},
	{"email", "", stringOnly},
	{"url", "", stringOnly},
	{"ascii", "", stringOnly},
	{"printable", "", stringOnly},
	{"alphanum", "", stringOnly},
	{"numeric", "", stringOnly},
	{"lower", "", stringOnly},
	{"upper", "", stringOnly},
	{"hexadecimal", "", stringOnly},
	{"starts", "foo", stringOnly},
	{"ends", "foo", stringOnly},
	{"contains", "foo", stringOnly},
}

func TestCheckOneValRule_KindMatrix(t *testing.T) {
	t.Parallel()
	for _, spec := range valSpecs {
		for _, ke := range allKindEntries {
			name := spec.name + "/" + ke.name
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				rule := ValidationRule{Name: spec.name, Value: spec.value}
				err := checkOneValRule(rule, "ggen", ke.kind, ke.name, "field f")
				accepted := spec.accept(ke.kind)
				if accepted && err != nil {
					t.Errorf("rule=%q kind=%s: expected accept, got: %v", spec.name, ke.name, err)
				}
				if !accepted && err == nil {
					t.Errorf("rule=%q kind=%s: expected reject, got nil", spec.name, ke.name)
				}
				if !accepted && err != nil {
					// Reject path must reference the rule name and field type.
					if !strings.Contains(err.Error(), spec.name) {
						t.Errorf("rule=%q kind=%s: diagnostic missing rule name: %v", spec.name, ke.name, err)
					}
					if !strings.Contains(err.Error(), ke.name) {
						t.Errorf("rule=%q kind=%s: diagnostic missing type name: %v", spec.name, ke.name, err)
					}
				}
			})
		}
	}
}

// TestCheckOneValRule_UnknownRuleRejected pins the new behavior:
// unrecognised rule names are flagged loudly, not silently no-op'd.
// A user typing `ggen:"b"` should get told `b` isn't a real rule —
// the alternative (silent acceptance) hides the typo until the
// generated code runs without the expected validation.
//
// Custom `@FuncName` rules are excluded — those resolve later in
// resolveCustomRules and have no static kind contract to check.
func TestCheckOneValRule_UnknownRuleRejected(t *testing.T) {
	t.Parallel()
	for _, ke := range allKindEntries {
		err := checkOneValRule(ValidationRule{Name: "futureRule", Value: "x"},
			"ggen", ke.kind, ke.name, "field f")
		if err == nil {
			t.Errorf("unknown rule on kind=%s should error, got nil", ke.name)
			continue
		}
		if !strings.Contains(err.Error(), "is not a known validation rule") {
			t.Errorf("unknown-rule diagnostic missing on kind=%s: %v", ke.name, err)
		}
	}
}

// TestCheckOneValRule_CustomAtPrefixTolerated pins the @ escape:
// `@FuncName` references survive the unknown-rule check and reach
// resolveCustomRules unimpeded.
func TestCheckOneValRule_CustomAtPrefixTolerated(t *testing.T) {
	t.Parallel()
	for _, ke := range allKindEntries {
		err := checkOneValRule(ValidationRule{Name: "@MyCheck"},
			"ggen", ke.kind, ke.name, "field f")
		if err != nil {
			t.Errorf("@FuncName on kind=%s should pass, got: %v", ke.name, err)
		}
	}
}

// TestCheckOneValRule_ValueShape covers per-rule value-shape errors
// independently of kind matching. Each row uses a kind that ACCEPTS
// the rule, so only the value-shape branch can fail.
func TestCheckOneValRule_ValueShape(t *testing.T) {
	t.Parallel()
	type tc struct {
		name     string
		rule     ValidationRule
		kind     TypeKind
		wantSub  string // substring expected in err.Error(); empty = expect nil
		typeName string
	}
	cases := []tc{
		// len/minlen/maxlen: integer required.
		{"len_good", ValidationRule{Name: "len", Value: "5"}, KindString, "", "string"},
		{"len_empty", ValidationRule{Name: "len"}, KindString, "requires an integer value", "string"},
		{"len_non_numeric", ValidationRule{Name: "len", Value: "abc"}, KindString, "value is not a valid integer", "string"},
		{"len_float", ValidationRule{Name: "len", Value: "1.5"}, KindString, "value is not a valid integer", "string"},
		{"len_whitespace", ValidationRule{Name: "len", Value: " 5 "}, KindString, "", "string"},
		{"len_negative", ValidationRule{Name: "len", Value: "-3"}, KindString, "", "string"},
		{"minlen_empty", ValidationRule{Name: "minlen"}, KindString, "requires an integer value", "string"},
		{"maxlen_non_numeric", ValidationRule{Name: "maxlen", Value: "x"}, KindSlice, "is not a valid integer", "[]int"},

		// runes/minrunes/maxrunes: integer required.
		{"runes_good", ValidationRule{Name: "runes", Value: "3"}, KindString, "", "string"},
		{"runes_empty", ValidationRule{Name: "runes"}, KindString, "requires an integer value", "string"},
		{"runes_bad", ValidationRule{Name: "minrunes", Value: "abc"}, KindString, "value is not a valid integer", "string"},

		// gt/gte/lt/lte: float required.
		{"gt_good", ValidationRule{Name: "gt", Value: "1"}, KindInt, "", "int"},
		{"gt_float_ok", ValidationRule{Name: "gt", Value: "1.5"}, KindFloat64, "", "float64"},
		{"gt_empty", ValidationRule{Name: "gt"}, KindInt, "requires a numeric value", "int"},
		{"gt_bad", ValidationRule{Name: "gt", Value: "abc"}, KindInt, "value is not a valid number", "int"},
		{"lte_bad", ValidationRule{Name: "lte", Value: "abc"}, KindInt, "value is not a valid number", "int"},

		// multiple: integer required.
		{"multiple_good", ValidationRule{Name: "multiple", Value: "2"}, KindInt, "", "int"},
		{"multiple_empty", ValidationRule{Name: "multiple"}, KindInt, "requires an integer value", "int"},
		{"multiple_bad", ValidationRule{Name: "multiple", Value: "abc"}, KindInt, "value is not a valid integer", "int"},

		// eq/neq numeric: must be valid number; string: any value OK.
		{"eq_str_any_value", ValidationRule{Name: "eq", Value: "abc"}, KindString, "", "string"},
		{"eq_int_bad", ValidationRule{Name: "eq", Value: "abc"}, KindInt, "value is not a valid number", "int"},
		{"eq_int_empty", ValidationRule{Name: "eq"}, KindInt, "requires a numeric value", "int"},
		{"neq_int_bad", ValidationRule{Name: "neq", Value: "x"}, KindInt, "value is not a valid number", "int"},

		// oneof: non-empty list; numeric kind requires numeric parts.
		{"oneof_str_any", ValidationRule{Name: "oneof", Value: "a|b|c"}, KindString, "", "string"},
		{"oneof_empty", ValidationRule{Name: "oneof"}, KindString, "requires a", "string"},
		{"oneof_num_good", ValidationRule{Name: "oneof", Value: "1|2|3"}, KindInt, "", "int"},
		{"oneof_num_bad_part", ValidationRule{Name: "oneof", Value: "1|two|3"}, KindInt, `part "two" is not a valid number`, "int"},
		{"oneof_num_trailing", ValidationRule{Name: "oneof", Value: " 1 | 2 "}, KindInt, "", "int"},

		// starts/ends/contains: non-empty value required.
		{"starts_good", ValidationRule{Name: "starts", Value: "x"}, KindString, "", "string"},
		{"starts_empty", ValidationRule{Name: "starts"}, KindString, "requires a non-empty value", "string"},
		{"ends_empty", ValidationRule{Name: "ends"}, KindString, "requires a non-empty value", "string"},
		{"contains_empty", ValidationRule{Name: "contains"}, KindString, "requires a non-empty value", "string"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := checkOneValRule(c.rule, "ggen", c.kind, c.typeName, "field f")
			if c.wantSub == "" {
				if err != nil {
					t.Errorf("expected nil, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wantSub)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), c.wantSub)
			}
		})
	}
}

// ----- mod rule matrix -----

type modSpec struct {
	name   string
	value  string
	accept func(TypeKind) bool
}

var modSpecs = []modSpec{
	{"trim", "", stringOnly},
	{"lower", "", stringOnly},
	{"upper", "", stringOnly},
	{"trimleft", "foo", stringOnly},
	{"trimright", "bar", stringOnly},
	{"replace", "a|b", stringOnly},
	{"clamp", "0|10", numericOnly},
}

func TestCheckOneModRule_KindMatrix(t *testing.T) {
	t.Parallel()
	for _, spec := range modSpecs {
		for _, ke := range allKindEntries {
			name := spec.name + "/" + ke.name
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				m := ModRule{Name: spec.name, Value: spec.value}
				err := checkOneModRule(m, "mod", ke.kind, ke.name, "field f")
				accepted := spec.accept(ke.kind)
				if accepted && err != nil {
					t.Errorf("mod=%q kind=%s: expected accept, got: %v", spec.name, ke.name, err)
				}
				if !accepted && err == nil {
					t.Errorf("mod=%q kind=%s: expected reject, got nil", spec.name, ke.name)
				}
				if !accepted && err != nil {
					if !strings.Contains(err.Error(), spec.name) {
						t.Errorf("mod=%q kind=%s: diagnostic missing mod name: %v", spec.name, ke.name, err)
					}
					if !strings.Contains(err.Error(), ke.name) {
						t.Errorf("mod=%q kind=%s: diagnostic missing type name: %v", spec.name, ke.name, err)
					}
				}
			})
		}
	}
}

// TestCheckOneModRule_UnknownModRejected mirrors the validator
// side: unrecognised mod names error out with a typo-catching
// diagnostic. `@FuncName` mods (custom-resolved later) are exempt.
func TestCheckOneModRule_UnknownModRejected(t *testing.T) {
	t.Parallel()
	for _, ke := range allKindEntries {
		err := checkOneModRule(ModRule{Name: "futureMod", Value: "x"},
			"mod", ke.kind, ke.name, "field f")
		if err == nil {
			t.Errorf("unknown mod on kind=%s should error, got nil", ke.name)
			continue
		}
		if !strings.Contains(err.Error(), "is not a known mod") {
			t.Errorf("unknown-mod diagnostic missing on kind=%s: %v", ke.name, err)
		}
	}
}

func TestCheckOneModRule_CustomAtPrefixTolerated(t *testing.T) {
	t.Parallel()
	for _, ke := range allKindEntries {
		err := checkOneModRule(ModRule{Name: "@MyMod"},
			"mod", ke.kind, ke.name, "field f")
		if err != nil {
			t.Errorf("@FuncName on kind=%s should pass, got: %v", ke.name, err)
		}
	}
}

func TestCheckOneModRule_ValueShape(t *testing.T) {
	t.Parallel()
	type tc struct {
		name    string
		mod     ModRule
		kind    TypeKind
		wantSub string
	}
	cases := []tc{
		// trim/lower/upper take no value — any value (or none) is accepted.
		{"trim_no_value", ModRule{Name: "trim"}, KindString, ""},
		{"lower_no_value", ModRule{Name: "lower"}, KindString, ""},
		{"upper_no_value", ModRule{Name: "upper"}, KindString, ""},

		// trimleft/trimright require non-empty value.
		{"trimleft_good", ModRule{Name: "trimleft", Value: "X"}, KindString, ""},
		{"trimleft_empty", ModRule{Name: "trimleft"}, KindString, "requires a non-empty value"},
		{"trimright_empty", ModRule{Name: "trimright"}, KindString, "requires a non-empty value"},

		// replace requires "old|new" with non-empty old.
		{"replace_good", ModRule{Name: "replace", Value: "old|new"}, KindString, ""},
		{"replace_empty_new_ok", ModRule{Name: "replace", Value: "old|"}, KindString, ""},
		{"replace_no_pipe", ModRule{Name: "replace", Value: "foo"}, KindString, "requires `old|new` form"},
		{"replace_empty_old", ModRule{Name: "replace", Value: "|new"}, KindString, "requires `old|new` form"},
		{"replace_empty_value", ModRule{Name: "replace"}, KindString, "requires `old|new` form"},

		// clamp requires "lo|hi"; at least one bound must be present; each
		// bound must be a valid number.
		{"clamp_good", ModRule{Name: "clamp", Value: "0|10"}, KindInt, ""},
		{"clamp_lo_only", ModRule{Name: "clamp", Value: "0|"}, KindInt, ""},
		{"clamp_hi_only", ModRule{Name: "clamp", Value: "|10"}, KindInt, ""},
		{"clamp_no_pipe", ModRule{Name: "clamp", Value: "10"}, KindInt, "is missing the lo`|`hi separator"},
		{"clamp_both_empty", ModRule{Name: "clamp", Value: "|"}, KindInt, "requires at least one of lo or hi"},
		{"clamp_bad_lo", ModRule{Name: "clamp", Value: "abc|10"}, KindInt, `lo "abc" is not a valid number`},
		{"clamp_bad_hi", ModRule{Name: "clamp", Value: "0|abc"}, KindInt, `hi "abc" is not a valid number`},
		{"clamp_float_ok", ModRule{Name: "clamp", Value: "0.5|10.5"}, KindFloat64, ""},
		{"clamp_whitespace", ModRule{Name: "clamp", Value: " 0 | 10 "}, KindInt, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := checkOneModRule(c.mod, "mod", c.kind, "T", "field f")
			if c.wantSub == "" {
				if err != nil {
					t.Errorf("expected nil, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wantSub)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), c.wantSub)
			}
		})
	}
}

// ----- skip-paths -----

// TestCheckValRules_KindStructSkipped: KindStruct fields opt out of the
// applicability check entirely (alias / custom-marshaler ambiguity).
func TestCheckValRules_KindStructSkipped(t *testing.T) {
	t.Parallel()
	// Every kind-mismatched rule below would normally reject on its
	// "right" kind. With kind=KindStruct they all pass.
	rules := []ValidationRule{
		{Name: "ascii"},
		{Name: "email"},
		{Name: "gt", Value: "5"},
		{Name: "multiple", Value: "2"},
		{Name: "len", Value: "5"},
		{Name: "minlen", Value: "abc"}, // even bad value should be ignored
		{Name: "oneof", Value: "a|b"},
	}
	if err := checkValRules(rules, "ggen", KindStruct, "Foo", "field f"); err != nil {
		t.Errorf("KindStruct must skip all val rules, got: %v", err)
	}
}

// TestCheckModRules_KindStructSkipped mirrors the val-side skip.
func TestCheckModRules_KindStructSkipped(t *testing.T) {
	t.Parallel()
	mods := []ModRule{
		{Name: "trim"},
		{Name: "clamp", Value: "0|10"},
		{Name: "clamp", Value: "garbage"}, // bad shape ignored under struct
		{Name: "replace", Value: "no-pipe"},
	}
	if err := checkModRules(mods, "mod", KindStruct, "Foo", "field f"); err != nil {
		t.Errorf("KindStruct must skip all mod rules, got: %v", err)
	}
}

// TestCheckValRules_CustomSkipped: @Func rules bypass the matrix —
// resolveCustomRules has already type-checked the signature against
// the field's exact go/types.Type.
func TestCheckValRules_CustomSkipped(t *testing.T) {
	t.Parallel()
	rules := []ValidationRule{
		// Kind=int but rule name says "ascii" — would normally reject.
		// Custom=true makes it skip the matrix entirely.
		{Name: "@MyCheck", Custom: true, FuncName: "MyCheck"},
	}
	if err := checkValRules(rules, "ggen", KindInt, "int", "field f"); err != nil {
		t.Errorf("Custom val rule must skip matrix, got: %v", err)
	}
}

func TestCheckModRules_CustomSkipped(t *testing.T) {
	t.Parallel()
	mods := []ModRule{
		{Name: "@MyMod", Custom: true, FuncName: "MyMod"},
	}
	if err := checkModRules(mods, "mod", KindInt, "int", "field f"); err != nil {
		t.Errorf("Custom mod must skip matrix, got: %v", err)
	}
}

// ----- helper-predicate sanity -----

func TestCanDive(t *testing.T) {
	t.Parallel()
	accepted := map[TypeKind]bool{
		KindSlice: true, KindArray: true, KindMap: true, KindBytes: true,
	}
	for _, ke := range allKindEntries {
		got := canDive(ke.kind)
		want := accepted[ke.kind]
		if got != want {
			t.Errorf("canDive(%s) = %v, want %v", ke.name, got, want)
		}
	}
}

func TestIsLenKind(t *testing.T) {
	t.Parallel()
	accepted := map[TypeKind]bool{
		KindString: true, KindSlice: true, KindArray: true,
		KindMap: true, KindBytes: true,
	}
	for _, ke := range allKindEntries {
		got := isLenKind(ke.kind)
		want := accepted[ke.kind]
		if got != want {
			t.Errorf("isLenKind(%s) = %v, want %v", ke.name, got, want)
		}
	}
}

func TestIsIntegralNumeric(t *testing.T) {
	t.Parallel()
	accepted := map[TypeKind]bool{
		KindInt: true, KindInt8: true, KindInt16: true, KindInt32: true, KindInt64: true,
		KindUint: true, KindUint8: true, KindUint16: true, KindUint32: true, KindUint64: true,
	}
	for _, ke := range allKindEntries {
		got := isIntegralNumeric(ke.kind)
		want := accepted[ke.kind]
		if got != want {
			t.Errorf("isIntegralNumeric(%s) = %v, want %v", ke.name, got, want)
		}
	}
}

// ----- needInt / needFloat -----

func TestNeedInt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		value   string
		wantSub string // empty = nil expected
	}{
		{"5", ""},
		{" 5 ", ""},
		{"-7", ""},
		{"0", ""},
		{"", "requires an integer value"},
		{"   ", "requires an integer value"},
		{"abc", "value is not a valid integer"},
		{"1.5", "value is not a valid integer"},
		{"5x", "value is not a valid integer"},
	}
	for _, c := range cases {
		t.Run("v="+c.value, func(t *testing.T) {
			t.Parallel()
			err := needInt(ValidationRule{Name: "len", Value: c.value}, "field f")
			if c.wantSub == "" {
				if err != nil {
					t.Errorf("expected nil, got: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("got %v, want substring %q", err, c.wantSub)
			}
		})
	}
}

func TestNeedFloat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		value   string
		wantSub string
	}{
		{"5", ""},
		{"5.5", ""},
		{"-1e3", ""},
		{" 5.0 ", ""},
		{"", "requires a numeric value"},
		{"abc", "value is not a valid number"},
		{"5..0", "value is not a valid number"},
	}
	for _, c := range cases {
		t.Run("v="+c.value, func(t *testing.T) {
			t.Parallel()
			err := needFloat(ValidationRule{Name: "gt", Value: c.value}, "field f")
			if c.wantSub == "" {
				if err != nil {
					t.Errorf("expected nil, got: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("got %v, want substring %q", err, c.wantSub)
			}
		})
	}
}

// ----- orchestrator: keys / dive / hintlen structural checks -----

// TestCheckRuleApplicability_Structural drives checkRuleApplicability
// through FieldInfo so the orchestration logic (keys:/dive:/hintlen
// scoping) gets exercised independently of the per-rule matrix.
func TestCheckRuleApplicability_Structural(t *testing.T) {
	t.Parallel()
	type tc struct {
		name    string
		fi      FieldInfo
		wantSub string // empty = expect nil
	}
	cases := []tc{
		// keys: only on maps.
		{
			"keys_val_on_string",
			FieldInfo{
				GoName: "S", GoType: "string", JSONName: "s", Kind: KindString,
				KeyValidation: []ValidationRule{{Name: "minlen", Value: "1"}},
				HintLen:       -1,
			},
			"`keys:` tag prefix is only valid on map[string]V fields",
		},
		{
			"keys_mod_on_slice",
			FieldInfo{
				GoName: "X", GoType: "[]int", JSONName: "x",
				Kind: KindSlice, ElemKind: KindInt, ElemType: "int",
				KeyMods: []ModRule{{Name: "trim"}},
				HintLen: -1,
			},
			"`keys:` tag prefix is only valid on map[string]V fields",
		},
		{
			"keys_val_on_map_ok",
			FieldInfo{
				GoName: "M", GoType: "map[string]int", JSONName: "m",
				Kind: KindMap, ElemKind: KindInt, ElemType: "int",
				KeyValidation: []ValidationRule{{Name: "minlen", Value: "2"}},
				HintLen:       -1,
			},
			"",
		},
		{
			"keys_val_on_map_wrong_rule",
			FieldInfo{
				GoName: "M", GoType: "map[string]int", JSONName: "m",
				Kind: KindMap, ElemKind: KindInt, ElemType: "int",
				// `gt` is numeric — invalid on string keys even though
				// the parent IS a map.
				KeyValidation: []ValidationRule{{Name: "gt", Value: "1"}},
				HintLen:       -1,
			},
			"`gt` is inapplicable to string",
		},

		// dive: only on containers.
		{
			"dive_on_int",
			FieldInfo{
				GoName: "N", GoType: "int", JSONName: "n", Kind: KindInt,
				ElemValidation: []ValidationRule{{Name: "minlen", Value: "1"}},
				HintLen:        -1,
			},
			"`dive:` tag prefix is only valid on slice/array/map fields",
		},
		{
			"dive_on_string",
			FieldInfo{
				GoName: "S", GoType: "string", JSONName: "s", Kind: KindString,
				ElemValidation: []ValidationRule{{Name: "minlen", Value: "1"}},
				HintLen:        -1,
			},
			"`dive:` tag prefix is only valid on slice/array/map fields",
		},
		{
			"dive_mod_on_int",
			FieldInfo{
				GoName: "N", GoType: "int", JSONName: "n", Kind: KindInt,
				ElemMods: []ModRule{{Name: "trim"}},
				HintLen:  -1,
			},
			"`dive:` tag prefix is only valid on slice/array/map fields",
		},
		{
			"dive_inner_on_int",
			FieldInfo{
				GoName: "N", GoType: "int", JSONName: "n", Kind: KindInt,
				InnerValidation: [][]ValidationRule{{{Name: "required"}}},
				HintLen:         -1,
			},
			"`dive:` tag prefix is only valid on slice/array/map fields",
		},
		{
			"dive_on_slice_int_with_ascii",
			FieldInfo{
				GoName: "X", GoType: "[]int", JSONName: "x",
				Kind: KindSlice, ElemKind: KindInt, ElemType: "int",
				ElemValidation: []ValidationRule{{Name: "ascii"}},
				HintLen:        -1,
			},
			"`ascii` is inapplicable to int",
		},
		{
			"dive_on_slice_int_with_gt_ok",
			FieldInfo{
				GoName: "X", GoType: "[]int", JSONName: "x",
				Kind: KindSlice, ElemKind: KindInt, ElemType: "int",
				ElemValidation: []ValidationRule{{Name: "gt", Value: "0"}},
				HintLen:        -1,
			},
			"",
		},
		{
			"dive_on_map_value_string_with_email_ok",
			FieldInfo{
				GoName: "M", GoType: "map[string]string", JSONName: "m",
				Kind: KindMap, ElemKind: KindString, ElemType: "string",
				ElemValidation: []ValidationRule{{Name: "email"}},
				HintLen:        -1,
			},
			"",
		},

		// hintlen restricted to slice/map.
		{
			"hintlen_on_int",
			FieldInfo{
				GoName: "N", GoType: "int", JSONName: "n", Kind: KindInt,
				HintLen: 5,
			},
			"`hintlen` is only valid on slice/map fields",
		},
		{
			"hintlen_on_string",
			FieldInfo{
				GoName: "S", GoType: "string", JSONName: "s", Kind: KindString,
				HintLen: 5,
			},
			"`hintlen` is only valid on slice/map fields",
		},
		{
			"hintlen_on_array",
			FieldInfo{
				GoName: "X", GoType: "[3]int", JSONName: "x",
				Kind: KindArray, ArrayLen: 3, ElemKind: KindInt, ElemType: "int",
				HintLen: 5,
			},
			"`hintlen` is only valid on slice/map fields",
		},
		{
			"hintlen_zero_on_bool",
			FieldInfo{
				GoName: "B", GoType: "bool", JSONName: "b", Kind: KindBool,
				HintLen: 0, // 0 is still "explicitly set", not "unset"
			},
			"`hintlen` is only valid on slice/map fields",
		},
		{
			"hintlen_unset_on_string_ok",
			FieldInfo{
				GoName: "S", GoType: "string", JSONName: "s", Kind: KindString,
				HintLen: -1,
			},
			"",
		},
		{
			"hintlen_on_slice_ok",
			FieldInfo{
				GoName: "X", GoType: "[]int", JSONName: "x",
				Kind: KindSlice, ElemKind: KindInt, ElemType: "int",
				HintLen: 16,
			},
			"",
		},
		{
			"hintlen_on_map_ok",
			FieldInfo{
				GoName: "M", GoType: "map[string]int", JSONName: "m",
				Kind: KindMap, ElemKind: KindInt, ElemType: "int",
				HintLen: 16,
			},
			"",
		},

		// happy: a full-feature map with keys + dive + hintlen + all rules valid.
		{
			"map_full_features_ok",
			FieldInfo{
				GoName: "M", GoType: "map[string]int", JSONName: "m",
				Kind: KindMap, ElemKind: KindInt, ElemType: "int",
				Validation:     []ValidationRule{{Name: "minlen", Value: "1"}},
				KeyValidation:  []ValidationRule{{Name: "minrunes", Value: "2"}},
				ElemValidation: []ValidationRule{{Name: "gte", Value: "0"}},
				KeyMods:        []ModRule{{Name: "lower"}},
				ElemMods:       []ModRule{{Name: "clamp", Value: "0|100"}},
				HintLen:        16,
			},
			"",
		},

		// pointer fields: rules apply to the pointee, fi.Kind == pointee kind.
		{
			"pointer_int_ascii_rejects",
			FieldInfo{
				GoName: "P", GoType: "*int", JSONName: "p",
				Pointer: true, PointeeType: "int", Kind: KindInt,
				Validation: []ValidationRule{{Name: "ascii"}},
				HintLen:    -1,
			},
			"`ascii` is inapplicable to *int",
		},
		{
			"pointer_string_email_ok",
			FieldInfo{
				GoName: "P", GoType: "*string", JSONName: "p",
				Pointer: true, PointeeType: "string", Kind: KindString,
				Validation: []ValidationRule{{Name: "email"}},
				HintLen:    -1,
			},
			"",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := checkRuleApplicability(c.fi)
			if c.wantSub == "" {
				if err != nil {
					t.Errorf("expected nil, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wantSub)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), c.wantSub)
			}
		})
	}
}

// TestCheckRuleApplicability_FieldNameInDiagnostic locks the
// user-facing convention: every reject must reference the Go field
// name so users can jump-to-definition / grep their source for the
// declared identifier (not the JSON wire name, which may shadow other
// identifiers or be `-` for ignored fields).
func TestCheckRuleApplicability_FieldNameInDiagnostic(t *testing.T) {
	t.Parallel()
	// Message format: "<Struct>.<Field>: `<rule>` is inapplicable to
	// <type>" — Go-qualified path, no `field` prefix, no `ggen rule`
	// tag-source noise. With StructName="Box" + GoName="Score" the
	// header reads `Box.Score: ...` for fast jump-to-definition in
	// editors and grep-pability in CI logs.
	fi := FieldInfo{
		StructName: "Box", GoName: "Score", GoType: "int", JSONName: "score", Kind: KindInt,
		Validation: []ValidationRule{{Name: "ascii"}},
		HintLen:    -1,
	}
	err := checkRuleApplicability(fi)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Box.Score") {
		t.Errorf("error %q must contain `Box.Score` (Struct.Field qualified path)", err.Error())
	}
	if strings.Contains(err.Error(), "field Score") || strings.Contains(err.Error(), "field score") {
		t.Errorf("error %q must NOT use the old `field <name>` shape", err.Error())
	}
	if !strings.Contains(err.Error(), "`ascii`") {
		t.Errorf("error %q must contain the rule name in backticks", err.Error())
	}
	if strings.Contains(err.Error(), "cannot be applied") || strings.Contains(err.Error(), "rule \"ascii\"") {
		t.Errorf("error %q should use new `is inapplicable to` form", err.Error())
	}
}
