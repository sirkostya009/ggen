package model

import (
	"strings"
	"testing"
)

// Unit tests for applicability.go. Two matrices do most of the work: every
// validation rule against every kind, and every mod against every kind.
// Value errors per rule and the structural checks get their own tables. The
// structural checks cover where keys:, inner: and hint may appear, and the
// rejection of KindStruct.

// kindEntry is one row of the kind matrix.
type kindEntry struct {
	kind TypeKind
	name string // label for humans, written like a Go type
}

// allKindEntries covers every TypeKind a field can resolve to, including
// KindStruct for the rejection of opaque types.
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
	{KindStruct, "Foo"},
}

// Predicates for which kinds a rule accepts, declared once so the per-rule
// tables stay readable.
var (
	anyKind         = func(k TypeKind) bool { return true }
	stringOnly      = func(k TypeKind) bool { return k == KindString }
	numericOnly     = IsNumeric
	integerOnly     = isIntegralNumeric
	lenable         = isLenKind
	stringOrNumeric = func(k TypeKind) bool { return k == KindString || IsNumeric(k) }
)

// valSpec is a validation rule plus a sample value that passes the value
// check on every accepted kind. That way the kind matrix never fails on the
// value itself, which TestCheckOneValRule_ValueShape covers.
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
	// eq and neq take strings or numbers. Use a value that is valid both
	// as a number and as a string, so neither path fails.
	{"eq", "5", stringOrNumeric},
	{"neq", "5", stringOrNumeric},
	// oneof: numeric parts must parse. "1|2|3" works for both string and
	// numeric kinds.
	{"oneof", "1|2|3", stringOrNumeric},
	{"url", "", stringOnly},
	{"alphanum", "", stringOnly},
	{"numeric", "", stringOnly},
	{"islower", "", stringOnly},
	{"isupper", "", stringOnly},
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
					// The rejection names the rule and the field type.
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

// Unknown rule names must give an error, not silently do nothing, so typos
// are caught. Custom `@FuncName` rules are excluded, because they are
// resolved later in resolvePipeCustoms.
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

// `@FuncName` references must survive the unknown-rule check.
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

