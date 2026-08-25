package ggen

import (
	"bytes"
	"encoding/json"
	"github.com/sirkostya009/ggen/internal/prealloc"
	"io"
	"iter"
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
//	var s Stream
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
	return (&Stream{}).Reset(r, buf)
}

// Reset binds the Stream to r with buf as the initial backing slice.
// buf is truncated to length 0 — its capacity is retained for parse
// working area. Pass nil to start with no backing (ReadMore allocates
// on first pull); pass a pre-sized slice to avoid growth allocs.
//
// Returns s so a freshly declared Stream can be built and handed to a
// decode helper in one expression:
//
//	v, err := s.Reset(r, buf).Value[T]()
func (s *Stream) Reset(r io.Reader, buf []byte) *Stream {
	*s = Stream{buf: buf[:0], r: r}
	return s
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
	// Bytes arrived — deliver them; a simultaneous non-EOF error re-surfaces
	// on the reader's next call (io.Reader contract: process n > 0 before
	// err — the number scanners' refill swallow would otherwise return a
	// TRUNCATED value with the fresh digits sitting unread in the buffer).
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
// String (false = allowinvalidutf8 opt-out).
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
		// Drained where a string must start matches bytes String's
		// ErrExpectString; a transient reader error propagates raw.
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
				s.Pos = start
				return "", false, ErrBadString
			}
			sawHigh = sawHigh || hi
			j = len(s.buf)
			err := s.ReadMore(start)
			j -= start
			start = 0
			if err != nil {
				// Post-compaction rebase: the whole window is the span, so the
				// off-the-end position is its end — mirrors bytes String's
				// len(data). Without it Offset() kept the pre-compaction cursor.
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
			s.Pos = start
			return "", false, ErrBadString
		}
		// Full-span check — a rune may straddle the chunk cursor j, so per-chunk
		// validation would false-error; ReadMore(start) keeps the span contiguous.
		if validate && (sawHigh || hi) && !utf8.Valid(s.buf[start:end]) {
			s.Pos = start
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
		// Drained here matches bytes String's ErrExpectString — see stringView.
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
			s.Pos = start
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
				s.Pos = start
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
			s.Pos = start
			return "", ErrBadString
		}
		// Full-span check — see stringView (rune may straddle j).
		if validate && (sawHigh || hi) && !utf8.Valid(s.buf[start:end]) {
			s.Pos = start
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
	// q memoizes the next '"' at or after j (len(buf) = none buffered) so the
	// quote probe is O(n) across escape iterations instead of a rescan to the
	// same far quote per escape — see the bytes-path skipString. Every refill
	// shifts indices, so refill sites reset q to -1 (re-locate).
	q := -1
	for {
		if q < j {
			if rel := bytes.IndexByte(s.buf[j:], '"'); rel >= 0 {
				q = j + rel
			} else {
				q = len(s.buf)
			}
		}
		// Bound the backslash probe to the closing quote (a whole-tail probe
		// makes SkipValue quadratic on fully buffered payloads).
		bsRel := bytes.IndexByte(s.buf[j:q], '\\')
		// Closing quote, no backslash before it — fast path.
		if q < len(s.buf) && bsRel < 0 {
			if hasCtrlByte(s.buf[j:q]) {
				s.Pos = j
				return ErrBadString
			}
			s.Pos = q + 1
			return nil
		}
		// Backslash before any closing quote — slow path: validate
		// literal bytes up to the backslash, then handle the escape.
		if bsRel >= 0 {
			bs := j + bsRel
			if hasCtrlByte(s.buf[j:bs]) {
				s.Pos = j
				return ErrBadString
			}
			// Need at least one byte past the backslash for the escape kind.
			if bs+1 >= len(s.buf) {
				err := s.ReadMore(bs)
				if err != nil {
					// ReadMore compacted from bs, so the backslash now sits at
					// 0 — leaving the pre-compaction cursor stamped an Offset()
					// past the end of the document.
					s.Pos = 0
					return NotEOF(err, ErrBadString)
				}
				j -= bs
				start = 0
				bs = 0
				q = -1
			}
			switch s.buf[bs+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				j = bs + 2
			case 'u':
				// Need 4 hex digits past `\u`.
				for bs+6 > len(s.buf) {
					err := s.ReadMore(bs)
					if err != nil {
						s.Pos = 0
						return NotEOF(err, ErrBadString)
					}
					j -= bs
					start = 0
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
		// Neither quote nor backslash in current buffer — validate
		// what's there, then read more.
		if hasCtrlByte(s.buf[j:]) {
			s.Pos = j
			return ErrBadString
		}
		// Everything buffered is validated and being DISCARDED — full
		// compaction keeps the window at chunk size instead of growing the
		// buffer toward the skipped string's length.
		j = len(s.buf)
		err := s.ReadMore(j)
		j = 0
		start = 0
		q = -1
		if err != nil {
			s.Pos = j
			return NotEOF(err, ErrUnterminated)
		}
	}
}

// stringSlow handles escape sequences into a fresh owned scratch buffer,
// seeded from the already-scanned prefix. The window compacts on refill
// (see loop comment); the result aliases the scratch, never s.buf.
func (s *Stream) stringSlow(start, j int, validate bool) (string, error) {
	bad, rawHigh := ctrlOrHigh(s.buf[start:j])
	if bad {
		s.Pos = start
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
				s.Pos = j
				return "", NotEOF(err, ErrUnterminated)
			}
		}
		// Raw bytes copy through a windowed inner loop, escape dispatch hoisted
		// out — see the bytes-path stringSlow. Bounded by the buffered window
		// too; a run reaching its edge resumes after the refill at the loop top.
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
			continue // refill at the loop top
		}
		c := s.buf[j]
		if c == '"' {
			// Escape outputs are valid encodings by construction (surrogates
			// error below), so only RAW bytes copied through can make buf
			// invalid — skip the walk when they were all ASCII. buf is the
			// assembled output, so refill boundaries can't split a rune here.
			if validate && (rawHigh || rawHi&0x80 != 0) && !utf8.Valid(buf) {
				s.Pos = j
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
					s.Pos = j
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
				for j+6 > len(s.buf) {
					err := s.ReadMore(j)
					j = 0
					if err != nil {
						s.Pos = j
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
							if err != io.ErrUnexpectedEOF {
								// Transient reader error — surfacing
								// ErrInvalidUTF8 for it would mislabel a
								// hiccup as a lone surrogate.
								s.Pos = j
								return "", err
							}
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
						s.Pos = j
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
		// Neither '"' nor '\\': the inner loop stopped at the window bound
		// mid-run, so the outer loop simply opens the next window.
	}
}

// Int64 scans an integer with per-digit overflow detection, sign applied
// at the end. Matches Int64.
func (s *Stream) Int64() (int64, error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return 0, NotEOF(err, ErrBadNumber)
		}
		i = 0
	}
	// start is the value head. Every refill keeps from it (number spans are
	// bounded at ~20 bytes, so retaining them is free) and rebases, so a
	// transient reader error can return with s.Pos back on the intact span and
	// a retry re-scans it — refilling from the cursor instead discarded the
	// '-' and the digits already folded into the accumulator, silently flipping
	// the sign or truncating the value.
	start := i
	neg := false
	if s.buf[i] == '-' {
		neg = true
		i++
		if i >= len(s.buf) {
			// Drained after a bare "-" is a grammar error, like the bytes
			// path; a transient reader error still propagates raw.
			err := s.ReadMore(start)
			i -= start
			start = 0
			if err != nil {
				s.Pos = start
				return 0, NotEOF(err, ErrBadNumber)
			}
		}
	}
	if s.buf[i] < '0' || s.buf[i] > '9' {
		s.Pos = start
		return 0, ErrBadNumber
	}
	// RFC 8259: no leading zeros. A '0' first digit must end the integer part;
	// peek one byte (refilling if the window ends here) to catch "01".
	if s.buf[i] == '0' {
		if i+1 >= len(s.buf) {
			// ReadMore compacts BEFORE the Read, so rebase unconditionally
			// (rebasing only on success left a stale cursor). Swallow only the
			// drained-reader case — bare "0" at EOF is a valid number, the
			// digit loop then hits the stable EOF; a transient error must
			// propagate or the "01" rejection is silently lost when a later
			// refill resumes the digit loop.
			err := s.ReadMore(start)
			i -= start
			start = 0
			if err != nil && err != io.ErrUnexpectedEOF {
				s.Pos = start
				return 0, err
			}
		}
		if i+1 < len(s.buf) && s.buf[i+1] >= '0' && s.buf[i+1] <= '9' {
			s.Pos = start
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
					s.Pos = start
					return 0, ErrBadNumber
				}
				break scan
			}
			d := uint64(c - '0')
			if u > limit/10 || (u == limit/10 && d > limit%10) {
				s.Pos = start
				return 0, ErrNumberOverflow
			}
			u = u*10 + d
			i++
		}
		err := s.ReadMore(start)
		i -= start
		start = 0
		if err != nil {
			// Drained is a legitimate value end (the digits ran to EOF); a
			// real reader error is NOT — swallowing it returned a TRUNCATED
			// value with a nil error, silently wrong at top level and a
			// bogus grammar error one frame up.
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

// Uint64 scans an unsigned integer with overflow detection. Returns
// ErrNumberOverflow when the magnitude exceeds MaxUint64.
func (s *Stream) Uint64() (uint64, error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return 0, NotEOF(err, ErrBadNumber)
		}
		i = 0
	}
	// start is the value head — refills keep from it so a transient reader
	// error leaves the span intact for a retry. See Int64.
	start := i
	if s.buf[i] < '0' || s.buf[i] > '9' {
		s.Pos = start
		return 0, ErrBadNumber
	}
	// RFC 8259: no leading zeros — see Int64's peek for the error contract.
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
			s.Pos = start
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
					s.Pos = start
					return 0, ErrBadNumber
				}
				break scan
			}
			d := uint64(c - '0')
			if n > Uint64Limit/10 || (n == Uint64Limit/10 && d > Uint64Limit%10) {
				s.Pos = start
				return 0, ErrNumberOverflow
			}
			n = n*10 + d
			i++
		}
		err := s.ReadMore(start)
		i -= start
		start = 0
		if err != nil {
			// Drained is a legitimate value end (the digits ran to EOF); a
			// real reader error is NOT — swallowing it returned a TRUNCATED
			// value with a nil error, silently wrong at top level and a
			// bogus grammar error one frame up.
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

// Float64 scans a JSON number span then delegates to strconv.ParseFloat
// (or the exact-short fast path for ≤16 B spans).
func (s *Stream) Float64() (float64, error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return 0, NotEOF(err, ErrBadNumber)
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
			// Drained is a legitimate value end (the digits ran to EOF); a
			// real reader error is NOT — swallowing it returned a TRUNCATED
			// value with a nil error, silently wrong at top level and a
			// bogus grammar error one frame up.
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
	// The refill loop collects a LOOSE [0-9.eE+-] span (it doubles as the
	// extent finder across refills), so run the RFC 8259 grammar over the
	// assembled — now contiguous — span. The grammar END is authoritative,
	// NOT the loose scan's: the bytes path is maximal-munch (it stops at the
	// first byte that cannot extend the number and leaves the rest to the
	// caller), so truncating here is what keeps the two paths byte-identical
	// on `1.5.5` / `1e5e` / `01` instead of erroring where bytes succeeds.
	end, gerr := skipNumber(s.buf[start:i], 0)
	if gerr != nil || end == 0 {
		s.Pos = start
		return 0, ErrBadNumber
	}
	i = start + end
	// Short spans skip ParseFloat's re-scan when exactly representable —
	// mirror of the bytes-path Float64 gate (exactShort is bit-identical
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
		s.Pos = start
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
func (s *Stream) refillSkip(i *int, rerr *error) bool {
	if *rerr != nil {
		// A recorded reader error ends the scan: retrying would issue fresh
		// blocking Reads on a reader that already failed, wedging the decode
		// where every sibling value scanner returns at once.
		return false
	}
	err := s.ReadMore(*i)
	*i = 0
	if err != nil {
		// A drained window legitimately ends the value; a real reader error
		// must abort the skip rather than read as "number ended here".
		if err != io.ErrUnexpectedEOF {
			*rerr = err
		}
		return false
	}
	return *i < len(s.buf)
}

// NotEOF keeps a real reader error intact and maps only the drained-window
// case to the grammar sentinel the caller would report at true end of input.
// Relabeling a recoverable hiccup as malformed JSON destroys error identity
// (and contradicts ReadMore's "a transient error that loses no bytes resumes
// losslessly" contract). Exported for generated stream decoders, whose
// dispatch-loop refills need the same mapping the primitives apply.
func NotEOF(err, sentinel error) error {
	if err != io.ErrUnexpectedEOF {
		return err
	}
	return sentinel
}

// orBadNumber prefers a recorded reader error over the grammar sentinel —
// a refill that failed for I/O reasons is not a malformed number.
func orBadNumber(rerr error) error {
	if rerr != nil {
		return rerr
	}
	return ErrBadNumber
}

// skipNumber is the stream mirror of the bytes-path [skipNumber] — same
// RFC 8259 grammar, same accept-set.
func (s *Stream) skipNumber() error {
	i := s.Pos
	var rerr error // set by refillSkip on a real (non-drained) reader error
	if i >= len(s.buf) && !s.refillSkip(&i, &rerr) {
		return orBadNumber(rerr)
	}
	if s.buf[i] == '-' {
		i++
		if i >= len(s.buf) && !s.refillSkip(&i, &rerr) {
			return orBadNumber(rerr)
		}
	}
	if s.buf[i] == '0' {
		i++
	} else if s.buf[i] >= '1' && s.buf[i] <= '9' {
		i++
		for {
			if i >= len(s.buf) && !s.refillSkip(&i, &rerr) {
				break
			}
			if s.buf[i] < '0' || s.buf[i] > '9' {
				break
			}
			i++
		}
	} else {
		return orBadNumber(rerr)
	}
	if (i < len(s.buf) || s.refillSkip(&i, &rerr)) && s.buf[i] == '.' {
		i++
		if (i >= len(s.buf) && !s.refillSkip(&i, &rerr)) || s.buf[i] < '0' || s.buf[i] > '9' {
			return orBadNumber(rerr)
		}
		i++
		for {
			if i >= len(s.buf) && !s.refillSkip(&i, &rerr) {
				break
			}
			if s.buf[i] < '0' || s.buf[i] > '9' {
				break
			}
			i++
		}
	}
	if (i < len(s.buf) || s.refillSkip(&i, &rerr)) && (s.buf[i] == 'e' || s.buf[i] == 'E') {
		i++
		if (i < len(s.buf) || s.refillSkip(&i, &rerr)) && (s.buf[i] == '+' || s.buf[i] == '-') {
			i++
		}
		if (i >= len(s.buf) && !s.refillSkip(&i, &rerr)) || s.buf[i] < '0' || s.buf[i] > '9' {
			return orBadNumber(rerr)
		}
		i++
		for {
			if i >= len(s.buf) && !s.refillSkip(&i, &rerr) {
				break
			}
			if s.buf[i] < '0' || s.buf[i] > '9' {
				break
			}
			i++
		}
	}
	if rerr != nil {
		return rerr
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
			return false, NotEOF(err, ErrBadBool)
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
				return false, NotEOF(err, ErrBadBool)
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
			return NotEOF(err, ErrUnexpectedEnd)
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
					return NotEOF(err, ErrBadLiteral)
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
// skip. SkipValue preserves its give-up position on error, so a failure at a
// byte strictly inside the window is surfaced as final immediately; only an
// off-the-end failure means "read more" and keeps reading until EOF says no
// more bytes can fix it. A huge value grows s.buf to its size, which is
// inherent: the raw bytes must be contiguous to hand off anyway.
func (s *Stream) CaptureValue() ([]byte, error) {
	start := s.Pos
	// eof: the reader drained during this capture (ReadMore returned
	// io.ErrUnexpectedEOF) — no more bytes can arrive, so the next skip's
	// verdict is final. Purely local; Stream carries no reader state.
	eof := false
	for {
		end, err := SkipValue(s.buf, start)
		// Trust the end when a byte past it is buffered (end < len) or the
		// stream is drained — else a NUMBER ending at the window edge (e.g.
		// "123" that may continue "1234") could be cut short. Every other
		// value is self-delimiting (closing quote/bracket, fixed literal),
		// so its clean end is final even at the edge: demanding a lookahead
		// byte there would block forever on a live reader (socket) that has
		// delivered the whole value and has nothing in flight.
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
		if eof {
			s.Pos = start
			return nil, err
		}
		// A skip that failed at a byte STRICTLY INSIDE the window is final —
		// no future byte can repair a malformation that sits before bytes we
		// already hold. Surfacing it now is what keeps a live (never-EOF)
		// reader from wedging the decode: without this, a fully-delivered
		// `{"a":}` on a socket blocked in Read forever waiting for bytes that
		// could not have helped. Only an off-the-end failure (end == len) is
		// ambiguous with truncation and keeps reading. SkipValue preserves
		// its give-up position on error, so no re-walk is needed. ErrMaxDepth
		// is final wherever it lands — no arriving byte can un-exceed the cap.
		if err != nil && (end < len(s.buf) || err == ErrMaxDepth) {
			s.Pos = start
			return nil, err
		}
		// First refill compacts the consumed prefix (keep=start): the dead
		// bytes before the value become free tail capacity instead of being
		// dragged through every grow. start rebases to 0; later refills are
		// pure grow. Entry deliberately does NOT compact — the value may
		// already be fully buffered, and ReadMore always Reads (could block).
		// One Read, then re-skip: once these bytes may complete the value, a
		// second Read on a momentarily-drained live reader would block. An
		// eager reader (file, bytes.Reader) fills the whole spare tail per
		// Read, so its skips still land on a doubling window; a short-read
		// regime re-skips per delivery — the price of liveness.
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
		// Drained right after '[' matches bytes skipArray, whose element
		// skipValue reports ErrUnexpectedEnd — not ErrBadArray.
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
			// skipString reports ErrExpectString only at the key head (drained
			// or a non-quote byte); the bytes loop reports ErrBadObject for
			// both, so the relabel is unconditional.
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
			return nil, NotEOF(err, ErrUnexpectedEnd)
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
					return nil, NotEOF(err, ErrBadLiteral)
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
	if depth > maxDepth {
		return nil, ErrMaxDepth
	}
	s.Pos++ // consume '['
	if err := s.SkipSpace(); err != nil {
		return nil, err
	}
	if s.Pos >= len(s.buf) {
		// Drained right after '[' matches bytes anyArray, whose element
		// anyValue reports ErrUnexpectedEnd.
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

func (s *Stream) anyObject(depth int) (map[string]any, error) {
	if depth > maxDepth {
		return nil, ErrMaxDepth
	}
	s.Pos++ // consume '{'
	if err := s.SkipSpace(); err != nil {
		return nil, err
	}
	if s.Pos >= len(s.buf) {
		// Drained right after '{' matches bytes anyObject, whose key String
		// reports ErrExpectString.
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
		key, err := s.String(true)
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

// Number is the stream counterpart of [Number] — returns json.Number
// copied out of the buffer.
func (s *Stream) Number() (json.Number, error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return "", NotEOF(err, ErrBadNumber)
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
			// Drained is a legitimate value end (the digits ran to EOF); a
			// real reader error is NOT — swallowing it returned a TRUNCATED
			// value with a nil error, silently wrong at top level and a
			// bogus grammar error one frame up.
			if err != io.ErrUnexpectedEOF {
				s.Pos = start
				return "", err
			}
			break
		}
		buf = s.buf
	}
	if i == start {
		s.Pos = start
		return "", ErrBadNumber
	}
	// Grammar end is authoritative — see Float64 (bytes-path parity).
	end, gerr := skipNumber(s.buf[start:i], 0)
	if gerr != nil || end == 0 {
		s.Pos = start
		return "", ErrBadNumber
	}
	i = start + end
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
			return nil, NotEOF(err, ErrUnexpectedEnd)
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
					return nil, NotEOF(err, ErrBadLiteral)
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
	if depth > maxDepth {
		return nil, ErrMaxDepth
	}
	s.Pos++
	if err := s.SkipSpace(); err != nil {
		return nil, err
	}
	if s.Pos >= len(s.buf) {
		// Drained right after '[' matches bytes anyArray, whose element
		// anyValue reports ErrUnexpectedEnd.
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

func (s *Stream) anyNumberObject(depth int) (map[string]any, error) {
	if depth > maxDepth {
		return nil, ErrMaxDepth
	}
	s.Pos++
	if err := s.SkipSpace(); err != nil {
		return nil, err
	}
	if s.Pos >= len(s.buf) {
		// Drained right after '{' matches bytes anyObject, whose key String
		// reports ErrExpectString.
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
		key, err := s.String(true)
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

// StreamDecoder is the interface a type satisfies to be read off a Stream.
// Every ggen-generated struct implements it; the bytes-path counterpart is
// Decoder, which needs no Stream. [Stream.Value], [Stream.Slice],
// [Stream.Array] and [Stream.Seq] constrain on it.
type StreamDecoder[T any] interface {
	DecodeFromStream(s *Stream) (T, error)
}

// Value reads one T from the Stream, leaving the cursor just past the value so
// the next read continues where this one stopped:
//
//	s := ggen.NewStream(r, buf)
//	u, err := s.Value[User]()
//
// An optional rcv is decoded INTO: generated decoders seed their result from
// the receiver and reset containers keeping capacity (`clear(m)`, `sl[:0]`), so
// handing back a previously decoded value reuses its maps and slices instead of
// allocating fresh ones. Only rcv[0] is read.
//
//	u, err = s.Value(u) // reuses u's containers
//
// The rcv's SCALAR fields are overwritten by the payload, but a field the
// payload omits keeps the old value — pass a zero T when that matters.
//
// The error is whatever the generated decoder returned — there is no enclosing
// array to name, so nothing is prepended.
func (s *Stream) Value[T StreamDecoder[T]](rcv ...T) (T, error) {
	var into T
	if len(rcv) > 0 {
		into = rcv[0]
	}
	return into.DecodeFromStream(s)
}

// Slice reads a JSON array of T, leaving the cursor just past the closing
// bracket. Trailing data is NOT rejected — a Stream may legitimately carry
// further values, and probing for them would block a live reader — so the
// caller decides what follows:
//
//	users, err := s.Slice[User]()
//	// s is still positioned; keep reading
//
// An optional rcv is reused as the destination buffer, and NOT merely appended
// into: the slice itself is truncated to keep its capacity, AND element i is
// decoded into rcv[0][i], so each element's own maps and slices are recycled
// too. That makes a steady-state re-decode allocation-free rather than just
// saving the outer backing array.
//
//	users, err = s.Slice(users) // reuses the slice AND every element's containers
//
// Elements past the new length keep their allocations but are not returned.
// A `[]` decodes to a non-nil empty slice, matching the bytes walker and
// jsonv2. Errors carry no element path (that wrapping lives in decode, which
// scan cannot import).
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
		// reuses that space instead of doubling — see [Stream.Seq].
		if s.Pos > 0 && len(s.buf) == cap(s.buf) {
			s.consumed += s.Pos
			copy(s.buf, s.buf[s.Pos:])
			s.buf = s.buf[:len(s.buf)-s.Pos]
			s.Pos = 0
		}
		// Decode into the element the buffer already holds at this index, so
		// its containers are recycled. Read BEFORE the append that overwrites
		// this slot — result and prev share a backing array.
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
			// Separator WS — scalar/alias element decoders don't skip leading
			// space themselves (mirrors the bytes walker's SkipSpace(i+1)).
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

// Array iterates a JSON array WITHOUT gathering it, yielding one element at a
// time. It is the lazy sibling of [Stream.Slice] — same grammar and the same
// element decoding, but nothing accumulates, so a million-element array costs
// one element of memory rather than a million:
//
//	for v, err := range ggen.NewStream(r, buf).Array[Item]() {
//		if err != nil { break }
//		handle(v)
//	}
//
// The range ends when the closing bracket is consumed, leaving the cursor just
// past it so the Stream can be read from again. A malformed array yields one
// error and stops. Breaking out early leaves the cursor INSIDE the array —
// there is no way to know where the array ends without walking it — so a
// Stream abandoned mid-range is only good for closing.
//
// Like [Stream.Seq] it reuses one value for the whole iteration, seeded from an
// optional rcv, so a long array settles at zero allocations per element; and
// like Seq that means a yielded value is valid only until the next pull. Copy
// anything retained past the loop body (strings are owned, maps/slices alias).
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
			// reuses that space. len == cap is exactly ReadMore's grow
			// condition, which keeps this off the path whenever the buffer has
			// spare tail: generated container decoders refill grow-only, so a
			// full window is the one state that doubles. The gate is what
			// bounds the cost — compacting every element would memmove the
			// unread remainder each time.
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
				// Separator WS — scalar/alias element decoders don't skip
				// leading space themselves.
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

// Seq yields consecutive top-level values off the Stream — concatenated JSON or
// NDJSON — and keeps going for as long as the reader keeps producing:
//
//	for v, err := range ggen.NewStream(conn, buf).Seq[Event]() {
//		if err != nil { break }
//		handle(v)
//	}
//
// The iteration ends cleanly (no error yielded) when the reader drains at a
// value boundary, so a file or a closed socket terminates the range normally.
// Anything else — a malformed value, a real reader error — is yielded once as
// the error half and then ends the sequence. Breaking out of the range stops
// reading; the Stream is left wherever the last value finished, so it can be
// handed to another method afterwards.
//
// It never reads past a completed value before delivering it, so a quiet
// socket cannot stall an element that has already arrived: the refill for the
// NEXT value happens on the next pull, not eagerly after yielding this one.
//
// Seq REUSES ONE VALUE for the whole iteration: each decode goes into the
// previous one, recycling its maps and slices, so a long stream settles at zero
// allocations per element. An optional rcv seeds that value with containers the
// caller already has; without it the sequence starts from a zero T declared
// before the loop and warms up on the first element.
//
// The window is kept bounded: consumed values are dropped (gated — see the
// loop body) so generated container decoders, whose refills are grow-only,
// cannot ratchet the buffer upward across a long stream.
//
// CONSEQUENCE: a yielded value is only valid until the next pull — the
// following iteration decodes over its containers. Copy anything you retain
// past the loop body (strings are owned, so only maps/slices alias). Ranging
// twice over the same Seq is fine; each run starts its own value.
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
				// At a value boundary with nothing buffered: pull once. A
				// drained reader is the clean end of the sequence.
				if err := s.ReadMore(s.Pos); err != nil {
					if err == io.ErrUnexpectedEOF {
						return
					}
					var zero T
					yield(zero, err)
					return
				}
				s.Pos = 0
				// The fresh chunk may lead with whitespace (NDJSON newline).
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
