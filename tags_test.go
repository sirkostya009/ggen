package main

import (
	"reflect"
	"testing"
)

func TestParseValidationTag(t *testing.T) {
	tests := []struct {
		input string
		want  ValidationTag
	}{
		// Default HintLen for an unset hint is -1 (the "fall through to
		// len/minlen/default" sentinel in mapPreallocCap/preallocCap).
		{"", ValidationTag{HintLen: -1}},
		{"required", ValidationTag{HintLen: -1, Outer: []ValidationRule{{Name: "required"}}}},
		{"required,minlen=2,maxlen=23", ValidationTag{HintLen: -1, Outer: []ValidationRule{
			{Name: "required"},
			{Name: "minlen", Value: "2"},
			{Name: "maxlen", Value: "23"},
		}}},
		{"min=0,max=100", ValidationTag{HintLen: -1, Outer: []ValidationRule{
			{Name: "min", Value: "0"},
			{Name: "max", Value: "100"},
		}}},
		{"minlen=1,maxlen=5,dive:minlen=3,maxlen=20", ValidationTag{
			HintLen: -1,
			Outer: []ValidationRule{
				{Name: "minlen", Value: "1"},
				{Name: "maxlen", Value: "5"},
			},
			Levels: [][]ValidationRule{{
				{Name: "minlen", Value: "3"},
				{Name: "maxlen", Value: "20"},
			}},
		}},
		{"dive:required", ValidationTag{HintLen: -1, Levels: [][]ValidationRule{{{Name: "required"}}}}},
		// Multi-level dive: each `dive:` peels one level.
		{"minlen=1,dive:maxlen=10,dive:required", ValidationTag{
			HintLen: -1,
			Outer:   []ValidationRule{{Name: "minlen", Value: "1"}},
			Levels: [][]ValidationRule{
				{{Name: "maxlen", Value: "10"}},
				{{Name: "required"}},
			},
		}},
		// Keys: only-for-maps bucket.
		{"minlen=1,keys:minrunes=3,maxrunes=32,dive:required", ValidationTag{
			HintLen: -1,
			Outer:   []ValidationRule{{Name: "minlen", Value: "1"}},
			Keys:    []ValidationRule{{Name: "minrunes", Value: "3"}, {Name: "maxrunes", Value: "32"}},
			Levels:  [][]ValidationRule{{{Name: "required"}}},
		}},
		// hintlen is lifted out of the rule list entirely.
		{"hintlen=16,dive:maxlen=10", ValidationTag{
			HintLen: 16,
			Levels:  [][]ValidationRule{{{Name: "maxlen", Value: "10"}}},
		}},
		// hintlen=0 is a user opt-out (explicit "no prealloc"),
		// distinct from unset.
		{"hintlen=0", ValidationTag{HintLen: 0}},
	}
	for _, tt := range tests {
		got := parseValidationTag(tt.input)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseValidationTag(%q)\n got = %+v\nwant = %+v", tt.input, got, tt.want)
		}
	}
}

func TestParseModTag(t *testing.T) {
	tests := []struct {
		input string
		want  ModTag
	}{
		{"", ModTag{}},
		{"trim,lower", ModTag{Outer: []ModRule{{Name: "trim"}, {Name: "lower"}}}},
		{"trim,dive:upper,dive:trim", ModTag{
			Outer: []ModRule{{Name: "trim"}},
			Levels: [][]ModRule{
				{{Name: "upper"}},
				{{Name: "trim"}},
			},
		}},
		{"keys:trim,lower", ModTag{Keys: []ModRule{{Name: "trim"}, {Name: "lower"}}}},
	}
	for _, tt := range tests {
		got := parseModTag(tt.input)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseModTag(%q)\n got = %+v\nwant = %+v", tt.input, got, tt.want)
		}
	}
}

func TestParseJSONTag(t *testing.T) {
	tests := []struct {
		input       string
		wantName    string
		wantOpts    JSONOptions
		wantIgnored bool
	}{
		{"", "", JSONOptions{}, false},
		{"field1", "field1", JSONOptions{}, false},
		{"field1,omitempty", "field1", JSONOptions{OmitEmpty: true}, false},
		{"field1,omitzero", "field1", JSONOptions{OmitZero: true}, false},
		{"field1,string", "field1", JSONOptions{String: true}, false},
		{"field1,omitempty,omitzero,string", "field1", JSONOptions{OmitEmpty: true, OmitZero: true, String: true}, false},
		{"-", "", JSONOptions{}, true},
		{",omitempty", "", JSONOptions{OmitEmpty: true}, false},
	}
	for _, tt := range tests {
		name, opts, ignored := parseJSONTag(tt.input)
		if name != tt.wantName || opts != tt.wantOpts || ignored != tt.wantIgnored {
			t.Errorf("parseJSONTag(%q) = (%q, %+v, %v), want (%q, %+v, %v)",
				tt.input, name, opts, ignored, tt.wantName, tt.wantOpts, tt.wantIgnored)
		}
	}
}
