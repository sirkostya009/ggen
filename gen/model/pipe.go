package model

import (
	"errors"
	"fmt"
	"go/types"
	"reflect"
	"strconv"
	"strings"
)

// This file parses the `pipe:` and `hint:` struct tags. The pipe describes
// how a field is decoded, changed and validated, in order. Grammar:
//
//	pipe        := stage ( "~" stage )*
//	first stage := variant ( "/" variant )*    // decode, picked by JSON shape
//	later stage := step ( WS step )*            // change or validate the value
//	                with inner: and keys: for container levels
//
// `required` and `optional` are taken out first and may appear anywhere. A
// Step holds either a ValidationRule or a ModRule. Mods and validators share
// one ordered list per level.

// Presence says whether the field's JSON key must appear. It is separate
// from the value checks, which are pipe steps, and from null handling, which
// is the nullzero variant.
type Presence uint8

const (
	PresenceDefault  Presence = iota // no marker: a missing key leaves the zero value, no error
	PresenceRequired                 // `required`: the key must appear
	PresenceOptional                 // `optional`: does nothing, it only documents intent
)

// Step is one element of the pipeline, a validator or a mod. IsMod says
// which, and only that one of V and M is used.
type Step struct {
	IsMod bool
	V     ValidationRule // when !IsMod
	M     ModRule        // when IsMod
}

// VariantKind tags a decode-stage variant.
type VariantKind uint8

const (
	VariantNative   VariantKind = iota // ".": the normal decode of the field type T
	VariantNullZero                    // "nullzero": JSON null sets the zero value of T
	VariantConvert                     // "@Conv": decode input type W, then call func(W) T
)

// Variant is one decode-stage alternative. ggen looks at the first byte of
// the incoming JSON value and uses the one variant that claims that shape.
type Variant struct {
	Kind VariantKind

	// The resolved converter, for VariantConvert only. Filled at parse time.
	Custom    bool
	PkgImport string
	PkgName   string
	FuncName  string
	Fallible  bool       // func(W) (T, error) or func(W) (T, bool), as opposed to func(W) T
	BoolForm  bool       // func(W) (T, bool), whose failure can carry an inline message
	Msg       string     // inline `:message`, bool form only
	InType    string     // Go type of the converter input W, the type decoded from JSON
	In        types.Type // resolved W, nil in the AST-only fallback mode
	InKind    TypeKind   // kind of W with all pointer levels removed
	InPointer bool       // W is `*T`: decoding null gives nil, so the variant also claims 'n'
}

// ParsedPipe is the result of parsePipeTag, before it is copied onto a
// FieldInfo.
type ParsedPipe struct {
	Presence Presence
	Variants []Variant // decode stage. Empty means one implicit native variant
	Outer    []Step    // value steps on the field or whole container, after decode
	Keys     []Step    // map key steps from `keys:`
	Levels   [][]Step  // nested levels: Levels[0] per element, Levels[1] one deeper, and so on
}

// ptokKind is the kind of a token produced by tokenizePipe.
type ptokKind uint8

const (
	ptWord   ptokKind = iota // text of a step or variant, may start with inner: or keys:
	ptSlash                  // "/" between variants
	ptTilde                  // "~" between stages
	ptLParen                 // "(" opens an inner: or keys: group
	ptRParen                 // ")" closes a group
	ptSemi                   // ";" is not valid, it is a token only to give a helpful error
)

type ptok struct {
	kind ptokKind
	text string // for ptWord
}

