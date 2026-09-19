// Package js holds what the JavaScript-family emitters share: literals,
// identifiers, doc comments, and runtime helpers that reproduce Go semantics
// exactly where library built-ins differ.
package js

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
)

// Quote renders s as a JavaScript string literal.
func Quote(s string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

// Regex renders a regex literal for a source already in JavaScript syntax.
func Regex(source, flags string) string {
	return "/" + strings.ReplaceAll(source, "/", `\/`) + "/" + flags
}

var reserved = map[string]struct{}{
	"break": {}, "case": {}, "catch": {}, "class": {}, "const": {}, "continue": {}, "debugger": {},
	"default": {}, "delete": {}, "do": {}, "else": {}, "enum": {}, "export": {}, "extends": {},
	"false": {}, "finally": {}, "for": {}, "function": {}, "if": {}, "import": {}, "in": {},
	"instanceof": {}, "new": {}, "null": {}, "return": {}, "super": {}, "switch": {}, "this": {},
	"throw": {}, "true": {}, "try": {}, "typeof": {}, "var": {}, "void": {}, "while": {}, "with": {},
	"let": {}, "static": {}, "yield": {}, "await": {}, "implements": {}, "interface": {},
	"package": {}, "private": {}, "protected": {}, "public": {},
	"any": {}, "boolean": {}, "never": {}, "number": {}, "object": {}, "string": {}, "symbol": {},
	"undefined": {}, "unknown": {}, "bigint": {},
	// not keywords, but a module cannot bind them
	"arguments": {}, "eval": {},
	// contextual in TypeScript, and confusing as a type name
	"type": {}, "namespace": {}, "declare": {}, "module": {},
}

// IsIdent reports whether s can name a declaration.
func IsIdent(s string) bool {
	_, r := reserved[s]
	return !r && identChars(s)
}

func identChars(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' || r == '$' || unicode.IsLetter(r) || (i > 0 && unicode.IsDigit(r)) {
			continue
		}
		return false
	}
	return true
}

// Ident turns s into an identifier by replacing invalid characters.
func Ident(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c != '_' && c != '$' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			b[i] = '_'
		}
	}
	if len(b) == 0 || ('0' <= b[0] && b[0] <= '9') {
		return "_" + string(b)
	}
	return string(b)
}

// PropKey renders an object key, quoted unless it is an identifier.
// "__proto__" becomes a computed key: as a plain or quoted key it would set
// the prototype instead of defining a property.
func PropKey(name string) string {
	if name == "__proto__" {
		return "[" + Quote(name) + "]"
	}
	if identChars(name) {
		return name
	}
	return Quote(name)
}

// prototypeMembers are the names Object.prototype defines. A schema field
// with such a name cannot be optional: both zod and valibot test presence
// with `in`, which finds the prototype's.
var prototypeMembers = map[string]struct{}{
	"constructor": {}, "hasOwnProperty": {}, "isPrototypeOf": {},
	"propertyIsEnumerable": {}, "toLocaleString": {}, "toString": {}, "valueOf": {},
	"__defineGetter__": {}, "__defineSetter__": {}, "__lookupGetter__": {}, "__lookupSetter__": {},
}

// PrototypeMember reports whether a JSON name collides with Object.prototype.
func PrototypeMember(name string) bool {
	_, ok := prototypeMembers[name]
	return ok
}

// FileStem is the last path element without extensions: "shared/money.ts" → "money".
func FileStem(path string) string {
	stem := path[strings.LastIndexByte(path, '/')+1:]
	if dot := strings.IndexByte(stem, '.'); dot > 0 {
		stem = stem[:dot]
	}
	return stem
}

// Doc renders a /** */ comment at indent, or "" for no text.
func Doc(text, indent string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	text = strings.ReplaceAll(text, "*/", `*\/`)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 1 {
		return indent + "/** " + lines[0] + " */\n"
	}
	var b strings.Builder
	b.WriteString(indent)
	b.WriteString("/**\n")
	for _, l := range lines {
		b.WriteString(strings.TrimRight(indent+" * "+l, " "))
		b.WriteByte('\n')
	}
	b.WriteString(indent)
	b.WriteString(" */\n")
	return b.String()
}

// Paren wraps a TypeScript type in parentheses when it has a top-level | or &.
func Paren(s string) string {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(', '[', '{', '<':
			depth++
		case ')', ']', '}', '>':
			depth--
		case '|', '&':
			if depth == 0 {
				return "(" + s + ")"
			}
		case '"':
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' {
					i++
				}
			}
		}
	}
	return s
}

