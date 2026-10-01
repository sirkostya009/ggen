// Package ggen is the runtime that ggen-generated code calls. This file holds
// the JSON scanning functions for the bytes path, where the whole input is
// one []byte. They do not copy and do not allocate on the common path.
//
// Every scan function takes (data, pos) and returns the new pos. A string
// without escapes is aliased: the returned string points into data through
// unsafe.String instead of holding a copy. The caller must not modify data
// while such strings are in use. A string with escapes is decoded into a new
// allocation.
package ggen

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

var (
	ErrExpectString   = errors.New("scan: expected string")
	ErrBadString      = errors.New("scan: invalid string")
	ErrInvalidUTF8    = errors.New("scan: invalid UTF-8")
	ErrUnterminated   = errors.New("scan: unterminated string")
	ErrBadNumber      = errors.New("scan: invalid number")
	ErrNumberOverflow = errors.New("scan: integer overflow")
	ErrBadBool        = errors.New("scan: invalid bool")
	ErrBadLiteral     = errors.New("scan: invalid literal")
	ErrBadValue       = errors.New("scan: invalid value")
	ErrBadArray       = errors.New("scan: invalid array")
	ErrBadObject      = errors.New("scan: invalid object")
	ErrMaxDepth       = errors.New("scan: exceeded max depth")
	ErrUnexpectedEnd  = errors.New("scan: unexpected end of input")
	ErrTrailingData   = errors.New("scan: trailing data after value")
)

// maxDepth limits how deeply arrays and objects may nest on every recursive
// decode path: SkipValue, Any, and generated decoders for self-referential
// structs. Without it a few MB of "[[[[..." would overflow the goroutine
// stack, which is a fatal error that cannot be recovered. jsonv2 uses the
// same limit. The check costs one compare per opened array or object and
// nothing on scalar values.
const maxDepth = 10000

// SkipSpace advances past JSON whitespace (space, tab, CR, LF).
func SkipSpace(data []byte, i int) int {
	for i < len(data) && data[i] <= ' ' && (data[i] == ' ' || data[i] == '\t' || data[i] == '\n' || data[i] == '\r') {
		i++
	}
	return i
}

// Expect consumes the byte c at data[i] and returns the new position.
func Expect(data []byte, i int, c byte) (int, error) {
	if i >= len(data) || data[i] != c {
		return i, fmt.Errorf("scan: expected %q at pos %d", c, i)
	}
	return i + 1, nil
}

// ObjectOpen consumes leading whitespace and the opening '{'.
func ObjectOpen(data []byte, i int) (int, error) {
	i = SkipSpace(data, i)
	if i >= len(data) || data[i] != '{' {
		return i, ErrBadObject
	}
	return i + 1, nil
}

// ArrayOpen consumes leading whitespace and the opening '['.
func ArrayOpen(data []byte, i int) (int, error) {
	i = SkipSpace(data, i)
	if i >= len(data) || data[i] != '[' {
		return i, ErrBadArray
	}
	return i + 1, nil
}

// String decodes the JSON string at data[i], which must start with '"'. If
// the string has no escapes, the result aliases data and nothing is copied.
// Otherwise it is decoded into a new allocation.
//
// validate turns on UTF-8 checking, which returns ErrInvalidUTF8 like jsonv2.
// UTF-8 stores each character (a rune) as 1 to 4 bytes, and not every byte
// sequence is valid. validate is false under the allowinvalidutf8 option.
// Then invalid bytes pass through, and an unpaired surrogate escape becomes
// U+FFFD, like encoding/json. A surrogate is half of a \uXXXX pair that
// together encodes one character above U+FFFF. Grammar checks, such as
// control bytes and escape forms, run either way.
func String(data []byte, i int, validate bool) (string, int, error) {
	if i >= len(data) || data[i] != '"' {
		return "", i, ErrExpectString
	}
	start := i + 1
	rest := data[start:]
	closeIdx := bytes.IndexByte(rest, '"')
	if closeIdx < 0 {
		// No closing quote, but there is a backslash. Walk the escape
		// grammar, so a cut-off `\u` escape or a trailing `\` reports
		// ErrBadString.
		if bsIdx := bytes.IndexByte(rest, '\\'); bsIdx >= 0 {
			return stringUnterminated(data, start, start+bsIdx, validate)
		}
		// A control byte before the end is an error whatever follows. The
		// stream scanners also report it before reading more input.
		if hasCtrlByte(rest) {
			return "", start + ctrlIndex(rest), ErrBadString
		}
		return "", len(data), ErrUnterminated
	}
	if bsIdx := bytes.IndexByte(rest[:closeIdx], '\\'); bsIdx >= 0 {
		// closeIdx is the first '"', which may be an escaped `\"`. Sizing the
		// buffer from it would be too small when an escaped quote comes
		// early, and the buffer would have to grow. So size it from the real
		// closing quote. The decoded string is never longer than that span.
		return stringSlow(data, start, start+bsIdx, stringSpanEnd(data, start)-start, validate)
	}
	if validate {
		if err := checkSpan(rest[:closeIdx]); err != nil {
			if err == ErrBadString {
				return "", start + ctrlIndex(rest[:closeIdx]), err
			}
			// Invalid UTF-8 has no single bad byte, so report the start.
			return "", start, err
		}
	} else if hasCtrlByte(rest[:closeIdx]) {
		return "", start + ctrlIndex(rest[:closeIdx]), ErrBadString
	}
	return unsafe.String(unsafe.SliceData(rest), closeIdx), start + closeIdx + 1, nil
}

// Detach returns a copy of s if s aliases data, and s unchanged if it does
// not. String and the StringAVX* functions return an alias when the string
// has no escapes. With escapes they return a new allocation from stringSlow,
// which needs no second copy. So -copy mode can call the same aliasing
// functions and copy each string at most once. The pointer range test is
// safe because Go's garbage collector never moves objects, and a stringSlow
// buffer is a separate allocation that never overlaps data.
func Detach(s string, data []byte) string {
	if len(s) == 0 {
		// An empty alias still points into data and would keep it alive.
		// Return a plain empty string instead.
		return ""
	}
	sp := uintptr(unsafe.Pointer(unsafe.StringData(s)))
	dp := uintptr(unsafe.Pointer(unsafe.SliceData(data)))
	if sp >= dp && sp < dp+uintptr(len(data)) {
		return strings.Clone(s) // s aliases data, so copy it
	}
	return s // s is already its own allocation
}

