package scan

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

// Stream wraps an io.Reader. The cursor lives in [Stream.Pos] — every
// scan primitive consumes from there and updates it on return. Callers
// that need to slice raw spans (RawJSON capture, json.Unmarshal
// fallback) snapshot Pos before the scan and read it again after.
//
// When a primitive's bounds check fails, it calls ReadMore(0) to pull
// a single chunk without shifting — Pos and any in-flight local
// cursors stay valid. Internal methods that hold a body span (String,
// Number) pass ReadMore(spanStart > 0) so the buffer stays bounded
// across long streams; they reset their local cursors and Pos after
// the shift.
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
	// Pos is the current parse cursor — the offset of the next byte to
	// consume. Every scan primitive reads from Pos and writes it back
	// before returning. Generated code reads Pos directly to capture
	// raw spans (e.g. `start := s.Pos; s.SkipValue(); raw := s.Bytes()[start:s.Pos]`).
	Pos int
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
	// fallback against s.Bytes()[start:s.Pos] — and restore the previous
	// value after.
	Shift bool
}

// NewStream allocates a Stream bound to r with buf as the initial
// backing slice. See Reset for the buf / Shift semantics. Equivalent
// to `var s Stream; s.Reset(r, buf); return &s`; use Reset directly
// to recycle an existing Stream (stack-allocatable, no heap alloc).
func NewStream(r io.Reader, buf []byte) *Stream {
	s := &Stream{}
	s.Reset(r, buf)
	return s
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
// that was previously >= keep (including [Stream.Pos] when the
// caller is mid-scan). String aliases into s.buf become invalid
// whenever keep > 0 — the bytes physically move (in-place memmove
// on the same backing) and the alias points at the wrong content
// afterwards.
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

// SkipSpace advances Pos past whitespace, pulling more bytes as
// needed. Compacts in-place: the consumed whitespace is overwritten
// by the next chunk via ReadMore(Pos). Caller must hold no aliases
// at offsets < Pos (a key alias from a prior KeyView is the obvious
// one — caller must either copy it before SkipSpace or treat it as
// discardable). When the Stream is in no-shift mode (RawJSON
// capture), ReadMore behaves as grow-only and Pos stays where it
// was.
func (s *Stream) SkipSpace() error {
	// Two-tier: the dominant compact-JSON case (cursor in-bounds, current
	// byte non-whitespace) returns inline. Everything else — whitespace to
	// skip, control bytes, refill at EOF — falls to skipSpaceSlow. The exact
	// no-temp shape is load-bearing: it inlines at cost 77 (budget 80); an
	// `i := s.Pos` temp variant costs 81 and does NOT inline.
	if s.Pos < len(s.buf) && s.buf[s.Pos] > ' ' {
		return nil
	}
	return s.skipSpaceSlow()
}

func (s *Stream) skipSpaceSlow() error {
	i := s.Pos
	// Hoist the buffer header into a local so the inner loop compares
	// against a registerized len(buf) instead of reloading s.buf through
	// the pointer every byte. Refill happens in the outer loop, after
	// which buf is reloaded.
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
			if err == io.ErrUnexpectedEOF {
				s.Pos = i
				return nil
			}
			s.Pos = i
			return err
		}
		if s.Shift {
			i = 0
		}
		buf = s.buf
	}
}

// ConsumeColon skips whitespace, consumes ':', then skips whitespace
// again — the canonical post-key key/value separator. Shifts in-place
// via the underlying SkipSpace calls, so any aliases the caller holds
// at offsets < Pos become invalid. Use this only after the key has
// been dispatched / consumed.
func (s *Stream) ConsumeColon() error {
	if err := s.SkipSpace(); err != nil {
		return err
	}
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return err
		}
		if s.Shift {
			i = 0
		}
	}
	if s.buf[i] != ':' {
		return ErrBadObject
	}
	s.Pos = i + 1
	return s.SkipSpace()
}

// ObjectOpen skips whitespace then consumes '{'.
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

// ArrayOpen skips whitespace then consumes '['.
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

