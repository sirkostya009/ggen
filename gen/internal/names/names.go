// Package names holds the identifier spellings the Swift and Kotlin emitters
// share. Both take a Go field or constant name and want the target's own
// convention for it.
package names

import (
	"strings"
	"unicode"
)

// LowerCamel turns a Go field name into a lowerCamel property: ID → id,
// URLPath → urlPath, CreatedAt → createdAt.
func LowerCamel(goName string) string {
	r := []rune(goName)
	i := 0
	for i < len(r) && unicode.IsUpper(r[i]) {
		i++
	}
	if i > 1 && i < len(r) {
		i-- // the last upper rune starts the next word
	}
	for j := range i {
		r[j] = unicode.ToLower(r[j])
	}
	return string(r)
}

// Words splits an enum constant's name (the type prefix already removed) or
// its value into words, so a target can spell them its own way.
func Words(s string) []string {
	var out []string
	var cur []rune
	rs := []rune(s)
	for i, c := range rs {
		switch {
		case unicode.IsLetter(c) || unicode.IsDigit(c):
			upperStart := unicode.IsUpper(c) && i > 0 &&
				(unicode.IsLower(rs[i-1]) || (i+1 < len(rs) && unicode.IsLower(rs[i+1]) && unicode.IsUpper(rs[i-1])))
			if upperStart && len(cur) > 0 {
				out = append(out, string(cur))
				cur = cur[:0]
			}
			cur = append(cur, c)
		default:
			if len(cur) > 0 {
				out = append(out, string(cur))
				cur = cur[:0]
			}
		}
	}
	if len(cur) > 0 {
		out = append(out, string(cur))
	}
	return out
}

// Ident reports whether s is a plain identifier: a letter or underscore, then
// letters, digits or underscores.
func Ident(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		if c == '_' || unicode.IsLetter(c) || (i > 0 && unicode.IsDigit(c)) {
			continue
		}
		return false
	}
	return true
}

// Title upper-cases the first rune.
func Title(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// Join joins words with sep after mapping each.
func Join(words []string, sep string, f func(string) string) string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = f(w)
	}
	return strings.Join(out, sep)
}
