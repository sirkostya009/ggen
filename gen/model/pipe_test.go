package model

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestParsePipeTag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		tag  string
		want ParsedPipe
	}{
		{
			name: "plain linear value steps, implicit native decode",
			tag:  "trim minlen=1 maxlen=50",
			want: ParsedPipe{
				Outer: []Step{
					{IsMod: true, M: ModRule{Name: "trim"}},
					{V: ValidationRule{Name: "minlen", Value: "1"}},
					{V: ValidationRule{Name: "maxlen", Value: "50"}},
				},
			},
		},
		{
			name: "presence lifted, order preserved across mods+validators",
			tag:  "required ~ trim minlen=1",
			want: ParsedPipe{
				Presence: PresenceRequired,
				Outer: []Step{
					{IsMod: true, M: ModRule{Name: "trim"}},
					{V: ValidationRule{Name: "minlen", Value: "1"}},
				},
			},
		},
		{
			name: "decode variants nullzero / native / converter",
			tag:  "nullzero / . / @AtoiStrict ~ gte=0 lte=150",
			want: ParsedPipe{
				Variants: []Variant{
					{Kind: VariantNullZero},
					{Kind: VariantNative},
					{Kind: VariantConvert, FuncName: "AtoiStrict"},
				},
				Outer: []Step{
					{V: ValidationRule{Name: "gte", Value: "0"}},
					{V: ValidationRule{Name: "lte", Value: "150"}},
				},
			},
		},
		{
			name: "no-space glyphs ./@Conv lex like spaced",
			tag:  "./@FromMoney ~ gte=0",
			want: ParsedPipe{
				Variants: []Variant{
					{Kind: VariantNative},
					{Kind: VariantConvert, FuncName: "FromMoney"},
				},
				Outer: []Step{{V: ValidationRule{Name: "gte", Value: "0"}}},
			},
		},
		{
			name: "inner group then whole-slice outer step",
			tag:  "optional ~ minlen=1 inner:(trim maxlen=20) maxlen=100",
			want: ParsedPipe{
				Presence: PresenceOptional,
				Outer: []Step{
					{V: ValidationRule{Name: "minlen", Value: "1"}},
					{V: ValidationRule{Name: "maxlen", Value: "100"}},
				},
				Levels: [][]Step{{
					{IsMod: true, M: ModRule{Name: "trim"}},
					{V: ValidationRule{Name: "maxlen", Value: "20"}},
				}},
			},
		},
		{
			name: "single-step inner needs no parens",
			tag:  "inner:trim",
			want: ParsedPipe{
				Levels: [][]Step{{
					{IsMod: true, M: ModRule{Name: "trim"}},
				}},
			},
		},
		{
			name: "nested inner groups peel levels",
			tag:  "inner:(minlen=1 inner:(gte=0 lte=100))",
			want: ParsedPipe{
				Levels: [][]Step{
					{{V: ValidationRule{Name: "minlen", Value: "1"}}},
					{{V: ValidationRule{Name: "gte", Value: "0"}}, {V: ValidationRule{Name: "lte", Value: "100"}}},
				},
			},
		},
		{
			name: "keys group + single-step inner",
			tag:  "keys:(minrunes=2 maxrunes=32) inner:trim",
			want: ParsedPipe{
				Keys: []Step{
					{V: ValidationRule{Name: "minrunes", Value: "2"}},
					{V: ValidationRule{Name: "maxrunes", Value: "32"}},
				},
				Levels: [][]Step{{
					{IsMod: true, M: ModRule{Name: "trim"}},
				}},
			},
		},
		{
			name: "quoted value with spaces + inline message",
			tag:  "contains='foo bar' @Check:'value is bad'",
			want: ParsedPipe{
				Outer: []Step{
					{V: ValidationRule{Name: "contains", Value: "foo bar"}},
					{V: ValidationRule{Name: "@Check", Msg: "value is bad"}},
				},
			},
		},
		{
			name: "decode-only variants, no value steps",
			tag:  "nullzero / .",
			want: ParsedPipe{
				Variants: []Variant{{Kind: VariantNullZero}, {Kind: VariantNative}},
			},
		},
		{
			name: "bare nullzero variant then value steps, no tilde",
			tag:  "nullzero gte=0 lte=150",
			want: ParsedPipe{
				Variants: []Variant{{Kind: VariantNullZero}},
				Outer: []Step{
					{V: ValidationRule{Name: "gte", Value: "0"}},
					{V: ValidationRule{Name: "lte", Value: "150"}},
				},
			},
		},
		{
			name: "presence + value steps, no tilde",
			tag:  "required trim minlen=1 maxlen=10",
			want: ParsedPipe{
				Presence: PresenceRequired,
				Outer: []Step{
					{IsMod: true, M: ModRule{Name: "trim"}},
					{V: ValidationRule{Name: "minlen", Value: "1"}},
					{V: ValidationRule{Name: "maxlen", Value: "10"}},
				},
			},
		},
		{
			name: "lone leading @Func stays a value step",
			tag:  "@Check minlen=1",
			want: ParsedPipe{
				Outer: []Step{
					{V: ValidationRule{Name: "@Check"}},
					{V: ValidationRule{Name: "minlen", Value: "1"}},
				},
			},
		},
		{
			// Presence words are taken out only at the top level. One inside
			// a group is rejected there, and does not mark the outer field.
			name: "presence lifted around an inner group",
			tag:  "required inner:(minlen=1)",
			want: ParsedPipe{
				Presence: PresenceRequired,
				Levels:   [][]Step{{{V: ValidationRule{Name: "minlen", Value: "1"}}}},
			},
		},
		{
			name: "converter-first needs slash to read as variant",
			tag:  "@FromMoney / . gte=0",
			want: ParsedPipe{
				Variants: []Variant{
					{Kind: VariantConvert, FuncName: "FromMoney"},
					{Kind: VariantNative},
				},
				Outer: []Step{{V: ValidationRule{Name: "gte", Value: "0"}}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parsePipeTag(tt.tag)
			if err != nil {
				t.Fatalf("parsePipeTag(%q) error: %v", tt.tag, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parsePipeTag(%q)\n got = %#v\nwant = %#v", tt.tag, got, tt.want)
			}
		})
	}
}

func TestParseHintTag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		tag  string
		want HintTag
	}{
		{"", HintTag{Outer: -1}},
		{"32", HintTag{Outer: 32}},
		{"32 inner:8", HintTag{Outer: 32, Levels: []int{8}}},
		{"inner:8", HintTag{Outer: -1, Levels: []int{8}}},
		{"8 inner:(4 inner:2)", HintTag{Outer: 8, Levels: []int{4, 2}}},
	}
	for _, tt := range tests {
		got, err := parseHintTag(tt.tag)
		if err != nil {
			t.Fatalf("parseHintTag(%q) error: %v", tt.tag, err)
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseHintTag(%q) = %#v, want %#v", tt.tag, got, tt.want)
		}
	}
}