// checkSpan checks a string body that has no escapes. It returns
// ErrBadString for a control byte (below 0x20) and ErrInvalidUTF8 for invalid
// UTF-8. The control byte scan uses SWAR, like hasCtrlByte: it loads 8 bytes
// as one uint64 and tests all 8 with a few arithmetic ops. It also collects
// the high bit of every byte. Bytes below 0x80 are plain ASCII, so a span
// with no high bit set skips the slower utf8.Valid walk. That is the common
// case.
func checkSpan(b []byte) error {
	const (
		ones uint64 = 0x0101010101010101
		high uint64 = 0x8080808080808080
		sub  uint64 = 0x20 * ones
	)
	var hi uint64
	i := 0
	for ; i+8 <= len(b); i += 8 {
		x := binary.LittleEndian.Uint64(b[i:])
		if (x-sub)&^x&high != 0 {
			return ErrBadString
		}
		hi |= x
	}
	var t byte
	for ; i < len(b); i++ {
		c := b[i]
		if c < 0x20 {
			return ErrBadString
		}
		t |= c
	}
	if hi&high != 0 || t&0x80 != 0 {
		if !utf8.Valid(b) {
			return ErrInvalidUTF8
		}
	}
	return nil
}

// ctrlOrHigh is hasCtrlByte that also reports whether any byte is 0x80 or
// above. The stream string scans check each chunk as it arrives. They cannot
// validate UTF-8 per chunk, because one character may be split between two
// reads. So they collect high across chunks and run utf8.Valid once over the
// whole string. high has no meaning when ctrl is true, because the caller
// returns an error.
func ctrlOrHigh(b []byte) (ctrl, high bool) {
	const (
		ones   uint64 = 0x0101010101010101
		high64 uint64 = 0x8080808080808080
		sub    uint64 = 0x20 * ones
	)
	var hi uint64
	i := 0
	for ; i+8 <= len(b); i += 8 {
		x := binary.LittleEndian.Uint64(b[i:])
		if (x-sub)&^x&high64 != 0 {
			return true, false
		}
		hi |= x
	}
	var t byte
	for ; i < len(b); i++ {
		c := b[i]
		if c < 0x20 {
			return true, false
		}
		t |= c
	}
	return false, hi&high64 != 0 || t&0x80 != 0
}

// CheckUTF8 returns ErrInvalidUTF8 when b contains invalid UTF-8. Generated
// code calls it on captured raw values (json.RawMessage, jsontext.Value),
// because jsonv2 rejects invalid UTF-8 inside raw values too. It looks for a
// set high bit 8 bytes at a time. ASCII-only spans never run utf8.Valid. The
// utf8.Valid walk starts at the first 8-byte word with a high bit. That is
// safe because the byte before it is ASCII, so a character starts there. An
// unpaired surrogate escape such as \uD800 is plain ASCII text here and
// passes. jsonv2 decodes escapes inside raw strings and rejects it, so this
// is a known difference (see .claude/backlog.md).
func CheckUTF8(b []byte) error {
	const high uint64 = 0x8080808080808080
	i := 0
	for ; i+8 <= len(b); i += 8 {
		if binary.LittleEndian.Uint64(b[i:])&high != 0 {
			if !utf8.Valid(b[i:]) {
				return ErrInvalidUTF8
			}
			return nil
		}
	}
	for ; i < len(b); i++ {
		if b[i] >= 0x80 {
			if !utf8.Valid(b[i:]) {
				return ErrInvalidUTF8
			}
			return nil
		}
	}
	return nil
}

// hasCtrlByte reports whether b contains a control character (below 0x20).
// JSON does not allow those unescaped in a string. It scans 8 bytes per
// iteration with SWAR.
func hasCtrlByte(b []byte) bool {
	const (
		ones uint64 = 0x0101010101010101
		high uint64 = 0x8080808080808080
		sub  uint64 = 0x20 * ones // 0x2020202020202020
	)
	i := 0
	for ; i+8 <= len(b); i += 8 {
		x := binary.LittleEndian.Uint64(b[i:])
		if (x-sub)&^x&high != 0 {
			return true
		}
	}
	for ; i < len(b); i++ {
		if b[i] < 0x20 {
			return true
		}
	}
	return false
}

// ctrlIndex returns the offset of the first control character in b. The
// caller has already learned from hasCtrlByte, ctrlOrHigh or checkSpan that
// one exists. It runs only on the error path. Every string scanner reports
// ErrBadString at the bad byte, and the SWAR checks return only a bool, so
// this finds the offset. The SIMD versions get the offset from their own
// scan.
func ctrlIndex(b []byte) int {
	for i, c := range b {
		if c < 0x20 {
			return i
		}
	}
	return len(b)
}

// stringSpanEnd returns the offset of the first unescaped '"' at or after
// start, or len(data) if there is none. A quote is escaped when an odd number
// of backslashes comes right before it. The result sizes stringSlow's buffer,
// because the decoded string is never longer than this raw span. It costs one
// extra bytes.IndexByte pass on the less common escape path. That is cheaper
// than growing the buffer several times.
func stringSpanEnd(data []byte, start int) int {
	i := start
	for i < len(data) {
		q := bytes.IndexByte(data[i:], '"')
		if q < 0 {
			return len(data)
		}
		i += q
		bs := 0
		for k := i - 1; k >= start && data[k] == '\\'; k-- {
			bs++
		}
		if bs&1 == 0 {
			return i
		}
		i++
	}
	return len(data)
}

// escRunWindow is how many plain bytes stringSlow's inner loop copies at most
// before it returns to the outer loop. The inner loop only copies bytes, and
// the outer loop handles escapes. This small inner loop measured faster on
// both escape-dense and long plain input. The exact value is not critical,
// and 16 measured best.
const escRunWindow = 16

