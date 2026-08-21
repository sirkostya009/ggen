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

// closeJSONString treats dst[from:] as the raw body of an in-progress JSON
// string and appends the closing quote, re-escaping the body first when it
// carries bytes a JSON string must escape. Raw-text emitters (URL
// query/opaque, netip zones, TextAppender output) drop caller-controlled
// bytes between quotes; the overwhelmingly common clean body costs one
// table walk.
func closeJSONString(dst []byte, from int) []byte {
	for _, c := range dst[from:] {
		if needEscapeNoHTML[c] {
			return AppendStringNoHTML(dst[:from], string(dst[from:]))
		}
	}
	return append(dst, '"')
}

// CloseJSONString / CloseJSONStringHTML are the exported pair generated code
// closes TextAppender output through (the CALLER wrote the opening `"` and
// recorded from = len(dst) before AppendText).
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

// AppendString appends the escaped body of s plus a closing `"`. The
// CALLER writes the opening `"`. HTML-safe: <, >, & → \uXXXX. Use
// AppendStringNoHTML for raw output.
func AppendString(dst []byte, s string) []byte {
	start := 0
	for i := 0; i < len(s); i++ {
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

// AppendStringNoHTML is AppendString without HTML escaping: <, >, &
// emitted literally, standard JSON escapes only. Same caller contract.
func AppendStringNoHTML(dst []byte, s string) []byte {
	start := 0
	for i := 0; i < len(s); i++ {
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
