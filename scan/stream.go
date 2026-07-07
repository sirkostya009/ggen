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
// slicing raw spans (RawJSON capture, json.Unmarshal fallback) snapshot
// Pos before the scan and read it again after.
//
//	var s scan.Stream
//	s.Reset(r, buf)
//
// Strings returned by Stream methods are owned copies — the buffer is
// purely a parse working area, recyclable immediately after decode.
type Stream struct {
	buf []byte
	r   io.Reader
	// Pos is the parse cursor — offset of the next byte to consume. Every
	// scan primitive reads it and writes it back. Generated code reads it
	// directly to capture raw spans.
	Pos int
	// consumed is the number of bytes discarded off the front of buf by
	// compacting ReadMore, so buf[0] sits at absolute document offset
	// consumed and the cursor's absolute offset is consumed + Pos (see
	// Offset). Pos alone is buffer-relative.
	consumed int
	// Err is the sticky reader error; once set (non-EOF) ReadMore returns
	// it without touching the reader.
	Err error
	// EOF flips true once the reader signals io.EOF.
	EOF bool
	// Shift controls in-place compaction. True (default) → ReadMore honors
	// keep > 0 and memmoves the kept suffix to offset 0; false → keep
	// treated as 0 (grow-only). Flip off for spans needing stable absolute
	// offsets (RawJSON capture, json.Unmarshal fallback), then restore.
	Shift bool
}

// Offset returns the absolute byte offset of the cursor within the full
// document — bytes already discarded by buffer compaction plus the
// buffer-relative [Stream.Pos]. Use it (vs Pos) when reporting a position
// that must stay meaningful across a long, compacting stream.
func (s *Stream) Offset() int { return s.consumed + s.Pos }

// NewStream allocates a Stream bound to r with buf as the initial
// backing slice (see Reset for buf / Shift semantics). Use Reset
// directly to recycle an existing Stream without the heap alloc.
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
// Callers MUST subtract `keep` from every cursor / index >= keep they
// hold (including [Stream.Pos] when mid-scan). String aliases into s.buf
// become invalid when keep > 0 — the bytes physically move.
//
// NEVER loops the Read call: one chunk in, return what the reader gave.
func (s *Stream) ReadMore(keep int) error {
	if !s.Shift {
		keep = 0
	}
	// Shift first, even on a subsequent Read error — callers adjust their
	// offsets unconditionally after a keep > 0 call.
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
		// Pathological (0, nil) reader — treat as EOF, don't spin.
		return io.ErrUnexpectedEOF
	}
	return nil
}

// SkipSpace advances Pos past whitespace, pulling more bytes as needed.
// Compacts in-place (consumed WS overwritten via ReadMore(Pos)), so the
// caller must hold no aliases at offsets < Pos (e.g. a prior KeyView key).
// No-shift mode (RawJSON capture) leaves Pos put.
func (s *Stream) SkipSpace() error {
	// Inlinable fast path: cursor in-bounds on a non-whitespace byte. The
	// no-temp shape is load-bearing — adding an `i := s.Pos` temp pushes it
	// over the inline budget.
	if s.Pos < len(s.buf) && s.buf[s.Pos] > ' ' {
		return nil
	}
	return s.skipSpaceSlow()
}

func (s *Stream) skipSpaceSlow() error {
	i := s.Pos
	// buf hoisted into a local so the inner loop compares against a
	// registerized len(buf); refill reloads it in the outer loop.
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
// again. Shifts in-place via SkipSpace, invalidating caller aliases at
// offsets < Pos — use only after the key is dispatched / consumed.
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
	v, owned, err := s.stringView()
	if err != nil {
		return "", err
	}
	if owned {
		// stringSlow already copied into fresh scratch — no second clone.
		return v, nil
	}
	return strings.Clone(v), nil
}

// StringView reads a JSON string value and returns it as an alias into the
// Stream's buffer — zero-copy. Escapes fall back to the copy path.
//
// USE ONLY where the caller finishes consuming the string before the next
// Stream op AND retains none of its bytes past that point. The alias is
// invalidated by the next compacting ReadMore, so a consumer that keeps a
// substring MUST use [Stream.String], which copies.
func (s *Stream) StringView() (string, error) {
	v, _, err := s.stringView()
	return v, err
}

