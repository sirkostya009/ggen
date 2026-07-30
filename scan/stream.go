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
// scan primitive consumes from there and updates it on return. Raw spans
// (RawJSON capture, json.Unmarshal fallback) come from [Stream.CaptureValue].
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
}

// Offset returns the absolute byte offset of the cursor within the full
// document — bytes already discarded by buffer compaction plus the
// buffer-relative [Stream.Pos]. Use it (vs Pos) when reporting a position
// that must stay meaningful across a long, compacting stream.
func (s *Stream) Offset() int { return s.consumed + s.Pos }

// NewStream allocates a Stream bound to r with buf as the initial
// backing slice (see Reset for buf semantics). Use Reset directly to
// recycle an existing Stream without the heap alloc.
func NewStream(r io.Reader, buf []byte) *Stream {
	s := &Stream{}
	s.Reset(r, buf)
	return s
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
	// Compact first, even on a subsequent Read error — callers adjust their
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
	if keep == 0 && cap(s.buf) == len(s.buf) {
		bigger := make([]byte, len(s.buf), max(cap(s.buf)*2, 1024))
		copy(bigger, s.buf)
		s.buf = bigger
	}
	n, err := s.r.Read(s.buf[len(s.buf):cap(s.buf)])
	s.buf = s.buf[:len(s.buf)+n]
	if n == 0 {
		// No progress is terminal for this call: drained (io.EOF), a real
		// error, or a pathological (0, nil) reader — don't spin.
		if err == nil || err == io.EOF {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	if err != nil && err != io.EOF {
		return err
	}
	return nil
}

// SkipSpace advances Pos past whitespace, pulling more bytes as needed.
// Compacts in-place (consumed WS overwritten via ReadMore(Pos)), so the
// caller must hold no aliases at offsets < Pos (e.g. a prior KeyView key).
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
		i = 0
		buf = s.buf
	}
}

// ConsumeColon skips whitespace, consumes ':', then skips whitespace
// again. Compacts in-place via SkipSpace, invalidating caller aliases at
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
		i = 0
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
// buffer — the result is owned, no dependency on the buffer. validate as in
// scan.String (false = allowinvalidutf8 opt-out).
func (s *Stream) String(validate bool) (string, error) {
	v, owned, err := s.stringView(validate)
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
func (s *Stream) StringView(validate bool) (string, error) {
	v, _, err := s.stringView(validate)
	return v, err
}

// stringView is the shared scan behind StringView / String. owned reports
// whether v already lives in fresh scratch (stringSlow escape path) vs
// aliasing s.buf.
func (s *Stream) stringView(validate bool) (v string, owned bool, err error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return "", false, err
		}
		i = 0
	}
	if s.buf[i] != '"' {
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
				return "", false, ErrBadString
			}
			sawHigh = sawHigh || hi
			j = len(s.buf)
			err := s.ReadMore(start)
			j -= start
			start = 0
			if err != nil {
				return "", false, ErrUnterminated
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
			return "", false, ErrBadString
		}
		// Full-span check — a rune may straddle the chunk cursor j, so per-chunk
		// validation would false-error; ReadMore(start) keeps the span contiguous.
		if validate && (sawHigh || hi) && !utf8.Valid(s.buf[start:end]) {
			return "", false, ErrInvalidUTF8
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
func (s *Stream) KeyView(validate bool) (string, error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return "", err
		}
		i = 0
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
	var hb byte
	for k := start; k < we; k++ {
		c := s.buf[k]
		if c == '"' {
			if validate && hb&0x80 != 0 && !utf8.Valid(s.buf[start:k]) {
				return "", ErrInvalidUTF8
			}
			s.Pos = k + 1
			return unsafe.String(unsafe.SliceData(s.buf[start:]), k-start), nil
		}
		if c == '\\' {
			return s.stringSlow(start, k, validate)
		}
		if c < 0x20 {
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
				return "", ErrBadString
			}
			sawHigh = sawHigh || hi
			j = len(s.buf)
			err := s.ReadMore(start)
			j -= start
			start = 0
			if err != nil {
				return "", ErrUnterminated
			}
			continue
		}
		end := j + rel
		if bsRel := bytes.IndexByte(s.buf[j:end], '\\'); bsRel >= 0 {
			return s.stringSlow(start, j+bsRel, validate)
		}
		bad, hi := ctrlOrHigh(s.buf[j:end])
		if bad {
			return "", ErrBadString
		}
		// Full-span check — see stringView (rune may straddle j).
		if validate && (sawHigh || hi) && !utf8.Valid(s.buf[start:end]) {
			return "", ErrInvalidUTF8
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
		i = 0
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
				j -= bs
				start = 0
				bs = 0
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
					j -= bs
					start = 0
					bs = 0
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
		// buffer toward the skipped string's length.
		j = len(s.buf)
		err := s.ReadMore(j)
		j = 0
		start = 0
		if err != nil {
			return ErrUnterminated
		}
	}
}

