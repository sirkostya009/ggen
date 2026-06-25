package decode

// IsEmail reports whether s matches a loose email format: non-space chars,
// exactly one '@' in the middle, and at least one '.' in the domain.
func IsEmail(s string) bool {
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

// IsASCII reports whether every byte in s is in the range 0..127.
func IsASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

// IsPrintable reports whether every byte in s is printable ASCII (>= 0x20, not DEL).
func IsPrintable(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

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

// IsLower reports whether s contains no uppercase ASCII letters.
func IsLower(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			return false
		}
	}
	return true
}

// IsUpper reports whether s contains no lowercase ASCII letters.
func IsUpper(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'a' && c <= 'z' {
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
	colon := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ':' {
			colon = i
			break
		}
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.' {
			continue
		}
		return false
	}
	if colon <= 0 || colon+3 > len(s) {
		return false
	}
	return s[colon+1] == '/' && s[colon+2] == '/'
}