// stringSlow decodes a string with at least one escape sequence.
// data[start:j] has no escapes, and data[j] is the first '\\'. capHint is the
// initial buffer size. Callers pass the raw length up to the unescaped closing
// quote, which bounds the decoded length. The returned string shares memory
// with the buffer through unsafe.String. The buffer is never written again.
func stringSlow(data []byte, start, j, capHint int, validate bool) (string, int, error) {
	bad, rawHigh := ctrlOrHigh(data[start:j])
	if bad {
		return "", start + ctrlIndex(data[start:j]), ErrBadString
	}
	buf := make([]byte, 0, capHint)
	buf = append(buf, data[start:j]...)
	var rawHi byte
	for j < len(data) {
		// Copy plain bytes in an inner loop of at most escRunWindow bytes.
		// Escapes are handled in the outer loop, which keeps the inner loop
		// small. A bulk copy with IndexByte and memmove measured slower (see
		// .claude/backlog.md).
		we := min(j+escRunWindow, len(data))
		for j < we {
			c := data[j]
			if c == '"' || c == '\\' {
				break
			}
			if c < 0x20 {
				return "", j, ErrBadString
			}
			rawHi |= c
			buf = append(buf, c)
			j++
		}
		if j >= len(data) {
			break
		}
		c := data[j]
		if c == '"' {
			// Decoded escapes are always valid UTF-8: surrogates error out
			// below, and AppendRune writes the rest. Only copied plain bytes
			// can make buf invalid, so skip the check when they were all
			// ASCII. Checking the whole buf gives the exact answer. Plain
			// segments always start and end on character boundaries,
			// because '\\' and '"' can never be the middle of a character.
			if validate && (rawHigh || rawHi&0x80 != 0) && !utf8.Valid(buf) {
				return "", j, ErrInvalidUTF8
			}
			return unsafe.String(unsafe.SliceData(buf), len(buf)), j + 1, nil
		}
		if c == '\\' {
			if j+1 >= len(data) {
				return "", len(data), ErrBadString
			}
			esc := data[j+1]
			switch esc {
			case '"', '\\', '/':
				buf = append(buf, esc)
				j += 2
			case 'b':
				buf = append(buf, '\b')
				j += 2
			case 'f':
				buf = append(buf, '\f')
				j += 2
			case 'n':
				buf = append(buf, '\n')
				j += 2
			case 'r':
				buf = append(buf, '\r')
				j += 2
			case 't':
				buf = append(buf, '\t')
				j += 2
			case 'u':
				if j+6 > len(data) {
					return "", uEscapeEnd(data, j), ErrBadString
				}
				r, ok := parseHex4(data[j+2 : j+6])
				if !ok {
					return "", j, ErrBadString
				}
				j += 6
				if utf16.IsSurrogate(r) {
					if j+6 <= len(data) {
						if data[j] == '\\' && data[j+1] == 'u' {
							if r2, ok := parseHex4(data[j+2 : j+6]); ok {
								if dec := utf16.DecodeRune(r, r2); dec != utf8.RuneError {
									r = dec
									j += 6
								}
							}
						}
					} else if validate && uEscapePrefix(data[j:]) {
						// The second half of the pair may still follow. The
						// input is cut short, not an unpaired surrogate.
						return "", len(data), ErrInvalidUTF8
					}
					// Still a surrogate, so it is unpaired. jsonv2 rejects it.
					// Without validate it becomes U+FFFD like encoding/json,
					// because AppendRune writes RuneError for a surrogate.
					if validate && utf16.IsSurrogate(r) {
						return "", j, ErrInvalidUTF8
					}
				}
				buf = utf8.AppendRune(buf, r)
			default:
				return "", j, ErrBadString
			}
			continue
		}
		// A byte that is neither '"' nor '\\' only reaches here when the
		// inner loop hit escRunWindow in the middle of plain bytes. The next
		// pass of the inner loop copies it.
	}
	return "", len(data), ErrUnterminated
}

// stringUnterminated returns the same position and error that stringSlow
// would for a string with no unescaped closing quote and a backslash at j.
// stringSlow cannot succeed there, so building its buffer would be wasted
// work. skipString cannot be used instead, because it does not check
// surrogates.
func stringUnterminated(data []byte, start, j int, validate bool) (string, int, error) {
	if bad, _ := ctrlOrHigh(data[start:j]); bad {
		return "", start + ctrlIndex(data[start:j]), ErrBadString
	}
	for j < len(data) {
		c := data[j]
		if c < 0x20 {
			return "", j, ErrBadString
		}
		if c != '\\' {
			j++
			continue
		}
		if j+1 >= len(data) {
			return "", len(data), ErrBadString
		}
		switch data[j+1] {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			j += 2
		case 'u':
			if j+6 > len(data) {
				return "", uEscapeEnd(data, j), ErrBadString
			}
			r, ok := parseHex4(data[j+2 : j+6])
			if !ok {
				return "", j, ErrBadString
			}
			j += 6
			if utf16.IsSurrogate(r) {
				if j+6 <= len(data) {
					if data[j] == '\\' && data[j+1] == 'u' {
						if r2, ok := parseHex4(data[j+2 : j+6]); ok {
							if utf16.DecodeRune(r, r2) != utf8.RuneError {
								j += 6
								continue
							}
						}
					}
				} else if validate && uEscapePrefix(data[j:]) {
					return "", len(data), ErrInvalidUTF8
				}
				if validate {
					return "", j, ErrInvalidUTF8
				}
			}
		default:
			return "", j, ErrBadString
		}
	}
	return "", len(data), ErrUnterminated
}

func parseHex4(b []byte) (rune, bool) {
	var r rune
	for _, h := range b {
		r <<= 4
		switch {
		case h >= '0' && h <= '9':
			r |= rune(h - '0')
		case h >= 'a' && h <= 'f':
			r |= rune(h - 'a' + 10)
		case h >= 'A' && h <= 'F':
			r |= rune(h - 'A' + 10)
		default:
			return 0, false
		}
	}
	return r, true
}

// uEscapePrefix reports whether tail, which is shorter than a full `\uXXXX`
// escape, could still become one with more input. That is true for an empty
// tail, `\`, `\u`, and `\u` followed only by hex digits. Anything else is
// malformed no matter what follows. A stream must not read more input for
// it, because a live reader could block forever.
func uEscapePrefix(tail []byte) bool {
	switch {
	case len(tail) == 0:
		return true
	case tail[0] != '\\':
		return false
	case len(tail) == 1:
		return true
	case tail[1] != 'u':
		return false
	}
	_, ok := parseHex4(tail[2:])
	return ok
}

// uEscapeEnd returns the error position for a `\uXXXX` escape at bs that the
// end of data cuts short. It is len(data) when the bytes so far are still a
// valid start of an escape, and bs otherwise.
func uEscapeEnd(data []byte, bs int) int {
	if uEscapePrefix(data[bs:]) {
		return len(data)
	}
	return bs
}

