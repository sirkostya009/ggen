package ggen

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"github.com/sirkostya009/ggen/internal/prealloc"
)

// Stream decodes JSON from an io.Reader. It keeps a buffer that holds a
// window of the input: the bytes read so far that are still needed. A refill
// ([Stream.ReadMore]) reads more bytes into the buffer. The cursor is
// [Stream.Pos]. Every scan method starts reading there and moves it forward
// on return. Raw spans, used for RawJSON capture and the json.Unmarshal
// fallback, come from [Stream.CaptureValue].
//
//	var s Stream
//	s.Reset(r, buf)
//
// Strings returned by Stream methods are owned copies. The buffer is only
// working space for parsing, and it can be reused right after decode.
type Stream struct {
	buf []byte
	r   io.Reader
	// Pos is the parse cursor, the offset in buf of the next byte to read.
	// Every scan method reads it and writes it back. Generated code reads it
	// directly to capture raw spans.
	Pos int
	// consumed is the number of bytes that ReadMore dropped from the front
	// of buf. So buf[0] is at document offset consumed, and the cursor is
	// at document offset consumed + Pos. See Offset. Pos alone is an
	// offset into buf.
	consumed int
}

// Offset returns the byte offset of the cursor in the whole document. It is
// the number of bytes already dropped from the buffer plus [Stream.Pos],
// which counts from the start of the buffer. Use Offset instead of Pos to
// report a position that must stay correct over a long stream.
func (s *Stream) Offset() int { return s.consumed + s.Pos }

// NewStream allocates a Stream that reads from r and starts with buf as its
// buffer. See Reset for how buf is used. Call Reset on an existing Stream
// to reuse it without a heap allocation.
func NewStream(r io.Reader, buf []byte) *Stream {
	return (&Stream{}).Reset(r, buf)
}

// Reset makes the Stream read from r and use buf as its buffer. buf is cut
// to length 0, and its capacity is kept as working space. Pass nil to start
// with no buffer, and ReadMore allocates one on the first read. Pass a slice
// with enough capacity to avoid allocations as the buffer grows.
//
// Reset returns s, so a new Stream can be set up and passed to a decode
// helper in one expression:
//
//	v, err := s.Reset(r, buf).Value[T]()
func (s *Stream) Reset(r io.Reader, buf []byte) *Stream {
	*s = Stream{buf: buf[:0], r: r}
	return s
}

// Bytes returns the current buffer. Changing the slice corrupts the Stream.
// Callers can reuse the slice after decode completes, because strings in the
// decoded value are owned copies.
func (s *Stream) Bytes() []byte { return s.buf }

// ReadMore makes one Read call on the reader. keep is the lowest offset the
// caller still needs. Bytes before keep may be dropped:
//
//   - keep == 0: nothing is dropped. If the buffer is full, a larger one is
//     allocated and the bytes are copied over.
//   - keep == len(s.buf): everything is dropped, and the read fills the
//     buffer from 0.
//   - 0 < keep < len(s.buf): s.buf[keep:] is moved down to the start of the
//     buffer, and the read fills the space freed at the end.
//
// Callers must subtract keep from every cursor or index >= keep they hold,
// including [Stream.Pos] in the middle of a scan. When keep > 0 the bytes
// move, so any string that aliases s.buf, meaning it points into the buffer
// with no copy, is no longer valid.
//
// ReadMore never loops on Read. It reads once and returns what the reader
// gave.
func (s *Stream) ReadMore(keep int) error {
	// Drop bytes first, even if the Read then fails. Callers always adjust
	// their offsets after a keep > 0 call.
	if keep > 0 {
		if keep >= len(s.buf) {
			s.consumed += len(s.buf)
			s.buf = s.buf[:0]
		} else {
			s.consumed += keep
			copy(s.buf, s.buf[keep:])
			s.buf = s.buf[:len(s.buf)-keep]
		}
	}
	if keep == 0 && cap(s.buf) == len(s.buf) {
		bigger := make([]byte, len(s.buf), max(cap(s.buf)*2, 1024))
		copy(bigger, s.buf)
		s.buf = bigger
	}
	n, err := s.r.Read(s.buf[len(s.buf):cap(s.buf)])
	s.buf = s.buf[:len(s.buf)+n]
	if n == 0 {
		// No bytes ends this call. The reader is drained (io.EOF), failed,
		// or returned (0, nil). Do not loop.
		if err == nil || err == io.EOF {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	// Bytes arrived, so deliver them and return nil. The io.Reader contract
	// says to process n > 0 bytes before looking at err. A non-EOF error
	// returned with them comes back on the next Read. Returning it now would
	// break the number scanners, which ignore a refill error. They would
	// return a cut-short number while the new digits sit unread in the
	// buffer.
	return nil
}

// SkipSpace moves Pos past whitespace, reading more bytes as needed. It
// drops the skipped whitespace from the buffer with ReadMore(Pos), so the
// caller must not hold aliases at offsets < Pos, such as an earlier KeyView
// key.
func (s *Stream) SkipSpace() error {
	// Fast path the compiler can inline: the cursor is in bounds on a byte
	// that is not whitespace. Keep it without a local variable. Adding
	// `i := s.Pos` makes it too big to inline.
	if s.Pos < len(s.buf) && s.buf[s.Pos] > ' ' {
		return nil
	}
	return s.skipSpaceSlow()
}

func (s *Stream) skipSpaceSlow() error {
	i := s.Pos
	// buf is copied to a local so the inner loop compares against
	// len(buf) held in a register. The outer loop reloads it after a refill.
	buf := s.buf
	for {
		for i < len(buf) {
			c := buf[i]
			if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				s.Pos = i
				return nil
			}
			i++
		}
		if err := s.ReadMore(i); err != nil {
			s.Pos = 0
			if err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		i = 0
		buf = s.buf
	}
}

// ConsumeColon skips whitespace, reads ':', then skips whitespace again.
// SkipSpace may drop bytes from the buffer, which breaks caller aliases at
// offsets < Pos. Call it only after the key has been matched and is no
// longer needed.
func (s *Stream) ConsumeColon() error {
	if err := s.SkipSpace(); err != nil {
		return err
	}
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return NotEOF(err, ErrBadObject)
		}
		i = 0
	}
	if s.buf[i] != ':' {
		return ErrBadObject
	}
	s.Pos = i + 1
	return s.SkipSpace()
}

// ObjectOpen skips whitespace then reads '{'.
func (s *Stream) ObjectOpen() error {
	if err := s.SkipSpace(); err != nil {
		return err
	}
	j := s.Pos
	if j >= len(s.buf) || s.buf[j] != '{' {
		return ErrBadObject
	}
	s.Pos = j + 1
	return nil
}

// ArrayOpen skips whitespace then reads '['.
func (s *Stream) ArrayOpen() error {
	if err := s.SkipSpace(); err != nil {
		return err
	}
	j := s.Pos
	if j >= len(s.buf) || s.buf[j] != '[' {
		return ErrBadArray
	}
	s.Pos = j + 1
	return nil
}

// String decodes a JSON string. It always copies the body out of the
// buffer, so the result does not depend on the buffer. validate works as in
// the package-level String. false is the allowinvalidutf8 opt-out.
func (s *Stream) String(validate bool) (string, error) {
	v, owned, err := s.stringView(validate)
	if err != nil {
		return "", err
	}
	if owned {
		// stringSlow already copied into a new buffer. Do not clone again.
		return v, nil
	}
	return strings.Clone(v), nil
}

// StringView reads a JSON string and returns it as an alias into the
// Stream's buffer, with no copy. A string with escapes is copied instead.
//
// Use it only when the caller is done with the string before the next
// Stream call and keeps none of its bytes after that. The next ReadMore that
// drops bytes breaks the alias. A caller that keeps any part of the string
// must use [Stream.String], which copies.
func (s *Stream) StringView(validate bool) (string, error) {
	v, _, err := s.stringView(validate)
	return v, err
}