// String decodes a JSON string. Always copies the body out of the
// buffer — the result is owned, no dependency on the buffer.
func (s *Stream) String() (string, error) {
	// String returns an OWNED copy — the buffer is a recyclable working
	// area. StringView does the scan and aliases the span; clone it so the
	// result outlives the buffer. (stringSlow already returns an owned copy
	// on the escape path; cloning it again is a rare extra copy.)
	v, err := s.StringView()
	if err != nil {
		return "", err
	}
	return strings.Clone(v), nil
}

// StringView reads a JSON string value and returns it as an alias into
// the Stream's buffer — zero-copy, like [Stream.KeyView] but without the
// short-key scalar prelude (value strings are often long, e.g. base64,
// where the SIMD IndexByte path wins). On escapes it falls back to the
// copy path (stringSlow), since aliasing only holds when the source
// bytes ARE the final bytes.
//
// USE ONLY where the caller finishes consuming the string before the
// next Stream operation AND retains none of its bytes past that point —
// e.g. decoding a base64 []byte (decoded bytes land in an independent
// slice), or parsing into a number/time/IP value type. The alias is
// invalidated by the next compacting ReadMore and the bytes are recycled
// as the Stream advances, so a consumer that keeps a substring of the
// input (url.URL slices its Path/RawQuery out of the source) MUST use
// [Stream.String], which copies. A parse error halts decoding, so an
// error value that retains the string never sees a subsequent buffer
// write.
func (s *Stream) StringView() (string, error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return "", err
		}
		if s.Shift {
			i = 0
		}
	}
	if s.buf[i] != '"' {
		return "", ErrExpectString
	}
	start := i + 1
	j := start
	for {
		rel := bytes.IndexByte(s.buf[j:], '"')
		if rel < 0 {
			if bsRel := bytes.IndexByte(s.buf[j:], '\\'); bsRel >= 0 {
				return s.stringSlow(start, j+bsRel)
			}
			if hasCtrlByte(s.buf[j:]) {
				return "", ErrBadString
			}
			j = len(s.buf)
			err := s.ReadMore(start)
			if s.Shift {
				j -= start
				start = 0
			}
			if err != nil {
				return "", ErrUnterminated
			}
			continue
		}
		end := j + rel
		if bsRel := bytes.IndexByte(s.buf[j:end], '\\'); bsRel >= 0 {
			return s.stringSlow(start, j+bsRel)
		}
		if hasCtrlByte(s.buf[j:end]) {
			return "", ErrBadString
		}
		s.Pos = end + 1
		return unsafe.String(unsafe.SliceData(s.buf[start:]), end-start), nil
	}
}

// KeyView reads a JSON string and returns it as an alias into the
// Stream's buffer — zero-copy, zero-allocation on the happy path.
// The returned string remains valid even after subsequent ReadMore
// grows the buf, because Go's GC keeps the underlying backing alive
// as long as any string aliases into it.
//
// USE ONLY for short-lived dispatch where the string never escapes
// the call frame — e.g. object-key matching in `switch key`
// dispatch. For values that go into the decoded struct,
// use [Stream.String] (which copies).
//
// On escape sequences in the key, falls back to the copy path
// (stringSlow) — aliasing only works when the source bytes ARE the
// final string bytes.
func (s *Stream) KeyView() (string, error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return "", err
		}
		if s.Shift {
			i = 0
		}
	}
	if s.buf[i] != '"' {
		return "", ErrExpectString
	}
	start := i + 1
	// Scalar prelude: dispatch keys are short (a few bytes), so one pass
	// locates the closing quote while validating backslash/control — sparing
	// the two bytes.IndexByte call setups that lose to a tight scalar loop on
	// short spans. A key longer than the window, an escape, or a quote not yet
	// buffered falls through to the IndexByte loop below, which RESUMES at the
	// window end (j = we) so the prelude-validated prefix is never re-scanned.
	// Applied to KeyView (short keys) but not Stream.String — value strings
	// are often long (base64), where the SIMD IndexByte path wins.
	we := min(start+stringPreludeWindow, len(s.buf))
	for k := start; k < we; k++ {
		c := s.buf[k]
		if c == '"' {
			s.Pos = k + 1
			return unsafe.String(unsafe.SliceData(s.buf[start:]), k-start), nil
		}
		if c == '\\' {
			return s.stringSlow(start, k)
		}
		if c < 0x20 {
			return "", ErrBadString
		}
	}
	j := we
	for {
		rel := bytes.IndexByte(s.buf[j:], '"')
		if rel < 0 {
			if bsRel := bytes.IndexByte(s.buf[j:], '\\'); bsRel >= 0 {
				return s.stringSlow(start, j+bsRel)
			}
			if hasCtrlByte(s.buf[j:]) {
				return "", ErrBadString
			}
			j = len(s.buf)
			err := s.ReadMore(start)
			if s.Shift {
				j -= start
				start = 0
			}
			if err != nil {
				return "", ErrUnterminated
			}
			continue
		}
		end := j + rel
		if bsRel := bytes.IndexByte(s.buf[j:end], '\\'); bsRel >= 0 {
			return s.stringSlow(start, j+bsRel)
		}
		if hasCtrlByte(s.buf[j:end]) {
			return "", ErrBadString
		}
		s.Pos = end + 1
		return unsafe.String(unsafe.SliceData(s.buf[start:]), end-start), nil
	}
}