// skipString moves past the JSON string at data[i], which must start with
// '"', without decoding it. Escapes are checked only for their form, not
// decoded. SkipValue and skipObject use it where the value is thrown away,
// so they avoid the allocation and decoding that String does for escaped
// strings. On error the returned position is where the scan stopped. It is
// an index inside data for malformed input, and len(data) when the input
// ended first. CaptureValue uses this to tell malformed input from input that
// is cut short.
func skipString(data []byte, i int) (int, error) {
	j := i + 1
	// q remembers the next '"' at or after j, or len(data) if there is none.
	// As j moves past escapes, q stays the first quote ahead. So the total
	// quote search is O(n). Searching again to the same far quote after every
	// escape would be O(n * escapes), which an attacker could exploit with
	// many escapes. Only an escaped quote, where j moves past q, forces a new
	// search.
	q := -1
	for {
		if q < j {
			if rel := bytes.IndexByte(data[j:], '"'); rel >= 0 {
				q = j + rel
			} else {
				q = len(data)
			}
		}
		// Search for a backslash only up to the quote.
		bsRel := bytes.IndexByte(data[j:q], '\\')
		if q < len(data) && bsRel < 0 {
			if hasCtrlByte(data[j:q]) {
				return j + ctrlIndex(data[j:q]), ErrBadString
			}
			return q + 1, nil
		}
		if bsRel < 0 {
			// A control byte before the end is an error whatever follows.
			// See String.
			if hasCtrlByte(data[j:]) {
				return j + ctrlIndex(data[j:]), ErrBadString
			}
			return len(data), ErrUnterminated
		}
		bs := j + bsRel
		if hasCtrlByte(data[j:bs]) {
			return j + ctrlIndex(data[j:bs]), ErrBadString
		}
		if bs+1 >= len(data) {
			return len(data), ErrBadString
		}
		switch data[bs+1] {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			j = bs + 2
		case 'u':
			if bs+6 > len(data) {
				return uEscapeEnd(data, bs), ErrBadString
			}
			if _, ok := parseHex4(data[bs+2 : bs+6]); !ok {
				return bs, ErrBadString
			}
			j = bs + 6
		default:
			return bs, ErrBadString
		}
	}
}

// Limits for parsing int64 and uint64 values. SignedNeg is the magnitude of
// math.MinInt64, which is 1<<63. They are exported because generated code
// uses them in its inlined scan loops.
const (
	Uint64Limit = ^uint64(0)      // MaxUint64
	SignedNeg   = uint64(1) << 63 // |MinInt64|
)

// Int64 scans a JSON number that must be an integer. A fraction or exponent
// is an error. Overflow returns ErrNumberOverflow instead of wrapping.
func Int64(data []byte, i int) (int64, int, error) {
	neg := false
	if i < len(data) && data[i] == '-' {
		neg = true
		i++
	}
	if i >= len(data) || data[i] < '0' || data[i] > '9' {
		return 0, i, ErrBadNumber
	}
	// RFC 8259 forbids leading zeros such as "01", so a leading '0' must end
	// the integer part. This costs one compare on the first byte.
	if data[i] == '0' && i+1 < len(data) && data[i+1] >= '0' && data[i+1] <= '9' {
		return 0, i, ErrBadNumber
	}
	limit := uint64(math.MaxInt64)
	if neg {
		limit = SignedNeg
	}
	var u uint64
	// Up to 18 digits cannot overflow int64, so they skip the per-digit
	// check. A 19th digit goes through the checked loop below.
	de := min(i+18, len(data))
	for i < de && data[i] >= '0' && data[i] <= '9' {
		u = u*10 + uint64(data[i]-'0')
		i++
	}
	for i < len(data) && data[i] >= '0' && data[i] <= '9' {
		d := uint64(data[i] - '0')
		if u > limit/10 || (u == limit/10 && d > limit%10) {
			return 0, i, ErrNumberOverflow
		}
		u = u*10 + d
		i++
	}
	if i < len(data) {
		c := data[i]
		if c == '.' || c == 'e' || c == 'E' {
			return 0, i, ErrBadNumber
		}
	}
	if neg {
		// -int64(u) would overflow at u == 1<<63, so return MinInt64.
		if u == SignedNeg {
			return math.MinInt64, i, nil
		}
		return -int64(u), i, nil
	}
	return int64(u), i, nil
}

// Uint64 scans a JSON number that must be a non-negative integer. It returns
// ErrNumberOverflow when the value exceeds [math.MaxUint64].
func Uint64(data []byte, i int) (uint64, int, error) {
	if i >= len(data) || data[i] < '0' || data[i] > '9' {
		return 0, i, ErrBadNumber
	}
	// No leading zeros. See Int64.
	if data[i] == '0' && i+1 < len(data) && data[i+1] >= '0' && data[i+1] <= '9' {
		return 0, i, ErrBadNumber
	}
	var n uint64
	// Up to 19 digits cannot overflow uint64. A 20th digit goes through the
	// checked loop. See Int64.
	de := min(i+19, len(data))
	for i < de && data[i] >= '0' && data[i] <= '9' {
		n = n*10 + uint64(data[i]-'0')
		i++
	}
	for i < len(data) && data[i] >= '0' && data[i] <= '9' {
		d := uint64(data[i] - '0')
		if n > Uint64Limit/10 || (n == Uint64Limit/10 && d > Uint64Limit%10) {
			return 0, i, ErrNumberOverflow
		}
		n = n*10 + d
		i++
	}
	if i < len(data) {
		c := data[i]
		if c == '.' || c == 'e' || c == 'E' {
			return 0, i, ErrBadNumber
		}
	}
	return n, i, nil
}

// Float64 scans a JSON number into a float64. It checks the JSON number
// grammar, then converts with a fast exact path or strconv.ParseFloat.
func Float64(data []byte, i int) (float64, int, error) {
	start := i
	// The RFC 8259 number grammar, written inline. It duplicates skipNumber
	// on purpose: calling skipNumber here measured much slower on this hot
	// path. The strict check matters because strconv accepts forms that JSON
	// forbids, such as ".5", "1.", "+1" and "01". It also keeps decode and
	// skip in agreement.
	n := len(data)
	if i < n && data[i] == '-' {
		i++
	}
	if i >= n {
		return 0, i, ErrBadNumber
	}
	switch {
	case data[i] == '0':
		i++
	case data[i] >= '1' && data[i] <= '9':
		i++
		for i < n && data[i] >= '0' && data[i] <= '9' {
			i++
		}
	default:
		return 0, i, ErrBadNumber
	}
	if i < n && data[i] == '.' {
		i++
		if i >= n || data[i] < '0' || data[i] > '9' {
			return 0, i, ErrBadNumber
		}
		i++
		for i < n && data[i] >= '0' && data[i] <= '9' {
			i++
		}
	}
	if i < n && (data[i] == 'e' || data[i] == 'E') {
		i++
		if i < n && (data[i] == '+' || data[i] == '-') {
			i++
		}
		if i >= n || data[i] < '0' || data[i] > '9' {
			return 0, i, ErrBadNumber
		}
		i++
		for i < n && data[i] >= '0' && data[i] <= '9' {
			i++
		}
	}
	// A short number that converts exactly skips ParseFloat, which would scan
	// it again. The 16-byte limit keeps out floats with 17 significant digits,
	// which are at least 18 characters long. Without the limit, those made
	// this fast path slower (see .claude/backlog.md).
	if i-start <= 16 {
		if v, ok := exactShort(data[start:i]); ok {
			return v, i, nil
		}
	}
	raw := unsafe.String(unsafe.SliceData(data[start:]), i-start)
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, start, err
	}
	return v, i, nil
}

