package main

import (
	"strings"
)

// JSONOptions captures the encoding hints from the `json` tag beyond the name.
// Decode/transform/validate config lives in the `pipe:` tag; prealloc in
// `hint:`. The json tag stays stdlib/jsonv2-compatible.
type JSONOptions struct {
	OmitEmpty bool   // omit during marshal if JSON-empty (null, "", [], {})
	OmitZero  bool   // omit during marshal if Go zero value
	String    bool   // wrap primitive as a JSON string on marshal, unwrap on unmarshal
	Format    string // jsonv2 format flag (e.g., RFC3339, unix, hex, base64)
	Inline    bool   // catch-all: map absorbs unknown keys, entries splice into parent object
}

func parseJSONTag(tag string) (name string, opts JSONOptions, ignored bool) {
	if tag == "" {
		return "", JSONOptions{}, false
	}
	parts := strings.Split(tag, ",")
	name = strings.TrimSpace(parts[0])
	if name == "-" {
		return "", JSONOptions{}, true
	}
	for _, opt := range parts[1:] {
		opt = strings.TrimSpace(opt)
		if rest, ok := strings.CutPrefix(opt, "format:"); ok {
			// strip single quotes
			if len(rest) >= 2 && rest[0] == '\'' && rest[len(rest)-1] == '\'' {
				rest = rest[1 : len(rest)-1]
			}
			opts.Format = rest
			continue
		}
		switch opt {
		case "omitempty":
			opts.OmitEmpty = true
		case "omitzero":
			opts.OmitZero = true
		case "string":
			opts.String = true
		case "inline":
			opts.Inline = true
		}
	}
	return name, opts, false
}