// stringPreludeWindow bounds the scalar single-pass scan in KeyView before
// it falls back to the bytes.IndexByte loop. Sized to cover typical object
// keys in one pass without pre-scanning long spans the SIMD path handles
// better.
const stringPreludeWindow = 24

// skipString advances Pos past a JSON string. No copy, no body
// decode — escapes are validated only enough to advance correctly
// (`\uXXXX` consumes 6, other escapes 2). Use when the caller doesn't
// need the string value (SkipValue, skipObject key-skip).
func (s *Stream) skipString() error {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return err
		}
		if s.Shift {
			i = 0
		}
	}
	if s.buf[i] != '"' {
		return ErrExpectString
	}
	start := i + 1
	j := start
	for {
		rel := bytes.IndexByte(s.buf[j:], '"')
		// Bound the backslash probe to the closing quote — whole-tail
		// scans per skipped string made SkipValue quadratic on fully
		// buffered payloads (Shift=false RawJSON capture).
		bsEnd := len(s.buf)
		if rel >= 0 {
			bsEnd = j + rel
		}
		bsRel := bytes.IndexByte(s.buf[j:bsEnd], '\\')
		// Closing quote with no backslash before it — fast path.
		if rel >= 0 && bsRel < 0 {
			end := j + rel
			if hasCtrlByte(s.buf[j:end]) {
				return ErrBadString
			}
			s.Pos = end + 1
			return nil
		}
		// Backslash before any closing quote — slow path: validate
		// literal bytes up to the backslash, then handle the escape.
		if bsRel >= 0 {
			bs := j + bsRel
			if hasCtrlByte(s.buf[j:bs]) {
				return ErrBadString
			}
			// Need at least one byte past the backslash for the escape kind.
			if bs+1 >= len(s.buf) {
				if err := s.ReadMore(bs); err != nil {
					return ErrBadString
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
						return ErrBadString
					}
					if s.Shift {
						j -= bs
						start = 0
						bs = 0
					}
				}
				if _, ok := parseHex4(s.buf[bs+2 : bs+6]); !ok {
					return ErrBadString
				}
				j = bs + 6
			default:
				return ErrBadString
			}
			continue
		}
		// Neither quote nor backslash in current buffer — validate
		// what's there, then read more.
		if hasCtrlByte(s.buf[j:]) {
			return ErrBadString
		}
		j = len(s.buf)
		err := s.ReadMore(start)
		if s.Shift {
			j -= start
			start = 0
		}
		if err != nil {
			return ErrUnterminated
		}
	}
}