// tokenizePipe splits a `pipe:` tag into a flat list of tokens. Words are
// separated by whitespace. The characters `/`, `~`, `(` and `)` end a word
// and become their own token even without spaces around them, so `./@F` and
// `. / @F` give the same tokens. A single-quoted part inside a word is read
// as is: whitespace and those characters do not end the word there. `\'` is
// a literal quote.
func tokenizePipe(tag string) ([]ptok, error) {
	var toks []ptok
	i := 0
	n := len(tag)
	for i < n {
		c := tag[i]
		switch c {
		case ' ', '\t', '\n', '\r':
			i++
		case '/':
			toks = append(toks, ptok{kind: ptSlash})
			i++
		case '~':
			toks = append(toks, ptok{kind: ptTilde})
			i++
		case '(':
			toks = append(toks, ptok{kind: ptLParen})
			i++
		case ')':
			toks = append(toks, ptok{kind: ptRParen})
			i++
		case ';':
			toks = append(toks, ptok{kind: ptSemi})
			i++
		default:
			var sb strings.Builder
			for i < n {
				ch := tag[i]
				if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' ||
					ch == '/' || ch == '~' || ch == '(' || ch == ')' || ch == ';' {
					break
				}
				if ch == '\'' {
					// Quoted part: copy it as is up to the closing quote, and
					// keep the `\'` escape. Unescaping here would hand
					// SplitPipeParts a bare quote that it reads as a real
					// delimiter, and `oneof='it\'s'|no` would become one broken
					// entry. stripQuotes and SplitPipeParts unescape later,
					// after splitting the parts.
					sb.WriteByte('\'')
					i++
					closed := false
					for i < n {
						if tag[i] == '\\' && i+1 < n && tag[i+1] == '\'' {
							sb.WriteString(`\'`)
							i += 2
							continue
						}
						if tag[i] == '\'' {
							i++
							closed = true
							break
						}
						sb.WriteByte(tag[i])
						i++
					}
					if !closed {
						// Closing it implicitly would silently change the
						// rule: `oneof='New York|LA` would read as one part.
						return nil, errors.New("unterminated ' quote in pipe tag")
					}
					sb.WriteByte('\'')
					continue
				}
				sb.WriteByte(ch)
				i++
			}
			toks = append(toks, ptok{kind: ptWord, text: sb.String()})
		}
	}
	return toks, nil
}

// SplitPipeParts splits a multi-part rule value, as used by oneof, replace
// and clamp, on `|` outside single-quoted parts. It removes one layer of
// quotes from each part. So `oneof='New York'|LA` quotes a space, and
// `replace='a|b'|c` quotes a literal `|`. parseStep leaves multi-part values
// with their quotes, so the quote positions are still there for this split.
func SplitPipeParts(s string) []string {
	var parts []string
	start, quoted := 0, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if quoted && i+1 < len(s) && s[i+1] == '\'' {
				i++
			}
		case '\'':
			quoted = !quoted
		case '|':
			if !quoted {
				parts = append(parts, stripQuotes(s[start:i]))
				start = i + 1
			}
		}
	}
	return append(parts, stripQuotes(s[start:]))
}

// stripQuotes removes one layer of surrounding single quotes from s and
// turns \' into '. Input without quotes is returned unchanged.
func stripQuotes(s string) string {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		s = s[1 : len(s)-1]
		s = strings.ReplaceAll(s, `\'`, `'`)
	}
	return s
}