// stringView is the shared scan behind StringView and String. owned
// reports whether v is already in a new buffer, from the stringSlow escape
// path, instead of aliasing s.buf.
func (s *Stream) stringView(validate bool) (v string, owned bool, err error) {
	i := s.Pos
	if i >= len(s.buf) {
		// Input that ends where a string must start reports
		// ErrExpectString, like the bytes String. Any other reader error is
		// returned as is.
		err := s.ReadMore(i)
		i = 0
		if err != nil {
			s.Pos = i
			return "", false, NotEOF(err, ErrExpectString)
		}
	}
	if s.buf[i] != '"' {
		s.Pos = i
		return "", false, ErrExpectString
	}
	start := i + 1
	j := start
	sawHigh := false
	for {
		rel := bytes.IndexByte(s.buf[j:], '"')
		if rel < 0 {
			if bsRel := bytes.IndexByte(s.buf[j:], '\\'); bsRel >= 0 {
				v, err := s.stringSlow(start, j+bsRel, validate)
				return v, true, err
			}
			bad, hi := ctrlOrHigh(s.buf[j:])
			if bad {
				s.Pos = j + ctrlIndex(s.buf[j:])
				return "", false, ErrBadString
			}
			sawHigh = sawHigh || hi
			j = len(s.buf)
			err := s.ReadMore(start)
			j -= start
			start = 0
			if err != nil {
				// After ReadMore(start) the whole window is the string body, so
				// the error position is the end of the window. This matches
				// len(data) in the bytes String, and keeps Offset() correct.
				s.Pos = len(s.buf)
				return "", false, NotEOF(err, ErrUnterminated)
			}
			continue
		}
		end := j + rel
		if bsRel := bytes.IndexByte(s.buf[j:end], '\\'); bsRel >= 0 {
			v, err := s.stringSlow(start, j+bsRel, validate)
			return v, true, err
		}
		bad, hi := ctrlOrHigh(s.buf[j:end])
		if bad {
			s.Pos = j + ctrlIndex(s.buf[j:end])
			return "", false, ErrBadString
		}
		// Validate the full span. A multi-byte rune may cross the cursor j,
		// so validating each piece alone would report false errors.
		// ReadMore(start) keeps the whole span in one slice.
		if validate && (sawHigh || hi) && !utf8.Valid(s.buf[start:end]) {
			s.Pos = start
			return "", false, ErrInvalidUTF8
		}
		s.Pos = end + 1
		return unsafe.String(unsafe.SliceData(s.buf[start:]), end-start), false, nil
	}
}

// KeyView reads a JSON string and returns it as an alias into the Stream's
// buffer, with no copy in the common case. The alias survives a ReadMore
// that only grows the buffer, but a ReadMore that drops bytes breaks it.
//
// Use it only for short-lived matching where the string never leaves the
// calling function, such as matching object keys. For values stored in the
// decoded struct use [Stream.String]. A string with escapes is copied.
func (s *Stream) KeyView(validate bool) (string, error) {
	i := s.Pos
	if i >= len(s.buf) {
		// Input that ends here reports ErrExpectString. See stringView.
		err := s.ReadMore(i)
		i = 0
		if err != nil {
			s.Pos = i
			return "", NotEOF(err, ErrExpectString)
		}
	}
	if s.buf[i] != '"' {
		s.Pos = i
		return "", ErrExpectString
	}
	start := i + 1
	// Short-key check: one byte-by-byte pass over the first few bytes
	// looks for the closing quote and checks for a backslash or control
	// byte. Short object keys finish here. A longer key, an escape, or a
	// quote not yet in the buffer goes to the IndexByte loop below, which
	// starts where this pass stopped (j = we).
	we := min(start+stringPreludeWindow, len(s.buf))
	var hb byte
	for k := start; k < we; k++ {
		c := s.buf[k]
		if c == '"' {
			if validate && hb&0x80 != 0 && !utf8.Valid(s.buf[start:k]) {
				s.Pos = start
				return "", ErrInvalidUTF8
			}
			s.Pos = k + 1
			return unsafe.String(unsafe.SliceData(s.buf[start:]), k-start), nil
		}
		if c == '\\' {
			return s.stringSlow(start, k, validate)
		}
		if c < 0x20 {
			s.Pos = k
			return "", ErrBadString
		}
		hb |= c
	}
	sawHigh := hb&0x80 != 0
	j := we
	for {
		rel := bytes.IndexByte(s.buf[j:], '"')
		if rel < 0 {
			if bsRel := bytes.IndexByte(s.buf[j:], '\\'); bsRel >= 0 {
				return s.stringSlow(start, j+bsRel, validate)
			}
			bad, hi := ctrlOrHigh(s.buf[j:])
			if bad {
				s.Pos = j + ctrlIndex(s.buf[j:])
				return "", ErrBadString
			}
			sawHigh = sawHigh || hi
			j = len(s.buf)
			err := s.ReadMore(start)
			j -= start
			start = 0
			if err != nil {
				s.Pos = len(s.buf)
				return "", NotEOF(err, ErrUnterminated)
			}
			continue
		}
		end := j + rel
		if bsRel := bytes.IndexByte(s.buf[j:end], '\\'); bsRel >= 0 {
			return s.stringSlow(start, j+bsRel, validate)
		}
		bad, hi := ctrlOrHigh(s.buf[j:end])
		if bad {
			s.Pos = j + ctrlIndex(s.buf[j:end])
			return "", ErrBadString
		}
		// Validate the full span, since a rune may cross j. See stringView.
		if validate && (sawHigh || hi) && !utf8.Valid(s.buf[start:end]) {
			s.Pos = start
			return "", ErrInvalidUTF8
		}
		s.Pos = end + 1
		return unsafe.String(unsafe.SliceData(s.buf[start:]), end-start), nil
	}
}

// stringPreludeWindow is how many bytes KeyView checks one at a time before
// it switches to the IndexByte loop.
const stringPreludeWindow = 24

// skipString moves Pos past a JSON string without copying or decoding it.
// Escapes are checked only enough to step over them. It is used when the
// value is not needed, as in SkipValue and when skipObject skips a key.
func (s *Stream) skipString() error {
	i := s.Pos
	if i >= len(s.buf) {
		err := s.ReadMore(i)
		i = 0
		if err != nil {
			s.Pos = i
			return NotEOF(err, ErrExpectString)
		}
	}
	if s.buf[i] != '"' {
		s.Pos = i
		return ErrExpectString
	}
	start := i + 1
	j := start
	// q remembers the next '"' at or after j, or len(buf) when none is
	// buffered. Without it, each escape would search again for the same far
	// quote, which is quadratic. See the bytes-path skipString. Every refill
	// shifts indices, so refill sites set q to -1 to search again.
	q := -1
	for {
		if q < j {
			if rel := bytes.IndexByte(s.buf[j:], '"'); rel >= 0 {
				q = j + rel
			} else {
				q = len(s.buf)
			}
		}
		// Search for a backslash only up to the closing quote. Searching the
		// whole rest of the buffer makes SkipValue quadratic when the whole
		// payload is buffered.
		bsRel := bytes.IndexByte(s.buf[j:q], '\\')
		// Fast path: a closing quote with no backslash before it.
		if q < len(s.buf) && bsRel < 0 {
			if hasCtrlByte(s.buf[j:q]) {
				s.Pos = j + ctrlIndex(s.buf[j:q])
				return ErrBadString
			}
			s.Pos = q + 1
			return nil
		}
		// Slow path: a backslash before any closing quote. Check the
		// plain bytes up to the backslash, then handle the escape.
		if bsRel >= 0 {
			bs := j + bsRel
			if hasCtrlByte(s.buf[j:bs]) {
				s.Pos = j + ctrlIndex(s.buf[j:bs])
				return ErrBadString
			}
			// Need at least one byte past the backslash for the escape kind.
			if bs+1 >= len(s.buf) {
				err := s.ReadMore(bs)
				if err != nil {
					// ReadMore moved the buffer to start at bs. A cut-short
					// escape reports the end of what arrived, like len(data) on
					// the bytes path.
					s.Pos = len(s.buf)
					return NotEOF(err, ErrBadString)
				}
				j -= bs
				bs = 0
				q = -1
			}
			switch s.buf[bs+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				j = bs + 2
			case 'u':
				// Need 4 hex digits after `\u`. Refill only while the
				// buffered tail can still become them. A non-hex byte is a
				// final error and must not wait on a live reader. See
				// uEscapePrefix.
				for bs+6 > len(s.buf) {
					if !uEscapePrefix(s.buf[bs:]) {
						s.Pos = bs
						return ErrBadString
					}
					err := s.ReadMore(bs)
					if err != nil {
						s.Pos = len(s.buf)
						return NotEOF(err, ErrBadString)
					}
					j -= bs
					bs = 0
					q = -1
				}
				if _, ok := parseHex4(s.buf[bs+2 : bs+6]); !ok {
					s.Pos = bs
					return ErrBadString
				}
				j = bs + 6
			default:
				s.Pos = bs
				return ErrBadString
			}
			continue
		}
		// No quote or backslash in the buffer. Check what is there, then
		// read more.
		if hasCtrlByte(s.buf[j:]) {
			s.Pos = j + ctrlIndex(s.buf[j:])
			return ErrBadString
		}
		// Everything buffered is checked and not needed again. Dropping it
		// all keeps the buffer at one read's size, instead of growing it to
		// the length of the skipped string.
		j = len(s.buf)
		err := s.ReadMore(j)
		j = 0
		q = -1
		if err != nil {
			s.Pos = j
			return NotEOF(err, ErrUnterminated)
		}
	}
}