// stringSlow handles escape sequences into a fresh owned scratch buffer,
// seeded from the already-scanned prefix. The window compacts on refill
// (see loop comment); the result aliases the scratch, never s.buf.
func (s *Stream) stringSlow(start, j int, validate bool) (string, error) {
	bad, rawHigh := ctrlOrHigh(s.buf[start:j])
	if bad {
		return "", ErrBadString
	}
	buf := make([]byte, 0, 32)
	buf = append(buf, s.buf[start:j]...)
	var rawHi byte
	// `start` is dead now — the decoded output lives in the owned scratch `buf`,
	// so s.buf can COMPACT (discard consumed bytes) on refill instead of growing
	// to hold the whole raw string. Every ReadMore keeps from the cursor j and
	// rebases it, exactly like Int64/Float64/Number, so a multi-MB escaped string
	// streams through a bounded buffer. Multi-byte reads (\X, \uXXXX, surrogate
	// pair) buffer their whole span via a compacting ensure-loop before indexing
	// s.buf[j+k].
	for {
		if j >= len(s.buf) {
			err := s.ReadMore(j)
			j = 0
			if err != nil {
				return "", ErrUnterminated
			}
		}
		c := s.buf[j]
		if c == '"' {
			// Escape outputs are valid encodings by construction (surrogates
			// error below), so only RAW bytes copied through can make buf
			// invalid — skip the walk when they were all ASCII. buf is the
			// assembled output, so refill boundaries can't split a rune here.
			if validate && (rawHigh || rawHi&0x80 != 0) && !utf8.Valid(buf) {
				return "", ErrInvalidUTF8
			}
			s.Pos = j + 1
			return unsafe.String(unsafe.SliceData(buf), len(buf)), nil
		}
		if c == '\\' {
			for j+1 >= len(s.buf) {
				err := s.ReadMore(j)
				j = 0
				if err != nil {
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
				for j+6 > len(s.buf) {
					err := s.ReadMore(j)
					j = 0
					if err != nil {
						return "", ErrBadString
					}
				}
				r, ok := parseHex4(s.buf[j+2 : j+6])
				if !ok {
					return "", ErrBadString
				}
				j += 6
				if utf16.IsSurrogate(r) {
					// Buffer the potential low-surrogate escape (\uXXXX, 6 bytes)
					// before pairing — it may straddle a refill boundary, and
					// without this the pair silently splits into two lone
					// surrogates (😀 → ��). Tolerate EOF here: a trailing lone
					// surrogate keeps r a surrogate and errors below, matching
					// the bytes path.
					for j+6 > len(s.buf) {
						err := s.ReadMore(j)
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
					// Still a surrogate → lone/unpaired: jsonv2 rejects;
					// permissive mode keeps the U+FFFD substitution.
					if validate && utf16.IsSurrogate(r) {
						return "", ErrInvalidUTF8
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
		rawHi |= c
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
		i = 0
	}
	neg := false
	if s.buf[i] == '-' {
		neg = true
		i++
		if i >= len(s.buf) {
			if err := s.ReadMore(i); err != nil {
				return 0, err
			}
			i = 0
		}
	}
	if s.buf[i] < '0' || s.buf[i] > '9' {
		return 0, ErrBadNumber
	}
	// RFC 8259: no leading zeros. A '0' first digit must end the integer part;
	// peek one byte (refilling if the window ends here) to catch "01".
	if s.buf[i] == '0' {
		if i+1 >= len(s.buf) {
			if err := s.ReadMore(i); err == nil {
				i = 0
			}
		}
		if i+1 < len(s.buf) && s.buf[i+1] >= '0' && s.buf[i+1] <= '9' {
			return 0, ErrBadNumber
		}
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
		i = 0
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
		i = 0
	}
	if s.buf[i] < '0' || s.buf[i] > '9' {
		return 0, ErrBadNumber
	}
	// RFC 8259: no leading zeros — see Int64.
	if s.buf[i] == '0' {
		if i+1 >= len(s.buf) {
			if err := s.ReadMore(i); err == nil {
				i = 0
			}
		}
		if i+1 < len(s.buf) && s.buf[i+1] >= '0' && s.buf[i+1] <= '9' {
			return 0, ErrBadNumber
		}
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
				if c == '.' || c == 'e' || c == 'E' {
					return 0, ErrBadNumber
				}
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
		i = 0
		if err != nil {
			break
		}
		buf = s.buf
	}
	s.Pos = i
	return n, nil
}

// Float64 scans a JSON number span then delegates to strconv.ParseFloat
// (or the exact-short fast path for ≤16 B spans).
func (s *Stream) Float64() (float64, error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return 0, err
		}
		i = 0
	}
	start := i
	// buf hoisted into a local; refills compact from start (bytes before it
	// are consumed) and rebase — without compaction every mid-number refill
	// lands with len == cap (readers fill the whole window) and doubles the
	// buffer. The span s.buf[start:i] is re-sliced after the loop, so the
	// rebase keeps it valid.
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
			break
		}
		buf = s.buf
	}
	if i == start {
		return 0, ErrBadNumber
	}
	// The refill loop collects a LOOSE [0-9.eE+-] span (it doubles as the
	// extent finder across refills), so validate the assembled — now
	// contiguous — span against the RFC 8259 grammar before parsing, matching
	// the bytes path. Short, L1-hot second pass on the stream tier.
	if end, gerr := skipNumber(s.buf[start:i], 0); gerr != nil || end != i-start {
		return 0, ErrBadNumber
	}
	// Short spans skip ParseFloat's re-scan when exactly representable —
	// mirror of the bytes-path scan.Float64 gate (exactShort is bit-identical
	// to ParseFloat on the accepted shape; the ≤16 B bound excludes the
	// 17-significant-digit floats that made the ungated variant regress).
	if i-start <= 16 {
		if v, ok := exactShort(s.buf[start:i]); ok {
			s.Pos = i
			return v, nil
		}
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
// and rebases *i — without compaction every mid-number refill lands with
// len == cap (readers fill the whole window) and doubles the buffer.
// Callers keep the hot `i < len(s.buf)` bounds check inline and call this
// only on exhaustion.
func (s *Stream) refillSkip(i *int) bool {
	err := s.ReadMore(*i)
	*i = 0
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
		i = 0
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
	return s.skipValueDepth(0)
}

func (s *Stream) skipValueDepth(depth int) error {
	if err := s.SkipSpace(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return ErrUnexpectedEnd
		}
		s.Pos = 0
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
		return s.skipArray(depth + 1)
	case '{':
		s.Pos++
		return s.skipObject(depth + 1)
	}
	return ErrBadValue
}

// CaptureValue returns the raw bytes of the next JSON value as a slice into the
// Stream's buffer — valid only until the next Stream op, so copy it if you retain
// it (json.Unmarshal / UnmarshalText / SetString consume it in place, no copy
// needed). It buffers the whole value contiguously and locates its end with the
// BYTES-path SkipValue — reused verbatim (SIMD tiers included), no streaming
// skip. A skip failure on a partial window is indistinguishable from truncation
// (no error position), so any error means "read more" and only surfaces once EOF
// says no more bytes can fix it — malformed input therefore buffers the stream's
// remainder before erroring. A huge value grows s.buf to its size, which is
// inherent: the raw bytes must be contiguous to hand off anyway.
func (s *Stream) CaptureValue() ([]byte, error) {
	start := s.Pos
	// eof: the reader drained during this capture (ReadMore returned
	// io.ErrUnexpectedEOF) — no more bytes can arrive, so the next skip's
	// verdict is final. Purely local; Stream carries no reader state.
	eof := false
	for {
		end, err := SkipValue(s.buf, start)
		// Trust the end only when a byte past it is buffered (end < len) or the
		// stream is drained — else a value ending at the window edge (e.g. a
		// number "123" that may continue "1234") could be cut short.
		if err == nil && (end < len(s.buf) || eof) {
			s.Pos = end
			return s.buf[start:end], nil
		}
		if eof {
			s.Pos = start
			return nil, err
		}
		// First refill compacts the consumed prefix (keep=start): the dead
		// bytes before the value become free tail capacity instead of being
		// dragged through every grow. start rebases to 0; later refills are
		// pure grow. Entry deliberately does NOT compact — the value may
		// already be fully buffered, and ReadMore always Reads (could block).
		if e := s.ReadMore(start); e != nil {
			if e != io.ErrUnexpectedEOF {
				s.Pos = 0
				return nil, e
			}
			eof = true
		}
		start = 0
		// Fill the spare capacity before re-skipping, so a byte-dribble
		// reader can't make the re-skip O(n²): skips land on a doubling window.
		for !eof && len(s.buf) < cap(s.buf) {
			if e := s.ReadMore(0); e != nil {
				if e != io.ErrUnexpectedEOF {
					s.Pos = 0
					return nil, e
				}
				eof = true
			}
		}
	}
}

func (s *Stream) skipArray(depth int) error {
	if depth > MaxDepth {
		return ErrMaxDepth
	}
	if err := s.SkipSpace(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return ErrBadArray
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

func (s *Stream) skipObject(depth int) error {
	if depth > MaxDepth {
		return ErrMaxDepth
	}
	if err := s.SkipSpace(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return ErrBadObject
		}
		s.Pos = 0
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
		if err := s.skipValueDepth(depth); err != nil {
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
	return s.anyValueDepth(0)
}

func (s *Stream) anyValueDepth(depth int) (any, error) {
	if err := s.SkipSpace(); err != nil {
		return nil, err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return nil, err
		}
		s.Pos = 0
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
		return s.String(true)
	case c == '-' || (c >= '0' && c <= '9'):
		return s.Float64()
	case c == '[':
		return s.anyArray(depth + 1)
	case c == '{':
		return s.anyObject(depth + 1)
	}
	return nil, ErrBadLiteral
}

func (s *Stream) anyArray(depth int) ([]any, error) {
	if depth > MaxDepth {
		return nil, ErrMaxDepth
	}
	s.Pos++ // consume '['
	if err := s.SkipSpace(); err != nil {
		return nil, err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return nil, err
		}
		s.Pos = 0
	}
	if s.buf[s.Pos] == ']' {
		s.Pos++
		return []any{}, nil
	}
	out := make([]any, 0, 4)
	for {
		v, err := s.anyValueDepth(depth)
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

func (s *Stream) anyObject(depth int) (map[string]any, error) {
	if depth > MaxDepth {
		return nil, ErrMaxDepth
	}
	s.Pos++ // consume '{'
	if err := s.SkipSpace(); err != nil {
		return nil, err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return nil, err
		}
		s.Pos = 0
	}
	if s.buf[s.Pos] == '}' {
		s.Pos++
		return map[string]any{}, nil
	}
	out := make(map[string]any, 4)
	for {
		key, err := s.String(true)
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
		v, err := s.anyValueDepth(depth)
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

// Number is the stream counterpart of [Number] — returns json.Number
// copied out of the buffer.
func (s *Stream) Number() (json.Number, error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return "", err
		}
		i = 0
	}
	start := i
	// buf hoisted into a local; refills compact from start and rebase (see
	// Float64) so a number straddling window edges can't balloon the buffer.
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
			break
		}
		buf = s.buf
	}
	if i == start {
		return "", ErrBadNumber
	}
	if end, gerr := skipNumber(s.buf[start:i], 0); gerr != nil || end != i-start {
		return "", ErrBadNumber
	}
	s.Pos = i
	return json.Number(string(s.buf[start:i])), nil
}

// AnyNumber is the [Stream.Any] variant that decodes JSON numbers as
// json.Number instead of float64.
func (s *Stream) AnyNumber() (any, error) {
	return s.anyNumberValueDepth(0)
}

func (s *Stream) anyNumberValueDepth(depth int) (any, error) {
	if err := s.SkipSpace(); err != nil {
		return nil, err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return nil, err
		}
		s.Pos = 0
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
		return s.String(true)
	case c == '-' || (c >= '0' && c <= '9'):
		return s.Number()
	case c == '[':
		return s.anyNumberArray(depth + 1)
	case c == '{':
		return s.anyNumberObject(depth + 1)
	}
	return nil, ErrBadLiteral
}

func (s *Stream) anyNumberArray(depth int) ([]any, error) {
	if depth > MaxDepth {
		return nil, ErrMaxDepth
	}
	s.Pos++
	if err := s.SkipSpace(); err != nil {
		return nil, err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return nil, err
		}
		s.Pos = 0
	}
	if s.buf[s.Pos] == ']' {
		s.Pos++
		return []any{}, nil
	}
	out := make([]any, 0, 4)
	for {
		v, err := s.anyNumberValueDepth(depth)
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

func (s *Stream) anyNumberObject(depth int) (map[string]any, error) {
	if depth > MaxDepth {
		return nil, ErrMaxDepth
	}
	s.Pos++
	if err := s.SkipSpace(); err != nil {
		return nil, err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return nil, err
		}
		s.Pos = 0
	}
	if s.buf[s.Pos] == '}' {
		s.Pos++
		return map[string]any{}, nil
	}
	out := make(map[string]any, 4)
	for {
		key, err := s.String(true)
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
		v, err := s.anyNumberValueDepth(depth)
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