func TestParsePipeTagErrors(t *testing.T) {
	t.Parallel()
	bad := []string{
		"required optional",     // cannot have both
		"foo / / bar ~ x",       // empty variant
		"@ ~ x",                 // empty @ reference
		"trim / lower",          // slash outside the decode stage
		"inner:trim ; maxlen=1", // `;` is not valid
		"inner:(trim maxlen=1",  // missing closing paren
		"inner:",                // prefix with nothing after it
		"(trim)",                // group with no prefix
		// presence belongs to the field's key, never to an element or a map
		// key. Bare, spaced and grouped forms are all rejected. Otherwise the
		// bare form would emit nothing and the grouped form would apply to
		// the outer key
		"inner:required",
		"inner: optional",
		"inner:(required minlen=1)",
		"keys:required",
		"keys:(optional maxlen=3)",
	}
	for _, tag := range bad {
		if _, err := parsePipeTag(tag); err == nil {
			t.Errorf("parsePipeTag(%q) expected error, got nil", tag)
		}
	}
}

// A capacity make() cannot handle is a parse error, not a makeslice panic at
// decode time. The limit is the largest int a 32-bit target can hold, so
// every accepted value is a valid constant on any target.
func TestParseHintTag_Ceiling(t *testing.T) {
	t.Parallel()
	for _, tag := range []string{"9223372036854775807", "2147483648", "4 inner:2147483648"} {
		if _, err := parseHintTag(tag); err == nil || !strings.Contains(err.Error(), "prealloc ceiling") {
			t.Errorf("parseHintTag(%q) = %v, want prealloc-ceiling error", tag, err)
		}
	}
	if _, err := parseHintTag("2147483647"); err != nil {
		t.Errorf("parseHintTag at the ceiling: %v", err)
	}
}