// stringSlow decodes a string with escapes into a new buffer it owns,
// starting with the part already scanned. Refills drop consumed bytes from
// the window, see the comment before the loop. The result points into the
// new buffer, never into s.buf.
func (s *Stream) stringSlow(start, j int, validate bool) (string, error) {
	bad, rawHigh := ctrlOrHigh(s.buf[start:j])
	if bad {
		s.Pos = start + ctrlIndex(s.buf[start:j])
		return "", ErrBadString
	}
	buf := make([]byte, 0, 32)
	buf = append(buf, s.buf[start:j]...)
	var rawHi byte
	// start is no longer needed, because the decoded output lives in buf. So
	// a refill can drop consumed bytes from s.buf instead of growing it to hold
	// the whole raw string. Every ReadMore keeps bytes from the cursor j and
	// shifts j to match, like Int64, Float64 and Number. A multi-megabyte
	// escaped string then passes through a buffer of bounded size. Escapes
	// longer than one byte (\X, \uXXXX, a surrogate pair) first get their whole
	// span into the buffer, with a refill loop that drops consumed bytes, before
	// reading s.buf[j+k].
	for {
		if j >= len(s.buf) {
			err := s.ReadMore(j)
			j = 0
			if err != nil {
				s.Pos = j
				return "", NotEOF(err, ErrUnterminated)
			}
		}
		// Plain bytes are copied by a short inner loop of at most escRunWindow
		// bytes, with escape handling outside it. See the bytes-path stringSlow.
		// The loop also stops at the end of the buffer. A run that reaches it
		// continues after the refill at the top of the outer loop.
		we := min(j+escRunWindow, len(s.buf))
		for j < we {
			c := s.buf[j]
			if c == '"' || c == '\\' {
				break
			}
			if c < 0x20 {
				s.Pos = j
				return "", ErrBadString
			}
			rawHi |= c
			buf = append(buf, c)
			j++
		}
		if j >= len(s.buf) {
			continue // refill at the top of the loop
		}
		c := s.buf[j]
		if c == '"' {
			// Decoded escapes are always valid UTF-8, since lone surrogates
			// fail below. Only plain bytes copied through can make buf
			// invalid, so skip validation when they were all ASCII. buf is
			// the finished output, so a refill cannot split a rune here.
			if validate && (rawHigh || rawHi&0x80 != 0) && !utf8.Valid(buf) {
				s.Pos = j
				return "", ErrInvalidUTF8
			}
			s.Pos = j + 1
			return unsafe.String(unsafe.SliceData(buf), len(buf)), nil
		}
		if c == '\\' {
			// The bounds check stays inline. The refill helper is a call,
			// made only at the end of the window. refillSkip is split the
			// same way.
			if j+2 > len(s.buf) {
				if err := s.ensureSpan(&j, 2); err != nil {
					// A cut-short escape reports the end of what arrived,
					// like len(data) on the bytes path.
					s.Pos = len(s.buf)
					return "", NotEOF(err, ErrBadString)
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
				// Refill only while the tail can still become \uXXXX. See
				// skipString.
				for j+6 > len(s.buf) {
					if !uEscapePrefix(s.buf[j:]) {
						s.Pos = j
						return "", ErrBadString
					}
					err := s.ReadMore(j)
					j = 0
					if err != nil {
						// A cut-short escape reports the end of what arrived,
						// like len(data) on the bytes path.
						s.Pos = len(s.buf)
						return "", NotEOF(err, ErrBadString)
					}
				}
				r, ok := parseHex4(s.buf[j+2 : j+6])
				if !ok {
					s.Pos = j
					return "", ErrBadString
				}
				j += 6
				if utf16.IsSurrogate(r) {
					// A surrogate is half of a UTF-16 pair. Runes above U+FFFF
					// are written as two \uXXXX escapes, a high surrogate then a
					// low one. Pair r with a low-surrogate escape right after it.
					// That escape may cross a refill boundary. Without the refill
					// the pair would split into two lone surrogates and decode
					// as two U+FFFD. Refill only while the buffered tail can
					// still become \uXXXX, so a live reader is never asked for
					// bytes past the closing quote. If the reader is drained, r
					// stays a lone surrogate, the same as on the bytes path.
					var err error
					for j+6 > len(s.buf) && uEscapePrefix(s.buf[j:]) {
						err = s.ReadMore(j)
						j = 0
						if err != nil {
							break
						}
					}
					if j+6 <= len(s.buf) && s.buf[j] == '\\' && s.buf[j+1] == 'u' {
						if r2, ok := parseHex4(s.buf[j+2 : j+6]); ok {
							if dec := utf16.DecodeRune(r, r2); dec != utf8.RuneError {
								r = dec
								j += 6
							}
						}
					}
					if err != nil && err != io.ErrUnexpectedEOF {
						// A reader error other than EOF. Reporting
						// ErrInvalidUTF8 here would call a passing read failure a
						// lone surrogate.
						s.Pos = len(s.buf)
						return "", err
					}
					// Still a surrogate means it has no pair. jsonv2 rejects
					// it. Permissive mode writes U+FFFD in its place.
					if validate && utf16.IsSurrogate(r) {
						s.Pos = j
						if err != nil {
							// The reader was drained while the tail could
							// still become an escape. The input is cut short,
							// not a lone surrogate. Report the end of what
							// arrived, like len(data) on the bytes path.
							s.Pos = len(s.buf)
						}
						return "", ErrInvalidUTF8
					}
				}
				buf = utf8.AppendRune(buf, r)
			default:
				s.Pos = j
				return "", ErrBadString
			}
			continue
		}
		// Neither '"' nor '\\'. The inner loop stopped at its escRunWindow
		// limit in the middle of a run, so the outer loop just continues.
	}
}

// ensureSpan makes sure n bytes from *j are in the buffer. Each refill drops
// the bytes before *j and sets *j to 0. The escapes stringSlow needs whole
// are at most 6 bytes. It returns ReadMore's error when the reader cannot
// supply them.
func (s *Stream) ensureSpan(j *int, n int) error {
	for *j+n > len(s.buf) {
		err := s.ReadMore(*j)
		*j = 0
		if err != nil {
			return err
		}
	}
	return nil
}

// Int64 scans an integer, checking for overflow at each digit and applying
// the sign at the end. It matches the package-level Int64, including error
// positions. A grammar error or overflow reports the byte where scanning
// stopped. Only a reader error other than EOF leaves Pos at the start of the
// value, so a retry scans the whole number again.
func (s *Stream) Int64() (int64, error) {
	i := s.Pos
	if i >= len(s.buf) {
		// ReadMore(i) drops the window before it reads, so the cursor
		// moves to 0 even when the Read fails.
		if err := s.ReadMore(i); err != nil {
			s.Pos = 0
			return 0, NotEOF(err, ErrBadNumber)
		}
		i = 0
	}
	// start is the start of the value. Every refill keeps bytes from start
	// and shifts the cursors. Numbers are about 20 bytes at most, so keeping
	// them costs nothing. Then a reader error other than EOF can return with
	// s.Pos on the whole number, and a retry scans it again. Refilling from
	// the cursor instead would drop the '-' and the digits already added to
	// the value, so a retry would silently lose the sign or digits.
	start := i
	neg := false
	if s.buf[i] == '-' {
		neg = true
		i++
		if i >= len(s.buf) {
			// Input that ends after a bare "-" is a grammar error, like
			// on the bytes path. Any other reader error is returned as is.
			err := s.ReadMore(start)
			i -= start
			start = 0
			if err != nil {
				if err != io.ErrUnexpectedEOF {
					s.Pos = start
					return 0, err
				}
				s.Pos = i
				return 0, ErrBadNumber
			}
		}
	}
	if s.buf[i] < '0' || s.buf[i] > '9' {
		s.Pos = i
		return 0, ErrBadNumber
	}
	// RFC 8259 forbids leading zeros. A first digit '0' must end the integer
	// part. Look at the next byte, refilling if the window ends here, to catch
	// "01".
	if s.buf[i] == '0' {
		if i+1 >= len(s.buf) {
			// ReadMore drops bytes before the Read, so always shift the
			// cursors. Ignore only a drained reader. A bare "0" at EOF is a
			// valid number, and the digit loop then sees EOF again. Any other
			// error must be returned. Otherwise a later refill resumes the
			// digit loop and "01" is silently accepted.
			err := s.ReadMore(start)
			i -= start
			start = 0
			if err != nil && err != io.ErrUnexpectedEOF {
				s.Pos = start
				return 0, err
			}
		}
		if i+1 < len(s.buf) && s.buf[i+1] >= '0' && s.buf[i+1] <= '9' {
			s.Pos = i
			return 0, ErrBadNumber
		}
	}
	limit := uint64(math.MaxInt64)
	if neg {
		limit = SignedNeg
	}
	var u uint64
	buf := s.buf
	// digits counts the digits read without an overflow check, across
	// refills. The first 18 digits cannot overflow int64. See the bytes-path
	// Int64. The checked loop reads digits only once digits reaches 18. If
	// the unchecked loop stops below 18, the buffer ran out, so the checked
	// loop does nothing and the refill follows.
	digits := 0
scan:
	for {
		de := min(i+18-digits, len(buf))
		for i < de && buf[i] >= '0' && buf[i] <= '9' {
			u = u*10 + uint64(buf[i]-'0')
			i++
			digits++
		}
		for i < len(buf) {
			c := buf[i]
			if c < '0' || c > '9' {
				if c == '.' || c == 'e' || c == 'E' {
					s.Pos = i
					return 0, ErrBadNumber
				}
				break scan
			}
			d := uint64(c - '0')
			if u > limit/10 || (u == limit/10 && d > limit%10) {
				s.Pos = i
				return 0, ErrNumberOverflow
			}
			u = u*10 + d
			i++
		}
		err := s.ReadMore(start)
		i -= start
		start = 0
		if err != nil {
			// A drained reader is a valid end of the value, since the
			// digits ran to EOF. Any other reader error is not. Ignoring it
			// would return a cut-short value with a nil error. That is
			// silently wrong at the top level and a false grammar error one
			// level up.
			if err != io.ErrUnexpectedEOF {
				s.Pos = start
				return 0, err
			}
			break
		}
		buf = s.buf
	}
	s.Pos = i
	if neg {
		if u == SignedNeg {
			return math.MinInt64, nil
		}
		return -int64(u), nil
	}
	return int64(u), nil
}

// Uint64 scans an unsigned integer and checks for overflow. It returns
// ErrNumberOverflow when the value is above MaxUint64.
func (s *Stream) Uint64() (uint64, error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			s.Pos = 0
			return 0, NotEOF(err, ErrBadNumber)
		}
		i = 0
	}
	// start is the start of the value. Refills keep bytes from it, so a
	// reader error other than EOF leaves the whole number for a retry. See
	// Int64.
	start := i
	if s.buf[i] < '0' || s.buf[i] > '9' {
		s.Pos = i
		return 0, ErrBadNumber
	}
	// RFC 8259 forbids leading zeros. See Int64 for how errors are handled.
	if s.buf[i] == '0' {
		if i+1 >= len(s.buf) {
			err := s.ReadMore(start)
			i -= start
			start = 0
			if err != nil && err != io.ErrUnexpectedEOF {
				s.Pos = start
				return 0, err
			}
		}
		if i+1 < len(s.buf) && s.buf[i+1] >= '0' && s.buf[i+1] <= '9' {
			s.Pos = i
			return 0, ErrBadNumber
		}
	}
	var n uint64
	buf := s.buf
	// The first 19 digits cannot overflow uint64. See the digits comment in
	// Int64.
	digits := 0