// IntRange is the inclusive range of a Go integer kind, clipped to the
// integers JavaScript holds exactly; ok is false when that is every safe
// integer.
func IntRange(bits int, unsigned bool) (lo, hi string, ok bool) {
	switch {
	case bits >= 53:
		if unsigned {
			return "0", "9007199254740991", true
		}
		return "", "", false
	case unsigned:
		return "0", strconv.FormatUint(1<<bits-1, 10), true
	}
	return strconv.FormatInt(-(1 << (bits - 1)), 10), strconv.FormatInt(1<<(bits-1)-1, 10), true
}

// Float32Max is the largest finite float32.
const Float32Max = "3.4028234663852886e38"

// Helpers collects the runtime helpers a file uses.
type Helpers struct{ used, direct []string }

// Use marks a helper, and the helpers it needs, and returns its name.
func (h *Helpers) Use(name string) string {
	if !slices.Contains(h.direct, name) {
		h.direct = append(h.direct, name)
	}
	h.mark(name)
	return name
}

func (h *Helpers) mark(name string) {
	if _, ok := helperSource[name]; !ok {
		panic("js: unknown helper " + name)
	}
	for _, dep := range helperDeps[name] {
		h.mark(dep)
	}
	if !slices.Contains(h.used, name) {
		h.used = append(h.used, name)
	}
}

// Direct returns the helpers a file refers to itself, sorted: what it imports
// from a runtime file.
func (h *Helpers) Direct() []string {
	names := slices.Clone(h.direct)
	slices.Sort(names)
	return names
}

// Source is the TypeScript source of every used helper, sorted by name, each
// followed by a blank line; "" when none is used.
func (h *Helpers) Source() string {
	return source(h.used, "")
}

// Runtime is the source of every helper, exported, for a shared runtime file.
func Runtime() string {
	names := make([]string, 0, len(helperSource))
	for n := range helperSource {
		names = append(names, n)
	}
	return source(names, "export ")
}

func source(names []string, prefix string) string {
	names = slices.Clone(names)
	slices.Sort(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(prefix)
		b.WriteString(helperSource[n])
		b.WriteString("\n\n")
	}
	return b.String()
}

var helperDeps = map[string][]string{
	"ggenIsIPv6":          {"ggenIsIPv4"},
	"ggenIsIP":            {"ggenIsIPv4", "ggenIsIPv6"},
	"ggenIsAddr":          {"ggenIsIP", "ggenIsIPv4", "ggenIsIPv6"},
	"ggenIsPrefix":        {"ggenIsIPv4", "ggenIsIPv6"},
	"ggenIsTime":          {"ggenDaysIn", "ggenTimeEncoder"},
	"ggenIsDateOnly":      {"ggenDaysIn"},
	"ggenIsDateTimeLocal": {"ggenDaysIn"},
	"ggenTrim":            {"ggenSpace"},
	"ggenToLower":         {"ggenMapCase"},
	"ggenToUpper":         {"ggenMapCase"},
	"ggenParsesURL":       {"ggenIsAddr"},
	"ggenIsDateTime":      {"ggenDaysIn"},
}

