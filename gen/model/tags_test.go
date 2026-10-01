package model

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
		// jsonv2 quoting: commas stay inside single quotes, and \' is a
		// literal quote.
		{"t,format:'Jan 2, 2006'", "t", JSONOptions{Format: "Jan 2, 2006"}, false, false},
		{"t,format:RFC3339", "t", JSONOptions{Format: "RFC3339"}, false, false},
		{"'a,b'", "a,b", JSONOptions{}, false, false},
		{`'it\'s',omitempty`, "it's", JSONOptions{OmitEmpty: true}, false, false},
		{"'-'", "-", JSONOptions{}, false, false},
		// The catch-all map has no name and no other option, and `inline` is
		// rejected instead of being read as a name.
		{",embed", "", JSONOptions{Embed: true}, false, false},
		{",inline", "", JSONOptions{}, false, true},
		{"extra,inline", "", JSONOptions{}, false, true},
		{"extra,embed", "", JSONOptions{}, false, true},
		{",embed,omitempty", "", JSONOptions{}, false, true},
		{",embed,string", "", JSONOptions{}, false, true},
		// forms jsonv2 rejects: `-` with options, and empty options.
		{"-,", "", JSONOptions{}, false, true},
		{"-,omitempty", "", JSONOptions{}, false, true},
		{"a,", "", JSONOptions{}, false, true},
		{"a,,omitempty", "", JSONOptions{}, false, true},
		// The name is kept as written, whitespace included, as in
		// encoding/json and jsonv2. An option padded with whitespace is an
		// error, as in jsonv2: "invalid character ' ' at start of option".
		{" a", " a", JSONOptions{}, false, false},
		{"a ", "a ", JSONOptions{}, false, false},
		{"' a '", " a ", JSONOptions{}, false, false},
		{"a, omitempty", "", JSONOptions{}, false, true},
		{"a,omitempty ", "", JSONOptions{}, false, true},
		// `case:` asks for case-insensitive matching, which is not
		// implemented, so it is rejected instead of silently matching
		// exactly. Near-miss spellings of a known option are rejected, as
		// jsonv2 does. A truly unknown word still passes.
		{"a,case:ignore", "", JSONOptions{}, false, true},
		{"a,case:strict", "", JSONOptions{}, false, true},
		{"a,case", "", JSONOptions{}, false, true},
		{"a,omitEmpty", "", JSONOptions{}, false, true},
		{"a,omit_empty", "", JSONOptions{}, false, true},
		{"a,OMITZERO", "", JSONOptions{}, false, true},
		{"a,String", "", JSONOptions{}, false, true},
		{"a,Embed", "", JSONOptions{}, false, true},
		{"a,Inline", "", JSONOptions{}, false, true},
		{"a,Format:hex", "", JSONOptions{}, false, true},
		{"a,format", "", JSONOptions{}, false, true},
		{"a,nocase", "a", JSONOptions{}, false, false},
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