scan:
	for {
		de := min(i+19-digits, len(buf))
		for i < de && buf[i] >= '0' && buf[i] <= '9' {
			n = n*10 + uint64(buf[i]-'0')
			i++
			digits++
		}
		for i < len(buf) {
			c := buf[i]
			if c < '0' || c > '9' {
				if c == '.' || c == 'e' || c == 'E' {
					s.Pos = i
					return 0, ErrBadNumber
				}
				break scan
			}
			d := uint64(c - '0')
			if n > Uint64Limit/10 || (n == Uint64Limit/10 && d > Uint64Limit%10) {
				s.Pos = i
				return 0, ErrNumberOverflow
			}
			n = n*10 + d
			i++
		}
		err := s.ReadMore(start)
		i -= start
		start = 0
		if err != nil {
			// A drained reader is a valid end of the value, since the
			// digits ran to EOF. Any other reader error is not. Ignoring it
			// would return a cut-short value with a nil error. That is
			// silently wrong at the top level and a false grammar error one
			// level up.
			if err != io.ErrUnexpectedEOF {
				s.Pos = start
				return 0, err
			}
			break
		}
		buf = s.buf
	}
	s.Pos = i
	return n, nil
}

// Float64 scans a JSON number and parses it with strconv.ParseFloat. Numbers
// of at most 16 bytes try the exactShort fast path first.
func (s *Stream) Float64() (float64, error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			s.Pos = 0
			return 0, NotEOF(err, ErrBadNumber)
		}
		i = 0
	}
	start := i
	// buf is copied to a local. Refills drop the bytes before start, which
	// are already consumed, and shift the cursors. Without that, every refill
	// in the middle of a number finds the buffer full, since readers fill the
	// whole window, and doubles it. s.buf[start:i] is sliced again after the
	// loop, so shifting keeps it valid.
	buf := s.buf
	if buf[i] == '-' {
		i++
	}
scan:
	for {
		for i < len(buf) {
			c := buf[i]
			if c >= '0' && c <= '9' || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
				i++
				continue
			}
			break scan
		}
		err := s.ReadMore(start)
		i -= start
		start = 0
		if err != nil {
			// A drained reader is a valid end of the value, since the
			// digits ran to EOF. Any other reader error is not. Ignoring it
			// would return a cut-short value with a nil error. That is
			// silently wrong at the top level and a false grammar error one
			// level up.
			if err != io.ErrUnexpectedEOF {
				s.Pos = start
				return 0, err
			}
			break
		}
		buf = s.buf
	}
	if i == start {
		s.Pos = i
		return 0, ErrBadNumber
	}
	// The refill loop collects any run of [0-9.eE+-] bytes, which finds the
	// extent across refills. Now that the span is in one slice, check it
	// against the RFC 8259 grammar. The number ends where the grammar stops,
	// not where the loose scan stopped. The bytes path stops at the first
	// byte that cannot extend the number and leaves the rest to the caller.
	// Cutting the span here keeps the two paths identical on inputs like
	// `1.5.5`, `1e5e` and `01`, instead of failing where the bytes path
	// succeeds. On a grammar error, end is where the grammar stopped, the same
	// position the bytes path reports.
	end, gerr := skipNumber(s.buf[start:i], 0)
	if gerr != nil {
		s.Pos = start + end
		return 0, ErrBadNumber
	}
	i = start + end
	// Short numbers that a float64 holds exactly skip ParseFloat, the same
	// as in the bytes-path Float64. exactShort gives the same bits as
	// ParseFloat for every input it accepts. The 16-byte limit keeps out
	// floats with 17 significant digits, which made the fast path slower
	// without it.
	if i-start <= 16 {
		if v, ok := exactShort(s.buf[start:i]); ok {
			s.Pos = i
			return v, nil
		}
	}
	raw := unsafe.String(unsafe.SliceData(s.buf[start:]), i-start)
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		s.Pos = start
		return 0, err
	}
	s.Pos = i
	return v, nil
}

// Float32 is the stream version of [Float32]. It collects the number like
// [Stream.Float64] and parses it at 32 bits, so the decimal is rounded only
// once.
func (s *Stream) Float32() (float32, error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return 0, NotEOF(err, ErrBadNumber)
		}
		i = 0
	}
	start := i
	buf := s.buf
	if buf[i] == '-' {
		i++
	}
