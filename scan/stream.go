package scan

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

// Stream wraps an io.Reader. Scan primitives operate on absolute
// offsets into the buffer. When a primitive's bounds check fails,
// it calls ReadMore to pull a single chunk; the buffer grows over
// time and offsets stay stable.
//
// Usage:
//
//	var s scan.Stream
//	s.Reset(r, buf)
//
// Strings returned by Stream methods are full copies of the relevant
// span — the buffer is purely a parse working area. Decoded values
// are owned and have no dependency on the buffer; caller can recycle
// the buffer immediately after decode.
type Stream struct {
	buf []byte
	r   io.Reader
	err error // sticky reader error
	eof bool
}

// Reset binds the Stream to r with buf as the initial backing slice.
// buf is truncated to length 0 — its capacity is retained for parse
// working area. Pass nil to start with no backing (ReadMore allocates
// on first pull); pass a pre-sized slice to avoid growth allocs.
func (s *Stream) Reset(r io.Reader, buf []byte) {
	*s = Stream{buf: buf[:0], r: r}
}

// Bytes returns the current backing buffer. Mutating the slice
// corrupts the Stream. Callers can recycle the slice after decode
// completes — strings inside the decoded value are owned copies.
func (s *Stream) Bytes() []byte { return s.buf }

// ReadMore pulls a single chunk from the reader, growing the buffer
// if it's full. NEVER loops, NEVER shifts: one Read in, return
// whatever the reader gave. Caller invokes after a bounds check
// fails and proceeds once at least one new byte has landed. Buffer
// offsets are stable across calls.
func (s *Stream) ReadMore() error {
	if s.err != nil {
		return s.err
	}
	if s.eof {
		return io.ErrUnexpectedEOF
	}
	if cap(s.buf) == len(s.buf) {
		bigger := make([]byte, len(s.buf), max(cap(s.buf)*2, 1024))
		copy(bigger, s.buf)
		s.buf = bigger
	}
	n, err := s.r.Read(s.buf[len(s.buf):cap(s.buf)])
	s.buf = s.buf[:len(s.buf)+n]
	if err == io.EOF {
		s.eof = true
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	if err != nil {
		s.err = err
		return err
	}
	if n == 0 {
		// Pathological reader returned (0, nil). Treat as EOF rather
		// than spinning.
		return io.ErrUnexpectedEOF
	}
	return nil
}

// SkipSpace advances past whitespace, pulling more bytes as needed.
func (s *Stream) SkipSpace(i int) (int, error) {
	for {
		if i >= len(s.buf) {
			if err := s.ReadMore(); err != nil {
				if err == io.ErrUnexpectedEOF {
					return i, nil
				}
				return i, err
			}
		}
		c := s.buf[i]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return i, nil
		}
		i++
	}
}

// ObjectOpen skips whitespace then consumes '{'.
func (s *Stream) ObjectOpen(i int) (int, error) {
	j, err := s.SkipSpace(i)
	if err != nil {
		return 0, err
	}
	if j >= len(s.buf) || s.buf[j] != '{' {
		return 0, ErrBadObject
	}
	return j + 1, nil
}

// ArrayOpen skips whitespace then consumes '['.
func (s *Stream) ArrayOpen(i int) (int, error) {
	j, err := s.SkipSpace(i)
	if err != nil {
		return 0, err
	}
	if j >= len(s.buf) || s.buf[j] != '[' {
		return 0, ErrBadArray
	}
	return j + 1, nil
}

// String decodes a JSON string. Always copies the body out of the
// buffer — the result is owned, no dependency on the buffer.
//
// Uses bytes.IndexByte over the buffered span; on miss, validates
// the already-buffered bytes for backslash/control then pulls more.
func (s *Stream) String(i int) (string, int, error) {
	if i >= len(s.buf) {
		if err := s.ReadMore(); err != nil {
			return "", 0, err
		}
	}
	if s.buf[i] != '"' {
		return "", 0, ErrExpectString
	}
	start := i + 1
	j := start
	for {
		rel := bytes.IndexByte(s.buf[j:], '"')
		if rel < 0 {
			// Closing quote not yet buffered. Validate the buffered span
			// for backslash/control before pulling more bytes — a
			// backslash means we're already on the slow path.
			if bsRel := bytes.IndexByte(s.buf[j:], '\\'); bsRel >= 0 {
				return s.stringSlow(start, j+bsRel)
			}
			for k := j; k < len(s.buf); k++ {
				if s.buf[k] < 0x20 {
					return "", 0, ErrBadString
				}
			}
			j = len(s.buf)
			if err := s.ReadMore(); err != nil {
				return "", 0, ErrUnterminated
			}
			continue
		}
		end := j + rel
		if bsRel := bytes.IndexByte(s.buf[j:end], '\\'); bsRel >= 0 {
			return s.stringSlow(start, j+bsRel)
		}
		for k := j; k < end; k++ {
			if s.buf[k] < 0x20 {
				return "", 0, ErrBadString
			}
		}
		return string(s.buf[start:end]), end + 1, nil
	}
}