// TestCheckOneValRule_ValueShape covers per-rule value-shape errors. Each row
// uses a kind that accepts the rule, so only the value-shape branch can fail.
func TestCheckOneValRule_ValueShape(t *testing.T) {
	t.Parallel()
	type tc struct {
		name     string
		rule     ValidationRule
		kind     TypeKind
		wantSub  string // text expected in err.Error(), empty means no error
		typeName string
	}
	cases := []tc{
		// len, minlen and maxlen need an integer.
		{"len_good", ValidationRule{Name: "len", Value: "5"}, KindString, "", "string"},
		{"len_empty", ValidationRule{Name: "len"}, KindString, "requires an integer value", "string"},
		{"len_non_numeric", ValidationRule{Name: "len", Value: "abc"}, KindString, "value is not a valid integer", "string"},
		{"len_float", ValidationRule{Name: "len", Value: "1.5"}, KindString, "value is not a valid integer", "string"},
		{"len_whitespace", ValidationRule{Name: "len", Value: " 5 "}, KindString, "", "string"},
		{"len_negative", ValidationRule{Name: "len", Value: "-3"}, KindString, "requires a non-negative integer", "string"},
		{"maxlen_negative", ValidationRule{Name: "maxlen", Value: "-1"}, KindString, "requires a non-negative integer", "string"},
		{"maxrunes_negative", ValidationRule{Name: "maxrunes", Value: "-2"}, KindString, "requires a non-negative integer", "string"},
		{"len_zero_ok", ValidationRule{Name: "len", Value: "0"}, KindString, "", "string"},
		{"minlen_empty", ValidationRule{Name: "minlen"}, KindString, "requires an integer value", "string"},
		{"maxlen_non_numeric", ValidationRule{Name: "maxlen", Value: "x"}, KindSlice, "is not a valid integer", "[]int"},

		// runes, minrunes and maxrunes need an integer.
		{"runes_good", ValidationRule{Name: "runes", Value: "3"}, KindString, "", "string"},
		{"runes_empty", ValidationRule{Name: "runes"}, KindString, "requires an integer value", "string"},
		{"runes_bad", ValidationRule{Name: "minrunes", Value: "abc"}, KindString, "value is not a valid integer", "string"},

		// gt, gte, lt and lte need a number.
		{"gt_good", ValidationRule{Name: "gt", Value: "1"}, KindInt, "", "int"},
		{"gt_float_ok", ValidationRule{Name: "gt", Value: "1.5"}, KindFloat64, "", "float64"},
		{"gt_empty", ValidationRule{Name: "gt"}, KindInt, "requires a numeric value", "int"},
		{"gt_bad", ValidationRule{Name: "gt", Value: "abc"}, KindInt, "value is not a valid number", "int"},
		{"lte_bad", ValidationRule{Name: "lte", Value: "abc"}, KindInt, "value is not a valid number", "int"},
		// Integer bounds parse at the kind's width and sign, so a uint64 can
		// take any bound up to 2^64-1. Fractional and out-of-range bounds
		// have their own errors.
		{"gte_uint64_above_int63", ValidationRule{Name: "gte", Value: "9223372036854775808"}, KindUint64, "", "uint64"},
		{"eq_uint64_max", ValidationRule{Name: "eq", Value: "18446744073709551615"}, KindUint64, "", "uint64"},
		{"lt_uint64_max", ValidationRule{Name: "lt", Value: "18446744073709551615"}, KindUint64, "", "uint64"},
		{"gt_int64_overflow", ValidationRule{Name: "gt", Value: "9223372036854775808"}, KindInt64, "out of the field type's range", "int64"},
		{"gt_fractional_int", ValidationRule{Name: "gt", Value: "1.5"}, KindInt, "integer field needs an integer bound", "int"},
		{"gt_uint8_negative", ValidationRule{Name: "gt", Value: "-1"}, KindUint8, "out of the field type's range", "uint8"},

		// multiple needs an integer.
		{"multiple_good", ValidationRule{Name: "multiple", Value: "2"}, KindInt, "", "int"},
		{"multiple_empty", ValidationRule{Name: "multiple"}, KindInt, "requires an integer value", "int"},
		{"multiple_bad", ValidationRule{Name: "multiple", Value: "abc"}, KindInt, "value is not a valid integer", "int"},
		{"multiple_uint64_above_int63", ValidationRule{Name: "multiple", Value: "9223372036854775808"}, KindUint64, "", "uint64"},
		{"multiple_uint64_zero", ValidationRule{Name: "multiple", Value: "0"}, KindUint64, "requires a positive integer", "uint64"},
		{"multiple_int_negative", ValidationRule{Name: "multiple", Value: "-2"}, KindInt, "requires a positive integer", "int"},
		{"multiple_int8_overflow", ValidationRule{Name: "multiple", Value: "300"}, KindInt8, "out of the field type's range", "int8"},
		{"multiple_fractional", ValidationRule{Name: "multiple", Value: "1.5"}, KindInt, "value is not a valid integer", "int"},

		// eq and neq: a numeric kind needs a valid number, a string takes
		// any value.
		{"eq_str_any_value", ValidationRule{Name: "eq", Value: "abc"}, KindString, "", "string"},
		{"eq_int_bad", ValidationRule{Name: "eq", Value: "abc"}, KindInt, "value is not a valid number", "int"},
		{"eq_int_empty", ValidationRule{Name: "eq"}, KindInt, "requires a numeric value", "int"},
		{"neq_int_bad", ValidationRule{Name: "neq", Value: "x"}, KindInt, "value is not a valid number", "int"},

		// oneof needs a non-empty list. A numeric kind needs numeric parts.
		{"oneof_str_any", ValidationRule{Name: "oneof", Value: "a|b|c"}, KindString, "", "string"},
		{"oneof_empty", ValidationRule{Name: "oneof"}, KindString, "requires a", "string"},
		{"oneof_num_good", ValidationRule{Name: "oneof", Value: "1|2|3"}, KindInt, "", "int"},
		{"oneof_num_bad_part", ValidationRule{Name: "oneof", Value: "1|two|3"}, KindInt, `part "two" is not a valid number`, "int"},
		{"oneof_num_trailing", ValidationRule{Name: "oneof", Value: " 1 | 2 "}, KindInt, "", "int"},
		{"oneof_quoted_space", ValidationRule{Name: "oneof", Value: "'New York'|LA"}, KindString, "", "string"},
		{"oneof_quoted_dup", ValidationRule{Name: "oneof", Value: "'LA'|LA"}, KindString, "is a duplicate", "string"},
		// Numeric parts are compared by value for duplicates. Integer kinds
		// compare the integer itself, because a float64 key would merge
		// different integers above 2^53. A float spelling of a whole number
		// still matches its integer, and floats treat -0 as 0.
		{"oneof_int64_above_2p53_distinct", ValidationRule{Name: "oneof", Value: "9007199254740993|9007199254740992"}, KindInt64, "", "int64"},
		{"oneof_uint64_top_distinct", ValidationRule{Name: "oneof", Value: "18446744073709551615|18446744073709551614"}, KindUint64, "", "uint64"},
		{"oneof_int_value_dup", ValidationRule{Name: "oneof", Value: "1|1.0"}, KindInt, "is a duplicate", "int"},
		{"oneof_int_plus_dup", ValidationRule{Name: "oneof", Value: "1|+1"}, KindInt, "is a duplicate", "int"},
		{"oneof_float_neg_zero_dup", ValidationRule{Name: "oneof", Value: "0|-0.0"}, KindFloat64, "is a duplicate", "float64"},
		{"oneof_float_distinct", ValidationRule{Name: "oneof", Value: "1.5|2.5"}, KindFloat64, "", "float64"},

		// len and minlen set the capacity of the container's make(). A
		// capacity make() cannot handle is rejected here instead of
		// panicking at decode. maxlen is only a bound, and strings never
		// preallocate.
		{"len_slice_prealloc_ceiling", ValidationRule{Name: "len", Value: "4294967296"}, KindSlice, "prealloc ceiling", "[]int"},
		{"minlen_map_prealloc_ceiling", ValidationRule{Name: "minlen", Value: "4294967296"}, KindMap, "prealloc ceiling", "map[string]int"},
		// The limit is MaxInt32, so every accepted value is a valid int
		// constant on a 32-bit target too.
		{"len_slice_at_ceiling", ValidationRule{Name: "len", Value: "2147483647"}, KindSlice, "", "[]int"},
		{"len_slice_over_ceiling", ValidationRule{Name: "len", Value: "2147483648"}, KindSlice, "prealloc ceiling", "[]int"},
		{"maxlen_slice_no_ceiling", ValidationRule{Name: "maxlen", Value: "4294967296"}, KindSlice, "", "[]int"},
		{"len_string_no_ceiling", ValidationRule{Name: "len", Value: "4294967296"}, KindString, "", "string"},

		// starts, ends and contains need a non-empty value.
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

// ----- mod matrix -----

type modSpec struct {
	name   string
	value  string
	accept func(TypeKind) bool
}

var modSpecs = []modSpec{
	{"trim", "", stringOnly},
	{"tolower", "", stringOnly},
	{"toupper", "", stringOnly},
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

// Unknown mod names must give an error, so typos are caught. `@FuncName`
// mods are excluded.
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
		// trim, tolower and toupper take no value. Any value, or none, is
		// accepted.
		{"trim_no_value", ModRule{Name: "trim"}, KindString, ""},
		{"tolower_no_value", ModRule{Name: "tolower"}, KindString, ""},
		{"toupper_no_value", ModRule{Name: "toupper"}, KindString, ""},

		// trimleft and trimright need a non-empty value.
		{"trimleft_good", ModRule{Name: "trimleft", Value: "X"}, KindString, ""},
		{"trimleft_empty", ModRule{Name: "trimleft"}, KindString, "requires a non-empty value"},
		{"trimright_empty", ModRule{Name: "trimright"}, KindString, "requires a non-empty value"},

		// replace needs "old|new" with a non-empty old.
		{"replace_good", ModRule{Name: "replace", Value: "old|new"}, KindString, ""},
		{"replace_empty_new_ok", ModRule{Name: "replace", Value: "old|"}, KindString, ""},
		{"replace_no_pipe", ModRule{Name: "replace", Value: "foo"}, KindString, "requires `old|new` form"},
		{"replace_empty_old", ModRule{Name: "replace", Value: "|new"}, KindString, "requires `old|new` form"},
		{"replace_empty_value", ModRule{Name: "replace"}, KindString, "requires `old|new` form"},
		{"replace_three_parts", ModRule{Name: "replace", Value: "a|b|c"}, KindString, "requires `old|new` form"},
		{"replace_quoted_pipe", ModRule{Name: "replace", Value: "'a|b'|c"}, KindString, ""},

		// clamp needs "lo|hi". At least one bound must be given, and each
		// bound must be a valid number.
		{"clamp_good", ModRule{Name: "clamp", Value: "0|10"}, KindInt, ""},
		{"clamp_lo_only", ModRule{Name: "clamp", Value: "0|"}, KindInt, ""},
		{"clamp_hi_only", ModRule{Name: "clamp", Value: "|10"}, KindInt, ""},
		{"clamp_no_pipe", ModRule{Name: "clamp", Value: "10"}, KindInt, "needs exactly one lo`|`hi separator"},
		{"clamp_three_parts", ModRule{Name: "clamp", Value: "0|5|10"}, KindInt, "needs exactly one lo`|`hi separator"},
		{"clamp_quoted_pipe_lo", ModRule{Name: "clamp", Value: "'1'|10"}, KindInt, ""},
		{"clamp_both_empty", ModRule{Name: "clamp", Value: "|"}, KindInt, "requires at least one of lo or hi"},
		{"clamp_bad_lo", ModRule{Name: "clamp", Value: "abc|10"}, KindInt, `lo "abc" is not a valid number`},
		{"clamp_bad_hi", ModRule{Name: "clamp", Value: "0|abc"}, KindInt, `hi "abc" is not a valid number`},
		{"clamp_float_ok", ModRule{Name: "clamp", Value: "0.5|10.5"}, KindFloat64, ""},
		{"clamp_whitespace", ModRule{Name: "clamp", Value: " 0 | 10 "}, KindInt, ""},
		{"clamp_uint64_max_hi", ModRule{Name: "clamp", Value: "|18446744073709551615"}, KindUint64, ""},
		{"clamp_fractional_int", ModRule{Name: "clamp", Value: "0|1.5"}, KindInt, "integer field needs integer bounds"},
		{"clamp_int8_overflow", ModRule{Name: "clamp", Value: "0|300"}, KindInt8, "out of the field type's range"},
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

// ----- KindStruct and custom rules -----

// KindStruct fields reach these checks only when they are real struct or
// opaque types, because eff() already resolved named primitives. Rules that
// need a specific kind must reject them. required, optional and @Func still
// pass.
func TestCheckValRules_KindStructRejected(t *testing.T) {
	t.Parallel()
	for _, r := range []ValidationRule{
		{Name: "alphanum"},
		{Name: "numeric"},
		{Name: "gt", Value: "5"},
		{Name: "multiple", Value: "2"},
		{Name: "len", Value: "5"},
		{Name: "oneof", Value: "a|b"},
		{Name: "eq", Value: "x"},
		{Name: "neq", Value: "x"},
	} {
		if err := checkValRules([]ValidationRule{r}, "ggen", KindStruct, "Foo", "field f"); err == nil {
			t.Errorf("rule %q on KindStruct must reject, got nil", r.Name)
		}
	}
	// Rules that work on any kind still work on structs.
	ok := []ValidationRule{{Name: "required"}, {Name: "optional"}}
	if err := checkValRules(ok, "ggen", KindStruct, "Foo", "field f"); err != nil {
		t.Errorf("required/optional on KindStruct must pass, got: %v", err)
	}
}

// TestCheckModRules_KindStructRejected is the same rejection test for mods.
func TestCheckModRules_KindStructRejected(t *testing.T) {
	t.Parallel()
	for _, m := range []ModRule{
		{Name: "trim"},
		{Name: "clamp", Value: "0|10"},
		{Name: "replace", Value: "a|b"},
	} {
		if err := checkModRules([]ModRule{m}, "mod", KindStruct, "Foo", "field f"); err == nil {
			t.Errorf("mod %q on KindStruct must reject, got nil", m.Name)
		}
	}
}

// @Func rules skip the matrix, because resolvePipeCustoms checks the
// signature against the field's go/types type.
func TestCheckValRules_CustomSkipped(t *testing.T) {
	t.Parallel()
	rules := []ValidationRule{
		// Custom=true skips the matrix, whatever the field kind.
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

// ----- helper predicates -----

func TestCanDive(t *testing.T) {
	t.Parallel()
	// []byte and [N]byte decode as one base64 string with no element loop,
	// so an `inner:` step there could never run.
	accepted := map[TypeKind]bool{
		KindSlice: true, KindArray: true, KindMap: true,
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
		wantSub string // empty means no error
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
			err := needFloat(ValidationRule{Name: "gt", Value: c.value}, "field f", KindFloat64)
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

// ----- structural checks: keys:, inner: and hint -----

// TestCheckRuleApplicability_Structural tests, through FieldInfo, where
// keys:, inner: and hint may appear. The rule matrix is tested separately.
func TestCheckRuleApplicability_Structural(t *testing.T) {
	t.Parallel()
	type tc struct {
		name    string
		fi      FieldInfo
		wantSub string // empty means no error
	}
	cases := []tc{
		// keys: is only valid on maps.
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
				// `gt` is numeric, so it is invalid on string keys even
				// though the parent is a map.
				KeyValidation: []ValidationRule{{Name: "gt", Value: "1"}},
				HintLen:       -1,
			},
			"`gt` is inapplicable to string",
		},

		// inner: is only valid on containers. A byte slice or array is not
		// one in JSON, since it is one base64 string, folded or not.
		{
			"dive_on_bytes",
			FieldInfo{
				GoName: "B", GoType: "[]byte", JSONName: "b", Kind: KindBytes,
				ElemMods: []ModRule{{Name: "trim"}},
				HintLen:  -1,
			},
			"`inner:` tag prefix is only valid on slice/array/map fields",
		},
		{
			"dive_on_byte_array_unfolded",
			FieldInfo{
				GoName: "B", GoType: "[3]byte", JSONName: "b",
				Kind: KindArray, ArrayLen: 3, ElemKind: KindUint8, ElemType: "byte",
				ElemValidation: []ValidationRule{{Name: "gt", Value: "1"}},
				HintLen:        -1,
			},
			"`inner:` tag prefix is only valid on slice/array/map fields",
		},
		{
			"dive_on_byte_array_format_array_ok",
			FieldInfo{
				GoName: "B", GoType: "[3]byte", JSONName: "b", Format: "array",
				Kind: KindArray, ArrayLen: 3, ElemKind: KindUint8, ElemType: "byte",
				ElemValidation: []ValidationRule{{Name: "gt", Value: "1"}},
				HintLen:        -1,
			},
			"",
		},
		{
			"dive_on_int",
			FieldInfo{
				GoName: "N", GoType: "int", JSONName: "n", Kind: KindInt,
				ElemValidation: []ValidationRule{{Name: "minlen", Value: "1"}},
				HintLen:        -1,
			},
			"`inner:` tag prefix is only valid on slice/array/map fields",
		},
		{
			"dive_on_string",
			FieldInfo{
				GoName: "S", GoType: "string", JSONName: "s", Kind: KindString,
				ElemValidation: []ValidationRule{{Name: "minlen", Value: "1"}},
				HintLen:        -1,
			},
			"`inner:` tag prefix is only valid on slice/array/map fields",
		},
		{
			"dive_mod_on_int",
			FieldInfo{
				GoName: "N", GoType: "int", JSONName: "n", Kind: KindInt,
				ElemMods: []ModRule{{Name: "trim"}},
				HintLen:  -1,
			},
			"`inner:` tag prefix is only valid on slice/array/map fields",
		},
		{
			"dive_inner_on_int",
			FieldInfo{
				GoName: "N", GoType: "int", JSONName: "n", Kind: KindInt,
				InnerValidation: [][]ValidationRule{{{Name: "required"}}},
				HintLen:         -1,
			},
			"`inner:` tag prefix is only valid on slice/array/map fields",
		},
		{
			"dive_on_slice_int_with_alphanum",
			FieldInfo{
				GoName: "X", GoType: "[]int", JSONName: "x",
				Kind: KindSlice, ElemKind: KindInt, ElemType: "int",
				ElemValidation: []ValidationRule{{Name: "alphanum"}},
				HintLen:        -1,
			},
			"`alphanum` is inapplicable to int",
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
			"dive_on_map_value_string_with_alphanum_ok",
			FieldInfo{
				GoName: "M", GoType: "map[string]string", JSONName: "m",
				Kind: KindMap, ElemKind: KindString, ElemType: "string",
				ElemValidation: []ValidationRule{{Name: "alphanum"}},
				HintLen:        -1,
			},
			"",
		},

		// hint is only valid on slices and maps.
		{
			"hintlen_on_int",
			FieldInfo{
				GoName: "N", GoType: "int", JSONName: "n", Kind: KindInt,
				HintLen: 5,
			},
			"`hint` is only valid on slice/map fields",
		},
		{
			"hintlen_on_string",
			FieldInfo{
				GoName: "S", GoType: "string", JSONName: "s", Kind: KindString,
				HintLen: 5,
			},
			"`hint` is only valid on slice/map fields",
		},
		{
			"hintlen_on_array",
			FieldInfo{
				GoName: "X", GoType: "[3]int", JSONName: "x",
				Kind: KindArray, ArrayLen: 3, ElemKind: KindInt, ElemType: "int",
				HintLen: 5,
			},
			"`hint` is only valid on slice/map fields",
		},
		{
			"hintlen_zero_on_bool",
			FieldInfo{
				GoName: "B", GoType: "bool", JSONName: "b", Kind: KindBool,
				HintLen: 0, // 0 still counts as set, not as unset
			},
			"`hint` is only valid on slice/map fields",
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

		// valid: a map using keys:, inner: and hint, with every rule valid.
		{
			"map_full_features_ok",
			FieldInfo{
				GoName: "M", GoType: "map[string]int", JSONName: "m",
				Kind: KindMap, ElemKind: KindInt, ElemType: "int",
				Validation:     []ValidationRule{{Name: "minlen", Value: "1"}},
				KeyValidation:  []ValidationRule{{Name: "minrunes", Value: "2"}},
				ElemValidation: []ValidationRule{{Name: "gte", Value: "0"}},
				KeyMods:        []ModRule{{Name: "tolower"}},
				ElemMods:       []ModRule{{Name: "clamp", Value: "0|100"}},
				HintLen:        16,
			},
			"",
		},

		// `omitempty` on a struct never omits it, so the option is rejected
		// instead of silently doing nothing. UnderlyingStruct is the answer
		// from go/types. A named primitive and a foreign array also read as
		// KindStruct, and neither is a struct.
		{
			"omitempty_on_struct_rejects",
			FieldInfo{
				GoName: "In", GoType: "Nested", JSONName: "in", Kind: KindStruct,
				OmitEmpty: true, UnderlyingStruct: true, HintLen: -1,
			},
			"`omitempty` is not applicable to a struct field (got Nested)",
		},
		{
			"omitzero_on_struct_ok",
			FieldInfo{
				GoName: "In", GoType: "Nested", JSONName: "in", Kind: KindStruct,
				OmitZero: true, UnderlyingStruct: true, HintLen: -1,
			},
			"",
		},
		{
			"omitempty_on_pointer_to_struct_ok",
			FieldInfo{
				GoName: "In", GoType: "*Nested", JSONName: "in", Kind: KindStruct,
				Pointer: true, PointeeType: "Nested", OmitEmpty: true, HintLen: -1,
			},
			"",
		},
		{
			"omitempty_on_named_primitive_ok",
			FieldInfo{
				GoName: "P", GoType: "Pri", JSONName: "p", Kind: KindStruct,
				NamedPrims: map[string]TypeKind{"Pri": KindString},
				OmitEmpty:  true, HintLen: -1,
			},
			"",
		},
		{
			"omitempty_on_foreign_array_ok",
			FieldInfo{
				GoName: "ID", GoType: "uuid.UUID", JSONName: "id", Kind: KindStruct,
				OmitEmpty: true, HintLen: -1,
			},
			"",
		},
		{
			"omitempty_on_time_ok",
			FieldInfo{
				GoName: "T", GoType: "time.Time", JSONName: "t", Kind: KindTime,
				OmitEmpty: true, UnderlyingStruct: true, HintLen: -1,
			},
			"",
		},
		{
			"omitempty_on_sqlnull_ok",
			FieldInfo{
				GoName: "N", GoType: "sql.NullString", JSONName: "n", Kind: KindSQLNull,
				OmitEmpty: true, UnderlyingStruct: true, HintLen: -1,
			},
			"",
		},

		// pointer fields: rules apply to the pointee, and fi.Kind is the
		// pointee kind.
		{
			"pointer_int_alphanum_rejects",
			FieldInfo{
				GoName: "P", GoType: "*int", JSONName: "p",
				Pointer: true, PointeeType: "int", Kind: KindInt,
				Validation: []ValidationRule{{Name: "alphanum"}},
				HintLen:    -1,
			},
			"`alphanum` is inapplicable to *int",
		},
		{
			"pointer_string_alphanum_ok",
			FieldInfo{
				GoName: "P", GoType: "*string", JSONName: "p",
				Pointer: true, PointeeType: "string", Kind: KindString,
				Validation: []ValidationRule{{Name: "alphanum"}},
				HintLen:    -1,
			},
			"",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := checkRuleApplicability(c.fi, false)
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

// Every rejection must name the Go field, not the JSON key, so users can
// jump to the definition or grep their source.
func TestCheckRuleApplicability_FieldNameInDiagnostic(t *testing.T) {
	t.Parallel()
	// Format: "<Struct>.<Field>: `<rule>` is inapplicable to <type>".
	fi := FieldInfo{
		StructName: "Box", GoName: "Score", GoType: "int", JSONName: "score", Kind: KindInt,
		Validation: []ValidationRule{{Name: "alphanum"}},
		HintLen:    -1,
	}
	err := checkRuleApplicability(fi, false)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Box.Score") {
		t.Errorf("error %q must contain `Box.Score` (Struct.Field qualified path)", err.Error())
	}
	if strings.Contains(err.Error(), "field Score") || strings.Contains(err.Error(), "field score") {
		t.Errorf("error %q must NOT use the old `field <name>` shape", err.Error())
	}
	if !strings.Contains(err.Error(), "`alphanum`") {
		t.Errorf("error %q must contain the rule name in backticks", err.Error())
	}
	if strings.Contains(err.Error(), "cannot be applied") || strings.Contains(err.Error(), "rule \"alphanum\"") {
		t.Errorf("error %q should use new `is inapplicable to` form", err.Error())
	}
}

// A field with no elements gets exactly one error for its `inner:` tag. The
// element check has no element type to judge a rule against, so it does not
// run and cannot print `is inapplicable to ` with an empty type.
func TestCheckRuleApplicability_NonDiveableReportsOnce(t *testing.T) {
	t.Parallel()
	cases := []FieldInfo{
		{
			StructName: "A", GoName: "G", GoType: "[]byte", JSONName: "g", Kind: KindBytes,
			ElemValidation: []ValidationRule{{Name: "gt", Value: "1"}},
			HintLen:        -1,
		},
		{
			StructName: "A", GoName: "G", GoType: "[3]byte", JSONName: "g",
			Kind: KindArray, ArrayLen: 3, ElemKind: KindUint8, ElemType: "byte",
			ElemMods: []ModRule{{Name: "trim"}},
			HintLen:  -1,
		},
		{
			StructName: "A", GoName: "N", GoType: "int", JSONName: "n", Kind: KindInt,
			ElemValidation: []ValidationRule{{Name: "gt", Value: "1"}},
			HintLen:        -1,
		},
	}
	for _, fi := range cases {
		t.Run(fi.GoType, func(t *testing.T) {
			t.Parallel()
			err := checkRuleApplicability(fi, true)
			if err == nil {
				t.Fatal("expected the `inner:` rejection")
			}
			n := 1
			if u, ok := err.(interface{ Unwrap() []error }); ok {
				n = len(u.Unwrap())
			}
			if n != 1 {
				t.Errorf("got %d diagnostics, want 1:\n%v", n, err)
			}
			if strings.Contains(err.Error(), "element") {
				t.Errorf("element-level diagnostic on a field with no elements:\n%v", err)
			}
		})
	}
}
