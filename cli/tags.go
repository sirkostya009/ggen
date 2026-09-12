package main

import (
	"fmt"
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
	Embed     bool   // embedded fallback: map absorbs unknown keys, entries splice into parent object
}

// parseJSONTag follows jsonv2's tag grammar: the name is taken verbatim
// (whitespace included — v1 and v2 both keep it), options split on commas
// OUTSIDE single-quoted regions (`format:'Jan 2, 2006'`, name `'a,b'`), `\'`
// is a literal quote, an empty or whitespace-padded option is malformed, and a
// bare `-` name with options is rejected — quote it (`'-'`) for a field
// literally named "-". Unknown option words pass silently (jsonv2 parity)
// with three exceptions, each a would-be silent no-op: `inline` (the older
// catch-all spelling), `case:` (case-insensitive key matching, which ggen
// does not implement), and a near-miss spelling of a known option
// (`omitEmpty`, `omit_empty`), which jsonv2 itself rejects.
func parseJSONTag(tag string) (name string, opts JSONOptions, ignored bool, err error) {
	if tag == "" {
		return "", JSONOptions{}, false, nil
	}
	if tag == "-" {
		return "", JSONOptions{}, true, nil
	}
	parts, unterminated := splitTagOpts(tag)
	if unterminated {
		return "", JSONOptions{}, false, fmt.Errorf("json tag %q: unterminated quoted section (odd number of `'`); escape a literal quote as \\'", tag)
	}
	name = parts[0]
	if name == "-" && len(parts) > 1 {
		return "", JSONOptions{}, false, fmt.Errorf(`json tag %q: use json:"-" to ignore the field, or json:"'-'" for a field named "-"`, tag)
	}
	name = unquoteTagValue(name)
	for _, opt := range parts[1:] {
		if opt == "" {
			return "", JSONOptions{}, false, fmt.Errorf("json tag %q: empty option", tag)
		}
		if opt != strings.TrimSpace(opt) {
			return "", JSONOptions{}, false, fmt.Errorf("json tag %q: option %q is padded with whitespace", tag, opt)
		}
		if rest, ok := strings.CutPrefix(opt, "format:"); ok {
			opts.Format = unquoteTagValue(rest)
			continue
		}
		switch opt {
		case "omitempty":
			opts.OmitEmpty = true
		case "omitzero":
			opts.OmitZero = true
		case "string":
			opts.String = true
		case "embed":
			opts.Embed = true
		default:
			if err := checkTagOptionWord(tag, opt); err != nil {
				return "", JSONOptions{}, false, err
			}
		}
	}
	// jsonv2: an embedded fallback carries no name and no other option, since
	// its entries splice into the parent rather than sitting under a key.
	if opts.Embed {
		switch {
		case name != "":
			return "", JSONOptions{}, false, fmt.Errorf("json tag %q: `embed` cannot carry a JSON name — its entries splice into the parent object", tag)
		case opts.OmitEmpty || opts.OmitZero || opts.String || opts.Format != "":
			return "", JSONOptions{}, false, fmt.Errorf("json tag %q: `embed` cannot be combined with another option", tag)
		}
	}
	return name, opts, false, nil
}

// checkTagOptionWord judges an option word parseJSONTag has no arm for. The
// word before a `:` is what jsonv2 keys on, normalised the way it does
// (lower-case, underscores dropped) so `omitEmpty`/`omit_empty`/`Format:hex`
// land on the option they were meant to be.
func checkTagOptionWord(tag, opt string) error {
	head, _, _ := strings.Cut(opt, ":")
	switch norm := strings.ReplaceAll(strings.ToLower(head), "_", ""); norm {
	case "inline":
		return fmt.Errorf("json tag %q: `inline` is not a tag option — the catch-all map is `json:\",embed\"` (jsonv2 spells it `embed`)", tag)
	case "case":
		return fmt.Errorf("json tag %q: `case:` is not supported — ggen matches JSON keys exactly; drop the option", tag)
	case "embed", "omitzero", "omitempty", "string", "format":
		return fmt.Errorf("json tag %q: invalid appearance of `%s` tag option; specify `%s` instead", tag, opt, norm)
	}
	return nil
}

// splitTagOpts splits a json tag on commas outside single-quoted regions.
// The second result reports an unterminated quoted section (odd quote count),
// which would otherwise swallow the option separators and leave the quote
// character sitting in the wire key.
func splitTagOpts(tag string) ([]string, bool) {
	var parts []string
	start, quoted := 0, false
	for i := 0; i < len(tag); i++ {
		switch tag[i] {
		case '\\':
			if quoted && i+1 < len(tag) && tag[i+1] == '\'' {
				i++
			}
		case '\'':
			quoted = !quoted
		case ',':
			if !quoted {
				parts = append(parts, tag[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, tag[start:]), quoted
}

// unquoteTagValue strips one level of single quotes and unescapes \'.
func unquoteTagValue(s string) string {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		s = strings.ReplaceAll(s[1:len(s)-1], `\'`, "'")
	}
	return s
}
