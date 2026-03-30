package main

import (
	"reflect"
	"testing"
)

func TestParseValidationTag(t *testing.T) {
	tests := []struct {
		input string
		want  []ValidationRule
	}{
		{"", nil},
		{"required", []ValidationRule{{Name: "required"}}},
		{"required,minlen=2,maxlen=23", []ValidationRule{
			{Name: "required"},
			{Name: "minlen", Value: "2"},
			{Name: "maxlen", Value: "23"},
		}},
		{"min=0,max=100", []ValidationRule{
			{Name: "min", Value: "0"},
			{Name: "max", Value: "100"},
		}},
	}
	for _, tt := range tests {
		got := parseValidationTag(tt.input)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseValidationTag(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestParseJSONTag(t *testing.T) {
	tests := []struct {
		input       string
		wantName    string
		wantIgnored bool
	}{
		{"", "", false},
		{"field1", "field1", false},
		{"field1,omitempty", "field1", false},
		{"-", "", true},
		{",omitempty", "", false},
	}
	for _, tt := range tests {
		name, ignored := parseJSONTag(tt.input)
		if name != tt.wantName || ignored != tt.wantIgnored {
			t.Errorf("parseJSONTag(%q) = (%q, %v), want (%q, %v)", tt.input, name, ignored, tt.wantName, tt.wantIgnored)
		}
	}
}