// Float32 scans a JSON number into a float32. It rounds the decimal once,
// as strconv.ParseFloat(s, 32) does. Parsing to float64 and then converting
// would round twice. That gives a result one float32 step off whenever the
// float64 lands exactly halfway between two float32 values. For example
// 1.0000000596046448, the shortest form of 1+2^-24, would decode to 1. It
// would also turn the in-range 3.4028235677973366e38 into an overflow. A
// value outside the float32 range returns ErrNumberOverflow.
func Float32(data []byte, i int) (float32, int, error) {
	start := i
	// The grammar check is copied from Float64. A shared function call would
	// be slower, as noted there.
	n := len(data)
	if i < n && data[i] == '-' {
		i++
	}
	if i >= n {
		return 0, i, ErrBadNumber
	}
	switch {
	case data[i] == '0':
		i++
	case data[i] >= '1' && data[i] <= '9':
		i++
		for i < n && data[i] >= '0' && data[i] <= '9' {
			i++
		}
	default:
		return 0, i, ErrBadNumber
	}
	if i < n && data[i] == '.' {
		i++
		if i >= n || data[i] < '0' || data[i] > '9' {
			return 0, i, ErrBadNumber
		}
		i++
		for i < n && data[i] >= '0' && data[i] <= '9' {
			i++
		}
	}
	if i < n && (data[i] == 'e' || data[i] == 'E') {
		i++
		if i < n && (data[i] == '+' || data[i] == '-') {
			i++
		}
		if i >= n || data[i] < '0' || data[i] > '9' {
			return 0, i, ErrBadNumber
		}
		i++
		for i < n && data[i] >= '0' && data[i] <= '9' {
			i++
		}
	}
	if i-start <= 16 {
		if v, ok := exactShort(data[start:i]); ok {
			if f, ok := narrowExact(v); ok {
				return f, i, nil
			}
		}
	}
	raw := unsafe.String(unsafe.SliceData(data[start:]), i-start)
	v, err := strconv.ParseFloat(raw, 32)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return 0, start, ErrNumberOverflow
		}
		return 0, start, err
	}
	return float32(v), i, nil
}

// narrowExact converts a correctly rounded float64 to float32 when the
// result is sure to match a direct float32 parse of the same decimal. That
// holds when v lies strictly between two float32 halfway points, because
// then both parses round to the same float32. It gives up when v is exactly a
// halfway point, because the original decimal may lie on either side of it.
// It also gives up in float32's subnormal range (the tiny values near zero),
// where the halfway points are spaced differently than the bit test assumes.
func narrowExact(v float64) (float32, bool) {
	b := math.Float64bits(v)
	if b&(1<<29-1) == 1<<28 {
		return 0, false
	}
	if e := b >> 52 & 0x7FF; e != 0 && e < 1023-126 {
		return 0, false
	}
	return float32(v), true
}

// exactPow10[k] is 10^k. float64 holds these exactly for k up to 22.
var exactPow10 = [23]float64{
	1e0, 1e1, 1e2, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9, 1e10, 1e11,
	1e12, 1e13, 1e14, 1e15, 1e16, 1e17, 1e18, 1e19, 1e20, 1e21, 1e22,
}

// exactShort parses [-]digits[.digits][eE[+-]digits] when the conversion
// can be done exactly (Clinger's method). A mantissa of at most 2^53-1 fits a
// float64 exactly. An exponent with |exp - fracDigits| <= 22 gives an exact
// power of ten. Then one float64 multiply or divide is correctly rounded, so
// the result is bit for bit what strconv.ParseFloat returns. strconv's
// atof64exact uses the same argument, with a limit of 2^52, one bit tighter
// than needed. Outside those limits it returns ok=false, and the caller uses
// ParseFloat. The caller keeps len(s) <= 16, so neither counter can overflow.
func exactShort(s []byte) (float64, bool) {
	i := 0
	neg := s[0] == '-'
	if neg {
		i = 1
	}
	var mant uint64
	digits, frac, exp := 0, 0, 0
	sawDot := false
	for ; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			mant = mant*10 + uint64(c-'0')
			digits++
			if sawDot {
				frac++
			}
			continue
		}
		if c == '.' && !sawDot {
			sawDot = true
			continue
		}
		if c != 'e' && c != 'E' {
			return 0, false
		}
		i++
		esign := 1
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			if s[i] == '-' {
				esign = -1
			}
			i++
		}
		if i >= len(s) {
			return 0, false
		}
		for ; i < len(s); i++ {
			d := s[i]
			if d < '0' || d > '9' {
				return 0, false
			}
			exp = exp*10 + int(d-'0')
			// |exp - frac| <= 22 with frac <= 15 means no usable exp is above
			// 37. Stopping here also keeps exp from wrapping around a 32-bit
			// int on long exponents, so "1e4294967296" never reads as exp 0.
			if exp > 999 {
				return 0, false
			}
		}
		exp *= esign
		break
	}
	// mant>>53 != 0 means mant >= 2^53, where float64(mant) starts rounding.
	if digits == 0 || mant>>53 != 0 {
		return 0, false
	}
	f := float64(mant)
	// Numbers without an exponent are the common case. The 16-byte limit
	// keeps frac at 15 or below, so they need no range check and skip the
	// power-of-ten work below. An explicit exponent of 0 also lands here,
	// where the power is just -frac.
	if exp == 0 {
		if frac > 0 {
			f /= exactPow10[frac]
		}
	} else {
		power := exp - frac
		if power < -22 || power > 22 {
			return 0, false
		}
		switch {
		case power > 0:
			f *= exactPow10[power]
		case power < 0:
			f /= exactPow10[-power]
		}
	}
	if neg {
		f = -f
	}
	return f, true
}

