package model

import (
	"fmt"
	"strings"
)

// JSONOptions holds the options of a `json` tag other than the name. Decode,
// transform and validation settings live in the `pipe:` tag, and prealloc
// sizes in `hint:`. That keeps the json tag compatible with encoding/json and
// jsonv2.
type JSONOptions struct {
	OmitEmpty bool   // skip on marshal when JSON-empty: null, "", [] or {}
	OmitZero  bool   // skip on marshal when the Go zero value
	String    bool   // marshal a primitive as a JSON string, unwrap it on unmarshal
	Format    string // jsonv2 format option, for example RFC3339, unix, hex, base64
	Embed     bool   // catch-all map: takes unknown keys, its entries splice into the parent object
}

// parseJSONTag follows jsonv2's tag grammar:
//   - The name is taken as written, whitespace included. encoding/json and
//     jsonv2 both keep it.
//   - Options split on commas outside single-quoted parts, as in
//     `format:'Jan 2, 2006'` or the name `'a,b'`. `\'` is a literal quote.
//   - An empty option or one padded with whitespace is an error.
//   - A bare `-` name with options is an error. A field named "-" must be
//     quoted as `'-'`.
//
// Unknown option words are ignored, like jsonv2 does. There are three
// exceptions, because ignoring them would silently do nothing: `inline`,
// which is not how the catch-all map is spelled, `case:`, which asks for
// case-insensitive key matching that ggen does not implement, and a near-miss
// spelling of a known option such as `omitEmpty` or `omit_empty`, which
// jsonv2 also rejects.
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
	// As in jsonv2, a catch-all map has no name and no other option. Its
	// entries splice into the parent object instead of sitting under a key.
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

// checkTagOptionWord checks an option word that parseJSONTag does not know.
// Like jsonv2, it looks at the word before any `:` and normalises it to lower
// case with underscores removed. That way `omitEmpty`, `omit_empty` and
// `Format:hex` match the option they were meant to be.
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

// splitTagOpts splits a json tag on commas outside single-quoted parts.
// The second result reports an unterminated quoted part, meaning an odd
// number of quotes. Without this check such a quote would hide the commas
// after it and end up as part of the JSON key.
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
