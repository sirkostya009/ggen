package main

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// This file owns the `pipe:` and `hint:` struct-tag grammar — the unified
// decode/transform/validate pipeline that replaced the old `ggen:`/`mod:`
// split. See .claude/tag-redesign.md for the full design. Grammar:
//
//	pipe        := stage ( "~" stage )*
//	first stage := variant ( "/" variant )*    // decode: JSON-shape dispatch
//	later stage := step ( WS step )*            // transform/validate the value
//	                with inner: / keys: / ; for container levels
//
// `required`/`optional` are lifted out (position-independent). A Step reuses
// the existing ValidationRule / ModRule types; mods and validators now share
// ONE ordered list per level.

// Presence is the field-presence axis (the JSON key), independent of the
// value (pipe steps) and the null shape (the nullzero variant).
type Presence uint8

const (
	PresenceDefault  Presence = iota // no marker: absent key → zero value, no error
	PresenceRequired                 // `required`: key must appear
	PresenceOptional                 // `optional`: explicit no-op marker (documents intent)
)

// Step is one ordered pipeline element — a validator or a mod, discriminated
// by IsMod (exactly one of V/M is meaningful).
type Step struct {
	IsMod bool
	V     ValidationRule // when !IsMod
	M     ModRule        // when IsMod
}

// VariantKind tags a decode-stage variant.
type VariantKind uint8

const (
	VariantNative   VariantKind = iota // "." — native decode of the field type T
	VariantNullZero                    // "nullzero" — JSON null → zero(T)
	VariantConvert                     // "@Conv" — scan input type W, call func(W)(T,...)
)

// Variant is one decode-stage alternative. ggen peeks the incoming JSON shape
// and routes to the single variant claiming it.
type Variant struct {
	Kind VariantKind

	// Convert-variant resolution (VariantConvert only), filled at parse time.
	Custom    bool
	PkgImport string
	PkgName   string
	FuncName  string
	Fallible  bool   // func(W)(T,error) / func(W)(T,bool) — vs infallible func(W)T
	BoolForm  bool   // func(W)(T,bool) — message-capable failure
	Msg       string // inline `:message` (bool-form only)
	InType    string // Go type literal of the converter input W (the scanned type)
	InKind    TypeKind
}

// ParsedPipe is the structured result of parsePipeTag, before it is stitched
// onto a FieldInfo.
type ParsedPipe struct {
	Presence Presence
	Variants []Variant // decode stage; nil/empty => implicit single native variant
	Outer    []Step    // value steps on the field / whole container (after decode)
	Keys     []Step    // map-key steps (`keys:`)
	Levels   [][]Step  // inner levels: Levels[0] per-element, Levels[1] one deeper, …
}

// pipe token kinds emitted by the tokenizer.
type ptokKind uint8

const (
	ptWord   ptokKind = iota // a step / variant text (may carry inner:/keys: prefix)
	ptSlash                  // "/" variant separator
	ptTilde                  // "~" stage separator
	ptLParen                 // "(" inner:/keys: group open
	ptRParen                 // ")" group close
	ptSemi                   // ";" — retired (errors with guidance)
)

type ptok struct {
	kind ptokKind
	text string // for ptWord
}