// skipNumber checks and consumes a JSON number by the RFC 8259 grammar
//
//	-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?
//
// It does not check the range, so a valid number that no Go type can hold,
// such as 1e400, is consumed. Malformed input returns ErrBadNumber.
func skipNumber(data []byte, i int) (int, error) {
	n := len(data)
	if i < n && data[i] == '-' {
		i++
	}
	if i >= n {
		return i, ErrBadNumber
	}
	switch {
	case data[i] == '0':
		i++
	case data[i] >= '1' && data[i] <= '9':
		i++
		for i < n && data[i] >= '0' && data[i] <= '9' {
			i++
		}
	default:
		return i, ErrBadNumber
	}
	if i < n && data[i] == '.' {
		i++
		if i >= n || data[i] < '0' || data[i] > '9' {
			return i, ErrBadNumber
		}
		i++
		for i < n && data[i] >= '0' && data[i] <= '9' {
			i++
		}
	}
	if i < n && (data[i] == 'e' || data[i] == 'E') {
		i++
		if i < n && (data[i] == '+' || data[i] == '-') {
			i++
		}
		if i >= n || data[i] < '0' || data[i] > '9' {
			return i, ErrBadNumber
		}
		i++
		for i < n && data[i] >= '0' && data[i] <= '9' {
			i++
		}
	}
	return i, nil
}

// The bytes of "null", "true" and "alse" read as one little-endian uint32.
// "alse" is the tail of "false". Comparing 4 bytes at once is one compare.
const (
	litNull = 0x6c6c756e // "null"
	litTrue = 0x65757274 // "true"
	litAlse = 0x65736c61 // "alse"
)

// Bool scans a JSON true or false literal. On failure the returned position
// is the start of the literal, not where the scan stopped. Finding that spot
// would make Bool too large for the compiler to inline. Callers that report
// a position call [BoolEnd] on their error path.
func Bool(data []byte, i int) (bool, int, error) {
	if i+4 <= len(data) && binary.LittleEndian.Uint32(data[i:]) == litTrue {
		return true, i + 4, nil
	}
	if i+5 <= len(data) && data[i] == 'f' && binary.LittleEndian.Uint32(data[i+1:]) == litAlse {
		return false, i + 5, nil
	}
	return false, i, ErrBadBool
}

// BoolEnd returns where a failed [Bool] scan at i stopped. That is the first
// byte that does not match the literal, or len(data) when data[i:] is a
// shorter start of it, because more bytes could still complete it. Generated
// bytes-path decoders, the Any functions and SkipValue use it as the error
// position.
func BoolEnd(data []byte, i int) int {
	if i < len(data) && data[i] == 'f' {
		return litEnd(data, i, "false")
	}
	return litEnd(data, i, "true")
}

// Null consumes a null literal. It returns the new position and true on a
// match, and (i, false) otherwise.
func Null(data []byte, i int) (int, bool) {
	if i+4 <= len(data) && binary.LittleEndian.Uint32(data[i:]) == litNull {
		return i + 4, true
	}
	return i, false
}

// SkipValue skips one JSON value of any kind. On error the returned position
// is where the scan stopped. It is a byte inside data for malformed input, and
// len(data) when the input ended first. CaptureValue uses this to tell
// malformed input from input that is cut short.
func SkipValue(data []byte, i int) (int, error) {
	return skipValue(data, i, 0)
}

// litEnd returns where a scan for the literal want stopped. It is len(data)
// when data[i:] is a shorter start of want, because more bytes could still
// complete it. Otherwise it is the index of the first byte that does not
// match, which is final. Bool and Null return the literal start on a miss to
// stay small enough to inline. That position would wrongly mark a partial
// `tru` at the end of a chunk as malformed.
func litEnd(data []byte, i int, want string) int {
	k := 0
	for i+k < len(data) && k < len(want) && data[i+k] == want[k] {
		k++
	}
	if i+k >= len(data) {
		return len(data)
	}
	return i + k
}

func skipValue(data []byte, i, depth int) (int, error) {
	i = SkipSpace(data, i)
	if i >= len(data) {
		return i, ErrUnexpectedEnd
	}
	switch data[i] {
	case '"':
		return skipString(data, i)
	case 't', 'f':
		_, j, err := Bool(data, i)
		if err != nil {
			return BoolEnd(data, i), err
		}
		return j, nil
	case 'n':
		if j, ok := Null(data, i); ok {
			return j, nil
		}
		return litEnd(data, i, "null"), ErrBadLiteral
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return skipNumber(data, i)
	case '[':
		return skipArray(data, i+1, depth+1)
	case '{':
		return skipObject(data, i+1, depth+1)
	}
	return i, ErrBadValue
}

func skipArray(data []byte, i, depth int) (int, error) {
	if depth > maxDepth {
		return i, ErrMaxDepth
	}
	i = SkipSpace(data, i)
	if i < len(data) && data[i] == ']' {
		return i + 1, nil
	}
	for {
		j, err := skipValue(data, i, depth)
		if err != nil {
			return j, err
		}
		i = SkipSpace(data, j)
		if i >= len(data) {
			return i, ErrBadArray
		}
		if data[i] == ',' {
			i++
			continue
		}
		if data[i] == ']' {
			return i + 1, nil
		}
		return i, ErrBadArray
	}
}

func skipObject(data []byte, i, depth int) (int, error) {
	if depth > maxDepth {
		return i, ErrMaxDepth
	}
	i = SkipSpace(data, i)
	if i < len(data) && data[i] == '}' {
		return i + 1, nil
	}
	for {
		if i >= len(data) || data[i] != '"' {
			return i, ErrBadObject
		}
		j, err := skipString(data, i)
		if err != nil {
			return j, err
		}
		j = SkipSpace(data, j)
		if j >= len(data) || data[j] != ':' {
			return j, ErrBadObject
		}
		j = SkipSpace(data, j+1)
		k, err := skipValue(data, j, depth)
		if err != nil {
			return k, err
		}
		i = SkipSpace(data, k)
		if i >= len(data) {
			return i, ErrBadObject
		}
		if data[i] == ',' {
			i = SkipSpace(data, i+1)
			continue
		}
		if data[i] == '}' {
			return i + 1, nil
		}
		return i, ErrBadObject
	}
}

// Any decodes the JSON value at i into a Go any with the encoding/json
// defaults: null is nil, true and false are bool, a number is float64, a
// string is string, an array is []any and an object is map[string]any.
// Strings alias data as in [String]. validate is the UTF-8 switch from
// [String], applied to every string and object key in the value.
func Any(data []byte, i int, validate bool) (any, int, error) {
	return anyValue(data, i, 0, validate)
}

