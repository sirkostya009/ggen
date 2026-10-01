package ggen

var (
	needEscapeHTML   [256]bool
	needEscapeNoHTML [256]bool
)

func init() {
	for c := range 256 {
		base := c < 0x20 || c == '"' || c == '\\'
		needEscapeNoHTML[c] = base
		needEscapeHTML[c] = base || c == '<' || c == '>' || c == '&'
	}
}

// closeJSONString treats dst[from:] as the unescaped body of a JSON string
// that is being written, and appends the closing quote. If the body holds
// bytes that JSON requires to be escaped, it rewrites the body escaped first.
// Some writers copy text the caller controls straight between the quotes: URL
// query and opaque parts, netip zones, TextAppender output. Most bodies need
// no escaping, and then this costs one pass over a lookup table.
func closeJSONString(dst []byte, from int) []byte {
	for _, c := range dst[from:] {
		if needEscapeNoHTML[c] {
			return AppendStringNoHTML(dst[:from], string(dst[from:]))
		}
	}
	return append(dst, '"')
}

// CloseJSONString is the exported closeJSONString. Generated code uses it and
// CloseJSONStringHTML to close TextAppender output. The caller has written the
// opening '"' and set from = len(dst) before calling AppendText.
func CloseJSONString(dst []byte, from int) []byte {
	return closeJSONString(dst, from)
}

func CloseJSONStringHTML(dst []byte, from int) []byte {
	for _, c := range dst[from:] {
		if needEscapeHTML[c] {
			return AppendString(dst[:from], string(dst[from:]))
		}
	}
	return append(dst, '"')
}

// AppendString appends s escaped as a JSON string body, then the closing
// '"'. The caller writes the opening '"'. The output is HTML-safe: <, > and &
// are written as \uXXXX escapes. Use AppendStringNoHTML to keep them as is.
func AppendString(dst []byte, s string) []byte {
	start := 0
	for i := range len(s) {
		c := s[i]
		if !needEscapeHTML[c] {
			continue
		}
		if start < i {
			dst = append(dst, s[start:i]...)
		}
		switch c {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		default:
			const hex = "0123456789abcdef"
			dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
		}
		start = i + 1
	}
	if start < len(s) {
		dst = append(dst, s[start:]...)
	}
	return append(dst, '"')
}

// AppendStringNoHTML is AppendString without HTML escaping. It writes <, >
// and & as is and applies only the standard JSON escapes. The caller writes
// the opening '"', as with AppendString.
func AppendStringNoHTML(dst []byte, s string) []byte {
	start := 0
	for i := range len(s) {
		c := s[i]
		if !needEscapeNoHTML[c] {
			continue
		}
		if start < i {
			dst = append(dst, s[start:i]...)
		}
		switch c {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		default:
			const hex = "0123456789abcdef"
			dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
		}
		start = i + 1
	}
	if start < len(s) {
		dst = append(dst, s[start:]...)
	}
	return append(dst, '"')
}