// parsePipeTag parses a `pipe:"..."` tag value into a ParsedPipe. It does not
// resolve `@Func` signatures or check that rules fit the field type. Those
// steps run later with type information, in resolvePipeCustoms and
// checkRuleApplicability.
func parsePipeTag(tag string) (ParsedPipe, error) {
	var out ParsedPipe
	if strings.TrimSpace(tag) == "" {
		return out, nil
	}
	toks, err := tokenizePipe(tag)
	if err != nil {
		return out, err
	}

	// First pass: take the presence markers `required` and `optional` out
	// of the top-level tokens. Inside an `inner:(…)` or `keys:(…)` group, or
	// right after a bare prefix word, they stay in place so parseStep rejects
	// them. Presence belongs to the field's key. An element or a map key has
	// no presence, so taking the marker out there would silently apply it to
	// the outer field.
	filtered := toks[:0:0]
	depth := 0
	for i, t := range toks {
		//exhaustive:ignore not every kind applies here
		switch t.kind {
		case ptLParen:
			depth++
		case ptRParen:
			depth--
		case ptWord:
			if depth != 0 || (i > 0 && isBarePrefix(toks[i-1])) {
				break
			}
			switch t.text {
			case "required":
				if out.Presence == PresenceOptional {
					return out, errors.New("`required` and `optional` are mutually exclusive")
				}
				out.Presence = PresenceRequired
				continue
			case "optional":
				if out.Presence == PresenceRequired {
					return out, errors.New("`required` and `optional` are mutually exclusive")
				}
				out.Presence = PresenceOptional
				continue
			}
		}
		filtered = append(filtered, t)
	}
	toks = filtered

	// Find where the decode stage ends. An explicit `~` separates the decode
	// stage from the value stage. Without `~`, the decode stage is the run of
	// variant words at the start: `.`, `nullzero`, or converters joined by
	// `/`. A lone `@Func` at the start stays a value step. A converter is only
	// read as a decode variant next to `/`, `~` or `.`.
	tildeAt := -1
	for j, t := range toks {
		if t.kind == ptTilde {
			tildeAt = j
			break
		}
	}

	var decodeToks, valueToks []ptok
	if tildeAt >= 0 {
		decodeToks = toks[:tildeAt]
		valueToks = toks[tildeAt+1:]
	} else {
		n := leadingDecodeExtent(toks)
		decodeToks = toks[:n]
		valueToks = toks[n:]
	}

	if len(decodeToks) > 0 {
		out.Variants, err = parseVariants(decodeToks)
		if err != nil {
			return out, err
		}
	}
	if err := parseValueSteps(valueToks, &out); err != nil {
		return out, err
	}
	return out, nil
}

// isBarePrefix reports whether t is a bare `inner:` or `keys:` word, whose
// step is the next token.
func isBarePrefix(t ptok) bool {
	return t.kind == ptWord && (t.text == "inner:" || t.text == "keys:")
}

// leadingDecodeExtent returns how many tokens at the start, `/` separators
// included, form an implicit decode stage when there is no `~`. The stage
// starts only when the first token is `.` or `nullzero`, or a `@Func`
// directly followed by `/`. It then takes the run of variants joined by `/`.
// It returns 0 when the whole pipe is value steps.
func leadingDecodeExtent(toks []ptok) int {
	isVariantWord := func(t ptok) bool {
		return t.kind == ptWord && (t.text == "." || t.text == "nullzero" || strings.HasPrefix(t.text, "@"))
	}
	if len(toks) == 0 || toks[0].kind != ptWord {
		return 0
	}
	first := toks[0]
	starts := first.text == "." || first.text == "nullzero" ||
		(strings.HasPrefix(first.text, "@") && len(toks) >= 2 && toks[1].kind == ptSlash)
	if !starts {
		return 0
	}
	i := 0
	for i < len(toks) && isVariantWord(toks[i]) {
		i++
		if i < len(toks) && toks[i].kind == ptSlash {
			i++ // another variant must follow
			continue
		}
		break
	}
	return i
}

// parseVariants turns the decode-stage tokens into a variant list. Variants
// are separated by `/`, and each is one word: `.`, `nullzero` or `@Conv`.
func parseVariants(toks []ptok) ([]Variant, error) {
	var vars []Variant
	expectWord := true
	for _, t := range toks {
		//exhaustive:ignore not every kind applies here
		switch t.kind {
		case ptSlash:
			if expectWord {
				return nil, errors.New("empty decode variant around `/`")
			}
			expectWord = true
		case ptTilde, ptSemi:
			return nil, fmt.Errorf("`%s` is not valid in the decode stage", glyph(t.kind))
		case ptWord:
			if !expectWord {
				return nil, fmt.Errorf("decode variant %q must be separated by `/`", t.text)
			}
			v, err := parseOneVariant(t.text)
			if err != nil {
				return nil, err
			}
			vars = append(vars, v)
			expectWord = false
		}
	}
	if expectWord {
		return nil, errors.New("trailing `/` with no decode variant")
	}
	return vars, nil
}