// stringSlow handles escape sequences. Builds a fresh buffer — not zero-copy.
func (s *Stream) stringSlow(start, j int) (string, int, error) {
	buf := make([]byte, 0, 32)
	buf = append(buf, s.buf[start:j]...)
	for {
		if j >= len(s.buf) {
			if err := s.ReadMore(); err != nil {
				return "", 0, ErrUnterminated
			}
		}
		c := s.buf[j]
		if c == '"' {
			return string(buf), j + 1, nil
		}
		if c == '\\' {
			if j+1 >= len(s.buf) {
				if err := s.ReadMore(); err != nil {
					return "", 0, ErrBadString
				}
			}
			esc := s.buf[j+1]
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
				for k := range 4 {
					pos := j + 2 + k
					if pos >= len(s.buf) {
						if err := s.ReadMore(); err != nil {
							return "", 0, ErrBadString
						}
					}
				}
				r, ok := parseHex4(s.buf[j+2 : j+6])
				if !ok {
					return "", 0, ErrBadString
				}
				j += 6
				if utf16.IsSurrogate(r) {
					if j+6 <= len(s.buf) && s.buf[j] == '\\' && s.buf[j+1] == 'u' {
						if r2, ok := parseHex4(s.buf[j+2 : j+6]); ok {
							if dec := utf16.DecodeRune(r, r2); dec != utf8.RuneError {
								r = dec
								j += 6
							}
						}
					}
				}
				buf = utf8.AppendRune(buf, r)
			default:
				return "", 0, ErrBadString
			}
			continue
		}
		if c < 0x20 {
			return "", 0, ErrBadString
		}
		buf = append(buf, c)
		j++
	}
}

// Int64 scans an integer.
func (s *Stream) Int64(i int) (int64, int, error) {
	if i >= len(s.buf) {
		if err := s.ReadMore(); err != nil {
			return 0, 0, err
		}
	}
	neg := false
	if s.buf[i] == '-' {
		neg = true
		i++
		if i >= len(s.buf) {
			if err := s.ReadMore(); err != nil {
				return 0, 0, err
			}
		}
	}
	if s.buf[i] < '0' || s.buf[i] > '9' {
		return 0, 0, ErrBadNumber
	}
	var n int64
	for {
		if i >= len(s.buf) {
			if err := s.ReadMore(); err != nil {
				break
			}
		}
		c := s.buf[i]
		if c < '0' || c > '9' {
			if c == '.' || c == 'e' || c == 'E' {
				return 0, 0, ErrBadNumber
			}
			break
		}
		n = n*10 + int64(c-'0')
		i++
	}
	if neg {
		n = -n
	}
	return n, i, nil
}

// Uint64 scans an unsigned integer.
func (s *Stream) Uint64(i int) (uint64, int, error) {
	if i >= len(s.buf) {
		if err := s.ReadMore(); err != nil {
			return 0, 0, err
		}
	}
	if s.buf[i] < '0' || s.buf[i] > '9' {
		return 0, 0, ErrBadNumber
	}
	var n uint64
	for {
		if i >= len(s.buf) {
			if err := s.ReadMore(); err != nil {
				break
			}
		}
		c := s.buf[i]
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + uint64(c-'0')
		i++
	}
	return n, i, nil
}