// stringView is the shared scan behind StringView / String. owned reports
// whether v already lives in fresh scratch (stringSlow escape path) vs
// aliasing s.buf.
func (s *Stream) stringView() (v string, owned bool, err error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return "", false, err
		}
		if s.Shift {
			i = 0
		}
	}
	if s.buf[i] != '"' {
		return "", false, ErrExpectString
	}
	start := i + 1
	j := start
	for {
		rel := bytes.IndexByte(s.buf[j:], '"')
		if rel < 0 {
			if bsRel := bytes.IndexByte(s.buf[j:], '\\'); bsRel >= 0 {
				v, err := s.stringSlow(start, j+bsRel)
				return v, true, err
			}
			if hasCtrlByte(s.buf[j:]) {
				return "", false, ErrBadString
			}
			j = len(s.buf)
			err := s.ReadMore(start)
			if s.Shift {
				j -= start
				start = 0
			}
			if err != nil {
				return "", false, ErrUnterminated
			}
			continue
		}
		end := j + rel
		if bsRel := bytes.IndexByte(s.buf[j:end], '\\'); bsRel >= 0 {
			v, err := s.stringSlow(start, j+bsRel)
			return v, true, err
		}
		if hasCtrlByte(s.buf[j:end]) {
			return "", false, ErrBadString
		}
		s.Pos = end + 1
		return unsafe.String(unsafe.SliceData(s.buf[start:]), end-start), false, nil
	}
}

// KeyView reads a JSON string and returns it as an alias into the
// Stream's buffer — zero-copy on the happy path. The alias survives a
// grow-only ReadMore but is invalidated by a compacting one.
//
// USE ONLY for short-lived dispatch where the string never escapes the
// call frame (object-key matching). For values stored in the decoded
// struct use [Stream.String]. Escapes fall back to the copy path.
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
	// Scalar prelude: one pass over the first window locates the closing
	// quote while validating backslash/control — short dispatch keys finish
	// here. A longer key / escape / not-yet-buffered quote falls to the
	// IndexByte loop, which resumes at the window end (j = we).
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

// stringPreludeWindow bounds KeyView's scalar pass before it falls back to
// the IndexByte loop.
const stringPreludeWindow = 24

// skipString advances Pos past a JSON string — no copy, no decode,
// escapes validated only enough to advance. Used when the value isn't
// needed (SkipValue, skipObject key-skip).
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
		// Bound the backslash probe to the closing quote (a whole-tail probe
		// makes SkipValue quadratic on fully buffered payloads).
		bsEnd := len(s.buf)
		if rel >= 0 {
			bsEnd = j + rel
		}
		bsRel := bytes.IndexByte(s.buf[j:bsEnd], '\\')
		// Closing quote, no backslash before it — fast path.
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
		// Everything buffered is validated and being DISCARDED — full
		// compaction keeps the window at chunk size instead of growing the
		// buffer toward the skipped string's length (ReadMore no-ops the
		// keep under Shift=false, so RawJSON capture is unaffected).
		j = len(s.buf)
		err := s.ReadMore(j)
		if s.Shift {
			j = 0
			start = 0
		}
		if err != nil {
			return ErrUnterminated
		}
	}
}

