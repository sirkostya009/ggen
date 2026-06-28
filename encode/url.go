// The url-encoding mode bits, urlTable, and the bodies of AppendURL /
// appendUserinfo / appendURLEscape are ported from Go's net/url (BSD-style
// license) to provide an append-style, allocation-free URL.String.

package encode

import "net/url"

// AppendURL appends u's wire form to dst, byte-for-byte equal to
// url.URL.String, with zero allocation. The output never contains a raw `"`
// or `\`, so it's safe to drop between JSON quotes.
func AppendURL(dst []byte, u url.URL) []byte {
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
		// Prefer RawPath only when it's a valid encoding of Path.
		switch {
		case u.RawPath != "" && validURLEncoded(u.RawPath, urlEncodePath):
			dst = append(dst, u.RawPath...)
		case u.Path == "*":
			dst = append(dst, '*')
		default:
			before := len(dst)
			if u.Path != "" && u.Path[0] != '/' && u.Host != "" {
				dst = append(dst, '/')
			}
			// RFC 3986 §4.2: a relative-path first segment containing
			// ':' must be prefixed with ./ to disambiguate from scheme.
			if before == 0 && len(dst) == before {
				if seg := pathFirstSegment(u.Path); containsColon(seg) {
					dst = append(dst, '.', '/')
				}
			}
			dst = appendURLEscape(dst, u.Path, urlEncodePath)
		}
	}
	if u.ForceQuery || u.RawQuery != "" {
		dst = append(dst, '?')
		dst = append(dst, u.RawQuery...)
	}
	if u.Fragment != "" {
		dst = append(dst, '#')
		if u.RawFragment != "" && validURLEncodedFragment(u.RawFragment) {
			dst = append(dst, u.RawFragment...)
		} else {
			dst = appendURLEscape(dst, u.Fragment, urlEncodeFragment)
		}
	}
	return dst
}

// appendUserinfo emits username[:password] with per-mode escaping.
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

// appendURLEscape writes s into dst, percent-escaping each byte that needs
// escaping under mode. Space in encodeQueryComponent becomes '+'.
func appendURLEscape(dst []byte, s string, mode urlEncoding) []byte {
	const upperhex = "0123456789ABCDEF"
	for i := 0; i < len(s); i++ {
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

// validURLEncoded reports whether s is already a valid encoded form for the
// given mode (every char is an unreserved/sub-delim, an `@`/`:`, or part of
// a %XX escape).
func validURLEncoded(s string, mode urlEncoding) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '!', '$', '&', '\'', '(', ')', '*', '+', ',', ';', '=', ':', '@':
			// sub-delims + ":" + "@" — always allowed
		case '[', ']':
			// not strictly RFC 3986 but accepted by browsers/Go
		case '%':
			// percent-encoded byte — assumed well-formed
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

func pathFirstSegment(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return s[:i]
		}
	}
	return s
}

func containsColon(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return true
		}
	}
	return false
}

// URL-encoding mode bits + lookup table, ported from net/url.
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