// Float64 scans a JSON number span then delegates to strconv.ParseFloat.
func (s *Stream) Float64(i int) (float64, int, error) {
	if i >= len(s.buf) {
		if err := s.ReadMore(); err != nil {
			return 0, 0, err
		}
	}
	start := i
	if s.buf[i] == '-' {
		i++
	}
	for {
		if i >= len(s.buf) {
			if err := s.ReadMore(); err != nil {
				break
			}
		}
		c := s.buf[i]
		if c >= '0' && c <= '9' || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
			i++
			continue
		}
		break
	}
	if i == start {
		return 0, 0, ErrBadNumber
	}
	raw := unsafe.String(unsafe.SliceData(s.buf[start:]), i-start)
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, 0, err
	}
	return v, i, nil
}

// Bool scans a true/false literal byte-by-byte: each char is
// bounds-checked individually and one ReadMore is issued only when
// the buffer is exhausted at that position. Mismatch fails fast
// without fetching the remaining chars.
func (s *Stream) Bool(i int) (bool, int, error) {
	if i >= len(s.buf) {
		if err := s.ReadMore(); err != nil {
			return false, 0, err
		}
	}
	var want string
	switch s.buf[i] {
	case 't':
		want = "rue"
	case 'f':
		want = "alse"
	default:
		return false, 0, ErrBadBool
	}
	for k := 0; k < len(want); k++ {
		pos := i + 1 + k
		if pos >= len(s.buf) {
			if err := s.ReadMore(); err != nil {
				return false, 0, ErrBadBool
			}
		}
		if s.buf[pos] != want[k] {
			return false, 0, ErrBadBool
		}
	}
	return s.buf[i] == 't', i + 1 + len(want), nil
}

// SkipValue skips an arbitrary JSON value (literal/number/string/array/object).
func (s *Stream) SkipValue(i int) (int, error) {
	j, err := s.SkipSpace(i)
	if err != nil {
		return 0, err
	}
	if j >= len(s.buf) {
		if err := s.ReadMore(); err != nil {
			return 0, ErrUnexpectedEnd
		}
	}
	switch s.buf[j] {
	case '"':
		_, k, err := s.String(j)
		return k, err
	case 't', 'f':
		_, k, err := s.Bool(j)
		return k, err
	case 'n':
		for k := 0; k < 3; k++ {
			pos := j + 1 + k
			if pos >= len(s.buf) {
				if err := s.ReadMore(); err != nil {
					return 0, ErrBadLiteral
				}
			}
			if s.buf[pos] != "ull"[k] {
				return 0, ErrBadLiteral
			}
		}
		return j + 4, nil
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		_, k, err := s.Float64(j)
		return k, err
	case '[':
		return s.skipArray(j + 1)
	case '{':
		return s.skipObject(j + 1)
	}
	return 0, ErrBadValue
}

func (s *Stream) skipArray(i int) (int, error) {
	j, err := s.SkipSpace(i)
	if err != nil {
		return 0, err
	}
	if j >= len(s.buf) {
		if err := s.ReadMore(); err != nil {
			return 0, ErrBadArray
		}
	}
	if s.buf[j] == ']' {
		return j + 1, nil
	}
	for {
		k, err := s.SkipValue(j)
		if err != nil {
			return 0, err
		}
		j, err = s.SkipSpace(k)
		if err != nil {
			return 0, err
		}
		if j >= len(s.buf) {
			if err := s.ReadMore(); err != nil {
				return 0, ErrBadArray
			}
		}
		if s.buf[j] == ',' {
			j++
			continue
		}
		if s.buf[j] == ']' {
			return j + 1, nil
		}
		return 0, ErrBadArray
	}
}

func (s *Stream) skipObject(i int) (int, error) {
	j, err := s.SkipSpace(i)
	if err != nil {
		return 0, err
	}
	if j >= len(s.buf) {
		if err := s.ReadMore(); err != nil {
			return 0, ErrBadObject
		}
	}
	if s.buf[j] == '}' {
		return j + 1, nil
	}
	for {
		_, k, err := s.String(j)
		if err != nil {
			return 0, err
		}
		j, err = s.SkipSpace(k)
		if err != nil {
			return 0, err
		}
		if j >= len(s.buf) {
			if err := s.ReadMore(); err != nil {
				return 0, ErrBadObject
			}
		}
		if s.buf[j] != ':' {
			return 0, ErrBadObject
		}
		j++
		k, err = s.SkipValue(j)
		if err != nil {
			return 0, err
		}
		j, err = s.SkipSpace(k)
		if err != nil {
			return 0, err
		}
		if j >= len(s.buf) {
			if err := s.ReadMore(); err != nil {
				return 0, ErrBadObject
			}
		}
		if s.buf[j] == ',' {
			j++
			continue
		}
		if s.buf[j] == '}' {
			return j + 1, nil
		}
		return 0, ErrBadObject
	}
}

