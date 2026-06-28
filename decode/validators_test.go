package decode

import (
	"strings"
	"testing"
)

// BenchmarkIsEmail compares IsEmail against the two-pass refEmail over a long
// domain.
func BenchmarkIsEmail(b *testing.B) {
	long := "user@" + strings.Repeat("a", 65000) + ".com"
	b.Run("single_pass", func(b *testing.B) {
		for range b.N {
			if !IsEmail(long) {
				b.Fatal("want valid")
			}
		}
	})
	b.Run("two_pass", func(b *testing.B) {
		for range b.N {
			if !refEmail(long) {
				b.Fatal("want valid")
			}
		}
	})
}

// Reference implementations the predicates must match byte-for-byte.
func refPrintable(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}
func refAlphanum(s string) bool {
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
func refNumeric(s string) bool {
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
func refLower(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			return false
		}
	}
	return true
}
func refUpper(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'a' && c <= 'z' {
			return false
		}
	}
	return true
}
func refHex(s string) bool {
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
func refEmail(s string) bool {
	at := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			return false
		}
		if c == '@' {
			if at >= 0 {
				return false
			}
			at = i
		}
	}
	if at <= 0 || at >= len(s)-1 {
		return false
	}
	dot := -1
	for i := at + 1; i < len(s); i++ {
		if s[i] == '.' {
			dot = i
		}
	}
	return dot > at+1 && dot < len(s)-1
}

// TestCharsetPredicates_Parity guards the charset predicates against an
// independent reference for every byte value as a 1-char string, plus empty
// and a few representative multi-byte strings.
func TestCharsetPredicates_Parity(t *testing.T) {
	preds := []struct {
		name      string
		got, want func(string) bool
	}{
		{"IsPrintable", IsPrintable, refPrintable},
		{"IsAlphanum", IsAlphanum, refAlphanum},
		{"IsNumeric", IsNumeric, refNumeric},
		{"IsLower", IsLower, refLower},
		{"IsUpper", IsUpper, refUpper},
		{"IsHex", IsHex, refHex},
	}
	inputs := []string{"", "a", "Z", "0", "9", "f", "F", "g", "abc123", "ABCXYZ",
		"deadBEEF", "hello world", "tab\tdr", "\x00", "\x7f", "\x80", "ünïcödé",
		strings.Repeat("x", 100), "MixedCase", "0123456789", "g123", "/", ":", "@"}
	// every byte as a 1-char string
	for b := 0; b < 256; b++ {
		inputs = append(inputs, string([]byte{byte(b)}))
	}
	for _, p := range preds {
		for _, in := range inputs {
			if got, want := p.got(in), p.want(in); got != want {
				t.Errorf("%s(%q) = %v, want %v", p.name, in, got, want)
			}
		}
	}
}

// TestIsEmail_SinglePassParity pins single-pass IsEmail against the two-pass
// reference over a corpus exercising the @/dot edge cases.
func TestIsEmail_SinglePassParity(t *testing.T) {
	cases := []string{
		"", "a", "@", "a@", "@b", "a@b", "a@b.c", "a@b.", "a@.c", "a.b@c.d",
		"a@b.c.d", "user@example.com", "u@x.y", "a@@b.c", "a@b@c.d",
		"a b@c.d", "a@b c.d", "a@b.c ", " a@b.c", "first.last@sub.domain.tld",
		"x@y.", ".@.", "a@.", "a@b..c", "no-at-sign.com", "@dot.com",
		"a@b.cd", "trailing.@x", "x@..y", "x@y..", "x.@y.z",
	}
	for _, s := range cases {
		if got, want := IsEmail(s), refEmail(s); got != want {
			t.Errorf("IsEmail(%q) = %v, want %v", s, got, want)
		}
	}
}