func anyValue(data []byte, i, depth int, validate bool) (any, int, error) {
	i = SkipSpace(data, i)
	if i >= len(data) {
		return nil, i, ErrUnexpectedEnd
	}
	switch c := data[i]; {
	case c == 'n':
		j, ok := Null(data, i)
		if !ok {
			return nil, litEnd(data, i, "null"), ErrBadLiteral
		}
		return nil, j, nil
	case c == 't' || c == 'f':
		v, j, err := Bool(data, i)
		if err != nil {
			return v, BoolEnd(data, i), err
		}
		return v, j, nil
	case c == '"':
		s, j, err := String(data, i, validate)
		return s, j, err
	case c == '-' || (c >= '0' && c <= '9'):
		v, j, err := Float64(data, i)
		return v, j, err
	case c == '[':
		return anyArray(data, i, depth+1, validate)
	case c == '{':
		return anyObject(data, i, depth+1, validate)
	}
	return nil, i, ErrBadLiteral
}

func anyArray(data []byte, i, depth int, validate bool) ([]any, int, error) {
	if depth > maxDepth {
		return nil, i, ErrMaxDepth
	}
	i++ // consume '['
	i = SkipSpace(data, i)
	if i < len(data) && data[i] == ']' {
		return []any{}, i + 1, nil
	}
	out := make([]any, 0, 4)
	for {
		v, j, err := anyValue(data, i, depth, validate)
		if err != nil {
			return nil, j, err
		}
		out = append(out, v)
		i = SkipSpace(data, j)
		if i >= len(data) {
			return nil, i, ErrBadArray
		}
		if data[i] == ',' {
			i = SkipSpace(data, i+1)
			continue
		}
		if data[i] == ']' {
			return out, i + 1, nil
		}
		return nil, i, ErrBadArray
	}
}

func anyObject(data []byte, i, depth int, validate bool) (map[string]any, int, error) {
	if depth > maxDepth {
		return nil, i, ErrMaxDepth
	}
	i++ // consume '{'
	i = SkipSpace(data, i)
	if i < len(data) && data[i] == '}' {
		return map[string]any{}, i + 1, nil
	}
	out := make(map[string]any, 4)
	for {
		key, j, err := String(data, i, validate)
		if err != nil {
			return nil, j, err
		}
		j = SkipSpace(data, j)
		if j >= len(data) || data[j] != ':' {
			return nil, j, ErrBadObject
		}
		j = SkipSpace(data, j+1)
		v, k, err := anyValue(data, j, depth, validate)
		if err != nil {
			return nil, k, err
		}
		out[key] = v
		i = SkipSpace(data, k)
		if i >= len(data) {
			return nil, i, ErrBadObject
		}
		if data[i] == ',' {
			i = SkipSpace(data, i+1)
			continue
		}
		if data[i] == '}' {
			return out, i + 1, nil
		}
		return nil, i, ErrBadObject
	}
}

// Number scans a JSON number and returns its raw text as a json.Number. The
// result aliases data, so the caller must not modify data while it is in
// use. The grammar is the same as in [Float64], but the number is not
// converted.
func Number(data []byte, i int) (json.Number, int, error) {
	start := i
	i, err := skipNumber(data, i) // strict grammar, see Float64
	if err != nil {
		return "", i, err
	}
	return json.Number(unsafe.String(unsafe.SliceData(data[start:]), i-start)), i, nil
}

// AnyNumber is [Any] with numbers decoded as json.Number instead of float64,
// which keeps their exact digits. It matches json.Decoder.UseNumber in
// encoding/json.
func AnyNumber(data []byte, i int, validate bool) (any, int, error) {
	return anyNumberValue(data, i, 0, validate)
}

func anyNumberValue(data []byte, i, depth int, validate bool) (any, int, error) {
	i = SkipSpace(data, i)
	if i >= len(data) {
		return nil, i, ErrUnexpectedEnd
	}
	switch c := data[i]; {
	case c == 'n':
		j, ok := Null(data, i)
		if !ok {
			return nil, litEnd(data, i, "null"), ErrBadLiteral
		}
		return nil, j, nil
	case c == 't' || c == 'f':
		v, j, err := Bool(data, i)
		if err != nil {
			return v, BoolEnd(data, i), err
		}
		return v, j, nil
	case c == '"':
		s, j, err := String(data, i, validate)
		return s, j, err
	case c == '-' || (c >= '0' && c <= '9'):
		v, j, err := Number(data, i)
		return v, j, err
	case c == '[':
		return anyNumberArray(data, i, depth+1, validate)
	case c == '{':
		return anyNumberObject(data, i, depth+1, validate)
	}
	return nil, i, ErrBadLiteral
}

func anyNumberArray(data []byte, i, depth int, validate bool) ([]any, int, error) {
	if depth > maxDepth {
		return nil, i, ErrMaxDepth
	}
	i++
	i = SkipSpace(data, i)
	if i < len(data) && data[i] == ']' {
		return []any{}, i + 1, nil
	}
	out := make([]any, 0, 4)
	for {
		v, j, err := anyNumberValue(data, i, depth, validate)
		if err != nil {
			return nil, j, err
		}
		out = append(out, v)
		i = SkipSpace(data, j)
		if i >= len(data) {
			return nil, i, ErrBadArray
		}
		if data[i] == ',' {
			i = SkipSpace(data, i+1)
			continue
		}
		if data[i] == ']' {
			return out, i + 1, nil
		}
		return nil, i, ErrBadArray
	}
}

func anyNumberObject(data []byte, i, depth int, validate bool) (map[string]any, int, error) {
	if depth > maxDepth {
		return nil, i, ErrMaxDepth
	}
	i++
	i = SkipSpace(data, i)
	if i < len(data) && data[i] == '}' {
		return map[string]any{}, i + 1, nil
	}
	out := make(map[string]any, 4)
	for {
		key, j, err := String(data, i, validate)
		if err != nil {
			return nil, j, err
		}
		j = SkipSpace(data, j)
		if j >= len(data) || data[j] != ':' {
			return nil, j, ErrBadObject
		}
		j = SkipSpace(data, j+1)
		v, k, err := anyNumberValue(data, j, depth, validate)
		if err != nil {
			return nil, k, err
		}
		out[key] = v
		i = SkipSpace(data, k)
		if i >= len(data) {
			return nil, i, ErrBadObject
		}
		if data[i] == ',' {
			i = SkipSpace(data, i+1)
			continue
		}
		if data[i] == '}' {
			return out, i + 1, nil
		}
		return nil, i, ErrBadObject
	}
}

// AnyCopy is [Any] with every decoded string, both values and object keys,
// in its own memory instead of aliasing data. Generated bytes-path decoders
// use it under -copy, so an `any` field stays intact if data changes later.
// Numbers decode to float64. Use [AnyNumberCopy] for json.Number. Strings go
// through [Detach], so a string with escapes, which already has its own
// memory, is not copied again.
func AnyCopy(data []byte, i int, validate bool) (any, int, error) {
	return anyCopyValue(data, i, 0, validate)
}

