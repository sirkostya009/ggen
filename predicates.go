package ggen

import (
	"strings"
	"unicode"
)

// IsAlphanum reports whether every byte in s is an ASCII letter or digit.
func IsAlphanum(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// IsNumeric reports whether every byte in s is an ASCII digit.
func IsNumeric(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := range len(s) {
		if c := s[i]; c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// IsLower reports whether s contains no uppercase Unicode letters. It backs
// the islower rule. The test is "no uppercase", not "every character is
// lowercase". So digits, punctuation, spaces and scripts without case pass.
// The paired LowerError says "contains uppercase letters".
func IsLower(s string) bool {
	for _, r := range s {
		if unicode.IsUpper(r) {
			return false
		}
	}
	return true
}

// IsUpper reports whether s contains no lowercase Unicode letters. It backs
// the isupper rule and mirrors [IsLower].
func IsUpper(s string) bool {
	for _, r := range s {
		if unicode.IsLower(r) {
			return false
		}
	}
	return true
}

// IsHex reports whether every byte in s is an ASCII hex digit.
func IsHex(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// IsURL reports whether s has the form "scheme://rest" with a non-empty
// scheme and rest. It does not parse s into a *url.URL.
func IsURL(s string) bool {
	schema, rest, ok := strings.Cut(s, "://")
	return ok && len(schema) > 0 && len(rest) > 0
}