// stringSlow handles escape sequences. Builds a fresh local buffer
// (`buf`) and copies the already-scanned prefix into it, so once we
// enter the slow path the bytes in s.buf at offsets < j are no longer
// needed — every ReadMore inside the loop passes 0 (grow-only) so
// offsets stay stable across reads.
func (s *Stream) stringSlow(start, j int) (string, error) {
	buf := make([]byte, 0, 32)
	buf = append(buf, s.buf[start:j]...)
	for {
		if j >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return "", ErrUnterminated
			}
		}
		c := s.buf[j]
		if c == '"' {
			s.Pos = j + 1
			// buf is a function-local write-once scratch — aliasing it is
			// an owned copy, same contract as Stream.String's copies.
			return unsafe.String(unsafe.SliceData(buf), len(buf)), nil
		}
		if c == '\\' {
			if j+1 >= len(s.buf) {
				if err := s.ReadMore(0); err != nil {
					return "", ErrBadString
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
							return "", ErrBadString
						}
					}
				}
				r, ok := parseHex4(s.buf[j+2 : j+6])
				if !ok {
					return "", ErrBadString
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
				return "", ErrBadString
			}
			continue
		}
		if c < 0x20 {
			return "", ErrBadString
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
func (s *Stream) Int64() (int64, error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return 0, err
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
				return 0, err
			}
			if s.Shift {
				i = 0
			}
		}
	}
	if s.buf[i] < '0' || s.buf[i] > '9' {
		return 0, ErrBadNumber
	}
	limit := uint64(math.MaxInt64)
	if neg {
		limit = SignedNeg
	}
	var u uint64
	// buf hoisted into a local (see skipSpaceSlow): the digit loop runs
	// against a registerized len(buf); refill reloads buf in the outer loop.
	buf := s.buf
scan:
	for {
		for i < len(buf) {
			c := buf[i]
			if c < '0' || c > '9' {
				if c == '.' || c == 'e' || c == 'E' {
					return 0, ErrBadNumber
				}
				break scan
			}
			d := uint64(c - '0')
			if u > limit/10 || (u == limit/10 && d > limit%10) {
				return 0, ErrNumberOverflow
			}
			u = u*10 + d
			i++
		}
		err := s.ReadMore(i)
		// ReadMore shifts only when s.Shift — in no-shift mode the
		// bytes stay put and resetting i would re-read consumed digits.
		if s.Shift {
			i = 0
		}
		if err != nil {
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

// Uint64 scans an unsigned integer with overflow detection. Returns
// ErrNumberOverflow when the magnitude exceeds MaxUint64.
func (s *Stream) Uint64() (uint64, error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return 0, err
		}
		if s.Shift {
			i = 0
		}
	}
	if s.buf[i] < '0' || s.buf[i] > '9' {
		return 0, ErrBadNumber
	}
	var n uint64
	// buf hoisted into a local (see skipSpaceSlow).
	buf := s.buf
scan:
	for {
		for i < len(buf) {
			c := buf[i]
			if c < '0' || c > '9' {
				break scan
			}
			d := uint64(c - '0')
			if n > Uint64Limit/10 || (n == Uint64Limit/10 && d > Uint64Limit%10) {
				return 0, ErrNumberOverflow
			}
			n = n*10 + d
			i++
		}
		err := s.ReadMore(i)
		// See Int64: no-shift refills move no bytes; keep the cursor.
		if s.Shift {
			i = 0
		}
		if err != nil {
			break
		}
		buf = s.buf
	}
	s.Pos = i
	return n, nil
}

// Float64 scans a JSON number span then delegates to strconv.ParseFloat.
func (s *Stream) Float64() (float64, error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(0); err != nil {
			return 0, err
		}
	}
	start := i
	// buf hoisted into a local (see skipSpaceSlow); ReadMore(0) is grow-only
	// so the cursor never resets and the span stays stable.
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
		if err := s.ReadMore(0); err != nil {
			break
		}
		buf = s.buf
	}
	if i == start {
		return 0, ErrBadNumber
	}
	raw := unsafe.String(unsafe.SliceData(s.buf[start:]), i-start)
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, err
	}
	s.Pos = i
	return v, nil
}

// Bool scans a true/false literal byte-by-byte: each char is
// bounds-checked individually and one ReadMore is issued only when
// the buffer is exhausted at that position. Mismatch fails fast
// without fetching the remaining chars. The first byte is captured
// up-front so later compactions can discard s.buf[i] safely.
func (s *Stream) Bool() (bool, error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return false, err
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
		return false, ErrBadBool
	}
	for k := 0; k < len(want); k++ {
		pos := i + 1 + k
		if pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return false, ErrBadBool
			}
		}
		if s.buf[pos] != want[k] {
			return false, ErrBadBool
		}
	}
	s.Pos = i + 1 + len(want)
	return first == 't', nil
}

