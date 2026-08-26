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
		wantErr     bool
	}{
		{"", "", JSONOptions{}, false, false},
		{"field1", "field1", JSONOptions{}, false, false},
		{"field1,omitempty", "field1", JSONOptions{OmitEmpty: true}, false, false},
		{"field1,omitzero", "field1", JSONOptions{OmitZero: true}, false, false},
		{"field1,string", "field1", JSONOptions{String: true}, false, false},
		{"field1,omitempty,omitzero,string", "field1", JSONOptions{OmitEmpty: true, OmitZero: true, String: true}, false, false},
		{"-", "", JSONOptions{}, true, false},
		{",omitempty", "", JSONOptions{OmitEmpty: true}, false, false},
		// jsonv2 quoting: commas survive inside single quotes, \' is literal.
		{"t,format:'Jan 2, 2006'", "t", JSONOptions{Format: "Jan 2, 2006"}, false, false},
		{"t,format:RFC3339", "t", JSONOptions{Format: "RFC3339"}, false, false},
		{"'a,b'", "a,b", JSONOptions{}, false, false},
		{`'it\'s',omitempty`, "it's", JSONOptions{OmitEmpty: true}, false, false},
		{"'-'", "-", JSONOptions{}, false, false},
		// The embedded fallback carries no name and no companion option, and
		// the older `inline` spelling is rejected rather than read as a name.
		{",embed", "", JSONOptions{Embed: true}, false, false},
		{",inline", "", JSONOptions{}, false, true},
		{"extra,inline", "", JSONOptions{}, false, true},
		{"extra,embed", "", JSONOptions{}, false, true},
		{",embed,omitempty", "", JSONOptions{}, false, true},
		{",embed,string", "", JSONOptions{}, false, true},
		// jsonv2 malformed forms: `-` with options, empty options.
		{"-,", "", JSONOptions{}, false, true},
		{"-,omitempty", "", JSONOptions{}, false, true},
		{"a,", "", JSONOptions{}, false, true},
		{"a,,omitempty", "", JSONOptions{}, false, true},
	}
	for _, tt := range tests {
		name, opts, ignored, err := parseJSONTag(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseJSONTag(%q) err = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if name != tt.wantName || opts != tt.wantOpts || ignored != tt.wantIgnored {
			t.Errorf("parseJSONTag(%q) = (%q, %+v, %v), want (%q, %+v, %v)",
				tt.input, name, opts, ignored, tt.wantName, tt.wantOpts, tt.wantIgnored)
		}
	}
}
