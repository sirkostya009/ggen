package validation

import (
	"strings"
	"unicode"
)

// IsAlphanum reports whether every byte in s is an ASCII letter or digit.
func IsAlphanum(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
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
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// IsLower reports whether s contains no uppercase unicode characters.
// Caseless characters (digits, punctuation, spaces, uncased scripts) pass —
// the class is "no uppercase", not "every rune lowercase" (the paired
// LowerError renders "contains uppercase letters").
func IsLower(s string) bool {
	for _, r := range s {
		if unicode.IsUpper(r) {
			return false
		}
	}
	return true
}

// IsUpper reports whether s contains no lowercase unicode characters —
// mirror of [IsLower].
func IsUpper(s string) bool {
	for _, r := range s {
		if unicode.IsLower(r) {
			return false
		}
	}
	return true
}

// IsHex reports whether every byte is a valid hex digit.
func IsHex(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// IsURL reports whether s starts with "scheme://...". Does not allocate a *url.URL.
func IsURL(s string) bool {
	schema, rest, ok := strings.Cut(s, "://")
	return ok && len(schema) > 0 && len(rest) > 0
}
