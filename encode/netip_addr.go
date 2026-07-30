package encode

import "net/netip"

// AppendNetipAddr appends a's text form plus the closing `"` — the CALLER
// writes the opening `"` (AppendString convention). A zone is arbitrary
// bytes (ParseAddr accepts `%q"z`), so zoned text is escape-checked and
// re-escaped when dirty; zone-free addrs can't need escaping and append raw.
// If another raw-text emitter (URL query/opaque, TextAppender output) ever
// needs this close-and-reescape-iff-dirty tail, lift it into a shared helper.
func AppendNetipAddr(dst []byte, a netip.Addr) []byte {
	from := len(dst)
	dst, _ = a.AppendText(dst) // infallible — netip.Addr.AppendText never errors
	if a.Zone() == "" {
		return append(dst, '"')
	}
	for _, c := range dst[from:] {
		if needEscapeNoHTML[c] {
			return AppendStringNoHTML(dst[:from], string(dst[from:]))
		}
	}
	return append(dst, '"')
}
