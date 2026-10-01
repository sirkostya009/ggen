package ggen

import "net/netip"

// AppendNetipAddr appends the text form of a and the closing '"'. The caller
// writes the opening '"', as with AppendString. A zone can hold any bytes
// (ParseAddr accepts `%q"z`), so text with a zone goes through the JSON
// escape check. An address without a zone never needs escaping and is
// appended as is.
func AppendNetipAddr(dst []byte, a netip.Addr) []byte {
	from := len(dst)
	dst, _ = a.AppendText(dst) // netip.Addr.AppendText never returns an error
	if a.Zone() == "" {
		return append(dst, '"')
	}
	return closeJSONString(dst, from)
}

// AppendNetipAddrHTML is AppendNetipAddr with HTML-safe escaping for text
// with a zone. Text without a zone has no <, > or & to escape.
func AppendNetipAddrHTML(dst []byte, a netip.Addr) []byte {
	from := len(dst)
	dst, _ = a.AppendText(dst)
	if a.Zone() == "" {
		return append(dst, '"')
	}
	return CloseJSONStringHTML(dst, from)
}
