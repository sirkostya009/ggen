package main

import (
	"testing"
)

func TestParseJSONTag(t *testing.T) {
	t.Parallel()
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