// Any is the stream-buffered counterpart of [Any]. Mirrors stdlib
// encoding/json defaults for the JSON-to-any mapping.
func (s *Stream) Any(i int) (any, int, error) {
	i, err := s.SkipSpace(i)
	if err != nil {
		return nil, 0, err
	}
	if i >= len(s.buf) {
		if err := s.ReadMore(); err != nil {
			return nil, 0, err
		}
	}
	switch c := s.buf[i]; {
	case c == 'n':
		for k := 0; k < 3; k++ {
			pos := i + 1 + k
			if pos >= len(s.buf) {
				if err := s.ReadMore(); err != nil {
					return nil, 0, err
				}
			}
			if s.buf[pos] != "ull"[k] {
				return nil, 0, ErrBadLiteral
			}
		}
		return nil, i + 4, nil
	case c == 't' || c == 'f':
		v, j, err := s.Bool(i)
		return v, j, err
	case c == '"':
		v, j, err := s.String(i)
		return v, j, err
	case c == '-' || (c >= '0' && c <= '9'):
		v, j, err := s.Float64(i)
		return v, j, err
	case c == '[':
		return s.anyArray(i)
	case c == '{':
		return s.anyObject(i)
	}
	return nil, 0, ErrBadLiteral
}

func (s *Stream) anyArray(i int) ([]any, int, error) {
	i++ // consume '['
	i, err := s.SkipSpace(i)
	if err != nil {
		return nil, 0, err
	}
	if i >= len(s.buf) {
		if err := s.ReadMore(); err != nil {
			return nil, 0, err
		}
	}
	if s.buf[i] == ']' {
		return []any{}, i + 1, nil
	}
	out := make([]any, 0, 4)
	for {
		v, j, err := s.Any(i)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, v)
		i, err = s.SkipSpace(j)
		if err != nil {
			return nil, 0, err
		}
		if i >= len(s.buf) {
			if err := s.ReadMore(); err != nil {
				return nil, 0, err
			}
		}
		if s.buf[i] == ',' {
			i, err = s.SkipSpace(i + 1)
			if err != nil {
				return nil, 0, err
			}
			continue
		}
		if s.buf[i] == ']' {
			return out, i + 1, nil
		}
		return nil, 0, ErrBadArray
	}
}

func (s *Stream) anyObject(i int) (map[string]any, int, error) {
	i++ // consume '{'
	i, err := s.SkipSpace(i)
	if err != nil {
		return nil, 0, err
	}
	if i >= len(s.buf) {
		if err := s.ReadMore(); err != nil {
			return nil, 0, err
		}
	}
	if s.buf[i] == '}' {
		return map[string]any{}, i + 1, nil
	}
	out := make(map[string]any, 4)
	for {
		key, j, err := s.String(i)
		if err != nil {
			return nil, 0, err
		}
		j, err = s.SkipSpace(j)
		if err != nil {
			return nil, 0, err
		}
		if j >= len(s.buf) {
			if err := s.ReadMore(); err != nil {
				return nil, 0, err
			}
		}
		if s.buf[j] != ':' {
			return nil, 0, ErrBadObject
		}
		j, err = s.SkipSpace(j + 1)
		if err != nil {
			return nil, 0, err
		}
		v, k, err := s.Any(j)
		if err != nil {
			return nil, 0, err
		}
		out[key] = v
		i, err = s.SkipSpace(k)
		if err != nil {
			return nil, 0, err
		}
		if i >= len(s.buf) {
			if err := s.ReadMore(); err != nil {
				return nil, 0, err
			}
		}
		if s.buf[i] == ',' {
			i, err = s.SkipSpace(i + 1)
			if err != nil {
				return nil, 0, err
			}
			continue
		}
		if s.buf[i] == '}' {
			return out, i + 1, nil
		}
		return nil, 0, ErrBadObject
	}
}