// stringSlow handles escape sequences into a fresh local buffer copied
// from the already-scanned prefix. Every ReadMore in the loop passes 0
// (grow-only) so offsets stay stable across reads.
func (s *Stream) stringSlow(start, j int) (string, error) {
	if hasCtrlByte(s.buf[start:j]) {
		return "", ErrBadString
	}
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

// Int64 scans an integer with per-digit overflow detection, sign applied
// at the end. Matches scan.Int64.
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
	buf := s.buf
	// digits counts unchecked-prefix consumption across refills. First ≤18
	// digits can't overflow int64 (see bytes-path Int64); the checked loop
	// only ever consumes once digits hits 18 — an unchecked exit below 18
	// means the buffered bytes ran out, so the checked loop no-ops into the
	// refill.
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
		// No-shift mode moves no bytes — resetting i would re-read digits.
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
	buf := s.buf
	// First ≤19 digits can't overflow uint64 — see Int64's prefix comment.
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
	// buf hoisted into a local; ReadMore(0) is grow-only so the span stays stable.
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

// refillSkip refills the window mid-skip. Skip-path exclusive: bytes
// before *i are consumed and discardable, so the refill compacts from *i
// (ReadMore no-ops the keep under Shift=false) and rebases *i — without
// compaction every mid-number refill lands with len == cap (readers fill
// the whole window) and doubles the buffer. Callers keep the hot
// `i < len(s.buf)` bounds check inline and call this only on exhaustion.
func (s *Stream) refillSkip(i *int) bool {
	err := s.ReadMore(*i)
	if s.Shift {
		*i = 0
	}
	return err == nil && *i < len(s.buf)
}

// skipNumber is the stream mirror of the bytes-path [skipNumber] — same
// RFC 8259 grammar, same accept-set.
func (s *Stream) skipNumber() error {
	i := s.Pos
	if i >= len(s.buf) && !s.refillSkip(&i) {
		return ErrBadNumber
	}
	if s.buf[i] == '-' {
		i++
		if i >= len(s.buf) && !s.refillSkip(&i) {
			return ErrBadNumber
		}
	}
	if s.buf[i] == '0' {
		i++
	} else if s.buf[i] >= '1' && s.buf[i] <= '9' {
		i++
		for {
			if i >= len(s.buf) && !s.refillSkip(&i) {
				break
			}
			if s.buf[i] < '0' || s.buf[i] > '9' {
				break
			}
			i++
		}
	} else {
		return ErrBadNumber
	}
	if (i < len(s.buf) || s.refillSkip(&i)) && s.buf[i] == '.' {
		i++
		if (i >= len(s.buf) && !s.refillSkip(&i)) || s.buf[i] < '0' || s.buf[i] > '9' {
			return ErrBadNumber
		}
		i++
		for {
			if i >= len(s.buf) && !s.refillSkip(&i) {
				break
			}
			if s.buf[i] < '0' || s.buf[i] > '9' {
				break
			}
			i++
		}
	}
	if (i < len(s.buf) || s.refillSkip(&i)) && (s.buf[i] == 'e' || s.buf[i] == 'E') {
		i++
		if (i < len(s.buf) || s.refillSkip(&i)) && (s.buf[i] == '+' || s.buf[i] == '-') {
			i++
		}
		if (i >= len(s.buf) && !s.refillSkip(&i)) || s.buf[i] < '0' || s.buf[i] > '9' {
			return ErrBadNumber
		}
		i++
		for {
			if i >= len(s.buf) && !s.refillSkip(&i) {
				break
			}
			if s.buf[i] < '0' || s.buf[i] > '9' {
				break
			}
			i++
		}
	}
	s.Pos = i
	return nil
}

// Bool scans a true/false literal byte-by-byte, bounds-checking each char
// and refilling only when exhausted. Mismatch fails fast.
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
		if err := s.ReadMore(s.Pos); err != nil {
			return ErrUnexpectedEnd
		}
		if s.Shift {
			s.Pos = 0
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
		return s.skipNumber()
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
		if err := s.ReadMore(s.Pos); err != nil {
			return ErrBadArray
		}
		if s.Shift {
			s.Pos = 0
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
		if err := s.ReadMore(s.Pos); err != nil {
			return ErrBadObject
		}
		if s.Shift {
			s.Pos = 0
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
			// The loop head expects the next key's opening quote directly —
			// skip separator whitespace here (the bytes-path mirror does the
			// same); without it pretty-printed input fails ErrExpectString.
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

// Number is the stream counterpart of [Number] — returns json.Number
// copied out of the buffer.
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
	// buf hoisted into a local; ReadMore(0) is grow-only so the span stays stable.
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