// SkipValue skips an arbitrary JSON value (literal/number/string/array/object).
func (s *Stream) SkipValue() error {
	if err := s.SkipSpace(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(0); err != nil {
			return ErrUnexpectedEnd
		}
	}
	switch s.buf[s.Pos] {
	case '"':
		return s.skipString()
	case 't', 'f':
		_, err := s.Bool()
		return err
	case 'n':
		j := s.Pos
		for k := range 3 {
			pos := j + 1 + k
			if pos >= len(s.buf) {
				if err := s.ReadMore(0); err != nil {
					return ErrBadLiteral
				}
			}
			if s.buf[pos] != "ull"[k] {
				return ErrBadLiteral
			}
		}
		s.Pos = j + 4
		return nil
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		_, err := s.Float64()
		return err
	case '[':
		s.Pos++
		return s.skipArray()
	case '{':
		s.Pos++
		return s.skipObject()
	}
	return ErrBadValue
}

func (s *Stream) skipArray() error {
	if err := s.SkipSpace(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(0); err != nil {
			return ErrBadArray
		}
	}
	if s.buf[s.Pos] == ']' {
		s.Pos++
		return nil
	}
	for {
		if err := s.SkipValue(); err != nil {
			return err
		}
		if err := s.SkipSpace(); err != nil {
			return err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return ErrBadArray
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

func (s *Stream) skipObject() error {
	if err := s.SkipSpace(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(0); err != nil {
			return ErrBadObject
		}
	}
	if s.buf[s.Pos] == '}' {
		s.Pos++
		return nil
	}
	for {
		if err := s.skipString(); err != nil {
			return err
		}
		if err := s.SkipSpace(); err != nil {
			return err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return ErrBadObject
			}
		}
		if s.buf[s.Pos] != ':' {
			return ErrBadObject
		}
		s.Pos++
		if err := s.SkipValue(); err != nil {
			return err
		}
		if err := s.SkipSpace(); err != nil {
			return err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return ErrBadObject
			}
		}
		if s.buf[s.Pos] == ',' {
			s.Pos++
			continue
		}
		if s.buf[s.Pos] == '}' {
			s.Pos++
			return nil
		}
		return ErrBadObject
	}
}

// Any is the stream-buffered counterpart of [Any]. Mirrors stdlib
// encoding/json defaults for the JSON-to-any mapping.
func (s *Stream) Any() (any, error) {
	if err := s.SkipSpace(); err != nil {
		return nil, err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return nil, err
		}
		if s.Shift {
			s.Pos = 0
		}
	}
	switch c := s.buf[s.Pos]; {
	case c == 'n':
		j := s.Pos
		for k := range 3 {
			pos := j + 1 + k
			if pos >= len(s.buf) {
				if err := s.ReadMore(0); err != nil {
					return nil, err
				}
			}
			if s.buf[pos] != "ull"[k] {
				return nil, ErrBadLiteral
			}
		}
		s.Pos = j + 4
		return nil, nil
	case c == 't' || c == 'f':
		return s.Bool()
	case c == '"':
		return s.String()
	case c == '-' || (c >= '0' && c <= '9'):
		return s.Float64()
	case c == '[':
		return s.anyArray()
	case c == '{':
		return s.anyObject()
	}
	return nil, ErrBadLiteral
}

