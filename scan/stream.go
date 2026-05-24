package scan

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

// Stream wraps an io.Reader. Scan primitives operate on absolute
// offsets into the buffer. When a primitive's bounds check fails,
// it calls ReadMore(0) to pull a single chunk without shifting —
// internal Stream methods preserve the offset coordinate system so
// their callers' cursors stay valid. Callers that own the buffer at
// a coarser granularity (top-level decode loops between iterations)
// can pass ReadMore(keep) with keep > 0 to compact the buffer; see
// the ReadMore doc for the cursor-adjustment contract.
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
	// Err is the sticky reader error. Once set (non-EOF), every
	// subsequent ReadMore call returns it without touching the reader.
	Err error
	// EOF flips true once the reader has signaled io.EOF.
	EOF bool
	// Shift controls in-place buffer compaction. When true (default
	// after Reset), ReadMore honors `keep > 0` and memmoves the kept
	// suffix down to offset 0. When false, ReadMore treats any keep
	// as 0 (grow-only). Callers flip it off for spans that depend on
	// stable absolute buffer offsets — RawJSON capture, json.Unmarshal
	// fallback against s.Bytes()[start:end] — and restore the previous
	// value after.
	Shift bool
}

// Reset binds the Stream to r with buf as the initial backing slice.
// buf is truncated to length 0 — its capacity is retained for parse
// working area. Pass nil to start with no backing (ReadMore allocates
// on first pull); pass a pre-sized slice to avoid growth allocs.
// Shift defaults to true so generated decoders compact the buffer
// across long streams; flip it off for RawJSON-style stable-offset
// spans.
func (s *Stream) Reset(r io.Reader, buf []byte) {
	*s = Stream{buf: buf[:0], r: r, Shift: true}
}

// Bytes returns the current backing buffer. Mutating the slice
// corrupts the Stream. Callers can recycle the slice after decode
// completes — strings inside the decoded value are owned copies.
func (s *Stream) Bytes() []byte { return s.buf }

