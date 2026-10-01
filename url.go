// The URL encoding mode bits, urlTable, and the bodies of appendURLRaw,
// appendUserinfo and appendURLEscape are ported from Go's net/url (BSD-style
// license). They give an append-style url.URL.String that does not allocate.

package ggen

import "net/url"

// AppendURL appends the text form of u and the closing '"'. The caller writes
// the opening '"', as with AppendString. The text is byte for byte what
// url.URL.String returns, JSON-escaped when needed. String copies RawQuery and
// Opaque unchanged, and Host may contain '"'. So even a URL that ggen itself
// decoded can hold bytes that would break the JSON output without escaping.
func AppendURL(dst []byte, u url.URL) []byte {
	from := len(dst)
	return closeJSONString(appendURLRaw(dst, u), from)
}

// AppendURLHTML is AppendURL with HTML-safe escaping, for htmlescape mode.
func AppendURLHTML(dst []byte, u url.URL) []byte {
	from := len(dst)
	return CloseJSONStringHTML(appendURLRaw(dst, u), from)
}

// appendURLRaw appends the text form of u, byte for byte equal to
// url.URL.String, without allocating.
func appendURLRaw(dst []byte, u url.URL) []byte {
	entry := len(dst)
	if u.Scheme != "" {
		dst = append(dst, u.Scheme...)
		dst = append(dst, ':')
	}
	if u.Opaque != "" {
		dst = append(dst, u.Opaque...)
	} else {
		hasAuthority := u.Scheme != "" || u.Host != "" || u.User != nil
		omit := u.OmitHost && u.Host == "" && u.User == nil
		if hasAuthority && !omit {
			if u.Host != "" || u.Path != "" || u.User != nil {
				dst = append(dst, '/', '/')
			}
			if u.User != nil {
				dst = appendUserinfo(dst, u.User)
				dst = append(dst, '@')
			}
			if u.Host != "" {
				dst = appendURLEscape(dst, u.Host, urlEncodeHost)
			}
		}
		// Same rules as url.URL.EscapedPath. RawPath is used only when it is
		// validly encoded and percent-decodes back to Path, so an out-of-date
		// RawPath is ignored. Otherwise a "*" path is written as is, and any
		// other path is escaped. Two checks below need the first byte and the
		// first segment of the escaped path: adding a '/' before the path, and
		// the RFC 3986 section 4.2 "./" prefix. They read the unescaped bytes
		// instead of building the escaped path. That gives the same answer,
		// because escaping keeps a leading '/' and never adds a ':' or '/'.
		raw := u.RawPath != "" && validURLEncoded(u.RawPath, urlEncodePath) &&
			urlUnescapesTo(u.RawPath, u.Path)
		hasPath := raw || u.Path != ""
		var p0 byte // first byte of the escaped path
		var seg string
		switch {
		case raw:
			p0, seg = u.RawPath[0], pathFirstSegment(u.RawPath)
		case u.Path == "*":
			p0, seg = '*', "*"
		case hasPath:
			p0, seg = u.Path[0], pathFirstSegment(u.Path)
		}
		if hasPath && p0 != '/' && u.Host != "" {
			dst = append(dst, '/')
		}
		// The section 4.2 rule applies only when no part of this URL comes
		// before the path. Compare against entry, not 0, so bytes already in
		// dst before the URL do not hide it.
		if len(dst) == entry && containsColon(seg) {
			dst = append(dst, '.', '/')
		}
		switch {
		case raw:
			dst = append(dst, u.RawPath...)
		case u.Path == "*":
			dst = append(dst, '*')
		default:
			dst = appendURLEscape(dst, u.Path, urlEncodePath)
		}
	}
	if u.ForceQuery || u.RawQuery != "" {
		dst = append(dst, '?')
		dst = append(dst, u.RawQuery...)
	}
	if u.Fragment != "" {
		dst = append(dst, '#')
		// Same rules as url.URL.EscapedFragment. RawFragment is used only
		// when it is valid and percent-decodes back to Fragment, so an
		// out-of-date RawFragment is ignored.
		if u.RawFragment != "" && validURLEncodedFragment(u.RawFragment) &&
			urlUnescapesTo(u.RawFragment, u.Fragment) {
			dst = append(dst, u.RawFragment...)
		} else {
			dst = appendURLEscape(dst, u.Fragment, urlEncodeFragment)
		}
	}
	return dst
}

// appendUserinfo appends username or username:password, escaped for the
// userinfo part of a URL.
func appendUserinfo(dst []byte, u *url.Userinfo) []byte {
	if u == nil {
		return dst
	}
	dst = appendURLEscape(dst, u.Username(), urlEncodeUserPassword)
	if pw, ok := u.Password(); ok {
		dst = append(dst, ':')
		dst = appendURLEscape(dst, pw, urlEncodeUserPassword)
	}
	return dst
}

