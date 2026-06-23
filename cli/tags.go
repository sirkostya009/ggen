package main

import (
	"fmt"
	"strconv"
	"strings"
)

// ValidationTag holds everything parsed out of a `ggen:"..."` tag after the
// mode prefixes (`keys:`, `dive:`) are applied:
//
//   - Outer    — rules on the field itself (or the whole slice/map).
//   - Keys     — rules after `keys:` — map keys only (always strings).
//   - Levels   — rules after each `dive:` token. Levels[0] is the first dive
//     (per-element for slices, per-value for maps); Levels[1] is the second
//     dive (per-element of the inner slice), and so on.
//   - HintLen  — explicit preallocation hint (`hintlen=N`), pulled out of
//     whichever bucket it was declared in; slice/map sizing uses it ahead of
//     `len`/`maxlen`/`minlen`.
type ValidationTag struct {
	Outer   []ValidationRule
	Keys    []ValidationRule
	Levels  [][]ValidationRule
	HintLen int
}

// ModTag mirrors ValidationTag for the `mod:"..."` tag.
type ModTag struct {
	Outer  []ModRule
	Keys   []ModRule
	Levels [][]ModRule
}

// parseValidationTag splits a comma-separated tag into buckets keyed by mode
// prefixes. `dive:` switches to the next level (each additional `dive:` peels
// one more level); `keys:` switches to the map-key bucket. A bucket is only
// allocated when rules are written into it.
//
//	`ggen:"minlen=1,dive:maxlen=10,dive:required,keys:minrunes=3"`
//
// → Outer: minlen=1; Levels[0]: maxlen=10; Levels[1]: required; Keys: minrunes=3.
//
// HintLen is initialized to -1 (sentinel "unset"). `hintlen=0` is a
// valid user opt-out — explicitly disable any prealloc on this field.
// Negative explicit values surface as parse errors via the
// `_HintLenInvalid` sentinel; callers that need parse-time errors
// should use parseValidationTagE.
func parseValidationTag(tag string) ValidationTag {
	out, _ := parseValidationTagE(tag)
	return out
}

// parseValidationTagE is the error-returning variant — surfaces
// invalid `hintlen=N` (negative, non-numeric).
func parseValidationTagE(tag string) (ValidationTag, error) {
	out := ValidationTag{HintLen: -1}
	if tag == "" {
		return out, nil
	}
	// target points at the bucket the next rule will land in. For Levels
	// we track an explicit index instead of a pointer because the underlying
	// slice is reallocated on growth.
	const (
		modeOuter = iota
		modeKeys
		modeLevels
	)
	mode := modeOuter
	lvl := -1 // -1 until the first `dive:` pushes level 0

	for _, part := range strings.Split(tag, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// Mode-switch prefixes can appear repeatedly. A lone `dive:` with no
		// trailing rule still advances the level.
		if rest, ok := strings.CutPrefix(part, "dive:"); ok {
			mode = modeLevels
			lvl++
			out.Levels = append(out.Levels, nil)
			part = strings.TrimSpace(rest)
			if part == "" {
				continue
			}
		} else if rest, ok := strings.CutPrefix(part, "keys:"); ok {
			mode = modeKeys
			part = strings.TrimSpace(rest)
			if part == "" {
				continue
			}
		}
		name, value, _ := strings.Cut(part, "=")
		// hintlen is a sizing hint, not a validation — pull it into a
		// dedicated field so renderers don't accidentally emit a runtime
		// check for it. It's only meaningful on the outer bucket.
		// `hintlen=0` is a valid opt-out (explicit "no prealloc");
		// negative values are a user error.
		if name == "hintlen" {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return out, fmt.Errorf("hintlen=%q is not a valid integer", value)
			}
			if n < 0 {
				return out, fmt.Errorf("hintlen=%d must be ≥ 0 (use 0 to explicitly disable prealloc)", n)
			}
			out.HintLen = n
			continue
		}
		rule := ValidationRule{Name: name, Value: value}
		switch mode {
		case modeKeys:
			out.Keys = append(out.Keys, rule)
		case modeLevels:
			out.Levels[lvl] = append(out.Levels[lvl], rule)
		default:
			out.Outer = append(out.Outer, rule)
		}
	}
	return out, nil
}

// parseModTag mirrors parseValidationTag's `dive:` / `keys:` semantics for
// the `mod:"..."` tag. Each `dive:` advances the per-element level.
func parseModTag(tag string) ModTag {
	var out ModTag
	if tag == "" {
		return out
	}
	const (
		modeOuter = iota
		modeKeys
		modeLevels
	)
	mode := modeOuter
	lvl := -1

	for _, part := range strings.Split(tag, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(part, "dive:"); ok {
			mode = modeLevels
			lvl++
			out.Levels = append(out.Levels, nil)
			part = strings.TrimSpace(rest)
			if part == "" {
				continue
			}
		} else if rest, ok := strings.CutPrefix(part, "keys:"); ok {
			mode = modeKeys
			part = strings.TrimSpace(rest)
			if part == "" {
				continue
			}
		}
		name, value, _ := strings.Cut(part, "=")
		rule := ModRule{Name: name, Value: value}
		switch mode {
		case modeKeys:
			out.Keys = append(out.Keys, rule)
		case modeLevels:
			out.Levels[lvl] = append(out.Levels[lvl], rule)
		default:
			out.Outer = append(out.Outer, rule)
		}
	}
	return out
}

// JSONOptions captures the encoding hints from the `json` tag beyond the name.
type JSONOptions struct {
	OmitEmpty bool   // omit during marshal if JSON-empty (null, "", [], {})
	OmitZero  bool   // omit during marshal if Go zero value
	NullZero  bool   // decode: accept explicit JSON null on a non-pointer value field (null → Go zero) instead of erroring
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
			// Single-quoted literal: strip quotes.
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
		case "nullzero":
			opts.NullZero = true
		case "string":
			opts.String = true
		case "inline":
			opts.Inline = true
		}
	}
	return name, opts, false
}
