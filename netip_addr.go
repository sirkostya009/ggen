package ggen

import "net/netip"

// AppendNetipAddr appends a's text form plus the closing `"` — the CALLER
// writes the opening `"` (AppendString convention). A zone is arbitrary
// bytes (ParseAddr accepts `%q"z`), so zoned text closes via the escape
// check; zone-free addrs can't need escaping and append raw.
func AppendNetipAddr(dst []byte, a netip.Addr) []byte {
	from := len(dst)
	dst, _ = a.AppendText(dst) // infallible — netip.Addr.AppendText never errors
	if a.Zone() == "" {
		return append(dst, '"')
	}
	return closeJSONString(dst, from)
}

// AppendNetipAddrHTML is AppendNetipAddr closing zoned text through the
// HTML-safe escape set; zone-free text has no <>& to escape either.
func AppendNetipAddrHTML(dst []byte, a netip.Addr) []byte {
	from := len(dst)
	dst, _ = a.AppendText(dst)
	if a.Zone() == "" {
		return append(dst, '"')
	}
	return CloseJSONStringHTML(dst, from)
}