func anyCopyValue(data []byte, i, depth int, validate bool) (any, int, error) {
	i = SkipSpace(data, i)
	if i >= len(data) {
		return nil, i, ErrUnexpectedEnd
	}
	switch c := data[i]; {
	case c == 'n':
		j, ok := Null(data, i)
		if !ok {
			return nil, litEnd(data, i, "null"), ErrBadLiteral
		}
		return nil, j, nil
	case c == 't' || c == 'f':
		v, j, err := Bool(data, i)
		if err != nil {
			return v, BoolEnd(data, i), err
		}
		return v, j, nil
	case c == '"':
		s, j, err := String(data, i, validate)
		if err != nil {
			return nil, j, err
		}
		return Detach(s, data), j, nil
	case c == '-' || (c >= '0' && c <= '9'):
		v, j, err := Float64(data, i)
		return v, j, err
	case c == '[':
		return anyArrayCopy(data, i, depth+1, validate)
	case c == '{':
		return anyObjectCopy(data, i, depth+1, validate)
	}
	return nil, i, ErrBadLiteral
}

func anyArrayCopy(data []byte, i, depth int, validate bool) ([]any, int, error) {
	if depth > maxDepth {
		return nil, i, ErrMaxDepth
	}
	i++ // consume '['
	i = SkipSpace(data, i)
	if i < len(data) && data[i] == ']' {
		return []any{}, i + 1, nil
	}
	out := make([]any, 0, 4)
	for {
		v, j, err := anyCopyValue(data, i, depth, validate)
		if err != nil {
			return nil, j, err
		}
		out = append(out, v)
		i = SkipSpace(data, j)
		if i >= len(data) {
			return nil, i, ErrBadArray
		}
		if data[i] == ',' {
			i = SkipSpace(data, i+1)
			continue
		}
		if data[i] == ']' {
			return out, i + 1, nil
		}
		return nil, i, ErrBadArray
	}
}

func anyObjectCopy(data []byte, i, depth int, validate bool) (map[string]any, int, error) {
	if depth > maxDepth {
		return nil, i, ErrMaxDepth
	}
	i++ // consume '{'
	i = SkipSpace(data, i)
	if i < len(data) && data[i] == '}' {
		return map[string]any{}, i + 1, nil
	}
	out := make(map[string]any, 4)
	for {
		key, j, err := String(data, i, validate)
		if err != nil {
			return nil, j, err
		}
		j = SkipSpace(data, j)
		if j >= len(data) || data[j] != ':' {
			return nil, j, ErrBadObject
		}
		j = SkipSpace(data, j+1)
		v, k, err := anyCopyValue(data, j, depth, validate)
		if err != nil {
			return nil, k, err
		}
		out[Detach(key, data)] = v
		i = SkipSpace(data, k)
		if i >= len(data) {
			return nil, i, ErrBadObject
		}
		if data[i] == ',' {
			i = SkipSpace(data, i+1)
			continue
		}
		if data[i] == '}' {
			return out, i + 1, nil
		}
		return nil, i, ErrBadObject
	}
}

// AnyNumberCopy is [AnyCopy] with numbers decoded as json.Number, for -copy
// together with -usenumber. The json.Number text is copied too, because
// [Number] aliases data like [String].
func AnyNumberCopy(data []byte, i int, validate bool) (any, int, error) {
	return anyNumberCopyValue(data, i, 0, validate)
}

func anyNumberCopyValue(data []byte, i, depth int, validate bool) (any, int, error) {
	i = SkipSpace(data, i)
	if i >= len(data) {
		return nil, i, ErrUnexpectedEnd
	}
	switch c := data[i]; {
	case c == 'n':
		j, ok := Null(data, i)
		if !ok {
			return nil, litEnd(data, i, "null"), ErrBadLiteral
		}
		return nil, j, nil
	case c == 't' || c == 'f':
		v, j, err := Bool(data, i)
		if err != nil {
			return v, BoolEnd(data, i), err
		}
		return v, j, nil
	case c == '"':
		s, j, err := String(data, i, validate)
		if err != nil {
			return nil, j, err
		}
		return Detach(s, data), j, nil
	case c == '-' || (c >= '0' && c <= '9'):
		n, j, err := Number(data, i)
		if err != nil {
			return nil, j, err
		}
		return json.Number(strings.Clone(string(n))), j, nil
	case c == '[':
		return anyNumberArrayCopy(data, i, depth+1, validate)
	case c == '{':
		return anyNumberObjectCopy(data, i, depth+1, validate)
	}
	return nil, i, ErrBadLiteral
}

func anyNumberArrayCopy(data []byte, i, depth int, validate bool) ([]any, int, error) {
	if depth > maxDepth {
		return nil, i, ErrMaxDepth
	}
	i++
	i = SkipSpace(data, i)
	if i < len(data) && data[i] == ']' {
		return []any{}, i + 1, nil
	}
	out := make([]any, 0, 4)
	for {
		v, j, err := anyNumberCopyValue(data, i, depth, validate)
		if err != nil {
			return nil, j, err
		}
		out = append(out, v)
		i = SkipSpace(data, j)
		if i >= len(data) {
			return nil, i, ErrBadArray
		}
		if data[i] == ',' {
			i = SkipSpace(data, i+1)
			continue
		}
		if data[i] == ']' {
			return out, i + 1, nil
		}
		return nil, i, ErrBadArray
	}
}

func anyNumberObjectCopy(data []byte, i, depth int, validate bool) (map[string]any, int, error) {
	if depth > maxDepth {
		return nil, i, ErrMaxDepth
	}
	i++
	i = SkipSpace(data, i)
	if i < len(data) && data[i] == '}' {
		return map[string]any{}, i + 1, nil
	}
	out := make(map[string]any, 4)
	for {
		key, j, err := String(data, i, validate)
		if err != nil {
			return nil, j, err
		}
		j = SkipSpace(data, j)
		if j >= len(data) || data[j] != ':' {
			return nil, j, ErrBadObject
		}
		j = SkipSpace(data, j+1)
		v, k, err := anyNumberCopyValue(data, j, depth, validate)
		if err != nil {
			return nil, k, err
		}
		out[Detach(key, data)] = v
		i = SkipSpace(data, k)
		if i >= len(data) {
			return nil, i, ErrBadObject
		}
		if data[i] == ',' {
			i = SkipSpace(data, i+1)
			continue
		}
		if data[i] == '}' {
			return out, i + 1, nil
		}
		return nil, i, ErrBadObject
	}
}