scan:
	for {
		for i < len(buf) {
			c := buf[i]
			if c >= '0' && c <= '9' || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
				i++
				continue
			}
			break scan
		}
		err := s.ReadMore(start)
		i -= start
		start = 0
		if err != nil {
			if err != io.ErrUnexpectedEOF {
				s.Pos = start
				return 0, err
			}
			break
		}
		buf = s.buf
	}
	if i == start {
		s.Pos = start
		return 0, ErrBadNumber
	}
	end, gerr := skipNumber(s.buf[start:i], 0)
	if gerr != nil || end == 0 {
		s.Pos = start
		return 0, ErrBadNumber
	}
	i = start + end
	if i-start <= 16 {
		if v, ok := exactShort(s.buf[start:i]); ok {
			if f, ok := narrowExact(v); ok {
				s.Pos = i
				return f, nil
			}
		}
	}
	raw := unsafe.String(unsafe.SliceData(s.buf[start:]), i-start)
	v, err := strconv.ParseFloat(raw, 32)
	if err != nil {
		s.Pos = start
		if errors.Is(err, strconv.ErrRange) {
			return 0, ErrNumberOverflow
		}
		return 0, err
	}
	s.Pos = i
	return float32(v), nil
}

// refillSkip refills the window while skipping a value. Only skip code uses
// it. Bytes before i are consumed and not needed, so the refill drops them
// and sets i to 0. Without that, every refill in the middle of a number
// finds the buffer full, since readers fill the whole window, and doubles
// it. Callers keep the frequent `i < len(s.buf)` check inline and call this
// only when the buffer runs out. The cursor and the saved error are passed
// and returned by value. Passing the cursor's address to this call, which
// cannot be inlined, would force every digit step to go through memory.
func (s *Stream) refillSkip(i int, rerr error) (int, error, bool) {
	if rerr != nil {
		// A saved reader error ends the scan. Retrying would make new
		// blocking Reads on a reader that already failed and could hang the
		// decode, while every other value scanner returns at once.
		return i, rerr, false
	}
	err := s.ReadMore(i)
	i = 0
	if err != nil {
		// A drained reader is a valid end of the value. Any other reader
		// error must stop the skip, not be taken as "the number ended here".
		if err != io.ErrUnexpectedEOF {
			rerr = err
		}
		return i, rerr, false
	}
	return i, rerr, i < len(s.buf)
}

// NotEOF returns err unchanged unless the reader is drained. A drained
// reader becomes sentinel, the grammar error the caller reports at the real
// end of input. A reader error that loses no bytes can be retried, so
// reporting it as malformed JSON would give the wrong error. It is exported
// for generated stream decoders, whose refills in the key loop need the
// same mapping the scan methods use.
func NotEOF(err, sentinel error) error {
	if err != io.ErrUnexpectedEOF {
		return err
	}
	return sentinel
}

// orBadNumber returns the saved reader error if there is one, and
// ErrBadNumber otherwise. A refill that failed with an I/O error is not a
// malformed number.
func orBadNumber(rerr error) error {
	if rerr != nil {
		return rerr
	}
	return ErrBadNumber
}

// skipNumber is the stream version of the bytes-path [skipNumber]. It uses
// the same RFC 8259 grammar, accepts the same inputs and stops at the same
// position. refillSkip drops bytes and moves i, so every exit must write i
// back to Pos. Otherwise Offset() counts the dropped bytes twice.
func (s *Stream) skipNumber() error {
	i := s.Pos
	var rerr error // set by refillSkip on a reader error other than EOF
	var ok bool
	if i >= len(s.buf) {
		if i, rerr, ok = s.refillSkip(i, rerr); !ok {
			s.Pos = i
			return orBadNumber(rerr)
		}
	}
	if s.buf[i] == '-' {
		i++
		if i >= len(s.buf) {
			if i, rerr, ok = s.refillSkip(i, rerr); !ok {
				s.Pos = i
				return orBadNumber(rerr)
			}
		}
	}
	//nolint:gocritic // break must leave the loop, not a switch
	if s.buf[i] == '0' {
		i++
	} else if s.buf[i] >= '1' && s.buf[i] <= '9' {
		i++
		for {
			if i >= len(s.buf) {
				if i, rerr, ok = s.refillSkip(i, rerr); !ok {
					break
				}
			}
			if s.buf[i] < '0' || s.buf[i] > '9' {
				break
			}
			i++
		}
	} else {
		s.Pos = i
		return orBadNumber(rerr)
	}
	ok = i < len(s.buf)
	if !ok {
		i, rerr, ok = s.refillSkip(i, rerr)
	}
	if ok && s.buf[i] == '.' {
		i++
		if i >= len(s.buf) {
			i, rerr, ok = s.refillSkip(i, rerr)
		} else {
			ok = true
		}
		if !ok || s.buf[i] < '0' || s.buf[i] > '9' {
			s.Pos = i
			return orBadNumber(rerr)
		}
		i++
		for {
			if i >= len(s.buf) {
				if i, rerr, ok = s.refillSkip(i, rerr); !ok {
					break
				}
			}
			if s.buf[i] < '0' || s.buf[i] > '9' {
				break
			}
			i++
		}
	}
	ok = i < len(s.buf)
	if !ok {
		i, rerr, ok = s.refillSkip(i, rerr)
	}
	if ok && (s.buf[i] == 'e' || s.buf[i] == 'E') {
		i++
		if i >= len(s.buf) {
			i, rerr, ok = s.refillSkip(i, rerr)
		} else {
			ok = true
		}
		if ok && (s.buf[i] == '+' || s.buf[i] == '-') {
			i++
		}
		if i >= len(s.buf) {
			i, rerr, ok = s.refillSkip(i, rerr)
		} else {
			ok = true
		}
		if !ok || s.buf[i] < '0' || s.buf[i] > '9' {
			s.Pos = i
			return orBadNumber(rerr)
		}
		i++
		for {
			if i >= len(s.buf) {
				if i, rerr, ok = s.refillSkip(i, rerr); !ok {
					break
				}
			}
			if s.buf[i] < '0' || s.buf[i] > '9' {
				break
			}
			i++
		}
	}
	s.Pos = i
	return rerr
}

// Bool scans a true or false literal one byte at a time, refilling only when
// the buffer runs out. A wrong byte fails at once. Error positions are
// described in the loop below.
func (s *Stream) Bool() (bool, error) {
	i := s.Pos
	if i >= len(s.buf) {
		// ReadMore(i) drops everything before i, so the literal starts at 0
		// now. Set Pos before any error return reports it.
		err := s.ReadMore(i)
		i = 0
		s.Pos = i
		if err != nil {
			return false, NotEOF(err, ErrBadBool)
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
		s.Pos = i
		return false, ErrBadBool
	}
	// On failure Pos is where the scan gave up, like the bytes BoolEnd. That
	// is the wrong byte, or the end of the window when the reader drained in
	// the middle of the literal. A reader error other than EOF leaves Pos at
	// the start of the literal, so a retry scans it again. The refill only
	// grows the buffer, so the literal is still there.
	for k := range len(want) {
		pos := i + 1 + k
		if pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				if err = NotEOF(err, ErrBadBool); err == ErrBadBool {
					s.Pos = pos
				}
				return false, err
			}
		}
		if s.buf[pos] != want[k] {
			s.Pos = pos
			return false, ErrBadBool
		}
	}
	s.Pos = i + 1 + len(want)
	return first == 't', nil
}

// SkipValue skips any JSON value: a literal, number, string, array or
// object.
func (s *Stream) SkipValue() error {
	return s.skipValueDepth(0)
}

func (s *Stream) skipValueDepth(depth int) error {
	if err := s.SkipSpace(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return NotEOF(err, ErrUnexpectedEnd)
		}
		s.Pos = 0
	}
	switch s.buf[s.Pos] {
	case '"':
		return s.skipString()
	case 't':
		return s.skipLiteral("true", ErrBadBool)
	case 'f':
		return s.skipLiteral("false", ErrBadBool)
	case 'n':
		return s.skipLiteral("null", ErrBadLiteral)
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return s.skipNumber()
	case '[':
		s.Pos++
		return s.skipArray(depth + 1)
	case '{':
		s.Pos++
		return s.skipObject(depth + 1)
	}
	return ErrBadValue
}

// skipLiteral reads the fixed literal want, whose first byte is at Pos. It
// reads one byte at a time, so a wrong byte fails without reading the rest.
// bad is the error to return. Pos then marks where the scan gave up, like
// the bytes-path litEnd. That is the end of what arrived when the input is
// cut short, and the first wrong byte otherwise.
func (s *Stream) skipLiteral(want string, bad error) error {
	j := s.Pos
	for k := 1; k < len(want); k++ {
		pos := j + k
		if pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				s.Pos = pos
				return NotEOF(err, bad)
			}
		}
		if s.buf[pos] != want[k] {
			s.Pos = pos
			return bad
		}
	}
	s.Pos = j + len(want)
	return nil
}