// tokenizePipe splits a `pipe:` tag into a flat token stream. Words are
// whitespace-separated; the structural glyphs `/ ~ ( )` terminate a word and
// emit their own token even without surrounding spaces (so `./@F` and `. / @F`
// lex identically). A single-quoted span inside a word is read literally —
// whitespace and glyphs within it are not terminators — with `\'` for a
// literal quote.
func tokenizePipe(tag string) ([]ptok, error) {
	var toks []ptok
	i := 0
	n := len(tag)
	for i < n {
		c := tag[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '/':
			toks = append(toks, ptok{kind: ptSlash})
			i++
		case c == '~':
			toks = append(toks, ptok{kind: ptTilde})
			i++
		case c == '(':
			toks = append(toks, ptok{kind: ptLParen})
			i++
		case c == ')':
			toks = append(toks, ptok{kind: ptRParen})
			i++
		case c == ';':
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
					// Quoted span: copy literally to the closing quote (\' is an
					// escaped quote). Quotes kept; stripQuotes removes them later.
					sb.WriteByte('\'')
					i++
					for i < n {
						if tag[i] == '\\' && i+1 < n && tag[i+1] == '\'' {
							sb.WriteByte('\'')
							i += 2
							continue
						}
						if tag[i] == '\'' {
							i++
							break
						}
						sb.WriteByte(tag[i])
						i++
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

// stripQuotes removes a single layer of surrounding single quotes from s and
// unescapes \' → '. Unquoted input is returned unchanged.
func stripQuotes(s string) string {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		s = s[1 : len(s)-1]
		s = strings.ReplaceAll(s, `\'`, `'`)
	}
	return s
}

// parsePipeTag parses a `pipe:"..."` tag value into a ParsedPipe. It does NOT
// resolve @-func signatures or check applicability — those run later with type
// info (resolveCustomRules / checkRuleApplicability).
func parsePipeTag(tag string) (ParsedPipe, error) {
	var out ParsedPipe
	if strings.TrimSpace(tag) == "" {
		return out, nil
	}
	toks, err := tokenizePipe(tag)
	if err != nil {
		return out, err
	}

	// Pass 1: lift presence markers (`required`/`optional`) from anywhere and
	// drop them from the stream.
	filtered := toks[:0:0]
	for _, t := range toks {
		if t.kind == ptWord {
			switch t.text {
			case "required":
				if out.Presence == PresenceOptional {
					return out, fmt.Errorf("`required` and `optional` are mutually exclusive")
				}
				out.Presence = PresenceRequired
				continue
			case "optional":
				if out.Presence == PresenceRequired {
					return out, fmt.Errorf("`required` and `optional` are mutually exclusive")
				}
				out.Presence = PresenceOptional
				continue
			}
		}
		filtered = append(filtered, t)
	}
	toks = filtered

	// Decide where the decode stage ends. An explicit `~` splits decode |
	// value. Without `~`, the decode stage is the leading run of variant
	// keywords (`.`, `nullzero`, or a `/`-joined converter list); a lone
	// leading `@Func` stays a value step (a converter needs `/`, `~`, or `.`
	// adjacency to read as a decode variant).
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

// leadingDecodeExtent returns how many leading tokens (including `/`
// separators) form an implicit decode stage when there is no explicit `~`. The
// stage starts only when the first token is `.`/`nullzero`, or a `@Func`
// immediately followed by `/`, then consumes the `/`-separated variant run.
// Returns 0 when the whole pipe is value steps.
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

// parseVariants turns the decode-stage tokens into a variant list. Variants are
// `/`-separated and each is a single word (`.`, `nullzero`, or `@Conv`).
func parseVariants(toks []ptok) ([]Variant, error) {
	var vars []Variant
	expectWord := true
	for _, t := range toks {
		switch t.kind {
		case ptSlash:
			if expectWord {
				return nil, fmt.Errorf("empty decode variant around `/`")
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
		return nil, fmt.Errorf("trailing `/` with no decode variant")
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
			return Variant{}, fmt.Errorf("empty `@` converter reference")
		}
		return Variant{Kind: VariantConvert, FuncName: ref, Msg: msg}, nil
	}
	return Variant{}, fmt.Errorf("decode variant %q must be `.`, `nullzero`, or `@Converter`", word)
}

// parseValueSteps parses the value-region tokens into out.Outer/Keys/Levels.
// `inner:` / `keys:` scope the next inner level / map keys: a bare prefix takes
// exactly ONE following step (`inner:trim`), parentheses group several
// (`inner:(trim maxlen=20)`), and inner groups nest (`inner:(a inner:(b))`).
func parseValueSteps(toks []ptok, out *ParsedPipe) error {
	return parseScope(toks, 0, out)
}

// parseScope parses one container level's tokens. lvl 0 → out.Outer; lvl n →
// out.Levels[n-1] (grown on demand). `inner:` recurses at lvl+1; `keys:` routes
// to out.Keys and is only valid at the top level.
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
			i++ // cosmetic separator
		case ptSlash:
			return fmt.Errorf("`/` is only valid in the decode stage")
		case ptSemi:
			return fmt.Errorf("`;` is no longer supported — group inner steps with `inner:(…)`")
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

// parsePrefixEntry handles an `inner:`/`keys:` entry at toks[idx]. rest is the
// text after the prefix on the same word. Returns the index just past the
// entry. A non-empty rest is the single step (`inner:trim`); an empty rest
// looks at the next token — `(` opens a group, otherwise the next word is the
// single step.
func parsePrefixEntry(toks []ptok, idx int, rest string, lvl int, isKeys bool, out *ParsedPipe) (int, error) {
	label := "inner:"
	if isKeys {
		label = "keys:"
		if lvl != 0 {
			return 0, fmt.Errorf("`keys:` is only valid at the top level")
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
		close, err := matchParen(toks, idx+1)
		if err != nil {
			return 0, err
		}
		group := toks[idx+2 : close]
		if len(group) == 0 {
			return 0, fmt.Errorf("empty `%s(…)` group", label)
		}
		if isKeys {
			// keys are strings — no further nesting; parse flat into out.Keys.
			tmp := &ParsedPipe{}
			if err := parseScope(group, 0, tmp); err != nil {
				return 0, err
			}
			if len(tmp.Levels) > 0 || len(tmp.Keys) > 0 {
				return 0, fmt.Errorf("`inner:`/`keys:` is not valid inside `keys:(…)`")
			}
			out.Keys = append(out.Keys, tmp.Outer...)
		} else if err := parseScope(group, lvl+1, out); err != nil {
			return 0, err
		}
		return close + 1, nil
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
		switch toks[i].kind {
		case ptLParen:
			depth++
		case ptRParen:
			if depth--; depth == 0 {
				return i, nil
			}
		}
	}
	return 0, fmt.Errorf("unbalanced `(` — missing `)`")
}

func parenText(k ptokKind) string {
	if k == ptLParen {
		return "("
	}
	return ")"
}

// parseStep parses one value-stage step word into a Step. Custom funcs
// (`@Func`, `@pkg.Func`, optional `:message`) are parked as validator-shaped
// placeholders here; mod-vs-validator is decided later from the signature.
func parseStep(word string) (Step, error) {
	if word == "" {
		return Step{}, fmt.Errorf("empty pipe step")
	}
	if strings.HasPrefix(word, "@") {
		ref, msg := splitFuncMsg(word[1:])
		if ref == "" {
			return Step{}, fmt.Errorf("empty `@` reference")
		}
		// Name keeps the leading `@` for resolver lookup.
		return Step{V: ValidationRule{Name: "@" + ref, Msg: msg}}, nil
	}
	name, value, _ := strings.Cut(word, "=")
	value = stripQuotes(value)
	if isModName(name) {
		return Step{IsMod: true, M: ModRule{Name: name, Value: value}}, nil
	}
	return Step{V: ValidationRule{Name: name, Value: value}}, nil
}

// isModName reports whether a builtin step name is a mod (transform) rather
// than a validator. Keep in sync with the mod vocabulary in applicability.go.
func isModName(name string) bool {
	switch name {
	case "trim", "lower", "upper", "trimleft", "trimright", "replace", "clamp":
		return true
	}
	return false
}

// splitFuncMsg splits a custom-func reference from its optional inline message.
// `Even:'must be even'` → ("Even", "must be even"); `pkg.Even` → ("pkg.Even", "").
// The message is everything after the first `:`; quotes are stripped.
func splitFuncMsg(s string) (ref, msg string) {
	ref, msg, found := strings.Cut(s, ":")
	if found {
		msg = stripQuotes(msg)
	}
	return ref, msg
}

// stepsFromLegacy translates the legacy split buckets into a unified ordered
// step list, mods first then validators (the historical execution order), so
// the ggen:/mod: → pipe shim emits byte-identical output.
func stepsFromLegacy(mods []ModRule, vals []ValidationRule) []Step {
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

// splitSteps partitions an ordered step list back into the legacy
// validator/mod buckets (order within each preserved). These feed the
// order-independent consumers; the ordered Pipe stays source of truth for
// outer/key emit order.
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

// deriveBuckets (re)populates the legacy split buckets on fi from its unified
// Pipe/KeyPipe/Levels. Called for pipe-tagged fields after parse and again
// after custom-func classification stamps the steps.
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

// applyPipeTags reads the `pipe:`/`hint:` tags off a field and populates the
// unified pipe model on fi. An absent tag yields an empty pipe. Decode-stage
// variants: nullzero → fi.NullZero, native is implicit, converters are
// resolved later. Custom @-steps stay unclassified for resolvePipeCustoms.
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
			// resolved + shape-checked later (resolvePipeCustoms)
		}
	}
	deriveBuckets(fi)
	return nil
}

func glyph(k ptokKind) string {
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

// HintTag is the parsed `hint:"N inner:M ..."` prealloc-capacity tag.
type HintTag struct {
	Outer  int   // -1 = unset
	Levels []int // per-inner-level capacity; entry -1 = unset
}

// parseHintTag parses the `hint:` tag — prealloc capacities only, one int per
// level. `hint:"32 inner:8"` → Outer 32, Levels[0] 8; nest with
// `hint:"8 inner:(4 inner:2)"`.
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

// parseHintScope parses one hint level's tokens. lvl 0 → Outer; lvl n →
// Levels[n-1]. `inner:` takes a single capacity (`inner:8`) or a `(…)` group.
func parseHintScope(toks []ptok, lvl int, out *HintTag) error {
	setN := func(word string) error {
		n, err := strconv.Atoi(word)
		if err != nil {
			return fmt.Errorf("hint %q is not a valid integer", word)
		}
		if n < 0 {
			return fmt.Errorf("hint %d must be ≥ 0", n)
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
				return fmt.Errorf("`inner:` with no following capacity or `(…)` group")
			}
			if toks[i+1].kind == ptLParen {
				close, err := matchParen(toks, i+1)
				if err != nil {
					return err
				}
				if err := parseHintScope(toks[i+2:close], lvl+1, out); err != nil {
					return err
				}
				i = close + 1
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