func parseOneVariant(word string) (Variant, error) {
	switch {
	case word == ".":
		return Variant{Kind: VariantNative}, nil
	case word == "nullzero":
		return Variant{Kind: VariantNullZero}, nil
	case strings.HasPrefix(word, "@"):
		ref, msg := splitFuncMsg(word[1:])
		if ref == "" {
			return Variant{}, errors.New("empty `@` converter reference")
		}
		return Variant{Kind: VariantConvert, FuncName: ref, Msg: msg}, nil
	}
	return Variant{}, fmt.Errorf("decode variant %q must be `.`, `nullzero`, or `@Converter`", word)
}

// parseValueSteps parses the value-stage tokens into out.Outer, out.Keys and
// out.Levels. `inner:` applies steps to the next nested level and `keys:` to
// map keys. A bare prefix takes exactly one step, as in `inner:trim`.
// Parentheses group several, as in `inner:(trim maxlen=20)`, and inner groups
// can nest, as in `inner:(a inner:(b))`.
func parseValueSteps(toks []ptok, out *ParsedPipe) error {
	return parseScope(toks, 0, out)
}

// parseScope parses the tokens of one container level. Level 0 goes to
// out.Outer and level n to out.Levels[n-1], which grows as needed. `inner:`
// recurses with lvl+1. `keys:` goes to out.Keys and is only valid at the top
// level.
func parseScope(toks []ptok, lvl int, out *ParsedPipe) error {
	add := func(s Step) {
		if lvl == 0 {
			out.Outer = append(out.Outer, s)
			return
		}
		for len(out.Levels) < lvl {
			out.Levels = append(out.Levels, nil)
		}
		out.Levels[lvl-1] = append(out.Levels[lvl-1], s)
	}
	i := 0
	for i < len(toks) {
		t := toks[i]
		switch t.kind {
		case ptTilde:
			i++ // a separator with no meaning here
		case ptSlash:
			return errors.New("`/` is only valid in the decode stage")
		case ptSemi:
			return errors.New("`;` is no longer supported — group inner steps with `inner:(…)`")
		case ptLParen, ptRParen:
			return fmt.Errorf("unexpected `%s` — parentheses must follow `inner:`/`keys:`", parenText(t.kind))
		case ptWord:
			if rest, ok := strings.CutPrefix(t.text, "inner:"); ok {
				next, err := parsePrefixEntry(toks, i, rest, lvl, false, out)
				if err != nil {
					return err
				}
				i = next
				continue
			}
			if rest, ok := strings.CutPrefix(t.text, "keys:"); ok {
				next, err := parsePrefixEntry(toks, i, rest, lvl, true, out)
				if err != nil {
					return err
				}
				i = next
				continue
			}
			s, err := parseStep(t.text)
			if err != nil {
				return err
			}
			add(s)
			i++
		}
	}
	return nil
}