// CaptureValue returns the raw bytes of the next JSON value as a slice of
// the Stream's buffer. The slice is valid only until the next Stream call,
// so copy it to keep it. json.Unmarshal, UnmarshalText and SetString use it
// in place and need no copy. CaptureValue gets the whole value into the
// buffer as one slice and finds its end with the bytes-path SkipValue,
// unchanged. The SIMD tiers do the same with their own SkipValue. On error,
// SkipValue returns where it gave up. A failure at a byte inside the window,
// not at its end, is final and is returned at once. Only a failure at the
// end of the window means "read more", and reading continues until EOF says
// no more bytes can fix it. A huge value grows s.buf to its size. That
// cannot be avoided, because the raw bytes must be returned as one slice.
func (s *Stream) CaptureValue() ([]byte, error) {
	start := s.Pos
	// eof means the reader drained during this capture, when ReadMore
	// returned io.ErrUnexpectedEOF. No more bytes can arrive, so the next
	// skip's result is final. It is local, since Stream keeps no reader state.
	eof := false
	for {
		end, err := SkipValue(s.buf, start)
		// Trust the end when a byte after it is buffered (end < len) or the
		// reader is drained. Otherwise a number that ends at the window edge,
		// such as "123" that may go on as "1234", could be cut short. Every
		// other value ends on its own: a closing quote or bracket, or a fixed
		// literal. So its end is final even at the edge. Waiting for one more
		// byte there would block forever on a live reader, such as a socket,
		// that has sent the whole value and has nothing more to send.
		if err == nil {
			if end < len(s.buf) || eof {
				s.Pos = end
				return s.buf[start:end], nil
			}
			if c := s.buf[SkipSpace(s.buf, start)]; c != '-' && (c < '0' || c > '9') {
				s.Pos = end
				return s.buf[start:end], nil
			}
		}
		// A final error leaves Pos where the skip gave up, the position the
		// bytes path reports for the same span, not the start of the value.
		if eof {
			s.Pos = end
			return nil, err
		}
		// A skip that failed at a byte inside the window, not at its end, is
		// final. No later byte can repair an error that comes before bytes we
		// already hold. Returning it now keeps a live reader that never sends
		// EOF from hanging the decode. Without this, a complete `{"a":}` on a
		// socket would block in Read forever, waiting for bytes that cannot
		// help. Only a failure at the end (end == len) could be cut-short
		// input, so only that keeps reading. SkipValue returns where it gave
		// up, so there is no need to scan again. ErrMaxDepth is final wherever
		// it happens, because no new byte can bring the depth back down.
		if err != nil && (end < len(s.buf) || err == ErrMaxDepth) {
			s.Pos = end
			return nil, err
		}
		// The first refill drops the bytes before the value (keep=start). That
		// frees space at the end instead of copying unused bytes on every
		// grow. start becomes 0, so later refills only grow. Do not drop bytes
		// before the first skip. The value may already be fully buffered, and
		// ReadMore always calls Read, which could block. Read once, then skip
		// again. Once these bytes might complete the value, a second Read on a
		// live reader with nothing to send yet would block. A reader that
		// fills all free space per Read, such as a file or bytes.Reader, still
		// doubles the window each time. A reader that returns small pieces
		// causes a new skip per piece. That is the cost of never blocking on a
		// complete value.
		if e := s.ReadMore(start); e != nil {
			if e != io.ErrUnexpectedEOF {
				s.Pos = 0
				return nil, e
			}
			eof = true
		}
		start = 0
	}
}

func (s *Stream) skipArray(depth int) error {
	if depth > maxDepth {
		return ErrMaxDepth
	}
	if err := s.SkipSpace(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		// Input that ends right after '[' reports ErrUnexpectedEnd, not
		// ErrBadArray. That matches the bytes skipArray, where the element
		// skipValue reports it.
		if err := s.ReadMore(s.Pos); err != nil {
			return NotEOF(err, ErrUnexpectedEnd)
		}
		s.Pos = 0
	}
	if s.buf[s.Pos] == ']' {
		s.Pos++
		return nil
	}
	for {
		if err := s.skipValueDepth(depth); err != nil {
			return err
		}
		if err := s.SkipSpace(); err != nil {
			return err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return NotEOF(err, ErrBadArray)
			}
		}
		if s.buf[s.Pos] == ',' {
			s.Pos++
			continue
		}
		if s.buf[s.Pos] == ']' {
			s.Pos++
			return nil
		}
		return ErrBadArray
	}
}

func (s *Stream) skipObject(depth int) error {
	if depth > maxDepth {
		return ErrMaxDepth
	}
	if err := s.SkipSpace(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return NotEOF(err, ErrBadObject)
		}
		s.Pos = 0
	}
	if s.buf[s.Pos] == '}' {
		s.Pos++
		return nil
	}
	for {
		if err := s.skipString(); err != nil {
			// skipString reports ErrExpectString only when the key does not
			// start with '"', or the input ends there. The bytes loop reports
			// ErrBadObject for both, so always replace it.
			if err == ErrExpectString {
				return ErrBadObject
			}
			return err
		}
		if err := s.SkipSpace(); err != nil {
			return err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return NotEOF(err, ErrBadObject)
			}
		}
		if s.buf[s.Pos] != ':' {
			return ErrBadObject
		}
		s.Pos++
		if err := s.skipValueDepth(depth); err != nil {
			return err
		}
		if err := s.SkipSpace(); err != nil {
			return err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return NotEOF(err, ErrBadObject)
			}
		}
		if s.buf[s.Pos] == ',' {
			s.Pos++
			// The top of the loop expects the next key's opening quote. Skip
			// whitespace after ',' here, as the bytes path does. Without it
			// pretty-printed input fails with ErrExpectString.
			if err := s.SkipSpace(); err != nil {
				return err
			}
			continue
		}
		if s.buf[s.Pos] == '}' {
			s.Pos++
			return nil
		}
		return ErrBadObject
	}
}

// Any is the stream version of [Any]. It maps JSON to Go values the same
// way encoding/json does by default.
func (s *Stream) Any(validate bool) (any, error) {
	return s.anyValueDepth(0, validate)
}

func (s *Stream) anyValueDepth(depth int, validate bool) (any, error) {
	if err := s.SkipSpace(); err != nil {
		return nil, err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return nil, NotEOF(err, ErrUnexpectedEnd)
		}
		s.Pos = 0
	}
	switch c := s.buf[s.Pos]; {
	case c == 'n':
		return nil, s.skipLiteral("null", ErrBadLiteral)
	case c == 't' || c == 'f':
		return s.Bool()
	case c == '"':
		return s.String(validate)
	case c == '-' || (c >= '0' && c <= '9'):
		return s.Float64()
	case c == '[':
		return s.anyArray(depth+1, validate)
	case c == '{':
		return s.anyObject(depth+1, validate)
	}
	return nil, ErrBadLiteral
}

func (s *Stream) anyArray(depth int, validate bool) ([]any, error) {
	if depth > maxDepth {
		return nil, ErrMaxDepth
	}
	s.Pos++ // skip '['
	if err := s.SkipSpace(); err != nil {
		return nil, err
	}
	if s.Pos >= len(s.buf) {
		// Input that ends right after '[' reports ErrUnexpectedEnd, like
		// the bytes anyArray, where the element anyValue reports it.
		if err := s.ReadMore(s.Pos); err != nil {
			return nil, NotEOF(err, ErrUnexpectedEnd)
		}
		s.Pos = 0
	}
	if s.buf[s.Pos] == ']' {
		s.Pos++
		return []any{}, nil
	}
	out := make([]any, 0, 4)
	for {
		v, err := s.anyValueDepth(depth, validate)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		if err := s.SkipSpace(); err != nil {
			return nil, err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(s.Pos); err != nil {
				return nil, NotEOF(err, ErrBadArray)
			}
			s.Pos = 0
		}
		if s.buf[s.Pos] == ',' {
			s.Pos++
			if err := s.SkipSpace(); err != nil {
				return nil, err
			}
			continue
		}
		if s.buf[s.Pos] == ']' {
			s.Pos++
			return out, nil
		}
		return nil, ErrBadArray
	}
}