// appendURLEscape appends s to dst and percent-escapes each byte that mode
// does not allow. In urlEncodeQueryComponent mode a space becomes '+'.
func appendURLEscape(dst []byte, s string, mode urlEncoding) []byte {
	const upperhex = "0123456789ABCDEF"
	for i := range len(s) {
		c := s[i]
		switch {
		case urlTable[c]&mode != 0:
			dst = append(dst, c)
		case c == ' ' && mode == urlEncodeQueryComponent:
			dst = append(dst, '+')
		default:
			dst = append(dst, '%', upperhex[c>>4], upperhex[c&15])
		}
	}
	return dst
}

// validURLEncoded reports whether s is already validly encoded for mode.
// Every byte must be a sub-delimiter, '@', ':', '[', ']', '%', or a byte that
// mode allows unescaped.
func validURLEncoded(s string, mode urlEncoding) bool {
	for i := range len(s) {
		switch s[i] {
		case '!', '$', '&', '\'', '(', ')', '*', '+', ',', ';', '=', ':', '@':
			// sub-delimiters, ':' and '@' are always allowed
		case '[', ']':
			// not strict RFC 3986, but browsers and Go accept them
		case '%':
			// start of a %XX escape, assumed well-formed
		default:
			if urlTable[s[i]]&mode == 0 {
				return false
			}
		}
	}
	return true
}

// validURLEncodedFragment is validURLEncoded for the fragment mode.
func validURLEncodedFragment(s string) bool {
	return validURLEncoded(s, urlEncodeFragment)
}

// urlUnescapesTo reports whether escaped percent-decodes exactly to want. It
// is the unescape(RawPath) == Path check from url.URL.EscapedPath, without
// allocating. A malformed %XX returns false. In net/url the unescape fails
// there too, and Path is escaped instead.
func urlUnescapesTo(escaped, want string) bool {
	j := 0
	for i := 0; i < len(escaped); {
		c := escaped[i]
		if c == '%' {
			if i+2 >= len(escaped) {
				return false
			}
			hi, ok1 := unhexURL(escaped[i+1])
			lo, ok2 := unhexURL(escaped[i+2])
			if !ok1 || !ok2 {
				return false
			}
			c = hi<<4 | lo
			i += 3
		} else {
			i++
		}
		if j >= len(want) || want[j] != c {
			return false
		}
		j++
	}
	return j == len(want)
}

func unhexURL(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func pathFirstSegment(s string) string {
	for i := range len(s) {
		if s[i] == '/' {
			return s[:i]
		}
	}
	return s
}

func containsColon(s string) bool {
	for i := range len(s) {
		if s[i] == ':' {
			return true
		}
	}
	return false
}

// urlEncoding is a set of URL parts, one bit each. urlTable maps a byte to the
// parts where it may appear unescaped. Both are ported from net/url.
type urlEncoding uint8

const (
	urlEncodePath urlEncoding = 1 << iota
	urlEncodePathSegment
	urlEncodeHost
	urlEncodeZone
	urlEncodeUserPassword
	urlEncodeQueryComponent
	urlEncodeFragment
)

var urlTable = [256]urlEncoding{
	'!':  urlEncodeFragment | urlEncodeZone | urlEncodeHost,
	'"':  urlEncodeZone | urlEncodeHost,
	'$':  urlEncodeFragment | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'&':  urlEncodeFragment | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'\'': urlEncodeZone | urlEncodeHost,
	'(':  urlEncodeFragment | urlEncodeZone | urlEncodeHost,
	')':  urlEncodeFragment | urlEncodeZone | urlEncodeHost,
	'*':  urlEncodeFragment | urlEncodeZone | urlEncodeHost,
	'+':  urlEncodeFragment | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	',':  urlEncodeFragment | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePath,
	'-':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'.':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'/':  urlEncodeFragment | urlEncodePath,
	'0':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'1':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'2':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'3':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'4':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'5':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'6':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'7':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'8':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'9':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	':':  urlEncodeFragment | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	';':  urlEncodeFragment | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePath,
	'<':  urlEncodeZone | urlEncodeHost,
	'=':  urlEncodeFragment | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'>':  urlEncodeZone | urlEncodeHost,
	'?':  urlEncodeFragment,
	'@':  urlEncodeFragment | urlEncodePathSegment | urlEncodePath,
	'A':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'B':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'C':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'D':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'E':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'F':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'G':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'H':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'I':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'J':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'K':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'L':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'M':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'N':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'O':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'P':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'Q':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'R':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'S':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'T':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'U':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'V':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'W':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'X':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'Y':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'Z':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'[':  urlEncodeZone | urlEncodeHost,
	']':  urlEncodeZone | urlEncodeHost,
	'_':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'a':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'b':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'c':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'd':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'e':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'f':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'g':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'h':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'i':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'j':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'k':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'l':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'm':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'n':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'o':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'p':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'q':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'r':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	's':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	't':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'u':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'v':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'w':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'x':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'y':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'z':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
	'~':  urlEncodeFragment | urlEncodeQueryComponent | urlEncodeUserPassword | urlEncodeZone | urlEncodeHost | urlEncodePathSegment | urlEncodePath,
}