// ReadMore pulls a single chunk from the reader. `keep` is the lowest
// offset the caller still requires; bytes at offsets < keep are
// eligible for discard:
//
//   - keep == 0           — grow without shifting (allocate larger
//     backing if currently full).
//   - keep == len(s.buf)  — discard everything; reads refill from 0.
//   - 0 < keep < len(buf) — in-place memmove of s.buf[keep:n] down
//     to s.buf[0:n-keep], reads refill the freed tail.
//
// Callers MUST subtract `keep` from every cursor / index they hold
// that was previously >= keep. String aliases into s.buf become
// invalid whenever keep > 0 — the bytes physically move (in-place
// memmove on the same backing) and the alias points at the wrong
// content afterwards.
//
// NEVER loops the Read call: one chunk in, return whatever the reader
// gave.
func (s *Stream) ReadMore(keep int) error {
	if !s.Shift {
		keep = 0
	}
	// Shift first, even when we ultimately return an error. Callers
	// adjust their offsets unconditionally after a keep > 0 call, so
	// the buffer state must match that expectation regardless of
	// whether the subsequent Read succeeds.
	if keep > 0 {
		if keep >= len(s.buf) {
			s.buf = s.buf[:0]
		} else {
			copy(s.buf, s.buf[keep:])
			s.buf = s.buf[:len(s.buf)-keep]
		}
	}
	if s.Err != nil {
		return s.Err
	}
	if s.EOF {
		return io.ErrUnexpectedEOF
	}
	if keep == 0 && cap(s.buf) == len(s.buf) {
		bigger := make([]byte, len(s.buf), max(cap(s.buf)*2, 1024))
		copy(bigger, s.buf)
		s.buf = bigger
	}
	n, err := s.r.Read(s.buf[len(s.buf):cap(s.buf)])
	s.buf = s.buf[:len(s.buf)+n]
	if err == io.EOF {
		s.EOF = true
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	if err != nil {
		s.Err = err
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
// Compacts in-place: the consumed whitespace is overwritten by the
// next chunk via ReadMore(i). Caller must hold no offsets < i (the
// key alias from a prior KeyView is the obvious one — caller must
// either copy it before SkipSpace or treat it as discardable). When
// the Stream is in no-shift mode (RawJSON capture), ReadMore behaves
// as grow-only and the cursor stays where it was.
func (s *Stream) SkipSpace(i int) (int, error) {
	for {
		if i >= len(s.buf) {
			if err := s.ReadMore(i); err != nil {
				if err == io.ErrUnexpectedEOF {
					return i, nil
				}
				return i, err
			}
			if s.Shift {
				i = 0
			}
		}
		c := s.buf[i]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return i, nil
		}
		i++
	}
}

// ConsumeColon skips whitespace, consumes ':', then skips whitespace
// again — the canonical post-key key/value separator. Shifts in-place
// via the underlying SkipSpace calls, so any aliases the caller holds
// at offsets < i become invalid. Use this only after the key has been
// dispatched / consumed.
func (s *Stream) ConsumeColon(i int) (int, error) {
	i, err := s.SkipSpace(i)
	if err != nil {
		return 0, err
	}
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return 0, err
		}
		if s.Shift {
			i = 0
		}
	}
	if s.buf[i] != ':' {
		return 0, ErrBadObject
	}
	return s.SkipSpace(i + 1)
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
// Compacts in-place when the buffer fills mid-scan: ReadMore(start)
// preserves the partial string body (`s.buf[start:j]`) and discards
// everything before it.
func (s *Stream) String(i int) (string, int, error) {
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return "", 0, err
		}
		if s.Shift {
			i = 0
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
			err := s.ReadMore(start)
			if s.Shift {
				j -= start
				start = 0
			}
			if err != nil {
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

// KeyView reads a JSON string and returns it as an alias into the
// Stream's buffer — zero-copy, zero-allocation on the happy path.
// The returned string remains valid even after subsequent ReadMore
// grows the buf, because Go's GC keeps the underlying backing alive
// as long as any string aliases into it.
//
// USE ONLY for short-lived dispatch where the string never escapes
// the call frame — e.g. object-key matching in `switch len(key)` /
// `if key == "X"` chains. For values that go into the decoded struct,
// use [Stream.String] (which copies).
//
// On escape sequences in the key, falls back to the copy path
// (stringSlow) — aliasing only works when the source bytes ARE the
// final string bytes.
func (s *Stream) KeyView(i int) (string, int, error) {
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return "", 0, err
		}
		if s.Shift {
			i = 0
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
			if bsRel := bytes.IndexByte(s.buf[j:], '\\'); bsRel >= 0 {
				return s.stringSlow(start, j+bsRel)
			}
			for k := j; k < len(s.buf); k++ {
				if s.buf[k] < 0x20 {
					return "", 0, ErrBadString
				}
			}
			j = len(s.buf)
			err := s.ReadMore(start)
			if s.Shift {
				j -= start
				start = 0
			}
			if err != nil {
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
		return unsafe.String(unsafe.SliceData(s.buf[start:]), end-start), end + 1, nil
	}
}

// skipString advances past a JSON string and returns the position one
// byte past the closing `"`. No copy, no body decode — escapes are
// validated only enough to advance correctly (`\uXXXX` consumes 6,
// other escapes 2). Use when the caller doesn't need the string value
// (SkipValue, skipObject key-skip).
func (s *Stream) skipString(i int) (int, error) {
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return 0, err
		}
		if s.Shift {
			i = 0
		}
	}
	if s.buf[i] != '"' {
		return 0, ErrExpectString
	}
	start := i + 1
	j := start
	for {
		rel := bytes.IndexByte(s.buf[j:], '"')
		bsRel := bytes.IndexByte(s.buf[j:], '\\')
		// Closing quote first (no backslash before it) — fast path.
		if rel >= 0 && (bsRel < 0 || rel < bsRel) {
			end := j + rel
			for k := j; k < end; k++ {
				if s.buf[k] < 0x20 {
					return 0, ErrBadString
				}
			}
			return end + 1, nil
		}
		// Backslash before any closing quote — slow path: validate
		// literal bytes up to the backslash, then handle the escape.
		if bsRel >= 0 {
			bs := j + bsRel
			for k := j; k < bs; k++ {
				if s.buf[k] < 0x20 {
					return 0, ErrBadString
				}
			}
			// Need at least one byte past the backslash for the escape kind.
			if bs+1 >= len(s.buf) {
				if err := s.ReadMore(bs); err != nil {
					return 0, ErrBadString
				}
				if s.Shift {
					j -= bs
					start = 0
					bs = 0
				}
			}
			switch s.buf[bs+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				j = bs + 2
			case 'u':
				// Need 4 hex digits past `\u`.
				for bs+6 > len(s.buf) {
					if err := s.ReadMore(bs); err != nil {
						return 0, ErrBadString
					}
					if s.Shift {
						j -= bs
						start = 0
						bs = 0
					}
				}
				if _, ok := parseHex4(s.buf[bs+2 : bs+6]); !ok {
					return 0, ErrBadString
				}
				j = bs + 6
			default:
				return 0, ErrBadString
			}
			continue
		}
		// Neither quote nor backslash in current buffer — validate
		// what's there, then read more.
		for k := j; k < len(s.buf); k++ {
			if s.buf[k] < 0x20 {
				return 0, ErrBadString
			}
		}
		j = len(s.buf)
		err := s.ReadMore(start)
		if s.Shift {
			j -= start
			start = 0
		}
		if err != nil {
			return 0, ErrUnterminated
		}
	}
}

// stringSlow handles escape sequences. Builds a fresh local buffer
// (`buf`) and copies the already-scanned prefix into it, so once we
// enter the slow path the bytes in s.buf at offsets < j are no longer
// needed — every ReadMore inside the loop passes `j` as keep so those
// bytes are discarded and j is reset to 0 in the new coord system.
func (s *Stream) stringSlow(start, j int) (string, int, error) {
	buf := make([]byte, 0, 32)
	buf = append(buf, s.buf[start:j]...)
	for {
		if j >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return "", 0, ErrUnterminated
			}
		}
		c := s.buf[j]
		if c == '"' {
			return string(buf), j + 1, nil
		}
		if c == '\\' {
			if j+1 >= len(s.buf) {
				if err := s.ReadMore(0); err != nil {
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
						if err := s.ReadMore(0); err != nil {
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

// Int64 scans an integer. Accumulates digits into u (uint64) with
// per-digit overflow detection; the sign is applied at the end. No
// buffer span preserved, so the loop's ReadMore passes i as keep —
// bytes < i are discarded and i resets to 0. Matches Int64 in scan.go
// for out-of-range error reporting.
func (s *Stream) Int64(i int) (int64, int, error) {
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return 0, 0, err
		}
		if s.Shift {
			i = 0
		}
	}
	neg := false
	if s.buf[i] == '-' {
		neg = true
		i++
		if i >= len(s.buf) {
			if err := s.ReadMore(i); err != nil {
				return 0, 0, err
			}
			if s.Shift {
				i = 0
			}
		}
	}
	if s.buf[i] < '0' || s.buf[i] > '9' {
		return 0, 0, ErrBadNumber
	}
	limit := uint64(math.MaxInt64)
	if neg {
		limit = SignedNeg
	}
	var u uint64
	for {
		if i >= len(s.buf) {
			err := s.ReadMore(i)
			i = 0
			if err != nil {
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
		d := uint64(c - '0')
		if u > limit/10 || (u == limit/10 && d > limit%10) {
			return 0, 0, ErrNumberOverflow
		}
		u = u*10 + d
		i++
	}
	if neg {
		if u == SignedNeg {
			return math.MinInt64, i, nil
		}
		return -int64(u), i, nil
	}
	return int64(u), i, nil
}

// Uint64 scans an unsigned integer with overflow detection. Returns
// ErrNumberOverflow when the magnitude exceeds MaxUint64.
func (s *Stream) Uint64(i int) (uint64, int, error) {
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return 0, 0, err
		}
		if s.Shift {
			i = 0
		}
	}
	if s.buf[i] < '0' || s.buf[i] > '9' {
		return 0, 0, ErrBadNumber
	}
	var n uint64
	for {
		if i >= len(s.buf) {
			err := s.ReadMore(i)
			i = 0
			if err != nil {
				break
			}
		}
		c := s.buf[i]
		if c < '0' || c > '9' {
			break
		}
		d := uint64(c - '0')
		if n > Uint64Limit/10 || (n == Uint64Limit/10 && d > Uint64Limit%10) {
			return 0, 0, ErrNumberOverflow
		}
		n = n*10 + d
		i++
	}
	return n, i, nil
}

// Float64 scans a JSON number span then delegates to strconv.ParseFloat.
func (s *Stream) Float64(i int) (float64, int, error) {
	if i >= len(s.buf) {
		if err := s.ReadMore(0); err != nil {
			return 0, 0, err
		}
	}
	start := i
	if s.buf[i] == '-' {
		i++
	}
	for {
		if i >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
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
// without fetching the remaining chars. The first byte is captured
// up-front so later compactions can discard s.buf[i] safely.
func (s *Stream) Bool(i int) (bool, int, error) {
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return false, 0, err
		}
		if s.Shift {
			i = 0
		}
	}
	first := s.buf[i]
	var want string
	switch first {
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
			if err := s.ReadMore(0); err != nil {
				return false, 0, ErrBadBool
			}
		}
		if s.buf[pos] != want[k] {
			return false, 0, ErrBadBool
		}
	}
	return first == 't', i + 1 + len(want), nil
}

// SkipValue skips an arbitrary JSON value (literal/number/string/array/object).
func (s *Stream) SkipValue(i int) (int, error) {
	j, err := s.SkipSpace(i)
	if err != nil {
		return 0, err
	}
	if j >= len(s.buf) {
		if err := s.ReadMore(0); err != nil {
			return 0, ErrUnexpectedEnd
		}
		j = 0
	}
	switch s.buf[j] {
	case '"':
		return s.skipString(j)
	case 't', 'f':
		_, k, err := s.Bool(j)
		return k, err
	case 'n':
		for k := range 3 {
			pos := j + 1 + k
			if pos >= len(s.buf) {
				if err := s.ReadMore(0); err != nil {
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
		if err := s.ReadMore(0); err != nil {
			return 0, ErrBadArray
		}
		j = 0
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
			if err := s.ReadMore(0); err != nil {
				return 0, ErrBadArray
			}
			j = 0
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
		if err := s.ReadMore(0); err != nil {
			return 0, ErrBadObject
		}
		j = 0
	}
	if s.buf[j] == '}' {
		return j + 1, nil
	}
	for {
		k, err := s.skipString(j)
		if err != nil {
			return 0, err
		}
		j, err = s.SkipSpace(k)
		if err != nil {
			return 0, err
		}
		if j >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return 0, ErrBadObject
			}
			j = 0
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
			if err := s.ReadMore(0); err != nil {
				return 0, ErrBadObject
			}
			j = 0
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
		if err := s.ReadMore(i); err != nil {
			return nil, 0, err
		}
		if s.Shift {
			i = 0
		}
	}
	switch c := s.buf[i]; {
	case c == 'n':
		for k := range 3 {
			pos := i + 1 + k
			if pos >= len(s.buf) {
				if err := s.ReadMore(0); err != nil {
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
		if err := s.ReadMore(i); err != nil {
			return nil, 0, err
		}
		if s.Shift {
			i = 0
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
			if err := s.ReadMore(i); err != nil {
				return nil, 0, err
			}
			if s.Shift {
				i = 0
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
		if err := s.ReadMore(i); err != nil {
			return nil, 0, err
		}
		if s.Shift {
			i = 0
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
			if err := s.ReadMore(0); err != nil {
				return nil, 0, err
			}
			j = 0
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
			if err := s.ReadMore(i); err != nil {
				return nil, 0, err
			}
			if s.Shift {
				i = 0
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
		if err := s.ReadMore(i); err != nil {
			return "", 0, err
		}
		if s.Shift {
			i = 0
		}
	}
	start := i
	if s.buf[i] == '-' {
		i++
	}
	for {
		if i >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
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
		if err := s.ReadMore(i); err != nil {
			return nil, 0, err
		}
		if s.Shift {
			i = 0
		}
	}
	switch c := s.buf[i]; {
	case c == 'n':
		for k := range 3 {
			pos := i + 1 + k
			if pos >= len(s.buf) {
				if err := s.ReadMore(0); err != nil {
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
		if err := s.ReadMore(i); err != nil {
			return nil, 0, err
		}
		if s.Shift {
			i = 0
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
			if err := s.ReadMore(i); err != nil {
				return nil, 0, err
			}
			if s.Shift {
				i = 0
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
		if err := s.ReadMore(i); err != nil {
			return nil, 0, err
		}
		if s.Shift {
			i = 0
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
			if err := s.ReadMore(0); err != nil {
				return nil, 0, err
			}
			j = 0
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
			if err := s.ReadMore(i); err != nil {
				return nil, 0, err
			}
			if s.Shift {
				i = 0
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