var helperSource = map[string]string{
	"ggenBytes": `function ggenBytes(s: string): number {
  let n = s.length;
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c < 0x80) continue;
    if (c >= 0xd800 && c < 0xdc00 && i + 1 < s.length) {
      n += 2; // a surrogate pair is four bytes
      i++;
      continue;
    }
    n += c < 0x800 ? 1 : 2;
  }
  return n;
}`,
	"ggenRunes": `function ggenRunes(s: string): number {
  let n = 0;
  for (const _ of s) n++;
  return n;
}`,
	"ggenEntries": `function ggenEntries(o: object): number {
  return Object.keys(o).length;
}`,
	"ggenDecoded": `function ggenDecoded(s: string, bitsPerChar: number): number {
  return Math.floor((s.replace(/=+$/, "").length * bitsPerChar) / 8);
}`,
	"ggenIsURL": `function ggenIsURL(s: string): boolean {
  const i = s.indexOf("://");
  return i > 0 && i + 3 < s.length;
}`,
	"ggenIsIPv4": `function ggenIsIPv4(s: string): boolean {
  const parts = s.split(".");
  return parts.length === 4 && parts.every((p) => /^(0|[1-9][0-9]{0,2})$/.test(p) && Number(p) <= 255);
}`,
	"ggenIsIPv6": `function ggenIsIPv6(s: string): boolean {
  const halves = s.split("::");
  if (halves.length > 2) return false;
  const groups = halves.flatMap((h) => (h === "" ? [] : h.split(":")));
  let count = 0;
  for (let i = 0; i < groups.length; i++) {
    if (i === groups.length - 1 && groups[i].includes(".") && !s.endsWith("::")) {
      if (!ggenIsIPv4(groups[i])) return false;
      count += 2;
    } else if (/^[0-9a-fA-F]{1,4}$/.test(groups[i])) {
      count++;
    } else {
      return false;
    }
  }
  return halves.length === 2 ? count < 8 : count === 8;
}`,
	"ggenIsIP": `function ggenIsIP(s: string): boolean {
  return ggenIsIPv4(s) || ggenIsIPv6(s);
}`,
	"ggenIsAddr": `function ggenIsAddr(s: string): boolean {
  const zone = s.indexOf("%");
  if (zone < 0) return ggenIsIP(s);
  return zone + 1 < s.length && ggenIsIPv6(s.slice(0, zone));
}`,
	"ggenIsPrefix": `function ggenIsPrefix(s: string): boolean {
  const slash = s.lastIndexOf("/");
  if (slash < 0 || !/^(0|[1-9][0-9]{0,2})$/.test(s.slice(slash + 1))) return false;
  const bits = Number(s.slice(slash + 1));
  const addr = s.slice(0, slash);
  if (ggenIsIPv4(addr)) return bits <= 32;
  return ggenIsIPv6(addr) && bits <= 128;
}`,
	"ggenIsTime":          isTimeHelper,
	"ggenTimeEncoder":     timeEncoder,
	"ggenIsDateOnly":      isDateOnlyHelper,
	"ggenIsTimeOnly":      isTimeOnlyHelper,
	"ggenIsDateTimeLocal": isDateTimeHelperLocal,
	"ggenDaysIn":          daysInHelper,
	"ggenIsDateTime":      isDateTimeHelper,
	"ggenIsBigFloat":      isBigFloatHelper,
	"ggenIsRational":      isRationalHelper,
	"ggenParsesURL":       parsesURLHelper,
	"ggenIsDuration": `function ggenIsDuration(s: string): boolean {
  if (!/^[-+]?(?:0|(?:(?:\d+(?:\.\d*)?|\.\d+)(?:ns|us|µs|μs|ms|h|m|s))+)$/.test(s)) return false;
  const unit: Record<string, bigint> = { ns: 1n, us: 1000n, "µs": 1000n, "μs": 1000n, ms: 1000000n, s: 1000000000n, m: 60000000000n, h: 3600000000000n };
  // Exact, because a Duration is int64 nanoseconds: at that size a double
  // cannot tell the largest one from the first that overflows.
  // A negative duration reaches one further: int64's smallest value.
  const limit = s.startsWith("-") ? 9223372036854775808n : 9223372036854775807n;
  let total = 0n;
  for (const m of s.matchAll(/(\d+(?:\.\d*)?|\.\d+)(ns|us|µs|μs|ms|h|m|s)/g)) {
    const [whole, frac = ""] = m[1].split(".");
    const u = unit[m[2]];
    total += (whole === "" ? 0n : BigInt(whole)) * u;
    if (frac !== "") total += (BigInt(frac) * u) / 10n ** BigInt(frac.length); // Go truncates
    if (total > limit) return false;
  }
  return true;
}`,
	"ggenIsNumber": `function ggenIsNumber(s: string): boolean {
  return /^-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?$/.test(s) && Number.isFinite(Number(s));
}`,
	"ggenFits": `function ggenFits(s: string, min: bigint, max: bigint): boolean {
  // A failed check does not stop the ones after it, so this runs on text the
  // format check already rejected.
  try {
    const n = BigInt(s);
    return n >= min && n <= max;
  } catch {
    return false;
  }
}`,
	"ggenTrim": `function ggenTrim(s: string): string {
  // Go trims unicode.IsSpace, which has U+0085 and not U+FEFF.
  let i = 0;
  let j = s.length;
  while (i < j && ggenSpace.test(s[i])) i++;
  while (j > i && ggenSpace.test(s[j - 1])) j--;
  return s.slice(i, j);
}`,
	"ggenSpace": `const ggenSpace = /[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]/;`,
	"ggenToLower": `function ggenToLower(s: string): string {
  return ggenMapCase(s, false);
}`,
	"ggenToUpper": `function ggenToUpper(s: string): string {
  return ggenMapCase(s, true);
}`,
	"ggenMapCase": `function ggenMapCase(s: string, upper: boolean): string {
  // Go maps every rune on its own and never expands one into several, so a
  // final sigma stays sigma and ß stays ß. U+0130 is the one simple mapping
  // JavaScript spells with two code points.
  let out = "";
  for (const r of s) {
    if (!upper && r === "\u0130") {
      out += "i";
      continue;
    }
    const m = upper ? r.toUpperCase() : r.toLowerCase();
    out += [...m].length === 1 ? m : r;
  }
  return out;
}`,
	"ggenInteger":   `const ggenInteger = /^-?(?:0|[1-9]\d*)$/;`,
	"ggenUnsigned":  `const ggenUnsigned = /^(?:0|[1-9]\d*)$/;`,
	"ggenBase64":    `const ggenBase64 = /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/;`,
	"ggenBase64URL": `const ggenBase64URL = /^(?:[A-Za-z0-9_-]{4})*(?:[A-Za-z0-9_-]{2}==|[A-Za-z0-9_-]{3}=)?$/;`,
	"ggenBase32":    `const ggenBase32 = /^(?:[A-Z2-7]{8})*(?:[A-Z2-7]{2}={6}|[A-Z2-7]{4}={4}|[A-Z2-7]{5}={3}|[A-Z2-7]{7}=)?$/;`,
	"ggenBase32Hex": `const ggenBase32Hex = /^(?:[0-9A-V]{8})*(?:[0-9A-V]{2}={6}|[0-9A-V]{4}={4}|[0-9A-V]{5}={3}|[0-9A-V]{7}=)?$/;`,
	"ggenHex":       `const ggenHex = /^(?:[0-9a-fA-F]{2})*$/;`,
}