// parsePrefixEntry handles an `inner:` or `keys:` entry at toks[idx]. rest is
// the text after the prefix in the same word. It returns the index just after
// the entry. A non-empty rest is the single step, as in `inner:trim`. With an
// empty rest it looks at the next token. `(` opens a group, and any other
// word is the single step.
func parsePrefixEntry(toks []ptok, idx int, rest string, lvl int, isKeys bool, out *ParsedPipe) (int, error) {
	label := "inner:"
	if isKeys {
		label = "keys:"
		if lvl != 0 {
			return 0, errors.New("`keys:` is only valid at the top level")
		}
	}
	addOne := func(s Step) {
		if isKeys {
			out.Keys = append(out.Keys, s)
			return
		}
		for len(out.Levels) < lvl+1 {
			out.Levels = append(out.Levels, nil)
		}
		out.Levels[lvl] = append(out.Levels[lvl], s)
	}
	if rest != "" {
		s, err := parseStep(rest)
		if err != nil {
			return 0, err
		}
		addOne(s)
		return idx + 1, nil
	}
	if idx+1 >= len(toks) {
		return 0, fmt.Errorf("`%s` with no following step or `(…)` group", label)
	}
	switch toks[idx+1].kind {
	case ptLParen:
		closeIdx, err := matchParen(toks, idx+1)
		if err != nil {
			return 0, err
		}
		group := toks[idx+2 : closeIdx]
		if len(group) == 0 {
			return 0, fmt.Errorf("empty `%s(…)` group", label)
		}
		if isKeys {
			// Keys are strings and cannot nest. Parse the group flat into
			// out.Keys.
			tmp := &ParsedPipe{}
			if err := parseScope(group, 0, tmp); err != nil {
				return 0, err
			}
			if len(tmp.Levels) > 0 || len(tmp.Keys) > 0 {
				return 0, errors.New("`inner:`/`keys:` is not valid inside `keys:(…)`")
			}
			out.Keys = append(out.Keys, tmp.Outer...)
		} else if err := parseScope(group, lvl+1, out); err != nil {
			return 0, err
		}
		return closeIdx + 1, nil
	case ptWord:
		s, err := parseStep(toks[idx+1].text)
		if err != nil {
			return 0, err
		}
		addOne(s)
		return idx + 2, nil
	default:
		return 0, fmt.Errorf("expected a step or `(` after `%s`", label)
	}
}

// matchParen returns the index of the `)` balancing the `(` at open.
func matchParen(toks []ptok, open int) (int, error) {
	depth := 0
	for i := open; i < len(toks); i++ {
		//exhaustive:ignore not every kind applies here
		switch toks[i].kind {
		case ptLParen:
			depth++
		case ptRParen:
			if depth--; depth == 0 {
				return i, nil
			}
		}
	}
	return 0, errors.New("unbalanced `(` — missing `)`")
}

func parenText(k ptokKind) string {
	if k == ptLParen {
		return "("
	}
	return ")"
}

// parseStep parses one value-stage word into a Step. A custom func such as
// `@Func` or `@pkg.Func`, with an optional `:message`, is stored as a
// validator placeholder. Whether it is a mod or a validator is decided later
// from its signature.
func parseStep(word string) (Step, error) {
	switch word {
	case "":
		return Step{}, errors.New("empty pipe step")
	case "required", "optional":
		// parsePipeTag takes out every top-level presence word first, so one
		// that gets here is under `inner:` or `keys:`.
		return Step{}, fmt.Errorf("`%s` marks the field's own presence and is not valid under `inner:`/`keys:` — an element or map key is never absent", word)
	}
	if strings.HasPrefix(word, "@") {
		ref, msg := splitFuncMsg(word[1:])
		if ref == "" {
			return Step{}, errors.New("empty `@` reference")
		}
		// Name keeps the leading `@`, which the resolver looks for.
		return Step{V: ValidationRule{Name: "@" + ref, Msg: msg}}, nil
	}
	name, value, _ := strings.Cut(word, "=")
	if !isMultiPartName(name) {
		// Multi-part values keep their quotes for SplitPipeParts. All other
		// values lose one layer of quotes here.
		value = stripQuotes(value)
	}
	if isModName(name) {
		return Step{IsMod: true, M: ModRule{Name: name, Value: value}}, nil
	}
	return Step{V: ValidationRule{Name: name, Value: value}}, nil
}

// isMultiPartName reports whether a rule's value is a list separated by `|`,
// which SplitPipeParts splits later. Keep it in sync with the callers of
// SplitPipeParts in applicability.go and cmd/ggen/generate.go.
func isMultiPartName(name string) bool {
	switch name {
	case "oneof", "replace", "clamp":
		return true
	}
	return false
}

