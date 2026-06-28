package main

import (
	"reflect"
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
		"required optional",     // mutually exclusive
		"foo / / bar ~ x",       // empty variant
		"@ ~ x",                 // empty @ ref
		"trim / lower",          // slash outside the decode stage
		"inner:trim ; maxlen=1", // `;` not a valid glyph
		"inner:(trim maxlen=1",  // unbalanced paren
		"inner:",                // prefix with nothing following
		"(trim)",                // stray group
	}
	for _, tag := range bad {
		if _, err := parsePipeTag(tag); err == nil {
			t.Errorf("parsePipeTag(%q) expected error, got nil", tag)
		}
	}
}