// FormatBits is the bits one character of a binary encoding carries, or 0.
// Unlike FormatCheck it marks no helper as used.
func FormatBits(format string) int {
	switch format {
	case "base64", "base64url":
		return 6
	case "base32", "base32hex":
		return 5
	case "hex":
		return 4
	}
	return 0
}

// Check is how a string format is validated: a regex to test, or a function
// to call when the format needs more than a pattern.
type Check struct {
	Name string
	Func bool
	Bits int // bits per character of a binary encoding, 0 otherwise
}

// FormatCheck returns the check for a string format, marking its helper used;
// ok is false for formats with no helper of their own.
func (h *Helpers) FormatCheck(format string) (Check, bool) {
	c := Check{Bits: FormatBits(format)}
	switch format {
	case "duration":
		c.Name, c.Func = "ggenIsDuration", true
	case "number-string":
		c.Name, c.Func = "ggenIsNumber", true
	case "int-string":
		c.Name = "ggenInteger"
	case "uint-string":
		c.Name = "ggenUnsigned"
	case "base64":
		c.Name = "ggenBase64"
	case "base64url":
		c.Name = "ggenBase64URL"
	case "base32":
		c.Name = "ggenBase32"
	case "base32hex":
		c.Name = "ggenBase32Hex"
	case "hex":
		c.Name = "ggenHex"
	default:
		return Check{}, false
	}
	h.Use(c.Name)
	return c, true
}

// TimeCheck returns the check for a Go time layout: the common layouts get a
// regex-backed helper, every other one walks the layout at run time.
func (h *Helpers) TimeCheck(layout string) (name string, oneArg bool) {
	switch layout {
	case "2006-01-02":
		return h.Use("ggenIsDateOnly"), true
	case "15:04:05":
		return h.Use("ggenIsTimeOnly"), true
	case "2006-01-02 15:04:05":
		return h.Use("ggenIsDateTimeLocal"), true
	}
	return h.Use("ggenIsTime"), false
}

// Count renders "3 bytes" / "1 byte" for a message, with the plural the unit
// takes ("entry" -> "entries").
func Count(n, unit string) string {
	if n == "1" {
		return n + " " + unit
	}
	if strings.HasSuffix(unit, "y") && unit != "key" {
		return n + " " + strings.TrimSuffix(unit, "y") + "ies"
	}
	return n + " " + unit + "s"
}

// UTF16Len is the length JavaScript sees, which String.slice counts in.
func UTF16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// ClassRegex is the anchored regex of a character-class rule.
func ClassRegex(rule string) string {
	switch rule {
	case "alphanum":
		return `/^[A-Za-z0-9]+$/`
	case "numeric":
		return `/^[0-9]+$/`
	case "hexadecimal":
		return `/^[0-9A-Fa-f]+$/`
	case "islower":
		return `/^\P{Lu}*$/u`
	case "isupper":
		return `/^\P{Ll}*$/u`
	}
	return ""
}