// isModName reports whether a built-in step name is a mod, which changes the
// value, rather than a validator. Keep it in sync with the mod names in
// applicability.go.
func isModName(name string) bool {
	switch name {
	case "trim", "tolower", "toupper", "trimleft", "trimright", "replace", "clamp":
		return true
	}
	return false
}

// splitFuncMsg splits a custom func reference from its optional inline
// message. `Even:'must be even'` gives ("Even", "must be even"), and
// `pkg.Even` gives ("pkg.Even", ""). The message is everything after the
// first `:`, with its quotes removed.
func splitFuncMsg(s string) (ref, msg string) {
	ref, msg, found := strings.Cut(s, ":")
	if found {
		msg = stripQuotes(msg)
	}
	return ref, msg
}

// StepsFromLegacy builds an ordered step list from separate mod and rule
// lists, mods first, then validators. It is used for synthetic fields that
// set only those lists.
func StepsFromLegacy(mods []ModRule, vals []ValidationRule) []Step {
	if len(mods) == 0 && len(vals) == 0 {
		return nil
	}
	steps := make([]Step, 0, len(mods)+len(vals))
	for _, m := range mods {
		steps = append(steps, Step{IsMod: true, M: m})
	}
	for _, v := range vals {
		steps = append(steps, Step{V: v})
	}
	return steps
}

// SplitCustomSteps splits an ordered step list into built-in and `@Func`
// steps. Each list keeps the declared order. Pointer fields need this split:
// built-in steps run on the value the pointer points to, and `@Func` steps
// on the pointer itself.
func SplitCustomSteps(steps []Step) (builtin, custom []Step) {
	for _, st := range steps {
		if StepIsCustom(st) {
			custom = append(custom, st)
		} else {
			builtin = append(builtin, st)
		}
	}
	return builtin, custom
}

// StepIsCustom reports whether a step calls a user `@Func`.
func StepIsCustom(st Step) bool {
	if st.IsMod {
		return st.M.Custom
	}
	return st.V.Custom
}

// splitSteps splits an ordered step list into a validator list and a mod
// list, each in its original order. Code that does not care about order
// reads these. The ordered Pipe stays the source of truth for the order of
// outer and key steps.
func splitSteps(steps []Step) (vs []ValidationRule, ms []ModRule) {
	for _, s := range steps {
		if s.IsMod {
			ms = append(ms, s.M)
		} else {
			vs = append(vs, s.V)
		}
	}
	return vs, ms
}

// deriveBuckets fills fi's separate rule and mod lists from Pipe, KeyPipe
// and Levels. It runs after parsing, and again after custom funcs are
// classified and written into the steps.
func deriveBuckets(fi *FieldInfo) {
	fi.Validation, fi.Mods = splitSteps(fi.Pipe)
	fi.KeyValidation, fi.KeyMods = splitSteps(fi.KeyPipe)
	fi.ElemValidation, fi.ElemMods = nil, nil
	fi.InnerValidation, fi.InnerMods = nil, nil
	if len(fi.Levels) > 0 {
		fi.ElemValidation, fi.ElemMods = splitSteps(fi.Levels[0])
		for _, lv := range fi.Levels[1:] {
			vv, mm := splitSteps(lv)
			fi.InnerValidation = append(fi.InnerValidation, vv)
			fi.InnerMods = append(fi.InnerMods, mm)
		}
	}
}

// applyPipeTags reads the `pipe:` and `hint:` tags of a field and fills the
// pipe fields of fi. A missing tag gives an empty pipe. For the decode
// variants, nullzero sets fi.NullZero, native needs nothing, and converters
// are resolved later. Custom `@` steps stay unclassified until
// resolvePipeCustoms.
func applyPipeTags(fi *FieldInfo, tag reflect.StructTag, goName string) error {
	pp, err := parsePipeTag(tag.Get("pipe"))
	if err != nil {
		return fmt.Errorf("field %s: pipe: %w", goName, err)
	}
	ht, err := parseHintTag(tag.Get("hint"))
	if err != nil {
		return fmt.Errorf("field %s: hint: %w", goName, err)
	}
	fi.Presence = pp.Presence
	fi.Variants = pp.Variants
	fi.Pipe = pp.Outer
	fi.KeyPipe = pp.Keys
	fi.Levels = pp.Levels
	fi.HintLen = ht.Outer
	fi.HintLevels = ht.Levels
	for _, v := range pp.Variants {
		switch v.Kind {
		case VariantNative:
		case VariantNullZero:
			fi.NullZero = true
		case VariantConvert:
			// resolved and shape-checked later in resolvePipeCustoms
		}
	}
	deriveBuckets(fi)
	return nil
}