func (s *Stream) anyObject(depth int, validate bool) (map[string]any, error) {
	if depth > maxDepth {
		return nil, ErrMaxDepth
	}
	s.Pos++ // skip '{'
	if err := s.SkipSpace(); err != nil {
		return nil, err
	}
	if s.Pos >= len(s.buf) {
		// Input that ends right after '{' reports ErrExpectString, like
		// the bytes anyObject, where the key String reports it.
		if err := s.ReadMore(s.Pos); err != nil {
			return nil, NotEOF(err, ErrExpectString)
		}
		s.Pos = 0
	}
	if s.buf[s.Pos] == '}' {
		s.Pos++
		return map[string]any{}, nil
	}
	out := make(map[string]any, 4)
	for {
		key, err := s.String(validate)
		if err != nil {
			return nil, err
		}
		if err := s.SkipSpace(); err != nil {
			return nil, err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return nil, NotEOF(err, ErrBadObject)
			}
		}
		if s.buf[s.Pos] != ':' {
			return nil, ErrBadObject
		}
		s.Pos++
		if err := s.SkipSpace(); err != nil {
			return nil, err
		}
		v, err := s.anyValueDepth(depth, validate)
		if err != nil {
			return nil, err
		}
		out[key] = v
		if err := s.SkipSpace(); err != nil {
			return nil, err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(s.Pos); err != nil {
				return nil, NotEOF(err, ErrBadObject)
			}
			s.Pos = 0
		}
		if s.buf[s.Pos] == ',' {
			s.Pos++
			if err := s.SkipSpace(); err != nil {
				return nil, err
			}
			continue
		}
		if s.buf[s.Pos] == '}' {
			s.Pos++
			return out, nil
		}
		return nil, ErrBadObject
	}
}

// Number is the stream version of [Number]. It returns a json.Number copied
// out of the buffer.
func (s *Stream) Number() (json.Number, error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			s.Pos = 0
			return "", NotEOF(err, ErrBadNumber)
		}
		i = 0
	}
	start := i
	// buf is copied to a local. Refills drop the bytes before start and shift
	// the cursors, see Float64, so a number that crosses window edges cannot
	// make the buffer grow without bound.
	buf := s.buf
	if buf[i] == '-' {
		i++
	}
scan:
	for {
		for i < len(buf) {
			c := buf[i]
			if c >= '0' && c <= '9' || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
				i++
				continue
			}
			break scan
		}
		err := s.ReadMore(start)
		i -= start
		start = 0
		if err != nil {
			// A drained reader is a valid end of the value, since the
			// digits ran to EOF. Any other reader error is not. Ignoring it
			// would return a cut-short value with a nil error. That is
			// silently wrong at the top level and a false grammar error one
			// level up.
			if err != io.ErrUnexpectedEOF {
				s.Pos = start
				return "", err
			}
			break
		}
		buf = s.buf
	}
	if i == start {
		s.Pos = i
		return "", ErrBadNumber
	}
	// The number ends where the grammar stops, to match the bytes path. See
	// Float64.
	end, gerr := skipNumber(s.buf[start:i], 0)
	if gerr != nil {
		s.Pos = start + end
		return "", ErrBadNumber
	}
	i = start + end
	s.Pos = i
	return json.Number(string(s.buf[start:i])), nil
}

// AnyNumber is [Stream.Any] but decodes JSON numbers as json.Number instead
// of float64.
func (s *Stream) AnyNumber(validate bool) (any, error) {
	return s.anyNumberValueDepth(0, validate)
}

func (s *Stream) anyNumberValueDepth(depth int, validate bool) (any, error) {
	if err := s.SkipSpace(); err != nil {
		return nil, err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return nil, NotEOF(err, ErrUnexpectedEnd)
		}
		s.Pos = 0
	}
	switch c := s.buf[s.Pos]; {
	case c == 'n':
		return nil, s.skipLiteral("null", ErrBadLiteral)
	case c == 't' || c == 'f':
		return s.Bool()
	case c == '"':
		return s.String(validate)
	case c == '-' || (c >= '0' && c <= '9'):
		return s.Number()
	case c == '[':
		return s.anyNumberArray(depth+1, validate)
	case c == '{':
		return s.anyNumberObject(depth+1, validate)
	}
	return nil, ErrBadLiteral
}

func (s *Stream) anyNumberArray(depth int, validate bool) ([]any, error) {
	if depth > maxDepth {
		return nil, ErrMaxDepth
	}
	s.Pos++
	if err := s.SkipSpace(); err != nil {
		return nil, err
	}
	if s.Pos >= len(s.buf) {
		// Input that ends right after '[' reports ErrUnexpectedEnd, like
		// the bytes anyArray, where the element anyValue reports it.
		if err := s.ReadMore(s.Pos); err != nil {
			return nil, NotEOF(err, ErrUnexpectedEnd)
		}
		s.Pos = 0
	}
	if s.buf[s.Pos] == ']' {
		s.Pos++
		return []any{}, nil
	}
	out := make([]any, 0, 4)
	for {
		v, err := s.anyNumberValueDepth(depth, validate)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		if err := s.SkipSpace(); err != nil {
			return nil, err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(s.Pos); err != nil {
				return nil, NotEOF(err, ErrBadArray)
			}
			s.Pos = 0
		}
		if s.buf[s.Pos] == ',' {
			s.Pos++
			if err := s.SkipSpace(); err != nil {
				return nil, err
			}
			continue
		}
		if s.buf[s.Pos] == ']' {
			s.Pos++
			return out, nil
		}
		return nil, ErrBadArray
	}
}

func (s *Stream) anyNumberObject(depth int, validate bool) (map[string]any, error) {
	if depth > maxDepth {
		return nil, ErrMaxDepth
	}
	s.Pos++
	if err := s.SkipSpace(); err != nil {
		return nil, err
	}
	if s.Pos >= len(s.buf) {
		// Input that ends right after '{' reports ErrExpectString, like
		// the bytes anyObject, where the key String reports it.
		if err := s.ReadMore(s.Pos); err != nil {
			return nil, NotEOF(err, ErrExpectString)
		}
		s.Pos = 0
	}
	if s.buf[s.Pos] == '}' {
		s.Pos++
		return map[string]any{}, nil
	}
	out := make(map[string]any, 4)
	for {
		key, err := s.String(validate)
		if err != nil {
			return nil, err
		}
		if err := s.SkipSpace(); err != nil {
			return nil, err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return nil, NotEOF(err, ErrBadObject)
			}
		}
		if s.buf[s.Pos] != ':' {
			return nil, ErrBadObject
		}
		s.Pos++
		if err := s.SkipSpace(); err != nil {
			return nil, err
		}
		v, err := s.anyNumberValueDepth(depth, validate)
		if err != nil {
			return nil, err
		}
		out[key] = v
		if err := s.SkipSpace(); err != nil {
			return nil, err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(s.Pos); err != nil {
				return nil, NotEOF(err, ErrBadObject)
			}
			s.Pos = 0
		}
		if s.buf[s.Pos] == ',' {
			s.Pos++
			if err := s.SkipSpace(); err != nil {
				return nil, err
			}
			continue
		}
		if s.buf[s.Pos] == '}' {
			s.Pos++
			return out, nil
		}
		return nil, ErrBadObject
	}
}

// StreamDecoder is the interface a type implements to be read from a Stream.
// Every struct ggen generates code for implements it. The bytes-path version
// is Decoder, which needs no Stream. [Stream.Value], [Stream.Slice],
// [Stream.Array] and [Stream.Seq] require it.
type StreamDecoder[T any] interface {
	DecodeFromStream(s *Stream) (T, error)
}

// Value reads one T from the Stream and leaves the cursor just after it, so
// the next read continues where this one stopped:
//
//	s := ggen.NewStream(r, buf)
//	u, err := s.Value[User]()
//
// An optional rcv is decoded into. Generated decoders start their result
// from the receiver and empty its containers while keeping their capacity
// (`clear(m)`, `sl[:0]`). So passing back a value decoded before reuses its
// maps and slices instead of allocating new ones. Only rcv[0] is read.
//
//	u, err = s.Value(u) // reuses u's containers
//
// The receiver only lends its memory. The result equals a fresh decode of the
// payload. Every field the payload omits comes back zeroed, and containers
// are emptied and refilled while keeping their capacity.
//
// The error is whatever the generated decoder returned. There is no
// enclosing array index to add to its path.
func (s *Stream) Value[T StreamDecoder[T]](rcv ...T) (T, error) {
	var into T
	if len(rcv) > 0 {
		into = rcv[0]
	}
	return into.DecodeFromStream(s)
}