// Number is the stream-buffered counterpart of [Number]: scans a JSON
// number span and returns it as json.Number copied out of the buffer.
func (s *Stream) Number(i int) (json.Number, int, error) {
	if i >= len(s.buf) {
		if err := s.ReadMore(); err != nil {
			return "", 0, err
		}
	}
	start := i
	if s.buf[i] == '-' {
		i++
	}
	for {
		if i >= len(s.buf) {
			if err := s.ReadMore(); err != nil {
				break
			}
		}
		c := s.buf[i]
		if c >= '0' && c <= '9' || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
			i++
			continue
		}
		break
	}
	if i == start || (i == start+1 && s.buf[start] == '-') {
		return "", 0, ErrBadNumber
	}
	return json.Number(string(s.buf[start:i])), i, nil
}

// AnyNumber is the [Stream.Any] variant that decodes JSON numbers as
// json.Number instead of float64.
func (s *Stream) AnyNumber(i int) (any, int, error) {
	i, err := s.SkipSpace(i)
	if err != nil {
		return nil, 0, err
	}
	if i >= len(s.buf) {
		if err := s.ReadMore(); err != nil {
			return nil, 0, err
		}
	}
	switch c := s.buf[i]; {
	case c == 'n':
		for k := 0; k < 3; k++ {
			pos := i + 1 + k
			if pos >= len(s.buf) {
				if err := s.ReadMore(); err != nil {
					return nil, 0, err
				}
			}
			if s.buf[pos] != "ull"[k] {
				return nil, 0, ErrBadLiteral
			}
		}
		return nil, i + 4, nil
	case c == 't' || c == 'f':
		v, j, err := s.Bool(i)
		return v, j, err
	case c == '"':
		v, j, err := s.String(i)
		return v, j, err
	case c == '-' || (c >= '0' && c <= '9'):
		v, j, err := s.Number(i)
		return v, j, err
	case c == '[':
		return s.anyNumberArray(i)
	case c == '{':
		return s.anyNumberObject(i)
	}
	return nil, 0, ErrBadLiteral
}

func (s *Stream) anyNumberArray(i int) ([]any, int, error) {
	i++
	i, err := s.SkipSpace(i)
	if err != nil {
		return nil, 0, err
	}
	if i >= len(s.buf) {
		if err := s.ReadMore(); err != nil {
			return nil, 0, err
		}
	}
	if s.buf[i] == ']' {
		return []any{}, i + 1, nil
	}
	out := make([]any, 0, 4)
	for {
		v, j, err := s.AnyNumber(i)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, v)
		i, err = s.SkipSpace(j)
		if err != nil {
			return nil, 0, err
		}
		if i >= len(s.buf) {
			if err := s.ReadMore(); err != nil {
				return nil, 0, err
			}
		}
		if s.buf[i] == ',' {
			i, err = s.SkipSpace(i + 1)
			if err != nil {
				return nil, 0, err
			}
			continue
		}
		if s.buf[i] == ']' {
			return out, i + 1, nil
		}
		return nil, 0, ErrBadArray
	}
}

func (s *Stream) anyNumberObject(i int) (map[string]any, int, error) {
	i++
	i, err := s.SkipSpace(i)
	if err != nil {
		return nil, 0, err
	}
	if i >= len(s.buf) {
		if err := s.ReadMore(); err != nil {
			return nil, 0, err
		}
	}
	if s.buf[i] == '}' {
		return map[string]any{}, i + 1, nil
	}
	out := make(map[string]any, 4)
	for {
		key, j, err := s.String(i)
		if err != nil {
			return nil, 0, err
		}
		j, err = s.SkipSpace(j)
		if err != nil {
			return nil, 0, err
		}
		if j >= len(s.buf) {
			if err := s.ReadMore(); err != nil {
				return nil, 0, err
			}
		}
		if s.buf[j] != ':' {
			return nil, 0, ErrBadObject
		}
		j, err = s.SkipSpace(j + 1)
		if err != nil {
			return nil, 0, err
		}
		v, k, err := s.AnyNumber(j)
		if err != nil {
			return nil, 0, err
		}
		out[key] = v
		i, err = s.SkipSpace(k)
		if err != nil {
			return nil, 0, err
		}
		if i >= len(s.buf) {
			if err := s.ReadMore(); err != nil {
				return nil, 0, err
			}
		}
		if s.buf[i] == ',' {
			i, err = s.SkipSpace(i + 1)
			if err != nil {
				return nil, 0, err
			}
			continue
		}
		if s.buf[i] == '}' {
			return out, i + 1, nil
		}
		return nil, 0, ErrBadObject
	}
}