func (s *Stream) anyArray() ([]any, error) {
	s.Pos++ // consume '['
	if err := s.SkipSpace(); err != nil {
		return nil, err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return nil, err
		}
		if s.Shift {
			s.Pos = 0
		}
	}
	if s.buf[s.Pos] == ']' {
		s.Pos++
		return []any{}, nil
	}
	out := make([]any, 0, 4)
	for {
		v, err := s.Any()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		if err := s.SkipSpace(); err != nil {
			return nil, err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(s.Pos); err != nil {
				return nil, err
			}
			if s.Shift {
				s.Pos = 0
			}
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

func (s *Stream) anyObject() (map[string]any, error) {
	s.Pos++ // consume '{'
	if err := s.SkipSpace(); err != nil {
		return nil, err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return nil, err
		}
		if s.Shift {
			s.Pos = 0
		}
	}
	if s.buf[s.Pos] == '}' {
		s.Pos++
		return map[string]any{}, nil
	}
	out := make(map[string]any, 4)
	for {
		key, err := s.String()
		if err != nil {
			return nil, err
		}
		if err := s.SkipSpace(); err != nil {
			return nil, err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return nil, err
			}
		}
		if s.buf[s.Pos] != ':' {
			return nil, ErrBadObject
		}
		s.Pos++
		if err := s.SkipSpace(); err != nil {
			return nil, err
		}
		v, err := s.Any()
		if err != nil {
			return nil, err
		}
		out[key] = v
		if err := s.SkipSpace(); err != nil {
			return nil, err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(s.Pos); err != nil {
				return nil, err
			}
			if s.Shift {
				s.Pos = 0
			}
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

// Number is the stream-buffered counterpart of [Number]: scans a JSON
// number span and returns it as json.Number copied out of the buffer.
func (s *Stream) Number() (json.Number, error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return "", err
		}
		if s.Shift {
			i = 0
		}
	}
	start := i
	// buf hoisted into a local (see skipSpaceSlow); ReadMore(0) is grow-only
	// so the cursor never resets and the span stays stable.
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
		if err := s.ReadMore(0); err != nil {
			break
		}
		buf = s.buf
	}
	if i == start || (i == start+1 && s.buf[start] == '-') {
		return "", ErrBadNumber
	}
	s.Pos = i
	return json.Number(string(s.buf[start:i])), nil
}

// AnyNumber is the [Stream.Any] variant that decodes JSON numbers as
// json.Number instead of float64.
func (s *Stream) AnyNumber() (any, error) {
	if err := s.SkipSpace(); err != nil {
		return nil, err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return nil, err
		}
		if s.Shift {
			s.Pos = 0
		}
	}
	switch c := s.buf[s.Pos]; {
	case c == 'n':
		j := s.Pos
		for k := range 3 {
			pos := j + 1 + k
			if pos >= len(s.buf) {
				if err := s.ReadMore(0); err != nil {
					return nil, err
				}
			}
			if s.buf[pos] != "ull"[k] {
				return nil, ErrBadLiteral
			}
		}
		s.Pos = j + 4
		return nil, nil
	case c == 't' || c == 'f':
		return s.Bool()
	case c == '"':
		return s.String()
	case c == '-' || (c >= '0' && c <= '9'):
		return s.Number()
	case c == '[':
		return s.anyNumberArray()
	case c == '{':
		return s.anyNumberObject()
	}
	return nil, ErrBadLiteral
}

func (s *Stream) anyNumberArray() ([]any, error) {
	s.Pos++
	if err := s.SkipSpace(); err != nil {
		return nil, err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return nil, err
		}
		if s.Shift {
			s.Pos = 0
		}
	}
	if s.buf[s.Pos] == ']' {
		s.Pos++
		return []any{}, nil
	}
	out := make([]any, 0, 4)
	for {
		v, err := s.AnyNumber()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		if err := s.SkipSpace(); err != nil {
			return nil, err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(s.Pos); err != nil {
				return nil, err
			}
			if s.Shift {
				s.Pos = 0
			}
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

func (s *Stream) anyNumberObject() (map[string]any, error) {
	s.Pos++
	if err := s.SkipSpace(); err != nil {
		return nil, err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return nil, err
		}
		if s.Shift {
			s.Pos = 0
		}
	}
	if s.buf[s.Pos] == '}' {
		s.Pos++
		return map[string]any{}, nil
	}
	out := make(map[string]any, 4)
	for {
		key, err := s.String()
		if err != nil {
			return nil, err
		}
		if err := s.SkipSpace(); err != nil {
			return nil, err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return nil, err
			}
		}
		if s.buf[s.Pos] != ':' {
			return nil, ErrBadObject
		}
		s.Pos++
		if err := s.SkipSpace(); err != nil {
			return nil, err
		}
		v, err := s.AnyNumber()
		if err != nil {
			return nil, err
		}
		out[key] = v
		if err := s.SkipSpace(); err != nil {
			return nil, err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(s.Pos); err != nil {
				return nil, err
			}
			if s.Shift {
				s.Pos = 0
			}
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