// Slice reads a JSON array of T and leaves the cursor just after the closing
// bracket. Data after it is not rejected. A Stream may carry more values,
// and looking for them would block a live reader. The caller decides what
// comes next:
//
//	users, err := s.Slice[User]()
//	// s is still positioned, so keep reading
//
// An optional rcv is reused, not just appended to. The slice is cut to
// length 0 and keeps its capacity, and element i is decoded into rcv[0][i].
// So each element's own maps and slices are reused too. Decoding the same
// shape again then allocates nothing, instead of only saving the outer
// array.
//
//	users, err = s.Slice(users) // reuses the slice and each element's containers
//
// Elements past the new length keep their allocations but are not returned.
// A `[]` decodes to a non-nil empty slice, matching the bytes path and
// jsonv2. Errors carry no element index in their path.
func (s *Stream) Slice[T StreamDecoder[T]](rcv ...[]T) ([]T, error) {
	var prev []T
	if len(rcv) > 0 {
		prev = rcv[0]
	}
	if err := s.ArrayOpen(); err != nil {
		return nil, err
	}
	if err := s.SkipSpace(); err != nil {
		return nil, err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return nil, NotEOF(err, ErrBadArray)
		}
		s.Pos = 0
	}
	result := prev[:0]
	if cap(result) == 0 {
		result = make([]T, 0, prealloc.Cap(unsafe.Sizeof(*new(T))))
	}
	if s.buf[s.Pos] == ']' {
		s.Pos++
		return result, nil
	}
	for {
		// Drop consumed elements once the window is full, so the next refill
		// reuses that space instead of doubling the buffer. See [Stream.Array].
		if s.Pos > 0 && len(s.buf) == cap(s.buf) {
			s.consumed += s.Pos
			copy(s.buf, s.buf[s.Pos:])
			s.buf = s.buf[:len(s.buf)-s.Pos]
			s.Pos = 0
		}
		// Decode into the element already at this index, so its containers
		// are reused. Read it before the append that overwrites this slot,
		// because result and prev share one backing array.
		var into T
		if i := len(result); i < len(prev) {
			into = prev[i]
		}
		v, err := into.DecodeFromStream(s)
		if err != nil {
			return nil, err
		}
		result = append(result, v)
		if err := s.SkipSpace(); err != nil {
			return nil, err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(s.Pos); err != nil {
				return nil, NotEOF(err, ErrBadArray)
			}
			s.Pos = 0
		}
		switch s.buf[s.Pos] {
		case ',':
			s.Pos++
			// Skip whitespace after ','. Scalar and alias element decoders do
			// not skip leading whitespace. The bytes path does the same with
			// SkipSpace(i+1).
			if err := s.SkipSpace(); err != nil {
				return nil, err
			}
		case ']':
			s.Pos++
			return result, nil
		default:
			return nil, ErrBadArray
		}
	}
}

// Array iterates over a JSON array without collecting it, yielding one
// element at a time. It is the lazy version of [Stream.Slice], with the same
// grammar and element decoding. Nothing builds up, so a million-element
// array costs the memory of one element, not a million:
//
//	for v, err := range ggen.NewStream(r, buf).Array[Item]() {
//		if err != nil { break }
//		handle(v)
//	}
//
// The range ends when the closing bracket is read, and leaves the cursor just
// after it so the Stream can be read again. A malformed array yields one
// error and stops. Breaking out early leaves the cursor inside the array.
// There is no way to find the end of the array without reading through it.
// So a Stream left in the middle of a range is only good for closing.
//
// Like [Stream.Seq], it reuses one value for the whole loop, starting from an
// optional rcv. A long array then reaches zero allocations per element. Also
// like Seq, a yielded value is valid only until the next step. Copy anything
// kept after the loop body. Strings are owned, but maps and slices are
// reused.
func (s *Stream) Array[T StreamDecoder[T]](rcv ...T) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var into, zero T
		if len(rcv) > 0 {
			into = rcv[0]
		}
		if err := s.ArrayOpen(); err != nil {
			yield(zero, err)
			return
		}
		if err := s.SkipSpace(); err != nil {
			yield(zero, err)
			return
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(s.Pos); err != nil {
				yield(zero, NotEOF(err, ErrBadArray))
				return
			}
			s.Pos = 0
		}
		if s.buf[s.Pos] == ']' {
			s.Pos++
			return
		}
		for {
			// Drop yielded elements once the window is full, so the next refill
			// reuses that space. len == cap is exactly the condition under
			// which ReadMore grows the buffer, so this does nothing while the
			// buffer has free space at the end. Generated container decoders
			// refill without dropping bytes, so a full window is the one state
			// that doubles the buffer. The condition keeps the cost low.
			// Dropping bytes after every element would copy all the unread
			// bytes each time.
			if s.Pos > 0 && len(s.buf) == cap(s.buf) {
				s.consumed += s.Pos
				copy(s.buf, s.buf[s.Pos:])
				s.buf = s.buf[:len(s.buf)-s.Pos]
				s.Pos = 0
			}
			v, err := into.DecodeFromStream(s)
			if err != nil {
				yield(zero, err)
				return
			}
			if !yield(v, nil) {
				return
			}
			into = v
			if err := s.SkipSpace(); err != nil {
				yield(zero, err)
				return
			}
			if s.Pos >= len(s.buf) {
				if err := s.ReadMore(s.Pos); err != nil {
					yield(zero, NotEOF(err, ErrBadArray))
					return
				}
				s.Pos = 0
			}
			switch s.buf[s.Pos] {
			case ',':
				s.Pos++
				// Skip whitespace after ','. Scalar and alias element decoders
				// do not skip leading whitespace.
				if err := s.SkipSpace(); err != nil {
					yield(zero, err)
					return
				}
			case ']':
				s.Pos++
				return
			default:
				yield(zero, ErrBadArray)
				return
			}
		}
	}
}

// Seq yields top-level values from the Stream one after another, as in
// concatenated JSON or NDJSON. It keeps going as long as the reader keeps
// producing:
//
//	for v, err := range ggen.NewStream(conn, buf).Seq[Event]() {
//		if err != nil { break }
//		handle(v)
//	}
//
// The loop ends cleanly, with no error yielded, when the reader drains
// between values. So a file or a closed socket ends the range normally.
// Anything else, such as a malformed value or a reader error, is yielded
// once as the error and then ends the sequence. Breaking out of the range
// stops reading. The Stream is left where the last value ended, so it can be
// passed to another method afterwards.
//
// It never reads past a complete value before yielding it, so a quiet socket
// cannot hold back an element that has already arrived. The refill for the
// next value happens on the next step, not right after yielding this one.
//
// Seq reuses one value for the whole loop. Each decode goes into the previous
// value and reuses its maps and slices, so a long stream reaches zero
// allocations per element. An optional rcv gives that value containers the
// caller already has. Without it, the sequence starts from a zero T and
// allocates its containers on the first element.
//
// The window stays bounded. Consumed values are dropped when the window is
// full, see [Stream.Array] for why. Generated container decoders refill
// without dropping bytes, so without this they would keep growing the
// buffer over a long stream.
//
// As a result, a yielded value is only valid until the next step, because
// the next step decodes over its containers. Copy anything you keep after
// the loop body. Strings are owned, so only maps and slices are shared.
// Ranging twice over the same Seq is fine. Each run starts its own value.
func (s *Stream) Seq[T StreamDecoder[T]](rcv ...T) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var into T
		if len(rcv) > 0 {
			into = rcv[0]
		}
		for {
			if err := s.SkipSpace(); err != nil {
				var zero T
				yield(zero, err)
				return
			}
			if s.Pos >= len(s.buf) {
				// Between values with nothing buffered, read once. A drained
				// reader is the clean end of the sequence.
				if err := s.ReadMore(s.Pos); err != nil {
					if err == io.ErrUnexpectedEOF {
						return
					}
					var zero T
					yield(zero, err)
					return
				}
				s.Pos = 0
				// The new bytes may start with whitespace, such as an NDJSON
				// newline.
				continue
			}
			// Drop consumed values once the window is full.
			if s.Pos > 0 && len(s.buf) == cap(s.buf) {
				s.consumed += s.Pos
				copy(s.buf, s.buf[s.Pos:])
				s.buf = s.buf[:len(s.buf)-s.Pos]
				s.Pos = 0
			}
			v, err := into.DecodeFromStream(s)
			if !yield(v, err) || err != nil {
				return
			}
			// Carry this element's containers into the next decode.
			into = v
		}
	}
}