func glyph(k ptokKind) string {
	//exhaustive:ignore not every kind applies here
	switch k {
	case ptSlash:
		return "/"
	case ptTilde:
		return "~"
	case ptLParen:
		return "("
	case ptRParen:
		return ")"
	case ptSemi:
		return ";"
	}
	return "?"
}

// HintTag is the parsed `hint:"N inner:M ..."` tag, which sets prealloc
// capacities.
type HintTag struct {
	Outer  int   // -1 means unset
	Levels []int // capacity per nested level, -1 means unset
}

// parseHintTag parses the `hint:` tag. It holds only prealloc capacities, one
// integer per level. `hint:"32 inner:8"` gives Outer 32 and Levels[0] 8.
// Levels nest as in `hint:"8 inner:(4 inner:2)"`.
func parseHintTag(tag string) (HintTag, error) {
	out := HintTag{Outer: -1}
	if strings.TrimSpace(tag) == "" {
		return out, nil
	}
	toks, err := tokenizePipe(tag)
	if err != nil {
		return out, err
	}
	return out, parseHintScope(toks, 0, &out)
}

// parseHintScope parses the tokens of one hint level. Level 0 sets Outer and
// level n sets Levels[n-1]. `inner:` takes one capacity, as in `inner:8`, or
// a `(…)` group.
func parseHintScope(toks []ptok, lvl int, out *HintTag) error {
	setN := func(word string) error {
		n, err := strconv.Atoi(word)
		if err != nil {
			return fmt.Errorf("hint %q is not a valid integer", word)
		}
		if n < 0 {
			return fmt.Errorf("hint %d must be ≥ 0", n)
		}
		if n > maxPrealloc {
			return fmt.Errorf("hint %d exceeds the %d prealloc ceiling — it is pasted into make() and would panic at decode", n, maxPrealloc)
		}
		if lvl == 0 {
			out.Outer = n
			return nil
		}
		for len(out.Levels) < lvl {
			out.Levels = append(out.Levels, -1)
		}
		out.Levels[lvl-1] = n
		return nil
	}
	i := 0
	for i < len(toks) {
		t := toks[i]
		if t.kind != ptWord {
			return fmt.Errorf("`%s` is not valid in a hint tag", glyph(t.kind))
		}
		if rest, ok := strings.CutPrefix(t.text, "inner:"); ok {
			if rest != "" {
				for len(out.Levels) < lvl+1 {
					out.Levels = append(out.Levels, -1)
				}
				if err := parseHintScope([]ptok{{kind: ptWord, text: rest}}, lvl+1, out); err != nil {
					return err
				}
				i++
				continue
			}
			if i+1 >= len(toks) {
				return errors.New("`inner:` with no following capacity or `(…)` group")
			}
			if toks[i+1].kind == ptLParen {
				closeIdx, err := matchParen(toks, i+1)
				if err != nil {
					return err
				}
				if err := parseHintScope(toks[i+2:closeIdx], lvl+1, out); err != nil {
					return err
				}
				i = closeIdx + 1
				continue
			}
			if err := parseHintScope(toks[i+1:i+2], lvl+1, out); err != nil {
				return err
			}
			i += 2
			continue
		}
		if err := setN(t.text); err != nil {
			return err
		}
		i++
	}
	return nil
}