// SplitPipeParts splits on `|` only outside single quotes, and each part
// loses one layer of quotes. \' is a literal quote. Multi-part values reach
// it with their quotes, because parseStep does not strip quotes from oneof,
// replace and clamp values.
func TestSplitPipeParts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want []string
	}{
		{"a|b|c", []string{"a", "b", "c"}},
		{"'New York'|LA", []string{"New York", "LA"}},
		{"'a|b'|c", []string{"a|b", "c"}},
		{`'it\'s'|x`, []string{"it's", "x"}},
		{"0|", []string{"0", ""}},
		{"|100", []string{"", "100"}},
		{"'a b c'", []string{"a b c"}},
		{"", []string{""}},
	}
	for _, c := range cases {
		if got := SplitPipeParts(c.in); !slices.Equal(got, c.want) {
			t.Errorf("SplitPipeParts(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// parseStep keeps the quotes of multi-part values for SplitPipeParts, but
// still strips them from single-value steps.
func TestParseStep_MultiPartKeepsQuotes(t *testing.T) {
	t.Parallel()
	st, err := parseStep("oneof='a b'|c")
	if err != nil || st.V.Value != "'a b'|c" {
		t.Errorf("oneof value = %q, %v; want raw with quotes", st.V.Value, err)
	}
	st, err = parseStep("starts='a b'")
	if err != nil || st.V.Value != "a b" {
		t.Errorf("starts value = %q, %v; want stripped", st.V.Value, err)
	}
}

// The same through the tokenizer. The unit test above gives SplitPipeParts
// its input directly, so it cannot catch tokenizePipe unescaping \' inside a
// quoted part. SplitPipeParts would then read that bare quote as the start or
// end of a quoted part, and `oneof='it\'s'|no` would become one broken entry
// "'it's'|no", silently rejecting both intended values.
func TestParsePipeTag_EscapedQuoteInMultiPartValue(t *testing.T) {
	t.Parallel()
	pp, err := parsePipeTag(`oneof='it\'s'|no`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(pp.Outer) != 1 {
		t.Fatalf("got %d steps, want 1: %+v", len(pp.Outer), pp.Outer)
	}
	got := SplitPipeParts(pp.Outer[0].V.Value)
	if want := []string{"it's", "no"}; !slices.Equal(got, want) {
		t.Errorf("oneof parts = %q, want %q (raw value %q)", got, want, pp.Outer[0].V.Value)
	}

	// Same for a two-part mod value.
	pp, err = parsePipeTag(`replace='don\'t'|x`)
	if err != nil {
		t.Fatalf("replace parse: %v", err)
	}
	if len(pp.Outer) != 1 || !pp.Outer[0].IsMod {
		t.Fatalf("want one mod step, got %+v", pp.Outer)
	}
	got = SplitPipeParts(pp.Outer[0].M.Value)
	if want := []string{"don't", "x"}; !slices.Equal(got, want) {
		t.Errorf("replace parts = %q, want %q", got, want)
	}
}
